package eval

import (
	"math"
	"testing"
	"time"

	"tenantscaler/internal/model"
)

func sec(s float64) time.Duration { return time.Duration(math.Round(s * float64(time.Second))) }

// served builds a completed record: first token after ttft seconds, then
// out-1 more tokens tpot seconds apart.
func served(tenant string, arrival, ttft, tpot float64, out int) Record {
	first := arrival + ttft
	return Record{Tenant: tenant, Arrival: sec(arrival), FirstToken: sec(first),
		Done: sec(first + tpot*float64(max(out-1, 0))), OutputTokens: out, InputTokens: 100}
}

func shed(tenant string, arrival float64) Record {
	return Record{Tenant: tenant, Arrival: sec(arrival), Shed: true, ShedReason: "over share", OutputTokens: 10}
}

func failed(tenant string, arrival float64) Record {
	return Record{Tenant: tenant, Arrival: sec(arrival), Failed: true, OutputTokens: 10}
}

func near(a, b float64) bool {
	if math.IsNaN(a) || math.IsNaN(b) {
		return math.IsNaN(a) && math.IsNaN(b)
	}
	return math.Abs(a-b) <= 1e-9*math.Max(1, math.Abs(b))
}

func TestRecordLatencies(t *testing.T) {
	r := served("A", 10, 1.5, 0.05, 11)
	if r.TTFT() != 1500*time.Millisecond || r.TPOT() != 50*time.Millisecond || !r.Completed() {
		t.Errorf("TTFT %v TPOT %v completed %v; want 1.5s, 50ms, true", r.TTFT(), r.TPOT(), r.Completed())
	}
	if one := served("A", 10, 1, 0, 1); one.TPOT() != 0 {
		t.Errorf("single-token TPOT = %v, want 0", one.TPOT())
	}
	slo := model.SLO{TTFTSeconds: 2, TPOTMillis: 100, TargetFraction: 0.99}
	tests := []struct {
		name string
		r    Record
		met  bool
	}{
		{"fast", served("A", 0, 1, 0.05, 11), true},
		{"ttft at the bound", served("A", 0, 2, 0.05, 11), true},
		{"slow ttft", served("A", 0, 2.5, 0.05, 11), false},
		{"slow tpot", served("A", 0, 1, 0.2, 5), false},
		{"one token ignores tpot", served("A", 0, 1.5, 0, 1), true},
		{"shed", shed("A", 0), false},
		{"failed", failed("A", 0), false},
		{"unfinished", Record{Tenant: "A", Arrival: sec(3), OutputTokens: 4}, false},
	}
	for _, tc := range tests {
		if got := tc.r.MetSLO(slo); got != tc.met {
			t.Errorf("%s: MetSLO = %v, want %v", tc.name, got, tc.met)
		}
	}
}

// fixture is a small run with hand-computed metrics. SLO bounds are 2 s
// TTFT and 100 ms TPOT for A, B and C (targets 0.5, 0.25 and 1); D has no
// SLO. The spike window is [100 s, 200 s).
func fixture() RunData {
	slo := func(target float64) model.SLO {
		return model.SLO{TTFTSeconds: 2, TPOTMillis: 100, TargetFraction: target}
	}
	return RunData{
		Setup: "S7", Run: 3, Seed: 103,
		Records: []Record{
			served("A", 110, 1, 0.05, 11),    // met
			served("A", 120, 3, 0, 1),        // miss: TTFT
			served("A", 130, 0.5, 0.2, 5),    // miss: TPOT
			served("A", 140, 1.5, 0, 1),      // met: one token, TTFT only
			failed("A", 150),                 // miss in both figures
			shed("A", 160),                   // miss in the shed-aware figure only
			served("B", 105, 0.2, 0.01, 3),   // met
			served("B", 100, 0.5, 0, 1),      // met: arrives at SpikeStart, inside
			served("B", 199, 2.5, 0, 1),      // miss
			served("B", 200, 5, 0, 1),        // outside: arrives at SpikeEnd
			served("B", 99, 0.3, 0, 1),       // outside: before the spike
			{Tenant: "B", Arrival: sec(170)}, // unfinished: miss
			served("C", 50, 0.1, 0.02, 101),  // outside, met
			shed("C", 110),                   // in window
			shed("C", 120),                   // in window
			served("C", 130, 1, 0.05, 2),     // in window, met
			served("C", 300, 4, 0, 1),        // outside, miss
			served("D", 120, 1, 0, 1),        // no SLO: ignored by SLO metrics
		},
		SLOs:       map[string]model.SLO{"A": slo(0.5), "B": slo(0.25), "C": slo(1)},
		Victims:    []string{"A", "B", "D"},
		Noisy:      "C",
		SpikeStart: sec(100), SpikeEnd: sec(200),
		ActionTimes:     []time.Duration{sec(50), sec(130), sec(112.5)},
		PolicyLatencies: []time.Duration{time.Millisecond, 2 * time.Millisecond, 4500 * time.Microsecond},
		Timeline: []Sample{ // deliberately unsorted
			{T: sec(3), Ready: 5, CounterGap: math.NaN()},
			{T: sec(0), Ready: 2, Starting: 1, CounterGap: 1.2},
			{T: sec(1), Ready: 4, CounterGap: 3.5},
		},
	}
}

