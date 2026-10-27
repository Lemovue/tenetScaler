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

	"tenantscaler/internal/autoscale"
	"tenantscaler/internal/config"
	"tenantscaler/internal/pool"
	"tenantscaler/internal/scalerun"
)

func init() {
	register("baseline", "run a baseline autoscaler (KEDA queue length or CPU-style HPA) on a pool", runBaseline)
}

func runBaseline(args []string) error {
	fs := flag.NewFlagSet("baseline", flag.ContinueOnError)
	algo := fs.String("algo", scalerun.AlgoKEDA, "autoscaler: keda (queue length) | hpa (slot occupancy as the CPU proxy)")
	poolURL := fs.String("pool-url", "http://127.0.0.1:8100", "pool manager API base URL")
	gatewayURL := fs.String("gateway-url", "", "gateway base URL; KEDA adds the queued count of its GET /stats (empty = replica queues only)")
	cfgPath := fs.String("config", "", "YAML config: pool min/capacity and slots per replica (empty = built-in default)")
	cycle := fs.Duration("cycle", 15*time.Second, "sync period (the Kubernetes HPA default is 15s)")
	sample := fs.Duration("sample", scalerun.DefaultSampleEvery, "hpa: slot-occupancy sampling period; each sync averages the samples since the last one")
	window := fs.Duration("scale-down-window", autoscale.DefaultScaleDownWindow, "scale-down stabilisation window")
	addr := fs.String("addr", "127.0.0.1:8300", "status API listen address (GET /status, /healthz, /metrics)")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil // -h printed the flags; asking for help is not a failure
		}
		return err
	}
	if *cycle <= 0 {
		return fmt.Errorf("-cycle must be > 0, got %v", *cycle)
	}
	if *sample <= 0 {
		return fmt.Errorf("-sample must be > 0, got %v", *sample)
	}
	cfg, err := loadConfig(*cfgPath)
	if err != nil {
		return err
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	r, err := baselineRunner(cfg, *algo, *window, logger)
	if err != nil {
		return err
	}
	r.Pool = pool.Client{BaseURL: *poolURL}
	r.Cycle, r.SampleEvery = *cycle, *sample
	if *gatewayURL != "" {
		if *algo != scalerun.AlgoKEDA {
			return errors.New("-gateway-url only applies to -algo keda (the HPA scales on slot occupancy)")
		}
		r.GatewayStatsURL = strings.TrimRight(*gatewayURL, "/") + "/stats"
	}
	if err := r.Validate(); err != nil {
		return err
	}

	ln, err := net.Listen("tcp", *addr)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", *addr, err)
	}
	srv := &http.Server{Handler: r.Handler(), ReadHeaderTimeout: 5 * time.Second}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	fmt.Printf("baseline %s autoscaler on http://%s: pool %s, replicas [%d, %d], cycle %s, scale-down window %s\n",
		*algo, ln.Addr(), *poolURL, cfg.Pool.MinReplicas, cfg.Pool.CapacityReplicas, *cycle, *window)

	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve(ln) }()
	runDone := make(chan error, 1)
	go func() { runDone <- r.Run(ctx) }()

	var errs []error
	runReturned := false
	select {
	case err := <-serveErr:
		errs = append(errs, fmt.Errorf("serve: %w", err))
		stop()
	case err := <-runDone: // only an invalid configuration ends Run early
		errs = append(errs, err)
		runReturned = true
		stop()
	case <-ctx.Done():
		stop() // a second signal terminates immediately
	}
	if !runReturned {
		errs = append(errs, <-runDone)
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		errs = append(errs, fmt.Errorf("shutdown: %w", err))
	}
	return errors.Join(errs...)
}

// baselineRunner builds the autoscaler from the config: the documented
// defaults (KEDA 4 queued per replica, HPA target 0.7, tolerance 0.1)
// between pool.minReplicas and pool.capacityReplicas.
func baselineRunner(cfg config.Config, algo string, window time.Duration, logger *slog.Logger) (*scalerun.Runner, error) {
	minR, maxR := cfg.Pool.MinReplicas, cfg.Pool.CapacityReplicas
	r := &scalerun.Runner{Algo: algo, MaxNumSeqs: baselineSlots(cfg), Logger: logger}
	switch algo {
	case scalerun.AlgoKEDA:
		kc := autoscale.DefaultKEDAConfig(minR, maxR)
		kc.ScaleDownWindow = window
		if err := kc.Validate(); err != nil {
			return nil, err
		}
		r.KEDA = autoscale.NewKEDA(kc, minR)
	case scalerun.AlgoHPA:
		hc := autoscale.DefaultHPAConfig(minR, maxR)
		hc.ScaleDownWindow = window
		if err := hc.Validate(); err != nil {
			return nil, err
		}
		r.HPA = autoscale.NewHPA(hc, minR)
	default:
		return nil, fmt.Errorf("-algo must be %s or %s, got %q", scalerun.AlgoKEDA, scalerun.AlgoHPA, algo)
	}
	return r, nil
}

// baselineSlots is the number of sequences one replica runs at once: the
// llama.cpp parallel slots or the simulated engine's maxNumSeqs.
func baselineSlots(cfg config.Config) int {
	if cfg.Backend.Kind == config.BackendLlama {
		return cfg.Backend.Llama.Parallel
	}
	return cfg.Engine.MaxNumSeqs
}
