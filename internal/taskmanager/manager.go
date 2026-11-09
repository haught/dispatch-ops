// Package taskmanager runs queued jobs as Kubernetes Jobs and watches over
// them: it starts jobs within the concurrency limit, notices pods that fail
// before their runner reports, enforces start, heartbeat, timeout and cancel
// deadlines, and cleans up orphaned Kubernetes objects. It also launches
// scheduled jobs, sends notifications and deletes data past its retention.
package taskmanager

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"

	"dispatch-ops/internal/auth"
	"dispatch-ops/internal/credentials"
	"dispatch-ops/internal/metrics"
	"dispatch-ops/internal/notify"
	"dispatch-ops/internal/store"
	"dispatch-ops/internal/tlscert"
)

// Config holds the task manager's settings.
type Config struct {
	Namespace string
	// Installation labels the Kubernetes objects this task manager creates
	// (joblabels.Installation) and names its leader Lease: the operator
	// sets it to the DispatchOps's UID. The task manager deletes only
	// Kubernetes Jobs with its own label or whose job is in its database
	// (see Manager.isOurs). Empty, they are unlabeled.
	Installation string
	RunnerAPIURL string
	// RunnerAPITLSDir holds the API server's certificate Secret; job pods
	// trust what tlscert.ReadTrustBundle reads from it. Read for each job,
	// so a renewed certificate reaches new jobs. Empty means job pods trust
	// only the system's roots.
	RunnerAPITLSDir   string
	MaxConcurrentJobs int
	JobTTL            time.Duration
	StartTimeout      time.Duration
	// JobRetention deletes jobs this long after they finish; 0 keeps them.
	JobRetention time.Duration
	// AuditRetention deletes audit log entries this old; 0 keeps them.
	AuditRetention time.Duration
	// JobResources are job pods' CPU and memory requests and limits.
	JobResources corev1.ResourceRequirements
	// JobWorkSizeLimit caps job pods' working directory; zero means no cap.
	JobWorkSizeLimit resource.Quantity

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
			m.end(ctx, j, store.JobError, "Could not create the Kubernetes Job: "+err.Error(), nil)
			continue
		}
		metrics.JobsDispatched.WithLabelValues("ok").Inc()
		m.Log.Info("job dispatched", "job_id", j.ID, "k8s_job", k8sJobName(j.ID.String()), "image", j.Config.ExecutionEnvironment.Image)
	}
	return nil
}

