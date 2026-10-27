package sim

import (
	"math"
	"slices"
	"time"

	"tenantscaler/internal/eval"
	"tenantscaler/internal/fairq"
	"tenantscaler/internal/model"
	"tenantscaler/internal/workload"
)

// reqState is where a request is.
type reqState uint8

const (
	stNew      reqState = iota // not arrived yet
	stGateway                  // held at the gateway
	stWaiting                  // in a replica's FCFS queue
	stRunning                  // holding a replica slot
	stDone                     // completed
	stShed                     // rejected by the gateway
	stTimedOut                 // no replica slot within DispatchTimeout (live: 503)
)

// request is one generated request and its fate.
type request struct {
	id   int
	spec workload.Spec
	ts   *tenantState
	// fq and sched are set when a fairq scheduler admitted the request.
	fq    *fairq.Request
	sched *fairq.Scheduler
	rep   *replica

	state                     reqState
	started, firstToken, done time.Duration
	// interval is the inter-token time fixed when the request started.
	interval   time.Duration
	shedReason string
}

// produced returns how many output tokens req has streamed by now: none
// before it runs, the first at firstToken, then one per interval, as the
// live gateway counts SSE chunks.
func (req *request) produced(now time.Duration) int {
	out := req.spec.OutputTokens
	switch {
	case req.state != stRunning || now < req.firstToken:
		return 0
	case req.interval <= 0:
		return out
	}
	return min(out, 1+int((now-req.firstToken)/req.interval))
}

// setState moves req to st, keeping the tenant's counters in step.
func (req *request) setState(st reqState) {
	req.ts.count(req.state, -1)
	req.state = st
	req.ts.count(st, 1)
}

// record returns the request's eval.Record. A request that timed out in
// the gateway queue, or was neither shed nor completed when the run
// stopped, is Failed.
func (req *request) record() eval.Record {
	r := eval.Record{
		ID: uint64(req.id), Tenant: req.spec.Tenant, Priority: req.spec.Priority,
		Arrival:     req.spec.Offset,
		InputTokens: req.spec.InputTokens, OutputTokens: req.spec.OutputTokens,
	}
	switch req.state {
	case stShed:
		r.Shed, r.ShedReason = true, req.shedReason
	case stDone:
		r.FirstToken, r.Done = req.firstToken, req.done
	default:
		r.Failed = true
	}
	return r
}

// tenantState holds a tenant's live counters and its window statistics
// since the last control tick.
type tenantState struct {
	name string
	slo  model.SLO
	// gwQueued are held at the gateway, waiting in a replica queue and
	// running in a replica slot.
	gwQueued, waiting, running int

	winArrivals   int
	winWeighted   float64
	winShedWeight float64
	winShed       int
	winTTFT       []float64
	winMet        int
	winCompleted  int
}

func (t *tenantState) count(st reqState, d int) {
	switch st {
	case stGateway:
		t.gwQueued += d
	case stWaiting:
		t.waiting += d
	case stRunning:
		t.running += d
	}
}

// noteArrival counts an arrival, shed or not, with its weighted tokens
// (input + 2 x max output), as the live gateway does.
func (t *tenantState) noteArrival(sp workload.Spec) {
	t.winArrivals++
	t.winWeighted += model.WeightedTokens(float64(sp.InputTokens), float64(sp.OutputTokens))
}

func (t *tenantState) resetWindow() {
	t.winArrivals, t.winWeighted, t.winShed, t.winShedWeight = 0, 0, 0, 0
	t.winTTFT, t.winMet, t.winCompleted = t.winTTFT[:0], 0, 0
}

// shed records a gateway rejection.
func (s *simulator) shed(req *request, reason string) {
	req.setState(stShed)
	req.shedReason = reason
	req.ts.winShed++
	req.ts.winShedWeight += model.WeightedTokens(float64(req.spec.InputTokens), float64(req.spec.OutputTokens))
	s.resolved++
}

