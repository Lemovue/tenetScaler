package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"tenantscaler/internal/engine"
	"tenantscaler/internal/model"
	"tenantscaler/internal/pool"
	"tenantscaler/internal/replica"
)

// tinyEngine is a fast simulated engine: ready at once, 1 ms per token.
func tinyEngine() engine.Params {
	return engine.Params{
		PrefillOverhead: time.Millisecond, InterTokenLatency: time.Millisecond,
		MaxNumSeqs: 2, TimeFactorUnderLoad: 1,
	}
}

// TestEndToEndThroughSimulatedPool runs the gateway against an in-process
// pool of simulated replicas found through the pool's HTTP API.
func TestEndToEndThroughSimulatedPool(t *testing.T) {
	p := pool.New(pool.Config{Name: "gw-test", Params: tinyEngine(), MaxReplicas: 2, Seed: 1})
	t.Cleanup(func() {
		if err := p.Close(); err != nil {
			t.Errorf("close pool: %v", err)
		}
	})
	if err := p.Scale(2); err != nil {
		t.Fatal(err)
	}
	api := httptest.NewServer(p.Handler())
	t.Cleanup(api.Close)
	disc := NewPollingDiscovery(pool.Client{BaseURL: api.URL}.Endpoints, 20*time.Millisecond)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	disc.Start(ctx)
	waitFor(t, "two ready replicas", 5*time.Second, func() bool { return len(disc.Endpoints()) == 2 })

	g, base := startGateway(t, Config{Discovery: disc, SlotsPerReplica: 2})
	r := post(t, base, "A", "standard", completionBody("one two three four", 8, true))
	if r.status != http.StatusOK || r.tokens != 8 || !r.done {
		t.Fatalf("stream: status %d tokens %d done %v:\n%s", r.status, r.tokens, r.done, r.body)
	}
	if r.header.Get(replica.SimulatedHeader) != "true" || !strings.HasPrefix(r.header.Get("Content-Type"), "text/event-stream") {
		t.Errorf("response headers %v: want the replica's SSE content type and %s: true", r.header, replica.SimulatedHeader)
	}
	if !strings.Contains(r.body, `"completion_tokens":8`) {
		t.Errorf("final chunk with usage not relayed:\n%s", r.body)
	}

	nr := post(t, base, "B", "", completionBody("x", 5, false))
	var resp replica.CompletionResponse
	if nr.status != http.StatusOK || json.Unmarshal([]byte(nr.body), &resp) != nil || resp.Usage == nil || resp.Usage.CompletionTokens != 5 {
		t.Fatalf("non-stream: status %d body %s", nr.status, nr.body)
	}

	for series, want := range map[string]float64{
		`tenantscaler_gateway_output_tokens_total{tenant="A"}`:           8,
		`tenantscaler_gateway_output_tokens_total{tenant="B"}`:           5,
		`tenantscaler_gateway_completed_total{slo="met",tenant="A"}`:     1,
		`tenantscaler_gateway_completed_total{slo="met",tenant="B"}`:     1,
		`tenantscaler_gateway_arrival_weighted_tokens_total{tenant="A"}`: 4 + 2*8,
		`tenantscaler_gateway_endpoints`:                                 2,
	} {
		if v := metricValue(t, base, series); v != want {
			t.Errorf("%s = %v, want %v", series, v, want)
		}
	}
	st := g.Stats(time.Minute)
	if st.Endpoints != 2 || st.Tenants[0].Completed != 1 || st.Tenants[1].Completed != 1 || st.InFlight != 0 {
		t.Errorf("stats %+v", st)
	}
	// In fair mode the counter is charged the actual output: (4 + 2*8)/1.
	for _, s := range st.Scheduler {
		if s.Tenant == "A" && s.Counter != 20 {
			t.Errorf("A's virtual counter = %v, want 20", s.Counter)
		}
	}
}

// llamaScript is a llama-server-style stream: text pieces of varying size,
// an empty-text chunk, and a final chunk with empty text and usage.
const llamaScript = `data: {"choices":[{"text":"Hel","index":0,"finish_reason":null}],"object":"text_completion"}

data: {"choices":[{"text":"","index":0,"finish_reason":null}],"object":"text_completion"}

data: {"choices":[{"text":"lo","index":0,"finish_reason":null}],"object":"text_completion"}

data: {"choices":[{"text":" world","index":0,"finish_reason":null}],"object":"text_completion"}

data: {"choices":[{"text":"","index":0,"finish_reason":"length"}],"object":"text_completion"USAGE}

data: [DONE]

`

