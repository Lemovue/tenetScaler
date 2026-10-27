package fairq

import (
	"math"
	"math/rand/v2"
	"slices"
	"testing"
	"time"

	"tenantscaler/internal/model"
	"tenantscaler/internal/policy"
)

// Two always-backlogged tenants with weights 1 and 3 receive weighted
// tokens in a 1:3 ratio, with random lengths, 4 requests in flight,
// streamed output and estimate corrections at completion.
func TestFairWeightedVTCTokenShare(t *testing.T) {
	h := newHarness(t, Config{Mode: ModeFair, Seed: 1})
	setPolicy(t, h.s, 1, map[string]policy.TenantPolicy{"A": {Weight: 1}, "B": {Weight: 3}})
	rng := rand.New(rand.NewPCG(42, 7))
	actual := map[*Request]int{}
	queued := map[string]int{}
	served := map[string]float64{}
	var inflight []*Request
	const slots, dispatches = 4, 8000
	for i := range dispatches {
		h.now = t0.Add(time.Duration(i) * 10 * time.Millisecond)
		if len(inflight) == slots {
			r := inflight[0]
			inflight = inflight[1:]
			h.s.OnOutputTokens(r, actual[r]/2)
			h.s.Complete(r, actual[r], h.now)
			served[r.Tenant] += model.WeightedTokens(float64(r.InputTokens), float64(actual[r]))
		}
		for _, tn := range []string{"A", "B"} {
			for queued[tn] < 8 {
				r := h.add(tn, model.PriorityStandard, withTokens(50+rng.IntN(450), 512))
				actual[r] = 10 + rng.IntN(300)
				queued[tn]++
			}
		}
		r := h.next()
		queued[r.Tenant]--
		inflight = append(inflight, r)
	}
	ratio := served["B"] / served["A"]
	t.Logf("B:A weighted-token ratio %.4f over %d dispatches", ratio, dispatches)
	if math.Abs(ratio-3) > 0.02*3 {
		t.Fatalf("B:A weighted-token ratio = %.4f, want 3 +/- 2%% (A=%.0f B=%.0f)", ratio, served["A"], served["B"])
	}
}

// With equal request sizes, weights 1 and 3 give exactly three B
// dispatches per A dispatch.
func TestFairWeightedVTCDispatchPattern(t *testing.T) {
	h := newHarness(t, Config{Mode: ModeFair})
	setPolicy(t, h.s, 1, map[string]policy.TenantPolicy{"A": {Weight: 1}, "B": {Weight: 3}})
	count := map[string]int{}
	for range 5 {
		h.add("A", model.PriorityStandard)
		h.add("B", model.PriorityStandard)
	}
	for range 400 {
		r := h.next()
		h.s.Complete(r, 100, h.now)
		count[r.Tenant]++
		h.add(r.Tenant, model.PriorityStandard)
	}
	if count["A"] < 99 || count["A"] > 101 || count["B"] < 299 || count["B"] > 301 {
		t.Fatalf("dispatches A=%d B=%d, want about 100 and 300", count["A"], count["B"])
	}
}

// warmAB returns a fair scheduler in which A and B have each been served
// 300 weighted tokens per request (A 31 times, B 30 times: counters 9300
// and 9000), each with 5 requests still queued, plus those queued requests.
func warmAB(t *testing.T) (*harness, map[string][]*Request) {
	t.Helper()
	h := newHarness(t, Config{Mode: ModeFair})
	pending := map[string][]*Request{}
	add := func(tn string) { pending[tn] = append(pending[tn], h.add(tn, model.PriorityStandard)) }
	for range 5 {
		add("A")
		add("B")
	}
	for range 61 {
		r := h.next()
		if r != pending[r.Tenant][0] {
			t.Fatalf("dispatched request %d, want FIFO head %d", r.ID, pending[r.Tenant][0].ID)
		}
		pending[r.Tenant] = pending[r.Tenant][1:]
		h.s.Complete(r, 100, h.now)
		add(r.Tenant)
	}
	if a, b := state(t, h.s, "A").Counter, state(t, h.s, "B").Counter; !approx(a, 9300) || !approx(b, 9000) {
		t.Fatalf("warm-up counters A=%v B=%v, want 9300 and 9000", a, b)
	}
	return h, pending
}

