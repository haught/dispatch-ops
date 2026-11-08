package store

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// JobTemplate is a row in job_templates with its credential IDs.
type JobTemplate struct {
	ID                     uuid.UUID
	Name                   string
	Description            string
	JobType                string
	ExecutionEnvironmentID uuid.UUID
	ProjectID              *uuid.UUID
	Entrypoint             string
	InventoryID            *uuid.UUID
	CredentialIDs          []uuid.UUID
	Command                []string
	Arguments              []string
	ExtraVars              map[string]any
	Limit                  string
	Tags                   string
	SkipTags               string
	Verbosity              int
	TimeoutSeconds         int
	AllowOverrides         []string
	CreatedAt              time.Time
	UpdatedAt              time.Time
}

const templateColumns = `t.id, t.name, t.description, t.job_type, t.execution_environment_id,
	t.project_id, t.entrypoint, t.inventory_id,
	ARRAY(SELECT tc.credential_id FROM job_template_credentials tc WHERE tc.job_template_id = t.id ORDER BY tc.credential_id),
	t.command, t.arguments, t.extra_vars, t.limit_pattern, t.job_tags, t.skip_tags, t.verbosity,
	t.timeout_seconds, t.allow_overrides, t.created_at, t.updated_at`

func (t *JobTemplate) scanTargets() []any {
	return []any{&t.ID, &t.Name, &t.Description, &t.JobType, &t.ExecutionEnvironmentID,
		&t.ProjectID, &t.Entrypoint, &t.InventoryID,
		&t.CredentialIDs, &t.Command, &t.Arguments, &t.ExtraVars, &t.Limit, &t.Tags, &t.SkipTags, &t.Verbosity,
		&t.TimeoutSeconds, &t.AllowOverrides, &t.CreatedAt, &t.UpdatedAt}
}

// JobTemplateFields are a job template's editable fields.
type JobTemplateFields struct {
	Name                   string
	Description            string
	JobType                string
	ExecutionEnvironmentID uuid.UUID
	ProjectID              *uuid.UUID
	Entrypoint             string
	InventoryID            *uuid.UUID
	CredentialIDs          []uuid.UUID
	Command                []string
	Arguments              []string
	ExtraVars              map[string]any
	Limit                  string
	Tags                   string
	SkipTags               string
	Verbosity              int
	TimeoutSeconds         int
	AllowOverrides         []string
}

// ListJobTemplates returns job templates, by name.
func (s *Store) ListJobTemplates(ctx context.Context, search string, page Page) ([]JobTemplate, int, error) {
	return list(ctx, s.pool, listQuery{
		columns: templateColumns, from: `job_templates t`,
		where: `t.name ILIKE $1`, args: []any{likePattern(search)}, orderBy: `t.name`,
	}, page, scanner[JobTemplate]())
}

// GetJobTemplate returns one job template.
func (s *Store) GetJobTemplate(ctx context.Context, id uuid.UUID) (JobTemplate, error) {
	return getJobTemplate(ctx, s.pool, id, false)
}

func getJobTemplate(ctx context.Context, db querier, id uuid.UUID, forUpdate bool) (JobTemplate, error) {
	sql := `SELECT ` + templateColumns + ` FROM job_templates t WHERE t.id = $1`
	if forUpdate {
		sql += ` FOR UPDATE`
	}
	var t JobTemplate
	err := db.QueryRow(ctx, sql, id).Scan(t.scanTargets()...)
	return t, mapErr(err)
}

