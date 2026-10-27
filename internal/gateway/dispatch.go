package gateway

import (
	"context"
	"errors"
	"maps"
	"slices"
	"time"

	"tenantscaler/internal/fairq"
)

// downFor is how long an endpoint that refused a connection or answered
// 503 is skipped, so requests do not chase a replica that is shutting down
// before discovery drops it.
const downFor = 2 * time.Second

// Errors returned by admit and waitDispatch.
var (
	errDispatchTimeout = errors.New("no replica slot became free within the dispatch timeout")
	errClientGone      = errors.New("client went away while queued")
	errShutdown        = errors.New("gateway is shutting down")
	errTenantRemoved   = errors.New("the tenant was removed")
)

// pending is an admitted request waiting for, or holding, a replica slot.
type pending struct {
	req    *fairq.Request
	sched  *fairq.Scheduler // the scheduler that admitted req
	tenant string
	// ready receives the endpoint once at dispatch, or "" when the request
	// is withdrawn (buffered, never blocks).
	ready chan string
	// endpoint is the slot's endpoint; guarded by Gateway.mu.
	endpoint string
}

// run is the dispatcher goroutine: on every wake-up (arrival, completion,
// endpoint change, policy change) or safety tick it refreshes the endpoint
// set and dispatches queued requests into free slots.
func (g *Gateway) run() {
	defer close(g.done)
	t := time.NewTicker(tickEvery)
	defer t.Stop()
	for {
		g.refreshEndpoints()
		g.mu.Lock()
		g.dispatchLocked(g.now())
		g.mu.Unlock()
		select {
		case <-g.ctx.Done():
			return
		case <-g.wake:
		case <-t.C:
		}
	}
}

// refreshEndpoints reads the discovery and installs a changed endpoint set.
func (g *Gateway) refreshEndpoints() {
	eps := normalizeEndpoints(g.cfg.Discovery.Endpoints())
	g.mu.Lock()
	if slices.Equal(eps, g.endpoints) {
		g.mu.Unlock()
		return
	}
	g.endpoints = eps
	if g.cfg.Mode == ModePartition {
		g.owners = partition(eps, g.order, g.sharesLocked())
	}
	g.mu.Unlock()
	g.metrics.endpoints.Set(float64(len(eps)))
	g.log.Info("endpoints changed", "endpoints", eps)
}

// dispatchLocked hands queued requests to free slots until no request or
// no eligible slot is left.
func (g *Gateway) dispatchLocked(now time.Time) {
	if g.cfg.Mode != ModePartition {
		for g.sched.Len() > 0 {
			ep := g.pickLocked(g.endpoints, now, "")
			if ep == "" {
				return
			}
			r := g.sched.NextWithin(now, g.freeSlotsLocked(now))
			if r == nil {
				return // the free slots are reserved for other tenants' shares
			}
			g.assignLocked(r, ep)
		}
		return
	}
	// Partition: every tenant drains its FIFO onto its own endpoints only.
	tenants := slices.Sorted(maps.Keys(g.parts))
	for progress := true; progress; {
		progress = false
		for _, t := range tenants {
			s := g.parts[t]
			if s.Len() == 0 {
				continue
			}
			if ep := g.pickLocked(g.owners[t], now, ""); ep != "" {
				g.assignLocked(s.Next(now), ep)
				progress = true
			}
		}
	}
}

// freeSlotsLocked counts the free dispatch slots on endpoints not marked
// down.
func (g *Gateway) freeSlotsLocked(now time.Time) int {
	n := 0
	for _, ep := range g.endpoints {
		if until, down := g.down[ep]; down && now.Before(until) {
			continue
		}
		n += max(0, g.cfg.SlotsPerReplica-g.load[ep])
	}
	return n
}

// pickLocked chooses an endpoint among cands, skipping exclude and
// endpoints marked down. Passthrough takes the next one round robin with
// no limit; the other modes take the least-loaded endpoint with a free
// slot in the dispatch window (ties: lowest URL), or "" when none is free.
func (g *Gateway) pickLocked(cands []string, now time.Time, exclude string) string {
	usable := func(ep string) bool {
		if ep == exclude {
			return false
		}
		until, down := g.down[ep]
		if down && !now.Before(until) {
			delete(g.down, ep)
			down = false
		}
		return !down
	}
	if g.cfg.Mode == ModePassthrough {
		n := len(cands)
		for i := range n {
			if ep := cands[(g.rr+i)%n]; usable(ep) {
				g.rr = (g.rr + i + 1) % n
				return ep
			}
		}
		return ""
	}
	best, bestLoad := "", g.cfg.SlotsPerReplica
	for _, ep := range cands {
		if l := g.load[ep]; l < bestLoad && usable(ep) {
			best, bestLoad = ep, l
		}
	}
	return best
}

// assignLocked gives the dispatched request r the endpoint's slot and wakes
// its handler.
func (g *Gateway) assignLocked(r *fairq.Request, ep string) {
	p := g.pending[r]
	delete(g.pending, r)
	p.endpoint = ep
	g.load[ep]++
	c := g.countsLocked(p.tenant)
	c.queued--
	c.inflight++
	g.metrics.queue.WithLabelValues(p.tenant).Set(float64(c.queued))
	g.metrics.inflight.WithLabelValues(p.tenant).Set(float64(c.inflight))
	p.ready <- ep
}

