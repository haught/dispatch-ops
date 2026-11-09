package store

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"dispatch-ops/internal/metrics"
)

// ErrJobState means the job is not in a state that allows the operation,
// e.g. reporting events for a job that has already finished.
var ErrJobState = errors.New("job is not in a state that allows this")

// Job statuses.
const (
	JobPending    = "pending"
	JobWaiting    = "waiting"
	JobRunning    = "running"
	JobSuccessful = "successful"
	JobFailed     = "failed"
	JobError      = "error"
	JobCanceled   = "canceled"
)

// JobFinished reports whether status is final.
func JobFinished(status string) bool {
	switch status {
	case JobSuccessful, JobFailed, JobError, JobCanceled:
		return true
	}
	return false
}

// JobConfig is the resolved configuration a job runs with, stored as JSON
// in jobs.config when the job is launched.
type JobConfig struct {
	JobType              string                  `json:"jobType"`
	ExecutionEnvironment JobExecutionEnvironment `json:"executionEnvironment"`
	Project              *JobProject             `json:"project,omitempty"`
	Entrypoint           string                  `json:"entrypoint,omitempty"`
	InventoryID          *uuid.UUID              `json:"inventoryId"`
	CredentialIDs        []uuid.UUID             `json:"credentialIds"`
	Command              []string                `json:"command"`
	Arguments            []string                `json:"arguments,omitempty"`
	ExtraVars            map[string]any          `json:"extraVars"`
	Limit                string                  `json:"limit"`
	Tags                 string                  `json:"tags,omitempty"`
	SkipTags             string                  `json:"skipTags,omitempty"`
	Verbosity            int                     `json:"verbosity,omitempty"`
	TimeoutSeconds       int                     `json:"timeoutSeconds"`
}

// JobProject is the repository a job checks out, as it was at launch.
type JobProject struct {
	ID           uuid.UUID  `json:"id"`
	Name         string     `json:"name"`
	ScmURL       string     `json:"scmUrl"`
	ScmRef       string     `json:"scmRef"`
	CredentialID *uuid.UUID `json:"credentialId,omitempty"`
}

// JobExecutionEnvironment is the image a job runs in.
type JobExecutionEnvironment struct {
	ID                   uuid.UUID  `json:"id"`
	Name                 string     `json:"name"`
	Image                string     `json:"image"`
	PullPolicy           string     `json:"pullPolicy"`
	RegistryCredentialID *uuid.UUID `json:"registryCredentialId,omitempty"`
}

// UserRef identifies a user.
type UserRef struct {
	ID       uuid.UUID
	Username string
}

// Job is a row in jobs.
type Job struct {
	ID                uuid.UUID
	TemplateID        *uuid.UUID
	Name              string
	Status            string
	LaunchType        string
	LaunchedBy        *UserRef
	Config            JobConfig
	CancelRequested   bool
	CancelRequestedAt *time.Time
	ExitCode          *int
	FailureReason     string
	K8sNamespace      *string
	K8sJobName        *string
	RunnerVersion     string
	ScmRevision       string
	CreatedAt         time.Time
	DispatchedAt      *time.Time
	StartedAt         *time.Time
	FinishedAt        *time.Time
	LastHeartbeatAt   *time.Time
	// The schedule that launched the job, if one did and it still exists.
	Schedule *ScheduleRef
	// The job this one is a relaunch of, if it still exists.
	RelaunchOf *uuid.UUID
	// The inventory an inventory_sync job syncs, if it still exists.
	SyncInventoryID *uuid.UUID
}

// ScheduleRef identifies a schedule.
type ScheduleRef struct {
	ID   uuid.UUID
	Name string
}

const jobColumns = `j.id, j.job_template_id, j.name, j.status, j.launch_type, j.launched_by, u.username,
	j.config, j.cancel_requested, j.cancel_requested_at, j.exit_code, j.failure_reason,
	j.k8s_namespace, j.k8s_job_name, j.runner_version, j.scm_revision,
	j.created_at, j.dispatched_at, j.started_at, j.finished_at, j.last_heartbeat_at,
	j.schedule_id, sc.name, j.relaunch_of, j.sync_inventory_id`

const jobFrom = `jobs j LEFT JOIN users u ON u.id = j.launched_by LEFT JOIN schedules sc ON sc.id = j.schedule_id`

