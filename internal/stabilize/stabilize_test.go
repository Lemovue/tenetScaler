package stabilize

import (
	"encoding/json"
	"math/rand/v2"
	"testing"
	"time"
)

var t0 = time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC)

type call struct {
	at      time.Duration // offset from t0
	desired int
	want    int
}

func TestNext(t *testing.T) {
	base := Config{ScaleDownWindow: time.Minute, MaxStep: 4, Min: 1, Max: 20}
	with := func(f func(*Config)) Config { c := base; f(&c); return c }
	cases := []struct {
		name    string
		cfg     Config
		initial int
		calls   []call
	}{
		{"steady desired holds", base, 4, []call{{0, 4, 4}, {15 * time.Second, 4, 4}}},
		{"scale up immediately", base, 2, []call{{0, 5, 5}}},
		{"scale up bounded by MaxStep", base, 2, []call{
			{0, 12, 6}, {15 * time.Second, 12, 10}, {30 * time.Second, 12, 12},
		}},
		{"scale up stops at the latest desired, not the window max", base, 2, []call{
			{0, 12, 6}, {15 * time.Second, 8, 8},
		}},
		{"clock stepped back: a future sample protects for one window", base, 10, []call{
			{time.Hour, 10, 10},
			// The clock jumps back an hour; 10@1h is re-dated to 0s.
			{0, 1, 10}, {45 * time.Second, 1, 10},
			{60 * time.Second, 1, 6}, {75 * time.Second, 1, 2},
		}},
		{"brief dip inside the window is ignored", base, 8, []call{
			{0, 8, 8}, {15 * time.Second, 3, 8}, {30 * time.Second, 2, 8}, {45 * time.Second, 3, 8},
			// 8@0s leaves the window at 60s; the window max is now 3.
			{60 * time.Second, 3, 4}, {75 * time.Second, 3, 3},
		}},
		{"scale down to the window max, not the latest value", base, 10, []call{
			{0, 10, 10}, {20 * time.Second, 7, 10}, {40 * time.Second, 2, 10},
			{61 * time.Second, 2, 7}, // window holds 7@20s, 2@40s, 2@61s
			{81 * time.Second, 2, 3}, // 7@20s left; max 2, bounded by MaxStep
			{82 * time.Second, 2, 2},
		}},
		{"scale up stays immediate during a down window", base, 5, []call{
			{0, 9, 9}, {10 * time.Second, 3, 9}, {20 * time.Second, 12, 12},
		}},
		{"sample exactly one window old is excluded", base, 8, []call{
			{0, 8, 8}, {time.Minute, 3, 4},
		}},
		{"zero window scales down at once, bounded", with(func(c *Config) { c.ScaleDownWindow = 0 }), 10, []call{
			{0, 1, 6}, {time.Second, 1, 2}, {2 * time.Second, 1, 1},
		}},
		{"negative window is treated as zero", with(func(c *Config) { c.ScaleDownWindow = -time.Minute }), 6, []call{
			{0, 1, 2},
		}},
		{"clamp to Max", with(func(c *Config) { c.Max = 10 }), 8, []call{
			{0, 50, 10}, {time.Second, 50, 10},
		}},
		{"clamp to Min", with(func(c *Config) { c.Min, c.ScaleDownWindow = 2, 0 }), 3, []call{
			{0, 0, 2}, {time.Second, -5, 2},
		}},
		{"initial below Min jumps into range", with(func(c *Config) { c.Min, c.MaxStep = 3, 1 }), 0, []call{
			{0, 0, 3},
		}},
		{"initial above Max jumps into range", with(func(c *Config) { c.Max = 5 }), 9, []call{
			{0, 9, 5},
		}},
		{"MaxStep below 1 is treated as 1", with(func(c *Config) { c.MaxStep = 0 }), 1, []call{
			{0, 5, 2}, {time.Second, 5, 3},
		}},
		{"Min wins when Max < Min", with(func(c *Config) { c.Min, c.Max = 6, 4 }), 6, []call{
			{0, 9, 6}, {time.Second, 1, 6},
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := New(c.cfg, c.initial)
			for i, k := range c.calls {
				if got := s.Next(t0.Add(k.at), k.desired); got != k.want {
					t.Fatalf("call %d (t=%v, desired=%d) = %d, want %d", i, k.at, k.desired, got, k.want)
				}
			}
		})
	}
}

func TestHistoryIsPruned(t *testing.T) {
	cfg := Config{ScaleDownWindow: time.Minute, MaxStep: 4, Min: 1, Max: 20}
	s := New(cfg, 5)
	var now time.Time
	for i := range 40 {
		now = t0.Add(time.Duration(i) * 15 * time.Second)
		s.Next(now, 5+i%3)
	}
	h := s.State().History
	// Strictly newer than now-60s: now-45s, now-30s, now-15s and now.
	if len(h) != 4 {
		t.Fatalf("history has %d samples, want 4: %+v", len(h), h)
	}
	for _, x := range h {
		if !x.Time.After(now.Add(-cfg.ScaleDownWindow)) {
			t.Errorf("sample at %v is outside the window ending %v", x.Time, now)
		}
	}
	if !h[len(h)-1].Time.Equal(now) {
		t.Errorf("latest sample %v, want %v", h[len(h)-1].Time, now)
	}
}

