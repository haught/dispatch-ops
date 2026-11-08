// Command dispatch-operator installs and upgrades Dispatch Ops on
// Kubernetes from DispatchOps resources.
package main

import (
	"flag"
	"fmt"
	"os"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	"dispatch-ops/internal/operator"
	"dispatch-ops/internal/operator/api/v1alpha1"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

func main() {
	var (
		serverImage    string
		metricsAddr    string
		probeAddr      string
		leaderElect    bool
		watchNamespace string
		showVersion    bool
	)
	flag.StringVar(&serverImage, "server-image", "dispatch-ops:"+version,
		"dispatch-ops server image for DispatchOps resources that don't name one")
	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8080", `metrics address; "0" turns metrics off`)
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "health probe address")
	flag.BoolVar(&leaderElect, "leader-elect", true, "elect one active operator replica with a Lease")
	flag.StringVar(&watchNamespace, "watch-namespaces", os.Getenv("WATCH_NAMESPACES"),
		"comma-separated namespaces to watch; empty watches all (WATCH_NAMESPACES)")
	flag.BoolVar(&showVersion, "version", false, "print the version and exit")
	opts := zap.Options{}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()
	if showVersion {
		fmt.Println(version)
		return
	}
	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))
	log := ctrl.Log.WithName("setup")

	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		panic(err)
	}
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		panic(err)
	}

	// Only cache the objects the operator made, not every Job or
	// Deployment in the cluster.
	ours := cache.ByObject{Label: labels.SelectorFromSet(labels.Set{"app.kubernetes.io/managed-by": operator.ManagedBy})}
	cacheOpts := cache.Options{ByObject: map[client.Object]cache.ByObject{
		&appsv1.Deployment{}: ours, &appsv1.StatefulSet{}: ours, &batchv1.Job{}: ours,
		&corev1.Service{}: ours, &corev1.ServiceAccount{}: ours, &networkingv1.Ingress{}: ours,
		&rbacv1.Role{}: ours, &rbacv1.RoleBinding{}: ours,
	}}
	if watchNamespace != "" {
		cacheOpts.DefaultNamespaces = map[string]cache.Config{}
		for ns := range strings.SplitSeq(watchNamespace, ",") {
			if ns = strings.TrimSpace(ns); ns != "" {
				cacheOpts.DefaultNamespaces[ns] = cache.Config{}
			}
		}
	}

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                 scheme,
		Cache:                  cacheOpts,
		Metrics:                metricsserver.Options{BindAddress: metricsAddr},
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         leaderElect,
		LeaderElectionID:       "dispatch-ops-operator.dispatch-ops.io",
	})
	if err != nil {
		log.Error(err, "unable to start the manager")
		os.Exit(1)
	}
	if err := (&operator.Reconciler{
		Client:       mgr.GetClient(),
		APIReader:    mgr.GetAPIReader(),
		Scheme:       mgr.GetScheme(),
		DefaultImage: serverImage,
	}).SetupWithManager(mgr); err != nil {
		log.Error(err, "unable to set up the controller")
		os.Exit(1)
	}
	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		log.Error(err, "unable to set up health check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		log.Error(err, "unable to set up ready check")
		os.Exit(1)
	}

	log.Info("starting", "version", version, "default_server_image", serverImage, "watch_namespaces", watchNamespace)
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		log.Error(err, "operator stopped")
		os.Exit(1)
	}
}
