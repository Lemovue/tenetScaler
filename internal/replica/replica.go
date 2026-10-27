// Package replica is one SIMULATED OpenAI-compatible LLM engine: the HTTP
// API, streaming, admission and metrics are real; the compute is simulated
// by sleeping for the latencies computed by package engine. It models a
// vLLM-like server: at most MaxNumSeqs requests run at once and the rest
// wait in the replica's own FCFS queue, which is what happens when a
// gateway does not hold requests back.
package replica

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"sync"
	"sync/atomic"
	"time"

	"tenantscaler/internal/engine"
)

// Lifecycle states reported by State.
const (
	// StateStopped means Start has not been called yet.
	StateStopped = "stopped"
	// StateStarting means the simulated model is loading (cold start).
	StateStarting = "starting"
	// StateReady means the replica accepts requests.
	StateReady = "ready"
	// StateDraining means BeginDrain was called: in-flight and queued
	// requests finish, new requests get 503.
	StateDraining = "draining"
)

// Stats is a snapshot of the replica's request load.
type Stats struct {
	// Running is the number of requests in the simulated batch.
	Running int `json:"running"`
	// Waiting is the number of requests in the replica's FCFS queue.
	Waiting int `json:"waiting"`
}

// errUnavailable is returned by admit when the replica is not ready.
var errUnavailable = errors.New("replica unavailable")

// Replica is one simulated engine. It is safe for concurrent use.
type Replica struct {
	id      string
	params  engine.Params
	metrics *metrics
	reqSeq  atomic.Uint64
	// drained is closed once the replica is draining and idle.
	drained chan struct{}

	mu        sync.Mutex
	rng       *rand.Rand // jitter source, guarded by mu
	started   bool
	readyAt   time.Time
	draining  bool
	signalled bool // drained has been closed
	running   int
	queue     []*waiter // FCFS; queue[0] is served next
}

// waiter is one request waiting for a running slot.
type waiter struct {
	ready   chan struct{}
	granted bool // set under Replica.mu when a slot is handed over
}

// New creates a stopped replica with the given simulated engine parameters.
// seed makes the latency jitter reproducible.
func New(id string, p engine.Params, seed int64) *Replica {
	return &Replica{
		id:      id,
		params:  p,
		metrics: newMetrics(DefaultModel),
		drained: make(chan struct{}),
		rng:     rand.New(rand.NewSource(seed)),
	}
}

// ID returns the replica's identifier.
func (r *Replica) ID() string { return r.id }

// Start begins the simulated model load at now. The replica reports
// StateStarting until StartupDuration has elapsed on the real clock and
// StateReady afterwards. Calling Start again has no effect.
func (r *Replica) Start(now time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.started {
		return
	}
	r.started = true
	r.readyAt = now.Add(r.params.StartupDuration)
}

// State returns the current lifecycle state (one of the State constants).
func (r *Replica) State() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.stateLocked(time.Now())
}

func (r *Replica) stateLocked(now time.Time) string {
	switch {
	case r.draining:
		return StateDraining
	case !r.started:
		return StateStopped
	case now.Before(r.readyAt):
		return StateStarting
	default:
		return StateReady
	}
}

// BeginDrain stops admitting new requests (readiness turns false and new
// requests get 503) while running and queued requests finish. It cannot be
// undone.
func (r *Replica) BeginDrain() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.draining = true
	r.signalIfDrainedLocked()
}

// Drained reports whether the replica is draining and has no running or
// queued requests left.
func (r *Replica) Drained() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.drainedLocked()
}

// WaitDrained blocks until Drained is true or ctx ends. It only returns nil
// after BeginDrain has been called.
func (r *Replica) WaitDrained(ctx context.Context) error {
	select {
	case <-r.drained:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("replica %s: waiting for drain: %w", r.id, ctx.Err())
	}
}

// Stats returns the current running and waiting request counts.
func (r *Replica) Stats() Stats {
	r.mu.Lock()
	defer r.mu.Unlock()
	return Stats{Running: r.running, Waiting: len(r.queue)}
}

func (r *Replica) drainedLocked() bool {
	return r.draining && r.running == 0 && len(r.queue) == 0
}

func (r *Replica) signalIfDrainedLocked() {
	if !r.signalled && r.drainedLocked() {
		r.signalled = true
		close(r.drained)
	}
}

// admit reserves a running slot for a new request. It fails with
// errUnavailable (wrapped, naming the state) unless the replica is ready.
// When all MaxNumSeqs slots are busy, the request waits FCFS until a slot
// is handed to it or ctx ends. On success the caller must call release.
func (r *Replica) admit(ctx context.Context) error {
	r.mu.Lock()
	if st := r.stateLocked(time.Now()); st != StateReady {
		r.mu.Unlock()
		return fmt.Errorf("%w: replica %s is %s", errUnavailable, r.id, st)
	}
	if r.running < r.params.MaxNumSeqs && len(r.queue) == 0 {
		r.running++
		r.mu.Unlock()
		return nil
	}
	w := &waiter{ready: make(chan struct{})}
	r.queue = append(r.queue, w)
	r.mu.Unlock()

	select {
	case <-w.ready:
		return nil
	case <-ctx.Done():
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if w.granted {
		// The slot arrived together with the cancellation: pass it on.
		r.releaseLocked()
	} else {
		r.removeLocked(w)
		r.signalIfDrainedLocked()
	}
	return ctx.Err()
}

// release frees the caller's running slot, handing it to the oldest
// waiter if there is one.
func (r *Replica) release() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.releaseLocked()
}

func (r *Replica) releaseLocked() {
	if len(r.queue) > 0 {
		w := r.queue[0]
		r.queue[0] = nil
		r.queue = r.queue[1:]
		w.granted = true
		close(w.ready)
	} else {
		r.running--
	}
	r.signalIfDrainedLocked()
}

func (r *Replica) removeLocked(w *waiter) {
	for i, q := range r.queue {
		if q == w {
			r.queue = append(r.queue[:i], r.queue[i+1:]...)
			return
		}
	}
}

// prefillDelay is the jittered prefill time of a prompt with inTok tokens
// at the running count of this moment (the request itself included).
func (r *Replica) prefillDelay(inTok int) time.Duration {
	return r.delay(func(running int) time.Duration { return r.params.PrefillTime(inTok, running) })
}

// tokenDelay is the jittered decode time of one output token at the
// running count of this moment.
func (r *Replica) tokenDelay() time.Duration {
	return r.delay(r.params.TokenInterval)
}

// delay returns a jittered latency computed from the running count at this
// moment. Waiting requests do not count: they are not in the batch.
func (r *Replica) delay(base func(running int) time.Duration) time.Duration {
	r.mu.Lock()
	defer r.mu.Unlock()
	return engine.Jitter(base(r.running), r.params.JitterFraction, r.rng)
}

// sleep waits for d or until ctx ends (client disconnect).
func sleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
