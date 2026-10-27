package pool

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"tenantscaler/internal/config"
	"tenantscaler/internal/engine"
	"tenantscaler/internal/replica"
)

// shutdownTimeout bounds a simulated replica's graceful HTTP shutdown and
// the wait for its in-flight requests to abort when it is closed.
const shutdownTimeout = 5 * time.Second

// simulatedBackend hosts SIMULATED replicas in-process (package replica),
// each on its own http.Server bound to 127.0.0.1:0.
type simulatedBackend struct {
	params engine.Params
	seed   int64

	mu       sync.Mutex
	launched int64 // successful launches; replica n gets jitter seed seed+n
}

// NewSimulatedBackend returns the in-process SIMULATED engine backend. The
// n-th replica it launches gets the jitter seed seed+n, so a pool's
// replica "<name>-n" behaves the same as before backends existed.
func NewSimulatedBackend(p engine.Params, seed int64) Backend {
	return &simulatedBackend{params: p, seed: seed}
}

// Name returns config.BackendSimulated.
func (b *simulatedBackend) Name() string { return config.BackendSimulated }

// Simulated returns true: the compute is simulated.
func (b *simulatedBackend) Simulated() bool { return true }

// Launch starts a simulated replica in state "starting"; it turns ready
// after the engine's StartupDuration. slot is not used: simulated replicas
// share the host process.
func (b *simulatedBackend) Launch(id string, _ int) (Instance, error) {
	if err := b.params.Validate(); err != nil {
		return nil, err
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("listen for new replica: %w", err)
	}
	b.mu.Lock()
	b.launched++
	seed := b.seed + b.launched
	b.mu.Unlock()

	rep := replica.New(id, b.params, seed)
	inst := &simInstance{
		rep:     rep,
		srv:     &http.Server{Handler: rep.Handler(), ReadHeaderTimeout: 5 * time.Second},
		url:     "http://" + ln.Addr().String(),
		startup: b.params.StartupDuration,
		started: time.Now(),
		served:  make(chan struct{}),
	}
	rep.Start(inst.started)
	go func() {
		defer close(inst.served)
		// Serve only returns once the server is shut down or closed.
		_ = inst.srv.Serve(ln)
	}()
	return inst, nil
}

// simInstance is one simulated replica and its HTTP server.
type simInstance struct {
	rep     *replica.Replica
	srv     *http.Server
	url     string
	startup time.Duration
	started time.Time
	served  chan struct{} // closed when Serve has returned

	closed    atomic.Bool // Close has been called
	closeOnce sync.Once
	closeErr  error
}

func (s *simInstance) ID() string                            { return s.rep.ID() }
func (s *simInstance) URL() string                           { return s.url }
func (s *simInstance) Stats() replica.Stats                  { return s.rep.Stats() }
func (s *simInstance) BeginDrain()                           { s.rep.BeginDrain() }
func (s *simInstance) WaitDrained(ctx context.Context) error { return s.rep.WaitDrained(ctx) }

// State is the replica's state, or replica.StateStopped once closed.
func (s *simInstance) State() string {
	if s.closed.Load() {
		return replica.StateStopped
	}
	return s.rep.State()
}

// ColdStart is the simulated model-load time once it has elapsed, else 0.
func (s *simInstance) ColdStart() time.Duration {
	if time.Since(s.started) < s.startup {
		return 0
	}
	return s.startup
}

// Close shuts the server down. A drained replica shuts down gracefully, so
// the last responses are flushed completely; otherwise listeners and
// connections close at once, in-flight requests abort, and Close waits
// (bounded by shutdownTimeout) until their handlers have returned.
func (s *simInstance) Close() error {
	s.closeOnce.Do(func() {
		s.closeErr = s.close()
		s.closed.Store(true)
	})
	return s.closeErr
}

func (s *simInstance) close() error {
	var errs []error
	if s.rep.Drained() {
		ctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()
		if s.srv.Shutdown(ctx) != nil {
			_ = s.srv.Close()
		}
	} else if err := s.srv.Close(); err != nil {
		errs = append(errs, fmt.Errorf("close replica %s: %w", s.rep.ID(), err))
	}
	ctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	s.rep.BeginDrain()
	if err := s.rep.WaitDrained(ctx); err != nil {
		errs = append(errs, err)
	}
	select {
	case <-s.served:
	case <-ctx.Done():
		errs = append(errs, fmt.Errorf("replica %s: server did not stop: %w", s.rep.ID(), ctx.Err()))
	}
	return errors.Join(errs...)
}
