package operator

import (
	"time"

	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"dispatch-ops/internal/operator/api/v1alpha1"
)

const ns = "ops"

type env struct {
	t   *testing.T
	ctx context.Context
	c   client.Client
	r   *Reconciler
	key types.NamespacedName
}

func newEnv(t *testing.T, cr *v1alpha1.DispatchOps, objs ...client.Object) *env {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	cr.Name, cr.Namespace, cr.UID = "dops", ns, "uid-1"
	c := fake.NewClientBuilder().WithScheme(scheme).
		WithStatusSubresource(&v1alpha1.DispatchOps{}, &appsv1.Deployment{}, &appsv1.StatefulSet{}, &batchv1.Job{}).
		WithObjects(append(objs, cr)...).Build()
	return &env{
		t: t, ctx: context.Background(), c: c,
		r:   &Reconciler{Client: c, APIReader: c, Scheme: scheme, DefaultImage: "dispatch-ops:1.0"},
		key: types.NamespacedName{Namespace: ns, Name: "dops"},
	}
}

func (e *env) reconcile() *v1alpha1.DispatchOps {
	e.t.Helper()
	if _, err := e.r.Reconcile(e.ctx, ctrl.Request{NamespacedName: e.key}); err != nil {
		e.t.Fatalf("reconcile: %v", err)
	}
	return e.cr()
}

func (e *env) cr() *v1alpha1.DispatchOps {
	e.t.Helper()
	var cr v1alpha1.DispatchOps
	if err := e.c.Get(e.ctx, e.key, &cr); err != nil {
		e.t.Fatal(err)
	}
	return &cr
}

func (e *env) get(name string, obj client.Object) bool {
	e.t.Helper()
	err := e.c.Get(e.ctx, types.NamespacedName{Namespace: ns, Name: name}, obj)
	if client.IgnoreNotFound(err) != nil {
		e.t.Fatal(err)
	}
	return err == nil
}

func (e *env) condition(cr *v1alpha1.DispatchOps, t string) metav1.Condition {
	e.t.Helper()
	c := meta.FindStatusCondition(cr.Status.Conditions, t)
	if c == nil {
		e.t.Fatalf("no %s condition in %+v", t, cr.Status.Conditions)
	}
	return *c
}

func (e *env) wantCondition(cr *v1alpha1.DispatchOps, t string, status metav1.ConditionStatus, reason string) {
	e.t.Helper()
	if c := e.condition(cr, t); c.Status != status || c.Reason != reason {
		e.t.Errorf("%s = %s/%s (%s), want %s/%s", t, c.Status, c.Reason, c.Message, status, reason)
	}
}

func (e *env) databaseReady() {
	e.t.Helper()
	var sts appsv1.StatefulSet
	if !e.get("dops-postgres", &sts) {
		e.t.Fatal("no PostgreSQL StatefulSet")
	}
	sts.Status.ReadyReplicas = 1
	if err := e.c.Status().Update(e.ctx, &sts); err != nil {
		e.t.Fatal(err)
	}
}

// finishMigration marks the migration Job for image complete (or failed).
func (e *env) finishMigration(image string, ok bool) {
	e.t.Helper()
	var job batchv1.Job
	if !e.get(install{DispatchOps: e.cr()}.migrationJobName(image), &job) {
		e.t.Fatalf("no migration job for %s", image)
	}
	cond := batchv1.JobComplete
	if !ok {
		cond = batchv1.JobFailed
	}
	job.Status.Conditions = append(job.Status.Conditions, batchv1.JobCondition{Type: cond, Status: corev1.ConditionTrue, Message: "boom"})
	if err := e.c.Status().Update(e.ctx, &job); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) deploymentsAvailable() {
	e.t.Helper()
	for _, name := range []string{"dops-api", "dops-task-manager"} {
		var d appsv1.Deployment
		if !e.get(name, &d) {
			e.t.Fatalf("no deployment %s", name)
		}
		d.Status.ObservedGeneration = d.Generation
		d.Status.UpdatedReplicas, d.Status.AvailableReplicas = *d.Spec.Replicas, *d.Spec.Replicas
		if err := e.c.Status().Update(e.ctx, &d); err != nil {
			e.t.Fatal(err)
		}
	}
}

