package operator

import (
	"slices"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"dispatch-ops/internal/operator/api/v1alpha1"
)

// planted is a Secret a task manager could create in its job namespace:
// named like one the operator generates for another installation, with
// values the attacker knows, and labels it can choose.
func planted(namespace, name string, labels map[string]string) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace, Labels: labels,
			Annotations: map[string]string{generatedAnnotation: "true"}},
		Data: map[string][]byte{"key": []byte("attacker-key"), "password": []byte("attacker-pw")},
	}
}

// finalizeNamed deletes namespace/name and reconciles it until it is
// gone.
func (e *env) finalizeNamed(namespace, name string) {
	e.t.Helper()
	if err := e.c.Delete(e.ctx, e.getCR(namespace, name)); err != nil {
		e.t.Fatal(err)
	}
	if _, err := e.r.Reconcile(e.ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: namespace, Name: name}}); err != nil {
		e.t.Fatal(err)
	}
	if err := e.c.Get(e.ctx, types.NamespacedName{Namespace: namespace, Name: name}, &v1alpha1.DispatchOps{}); err == nil {
		e.t.Fatalf("%s/%s wasn't finalized", namespace, name)
	}
}

// collectGarbage deletes what uid owned in namespace, as Kubernetes would
// once that DispatchOps is gone (the fake client doesn't).
func (e *env) collectGarbage(namespace string, uid types.UID) {
	e.t.Helper()
	for _, list := range []client.ObjectList{&appsv1.DeploymentList{}, &appsv1.StatefulSetList{}, &batchv1.JobList{},
		&corev1.ServiceList{}, &corev1.ServiceAccountList{}, &networkingv1.NetworkPolicyList{}, &networkingv1.IngressList{},
		&rbacv1.RoleList{}, &rbacv1.RoleBindingList{}} {
		if err := e.c.List(e.ctx, list, client.InNamespace(namespace)); err != nil {
			e.t.Fatal(err)
		}
		items, err := meta.ExtractList(list)
		if err != nil {
			e.t.Fatal(err)
		}
		for _, item := range items {
			o := item.(client.Object)
			if ref := metav1.GetControllerOf(o); ref != nil && ref.UID == uid {
				if err := e.c.Delete(e.ctx, o); err != nil {
					e.t.Fatal(err)
				}
			}
		}
	}
}

