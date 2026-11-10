package operator

import (
	"slices"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/workqueue"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"dispatch-ops/internal/operator/api/v1alpha1"
	"dispatch-ops/internal/taskmanager/joblabels"
)

var t0 = metav1.NewTime(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))

// another is a second DispatchOps.
func another(namespace, name, uid string, created metav1.Time, jobNamespace string) *v1alpha1.DispatchOps {
	return &v1alpha1.DispatchOps{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace, UID: types.UID(uid), CreationTimestamp: created},
		Spec:       v1alpha1.DispatchOpsSpec{TaskManager: v1alpha1.TaskManagerSpec{JobNamespace: jobNamespace}},
	}
}

func (e *env) reconcileNamed(namespace, name string) (ctrl.Result, *v1alpha1.DispatchOps) {
	e.t.Helper()
	key := types.NamespacedName{Namespace: namespace, Name: name}
	res, err := e.r.Reconcile(e.ctx, ctrl.Request{NamespacedName: key})
	if err != nil {
		e.t.Fatalf("reconcile %s: %v", key, err)
	}
	var cr v1alpha1.DispatchOps
	if err := e.c.Get(e.ctx, key, &cr); err != nil {
		e.t.Fatal(err)
	}
	return res, &cr
}

// R2-H2: two DispatchOps in one namespace. A namespace hosts one
// installation: the first to claim it keeps it, and the other is refused
// outright, before anything is made for it.
func TestSecondInstallationInANamespaceIsRefused(t *testing.T) {
	newer := another(ns, "second", "uid-2", metav1.NewTime(t0.Add(time.Minute)), "")
	e := newEnv(t, &v1alpha1.DispatchOps{ObjectMeta: metav1.ObjectMeta{CreationTimestamp: t0}}, newer)
	e.reconcile()

	for range 2 {
		res, cr := e.reconcileNamed(ns, "second")
		e.wantCondition(cr, CondReady, metav1.ConditionFalse, ReasonNamespaceInUse)
		e.wantCondition(cr, CondTaskManagerReady, metav1.ConditionFalse, ReasonNamespaceInUse)
		// The holder is in the same namespace, so it may be named; its UID
		// never is.
		if c := e.condition(cr, CondReady); !strings.Contains(c.Message, "DispatchOps ops/dops") || strings.Contains(c.Message, "uid-1") {
			t.Errorf("refusal = %q, want it to name ops/dops and not its UID", c.Message)
		}
		if res.RequeueAfter == 0 {
			t.Error("a refused installation isn't checked again")
		}
		// Refused outright: nothing is made for it.
		var sts appsv1.StatefulSet
		var role rbacv1.Role
		var key corev1.Secret
		if e.get("second-postgres", &sts) || e.get("second-task-manager", &role) || e.get("second-encryption-key", &key) {
			t.Error("the refused installation got a database, an encryption key or a task manager Role")
		}
		// The first one is unaffected.
		e.deploy()
		var tm appsv1.Deployment
		if !e.get("dops-task-manager", &tm) || !e.get("dops-task-manager", &role) {
			t.Fatal("the installation that holds the namespace has no task manager")
		}
		if owner := e.claimOwner(ns); owner != "uid-1" {
			t.Fatalf("claim on %s held by %q", ns, owner)
		}
	}

	// Deleting the refused one leaves the holder's claim alone.
	if err := e.c.Delete(e.ctx, e.getCR(ns, "second")); err != nil {
		t.Fatal(err)
	}
	if _, err := e.r.Reconcile(e.ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: "second"}}); err != nil {
		t.Fatal(err)
	}
	if owner := e.claimOwner(ns); owner != "uid-1" {
		t.Errorf("after the refused DispatchOps went, the claim is %q's", owner)
	}
}

// jobPod is a pod of a job of the installation uid, in namespace.
func jobPod(namespace, name, uid string, phase corev1.PodPhase) *corev1.Pod {
	l := jobPodLabels()
	if uid != "" {
		l[joblabels.Installation] = uid
	}
	return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace, Labels: l}, Status: corev1.PodStatus{Phase: phase}}
}

