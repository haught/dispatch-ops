package operator

import (
	"crypto/sha256"
	"encoding/hex"
	"net/url"
	"strconv"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	"dispatch-ops/internal/operator/api/v1alpha1"
	"dispatch-ops/internal/tlscert"
)

// Components, used in names and labels.
const (
	componentAPI         = "api"
	componentTaskManager = "task-manager"
	componentDatabase    = "postgres"
	componentMigrate     = "migrate"
)

const (
	apiHTTPSPort  = 8443
	apiHTTPPort   = 8080
	metricsPort   = 9090
	postgresPort  = 5432
	postgresUser  = "dispatch"
	postgresDB    = "dispatch"
	adminPassPath = "/var/run/dispatch-ops/admin"
	// Where the API certificate Secret is mounted: in the API server, which
	// serves it, and in the task manager, which hands its CA to job pods.
	apiTLSPath   = "/var/run/dispatch-ops/tls"
	runnerCAPath = "/var/run/dispatch-ops/api-tls"
	// selfSignedValidity is how long the generated certificate is valid.
	selfSignedValidity = 10 * 365 * 24 * time.Hour
	// ManagedBy is the app.kubernetes.io/managed-by label of every object
	// the operator creates. (Job pods, which the task manager creates, are
	// labeled managed-by=dispatch-ops instead.)
	ManagedBy = "dispatch-ops-operator"
)

// install wraps a DispatchOps with the values the operator derives from it.
type install struct {
	*v1alpha1.DispatchOps
	// image is the server image the API server and task manager run: the
	// one whose migrations have completed.
	image string
}

func (in install) name(component string) string { return in.Name + "-" + component }

func (in install) labels(component string) map[string]string {
	return map[string]string{
		"app.kubernetes.io/name":       "dispatch-ops",
		"app.kubernetes.io/instance":   in.Name,
		"app.kubernetes.io/component":  component,
		"app.kubernetes.io/managed-by": ManagedBy,
	}
}

func (in install) selector(component string) map[string]string {
	return map[string]string{
		"app.kubernetes.io/name":      "dispatch-ops",
		"app.kubernetes.io/instance":  in.Name,
		"app.kubernetes.io/component": component,
	}
}

func (in install) meta(component, name string) metav1.ObjectMeta {
	return metav1.ObjectMeta{Name: name, Namespace: in.Namespace, Labels: in.labels(component)}
}

func (in install) jobNamespace() string {
	if ns := in.Spec.TaskManager.JobNamespace; ns != "" {
		return ns
	}
	return in.Namespace
}

// jobRBACName names the task manager's Role and RoleBinding in the job
// namespace. In another namespace it includes the DispatchOps namespace,
// so two installations can share a job namespace.
func (in install) jobRBACName() string {
	if in.jobNamespace() == in.Namespace {
		return in.name(componentTaskManager)
	}
	return in.Namespace + "-" + in.name(componentTaskManager)
}

// --- Secrets -----------------------------------------------------------------

func (in install) encryptionKeySecretName() string {
	if ref := in.Spec.EncryptionKeySecret; ref != nil {
		return ref.Name
	}
	return in.name("encryption-key")
}

func (in install) adminPasswordSecret() corev1.SecretKeySelector {
	if ref := in.Spec.Admin.PasswordSecret; ref != nil {
		return *ref
	}
	return corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: in.name("admin-password")}, Key: "password"}
}

func (in install) databaseURLSecret() corev1.SecretKeySelector {
	if ext := in.Spec.Database.External; ext != nil {
		return ext.URLSecret
	}
	return corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: in.name(componentDatabase)}, Key: "url"}
}

// postgresURL is the connection URL of the managed database.
func (in install) postgresURL(password string) string {
	return "postgres://" + postgresUser + ":" + password + "@" + in.name(componentDatabase) + "." + in.Namespace +
		".svc:" + strconv.Itoa(postgresPort) + "/" + postgresDB + "?sslmode=disable"
}

