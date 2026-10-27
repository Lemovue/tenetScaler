package realrun

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"math"
	"path/filepath"
	"slices"
	"time"

	"tenantscaler/internal/config"
	"tenantscaler/internal/eval"
	"tenantscaler/internal/loadgen"
	"tenantscaler/internal/model"
	"tenantscaler/internal/sim"
	"tenantscaler/internal/stack"
	"tenantscaler/internal/workload"
)

// RunResult is the outcome of one run of one setup.
type RunResult struct {
	Setup string `json:"setup"`
	Run   int    `json:"run"`
	Seed  int64  `json:"seed"`
	// Simulated is true when the replicas' compute was simulated.
	Simulated bool             `json:"simulated"`
	Engine    stack.EngineInfo `json:"engine"`
	Metrics   eval.RunMetrics  `json:"metrics"`
	// Requests is the number of requests sent.
	Requests int `json:"requests"`
	// InitialReplicas is the warm pool the stack started before traffic.
	InitialReplicas int `json:"initialReplicas"`
	// ControlPhaseSeconds is the seeded delay of the control loop's first
	// sync after the stack started (absent without a control plane).
	ControlPhaseSeconds *float64 `json:"controlPhaseSeconds,omitempty"`
	// StartedAt is when traffic started; StartupSeconds is how long the
	// stack took to start (initial replicas ready); WallSeconds is the
	// time from the first request until the last one finished.
	StartedAt      time.Time `json:"startedAt"`
	StartupSeconds float64   `json:"startupSeconds"`
	WallSeconds    float64   `json:"wallSeconds"`
	// ColdStartSeconds are the launch-to-ready times of every replica
	// the run's pool brought up (the warm pool, scale-ups, replacements),
	// including those scaled away before the run ended, by replica ID;
	// measured for real replicas, configured for simulated ones.
	ColdStartSeconds []float64 `json:"coldStartSeconds"`
	// DrainTimedOut is true when requests were still running
	// DrainTimeout after the scenario ended (they are recorded as failed).
	DrainTimedOut bool `json:"drainTimedOut"`
	// FailedBy counts failed requests by cause, as the client saw them.
	FailedBy map[string]int `json:"failedBy,omitempty"`
	// Order is the run's position (1-based) in its run index's seeded
	// setup order; Sequence its position among all runs of the plan.
	Order    int `json:"order"`
	Sequence int `json:"sequence"`
	// CPUPerformanceStartPct and CPUPerformanceEndPct are the CPU's
	// effective frequency, % of nominal, when traffic started and ended
	// (absent where not readable).
	CPUPerformanceStartPct *float64 `json:"cpuPerformanceStartPct,omitempty"`
	CPUPerformanceEndPct   *float64 `json:"cpuPerformanceEndPct,omitempty"`
	// TokenizerFallbacks counts prompts the gateway counted in words
	// because the replicas' tokenizer was unavailable (0 on a clean run).
	TokenizerFallbacks int64 `json:"tokenizerFallbacks"`

	data eval.RunData // for the plots
}

// Run executes the plan: for every run index k and every setup, one at a
// time (never two stacks at once), it checks the guard, starts the stack,
// replays the scenario (seed BaseSeed+k) at real time while sampling the
// stack every second, waits for in-flight requests (bounded by
// DrainTimeout), stops the stack and cools down. Setups are interleaved
// within each run index, so slow drifts of the machine (temperature,
// background load) affect all setups alike. It then writes the results to
// OutDir (see writeOutputs). On an error or cancellation it writes what
// was completed and returns the error with that partial summary.
func Run(ctx context.Context, plan Plan) (Summary, error) {
	p, err := plan.normalize()
	if err != nil {
		return Summary{}, err
	}
	if err := prepareOutDir(p); err != nil {
		return Summary{}, err
	}
	if p.pinning, err = pinServices(p.Config, p.Logger); err != nil {
		return Summary{}, fmt.Errorf("realrun: %w", err)
	}
	var results []RunResult
	finish := func(runErr error) (Summary, error) {
		sum := buildSummary(p, results)
		if len(results) > 0 {
			runErr = errors.Join(runErr, writeOutputs(p, sum))
		}
		return sum, runErr
	}
	first, seq := true, 0
	for k := range p.Runs {
		seed := p.BaseSeed + int64(k)
		trace := workload.Generate(p.Scenario, seed)
		for pos, setup := range runOrder(p.Setups, p.BaseSeed, k) {
			if !first {
				if err := sleepCtx(ctx, p.Cooldown); err != nil {
					return finish(err)
				}
			}
			first = false
			if err := p.Guard(ctx); err != nil {
				return finish(fmt.Errorf("realrun: guard before %s run %d: %w", setup, k, err))
			}
			seq++
			rr, err := runOne(ctx, p, setup, k, seed, trace)
			if err != nil {
				return finish(fmt.Errorf("realrun: %s run %d: %w", setup, k, err))
			}
			rr.Order, rr.Sequence = pos+1, seq
			results = append(results, rr)
			p.Logger.Info("run done", "setup", setup, "run", k, "victimViolationPct", rr.Metrics.VictimViolationPct,
				"noisyShedPct", rr.Metrics.NoisyShedPct, "completed", rr.Metrics.Completed, "failed", rr.Metrics.Failed)
		}
	}
	return finish(nil)
}

