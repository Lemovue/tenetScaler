package experiment

import (
	"math"
	"strings"
	"testing"
	"time"

	"tenantscaler/internal/config"
	"tenantscaler/internal/forecast"
	"tenantscaler/internal/model"
	"tenantscaler/internal/workload"
)

func TestForecastWritesReport(t *testing.T) {
	dir := t.TempDir()
	rep, err := Forecast(ForecastPlan{
		Config: config.Default(), Scenario: workload.DefaultScenario(), Runs: 2, BaseSeed: 1,
		ColdStart: 30 * time.Second, SeriesDuration: 1200 * time.Second, OutDir: dir,
	})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Periodic != "B" || rep.Spike != "C" || rep.Season != 8 || rep.SeriesBuckets != 80 || rep.TrainBuckets != 48 {
		t.Fatalf("report header = %+v", rep)
	}
	if len(rep.PeriodicAccuracy) != 3 || rep.PeriodicAccuracy[2].Method != "holt-winters" || rep.PeriodicAccuracy[2].MAPE.N != 2 {
		t.Fatalf("periodic accuracy = %+v", rep.PeriodicAccuracy)
	}
	if rep.SpikeStartSeconds != 900 || len(rep.SpikeProbes) != 3 {
		t.Fatalf("spike analysis start %v probes %d", rep.SpikeStartSeconds, len(rep.SpikeProbes))
	}
	// No forecaster can see an unannounced spike coming.
	for _, p := range rep.SpikeProbes {
		if !(p.Onset.Mean < 0.6) {
			t.Fatalf("%s forecast %.2f of the spike onset: it cannot know about it", p.Method, p.Onset.Mean)
		}
	}
	if len(rep.Control) != 4 || rep.Control[1].ForecastRaisedPeriodic.N != 2 || rep.Control[0].ForecastRaisedPeriodic.Mean != 0 {
		t.Fatalf("control effects = %+v", rep.Control)
	}
	md := readFile(t, dir, "forecast.md")
	for _, want := range []string{"SIMULATED", "## Leakage statement", "BEFORE the model observes it", "chronological",
		"## Accuracy: periodic tenant B", "## Failure analysis: spike tenant C", "S7F-ts-full-forecast", "holt-winters", "seasonal-naive"} {
		if !strings.Contains(md, want) {
			t.Fatalf("forecast.md lacks %q:\n%s", want, md)
		}
	}
}

func TestForecastPlanValidation(t *testing.T) {
	base := ForecastPlan{Config: config.Default(), Scenario: workload.DefaultScenario(), Runs: 1, ColdStart: time.Second, OutDir: t.TempDir()}
	cases := []struct {
		name string
		edit func(*ForecastPlan)
		want string
	}{
		{"no periodic tenant", func(p *ForecastPlan) { p.Scenario = workload.ReclaimScenario() }, "no periodic tenant"},
		{"no runs", func(p *ForecastPlan) { p.Runs = 0 }, "runs"},
		{"no cold start", func(p *ForecastPlan) { p.ColdStart = 0 }, "cold start"},
		{"too short", func(p *ForecastPlan) { p.SeriesDuration, p.Scenario.Duration = time.Second, 200*time.Second }, "too short"},
	}
	for _, c := range cases {
		p := base
		p.Scenario = workload.DefaultScenario()
		c.edit(&p)
		if _, err := Forecast(p); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Fatalf("%s: err = %v, want %q", c.name, err, c.want)
		}
	}
}

func TestRateSeries(t *testing.T) {
	specs := []workload.Spec{
		{Offset: 0, Tenant: "B", InputTokens: 10, OutputTokens: 5},
		{Offset: 9 * time.Second, Tenant: "B", InputTokens: 20, OutputTokens: 0},
		{Offset: 3 * time.Second, Tenant: "C", InputTokens: 1000, OutputTokens: 1000},
		{Offset: 15 * time.Second, Tenant: "B", InputTokens: 30, OutputTokens: 15},
		{Offset: 40 * time.Second, Tenant: "B", InputTokens: 100, OutputTokens: 100}, // trailing partial bucket
	}
	got := rateSeries(specs, "B", 10*time.Second, 3)
	want := []float64{(model.WeightedTokens(10, 5) + 20) / 10, model.WeightedTokens(30, 15) / 10, 0}
	for i := range want {
		if math.Abs(got[i]-want[i]) > 1e-12 {
			t.Fatalf("series = %v, want %v", got, want)
		}
	}
}

// oneStepRatio and trustedPct must only use data before the bucket they
// score: a forecaster that sees series[k] early would return exactly 1.
func TestProbesDoNotLeak(t *testing.T) {
	series := []float64{1, 1, 1, 1, 9}
	if r := oneStepRatio(series, 4, forecast.NewNaive()); r != 1.0/9 {
		t.Fatalf("naive onset ratio = %v, want 1/9 (the last value seen is 1)", r)
	}
	if r := oneStepRatio(series, 0, forecast.NewNaive()); !math.IsNaN(r) {
		t.Fatalf("ratio at k=0 = %v, want NaN", r)
	}
	if p := trustedPct([]float64{5, 5, 5, 5, 5, 5, 5, 5}, 6, 8, forecast.NewNaive()); p != 100 {
		t.Fatalf("a perfect forecaster should be trusted, got %v%%", p)
	}
}
