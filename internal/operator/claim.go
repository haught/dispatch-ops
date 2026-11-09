package operator

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"dispatch-ops/internal/operator/api/v1alpha1"
)

// Claims. An installation holds every namespace it uses through a claim, a
// ConfigMap named claimName there: its own namespace, claimed at its first
// reconcile before anything else is made, and its job namespace, claimed
// before its task manager is granted anything there. A claim is only ever
// created, never taken over: when the ConfigMap exists, whoever created it
// holds the namespace, so of two installations (of this operator or of
// another release sharing the namespace) exactly one gets it.
//
// A claim is never deleted either. Once its installation has left the
// namespace (its Jobs, task manager Role and RoleBinding, and job
// NetworkPolicy gone, and none of its job pods running), the claim becomes
// a tombstone (releasedLabel): the namespace is still marked as used,
// because what the operator removes isn't all that may be left there. A
// task manager runs the tenant's image and can create Secrets (of any
// name), and Jobs whose pods run as any service account, without the
// labels the operator cleans up by; a deleted installation keeps its
// generated Secrets and database volume. So:
//
//   - a former job namespace (roleJobNamespace) is refused both as another
//     installation's job namespace and as a DispatchOps's own namespace;
//     only the installation that released it may take it back;
//   - a deleted installation's own namespace (roleControlPlane) is refused
//     as a job namespace, but may host a DispatchOps again: one of the same
//     name takes over the predecessor's kept Secrets (see adoptKeptSecrets).
//
// Until an administrator deletes the tombstone, after making sure the
// namespace holds nothing left by the previous installation (or better,
// deletes and recreates the namespace).
//
// The job namespaces an installation holds claims in are recorded in its
// status (status.jobNamespaces), written before it claims one and after it
// releases it, so the operator finds them again whatever happens to the
// DispatchOps's spec, the namespaces' opt-in, or its own namespace. The
// status is the operator's alone, unless tenants are granted
// dispatchops/status (don't).
//
// Whoever can write ConfigMaps in a namespace can forge, change or delete
// a claim or tombstone there. In a job namespace that is the operator and
// the administrators (the task manager's Role has no ConfigMap
// permissions). In a DispatchOps's own namespace it is also the tenant, who
// can only refuse their own installation by it, or lift the protection of
// their own namespace and data; the operator only looks for an
// installation's claims in namespaces that the installation itself or the
// administrator chose (claimedNamespaces).

const (
	claimName      = "dispatch-ops-claim"
	componentClaim = "claim"
	// claimedByAnnotation is the claiming DispatchOps's namespace/name, for
	// people and for messages to DispatchOps in the same namespace. A
	// tombstone keeps it.
	claimedByAnnotation = "dispatch-ops.io/claimed-by"
	// releasedLabel makes a claim a tombstone; its value is the role the
	// namespace had for the installation that released it.
	releasedLabel = "dispatch-ops.io/released"
	// releasedUIDAnnotation, on a tombstone, is the UID of the installation
	// that released it.
	releasedUIDAnnotation = "dispatch-ops.io/released-uid"
	// roleJobNamespace: the namespace was the installation's job namespace
	// (and not its own).
	roleJobNamespace = "job-namespace"
	// roleControlPlane: the namespace was the installation's own, which
	// keeps its generated Secrets and database volume.
	roleControlPlane = "control-plane"
)

// claimHolder is the installation that holds a claim, or that released a
// tombstone.
type claimHolder struct {
	uid             types.UID
	namespace, name string
	// released is the tombstone's role, or "" for a claim.
	released string
	// claim is the ConfigMap, as read.
	claim *corev1.ConfigMap
}

// describe names the holder in a message to asker: by its DispatchOps if
// that is in asker's namespace, where asker can see it anyway, and
// otherwise not at all. Its UID goes to the operator's log only.
func (h *claimHolder) describe(asker *v1alpha1.DispatchOps) string {
	if h.name != "" && h.namespace == asker.Namespace {
		return fmt.Sprintf("DispatchOps %s/%s", h.namespace, h.name)
	}
	return "another Dispatch Ops installation"
}

