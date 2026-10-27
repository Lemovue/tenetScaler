package demo

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"tenantscaler/internal/control"
	"tenantscaler/internal/model"
	"tenantscaler/internal/sim"
)

func TestHealthz(t *testing.T) {
	_, ts := newTestServer(t, testOptions(t))
	var got map[string]string
	getJSON(t, ts.URL+"/healthz", &got)
	if got["status"] != "ok" {
		t.Fatalf("healthz %v", got)
	}
}

// TestABRun runs a whole A/B comparison over HTTP: start, a concurrent
// start answered 409, live state while it runs, both stacks finished with
// metrics, and the TenantScaler controller's decisions afterwards.
func TestABRun(t *testing.T) {
	opt := testOptions(t)
	_, ts := newTestServer(t, opt)

	var idle State
	getJSON(t, ts.URL+"/api/state", &idle)
	if idle.Phase != PhaseIdle || len(idle.Stacks) != 2 || idle.Stacks[0].Status != StatusPending || !idle.Backend.Simulated {
		t.Fatalf("idle state %+v", idle)
	}
	var none DecisionsReply
	getJSON(t, ts.URL+"/api/decisions", &none)
	if len(none.Records) != 0 || none.Live {
		t.Fatalf("decisions before any run: %+v", none)
	}

	status, _, body := do(t, http.MethodPost, ts.URL+"/api/ab/start", nil, nil)
	if status != http.StatusAccepted {
		t.Fatalf("start: %d %s", status, body)
	}
	var started map[string]int64
	if err := json.Unmarshal(body, &started); err != nil || started["run"] != 1 || started["seed"] != opt.Seed {
		t.Fatalf("start reply %s (%v)", body, err)
	}
	if status, _, body := do(t, http.MethodPost, ts.URL+"/api/ab/start", nil, nil); status != http.StatusConflict {
		t.Fatalf("concurrent start: %d %s, want 409", status, body)
	}

	running := waitState(t, ts.URL, 30*time.Second, func(st State) bool {
		return st.Stacks[0].Status == StatusRunning && len(st.Stacks[0].Samples) > 0
	})
	if running.Phase != PhaseRunning || running.Stacks[1].Status != StatusPending || running.Progress < 0 || running.Progress > 0.5 {
		t.Fatalf("while the baseline runs: phase %q progress %v stacks %+v", running.Phase, running.Progress, running.Stacks)
	}
	if running.Stacks[0].GatewayURL == "" || running.Stacks[1].GatewayURL != "" {
		t.Errorf("gateway URLs while the baseline runs: %q and %q", running.Stacks[0].GatewayURL, running.Stacks[1].GatewayURL)
	}

	// One stack at a time: when TenantScaler replays traffic the baseline
	// has finished and its gateway is gone; the controller's decisions are
	// served live.
	tsRunning := waitState(t, ts.URL, 60*time.Second, func(st State) bool {
		return st.Stacks[1].Status == StatusRunning || st.Phase != PhaseRunning
	})
	if tsRunning.Stacks[1].Status != StatusRunning || tsRunning.Stacks[0].Status != StatusDone ||
		tsRunning.Stacks[0].GatewayURL != "" || tsRunning.Stacks[1].GatewayURL == "" || tsRunning.Stacks[1].TrafficStartedAt == nil {
		t.Fatalf("while TenantScaler runs: phase %q stacks %+v", tsRunning.Phase, tsRunning.Stacks)
	}
	live := waitDecisions(t, ts.URL, 10*time.Second)
	if !live.Live || live.Setup != sim.SetupFull || len(live.Records) == 0 || live.TrafficStartedAt == nil ||
		!live.TrafficStartedAt.Equal(*tsRunning.Stacks[1].TrafficStartedAt) {
		t.Fatalf("decisions while TenantScaler runs: live %v setup %q %d records, traffic start %v (stack %v)",
			live.Live, live.Setup, len(live.Records), live.TrafficStartedAt, tsRunning.Stacks[1].TrafficStartedAt)
	}

	st := waitState(t, ts.URL, 60*time.Second, func(st State) bool { return st.Phase != PhaseRunning })
	if st.Phase != PhaseDone || st.Progress != 1 || st.Error != "" {
		t.Fatalf("final phase %q progress %v error %q", st.Phase, st.Progress, st.Error)
	}
	wantSetups := []string{sim.SetupKEDAQueue, sim.SetupFull}
	for i, s := range st.Stacks {
		if s.Setup != wantSetups[i] || s.Status != StatusDone || s.Metrics == nil || !s.Simulated {
			t.Fatalf("stack %d: setup %q status %q metrics %v simulated %v", i, s.Setup, s.Status, s.Metrics, s.Simulated)
		}
		if s.Requests == 0 || s.Metrics.Completed+s.Metrics.Shed+s.Metrics.Failed != s.Requests {
			t.Errorf("%s: %d requests, metrics count %d completed %d shed %d failed", s.Setup, s.Requests,
				s.Metrics.Completed, s.Metrics.Shed, s.Metrics.Failed)
		}
		if s.Metrics.Completed == 0 || s.Metrics.ReplicaMinutes <= 0 {
			t.Errorf("%s: metrics %+v", s.Setup, *s.Metrics)
		}
		if len(s.Tenants) != 3 || s.Tenants[0].Tenant != "A" || s.Tenants[0].Requests == 0 {
			t.Errorf("%s: tenant results %+v", s.Setup, s.Tenants)
		}
		if len(s.Samples) < 3 || s.Engine == nil || !s.Engine.Simulated || s.TrafficStartedAt == nil {
			t.Fatalf("%s: %d samples, engine %+v", s.Setup, len(s.Samples), s.Engine)
		}
		last := s.Samples[len(s.Samples)-1]
		if last.Ready < 1 || len(last.Tenants) != 3 {
			t.Errorf("%s: last sample %+v", s.Setup, last)
		}
		if s.GatewayURL != "" {
			t.Errorf("%s: gateway URL %q after the stack stopped", s.Setup, s.GatewayURL)
		}
	}
	// Identical traffic, one stack after the other (decision Q7).
	base, full := st.Stacks[0], st.Stacks[1]
	if base.Requests != full.Requests {
		t.Errorf("the stacks replayed %d and %d requests, want the same trace", base.Requests, full.Requests)
	}
	for k := range base.Tenants {
		if b, f := base.Tenants[k], full.Tenants[k]; b.Tenant != f.Tenant || b.Requests != f.Requests {
			t.Errorf("tenant %d: baseline %s %d requests, TenantScaler %s %d", k, b.Tenant, b.Requests, f.Tenant, f.Requests)
		}
	}
	baseEnd := base.TrafficStartedAt.Add(time.Duration(base.ElapsedSeconds * float64(time.Second)))
	if full.TrafficStartedAt.Before(baseEnd) {
		t.Errorf("TenantScaler's traffic started at %v, before the baseline's replay ended at %v", full.TrafficStartedAt, baseEnd)
	}

	var dec DecisionsReply
	getJSON(t, ts.URL+"/api/decisions?limit=5", &dec)
	if dec.Setup != sim.SetupFull || dec.Live || len(dec.Records) != 5 || dec.Total < 5 || dec.TrafficStartedAt == nil {
		t.Fatalf("decisions after the run: setup %q live %v %d of %d records", dec.Setup, dec.Live, len(dec.Records), dec.Total)
	}
	for i, r := range dec.Records {
		if r.Reason == "" || (i > 0 && r.Cycle > dec.Records[i-1].Cycle) {
			t.Errorf("record %d %+v: want a reason, newest first", i, r)
		}
	}
	var onlyC DecisionsReply
	getJSON(t, ts.URL+"/api/decisions?tenant=C", &onlyC)
	if len(onlyC.Records) == 0 {
		t.Fatal("no decisions for tenant C")
	}
	for _, r := range onlyC.Records {
		if r.Tenant != "C" {
			t.Fatalf("tenant filter C returned %+v", r)
		}
	}
	var pool DecisionsReply
	getJSON(t, ts.URL+"/api/decisions?tenant="+url.QueryEscape(control.PoolRecordTenant), &pool)
	if len(pool.Records) == 0 || pool.Records[0].Tenant != control.PoolRecordTenant || pool.Records[0].Reason == "" {
		t.Errorf("pool decision records %+v", pool.Records)
	}
	for _, bad := range []string{"?tenant=Z", "?limit=0", "?limit=x"} {
		if status, _, body := do(t, http.MethodGet, ts.URL+"/api/decisions"+bad, nil, nil); status != http.StatusBadRequest {
			t.Errorf("decisions%s: %d %s, want 400", bad, status, body)
		}
	}
	matches, err := filepath.Glob(filepath.Join(opt.LogDir, "decisions-*-run1.jsonl"))
	if err != nil || len(matches) != 1 {
		t.Fatalf("decision records file in %s: %v %v", opt.LogDir, matches, err)
	}
	data, err := os.ReadFile(matches[0])
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) < dec.Total {
		t.Errorf("%s holds %d records, the log keeps %d", matches[0], len(lines), dec.Total)
	}
	for i, line := range lines {
		var rec model.DecisionRecord
		if err := json.Unmarshal([]byte(line), &rec); err != nil || rec.Reason == "" {
			t.Fatalf("%s line %d: %v %q", matches[0], i+1, err, line)
		}
	}
	if status, _, body := do(t, http.MethodPost, ts.URL+"/api/ab/stop", nil, nil); status != http.StatusConflict {
		t.Errorf("stop after the run: %d %s, want 409", status, body)
	}
}

