package operator

import (
	"context"
	"fmt"
	"maps"
	"reflect"
	"slices"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/selection"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/workqueue"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"dispatch-ops/internal/operator/api/v1alpha1"
)

// A job namespace serves a single installation: its task manager creates
// pods there, and a pod can mount any Secret in its namespace, its job
// tokens and registry Secrets included. An installation holds a job
// namespace by its claim there (see claim.go), and one that leaves a job
// namespace takes everything of its own there with it (leaveJobNamespaces)
// before it releases the claim.

// operatorSelector selects the objects the operator labeled with one of
// components.
func operatorSelector(components ...string) labels.Selector {
	req, err := labels.NewRequirement("app.kubernetes.io/component", selection.In, components)
	if err != nil {
		panic(err)
	}
	return labels.SelectorFromSet(labels.Set{"app.kubernetes.io/managed-by": ManagedBy}).Add(*req)
}

// operatorObjectOwner is the UID of the DispatchOps the operator made o
// for: its ownerLabel, or in a DispatchOps's own namespace its controller.
func operatorObjectOwner(o client.Object) types.UID {
	if uid := o.GetLabels()[ownerLabel]; uid != "" {
		return types.UID(uid)
	}
	if ref := metav1.GetControllerOf(o); ref != nil && ref.Kind == "DispatchOps" {
		if gv, err := schema.ParseGroupVersion(ref.APIVersion); err == nil && gv.Group == v1alpha1.GroupVersion.Group {
			return ref.UID
		}
	}
	return ""
}

// installationJobLabels select the Jobs and pods the task manager of the
// installation uid created.
func installationJobLabels(uid types.UID) client.MatchingLabels {
	l := client.MatchingLabels(jobPodLabels())
	l[installationLabel] = string(uid)
	return l
}

// jobNamespaceOccupants are the installations other than self that have
// objects in ns, each with one of them for the log, read from the API
// server (the cache holds only some of them): the task manager's Role and
// RoleBinding and the job NetworkPolicy the operator made (by their
// ownerLabel, or in a DispatchOps's own namespace their owner reference),
// and the Jobs and pods a task manager created (by their installation
// label). Objects of another operator release count too. An object labeled
// with neither (a Job from a task manager older than installation labels)
// counts for no one. Claims decide who holds a namespace; this catches what
// an installation left there without one.
func (r *Reconciler) jobNamespaceOccupants(ctx context.Context, ns string, self types.UID) (map[types.UID]string, error) {
	occ := map[types.UID]string{}
	installation := func(o client.Object) types.UID { return types.UID(o.GetLabels()[installationLabel]) }
	for _, k := range []struct {
		list  client.ObjectList
		sel   labels.Selector
		owner func(client.Object) types.UID
	}{
		{&rbacv1.RoleList{}, operatorSelector(componentTaskManager), operatorObjectOwner},
		{&rbacv1.RoleBindingList{}, operatorSelector(componentTaskManager), operatorObjectOwner},
		{&networkingv1.NetworkPolicyList{}, operatorSelector(componentJobs, componentUnlabeledJobs), operatorObjectOwner},
		{&batchv1.JobList{}, labels.SelectorFromSet(jobPodLabels()), installation},
		{&corev1.PodList{}, labels.SelectorFromSet(jobPodLabels()), installation},
	} {
		if err := r.APIReader.List(ctx, k.list, client.InNamespace(ns), client.MatchingLabelsSelector{Selector: k.sel}); err != nil {
			return nil, fmt.Errorf("job namespace %s: %w", ns, err)
		}
		items, err := meta.ExtractList(k.list)
		if err != nil {
			return nil, err
		}
		for _, item := range items {
			o := item.(client.Object)
			if uid := k.owner(o); uid != "" && uid != self && occ[uid] == "" {
				occ[uid] = reflect.TypeOf(o).Elem().Name() + " " + o.GetName()
			}
		}
	}
	return occ, nil
}

