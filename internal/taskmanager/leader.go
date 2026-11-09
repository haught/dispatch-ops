package taskmanager

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/google/uuid"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/leaderelection"
	"k8s.io/client-go/tools/leaderelection/resourcelock"

	"dispatch-ops/internal/taskmanager/joblabels"
)

// KubeConfig returns the in-cluster configuration when running in a pod,
// and otherwise the kubeconfig (KUBECONFIG or ~/.kube/config). A non-empty
// kubeContext always uses the kubeconfig with that context.
func KubeConfig(kubeContext string) (*rest.Config, error) {
	if kubeContext == "" {
		if cfg, err := rest.InClusterConfig(); err == nil {
			return cfg, nil
		}
	}
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	cfg, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules,
		&clientcmd.ConfigOverrides{CurrentContext: kubeContext}).ClientConfig()
	if err != nil {
		return nil, fmt.Errorf("load Kubernetes config: %w", err)
	}
	return cfg, nil
}

// RunWithLeaderElection runs the manager only while this process holds the
// Lease, so that at most one task manager acts at a time. It returns when
// ctx is done or leadership is lost; the caller should then exit, so that a
// restarted process rejoins the election cleanly.
func RunWithLeaderElection(ctx context.Context, kube kubernetes.Interface, m *Manager, log *slog.Logger) error {
	host, _ := os.Hostname()
	identity := host + "_" + uuid.NewString()[:8]
	// One Lease per installation (see joblabels.LeaseName), so another
	// installation can't hold this one's.
	lock := &resourcelock.LeaseLock{
		LeaseMeta:  metav1.ObjectMeta{Name: joblabels.LeaseName(m.Config.Installation), Namespace: m.Config.Namespace},
		Client:     kube.CoordinationV1(),
		LockConfig: resourcelock.ResourceLockConfig{Identity: identity},
	}

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	runErr := make(chan error, 1)
	leaderelection.RunOrDie(runCtx, leaderelection.LeaderElectionConfig{
		Lock:            lock,
		ReleaseOnCancel: true,
		LeaseDuration:   15 * time.Second,
		RenewDeadline:   10 * time.Second,
		RetryPeriod:     2 * time.Second,
		Callbacks: leaderelection.LeaderCallbacks{
			OnStartedLeading: func(leading context.Context) {
				log.Info("became task manager leader", "identity", identity)
				runErr <- m.Run(leading)
				cancel() // the manager stopped: step down
			},
			OnStoppedLeading: func() {},
			OnNewLeader: func(id string) {
				if id != identity {
					log.Info("waiting for leadership", "leader", id)
				}
			},
		},
	})
	select {
	case err := <-runErr:
		if err != nil {
			return err
		}
	default:
	}
	if ctx.Err() == nil {
		return fmt.Errorf("lost task manager leadership")
	}
	return nil
}
