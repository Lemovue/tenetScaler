// Package estimate turns raw per-tenant observations into the two numbers the
// arbiter needs: how many replicas a tenant wants (demand) and how close it
// is to violating its latency SLO (slack).
package estimate

import (
	"math"

	"tenantscaler/internal/model"
)

// intTolerance is the largest excess over an integer k, relative to k, that
// Replicas treats as floating-point noise rather than real demand: k+e with
// 0 < e <= intTolerance*k gives k replicas, not k+1. Without it, 100*1.1/110
// (= 1.0000000000000002, one ULP above 1) would need 2 replicas instead of 1.
// It is well above the few ULPs Replicas' own arithmetic adds, so error
// accumulated upstream (smoothed rates, forecasts) is absorbed too, yet below
// a million replicas it swallows less than 1e-6 of a replica.
const intTolerance = 1e-12

// maxIntFloat is the smallest float64 that no longer fits in an int.
const maxIntFloat = float64(math.MaxInt)

// Replicas returns ceil(rate*(1+headroom)/capacityPerReplica): the number of
// replicas needed to serve weightedTokenRate weighted tokens per second while
// keeping a headroom fraction spare. It returns 0 when the rate is <= 0 (or
// NaN), when rate*(1+headroom) is <= 0, and when capacityPerReplica is not a
// positive number (config.Validate rejects such configurations). A positive
// rate always needs at least one replica, and results too large for an int
// saturate at math.MaxInt. A quotient exceeding an integer k >= 1 by at most
// intTolerance*k is floating-point noise and gives k.
func Replicas(weightedTokenRate, headroom, capacityPerReplica float64) int {
	if !(weightedTokenRate > 0) || !(capacityPerReplica > 0) {
		return 0
	}
	need := weightedTokenRate * (1 + headroom) / capacityPerReplica
	switch {
	case !(need > 0):
		return 0
	case need >= maxIntFloat:
		return math.MaxInt
	}
	n := math.Ceil(need)
	if k := math.Floor(need); k >= 1 && need-k <= intTolerance*k {
		n = k
	}
	return int(n)
}

// minAttainmentSpan keeps the attainment slack finite for a 100% target.
const minAttainmentSpan = 0.01

// Slack is how close the tenant is to violating its SLO, in [-1, 1]; lower
// is closer (negative means it is already violating). It is the smaller of
// two margins:
//   - latency: (SLO.TTFT - p99 TTFT) / SLO.TTFT;
//   - attainment: (attainment - target) / max(1 - target, 0.01), so a
//     tenant whose requests fail fast (tiny p99, attainment 0) still shows
//     pressure.
//
// When nothing completed in the window (HasSLOData is false) there is no
// latency to measure, so the queue decides: -1 if requests are waiting
// (starving), 1 otherwise (idle, comfortable). The same queue rule stands in
// for the latency margin when that is undefined (a NaN p99, or a zero TTFT
// bound with a zero p99).
func Slack(o model.TenantObservation, slo model.SLO) float64 {
	queue := 1.0
	if o.QueueLength > 0 {
		queue = -1
	}
	if !o.HasSLOData {
		return queue
	}
	lat := (slo.TTFTSeconds - o.P99TTFTSeconds) / slo.TTFTSeconds
	if math.IsNaN(lat) {
		lat = queue
	}
	s := lat
	if slo.TargetFraction > 0 { // the attainment margin needs a target
		att := (o.SLOAttainment - slo.TargetFraction) / math.Max(1-slo.TargetFraction, minAttainmentSpan)
		if !math.IsNaN(att) {
			s = math.Min(lat, att)
		}
	}
	return math.Max(-1, math.Min(1, s))
}
