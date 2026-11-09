package taskmanager

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"dispatch-ops/internal/store"
	"dispatch-ops/internal/taskmanager/joblabels"
)

// R2-M4: every object a job gets carries its installation.
func TestJobObjectsAreLabeledWithTheInstallation(t *testing.T) {
	cfg := Config{Namespace: ns, Installation: "uid-a"}
	j := testJob()
	kj := buildJob(j, cfg, true, true)
	pull, err := buildPullSecret(j, cfg, map[string]string{"server": "ghcr.io"})
	if err != nil {
		t.Fatal(err)
	}
	for what, labels := range map[string]map[string]string{
		"job":          kj.Labels,
		"pod":          kj.Spec.Template.Labels,
		"token secret": buildTokenSecret(j, cfg, "tok", nil).Labels,
		"pull secret":  pull.Labels,
	} {
		// The operator's job NetworkPolicy selects these (joblabels).
		if labels[LabelInstallation] != "uid-a" || labels[LabelJobID] != j.ID.String() ||
			labels[joblabels.ManagedBy] != joblabels.ManagedByValue || labels[joblabels.Component] != joblabels.ComponentValue {
			t.Errorf("%s labels = %v", what, labels)
		}
	}
	// Without an installation (outside the operator), none.
	if _, ok := buildJob(j, Config{Namespace: ns}, false, false).Spec.Template.Labels[LabelInstallation]; ok {
		t.Error("installation label without an installation")
	}
}

// R2 Low: every unfinished, inactive Job is a candidate whatever its label;
// the database decides (see TestReapOrphansLeavesOtherInstallationsAlone).
func TestOrphanCandidatesAndOwnership(t *testing.T) {
	kjob := func(labels map[string]string) *batchv1.Job {
		return &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Labels: labels}}
	}
	mine, other, legacy, done, active := uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New()
	finished := kjob(map[string]string{LabelInstallation: "uid-a"})
	finished.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobComplete, Status: corev1.ConditionTrue}}
	kjobs := map[uuid.UUID]*batchv1.Job{
		mine:   kjob(map[string]string{LabelInstallation: "uid-a"}),
		other:  kjob(map[string]string{LabelInstallation: "uid-b"}),
		legacy: kjob(map[string]string{}),
		done:   finished,
		active: kjob(map[string]string{LabelInstallation: "uid-a"}),
	}
	m := &Manager{Config: Config{Installation: "uid-a"}}
	got := m.orphanCandidates(kjobs, map[uuid.UUID]bool{active: true})
	slices.SortFunc(got, func(a, b uuid.UUID) int { return slices.Compare(a[:], b[:]) })
	want := []uuid.UUID{mine, other, legacy}
	slices.SortFunc(want, func(a, b uuid.UUID) int { return slices.Compare(a[:], b[:]) })
	if !slices.Equal(got, want) {
		t.Errorf("candidates = %v, want %v", got, want)
	}
	// Ours by label, or by being in the database whatever the label.
	for _, tc := range []struct {
		id   uuid.UUID
		inDB bool
		want bool
	}{
		{mine, false, true}, {other, false, false}, {legacy, false, false},
		{other, true, true}, {legacy, true, true},
	} {
		if got := m.isOurs(kjobs[tc.id].Labels, tc.inDB); got != tc.want {
			t.Errorf("isOurs(%v, inDB %t) = %t", kjobs[tc.id].Labels, tc.inDB, got)
		}
	}
	// A task manager without an installation owns only unlabeled objects.
	bare := &Manager{}
	if !bare.isOurs(nil, false) || bare.isOurs(kjobs[mine].Labels, false) {
		t.Error("ownership without an installation misclassified")
	}
}