// k8sJob is a Kubernetes Job of the installation uid, in namespace.
func k8sJob(namespace, name, uid string) *batchv1.Job {
	l := jobPodLabels()
	if uid != "" {
		l[joblabels.Installation] = uid
	}
	return &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace, Labels: l}}
}

// R2-H2 recheck: the incumbent keeps its job namespace. An older
// DispatchOps that switches its jobNamespace to it is refused and gets no
// Role; once the incumbent is deleted and all of its objects are gone, it
// gets the namespace.
func TestIncumbentKeepsJobNamespace(t *testing.T) {
	// Older than ours, running its jobs in its own namespace for now.
	older := another("team-b", "b", "uid-b", metav1.NewTime(t0.Add(-time.Hour)), "")
	e := newEnv(t, &v1alpha1.DispatchOps{
		ObjectMeta: metav1.ObjectMeta{CreationTimestamp: t0},
		Spec:       v1alpha1.DispatchOpsSpec{TaskManager: v1alpha1.TaskManagerSpec{JobNamespace: "jobs"}},
	}, jobsNamespace(""), older, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "team-b", Labels: restricted()}},
		k8sJob("jobs", "dispatch-job-a", "uid-1"))
	// Any DispatchOps may use "jobs".
	e.r.AllowedJobNamespaces = map[string][]string{"jobs": {ns, "team-b"}}
	e.deploy()
	olderRole := install{DispatchOps: older}.foreignJobObjectName(componentTaskManager)
	var role rbacv1.Role
	var job batchv1.Job
	if !e.exists("jobs", foreignName(componentTaskManager), &role) {
		t.Fatal("no task manager Role in the job namespace")
	}

	// b moves its jobs to "jobs".
	b := &v1alpha1.DispatchOps{}
	if err := e.c.Get(e.ctx, types.NamespacedName{Namespace: "team-b", Name: "b"}, b); err != nil {
		t.Fatal(err)
	}
	b.Spec.TaskManager.JobNamespace = "jobs"
	if err := e.c.Update(e.ctx, b); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		_, cr := e.reconcileNamed("team-b", "b")
		e.wantCondition(cr, CondReady, metav1.ConditionFalse, ReasonJobNamespaceInUse)
		// The incumbent is in another namespace: neither named nor its UID
		// given.
		if c := e.condition(cr, CondReady); strings.Contains(c.Message, "ops/dops") || strings.Contains(c.Message, "uid-1") {
			t.Errorf("refusal = %q", c.Message)
		}
		if e.exists("jobs", olderRole, &role) {
			t.Fatal("the older DispatchOps got a Role in the incumbent's job namespace")
		}
		// The incumbent keeps everything.
		_, cr = e.reconcileNamed(ns, "dops")
		if c := e.condition(cr, CondReady); c.Reason == ReasonJobNamespaceInUse {
			t.Fatalf("the incumbent was refused: %s", c.Message)
		}
		var tm appsv1.Deployment
		if !e.get("dops-task-manager", &tm) || !e.exists("jobs", foreignName(componentTaskManager), &role) ||
			!e.exists("jobs", "dispatch-job-a", &job) {
			t.Fatal("the incumbent lost its task manager, Role or Job")
		}
	}

	// The incumbent goes; while its job pod still runs, b waits.
	pod := jobPod("jobs", "dispatch-job-a-xyz", "uid-1", corev1.PodRunning)
	if err := e.c.Create(e.ctx, pod); err != nil {
		t.Fatal(err)
	}
	if err := e.c.Delete(e.ctx, e.cr()); err != nil {
		t.Fatal(err)
	}
	res, err := e.r.Reconcile(e.ctx, ctrl.Request{NamespacedName: e.key})
	if err != nil || res.RequeueAfter != cleanupPoll {
		t.Errorf("finalize with a job pod running = %+v, %v; want a requeue after %s", res, err, cleanupPoll)
	}
	var np networkingv1.NetworkPolicy
	if e.exists("jobs", "dispatch-job-a", &job) || e.exists("jobs", foreignName(componentTaskManager), &role) {
		t.Error("finalizing kept the incumbent's Job or Role")
	}
	if !e.exists("jobs", foreignName(componentJobs), &np) {
		t.Error("the job NetworkPolicy went while a job pod still ran")
	}
	if e.claimOwner("jobs") != "uid-1" || e.claimOwner(ns) != "uid-1" {
		t.Error("a claim was released while a job pod still ran")
	}
	_, cr := e.reconcileNamed("team-b", "b")
	e.wantCondition(cr, CondReady, metav1.ConditionFalse, ReasonJobNamespaceInUse)

	// Its pod stops (garbage collected with its Job): the incumbent is gone.
	if err := e.c.Delete(e.ctx, pod); err != nil {
		t.Fatal(err)
	}
	if _, err := e.r.Reconcile(e.ctx, ctrl.Request{NamespacedName: e.key}); err != nil {
		t.Fatal(err)
	}
	if e.exists("jobs", foreignName(componentJobs), &np) {
		t.Error("the job NetworkPolicy survived the DispatchOps")
	}
	if err := e.c.Get(e.ctx, e.key, &v1alpha1.DispatchOps{}); err == nil {
		t.Error("the DispatchOps wasn't finalized")
	}
	if owner, own := e.claimOwner("jobs"), e.claimOwner(ns); owner != "" || own != "" {
		t.Errorf("claims left behind: jobs %q, ops %q", owner, own)
	}
	// The incumbent's job namespace isn't handed on: its task manager may
	// have left objects without its labels there. Once an administrator has
	// checked and cleared the tombstone, b gets it.
	e.wantTombstone("jobs", roleJobNamespace, "uid-1")
	_, cr = e.reconcileNamed("team-b", "b")
	e.wantCondition(cr, CondReady, metav1.ConditionFalse, ReasonJobNamespaceReleased)
	if c := e.condition(cr, CondReady); !strings.Contains(c.Message, "delete ConfigMap jobs/dispatch-ops-claim") ||
		strings.Contains(c.Message, "uid-1") {
		t.Errorf("refusal = %q", c.Message)
	}
	e.deleteTombstone("jobs")
	_, cr = e.reconcileNamed("team-b", "b")
	if c := e.condition(cr, CondReady); c.Reason == ReasonJobNamespaceInUse {
		t.Errorf("b is still refused: %s", c.Message)
	}
	if owner := e.claimOwner("jobs"); owner != "uid-b" {
		t.Errorf("claim on jobs held by %q, want uid-b", owner)
	}
}

