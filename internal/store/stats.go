package store

import (
	"context"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"dispatch-ops/internal/rbac"
)

// StatsScope is the slice of jobs dashboard statistics count: those
// created in [Since, Until) that ReadableBy may see (all jobs if nil).
type StatsScope struct {
	Since, Until time.Time
	ReadableBy   *uuid.UUID
}

// where returns the scope's condition on the jobs aliased as alias, and its
// arguments, which start at $1.
func (sc StatsScope) where(alias string) (string, []any) {
	cond := alias + `.created_at >= $1 AND ` + alias + `.created_at < $2`
	args := []any{sc.Since, sc.Until}
	if sc.ReadableBy != nil {
		args = append(args, *sc.ReadableBy)
		cond += ` AND ` + jobReadableCond(alias, "$3")
	}
	return cond, args
}

// JobTotals counts jobs by outcome. Durations and waits are in seconds and
// nil when no job had one.
type JobTotals struct {
	Total, Successful, Failed, Error, Canceled, Active int
	P50Duration, P95Duration, P50QueueWait             *float64
}

// SuccessRate is the share of jobs that ran to an outcome (successful,
// failed or error) that succeeded, or nil if none did.
func (t JobTotals) SuccessRate() *float64 {
	n := t.Successful + t.Failed + t.Error
	if n == 0 {
		return nil
	}
	r := float64(t.Successful) / float64(n)
	return &r
}

// The run time of jobs that ran to an outcome, in seconds.
const jobRunSeconds = `extract(epoch FROM j.finished_at - j.started_at)`
const jobRanFilter = `j.started_at IS NOT NULL AND j.finished_at IS NOT NULL
	AND j.status IN ('successful', 'failed', 'error')`

// GetJobTotals counts the jobs in sc.
func (s *Store) GetJobTotals(ctx context.Context, sc StatsScope) (JobTotals, error) {
	where, args := sc.where("j")
	var t JobTotals
	err := s.pool.QueryRow(ctx, `
		SELECT count(*),
			count(*) FILTER (WHERE j.status = 'successful'),
			count(*) FILTER (WHERE j.status = 'failed'),
			count(*) FILTER (WHERE j.status = 'error'),
			count(*) FILTER (WHERE j.status = 'canceled'),
			count(*) FILTER (WHERE j.status IN ('pending', 'waiting', 'running')),
			percentile_cont(0.5) WITHIN GROUP (ORDER BY `+jobRunSeconds+`) FILTER (WHERE `+jobRanFilter+`),
			percentile_cont(0.95) WITHIN GROUP (ORDER BY `+jobRunSeconds+`) FILTER (WHERE `+jobRanFilter+`),
			percentile_cont(0.5) WITHIN GROUP (ORDER BY extract(epoch FROM j.started_at - j.created_at))
				FILTER (WHERE j.started_at IS NOT NULL)
		FROM jobs j WHERE `+where, args...).Scan(
		&t.Total, &t.Successful, &t.Failed, &t.Error, &t.Canceled, &t.Active,
		&t.P50Duration, &t.P95Duration, &t.P50QueueWait)
	return t, err
}

// JobBucket counts the jobs created in one bucket of a series.
type JobBucket struct {
	Start                                       time.Time
	Successful, Failed, Error, Canceled, Active int
}

// ListJobSeries counts the jobs in sc per hour or day (bucket is "hour" or
// "day") of loc, oldest first. Every bucket from the one holding sc.Since to
// the one holding sc.Until is returned, empty or not.
func (s *Store) ListJobSeries(ctx context.Context, sc StatsScope, bucket string, loc *time.Location) ([]JobBucket, error) {
	where, args := sc.where("j")
	args = append(args, bucket, loc.String())
	n := len(args)
	rows, err := s.pool.Query(ctx, `
		SELECT date_trunc($`+strconv.Itoa(n-1)+`, j.created_at, $`+strconv.Itoa(n)+`) AS b,
			count(*) FILTER (WHERE j.status = 'successful'),
			count(*) FILTER (WHERE j.status = 'failed'),
			count(*) FILTER (WHERE j.status = 'error'),
			count(*) FILTER (WHERE j.status = 'canceled'),
			count(*) FILTER (WHERE j.status IN ('pending', 'waiting', 'running'))
		FROM jobs j WHERE `+where+` GROUP BY b`, args...)
	if err != nil {
		return nil, err
	}
	counted := map[int64]JobBucket{}
	var b JobBucket
	if _, err := pgx.ForEachRow(rows, []any{&b.Start, &b.Successful, &b.Failed, &b.Error, &b.Canceled, &b.Active}, func() error {
		counted[b.Start.Unix()] = b
		return nil
	}); err != nil {
		return nil, err
	}

	var out []JobBucket
	for start := TruncateTo(sc.Since, bucket, loc); start.Before(sc.Until); start = nextBucket(start, bucket) {
		b := counted[start.Unix()]
		b.Start = start
		out = append(out, b)
	}
	return out, nil
}

