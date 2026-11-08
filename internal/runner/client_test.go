package runner

import (
	"crypto/tls"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"dispatch-ops/internal/runner/runnerapi"
	"dispatch-ops/internal/tlscert"
)

// The runner reaches an API serving a self-signed certificate only when
// told to trust it.
func TestClientTrustsAPICertificate(t *testing.T) {
	certPEM, keyPEM, err := tlscert.SelfSigned([]string{"127.0.0.1"}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok" {
			w.WriteHeader(http.StatusUnauthorized)
		}
	}))
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{cert}}
	srv.StartTLS()
	defer srv.Close()

	get := func(ca []byte) error {
		c, err := newClient(srv.URL, "tok", "test", ca)
		if err != nil {
			t.Fatal(err)
		}
		req, _ := http.NewRequest(http.MethodGet, srv.URL, nil)
		req.Header.Set("Authorization", "Bearer tok")
		resp, err := c.ClientInterface.(*runnerapi.Client).Client.Do(req)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				t.Errorf("status %d", resp.StatusCode)
			}
		}
		return err
	}
	if err := get(certPEM); err != nil {
		t.Errorf("trusting the certificate: %v", err)
	}
	if err := get(nil); err == nil {
		t.Error("without the certificate: connected to an untrusted server")
	}
	if _, err := newClient(srv.URL, "tok", "test", []byte("not PEM")); err == nil {
		t.Error("bad CA: want an error")
	}
}