// deleteK8sJob never deletes a Job labeled for another installation whose
// job isn't in the database.
func TestDeleteK8sJobChecksOwnership(t *testing.T) {
	kube := fake.NewClientset()
	m := &Manager{Kube: kube, Config: Config{Namespace: ns, Installation: "uid-a"}, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	ctx := context.Background()
	kj, err := kube.BatchV1().Jobs(ns).Create(ctx, buildJob(testJob(), Config{Namespace: ns, Installation: "uid-b"}, false, false), metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	m.deleteK8sJob(ctx, kj, false)
	if _, err := kube.BatchV1().Jobs(ns).Get(ctx, kj.Name, metav1.GetOptions{}); err != nil {
		t.Fatalf("another installation's Job was deleted: %v", err)
	}
	m.deleteK8sJob(ctx, kj, true)
	if _, err := kube.BatchV1().Jobs(ns).Get(ctx, kj.Name, metav1.GetOptions{}); err == nil {
		t.Error("a Job whose job is in the database wasn't deleted")
	}
}

func TestLeaseNameIsPerInstallation(t *testing.T) {
	a, b := joblabels.LeaseName("uid-a"), joblabels.LeaseName("uid-b")
	if a == b || a == joblabels.LeaseName("") || len(a) > 63 {
		t.Errorf("lease names %q, %q", a, b)
	}
	if joblabels.LeaseName("") != "dispatch-ops-task-manager" {
		t.Errorf("lease name without an installation = %q", joblabels.LeaseName(""))
	}
	if a != joblabels.LeaseName("uid-a") {
		t.Error("lease name isn't stable")
	}
}

// Another installation's (and unknown unlabeled) Kubernetes Jobs survive
// reaping; this installation's orphans don't.
func TestReapOrphansLeavesOtherInstallationsAlone(t *testing.T) {
	e := newEnv(t)
	e.m.Config.Installation = "uid-a"
	e.m.Config.OrphanGrace = time.Nanosecond
	j := e.launch(60)
	e.m.Tick(e.ctx)
	if got := e.k8sJob(j.ID).Labels[LabelInstallation]; got != "uid-a" {
		t.Fatalf("installation label = %q", got)
	}
	if err := e.st.EndJob(e.ctx, j.ID, store.JobSuccessful, nil, "", store.JobWaiting); err != nil {
		t.Fatal(err)
	}
	strays := map[string]Config{
		"another installation's": {Namespace: ns, Installation: "uid-b"},
		"an unlabeled one":       {Namespace: ns},
	}
	names := map[string]string{}
	for what, cfg := range strays {
		stray := buildJob(store.Job{ID: uuid.New(), Config: j.Config}, cfg, false, false)
		if _, err := e.kube.BatchV1().Jobs(ns).Create(e.ctx, stray, metav1.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
		names[what] = stray.Name
	}
	e.m.Tick(e.ctx)
	if e.k8sJob(j.ID) != nil {
		t.Error("this installation's orphan was not deleted")
	}
	for what, name := range names {
		if _, err := e.kube.BatchV1().Jobs(ns).Get(e.ctx, name, metav1.GetOptions{}); err != nil {
			t.Errorf("%s Kubernetes Job was deleted: %v", what, err)
		}
	}
}

// R2 Low: a DispatchOps created again (a new UID) on the same database
// keeps its in-flight jobs: their Kubernetes Jobs are its own because the
// jobs are in its database, whatever their label.
func TestRecreatedInstallationKeepsItsJobs(t *testing.T) {
	e := newEnv(t)
	e.m.Config.Installation = "uid-old"
	e.m.Config.MissingGrace = time.Nanosecond
	e.m.Config.OrphanGrace = time.Nanosecond
	waiting, finishing := e.launch(60), e.launch(60)
	e.m.Tick(e.ctx)

	e.m.Config.Installation = "uid-new"
	time.Sleep(time.Millisecond)
	e.m.Tick(e.ctx)
	for _, j := range []store.Job{waiting, finishing} {
		if got := e.job(j.ID); got.Status != store.JobWaiting {
			t.Errorf("in-flight job = %s %q, want still waiting", got.Status, got.FailureReason)
		}
		if e.k8sJob(j.ID) == nil {
			t.Error("its Kubernetes Job was deleted")
		}
	}
	// Once finished, its Job is reaped like any other of ours.
	if err := e.st.EndJob(e.ctx, finishing.ID, store.JobSuccessful, nil, "", store.JobWaiting); err != nil {
		t.Fatal(err)
	}
	time.Sleep(time.Millisecond)
	e.m.Tick(e.ctx)
	if e.k8sJob(finishing.ID) != nil {
		t.Error("the finished job's Kubernetes Job, labeled with the old UID, was not reaped")
	}
	if e.k8sJob(waiting.ID) == nil {
		t.Error("the waiting job's Kubernetes Job was deleted")
	}
}

// R2 Low: the task manager only creates Secrets. It creates the Job
// suspended, then its Secrets owned by that Job, then starts the Job.
func TestLaunchCreatesSuspendedJobThenOwnedSecrets(t *testing.T) {
	e := newEnv(t)
	cred := store.Credential{ID: uuid.New(), Name: "reg-" + uuid.NewString()[:8], Kind: "registry"}
	if err := e.m.Vault.Seal(&cred, map[string]string{"server": "ghcr.io", "username": "u", "password": "p"}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.st.CreateCredential(e.ctx, cred); err != nil {
		t.Fatal(err)
	}
	j, err := e.st.CreateJob(e.ctx, store.NewJob{Name: "test", LaunchType: "api", Config: store.JobConfig{
		JobType: "container", Command: []string{"true"}, TimeoutSeconds: 60,
		ExecutionEnvironment: store.JobExecutionEnvironment{ID: e.ee.ID, Image: e.ee.Image, PullPolicy: "Always", RegistryCredentialID: &cred.ID},
	}})
	if err != nil {
		t.Fatal(err)
	}
	e.kube.ClearActions()
	e.m.Tick(e.ctx)

	var got []string
	for _, a := range e.kube.Actions() {
		res := a.GetResource().Resource
		if res != "jobs" && res != "secrets" || a.GetVerb() == "list" {
			continue
		}
		step := a.GetVerb() + " " + res
		switch a := a.(type) {
		case k8stesting.CreateAction:
			switch o := a.GetObject().(type) {
			case *batchv1.Job:
				step += fmt.Sprintf(" suspend=%t", o.Spec.Suspend != nil && *o.Spec.Suspend)
			case *corev1.Secret:
				if len(o.OwnerReferences) != 1 || o.OwnerReferences[0].Kind != "Job" || o.OwnerReferences[0].Name != k8sJobName(j.ID.String()) {
					t.Errorf("secret %s owners = %+v", o.Name, o.OwnerReferences)
				}
			}
		case k8stesting.PatchAction:
			step += " " + string(a.GetPatch())
		}
		got = append(got, step)
	}
	want := []string{"create jobs suspend=true", "create secrets", "create secrets", `patch jobs {"spec":{"suspend":false}}`}
	if !slices.Equal(got, want) {
		t.Errorf("actions = %q, want %q", got, want)
	}
	if kj := e.k8sJob(j.ID); kj == nil || kj.Spec.Suspend == nil || *kj.Spec.Suspend {
		t.Errorf("the Job wasn't started: %+v", kj)
	}
}

// If a Secret can't be created or the Job can't be started, the Job is
// deleted, which garbage collects the Secrets already made.
func TestLaunchFailureDeletesTheJob(t *testing.T) {
	for _, fail := range []struct{ verb, resource string }{{"create", "secrets"}, {"patch", "jobs"}} {
		t.Run(fail.verb+" "+fail.resource, func(t *testing.T) {
			e := newEnv(t)
			e.kube.PrependReactor(fail.verb, fail.resource, func(k8stesting.Action) (bool, runtime.Object, error) {
				return true, nil, errors.New("boom")
			})
			j := e.launch(60)
			e.kube.ClearActions()
			e.m.Tick(e.ctx)
			if got := e.job(j.ID); got.Status != store.JobError || !strings.Contains(got.FailureReason, "boom") {
				t.Errorf("job = %s %q", got.Status, got.FailureReason)
			}
			if e.k8sJob(j.ID) != nil {
				t.Error("the Kubernetes Job survived")
			}
			deleted := false
			for _, a := range e.kube.Actions() {
				if d, ok := a.(k8stesting.DeleteAction); ok && d.GetResource().Resource == "jobs" {
					p := d.GetDeleteOptions().PropagationPolicy
					deleted = p != nil && *p == metav1.DeletePropagationBackground
				}
				if a.GetResource().Resource == "secrets" && a.GetVerb() != "create" {
					t.Errorf("the task manager did %s on secrets", a.GetVerb())
				}
			}
			if !deleted {
				t.Error("the Job wasn't deleted in the background (so its Secrets would stay)")
			}
		})
	}
}

// A Job left suspended (the task manager stopped mid-launch) fails the job
// and is deleted, with its Secrets.
func TestJobLeftSuspendedFails(t *testing.T) {
	e := newEnv(t)
	e.m.Config.MissingGrace = time.Nanosecond
	j := e.launch(60)
	e.m.Tick(e.ctx)
	kj := e.k8sJob(j.ID)
	kj.Spec.Suspend = new(true)
	if _, err := e.kube.BatchV1().Jobs(ns).Update(e.ctx, kj, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(time.Millisecond)
	e.m.Tick(e.ctx)
	if got := e.job(j.ID); got.Status != store.JobError || !strings.Contains(got.FailureReason, "never started") {
		t.Errorf("job = %s %q", got.Status, got.FailureReason)
	}
	if e.k8sJob(j.ID) != nil {
		t.Error("the suspended Job wasn't deleted")
	}
}
