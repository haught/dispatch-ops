// Package taskmanager runs queued jobs as Kubernetes Jobs and watches over
// them: it starts jobs within the concurrency limit, notices pods that fail
// before their runner reports, enforces start, heartbeat, timeout and cancel
// deadlines, and cleans up orphaned Kubernetes objects. It also launches
// scheduled jobs, sends notifications and deletes data past its retention.
package taskmanager

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"

	"dispatch-ops/internal/auth"
	"dispatch-ops/internal/credentials"
	"dispatch-ops/internal/metrics"
	"dispatch-ops/internal/notify"
	"dispatch-ops/internal/store"
)

// Config holds the task manager's settings.
type Config struct {
	Namespace         string
	RunnerAPIURL      string
	MaxConcurrentJobs int
	JobTTL            time.Duration
	StartTimeout      time.Duration
	// JobRetention deletes jobs this long after they finish; 0 keeps them.
	JobRetention time.Duration
	// AuditRetention deletes audit log entries this old; 0 keeps them.
	AuditRetention time.Duration

	// The rest have defaults (see withDefaults); tests shorten them.

	// PollInterval is how often to reconcile when nothing wakes the loop.
	PollInterval time.Duration
	// HeartbeatTimeout fails a running job whose runner has gone quiet.
	HeartbeatTimeout time.Duration
	// ImagePullTimeout fails a waiting job whose image can't be pulled.
	ImagePullTimeout time.Duration
	// CancelGrace is how long a runner has to stop a canceled job before
	// its pod is deleted.
	CancelGrace time.Duration
	// TimeoutGrace is how long past its timeout a job may run (the runner
	// stops it itself) before its pod is deleted.
	TimeoutGrace time.Duration
	// MissingGrace is how long a waiting job may lack its Kubernetes Job,
	// e.g. after a crash between claiming the job and creating it.
	MissingGrace time.Duration
	// OrphanGrace is how long after a job finishes its pod may keep running
	// (the runner exits right after reporting) before it is deleted.
	OrphanGrace time.Duration
	// RetentionInterval is how often old data is deleted.
	RetentionInterval time.Duration
}

func (c Config) withDefaults() Config {
	def := func(d *time.Duration, v time.Duration) {
		if *d == 0 {
			*d = v
		}
	}
	def(&c.PollInterval, 10*time.Second)
	def(&c.HeartbeatTimeout, 90*time.Second)
	def(&c.ImagePullTimeout, 3*time.Minute)
	def(&c.CancelGrace, 2*time.Minute)
	def(&c.TimeoutGrace, 2*time.Minute)
	def(&c.MissingGrace, time.Minute)
	def(&c.OrphanGrace, 2*time.Minute)
	def(&c.RetentionInterval, time.Hour)
	return c
}

// Manager is the task manager. Only one should run at a time; see the
// leader election in the task-manager command.
type Manager struct {
	Store  *store.Store
	Kube   kubernetes.Interface
	Vault  credentials.Vault
	Config Config
	Log    *slog.Logger
	// Notifier sends notifications, with secrets from NotifyVault. Without
	// it, notifications fail.
	Notifier    *notify.Sender
	NotifyVault notify.Vault

	// leading is set while Run runs (i.e. while this process leads).
	leading atomic.Bool
}

// Run dispatches and reconciles jobs until ctx is done.
func (m *Manager) Run(ctx context.Context) error {
	m.Config = m.Config.withDefaults()
	m.Log.Info("task manager started", "namespace", m.Config.Namespace, "max_concurrent_jobs", m.Config.MaxConcurrentJobs,
		"job_retention", m.Config.JobRetention, "audit_retention", m.Config.AuditRetention)
	m.leading.Store(true)
	defer m.leading.Store(false)

	// Job status changes wake both the dispatcher and the notifier, which
	// sends the notifications the change queued.
	wake, wakeNotify := make(chan struct{}, 1), make(chan struct{}, 1)
	signal := func(ch chan struct{}) {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
	go m.Store.Listen(ctx, []string{store.ChannelJobs},
		func(store.Notification) {
			signal(wake)
			signal(wakeNotify)
		},
		func(err error) { m.Log.Warn("job queue listener disconnected; reconnecting", "err", err) },
	)
	go m.notifyLoop(ctx, wakeNotify)
	go m.cleanupLoop(ctx)

	ticker := time.NewTicker(m.Config.PollInterval)
	defer ticker.Stop()
	for {
		m.Tick(ctx)
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		case <-wake:
			// Let a burst of notifications (e.g. many launches) settle.
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(250 * time.Millisecond):
			}
		}
	}
}

