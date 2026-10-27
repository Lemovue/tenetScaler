package fairq

import (
	"math"
	"time"

	"tenantscaler/internal/model"
)

// Admit decides whether r is queued. It sets r.EffectivePriority and, on
// admission, enqueues r. saturated reports whether the replica pool has no
// free slot; fair and roundrobin modes use it.
//
// Fair mode applies, in order: (1) the critical quota: a critical request
// whose tenant already has policy CriticalSlots (> 0) critical requests
// queued or in flight is downgraded to standard; (2) a sheddable request of
// a tenant whose policy has Shed is rejected "over-share" while the pool is
// contended: saturated, or any request already queued here. With a free
// slot and an empty queue the request would be served at once without
// delaying anyone, so shedding it would only waste capacity; (3) a sheddable
// request whose predicted wait, (queued+1) / dispatch rate over the last
// 10 s, exceeds Deadline-now is rejected "deadline-unreachable"; (4)
// queue-full; (5) the counter lift (see lift) for a tenant with nothing
// queued, whether or not it has requests in flight.
//
// Step 3 is skipped when the request has no deadline, when the rate is
// zero, and when the tenant has nothing queued. With nothing queued the
// request is next in its tenant and the lift puts the tenant at the fair
// frontier, while the dispatch rate of a tenant that has not been
// backlogged measures its demand, not the service it would get: a tenant
// sending one request every 3 s would otherwise have every 2 s deadline
// predicted as a 3.3-10 s wait.
//
// Roundrobin mode rejects sheddable requests with "saturated" while
// saturated is true, then applies queue-full. FCFS mode applies queue-full
// only. Queue-full: the tenant has MaxQueuePerTenant requests queued and r
// is not effectively critical. The baseline modes ignore the policy except
// for the Retry-After base.
//
// A rejection carries RetryAfter = max(policy RetryAfterSeconds, 1 s)
// scaled by (1 + RetryJitter*U[0,1)) from the seeded generator.
func (s *Scheduler) Admit(r *Request, now time.Time, saturated bool) Verdict {
	t := s.tenant(r.Tenant)
	r.EffectivePriority = r.Priority
	reason := ""
	switch s.cfg.Mode {
	case ModeFair:
		reason = s.fairAdmission(t, r, now, saturated)
	case ModeRoundRobin:
		if saturated && r.Priority.Sheddable() {
			reason = ReasonSaturated
		}
	}
	if reason == "" && t.queued >= s.cfg.MaxQueuePerTenant && !r.EffectivePriority.Critical() {
		reason = ReasonQueueFull
	}
	if reason != "" {
		t.rejected[reason]++
		return Verdict{RetryAfter: s.retryAfter(r.Tenant), Reason: reason}
	}
	if s.cfg.Mode == ModeFair && t.queued == 0 {
		s.lift(t)
	}
	s.enqueue(t, r)
	return Verdict{Admitted: true}
}

// fairAdmission applies fair-mode steps 1-3 and returns a rejection reason,
// or "" when the request may proceed.
func (s *Scheduler) fairAdmission(t *tenant, r *Request, now time.Time, saturated bool) string {
	tp := s.pol.Tenants[r.Tenant]
	if r.Priority.Critical() && tp.CriticalSlots > 0 && t.criticalActive >= tp.CriticalSlots {
		r.EffectivePriority = model.PriorityStandard
	}
	if !r.Priority.Sheddable() {
		return ""
	}
	if tp.Shed && (saturated || s.queued > 0) {
		return ReasonOverShare
	}
	if r.Deadline.IsZero() || t.queued == 0 {
		return ""
	}
	rate := s.dispatchRate(t, now)
	if rate > 0 && float64(t.queued+1)/rate > r.Deadline.Sub(now).Seconds() {
		return ReasonDeadlineUnreachable
	}
	return ""
}

// dispatchRate returns the tenant's dispatches per second over the
// rateWindow ending at now.
func (s *Scheduler) dispatchRate(t *tenant, now time.Time) float64 {
	t.pruneDispatches(now)
	return float64(len(t.dispatches)) / rateWindow.Seconds()
}

// pruneDispatches drops dispatch times at or before now-rateWindow, so the
// remaining ones lie in (now-rateWindow, now].
func (t *tenant) pruneDispatches(now time.Time) {
	cut := now.Add(-rateWindow)
	i := 0
	for i < len(t.dispatches) && !t.dispatches[i].After(cut) {
		i++
	}
	t.dispatches = t.dispatches[i:]
}

// lift is the VTC counter lift for a tenant t whose queue is empty and which
// is about to queue a request, so it cannot claim service for the time it
// was not backlogged. Its counter is raised to the smallest counter among
// tenants with queued work, or, when no tenant has queued work, to the
// current counter of the tenant whose queue emptied last. Tenants with only
// in-flight work are not a reference: their counters stopped tracking the
// fair frontier when their queues emptied, so a long-running request would
// otherwise let a returning tenant monopolise the pool. It never lowers a
// counter.
func (s *Scheduler) lift(t *tenant) {
	var ref *tenant
	for _, name := range s.names {
		if o := s.tenants[name]; o.queued > 0 && (ref == nil || o.counter < ref.counter) {
			ref = o
		}
	}
	if ref == nil {
		ref = s.lastDrained
	}
	if ref != nil {
		t.counter = math.Max(t.counter, ref.counter)
	}
}

// enqueue queues an admitted request.
func (s *Scheduler) enqueue(t *tenant, r *Request) {
	r.owner, r.ten, r.state = s, t, stateQueued
	r.EstOutputTokens, r.Dispatched = 0, time.Time{}
	r.weight, r.chargedOut = 0, 0
	s.queueFor(t, r).add(r)
	t.queued++
	s.queued++
	t.admitted++
	if r.EffectivePriority.Critical() {
		t.criticalActive++
	}
}

// queueFor returns the queue that holds r in the current mode.
func (s *Scheduler) queueFor(t *tenant, r *Request) *queue {
	switch s.cfg.Mode {
	case ModeRoundRobin:
		return t.bands[band(r.EffectivePriority)]
	case ModeFCFS:
		return s.fifo
	}
	return t.fair
}

// retryAfter returns the jittered Retry-After for a rejection.
func (s *Scheduler) retryAfter(tenant string) time.Duration {
	base := time.Duration(max(s.pol.Tenants[tenant].RetryAfterSeconds, 1)) * time.Second
	return time.Duration(float64(base) * (1 + s.cfg.RetryJitter*s.rng.Float64()))
}