// countDispatches dispatches n requests, keeping A and B backlogged, and
// returns how many went to tenant.
func countDispatches(h *harness, n int, tenant string) int {
	got := 0
	for range n {
		r := h.next()
		h.s.Complete(r, 100, h.now)
		if r.Tenant == tenant {
			got++
		} else {
			h.add(r.Tenant, model.PriorityStandard)
		}
	}
	return got
}

func TestFairCounterLift(t *testing.T) {
	t.Run("returning tenant is lifted to the min queued counter", func(t *testing.T) {
		h, _ := warmAB(t)
		minQueued := math.Min(state(t, h.s, "A").Counter, state(t, h.s, "B").Counter)
		for range 30 {
			h.add("C", model.PriorityStandard)
		}
		if c := state(t, h.s, "C").Counter; c != minQueued {
			t.Fatalf("C counter after lift = %v, want min queued counter %v", c, minQueued)
		}
		withLift := countDispatches(h, 30, "C")

		// Counterfactual: least-counter-first without the lift. C keeps the
		// counter of a newcomer (0) and monopolises the pool.
		h2, _ := warmAB(t)
		for range 30 {
			h2.add("C", model.PriorityStandard)
		}
		h2.s.tenants["C"].counter = 0
		withoutLift := countDispatches(h2, 30, "C")

		if withLift != 10 {
			t.Errorf("with lift C got %d of 30 dispatches, want its fair third (10)", withLift)
		}
		if withoutLift != 30 {
			t.Errorf("without lift C got %d of 30 dispatches, want all 30 (monopoly)", withoutLift)
		}
	})

	t.Run("lift never lowers a counter", func(t *testing.T) {
		h, pending := warmAB(t)
		for _, r := range pending["A"] {
			if !h.s.Cancel(r) {
				t.Fatalf("cancel of queued request %d failed", r.ID)
			}
		}
		h.add("A", model.PriorityStandard)
		if a := state(t, h.s, "A").Counter; !approx(a, 9300) {
			t.Fatalf("A counter = %v, want unchanged 9300 (B's 9000 is lower)", a)
		}
	})

	t.Run("no queued work: lift to the last tenant whose queue emptied", func(t *testing.T) {
		h := newHarness(t, Config{Mode: ModeFair})
		for range 3 {
			h.add("A", model.PriorityStandard)
		}
		for _, r := range drain(h) {
			h.s.Complete(r, 100, h.now)
		}
		h.add("B", model.PriorityStandard)
		if b := state(t, h.s, "B").Counter; !approx(b, 900) {
			t.Fatalf("B counter = %v, want A's counter 900", b)
		}
	})

	// The lift follows VTC's waiting queue: only queued work makes a tenant
	// a reference, and a tenant with an empty queue is lifted even while it
	// has requests in flight.
	t.Run("tenant with only in-flight work is not a reference", func(t *testing.T) {
		h := warmXZ(t)
		for range 60 {
			h.add("Y", model.PriorityStandard)
		}
		if y := state(t, h.s, "Y").Counter; !approx(y, 30000) {
			t.Fatalf("Y counter after lift = %v, want backlogged Z's 30000, not in-flight X's 300", y)
		}
		if got := countDispatches(h, 60, "Y"); got != 30 {
			t.Fatalf("Y got %d of 60 dispatches, want its fair half (30)", got)
		}
	})

	t.Run("tenant with in-flight work and an empty queue is lifted", func(t *testing.T) {
		h := warmXZ(t)
		for range 10 {
			h.add("X", model.PriorityStandard)
		}
		if x := state(t, h.s, "X").Counter; !approx(x, 30000) {
			t.Fatalf("X counter after lift = %v, want Z's 30000", x)
		}
		if got := countDispatches(h, 10, "X"); got != 5 {
			t.Fatalf("X got %d of 10 dispatches, want its fair half (5)", got)
		}
	})

	t.Run("no queued work: in-flight counters are not a reference", func(t *testing.T) {
		h := warmXZ(t)
		// Z's queue empties last while X stays in flight at 300. Short
		// outputs then lower Z's counter: the lift reads it at lift time.
		for _, r := range drain(h) {
			h.s.Complete(r, 10, h.now)
		}
		z := state(t, h.s, "Z").Counter
		if !approx(z, 30000+5*300-5*2*90) {
			t.Fatalf("Z counter = %v, want 30600", z)
		}
		h.add("Y", model.PriorityStandard)
		if y := state(t, h.s, "Y").Counter; !approx(y, z) {
			t.Fatalf("Y counter after lift = %v, want Z's current counter %v (X's in-flight 300 is lower)", y, z)
		}
	})
}