func TestComputeFixture(t *testing.T) {
	m := Compute(fixture())
	scalars := []struct {
		name      string
		got, want float64
	}{
		// Victims A, B in [100, 200): 9 non-shed requests, 5 misses (A 120,
		// A 130, A 150 failed, B 199, B 170 unfinished long before the
		// observed end of 304 s); plus A 160 shed: 6 of 10.
		{"VictimViolationPct", m.VictimViolationPct, 500.0 / 9},
		{"VictimShedAwareViolationPct", m.VictimShedAwareViolationPct, 60},
		// C: 2 of 5 requests shed.
		{"NoisyShedPct", m.NoisyShedPct, 40},
		// First action at or after 100 s is 112.5 s.
		{"ReactionTimeSec", m.ReactionTimeSec, 12.5},
		// Sorted: 3 replicas for 1 s, 4 for 2 s, 5 for the final 1 s = 16 replica-seconds.
		{"ReplicaMinutes", m.ReplicaMinutes, 16.0 / 60},
		// Window attainment over non-shed requests 2/5, 2/4, 1/1 against
		// targets 0.5, 0.25, 1: x = 0.8, 1 (capped from 2), 1;
		// Jain = 2.8^2 / (3 * 2.64) = 98/99.
		{"JainIndex", m.JainIndex, 98.0 / 99},
		{"MaxCounterGap", m.MaxCounterGap, 3.5},
		{"ReclaimLatencySec", m.ReclaimLatencySec, math.NaN()},
		{"PolicyLatencyMeanMs", m.PolicyLatencyMeanMs, 2.5},
	}
	for _, s := range scalars {
		if !near(s.got, s.want) {
			t.Errorf("%s = %v, want %v", s.name, s.got, s.want)
		}
	}
	// Completed: A 4, B 5, C 3, D 1. Shed: A 160, C 110, C 120. Failed: A 150.
	if m.Completed != 13 || m.Shed != 3 || m.Failed != 1 {
		t.Errorf("completed/shed/failed = %d/%d/%d, want 13/3/1", m.Completed, m.Shed, m.Failed)
	}
	// p99 by nearest rank over at most 100 values is the maximum.
	wantTTFT := map[string]float64{"A": 3, "B": 5, "C": 4, "D": 1}
	wantTPOT := map[string]float64{"A": 0.2, "B": 0.01, "C": 0.05, "D": math.NaN()}
	for tenant, want := range wantTTFT {
		if got, ok := m.P99TTFT[tenant]; !ok || !near(got, want) {
			t.Errorf("P99TTFT[%s] = %v (present %v), want %v", tenant, got, ok, want)
		}
		if got, ok := m.P99TPOT[tenant]; !ok || !near(got, wantTPOT[tenant]) {
			t.Errorf("P99TPOT[%s] = %v (present %v), want %v", tenant, got, ok, wantTPOT[tenant])
		}
	}
	if len(m.P99TTFT) != 4 || len(m.P99TPOT) != 4 {
		t.Errorf("p99 maps have %d/%d tenants, want 4", len(m.P99TTFT), len(m.P99TPOT))
	}
}