// generatedSecret is a Secret the operator creates once and never changes
// or deletes: losing one would lose data or access.
func (in install) generatedSecret(component, name string, data map[string]string) *corev1.Secret {
	s := &corev1.Secret{ObjectMeta: in.meta(component, name), Type: corev1.SecretTypeOpaque, StringData: data}
	s.Annotations = map[string]string{"dispatch-ops.io/generated": "true"}
	return s
}

// --- API certificate -------------------------------------------------------------

func (in install) tlsEnabled() bool {
	return in.Spec.API.TLS.Enabled == nil || *in.Spec.API.TLS.Enabled
}

// apiTLSSecretName is the Secret the API server serves: the user's, or the
// one the operator generates.
func (in install) apiTLSSecretName() string {
	if n := in.Spec.API.TLS.SecretName; n != "" {
		return n
	}
	return in.name(componentAPI) + "-tls"
}

func (in install) apiPort() int32 {
	if in.tlsEnabled() {
		return apiHTTPSPort
	}
	return apiHTTPPort
}

// apiPortName is the API's container and Service port name. Some ingress
// controllers (Traefik) use HTTPS to a backend port named https.
func (in install) apiPortName() string {
	if in.tlsEnabled() {
		return "https"
	}
	return "http"
}

// apiCertHosts are the names the generated certificate is valid for: the
// Service's, which job pods use, localhost (for port-forwarding), and the
// ingress and external hosts.
func (in install) apiCertHosts() []string {
	svc := in.name(componentAPI)
	hosts := []string{
		svc + "." + in.Namespace + ".svc", svc, svc + "." + in.Namespace,
		svc + "." + in.Namespace + ".svc.cluster.local", "localhost", "127.0.0.1",
	}
	if ing := in.Spec.API.Ingress; ing != nil && ing.Host != "" {
		hosts = append(hosts, ing.Host)
	}
	if u, err := url.Parse(in.Spec.ExternalURL); err == nil && u.Hostname() != "" {
		hosts = append(hosts, u.Hostname())
	}
	return hosts
}

// generatedTLSSecret is a new self-signed ECDSA P-384 certificate for the
// API server. Like the other generated Secrets, it is never changed.
func (in install) generatedTLSSecret() (*corev1.Secret, error) {
	certPEM, keyPEM, err := tlscert.SelfSigned(in.apiCertHosts(), selfSignedValidity)
	if err != nil {
		return nil, err
	}
	s := &corev1.Secret{
		ObjectMeta: in.meta(componentAPI, in.apiTLSSecretName()),
		Type:       corev1.SecretTypeTLS,
		Data: map[string][]byte{
			corev1.TLSCertKey:       certPEM,
			corev1.TLSPrivateKeyKey: keyPEM,
			// Self-signed: the certificate is its own CA.
			"ca.crt": certPEM,
		},
	}
	s.Annotations = map[string]string{"dispatch-ops.io/generated": "true"}
	return s, nil
}

func (in install) apiTLSVolume(name string) corev1.Volume {
	return corev1.Volume{Name: name, VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{
		SecretName: in.apiTLSSecretName(),
	}}}
}

// --- Server containers ---------------------------------------------------------

func secretEnv(name string, sel corev1.SecretKeySelector, optional bool) corev1.EnvVar {
	if optional {
		sel.Optional = new(true)
	}
	return corev1.EnvVar{Name: name, ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &sel}}
}

func keySelector(secret, key string) corev1.SecretKeySelector {
	return corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: secret}, Key: key}
}

// commonEnv is what every dispatch-ops process needs.
func (in install) commonEnv() []corev1.EnvVar {
	return []corev1.EnvVar{
		secretEnv("DISPATCH_DATABASE_URL", in.databaseURLSecret(), false),
		secretEnv("DISPATCH_ENCRYPTION_KEY", keySelector(in.encryptionKeySecretName(), "key"), false),
		secretEnv("DISPATCH_OLD_ENCRYPTION_KEYS", keySelector(in.encryptionKeySecretName(), "oldKeys"), true),
		{Name: "DISPATCH_LOG_LEVEL", Value: in.Spec.LogLevel},
		{Name: "DISPATCH_LOG_FORMAT", Value: "json"},
	}
}

