package workload

import (
	"cmp"
	"hash/fnv"
	"math"
	"math/rand/v2"
	"slices"

	"tenantscaler/internal/model"
)

// Generate produces the scenario's requests sorted by Offset (ties keep
// tenant order). It is deterministic: the same scenario and seed always
// give the same specs.
//
// Arrivals of each tenant form a non-homogeneous Gamma renewal process: gaps
// are Gamma with shape 1/CV^2 and mean 1 in "operational time", which is
// mapped to real time through the cumulative rate Λ(t) = ∫ RateAt(p, u) du
// (time rescaling). The arrival rate therefore follows RateAt while the gaps
// keep the requested burstiness; CV = 1 gives a Poisson process.
//
// Each tenant draws from its own random streams for arrivals, priorities
// and lengths, keyed by seed and tenant name, so changing one tenant or the
// length source does not move any other tenant's arrivals.
//
// Every request produces at least its first token, so a sample with
// Out = 0 (valid; LoadAzure keeps such rows) is replayed with one output
// token.
//
// Generate does not validate s (see Validate). For an invalid scenario it
// still returns a deterministic trace: CV <= 0 counts as 1, a mix with no
// positive weight yields standard requests, lengths are clamped to
// [max(Min, 1), max(Max, Min)] and sampled lengths to In >= 0, Out >= 1.
func Generate(s Scenario, seed int64) []Spec {
	d := s.Duration.Seconds()
	var specs []Spec
	for _, p := range s.Tenants {
		arrivalRNG := stream(seed, p.Tenant, "arrivals")
		priorityRNG := stream(seed, p.Tenant, "priority")
		lengthRNG := stream(seed, p.Tenant, "lengths")
		for _, t := range arrivals(segments(p, d), shape(p.CV), arrivalRNG) {
			off := seconds(t)
			if off >= s.Duration {
				break
			}
			in, out := lengths(p, s.Samples, lengthRNG)
			specs = append(specs, Spec{
				Offset:       off,
				Tenant:       p.Tenant,
				Priority:     samplePriority(p.Mix, priorityRNG),
				InputTokens:  in,
				OutputTokens: out,
			})
		}
	}
	slices.SortStableFunc(specs, func(a, b Spec) int { return cmp.Compare(a.Offset, b.Offset) })
	return specs
}

// stream returns an independent, deterministic random source for one
// tenant and purpose.
func stream(seed int64, tenant, purpose string) *rand.Rand {
	h := fnv.New64a()
	h.Write([]byte(tenant))
	h.Write([]byte{0})
	h.Write([]byte(purpose))
	return rand.New(rand.NewPCG(uint64(seed), h.Sum64()))
}

// shape converts an inter-arrival CV to the Gamma shape 1/CV^2.
func shape(cv float64) float64 {
	if !finite(cv) || cv <= 0 {
		cv = 1
	}
	return 1 / (cv * cv)
}

// maxBursts caps how many periodic bursts segments resolves in one run.
const maxBursts = 1 << 20

// segment is an interval of constant rate.
type segment struct{ start, end, rate float64 }

func (g segment) mass() float64 { return g.rate * (g.end - g.start) }

