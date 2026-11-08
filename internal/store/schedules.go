package store

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"dispatch-ops/internal/cronspec"
	"dispatch-ops/internal/metrics"
	"dispatch-ops/internal/rbac"
)

// Schedule is a row in schedules.
type Schedule struct {
	ID           uuid.UUID
	Name         string
	Description  string
	TemplateID   uuid.UUID
	TemplateName string
	Cron         string
	Timezone     string
	Enabled      bool
	Overrides    LaunchOverrides
	NextRunAt    *time.Time
	LastRunAt    *time.Time
	LastJobID    *uuid.UUID
	LastError    string
	CreatedBy    *UserRef
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

const scheduleColumns = `s.id, s.name, s.description, s.job_template_id, t.name, s.cron, s.timezone, s.enabled,
	s.overrides, s.next_run_at, s.last_run_at, s.last_job_id, s.last_error, s.created_by, u.username,
	s.created_at, s.updated_at`

const scheduleFrom = `schedules s JOIN job_templates t ON t.id = s.job_template_id LEFT JOIN users u ON u.id = s.created_by`

type scheduleRow struct {
	Schedule
	createdByID   *uuid.UUID
	createdByName *string
}

func scanSchedule(row pgx.CollectableRow) (Schedule, error) {
	var r scheduleRow
	err := row.Scan(&r.ID, &r.Name, &r.Description, &r.TemplateID, &r.TemplateName, &r.Cron, &r.Timezone, &r.Enabled,
		&r.Overrides, &r.NextRunAt, &r.LastRunAt, &r.LastJobID, &r.LastError, &r.createdByID, &r.createdByName,
		&r.CreatedAt, &r.UpdatedAt)
	if r.createdByID != nil && r.createdByName != nil {
		r.CreatedBy = &UserRef{ID: *r.createdByID, Username: *r.createdByName}
	}
	return r.Schedule, err
}

// ScheduleFields are a schedule's editable fields. NextRunAt must be set
// when Enabled is, and nil otherwise.
type ScheduleFields struct {
	Name        string
	Description string
	Cron        string
	Timezone    string
	Enabled     bool
	Overrides   LaunchOverrides
	NextRunAt   *time.Time
}

// ScheduleFilter narrows ListSchedules.
type ScheduleFilter struct {
	Search     string
	TemplateID *uuid.UUID
	// ReadableBy, if set, limits the list to schedules of templates that
	// user may read.
	ReadableBy *uuid.UUID
}

// ListSchedules returns schedules, by template name and then name.
func (s *Store) ListSchedules(ctx context.Context, f ScheduleFilter, page Page) ([]Schedule, int, error) {
	q := listQuery{
		columns: scheduleColumns, from: scheduleFrom,
		where: `s.name ILIKE $1`, args: []any{likePattern(f.Search)}, orderBy: `t.name, s.name, s.id`,
	}
	if f.TemplateID != nil {
		q.args = append(q.args, *f.TemplateID)
		q.and(`s.job_template_id = $` + strconv.Itoa(len(q.args)))
	}
	q.readableBy(f.ReadableBy, rbac.JobTemplate, "s.job_template_id")
	return list(ctx, s.pool, q, page, scanSchedule)
}

// GetSchedule returns one schedule.
func (s *Store) GetSchedule(ctx context.Context, id uuid.UUID) (Schedule, error) {
	return getSchedule(ctx, s.pool, id, false)
}

func getSchedule(ctx context.Context, db querier, id uuid.UUID, forUpdate bool) (Schedule, error) {
	sql := `SELECT ` + scheduleColumns + ` FROM ` + scheduleFrom + ` WHERE s.id = $1`
	if forUpdate {
		sql += ` FOR UPDATE OF s`
	}
	rows, err := db.Query(ctx, sql, id)
	if err != nil {
		return Schedule{}, err
	}
	sc, err := pgx.CollectExactlyOneRow(rows, scanSchedule)
	return sc, mapErr(err)
}

// CreateSchedule adds a schedule to a job template. It returns ErrConflict
// if the template already has a schedule with the name.
func (s *Store) CreateSchedule(ctx context.Context, templateID uuid.UUID, createdBy *uuid.UUID, f ScheduleFields) (Schedule, error) {
	var id uuid.UUID
	err := s.pool.QueryRow(ctx, `
		INSERT INTO schedules (name, description, job_template_id, cron, timezone, enabled, overrides, next_run_at, created_by)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9) RETURNING id`,
		f.Name, f.Description, templateID, f.Cron, f.Timezone, f.Enabled, f.Overrides, f.NextRunAt, createdBy).Scan(&id)
	if err != nil {
		return Schedule{}, mapErr(err)
	}
	return s.GetSchedule(ctx, id)
}

// UpdateSchedule loads a schedule, lets update change its fields, and saves
// them. update gets the current schedule too.
func (s *Store) UpdateSchedule(ctx context.Context, id uuid.UUID, update func(Schedule, *ScheduleFields) error) (Schedule, error) {
	var out Schedule
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		cur, err := getSchedule(ctx, tx, id, true)
		if err != nil {
			return err
		}
		f := ScheduleFields{
			Name: cur.Name, Description: cur.Description, Cron: cur.Cron, Timezone: cur.Timezone,
			Enabled: cur.Enabled, Overrides: cur.Overrides, NextRunAt: cur.NextRunAt,
		}
		if err := update(cur, &f); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
			UPDATE schedules SET name = $2, description = $3, cron = $4, timezone = $5, enabled = $6,
				overrides = $7, next_run_at = $8
			WHERE id = $1`,
			id, f.Name, f.Description, f.Cron, f.Timezone, f.Enabled, f.Overrides, f.NextRunAt); err != nil {
			return mapErr(err)
		}
		out, err = getSchedule(ctx, tx, id, false)
		return err
	})
	return out, err
}

// DeleteSchedule deletes a schedule. Jobs it launched are kept.
func (s *Store) DeleteSchedule(ctx context.Context, id uuid.UUID) error {
	return s.deleteByID(ctx, "schedules", id)
}

// ScheduleRun is the outcome of one schedule firing.
type ScheduleRun struct {
	Schedule Schedule
	// JobID is the launched job, if the launch succeeded.
	JobID *uuid.UUID
	// Err is why no job was launched.
	Err error
}

// RunDueSchedules launches a job for each enabled schedule whose next run
// time has passed, up to limit, and moves each one's next run time on. A
// schedule that was due several times (e.g. while no task manager ran)
// launches once. Each schedule is handled in its own transaction, so
// concurrent callers never launch the same run twice.
func (s *Store) RunDueSchedules(ctx context.Context, limit int) ([]ScheduleRun, error) {
	var runs []ScheduleRun
	for len(runs) < limit {
		var run *ScheduleRun
		err := s.inTx(ctx, func(tx pgx.Tx) error {
			var id uuid.UUID
			err := tx.QueryRow(ctx, `
				SELECT id FROM schedules WHERE next_run_at <= now()
				ORDER BY next_run_at LIMIT 1 FOR UPDATE SKIP LOCKED`).Scan(&id)
			if errors.Is(err, pgx.ErrNoRows) {
				return nil
			}
			if err != nil {
				return err
			}
			r, err := runSchedule(ctx, tx, id)
			run = &r
			return err
		})
		if err != nil {
			return runs, err
		}
		if run == nil {
			break
		}
		if run.Err != nil {
			metrics.ScheduleRuns.WithLabelValues("error").Inc()
		} else {
			metrics.ScheduleRuns.WithLabelValues("launched").Inc()
		}
		runs = append(runs, *run)
	}
	return runs, nil
}

// runSchedule launches the locked schedule id's job and records the run.
// A problem with the launch itself (e.g. an override the template no
// longer allows) is recorded on the schedule and returned in the run; only
// database errors are returned as errors.
func runSchedule(ctx context.Context, tx pgx.Tx, id uuid.UUID) (ScheduleRun, error) {
	sc, err := getSchedule(ctx, tx, id, false)
	if err != nil {
		return ScheduleRun{}, err
	}
	var now time.Time
	if err := tx.QueryRow(ctx, `SELECT now()`).Scan(&now); err != nil {
		return ScheduleRun{}, err
	}
	run := ScheduleRun{Schedule: sc}

	// Plan the next run first: a schedule whose expression no longer
	// parses (e.g. a time zone dropped from tzdata) is disabled.
	var next *time.Time
	spec, err := cronspec.Parse(sc.Cron, sc.Timezone)
	if err != nil {
		run.Err = fmt.Errorf("schedule disabled: %w", err)
	} else if n := spec.Next(now); !n.IsZero() {
		next = &n
	}

	if run.Err == nil {
		run.JobID, run.Err = launchScheduled(ctx, tx, sc)
		if run.Err != nil && !isLaunchProblem(run.Err) {
			return ScheduleRun{}, run.Err
		}
	}

	lastError := ""
	if run.Err != nil {
		lastError = run.Err.Error()
	}
	_, err = tx.Exec(ctx, `
		UPDATE schedules SET next_run_at = $2, enabled = $2::timestamptz IS NOT NULL, last_run_at = now(),
			last_job_id = coalesce($3, last_job_id), last_error = $4
		WHERE id = $1`, id, next, run.JobID, lastError)
	if err != nil {
		return ScheduleRun{}, err
	}
	sc.NextRunAt, sc.Enabled, sc.LastRunAt, sc.LastError = next, next != nil, &now, lastError
	if run.JobID != nil {
		sc.LastJobID = run.JobID
	}
	run.Schedule = sc
	return run, nil
}

func isLaunchProblem(err error) bool {
	var le *LaunchError
	return errors.As(err, &le) || errors.Is(err, ErrNotFound)
}

func launchScheduled(ctx context.Context, tx pgx.Tx, sc Schedule) (*uuid.UUID, error) {
	t, err := getJobTemplate(ctx, tx, sc.TemplateID, false)
	if err != nil {
		return nil, err
	}
	cfg, err := resolveJobConfig(ctx, tx, t, sc.Overrides)
	if err != nil {
		return nil, err
	}
	jobID, err := createJob(ctx, tx, NewJob{
		TemplateID: &t.ID, Name: t.Name, LaunchType: "schedule", Config: cfg, ScheduleID: &sc.ID,
	})
	if err != nil {
		return nil, err
	}
	after := map[string]any{"jobTemplateId": t.ID, "launchType": "schedule", "scheduleId": sc.ID}
	_, err = tx.Exec(ctx, `
		INSERT INTO audit_log (actor_name, auth_method, action, resource_type, resource_id, resource_name, after)
		VALUES ($1, 'schedule', 'launch', 'job', $2, $3, $4)`,
		"schedule "+sc.Name, jobID, t.Name, after)
	if err != nil {
		return nil, err
	}
	return &jobID, nil
}