func (in install) metricsEnabled() bool {
	return in.Spec.Metrics.Enabled == nil || *in.Spec.Metrics.Enabled
}

// withMetrics serves metrics from a long-running server container, or
// turns them off.
func (in install) withMetrics(pod *corev1.PodTemplateSpec) {
	c := &pod.Spec.Containers[0]
	if !in.metricsEnabled() {
		c.Env = append(c.Env, corev1.EnvVar{Name: "DISPATCH_METRICS_ADDR", Value: ""})
		return
	}
	c.Env = append(c.Env, corev1.EnvVar{Name: "DISPATCH_METRICS_ADDR", Value: ":" + strconv.Itoa(metricsPort)})
	c.Ports = append(c.Ports, corev1.ContainerPort{Name: "metrics", ContainerPort: metricsPort, Protocol: corev1.ProtocolTCP})
	pod.Annotations = map[string]string{
		"prometheus.io/scrape": "true",
		"prometheus.io/port":   strconv.Itoa(metricsPort),
		"prometheus.io/path":   "/metrics",
	}
}

func restrictedContainerSecurity() *corev1.SecurityContext {
	return &corev1.SecurityContext{
		AllowPrivilegeEscalation: new(false),
		ReadOnlyRootFilesystem:   new(true),
		RunAsNonRoot:             new(true),
		Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
	}
}

func (in install) serverPod(component string, containers []corev1.Container, initContainers []corev1.Container) corev1.PodTemplateSpec {
	for _, list := range [][]corev1.Container{containers, initContainers} {
		for i := range list {
			list[i].Image = in.image
			list[i].ImagePullPolicy = in.Spec.ImagePullPolicy
			list[i].SecurityContext = restrictedContainerSecurity()
		}
	}
	return corev1.PodTemplateSpec{
		ObjectMeta: metav1.ObjectMeta{Labels: in.labels(component)},
		Spec: corev1.PodSpec{
			ImagePullSecrets:             in.Spec.ImagePullSecrets,
			AutomountServiceAccountToken: new(false),
			EnableServiceLinks:           new(false),
			SecurityContext: &corev1.PodSecurityContext{
				RunAsNonRoot:   new(true),
				SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
			},
			InitContainers: initContainers,
			Containers:     containers,
		},
	}
}

// --- Migrations ----------------------------------------------------------------

// migrationJobName is unique per image, so each upgrade runs its own Job.
func (in install) migrationJobName(image string) string {
	sum := sha256.Sum256([]byte(image))
	return in.name(componentMigrate) + "-" + hex.EncodeToString(sum[:])[:10]
}

// migrationJob applies the image's database migrations, then creates the
// administrator if no user has their username.
func (in install) migrationJob(image string) *batchv1.Job {
	in.image = image
	pass := in.adminPasswordSecret()
	migrate := corev1.Container{Name: "migrate", Args: []string{"migrate", "up"}, Env: in.commonEnv()}
	admin := corev1.Container{
		Name: "admin",
		Args: []string{"user", "create", "-username", in.Spec.Admin.Username, "-display-name", "Administrator",
			"-admin", "-password-file", adminPassPath + "/password", "-if-missing"},
		Env:          in.commonEnv(),
		VolumeMounts: []corev1.VolumeMount{{Name: "admin", MountPath: adminPassPath, ReadOnly: true}},
	}
	pod := in.serverPod(componentMigrate, []corev1.Container{admin}, []corev1.Container{migrate})
	pod.Spec.RestartPolicy = corev1.RestartPolicyNever
	pod.Spec.Volumes = []corev1.Volume{{Name: "admin", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{
		SecretName: pass.Name, Items: []corev1.KeyToPath{{Key: pass.Key, Path: "password"}},
	}}}}
	job := &batchv1.Job{
		ObjectMeta: in.meta(componentMigrate, in.migrationJobName(image)),
		Spec: batchv1.JobSpec{
			BackoffLimit:            new(int32(3)),
			TTLSecondsAfterFinished: new(int32(7 * 24 * 3600)),
			Template:                pod,
		},
	}
	job.Annotations = map[string]string{"dispatch-ops.io/image": image}
	return job
}

