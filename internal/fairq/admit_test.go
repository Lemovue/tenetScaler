package fairq

import (
	"math"
	"slices"
	"testing"
	"time"

	"tenantscaler/internal/model"
	"tenantscaler/internal/policy"
)

func TestFairCriticalQuota(t *testing.T) {
	h := newHarness(t, Config{Mode: ModeFair})
	setPolicy(t, h.s, 1, map[string]policy.TenantPolicy{"A": {Weight: 1, CriticalSlots: 2}})
	c1 := h.add("A", model.PriorityCritical)
	c2 := h.add("A", model.PriorityCritical)
	c3 := h.add("A", model.PriorityCritical)
	for _, tc := range []struct {
		r    *Request
		want model.Priority
	}{{c1, model.PriorityCritical}, {c2, model.PriorityCritical}, {c3, model.PriorityStandard}} {
		if tc.r.EffectivePriority != tc.want {
			t.Errorf("request %d effective priority = %v, want %v", tc.r.ID, tc.r.EffectivePriority, tc.want)
		}
	}
	if st := state(t, h.s, "A"); st.CriticalActive != 2 {
		t.Fatalf("CriticalActive = %d, want 2", st.CriticalActive)
	}
	if h.next() != c1 {
		t.Fatal("first dispatch is not the oldest critical request")
	}
	if c4 := h.add("A", model.PriorityCritical); c4.EffectivePriority != model.PriorityStandard {
		t.Fatal("critical request admitted as critical while the quota is held by queued+in-flight requests")
	}
	h.s.Complete(c1, 10, h.now)
	if st := state(t, h.s, "A"); st.CriticalActive != 1 {
		t.Fatalf("CriticalActive after completion = %d, want 1", st.CriticalActive)
	}
	if c5 := h.add("A", model.PriorityCritical); c5.EffectivePriority != model.PriorityCritical {
		t.Fatal("critical request downgraded although a critical slot was free")
	}
	// No CriticalSlots (B is not in the policy): no cap.
	for range 5 {
		if r := h.add("B", model.PriorityCritical); r.EffectivePriority != model.PriorityCritical {
			t.Fatal("critical request downgraded without a quota")
		}
	}
}

// A critical request downgraded by the quota is scheduled as standard: it
// is ordered by deadline among the standard requests and behind a genuine
// critical request admitted after it.
func TestFairDowngradedCriticalIsScheduledAsStandard(t *testing.T) {
	h := newHarness(t, Config{Mode: ModeFair})
	setPolicy(t, h.s, 1, map[string]policy.TenantPolicy{"A": {Weight: 1, CriticalSlots: 1}})
	c1 := h.add("A", model.PriorityCritical, withDeadline(9*time.Second))
	c2 := h.add("A", model.PriorityCritical, withDeadline(5*time.Second)) // over the quota
	std := h.add("A", model.PriorityStandard, withDeadline(2*time.Second))
	if c2.EffectivePriority != model.PriorityStandard {
		t.Fatalf("c2 effective priority = %v, want standard (the one slot is held by c1)", c2.EffectivePriority)
	}
	if got := h.next(); got != c1 {
		t.Fatalf("first dispatch = %d, want critical c1 (%d) ahead of the downgraded c2 despite its later deadline", got.ID, c1.ID)
	}
	h.s.Complete(c1, 10, h.now)
	c3 := h.add("A", model.PriorityCritical, withDeadline(9*time.Second))
	if c3.EffectivePriority != model.PriorityCritical {
		t.Fatal("critical request downgraded although c1 released the only slot")
	}
	got := ids(drain(h))
	if want := []uint64{c3.ID, std.ID, c2.ID}; !slices.Equal(got, want) {
		t.Fatalf("dispatch order = %v, want %v: genuine critical, then standard by deadline with the downgraded c2 last", got, want)
	}
}

