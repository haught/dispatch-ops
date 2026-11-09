package operator

import (
	"context"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/workqueue"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"dispatch-ops/internal/operator/api/v1alpha1"
)

// claimOwner is the UID holding the claim on namespace, or "" if there is
// none.
func (e *env) claimOwner(namespace string) types.UID {
	e.t.Helper()
	var cm corev1.ConfigMap
	if !e.exists(namespace, claimName, &cm) {
		return ""
	}
	return claimOwner(&cm)
}

// deployNamed is deploy for another DispatchOps than the env's.
func (e *env) deployNamed(namespace, name string) *v1alpha1.DispatchOps {
	e.t.Helper()
	e.reconcileNamed(namespace, name)
	var sts appsv1.StatefulSet
	if e.exists(namespace, name+"-postgres", &sts) {
		sts.Status.ReadyReplicas = 1
		if err := e.c.Status().Update(e.ctx, &sts); err != nil {
			e.t.Fatal(err)
		}
	}
	_, cr := e.reconcileNamed(namespace, name)
	var job batchv1.Job
	if e.exists(namespace, install{DispatchOps: cr}.migrationJobName("dispatch-ops:1.0"), &job) {
		job.Status.Conditions = append(job.Status.Conditions, batchv1.JobCondition{Type: batchv1.JobComplete, Status: corev1.ConditionTrue})
		if err := e.c.Status().Update(e.ctx, &job); err != nil {
			e.t.Fatal(err)
		}
	}
	_, cr = e.reconcileNamed(namespace, name)
	return cr
}

// noOtherInstallation fails if cr's status gives away anything of the
// installation uid in namespace/name: its UID, or its name, unless it is
// in cr's namespace.
func (e *env) noOtherInstallation(cr *v1alpha1.DispatchOps, uid, namespace, name string) {
	e.t.Helper()
	for _, c := range cr.Status.Conditions {
		if strings.Contains(c.Message, uid) || (namespace != cr.Namespace && strings.Contains(c.Message, namespace+"/"+name)) {
			e.t.Errorf("%s condition of %s/%s gives away the other installation: %q", c.Type, cr.Namespace, cr.Name, c.Message)
		}
	}
}

