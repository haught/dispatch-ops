package operator

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"dispatch-ops/internal/operator/api/v1alpha1"
	"dispatch-ops/internal/tlscert"
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
	haveNS := false
	for _, o := range objs {
		if n, ok := o.(*corev1.Namespace); ok && n.Name == ns {
			haveNS = true
		}
	}
	if !haveNS {
		objs = append(objs, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns, Labels: restricted()}})
	}
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
	if vols := withoutVolume(api.Spec.Template.Spec.Volumes, "postgres-ca"); len(vols) != 1 || vols[0].Secret.SecretName != "dops-api-tls" || len(c.VolumeMounts) != 2 {
		t.Errorf("API volumes = %+v, mounts %+v", vols, c.VolumeMounts)
	}

	e.get("dops-task-manager", &tm)
	tc := tm.Spec.Template.Spec.Containers[0]
	if v, _ := envValue(tc, "DISPATCH_RUNNER_API_TLS_DIR"); v.Value != "/var/run/dispatch-ops/api-tls" {
		t.Errorf("DISPATCH_RUNNER_API_TLS_DIR = %q", v.Value)
	}
	// L7: the task manager gets the certificate to trust, never the key.
	checkTrustVolume(t, tm.Spec.Template.Spec.Volumes, "dops-api-tls")

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
	if api.Spec.Template.Spec.Volumes[0].Secret.SecretName != "from-cert-manager" {
		t.Errorf("API volumes don't use the given Secret: %+v", api.Spec.Template.Spec.Volumes)
	}
	checkTrustVolume(t, tm.Spec.Template.Spec.Volumes, "from-cert-manager")
}

// checkTrustVolume checks that vols is only the public half of the API
// certificate Secret: tls.crt required, ca.crt optional (a certificate from
// a public CA may have none), and no tls.key.
func checkTrustVolume(t *testing.T, vols []corev1.Volume, secret string) {
	t.Helper()
	vols = withoutVolume(vols, "postgres-ca") // the database's CA, not the API's
	if len(vols) != 1 || vols[0].Projected == nil || vols[0].Secret != nil {
		t.Fatalf("task manager volumes = %+v, want one projected volume", vols)
	}
	keys := map[string]bool{}
	for _, src := range vols[0].Projected.Sources {
		if src.Secret == nil || src.Secret.Name != secret {
			t.Errorf("projection %+v isn't Secret %s", src, secret)
			continue
		}
		optional := src.Secret.Optional != nil && *src.Secret.Optional
		for _, it := range src.Secret.Items {
			keys[it.Key] = true
			if want := it.Key == "ca.crt"; optional != want {
				t.Errorf("%s optional = %v, want %v", it.Key, optional, want)
			}
		}
	}
	if !keys["tls.crt"] || !keys["ca.crt"] || keys["tls.key"] || len(keys) != 2 {
		t.Errorf("task manager mounts keys %v, want tls.crt and ca.crt only", keys)
	}
}

// L11: generated certificates are renewed before they expire and when the
// names they must cover change; the database restarts onto its new one.
// Certificates you provide are never touched.
func TestGeneratedCertificatesRenew(t *testing.T) {
	e := newEnv(t, &v1alpha1.DispatchOps{Spec: v1alpha1.DispatchOpsSpec{ExternalURL: "https://dispatch.example.com"}})
	e.deploy()
	e.deploymentsAvailable()
	if res, err := e.r.Reconcile(e.ctx, ctrl.Request{NamespacedName: e.key}); err != nil || res.RequeueAfter != readyRecheck {
		t.Errorf("ready reconcile = %+v, %v; want a requeue after %s for renewal", res, err, readyRecheck)
	}

	certOf := func(name string) (*x509.Certificate, corev1.Secret) {
		t.Helper()
		var s corev1.Secret
		if !e.get(name, &s) {
			t.Fatalf("no Secret %s", name)
		}
		block, _ := pem.Decode(s.Data["tls.crt"])
		cert, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			t.Fatal(err)
		}
		return cert, s
	}
	stsHash := func() string {
		var sts appsv1.StatefulSet
		e.get("dops-postgres", &sts)
		return sts.Spec.Template.Annotations[postgresCertAnnotation]
	}

	api, _ := certOf("dops-api-tls")
	if d := time.Until(api.NotAfter); d > selfSignedValidity || d < selfSignedValidity-48*time.Hour {
		t.Errorf("certificate valid for %s, want about %s", d, selfSignedValidity)
	}
	db, dbSecret := certOf("dops-postgres-tls")
	if h := stsHash(); h == "" || h != certHash(&dbSecret) {
		t.Errorf("database pod cert annotation = %q, want %q", h, certHash(&dbSecret))
	}

	// Nothing to do: the same certificates.
	e.reconcile()
	if again, _ := certOf("dops-api-tls"); again.SerialNumber.Cmp(api.SerialNumber) != 0 {
		t.Error("renewed a certificate that didn't need it")
	}

	// Close to expiry: renewed, and the database rolls.
	expiring := func(name string, hosts []string) {
		t.Helper()
		certPEM, keyPEM, err := tlscert.SelfSigned(hosts, renewBefore-time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		_, s := certOf(name)
		s.Data = map[string][]byte{"tls.crt": certPEM, "tls.key": keyPEM, "ca.crt": certPEM}
		if err := e.c.Update(e.ctx, &s); err != nil {
			t.Fatal(err)
		}
	}
	in := install{DispatchOps: e.cr()}
	expiring("dops-postgres-tls", in.postgresCertHosts())
	e.reconcile()
	renewed, s := certOf("dops-postgres-tls")
	if renewed.SerialNumber.Cmp(db.SerialNumber) == 0 || time.Until(renewed.NotAfter) < renewBefore {
		t.Errorf("database certificate not renewed: expires %s", renewed.NotAfter)
	}
	if _, err := tls.X509KeyPair(s.Data["tls.crt"], s.Data["tls.key"]); err != nil {
		t.Errorf("renewed key pair: %v", err)
	}
	if h := stsHash(); h != certHash(&s) {
		t.Errorf("database pod cert annotation = %q, want the renewed %q", h, certHash(&s))
	}

	// A new public name: the API certificate is reissued to cover it.
	cur := e.cr()
	cur.Spec.ExternalURL = "https://ops.example.org"
	if err := e.c.Update(e.ctx, cur); err != nil {
		t.Fatal(err)
	}
	e.reconcile()
	if c, _ := certOf("dops-api-tls"); c.VerifyHostname("ops.example.org") != nil {
		t.Errorf("API certificate not reissued for the new name: %v", c.DNSNames)
	}

	// A Secret the operator didn't generate is left alone, even expiring.
	_, s = certOf("dops-api-tls")
	delete(s.Annotations, generatedAnnotation)
	if err := e.c.Update(e.ctx, &s); err != nil {
		t.Fatal(err)
	}
	expiring("dops-api-tls", install{DispatchOps: e.cr()}.apiCertHosts())
	before, _ := certOf("dops-api-tls")
	e.reconcile()
	if after, _ := certOf("dops-api-tls"); after.SerialNumber.Cmp(before.SerialNumber) != 0 {
		t.Error("renewed a certificate Secret the operator didn't generate")
	}
}

