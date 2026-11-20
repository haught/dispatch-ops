package store

import (
	"context"
	"encoding/json"
	"math"
	"slices"
	"strconv"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// maxJobHosts caps the hosts one job may record results for. Results are
// counted past the output cap, so without it a job could add rows without
// end by reporting made-up hosts.
var maxJobHosts = 100_000

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

// Host statuses, worst first.
const (
	HostUnreachable = "unreachable"
	HostFailed      = "failed"
	HostChanged     = "changed"
	HostOK          = "ok"
	HostSkipped     = "skipped"
)

// HostStatuses are the host statuses, worst first.
var HostStatuses = []string{HostUnreachable, HostFailed, HostChanged, HostOK, HostSkipped}

// Status is the host's overall result: the worst of its task results. It
// matches job_host_results.status.
func (h JobHostSummary) Status() string {
	switch {
	case h.Unreachable > 0:
		return HostUnreachable
	case h.Failed > 0:
		return HostFailed
	case h.Changed > 0:
		return HostChanged
	case h.OK > 0:
		return HostOK
	}
	return HostSkipped
}

// hostTally accumulates one batch's host results, to add to (or, for a
// recap, replace) the stored ones.
type hostTally struct {
	counts map[string]*JobHostSummary
	recap  map[string]*JobHostSummary
}

func (t *hostTally) host(m *map[string]*JobHostSummary, name string) *JobHostSummary {
	if *m == nil {
		*m = map[string]*JobHostSummary{}
	}
	h := (*m)[name]
	if h == nil {
		h = &JobHostSummary{HostName: name}
		(*m)[name] = h
	}
	return h
}

// add counts one event. Like Ansible's recap, a failure with ignore_errors
// counts as ok and ignored. data is the event's data as JSON.
func (t *hostTally) add(typ string, host *string, data *string) {
	if typ == "playbook_stats" {
		t.addRecap(data)
		return
	}
	var d struct {
		Changed bool `json:"changed"`
		Ignored bool `json:"ignored"`
	}
	if data != nil {
		_ = json.Unmarshal([]byte(*data), &d)
	}
	var r JobHostSummary
	switch typ {
	case "host_ok":
		r.OK = 1
	case "host_failed":
		if d.Ignored {
			r.OK, r.Ignored = 1, 1
		} else {
			r.Failed = 1
		}
	case "host_unreachable":
		r.Unreachable = 1
	case "host_skipped":
		r.Skipped = 1
	default:
		return
	}
	if r.OK > 0 && d.Changed {
		r.Changed = 1
	}
	if host == nil {
		return
	}
	h := t.host(&t.counts, *host)
	h.OK += r.OK
	h.Changed += r.Changed
	h.Failed += r.Failed
	h.Unreachable += r.Unreachable
	h.Skipped += r.Skipped
	h.Ignored += r.Ignored
}

// addRecap takes a chunk of Ansible's recap, whose numbers replace the
// counts for the hosts it lists (it also knows about rescued tasks).
func (t *hostTally) addRecap(data *string) {
	if data == nil {
		return
	}
	var chunk struct {
		Hosts map[string]struct {
			OK          float64 `json:"ok"`
			Changed     float64 `json:"changed"`
			Failures    float64 `json:"failures"`
			Unreachable float64 `json:"unreachable"`
			Skipped     float64 `json:"skipped"`
			Rescued     float64 `json:"rescued"`
			Ignored     float64 `json:"ignored"`
		} `json:"hosts"`
	}
	if json.Unmarshal([]byte(*data), &chunk) != nil {
		return
	}
	for name, st := range chunk.Hosts {
		h := t.host(&t.recap, name)
		h.OK, h.Changed, h.Failed, h.Unreachable = count(st.OK), count(st.Changed), count(st.Failures), count(st.Unreachable)
		h.Skipped, h.Rescued, h.Ignored = count(st.Skipped), count(st.Rescued), count(st.Ignored)
		// The recap replaces what this batch counted for the host.
		delete(t.counts, name)
	}
}

// count keeps a reported number within the database's integer columns.
func count(f float64) int {
	if !(f > 0) {
		return 0
	}
	return int(min(f, math.MaxInt32))
}

func (t *hostTally) empty() bool { return len(t.counts) == 0 && len(t.recap) == 0 }

// save adds the tally to the job's stored host results, within maxJobHosts.
func (t *hostTally) save(ctx context.Context, tx pgx.Tx, jobID uuid.UUID, hostCount int) (int, error) {
	if t.empty() {
		return hostCount, nil
	}
	// Near the cap, drop hosts the job doesn't have yet beyond it.
	if hostCount+len(t.counts)+len(t.recap) > maxJobHosts {
		names := make([]string, 0, len(t.counts)+len(t.recap))
		for n := range t.counts {
			names = append(names, n)
		}
		for n := range t.recap {
			names = append(names, n)
		}
		rows, err := tx.Query(ctx, `SELECT host_name FROM job_host_results WHERE job_id = $1 AND host_name = ANY($2)`, jobID, names)
		if err != nil {
			return hostCount, err
		}
		known, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			return hostCount, err
		}
		room := maxJobHosts - hostCount
		slices.Sort(names)
		for _, n := range names {
			if slices.Contains(known, n) {
				continue
			}
			if room > 0 {
				room--
				continue
			}
			delete(t.counts, n)
			delete(t.recap, n)
		}
	}

	inserted := 0
	upsert := func(m map[string]*JobHostSummary, onConflict string) error {
		if len(m) == 0 {
			return nil
		}
		var names []string
		var ok, changed, failed, unreachable, skipped, rescued, ignored []int
		for _, h := range m {
			names = append(names, h.HostName)
			ok, changed, failed = append(ok, h.OK), append(changed, h.Changed), append(failed, h.Failed)
			unreachable, skipped = append(unreachable, h.Unreachable), append(skipped, h.Skipped)
			rescued, ignored = append(rescued, h.Rescued), append(ignored, h.Ignored)
		}
		rows, err := tx.Query(ctx, `
			INSERT INTO job_host_results (job_id, host_name, ok, changed, failed, unreachable, skipped, rescued, ignored)
			SELECT $1, h.* FROM unnest($2::text[], $3::int[], $4::int[], $5::int[], $6::int[], $7::int[], $8::int[], $9::int[])
				AS h(host_name, ok, changed, failed, unreachable, skipped, rescued, ignored)
			ON CONFLICT (job_id, host_name) DO UPDATE SET `+onConflict+`
			RETURNING old.job_id IS NULL`,
			jobID, names, ok, changed, failed, unreachable, skipped, rescued, ignored)
		if err != nil {
			return err
		}
		created, err := pgx.CollectRows(rows, pgx.RowTo[bool])
		for _, c := range created {
			if c {
				inserted++
			}
		}
		return err
	}
	// Each counter as a sum (task results) or as reported (the recap).
	sets := func(f func(col string) string) string {
		var s []string
		for _, col := range []string{"ok", "changed", "failed", "unreachable", "skipped", "rescued", "ignored"} {
			s = append(s, col+" = "+f(col))
		}
		return strings.Join(s, ", ")
	}
	if err := upsert(t.counts, sets(func(c string) string { return "job_host_results." + c + " + excluded." + c })); err != nil {
		return hostCount, err
	}
	if err := upsert(t.recap, sets(func(c string) string { return "excluded." + c })); err != nil {
		return hostCount, err
	}
	return hostCount + inserted, nil
}

