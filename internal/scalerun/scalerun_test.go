package scalerun

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"tenantscaler/internal/autoscale"
	"tenantscaler/internal/engine"
	"tenantscaler/internal/pool"
	"tenantscaler/internal/replica"
)

var discard = slog.New(slog.DiscardHandler)

// fakePool records Scale calls; Scale updates Desired.
type fakePool struct {
	mu                  sync.Mutex
	st                  pool.Status
	statusErr, scaleErr error
	scales              []int
}

func (f *fakePool) Scale(_ context.Context, n int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.scaleErr != nil {
		return f.scaleErr
	}
	f.scales = append(f.scales, n)
	f.st.Desired = n
	return nil
}

func (f *fakePool) Status(context.Context) (pool.Status, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	st := f.st
	st.Replicas = slices.Clone(f.st.Replicas)
	return st, f.statusErr
}

func (f *fakePool) scaleCalls() []int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.scales)
}

// newFakePool builds a pool status from replicas (Ready/Starting derived).
func newFakePool(desired int, reps ...pool.ReplicaStatus) *fakePool {
	st := pool.Status{Desired: desired, Replicas: reps}
	for _, r := range reps {
		switch r.State {
		case replica.StateReady:
			st.Ready++
		case replica.StateStarting:
			st.Starting++
		}
	}
	return &fakePool{st: st}
}

func ready(running, waiting int) pool.ReplicaStatus {
	return pool.ReplicaStatus{State: replica.StateReady, Running: running, Waiting: waiting}
}

func starting() pool.ReplicaStatus { return pool.ReplicaStatus{State: replica.StateStarting} }

// gatewayStats serves a stats JSON body.
func gatewayStats(t *testing.T, code int, body string) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(code)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv.URL + "/stats"
}

// fakeClock advances 15 s per call unless set explicitly.
type fakeClock struct{ t time.Time }

func (c *fakeClock) now() time.Time { c.t = c.t.Add(15 * time.Second); return c.t }

func kedaRunner(p Pool, statsURL string, clock *fakeClock) *Runner {
	cfg := autoscale.KEDAConfig{TargetPerReplica: 4, Tolerance: 0.1, ScaleDownWindow: 300 * time.Second, Min: 1, Max: 4}
	return &Runner{Algo: AlgoKEDA, KEDA: autoscale.NewKEDA(cfg, 1), Pool: p, GatewayStatsURL: statsURL,
		Clock: clock.now, Logger: discard}
}

func hpaRunner(p Pool) *Runner {
	cfg := autoscale.HPAConfig{Target: 0.5, Tolerance: 0.1, ScaleDownWindow: 300 * time.Second, Min: 1, Max: 8}
	return &Runner{Algo: AlgoHPA, HPA: autoscale.NewHPA(cfg, 1), Pool: p, MaxNumSeqs: 4,
		Clock: (&fakeClock{}).now, Logger: discard}
}

func TestKEDAScalesUpOnReplicaAndGatewayQueue(t *testing.T) {
	p := newFakePool(1, ready(4, 6))
	r := kedaRunner(p, gatewayStats(t, http.StatusOK, `{"queued":6,"inFlight":4}`), &fakeClock{})
	var seen []Decision
	r.OnDecision = func(d Decision) { seen = append(seen, d) }

	d, err := r.RunOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	// queue 6 + 6 = 12, 4 per replica -> 3 replicas.
	if !d.MetricOK || d.Metric != 12 || d.Current != 1 || d.Desired != 3 || !d.Scaled {
		t.Errorf("decision %+v, want metric 12 and a scale from 1 to 3", d)
	}
	if !strings.Contains(d.Reason, "scale up 1 -> 3") || !strings.Contains(d.Reason, "6 in the gateway") {
		t.Errorf("reason %q does not explain the scale-up", d.Reason)
	}
	if got := p.scaleCalls(); !slices.Equal(got, []int{3}) {
		t.Errorf("Scale calls %v, want [3]", got)
	}
	if last, ok := r.Last(); !ok || last != d || len(seen) != 1 || seen[0] != d {
		t.Errorf("Last() = %+v, %v and OnDecision saw %v; want the returned decision", last, ok, seen)
	}
}

func TestKEDAHoldsThenScalesDownAfterWindow(t *testing.T) {
	p := newFakePool(2, ready(4, 2), ready(4, 3)) // queue 5 on 2 replicas: proposal 2
	clock := &fakeClock{t: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	r := kedaRunner(p, "", clock)
	d, err := r.RunOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if d.Scaled || d.Desired != 2 || len(p.scaleCalls()) != 0 {
		t.Fatalf("decision %+v, Scale calls %v: want a hold at 2 without a Scale call", d, p.scaleCalls())
	}

	p.mu.Lock()
	p.st.Replicas = []pool.ReplicaStatus{ready(1, 0), ready(0, 0)} // queue drained
	p.mu.Unlock()
	if d, _ := r.RunOnce(context.Background()); d.Scaled || d.Desired != 2 {
		t.Errorf("inside the scale-down window: %+v, want a hold at 2", d)
	}
	clock.t = clock.t.Add(301 * time.Second)
	if d, _ := r.RunOnce(context.Background()); !d.Scaled || d.Desired != 1 {
		t.Errorf("after the window: %+v, want a scale down to 1", d)
	}
	if got := p.scaleCalls(); !slices.Equal(got, []int{1}) {
		t.Errorf("Scale calls %v, want [1]", got)
	}
}

func TestRunOnceErrorsChangeNothing(t *testing.T) {
	tests := []struct {
		name    string
		pool    *fakePool
		stats   func(t *testing.T) string
		wantErr string
		poolOK  bool
	}{
		{"pool status", &fakePool{statusErr: errors.New("pool down")}, nil, "pool down", false},
		{"gateway status", newFakePool(1, ready(4, 9)),
			func(t *testing.T) string { return gatewayStats(t, http.StatusBadGateway, "oops") }, "502", true},
		{"gateway field", newFakePool(1, ready(4, 9)),
			func(t *testing.T) string { return gatewayStats(t, http.StatusOK, `{"inFlight":3}`) }, `no "queued" field`, true},
		{"scale", &fakePool{st: pool.Status{Desired: 1, Ready: 1, Replicas: []pool.ReplicaStatus{ready(4, 20)}},
			scaleErr: errors.New("quota")}, nil, "scale pool to 4: quota", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			url := ""
			if tc.stats != nil {
				url = tc.stats(t)
			}
			r := kedaRunner(tc.pool, url, &fakeClock{})
			d, err := r.RunOnce(context.Background())
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) || d.Error != err.Error() {
				t.Fatalf("err = %v (decision error %q), want it to mention %q", err, d.Error, tc.wantErr)
			}
			if d.Scaled || d.PoolOK != tc.poolOK || len(tc.pool.scaleCalls()) != 0 {
				t.Errorf("decision %+v, Scale calls %v: want no scaling and PoolOK %v", d, tc.pool.scaleCalls(), tc.poolOK)
			}
		})
	}
}

