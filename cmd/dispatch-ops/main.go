// Command dispatch-ops runs the Dispatch Ops API server and its admin tasks.
package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/golang-migrate/migrate/v4"
	"k8s.io/client-go/kubernetes"
	"k8s.io/klog/v2"

	"dispatch-ops/internal/api"
	"dispatch-ops/internal/auth"
	"dispatch-ops/internal/config"
	"dispatch-ops/internal/credentials"
	"dispatch-ops/internal/db"
	"dispatch-ops/internal/jobstream"
	"dispatch-ops/internal/metrics"
	"dispatch-ops/internal/notify"
	"dispatch-ops/internal/oidc"
	"dispatch-ops/internal/secrets"
	"dispatch-ops/internal/server"
	"dispatch-ops/internal/store"
	"dispatch-ops/internal/taskmanager"
	"dispatch-ops/internal/tlscert"
	"dispatch-ops/web"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

const usage = `Usage: dispatch-ops <command> [arguments]

Commands:
  serve                     Run the API server and web UI
  task-manager              Run the task manager, which runs jobs on Kubernetes
  migrate up                Apply all pending database migrations
  migrate down              Roll back the most recent migration
  migrate version           Show the current migration version
  migrate force <version>   Mark a version as applied after fixing a failed migration
  user create [flags]       Create a local user (run "dispatch-ops user create -h")
  user grant-admin <name>   Make an existing user a system administrator
  gen-key                   Print a new random credential encryption key
  version                   Print the version

Configuration is read from DISPATCH_* environment variables; see README.md.
`

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	switch args[0] {
	case "serve":
		return serve()
	case "task-manager":
		return runTaskManager()
	case "gen-key":
		fmt.Println(secrets.GenerateKey())
		return nil
	case "migrate":
		return migrateCmd(args[1:])
	case "user":
		switch {
		case len(args) >= 2 && args[1] == "create":
			return createUser(args[2:])
		case len(args) == 3 && args[1] == "grant-admin":
			return grantAdmin(args[2])
		}
		return errors.New("usage: dispatch-ops user create [flags] | user grant-admin <username>")
	case "version":
		fmt.Println(version)
		return nil
	case "help", "-h", "--help":
		fmt.Print(usage)
		return nil
	default:
		return fmt.Errorf("unknown command %q\n\n%s", args[0], usage)
	}
}

func newLogger(cfg config.Config) *slog.Logger {
	var level slog.Level
	if err := level.UnmarshalText([]byte(cfg.LogLevel)); err != nil {
		level = slog.LevelInfo
	}
	opts := &slog.HandlerOptions{Level: level}
	if cfg.LogFormat == "text" {
		return slog.New(slog.NewTextHandler(os.Stderr, opts))
	}
	return slog.New(slog.NewJSONHandler(os.Stderr, opts))
}

