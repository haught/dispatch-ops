package store

import (
	"context"
	"crypto/sha256"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// LoginLimits bounds failed password sign-ins within a sliding Window, per
// username and per client address. A zero maximum turns that limit off.
type LoginLimits struct {
	Window         time.Duration
	MaxPerUsername int
	MaxPerIP       int
}

// Enabled reports whether any limit applies.
func (l LoginLimits) Enabled() bool {
	return l.Window > 0 && (l.MaxPerUsername > 0 || l.MaxPerIP > 0)
}

// loginUsernameHash keys failures by the username as typed, lowercased like
// sign-in matches it, and hashed so the table never holds a password typed
// into the wrong field.
func loginUsernameHash(username string) []byte {
	h := sha256.Sum256([]byte(strings.ToLower(username)))
	return h[:]
}

// LoginRetryAfter returns how long sign-ins for username, or from clientIP,
// must wait because one of limits is reached, or 0 if they may proceed.
func (s *Store) LoginRetryAfter(ctx context.Context, username, clientIP string, limits LoginLimits) (time.Duration, error) {
	if !limits.Enabled() {
		return 0, nil
	}
	// Each limit is reached when its max-th most recent failure is still
	// inside the window; it lifts when that failure leaves the window.
	var wait time.Duration
	check := func(column string, key any, max int) error {
		if max <= 0 {
			return nil
		}
		var secs float64
		err := s.pool.QueryRow(ctx, `
			SELECT extract(epoch FROM failed_at + make_interval(secs => $2) - now())::float8
			FROM login_failures
			WHERE `+column+` = $1 AND failed_at > now() - make_interval(secs => $2)
			ORDER BY failed_at DESC
			OFFSET $3 LIMIT 1`,
			key, limits.Window.Seconds(), max-1,
		).Scan(&secs)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		if d := time.Duration(secs * float64(time.Second)); d > wait {
			wait = d
		}
		return nil
	}
	if err := check("username_hash", loginUsernameHash(username), limits.MaxPerUsername); err != nil {
		return 0, err
	}
	if err := check("client_ip", clientIP, limits.MaxPerIP); err != nil {
		return 0, err
	}
	if wait > 0 && wait < time.Second {
		wait = time.Second
	}
	return wait, nil
}

// RecordLoginFailure notes a failed password sign-in.
func (s *Store) RecordLoginFailure(ctx context.Context, username, clientIP string) error {
	_, err := s.pool.Exec(ctx,
		`INSERT INTO login_failures (username_hash, client_ip) VALUES ($1, $2)`,
		loginUsernameHash(username), clientIP)
	return err
}

// ClearLoginFailures forgets username's failed sign-ins after it signs in,
// so its limit starts afresh. Failures from the client address still count.
func (s *Store) ClearLoginFailures(ctx context.Context, username string) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM login_failures WHERE username_hash = $1`, loginUsernameHash(username))
	return err
}

// DeleteOldLoginFailures removes failures older than window, which no limit
// counts any more.
func (s *Store) DeleteOldLoginFailures(ctx context.Context, window time.Duration) (int64, error) {
	tag, err := s.pool.Exec(ctx, `DELETE FROM login_failures WHERE failed_at <= now() - make_interval(secs => $1)`, window.Seconds())
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}
