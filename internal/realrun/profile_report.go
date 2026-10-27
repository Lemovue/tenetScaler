package realrun

import (
	"bytes"
	"cmp"
	"fmt"
	"io"
	"maps"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"tenantscaler/internal/eval"
)

// derive sets the capacity C and the calibrated simulated-engine
// parameters from the measurements:
//   - C = weighted tokens/s at the highest level meeting the SLO;
//   - PrefillPerToken = slope of median TTFT between the two prefill probe
//     lengths (per prompt word, the unit the simulated engine counts), and
//     PrefillOverhead = the short probe's TTFT minus its prompt's share;
//   - InterTokenLatency = median TPOT at concurrency 1;
//   - TimeFactorUnderLoad = the least-squares load factor of the median
//     TPOT growth and PrefillFactorUnderLoad = that of the median TTFT
//     growth (LoadFit.TPOT and LoadFit.TTFT, at least 1; the config's when
//     not measured);
//   - MaxNumSeqs = Slots; StartupDuration = the measured cold start;
//     JitterFraction stays the config's (not measured).
func (r *ProfileResult) derive() {
	for _, lv := range r.Levels {
		if lv.MeetsSLO && lv.Concurrency > r.CapacityConcurrency {
			r.CapacityConcurrency, r.CapacityTokensPerSec = lv.Concurrency, lv.WeightedTokensPerSec
		}
	}
	cal := r.Plan.Config.Engine
	cal.MaxNumSeqs = r.Slots
	cal.StartupDuration = r.ColdStart.String()
	if len(r.Prefill) == 2 {
		short, long := r.Prefill[0], r.Prefill[1]
		perWord := 0.0
		if dw := float64(long.Words - short.Words); dw > 0 {
			perWord = math.Max(0, (long.TTFTP50-short.TTFTP50)/dw)
		}
		overhead := math.Max(0, short.TTFTP50-perWord*float64(short.Words))
		cal.PrefillPerToken = seconds(perWord).String()
		cal.PrefillOverhead = seconds(overhead).String()
	}
	if one, ok := levelAt(r.Levels, 1); ok && one.TPOTP50 > 0 {
		cal.InterTokenLatency = seconds(one.TPOTP50).String()
	}
	r.LoadFit = fitLoadFactor(r.Levels, r.Slots)
	if !math.IsNaN(r.LoadFit.TPOT) {
		cal.TimeFactorUnderLoad = factor(r.LoadFit.TPOT)
	}
	if !math.IsNaN(r.LoadFit.TTFT) {
		cal.PrefillFactorUnderLoad = factor(r.LoadFit.TTFT)
	}
	r.Calibrated = cal
}

// factor rounds a fitted load factor to 0.01, at least 1.
func factor(f float64) float64 { return math.Round(math.Max(1, f)*100) / 100 }

// seconds converts float seconds to a duration rounded to a microsecond.
func seconds(s float64) time.Duration {
	return time.Duration(s * float64(time.Second)).Round(time.Microsecond)
}

// Label is the one-line description of what was measured.
func (r ProfileResult) Label() string {
	if r.Engine.Simulated {
		return "SIMULATED engine profile (modelled latencies, not real inference)"
	}
	return fmt.Sprintf("REAL inference profile: %s, replicas with %d threads (no GPU), %d slots; %s; CPU %s; %s",
		r.Engine.Label(), r.Threads, r.Slots, cmp.Or(r.Pinning, "CPU-pinned"), r.CPU, r.Date.Format("2006-01-02"))
}

// LevelsTable is the per-level measurement table.
func (r ProfileResult) LevelsTable() eval.Table {
	t := eval.Table{Header: []string{"concurrency", "latency sample", "completed in window", "failed",
		"p50 TTFT s", "p99 TTFT s", "p50 TPOT ms", "p99 TPOT ms", "output tok/s", "weighted tok/s",
		"prompt tokens/word", "meets SLO"}}
	for _, lv := range r.Levels {
		t.Rows = append(t.Rows, []string{
			strconv.Itoa(lv.Concurrency), strconv.Itoa(lv.Requests), strconv.Itoa(lv.Completed), failedCell(lv),
			eval.FormatValue(lv.TTFTP50), eval.FormatValue(lv.TTFTP99),
			eval.FormatValue(1000 * lv.TPOTP50), eval.FormatValue(1000 * lv.TPOTP99),
			eval.FormatValue(lv.OutputTokensPerSec), eval.FormatValue(lv.WeightedTokensPerSec),
			eval.FormatValue(lv.PromptTokensPerWord), strconv.FormatBool(lv.MeetsSLO),
		})
	}
	return t
}