// What another installation (another operator's, or a deleted
// DispatchOps's leftovers) has in a job namespace keeps it, until it is
// gone.
func TestForeignObjectsHoldTheJobNamespace(t *testing.T) {
	for _, tc := range []struct {
		name string
		obj  client.Object
	}{
		{"job", k8sJob(ns, "dispatch-job-x", "uid-x")},
		{"pod", jobPod(ns, "dispatch-job-x-abc", "uid-x", corev1.PodSucceeded)},
		{"role", &rbacv1.Role{ObjectMeta: metav1.ObjectMeta{Name: "x-task-manager", Namespace: ns, Labels: map[string]string{
			ownerLabel: "uid-x", "app.kubernetes.io/managed-by": ManagedBy, "app.kubernetes.io/component": componentTaskManager,
		}}}},
		{"network policy, by owner reference", &networkingv1.NetworkPolicy{ObjectMeta: metav1.ObjectMeta{
			Name: "x-jobs", Namespace: ns,
			Labels: map[string]string{"app.kubernetes.io/managed-by": ManagedBy, "app.kubernetes.io/component": componentJobs},
			OwnerReferences: []metav1.OwnerReference{{APIVersion: v1alpha1.GroupVersion.String(), Kind: "DispatchOps",
				Name: "x", UID: "uid-x", Controller: new(true)}},
		}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t, &v1alpha1.DispatchOps{}, tc.obj)
			_, cr := e.reconcileNamed(ns, "dops")
			e.wantCondition(cr, CondReady, metav1.ConditionFalse, ReasonJobNamespaceInUse)
			if c := e.condition(cr, CondReady); strings.Contains(c.Message, "uid-x") {
				t.Errorf("refusal = %q gives the other installation's UID", c.Message)
			}
			if err := e.c.Delete(e.ctx, tc.obj); err != nil {
				t.Fatal(err)
			}
			_, cr = e.reconcileNamed(ns, "dops")
			if c := e.condition(cr, CondReady); c.Reason == ReasonJobNamespaceInUse {
				t.Errorf("still refused: %s", c.Message)
			}
		})
	}
	// Unlabeled job objects (from a task manager before installation
	// labels) are no one's, and a DispatchOps's other objects in its own
	// namespace aren't job namespace objects.
	other := another(ns, "other", "uid-2", t0, "jobs")
	otherAPI := &networkingv1.NetworkPolicy{ObjectMeta: metav1.ObjectMeta{Name: "other-api", Namespace: ns,
		Labels: map[string]string{"app.kubernetes.io/managed-by": ManagedBy, "app.kubernetes.io/component": componentAPI},
		OwnerReferences: []metav1.OwnerReference{{APIVersion: v1alpha1.GroupVersion.String(), Kind: "DispatchOps",
			Name: "other", UID: "uid-2", Controller: new(true)}},
	}}
	e := newEnv(t, &v1alpha1.DispatchOps{}, k8sJob(ns, "dispatch-job-old", ""), other, otherAPI)
	if occ, err := e.r.jobNamespaceOccupants(e.ctx, ns, "uid-1"); err != nil || len(occ) != 0 {
		t.Errorf("occupants = %v, %v", occ, err)
	}
}

