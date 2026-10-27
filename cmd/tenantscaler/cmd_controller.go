package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"tenantscaler/internal/config"
	"tenantscaler/internal/control"
	"tenantscaler/internal/controller"
	"tenantscaler/internal/envfile"
	"tenantscaler/internal/k8s"
	"tenantscaler/internal/pool"
)

func init() {
	register("controller", "run the controller: arbitrate replicas, scale the pool, push gateway policy", runController)
}

// controllerFlags are the controller subcommand's flags.
type controllerFlags struct {
	addr, cfgPath, poolURL, gateways, metrics, promURL, decisionsFile string
	window                                                            time.Duration
	k8s                                                               bool
	kubeconfig, namespace, deployment                                 string
	overrides                                                         controller.Overrides
}

func parseControllerFlags(args []string) (*controllerFlags, error) {
	fs := flag.NewFlagSet("controller", flag.ContinueOnError)
	f := &controllerFlags{}
	o := &f.overrides
	fs.StringVar(&f.addr, "addr", "127.0.0.1:8090", "API listen address (/decisions, /status, /healthz, /metrics)")
	fs.StringVar(&f.cfgPath, "config", "", "YAML config (empty = built-in default)")
	fs.StringVar(&f.poolURL, "pool-url", "http://127.0.0.1:8100", "local mode: pool manager API to scale")
	fs.StringVar(&f.gateways, "gateway", "http://127.0.0.1:8080", "comma-separated gateway URLs: /stats source and /policy target")
	fs.StringVar(&f.metrics, "metrics", controller.SourceGateway, "metrics source: gateway (/stats) or prometheus")
	fs.StringVar(&f.promURL, "prometheus-url", "http://127.0.0.1:9090", "Prometheus URL for -metrics prometheus")
	fs.DurationVar(&o.Cycle, "cycle", 0, "control period (0 = controller.cycleSeconds from the config)")
	fs.DurationVar(&f.window, "window", 0, "metrics window (0 = the cycle for gateway, max(cycle, 1m) for prometheus: rate() needs several scrapes)")
	fs.DurationVar(&o.ColdStart, "cold-start", 0, "replica cold start that sets the forecast horizon (0 = engine.startupDuration from the config)")
	fs.BoolVar(&o.DisableBorrowing, "disable-borrowing", false, "ablation: tenants cannot borrow idle guaranteed replicas")
	fs.BoolVar(&o.DisableCoupling, "disable-coupling", false, "ablation: do not push policy to the gateways")
	fs.BoolVar(&o.Forecast, "forecast", false, "enable the Holt-Winters demand forecaster")
	fs.StringVar(&f.decisionsFile, "decisions-file", ".run/controller-decisions.jsonl", `append decision records here as JSON Lines ("" = memory only)`)
	fs.BoolVar(&f.k8s, "k8s", false, "Kubernetes mode: tenants from TenantScaler resources, scale a Deployment")
	fs.StringVar(&f.kubeconfig, "kubeconfig", envfile.Get("TS_KUBECONFIG", ""), "Kubernetes mode: kubeconfig path (default $TS_KUBECONFIG; empty = in-cluster)")
	fs.StringVar(&f.namespace, "namespace", envfile.Get("TS_NAMESPACE", ""), "Kubernetes mode: namespace (default $TS_NAMESPACE; empty = the pod's own)")
	fs.StringVar(&f.deployment, "deployment", "model-pool", "Kubernetes mode: model pool Deployment to scale")
	fs.StringVar(&o.Pool, "pool", "", "pool name; Kubernetes mode reads the TenantScalers of this pool (empty = pool.name from the config)")
	fs.IntVar(&o.Capacity, "capacity", 0, "pool replica budget (0 = pool.capacityReplicas from the config)")
	if err := fs.Parse(args); err != nil {
		return nil, err
	}
	return f, nil
}

// ctlParts are the pieces built from the flags before the controller
// listens, so a bad flag fails fast.
type ctlParts struct {
	cfg       config.Config
	coldStart time.Duration
	source    controller.MetricsSource
	publisher *controller.HTTPPublisher // nil when coupling is off
}

func newCtlParts(f *controllerFlags) (ctlParts, error) {
	cfg, err := loadConfig(f.cfgPath)
	if err != nil {
		return ctlParts{}, err
	}
	coldStart, err := f.overrides.Apply(&cfg)
	if err != nil {
		return ctlParts{}, err
	}
	gateways := ctlSplitList(f.gateways)
	var gwClient *http.Client // nil: the default client
	if key := envfile.Get("TS_ADMIN_KEY", ""); key != "" {
		gwClient = controller.BearerClient(key)
	}
	src, err := controller.NewSource(controller.SourceOptions{
		Kind: f.metrics, Gateways: gateways, PrometheusURL: f.promURL, Window: f.window, Cycle: ctlCycle(cfg),
		HTTP: gwClient,
	})
	if err != nil {
		return ctlParts{}, fmt.Errorf("-metrics %s: %w", f.metrics, err)
	}
	pub, err := controller.NewPublisher(cfg, gateways)
	if err != nil {
		return ctlParts{}, fmt.Errorf("%w (pass -gateway, or -disable-coupling)", err)
	}
	if pub != nil {
		pub.HTTP = gwClient
	}
	return ctlParts{cfg: cfg, coldStart: coldStart, source: src, publisher: pub}, nil
}

