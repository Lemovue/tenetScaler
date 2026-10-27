package eval

import (
	"encoding/csv"
	"fmt"
	"io"
	"math"
	"strconv"
	"strings"
)

// Table is a rectangular table of formatted cells, written as CSV or as a
// Markdown table.
type Table struct {
	Header []string
	Rows   [][]string
}

func (t Table) validate() error {
	if len(t.Header) == 0 {
		return fmt.Errorf("table has no columns")
	}
	for i, row := range t.Rows {
		if len(row) != len(t.Header) {
			return fmt.Errorf("table row %d has %d cells, want %d", i, len(row), len(t.Header))
		}
	}
	return nil
}

// WriteCSV writes the table as RFC 4180 CSV.
func (t Table) WriteCSV(w io.Writer) error {
	if err := t.validate(); err != nil {
		return err
	}
	cw := csv.NewWriter(w)
	if err := cw.Write(t.Header); err != nil {
		return err
	}
	if err := cw.WriteAll(t.Rows); err != nil {
		return err
	}
	return cw.Error()
}

// WriteMarkdown writes the table as a GitHub-flavoured Markdown table.
// Pipes in cells are escaped and newlines become spaces.
func (t Table) WriteMarkdown(w io.Writer) error {
	if err := t.validate(); err != nil {
		return err
	}
	var b strings.Builder
	line := func(cells []string) {
		b.WriteString("|")
		for _, c := range cells {
			c = strings.ReplaceAll(c, "|", `\|`)
			c = strings.Join(strings.Fields(strings.ReplaceAll(c, "\n", " ")), " ")
			b.WriteString(" " + c + " |")
		}
		b.WriteString("\n")
	}
	line(t.Header)
	sep := make([]string, len(t.Header))
	for i := range sep {
		sep[i] = "---"
	}
	line(sep)
	for _, row := range t.Rows {
		line(row)
	}
	_, err := io.WriteString(w, b.String())
	return err
}

// FormatValue formats a metric value with three decimals; NaN becomes
// "n/a" (which pandas and spreadsheets read as missing).
func FormatValue(v float64) string {
	switch {
	case math.IsNaN(v):
		return "n/a"
	case math.IsInf(v, 1):
		return "inf"
	case math.IsInf(v, -1):
		return "-inf"
	}
	v = math.Round(v*1000) / 1000
	if v == 0 {
		v = 0 // drop the sign of negative zero
	}
	return strconv.FormatFloat(v, 'f', 3, 64)
}

// Metric is one scalar column of the results: a name (with its unit) and
// how to read it from RunMetrics.
type Metric struct {
	Name  string
	Value func(RunMetrics) float64
}

// Metrics returns the standard report columns in order: the scalar
// RunMetrics fields, p99 TTFT and TPOT for each given tenant, and the
// request counts.
func Metrics(tenants []string) []Metric {
	ms := []Metric{
		{"victim_violation_pct", func(m RunMetrics) float64 { return m.VictimViolationPct }},
		{"victim_shed_aware_violation_pct", func(m RunMetrics) float64 { return m.VictimShedAwareViolationPct }},
		{"noisy_shed_pct", func(m RunMetrics) float64 { return m.NoisyShedPct }},
		{"reaction_time_s", func(m RunMetrics) float64 { return m.ReactionTimeSec }},
		{"replica_minutes", func(m RunMetrics) float64 { return m.ReplicaMinutes }},
		{"jain_index", func(m RunMetrics) float64 { return m.JainIndex }},
		{"max_counter_gap", func(m RunMetrics) float64 { return m.MaxCounterGap }},
		{"reclaim_latency_s", func(m RunMetrics) float64 { return m.ReclaimLatencySec }},
		{"policy_latency_mean_ms", func(m RunMetrics) float64 { return m.PolicyLatencyMeanMs }},
	}
	for _, t := range tenants {
		ms = append(ms,
			Metric{"p99_ttft_s_" + t, func(m RunMetrics) float64 { return lookup(m.P99TTFT, t) }},
			Metric{"p99_tpot_s_" + t, func(m RunMetrics) float64 { return lookup(m.P99TPOT, t) }})
	}
	return append(ms,
		Metric{"completed", func(m RunMetrics) float64 { return float64(m.Completed) }},
		Metric{"shed", func(m RunMetrics) float64 { return float64(m.Shed) }},
		Metric{"failed", func(m RunMetrics) float64 { return float64(m.Failed) }})
}

