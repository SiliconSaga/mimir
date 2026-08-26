// Command manager runs the Mimir DataService operator.
//
// It vends a database inside an existing server: an app declares one
// DataService in its own namespace and receives a database, an owning role and
// a Secret. See docs/plans/2026-08-17-dataservice-vending-design.md.
package main

import (
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	mimirv1alpha1 "github.com/SiliconSaga/mimir/operator/api/v1alpha1"
	"github.com/SiliconSaga/mimir/operator/internal/controller"
	"github.com/SiliconSaga/mimir/operator/internal/engine"
)

var (
	scheme   = runtime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(mimirv1alpha1.AddToScheme(scheme))
}

func main() {
	var metricsAddr, probeAddr string
	var enableLeaderElection bool

	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8080", "Address the metric endpoint binds to.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "Address the probe endpoint binds to.")
	flag.BoolVar(&enableLeaderElection, "leader-elect", false, "Enable leader election for controller manager.")
	opts := zap.Options{Development: false}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

	shared, err := sharedClustersFromEnv()
	if err != nil {
		setupLog.Error(err, "invalid shared cluster configuration")
		os.Exit(1)
	}
	if len(shared) == 0 {
		// Not fatal: the operator still starts and reports a clear condition on
		// every shared request. Failing to start would make a config typo look
		// like a crashloop instead of a misconfiguration.
		setupLog.Info("no shared clusters configured; shared placement will report ClusterNotFound")
	}

	registry := engine.NewRegistry(engine.Postgres{})
	setupLog.Info("engines registered", "engines", registry.Engines())

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsserver.Options{BindAddress: metricsAddr},
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         enableLeaderElection,
		LeaderElectionID:       "dataservice.mimir.siliconsaga.org",
		Client: client.Options{
			Cache: &client.CacheOptions{
				// Read Secrets straight from the API server instead of through
				// the manager's cache.
				//
				// The cache backs every read with an informer, which LISTs and
				// WATCHes the whole type cluster-wide. For Secrets that means
				// holding every credential in the cluster in memory and needing
				// RBAC to enumerate them — for the sake of two objects: one
				// admin Secret read by name, and one published Secret per
				// tenant. Uncached reads let the ClusterRole drop list/watch.
				DisableFor: []client.Object{&corev1.Secret{}},
			},
		},
	})
	if err != nil {
		setupLog.Error(err, "unable to start manager")
		os.Exit(1)
	}

	if err := (&controller.DataServiceReconciler{
		Client:         mgr.GetClient(),
		Scheme:         mgr.GetScheme(),
		Registry:       registry,
		SharedClusters: shared,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "DataService")
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

// sharedClustersFromEnv reads per-engine shared cluster config from the
// environment, so the operator carries no opinion about how those clusters are
// deployed and a new engine needs no code change to be pointed somewhere.
//
// For engine "postgres" the variables are:
//
//	MIMIR_POSTGRES_HOST                  required — what consumers connect to
//	MIMIR_POSTGRES_PORT                  default 5432
//	MIMIR_POSTGRES_ADMIN_HOST            default = HOST — where DDL runs
//	MIMIR_POSTGRES_ADMIN_PORT            default = PORT
//	MIMIR_POSTGRES_ADMIN_SECRET          required, "namespace/name"
//	MIMIR_POSTGRES_ADMIN_USER_KEY        default "user"
//	MIMIR_POSTGRES_ADMIN_PASSWORD_KEY    default "password"
//	MIMIR_POSTGRES_ADMIN_DATABASE        default "postgres"
//	MIMIR_POSTGRES_TLS                   default "true"
func sharedClustersFromEnv() (map[mimirv1alpha1.Engine]controller.SharedCluster, error) {
	out := map[mimirv1alpha1.Engine]controller.SharedCluster{}

	for _, e := range []mimirv1alpha1.Engine{
		mimirv1alpha1.EnginePostgres,
		mimirv1alpha1.EngineMySQL,
		mimirv1alpha1.EngineMongoDB,
	} {
		prefix := "MIMIR_" + strings.ToUpper(string(e)) + "_"

		host := os.Getenv(prefix + "HOST")
		if host == "" {
			continue // engine not configured; that is fine
		}

		adminSecret := os.Getenv(prefix + "ADMIN_SECRET")
		ns, name, ok := strings.Cut(adminSecret, "/")
		if !ok || ns == "" || name == "" {
			return nil, fmt.Errorf("%sADMIN_SECRET must be namespace/name, got %q", prefix, adminSecret)
		}

		port := int32(defaultPort(e))
		if raw := os.Getenv(prefix + "PORT"); raw != "" {
			p, err := parsePort(raw)
			if err != nil {
				return nil, fmt.Errorf("%sPORT: %w", prefix, err)
			}
			port = p
		}

		tls := true
		if raw := os.Getenv(prefix + "TLS"); raw != "" {
			t, err := strconv.ParseBool(raw)
			if err != nil {
				return nil, fmt.Errorf("%sTLS: %w", prefix, err)
			}
			tls = t
		}

		// Admin traffic defaults to the consumer host when not set separately,
		// which is correct for engines with no pooler in front.
		adminHost := envOr(prefix+"ADMIN_HOST", host)
		adminPort := port
		if raw := os.Getenv(prefix + "ADMIN_PORT"); raw != "" {
			p, err := parsePort(raw)
			if err != nil {
				return nil, fmt.Errorf("%sADMIN_PORT: %w", prefix, err)
			}
			adminPort = p
		}

		out[e] = controller.SharedCluster{
			Host:                 host,
			Port:                 port,
			AdminHost:            adminHost,
			AdminPort:            adminPort,
			AdminSecretNamespace: ns,
			AdminSecretName:      name,
			AdminUserKey:         envOr(prefix+"ADMIN_USER_KEY", "user"),
			AdminPasswordKey:     envOr(prefix+"ADMIN_PASSWORD_KEY", "password"),
			AdminDatabase:        envOr(prefix+"ADMIN_DATABASE", defaultAdminDatabase(e)),
			TLS:                  tls,
		}
	}
	return out, nil
}

// parsePort accepts only a real TCP port. Without the range check a typo like
// "5432 " or "54320000" becomes a silently wrong int32 and the failure surfaces
// much later as an unexplained connection error.
func parsePort(raw string) (int32, error) {
	p, err := strconv.ParseInt(raw, 10, 32)
	if err != nil {
		return 0, err
	}
	if p < 1 || p > 65535 {
		return 0, fmt.Errorf("port %d out of range 1-65535", p)
	}
	return int32(p), nil
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func defaultPort(e mimirv1alpha1.Engine) int {
	switch e {
	case mimirv1alpha1.EngineMySQL:
		return 3306
	case mimirv1alpha1.EngineMongoDB:
		return 27017
	default:
		return 5432
	}
}

func defaultAdminDatabase(e mimirv1alpha1.Engine) string {
	switch e {
	case mimirv1alpha1.EngineMySQL:
		return "mysql"
	case mimirv1alpha1.EngineMongoDB:
		return "admin"
	default:
		return "postgres"
	}
}
