package experiment

import (
	"bytes"
	"fmt"
	"math"
	"strconv"

	"tenantscaler/internal/eval"
)

// statCells formats stats exactly like the main summary table:
// "mean ± ci95", "(n=k)" when only k of runs define it, "n/a" for none.
func statCells(runs int, stats ...eval.Stat) []string {
	metrics := make([]eval.Metric, len(stats))
	for i := range metrics {
		metrics[i] = eval.Metric{Name: strconv.Itoa(i)}
	}
	row := eval.SummaryTable([]eval.SetupSummary{{Runs: runs, Stats: stats}}, metrics).Rows[0]
	return row[2:]
}

// compareStats says how a's mean relates to b's, judged by whether their
// 95% confidence intervals overlap (a conservative test). With fewer than
// two runs a CI is undefined, so no difference is claimed.
func compareStats(a, b eval.Stat) string {
	if a.N == 0 || b.N == 0 || math.IsNaN(a.Mean) || math.IsNaN(b.Mean) {
		return "not comparable with"
	}
	if a.N < 2 || b.N < 2 || math.IsNaN(a.CI95) || math.IsNaN(b.CI95) {
		return "not comparable (fewer than 2 runs, no confidence interval) with"
	}
	switch {
	case a.Mean+a.CI95 < b.Mean-b.CI95:
		return "lower than"
	case a.Mean-a.CI95 > b.Mean+b.CI95:
		return "higher than"
	}
	return "not distinguishable (95% CIs overlap) from"
}

