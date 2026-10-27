package realrun

import (
	"bytes"
	"fmt"
	"maps"
	"slices"
	"strings"

	"tenantscaler/internal/eval"
	"tenantscaler/internal/gateway"
	"tenantscaler/internal/pool"
	"tenantscaler/internal/sim"
	"tenantscaler/internal/stack"
)

// summaryMarkdown renders summary.md: the label, the setting, the main
// table (every metric, mean ± 95% CI, N per setup), the plots, the setups,
// per-run facts and what was not measured.
func summaryMarkdown(p Plan, sum Summary, metrics []eval.Metric, plots []plotFile) ([]byte, error) {
	var b bytes.Buffer
	title := "Evaluation on real replicas"
	if sum.Simulated {
		title = "Evaluation on SIMULATED replicas"
	}
	fmt.Fprintf(&b, "# %s\n\n**%s**\n\n", title, sum.Label)
	sc := sum.Scenario
	fmt.Fprintf(&b, "Scenario `%s`: %.0f s; noisy tenant %s spikes from %.0f s to %.0f s; victims %s. "+
		"Every setup replays the same trace in real time (seed %s for run k = 0..%d, the same for every setup), "+
		"one setup at a time on a fresh pool: %d replicas started and ready before traffic (the static partition: all %d), "+
		"budget %d replicas, %d slots per replica, control cycle %g s, %s cooldown between runs. "+
		"Every control loop makes its first sync a seeded delay in [0, cycle) after its stack starts (the same delay for "+
		"every setup of a run index; listed per run below), so runs sample the phase of the control cycle relative to the spike. "+
		"Latencies are measured by the load generator (wall clock, streaming).\n\n",
		sc.Name, sc.DurationSeconds, sc.Noisy, sc.SpikeStartSeconds, sc.SpikeEndSeconds, strings.Join(sc.Victims, ", "),
		seedRange(sum.Seeds), max(sum.Runs-1, 0), sum.InitialReplicas, sum.CapacityReplicas, sum.CapacityReplicas,
		sum.SlotsPerReplica, sum.CycleSeconds, p.Cooldown)
	if done := len(sum.Results); done < sum.Runs*len(sum.Setups) {
		fmt.Fprintf(&b, "**Incomplete:** %d of %d planned runs finished.\n\n", done, sum.Runs*len(sum.Setups))
	}
	b.WriteString("## Main results (mean ± 95% CI over runs)\n\n")
	if err := MainTable(sum, metrics).WriteMarkdown(&b); err != nil {
		return nil, err
	}
	b.WriteString("\n" + metricNotes + "\n\n")
	for _, pf := range plots {
		fmt.Fprintf(&b, "![%s](%s)\n\n", pf.alt, pf.name)
	}
	if sum.Claim != nil {
		b.WriteString(claimMarkdown(sum.Claim, sum.ReadinessHoldSeconds))
	}
	b.WriteString("## Setups\n\n")
	st := eval.Table{Header: []string{"setup", "gateway", "control plane"}}
	for _, s := range sum.Setups {
		st.Rows = append(st.Rows, []string{s.Name, s.Gateway, s.ControlPlane})
	}
	if err := st.WriteMarkdown(&b); err != nil {
		return nil, err
	}
	b.WriteString("\nKEDA (S2/S4) uses the Kubernetes defaults of internal/autoscale: 4 queued requests per replica, " +
		"activation 0, scale-down window 300 s, at most max(4, current) replicas added per sync.\n\n")
	b.WriteString("## Runs\n\nEvery run, in the order it ran (`#` = sequence; `order` = position in its run index's seeded " +
		"setup order, decision Q14). CPU % is the effective frequency as a percentage of nominal when traffic started and ended.\n\n")
	if err := runsFactsTable(sum).WriteMarkdown(&b); err != nil {
		return nil, err
	}
	b.WriteString("\n## Drift check (first vs last run of each setup)\n\n" +
		"Each setup's first and last run (by sequence) on the main metric and the CPU frequency. They use different seeds " +
		"(different traces), so a change here mixes drift and trace; a change in the same direction for every setup points " +
		"to drift of the machine.\n\n")
	if err := driftTable(sum).WriteMarkdown(&b); err != nil {
		return nil, err
	}
	b.WriteString("\nPer-run raw data: `records/<setup>-run<k>.csv` (one row per request), `timeline/<setup>-run<k>.csv` " +
		"(1 s samples), `decisions/<setup>-run<k>.jsonl` (controller setups: every decision with its reason); " +
		"all metrics per run in `runs.csv`, everything in `summary.json`.\n\n")
	b.WriteString("## What was NOT measured\n\n")
	for _, line := range notMeasured(p, sum) {
		b.WriteString("- " + line + "\n")
	}
	return b.Bytes(), nil
}

