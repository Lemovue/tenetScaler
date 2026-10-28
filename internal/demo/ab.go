package demo

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"path/filepath"
	"sync"
	"time"

	"tenantscaler/internal/eval"
	"tenantscaler/internal/gateway"
	"tenantscaler/internal/loadgen"
	"tenantscaler/internal/model"
	"tenantscaler/internal/sim"
	"tenantscaler/internal/stack"
	"tenantscaler/internal/workload"
)

// abRun is one A/B run: the two stacks, one after the other.
type abRun struct {
	id     int
	seed   int64
	opt    *Options
	roles  sim.Roles
	cancel context.CancelFunc
	done   chan struct{} // closed when the run has ended and every stack is stopped
	stats  *http.Client  // reads the running stack's gateway /stats

	mu        sync.Mutex
	phase     string
	message   string
	err       string
	startedAt time.Time
	stacks    []*stackRun
	live      *stack.Stack           // the running TenantScaler stack (live decisions)
	decisions []model.DecisionRecord // the stopped TenantScaler stack's decisions
	decStart  time.Time              // traffic start of the TenantScaler stack
	liveMode  *liveMode              // set for a live run (StartLive)
	observe   *ObserveTarget         // set for an observing run (StartObserve)

	// dataset is an A/B run's source of request sizes (a Dataset key; ""
	// in live and observing runs, which replay no trace), loadDataset
	// reads a trace dataset, and datasetUse is what the run used, once
	// known.
	dataset     string
	loadDataset func(key string) ([]workload.LengthSample, DatasetUse, error)
	datasetUse  *DatasetUse
}

// stackRun is the progress of one stack, guarded by abRun.mu.
type stackRun struct {
	plan          stackPlan
	status, err   string
	engine        *stack.EngineInfo
	simulated     bool
	trafficStart  time.Time
	elapsed       time.Duration
	requests      int
	gatewayURL    string // set while the stack runs
	samples       []LiveSample
	metrics       *eval.RunMetrics
	tenants       []TenantResult
	drainTimedOut bool
}

// newRun prepares an A/B run; execute runs it.
func newRun(id int, seed int64, opt *Options, roles sim.Roles, cancel context.CancelFunc) *abRun {
	r := &abRun{id: id, seed: seed, opt: opt, roles: roles, cancel: cancel, done: make(chan struct{}),
		stats: &http.Client{Timeout: 2 * time.Second}, phase: PhaseRunning, startedAt: time.Now(),
		message: "Starting the A/B run"}
	for _, p := range abStacks {
		r.stacks = append(r.stacks, &stackRun{plan: p, status: StatusPending})
	}
	return r
}

// finished reports whether the run has ended.
func (r *abRun) finished() bool {
	select {
	case <-r.done:
		return true
	default:
		return false
	}
}

// execute runs both stacks in order and records how the run ended.
func (r *abRun) execute(ctx context.Context) {
	defer close(r.done)
	defer r.cancel()
	log := r.opt.Logger.With("abRun", r.id, "seed", r.seed)
	sc, use := r.opt.Scenario, syntheticUse()
	if r.dataset != "" && r.dataset != DatasetSynthetic {
		r.setMessage("Reading the " + datasetName(r.dataset) + " (once per server: a large file takes a few seconds)")
		samples, u, err := r.loadDataset(r.dataset)
		if err != nil {
			r.end(err, log)
			return
		}
		sc.Samples, use = samples, u
	}
	r.mu.Lock()
	r.datasetUse = &use
	r.mu.Unlock()
	trace := workload.Generate(sc, r.seed)
	log.Info("A/B run started", "scenario", sc.Name, "dataset", use.Key, "requests", len(trace), "backend", r.opt.Backend.Kind)
	for i := range r.stacks {
		if i > 0 && r.opt.Cooldown > 0 {
			r.setMessage(fmt.Sprintf("Cooling down for %s before %s", r.opt.Cooldown, r.stacks[i].plan.title))
			if err := sleepCtx(ctx, r.opt.Cooldown); err != nil {
				r.end(err, log)
				return
			}
		}
		if err := r.runStack(ctx, i, trace, log); err != nil {
			r.end(err, log)
			return
		}
	}
	r.end(nil, log)
}

