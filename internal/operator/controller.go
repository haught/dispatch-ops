// Package operator is the Kubernetes operator that installs and upgrades
// Dispatch Ops from a DispatchOps resource.
package operator

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/netip"
	"net/url"
	"reflect"
	"slices"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/workqueue"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"dispatch-ops/internal/operator/api/v1alpha1"
	"dispatch-ops/internal/tlscert"
)

// Condition types reported in status.
const (
	CondDatabaseReady    = "DatabaseReady"
	CondMigrated         = "Migrated"
	CondAPIReady         = "APIReady"
	CondTaskManagerReady = "TaskManagerReady"
	CondReady            = "Ready"
	// CondDegraded is set (True) only while something needs an
	// administrator: the generated encryption key has gone missing.
	CondDegraded = "Degraded"
)

// Reasons the task manager isn't deployed for, besides rolling out.
const (
	// ReasonNamespaceInUse: another installation holds the DispatchOps's
	// namespace (see claim.go): a namespace hosts one installation. The
	// whole installation is refused.
	ReasonNamespaceInUse = "NamespaceInUse"
	// ReasonJobNamespaceInUse: the job namespace is another installation's
	// (see Reconciler.holdJobNamespace). The whole installation is refused.
	ReasonJobNamespaceInUse = "JobNamespaceInUse"
	// ReasonJobNamespaceNotAllowed: the job namespace isn't opted in.
	ReasonJobNamespaceNotAllowed = "JobNamespaceNotAllowed"
	// ReasonJobNamespacePodSecurity: the job namespace doesn't enforce the
	// restricted (or baseline) Pod Security Standard.
	ReasonJobNamespacePodSecurity = "JobNamespacePodSecurity"
	// ReasonEncryptionKeyMissing: the encryption key Secret the operator
	// generated is gone, and it won't make another (see
	// ensureEncryptionKey).
	ReasonEncryptionKeyMissing = "EncryptionKeyMissing"
	// ReasonNamespaceReleased: the DispatchOps's namespace was another
	// installation's job namespace (a tombstone; see claim.go). The whole
	// installation is refused.
	ReasonNamespaceReleased = "NamespaceReleased"
	// ReasonJobNamespaceReleased: the job namespace was another
	// installation's job namespace, or a deleted installation's own
	// namespace (a tombstone; see claim.go). The whole installation is
	// refused.
	ReasonJobNamespaceReleased = "JobNamespaceReleased"
	// ReasonForeignSecret: on a first install, a Secret the operator would
	// generate already exists, and it didn't generate it for this
	// installation (see checkGeneratedSecrets).
	ReasonForeignSecret = "ForeignSecret"
)

const (
	finalizer = "dispatch-ops.io/cleanup"
	// specHashAnnotation records what the operator last applied to an
	// object, so it only writes when that changes.
	specHashAnnotation = "dispatch-ops.io/spec-hash"
	// ownerLabel marks objects outside the DispatchOps namespace, which
	// can't have owner references, with the owner's UID.
	ownerLabel = "dispatch-ops.io/owner-uid"
	// pollInterval re-checks an installation that isn't ready, for changes
	// the operator doesn't watch (such as Secrets it doesn't own).
	pollInterval = 30 * time.Second
	// cleanupPoll re-checks sooner while an installation's job pods are
	// still stopping in a namespace it is leaving (see leaveJobNamespaces).
	cleanupPoll = 5 * time.Second
)

// Reconciler reconciles DispatchOps resources.
type Reconciler struct {
	client.Client
	// APIReader reads without the cache, for objects the operator doesn't
	// watch.
	APIReader client.Reader
	Scheme    *runtime.Scheme
	// DefaultImage is the server image used when a DispatchOps names none:
	// the one this operator was released with.
	DefaultImage string
	// AllowedJobNamespaces maps each namespace a DispatchOps may run jobs
	// in, besides its own, to the namespaces whose DispatchOps may. Each
	// serves one installation at a time: the one holding its claim (see
	// claim.go).
	AllowedJobNamespaces map[string][]string
	// DefaultJobNamespaces maps a namespace to the job namespace its
	// DispatchOps run their jobs in when spec.taskManager.jobNamespace is
	// unset (see withDefaults); without an entry, their own namespace.
	// Each is also in AllowedJobNamespaces for that namespace.
	DefaultJobNamespaces map[string]string
	// AllowJobNamespacesWithoutPodSecurity runs jobs in namespaces that
	// don't enforce the restricted or baseline Pod Security Standard. By
	// default the operator refuses them: whoever can create a DispatchOps
	// chooses the task manager's image, which can create any pod there.
	AllowJobNamespacesWithoutPodSecurity bool
	// JobDNS is where job pods may send DNS queries; nil means
	// DefaultJobDNS.
	JobDNS *JobDNS
	// Namespaces are the only namespaces the operator may read and write
	// (the ones it watches, and AllowedJobNamespaces). Empty means it has
	// access to every namespace; only then does it look for
	// AllowJobsFromLabel on a job namespace.
	Namespaces []string
}

// readyRecheck is how often a ready installation is reconciled anyway: the
// operator doesn't watch namespaces, so this is how soon it notices that a
// job namespace no longer allows it (its opt-in label or its Pod Security
// label removed), and when generated certificates get renewed (well within
// renewBefore of expiry). A reconcile that changes nothing costs a few
// reads, mostly from the cache.
const readyRecheck = 5 * time.Minute