// failedCell renders a level's failures with their causes, e.g.
// "1 (transport error: 1)".
func failedCell(lv Level) string {
	if lv.Failed == 0 {
		return "0"
	}
	causes := make([]string, 0, len(lv.FailedBy))
	for _, c := range slices.Sorted(maps.Keys(lv.FailedBy)) {
		causes = append(causes, fmt.Sprintf("%s: %d", c, lv.FailedBy[c]))
	}
	return fmt.Sprintf("%d (%s)", lv.Failed, strings.Join(causes, ", "))
}

// YAMLSnippet is the configuration that applies the measurement: the
// controller's capacity C and the calibrated simulated engine.
func (r ProfileResult) YAMLSnippet() string {
	var b strings.Builder
	if !math.IsNaN(r.CapacityTokensPerSec) {
		fmt.Fprintf(&b, "controller:\n  replicaCapacityTokensPerSec: %.0f\n", r.CapacityTokensPerSec)
	}
	e := r.Calibrated
	fmt.Fprintf(&b, "engine:\n  prefillOverhead: %s\n  prefillPerToken: %s\n  interTokenLatency: %s\n"+
		"  maxNumSeqs: %d\n  timeFactorUnderLoad: %g\n  prefillFactorUnderLoad: %g\n  startupDuration: %s\n  jitterFraction: %g\n",
		e.PrefillOverhead, e.PrefillPerToken, e.InterTokenLatency, e.MaxNumSeqs, e.TimeFactorUnderLoad,
		e.PrefillFactorUnderLoad, e.StartupDuration, e.JitterFraction)
	return b.String()
}

// capacityLine states C, or that no level met the SLO.
func (r ProfileResult) capacityLine() string {
	if math.IsNaN(r.CapacityTokensPerSec) {
		return fmt.Sprintf("C: no concurrency level met the SLO (p99 TTFT <= %gs and p99 TPOT <= %gms); the config keeps replicaCapacityTokensPerSec: %g.",
			r.SLO.TTFTSeconds, r.SLO.TPOTMillis, r.Configured)
	}
	return fmt.Sprintf("C = %.0f weighted tokens/s per replica (concurrency %d, the highest level meeting p99 TTFT <= %gs and p99 TPOT <= %gms); the config uses %g.",
		r.CapacityTokensPerSec, r.CapacityConcurrency, r.SLO.TTFTSeconds, r.SLO.TPOTMillis, r.Configured)
}

// WriteText prints the result for the profile command.
func (r ProfileResult) WriteText(w io.Writer) error {
	var b bytes.Buffer
	fmt.Fprintf(&b, "%s\n", r.Label())
	fmt.Fprintf(&b, "cold start (launch to ready): %s\n", r.ColdStart)
	for _, pt := range r.Prefill {
		fmt.Fprintf(&b, "prefill probe: %d words (%.1f server tokens), median TTFT %.3fs over %d requests\n",
			pt.Words, pt.PromptTokens, pt.TTFTP50, pt.Requests)
	}
	fmt.Fprintf(&b, "\nclosed-loop levels (%s measured after %s warm-up per level):\n", r.Plan.LevelDuration, r.Plan.Warmup)
	if err := r.LevelsTable().WriteMarkdown(&b); err != nil {
		return err
	}
	fmt.Fprintf(&b, "\n%s\n\n%s\n\nYAML (capacity, and the simulated engine calibrated from this measurement):\n%s",
		r.capacityLine(), r.fitLine(), r.YAMLSnippet())
	if err := r.writeCheck(&b); err != nil {
		return err
	}
	_, err := w.Write(b.Bytes())
	return err
}

