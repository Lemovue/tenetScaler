// Package pool is the local stand-in for the model Deployment. It hosts
// replicas through a Backend: SIMULATED in-process engines by default, or
// real llama-server processes (package llama). It scales them the way a
// Deployment would: new replicas cold-start, removed replicas drain before
// they are shut down, and replicas that stop on their own are replaced.
package pool

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sort"
	"sync"
	"time"

	"tenantscaler/internal/engine"
	"tenantscaler/internal/model"
	"tenantscaler/internal/replica"
)

// ErrClosed is returned by Scale after Close.
var ErrClosed = errors.New("pool is closed")

// Config configures a Pool.
type Config struct {
	// Name prefixes replica IDs ("<name>-1", "<name>-2", ...).
	Name string
	// Params are the simulated engine parameters of every replica when
	// Backend is nil.
	Params engine.Params
	// MaxReplicas caps the desired replica count (the GPU budget). It is
	// also the number of CPU slots handed to the Backend.
	MaxReplicas int
	// Seed derives each simulated replica's jitter seed (Seed + replica
	// number) when Backend is nil.
	Seed int64
	// Backend launches the replicas; nil means NewSimulatedBackend(Params,
	// Seed).
	Backend Backend
	// Logger receives lifecycle warnings; nil discards them.
	Logger *slog.Logger
	// ReconcileInterval is how often the pool, between Scale calls,
	// replaces replicas that stopped on their own and retries failed
	// launches; 0 means DefaultReconcileInterval. While rounds keep having
	// to repair something the wait doubles, up to maxReconcileBackoff.
	ReconcileInterval time.Duration
}

// ReplicaStatus describes one hosted replica.
type ReplicaStatus struct {
	ID      string `json:"id"`
	URL     string `json:"url"`
	State   string `json:"state"`
	Running int    `json:"running"`
	Waiting int    `json:"waiting"`
	// Slot is the CPU slot the replica was launched on.
	Slot int `json:"slot"`
	// ColdStartSeconds is the time from launch to ready (0 while
	// starting); measured for real replicas, configured for simulated ones.
	ColdStartSeconds float64 `json:"coldStartSeconds"`
}

// Status is a snapshot of the pool. Replicas are listed oldest first.
type Status struct {
	Desired  int             `json:"desired"`
	Ready    int             `json:"ready"`
	Starting int             `json:"starting"`
	Draining int             `json:"draining"`
	Replicas []ReplicaStatus `json:"replicas"`
	// Backend is the backend name: "simulated" or "llama".
	Backend string `json:"backend"`
	// Simulated is true when the replicas' compute is simulated.
	Simulated bool `json:"simulated"`
}

// Observation converts the status to the controller's pool observation.
func (s Status) Observation() model.PoolObservation {
	return model.PoolObservation{
		ReadyReplicas:    s.Ready,
		StartingReplicas: s.Starting,
		DesiredReplicas:  s.Desired,
	}
}

// Pool hosts replicas launched by its Backend. It is safe for concurrent
// use.
type Pool struct {
	cfg     Config
	backend Backend
	log     *slog.Logger
	ctx     context.Context // canceled by Close
	cancel  context.CancelFunc
	wg      sync.WaitGroup // reconcile, retire and reap goroutines

	mu      sync.Mutex
	desired int
	seq     int
	members []*member // creation order, oldest first
	closed  bool
}

// New creates an empty pool and starts its reconcile loop. Call Scale to
// start replicas and Close to stop them. An empty Name defaults to
// "replica".
func New(cfg Config) *Pool {
	if cfg.Name == "" {
		cfg.Name = "replica"
	}
	backend := cfg.Backend
	if backend == nil {
		backend = NewSimulatedBackend(cfg.Params, cfg.Seed)
	}
	logger := cfg.Logger
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	interval := cfg.ReconcileInterval
	if interval <= 0 {
		interval = DefaultReconcileInterval
	}
	ctx, cancel := context.WithCancel(context.Background())
	p := &Pool{cfg: cfg, backend: backend, log: logger, ctx: ctx, cancel: cancel}
	p.wg.Add(1)
	go p.reconcileLoop(interval)
	return p
}

// Scale sets the desired replica count, clamped to [0, MaxReplicas].
// Missing replicas are launched in state "starting" (a simulated replica
// turns ready after the engine's StartupDuration, a real one once its
// health check passes). Replicas that stopped on their own are removed and
// replaced; the reconcile loop does the same between Scale calls. Surplus
// replicas are drained, starting ones first and then the newest ready
// ones, and shut down once their in-flight requests have finished.
func (p *Pool) Scale(n int) error {
	if p.cfg.Backend == nil {
		if err := p.cfg.Params.Validate(); err != nil {
			return fmt.Errorf("pool %s: %w", p.cfg.Name, err)
		}
	}
	n = min(max(n, 0), max(p.cfg.MaxReplicas, 0))
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return ErrClosed
	}
	p.desired = n
	p.reapStoppedLocked()
	if err := p.fillLocked(); err != nil {
		return err
	}
	p.trimLocked()
	return nil
}

// Status returns a snapshot of the desired count and every hosted replica,
// including draining ones that have not shut down yet. The replicas' loads
// are read in parallel, so a slow /metrics answer of one real replica does
// not add up across the pool.
func (p *Pool) Status() Status {
	p.mu.Lock()
	members := append([]*member(nil), p.members...)
	st := Status{
		Desired:   p.desired,
		Replicas:  make([]ReplicaStatus, 0, len(members)),
		Backend:   p.backend.Name(),
		Simulated: p.backend.Simulated(),
	}
	p.mu.Unlock()
	loads := make([]replica.Stats, len(members))
	var reading sync.WaitGroup
	for i, m := range members {
		reading.Add(1)
		go func() {
			defer reading.Done()
			loads[i] = m.inst.Stats()
		}()
	}
	reading.Wait()
	for i, m := range members {
		state := m.inst.State()
		switch state {
		case replica.StateReady:
			st.Ready++
		case replica.StateStarting:
			st.Starting++
		case replica.StateDraining:
			st.Draining++
		}
		rs := ReplicaStatus{
			ID: m.inst.ID(), URL: m.inst.URL(), State: state,
			Running: loads[i].Running, Waiting: loads[i].Waiting, Slot: m.slot,
		}
		if cs, ok := m.inst.(ColdStarter); ok {
			rs.ColdStartSeconds = cs.ColdStart().Seconds()
		}
		st.Replicas = append(st.Replicas, rs)
	}
	return st
}

// Endpoints returns the base URLs of ready replicas, sorted.
func (p *Pool) Endpoints() []string {
	p.mu.Lock()
	members := append([]*member(nil), p.members...)
	p.mu.Unlock()
	urls := []string{}
	for _, m := range members {
		if m.inst.State() == replica.StateReady {
			urls = append(urls, m.inst.URL())
		}
	}
	sort.Strings(urls)
	return urls
}

// Close stops every replica immediately and in parallel: in-flight
// requests abort and real processes are killed. It waits until every
// instance is closed and all pool goroutines have exited. Calling Close
// again returns nil.
func (p *Pool) Close() error {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil
	}
	p.closed = true
	members := p.members
	p.members = nil
	p.mu.Unlock()

	p.cancel()
	errs := make([]error, len(members))
	var closing sync.WaitGroup
	for i, m := range members {
		closing.Add(1)
		go func() {
			defer closing.Done()
			errs[i] = m.inst.Close()
		}()
	}
	closing.Wait()
	p.wg.Wait()
	return errors.Join(errs...)
}