// SetupWithManager registers the reconciler and what it watches.
//
// It reconciles one DispatchOps at a time (MaxConcurrentReconciles: 1, the
// default, set explicitly because the operator relies on it). Which
// installation holds a namespace is decided by the create-only claims,
// which hold whatever the concurrency, between operator releases too. But
// the checks before a claim and a grant (no DispatchOps in the job
// namespace, no other installation's job objects there) are
// check-then-act: with one reconcile at a time, no other reconcile of
// this operator grants anything in between. Raise it only after making those atomic.
func (r *Reconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&v1alpha1.DispatchOps{}).
		WithOptions(controller.Options{MaxConcurrentReconciles: 1}).
		// Another DispatchOps moving into, out of or away from a job
		// namespace, or created in one: reconcile at once every one that
		// runs its jobs there, so one waiting for it tries again, and one
		// whose job namespace now hosts a DispatchOps leaves it.
		Watches(&v1alpha1.DispatchOps{}, handler.Funcs{
			CreateFunc: func(ctx context.Context, e event.CreateEvent, q workqueue.TypedRateLimitingInterface[reconcile.Request]) {
				r.enqueueJobNamespaceUsers(ctx, q, e.Object)
			},
			UpdateFunc: func(ctx context.Context, e event.UpdateEvent, q workqueue.TypedRateLimitingInterface[reconcile.Request]) {
				o, n := e.ObjectOld.(*v1alpha1.DispatchOps), e.ObjectNew.(*v1alpha1.DispatchOps)
				if (install{DispatchOps: r.withDefaults(o)}).jobNamespace() != (install{DispatchOps: r.withDefaults(n)}).jobNamespace() ||
					o.DeletionTimestamp.IsZero() != n.DeletionTimestamp.IsZero() {
					r.enqueueJobNamespaceUsers(ctx, q, o, n)
				}
			},
			DeleteFunc: func(ctx context.Context, e event.DeleteEvent, q workqueue.TypedRateLimitingInterface[reconcile.Request]) {
				r.enqueueJobNamespaceUsers(ctx, q, e.Object)
			},
		}).
		Owns(&appsv1.Deployment{}).
		Owns(&appsv1.StatefulSet{}).
		Owns(&batchv1.Job{}).
		Owns(&corev1.Service{}).
		Owns(&corev1.ServiceAccount{}).
		Owns(&networkingv1.Ingress{}).
		Owns(&networkingv1.NetworkPolicy{}).
		Named("dispatchops").
		Complete(r)
}

// withDefaults fills in what the CRD's defaults would, so a resource that
// skipped them (or an older one) still works, and the job namespace from
// DefaultJobNamespaces.
func (r *Reconciler) withDefaults(d *v1alpha1.DispatchOps) *v1alpha1.DispatchOps {
	d = d.DeepCopy()
	s := &d.Spec
	if s.TaskManager.JobNamespace == "" {
		s.TaskManager.JobNamespace = r.DefaultJobNamespaces[d.Namespace]
	}
	if s.ImagePullPolicy == "" {
		s.ImagePullPolicy = corev1.PullIfNotPresent
	}
	if s.API.Replicas == nil {
		s.API.Replicas = new(int32(2))
	}
	if s.TaskManager.Replicas == nil {
		s.TaskManager.Replicas = new(int32(1))
	}
	if s.Database.External == nil && s.Database.Managed == nil {
		s.Database.Managed = &v1alpha1.ManagedDatabase{}
	}
	if s.Admin.Username == "" {
		s.Admin.Username = "admin"
	}
	if s.LogLevel == "" {
		s.LogLevel = "info"
	}
	return d
}

// Reconcile brings one installation towards its spec: secrets, database,
// migrations for the wanted image, then the API server and task manager on
// the migrated image.
func (r *Reconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var cr v1alpha1.DispatchOps
	if err := r.Get(ctx, req.NamespacedName, &cr); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !cr.DeletionTimestamp.IsZero() {
		return r.finalize(ctx, &cr)
	}
	if controllerutil.AddFinalizer(&cr, finalizer) {
		if err := r.Update(ctx, &cr); err != nil {
			return ctrl.Result{}, err
		}
	}

	in := install{DispatchOps: r.withDefaults(&cr), image: cr.Status.Image}
	status := cr.Status.DeepCopy()
	status.ObservedGeneration = cr.Generation
	set := func(t string, ok bool, reason, msg string) {
		s := metav1.ConditionFalse
		if ok {
			s = metav1.ConditionTrue
		}
		meta.SetStatusCondition(&status.Conditions, metav1.Condition{
			Type: t, Status: s, Reason: reason, Message: msg, ObservedGeneration: cr.Generation,
		})
	}

	// A namespace hosts one installation: claim it before making anything
	// in it, the encryption key and database included.
	reason, refusal, err := r.claimOwnNamespace(ctx, &cr, in)
	if err != nil {
		return ctrl.Result{}, err
	}
	if refusal != "" {
		return r.refuse(ctx, &cr, in, status, set, reason, refusal)
	}

	// A job namespace serves one installation: the task manager can create
	// pods there, and a pod can mount any Secret in its namespace. Claim it
	// before granting anything there: reconcileServers grants only in
	// in.heldJobNamespace. (One it may not use is refused there.)
	ok, _, err := r.mayUseJobNamespace(ctx, in)
	if err == nil && ok {
		reason, refusal, err = r.holdJobNamespace(ctx, &cr, in)
		if err == nil && refusal == "" {
			in.heldJobNamespace = in.jobNamespace()
		}
		if errors.Is(err, errNoNamespace) {
			err = nil // refused in reconcileServers
		}
	}
	if err == nil && refusal != "" {
		return r.refuse(ctx, &cr, in, status, set, reason, refusal)
	}
	var pending bool
	if err == nil {
		pending, err = r.reconcile(ctx, &cr, in, status, set)
	}
	keyErr, foreignErr := (*encryptionKeyMissingError)(nil), (*foreignSecretError)(nil)
	if errors.As(err, &keyErr) {
		set(CondDegraded, true, ReasonEncryptionKeyMissing, keyErr.Error())
		set(CondReady, false, ReasonEncryptionKeyMissing, keyErr.Error())
		err = nil
	} else if errors.As(err, &foreignErr) {
		set(CondReady, false, ReasonForeignSecret, foreignErr.Error())
		err = nil
	} else if err != nil {
		set(CondReady, false, "Error", err.Error())
	} else {
		meta.RemoveStatusCondition(&status.Conditions, CondDegraded)
		ready := true
		for _, t := range []string{CondDatabaseReady, CondMigrated, CondAPIReady, CondTaskManagerReady} {
			if !meta.IsStatusConditionTrue(status.Conditions, t) {
				ready = false
				c := meta.FindStatusCondition(status.Conditions, t)
				set(CondReady, false, c.Reason, t+": "+c.Message)
				break
			}
		}
		if ready {
			set(CondReady, true, "Ready", "Dispatch Ops is running "+status.Image)
		}
	}

	// The claims keep cr.Status.JobNamespaces up to date as they go.
	status.JobNamespaces = cr.Status.JobNamespaces
	if !reflect.DeepEqual(status, &cr.Status) {
		cr.Status = *status
		if uerr := r.Status().Update(ctx, &cr); uerr != nil {
			return ctrl.Result{}, errors.Join(err, uerr)
		}
	}
	switch {
	case err != nil:
		return ctrl.Result{}, err
	case pending:
		return ctrl.Result{RequeueAfter: cleanupPoll}, nil
	case !meta.IsStatusConditionTrue(status.Conditions, CondReady):
		return ctrl.Result{RequeueAfter: pollInterval}, nil
	}
	return ctrl.Result{RequeueAfter: readyRecheck}, nil
}