func TestComputeEmpty(t *testing.T) {
	m := Compute(RunData{})
	for name, v := range map[string]float64{
		"VictimViolationPct": m.VictimViolationPct, "VictimShedAwareViolationPct": m.VictimShedAwareViolationPct,
		"NoisyShedPct": m.NoisyShedPct, "ReactionTimeSec": m.ReactionTimeSec, "JainIndex": m.JainIndex,
		"MaxCounterGap": m.MaxCounterGap, "ReclaimLatencySec": m.ReclaimLatencySec, "PolicyLatencyMeanMs": m.PolicyLatencyMeanMs,
	} {
		if !math.IsNaN(v) {
			t.Errorf("%s = %v on an empty run, want NaN", name, v)
		}
	}
	if m.ReplicaMinutes != 0 || m.Completed != 0 || m.Shed != 0 || m.Failed != 0 || len(m.P99TTFT) != 0 {
		t.Errorf("empty run gave %+v", m)
	}
	// An SLO tenant without requests still gets a (NaN) p99 entry.
	m = Compute(RunData{SLOs: map[string]model.SLO{"A": {TTFTSeconds: 1, TPOTMillis: 1, TargetFraction: 1}}})
	if v, ok := m.P99TTFT["A"]; !ok || !math.IsNaN(v) {
		t.Errorf("P99TTFT[A] = %v (present %v), want NaN", v, ok)
	}
}

