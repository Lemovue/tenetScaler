package gateway

import (
	"bufio"
	"context"
	"net/http"
	"strings"
	"testing"
	"time"
)

// TestCloseAnswersQueuedAndCancelsInFlight: Close answers a queued request
// and an in-flight one 503, cancels the upstream call, frees every slot,
// and counts both as shutdown failures, not SLO outcomes.
func TestCloseAnswersQueuedAndCancelsInFlight(t *testing.T) {
	up := newFakeUpstream(t, func(f *fakeUpstream) { f.release = make(chan struct{}) })
	g, base := startGateway(t, Config{SlotsPerReplica: 1, Discovery: StaticDiscovery{up.URL()}})
	results := make(chan result, 2)
	go func() { results <- post(t, base, "A", "", completionBody("w", 2, true)) }()
	waitFor(t, "A's request at the replica", 5*time.Second, func() bool { active, _, _ := up.stats(); return active == 1 })
	go func() { results <- post(t, base, "B", "", completionBody("w", 2, true)) }()
	waitFor(t, "B's request to queue", 5*time.Second, func() bool { return g.Stats(0).Queued == 1 })

	g.Close()
	for range 2 {
		if r := <-results; r.status != http.StatusServiceUnavailable || errorBody(t, r.body).Type != "unavailable" {
			t.Errorf("status %d, want 503 unavailable: %s", r.status, r.body)
		}
	}
	for _, tenant := range []string{"A", "B"} {
		if v := metricValue(t, base, `tenantscaler_gateway_failed_total{reason="shutdown",tenant="`+tenant+`"}`); v != 1 {
			t.Errorf("%s: shutdown failures = %v, want 1", tenant, v)
		}
		if v := metricValue(t, base, `tenantscaler_gateway_completed_total{slo="missed",tenant="`+tenant+`"}`); v != 0 {
			t.Errorf("%s: missed completions = %v, want 0", tenant, v)
		}
	}
	waitFor(t, "the upstream call to be canceled", 5*time.Second, func() bool { active, _, _ := up.stats(); return active == 0 })
	if st := g.Stats(0); st.InFlight != 0 || st.Queued != 0 || slotsHeld(g) != 0 {
		t.Errorf("after Close: in flight %d, queued %d, slots held %d, want 0", st.InFlight, st.Queued, slotsHeld(g))
	}
	if r := post(t, base, "A", "", completionBody("w", 2, true)); r.status != http.StatusServiceUnavailable {
		t.Errorf("request after Close: status %d, want 503", r.status)
	}
}

// TestClientDisconnectMidStream: a client that goes away mid-stream frees
// its slot, is counted as canceled (not as an SLO outcome), and its tenant
// is charged the tokens produced, not the estimate charged at dispatch.
func TestClientDisconnectMidStream(t *testing.T) {
	up := newFakeUpstream(t, func(f *fakeUpstream) { f.tokenDelay = 20 * time.Millisecond })
	g, base := startGateway(t, Config{SlotsPerReplica: 1, Discovery: StaticDiscovery{up.URL()}})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	const maxTokens = 50
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/v1/completions",
		strings.NewReader(completionBody("w w", maxTokens, true)))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set(TenantHeader, "A")
	resp, err := testClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	line, err := bufio.NewReader(resp.Body).ReadString('\n')
	if err != nil || !strings.HasPrefix(line, "data: {") {
		t.Fatalf("first line %q, %v: want a token chunk", line, err)
	}
	cancel()
	resp.Body.Close()

	waitFor(t, "the cancellation to be counted", 5*time.Second, func() bool {
		return metricValue(t, base, `tenantscaler_gateway_failed_total{reason="canceled",tenant="A"}`) == 1
	})
	if st := g.Stats(0); st.InFlight != 0 || slotsHeld(g) != 0 {
		t.Errorf("after the disconnect: in flight %d, slots held %d, want 0", st.InFlight, slotsHeld(g))
	}
	for _, slo := range []string{sloMet, sloMissed} {
		if v := metricValue(t, base, `tenantscaler_gateway_completed_total{slo="`+slo+`",tenant="A"}`); v != 0 {
			t.Errorf("completed{slo=%q} = %v, want 0", slo, v)
		}
	}
	// Charged at dispatch: in + 2 x maxTokens; corrected at the end to
	// in + 2 x produced, with at least one token produced.
	const in = 2
	if a := tenantState(t, g.Stats(0), "A"); a.InFlight != 0 || a.Counter < in+2 || a.Counter >= in+2*maxTokens {
		t.Errorf("A's scheduler state %+v, want the counter in [%d, %d)", a, in+2, in+2*maxTokens)
	}
}