// --- API server ----------------------------------------------------------------

func (in install) apiEnv() []corev1.EnvVar {
	env := append(in.commonEnv(),
		corev1.EnvVar{Name: "DISPATCH_LISTEN_ADDR", Value: ":" + strconv.Itoa(int(in.apiPort()))},
		corev1.EnvVar{Name: "DISPATCH_SECURE_COOKIES", Value: strconv.FormatBool(in.Spec.SecureCookies == nil || *in.Spec.SecureCookies)},
		corev1.EnvVar{Name: "DISPATCH_TLS", Value: strconv.FormatBool(in.tlsEnabled())},
	)
	if in.tlsEnabled() {
		env = append(env,
			corev1.EnvVar{Name: "DISPATCH_TLS_CERT_FILE", Value: apiTLSPath + "/" + corev1.TLSCertKey},
			corev1.EnvVar{Name: "DISPATCH_TLS_KEY_FILE", Value: apiTLSPath + "/" + corev1.TLSPrivateKeyKey},
		)
	}
	if d := in.Spec.SessionTTL; d != nil {
		env = append(env, corev1.EnvVar{Name: "DISPATCH_SESSION_TTL", Value: d.Duration.String()})
	}
	if in.Spec.ExternalURL != "" {
		env = append(env, corev1.EnvVar{Name: "DISPATCH_EXTERNAL_URL", Value: in.Spec.ExternalURL})
	}
	if o := in.Spec.OIDC; o != nil {
		env = append(env,
			corev1.EnvVar{Name: "DISPATCH_OIDC_ISSUER", Value: o.Issuer},
			corev1.EnvVar{Name: "DISPATCH_OIDC_CLIENT_ID", Value: o.ClientID},
		)
		if o.ClientSecret != nil {
			env = append(env, secretEnv("DISPATCH_OIDC_CLIENT_SECRET", *o.ClientSecret, false))
		}
		for _, kv := range [][2]string{
			{"DISPATCH_OIDC_SCOPES", strings.Join(o.Scopes, ",")},
			{"DISPATCH_OIDC_USERNAME_CLAIM", o.UsernameClaim},
			{"DISPATCH_OIDC_GROUPS_CLAIM", o.GroupsClaim},
			{"DISPATCH_OIDC_DISPLAY_NAME", o.DisplayName},
		} {
			if kv[1] != "" {
				env = append(env, corev1.EnvVar{Name: kv[0], Value: kv[1]})
			}
		}
	}
	return env
}

func (in install) apiDeployment() *appsv1.Deployment {
	scheme := corev1.URISchemeHTTP
	if in.tlsEnabled() {
		// The kubelet doesn't verify probe certificates.
		scheme = corev1.URISchemeHTTPS
	}
	probe := func(path string) *corev1.Probe {
		return &corev1.Probe{
			ProbeHandler: corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{
				Path: path, Port: intstr.FromString(in.apiPortName()), Scheme: scheme,
			}},
			PeriodSeconds: 10, TimeoutSeconds: 3, FailureThreshold: 3,
		}
	}
	c := corev1.Container{
		Name:           componentAPI,
		Args:           []string{"serve"},
		Env:            in.apiEnv(),
		Ports:          []corev1.ContainerPort{{Name: in.apiPortName(), ContainerPort: in.apiPort(), Protocol: corev1.ProtocolTCP}},
		ReadinessProbe: probe("/readyz"),
		LivenessProbe:  probe("/healthz"),
		Resources:      in.Spec.API.Resources,
	}
	if in.tlsEnabled() {
		c.VolumeMounts = []corev1.VolumeMount{{Name: "tls", MountPath: apiTLSPath, ReadOnly: true}}
	}
	pod := in.serverPod(componentAPI, []corev1.Container{c}, nil)
	if in.tlsEnabled() {
		pod.Spec.Volumes = []corev1.Volume{in.apiTLSVolume("tls")}
	}
	in.withMetrics(&pod)
	return &appsv1.Deployment{
		ObjectMeta: in.meta(componentAPI, in.name(componentAPI)),
		Spec: appsv1.DeploymentSpec{
			Replicas: in.Spec.API.Replicas,
			Selector: &metav1.LabelSelector{MatchLabels: in.selector(componentAPI)},
			Template: pod,
		},
	}
}