// jobRow holds the nullable launched_by and schedule columns while scanning.
type jobRow struct {
	Job
	launchedByID   *uuid.UUID
	launchedByName *string
	scheduleID     *uuid.UUID
	scheduleName   *string
}

func (j *jobRow) scanTargets() []any {
	return []any{&j.ID, &j.TemplateID, &j.Name, &j.Status, &j.LaunchType, &j.launchedByID, &j.launchedByName,
		&j.Config, &j.CancelRequested, &j.CancelRequestedAt, &j.ExitCode, &j.FailureReason,
		&j.K8sNamespace, &j.K8sJobName, &j.RunnerVersion, &j.ScmRevision,
		&j.CreatedAt, &j.DispatchedAt, &j.StartedAt, &j.FinishedAt, &j.LastHeartbeatAt,
		&j.scheduleID, &j.scheduleName, &j.RelaunchOf, &j.SyncInventoryID}
}

func (j *jobRow) job() Job {
	if j.launchedByID != nil && j.launchedByName != nil {
		j.LaunchedBy = &UserRef{ID: *j.launchedByID, Username: *j.launchedByName}
	}
	if j.scheduleID != nil && j.scheduleName != nil {
		j.Schedule = &ScheduleRef{ID: *j.scheduleID, Name: *j.scheduleName}
	}
	return j.Job
}

func scanJob(row pgx.CollectableRow) (Job, error) {
	var j jobRow
	err := row.Scan(j.scanTargets()...)
	return j.job(), err
}

// NewJob holds the fields for launching a job.
type NewJob struct {
	TemplateID *uuid.UUID
	Name       string
	LaunchType string
	LaunchedBy *uuid.UUID
	Config     JobConfig
	ScheduleID *uuid.UUID
	RelaunchOf *uuid.UUID
	// SyncInventoryID is the inventory an inventory_sync job syncs.
	SyncInventoryID *uuid.UUID
}

// CreateJob queues a job as pending.
func (s *Store) CreateJob(ctx context.Context, in NewJob) (Job, error) {
	id, err := createJob(ctx, s.pool, in)
	if err != nil {
		return Job{}, err
	}
	return s.GetJob(ctx, id)
}

func createJob(ctx context.Context, db querier, in NewJob) (uuid.UUID, error) {
	var id uuid.UUID
	err := db.QueryRow(ctx, `
		INSERT INTO jobs (job_template_id, name, launch_type, launched_by, config, schedule_id, relaunch_of, sync_inventory_id)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8) RETURNING id`,
		in.TemplateID, in.Name, in.LaunchType, in.LaunchedBy, in.Config, in.ScheduleID, in.RelaunchOf, in.SyncInventoryID).Scan(&id)
	if err != nil {
		return uuid.Nil, mapErr(err)
	}
	metrics.JobsLaunched.WithLabelValues(in.Config.JobType, in.LaunchType).Inc()
	return id, nil
}

// finishedJob is what the queries ending a job return, for metrics.
type finishedJob struct {
	jobType    string
	startedAt  *time.Time
	finishedAt time.Time
}

const finishedJobReturning = ` RETURNING config->>'jobType', started_at, finished_at`

func (f finishedJob) record(status string) {
	metrics.JobsFinished.WithLabelValues(f.jobType, status).Inc()
	if f.startedAt != nil {
		metrics.JobDuration.WithLabelValues(f.jobType, status).Observe(f.finishedAt.Sub(*f.startedAt).Seconds())
	}
}

// endJobWhere runs an UPDATE that ends at most one job, records metrics if
// it did, and returns ErrJobState if it didn't.
func endJobWhere(ctx context.Context, db querier, status, sql string, args ...any) error {
	var f finishedJob
	err := db.QueryRow(ctx, sql+finishedJobReturning, args...).Scan(&f.jobType, &f.startedAt, &f.finishedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrJobState
	}
	if err != nil {
		return err
	}
	f.record(status)
	return nil
}

// GetJob returns one job.
func (s *Store) GetJob(ctx context.Context, id uuid.UUID) (Job, error) {
	return getJob(ctx, s.pool, id, false)
}

func getJob(ctx context.Context, db querier, id uuid.UUID, forUpdate bool) (Job, error) {
	sql := `SELECT ` + jobColumns + ` FROM ` + jobFrom + ` WHERE j.id = $1`
	if forUpdate {
		sql += ` FOR UPDATE OF j`
	}
	var j jobRow
	if err := db.QueryRow(ctx, sql, id).Scan(j.scanTargets()...); err != nil {
		return Job{}, mapErr(err)
	}
	return j.job(), nil
}

