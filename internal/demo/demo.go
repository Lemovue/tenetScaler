// Package demo serves the TenantScaler dashboard and runs its sequential A/B
// comparison (DESIGN.md section 18, PLAN.md decision Q7).
//
// Run A/B replays one recorded trace (workload.RealDemoScenario by default)
// first on the baseline stack, S2-keda-queue (passthrough gateway + the
// queue-based KEDA autoscaler), and then on TenantScaler, S7-ts-full (fair
// gateway + controller). The stacks run one at a time through
// internal/stack, each with the config's full replica budget, the same warm
// pool and the same seed, so both see identical traffic. While a stack runs
// the server samples it every second (replicas ready, starting and desired;
// per tenant the gateway-measured p99 TTFT, SLO attainment, arrival rate and
// shed requests over a 10 s window); when it finishes, eval.Compute turns the
// client-side records into the run's metrics.
//
// The replicas are REAL llama.cpp processes or the SIMULATED engine,
// whichever the config's backend selects (see Resolve); every label the
// dashboard shows says which.
package demo

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"tenantscaler/internal/config"
	"tenantscaler/internal/realrun"
	"tenantscaler/internal/sim"
	"tenantscaler/internal/workload"
)

const (
	// DefaultWindow is the window of the live per-tenant statistics.
	DefaultWindow = 10 * time.Second
	// DefaultSampleEvery is the live sampling interval (and the timeline
	// resolution of the evaluation).
	DefaultSampleEvery = time.Second
	// DefaultResultsDir is where /api/results looks for evaluation
	// summaries.
	DefaultResultsDir = "results"
	// RealCooldown is the pause between the two stacks on real replicas:
	// the first stack's llama-server processes have exited and released
	// their memory, and the cores have cooled a little, before the second
	// stack starts.
	RealCooldown = 10 * time.Second
	// stopTimeout bounds a stack's graceful shutdown.
	stopTimeout = 30 * time.Second
)

// Stack keys of the A/B comparison, in run order.
const (
	// KeyBaseline is the queue-based KEDA baseline (S2-keda-queue).
	KeyBaseline = "baseline"
	// KeyTenantScaler is the full TenantScaler stack (S7-ts-full).
	KeyTenantScaler = "tenantscaler"
)

// stackPlan is one side of the A/B comparison.
type stackPlan struct {
	key, setup, title string
}

// abStacks are the two stacks, in run order.
var abStacks = []stackPlan{
	{key: KeyBaseline, setup: sim.SetupKEDAQueue, title: "Baseline: queue-based KEDA"},
	{key: KeyTenantScaler, setup: sim.SetupFull, title: "TenantScaler"},
}

// Options configures a Server.
type Options struct {
	// Config is the configuration of both stacks; its backend selects real
	// or simulated replicas (see Resolve).
	Config config.Config
	// Backend describes the backend for the dashboard's labels; the zero
	// value means DescribeBackend(Config).
	Backend BackendInfo
	// Scenario is the replayed trace (see ScenarioByName).
	Scenario workload.Scenario
	// Seed is the default seed of an A/B run: it keys the trace, the
	// prompts and both stacks, so the two stacks see identical traffic.
	Seed int64
	// InitialReplicas is the warm pool each stack starts (and waits for)
	// before traffic; 0 means realrun.DefaultInitialReplicas. It is
	// clamped to [pool.minReplicas, pool.capacityReplicas].
	InitialReplicas int
	// LogDir receives the llama-server logs and the TenantScaler
	// controller's decision records (JSON Lines, one file per A/B run:
	// decisions-<start, UTC>-run<N>.jsonl); "" keeps the llama logs in
	// their default directory and the decisions in memory only.
	LogDir string
	// ResultsDir is where /api/results looks (default DefaultResultsDir).
	ResultsDir string
	// SampleEvery is the live sampling interval (default
	// DefaultSampleEvery); Window the live statistics window (default
	// DefaultWindow).
	SampleEvery, Window time.Duration
	// DrainTimeout bounds the wait for requests still in flight after the
	// scenario's last arrival (default realrun.DefaultDrainTimeout); the
	// requests still running then are aborted and recorded as failed.
	DrainTimeout time.Duration
	// Cooldown separates the two stacks (RealCooldown is advised for real
	// replicas; 0 for none).
	Cooldown time.Duration
	// Guard, when set, is checked before each stack starts (for example a
	// closure over realrun.DefaultGuard, the machine-safety check); an
	// error aborts the A/B run.
	Guard func(ctx context.Context) error
	// Logger receives progress; nil means slog.Default().
	Logger *slog.Logger
}

// normalize validates o and applies the defaults.
func (o Options) normalize() (Options, error) {
	if err := o.Config.Validate(); err != nil {
		return o, fmt.Errorf("demo: config: %w", err)
	}
	if o.Config.Backend.Kind == config.BackendLlama && o.Config.Backend.Llama.GPULayers != 0 {
		return o, fmt.Errorf("demo: backend.llama.gpuLayers is %d: real replicas run on the CPU only (gpuLayers 0; PLAN.md decision Q8)",
			o.Config.Backend.Llama.GPULayers)
	}
	if err := o.Scenario.Validate(); err != nil {
		return o, fmt.Errorf("demo: %w", err)
	}
	known := map[string]bool{}
	for _, t := range o.Config.Tenants {
		known[t.Name] = true
	}
	for _, t := range o.Scenario.Tenants {
		if !known[t.Tenant] {
			return o, fmt.Errorf("demo: scenario tenant %q is not in the config (the gateway would answer 400)", t.Tenant)
		}
	}
	if o.InitialReplicas < 0 || o.SampleEvery < 0 || o.Window < 0 || o.DrainTimeout < 0 || o.Cooldown < 0 {
		return o, errors.New("demo: initial replicas, sample interval, window, drain timeout and cooldown must be >= 0")
	}
	if o.InitialReplicas == 0 {
		o.InitialReplicas = realrun.DefaultInitialReplicas
	}
	o.InitialReplicas = min(max(o.InitialReplicas, o.Config.Pool.MinReplicas), o.Config.Pool.CapacityReplicas)
	if o.ResultsDir == "" {
		o.ResultsDir = DefaultResultsDir
	}
	if o.SampleEvery == 0 {
		o.SampleEvery = DefaultSampleEvery
	}
	if o.Window == 0 {
		o.Window = DefaultWindow
	}
	if o.DrainTimeout == 0 {
		o.DrainTimeout = realrun.DefaultDrainTimeout
	}
	if o.Backend.Kind == "" {
		o.Backend = DescribeBackend(o.Config, "")
	}
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	return o, nil
}
