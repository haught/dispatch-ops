// Package server assembles the top-level HTTP handler: the API (including the
// runner API), health probes and the embedded single-page app.
package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"io/fs"
	"log/slog"
	"net/http"
	"path"
	"regexp"
	"strconv"
	"strings"
	"time"

	"dispatch-ops/internal/metrics"
	"dispatch-ops/internal/store"
)

// spaFallback is the page adapter-static generates for client-side routes.
const spaFallback = "200.html"

// New returns the handler for the whole server.
func New(apiHandler http.Handler, st *store.Store, ui fs.FS, log *slog.Logger, opts ...Option) http.Handler {
	o := options{bodyReadTimeout: DefaultBodyReadTimeout}
	for _, opt := range opts {
		opt(&o)
	}
	mux := http.NewServeMux()
	mux.Handle("/api/", apiHandler)
	mux.Handle("/runner/", apiHandler)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte("ok\n"))
	})
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		w.Header().Set("Content-Type", "text/plain")
		if err := st.Ping(ctx); err != nil {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte("database unavailable\n"))
			return
		}
		_, _ = w.Write([]byte("ok\n"))
	})
	// "/" rather than "GET /": a method-specific catch-all would conflict with "/api/".
	mux.Handle("/", getOnly(spa(ui, log)))

	var h http.Handler = mux
	h = limitBodyReadTime(o.bodyReadTimeout, h)
	h = withClientIP(o.trustedProxies, h)
	h = http.NewCrossOriginProtection().Handler(h)
	h = securityHeaders(contentSecurityPolicy(ui), h)
	h = recoverPanics(log, h)
	h = logRequests(log, h)
	return h
}

// spa serves the built UI: real files as-is, and the SPA fallback page for
// any other path so client-side routes work on reload.
func spa(ui fs.FS, log *slog.Logger) http.Handler {
	index, err := fs.ReadFile(ui, spaFallback)
	if err != nil {
		log.Warn("UI is not built; run `make web` to embed it")
		return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte("The Dispatch Ops UI was not built into this binary. Run `make web` and rebuild.\n"))
		})
	}
	files := http.FileServerFS(ui)

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name := strings.TrimPrefix(path.Clean(r.URL.Path), "/")
		if name != "" && name != spaFallback && !hasDotSegment(name) {
			if info, err := fs.Stat(ui, name); err == nil && !info.IsDir() {
				if strings.HasPrefix(name, "_app/immutable/") {
					w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
				}
				files.ServeHTTP(w, r)
				return
			}
			// A missing asset should 404, not return the app shell.
			if path.Ext(name) != "" {
				http.NotFound(w, r)
				return
			}
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache")
		_, _ = w.Write(index)
	})
}

func getOnly(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func hasDotSegment(name string) bool {
	for seg := range strings.SplitSeq(name, "/") {
		if strings.HasPrefix(seg, ".") {
			return true
		}
	}
	return false
}

// hstsMaxAge is two years, the usual value for HSTS preload lists.
const hstsMaxAge = 2 * 365 * 24 * 60 * 60

// securityHeaders sets the headers every response carries: the content
// security policy, no framing or sniffing, HSTS on TLS connections, and
// no caching of API responses, which may hold secrets such as a new API
// token.
func securityHeaders(csp string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", csp)
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "same-origin")
		// Only on connections this server terminates TLS for: an HSTS header
		// over plain HTTP is ignored, and behind a TLS-terminating proxy the
		// proxy should send it.
		if r.TLS != nil {
			h.Set("Strict-Transport-Security", "max-age="+strconv.Itoa(hstsMaxAge)+"; includeSubDomains")
		}
		if strings.HasPrefix(r.URL.Path, "/api/") || strings.HasPrefix(r.URL.Path, "/runner/") {
			h.Set("Cache-Control", "no-store")
		}
		next.ServeHTTP(w, r)
	})
}

// inlineScript matches a <script> element without a src attribute, which is
// how the SvelteKit fallback page boots the app.
var inlineScript = regexp.MustCompile(`(?s)<script(\s[^>]*)?>(.*?)</script>`)

