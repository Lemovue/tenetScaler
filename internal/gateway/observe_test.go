package gateway

import (
	"math"
	"net/http"
	"testing"
	"time"
)

var t0 = time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC)

func at(s float64) time.Time { return t0.Add(time.Duration(s * float64(time.Second))) }

// TestObservationWindowMath: only events in (now-window, now] count;
// arrivals include shed requests; attainment and the nearest-rank p99 are
// over completed requests, failed ones included as misses.
func TestObservationWindowMath(t *testing.T) {
	o := newObservations()
	for s := 1; s <= 10; s++ { // one arrival per second, 100 weighted tokens each
		o.arrival("A", at(float64(s)), 100)
	}
	o.shed("A", at(4), 100) // outside a 5 s window ending at 10 s
	o.shed("A", at(7), 100)
	o.shed("A", at(10), 100)
	ttfts := []float64{0.1, 0.2, 0.3, 3.0} // the 3 s one misses the SLO
	for i, ttft := range ttfts {
		o.completed("A", at(6+float64(i)), time.Duration(ttft*float64(time.Second)), ttft < 2)
	}
	o.completed("A", at(2), 10*time.Second, false) // outside the window
	o.failed("A", at(1), 120*time.Second)          // outside the window

	got, failed := o.observe("A", at(10), 5*time.Second)
	// Arrivals at 6, 7, 8, 9, 10 s.
	if got.ArrivalRequestsPerSec != 1 || got.ArrivalWeightedTokensPerSec != 100 {
		t.Errorf("rates %v req/s %v tok/s, want 1 and 100", got.ArrivalRequestsPerSec, got.ArrivalWeightedTokensPerSec)
	}
	// Admitted = offered minus the 2 shed in the window: (500-200)/5 s.
	if got.AdmittedWeightedTokensPerSec != 60 {
		t.Errorf("admitted %v tok/s, want 60", got.AdmittedWeightedTokensPerSec)
	}
	if got.Shed != 2 || got.Completed != 4 || !got.HasSLOData {
		t.Errorf("shed %d completed %d hasData %v, want 2, 4, true", got.Shed, got.Completed, got.HasSLOData)
	}
	if got.SLOAttainment != 0.75 {
		t.Errorf("attainment %v, want 0.75", got.SLOAttainment)
	}
	if got.P99TTFTSeconds != 3 || failed != 0 {
		t.Errorf("p99 TTFT %v with %d failed, want 3 (nearest rank of 4 values) and none", got.P99TTFTSeconds, failed)
	}

	// A request that waited 6 s and then failed is a miss, and its wait
	// (a lower bound of its TTFT) enters the p99.
	o.failed("A", at(10), 6*time.Second)
	got, failed = o.observe("A", at(10), 5*time.Second)
	if got.Completed != 5 || failed != 1 || got.SLOAttainment != 0.6 || got.P99TTFTSeconds != 6 {
		t.Errorf("with a failure: completed %d failed %d attainment %v p99 %v, want 5, 1, 0.6, 6",
			got.Completed, failed, got.SLOAttainment, got.P99TTFTSeconds)
	}

	empty, _ := o.observe("A", at(100), 5*time.Second)
	if empty.Completed != 0 || empty.HasSLOData || empty.ArrivalRequestsPerSec != 0 || empty.P99TTFTSeconds != 0 {
		t.Errorf("quiet window %+v, want zeros", empty)
	}
	if unknown, _ := o.observe("Z", at(10), 5*time.Second); unknown.Tenant != "Z" || unknown.HasSLOData {
		t.Errorf("unknown tenant %+v", unknown)
	}
}

