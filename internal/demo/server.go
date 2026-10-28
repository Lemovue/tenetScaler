package demo

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"tenantscaler/internal/model"
	"tenantscaler/internal/sim"
	"tenantscaler/internal/stack"
)

var (
	// ErrBusy is returned by StartAB while an A/B run is in progress.
	ErrBusy = errors.New("demo: an A/B run is already in progress")
	// ErrNotRunning is returned by StopAB when no A/B run is in progress.
	ErrNotRunning = errors.New("demo: no A/B run is in progress")
	// ErrClosed is returned by StartAB once Close has been called.
	ErrClosed = errors.New("demo: the server is shutting down")
)

// Server runs the A/B comparison and serves the dashboard (see Handler).
// It is safe for concurrent use. Create it with New and always Close it:
// Close stops a running stack, which kills real replicas.
type Server struct {
	opt      Options
	roles    sim.Roles
	scenario ScenarioInfo

	mu        sync.Mutex
	run       *abRun // current or last A/B run; nil before the first
	runs      int    // A/B runs started
	closed    bool
	observing *ObserveTarget // set by StartObserve: this dashboard only watches a cluster

	dsMu   sync.Mutex               // serializes reading trace files
	loaded map[string]loadedDataset // fitted trace datasets by key
}

// New validates opt and returns an idle server.
func New(opt Options) (*Server, error) {
	o, err := opt.normalize()
	if err != nil {
		return nil, err
	}
	roles := sim.ScenarioRoles(o.Scenario)
	return &Server{opt: o, roles: roles, scenario: describeScenario(o.Scenario, roles, o.Config)}, nil
}

// StartAB begins an A/B run with seed on the synthetic dataset in the
// background and returns its number. It returns ErrBusy while another run
// is in progress and ErrClosed after Close. A dashboard that watches a
// cluster (StartObserve) never starts a stack of its own: StartAB starts
// watching again instead.
func (s *Server) StartAB(seed int64) (int, error) {
	return s.StartABWith(seed, DatasetSynthetic)
}

// StartABWith is StartAB with the request sizes taken from a dataset (see
// Datasets); it returns ErrUnknownDataset for one that is unknown or whose
// file is missing.
func (s *Server) StartABWith(seed int64, dataset string) (int, error) {
	s.mu.Lock()
	target := s.observing
	s.mu.Unlock()
	if target != nil {
		return s.StartObserve(*target)
	}
	if !s.datasetAvailable(dataset) {
		return 0, fmt.Errorf("%w: %q", ErrUnknownDataset, dataset)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return 0, ErrClosed
	}
	if s.run != nil && !s.run.finished() {
		return 0, ErrBusy
	}
	s.runs++
	ctx, cancel := context.WithCancel(context.Background())
	s.run = newRun(s.runs, seed, &s.opt, s.roles, cancel)
	s.run.dataset, s.run.loadDataset = dataset, s.datasetSamples
	go s.run.execute(ctx)
	return s.runs, nil
}