// contentSecurityPolicy returns the policy for the whole server. The UI is a
// static build served from this origin; the only inline script is the
// bootstrap in the fallback page, which is allowed by its hash rather than
// by 'unsafe-inline'. Styles allow 'unsafe-inline' because Svelte sets style
// attributes and transitions at run time; that can't run script.
func contentSecurityPolicy(ui fs.FS) string {
	scripts := []string{"'self'"}
	if index, err := fs.ReadFile(ui, spaFallback); err == nil {
		for _, m := range inlineScript.FindAllSubmatch(index, -1) {
			if bytes.Contains(bytes.ToLower(m[1]), []byte("src=")) {
				continue
			}
			sum := sha256.Sum256(m[2])
			scripts = append(scripts, "'sha256-"+base64.StdEncoding.EncodeToString(sum[:])+"'")
		}
	}
	return strings.Join([]string{
		"default-src 'self'",
		"script-src " + strings.Join(scripts, " "),
		"style-src 'self' 'unsafe-inline'",
		"img-src 'self' data:",
		"font-src 'self'",
		"connect-src 'self'",
		"object-src 'none'",
		"base-uri 'none'",
		"form-action 'self'",
		"frame-ancestors 'none'",
	}, "; ")
}

func recoverPanics(log *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if v := recover(); v != nil {
				if v == http.ErrAbortHandler {
					panic(v)
				}
				log.ErrorContext(r.Context(), "panic serving request", "method", r.Method, "path", r.URL.Path, "panic", v)
				w.Header().Set("Content-Type", "application/problem+json")
				w.WriteHeader(http.StatusInternalServerError)
				_, _ = w.Write([]byte(`{"status":500,"title":"Internal server error"}` + "\n"))
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// statusRecorder captures the response status for logging. Unwrap lets
// http.ResponseController reach the underlying writer (needed for SSE flushes).
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (s *statusRecorder) WriteHeader(code int) {
	if s.status == 0 {
		s.status = code
	}
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusRecorder) Write(b []byte) (int, error) {
	if s.status == 0 {
		s.status = http.StatusOK
	}
	return s.ResponseWriter.Write(b)
}

func (s *statusRecorder) Unwrap() http.ResponseWriter { return s.ResponseWriter }

// streamOperation is the live job stream, whose duration is how long the
// client watched, so it is left out of the latency histogram.
const streamOperation = "streamJob"

// logRequests logs each request and records its metrics.
func logRequests(log *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w}
		ctx := metrics.WithOperationSlot(r.Context())
		next.ServeHTTP(rec, r.WithContext(ctx))
		elapsed := time.Since(start)
		status := rec.status
		if status == 0 {
			status = http.StatusOK
		}

		op := metrics.Operation(ctx)
		probe := r.URL.Path == "/healthz" || r.URL.Path == "/readyz"
		switch {
		case op != "":
		case probe:
			op = metrics.OpProbe
		case strings.HasPrefix(r.URL.Path, "/api/"), strings.HasPrefix(r.URL.Path, "/runner/"):
			op = metrics.OpUnmatched
		default:
			op = metrics.OpUI
		}
		metrics.HTTPRequests.WithLabelValues(op, r.Method, strconv.Itoa(status)).Inc()
		if op != streamOperation {
			metrics.HTTPDuration.WithLabelValues(op).Observe(elapsed.Seconds())
		}

		if probe {
			return
		}
		level := slog.LevelInfo
		if status >= 500 {
			level = slog.LevelError
		}
		attrs := []slog.Attr{
			slog.String("method", r.Method),
			slog.String("path", r.URL.Path),
			slog.Int("status", status),
			slog.Duration("duration", elapsed),
		}
		if op != metrics.OpUI && op != metrics.OpUnmatched {
			attrs = append(attrs, slog.String("operation", op))
		}
		log.LogAttrs(r.Context(), level, "request", attrs...)
	})
}