// end records the outcome: done, stopped (ctx canceled), stopped with a
// failed shutdown (*stoppedError) or error. It claims that the stack was
// shut down only when stopping it reported no error.
func (r *abRun) end(err error, log *slog.Logger) {
	r.mu.Lock()
	defer r.mu.Unlock()
	level := slog.LevelInfo
	var stopped *stoppedError
	switch {
	case err == nil:
		r.phase, r.message = PhaseDone, "Both stacks finished: compare the panels and the summary"
	case errors.As(err, &stopped):
		r.phase, r.err, level = PhaseStopped, stopped.Error(), slog.LevelError
		r.message = "Stopped, but shutting the stack down reported an error"
		if !r.opt.Backend.Simulated {
			r.message += ": llama-server replicas may still be running; run tenantscaler cleanup to kill them"
		}
	case errors.Is(err, context.Canceled):
		r.phase, r.message = PhaseStopped, "Stopped: every replica of the stack was shut down"
	default:
		r.phase, r.message, r.err, level = PhaseError, "The A/B run failed", err.Error(), slog.LevelError
	}
	for _, s := range r.stacks {
		if s.status == StatusPending {
			s.status = StatusSkipped
		}
	}
	log.Log(context.Background(), level, "A/B run ended", "phase", r.phase, "err", err)
}

// runStack runs stack i: start (warm pool ready), replay the trace while
// sampling, stop, then compute its metrics. A canceled ctx stops the
// stack and returns context.Canceled.
func (r *abRun) runStack(ctx context.Context, i int, trace []workload.Spec, log *slog.Logger) error {
	sr := r.stacks[i]
	setup := sr.plan.setup
	r.setStatus(i, StatusStarting, fmt.Sprintf("%s: starting %d warm replicas (%s)", sr.plan.title, r.opt.InitialReplicas, r.opt.Backend.Label))
	if r.opt.Guard != nil {
		if err := r.opt.Guard(ctx); err != nil {
			return r.fail(ctx, i, fmt.Errorf("machine-safety guard before %s: %w", setup, err))
		}
	}
	opt := stack.Options{Setup: setup, Config: r.opt.Config, InitialReplicas: r.opt.InitialReplicas,
		LogDir: r.opt.LogDir, Seed: r.seed, Logger: r.opt.Logger.With("abRun", r.id)}
	if stack.HasController(setup) && r.opt.LogDir != "" {
		opt.DecisionsFile = filepath.Join(r.opt.LogDir, r.decisionsFileName())
	}
	// On error Start has stopped what it started; when the run was
	// stopped, anything its error reports besides the cancellation is a
	// failure of that cleanup, and fail reports it.
	st, err := stack.Start(ctx, opt)
	if err != nil {
		return r.fail(ctx, i, err)
	}
	start := time.Now()
	r.trafficStarted(i, st, start, len(trace))
	log.Info("replaying the trace", "setup", setup, "requests", len(trace), "url", st.URL())

	sctx, stopSampling := context.WithCancel(ctx)
	timeline := make(chan []eval.Sample, 1)
	go func() { timeline <- r.sample(sctx, i, st, start) }()
	dur := r.opt.Scenario.Duration
	rctx, cancelReplay := context.WithTimeout(ctx, dur+r.opt.DrainTimeout)
	res, replayErr := loadgen.ReplayResult(rctx, trace, []loadgen.Target{{Name: setup, URL: st.URL()}},
		loadgen.Options{Speed: 1, Seed: r.seed, Logger: r.opt.Logger.With("abRun", r.id, "setup", setup)})
	cancelReplay()
	stopSampling()
	samples := <-timeline
	actions, latencies := st.ActionTimes(start), st.PolicyLatencies()

	r.setStatus(i, StatusStopping, fmt.Sprintf("%s: stopping the stack", sr.plan.title))
	stopCtx, cancelStop := context.WithTimeout(context.Background(), stopTimeout)
	stopErr := st.Stop(stopCtx)
	cancelStop()
	r.stackStopped(i, st, start)
	if stopErr != nil {
		stopErr = fmt.Errorf("stop %s: %w", setup, stopErr)
	}

	drainTimedOut := false
	switch {
	case ctx.Err() != nil:
		return r.fail(ctx, i, errors.Join(ctx.Err(), stopErr))
	case errors.Is(replayErr, context.DeadlineExceeded):
		drainTimedOut = true
		log.Warn("requests still running after the drain timeout were aborted", "setup", setup, "drainTimeout", r.opt.DrainTimeout)
	case replayErr != nil:
		return r.fail(ctx, i, errors.Join(replayErr, stopErr))
	}
	if stopErr != nil {
		return r.fail(ctx, i, stopErr)
	}
	recs := res.Records[setup]
	slos := r.slos()
	m := eval.Compute(eval.RunData{
		Setup: setup, Run: r.id, Seed: r.seed, Records: recs, Timeline: samples,
		SLOs: slos, GuaranteedSlots: r.guaranteedSlots(),
		Victims: r.roles.Victims, Noisy: r.roles.Noisy, SpikeStart: r.roles.SpikeStart, SpikeEnd: r.roles.SpikeEnd,
		ActionTimes: actions, PolicyLatencies: latencies, Owner: r.roles.Owner, OwnerReturn: r.roles.OwnerReturn,
	})
	r.mu.Lock()
	sr.status, sr.metrics, sr.drainTimedOut = StatusDone, &m, drainTimedOut
	sr.tenants = tenantResults(tenantNames(r.opt.Scenario), recs, slos, m)
	sr.simulated = sr.simulated || res.Simulated(setup)
	r.mu.Unlock()
	log.Info("stack finished", "setup", setup, "victimViolationPct", m.VictimViolationPct,
		"replicaMinutes", m.ReplicaMinutes, "completed", m.Completed, "shed", m.Shed, "failed", m.Failed)
	return nil
}

