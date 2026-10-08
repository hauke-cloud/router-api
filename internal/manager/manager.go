// Package manager is what the three managers of router-api have in common:
// flags, logging, CRD installation and the controller-runtime manager.
package manager

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"

	"github.com/go-logr/logr"
	"github.com/prometheus/client_golang/prometheus"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	"github.com/hauke-cloud/router-api/internal/crd"
	"github.com/hauke-cloud/router-api/internal/version"
)

// Definition describes one manager.
type Definition struct {
	// Name of the binary, also the leader election ID and the field owner.
	Name string
	// Groups are the API groups whose CRDs this manager installs. A provider
	// lists the core group next to its own: it reads core objects, and all
	// managers of one release embed the same definitions.
	Groups []string
	// AddToScheme registers the API types the manager uses.
	AddToScheme []func(*runtime.Scheme) error
	// Setup registers the controllers.
	Setup func(mgr ctrl.Manager) error
}

// Config is what the flags of a manager say.
type Config struct {
	MetricsAddress          string
	ProbeAddress            string
	LeaderElection          bool
	LeaderElectionNamespace string
	Namespace               string
	InstallCRDs             bool
	LogLevel                string
	LogFormat               string
	ShowVersion             bool
}

// ParseFlags reads a manager's command line.
func ParseFlags(name string, args []string, output io.Writer) (*Config, error) {
	cfg := &Config{}
	flags := flag.NewFlagSet(name, flag.ContinueOnError)
	flags.SetOutput(output)
	flags.StringVar(&cfg.MetricsAddress, "metrics-bind-address", ":8080", "address the metrics endpoint binds to, 0 to disable")
	flags.StringVar(&cfg.ProbeAddress, "health-probe-bind-address", ":8081", "address the health probes bind to")
	flags.BoolVar(&cfg.LeaderElection, "leader-elect", false, "elect a leader so that only one replica acts")
	flags.StringVar(&cfg.LeaderElectionNamespace, "leader-election-namespace", "", "namespace of the leader election lease, defaults to the pod's")
	flags.StringVar(&cfg.Namespace, "namespace", "", "only act on objects in this namespace, empty for all")
	flags.BoolVar(&cfg.InstallCRDs, "install-crds", true, "apply the CustomResourceDefinitions embedded in this binary at start-up")
	flags.StringVar(&cfg.LogLevel, "log-level", "info", "debug, info, warn or error")
	flags.StringVar(&cfg.LogFormat, "log-format", "json", "json or text")
	flags.BoolVar(&cfg.ShowVersion, "version", false, "print the version and exit")
	if err := flags.Parse(args); err != nil {
		return nil, err
	}
	if flags.NArg() > 0 {
		return nil, fmt.Errorf("unexpected argument %q", flags.Arg(0))
	}
	var level slog.Level
	if err := level.UnmarshalText([]byte(cfg.LogLevel)); err != nil {
		return nil, fmt.Errorf("-log-level %q: %w", cfg.LogLevel, err)
	}
	if cfg.LogFormat != "json" && cfg.LogFormat != "text" {
		return nil, fmt.Errorf("-log-format %q: has to be json or text", cfg.LogFormat)
	}
	return cfg, nil
}

func newLogger(cfg *Config, output io.Writer) *slog.Logger {
	var level slog.Level
	_ = level.UnmarshalText([]byte(cfg.LogLevel))
	options := &slog.HandlerOptions{Level: level}
	if strings.EqualFold(cfg.LogFormat, "text") {
		return slog.New(slog.NewTextHandler(output, options))
	}
	return slog.New(slog.NewJSONHandler(output, options))
}

// Scheme returns a scheme with the built-in types and those the definition
// adds.
func Scheme(def *Definition) (*runtime.Scheme, error) {
	scheme := runtime.NewScheme()
	adders := append([]func(*runtime.Scheme) error{clientgoscheme.AddToScheme, apiextensionsv1.AddToScheme}, def.AddToScheme...)
	for _, add := range adders {
		if err := add(scheme); err != nil {
			return nil, fmt.Errorf("register types: %w", err)
		}
	}
	return scheme, nil
}

