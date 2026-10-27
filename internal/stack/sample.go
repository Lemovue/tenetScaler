package stack

import (
	"math"
	"time"

	"tenantscaler/internal/eval"
	"tenantscaler/internal/fairq"
	"tenantscaler/internal/gateway"
)

// sampleWindow is the statistics window of the gateway read by Sample; the
// sample uses only the gateway's live counts, so it is kept short.
const sampleWindow = time.Second

// Sample returns the stack's state now as an evaluation sample, with T the
// time since start:
//   - Ready counts ready and draining replicas (both still hold their CPU
//     cores and serve, as the simulator counts them), Starting the
//     replicas loading the model, Desired the pool's desired count;
//   - InFlight and Queued are per tenant the gateway's forwarded and
//     queued requests. In passthrough mode (S2, S5) the gateway forwards
//     at once, and requests waiting inside a replica's own queue count as
//     InFlight: the replicas do not report their queue per tenant;
//   - CounterGap is, in fair mode (S6, S7), the largest difference of the
//     scheduler's virtual token counters among tenants with queued
//     requests (0 with one such tenant), NaN otherwise.
//
// It also records the cold starts of the replicas it sees (ColdStarts).
func (s *Stack) Sample(start time.Time) eval.Sample {
	now := time.Now()
	st := s.pool.Status()
	s.recordColdStarts(st)
	gs := s.gw.Stats(sampleWindow)
	smp := eval.Sample{
		T: now.Sub(start), Ready: st.Ready + st.Draining, Starting: st.Starting, Desired: st.Desired,
		InFlight: map[string]int{}, Queued: map[string]int{}, CounterGap: math.NaN(),
	}
	for _, o := range gs.Tenants {
		smp.InFlight[o.Tenant] = o.InFlight
		smp.Queued[o.Tenant] = o.QueueLength
	}
	if s.spec.gateway == gateway.ModeFair {
		smp.CounterGap = counterGap(gs.Scheduler)
	}
	return smp
}

// counterGap is the max-min virtual token counter among tenants with
// queued requests, or NaN when none has any (the simulator's definition).
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
