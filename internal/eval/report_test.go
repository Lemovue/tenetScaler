package eval

import (
	"errors"
	"math"
	"reflect"
	"strings"
	"testing"
)

func TestTableWriters(t *testing.T) {
	tbl := Table{Header: []string{"setup", "note"}, Rows: [][]string{{"S1", "a,b|c"}, {"S2", "say \"hi\"\nnow"}}}
	var csvOut, mdOut strings.Builder
	if err := tbl.WriteCSV(&csvOut); err != nil {
		t.Fatal(err)
	}
	wantCSV := "setup,note\nS1,\"a,b|c\"\nS2,\"say \"\"hi\"\"\nnow\"\n"
	if csvOut.String() != wantCSV {
		t.Errorf("CSV:\n%q\nwant\n%q", csvOut.String(), wantCSV)
	}
	if err := tbl.WriteMarkdown(&mdOut); err != nil {
		t.Fatal(err)
	}
	wantMD := "| setup | note |\n| --- | --- |\n| S1 | a,b\\|c |\n| S2 | say \"hi\" now |\n"
	if mdOut.String() != wantMD {
		t.Errorf("Markdown:\n%q\nwant\n%q", mdOut.String(), wantMD)
	}
}

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errors.New("disk full") }

func TestTableErrors(t *testing.T) {
	bad := []struct {
		name string
		tbl  Table
		want string
	}{
		{"no columns", Table{}, "no columns"},
		{"ragged", Table{Header: []string{"a", "b"}, Rows: [][]string{{"1"}}}, "row 0 has 1 cells, want 2"},
	}
	for _, tc := range bad {
		for _, write := range []func(Table) error{
			func(tb Table) error { return tb.WriteCSV(&strings.Builder{}) },
			func(tb Table) error { return tb.WriteMarkdown(&strings.Builder{}) },
		} {
			if err := write(tc.tbl); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("%s: error %v, want one containing %q", tc.name, err, tc.want)
			}
		}
	}
	ok := Table{Header: []string{"a"}, Rows: [][]string{{"1"}}}
	if err := ok.WriteCSV(failingWriter{}); err == nil {
		t.Error("WriteCSV ignored a write error")
	}
	if err := ok.WriteMarkdown(failingWriter{}); err == nil {
		t.Error("WriteMarkdown ignored a write error")
	}
}

func TestFormatValue(t *testing.T) {
	tests := map[float64]string{1.23456: "1.235", 2: "2.000", -0.0001: "0.000", -2.5: "-2.500", 1234.5: "1234.500",
		math.NaN(): "n/a", math.Inf(1): "inf", math.Inf(-1): "-inf"}
	for v, want := range tests {
		if got := FormatValue(v); got != want {
			t.Errorf("FormatValue(%v) = %q, want %q", v, got, want)
		}
	}
}

func TestMetrics(t *testing.T) {
	ms := Metrics([]string{"A"})
	m := RunMetrics{
		VictimViolationPct: 1, VictimShedAwareViolationPct: 2, NoisyShedPct: 3, ReactionTimeSec: 4,
		ReplicaMinutes: 5, JainIndex: 6, MaxCounterGap: 7, ReclaimLatencySec: 8, PolicyLatencyMeanMs: 9,
		P99TTFT: map[string]float64{"A": 10}, P99TPOT: map[string]float64{"A": 11},
		Completed: 12, Shed: 13, Failed: 14,
	}
	wantNames := []string{"victim_violation_pct", "victim_shed_aware_violation_pct", "noisy_shed_pct",
		"reaction_time_s", "replica_minutes", "jain_index", "max_counter_gap", "reclaim_latency_s",
		"policy_latency_mean_ms", "p99_ttft_s_A", "p99_tpot_s_A", "completed", "shed", "failed"}
	if names := metricNames(ms); !reflect.DeepEqual(names, wantNames) {
		t.Fatalf("metric names %v, want %v", names, wantNames)
	}
	for i, mt := range ms {
		if got := mt.Value(m); got != float64(i+1) {
			t.Errorf("%s = %v, want %d", mt.Name, got, i+1)
		}
	}
	if v := ms[9].Value(RunMetrics{}); !math.IsNaN(v) {
		t.Errorf("p99 of a tenant without data = %v, want NaN", v)
	}
}