// Leaving a job namespace deletes the installation's own Jobs there, then
// its Role and RoleBinding, and its NetworkPolicy only once its job pods
// have stopped. Nobody else's are touched.
func TestRevokeDeletesTheInstallationsJobs(t *testing.T) {
	e := newEnv(t, &v1alpha1.DispatchOps{Spec: v1alpha1.DispatchOpsSpec{
		TaskManager: v1alpha1.TaskManagerSpec{JobNamespace: "jobs"},
	}}, jobsNamespace(ns),
		k8sJob("jobs", "dispatch-job-mine", "uid-1"),
		k8sJob("jobs", "dispatch-job-unlabeled", ""),
		k8sJob("elsewhere", "dispatch-job-theirs", "uid-x"),
		jobPod("jobs", "dispatch-job-mine-abc", "uid-1", corev1.PodRunning))
	e.deploy()
	var job batchv1.Job
	if !e.exists("jobs", "dispatch-job-mine", &job) {
		t.Fatal("the installation's own Job was deleted while it holds the namespace")
	}

	e.setNamespaceLabel("someone-else")
	res, err := e.r.Reconcile(e.ctx, ctrl.Request{NamespacedName: e.key})
	if err != nil || res.RequeueAfter != cleanupPoll {
		t.Errorf("reconcile = %+v, %v; want a requeue after %s", res, err, cleanupPoll)
	}
	var role rbacv1.Role
	var binding rbacv1.RoleBinding
	var np networkingv1.NetworkPolicy
	if e.exists("jobs", "dispatch-job-mine", &job) {
		t.Error("the installation's Job survived losing the job namespace")
	}
	if !e.exists("jobs", "dispatch-job-unlabeled", &job) || !e.exists("elsewhere", "dispatch-job-theirs", &job) {
		t.Error("deleted a Job that isn't the installation's")
	}
	if e.exists("jobs", foreignName(componentTaskManager), &role) || e.exists("jobs", foreignName(componentTaskManager), &binding) {
		t.Error("the Role or RoleBinding survived")
	}
	if !e.exists("jobs", foreignName(componentJobs), &np) {
		t.Error("the NetworkPolicy went while a job pod still ran")
	}

	var pod corev1.Pod
	e.exists("jobs", "dispatch-job-mine-abc", &pod)
	pod.Status.Phase = corev1.PodFailed
	if err := e.c.Status().Update(e.ctx, &pod); err != nil {
		t.Fatal(err)
	}
	if _, err := e.r.Reconcile(e.ctx, ctrl.Request{NamespacedName: e.key}); err != nil {
		t.Fatal(err)
	}
	if e.exists("jobs", foreignName(componentJobs), &np) {
		t.Error("the NetworkPolicy survived its job pods")
	}
}