// fail ends stack i. When ctx was canceled (the run was stopped) it marks
// the stack stopped and returns context.Canceled or, when err reports more
// than the cancellation (stopping the stack failed), records that part as
// the stack's error and returns it as a *stoppedError. Otherwise it marks
// the stack failed and returns err.
func (r *abRun) fail(ctx context.Context, i int, err error) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	sr := r.stacks[i]
	if ctx.Err() != nil {
		sr.status = StatusStopped
		cause := beyondCancel(err)
		if cause == nil {
			return context.Canceled
		}
		sr.err = cause.Error()
		return &stoppedError{title: sr.plan.title, cause: cause}
	}
	sr.status, sr.err = StatusFailed, err.Error()
	return fmt.Errorf("%s: %w", sr.plan.title, err)
}

// decisionsFileName names the JSON Lines file of the run's controller
// decisions after the run's start (UTC) and number, so a restarted demo
// never appends to the file of an earlier session.
func (r *abRun) decisionsFileName() string {
	return fmt.Sprintf("decisions-%s-run%d.jsonl", r.startedAt.UTC().Format("20060102T150405Z"), r.id)
}

// sample records a live sample every SampleEvery until ctx is done, and
// returns the evaluation timeline: the samples taken within the scenario's
// duration, as the real-replica evaluation records them.
func (r *abRun) sample(ctx context.Context, i int, st *stack.Stack, start time.Time) []eval.Sample {
	var timeline []eval.Sample
	tick := time.NewTicker(r.opt.SampleEvery)
	defer tick.Stop()
	for {
		smp := st.Sample(start)
		gs, err := r.gatewayStats(ctx, st.URL())
		if err != nil && ctx.Err() == nil {
			r.opt.Logger.Debug("gateway stats unavailable", "err", err)
		}
		if smp.T < r.opt.Scenario.Duration {
			timeline = append(timeline, smp)
		}
		r.addSample(i, smp, liveSample(smp, gs), gs)
		select {
		case <-ctx.Done():
			return timeline
		case <-tick.C:
		}
	}
}