// Tick runs one pass: launch due schedules, dispatch, reconcile.
func (m *Manager) Tick(ctx context.Context) {
	start := time.Now()
	defer func() { metrics.TaskManagerTick.Observe(time.Since(start).Seconds()) }()
	m.Config = m.Config.withDefaults()
	if err := m.runSchedules(ctx); err != nil && ctx.Err() == nil {
		m.Log.Error("running schedules", "err", err)
	}
	if err := m.dispatch(ctx); err != nil && ctx.Err() == nil {
		m.Log.Error("dispatching jobs", "err", err)
	}
	if err := m.reconcile(ctx); err != nil && ctx.Err() == nil {
		m.Log.Error("reconciling jobs", "err", err)
	}
}

// --- Dispatch ----------------------------------------------------------------

func (m *Manager) dispatch(ctx context.Context) error {
	tokens := map[uuid.UUID]string{}
	jobs, err := m.Store.ClaimJobs(ctx, m.Config.MaxConcurrentJobs, func(j store.Job) (store.Dispatch, error) {
		secret, hash, _, err := auth.NewToken(auth.JobTokenPrefix)
		if err != nil {
			return store.Dispatch{}, err
		}
		tokens[j.ID] = secret
		return store.Dispatch{TokenHash: hash, K8sNamespace: m.Config.Namespace, K8sJobName: k8sJobName(j.ID.String())}, nil
	})
	if err != nil {
		return err
	}
	for _, j := range jobs {
		if err := m.launch(ctx, j, tokens[j.ID]); err != nil {
			m.Log.Error("creating Kubernetes Job failed", "job_id", j.ID, "err", err)
			metrics.JobsDispatched.WithLabelValues("error").Inc()
			m.end(ctx, j, store.JobError, "Could not create the Kubernetes Job: "+err.Error(), true)
			continue
		}
		metrics.JobsDispatched.WithLabelValues("ok").Inc()
		m.Log.Info("job dispatched", "job_id", j.ID, "k8s_job", k8sJobName(j.ID.String()), "image", j.Config.ExecutionEnvironment.Image)
	}
	return nil
}

// launch creates the job's Secrets and Kubernetes Job, then makes the Job
// own the Secrets so they are deleted with it.
func (m *Manager) launch(ctx context.Context, j store.Job, token string) error {
	ns := m.Config.Namespace
	secrets := []*corev1.Secret{buildTokenSecret(j, ns, token)}
	if credID := j.Config.ExecutionEnvironment.RegistryCredentialID; credID != nil {
		creds, err := m.Store.GetCredentials(ctx, []uuid.UUID{*credID})
		if errors.Is(err, store.ErrNotFound) {
			return errors.New("the execution environment's registry credential no longer exists")
		}
		if err != nil {
			return err
		}
		inputs, err := m.Vault.Inputs(creds[0])
		if err != nil {
			return err
		}
		pull, err := buildPullSecret(j, ns, inputs)
		if err != nil {
			return err
		}
		secrets = append(secrets, pull)
	}

	var created []string
	cleanup := func() {
		for _, name := range created {
			_ = m.Kube.CoreV1().Secrets(ns).Delete(context.WithoutCancel(ctx), name, metav1.DeleteOptions{})
		}
	}
	for _, s := range secrets {
		if _, err := m.Kube.CoreV1().Secrets(ns).Create(ctx, s, metav1.CreateOptions{}); err != nil {
			cleanup()
			return fmt.Errorf("create secret: %w", err)
		}
		created = append(created, s.Name)
	}

	kj, err := m.Kube.BatchV1().Jobs(ns).Create(ctx, buildJob(j, m.Config, len(secrets) > 1), metav1.CreateOptions{})
	if err != nil {
		cleanup()
		return fmt.Errorf("create job: %w", err)
	}

	owner, _ := json.Marshal(map[string]any{"metadata": map[string]any{"ownerReferences": []metav1.OwnerReference{{
		APIVersion: "batch/v1", Kind: "Job", Name: kj.Name, UID: kj.UID,
		BlockOwnerDeletion: new(true),
	}}}})
	for _, name := range created {
		if _, err := m.Kube.CoreV1().Secrets(ns).Patch(ctx, name, types.MergePatchType, owner, metav1.PatchOptions{}); err != nil {
			// Not fatal: reconcile deletes unowned secrets later.
			m.Log.Warn("setting secret owner failed", "secret", name, "err", err)
		}
	}
	return nil
}

// --- Reconcile ---------------------------------------------------------------