func serve() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	keys, err := cfg.Keyring()
	if err != nil {
		return err
	}
	log := newLogger(cfg)
	slog.SetDefault(log)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if cfg.AutoMigrate {
		log.Info("applying database migrations")
		if err := db.MigrateUp(cfg.DatabaseURL); err != nil {
			return err
		}
	}

	pool, err := db.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()
	st := store.New(pool)

	hub := jobstream.NewHub()
	go hub.Run(ctx, st, log)

	var sso *oidc.Provider
	if o := cfg.OIDC; o.Enabled() {
		sso = oidc.New(oidc.Config{
			Issuer: o.Issuer, ClientID: o.ClientID, ClientSecret: o.ClientSecret, RedirectURL: cfg.OIDCRedirectURL(),
			Scopes: o.Scopes, UsernameClaim: o.UsernameClaim, GroupsClaim: o.GroupsClaim, DisplayName: o.DisplayName,
		})
		log.Info("single sign-on enabled", "issuer", o.Issuer, "redirect_url", cfg.OIDCRedirectURL())
	}

	apiHandler, err := api.NewHandler(&api.Handlers{
		Store:         st,
		Vault:         credentials.Vault{Keys: keys},
		Hub:           hub,
		OIDC:          sso,
		Log:           log,
		SessionTTL:    cfg.SessionTTL,
		SecureCookies: cfg.SecureCookies,
		Version:       version,
		Notifier:      &notify.Sender{ExternalURL: cfg.ExternalURL, Version: version},
		LoginLimits: store.LoginLimits{
			Window: cfg.Login.Window, MaxPerUsername: cfg.Login.MaxPerUsername, MaxPerIP: cfg.Login.MaxPerIP,
		},
	})
	if err != nil {
		return err
	}

	srv := &http.Server{
		Addr: cfg.ListenAddr,
		Handler: server.New(apiHandler, st, web.Dist(), log,
			server.WithTrustedProxies(cfg.TrustedProxies),
			server.WithBodyReadTimeout(server.DefaultBodyReadTimeout)),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
		// No ReadTimeout or WriteTimeout: job log streams (SSE) are
		// long-lived, and ReadTimeout would cancel their context. Request
		// bodies get their own deadline instead (server.WithBodyReadTimeout).
	}
	if cfg.TLS {
		if srv.TLSConfig, err = serverTLS(cfg, log); err != nil {
			return err
		}
	}

	go cleanupExpiredTokens(ctx, st, cfg.Login.Window, log)
	if addr := cfg.MetricsAddrOr(":9090"); addr != "" {
		go metrics.Serve(ctx, addr, log)
	}

	errc := make(chan error, 1)
	go func() {
		log.Info("listening", "addr", cfg.ListenAddr, "tls", cfg.TLS, "version", version)
		if cfg.TLS {
			errc <- srv.ListenAndServeTLS("", "")
		} else {
			errc <- srv.ListenAndServe()
		}
	}()

	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	log.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	return srv.Shutdown(shutdownCtx)
}

// serverTLS serves DISPATCH_TLS_CERT_FILE and DISPATCH_TLS_KEY_FILE,
// reloading them when they change, or else a self-signed ECDSA P-384
// certificate made now, which is never written to disk.
func serverTLS(cfg config.Config, log *slog.Logger) (*tls.Config, error) {
	tc := &tls.Config{MinVersion: tls.VersionTLS12}
	if cfg.TLSCertFile != "" {
		r, err := tlscert.NewReloader(cfg.TLSCertFile, cfg.TLSKeyFile, log)
		if err != nil {
			return nil, err
		}
		tc.GetCertificate = r.GetCertificate
		log.Info("serving the TLS certificate from files", "cert_file", cfg.TLSCertFile, "sha256", tlscert.Fingerprint(r.Certificate()))
		return tc, nil
	}

	var hosts []string
	add := func(h string) {
		if h != "" && !slices.Contains(hosts, h) {
			hosts = append(hosts, h)
		}
	}
	if u, err := url.Parse(cfg.ExternalURL); err == nil {
		add(u.Hostname())
	}
	for _, h := range []string{"localhost", "127.0.0.1", "::1"} {
		add(h)
	}
	if h, err := os.Hostname(); err == nil {
		add(h)
	}
	certPEM, keyPEM, err := tlscert.SelfSigned(hosts, 365*24*time.Hour)
	if err != nil {
		return nil, err
	}
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return nil, err
	}
	tc.Certificates = []tls.Certificate{cert}
	log.Warn("serving a self-signed TLS certificate made at startup; set DISPATCH_TLS_CERT_FILE and DISPATCH_TLS_KEY_FILE to use your own",
		"hosts", hosts, "sha256", tlscert.Fingerprint(&cert))
	return tc, nil
}