// R2-H2 F1: a namespace that hosts another installation's control plane (its
// DispatchOps, encryption key and database Secrets) is never a job
// namespace, whether that installation runs its jobs there or not, has
// migrated or not, or has been reconciled at all yet; even when the
// administrator allowed it as one, and the attacker's DispatchOps is older.
func TestControlPlaneNamespaceIsNoJobNamespace(t *testing.T) {
	for _, tc := range []struct {
		name   string
		victim func(e *env)
		owner  types.UID
	}{
		{"migrated, jobs elsewhere", func(e *env) { e.deployNamed("team-v", "v") }, "uid-v"},
		{"not migrated", func(e *env) { e.reconcileNamed("team-v", "v") }, "uid-v"},
		{"not reconciled yet", func(*env) {}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			victim := another("team-v", "v", "uid-v", t0, "jobs-v")
			e := newEnv(t, &v1alpha1.DispatchOps{
				ObjectMeta: metav1.ObjectMeta{CreationTimestamp: metav1.NewTime(t0.Add(-time.Hour))},
				Spec:       v1alpha1.DispatchOpsSpec{TaskManager: v1alpha1.TaskManagerSpec{JobNamespace: "team-v"}},
			}, victim,
				&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "team-v", Labels: restricted()}},
				&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "jobs-v", Labels: restricted()}})
			// The administrator's mistake: the victim's namespace allowed as
			// the attacker's job namespace.
			e.r.AllowedJobNamespaces = map[string][]string{"team-v": {ns}, "jobs-v": {"team-v"}}
			tc.victim(e)

			// Refused outright: no database, so nothing to deploy.
			e.reconcile()
			cr := e.reconcile()
			e.wantCondition(cr, CondReady, metav1.ConditionFalse, ReasonJobNamespaceInUse)
			var sts appsv1.StatefulSet
			if e.get("dops-postgres", &sts) {
				t.Error("the refused attacker got a database")
			}
			e.noOtherInstallation(cr, "uid-v", "team-v", "v")
			var role rbacv1.Role
			var binding rbacv1.RoleBinding
			var tm appsv1.Deployment
			if e.exists("team-v", foreignName(componentTaskManager), &role) || e.exists("team-v", foreignName(componentTaskManager), &binding) ||
				e.get("dops-task-manager", &tm) {
				t.Error("the attacker got a task manager, or a Role in the victim's namespace")
			}
			if owner := e.claimOwner("team-v"); owner != tc.owner {
				t.Errorf("claim on team-v held by %q, want %q", owner, tc.owner)
			}

			// The victim is unaffected.
			v := e.deployNamed("team-v", "v")
			if c := e.condition(v, CondReady); c.Reason == ReasonNamespaceInUse || c.Reason == ReasonJobNamespaceInUse {
				t.Errorf("the victim was refused: %s", c.Message)
			}
			if e.claimOwner("team-v") != "uid-v" || e.claimOwner("jobs-v") != "uid-v" {
				t.Error("the victim doesn't hold its namespaces")
			}
		})
	}

	// The attacker got there first: the victim, created later, is refused
	// before anything of it is made, and the attacker leaves at once.
	e := newEnv(t, &v1alpha1.DispatchOps{
		Spec: v1alpha1.DispatchOpsSpec{TaskManager: v1alpha1.TaskManagerSpec{JobNamespace: "team-v"}},
	}, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "team-v", Labels: restricted()}})
	e.r.AllowedJobNamespaces = map[string][]string{"team-v": {ns}}
	e.deploy()
	var role rbacv1.Role
	if !e.exists("team-v", foreignName(componentTaskManager), &role) || e.claimOwner("team-v") != "uid-1" {
		t.Fatal("the attacker didn't get its job namespace")
	}
	victim := another("team-v", "v", "uid-v", t0, "")
	if err := e.c.Create(e.ctx, victim); err != nil {
		t.Fatal(err)
	}
	q := workqueue.NewTypedRateLimitingQueue(workqueue.DefaultTypedControllerRateLimiter[reconcile.Request]())
	defer q.ShutDown()
	e.r.enqueueJobNamespaceUsers(e.ctx, q, victim)
	if req, _ := q.Get(); q.Len() != 0 || req.NamespacedName != e.key {
		t.Errorf("creating the victim queued %v, want only the attacker", req)
	}

	_, v := e.reconcileNamed("team-v", "v")
	e.wantCondition(v, CondReady, metav1.ConditionFalse, ReasonNamespaceInUse)
	e.noOtherInstallation(v, "uid-1", ns, "dops")
	if c := e.condition(v, CondReady); !strings.Contains(c.Message, "another Dispatch Ops installation") {
		t.Errorf("refusal = %q", c.Message)
	}
	var key corev1.Secret
	if e.exists("team-v", "v-encryption-key", &key) {
		t.Error("the refused victim got an encryption key in a namespace the attacker held")
	}

	cr := e.reconcile()
	e.wantCondition(cr, CondReady, metav1.ConditionFalse, ReasonJobNamespaceInUse)
	e.noOtherInstallation(cr, "uid-v", "team-v", "v")
	if e.exists("team-v", foreignName(componentTaskManager), &role) || e.claimOwner("team-v") != "" {
		t.Error("the attacker kept its Role or claim in the victim's namespace")
	}
	// What its task manager may have left there (Secrets named like the
	// victim's, Jobs and pods without its label) stays: the attacker's
	// claim is a tombstone, and the victim is refused until an
	// administrator clears it.
	e.wantTombstone("team-v", roleJobNamespace, "uid-1")
	_, v = e.reconcileNamed("team-v", "v")
	e.wantCondition(v, CondReady, metav1.ConditionFalse, ReasonNamespaceReleased)
	if c := e.condition(v, CondReady); !strings.Contains(c.Message, "delete ConfigMap team-v/dispatch-ops-claim") {
		t.Errorf("refusal = %q", c.Message)
	}
	e.noOtherInstallation(v, "uid-1", ns, "dops")
	if e.exists("team-v", "v-encryption-key", &key) {
		t.Error("the refused victim got an encryption key in the attacker's former job namespace")
	}
	e.deleteTombstone("team-v")
	v = e.deployNamed("team-v", "v")
	if c := e.condition(v, CondReady); c.Reason == ReasonNamespaceInUse {
		t.Errorf("the victim is still refused: %s", c.Message)
	}
	if e.claimOwner("team-v") != "uid-v" {
		t.Error("the victim doesn't hold its namespace")
	}
}

