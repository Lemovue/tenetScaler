package realrun

import (
	"context"
	"encoding/csv"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"tenantscaler/internal/workload"
)

// The scaling curve on the simulated backend: one level per replica count,
// the pooled throughput is the sum of the replicas', and the CSV holds one
// row per replica plus a pooled row per level.
func TestScalingCurveSimulated(t *testing.T) {
	plan := ProfilePlan{
		Config:        fastConfig(),
		In:            workload.LengthDist{Median: 20, Sigma: 0.3, Min: 4, Max: 40},
		Out:           workload.LengthDist{Median: 8, Sigma: 0.3, Min: 2, Max: 12},
		LevelDuration: 300 * time.Millisecond, Warmup: 100 * time.Millisecond,
		Seed: 3, LogDir: t.TempDir(), Logger: quietLogger(),
	}
	levels, err := ScalingCurve(context.Background(), plan, 2, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(levels) != 2 || levels[0].Replicas != 1 || levels[1].Replicas != 2 || len(levels[1].PerReplica) != 2 {
		t.Fatalf("levels %+v", levels)
	}
	for _, lv := range levels {
		sum := 0.0
		for _, r := range lv.PerReplica {
			if r.Concurrency != 2 || !(r.WeightedTokensPerSec > 0) || r.Failed != 0 {
				t.Errorf("%d replicas: replica level %+v, want concurrency = slots (2), throughput, no failures", lv.Replicas, r)
			}
			sum += r.WeightedTokensPerSec
		}
		if math.Abs(sum-lv.Pooled.WeightedTokensPerSec) > 1e-6*sum {
			t.Errorf("%d replicas: pooled %v tok/s, replicas sum to %v", lv.Replicas, lv.Pooled.WeightedTokensPerSec, sum)
		}
	}
	if _, err := ScalingCurve(context.Background(), plan, 4, 0); err == nil {
		t.Error("more replicas than the budget (3) accepted")
	}
	path := filepath.Join(t.TempDir(), "scaling.csv")
	if err := WriteScalingCSV(path, levels); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	rows, err := csv.NewReader(f).ReadAll()
	if err != nil || len(rows) != 1+(1+1)+(2+1) {
		t.Fatalf("csv rows %d (%v), want header + 2 + 3", len(rows), err)
	}
	if table := ScalingTable(levels); len(table.Rows) != 2 || table.Rows[0][5] != "1.000" {
		t.Errorf("table %v: efficiency of 1 replica must be 1", table.Rows)
	}
}

func TestDeriveBudget(t *testing.T) {
	levels := []ScalingLevel{
		{Replicas: 1, Pooled: Level{TPOTP99: 0.05, TTFTP99: 1.0, WeightedTokensPerSec: 130}},
		{Replicas: 4, Pooled: Level{TPOTP99: 0.1234, TTFTP99: 1.9, WeightedTokensPerSec: 482}},
	}
	b, err := DeriveBudget(levels, 4)
	if err != nil {
		t.Fatal(err)
	}
	// 1.25 x 123.4 ms = 154.25 ms -> 160 ms; 482 / 4 = 120.5 -> 121 (rounded).
	if b.TPOTSLOMillis != 160 || b.CapacityPerReplica != 121 || !b.MeetsTTFT {
		t.Fatalf("derivation %+v", b)
	}
	if !strings.Contains(b.Explain(), "= 160 ms") || !strings.Contains(b.Explain(), "meets it") {
		t.Errorf("explanation:\n%s", b.Explain())
	}
	levels[1].Pooled.TTFTP99 = 2.5
	if b, _ := DeriveBudget(levels, 4); b.MeetsTTFT || !strings.Contains(b.Explain(), "does NOT meet") {
		t.Errorf("a full budget missing the TTFT SLO must be flagged: %+v", b)
	}
	if _, err := DeriveBudget(levels, 3); err == nil {
		t.Error("a missing level was accepted")
	}
}

func TestParseTypeperf(t *testing.T) {
	out := "\r\n\"(PDH-CSV 4.0)\",\"\\\\NILAY\\Processor Information(_Total)\\% Processor Performance\"\r\n\"10/27/2026 12:49:08.551\",\"93.026137\"\r\nExiting, please wait...\r\n"
	if v := parseTypeperf(out); math.Abs(v-93.026137) > 1e-9 {
		t.Fatalf("parseTypeperf = %v, want 93.026137", v)
	}
	if v := parseTypeperf("error"); !math.IsNaN(v) {
		t.Fatalf("no sample: %v, want NaN", v)
	}
}
