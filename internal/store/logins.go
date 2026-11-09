package store

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"net/netip"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// LoginLimits bounds failed password sign-ins within a sliding Window. A
// zero maximum turns that limit off.
//
// The limit on one username from one client address is the tight one: it
// stops guessing a password without letting anyone else lock the account's
// owner out from their own address. The limit on a username from all
// addresses is far higher, so locking an account out takes many addresses,
// and it doesn't apply to an address the username has signed in from with
// its password within KnownLoginClientPeriod, so even then the owner can
// still sign in from the addresses they use.
type LoginLimits struct {
	Window time.Duration
	// MaxPerUsernameIP is how many failures one username may have from one
	// client address.
	MaxPerUsernameIP int
	// MaxPerUsername is how many failures one username may have from all
	// addresses together, before attempts from addresses it hasn't signed
	// in from are refused.
	MaxPerUsername int
	// MaxPerIP is how many failures one client address may have, for any
	// usernames.
	MaxPerIP int
}

// Enabled reports whether any limit applies.
func (l LoginLimits) Enabled() bool {
	return l.Window > 0 && (l.MaxPerUsernameIP > 0 || l.MaxPerUsername > 0 || l.MaxPerIP > 0)
}

// LoginLimit names one of LoginLimits' limits.
type LoginLimit string

const (
	LoginLimitUsernameIP LoginLimit = "username_ip"
	LoginLimitUsername   LoginLimit = "username"
	LoginLimitIP         LoginLimit = "ip"
)

// loginUsernameHash keys failures by the username as typed, lowercased like
// sign-in matches it, and hashed so the table never holds a password typed
// into the wrong field.
func loginUsernameHash(username string) []byte {
	h := sha256.Sum256([]byte(strings.ToLower(username)))
	return h[:]
}

// IPv6 prefixes whose addresses translate or tunnel IPv4 clients, which
// keying by /64 would lump together.
var (
	// NAT64 (RFC 6052, RFC 8215) embeds the IPv4 client in the last 32 bits.
	nat64Prefixes = []netip.Prefix{netip.MustParsePrefix("64:ff9b::/96"), netip.MustParsePrefix("64:ff9b:1::/48")}
	// Teredo (RFC 4380) puts many clients in one server prefix; each
	// address ends in its client's public IPv4 address, bit-inverted. The
	// mapped port varies, so the whole address can't be the key.
	teredoPrefix = netip.MustParsePrefix("2001::/32")
)

// LoginClientKey is the client address failures are counted against. An
// IPv6 client is keyed by its /64, the smallest block a site is usually
// given, so an attacker can't get a fresh limit from each address in it.
// IPv4 addresses, including IPv4-mapped IPv6 ones, are kept whole. So are
// IPv4 clients behind NAT64, keyed by the IPv4 address in the last 32 bits,
// and Teredo clients, keyed by their whole address, since many of each
// share a /64.
func LoginClientKey(clientIP string) string {
	addr, err := netip.ParseAddr(clientIP)
	if err != nil {
		return clientIP
	}
	addr = addr.Unmap().WithZone("")
	if addr.Is4() {
		return addr.String()
	}
	for _, p := range nat64Prefixes {
		if p.Contains(addr) {
			b := addr.As16()
			return netip.AddrFrom4([4]byte(b[12:])).String()
		}
	}
	if teredoPrefix.Contains(addr) {
		b := addr.As16()
		return netip.AddrFrom4([4]byte{^b[12], ^b[13], ^b[14], ^b[15]}).String()
	}
	p, err := addr.Prefix(64)
	if err != nil {
		return clientIP
	}
	return p.String()
}

// KnownLoginClientPeriod is how long a client address a username signed in
// from stays exempt from the username's total limit.
const KnownLoginClientPeriod = 90 * 24 * time.Hour

// loginLockKey is a Postgres advisory lock key for sign-ins sharing key.
func loginLockKey(kind string, key []byte) int64 {
	h := sha256.Sum256(append([]byte("dispatch-ops login "+kind+"\x00"), key...))
	return int64(binary.BigEndian.Uint64(h[:8]))
}

// LoginAttempt is ReserveLoginAttempt's result.
type LoginAttempt struct {
	// ID is the failure reserved for this attempt, set when it may go
	// ahead. It counts as a failure until LoginSucceeded or
	// ReleaseLoginAttempt removes it.
	ID int64
	// RetryAfter, if positive, is how long until the attempt would be
	// allowed; it was refused and nothing was recorded.
	RetryAfter time.Duration
	// Tripped are the limits this refusal is the first to meet in the
	// window, for the audit log. Later refusals by the same limit within
	// the window leave it empty, so a flood of attempts isn't logged one by
	// one.
	Tripped []LoginLimit
}

