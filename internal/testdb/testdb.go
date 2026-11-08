// Package testdb gives database tests their own migrated database.
package testdb

import (
	"context"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"dispatch-ops/internal/db"
)

// Open connects to a database for one test package, creating and migrating
// it if needed. It is named after the database in DISPATCH_TEST_DATABASE_URL
// plus suffix, so packages, which `go test` runs in parallel, don't see each
// other's jobs. The test is skipped if DISPATCH_TEST_DATABASE_URL is unset.
func Open(t *testing.T, suffix string) (*pgxpool.Pool, string) {
	t.Helper()
	base := os.Getenv("DISPATCH_TEST_DATABASE_URL")
	if base == "" {
		t.Skip("DISPATCH_TEST_DATABASE_URL not set; skipping database test (see `make test-db`)")
	}
	u, err := url.Parse(base)
	if err != nil {
		t.Fatal(err)
	}
	name := strings.TrimPrefix(u.Path, "/") + "_" + suffix

	ctx := context.Background()
	admin, err := pgx.Connect(ctx, base)
	if err != nil {
		t.Fatal(err)
	}
	var exists bool
	if err := admin.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_database WHERE datname = $1)`, name).Scan(&exists); err != nil {
		t.Fatal(err)
	}
	if !exists {
		if _, err := admin.Exec(ctx, `CREATE DATABASE `+pgx.Identifier{name}.Sanitize()); err != nil && !strings.Contains(err.Error(), "already exists") {
			t.Fatal(err)
		}
	}
	admin.Close(ctx)

	u.Path = "/" + name
	dsn := u.String()
	if err := db.MigrateUp(dsn); err != nil {
		t.Fatal(err)
	}
	pool, err := db.Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return pool, dsn
}
