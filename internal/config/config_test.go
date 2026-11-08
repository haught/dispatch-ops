package config

import (
	"testing"
	"time"
)

func TestParseRetention(t *testing.T) {
	for in, want := range map[string]time.Duration{
		"0": 0, "90d": 90 * 24 * time.Hour, "720h": 720 * time.Hour, "0d": 0,
	} {
		if got, err := ParseRetention(in); err != nil || got != want {
			t.Errorf("ParseRetention(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	for _, in := range []string{"", "x", "-1d", "1.5d", "30m", "-2h"} {
		if _, err := ParseRetention(in); err == nil {
			t.Errorf("ParseRetention(%q) succeeded", in)
		}
	}
}

func TestMetricsAddr(t *testing.T) {
	t.Setenv("DISPATCH_DATABASE_URL", "postgres://x")
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if got := c.MetricsAddrOr(":9090"); got != ":9090" {
		t.Errorf("default = %q", got)
	}
	t.Setenv("DISPATCH_METRICS_ADDR", "")
	c, _ = Load()
	if got := c.MetricsAddrOr(":9090"); got != "" {
		t.Errorf("empty turns metrics off, got %q", got)
	}
	t.Setenv("DISPATCH_METRICS_ADDR", "127.0.0.1:9100")
	c, _ = Load()
	if got := c.MetricsAddrOr(":9090"); got != "127.0.0.1:9100" {
		t.Errorf("set = %q", got)
	}
}

func TestTLS(t *testing.T) {
	t.Setenv("DISPATCH_DATABASE_URL", "postgres://x")
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if !c.TLS || c.ListenAddr != ":8443" {
		t.Errorf("default: TLS %t on %q, want true on :8443", c.TLS, c.ListenAddr)
	}

	t.Setenv("DISPATCH_TLS", "false")
	c, _ = Load()
	if c.TLS || c.ListenAddr != ":8080" {
		t.Errorf("TLS off: TLS %t on %q, want false on :8080", c.TLS, c.ListenAddr)
	}
	t.Setenv("DISPATCH_LISTEN_ADDR", ":9000")
	c, _ = Load()
	if c.ListenAddr != ":9000" {
		t.Errorf("explicit listen address = %q", c.ListenAddr)
	}

	t.Setenv("DISPATCH_TLS", "true")
	t.Setenv("DISPATCH_TLS_CERT_FILE", "/tls/tls.crt")
	if _, err := Load(); err == nil {
		t.Error("cert without key: want an error")
	}
	t.Setenv("DISPATCH_TLS_KEY_FILE", "/tls/tls.key")
	if c, err = Load(); err != nil || c.TLSCertFile != "/tls/tls.crt" || c.TLSKeyFile != "/tls/tls.key" {
		t.Errorf("cert and key: %+v, %v", c, err)
	}
}
