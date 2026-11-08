// Package config loads runtime configuration from environment variables.
package config

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"dispatch-ops/internal/secrets"
)

// Config is the configuration shared by the dispatch-ops commands.
type Config struct {
	// DatabaseURL is a PostgreSQL connection string (DISPATCH_DATABASE_URL).
	DatabaseURL string
	// ListenAddr is the address the API server binds to (DISPATCH_LISTEN_ADDR).
	ListenAddr string
	// SecureCookies marks the session cookie Secure; turn off only for plain-HTTP
	// local development (DISPATCH_SECURE_COOKIES).
	SecureCookies bool
	// SessionTTL is how long a browser session lasts (DISPATCH_SESSION_TTL).
	SessionTTL time.Duration
	// AutoMigrate applies pending migrations when the server starts
	// (DISPATCH_AUTO_MIGRATE).
	AutoMigrate bool
	// LogLevel is debug, info, warn or error (DISPATCH_LOG_LEVEL).
	LogLevel string
	// LogFormat is text or json (DISPATCH_LOG_FORMAT).
	LogFormat string

	// EncryptionKey encrypts credentials: base64 of 32 random bytes
	// (DISPATCH_ENCRYPTION_KEY). Required by serve and task-manager.
	EncryptionKey string
	// OldEncryptionKeys are previous keys, still used to decrypt, during a
	// key rotation (DISPATCH_OLD_ENCRYPTION_KEYS, comma-separated).
	OldEncryptionKeys []string

	TaskManager TaskManager
}

// TaskManager configures the task-manager command.
type TaskManager struct {
	// Namespace is where job pods run (DISPATCH_JOB_NAMESPACE).
	Namespace string
	// KubeContext selects a kubeconfig context when running outside the
	// cluster (DISPATCH_KUBE_CONTEXT). In a pod, the in-cluster config is used.
	KubeContext string
	// RunnerAPIURL is the API server's base URL as job pods reach it
	// (DISPATCH_RUNNER_API_URL), e.g. http://dispatch-ops.dispatch-system.svc:8080.
	RunnerAPIURL string
	// MaxConcurrentJobs limits jobs waiting or running at once
	// (DISPATCH_MAX_CONCURRENT_JOBS).
	MaxConcurrentJobs int
	// JobTTL is how long finished Kubernetes Jobs and their pods are kept for
	// debugging (DISPATCH_JOB_TTL).
	JobTTL time.Duration
	// StartTimeout is how long a job may wait for its pod to start its runner
	// before it fails (DISPATCH_JOB_START_TIMEOUT).
	StartTimeout time.Duration
	// LeaderElection makes replicas elect one active task manager with a
	// Kubernetes Lease (DISPATCH_LEADER_ELECTION).
	LeaderElection bool
}

// Load reads configuration from the environment and validates what every
// command needs. Command-specific settings are checked by Keyring and
// ValidateTaskManager.
func Load() (Config, error) {
	c := Config{
		DatabaseURL:   os.Getenv("DISPATCH_DATABASE_URL"),
		ListenAddr:    getenv("DISPATCH_LISTEN_ADDR", ":8080"),
		LogLevel:      getenv("DISPATCH_LOG_LEVEL", "info"),
		LogFormat:     getenv("DISPATCH_LOG_FORMAT", "json"),
		EncryptionKey: os.Getenv("DISPATCH_ENCRYPTION_KEY"),
		TaskManager: TaskManager{
			Namespace:    getenv("DISPATCH_JOB_NAMESPACE", "dispatch-jobs"),
			KubeContext:  os.Getenv("DISPATCH_KUBE_CONTEXT"),
			RunnerAPIURL: strings.TrimRight(os.Getenv("DISPATCH_RUNNER_API_URL"), "/"),
		},
	}
	if v := os.Getenv("DISPATCH_OLD_ENCRYPTION_KEYS"); v != "" {
		c.OldEncryptionKeys = strings.Split(v, ",")
	}

	var errs []error
	collect := func(err error) {
		if err != nil {
			errs = append(errs, err)
		}
	}
	var err error
	c.SecureCookies, err = getbool("DISPATCH_SECURE_COOKIES", true)
	collect(err)
	c.AutoMigrate, err = getbool("DISPATCH_AUTO_MIGRATE", false)
	collect(err)
	c.SessionTTL, err = getduration("DISPATCH_SESSION_TTL", 12*time.Hour)
	collect(err)
	tm := &c.TaskManager
	tm.MaxConcurrentJobs, err = getint("DISPATCH_MAX_CONCURRENT_JOBS", 10)
	collect(err)
	tm.JobTTL, err = getduration("DISPATCH_JOB_TTL", time.Hour)
	collect(err)
	tm.StartTimeout, err = getduration("DISPATCH_JOB_START_TIMEOUT", 10*time.Minute)
	collect(err)
	tm.LeaderElection, err = getbool("DISPATCH_LEADER_ELECTION", true)
	collect(err)

	if c.DatabaseURL == "" {
		errs = append(errs, errors.New("DISPATCH_DATABASE_URL is required"))
	}
	switch c.LogFormat {
	case "text", "json":
	default:
		errs = append(errs, fmt.Errorf("DISPATCH_LOG_FORMAT must be text or json, got %q", c.LogFormat))
	}
	return c, errors.Join(errs...)
}

// Keyring returns the credential encryption keyring.
func (c Config) Keyring() (*secrets.Keyring, error) {
	if c.EncryptionKey == "" {
		return nil, errors.New("DISPATCH_ENCRYPTION_KEY is required; generate one with `dispatch-ops gen-key`")
	}
	return secrets.NewKeyring(c.EncryptionKey, c.OldEncryptionKeys...)
}

// ValidateTaskManager checks the settings the task-manager command needs.
func (c Config) ValidateTaskManager() error {
	tm := c.TaskManager
	var errs []error
	if tm.RunnerAPIURL == "" {
		errs = append(errs, errors.New("DISPATCH_RUNNER_API_URL is required: the API URL job pods use"))
	} else if u, err := url.Parse(tm.RunnerAPIURL); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		errs = append(errs, fmt.Errorf("DISPATCH_RUNNER_API_URL must be an http(s) URL, got %q", tm.RunnerAPIURL))
	}
	if tm.MaxConcurrentJobs < 1 {
		errs = append(errs, errors.New("DISPATCH_MAX_CONCURRENT_JOBS must be at least 1"))
	}
	if tm.Namespace == "" {
		errs = append(errs, errors.New("DISPATCH_JOB_NAMESPACE must not be empty"))
	}
	return errors.Join(errs...)
}

func getenv(key, def string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return def
}

func getbool(key string, def bool) (bool, error) {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return def, nil
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return def, fmt.Errorf("%s: %w", key, err)
	}
	return b, nil
}

func getint(key string, def int) (int, error) {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return def, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return def, fmt.Errorf("%s: %w", key, err)
	}
	return n, nil
}

func getduration(key string, def time.Duration) (time.Duration, error) {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return def, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return def, fmt.Errorf("%s: %w", key, err)
	}
	if d <= 0 {
		return def, fmt.Errorf("%s must be positive", key)
	}
	return d, nil
}
