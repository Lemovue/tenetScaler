package arbiter

import (
	"fmt"
	"math/rand/v2"
	"reflect"
	"strings"
	"testing"
)

const propertyCases = 5000

// randomCase draws a valid arbitration problem. Priorities and slacks come
// from small sets half of the time so that ties (and the name tie-break) are
// exercised often.
func randomCase(rng *rand.Rand) (int, []Input) {
	names := []string{"A", "B", "C", "D", "E", "F", "G"}
	rng.Shuffle(len(names), func(i, j int) { names[i], names[j] = names[j], names[i] })
	slacks := []float64{-1, -0.25, 0, 0.05, 0.1, 0.5, 1}
	in := make([]Input, 1+rng.IntN(6))
	sumG := 0
	for i := range in {
		g := rng.IntN(5)
		slack := slacks[rng.IntN(len(slacks))]
		if rng.IntN(2) == 0 {
			slack = rng.Float64()*2 - 1
		}
		in[i] = Input{
			Name:        names[i],
			Demand:      rng.IntN(13),
			Guaranteed:  g,
			MaxReplicas: max(1, g) + rng.IntN(6),
			Priority:    100 * rng.IntN(3),
			SLOSlack:    slack,
		}
		sumG += g
	}
	return sumG + rng.IntN(15), in
}

// TestArbitrateProperties checks every invariant of the design on random
// inputs (fixed seed), with and without borrowing.
func TestArbitrateProperties(t *testing.T) {
	rng := rand.New(rand.NewPCG(2026, 10))
	for c := range propertyCases {
		capacity, in := randomCase(rng)
		for _, borrowing := range []bool{true, false} {
			opt := Options{Borrowing: borrowing}
			res, err := Arbitrate(capacity, in, opt)
			if err != nil {
				t.Fatalf("case %d: unexpected error %v for capacity %d, %+v", c, err, capacity, in)
			}
			if msg := violation(capacity, in, opt, res); msg != "" {
				t.Fatalf("case %d (borrowing=%v): %s\ncapacity %d\ninputs %+v\nresults %+v", c, borrowing, msg, capacity, in, res)
			}
			again, _ := Arbitrate(capacity, in, opt)
			if !reflect.DeepEqual(res, again) {
				t.Fatalf("case %d: not deterministic", c)
			}
			shuffled := append([]Input(nil), in...)
			rng.Shuffle(len(shuffled), func(i, j int) { shuffled[i], shuffled[j] = shuffled[j], shuffled[i] })
			other, _ := Arbitrate(capacity, shuffled, opt)
			if !reflect.DeepEqual(byName(res), byName(other)) {
				t.Fatalf("case %d: result depends on input order", c)
			}
		}
	}
}

func byName(res []Result) map[string]Result {
	m := make(map[string]Result, len(res))
	for _, r := range res {
		m[r.Name] = r
	}
	return m
}