func envValue(c corev1.Container, name string) (corev1.EnvVar, bool) {
	for _, e := range c.Env {
		if e.Name == name {
			return e, true
		}
	}
	return corev1.EnvVar{}, false
}

func portNamed(c corev1.Container, name string) (int32, bool) {
	for _, p := range c.Ports {
		if p.Name == name {
			return p.ContainerPort, true
		}
	}
	return 0, false
}

func TestInstallAndUpgrade(t *testing.T) {
	e := newEnv(t, &v1alpha1.DispatchOps{})

	// First: secrets and the database, nothing that needs the schema.
	cr := e.reconcile()
	e.wantCondition(cr, CondDatabaseReady, metav1.ConditionFalse, "Starting")
	e.wantCondition(cr, CondReady, metav1.ConditionFalse, "Starting")
	if !strings.Contains(strings.Join(cr.Finalizers, ","), finalizer) {
		t.Errorf("finalizers = %v", cr.Finalizers)
	}
	var key, admin, pg corev1.Secret
	if !e.get("dops-encryption-key", &key) || !e.get("dops-admin-password", &admin) || !e.get("dops-postgres", &pg) {
		t.Fatal("generated secrets missing")
	}
	if len(key.OwnerReferences) != 0 || len(pg.OwnerReferences) != 0 {
		t.Error("generated secrets must outlive the DispatchOps")
	}
	if !strings.Contains(pg.StringData["url"], "@dops-postgres.ops.svc:5432/dispatch") {
		t.Errorf("database url = %q", pg.StringData["url"])
	}
	var dep appsv1.Deployment
	if e.get("dops-api", &dep) {
		t.Fatal("API deployed before the database was migrated")
	}

	// Database ready: migrate.
	e.databaseReady()
	cr = e.reconcile()
	e.wantCondition(cr, CondMigrated, metav1.ConditionFalse, "Migrating")
	var job batchv1.Job
	if !e.get(install{DispatchOps: cr}.migrationJobName("dispatch-ops:1.0"), &job) {
		t.Fatal("no migration job")
	}
	pod := job.Spec.Template.Spec
	if pod.InitContainers[0].Image != "dispatch-ops:1.0" || strings.Join(pod.InitContainers[0].Args, " ") != "migrate up" {
		t.Errorf("migrate container = %v %v", pod.InitContainers[0].Image, pod.InitContainers[0].Args)
	}
	if args := strings.Join(pod.Containers[0].Args, " "); !strings.Contains(args, "-username admin") || !strings.Contains(args, "-if-missing") {
		t.Errorf("admin container args = %s", args)
	}

	// Migrated: the servers roll out on that image.
	e.finishMigration("dispatch-ops:1.0", true)
	cr = e.reconcile()
	if cr.Status.Image != "dispatch-ops:1.0" {
		t.Errorf("status.image = %q", cr.Status.Image)
	}
	e.wantCondition(cr, CondAPIReady, metav1.ConditionFalse, "RollingOut")
	var api, tm appsv1.Deployment
	e.get("dops-api", &api)
	e.get("dops-task-manager", &tm)
	if api.Spec.Template.Spec.Containers[0].Image != "dispatch-ops:1.0" || *api.Spec.Replicas != 2 {
		t.Errorf("api deployment: image %s replicas %d", api.Spec.Template.Spec.Containers[0].Image, *api.Spec.Replicas)
	}
	if v, _ := envValue(tm.Spec.Template.Spec.Containers[0], "DISPATCH_RUNNER_API_URL"); v.Value != "https://dops-api.ops.svc:8443" {
		t.Errorf("runner API URL = %q", v.Value)
	}
	if v, _ := envValue(api.Spec.Template.Spec.Containers[0], "DISPATCH_DATABASE_URL"); v.ValueFrom.SecretKeyRef.Name != "dops-postgres" {
		t.Errorf("database URL env = %+v", v)
	}
	if tm.Spec.Template.Spec.ServiceAccountName != "dops-task-manager" {
		t.Errorf("task manager service account = %q", tm.Spec.Template.Spec.ServiceAccountName)
	}
	var role rbacv1.Role
	if !e.get("dops-task-manager", &role) {
		t.Error("no task manager Role in the job namespace")
	}

	e.deploymentsAvailable()
	cr = e.reconcile()
	e.wantCondition(cr, CondReady, metav1.ConditionTrue, "Ready")

	// Nothing changed: nothing is rewritten.
	e.get("dops-api", &api)
	rv := api.ResourceVersion
	e.reconcile()
	e.get("dops-api", &api)
	if api.ResourceVersion != rv {
		t.Error("a reconcile without changes updated the API deployment")
	}

	// Upgrade: migrations first, and the servers keep the old image until
	// they're done.
	cr.Spec.Image = "dispatch-ops:2.0"
	if err := e.c.Update(e.ctx, cr); err != nil {
		t.Fatal(err)
	}
	cr = e.reconcile()
	e.wantCondition(cr, CondMigrated, metav1.ConditionFalse, "Migrating")
	e.get("dops-api", &api)
	if img := api.Spec.Template.Spec.Containers[0].Image; img != "dispatch-ops:1.0" {
		t.Errorf("API runs %s before the migration finished", img)
	}
	e.finishMigration("dispatch-ops:2.0", true)
	cr = e.reconcile()
	e.get("dops-api", &api)
	if img := api.Spec.Template.Spec.Containers[0].Image; img != "dispatch-ops:2.0" || cr.Status.Image != "dispatch-ops:2.0" {
		t.Errorf("after the migration: API runs %s, status.image %s", img, cr.Status.Image)
	}

	// Generated secrets are never regenerated.
	var key2 corev1.Secret
	e.get("dops-encryption-key", &key2)
	if key2.StringData["key"] != key.StringData["key"] {
		t.Error("the encryption key changed")
	}
}