func TestValidate(t *testing.T) {
	keda := autoscale.NewKEDA(autoscale.DefaultKEDAConfig(1, 4), 1)
	hpa := autoscale.NewHPA(autoscale.DefaultHPAConfig(1, 4), 1)
	p := newFakePool(1)
	tests := []struct {
		name string
		r    *Runner
		want string
	}{
		{"unknown algo", &Runner{Algo: "vpa", Pool: p}, "algo must be"},
		{"keda without scaler", &Runner{Algo: AlgoKEDA, Pool: p}, "needs a KEDA"},
		{"hpa without scaler", &Runner{Algo: AlgoHPA, Pool: p, MaxNumSeqs: 4}, "needs an HPA"},
		{"hpa without slots", &Runner{Algo: AlgoHPA, HPA: hpa, Pool: p}, "MaxNumSeqs"},
		{"no pool", &Runner{Algo: AlgoKEDA, KEDA: keda}, "Pool is required"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := tc.r.RunOnce(context.Background()); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %v, want it to mention %q", err, tc.want)
			}
		})
	}
	r := &Runner{Algo: AlgoKEDA, KEDA: keda, Pool: p}
	if err := r.Run(context.Background()); err == nil || !strings.Contains(err.Error(), "cycle") {
		t.Errorf("Run without a cycle: err = %v, want a cycle error", err)
	}
	r = &Runner{Algo: AlgoHPA, HPA: hpa, Pool: p, MaxNumSeqs: 4, Cycle: time.Second, SampleEvery: -time.Second}
	if err := r.Run(context.Background()); err == nil || !strings.Contains(err.Error(), "sample period") {
		t.Errorf("Run with a negative sample period: err = %v, want a sample period error", err)
	}
}

func TestRunSyncsEveryCycleUntilDone(t *testing.T) {
	p := newFakePool(1, ready(0, 0))
	r := kedaRunner(p, "", &fakeClock{})
	r.Cycle = 5 * time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if err := r.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if st := status(t, r); st.Cycles < 2 || st.Errors != 0 {
		t.Errorf("status %+v, want several error-free cycles", st)
	}
}

func TestHandler(t *testing.T) {
	r := kedaRunner(newFakePool(1, ready(4, 12)), "", &fakeClock{})
	r.Cycle = 15 * time.Second
	if st := status(t, r); st.Last != nil || st.Cycles != 0 {
		t.Errorf("before any sync: %+v", st)
	}
	if _, err := r.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	st := status(t, r)
	if st.Algo != AlgoKEDA || st.CycleSeconds != 15 || st.Cycles != 1 || st.Last == nil || st.Last.Desired != 3 {
		t.Errorf("status %+v, want one keda cycle that chose 3 replicas", st)
	}
	srv := httptest.NewServer(r.Handler())
	defer srv.Close()
	for path, want := range map[string]string{
		"/healthz": `"ok"`,
		"/metrics": `tenantscaler_baseline_desired_replicas{algo="keda"} 3`,
	} {
		resp, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), want) {
			t.Errorf("GET %s: %s, body does not contain %q", path, resp.Status, want)
		}
	}
}

// status fetches GET /status through the handler.
func status(t *testing.T, r *Runner) Status {
	t.Helper()
	rec := httptest.NewRecorder()
	r.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/status", nil))
	var st Status
	if err := json.NewDecoder(rec.Body).Decode(&st); err != nil {
		t.Fatalf("decode /status (%d): %v", rec.Code, err)
	}
	return st
}

func TestInProcessAdapter(t *testing.T) {
	params := engine.DefaultParams()
	params.StartupDuration = 0
	p := pool.New(pool.Config{Name: "scalerun-test", Params: params, MaxReplicas: 2, Seed: 1})
	defer func() {
		if err := p.Close(); err != nil {
			t.Errorf("close pool: %v", err)
		}
	}()
	a := InProcess(p)
	if err := a.Scale(context.Background(), 2); err != nil {
		t.Fatal(err)
	}
	st, err := a.Status(context.Background())
	if err != nil || st.Desired != 2 || len(st.Replicas) != 2 {
		t.Errorf("Status() = %+v, %v; want 2 desired replicas", st, err)
	}
}