// CreateJobTemplate inserts a job template. It returns ErrInvalidReference
// if the execution environment, project, inventory or a credential does not
// exist.
func (s *Store) CreateJobTemplate(ctx context.Context, in JobTemplateFields) (JobTemplate, error) {
	var out JobTemplate
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		var id uuid.UUID
		err := tx.QueryRow(ctx, `
			INSERT INTO job_templates (name, description, job_type, execution_environment_id, project_id, entrypoint,
				inventory_id, command, arguments, extra_vars, limit_pattern, job_tags, skip_tags, verbosity,
				timeout_seconds, allow_overrides)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16)
			RETURNING id`,
			in.Name, in.Description, in.JobType, in.ExecutionEnvironmentID, in.ProjectID, in.Entrypoint,
			in.InventoryID, nonNilStrings(in.Command), nonNilStrings(in.Arguments), nonNil(in.ExtraVars), in.Limit,
			in.Tags, in.SkipTags, in.Verbosity, in.TimeoutSeconds, nonNilStrings(in.AllowOverrides),
		).Scan(&id)
		if err != nil {
			return mapErr(err)
		}
		if err := setTemplateCredentials(ctx, tx, id, in.CredentialIDs); err != nil {
			return err
		}
		out, err = getJobTemplate(ctx, tx, id, false)
		return err
	})
	return out, err
}

// UpdateJobTemplate loads a job template's fields, lets update change them,
// and saves the result. Credentials are replaced with the updated list.
func (s *Store) UpdateJobTemplate(ctx context.Context, id uuid.UUID, update func(*JobTemplateFields) error) (JobTemplate, error) {
	var out JobTemplate
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		t, err := getJobTemplate(ctx, tx, id, true)
		if err != nil {
			return err
		}
		f := JobTemplateFields{
			Name: t.Name, Description: t.Description, JobType: t.JobType,
			ExecutionEnvironmentID: t.ExecutionEnvironmentID, ProjectID: t.ProjectID, Entrypoint: t.Entrypoint,
			InventoryID: t.InventoryID, CredentialIDs: t.CredentialIDs, Command: t.Command, Arguments: t.Arguments,
			ExtraVars: t.ExtraVars, Limit: t.Limit, Tags: t.Tags, SkipTags: t.SkipTags, Verbosity: t.Verbosity,
			TimeoutSeconds: t.TimeoutSeconds, AllowOverrides: t.AllowOverrides,
		}
		if err := update(&f); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
			UPDATE job_templates SET name = $2, description = $3, execution_environment_id = $4, project_id = $5,
				entrypoint = $6, inventory_id = $7, command = $8, arguments = $9, extra_vars = $10, limit_pattern = $11,
				job_tags = $12, skip_tags = $13, verbosity = $14, timeout_seconds = $15, allow_overrides = $16
			WHERE id = $1`,
			id, f.Name, f.Description, f.ExecutionEnvironmentID, f.ProjectID, f.Entrypoint, f.InventoryID,
			nonNilStrings(f.Command), nonNilStrings(f.Arguments), nonNil(f.ExtraVars), f.Limit,
			f.Tags, f.SkipTags, f.Verbosity, f.TimeoutSeconds, nonNilStrings(f.AllowOverrides),
		); err != nil {
			return mapErr(err)
		}
		if _, err := tx.Exec(ctx, `DELETE FROM job_template_credentials WHERE job_template_id = $1`, id); err != nil {
			return err
		}
		if err := setTemplateCredentials(ctx, tx, id, f.CredentialIDs); err != nil {
			return err
		}
		out, err = getJobTemplate(ctx, tx, id, false)
		return err
	})
	return out, err
}

func setTemplateCredentials(ctx context.Context, tx pgx.Tx, templateID uuid.UUID, credentialIDs []uuid.UUID) error {
	if len(credentialIDs) == 0 {
		return nil
	}
	_, err := tx.Exec(ctx, `
		INSERT INTO job_template_credentials (job_template_id, credential_id)
		SELECT $1, c FROM unnest($2::uuid[]) AS c
		ON CONFLICT DO NOTHING`, templateID, credentialIDs)
	return mapErr(err)
}

// DeleteJobTemplate deletes a job template. Its jobs are kept.
func (s *Store) DeleteJobTemplate(ctx context.Context, id uuid.UUID) error {
	return s.deleteByID(ctx, "job_templates", id)
}

func nonNilStrings(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}
