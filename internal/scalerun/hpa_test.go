package scalerun

import (
	"context"
	"errors"
	"math"
	"slices"
	"strings"
	"testing"
	"time"

	"tenantscaler/internal/autoscale"
	"tenantscaler/internal/pool"
	"tenantscaler/internal/replica"
)

func draining(running int) pool.ReplicaStatus {
	return pool.ReplicaStatus{State: replica.StateDraining, Running: running}
}

// setReplicas replaces the replicas (Ready and Starting derived) and keeps
// Desired.
func (f *fakePool) setReplicas(reps ...pool.ReplicaStatus) {
	np := newFakePool(0, reps...)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.st.Replicas, f.st.Ready, f.st.Starting = np.st.Replicas, np.st.Ready, np.st.Starting
}

func TestHPACountsReplicasNotReadyAsIdle(t *testing.T) {
	tests := []struct {
		name    string
		pool    *fakePool
		metric  float64
		desired int
	}{
		// Ready replicas run 4/4 and 2/4 slots while a third starts:
		// (1 + 0.5 + 0) / 3 = 0.5, the target, so hold at 3 (as the
		// Kubernetes replica calculator does with an unready pod).
		{"starting replica counts as idle", newFakePool(3, ready(4, 9), ready(2, 0), starting()), 0.5, 3},
		{"replica not relaunched yet counts as idle", newFakePool(3, ready(4, 0), ready(2, 0)), 0.5, 3},
		{"saturated ready replicas scale up", newFakePool(2, ready(4, 0), ready(4, 3)), 1, 4},
		{"draining replica is left out", newFakePool(2, ready(4, 0), ready(4, 0), draining(4)), 1, 4},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			d, err := hpaRunner(tc.pool).RunOnce(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if !d.MetricOK || math.Abs(d.Metric-tc.metric) > 1e-9 || d.Desired != tc.desired || d.Samples != 1 {
				t.Errorf("decision %+v, want occupancy %v from 1 sample and %d replicas", d, tc.metric, tc.desired)
			}
			want := []int{}
			if tc.desired != d.Current {
				want = []int{tc.desired}
			}
			if got := tc.pool.scaleCalls(); !slices.Equal(got, want) || d.Scaled != (len(want) > 0) {
				t.Errorf("Scale calls %v (scaled %v), want %v", got, d.Scaled, want)
			}
		})
	}
}

func TestHPADoesNotCompoundWhileReplicasStart(t *testing.T) {
	p := newFakePool(1, ready(4, 0))
	cfg := autoscale.DefaultHPAConfig(1, 10) // target 0.7
	r := &Runner{Algo: AlgoHPA, HPA: autoscale.NewHPA(cfg, 1), Pool: p, MaxNumSeqs: 4,
		Clock: (&fakeClock{}).now, Logger: discard}
	ctx := context.Background()
	if d, err := r.RunOnce(ctx); err != nil || !d.Scaled || d.Desired != 2 {
		t.Fatalf("first sync: %+v, %v; want 1.0 / 0.7 -> a scale up to 2", d, err)
	}
	// The new replica starts over several syncs while the old one stays
	// saturated: (1 + 0) / 2 = 0.5 against 0.7 proposes ceil(2 * 0.71) = 2.
	// Reading the ready replica alone (1.0) would go 2 -> 3 -> 5 -> 8.
	p.setReplicas(ready(4, 0), starting())
	for i := range 3 {
		if d, err := r.RunOnce(ctx); err != nil || d.Scaled || d.Desired != 2 || d.Metric != 0.5 {
			t.Errorf("sync %d while starting: %+v, %v; want a hold at 2 with occupancy 0.5", i+2, d, err)
		}
	}
	if got := p.scaleCalls(); !slices.Equal(got, []int{2}) {
		t.Errorf("Scale calls %v, want [2]", got)
	}
}

func TestHPAAveragesOccupancyOverTheCycle(t *testing.T) {
	p := newFakePool(2, ready(4, 0), ready(4, 0))
	r := hpaRunner(p) // target 0.5
	ctx := context.Background()
	for range 2 {
		if err := r.Sample(ctx); err != nil {
			t.Fatal(err)
		}
	}
	p.setReplicas(ready(0, 0), ready(0, 0)) // idle just when the sync reads it
	d, err := r.RunOnce(ctx)
	// (1 + 1 + 0) / 3 = 0.67 -> ceil(2 * 0.67 / 0.5) = 3; the instantaneous
	// reading (0) would not scale up.
	if err != nil || d.Samples != 3 || math.Abs(d.Metric-2.0/3) > 1e-9 || !d.Scaled || d.Desired != 3 {
		t.Fatalf("decision %+v, %v; want the mean 0.67 of 3 samples and a scale to 3", d, err)
	}
	if !strings.Contains(d.Reason, "mean of 3 samples") {
		t.Errorf("reason %q does not say the occupancy is a mean", d.Reason)
	}
	// The next sync starts a new window: only its own (idle) sample.
	if d, err := r.RunOnce(ctx); err != nil || d.Samples != 1 || d.Metric != 0 || d.Scaled {
		t.Errorf("next decision %+v, %v; want 1 idle sample and a hold (scale-down window)", d, err)
	}
}

func TestSampleIsHPAOnly(t *testing.T) {
	broken := &fakePool{statusErr: errors.New("pool down")}
	if err := kedaRunner(broken, "", &fakeClock{}).Sample(context.Background()); err != nil {
		t.Errorf("KEDA Sample = %v, want nil without reading the pool", err)
	}
	if err := hpaRunner(broken).Sample(context.Background()); err == nil || !strings.Contains(err.Error(), "pool down") {
		t.Errorf("HPA Sample = %v, want the pool status error", err)
	}
}

func TestHPANoScaleCallWhenUnchanged(t *testing.T) {
	tests := []struct {
		name   string
		pool   *fakePool
		reason string
	}{
		{"within tolerance", newFakePool(2, ready(2, 0), ready(2, 5)), "hold at 2"},
		{"no ready replica", newFakePool(2, starting(), starting()), "no ready replica"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			d, err := hpaRunner(tc.pool).RunOnce(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if d.Scaled || d.Desired != 2 || len(tc.pool.scaleCalls()) != 0 || !strings.Contains(d.Reason, tc.reason) {
				t.Errorf("decision %+v, Scale calls %v: want a hold (%q)", d, tc.pool.scaleCalls(), tc.reason)
			}
		})
	}
}

func TestRunSamplesOccupancyBetweenSyncs(t *testing.T) {
	r := hpaRunner(newFakePool(1, ready(2, 0)))
	r.Cycle, r.SampleEvery = 60*time.Millisecond, 5*time.Millisecond
	var samples []int // OnDecision runs on the goroutine calling Run
	r.OnDecision = func(d Decision) { samples = append(samples, d.Samples) }
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if err := r.Run(ctx); err != nil {
		t.Fatal(err)
	}
	// The first sync has only its own sample; the next, 60 ms later, also
	// has the samples taken every 5 ms in between.
	if len(samples) < 2 || samples[0] != 1 || samples[1] < 3 {
		t.Errorf("samples per sync %v, want 1 and then several", samples)
	}
}
