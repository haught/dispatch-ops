// Package server assembles the top-level HTTP handler: the API (including the
// runner API), health probes and the embedded single-page app.
package server

import (
	"context"
	"io/fs"
	"log/slog"
	"net/http"
	"path"
	"strings"
	"time"

	"dispatch-ops/internal/store"
)

// spaFallback is the page adapter-static generates for client-side routes.
const spaFallback = "200.html"

// New returns the handler for the whole server.
func New(apiHandler http.Handler, st *store.Store, ui fs.FS, log *slog.Logger) http.Handler {
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
	h = http.NewCrossOriginProtection().Handler(h)
	h = securityHeaders(h)
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

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "same-origin")
		next.ServeHTTP(w, r)
	})
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

func logRequests(log *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w}
		next.ServeHTTP(rec, r)
		if r.URL.Path == "/healthz" || r.URL.Path == "/readyz" {
			return
		}
		status := rec.status
		if status == 0 {
			status = http.StatusOK
		}
		level := slog.LevelInfo
		if status >= 500 {
			level = slog.LevelError
		}
		log.LogAttrs(r.Context(), level, "request",
			slog.String("method", r.Method),
			slog.String("path", r.URL.Path),
			slog.Int("status", status),
			slog.Duration("duration", time.Since(start)),
		)
	})
}