// complete records a finished request and tells its scheduler.
func (s *simulator) complete(req *request) {
	req.setState(stDone)
	s.resolved++
	t := req.ts
	rec := req.record()
	t.winCompleted++
	t.winTTFT = append(t.winTTFT, rec.TTFT().Seconds())
	if rec.MetSLO(t.slo) {
		t.winMet++
	}
	if req.sched != nil {
		req.sched.Complete(req.fq, req.spec.OutputTokens, s.clock())
	}
	if i := slices.Index(s.charging, req); i >= 0 {
		s.charging = slices.Delete(s.charging, i, i+1)
	}
}

// observe builds the controller's observation over the window since the
// last tick, like the live gateway's /stats: arrivals (shed included) as
// weighted tokens/s, the current gateway queue and forwarded in-flight
// requests, and attainment / p99 TTFT / shed count of the window.
func (s *simulator) observe() model.Observation {
	win := (s.now - s.lastTick).Seconds()
	ready, starting := 0, 0
	for _, r := range s.reps {
		switch r.state {
		case repReady:
			ready++
		case repStarting, repPending:
			starting++
		}
	}
	obs := model.Observation{
		Time: s.clock(), OK: true,
		Pool: model.PoolObservation{ReadyReplicas: ready, StartingReplicas: starting, DesiredReplicas: s.target},
	}
	for _, t := range s.tenants {
		o := model.TenantObservation{
			Tenant: t.name, QueueLength: t.gwQueued, InFlight: t.waiting + t.running,
			Completed: t.winCompleted, Shed: t.winShed,
		}
		if win > 0 {
			o.ArrivalWeightedTokensPerSec = t.winWeighted / win
			o.AdmittedWeightedTokensPerSec = max(t.winWeighted-t.winShedWeight, 0) / win
			o.ArrivalRequestsPerSec = float64(t.winArrivals) / win
		}
		if t.winCompleted > 0 {
			o.HasSLOData = true
			o.SLOAttainment = float64(t.winMet) / float64(t.winCompleted)
			o.P99TTFTSeconds = eval.Percentile(t.winTTFT, 99)
		}
		obs.Tenants = append(obs.Tenants, o)
	}
	return obs
}

// sample records the timeline (inside the scenario duration) and the slot
// occupancy the CPU-style HPA reads. Ready counts replicas that hold a GPU
// and serve (ready and draining); Starting counts loading replicas; pending
// replicas hold no GPU and count in neither. InFlight counts requests in a
// replica slot; Queued counts the rest still waiting, wherever they wait.
func (s *simulator) sample() {
	ready, starting, busy, serving := 0, 0, 0, 0
	for _, r := range s.reps {
		switch r.state {
		case repReady:
			ready++
			serving++
			busy += r.running
		case repDraining:
			ready++
		case repStarting:
			starting++
		}
	}
	if serving > 0 {
		// Same definition as the live HPA (internal/scalerun occupancy):
		// replicas that should exist but are not ready yet count as idle,
		// like not-ready pods in the Kubernetes replica calculator.
		n := max(s.target, serving+starting)
		s.occSum += float64(busy) / float64(n*s.params.MaxNumSeqs)
		s.occN++
	}
	if s.now >= s.o.Scenario.Duration {
		return
	}
	smp := eval.Sample{
		T: s.now, Ready: ready, Starting: starting, Desired: s.target,
		InFlight: map[string]int{}, Queued: map[string]int{}, CounterGap: math.NaN(),
	}
	for _, t := range s.tenants {
		smp.InFlight[t.name] = t.running
		smp.Queued[t.name] = t.gwQueued + t.waiting
	}
	if s.spec.gateway == GatewayFair {
		s.chargeTokens()
		smp.CounterGap = counterGap(s.sched.Snapshot())
	}
	s.timeline = append(s.timeline, smp)
}

// counterGap is the max-min VTC counter among tenants with queued work, or
// NaN when no tenant has any.
func counterGap(states []fairq.TenantState) float64 {
	lo, hi := math.Inf(1), math.Inf(-1)
	for _, st := range states {
		if st.Queued > 0 {
			lo, hi = math.Min(lo, st.Counter), math.Max(hi, st.Counter)
		}
	}
	if hi < lo {
		return math.NaN()
	}
	return hi - lo
}