// L11: once the first migration has created the administrator, their
// password Secret may be deleted: it isn't made again, and later upgrades
// migrate without it.
func TestAdminPasswordSecretMayBeDeleted(t *testing.T) {
	e := newEnv(t, &v1alpha1.DispatchOps{})
	e.deploy()
	var admin corev1.Secret
	if !e.get("dops-admin-password", &admin) {
		t.Fatal("no admin password Secret")
	}
	if err := e.c.Delete(e.ctx, &admin); err != nil {
		t.Fatal(err)
	}
	e.reconcile()
	if e.get("dops-admin-password", &admin) {
		t.Error("admin password Secret made again after the administrator was created")
	}

	cur := e.cr()
	cur.Spec.Image = "dispatch-ops:2.0"
	if err := e.c.Update(e.ctx, cur); err != nil {
		t.Fatal(err)
	}
	e.reconcile()
	var job batchv1.Job
	if !e.get(install{DispatchOps: e.cr()}.migrationJobName("dispatch-ops:2.0"), &job) {
		t.Fatal("no migration job for the upgrade")
	}
	pod := job.Spec.Template.Spec
	if len(pod.InitContainers) != 0 || len(pod.Containers) != 1 || pod.Containers[0].Name != "migrate" || len(withoutVolume(pod.Volumes, "postgres-ca")) != 0 {
		t.Errorf("upgrade migration without the admin Secret = init %+v, containers %+v, volumes %+v",
			pod.InitContainers, pod.Containers, pod.Volumes)
	}
	e.finishMigration("dispatch-ops:2.0", true)
	if cr := e.reconcile(); cr.Status.Image != "dispatch-ops:2.0" {
		t.Errorf("status image = %s after the upgrade", cr.Status.Image)
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
	if vols := withoutVolume(api.Spec.Template.Spec.Volumes, "postgres-ca"); len(vols) != 0 {
		t.Errorf("volumes = %+v", vols)
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

// foreignName is what the operator names a component's object in another
// job namespace, for the test DispatchOps.
func foreignName(component string) string {
	return install{DispatchOps: &v1alpha1.DispatchOps{ObjectMeta: metav1.ObjectMeta{Name: "dops", Namespace: ns}}}.
		foreignJobObjectName(component)
}

// withoutVolume drops the named volume.
func withoutVolume(vols []corev1.Volume, name string) []corev1.Volume {
	var out []corev1.Volume
	for _, v := range vols {
		if v.Name != name {
			out = append(out, v)
		}
	}
	return out
}

// restricted are the labels of a namespace that enforces the restricted
// Pod Security Standard.
func restricted() map[string]string { return map[string]string{podSecurityEnforce: "restricted"} }

func jobsNamespace(allowFrom string) *corev1.Namespace {
	n := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "jobs", Labels: restricted()}}
	if allowFrom != "" {
		n.Labels[AllowJobsFromLabel] = allowFrom
	}
	return n
}

func (e *env) setNamespaceLabel(value string) {
	e.t.Helper()
	var n corev1.Namespace
	if err := e.c.Get(e.ctx, types.NamespacedName{Name: "jobs"}, &n); err != nil {
		e.t.Fatal(err)
	}
	n.Labels[AllowJobsFromLabel] = value
	if err := e.c.Update(e.ctx, &n); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) exists(namespace, name string, obj client.Object) bool {
	e.t.Helper()
	err := e.c.Get(e.ctx, types.NamespacedName{Namespace: namespace, Name: name}, obj)
	if client.IgnoreNotFound(err) != nil {
		e.t.Fatal(err)
	}
	return err == nil
}

func TestJobNamespaceMustOptIn(t *testing.T) {
	for _, tc := range []struct {
		name      string
		label     string
		allowed   map[string][]string
		scoped    bool
		wantAllow bool
	}{
		{name: "unlabeled", wantAllow: false},
		{name: "labeled for another namespace", label: "team-b", wantAllow: false},
		{name: "labeled", label: ns, wantAllow: true},
		{name: "allowed by the operator", allowed: map[string][]string{"jobs": {ns}}, wantAllow: true},
		{name: "allowed by the operator, scoped", allowed: map[string][]string{"jobs": {"team-b", ns}}, scoped: true, wantAllow: true},
		// R2-H2 F5: the allow-list names who may use each job namespace.
		{name: "allowed for another namespace", allowed: map[string][]string{"jobs": {"team-b"}}, wantAllow: false},
		{name: "allowed for another namespace, scoped", allowed: map[string][]string{"jobs": {"team-b"}}, scoped: true, wantAllow: false},
		{name: "another job namespace allowed", allowed: map[string][]string{"other-jobs": {ns}}, scoped: true, wantAllow: false},
		// A scoped operator has no access to other namespaces, so it can't
		// honor the label.
		{name: "labeled, scoped", label: ns, scoped: true, wantAllow: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t, &v1alpha1.DispatchOps{Spec: v1alpha1.DispatchOpsSpec{
				TaskManager: v1alpha1.TaskManagerSpec{JobNamespace: "jobs"},
			}}, jobsNamespace(tc.label))
			e.r.AllowedJobNamespaces = tc.allowed
			if tc.scoped {
				e.r.Namespaces = []string{ns, "jobs"}
			}
			e.deploy()
			cr := e.cr()
			var role rbacv1.Role
			var binding rbacv1.RoleBinding
			var tm appsv1.Deployment
			gotRole := e.exists("jobs", foreignName(componentTaskManager), &role)
			gotBinding := e.exists("jobs", foreignName(componentTaskManager), &binding)
			gotTM := e.get("dops-task-manager", &tm)
			if gotRole != tc.wantAllow || gotBinding != tc.wantAllow || gotTM != tc.wantAllow {
				t.Errorf("role %t binding %t task manager %t, want %t", gotRole, gotBinding, gotTM, tc.wantAllow)
			}
			if !tc.wantAllow {
				e.wantCondition(cr, CondTaskManagerReady, metav1.ConditionFalse, "JobNamespaceNotAllowed")
				var api appsv1.Deployment
				if !e.get("dops-api", &api) {
					t.Error("the API server wasn't deployed")
				}
			}
		})
	}
}

