// Command dispatch-operator installs and upgrades Dispatch Ops on
// Kubernetes from DispatchOps resources.
package main

import (
	"context"
	"flag"
	"fmt"
	"maps"
	"net/netip"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/validation"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	"dispatch-ops/internal/operator"
	"dispatch-ops/internal/operator/api/v1alpha1"
	"dispatch-ops/internal/operator/crd"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

func main() {
	var (
		serverImage          string
		metricsAddr          string
		probeAddr            string
		leaderElect          bool
		watchNamespace       string
		allowedJobNamespaces string
		defaultJobNamespaces string
		noPodSecurity        bool
		dnsNamespaces        string
		dnsPods              string
		dnsCIDRs             string
		showVersion          bool
	)
	flag.StringVar(&serverImage, "server-image", "ghcr.io/haught/dispatch-ops/server:"+version,
		"dispatch-ops server image for DispatchOps resources that don't name one")
	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8080", `metrics address; "0" turns metrics off`)
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "health probe address")
	flag.BoolVar(&leaderElect, "leader-elect", true, "elect one active operator replica with a Lease")
	flag.StringVar(&watchNamespace, "watch-namespaces", os.Getenv("WATCH_NAMESPACES"),
		"comma-separated namespaces to watch; empty watches all (WATCH_NAMESPACES)")
	flag.StringVar(&allowedJobNamespaces, "allowed-job-namespaces", os.Getenv("ALLOWED_JOB_NAMESPACES"),
		"namespaces DispatchOps may run jobs in besides their own, each with the namespaces whose DispatchOps may: "+
			"<job namespace>=<namespace>[|<namespace>...], comma-separated, e.g. jobs-a=team-a,jobs-b=team-b|team-c "+
			"(ALLOWED_JOB_NAMESPACES). None may be in --watch-namespaces. When watching all namespaces, a namespace "+
			"labeled "+operator.AllowJobsFromLabel+"=<namespace> also allows that namespace's")
	flag.StringVar(&defaultJobNamespaces, "default-job-namespaces", os.Getenv("DEFAULT_JOB_NAMESPACES"),
		"the job namespace a DispatchOps runs its jobs in when its spec.taskManager.jobNamespace is unset, per "+
			"namespace: <namespace>=<job namespace>, comma-separated, e.g. team-a=team-a-jobs (DEFAULT_JOB_NAMESPACES). "+
			"Each is also allowed for its namespace, as in --allowed-job-namespaces. Without one, a DispatchOps runs "+
			"its jobs in its own namespace")
	flag.BoolVar(&noPodSecurity, "allow-job-namespaces-without-pod-security", envBool("ALLOW_JOB_NAMESPACES_WITHOUT_POD_SECURITY"),
		"run jobs in namespaces that aren't labeled pod-security.kubernetes.io/enforce=restricted (or baseline) "+
			"(ALLOW_JOB_NAMESPACES_WITHOUT_POD_SECURITY). Whoever can create a DispatchOps can then create any pod, "+
			"privileged ones included, in its job namespace")
	flag.StringVar(&dnsNamespaces, "job-dns-namespace-selector", envOr("JOB_DNS_NAMESPACE_SELECTOR", corev1.LabelMetadataName+"=kube-system"),
		"label selector of the namespace of the cluster DNS pods job pods may query (JOB_DNS_NAMESPACE_SELECTOR); "+
			"empty with --job-dns-pod-selector set means every namespace")
	flag.StringVar(&dnsPods, "job-dns-pod-selector", envOr("JOB_DNS_POD_SELECTOR", "k8s-app=kube-dns"),
		"label selector of the cluster DNS pods job pods may query on port 53 or 5353 (JOB_DNS_POD_SELECTOR); "+
			"with this and --job-dns-namespace-selector both empty and no --job-dns-cidrs, job pods may query DNS anywhere")
	flag.StringVar(&dnsCIDRs, "job-dns-cidrs", os.Getenv("JOB_DNS_CIDRS"),
		"comma-separated CIDRs of more DNS resolvers job pods may query, e.g. NodeLocal DNSCache's 169.254.20.10/32, "+
			"and in its iptables mode the kube-dns Service's ClusterIP (JOB_DNS_CIDRS)")
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

	jobDNS, err := parseJobDNS(dnsNamespaces, dnsPods, splitList(dnsCIDRs))
	if err != nil {
		log.Error(err, "invalid job DNS settings")
		os.Exit(1)
	}

	watched := splitList(watchNamespace)
	allowedJobNS, err := parseAllowedJobNamespaces(allowedJobNamespaces)
	if err != nil {
		log.Error(err, "invalid allowed job namespaces")
		os.Exit(1)
	}
	defaultJobNS, err := parseDefaultJobNamespaces(defaultJobNamespaces)
	if err != nil {
		log.Error(err, "invalid default job namespaces")
		os.Exit(1)
	}
	allowDefaultJobNamespaces(allowedJobNS, defaultJobNS)
	if err := checkAllowedJobNamespaces(allowedJobNS, watched); err != nil {
		log.Error(err, "invalid job namespaces")
		os.Exit(1)
	}

	// Bring the DispatchOps CRD up to this version: Helm installs it from
	// the chart's crds/ but never upgrades it.
	cfg := ctrl.GetConfigOrDie()
	direct, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		log.Error(err, "unable to create a client")
		os.Exit(1)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	err = crd.Ensure(ctx, direct, version)
	cancel()
	if err != nil {
		log.Error(err, "unable to update the DispatchOps CRD")
		os.Exit(1)
	}

	// Only cache the objects the operator made, not every Job or
	// Deployment in the cluster.
	ours := cache.ByObject{Label: labels.SelectorFromSet(labels.Set{"app.kubernetes.io/managed-by": operator.ManagedBy})}
	// What it makes in job namespaces.
	inJobNS := ours
	cacheOpts := cache.Options{ByObject: map[client.Object]cache.ByObject{
		&appsv1.Deployment{}: ours, &appsv1.StatefulSet{}: ours, &batchv1.Job{}: ours,
		&corev1.Service{}: ours, &corev1.ServiceAccount{}: ours, &networkingv1.Ingress{}: ours,
	}}
	// Namespaces the operator may access, when not all of them: with
	// --watch-namespaces, the chart grants it Roles only there and in the
	// allowed job namespaces.
	var accessible []string
	if len(watched) > 0 {
		cacheOpts.DefaultNamespaces = map[string]cache.Config{}
		inJobNS.Namespaces = map[string]cache.Config{}
		for _, ns := range watched {
			cacheOpts.DefaultNamespaces[ns] = cache.Config{}
			inJobNS.Namespaces[ns] = cache.Config{}
		}
		for ns := range allowedJobNS {
			inJobNS.Namespaces[ns] = cache.Config{}
		}
		accessible = slices.Sorted(maps.Keys(inJobNS.Namespaces))
	}
	for _, o := range []client.Object{&rbacv1.Role{}, &rbacv1.RoleBinding{}, &networkingv1.NetworkPolicy{}} {
		cacheOpts.ByObject[o] = inJobNS
	}

	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
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
		Client:               mgr.GetClient(),
		APIReader:            mgr.GetAPIReader(),
		Scheme:               mgr.GetScheme(),
		DefaultImage:         serverImage,
		AllowedJobNamespaces: allowedJobNS,
		DefaultJobNamespaces: defaultJobNS,
		Namespaces:           accessible,
		JobDNS:               jobDNS,

		AllowJobNamespacesWithoutPodSecurity: noPodSecurity,
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

	if noPodSecurity {
		log.Info("WARNING: job namespaces need not enforce Pod Security; whoever can create a DispatchOps can create " +
			"privileged pods in its job namespace")
	}
	log.Info("starting", "version", version, "default_server_image", serverImage, "watch_namespaces", watchNamespace,
		"allowed_job_namespaces", allowedJobNamespaces, "default_job_namespaces", defaultJobNamespaces)
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		log.Error(err, "operator stopped")
		os.Exit(1)
	}
}

