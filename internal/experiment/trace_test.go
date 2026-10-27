package experiment

import (
	"math"
	"strings"
	"testing"
	"time"

	"tenantscaler/internal/config"
)

func TestLoadAzureTrace(t *testing.T) {
	csv := "TIMESTAMP,ContextTokens,GeneratedTokens\n" +
		"2024-05-10 00:00:00.009930+00:00,100,10\n" +
		"2024-05-10 00:00:30.000000+00:00,50,5\n" +
		"2024-05-10 00:01:10.000000+00:00,10,0\n" +
		"2024-05-09 23:59:59.000000+00:00,1,1\n"
	s, err := LoadAzureTrace(strings.NewReader(csv), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	// Bucket 0: (100+20) + (50+10) + early (1+2) tokens, 3 requests; bucket 1: 10 tokens, 1 request.
	if s.Rows != 4 || s.Early != 1 || len(s.Weighted) != 2 ||
		math.Abs(s.Weighted[0]-183.0/60) > 1e-9 || math.Abs(s.Weighted[1]-10.0/60) > 1e-9 || math.Abs(s.Requests[0]-3.0/60) > 1e-9 {
		t.Fatalf("series %+v", s)
	}
	for name, bad := range map[string]string{
		"columns": "time,a,b\n",
		"time":    "TIMESTAMP,ContextTokens,GeneratedTokens\nyesterday,1,1\n",
		"tokens":  "TIMESTAMP,ContextTokens,GeneratedTokens\n2024-05-10 00:00:00.0+00:00,-1,1\n",
		"empty":   "TIMESTAMP,ContextTokens,GeneratedTokens\n",
	} {
		if _, err := LoadAzureTrace(strings.NewReader(bad), time.Minute); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// The real-trace evaluation on a small synthetic periodic series: every
// held-out season is scored by every method, and S7 and S7F are simulated
// with tenant B driven by the rescaled series.
func TestEvaluateTrace(t *testing.T) {
	const season = 12
	var w, r []float64
	for i := range 5 * season {
		v := 100 + 80*math.Sin(2*math.Pi*float64(i)/season)
		w, r = append(w, v), append(r, v/100)
	}
	cfg := config.Default()
	rep, err := EvaluateTrace(TracePlan{
		Series: RateSeries{Bucket: time.Minute, Weighted: w, Requests: r, Rows: 1},
		Season: season, HeldOutSeasons: 2, Config: cfg, Runs: 1, BaseSeed: 1, ColdStart: 5 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Accuracy) != 2*3 || len(rep.MAPE) != 3 || rep.MAPE["seasonal-naive"].N != 2 {
		t.Fatalf("accuracy %+v / %+v", rep.Accuracy, rep.MAPE)
	}
	// A pure season repeats exactly: seasonal-naive is exact.
	if m := rep.MAPE["seasonal-naive"].Mean; m > 1e-6 {
		t.Errorf("seasonal-naive MAPE %v on an exact season, want 0", m)
	}
	if len(rep.Effects) != 2 || rep.Effects[0].BViolationPct.N != 1 || !(rep.Scale > 0) || rep.Steps == 0 {
		t.Fatalf("effects %+v scale %v steps %d", rep.Effects, rep.Scale, rep.Steps)
	}
	md, err := TraceMarkdown(rep)
	if err != nil || !strings.Contains(string(md), "SIMULATED") || !strings.Contains(string(md), "DynamoLLM") {
		t.Fatalf("markdown %v:\n%s", err, md)
	}
	if _, err := EvaluateTrace(TracePlan{Series: RateSeries{Weighted: w[:season]}, Season: season, HeldOutSeasons: 1}); err == nil {
		t.Error("a series shorter than 3 seasons was accepted")
	}
}
