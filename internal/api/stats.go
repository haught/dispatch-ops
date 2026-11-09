package api

import (
	"context"
	"time"

	"dispatch-ops/internal/api/apigen"
	"dispatch-ops/internal/store"
)

// How many rows each part of the dashboard statistics returns.
const (
	statsTemplates        = 20
	statsProblemHosts     = 10
	statsInventorySyncs   = 50
	statsDeliveryFailures = 5
)

func (h *Handlers) GetStats(ctx context.Context, req apigen.GetStatsRequestObject) (apigen.GetStatsResponseObject, error) {
	window := apigen.N7d
	if req.Params.Window != nil {
		window = *req.Params.Window
	}
	loc := time.UTC
	if tz := deref(req.Params.Tz); tz != "" {
		var err error
		if loc, err = time.LoadLocation(tz); err != nil {
			return nil, badRequest("Unknown time zone %q", tz)
		}
	}

	// The window is whole buckets ending with the current one; the previous
	// window is as long and ends as far into its last bucket, so the two
	// compare like for like.
	now := time.Now()
	bucket := apigen.Day
	var since time.Time
	var back func(time.Time) time.Time
	switch window {
	case apigen.N24h:
		bucket = apigen.Hour
		since = store.TruncateTo(now, string(bucket), loc).Add(-23 * time.Hour)
		back = func(t time.Time) time.Time { return t.Add(-24 * time.Hour) }
	case apigen.N30d:
		since = store.TruncateTo(now, string(bucket), loc).AddDate(0, 0, -29)
		back = func(t time.Time) time.Time { return t.AddDate(0, 0, -30) }
	default:
		window = apigen.N7d
		since = store.TruncateTo(now, string(bucket), loc).AddDate(0, 0, -6)
		back = func(t time.Time) time.Time { return t.AddDate(0, 0, -7) }
	}
	// Include jobs created during this request.
	until := now.Add(time.Second)
	scope := store.StatsScope{Since: since, Until: until, ReadableBy: readableBy(ctx)}
	prev := store.StatsScope{Since: back(since), Until: back(until), ReadableBy: scope.ReadableBy}

	totals, err := h.Store.GetJobTotals(ctx, scope)
	if err != nil {
		return nil, err
	}
	prevTotals, err := h.Store.GetJobTotals(ctx, prev)
	if err != nil {
		return nil, err
	}
	series, err := h.Store.ListJobSeries(ctx, scope, string(bucket), loc)
	if err != nil {
		return nil, err
	}
	templates, err := h.Store.ListTemplateStats(ctx, scope, statsTemplates)
	if err != nil {
		return nil, err
	}
	hosts, err := h.Store.ListProblemHosts(ctx, scope, statsProblemHosts)
	if err != nil {
		return nil, err
	}
	syncs, err := h.Store.ListInventorySyncStates(ctx, scope.ReadableBy, statsInventorySyncs)
	if err != nil {
		return nil, err
	}

	out := apigen.DashboardStats{
		Window:         window,
		Bucket:         bucket,
		Totals:         toAPIJobTotals(totals),
		PreviousTotals: toAPIJobTotals(prevTotals),
		Series:         make([]apigen.JobSeriesBucket, 0, len(series)),
		Templates:      make([]apigen.TemplateStats, 0, len(templates)),
		ProblemHosts:   make([]apigen.ProblemHost, 0, len(hosts)),
		InventorySyncs: make([]apigen.InventorySyncHealth, 0, len(syncs)),
	}
	for _, b := range series {
		out.Series = append(out.Series, apigen.JobSeriesBucket{
			Start: b.Start, Successful: b.Successful, Failed: b.Failed, Error: b.Error, Canceled: b.Canceled, Active: b.Active,
		})
	}
	for _, t := range templates {
		recent := make([]apigen.JobStatus, len(t.Recent))
		for i, s := range t.Recent {
			recent[i] = apigen.JobStatus(s)
		}
		out.Templates = append(out.Templates, apigen.TemplateStats{
			Id: t.ID, Name: t.Name, Runs: t.Runs, Failures: t.Failures, P50DurationSeconds: f32(t.P50Duration), Recent: recent,
		})
	}
	for _, ph := range hosts {
		out.ProblemHosts = append(out.ProblemHosts, apigen.ProblemHost{
			HostName: ph.HostName, FailedJobs: ph.FailedJobs, UnreachableJobs: ph.UnreachableJobs,
			LastSeenAt: ph.LastSeenAt, LastJobId: ph.LastJobID,
		})
	}
	for _, s := range syncs {
		out.InventorySyncs = append(out.InventorySyncs, apigen.InventorySyncHealth{
			InventoryId: s.InventoryID, InventoryName: s.InventoryName, LastSyncedAt: s.LastSyncedAt,
			LastJobId: s.LastJobID, LastJobStatus: (*apigen.JobStatus)(s.LastJobStatus), LastJobFinishedAt: s.LastJobFinishedAt,
		})
	}

	// The queue and notifications aren't limited to what the caller may
	// see, so only those who may see everything get them.
	if p := caller(ctx); p.IsAdmin || p.IsAuditor {
		if out.Queue, err = h.queueStats(ctx); err != nil {
			return nil, err
		}
		if out.Notifications, err = h.notificationStats(ctx, since, until); err != nil {
			return nil, err
		}
	}
	return apigen.GetStats200JSONResponse(out), nil
}

