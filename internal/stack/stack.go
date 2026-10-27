// Package stack assembles ONE live TenantScaler or baseline stack for an
// evaluation setup: a replica pool (SIMULATED in-process engines or REAL
// llama-server processes, chosen by the config's backend), a gateway
// served over HTTP on 127.0.0.1, and the setup's control plane (the
// TenantScaler controller, a KEDA-style baseline autoscaler, or none). It
// is shared by the real-replica evaluation (internal/realrun) and the demo.
//
// Setups (names shared with internal/sim):
//
//	S2 KEDA queue   passthrough gateway   scalerun KEDA on replica waiting + gateway queue
//	S3 static       partition gateway     none, pool at capacity
//	S4 Flow-Control roundrobin gateway    scalerun KEDA on the gateway queue
//	S5 no gateway   passthrough gateway   control.Core, gateway coupling disabled
//	S6 no borrowing fair gateway          control.Core, borrowing disabled
//	S7 TenantScaler fair gateway          control.Core
//
// Everything runs in-process: the controller reads the gateway's /stats
// over HTTP and publishes its policy with POST /policy, but scales the
// in-process pool directly.
package stack

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"time"

	"tenantscaler/internal/config"
	"tenantscaler/internal/controller"
	"tenantscaler/internal/gateway"
	"tenantscaler/internal/model"
	"tenantscaler/internal/pool"
)

// DiscoveryInterval is how often the gateway re-reads the in-process
// pool's ready replicas. It is shorter than llama.DefaultDrainGrace, so the
// gateway stops dispatching to a draining replica before it is killed.
const DiscoveryInterval = 250 * time.Millisecond

// Options configures Start.
type Options struct {
	// Setup is one of Supported.
	Setup string
	// Config supplies the pool budget, tenants and SLOs, controller tuning,
	// dispatch window, engine parameters and the replica backend.
	Config config.Config
	// InitialReplicas is the warm pool started (and ready) before Start
	// returns, clamped to [pool.minReplicas, pool.capacityReplicas]; 0
	// means pool.minReplicas. The static partition always starts at
	// capacity.
	InitialReplicas int
	// LogDir receives the llama-server logs (llama backend; "" means
	// llama.DefaultLogDir).
	LogDir string
	// Seed seeds the simulated engines' jitter, the gateway's Retry-After
	// jitter and the control phase: the control loop makes its first sync
	// a seeded delay in [0, cycle) after Start, so runs with different
	// seeds sample the phase of the control cycle relative to the traffic
	// (see ControlPhase).
	Seed int64
	// Logger receives the components' logs; nil means slog.Default().
	Logger *slog.Logger
	// DecisionsFile, when set, appends the controller's decision records
	// as JSON Lines (controller setups only).
	DecisionsFile string
	// ReadinessHold delays when every launched replica, the warm pool
	// included, counts as ready (pool.WithReadinessHold): an injected load
	// delay, labelled pool.HoldLabel wherever it is reported.
	ReadinessHold time.Duration
	// PublicAddr, when set, adds a second listener (for example
	// 0.0.0.0:8081) serving only the gateway's PublicHandler, for
	// teammates' laptops in the live demo (decision Q16); TenantKeys are
	// then required and enforced on completions.
	PublicAddr string
	TenantKeys map[string]gateway.KeyGrant
}

// Stack is one running setup. Create it with Start and always Stop it.
type Stack struct {
	setup     string
	spec      setupSpec
	cfg       config.Config
	log       *slog.Logger
	pool      *pool.Pool
	engine    EngineInfo
	gw        *gateway.Gateway
	srv       *http.Server
	pubSrv    *http.Server // PublicAddr's listener, if any
	pubAddr   string
	url       string
	decisions *controller.DecisionLog
	rec       *recorder
	initial   int           // warm pool started by Start
	phase     time.Duration // delay of the control loop's first sync
	control   bool          // a control loop runs

	coldMu sync.Mutex
	colds  map[string]float64 // cold start (s) of every replica seen ready, by ID

	cancel   context.CancelFunc // stops discovery and the control loop
	loops    sync.WaitGroup     // control loop and HTTP server goroutines
	serveErr error              // set by the server goroutine before loops.Done

	stopOnce sync.Once
	stopErr  error
}