// parseJobDNS makes the operator's JobDNS from its flags.
func parseJobDNS(namespaces, pods string, cidrs []string) (*operator.JobDNS, error) {
	dns := &operator.JobDNS{CIDRs: cidrs}
	for _, f := range []struct {
		flag, value string
		dst         **metav1.LabelSelector
	}{
		{"--job-dns-namespace-selector", namespaces, &dns.NamespaceSelector},
		{"--job-dns-pod-selector", pods, &dns.PodSelector},
	} {
		if strings.TrimSpace(f.value) == "" {
			continue
		}
		sel, err := metav1.ParseToLabelSelector(f.value)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", f.flag, err)
		}
		*f.dst = sel
	}
	for _, c := range cidrs {
		if _, err := netip.ParsePrefix(c); err != nil {
			return nil, fmt.Errorf("--job-dns-cidrs: %w", err)
		}
	}
	return dns, nil
}

func envOr(name, def string) string {
	if v, ok := os.LookupEnv(name); ok {
		return v
	}
	return def
}

func envBool(name string) bool {
	v, _ := strconv.ParseBool(os.Getenv(name))
	return v
}

// parseAllowedJobNamespaces parses --allowed-job-namespaces: comma-separated
// entries <job namespace>=<namespace>[|<namespace>...], each naming the
// namespaces whose DispatchOps may run jobs in that job namespace. An entry
// without "=" is refused rather than read as open to every namespace.
func parseAllowedJobNamespaces(s string) (map[string][]string, error) {
	out := map[string][]string{}
	for _, item := range splitList(s) {
		jobNS, list, ok := strings.Cut(item, "=")
		jobNS = strings.TrimSpace(jobNS)
		if !ok {
			return nil, fmt.Errorf("--allowed-job-namespaces: %q doesn't say which namespaces' DispatchOps may run jobs there; "+
				"write %s=<namespace>[|<namespace>...]", item, jobNS)
		}
		if errs := validation.IsDNS1123Label(jobNS); len(errs) > 0 {
			return nil, fmt.Errorf("--allowed-job-namespaces: %q is not a namespace: %s", jobNS, strings.Join(errs, "; "))
		}
		if _, dup := out[jobNS]; dup {
			return nil, fmt.Errorf("--allowed-job-namespaces: %s is listed twice; list all of its namespaces in one entry, separated by |", jobNS)
		}
		var sources []string
		for src := range strings.SplitSeq(list, "|") {
			src = strings.TrimSpace(src)
			if src == "" {
				return nil, fmt.Errorf("--allowed-job-namespaces: %q has an empty namespace", item)
			}
			if errs := validation.IsDNS1123Label(src); len(errs) > 0 {
				return nil, fmt.Errorf("--allowed-job-namespaces: %q is not a namespace: %s", src, strings.Join(errs, "; "))
			}
			sources = append(sources, src)
		}
		slices.Sort(sources)
		out[jobNS] = slices.Compact(sources)
	}
	return out, nil
}