// gatewayStats reads the stack gateway's statistics over the live window;
// nil with an error when they cannot be read.
func (r *abRun) gatewayStats(ctx context.Context, base string) (*gateway.Stats, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/stats?window="+r.opt.Window.String(), nil)
	if err != nil {
		return nil, err
	}
	if r.observe != nil && r.observe.AdminKey != "" { // an observed gateway guards /stats
		req.Header.Set("Authorization", "Bearer "+r.observe.AdminKey)
	}
	resp, err := r.stats.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s/stats: %s", base, resp.Status)
	}
	var gs gateway.Stats
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&gs); err != nil {
		return nil, fmt.Errorf("decode %s/stats: %w", base, err)
	}
	return &gs, nil
}

// addSample appends a live sample of stack i and updates its status and
// the progress message.
func (r *abRun) addSample(i int, smp eval.Sample, ls LiveSample, gs *gateway.Stats) {
	r.mu.Lock()
	defer r.mu.Unlock()
	sr := r.stacks[i]
	if sr.status != StatusRunning && sr.status != StatusDraining {
		return
	}
	sr.samples = append(sr.samples, ls)
	sr.elapsed = smp.T
	if r.liveMode != nil || r.observe != nil { // no trace: runs until stopped
		return
	}
	dur := r.opt.Scenario.Duration
	if smp.T < dur {
		r.message = fmt.Sprintf("%s: replaying traffic, %.0f s of %.0f s", sr.plan.title, smp.T.Seconds(), dur.Seconds())
		return
	}
	sr.status = StatusDraining
	r.message = fmt.Sprintf("%s: trace sent, waiting for the last requests", sr.plan.title)
	if gs != nil {
		r.message = fmt.Sprintf("%s: trace sent, waiting for %d requests in flight or queued", sr.plan.title, gs.InFlight+gs.Queued)
	}
}

// setStatus sets stack i's status and the run's message.
func (r *abRun) setStatus(i int, status, message string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.stacks[i].status, r.message = status, message
}

// setMessage sets the run's message.
func (r *abRun) setMessage(message string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.message = message
}

// trafficStarted records that stack i's replay begins.
func (r *abRun) trafficStarted(i int, st *stack.Stack, start time.Time, requests int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	sr := r.stacks[i]
	eng := st.Engine()
	sr.status, sr.engine, sr.simulated, sr.trafficStart, sr.requests = StatusRunning, &eng, st.Simulated(), start, requests
	sr.gatewayURL = st.URL()
	r.message = fmt.Sprintf("%s: replaying traffic", sr.plan.title)
	if stack.HasController(sr.plan.setup) {
		r.live, r.decisions, r.decStart = st, nil, start
	}
}

// stackStopped records that stack i has stopped (its gateway no longer
// listens) and keeps the decisions of a TenantScaler stack.
func (r *abRun) stackStopped(i int, st *stack.Stack, start time.Time) {
	var recs []model.DecisionRecord
	controlled := stack.HasController(st.Setup())
	if controlled {
		recs = st.Decisions(0)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.stacks[i].gatewayURL = ""
	if controlled {
		r.live, r.decisions, r.decStart = nil, recs, start
	}
}

// slos maps every configured tenant to its SLO.
func (r *abRun) slos() map[string]model.SLO {
	m := make(map[string]model.SLO, len(r.opt.Config.Tenants))
	for _, t := range r.opt.Config.Tenants {
		m[t.Name] = t.SLO
	}
	return m
}

// guaranteedSlots is every tenant's guaranteed replicas times the dispatch
// slots per replica.
func (r *abRun) guaranteedSlots() map[string]int {
	m := make(map[string]int, len(r.opt.Config.Tenants))
	for _, t := range r.opt.Config.Tenants {
		m[t.Name] = t.GuaranteedReplicas * r.opt.Config.SlotsPerReplica()
	}
	return m
}

// sleepCtx waits d or until ctx is done.
func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
