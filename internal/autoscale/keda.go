package autoscale

import (
	"fmt"
	"math"
	"time"
)

// KEDAConfig configures a KEDA ScaledObject with a queue-length trigger.
// KEDA exposes the trigger as an external metric with an AverageValue
// target, which an HPA then scales on.
type KEDAConfig struct {
	// TargetPerReplica is the queue length per replica KEDA aims for (the
	// AverageValue target).
	TargetPerReplica float64
	// Activation is KEDA's activation threshold: the scaler is active only
	// when the total queue length is above it.
	Activation float64
	// Tolerance is the HPA tolerance on the usage ratio
	// total / (current * TargetPerReplica).
	Tolerance float64
	// ScaleDownWindow is the scale-down stabilisation window (it also
	// plays the role of KEDA's cooldown before deactivating).
	ScaleDownWindow time.Duration
	// Min and Max bound the replica count (Min may be 0: scale to zero).
	Min, Max int
}

// DefaultKEDAConfig returns the baseline queue-length scaler:
// TargetPerReplica 4, Activation 0, Tolerance 0.1, 300 s scale-down window,
// between minReplicas and maxReplicas (maxReplicas is the pool capacity).
func DefaultKEDAConfig(minReplicas, maxReplicas int) KEDAConfig {
	return KEDAConfig{
		TargetPerReplica: DefaultKEDATargetPerReplica,
		Activation:       DefaultKEDAActivation,
		Tolerance:        DefaultTolerance,
		ScaleDownWindow:  DefaultScaleDownWindow,
		Min:              minReplicas,
		Max:              maxReplicas,
	}
}

// Validate checks the configuration.
func (c KEDAConfig) Validate() error {
	if !finite(c.TargetPerReplica) || c.TargetPerReplica <= 0 {
		return fmt.Errorf("keda: targetPerReplica must be a finite number > 0, got %v", c.TargetPerReplica)
	}
	if !finite(c.Activation) {
		return fmt.Errorf("keda: activation must be finite, got %v", c.Activation)
	}
	if err := validateBehavior(c.Tolerance, c.ScaleDownWindow, c.Min, c.Max); err != nil {
		return fmt.Errorf("keda: %w", err)
	}
	return nil
}

// KEDA is a KEDA queue-length autoscaler for one workload. It is not safe
// for concurrent use.
type KEDA struct {
	cfg KEDAConfig
	b   behavior
}

// NewKEDA returns a KEDA scaler for one workload. initial and the first
// Step are treated as in NewHPA: initial (clamped to [Min, Max]) is only
// what Desired reports before the first Step, and the first Step records
// the current count it is given, so an existing workload holds its size
// for one ScaleDownWindow. Call cfg.Validate first: with an invalid
// TargetPerReplica the scaler never changes the replica count.
func NewKEDA(cfg KEDAConfig, initial int) *KEDA {
	return &KEDA{cfg: cfg, b: newBehavior(cfg.ScaleDownWindow, cfg.Min, cfg.Max, initial)}
}

// Step runs one sync at time now for a workload with current replicas and
// total queued requests, and returns the desired replica count. When total
// <= Activation the proposal is Min (deactivation, still subject to the
// scale-down window). Otherwise it is ceil(total/TargetPerReplica), or
// current when current > 0 and |total/(current*TargetPerReplica) - 1| <=
// Tolerance. Stabilisation, scale-up limit and clamp follow as for the HPA.
// A negative total is treated as 0; a non-finite total changes nothing.
func (k *KEDA) Step(now time.Time, current int, total float64) int {
	current = max(current, 0)
	if !finite(total) || !(k.cfg.TargetPerReplica > 0) {
		return k.b.hold(now, current)
	}
	total = math.Max(total, 0)
	var proposal int
	switch {
	case total <= k.cfg.Activation:
		proposal = k.cfg.Min
	case current > 0 && math.Abs(total/(float64(current)*k.cfg.TargetPerReplica)-1) <= k.cfg.Tolerance:
		proposal = current
	default:
		proposal = ceilReplicas(total / k.cfg.TargetPerReplica)
	}
	return k.b.next(now, current, proposal)
}

// Desired returns the replica count of the last Step, or initial clamped
// to [Min, Max] before the first Step.
func (k *KEDA) Desired() int { return k.b.desired }