// R2-H2 F3: claims are create-only, so of installations (of one operator
// or of two releases sharing the namespace) claiming a namespace at once,
// exactly one gets it, and keeps it.
func TestClaimRace(t *testing.T) {
	a := another("team-a", "a", "uid-a", t0, "jobs")
	b := another("team-b", "b", "uid-b", t0, "jobs")
	e := newEnv(t, &v1alpha1.DispatchOps{}, a, b)
	other := *e.r // another operator release, with the same API server
	var wg sync.WaitGroup
	var mu sync.Mutex
	won := map[types.UID]int{}
	lost := map[types.UID]int{}
	for i := range 20 {
		r, cr := e.r, a
		if i%2 == 1 {
			r, cr = &other, b
		}
		wg.Go(func() {
			holder, err := r.claimNamespace(e.ctx, cr, "jobs")
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err != nil:
				t.Error(err)
			case holder == nil:
				won[cr.UID]++
			default:
				if holder.uid == cr.UID || holder.describe(cr) != "another Dispatch Ops installation" {
					t.Errorf("holder %+v for %s", holder, cr.UID)
				}
				lost[cr.UID]++
			}
		})
	}
	wg.Wait()
	winner := e.claimOwner("jobs")
	if len(won) != 1 || won[winner] != 10 || len(lost) != 1 || lost[winner] != 0 {
		t.Errorf("claim held by %q; won %v, lost %v", winner, won, lost)
	}

	// Through reconciles: the second to come is refused, and gets no Role.
	e = newEnv(t, &v1alpha1.DispatchOps{}, a, b, jobsNamespace(""),
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "team-a", Labels: restricted()}},
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "team-b", Labels: restricted()}})
	e.r.AllowedJobNamespaces = map[string][]string{"jobs": {"team-a", "team-b"}}
	other = *e.r
	e.deployNamed("team-a", "a")
	e.r = &other
	cr := e.deployNamed("team-b", "b")
	e.wantCondition(cr, CondReady, metav1.ConditionFalse, ReasonJobNamespaceInUse)
	e.noOtherInstallation(cr, "uid-a", "team-a", "a")
	var role rbacv1.Role
	if !e.exists("jobs", install{DispatchOps: a}.foreignJobObjectName(componentTaskManager), &role) ||
		e.exists("jobs", install{DispatchOps: b}.foreignJobObjectName(componentTaskManager), &role) || e.claimOwner("jobs") != "uid-a" {
		t.Error("the job namespace isn't a's alone")
	}
}

