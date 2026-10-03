// Command db-backup-operator runs the BackupPolicy controller.
package main

import (
	"flag"
	"os"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/kubernetes"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	backupv1 "github.com/EquinoxWN/db-backup-operator/api/v1alpha1"
	"github.com/EquinoxWN/db-backup-operator/internal/controller"
	"github.com/EquinoxWN/db-backup-operator/internal/podexec"
)

func main() {
	var metricsAddr, probeAddr string
	var leaderElect bool
	flag.StringVar(&metricsAddr, "metrics-bind-address", "0", "metrics address (\"0\" disables metrics)")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "health and readiness probe address")
	flag.BoolVar(&leaderElect, "leader-elect", false, "run only one active replica via leader election")
	opts := zap.Options{}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()
	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))
	log := ctrl.Log.WithName("setup")

	scheme := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(backupv1.AddToScheme(scheme))

	cfg := ctrl.GetConfigOrDie()
	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsserver.Options{BindAddress: metricsAddr},
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         leaderElect,
		LeaderElectionID:       "db-backup-operator.dbbackup.equinoxwn.github.io",
	})
	if err != nil {
		log.Error(err, "creating manager")
		os.Exit(1)
	}
	clientset, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		log.Error(err, "creating clientset")
		os.Exit(1)
	}
	r := &controller.BackupPolicyReconciler{
		Client: mgr.GetClient(),
		Exec:   podexec.Executor{Config: cfg, Clientset: clientset},
		Now:    time.Now,
	}
	if err := r.SetupWithManager(mgr); err != nil {
		log.Error(err, "setting up controller")
		os.Exit(1)
	}
	utilruntime.Must(mgr.AddHealthzCheck("healthz", healthz.Ping))
	utilruntime.Must(mgr.AddReadyzCheck("readyz", healthz.Ping))
	log.Info("starting manager")
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		log.Error(err, "running manager")
		os.Exit(1)
	}
}
