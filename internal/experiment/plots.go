package experiment

import (
	"bytes"
	"fmt"
	"io"
	"math"
	"slices"
	"time"

	"tenantscaler/internal/eval"
	"tenantscaler/internal/sim"
)

// plotFile is one written SVG and its alt text.
type plotFile struct{ name, alt string }

// victimTTFTWindow is the trailing window of the victim p99 TTFT timeline.
const victimTTFTWindow = 10 * time.Second

// writePlots writes the SVG charts and returns them in report order.
func writePlots(out outcome) ([]plotFile, error) {
	var files []plotFile
	add := func(name, alt string, chart interface{ WriteSVG(io.Writer) error }) error {
		var b bytes.Buffer
		if err := chart.WriteSVG(&b); err != nil {
			return fmt.Errorf("plot %s: %w", name, err)
		}
		if err := writeFile(out.plan.OutDir, name, b.Bytes()); err != nil {
			return err
		}
		files = append(files, plotFile{name: name, alt: alt})
		return nil
	}
	if err := add("victim_violation.svg", "Victim SLO violation by setup with 95% CI", victimChart(out)); err != nil {
		return nil, err
	}
	if len(out.points) > 1 {
		specified, variants := splitSetups(out.plan.Setups)
		ablations := slices.DeleteFunc(slices.Clone(specified), func(s string) bool { return !slices.Contains(tenantScalerAblations, s) })
		specified = slices.DeleteFunc(specified, func(s string) bool { return slices.Contains(tenantScalerAblations, s) })
		if len(specified) > 0 {
			if err := add("cold_start_sweep.svg", "Victim SLO violation versus cold start",
				sweepChart(out, "Victim SLO violation vs replica cold start (simulated, mean over runs)", specified)); err != nil {
				return nil, err
			}
		}
		if len(ablations) > 0 {
			if slices.Contains(out.plan.Setups, sim.SetupFull) {
				ablations = append([]string{sim.SetupFull}, ablations...)
			}
			if err := add("cold_start_sweep_ablations.svg", "Victim SLO violation versus cold start: TenantScaler ablations next to S7",
				sweepChart(out, "TenantScaler ablations vs cold start (simulated, mean over runs)", ablations)); err != nil {
				return nil, err
			}
		}
		if len(variants) > 0 {
			var setups []string
			for _, s := range sensitivityReference {
				if slices.Contains(out.plan.Setups, s) {
					setups = append(setups, s)
				}
			}
			if err := add("cold_start_sweep_sensitivity.svg", "Victim SLO violation versus cold start: sensitivity variants next to S4 and S7",
				sweepChart(out, "Sensitivity variants vs cold start (simulated, mean over runs)", append(setups, variants...))); err != nil {
				return nil, err
			}
		}
	}
	s2, ok2 := out.examples[sim.SetupKEDAQueue]
	s7, ok7 := out.examples[sim.SetupFull]
	if ok2 && ok7 {
		if err := add("timeline_replicas.svg", "Ready replicas over time, S2 vs S7", replicaTimeline(s2, s7)); err != nil {
			return nil, err
		}
		if err := add("timeline_victim_ttft.svg", "Victim p99 TTFT over time, S2 vs S7", ttftTimeline(out, s2, s7)); err != nil {
			return nil, err
		}
	}
	return files, nil
}

func victimChart(out outcome) eval.BarChart {
	vi, si := metricIndex(out.metrics, metricVictim), metricIndex(out.metrics, metricVictimShedAware)
	c := eval.BarChart{
		Title:  fmt.Sprintf("Victim SLO violation during the spike (simulated, cold start %gs, %d runs)", out.plan.DefaultColdStart.Seconds(), out.plan.Runs),
		YLabel: "% of victim requests", Width: 960, Height: 420,
		Series: []eval.BarSeries{{Name: "served requests"}, {Name: "shed counted as miss"}},
	}
	for _, s := range out.main.sums {
		c.Groups = append(c.Groups, s.Setup)
		for k, idx := range []int{vi, si} {
			c.Series[k].Values = append(c.Series[k].Values, s.Stats[idx].Mean)
			c.Series[k].Errors = append(c.Series[k].Errors, s.Stats[idx].CI95)
		}
	}
	return c
}

// sensitivityReference are the setups the sensitivity chart draws next to
// the variants: the baseline S4b varies, and TenantScaler.
var sensitivityReference = []string{sim.SetupFlowControl, sim.SetupFull}

