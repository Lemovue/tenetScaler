package experiment

import (
	"math"
	"slices"
	"strings"
	"testing"
	"time"

	"tenantscaler/internal/config"
	"tenantscaler/internal/eval"
	"tenantscaler/internal/sim"
)

func TestScenarioByName(t *testing.T) {
	for name, want := range map[string]string{"default": "default", " Reclaim ": "reclaim"} {
		sc, err := ScenarioByName(name)
		if err != nil || sc.Name != want {
			t.Fatalf("ScenarioByName(%q) = %q, %v; want %q", name, sc.Name, err, want)
		}
	}
	if _, err := ScenarioByName("bursty"); err == nil || !strings.Contains(err.Error(), "default|reclaim") {
		t.Fatalf("unknown scenario: err = %v", err)
	}
}

func TestParseDurations(t *testing.T) {
	got, err := ParseDurations(" 10s, 30s,,2m ")
	if err != nil || !slices.Equal(got, []time.Duration{10 * time.Second, 30 * time.Second, 2 * time.Minute}) {
		t.Fatalf("ParseDurations = %v, %v", got, err)
	}
	if got, err := ParseDurations(""); err != nil || len(got) != 0 {
		t.Fatalf("empty list = %v, %v", got, err)
	}
	if _, err := ParseDurations("10s,ten"); err == nil || !strings.Contains(err.Error(), `"ten"`) {
		t.Fatalf("bad item: err = %v", err)
	}
}

func TestWithCapacity(t *testing.T) {
	base := config.Default() // guarantees 3+3+3 of 10
	got, err := WithCapacity(base, 6)
	if err != nil {
		t.Fatal(err)
	}
	if got.Pool.CapacityReplicas != 6 || got.Pool.MinReplicas != 1 {
		t.Fatalf("pool = %+v", got.Pool)
	}
	for _, tn := range got.Tenants {
		if tn.GuaranteedReplicas != 2 {
			t.Fatalf("tenant %s guaranteed %d, want 2 (scaled 3*6/9)", tn.Name, tn.GuaranteedReplicas)
		}
	}
	if base.Tenants[0].GuaranteedReplicas != 3 || base.Pool.CapacityReplicas != 10 {
		t.Fatal("WithCapacity modified its input")
	}
	if got, err := WithCapacity(base, 9); err != nil || got.Tenants[0].GuaranteedReplicas != 3 {
		t.Fatalf("guarantees that fit must stay: %+v, %v", got.Tenants, err)
	}
	if _, err := WithCapacity(base, 0); err == nil {
		t.Fatal("capacity 0 accepted")
	}
}

// The terminal table uses summary.md's formatting: no "± n/a" for a
// single run, and "(n=k)" when only k runs define a metric.
func TestMetricsTable(t *testing.T) {
	sum := Summary{Results: []ColdStartResult{{ColdStartSeconds: 30, Setups: []SetupResult{
		{Setup: "one-run", Runs: 1, Metrics: map[string]eval.Stat{metricVictim: {Mean: 10, Std: math.NaN(), CI95: math.NaN(), N: 1}}},
		{Setup: "partial", Runs: 3, Metrics: map[string]eval.Stat{metricVictim: {Mean: 2, Std: 0.5, CI95: 1, N: 2}}},
		{Setup: "none", Runs: 3, Metrics: map[string]eval.Stat{metricVictim: {Mean: math.NaN(), Std: math.NaN(), CI95: math.NaN()}}},
	}}}}
	tab, err := MetricsTable(sum, 30, metricVictim)
	if err != nil {
		t.Fatal(err)
	}
	want := [][]string{{"one-run", "1", "10.000"}, {"partial", "3", "2.000 ± 1.000 (n=2)"}, {"none", "3", "n/a"}}
	if !slices.Equal(tab.Header, []string{"setup", "runs", metricVictim}) || len(tab.Rows) != len(want) {
		t.Fatalf("table = %+v", tab)
	}
	for i, row := range tab.Rows {
		if !slices.Equal(row, want[i]) {
			t.Fatalf("row %d = %q, want %q", i, row, want[i])
		}
	}
	if _, err := MetricsTable(sum, 10, metricVictim); err == nil {
		t.Fatal("missing cold start accepted")
	}
	if _, err := MetricsTable(sum, 30, "bogus"); err == nil {
		t.Fatal("unknown metric accepted")
	}
}

func TestCompareStats(t *testing.T) {
	one := func(m float64) eval.Stat { return eval.Stat{Mean: m, Std: math.NaN(), CI95: math.NaN(), N: 1} }
	two := func(m, ci float64) eval.Stat { return eval.Stat{Mean: m, Std: ci, CI95: ci, N: 2} }
	cases := []struct {
		a, b eval.Stat
		want string
	}{
		{one(1), one(5), "not comparable (fewer than 2 runs"},
		{two(1, 0.1), one(5), "not comparable (fewer than 2 runs"},
		{two(1, 0.1), two(5, 0.1), "lower than"},
		{two(5, 0.1), two(1, 0.1), "higher than"},
		{two(1, 3), two(5, 3), "not distinguishable"},
		{eval.Stat{Mean: math.NaN()}, two(1, 1), "not comparable with"},
	}
	for _, c := range cases {
		if got := compareStats(c.a, c.b); !strings.HasPrefix(got, c.want) {
			t.Fatalf("compareStats(%+v, %+v) = %q, want %q...", c.a, c.b, got, c.want)
		}
	}
}

// backlogNote must say plainly when VTC ordering never decided anything.
func TestBacklogNote(t *testing.T) {
	metrics := eval.Metrics(nil)
	gi := metricIndex(metrics, metricCounterGap)
	summary := func(setup string, runs, withGap int) eval.SetupSummary {
		stats := make([]eval.Stat, len(metrics))
		stats[gi].N = withGap
		return eval.SetupSummary{Setup: setup, Runs: runs, Stats: stats}
	}
	note := func(sums ...eval.SetupSummary) string {
		return backlogNote(outcome{metrics: metrics, main: sweepPoint{sums: sums}})
	}
	if n := note(summary(sim.SetupKEDAQueue, 2, 0), summary(sim.SetupFull, 2, 0)); !strings.Contains(n, "VTC fairness never engaged") {
		t.Fatalf("no backlog anywhere: %q", n)
	}
	if n := note(summary(sim.SetupFull, 2, 1), summary(sim.SetupFullForecast, 2, 0)); strings.Contains(n, "never engaged") ||
		!strings.Contains(n, sim.SetupFull+" 1/2") || !strings.Contains(n, sim.SetupFullForecast+" 0/2") {
		t.Fatalf("backlog in one run: %q", n)
	}
	if n := note(summary(sim.SetupKEDAQueue, 2, 0)); n != "" {
		t.Fatalf("no fair-gateway setup: %q", n)
	}
}

// The S4 disclosure appears exactly when S4 is evaluated.
func TestThreatsDiscloseS4(t *testing.T) {
	with := threatsMarkdown(Plan{Setups: []string{sim.SetupFlowControl, sim.SetupFull}})
	if !strings.Contains(with, "weak by construction") || !strings.Contains(with, sim.SetupFlowControlOutstanding) {
		t.Fatalf("threats with S4 lack the disclosure:\n%s", with)
	}
	if without := threatsMarkdown(Plan{Setups: []string{sim.SetupFull}}); strings.Contains(without, "weak by construction") {
		t.Fatalf("threats without S4 mention it:\n%s", without)
	}
}