// metricNotes explains the metric columns.
const metricNotes = "`victim_violation_pct`: share of the victims' requests arriving during the spike that missed " +
	"their TTFT or TPOT SLO (shed requests excluded, failed ones count as misses); `victim_shed_aware_violation_pct` also " +
	"counts shed requests as misses; `noisy_shed_pct`: share of the noisy tenant's requests rejected with 429; " +
	"`reaction_time_s`: first protective action (scale-up or newly applied shedding) after the spike starts, which " +
	"includes the wait for the next control cycle (its phase is sampled per run, see above); " +
	"`replica_minutes`: integral of ready + starting replicas over the scenario; `jain_index`: Jain's fairness of SLO " +
	"attainment / target during the spike; `max_counter_gap`: largest virtual-token-counter gap between backlogged " +
	"tenants (fair gateway only); `policy_latency_mean_ms`: controller-to-gateway policy propagation; " +
	"`p99_*`: per-tenant p99 over completed requests (seconds); n/a = not defined for that setup."

// runsFactsTable lists every run with its request outcomes, warm pool,
// measured start-up, control phase, and the number and mean cold start of
// the replicas its pool brought up (scaled-away ones included).
func runsFactsTable(sum Summary) eval.Table {
	t := eval.Table{Header: []string{"#", "setup", "run", "order", "seed", "victim violation %", "victim shed-aware violation %",
		"noisy shed %", "requests", "completed", "shed", "failed", "failure causes", "warm replicas", "stack start s",
		"control phase s", "replicas started", "mean cold start s", "drain timed out", "CPU % start/end", "tokenizer fallbacks"}}
	for _, r := range sum.Results {
		phase := "-"
		if r.ControlPhaseSeconds != nil {
			phase = eval.FormatValue(*r.ControlPhaseSeconds)
		}
		m := r.Metrics
		t.Rows = append(t.Rows, []string{fmt.Sprint(r.Sequence), r.Setup, fmt.Sprint(r.Run), fmt.Sprint(r.Order), fmt.Sprint(r.Seed),
			eval.FormatValue(m.VictimViolationPct), eval.FormatValue(m.VictimShedAwareViolationPct), eval.FormatValue(m.NoisyShedPct),
			fmt.Sprint(r.Requests), fmt.Sprint(m.Completed), fmt.Sprint(m.Shed), fmt.Sprint(m.Failed),
			causes(r.FailedBy), fmt.Sprint(r.InitialReplicas), eval.FormatValue(r.StartupSeconds), phase,
			fmt.Sprint(len(r.ColdStartSeconds)), eval.FormatValue(meanOf(r.ColdStartSeconds)), fmt.Sprint(r.DrainTimedOut),
			pctOrDash(r.CPUPerformanceStartPct) + " / " + pctOrDash(r.CPUPerformanceEndPct), fmt.Sprint(r.TokenizerFallbacks)})
	}
	return t
}

// driftTable compares each setup's first and last run (by sequence).
func driftTable(sum Summary) eval.Table {
	t := eval.Table{Header: []string{"setup", "first run (#)", "last run (#)", "victim violation % first", "last",
		"change", "CPU % first", "CPU % last"}}
	for _, s := range sum.Setups {
		var first, last *RunResult
		for i := range sum.Results {
			r := &sum.Results[i]
			if r.Setup != s.Name {
				continue
			}
			if first == nil || r.Sequence < first.Sequence {
				first = r
			}
			if last == nil || r.Sequence > last.Sequence {
				last = r
			}
		}
		if first == nil || first == last {
			continue
		}
		f, l := first.Metrics.VictimViolationPct, last.Metrics.VictimViolationPct
		t.Rows = append(t.Rows, []string{s.Name, fmt.Sprintf("run %d (#%d)", first.Run, first.Sequence),
			fmt.Sprintf("run %d (#%d)", last.Run, last.Sequence), eval.FormatValue(f), eval.FormatValue(l), eval.FormatValue(l - f),
			pctOrDash(first.CPUPerformanceStartPct), pctOrDash(last.CPUPerformanceStartPct)})
	}
	return t
}

