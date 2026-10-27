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
	if err := fs.Parse(args); err != nil {
		return err
	}
	keys, err := gateway.ParseTenantKeys(envfile.Get("TS_TENANT_KEYS", ""))
	if err != nil {
		return fmt.Errorf("$TS_TENANT_KEYS: %w", err)
	}
	cfg, info, err := demo.Resolve(*backend, *cfgPath)
	if err != nil {
		return err
	}
	sc, err := demo.ScenarioByName(*scenario)
	if err != nil {
		return err
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	opt := demo.Options{Config: cfg, Backend: info, Scenario: sc, Seed: *seed, LogDir: *logDir, Logger: logger}
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
		if _, err := srv.StartLive(*live, keys); err != nil {
			stop()
			_ = httpSrv.Close()
			return err
		}
		fmt.Printf("live demo (%s): the TenantScaler stack starts; teammates send traffic to %s with their API keys\n", demo.LiveLabel, *live)
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
