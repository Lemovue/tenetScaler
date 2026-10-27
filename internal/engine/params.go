// Package engine is the SIMULATED LLM engine latency model. Its parameters
// mirror llm-d-inference-sim (time-to-first-token, inter-token latency,
// prefill time per token, max-num-seqs, time-factor-under-load,
// startup-duration). Values are illustrative, not measured on a GPU.
package engine

import (
	"fmt"
	"time"
)

// Params configures one simulated replica.
type Params struct {
	// PrefillOverhead is the fixed part of time-to-first-token.
	PrefillOverhead time.Duration `json:"prefillOverhead"`
	// PrefillPerToken is added per input token.
	PrefillPerToken time.Duration `json:"prefillPerToken"`
	// InterTokenLatency is the decode time per output token at load 1.
	InterTokenLatency time.Duration `json:"interTokenLatency"`
	// MaxNumSeqs is the number of requests a replica runs concurrently;
	// further requests wait in the replica's own FCFS queue.
	MaxNumSeqs int `json:"maxNumSeqs"`
	// TimeFactorUnderLoad (>= 1) multiplies the decode time per token when
	// MaxNumSeqs requests run at once; it scales linearly down to 1 at one
	// request.
	TimeFactorUnderLoad float64 `json:"timeFactorUnderLoad"`
	// PrefillFactorUnderLoad (>= 1) does the same for prefill. Zero means
	// TimeFactorUnderLoad, one factor for both. They differ on llama.cpp,
	// which schedules prompt processing ahead of decoding: from 1 to 4
	// running requests, TTFT grew 1.17x and TPOT 4.3x (results/real/profile.md).
	PrefillFactorUnderLoad float64 `json:"prefillFactorUnderLoad,omitempty"`
	// StartupDuration is the simulated model-load time (cold start).
	StartupDuration time.Duration `json:"startupDuration"`
	// JitterFraction is the relative standard deviation applied to latencies.
	JitterFraction float64 `json:"jitterFraction"`
}

// DefaultParams is an illustrative GPU-like profile for a small model.
func DefaultParams() Params {
	return Params{
		PrefillOverhead:     40 * time.Millisecond,
		PrefillPerToken:     200 * time.Microsecond,
		InterTokenLatency:   25 * time.Millisecond,
		MaxNumSeqs:          8,
		TimeFactorUnderLoad: 2.0,
		StartupDuration:     30 * time.Second,
		JitterFraction:      0.05,
	}
}

// Validate checks parameter ranges.
func (p Params) Validate() error {
	switch {
	case p.PrefillOverhead < 0 || p.PrefillPerToken < 0 || p.StartupDuration < 0:
		return fmt.Errorf("engine durations must be >= 0")
	case p.InterTokenLatency <= 0:
		return fmt.Errorf("engine interTokenLatency must be > 0")
	case p.MaxNumSeqs < 1:
		return fmt.Errorf("engine maxNumSeqs must be >= 1, got %d", p.MaxNumSeqs)
	case p.TimeFactorUnderLoad < 1:
		return fmt.Errorf("engine timeFactorUnderLoad must be >= 1, got %v", p.TimeFactorUnderLoad)
	case p.PrefillFactorUnderLoad != 0 && p.PrefillFactorUnderLoad < 1:
		return fmt.Errorf("engine prefillFactorUnderLoad must be 0 (= timeFactorUnderLoad) or >= 1, got %v", p.PrefillFactorUnderLoad)
	case p.JitterFraction < 0 || p.JitterFraction > 0.3:
		return fmt.Errorf("engine jitterFraction must be in [0, 0.3], got %v", p.JitterFraction)
	}
	return nil
}

// PrefillFactor is the effective prefill factor at full load:
// PrefillFactorUnderLoad, or TimeFactorUnderLoad when that is zero.
func (p Params) PrefillFactor() float64 {
	if p.PrefillFactorUnderLoad == 0 {
		return p.TimeFactorUnderLoad
	}
	return p.PrefillFactorUnderLoad
}