func lookup(m map[string]float64, key string) float64 {
	if v, ok := m[key]; ok {
		return v
	}
	return math.NaN()
}

// RunResult is one run's identity and metrics.
type RunResult struct {
	Setup   string
	Run     int
	Seed    int64
	Metrics RunMetrics
}

// Evaluate computes the metrics of d and tags them with its setup, run and
// seed.
func Evaluate(d RunData) RunResult {
	return RunResult{Setup: d.Setup, Run: d.Run, Seed: d.Seed, Metrics: Compute(d)}
}

// SetupSummary holds the statistics of one setup's runs; Stats is aligned
// with the metric list it was built from.
type SetupSummary struct {
	Setup string
	Runs  int
	Stats []Stat
}

// SummarizeRuns groups results by setup, in order of first appearance, and
// summarizes every metric over each setup's runs.
func SummarizeRuns(results []RunResult, metrics []Metric) []SetupSummary {
	var order []string
	bySetup := map[string][]RunMetrics{}
	for _, r := range results {
		if _, ok := bySetup[r.Setup]; !ok {
			order = append(order, r.Setup)
		}
		bySetup[r.Setup] = append(bySetup[r.Setup], r.Metrics)
	}
	sums := make([]SetupSummary, 0, len(order))
	for _, setup := range order {
		runs := bySetup[setup]
		s := SetupSummary{Setup: setup, Runs: len(runs), Stats: make([]Stat, len(metrics))}
		for i, mt := range metrics {
			vals := make([]float64, len(runs))
			for j, rm := range runs {
				vals[j] = mt.Value(rm)
			}
			s.Stats[i] = Summarize(vals)
		}
		sums = append(sums, s)
	}
	return sums
}

// RunsTable lists every run: setup, run, seed and one column per metric.
func RunsTable(results []RunResult, metrics []Metric) Table {
	t := Table{Header: append([]string{"setup", "run", "seed"}, metricNames(metrics)...)}
	for _, r := range results {
		row := []string{r.Setup, strconv.Itoa(r.Run), strconv.FormatInt(r.Seed, 10)}
		for _, mt := range metrics {
			row = append(row, FormatValue(mt.Value(r.Metrics)))
		}
		t.Rows = append(t.Rows, row)
	}
	return t
}

// SummaryTable is the human-readable summary: one row per setup, each
// metric cell "mean ± ci95", with "(n=k)" added when only k of the runs
// define the metric.
func SummaryTable(sums []SetupSummary, metrics []Metric) Table {
	t := Table{Header: append([]string{"setup", "runs"}, metricNames(metrics)...)}
	for _, s := range sums {
		row := []string{s.Setup, strconv.Itoa(s.Runs)}
		for i := range metrics {
			row = append(row, formatStat(s.Stats[i], s.Runs))
		}
		t.Rows = append(t.Rows, row)
	}
	return t
}

// StatsTable is the machine-readable summary in long format: one row per
// setup and metric with n, mean, std and ci95.
func StatsTable(sums []SetupSummary, metrics []Metric) Table {
	t := Table{Header: []string{"setup", "metric", "n", "mean", "std", "ci95"}}
	for _, s := range sums {
		for i, mt := range metrics {
			st := s.Stats[i]
			t.Rows = append(t.Rows, []string{s.Setup, mt.Name, strconv.Itoa(st.N),
				FormatValue(st.Mean), FormatValue(st.Std), FormatValue(st.CI95)})
		}
	}
	return t
}

func formatStat(st Stat, runs int) string {
	if st.N == 0 {
		return "n/a"
	}
	s := FormatValue(st.Mean)
	if !math.IsNaN(st.CI95) {
		s += " ± " + FormatValue(st.CI95)
	}
	if st.N != runs {
		s += fmt.Sprintf(" (n=%d)", st.N)
	}
	return s
}

func metricNames(metrics []Metric) []string {
	names := make([]string, len(metrics))
	for i, m := range metrics {
		names[i] = m.Name
	}
	return names
}
