package taskmanager

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"dispatch-ops/internal/metrics"
	"dispatch-ops/internal/notify"
	"dispatch-ops/internal/store"
)

// --- Schedules -----------------------------------------------------------------

// scheduleBatch caps the schedules fired in one pass, so a backlog doesn't
// hold up dispatching.
const scheduleBatch = 100

// runSchedules launches the jobs of schedules that are due.
func (m *Manager) runSchedules(ctx context.Context) error {
	runs, err := m.Store.RunDueSchedules(ctx, scheduleBatch)
	for _, r := range runs {
		log := m.Log.With("schedule_id", r.Schedule.ID, "schedule", r.Schedule.Name, "job_template_id", r.Schedule.TemplateID, "inventory_id", r.Schedule.InventoryID)
		if r.Err != nil {
			log.Warn("schedule could not launch its job", "err", r.Err)
			continue
		}
		log.Info("schedule launched job", "job_id", r.JobID, "next_run_at", r.Schedule.NextRunAt)
	}
	return err
}

// --- Notifications -------------------------------------------------------------

const (
	notifyBatch = 20
	// notifyLease is how long a claimed notification is held. It must be
	// longer than a send can take (see notify.Sender.Timeout).
	notifyLease = 2 * time.Minute
	// notifyMaxAttempts is how often a notification is tried before it
	// is marked failed.
	notifyMaxAttempts = 5
)

// notifyRetryAfter is the wait before attempt n+1, after n attempts failed.
func notifyRetryAfter(attempts int) time.Duration {
	switch attempts {
	case 1:
		return 30 * time.Second
	case 2:
		return 2 * time.Minute
	case 3:
		return 10 * time.Minute
	default:
		return 30 * time.Minute
	}
}

// SendNotifications sends the notifications that are due, and returns how
// many it tried.
func (m *Manager) SendNotifications(ctx context.Context) (int, error) {
	total := 0
	for {
		batch, err := m.Store.ClaimDeliveries(ctx, notifyBatch, notifyLease)
		if err != nil {
			return total, err
		}
		var wg sync.WaitGroup
		for _, d := range batch {
			wg.Go(func() { m.deliver(ctx, d) })
		}
		wg.Wait()
		total += len(batch)
		if len(batch) < notifyBatch || ctx.Err() != nil {
			return total, ctx.Err()
		}
	}
}

func (m *Manager) deliver(ctx context.Context, d store.Delivery) {
	log := m.Log.With("delivery_id", d.ID, "job_id", d.JobID, "target", d.TargetName, "event", d.Event, "attempt", d.Attempts)
	start := time.Now()
	err := m.send(ctx, d)
	metrics.NotificationDuration.WithLabelValues(d.TargetKind).Observe(time.Since(start).Seconds())
	if ctx.Err() != nil {
		return // shutting down: the lease runs out and it is retried
	}

	var retry time.Duration
	result := "sent"
	switch {
	case err == nil:
		log.Info("notification sent")
	case !notify.IsPermanent(err) && d.Attempts < notifyMaxAttempts:
		retry = notifyRetryAfter(d.Attempts)
		result = "retry"
		log.Warn("notification failed; will retry", "err", err, "retry_in", retry)
	default:
		result = "failed"
		log.Error("notification failed", "err", err)
	}
	metrics.NotificationsSent.WithLabelValues(d.TargetKind, result).Inc()
	if err := m.Store.CompleteDelivery(ctx, d.ID, err, retry); err != nil {
		log.Error("recording notification result", "err", err)
	}
}

func (m *Manager) send(ctx context.Context, d store.Delivery) error {
	if m.Notifier == nil {
		return &notify.PermanentError{Err: errors.New("notifications are not configured in this task manager")}
	}
	target, err := m.Store.GetNotificationTarget(ctx, d.TargetID)
	if err != nil {
		return err
	}
	secrets, err := m.NotifyVault.Open(target)
	if err != nil {
		return &notify.PermanentError{Err: err}
	}
	job, err := m.Store.GetJob(ctx, d.JobID)
	if err != nil {
		return err
	}
	return m.Notifier.Send(ctx, target, secrets, notify.Event{Name: d.Event, Job: &job, DeliveryID: d.ID})
}

