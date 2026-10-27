package loadgen

import (
	"context"
	"math"
	"slices"
	"sync"
	"testing"
	"time"

	"tenantscaler/internal/eval"
	"tenantscaler/internal/workload"
)

// tinyLengths keeps live-test requests short.
var tinyLengths = workload.LengthDist{Median: 3, Sigma: 0.3, Min: 1, Max: 6}

// liveScenario is a fast scenario: A steady at 25 req/s, C spiking from
// 2 to 60 req/s (wall-clock at Speed 1).
func liveScenario() workload.Scenario {
	mix := workload.PriorityMix{Critical: 1, Standard: 2, Sheddable: 1}
	return workload.Scenario{
		Name:     "live-test",
		Duration: 10 * time.Second,
		Tenants: []workload.TenantPattern{
			{Tenant: "A", Kind: workload.KindSteady, BaseRPS: 25, PeakRPS: 25, CV: 1, Mix: mix, In: tinyLengths, Out: tinyLengths},
			{Tenant: "C", Kind: workload.KindSpike, BaseRPS: 2, PeakRPS: 60,
				SpikeStartSeconds: 5, SpikeDurationSeconds: 5, CV: 1, Mix: mix, In: tinyLengths, Out: tinyLengths},
		},
	}
}

func TestLiveSendsIdenticalTrafficAndSpikes(t *testing.T) {
	x, y := &fakeLLM{}, &fakeLLM{}
	targets := []Target{{Name: "x", URL: serve(t, x).URL}, {Name: "y", URL: serve(t, y).URL}}
	var mu sync.Mutex
	arrivals := map[string]map[uint64]time.Duration{"x": {}, "y": {}}
	onRecord := func(target string, r eval.Record) {
		mu.Lock()
		defer mu.Unlock()
		arrivals[target][r.ID] = r.Arrival
	}
	l, err := NewLive(liveScenario(), targets, 3, Options{Logger: discard, OnRecord: onRecord})
	if err != nil {
		t.Fatal(err)
	}
	if err := l.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := l.Start(context.Background()); err == nil {
		t.Error("second Start succeeded, want an error")
	}

	time.Sleep(600 * time.Millisecond)
	base := x.countTenant("C")
	const spikeFor = 600 * time.Millisecond
	l.Spike(spikeFor)
	if active, until := l.SpikeState(); !active || time.Until(until) > spikeFor || time.Until(until) < spikeFor/2 {
		t.Errorf("SpikeState() = %v, %v; want active until about %v from now", active, until, spikeFor)
	}
	time.Sleep(spikeFor)
	spiked := x.countTenant("C") - base
	l.Spike(0)
	if active, _ := l.SpikeState(); active {
		t.Error("Spike(0) did not end the spike")
	}
	l.Stop()
	l.Wait()

	// Expected: about 1.2 C requests before the spike, 36 during it.
	if spiked < 15 || base > 6 || spiked <= 3*base {
		t.Errorf("C sent %d requests in 600ms before the spike and %d in 600ms of spike; want a clear rise", base, spiked)
	}
	if a := x.countTenant("A"); a < 10 {
		t.Errorf("steady tenant A sent only %d requests in about 1.2s at 25 req/s", a)
	}

	keys := func(f *fakeLLM) []string {
		var ks []string
		for _, r := range f.requests() {
			ks = append(ks, r.key())
		}
		slices.Sort(ks)
		return ks
	}
	kx, ky := keys(x), keys(y)
	if !slices.Equal(kx, ky) {
		t.Errorf("targets received different traffic: %d vs %d requests", len(kx), len(ky))
	}
	if len(slices.Compact(slices.Clone(kx))) != len(kx) {
		t.Error("a request was sent twice to the same target, or two requests shared a prompt")
	}
	counts := l.Counts()
	if counts["x"].Completed != len(kx) || counts["y"].Completed != len(ky) || counts["x"].Failed+counts["y"].Failed != 0 {
		t.Errorf("Counts() = %+v, want %d completed per target and no failures", counts, len(kx))
	}

	// The same request leaves for both targets at the same moment.
	mu.Lock()
	defer mu.Unlock()
	if len(arrivals["x"]) != len(kx) || len(arrivals["y"]) != len(kx) {
		t.Fatalf("OnRecord saw %d and %d records, want %d per target", len(arrivals["x"]), len(arrivals["y"]), len(kx))
	}
	for id, ax := range arrivals["x"] {
		ay, ok := arrivals["y"][id]
		if !ok || (ax-ay).Abs() > 50*time.Millisecond {
			t.Errorf("request %d arrived at %v on x and %v (present %v) on y, want the same moment", id, ax, ay, ok)
		}
	}
}