// segments splits [0, d) at every point where the pattern's rate can
// change and evaluates the rate on each piece.
func segments(p TenantPattern, d float64) []segment {
	if !(d > 0) {
		return nil
	}
	cuts := []float64{0, d}
	switch {
	case p.Kind == KindPeriodic && hasBursts(p):
		// Bounded so that a pathological period (Validate requires >= 1 s)
		// cannot loop forever or exhaust memory.
		k := math.Floor(-p.PhaseSeconds / p.PeriodSeconds)
		for n := 0; n < maxBursts; n++ {
			start := p.PhaseSeconds + (k+float64(n))*p.PeriodSeconds
			if !(start < d) {
				break
			}
			cuts = append(cuts, start, start+p.BurstSeconds)
		}
	case p.Kind == KindSpike && hasSpike(p):
		cuts = append(cuts, p.SpikeStartSeconds, p.SpikeStartSeconds+p.SpikeDurationSeconds)
	case p.Kind == KindSeries && finite(p.SeriesBucketSeconds) && p.SeriesBucketSeconds > 0:
		for i := 1; i <= len(p.Series) && float64(i)*p.SeriesBucketSeconds < d; i++ {
			cuts = append(cuts, float64(i)*p.SeriesBucketSeconds)
		}
	}
	slices.Sort(cuts)
	var segs []segment
	for i := 1; i < len(cuts); i++ {
		a, b := max(cuts[i-1], 0), min(cuts[i], d)
		if b > a {
			segs = append(segs, segment{start: a, end: b, rate: RateAt(p, (a+b)/2)})
		}
	}
	return segs
}

// arrivals returns arrival times (seconds) of a renewal process whose gaps
// are Gamma(k, 1/k) in operational time, mapped to real time by inverting
// the cumulative rate over segs.
func arrivals(segs []segment, k float64, r *rand.Rand) []float64 {
	var out []float64
	i := 0
	cum := 0.0 // cumulative rate at segs[i].start
	s := 0.0   // operational time of the next arrival
	for {
		s += sampleGamma(r, k) / k
		for i < len(segs) && cum+segs[i].mass() <= s {
			cum += segs[i].mass()
			i++
		}
		if i == len(segs) {
			return out
		}
		// s >= cum always holds, so a zero-rate segment (mass 0) is skipped
		// by the loop above and rate > 0 here.
		out = append(out, segs[i].start+(s-cum)/segs[i].rate)
	}
}

// sampleGamma draws from Gamma(k, 1) using Marsaglia and Tsang (2000); for
// k < 1 it uses the boost Gamma(k) = Gamma(k+1) * U^(1/k).
func sampleGamma(r *rand.Rand, k float64) float64 {
	if k < 1 {
		u := 1 - r.Float64() // (0, 1]
		return sampleGamma(r, k+1) * math.Pow(u, 1/k)
	}
	d := k - 1.0/3
	c := 1 / math.Sqrt(9*d)
	for {
		x := r.NormFloat64()
		v := 1 + c*x
		if v <= 0 {
			continue
		}
		v = v * v * v
		u := 1 - r.Float64()
		if u < 1-0.0331*x*x*x*x || math.Log(u) < 0.5*x*x+d*(1-v+math.Log(v)) {
			return d * v
		}
	}
}

// samplePriority picks a class with probability proportional to its weight.
func samplePriority(m PriorityMix, r *rand.Rand) model.Priority {
	w := func(v float64) float64 {
		if !finite(v) || v < 0 {
			return 0
		}
		return v
	}
	c, st, sh := w(m.Critical), w(m.Standard), w(m.Sheddable)
	total := c + st + sh
	if total <= 0 {
		return model.PriorityStandard
	}
	u := r.Float64() * total
	switch {
	case u < c:
		return model.PriorityCritical
	case u < c+st:
		return model.PriorityStandard
	default:
		return model.PrioritySheddable
	}
}

// lengths draws one request's input and output token counts.
func lengths(p TenantPattern, samples []LengthSample, r *rand.Rand) (in, out int) {
	if len(samples) > 0 {
		smp := samples[r.IntN(len(samples))]
		return max(smp.In, 0), max(smp.Out, 1)
	}
	return p.In.sample(r), p.Out.sample(r)
}

// sample draws one clamped log-normal length.
func (d LengthDist) sample(r *rand.Rand) int {
	lo := max(d.Min, 1)
	hi := max(d.Max, lo)
	v := d.Median * math.Exp(d.Sigma*r.NormFloat64())
	switch {
	case !(v >= float64(lo)): // also catches NaN
		return lo
	case v >= float64(hi):
		return hi
	default:
		return min(max(int(math.Round(v)), lo), hi)
	}
}
