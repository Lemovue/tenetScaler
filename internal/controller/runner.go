package controller

import (
	"context"
	"errors"
	"log/slog"
	"maps"
	"sync"
	"time"

	"tenantscaler/internal/control"
	"tenantscaler/internal/model"
)

// minCycleTimeout is the shortest deadline Run gives one cycle, so a tiny
// test cycle does not starve real HTTP calls.
const minCycleTimeout = time.Second

// Cycle stages whose failures the Runner reports in Status.Errors and in
// tenantscaler_controller_errors_total{stage}.
const (
	StageTenants     = "tenants"     // BeforeCycle (e.g. listing TenantScalers)
	StageMetrics     = "metrics"     // MetricsSource.Observe
	StagePoolStatus  = "poolStatus"  // Actuator.Status
	StageActuation   = "actuation"   // Actuator.SetReplicas
	StagePublish     = "publish"     // Publisher.Publish
	StageDecisionLog = "decisionLog" // DecisionLog file writes
)

// Runner drives control.Core in real time: read metrics and the pool, step
// the core, scale the pool, publish the policy, log the decision. Create it
// with a composite literal and use it through a pointer.
type Runner struct {
	Core     *control.Core
	Source   MetricsSource
	Actuator Actuator
	// Publisher is nil when gateway coupling is off.
	Publisher Publisher
	// Log keeps the decision records; nil means they are not kept.
	Log *DecisionLog
	// Cycle is the control period used by Run.
	Cycle time.Duration
	// Clock stamps observations; nil means time.Now.
	Clock func() time.Time
	// Logger receives cycle summaries and errors; nil means slog.Default().
	Logger *slog.Logger
	// BeforeCycle runs first in every cycle (e.g. reload tenants from
	// TenantScalers). Its error is reported and the cycle goes on.
	BeforeCycle func(ctx context.Context) error
	// AfterDecision runs last in every cycle (e.g. write TenantScaler status).
	AfterDecision func(ctx context.Context, d control.Decision)

	once      sync.Once
	cycleMu   sync.Mutex // serializes cycles: Core is not goroutine-safe
	poolKnown bool       // the pool status was read at least once; guarded by cycleMu
	mu        sync.Mutex // guards status
	status    Status
	metrics   *runnerMetrics
}

// ErrorRecord is the most recent failure of one cycle stage. Compare Cycle
// with Status.Cycle to tell a current problem from a past one.
type ErrorRecord struct {
	Cycle int64     `json:"cycle"`
	Time  time.Time `json:"time"`
	Error string    `json:"error"`
}

// Status is what GET /status reports.
type Status struct {
	// Cycle is the decision core's number of the last cycle: 0 before the
	// first, and also while the pool status was never read (the core is
	// not stepped then).
	Cycle        int64   `json:"cycle"`
	CycleSeconds float64 `json:"cycleSeconds"`
	// Time is when the last cycle observed the system.
	Time        time.Time              `json:"time"`
	FailStatic  bool                   `json:"failStatic"`
	Pool        model.PoolObservation  `json:"pool"`
	Observation *model.Observation     `json:"observation,omitempty"`
	Decision    *control.Decision      `json:"decision,omitempty"`
	Errors      map[string]ErrorRecord `json:"errors"`
}

// stageError is one failure inside a cycle.
type stageError struct {
	stage string
	err   error
}

func (r *Runner) init() {
	r.status.Errors = map[string]ErrorRecord{}
	r.metrics = newRunnerMetrics()
}

func (r *Runner) logger() *slog.Logger {
	if r.Logger != nil {
		return r.Logger
	}
	return slog.Default()
}

func (r *Runner) now() time.Time {
	if r.Clock != nil {
		return r.Clock()
	}
	return time.Now()
}

// validate reports a Runner that cannot run.
func (r *Runner) validate() error {
	var errs []error
	if r.Core == nil {
		errs = append(errs, errors.New("Core is nil"))
	}
	if r.Source == nil {
		errs = append(errs, errors.New("Source is nil"))
	}
	if r.Actuator == nil {
		errs = append(errs, errors.New("Actuator is nil"))
	}
	if err := errors.Join(errs...); err != nil {
		return errors.Join(errors.New("controller runner"), err)
	}
	return nil
}

