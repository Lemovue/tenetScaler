package autoscale

import (
	"fmt"
	"math"
	"time"
)

// HPAConfig configures a Kubernetes-style HorizontalPodAutoscaler on a
// per-pod average metric (AverageValue / Utilization target).
type HPAConfig struct {
	// Target is the per-pod average the HPA aims for (utilisation in
	// [0, 1] for the CPU-style HPA).
	Target float64
	// Tolerance is the relative band around Target in which nothing
	// changes: |value/Target - 1| <= Tolerance.
	Tolerance float64
	// ScaleDownWindow is the scale-down stabilisation window.
	ScaleDownWindow time.Duration
	// Min and Max bound the replica count.
	Min, Max int
}

// DefaultHPAConfig returns the baseline CPU-style HPA: Target 0.7,
// Tolerance 0.1, 300 s scale-down window, between minReplicas and maxReplicas
// (maxReplicas is the pool capacity: pods beyond it would sit Pending).
func DefaultHPAConfig(minReplicas, maxReplicas int) HPAConfig {
	return HPAConfig{
		Target:          DefaultHPATarget,
		Tolerance:       DefaultTolerance,
		ScaleDownWindow: DefaultScaleDownWindow,
		Min:             minReplicas,
		Max:             maxReplicas,
	}
}

// Validate checks the configuration.
func (c HPAConfig) Validate() error {
	if !finite(c.Target) || c.Target <= 0 {
		return fmt.Errorf("hpa: target must be a finite number > 0, got %v", c.Target)
	}
	if err := validateBehavior(c.Tolerance, c.ScaleDownWindow, c.Min, c.Max); err != nil {
		return fmt.Errorf("hpa: %w", err)
	}
	return nil
}

// HPA is a HorizontalPodAutoscaler for one workload. It is not safe for
// concurrent use.
type HPA struct {
	cfg HPAConfig
	b   behavior
}

// NewHPA returns an HPA for one workload. initial, clamped to [Min, Max],
// is only what Desired reports before the first Step. As in Kubernetes, the
// first Step records the current replica count it is given as a proposal,
// so an HPA created over a running workload does not scale it down during
// its first ScaleDownWindow, whatever initial is. Call cfg.Validate first:
// with an invalid Target the HPA never changes the replica count.
func NewHPA(cfg HPAConfig, initial int) *HPA {
	return &HPA{cfg: cfg, b: newBehavior(cfg.ScaleDownWindow, cfg.Min, cfg.Max, initial)}
}

// Step runs one HPA sync at time now for a workload with current replicas
// whose per-pod average metric is value, and returns the desired replica
// count. The proposal is ceil(current*value/Target), or current when
// |value/Target - 1| <= Tolerance; see the package documentation for the
// stabilisation, scale-up limit and clamp that follow. A negative value is
// treated as 0; a non-finite value changes nothing.
func (h *HPA) Step(now time.Time, current int, value float64) int {
	current = max(current, 0)
	if !finite(value) || !(h.cfg.Target > 0) {
		return h.b.hold(now, current)
	}
	ratio := math.Max(value, 0) / h.cfg.Target
	proposal := current
	if math.Abs(ratio-1) > h.cfg.Tolerance {
		proposal = ceilReplicas(float64(current) * ratio)
	}
	return h.b.next(now, current, proposal)
}

// Desired returns the replica count of the last Step, or initial clamped
// to [Min, Max] before the first Step.
func (h *HPA) Desired() int { return h.b.desired }
