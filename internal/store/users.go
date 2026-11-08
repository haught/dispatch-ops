package store

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// User is a row in users, with the global roles the user holds directly or
// through a team.
type User struct {
	ID           uuid.UUID
	Username     string
	DisplayName  string
	Email        string
	AuthSource   string
	PasswordHash *string
	IsAdmin      bool
	IsAuditor    bool
	IsActive     bool
	CreatedAt    time.Time
	LastLoginAt  *time.Time
}

const userColumns = `u.id, u.username, u.display_name, u.email, u.auth_source, u.password_hash,
	EXISTS (SELECT 1 FROM user_roles r WHERE r.user_id = u.id AND r.resource_type IS NULL AND r.role = 'system_admin'),
	EXISTS (SELECT 1 FROM user_roles r WHERE r.user_id = u.id AND r.resource_type IS NULL AND r.role = 'auditor'),
	u.is_active, u.created_at, u.last_login_at`

func (u *User) scanTargets() []any {
	return []any{&u.ID, &u.Username, &u.DisplayName, &u.Email, &u.AuthSource, &u.PasswordHash,
		&u.IsAdmin, &u.IsAuditor, &u.IsActive, &u.CreatedAt, &u.LastLoginAt}
}

// NewLocalUser holds the fields for creating a password-based user.
type NewLocalUser struct {
	Username     string
	DisplayName  string
	Email        string
	PasswordHash string
	// IsAdmin grants the system_admin role.
	IsAdmin bool
}

// CreateLocalUser inserts a local (password) user. It returns ErrConflict if
// the username is taken, compared case-insensitively.
func (s *Store) CreateLocalUser(ctx context.Context, in NewLocalUser) (User, error) {
	var out User
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		var id uuid.UUID
		err := tx.QueryRow(ctx, `
			INSERT INTO users (username, display_name, email, auth_source, password_hash)
			VALUES ($1, $2, $3, 'local', $4) RETURNING id`,
			in.Username, in.DisplayName, in.Email, in.PasswordHash).Scan(&id)
		if err != nil {
			return mapErr(err)
		}
		if in.IsAdmin {
			if err := grantSystemAdmin(ctx, tx, id); err != nil {
				return err
			}
		}
		out, err = getUser(ctx, tx, id)
		return err
	})
	return out, err
}

func grantSystemAdmin(ctx context.Context, db querier, userID uuid.UUID) error {
	_, err := db.Exec(ctx, `
		INSERT INTO role_grants (role, user_id) VALUES ('system_admin', $1) ON CONFLICT DO NOTHING`, userID)
	return mapErr(err)
}

// GrantSystemAdmin makes a user a system administrator.
func (s *Store) GrantSystemAdmin(ctx context.Context, userID uuid.UUID) error {
	return grantSystemAdmin(ctx, s.pool, userID)
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
	return getUser(ctx, s.pool, id)
}

func getUser(ctx context.Context, db querier, id uuid.UUID) (User, error) {
	var u User
	err := db.QueryRow(ctx, `SELECT `+userColumns+` FROM users u WHERE u.id = $1`, id).Scan(u.scanTargets()...)
	return u, mapErr(err)
}

// ListUsers returns users whose username, display name or email contains
// search, by username.
func (s *Store) ListUsers(ctx context.Context, search string, page Page) ([]User, int, error) {
	return list(ctx, s.pool, listQuery{
		columns: userColumns, from: `users u`,
		where: `(u.username ILIKE $1 OR u.display_name ILIKE $1 OR u.email ILIKE $1)`,
		args:  []any{likePattern(search)}, orderBy: `lower(u.username)`,
	}, page, scanner[User]())
}

// UserFields are a user's editable fields. PasswordHash is nil for OIDC
// users.
type UserFields struct {
	Username     string
	DisplayName  string
	Email        string
	IsActive     bool
	AuthSource   string // read-only
	PasswordHash *string
}

