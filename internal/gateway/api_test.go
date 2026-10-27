package gateway

import (
	"bytes"
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"tenantscaler/internal/fairq"
	"tenantscaler/internal/policy"
)

// postPolicy sends a policy (any JSON value) and returns status and body.
func postPolicy(t *testing.T, base string, p any) (int, PolicyResponse) {
	t.Helper()
	data, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := testClient.Post(base+"/policy", "application/json", bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var pr PolicyResponse
	if resp.StatusCode == http.StatusOK {
		if err := json.NewDecoder(resp.Body).Decode(&pr); err != nil {
			t.Fatal(err)
		}
	}
	return resp.StatusCode, pr
}

// TestShedAfterPolicy: once the controller marks C as shedding, C's
// sheddable requests get 429 with Retry-After while the pool is contended,
// and A's critical and standard requests, and C's critical ones, keep
// succeeding. With a free slot and nothing queued, C's sheddable requests
// are served: shedding is work-conserving.
func TestShedAfterPolicy(t *testing.T) {
	up := newFakeUpstream(t, func(f *fakeUpstream) {
		f.tokenDelay = time.Millisecond
		f.release = make(chan struct{})
	})
	g, base := startGateway(t, Config{SlotsPerReplica: 1, Tenants: testTenants("A", "C"), Discovery: StaticDiscovery{up.URL()}})
	body := completionBody("w w", 3, true)
	status, pr := postPolicy(t, base, policy.Policy{Version: 1, IssuedAt: time.Now(), Tenants: map[string]policy.TenantPolicy{
		"A": {Weight: 2, RetryAfterSeconds: 2},
		"C": {Weight: 1, Shed: true, RetryAfterSeconds: 2},
	}})
	if status != http.StatusOK || !pr.Applied || pr.Version != 1 {
		t.Fatalf("POST /policy: %d %+v", status, pr)
	}

	var mu sync.Mutex
	got := map[string][]result{}
	var wg sync.WaitGroup
	send := func(tenant, prio string) {
		wg.Go(func() {
			r := post(t, base, tenant, prio, body)
			mu.Lock()
			got[tenant+"/"+prio] = append(got[tenant+"/"+prio], r)
			mu.Unlock()
		})
	}
	// A's first request takes the only slot and is held there.
	send("A", "standard")
	waitFor(t, "A's request to hold the slot", 5*time.Second, func() bool {
		active, _, _ := up.stats()
		return active == 1
	})
	var shed []result
	for range 10 {
		shed = append(shed, post(t, base, "C", "sheddable", body))
	}
	for range 3 {
		send("A", "critical")
		send("A", "standard")
	}
	send("C", "critical")
	waitFor(t, "7 requests to queue", 5*time.Second, func() bool { return g.Stats(0).Queued == 7 })
	up.open()
	wg.Wait()
	for _, r := range shed {
		if r.status != http.StatusTooManyRequests {
			t.Fatalf("C sheddable: status %d, want 429: %s", r.status, r.body)
		}
		ra, err := strconv.Atoi(r.header.Get("Retry-After"))
		if err != nil || ra < 2 || ra > 3 {
			t.Errorf("Retry-After %q, want whole seconds in [2, 3] (base 2 s with up to 50%% jitter)", r.header.Get("Retry-After"))
		}
		if e := errorBody(t, r.body); e.Type != "rate_limited" || e.Reason != fairq.ReasonOverShare || e.Tenant != "C" {
			t.Errorf("429 body %+v", e)
		}
	}
	for key, n := range map[string]int{"A/critical": 3, "A/standard": 4, "C/critical": 1} {
		if len(got[key]) != n {
			t.Errorf("%s: %d results, want %d", key, len(got[key]), n)
		}
		for _, r := range got[key] {
			if r.status != http.StatusOK || r.tokens != 3 {
				t.Errorf("%s: status %d tokens %d, want 200 with 3 tokens (only C's sheddable traffic is shed)", key, r.status, r.tokens)
			}
		}
	}
	if r := post(t, base, "C", "sheddable", body); r.status != http.StatusOK {
		t.Errorf("C sheddable on an idle pool: status %d, want 200 (shedding is work-conserving)", r.status)
	}
	if v := metricValue(t, base, `tenantscaler_gateway_shed_total{reason="over-share",tenant="C"}`); v != 10 {
		t.Errorf("shed_total = %v, want 10", v)
	}
	// Demand counts every arrival, shed or not: C sent 11 sheddable
	// requests (10 shed) and 1 critical one.
	if v := metricValue(t, base, `tenantscaler_gateway_arrivals_total{priority="sheddable",tenant="C"}`); v != 11 {
		t.Errorf("C's sheddable arrivals = %v, want 11 (shed ones included)", v)
	}
	st := g.Stats(time.Minute)
	c := st.Tenants[1]
	if got := c.ArrivalRequestsPerSec * st.WindowSeconds; c.Tenant != "C" || math.Abs(got-12) > 1e-9 || c.Shed != 10 {
		t.Errorf("C's window: %v arrivals (%+v), want 12 with 10 shed", got, c)
	}
}

// TestPolicyVersioningAndLatency runs on a manual clock, so the
// propagation latency (receive time minus IssuedAt) is exact.
func TestPolicyVersioningAndLatency(t *testing.T) {
	clk := newTestClock()
	g, base := startGatewayAt(t, Config{Discovery: &testDiscovery{}}, clk.Now)
	var initial policy.Policy
	if getJSON(t, base+"/policy", &initial); initial.Version != 0 || initial.Tenants["A"].Weight != 1 {
		t.Fatalf("initial policy %+v, want the version-0 equal-weight policy", initial)
	}
	v5 := policy.Policy{Version: 5, IssuedAt: clk.Now().Add(-250 * time.Millisecond),
		Tenants: map[string]policy.TenantPolicy{"A": {Weight: 3}, "B": {Weight: 1}}}
	status, pr := postPolicy(t, base, v5)
	if status != http.StatusOK || !pr.Applied || pr.Version != 5 {
		t.Fatalf("v5: %d %+v", status, pr)
	}
	if pr.LatencyMs != 250 {
		t.Errorf("propagation latency %v ms, want 250 ms", pr.LatencyMs)
	}
	const latCount, latSum = "tenantscaler_gateway_policy_propagation_seconds_count", "tenantscaler_gateway_policy_propagation_seconds_sum"
	if n, sum := metricValue(t, base, latCount), metricValue(t, base, latSum); n != 1 || sum != 0.25 {
		t.Errorf("propagation histogram: %v samples summing to %v s, want 1 of 0.25 s", n, sum)
	}
	status, pr = postPolicy(t, base, policy.Policy{Version: 3, Tenants: map[string]policy.TenantPolicy{"A": {Weight: 9}}})
	if status != http.StatusOK || pr.Applied || pr.Version != 5 {
		t.Errorf("older v3: %d %+v, want 200 with applied false and version 5", status, pr)
	}
	var cur policy.Policy
	if getJSON(t, base+"/policy", &cur); cur.Version != 5 || cur.Tenants["A"].Weight != 3 {
		t.Errorf("GET /policy = %+v, want v5", cur)
	}
	st := g.Stats(0)
	if st.PolicyVersion != 5 || st.PolicyLatencyMs != 250 {
		t.Errorf("stats policy version %d latency %v ms, want 5 and 250", st.PolicyVersion, st.PolicyLatencyMs)
	}
	if a := tenantState(t, st, "A"); a.Weight != 3 {
		t.Errorf("scheduler weight of A = %v, want 3", a.Weight)
	}
	if g.SetPolicy(policy.Policy{Version: 4}) {
		t.Error("SetPolicy accepted an older version")
	}
	if !g.SetPolicy(v5) {
		t.Error("SetPolicy rejected a resend of the current version")
	}
	if v := metricValue(t, base, "tenantscaler_gateway_policy_version"); v != 5 {
		t.Errorf("policy_version gauge = %v, want 5", v)
	}
	// Without IssuedAt the latency is unknown: reported as 0, not observed.
	if !g.SetPolicy(policy.Policy{Version: 6, Tenants: map[string]policy.TenantPolicy{"A": {Weight: 1}}}) {
		t.Fatal("v6 not applied")
	}
	if st := g.Stats(0); st.PolicyVersion != 6 || st.PolicyLatencyMs != 0 {
		t.Errorf("v6 without IssuedAt: version %d latency %v ms, want 6 and 0", st.PolicyVersion, st.PolicyLatencyMs)
	}
	if n := metricValue(t, base, latCount); n != 2 {
		t.Errorf("propagation histogram has %v samples, want 2 (v5 twice; v6 has no IssuedAt)", n)
	}

	for name, bad := range map[string]any{
		"zero version":    policy.Policy{Version: 0},
		"negative weight": policy.Policy{Version: 9, Tenants: map[string]policy.TenantPolicy{"A": {Weight: -1}}},
		"unknown field":   map[string]any{"version": 9, "tenantz": map[string]any{}},
	} {
		if status, _ := postPolicy(t, base, bad); status != http.StatusBadRequest {
			t.Errorf("%s: status %d, want 400", name, status)
		}
	}
}

func TestBadRequests(t *testing.T) {
	up := newFakeUpstream(t, nil)
	_, base := startGateway(t, Config{Discovery: StaticDiscovery{up.URL()}})
	good := completionBody("w", 2, true)
	for _, tt := range []struct {
		name, tenant, priority, body string
	}{
		{"unknown tenant", "Z", "", good},
		{"missing tenant", "", "", good},
		{"bad priority", "A", "urgent", good},
		{"malformed JSON", "A", "", "{"},
		{"missing prompt", "A", "", `{"max_tokens": 2}`},
		{"non-string prompt", "A", "", `{"prompt": 7}`},
		{"zero max_tokens", "A", "", `{"prompt": "w", "max_tokens": 0}`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			r := post(t, base, tt.tenant, tt.priority, tt.body)
			if r.status != http.StatusBadRequest {
				t.Fatalf("status %d, want 400: %s", r.status, r.body)
			}
			if e := errorBody(t, r.body); e.Type != "invalid_request_error" || e.Message == "" {
				t.Errorf("error body %+v", e)
			}
		})
	}
	req, _ := http.NewRequest(http.MethodPost, base+"/v1/completions", strings.NewReader(good))
	req.Header.Set(TenantHeaderAlias, "B")
	req.Header.Set(PriorityHeader, "2")
	resp, err := testClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("alias tenant header with integer priority: status %d, want 200", resp.StatusCode)
	}
	if _, _, tenants := up.stats(); len(tenants) != 1 {
		t.Errorf("upstream saw %d requests, want only the valid one", len(tenants))
	}
}