// violation returns a description of the first broken invariant, or "".
func violation(capacity int, in []Input, opt Options, res []Result) string {
	if len(res) != len(in) {
		return fmt.Sprintf("%d results for %d inputs", len(res), len(in))
	}
	sum, sumWant := 0, 0
	for i, t := range in {
		r := res[i]
		g, base, want := r.Grant, min(t.Demand, t.Guaranteed), min(t.Demand, t.MaxReplicas)
		sum += g
		sumWant += want
		switch {
		case r.Name != t.Name:
			return fmt.Sprintf("result %d is %q, want input order (%q)", i, r.Name, t.Name)
		case g < base:
			return fmt.Sprintf("%s: grant %d below min(demand, guarantee) %d", t.Name, g, base)
		case t.Demand >= t.Guaranteed && g > want:
			return fmt.Sprintf("%s: grant %d above min(demand, max) %d", t.Name, g, want)
		case t.Demand < t.Guaranteed && g != t.Demand:
			return fmt.Sprintf("%s: demand %d within guarantee but grant %d", t.Name, t.Demand, g)
		case !opt.Borrowing && g != base:
			return fmt.Sprintf("%s: borrowing off but grant %d != %d", t.Name, g, base)
		case r.Shedding != (t.Demand > g):
			return fmt.Sprintf("%s: shedding %v with demand %d, grant %d", t.Name, r.Shedding, t.Demand, g)
		case !strings.HasPrefix(r.Reason, t.Name+" got "):
			return fmt.Sprintf("%s: reason %q does not start with the name", t.Name, r.Reason)
		case r.Shedding != strings.Contains(r.Reason, "429"):
			return fmt.Sprintf("%s: reason %q disagrees with shedding=%v", t.Name, r.Reason, r.Shedding)
		case !opt.Borrowing && strings.Contains(strings.ReplaceAll(r.Reason, "borrowing is disabled", ""), "borrow"):
			return fmt.Sprintf("%s: borrowing off but reason %q talks about borrowing", t.Name, r.Reason)
		}
		if msg := stepViolation(t, r, base); msg != "" {
			return msg
		}
	}
	if sum > capacity {
		return fmt.Sprintf("grants add up to %d > capacity %d", sum, capacity)
	}
	if !opt.Borrowing {
		return ""
	}
	if sum < capacity {
		for i, t := range in {
			if res[i].Grant < min(t.Demand, t.MaxReplicas) {
				return fmt.Sprintf("%d spare left but %s is hungry (grant %d)", capacity-sum, t.Name, res[i].Grant)
			}
		}
	}
	// Work conservation: the pool lends everything anyone can use.
	if sum != min(capacity, sumWant) {
		return fmt.Sprintf("grants add up to %d, want min(capacity %d, total wanted %d)", sum, capacity, sumWant)
	}
	return roundRobinViolation(in, res)
}

// stepViolation checks that the steps are one guarantee step followed by one
// borrow step per borrowed replica, each naming why it was served there.
func stepViolation(t Input, r Result, base int) string {
	if len(r.Steps) != 1+r.Grant-base {
		return fmt.Sprintf("%s: %d steps for %d borrowed", t.Name, len(r.Steps), r.Grant-base)
	}
	if s := r.Steps[0]; s.Rule != RuleGuarantee || s.Grant != base || s.Note == "" {
		return fmt.Sprintf("%s: first step %+v, want guarantee with grant %d", t.Name, s, base)
	}
	keys := []string{"higher priority than", "lower SLO slack than", "sorts first by name", "the only tenant still wanting more"}
	for k, s := range r.Steps[1:] {
		if s.Rule != RuleBorrow || s.Grant != base+k+1 {
			return fmt.Sprintf("%s: step %d is %+v, want borrow with grant %d", t.Name, k+1, s, base+k+1)
		}
		named := false
		for _, key := range keys {
			named = named || strings.Contains(s.Note, key)
		}
		if !named {
			return fmt.Sprintf("%s: borrow note %q names no ordering key", t.Name, s.Note)
		}
	}
	return ""
}

// roundRobinViolation checks the round-robin shape: compared with any tenant
// X that is still hungry, no tenant borrowed more than one extra replica, and
// one that did must rank ahead of X in lending order.
func roundRobinViolation(in []Input, res []Result) string {
	borrowed := func(i int) int { return res[i].Grant - min(in[i].Demand, in[i].Guaranteed) }
	for x := range in {
		if res[x].Grant >= min(in[x].Demand, in[x].MaxReplicas) {
			continue
		}
		for y := range in {
			bx, by := borrowed(x), borrowed(y)
			switch {
			case by > bx+1:
				return fmt.Sprintf("%s borrowed %d while hungry %s borrowed only %d", in[y].Name, by, in[x].Name, bx)
			case by == bx+1 && lendingOrder(in[y], in[x]) > 0:
				return fmt.Sprintf("%s got an extra round over %s but ranks behind it", in[y].Name, in[x].Name)
			}
		}
	}
	return ""
}