func TestFairOverShareShedding(t *testing.T) {
	cases := []struct {
		tenant string
		p      model.Priority
		reason string // "" = admitted
	}{
		{"A", model.PrioritySheddable, ReasonOverShare},
		{"A", model.PriorityStandard, ""},
		{"A", model.PriorityCritical, ""},
		{"B", model.PrioritySheddable, ""}, // B is not shedding
	}
	h := newHarness(t, Config{Mode: ModeFair, Seed: 3})
	setPolicy(t, h.s, 1, map[string]policy.TenantPolicy{
		"A": {Weight: 1, Shed: true, RetryAfterSeconds: 2},
		"B": {Weight: 1},
	})
	for _, tc := range cases {
		v := h.s.Admit(h.req(tc.tenant, tc.p), h.now, true) // no free slot
		if v.Admitted != (tc.reason == "") || v.Reason != tc.reason {
			t.Errorf("%s %s: verdict %+v, want reason %q", tc.tenant, tc.p.Class(), v, tc.reason)
		}
		if !v.Admitted && (v.RetryAfter < 2*time.Second || v.RetryAfter >= 3*time.Second) {
			t.Errorf("RetryAfter = %v, want in [2s, 3s)", v.RetryAfter)
		}
	}
	st := state(t, h.s, "A")
	if st.Rejected[ReasonOverShare] != 1 || st.Admitted != 2 || !st.Shed {
		t.Fatalf("A state = %+v, want 1 over-share rejection, 2 admitted, Shed", st)
	}
}

// Over-share shedding is work-conserving: with a free slot and nothing
// queued, a shedding tenant's sheddable request is served at once; it is
// shed while anything is queued or no slot is free.
func TestFairOverShareOnlyUnderContention(t *testing.T) {
	h := newHarness(t, Config{Mode: ModeFair})
	setPolicy(t, h.s, 1, map[string]policy.TenantPolicy{"A": {Weight: 1, Shed: true}, "B": {Weight: 1}})
	if v := h.s.Admit(h.req("A", model.PrioritySheddable), h.now, false); !v.Admitted {
		t.Fatalf("free slot, empty queue: verdict %+v, want admitted", v)
	}
	h.next()
	h.add("B", model.PriorityStandard)
	if v := h.s.Admit(h.req("A", model.PrioritySheddable), h.now, false); v.Admitted || v.Reason != ReasonOverShare {
		t.Fatalf("B queued: verdict %+v, want over-share", v)
	}
	h.next()
	if v := h.s.Admit(h.req("A", model.PrioritySheddable), h.now, true); v.Admitted || v.Reason != ReasonOverShare {
		t.Fatalf("saturated: verdict %+v, want over-share", v)
	}
	if st := state(t, h.s, "A"); st.Admitted != 1 || st.Rejected[ReasonOverShare] != 2 {
		t.Fatalf("A state = %+v, want 1 admitted, 2 over-share rejections", st)
	}
}

// deadlineSetup returns a fair scheduler whose tenant A dispatched one
// request per second at t0..t0+4s and has 2 requests queued, at t0+5s.
// Its dispatch rate is then 5/10 s = 0.5/s, so the predicted wait of a new
// request is (2+1)/0.5 = 6 s.
func deadlineSetup(t *testing.T) *harness {
	t.Helper()
	h := newHarness(t, Config{Mode: ModeFair})
	for range 5 {
		h.add("A", model.PriorityStandard)
	}
	for i := range 5 {
		h.now = t0.Add(time.Duration(i) * time.Second)
		h.s.Complete(h.next(), 10, h.now)
	}
	h.now = t0.Add(5 * time.Second)
	h.add("A", model.PriorityStandard)
	h.add("A", model.PriorityStandard)
	return h
}