// TestStop stops a run while the baseline replays traffic, checks that
// the stack was shut down (its gateway no longer accepts connections) and
// the second stack never started, and that a new run can start afterwards.
func TestStop(t *testing.T) {
	srv, ts := newTestServer(t, testOptions(t))
	if status, _, body := do(t, http.MethodPost, ts.URL+"/api/ab/start", []byte(`{"seed": 11}`), nil); status != http.StatusAccepted {
		t.Fatalf("start: %d %s", status, body)
	}
	running := waitState(t, ts.URL, 30*time.Second, func(st State) bool { return st.Stacks[0].Status == StatusRunning })
	gw := running.Stacks[0].GatewayURL
	if status, err := probe(gw + "/healthz"); err != nil || status != http.StatusOK {
		t.Fatalf("the running baseline's gateway %q: %d %v", gw, status, err)
	}
	status, _, body := do(t, http.MethodPost, ts.URL+"/api/ab/stop", nil, nil)
	if status != http.StatusOK {
		t.Fatalf("stop: %d %s", status, body)
	}
	st := srv.State()
	if st.Phase != PhaseStopped || st.Seed != 11 || st.Stacks[0].Status != StatusStopped || st.Stacks[1].Status != StatusSkipped {
		t.Fatalf("after stop: phase %q seed %d stacks %q %q", st.Phase, st.Seed, st.Stacks[0].Status, st.Stacks[1].Status)
	}
	if st.Error != "" || st.Stacks[0].Error != "" || st.Stacks[0].GatewayURL != "" || !strings.Contains(st.Message, "every replica") {
		t.Errorf("after a clean stop: error %q, stack error %q, gateway %q, message %q",
			st.Error, st.Stacks[0].Error, st.Stacks[0].GatewayURL, st.Message)
	}
	if status, err := probe(gw + "/healthz"); err == nil {
		t.Errorf("the baseline's gateway %s still answers (%d) after the stop returned", gw, status)
	}
	if st.Stacks[0].Metrics != nil {
		t.Errorf("a stopped stack has metrics %+v", st.Stacks[0].Metrics)
	}
	if status, _, _ := do(t, http.MethodPost, ts.URL+"/api/ab/stop", nil, nil); status != http.StatusConflict {
		t.Errorf("second stop: %d, want 409", status)
	}
	if status, _, body := do(t, http.MethodPost, ts.URL+"/api/ab/start", []byte(`{"seed":"x"}`), nil); status != http.StatusBadRequest {
		t.Errorf("start with a bad seed: %d %s, want 400", status, body)
	}
	if status, _, body := do(t, http.MethodPost, ts.URL+"/api/ab/start", nil, nil); status != http.StatusAccepted {
		t.Fatalf("restart after stop: %d %s", status, body)
	}
	if got := srv.State().Run; got != 2 {
		t.Errorf("run counter %d, want 2", got)
	}
	// The cleanup's Close stops the second run.
}

