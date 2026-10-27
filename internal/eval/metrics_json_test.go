package eval

import (
	"encoding/json"
	"math"
	"strings"
	"testing"

	"tenantscaler/internal/model"
)

// sameMetrics reports every field where got and want differ (NaN equals
// NaN).
func sameMetrics(t *testing.T, name string, got, want RunMetrics) {
	t.Helper()
	for _, mt := range Metrics(nil) {
		if g, w := mt.Value(got), mt.Value(want); !near(g, w) {
			t.Errorf("%s: %s = %v after the round trip, want %v", name, mt.Name, g, w)
		}
	}
	for _, p := range []struct {
		field     string
		got, want map[string]float64
	}{{"P99TTFT", got.P99TTFT, want.P99TTFT}, {"P99TPOT", got.P99TPOT, want.P99TPOT}} {
		if (p.got == nil) != (p.want == nil) || len(p.got) != len(p.want) {
			t.Errorf("%s: %s = %v after the round trip, want %v", name, p.field, p.got, p.want)
			continue
		}
		for k, w := range p.want {
			if g, ok := p.got[k]; !ok || !near(g, w) {
				t.Errorf("%s: %s[%s] = %v (present %v) after the round trip, want %v", name, p.field, k, g, ok, w)
			}
		}
	}
}

func TestRunMetricsJSONRoundTrip(t *testing.T) {
	slo := model.SLO{TTFTSeconds: 1, TPOTMillis: 1, TargetFraction: 1}
	tests := []struct {
		name  string
		m     RunMetrics
		nulls []string // fragments the encoding must contain
	}{
		{"empty run", Compute(RunData{}), []string{`"ReclaimLatencySec":null`, `"JainIndex":null`, `"P99TTFT":{}`}},
		{"SLO tenant without data", Compute(RunData{SLOs: map[string]model.SLO{"A": slo}}), []string{`"P99TTFT":{"A":null}`}},
		{"fixture", Compute(fixture()), []string{`"ReclaimLatencySec":null`, `"D":null`, `"Completed":13`}},
		{"zero value", RunMetrics{}, []string{`"P99TTFT":null`, `"JainIndex":0`}},
	}
	for _, tc := range tests {
		data, err := json.Marshal(tc.m)
		if err != nil {
			t.Fatalf("%s: Marshal: %v", tc.name, err)
		}
		for _, want := range tc.nulls {
			if !strings.Contains(string(data), want) {
				t.Errorf("%s: %s lacks %s", tc.name, data, want)
			}
		}
		var back RunMetrics
		if err := json.Unmarshal(data, &back); err != nil {
			t.Fatalf("%s: Unmarshal: %v", tc.name, err)
		}
		sameMetrics(t, tc.name, back, tc.m)
	}
}

func TestRunMetricsJSONEdgeCases(t *testing.T) {
	// A whole RunResult encodes, NaN metrics included.
	data, err := json.Marshal(Evaluate(RunData{Setup: "S7", Run: 2, Seed: 102}))
	if err != nil || !strings.Contains(string(data), `"Setup":"S7"`) || !strings.Contains(string(data), `"ReclaimLatencySec":null`) {
		t.Errorf("Marshal(RunResult) = %s, %v", data, err)
	}
	// Infinite values are written as null and read back as NaN.
	data, err = json.Marshal(RunMetrics{ReplicaMinutes: math.Inf(1), P99TTFT: map[string]float64{"A": math.Inf(-1)}})
	if err != nil {
		t.Fatal(err)
	}
	var back RunMetrics
	if err := json.Unmarshal(data, &back); err != nil || !math.IsNaN(back.ReplicaMinutes) || !math.IsNaN(back.P99TTFT["A"]) {
		t.Errorf("infinite values: %s decoded to %+v, %v", data, back, err)
	}
	// Missing scalars become NaN; counts and absent maps keep their zero value.
	if err := json.Unmarshal([]byte(`{"Completed":2}`), &back); err != nil ||
		!math.IsNaN(back.JainIndex) || back.Completed != 2 || back.P99TTFT != nil {
		t.Errorf("partial object decoded to %+v, %v", back, err)
	}
	if err := json.Unmarshal([]byte(`{"JainIndex":"x"}`), &back); err == nil {
		t.Error("a string metric should not decode")
	}
}
