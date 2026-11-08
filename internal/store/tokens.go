package store

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// TokenKind distinguishes browser sessions from API tokens.
type TokenKind string

const (
	TokenKindSession TokenKind = "session"
	TokenKindAPI     TokenKind = "api"
)

// TokenScope limits what an API token may do.
type TokenScope string

const (
	// ScopeRead tokens may only make GET requests.
	ScopeRead TokenScope = "read"
	// ScopeWrite tokens may do anything their user may.
	ScopeWrite TokenScope = "write"
)

// Token is a row in auth_tokens. The secret itself is never stored.
type Token struct {
	ID         uuid.UUID
	UserID     uuid.UUID
	Kind       TokenKind
	Name       string
	Scope      TokenScope
	Prefix     string
	CreatedAt  time.Time
	ExpiresAt  *time.Time
	LastUsedAt *time.Time
}

const tokenColumns = `t.id, t.user_id, t.kind, t.name, t.scope, t.token_prefix, t.created_at, t.expires_at, t.last_used_at`

func (t *Token) scanTargets() []any {
	return []any{&t.ID, &t.UserID, &t.Kind, &t.Name, &t.Scope, &t.Prefix, &t.CreatedAt, &t.ExpiresAt, &t.LastUsedAt}
}

// NewToken holds the fields for storing a token.
type NewToken struct {
	UserID    uuid.UUID
	Kind      TokenKind
	Name      string
	Scope     TokenScope // ScopeWrite if empty
	Hash      []byte
	Prefix    string
	ExpiresAt *time.Time
}

// CreateToken stores a token hash.
func (s *Store) CreateToken(ctx context.Context, in NewToken) (Token, error) {
	if in.Scope == "" {
		in.Scope = ScopeWrite
	}
	var t Token
	err := s.pool.QueryRow(ctx, `
		INSERT INTO auth_tokens AS t (user_id, kind, name, scope, token_hash, token_prefix, expires_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		RETURNING `+tokenColumns,
		in.UserID, in.Kind, in.Name, in.Scope, in.Hash, in.Prefix, in.ExpiresAt,
	).Scan(t.scanTargets()...)
	return t, mapErr(err)
}

// lastUsedGranularity limits how often last_used_at is written, so busy API
// clients don't cause a write on every request.
const lastUsedGranularity = time.Minute

// LookupToken finds an unexpired token by hash, owned by an active user, and
// records that it was used. It returns ErrNotFound otherwise.
func (s *Store) LookupToken(ctx context.Context, hash []byte) (Token, User, error) {
	var t Token
	var u User
	err := s.pool.QueryRow(ctx, `
		SELECT `+tokenColumns+`, `+userColumns+`
		FROM auth_tokens t JOIN users u ON u.id = t.user_id
		WHERE t.token_hash = $1
		  AND u.is_active
		  AND (t.expires_at IS NULL OR t.expires_at > now())`, hash,
	).Scan(append(t.scanTargets(), u.scanTargets()...)...)
	if err != nil {
		return Token{}, User{}, mapErr(err)
	}
	if t.LastUsedAt == nil || time.Since(*t.LastUsedAt) > lastUsedGranularity {
		if _, err := s.pool.Exec(ctx, `UPDATE auth_tokens SET last_used_at = now() WHERE id = $1`, t.ID); err != nil {
			return Token{}, User{}, err
		}
	}
	return t, u, nil
}

// ListTokens returns a user's tokens of one kind, newest first.
func (s *Store) ListTokens(ctx context.Context, userID uuid.UUID, kind TokenKind) ([]Token, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT `+tokenColumns+` FROM auth_tokens t
		WHERE t.user_id = $1 AND t.kind = $2
		ORDER BY t.created_at DESC`, userID, kind)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (Token, error) {
		var t Token
		err := row.Scan(t.scanTargets()...)
		return t, err
	})
}

// DeleteToken deletes one of a user's tokens and returns it. It returns
// ErrNotFound if the user has no such token of that kind.
func (s *Store) DeleteToken(ctx context.Context, userID, tokenID uuid.UUID, kind TokenKind) (Token, error) {
	var t Token
	err := s.pool.QueryRow(ctx, `
		DELETE FROM auth_tokens t WHERE t.id = $1 AND t.user_id = $2 AND t.kind = $3
		RETURNING `+tokenColumns, tokenID, userID, kind).Scan(t.scanTargets()...)
	return t, mapErr(err)
}

// DeleteExpiredTokens removes expired sessions and API tokens.
func (s *Store) DeleteExpiredTokens(ctx context.Context) (int64, error) {
	tag, err := s.pool.Exec(ctx, `DELETE FROM auth_tokens WHERE expires_at <= now()`)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}
