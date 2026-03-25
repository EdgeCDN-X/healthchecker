package main

import (
	"flag"
	"log"
	"os"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	infrastructurev1alpha1 "github.com/EdgeCDN-X/edgecdnx-controller/api/v1alpha1"
	"github.com/EdgeCDN-X/healthchecker.git/internal/controller"
	"github.com/EdgeCDN-X/healthchecker.git/internal/healthchecker"
)

var (
	scheme   = runtime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")
)

func main() {
	var probeAddr string
	var enableLeaderElection bool
	var production bool
	var prometheusEndpoint string
	var prometheusClientCert string
	var prometheusClientKey string
	var prometheusCA string
	var prometheusScrapeInterval time.Duration

	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "The address the probe endpoint binds to.")
	flag.BoolVar(&enableLeaderElection, "leader-elect", false, "Enable leader election for controller manager.")
	flag.BoolVar(&production, "production", false, "Enable production logging.")
	flag.StringVar(&prometheusEndpoint, "prometheus-endpoint", "", "Prometheus base URL used for alert scraping (optional).")
	flag.StringVar(&prometheusClientCert, "prometheus-client-cert", "", "Path to mTLS client certificate for Prometheus (optional).")
	flag.StringVar(&prometheusClientKey, "prometheus-client-key", "", "Path to mTLS client private key for Prometheus (optional).")
	flag.StringVar(&prometheusCA, "prometheus-ca", "", "Path to custom CA certificate for Prometheus TLS verification (optional).")
	flag.DurationVar(&prometheusScrapeInterval, "prometheus-scrape-interval", 30*time.Second, "Scrape interval for Prometheus alerts.")
	flag.Parse()

	opts := zap.Options{
		Development: !production,
	}
	opts.BindFlags(flag.CommandLine)
	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(infrastructurev1alpha1.AddToScheme(scheme))

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                 scheme,
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         enableLeaderElection,
		LeaderElectionID:       "healthchecker.edgecdnx.com",
	})
	if err != nil {
		log.Fatalf("unable to start manager: %v", err)
	}

	if err = (&controller.LocationHealthcheckController{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
		Prometheus: healthchecker.PrometheusConfig{
			Endpoint:       prometheusEndpoint,
			ClientCertFile: prometheusClientCert,
			ClientKeyFile:  prometheusClientKey,
			CAFile:         prometheusCA,
			Interval:       prometheusScrapeInterval,
		},
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller")
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
