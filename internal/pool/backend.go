package pool

import (
	"context"
	"time"

	"tenantscaler/internal/replica"
)

// Instance is one replica hosted by a Pool: a simulated in-process engine
// (NewSimulatedBackend) or a real llama-server process (package llama).
type Instance interface {
	// ID is the replica identifier given to Backend.Launch.
	ID() string
	// URL is the replica's OpenAI-compatible base URL, e.g.
	// "http://127.0.0.1:41234".
	URL() string
	// State is replica.StateStarting, StateReady, StateDraining or
	// StateStopped (the instance has exited or was closed).
	State() string
	// Stats is the current request load: Running in the engine's batch,
	// Waiting in the engine's own queue.
	Stats() replica.Stats
	// BeginDrain marks the instance for removal: it leaves the ready set
	// while in-flight work finishes. It cannot be undone.
	BeginDrain()
	// WaitDrained blocks until in-flight work has finished after
	// BeginDrain, or ctx ends.
	WaitDrained(ctx context.Context) error
	// Close stops the instance now; in-flight requests abort. A real
	// process is killed and reaped. Close is idempotent and safe for
	// concurrent use.
	Close() error
}

// Backend launches instances. One Backend serves one Pool.
type Backend interface {
	// Name is config.BackendSimulated ("simulated") or config.BackendLlama
	// ("llama").
	Name() string
	// Simulated reports whether the instances' compute is simulated, so
	// that UIs and reports can label their numbers.
	Simulated() bool
	// Launch starts a replica with the given ID. slot is in
	// [0, MaxReplicas) and selects the replica's CPU affinity mask; a slot
	// is held by at most one live instance, except while a draining
	// instance shares its slot with its replacement. The returned instance
	// is usually still starting.
	Launch(id string, slot int) (Instance, error)
}

// ColdStarter is implemented by instances that report their cold start:
// the time from launch until they first became ready, or 0 while still
// starting. Pool.Status reports it as ReplicaStatus.ColdStartSeconds.
type ColdStarter interface {
	ColdStart() time.Duration
}

// Failer is implemented by instances that can explain why they stopped on
// their own: Err returns the cause (a crash's exit status, a readiness
// timeout) or nil. The pool logs it when it replaces such an instance.
type Failer interface {
	Err() error
}
