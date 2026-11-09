package taskmanager

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"dispatch-ops/internal/auth"
	"dispatch-ops/internal/credentials"
	"dispatch-ops/internal/secrets"
	"dispatch-ops/internal/store"
	"dispatch-ops/internal/testdb"
)

const ns = "jobs"

func testJob() store.Job {
	return store.Job{
		ID:   uuid.New(),
		Name: "nightly",
		Config: store.JobConfig{
			TimeoutSeconds: 600,
			ExecutionEnvironment: store.JobExecutionEnvironment{
				Image: "ghcr.io/acme/ee-shell:1", PullPolicy: "IfNotPresent",
			},
		},
	}
}

func TestBuildJob(t *testing.T) {
	j := testJob()
	kj := buildJob(j, Config{Namespace: ns, RunnerAPIURL: "http://api:8080", JobTTL: time.Hour, StartTimeout: 10 * time.Minute}, true, true)

	if kj.Name != "dispatch-job-"+j.ID.String() || kj.Labels[LabelJobID] != j.ID.String() {
		t.Errorf("metadata = %+v", kj.ObjectMeta)
	}
	if *kj.Spec.BackoffLimit != 0 || *kj.Spec.TTLSecondsAfterFinished != 3600 || *kj.Spec.ActiveDeadlineSeconds != 600+600+300 {
		t.Errorf("job spec = backoff %d ttl %d deadline %d", *kj.Spec.BackoffLimit, *kj.Spec.TTLSecondsAfterFinished, *kj.Spec.ActiveDeadlineSeconds)
	}
	pod := kj.Spec.Template.Spec
	if pod.RestartPolicy != corev1.RestartPolicyNever || *pod.AutomountServiceAccountToken || !*pod.SecurityContext.RunAsNonRoot {
		t.Error("pod is not locked down")
	}
	c := pod.Containers[0]
	if c.Image != "ghcr.io/acme/ee-shell:1" || *c.SecurityContext.AllowPrivilegeEscalation {
		t.Errorf("container = %+v", c)
	}
	env := map[string]string{}
	for _, e := range c.Env {
		env[e.Name] = e.Value
	}
	if env["DISPATCH_API_URL"] != "http://api:8080" || env["DISPATCH_JOB_ID"] != j.ID.String() || env["DISPATCH_JOB_TOKEN_FILE"] != tokenFile ||
		env["DISPATCH_API_CA_FILE"] != apiCAFile {
		t.Errorf("env = %v", env)
	}
	for _, e := range buildJob(j, Config{}, false, false).Spec.Template.Spec.Containers[0].Env {
		if e.Name == "DISPATCH_API_CA_FILE" {
			t.Error("DISPATCH_API_CA_FILE set without a CA")
		}
	}
	if len(pod.ImagePullSecrets) != 1 || pod.ImagePullSecrets[0].Name != pullSecretName(j.ID.String()) {
		t.Errorf("imagePullSecrets = %v", pod.ImagePullSecrets)
	}
	if len(c.Resources.Limits) != 0 {
		t.Errorf("resources without config = %+v", c.Resources)
	}
	for _, v := range pod.Volumes {
		if v.Name == "work" && v.EmptyDir.SizeLimit != nil {
			t.Errorf("work volume limited without config: %v", v.EmptyDir.SizeLimit)
		}
	}
	limited := buildJob(j, Config{
		JobResources: corev1.ResourceRequirements{
			Requests: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("256Mi")},
			Limits:   corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("2Gi")},
		},
		JobWorkSizeLimit: resource.MustParse("4Gi"),
	}, false, false).Spec.Template.Spec
	if r := limited.Containers[0].Resources; r.Limits.Memory().String() != "2Gi" || r.Requests.Memory().String() != "256Mi" {
		t.Errorf("job resources = %+v", r)
	}
	for _, v := range limited.Volumes {
		if v.Name == "work" && (v.EmptyDir.SizeLimit == nil || v.EmptyDir.SizeLimit.String() != "4Gi") {
			t.Errorf("work volume size limit = %v", v.EmptyDir.SizeLimit)
		}
	}
	for _, v := range pod.Volumes {
		if v.Name == "secrets" && v.EmptyDir.Medium != corev1.StorageMediumMemory {
			t.Error("credentials volume is not in memory")
		}
	}
}