// ErrLoginBusy means ReserveLoginAttempt waited too long for other attempts
// for the same username or address; the attempt was refused and nothing was
// recorded.
var ErrLoginBusy = errors.New("too many concurrent sign-in attempts")

// loginLockTimeout bounds how long ReserveLoginAttempt waits for other
// attempts for the same username or address, so a flood of them can't hold
// pool connections for long.
const loginLockTimeout = "2s"

// ReserveLoginAttempt checks limits for a password sign-in for username
// from clientIP, and if it may go ahead, records it as a failure before the
// password is checked. Attempts for the same username or from the same
// address are serialized, so concurrent attempts can't all pass the check
// before any of them is counted. Call LoginSucceeded if the password turns
// out to be right, or ReleaseLoginAttempt if it is never checked.
//
// An attempt that a limit already refuses is turned away by a read-only
// check that takes no locks, so a flood of refused attempts doesn't queue
// on them; only attempts that may go ahead are serialized. If the locks
// can't be had within loginLockTimeout, it returns ErrLoginBusy.
func (s *Store) ReserveLoginAttempt(ctx context.Context, username, clientIP string, limits LoginLimits) (LoginAttempt, error) {
	var out LoginAttempt
	if !limits.Enabled() {
		return out, nil
	}
	user, ip := loginUsernameHash(username), LoginClientKey(clientIP)

	// Most attempts in a flood are refused: check without locks first. The
	// lockout notes are upserts that need no lock either.
	reached, wait, err := loginLimitsReached(ctx, s.pool, user, ip, limits)
	if err != nil {
		return LoginAttempt{}, err
	}
	if len(reached) > 0 {
		return refuseLoginAttempt(ctx, s.pool, user, ip, limits, reached, wait)
	}

	err = s.inTx(ctx, func(tx pgx.Tx) error {
		out = LoginAttempt{}
		if _, err := tx.Exec(ctx, `SET LOCAL lock_timeout = '`+loginLockTimeout+`'`); err != nil {
			return err
		}
		// Always the username's lock first, then the address's: a fixed
		// order can't deadlock.
		for _, key := range []int64{loginLockKey("user", user), loginLockKey("ip", []byte(ip))} {
			if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, key); err != nil {
				return err
			}
		}
		// Check again under the locks: attempts that passed the first check
		// together may have filled the limit since.
		reached, wait, err := loginLimitsReached(ctx, tx, user, ip, limits)
		if err != nil {
			return err
		}
		if len(reached) == 0 {
			return tx.QueryRow(ctx,
				`INSERT INTO login_failures (username_hash, client_ip) VALUES ($1, $2) RETURNING id`,
				user, ip).Scan(&out.ID)
		}
		out, err = refuseLoginAttempt(ctx, tx, user, ip, limits, reached, wait)
		return err
	})
	if pgErr := (*pgconn.PgError)(nil); errors.As(err, &pgErr) && pgErr.Code == "55P03" { // lock_not_available
		return LoginAttempt{}, ErrLoginBusy
	}
	if err != nil {
		return LoginAttempt{}, err
	}
	return out, nil
}

// loginLimitsReached returns the limits that failures for user from ip
// already meet, and how long until the last of them lifts.
func loginLimitsReached(ctx context.Context, q querier, user []byte, ip string, limits LoginLimits) ([]LoginLimit, time.Duration, error) {
	// Each limit is reached when its max-th most recent failure is still
	// inside the window; it lifts when that failure leaves it.
	var wait time.Duration
	var reached []LoginLimit
	check := func(limit LoginLimit, where string, maxFailures int, args ...any) error {
		if maxFailures <= 0 {
			return nil
		}
		window, offset := "$"+strconv.Itoa(len(args)+1), "$"+strconv.Itoa(len(args)+2)
		var secs float64
		err := q.QueryRow(ctx, `
			SELECT extract(epoch FROM failed_at + make_interval(secs => `+window+`) - now())::float8
			FROM login_failures
			WHERE `+where+` AND failed_at > now() - make_interval(secs => `+window+`)
			ORDER BY failed_at DESC
			OFFSET `+offset+` LIMIT 1`,
			append(args, limits.Window.Seconds(), maxFailures-1)...,
		).Scan(&secs)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		reached = append(reached, limit)
		wait = max(wait, time.Duration(secs*float64(time.Second)))
		return nil
	}
	if err := check(LoginLimitUsernameIP, `username_hash = $1 AND client_ip = $2`, limits.MaxPerUsernameIP, user, ip); err != nil {
		return nil, 0, err
	}
	// The total doesn't apply to a client the username has signed in from:
	// failures from elsewhere mustn't lock the owner out of their own
	// address (the per-address limits still apply to it).
	if err := check(LoginLimitUsername, `username_hash = $1 AND NOT EXISTS (
			SELECT 1 FROM login_known_clients k
			WHERE k.username_hash = $1 AND k.client_ip = $2 AND k.signed_in_at > now() - make_interval(secs => $3))`,
		limits.MaxPerUsername, user, ip, KnownLoginClientPeriod.Seconds()); err != nil {
		return nil, 0, err
	}
	if err := check(LoginLimitIP, `client_ip = $1`, limits.MaxPerIP, ip); err != nil {
		return nil, 0, err
	}
	return reached, wait, nil
}