func TestJobNamespaceOptOutRemovesRBAC(t *testing.T) {
	e := newEnv(t, &v1alpha1.DispatchOps{Spec: v1alpha1.DispatchOpsSpec{
		TaskManager: v1alpha1.TaskManagerSpec{JobNamespace: "jobs"},
	}}, jobsNamespace(ns))
	e.deploy()
	var role rbacv1.Role
	var np networkingv1.NetworkPolicy
	if !e.exists("jobs", foreignName(componentTaskManager), &role) || !e.exists("jobs", foreignName(componentJobs), &np) {
		t.Fatal("no Role or NetworkPolicy in the opted-in job namespace")
	}
	e.setNamespaceLabel("someone-else")
	if _, err := e.r.Reconcile(e.ctx, ctrl.Request{NamespacedName: e.key}); err != nil {
		t.Fatal(err)
	}
	if e.exists("jobs", foreignName(componentTaskManager), &role) || e.exists("jobs", foreignName(componentJobs), &np) {
		t.Error("RBAC or NetworkPolicy survived the namespace opting out")
	}
	e.wantCondition(e.cr(), CondTaskManagerReady, metav1.ConditionFalse, "JobNamespaceNotAllowed")
}

func TestRefusesToAdoptObjects(t *testing.T) {
	foreign := func(obj client.Object, labels map[string]string) client.Object {
		obj.SetNamespace("jobs")
		obj.SetName(foreignName(componentTaskManager))
		obj.SetLabels(labels)
		return obj
	}
	for name, labels := range map[string]map[string]string{
		"unlabeled": nil,
		// Labeled by someone who knows the UID, but not managed by the
		// operator.
		"owner label only": {ownerLabel: "uid-1"},
		"another owner":    {ownerLabel: "uid-2", "app.kubernetes.io/managed-by": ManagedBy},
	} {
		t.Run(name, func(t *testing.T) {
			existing := foreign(&rbacv1.Role{Rules: []rbacv1.PolicyRule{{Verbs: []string{"get"}, APIGroups: []string{""}, Resources: []string{"configmaps"}}}}, labels)
			e := newEnv(t, &v1alpha1.DispatchOps{Spec: v1alpha1.DispatchOpsSpec{
				TaskManager: v1alpha1.TaskManagerSpec{JobNamespace: "jobs"},
			}}, jobsNamespace(ns), existing)
			e.reconcile()
			e.databaseReady()
			e.reconcile()
			e.finishMigration("dispatch-ops:1.0", true)
			_, err := e.r.Reconcile(e.ctx, ctrl.Request{NamespacedName: e.key})
			if err == nil || !strings.Contains(err.Error(), "doesn't belong to DispatchOps") {
				t.Errorf("reconcile error = %v, want a refusal", err)
			}
			var role rbacv1.Role
			if !e.exists("jobs", foreignName(componentTaskManager), &role) || role.Rules[0].Resources[0] != "configmaps" {
				t.Errorf("the existing Role was changed: %+v", role.Rules)
			}
			e.wantCondition(e.cr(), CondReady, metav1.ConditionFalse, "Error")

			// Nor is it deleted when the DispatchOps is.
			if err := e.c.Delete(e.ctx, e.cr()); err != nil {
				t.Fatal(err)
			}
			if _, err := e.r.Reconcile(e.ctx, ctrl.Request{NamespacedName: e.key}); err != nil {
				t.Fatal(err)
			}
			if !e.exists("jobs", foreignName(componentTaskManager), &role) {
				t.Error("finalizing deleted a Role the operator didn't create")
			}
		})
	}

	// In its own namespace too.
	svc := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: "dops-postgres", Namespace: ns}}
	e := newEnv(t, &v1alpha1.DispatchOps{}, svc)
	if _, err := e.r.Reconcile(e.ctx, ctrl.Request{NamespacedName: e.key}); err == nil || !strings.Contains(err.Error(), "doesn't belong") {
		t.Errorf("reconcile error = %v, want a refusal", err)
	}
}

