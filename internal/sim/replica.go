package sim

import (
	"slices"
	"time"

	"tenantscaler/internal/engine"
)

// repState is a replica's lifecycle state.
type repState uint8

const (
	repPending  repState = iota // wanted, waiting for a free GPU slot
	repStarting                 // loading the model (cold start)
	repReady                    // serving
	repDraining                 // finishing accepted requests, then gone
	repGone                     // removed
)

// replica is one simulated engine instance.
type replica struct {
	id    int
	state repState
	// startedAt is when loading began; readyAt when it could first serve.
	// Replicas of the initial warm pool have both at 0.
	startedAt, readyAt time.Duration
	// running requests hold one of MaxNumSeqs slots; waiting requests
	// queue FCFS inside the replica.
	running int
	waiting []*request
	// assigned counts requests sent here and not finished (running +
	// waiting): the gateway's dispatch-window load.
	assigned int
	// served counts requests started here; firstServe is the first start.
	served     int
	firstServe time.Duration
	// owner is the tenant owning this replica in partition mode.
	owner string
}

// newReplica creates a pending replica.
func (s *simulator) newReplica() *replica {
	r := &replica{id: s.nextID, state: repPending}
	s.nextID++
	s.reps = append(s.reps, r)
	s.history = append(s.history, r)
	return r
}

// physical counts replicas holding a GPU slot.
func (s *simulator) physical() int {
	n := 0
	for _, r := range s.reps {
		if r.state == repStarting || r.state == repReady || r.state == repDraining {
			n++
		}
	}
	return n
}

func (s *simulator) notePhysical() {
	s.diag.PeakPhysicalReplicas = max(s.diag.PeakPhysicalReplicas, s.physical())
}

// readyReps returns the replicas accepting new requests, by id.
func (s *simulator) readyReps() []*replica {
	var out []*replica
	for _, r := range s.reps {
		if r.state == repReady {
			out = append(out, r)
		}
	}
	return out
}

// scaleTo sets the wanted replica count (pending + starting + ready),
// clamped to [0, capacity], adding or removing replicas.
func (s *simulator) scaleTo(n int) {
	n = min(max(n, 0), s.cfg.Pool.CapacityReplicas)
	s.target = n
	var active []*replica
	for _, r := range s.reps {
		if r.state <= repReady {
			active = append(active, r)
		}
	}
	switch {
	case len(active) < n:
		for range n - len(active) {
			s.newReplica()
		}
	case len(active) > n:
		s.removeReplicas(active, len(active)-n)
	}
	s.startPending()
}

// removeReplicas takes k replicas out of active: pending ones first, then
// starting, then idle ready, then busy ready (which drain); newest first
// inside each group.
func (s *simulator) removeReplicas(active []*replica, k int) {
	rank := func(r *replica) int {
		switch {
		case r.state == repPending:
			return 0
		case r.state == repStarting:
			return 1
		case r.assigned == 0:
			return 2
		}
		return 3
	}
	order := slices.Clone(active)
	slices.SortStableFunc(order, func(a, b *replica) int {
		if ra, rb := rank(a), rank(b); ra != rb {
			return ra - rb
		}
		return b.id - a.id
	})
	endpoints := false
	for _, r := range order[:k] {
		if r.state == repReady {
			endpoints = true
		}
		if r.state == repReady && r.assigned > 0 {
			r.state = repDraining
			continue
		}
		r.state = repGone
	}
	s.compact()
	if endpoints {
		s.endpointsChanged()
	}
}

// compact drops gone replicas from the live list.
func (s *simulator) compact() {
	s.reps = slices.DeleteFunc(s.reps, func(r *replica) bool { return r.state == repGone })
}

// startPending begins loading pending replicas, oldest first, while a GPU
// slot is free.
func (s *simulator) startPending() {
	free := s.cfg.Pool.CapacityReplicas - s.physical()
	for _, r := range s.reps {
		if free <= 0 {
			break
		}
		if r.state == repPending {
			r.state, r.startedAt, r.readyAt = repStarting, s.now, s.now+s.st.ColdStart
			s.schedule(r.readyAt, &event{kind: evReady, rep: r})
			free--
		}
	}
	s.notePhysical()
}

// onReady completes a cold start, unless the replica was removed meanwhile.
func (s *simulator) onReady(r *replica) {
	if r.state != repStarting {
		return
	}
	r.state = repReady
	s.endpointsChanged()
	s.dispatch()
}

// accept hands req to replica r: it starts at once if a slot is free and
// waits in the replica's FCFS queue otherwise.
func (s *simulator) accept(r *replica, req *request) {
	r.assigned++
	req.rep = r
	if r.running < s.params.MaxNumSeqs {
		s.startOn(r, req)
		return
	}
	r.waiting = append(r.waiting, req)
	req.setState(stWaiting)
}

// startOn begins serving req on r: prefill, then out-1 tokens, with
// latencies from the engine model at the running count at the start.
func (s *simulator) startOn(r *replica, req *request) {
	r.running++
	if r.served == 0 {
		r.firstServe = s.now
	}
	r.served++
	p, frac := s.params, s.params.JitterFraction
	prefill := engine.Jitter(p.PrefillTime(req.spec.InputTokens, r.running), frac, s.rng)
	interval := engine.Jitter(p.TokenInterval(r.running), frac, s.rng)
	req.started, req.interval = s.now, interval
	req.firstToken = s.now + prefill
	req.done = req.firstToken + time.Duration(max(req.spec.OutputTokens-1, 0))*interval
	req.setState(stRunning)
	s.schedule(req.done, &event{kind: evDone, req: req})
}

// onDone finishes req: frees its slot, starts the replica's next waiting
// request, retires a drained replica and lets the gateway dispatch.
func (s *simulator) onDone(req *request) {
	r := req.rep
	r.running--
	r.assigned--
	s.complete(req)
	if len(r.waiting) > 0 && r.running < s.params.MaxNumSeqs {
		next := r.waiting[0]
		r.waiting = r.waiting[1:]
		s.startOn(r, next)
	}
	if r.state == repDraining && r.assigned == 0 {
		r.state = repGone
		s.compact()
		s.startPending()
	}
	s.dispatch()
}

// leastLoaded returns the replica of eps with the fewest assigned requests
// below the dispatch window (lowest id on ties), or nil when all are full.
func leastLoaded(eps []*replica, window int) *replica {
	var best *replica
	for _, r := range eps {
		if r.assigned < window && (best == nil || r.assigned < best.assigned) {
			best = r
		}
	}
	return best
}
