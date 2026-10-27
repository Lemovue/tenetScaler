// Package arbiter decides who gets the scarce replicas of a shared pool.
//
// Every tenant first receives its demand up to its guarantee. Whatever the
// pool has left over (including guarantees that idle tenants are not using)
// is lent to tenants that want more, one replica at a time, round-robin in
// order of priority (higher first), then SLO slack (closer to violating
// first), then name. Because the allocation is recomputed from scratch each
// cycle, a tenant whose demand returns gets its guarantee back on the next
// cycle without any separate reclaim logic.
package arbiter

import (
	"cmp"
	"errors"
	"fmt"
	"math"
	"slices"
	"strings"

	"tenantscaler/internal/model"
)

// Step rule names recorded in Result.Steps.
const (
	RuleGuarantee = "guarantee"
	RuleBorrow    = "borrow"
)

// ErrOverPromised is wrapped by the error Arbitrate returns when the
// guarantees add up to more than the pool's capacity.
var ErrOverPromised = errors.New("refusing to promise more replicas than exist")

// Input is one tenant's request for replicas in one control cycle.
type Input struct {
	// Name identifies the tenant; it must be non-empty and unique.
	Name string
	// Demand is the number of replicas the tenant wants this cycle (>= 0).
	Demand int
	// Guaranteed is the number of replicas the tenant is promised (>= 0).
	Guaranteed int
	// MaxReplicas caps what the tenant may hold, borrowing included
	// (>= Guaranteed and >= 1).
	MaxReplicas int
	// Priority orders tenants when spare replicas are lent: higher first.
	Priority int
	// SLOSlack breaks priority ties: lower (closer to violating) first. It
	// must not be NaN.
	SLOSlack float64
}

// Options switch parts of the algorithm on and off.
type Options struct {
	// Borrowing lends spare replicas beyond guarantees. Turning it off is
	// the static-partition ablation.
	Borrowing bool
}

// Result is one tenant's allocation.
type Result struct {
	// Name is the tenant's name.
	Name string
	// Grant is the number of replicas the tenant receives.
	Grant int
	// Shedding is true when Demand > Grant: the gateway should reject the
	// tenant's sheddable requests until the gap closes.
	Shedding bool
	// Steps lists the rules applied, in order; each step's Grant is the
	// tenant's grant after that step.
	Steps []model.DecisionStep
	// Reason is one plain-English sentence (or two) for the dashboard.
	Reason string
}

// Arbitrate splits capacity replicas among the tenants in in and returns one
// Result per tenant, in input order. It does not modify in.
//
// It returns an error if capacity < 0, a name is empty or duplicated, a
// Demand or Guaranteed is negative, a MaxReplicas is < 1 or < Guaranteed, a
// SLOSlack is NaN, or the guarantees add up to more than capacity.
func Arbitrate(capacity int, in []Input, opt Options) ([]Result, error) {
	if err := validate(capacity, in); err != nil {
		return nil, err
	}
	res := make([]Result, len(in))
	spare := capacity
	for i, t := range in {
		g := min(t.Demand, t.Guaranteed)
		res[i] = Result{
			Name:  t.Name,
			Grant: g,
			Steps: []model.DecisionStep{{Rule: RuleGuarantee, Grant: g, Note: guaranteeNote(t)}},
		}
		spare -= g
	}
	if opt.Borrowing {
		lend(spare, in, res)
	}
	for i, t := range in {
		res[i].Shedding = t.Demand > res[i].Grant
		res[i].Reason = reason(t, res[i], opt)
	}
	return res, nil
}

// lend hands out spare replicas one at a time, round-robin over the hungry
// tenants (those below min(Demand, MaxReplicas)) in lending order, until the
// spare runs out or nobody is hungry. A tenant leaves the rotation as soon
// as it is satisfied.
func lend(spare int, in []Input, res []Result) {
	want := func(i int) int { return min(in[i].Demand, in[i].MaxReplicas) }
	var line []int
	for i := range in {
		if want(i) > res[i].Grant {
			line = append(line, i)
		}
	}
	slices.SortFunc(line, func(a, b int) int { return lendingOrder(in[a], in[b]) })

	total := spare
	for round := 1; spare > 0 && len(line) > 0; round++ {
		var still []int
		for pos, i := range line {
			if spare == 0 {
				break
			}
			spare--
			res[i].Grant++
			note := fmt.Sprintf("spare replica %d of %d (round %d): %s",
				total-spare, total, round, placeNote(in, line, pos))
			res[i].Steps = append(res[i].Steps, model.DecisionStep{Rule: RuleBorrow, Grant: res[i].Grant, Note: note})
			if want(i) > res[i].Grant {
				still = append(still, i)
			}
		}
		line = still
	}
}

// lendingOrder compares two tenants for lending: higher Priority first, then
// lower SLOSlack, then Name. Names are unique, so the order is total and the
// allocation does not depend on input order.
func lendingOrder(a, b Input) int {
	if c := cmp.Compare(b.Priority, a.Priority); c != 0 {
		return c
	}
	if c := cmp.Compare(a.SLOSlack, b.SLOSlack); c != 0 {
		return c
	}
	return strings.Compare(a.Name, b.Name)
}

func validate(capacity int, in []Input) error {
	if capacity < 0 {
		return fmt.Errorf("capacity must be >= 0, got %d", capacity)
	}
	seen := make(map[string]bool, len(in))
	// sum is exact unless overflow is set: guarantees too large to add up
	// in an int must still fail the capacity check, not wrap around past it.
	sum, overflow := 0, false
	for i, t := range in {
		switch {
		case t.Name == "":
			return fmt.Errorf("tenant at index %d has an empty name", i)
		case seen[t.Name]:
			return fmt.Errorf("duplicate tenant %q", t.Name)
		case t.Demand < 0:
			return fmt.Errorf("tenant %q: demand must be >= 0, got %d", t.Name, t.Demand)
		case t.Guaranteed < 0:
			return fmt.Errorf("tenant %q: guaranteed must be >= 0, got %d", t.Name, t.Guaranteed)
		case t.MaxReplicas < 1:
			return fmt.Errorf("tenant %q: maxReplicas must be >= 1, got %d", t.Name, t.MaxReplicas)
		case t.MaxReplicas < t.Guaranteed:
			return fmt.Errorf("tenant %q: maxReplicas %d is below its guarantee %d", t.Name, t.MaxReplicas, t.Guaranteed)
		case math.IsNaN(t.SLOSlack):
			return fmt.Errorf("tenant %q: SLO slack is NaN", t.Name)
		}
		seen[t.Name] = true
		if t.Guaranteed > math.MaxInt-sum {
			overflow = true
		} else {
			sum += t.Guaranteed
		}
	}
	switch {
	case overflow:
		return fmt.Errorf("%w: guaranteed replicas add up to more than %d but capacity is %d", ErrOverPromised, math.MaxInt, capacity)
	case sum > capacity:
		return fmt.Errorf("%w: guaranteed replicas add up to %d but capacity is %d", ErrOverPromised, sum, capacity)
	}
	return nil
}
