// Package realrun is the evaluation on REAL replicas (requirements R26-R28):
// it replays a workload scenario against live stacks (internal/stack) of
// real llama.cpp llama-server replicas, one setup at a time, records every
// request and a per-second timeline, computes the evaluation metrics
// (internal/eval) and writes the results with mean ± 95% CI over runs. The
// same code runs on the simulated backend, for tests; its results are then
// labelled SIMULATED.
//
// It also holds the real replica profile (Profile): the per-replica
// capacity C and simulated-engine parameters calibrated to this machine.
package realrun

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"slices"
	"time"

	"tenantscaler/internal/config"
	"tenantscaler/internal/stack"
	"tenantscaler/internal/workload"
)

// Plan defaults and limits.
const (
	// DefaultInitialReplicas is the warm pool of every scaling setup.
	DefaultInitialReplicas = 2
	// DefaultDrainTimeout bounds the wait for in-flight requests after the
	// scenario's last arrival; it exceeds the gateway's 120 s dispatch
	// timeout, so queued requests resolve (served or 503) first.
	DefaultDrainTimeout = 150 * time.Second
	// DefaultSampleEvery is the timeline resolution.
	DefaultSampleEvery = time.Second
	// stopTimeout bounds a stack's graceful shutdown.
	stopTimeout = 30 * time.Second
	// GuardScript is the machine-safety check DefaultGuard runs.
	GuardScript = "scripts/dev/resource_guard.py"
	// guardRetryEvery and guardMaxWait pace DefaultGuard's retries.
	guardRetryEvery = 30 * time.Second
	guardMaxWait    = 5 * time.Minute
)

// Plan configures Run.
type Plan struct {
	// Config is the configuration of every stack (config/real.yaml for
	// real replicas).
	Config config.Config
	// pinning describes the CPU pinning Run applied (pinServices).
	pinning string
	// ReadinessHold is the injected load delay of every replica (decision
	// Q12, pool.WithReadinessHold); 0 means none.
	ReadinessHold time.Duration
	// Setups are run in this order inside every run index; each must be in
	// stack.Supported.
	Setups []string
	// Runs is the number of runs per setup (>= 1). Run k uses the seed
	// BaseSeed+k for every setup, so all setups replay the same trace.
	Runs     int
	BaseSeed int64
	// Scenario is the replayed workload (workload.RealScenario).
	Scenario workload.Scenario
	// InitialReplicas is the warm pool before traffic starts, in
	// [pool.minReplicas, pool.capacityReplicas] (default
	// DefaultInitialReplicas, clamped to that range; the static partition
	// always runs at capacity).
	InitialReplicas int
	// OutDir receives the results. Run refuses a directory that already
	// holds results of an earlier Run unless Overwrite is set, so the raw
	// files of two experiments are never mixed.
	OutDir string
	// Overwrite removes an earlier Run's results from OutDir (see
	// clearOutputs) before running; other files there are kept.
	Overwrite bool
	// Cooldown separates consecutive runs (the machine cools down and the
	// previous replicas' memory is released).
	Cooldown time.Duration
	// Guard is checked before every run; nil means DefaultGuard logging
	// to Logger.
	Guard func(ctx context.Context) error
	// LogDir receives the llama-server logs ("" = llama.DefaultLogDir).
	LogDir string
	// DrainTimeout bounds the wait for in-flight requests after the
	// scenario's end (default DefaultDrainTimeout); requests still running
	// then are aborted and recorded as failed.
	DrainTimeout time.Duration
	// SampleEvery is the timeline resolution (default 1 s).
	SampleEvery time.Duration
	// Logger receives progress; nil means slog.Default().
	Logger *slog.Logger
}

