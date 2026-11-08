package store

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"dispatch-ops/internal/rbac"
)

// Credential is a row in credentials. SecretInputs is ciphertext; see the
// secrets package.
type Credential struct {
	ID           uuid.UUID
	Name         string
	Description  string
	Kind         string
	PublicInputs map[string]string
	SecretFields []string
	SecretInputs []byte
	KeyID        string
	CreatedBy    *uuid.UUID
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

const credentialColumns = `c.id, c.name, c.description, c.kind, c.public_inputs, c.secret_fields,
	c.secret_inputs, c.key_id, c.created_by, c.created_at, c.updated_at`

func (c *Credential) scanTargets() []any {
	return []any{&c.ID, &c.Name, &c.Description, &c.Kind, &c.PublicInputs, &c.SecretFields,
		&c.SecretInputs, &c.KeyID, &c.CreatedBy, &c.CreatedAt, &c.UpdatedAt}
}

// CredentialFilter narrows ListCredentials.
type CredentialFilter struct {
	Search string
	Kind   string
	// ReadableBy, if set, limits the list to credentials that user may read.
	ReadableBy *uuid.UUID
}

// ListCredentials returns credentials, by name.
func (s *Store) ListCredentials(ctx context.Context, f CredentialFilter, page Page) ([]Credential, int, error) {
	q := listQuery{
		columns: credentialColumns, from: `credentials c`,
		where: `c.name ILIKE $1`, args: []any{likePattern(f.Search)}, orderBy: `c.name`,
	}
	if f.Kind != "" {
		q.where += ` AND c.kind = $2`
		q.args = append(q.args, f.Kind)
	}
	q.readableBy(f.ReadableBy, rbac.Credential, "c.id")
	return list(ctx, s.pool, q, page, scanner[Credential]())
}

// GetCredential returns one credential.
func (s *Store) GetCredential(ctx context.Context, id uuid.UUID) (Credential, error) {
	return getCredential(ctx, s.pool, id, false)
}

func getCredential(ctx context.Context, db querier, id uuid.UUID, forUpdate bool) (Credential, error) {
	sql := `SELECT ` + credentialColumns + ` FROM credentials c WHERE c.id = $1`
	if forUpdate {
		sql += ` FOR UPDATE`
	}
	var c Credential
	err := db.QueryRow(ctx, sql, id).Scan(c.scanTargets()...)
	return c, mapErr(err)
}

// GetCredentials returns the credentials with the given IDs, in that order.
// It returns ErrNotFound if any is missing.
func (s *Store) GetCredentials(ctx context.Context, ids []uuid.UUID) ([]Credential, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT `+credentialColumns+`
		FROM unnest($1::uuid[]) WITH ORDINALITY AS want(id, n)
		JOIN credentials c ON c.id = want.id
		ORDER BY want.n`, ids)
	if err != nil {
		return nil, err
	}
	creds, err := pgx.CollectRows(rows, scanner[Credential]())
	if err != nil {
		return nil, err
	}
	if len(creds) != len(ids) {
		return nil, ErrNotFound
	}
	return creds, nil
}

// CreateCredential inserts a credential. The caller sets the ID, because it
// is bound into the encrypted value.
func (s *Store) CreateCredential(ctx context.Context, c Credential) (Credential, error) {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO credentials (id, name, description, kind, public_inputs, secret_fields, secret_inputs, key_id, created_by)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
		c.ID, c.Name, c.Description, c.Kind, c.PublicInputs, c.SecretFields, c.SecretInputs, c.KeyID, c.CreatedBy)
	if err != nil {
		return Credential{}, mapErr(err)
	}
	return s.GetCredential(ctx, c.ID)
}

// UpdateCredential loads a credential, lets update change it, and saves the
// name, description, inputs and key ID.
func (s *Store) UpdateCredential(ctx context.Context, id uuid.UUID, update func(*Credential) error) (Credential, error) {
	var out Credential
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		c, err := getCredential(ctx, tx, id, true)
		if err != nil {
			return err
		}
		if err := update(&c); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
			UPDATE credentials SET name = $2, description = $3, public_inputs = $4, secret_fields = $5,
				secret_inputs = $6, key_id = $7
			WHERE id = $1`,
			id, c.Name, c.Description, c.PublicInputs, c.SecretFields, c.SecretInputs, c.KeyID); err != nil {
			return mapErr(err)
		}
		out, err = getCredential(ctx, tx, id, false)
		return err
	})
	return out, err
}

// DeleteCredential deletes a credential. It returns ErrInUse if a job
// template uses it.
func (s *Store) DeleteCredential(ctx context.Context, id uuid.UUID) error {
	return s.deleteByID(ctx, "credentials", id)
}