// launch creates the job's Kubernetes Job suspended, then its Secrets with
// that Job as their owner, so they are garbage collected with it, and only
// then lets the Job start (its pod needs the Secrets). So the task manager
// only ever creates Secrets: its Role has no other verb on them, not even
// patch, which would return any Secret it names. If anything fails once the
// Job exists, deleting the Job deletes whatever Secrets were made.
func (m *Manager) launch(ctx context.Context, j store.Job, token string) error {
	ns := m.Config.Namespace
	var ca []byte
	if dir := m.Config.RunnerAPITLSDir; dir != "" {
		var err error
		if ca, err = tlscert.ReadTrustBundle(dir); err != nil {
			return fmt.Errorf("read the API server's certificate for the runner: %w", err)
		}
	}
	secrets := []*corev1.Secret{buildTokenSecret(j, m.Config, token, ca)}
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
		pull, err := buildPullSecret(j, m.Config, inputs)
		if err != nil {
			return err
		}
		secrets = append(secrets, pull)
	}

	kj, err := m.Kube.BatchV1().Jobs(ns).Create(ctx, buildJob(j, m.Config, len(secrets) > 1, len(ca) > 0), metav1.CreateOptions{})
	if err != nil {
		return fmt.Errorf("create job: %w", err)
	}
	fail := func(err error) error {
		m.deleteK8sJob(context.WithoutCancel(ctx), kj, true)
		return err
	}
	// No blockOwnerDeletion: it needs update on jobs/finalizers where
	// OwnerReferencesPermissionEnforcement is on, and the task manager
	// deletes Jobs in the background anyway.
	owner := metav1.OwnerReference{APIVersion: "batch/v1", Kind: "Job", Name: kj.Name, UID: kj.UID}
	for _, s := range secrets {
		s.OwnerReferences = []metav1.OwnerReference{owner}
		if _, err := m.Kube.CoreV1().Secrets(ns).Create(ctx, s, metav1.CreateOptions{}); err != nil {
			return fail(fmt.Errorf("create secret: %w", err))
		}
	}
	if _, err := m.Kube.BatchV1().Jobs(ns).Patch(ctx, kj.Name, types.MergePatchType, []byte(`{"spec":{"suspend":false}}`),
		metav1.PatchOptions{}); err != nil {
		return fail(fmt.Errorf("start job: %w", err))
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

	// Every job object, whatever its installation label: whose it is
	// depends on the database too (see isOurs).
	kjobByID := map[uuid.UUID]*batchv1.Job{}
	for i := range kjobs.Items {
		kj := &kjobs.Items[i]
		if id, err := uuid.Parse(kj.Labels[LabelJobID]); err == nil {
			kjobByID[id] = kj
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
		// In the database, so ours whatever its label.
		m.check(ctx, now, j, kjobByID[j.ID], podsByID[j.ID])
	}

	return m.reapOrphans(ctx, now, kjobByID, isActive)
}

// owner is whose a job object is, by its installation label alone.
type owner int

const (
	// ours: labeled with this task manager's installation (or, without
	// one, unlabeled).
	ours owner = iota
	// unlabeled: from a task manager that didn't label its installation
	// yet.
	unlabeled
	// notOurs: labeled with another installation.
	notOurs
)

// ownership says whose a job object with these labels is, by its label.
func (m *Manager) ownership(labels map[string]string) owner {
	inst, labeled := labels[LabelInstallation]
	switch {
	case labeled && inst == m.Config.Installation:
		return ours
	case labeled:
		return notOurs
	case m.Config.Installation == "":
		return ours
	default:
		return unlabeled
	}
}

// isOurs reports whether a Kubernetes Job is this installation's, and so
// one it may delete: its job is in this installation's database (inDB),
// whatever its label says, or it is labeled with this installation. A
// label alone isn't the whole story: a DispatchOps deleted and created
// again on the same database has a new UID, and an older task manager
// didn't label Jobs. A Job labeled otherwise whose job isn't in the
// database is never ours. The job namespace serves one installation (the
// operator refuses a second), so this is defense in depth.
func (m *Manager) isOurs(labels map[string]string, inDB bool) bool {
	return inDB || m.ownership(labels) == ours
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
			m.end(ctx, j, store.JobCanceled, "Canceled before it started", kj)
		case kj == nil:
			if since(j.DispatchedAt) > cfg.MissingGrace {
				m.end(ctx, j, store.JobError, "The job's Kubernetes Job is missing", nil)
			}
		case kj.Spec.Suspend != nil && *kj.Spec.Suspend:
			// launch stopped before it started the Job (see launch); its
			// Secrets may be missing.
			if since(j.DispatchedAt) > cfg.MissingGrace {
				m.end(ctx, j, store.JobError, "The job's Kubernetes Job was never started", kj)
			}
		case jobDone(kj):
			m.end(ctx, j, store.JobError, "The job pod ended before the runner started"+describePods(pods)+oldRunnerHint(pods), nil)
		case since(j.DispatchedAt) > cfg.ImagePullTimeout && imagePullProblem(pods) != "":
			m.end(ctx, j, store.JobError, fmt.Sprintf("Cannot pull image %s: %s",
				j.Config.ExecutionEnvironment.Image, imagePullProblem(pods)), kj)
		case since(j.DispatchedAt) > cfg.StartTimeout:
			m.end(ctx, j, store.JobError, fmt.Sprintf("The job did not start within %s", cfg.StartTimeout)+describePods(pods), kj)
		}

	case store.JobRunning:
		timeout := time.Duration(j.Config.TimeoutSeconds) * time.Second
		switch {
		case kj == nil:
			m.end(ctx, j, store.JobError, "The job's Kubernetes Job disappeared while it was running", nil)
		case jobDone(kj):
			m.end(ctx, j, store.JobError, "The job pod ended without reporting a result"+describePods(pods), nil)
		case since(j.LastHeartbeatAt) > cfg.HeartbeatTimeout:
			m.end(ctx, j, store.JobError, fmt.Sprintf("The runner stopped responding (no heartbeat for %s)", cfg.HeartbeatTimeout), kj)
		case since(j.StartedAt) > timeout+cfg.TimeoutGrace:
			m.end(ctx, j, store.JobFailed, fmt.Sprintf("Timed out after %s (the runner did not stop the job, so its pod was deleted)", timeout), kj)
		case j.CancelRequested && since(j.CancelRequestedAt) > cfg.CancelGrace:
			m.end(ctx, j, store.JobCanceled, "Canceled (the runner did not stop in time, so its pod was deleted)", kj)
		}
	}
}

