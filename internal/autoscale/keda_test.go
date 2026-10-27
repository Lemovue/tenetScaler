package autoscale

import (
	"math"
	"testing"
	"time"
)

func kedaConfig(activation float64, window time.Duration) KEDAConfig {
	return KEDAConfig{TargetPerReplica: 4, Activation: activation, Tolerance: 0.1, ScaleDownWindow: window, Min: 0, Max: 20}
}

func TestKEDAAverageValueToleranceAndActivation(t *testing.T) {
	cases := []struct {
		name       string
		activation float64
		current    int
		total      float64
		want       int
	}{
		{"AverageValue: ceil(total/target)", 0, 2, 20, 5},
		{"within tolerance", 0, 3, 12.5, 3},
		{"outside tolerance", 0, 3, 13.5, 4},
		{"inactive at zero stays at zero", 0, 0, 0, 0},
		{"activation from zero", 0, 0, 0.5, 1},
		{"at the activation threshold: Min", 5, 3, 5, 0},
		{"above the activation threshold", 5, 0, 5.5, 2},
		{"negative total is 0", 0, 2, -3, 0},
		{"missing metric holds", 0, 3, math.NaN(), 3},
		{"above Max is clamped", 0, 30, 120, 20},
	}
	for _, c := range cases {
		k := NewKEDA(kedaConfig(c.activation, 0), 0)
		if got := k.Step(t0, c.current, c.total); got != c.want {
			t.Errorf("%s: Step(current=%d, total=%v) = %d, want %d", c.name, c.current, c.total, got, c.want)
		}
		if k.Desired() != c.want {
			t.Errorf("%s: Desired() = %d, want %d", c.name, k.Desired(), c.want)
		}
	}
}

func TestKEDAScaleUpLimitAndMax(t *testing.T) {
	runSteps(t, "keda", NewKEDA(kedaConfig(0, 300*time.Second), 1), []step{
		{0, 1, 100, 5},   // wants 25, +4
		{15, 5, 100, 10}, // +100%
		{30, 10, 100, 20},
		{45, 20, 1000, 20}, // wants 250, Max 20
	})
}

// Deactivation (total <= Activation proposes Min) waits for the window,
// like KEDA's cooldown, and then scales to zero.
func TestKEDADelayedDeactivation(t *testing.T) {
	runSteps(t, "keda", NewKEDA(kedaConfig(5, 300*time.Second), 3), []step{
		{0, 3, 12, 3},
		{60, 3, 2, 3},
		{299, 3, 2, 3},
		{300, 3, 2, 0},
		{315, 0, 6, 2}, // reactivation is immediate
	})
}

// A scaler created with initial 0 over a workload already at 3 replicas
// still holds 3 for one window: the first Step seeds the scale-down history
// with the current count, not with initial.
func TestKEDAFirstStepSeedsCurrent(t *testing.T) {
	runSteps(t, "keda", NewKEDA(kedaConfig(0, 300*time.Second), 0), []step{
		{0, 3, 0, 3},
		{299, 3, 0, 3},
		{300, 3, 0, 0},
	})
}

func TestKEDAMinClampAndInvalidTarget(t *testing.T) {
	cfg := kedaConfig(0, 0)
	cfg.Min = 2
	if got := NewKEDA(cfg, 2).Step(t0, 4, 0); got != 2 {
		t.Errorf("idle with Min 2 = %d, want 2", got)
	}
	cfg.TargetPerReplica = 0
	if got := NewKEDA(cfg, 2).Step(t0, 4, 50); got != 4 {
		t.Errorf("Step with TargetPerReplica 0 = %d, want 4 (hold)", got)
	}
}