// runOne runs one setup once and writes its raw records and timeline.
func runOne(ctx context.Context, p Plan, setup string, k int, seed int64, trace []workload.Spec) (RunResult, error) {
	log := p.Logger.With("setup", setup, "run", k)
	opt := stack.Options{
		Setup: setup, Config: p.Config, InitialReplicas: p.InitialReplicas, LogDir: p.LogDir, Seed: seed,
		Logger:        p.Logger.With("run", k), // the stack adds the setup
		ReadinessHold: p.ReadinessHold,
	}
	if stack.HasController(setup) {
		opt.DecisionsFile = filepath.Join(p.OutDir, decisionsDir, runFile(setup, k, ".jsonl"))
	}
	began := time.Now()
	st, err := stack.Start(ctx, opt)
	if err != nil {
		return RunResult{}, err
	}
	rr := RunResult{Setup: setup, Run: k, Seed: seed, Engine: st.Engine(), Requests: len(trace),
		InitialReplicas: st.InitialReplicas(), StartupSeconds: time.Since(began).Seconds()}
	if phase, ok := st.ControlPhase(); ok {
		secs := phase.Seconds()
		rr.ControlPhaseSeconds = &secs
	}
	log.Info("replaying scenario", "scenario", p.Scenario.Name, "requests", len(trace), "url", st.URL())

	rr.CPUPerformanceStartPct = readable(CPUPerformancePct())
	start := time.Now()
	rr.StartedAt = start
	sctx, stopSampling := context.WithCancel(ctx)
	timeline := make(chan []eval.Sample, 1)
	go func() { timeline <- sampleTimeline(sctx, st, start, p.SampleEvery, p.Scenario.Duration) }()
	rctx, cancel := context.WithTimeout(ctx, p.Scenario.Duration+p.DrainTimeout)
	res, replayErr := loadgen.ReplayResult(rctx, trace, []loadgen.Target{{Name: setup, URL: st.URL()}},
		loadgen.Options{Speed: 1, Seed: seed, Logger: log})
	cancel()
	rr.WallSeconds = time.Since(start).Seconds()
	rr.CPUPerformanceEndPct = readable(CPUPerformancePct())
	rr.TokenizerFallbacks = st.TokenizerFallbacks()
	stopSampling()
	samples := <-timeline
	actions, latencies := st.ActionTimes(start), st.PolicyLatencies()
	stopCtx, stopCancel := context.WithTimeout(context.Background(), stopTimeout)
	stopErr := st.Stop(stopCtx)
	stopCancel()
	colds := st.ColdStarts() // complete once the stack has stopped
	for _, id := range slices.Sorted(maps.Keys(colds)) {
		rr.ColdStartSeconds = append(rr.ColdStartSeconds, colds[id])
	}

	switch {
	case ctx.Err() != nil:
		return rr, errors.Join(ctx.Err(), stopErr)
	case errors.Is(replayErr, context.DeadlineExceeded):
		rr.DrainTimedOut = true
		log.Warn("requests still running after the drain timeout were aborted", "drainTimeout", p.DrainTimeout)
	case replayErr != nil:
		return rr, errors.Join(replayErr, stopErr)
	}
	if stopErr != nil {
		return rr, fmt.Errorf("stop stack: %w", stopErr)
	}
	rr.Simulated = st.Simulated() || res.Simulated(setup)
	rr.FailedBy = res.Counts[setup].FailedBy
	rr.data = runData(p, setup, k, seed, res.Records[setup], samples, actions, latencies)
	rr.Metrics = eval.Compute(rr.data)
	return rr, writeRunFiles(p.OutDir, rr, res)
}

// runData assembles the evaluation input of one run.
func runData(p Plan, setup string, k int, seed int64, recs []eval.Record, timeline []eval.Sample,
	actions, latencies []time.Duration) eval.RunData {
	roles := sim.ScenarioRoles(p.Scenario)
	return eval.RunData{
		Setup: setup, Run: k, Seed: seed, Records: recs, Timeline: timeline,
		SLOs: slos(p.Config), GuaranteedSlots: guaranteedSlots(p.Config),
		Victims: roles.Victims, Noisy: roles.Noisy, SpikeStart: roles.SpikeStart, SpikeEnd: roles.SpikeEnd,
		ActionTimes: actions, PolicyLatencies: latencies, Owner: roles.Owner, OwnerReturn: roles.OwnerReturn,
	}
}

// sampleTimeline samples the stack every interval from start until the
// scenario duration has elapsed or ctx is done.
func sampleTimeline(ctx context.Context, st *stack.Stack, start time.Time, every, until time.Duration) []eval.Sample {
	var out []eval.Sample
	tick := time.NewTicker(every)
	defer tick.Stop()
	for {
		smp := st.Sample(start)
		if smp.T >= until {
			return out
		}
		out = append(out, smp)
		select {
		case <-ctx.Done():
			return out
		case <-tick.C:
		}
	}
}

// slos maps every tenant to its SLO.
func slos(cfg config.Config) map[string]model.SLO {
	m := make(map[string]model.SLO, len(cfg.Tenants))
	for _, t := range cfg.Tenants {
		m[t.Name] = t.SLO
	}
	return m
}

// guaranteedSlots is every tenant's guaranteed replicas times the dispatch
// slots per replica.
func guaranteedSlots(cfg config.Config) map[string]int {
	m := make(map[string]int, len(cfg.Tenants))
	for _, t := range cfg.Tenants {
		m[t.Name] = t.GuaranteedReplicas * cfg.SlotsPerReplica()
	}
	return m
}

// runFile names a per-run file: "<setup>-run<k><ext>".
func runFile(setup string, k int, ext string) string { return fmt.Sprintf("%s-run%d%s", setup, k, ext) }

// sleepCtx waits d or until ctx is done.
func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// meanOf is the mean of xs, NaN when empty.
func meanOf(xs []float64) float64 {
	if len(xs) == 0 {
		return math.NaN()
	}
	s := 0.0
	for _, x := range xs {
		s += x
	}
	return s / float64(len(xs))
}
