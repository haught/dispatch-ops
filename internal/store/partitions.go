package store

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
)

// Job output is partitioned by the day (UTC) its job was created, using the
// time in the job's UUIDv7 (see migration 15). The task manager creates
// partitions ahead of time and drops whole days once their jobs have
// expired.

// jobPartitioned are the tables partitioned by job.
var jobPartitioned = []string{"job_events", "job_host_results"}

// JobPartition is one day's partition of a job-partitioned table.
type JobPartition struct {
	Table  string // the partition
	Parent string
	Day    time.Time // UTC
	// DetachPending is set if an earlier drop was interrupted mid-detach.
	DetachPending bool
}

// EnsureJobPartitions creates any missing partitions for jobs created from
// yesterday to ahead days from today (UTC).
func (s *Store) EnsureJobPartitions(ctx context.Context, ahead int) error {
	_, err := s.pool.Exec(ctx, `
		SELECT ensure_job_partitions((now() AT TIME ZONE 'UTC')::date - 1, (now() AT TIME ZONE 'UTC')::date + $1::int)`, ahead)
	return err
}

// ListJobPartitions returns the partitions of job-partitioned tables,
// oldest first.
func (s *Store) ListJobPartitions(ctx context.Context) ([]JobPartition, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT c.relname::text, p.relname::text, job_partition_day(c.relname), i.inhdetachpending
		FROM pg_inherits i
		JOIN pg_class c ON c.oid = i.inhrelid
		JOIN pg_class p ON p.oid = i.inhparent
		WHERE i.inhparent IN (SELECT to_regclass(t) FROM unnest($1::text[]) t)
		ORDER BY 3, 1`, jobPartitioned)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (JobPartition, error) {
		var p JobPartition
		err := row.Scan(&p.Table, &p.Parent, &p.Day, &p.DetachPending)
		return p, err
	})
}

// JobPartitionsAhead returns how many days past today (UTC) every
// job-partitioned table has partitions for: 0 means only today's exist,
// and a negative number that today's jobs can't store output.
func (s *Store) JobPartitionsAhead(ctx context.Context) (int, error) {
	var ahead int
	err := s.pool.QueryRow(ctx, `
		SELECT coalesce(min(last) - (now() AT TIME ZONE 'UTC')::date, -1)
		FROM (
			SELECT max(job_partition_day(c.relname)) AS last
			FROM unnest($1::text[]) t
			LEFT JOIN pg_inherits i ON i.inhparent = to_regclass(t) AND NOT i.inhdetachpending
			LEFT JOIN pg_class c ON c.oid = i.inhrelid
			GROUP BY t
		) p`, jobPartitioned).Scan(&ahead)
	return ahead, err
}

// ExpiredJobs is what DeleteExpiredJobs removed.
type ExpiredJobs struct {
	Jobs       int64 // job rows
	Partitions int   // partitions dropped, with all their rows
}

// DeleteExpiredJobs deletes whole days of jobs, and their output, once every
// job created that day (UTC) finished more than age ago. A day is dropped
// as a unit: its partitions are detached and dropped, then its job rows
// deleted, whose output is by then gone. So jobs are kept at least age,
// and up to a day longer. A day with a job that hasn't finished is kept.
func (s *Store) DeleteExpiredJobs(ctx context.Context, age time.Duration) (ExpiredJobs, error) {
	var out ExpiredJobs
	var cutoff time.Time
	if err := s.pool.QueryRow(ctx, `SELECT now() - make_interval(secs => $1)`, age.Seconds()).Scan(&cutoff); err != nil {
		return out, err
	}
	// Days entirely before the cutoff: their jobs were all created before it.
	last := cutoff.UTC().Truncate(24 * time.Hour)

	parts, err := s.ListJobPartitions(ctx)
	if err != nil {
		return out, err
	}
	byDay := map[time.Time][]JobPartition{}
	for _, p := range parts {
		if p.Day.Before(last) {
			byDay[p.Day] = append(byDay[p.Day], p)
		}
	}
	// Days of jobs whose partitions are already gone (an interrupted run).
	rows, err := s.pool.Query(ctx, `
		SELECT DISTINCT (uuid_extract_timestamp(id) AT TIME ZONE 'UTC')::date
		FROM jobs WHERE id < job_id_floor($1)`, last)
	if err != nil {
		return out, err
	}
	jobDays, err := pgx.CollectRows(rows, pgx.RowTo[time.Time])
	if err != nil {
		return out, err
	}
	for _, d := range jobDays {
		if _, ok := byDay[d]; !ok {
			byDay[d] = nil
		}
	}
	days := make([]time.Time, 0, len(byDay))
	for d := range byDay {
		days = append(days, d)
	}
	slices.SortFunc(days, func(a, b time.Time) int { return a.Compare(b) })

	for _, day := range days {
		lo, hi := day, day.AddDate(0, 0, 1)
		var keep bool
		if err := s.pool.QueryRow(ctx, `
			SELECT EXISTS (SELECT 1 FROM jobs
				WHERE id >= job_id_floor($1) AND id < job_id_floor($2)
					AND (finished_at IS NULL OR finished_at >= $3))`, lo, hi, cutoff).Scan(&keep); err != nil {
			return out, err
		}
		if keep {
			continue
		}
		for _, p := range byDay[day] {
			if err := s.dropJobPartition(ctx, p); err != nil {
				return out, err
			}
			out.Partitions++
		}
		n, err := deleteInBatches(ctx, s, `
			DELETE FROM jobs WHERE id IN (
				SELECT id FROM jobs WHERE id >= job_id_floor($1) AND id < job_id_floor($2) LIMIT $3)`, lo, hi)
		out.Jobs += n
		if err != nil {
			return out, err
		}
	}
	return out, nil
}

// dropJobPartition detaches p without blocking writers to its parent, then
// drops it.
func (s *Store) dropJobPartition(ctx context.Context, p JobPartition) error {
	mode := "CONCURRENTLY"
	if p.DetachPending {
		mode = "FINALIZE"
	}
	parent, part := pgx.Identifier{p.Parent}.Sanitize(), pgx.Identifier{p.Table}.Sanitize()
	// Not in a transaction: DETACH ... CONCURRENTLY can't run in one.
	if _, err := s.pool.Exec(ctx, fmt.Sprintf(`ALTER TABLE %s DETACH PARTITION %s %s`, parent, part, mode)); err != nil {
		return fmt.Errorf("detach %s: %w", p.Table, err)
	}
	if _, err := s.pool.Exec(ctx, `DROP TABLE `+part); err != nil {
		return fmt.Errorf("drop %s: %w", p.Table, err)
	}
	return nil
}