func TestFairDeadlineUnreachable(t *testing.T) {
	cases := []struct {
		name     string
		at       time.Duration // offset from t0 of the admission
		p        model.Priority
		deadline time.Duration // relative to the admission time; 0 = none
		reason   string
	}{
		{"wait 6s exceeds 5s budget", 5 * time.Second, model.PrioritySheddable, 5 * time.Second, ReasonDeadlineUnreachable},
		{"wait 6s equals 6s budget", 5 * time.Second, model.PrioritySheddable, 6 * time.Second, ""},
		{"deadline already passed", 5 * time.Second, model.PrioritySheddable, -time.Second, ReasonDeadlineUnreachable},
		{"standard is never shed", 5 * time.Second, model.PriorityStandard, time.Second, ""},
		{"no deadline", 5 * time.Second, model.PrioritySheddable, 0, ""},
		// At t0+13.5s only the t0+4s dispatch is in the window: rate 0.1/s,
		// predicted wait 30 s.
		{"one dispatch left in window", 13500 * time.Millisecond, model.PrioritySheddable, 29 * time.Second, ReasonDeadlineUnreachable},
		// At t0+14s the t0+4s dispatch sits exactly on the window edge and
		// is excluded: the rate is 0 and the check is skipped.
		{"window empty: check skipped", 14 * time.Second, model.PrioritySheddable, time.Millisecond, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := deadlineSetup(t)
			h.now = t0.Add(tc.at)
			r := h.req("A", tc.p)
			if tc.deadline != 0 {
				r.Deadline = h.now.Add(tc.deadline)
			}
			v := h.s.Admit(r, h.now, false)
			if v.Admitted != (tc.reason == "") || v.Reason != tc.reason {
				t.Fatalf("verdict %+v, want reason %q", v, tc.reason)
			}
		})
	}
}

// The deadline check needs a backlog. With nothing queued in its tenant a
// request goes to the next free slot, and the dispatch rate of a tenant that
// is not backlogged measures its demand, not the service it would get. A
// tenant sending one sheddable request every 3 s with a 2 s deadline, each
// dispatched and completed at once, is always admitted, although its rate
// is non-zero and (0+1)/rate >= 3.3 s exceeds the deadline.
func TestFairDeadlineCheckNeedsBacklog(t *testing.T) {
	h := newHarness(t, Config{Mode: ModeFair})
	for i := range 40 {
		h.now = t0.Add(time.Duration(i) * 3 * time.Second)
		r := h.add("A", model.PrioritySheddable, withDeadline(2*time.Second))
		if got := h.next(); got != r {
			t.Fatalf("dispatched request %d, want %d", got.ID, r.ID)
		}
		h.s.Complete(r, 10, h.now)
	}
	if st := state(t, h.s, "A"); st.Admitted != 40 {
		t.Fatalf("A admitted %d of 40 sheddable requests (rejections %v)", st.Admitted, st.Rejected)
	}
}

func TestQueueFull(t *testing.T) {
	for _, mode := range []Mode{ModeFair, ModeRoundRobin, ModeFCFS} {
		t.Run(string(mode), func(t *testing.T) {
			h := newHarness(t, Config{Mode: mode, MaxQueuePerTenant: 2})
			h.add("A", model.PriorityStandard)
			h.add("A", model.PriorityStandard)
			for _, p := range []model.Priority{model.PriorityStandard, model.PrioritySheddable} {
				v := h.s.Admit(h.req("A", p), h.now, false)
				if v.Admitted || v.Reason != ReasonQueueFull || v.RetryAfter < time.Second {
					t.Fatalf("%s request at a full queue: verdict %+v, want queue-full", p.Class(), v)
				}
			}
			h.add("A", model.PriorityCritical) // critical bypasses the bound
			h.add("B", model.PriorityStandard) // bound is per tenant
			if st := state(t, h.s, "A"); st.Queued != 3 || st.Rejected[ReasonQueueFull] != 2 {
				t.Fatalf("A state = %+v, want 3 queued and 2 queue-full rejections", st)
			}
			drain(h)
			h.add("A", model.PriorityStandard) // dispatching frees queue room
		})
	}
	t.Run("downgraded critical is bounded", func(t *testing.T) {
		h := newHarness(t, Config{Mode: ModeFair, MaxQueuePerTenant: 1})
		setPolicy(t, h.s, 1, map[string]policy.TenantPolicy{"A": {Weight: 1, CriticalSlots: 1}})
		h.add("A", model.PriorityCritical)
		v := h.s.Admit(h.req("A", model.PriorityCritical), h.now, false)
		if v.Admitted || v.Reason != ReasonQueueFull {
			t.Fatalf("downgraded critical at a full queue: verdict %+v, want queue-full", v)
		}
	})
}