// normalize validates p and applies the defaults.
func (p Plan) normalize() (Plan, error) {
	if err := p.Config.Validate(); err != nil {
		return p, fmt.Errorf("realrun: config: %w", err)
	}
	if err := cpuOnly(p.Config); err != nil {
		return p, fmt.Errorf("realrun: %w", err)
	}
	if err := p.Scenario.Validate(); err != nil {
		return p, fmt.Errorf("realrun: %w", err)
	}
	known := map[string]bool{}
	for _, t := range p.Config.Tenants {
		known[t.Name] = true
	}
	for _, t := range p.Scenario.Tenants {
		if !known[t.Tenant] {
			return p, fmt.Errorf("realrun: scenario tenant %q is not in the config (the gateway would answer 400)", t.Tenant)
		}
	}
	if len(p.Setups) == 0 {
		return p, errors.New("realrun: no setups")
	}
	for i, s := range p.Setups {
		if !slices.Contains(stack.Supported, s) {
			return p, fmt.Errorf("realrun: unsupported setup %q (want one of %v)", s, stack.Supported)
		}
		if slices.Contains(p.Setups[:i], s) {
			return p, fmt.Errorf("realrun: setup %q listed twice", s)
		}
	}
	switch {
	case p.Runs < 1:
		return p, fmt.Errorf("realrun: runs must be >= 1, got %d", p.Runs)
	case p.OutDir == "":
		return p, errors.New("realrun: no output directory")
	case p.Cooldown < 0 || p.DrainTimeout < 0 || p.SampleEvery < 0 || p.InitialReplicas < 0:
		return p, errors.New("realrun: cooldown, drain timeout, sample interval and initial replicas must be >= 0")
	}
	lo, hi := p.Config.Pool.MinReplicas, p.Config.Pool.CapacityReplicas
	switch {
	case p.InitialReplicas == 0:
		p.InitialReplicas = min(max(DefaultInitialReplicas, lo), hi)
	case p.InitialReplicas < lo || p.InitialReplicas > hi:
		return p, fmt.Errorf("realrun: initial replicas %d outside the pool's [minReplicas %d, capacityReplicas %d]",
			p.InitialReplicas, lo, hi)
	}
	if p.DrainTimeout == 0 {
		p.DrainTimeout = DefaultDrainTimeout
	}
	if p.SampleEvery == 0 {
		p.SampleEvery = DefaultSampleEvery
	}
	if p.Logger == nil {
		p.Logger = slog.Default()
	}
	if p.Guard == nil {
		logger := p.Logger
		p.Guard = func(ctx context.Context) error { return DefaultGuard(ctx, logger) }
	}
	return p, nil
}

// cpuOnly rejects a llama backend that offloads layers to a GPU: real runs
// stay on CPU cores (PLAN.md decision Q8), and their reports say so.
func cpuOnly(cfg config.Config) error {
	if cfg.Backend.Kind == config.BackendLlama && cfg.Backend.Llama.GPULayers != 0 {
		return fmt.Errorf("backend.llama.gpuLayers is %d: real replicas must run on the CPU only (gpuLayers 0; PLAN.md decision Q8)",
			cfg.Backend.Llama.GPULayers)
	}
	return nil
}

// DefaultGuard runs the machine-safety guard (python GuardScript, from the
// working directory) and, while it fails, logs a warning to logger (nil
// means slog.Default()) and re-checks every 30 s for up to 5 minutes
// before giving up.
func DefaultGuard(ctx context.Context, logger *slog.Logger) error {
	if logger == nil {
		logger = slog.Default()
	}
	python, err := exec.LookPath("python")
	if err != nil {
		if python, err = exec.LookPath("python3"); err != nil {
			return errors.New("realrun guard: python is not installed (needed for " + GuardScript + ")")
		}
	}
	if _, err := os.Stat(GuardScript); err != nil {
		return fmt.Errorf("realrun guard: %w (run from the repository root)", err)
	}
	deadline := time.Now().Add(guardMaxWait)
	for {
		var out bytes.Buffer
		cmd := exec.CommandContext(ctx, python, GuardScript)
		cmd.Stdout, cmd.Stderr = &out, &out
		err := cmd.Run()
		if err == nil {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("realrun guard: still failing after %s: %w\n%s", guardMaxWait, err, out.Bytes())
		}
		logger.Warn("resource guard failed; waiting before re-checking", "retryIn", guardRetryEvery, "output", out.String())
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(guardRetryEvery):
		}
	}
}
