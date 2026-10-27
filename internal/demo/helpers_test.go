package demo

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"tenantscaler/internal/config"
	"tenantscaler/internal/engine"
	"tenantscaler/internal/model"
)

// fastConfig is a SIMULATED 3-replica config whose replicas start in 50 ms
// and answer in milliseconds, with a 200 ms control cycle and a tiny
// per-replica capacity, so any traffic makes the controller act.
func fastConfig() config.Config {
	cfg := config.Default()
	cfg.Pool.CapacityReplicas, cfg.Pool.MinReplicas = 3, 1
	slo := model.SLO{TTFTSeconds: 2, TPOTMillis: 100, TargetFraction: 0.99}
	cfg.Tenants = []model.TenantSpec{
		{Name: "A", GuaranteedReplicas: 1, MaxReplicas: 3, Priority: 100, SLO: slo},
		{Name: "B", GuaranteedReplicas: 1, MaxReplicas: 3, Priority: 100, SLO: slo},
		{Name: "C", GuaranteedReplicas: 1, MaxReplicas: 3, Priority: 100, SLO: slo},
	}
	cfg.Controller.CycleSeconds = 0.2
	cfg.Controller.ScaleDownWindowSeconds = 1
	cfg.Controller.ReplicaCapacityTokensPerSec = 1
	cfg.Engine = config.EngineFromParams(engine.Params{
		PrefillOverhead: 2 * time.Millisecond, PrefillPerToken: 50 * time.Microsecond,
		InterTokenLatency: 2 * time.Millisecond, MaxNumSeqs: 2, TimeFactorUnderLoad: 1.5,
		StartupDuration: 50 * time.Millisecond, JitterFraction: 0.05,
	})
	return cfg
}

// testOptions are Options for a fast SIMULATED A/B run of the tiny
// scenario, logging nowhere and keeping files in temporary directories.
func testOptions(t *testing.T) Options {
	t.Helper()
	return Options{
		Config: fastConfig(), Scenario: TinyScenario(), Seed: 7, LogDir: t.TempDir(), ResultsDir: t.TempDir(),
		SampleEvery: 200 * time.Millisecond, DrainTimeout: 20 * time.Second,
		Logger: slog.New(slog.DiscardHandler),
	}
}

// newTestServer starts a demo server over HTTP; both are closed when the
// test ends (Close stops a running A/B run first).
func newTestServer(t *testing.T, opt Options) (*Server, *httptest.Server) {
	t.Helper()
	srv, err := New(opt)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := srv.Close(ctx); err != nil {
			t.Errorf("close: %v", err)
		}
		ts.Close()
	})
	return srv, ts
}

// do sends a request and returns the status, the response headers and
// the body.
func do(t *testing.T, method, url string, body []byte, header map[string]string) (int, http.Header, []byte) {
	t.Helper()
	req, err := http.NewRequest(method, url, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range header {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, resp.Header, data
}

// getJSON GETs url, expects 200 and decodes the body into v.
func getJSON(t *testing.T, url string, v any) {
	t.Helper()
	status, _, body := do(t, http.MethodGet, url, nil, nil)
	if status != http.StatusOK {
		t.Fatalf("GET %s: %d %s", url, status, body)
	}
	if err := json.Unmarshal(body, v); err != nil {
		t.Fatalf("GET %s: decode: %v\n%s", url, err, body)
	}
}

// waitState polls /api/state until ok reports true or the deadline.
func waitState(t *testing.T, base string, timeout time.Duration, ok func(State) bool) State {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		var st State
		getJSON(t, base+"/api/state", &st)
		if ok(st) {
			return st
		}
		if time.Now().After(deadline) {
			t.Fatalf("state not reached after %s: phase %q message %q stacks %+v", timeout, st.Phase, st.Message, st.Stacks)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// probe GETs url on a fresh connection and returns the status; err is set
// when nothing answered (for example the port no longer listens).
func probe(url string) (int, error) {
	client := &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{DisableKeepAlives: true}}
	resp, err := client.Get(url)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode, nil
}

// waitDecisions polls /api/decisions until it holds records or is no
// longer live, or the deadline, and returns the last reply.
func waitDecisions(t *testing.T, base string, timeout time.Duration) DecisionsReply {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		var reply DecisionsReply
		getJSON(t, base+"/api/decisions", &reply)
		if len(reply.Records) > 0 || !reply.Live || time.Now().After(deadline) {
			return reply
		}
		time.Sleep(50 * time.Millisecond)
	}
}
