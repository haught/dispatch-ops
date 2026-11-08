// Package v1alpha1 is the DispatchOps custom resource: one installation of
// Dispatch Ops, which the operator deploys and keeps up to date.
//
// +kubebuilder:object:generate=true
// +groupName=dispatch-ops.io
package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/scheme"
)

// GroupVersion is the API group and version of DispatchOps.
var GroupVersion = schema.GroupVersion{Group: "dispatch-ops.io", Version: "v1alpha1"}

// SchemeBuilder registers the types with a scheme.
var SchemeBuilder = &scheme.Builder{GroupVersion: GroupVersion}

// AddToScheme adds the types to a scheme.
var AddToScheme = SchemeBuilder.AddToScheme

func init() {
	SchemeBuilder.Register(&DispatchOps{}, &DispatchOpsList{})
}

// DispatchOps is one installation of Dispatch Ops: its API server, task
// manager and database.
//
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=dops,categories=dispatch-ops
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Image",type=string,JSONPath=`.status.image`
// +kubebuilder:printcolumn:name="Reason",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].reason`,priority=1
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type DispatchOps struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   DispatchOpsSpec   `json:"spec,omitempty"`
	Status DispatchOpsStatus `json:"status,omitempty"`
}

// DispatchOpsList is a list of DispatchOps.
//
// +kubebuilder:object:root=true
type DispatchOpsList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []DispatchOps `json:"items"`
}

// DispatchOpsSpec is the desired installation.
type DispatchOpsSpec struct {
	// Image is the dispatch-ops server image. Defaults to the image the
	// operator was released with. Changing it upgrades the installation:
	// database migrations run first, then the API server and task manager
	// roll out.
	// +optional
	Image string `json:"image,omitempty"`
	// +kubebuilder:validation:Enum=Always;IfNotPresent;Never
	// +kubebuilder:default=IfNotPresent
	// +optional
	ImagePullPolicy corev1.PullPolicy `json:"imagePullPolicy,omitempty"`
	// +optional
	ImagePullSecrets []corev1.LocalObjectReference `json:"imagePullSecrets,omitempty"`

	// +kubebuilder:default={}
	// +optional
	API APISpec `json:"api,omitempty"`
	// +kubebuilder:default={}
	// +optional
	TaskManager TaskManagerSpec `json:"taskManager,omitempty"`
	// Database is where all state is kept.
	// +kubebuilder:default={managed:{}}
	// +optional
	Database DatabaseSpec `json:"database,omitempty"`

	// EncryptionKeySecret names a Secret whose "key" holds the credential
	// encryption key (base64 of 32 bytes; `dispatch-ops gen-key` makes one),
	// and optionally "oldKeys" (comma-separated) during a key rotation.
	// Without it, the operator generates <name>-encryption-key. Losing the
	// key loses every stored credential, so back it up; the operator never
	// deletes it.
	// +optional
	EncryptionKeySecret *corev1.LocalObjectReference `json:"encryptionKeySecret,omitempty"`

	// +kubebuilder:default={}
	// +optional
	Admin AdminSpec `json:"admin,omitempty"`

	// ExternalURL is the server's URL as browsers reach it, e.g.
	// https://dispatch.example.com. Single sign-on needs it.
	// +kubebuilder:validation:Pattern=`^https?://[^/]+(/.*)?$`
	// +optional
	ExternalURL string `json:"externalURL,omitempty"`
	// +optional
	OIDC *OIDCSpec `json:"oidc,omitempty"`
	// SecureCookies marks the session cookie Secure. Turn off only when
	// browsers reach the server over plain HTTP.
	// +kubebuilder:default=true
	// +optional
	SecureCookies *bool `json:"secureCookies,omitempty"`
	// SessionTTL is how long a browser session lasts, e.g. 12h.
	// +optional
	SessionTTL *metav1.Duration `json:"sessionTTL,omitempty"`
	// +kubebuilder:validation:Enum=debug;info;warn;error
	// +kubebuilder:default=info
	// +optional
	LogLevel string `json:"logLevel,omitempty"`

	// +kubebuilder:default={}
	// +optional
	Retention RetentionSpec `json:"retention,omitempty"`
	// +kubebuilder:default={}
	// +optional
	Metrics MetricsSpec `json:"metrics,omitempty"`
}

// RetentionSpec is how long old data is kept. The task manager deletes
// what is older every hour.
type RetentionSpec struct {
	// Jobs is how long finished jobs (with their output and notifications)
	// are kept, e.g. 2160h for 90 days. 0s keeps them forever. Defaults to
	// 90 days.
	// +optional
	Jobs *metav1.Duration `json:"jobs,omitempty"`
	// AuditLog is how long audit log entries are kept. Unset or 0s keeps
	// them forever.
	// +optional
	AuditLog *metav1.Duration `json:"auditLog,omitempty"`
}

// MetricsSpec configures the Prometheus metrics the API server and task
// manager serve at :9090/metrics, on a port of their own (named
// "metrics") that the API Service doesn't expose.
type MetricsSpec struct {
	// Enabled serves metrics, and adds prometheus.io/scrape annotations to
	// the pods.
	// +kubebuilder:default=true
	// +optional
	Enabled *bool `json:"enabled,omitempty"`
}