// TestCloseRefusesNewRuns checks that Close stops a run and that the
// server then refuses new ones with 503.
func TestCloseRefusesNewRuns(t *testing.T) {
	srv, ts := newTestServer(t, testOptions(t))
	if _, err := srv.StartAB(1); err != nil {
		t.Fatal(err)
	}
	if err := srv.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	if st := srv.State(); st.Phase != PhaseStopped || st.Error != "" {
		t.Fatalf("after Close: phase %q error %q", st.Phase, st.Error)
	}
	if status, _, body := do(t, http.MethodPost, ts.URL+"/api/ab/start", nil, nil); status != http.StatusServiceUnavailable {
		t.Fatalf("start after Close: %d %s, want 503", status, body)
	}
}

// TestGuardFailure checks that a failing machine-safety guard ends the run
// in the error phase before any replica starts.
func TestGuardFailure(t *testing.T) {
	opt := testOptions(t)
	opt.Guard = func(context.Context) error { return errors.New("RAM above the limit") }
	srv, ts := newTestServer(t, opt)
	if _, err := srv.StartAB(1); err != nil {
		t.Fatal(err)
	}
	st := waitState(t, ts.URL, 10*time.Second, func(st State) bool { return st.Phase != PhaseRunning })
	if st.Phase != PhaseError || !strings.Contains(st.Error, "RAM above the limit") || !strings.Contains(st.Error, "guard") {
		t.Fatalf("phase %q error %q", st.Phase, st.Error)
	}
	if st.Stacks[0].Status != StatusFailed || st.Stacks[0].Error == "" || st.Stacks[1].Status != StatusSkipped {
		t.Fatalf("stacks %q (%q) and %q", st.Stacks[0].Status, st.Stacks[0].Error, st.Stacks[1].Status)
	}
}