// Start builds the setup's stack: it launches the pool, waits (bounded by
// ReadyTimeout) until the initial replicas are ready, starts the gateway on
// 127.0.0.1:0 and the control plane. ctx bounds the start-up only; the
// stack runs until Stop. On error everything started so far is stopped.
func Start(ctx context.Context, o Options) (*Stack, error) {
	spec, ok := specs[o.Setup]
	if !ok {
		return nil, fmt.Errorf("stack: unsupported setup %q (want one of %v)", o.Setup, Supported)
	}
	cfg := o.Config
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("stack: config: %w", err)
	}
	logger := o.Logger
	if logger == nil {
		logger = slog.Default()
	}
	logger = logger.With("setup", o.Setup)
	backend, err := NewBackend(cfg, o.LogDir, o.Seed, logger)
	if err != nil {
		return nil, fmt.Errorf("stack: %w", err)
	}
	backend = pool.WithReadinessHold(backend, o.ReadinessHold)
	timeout, err := ReadyTimeout(cfg)
	if err != nil {
		return nil, fmt.Errorf("stack: %w", err)
	}
	timeout += max(o.ReadinessHold, 0)
	s := &Stack{setup: o.Setup, spec: spec, cfg: cfg, log: logger, rec: newRecorder(), colds: map[string]float64{}}
	s.pool = pool.New(pool.Config{
		Name: cfg.Pool.Name, MaxReplicas: cfg.Pool.CapacityReplicas, Seed: o.Seed, Backend: backend, Logger: logger,
	})
	n := initialReplicas(cfg, spec, o.InitialReplicas)
	began := time.Now()
	if err := s.pool.Scale(n); err != nil {
		return nil, errors.Join(fmt.Errorf("stack %s: start %d replicas: %w", o.Setup, n, err), s.pool.Close())
	}
	if err := WaitReady(ctx, s.pool, n, timeout); err != nil {
		return nil, errors.Join(fmt.Errorf("stack %s: %w", o.Setup, err), s.pool.Close())
	}
	s.initial = n
	s.recordColdStarts(s.pool.Status())
	s.engine = engineInfo(ctx, backend, s.pool.Endpoints(), logger)
	logger.Info("replicas ready", "replicas", n, "backend", backend.Name(), "after", time.Since(began).Round(time.Millisecond))

	runCtx, cancel := context.WithCancel(context.Background())
	s.cancel = cancel
	if err := s.startGateway(runCtx, o); err != nil {
		cancel()
		return nil, errors.Join(fmt.Errorf("stack %s: %w", o.Setup, err), s.pool.Close())
	}
	if err := s.startControl(runCtx, o, n); err != nil {
		return nil, errors.Join(fmt.Errorf("stack %s: %w", o.Setup, err), s.Stop(context.Background()))
	}
	logger.Info("stack started", "url", s.url, "gateway", spec.gateway, "control", spec.describe, "controlPhase", s.phase)
	return s, nil
}

// initialReplicas is the warm pool size of a setup.
func initialReplicas(cfg config.Config, spec setupSpec, requested int) int {
	if spec.fullPool {
		return cfg.Pool.CapacityReplicas
	}
	if requested == 0 {
		requested = cfg.Pool.MinReplicas
	}
	return min(max(requested, cfg.Pool.MinReplicas), cfg.Pool.CapacityReplicas)
}

// startGateway starts endpoint discovery over the in-process pool, the
// gateway and its HTTP server on 127.0.0.1:0.
func (s *Stack) startGateway(ctx context.Context, o Options) error {
	if o.PublicAddr != "" && len(o.TenantKeys) == 0 {
		return errors.New("a public gateway address needs tenant API keys ($TS_TENANT_KEYS)")
	}
	disc := gateway.NewPollingDiscovery(func(context.Context) ([]string, error) {
		return s.pool.Endpoints(), nil
	}, DiscoveryInterval)
	disc.Logger = s.log
	if err := disc.Refresh(ctx); err != nil { // the gateway starts with endpoints
		return fmt.Errorf("discover endpoints: %w", err)
	}
	disc.Start(ctx)
	gw, err := gateway.New(gateway.Config{
		Mode: s.spec.gateway, Tenants: s.cfg.Tenants, SlotsPerReplica: s.cfg.SlotsPerReplica(),
		Discovery: disc, Seed: o.Seed, Logger: s.log, TokenCounter: tokenCounter(s.cfg), TenantKeys: o.TenantKeys,
	})
	if err != nil {
		return err
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		gw.Close()
		return fmt.Errorf("listen for the gateway: %w", err)
	}
	s.gw = gw
	s.url = "http://" + ln.Addr().String()
	s.srv = &http.Server{Handler: gw.Handler(), ReadHeaderTimeout: 5 * time.Second}
	s.loops.Add(1)
	go func() {
		defer s.loops.Done()
		if err := s.srv.Serve(ln); !errors.Is(err, http.ErrServerClosed) {
			s.serveErr = fmt.Errorf("gateway server: %w", err)
		}
	}()
	if o.PublicAddr == "" {
		return nil
	}
	pub, err := net.Listen("tcp", o.PublicAddr)
	if err != nil {
		gw.Close()
		_ = s.srv.Close()
		s.loops.Wait()
		return fmt.Errorf("listen on the public address %s: %w", o.PublicAddr, err)
	}
	s.pubAddr = pub.Addr().String()
	s.pubSrv = &http.Server{Handler: gw.PublicHandler(), ReadHeaderTimeout: 5 * time.Second}
	s.loops.Add(1)
	go func() {
		defer s.loops.Done()
		if err := s.pubSrv.Serve(pub); !errors.Is(err, http.ErrServerClosed) {
			s.serveErr = fmt.Errorf("public gateway server: %w", err)
		}
	}()
	s.log.Info("public gateway listening", "addr", s.pubAddr, "tenantKeys", len(o.TenantKeys))
	return nil
}