// TruncateTo returns the start of the hour or day (bucket) of loc that
// holds t.
func TruncateTo(t time.Time, bucket string, loc *time.Location) time.Time {
	t = t.In(loc)
	if bucket == "hour" {
		return time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), 0, 0, 0, loc)
	}
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, loc)
}

func nextBucket(t time.Time, bucket string) time.Time {
	if bucket == "hour" {
		// Truncate again, so a DST change doesn't misalign the series.
		return TruncateTo(t.Add(90*time.Minute), bucket, t.Location())
	}
	return t.AddDate(0, 0, 1)
}

// TemplateStat summarizes one job template's jobs in a scope.
type TemplateStat struct {
	ID          uuid.UUID
	Name        string
	Runs        int
	Failures    int
	P50Duration *float64
	// Recent are the statuses of its last 10 jobs of any age, newest first.
	Recent []string
}

// ListTemplateStats returns the limit job templates with the most jobs in
// sc, most first.
func (s *Store) ListTemplateStats(ctx context.Context, sc StatsScope, limit int) ([]TemplateStat, error) {
	where, args := sc.where("j")
	recentWhere := `true`
	if sc.ReadableBy != nil {
		recentWhere = jobReadableCond("rj", "$3")
	}
	args = append(args, limit)
	rows, err := s.pool.Query(ctx, `
		SELECT t.id, t.name, s.runs, s.failures, s.p50,
			ARRAY(SELECT rj.status FROM jobs rj WHERE rj.job_template_id = t.id AND `+recentWhere+`
				ORDER BY rj.created_at DESC LIMIT 10)
		FROM (
			SELECT j.job_template_id, count(*) AS runs,
				count(*) FILTER (WHERE j.status IN ('failed', 'error')) AS failures,
				percentile_cont(0.5) WITHIN GROUP (ORDER BY `+jobRunSeconds+`) FILTER (WHERE `+jobRanFilter+`) AS p50
			FROM jobs j WHERE `+where+` AND j.job_template_id IS NOT NULL
			GROUP BY j.job_template_id
			ORDER BY runs DESC LIMIT $`+strconv.Itoa(len(args))+`
		) s JOIN job_templates t ON t.id = s.job_template_id
		ORDER BY s.runs DESC, t.name`, args...)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (TemplateStat, error) {
		var t TemplateStat
		err := row.Scan(&t.ID, &t.Name, &t.Runs, &t.Failures, &t.P50Duration, &t.Recent)
		return t, err
	})
}

// ProblemHost is a host that failed or was unreachable in jobs in a scope.
type ProblemHost struct {
	HostName        string
	FailedJobs      int
	UnreachableJobs int
	LastSeenAt      time.Time
	LastJobID       uuid.UUID
}

// ListProblemHosts returns the limit hosts that failed (not counting
// ignored failures) or were unreachable in the most jobs in sc.
func (s *Store) ListProblemHosts(ctx context.Context, sc StatsScope, limit int) ([]ProblemHost, error) {
	where, args := sc.where("j")
	args = append(args, limit)
	rows, err := s.pool.Query(ctx, `
		SELECT e.host_name,
			count(DISTINCT e.job_id) FILTER (WHERE e.event_type = 'host_failed'),
			count(DISTINCT e.job_id) FILTER (WHERE e.event_type = 'host_unreachable'),
			max(e.created_at),
			(array_agg(e.job_id ORDER BY e.created_at DESC))[1]
		FROM jobs j JOIN job_events e ON e.job_id = j.id
		WHERE `+where+` AND e.host_name IS NOT NULL
			AND (e.event_type = 'host_unreachable'
				OR (e.event_type = 'host_failed' AND coalesce(e.data->>'ignored' = 'true', false) = false))
		GROUP BY e.host_name
		ORDER BY count(DISTINCT e.job_id) DESC, max(e.created_at) DESC, e.host_name
		LIMIT $`+strconv.Itoa(len(args)), args...)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (ProblemHost, error) {
		var h ProblemHost
		err := row.Scan(&h.HostName, &h.FailedJobs, &h.UnreachableJobs, &h.LastSeenAt, &h.LastJobID)
		return h, err
	})
}