// holdJobNamespace makes sure in's job namespace, which it may use, is
// its alone, and claims it: "" if so, and otherwise the reason and message
// to refuse it with. Another namespace than its own must not host a
// DispatchOps (that installation's Secrets are there, claimed or not),
// must hold no other installation's job objects, and must not be claimed
// by another installation, or be a tombstone (see claim.go): another
// installation's former job namespace, or a deleted one's namespace.
func (r *Reconciler) holdJobNamespace(ctx context.Context, cr *v1alpha1.DispatchOps, in install) (string, string, error) {
	ns := in.jobNamespace()
	logger := log.FromContext(ctx)
	if ns != cr.Namespace {
		var list v1alpha1.DispatchOpsList
		if err := r.APIReader.List(ctx, &list, client.InNamespace(ns)); err != nil {
			return "", "", fmt.Errorf("job namespace %s: %w", ns, err)
		}
		if len(list.Items) > 0 {
			logger.Info("refusing a job namespace that hosts a DispatchOps", "jobNamespace", ns,
				"dispatchOps", list.Items[0].Name, "uid", list.Items[0].UID)
			return ReasonJobNamespaceInUse, fmt.Sprintf("Job namespace %s hosts another Dispatch Ops installation, which "+
				"can't share it: the task manager could create pods there mounting that installation's Secrets. Set "+
				"spec.taskManager.jobNamespace to a namespace of its own", ns), nil
		}
	}
	occ, err := r.jobNamespaceOccupants(ctx, ns, cr.UID)
	if err != nil {
		return "", "", err
	}
	for uid, example := range occ {
		logger.Info("refusing a job namespace with another installation's objects", "jobNamespace", ns, "uid", uid, "object", example)
		return ReasonJobNamespaceInUse, fmt.Sprintf("Job namespace %s holds objects of another Dispatch Ops installation "+
			"(its task manager Role, job NetworkPolicy, Jobs or job pods). A job namespace serves only one installation: "+
			"set spec.taskManager.jobNamespace to a namespace of its own, or wait until those objects are gone (the "+
			"operator log names their installation, for an administrator to clean up what a deleted one left)", ns), nil
	}
	if ns == cr.Namespace {
		return "", "", nil // claimed with the namespace
	}
	holder, err := r.claimJobNamespace(ctx, cr, in)
	if err != nil || holder == nil {
		return "", "", err
	}
	clear := fmt.Sprintf("An administrator must delete ConfigMap %s/%s after making sure the namespace holds nothing "+
		"left by the previous installation", ns, claimName)
	switch holder.released {
	case roleJobNamespace:
		logger.Info("refusing a former job namespace", "jobNamespace", ns, "releasedBy", holder.uid)
		return ReasonJobNamespaceReleased, fmt.Sprintf("Job namespace %s was the job namespace of another Dispatch Ops "+
			"installation, whose task manager could have left Secrets, pods or Jobs there, and isn't handed to another "+
			"one. %s (or better, delete and recreate the namespace); or set spec.taskManager.jobNamespace to a "+
			"namespace of its own", ns, clear), nil
	case roleControlPlane:
		logger.Info("refusing a deleted installation's namespace as a job namespace", "jobNamespace", ns, "releasedBy", holder.uid)
		return ReasonJobNamespaceReleased, fmt.Sprintf("Job namespace %s was the namespace of a deleted Dispatch Ops "+
			"installation, whose generated Secrets (its encryption key among them) and database volume are kept "+
			"there, for job pods to mount. %s (delete its Secrets and PersistentVolumeClaims, or better, delete and "+
			"recreate the namespace); or set spec.taskManager.jobNamespace to a namespace of its own", ns, clear), nil
	}
	logger.Info("refusing a job namespace held by another installation", "jobNamespace", ns, "holderUID", holder.uid)
	return ReasonJobNamespaceInUse, fmt.Sprintf("Job namespace %s is held by %s. A job namespace serves only one "+
		"installation, and the one holding it keeps it: set spec.taskManager.jobNamespace to a namespace of its own, "+
		"or wait until that installation has left it", ns, holder.describe(cr)), nil
}

// enqueueJobNamespaceUsers queues every other DispatchOps whose job
// namespace is one of objs's job namespaces or namespaces, so a change to
// one (moving its jobs, being created or deleted) is acted on at once: one
// waiting for a job namespace tries again, and one whose job namespace now
// hosts a DispatchOps leaves it.
func (r *Reconciler) enqueueJobNamespaceUsers(ctx context.Context, q workqueue.TypedRateLimitingInterface[reconcile.Request],
	objs ...client.Object) {
	namespaces, self := map[string]bool{}, map[types.UID]bool{}
	for _, o := range objs {
		if d, ok := o.(*v1alpha1.DispatchOps); ok {
			namespaces[install{DispatchOps: d}.jobNamespace()] = true
			namespaces[d.Namespace] = true
			self[d.UID] = true
		}
	}
	var list v1alpha1.DispatchOpsList
	if err := r.List(ctx, &list); err != nil {
		log.FromContext(ctx).Error(err, "listing DispatchOps sharing a job namespace")
		return
	}
	for i := range list.Items {
		d := &list.Items[i]
		if !self[d.UID] && namespaces[install{DispatchOps: d}.jobNamespace()] {
			q.Add(reconcile.Request{NamespacedName: types.NamespacedName{Namespace: d.Namespace, Name: d.Name}})
		}
	}
}