func TestPruneOnlyDeletesItsOwnObjects(t *testing.T) {
	ours := map[string]string{ownerLabel: "uid-1", "app.kubernetes.io/managed-by": ManagedBy}
	// Labeled as ours but named differently: not something the operator
	// made, so it stays.
	other := &rbacv1.Role{ObjectMeta: metav1.ObjectMeta{Name: "kube-admin", Namespace: "kube-system", Labels: ours}}
	// What an earlier job namespace left behind: deleted.
	stale := &rbacv1.Role{ObjectMeta: metav1.ObjectMeta{Name: foreignName(componentTaskManager), Namespace: "old-jobs", Labels: ours}}
	// What an operator before the names' hash left behind: deleted too.
	legacy := &rbacv1.Role{ObjectMeta: metav1.ObjectMeta{Name: "ops-dops-task-manager", Namespace: "older-jobs", Labels: ours}}
	e := newEnv(t, &v1alpha1.DispatchOps{}, other, stale, legacy)
	e.deploy()
	var role rbacv1.Role
	if !e.exists("kube-system", "kube-admin", &role) {
		t.Error("pruned a Role the operator didn't name")
	}
	if e.exists("old-jobs", foreignName(componentTaskManager), &role) {
		t.Error("the old job namespace's Role survived")
	}
	if e.exists("older-jobs", "ops-dops-task-manager", &role) {
		t.Error("the Role with the name from before the hash survived")
	}
}

func TestNetworkPolicies(t *testing.T) {
	// What an older operator made for job pods without an installation
	// label.
	old := &networkingv1.NetworkPolicy{ObjectMeta: metav1.ObjectMeta{
		Name: "dops-jobs-unlabeled", Namespace: ns,
		Labels: map[string]string{"app.kubernetes.io/managed-by": ManagedBy, "app.kubernetes.io/component": componentUnlabeledJobs},
		OwnerReferences: []metav1.OwnerReference{{APIVersion: v1alpha1.GroupVersion.String(), Kind: "DispatchOps",
			Name: "dops", UID: "uid-1", Controller: new(true)}},
	}}
	e := newEnv(t, &v1alpha1.DispatchOps{Spec: v1alpha1.DispatchOpsSpec{
		NetworkPolicy: v1alpha1.NetworkPolicySpec{JobEgressExcept: []string{"10.96.0.0/12", "fd00:10:96::/112"}},
	}}, old)
	e.deploy()

	var pg networkingv1.NetworkPolicy
	if !e.get("dops-postgres", &pg) {
		t.Fatal("no database NetworkPolicy")
	}
	var from []string
	for _, p := range pg.Spec.Ingress[0].From {
		from = append(from, p.PodSelector.MatchLabels["app.kubernetes.io/component"])
	}
	if got := strings.Join(from, ","); got != "api,task-manager,migrate" || pg.Spec.Ingress[0].Ports[0].Port.IntVal != 5432 ||
		len(pg.Spec.Ingress) != 1 || pg.Spec.PodSelector.MatchLabels["app.kubernetes.io/component"] != "postgres" {
		t.Errorf("database policy = %+v", pg.Spec)
	}

	var api, tm networkingv1.NetworkPolicy
	e.get("dops-api", &api)
	e.get("dops-task-manager", &tm)
	if len(api.Spec.Ingress) != 2 || api.Spec.Ingress[0].From != nil || api.Spec.Ingress[0].Ports[0].Port.IntVal != 8443 {
		t.Errorf("API policy = %+v", api.Spec.Ingress)
	}
	for _, p := range []networkingv1.NetworkPolicy{api, tm} {
		m := p.Spec.Ingress[len(p.Spec.Ingress)-1]
		if m.Ports[0].Port.IntVal != 9090 || m.From[0].PodSelector.MatchExpressions[0].Key != jobPodIDLabel ||
			m.From[0].PodSelector.MatchExpressions[0].Operator != metav1.LabelSelectorOpDoesNotExist {
			t.Errorf("%s metrics rule = %+v", p.Name, m)
		}
	}
	if len(tm.Spec.Ingress) != 1 {
		t.Errorf("task manager policy = %+v", tm.Spec.Ingress)
	}

	var jobs networkingv1.NetworkPolicy
	if !e.get("dops-jobs", &jobs) {
		t.Fatal("no job pod NetworkPolicy")
	}
	// R2-L4 recheck: every job pod in the job namespace, whatever its
	// installation label (the namespace is this installation's alone), so
	// one policy does; the second an older operator made is deleted.
	var unlabeled networkingv1.NetworkPolicy
	if e.get("dops-jobs-unlabeled", &unlabeled) {
		t.Error("the NetworkPolicy for unlabeled job pods survived")
	}
	s := jobs.Spec
	if sel := s.PodSelector; len(sel.MatchLabels) != 2 || sel.MatchLabels["app.kubernetes.io/component"] != "job" ||
		sel.MatchLabels["app.kubernetes.io/managed-by"] != "dispatch-ops" || len(sel.MatchExpressions) != 0 ||
		len(s.Ingress) != 0 || len(s.PolicyTypes) != 2 {
		t.Errorf("job policy = %+v", s)
	}
	if to := s.Egress[0].To[0]; to.NamespaceSelector.MatchLabels[corev1.LabelMetadataName] != ns ||
		to.PodSelector.MatchLabels["app.kubernetes.io/component"] != "api" || s.Egress[0].Ports[0].Port.IntVal != 8443 {
		t.Errorf("job egress to the API = %+v", s.Egress[0])
	}
	// R2-L5: DNS only to the cluster's DNS pods.
	if dns := s.Egress[1]; len(dns.To) != 1 || dns.Ports[0].Port.IntVal != 53 ||
		dns.To[0].NamespaceSelector.MatchLabels[corev1.LabelMetadataName] != "kube-system" ||
		dns.To[0].PodSelector.MatchLabels["k8s-app"] != "kube-dns" || dns.To[0].IPBlock != nil {
		t.Errorf("job DNS egress = %+v", s.Egress[1])
	}
	v4, v6 := s.Egress[2].To[0].IPBlock, s.Egress[2].To[1].IPBlock
	if v4.CIDR != "0.0.0.0/0" || strings.Join(v4.Except, ",") != "169.254.0.0/16,100.100.100.200/32,168.63.129.16/32,10.96.0.0/12" ||
		v6.CIDR != "::/0" || strings.Join(v6.Except, ",") != "fe80::/10,fd00:ec2::254/128,fd00:ec2::23/128,fd20:ce::254/128,fd00:10:96::/112" {
		t.Errorf("job egress = %+v / %+v", v4, v6)
	}

	// A bad CIDR is an error, not a policy that lets everything through.
	cr := e.cr()
	cr.Spec.NetworkPolicy.JobEgressExcept = []string{"10.0.0.0/33"}
	if err := e.c.Update(e.ctx, cr); err != nil {
		t.Fatal(err)
	}
	if _, err := e.r.Reconcile(e.ctx, ctrl.Request{NamespacedName: e.key}); err == nil {
		t.Error("an invalid CIDR reconciled")
	}

	// Turned off, they are deleted.
	cr = e.cr()
	cr.Spec.NetworkPolicy = v1alpha1.NetworkPolicySpec{Enabled: new(false)}
	if err := e.c.Update(e.ctx, cr); err != nil {
		t.Fatal(err)
	}
	e.reconcile()
	for _, name := range []string{"dops-postgres", "dops-api", "dops-task-manager", "dops-jobs", "dops-jobs-unlabeled"} {
		var np networkingv1.NetworkPolicy
		if e.get(name, &np) {
			t.Errorf("NetworkPolicy %s survived being turned off", name)
		}
	}
}

