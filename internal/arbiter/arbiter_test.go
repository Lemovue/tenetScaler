package arbiter

import (
	"errors"
	"math"
	"reflect"
	"slices"
	"strings"
	"testing"

	"tenantscaler/internal/model"
)

// workedExample is the problem statement's pool: capacity 10, guarantees
// 3/3/3, and B closer to violating its SLO than C.
func workedExample(dA, dB, dC int) []Input {
	return []Input{
		{Name: "A", Demand: dA, Guaranteed: 3, MaxReplicas: 8, Priority: 100, SLOSlack: 0.5},
		{Name: "B", Demand: dB, Guaranteed: 3, MaxReplicas: 8, Priority: 100, SLOSlack: 0.05},
		{Name: "C", Demand: dC, Guaranteed: 3, MaxReplicas: 8, Priority: 100, SLOSlack: 0.10},
	}
}

func mustArbitrate(t *testing.T, capacity int, in []Input, opt Options) []Result {
	t.Helper()
	res, err := Arbitrate(capacity, in, opt)
	if err != nil {
		t.Fatalf("Arbitrate: %v", err)
	}
	return res
}

func grants(res []Result) []int {
	out := make([]int, len(res))
	for i, r := range res {
		out[i] = r.Grant
	}
	return out
}

func TestWorkedExampleBorrow(t *testing.T) {
	res := mustArbitrate(t, 10, workedExample(2, 4, 8), Options{Borrowing: true})
	want := []Result{
		{
			Name: "A", Grant: 2,
			Steps:  []model.DecisionStep{{Rule: RuleGuarantee, Grant: 2, Note: "demand 2 is within the guarantee of 3"}},
			Reason: "A got 2 replicas: its full demand, within its guarantee of 3.",
		},
		{
			Name: "B", Grant: 4,
			Steps: []model.DecisionStep{
				{Rule: RuleGuarantee, Grant: 3, Note: "demand 4 exceeds the guarantee of 3"},
				{Rule: RuleBorrow, Grant: 4, Note: "spare replica 1 of 2 (round 1): lower SLO slack than C (0.05 < 0.10)"},
			},
			Reason: "B got 4 replicas: guarantee 3 plus 1 borrowed, its full demand.",
		},
		{
			Name: "C", Grant: 4, Shedding: true,
			Steps: []model.DecisionStep{
				{Rule: RuleGuarantee, Grant: 3, Note: "demand 8 exceeds the guarantee of 3"},
				{Rule: RuleBorrow, Grant: 4, Note: "spare replica 2 of 2 (round 1): after B, which has lower SLO slack than C (0.05 < 0.10)"},
			},
			Reason: "C got 4 replicas: guarantee 3 plus 1 borrowed. It wanted 8, so its sheddable requests get 429 until capacity frees up.",
		},
	}
	if !reflect.DeepEqual(res, want) {
		t.Fatalf("got\n%#v\nwant\n%#v", res, want)
	}
}

// TestWorkedExampleReclaim is the second half of the worked example: A's
// demand rises to 3 and it gets its guarantee back from C on the next cycle,
// with no reclaim-specific code.
func TestWorkedExampleReclaim(t *testing.T) {
	opt := Options{Borrowing: true}
	if got := grants(mustArbitrate(t, 10, workedExample(2, 4, 8), opt)); !slices.Equal(got, []int{2, 4, 4}) {
		t.Fatalf("cycle 1 grants = %v, want [2 4 4]", got)
	}
	res := mustArbitrate(t, 10, workedExample(3, 4, 8), opt)
	if got := grants(res); !slices.Equal(got, []int{3, 4, 3}) {
		t.Fatalf("cycle 2 grants = %v, want [3 4 3]", got)
	}
	if res[0].Shedding || res[1].Shedding || !res[2].Shedding {
		t.Errorf("shedding = %v/%v/%v, want only C", res[0].Shedding, res[1].Shedding, res[2].Shedding)
	}
	wantB := "spare replica 1 of 1 (round 1): lower SLO slack than C (0.05 < 0.10)"
	if n := len(res[1].Steps); n != 2 || res[1].Steps[1].Note != wantB {
		t.Errorf("B steps = %+v, want a single borrow noted %q", res[1].Steps, wantB)
	}
	if n := len(res[2].Steps); n != 1 {
		t.Errorf("C borrowed in cycle 2: %+v", res[2].Steps)
	}
	wantC := "C got 3 replicas: its guarantee of 3; no spare capacity was left to borrow. It wanted 8, so its sheddable requests get 429 until capacity frees up."
	if res[2].Reason != wantC {
		t.Errorf("C reason = %q, want %q", res[2].Reason, wantC)
	}
}

