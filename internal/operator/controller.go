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
	"reflect"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"dispatch-ops/internal/operator/api/v1alpha1"
)

// Condition types reported in status.
const (
	CondDatabaseReady    = "DatabaseReady"
	CondMigrated         = "Migrated"
	CondAPIReady         = "APIReady"
	CondTaskManagerReady = "TaskManagerReady"
	CondReady            = "Ready"
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
}

// SetupWithManager registers the reconciler and what it watches.
func (r *Reconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&v1alpha1.DispatchOps{}).
		Owns(&appsv1.Deployment{}).
		Owns(&appsv1.StatefulSet{}).
		Owns(&batchv1.Job{}).
		Owns(&corev1.Service{}).
		Owns(&corev1.ServiceAccount{}).
		Owns(&networkingv1.Ingress{}).
		Named("dispatchops").
		Complete(r)
}

// withDefaults fills in what the CRD's defaults would, so a resource that
// skipped them (or an older one) still works.
func withDefaults(d *v1alpha1.DispatchOps) *v1alpha1.DispatchOps {
	d = d.DeepCopy()
	s := &d.Spec
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
		return ctrl.Result{}, r.finalize(ctx, &cr)
	}
	if controllerutil.AddFinalizer(&cr, finalizer) {
		if err := r.Update(ctx, &cr); err != nil {
			return ctrl.Result{}, err
		}
	}

	in := install{DispatchOps: withDefaults(&cr), image: cr.Status.Image}
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

	err := r.reconcile(ctx, &cr, in, status, set)
	if err != nil {
		set(CondReady, false, "Error", err.Error())
	} else {
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

	if !reflect.DeepEqual(status, &cr.Status) {
		cr.Status = *status
		if uerr := r.Status().Update(ctx, &cr); uerr != nil {
			return ctrl.Result{}, errors.Join(err, uerr)
		}
	}
	if err != nil {
		return ctrl.Result{}, err
	}
	if !meta.IsStatusConditionTrue(status.Conditions, CondReady) {
		return ctrl.Result{RequeueAfter: pollInterval}, nil
	}
	return ctrl.Result{}, nil
}

type setCondition func(t string, ok bool, reason, msg string)

func (r *Reconciler) reconcile(ctx context.Context, cr *v1alpha1.DispatchOps, in install,
	status *v1alpha1.DispatchOpsStatus, set setCondition) error {
	if err := r.ensureSecrets(ctx, in); err != nil {
		return err
	}

	dbReady, err := r.reconcileDatabase(ctx, cr, in, set)
	if err != nil {
		return err
	}

	want := in.Spec.Image
	if want == "" {
		want = r.DefaultImage
	}
	if status.Image != want {
		done, err := r.migrate(ctx, cr, in, want, dbReady, set)
		if err != nil {
			return err
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
		return nil
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
// exist. They have no owner, so deleting the DispatchOps keeps them.
func (r *Reconciler) ensureSecrets(ctx context.Context, in install) error {
	var want []*corev1.Secret
	if in.Spec.EncryptionKeySecret == nil {
		key := make([]byte, 32)
		_, _ = rand.Read(key)
		want = append(want, in.generatedSecret("encryption-key", in.encryptionKeySecretName(),
			map[string]string{"key": base64.StdEncoding.EncodeToString(key)}))
	}
	if in.Spec.Admin.PasswordSecret == nil {
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
		if err := r.ensureSecret(ctx, in.apiTLSSecretName(), in.Namespace, in.generatedTLSSecret); err != nil {
			return err
		}
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

func (r *Reconciler) reconcileDatabase(ctx context.Context, cr *v1alpha1.DispatchOps, in install, set setCondition) (bool, error) {
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

	if err := r.apply(ctx, cr, in.postgresService()); err != nil {
		return false, err
	}
	sts := in.postgresStatefulSet()
	if err := r.apply(ctx, cr, sts); err != nil {
		return false, err
	}
	if sts.Status.ReadyReplicas < 1 {
		set(CondDatabaseReady, false, "Starting", "Waiting for PostgreSQL ("+sts.Name+") to be ready")
		return false, nil
	}
	set(CondDatabaseReady, true, "Managed", "PostgreSQL is running in "+sts.Name)
	return true, nil
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

func (r *Reconciler) reconcileServers(ctx context.Context, cr *v1alpha1.DispatchOps, in install, set setCondition) error {
	objs := []client.Object{
		in.apiService(), in.taskManagerServiceAccount(), in.taskManagerRole(), in.taskManagerRoleBinding(),
	}
	for _, o := range objs {
		if err := r.apply(ctx, cr, o); err != nil {
			return err
		}
	}
	if err := r.pruneJobRBAC(ctx, cr, in.jobNamespace()); err != nil {
		return err
	}

	if in.Spec.API.Ingress != nil {
		if err := r.apply(ctx, cr, in.ingress()); err != nil {
			return err
		}
	} else if err := r.deleteIfOwned(ctx, cr, &networkingv1.Ingress{ObjectMeta: metav1.ObjectMeta{Name: in.Name, Namespace: in.Namespace}}); err != nil {
		return err
	}

	for _, d := range []struct {
		dep  *appsv1.Deployment
		cond string
	}{{in.apiDeployment(), CondAPIReady}, {in.taskManagerDeployment(), CondTaskManagerReady}} {
		if err := r.apply(ctx, cr, d.dep); err != nil {
			return err
		}
		ok, msg := deploymentReady(d.dep)
		reason := "Available"
		if !ok {
			reason = "RollingOut"
		}
		set(d.cond, ok, reason, msg)
	}
	return nil
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
			return nil // the cache was behind; the next reconcile updates it
		}
		return err
	}
	if err != nil {
		return err
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

// pruneJobRBAC deletes the task manager's Roles and RoleBindings in
// namespaces other than keep (all of them if keep is empty), which are
// left behind when the job namespace changes, or the DispatchOps is
// deleted.
func (r *Reconciler) pruneJobRBAC(ctx context.Context, cr *v1alpha1.DispatchOps, keep string) error {
	sel := client.MatchingLabels{ownerLabel: string(cr.UID)}
	var roles rbacv1.RoleList
	if err := r.APIReader.List(ctx, &roles, sel); err != nil {
		return err
	}
	var bindings rbacv1.RoleBindingList
	if err := r.APIReader.List(ctx, &bindings, sel); err != nil {
		return err
	}
	var objs []client.Object
	for i := range roles.Items {
		objs = append(objs, &roles.Items[i])
	}
	for i := range bindings.Items {
		objs = append(objs, &bindings.Items[i])
	}
	for _, o := range objs {
		if o.GetNamespace() != keep {
			if err := r.Delete(ctx, o); client.IgnoreNotFound(err) != nil {
				return err
			}
		}
	}
	return nil
}

// finalize cleans up what owner references can't: RBAC in another job
// namespace. Owned objects are garbage collected; generated Secrets and the
// database volume are kept.
func (r *Reconciler) finalize(ctx context.Context, cr *v1alpha1.DispatchOps) error {
	if !controllerutil.ContainsFinalizer(cr, finalizer) {
		return nil
	}
	if err := r.pruneJobRBAC(ctx, cr, ""); err != nil {
		return err
	}
	controllerutil.RemoveFinalizer(cr, finalizer)
	return r.Update(ctx, cr)
}