// StopAB cancels the running A/B run and waits until its stack has
// stopped (real replicas killed) or ctx ends. It returns ErrNotRunning
// when no run is in progress.
func (s *Server) StopAB(ctx context.Context) error {
	s.mu.Lock()
	run := s.run
	s.mu.Unlock()
	if run == nil || run.finished() {
		return ErrNotRunning
	}
	run.setMessage("Stopping: shutting the stack down")
	run.cancel()
	select {
	case <-run.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Close refuses new A/B runs, stops a running one and waits for its stack
// to stop, bounded by ctx.
func (s *Server) Close(ctx context.Context) error {
	s.mu.Lock()
	s.closed = true
	s.mu.Unlock()
	if err := s.StopAB(ctx); err != nil && !errors.Is(err, ErrNotRunning) {
		return err
	}
	return nil
}

// State returns the dashboard state: phase, progress, and per stack its
// live samples and, once finished, its metrics.
func (s *Server) State() State {
	st := State{Phase: PhaseIdle, Message: "Ready: press Run A/B to replay the same traffic on the baseline, then on TenantScaler",
		Seed: s.opt.Seed, Backend: s.opt.Backend, Scenario: s.scenario, WindowSeconds: s.opt.Window.Seconds()}
	s.mu.Lock()
	run := s.run
	st.Run = s.runs
	s.mu.Unlock()
	if run == nil {
		for _, p := range abStacks {
			st.Stacks = append(st.Stacks, s.stackState(&stackRun{plan: p, status: StatusPending}))
		}
		return st
	}
	run.mu.Lock()
	defer run.mu.Unlock()
	started := run.startedAt
	st.Phase, st.Message, st.Error, st.Seed, st.StartedAt = run.phase, run.message, run.err, run.seed, &started
	st.Dataset = run.datasetUse
	if run.liveMode != nil || run.observe != nil {
		st.Live = true
		if run.liveMode != nil {
			st.LiveAddress = run.liveMode.public
		}
		for _, sr := range run.stacks {
			st.Stacks = append(st.Stacks, s.stackState(sr))
		}
		return st // no trace, so no progress
	}
	dur := s.opt.Scenario.Duration
	for _, sr := range run.stacks {
		st.Stacks = append(st.Stacks, s.stackState(sr))
		switch {
		case sr.status == StatusDone:
			st.Progress++
		case dur > 0:
			st.Progress += min(float64(sr.elapsed)/float64(dur), 1)
		}
	}
	st.Progress /= float64(len(run.stacks))
	return st
}

// stackState converts a stack's progress (read under its run's lock).
func (s *Server) stackState(sr *stackRun) StackState {
	gw, ctl, _ := stack.Describe(sr.plan.setup)
	ss := StackState{
		Key: sr.plan.key, Setup: sr.plan.setup, Title: sr.plan.title, Gateway: gw, ControlPlane: ctl,
		Status: sr.status, Error: sr.err, GatewayURL: sr.gatewayURL, Engine: sr.engine, Simulated: sr.simulated || s.opt.Backend.Simulated,
		ElapsedSeconds: seconds(sr.elapsed), Requests: sr.requests,
		Samples: sr.samples[:len(sr.samples):len(sr.samples)], Metrics: sr.metrics, Tenants: sr.tenants,
		DrainTimedOut: sr.drainTimedOut,
	}
	if ss.Samples == nil {
		ss.Samples = []LiveSample{}
	}
	if !sr.trafficStart.IsZero() {
		t := sr.trafficStart
		ss.TrafficStartedAt = &t
	}
	return ss
}

// DecisionsReply is what GET /api/decisions serves.
type DecisionsReply struct {
	// Setup is the TenantScaler setup whose controller made the decisions.
	Setup string `json:"setup"`
	// Live is true while that stack runs.
	Live bool `json:"live"`
	// TrafficStartedAt is when that stack's replay began (nil before it
	// ran), so records can be shown relative to the traffic.
	TrafficStartedAt *time.Time `json:"trafficStartedAt,omitempty"`
	// Total is the number of records kept, all tenants included.
	Total int `json:"total"`
	// Records are the matching records, newest first.
	Records []model.DecisionRecord `json:"records"`
}

// Decisions returns up to limit (<= 0: all) decision records of the
// TenantScaler controller, newest first, only tenant's when tenant is not
// empty: live while the TenantScaler stack runs, then those of its last
// run.
func (s *Server) Decisions(limit int, tenant string) DecisionsReply {
	reply := DecisionsReply{Setup: sim.SetupFull, Records: []model.DecisionRecord{}}
	s.mu.Lock()
	run := s.run
	s.mu.Unlock()
	if run == nil {
		return reply
	}
	run.mu.Lock()
	live, recs, start, observed := run.live, run.decisions, run.decStart, run.observe != nil
	run.mu.Unlock()
	if live != nil {
		recs, reply.Live = live.Decisions(0), true
	}
	if observed { // the observed controller's log, read every few seconds
		reply.Live = !run.finished()
	}
	if !start.IsZero() {
		reply.TrafficStartedAt = &start
	}
	reply.Total = len(recs)
	for _, r := range recs {
		if limit > 0 && len(reply.Records) >= limit {
			break
		}
		if tenant == "" || r.Tenant == tenant {
			reply.Records = append(reply.Records, r)
		}
	}
	return reply
}