func TestManagedDatabaseIsHardened(t *testing.T) {
	e := newEnv(t, &v1alpha1.DispatchOps{})
	e.deploy()

	var pg corev1.Secret
	e.get("dops-postgres", &pg)
	if !strings.HasSuffix(pg.StringData["url"], "?sslmode=verify-full&sslrootcert=%2Fvar%2Frun%2Fdispatch-ops%2Fpostgres-ca%2Fca.crt") {
		t.Errorf("database url = %q", pg.StringData["url"])
	}
	var tlsSecret corev1.Secret
	if !e.get("dops-postgres-tls", &tlsSecret) || len(tlsSecret.OwnerReferences) != 0 {
		t.Fatal("no generated database certificate, or it is owned")
	}
	block, _ := pem.Decode(tlsSecret.Data["tls.crt"])
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	if err := cert.VerifyHostname("dops-postgres.ops.svc"); err != nil {
		t.Error(err)
	}

	var sts appsv1.StatefulSet
	e.get("dops-postgres", &sts)
	pod := sts.Spec.Template.Spec
	psc := pod.SecurityContext
	if !*psc.RunAsNonRoot || *psc.RunAsUser != 999 || *psc.RunAsGroup != 999 || *psc.FSGroup != 999 ||
		psc.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault {
		t.Errorf("pod security = %+v", psc)
	}
	for _, c := range append(pod.InitContainers, pod.Containers...) {
		sc := c.SecurityContext
		if *sc.AllowPrivilegeEscalation || !*sc.ReadOnlyRootFilesystem || !*sc.RunAsNonRoot || *sc.RunAsUser != 999 ||
			len(sc.Capabilities.Drop) != 1 || sc.Capabilities.Drop[0] != "ALL" || sc.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault {
			t.Errorf("%s security = %+v", c.Name, sc)
		}
		if c.Image != DefaultPostgresImage || !strings.Contains(c.Image, "@sha256:") {
			t.Errorf("%s image = %s", c.Name, c.Image)
		}
	}
	c := pod.Containers[0]
	if args := strings.Join(c.Args, " "); !strings.Contains(args, "-c ssl=on") || !strings.Contains(args, "ssl_key_file=/var/run/postgresql-tls/tls.key") {
		t.Errorf("postgres args = %s", args)
	}
	if c.Resources.Limits.Memory().String() != "1Gi" || c.Resources.Requests.Cpu().String() != "100m" {
		t.Errorf("default resources = %+v", c.Resources)
	}
	if !strings.Contains(pod.InitContainers[0].Command[2], "install -m 0600") {
		t.Errorf("init container = %v", pod.InitContainers[0].Command)
	}

	// Resources and image can be overridden.
	cr := e.cr()
	cr.Spec.Database.Managed = &v1alpha1.ManagedDatabase{
		Image:     "postgres:17.11",
		Resources: corev1.ResourceRequirements{Limits: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("4Gi")}},
	}
	if err := e.c.Update(e.ctx, cr); err != nil {
		t.Fatal(err)
	}
	e.reconcile()
	e.get("dops-postgres", &sts)
	c = sts.Spec.Template.Spec.Containers[0]
	if c.Image != "postgres:17.11" || c.Resources.Limits.Memory().String() != "4Gi" || len(c.Resources.Requests) != 0 {
		t.Errorf("overridden: image %s resources %+v", c.Image, c.Resources)
	}
}

