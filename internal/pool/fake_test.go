package pool

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"tenantscaler/internal/replica"
)

// fakeInstance is a controllable Instance: tests set its state and decide
// when its drain finishes.
type fakeInstance struct {
	id, url   string
	coldStart time.Duration
	drainDone chan struct{} // closed by finishDrain
	onClose   func()        // optional hook run by Close
	onStats   func()        // optional hook run by Stats

	mu       sync.Mutex
	state    string
	failure  error // returned by Err
	finished bool
	closes   int
}

func (f *fakeInstance) ID() string  { return f.id }
func (f *fakeInstance) URL() string { return f.url }

func (f *fakeInstance) Stats() replica.Stats {
	if f.onStats != nil {
		f.onStats()
	}
	return replica.Stats{Running: 1}
}

func (f *fakeInstance) ColdStart() time.Duration { return f.coldStart }

func (f *fakeInstance) State() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.state
}

func (f *fakeInstance) setState(s string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.state = s
}

// crash stops the instance on its own with the given cause.
func (f *fakeInstance) crash(cause error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.state, f.failure = replica.StateStopped, cause
}

func (f *fakeInstance) Err() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.failure
}

func (f *fakeInstance) BeginDrain() { f.setState(replica.StateDraining) }

func (f *fakeInstance) finishDrain() {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.finished {
		f.finished = true
		close(f.drainDone)
	}
}

func (f *fakeInstance) WaitDrained(ctx context.Context) error {
	select {
	case <-f.drainDone:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (f *fakeInstance) Close() error {
	if f.onClose != nil {
		f.onClose()
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closes++
	f.state = replica.StateStopped
	return nil
}

func (f *fakeInstance) closeCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.closes
}

// fakeBackend launches fakeInstances in state ready and records the slots.
type fakeBackend struct {
	mu      sync.Mutex
	insts   map[string]*fakeInstance
	slots   []string // "id@slot" in launch order
	failErr error    // returned by the next Launch, then cleared
	onClose func()
	onStats func()
}

func newFakeBackend() *fakeBackend { return &fakeBackend{insts: map[string]*fakeInstance{}} }

func (b *fakeBackend) Name() string    { return "fake" }
func (b *fakeBackend) Simulated() bool { return false }

func (b *fakeBackend) Launch(id string, slot int) (Instance, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if err := b.failErr; err != nil {
		b.failErr = nil
		return nil, err
	}
	f := &fakeInstance{
		id: id, url: "http://fake/" + id, state: replica.StateReady,
		coldStart: 1500 * time.Millisecond, drainDone: make(chan struct{}),
		onClose: b.onClose, onStats: b.onStats,
	}
	b.insts[id] = f
	b.slots = append(b.slots, fmt.Sprintf("%s@%d", id, slot))
	return f, nil
}

func (b *fakeBackend) setFailErr(err error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.failErr = err
}

func (b *fakeBackend) inst(id string) *fakeInstance {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.insts[id]
}

func (b *fakeBackend) launched() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return strings.Join(b.slots, ",")
}

// newFakePool returns a pool of fake instances whose reconcile loop never
// runs during the test, so that only Scale changes the membership; the
// reconcile tests use newReconcilingPool instead.
func newFakePool(t *testing.T, maxReplicas int) (*Pool, *fakeBackend) {
	t.Helper()
	b := newFakeBackend()
	p := New(Config{Name: "p", MaxReplicas: maxReplicas, Backend: b, ReconcileInterval: time.Hour})
	t.Cleanup(func() {
		if err := p.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	return p, b
}

// slotsOf lists "id@slot:state" for every replica in the status.
func slotsOf(st Status) string {
	out := make([]string, len(st.Replicas))
	for i, r := range st.Replicas {
		out[i] = fmt.Sprintf("%s@%d:%s", r.ID, r.Slot, r.State)
	}
	return strings.Join(out, ",")
}