// warmXZ returns a fair scheduler in which X has one request in flight and
// nothing queued (counter 300), while Z, kept backlogged with 5 queued
// requests, has been served 99 requests since it was lifted to X's 300
// (counter 30000). Every request is charged 300 weighted tokens.
func warmXZ(t *testing.T) *harness {
	t.Helper()
	h := newHarness(t, Config{Mode: ModeFair})
	x := h.add("X", model.PriorityStandard)
	if r := h.next(); r != x {
		t.Fatalf("dispatched request %d, want X's %d", r.ID, x.ID)
	}
	for range 5 {
		h.add("Z", model.PriorityStandard)
	}
	for range 99 {
		r := h.next()
		h.s.Complete(r, 100, h.now)
		h.add(r.Tenant, model.PriorityStandard)
	}
	xs, zs := state(t, h.s, "X"), state(t, h.s, "Z")
	if xs.InFlight != 1 || xs.Queued != 0 || !approx(xs.Counter, 300) || zs.Queued != 5 || !approx(zs.Counter, 30000) {
		t.Fatalf("warm-up: X=%+v Z=%+v, want X 1 in flight at 300 and Z 5 queued at 30000", xs, zs)
	}
	return h
}

// Inside a tenant: highest effective priority, then earliest deadline (no
// deadline last), then lowest ID.
func TestFairOrderingInsideTenant(t *testing.T) {
	h := newHarness(t, Config{Mode: ModeFair})
	specs := []struct {
		p        model.Priority
		deadline time.Duration // 0 = none
	}{
		{model.PriorityStandard, 5 * time.Second},         // 1
		{model.PriorityStandard, time.Second},             // 2
		{model.PriorityStandard, 0},                       // 3
		{model.PriorityCritical, 9 * time.Second},         // 4
		{model.PrioritySheddable, 500 * time.Millisecond}, // 5
		{model.PriorityStandard, time.Second},             // 6: ties with 2
		{model.Priority(5), 0},                            // 7: above PriorityCritical
	}
	for _, sp := range specs {
		if sp.deadline > 0 {
			h.add("A", sp.p, withDeadline(sp.deadline))
		} else {
			h.add("A", sp.p)
		}
	}
	got := ids(drain(h))
	want := []uint64{7, 4, 2, 6, 1, 3, 5}
	if !slices.Equal(got, want) {
		t.Fatalf("dispatch order = %v, want %v", got, want)
	}
}

func TestFairChargeCorrections(t *testing.T) {
	h := newHarness(t, Config{Mode: ModeFair})
	setPolicy(t, h.s, 1, map[string]policy.TenantPolicy{"A": {Weight: 2}})
	counter := func() float64 { return state(t, h.s, "A").Counter }
	check := func(step string, want float64) {
		t.Helper()
		if got := counter(); !approx(got, want) {
			t.Fatalf("%s: counter = %v, want %v", step, got, want)
		}
	}

	r1 := h.add("A", model.PriorityStandard, withTokens(10, 0)) // unbounded output
	if h.next() != r1 || r1.EstOutputTokens != 128 {
		t.Fatalf("r1 estimate = %v, want the default 128", r1.EstOutputTokens)
	}
	check("dispatch charges (10+2*128)/2", 133)
	h.s.OnOutputTokens(r1, 100)
	check("output below the estimate charges nothing", 133)
	h.s.OnOutputTokens(r1, 150)
	check("excess 22 tokens charged 2*22/2", 155)
	h.s.OnOutputTokens(r1, 140)
	check("non-monotonic report ignored", 155)
	setPolicy(t, h.s, 2, map[string]policy.TenantPolicy{"A": {Weight: 4}})
	h.s.Complete(r1, 160, h.now)
	check("completion charges the last 10 tokens at the dispatch weight 2", 165)
	h.s.Complete(r1, 999, h.now)
	h.s.OnOutputTokens(r1, 999)
	check("completed request is not charged again", 165)
	if st := state(t, h.s, "A"); st.InFlight != 0 || !approx(st.EstOutputTokens, 134.4) {
		t.Fatalf("after r1: InFlight=%d estimate=%v, want 0 and EWMA 0.8*128+0.2*160=134.4", st.InFlight, st.EstOutputTokens)
	}

	r2 := h.add("A", model.PriorityStandard, withTokens(10, 1000))
	h.next()
	check("dispatch at weight 4 charges (10+2*134.4)/4", 165+69.7)
	h.s.Complete(r2, 20, h.now)
	check("short output lowers the counter to the net (10+2*20)/4", 165+12.5)

	r3 := h.add("A", model.PriorityStandard, withTokens(10, 50))
	h.next()
	if r3.EstOutputTokens != 50 {
		t.Fatalf("r3 estimate = %v, want MaxOutputTokens 50", r3.EstOutputTokens)
	}
}

