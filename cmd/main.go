// Command ephora-operator runs the PreviewEnvironment controller.
//
// This replaces the zero-code generic `helm-operator` binary the Operator
// SDK Helm plugin scaffolds by default: DAT's requirements (dynamic
// per-instance chart source, TTL-based expiry, a real cleanup finalizer,
// dedicated namespace + NetworkPolicy/ResourceQuota provisioning, periodic
// orphan-namespace detection) go beyond what that generic reconciler can do
// against a single statically-watched chart. ADR-01 explicitly anticipates
// this as a fallback ("passage partiel en Go si la complexité augmente");
// see internal/controller/previewenvironment_controller.go for detail. The
// operator still only ever drives Kubernetes through namespace/NetworkPolicy/
// ResourceQuota bookkeeping and Helm SDK install/upgrade/uninstall calls —
// it does not become a general-purpose, hand-rolled controller.
package main

import (
	"flag"
	"os"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	ephoraiov1alpha1 "github.com/ngatcheu/ephora-operator/api/v1alpha1"
	"github.com/ngatcheu/ephora-operator/internal/controller"
)

var scheme = runtime.NewScheme()

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(ephoraiov1alpha1.AddToScheme(scheme))
}

func main() {
	var metricsAddr string
	var probeAddr string
	var enableLeaderElection bool
	var orphanSweepInterval time.Duration
	var cleanupTimeout time.Duration

	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8080", "The address the metrics endpoint binds to.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "The address the health/readiness probes bind to.")
	flag.BoolVar(&enableLeaderElection, "leader-elect", false,
		"Enable leader election. Required when running more than one operator replica.")
	flag.DurationVar(&orphanSweepInterval, "orphan-sweep-interval", controller.DefaultOrphanSweepInterval,
		"How often to sweep for orphaned preview namespaces (DAT §4.5).")
	flag.DurationVar(&cleanupTimeout, "cleanup-timeout", controller.DefaultCleanupTimeout,
		"How long the cleanup finalizer waits for namespace teardown before force-releasing (DAT §7).")

	opts := zap.Options{Development: false}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))
	setupLog := ctrl.Log.WithName("setup")

	restConfig := ctrl.GetConfigOrDie()

	mgr, err := ctrl.NewManager(restConfig, ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsserver.Options{BindAddress: metricsAddr},
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         enableLeaderElection,
		LeaderElectionID:       "ephora-operator-lock.ephora.io",
	})
	if err != nil {
		setupLog.Error(err, "unable to start manager")
		os.Exit(1)
	}

	if err = (&controller.PreviewEnvironmentReconciler{
		Client:         mgr.GetClient(),
		Scheme:         mgr.GetScheme(),
		RESTConfig:     mgr.GetConfig(),
		Recorder:       mgr.GetEventRecorderFor("ephora-operator"),
		CleanupTimeout: cleanupTimeout,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "PreviewEnvironment")
		os.Exit(1)
	}

	if err = mgr.Add(&controller.OrphanSweeper{
		Client:   mgr.GetClient(),
		Interval: orphanSweepInterval,
	}); err != nil {
		setupLog.Error(err, "unable to register orphan sweeper")
		os.Exit(1)
	}

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up health check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up ready check")
		os.Exit(1)
	}

	setupLog.Info("starting manager")
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		setupLog.Error(err, "problem running manager")
		os.Exit(1)
	}
}