// revokeJobAccess takes away what lets cr's task manager run jobs: its
// Deployment, then everything of its own in job namespaces, and its claims
// on them (see leaveJobNamespaces).
func (r *Reconciler) revokeJobAccess(ctx context.Context, cr *v1alpha1.DispatchOps, in install) (bool, error) {
	tm := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: in.name(componentTaskManager), Namespace: in.Namespace}}
	if err := r.deleteIfOwned(ctx, cr, tm); err != nil {
		return false, err
	}
	return r.leaveJobNamespaces(ctx, cr, in, "", "", nil)
}

// jobNamespaceKind is a kind of object the operator makes in a job
// namespace, by the components it is labeled and named with.
type jobNamespaceKind struct {
	newList    func() client.ObjectList
	newObject  func() client.Object
	components []string
}

var (
	jobRBACKinds = []jobNamespaceKind{
		{func() client.ObjectList { return &rbacv1.RoleList{} }, func() client.Object { return &rbacv1.Role{} },
			[]string{componentTaskManager}},
		{func() client.ObjectList { return &rbacv1.RoleBindingList{} }, func() client.Object { return &rbacv1.RoleBinding{} },
			[]string{componentTaskManager}},
	}
	jobNetworkPolicyKind = jobNamespaceKind{
		func() client.ObjectList { return &networkingv1.NetworkPolicyList{} }, func() client.Object { return &networkingv1.NetworkPolicy{} },
		[]string{componentJobs, componentUnlabeledJobs},
	}
)

// leaveJobNamespaces takes away what cr's installation has in job
// namespaces other than keepNS ("" for every one), in this order: its
// Jobs (in the background, so their pods and the Secrets they own go too),
// the task manager's Role and RoleBinding, its Jobs again (any created in
// between), and last the job NetworkPolicy, once none of its job pods
// still runs there. pending reports that some still do, so the
// NetworkPolicy stays for now: call again soon. In keepNS, only the
// operator's objects that aren't in keep (such as an outdated
// NetworkPolicy) are deleted. Then it releases its claims on the job
// namespaces it has left (they become tombstones), but holdNS (which it
// keeps, though it may not run jobs there right now) and its own
// namespace, whose claim goes only with the DispatchOps (see finalize).
//
// Jobs and job pods count only in its own namespace and those it holds
// claims in (claimedNamespaces), the only ones its task manager could have
// created them in: one labeled for it anywhere else (forged, say) can't
// hold up its cleanup.
func (r *Reconciler) leaveJobNamespaces(ctx context.Context, cr *v1alpha1.DispatchOps, in install, keepNS, holdNS string,
	keep []client.Object) (pending bool, err error) {
	held, err := r.claimedNamespaces(ctx, cr, in)
	if err != nil {
		return false, err
	}
	namespaces := append(slices.Sorted(maps.Keys(held)), cr.Namespace)
	if err := r.deleteJobs(ctx, cr, namespaces, keepNS); err != nil {
		return false, err
	}
	for _, k := range jobRBACKinds {
		if err := r.pruneJobNamespaceObjects(ctx, cr, in, k, keep, nil); err != nil {
			return false, err
		}
	}
	if err := r.deleteJobs(ctx, cr, namespaces, keepNS); err != nil {
		return false, err
	}
	running, err := r.namespacesWithJobPods(ctx, cr, namespaces, keepNS)
	if err != nil {
		return false, err
	}
	if err := r.pruneJobNamespaceObjects(ctx, cr, in, jobNetworkPolicyKind, keep, running); err != nil {
		return false, err
	}
	stay := maps.Clone(running)
	stay[keepNS], stay[holdNS] = true, true
	if err := r.releaseClaims(ctx, cr, held, stay); err != nil {
		return false, err
	}
	return len(running) > 0, nil
}

// listNamespaces are the namespaces to list in: those the operator may
// access, or "" for all of them.
func (r *Reconciler) listNamespaces() []string {
	if len(r.Namespaces) == 0 {
		return []string{""}
	}
	return r.Namespaces
}

// deleteJobs deletes the Kubernetes Jobs cr's task manager created
// (labeled with its installation) in namespaces but keepNS, with
// background propagation, so their pods and the Secrets they own are
// deleted too.
func (r *Reconciler) deleteJobs(ctx context.Context, cr *v1alpha1.DispatchOps, namespaces []string, keepNS string) error {
	for _, ns := range namespaces {
		if ns == keepNS {
			continue
		}
		var list batchv1.JobList
		if err := r.APIReader.List(ctx, &list, installationJobLabels(cr.UID), client.InNamespace(ns)); err != nil {
			return err
		}
		for i := range list.Items {
			j := &list.Items[i]
			log.FromContext(ctx).Info("deleting job", "namespace", j.Namespace, "name", j.Name)
			if err := r.Delete(ctx, j, client.PropagationPolicy(metav1.DeletePropagationBackground)); client.IgnoreNotFound(err) != nil {
				return err
			}
		}
	}
	return nil
}