// TestIdleGuaranteeIsLentAndReclaimed: an idle tenant's guarantee is lent
// out, and comes back in full the cycle its demand returns.
func TestIdleGuaranteeIsLentAndReclaimed(t *testing.T) {
	opt := Options{Borrowing: true}
	idle := mustArbitrate(t, 10, workedExample(0, 4, 8), opt)
	if got := grants(idle); !slices.Equal(got, []int{0, 4, 6}) {
		t.Fatalf("idle grants = %v, want [0 4 6]", got)
	}
	wantA := "A got 0 replicas: no demand this cycle, so its guarantee of 3 is free for others to borrow until it needs it back."
	if idle[0].Reason != wantA {
		t.Errorf("A reason = %q, want %q", idle[0].Reason, wantA)
	}
	back := mustArbitrate(t, 10, workedExample(3, 4, 8), opt)
	if got := grants(back); !slices.Equal(got, []int{3, 4, 3}) {
		t.Fatalf("return grants = %v, want [3 4 3]", got)
	}
}

func TestBorrowingDisabled(t *testing.T) {
	res := mustArbitrate(t, 10, workedExample(2, 4, 8), Options{Borrowing: false})
	if got := grants(res); !slices.Equal(got, []int{2, 3, 3}) {
		t.Fatalf("grants = %v, want [2 3 3]", got)
	}
	for _, r := range res {
		if len(r.Steps) != 1 || r.Steps[0].Rule != RuleGuarantee {
			t.Errorf("%s steps = %+v, want only the guarantee step", r.Name, r.Steps)
		}
	}
	if res[0].Shedding || !res[1].Shedding || !res[2].Shedding {
		t.Errorf("shedding = %v/%v/%v, want B and C", res[0].Shedding, res[1].Shedding, res[2].Shedding)
	}
	want := "C got 3 replicas: its guarantee of 3 (borrowing is disabled). It wanted 8, so its sheddable requests get 429 until its demand drops to its guarantee."
	if res[2].Reason != want {
		t.Errorf("C reason = %q, want %q", res[2].Reason, want)
	}

	// An idle tenant's guarantee is not lent out in the ablation, and its
	// reason must not claim it is.
	idle := mustArbitrate(t, 10, workedExample(0, 4, 8), Options{Borrowing: false})
	if got := grants(idle); !slices.Equal(got, []int{0, 3, 3}) {
		t.Fatalf("idle grants = %v, want [0 3 3]", got)
	}
	wantA := "A got 0 replicas: no demand this cycle, and its guarantee of 3 sits idle (borrowing is disabled)."
	if idle[0].Reason != wantA {
		t.Errorf("A reason = %q, want %q", idle[0].Reason, wantA)
	}
}

func TestLendingOrder(t *testing.T) {
	cases := []struct {
		name      string
		in        []Input
		capacity  int
		want      []int
		winner    int
		wantNotes string
	}{
		{
			name: "tie broken by name regardless of input order",
			in: []Input{
				{Name: "c", Demand: 1, MaxReplicas: 1, Priority: 1, SLOSlack: 0.2},
				{Name: "a", Demand: 1, MaxReplicas: 1, Priority: 1, SLOSlack: 0.2},
				{Name: "b", Demand: 1, MaxReplicas: 1, Priority: 1, SLOSlack: 0.2},
			},
			capacity: 1, want: []int{0, 1, 0}, winner: 1,
			wantNotes: "spare replica 1 of 1 (round 1): the same priority and SLO slack as b, and a sorts first by name",
		},
		{
			name: "priority beats slack",
			in: []Input{
				{Name: "Y", Demand: 2, MaxReplicas: 2, Priority: 100, SLOSlack: -1},
				{Name: "X", Demand: 2, MaxReplicas: 2, Priority: 200, SLOSlack: 0.9},
			},
			capacity: 1, want: []int{0, 1}, winner: 1,
			wantNotes: "spare replica 1 of 1 (round 1): higher priority than Y (200 > 100)",
		},
		{
			name: "slack beats name",
			in: []Input{
				{Name: "A", Demand: 2, MaxReplicas: 2, SLOSlack: 0.052},
				{Name: "B", Demand: 2, MaxReplicas: 2, SLOSlack: 0.051},
			},
			capacity: 1, want: []int{0, 1}, winner: 1,
			wantNotes: "spare replica 1 of 1 (round 1): lower SLO slack than A (0.051 < 0.052)",
		},
		{
			name: "single hungry tenant",
			in: []Input{
				{Name: "A", Demand: 3, Guaranteed: 1, MaxReplicas: 4},
				{Name: "B", Demand: 1, Guaranteed: 1, MaxReplicas: 4},
			},
			capacity: 3, want: []int{2, 1}, winner: 0,
			wantNotes: "spare replica 1 of 1 (round 1): the only tenant still wanting more",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			res := mustArbitrate(t, c.capacity, c.in, Options{Borrowing: true})
			if got := grants(res); !slices.Equal(got, c.want) {
				t.Fatalf("grants = %v, want %v", got, c.want)
			}
			steps := res[c.winner].Steps
			if last := steps[len(steps)-1]; last.Rule != RuleBorrow || last.Note != c.wantNotes {
				t.Fatalf("winner's last step = %+v, want borrow noted %q", last, c.wantNotes)
			}
		})
	}
}