func TestSummaries(t *testing.T) {
	nan := math.NaN()
	all := Metrics(nil)
	metrics := []Metric{all[0], all[3]} // victim_violation_pct, reaction_time_s
	results := []RunResult{
		{Setup: "S2", Run: 1, Seed: 11, Metrics: RunMetrics{VictimViolationPct: 10, ReactionTimeSec: 5}},
		{Setup: "S7", Run: 1, Seed: 11, Metrics: RunMetrics{VictimViolationPct: 1, ReactionTimeSec: nan}},
		{Setup: "S2", Run: 2, Seed: 12, Metrics: RunMetrics{VictimViolationPct: 20, ReactionTimeSec: nan}},
		{Setup: "S2", Run: 3, Seed: 13, Metrics: RunMetrics{VictimViolationPct: 30, ReactionTimeSec: 7}},
	}
	runs := RunsTable(results, metrics)
	wantRuns := Table{
		Header: []string{"setup", "run", "seed", "victim_violation_pct", "reaction_time_s"},
		Rows: [][]string{
			{"S2", "1", "11", "10.000", "5.000"},
			{"S7", "1", "11", "1.000", "n/a"},
			{"S2", "2", "12", "20.000", "n/a"},
			{"S2", "3", "13", "30.000", "7.000"},
		},
	}
	if !reflect.DeepEqual(runs, wantRuns) {
		t.Errorf("RunsTable = %v, want %v", runs, wantRuns)
	}

	sums := SummarizeRuns(results, metrics)
	if len(sums) != 2 || sums[0].Setup != "S2" || sums[0].Runs != 3 || sums[1].Setup != "S7" || sums[1].Runs != 1 {
		t.Fatalf("SummarizeRuns grouping = %+v", sums)
	}
	// S2 victim: 10, 20, 30 -> mean 20, std 10, CI95 = 4.302653 * 10 / sqrt(3) = 24.841.
	// S2 reaction: 5, 7 -> mean 6, std sqrt(2), CI95 = 12.706205 * sqrt(2) / sqrt(2).
	summary := SummaryTable(sums, metrics)
	wantSummary := Table{
		Header: []string{"setup", "runs", "victim_violation_pct", "reaction_time_s"},
		Rows: [][]string{
			{"S2", "3", "20.000 ± 24.841", "6.000 ± 12.706 (n=2)"},
			{"S7", "1", "1.000", "n/a"},
		},
	}
	if !reflect.DeepEqual(summary, wantSummary) {
		t.Errorf("SummaryTable = %v, want %v", summary, wantSummary)
	}
	stats := StatsTable(sums, metrics)
	wantStats := Table{
		Header: []string{"setup", "metric", "n", "mean", "std", "ci95"},
		Rows: [][]string{
			{"S2", "victim_violation_pct", "3", "20.000", "10.000", "24.841"},
			{"S2", "reaction_time_s", "2", "6.000", "1.414", "12.706"},
			{"S7", "victim_violation_pct", "1", "1.000", "n/a", "n/a"},
			{"S7", "reaction_time_s", "0", "n/a", "n/a", "n/a"},
		},
	}
	if !reflect.DeepEqual(stats, wantStats) {
		t.Errorf("StatsTable = %v, want %v", stats, wantStats)
	}
	if got := SummarizeRuns(nil, metrics); len(got) != 0 {
		t.Errorf("SummarizeRuns(nil) = %v, want empty", got)
	}
}

func TestEvaluate(t *testing.T) {
	r := Evaluate(fixture())
	if r.Setup != "S7" || r.Run != 3 || r.Seed != 103 || !near(r.Metrics.VictimViolationPct, 500.0/9) {
		t.Errorf("Evaluate = %+v", r)
	}
	var md strings.Builder
	if err := RunsTable([]RunResult{r}, Metrics([]string{"A", "B", "C", "D"})).WriteMarkdown(&md); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(md.String(), "| S7 | 3 | 103 | 55.556 | 60.000 | 40.000 | 12.500 |") {
		t.Errorf("runs table row not as expected:\n%s", md.String())
	}
}