func TestDatabaseURLMovesToTLS(t *testing.T) {
	// What an older operator generated, without the owner label, for an
	// installation it migrated.
	generated := map[string]string{"dispatch-ops.io/generated": "true"}
	old := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "dops-postgres", Namespace: ns, Annotations: generated},
		Data: map[string][]byte{
			"password": []byte("pw"),
			"url":      []byte("postgres://dispatch:pw@dops-postgres.ops.svc:5432/dispatch?sslmode=disable"),
		},
	}
	oldKey := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "dops-encryption-key", Namespace: ns, Annotations: generated},
		Data: map[string][]byte{"key": []byte("k")}}
	e := newEnv(t, &v1alpha1.DispatchOps{Status: v1alpha1.DispatchOpsStatus{Image: "dispatch-ops:1.0", EncryptionKeyGenerated: true}},
		old, oldKey)
	e.reconcile()
	e.databaseReady()
	e.reconcile()
	// Labeled as the installation's now.
	for _, name := range []string{"dops-postgres", "dops-encryption-key"} {
		var s corev1.Secret
		if e.get(name, &s); s.Labels[ownerLabel] != "uid-1" {
			t.Errorf("secret %s labels = %v", name, s.Labels)
		}
	}
	var api appsv1.Deployment
	e.get("dops-api", &api)
	if got := api.Spec.Template.Annotations[dbSSLModeAnnotation]; got != "disable" {
		t.Errorf("before the database rolled out: annotation %q", got)
	}

	// PostgreSQL serves TLS now: the URL requires it, and the servers
	// restart to pick that up.
	var sts appsv1.StatefulSet
	e.get("dops-postgres", &sts)
	sts.Status.ObservedGeneration, sts.Status.UpdatedReplicas = sts.Generation, 1
	if err := e.c.Status().Update(e.ctx, &sts); err != nil {
		t.Fatal(err)
	}
	e.reconcile()
	var s corev1.Secret
	e.get("dops-postgres", &s)
	if got := string(s.Data["url"]); got != "postgres://dispatch:pw@dops-postgres.ops.svc:5432/dispatch?"+
		"sslmode=verify-full&sslrootcert=%2Fvar%2Frun%2Fdispatch-ops%2Fpostgres-ca%2Fca.crt" {
		t.Errorf("url = %q", got)
	}
	e.get("dops-api", &api)
	if got := api.Spec.Template.Annotations[dbSSLModeAnnotation]; got != "verify-full" {
		t.Errorf("after: annotation %q", got)
	}
	if api.Spec.Template.Annotations["prometheus.io/scrape"] != "true" {
		t.Errorf("annotations = %v", api.Spec.Template.Annotations)
	}
}

func TestMaxJobOutput(t *testing.T) {
	limit := resource.MustParse("1Gi")
	e := newEnv(t, &v1alpha1.DispatchOps{Spec: v1alpha1.DispatchOpsSpec{API: v1alpha1.APISpec{MaxJobOutput: &limit}}})
	e.deploy()
	var api appsv1.Deployment
	e.get("dops-api", &api)
	if v, _ := envValue(api.Spec.Template.Spec.Containers[0], "DISPATCH_JOB_MAX_OUTPUT"); v.Value != "1Gi" {
		t.Errorf("DISPATCH_JOB_MAX_OUTPUT = %q", v.Value)
	}

	// Unset by default, so the server's own default applies.
	e = newEnv(t, &v1alpha1.DispatchOps{})
	e.deploy()
	e.get("dops-api", &api)
	if v, ok := envValue(api.Spec.Template.Spec.Containers[0], "DISPATCH_JOB_MAX_OUTPUT"); ok {
		t.Errorf("DISPATCH_JOB_MAX_OUTPUT = %q without maxJobOutput", v.Value)
	}
}