// A claim is released only once the installation has left: its Jobs, Role,
// RoleBinding and NetworkPolicy gone and its job pods stopped. Its status
// records where it holds claims, so a job namespace it no longer names is
// still cleaned up and released. Released claims become tombstones.
func TestClaimReleasedOnlyAfterLeaving(t *testing.T) {
	e := newEnv(t, &v1alpha1.DispatchOps{Spec: v1alpha1.DispatchOpsSpec{
		TaskManager: v1alpha1.TaskManagerSpec{JobNamespace: "jobs"},
	}}, jobsNamespace(ns), k8sJob("jobs", "dispatch-job-mine", "uid-1"), jobPod("jobs", "dispatch-job-mine-abc", "uid-1", corev1.PodRunning))
	e.deploy()
	if e.claimOwner("jobs") != "uid-1" || e.claimOwner(ns) != "uid-1" || !slices.Equal(e.cr().Status.JobNamespaces, []string{"jobs"}) {
		t.Fatalf("claims: jobs %q, own %q, status %v", e.claimOwner("jobs"), e.claimOwner(ns), e.cr().Status.JobNamespaces)
	}

	// Back to its own namespace.
	cr := e.cr()
	cr.Spec.TaskManager.JobNamespace = ""
	if err := e.c.Update(e.ctx, cr); err != nil {
		t.Fatal(err)
	}
	res, err := e.r.Reconcile(e.ctx, ctrl.Request{NamespacedName: e.key})
	if err != nil || res.RequeueAfter != cleanupPoll {
		t.Errorf("reconcile = %+v, %v; want a requeue after %s", res, err, cleanupPoll)
	}
	var job batchv1.Job
	var role rbacv1.Role
	var np networkingv1.NetworkPolicy
	if e.exists("jobs", "dispatch-job-mine", &job) || e.exists("jobs", foreignName(componentTaskManager), &role) {
		t.Error("its Job or Role stayed in the job namespace it left")
	}
	if !e.exists("jobs", foreignName(componentJobs), &np) || e.claimOwner("jobs") != "uid-1" {
		t.Error("its NetworkPolicy or claim went while a job pod still ran")
	}

	var pod corev1.Pod
	e.exists("jobs", "dispatch-job-mine-abc", &pod)
	pod.Status.Phase = corev1.PodSucceeded
	if err := e.c.Status().Update(e.ctx, &pod); err != nil {
		t.Fatal(err)
	}
	e.reconcile()
	if e.exists("jobs", foreignName(componentJobs), &np) || e.claimOwner("jobs") != "" {
		t.Error("its NetworkPolicy or claim survived leaving")
	}
	e.wantTombstone("jobs", roleJobNamespace, "uid-1")
	if e.claimOwner(ns) != "uid-1" || len(e.cr().Status.JobNamespaces) != 0 {
		t.Errorf("own claim %q, status %v", e.claimOwner(ns), e.cr().Status.JobNamespaces)
	}

	// Deleted: its own namespace's claim goes last, to a tombstone.
	if err := e.c.Delete(e.ctx, e.cr()); err != nil {
		t.Fatal(err)
	}
	if _, err := e.r.Reconcile(e.ctx, ctrl.Request{NamespacedName: e.key}); err != nil {
		t.Fatal(err)
	}
	if e.claimOwner(ns) != "" {
		t.Error("the claim on its namespace survived the DispatchOps")
	}
	e.wantTombstone(ns, roleControlPlane, "uid-1")
}

// wantTombstone fails unless namespace's claim is a tombstone for role,
// released by uid, that says how to clear it.
func (e *env) wantTombstone(namespace, role string, uid types.UID) {
	e.t.Helper()
	var cm corev1.ConfigMap
	if !e.exists(namespace, claimName, &cm) {
		e.t.Fatalf("no tombstone in %s", namespace)
	}
	if releasedRole(&cm) != role || cm.Annotations[releasedUIDAnnotation] != string(uid) || claimOwner(&cm) != "" ||
		!strings.Contains(cm.Data["README"], "delete") {
		e.t.Errorf("tombstone in %s = %+v, want role %s released by %s", namespace, cm, role, uid)
	}
}

// deleteTombstone is the administrator clearing namespace's tombstone.
func (e *env) deleteTombstone(namespace string) {
	e.t.Helper()
	if err := e.c.Delete(e.ctx, &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: claimName}}); err != nil {
		e.t.Fatal(err)
	}
}