func forecastMarkdown(rep ForecastReport) ([]byte, error) {
	var b bytes.Buffer
	w := func(format string, args ...any) { fmt.Fprintf(&b, format, args...) }
	runs := len(rep.Seeds)
	w("# Demand forecaster evaluation (simulated workload)\n\n")
	w("> **SIMULATED:** the series are synthetic traces from `workload.Generate`, and the control-loop effect comes from the discrete-event simulator (`internal/sim`) with the simulated engine latency model.\n\n")
	s := rep.Spec
	w("Forecaster: additive Holt-Winters (alpha %g, beta %g, gamma %g), compared with naive (last value) and seasonal-naive (value one season earlier). ", s.Alpha, s.Beta, s.Gamma)
	w("Series: per-bucket arrival demand in weighted tokens/s (input + 2 x max output tokens, what the controller observes); bucket = one control cycle = %gs; season = %d buckets (the periodic tenant's period). ", rep.BucketSeconds, rep.Season)
	w("Each series has %d buckets (%gs): the first %d (%.0f%%) train, the remaining %d are held out. Seeds %v; tables show mean ± 95%% CI over seeds.\n\n",
		rep.SeriesBuckets, float64(rep.SeriesBuckets)*rep.BucketSeconds, rep.TrainBuckets, 100*TrainFraction, rep.SeriesBuckets-rep.TrainBuckets, rep.Seeds)

	w("## Leakage statement\n\n")
	w("- The split is chronological: a model observes only the training buckets before it is scored; nothing is shuffled.\n")
	w("- Scoring is rolling one-step-ahead (`forecast.Evaluate`): each held-out bucket t is forecast BEFORE the model observes it, so a prediction uses only buckets < t. A spy-series unit test in `internal/forecast` checks this.\n")
	w("- Hyperparameters were fixed a priori and the season comes from the scenario's declared period; nothing was tuned on held-out buckets.\n")
	w("- Every seed is an independently generated trace.\n")
	if rep.Spike != "" {
		w("- For the failure analysis the spike tenant's spike is moved into the held-out part (start %gs), so no model sees it during training.\n", rep.SpikeStartSeconds)
	}

	w("\n## Accuracy: periodic tenant %s (held-out, rolling one-step)\n\n", rep.Periodic)
	if err := accuracyTable(rep.PeriodicAccuracy, runs).WriteMarkdown(&b); err != nil {
		return nil, err
	}
	if hw, base, ok := hwAndBest(rep.PeriodicAccuracy); ok {
		w("\nHolt-Winters MAPE is %s %s (%s vs %s).\n", compareStats(hw.MAPE, base.MAPE), base.Method,
			eval.FormatValue(hw.MAPE.Mean), eval.FormatValue(base.MAPE.Mean))
	}

	if rep.Spike != "" {
		w("\n## Failure analysis: spike tenant %s\n\n", rep.Spike)
		w("An unannounced spike has no history to learn from. Ratios are one-step forecast / actual; 1 is perfect, below 1 under-forecasts, above 1 over-forecasts.\n\n")
		t := eval.Table{Header: []string{"method", "MAPE %", "sMAPE %", "MAE (weighted tok/s)", "forecast/actual at onset", "forecast/actual after the spike"}}
		for i, a := range rep.SpikeAccuracy {
			pr := rep.SpikeProbes[i]
			t.Rows = append(t.Rows, append([]string{a.Method}, statCells(runs, a.MAPE, a.SMAPE, a.MAE, pr.Onset, pr.After)...))
		}
		if err := t.WriteMarkdown(&b); err != nil {
			return nil, err
		}
		if hw, ok := probeFor(rep.SpikeProbes, "holt-winters"); ok {
			w("\n- At the onset Holt-Winters forecasts %s of the actual demand: it does not anticipate the spike. ", eval.FormatValue(hw.Onset.Mean))
			w("An under-forecast costs nothing extra by construction: the controller acts on max(reactive, forecast), so it falls back on the reactive estimate, which sees the spike at the next control cycle.\n")
			verdict := "an over-forecast, which would hold replicas the tenant no longer needs"
			if !(hw.After.Mean > 1) {
				verdict = "no over-forecast"
			}
			w("- In the first bucket after the spike Holt-Winters forecasts %s of the actual demand: %s. ", eval.FormatValue(hw.After.Mean), verdict)
			w("The controller's trust gate (`forecast.Tracker`, %d scored buckets, distrusts a mean APE above 100%%) trusted the forecaster after %s%% of the buckets in the season following the spike.\n",
				trustWindow, statCells(runs, rep.TrustedAfterSpikePct)[0])
		}
	}

	w("\n## Effect on the control loop: S7 vs S7F (simulated)\n\n")
	t := eval.Table{Header: []string{"scenario", "setup", rep.Periodic + " p99 TTFT (s)", rep.Periodic + " SLO violation %", "replica-minutes",
		"cycles forecast raised " + rep.Periodic, "cycles forecast raised " + rep.Spike}}
	for _, c := range rep.Control {
		t.Rows = append(t.Rows, append([]string{fmt.Sprintf("%s (%gs)", c.Scenario, c.ScenarioSeconds), c.Setup},
			statCells(runs, c.PeriodicP99TTFT, c.PeriodicViolationPct, c.ReplicaMinutes, c.ForecastRaisedPeriodic, c.ForecastRaisedSpike)...))
	}
	if err := t.WriteMarkdown(&b); err != nil {
		return nil, err
	}
	for i := 0; i+1 < len(rep.Control); i += 2 {
		s7, s7f := rep.Control[i], rep.Control[i+1]
		w("\n- %s: with the forecaster, %s's SLO violation is %s S7's (%s%% vs %s%%) and replica-minutes are %s S7's (%s vs %s).",
			s7.Scenario, rep.Periodic, compareStats(s7f.PeriodicViolationPct, s7.PeriodicViolationPct),
			eval.FormatValue(s7f.PeriodicViolationPct.Mean), eval.FormatValue(s7.PeriodicViolationPct.Mean),
			compareStats(s7f.ReplicaMinutes, s7.ReplicaMinutes), eval.FormatValue(s7f.ReplicaMinutes.Mean), eval.FormatValue(s7.ReplicaMinutes.Mean))
		if rep.Spike != "" && s7f.ForecastRaisedSpike.Mean > 0 {
			w(" The forecast also raised the spike tenant %s's demand in %s cycles; %s has no recurring peak, so that capacity was not driven by a real seasonal pattern.",
				rep.Spike, eval.FormatValue(s7f.ForecastRaisedSpike.Mean), rep.Spike)
		}
	}
	w("\n\nHolt-Winters needs two full seasons before it forecasts and %d accurate buckets before it is trusted, so in a short run it acts only in the second half.\n", trustWindow)
	return b.Bytes(), nil
}

func accuracyTable(acc []MethodAccuracy, runs int) eval.Table {
	t := eval.Table{Header: []string{"method", "MAPE %", "sMAPE %", "MAE (weighted tok/s)"}}
	for _, a := range acc {
		t.Rows = append(t.Rows, append([]string{a.Method}, statCells(runs, a.MAPE, a.SMAPE, a.MAE)...))
	}
	return t
}

// hwAndBest returns Holt-Winters' accuracy and the best baseline by MAPE.
func hwAndBest(acc []MethodAccuracy) (hw, best MethodAccuracy, ok bool) {
	found := false
	for _, a := range acc {
		switch {
		case a.Method == "holt-winters":
			hw, found = a, true
		case best.Method == "" || a.MAPE.Mean < best.MAPE.Mean:
			best = a
		}
	}
	return hw, best, found && best.Method != ""
}

func probeFor(probes []SpikeProbe, method string) (SpikeProbe, bool) {
	for _, p := range probes {
		if p.Method == method {
			return p, true
		}
	}
	return SpikeProbe{}, false
}
