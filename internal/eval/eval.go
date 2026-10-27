// Package eval turns the per-request records and per-second timeline of one
// evaluation run into the metrics TenantScaler is judged on (victim SLO
// violations during the spike, p99 TTFT/TPOT, shed %, reaction time,
// replica-minutes, Jain's index, service gap, reclaim latency, policy
// latency), summarizes repeated runs with a t-distribution 95% confidence
// interval, and writes CSV, Markdown and SVG results.
package eval

import (
	"cmp"
	"maps"
	"math"
	"slices"
	"time"

	"tenantscaler/internal/model"
)

// Record is the outcome of one request.
type Record struct {
	ID       uint64
	Tenant   string
	Priority model.Priority
	// Arrival is always set (offset from the run start). FirstToken and
	// Done are offsets from the run start too, and zero when the request
	// was shed or failed.
	Arrival, FirstToken, Done time.Duration
	Shed                      bool
	ShedReason                string
	Failed                    bool
	InputTokens, OutputTokens int
}

// Completed reports whether the request finished: it was neither shed nor
// failed and has a completion time.
func (r Record) Completed() bool { return !r.Shed && !r.Failed && r.Done > 0 }

// TTFT is the time to first token, FirstToken - Arrival.
func (r Record) TTFT() time.Duration { return r.FirstToken - r.Arrival }

// TPOT is the mean time per output token after the first,
// (Done - FirstToken) / (OutputTokens - 1); zero when OutputTokens <= 1.
func (r Record) TPOT() time.Duration {
	if r.OutputTokens <= 1 {
		return 0
	}
	return (r.Done - r.FirstToken) / time.Duration(r.OutputTokens-1)
}

// MetSLO reports whether the request completed and met slo
// (model.SLO.Met on its TTFT and TPOT).
func (r Record) MetSLO(slo model.SLO) bool {
	return r.Completed() && slo.Met(r.TTFT(), r.TPOT(), r.OutputTokens)
}

// Sample is the system state at one instant; runs record one per second.
type Sample struct {
	T                        time.Duration
	Ready, Starting, Desired int
	// InFlight counts each tenant's requests holding a replica slot (being
	// served). Queued counts its admitted requests still waiting for one,
	// wherever they wait: in the gateway queue or, when the gateway passes
	// requests straight through, in a replica's FCFS waiting queue.
	InFlight, Queued map[string]int
	// CounterGap is the max-min VTC counter among backlogged tenants (fair
	// mode), NaN otherwise.
	CounterGap float64
}

// RunData is everything recorded in one run.
type RunData struct {
	Setup string
	Run   int
	Seed  int64
	// Records holds every request; Timeline holds one Sample per second.
	Records  []Record
	Timeline []Sample
	// SLOs and GuaranteedSlots (guaranteed replicas x dispatch slots per
	// replica) are keyed by tenant.
	SLOs            map[string]model.SLO
	GuaranteedSlots map[string]int
	// Victims are the tenants whose traffic did not change; Noisy is the
	// spiking tenant. The spike window is [SpikeStart, SpikeEnd).
	Victims              []string
	Noisy                string
	SpikeStart, SpikeEnd time.Duration
	// ActionTimes are protective actions: desired replicas up, or a shed
	// policy applied.
	ActionTimes     []time.Duration
	PolicyLatencies []time.Duration
	// Owner and OwnerReturn describe the reclaim scenario; an empty Owner
	// or zero OwnerReturn means not applicable.
	Owner       string
	OwnerReturn time.Duration
}