// notifyLoop sends notifications whenever woken, and regularly for retries.
func (m *Manager) notifyLoop(ctx context.Context, wake <-chan struct{}) {
	ticker := time.NewTicker(m.Config.PollInterval)
	defer ticker.Stop()
	for {
		if _, err := m.SendNotifications(ctx); err != nil && ctx.Err() == nil {
			m.Log.Error("sending notifications", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		case <-wake:
		}
	}
}

// --- Retention -----------------------------------------------------------------

// CleanUp deletes finished jobs and audit log entries older than their
// retention periods (if set).
func (m *Manager) CleanUp(ctx context.Context) error {
	var errs []error
	if age := m.Config.JobRetention; age > 0 {
		n, err := m.Store.DeleteJobsFinishedBefore(ctx, age)
		metrics.RetentionDeleted.WithLabelValues("jobs").Add(float64(n))
		if n > 0 {
			m.Log.Info("deleted old jobs", "count", n, "retention", age)
		}
		errs = append(errs, err)
	}
	if age := m.Config.AuditRetention; age > 0 {
		n, err := m.Store.DeleteAuditEntriesBefore(ctx, age)
		metrics.RetentionDeleted.WithLabelValues("audit_log").Add(float64(n))
		if n > 0 {
			m.Log.Info("deleted old audit log entries", "count", n, "retention", age)
		}
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

func (m *Manager) cleanupLoop(ctx context.Context) {
	if m.Config.JobRetention <= 0 && m.Config.AuditRetention <= 0 {
		return
	}
	// Not right at start, so a crash-looping task manager doesn't run it
	// over and over.
	t := time.NewTimer(time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		if err := m.CleanUp(ctx); err != nil && ctx.Err() == nil {
			m.Log.Error("deleting old data", "err", err)
		}
		t.Reset(m.Config.RetentionInterval)
	}
}

// --- Metrics -------------------------------------------------------------------

// Collector exports gauges read from the database (queue depth and the
// like). Only the leader reports them, so replicas don't repeat them; every
// replica reports whether it leads.
type Collector struct {
	m *Manager
}

// NewCollector returns the collector for m; register it with
// metrics.Registry.
func NewCollector(m *Manager) *Collector { return &Collector{m: m} }

var (
	descLeader = prometheus.NewDesc("dispatch_taskmanager_leader",
		"1 if this task manager is the active leader, else 0.", nil, nil)
	descJobs = prometheus.NewDesc("dispatch_jobs",
		"Jobs that are pending, waiting or running, by status.", []string{"status"}, nil)
	descOldestPending = prometheus.NewDesc("dispatch_job_queue_oldest_pending_seconds",
		"How long the oldest pending job has waited (0 if none).", nil, nil)
	descNotificationsPending = prometheus.NewDesc("dispatch_notifications_pending",
		"Notifications waiting to be sent, including those waiting to retry.", nil, nil)
)

// Describe implements prometheus.Collector.
func (c *Collector) Describe(ch chan<- *prometheus.Desc) {
	ch <- descLeader
	ch <- descJobs
	ch <- descOldestPending
	ch <- descNotificationsPending
}

// Collect implements prometheus.Collector.
func (c *Collector) Collect(ch chan<- prometheus.Metric) {
	leading := c.m.leading.Load()
	ch <- prometheus.MustNewConstMetric(descLeader, prometheus.GaugeValue, boolFloat(leading))
	if !leading {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if q, err := c.m.Store.GetQueueStats(ctx); err == nil {
		for status, n := range q.ByStatus {
			ch <- prometheus.MustNewConstMetric(descJobs, prometheus.GaugeValue, float64(n), status)
		}
		ch <- prometheus.MustNewConstMetric(descOldestPending, prometheus.GaugeValue, q.OldestPending.Seconds())
	} else {
		ch <- prometheus.NewInvalidMetric(descJobs, err)
	}
	if n, err := c.m.Store.CountPendingDeliveries(ctx); err == nil {
		ch <- prometheus.MustNewConstMetric(descNotificationsPending, prometheus.GaugeValue, float64(n))
	} else {
		ch <- prometheus.NewInvalidMetric(descNotificationsPending, err)
	}
}

func boolFloat(b bool) float64 {
	if b {
		return 1
	}
	return 0
}
