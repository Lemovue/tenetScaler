package realrun

import (
	"fmt"
	"math"
	"math/rand/v2"
	"slices"

	"tenantscaler/internal/pool"
)

// runOrder is the order of the setups in run index k: a seeded shuffle,
// different for every run index and reproducible from the base seed
// (decision Q14), so slow drifts of the machine do not always hit the same
// setup first.
func runOrder(setups []string, baseSeed int64, k int) []string {
	out := slices.Clone(setups)
	rng := rand.New(rand.NewPCG(uint64(baseSeed), uint64(k)+0x9e3779b97f4a7c15))
	rng.Shuffle(len(out), func(i, j int) { out[i], out[j] = out[j], out[i] })
	return out
}

// readable returns &v, or nil when v is NaN (not readable).
func readable(v float64) *float64 {
	if math.IsNaN(v) {
		return nil
	}
	return &v
}

// holdLabel describes the readiness hold for result labels (decision Q12).
func holdLabel(seconds float64) string {
	if seconds <= 0 {
		return "no readiness hold (T = 0: replicas count as ready as soon as llama-server is)"
	}
	return fmt.Sprintf("readiness hold T = %g s: %s", seconds, pool.HoldLabel)
}