type setCondition func(t string, ok bool, reason, msg string)

// refuse refuses cr's installation outright, for reason: it takes away its
// task manager and whatever it has in job namespaces (see
// revokeJobAccess), and reports why in status.
func (r *Reconciler) refuse(ctx context.Context, cr *v1alpha1.DispatchOps, in install, status *v1alpha1.DispatchOpsStatus,
	set setCondition, reason, msg string) (ctrl.Result, error) {
	pending, err := r.revokeJobAccess(ctx, cr, in)
	set(CondTaskManagerReady, false, reason, msg)
	set(CondReady, false, reason, msg)
	status.JobNamespaces = cr.Status.JobNamespaces
	if !reflect.DeepEqual(status, &cr.Status) {
		cr.Status = *status
		if uerr := r.Status().Update(ctx, cr); uerr != nil {
			return ctrl.Result{}, errors.Join(err, uerr)
		}
	}
	if pending {
		return ctrl.Result{RequeueAfter: cleanupPoll}, err
	}
	return ctrl.Result{RequeueAfter: pollInterval}, err
}

// reconcile does the work of Reconcile for an installation that may have
// its job namespace. pending reports that its job pods are still stopping
// in a namespace it left (see leaveJobNamespaces).
func (r *Reconciler) reconcile(ctx context.Context, cr *v1alpha1.DispatchOps, in install,
	status *v1alpha1.DispatchOpsStatus, set setCondition) (pending bool, err error) {
	if err := r.ensureSecrets(ctx, in); err != nil {
		return false, err
	}
	if in.Spec.EncryptionKeySecret == nil {
		status.EncryptionKeyGenerated = true
	}

	dbReady, err := r.reconcileDatabase(ctx, cr, &in, set)
	if err != nil {
		return false, err
	}

	want := in.Spec.Image
	if want == "" {
		want = r.DefaultImage
	}
	if status.Image != want {
		done, err := r.migrate(ctx, cr, in, want, dbReady, set)
		if err != nil {
			return false, err
		}
		if done {
			status.Image = want
			in.image = want
		}
	} else {
		set(CondMigrated, true, "UpToDate", "The database schema matches "+want)
	}

	if in.image == "" {
		set(CondAPIReady, false, "WaitingForMigration", "Waiting for the first database migration")
		set(CondTaskManagerReady, false, "WaitingForMigration", "Waiting for the first database migration")
		return false, nil
	}
	return r.reconcileServers(ctx, cr, in, set)
}

// --- Secrets ---------------------------------------------------------------------