// Changing one DispatchOps's job namespace reconciles every other that
// runs its jobs in the old or the new one, at once.
func TestJobNamespaceChangeRequeuesItsUsers(t *testing.T) {
	e := newEnv(t, &v1alpha1.DispatchOps{Spec: v1alpha1.DispatchOpsSpec{TaskManager: v1alpha1.TaskManagerSpec{JobNamespace: "jobs"}}},
		another("team-b", "b", "uid-b", t0, "team-b"),
		another("team-c", "c", "uid-c", t0, "other"),
		another("team-d", "d", "uid-d", t0, "team-d"))
	q := workqueue.NewTypedRateLimitingQueue(workqueue.DefaultTypedControllerRateLimiter[reconcile.Request]())
	defer q.ShutDown()
	moved := another("team-d", "d", "uid-d", t0, "jobs")
	before := another("team-d", "d", "uid-d", t0, "team-b")
	e.r.enqueueJobNamespaceUsers(e.ctx, q, before, moved)
	var got []string
	for q.Len() > 0 {
		req, _ := q.Get()
		got = append(got, req.String())
		q.Done(req)
	}
	slices.Sort(got)
	if want := []string{"ops/dops", "team-b/b"}; !slices.Equal(got, want) {
		t.Errorf("queued %v, want %v", got, want)
	}
}

func (e *env) getCR(namespace, name string) *v1alpha1.DispatchOps {
	e.t.Helper()
	var cr v1alpha1.DispatchOps
	if err := e.c.Get(e.ctx, types.NamespacedName{Namespace: namespace, Name: name}, &cr); err != nil {
		e.t.Fatal(err)
	}
	return &cr
}

// R2-H2 F5: a DispatchOps whose namespace isn't mapped to a job namespace
// is refused it, however old, and claims nothing there.
func TestUnmappedNamespaceIsRefusedAJobNamespace(t *testing.T) {
	squatter := another("team-c", "c", "uid-c", metav1.NewTime(t0.Add(-2*time.Hour)), "jobs")
	e := newEnv(t, &v1alpha1.DispatchOps{
		ObjectMeta: metav1.ObjectMeta{CreationTimestamp: t0},
		Spec:       v1alpha1.DispatchOpsSpec{TaskManager: v1alpha1.TaskManagerSpec{JobNamespace: "jobs"}},
	}, squatter, jobsNamespace(""), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "team-c", Labels: restricted()}})
	e.r.AllowedJobNamespaces = map[string][]string{"jobs": {ns}}
	cr := e.deployNamed("team-c", "c")
	e.wantCondition(cr, CondTaskManagerReady, metav1.ConditionFalse, ReasonJobNamespaceNotAllowed)
	var role rbacv1.Role
	if e.exists("jobs", install{DispatchOps: squatter}.foreignJobObjectName(componentTaskManager), &role) {
		t.Error("the squatter got a task manager Role")
	}
	if owner := e.claimOwner("jobs"); owner != "" {
		t.Errorf("claim on jobs held by %q", owner)
	}
	// The namespace mapped to it gets it.
	e.deploy()
	if !e.exists("jobs", foreignName(componentTaskManager), &role) || e.claimOwner("jobs") != "uid-1" {
		t.Error("the mapped installation didn't get the job namespace")
	}
}