// PublicAddr is the address of the public listener ("" without one).
func (s *Stack) PublicAddr() string { return s.pubAddr }

// URL is the gateway's base URL (http://127.0.0.1:port), the stack's
// OpenAI-compatible entry point.
func (s *Stack) URL() string { return s.url }

// Setup returns the setup name.
func (s *Stack) Setup() string { return s.setup }

// GatewayMode returns the gateway mode of the setup.
func (s *Stack) GatewayMode() string { return s.spec.gateway }

// Engine describes what serves the requests (backend, and for real
// replicas the llama.cpp build and model reported by a replica).
func (s *Stack) Engine() EngineInfo { return s.engine }

// Simulated reports whether the replicas' compute is simulated.
func (s *Stack) Simulated() bool { return s.engine.Simulated }

// PoolStatus returns the pool's current status.
func (s *Stack) PoolStatus() pool.Status { return s.pool.Status() }

// InitialReplicas is the warm pool Start brought up (and waited for):
// Options.InitialReplicas after clamping, or the capacity for the static
// partition.
func (s *Stack) InitialReplicas() int { return s.initial }

// ControlPhase is the seeded delay of the control loop's first sync after
// Start (see Options.Seed); ok is false for setups without a control
// plane.
func (s *Stack) ControlPhase() (phase time.Duration, ok bool) { return s.phase, s.control }

// ColdStarts returns the launch-to-ready time in seconds of every replica
// seen ready, by replica ID: measured for real replicas, configured for
// simulated ones. Start, every Sample and Stop record the pool's ready and
// draining replicas, so after Stop it also holds the replicas scaled away
// during the run (a replica would have to start and be removed between
// two samples to be missed).
func (s *Stack) ColdStarts() map[string]float64 {
	s.coldMu.Lock()
	defer s.coldMu.Unlock()
	out := make(map[string]float64, len(s.colds))
	for id, c := range s.colds {
		out[id] = c
	}
	return out
}

// recordColdStarts records the cold start of every replica of st that has
// one (ready or draining; 0 means still starting).
func (s *Stack) recordColdStarts(st pool.Status) {
	s.coldMu.Lock()
	defer s.coldMu.Unlock()
	for _, r := range st.Replicas {
		if r.ColdStartSeconds > 0 {
			s.colds[r.ID] = r.ColdStartSeconds
		}
	}
}

// Decisions returns up to limit of the controller's decision records,
// newest first (limit <= 0 means all kept); nil for setups without the
// TenantScaler controller.
func (s *Stack) Decisions(limit int) []model.DecisionRecord {
	if s.decisions == nil {
		return nil
	}
	return s.decisions.List(limit, "")
}

// Stop stops the control loop, shuts the gateway down (queued requests get
// 503, in-flight ones are cut), closes the pool, which kills real
// replicas, and returns only when all of it has stopped. ctx bounds the
// graceful HTTP shutdown; after it the server is closed hard. Stop is
// idempotent and returns the same error every time.
func (s *Stack) Stop(ctx context.Context) error {
	s.stopOnce.Do(func() {
		var errs []error
		if s.cancel != nil {
			s.cancel()
		}
		if s.gw != nil {
			s.gw.Close()
		}
		for _, srv := range []*http.Server{s.pubSrv, s.srv} {
			if srv == nil {
				continue
			}
			if err := srv.Shutdown(ctx); err != nil {
				errs = append(errs, fmt.Errorf("gateway shutdown: %w", err), srv.Close())
			}
		}
		s.loops.Wait()
		s.recordColdStarts(s.pool.Status())
		errs = append(errs, s.serveErr, s.pool.Close())
		if s.decisions != nil {
			errs = append(errs, s.decisions.Close())
		}
		s.stopErr = errors.Join(errs...)
		s.log.Info("stack stopped", "err", s.stopErr)
	})
	return s.stopErr
}

// TokenizerFallbacks is how many prompts the gateway counted in words
// because the replicas' tokenizer was unavailable (0 for simulated stacks,
// whose gateway counts words by design).
func (s *Stack) TokenizerFallbacks() int64 { return s.gw.TokenizerFallbacks() }