// claimFor is cr's claim on ns.
func claimFor(cr *v1alpha1.DispatchOps, ns string) *corev1.ConfigMap {
	return &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{
		Name:      claimName,
		Namespace: ns,
		Labels: map[string]string{
			"app.kubernetes.io/name":       "dispatch-ops",
			"app.kubernetes.io/component":  componentClaim,
			"app.kubernetes.io/managed-by": ManagedBy,
			ownerLabel:                     string(cr.UID),
		},
		Annotations: map[string]string{claimedByAnnotation: cr.Namespace + "/" + cr.Name},
	}}
}

// claimOwner is the UID of the installation that holds claim cm, or "" if
// cm is a tombstone or isn't a claim the operator made (either of which
// holds the namespace for no installation, and so against every one).
func claimOwner(cm *corev1.ConfigMap) types.UID {
	if cm.Labels["app.kubernetes.io/managed-by"] != ManagedBy || cm.Labels[releasedLabel] != "" {
		return ""
	}
	return types.UID(cm.Labels[ownerLabel])
}

// releasedRole is tombstone cm's role, or "" if cm isn't a tombstone. One
// whose role is unknown counts as a former job namespace, the stricter.
func releasedRole(cm *corev1.ConfigMap) string {
	if cm.Labels["app.kubernetes.io/managed-by"] != ManagedBy {
		return ""
	}
	switch role := cm.Labels[releasedLabel]; role {
	case "", roleControlPlane:
		return role
	}
	return roleJobNamespace
}

// getClaim reads ns's claim from the API server (the cache has no
// ConfigMaps): nil if there is none.
func (r *Reconciler) getClaim(ctx context.Context, ns string) (*corev1.ConfigMap, error) {
	var cm corev1.ConfigMap
	err := r.APIReader.Get(ctx, types.NamespacedName{Namespace: ns, Name: claimName}, &cm)
	if apierrors.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("claim in namespace %s: %w", ns, err)
	}
	return &cm, nil
}

// errNoNamespace: the namespace to claim doesn't exist (yet).
var errNoNamespace = errors.New("namespace not found")

// holderOf is who holds claim cm (or released it, if it is a tombstone),
// or nil if cr holds it.
func holderOf(cm *corev1.ConfigMap, cr *v1alpha1.DispatchOps) *claimHolder {
	h := &claimHolder{uid: claimOwner(cm), released: releasedRole(cm), claim: cm}
	if h.released != "" {
		h.uid = types.UID(cm.Annotations[releasedUIDAnnotation])
	} else if h.uid == cr.UID {
		return nil
	}
	if namespace, name, ok := strings.Cut(cm.Annotations[claimedByAnnotation], "/"); ok {
		h.namespace, h.name = namespace, name
	}
	return h
}

// claimNamespace claims ns for cr. It returns nil if cr holds the claim (it
// just made it, or had before), and otherwise who holds it, or released
// the tombstone there.
func (r *Reconciler) claimNamespace(ctx context.Context, cr *v1alpha1.DispatchOps, ns string) (*claimHolder, error) {
	for range 3 {
		cm, err := r.getClaim(ctx, ns)
		if err != nil {
			return nil, err
		}
		if cm != nil {
			return holderOf(cm, cr), nil
		}
		err = r.Create(ctx, claimFor(cr, ns))
		if err == nil {
			log.FromContext(ctx).Info("claimed namespace", "namespace", ns)
			return nil, nil
		}
		if apierrors.IsNotFound(err) {
			return nil, fmt.Errorf("claim in namespace %s: %w", ns, errNoNamespace)
		}
		if !apierrors.IsAlreadyExists(err) {
			return nil, fmt.Errorf("claim in namespace %s: %w", ns, err)
		}
		// Someone else's create came first: read whose.
	}
	return nil, fmt.Errorf("the claim in namespace %s keeps changing", ns)
}

// retake makes tombstone cm cr's claim again, unless it has changed since
// it was read (a conflict: read it again). Like a create, of installations
// retaking it at once exactly one gets it.
func (r *Reconciler) retake(ctx context.Context, cr *v1alpha1.DispatchOps, cm *corev1.ConfigMap) error {
	c := claimFor(cr, cm.Namespace)
	c.ResourceVersion = cm.ResourceVersion
	if err := r.Update(ctx, c); err != nil {
		return fmt.Errorf("claim in namespace %s: %w", cm.Namespace, err)
	}
	log.FromContext(ctx).Info("claimed released namespace", "namespace", cm.Namespace,
		"releasedBy", cm.Annotations[releasedUIDAnnotation], "role", releasedRole(cm))
	return nil
}

