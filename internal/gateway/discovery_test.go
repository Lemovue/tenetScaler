package gateway

import (
	"context"
	"errors"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestStaticDiscoveryNormalizes(t *testing.T) {
	got := StaticDiscovery{
		" http://b:8000/ ", "a:8000", "", "http://b:8000", "https://c",
		"http://h:11434/v1/", "g:9000/v1", "http://d/v1beta",
	}.Endpoints()
	want := []string{"http://a:8000", "http://b:8000", "http://d/v1beta", "http://g:9000", "http://h:11434", "https://c"}
	if !slices.Equal(got, want) {
		t.Errorf("Endpoints() = %v, want %v", got, want)
	}
}

// TestEndpointWithV1Suffix: an OpenAI-style base URL ending in /v1 reaches
// {base}/v1/completions, not {base}/v1/v1/completions.
func TestEndpointWithV1Suffix(t *testing.T) {
	up := newFakeUpstream(t, nil)
	_, base := startGateway(t, Config{Discovery: StaticDiscovery{up.URL() + "/v1"}})
	if r := post(t, base, "A", "", completionBody("w", 2, true)); r.status != 200 || r.tokens != 2 {
		t.Errorf("status %d tokens %d, want 200 with 2 tokens: %s", r.status, r.tokens, r.body)
	}
}

// TestPollingDiscovery: refreshes replace the list, failures keep the last
// good one, subscribers hear only real changes.
func TestPollingDiscovery(t *testing.T) {
	var mu sync.Mutex
	eps, fail := []string{"http://b", "http://a"}, error(nil)
	d := NewPollingDiscovery(func(context.Context) ([]string, error) {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(eps), fail
	}, time.Hour)
	var notified atomic.Int32
	unsubscribe := d.Subscribe(func() { notified.Add(1) })
	ctx := context.Background()

	if err := d.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	if got := d.Endpoints(); !slices.Equal(got, []string{"http://a", "http://b"}) {
		t.Errorf("Endpoints() = %v", got)
	}
	_ = d.Refresh(ctx) // unchanged: no notification
	if n := notified.Load(); n != 1 {
		t.Errorf("notified %d times, want 1", n)
	}

	mu.Lock()
	fail = errors.New("pool unreachable")
	mu.Unlock()
	if err := d.Refresh(ctx); err == nil || d.Err() == nil {
		t.Error("failed fetch not reported")
	}
	if got := d.Endpoints(); len(got) != 2 {
		t.Errorf("after a failure Endpoints() = %v, want the last good list", got)
	}

	mu.Lock()
	eps, fail = []string{"http://c"}, nil
	mu.Unlock()
	unsubscribe()
	if err := d.Refresh(ctx); err != nil || d.Err() != nil {
		t.Fatalf("recovery: %v", err)
	}
	if notified.Load() != 1 {
		t.Error("an unsubscribed callback was called")
	}
	if got := d.Endpoints(); !slices.Equal(got, []string{"http://c"}) {
		t.Errorf("Endpoints() = %v", got)
	}
}

func TestPollingDiscoveryStartPollsUntilCanceled(t *testing.T) {
	var calls atomic.Int32
	d := NewPollingDiscovery(func(context.Context) ([]string, error) {
		calls.Add(1)
		return []string{"http://x"}, nil
	}, 10*time.Millisecond)
	ctx, cancel := context.WithCancel(context.Background())
	d.Start(ctx)
	d.Start(ctx) // no second poller
	waitFor(t, "repeated polls", 5*time.Second, func() bool { return calls.Load() >= 3 })
	cancel()
	time.Sleep(30 * time.Millisecond) // let the poller observe the cancellation
	n := calls.Load()
	time.Sleep(50 * time.Millisecond)
	if calls.Load() != n {
		t.Error("discovery kept polling after its context was canceled")
	}
	if got := d.Endpoints(); !slices.Equal(got, []string{"http://x"}) {
		t.Errorf("Endpoints() = %v", got)
	}
}

// TestGatewayFollowsDiscoveryChanges: a notifying discovery wakes the
// dispatcher, so a queued request is served as soon as a replica appears.
func TestGatewayFollowsDiscoveryChanges(t *testing.T) {
	up := newFakeUpstream(t, nil)
	var mu sync.Mutex
	var eps []string
	d := NewPollingDiscovery(func(context.Context) ([]string, error) {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(eps), nil
	}, time.Hour) // no polling: only explicit refreshes
	g, base := startGateway(t, Config{Discovery: d})
	done := make(chan result, 1)
	go func() { done <- post(t, base, "A", "", completionBody("w", 2, true)) }()
	waitFor(t, "the request to queue", 5*time.Second, func() bool { return g.Stats(0).Queued == 1 })

	mu.Lock()
	eps = []string{up.URL()}
	mu.Unlock()
	if err := d.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if r := <-done; r.status != 200 || r.tokens != 2 {
		t.Fatalf("status %d tokens %d", r.status, r.tokens)
	}
	if st := g.Stats(0); st.Endpoints != 1 {
		t.Errorf("gateway sees %d endpoints, want 1", st.Endpoints)
	}
}
