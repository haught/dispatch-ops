// Package metrics defines the Prometheus metrics dispatch-ops exports and
// serves them.
//
// Counters are per process: each API server and task manager counts what it
// did, and Prometheus sums them. Gauges read from the database (queue depth
// and the like) are exported by the task manager leader only, so they are
// not repeated by every replica.
package metrics

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

const namespace = "dispatch"

// Registry holds every dispatch-ops metric plus the Go runtime and process
// collectors.
var Registry = prometheus.NewRegistry()

func init() {
	Registry.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
		HTTPRequests, HTTPDuration, JobStreams,
		JobsLaunched, JobsFinished, JobDuration, JobQueueWait,
		JobsDispatched, JobsEndedByTaskManager, ScheduleRuns,
		NotificationsSent, NotificationDuration, RetentionDeleted, TaskManagerTick,
	)
}

// jobDurationBuckets span a few seconds to a day.
var jobDurationBuckets = []float64{1, 5, 15, 30, 60, 120, 300, 600, 1800, 3600, 3 * 3600, 6 * 3600, 24 * 3600}

var (
	// HTTPRequests counts API requests by OpenAPI operation ID.
	HTTPRequests = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace, Name: "http_requests_total",
		Help: "HTTP requests by API operation, method and status code. Requests outside the API are operation \"ui\", \"probe\" or \"unmatched\".",
	}, []string{"operation", "method", "code"})
	// HTTPDuration observes request latency, except for live job streams.
	HTTPDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: namespace, Name: "http_request_duration_seconds",
		Help:    "HTTP request latency by API operation (live job streams excluded).",
		Buckets: prometheus.DefBuckets,
	}, []string{"operation"})
	// JobStreams is the number of open live job streams (SSE).
	JobStreams = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: namespace, Name: "job_streams_active",
		Help: "Open live job output streams.",
	})

	// JobsLaunched counts jobs queued, by how they were launched.
	JobsLaunched = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace, Name: "jobs_launched_total",
		Help: "Jobs queued, by job type and launch type (manual, api, schedule, relaunch).",
	}, []string{"job_type", "launch_type"})
	// JobsFinished counts jobs reaching a final status.
	JobsFinished = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace, Name: "jobs_finished_total",
		Help: "Jobs that reached a final status, by job type and status.",
	}, []string{"job_type", "status"})
	// JobDuration observes how long finished jobs ran.
	JobDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: namespace, Name: "job_duration_seconds",
		Help:    "Run time of finished jobs that started, from start to finish, by job type and status.",
		Buckets: jobDurationBuckets,
	}, []string{"job_type", "status"})
	// JobQueueWait observes how long jobs waited between launch and start.
	JobQueueWait = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: namespace, Name: "job_queue_wait_seconds",
		Help:    "Time from launch until the job's runner started, by job type.",
		Buckets: []float64{1, 2, 5, 10, 30, 60, 120, 300, 600, 1800, 3600},
	}, []string{"job_type"})

	// JobsDispatched counts Kubernetes Jobs the task manager created.
	JobsDispatched = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace, Name: "taskmanager_jobs_dispatched_total",
		Help: "Jobs handed to Kubernetes, by result (ok, error).",
	}, []string{"result"})
	// JobsEndedByTaskManager counts jobs the task manager failed or
	// canceled itself, e.g. after a timeout.
	JobsEndedByTaskManager = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace, Name: "taskmanager_jobs_ended_total",
		Help: "Jobs the task manager ended itself (timeouts, lost pods, cancel deadlines), by status.",
	}, []string{"status"})
	// ScheduleRuns counts schedule firings.
	ScheduleRuns = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace, Name: "schedule_runs_total",
		Help: "Schedule firings, by result (launched, error).",
	}, []string{"result"})
	// NotificationsSent counts notification delivery attempts.
	NotificationsSent = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace, Name: "notifications_total",
		Help: "Notification delivery attempts, by target kind and result (sent, retry, failed).",
	}, []string{"kind", "result"})
	// NotificationDuration observes how long sending a notification took.
	NotificationDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: namespace, Name: "notification_send_duration_seconds",
		Help:    "Time to send one notification, by target kind.",
		Buckets: prometheus.DefBuckets,
	}, []string{"kind"})
	// RetentionDeleted counts rows deleted by retention.
	RetentionDeleted = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace, Name: "retention_deleted_total",
		Help: "Rows deleted because they were older than the retention period, by kind (jobs, audit_log).",
	}, []string{"kind"})
	// TaskManagerTick observes the duration of task manager passes.
	TaskManagerTick = prometheus.NewHistogram(prometheus.HistogramOpts{
		Namespace: namespace, Name: "taskmanager_tick_duration_seconds",
		Help:    "Time for one task manager pass (schedules, dispatch and reconcile).",
		Buckets: prometheus.DefBuckets,
	})
)

// Handler serves the registry in the Prometheus exposition format.
func Handler() http.Handler {
	return promhttp.HandlerFor(Registry, promhttp.HandlerOpts{Registry: Registry})
}

// Serve serves /metrics on addr until ctx is done. Metrics get their own
// listener so they are not exposed wherever the API is.
func Serve(ctx context.Context, addr string, log *slog.Logger) {
	mux := http.NewServeMux()
	mux.Handle("GET /metrics", Handler())
	srv := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdown)
	}()
	log.Info("serving metrics", "addr", addr)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Error("metrics server failed", "addr", addr, "err", err)
	}
}

// Operation labels for requests that are not API operations.
const (
	OpUI        = "ui"
	OpProbe     = "probe"
	OpUnmatched = "unmatched"
)

type opKey struct{}

// opHolder carries the matched API operation back out to the middleware
// that records the request.
type opHolder struct{ op string }

// WithOperationSlot returns a context in which SetOperation can record the
// operation a request matched.
func WithOperationSlot(ctx context.Context) context.Context {
	return context.WithValue(ctx, opKey{}, &opHolder{})
}

// SetOperation records the API operation the request matched.
func SetOperation(ctx context.Context, op string) {
	if h, ok := ctx.Value(opKey{}).(*opHolder); ok {
		h.op = op
	}
}

// Operation returns the operation recorded with SetOperation, or "".
func Operation(ctx context.Context) string {
	if h, ok := ctx.Value(opKey{}).(*opHolder); ok {
		return h.op
	}
	return ""
}