func (h *Handlers) queueStats(ctx context.Context) (*apigen.QueueStats, error) {
	q, err := h.Store.GetQueueStats(ctx)
	if err != nil {
		return nil, err
	}
	pending, err := h.Store.CountPendingDeliveries(ctx)
	if err != nil {
		return nil, err
	}
	return &apigen.QueueStats{
		Pending: q.ByStatus[store.JobPending], Waiting: q.ByStatus[store.JobWaiting], Running: q.ByStatus[store.JobRunning],
		OldestPendingSeconds: float32(q.OldestPending.Seconds()), NotificationsPending: pending,
	}, nil
}

func (h *Handlers) notificationStats(ctx context.Context, since, until time.Time) (*apigen.NotificationStats, error) {
	kinds, failures, err := h.Store.GetDeliveryStats(ctx, since, until, statsDeliveryFailures)
	if err != nil {
		return nil, err
	}
	out := &apigen.NotificationStats{
		ByKind:         make([]apigen.NotificationKindStats, 0, len(kinds)),
		RecentFailures: make([]apigen.NotificationFailure, 0, len(failures)),
	}
	for _, k := range kinds {
		out.ByKind = append(out.ByKind, apigen.NotificationKindStats{
			Kind: apigen.NotificationKind(k.Kind), Sent: k.Sent, Failed: k.Failed, Pending: k.Pending,
		})
	}
	for _, f := range failures {
		out.RecentFailures = append(out.RecentFailures, apigen.NotificationFailure{
			TargetId: f.TargetID, TargetName: f.TargetName, Kind: apigen.NotificationKind(f.Kind), JobId: f.JobID,
			Event: apigen.NotificationEvent(f.Event), Error: f.Error, CreatedAt: f.CreatedAt,
		})
	}
	return out, nil
}

func toAPIJobTotals(t store.JobTotals) apigen.JobTotals {
	return apigen.JobTotals{
		Total: t.Total, Successful: t.Successful, Failed: t.Failed, Error: t.Error, Canceled: t.Canceled, Active: t.Active,
		SuccessRate: f32(t.SuccessRate()), P50DurationSeconds: f32(t.P50Duration), P95DurationSeconds: f32(t.P95Duration),
		P50QueueWaitSeconds: f32(t.P50QueueWait),
	}
}

func f32(p *float64) *float32 {
	if p == nil {
		return nil
	}
	v := float32(*p)
	return &v
}