func (m *Manager) reconcile(ctx context.Context) error {
	ns := m.Config.Namespace
	// Job timestamps come from the database's clock, so measure ages with it.
	now, err := m.Store.Now(ctx)
	if err != nil {
		return err
	}
	active, err := m.Store.ListActiveJobs(ctx)
	if err != nil {
		return err
	}
	list := metav1.ListOptions{LabelSelector: managedSelector}
	kjobs, err := m.Kube.BatchV1().Jobs(ns).List(ctx, list)
	if err != nil {
		return fmt.Errorf("list Kubernetes Jobs: %w", err)
	}
	pods, err := m.Kube.CoreV1().Pods(ns).List(ctx, list)
	if err != nil {
		return fmt.Errorf("list pods: %w", err)
	}

	kjobByID := map[uuid.UUID]*batchv1.Job{}
	for i := range kjobs.Items {
		if id, err := uuid.Parse(kjobs.Items[i].Labels[LabelJobID]); err == nil {
			kjobByID[id] = &kjobs.Items[i]
		}
	}
	podsByID := map[uuid.UUID][]corev1.Pod{}
	for _, p := range pods.Items {
		if id, err := uuid.Parse(p.Labels[LabelJobID]); err == nil {
			podsByID[id] = append(podsByID[id], p)
		}
	}

	isActive := map[uuid.UUID]bool{}
	for _, j := range active {
		isActive[j.ID] = true
		m.check(ctx, now, j, kjobByID[j.ID], podsByID[j.ID])
	}

	if err := m.reapOrphans(ctx, now, kjobByID, isActive); err != nil {
		return err
	}
	return m.reapSecrets(ctx)
}

// check decides whether an active job has failed, from its database record
// and its Kubernetes Job and pods.
func (m *Manager) check(ctx context.Context, now time.Time, j store.Job, kj *batchv1.Job, pods []corev1.Pod) {
	cfg := m.Config
	since := func(t *time.Time) time.Duration {
		if t == nil {
			return 0
		}
		return now.Sub(*t)
	}

	switch j.Status {
	case store.JobWaiting:
		switch {
		case j.CancelRequested:
			m.end(ctx, j, store.JobCanceled, "Canceled before it started", true)
		case kj == nil:
			if since(j.DispatchedAt) > cfg.MissingGrace {
				m.end(ctx, j, store.JobError, "The job's Kubernetes Job is missing", false)
			}
		case jobDone(kj):
			m.end(ctx, j, store.JobError, "The job pod ended before the runner started"+describePods(pods), false)
		case since(j.DispatchedAt) > cfg.ImagePullTimeout && imagePullProblem(pods) != "":
			m.end(ctx, j, store.JobError, fmt.Sprintf("Cannot pull image %s: %s",
				j.Config.ExecutionEnvironment.Image, imagePullProblem(pods)), true)
		case since(j.DispatchedAt) > cfg.StartTimeout:
			m.end(ctx, j, store.JobError, fmt.Sprintf("The job did not start within %s", cfg.StartTimeout)+describePods(pods), true)
		}

	case store.JobRunning:
		timeout := time.Duration(j.Config.TimeoutSeconds) * time.Second
		switch {
		case kj == nil:
			m.end(ctx, j, store.JobError, "The job's Kubernetes Job disappeared while it was running", false)
		case jobDone(kj):
			m.end(ctx, j, store.JobError, "The job pod ended without reporting a result"+describePods(pods), false)
		case since(j.LastHeartbeatAt) > cfg.HeartbeatTimeout:
			m.end(ctx, j, store.JobError, fmt.Sprintf("The runner stopped responding (no heartbeat for %s)", cfg.HeartbeatTimeout), true)
		case since(j.StartedAt) > timeout+cfg.TimeoutGrace:
			m.end(ctx, j, store.JobFailed, fmt.Sprintf("Timed out after %s (the runner did not stop the job, so its pod was deleted)", timeout), true)
		case j.CancelRequested && since(j.CancelRequestedAt) > cfg.CancelGrace:
			m.end(ctx, j, store.JobCanceled, "Canceled (the runner did not stop in time, so its pod was deleted)", true)
		}
	}
}

// end gives a job a final status, unless its runner got there first, and
// optionally deletes its Kubernetes Job.
func (m *Manager) end(ctx context.Context, j store.Job, status, reason string, deleteK8sJob bool) {
	err := m.Store.EndJob(ctx, j.ID, status, nil, reason, store.JobWaiting, store.JobRunning)
	switch {
	case errors.Is(err, store.ErrJobState):
		return // already finished
	case err != nil:
		m.Log.Error("ending job", "job_id", j.ID, "err", err)
		return
	}
	m.Log.Info("job ended by task manager", "job_id", j.ID, "status", status, "reason", reason)
	metrics.JobsEndedByTaskManager.WithLabelValues(status).Inc()
	if deleteK8sJob {
		m.deleteK8sJob(ctx, k8sJobName(j.ID.String()))
	}
}