// Main runs a manager and exits the process with its result.
func Main(def *Definition) {
	if err := Run(ctrl.SetupSignalHandler(), def, os.Args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return
		}
		fmt.Fprintln(os.Stderr, def.Name+":", err)
		os.Exit(1)
	}
}

// Run runs a manager until ctx is cancelled.
func Run(ctx context.Context, def *Definition, args []string) error {
	cfg, err := ParseFlags(def.Name, args, os.Stderr)
	if err != nil {
		return err
	}
	if cfg.ShowVersion {
		fmt.Println(version.Version())
		return nil
	}

	logger := newLogger(cfg, os.Stdout)
	slog.SetDefault(logger)
	ctrl.SetLogger(logr.FromSlogHandler(logger.Handler()))

	restConfig, err := ctrl.GetConfig()
	if err != nil {
		return fmt.Errorf("load kubernetes configuration: %w", err)
	}
	return Start(ctx, def, cfg, restConfig, logger)
}

// Start runs a manager against the given cluster until ctx is cancelled.
func Start(ctx context.Context, def *Definition, cfg *Config, restConfig *rest.Config, logger *slog.Logger) error {
	logger.Info("starting "+def.Name,
		"version", version.Version(), "commit", version.Commit(),
		"namespace", cfg.Namespace, "leader_election", cfg.LeaderElection, "install_crds", cfg.InstallCRDs)

	scheme, err := Scheme(def)
	if err != nil {
		return err
	}

	if cfg.InstallCRDs {
		// Before the manager: an informer for a type the API server does
		// not serve yet fails instead of waiting.
		bootstrap, err := client.New(restConfig, client.Options{Scheme: scheme})
		if err != nil {
			return fmt.Errorf("build bootstrap client: %w", err)
		}
		for _, group := range def.Groups {
			if err := crd.Install(ctx, bootstrap, group, logger); err != nil {
				return err
			}
		}
	}

	options := ctrl.Options{
		Scheme:                        scheme,
		Metrics:                       metricsserver.Options{BindAddress: cfg.MetricsAddress},
		HealthProbeBindAddress:        cfg.ProbeAddress,
		LeaderElection:                cfg.LeaderElection,
		LeaderElectionID:              def.Name + ".router.hauke.cloud",
		LeaderElectionNamespace:       cfg.LeaderElectionNamespace,
		LeaderElectionReleaseOnCancel: true,
		Client: client.Options{
			Cache: &client.CacheOptions{
				// Read on demand. Caching them would mean holding every
				// Secret and ConfigMap the manager is allowed to see.
				DisableFor: []client.Object{&corev1.Secret{}, &corev1.ConfigMap{}},
			},
		},
	}
	if cfg.Namespace != "" {
		options.Cache = cache.Options{DefaultNamespaces: map[string]cache.Config{cfg.Namespace: {}}}
	}
	mgr, err := ctrl.NewManager(restConfig, options)
	if err != nil {
		return fmt.Errorf("build manager: %w", err)
	}

	// The registry is the process's. More than one manager in a process,
	// as in the system test, share the metric.
	var registered prometheus.AlreadyRegisteredError
	if err := ctrlmetrics.Registry.Register(version.Collector()); err != nil && !errors.As(err, &registered) {
		return fmt.Errorf("register build info metric: %w", err)
	}
	if cfg.ProbeAddress != "0" {
		if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
			return fmt.Errorf("register health check: %w", err)
		}
		if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
			return fmt.Errorf("register readiness check: %w", err)
		}
	}
	if err := def.Setup(mgr); err != nil {
		return fmt.Errorf("register controllers: %w", err)
	}

	if err := mgr.Start(ctx); err != nil {
		return fmt.Errorf("run manager: %w", err)
	}
	return nil
}
