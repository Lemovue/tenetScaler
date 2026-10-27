package experiment

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"strings"
	"time"

	"tenantscaler/internal/config"
	"tenantscaler/internal/eval"
	"tenantscaler/internal/model"
	"tenantscaler/internal/sim"
)

// Parameters records everything a reader needs to reproduce the runs.
type Parameters struct {
	CapacityReplicas int                `json:"capacityReplicas"`
	MinReplicas      int                `json:"minReplicas"`
	InitialReplicas  int                `json:"initialReplicas"`
	SlotsPerReplica  int                `json:"slotsPerReplica"`
	Tenants          []model.TenantSpec `json:"tenants"`
	// Controller is the configured controller; ForecastS7F is the
	// forecaster that S7F switches on.
	Controller             config.ControllerSpec `json:"controller"`
	ForecastS7F            config.ForecastSpec   `json:"forecastS7F"`
	Engine                 config.EngineSpec     `json:"engine"`
	HPA                    ScalerParameters      `json:"hpa"`
	KEDA                   ScalerParameters      `json:"keda"`
	PolicyDelayMs          float64               `json:"policyDelayMs"`
	DispatchTimeoutSeconds float64               `json:"dispatchTimeoutSeconds"`
	SampleEverySeconds     float64               `json:"sampleEverySeconds"`
	DrainLimitSeconds      float64               `json:"drainLimitSeconds"`
}

// ScalerParameters are a baseline autoscaler's settings: Target is the
// HPA's utilisation target or KEDA's queued requests per replica.
type ScalerParameters struct {
	Target                 float64 `json:"target"`
	Tolerance              float64 `json:"tolerance"`
	ScaleDownWindowSeconds float64 `json:"scaleDownWindowSeconds"`
	Min                    int     `json:"min"`
	Max                    int     `json:"max"`
}

func parametersFor(p Plan) (Parameters, error) {
	st, err := sim.Resolve(sim.Options{Setup: sim.SetupKEDAQueue, Config: p.Config, Scenario: p.Scenario, ColdStart: p.DefaultColdStart})
	if err != nil {
		return Parameters{}, err
	}
	return Parameters{
		CapacityReplicas: p.Config.Pool.CapacityReplicas, MinReplicas: p.Config.Pool.MinReplicas,
		InitialReplicas: st.InitialReplicas, SlotsPerReplica: st.SlotsPerReplica,
		Tenants: p.Config.Tenants, Controller: p.Config.Controller,
		ForecastS7F: sim.ForecastSpecFor(p.Config.Controller, p.Scenario),
		Engine:      p.Config.Engine,
		HPA: ScalerParameters{Target: st.HPA.Target, Tolerance: st.HPA.Tolerance,
			ScaleDownWindowSeconds: st.HPA.ScaleDownWindow.Seconds(), Min: st.HPA.Min, Max: st.HPA.Max},
		KEDA: ScalerParameters{Target: st.KEDA.TargetPerReplica, Tolerance: st.KEDA.Tolerance,
			ScaleDownWindowSeconds: st.KEDA.ScaleDownWindow.Seconds(), Min: st.KEDA.Min, Max: st.KEDA.Max},
		PolicyDelayMs:          float64(st.PolicyDelay) / float64(time.Millisecond),
		DispatchTimeoutSeconds: st.DispatchTimeout.Seconds(),
		SampleEverySeconds:     st.SampleEvery.Seconds(),
		DrainLimitSeconds:      sim.DrainLimit.Seconds(),
	}, nil
}

// writeOutputs writes every result file.
func writeOutputs(out outcome) error {
	dir := out.plan.OutDir
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create %s: %w", dir, err)
	}
	var csv bytes.Buffer
	if err := runsTable(out).WriteCSV(&csv); err != nil {
		return err
	}
	if err := writeFile(dir, "runs.csv", csv.Bytes()); err != nil {
		return err
	}
	js, err := json.MarshalIndent(out.summary, "", "  ")
	if err != nil {
		return fmt.Errorf("encode summary: %w", err)
	}
	if err := writeFile(dir, "summary.json", append(js, '\n')); err != nil {
		return err
	}
	plots, err := writePlots(out)
	if err != nil {
		return err
	}
	md, err := summaryMarkdown(out, plots)
	if err != nil {
		return err
	}
	return writeFile(dir, "summary.md", md)
}

