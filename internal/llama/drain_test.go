package llama

import (
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	"tenantscaler/internal/replica"
)

// TestDrainGrace checks that an idle replica is not declared drained
// before the drain grace has passed since BeginDrain: until then a gateway
// may still dispatch to it from a stale endpoint list.
func TestDrainGrace(t *testing.T) {
	const grace = 400 * time.Millisecond
	l := fakeLauncher(t)
	l.DrainGrace = grace
	p := launch(t, l, "grace-1", 0)
	waitFor(t, 30*time.Second, "ready", func() bool { return p.State() == replica.StateReady })
	p.BeginDrain()
	began := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := p.WaitDrained(ctx); err != nil {
		t.Fatal(err)
	}
	if waited := time.Since(began); waited < grace {
		t.Fatalf("idle replica drained after %v, before the %v grace", waited, grace)
	}
}

// TestDrainWithBrokenMetrics checks that a draining replica whose /metrics
// keeps failing is declared drained after maxDrainFailures readings, with
// a warning, instead of being kept (and its CPU slot held) forever.
func TestDrainWithBrokenMetrics(t *testing.T) {
	t.Setenv(envFakeMode, "metrics-broken")
	logs := &lockedBuffer{}
	l := fakeLauncher(t)
	l.DrainGrace = time.Millisecond
	l.Logger = slog.New(slog.NewTextHandler(logs, nil))
	p := launch(t, l, "broken-1", 0)
	waitFor(t, 30*time.Second, "ready", func() bool { return p.State() == replica.StateReady })
	p.BeginDrain()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := p.WaitDrained(ctx); err != nil {
		t.Fatalf("WaitDrained with a broken /metrics: %v", err)
	}
	if hits := metricsHits(t, p); hits < maxDrainFailures {
		t.Fatalf("declared drained after %d /metrics readings, want >= %d", hits, maxDrainFailures)
	}
	if !strings.Contains(logs.String(), "keeps failing while draining") {
		t.Fatalf("no warning about the forced drain:\n%s", logs)
	}
}

// TestStatsCache checks with an injected clock that Stats serves a cached
// reading for statsMaxAge and fetches /metrics again afterwards.
func TestStatsCache(t *testing.T) {
	p := launch(t, fakeLauncher(t), "cache-1", 0)
	now := time.Unix(1_000_000, 0)
	p.statsClock = func() time.Time { return now } // before any Stats call
	waitFor(t, 30*time.Second, "ready", func() bool { return p.State() == replica.StateReady })
	if hits := metricsHits(t, p); hits != 0 {
		t.Fatalf("%d /metrics hits before the first Stats", hits)
	}
	steps := []struct {
		advance time.Duration
		hits    int
	}{
		{0, 1},                              // first reading fetches
		{statsMaxAge - time.Millisecond, 1}, // still cached
		{time.Millisecond, 2},               // statsMaxAge old: fetched again
		{0, 2},                              // the new reading is cached
	}
	for i, s := range steps {
		now = now.Add(s.advance)
		if st := p.Stats(); st != (replica.Stats{}) {
			t.Fatalf("step %d: idle stats %+v", i, st)
		}
		if hits := metricsHits(t, p); hits != s.hits {
			t.Fatalf("step %d: %d /metrics hits, want %d", i, hits, s.hits)
		}
	}
}

// TestStatsWarnsRateLimited checks that failing /metrics reads are logged
// at Warn, at most once per statsWarnEvery.
func TestStatsWarnsRateLimited(t *testing.T) {
	t.Setenv(envFakeMode, "metrics-broken")
	logs := &lockedBuffer{}
	l := fakeLauncher(t)
	l.Logger = slog.New(slog.NewTextHandler(logs, nil))
	p := launch(t, l, "warn-1", 0)
	now := time.Unix(1_000_000, 0)
	p.statsClock = func() time.Time { return now }
	waitFor(t, 30*time.Second, "ready", func() bool { return p.State() == replica.StateReady })
	const msg = "read llama-server metrics"
	// Readings at 0, 1 s, statsWarnEvery-1 s and statsWarnEvery: each one
	// fetches (they are more than statsMaxAge apart) and fails.
	for i, advance := range []time.Duration{0, time.Second, statsWarnEvery - 2*time.Second, time.Second} {
		now = now.Add(advance)
		p.Stats()
		want := 1
		if i == 3 { // statsWarnEvery after the first warning
			want = 2
		}
		if got := strings.Count(logs.String(), msg); got != want {
			t.Fatalf("after reading %d: %d warnings, want %d:\n%s", i, got, want, logs)
		}
	}
	if !strings.Contains(logs.String(), "level=WARN") {
		t.Fatalf("metrics failures not logged at Warn:\n%s", logs)
	}
}
