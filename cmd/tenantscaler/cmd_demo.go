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

	"tenantscaler/internal/demo"
	"tenantscaler/internal/envfile"
	"tenantscaler/internal/gateway"
	"tenantscaler/internal/realrun"
)

// demoShutdownTimeout bounds the graceful shutdown: stopping a running
// stack (real replicas are killed) and closing the HTTP server.
const demoShutdownTimeout = 60 * time.Second

func init() {
	register("demo", "run the dashboard: sequential A/B of the KEDA baseline and TenantScaler on the same traffic", runDemo)
}

func runDemo(args []string) error {
	fs := flag.NewFlagSet("demo", flag.ContinueOnError)
	addr := fs.String("addr", envfile.Get("TS_DEMO_ADDR", "127.0.0.1:8080"), "dashboard listen address (default $TS_DEMO_ADDR)")
	cfgPath := fs.String("config", "", "YAML config (empty: "+demo.DefaultRealConfig+" for llama, the built-in default for the simulated engine)")
	backend := fs.String("backend", demo.ModeAuto, "replicas: auto (llama when $TS_LLAMA_SERVER and $TS_LLAMA_MODEL exist) | llama | simulated")
	scenario := fs.String("scenario", demo.ScenarioRealDemo, "traffic: real-demo (120 s) | tiny (6 s, for tests)")
	logDir := fs.String("log-dir", ".run/demo", "directory for llama-server logs and the controller's decision records")
	seed := fs.Int64("seed", 1, "seed of the replayed trace (both stacks get the same traffic)")
	live := fs.String("live", "", "start a live demo instead of waiting for Run A/B: the TenantScaler stack's gateway also listens "+
		"on this address (e.g. 0.0.0.0:8081) for teammates' laptops (tenantscaler loadgen -gateway); needs $TS_TENANT_KEYS")
	sideBySide := fs.Bool("side-by-side", false, "with -live: run the baseline stack too and send it a copy of every live request, so "+
		"both panels show the same live traffic; needs -backend simulated (two real stacks would share the P-cores and exceed the "+
		"8-thread budget)")
	observeGW := fs.String("observe-gateway", "", "watch a TenantScaler running in Kubernetes instead of starting one: its gateway's "+
		"base URL, e.g. http://127.0.0.1:8081 (scripts/k3s/expose.sh); needs -observe-controller and -observe-keys")
	observeCtl := fs.String("observe-controller", "", "with -observe-gateway: the controller's base URL, e.g. http://127.0.0.1:18090")
	observeBase := fs.String("observe-baseline-gateway", "", "with -observe-gateway: the side-by-side baseline gateway's base URL "+
		"(deploy/k3s/side-by-side), e.g. http://127.0.0.1:18082, shown in the baseline panel")
	observeReplicas := fs.String("observe-replicas", "simulated", "with -observe-gateway: what the cluster's replicas run, for the "+
		"dashboard's labels: simulated (deploy/k8s) | llama (deploy/k3s/llama)")
	observeKeys := fs.String("observe-keys", "", "with -observe-gateway: the env file holding the cluster's TS_ADMIN_KEY "+
		"(scripts/k3s/keys.ps1 writes it); the key is not printed")
	if err := fs.Parse(args); err != nil {
		return err
	}
	observing := *observeGW != "" || *observeCtl != ""
	switch {
	case observing && (*observeGW == "" || *observeCtl == "" || *observeKeys == ""):
		return errors.New("demo: -observe-gateway, -observe-controller and -observe-keys go together")
	case observing && *live != "":
		return errors.New("demo: -live and -observe-gateway exclude each other")
	case *sideBySide && *live == "":
		return errors.New("demo: -side-by-side needs -live")
	case *observeReplicas != "simulated" && *observeReplicas != "llama":
		return fmt.Errorf("demo: -observe-replicas must be simulated or llama, got %q", *observeReplicas)
	}
	keys, err := gateway.ParseTenantKeys(envfile.Get("TS_TENANT_KEYS", ""))
	if err != nil {
		return fmt.Errorf("$TS_TENANT_KEYS: %w", err)
	}
	mode := *backend
	if observing {
		mode = demo.ModeSimulated // nothing runs here; the cluster's replicas are the simulated engine
	}
	cfg, info, err := demo.Resolve(mode, *cfgPath)
	if err != nil {
		return err
	}
	if *sideBySide && !info.Simulated {
		return errors.New("demo: -side-by-side runs both stacks at once: with real replicas they would share the P-cores and " +
			"exceed the 8-thread budget; use -backend simulated")
	}
	var adminKey string
	if observing {
		info = demo.ObservedBackend(cfg, info.ConfigPath, *observeReplicas == "llama")
		if adminKey, err = envValue(*observeKeys, "TS_ADMIN_KEY"); err != nil {
			return err
		}
	}
	sc, err := demo.ScenarioByName(*scenario)
	if err != nil {
		return err
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	opt := demo.Options{Config: cfg, Backend: info, Scenario: sc, Seed: *seed, LogDir: *logDir, Logger: logger,
		LiveAddr: *live, LiveKeys: keys, LiveSideBySide: *sideBySide} // the dashboard's Go live button
	if !info.Simulated {
		opt.Cooldown = demo.RealCooldown
		opt.Guard = func(ctx context.Context) error { return realrun.DefaultGuard(ctx, logger) }
	}
	srv, err := demo.New(opt)
	if err != nil {
		return err
	}
	ln, err := net.Listen("tcp", *addr)
	if err != nil {
		return fmt.Errorf("demo: listen on %s: %w", *addr, err)
	}
	httpSrv := &http.Server{Handler: srv.Handler(), ReadHeaderTimeout: 5 * time.Second}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	serveErr := make(chan error, 1)
	go func() { serveErr <- httpSrv.Serve(ln) }()
	logger.Info("dashboard ready", "url", "http://"+ln.Addr().String(), "backend", info.Label,
		"reason", info.Reason, "config", info.ConfigPath, "scenario", sc.Name)
	fmt.Printf("dashboard on http://%s (%s; scenario %s)\n", ln.Addr(), info.Label, sc.Name)
	if *live != "" {
		if _, err := srv.StartLive(*live, keys, *sideBySide); err != nil {
			stop()
			_ = httpSrv.Close()
			return err
		}
		fmt.Printf("live demo (%s): the TenantScaler stack starts; teammates send traffic to %s with their API keys\n", demo.LiveLabel, *live)
	}
	if observing {
		target := demo.ObserveTarget{GatewayURL: *observeGW, ControllerURL: *observeCtl, BaselineGatewayURL: *observeBase, AdminKey: adminKey}
		if _, err := srv.StartObserve(target); err != nil {
			stop()
			_ = httpSrv.Close()
			return err
		}
		fmt.Printf("observing the TenantScaler in Kubernetes: gateway %s, controller %s\n", *observeGW, *observeCtl)
	}

	select {
	case <-ctx.Done():
		logger.Info("shutting down: stopping any running stack")
	case err = <-serveErr:
	}
	shutdown, cancel := context.WithTimeout(context.Background(), demoShutdownTimeout)
	defer cancel()
	closeErr := srv.Close(shutdown) // stops the stack first: open state polls must not hold the HTTP shutdown
	if serr := httpSrv.Shutdown(shutdown); serr != nil {
		closeErr = errors.Join(closeErr, serr, httpSrv.Close())
	}
	if errors.Is(err, http.ErrServerClosed) {
		err = nil
	}
	return errors.Join(err, closeErr)
}

// envValue returns key's value from an env file of KEY=VALUE lines without
// putting it into the process environment (the cluster's admin key must
// not replace this machine's).
func envValue(path, key string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	for line := range strings.Lines(strings.TrimPrefix(string(data), "\ufeff")) {
		if k, v, ok := strings.Cut(strings.TrimSpace(line), "="); ok && strings.TrimSpace(k) == key {
			return strings.TrimSpace(v), nil
		}
	}
	return "", fmt.Errorf("%s has no %s line", path, key)
}
