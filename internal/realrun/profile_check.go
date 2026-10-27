package realrun

import (
	"context"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"tenantscaler/internal/config"
	"tenantscaler/internal/eval"
)

// calibrationTolerance is the relative deviation of a simulated p50 TTFT,
// p50 TPOT or weighted throughput from the measured one that the
// calibration check accepts.
const calibrationTolerance = 0.25

// LoadFit is the least-squares fit of the simulated engine's load factors
// to the measured latency growth. The engine scales prefill by
// 1 + (F-1)(c-1)/(slots-1) at c running requests with F =
// PrefillFactorUnderLoad, and decode the same way with F =
// TimeFactorUnderLoad, so each fit minimises the squared error of that
// factor against the measured median ratios (level c over level 1, c =
// 2..slots): TTFT (the prefill factor) uses the TTFT ratios, TPOT (the
// decode factor) the TPOT ratios, and Joint both, i.e. the best single
// factor (reported for reference). A fit is NaN when its ratios were not
// measured.
type LoadFit struct {
	TTFT, TPOT, Joint float64
	// TTFTGrowth and TPOTGrowth are the measured median TTFT and TPOT at
	// concurrency slots over those at concurrency 1 (NaN if unmeasured).
	TTFTGrowth, TPOTGrowth float64
}

// fitLoadFactor fits LoadFit to the levels of a replica with the given
// slots.
func fitLoadFactor(levels []Level, slots int) LoadFit {
	nan := math.NaN()
	fit := LoadFit{TTFT: nan, TPOT: nan, Joint: nan, TTFTGrowth: nan, TPOTGrowth: nan}
	one, ok := levelAt(levels, 1)
	if !ok || slots < 2 {
		return fit
	}
	// With x = (c-1)/(slots-1) and ratio r, the least-squares F-1 of
	// sum (x(F-1) - (r-1))^2 is sum x(r-1) / sum x^2.
	type sums struct{ xr, xx float64 }
	var ttft, tpot sums
	add := func(s *sums, x, v, base float64) {
		if v > 0 && base > 0 {
			s.xr += x * (v/base - 1)
			s.xx += x * x
		}
	}
	for c := 2; c <= slots; c++ {
		if lv, ok := levelAt(levels, c); ok {
			x := float64(c-1) / float64(slots-1)
			add(&ttft, x, lv.TTFTP50, one.TTFTP50)
			add(&tpot, x, lv.TPOTP50, one.TPOTP50)
		}
	}
	solve := func(s sums) float64 {
		if s.xx == 0 {
			return nan
		}
		return 1 + s.xr/s.xx
	}
	fit.TTFT, fit.TPOT, fit.Joint = solve(ttft), solve(tpot), solve(sums{ttft.xr + tpot.xr, ttft.xx + tpot.xx})
	if full, ok := levelAt(levels, slots); ok {
		fit.TTFTGrowth, fit.TPOTGrowth = ratio(full.TTFTP50, one.TTFTP50), ratio(full.TPOTP50, one.TPOTP50)
	}
	return fit
}

// ratio is a/b, NaN unless both are positive.
func ratio(a, b float64) float64 {
	if a > 0 && b > 0 {
		return a / b
	}
	return math.NaN()
}

// levelAt returns the level at concurrency c.
func levelAt(levels []Level, c int) (Level, bool) {
	for _, lv := range levels {
		if lv.Concurrency == c {
			return lv, true
		}
	}
	return Level{}, false
}

// CalibrationCheck compares a SIMULATED replica running the calibrated
// parameters, driven through the same concurrency levels, with the
// measured replica.
type CalibrationCheck struct {
	// LevelDuration is the simulated run's measured window per level and
	// Tolerance the accepted relative deviation.
	LevelDuration time.Duration
	Tolerance     float64
	// Levels are the simulated replica's levels.
	Levels []Level
	// Mismatches lists every level statistic (p50 TTFT, p50 TPOT, weighted
	// tokens/s, SLO verdict) on which the simulated replica differs from
	// the measured one by more than Tolerance; empty when it reproduces
	// the measurement.
	Mismatches []string
	// Skipped says why the check did not run ("" when it ran).
	Skipped string
}

// Ran reports whether the check ran.
func (c CalibrationCheck) Ran() bool { return c.Skipped == "" }

// checkCalibration runs the calibration check (unless the plan disables
// it) and stores it in r.Check. Only a cancelled ctx is an error: a check
// that cannot run is recorded as skipped.
func (r *ProfileResult) checkCalibration(ctx context.Context) error {
	chk := CalibrationCheck{LevelDuration: r.Plan.CheckLevelDuration, Tolerance: calibrationTolerance}
	defer func() { r.Check = chk }()
	if chk.LevelDuration < 0 {
		chk.Skipped = "disabled by the plan"
		return nil
	}
	if err := r.Validate(); err != nil {
		chk.Skipped = "the calibrated parameters are not a valid simulated engine: " + err.Error()
		return nil
	}
	plan := r.Plan
	plan.Config.Backend.Kind = config.BackendSimulated
	plan.Config.Engine = r.Calibrated
	plan.LevelDuration, plan.CheckLevelDuration = chk.LevelDuration, -1
	plan.Logger = r.Plan.Logger.With("profile", "calibration check (SIMULATED replica)")
	r.Plan.Logger.Info("checking the calibrated parameters on a SIMULATED replica", "levelDuration", chk.LevelDuration)
	sim, err := Profile(ctx, plan)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		chk.Skipped = "the simulated run failed: " + err.Error()
		r.Plan.Logger.Warn("calibration check failed", "err", err)
		return nil
	}
	chk.Levels = sim.Levels
	chk.Mismatches = compareLevels(r.Levels, sim.Levels, chk.Tolerance)
	return nil
}

