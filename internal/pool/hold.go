package pool

import (
	"time"

	"tenantscaler/internal/replica"
)

// HoldLabel is how every report must describe a readiness hold (BRIEF-v2
// Q12): the delay is injected, the inference is real.
const HoldLabel = "injected load delay (emulates network model load); inference is real"

// WithReadinessHold wraps a backend so that every replica it launches is
// reported as starting until hold has passed since its launch, even if the
// engine is ready earlier. The process really starts and loads the model;
// the hold only delays when the pool, the gateway's discovery and the
// autoscalers see it as ready, emulating the minutes a real model load
// takes over the network (decision Q12). hold <= 0 returns b unchanged.
func WithReadinessHold(b Backend, hold time.Duration) Backend {
	if hold <= 0 {
		return b
	}
	return heldBackend{Backend: b, hold: hold}
}

type heldBackend struct {
	Backend
	hold time.Duration
}

func (b heldBackend) Launch(id string, slot int) (Instance, error) {
	inst, err := b.Backend.Launch(id, slot)
	if err != nil {
		return nil, err
	}
	return &heldInstance{Instance: inst, until: time.Now().Add(b.hold), hold: b.hold}, nil
}

// heldInstance reports "starting" until its hold has passed.
type heldInstance struct {
	Instance
	until time.Time
	hold  time.Duration
}

// State is the engine's state, except that a ready engine still inside
// its hold is reported as starting.
func (h *heldInstance) State() string {
	s := h.Instance.State()
	if s == replica.StateReady && time.Now().Before(h.until) {
		return replica.StateStarting
	}
	return s
}

// ColdStart is the time from launch until the replica was reported ready:
// the engine's own cold start or the hold, whichever is longer; 0 while
// starting.
func (h *heldInstance) ColdStart() time.Duration {
	if h.State() != replica.StateReady && h.Instance.State() != replica.StateDraining {
		return 0
	}
	if cs, ok := h.Instance.(ColdStarter); ok {
		return max(cs.ColdStart(), h.hold)
	}
	return h.hold
}

// Err forwards the engine's failure cause.
func (h *heldInstance) Err() error {
	if f, ok := h.Instance.(Failer); ok {
		return f.Err()
	}
	return nil
}
