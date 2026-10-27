package fairq

import (
	"math"
	"time"

	"tenantscaler/internal/model"
)

// Next is NextWithin without a slot count: no share reservation holds a
// request back.
func (s *Scheduler) Next(now time.Time) *Request { return s.NextWithin(now, math.MaxInt) }

// NextWithin removes and returns the request to dispatch now into one of
// free free slots, or nil when nothing may be dispatched. The caller calls
// it only when a replica slot is free (free >= 1).
//
// Fair mode picks the tenant with the smallest counter among the tenants
// with queued work that may take a slot (ties: lowest name), then inside it
// the request with the highest effective priority, then the earliest
// deadline (none last), then the lowest ID, and charges the tenant
// (InputTokens + 2*est) / weight, where est = min(MaxOutputTokens, tenant
// estimate) and weight = policy.WeightFor(tenant). A tenant the policy
// sheds (over its share) may take a slot while its in-flight requests are
// below its share (see share), and beyond it only if the free-1 slots left
// still cover the unused shares of all other active tenants: it cannot take
// the slots a guaranteed tenant needs for its next requests, since requests
// are not preempted (see mayDispatch). A nil
// result with requests queued means the free slots are reserved; the
// caller retries when a slot frees up or the policy changes. Roundrobin
// mode serves the highest non-empty band, rotating over its tenants one
// request per turn in name order, FIFO inside a tenant. FCFS mode serves by
// (Arrival, ID). Neither reserves slots.
func (s *Scheduler) NextWithin(now time.Time, free int) *Request {
	if s.queued == 0 {
		return nil
	}
	var r *Request
	switch s.cfg.Mode {
	case ModeFair:
		if t := s.minCounterTenant(free); t != nil {
			r = t.fair.take()
		}
	case ModeRoundRobin:
		r = s.nextRoundRobin()
	default:
		r = s.fifo.take()
	}
	if r == nil {
		return nil
	}
	t := r.ten
	s.unqueue(t)
	t.inFlight++
	r.state = stateInFlight
	r.Dispatched = now
	r.EstOutputTokens = t.estOut
	if r.MaxOutputTokens > 0 {
		r.EstOutputTokens = min(r.EstOutputTokens, float64(r.MaxOutputTokens))
	}
	if s.cfg.Mode == ModeFair {
		r.weight = s.pol.WeightFor(t.name)
		r.chargedOut = r.EstOutputTokens
		in := float64(max(r.InputTokens, 0))
		t.counter += model.WeightedTokens(in, r.EstOutputTokens) / r.weight
		t.pruneDispatches(now)
		t.dispatches = append(t.dispatches, now)
	}
	return r
}

// minCounterTenant returns the backlogged tenant with the smallest counter
// that may take one of free slots; names are sorted, so the lowest name
// wins ties.
func (s *Scheduler) minCounterTenant(free int) *tenant {
	var best *tenant
	for _, name := range s.names {
		t := s.tenants[name]
		if t.queued > 0 && (best == nil || t.counter < best.counter) && s.mayDispatch(t, free) {
			best = t
		}
	}
	return best
}

// mayDispatch reports whether t may take one of free slots. Only a tenant
// the controller found over its share (policy Shed) is limited: below its
// share it may, beyond it only if the slots left after this one cover the
// other tenants' unused shares. Tenants within their share are never held
// back, even when a burst exceeds last cycle's grant.
func (s *Scheduler) mayDispatch(t *tenant, free int) bool {
	if !s.pol.Tenants[t.name].Shed || t.inFlight < s.share(t) {
		return true
	}
	reserve := 0
	for _, name := range s.names {
		if u := s.tenants[name]; u != t {
			reserve += max(0, s.share(u)-u.inFlight)
		}
	}
	return free-1 >= reserve
}

// share is t's share of the dispatch slots while it has work queued or in
// flight: the policy's Slots (its grant), at least GuaranteedSlots, so an
// owner whose idle guarantee was lent out gets it back as soon as it
// returns. A tenant with nothing in the gateway reserves nothing, so its
// guarantee can be borrowed.
func (s *Scheduler) share(t *tenant) int {
	tp := s.pol.Tenants[t.name]
	if t.queued > 0 || t.inFlight > 0 {
		return max(tp.Slots, tp.GuaranteedSlots)
	}
	return 0
}

// nextRoundRobin takes the next request in roundrobin mode: strict bands,
// critical first; inside a band the first backlogged tenant after the one
// served last (name order, wrapping around).
func (s *Scheduler) nextRoundRobin() *Request {
	for b := numBands - 1; b >= 0; b-- {
		var first, after *tenant
		for _, name := range s.names {
			t := s.tenants[name]
			if t.bands[b].Len() == 0 {
				continue
			}
			if first == nil {
				first = t
			}
			if name > s.rrLast[b] {
				after = t
				break
			}
		}
		if after == nil {
			after = first
		}
		if after != nil {
			s.rrLast[b] = after.name
			return after.bands[b].take()
		}
	}
	return nil
}

// OnOutputTokens records that r has produced `produced` output tokens so
// far (monotonic). In fair mode, tokens beyond what was already charged are
// charged now at 2 per token divided by the weight used at dispatch. It is a
// no-op for a request that is not in flight in this scheduler.
func (s *Scheduler) OnOutputTokens(r *Request, produced int) {
	if r.owner != s || r.state != stateInFlight || s.cfg.Mode != ModeFair {
		return
	}
	if p := float64(produced); p > r.chargedOut {
		r.ten.counter += model.OutputTokenWeight * (p - r.chargedOut) / r.weight
		r.chargedOut = p
	}
}

// Complete finishes an in-flight request that produced outputTokens tokens.
// In fair mode it corrects the tenant's counter to the actual output (which
// may lower it), so the request's net charge is
// (InputTokens + 2*outputTokens) / dispatch weight. It updates the tenant's
// output-length estimate (EWMA, alpha 0.2) and frees the request's in-flight
// and critical accounting. It is a no-op for a request that is not in
// flight in this scheduler.
func (s *Scheduler) Complete(r *Request, outputTokens int, now time.Time) {
	if r.owner != s || r.state != stateInFlight {
		return
	}
	t := r.ten
	out := float64(max(outputTokens, 0))
	if s.cfg.Mode == ModeFair {
		t.counter += model.OutputTokenWeight * (out - r.chargedOut) / r.weight
		t.pruneDispatches(now)
	}
	t.estOut = (1-estimateAlpha)*t.estOut + estimateAlpha*out
	t.inFlight--
	r.state = stateDone
	release(t, r)
}

// Cancel removes a still-queued request and reports whether it was queued
// in this scheduler. A dispatched request cannot be cancelled; its slot is
// freed by Complete.
func (s *Scheduler) Cancel(r *Request) bool {
	if r.owner != s || r.state != stateQueued {
		return false
	}
	t := r.ten
	r.q.remove(r)
	s.unqueue(t)
	r.state = stateDone
	release(t, r)
	return true
}

// unqueue accounts for a request that left t's queue, by dispatch or
// cancellation. When t's queue is now empty, t becomes the lift reference
// for when no tenant has queued work.
func (s *Scheduler) unqueue(t *tenant) {
	t.queued--
	s.queued--
	if t.queued == 0 {
		s.lastDrained = t
	}
}

// release drops the critical-quota accounting of r, which is finished.
func release(t *tenant, r *Request) {
	if r.EffectivePriority.Critical() {
		t.criticalActive--
	}
}