// rejections returns the Retry-After of n over-share rejections (the pool
// is saturated, so every one is shed).
func rejections(seed int64, jitter float64, retrySeconds, n int) []time.Duration {
	s := New(Config{Mode: ModeFair, Seed: seed, RetryJitter: jitter})
	s.SetPolicy(policy.Policy{Version: 1, Tenants: map[string]policy.TenantPolicy{
		"A": {Weight: 1, Shed: true, RetryAfterSeconds: retrySeconds},
	}})
	out := make([]time.Duration, n)
	for i := range out {
		out[i] = s.Admit(&Request{ID: uint64(i + 1), Tenant: "A", Priority: model.PrioritySheddable}, t0, true).RetryAfter
	}
	return out
}

func TestRetryAfterJitter(t *testing.T) {
	cases := []struct {
		name         string
		jitter       float64
		retrySeconds int
		lo, hi       time.Duration // want every value in [lo, hi)
	}{
		{"default jitter 0.5", 0, 3, 3 * time.Second, 4500 * time.Millisecond},
		{"base below 1s is raised to 1s", 0, 0, time.Second, 1500 * time.Millisecond},
		{"jitter 0.2", 0.2, 10, 10 * time.Second, 12 * time.Second},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := rejections(11, tc.jitter, tc.retrySeconds, 500)
			lo, hi := slices.Min(got), slices.Max(got)
			if lo < tc.lo || hi >= tc.hi {
				t.Fatalf("RetryAfter range [%v, %v], want within [%v, %v)", lo, hi, tc.lo, tc.hi)
			}
			// The jitter really spreads the values over most of the range.
			if spread := float64(hi-lo) / float64(tc.hi-tc.lo); spread < 0.9 {
				t.Fatalf("RetryAfter spread %.2f of the jitter range, want >= 0.9", spread)
			}
		})
	}
	t.Run("negative jitter disables it", func(t *testing.T) {
		for _, d := range rejections(11, -1, 2, 20) {
			if d != 2*time.Second {
				t.Fatalf("RetryAfter = %v, want exactly 2s", d)
			}
		}
	})
	t.Run("deterministic per seed", func(t *testing.T) {
		a, b, c := rejections(7, 0, 2, 50), rejections(7, 0, 2, 50), rejections(8, 0, 2, 50)
		if !slices.Equal(a, b) {
			t.Fatal("same seed produced different Retry-After sequences")
		}
		if slices.Equal(a, c) {
			t.Fatal("different seeds produced identical Retry-After sequences")
		}
	})
}

