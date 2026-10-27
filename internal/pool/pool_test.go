package pool

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"

	"tenantscaler/internal/engine"
	"tenantscaler/internal/model"
	"tenantscaler/internal/replica"
)

// testParams is a millisecond-scale engine without jitter.
func testParams(startup time.Duration) engine.Params {
	return engine.Params{
		PrefillOverhead:     time.Millisecond,
		InterTokenLatency:   time.Millisecond,
		MaxNumSeqs:          2,
		TimeFactorUnderLoad: 1,
		StartupDuration:     startup,
	}
}

func newPool(t *testing.T, startup time.Duration, maxReplicas int) *Pool {
	t.Helper()
	p := New(Config{Name: "p", Params: testParams(startup), MaxReplicas: maxReplicas, Seed: 7})
	t.Cleanup(func() {
		if err := p.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	return p
}

func waitFor(t *testing.T, timeout time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out after %v waiting for %s", timeout, what)
		}
		time.Sleep(time.Millisecond)
	}
}

func ids(st Status) []string {
	out := make([]string, len(st.Replicas))
	for i, r := range st.Replicas {
		out[i] = r.ID + ":" + r.State
	}
	return out
}

// complete sends one non-streamed completion to a replica base URL.
func complete(ctx context.Context, base string, tokens int) (int, error) {
	body := fmt.Sprintf(`{"prompt":"hello pool","max_tokens":%d}`, tokens)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/v1/completions", strings.NewReader(body))
	if err != nil {
		return 0, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.Header.Get(replica.SimulatedHeader) != "true" {
		return resp.StatusCode, errors.New("missing simulated header")
	}
	return resp.StatusCode, nil
}

// TestScaleUpStarting checks the cold-start phase deterministically: with
// an hour-long StartupDuration new replicas cannot turn ready mid-test.
func TestScaleUpStarting(t *testing.T) {
	p := newPool(t, time.Hour, 4)
	if err := p.Scale(2); err != nil {
		t.Fatal(err)
	}
	st := p.Status()
	if st.Desired != 2 || st.Starting != 2 || st.Ready != 0 || st.Draining != 0 || len(st.Replicas) != 2 {
		t.Fatalf("right after Scale(2): %+v", st)
	}
	for _, r := range st.Replicas {
		if r.State != replica.StateStarting || !strings.HasPrefix(r.URL, "http://127.0.0.1:") {
			t.Fatalf("replica %+v, want starting on 127.0.0.1", r)
		}
	}
	if eps := p.Endpoints(); len(eps) != 0 {
		t.Fatalf("endpoints while starting: %v", eps)
	}
	if got := st.Observation(); got != (model.PoolObservation{StartingReplicas: 2, DesiredReplicas: 2}) {
		t.Fatalf("observation %+v", got)
	}
}

// TestScaleUpBecomesReady checks the transition to ready. It asserts only
// that readiness is first seen no earlier than StartupDuration after Scale,
// a bound that a slow or paused machine cannot violate.
func TestScaleUpBecomesReady(t *testing.T) {
	const startup = 50 * time.Millisecond
	p := newPool(t, startup, 4)
	start := time.Now()
	if err := p.Scale(2); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 2*time.Second, "2 ready replicas", func() bool { return p.Status().Ready == 2 })
	if waited := time.Since(start); waited < startup {
		t.Fatalf("ready after %v, before StartupDuration %v", waited, startup)
	}
	eps := p.Endpoints()
	if len(eps) != 2 || !sort.StringsAreSorted(eps) {
		t.Fatalf("endpoints %v, want 2 sorted URLs", eps)
	}
	for _, ep := range eps {
		if !strings.HasPrefix(ep, "http://127.0.0.1:") {
			t.Fatalf("endpoint %q not on 127.0.0.1", ep)
		}
		if code, err := complete(context.Background(), ep, 3); err != nil || code != http.StatusOK {
			t.Fatalf("completion on %s: %d, %v", ep, code, err)
		}
	}
	if got := p.Status().Observation(); got != (model.PoolObservation{ReadyReplicas: 2, DesiredReplicas: 2}) {
		t.Fatalf("observation %+v", got)
	}
}

func TestScaleClamps(t *testing.T) {
	p := newPool(t, 0, 3)
	if err := p.Scale(10); err != nil {
		t.Fatal(err)
	}
	if st := p.Status(); st.Desired != 3 || len(st.Replicas) != 3 {
		t.Fatalf("Scale(10) with max 3: %+v", st)
	}
	if err := p.Scale(-5); err != nil {
		t.Fatal(err)
	}
	if st := p.Status(); st.Desired != 0 {
		t.Fatalf("Scale(-5): desired %d, want 0", st.Desired)
	}
	waitFor(t, 2*time.Second, "all replicas shut down", func() bool { return len(p.Status().Replicas) == 0 })
}

// TestDrainOrder checks the removal order without timing: a starting
// replica goes before a ready one even when it is older, and ready
// replicas go newest first. (In a live pool every replica shares one
// StartupDuration, so starting ones are always the newest and
// TestScaleDownOrder cannot tell these rules apart.)
func TestDrainOrder(t *testing.T) {
	now := time.Now()
	mk := func(id string, startedAgo time.Duration) *member {
		r := replica.New(id, testParams(time.Hour), 1)
		r.Start(now.Add(-startedAgo))
		return &member{inst: &simInstance{rep: r}}
	}
	active := []*member{ // oldest first, as Pool.members
		mk("ready-old", 3*time.Hour),
		mk("starting-a", 0),
		mk("ready-mid", 2*time.Hour),
		mk("starting-b", 0),
		mk("ready-new", 61*time.Minute),
	}
	var got []string
	for _, m := range drainOrder(active) {
		got = append(got, m.inst.ID())
	}
	want := "starting-b,starting-a,ready-new,ready-mid,ready-old"
	if strings.Join(got, ",") != want {
		t.Fatalf("drain order %v, want %s", got, want)
	}
}