func TestCancel(t *testing.T) {
	h := newHarness(t, Config{Mode: ModeFair})
	setPolicy(t, h.s, 1, map[string]policy.TenantPolicy{"A": {Weight: 1, CriticalSlots: 5}})
	a := h.add("A", model.PriorityCritical)
	b := h.add("A", model.PriorityStandard)
	c := h.add("A", model.PriorityStandard)
	d := h.add("A", model.PriorityCritical)
	if st := state(t, h.s, "A"); st.CriticalActive != 2 || st.Queued != 4 {
		t.Fatalf("before cancel: %+v", st)
	}
	if !h.s.Cancel(b) || !h.s.Cancel(d) {
		t.Fatal("cancel of a queued request failed")
	}
	if h.s.Cancel(b) {
		t.Fatal("second cancel of the same request succeeded")
	}
	if st := state(t, h.s, "A"); st.CriticalActive != 1 || st.Queued != 2 || h.s.Len() != 2 {
		t.Fatalf("after cancel: %+v Len=%d", st, h.s.Len())
	}
	if got := h.next(); got != a {
		t.Fatalf("Next = %d, want critical %d", got.ID, a.ID)
	}
	if h.s.Cancel(a) {
		t.Fatal("cancel of an in-flight request succeeded")
	}
	other := New(Config{})
	if other.Cancel(c) {
		t.Fatal("another scheduler cancelled a request it does not own")
	}
	if got := ids(drain(h)); !slices.Equal(got, []uint64{c.ID}) {
		t.Fatalf("remaining dispatches = %v, want [%d]", got, c.ID)
	}
}

// Slot reservation: a tenant over its share (Shed) may not take the slots
// another active tenant's unused share needs; tenants within their share,
// and tenants that are not shedding, are never held back; Next (no slot
// count) ignores reservations.
func TestNextWithinReservesShares(t *testing.T) {
	h := newHarness(t, Config{Mode: ModeFair})
	setPolicy(t, h.s, 1, map[string]policy.TenantPolicy{
		"A": {Weight: 1, Slots: 2, GuaranteedSlots: 2},
		"C": {Weight: 2, Shed: true, Slots: 1, GuaranteedSlots: 1},
	})
	a1 := h.add("A", model.PriorityStandard)
	if got := h.s.NextWithin(h.now, 4); got != a1 {
		t.Fatalf("first dispatch = %v, want A's request", got)
	}
	c1 := h.add("C", model.PriorityStandard)
	c2 := h.add("C", model.PriorityStandard)
	if got := h.s.NextWithin(h.now, 3); got != c1 {
		t.Fatal("C within its share of 1 must be dispatched")
	}
	// C is at its share; A (active, 1 in flight) has 1 unused slot.
	if got := h.s.NextWithin(h.now, 1); got != nil {
		t.Fatalf("C beyond its share took A's reserved slot: %v", got)
	}
	if got := h.s.NextWithin(h.now, 2); got != c2 {
		t.Fatal("C beyond its share should get a slot that leaves A's reserve free")
	}
	// A is never held back within its share, nor is a tenant that does not shed.
	c3 := h.add("C", model.PriorityStandard)
	a2 := h.add("A", model.PriorityStandard)
	if got := h.s.NextWithin(h.now, 1); got != a2 {
		t.Fatalf("A within its share: got %v, want A's request", got)
	}
	setPolicy(t, h.s, 2, map[string]policy.TenantPolicy{"A": {Weight: 1, Slots: 4}, "C": {Weight: 2, Slots: 1}})
	if got := h.s.NextWithin(h.now, 1); got != c3 {
		t.Fatal("a tenant that is not shedding must not be held back")
	}
	setPolicy(t, h.s, 3, map[string]policy.TenantPolicy{"A": {Weight: 1, Slots: 4}, "C": {Weight: 2, Shed: true}})
	c4 := h.add("C", model.PriorityStandard)
	if got := h.s.NextWithin(h.now, 1); got != nil {
		t.Fatal("C must leave A's unused share free")
	}
	if got := h.s.Next(h.now); got != c4 {
		t.Fatal("Next must ignore reservations")
	}
}