// TestRoundRobin checks that spare is dealt one replica per tenant per round
// and that the notes number the replicas and rounds.
func TestRoundRobin(t *testing.T) {
	in := []Input{
		{Name: "C", Demand: 5, MaxReplicas: 5, SLOSlack: 0.3},
		{Name: "A", Demand: 5, MaxReplicas: 5, SLOSlack: 0.1},
		{Name: "B", Demand: 5, MaxReplicas: 5, SLOSlack: 0.2},
	}
	res := mustArbitrate(t, 7, in, Options{Borrowing: true})
	if got := grants(res); !slices.Equal(got, []int{2, 3, 2}) {
		t.Fatalf("grants (C, A, B) = %v, want [2 3 2]", got)
	}
	wantA := []string{
		"spare replica 1 of 7 (round 1): lower SLO slack than B (0.10 < 0.20)",
		"spare replica 4 of 7 (round 2): lower SLO slack than B (0.10 < 0.20)",
		"spare replica 7 of 7 (round 3): lower SLO slack than B (0.10 < 0.20)",
	}
	for i, w := range wantA {
		if got := res[1].Steps[i+1].Note; got != w {
			t.Errorf("A borrow %d note = %q, want %q", i+1, got, w)
		}
	}
	wantC := "spare replica 6 of 7 (round 2): after B, which has lower SLO slack than C (0.20 < 0.30)"
	if got := res[0].Steps[2].Note; got != wantC {
		t.Errorf("C second borrow note = %q, want %q", got, wantC)
	}
}

func TestMaxReplicasCaps(t *testing.T) {
	in := []Input{
		{Name: "A", Demand: 10, Guaranteed: 2, MaxReplicas: 5, SLOSlack: 0.1},
		{Name: "B", Demand: 3, Guaranteed: 2, MaxReplicas: 8, SLOSlack: 0.2},
	}
	res := mustArbitrate(t, 20, in, Options{Borrowing: true})
	if got := grants(res); !slices.Equal(got, []int{5, 3}) {
		t.Fatalf("grants = %v, want [5 3] (spare left idle rather than exceed MaxReplicas)", got)
	}
	if !res[0].Shedding || res[1].Shedding {
		t.Errorf("shedding = %v/%v, want only A", res[0].Shedding, res[1].Shedding)
	}
	want := "A got 5 replicas: guarantee 2 plus 3 borrowed, capped at its maximum of 5. It wanted 10, so its sheddable requests get 429 until its demand drops to that maximum."
	if res[0].Reason != want {
		t.Errorf("A reason = %q, want %q", res[0].Reason, want)
	}
}

func TestReasons(t *testing.T) {
	cases := []struct {
		name     string
		capacity int
		in       Input
		want     string
	}{
		{"no demand, no guarantee", 0, Input{Name: "A", MaxReplicas: 1}, "A got 0 replicas: no demand this cycle."},
		{"singular replica", 1, Input{Name: "A", Demand: 1, Guaranteed: 1, MaxReplicas: 1}, "A got 1 replica: its full demand, within its guarantee of 1."},
		{"borrowed without a guarantee", 5, Input{Name: "A", Demand: 2, MaxReplicas: 4}, "A got 2 replicas: 2 borrowed (it has no guarantee), its full demand."},
		{"nothing to borrow, no guarantee", 0, Input{Name: "A", Demand: 2, MaxReplicas: 4},
			"A got 0 replicas: it has no guarantee; no spare capacity was left to borrow. It wanted 2, so its sheddable requests get 429 until capacity frees up."},
		{"guarantee equals maximum", 9, Input{Name: "A", Demand: 5, Guaranteed: 3, MaxReplicas: 3},
			"A got 3 replicas: its guarantee of 3, capped at its maximum of 3. It wanted 5, so its sheddable requests get 429 until its demand drops to that maximum."},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			res := mustArbitrate(t, c.capacity, []Input{c.in}, Options{Borrowing: true})
			if res[0].Reason != c.want {
				t.Fatalf("reason = %q, want %q", res[0].Reason, c.want)
			}
		})
	}
}