// end gives a job a final status, unless its runner got there first, and
// then deletes kj, its Kubernetes Job, unless nil.
func (m *Manager) end(ctx context.Context, j store.Job, status, reason string, kj *batchv1.Job) {
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
	if kj != nil {
		m.deleteK8sJob(ctx, kj, true)
	}
}

// deleteK8sJob deletes a Kubernetes Job, if it is this installation's (see
// isOurs; inDB says its job is in this installation's database), and only
// that very Job (by UID). Background propagation deletes its pods and the
// Secrets it owns too: the task manager can't delete Secrets itself.
func (m *Manager) deleteK8sJob(ctx context.Context, kj *batchv1.Job, inDB bool) {
	if !m.isOurs(kj.Labels, inDB) {
		m.Log.Warn("not deleting another installation's Kubernetes Job", "k8s_job", kj.Name,
			"installation", kj.Labels[LabelInstallation])
		return
	}
	policy := metav1.DeletePropagationBackground
	opts := metav1.DeleteOptions{PropagationPolicy: &policy}
	if kj.UID != "" {
		opts.Preconditions = &metav1.Preconditions{UID: &kj.UID}
	}
	err := m.Kube.BatchV1().Jobs(m.Config.Namespace).Delete(ctx, kj.Name, opts)
	if err != nil && !apierrors.IsNotFound(err) {
		m.Log.Warn("deleting Kubernetes Job", "k8s_job", kj.Name, "err", err)
	}
}

// reapOrphans deletes this installation's Kubernetes Jobs that are still
// running although their dispatch-ops job has finished (or no longer
// exists). One whose job isn't in the database is deleted only if it is
// labeled with this installation (see isOurs).
func (m *Manager) reapOrphans(ctx context.Context, now time.Time, kjobs map[uuid.UUID]*batchv1.Job, isActive map[uuid.UUID]bool) error {
	candidates := m.orphanCandidates(kjobs, isActive)
	if len(candidates) == 0 {
		return nil
	}
	ended, err := m.Store.GetJobEnds(ctx, candidates)
	if err != nil {
		return err
	}
	for _, id := range candidates {
		finishedAt, inDB := ended[id]
		switch {
		case !m.isOurs(kjobs[id].Labels, inDB):
			continue // another installation's
		case !inDB:
		case finishedAt == nil:
			continue // became active after we listed active jobs
		case now.Sub(*finishedAt) < m.Config.OrphanGrace:
			continue
		}
		m.Log.Info("deleting orphaned Kubernetes Job", "job_id", id, "k8s_job", kjobs[id].Name)
		m.deleteK8sJob(ctx, kjobs[id], inDB)
	}
	return nil
}

// orphanCandidates are the Kubernetes Jobs reapOrphans looks up in the
// database: unfinished and not active, whatever their label (see isOurs).
func (m *Manager) orphanCandidates(kjobs map[uuid.UUID]*batchv1.Job, isActive map[uuid.UUID]bool) []uuid.UUID {
	var candidates []uuid.UUID
	for id, kj := range kjobs {
		if !isActive[id] && !jobDone(kj) {
			candidates = append(candidates, id)
		}
	}
	return candidates
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

// oldRunnerHint explains a runner that looked for the token file job pods
// used to mount: its image was built on a runner-base from before the token
// moved to DISPATCH_JOB_TOKEN.
func oldRunnerHint(pods []corev1.Pod) string {
	for _, p := range pods {
		for _, cs := range p.Status.ContainerStatuses {
			if t := cs.State.Terminated; t != nil && strings.Contains(t.Message, "DISPATCH_JOB_TOKEN_FILE") {
				return ". The execution environment's runner is too old for this version of Dispatch Ops: rebuild the image on the current runner-base"
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