// RunMetrics are the metrics of one run. Percentages are 0-100, times are
// seconds unless the name says otherwise, and NaN means "not defined for
// this run" (for example no requests in the window).
type RunMetrics struct {
	// VictimViolationPct is the share of the victims' non-shed requests
	// arriving in the spike window that missed the SLO; failed requests
	// count as misses, and so do unfinished ones that had certainly missed
	// it by the end of the run (see Compute). VictimShedAwareViolationPct
	// also counts shed requests, as misses.
	VictimViolationPct, VictimShedAwareViolationPct float64
	// P99TTFT and P99TPOT are per-tenant nearest-rank p99s over completed
	// requests (TPOT only for OutputTokens > 1), in seconds; NaN without data.
	P99TTFT, P99TPOT map[string]float64
	// NoisyShedPct is the share of the noisy tenant's requests that were
	// shed over the whole run.
	NoisyShedPct float64
	// ReactionTimeSec is the first ActionTime >= SpikeStart minus
	// SpikeStart; NaN if there is none or no spike window.
	ReactionTimeSec float64
	// ReplicaMinutes integrates Ready+Starting over the timeline (each
	// sample holds until the next, the last for one second) / 60.
	ReplicaMinutes float64
	// JainIndex is Jain's index over the spike window of
	// x_i = min(1, attainment_i / target_i) for every tenant with an SLO and
	// at least one non-shed request in the window. attainment_i is the
	// share of those requests that met the SLO, counted as in
	// VictimViolationPct (shed requests excluded, failed ones misses), in
	// line with model.TenantObservation.SLOAttainment; shedding shows in
	// NoisyShedPct and VictimShedAwareViolationPct instead.
	JainIndex float64
	// MaxCounterGap is the largest defined CounterGap (NaN if never defined).
	MaxCounterGap float64
	// ReclaimLatencySec is how long the returning owner keeps suffering
	// after it comes back: among the owner's requests arriving in
	// [OwnerReturn, OwnerReturn+ReclaimHorizon), the arrival offset from
	// OwnerReturn of the LAST one that missed its TTFT bound (shed, failed
	// and unfinished requests count as misses); 0 when none missed. A value
	// near ReclaimHorizon means the owner was not reclaimed within the
	// horizon. NaN if not applicable: no Owner or OwnerReturn, no SLO for the
	// owner, or no owner request in the horizon.
	ReclaimLatencySec float64
	// PolicyLatencyMeanMs is the mean of PolicyLatencies in milliseconds.
	PolicyLatencyMeanMs float64
	// Completed, Shed and Failed count requests over the whole run.
	Completed, Shed, Failed int
}

// Compute derives the metrics of one run. Requests of tenants without an
// SLO entry are ignored by the SLO-based metrics.
//
// A run may stop with requests still unfinished. The SLO-based metrics
// judge such a request as if it finished at the run's observed end (the
// later of the last Timeline sample and the latest record timestamp, which
// never exceeds the true end): if even that best case misses the SLO the
// request is a miss, otherwise its outcome is unknown and it is left out
// of both numerator and denominator.
func Compute(d RunData) RunMetrics {
	m := RunMetrics{
		P99TTFT: map[string]float64{},
		P99TPOT: map[string]float64{},
	}
	ttft, tpot := map[string][]float64{}, map[string][]float64{}
	for t := range d.SLOs {
		ttft[t], tpot[t] = nil, nil
	}
	noisyTotal, noisyShed := 0, 0
	for _, r := range d.Records {
		if _, ok := ttft[r.Tenant]; !ok {
			ttft[r.Tenant], tpot[r.Tenant] = nil, nil
		}
		switch {
		case r.Shed:
			m.Shed++
		case r.Failed:
			m.Failed++
		case r.Completed():
			m.Completed++
			ttft[r.Tenant] = append(ttft[r.Tenant], r.TTFT().Seconds())
			if r.OutputTokens > 1 {
				tpot[r.Tenant] = append(tpot[r.Tenant], r.TPOT().Seconds())
			}
		}
		if d.Noisy != "" && r.Tenant == d.Noisy {
			noisyTotal++
			if r.Shed {
				noisyShed++
			}
		}
	}
	for t := range ttft {
		m.P99TTFT[t] = Percentile(ttft[t], 99)
		m.P99TPOT[t] = Percentile(tpot[t], 99)
	}
	m.NoisyShedPct = pct(noisyShed, noisyTotal)
	timeline := slices.Clone(d.Timeline)
	slices.SortStableFunc(timeline, func(a, b Sample) int { return cmp.Compare(a.T, b.T) })
	end := observedEnd(d, timeline)
	m.VictimViolationPct, m.VictimShedAwareViolationPct = victimViolations(d, end)
	m.JainIndex = spikeJain(d, end)
	m.ReactionTimeSec = reactionTime(d)
	m.ReplicaMinutes = replicaMinutes(timeline)
	m.MaxCounterGap = maxCounterGap(timeline)
	m.ReclaimLatencySec = reclaimLatency(d)
	m.PolicyLatencyMeanMs = meanMillis(d.PolicyLatencies)
	return m
}

// inSpike reports whether a request arrived inside the spike window.
func inSpike(d RunData, r Record) bool {
	return r.Arrival >= d.SpikeStart && r.Arrival < d.SpikeEnd
}

// observedEnd is the latest instant the run is known to have reached: its
// last Timeline sample (timeline is sorted by T) or the latest timestamp of
// any record, whichever is later.
func observedEnd(d RunData, timeline []Sample) time.Duration {
	var end time.Duration
	if n := len(timeline); n > 0 {
		end = timeline[n-1].T
	}
	for _, r := range d.Records {
		end = max(end, r.Arrival, r.FirstToken, r.Done)
	}
	return end
}