// JobFilter narrows ListJobs.
type JobFilter struct {
	Status     string
	TemplateID *uuid.UUID
	ScheduleID *uuid.UUID
	// SyncInventoryID limits the list to the inventory's sync jobs.
	SyncInventoryID *uuid.UUID
	// ReadableBy, if set, limits the list to jobs that user launched, whose
	// template they hold a role on, or that sync an inventory they hold a
	// role on.
	ReadableBy *uuid.UUID
}

// ListJobs returns jobs, newest first.
func (s *Store) ListJobs(ctx context.Context, f JobFilter, page Page) ([]Job, int, error) {
	q := listQuery{columns: jobColumns, from: jobFrom, orderBy: `j.created_at DESC, j.id`}
	var conds []string
	if f.Status != "" {
		q.args = append(q.args, f.Status)
		conds = append(conds, `j.status = $1`)
	}
	if f.TemplateID != nil {
		q.args = append(q.args, *f.TemplateID)
		conds = append(conds, `j.job_template_id = $`+strconv.Itoa(len(q.args)))
	}
	if f.ScheduleID != nil {
		q.args = append(q.args, *f.ScheduleID)
		conds = append(conds, `j.schedule_id = $`+strconv.Itoa(len(q.args)))
	}
	if f.SyncInventoryID != nil {
		q.args = append(q.args, *f.SyncInventoryID)
		conds = append(conds, `j.sync_inventory_id = $`+strconv.Itoa(len(q.args)))
	}
	if f.ReadableBy != nil {
		q.args = append(q.args, *f.ReadableBy)
		n := strconv.Itoa(len(q.args))
		conds = append(conds, `(j.launched_by = $`+n+` OR EXISTS (SELECT 1 FROM user_roles r
			WHERE r.user_id = $`+n+` AND r.resource_type = 'job_template'
				AND (r.resource_id IS NULL OR r.resource_id = j.job_template_id))
			OR (j.sync_inventory_id IS NOT NULL AND EXISTS (SELECT 1 FROM user_roles r
			WHERE r.user_id = $`+n+` AND r.resource_type = 'inventory'
				AND (r.resource_id IS NULL OR r.resource_id = j.sync_inventory_id))))`)
	}
	q.where = strings.Join(conds, " AND ")
	return list(ctx, s.pool, q, page, scanJob)
}

// CancelJob cancels a pending job at once, or asks a started job's runner to
// stop. It returns ErrJobState if the job has already finished.
func (s *Store) CancelJob(ctx context.Context, id uuid.UUID) (Job, error) {
	var out Job
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		j, err := getJob(ctx, tx, id, true)
		if err != nil {
			return err
		}
		switch {
		case JobFinished(j.Status):
			return ErrJobState
		case j.Status == JobPending:
			err = endJobWhere(ctx, tx, JobCanceled, `
				UPDATE jobs SET status = 'canceled', cancel_requested = true, cancel_requested_at = now(),
					finished_at = now(), failure_reason = 'Canceled before it started'
				WHERE id = $1`, id)
		case !j.CancelRequested:
			_, err = tx.Exec(ctx, `UPDATE jobs SET cancel_requested = true, cancel_requested_at = now() WHERE id = $1`, id)
		}
		if err != nil {
			return err
		}
		out, err = getJob(ctx, tx, id, false)
		return err
	})
	return out, err
}

// EndJob gives a job a final status if its current status is one of from.
// It revokes the job's runner token. It returns ErrJobState otherwise.
func (s *Store) EndJob(ctx context.Context, id uuid.UUID, status string, exitCode *int, reason string, from ...string) error {
	return endJobWhere(ctx, s.pool, status, `
		UPDATE jobs SET status = $2, exit_code = $3, failure_reason = $4, finished_at = now(), runner_token_hash = NULL
		WHERE id = $1 AND status = ANY($5)`, id, status, exitCode, reason, from)
}

// FinishJob records the result a runner reported for its running job, like
// EndJob, along with the commit its project was checked out at.
func (s *Store) FinishJob(ctx context.Context, id uuid.UUID, status string, exitCode *int, reason, scmRevision string) error {
	return endJobWhere(ctx, s.pool, status, `
		UPDATE jobs SET status = $2, exit_code = $3, failure_reason = $4, scm_revision = $5,
			finished_at = now(), runner_token_hash = NULL
		WHERE id = $1 AND status = 'running'`, id, status, exitCode, reason, scmRevision)
}