// parseDefaultJobNamespaces parses --default-job-namespaces: comma-separated
// entries <namespace>=<job namespace>, at most one per namespace.
func parseDefaultJobNamespaces(s string) (map[string]string, error) {
	out := map[string]string{}
	for _, item := range splitList(s) {
		src, jobNS, ok := strings.Cut(item, "=")
		src, jobNS = strings.TrimSpace(src), strings.TrimSpace(jobNS)
		if !ok || jobNS == "" {
			return nil, fmt.Errorf("--default-job-namespaces: %q doesn't name a job namespace; write <namespace>=<job namespace>", item)
		}
		for _, ns := range []string{src, jobNS} {
			if errs := validation.IsDNS1123Label(ns); len(errs) > 0 {
				return nil, fmt.Errorf("--default-job-namespaces: %q is not a namespace: %s", ns, strings.Join(errs, "; "))
			}
		}
		if src == jobNS {
			return nil, fmt.Errorf("--default-job-namespaces: %s=%s: a DispatchOps runs jobs in its own namespace "+
				"without a default; leave it out", src, jobNS)
		}
		if _, dup := out[src]; dup {
			return nil, fmt.Errorf("--default-job-namespaces: %s is listed twice", src)
		}
		out[src] = jobNS
	}
	return out, nil
}

// allowDefaultJobNamespaces adds each default job namespace to allowed, for
// the namespace it is the default of.
func allowDefaultJobNamespaces(allowed map[string][]string, defaults map[string]string) {
	for src, jobNS := range defaults {
		sources := append(allowed[jobNS], src)
		slices.Sort(sources)
		allowed[jobNS] = slices.Compact(sources)
	}
}

// checkAllowedJobNamespaces refuses an allowed job namespace that is also
// watched: one that hosts a DispatchOps, and with it that installation's
// encryption key and database Secrets, mustn't be another's job namespace,
// whose task manager can create pods there that mount them. With watched
// namespaces, the namespaces allowed to use a job namespace must be among
// them.
func checkAllowedJobNamespaces(allowed map[string][]string, watched []string) error {
	for _, jobNS := range slices.Sorted(maps.Keys(allowed)) {
		if slices.Contains(watched, jobNS) {
			return fmt.Errorf("--allowed-job-namespaces: %s is also in --watch-namespaces. A namespace that hosts DispatchOps "+
				"can't be another installation's job namespace, whose task manager could create pods there that mount its "+
				"Secrets; give jobs a namespace of their own", jobNS)
		}
		if len(watched) == 0 {
			continue
		}
		for _, src := range allowed[jobNS] {
			if !slices.Contains(watched, src) {
				return fmt.Errorf("--allowed-job-namespaces: %s=%s: %s is not in --watch-namespaces", jobNS,
					strings.Join(allowed[jobNS], "|"), src)
			}
		}
	}
	return nil
}

// splitList splits a comma-separated list, dropping empty items.
func splitList(s string) []string {
	var out []string
	for item := range strings.SplitSeq(s, ",") {
		if item = strings.TrimSpace(item); item != "" {
			out = append(out, item)
		}
	}
	return out
}