// R2-M5: the job namespace must enforce Pod Security, unless the operator
// was started otherwise.
func TestJobNamespaceMustEnforcePodSecurity(t *testing.T) {
	for _, tc := range []struct {
		name      string
		label     string
		optOut    bool
		wantAllow bool
	}{
		{name: "unlabeled", wantAllow: false},
		{name: "privileged", label: "privileged", wantAllow: false},
		{name: "baseline", label: "baseline", wantAllow: true},
		{name: "restricted", label: "restricted", wantAllow: true},
		{name: "unlabeled, operator opted out", optOut: true, wantAllow: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			n := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}
			if tc.label != "" {
				n.Labels = map[string]string{podSecurityEnforce: tc.label}
			}
			e := newEnv(t, &v1alpha1.DispatchOps{}, n)
			e.r.AllowJobNamespacesWithoutPodSecurity = tc.optOut
			e.deploy()
			var role rbacv1.Role
			var tm, api appsv1.Deployment
			gotRole, gotTM := e.get("dops-task-manager", &role), e.get("dops-task-manager", &tm)
			if gotRole != tc.wantAllow || gotTM != tc.wantAllow {
				t.Errorf("role %t task manager %t, want %t", gotRole, gotTM, tc.wantAllow)
			}
			if !tc.wantAllow {
				e.wantCondition(e.cr(), CondTaskManagerReady, metav1.ConditionFalse, ReasonJobNamespacePodSecurity)
				if !e.get("dops-api", &api) {
					t.Error("the API server wasn't deployed")
				}
			}
		})
	}

	// Removing the label takes the task manager's access away again.
	e := newEnv(t, &v1alpha1.DispatchOps{})
	e.deploy()
	var n corev1.Namespace
	if err := e.c.Get(e.ctx, types.NamespacedName{Name: ns}, &n); err != nil {
		t.Fatal(err)
	}
	n.Labels = nil
	if err := e.c.Update(e.ctx, &n); err != nil {
		t.Fatal(err)
	}
	e.reconcile()
	var role rbacv1.Role
	var binding rbacv1.RoleBinding
	if e.get("dops-task-manager", &role) || e.get("dops-task-manager", &binding) {
		t.Error("the task manager kept its Role after the namespace stopped enforcing Pod Security")
	}
}

// R2-M4, R2-L10: the task manager knows its installation, has its own
// Lease, and can't read Secrets.
func TestTaskManagerIdentityAndRBAC(t *testing.T) {
	e := newEnv(t, &v1alpha1.DispatchOps{})
	e.deploy()
	var tm appsv1.Deployment
	e.get("dops-task-manager", &tm)
	if v, _ := envValue(tm.Spec.Template.Spec.Containers[0], joblabels.InstallationEnv); v.Value != "uid-1" {
		t.Errorf("%s = %q", joblabels.InstallationEnv, v.Value)
	}
	var role rbacv1.Role
	e.get("dops-task-manager", &role)
	var leaseNames []string
	for _, r := range role.Rules {
		switch r.Resources[0] {
		case "secrets":
			// Not even patch (it returns the Secret) or delete (the
			// encryption key is in the namespace by default).
			if !slices.Equal(r.Verbs, []string{"create"}) {
				t.Errorf("the task manager may %v secrets, want only create", r.Verbs)
			}
		case "jobs":
			// Created suspended, then started.
			if !slices.Contains(r.Verbs, "create") || !slices.Contains(r.Verbs, "patch") {
				t.Errorf("the task manager may %v jobs", r.Verbs)
			}
		case "leases":
			if slices.Contains(r.Verbs, "update") {
				leaseNames = r.ResourceNames
			}
		}
	}
	// Its own only: not the shared name of task managers run outside the
	// operator (joblabels.LeaseName("")), where a Lease planted by another
	// installation's task manager could hold up its leader election.
	if !slices.Equal(leaseNames, []string{joblabels.LeaseName("uid-1")}) {
		t.Errorf("lease update limited to %v", leaseNames)
	}
	other := install{DispatchOps: another(ns, "x", "uid-2", t0, "")}
	if joblabels.LeaseName(other.installation()) == joblabels.LeaseName("uid-1") {
		t.Error("two installations share a Lease name")
	}
}