// F1: what a task manager leaves in its job namespace without the
// installation's label (Secrets named like another installation's, pods
// labeled as its API, Jobs running as its task manager) survives its
// leaving. So the namespace isn't handed on: its claim becomes a tombstone,
// which refuses it both as a DispatchOps's namespace and as another
// installation's job namespace, until an administrator clears it.
func TestReleasedJobNamespaceIsNotReused(t *testing.T) {
	e := newEnv(t, &v1alpha1.DispatchOps{
		Spec: v1alpha1.DispatchOpsSpec{TaskManager: v1alpha1.TaskManagerSpec{JobNamespace: "jobs"}},
	}, jobsNamespace(ns), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "team-b", Labels: restricted()}},
		another("team-b", "b", "uid-b", t0, "jobs"))
	e.deploy()
	if e.claimOwner("jobs") != "uid-1" {
		t.Fatal("the attacker doesn't hold its job namespace")
	}
	// What the attacker's task manager can create there.
	for _, name := range []string{"v-encryption-key", "v-admin-password", "v-postgres", "v-postgres-ca"} {
		if err := e.c.Create(e.ctx, planted("jobs", name, nil)); err != nil {
			t.Fatal(err)
		}
	}
	job := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: "planted", Namespace: "jobs"},
		Spec: batchv1.JobSpec{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{ServiceAccountName: "v-task-manager",
			Containers: []corev1.Container{{Name: "c", Image: "evil"}}}}}}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "planted-api", Namespace: "jobs", Labels: map[string]string{
		"app.kubernetes.io/name": "dispatch-ops", "app.kubernetes.io/instance": "v", "app.kubernetes.io/component": componentAPI}}}
	if err := e.c.Create(e.ctx, job); err != nil {
		t.Fatal(err)
	}
	if err := e.c.Create(e.ctx, pod); err != nil {
		t.Fatal(err)
	}

	// A DispatchOps created there: refused while the attacker holds it,
	// which makes the attacker leave.
	if err := e.c.Create(e.ctx, another("jobs", "v", "uid-v", t0, "")); err != nil {
		t.Fatal(err)
	}
	_, v := e.reconcileNamed("jobs", "v")
	e.wantCondition(v, CondReady, metav1.ConditionFalse, ReasonNamespaceInUse)
	cr := e.reconcile()
	e.wantCondition(cr, CondReady, metav1.ConditionFalse, ReasonJobNamespaceInUse)
	e.reconcile()
	e.wantTombstone("jobs", roleJobNamespace, "uid-1")
	if !slices.Equal(e.cr().Status.JobNamespaces, nil) {
		t.Errorf("status.jobNamespaces = %v after leaving", e.cr().Status.JobNamespaces)
	}

	// Still refused, now by the tombstone, and nothing of it is made.
	for range 2 {
		_, v = e.reconcileNamed("jobs", "v")
		e.wantCondition(v, CondReady, metav1.ConditionFalse, ReasonNamespaceReleased)
		if c := e.condition(v, CondReady); !strings.Contains(c.Message, "delete ConfigMap jobs/dispatch-ops-claim after making "+
			"sure the namespace holds nothing left by the previous installation") {
			t.Errorf("refusal = %q", c.Message)
		}
		e.noOtherInstallation(v, "uid-1", ns, "dops")
	}
	var sts appsv1.StatefulSet
	var key corev1.Secret
	if e.exists("jobs", "v-postgres", &sts) {
		t.Error("the victim got a database in the attacker's former job namespace")
	}
	if !e.exists("jobs", "v-encryption-key", &key) || string(key.Data["key"]) != "attacker-key" || key.Labels[ownerLabel] != "" {
		t.Errorf("the planted key was changed: %+v", key.ObjectMeta)
	}

	// Nor is it another installation's job namespace (b, opted in by
	// label once v is gone).
	e.finalizeNamed("jobs", "v")
	e.wantTombstone("jobs", roleJobNamespace, "uid-1")
	e.setNamespaceLabel("team-b")
	for range 2 {
		_, b := e.reconcileNamed("team-b", "b")
		e.wantCondition(b, CondReady, metav1.ConditionFalse, ReasonJobNamespaceReleased)
		if c := e.condition(b, CondReady); !strings.Contains(c.Message, "delete ConfigMap jobs/dispatch-ops-claim") {
			t.Errorf("refusal = %q", c.Message)
		}
	}
	var role rbacv1.Role
	if e.exists("jobs", install{DispatchOps: e.getCR("team-b", "b")}.foreignJobObjectName(componentTaskManager), &role) {
		t.Error("b got a Role in the former job namespace")
	}
	e.wantTombstone("jobs", roleJobNamespace, "uid-1")
}