func TestBuildPullSecret(t *testing.T) {
	s, err := buildPullSecret(testJob(), ns, map[string]string{"server": "ghcr.io", "username": "bot", "password": "pw"})
	if err != nil {
		t.Fatal(err)
	}
	var cfg struct {
		Auths map[string]struct{ Username, Password, Auth string }
	}
	if err := json.Unmarshal(s.Data[corev1.DockerConfigJsonKey], &cfg); err != nil {
		t.Fatal(err)
	}
	if a := cfg.Auths["ghcr.io"]; a.Username != "bot" || a.Password != "pw" || a.Auth != "Ym90OnB3" {
		t.Errorf("auths = %+v", cfg.Auths)
	}
}

// --- Database tests ---

type env struct {
	t    *testing.T
	ctx  context.Context
	st   *store.Store
	kube *fake.Clientset
	m    *Manager
	ee   store.ExecutionEnvironment
	pool *pgxpool.Pool
}

func newEnv(t *testing.T) *env {
	pool, _ := testdb.Open(t, "taskmanager")
	ctx := context.Background()
	// This database belongs to this package; finish anything earlier runs left.
	for _, sql := range []string{
		`UPDATE jobs SET status = 'canceled', finished_at = now() WHERE status IN ('pending', 'waiting', 'running')`,
		`DELETE FROM schedules`,
		`UPDATE notification_deliveries SET status = 'failed' WHERE status = 'pending'`,
	} {
		if _, err := pool.Exec(ctx, sql); err != nil {
			t.Fatal(err)
		}
	}
	st := store.New(pool)
	keys, _ := secrets.NewKeyring(secrets.GenerateKey())
	kube := fake.NewClientset()
	e := &env{t: t, ctx: ctx, st: st, kube: kube, pool: pool}
	e.m = &Manager{
		Store: st, Kube: kube, Vault: credentials.Vault{Keys: keys},
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Config: Config{
			Namespace: ns, RunnerAPIURL: "http://api", MaxConcurrentJobs: 2, JobTTL: time.Hour, StartTimeout: time.Hour,
		},
	}
	var err error
	e.ee, err = st.CreateExecutionEnvironment(ctx, store.ExecutionEnvironmentFields{
		Name: "ee-" + uuid.NewString()[:8], Image: "local/ee:1", PullPolicy: "IfNotPresent",
	})
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func (e *env) launch(timeout int) store.Job {
	e.t.Helper()
	j, err := e.st.CreateJob(e.ctx, store.NewJob{Name: "test", LaunchType: "api", Config: store.JobConfig{
		JobType: "container", Command: []string{"true"}, TimeoutSeconds: timeout,
		ExecutionEnvironment: store.JobExecutionEnvironment{ID: e.ee.ID, Name: e.ee.Name, Image: e.ee.Image, PullPolicy: e.ee.PullPolicy},
	}})
	if err != nil {
		e.t.Fatal(err)
	}
	return j
}

func (e *env) job(id uuid.UUID) store.Job {
	e.t.Helper()
	j, err := e.st.GetJob(e.ctx, id)
	if err != nil {
		e.t.Fatal(err)
	}
	return j
}

func (e *env) k8sJob(id uuid.UUID) *batchv1.Job {
	kj, err := e.kube.BatchV1().Jobs(ns).Get(e.ctx, k8sJobName(id.String()), metav1.GetOptions{})
	if err != nil {
		return nil
	}
	return kj
}

// addPod adds a pod for the job, as the Job controller would.
func (e *env) addPod(id uuid.UUID, status corev1.PodStatus) {
	e.t.Helper()
	labels := labelsFor(store.Job{ID: id})
	_, err := e.kube.CoreV1().Pods(ns).Create(e.ctx, &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: k8sJobName(id.String()) + "-abcde", Namespace: ns, Labels: labels},
		Status:     status,
	}, metav1.CreateOptions{})
	if err != nil {
		e.t.Fatal(err)
	}
}

