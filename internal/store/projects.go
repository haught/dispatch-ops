package store

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"dispatch-ops/internal/rbac"
)

// Project is a row in projects: a git repository holding playbooks and scripts.
type Project struct {
	ID           uuid.UUID
	Name         string
	Description  string
	ScmURL       string
	ScmRef       string
	CredentialID *uuid.UUID
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

const projectColumns = `p.id, p.name, p.description, p.scm_url, p.scm_ref, p.credential_id, p.created_at, p.updated_at`

func (p *Project) scanTargets() []any {
	return []any{&p.ID, &p.Name, &p.Description, &p.ScmURL, &p.ScmRef, &p.CredentialID, &p.CreatedAt, &p.UpdatedAt}
}

// ProjectFields are a project's editable fields.
type ProjectFields struct {
	Name         string
	Description  string
	ScmURL       string
	ScmRef       string
	CredentialID *uuid.UUID
}

// ListProjects returns projects, by name. If readableBy is set, only those
// that user may read.
func (s *Store) ListProjects(ctx context.Context, search string, readableBy *uuid.UUID, page Page) ([]Project, int, error) {
	q := listQuery{
		columns: projectColumns, from: `projects p`,
		where: `p.name ILIKE $1`, args: []any{likePattern(search)}, orderBy: `p.name`,
	}
	q.readableBy(readableBy, rbac.Project, "p.id")
	return list(ctx, s.pool, q, page, scanner[Project]())
}

// GetProject returns one project.
func (s *Store) GetProject(ctx context.Context, id uuid.UUID) (Project, error) {
	return getProject(ctx, s.pool, id)
}

func getProject(ctx context.Context, db querier, id uuid.UUID) (Project, error) {
	var p Project
	err := db.QueryRow(ctx, `SELECT `+projectColumns+` FROM projects p WHERE p.id = $1`, id).Scan(p.scanTargets()...)
	return p, mapErr(err)
}

// CreateProject inserts a project.
func (s *Store) CreateProject(ctx context.Context, in ProjectFields) (Project, error) {
	var p Project
	err := s.pool.QueryRow(ctx, `
		INSERT INTO projects AS p (name, description, scm_url, scm_ref, credential_id)
		VALUES ($1, $2, $3, $4, $5)
		RETURNING `+projectColumns,
		in.Name, in.Description, in.ScmURL, in.ScmRef, in.CredentialID).Scan(p.scanTargets()...)
	return p, mapErr(err)
}

// UpdateProject loads a project's fields, lets update change them, and
// saves the result.
func (s *Store) UpdateProject(ctx context.Context, id uuid.UUID, update func(*ProjectFields) error) (Project, error) {
	var out Project
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		var f ProjectFields
		err := tx.QueryRow(ctx, `
			SELECT name, description, scm_url, scm_ref, credential_id
			FROM projects WHERE id = $1 FOR UPDATE`, id).
			Scan(&f.Name, &f.Description, &f.ScmURL, &f.ScmRef, &f.CredentialID)
		if err != nil {
			return mapErr(err)
		}
		if err := update(&f); err != nil {
			return err
		}
		err = tx.QueryRow(ctx, `
			UPDATE projects AS p
			SET name = $2, description = $3, scm_url = $4, scm_ref = $5, credential_id = $6
			WHERE p.id = $1
			RETURNING `+projectColumns,
			id, f.Name, f.Description, f.ScmURL, f.ScmRef, f.CredentialID).Scan(out.scanTargets()...)
		return mapErr(err)
	})
	return out, err
}

// DeleteProject deletes a project. It returns ErrInUse if a job template
// uses it.
func (s *Store) DeleteProject(ctx context.Context, id uuid.UUID) error {
	return s.deleteByID(ctx, "projects", id)
}