// randomString returns n random bytes, hex-encoded (safe in URLs).
func randomString(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// ensureSecrets creates the Secrets the operator generates if they don't
// exist, and renews the certificates among them. They have no owner, so
// deleting the DispatchOps keeps them.
func (r *Reconciler) ensureSecrets(ctx context.Context, in install) error {
	if err := r.checkGeneratedSecrets(ctx, in); err != nil {
		return err
	}
	if in.Spec.EncryptionKeySecret == nil {
		if err := r.ensureEncryptionKey(ctx, in); err != nil {
			return err
		}
	}
	var want []*corev1.Secret
	// The administrator's initial password is only needed until the first
	// migration has created them; after that the Secret may be deleted (as
	// the README recommends) and isn't made again.
	if in.Spec.Admin.PasswordSecret == nil && in.Status.Image == "" {
		sel := in.adminPasswordSecret()
		want = append(want, in.generatedSecret("admin", sel.Name,
			map[string]string{"username": in.Spec.Admin.Username, sel.Key: randomString(12)}))
	}
	if in.Spec.Database.Managed != nil {
		pw := randomString(16)
		want = append(want, in.generatedSecret(componentDatabase, in.name(componentDatabase),
			map[string]string{"password": pw, "url": in.postgresURL(pw)}))
	}
	for _, s := range want {
		if err := r.ensureSecret(ctx, s.Name, in.Namespace, func() (*corev1.Secret, error) { return s, nil }); err != nil {
			return err
		}
	}
	// Making a certificate costs more than the others, so only when needed.
	if in.tlsEnabled() && in.Spec.API.TLS.SecretName == "" {
		if err := r.ensureCert(ctx, in.apiTLSSecretName(), in.Namespace, in.apiCertHosts(), in.generatedTLSSecret, nil); err != nil {
			return err
		}
	}
	if in.Spec.Database.Managed != nil {
		if err := r.ensurePostgresTLS(ctx, in); err != nil {
			return err
		}
	}
	return nil
}

// foreignSecretError reports a Secret that a first install would use as
// one the operator generated, but that it didn't generate for this
// installation.
type foreignSecretError struct {
	name string
	uid  types.UID
}

func (e *foreignSecretError) Error() string {
	return fmt.Sprintf("Secret %s already exists, but the operator didn't generate it for this DispatchOps (it isn't "+
		"labeled %s=%s). Anyone who could create Secrets in this namespace (a task manager whose job namespace it "+
		"was, say) could have left it, with values they know, so the operator won't use it. Delete it, and the "+
		"operator generates a new one; or, if you made it for this installation, label it %s=%s",
		e.name, ownerLabel, e.uid, ownerLabel, e.uid)
}

// checkGeneratedSecrets makes sure the Secrets the operator generates for
// in, where they exist already, are its own: labeled with its UID. On a
// first install (it has no status.encryptionKeyGenerated or status.image
// yet) another is a foreignSecretError: the operator would otherwise take
// it as the one it generated, and use a key, password or certificate that
// someone else knows. (A kept Secret of a deleted DispatchOps of the same
// name was relabeled when its namespace was claimed; see
// adoptKeptSecrets.) Once installed, an installation uses the Secrets it
// has; one generated by an operator from before the label (with
// generatedAnnotation but no ownerLabel) gets it.
func (r *Reconciler) checkGeneratedSecrets(ctx context.Context, in install) error {
	established := in.Status.EncryptionKeyGenerated || in.Status.Image != ""
	for _, name := range in.generatedSecretNames() {
		var s corev1.Secret
		err := r.APIReader.Get(ctx, types.NamespacedName{Namespace: in.Namespace, Name: name}, &s)
		if apierrors.IsNotFound(err) {
			continue
		}
		if err != nil {
			return fmt.Errorf("secret %s: %w", name, err)
		}
		owner, has := s.Labels[ownerLabel]
		switch {
		case owner == string(in.UID):
		case !established:
			log.FromContext(ctx).Info("refusing a Secret the operator didn't generate for this installation", "secret", name,
				"ownerUID", owner)
			return &foreignSecretError{name: name, uid: in.UID}
		case !has && s.Annotations[generatedAnnotation] == "true":
			if err := r.relabelSecret(ctx, &s, in.UID); err != nil {
				return err
			}
		}
	}
	return nil
}

// adoptKeptSecrets takes over, for in, the Secrets that the deleted
// installation formerUID, a DispatchOps of the same name in the same
// namespace, generated and left there (see claimOwnNamespace): those of
// its generated names labeled with formerUID get in's UID instead. Others
// stay as they are (and are refused by checkGeneratedSecrets).
func (r *Reconciler) adoptKeptSecrets(ctx context.Context, in install, formerUID types.UID) error {
	for _, name := range in.keptSecretNames() {
		var s corev1.Secret
		err := r.APIReader.Get(ctx, types.NamespacedName{Namespace: in.Namespace, Name: name}, &s)
		if apierrors.IsNotFound(err) {
			continue
		}
		if err != nil {
			return fmt.Errorf("secret %s: %w", name, err)
		}
		if s.Labels[ownerLabel] != string(formerUID) {
			continue
		}
		log.FromContext(ctx).Info("taking over a kept Secret of the deleted installation", "secret", name, "formerUID", formerUID)
		if err := r.relabelSecret(ctx, &s, in.UID); err != nil {
			return err
		}
	}
	return nil
}

// relabelSecret labels s with uid (ownerLabel).
func (r *Reconciler) relabelSecret(ctx context.Context, s *corev1.Secret, uid types.UID) error {
	s.Labels = maps.Clone(s.Labels)
	if s.Labels == nil {
		s.Labels = map[string]string{}
	}
	s.Labels[ownerLabel] = string(uid)
	if err := r.Update(ctx, s); err != nil {
		return fmt.Errorf("secret %s: %w", s.Name, err)
	}
	return nil
}

// encryptionKeyMissingError reports a generated encryption key Secret that
// has gone missing.
type encryptionKeyMissingError struct{ name string }

func (e *encryptionKeyMissingError) Error() string {
	return fmt.Sprintf("The encryption key Secret %s, which the operator generated, is missing. The database may hold "+
		"credentials encrypted with that key, so the operator won't generate a new one, which would leave them unreadable: "+
		"restore the Secret from your backup. To start over without the stored credentials, create the Secret "+
		"yourself, with a new key (dispatch-ops gen-key) as \"key\"", e.name)
}

// ensureEncryptionKey generates the encryption key Secret, the first time
// only. Once the installation has had one (status.encryptionKeyGenerated,
// or for one from before that field, a migrated database), a missing
// Secret is an encryptionKeyMissingError instead: a new key would make the
// stored credentials unreadable, whoever deleted the old one.
func (r *Reconciler) ensureEncryptionKey(ctx context.Context, in install) error {
	name := in.encryptionKeySecretName()
	var existing corev1.Secret
	err := r.APIReader.Get(ctx, types.NamespacedName{Namespace: in.Namespace, Name: name}, &existing)
	switch {
	case err == nil:
		return nil
	case !apierrors.IsNotFound(err):
		return fmt.Errorf("secret %s: %w", name, err)
	case in.Status.EncryptionKeyGenerated || in.Status.Image != "":
		return &encryptionKeyMissingError{name: name}
	}
	key := make([]byte, 32)
	_, _ = rand.Read(key)
	s := in.generatedSecret("encryption-key", name, map[string]string{"key": base64.StdEncoding.EncodeToString(key)})
	return r.ensureSecret(ctx, name, in.Namespace, func() (*corev1.Secret, error) { return s, nil })
}

// ensurePostgresTLS creates the managed database's CA and the certificate
// it serves, issued by that CA, and renews them: the certificate as any
// other (see ensureCert), and when the CA isn't its issuer (a new CA, or a
// self-signed one from an older operator); the CA only near its expiry,
// years from now. The servers trust the CA, so they keep reaching the
// database when its certificate is renewed.
func (r *Reconciler) ensurePostgresTLS(ctx context.Context, in install) error {
	name := in.postgresCASecretName()
	if err := r.ensureSecret(ctx, name, in.Namespace, in.generatedPostgresCASecret); err != nil {
		return err
	}
	var ca corev1.Secret
	if err := r.APIReader.Get(ctx, types.NamespacedName{Namespace: in.Namespace, Name: name}, &ca); err != nil {
		return fmt.Errorf("secret %s: %w", name, err)
	}
	if ca.Annotations[generatedAnnotation] == "true" {
		cert, err := tlscert.ParseCertificate(ca.Data[caCertKey])
		if err != nil || len(ca.Data[caKeyKey]) == 0 || time.Until(cert.NotAfter) < postgresCARenewBefore {
			fresh, err := in.generatedPostgresCASecret()
			if err != nil {
				return err
			}
			log.FromContext(ctx).Info("renewing generated CA", "secret", name)
			ca.Data = fresh.Data
			if err := r.Update(ctx, &ca); err != nil {
				return fmt.Errorf("secret %s: %w", name, err)
			}
		}
	}
	return r.ensureCert(ctx, in.postgresTLSSecretName(), in.Namespace, in.postgresCertHosts(),
		func() (*corev1.Secret, error) { return in.postgresTLSSecret(&ca) },
		func(s *corev1.Secret) bool { return !issuedBy(s, ca.Data[caCertKey]) })
}

// ensureCert creates a generated certificate Secret, or renews it when it
// nears expiry or no longer covers hosts (see certNeedsRenewal). The API
// server reloads its certificate files when they change, and the task
// manager reads the API's for each job; the managed database's pod is
// restarted through its certificate annotation.
// stale, if not nil, also renews the certificate when it returns true.
func (r *Reconciler) ensureCert(ctx context.Context, name, namespace string, hosts []string,
	generate func() (*corev1.Secret, error), stale func(*corev1.Secret) bool) error {
	var existing corev1.Secret
	err := r.APIReader.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, &existing)
	if apierrors.IsNotFound(err) {
		return r.ensureSecret(ctx, name, namespace, generate)
	}
	if err != nil {
		return fmt.Errorf("secret %s: %w", name, err)
	}
	if !certNeedsRenewal(&existing, hosts, time.Now()) &&
		(stale == nil || existing.Annotations[generatedAnnotation] != "true" || !stale(&existing)) {
		return nil
	}
	fresh, err := generate()
	if err != nil {
		return err
	}
	log.FromContext(ctx).Info("renewing generated certificate", "secret", name)
	existing.Data = fresh.Data
	if err := r.Update(ctx, &existing); err != nil {
		return fmt.Errorf("secret %s: %w", name, err)
	}
	return nil
}

