package gateway

import (
	"maps"
	"slices"
	"sync"
	"time"

	"tenantscaler/internal/eval"
	"tenantscaler/internal/fairq"
	"tenantscaler/internal/model"
)

// Stats is the gateway's state as served by GET /stats: what the
// controller reads every cycle and what the evaluation samples every second.
type Stats struct {
	Time time.Time `json:"time"`
	Mode string    `json:"mode"`
	// WindowSeconds is the observation window of Tenants: the requested
	// window, shortened to the gateway's uptime (at least 1 s) while the
	// gateway is younger than it, so that rates are not diluted by time
	// before the gateway existed.
	WindowSeconds float64 `json:"windowSeconds"`
	// PolicyVersion is the policy in force; PolicyLatencyMs is its
	// propagation latency (receive time minus IssuedAt).
	PolicyVersion   int64   `json:"policyVersion"`
	PolicyLatencyMs float64 `json:"policyLatencyMs"`
	// Endpoints is the number of ready replicas; Slots is Endpoints times
	// the dispatch window (in passthrough mode: the replicas' concurrent
	// capacity, which that mode does not enforce).
	Endpoints int `json:"endpoints"`
	Slots     int `json:"slots"`
	// InFlight and Queued are current totals over all tenants.
	InFlight int `json:"inFlight"`
	Queued   int `json:"queued"`
	// Tenants are the windowed per-tenant observations, sorted by tenant.
	// Completed, SLOAttainment and P99TTFTSeconds include the admitted
	// requests that failed (see Failed) as SLO misses.
	Tenants []model.TenantObservation `json:"tenants"`
	// Failed counts, per known tenant, the admitted requests that failed in
	// the window: no replica slot within the dispatch timeout, or an
	// upstream failure. Requests the client canceled, or that shutdown or
	// tenant removal cut off, are neither failed nor completed.
	Failed map[string]int `json:"failed"`
	// Scheduler is the scheduler state per tenant (virtual token counters
	// in fair mode), sorted by tenant.
	Scheduler []fairq.TenantState `json:"scheduler"`
}

// buffer is a FIFO of timestamped events: appended at the back, pruned at
// the front. It reuses its backing array, compacting once the dead prefix
// is at least half of it, so steady use does not grow memory.
type buffer[T any] struct {
	items []T
	head  int
}

func (b *buffer[T]) push(v T) { b.items = append(b.items, v) }

// pruneWhile drops events from the front while old reports true.
func (b *buffer[T]) pruneWhile(old func(T) bool) {
	for b.head < len(b.items) && old(b.items[b.head]) {
		var zero T
		b.items[b.head] = zero
		b.head++
	}
	if b.head > 0 && b.head*2 >= len(b.items) {
		n := copy(b.items, b.items[b.head:])
		clear(b.items[n:])
		b.items = b.items[:n]
		b.head = 0
	}
}

// live returns the events still held, oldest first.
func (b *buffer[T]) live() []T { return b.items[b.head:] }

type arrivalEvent struct {
	at       time.Time
	weighted float64
}

type completionEvent struct {
	at   time.Time
	ttft time.Duration
	met  bool
	// failed marks a request that failed (an SLO miss) instead of
	// completing.
	failed bool
}

// tenantEvents holds one tenant's events of the last MaxStatsWindow.
type tenantEvents struct {
	arrivals buffer[arrivalEvent]
	// sheds are rejected arrivals, with their weighted tokens.
	sheds       buffer[arrivalEvent]
	completions buffer[completionEvent]
}

// prune drops events at or before now-MaxStatsWindow.
func (e *tenantEvents) prune(now time.Time) {
	cut := now.Add(-MaxStatsWindow)
	e.arrivals.pruneWhile(func(a arrivalEvent) bool { return !a.at.After(cut) })
	e.sheds.pruneWhile(func(a arrivalEvent) bool { return !a.at.After(cut) })
	e.completions.pruneWhile(func(c completionEvent) bool { return !c.at.After(cut) })
}

// observations keeps per-tenant event buffers for windowed statistics. It
// is safe for concurrent use.
type observations struct {
	mu      sync.Mutex
	tenants map[string]*tenantEvents
}

func newObservations() *observations {
	return &observations{tenants: map[string]*tenantEvents{}}
}

// eventsLocked returns the tenant's buffers, creating them on first use.
func (o *observations) eventsLocked(tenant string) *tenantEvents {
	e, ok := o.tenants[tenant]
	if !ok {
		e = &tenantEvents{}
		o.tenants[tenant] = e
	}
	return e
}

// arrival records an arriving request (admitted or shed) and its weighted
// tokens.
func (o *observations) arrival(tenant string, at time.Time, weighted float64) {
	o.mu.Lock()
	defer o.mu.Unlock()
	e := o.eventsLocked(tenant)
	e.arrivals.push(arrivalEvent{at: at, weighted: weighted})
	e.prune(at)
}