func TestStateRestoreRoundTrip(t *testing.T) {
	cfg := Config{ScaleDownWindow: time.Minute, MaxStep: 2, Min: 1, Max: 12}
	original := New(cfg, 3)
	for i, d := range []int{9, 9, 4, 2, 2} {
		original.Next(t0.Add(time.Duration(i)*10*time.Second), d)
	}
	saved := original.State()
	data, err := json.Marshal(saved)
	if err != nil {
		t.Fatalf("marshal state: %v", err)
	}
	var loaded State
	if err := json.Unmarshal(data, &loaded); err != nil {
		t.Fatalf("unmarshal state: %v", err)
	}
	restored := Restore(cfg, loaded)
	assertSameState(t, original.State(), restored.State())

	// Both must now behave identically, including the remembered high values.
	for i, d := range []int{1, 1, 1, 7, 1, 1, 1, 1} {
		now := t0.Add(time.Minute + time.Duration(i)*10*time.Second)
		a, b := original.Next(now, d), restored.Next(now, d)
		if a != b {
			t.Fatalf("step %d: original %d, restored %d", i, a, b)
		}
	}
	assertSameState(t, original.State(), restored.State())
}

func TestStateIsACopy(t *testing.T) {
	s := New(Config{ScaleDownWindow: time.Minute, MaxStep: 4, Min: 0, Max: 10}, 5)
	s.Next(t0, 5)
	st := s.State()
	st.History[0].Desired = 99
	if got := s.State().History[0].Desired; got != 5 {
		t.Fatalf("mutating a returned State changed the stabilizer: desired = %d", got)
	}
	r := Restore(s.cfg, st)
	st.History[0].Desired = 1
	if got := r.State().History[0].Desired; got != 99 {
		t.Fatalf("mutating the State passed to Restore changed the stabilizer: desired = %d", got)
	}
}

// TestRestoreFutureDatedHistory: a State saved on a host whose clock ran an
// hour ahead must not block scale-down for that hour, only for one window
// from the first call after Restore.
func TestRestoreFutureDatedHistory(t *testing.T) {
	cfg := Config{ScaleDownWindow: time.Minute, MaxStep: 4, Min: 1, Max: 20}
	s := Restore(cfg, State{Current: 10, History: []Sample{{Time: t0.Add(time.Hour), Desired: 10}}})
	if got := s.Next(t0, 1); got != 10 {
		t.Fatalf("first Next = %d, want 10 (the restored sample still protects)", got)
	}
	if h := s.State().History; len(h) != 2 || !h[0].Time.Equal(t0) {
		t.Fatalf("history after first Next = %+v, want the future sample re-dated to %v", h, t0)
	}
	for _, k := range []call{{15 * time.Second, 1, 10}, {45 * time.Second, 1, 10}, {time.Minute, 1, 6}, {75 * time.Second, 1, 2}} {
		if got := s.Next(t0.Add(k.at), k.desired); got != k.want {
			t.Fatalf("Next(t0+%v, %d) = %d, want %d", k.at, k.desired, got, k.want)
		}
	}
}

// TestStartHoldsScaleDownForOneWindow: like the HPA, a stabilizer with no
// history counts its starting value as wanted at the first call, so one low
// sample right after a (re)start cannot shrink the pool and flap it back.
func TestStartHoldsScaleDownForOneWindow(t *testing.T) {
	cfg := Config{ScaleDownWindow: 2 * time.Minute, MaxStep: 4, Min: 1, Max: 10}
	starts := []struct {
		name string
		make func() *Stabilizer
	}{
		{"New", func() *Stabilizer { return New(cfg, 10) }},
		{"Restore with nil history", func() *Stabilizer { return Restore(cfg, State{Current: 10}) }},
		{"Restore with empty history", func() *Stabilizer { return Restore(cfg, State{Current: 10, History: []Sample{}}) }},
		{"Restore of a State saved before the first call", func() *Stabilizer { return Restore(cfg, New(cfg, 10).State()) }},
	}
	for _, st := range starts {
		t.Run(st.name+"/dip then recovery does not flap", func(t *testing.T) {
			s := st.make()
			for _, k := range []call{{0, 2, 10}, {15 * time.Second, 10, 10}} {
				if got := s.Next(t0.Add(k.at), k.desired); got != k.want {
					t.Fatalf("Next(t0+%v, %d) = %d, want %d", k.at, k.desired, got, k.want)
				}
			}
		})
		t.Run(st.name+"/scale-down waits until the seed leaves the window", func(t *testing.T) {
			s := st.make()
			for _, k := range []call{{0, 2, 10}, {time.Minute, 2, 10}, {2*time.Minute - time.Second, 2, 10}, {2 * time.Minute, 2, 6}, {2*time.Minute + 15*time.Second, 2, 2}} {
				if got := s.Next(t0.Add(k.at), k.desired); got != k.want {
					t.Fatalf("Next(t0+%v, %d) = %d, want %d", k.at, k.desired, got, k.want)
				}
			}
		})
	}
	t.Run("seed is recorded at the first call's time", func(t *testing.T) {
		s := New(cfg, 10)
		s.Next(t0, 2)
		assertSameState(t, s.State(), State{Current: 10, History: []Sample{{Time: t0, Desired: 10}, {Time: t0, Desired: 2}}})
	})
	t.Run("restored stale history is not reseeded", func(t *testing.T) {
		// A non-empty history continues as if the stabilizer never stopped;
		// the old sample has aged out, so scale-down starts at once.
		s := Restore(cfg, State{Current: 10, History: []Sample{{Time: t0.Add(-time.Hour), Desired: 10}}})
		if got := s.Next(t0, 2); got != 6 {
			t.Fatalf("Next = %d, want 6 (stale sample pruned, no seed)", got)
		}
	})
}

