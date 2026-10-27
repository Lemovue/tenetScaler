package estimate

import (
	"math"
	"testing"

	"tenantscaler/internal/model"
)

func TestReplicas(t *testing.T) {
	cases := []struct {
		name                     string
		rate, headroom, capacity float64
		want                     int
	}{
		{"zero rate", 0, 0.2, 400, 0},
		{"negative rate", -50, 0.2, 400, 0},
		{"NaN rate", math.NaN(), 0.2, 400, 0},
		{"exact multiple, no headroom", 800, 0, 400, 2},
		{"fraction rounds up", 1000, 0, 400, 3},
		{"headroom pushes past a boundary", 400, 0.2, 400, 2},
		{"headroom applied before ceil", 1000, 0.2, 400, 3},
		{"headroom 0.5", 1000, 0.5, 400, 4},
		{"tiny positive rate needs one replica", 1e-9, 0.2, 400, 1},
		{"floating-point noise is not demand", 100, 0.1, 110, 1},
		{"excess above the tolerance is demand", 400 * (1 + 1e-10), 0, 400, 2},
		{"exact large integer", 1e9 * 400, 0, 400, 1_000_000_000},
		{"large value still rounds up", (2e9 + 0.5) * 400, 0, 400, 2_000_000_001},
		{"largest exactly representable integer", (1 << 53) * 400, 0, 400, 1 << 53},
		{"headroom of -1 cancels the rate", 400, -1, 400, 0},
		{"zero capacity per replica", 400, 0.2, 0, 0},
		{"negative capacity per replica", 400, 0.2, -1, 0},
		{"NaN capacity per replica", 400, 0.2, math.NaN(), 0},
		{"huge rate saturates", 1e300, 0.2, 400, math.MaxInt},
		{"infinite rate saturates", math.Inf(1), 0.2, 400, math.MaxInt},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := Replicas(c.rate, c.headroom, c.capacity); got != c.want {
				t.Fatalf("Replicas(%v, %v, %v) = %d, want %d", c.rate, c.headroom, c.capacity, got, c.want)
			}
		})
	}
}

// TestReplicasNoiseCaseIsReal guards the noise test above: the naive formula
// really does produce a value just above 1 for these inputs. The operands
// are variables because Go evaluates constant expressions exactly.
func TestReplicasNoiseCaseIsReal(t *testing.T) {
	rate, headroom, capacity := 100.0, 0.1, 110.0
	if naive := math.Ceil(rate * (1 + headroom) / capacity); naive != 2 {
		t.Fatalf("naive ceil = %v; the noise case no longer exercises the tolerance", naive)
	}
}

// TestReplicasIsCeilAcrossMagnitudes: an exact integer is never taken down,
// and a half replica rounds up unless it is within the documented relative
// tolerance of n, which first happens at n = 5e11, far beyond any real pool.
// A power-of-two capacity keeps every product and quotient exact, so only
// the rounding rule is under test.
func TestReplicasIsCeilAcrossMagnitudes(t *testing.T) {
	const capacity = 1024
	for k := 1; k <= 1e15; k *= 10 {
		for _, n := range []int{k, k + 1, 3 * k, 7*k - 1} {
			if got := Replicas(float64(n)*capacity, 0, capacity); got != n {
				t.Errorf("Replicas(%d*%d, 0, %d) = %d, want %d", n, capacity, capacity, got, n)
			}
			if n >= 1<<52 { // n+0.5 is no longer representable
				continue
			}
			want := n + 1
			if 0.5 <= intTolerance*float64(n) {
				want = n
			}
			half := float64(n) + 0.5
			if got := Replicas(half*capacity, 0, capacity); got != want {
				t.Errorf("Replicas(%v*%d, 0, %d) = %d, want %d", half, capacity, capacity, got, want)
			}
		}
	}
}

func TestReplicasMonotonicInRate(t *testing.T) {
	prev := 0
	for rate := 0.0; rate <= 5000; rate += 7.5 {
		got := Replicas(rate, 0.2, 400)
		if got < prev {
			t.Fatalf("Replicas decreased from %d to %d at rate %v", prev, got, rate)
		}
		if float64(got)*400 < rate*1.2*(1-intTolerance) {
			t.Fatalf("Replicas(%v) = %d cannot serve rate*(1+headroom) = %v", rate, got, rate*1.2)
		}
		prev = got
	}
}

func TestSlack(t *testing.T) {
	slo := model.SLO{TTFTSeconds: 2, TPOTMillis: 100, TargetFraction: 0.99}
	withData := func(p99 float64, queue int) model.TenantObservation {
		return model.TenantObservation{HasSLOData: true, Completed: 10, P99TTFTSeconds: p99, QueueLength: queue, SLOAttainment: 1}
	}
	withAttainment := func(p99, attainment float64) model.TenantObservation {
		o := withData(p99, 0)
		o.SLOAttainment = attainment
		return o
	}
	cases := []struct {
		name string
		obs  model.TenantObservation
		slo  model.SLO
		want float64
	}{
		{"comfortable", withData(1, 0), slo, 0.5},
		{"exactly at the bound", withData(2, 0), slo, 0},
		{"violating", withData(3, 0), slo, -0.5},
		{"badly violating clamps to -1", withData(10, 5), slo, -1},
		{"zero latency", withData(0, 0), slo, 1},
		{"negative p99 clamps to 1", withData(-4, 0), slo, 1},
		{"infinite p99 clamps to -1", withData(math.Inf(1), 0), slo, -1},
		{"queue ignored when data exists", withData(1, 7), slo, 0.5},
		{"no data with queue is starving", model.TenantObservation{QueueLength: 3}, slo, -1},
		{"no data, no queue is idle", model.TenantObservation{}, slo, 1},
		{"no data ignores stale p99", model.TenantObservation{P99TTFTSeconds: 9}, slo, 1},
		{"NaN p99 with queue falls back to starving", withData(math.NaN(), 2), slo, -1},
		{"NaN p99 without queue falls back to idle", withData(math.NaN(), 0), slo, 1},
		{"zero bound and zero p99 fall back", withData(0, 1), model.SLO{}, -1},
		{"zero bound with latency is violating", withData(0.5, 0), model.SLO{}, -1},
		{"fast failures still show pressure", withAttainment(0.1, 0.5), slo, -1},
		{"attainment slightly under target", withAttainment(1, 0.985), slo, -0.5},
		{"attainment at target, latency decides", withAttainment(1, 0.995), slo, 0.5},
		{"100% target uses the minimum span", withAttainment(1, 0.995), model.SLO{TTFTSeconds: 2, TPOTMillis: 100, TargetFraction: 1}, -0.5},
		{"NaN attainment falls back to latency", withAttainment(1, math.NaN()), slo, 0.5},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := Slack(c.obs, c.slo)
			if math.Abs(got-c.want) > 1e-12 {
				t.Fatalf("Slack = %v, want %v", got, c.want)
			}
		})
	}
}