// splitSetups separates the specified setups (sim.AllSetups) from the
// sensitivity variants, keeping plan order. A line chart holds at most 8
// distinct colours, as many as there are specified setups.
func splitSetups(setups []string) (specified, variants []string) {
	for _, s := range setups {
		if slices.Contains(sim.AllSetups, s) {
			specified = append(specified, s)
		} else {
			variants = append(variants, s)
		}
	}
	return specified, variants
}

// sweepChart draws the victims' violation against the cold start, one
// line per setup.
func sweepChart(out outcome, title string, setups []string) eval.LineChart {
	vi := metricIndex(out.metrics, metricVictim)
	c := eval.LineChart{
		Title:  title,
		XLabel: "cold start (s)", YLabel: "victim violation %", Width: 960, Height: 420,
	}
	for _, setup := range setups {
		ser := eval.Series{Name: setup}
		for _, pt := range out.points {
			for _, s := range pt.sums {
				if s.Setup == setup {
					ser.X = append(ser.X, pt.coldStart.Seconds())
					ser.Y = append(ser.Y, s.Stats[vi].Mean)
				}
			}
		}
		c.Series = append(c.Series, ser)
	}
	return c
}

func replicaTimeline(runs ...eval.RunData) eval.LineChart {
	c := eval.LineChart{
		Title:  "Ready replicas, run 1 (simulated)",
		XLabel: "time (s)", YLabel: "replicas", Width: 960, Height: 360,
	}
	for _, d := range runs {
		ser := eval.Series{Name: d.Setup + " ready"}
		for _, s := range d.Timeline {
			ser.X = append(ser.X, s.T.Seconds())
			ser.Y = append(ser.Y, float64(s.Ready))
		}
		c.Series = append(c.Series, ser)
	}
	return c
}

func ttftTimeline(out outcome, runs ...eval.RunData) eval.LineChart {
	roles := sim.ScenarioRoles(out.plan.Scenario)
	c := eval.LineChart{
		Title: fmt.Sprintf("Victim p99 TTFT, trailing %gs (simulated; spike %g-%gs)",
			victimTTFTWindow.Seconds(), roles.SpikeStart.Seconds(), roles.SpikeEnd.Seconds()),
		XLabel: "time (s)", YLabel: "seconds", Width: 960, Height: 360,
	}
	for _, d := range runs {
		xs, ys := victimTTFT(d, victimTTFTWindow)
		c.Series = append(c.Series, eval.Series{Name: d.Setup + " victims p99 TTFT", X: xs, Y: ys})
	}
	if slo, ok := victimTTFTSLO(out, roles.Victims); ok && len(runs) > 0 {
		ser := eval.Series{Name: "TTFT SLO"}
		for _, s := range runs[0].Timeline {
			ser.X = append(ser.X, s.T.Seconds())
			ser.Y = append(ser.Y, slo)
		}
		c.Series = append(c.Series, ser)
	}
	return c
}

// victimTTFTSLO returns the victims' common TTFT bound, if they share one.
func victimTTFTSLO(out outcome, victims []string) (float64, bool) {
	v := math.NaN()
	for _, t := range out.plan.Config.Tenants {
		for _, name := range victims {
			if t.Name != name {
				continue
			}
			if !math.IsNaN(v) && v != t.SLO.TTFTSeconds {
				return 0, false
			}
			v = t.SLO.TTFTSeconds
		}
	}
	return v, !math.IsNaN(v)
}

// victimTTFT returns, at every timeline sample, the p99 TTFT of victim
// requests whose first token came in the trailing window (NaN if none).
func victimTTFT(d eval.RunData, window time.Duration) (xs, ys []float64) {
	victim := map[string]bool{}
	for _, v := range d.Victims {
		victim[v] = true
	}
	for _, s := range d.Timeline {
		var ttfts []float64
		for _, r := range d.Records {
			if victim[r.Tenant] && r.Completed() && r.FirstToken <= s.T && r.FirstToken > s.T-window {
				ttfts = append(ttfts, r.TTFT().Seconds())
			}
		}
		xs = append(xs, s.T.Seconds())
		ys = append(ys, eval.Percentile(ttfts, 99))
	}
	return xs, ys
}

// tenantScalerAblations are drawn on their own cold-start chart next to S7,
// so no chart has more series than distinct colours.
var tenantScalerAblations = []string{sim.SetupNoGateway, sim.SetupNoBorrowing, sim.SetupNaiveDemand}