func TestFailedMigrationKeepsOldImage(t *testing.T) {
	e := newEnv(t, &v1alpha1.DispatchOps{})
	e.reconcile()
	e.databaseReady()
	e.reconcile()
	e.finishMigration("dispatch-ops:1.0", true)
	cr := e.reconcile()

	cr.Spec.Image = "dispatch-ops:bad"
	if err := e.c.Update(e.ctx, cr); err != nil {
		t.Fatal(err)
	}
	e.reconcile()
	e.finishMigration("dispatch-ops:bad", false)
	cr = e.reconcile()
	e.wantCondition(cr, CondMigrated, metav1.ConditionFalse, "MigrationFailed")
	e.wantCondition(cr, CondReady, metav1.ConditionFalse, "MigrationFailed")
	var api appsv1.Deployment
	e.get("dops-api", &api)
	if img := api.Spec.Template.Spec.Containers[0].Image; img != "dispatch-ops:1.0" || cr.Status.Image != "dispatch-ops:1.0" {
		t.Errorf("after a failed migration: API runs %s, status.image %s", img, cr.Status.Image)
	}
}

func TestExternalDatabaseAndSettings(t *testing.T) {
	cr := &v1alpha1.DispatchOps{Spec: v1alpha1.DispatchOpsSpec{
		Image: "registry.example/dispatch-ops:3",
		Database: v1alpha1.DatabaseSpec{External: &v1alpha1.ExternalDatabase{
			URLSecret: corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: "db"}, Key: "dsn"},
		}},
		EncryptionKeySecret: &corev1.LocalObjectReference{Name: "my-key"},
		ExternalURL:         "https://dispatch.example.com",
		OIDC: &v1alpha1.OIDCSpec{
			Issuer: "https://idp.example.com", ClientID: "dispatch", Scopes: []string{"profile", "groups"},
			ClientSecret: &corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: "oidc"}, Key: "secret"},
		},
		API: v1alpha1.APISpec{Ingress: &v1alpha1.IngressSpec{Host: "dispatch.example.com", TLSSecretName: "tls"}},
		Retention: v1alpha1.RetentionSpec{
			Jobs: &metav1.Duration{Duration: 30 * 24 * time.Hour}, AuditLog: &metav1.Duration{},
		},
	}}
	e := newEnv(t, cr)

	cr = e.reconcile()
	e.wantCondition(cr, CondDatabaseReady, metav1.ConditionFalse, "SecretMissing")
	var s corev1.Secret
	if e.get("dops-encryption-key", &s) || e.get("dops-postgres", &s) {
		t.Error("generated a secret the spec provides")
	}
	var sts appsv1.StatefulSet
	if e.get("dops-postgres", &sts) {
		t.Error("ran PostgreSQL with an external database")
	}

	if err := e.c.Create(e.ctx, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "db", Namespace: ns},
		Data:       map[string][]byte{"dsn": []byte("postgres://x")},
	}); err != nil {
		t.Fatal(err)
	}
	cr = e.reconcile()
	e.wantCondition(cr, CondDatabaseReady, metav1.ConditionTrue, "External")
	e.finishMigration("registry.example/dispatch-ops:3", true)
	e.reconcile()

	var api appsv1.Deployment
	e.get("dops-api", &api)
	c := api.Spec.Template.Spec.Containers[0]
	for name, want := range map[string]string{
		"DISPATCH_EXTERNAL_URL": "https://dispatch.example.com", "DISPATCH_OIDC_ISSUER": "https://idp.example.com",
		"DISPATCH_OIDC_SCOPES": "profile,groups", "DISPATCH_SECURE_COOKIES": "true",
	} {
		if v, _ := envValue(c, name); v.Value != want {
			t.Errorf("%s = %q, want %q", name, v.Value, want)
		}
	}
	if v, _ := envValue(c, "DISPATCH_OIDC_CLIENT_SECRET"); v.ValueFrom == nil || v.ValueFrom.SecretKeyRef.Name != "oidc" {
		t.Errorf("client secret env = %+v", v)
	}
	if v, _ := envValue(c, "DISPATCH_DATABASE_URL"); v.ValueFrom.SecretKeyRef.Key != "dsn" {
		t.Errorf("database URL env = %+v", v)
	}
	if v, _ := envValue(c, "DISPATCH_ENCRYPTION_KEY"); v.ValueFrom.SecretKeyRef.Name != "my-key" {
		t.Errorf("encryption key env = %+v", v)
	}
	if p, _ := portNamed(c, "metrics"); p != 9090 || api.Spec.Template.Annotations["prometheus.io/scrape"] != "true" {
		t.Errorf("API metrics port = %d, annotations = %v", p, api.Spec.Template.Annotations)
	}
	var svc corev1.Service
	e.get("dops-api", &svc)
	if len(svc.Spec.Ports) != 1 {
		t.Errorf("the API Service exposes %v; metrics should stay off it", svc.Spec.Ports)
	}

	var tm appsv1.Deployment
	e.get("dops-task-manager", &tm)
	tc := tm.Spec.Template.Spec.Containers[0]
	for name, want := range map[string]string{
		"DISPATCH_EXTERNAL_URL": "https://dispatch.example.com", "DISPATCH_JOB_RETENTION": "720h0m0s",
		"DISPATCH_AUDIT_RETENTION": "0s", "DISPATCH_METRICS_ADDR": ":9090",
	} {
		if v, _ := envValue(tc, name); v.Value != want {
			t.Errorf("task manager %s = %q, want %q", name, v.Value, want)
		}
	}

	// Metrics can be turned off.
	cur := e.cr()
	cur.Spec.Metrics.Enabled = new(false)
	if err := e.c.Update(e.ctx, cur); err != nil {
		t.Fatal(err)
	}
	e.reconcile()
	e.get("dops-task-manager", &tm)
	tc = tm.Spec.Template.Spec.Containers[0]
	if v, ok := envValue(tc, "DISPATCH_METRICS_ADDR"); !ok || v.Value != "" {
		t.Errorf("metrics off: DISPATCH_METRICS_ADDR = %+v", v)
	}
	if _, ok := portNamed(tc, "metrics"); ok || len(tm.Spec.Template.Annotations) != 0 {
		t.Errorf("metrics off: ports %v, annotations %v", tc.Ports, tm.Spec.Template.Annotations)
	}

	var ing networkingv1.Ingress
	if !e.get("dops", &ing) {
		t.Error("no ingress")
	} else if len(ing.Spec.TLS) != 1 || ing.Spec.TLS[0].SecretName != "tls" {
		t.Errorf("ingress TLS = %+v", ing.Spec.TLS)
	}
}