// runsTable lists every run at every cold start.
func runsTable(out outcome) eval.Table {
	var t eval.Table
	for _, pt := range out.points {
		rt := eval.RunsTable(pt.results, out.metrics)
		t.Header = append([]string{"cold_start_s"}, rt.Header...)
		cs := eval.FormatValue(pt.coldStart.Seconds())
		for _, row := range rt.Rows {
			t.Rows = append(t.Rows, append([]string{cs}, row...))
		}
	}
	return t
}

// transpose turns a summary table (one row per setup) into one row per
// metric with a column per setup, which reads better with many metrics.
func transpose(t eval.Table) eval.Table {
	out := eval.Table{Header: []string{"metric"}}
	for _, row := range t.Rows {
		out.Header = append(out.Header, row[0])
	}
	for c := 1; c < len(t.Header); c++ {
		row := []string{t.Header[c]}
		for _, r := range t.Rows {
			row = append(row, r[c])
		}
		out.Rows = append(out.Rows, row)
	}
	return out
}

// sweepTable has one row per setup and one victim-violation cell per cold
// start.
func sweepTable(out outcome) eval.Table {
	vi := metricIndex(out.metrics, metricVictim)
	victim := []eval.Metric{out.metrics[vi]}
	t := eval.Table{Header: []string{"setup"}}
	cells := map[string][]string{}
	for _, pt := range out.points {
		t.Header = append(t.Header, fmt.Sprintf("cold start %gs", pt.coldStart.Seconds()))
		var sums []eval.SetupSummary
		for _, s := range pt.sums {
			sums = append(sums, eval.SetupSummary{Setup: s.Setup, Runs: s.Runs, Stats: []eval.Stat{s.Stats[vi]}})
		}
		for _, row := range eval.SummaryTable(sums, victim).Rows {
			cells[row[0]] = append(cells[row[0]], row[2])
		}
	}
	for _, setup := range out.plan.Setups {
		t.Rows = append(t.Rows, append([]string{setup}, cells[setup]...))
	}
	return t
}