// RunOnce runs one control cycle and returns its decision. Core, Source and
// Actuator must be set. If the metrics or the pool status cannot be read the
// core fails static: the pool is not scaled and the last policy is
// republished (so a restarted gateway gets it back). Until the pool status
// has been read once the core is not stepped at all (see waitForPool).
// Every stage failure is logged and kept in Status.Errors; it never stops
// the cycle.
//
// The published policy is stamped with the time it is sent, so the
// gateway's propagation latency (R14) measures delivery only, not the pool
// round trips of this cycle or the age of a republished policy. A gateway
// that ignores the policy because it holds a newer version (after a
// controller restart) makes the core continue numbering above that version.
func (r *Runner) RunOnce(ctx context.Context) control.Decision {
	r.once.Do(r.init)
	r.cycleMu.Lock()
	defer r.cycleMu.Unlock()
	var errs []stageError
	fail := func(stage string, err error) { errs = append(errs, stageError{stage, err}) }

	if r.BeforeCycle != nil {
		if err := r.BeforeCycle(ctx); err != nil {
			fail(StageTenants, err)
		}
	}
	obs := r.observe(ctx, fail)

	var d control.Decision
	if r.poolKnown {
		d = r.Core.Step(obs)
	} else {
		d = r.waitForPool(obs)
	}
	if !d.FailStatic {
		if err := r.Actuator.SetReplicas(ctx, d.PoolReplicas); err != nil {
			fail(StageActuation, err)
		}
	}
	if d.Policy != nil && r.Publisher != nil {
		pol := *d.Policy // a copy: the core keeps d.Policy as its last policy
		pol.IssuedAt = r.now()
		if err := r.Publisher.Publish(ctx, pol); err != nil {
			fail(StagePublish, err)
			if v := staleGatewayVersion(err); r.adoptPolicyVersion(v) {
				r.logger().Warn("a gateway holds a newer policy version (controller restarted?): continuing above it",
					"gatewayVersion", v, "nextVersion", v+1)
			}
		}
	}
	if r.Log != nil {
		r.Log.Add(d.Records...)
		if err := r.Log.Err(); err != nil {
			fail(StageDecisionLog, err)
		}
	}
	if r.AfterDecision != nil {
		r.AfterDecision(ctx, d)
	}
	r.finish(obs, d, errs)
	return d
}

// observe reads the metrics and the pool, stamps the time and marks the
// observation not OK when either read fails.
func (r *Runner) observe(ctx context.Context, fail func(string, error)) model.Observation {
	obs, err := r.Source.Observe(ctx)
	if err != nil {
		obs = model.Observation{Error: err.Error()}
		fail(StageMetrics, err)
	} else {
		obs.OK, obs.Error = true, ""
	}
	obs.Time = r.now()
	pool, err := r.Actuator.Status(ctx)
	if err != nil {
		fail(StagePoolStatus, err)
		// Without the pool size the controller cannot scale safely.
		if obs.OK {
			obs.OK, obs.Error = false, "pool status unavailable: "+err.Error()
		}
		return obs
	}
	obs.Pool = pool
	r.poolKnown = true
	return obs
}

// waitForPool is the decision of a cycle before the pool status was ever
// read. The core is not stepped: it adopts the pool's size only in its
// first cycle, so a blind first cycle would make it start from MinReplicas
// and later cut a running pool. Nothing is scaled and there are no
// records; a restored last policy is republished.
func (r *Runner) waitForPool(obs model.Observation) control.Decision {
	s := r.Core.State()
	return control.Decision{
		Cycle: s.Cycle, Time: obs.Time, Policy: s.LastPolicy,
		Records: []model.DecisionRecord{}, FailStatic: true,
	}
}

// finish records the cycle in Status and the metrics, and logs it.
func (r *Runner) finish(obs model.Observation, d control.Decision, errs []stageError) {
	log := r.logger()
	for _, e := range errs {
		log.Error("controller cycle stage failed", "stage", e.stage, "cycle", d.Cycle, "err", e.err)
	}
	var version int64
	if d.Policy != nil {
		version = d.Policy.Version
	}
	log.Info("control cycle", "cycle", d.Cycle, "failStatic", d.FailStatic,
		"desiredReplicas", d.DesiredReplicas, "poolReplicas", d.PoolReplicas,
		"readyReplicas", obs.Pool.ReadyReplicas, "policyVersion", version)

	r.metrics.observe(d, errs)

	r.mu.Lock()
	defer r.mu.Unlock()
	r.status.Cycle = d.Cycle
	r.status.CycleSeconds = r.Cycle.Seconds()
	r.status.Time = obs.Time
	r.status.FailStatic = d.FailStatic
	r.status.Pool = obs.Pool
	r.status.Observation = &obs
	r.status.Decision = &d
	for _, e := range errs {
		r.status.Errors[e.stage] = ErrorRecord{Cycle: d.Cycle, Time: obs.Time, Error: e.err.Error()}
	}
}

// Status returns a snapshot of the last cycle.
func (r *Runner) Status() Status {
	r.once.Do(r.init)
	r.mu.Lock()
	defer r.mu.Unlock()
	s := r.status
	s.Errors = maps.Clone(r.status.Errors)
	return s
}

// Run calls RunOnce at once and then every Cycle until ctx is done, when it
// returns nil. Each cycle gets a deadline of max(Cycle, 1 s) so a hung
// dependency cannot stall the loop. It returns an error only when the
// Runner is misconfigured.
func (r *Runner) Run(ctx context.Context) error {
	if err := r.validate(); err != nil {
		return err
	}
	if r.Cycle <= 0 {
		return errors.New("controller runner: Cycle must be > 0")
	}
	ticker := time.NewTicker(r.Cycle)
	defer ticker.Stop()
	for ctx.Err() == nil {
		cctx, cancel := context.WithTimeout(ctx, max(r.Cycle, minCycleTimeout))
		r.RunOnce(cctx)
		cancel()
		select {
		case <-ctx.Done():
		case <-ticker.C:
		}
	}
	return nil
}