// ensureSecret creates the Secret that generate returns, unless one with its
// name exists.
func (r *Reconciler) ensureSecret(ctx context.Context, name, namespace string, generate func() (*corev1.Secret, error)) error {
	var existing corev1.Secret
	err := r.APIReader.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, &existing)
	if apierrors.IsNotFound(err) {
		var s *corev1.Secret
		if s, err = generate(); err == nil {
			log.FromContext(ctx).Info("generating secret", "secret", name)
			err = r.Create(ctx, s)
		}
	}
	if err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("secret %s: %w", name, err)
	}
	return nil
}

// --- Database --------------------------------------------------------------------

func (r *Reconciler) reconcileDatabase(ctx context.Context, cr *v1alpha1.DispatchOps, in *install, set setCondition) (bool, error) {
	if ext := in.Spec.Database.External; ext != nil {
		sel := ext.URLSecret
		var s corev1.Secret
		err := r.APIReader.Get(ctx, types.NamespacedName{Namespace: in.Namespace, Name: sel.Name}, &s)
		switch {
		case apierrors.IsNotFound(err):
			set(CondDatabaseReady, false, "SecretMissing", fmt.Sprintf("Secret %s does not exist", sel.Name))
			return false, nil
		case err != nil:
			return false, err
		case len(s.Data[sel.Key]) == 0:
			set(CondDatabaseReady, false, "SecretMissing", fmt.Sprintf("Secret %s has no %q", sel.Name, sel.Key))
			return false, nil
		}
		set(CondDatabaseReady, true, "External", "Using the database in Secret "+sel.Name)
		return true, nil
	}

	if err := r.applyNetworkPolicy(ctx, cr, *in, in.postgresNetworkPolicy()); err != nil {
		return false, err
	}
	var cert corev1.Secret
	if err := r.APIReader.Get(ctx, types.NamespacedName{Namespace: in.Namespace, Name: in.postgresTLSSecretName()}, &cert); err != nil {
		return false, fmt.Errorf("secret %s: %w", in.postgresTLSSecretName(), err)
	}
	in.postgresCertHash = certHash(&cert)
	var ca corev1.Secret
	if err := r.APIReader.Get(ctx, types.NamespacedName{Namespace: in.Namespace, Name: in.postgresCASecretName()}, &ca); err != nil {
		return false, fmt.Errorf("secret %s: %w", in.postgresCASecretName(), err)
	}
	in.postgresCAHash = hash16(ca.Data[caCertKey])
	if err := r.apply(ctx, cr, in.postgresService()); err != nil {
		return false, err
	}
	sts := in.postgresStatefulSet()
	if err := r.apply(ctx, cr, sts); err != nil {
		return false, err
	}
	mode, err := r.requireDatabaseTLS(ctx, *in, sts)
	if err != nil {
		return false, err
	}
	in.dbSSLMode = mode
	if sts.Status.ReadyReplicas < 1 {
		set(CondDatabaseReady, false, "Starting", "Waiting for PostgreSQL ("+sts.Name+") to be ready")
		return false, nil
	}
	set(CondDatabaseReady, true, "Managed", "PostgreSQL is running in "+sts.Name)
	return true, nil
}