// --- Task manager ------------------------------------------------------------

// Dispatch is what the task manager records when it hands a job to Kubernetes.
type Dispatch struct {
	TokenHash    []byte
	K8sNamespace string
	K8sJobName   string
}

// ClaimJobs moves up to the number of free slots (maxActive minus jobs
// already waiting or running) of the oldest pending jobs to waiting, with
// the details from dispatch, and returns them.
func (s *Store) ClaimJobs(ctx context.Context, maxActive int, dispatch func(Job) (Dispatch, error)) ([]Job, error) {
	var claimed []Job
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		// Serialize claimers so two can't both count the same free slot.
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext('dispatch-ops.claim-jobs'))`); err != nil {
			return err
		}
		var active int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM jobs WHERE status IN ('waiting', 'running')`).Scan(&active); err != nil {
			return err
		}
		free := maxActive - active
		if free <= 0 {
			return nil
		}
		rows, err := tx.Query(ctx, `
			SELECT `+jobColumns+` FROM `+jobFrom+`
			WHERE j.status = 'pending' ORDER BY j.created_at, j.id
			LIMIT $1 FOR UPDATE OF j SKIP LOCKED`, free)
		if err != nil {
			return err
		}
		jobs, err := pgx.CollectRows(rows, scanJob)
		if err != nil {
			return err
		}
		for _, j := range jobs {
			d, err := dispatch(j)
			if err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `
				UPDATE jobs SET status = 'waiting', runner_token_hash = $2, k8s_namespace = $3, k8s_job_name = $4,
					dispatched_at = now()
				WHERE id = $1`, j.ID, d.TokenHash, d.K8sNamespace, d.K8sJobName); err != nil {
				return err
			}
			j.Status = JobWaiting
			j.K8sNamespace, j.K8sJobName = &d.K8sNamespace, &d.K8sJobName
			now := time.Now()
			j.DispatchedAt = &now
			claimed = append(claimed, j)
		}
		return nil
	})
	return claimed, err
}

// ListActiveJobs returns jobs that are waiting or running.
func (s *Store) ListActiveJobs(ctx context.Context) ([]Job, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+jobColumns+` FROM `+jobFrom+`
		WHERE j.status IN ('waiting', 'running') ORDER BY j.created_at`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, scanJob)
}

// GetJobEnds returns, for each of the given jobs that exists, when it
// finished (nil if it has not).
func (s *Store) GetJobEnds(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]*time.Time, error) {
	rows, err := s.pool.Query(ctx, `SELECT id, finished_at FROM jobs WHERE id = ANY($1)`, ids)
	if err != nil {
		return nil, err
	}
	out := map[uuid.UUID]*time.Time{}
	var id uuid.UUID
	var finished *time.Time
	_, err = pgx.ForEachRow(rows, []any{&id, &finished}, func() error {
		out[id] = finished
		return nil
	})
	return out, err
}

// --- Runner ------------------------------------------------------------------

// LookupJobToken returns the job a runner token belongs to. Tokens only
// work while the job is waiting or running.
func (s *Store) LookupJobToken(ctx context.Context, hash []byte) (uuid.UUID, error) {
	var id uuid.UUID
	err := s.pool.QueryRow(ctx, `
		SELECT id FROM jobs WHERE runner_token_hash = $1 AND status IN ('waiting', 'running')`, hash).Scan(&id)
	return id, mapErr(err)
}

// StartJob moves a waiting job to running when its runner fetches the spec.
// It returns ErrJobState if the job is not waiting, which also means the
// spec was already fetched.
func (s *Store) StartJob(ctx context.Context, id uuid.UUID, runnerVersion string) (Job, error) {
	var jobType string
	var waited float64
	err := s.pool.QueryRow(ctx, `
		UPDATE jobs SET status = 'running', started_at = now(), last_heartbeat_at = now(), runner_version = $2
		WHERE id = $1 AND status = 'waiting'
		RETURNING config->>'jobType', extract(epoch FROM started_at - created_at)::float8`, id, runnerVersion).Scan(&jobType, &waited)
	if errors.Is(err, pgx.ErrNoRows) {
		return Job{}, ErrJobState
	}
	if err != nil {
		return Job{}, err
	}
	metrics.JobQueueWait.WithLabelValues(jobType).Observe(waited)
	return s.GetJob(ctx, id)
}