func summaryMarkdown(out outcome, plots []plotFile) ([]byte, error) {
	p, s := out.plan, out.summary
	var b bytes.Buffer
	w := func(format string, args ...any) { fmt.Fprintf(&b, format, args...) }
	w("# TenantScaler evaluation (simulated)\n\n> **%s**\n\n", Disclaimer)
	w("Scenario `%s` (%gs): noisy tenant **%s** spikes during [%gs, %gs); victims %s. ",
		s.Scenario, s.ScenarioSeconds, s.Noisy, s.SpikeStartSeconds, s.SpikeEndSeconds, strings.Join(s.Victims, ", "))
	w("%d runs per setup and cold start with seeds %v (the same seeds, hence the same request traces, for every setup). ", s.Runs, s.Seeds)
	w("Budget %d replicas; every scaling setup starts with %d warm replicas, the static partition with all %d.\n\n",
		s.Parameters.CapacityReplicas, s.Parameters.InitialReplicas, s.Parameters.CapacityReplicas)

	w("## Setups\n\n")
	setups := eval.Table{Header: []string{"setup", "gateway", "control plane"}}
	for _, si := range s.Setups {
		setups.Rows = append(setups.Rows, []string{si.Name, si.Gateway, si.ControlPlane})
	}
	if err := setups.WriteMarkdown(&b); err != nil {
		return nil, err
	}

	w("\n## Main results (cold start %gs; mean ± 95%% CI over runs, t-distribution)\n\n", p.DefaultColdStart.Seconds())
	w("`runs` is N; a cell marked `(n=k)` is defined in only k runs, `n/a` in none. Percentages are 0-100, times in seconds.\n\n")
	if err := transpose(eval.SummaryTable(out.main.sums, out.metrics)).WriteMarkdown(&b); err != nil {
		return nil, err
	}
	w("\n%s\n", metricGlossary)
	if note := backlogNote(out); note != "" {
		w("\n%s\n", note)
	}

	w("\n## Cold-start sweep: victim SLO violation %% during the spike (mean ± 95%% CI)\n\n")
	if err := sweepTable(out).WriteMarkdown(&b); err != nil {
		return nil, err
	}

	if len(plots) > 0 {
		w("\n## Plots (simulated)\n\n")
		for _, pf := range plots {
			w("![%s](%s)\n\n", pf.alt, pf.name)
		}
	}

	w("## Parameters\n\n")
	pr := s.Parameters
	ctl := pr.Controller
	w("- Tenants: ")
	for i, t := range pr.Tenants {
		if i > 0 {
			w("; ")
		}
		w("%s guaranteed %d, max %d, priority %d, SLO TTFT %gs / TPOT %gms / %g%%",
			t.Name, t.GuaranteedReplicas, t.MaxReplicas, t.Priority, t.SLO.TTFTSeconds, t.SLO.TPOTMillis, 100*t.SLO.TargetFraction)
	}
	w("\n- Pool: capacity %d, min %d, dispatch window %d per replica.\n", pr.CapacityReplicas, pr.MinReplicas, pr.SlotsPerReplica)
	w("- Controller (S5-S7F): cycle %gs, headroom %g, C = %g weighted tokens/s per replica, scale-down window %gs, max step %d, Retry-After %ds.\n",
		ctl.CycleSeconds, ctl.Headroom, ctl.ReplicaCapacityTokensPerSec, ctl.ScaleDownWindowSeconds, ctl.MaxStepPerCycle, ctl.RetryAfterSeconds)
	f := pr.ForecastS7F
	w("- S7F forecaster: Holt-Winters alpha %g, beta %g, gamma %g, bucket %gs, season %d buckets (fixed a priori).\n", f.Alpha, f.Beta, f.Gamma, f.BucketSeconds, f.SeasonBuckets)
	w("- Baselines: HPA target utilisation %g (engine slot occupancy as the CPU proxy, averaged over the cycle), KEDA %g requests per replica (queued for S2 and S4, outstanding for S4b); tolerance %g; scale-down window %gs; both synced every control cycle and capped at the budget.\n",
		pr.HPA.Target, pr.KEDA.Target, pr.HPA.Tolerance, pr.HPA.ScaleDownWindowSeconds)
	e := pr.Engine
	prefillFactor := e.PrefillFactorUnderLoad
	if prefillFactor == 0 {
		prefillFactor = e.TimeFactorUnderLoad
	}
	w("- Simulated engine: prefill %s + %s/token, inter-token %s, %d concurrent sequences, prefill x%g and decode x%g at full load, jitter %g.\n",
		e.PrefillOverhead, e.PrefillPerToken, e.InterTokenLatency, e.MaxNumSeqs, prefillFactor, e.TimeFactorUnderLoad, e.JitterFraction)
	w("- Policy propagation delay %gms (simulated); gateway dispatch timeout %gs (a request still queued at the gateway then fails, like the live gateway's 503); timeline every %gs; unfinished requests fail %gs after the scenario ends.\n",
		pr.PolicyDelayMs, pr.DispatchTimeoutSeconds, pr.SampleEverySeconds, pr.DrainLimitSeconds)
	w("\n%s", threatsMarkdown(p))
	return b.Bytes(), nil
}