// namespacesWithJobPods are those of namespaces but keepNS where pods of
// cr's jobs may still run (they haven't succeeded or failed).
func (r *Reconciler) namespacesWithJobPods(ctx context.Context, cr *v1alpha1.DispatchOps, namespaces []string,
	keepNS string) (map[string]bool, error) {
	running := map[string]bool{}
	for _, ns := range namespaces {
		if ns == keepNS {
			continue
		}
		var list corev1.PodList
		if err := r.APIReader.List(ctx, &list, installationJobLabels(cr.UID), client.InNamespace(ns)); err != nil {
			return nil, err
		}
		for _, p := range list.Items {
			if p.Status.Phase != corev1.PodSucceeded && p.Status.Phase != corev1.PodFailed {
				running[p.Namespace] = true
			}
		}
	}
	return running, nil
}

// pruneJobNamespaceObjects deletes the objects of kind k the operator made
// for cr (now or before; see foreignJobObjectName), except keep and those
// in skip's namespaces. Elsewhere than cr's namespace, it deletes only
// objects with the names the operator gives them, its managed-by label and
// cr's ownerLabel; in cr's namespace, only those cr controls.
func (r *Reconciler) pruneJobNamespaceObjects(ctx context.Context, cr *v1alpha1.DispatchOps, in install, k jobNamespaceKind,
	keep []client.Object, skip map[string]bool) error {
	kept := func(o client.Object) bool {
		for _, x := range keep {
			if reflect.TypeOf(x) == reflect.TypeOf(o) && x.GetNamespace() == o.GetNamespace() && x.GetName() == o.GetName() {
				return true
			}
		}
		return false
	}
	var foreign []string
	for _, c := range k.components {
		foreign = append(foreign, in.foreignJobObjectName(c), in.legacyForeignJobObjectName(c))
	}
	sel := client.MatchingLabels{ownerLabel: string(cr.UID), "app.kubernetes.io/managed-by": ManagedBy}
	for _, ns := range r.listNamespaces() {
		if ns == cr.Namespace {
			continue
		}
		list := k.newList()
		if err := r.APIReader.List(ctx, list, sel, client.InNamespace(ns)); err != nil {
			return err
		}
		objs, err := meta.ExtractList(list)
		if err != nil {
			return err
		}
		for _, ro := range objs {
			o := ro.(client.Object)
			if o.GetNamespace() == cr.Namespace || skip[o.GetNamespace()] || !slices.Contains(foreign, o.GetName()) || kept(o) {
				continue
			}
			log.FromContext(ctx).Info("deleting", "kind", reflect.TypeOf(o).Elem().Name(), "namespace", o.GetNamespace(), "name", o.GetName())
			if err := r.Delete(ctx, o); client.IgnoreNotFound(err) != nil {
				return err
			}
		}
	}
	if skip[cr.Namespace] {
		return nil
	}
	for _, c := range k.components {
		o := k.newObject()
		o.SetNamespace(cr.Namespace)
		o.SetName(in.name(c))
		if kept(o) {
			continue
		}
		if err := r.deleteIfOwned(ctx, cr, o); err != nil {
			return err
		}
	}
	return nil
}

// finalize cleans up what owner references can't, or not soon enough:
// the task manager first, so it starts no more jobs, then everything of
// the installation in job namespaces, found through its status even if its
// own namespace is being deleted, and its claims there (see
// leaveJobNamespaces), and last it releases its claim on its own
// namespace: a tombstone that keeps the namespace from becoming anyone's
// job namespace, since generated Secrets and the database volume are kept
// (other owned objects are garbage collected).
func (r *Reconciler) finalize(ctx context.Context, cr *v1alpha1.DispatchOps) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(cr, finalizer) {
		return ctrl.Result{}, nil
	}
	pending, err := r.revokeJobAccess(ctx, cr, install{DispatchOps: withDefaults(cr)})
	if err != nil {
		return ctrl.Result{}, err
	}
	if pending {
		return ctrl.Result{RequeueAfter: cleanupPoll}, nil
	}
	own, err := r.ownClaim(ctx, cr)
	if err != nil {
		return ctrl.Result{}, err
	}
	if own != nil {
		if err := r.releaseClaim(ctx, own, roleControlPlane); err != nil {
			return ctrl.Result{}, err
		}
	}
	controllerutil.RemoveFinalizer(cr, finalizer)
	return ctrl.Result{}, r.Update(ctx, cr)
}