// setJobCondition marks the Kubernetes Job complete or failed.
func (e *env) setJobCondition(id uuid.UUID, typ batchv1.JobConditionType) {
	e.t.Helper()
	kj := e.k8sJob(id)
	kj.Status.Conditions = append(kj.Status.Conditions, batchv1.JobCondition{Type: typ, Status: corev1.ConditionTrue})
	if _, err := e.kube.BatchV1().Jobs(ns).UpdateStatus(e.ctx, kj, metav1.UpdateOptions{}); err != nil {
		e.t.Fatal(err)
	}
}

func TestDispatchRespectsConcurrencyAndCreatesObjects(t *testing.T) {
	e := newEnv(t)
	a, b, c := e.launch(60), e.launch(60), e.launch(60)
	e.m.Tick(e.ctx)

	// Two slots: the oldest two are dispatched.
	if e.job(a.ID).Status != store.JobWaiting || e.job(b.ID).Status != store.JobWaiting || e.job(c.ID).Status != store.JobPending {
		t.Fatalf("statuses = %s %s %s", e.job(a.ID).Status, e.job(b.ID).Status, e.job(c.ID).Status)
	}
	kj := e.k8sJob(a.ID)
	if kj == nil {
		t.Fatal("no Kubernetes Job created")
	}
	tok, err := e.kube.CoreV1().Secrets(ns).Get(e.ctx, tokenSecretName(a.ID.String()), metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(tok.Data["token"]), "doj_") {
		t.Errorf("token secret = %q", tok.Data["token"])
	}
	if len(tok.OwnerReferences) != 1 || tok.OwnerReferences[0].Name != kj.Name {
		t.Errorf("token secret owner = %v", tok.OwnerReferences)
	}

	// The stored hash matches the token in the Secret.
	if id, err := e.st.LookupJobToken(e.ctx, auth.HashToken(string(tok.Data["token"]))); err != nil || id != a.ID {
		t.Errorf("LookupJobToken = %v, %v", id, err)
	}

	// When a job finishes, the next one is dispatched.
	if err := e.st.EndJob(e.ctx, a.ID, store.JobSuccessful, nil, "", store.JobWaiting); err != nil {
		t.Fatal(err)
	}
	e.m.Tick(e.ctx)
	if e.job(c.ID).Status != store.JobWaiting {
		t.Errorf("third job = %s", e.job(c.ID).Status)
	}
}

func TestAPICertificateReachesJobPods(t *testing.T) {
	e := newEnv(t)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "tls.crt"), []byte("LEAF"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "ca.crt"), []byte("CA"), 0o600); err != nil {
		t.Fatal(err)
	}
	e.m.Config.RunnerAPITLSDir = dir
	j := e.launch(60)
	e.m.Tick(e.ctx)
	tok, err := e.kube.CoreV1().Secrets(ns).Get(e.ctx, tokenSecretName(j.ID.String()), metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if string(tok.Data["ca.crt"]) != "CA" {
		t.Errorf("token secret ca.crt = %q", tok.Data["ca.crt"])
	}
	found := false
	for _, v := range e.k8sJob(j.ID).Spec.Template.Spec.Containers[0].Env {
		found = found || (v.Name == "DISPATCH_API_CA_FILE" && v.Value == apiCAFile)
	}
	if !found {
		t.Error("job pod has no DISPATCH_API_CA_FILE")
	}

	// Without a readable certificate, the job fails rather than running a
	// runner that can't reach the API.
	e.m.Config.RunnerAPITLSDir = filepath.Join(dir, "missing")
	j2 := e.launch(60)
	e.m.Tick(e.ctx)
	if got := e.job(j2.ID).Status; got != store.JobError {
		t.Errorf("job without the API certificate = %s, want error", got)
	}
}