// deploy reconciles a new installation with a managed database until its
// servers are deployed.
func (e *env) deploy() {
	e.t.Helper()
	e.reconcile()
	e.databaseReady()
	e.reconcile()
	e.finishMigration("dispatch-ops:1.0", true)
	e.reconcile()
}

func TestAPIServesTLS(t *testing.T) {
	e := newEnv(t, &v1alpha1.DispatchOps{Spec: v1alpha1.DispatchOpsSpec{
		ExternalURL: "https://dispatch.example.com",
		API:         v1alpha1.APISpec{Ingress: &v1alpha1.IngressSpec{Host: "ui.example.com", Annotations: map[string]string{"a": "b"}}},
	}})
	e.deploy()

	// A self-signed P-384 certificate for the Service and public names.
	var s corev1.Secret
	if !e.get("dops-api-tls", &s) {
		t.Fatal("no generated TLS Secret")
	}
	if s.Type != corev1.SecretTypeTLS || len(s.OwnerReferences) != 0 || string(s.Data["ca.crt"]) != string(s.Data["tls.crt"]) {
		t.Errorf("TLS Secret: type %s, owners %v", s.Type, s.OwnerReferences)
	}
	block, _ := pem.Decode(s.Data["tls.crt"])
	if block == nil {
		t.Fatal("tls.crt is not PEM")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	if pub, ok := cert.PublicKey.(*ecdsa.PublicKey); !ok || pub.Curve != elliptic.P384() {
		t.Errorf("key is %T, want ECDSA P-384", cert.PublicKey)
	}
	for _, host := range []string{"dops-api.ops.svc", "dops-api.ops.svc.cluster.local", "localhost", "ui.example.com", "dispatch.example.com"} {
		if err := cert.VerifyHostname(host); err != nil {
			t.Errorf("certificate: %v", err)
		}
	}
	if _, err := tls.X509KeyPair(s.Data["tls.crt"], s.Data["tls.key"]); err != nil {
		t.Errorf("key pair: %v", err)
	}

	var api, tm appsv1.Deployment
	e.get("dops-api", &api)
	c := api.Spec.Template.Spec.Containers[0]
	for name, want := range map[string]string{
		"DISPATCH_TLS": "true", "DISPATCH_LISTEN_ADDR": ":8443",
		"DISPATCH_TLS_CERT_FILE": "/var/run/dispatch-ops/tls/tls.crt", "DISPATCH_TLS_KEY_FILE": "/var/run/dispatch-ops/tls/tls.key",
	} {
		if v, _ := envValue(c, name); v.Value != want {
			t.Errorf("%s = %q, want %q", name, v.Value, want)
		}
	}
	if p, _ := portNamed(c, "https"); p != 8443 {
		t.Errorf("https port = %d", p)
	}
	if g := c.ReadinessProbe.HTTPGet; g.Scheme != corev1.URISchemeHTTPS || g.Port.StrVal != "https" {
		t.Errorf("readiness probe = %+v", g)
	}
	if vols := api.Spec.Template.Spec.Volumes; len(vols) != 1 || vols[0].Secret.SecretName != "dops-api-tls" || len(c.VolumeMounts) != 1 {
		t.Errorf("API volumes = %+v, mounts %+v", vols, c.VolumeMounts)
	}

	e.get("dops-task-manager", &tm)
	tc := tm.Spec.Template.Spec.Containers[0]
	if v, _ := envValue(tc, "DISPATCH_RUNNER_API_TLS_DIR"); v.Value != "/var/run/dispatch-ops/api-tls" {
		t.Errorf("DISPATCH_RUNNER_API_TLS_DIR = %q", v.Value)
	}
	if vols := tm.Spec.Template.Spec.Volumes; len(vols) != 1 || vols[0].Secret.SecretName != "dops-api-tls" {
		t.Errorf("task manager volumes = %+v", vols)
	}

	var svc corev1.Service
	e.get("dops-api", &svc)
	if p := svc.Spec.Ports[0]; p.Name != "https" || p.Port != 8443 {
		t.Errorf("Service port = %+v", p)
	}
	var ing networkingv1.Ingress
	e.get("dops", &ing)
	if ing.Annotations["nginx.ingress.kubernetes.io/backend-protocol"] != "HTTPS" || ing.Annotations["a"] != "b" {
		t.Errorf("ingress annotations = %v", ing.Annotations)
	}
	if b := ing.Spec.Rules[0].HTTP.Paths[0].Backend.Service.Port.Name; b != "https" {
		t.Errorf("ingress backend port = %q", b)
	}

	// A certificate of your own (from cert-manager, say) replaces it.
	cur := e.cr()
	cur.Spec.API.TLS.SecretName = "from-cert-manager"
	if err := e.c.Update(e.ctx, cur); err != nil {
		t.Fatal(err)
	}
	e.reconcile()
	e.get("dops-api", &api)
	e.get("dops-task-manager", &tm)
	if api.Spec.Template.Spec.Volumes[0].Secret.SecretName != "from-cert-manager" ||
		tm.Spec.Template.Spec.Volumes[0].Secret.SecretName != "from-cert-manager" {
		t.Errorf("volumes don't use the given Secret: %+v / %+v", api.Spec.Template.Spec.Volumes, tm.Spec.Template.Spec.Volumes)
	}
}

func TestAPIWithoutTLS(t *testing.T) {
	e := newEnv(t, &v1alpha1.DispatchOps{Spec: v1alpha1.DispatchOpsSpec{
		API: v1alpha1.APISpec{TLS: v1alpha1.APITLSSpec{Enabled: new(false)}, Ingress: &v1alpha1.IngressSpec{Host: "ui.example.com"}},
	}})
	e.deploy()
	var s corev1.Secret
	if e.get("dops-api-tls", &s) {
		t.Error("generated a certificate with TLS off")
	}
	var api, tm appsv1.Deployment
	e.get("dops-api", &api)
	c := api.Spec.Template.Spec.Containers[0]
	if v, _ := envValue(c, "DISPATCH_TLS"); v.Value != "false" {
		t.Errorf("DISPATCH_TLS = %q", v.Value)
	}
	if p, _ := portNamed(c, "http"); p != 8080 || c.ReadinessProbe.HTTPGet.Scheme != corev1.URISchemeHTTP {
		t.Errorf("http port %d, probe %+v", p, c.ReadinessProbe.HTTPGet)
	}
	if len(api.Spec.Template.Spec.Volumes) != 0 {
		t.Errorf("volumes = %+v", api.Spec.Template.Spec.Volumes)
	}
	e.get("dops-task-manager", &tm)
	if v, _ := envValue(tm.Spec.Template.Spec.Containers[0], "DISPATCH_RUNNER_API_URL"); v.Value != "http://dops-api.ops.svc:8080" {
		t.Errorf("runner API URL = %q", v.Value)
	}
	var ing networkingv1.Ingress
	e.get("dops", &ing)
	if _, ok := ing.Annotations["nginx.ingress.kubernetes.io/backend-protocol"]; ok {
		t.Errorf("ingress annotations = %v", ing.Annotations)
	}
}

func TestJobNamespaceRBACIsCleanedUp(t *testing.T) {
	cr := &v1alpha1.DispatchOps{Spec: v1alpha1.DispatchOpsSpec{
		TaskManager: v1alpha1.TaskManagerSpec{JobNamespace: "jobs"},
	}}
	e := newEnv(t, cr)
	e.reconcile()
	e.databaseReady()
	e.reconcile()
	e.finishMigration("dispatch-ops:1.0", true)
	cr = e.reconcile()

	var role rbacv1.Role
	key := types.NamespacedName{Namespace: "jobs", Name: "ops-dops-task-manager"}
	if err := e.c.Get(e.ctx, key, &role); err != nil {
		t.Fatalf("no Role in the job namespace: %v", err)
	}
	if role.Labels[ownerLabel] != "uid-1" || len(role.OwnerReferences) != 0 {
		t.Errorf("role labels %v owners %v", role.Labels, role.OwnerReferences)
	}
	var tm appsv1.Deployment
	e.get("dops-task-manager", &tm)
	if v, _ := envValue(tm.Spec.Template.Spec.Containers[0], "DISPATCH_JOB_NAMESPACE"); v.Value != "jobs" {
		t.Errorf("job namespace env = %q", v.Value)
	}

	// Deleting the DispatchOps removes RBAC that owner references can't.
	if err := e.c.Delete(e.ctx, cr); err != nil {
		t.Fatal(err)
	}
	if _, err := e.r.Reconcile(e.ctx, ctrl.Request{NamespacedName: e.key}); err != nil {
		t.Fatal(err)
	}
	if err := e.c.Get(e.ctx, key, &role); err == nil {
		t.Error("the job namespace Role survived")
	}
	var binding rbacv1.RoleBinding
	if err := e.c.Get(e.ctx, key, &binding); err == nil {
		t.Error("the job namespace RoleBinding survived")
	}
}