// UpdateUser loads a user's fields, lets update change them, and saves the
// result.
func (s *Store) UpdateUser(ctx context.Context, id uuid.UUID, update func(*UserFields) error) (User, error) {
	var out User
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		var f UserFields
		err := tx.QueryRow(ctx, `
			SELECT username, display_name, email, is_active, auth_source, password_hash
			FROM users WHERE id = $1 FOR UPDATE`, id).
			Scan(&f.Username, &f.DisplayName, &f.Email, &f.IsActive, &f.AuthSource, &f.PasswordHash)
		if err != nil {
			return mapErr(err)
		}
		if err := update(&f); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
			UPDATE users SET username = $2, display_name = $3, email = $4, is_active = $5, password_hash = $6
			WHERE id = $1`, id, f.Username, f.DisplayName, f.Email, f.IsActive, f.PasswordHash); err != nil {
			return mapErr(err)
		}
		// Deactivating a user ends their sessions; their API tokens stop
		// working while the user is inactive.
		if !f.IsActive {
			if _, err := tx.Exec(ctx, `DELETE FROM auth_tokens WHERE user_id = $1 AND kind = 'session'`, id); err != nil {
				return err
			}
		}
		out, err = getUser(ctx, tx, id)
		return err
	})
	return out, err
}

// DeleteUser deletes a user with their tokens, team memberships and roles.
// Jobs they launched are kept.
func (s *Store) DeleteUser(ctx context.Context, id uuid.UUID) error {
	return s.deleteByID(ctx, "users", id)
}

// RecordLogin sets the user's last sign-in time to now.
func (s *Store) RecordLogin(ctx context.Context, id uuid.UUID) error {
	_, err := s.pool.Exec(ctx, `UPDATE users SET last_login_at = now() WHERE id = $1`, id)
	return err
}

// OIDCIdentity is who an OIDC provider says signed in.
type OIDCIdentity struct {
	Issuer      string
	Subject     string
	Username    string
	DisplayName string
	Email       string
	// Groups are the values of the groups claim, matched against teams'
	// OIDC groups.
	Groups []string
}

// SignInOIDCUser finds the user with id's issuer and subject, or creates
// one, and updates their profile and OIDC team memberships from id. The
// username is only changed if the new one is free. It returns ErrConflict
// if a new user's username is taken. The caller must check IsActive.
func (s *Store) SignInOIDCUser(ctx context.Context, id OIDCIdentity) (User, error) {
	var out User
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		var userID uuid.UUID
		var active bool
		err := tx.QueryRow(ctx, `
			SELECT id, is_active FROM users
			WHERE auth_source = 'oidc' AND oidc_issuer = $1 AND oidc_subject = $2 FOR UPDATE`,
			id.Issuer, id.Subject).Scan(&userID, &active)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			err = tx.QueryRow(ctx, `
				INSERT INTO users (username, display_name, email, auth_source, oidc_issuer, oidc_subject, last_login_at)
				VALUES ($1, $2, $3, 'oidc', $4, $5, now()) RETURNING id`,
				id.Username, id.DisplayName, id.Email, id.Issuer, id.Subject).Scan(&userID)
			if err != nil {
				return mapErr(err)
			}
			active = true
		case err != nil:
			return err
		default:
			if !active {
				out, err = getUser(ctx, tx, userID)
				return err
			}
			if _, err := tx.Exec(ctx, `
				UPDATE users SET display_name = $2, email = $3, last_login_at = now(),
					username = CASE WHEN EXISTS (
						SELECT 1 FROM users o WHERE lower(o.username) = lower($4) AND o.id <> $1
					) THEN username ELSE $4 END
				WHERE id = $1`, userID, id.DisplayName, id.Email, id.Username); err != nil {
				return mapErr(err)
			}
		}

		groups := nonNilStrings(id.Groups)
		if _, err := tx.Exec(ctx, `
			INSERT INTO team_members (team_id, user_id, source)
			SELECT t.id, $1, 'oidc' FROM teams t WHERE t.oidc_groups && $2::text[]
			ON CONFLICT DO NOTHING`, userID, groups); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
			DELETE FROM team_members m USING teams t
			WHERE m.team_id = t.id AND m.user_id = $1 AND m.source = 'oidc'
				AND NOT (t.oidc_groups && $2::text[])`, userID, groups); err != nil {
			return err
		}
		out, err = getUser(ctx, tx, userID)
		return err
	})
	return out, err
}