// requireDatabaseTLS switches the managed database's URL, which older
// operators generated with sslmode=disable or require, to postgresSSLMode
// (verify-full, with the CA the server pods mount): from disable once
// PostgreSQL has rolled out serving TLS. It returns the URL's sslmode,
// which restarts the server pods when it changes (dbSSLModeAnnotation).
func (r *Reconciler) requireDatabaseTLS(ctx context.Context, in install, sts *appsv1.StatefulSet) (string, error) {
	var s corev1.Secret
	name := in.name(componentDatabase)
	if err := r.APIReader.Get(ctx, types.NamespacedName{Namespace: in.Namespace, Name: name}, &s); err != nil {
		return "", fmt.Errorf("secret %s: %w", name, err)
	}
	u, err := url.Parse(string(s.Data["url"]))
	if err != nil {
		return "", fmt.Errorf("secret %s: the url doesn't parse", name)
	}
	q := u.Query()
	mode := q.Get("sslmode")
	st := sts.Status
	rolledOut := st.ObservedGeneration >= sts.Generation && st.UpdatedReplicas >= 1 && st.ReadyReplicas >= 1 &&
		(st.UpdateRevision == "" || st.UpdateRevision == st.CurrentRevision)
	if s.Annotations[generatedAnnotation] != "true" || (mode != "disable" && mode != "require") ||
		(mode == "disable" && !rolledOut) {
		return mode, nil
	}
	for k, v := range postgresTLSQuery() {
		q[k] = v
	}
	u.RawQuery = q.Encode()
	s.Data["url"] = []byte(u.String())
	log.FromContext(ctx).Info("verifying TLS to the managed database", "secret", name, "sslmode", postgresSSLMode)
	if err := r.Update(ctx, &s); err != nil {
		return "", fmt.Errorf("secret %s: %w", name, err)
	}
	return postgresSSLMode, nil
}

// applyNetworkPolicy applies np in the DispatchOps namespace, or deletes it
// when NetworkPolicies are off.
func (r *Reconciler) applyNetworkPolicy(ctx context.Context, cr *v1alpha1.DispatchOps, in install, np *networkingv1.NetworkPolicy) error {
	if in.networkPoliciesEnabled() {
		return r.apply(ctx, cr, np)
	}
	return r.deleteIfOwned(ctx, cr, &networkingv1.NetworkPolicy{ObjectMeta: metav1.ObjectMeta{Name: np.Name, Namespace: np.Namespace}})
}

// --- Migrations ------------------------------------------------------------------

// migrate runs image's migrations in a Job and reports whether they are
// done. The API server and task manager keep running the previous image
// until then.
func (r *Reconciler) migrate(ctx context.Context, cr *v1alpha1.DispatchOps, in install, image string,
	dbReady bool, set setCondition) (bool, error) {
	if !dbReady {
		set(CondMigrated, false, "WaitingForDatabase", "Migrations for "+image+" wait for the database")
		return false, nil
	}
	if cr.Status.Image != "" {
		// An upgrade: the administrator was created by an earlier
		// migration. If their password Secret has since been deleted,
		// migrate without it rather than wait for a Secret that won't come.
		sel := in.adminPasswordSecret()
		var s corev1.Secret
		err := r.APIReader.Get(ctx, types.NamespacedName{Namespace: in.Namespace, Name: sel.Name}, &s)
		switch {
		case apierrors.IsNotFound(err):
			in.skipAdmin = true
		case err != nil:
			return false, fmt.Errorf("secret %s: %w", sel.Name, err)
		}
	}
	job := in.migrationJob(image)
	var existing batchv1.Job
	err := r.Get(ctx, client.ObjectKeyFromObject(job), &existing)
	if apierrors.IsNotFound(err) {
		if err := controllerutil.SetControllerReference(cr, job, r.Scheme); err != nil {
			return false, err
		}
		log.FromContext(ctx).Info("migrating the database", "image", image, "job", job.Name)
		if err := r.Create(ctx, job); err != nil && !apierrors.IsAlreadyExists(err) {
			return false, err
		}
		set(CondMigrated, false, "Migrating", "Running migrations for "+image+" in Job "+job.Name)
		return false, nil
	}
	if err != nil {
		return false, err
	}
	for _, c := range existing.Status.Conditions {
		if c.Status != corev1.ConditionTrue {
			continue
		}
		switch c.Type {
		case batchv1.JobComplete:
			log.FromContext(ctx).Info("database migrated", "image", image)
			set(CondMigrated, true, "Migrated", "Migrations for "+image+" completed")
			return true, nil
		case batchv1.JobFailed:
			set(CondMigrated, false, "MigrationFailed", fmt.Sprintf(
				"Job %s failed: %s. See `kubectl logs job/%s --all-containers`; delete the Job to retry",
				job.Name, c.Message, job.Name))
			return false, nil
		}
	}
	set(CondMigrated, false, "Migrating", "Running migrations for "+image+" in Job "+job.Name)
	return false, nil
}

// --- API server and task manager ---------------------------------------------------