// admit runs the scheduler's admission for req and, when admitted,
// registers it as pending. The caller then kicks the dispatcher. It returns
// errTenantRemoved when the tenant was removed after the caller looked it
// up.
func (g *Gateway) admit(req *fairq.Request) (*pending, fairq.Verdict, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if _, ok := g.tenants[req.Tenant]; !ok {
		return nil, fairq.Verdict{}, errTenantRemoved
	}
	s := g.sched
	if g.cfg.Mode == ModePartition {
		s = g.partitionSchedulerLocked(req.Tenant)
	}
	// Roundrobin sheds sheddable traffic while no slot is free; fair mode
	// sheds an over-share tenant's sheddable traffic only then (or while
	// requests are queued).
	shedsWhenFull := g.cfg.Mode == ModeRoundRobin || g.cfg.Mode == ModeFair
	saturated := shedsWhenFull && g.pickLocked(g.endpoints, req.Arrival, "") == ""
	v := s.Admit(req, req.Arrival, saturated)
	if !v.Admitted {
		return nil, v, nil
	}
	p := &pending{req: req, sched: s, tenant: req.Tenant, ready: make(chan string, 1)}
	g.pending[req] = p
	c := g.countsLocked(req.Tenant)
	c.queued++
	g.metrics.queue.WithLabelValues(req.Tenant).Set(float64(c.queued))
	return p, v, nil
}

// waitDispatch blocks until p is dispatched and returns its endpoint. If
// the client goes away, the dispatch timeout expires or the gateway closes
// first, the request is withdrawn from the queue and an error returned.
// A request dispatched at that very moment keeps its slot, except when the
// client is gone. A request withdrawn by withdrawLocked returns
// errTenantRemoved.
func (g *Gateway) waitDispatch(ctx context.Context, p *pending) (string, error) {
	timer := time.NewTimer(g.cfg.DispatchTimeout)
	defer timer.Stop()
	var cause error
	select {
	case ep := <-p.ready:
		if ep == "" {
			return "", errTenantRemoved
		}
		return ep, nil
	case <-ctx.Done():
		cause = errClientGone
	case <-timer.C:
		cause = errDispatchTimeout
	case <-g.ctx.Done():
		cause = errShutdown
	}
	g.mu.Lock()
	if g.unqueueLocked(p) {
		g.mu.Unlock()
		return "", cause
	}
	g.mu.Unlock()
	ep := <-p.ready // dispatched or withdrawn concurrently: already sent
	switch {
	case ep == "":
		return "", errTenantRemoved
	case cause != errClientGone:
		return ep, nil
	}
	g.finish(p, 0)
	return "", cause
}

// unqueueLocked removes p from its scheduler's queue and the gateway's
// bookkeeping, and reports whether it was still queued.
func (g *Gateway) unqueueLocked(p *pending) bool {
	if !p.sched.Cancel(p.req) {
		return false
	}
	delete(g.pending, p.req)
	c := g.countsLocked(p.tenant)
	c.queued--
	g.metrics.queue.WithLabelValues(p.tenant).Set(float64(c.queued))
	return true
}

// withdrawLocked removes every request still queued in s and tells each
// waiting handler so (errTenantRemoved).
func (g *Gateway) withdrawLocked(s *fairq.Scheduler) {
	for _, p := range g.pending {
		if p.sched == s && g.unqueueLocked(p) {
			p.ready <- ""
		}
	}
}

// reroute moves p's slot from the failed endpoint to another free one in
// its candidate set, marking the failed endpoint down. It reports false
// when no other slot is free; p then keeps its original slot.
func (g *Gateway) reroute(p *pending, failed string) (string, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	now := g.now()
	g.down[failed] = now.Add(downFor)
	cands := g.endpoints
	if g.cfg.Mode == ModePartition {
		cands = g.owners[p.tenant]
	}
	next := g.pickLocked(cands, now, failed)
	if next == "" {
		return "", false
	}
	g.releaseSlotLocked(failed)
	g.load[next]++
	p.endpoint = next
	return next, true
}

// finish completes p in its scheduler with the produced output tokens and
// frees its slot.
func (g *Gateway) finish(p *pending, outputTokens int) {
	g.mu.Lock()
	p.sched.Complete(p.req, outputTokens, g.now())
	g.releaseSlotLocked(p.endpoint)
	c := g.countsLocked(p.tenant)
	c.inflight--
	g.metrics.inflight.WithLabelValues(p.tenant).Set(float64(c.inflight))
	g.mu.Unlock()
	g.kick()
}

// releaseSlotLocked frees one slot of ep.
func (g *Gateway) releaseSlotLocked(ep string) {
	if g.load[ep]--; g.load[ep] <= 0 {
		delete(g.load, ep)
	}
}

// onTokens charges the output tokens produced so far (fair mode).
func (g *Gateway) onTokens(p *pending, produced int) {
	g.mu.Lock()
	p.sched.OnOutputTokens(p.req, produced)
	g.mu.Unlock()
}

// partition assigns endpoints (sorted) to tenants (in order): tenant i
// owns the next shares[i] endpoints, as far as they reach, and leftover
// endpoints are dealt round robin in tenant order.
func partition(endpoints, tenants []string, shares map[string]int) map[string][]string {
	out := make(map[string][]string, len(tenants))
	i := 0
	for _, t := range tenants {
		end := min(i+max(shares[t], 0), len(endpoints))
		out[t] = slices.Clone(endpoints[i:end])
		i = end
	}
	if len(tenants) == 0 {
		return out
	}
	for j, ep := range endpoints[i:] {
		t := tenants[j%len(tenants)]
		out[t] = append(out[t], ep)
	}
	return out
}