func (m *Manager) deleteK8sJob(ctx context.Context, name string) {
	// Background propagation deletes the pods and owned Secrets too.
	policy := metav1.DeletePropagationBackground
	err := m.Kube.BatchV1().Jobs(m.Config.Namespace).Delete(ctx, name, metav1.DeleteOptions{PropagationPolicy: &policy})
	if err != nil && !apierrors.IsNotFound(err) {
		m.Log.Warn("deleting Kubernetes Job", "k8s_job", name, "err", err)
	}
}

// reapOrphans deletes Kubernetes Jobs that are still running although their
// dispatch-ops job has finished (or no longer exists).
func (m *Manager) reapOrphans(ctx context.Context, now time.Time, kjobs map[uuid.UUID]*batchv1.Job, isActive map[uuid.UUID]bool) error {
	var candidates []uuid.UUID
	for id, kj := range kjobs {
		if !isActive[id] && !jobDone(kj) {
			candidates = append(candidates, id)
		}
	}
	if len(candidates) == 0 {
		return nil
	}
	ended, err := m.Store.GetJobEnds(ctx, candidates)
	if err != nil {
		return err
	}
	for _, id := range candidates {
		finishedAt, exists := ended[id]
		switch {
		case !exists:
		case finishedAt == nil:
			continue // became active after we listed active jobs
		case now.Sub(*finishedAt) < m.Config.OrphanGrace:
			continue
		}
		m.Log.Info("deleting orphaned Kubernetes Job", "job_id", id, "k8s_job", kjobs[id].Name)
		m.deleteK8sJob(ctx, kjobs[id].Name)
	}
	return nil
}

// reapSecrets deletes job Secrets left without an owner Job, e.g. after a
// crash between creating them and creating the Job.
func (m *Manager) reapSecrets(ctx context.Context) error {
	secrets, err := m.Kube.CoreV1().Secrets(m.Config.Namespace).List(ctx, metav1.ListOptions{LabelSelector: managedSelector})
	if err != nil {
		return fmt.Errorf("list secrets: %w", err)
	}
	for _, s := range secrets.Items {
		if len(s.OwnerReferences) == 0 && time.Since(s.CreationTimestamp.Time) > 5*time.Minute {
			m.Log.Info("deleting orphaned job secret", "secret", s.Name)
			_ = m.Kube.CoreV1().Secrets(m.Config.Namespace).Delete(ctx, s.Name, metav1.DeleteOptions{})
		}
	}
	return nil
}

// --- Kubernetes status helpers ---------------------------------------------

// jobDone reports whether a Kubernetes Job has completed or failed.
func jobDone(kj *batchv1.Job) bool {
	for _, c := range kj.Status.Conditions {
		if (c.Type == batchv1.JobComplete || c.Type == batchv1.JobFailed) && c.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}

// imagePullProblem returns why a job's image can't be pulled, or "".
func imagePullProblem(pods []corev1.Pod) string {
	for _, p := range pods {
		for _, cs := range p.Status.ContainerStatuses {
			if w := cs.State.Waiting; w != nil {
				switch w.Reason {
				case "ErrImagePull", "ImagePullBackOff", "InvalidImageName", "ErrImageNeverPull":
					if w.Message != "" {
						return w.Reason + ": " + w.Message
					}
					return w.Reason
				}
			}
		}
	}
	return ""
}

// describePods summarizes pod state for a failure reason, e.g.
// " (pod: OOMKilled, exit code 137)".
func describePods(pods []corev1.Pod) string {
	for _, p := range pods {
		for _, cs := range p.Status.ContainerStatuses {
			if t := cs.State.Terminated; t != nil {
				msg := fmt.Sprintf("%s, exit code %d", t.Reason, t.ExitCode)
				if t.Message != "" {
					msg += ": " + t.Message
				}
				return " (container " + msg + ")"
			}
			if w := cs.State.Waiting; w != nil && w.Reason != "" && w.Reason != "ContainerCreating" {
				msg := w.Reason
				if w.Message != "" {
					msg += ": " + w.Message
				}
				return " (container " + msg + ")"
			}
		}
		for _, c := range p.Status.Conditions {
			if c.Type == corev1.PodScheduled && c.Status == corev1.ConditionFalse && c.Message != "" {
				return " (pod not scheduled: " + c.Message + ")"
			}
		}
		if p.Status.Reason != "" {
			return " (pod " + p.Status.Reason + ": " + p.Status.Message + ")"
		}
	}
	return ""
}