// TestStatsWindowDuringWarmUp: while the gateway is younger than the
// window, rates are divided by its uptime (at least 1 s), not by time
// before it existed.
func TestStatsWindowDuringWarmUp(t *testing.T) {
	clk := newTestClock()
	up := newFakeUpstream(t, nil)
	g, base := startGatewayAt(t, Config{Discovery: StaticDiscovery{up.URL()}}, clk.Now)
	clk.advance(200 * time.Millisecond)
	if r := post(t, base, "A", "", completionBody("w", 2, true)); r.status != http.StatusOK {
		t.Fatalf("status %d", r.status)
	}
	for _, tt := range []struct {
		uptime time.Duration
		window float64
	}{
		{200 * time.Millisecond, 1}, // the 1 s floor
		{5 * time.Second, 5},        // the uptime
		{time.Minute, DefaultStatsWindow.Seconds()},
	} {
		clk.advance(tt.uptime - (clk.Now().Sub(t0)))
		st := g.Stats(0)
		want := 1 / tt.window // one request of weight 1 + 2*2 = 5
		if tt.uptime == time.Minute {
			want = 0 // the request is older than the window
		}
		a := st.Tenants[0]
		if st.WindowSeconds != tt.window || math.Abs(a.ArrivalRequestsPerSec-want) > 1e-12 || math.Abs(a.ArrivalWeightedTokensPerSec-5*want) > 1e-12 {
			t.Errorf("uptime %s: window %v s, A %+v; want %v s and %v req/s", tt.uptime, st.WindowSeconds, a, tt.window, want)
		}
	}
}

// TestObservationsPruneOldEvents: nothing older than MaxStatsWindow is
// kept, even for a tenant that is only ever written.
func TestObservationsPruneOldEvents(t *testing.T) {
	o := newObservations()
	for s := range 600 { // 10 minutes, one event per second
		o.arrival("A", at(float64(s)), 1)
		o.completed("A", at(float64(s)), time.Second, true)
	}
	e := o.tenants["A"]
	limit := int(MaxStatsWindow / time.Second)
	if n := len(e.arrivals.live()); n > limit {
		t.Errorf("%d arrivals kept, want <= %d", n, limit)
	}
	if n := len(e.completions.live()); n > limit {
		t.Errorf("%d completions kept, want <= %d", n, limit)
	}
	if c := cap(e.arrivals.items); c > 4*limit {
		t.Errorf("arrival buffer capacity %d grows without bound", c)
	}
	got, _ := o.observe("A", at(599), MaxStatsWindow)
	if math.Abs(got.ArrivalRequestsPerSec-1) > 1e-9 {
		t.Errorf("rate over the full window = %v, want 1", got.ArrivalRequestsPerSec)
	}
}

func TestBufferPruneAndCompact(t *testing.T) {
	var b buffer[int]
	for i := range 100 {
		b.push(i)
	}
	b.pruneWhile(func(v int) bool { return v < 60 })
	if live := b.live(); len(live) != 40 || live[0] != 60 || b.head != 0 {
		t.Fatalf("after pruning: %d live from %d, head %d", len(live), live[0], b.head)
	}
	b.pruneWhile(func(v int) bool { return v < 65 })
	if live := b.live(); len(live) != 35 || live[0] != 65 {
		t.Fatalf("after a small prune: %d live from %d", len(live), live[0])
	}
	b.pruneWhile(func(int) bool { return true })
	if len(b.live()) != 0 {
		t.Fatal("buffer not empty after pruning everything")
	}
}

func TestParseWindow(t *testing.T) {
	for _, tt := range []struct {
		in   string
		want time.Duration
		ok   bool
	}{
		{"", 0, true},
		{"15s", 15 * time.Second, true},
		{"1m30s", 90 * time.Second, true},
		{"10", 10 * time.Second, true},
		{"2.5", 2500 * time.Millisecond, true},
		{"0", 0, false},
		{"-1s", 0, false},
		{"soon", 0, false},
	} {
		got, err := parseWindow(tt.in)
		if (err == nil) != tt.ok || (tt.ok && got != tt.want) {
			t.Errorf("parseWindow(%q) = %v, %v; want %v, ok %v", tt.in, got, err, tt.want, tt.ok)
		}
	}
}
