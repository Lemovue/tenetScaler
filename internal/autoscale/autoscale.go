// Package autoscale implements the per-workload baseline autoscalers that
// TenantScaler is compared against: a Kubernetes HorizontalPodAutoscaler
// with the autoscaling/v2 default behaviour (HPA) and a KEDA queue-length
// scaler, which KEDA realises as an HPA on an external AverageValue metric
// (KEDA). Both are clock-agnostic: Step takes the current time, so the
// discrete-event simulator can drive them in virtual time.
//
// One Step is one HPA sync. It follows the Kubernetes controller:
//
//  1. Proposal: the metric-based replica count; the current count when the
//     usage ratio is within Tolerance of 1.
//  2. Scale-down stabilisation: every proposal is recorded with its time; a
//     scale-down goes only to the highest proposal recorded within
//     ScaleDownWindow (so it happens after the window, never early), and
//     never above the current count. Scale-up has no window (the default).
//     The first Step also records the current count it is given, as the
//     Kubernetes controller does when it first reconciles an HPA
//     (recordInitialRecommendation), so an autoscaler created over a
//     running workload holds that size for one window.
//  3. Scale-up limit: at most max(4, current) replicas are added per Step
//     (the default policies "4 pods" and "100%", whichever allows more).
//     Kubernetes applies these per 15 s period; here they apply per Step.
//  4. Clamp to [Min, Max].
//
// A non-finite metric (metrics unavailable) changes nothing: Step returns
// the current count clamped to [Min, Max] and records no proposal, as the
// Kubernetes controller skips a sync whose metrics cannot be read.
package autoscale

import (
	"fmt"
	"math"
	"time"
)

// Defaults for the baseline setups (documented with the results).
const (
	// DefaultHPATarget is the per-pod utilisation the CPU-style HPA aims for.
	DefaultHPATarget = 0.7
	// DefaultTolerance is the Kubernetes default HPA tolerance.
	DefaultTolerance = 0.1
	// DefaultScaleDownWindow is the Kubernetes default scale-down
	// stabilisation window (and KEDA's default cooldown).
	DefaultScaleDownWindow = 300 * time.Second
	// DefaultKEDATargetPerReplica is the queued requests per replica the
	// KEDA scaler aims for.
	DefaultKEDATargetPerReplica = 4.0
	// DefaultKEDAActivation is the queue length the KEDA scaler must exceed
	// to be active.
	DefaultKEDAActivation = 0.0
)

// scaleUpPods is the "Pods" scale-up policy of the default HPA behaviour.
const scaleUpPods = 4

// recommendation is one recorded proposal.
type recommendation struct {
	at       time.Time
	replicas int
}

// behavior is the part of an HPA sync shared by both scalers: scale-down
// stabilisation, scale-up limit and clamping.
type behavior struct {
	window                   time.Duration
	minReplicas, maxReplicas int
	history                  []recommendation
	desired                  int
	started                  bool
}

// newBehavior returns a behavior whose desired count before the first Step
// is initial clamped to [minReplicas, maxReplicas]. initial never enters the
// scale-down history; start seeds it.
func newBehavior(window time.Duration, minReplicas, maxReplicas, initial int) behavior {
	b := behavior{window: window, minReplicas: minReplicas, maxReplicas: maxReplicas}
	b.desired = b.clamp(initial)
	return b
}

// start seeds the scale-down history at the first Step with the current
// replica count, stamped now, exactly as Kubernetes records it on the first
// reconcile of an HPA.
func (b *behavior) start(now time.Time, current int) {
	if !b.started {
		b.started = true
		b.history = append(b.history, recommendation{at: now, replicas: current})
	}
}

// next records proposal and returns the stabilised, rate-limited and
// clamped replica count.
func (b *behavior) next(now time.Time, current, proposal int) int {
	b.start(now, current)
	cutoff := now.Add(-b.window)
	highest := proposal
	kept := b.history[:0]
	for _, r := range b.history {
		if r.at.After(cutoff) {
			kept = append(kept, r)
			highest = max(highest, r.replicas)
		}
	}
	b.history = append(kept, recommendation{at: now, replicas: proposal})

	out := current
	switch {
	case proposal > current:
		out = min(proposal, current+max(scaleUpPods, current))
	case proposal < current:
		out = min(current, highest)
	}
	b.desired = b.clamp(out)
	return b.desired
}

// hold handles a sync without a usable metric.
func (b *behavior) hold(now time.Time, current int) int {
	b.start(now, current)
	b.desired = b.clamp(current)
	return b.desired
}

func (b *behavior) clamp(n int) int { return max(b.minReplicas, min(n, b.maxReplicas)) }

// validateBehavior checks the fields both configurations share.
func validateBehavior(tolerance float64, window time.Duration, minReplicas, maxReplicas int) error {
	switch {
	case !finite(tolerance) || tolerance < 0:
		return fmt.Errorf("tolerance must be a finite number >= 0, got %v", tolerance)
	case window < 0:
		return fmt.Errorf("scale-down window must be >= 0, got %v", window)
	case minReplicas < 0:
		return fmt.Errorf("min replicas must be >= 0, got %d", minReplicas)
	case maxReplicas < 1 || maxReplicas < minReplicas:
		return fmt.Errorf("max replicas must be >= 1 and >= min (%d), got %d", minReplicas, maxReplicas)
	}
	return nil
}

// ceilReplicas rounds a replica count up. It ignores float noise (so that
// 6.000000000000001 is 6, not 7) and caps absurd values so the conversion
// to int is well defined; callers clamp to Max afterwards.
func ceilReplicas(x float64) int {
	const noise, ceiling = 1e-9, 1 << 30
	return int(math.Min(math.Ceil(x-noise), ceiling))
}

func finite(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) }