func runController(args []string) error {
	f, err := parseControllerFlags(args)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil // -h printed the flags; asking for help is not a failure
		}
		return err
	}
	parts, err := newCtlParts(f)
	if err != nil {
		return err
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))

	ln, err := net.Listen("tcp", f.addr)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", f.addr, err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	front := &controller.StartupHandler{}
	srv := &http.Server{Handler: front, ReadHeaderTimeout: 5 * time.Second}
	serveErr := make(chan error, 1)
	go func() {
		if err := srv.Serve(ln); !errors.Is(err, http.ErrServerClosed) {
			serveErr <- fmt.Errorf("serve: %w", err)
			stop()
		}
		close(serveErr)
	}()
	cfg := parts.cfg
	fmt.Printf("controller on http://%s: %s mode, metrics from %s, cycle %s, pool %q of %d replicas\n",
		ln.Addr(), ctlModeName(f.k8s), f.metrics, ctlCycle(cfg), cfg.Pool.Name, cfg.Pool.CapacityReplicas)

	runner, decisions, runErr := buildControllerRunner(ctx, parts, f, logger)
	if runErr == nil {
		front.Set(runner.Handler())
		runErr = runner.Run(ctx)
	}
	if errors.Is(runErr, context.Canceled) {
		runErr = nil // stopped by a signal while waiting for tenants
	}
	stop() // a second signal terminates immediately
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	errs := []error{runErr, srv.Shutdown(shutdownCtx), <-serveErr}
	if decisions != nil {
		errs = append(errs, decisions.Close())
	}
	return errors.Join(errs...)
}

// buildControllerRunner wires the Runner for local or Kubernetes mode. In
// Kubernetes mode it first waits (retrying every cycle) for the pool's
// TenantScalers. Before the first cycle it continues the policy versions the
// gateways (and TenantScaler statuses) already hold, so a restarted
// controller's policies are applied. The returned decision log must be
// closed by the caller.
func buildControllerRunner(ctx context.Context, parts ctlParts, f *controllerFlags, logger *slog.Logger) (*controller.Runner, *controller.DecisionLog, error) {
	cfg := parts.cfg
	cycle := ctlCycle(cfg)
	r := &controller.Runner{Source: parts.source, Cycle: cycle, Logger: logger}
	var versions []controller.VersionSource
	if parts.publisher != nil {
		r.Publisher = parts.publisher
		versions = append(versions, parts.publisher)
	}

	if f.k8s {
		client, err := k8s.NewClient(f.kubeconfig, f.namespace)
		if err != nil {
			return nil, nil, err
		}
		kt := &controller.KubeTenants{Client: client, Pool: cfg.Pool.Name, Capacity: cfg.Pool.CapacityReplicas, Logger: logger}
		specs, err := kt.Wait(ctx, cycle)
		if err != nil {
			return nil, nil, err
		}
		cfg.Tenants = specs
		r.Actuator = k8s.DeploymentActuator{C: client, Deployment: f.deployment}
		r.BeforeCycle, r.AfterDecision = kt.BeforeCycle, kt.AfterDecision
		if r.Core, err = control.NewCore(cfg, parts.coldStart); err != nil {
			return nil, nil, err
		}
		kt.Core = r.Core
		versions = append(versions, kt)
	} else {
		r.Actuator = controller.PoolActuator{Client: &pool.Client{BaseURL: f.poolURL}}
		var err error
		if r.Core, err = control.NewCore(cfg, parts.coldStart); err != nil {
			return nil, nil, err
		}
	}
	if err := r.SeedPolicyVersion(ctx, versions...); err != nil {
		logger.Warn("could not read every policy version in use; a gateway that ignores a policy as stale raises it later", "err", err)
	}
	decisions, err := controller.NewDecisionLog(controller.DefaultLogCapacity, f.decisionsFile)
	if err != nil {
		return nil, nil, err
	}
	r.Log = decisions
	return r, decisions, nil
}

func ctlCycle(cfg config.Config) time.Duration {
	return time.Duration(cfg.Controller.CycleSeconds * float64(time.Second))
}

func ctlModeName(k8sMode bool) string {
	if k8sMode {
		return "kubernetes"
	}
	return "local"
}

// ctlSplitList splits a comma-separated list, dropping empty items.
func ctlSplitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