func (r *Reconciler) reconcileServers(ctx context.Context, cr *v1alpha1.DispatchOps, in install, set setCondition) (bool, error) {
	// Caught here rather than by an API server that won't start.
	for _, p := range in.Spec.API.TrustedProxies {
		if _, err := netip.ParsePrefix(p); err != nil {
			if _, err := netip.ParseAddr(p); err != nil {
				return false, fmt.Errorf("api.trustedProxies: %q is not an address or CIDR", p)
			}
		}
	}
	for _, c := range []string{componentAPI, componentTaskManager} {
		if err := r.applyNetworkPolicy(ctx, cr, in, in.serverNetworkPolicy(c)); err != nil {
			return false, err
		}
	}
	for _, o := range []client.Object{in.apiService(), in.taskManagerServiceAccount()} {
		if err := r.apply(ctx, cr, o); err != nil {
			return false, err
		}
	}

	refusal, why, err := r.jobNamespaceAllowed(ctx, in)
	if err != nil {
		return false, err
	}
	if refusal == "" && in.heldJobNamespace != in.jobNamespace() {
		// Never granted without the claim; it isn't held only when the
		// namespace is missing.
		refusal, why = ReasonJobNamespaceNotAllowed, fmt.Sprintf("Job namespace %s does not exist", in.jobNamespace())
	}
	allowed := refusal == ""
	var keep []client.Object
	keepNS := ""
	if allowed {
		keepNS = in.jobNamespace()
		// The NetworkPolicy first, so no job pod starts without it.
		if in.networkPoliciesEnabled() {
			np, err := in.jobNetworkPolicy(r.JobDNS)
			if err != nil {
				return false, err
			}
			keep = append(keep, np)
		}
		keep = append(keep, in.taskManagerRole(), in.taskManagerRoleBinding())
		for _, o := range keep {
			if err := r.apply(ctx, cr, o); err != nil {
				return false, err
			}
		}
	}
	// Everything elsewhere, and if not allowed, there too.
	pending, err := r.leaveJobNamespaces(ctx, cr, in, keepNS, in.heldJobNamespace, keep)
	if err != nil {
		return false, err
	}

	if in.Spec.API.Ingress != nil {
		if err := r.apply(ctx, cr, in.ingress()); err != nil {
			return false, err
		}
	} else if err := r.deleteIfOwned(ctx, cr, &networkingv1.Ingress{ObjectMeta: metav1.ObjectMeta{Name: in.Name, Namespace: in.Namespace}}); err != nil {
		return false, err
	}

	deps := []struct {
		dep  *appsv1.Deployment
		cond string
	}{{in.apiDeployment(), CondAPIReady}}
	if allowed {
		deps = append(deps, struct {
			dep  *appsv1.Deployment
			cond string
		}{in.taskManagerDeployment(), CondTaskManagerReady})
	} else {
		// Without its Role there, the task manager couldn't run jobs; an
		// existing one is left as it is, and can't either.
		set(CondTaskManagerReady, false, refusal, why)
	}
	for _, d := range deps {
		if err := r.apply(ctx, cr, d.dep); err != nil {
			return false, err
		}
		ok, msg := deploymentReady(d.dep)
		reason := "Available"
		if !ok {
			reason = "RollingOut"
		}
		set(d.cond, ok, reason, msg)
	}
	return pending, nil
}

func deploymentReady(d *appsv1.Deployment) (bool, string) {
	want := int32(1)
	if d.Spec.Replicas != nil {
		want = *d.Spec.Replicas
	}
	s := d.Status
	if s.ObservedGeneration < d.Generation || s.UpdatedReplicas < want || s.AvailableReplicas < want {
		return false, fmt.Sprintf("%s: %d of %d replicas updated and available", d.Name, min(s.UpdatedReplicas, s.AvailableReplicas), want)
	}
	return true, fmt.Sprintf("%s: %d replicas available", d.Name, want)
}

// --- Applying objects --------------------------------------------------------------

// apply creates obj, or replaces the existing object's labels, annotations
// and spec with obj's when they differ from what the operator last
// applied. Objects in the DispatchOps namespace are owned by it; others
// are labeled with its UID. Afterwards obj is the object as the API server
// has it (or the cache, when nothing changed), so its status can be read:
// right after an update the cache may still hold the old generation.
func (r *Reconciler) apply(ctx context.Context, cr *v1alpha1.DispatchOps, obj client.Object) error {
	if obj.GetNamespace() == cr.Namespace {
		if err := controllerutil.SetControllerReference(cr, obj, r.Scheme); err != nil {
			return err
		}
	} else {
		labels := maps.Clone(obj.GetLabels())
		if labels == nil {
			labels = map[string]string{}
		}
		labels[ownerLabel] = string(cr.UID)
		obj.SetLabels(labels)
	}
	b, err := json.Marshal(obj)
	if err != nil {
		return err
	}
	sum := sha256.Sum256(b)
	hash := hex.EncodeToString(sum[:])[:16]
	ann := maps.Clone(obj.GetAnnotations())
	if ann == nil {
		ann = map[string]string{}
	}
	ann[specHashAnnotation] = hash
	obj.SetAnnotations(ann)

	existing := reflect.New(reflect.TypeOf(obj).Elem()).Interface().(client.Object)
	err = r.Get(ctx, client.ObjectKeyFromObject(obj), existing)
	if apierrors.IsNotFound(err) {
		err = r.Create(ctx, obj)
		if apierrors.IsAlreadyExists(err) {
			// The cache was behind, or only holds objects the operator
			// labeled: the object is someone else's.
			if err := r.APIReader.Get(ctx, client.ObjectKeyFromObject(obj), existing); err != nil {
				return client.IgnoreNotFound(err)
			}
			if !belongsTo(existing, cr) {
				return notOurs(obj, cr)
			}
			return nil // the next reconcile updates it
		}
		return err
	}
	if err != nil {
		return err
	}
	if !belongsTo(existing, cr) {
		return notOurs(obj, cr)
	}
	if existing.GetAnnotations()[specHashAnnotation] == hash {
		reflect.ValueOf(obj).Elem().Set(reflect.ValueOf(existing).Elem())
		return nil
	}
	// Keep what others added to the metadata (e.g. the deployment
	// controller's revision annotation).
	merged := maps.Clone(existing.GetAnnotations())
	if merged == nil {
		merged = map[string]string{}
	}
	maps.Copy(merged, ann)
	obj.SetAnnotations(merged)
	obj.SetResourceVersion(existing.GetResourceVersion())
	log.FromContext(ctx).Info("updating", "kind", reflect.TypeOf(obj).Elem().Name(), "name", obj.GetName())
	return r.Update(ctx, obj)
}