// F1, on its own: the tombstone gone (an administrator cleared it without
// looking), a first install still won't take a Secret it would generate
// from whoever left it there, labeled or not: only one labeled with its own
// UID.
func TestFirstInstallRefusesForeignSecrets(t *testing.T) {
	for _, name := range []string{"dops-encryption-key", "dops-admin-password", "dops-postgres", "dops-postgres-ca",
		"dops-postgres-tls", "dops-api-tls"} {
		for label, labels := range map[string]map[string]string{
			"unlabeled":     nil,
			"another owner": {ownerLabel: "uid-x", "app.kubernetes.io/managed-by": ManagedBy},
		} {
			t.Run(name+", "+label, func(t *testing.T) {
				e := newEnv(t, &v1alpha1.DispatchOps{}, planted(ns, name, labels))
				for range 2 {
					res, err := e.r.Reconcile(e.ctx, ctrl.Request{NamespacedName: e.key})
					if err != nil || res.RequeueAfter != pollInterval {
						t.Errorf("reconcile = %+v, %v", res, err)
					}
				}
				cr := e.cr()
				e.wantCondition(cr, CondReady, metav1.ConditionFalse, ReasonForeignSecret)
				if c := e.condition(cr, CondReady); !strings.Contains(c.Message, name) || !strings.Contains(c.Message, ownerLabel+"=uid-1") {
					t.Errorf("refusal = %q", c.Message)
				}
				var sts appsv1.StatefulSet
				var s corev1.Secret
				if e.get("dops-postgres", &sts) {
					t.Error("installed on a foreign Secret")
				}
				if !e.get(name, &s) || string(s.Data["key"]) != "attacker-key" || s.Labels[ownerLabel] != labels[ownerLabel] {
					t.Errorf("the foreign Secret was changed: %+v", s.ObjectMeta)
				}

				// Deleted, the operator generates its own.
				if err := e.c.Delete(e.ctx, &s); err != nil {
					t.Fatal(err)
				}
				e.deploy()
				if !e.get(name, &s) || s.Labels[ownerLabel] != "uid-1" || string(s.Data["key"]) == "attacker-key" {
					t.Errorf("generated %s = %+v", name, s.ObjectMeta)
				}
				if c := e.condition(e.cr(), CondReady); c.Reason == ReasonForeignSecret {
					t.Errorf("still refused: %s", c.Message)
				}
			})
		}
	}

	// One the tenant made for this installation, and labeled for it, is
	// taken.
	own := planted(ns, "dops-encryption-key", map[string]string{ownerLabel: "uid-1"})
	e := newEnv(t, &v1alpha1.DispatchOps{}, own)
	e.deploy()
	var s corev1.Secret
	if e.get("dops-encryption-key", &s); string(s.Data["key"]) != "attacker-key" {
		t.Error("the Secret labeled for the installation was replaced")
	}
	if c := e.condition(e.cr(), CondReady); c.Reason == ReasonForeignSecret {
		t.Errorf("refused: %s", c.Message)
	}

	// A Secret the spec names isn't generated, so isn't checked.
	e = newEnv(t, &v1alpha1.DispatchOps{Spec: v1alpha1.DispatchOpsSpec{
		EncryptionKeySecret: &corev1.LocalObjectReference{Name: "dops-encryption-key"},
	}}, planted(ns, "dops-encryption-key", nil))
	e.deploy()
	if c := e.condition(e.cr(), CondReady); c.Reason == ReasonForeignSecret {
		t.Errorf("refused a Secret the spec names: %s", c.Message)
	}
}

// F2: the job namespaces an installation holds are recorded in its status,
// which tenants can't write, not in its own namespace's claim, which they
// can. Neither a cleared claim nor a changed spec, nor its own namespace
// going (with its claim), makes the operator forget one: its Jobs are
// deleted, and the claim released once its pods are done.
func TestStatusRecordsHeldJobNamespaces(t *testing.T) {
	e := newEnv(t, &v1alpha1.DispatchOps{
		Spec: v1alpha1.DispatchOpsSpec{TaskManager: v1alpha1.TaskManagerSpec{JobNamespace: "jobs"}},
	}, jobsNamespace(ns), k8sJob("jobs", "dispatch-job-a", "uid-1"),
		jobPod("jobs", "dispatch-job-a-x", "uid-1", corev1.PodRunning))
	e.deploy()
	if got := e.cr().Status.JobNamespaces; !slices.Equal(got, []string{"jobs"}) {
		t.Fatalf("status.jobNamespaces = %v", got)
	}
	var own corev1.ConfigMap
	if !e.exists(ns, claimName, &own) || len(own.Data) != 0 {
		t.Errorf("own claim records something: %+v", own.Data)
	}

	// The tenant rewrites its own claim, and moves its jobs back home.
	own.Annotations, own.Data = nil, map[string]string{"jobNamespaces": ""}
	if err := e.c.Update(e.ctx, &own); err != nil {
		t.Fatal(err)
	}
	cr := e.cr()
	cr.Spec.TaskManager.JobNamespace = ""
	if err := e.c.Update(e.ctx, cr); err != nil {
		t.Fatal(err)
	}
	e.reconcile()
	var job batchv1.Job
	var role rbacv1.Role
	if e.exists("jobs", "dispatch-job-a", &job) || e.exists("jobs", foreignName(componentTaskManager), &role) {
		t.Error("its Job or Role stayed in the job namespace it left")
	}
	if e.claimOwner("jobs") != "uid-1" || !slices.Equal(e.cr().Status.JobNamespaces, []string{"jobs"}) {
		t.Errorf("forgot jobs while its pod still runs: claim %q, status %v", e.claimOwner("jobs"), e.cr().Status.JobNamespaces)
	}
	var pod corev1.Pod
	e.exists("jobs", "dispatch-job-a-x", &pod)
	pod.Status.Phase = corev1.PodSucceeded
	if err := e.c.Status().Update(e.ctx, &pod); err != nil {
		t.Fatal(err)
	}
	e.reconcile()
	e.wantTombstone("jobs", roleJobNamespace, "uid-1")
	if got := e.cr().Status.JobNamespaces; len(got) != 0 {
		t.Errorf("status.jobNamespaces = %v after releasing", got)
	}
}