// TestUpstreamFailures: refusals are retried on another replica; other
// upstream failures are answered 502 (or cut the stream off) and count as
// failed requests and SLO misses, so attainment does not hide them.
func TestUpstreamFailures(t *testing.T) {
	dead := httptest.NewServer(http.NotFoundHandler())
	dead.Close() // connection refused from now on
	broken := newFakeUpstream(t, func(f *fakeUpstream) { f.status = http.StatusInternalServerError })
	draining := newFakeUpstream(t, func(f *fakeUpstream) { f.status = http.StatusServiceUnavailable })
	crashing := newFakeUpstream(t, func(f *fakeUpstream) { f.breakAfter = 1 })
	healthy := newFakeUpstream(t, nil)
	for _, tt := range []struct {
		name      string
		endpoints []string
		status    int
		failed    bool
	}{
		{"connection refused", []string{dead.URL}, http.StatusBadGateway, true},
		{"replica error", []string{broken.URL()}, http.StatusBadGateway, true},
		{"only a draining replica", []string{draining.URL()}, http.StatusBadGateway, true},
		{"stream broken off", []string{crashing.URL()}, http.StatusOK, true},
		{"refused, then retried elsewhere", []string{dead.URL, healthy.URL()}, http.StatusOK, false},
		{"draining, then retried elsewhere", []string{draining.URL(), healthy.URL()}, http.StatusOK, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			g, base := startGateway(t, Config{SlotsPerReplica: 1, Discovery: StaticDiscovery(tt.endpoints)})
			r := post(t, base, "A", "", completionBody("w", 2, true))
			if r.status != tt.status {
				t.Fatalf("status %d, want %d: %s", r.status, tt.status, r.body)
			}
			if tt.status == http.StatusBadGateway {
				if e := errorBody(t, r.body); e.Type != "upstream_error" || e.Tenant != "A" {
					t.Errorf("error body %+v", e)
				}
			}
			if tt.failed == r.done {
				t.Errorf("stream ended with [DONE] = %v, want %v", r.done, !tt.failed)
			}
			want, slo := 0, sloMet
			if tt.failed {
				want, slo = 1, sloMissed
			}
			if v := metricValue(t, base, `tenantscaler_gateway_failed_total{reason="upstream",tenant="A"}`); v != float64(want) {
				t.Errorf("upstream failures = %v, want %v", v, want)
			}
			if v := metricValue(t, base, `tenantscaler_gateway_completed_total{slo="`+slo+`",tenant="A"}`); v != 1 {
				t.Errorf("completed{slo=%q} = %v, want 1", slo, v)
			}
			st := g.Stats(time.Minute)
			if a := st.Tenants[0]; a.Completed != 1 || st.Failed["A"] != want || a.SLOAttainment != float64(1-want) {
				t.Errorf("window: %+v with %d failed, want 1 completed, %d failed", a, st.Failed["A"], want)
			}
			// The failed or retried request must not leak its slot.
			if st.InFlight != 0 || st.Queued != 0 || slotsHeld(g) != 0 {
				t.Errorf("after the request: in flight %d, queued %d, slots held %d, want 0", st.InFlight, st.Queued, slotsHeld(g))
			}
		})
	}
}