// TestScaleDownOrder checks that scale-down removes starting replicas
// before ready ones, and the newest ready ones first.
func TestScaleDownOrder(t *testing.T) {
	const startup = 250 * time.Millisecond
	p := newPool(t, startup, 5)
	if err := p.Scale(3); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 2*time.Second, "3 ready", func() bool { return p.Status().Ready == 3 })
	if err := p.Scale(5); err != nil { // p-4, p-5 start loading
		t.Fatal(err)
	}
	if err := p.Scale(4); err != nil { // drains newest starting: p-5
		t.Fatal(err)
	}
	if err := p.Scale(2); err != nil { // drains p-4 (starting), then p-3 (newest ready)
		t.Fatal(err)
	}
	waitFor(t, 2*time.Second, "drained replicas removed", func() bool { return len(p.Status().Replicas) == 2 })
	st := p.Status()
	if got, want := strings.Join(ids(st), ","), "p-1:ready,p-2:ready"; got != want {
		t.Fatalf("replicas %s, want %s", got, want)
	}
	if st.Desired != 2 || st.Ready != 2 || st.Starting != 0 || st.Draining != 0 {
		t.Fatalf("status %+v", st)
	}
	if err := p.Scale(3); err != nil { // scale up again: a new replica, never a draining one
		t.Fatal(err)
	}
	if got := ids(p.Status()); got[len(got)-1] != "p-6:starting" {
		t.Fatalf("after re-scale-up: %v, want p-6 starting", got)
	}
}

// TestScaleDownDrainsInFlight checks that a removed replica finishes its
// running request before its server shuts down.
func TestScaleDownDrainsInFlight(t *testing.T) {
	params := testParams(0)
	params.InterTokenLatency = 4 * time.Millisecond
	p := New(Config{Name: "p", Params: params, MaxReplicas: 2, Seed: 1})
	t.Cleanup(func() { _ = p.Close() })
	if err := p.Scale(1); err != nil {
		t.Fatal(err)
	}
	ep := p.Endpoints()[0]
	type result struct {
		code int
		err  error
	}
	done := make(chan result, 1)
	go func() {
		code, err := complete(context.Background(), ep, 50)
		done <- result{code, err}
	}()
	waitFor(t, time.Second, "request running", func() bool {
		st := p.Status()
		return len(st.Replicas) == 1 && st.Replicas[0].Running == 1
	})
	if err := p.Scale(0); err != nil {
		t.Fatal(err)
	}
	st := p.Status()
	if st.Draining != 1 || st.Desired != 0 || len(p.Endpoints()) != 0 {
		t.Fatalf("while draining: %+v, endpoints %v", st, p.Endpoints())
	}
	if r := <-done; r.err != nil || r.code != http.StatusOK {
		t.Fatalf("in-flight request: %d, %v", r.code, r.err)
	}
	waitFor(t, 2*time.Second, "drained replica removed", func() bool { return len(p.Status().Replicas) == 0 })
	if _, err := http.Get(ep + "/health/live"); err == nil {
		t.Fatal("drained replica still accepts connections")
	}
}

func TestScaleRejectsInvalidParams(t *testing.T) {
	p := New(Config{Name: "p", Params: engine.Params{}, MaxReplicas: 1})
	defer p.Close()
	if err := p.Scale(1); err == nil {
		t.Fatal("Scale with zero engine params succeeded")
	}
}

// TestCloseStopsEverything checks that Close aborts in-flight work, frees
// every listener and leaves no goroutines behind.
func TestCloseStopsEverything(t *testing.T) {
	hc := &http.Client{Transport: &http.Transport{}}
	before := runtime.NumGoroutine()
	p := New(Config{Name: "p", Params: testParams(0), MaxReplicas: 3, Seed: 1})
	if err := p.Scale(3); err != nil {
		t.Fatal(err)
	}
	eps := p.Endpoints()
	errc := make(chan error, 1)
	go func() {
		req, _ := http.NewRequest(http.MethodPost, eps[0]+"/v1/completions",
			strings.NewReader(`{"prompt":"x","max_tokens":30000,"stream":true}`))
		resp, err := hc.Do(req)
		if err == nil {
			_, err = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}
		errc <- err
	}()
	waitFor(t, time.Second, "request running", func() bool {
		running := 0
		for _, r := range p.Status().Replicas {
			running += r.Running
		}
		return running == 1
	})

	start := time.Now()
	if err := p.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("Close took %v", d)
	}
	if err := <-errc; err == nil {
		t.Fatal("in-flight stream completed normally after Close")
	}
	for _, ep := range eps {
		if _, err := hc.Get(ep + "/health/live"); err == nil {
			t.Fatalf("%s still accepts connections after Close", ep)
		}
	}
	if err := p.Scale(1); !errors.Is(err, ErrClosed) {
		t.Fatalf("Scale after Close: %v, want ErrClosed", err)
	}
	if err := p.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	hc.CloseIdleConnections()
	waitFor(t, 2*time.Second, "goroutines to exit", func() bool { return runtime.NumGoroutine() <= before })
}