// F2: deleting a DispatchOps's namespace deletes its claim there before
// the DispatchOps is finalized. Its job namespace, which the spec no longer
// names, is still cleaned up, through its status.
func TestFinalizeFindsJobNamespacesThroughStatus(t *testing.T) {
	e := newEnv(t, &v1alpha1.DispatchOps{
		Spec: v1alpha1.DispatchOpsSpec{TaskManager: v1alpha1.TaskManagerSpec{JobNamespace: "jobs"}},
	}, jobsNamespace(ns), k8sJob("jobs", "dispatch-job-a", "uid-1"),
		jobPod("jobs", "dispatch-job-a-x", "uid-1", corev1.PodRunning))
	e.deploy()
	cr := e.cr()
	cr.Spec.TaskManager.JobNamespace = "elsewhere"
	if err := e.c.Update(e.ctx, cr); err != nil {
		t.Fatal(err)
	}
	e.deleteTombstone(ns) // the own claim, gone with the namespace
	if err := e.c.Delete(e.ctx, e.cr()); err != nil {
		t.Fatal(err)
	}
	res, err := e.r.Reconcile(e.ctx, ctrl.Request{NamespacedName: e.key})
	if err != nil || res.RequeueAfter != cleanupPoll {
		t.Errorf("finalize with a job pod running = %+v, %v; want a requeue after %s", res, err, cleanupPoll)
	}
	var job batchv1.Job
	var role rbacv1.Role
	if e.exists("jobs", "dispatch-job-a", &job) || e.exists("jobs", foreignName(componentTaskManager), &role) {
		t.Error("finalizing kept its Job or Role in the job namespace")
	}
	if e.claimOwner("jobs") != "uid-1" {
		t.Error("its claim went while a job pod still ran")
	}
	var pod corev1.Pod
	e.exists("jobs", "dispatch-job-a-x", &pod)
	if err := e.c.Delete(e.ctx, &pod); err != nil {
		t.Fatal(err)
	}
	if _, err := e.r.Reconcile(e.ctx, ctrl.Request{NamespacedName: e.key}); err != nil {
		t.Fatal(err)
	}
	if err := e.c.Get(e.ctx, e.key, &v1alpha1.DispatchOps{}); err == nil {
		t.Error("not finalized")
	}
	e.wantTombstone("jobs", roleJobNamespace, "uid-1")
}

// F3: a deleted installation keeps its generated Secrets and database
// volume in its namespace, which is then refused as anyone's job
// namespace, by allow-list or by label.
func TestFinalizedNamespaceIsNoJobNamespace(t *testing.T) {
	for _, mode := range []string{"label", "allow-list"} {
		t.Run(mode, func(t *testing.T) {
			e := newEnv(t, &v1alpha1.DispatchOps{
				Spec: v1alpha1.DispatchOpsSpec{TaskManager: v1alpha1.TaskManagerSpec{JobNamespace: "team-v"}},
			}, another("team-v", "v", "uid-v", t0, ""), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "team-v", Labels: restricted()}})
			e.deployNamed("team-v", "v")
			e.finalizeNamed("team-v", "v")
			e.wantTombstone("team-v", roleControlPlane, "uid-v")
			var key corev1.Secret
			if !e.exists("team-v", "v-encryption-key", &key) {
				t.Fatal("the deleted installation's encryption key wasn't kept")
			}
			if mode == "label" {
				var n corev1.Namespace
				if err := e.c.Get(e.ctx, types.NamespacedName{Name: "team-v"}, &n); err != nil {
					t.Fatal(err)
				}
				n.Labels[AllowJobsFromLabel] = ns
				if err := e.c.Update(e.ctx, &n); err != nil {
					t.Fatal(err)
				}
			} else {
				e.r.AllowedJobNamespaces = map[string][]string{"team-v": {ns}}
			}
			for range 2 {
				cr := e.reconcile()
				e.wantCondition(cr, CondReady, metav1.ConditionFalse, ReasonJobNamespaceReleased)
				if c := e.condition(cr, CondReady); !strings.Contains(c.Message, "delete ConfigMap team-v/dispatch-ops-claim") ||
					!strings.Contains(c.Message, "PersistentVolumeClaims") {
					t.Errorf("refusal = %q", c.Message)
				}
			}
			var role rbacv1.Role
			var tm appsv1.Deployment
			if e.exists("team-v", foreignName(componentTaskManager), &role) || e.get("dops-task-manager", &tm) {
				t.Error("granted a job namespace holding a deleted installation's Secrets")
			}
			e.wantTombstone("team-v", roleControlPlane, "uid-v")
		})
	}
}

