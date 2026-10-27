package replica

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
	"time"

	"tenantscaler/internal/engine"
)

// fastParams is a millisecond-scale engine with no jitter and no load
// slowdown, so tests are quick and their timing predictable.
func fastParams(maxSeqs int) engine.Params {
	return engine.Params{
		PrefillOverhead:     2 * time.Millisecond,
		InterTokenLatency:   time.Millisecond,
		MaxNumSeqs:          maxSeqs,
		TimeFactorUnderLoad: 1,
	}
}

// newReady returns a started replica (StartupDuration 0) behind a test
// server that is closed at the end of the test.
func newReady(t *testing.T, p engine.Params) (*Replica, *httptest.Server) {
	t.Helper()
	r := New("r0", p, 1)
	r.Start(time.Now())
	srv := httptest.NewServer(r.Handler())
	t.Cleanup(srv.Close)
	return r, srv
}

// waitFor polls cond every millisecond until it holds or timeout passes.
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

func TestLifecycleStates(t *testing.T) {
	p := fastParams(1)
	p.StartupDuration = time.Hour
	r := New("r0", p, 1)
	if got := r.State(); got != StateStopped {
		t.Fatalf("before Start: state %q, want %q", got, StateStopped)
	}
	now := time.Now()
	r.Start(now)
	if got := r.State(); got != StateStarting {
		t.Fatalf("after Start: state %q, want %q", got, StateStarting)
	}
	r.Start(now.Add(-2 * time.Hour)) // second Start is ignored
	if got := r.State(); got != StateStarting {
		t.Fatalf("after second Start: state %q, want %q", got, StateStarting)
	}
	r.BeginDrain()
	if got := r.State(); got != StateDraining {
		t.Fatalf("after BeginDrain: state %q, want %q", got, StateDraining)
	}
	if !r.Drained() {
		t.Fatal("idle draining replica should be drained")
	}
	if err := r.WaitDrained(context.Background()); err != nil {
		t.Fatalf("WaitDrained: %v", err)
	}
}

func TestStartInThePastIsReady(t *testing.T) {
	p := fastParams(1)
	p.StartupDuration = time.Minute
	r := New("r0", p, 1)
	r.Start(time.Now().Add(-time.Minute))
	if got := r.State(); got != StateReady {
		t.Fatalf("state %q, want %q", got, StateReady)
	}
}

func TestAdmitRejectsUnlessReady(t *testing.T) {
	r := New("r0", fastParams(1), 1)
	if err := r.admit(context.Background()); !errors.Is(err, errUnavailable) {
		t.Fatalf("admit before Start: err %v, want errUnavailable", err)
	}
	r.Start(time.Now())
	r.BeginDrain()
	if err := r.admit(context.Background()); !errors.Is(err, errUnavailable) {
		t.Fatalf("admit while draining: err %v, want errUnavailable", err)
	}
}

// TestAdmitFCFS checks that waiters get slots strictly in arrival order.
func TestAdmitFCFS(t *testing.T) {
	r := New("r0", fastParams(1), 1)
	r.Start(time.Now())
	ctx := context.Background()
	if err := r.admit(ctx); err != nil {
		t.Fatal(err)
	}
	const n = 4
	order := make(chan int, n)
	for i := 0; i < n; i++ {
		go func() {
			if err := r.admit(ctx); err == nil {
				order <- i
			}
		}()
		waitFor(t, time.Second, "waiter to queue", func() bool { return r.Stats().Waiting == i+1 })
	}
	for want := 0; want < n; want++ {
		r.release() // hand the slot to the oldest waiter
		if got := <-order; got != want {
			t.Fatalf("slot %d went to waiter %d", want, got)
		}
		if s := r.Stats(); s.Running != 1 || s.Waiting != n-1-want {
			t.Fatalf("after grant %d: stats %+v", want, s)
		}
	}
	r.release()
	if s := r.Stats(); s != (Stats{}) {
		t.Fatalf("final stats %+v, want zero", s)
	}
}

