// Package config loads runtime configuration from environment variables.
package config

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"time"
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
}

// Load reads configuration from the environment and validates it.
func Load() (Config, error) {
	c := Config{
		DatabaseURL: os.Getenv("DISPATCH_DATABASE_URL"),
		ListenAddr:  getenv("DISPATCH_LISTEN_ADDR", ":8080"),
		LogLevel:    getenv("DISPATCH_LOG_LEVEL", "info"),
		LogFormat:   getenv("DISPATCH_LOG_FORMAT", "json"),
	}
	var errs []error
	var err error
	if c.SecureCookies, err = getbool("DISPATCH_SECURE_COOKIES", true); err != nil {
		errs = append(errs, err)
	}
	if c.AutoMigrate, err = getbool("DISPATCH_AUTO_MIGRATE", false); err != nil {
		errs = append(errs, err)
	}
	if c.SessionTTL, err = getduration("DISPATCH_SESSION_TTL", 12*time.Hour); err != nil {
		errs = append(errs, err)
	}
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
