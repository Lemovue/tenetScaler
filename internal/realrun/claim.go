package realrun

import (
	"encoding/json"
	"fmt"
	"math"
	"slices"
	"strings"

	"tenantscaler/internal/eval"
	"tenantscaler/internal/sim"
)

// Claim is the pre-registered comparison of README.md (decision Q15):
// victim SLO attainment during the spike, S7 minus S4, paired by run index
// (both setups replay the same trace in run k).
type Claim struct {
	// Pairs are the run indexes with both setups' victim attainment.
	Pairs []ClaimPair `json:"pairs"`
	// Diff is the mean paired difference S7 - S4 (percentage points) with
	// its Student-t 95% CI.
	Diff eval.Stat `json:"diff"`
	// Verdict is "supported" (the CI is wholly above 0), "contradicted"
	// (wholly below 0) or "not shown".
	Verdict string `json:"verdict"`
}

// ClaimPair holds one run index's numbers for S7 and S4.
type ClaimPair struct {
	Run                   int     `json:"run"`
	S7Attainment          float64 `json:"s7AttainmentPct"`
	S4Attainment          float64 `json:"s4AttainmentPct"`
	S7ShedAwareAttainment float64 `json:"s7ShedAwareAttainmentPct"`
	S4ShedAware           float64 `json:"s4ShedAwareAttainmentPct"`
	S7NoisyShedPct        float64 `json:"s7NoisyShedPct"`
	S4NoisyShedPct        float64 `json:"s4NoisyShedPct"`
	S7ReplicaMinutes      float64 `json:"s7ReplicaMinutes"`
	S4ReplicaMinutes      float64 `json:"s4ReplicaMinutes"`
	Diff                  float64 `json:"diffPoints"`
}

// buildClaim pairs S7 and S4 runs by run index; nil when either setup is
// missing.
func buildClaim(results []RunResult) *Claim {
	by := map[string]map[int]RunResult{}
	for _, r := range results {
		if by[r.Setup] == nil {
			by[r.Setup] = map[int]RunResult{}
		}
		by[r.Setup][r.Run] = r
	}
	s7, s4 := by[sim.SetupFull], by[sim.SetupFlowControl]
	if len(s7) == 0 || len(s4) == 0 {
		return nil
	}
	c := &Claim{}
	var diffs []float64
	for _, k := range sortedKeys(s7) {
		b, ok := s4[k]
		if !ok {
			continue
		}
		a := s7[k]
		p := ClaimPair{Run: k,
			S7Attainment: 100 - a.Metrics.VictimViolationPct, S4Attainment: 100 - b.Metrics.VictimViolationPct,
			S7ShedAwareAttainment: 100 - a.Metrics.VictimShedAwareViolationPct, S4ShedAware: 100 - b.Metrics.VictimShedAwareViolationPct,
			S7NoisyShedPct: a.Metrics.NoisyShedPct, S4NoisyShedPct: b.Metrics.NoisyShedPct,
			S7ReplicaMinutes: a.Metrics.ReplicaMinutes, S4ReplicaMinutes: b.Metrics.ReplicaMinutes,
		}
		p.Diff = p.S7Attainment - p.S4Attainment
		c.Pairs = append(c.Pairs, p)
		diffs = append(diffs, p.Diff)
	}
	c.Diff = eval.Summarize(diffs)
	switch lo, hi := c.Diff.Mean-c.Diff.CI95, c.Diff.Mean+c.Diff.CI95; {
	case math.IsNaN(c.Diff.CI95):
		c.Verdict = "not shown"
	case lo > 0:
		c.Verdict = "supported"
	case hi < 0:
		c.Verdict = "contradicted"
	default:
		c.Verdict = "not shown"
	}
	return c
}

func sortedKeys(m map[int]RunResult) []int {
	keys := make([]int, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

// claimMarkdown renders the comparison with every pair.
func claimMarkdown(c *Claim, holdSeconds float64) string {
	var b strings.Builder
	fmt.Fprintf(&b, "## Pre-registered claim: S7 vs S4 (README.md)\n\n"+
		"Victim (A, B) SLO attainment during C's spike = 100 - victim_violation_pct, paired by run index (same trace); "+
		"%s. Decision rule: supported if the 95%% CI of the mean paired difference S7 - S4 is wholly above 0.\n\n",
		holdLabel(holdSeconds))
	t := eval.Table{Header: []string{"run", "S7 attainment %", "S4 attainment %", "S7 - S4", "S7 shed-aware %", "S4 shed-aware %",
		"C shed % S7", "C shed % S4", "replica-min S7", "replica-min S4"}}
	for _, p := range c.Pairs {
		t.Rows = append(t.Rows, []string{fmt.Sprint(p.Run), eval.FormatValue(p.S7Attainment), eval.FormatValue(p.S4Attainment),
			eval.FormatValue(p.Diff), eval.FormatValue(p.S7ShedAwareAttainment), eval.FormatValue(p.S4ShedAware),
			eval.FormatValue(p.S7NoisyShedPct), eval.FormatValue(p.S4NoisyShedPct),
			eval.FormatValue(p.S7ReplicaMinutes), eval.FormatValue(p.S4ReplicaMinutes)})
	}
	_ = t.WriteMarkdown(&b)
	ci := "n/a (one pair)"
	if !math.IsNaN(c.Diff.CI95) {
		ci = fmt.Sprintf("[%s, %s]", eval.FormatValue(c.Diff.Mean-c.Diff.CI95), eval.FormatValue(c.Diff.Mean+c.Diff.CI95))
	}
	fmt.Fprintf(&b, "\nMean paired difference S7 - S4: **%s percentage points**, 95%% CI %s, N = %d pairs. **Verdict: %s.**\n\n",
		eval.FormatValue(c.Diff.Mean), ci, c.Diff.N, c.Verdict)
	return b.String()
}

// MarshalJSON writes values that are undefined for a run (NaN) as null.
func (p ClaimPair) MarshalJSON() ([]byte, error) {
	f := func(v float64) *float64 {
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return nil
		}
		return &v
	}
	return json.Marshal(struct {
		Run                   int      `json:"run"`
		S7Attainment          *float64 `json:"s7AttainmentPct"`
		S4Attainment          *float64 `json:"s4AttainmentPct"`
		S7ShedAwareAttainment *float64 `json:"s7ShedAwareAttainmentPct"`
		S4ShedAware           *float64 `json:"s4ShedAwareAttainmentPct"`
		S7NoisyShedPct        *float64 `json:"s7NoisyShedPct"`
		S4NoisyShedPct        *float64 `json:"s4NoisyShedPct"`
		S7ReplicaMinutes      *float64 `json:"s7ReplicaMinutes"`
		S4ReplicaMinutes      *float64 `json:"s4ReplicaMinutes"`
		Diff                  *float64 `json:"diffPoints"`
	}{p.Run, f(p.S7Attainment), f(p.S4Attainment), f(p.S7ShedAwareAttainment), f(p.S4ShedAware),
		f(p.S7NoisyShedPct), f(p.S4NoisyShedPct), f(p.S7ReplicaMinutes), f(p.S4ReplicaMinutes), f(p.Diff)})
}