// compareLevels lists the statistics on which sim deviates from measured
// by more than tol (relative), and differing SLO verdicts.
func compareLevels(measured, sim []Level, tol float64) []string {
	var out []string
	for _, m := range measured {
		s, ok := levelAt(sim, m.Concurrency)
		if !ok {
			out = append(out, fmt.Sprintf("c=%d: not simulated", m.Concurrency))
			continue
		}
		stats := []struct {
			name, unit    string
			scale, mv, sv float64
		}{
			{"p50 TTFT", " s", 1, m.TTFTP50, s.TTFTP50},
			{"p50 TPOT", " ms", 1000, m.TPOTP50, s.TPOTP50},
			{"weighted tok/s", "", 1, m.WeightedTokensPerSec, s.WeightedTokensPerSec},
		}
		for _, st := range stats {
			if math.IsNaN(st.mv) && math.IsNaN(st.sv) {
				continue
			}
			if dev := st.sv/st.mv - 1; !(math.Abs(dev) <= tol) {
				out = append(out, fmt.Sprintf("c=%d %s: simulated %.3f%s vs measured %.3f%s (%+.0f%%)",
					m.Concurrency, st.name, st.scale*st.sv, st.unit, st.scale*st.mv, st.unit, 100*dev))
			}
		}
		if m.MeetsSLO != s.MeetsSLO {
			out = append(out, fmt.Sprintf("c=%d SLO verdict: simulated %t vs measured %t", m.Concurrency, s.MeetsSLO, m.MeetsSLO))
		}
	}
	return out
}

// checkTable shows, per level, the measured and simulated values as
// "measured / simulated".
func (r ProfileResult) checkTable() eval.Table {
	t := eval.Table{Header: []string{"concurrency", "p50 TTFT s", "p99 TTFT s", "p50 TPOT ms", "p99 TPOT ms",
		"weighted tok/s", "meets SLO"}}
	pair := func(m, s float64) string { return eval.FormatValue(m) + " / " + eval.FormatValue(s) }
	for _, m := range r.Levels {
		s, ok := levelAt(r.Check.Levels, m.Concurrency)
		if !ok {
			continue
		}
		t.Rows = append(t.Rows, []string{strconv.Itoa(m.Concurrency),
			pair(m.TTFTP50, s.TTFTP50), pair(m.TTFTP99, s.TTFTP99),
			pair(1000*m.TPOTP50, 1000*s.TPOTP50), pair(1000*m.TPOTP99, 1000*s.TPOTP99),
			pair(m.WeightedTokensPerSec, s.WeightedTokensPerSec),
			strconv.FormatBool(m.MeetsSLO) + " / " + strconv.FormatBool(s.MeetsSLO)})
	}
	return t
}

// fitLine explains the load-factor fit with its measured inputs.
func (r ProfileResult) fitLine() string {
	f := r.LoadFit
	return fmt.Sprintf("Load factors: from concurrency 1 to %d the measured median TTFT grew %s x and the median TPOT %s x. "+
		"Fitted by least squares over concurrency 2..%d: F = %s for prefill (used: prefillFactorUnderLoad %g) and %s for decode "+
		"(used: timeFactorUnderLoad %g); one factor for both would be %s.",
		r.Slots, eval.FormatValue(f.TTFTGrowth), eval.FormatValue(f.TPOTGrowth), r.Slots,
		eval.FormatValue(f.TTFT), r.Calibrated.PrefillFactorUnderLoad, eval.FormatValue(f.TPOT),
		r.Calibrated.TimeFactorUnderLoad, eval.FormatValue(f.Joint))
}

// checkVerdict states the calibration check's outcome in one paragraph.
func (r ProfileResult) checkVerdict() string {
	c := r.Check
	if !c.Ran() {
		return "Calibration check not run: " + c.Skipped + ". The calibrated parameters are unverified."
	}
	tol := fmt.Sprintf("±%.0f%% on p50 TTFT, p50 TPOT and weighted tokens/s, and the same SLO verdict, at every level", 100*c.Tolerance)
	if len(c.Mismatches) == 0 {
		return fmt.Sprintf("Calibration check: the SIMULATED replica running these parameters reproduces the measurement (%s).", tol)
	}
	return fmt.Sprintf("Calibration check: the SIMULATED replica running these parameters does NOT reproduce the measurement "+
		"(%s); %d deviations: %s.", tol, len(c.Mismatches), strings.Join(c.Mismatches, "; "))
}