// sloOutcome reports whether r met slo and whether that is decided yet in
// a run observed until end. Shed and failed requests are decided misses
// and completed ones are judged by MetSLO. An unfinished request produces
// its missing first token and its last token at end at the earliest, so
// it is a decided miss when even finishing at end would miss the SLO, and
// undecided otherwise.
func sloOutcome(r Record, slo model.SLO, end time.Duration) (met, decided bool) {
	switch {
	case r.Shed || r.Failed:
		return false, true
	case r.Completed():
		return r.MetSLO(slo), true
	}
	best := r
	if best.FirstToken == 0 {
		best.FirstToken = end
	}
	best.Done = end
	return false, !slo.Met(best.TTFT(), best.TPOT(), r.OutputTokens)
}

func victimViolations(d RunData, end time.Duration) (servedPct, shedAwarePct float64) {
	victims := map[string]bool{}
	for _, v := range d.Victims {
		victims[v] = true
	}
	served, servedMiss, all, allMiss := 0, 0, 0, 0
	for _, r := range d.Records {
		slo, ok := d.SLOs[r.Tenant]
		if !victims[r.Tenant] || !ok || !inSpike(d, r) {
			continue
		}
		met, decided := sloOutcome(r, slo, end)
		if !decided {
			continue
		}
		all++
		if !met {
			allMiss++
		}
		if !r.Shed {
			served++
			if !met {
				servedMiss++
			}
		}
	}
	return pct(servedMiss, served), pct(allMiss, all)
}

func spikeJain(d RunData, end time.Duration) float64 {
	total, met := map[string]int{}, map[string]int{}
	for _, r := range d.Records {
		slo, ok := d.SLOs[r.Tenant]
		if !ok || r.Shed || !inSpike(d, r) {
			continue
		}
		hit, decided := sloOutcome(r, slo, end)
		if !decided {
			continue
		}
		total[r.Tenant]++
		if hit {
			met[r.Tenant]++
		}
	}
	var xs []float64
	for _, t := range slices.Sorted(maps.Keys(d.SLOs)) {
		slo := d.SLOs[t]
		if total[t] == 0 || !(slo.TargetFraction > 0) {
			continue
		}
		attainment := float64(met[t]) / float64(total[t])
		xs = append(xs, math.Min(1, attainment/slo.TargetFraction))
	}
	return Jain(xs)
}

func reactionTime(d RunData) float64 {
	if d.SpikeEnd <= d.SpikeStart {
		return math.NaN()
	}
	best := math.NaN()
	for _, a := range d.ActionTimes {
		if a >= d.SpikeStart {
			if s := (a - d.SpikeStart).Seconds(); !(s >= best) {
				best = s
			}
		}
	}
	return best
}

// replicaMinutes integrates Ready+Starting over a timeline sorted by T.
func replicaMinutes(timeline []Sample) float64 {
	var replicaSeconds float64
	for i, s := range timeline {
		hold := time.Second
		if i+1 < len(timeline) {
			hold = timeline[i+1].T - s.T
		}
		replicaSeconds += float64(s.Ready+s.Starting) * hold.Seconds()
	}
	return replicaSeconds / 60
}

func maxCounterGap(timeline []Sample) float64 {
	best := math.NaN()
	for _, s := range timeline {
		if !math.IsNaN(s.CounterGap) && !(s.CounterGap <= best) {
			best = s.CounterGap
		}
	}
	return best
}

// ReclaimHorizon bounds how long after OwnerReturn ReclaimLatencySec looks
// for the owner's TTFT misses.
const ReclaimHorizon = 120 * time.Second

// reclaimLatency implements RunMetrics.ReclaimLatencySec from the owner's
// request outcomes, so it reflects what the owner experienced rather than a
// sampled queue state.
func reclaimLatency(d RunData) float64 {
	slo, ok := d.SLOs[d.Owner]
	if d.Owner == "" || d.OwnerReturn <= 0 || !ok {
		return math.NaN()
	}
	end := d.OwnerReturn + ReclaimHorizon
	seen := false
	var lastMiss time.Duration
	for _, r := range d.Records {
		if r.Tenant != d.Owner || r.Arrival < d.OwnerReturn || r.Arrival >= end {
			continue
		}
		seen = true
		missed := r.Shed || r.Failed || r.FirstToken <= 0 || r.FirstToken-r.Arrival > slo.TTFT()
		if missed && r.Arrival-d.OwnerReturn > lastMiss {
			lastMiss = r.Arrival - d.OwnerReturn
		}
	}
	if !seen {
		return math.NaN()
	}
	return lastMiss.Seconds()
}

func meanMillis(ds []time.Duration) float64 {
	if len(ds) == 0 {
		return math.NaN()
	}
	var sum float64
	for _, d := range ds {
		sum += float64(d) / float64(time.Millisecond)
	}
	return sum / float64(len(ds))
}

// pct returns 100*n/total, or NaN when total is zero.
func pct(n, total int) float64 {
	if total == 0 {
		return math.NaN()
	}
	return 100 * float64(n) / float64(total)
}