// releaseClaim turns claim cm into a tombstone for role, unless it has
// changed since it was read.
func (r *Reconciler) releaseClaim(ctx context.Context, cm *corev1.ConfigMap, role string) error {
	log.FromContext(ctx).Info("releasing namespace", "namespace", cm.Namespace, "role", role)
	t := cm.DeepCopy()
	t.Labels = maps.Clone(t.Labels)
	delete(t.Labels, ownerLabel)
	t.Labels[releasedLabel] = role
	t.Annotations = maps.Clone(t.Annotations)
	if t.Annotations == nil {
		t.Annotations = map[string]string{}
	}
	t.Annotations[releasedUIDAnnotation] = string(claimOwner(cm))
	t.Data = map[string]string{"README": tombstoneNote(cm.Namespace, role)}
	err := r.Update(ctx, t)
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("claim in namespace %s: %w", cm.Namespace, err)
	}
	return nil
}

// tombstoneNote explains a tombstone to whoever finds it.
func tombstoneNote(ns, role string) string {
	what := "This namespace was the job namespace of a Dispatch Ops installation, whose task manager could have left " +
		"Secrets, pods or Jobs here. The operator won't use it as another installation's job namespace, or as a " +
		"DispatchOps's namespace,"
	if role == roleControlPlane {
		what = "This namespace was the namespace of a deleted Dispatch Ops installation; its generated Secrets (its " +
			"encryption key among them) and database volume are kept here, and a DispatchOps of the same name " +
			"created here again takes them over. The operator won't use it as a job namespace"
	}
	return what + " until an administrator deletes ConfigMap " + ns + "/" + claimName + " after making sure the " +
		"namespace holds nothing left by the previous installation (or better, deletes and recreates the namespace).\n"
}

// claimOwnNamespace claims cr's own namespace: "" if cr holds it, and
// otherwise the reason and message to refuse it with. A deleted
// installation's namespace may be taken again, and a DispatchOps of the
// same name takes over its predecessor's kept Secrets first (see
// adoptKeptSecrets); a former job namespace may not.
func (r *Reconciler) claimOwnNamespace(ctx context.Context, cr *v1alpha1.DispatchOps, in install) (string, string, error) {
	logger := log.FromContext(ctx)
	for range 3 {
		holder, err := r.claimNamespace(ctx, cr, cr.Namespace)
		if err != nil || holder == nil {
			return "", "", err
		}
		switch holder.released {
		case "":
			logger.Info("refusing a namespace held by another installation", "holderUID", holder.uid)
			return ReasonNamespaceInUse, fmt.Sprintf("Namespace %s is held by %s. A namespace hosts only one Dispatch "+
				"Ops installation: create this one in a namespace of its own", cr.Namespace, holder.describe(cr)), nil
		case roleJobNamespace:
			logger.Info("refusing a former job namespace", "releasedBy", holder.uid)
			return ReasonNamespaceReleased, fmt.Sprintf("Namespace %s was the job namespace of a Dispatch Ops "+
				"installation, whose task manager could have left Secrets, pods or Jobs here for this one to use. "+
				"An administrator must delete ConfigMap %s/%s after making sure the namespace holds nothing left by "+
				"the previous installation (or better, delete and recreate the namespace); or create this "+
				"DispatchOps in a namespace of its own", cr.Namespace, cr.Namespace, claimName), nil
		}
		if holder.namespace == cr.Namespace && holder.name == cr.Name && holder.uid != "" {
			if err := r.adoptKeptSecrets(ctx, in, holder.uid); err != nil {
				return "", "", err
			}
		}
		err = r.retake(ctx, cr, holder.claim)
		if err == nil {
			return "", "", nil
		}
		if !apierrors.IsConflict(err) && !apierrors.IsNotFound(err) {
			return "", "", err
		}
		// Changed since read: read it again.
	}
	return "", "", fmt.Errorf("the claim in namespace %s keeps changing", cr.Namespace)
}

// recordJobNamespaces records namespaces as cr's job namespaces in its
// status at once (see v1alpha1.DispatchOpsStatus.JobNamespaces).
func (r *Reconciler) recordJobNamespaces(ctx context.Context, cr *v1alpha1.DispatchOps, namespaces []string) error {
	namespaces = slices.Compact(slices.Sorted(slices.Values(namespaces)))
	if slices.Equal(namespaces, cr.Status.JobNamespaces) {
		return nil
	}
	if len(namespaces) == 0 {
		namespaces = nil
	}
	cr.Status.JobNamespaces = namespaces
	if err := r.Status().Update(ctx, cr); err != nil {
		return fmt.Errorf("recording job namespaces in status: %w", err)
	}
	return nil
}

