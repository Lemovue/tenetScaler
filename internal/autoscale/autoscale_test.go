package autoscale

import (
	"math"
	"strings"
	"testing"
	"time"
)

// t0 is the injected clock origin; at(s) is s seconds after it.
var t0 = time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)

func at(sec int) time.Time { return t0.Add(time.Duration(sec) * time.Second) }

// stepper is what HPA and KEDA have in common.
type stepper interface {
	Step(now time.Time, current int, metric float64) int
}

// step is one scripted sync: at sec seconds, with current replicas and the
// metric (per-pod value for HPA, total queue for KEDA), expect want.
type step struct {
	sec     int
	current int
	metric  float64
	want    int
}

func runSteps(t *testing.T, name string, s stepper, steps []step) {
	t.Helper()
	for _, st := range steps {
		if got := s.Step(at(st.sec), st.current, st.metric); got != st.want {
			t.Errorf("%s: t=%ds current=%d metric=%v: got %d, want %d",
				name, st.sec, st.current, st.metric, got, st.want)
		}
	}
}

func TestValidate(t *testing.T) {
	hpa := DefaultHPAConfig(1, 10)
	keda := DefaultKEDAConfig(0, 10)
	if err := hpa.Validate(); err != nil {
		t.Errorf("default HPA config invalid: %v", err)
	}
	if err := keda.Validate(); err != nil {
		t.Errorf("default KEDA config invalid: %v", err)
	}
	if hpa.Target != 0.7 || hpa.Tolerance != 0.1 || hpa.ScaleDownWindow != 300*time.Second {
		t.Errorf("HPA defaults = %+v, want Target 0.7, Tolerance 0.1, window 300s", hpa)
	}
	if keda.TargetPerReplica != 4 || keda.Activation != 0 || keda.ScaleDownWindow != 300*time.Second {
		t.Errorf("KEDA defaults = %+v, want TargetPerReplica 4, Activation 0, window 300s", keda)
	}

	bad := []struct {
		name string
		err  error
		want string
	}{
		{"hpa zero target", with(hpa, func(c *HPAConfig) { c.Target = 0 }).Validate(), "target"},
		{"hpa NaN target", with(hpa, func(c *HPAConfig) { c.Target = math.NaN() }).Validate(), "target"},
		{"hpa negative tolerance", with(hpa, func(c *HPAConfig) { c.Tolerance = -0.1 }).Validate(), "tolerance"},
		{"hpa negative window", with(hpa, func(c *HPAConfig) { c.ScaleDownWindow = -time.Second }).Validate(), "window"},
		{"hpa negative min", with(hpa, func(c *HPAConfig) { c.Min = -1 }).Validate(), "min"},
		{"hpa max below min", with(hpa, func(c *HPAConfig) { c.Min, c.Max = 5, 4 }).Validate(), "max"},
		{"hpa zero max", with(hpa, func(c *HPAConfig) { c.Min, c.Max = 0, 0 }).Validate(), "max"},
		{"keda zero target", with(keda, func(c *KEDAConfig) { c.TargetPerReplica = 0 }).Validate(), "targetPerReplica"},
		{"keda infinite activation", with(keda, func(c *KEDAConfig) { c.Activation = math.Inf(1) }).Validate(), "activation"},
		{"keda NaN tolerance", with(keda, func(c *KEDAConfig) { c.Tolerance = math.NaN() }).Validate(), "tolerance"},
	}
	for _, b := range bad {
		if b.err == nil || !strings.Contains(b.err.Error(), b.want) {
			t.Errorf("%s: Validate() = %v, want an error mentioning %q", b.name, b.err, b.want)
		}
	}
}

func with[C any](c C, edit func(*C)) C {
	edit(&c)
	return c
}

func TestCeilReplicas(t *testing.T) {
	cases := map[float64]int{
		0: 0, 0.2: 1, 4: 4, 4.000000000000001: 4, 8.000000000000002: 8, 4.01: 5, 1e300: 1 << 30,
	}
	for in, want := range cases {
		if got := ceilReplicas(in); got != want {
			t.Errorf("ceilReplicas(%v) = %d, want %d", in, got, want)
		}
	}
}
