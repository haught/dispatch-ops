package server

import (
	"crypto/sha256"
	"encoding/base64"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
)

const bootScript = `
	{
		const element = document.currentScript.parentElement;
		import("/_app/immutable/entry/start.js").then((kit) => kit.start(element));
	}
`

func testUI() fstest.MapFS {
	return fstest.MapFS{
		spaFallback: {Data: []byte(`<!doctype html><html><head>` +
			`<script src="/_app/x.js"></script></head><body><div style="display: contents"><script>` +
			bootScript + `</script></div></body></html>`)},
	}
}

func testHandler() http.Handler {
	api := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"token":"secret"}`)
	})
	return New(api, nil, testUI(), slog.New(slog.DiscardHandler))
}

func TestContentSecurityPolicy(t *testing.T) {
	csp := contentSecurityPolicy(testUI())
	sum := sha256.Sum256([]byte(bootScript))
	want := "'sha256-" + base64.StdEncoding.EncodeToString(sum[:]) + "'"
	if !strings.Contains(csp, "script-src 'self' "+want) {
		t.Errorf("CSP %q doesn't allow the inline boot script by hash %s", csp, want)
	}
	if strings.Count(csp, "'sha256-") != 1 {
		t.Errorf("CSP %q should hash only the inline script, not the external one", csp)
	}
	for _, d := range []string{"object-src 'none'", "base-uri 'none'", "frame-ancestors 'none'", "connect-src 'self'"} {
		if !strings.Contains(csp, d) {
			t.Errorf("CSP %q lacks %q", csp, d)
		}
	}
	if strings.Contains(csp, "script-src 'self' 'unsafe-inline'") {
		t.Errorf("CSP %q allows inline script", csp)
	}
}

func TestSecurityHeaders(t *testing.T) {
	h := testHandler()
	for _, path := range []string{"/", "/jobs/1", "/api/v1/tokens"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		hdr := rec.Result().Header
		if hdr.Get("Content-Security-Policy") == "" {
			t.Errorf("%s: no Content-Security-Policy", path)
		}
		if hdr.Get("Strict-Transport-Security") != "" {
			t.Errorf("%s: HSTS sent over plain HTTP", path)
		}
		cc := hdr.Get("Cache-Control")
		if strings.HasPrefix(path, "/api/") && cc != "no-store" {
			t.Errorf("%s: Cache-Control = %q, want no-store", path, cc)
		}
		if !strings.HasPrefix(path, "/api/") && cc == "no-store" {
			t.Errorf("%s: UI page shouldn't be no-store", path)
		}
	}
}

func TestHSTSOverTLS(t *testing.T) {
	srv := httptest.NewTLSServer(testHandler())
	defer srv.Close()
	resp, err := srv.Client().Get(srv.URL + "/api/v1/me")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if got := resp.Header.Get("Strict-Transport-Security"); !strings.HasPrefix(got, "max-age=63072000") {
		t.Errorf("Strict-Transport-Security = %q, want max-age=63072000...", got)
	}
}