func TestTokenCounting(t *testing.T) {
	for _, tt := range []struct {
		name   string
		usage  string
		stream bool
		want   float64
	}{
		{"prefers usage.completion_tokens", `,"usage":{"prompt_tokens":2,"completion_tokens":7,"total_tokens":9}`, true, 7},
		{"falls back to non-empty text chunks", "", true, 3},
	} {
		t.Run(tt.name, func(t *testing.T) {
			script := strings.Replace(llamaScript, "USAGE", tt.usage, 1)
			up := newFakeUpstream(t, func(f *fakeUpstream) { f.script = script })
			_, base := startGateway(t, Config{Discovery: StaticDiscovery{up.URL()}})
			r := post(t, base, "A", "", completionBody("hi", 16, true))
			if r.status != http.StatusOK {
				t.Fatalf("status %d: %s", r.status, r.body)
			}
			if r.body != script {
				t.Errorf("stream not relayed unchanged:\n got %q\nwant %q", r.body, script)
			}
			if r.header.Get("X-Fake-Upstream") != "yes" {
				t.Error("upstream response headers not relayed")
			}
			if v := metricValue(t, base, `tenantscaler_gateway_output_tokens_total{tenant="A"}`); v != tt.want {
				t.Errorf("output tokens = %v, want %v", v, tt.want)
			}
		})
	}
	t.Run("non-streamed usage", func(t *testing.T) {
		up := newFakeUpstream(t, nil)
		_, base := startGateway(t, Config{Discovery: StaticDiscovery{up.URL()}})
		if r := post(t, base, "A", "", completionBody("hi", 6, false)); r.status != http.StatusOK {
			t.Fatalf("status %d", r.status)
		}
		if v := metricValue(t, base, `tenantscaler_gateway_output_tokens_total{tenant="A"}`); v != 6 {
			t.Errorf("output tokens = %v, want 6", v)
		}
	})
}

// TestTTFTAndTPOTAtGateway checks the gateway's own latency measurement
// and SLO accounting: a 60 ms first token and 10 ms per later token meet a
// 2 s / 100 ms SLO but miss a 20 ms TTFT SLO.
func TestTTFTAndTPOTAtGateway(t *testing.T) {
	up := newFakeUpstream(t, func(f *fakeUpstream) {
		f.firstDelay, f.tokenDelay = 60*time.Millisecond, 10*time.Millisecond
	})
	tenants := testTenants("A", "B")
	tenants[1].SLO = model.SLO{TTFTSeconds: 0.02, TPOTMillis: 100, TargetFraction: 0.99}
	g, base := startGateway(t, Config{Tenants: tenants, Discovery: StaticDiscovery{up.URL()}})
	for _, tenant := range []string{"A", "B"} {
		if r := post(t, base, tenant, "", completionBody("w", 6, true)); r.status != http.StatusOK || r.tokens != 6 {
			t.Fatalf("tenant %s: status %d tokens %d", tenant, r.status, r.tokens)
		}
	}
	st := g.Stats(time.Minute)
	a, b := st.Tenants[0], st.Tenants[1]
	if a.P99TTFTSeconds < 0.06 || a.P99TTFTSeconds > 1 {
		t.Errorf("A's p99 TTFT = %.3f s, want >= 0.06 s", a.P99TTFTSeconds)
	}
	if a.SLOAttainment != 1 || b.SLOAttainment != 0 || !b.HasSLOData {
		t.Errorf("attainment A %v B %v, want 1 and 0", a.SLOAttainment, b.SLOAttainment)
	}
	tpotSum := metricValue(t, base, `tenantscaler_gateway_tpot_seconds_sum{tenant="A"}`)
	if tpotSum < 0.009 || tpotSum > 0.1 {
		t.Errorf("A's TPOT = %.4f s, want ~0.010 s (5 gaps of 10 ms over 5)", tpotSum)
	}
	if v := metricValue(t, base, `tenantscaler_gateway_completed_total{slo="missed",tenant="B"}`); v != 1 {
		t.Errorf("B's missed completions = %v, want 1", v)
	}
}
