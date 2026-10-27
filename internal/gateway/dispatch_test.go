package gateway

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"sync"
	"testing"
	"time"

	"tenantscaler/internal/fairq"
	"tenantscaler/internal/policy"
)

// TestFairWindowNeverExceedsSlots floods two replicas with concurrent
// requests: no replica ever runs more than SlotsPerReplica of them, the
// rest wait in the gateway, and every request completes.
func TestFairWindowNeverExceedsSlots(t *testing.T) {
	up1 := newFakeUpstream(t, func(f *fakeUpstream) { f.tokenDelay = 2 * time.Millisecond })
	up2 := newFakeUpstream(t, func(f *fakeUpstream) { f.tokenDelay = 2 * time.Millisecond })
	g, base := startGateway(t, Config{SlotsPerReplica: 2, Discovery: StaticDiscovery{up1.URL(), up2.URL()}})

	var wg sync.WaitGroup
	results := make([]result, 24)
	for i := range results {
		wg.Go(func() {
			results[i] = post(t, base, []string{"A", "B"}[i%2], "", completionBody("w w", 5, true))
		})
	}
	maxInFlight := 0
	sampling := make(chan struct{})
	go func() {
		defer close(sampling)
		for range 200 {
			maxInFlight = max(maxInFlight, g.Stats(0).InFlight)
			time.Sleep(time.Millisecond)
		}
	}()
	wg.Wait()
	<-sampling

	for i, r := range results {
		if r.status != http.StatusOK || r.tokens != 5 || !r.done {
			t.Fatalf("request %d: status %d tokens %d done %v: %s", i, r.status, r.tokens, r.done, r.body)
		}
	}
	_, max1, n1 := up1.stats()
	_, max2, n2 := up2.stats()
	if max1 > 2 || max2 > 2 {
		t.Errorf("replica concurrency %d and %d, want <= 2 (the dispatch window)", max1, max2)
	}
	if max(max1, max2) < 2 {
		t.Errorf("replica concurrency never reached the window of 2 (got %d, %d)", max1, max2)
	}
	if len(n1)+len(n2) != len(results) || len(n1) == 0 || len(n2) == 0 {
		t.Errorf("replicas served %d and %d requests, want %d spread over both", len(n1), len(n2), len(results))
	}
	if maxInFlight > 4 {
		t.Errorf("gateway in-flight peaked at %d, want <= 4 (2 replicas x 2 slots)", maxInFlight)
	}
	if st := g.Stats(0); st.Slots != 4 || st.Endpoints != 2 || st.InFlight != 0 || st.Queued != 0 {
		t.Errorf("stats after the run: %+v", st)
	}
}

// TestQueueingByMode holds every request at a 1-slot replica: passthrough
// forwards them all at once (they queue in the replica), fair mode keeps
// all but one in its own queue.
func TestQueueingByMode(t *testing.T) {
	for _, tt := range []struct {
		mode             string
		upstream, queued int
	}{
		{ModePassthrough, 4, 0},
		{ModeFair, 1, 3},
	} {
		t.Run(tt.mode, func(t *testing.T) {
			up := newFakeUpstream(t, func(f *fakeUpstream) { f.release = make(chan struct{}) })
			g, base := startGateway(t, Config{Mode: tt.mode, SlotsPerReplica: 1, Discovery: StaticDiscovery{up.URL()}})
			var wg sync.WaitGroup
			statuses := make([]int, 4)
			for i := range statuses {
				wg.Go(func() { statuses[i] = post(t, base, "A", "", completionBody("w", 2, true)).status })
			}
			waitFor(t, "requests to arrive", 5*time.Second, func() bool {
				active, _, _ := up.stats()
				return active == tt.upstream && g.Stats(0).Queued == tt.queued
			})
			time.Sleep(30 * time.Millisecond) // nothing else may be forwarded meanwhile
			if active, _, _ := up.stats(); active != tt.upstream {
				t.Fatalf("upstream has %d requests, want %d", active, tt.upstream)
			}
			up.open()
			wg.Wait()
			for i, s := range statuses {
				if s != http.StatusOK {
					t.Errorf("request %d: status %d", i, s)
				}
			}
		})
	}
}

