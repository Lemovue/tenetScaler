package main

import (
	"cmp"
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

	"tenantscaler/internal/config"
	"tenantscaler/internal/engine"
	"tenantscaler/internal/llama"
	"tenantscaler/internal/pool"
)

func init() {
	register("pool", "run the local pool manager (SIMULATED replicas, or real llama-server with -backend llama)", runPool)
}

func runPool(args []string) error {
	fs := flag.NewFlagSet("pool", flag.ContinueOnError)
	addr := fs.String("addr", "127.0.0.1:8100", "pool API listen address")
	cfgPath := fs.String("config", "", "YAML config (pool, engine and backend sections; empty = built-in default)")
	initial := fs.Int("replicas", -1, "initial replica count (-1 = pool.minReplicas from the config)")
	coldStart := fs.Duration("cold-start", 0, "override engine.startupDuration, the simulated model load time (simulated backend only)")
	backendKind := fs.String("backend", "", "replica backend: simulated or llama (empty = backend.kind from the config)")
	logDir := fs.String("log-dir", llama.DefaultLogDir, "llama backend: directory for the per-replica logs")
	pidFile := fs.String("pid-file", llama.DefaultPIDFile, "llama backend: launched PIDs are appended here (see 'tenantscaler cleanup')")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil // -h printed the flags; asking for help is not a failure
		}
		return err
	}
	cfg, params, err := replicaCmdParams(fs, *cfgPath, *coldStart)
	if err != nil {
		return err
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	backend, what, err := poolBackend(fs, cfg, cmp.Or(*backendKind, cfg.Backend.Kind), params, *logDir, *pidFile, logger)
	if err != nil {
		return err
	}
	n := *initial
	if n < 0 {
		n = cfg.Pool.MinReplicas
	}
	ln, err := net.Listen("tcp", *addr)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", *addr, err)
	}
	p := pool.New(pool.Config{
		Name: cfg.Pool.Name, Params: params, MaxReplicas: cfg.Pool.CapacityReplicas, Seed: 1,
		Backend: backend, Logger: logger,
	})
	if err := p.Scale(n); err != nil {
		return errors.Join(err, ln.Close(), p.Close())
	}
	srv := &http.Server{Handler: p.Handler(), ReadHeaderTimeout: 5 * time.Second}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	fmt.Printf("pool %q on http://%s: %d %s starting (max %d)\n",
		cfg.Pool.Name, ln.Addr(), p.Status().Desired, what, cfg.Pool.CapacityReplicas)
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()
	var serveErr error
	select {
	case err := <-errc:
		serveErr = fmt.Errorf("serve: %w", err)
	case <-ctx.Done():
		stop() // a second signal terminates immediately
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		serveErr = errors.Join(serveErr, fmt.Errorf("shutdown: %w", err))
	}
	return errors.Join(serveErr, p.Close())
}

// poolBackend builds the replica backend of the given kind and describes
// it for the start-up line. nil means the pool's default simulated backend.
func poolBackend(fs *flag.FlagSet, cfg config.Config, kind string, params engine.Params,
	logDir, pidFile string, logger *slog.Logger) (pool.Backend, string, error) {
	switch kind {
	case config.BackendSimulated:
		return nil, fmt.Sprintf("SIMULATED replicas (cold start %s)", params.StartupDuration), nil
	case config.BackendLlama:
	default:
		return nil, "", fmt.Errorf("-backend must be %q or %q, got %q", config.BackendSimulated, config.BackendLlama, kind)
	}
	coldStartSet := false
	fs.Visit(func(f *flag.Flag) { coldStartSet = coldStartSet || f.Name == "cold-start" })
	if coldStartSet {
		return nil, "", errors.New("-cold-start applies to the simulated backend only: real replicas report their measured cold start")
	}
	spec := cfg.Backend
	spec.Kind = config.BackendLlama
	if err := spec.Validate(cfg.Pool.CapacityReplicas); err != nil {
		return nil, "", fmt.Errorf("config: %w", err)
	}
	l := &llama.Launcher{Spec: spec.Llama, LogDir: logDir, PIDFile: pidFile, Logger: logger}
	if err := l.Check(); err != nil {
		return nil, "", err
	}
	return llama.NewBackend(l), fmt.Sprintf("REAL llama-server replicas (%d threads, %d slots each; logs in %s)",
		spec.Llama.Threads, spec.Llama.Parallel, logDir), nil
}