// InventorySyncState is where an inventory source's syncing stands.
type InventorySyncState struct {
	InventoryID       uuid.UUID
	InventoryName     string
	LastSyncedAt      *time.Time
	LastJobID         *uuid.UUID
	LastJobStatus     *string
	LastJobFinishedAt *time.Time
}

// ListInventorySyncStates returns up to limit inventory sources that
// readableBy may see (all if nil), least recently synced first.
func (s *Store) ListInventorySyncStates(ctx context.Context, readableBy *uuid.UUID, limit int) ([]InventorySyncState, error) {
	where, args := `true`, []any{limit}
	if readableBy != nil {
		args = append(args, *readableBy)
		where = `EXISTS (SELECT 1 FROM user_roles r WHERE r.user_id = $2 AND r.resource_type = '` +
			string(rbac.Inventory) + `' AND (r.resource_id IS NULL OR r.resource_id = i.id))`
	}
	rows, err := s.pool.Query(ctx, `
		SELECT i.id, i.name, s.last_synced_at, s.last_job_id, lj.status, lj.finished_at
		FROM inventory_sources s
			JOIN inventories i ON i.id = s.inventory_id
			LEFT JOIN jobs lj ON lj.id = s.last_job_id
		WHERE `+where+`
		ORDER BY s.last_synced_at ASC NULLS FIRST, i.name
		LIMIT $1`, args...)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (InventorySyncState, error) {
		var st InventorySyncState
		err := row.Scan(&st.InventoryID, &st.InventoryName, &st.LastSyncedAt, &st.LastJobID, &st.LastJobStatus, &st.LastJobFinishedAt)
		return st, err
	})
}

// DeliveryKindStats counts notification deliveries to one kind of target.
type DeliveryKindStats struct {
	Kind                  string
	Sent, Failed, Pending int
}

// DeliveryFailure is a notification that could not be delivered.
type DeliveryFailure struct {
	TargetID   uuid.UUID
	TargetName string
	Kind       string
	JobID      uuid.UUID
	Event      string
	Error      string
	CreatedAt  time.Time
}

// GetDeliveryStats counts the notification deliveries created in [since,
// until) by target kind, and returns the latest limit of them that failed.
func (s *Store) GetDeliveryStats(ctx context.Context, since, until time.Time, limit int) ([]DeliveryKindStats, []DeliveryFailure, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT t.kind,
			count(*) FILTER (WHERE d.status = 'sent'),
			count(*) FILTER (WHERE d.status = 'failed'),
			count(*) FILTER (WHERE d.status = 'pending')
		FROM notification_deliveries d JOIN notification_targets t ON t.id = d.target_id
		WHERE d.created_at >= $1 AND d.created_at < $2
		GROUP BY t.kind ORDER BY t.kind`, since, until)
	if err != nil {
		return nil, nil, err
	}
	kinds, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (DeliveryKindStats, error) {
		var k DeliveryKindStats
		err := row.Scan(&k.Kind, &k.Sent, &k.Failed, &k.Pending)
		return k, err
	})
	if err != nil {
		return nil, nil, err
	}

	rows, err = s.pool.Query(ctx, `
		SELECT d.target_id, t.name, t.kind, d.job_id, d.event, d.last_error, d.created_at
		FROM notification_deliveries d JOIN notification_targets t ON t.id = d.target_id
		WHERE d.status = 'failed' AND d.created_at >= $1 AND d.created_at < $2
		ORDER BY d.created_at DESC, d.id DESC
		LIMIT $3`, since, until, limit)
	if err != nil {
		return nil, nil, err
	}
	failures, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (DeliveryFailure, error) {
		var f DeliveryFailure
		err := row.Scan(&f.TargetID, &f.TargetName, &f.Kind, &f.JobID, &f.Event, &f.Error, &f.CreatedAt)
		return f, err
	})
	return kinds, failures, err
}
