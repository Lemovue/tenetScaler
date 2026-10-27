package engine

import (
	"math"
	"math/rand"
	"strings"
	"time"
)

// LoadFactor is the decode-time multiplier when running requests execute
// at once: 1 + (F-1)*(running-1)/(MaxNumSeqs-1), clamped to [1, F] where F
// is TimeFactorUnderLoad. It is 1 when MaxNumSeqs is 1, because a replica
// that runs one request at a time is never "under load" in this model.
func (p Params) LoadFactor(running int) float64 {
	return p.loadFactor(p.TimeFactorUnderLoad, running)
}

// PrefillLoadFactor is LoadFactor with F = PrefillFactor().
func (p Params) PrefillLoadFactor(running int) float64 {
	return p.loadFactor(p.PrefillFactor(), running)
}

func (p Params) loadFactor(full float64, running int) float64 {
	if p.MaxNumSeqs <= 1 {
		return 1
	}
	hi := math.Max(1, full)
	f := 1 + (full-1)*float64(running-1)/float64(p.MaxNumSeqs-1)
	return math.Min(math.Max(f, 1), hi)
}

// PrefillTime is the simulated time to first token for a request with
// inputTokens prompt tokens while running requests (including this one)
// execute: (PrefillOverhead + PrefillPerToken*inputTokens) *
// PrefillLoadFactor. Negative token counts are treated as zero.
func (p Params) PrefillTime(inputTokens, running int) time.Duration {
	if inputTokens < 0 {
		inputTokens = 0
	}
	base := p.PrefillOverhead + p.PrefillPerToken*time.Duration(inputTokens)
	return scale(base, p.PrefillLoadFactor(running))
}

// TokenInterval is the simulated decode time of one output token while
// running requests execute: InterTokenLatency * LoadFactor.
func (p Params) TokenInterval(running int) time.Duration {
	return scale(p.InterTokenLatency, p.LoadFactor(running))
}

// Jitter returns d*(1+frac*N(0,1)) clamped to [0.5d, 1.5d]. It returns d
// unchanged when d <= 0, frac <= 0 or rng is nil, so callers can disable
// jitter without branching.
func Jitter(d time.Duration, frac float64, rng *rand.Rand) time.Duration {
	if d <= 0 || frac <= 0 || rng == nil {
		return d
	}
	f := math.Min(math.Max(1+frac*rng.NormFloat64(), 0.5), 1.5)
	return scale(d, f)
}

// CountTokens approximates a prompt's token count as its number of
// whitespace-separated words, with a minimum of 1 (an empty prompt still
// costs one prefill token).
func CountTokens(prompt string) int {
	return max(1, len(strings.Fields(prompt)))
}

// scale multiplies a duration by a factor, rounding to the nearest
// nanosecond.
func scale(d time.Duration, f float64) time.Duration {
	return time.Duration(math.Round(float64(d) * f))
}