func runTaskManager() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	if err := cfg.ValidateTaskManager(); err != nil {
		return err
	}
	keys, err := cfg.Keyring()
	if err != nil {
		return err
	}
	log := newLogger(cfg).With("component", "task-manager")
	slog.SetDefault(log)
	// client-go logs through klog; send that to the same logger.
	klog.SetSlogLogger(log)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	pool, err := db.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()

	restCfg, err := taskmanager.KubeConfig(cfg.TaskManager.KubeContext)
	if err != nil {
		return err
	}
	kube, err := kubernetes.NewForConfig(restCfg)
	if err != nil {
		return err
	}
	log.Info("connected to Kubernetes", "host", restCfg.Host, "context", cfg.TaskManager.KubeContext)

	tm := cfg.TaskManager
	m := &taskmanager.Manager{
		Store:       store.New(pool),
		Kube:        kube,
		Vault:       credentials.Vault{Keys: keys},
		Log:         log,
		Notifier:    &notify.Sender{ExternalURL: cfg.ExternalURL, Version: version},
		NotifyVault: notify.Vault{Keys: keys},
		Config: taskmanager.Config{
			Namespace:         tm.Namespace,
			RunnerAPIURL:      tm.RunnerAPIURL,
			RunnerAPITLSDir:   tm.RunnerAPITLSDir,
			MaxConcurrentJobs: tm.MaxConcurrentJobs,
			JobTTL:            tm.JobTTL,
			StartTimeout:      tm.StartTimeout,
			JobRetention:      tm.JobRetention,
			AuditRetention:    tm.AuditRetention,
			JobResources:      tm.JobResources,
			JobWorkSizeLimit:  tm.JobWorkSizeLimit,
		},
	}
	metrics.Registry.MustRegister(taskmanager.NewCollector(m))
	// A different default from serve's, so both can run on one machine.
	if addr := cfg.MetricsAddrOr(":9091"); addr != "" {
		go metrics.Serve(ctx, addr, log)
	}
	if cfg.ExternalURL == "" {
		log.Warn("DISPATCH_EXTERNAL_URL is not set, so notifications won't link to jobs")
	}
	if !tm.LeaderElection {
		log.Warn("leader election is off; run only one task manager")
		return m.Run(ctx)
	}
	return taskmanager.RunWithLeaderElection(ctx, kube, m, log)
}