// belongsTo reports whether the operator created obj for cr: in cr's
// namespace, cr controls it; elsewhere, it has cr's ownerLabel.
func belongsTo(obj client.Object, cr *v1alpha1.DispatchOps) bool {
	if obj.GetNamespace() == cr.Namespace {
		return metav1.IsControlledBy(obj, cr)
	}
	return obj.GetLabels()[ownerLabel] == string(cr.UID) && obj.GetLabels()["app.kubernetes.io/managed-by"] == ManagedBy
}

// notOurs is the error for an object the operator would replace but didn't
// create. It never takes such an object over: in another namespace it
// could be anyone's.
func notOurs(obj client.Object, cr *v1alpha1.DispatchOps) error {
	return fmt.Errorf("%s %s/%s already exists and doesn't belong to DispatchOps %s/%s; delete or rename it",
		reflect.TypeOf(obj).Elem().Name(), obj.GetNamespace(), obj.GetName(), cr.Namespace, cr.Name)
}

// jobNamespaceAllowed reports whether in may run jobs in its job
// namespace: "" if so, and otherwise a condition reason and why. The
// namespace must be opted in (mayUseJobNamespace), and enforce the
// restricted or baseline Pod Security Standard, unless the operator was
// started allowing namespaces without one.
func (r *Reconciler) jobNamespaceAllowed(ctx context.Context, in install) (string, string, error) {
	ns := in.jobNamespace()
	ok, why, err := r.mayUseJobNamespace(ctx, in)
	if err != nil || !ok {
		return ReasonJobNamespaceNotAllowed, why, err
	}
	if r.AllowJobNamespacesWithoutPodSecurity {
		return "", "", nil
	}
	var n corev1.Namespace
	err = r.APIReader.Get(ctx, types.NamespacedName{Name: ns}, &n)
	switch {
	case apierrors.IsNotFound(err):
		return ReasonJobNamespaceNotAllowed, fmt.Sprintf("Job namespace %s does not exist", ns), nil
	case apierrors.IsForbidden(err):
		return ReasonJobNamespacePodSecurity, fmt.Sprintf("The operator may not read namespace %s to check its Pod Security "+
			"label: grant it get on that namespace (the chart does), or start it with --allow-job-namespaces-without-pod-security", ns), nil
	case err != nil:
		return "", "", fmt.Errorf("namespace %s: %w", ns, err)
	}
	if !enforcesPodSecurity(&n) {
		return ReasonJobNamespacePodSecurity, fmt.Sprintf("Job namespace %s must enforce Pod Security, since the task manager "+
			"creates pods there: label it %s=restricted (job pods comply), or start the operator with "+
			"--allow-job-namespaces-without-pod-security", ns, podSecurityEnforce), nil
	}
	return "", "", nil
}

// enforcesPodSecurity reports whether n enforces the restricted or baseline
// Pod Security Standard, either of which keeps pods from mounting the
// node's filesystem or running privileged.
func enforcesPodSecurity(n *corev1.Namespace) bool {
	switch n.Labels[podSecurityEnforce] {
	case "restricted", "baseline":
		return true
	}
	return false
}

// mayUseJobNamespace reports whether in's job namespace is opted in for
// it, and if not, why. Its own namespace always is; another only if the
// operator was started allowing it for in's namespace, or (when the
// operator has access to every namespace) the namespace's
// AllowJobsFromLabel names in's namespace. Without this, anyone who may
// create a DispatchOps could have the task manager granted Secrets and Jobs
// in any namespace.
func (r *Reconciler) mayUseJobNamespace(ctx context.Context, in install) (bool, string, error) {
	ns := in.jobNamespace()
	sources, listed := r.AllowedJobNamespaces[ns]
	if ns == in.Namespace || slices.Contains(sources, in.Namespace) {
		return true, "", nil
	}
	notAllowed := fmt.Sprintf("Job namespace %s is not in the operator's allowed job namespaces", ns)
	if listed {
		notAllowed = fmt.Sprintf("The operator's allowed job namespaces don't allow job namespace %s for namespace %s", ns, in.Namespace)
	}
	if len(r.Namespaces) > 0 {
		return false, notAllowed, nil
	}
	var n corev1.Namespace
	err := r.APIReader.Get(ctx, types.NamespacedName{Name: ns}, &n)
	if apierrors.IsNotFound(err) {
		return false, fmt.Sprintf("Job namespace %s does not exist", ns), nil
	}
	if err != nil {
		return false, "", fmt.Errorf("namespace %s: %w", ns, err)
	}
	if n.Labels[AllowJobsFromLabel] != in.Namespace {
		return false, fmt.Sprintf("%s, and it is not labeled %s=%s", notAllowed, AllowJobsFromLabel, in.Namespace), nil
	}
	return true, "", nil
}

// deleteIfOwned deletes obj if it exists and belongs to cr.
func (r *Reconciler) deleteIfOwned(ctx context.Context, cr *v1alpha1.DispatchOps, obj client.Object) error {
	if err := r.Get(ctx, client.ObjectKeyFromObject(obj), obj); err != nil {
		return client.IgnoreNotFound(err)
	}
	if !metav1.IsControlledBy(obj, cr) {
		return nil
	}
	return client.IgnoreNotFound(r.Delete(ctx, obj))
}