func TestCrossOriginPostRejected(t *testing.T) {
	_, ts := newTestServer(t, testOptions(t))
	hdr := map[string]string{"Origin": "http://evil.example", "Sec-Fetch-Site": "cross-site"}
	for _, path := range []string{"/api/ab/start", "/api/ab/stop"} {
		if status, _, body := do(t, http.MethodPost, ts.URL+path, nil, hdr); status != http.StatusForbidden {
			t.Errorf("cross-site POST %s: %d %s, want 403", path, status, body)
		}
	}
	if status, _, _ := do(t, http.MethodGet, ts.URL+"/api/state", nil, hdr); status != http.StatusOK {
		t.Errorf("cross-site GET /api/state: %d, want 200 (safe method)", status)
	}
}

func TestStaticFiles(t *testing.T) {
	_, ts := newTestServer(t, testOptions(t))
	cases := []struct{ path, ctype, contains string }{
		{"/", "text/html; charset=utf-8", `<script src="app.js" defer></script>`},
		{"/index.html", "text/html; charset=utf-8", "<title>"},
		{"/app.js", "text/javascript; charset=utf-8", "/api/state"},
		{"/chart.js", "text/javascript; charset=utf-8", "getContext"},
		{"/decisions.js", "text/javascript; charset=utf-8", "/api/decisions"},
		{"/results.js", "text/javascript; charset=utf-8", "/api/results"},
		{"/util.js", "text/javascript; charset=utf-8", "textContent"},
		{"/summary.js", "text/javascript; charset=utf-8", "VictimViolationPct"},
		{"/style.css", "text/css; charset=utf-8", "prefers-color-scheme: dark"},
	}
	for _, c := range cases {
		status, hdr, body := do(t, http.MethodGet, ts.URL+c.path, nil, nil)
		if status != http.StatusOK {
			t.Errorf("GET %s: %d", c.path, status)
			continue
		}
		if got := hdr.Get("Content-Type"); got != c.ctype {
			t.Errorf("GET %s: Content-Type %q, want %q", c.path, got, c.ctype)
		}
		if !strings.Contains(string(body), c.contains) {
			t.Errorf("GET %s: body lacks %q", c.path, c.contains)
		}
		if hdr.Get("X-Content-Type-Options") != "nosniff" || !strings.Contains(hdr.Get("Content-Security-Policy"), "default-src 'self'") {
			t.Errorf("GET %s: security headers %v", c.path, hdr)
		}
	}
	if status, _, _ := do(t, http.MethodGet, ts.URL+"/missing.js", nil, nil); status != http.StatusNotFound {
		t.Errorf("missing file: %d, want 404", status)
	}
	status, hdr, _ := do(t, http.MethodGet, ts.URL+"/api/nothing", nil, nil)
	if status != http.StatusNotFound || !strings.HasPrefix(hdr.Get("Content-Type"), "application/json") {
		t.Errorf("unknown API path: %d %q, want a 404 JSON error", status, hdr.Get("Content-Type"))
	}
	for _, path := range []string{"/api/ab/start", "/api/ab/stop"} {
		status, hdr, body := do(t, http.MethodGet, ts.URL+path, nil, nil)
		if status != http.StatusMethodNotAllowed || hdr.Get("Allow") != http.MethodPost ||
			!strings.HasPrefix(hdr.Get("Content-Type"), "application/json") || !strings.Contains(string(body), "POST") {
			t.Errorf("GET %s: %d Allow %q %s, want a 405 JSON error with Allow: POST", path, status, hdr.Get("Allow"), body)
		}
	}
}

func TestConfigEndpoint(t *testing.T) {
	_, ts := newTestServer(t, testOptions(t))
	var c ConfigReply
	getJSON(t, ts.URL+"/api/config", &c)
	if !c.Backend.Simulated || c.Backend.Label != "SIMULATED engine" || c.CapacityReplicas != 3 || c.InitialReplicas != 2 {
		t.Fatalf("config %+v", c)
	}
	if len(c.Stacks) != 2 || c.Stacks[0].Setup != sim.SetupKEDAQueue || c.Stacks[1].Setup != sim.SetupFull || c.Stacks[1].Gateway != "fair" {
		t.Fatalf("stacks %+v", c.Stacks)
	}
	if c.Scenario.Name != ScenarioTiny || c.Scenario.Noisy != "C" || len(c.Scenario.Tenants) != 3 || c.Scenario.Tenants[0].TTFTSeconds != 2 {
		t.Fatalf("scenario %+v", c.Scenario)
	}
	if c.WindowSeconds != DefaultWindow.Seconds() || c.PoolRecordTenant == "" {
		t.Errorf("window %v s", c.WindowSeconds)
	}
}