// APISpec configures the API server, which also serves the web UI.
type APISpec struct {
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:default=2
	// +optional
	Replicas *int32 `json:"replicas,omitempty"`
	// +optional
	Resources corev1.ResourceRequirements `json:"resources,omitempty"`
	// ServiceType of the <name>-api Service.
	// +kubebuilder:validation:Enum=ClusterIP;NodePort;LoadBalancer
	// +kubebuilder:default=ClusterIP
	// +optional
	ServiceType corev1.ServiceType `json:"serviceType,omitempty"`
	// Ingress, if set, exposes the API server and UI.
	// +optional
	Ingress *IngressSpec `json:"ingress,omitempty"`
}

// IngressSpec configures an Ingress for the API server.
type IngressSpec struct {
	// +kubebuilder:validation:MinLength=1
	Host string `json:"host"`
	// +optional
	ClassName *string `json:"className,omitempty"`
	// TLSSecretName enables TLS with this certificate Secret.
	// +optional
	TLSSecretName string `json:"tlsSecretName,omitempty"`
	// +optional
	Annotations map[string]string `json:"annotations,omitempty"`
}

// TaskManagerSpec configures the task manager, which runs jobs as
// Kubernetes Jobs. Replicas elect one leader; more than one only adds a
// standby.
type TaskManagerSpec struct {
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:default=1
	// +optional
	Replicas *int32 `json:"replicas,omitempty"`
	// +optional
	Resources corev1.ResourceRequirements `json:"resources,omitempty"`
	// JobNamespace is where job pods run. It must exist. Defaults to the
	// DispatchOps namespace.
	// +optional
	JobNamespace string `json:"jobNamespace,omitempty"`
	// MaxConcurrentJobs limits jobs waiting or running at once.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:default=10
	// +optional
	MaxConcurrentJobs *int32 `json:"maxConcurrentJobs,omitempty"`
	// JobTTL is how long finished Kubernetes Jobs are kept, e.g. 1h.
	// +optional
	JobTTL *metav1.Duration `json:"jobTTL,omitempty"`
	// JobStartTimeout is how long a job's pod may take to start, e.g. 10m.
	// +optional
	JobStartTimeout *metav1.Duration `json:"jobStartTimeout,omitempty"`
}

// DatabaseSpec chooses the PostgreSQL database: an external one, or one
// the operator runs. Exactly one must be set.
//
// +kubebuilder:validation:XValidation:rule="has(self.external) != has(self.managed)",message="set exactly one of external and managed"
type DatabaseSpec struct {
	// +optional
	External *ExternalDatabase `json:"external,omitempty"`
	// +optional
	Managed *ManagedDatabase `json:"managed,omitempty"`
}

// ExternalDatabase is a PostgreSQL database the operator doesn't manage.
type ExternalDatabase struct {
	// URLSecret holds the connection URL, e.g.
	// postgres://user:password@host:5432/dispatch?sslmode=require.
	URLSecret corev1.SecretKeySelector `json:"urlSecret"`
}

// ManagedDatabase is a single PostgreSQL instance the operator runs in a
// StatefulSet. Its volume and generated password are kept when the
// DispatchOps is deleted. It has no backups or replication; use an external
// database where that matters.
type ManagedDatabase struct {
	// +kubebuilder:default="postgres:17"
	// +optional
	Image string `json:"image,omitempty"`
	// +kubebuilder:default="10Gi"
	// +optional
	StorageSize *resource.Quantity `json:"storageSize,omitempty"`
	// +optional
	StorageClassName *string `json:"storageClassName,omitempty"`
	// +optional
	Resources corev1.ResourceRequirements `json:"resources,omitempty"`
}

// AdminSpec is the local system administrator created at install.
type AdminSpec struct {
	// Username of the administrator, created if no user has it.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:default=admin
	// +optional
	Username string `json:"username,omitempty"`
	// PasswordSecret holds the administrator's initial password (at least
	// 12 characters). Without it, the operator generates
	// <name>-admin-password. Changing the Secret later doesn't change the
	// password.
	// +optional
	PasswordSecret *corev1.SecretKeySelector `json:"passwordSecret,omitempty"`
}

// OIDCSpec configures single sign-on.
type OIDCSpec struct {
	// +kubebuilder:validation:MinLength=1
	Issuer string `json:"issuer"`
	// +kubebuilder:validation:MinLength=1
	ClientID string `json:"clientID"`
	// +optional
	ClientSecret *corev1.SecretKeySelector `json:"clientSecret,omitempty"`
	// Scopes besides openid. Defaults to profile and email.
	// +optional
	Scopes []string `json:"scopes,omitempty"`
	// +optional
	UsernameClaim string `json:"usernameClaim,omitempty"`
	// +optional
	GroupsClaim string `json:"groupsClaim,omitempty"`
	// +optional
	DisplayName string `json:"displayName,omitempty"`
}

// DispatchOpsStatus is what the operator last observed.
type DispatchOpsStatus struct {
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// Image is the server image whose migrations have run, which the API
	// server and task manager run (or are rolling out to).
	// +optional
	Image string `json:"image,omitempty"`
	// Conditions: DatabaseReady, Migrated, APIReady, TaskManagerReady and
	// Ready (all of them).
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}
