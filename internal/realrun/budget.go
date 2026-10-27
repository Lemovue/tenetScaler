package realrun

import (
	"fmt"
	"math"
	"strings"

	"tenantscaler/internal/eval"
)

// Pre-registered rules (docs/DESIGN.md §20) that turn the replica scaling
// curve into the real configuration's SLO and capacity. They are fixed
// before the measurement they are applied to.
const (
	// TTFTSLOSeconds is the time-to-first-token bound (BRIEF-v2 N2).
	TTFTSLOSeconds = 2.0
	// TPOTSLOMargin multiplies the full budget's pooled p99 TPOT.
	TPOTSLOMargin = 1.25
	// TPOTSLOStepMillis is the rounding step (up) of the TPOT bound.
	TPOTSLOStepMillis = 10.0
)

// BudgetDerivation applies the rules to the scaling level with every
// replica of the budget loaded at the dispatch window: the best service the
// budget can give, with no queueing in front of it.
//
//   - TPOT SLO = TPOTSLOMargin x pooled p99 TPOT, rounded up to
//     TPOTSLOStepMillis, so a healthy full pool meets it and a violation
//     measures contention, not the engine's own speed;
//   - C (replicaCapacityTokensPerSec) = pooled weighted tokens/s divided by
//     the replicas: the capacity of one replica while the whole budget is
//     busy, interference included;
//   - TTFT SLO = TTFTSLOSeconds, which the full pool's pooled p99 TTFT must
//     meet (MeetsTTFT), or the budget is too large for the SLO.
type BudgetDerivation struct {
	Replicas                     int
	PooledTPOTP99, PooledTTFTP99 float64 // seconds
	TPOTSLOMillis                float64
	CapacityPerReplica           float64
	MeetsTTFT                    bool
}

// DeriveBudget applies the rules to the level with capacity replicas.
func DeriveBudget(levels []ScalingLevel, capacity int) (BudgetDerivation, error) {
	for _, lv := range levels {
		if lv.Replicas != capacity {
			continue
		}
		p := lv.Pooled
		if math.IsNaN(p.TPOTP99) || math.IsNaN(p.TTFTP99) || !(p.WeightedTokensPerSec > 0) {
			return BudgetDerivation{}, fmt.Errorf("budget: the %d-replica level has no usable measurement", capacity)
		}
		return BudgetDerivation{
			Replicas: capacity, PooledTPOTP99: p.TPOTP99, PooledTTFTP99: p.TTFTP99,
			TPOTSLOMillis:      math.Ceil(TPOTSLOMargin*1000*p.TPOTP99/TPOTSLOStepMillis) * TPOTSLOStepMillis,
			CapacityPerReplica: math.Round(p.WeightedTokensPerSec / float64(capacity)),
			MeetsTTFT:          p.TTFTP99 <= TTFTSLOSeconds,
		}, nil
	}
	return BudgetDerivation{}, fmt.Errorf("budget: no scaling level with %d replicas", capacity)
}

// Explain states the derivation with its inputs.
func (b BudgetDerivation) Explain() string {
	var s strings.Builder
	fmt.Fprintf(&s, "With all %d replicas loaded at the dispatch window: pooled p99 TPOT %s ms, pooled p99 TTFT %s s.\n",
		b.Replicas, eval.FormatValue(1000*b.PooledTPOTP99), eval.FormatValue(b.PooledTTFTP99))
	fmt.Fprintf(&s, "TPOT SLO = %g x %s ms, rounded up to %g ms = %g ms.\n",
		TPOTSLOMargin, eval.FormatValue(1000*b.PooledTPOTP99), TPOTSLOStepMillis, b.TPOTSLOMillis)
	fmt.Fprintf(&s, "C = pooled weighted tokens/s / %d = %g weighted tokens/s per replica.\n", b.Replicas, b.CapacityPerReplica)
	verdict := "meets"
	if !b.MeetsTTFT {
		verdict = "does NOT meet"
	}
	fmt.Fprintf(&s, "TTFT SLO = %g s: the full budget %s it (pooled p99 TTFT %s s).\n", TTFTSLOSeconds, verdict, eval.FormatValue(b.PooledTTFTP99))
	return s.String()
}