// TestWeightedFairness backlogs two tenants with weights 1 and 3 behind
// one slot: the weight-3 tenant is served ~3x the tokens.
func TestWeightedFairness(t *testing.T) {
	up := newFakeUpstream(t, nil)
	disc := &testDiscovery{}
	g, base := startGateway(t, Config{SlotsPerReplica: 1, Discovery: disc})
	if !g.SetPolicy(policy.Policy{Version: 1, Tenants: map[string]policy.TenantPolicy{
		"A": {Weight: 1, RetryAfterSeconds: 1}, "B": {Weight: 3, RetryAfterSeconds: 1},
	}}) {
		t.Fatal("policy not applied")
	}
	const perTenant = 24
	var wg sync.WaitGroup
	for i := range 2 * perTenant {
		tenant := []string{"A", "B"}[i%2]
		wg.Go(func() {
			if r := post(t, base, tenant, "", completionBody(tenant+" w w w", 4, true)); r.status != http.StatusOK {
				t.Errorf("tenant %s: status %d: %s", tenant, r.status, r.body)
			}
		})
	}
	waitFor(t, "both tenants backlogged", 5*time.Second, func() bool { return g.Stats(0).Queued == 2*perTenant })
	disc.set(up.URL())
	g.kick()
	wg.Wait()

	_, maxActive, order := up.stats()
	if maxActive != 1 {
		t.Errorf("replica concurrency %d, want 1", maxActive)
	}
	// While both are backlogged (the first perTenant dispatches), service
	// follows the weights; equal request sizes make tokens proportional to
	// request counts.
	served := map[string]int{}
	for _, tenant := range order[:perTenant] {
		served[tenant]++
	}
	ratio := float64(served["B"]) / float64(max(served["A"], 1))
	if ratio < 2.4 || ratio > 3.6 {
		t.Errorf("first %d dispatches: A %d, B %d (ratio %.2f), want ~3", perTenant, served["A"], served["B"], ratio)
	}
}

// TestPartitionRoutesTenantsToOwnEndpoints: with shares 1/1 over three
// endpoints, A owns the first and the leftover third, B the second.
func TestPartitionRoutesTenantsToOwnEndpoints(t *testing.T) {
	ups := []*fakeUpstream{newFakeUpstream(t, nil), newFakeUpstream(t, nil), newFakeUpstream(t, nil)}
	byURL := map[string]*fakeUpstream{}
	var urls []string
	for _, u := range ups {
		byURL[u.URL()] = u
		urls = append(urls, u.URL())
	}
	slices.Sort(urls)
	_, base := startGateway(t, Config{Mode: ModePartition, SlotsPerReplica: 1, Discovery: StaticDiscovery(urls)})

	var wg sync.WaitGroup
	for i := range 12 {
		tenant := []string{"A", "B"}[i%2]
		wg.Go(func() {
			if r := post(t, base, tenant, "", completionBody(tenant+" w", 3, true)); r.status != http.StatusOK {
				t.Errorf("tenant %s: status %d: %s", tenant, r.status, r.body)
			}
		})
	}
	wg.Wait()
	want := map[string]string{urls[0]: "A", urls[1]: "B", urls[2]: "A"}
	for url, owner := range want {
		_, maxActive, tenants := byURL[url].stats()
		if maxActive > 1 {
			t.Errorf("%s ran %d requests at once, want <= 1", url, maxActive)
		}
		for _, tenant := range tenants {
			if tenant != owner {
				t.Errorf("%s served tenant %s, but it belongs to %s", url, tenant, owner)
			}
		}
	}
}