func TestTrustedProxies(t *testing.T) {
	e := newEnv(t, &v1alpha1.DispatchOps{Spec: v1alpha1.DispatchOpsSpec{
		API: v1alpha1.APISpec{TrustedProxies: []string{"10.0.0.0/8", "192.0.2.7"}},
	}})
	e.deploy()
	var api appsv1.Deployment
	e.get("dops-api", &api)
	if v, _ := envValue(api.Spec.Template.Spec.Containers[0], "DISPATCH_TRUSTED_PROXIES"); v.Value != "10.0.0.0/8,192.0.2.7" {
		t.Errorf("DISPATCH_TRUSTED_PROXIES = %q", v.Value)
	}

	cr := e.cr()
	cr.Spec.API.TrustedProxies = []string{"10.0.0.0/99"}
	if err := e.c.Update(e.ctx, cr); err != nil {
		t.Fatal(err)
	}
	if _, err := e.r.Reconcile(e.ctx, ctrl.Request{NamespacedName: e.key}); err == nil || !strings.Contains(err.Error(), "trustedProxies") {
		t.Errorf("reconcile error = %v", err)
	}

	// Unset by default.
	e = newEnv(t, &v1alpha1.DispatchOps{})
	e.deploy()
	e.get("dops-api", &api)
	if _, ok := envValue(api.Spec.Template.Spec.Containers[0], "DISPATCH_TRUSTED_PROXIES"); ok {
		t.Error("DISPATCH_TRUSTED_PROXIES set by default")
	}
}

func TestJobResources(t *testing.T) {
	e := newEnv(t, &v1alpha1.DispatchOps{Spec: v1alpha1.DispatchOpsSpec{TaskManager: v1alpha1.TaskManagerSpec{
		JobResources: &corev1.ResourceRequirements{
			Requests: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("512Mi")},
			Limits:   corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("4Gi")},
		},
		JobWorkSizeLimit: new(resource.MustParse("10Gi")),
	}}})
	e.deploy()
	var tm appsv1.Deployment
	e.get("dops-task-manager", &tm)
	for name, want := range map[string]string{
		"DISPATCH_JOB_CPU_REQUEST": "0", "DISPATCH_JOB_MEMORY_REQUEST": "512Mi",
		"DISPATCH_JOB_CPU_LIMIT": "0", "DISPATCH_JOB_MEMORY_LIMIT": "4Gi", "DISPATCH_JOB_WORK_SIZE_LIMIT": "10Gi",
	} {
		if v, _ := envValue(tm.Spec.Template.Spec.Containers[0], name); v.Value != want {
			t.Errorf("%s = %q, want %q", name, v.Value, want)
		}
	}
}

