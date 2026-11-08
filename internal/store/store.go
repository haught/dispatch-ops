// Package store is the PostgreSQL data access layer.
package store

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

var (
	// ErrNotFound means the requested row does not exist.
	ErrNotFound = errors.New("not found")
	// ErrConflict means a unique constraint was violated.
	ErrConflict = errors.New("already exists")
	// ErrInvalidReference means a referenced row (e.g. an inventory named by
	// a job template) does not exist.
	ErrInvalidReference = errors.New("referenced resource does not exist")
	// ErrInUse means a row cannot be deleted because others reference it.
	ErrInUse = errors.New("in use")
)

// Store wraps the connection pool.
type Store struct {
	pool *pgxpool.Pool
}

// New returns a Store using pool.
func New(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool}
}

// Ping checks the database connection.
func (s *Store) Ping(ctx context.Context) error {
	return s.pool.Ping(ctx)
}

// Now returns the database's clock. Durations measured from timestamps the
// database set (e.g. now() in a query) should use it, not the local clock,
// which may be skewed.
func (s *Store) Now(ctx context.Context) (time.Time, error) {
	var t time.Time
	err := s.pool.QueryRow(ctx, `SELECT now()`).Scan(&t)
	return t, err
}

// querier is satisfied by the pool and by transactions.
type querier interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// inTx runs fn in a transaction, committing if it returns nil.
func (s *Store) inTx(ctx context.Context, fn func(tx pgx.Tx) error) error {
	return pgx.BeginFunc(ctx, s.pool, fn)
}

// Page selects a slice of a list.
type Page struct {
	Limit  int
	Offset int
}

// likePattern returns a case-insensitive substring pattern for ILIKE, with
// the user's input escaped.
func likePattern(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return "%" + r.Replace(s) + "%"
}

// mapErr converts driver errors to the package's sentinel errors. A foreign
// key violation is reported as ErrInvalidReference; deletes that can hit a
// RESTRICT constraint use mapDeleteErr instead.
func mapErr(err error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch pgErr.Code {
		case "23505": // unique_violation
			return ErrConflict
		case "23503": // foreign_key_violation
			return ErrInvalidReference
		}
	}
	return err
}

// mapDeleteErr is mapErr for deletes, where a foreign key violation means
// another row still references the one being deleted.
func mapDeleteErr(err error) error {
	err = mapErr(err)
	if errors.Is(err, ErrInvalidReference) {
		return ErrInUse
	}
	return err
}

// deleteByID deletes a row from table by primary key.
func (s *Store) deleteByID(ctx context.Context, table string, id any) error {
	tag, err := s.pool.Exec(ctx, `DELETE FROM `+table+` WHERE id = $1`, id)
	if err != nil {
		return mapDeleteErr(err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}