// writeCheck writes the calibration check: its verdict and, when it ran,
// the measured / simulated table.
func (r ProfileResult) writeCheck(b *bytes.Buffer) error {
	fmt.Fprintf(b, "\n%s\n", r.checkVerdict())
	if !r.Check.Ran() {
		return nil
	}
	fmt.Fprintf(b, "\nmeasured / SIMULATED (calibrated engine, %s measured per level after the same warm-up):\n\n", r.Check.LevelDuration)
	return r.checkTable().WriteMarkdown(b)
}

// Markdown renders profile.md.
func (r ProfileResult) Markdown() ([]byte, error) {
	var b bytes.Buffer
	fmt.Fprintf(&b, "# Replica profile\n\n**%s**\n\n", r.Label())
	in, out := r.Plan.In, r.Plan.Out
	fmt.Fprintf(&b, "One replica, driven directly (no gateway) by closed-loop clients: at concurrency c, c clients each send "+
		"their next request as soon as the previous one finishes. Every prompt is unique (no prompt-cache hits). "+
		"Lengths: prompt words log-normal median %g (sigma %g, %d-%d), output tokens median %g (sigma %g, %d-%d), "+
		"with ignore_eos so every answer has exactly its max_tokens. Each level runs %s of warm-up and %s of measurement. "+
		"Latency percentiles use the requests that arrived and finished inside the measured window (the latency sample); "+
		"throughput is the work of every request that finished inside the window, whatever its arrival, divided by the "+
		"window (unbiased for a closed loop in steady state). Weighted tokens = prompt words + 2 x output tokens (the "+
		"gateway's and controller's accounting); prompt tokens/word is the server tokenizer's count over the prompt words.\n\n",
		in.Median, in.Sigma, in.Min, in.Max, out.Median, out.Sigma, out.Min, out.Max, r.Plan.Warmup, r.Plan.LevelDuration)
	fmt.Fprintf(&b, "- Cold start (launch to first healthy response): **%s**\n", r.ColdStart)
	for _, pt := range r.Prefill {
		fmt.Fprintf(&b, "- Prefill probe, concurrency 1: %d prompt words (%.1f server tokens): median TTFT %.3f s over %d requests\n",
			pt.Words, pt.PromptTokens, pt.TTFTP50, pt.Requests)
	}
	b.WriteString("\n## Concurrency levels\n\n")
	if err := r.LevelsTable().WriteMarkdown(&b); err != nil {
		return nil, err
	}
	fmt.Fprintf(&b, "\n## Derived\n\n%s\n\n", r.capacityLine())
	b.WriteString("Simulated-engine parameters calibrated from this replica: prefill = overhead + per-word slope fitted to the two " +
		"prefill probes; interTokenLatency = median TPOT at concurrency 1; timeFactorUnderLoad = least-squares load factor " +
		"(below); maxNumSeqs = slots; startupDuration = the measured cold start; jitterFraction is kept from the config " +
		"(not measured).\n\n" + r.fitLine() + "\n\n```yaml\n")
	b.WriteString(r.YAMLSnippet())
	b.WriteString("```\n\n## Calibration check\n")
	if err := r.writeCheck(&b); err != nil {
		return nil, err
	}
	b.WriteString("\nNot measured here: more than one replica at a time (replicas are pinned to separate cores, " +
		"but share memory bandwidth and caches), the gateway's overhead, and GPU inference.\n")
	return b.Bytes(), nil
}

// WriteFiles writes profile.md to dir (created if needed) and returns its
// path.
func (r ProfileResult) WriteFiles(dir string) (string, error) {
	md, err := r.Markdown()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("create %s: %w", dir, err)
	}
	path := filepath.Join(dir, "profile.md")
	if err := os.WriteFile(path, md, 0o644); err != nil {
		return "", fmt.Errorf("write %s: %w", path, err)
	}
	return path, nil
}

// Validate checks that the calibrated engine parameters form a valid
// simulated engine.
func (r ProfileResult) Validate() error {
	p, err := r.Calibrated.Params()
	if err != nil {
		return err
	}
	return p.Validate()
}