// The job pods' NetworkPolicy selects every job pod in the job namespace,
// by the labels the task manager gives them (both come from joblabels), not
// only those labeled with this installation: the namespace is its alone.
func TestJobPodSelectorMatchesTaskManagerLabels(t *testing.T) {
	in := install{DispatchOps: another(ns, "dops", "uid-1", t0, "")}
	want := map[string]string{
		joblabels.ManagedBy: joblabels.ManagedByValue, joblabels.Component: joblabels.ComponentValue,
	}
	got := in.jobPodSelector()
	if len(got) != len(want) {
		t.Errorf("selector = %v, want %v", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("selector = %v, want %v", got, want)
		}
	}
	if jobPodIDLabel != joblabels.JobID {
		t.Error("job ID label differs")
	}
}

func TestJobDNSIsConfigurable(t *testing.T) {
	in := install{DispatchOps: (&Reconciler{}).withDefaults(another(ns, "dops", "uid-1", t0, ""))}
	np, err := in.jobNetworkPolicy(&JobDNS{CIDRs: []string{"169.254.20.10/32"}})
	if err != nil {
		t.Fatal(err)
	}
	if to := np.Spec.Egress[1].To; len(to) != 1 || to[0].IPBlock == nil || to[0].IPBlock.CIDR != "169.254.20.10/32" {
		t.Errorf("DNS to a CIDR = %+v", to)
	}
	if ports := np.Spec.Egress[1].Ports; len(ports) != 2 {
		t.Errorf("DNS ports to a CIDR = %+v, want 53 only", ports)
	}
	np, _ = in.jobNetworkPolicy(&JobDNS{})
	if to := np.Spec.Egress[1].To; to != nil {
		t.Errorf("DNS anywhere = %+v", to)
	}
	// A pod selector alone selects the DNS pods in every namespace, not
	// only the job namespace (NetworkPolicy's nil namespaceSelector), and
	// allows their port 5353 too (OpenShift's DNS pods listen there).
	np, _ = in.jobNetworkPolicy(&JobDNS{PodSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"dns": "yes"}}})
	if to := np.Spec.Egress[1].To; len(to) != 1 || to[0].NamespaceSelector == nil ||
		len(to[0].NamespaceSelector.MatchLabels)+len(to[0].NamespaceSelector.MatchExpressions) != 0 {
		t.Errorf("DNS to pods in any namespace = %+v", to)
	}
	var ports []int32
	for _, p := range np.Spec.Egress[1].Ports {
		ports = append(ports, p.Port.IntVal)
	}
	if !slices.Equal(ports, []int32{53, 53, 5353, 5353}) {
		t.Errorf("DNS ports to pods = %v", ports)
	}
	if _, err := in.jobNetworkPolicy(&JobDNS{CIDRs: []string{"nope"}}); err == nil {
		t.Error("a bad DNS CIDR was accepted")
	}
}

// Info: foreign object names are unambiguous and fit in 63 characters.
func TestForeignJobObjectNames(t *testing.T) {
	a := install{DispatchOps: another("a", "b-x", "1", t0, "jobs")}
	b := install{DispatchOps: another("a-b", "x", "2", t0, "jobs")}
	if a.foreignJobObjectName(componentJobs) == b.foreignJobObjectName(componentJobs) {
		t.Error("a/b-x and a-b/x collide")
	}
	if a.legacyForeignJobObjectName(componentJobs) != b.legacyForeignJobObjectName(componentJobs) {
		t.Error("the test doesn't reproduce the old collision")
	}
	long := install{DispatchOps: another("n2345678901234567890123456789012345678901234567890123456789012",
		"m2345678901234567890123456789012345678901234567890123456789012", "3", t0, "jobs")}
	if n := long.foreignJobObjectName(componentTaskManager); len(n) > 63 {
		t.Errorf("%q is %d characters", n, len(n))
	}
}