func TestJobNamespaceRBACIsCleanedUp(t *testing.T) {
	cr := &v1alpha1.DispatchOps{Spec: v1alpha1.DispatchOpsSpec{
		TaskManager: v1alpha1.TaskManagerSpec{JobNamespace: "jobs"},
	}}
	e := newEnv(t, cr, jobsNamespace(ns))
	e.reconcile()
	e.databaseReady()
	e.reconcile()
	e.finishMigration("dispatch-ops:1.0", true)
	cr = e.reconcile()

	var role rbacv1.Role
	key := types.NamespacedName{Namespace: "jobs", Name: foreignName(componentTaskManager)}
	if err := e.c.Get(e.ctx, key, &role); err != nil {
		t.Fatalf("no Role in the job namespace: %v", err)
	}
	if role.Labels[ownerLabel] != "uid-1" || len(role.OwnerReferences) != 0 {
		t.Errorf("role labels %v owners %v", role.Labels, role.OwnerReferences)
	}
	var np networkingv1.NetworkPolicy
	if !e.exists("jobs", foreignName(componentJobs), &np) || np.Labels[ownerLabel] != "uid-1" {
		t.Errorf("job NetworkPolicy in the job namespace: %+v", np.ObjectMeta)
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
	if e.exists("jobs", foreignName(componentJobs), &np) {
		t.Error("the job namespace NetworkPolicy survived")
	}
}

// R2 Low: the managed database's certificate comes from a long-lived CA,
// which is all the servers trust: renewing the certificate keeps the CA, so
// the servers (which read it only at start) keep reaching the database.
func TestPostgresCAStaysAcrossRenewal(t *testing.T) {
	e := newEnv(t, &v1alpha1.DispatchOps{})
	e.deploy()
	secret := func(name string) corev1.Secret {
		t.Helper()
		var s corev1.Secret
		if !e.get(name, &s) {
			t.Fatalf("no Secret %s", name)
		}
		return s
	}
	serial := func(s corev1.Secret) string {
		t.Helper()
		c, err := tlscert.ParseCertificate(s.Data["tls.crt"])
		if err != nil {
			t.Fatal(err)
		}
		return c.SerialNumber.String()
	}
	annotations := func() (api, db string) {
		var d appsv1.Deployment
		var sts appsv1.StatefulSet
		e.get("dops-api", &d)
		e.get("dops-postgres", &sts)
		return d.Spec.Template.Annotations[postgresCAAnnotation], sts.Spec.Template.Annotations[postgresCertAnnotation]
	}

	ca := secret("dops-postgres-ca")
	caCert, err := tlscert.ParseCertificate(ca.Data["ca.crt"])
	if err != nil || !caCert.IsCA || time.Until(caCert.NotAfter) < 9*365*24*time.Hour || len(ca.OwnerReferences) != 0 {
		t.Fatalf("CA = %+v, %v", caCert, err)
	}
	leaf := secret("dops-postgres-tls")
	if !issuedBy(&leaf, ca.Data["ca.crt"]) || string(leaf.Data["ca.crt"]) != string(ca.Data["ca.crt"]) {
		t.Error("the database's certificate isn't from the CA")
	}
	// The servers mount only the CA's certificate, never its key.
	var api appsv1.Deployment
	e.get("dops-api", &api)
	for _, v := range api.Spec.Template.Spec.Volumes {
		if v.Name == "postgres-ca" && (v.Secret == nil || v.Secret.SecretName != "dops-postgres-ca" ||
			len(v.Secret.Items) != 1 || v.Secret.Items[0].Key != "ca.crt") {
			t.Errorf("postgres-ca volume = %+v", v.VolumeSource)
		}
	}
	apiBefore, dbBefore := annotations()
	if apiBefore != hash16(ca.Data["ca.crt"]) || dbBefore != certHash(&leaf) {
		t.Errorf("annotations = %q, %q", apiBefore, dbBefore)
	}

	// The certificate nears expiry: renewed from the same CA. The database
	// restarts; the servers don't.
	certPEM, keyPEM, err := tlscert.Signed(install{DispatchOps: e.cr()}.postgresCertHosts(), renewBefore-time.Hour, ca.Data["ca.crt"], ca.Data["ca.key"])
	if err != nil {
		t.Fatal(err)
	}
	leaf.Data["tls.crt"], leaf.Data["tls.key"] = certPEM, keyPEM
	if err := e.c.Update(e.ctx, &leaf); err != nil {
		t.Fatal(err)
	}
	expiring := serial(leaf)
	e.reconcile()
	renewed := secret("dops-postgres-tls")
	if serial(renewed) == expiring || !issuedBy(&renewed, ca.Data["ca.crt"]) {
		t.Error("the certificate wasn't renewed from the CA")
	}
	if after := secret("dops-postgres-ca"); string(after.Data["ca.crt"]) != string(ca.Data["ca.crt"]) {
		t.Error("renewing the certificate changed the CA")
	}
	if apiAfter, dbAfter := annotations(); apiAfter != apiBefore || dbAfter != certHash(&renewed) {
		t.Errorf("after renewal: API %q (want %q, unchanged), database %q", apiAfter, apiBefore, dbAfter)
	}

	// A self-signed certificate from an older operator is replaced by one
	// from the CA.
	selfPEM, selfKey, err := tlscert.SelfSigned(install{DispatchOps: e.cr()}.postgresCertHosts(), selfSignedValidity)
	if err != nil {
		t.Fatal(err)
	}
	renewed.Data = map[string][]byte{"tls.crt": selfPEM, "tls.key": selfKey, "ca.crt": selfPEM}
	if err := e.c.Update(e.ctx, &renewed); err != nil {
		t.Fatal(err)
	}
	e.reconcile()
	if s := secret("dops-postgres-tls"); !issuedBy(&s, ca.Data["ca.crt"]) {
		t.Error("the self-signed certificate wasn't replaced")
	}

	// Only near its own expiry is the CA renewed, and the servers restart.
	oldPEM, oldKey, err := tlscert.CA("old", postgresCARenewBefore-time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	ca.Data = map[string][]byte{"ca.crt": oldPEM, "ca.key": oldKey}
	if err := e.c.Update(e.ctx, &ca); err != nil {
		t.Fatal(err)
	}
	e.reconcile()
	newCA := secret("dops-postgres-ca")
	if string(newCA.Data["ca.crt"]) == string(oldPEM) {
		t.Fatal("the expiring CA wasn't renewed")
	}
	if s := secret("dops-postgres-tls"); !issuedBy(&s, newCA.Data["ca.crt"]) {
		t.Error("the certificate wasn't reissued from the new CA")
	}
	if apiAfter, _ := annotations(); apiAfter != hash16(newCA.Data["ca.crt"]) {
		t.Errorf("servers' CA annotation = %q after the CA changed", apiAfter)
	}
}

// R2 Low: a generated encryption key that goes missing is never generated
// again: the installation reports Degraded until it is restored.
func TestEncryptionKeyIsNeverRegenerated(t *testing.T) {
	e := newEnv(t, &v1alpha1.DispatchOps{})
	e.reconcile()
	if !e.cr().Status.EncryptionKeyGenerated {
		t.Fatal("status.encryptionKeyGenerated isn't set")
	}
	var key corev1.Secret
	if !e.get("dops-encryption-key", &key) {
		t.Fatal("no encryption key")
	}
	if err := e.c.Delete(e.ctx, &key); err != nil {
		t.Fatal(err)
	}
	res, err := e.r.Reconcile(e.ctx, ctrl.Request{NamespacedName: e.key})
	if err != nil || res.RequeueAfter != pollInterval {
		t.Errorf("reconcile = %+v, %v", res, err)
	}
	var again corev1.Secret
	if e.get("dops-encryption-key", &again) {
		t.Fatal("a new encryption key was generated")
	}
	cr := e.cr()
	e.wantCondition(cr, CondDegraded, metav1.ConditionTrue, ReasonEncryptionKeyMissing)
	e.wantCondition(cr, CondReady, metav1.ConditionFalse, ReasonEncryptionKeyMissing)
	if c := e.condition(cr, CondDegraded); !strings.Contains(c.Message, "restore") {
		t.Errorf("message = %q", c.Message)
	}

	// Restored: back to normal.
	key.ResourceVersion = ""
	if err := e.c.Create(e.ctx, &key); err != nil {
		t.Fatal(err)
	}
	e.reconcile()
	if c := meta.FindStatusCondition(e.cr().Status.Conditions, CondDegraded); c != nil {
		t.Errorf("still %+v", c)
	}

	// An installation from before the status field: a migrated database
	// is enough to refuse.
	e = newEnv(t, &v1alpha1.DispatchOps{Status: v1alpha1.DispatchOpsStatus{Image: "dispatch-ops:0.9"}})
	e.reconcile()
	if e.get("dops-encryption-key", &again) {
		t.Error("a key was generated for a migrated installation")
	}
	e.wantCondition(e.cr(), CondDegraded, metav1.ConditionTrue, ReasonEncryptionKeyMissing)
}