// loginLockoutKey is the login_lockouts key of limit for user from ip.
func loginLockoutKey(limit LoginLimit, user []byte, ip string) [32]byte {
	var key []byte
	switch limit {
	case LoginLimitUsernameIP:
		key = append(append([]byte{}, user...), ip...)
	case LoginLimitUsername:
		key = user
	case LoginLimitIP:
		key = []byte(ip)
	}
	return sha256.Sum256(append([]byte(string(limit)+"\x00"), key...))
}

// refuseLoginAttempt returns the refusal of an attempt by the reached
// limits, and notes the first refusal by each limit in a window, for the
// audit log; the note lasts a window.
func refuseLoginAttempt(
	ctx context.Context, q querier, user []byte, ip string, limits LoginLimits, reached []LoginLimit, wait time.Duration,
) (LoginAttempt, error) {
	out := LoginAttempt{RetryAfter: max(wait, time.Second)}
	for _, limit := range reached {
		h := loginLockoutKey(limit, user, ip)
		// Most refusals find the note already there: check without writing,
		// since the upsert would lock the row and use a transaction ID and
		// WAL on every refused attempt in a flood.
		var live bool
		if err := q.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM login_lockouts WHERE key = $1 AND until > now())`, h[:]).Scan(&live); err != nil {
			return LoginAttempt{}, err
		}
		if live {
			continue
		}
		tag, err := q.Exec(ctx, `
			INSERT INTO login_lockouts (key, until) VALUES ($1, now() + make_interval(secs => $2))
			ON CONFLICT (key) DO UPDATE SET until = EXCLUDED.until WHERE login_lockouts.until <= now()`,
			h[:], limits.Window.Seconds())
		if err != nil {
			return LoginAttempt{}, err
		}
		if tag.RowsAffected() > 0 {
			out.Tripped = append(out.Tripped, limit)
		}
	}
	return out, nil
}

// LoginSucceeded forgets the reserved attempt id, and username's other
// failures from clientIP, after it signs in, so its limit there starts
// afresh. Failures from other addresses still count. It also records
// clientIP as known for username, so the username's total limit doesn't
// apply to it for KnownLoginClientPeriod.
func (s *Store) LoginSucceeded(ctx context.Context, id int64, username, clientIP string) error {
	_, err := s.pool.Exec(ctx, `
		WITH forgotten AS (
			DELETE FROM login_failures WHERE id = $1 OR (username_hash = $2 AND client_ip = $3)
		)
		INSERT INTO login_known_clients (username_hash, client_ip) VALUES ($2, $3)
		ON CONFLICT (username_hash, client_ip) DO UPDATE SET signed_in_at = now()`,
		id, loginUsernameHash(username), LoginClientKey(clientIP))
	return err
}

// ReleaseLoginAttempt forgets the reserved attempt id, whose password was
// never checked (the server was too busy, or failed).
func (s *Store) ReleaseLoginAttempt(ctx context.Context, id int64) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM login_failures WHERE id = $1`, id)
	return err
}

// DeleteOldLoginFailures removes failures older than window, which no limit
// counts any more, lapsed lockout notes, and client addresses not signed in
// from within KnownLoginClientPeriod.
func (s *Store) DeleteOldLoginFailures(ctx context.Context, window time.Duration) (int64, error) {
	tag, err := s.pool.Exec(ctx, `DELETE FROM login_failures WHERE failed_at <= now() - make_interval(secs => $1)`, window.Seconds())
	if err != nil {
		return 0, err
	}
	if _, err := s.pool.Exec(ctx, `DELETE FROM login_lockouts WHERE until <= now()`); err != nil {
		return 0, err
	}
	if _, err := s.pool.Exec(ctx, `DELETE FROM login_known_clients WHERE signed_in_at <= now() - make_interval(secs => $1)`,
		KnownLoginClientPeriod.Seconds()); err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}