func TestRegistryCredentialBecomesPullSecret(t *testing.T) {
	e := newEnv(t)
	cred := store.Credential{ID: uuid.New(), Name: "reg-" + uuid.NewString()[:8], Kind: "registry"}
	if err := e.m.Vault.Seal(&cred, map[string]string{"server": "ghcr.io", "username": "u", "password": "p"}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.st.CreateCredential(e.ctx, cred); err != nil {
		t.Fatal(err)
	}
	j2, err := e.st.CreateJob(e.ctx, store.NewJob{Name: "test", LaunchType: "api", Config: store.JobConfig{
		JobType: "container", Command: []string{"true"}, TimeoutSeconds: 60,
		ExecutionEnvironment: store.JobExecutionEnvironment{
			ID: e.ee.ID, Image: e.ee.Image, PullPolicy: "Always", RegistryCredentialID: &cred.ID,
		},
	}})
	if err != nil {
		t.Fatal(err)
	}
	e.m.Tick(e.ctx)
	kj := e.k8sJob(j2.ID)
	if kj == nil || len(kj.Spec.Template.Spec.ImagePullSecrets) != 1 {
		t.Fatalf("job = %v", kj)
	}
	pull, err := e.kube.CoreV1().Secrets(ns).Get(e.ctx, pullSecretName(j2.ID.String()), metav1.GetOptions{})
	if err != nil || pull.Type != corev1.SecretTypeDockerConfigJson {
		t.Fatalf("pull secret = %v, %v", pull, err)
	}
}

func TestWaitingJobFailures(t *testing.T) {
	e := newEnv(t)
	e.m.Config.MaxConcurrentJobs = 10
	e.m.Config.ImagePullTimeout = time.Nanosecond

	pull, canceled, crashed := e.launch(60), e.launch(60), e.launch(60)
	e.m.Tick(e.ctx)

	e.addPod(pull.ID, corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{
		State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ImagePullBackOff", Message: "not found"}},
	}}})
	if _, err := e.st.CancelJob(e.ctx, canceled.ID); err != nil {
		t.Fatal(err)
	}
	e.setJobCondition(crashed.ID, batchv1.JobFailed)
	e.addPod(crashed.ID, corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{
		State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{Reason: "Error", ExitCode: 127, Message: "exec: dispatch-runner: not found"}},
	}}})
	e.m.Tick(e.ctx)

	for _, tc := range []struct {
		job     store.Job
		status  string
		reason  string
		k8sGone bool
	}{
		{pull, store.JobError, "Cannot pull image local/ee:1: ImagePullBackOff: not found", true},
		{canceled, store.JobCanceled, "Canceled before it started", true},
		{crashed, store.JobError, "exit code 127: exec: dispatch-runner: not found", false},
	} {
		j := e.job(tc.job.ID)
		if j.Status != tc.status || !strings.Contains(j.FailureReason, tc.reason) {
			t.Errorf("job: %s %q, want %s %q", j.Status, j.FailureReason, tc.status, tc.reason)
		}
		if gone := e.k8sJob(tc.job.ID) == nil; gone != tc.k8sGone {
			t.Errorf("%s: Kubernetes Job deleted = %v, want %v", tc.reason, gone, tc.k8sGone)
		}
	}
}