// backlogNote says whether weighted-VTC ordering can have shaped the main
// results. It decides dispatch order only while requests queue at the fair
// gateway, and max_counter_gap is defined exactly in the runs where that
// happened. It is "" when no fair-gateway setup ran.
func backlogNote(out outcome) string {
	gi := metricIndex(out.metrics, metricCounterGap)
	var fair []string
	backlog := 0
	for _, s := range out.main.sums {
		if gw, _, _ := sim.Info(s.Setup); gw == sim.GatewayFair {
			fair = append(fair, fmt.Sprintf("%s %d/%d", s.Setup, s.Stats[gi].N, s.Runs))
			backlog += s.Stats[gi].N
		}
	}
	switch {
	case len(fair) == 0:
		return ""
	case backlog == 0:
		return "**VTC fairness never engaged in this configuration**: no fair-gateway setup ever had a gateway backlog, so " +
			"weighted-VTC ordering never decided a dispatch. The victim gap between the fair setups and the baselines reflects " +
			"shedding, least-loaded dispatch inside the dispatch window and scaling, not VTC fairness."
	}
	return "Weighted-VTC ordering decides dispatch order only while requests queue at the fair gateway. Runs with such a " +
		"backlog (max_counter_gap defined): " + strings.Join(fair, ", ") + ". In the other runs the fair setups' results " +
		"reflect shedding, least-loaded dispatch and scaling, not VTC fairness."
}

// threatsMarkdown is the threats-to-validity section for the plan.
func threatsMarkdown(p Plan) string {
	bullets := []string{
		"The engine is simulated: latencies follow the config's load model (calibrated to the real llama.cpp replica in " +
			"config/real.yaml, illustrative in the built-in default), with the load fixed at each request's start.",
		"The headline results are real runs on a budget of 4 llama.cpp replicas, one per physical P-core (results/real/); " +
			"this appendix only extends them to setups, budgets and cold starts the laptop cannot run.",
		"Shed and timed-out requests are not retried; real clients would retry after Retry-After and add load.",
		"Policy propagation is a constant delay; the live controller-to-gateway push has variable latency.",
		"The CPU-style HPA uses engine slot occupancy as its CPU proxy.",
	}
	if slices.Contains(p.Setups, sim.SetupFlowControl) {
		bullets = append(bullets, "S4, the closest related-work baseline, is weak by construction. As DESIGN section 14 "+
			"specifies, its KEDA trigger is the roundrobin gateway's queue. That queue excludes the up to `slotsPerReplica` "+
			"requests per replica already dispatched, and the gateway sheds sheddable overflow as `saturated` instead of "+
			"queueing it, so the queue reads about zero while the pool is saturated and S4 can scale down in the middle of a "+
			"spike once its scale-down window expires. The sensitivity variant `"+sim.SetupFlowControlOutstanding+"` (same "+
			"gateway, KEDA on gateway queue + dispatched requests; included by `-setups all`) shows how much of S4's result "+
			"comes from that metric: compare TenantScaler with it, not only with S4.")
	}
	bullets = append(bullets, "How much the gateway matters depends on whether the replica budget binds. C "+
		"(`tenantscaler profile`) is measured on ONE replica; a pool of replicas sustains more per replica (multiplexing over "+
		"all slots), so a budget sized by C can still absorb a spike once enough replicas are up, and then setups that scale up "+
		"early look alike for the victims. Rerun with `-capacity` (for example `-capacity 6`; tenant guarantees are scaled "+
		"down to fit) to evaluate a binding budget.")
	var b strings.Builder
	b.WriteString("## Threats to validity\n\n")
	for _, s := range bullets {
		b.WriteString("- " + s + "\n")
	}
	return b.String()
}

const metricGlossary = `Metric notes: ` + "`victim_violation_pct`" + ` = victims' requests arriving in the spike window that missed the TTFT/TPOT SLO (shed requests excluded); ` +
	"`victim_shed_aware_violation_pct`" + ` counts shed victim requests as misses; ` + "`noisy_shed_pct`" + ` = noisy tenant's requests shed over the run; ` +
	"`reaction_time_s`" + ` = first protective action (scale-up or shedding policy) after the spike starts; ` + "`replica_minutes`" + ` = replicas holding compute (ready or draining) integrated over the scenario; ` +
	"`jain_index`" + ` = Jain's fairness of SLO attainment during the spike; ` + "`max_counter_gap`" + ` = largest VTC counter gap among backlogged tenants (fair gateway only; n/a when the gateway never had a backlog); ` +
	"`reclaim_latency_s`" + ` applies to the reclaim scenario only.`