// shed records a rejected request and its weighted tokens.
func (o *observations) shed(tenant string, at time.Time, weighted float64) {
	o.mu.Lock()
	defer o.mu.Unlock()
	e := o.eventsLocked(tenant)
	e.sheds.push(arrivalEvent{at: at, weighted: weighted})
	e.prune(at)
}

// completed records a completed request, its TTFT and its SLO outcome.
func (o *observations) completed(tenant string, at time.Time, ttft time.Duration, met bool) {
	o.record(tenant, completionEvent{at: at, ttft: ttft, met: met})
}

// failed records an admitted request that failed at `at`: an SLO miss with
// its TTFT, or the time until it failed when no token was relayed.
func (o *observations) failed(tenant string, at time.Time, ttft time.Duration) {
	o.record(tenant, completionEvent{at: at, ttft: ttft, failed: true})
}

func (o *observations) record(tenant string, c completionEvent) {
	o.mu.Lock()
	defer o.mu.Unlock()
	e := o.eventsLocked(tenant)
	e.completions.push(c)
	e.prune(c.at)
}

// observe returns the tenant's observation over the window (now-w, now]
// and how many of its completed requests failed. QueueLength and InFlight
// are left for the caller, which knows the live counts. Failed requests
// count as completed SLO misses. P99TTFTSeconds is the nearest-rank p99
// (eval.Percentile) of the completed requests, 0 when none completed.
func (o *observations) observe(tenant string, now time.Time, w time.Duration) (ob model.TenantObservation, failed int) {
	ob.Tenant = tenant
	o.mu.Lock()
	defer o.mu.Unlock()
	e, ok := o.tenants[tenant]
	if !ok {
		return ob, 0
	}
	e.prune(now)
	from := now.Add(-w)
	in := func(t time.Time) bool { return t.After(from) && !t.After(now) }
	var tokens float64
	arrivals := 0
	for _, a := range e.arrivals.live() {
		if in(a.at) {
			arrivals++
			tokens += a.weighted
		}
	}
	shedTokens := 0.0
	for _, s := range e.sheds.live() {
		if in(s.at) {
			ob.Shed++
			shedTokens += s.weighted
		}
	}
	met := 0
	var ttfts []float64
	for _, c := range e.completions.live() {
		if !in(c.at) {
			continue
		}
		ob.Completed++
		ttfts = append(ttfts, c.ttft.Seconds())
		if c.met {
			met++
		}
		if c.failed {
			failed++
		}
	}
	secs := w.Seconds()
	ob.ArrivalWeightedTokensPerSec = tokens / secs
	ob.AdmittedWeightedTokensPerSec = max(tokens-shedTokens, 0) / secs
	ob.ArrivalRequestsPerSec = float64(arrivals) / secs
	if ob.Completed > 0 {
		ob.HasSLOData = true
		ob.SLOAttainment = float64(met) / float64(ob.Completed)
		ob.P99TTFTSeconds = eval.Percentile(ttfts, 99)
	}
	return ob, failed
}

// minEffectiveWindow is the shortest window Stats divides by right after
// the gateway started, so the first requests do not report huge rates.
const minEffectiveWindow = time.Second

// Stats returns the gateway state with per-tenant observations over the
// window ending now; window <= 0 means Config.StatsWindow, and it is capped
// at MaxStatsWindow and at the gateway's uptime (at least 1 s).
func (g *Gateway) Stats(window time.Duration) Stats {
	if window <= 0 {
		window = g.cfg.StatsWindow
	}
	now := g.now()
	window = min(window, MaxStatsWindow, max(now.Sub(g.start), minEffectiveWindow))
	g.mu.Lock()
	st := Stats{
		Time: now, Mode: g.cfg.Mode, WindowSeconds: window.Seconds(),
		PolicyVersion: g.pol.Version, PolicyLatencyMs: ms(g.polLat),
		Endpoints: len(g.endpoints), Slots: len(g.endpoints) * g.cfg.SlotsPerReplica,
	}
	names := slices.Sorted(maps.Keys(g.tenants))
	live := make(map[string]tenantCounts, len(g.counts))
	for t, c := range g.counts {
		live[t] = *c
		st.InFlight += c.inflight
		st.Queued += c.queued
	}
	if g.sched != nil {
		st.Scheduler = g.sched.Snapshot()
	}
	for _, t := range slices.Sorted(maps.Keys(g.parts)) {
		st.Scheduler = append(st.Scheduler, g.parts[t].Snapshot()...)
	}
	g.mu.Unlock()

	st.Tenants = make([]model.TenantObservation, 0, len(names))
	st.Failed = make(map[string]int, len(names))
	for _, t := range names {
		ob, failed := g.obs.observe(t, now, window)
		ob.QueueLength, ob.InFlight = live[t].queued, live[t].inflight
		st.Tenants = append(st.Tenants, ob)
		st.Failed[t] = failed
	}
	return st
}