// R2-H2 F2: job pods labeled for an installation count only in namespaces
// it holds claims in. One forged elsewhere, even next to a forged claim,
// doesn't hold up deleting the DispatchOps.
func TestForgedPodElsewhereDoesNotBlockFinalizer(t *testing.T) {
	ours := map[string]string{ownerLabel: "uid-1", "app.kubernetes.io/managed-by": ManagedBy}
	forgedClaim := claimFor(&v1alpha1.DispatchOps{ObjectMeta: metav1.ObjectMeta{Name: "dops", Namespace: ns, UID: "uid-1"}}, "evil")
	e := newEnv(t, &v1alpha1.DispatchOps{Spec: v1alpha1.DispatchOpsSpec{
		TaskManager: v1alpha1.TaskManagerSpec{JobNamespace: "jobs"},
	}}, jobsNamespace(""),
		jobPod("evil", "dispatch-job-forged", "uid-1", corev1.PodRunning),
		k8sJob("evil", "dispatch-job-forged", "uid-1"),
		forgedClaim,
		&rbacv1.Role{ObjectMeta: metav1.ObjectMeta{Name: foreignName(componentTaskManager), Namespace: "evil", Labels: ours}})
	e.r.AllowedJobNamespaces = map[string][]string{"jobs": {ns}}
	e.deploy()
	if err := e.c.Delete(e.ctx, e.cr()); err != nil {
		t.Fatal(err)
	}
	res, err := e.r.Reconcile(e.ctx, ctrl.Request{NamespacedName: e.key})
	if err != nil || res.RequeueAfter != 0 {
		t.Errorf("finalize = %+v, %v; want done", res, err)
	}
	if err := e.c.Get(e.ctx, e.key, &v1alpha1.DispatchOps{}); err == nil {
		t.Error("a forged pod held up deleting the DispatchOps")
	}
	var job batchv1.Job
	if !e.exists("evil", "dispatch-job-forged", &job) {
		t.Error("the operator acted on a Job in a namespace the installation doesn't hold")
	}
	if e.claimOwner("jobs") != "" || e.claimOwner(ns) != "" {
		t.Error("claims survived the DispatchOps")
	}
}

// The task manager is granted nothing in a job namespace without the claim
// on it, such as one that doesn't exist yet, even with the Pod Security
// check (which would notice) off.
func TestNoGrantWithoutClaim(t *testing.T) {
	e := newEnv(t, &v1alpha1.DispatchOps{Spec: v1alpha1.DispatchOpsSpec{
		TaskManager: v1alpha1.TaskManagerSpec{JobNamespace: "jobs"},
	}})
	e.r.AllowedJobNamespaces = map[string][]string{"jobs": {ns}}
	e.r.AllowJobNamespacesWithoutPodSecurity = true
	e.r.Client = interceptor.NewClient(e.c.(client.WithWatch), interceptor.Funcs{
		Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			if obj.GetNamespace() == "jobs" {
				return apierrors.NewNotFound(schema.GroupResource{Resource: "namespaces"}, "jobs")
			}
			return c.Create(ctx, obj, opts...)
		},
	})
	e.deploy()
	cr := e.cr()
	e.wantCondition(cr, CondTaskManagerReady, metav1.ConditionFalse, ReasonJobNamespaceNotAllowed)
	var tm appsv1.Deployment
	if e.get("dops-task-manager", &tm) {
		t.Error("the task manager was deployed without its job namespace's claim")
	}
}

// Claims hold only while no one else in a job namespace can write
// ConfigMaps: the task manager's Role must never allow it.
func TestTaskManagerCannotTouchClaims(t *testing.T) {
	for _, rule := range (install{DispatchOps: &v1alpha1.DispatchOps{}}).taskManagerRole().Rules {
		for _, res := range rule.Resources {
			if res == "configmaps" || res == "*" {
				t.Errorf("task manager rule %+v", rule)
			}
		}
	}
}
