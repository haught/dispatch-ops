package taskmanager

import (
	"encoding/base64"
	"encoding/json"
	"strings"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"dispatch-ops/internal/store"
)

// Labels and annotations on the Kubernetes objects the task manager creates.
const (
	LabelManagedBy  = "app.kubernetes.io/managed-by"
	LabelComponent  = "app.kubernetes.io/component"
	LabelJobID      = "dispatch-ops/job-id"
	AnnotationName  = "dispatch-ops/job-name"
	managedByValue  = "dispatch-ops"
	componentJobPod = "job"
)

// Paths inside job pods. The runner reads these from its environment.
const (
	tokenDir     = "/var/run/dispatch-ops"
	tokenFile    = tokenDir + "/token"
	apiCAFile    = tokenDir + "/ca.crt"
	workDir      = "/runner"
	secretsDir   = workDir + "/secrets"
	containerApp = "runner"
)

// managedSelector selects every object the task manager created.
var managedSelector = LabelManagedBy + "=" + managedByValue + "," + LabelComponent + "=" + componentJobPod

// k8sJobName is the name of the Kubernetes Job (and the prefix of its
// Secrets) for a dispatch-ops job.
func k8sJobName(id string) string { return "dispatch-job-" + id }

func tokenSecretName(id string) string { return k8sJobName(id) + "-token" }
func pullSecretName(id string) string  { return k8sJobName(id) + "-pull" }

func labelsFor(job store.Job) map[string]string {
	return map[string]string{
		LabelManagedBy: managedByValue,
		LabelComponent: componentJobPod,
		LabelJobID:     job.ID.String(),
	}
}

// buildTokenSecret holds the job's one-time runner token and, if not empty,
// the certificates the runner trusts to reach the API (ca.crt).
func buildTokenSecret(job store.Job, namespace, token string, ca []byte) *corev1.Secret {
	data := map[string][]byte{"token": []byte(token)}
	if len(ca) > 0 {
		data["ca.crt"] = ca
	}
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: tokenSecretName(job.ID.String()), Namespace: namespace, Labels: labelsFor(job)},
		Type:       corev1.SecretTypeOpaque,
		Data:       data,
	}
}

// buildPullSecret turns a registry credential into an image pull secret.
func buildPullSecret(job store.Job, namespace string, inputs map[string]string) (*corev1.Secret, error) {
	server := inputs["server"]
	auth := base64.StdEncoding.EncodeToString([]byte(inputs["username"] + ":" + inputs["password"]))
	cfg, err := json.Marshal(map[string]any{
		"auths": map[string]any{server: map[string]string{
			"username": inputs["username"], "password": inputs["password"], "auth": auth,
		}},
	})
	if err != nil {
		return nil, err
	}
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: pullSecretName(job.ID.String()), Namespace: namespace, Labels: labelsFor(job)},
		Type:       corev1.SecretTypeDockerConfigJson,
		Data:       map[string][]byte{corev1.DockerConfigJsonKey: cfg},
	}, nil
}

// buildJob returns the Kubernetes Job that runs a dispatch-ops job: one pod,
// never retried, running the execution environment image, whose entrypoint
// is the runner. withCA means the token Secret holds ca.crt.
func buildJob(job store.Job, cfg Config, withPullSecret, withCA bool) *batchv1.Job {
	id := job.ID.String()
	ee := job.Config.ExecutionEnvironment
	labels := labelsFor(job)

	// A backstop in case both the runner's own timeout and the task
	// manager's reconcile loop fail to stop the job.
	deadline := int64(job.Config.TimeoutSeconds) + int64(cfg.StartTimeout.Seconds()) + 300
	ttl := int32(cfg.JobTTL.Seconds())
	tokenMode := int32(0o444) // the image's (non-root) user must be able to read it
	secretsSize := resource.MustParse("16Mi")

	pod := corev1.PodSpec{
		RestartPolicy:                 corev1.RestartPolicyNever,
		AutomountServiceAccountToken:  new(false),
		EnableServiceLinks:            new(false),
		TerminationGracePeriodSeconds: new(int64(30)),
		SecurityContext: &corev1.PodSecurityContext{
			RunAsNonRoot:   new(true),
			SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
		},
		Containers: []corev1.Container{{
			Name:            containerApp,
			Image:           ee.Image,
			ImagePullPolicy: corev1.PullPolicy(ee.PullPolicy),
			Env: []corev1.EnvVar{
				{Name: "DISPATCH_JOB_ID", Value: id},
				{Name: "DISPATCH_API_URL", Value: cfg.RunnerAPIURL},
				{Name: "DISPATCH_JOB_TOKEN_FILE", Value: tokenFile},
				{Name: "DISPATCH_WORKDIR", Value: workDir},
			},
			VolumeMounts: []corev1.VolumeMount{
				{Name: "token", MountPath: tokenDir, ReadOnly: true},
				{Name: "work", MountPath: workDir},
				{Name: "secrets", MountPath: secretsDir},
			},
			SecurityContext: &corev1.SecurityContext{
				AllowPrivilegeEscalation: new(false),
				Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
			},
		}},
		Volumes: []corev1.Volume{
			{Name: "token", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{
				SecretName: tokenSecretName(id), DefaultMode: &tokenMode,
			}}},
			{Name: "work", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
			// Credentials are written here, in memory, never to the node's disk.
			{Name: "secrets", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{
				Medium: corev1.StorageMediumMemory, SizeLimit: &secretsSize,
			}}},
		},
	}
	if withCA {
		c := &pod.Containers[0]
		c.Env = append(c.Env, corev1.EnvVar{Name: "DISPATCH_API_CA_FILE", Value: apiCAFile})
	}
	if withPullSecret {
		pod.ImagePullSecrets = []corev1.LocalObjectReference{{Name: pullSecretName(id)}}
	}

	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:        k8sJobName(id),
			Namespace:   cfg.Namespace,
			Labels:      labels,
			Annotations: map[string]string{AnnotationName: truncate(job.Name, 253)},
		},
		Spec: batchv1.JobSpec{
			BackoffLimit:            new(int32(0)),
			ActiveDeadlineSeconds:   &deadline,
			TTLSecondsAfterFinished: &ttl,
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec:       pod,
			},
		},
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return strings.ToValidUTF8(s[:n], "")
}