func TestPartitionAssignment(t *testing.T) {
	eps := []string{"e1", "e2", "e3", "e4", "e5", "e6"}
	for _, tt := range []struct {
		name    string
		eps     []string
		tenants []string
		shares  map[string]int
		want    map[string][]string
	}{
		{"exact", eps[:4], []string{"A", "B"}, map[string]int{"A": 1, "B": 3},
			map[string][]string{"A": {"e1"}, "B": {"e2", "e3", "e4"}}},
		{"leftovers round robin", eps, []string{"A", "B"}, map[string]int{"A": 1, "B": 1},
			map[string][]string{"A": {"e1", "e3", "e5"}, "B": {"e2", "e4", "e6"}}},
		{"too few endpoints", eps[:2], []string{"A", "B", "C"}, map[string]int{"A": 1, "B": 2, "C": 1},
			map[string][]string{"A": {"e1"}, "B": {"e2"}, "C": {}}},
		{"zero shares", eps[:3], []string{"A", "B"}, map[string]int{},
			map[string][]string{"A": {"e1", "e3"}, "B": {"e2"}}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got := partition(tt.eps, tt.tenants, tt.shares)
			for _, tenant := range tt.tenants {
				if !slices.Equal(got[tenant], tt.want[tenant]) {
					t.Errorf("tenant %s owns %v, want %v", tenant, got[tenant], tt.want[tenant])
				}
			}
		})
	}
}

// TestClientCancelRemovesQueuedRequest: a client that gives up while its
// request waits for a slot leaves nothing behind in the queue.
func TestClientCancelRemovesQueuedRequest(t *testing.T) {
	g, base := startGateway(t, Config{Discovery: &testDiscovery{}})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan result, 1)
	go func() { done <- call(ctx, t, base, "A", "", completionBody("w", 4, true)) }()
	waitFor(t, "the request to queue", 5*time.Second, func() bool { return g.Stats(0).Queued == 1 })
	cancel()
	<-done
	waitFor(t, "the queue to empty", 5*time.Second, func() bool { return g.Stats(0).Queued == 0 })
	for _, s := range g.Stats(0).Scheduler {
		if s.Queued != 0 || s.InFlight != 0 {
			t.Errorf("scheduler state %+v, want nothing queued or in flight", s)
		}
	}
	waitFor(t, "the cancellation to be counted", 5*time.Second, func() bool {
		return metricValue(t, base, `tenantscaler_gateway_failed_total{reason="canceled",tenant="A"}`) == 1
	})
}

// TestNoEndpointTimesOut: without a replica the request is answered 503
// after the dispatch timeout, and counts as an SLO miss whose TTFT is at
// least its wait.
func TestNoEndpointTimesOut(t *testing.T) {
	g, base := startGateway(t, Config{Discovery: &testDiscovery{}, DispatchTimeout: 100 * time.Millisecond})
	start := time.Now()
	r := post(t, base, "A", "", completionBody("w", 4, true))
	if r.status != http.StatusServiceUnavailable {
		t.Fatalf("status %d, want 503: %s", r.status, r.body)
	}
	if e := errorBody(t, r.body); e.Type != "unavailable" || e.Tenant != "A" {
		t.Errorf("error %+v", e)
	}
	if took := time.Since(start); took < 100*time.Millisecond {
		t.Errorf("answered after %s, before the dispatch timeout", took)
	}
	if st := g.Stats(0); st.Queued != 0 {
		t.Errorf("queued = %d after the timeout, want 0", st.Queued)
	}
	if v := metricValue(t, base, fmt.Sprintf(`tenantscaler_gateway_failed_total{reason=%q,tenant="A"}`, FailDispatchTimeout)); v != 1 {
		t.Errorf("dispatch timeouts = %v, want 1", v)
	}
	st := g.Stats(time.Minute)
	if a := st.Tenants[0]; a.Completed != 1 || !a.HasSLOData || a.SLOAttainment != 0 || a.P99TTFTSeconds < 0.1 || st.Failed["A"] != 1 {
		t.Errorf("window after the timeout: %+v with %d failed, want 1 failed SLO miss with p99 >= 0.1 s", a, st.Failed["A"])
	}
	if v := metricValue(t, base, `tenantscaler_gateway_completed_total{slo="missed",tenant="A"}`); v != 1 {
		t.Errorf("missed completions = %v, want 1", v)
	}
	if v := metricValue(t, base, `tenantscaler_gateway_ttft_seconds_sum{tenant="A"}`); v < 0.1 {
		t.Errorf("TTFT sum = %v s, want the >= 0.1 s wait", v)
	}
}