func TestErrors(t *testing.T) {
	ok := Input{Name: "A", Demand: 1, Guaranteed: 1, MaxReplicas: 2}
	with := func(f func(*Input)) []Input { in := ok; f(&in); return []Input{in} }
	cases := []struct {
		name     string
		capacity int
		in       []Input
		want     string
	}{
		{"negative capacity", -1, nil, "capacity must be >= 0"},
		{"empty name", 5, with(func(i *Input) { i.Name = "" }), "index 0 has an empty name"},
		{"duplicate name", 5, []Input{ok, ok}, `duplicate tenant "A"`},
		{"negative demand", 5, with(func(i *Input) { i.Demand = -1 }), "demand must be >= 0"},
		{"negative guarantee", 5, with(func(i *Input) { i.Guaranteed = -1 }), "guaranteed must be >= 0"},
		{"zero max", 5, with(func(i *Input) { i.MaxReplicas, i.Guaranteed = 0, 0 }), "maxReplicas must be >= 1"},
		{"max below guarantee", 5, with(func(i *Input) { i.Guaranteed = 3 }), "maxReplicas 2 is below its guarantee 3"},
		{"NaN slack", 5, with(func(i *Input) { i.SLOSlack = math.NaN() }), "SLO slack is NaN"},
		{"over-promise", 8, workedExample(1, 1, 1), "guaranteed replicas add up to 9 but capacity is 8"},
		{"over-promise that overflows int", 10, hugeGuarantees(), "guaranteed replicas add up to more than"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			res, err := Arbitrate(c.capacity, c.in, Options{Borrowing: true})
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("err = %v, want it to contain %q", err, c.want)
			}
			if res != nil {
				t.Errorf("results = %+v, want nil on error", res)
			}
		})
	}
}

// hugeGuarantees returns two tenants whose guarantees each fit in an int but
// whose sum overflows it.
func hugeGuarantees() []Input {
	n := math.MaxInt/2 + 1
	return []Input{
		{Name: "A", Demand: n, Guaranteed: n, MaxReplicas: n},
		{Name: "B", Demand: n, Guaranteed: n, MaxReplicas: n},
	}
}

func TestOverPromiseIsDetectable(t *testing.T) {
	for _, in := range [][]Input{workedExample(1, 1, 1), hugeGuarantees()} {
		if _, err := Arbitrate(8, in, Options{Borrowing: true}); !errors.Is(err, ErrOverPromised) {
			t.Fatalf("err = %v, want it to wrap ErrOverPromised", err)
		}
	}
	if _, err := Arbitrate(9, workedExample(1, 1, 1), Options{}); err != nil {
		t.Fatalf("guarantees exactly equal to capacity must be accepted: %v", err)
	}
}

func TestEdgeCases(t *testing.T) {
	res := mustArbitrate(t, 5, nil, Options{Borrowing: true})
	if len(res) != 0 {
		t.Errorf("no tenants: got %+v", res)
	}
	in := workedExample(2, 4, 8)
	before := slices.Clone(in)
	mustArbitrate(t, 10, in, Options{Borrowing: true})
	if !slices.Equal(in, before) {
		t.Errorf("Arbitrate modified its input: %+v", in)
	}
}

func TestFormatPair(t *testing.T) {
	cases := []struct {
		a, b         float64
		wantA, wantB string
	}{
		{0.05, 0.1, "0.05", "0.10"},
		{0.051, 0.052, "0.051", "0.052"},
		{-1, 1, "-1.00", "1.00"},
		{1e-9, 2e-9, "1e-09", "2e-09"},
	}
	for _, c := range cases {
		if a, b := formatPair(c.a, c.b); a != c.wantA || b != c.wantB {
			t.Errorf("formatPair(%v, %v) = %q, %q; want %q, %q", c.a, c.b, a, b, c.wantA, c.wantB)
		}
	}
}