func TestAdmitCancelWhileQueued(t *testing.T) {
	r := New("r0", fastParams(1), 1)
	r.Start(time.Now())
	if err := r.admit(context.Background()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- r.admit(ctx) }()
	waitFor(t, time.Second, "waiter to queue", func() bool { return r.Stats().Waiting == 1 })
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("admit err %v, want context.Canceled", err)
	}
	if s := r.Stats(); s.Running != 1 || s.Waiting != 0 {
		t.Fatalf("stats %+v, want 1 running 0 waiting", s)
	}
	r.BeginDrain()
	if r.Drained() {
		t.Fatal("drained while a request still runs")
	}
	r.release()
	if !r.Drained() {
		t.Fatal("not drained after the last release")
	}
}

// TestDelayFollowsRunningCount walks the running count 1,2,3,2,1,0 with
// admit and release and checks that prefill and token delays use the live
// count: LoadFactor is 1, 2, 3 at 1, 2, 3 running (MaxNumSeqs 3, F 3).
func TestDelayFollowsRunningCount(t *testing.T) {
	p := engine.Params{
		PrefillOverhead:     10 * time.Millisecond,
		PrefillPerToken:     time.Millisecond,
		InterTokenLatency:   10 * time.Millisecond,
		MaxNumSeqs:          3,
		TimeFactorUnderLoad: 3,
	}
	r := New("r0", p, 1)
	r.Start(time.Now())
	const inTok = 5 // prefill base = 10ms + 5*1ms = 15ms
	steps := []struct {
		name           string
		admit          bool // admit one request, else release one
		prefill, token time.Duration
	}{
		{"admit to 1", true, 15 * time.Millisecond, 10 * time.Millisecond},
		{"admit to 2", true, 30 * time.Millisecond, 20 * time.Millisecond},
		{"admit to 3", true, 45 * time.Millisecond, 30 * time.Millisecond},
		{"release to 2", false, 30 * time.Millisecond, 20 * time.Millisecond},
		{"release to 1", false, 15 * time.Millisecond, 10 * time.Millisecond},
		{"release to 0", false, 15 * time.Millisecond, 10 * time.Millisecond},
	}
	for _, s := range steps {
		if s.admit {
			if err := r.admit(context.Background()); err != nil {
				t.Fatalf("%s: admit: %v", s.name, err)
			}
		} else {
			r.release()
		}
		if got := r.prefillDelay(inTok); got != s.prefill {
			t.Errorf("%s (running %d): prefill delay %v, want %v", s.name, r.Stats().Running, got, s.prefill)
		}
		if got := r.tokenDelay(); got != s.token {
			t.Errorf("%s (running %d): token delay %v, want %v", s.name, r.Stats().Running, got, s.token)
		}
	}
}

// TestJitterReproducibleBySeed checks that New's seed fixes the jitter
// sequence: equal seeds give equal delays, different seeds do not, and
// every delay stays within Jitter's [0.5d, 1.5d] clamp.
func TestJitterReproducibleBySeed(t *testing.T) {
	p := fastParams(1)
	p.PrefillOverhead = 10 * time.Millisecond
	p.InterTokenLatency = 10 * time.Millisecond
	p.JitterFraction = 0.1
	delays := func(seed int64) []time.Duration {
		r := New("r0", p, seed)
		out := make([]time.Duration, 16)
		for i := range out {
			if i%2 == 0 {
				out[i] = r.prefillDelay(1)
			} else {
				out[i] = r.tokenDelay()
			}
		}
		return out
	}
	a, b, c := delays(1), delays(1), delays(2)
	if !slices.Equal(a, b) {
		t.Fatalf("same seed, different delays:\n%v\n%v", a, b)
	}
	if slices.Equal(a, c) {
		t.Fatalf("seeds 1 and 2 gave identical delays %v", a)
	}
	base := 10 * time.Millisecond
	jittered := false
	for _, d := range slices.Concat(a, c) {
		if d < base/2 || d > base*3/2 {
			t.Fatalf("delay %v outside [%v, %v]", d, base/2, base*3/2)
		}
		jittered = jittered || d != base
	}
	if !jittered {
		t.Fatal("JitterFraction 0.1 left every delay unchanged")
	}
}

