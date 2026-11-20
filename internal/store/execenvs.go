package store

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// ExecutionEnvironment is a row in execution_environments: a registered job image.
type ExecutionEnvironment struct {
	ID                   uuid.UUID
	Name                 string
	Description          string
	Image                string
	PullPolicy           string
	RegistryCredentialID *uuid.UUID
	// How many job templates and inventory sources use it.
	TemplateCount        int
	InventorySourceCount int
	CreatedAt            time.Time
	UpdatedAt            time.Time
}

const execEnvColumns = `e.id, e.name, e.description, e.image, e.pull_policy, e.registry_credential_id,
	(SELECT count(*) FROM job_templates WHERE execution_environment_id = e.id),
	(SELECT count(*) FROM inventory_sources WHERE execution_environment_id = e.id),
	e.created_at, e.updated_at`

func (e *ExecutionEnvironment) scanTargets() []any {
	return []any{&e.ID, &e.Name, &e.Description, &e.Image, &e.PullPolicy, &e.RegistryCredentialID,
		&e.TemplateCount, &e.InventorySourceCount, &e.CreatedAt, &e.UpdatedAt}
}

// ExecutionEnvironmentFields are an execution environment's editable fields.
type ExecutionEnvironmentFields struct {
	Name                 string
	Description          string
	Image                string
	PullPolicy           string
	RegistryCredentialID *uuid.UUID
}

// ListExecutionEnvironments returns execution environments, by name.
func (s *Store) ListExecutionEnvironments(ctx context.Context, search string, page Page) ([]ExecutionEnvironment, int, error) {
	return list(ctx, s.pool, listQuery{
		columns: execEnvColumns, from: `execution_environments e`,
		where: `e.name ILIKE $1`, args: []any{likePattern(search)}, orderBy: `e.name`,
	}, page, scanner[ExecutionEnvironment]())
}

// GetExecutionEnvironment returns one execution environment.
func (s *Store) GetExecutionEnvironment(ctx context.Context, id uuid.UUID) (ExecutionEnvironment, error) {
	return getExecutionEnvironment(ctx, s.pool, id)
}

func getExecutionEnvironment(ctx context.Context, db querier, id uuid.UUID) (ExecutionEnvironment, error) {
	var e ExecutionEnvironment
	err := db.QueryRow(ctx, `SELECT `+execEnvColumns+` FROM execution_environments e WHERE e.id = $1`, id).Scan(e.scanTargets()...)
	return e, mapErr(err)
}

// CreateExecutionEnvironment inserts an execution environment.
func (s *Store) CreateExecutionEnvironment(ctx context.Context, in ExecutionEnvironmentFields) (ExecutionEnvironment, error) {
	var e ExecutionEnvironment
	err := s.pool.QueryRow(ctx, `
		INSERT INTO execution_environments AS e (name, description, image, pull_policy, registry_credential_id)
		VALUES ($1, $2, $3, $4, $5)
		RETURNING `+execEnvColumns,
		in.Name, in.Description, in.Image, in.PullPolicy, in.RegistryCredentialID).Scan(e.scanTargets()...)
	return e, mapErr(err)
}

// UpdateExecutionEnvironment loads an execution environment's fields, lets
// update change them, and saves the result.
func (s *Store) UpdateExecutionEnvironment(ctx context.Context, id uuid.UUID, update func(*ExecutionEnvironmentFields) error) (ExecutionEnvironment, error) {
	var out ExecutionEnvironment
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		var f ExecutionEnvironmentFields
		err := tx.QueryRow(ctx, `
			SELECT name, description, image, pull_policy, registry_credential_id
			FROM execution_environments WHERE id = $1 FOR UPDATE`, id).
			Scan(&f.Name, &f.Description, &f.Image, &f.PullPolicy, &f.RegistryCredentialID)
		if err != nil {
			return mapErr(err)
		}
		if err := update(&f); err != nil {
			return err
		}
		err = tx.QueryRow(ctx, `
			UPDATE execution_environments AS e
			SET name = $2, description = $3, image = $4, pull_policy = $5, registry_credential_id = $6
			WHERE e.id = $1
			RETURNING `+execEnvColumns,
			id, f.Name, f.Description, f.Image, f.PullPolicy, f.RegistryCredentialID).Scan(out.scanTargets()...)
		return mapErr(err)
	})
	return out, err
}

// DeleteExecutionEnvironment deletes an execution environment. If job
// templates or inventory sources use it, it returns an *InUseError naming
// them (or, if one was added meanwhile, plain ErrInUse).
func (s *Store) DeleteExecutionEnvironment(ctx context.Context, id uuid.UUID) error {
	var inUse InUseError
	var err error
	inUse.JobTemplates, inUse.JobTemplateCount, err = namesAndCount(ctx, s.pool, `
		SELECT count(*) OVER (), name FROM job_templates
		WHERE execution_environment_id = $1 ORDER BY name LIMIT $2`, id)
	if err != nil {
		return err
	}
	inUse.InventorySources, inUse.InventorySourceCount, err = namesAndCount(ctx, s.pool, `
		SELECT count(*) OVER (), i.name FROM inventory_sources src JOIN inventories i ON i.id = src.inventory_id
		WHERE src.execution_environment_id = $1 ORDER BY i.name LIMIT $2`, id)
	if err != nil {
		return err
	}
	if inUse.JobTemplateCount > 0 || inUse.InventorySourceCount > 0 {
		return &inUse
	}
	return s.deleteByID(ctx, "execution_environments", id)
}