func TestMetricsExposeContractNames(t *testing.T) {
	_, base := startGateway(t, Config{Discovery: &testDiscovery{}})
	text := scrape(t, base)
	for _, name := range []string{
		"tenantscaler_gateway_arrivals_total", "tenantscaler_gateway_arrival_weighted_tokens_total",
		"tenantscaler_gateway_shed_total", "tenantscaler_gateway_completed_total",
		"tenantscaler_gateway_ttft_seconds_bucket", "tenantscaler_gateway_tpot_seconds_bucket",
		"tenantscaler_gateway_queue_length", "tenantscaler_gateway_inflight",
		"tenantscaler_gateway_policy_version", "tenantscaler_gateway_policy_propagation_seconds_bucket",
		"tenantscaler_gateway_endpoints", "tenantscaler_gateway_failed_total",
		"tenantscaler_gateway_output_tokens_total",
	} {
		if !strings.Contains(text, "\n"+name) {
			t.Errorf("/metrics has no %s", name)
		}
	}
	if !strings.Contains(text, `tenantscaler_gateway_arrivals_total{priority="sheddable",tenant="B"} 0`) {
		t.Error("per-tenant series are not initialised at zero")
	}
}

// TestStatsEndpoint runs on a manual clock started 50 s before the request,
// so the window math is exact: rates are per second of the window, which is
// capped at the gateway's uptime and at MaxStatsWindow.
func TestStatsEndpoint(t *testing.T) {
	clk := newTestClock()
	up := newFakeUpstream(t, nil)
	_, base := startGatewayAt(t, Config{Discovery: StaticDiscovery{up.URL()}}, clk.Now)
	clk.advance(50 * time.Second)
	if r := post(t, base, "A", "", completionBody("one two three", 5, true)); r.status != http.StatusOK {
		t.Fatalf("status %d", r.status)
	}
	for _, tt := range []struct {
		query  string
		window float64
	}{
		{"", DefaultStatsWindow.Seconds()},
		{"?window=30s", 30},
		{"?window=10", 10},
		{"?window=1h", 50}, // the gateway is only 50 s old
	} {
		var st Stats
		if s := getJSON(t, base+"/stats"+tt.query, &st); s != http.StatusOK {
			t.Fatalf("GET /stats%s: %d", tt.query, s)
		}
		if st.Mode != ModeFair || st.WindowSeconds != tt.window || st.Endpoints != 1 || st.Slots != 2 || len(st.Tenants) != 2 {
			t.Fatalf("/stats%s: %+v", tt.query, st)
		}
		// in = 3 tokens, out = 5: weighted 3 + 2*5 = 13.
		a := st.Tenants[0]
		if a.Tenant != "A" || a.Completed != 1 || !a.HasSLOData || a.SLOAttainment != 1 || st.Failed["A"] != 0 ||
			a.ArrivalRequestsPerSec != 1/tt.window || a.ArrivalWeightedTokensPerSec != 13/tt.window {
			t.Errorf("/stats%s: tenant A %+v", tt.query, a)
		}
	}
	clk.advance(10 * time.Minute)
	var st Stats
	if getJSON(t, base+"/stats?window=1h", &st); st.WindowSeconds != MaxStatsWindow.Seconds() || st.Tenants[0].Completed != 0 {
		t.Errorf("window=1h after 10 min: %v s with %d completed, want the cap and none", st.WindowSeconds, st.Tenants[0].Completed)
	}
	for _, bad := range []string{"abc", "-5s", "0"} {
		if s := getJSON(t, base+"/stats?window="+bad, nil); s != http.StatusBadRequest {
			t.Errorf("window=%s: status %d, want 400", bad, s)
		}
	}
	var h map[string]any
	if s := getJSON(t, base+"/healthz", &h); s != http.StatusOK || h["status"] != "ok" {
		t.Errorf("GET /healthz: %d %v", s, h)
	}
}