// cleanupExpiredTokens deletes expired sessions and API tokens, and failed
// sign-ins older than the throttle window, every hour.
func cleanupExpiredTokens(ctx context.Context, st *store.Store, loginWindow time.Duration, log *slog.Logger) {
	t := time.NewTicker(time.Hour)
	defer t.Stop()
	for {
		n, err := st.DeleteExpiredTokens(ctx)
		if err != nil && ctx.Err() == nil {
			log.Warn("deleting expired tokens", "err", err)
		} else if n > 0 {
			log.Info("deleted expired tokens", "count", n)
		}
		if _, err := st.DeleteOldLoginFailures(ctx, loginWindow); err != nil && ctx.Err() == nil {
			log.Warn("deleting old failed sign-ins", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func migrateCmd(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: dispatch-ops migrate up|down|version|force <version>")
	}
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	m, err := db.Migrator(cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer m.Close()

	switch args[0] {
	case "up":
		err = m.Up()
	case "down":
		err = m.Steps(-1)
	case "force":
		if len(args) != 2 {
			return errors.New("usage: dispatch-ops migrate force <version>")
		}
		v, perr := strconv.Atoi(args[1])
		if perr != nil {
			return fmt.Errorf("invalid version %q", args[1])
		}
		err = m.Force(v)
	case "version":
	default:
		return fmt.Errorf("unknown migrate command %q", args[0])
	}
	if err != nil && !errors.Is(err, migrate.ErrNoChange) {
		return err
	}

	v, dirty, err := m.Version()
	switch {
	case errors.Is(err, migrate.ErrNilVersion):
		fmt.Println("version: none (no migrations applied)")
	case err != nil:
		return err
	default:
		fmt.Printf("version: %d dirty: %t\n", v, dirty)
	}
	return nil
}

func createUser(args []string) error {
	fs := flag.NewFlagSet("user create", flag.ContinueOnError)
	username := fs.String("username", "", "username (required)")
	displayName := fs.String("display-name", "", "display name")
	email := fs.String("email", "", "email address")
	admin := fs.Bool("admin", false, "grant system administrator rights")
	passwordStdin := fs.Bool("password-stdin", false, "read the password from standard input instead of generating one")
	passwordFile := fs.String("password-file", "", "read the password from this file instead of generating one")
	ifMissing := fs.Bool("if-missing", false, "do nothing, successfully, if the username is taken")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *username == "" {
		fs.Usage()
		return errors.New("-username is required")
	}

	var in io.Reader
	switch {
	case *passwordStdin && *passwordFile != "":
		return errors.New("use only one of -password-stdin and -password-file")
	case *passwordStdin:
		in = os.Stdin
	case *passwordFile != "":
		f, err := os.Open(*passwordFile)
		if err != nil {
			return err
		}
		defer f.Close()
		in = f
	}
	password, generated, err := readOrGeneratePassword(in)
	if err != nil {
		return err
	}
	hash, err := auth.HashPassword(password)
	if err != nil {
		return err
	}

	cfg, err := config.Load()
	if err != nil {
		return err
	}
	ctx := context.Background()
	pool, err := db.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()

	st := store.New(pool)
	if *ifMissing {
		if _, err := st.GetUserByUsername(ctx, *username); err == nil {
			fmt.Printf("user %s already exists\n", *username)
			return nil
		} else if !errors.Is(err, store.ErrNotFound) {
			return err
		}
	}
	u, err := st.CreateLocalUser(ctx, store.NewLocalUser{
		Username: *username, DisplayName: *displayName, Email: *email, PasswordHash: hash, IsAdmin: *admin,
	})
	if errors.Is(err, store.ErrConflict) {
		return fmt.Errorf("user %q already exists", *username)
	}
	if err != nil {
		return err
	}
	if err := st.AddAuditEntry(ctx, store.AuditEntry{
		ActorName: "dispatch-ops user create", AuthMethod: "cli", Action: "create", ResourceType: "user",
		ResourceID: &u.ID, ResourceName: u.Username, After: map[string]any{"username": u.Username, "isAdmin": u.IsAdmin},
	}); err != nil {
		return err
	}
	fmt.Printf("created user %s (id %s, admin: %t)\n", u.Username, u.ID, u.IsAdmin)
	if generated {
		fmt.Printf("password: %s\n", password)
	}
	return nil
}

func grantAdmin(username string) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	ctx := context.Background()
	pool, err := db.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()
	st := store.New(pool)
	u, err := st.GetUserByUsername(ctx, username)
	if errors.Is(err, store.ErrNotFound) {
		return fmt.Errorf("no user %q (single sign-on users exist once they have signed in)", username)
	}
	if err != nil {
		return err
	}
	if err := st.GrantSystemAdmin(ctx, u.ID); err != nil {
		return err
	}
	if err := st.AddAuditEntry(ctx, store.AuditEntry{
		ActorName: "dispatch-ops user grant-admin", AuthMethod: "cli", Action: "grant", ResourceType: "user",
		ResourceID: &u.ID, ResourceName: u.Username, After: map[string]any{"role": "system_admin", "user": u.Username},
	}); err != nil {
		return err
	}
	fmt.Printf("%s is a system administrator\n", u.Username)
	return nil
}

// readOrGeneratePassword reads the first line of in, or generates a
// password if in is nil.
func readOrGeneratePassword(in io.Reader) (password string, generated bool, err error) {
	if in == nil {
		return rand.Text(), true, nil
	}
	line, err := bufio.NewReader(in).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", false, err
	}
	password = strings.TrimRight(line, "\r\n")
	if len(password) < 12 {
		return "", false, errors.New("password must be at least 12 characters")
	}
	return password, false, nil
}