// pctOrDash formats an optional percentage, "-" when not readable.
func pctOrDash(v *float64) string {
	if v == nil {
		return "-"
	}
	return eval.FormatValue(*v)
}

// causes renders failure counts by cause, e.g. "http 503: 2; aborted: 1".
func causes(byCause map[string]int) string {
	if len(byCause) == 0 {
		return "-"
	}
	parts := make([]string, 0, len(byCause))
	for _, c := range slices.Sorted(maps.Keys(byCause)) {
		parts = append(parts, fmt.Sprintf("%s: %d", c, byCause[c]))
	}
	return strings.Join(parts, "; ")
}

// notMeasured lists what these results do not cover.
func notMeasured(p Plan, sum Summary) []string {
	var lines []string
	var skipped []string
	for _, s := range append(slices.Clone(sim.AllSetups), sim.SensitivitySetups...) {
		if !slices.Contains(p.Setups, s) {
			skipped = append(skipped, s)
		}
	}
	if len(skipped) > 0 {
		lines = append(lines, fmt.Sprintf("Setups %s: not part of these real runs; the SIMULATED evaluation (`tenantscaler experiment`) covers every setup.",
			strings.Join(skipped, ", ")))
	}
	lines = append(lines, "CPU temperature: not readable without administrator rights (the ACPI thermal zones Windows "+
		"exposes are not CPU sensors); the CPU's effective frequency is logged per run instead.")
	if sum.ReadinessHoldSeconds > 0 {
		lines = append(lines, fmt.Sprintf("Model load time: each replica's readiness is held %g s after launch, an %s; "+
			"the replicas' own load takes well under a second.", sum.ReadinessHoldSeconds, pool.HoldLabel))
	}
	if sum.Simulated {
		lines = append(lines, "Real inference: these replicas are SIMULATED (modelled latencies).")
	} else {
		lines = append(lines, "GPU inference: replicas run on CPU cores only (backend.llama.gpuLayers 0, enforced; PLAN.md decision Q8); a GPU pool would behave differently.")
	}
	var colds []float64
	for _, r := range sum.Results {
		colds = append(colds, r.ColdStartSeconds...)
	}
	lines = append(lines,
		fmt.Sprintf("Cold-start sensitivity: the replicas' cold start was not varied (mean %s s over the %d replicas the runs started); the cold-start sweep exists only in the simulated evaluation.",
			eval.FormatValue(meanOf(colds)), len(colds)),
		"Kubernetes: the pool is a local process manager, not a Deployment; KEDA and the controller act on it directly.",
		"The demand forecaster (S7F) is off in every real setup.",
		"Isolation of the measurement: the load generator, gateway and controller share this machine with the replicas (they are not pinned; replicas are).",
		"Tokens: the gateway and controller count prompt words, the replica's tokenizer counts tokens (see results/real/profile.md for the ratio).")
	if sum.Scenario.Noisy == "" {
		lines = append(lines, "Victim and noisy-tenant metrics: the scenario has no spike tenant.")
	}
	if roles := sim.ScenarioRoles(p.Scenario); roles.Owner == "" {
		lines = append(lines, "Reclaim latency: the scenario has no returning owner (n/a).")
	}
	hasFair, hasPassthrough := false, false
	for _, s := range p.Setups {
		gw, _, _ := stack.Describe(s)
		hasFair = hasFair || gw == gateway.ModeFair
		hasPassthrough = hasPassthrough || gw == gateway.ModePassthrough
	}
	if hasPassthrough {
		lines = append(lines, "Per-tenant queues inside the replicas: with the passthrough gateway, requests waiting inside a replica count as in flight in the timeline (replicas do not report their queue per tenant).")
	}
	if !hasFair {
		lines = append(lines, "Counter gap: no setup with the fair gateway was run.")
	}
	return lines
}

// seedRange renders the seeds as "a" or "a..b".
func seedRange(seeds []int64) string {
	switch len(seeds) {
	case 0:
		return "none"
	case 1:
		return fmt.Sprint(seeds[0])
	}
	return fmt.Sprintf("%d..%d", seeds[0], seeds[len(seeds)-1])
}
