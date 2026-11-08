package store

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// User is a row in the users table.
type User struct {
	ID           uuid.UUID
	Username     string
	DisplayName  string
	Email        string
	AuthSource   string
	PasswordHash *string
	IsAdmin      bool
	IsActive     bool
	CreatedAt    time.Time
	LastLoginAt  *time.Time
}

const userColumns = `u.id, u.username, u.display_name, u.email, u.auth_source,
	u.password_hash, u.is_admin, u.is_active, u.created_at, u.last_login_at`

func (u *User) scanTargets() []any {
	return []any{&u.ID, &u.Username, &u.DisplayName, &u.Email, &u.AuthSource,
		&u.PasswordHash, &u.IsAdmin, &u.IsActive, &u.CreatedAt, &u.LastLoginAt}
}

// NewLocalUser holds the fields for creating a password-based user.
type NewLocalUser struct {
	Username     string
	DisplayName  string
	Email        string
	PasswordHash string
	IsAdmin      bool
}

// CreateLocalUser inserts a local (password) user. It returns ErrConflict if
// the username is taken, compared case-insensitively.
func (s *Store) CreateLocalUser(ctx context.Context, in NewLocalUser) (User, error) {
	var u User
	err := s.pool.QueryRow(ctx, `
		INSERT INTO users AS u (username, display_name, email, auth_source, password_hash, is_admin)
		VALUES ($1, $2, $3, 'local', $4, $5)
		RETURNING `+userColumns,
		in.Username, in.DisplayName, in.Email, in.PasswordHash, in.IsAdmin,
	).Scan(u.scanTargets()...)
	return u, mapErr(err)
}

// GetUserByUsername looks a user up by username, case-insensitively.
func (s *Store) GetUserByUsername(ctx context.Context, username string) (User, error) {
	var u User
	err := s.pool.QueryRow(ctx,
		`SELECT `+userColumns+` FROM users u WHERE lower(u.username) = lower($1)`, username,
	).Scan(u.scanTargets()...)
	return u, mapErr(err)
}

// GetUser looks a user up by ID.
func (s *Store) GetUser(ctx context.Context, id uuid.UUID) (User, error) {
	var u User
	err := s.pool.QueryRow(ctx,
		`SELECT `+userColumns+` FROM users u WHERE u.id = $1`, id,
	).Scan(u.scanTargets()...)
	return u, mapErr(err)
}

// RecordLogin sets the user's last sign-in time to now.
func (s *Store) RecordLogin(ctx context.Context, id uuid.UUID) error {
	_, err := s.pool.Exec(ctx, `UPDATE users SET last_login_at = now() WHERE id = $1`, id)
	return err
}
