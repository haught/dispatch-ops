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
	"dispatch-ops/internal/taskmanager/joblabels"
)

// Labels and annotations on the Kubernetes objects the task manager
// creates. The operator selects job pods by the same labels (see joblabels).
const (
	LabelManagedBy    = joblabels.ManagedBy
	LabelComponent    = joblabels.Component
	LabelJobID        = joblabels.JobID
	LabelInstallation = joblabels.Installation
	AnnotationName    = "dispatch-ops/job-name"
	managedByValue    = joblabels.ManagedByValue
	componentJobPod   = joblabels.ComponentValue
)

// Paths inside job pods. The runner reads these from its environment.
const (
	caDir        = "/var/run/dispatch-ops"
	apiCAFile    = caDir + "/ca.crt"
	workDir      = "/runner"
	secretsDir   = workDir + "/secrets"
	containerApp = "runner"
)

// managedSelector selects the objects every task manager created, whatever
// its installation (see Manager.isOurs).
var managedSelector = LabelManagedBy + "=" + managedByValue + "," + LabelComponent + "=" + componentJobPod

// k8sJobName is the name of the Kubernetes Job (and the prefix of its
// Secrets) for a dispatch-ops job.
func k8sJobName(id string) string { return "dispatch-job-" + id }

func tokenSecretName(id string) string { return k8sJobName(id) + "-token" }
func pullSecretName(id string) string  { return k8sJobName(id) + "-pull" }

// labelsFor labels a job's objects, with the installation when there is
// one.
func labelsFor(job store.Job, installation string) map[string]string {
	l := map[string]string{
		LabelManagedBy: managedByValue,
		LabelComponent: componentJobPod,
		LabelJobID:     job.ID.String(),
	}
	if installation != "" {
		l[LabelInstallation] = installation
	}
	return l
}

// buildTokenSecret holds the job's one-time runner token and, if not empty,
// the certificates the runner trusts to reach the API (ca.crt).
func buildTokenSecret(job store.Job, cfg Config, token string, ca []byte) *corev1.Secret {
	data := map[string][]byte{"token": []byte(token)}
	if len(ca) > 0 {
		data["ca.crt"] = ca
	}
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: tokenSecretName(job.ID.String()), Namespace: cfg.Namespace, Labels: labelsFor(job, cfg.Installation)},
		Type:       corev1.SecretTypeOpaque,
		Data:       data,
	}
}

// buildPullSecret turns a registry credential into an image pull secret.
func buildPullSecret(job store.Job, cfg Config, inputs map[string]string) (*corev1.Secret, error) {
	server := inputs["server"]
	auth := base64.StdEncoding.EncodeToString([]byte(inputs["username"] + ":" + inputs["password"]))
	dockerCfg, err := json.Marshal(map[string]any{
		"auths": map[string]any{server: map[string]string{
			"username": inputs["username"], "password": inputs["password"], "auth": auth,
		}},
	})
	if err != nil {
		return nil, err
	}
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: pullSecretName(job.ID.String()), Namespace: cfg.Namespace, Labels: labelsFor(job, cfg.Installation)},
		Type:       corev1.SecretTypeDockerConfigJson,
		Data:       map[string][]byte{corev1.DockerConfigJsonKey: dockerCfg},
	}, nil
}

// buildJob returns the Kubernetes Job that runs a dispatch-ops job: one pod,
// never retried, running the execution environment image, whose entrypoint
// is the runner. withCA means the token Secret holds ca.crt. It is created
// suspended, so its Secrets can be created with it as their owner before its
// pod needs them (see Manager.launch).
func buildJob(job store.Job, cfg Config, withPullSecret, withCA bool) *batchv1.Job {
	id := job.ID.String()
	ee := job.Config.ExecutionEnvironment
	labels := labelsFor(job, cfg.Installation)

	// A backstop in case both the runner's own timeout and the task
	// manager's reconcile loop fail to stop the job. It counts from when the
	// Job is unsuspended, not created.
	deadline := int64(job.Config.TimeoutSeconds) + int64(cfg.StartTimeout.Seconds()) + 300
	ttl := int32(cfg.JobTTL.Seconds())
	caMode := int32(0o444) // the image's (non-root) user must be able to read it
	// Credentials, plus the job's inventory and extra vars, which can hold
	// secrets too; an inventory report is at most 32 MiB. Memory-backed, so
	// it counts against the job's memory limit only as far as it is used.
	secretsSize := resource.MustParse("64Mi")
	var workSize *resource.Quantity
	if !cfg.JobWorkSizeLimit.IsZero() {
		workSize = new(cfg.JobWorkSizeLimit.DeepCopy())
	}

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
				// An environment variable, not a mounted file: the job's code
				// runs as the runner's user, so it could read any file the
				// runner can, but the runner's environment is hidden from it
				// once the runner makes itself non-dumpable (runner.Harden).
				{Name: "DISPATCH_JOB_TOKEN", ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: tokenSecretName(id)}, Key: "token",
				}}},
				{Name: "DISPATCH_WORKDIR", Value: workDir},
			},
			VolumeMounts: []corev1.VolumeMount{
				{Name: "work", MountPath: workDir},
				{Name: "secrets", MountPath: secretsDir},
			},
			SecurityContext: &corev1.SecurityContext{
				AllowPrivilegeEscalation: new(false),
				Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
			},
			// So one job can't starve the node of CPU or memory.
			Resources: *cfg.JobResources.DeepCopy(),
			// A runner that fails before it reaches the API (say, one too old
			// for this task manager) explains itself in the job's failure.
			TerminationMessagePolicy: corev1.TerminationMessageFallbackToLogsOnError,
		}},
		Volumes: []corev1.Volume{
			// Capped, so one job can't fill the node's disk.
			{Name: "work", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{SizeLimit: workSize}}},
			// Credentials are written here, in memory, never to the node's disk.
			{Name: "secrets", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{
				Medium: corev1.StorageMediumMemory, SizeLimit: &secretsSize,
			}}},
		},
	}
	if withCA {
		// Only the certificates, never the token, from the job's Secret.
		c := &pod.Containers[0]
		c.Env = append(c.Env, corev1.EnvVar{Name: "DISPATCH_API_CA_FILE", Value: apiCAFile})
		c.VolumeMounts = append(c.VolumeMounts, corev1.VolumeMount{Name: "api-ca", MountPath: caDir, ReadOnly: true})
		pod.Volumes = append(pod.Volumes, corev1.Volume{Name: "api-ca", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{
			SecretName: tokenSecretName(id), DefaultMode: &caMode,
			Items: []corev1.KeyToPath{{Key: "ca.crt", Path: "ca.crt"}},
		}}})
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
			Suspend:                 new(true),
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
