package autoscale

import (
	"fmt"
	"math"
	"testing"
	"time"
)

func hpaConfig(window time.Duration) HPAConfig {
	return HPAConfig{Target: 0.7, Tolerance: 0.1, ScaleDownWindow: window, Min: 1, Max: 20}
}

// With no window every proposal takes effect at once, which isolates the
// proposal math, the tolerance and the clamps.
func TestHPAProposalToleranceAndClamps(t *testing.T) {
	cases := []struct {
		name    string
		current int
		value   float64
		want    int
	}{
		{"on target", 4, 0.7, 4},
		{"within tolerance above", 4, 0.75, 4},
		{"within tolerance below", 4, 0.65, 4},
		{"scale up rounds up", 4, 0.84, 5},
		{"scale down", 10, 0.35, 5},
		{"scale down rounds up", 10, 0.6, 9},
		{"float noise does not add a replica", 10, 0.56, 8},
		{"idle clamps to Min", 3, 0, 1},
		{"negative value is 0", 3, -1, 1},
		{"above Max is clamped", 25, 0.7, 20},
		{"missing metric holds", 4, math.NaN(), 4},
		{"infinite metric holds", 4, math.Inf(1), 4},
		{"missing metric still clamps", 25, math.NaN(), 20},
		{"zero replicas: no per-pod metric, only the Min clamp", 0, 0.9, 1},
	}
	for _, c := range cases {
		h := NewHPA(hpaConfig(0), 1)
		if got := h.Step(t0, c.current, c.value); got != c.want {
			t.Errorf("%s: Step(current=%d, value=%v) = %d, want %d", c.name, c.current, c.value, got, c.want)
		}
		if h.Desired() != c.want {
			t.Errorf("%s: Desired() = %d, want %d", c.name, h.Desired(), c.want)
		}
	}
}

// The default behaviour adds at most max(4, current) replicas per Step:
// 2 -> 6 (+4), 6 -> 12 (+100%), 12 -> 20 (Max) while the metric asks for 5x.
func TestHPAScaleUpLimit(t *testing.T) {
	runSteps(t, "hpa", NewHPA(hpaConfig(300*time.Second), 2), []step{
		{0, 2, 3.5, 6},
		{15, 6, 3.5, 12},
		{30, 12, 3.5, 20},
		{45, 20, 3.5, 20},
	})
}

// Load drops from 7 to 2.8 busy pods. Scale-down waits until the 10-replica
// proposals have left the 300 s window, then goes to the highest proposal
// still inside the window (8, then 6, then 4), not straight to 4.
func TestHPADelayedScaleDown(t *testing.T) {
	runSteps(t, "hpa", NewHPA(hpaConfig(300*time.Second), 10), []step{
		{0, 10, 0.7, 10},
		{100, 10, 0.56, 10},  // proposal 8
		{200, 10, 0.42, 10},  // proposal 6
		{250, 10, 0.28, 10},  // proposal 4
		{299, 10, 0.28, 10},  // the t=0 proposal of 10 is still in the window
		{300, 10, 0.28, 8},   // window (0s, 300s]: max(8, 6, 4, 4, 4)
		{400, 8, 0.35, 6},    // window (100s, 400s]: max(6, 4, 4, 4, 4)
		{500, 6, 2.8 / 6, 4}, // window (200s, 500s]: only 4s left
		{510, 4, 1.4, 8},     // scale-up is immediate despite the window
	})
}

// Kubernetes seeds the scale-down history with the replica count it finds
// at its first sync (recordInitialRecommendation), so an HPA created over a
// 6-replica workload holds 6 for one window whatever initial is, including
// initial = Min.
func TestHPAInitialReplicas(t *testing.T) {
	for _, initial := range []int{1, 6, 20} {
		runSteps(t, fmt.Sprintf("initial=%d", initial), NewHPA(hpaConfig(300*time.Second), initial), []step{
			{1000, 6, 0.1, 6},
			{1299, 6, 0.1, 6},
			{1300, 6, 0.1, 1},
		})
	}
	// initial never enters the history: after a first sync at 2 replicas,
	// a workload resized to 8 elsewhere goes back to the highest proposal in
	// the window (the seed 2), not to min(8, initial 20).
	runSteps(t, "initial above current", NewHPA(hpaConfig(300*time.Second), 20), []step{
		{1000, 2, 0.1, 2},
		{1100, 8, 0.1, 2},
	})
	for initial, want := range map[int]int{50: 20, -3: 1, 7: 7} {
		if got := NewHPA(hpaConfig(0), initial).Desired(); got != want {
			t.Errorf("NewHPA(initial=%d).Desired() = %d, want %d", initial, got, want)
		}
	}
}

// A sync without a metric neither scales nor records a proposal, so it
// cannot extend the scale-down hold.
func TestHPAMissingMetricRecordsNothing(t *testing.T) {
	runSteps(t, "hpa", NewHPA(hpaConfig(300*time.Second), 4), []step{
		{0, 4, 0.7, 4},
		{200, 8, math.NaN(), 8},
		{299, 8, 0.35, 4}, // only the 4s from t=0 are in the window: an 8 at t=200 would hold 8
	})
}

func TestHPAInvalidTargetHolds(t *testing.T) {
	h := NewHPA(HPAConfig{Target: 0, Min: 1, Max: 10}, 3)
	if got := h.Step(t0, 5, 0.9); got != 5 {
		t.Errorf("Step with Target 0 = %d, want 5 (hold)", got)
	}
}
