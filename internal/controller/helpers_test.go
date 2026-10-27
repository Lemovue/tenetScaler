package controller

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"tenantscaler/internal/config"
	"tenantscaler/internal/control"
	"tenantscaler/internal/model"
	"tenantscaler/internal/policy"
)

// t0 is the fixed start of the fake clock.
var t0 = time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)

// fakeClock advances by step on every call.
type fakeClock struct {
	mu   sync.Mutex
	now  time.Time
	step time.Duration
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(c.step)
	return c.now
}

// fakeSource returns obs, or err when set.
type fakeSource struct {
	mu    sync.Mutex
	obs   model.Observation
	err   error
	calls int
	// onObserve, when set, runs on every call with the call number.
	onObserve func(n int)
}

func (s *fakeSource) Observe(context.Context) (model.Observation, error) {
	s.mu.Lock()
	s.calls++
	n, obs, err, hook := s.calls, s.obs, s.err, s.onObserve
	s.mu.Unlock()
	if hook != nil {
		hook(n)
	}
	return obs, err
}

func (s *fakeSource) set(obs model.Observation, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.obs, s.err = obs, err
}

// fakeActuator records SetReplicas calls and reports pool.
type fakeActuator struct {
	mu        sync.Mutex
	pool      model.PoolObservation
	statusErr error
	setErr    error
	sets      []int
}

func (a *fakeActuator) SetReplicas(_ context.Context, n int) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.sets = append(a.sets, n)
	if a.setErr == nil {
		a.pool.DesiredReplicas = n
	}
	return a.setErr
}

func (a *fakeActuator) Status(context.Context) (model.PoolObservation, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.pool, a.statusErr
}

func (a *fakeActuator) setCalls() []int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]int(nil), a.sets...)
}

// fakePublisher records the policies it was given.
type fakePublisher struct {
	mu       sync.Mutex
	err      error
	policies []policy.Policy
}

func (p *fakePublisher) Publish(_ context.Context, pol policy.Policy) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.policies = append(p.policies, pol)
	return p.err
}

func (p *fakePublisher) published() []policy.Policy {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]policy.Policy(nil), p.policies...)
}

// testConfig is the default three-tenant config (A, B, C on 10 replicas,
// C = 400 weighted tokens/s per replica, headroom 0.2).
func testConfig(t *testing.T) config.Config {
	t.Helper()
	cfg := config.Default()
	if err := cfg.Validate(); err != nil {
		t.Fatalf("default config: %v", err)
	}
	return cfg
}

// loadObs is an observation where tenant A needs 6 replicas and B and C 1.
func loadObs() model.Observation {
	return model.Observation{OK: true, Tenants: []model.TenantObservation{
		{Tenant: "A", ArrivalWeightedTokensPerSec: 2000, HasSLOData: true, SLOAttainment: 1, P99TTFTSeconds: 0.5, Completed: 50},
		{Tenant: "B", ArrivalWeightedTokensPerSec: 100},
		{Tenant: "C", ArrivalWeightedTokensPerSec: 100},
	}}
}

// testRig is a Runner wired to fakes.
type testRig struct {
	r   *Runner
	src *fakeSource
	act *fakeActuator
	pub *fakePublisher
	log *DecisionLog
}

func newRig(t *testing.T, cfg config.Config) *testRig {
	t.Helper()
	core, err := control.NewCore(cfg, 0)
	if err != nil {
		t.Fatalf("NewCore: %v", err)
	}
	log, err := NewDecisionLog(0, "")
	if err != nil {
		t.Fatalf("NewDecisionLog: %v", err)
	}
	rig := &testRig{
		src: &fakeSource{obs: loadObs()},
		act: &fakeActuator{pool: model.PoolObservation{ReadyReplicas: 3, DesiredReplicas: 3}},
		pub: &fakePublisher{},
		log: log,
	}
	clock := &fakeClock{now: t0, step: 15 * time.Second}
	rig.r = &Runner{
		Core: core, Source: rig.src, Actuator: rig.act, Publisher: rig.pub, Log: log,
		Cycle: 15 * time.Second, Clock: clock.Now, Logger: discardLogger(),
	}
	return rig
}

func discardLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

var errBoom = errors.New("boom")
