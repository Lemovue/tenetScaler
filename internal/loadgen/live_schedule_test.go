package loadgen

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"tenantscaler/internal/eval"
	"tenantscaler/internal/workload"
)

// steadyScenario has one steady tenant A at rps (wall-clock at Speed 1).
func steadyScenario(rps float64) workload.Scenario {
	return workload.Scenario{
		Name:     "steady-test",
		Duration: time.Minute,
		Tenants: []workload.TenantPattern{{
			Tenant: "A", Kind: workload.KindSteady, BaseRPS: rps, PeakRPS: rps, CV: 1,
			Mix: workload.PriorityMix{Standard: 1}, In: tinyLengths, Out: tinyLengths,
		}},
	}
}

func TestLiveScheduleAbsorbsLatenessAndDispatchWork(t *testing.T) {
	l, err := NewLive(steadyScenario(250), []Target{{Name: "x", URL: "http://127.0.0.1:1"}}, 1, Options{Logger: discard})
	if err != nil {
		t.Fatal(err)
	}
	tn := l.tenants[0]
	start := time.Now()
	l.s.start, tn.mark = start, start

	// 125 gaps of one operational second at 250 req/s are 4 ms each:
	// the 125th arrival is due 500 ms after the start. Each arrival is
	// followed by 1 ms of work (dispatching), and every timer wakes a
	// little late; neither may lengthen the following gaps.
	const arrivals, want = 125, 500 * time.Millisecond
	var last time.Time
	for range arrivals {
		if !l.waitOperational(context.Background(), tn, 1) {
			t.Fatal("waitOperational returned false without cancellation")
		}
		last = time.Now()
		time.Sleep(time.Millisecond)
	}
	if got := tn.mark.Sub(start); (got - want).Abs() > time.Microsecond {
		t.Errorf("last scheduled arrival at %v, want exactly %v (the schedule drifted)", got, want)
	}
	// Integrating from the wake-up time instead loses at least the 1 ms
	// of work per arrival: 125 ms over the run.
	if got := last.Sub(start); got < want-time.Millisecond || got > want+80*time.Millisecond {
		t.Errorf("125 arrivals took %v, want about %v", got, want)
	}
}

func TestNewLiveSpikeTenant(t *testing.T) {
	targets := []Target{{Name: "x", URL: "http://127.0.0.1:1"}}
	tests := []struct {
		name     string
		scenario workload.Scenario
		spike    string
		want     map[string]bool // tenant -> controlled by Spike
		err      string
	}{
		{"only spike tenant by default", workload.DefaultScenario(), "", map[string]bool{"A": false, "B": false, "C": true}, ""},
		{"named among several", workload.ReclaimScenario(), "C", map[string]bool{"A": false, "B": false, "C": true}, ""},
		{"ambiguous", workload.ReclaimScenario(), "", nil, "several spike tenants"},
		{"not a spike tenant", workload.ReclaimScenario(), "B", nil, "not a spike tenant"},
		{"unknown", workload.DefaultScenario(), "Z", nil, "not a spike tenant"},
		{"no spike tenant", steadyScenario(1), "", map[string]bool{"A": false}, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			l, err := NewLive(tc.scenario, targets, 1, Options{SpikeTenant: tc.spike, Logger: discard})
			if tc.err != "" {
				if err == nil || !strings.Contains(err.Error(), tc.err) {
					t.Fatalf("err = %v, want it to mention %q", err, tc.err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			got := map[string]bool{}
			for _, tn := range l.tenants {
				got[tn.pattern.Tenant] = tn.spiked
			}
			for tenant, spiked := range tc.want {
				if got[tenant] != spiked {
					t.Errorf("tenant %s controlled by Spike = %v, want %v", tenant, got[tenant], spiked)
				}
			}
		})
	}
}

func TestLiveCancelAbortsAndStopDrains(t *testing.T) {
	for _, cancelCtx := range []bool{true, false} {
		name := "Stop lets requests in flight finish"
		if cancelCtx {
			name = "canceling the Start context aborts requests in flight"
		}
		t.Run(name, func(t *testing.T) {
			release := make(chan struct{})
			var once sync.Once
			unblock := func() { once.Do(func() { close(release) }) }
			srv := &fakeLLM{block: release}
			url := serve(t, srv).URL // closed at cleanup, after the deferred unblock:
			defer unblock()          // the server waits for its blocked handlers
			var mu sync.Mutex
			var recs []eval.Record
			l, err := NewLive(steadyScenario(40), []Target{{Name: "x", URL: url}}, 5, Options{
				Logger:   discard,
				OnRecord: func(_ string, r eval.Record) { mu.Lock(); recs = append(recs, r); mu.Unlock() },
			})
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if err := l.Start(ctx); err != nil {
				t.Fatal(err)
			}
			for deadline := time.Now().Add(2 * time.Second); len(srv.requests()) < 2; {
				if time.Now().After(deadline) {
					t.Fatal("no requests reached the server within 2s")
				}
				time.Sleep(5 * time.Millisecond)
			}
			if cancelCtx {
				cancel()
			} else {
				l.Stop()
			}
			done := make(chan struct{})
			go func() { l.Wait(); close(done) }()
			if !cancelCtx {
				select {
				case <-done:
					t.Fatal("Wait returned while requests were still in flight")
				case <-time.After(100 * time.Millisecond):
				}
				unblock()
			}
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("Wait did not return")
			}

			mu.Lock()
			defer mu.Unlock()
			c := l.Counts()["x"]
			if len(recs) < 2 || len(recs) != c.Completed+c.Shed+c.Failed {
				t.Fatalf("%d records, counts %+v: want every request sent recorded", len(recs), c)
			}
			for _, r := range recs {
				switch {
				case cancelCtx && !r.Failed:
					t.Errorf("record %+v: want it aborted (failed)", r)
				case !cancelCtx && !r.Completed():
					t.Errorf("record %+v: want it completed", r)
				}
			}
			if cancelCtx && c.FailedBy[failAborted] != len(recs) {
				t.Errorf("counts %+v: want all %d failures counted as %q", c, len(recs), failAborted)
			}
		})
	}
}
