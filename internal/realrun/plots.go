package realrun

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

// writePlots writes the SVG charts and returns them in report order: the
// victims' violation per setup with 95% CI error bars, and for run 0 the
// ready replicas and the victims' p99 TTFT over time of S2 vs S7 (or of
// the first two setups when those were not run).
func writePlots(p Plan, sum Summary) ([]plotFile, error) {
	var files []plotFile
	add := func(name, alt string, chart interface{ WriteSVG(io.Writer) error }) error {
		var b bytes.Buffer
		if err := chart.WriteSVG(&b); err != nil {
			return fmt.Errorf("plot %s: %w", name, err)
		}
		if err := writeFile(p.OutDir, name, b.Bytes()); err != nil {
			return err
		}
		files = append(files, plotFile{name: name, alt: alt})
		return nil
	}
	label := "real llama.cpp replicas"
	if sum.Simulated {
		label = "SIMULATED replicas"
	}
	if len(sum.Stats) > 0 {
		if err := add(violationPlotFile, "Victim SLO violation by setup, mean with 95% CI", victimChart(sum, label)); err != nil {
			return nil, err
		}
	}
	runs := timelineRuns(sum)
	if len(runs) == 0 {
		return files, nil
	}
	if err := add(replicasPlotFile, "Ready replicas over time, run 0", replicaTimeline(runs, label)); err != nil {
		return nil, err
	}
	if err := add(ttftPlotFile, "Victim p99 TTFT over time, run 0", ttftTimeline(p, runs, label)); err != nil {
		return nil, err
	}
	return files, nil
}

// victimChart is the victims' violation per setup (served requests, and
// shed counted as misses) with 95% CI error bars.
func victimChart(sum Summary, label string) eval.BarChart {
	c := eval.BarChart{
		Title:  fmt.Sprintf("Victim SLO violation during the spike (%s, mean ± 95%% CI over runs)", label),
		YLabel: "% of victim requests", Width: 960, Height: 420,
		Series: []eval.BarSeries{{Name: "served requests"}, {Name: "shed counted as miss"}},
	}
	for _, s := range sum.Stats {
		c.Groups = append(c.Groups, fmt.Sprintf("%s (N=%d)", s.Setup, s.Runs))
		for k, name := range []string{"victim_violation_pct", "victim_shed_aware_violation_pct"} {
			st := s.Metrics[name]
			c.Series[k].Values = append(c.Series[k].Values, st.Mean)
			c.Series[k].Errors = append(c.Series[k].Errors, st.CI95)
		}
	}
	return c
}

// timelineRuns picks run 0 of S2 and S7, or of the first two setups when
// neither was run.
func timelineRuns(sum Summary) []eval.RunData {
	pick := func(setups []string) []eval.RunData {
		var out []eval.RunData
		for _, s := range setups {
			for _, r := range sum.Results {
				if r.Setup == s && r.Run == 0 {
					out = append(out, r.data)
				}
			}
		}
		return out
	}
	if runs := pick([]string{sim.SetupKEDAQueue, sim.SetupFull}); len(runs) > 0 {
		return runs
	}
	var first []string
	for _, s := range sum.Setups {
		first = append(first, s.Name)
	}
	return pick(first[:min(2, len(first))])
}

// replicaTimeline plots ready replicas over time.
func replicaTimeline(runs []eval.RunData, label string) eval.LineChart {
	c := eval.LineChart{
		Title:  fmt.Sprintf("Ready replicas, run 0 (%s)", label),
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

// ttftTimeline plots the victims' trailing p99 TTFT and their TTFT SLO.
func ttftTimeline(p Plan, runs []eval.RunData, label string) eval.LineChart {
	roles := sim.ScenarioRoles(p.Scenario)
	c := eval.LineChart{
		Title: fmt.Sprintf("Victim p99 TTFT, trailing %gs, run 0 (%s; spike %g-%gs)",
			victimTTFTWindow.Seconds(), label, roles.SpikeStart.Seconds(), roles.SpikeEnd.Seconds()),
		XLabel: "time (s)", YLabel: "seconds", Width: 960, Height: 360,
	}
	for _, d := range runs {
		xs, ys := victimTTFT(d, victimTTFTWindow)
		c.Series = append(c.Series, eval.Series{Name: d.Setup + " victims p99 TTFT", X: xs, Y: ys})
	}
	if slo, ok := victimTTFTSLO(p, roles.Victims); ok {
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
func victimTTFTSLO(p Plan, victims []string) (float64, bool) {
	v := math.NaN()
	for _, t := range p.Config.Tenants {
		if !slices.Contains(victims, t.Name) {
			continue
		}
		if !math.IsNaN(v) && v != t.SLO.TTFTSeconds {
			return 0, false
		}
		v = t.SLO.TTFTSeconds
	}
	return v, !math.IsNaN(v)
}

// victimTTFT returns, at every timeline sample, the p99 TTFT of victim
// requests whose first token came in the trailing window (NaN if none).
func victimTTFT(d eval.RunData, window time.Duration) (xs, ys []float64) {
	for _, s := range d.Timeline {
		var ttfts []float64
		for _, r := range d.Records {
			if slices.Contains(d.Victims, r.Tenant) && r.Completed() && r.FirstToken <= s.T && r.FirstToken > s.T-window {
				ttfts = append(ttfts, r.TTFT().Seconds())
			}
		}
		xs = append(xs, s.T.Seconds())
		ys = append(ys, eval.Percentile(ttfts, 99))
	}
	return xs, ys
}
