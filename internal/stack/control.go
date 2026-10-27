package stack

import (
	"context"
	"fmt"
	"math/rand/v2"
	"sync"
	"time"

	"tenantscaler/internal/autoscale"
	"tenantscaler/internal/control"
	"tenantscaler/internal/controller"
	"tenantscaler/internal/gateway"
	"tenantscaler/internal/model"
	"tenantscaler/internal/policy"
	"tenantscaler/internal/pool"
	"tenantscaler/internal/scalerun"
)

// phaseStream separates the control phase's random stream from the other
// uses of Options.Seed.
const phaseStream = 0x7068617365

// controlPhase is the seeded delay in [0, cycle), in whole milliseconds,
// of a control loop's first sync.
func controlPhase(seed int64, cycle time.Duration) time.Duration {
	ms := int64(cycle / time.Millisecond)
	if ms < 1 {
		return 0
	}
	rng := rand.New(rand.NewPCG(uint64(seed), phaseStream))
	return time.Duration(rng.Int64N(ms)) * time.Millisecond
}

// startControl starts the setup's control plane, if any, on ctx, with its
// first sync delayed by the seeded control phase. Every control plane
// uses the config's control cycle, as in the simulator.
func (s *Stack) startControl(ctx context.Context, o Options, initial int) error {
	cycle := time.Duration(s.cfg.Controller.CycleSeconds * float64(time.Second))
	var run func(context.Context) error
	switch s.spec.control {
	case controlNone:
		return nil
	case controlKEDATotal, controlKEDAGateway:
		kc := autoscale.DefaultKEDAConfig(s.cfg.Pool.MinReplicas, s.cfg.Pool.CapacityReplicas)
		if err := kc.Validate(); err != nil {
			return err
		}
		r := &scalerun.Runner{
			Algo: scalerun.AlgoKEDA, KEDA: autoscale.NewKEDA(kc, initial),
			Pool:            s.kedaPool(),
			GatewayStatsURL: s.url + "/stats", Cycle: cycle, Logger: s.log,
		}
		run = r.Run
	case controlCore:
		r, err := s.newRunner(o, cycle)
		if err != nil {
			return err
		}
		run = r.Run
	}
	s.phase, s.control = controlPhase(o.Seed, cycle), true
	s.loops.Add(1)
	go func() {
		defer s.loops.Done()
		t := time.NewTimer(s.phase)
		defer t.Stop()
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		if err := run(ctx); err != nil {
			s.log.Error("control loop stopped", "err", err)
		}
	}()
	return nil
}

// newRunner builds the TenantScaler controller for a controlCore setup:
// gateway /stats over one cycle as the metrics source, the in-process pool
// as the actuator and, unless coupling is disabled, POST /policy to the
// gateway. The forecaster stays off, as in the simulator's S5-S7.
func (s *Stack) newRunner(o Options, cycle time.Duration) (*controller.Runner, error) {
	cfg := s.cfg
	cfg.Controller.Forecast.Enabled = false
	s.spec.adjust(&cfg.Controller)
	params, err := cfg.Engine.Params()
	if err != nil {
		return nil, err
	}
	core, err := control.NewCore(cfg, params.StartupDuration)
	if err != nil {
		return nil, err
	}
	decisions, err := controller.NewDecisionLog(0, o.DecisionsFile)
	if err != nil {
		return nil, fmt.Errorf("decision log: %w", err)
	}
	s.decisions = decisions
	r := &controller.Runner{
		Core:     core,
		Source:   controller.GatewaySource{URLs: []string{s.url}, Window: cycle},
		Actuator: &poolActuator{p: s.pool, rec: s.rec},
		Log:      decisions, Cycle: cycle, Logger: s.log,
	}
	if !cfg.Controller.DisableGatewayCoupling {
		r.Publisher = &recordingPublisher{
			inner: &controller.HTTPPublisher{URLs: []string{s.url}}, gw: s.gw, rec: s.rec,
		}
	}
	return r, nil
}

// recorder keeps the protective actions and policy propagation latencies
// of a run. It is safe for concurrent use.
type recorder struct {
	mu        sync.Mutex
	actions   []time.Time
	latencies []time.Duration
	shed      map[string]bool // tenants shed by the last applied policy
}