func (in install) apiService() *corev1.Service {
	t := in.Spec.API.ServiceType
	if t == "" {
		t = corev1.ServiceTypeClusterIP
	}
	return &corev1.Service{
		ObjectMeta: in.meta(componentAPI, in.name(componentAPI)),
		Spec: corev1.ServiceSpec{
			Type:     t,
			Selector: in.selector(componentAPI),
			Ports: []corev1.ServicePort{{
				Name: in.apiPortName(), Port: in.apiPort(), TargetPort: intstr.FromString(in.apiPortName()), Protocol: corev1.ProtocolTCP,
			}},
		},
	}
}

// apiURL is the API's address inside the cluster, which job pods use.
func (in install) apiURL() string {
	scheme := "http://"
	if in.tlsEnabled() {
		scheme = "https://"
	}
	return scheme + in.name(componentAPI) + "." + in.Namespace + ".svc:" + strconv.Itoa(int(in.apiPort()))
}

// backendProtocolAnnotation tells ingress-nginx to use HTTPS to the pods.
const backendProtocolAnnotation = "nginx.ingress.kubernetes.io/backend-protocol"

func (in install) ingress() *networkingv1.Ingress {
	spec := in.Spec.API.Ingress
	pathType := networkingv1.PathTypePrefix
	ing := &networkingv1.Ingress{
		ObjectMeta: in.meta(componentAPI, in.Name),
		Spec: networkingv1.IngressSpec{
			IngressClassName: spec.ClassName,
			Rules: []networkingv1.IngressRule{{
				Host: spec.Host,
				IngressRuleValue: networkingv1.IngressRuleValue{HTTP: &networkingv1.HTTPIngressRuleValue{
					Paths: []networkingv1.HTTPIngressPath{{
						Path: "/", PathType: &pathType,
						Backend: networkingv1.IngressBackend{Service: &networkingv1.IngressServiceBackend{
							Name: in.name(componentAPI), Port: networkingv1.ServiceBackendPort{Name: in.apiPortName()},
						}},
					}},
				}},
			}},
		},
	}
	ing.Annotations = spec.Annotations
	if in.tlsEnabled() {
		if _, set := ing.Annotations[backendProtocolAnnotation]; !set {
			ing.Annotations = make(map[string]string, len(spec.Annotations)+1)
			for k, v := range spec.Annotations {
				ing.Annotations[k] = v
			}
			ing.Annotations[backendProtocolAnnotation] = "HTTPS"
		}
	}
	if spec.TLSSecretName != "" {
		ing.Spec.TLS = []networkingv1.IngressTLS{{Hosts: []string{spec.Host}, SecretName: spec.TLSSecretName}}
	}
	return ing
}

// --- Task manager --------------------------------------------------------------

func (in install) taskManagerServiceAccount() *corev1.ServiceAccount {
	return &corev1.ServiceAccount{ObjectMeta: in.meta(componentTaskManager, in.name(componentTaskManager))}
}