// ownClaim is cr's claim on its own namespace, or nil if cr doesn't hold
// it.
func (r *Reconciler) ownClaim(ctx context.Context, cr *v1alpha1.DispatchOps) (*corev1.ConfigMap, error) {
	cm, err := r.getClaim(ctx, cr.Namespace)
	if err != nil || cm == nil || claimOwner(cm) != cr.UID {
		return nil, err
	}
	return cm, nil
}

// claimJobNamespace claims in's job namespace, another than its own, for
// cr: it records it in cr's status first, so it is found again (see
// claimedNamespaces). It returns who holds it (or released it), if not cr.
// The installation that released a job namespace may take it back: nobody
// else could use it in between.
func (r *Reconciler) claimJobNamespace(ctx context.Context, cr *v1alpha1.DispatchOps, in install) (*claimHolder, error) {
	ns := in.jobNamespace()
	for range 3 {
		cm, err := r.getClaim(ctx, ns)
		if err != nil {
			return nil, err
		}
		var tomb *corev1.ConfigMap
		if cm != nil {
			switch h := holderOf(cm, cr); {
			case h == nil:
				// Held already, and recorded; again in case the record was
				// lost.
			case h.released == roleJobNamespace && h.uid == cr.UID:
				tomb = cm
			default:
				return h, nil // nothing to record
			}
		}
		if err := r.recordJobNamespaces(ctx, cr, append(slices.Clone(cr.Status.JobNamespaces), ns)); err != nil {
			return nil, err
		}
		if tomb == nil {
			return r.claimNamespace(ctx, cr, ns)
		}
		err = r.retake(ctx, cr, tomb)
		if err == nil {
			return nil, nil
		}
		if !apierrors.IsConflict(err) && !apierrors.IsNotFound(err) {
			return nil, err
		}
	}
	return nil, fmt.Errorf("the claim in namespace %s keeps changing", ns)
}

// mayAccess reports whether the operator has access to ns.
func (r *Reconciler) mayAccess(ns string) bool {
	return len(r.Namespaces) == 0 || slices.Contains(r.Namespaces, ns)
}

// claimedNamespaces are the namespaces other than its own that cr holds
// claims in, with the claims. It looks only where the operator or the
// administrator sent it: the namespaces its status records, its job
// namespace, and the allowed job namespaces mapped to its namespace. Never
// elsewhere, so a claim forged in some other namespace (with pods labeled
// for the installation, say) doesn't hold up its cleanup; and never by
// what tenants can write but the spec, so they can't make the operator
// forget a namespace.
func (r *Reconciler) claimedNamespaces(ctx context.Context, cr *v1alpha1.DispatchOps, in install) (map[string]*corev1.ConfigMap, error) {
	held := map[string]*corev1.ConfigMap{}
	candidates := append(slices.Clone(cr.Status.JobNamespaces), in.jobNamespace())
	for jobNS, sources := range r.AllowedJobNamespaces {
		if slices.Contains(sources, cr.Namespace) {
			candidates = append(candidates, jobNS)
		}
	}
	for _, ns := range candidates {
		if held[ns] != nil || ns == cr.Namespace || !r.mayAccess(ns) {
			continue
		}
		cm, err := r.getClaim(ctx, ns)
		if err != nil {
			return nil, err
		}
		if cm != nil && claimOwner(cm) == cr.UID {
			held[ns] = cm
		}
	}
	return held, nil
}

// releaseClaims turns cr's claims on the namespaces in held but those in
// keep into tombstones, and records the rest in cr's status: a namespace
// recorded there that cr holds no claim in any more is forgotten.
func (r *Reconciler) releaseClaims(ctx context.Context, cr *v1alpha1.DispatchOps, held map[string]*corev1.ConfigMap,
	keep map[string]bool) error {
	var still []string
	for _, ns := range slices.Sorted(maps.Keys(held)) {
		if keep[ns] {
			still = append(still, ns)
			continue
		}
		if err := r.releaseClaim(ctx, held[ns], roleJobNamespace); err != nil {
			return err
		}
	}
	return r.recordJobNamespaces(ctx, cr, still)
}
