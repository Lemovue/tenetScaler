// Package sim is TenantScaler's discrete-event simulator: it replays a
// workload scenario in VIRTUAL time against one evaluation setup and
// records what happened to every request, so many setups, seeds and cold
// starts can be compared in seconds instead of hours.
//
// It drives the real decision code: the gateway scheduler is
// internal/fairq, the TenantScaler control plane is internal/control.Core,
// the baselines are internal/autoscale's HPA and KEDA, arrivals come from
// workload.Generate and latencies from the internal/engine latency model.
// Only the replicas' compute is SIMULATED (illustrative engine parameters,
// not measured on hardware), so every number this package produces must be
// labelled simulated.
//
// Model (DESIGN section 14):
//   - Replicas start "pending" (no free GPU slot while others drain),
//     "starting" for the cold start, then "ready" with MaxNumSeqs slots and
//     an FCFS waiting queue; on scale-down pending replicas go first, then
//     starting ones, then idle ready ones, then busy ready ones, which drain
//     (finish their accepted requests) before releasing their slot. The
//     pool never holds more than CapacityReplicas starting, ready and
//     draining replicas.
//   - Service: prefill = Jitter(PrefillTime(in, running)), then out-1 tokens
//     at Jitter(TokenInterval(running)), both with the running count at the
//     request's start (load fixed at start: a documented approximation).
//   - Gateways (DESIGN section 13): passthrough (round robin, no shedding),
//     fair / roundrobin (fairq + a dispatch window of SlotsPerReplica per
//     replica, least-loaded replica first), partition (per-tenant FIFO on
//     the tenant's own replicas). A request still waiting at the gateway
//     DispatchTimeout after it arrived fails (the live gateway's 503). In
//     fair mode the output tokens a request has streamed are charged to its
//     tenant's counter as they are produced, as the live gateway does.
//   - Control every Controller.CycleSeconds on an observation built like
//     the live gateway's /stats over the last cycle; policies reach the
//     gateway after PolicyDelay.
//
// The simulation is single-threaded and deterministic: the same Options
// always give the same RunData.
package sim

import (
	"container/heap"
	"fmt"
	"math/rand"
	"time"

	"tenantscaler/internal/autoscale"
	"tenantscaler/internal/config"
	"tenantscaler/internal/control"
	"tenantscaler/internal/engine"
	"tenantscaler/internal/eval"
	"tenantscaler/internal/fairq"
	"tenantscaler/internal/model"
	"tenantscaler/internal/workload"
)

// Options configures one simulated run.
type Options struct {
	// Setup is one of AllSetups.
	Setup string
	// Config supplies the pool budget, tenants (SLOs, guarantees), control
	// cycle, controller tuning, dispatch window and engine parameters.
	Config config.Config
	// Scenario is the workload; every scenario tenant must be in Config.
	Scenario workload.Scenario
	// Seed drives the trace, the latency jitter and the scheduler.
	Seed int64
	// ColdStart is the replica load time; zero means the engine's
	// StartupDuration.
	ColdStart time.Duration
	// InitialReplicas is the warm pool at t=0 (zero means
	// DefaultInitialReplicas); the static partition always starts, and
	// stays, at capacity.
	InitialReplicas int
	// SampleEvery is the timeline resolution (zero means 1 s).
	SampleEvery time.Duration
	// PolicyDelay is the controller-to-gateway propagation delay (zero
	// means DefaultPolicyDelay).
	PolicyDelay time.Duration
	// DispatchTimeout is the longest a request waits at the gateway for a
	// replica slot before it fails (zero means DefaultDispatchTimeout).
	DispatchTimeout time.Duration
}

// Diagnostics are simulator-side facts about a run that are not part of
// eval.RunData.
type Diagnostics struct {
	// Events counts processed events; ControlCycles counts control ticks.
	Events, ControlCycles int
	// PeakPhysicalReplicas is the most replicas holding a GPU slot
	// (starting, ready or draining) at any instant.
	PeakPhysicalReplicas int
	// ForecastRaisedCycles counts, per tenant, control cycles in which the
	// forecast raised the tenant's demand above the reactive estimate.
	ForecastRaisedCycles map[string]int
	// End is the virtual time the run stopped at.
	End time.Duration
}

// Result is a run's data plus diagnostics.
type Result struct {
	Data        eval.RunData
	Diagnostics Diagnostics
	Settings    Settings
}

// Run simulates one run and returns its data for eval.Compute. RunData.Run
// is left 0 for the caller to set.
func Run(o Options) (eval.RunData, error) {
	r, err := Simulate(o)
	return r.Data, err
}

// Simulate is Run plus diagnostics and the effective settings.
func Simulate(o Options) (Result, error) {
	s, err := newSimulator(o)
	if err != nil {
		return Result{}, err
	}
	s.run()
	return Result{Data: s.runData(), Diagnostics: s.diag, Settings: s.st}, nil
}

// epoch is the virtual wall-clock time of the run start.
var epoch = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