func newRecorder() *recorder { return &recorder{shed: map[string]bool{}} }

// scaledUp records a scale-up of the pool's desired replicas.
func (r *recorder) scaledUp(at time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.actions = append(r.actions, at)
}

// policyApplied records an applied policy's propagation latency and, when
// it newly sheds a tenant, a protective action.
func (r *recorder) policyApplied(at time.Time, p policy.Policy, latency time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.latencies = append(r.latencies, latency)
	newlyShed := false
	shed := make(map[string]bool, len(p.Tenants))
	for t, tp := range p.Tenants {
		if tp.Shed {
			shed[t] = true
			newlyShed = newlyShed || !r.shed[t]
		}
	}
	r.shed = shed
	if newlyShed {
		r.actions = append(r.actions, at)
	}
}

// ActionTimes returns the protective actions taken at or after start, as
// offsets from start: every increase of the pool's desired replicas, and
// every applied policy that newly sheds a tenant.
func (s *Stack) ActionTimes(start time.Time) []time.Duration {
	s.rec.mu.Lock()
	defer s.rec.mu.Unlock()
	var out []time.Duration
	for _, t := range s.rec.actions {
		if !t.Before(start) {
			out = append(out, t.Sub(start))
		}
	}
	return out
}

// PolicyLatencies returns the propagation latency of every policy the
// gateway applied (its receive time minus the policy's IssuedAt), in
// order.
func (s *Stack) PolicyLatencies() []time.Duration {
	s.rec.mu.Lock()
	defer s.rec.mu.Unlock()
	return append([]time.Duration(nil), s.rec.latencies...)
}

// scaleUp scales p to n and records a scale-up when the desired count
// grew.
func scaleUp(p *pool.Pool, rec *recorder, n int) error {
	before := p.Status().Desired
	if err := p.Scale(n); err != nil {
		return err
	}
	if p.Status().Desired > before {
		rec.scaledUp(time.Now())
	}
	return nil
}

// poolActuator is the controller's actuator over the in-process pool.
type poolActuator struct {
	p   *pool.Pool
	rec *recorder
}

// SetReplicas scales the pool.
func (a *poolActuator) SetReplicas(_ context.Context, n int) error { return scaleUp(a.p, a.rec, n) }

// Status reports the pool's ready, starting and desired replicas.
func (a *poolActuator) Status(context.Context) (model.PoolObservation, error) {
	return a.p.Status().Observation(), nil
}

// kedaPool is the KEDA baseline's view of the pool: S4 (controlKEDAGateway)
// scales on the gateway queue alone, S2 also on the replicas' waiting
// requests.
func (s *Stack) kedaPool() *scalePool {
	return &scalePool{p: s.pool, rec: s.rec, gatewayQueueOnly: s.spec.control == controlKEDAGateway}
}

// scalePool is the baseline autoscaler's view of the in-process pool. With
// gatewayQueueOnly it reports no requests waiting inside the replicas, so
// KEDA's queue metric is the gateway queue alone (setup S4).
type scalePool struct {
	p                *pool.Pool
	rec              *recorder
	gatewayQueueOnly bool
}

// Scale resizes the pool.
func (a *scalePool) Scale(_ context.Context, n int) error { return scaleUp(a.p, a.rec, n) }

// Status returns the pool status.
func (a *scalePool) Status(context.Context) (pool.Status, error) {
	st := a.p.Status()
	if a.gatewayQueueOnly {
		for i := range st.Replicas {
			st.Replicas[i].Waiting = 0
		}
	}
	return st, nil
}

// recordingPublisher publishes through inner and, once the in-process
// gateway runs the published version, records its propagation latency as
// the gateway measured it.
type recordingPublisher struct {
	inner controller.Publisher
	gw    *gateway.Gateway
	rec   *recorder
}

// Publish delivers p and records it when the gateway applied it.
func (r *recordingPublisher) Publish(ctx context.Context, p policy.Policy) error {
	err := r.inner.Publish(ctx, p)
	st := r.gw.Stats(time.Second)
	if st.PolicyVersion == p.Version {
		r.rec.policyApplied(time.Now(), p, time.Duration(st.PolicyLatencyMs*float64(time.Millisecond)))
	}
	return err
}