// Heartbeat records that a running job's runner is alive and reports
// whether the job should be canceled.
func (s *Store) Heartbeat(ctx context.Context, id uuid.UUID) (cancelRequested bool, err error) {
	err = s.pool.QueryRow(ctx, `
		UPDATE jobs SET last_heartbeat_at = now() WHERE id = $1 AND status = 'running'
		RETURNING cancel_requested`, id).Scan(&cancelRequested)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, ErrJobState
	}
	return cancelRequested, err
}

// JobEvent is a row in job_events.
type JobEvent struct {
	Seq      int64
	Time     time.Time
	Type     string
	HostName *string
	Stdout   string
	Data     map[string]any
}

// AppendJobEvents stores events for a running job, skipping any whose seq is
// already stored, and notifies stream listeners. It also counts as a
// heartbeat, and reports whether the job should be canceled.
func (s *Store) AppendJobEvents(ctx context.Context, id uuid.UUID, events []JobEvent) (cancelRequested bool, err error) {
	n := len(events)
	seqs, times := make([]int64, n), make([]time.Time, n)
	types, hosts, stdouts, data := make([]string, n), make([]*string, n), make([]string, n), make([]*string, n)
	for i, e := range events {
		seqs[i], times[i], types[i], hosts[i] = e.Seq, e.Time, e.Type, e.HostName
		stdouts[i] = stripNUL(e.Stdout)
		if hosts[i] != nil {
			h := stripNUL(*hosts[i])
			hosts[i] = &h
		}
		if e.Data != nil {
			b, err := json.Marshal(e.Data)
			if err != nil {
				return false, err
			}
			// PostgreSQL's jsonb rejects the NUL escape.
			d := strings.ReplaceAll(string(b), `\u0000`, ``)
			data[i] = &d
		}
	}

	err = s.inTx(ctx, func(tx pgx.Tx) error {
		err := tx.QueryRow(ctx, `
			UPDATE jobs SET last_heartbeat_at = now() WHERE id = $1 AND status = 'running'
			RETURNING cancel_requested`, id).Scan(&cancelRequested)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrJobState
		}
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO job_events (job_id, seq, created_at, event_type, host_name, stdout, data)
			SELECT $1, e.seq, e.created_at, e.event_type, e.host_name, e.stdout, e.data::jsonb
			FROM unnest($2::bigint[], $3::timestamptz[], $4::text[], $5::text[], $6::text[], $7::text[])
				AS e(seq, created_at, event_type, host_name, stdout, data)
			ON CONFLICT (job_id, seq) DO NOTHING`,
			id, seqs, times, types, hosts, stdouts, data); err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `SELECT pg_notify('dispatch_job_events', $1)`, id.String())
		return err
	})
	return cancelRequested, err
}

func stripNUL(s string) string {
	return strings.ReplaceAll(s, "\x00", "")
}

// JobEventFilter narrows ListJobEvents.
type JobEventFilter struct {
	AfterSeq int64
	HostName string // only events about this host, if set
}

// ListJobEvents returns up to limit events with seq greater than
// f.AfterSeq. It returns ErrNotFound if the job does not exist.
func (s *Store) ListJobEvents(ctx context.Context, jobID uuid.UUID, f JobEventFilter, limit int) ([]JobEvent, error) {
	sql := `SELECT seq, created_at, event_type, host_name, stdout, data
		FROM job_events WHERE job_id = $1 AND seq > $2`
	args := []any{jobID, f.AfterSeq, limit}
	if f.HostName != "" {
		sql += ` AND host_name = $4`
		args = append(args, f.HostName)
	}
	rows, err := s.pool.Query(ctx, sql+` ORDER BY seq LIMIT $3`, args...)
	if err != nil {
		return nil, err
	}
	events, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (JobEvent, error) {
		var e JobEvent
		err := row.Scan(&e.Seq, &e.Time, &e.Type, &e.HostName, &e.Stdout, &e.Data)
		return e, err
	})
	if err != nil {
		return nil, err
	}
	if len(events) == 0 {
		var exists bool
		if err := s.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM jobs WHERE id = $1)`, jobID).Scan(&exists); err != nil {
			return nil, err
		}
		if !exists {
			return nil, ErrNotFound
		}
	}
	return events, nil
}

