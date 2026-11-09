package store_test

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"testing"
	"time"

	"dispatch-ops/internal/store"
	"dispatch-ops/internal/testdb"
)

// A flood of attempts that a limit already refuses doesn't queue on the
// locks that attempts which may go ahead take, and those give up rather
// than wait long.
func TestReserveLoginAttemptRefusesWithoutLocking(t *testing.T) {
	pool, _ := testdb.Open(t, "store")
	st := store.New(pool)
	ctx := context.Background()
	limits := store.LoginLimits{Window: time.Minute, MaxPerUsername: 2}
	username := "user-" + rand.Text()[:10]
	// Fresh addresses from 198.18.0.0/15, reserved for testing.
	b := make([]byte, 2)
	_, _ = rand.Read(b)
	from := func(n int) string { return fmt.Sprintf("198.%d.%d.%d", 18+b[0]%2, b[1], n) }

	for i := range 2 {
		a, err := st.ReserveLoginAttempt(ctx, username, from(i), limits)
		if err != nil || a.ID == 0 || a.RetryAfter != 0 {
			t.Fatalf("attempt %d = %+v, %v; want it to go ahead", i, a, err)
		}
	}

	// Another connection holds the username's lock, as a slow attempt
	// would.
	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	userKey, _ := store.LoginLockKeys(username, from(9))
	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock($1)`, userKey); err != nil {
		t.Fatal(err)
	}
	defer func() { _, _ = conn.Exec(ctx, `SELECT pg_advisory_unlock($1)`, userKey) }()

	// The username is over its limit, so the attempt is refused at once.
	start := time.Now()
	a, err := st.ReserveLoginAttempt(ctx, username, from(9), limits)
	if err != nil || a.ID != 0 || a.RetryAfter <= 0 {
		t.Fatalf("attempt over the limit = %+v, %v; want refused", a, err)
	}
	if d := time.Since(start); d > time.Second {
		t.Errorf("refusal took %v while another attempt held the lock", d)
	}

	// An attempt under the limits must take the lock, and gives up after
	// the lock timeout with nothing recorded.
	other := "user-" + rand.Text()[:10]
	otherKey, _ := store.LoginLockKeys(other, from(9))
	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock($1)`, otherKey); err != nil {
		t.Fatal(err)
	}
	defer func() { _, _ = conn.Exec(ctx, `SELECT pg_advisory_unlock($1)`, otherKey) }()
	start = time.Now()
	a, err = st.ReserveLoginAttempt(ctx, other, from(9), limits)
	if !errors.Is(err, store.ErrLoginBusy) || a.ID != 0 {
		t.Fatalf("attempt waiting on the lock = %+v, %v; want ErrLoginBusy", a, err)
	}
	if d := time.Since(start); d > 10*time.Second {
		t.Errorf("gave up after %v", d)
	}
}

// Refused attempts that find the limit's lockout note already there leave
// it alone: no upsert that would lock the row and write WAL on each one in
// a flood. The note is still written, and audited, once.
func TestRefusedLoginAttemptsDontRewriteLockoutNote(t *testing.T) {
	pool, _ := testdb.Open(t, "store")
	st := store.New(pool)
	ctx := context.Background()
	limits := store.LoginLimits{Window: time.Minute, MaxPerUsername: 1}
	username := "user-" + rand.Text()[:10]
	b := make([]byte, 2)
	_, _ = rand.Read(b)
	from := func(n int) string { return fmt.Sprintf("198.%d.%d.%d", 18+b[0]%2, b[1], n) }

	if a, err := st.ReserveLoginAttempt(ctx, username, from(1), limits); err != nil || a.ID == 0 {
		t.Fatalf("first attempt = %+v, %v; want it to go ahead", a, err)
	}
	key := store.LoginLockoutKey(store.LoginLimitUsername, username, from(2))
	xmax := func() string {
		t.Helper()
		var x string
		if err := pool.QueryRow(ctx, `SELECT xmax::text FROM login_lockouts WHERE key = $1`, key).Scan(&x); err != nil {
			t.Fatal(err)
		}
		return x
	}
	for i := range 4 {
		a, err := st.ReserveLoginAttempt(ctx, username, from(2+i), limits)
		if err != nil || a.ID != 0 || a.RetryAfter <= 0 {
			t.Fatalf("attempt over the limit = %+v, %v; want refused", a, err)
		}
		if tripped := len(a.Tripped) > 0; tripped != (i == 0) {
			t.Errorf("refusal %d tripped %v", i, a.Tripped)
		}
		// A row lock (or update) by the upsert would set xmax.
		if x := xmax(); x != "0" {
			t.Fatalf("after refusal %d the lockout note's xmax = %s; it was locked or rewritten", i, x)
		}
	}
}