// TestReadinessDuringStartup checks the cold-start phase deterministically:
// with an hour-long StartupDuration the replica cannot turn ready mid-test.
func TestReadinessDuringStartup(t *testing.T) {
	p := fastParams(1)
	p.StartupDuration = time.Hour
	r := New("r0", p, 1)
	srv := httptest.NewServer(r.Handler())
	defer srv.Close()

	if code := getStatus(t, srv.URL+"/health/ready"); code != http.StatusServiceUnavailable {
		t.Fatalf("ready before Start: %d, want 503", code)
	}
	r.Start(time.Now())
	if code := getStatus(t, srv.URL+"/health/ready"); code != http.StatusServiceUnavailable {
		t.Fatalf("ready while starting: %d, want 503", code)
	}
	if code := getStatus(t, srv.URL+"/health/live"); code != http.StatusOK {
		t.Fatalf("live while starting: %d, want 200", code)
	}
	resp := postCompletion(t, context.Background(), srv.URL, `{"prompt":"hi","max_tokens":1}`)
	resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable || resp.Header.Get("Retry-After") == "" {
		t.Fatalf("completion while starting: %d (Retry-After %q), want 503 with Retry-After",
			resp.StatusCode, resp.Header.Get("Retry-After"))
	}
	if s := r.Stats(); s != (Stats{}) {
		t.Fatalf("rejected request left stats %+v", s)
	}
}

// TestReadyAfterStartup checks the transition to ready. It asserts only
// that readiness is first seen no earlier than StartupDuration after Start,
// a bound that a slow or paused machine cannot violate.
func TestReadyAfterStartup(t *testing.T) {
	p := fastParams(1)
	p.StartupDuration = 50 * time.Millisecond
	r := New("r0", p, 1)
	srv := httptest.NewServer(r.Handler())
	defer srv.Close()

	start := time.Now()
	r.Start(start)
	waitFor(t, 2*time.Second, "readiness", func() bool {
		return getStatus(t, srv.URL+"/health/ready") == http.StatusOK
	})
	if waited := time.Since(start); waited < p.StartupDuration {
		t.Fatalf("ready after %v, before StartupDuration %v", waited, p.StartupDuration)
	}
	if got := r.State(); got != StateReady {
		t.Fatalf("state %q, want %q", got, StateReady)
	}
	resp := postCompletion(t, context.Background(), srv.URL, `{"prompt":"hi","max_tokens":1}`)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("completion when ready: %d, want 200", resp.StatusCode)
	}
}

// TestDrainFinishesRunningAndQueued checks that draining rejects new work
// but completes both the running and the queued request.
func TestDrainFinishesRunningAndQueued(t *testing.T) {
	p := fastParams(1)
	p.InterTokenLatency = 3 * time.Millisecond
	r, srv := newReady(t, p)
	results := make(chan int, 2)
	for i := 0; i < 2; i++ {
		go func() {
			resp, err := doPost(context.Background(), srv.URL, `{"prompt":"a b","max_tokens":20}`)
			if err != nil {
				results <- -1
				return
			}
			resp.Body.Close()
			results <- resp.StatusCode
		}()
	}
	waitFor(t, time.Second, "1 running + 1 waiting", func() bool { return r.Stats() == Stats{Running: 1, Waiting: 1} })

	r.BeginDrain()
	if code := getStatus(t, srv.URL+"/health/ready"); code != http.StatusServiceUnavailable {
		t.Fatalf("ready while draining: %d, want 503", code)
	}
	resp := postCompletion(t, context.Background(), srv.URL, `{"prompt":"late","max_tokens":1}`)
	resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("new request while draining: %d, want 503", resp.StatusCode)
	}
	if r.Drained() {
		t.Fatal("drained while requests are in flight")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := r.WaitDrained(ctx); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if code := <-results; code != http.StatusOK {
			t.Fatalf("in-flight request finished with %d, want 200", code)
		}
	}
	if !r.Drained() || r.State() != StateDraining {
		t.Fatalf("after drain: Drained=%v state=%q", r.Drained(), r.State())
	}
}

func TestWaitDrainedHonoursContext(t *testing.T) {
	r := New("r0", fastParams(1), 1)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := r.WaitDrained(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("WaitDrained err %v, want context.Canceled", err)
	}
}