func TestRunningJobFailures(t *testing.T) {
	e := newEnv(t)
	e.m.Config.MaxConcurrentJobs = 10
	silent, oom, slow, stubborn := e.launch(60), e.launch(60), e.launch(1), e.launch(60)
	e.m.Tick(e.ctx)
	for _, j := range []store.Job{silent, oom, slow, stubborn} {
		if _, err := e.st.StartJob(e.ctx, j.ID, "test"); err != nil {
			t.Fatal(err)
		}
	}
	e.setJobCondition(oom.ID, batchv1.JobFailed)
	e.addPod(oom.ID, corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{
		State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{Reason: "OOMKilled", ExitCode: 137}},
	}}})
	if _, err := e.st.CancelJob(e.ctx, stubborn.ID); err != nil {
		t.Fatal(err)
	}

	// Nothing has timed out yet.
	e.m.Tick(e.ctx)
	if j := e.job(silent.ID); j.Status != store.JobRunning {
		t.Fatalf("silent job ended early: %s %s", j.Status, j.FailureReason)
	}
	if j := e.job(oom.ID); j.Status != store.JobError || !strings.Contains(j.FailureReason, "OOMKilled, exit code 137") {
		t.Errorf("oom job = %s %q", j.Status, j.FailureReason)
	}

	// Shorten the deadlines so the rest trip. The silent job's heartbeat is
	// the only one that's checked first, so keep the others beating.
	time.Sleep(1100 * time.Millisecond)
	e.m.Config.CancelGrace = time.Nanosecond
	e.m.Config.TimeoutGrace = time.Nanosecond
	for _, j := range []store.Job{slow, stubborn} {
		if _, err := e.st.Heartbeat(e.ctx, j.ID); err != nil {
			t.Fatal(err)
		}
	}
	e.m.Config.HeartbeatTimeout = time.Second
	e.m.Tick(e.ctx)

	for _, tc := range []struct {
		job    store.Job
		status string
		reason string
	}{
		{silent, store.JobError, "no heartbeat"},
		{slow, store.JobFailed, "Timed out after 1s"},
		{stubborn, store.JobCanceled, "did not stop in time"},
	} {
		j := e.job(tc.job.ID)
		if j.Status != tc.status || !strings.Contains(j.FailureReason, tc.reason) {
			t.Errorf("job: %s %q, want %s %q", j.Status, j.FailureReason, tc.status, tc.reason)
		}
		if e.k8sJob(tc.job.ID) != nil {
			t.Errorf("%s: Kubernetes Job not deleted", tc.reason)
		}
	}

	// The runner's own report wins over the task manager's.
	j := e.launch(60)
	e.m.Tick(e.ctx)
	if _, err := e.st.StartJob(e.ctx, j.ID, "test"); err != nil {
		t.Fatal(err)
	}
	if err := e.st.EndJob(e.ctx, j.ID, store.JobSuccessful, new(0), "", store.JobRunning); err != nil {
		t.Fatal(err)
	}
	e.setJobCondition(j.ID, batchv1.JobComplete)
	e.m.Tick(e.ctx)
	if got := e.job(j.ID); got.Status != store.JobSuccessful {
		t.Errorf("finished job = %s %q", got.Status, got.FailureReason)
	}
}

func TestOrphanedKubernetesJobsAreDeleted(t *testing.T) {
	e := newEnv(t)
	e.m.Config.OrphanGrace = time.Nanosecond
	j := e.launch(60)
	e.m.Tick(e.ctx)
	if err := e.st.EndJob(e.ctx, j.ID, store.JobSuccessful, nil, "", store.JobWaiting); err != nil {
		t.Fatal(err)
	}
	// A Kubernetes Job for a job the database doesn't know.
	stray := buildJob(store.Job{ID: uuid.New(), Config: j.Config}, e.m.Config, false, false)
	if _, err := e.kube.BatchV1().Jobs(ns).Create(e.ctx, stray, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	e.m.Tick(e.ctx)
	if e.k8sJob(j.ID) != nil {
		t.Error("Kubernetes Job of a finished job was not deleted")
	}
	if _, err := e.kube.BatchV1().Jobs(ns).Get(e.ctx, stray.Name, metav1.GetOptions{}); err == nil {
		t.Error("stray Kubernetes Job was not deleted")
	}
}