// taskManagerRole is what the task manager does in the job namespace: run
// Kubernetes Jobs with their token and pull Secrets, watch their pods, and
// hold the leader Lease.
func (in install) taskManagerRole() *rbacv1.Role {
	m := in.meta(componentTaskManager, in.jobRBACName())
	m.Namespace = in.jobNamespace()
	return &rbacv1.Role{
		ObjectMeta: m,
		Rules: []rbacv1.PolicyRule{
			{APIGroups: []string{"batch"}, Resources: []string{"jobs"}, Verbs: []string{"get", "list", "watch", "create", "delete"}},
			{APIGroups: []string{""}, Resources: []string{"pods"}, Verbs: []string{"get", "list", "watch"}},
			{APIGroups: []string{""}, Resources: []string{"secrets"}, Verbs: []string{"get", "list", "create", "delete", "patch"}},
			{APIGroups: []string{"coordination.k8s.io"}, Resources: []string{"leases"}, Verbs: []string{"get", "create", "update"}},
		},
	}
}

func (in install) taskManagerRoleBinding() *rbacv1.RoleBinding {
	m := in.meta(componentTaskManager, in.jobRBACName())
	m.Namespace = in.jobNamespace()
	return &rbacv1.RoleBinding{
		ObjectMeta: m,
		RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "Role", Name: in.jobRBACName()},
		Subjects: []rbacv1.Subject{{
			Kind: rbacv1.ServiceAccountKind, Name: in.name(componentTaskManager), Namespace: in.Namespace,
		}},
	}
}

func (in install) taskManagerDeployment() *appsv1.Deployment {
	tm := in.Spec.TaskManager
	env := append(in.commonEnv(),
		corev1.EnvVar{Name: "DISPATCH_RUNNER_API_URL", Value: in.apiURL()},
		corev1.EnvVar{Name: "DISPATCH_JOB_NAMESPACE", Value: in.jobNamespace()},
		corev1.EnvVar{Name: "DISPATCH_LEADER_ELECTION", Value: "true"},
	)
	if tm.MaxConcurrentJobs != nil {
		env = append(env, corev1.EnvVar{Name: "DISPATCH_MAX_CONCURRENT_JOBS", Value: strconv.Itoa(int(*tm.MaxConcurrentJobs))})
	}
	if tm.JobTTL != nil {
		env = append(env, corev1.EnvVar{Name: "DISPATCH_JOB_TTL", Value: tm.JobTTL.Duration.String()})
	}
	if tm.JobStartTimeout != nil {
		env = append(env, corev1.EnvVar{Name: "DISPATCH_JOB_START_TIMEOUT", Value: tm.JobStartTimeout.Duration.String()})
	}
	// For links to jobs in notifications.
	if in.Spec.ExternalURL != "" {
		env = append(env, corev1.EnvVar{Name: "DISPATCH_EXTERNAL_URL", Value: in.Spec.ExternalURL})
	}
	if r := in.Spec.Retention.Jobs; r != nil {
		env = append(env, corev1.EnvVar{Name: "DISPATCH_JOB_RETENTION", Value: r.Duration.String()})
	}
	if r := in.Spec.Retention.AuditLog; r != nil {
		env = append(env, corev1.EnvVar{Name: "DISPATCH_AUDIT_RETENTION", Value: r.Duration.String()})
	}
	c := corev1.Container{Name: componentTaskManager, Args: []string{"task-manager"}, Env: env, Resources: tm.Resources}
	if in.tlsEnabled() {
		// Job pods trust the API's certificate; see tlscert.ReadTrustBundle.
		c.Env = append(c.Env, corev1.EnvVar{Name: "DISPATCH_RUNNER_API_TLS_DIR", Value: runnerCAPath})
		c.VolumeMounts = []corev1.VolumeMount{{Name: "api-tls", MountPath: runnerCAPath, ReadOnly: true}}
	}
	pod := in.serverPod(componentTaskManager, []corev1.Container{c}, nil)
	if in.tlsEnabled() {
		pod.Spec.Volumes = []corev1.Volume{in.apiTLSVolume("api-tls")}
	}
	in.withMetrics(&pod)
	pod.Spec.ServiceAccountName = in.name(componentTaskManager)
	pod.Spec.AutomountServiceAccountToken = new(true)
	return &appsv1.Deployment{
		ObjectMeta: in.meta(componentTaskManager, in.name(componentTaskManager)),
		Spec: appsv1.DeploymentSpec{
			Replicas: tm.Replicas,
			Selector: &metav1.LabelSelector{MatchLabels: in.selector(componentTaskManager)},
			Template: pod,
		},
	}
}