// TestRoundRobinMode holds one slot: sheddable traffic is rejected as
// "saturated" at once; queued requests go out by strict priority band,
// then round robin across tenants (not FIFO) inside a band.
func TestRoundRobinMode(t *testing.T) {
	up := newFakeUpstream(t, func(f *fakeUpstream) { f.release = make(chan struct{}) })
	g, base := startGateway(t, Config{Mode: ModeRoundRobin, SlotsPerReplica: 1, Discovery: StaticDiscovery{up.URL()}})
	var wg sync.WaitGroup
	send := func(label, tenant, priority string) {
		wg.Go(func() {
			if r := post(t, base, tenant, priority, completionBody(label+" w", 2, true)); r.status != http.StatusOK {
				t.Errorf("%s: status %d: %s", label, r.status, r.body)
			}
		})
	}
	send("A0", "A", "standard")
	waitFor(t, "A0 to take the only slot", 5*time.Second, func() bool { active, _, _ := up.stats(); return active == 1 })

	r := post(t, base, "B", "sheddable", completionBody("shed w", 2, true))
	if r.status != http.StatusTooManyRequests {
		t.Fatalf("sheddable while saturated: status %d, want 429: %s", r.status, r.body)
	}
	if e := errorBody(t, r.body); e.Reason != fairq.ReasonSaturated || e.Tenant != "B" {
		t.Errorf("429 body %+v, want reason %q", e, fairq.ReasonSaturated)
	}
	if ra, err := strconv.Atoi(r.header.Get("Retry-After")); err != nil || ra < 1 {
		t.Errorf("Retry-After %q, want whole seconds >= 1", r.header.Get("Retry-After"))
	}

	for i, q := range [][3]string{{"A1", "A", "standard"}, {"A2", "A", "standard"}, {"B1", "B", "standard"}, {"B2", "B", "critical"}} {
		send(q[0], q[1], q[2])
		waitFor(t, q[0]+" to queue", 5*time.Second, func() bool { return g.Stats(0).Queued == i+1 })
	}
	up.open()
	wg.Wait()
	_, maxActive, order := up.stats()
	// B2 (critical) first; then the standard band alternates tenants,
	// starting after A, which A0 was last served from.
	if want := []string{"A0", "B2", "B1", "A1", "A2"}; !slices.Equal(order, want) || maxActive != 1 {
		t.Errorf("dispatch order %v (max %d at once), want %v one at a time", order, maxActive, want)
	}
}

// TestSlotReservation: while A has a request in flight and an unused share,
// a queued request of C, which the policy sheds, waits although a slot is
// free; A's next request takes that slot at once.
func TestSlotReservation(t *testing.T) {
	up := newFakeUpstream(t, func(f *fakeUpstream) { f.release = make(chan struct{}) })
	g, base := startGateway(t, Config{SlotsPerReplica: 2, Tenants: testTenants("A", "C"), Discovery: StaticDiscovery{up.URL()}})
	if !g.SetPolicy(policy.Policy{Version: 1, Tenants: map[string]policy.TenantPolicy{
		"A": {Weight: 1, Slots: 2, GuaranteedSlots: 2},
		"C": {Weight: 1, Shed: true},
	}}) {
		t.Fatal("policy rejected")
	}
	body := completionBody("w", 2, true)
	results := make(chan result, 3)
	send := func(tenant string) { go func() { results <- post(t, base, tenant, "", body) }() }
	active := func(n int) func() bool {
		return func() bool { a, _, _ := up.stats(); return a == n }
	}
	send("A")
	waitFor(t, "A's first request in flight", 5*time.Second, active(1))
	send("C")
	waitFor(t, "C's request to queue", 5*time.Second, func() bool { return g.Stats(0).Queued == 1 })
	time.Sleep(50 * time.Millisecond)
	if a, _, _ := up.stats(); a != 1 {
		t.Fatalf("%d requests at the replica, want 1: C took the slot reserved for A", a)
	}
	send("A")
	waitFor(t, "A's second request in flight", 5*time.Second, active(2))
	if q := g.Stats(0).Queued; q != 1 {
		t.Fatalf("queued = %d, want C's request still waiting", q)
	}
	up.open()
	for range 3 {
		if r := <-results; r.status != http.StatusOK {
			t.Errorf("status %d, want 200: %s", r.status, r.body)
		}
	}
}
