package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"hash/fnv"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"tenantscaler/internal/config"
	"tenantscaler/internal/engine"
	"tenantscaler/internal/replica"
)

// replicaDrainTimeout bounds how long a stopping replica waits for its
// in-flight requests before shutting down anyway.
const replicaDrainTimeout = 30 * time.Second

func init() {
	register("replica", "run one SIMULATED engine replica (vLLM-like OpenAI API)", runReplica)
}

func runReplica(args []string) error {
	fs := flag.NewFlagSet("replica", flag.ContinueOnError)
	addr := fs.String("addr", "127.0.0.1:8000", "listen address")
	cfgPath := fs.String("config", "", "YAML config whose engine section is used (empty = built-in default)")
	id := fs.String("id", "", "replica ID (empty = host name, i.e. the pod name in Kubernetes)")
	coldStart := fs.Duration("cold-start", 0, "override engine.startupDuration, the simulated model load time")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil // -h printed the flags; asking for help is not a failure
		}
		return err
	}
	_, params, err := replicaCmdParams(fs, *cfgPath, *coldStart)
	if err != nil {
		return err
	}
	if *id == "" {
		if *id, err = os.Hostname(); err != nil {
			*id = "replica"
		}
	}
	ln, err := net.Listen("tcp", *addr)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", *addr, err)
	}
	rep := replica.New(*id, params, replicaSeed(*id))
	srv := &http.Server{Handler: rep.Handler(), ReadHeaderTimeout: 5 * time.Second}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	rep.Start(time.Now())
	fmt.Printf("SIMULATED replica %s on http://%s (cold start %s, maxNumSeqs %d)\n",
		*id, ln.Addr(), params.StartupDuration, params.MaxNumSeqs)
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()
	select {
	case err := <-errc:
		return fmt.Errorf("serve: %w", err)
	case <-ctx.Done():
		stop() // a second signal terminates immediately
	}

	fmt.Printf("replica %s draining (up to %s)\n", *id, replicaDrainTimeout)
	rep.BeginDrain()
	drainCtx, cancelDrain := context.WithTimeout(context.Background(), replicaDrainTimeout)
	defer cancelDrain()
	drainErr := rep.WaitDrained(drainCtx)
	shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelShutdown()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return errors.Join(drainErr, fmt.Errorf("shutdown: %w", err), srv.Close())
	}
	return drainErr
}

// replicaCmdParams loads the configuration (default when cfgPath is empty)
// and its engine parameters, applying the -cold-start override when that
// flag was given on the command line.
func replicaCmdParams(fs *flag.FlagSet, cfgPath string, coldStart time.Duration) (config.Config, engine.Params, error) {
	cfg, err := loadConfig(cfgPath)
	if err != nil {
		return config.Config{}, engine.Params{}, err
	}
	params, err := cfg.Engine.Params()
	if err != nil {
		return config.Config{}, engine.Params{}, err
	}
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "cold-start" {
			params.StartupDuration = coldStart
		}
	})
	if err := params.Validate(); err != nil {
		return config.Config{}, engine.Params{}, fmt.Errorf("engine parameters: %w", err)
	}
	return cfg, params, nil
}

// replicaSeed derives a stable jitter seed from a replica ID, so replicas
// with different IDs do not jitter in lockstep.
func replicaSeed(id string) int64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(id))
	return int64(h.Sum64() >> 1)
}