// --- Managed database ----------------------------------------------------------

func (in install) postgresService() *corev1.Service {
	return &corev1.Service{
		ObjectMeta: in.meta(componentDatabase, in.name(componentDatabase)),
		Spec: corev1.ServiceSpec{
			Selector: in.selector(componentDatabase),
			Ports: []corev1.ServicePort{{
				Name: "postgres", Port: postgresPort, TargetPort: intstr.FromString("postgres"), Protocol: corev1.ProtocolTCP,
			}},
		},
	}
}

func (in install) postgresStatefulSet() *appsv1.StatefulSet {
	db := in.Spec.Database.Managed
	image := db.Image
	if image == "" {
		image = "postgres:17"
	}
	size := resource.MustParse("10Gi")
	if db.StorageSize != nil {
		size = *db.StorageSize
	}
	ready := &corev1.Probe{
		ProbeHandler: corev1.ProbeHandler{Exec: &corev1.ExecAction{
			Command: []string{"pg_isready", "-h", "127.0.0.1", "-U", postgresUser, "-d", postgresDB},
		}},
		PeriodSeconds: 5, TimeoutSeconds: 3, FailureThreshold: 3,
	}
	c := corev1.Container{
		Name:  componentDatabase,
		Image: image,
		Env: []corev1.EnvVar{
			{Name: "POSTGRES_USER", Value: postgresUser},
			{Name: "POSTGRES_DB", Value: postgresDB},
			secretEnv("POSTGRES_PASSWORD", keySelector(in.name(componentDatabase), "password"), false),
			{Name: "PGDATA", Value: "/var/lib/postgresql/data/pgdata"},
		},
		Ports:          []corev1.ContainerPort{{Name: "postgres", ContainerPort: postgresPort, Protocol: corev1.ProtocolTCP}},
		ReadinessProbe: ready,
		Resources:      db.Resources,
		VolumeMounts:   []corev1.VolumeMount{{Name: "data", MountPath: "/var/lib/postgresql/data"}},
	}
	return &appsv1.StatefulSet{
		ObjectMeta: in.meta(componentDatabase, in.name(componentDatabase)),
		Spec: appsv1.StatefulSetSpec{
			Replicas:    new(int32(1)),
			ServiceName: in.name(componentDatabase),
			Selector:    &metav1.LabelSelector{MatchLabels: in.selector(componentDatabase)},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: in.labels(componentDatabase)},
				Spec: corev1.PodSpec{
					AutomountServiceAccountToken: new(false),
					EnableServiceLinks:           new(false),
					// The postgres image's user, so it can write the volume.
					SecurityContext: &corev1.PodSecurityContext{FSGroup: new(int64(999))},
					Containers:      []corev1.Container{c},
				},
			},
			VolumeClaimTemplates: []corev1.PersistentVolumeClaim{{
				ObjectMeta: metav1.ObjectMeta{Name: "data", Labels: in.labels(componentDatabase)},
				Spec: corev1.PersistentVolumeClaimSpec{
					AccessModes:      []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
					StorageClassName: db.StorageClassName,
					Resources:        corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceStorage: size}},
				},
			}},
			// The data outlives the DispatchOps.
			PersistentVolumeClaimRetentionPolicy: &appsv1.StatefulSetPersistentVolumeClaimRetentionPolicy{
				WhenDeleted: appsv1.RetainPersistentVolumeClaimRetentionPolicyType,
				WhenScaled:  appsv1.RetainPersistentVolumeClaimRetentionPolicyType,
			},
		},
	}
}