// simulator is the state of one run.
type simulator struct {
	o      Options
	st     Settings
	spec   setupSpec
	cfg    config.Config // with the setup's controller settings
	params engine.Params
	roles  Roles
	rng    *rand.Rand // latency jitter

	now    time.Duration
	end    time.Duration // hard stop: Duration + DrainLimit
	events eventHeap
	seq    uint64

	reqs     []*request
	tenants  []*tenantState // config order
	byName   map[string]*tenantState
	resolved int // requests shed or completed

	// pool
	reps    []*replica // live replicas (not gone), by id
	history []*replica // every replica ever created (tests, diagnostics)
	nextID  int
	target  int // replicas the control plane wants (pending+starting+ready)

	// gateway
	sched     *fairq.Scheduler            // fair / roundrobin
	partSched map[string]*fairq.Scheduler // partition
	pending   []*request                  // passthrough: no ready replica yet
	rrNext    int                         // passthrough round-robin cursor
	charging  []*request                  // fair mode: dispatched, not finished

	// control
	hpa       *autoscale.HPA
	keda      *autoscale.KEDA
	core      *control.Core
	lastTick  time.Duration
	occSum    float64 // slot occupancy samples since the last tick (HPA)
	occN      int
	noisyShed bool // the applied policy sheds the noisy tenant

	// recording
	timeline  []eval.Sample
	actions   []time.Duration
	policyLat []time.Duration
	diag      Diagnostics
}

func newSimulator(o Options) (*simulator, error) {
	st, err := Resolve(o)
	if err != nil {
		return nil, err
	}
	s := &simulator{
		o: o, st: st, spec: setupSpecs[o.Setup], cfg: o.Config,
		roles:  ScenarioRoles(o.Scenario),
		rng:    rand.New(rand.NewSource(o.Seed ^ 0x5bd1e995)),
		end:    o.Scenario.Duration + DrainLimit,
		byName: map[string]*tenantState{},
		diag:   Diagnostics{ForecastRaisedCycles: map[string]int{}},
	}
	s.cfg.Controller = st.Controller
	if s.params, err = o.Config.Engine.Params(); err != nil {
		return nil, err
	}
	for _, t := range o.Config.Tenants {
		ts := &tenantState{name: t.Name, slo: t.SLO}
		s.tenants = append(s.tenants, ts)
		s.byName[t.Name] = ts
	}
	for i, sp := range workload.Generate(o.Scenario, o.Seed) {
		s.reqs = append(s.reqs, &request{id: i, spec: sp, ts: s.byName[sp.Tenant]})
	}
	if err := s.initControl(); err != nil {
		return nil, err
	}
	s.initGateway()
	for range st.InitialReplicas {
		r := s.newReplica()
		r.state = repReady // warm at t=0
	}
	s.target = st.InitialReplicas
	s.notePhysical()
	s.endpointsChanged()
	return s, nil
}

// run processes events until none are left or the drain limit is reached.
func (s *simulator) run() {
	if len(s.reqs) > 0 {
		s.schedule(s.reqs[0].spec.Offset, &event{kind: evArrival, req: s.reqs[0]})
	}
	if s.spec.scaler != scalerNone {
		s.schedule(s.cycle(), &event{kind: evTick})
	}
	s.schedule(0, &event{kind: evSample})
	for s.events.Len() > 0 {
		ev := heap.Pop(&s.events).(*event)
		if ev.at > s.end {
			break
		}
		s.now = ev.at
		s.diag.Events++
		switch ev.kind {
		case evArrival:
			if next := ev.req.id + 1; next < len(s.reqs) {
				s.schedule(s.reqs[next].spec.Offset, &event{kind: evArrival, req: s.reqs[next]})
			}
			s.arrive(ev.req)
		case evReady:
			s.onReady(ev.rep)
		case evDone:
			s.onDone(ev.req)
		case evTick:
			s.tick()
			if s.keepGoing(s.now + s.cycle()) {
				s.schedule(s.now+s.cycle(), ev)
			}
		case evPolicy:
			s.applyPolicy(ev.pol)
		case evSample:
			s.sample()
			if s.keepGoing(s.now + s.st.SampleEvery) {
				s.schedule(s.now+s.st.SampleEvery, ev)
			}
		case evTimeout:
			s.onTimeout(ev.req)
		}
	}
	s.diag.End = s.now
}

// keepGoing reports whether a periodic event at time next is still useful:
// before the scenario ends, or while requests remain unresolved, and never
// past the drain limit.
func (s *simulator) keepGoing(next time.Duration) bool {
	if next > s.end {
		return false
	}
	return next < s.o.Scenario.Duration || s.resolved < len(s.reqs)
}

// cycle is the control period.
func (s *simulator) cycle() time.Duration {
	return time.Duration(s.cfg.Controller.CycleSeconds * float64(time.Second))
}

// clock maps virtual time to the wall-clock time the decision code sees.
func (s *simulator) clock() time.Time { return epoch.Add(s.now) }

// offset maps a wall-clock time back to virtual time.
func offset(t time.Time) time.Duration { return t.Sub(epoch) }

// runData assembles the recorded run.
func (s *simulator) runData() eval.RunData {
	d := eval.RunData{
		Setup: s.o.Setup, Seed: s.o.Seed,
		Records:         make([]eval.Record, len(s.reqs)),
		Timeline:        s.timeline,
		SLOs:            map[string]model.SLO{},
		GuaranteedSlots: map[string]int{},
		Victims:         s.roles.Victims, Noisy: s.roles.Noisy,
		SpikeStart: s.roles.SpikeStart, SpikeEnd: s.roles.SpikeEnd,
		ActionTimes: s.actions, PolicyLatencies: s.policyLat,
		Owner: s.roles.Owner, OwnerReturn: s.roles.OwnerReturn,
	}
	for _, t := range s.cfg.Tenants {
		d.SLOs[t.Name] = t.SLO
		d.GuaranteedSlots[t.Name] = t.GuaranteedReplicas * s.st.SlotsPerReplica
	}
	for i, r := range s.reqs {
		d.Records[i] = r.record()
	}
	return d
}

// String names the run for error messages.
func (o Options) String() string {
	return fmt.Sprintf("%s seed %d cold start %v", o.Setup, o.Seed, o.ColdStart)
}