func TestLiveRateAt(t *testing.T) {
	sc := liveScenario()
	sc.Duration = 25 * time.Second
	sc.Tenants = append(sc.Tenants, workload.TenantPattern{
		Tenant: "B", Kind: workload.KindPeriodic, BaseRPS: 1, PeakRPS: 3,
		PeriodSeconds: 10, BurstSeconds: 2, PhaseSeconds: 1, CV: 1,
		Mix: workload.PriorityMix{Standard: 1}, In: tinyLengths, Out: tinyLengths,
	}, workload.TenantPattern{
		Tenant: "D", Kind: workload.KindSpike, BaseRPS: 1, PeakRPS: 4,
		SpikeStartSeconds: 4, SpikeDurationSeconds: 6, CV: 1,
		Mix: workload.PriorityMix{Standard: 1}, In: tinyLengths, Out: tinyLengths,
	})
	l, err := NewLive(sc, []Target{{Name: "x", URL: "http://127.0.0.1:1"}}, 1, Options{Speed: 2, SpikeTenant: "C"})
	if err != nil {
		t.Fatal(err)
	}
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	l.s.start = t0
	at := func(s float64) time.Time { return t0.Add(seconds(s)) }
	a, c, b, d := l.tenants[0], l.tenants[1], l.tenants[2], l.tenants[3]

	tests := []struct {
		name     string
		tn       *liveTenant
		wall     float64 // seconds since start
		rate     float64 // wall-clock req/s (Speed 2)
		boundary float64 // seconds since start; NaN = none
	}{
		{"steady", a, 7, 50, math.NaN()},
		{"periodic in burst", b, 0.5, 6, 1.5},              // scenario 1.0: burst until 3
		{"periodic after burst", b, 1.6, 2, 5.5},           // scenario 3.2: next burst at 11
		{"periodic near cycle end", b, 12, 2, 12.5},        // scenario 24: cycle restarts at 25
		{"periodic next cycle burst", b, 13, 6, 14},        // scenario 26 = cycle 1 at 1: burst until 28
		{"spike tenant before Spike", c, 6, 4, math.NaN()}, // ignores its scenario window
		{"other spike tenant before window", d, 1, 2, 2},   // scenario 2: spike starts at 4
		{"other spike tenant in window", d, 3, 8, 5},       // scenario 6: spike ends at 10
		{"other spike tenant after window", d, 6, 2, 12.5}, // scenario 12: cycle restarts at 25
		{"other spike tenant next cycle", d, 13, 2, 14.5},  // scenario 26 = cycle 1 at 1: spike at 29
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rate, next := l.rateAt(tc.tn, at(tc.wall))
			if math.Abs(rate-tc.rate) > 1e-9 {
				t.Errorf("rate = %v, want %v", rate, tc.rate)
			}
			switch {
			case math.IsNaN(tc.boundary) && !next.IsZero():
				t.Errorf("boundary = %v, want none", next.Sub(t0))
			case !math.IsNaN(tc.boundary) && (next.Sub(at(tc.boundary)).Abs() > time.Microsecond):
				t.Errorf("boundary = %v, want %vs", next.Sub(t0), tc.boundary)
			}
		})
	}

	l.Spike(time.Hour)
	_, until := l.SpikeState()
	if rate, next := l.rateAt(c, time.Now()); rate != 120 || !next.Equal(until) {
		t.Errorf("during a spike rate = %v until %v, want 2 x 60 = 120 until %v", rate, next, until)
	}
	if rate, _ := l.rateAt(d, at(1)); rate != 2 {
		t.Errorf("Spike changed tenant D (not the spike tenant): rate %v, want its own window's 2", rate)
	}
}

func TestNewLiveRejectsBadInput(t *testing.T) {
	ok := []Target{{Name: "x", URL: "http://127.0.0.1:1"}}
	bad := liveScenario()
	bad.Duration = 0
	if _, err := NewLive(bad, ok, 1, Options{}); err == nil {
		t.Error("invalid scenario accepted")
	}
	if _, err := NewLive(liveScenario(), nil, 1, Options{}); err == nil {
		t.Error("no targets accepted")
	}
	if _, err := NewLive(liveScenario(), ok, 1, Options{Speed: math.Inf(1)}); err == nil {
		t.Error("infinite speed accepted")
	}
	l, err := NewLive(liveScenario(), ok, 1, Options{})
	if err != nil {
		t.Fatal(err)
	}
	l.Stop()
	l.Wait() // never started: returns at once
}