// JobHostSummary is one host's task results in an Ansible job.
type JobHostSummary struct {
	HostName    string
	OK          int
	Changed     int
	Failed      int
	Unreachable int
	Skipped     int
	Rescued     int
	Ignored     int
}

// Status is the host's overall result: the worst of its task results.
func (h JobHostSummary) Status() string {
	switch {
	case h.Unreachable > 0:
		return "unreachable"
	case h.Failed > 0:
		return "failed"
	case h.Changed > 0:
		return "changed"
	case h.OK > 0:
		return "ok"
	}
	return "skipped"
}

// ListJobHostSummaries returns a job's per-host results, by host name.
// They are counted from the host_* events received so far; once the
// playbook_stats event arrives, its numbers (Ansible's own recap, which
// also knows about rescued tasks) replace the counts for the hosts it lists.
// It returns ErrNotFound if the job does not exist.
func (s *Store) ListJobHostSummaries(ctx context.Context, jobID uuid.UUID) ([]JobHostSummary, error) {
	var exists bool
	if err := s.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM jobs WHERE id = $1)`, jobID).Scan(&exists); err != nil {
		return nil, err
	}
	if !exists {
		return nil, ErrNotFound
	}

	rows, err := s.pool.Query(ctx, `
		SELECT host_name,
			-- Like Ansible's recap, failures with ignore_errors count as ok too.
			count(*) FILTER (WHERE succeeded),
			count(*) FILTER (WHERE succeeded AND changed),
			count(*) FILTER (WHERE event_type = 'host_failed' AND NOT ignored),
			count(*) FILTER (WHERE event_type = 'host_unreachable'),
			count(*) FILTER (WHERE event_type = 'host_skipped'),
			count(*) FILTER (WHERE event_type = 'host_failed' AND ignored)
		FROM (
			SELECT host_name, event_type,
				coalesce(data->>'changed' = 'true', false) AS changed,
				coalesce(data->>'ignored' = 'true', false) AS ignored,
				event_type = 'host_ok' OR (event_type = 'host_failed' AND coalesce(data->>'ignored' = 'true', false)) AS succeeded
			FROM job_events
			WHERE job_id = $1 AND host_name IS NOT NULL
				AND event_type IN ('host_ok', 'host_failed', 'host_unreachable', 'host_skipped')
		) e
		GROUP BY host_name ORDER BY host_name`, jobID)
	if err != nil {
		return nil, err
	}
	hosts, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (JobHostSummary, error) {
		var h JobHostSummary
		err := row.Scan(&h.HostName, &h.OK, &h.Changed, &h.Failed, &h.Unreachable, &h.Skipped, &h.Ignored)
		return h, err
	})
	if err != nil {
		return nil, err
	}

	// The recap comes in chunks of hosts.
	rows, err = s.pool.Query(ctx, `
		SELECT data FROM job_events WHERE job_id = $1 AND event_type = 'playbook_stats' ORDER BY seq`, jobID)
	if err != nil {
		return nil, err
	}
	type hostStats struct {
		OK          int `json:"ok"`
		Changed     int `json:"changed"`
		Failures    int `json:"failures"`
		Unreachable int `json:"unreachable"`
		Skipped     int `json:"skipped"`
		Rescued     int `json:"rescued"`
		Ignored     int `json:"ignored"`
	}
	recap := map[string]hostStats{}
	var chunk struct {
		Hosts map[string]hostStats `json:"hosts"`
	}
	_, err = pgx.ForEachRow(rows, []any{&chunk}, func() error {
		maps.Copy(recap, chunk.Hosts)
		chunk.Hosts = nil
		return nil
	})
	if err != nil || len(recap) == 0 {
		return hosts, err
	}
	byName := map[string]int{}
	for i, h := range hosts {
		byName[h.HostName] = i
	}
	for name, st := range recap {
		h := JobHostSummary{
			HostName: name, OK: st.OK, Changed: st.Changed, Failed: st.Failures, Unreachable: st.Unreachable,
			Skipped: st.Skipped, Rescued: st.Rescued, Ignored: st.Ignored,
		}
		if i, ok := byName[name]; ok {
			hosts[i] = h
		} else {
			hosts = append(hosts, h)
		}
	}
	slices.SortFunc(hosts, func(a, b JobHostSummary) int { return strings.Compare(a.HostName, b.HostName) })
	return hosts, nil
}
