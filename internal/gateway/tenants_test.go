package gateway

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"tenantscaler/internal/model"
	"tenantscaler/internal/policy"
)

// tenantNames returns the tenants of the per-tenant observations.
func tenantNames(st Stats) []string {
	var out []string
	for _, ob := range st.Tenants {
		out = append(out, ob.Tenant)
	}
	return out
}

// TestSetTenants: a new tenant is accepted and a removed one gets 400,
// while the removed tenant's already queued request is still served.
func TestSetTenants(t *testing.T) {
	up := newFakeUpstream(t, nil)
	disc := &testDiscovery{}
	g, base := startGateway(t, Config{Discovery: disc})
	queued := make(chan result, 1)
	go func() { queued <- post(t, base, "A", "", completionBody("w", 2, true)) }()
	waitFor(t, "A's request to queue", 5*time.Second, func() bool { return g.Stats(0).Queued == 1 })

	if err := g.SetTenants(testTenants("C")); err != nil {
		t.Fatal(err)
	}
	disc.set(up.URL())
	if r := <-queued; r.status != http.StatusOK || r.tokens != 2 {
		t.Errorf("A's request admitted before the removal: status %d tokens %d, want it served", r.status, r.tokens)
	}
	if r := post(t, base, "A", "", completionBody("w", 2, true)); r.status != http.StatusBadRequest || errorBody(t, r.body).Tenant != "A" {
		t.Errorf("removed tenant A: status %d, want 400: %s", r.status, r.body)
	}
	if r := post(t, base, "C", "", completionBody("w", 2, true)); r.status != http.StatusOK {
		t.Errorf("new tenant C: status %d, want 200: %s", r.status, r.body)
	}
	if names := tenantNames(g.Stats(0)); !slices.Equal(names, []string{"C"}) {
		t.Errorf("stats list tenants %v, want [C]", names)
	}
	if v := metricValue(t, base, `tenantscaler_gateway_shed_total{reason="over-share",tenant="C"}`); v != 0 {
		t.Errorf("C's series not initialised at zero: %v", v)
	}
	for name, bad := range map[string][]model.TenantSpec{"none": nil, "duplicate": testTenants("C", "C")} {
		if g.SetTenants(bad) == nil {
			t.Errorf("SetTenants accepted %s", name)
		}
	}
	if _, ok := g.tenantSpec("C"); !ok {
		t.Error("a rejected tenant set replaced the tenants")
	}
}

// TestSetTenantsPartitionWithdrawsQueued: in partition mode a removed
// tenant owns no endpoints, so its queued request is answered 503 at once
// rather than at the dispatch timeout, and is not an SLO outcome.
func TestSetTenantsPartitionWithdrawsQueued(t *testing.T) {
	g, base := startGateway(t, Config{Mode: ModePartition, Discovery: &testDiscovery{}, DispatchTimeout: time.Minute})
	done := make(chan result, 1)
	go func() { done <- post(t, base, "B", "", completionBody("w", 2, true)) }()
	waitFor(t, "B's request to queue", 5*time.Second, func() bool { return g.Stats(0).Queued == 1 })

	start := time.Now()
	if err := g.SetTenants(testTenants("A")); err != nil {
		t.Fatal(err)
	}
	r := <-done
	if took := time.Since(start); took > 5*time.Second {
		t.Errorf("answered after %s, want at once", took)
	}
	if e := errorBody(t, r.body); r.status != http.StatusServiceUnavailable || e.Type != "unavailable" || e.Tenant != "B" {
		t.Errorf("status %d error %+v, want 503 unavailable for B", r.status, e)
	}
	if v := metricValue(t, base, `tenantscaler_gateway_failed_total{reason="tenant-removed",tenant="B"}`); v != 1 {
		t.Errorf("tenant-removed failures = %v, want 1", v)
	}
	if v := metricValue(t, base, `tenantscaler_gateway_completed_total{slo="missed",tenant="B"}`); v != 0 {
		t.Errorf("B's missed completions = %v, want 0", v)
	}
	st := g.Stats(0)
	if st.Queued != 0 || len(st.Scheduler) != 1 || st.Scheduler[0].Tenant != "A" {
		t.Errorf("after the removal: queued %d, schedulers %+v, want 0 and only A's", st.Queued, st.Scheduler)
	}
	if r := post(t, base, "B", "", completionBody("w", 2, true)); r.status != http.StatusBadRequest {
		t.Errorf("new request of removed tenant B: status %d, want 400", r.status)
	}
}

// TestPartitionStatsListEachTenantOnce: each tenant's FIFO knows only its
// own tenant, so Stats.Scheduler has one entry per tenant, before and
// after a policy names them all, and the policy still reaches each.
func TestPartitionStatsListEachTenantOnce(t *testing.T) {
	g, _ := startGateway(t, Config{Mode: ModePartition, Tenants: testTenants("A", "B", "C"), Discovery: &testDiscovery{}})
	check := func(when string) {
		t.Helper()
		var names []string
		for _, s := range g.Stats(0).Scheduler {
			names = append(names, s.Tenant)
		}
		if !slices.Equal(names, []string{"A", "B", "C"}) {
			t.Errorf("%s: scheduler states for %v, want one per tenant", when, names)
		}
	}
	check("at start")
	if !g.SetPolicy(policy.Policy{Version: 1, Tenants: map[string]policy.TenantPolicy{
		"A": {Weight: 2, RetryAfterSeconds: 1}, "B": {Weight: 1}, "C": {Weight: 1},
	}}) {
		t.Fatal("policy not applied")
	}
	check("after a policy")
	if a := tenantState(t, g.Stats(0), "A"); a.Weight != 2 {
		t.Errorf("A's partition scheduler has weight %v, want the policy's 2", a.Weight)
	}
}

// TestFollowTenants: failed, empty and invalid reads keep the tenants; the
// first valid changed set replaces them.
func TestFollowTenants(t *testing.T) {
	g, _ := startGateway(t, Config{Discovery: &testDiscovery{}})
	var calls atomic.Int32
	var keptAB atomic.Bool
	list := func(context.Context) ([]model.TenantSpec, error) {
		switch calls.Add(1) {
		case 1:
			return nil, errors.New("tenantscalers is forbidden")
		case 2:
			return nil, nil
		case 3:
			return testTenants("A", "A"), nil
		case 4:
			keptAB.Store(slices.Equal(tenantNames(g.Stats(0)), []string{"A", "B"}))
		}
		return testTenants("C"), nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		g.FollowTenants(ctx, list, 10*time.Millisecond)
	}()
	waitFor(t, "tenant C from the source", 5*time.Second, func() bool { _, ok := g.tenantSpec("C"); return ok })
	cancel()
	<-done
	if !keptAB.Load() {
		t.Error("a failed, empty or invalid read replaced the tenants A and B")
	}
	if names := tenantNames(g.Stats(0)); !slices.Equal(names, []string{"C"}) {
		t.Errorf("tenants %v, want [C]", names)
	}
}