func TestReactionTime(t *testing.T) {
	tests := []struct {
		name       string
		start, end float64
		actions    []float64
		want       float64
	}{
		{"action at the spike start", 100, 200, []float64{100}, 0},
		{"earliest qualifying action", 100, 200, []float64{250, 90, 101.5}, 1.5},
		{"no action after the start", 100, 200, []float64{10, 99.9}, math.NaN()},
		{"no actions", 100, 200, nil, math.NaN()},
		{"no spike window", 0, 0, []float64{5}, math.NaN()},
	}
	for _, tc := range tests {
		d := RunData{SpikeStart: sec(tc.start), SpikeEnd: sec(tc.end)}
		for _, a := range tc.actions {
			d.ActionTimes = append(d.ActionTimes, sec(a))
		}
		if got := Compute(d).ReactionTimeSec; !near(got, tc.want) {
			t.Errorf("%s: ReactionTimeSec = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestReclaimLatency(t *testing.T) {
	slos := map[string]model.SLO{"A": {TTFTSeconds: 2, TPOTMillis: 100, TargetFraction: 0.99}}
	// req builds an owner request arriving at `at` seconds with the given
	// TTFT in seconds (ttft < 0 means it never produced a token).
	req := func(tenant string, at, ttft float64) Record {
		r := Record{Tenant: tenant, Arrival: sec(at), InputTokens: 10, OutputTokens: 10}
		if ttft >= 0 {
			r.FirstToken, r.Done = sec(at+ttft), sec(at+ttft+1)
		}
		return r
	}
	shed := func(at float64) Record { r := req("A", at, -1); r.Shed = true; return r }
	failed := func(at float64) Record { r := req("A", at, -1); r.Failed = true; return r }
	run := func(ret float64, records ...Record) RunData {
		return RunData{Owner: "A", OwnerReturn: sec(ret), SLOs: slos, Records: records}
	}
	horizon := ReclaimHorizon.Seconds()
	tests := []struct {
		name string
		d    RunData
		want float64
	}{
		{"served within SLO from the start", run(10, req("A", 10, 0.5), req("A", 12, 1.9)), 0},
		{"suffers until the last miss", run(10, req("A", 10.5, 3), req("A", 14, 2.5), req("A", 16, 0.4)), 4},
		{"unsorted records", run(10, req("A", 16, 0.4), req("A", 14, 2.5), req("A", 10.5, 3)), 4},
		{"shed counts as a miss", run(10, req("A", 11, 0.2), shed(17)), 7},
		{"failed counts as a miss", run(10, failed(13)), 3},
		{"unfinished counts as a miss", run(10, req("A", 15, -1)), 5},
		{"misses before the return are ignored", run(10, req("A", 9, 9), req("A", 11, 0.1)), 0},
		{"misses after the horizon are ignored", run(10, req("A", 11, 0.1), req("A", 10+horizon+1, 9)), 0},
		{"missing to the end of the horizon", run(10, req("A", 10+horizon-0.5, 9)), horizon - 0.5},
		{"other tenants do not count", run(10, req("A", 11, 0.1), req("B", 12, 9)), 0},
		{"no owner request in the horizon", run(10, req("B", 12, 9)), math.NaN()},
		{"no owner", RunData{OwnerReturn: sec(1), SLOs: slos, Records: []Record{req("A", 2, 9)}}, math.NaN()},
		{"zero return time", RunData{Owner: "A", SLOs: slos, Records: []Record{req("A", 2, 9)}}, math.NaN()},
		{"owner without an SLO", RunData{Owner: "A", OwnerReturn: sec(1), Records: []Record{req("A", 2, 9)}}, math.NaN()},
	}
	for _, tc := range tests {
		if got := Compute(tc.d).ReclaimLatencySec; !near(got, tc.want) {
			t.Errorf("%s: ReclaimLatencySec = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestUnfinishedRequests checks that a request still running when the run
// stops is a miss only when even finishing at the observed end would miss
// the SLO (2 s TTFT, 100 ms TPOT), and is left out otherwise.
func TestUnfinishedRequests(t *testing.T) {
	unfinished := func(arrival, firstToken float64, out int) Record {
		r := Record{Tenant: "A", Arrival: sec(arrival), OutputTokens: out}
		if firstToken > 0 {
			r.FirstToken = sec(firstToken)
		}
		return r
	}
	slo := model.SLO{TTFTSeconds: 2, TPOTMillis: 100, TargetFraction: 1}
	d := RunData{
		Records: []Record{
			served("A", 10, 1, 0.05, 11),  // met
			unfinished(97, 0, 10),         // miss: no first token 3 s after arrival
			unfinished(98, 0, 10),         // open: exactly 2 s, may still meet the bound
			unfinished(99, 0, 10),         // open: 1 s
			unfinished(97, 99.5, 10),      // miss: TTFT 2.5 s
			unfinished(95, 96, 11),        // miss: >= 400 ms per token even if done at 100 s
			unfinished(98.5, 99.5, 11),    // open: >= 50 ms per token so far
			unfinished(98, 99, 1),         // open: one token in time, not marked done
			served("C", 90, 1, 0.09, 101), // met; done at 100 s, the observed end
		},
		SLOs:     map[string]model.SLO{"A": slo, "C": slo},
		Victims:  []string{"A"},
		SpikeEnd: sec(1000),
	}
	tests := []struct {
		name                       string
		timeline                   []Sample
		violation, shedAware, jain float64
	}{
		// 4 decided A requests, 3 misses; x = 1/4 and 1 for A and C:
		// Jain = 1.25^2 / (2 * 1.0625) = 25/34.
		{"end from the records", nil, 75, 75, 25.0 / 34},
		// At 105 s every open request but the one-token one has missed:
		// 7 decided, 6 misses; x = 1/7 and 1: Jain = (64/49) / (100/49).
		{"end from the timeline", []Sample{{T: sec(105), CounterGap: math.NaN()}}, 600.0 / 7, 600.0 / 7, 0.64},
	}
	for _, tc := range tests {
		d.Timeline = tc.timeline
		m := Compute(d)
		if !near(m.VictimViolationPct, tc.violation) || !near(m.VictimShedAwareViolationPct, tc.shedAware) || !near(m.JainIndex, tc.jain) {
			t.Errorf("%s: violation %v, shed-aware %v, Jain %v; want %v, %v, %v", tc.name,
				m.VictimViolationPct, m.VictimShedAwareViolationPct, m.JainIndex, tc.violation, tc.shedAware, tc.jain)
		}
	}
}

func TestCounterGapAndReplicaMinutes(t *testing.T) {
	d := RunData{Timeline: []Sample{
		{T: sec(0), Ready: 1, Starting: 2, CounterGap: math.NaN()},
		{T: sec(5), Ready: 6, CounterGap: math.NaN()},
	}}
	m := Compute(d)
	// 3 replicas for 5 s, then 6 for the final second: 21 replica-seconds.
	if !near(m.ReplicaMinutes, 21.0/60) || !math.IsNaN(m.MaxCounterGap) {
		t.Errorf("ReplicaMinutes %v MaxCounterGap %v, want 0.35 and NaN", m.ReplicaMinutes, m.MaxCounterGap)
	}
}