// The documented reinstall: a DispatchOps deleted and created again under
// the same name in the same namespace takes over the generated Secrets
// (and database volume) its predecessor left, and the tombstone goes.
func TestReinstallTakesOverKeptData(t *testing.T) {
	e := newEnv(t, &v1alpha1.DispatchOps{})
	e.deploy()
	kept := map[string][]byte{}
	for _, name := range []string{"dops-encryption-key", "dops-postgres", "dops-postgres-ca"} {
		var s corev1.Secret
		if !e.get(name, &s) || s.Labels[ownerLabel] != "uid-1" {
			t.Fatalf("generated %s = %+v", name, s.ObjectMeta)
		}
		kept[name] = s.Data["key"]
		if name == "dops-postgres" {
			kept[name] = s.Data["url"]
		}
	}
	e.finalizeNamed(ns, "dops")
	e.wantTombstone(ns, roleControlPlane, "uid-1")
	e.collectGarbage(ns, "uid-1")

	again := another(ns, "dops", "uid-2", t0, "")
	if err := e.c.Create(e.ctx, again); err != nil {
		t.Fatal(err)
	}
	cr := e.reconcile()
	if cr.UID != "uid-2" {
		t.Fatalf("UID %s", cr.UID)
	}
	if e.claimOwner(ns) != "uid-2" {
		t.Errorf("claim on %s held by %q, want uid-2", ns, e.claimOwner(ns))
	}
	var cm corev1.ConfigMap
	if e.exists(ns, claimName, &cm); releasedRole(&cm) != "" || cm.Annotations[releasedUIDAnnotation] != "" {
		t.Errorf("the tombstone stayed: %+v", cm.ObjectMeta)
	}
	for name, v := range kept {
		var s corev1.Secret
		e.get(name, &s)
		got := s.Data["key"]
		if name == "dops-postgres" {
			got = s.Data["url"]
		}
		if s.Labels[ownerLabel] != "uid-2" || string(got) != string(v) {
			t.Errorf("kept %s = %+v, not taken over", name, s.ObjectMeta)
		}
	}
	e.databaseReady()
	e.reconcile()
	e.finishMigration("dispatch-ops:1.0", true)
	cr = e.reconcile()
	if c := e.condition(cr, CondReady); c.Reason == ReasonForeignSecret || c.Reason == ReasonNamespaceReleased {
		t.Errorf("the reinstall was refused: %s", c.Message)
	}
	var tm appsv1.Deployment
	if !e.get("dops-task-manager", &tm) {
		t.Error("the reinstall has no task manager")
	}
}

