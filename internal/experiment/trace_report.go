package experiment

import (
	"bytes"
	"fmt"
	"slices"

	"tenantscaler/internal/eval"
)

// TraceMarkdown is forecast-real.md: the forecasters' accuracy on held-out
// days of the real trace and their effect on tenant B in simulation.
func TraceMarkdown(rep TraceReport) ([]byte, error) {
	var b bytes.Buffer
	w := func(format string, a ...any) { fmt.Fprintf(&b, format, a...) }
	w("# Demand forecaster on a real trace\n\n")
	w("Trace: %s. Only the timestamps and token counts are used.\n\n", TraceCitation)
	w("Series: %d requests bucketed into %d buckets of %g s (weighted tokens/s = context + 2 x generated tokens); "+
		"season = %d buckets (one day). %d rows were timestamped before the first row and counted in bucket 0.\n\n",
		rep.Rows, rep.Buckets, rep.BucketSeconds, rep.Season, rep.Early)
	w("## Accuracy on held-out days (REAL trace)\n\n")
	w("Each of the last whole days is scored after observing every earlier bucket: rolling one-step forecasts, each made " +
		"BEFORE its bucket is observed (`forecast.Evaluate`), so no forecast uses its own or later data. Holt-Winters uses the " +
		"S7F hyperparameters fixed a priori (alpha 0.5, beta 0.1, gamma 0.3); nothing was tuned on these days.\n\n")
	days := eval.Table{Header: []string{"held-out day (season index)", "method", "MAPE %", "sMAPE %", "MAE (weighted tok/s)", "scored buckets"}}
	for _, a := range rep.Accuracy {
		days.Rows = append(days.Rows, []string{fmt.Sprint(a.Season), a.Method, eval.FormatValue(a.Metrics.MAPE),
			eval.FormatValue(a.Metrics.SMAPE), eval.FormatValue(a.Metrics.MAE), fmt.Sprint(a.Metrics.N)})
	}
	if err := days.WriteMarkdown(&b); err != nil {
		return nil, err
	}
	w("\nMean MAPE over the held-out days (95%% CI, Student t):\n\n")
	mt := eval.Table{Header: []string{"method", "MAPE %", "days"}}
	for _, name := range sortedNames(rep.MAPE) {
		st := rep.MAPE[name]
		mt.Rows = append(mt.Rows, []string{name, statCell(st), fmt.Sprint(st.N)})
	}
	if err := mt.WriteMarkdown(&b); err != nil {
		return nil, err
	}
	if hw, sn := rep.MAPE["holt-winters"], rep.MAPE["seasonal-naive"]; hw.N > 0 && sn.N > 0 {
		w("\nHolt-Winters' MAPE is %s seasonal-naive's (%s vs %s).\n", compareStats(hw, sn), eval.FormatValue(hw.Mean), eval.FormatValue(sn.Mean))
	}
	w("\n## Effect on tenant B (SIMULATED, driven by the real trace)\n\n")
	w("Tenant B's request rate follows the trace's last %d days, rescaled by %.4g so its 95th-percentile bucket needs 2 "+
		"replicas (B's guarantee is 1), one trace bucket per 10 s control cycle; A and C run steady at their base rates. "+
		"The first two days let Holt-Winters learn; B's numbers are over the remaining days. Calibrated simulated engine "+
		"(config/real.yaml), budget 4. Reaction time: for each of B's %d step-ups in replica need, the time from the step "+
		"until the pool's desired size first rose above its level 30 s before the step (negative = pre-warmed by the "+
		"forecast); steps with no rise within 60 s are counted as unreacted.\n\n",
		rep.SimulatedSeasons, rep.Scale, rep.Steps)
	et := eval.Table{Header: []string{"setup", "B SLO violation %", "B p99 TTFT s", "replica-minutes", "B reaction time s", "unreacted steps"}}
	for _, e := range rep.Effects {
		et.Rows = append(et.Rows, []string{e.Setup, statCell(e.BViolationPct), statCell(e.BP99TTFT),
			statCell(e.ReplicaMinutes), statCell(e.ReactionSeconds), statCell(e.Unreacted)})
	}
	if err := et.WriteMarkdown(&b); err != nil {
		return nil, err
	}
	if len(rep.Effects) == 2 {
		s7, s7f := rep.Effects[0], rep.Effects[1]
		w("\nWith the forecaster (S7F), B's SLO violation is %s S7's (without it); B's reaction time is %s S7's; replica-minutes are %s S7's.\n",
			compareStats(s7f.BViolationPct, s7.BViolationPct), compareStats(s7f.ReactionSeconds, s7.ReactionSeconds),
			compareStats(s7f.ReplicaMinutes, s7.ReplicaMinutes))
	}
	return b.Bytes(), nil
}

// statCell renders "mean ± ci95" (no CI with one value; n/a with none).
func statCell(st eval.Stat) string {
	if st.N == 0 {
		return "n/a"
	}
	if st.N == 1 {
		return eval.FormatValue(st.Mean)
	}
	return eval.FormatValue(st.Mean) + " ± " + eval.FormatValue(st.CI95)
}

func sortedNames(m map[string]eval.Stat) []string {
	names := make([]string, 0, len(m))
	for k := range m {
		names = append(names, k)
	}
	slices.Sort(names)
	return names
}