func TestRestoreUnderNewConfig(t *testing.T) {
	old := New(Config{ScaleDownWindow: time.Minute, MaxStep: 4, Min: 1, Max: 20}, 10)
	old.Next(t0, 10)
	r := Restore(Config{ScaleDownWindow: time.Minute, MaxStep: 4, Min: 1, Max: 6}, old.State())
	if got := r.Next(t0.Add(time.Second), 10); got != 6 {
		t.Fatalf("Next after restoring with Max 6 = %d, want 6", got)
	}
}

// TestNextProperties drives random configurations and desired sequences and
// checks the rules Next promises, against an independent log of samples
// that starts with the initial value seeded at the first call.
func TestNextProperties(t *testing.T) {
	rng := rand.New(rand.NewPCG(7, 11))
	for trial := range 500 {
		lo := rng.IntN(4)
		cfg := Config{
			ScaleDownWindow: time.Duration(rng.IntN(5)) * 15 * time.Second,
			MaxStep:         1 + rng.IntN(5),
			Min:             lo,
			Max:             lo + rng.IntN(15),
		}
		clamp := func(n int) int { return max(cfg.Min, min(cfg.Max, n)) }
		initial := rng.IntN(25)
		prev := initial
		s := New(cfg, initial)
		var log []Sample
		var start time.Time
		now := t0
		for range 60 {
			now = now.Add(time.Duration(1+rng.IntN(20)) * time.Second)
			if log == nil {
				start = now
				log = append(log, Sample{now, initial})
			}
			desired := rng.IntN(25) - 2
			log = append(log, Sample{now, desired})
			got := s.Next(now, desired)
			inRange := prev >= cfg.Min && prev <= cfg.Max
			if now.Before(start.Add(cfg.ScaleDownWindow)) && got < clamp(initial) {
				t.Fatalf("trial %d: %d below the starting value %d within the first window (cfg %+v)",
					trial, got, clamp(initial), cfg)
			}
			windowMax := desired
			for _, x := range log {
				if x.Time.After(now.Add(-cfg.ScaleDownWindow)) {
					windowMax = max(windowMax, x.Desired)
				}
			}
			switch {
			case got < cfg.Min || got > cfg.Max:
				t.Fatalf("trial %d: %d outside [%d, %d]", trial, got, cfg.Min, cfg.Max)
			case inRange && (got-prev > cfg.MaxStep || prev-got > cfg.MaxStep):
				t.Fatalf("trial %d: moved %d -> %d, MaxStep %d", trial, prev, got, cfg.MaxStep)
			case inRange && desired > prev && got < prev:
				t.Fatalf("trial %d: desired %d > %d but scaled down to %d", trial, desired, prev, got)
			case inRange && got > prev && desired <= prev:
				t.Fatalf("trial %d: scaled up %d -> %d without demand (desired %d)", trial, prev, got, desired)
			case inRange && got < prev && got < max(cfg.Min, min(cfg.Max, windowMax)):
				t.Fatalf("trial %d: scaled down to %d below window max %d", trial, got, windowMax)
			}
			// Exact value: up toward desired, otherwise down toward the window
			// max (holding if that is higher), one MaxStep at most, in range.
			target := clamp(min(prev, windowMax))
			if desired > prev {
				target = clamp(desired)
			}
			if want := clamp(prev + max(-cfg.MaxStep, min(cfg.MaxStep, target-prev))); got != want {
				t.Fatalf("trial %d: %d -> %d with desired %d, window max %d; want %d (cfg %+v)",
					trial, prev, got, desired, windowMax, want, cfg)
			}
			prev = got
		}
	}
}

func assertSameState(t *testing.T, a, b State) {
	t.Helper()
	if a.Current != b.Current || len(a.History) != len(b.History) {
		t.Fatalf("states differ: %+v vs %+v", a, b)
	}
	for i := range a.History {
		if !a.History[i].Time.Equal(b.History[i].Time) || a.History[i].Desired != b.History[i].Desired {
			t.Fatalf("history[%d] differs: %+v vs %+v", i, a.History[i], b.History[i])
		}
	}
}
