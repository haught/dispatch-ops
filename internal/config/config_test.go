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