// JobHostFilter narrows ListJobHostSummaries.
type JobHostFilter struct {
	Statuses []string // any of these, if set
	Search   string   // host name substring, if set
}

// JobHosts is a page of a job's host results.
type JobHosts struct {
	Items []JobHostSummary
	Total int // hosts matching the filter
	// ByStatus counts all of the job's hosts by status.
	ByStatus map[string]int
}

// ListJobHostSummaries returns a page of a job's per-host results, by host
// name, and counts. While the job runs they are counted from the task
// results received so far; once the recap arrives, its numbers replace
// those. It returns ErrNotFound if the job does not exist.
func (s *Store) ListJobHostSummaries(ctx context.Context, jobID uuid.UUID, f JobHostFilter, p Page) (JobHosts, error) {
	out := JobHosts{ByStatus: map[string]int{}}
	var exists bool
	if err := s.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM jobs WHERE id = $1)`, jobID).Scan(&exists); err != nil {
		return out, err
	}
	if !exists {
		return out, ErrNotFound
	}

	rows, err := s.pool.Query(ctx, `SELECT status, count(*) FROM job_host_results WHERE job_id = $1 GROUP BY status`, jobID)
	if err != nil {
		return out, err
	}
	var status string
	var n int
	if _, err := pgx.ForEachRow(rows, []any{&status, &n}, func() error {
		out.ByStatus[status] = n
		return nil
	}); err != nil {
		return out, err
	}

	where, args := `job_id = $1`, []any{jobID}
	if len(f.Statuses) > 0 {
		args = append(args, f.Statuses)
		where += ` AND status = ANY($2)`
	}
	if f.Search != "" {
		args = append(args, likePattern(f.Search))
		where += ` AND host_name ILIKE $` + strconv.Itoa(len(args))
	}
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM job_host_results WHERE `+where, args...).Scan(&out.Total); err != nil {
		return out, err
	}
	if p.Limit <= 0 {
		p.Limit = maxJobHosts
	}
	args = append(args, p.Limit, p.Offset)
	rows, err = s.pool.Query(ctx, `
		SELECT host_name, ok, changed, failed, unreachable, skipped, rescued, ignored
		FROM job_host_results WHERE `+where+`
		ORDER BY host_name LIMIT $`+strconv.Itoa(len(args)-1)+` OFFSET $`+strconv.Itoa(len(args)), args...)
	if err != nil {
		return out, err
	}
	out.Items, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (JobHostSummary, error) {
		var h JobHostSummary
		err := row.Scan(&h.HostName, &h.OK, &h.Changed, &h.Failed, &h.Unreachable, &h.Skipped, &h.Rescued, &h.Ignored)
		return h, err
	})
	return out, err
}