// A DispatchOps of another name may take a deleted installation's
// namespace, but not its Secrets, and the predecessor's tombstone goes
// with it: a later reinstall under the first name is a first install.
func TestReinstallTakesOverOnlyThePredecessorsSecrets(t *testing.T) {
	e := newEnv(t, &v1alpha1.DispatchOps{})
	e.deploy()
	e.finalizeNamed(ns, "dops")

	other := another(ns, "other", "uid-o", t0, "")
	if err := e.c.Create(e.ctx, other); err != nil {
		t.Fatal(err)
	}
	_, o := e.reconcileNamed(ns, "other")
	if e.claimOwner(ns) != "uid-o" {
		t.Errorf("other doesn't hold the namespace: %q (%+v)", e.claimOwner(ns), o.Status.Conditions)
	}
	var key corev1.Secret
	if e.get("dops-encryption-key", &key); key.Labels[ownerLabel] != "uid-1" {
		t.Errorf("another DispatchOps took over the deleted one's key: %v", key.Labels)
	}
	e.finalizeNamed(ns, "other")

	// The same name again: but the tombstone is other's now.
	if err := e.c.Create(e.ctx, another(ns, "dops", "uid-2", t0, "")); err != nil {
		t.Fatal(err)
	}
	cr := e.reconcile()
	e.wantCondition(cr, CondReady, metav1.ConditionFalse, ReasonForeignSecret)
	if e.get("dops-encryption-key", &key); key.Labels[ownerLabel] != "uid-1" {
		t.Errorf("took over a key without its predecessor's tombstone: %v", key.Labels)
	}
}

// The planted Secret is refused on a reinstall too, while the
// predecessor's own are taken over.
func TestReinstallRefusesAPlantedSecret(t *testing.T) {
	e := newEnv(t, &v1alpha1.DispatchOps{Spec: v1alpha1.DispatchOpsSpec{
		API: v1alpha1.APISpec{TLS: v1alpha1.APITLSSpec{Enabled: new(false)}},
	}})
	e.deploy()
	e.finalizeNamed(ns, "dops")
	if err := e.c.Create(e.ctx, planted(ns, "dops-api-tls", nil)); err != nil {
		t.Fatal(err)
	}
	if err := e.c.Create(e.ctx, another(ns, "dops", "uid-2", t0, "")); err != nil {
		t.Fatal(err)
	}
	cr := e.reconcile()
	e.wantCondition(cr, CondReady, metav1.ConditionFalse, ReasonForeignSecret)
	if c := e.condition(cr, CondReady); !strings.Contains(c.Message, "dops-api-tls") {
		t.Errorf("refusal = %q", c.Message)
	}
	var key corev1.Secret
	if e.get("dops-encryption-key", &key); key.Labels[ownerLabel] != "uid-2" {
		t.Errorf("the predecessor's key wasn't taken over: %v", key.Labels)
	}
}

// An installation that left its job namespace (its opt-in removed for a
// while, say) may take it back: nobody else could use it in between.
func TestHolderRetakesItsReleasedJobNamespace(t *testing.T) {
	e := newEnv(t, &v1alpha1.DispatchOps{
		Spec: v1alpha1.DispatchOpsSpec{TaskManager: v1alpha1.TaskManagerSpec{JobNamespace: "jobs"}},
	}, jobsNamespace(ns))
	e.deploy()
	e.setNamespaceLabel("")
	e.reconcile()
	e.wantTombstone("jobs", roleJobNamespace, "uid-1")
	e.setNamespaceLabel(ns)
	cr := e.reconcile()
	if c := e.condition(cr, CondTaskManagerReady); c.Reason == ReasonJobNamespaceReleased || c.Reason == ReasonJobNamespaceNotAllowed {
		t.Errorf("refused its own former job namespace: %s", c.Message)
	}
	var role rbacv1.Role
	if e.claimOwner("jobs") != "uid-1" || !e.exists("jobs", foreignName(componentTaskManager), &role) {
		t.Error("didn't take its job namespace back")
	}
	if got := cr.Status.JobNamespaces; !slices.Equal(got, []string{"jobs"}) {
		t.Errorf("status.jobNamespaces = %v", got)
	}

	// A DispatchOps created again under the same name is another
	// installation: refused.
	e.finalizeNamed(ns, "dops")
	if err := e.c.Create(e.ctx, &v1alpha1.DispatchOps{
		ObjectMeta: metav1.ObjectMeta{Name: "dops", Namespace: ns, UID: "uid-2"},
		Spec:       v1alpha1.DispatchOpsSpec{TaskManager: v1alpha1.TaskManagerSpec{JobNamespace: "jobs"}},
	}); err != nil {
		t.Fatal(err)
	}
	cr = e.reconcile()
	e.wantCondition(cr, CondReady, metav1.ConditionFalse, ReasonJobNamespaceReleased)
}