func TestUpstreamAPIKey(t *testing.T) {
	up := newFakeUpstream(t, nil)
	_, base := startGateway(t, Config{Discovery: StaticDiscovery{up.URL()}, UpstreamAPIKey: "sk-test"})
	if r := post(t, base, "A", "", completionBody("w", 1, false)); r.status != http.StatusOK {
		t.Fatalf("status %d", r.status)
	}
	up.mu.Lock()
	defer up.mu.Unlock()
	if len(up.auth) != 1 || up.auth[0] != "Bearer sk-test" {
		t.Errorf("upstream Authorization %q, want Bearer sk-test", up.auth)
	}
}

func TestNewValidatesConfig(t *testing.T) {
	disc := StaticDiscovery{}
	for name, cfg := range map[string]Config{
		"bad mode":        {Mode: "fifo", SlotsPerReplica: 1, Discovery: disc, Tenants: testTenants("A")},
		"zero slots":      {SlotsPerReplica: 0, Discovery: disc, Tenants: testTenants("A")},
		"no discovery":    {SlotsPerReplica: 1, Tenants: testTenants("A")},
		"no tenants":      {SlotsPerReplica: 1, Discovery: disc},
		"duplicate":       {SlotsPerReplica: 1, Discovery: disc, Tenants: testTenants("A", "A")},
		"negative share":  {SlotsPerReplica: 1, Discovery: disc, Tenants: testTenants("A"), PartitionShares: map[string]int{"A": -1}},
		"negative window": {SlotsPerReplica: 1, Discovery: disc, Tenants: testTenants("A"), StatsWindow: -time.Second},
	} {
		if g, err := New(cfg); err == nil {
			g.Close()
			t.Errorf("%s: New succeeded, want an error", name)
		}
	}
}