func TestSetPolicyVersioning(t *testing.T) {
	s := New(Config{})
	if p := s.Policy(); p.Version != 0 || len(p.Tenants) != 0 {
		t.Fatalf("initial policy = %+v, want empty version 0", p)
	}
	steps := []struct {
		name    string
		p       policy.Policy
		ok      bool
		version int64
		weightA float64
	}{
		{"equal policy at version 0", policy.Equal([]string{"A", "B"}), true, 0, 1},
		{"newer version", policy.Policy{Version: 2, Tenants: map[string]policy.TenantPolicy{"A": {Weight: 2}}}, true, 2, 2},
		{"older version ignored", policy.Policy{Version: 1, Tenants: map[string]policy.TenantPolicy{"A": {Weight: 7}}}, false, 2, 2},
		{"same version resent", policy.Policy{Version: 2, Tenants: map[string]policy.TenantPolicy{"A": {Weight: 3}}}, true, 2, 3},
		{"NaN weight rejected", policy.Policy{Version: 3, Tenants: map[string]policy.TenantPolicy{"A": {Weight: math.NaN()}}}, false, 2, 3},
		{"Inf weight rejected", policy.Policy{Version: 3, Tenants: map[string]policy.TenantPolicy{"A": {Weight: math.Inf(1)}}}, false, 2, 3},
	}
	for _, st := range steps {
		if got := s.SetPolicy(st.p); got != st.ok {
			t.Fatalf("%s: SetPolicy = %v, want %v", st.name, got, st.ok)
		}
		if p := s.Policy(); p.Version != st.version || p.WeightFor("A") != st.weightA {
			t.Fatalf("%s: policy v%d weight(A)=%v, want v%d weight %v", st.name, p.Version, p.WeightFor("A"), st.version, st.weightA)
		}
	}
	// The scheduler keeps its own copy of the tenant map, both ways.
	mine := map[string]policy.TenantPolicy{"A": {Weight: 5}}
	setPolicy(t, s, 10, mine)
	mine["A"] = policy.TenantPolicy{Weight: 99}
	p := s.Policy()
	p.Tenants["A"] = policy.TenantPolicy{Weight: 98}
	if w := s.Policy().WeightFor("A"); w != 5 {
		t.Fatalf("weight(A) = %v after mutating caller maps, want 5", w)
	}
}

func TestSnapshot(t *testing.T) {
	h := newHarness(t, Config{Mode: ModeFair})
	setPolicy(t, h.s, 1, map[string]policy.TenantPolicy{"m": {Weight: 4, Shed: true}, "a": {Weight: 1}})
	h.add("z", model.PriorityStandard)
	h.s.Admit(h.req("m", model.PrioritySheddable), h.now, false)
	snap := h.s.Snapshot()
	var names []string
	for _, ts := range snap {
		names = append(names, ts.Tenant)
	}
	if !slices.Equal(names, []string{"a", "m", "z"}) {
		t.Fatalf("snapshot tenants = %v, want sorted [a m z] including policy-only tenants", names)
	}
	m := snap[1]
	if m.Weight != 4 || !m.Shed || m.Rejected[ReasonOverShare] != 1 || m.EstOutputTokens != DefaultOutputEstimate {
		t.Fatalf("m = %+v", m)
	}
	m.Rejected[ReasonOverShare] = 50
	if got := state(t, h.s, "m").Rejected[ReasonOverShare]; got != 1 {
		t.Fatalf("snapshot map aliases scheduler state: got %d", got)
	}
	if z := snap[2]; z.Weight != 1 || z.Queued != 1 || z.Admitted != 1 {
		t.Fatalf("z = %+v, want weight 1 (not in policy), 1 queued, 1 admitted", z)
	}
}

func TestConfigDefaultsAndParseMode(t *testing.T) {
	got := New(Config{Mode: "lifo", RetryJitter: math.NaN()}).Config()
	want := Config{Mode: ModeFair, MaxQueuePerTenant: 1000, RetryJitter: 0.5, DefaultOutputTokens: 128}
	if got != want {
		t.Fatalf("defaults = %+v, want %+v", got, want)
	}
	for _, tc := range []struct {
		in   string
		want Mode
		ok   bool
	}{
		{"fair", ModeFair, true},
		{" RoundRobin ", ModeRoundRobin, true},
		{"fcfs", ModeFCFS, true},
		{"", "", false},
		{"lifo", "", false},
	} {
		m, err := ParseMode(tc.in)
		if m != tc.want || (err == nil) != tc.ok {
			t.Errorf("ParseMode(%q) = %q, %v; want %q ok=%v", tc.in, m, err, tc.want, tc.ok)
		}
	}
}
