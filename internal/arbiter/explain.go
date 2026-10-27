package arbiter

import (
	"fmt"
	"strconv"
)

// guaranteeNote explains the guarantee step.
func guaranteeNote(t Input) string {
	switch {
	case t.Demand == 0:
		return "no demand this cycle"
	case t.Demand <= t.Guaranteed:
		return fmt.Sprintf("demand %d is within the guarantee of %d", t.Demand, t.Guaranteed)
	default:
		return fmt.Sprintf("demand %d exceeds the guarantee of %d", t.Demand, t.Guaranteed)
	}
}

// placeNote explains why the tenant at line[pos] is where it is in this
// lending round, naming the ordering key that separates it from its
// neighbour: the tenant behind it, or (for the last) the tenant ahead of it.
func placeNote(in []Input, line []int, pos int) string {
	switch {
	case len(line) == 1:
		return "the only tenant still wanting more"
	case pos < len(line)-1:
		return orderKey(in[line[pos]], in[line[pos+1]])
	default:
		prev := in[line[pos-1]]
		return fmt.Sprintf("after %s, which has %s", prev.Name, orderKey(prev, in[line[pos]]))
	}
}

// orderKey names the first lending key on which a ranks ahead of b, e.g.
// "lower SLO slack than C (0.05 < 0.10)".
func orderKey(a, b Input) string {
	switch {
	case a.Priority != b.Priority:
		return fmt.Sprintf("higher priority than %s (%d > %d)", b.Name, a.Priority, b.Priority)
	case a.SLOSlack != b.SLOSlack:
		sa, sb := formatPair(a.SLOSlack, b.SLOSlack)
		return fmt.Sprintf("lower SLO slack than %s (%s < %s)", b.Name, sa, sb)
	default:
		return fmt.Sprintf("the same priority and SLO slack as %s, and %s sorts first by name", b.Name, a.Name)
	}
}

// formatPair prints two different numbers with two decimals, or with as
// many more as it takes for them to print differently.
func formatPair(a, b float64) (string, string) {
	for prec := 2; prec <= 6; prec++ {
		sa, sb := strconv.FormatFloat(a, 'f', prec, 64), strconv.FormatFloat(b, 'f', prec, 64)
		if sa != sb {
			return sa, sb
		}
	}
	return strconv.FormatFloat(a, 'g', -1, 64), strconv.FormatFloat(b, 'g', -1, 64)
}

// reason builds the dashboard sentence for one tenant's result.
func reason(t Input, r Result, opt Options) string {
	head := fmt.Sprintf("%s got %s: ", t.Name, replicas(r.Grant))
	borrowed := r.Grant - min(t.Demand, t.Guaranteed)
	switch {
	case t.Demand == 0 && t.Guaranteed > 0 && opt.Borrowing:
		return head + fmt.Sprintf("no demand this cycle, so its guarantee of %d is free for others to borrow until it needs it back.", t.Guaranteed)
	case t.Demand == 0 && t.Guaranteed > 0:
		return head + fmt.Sprintf("no demand this cycle, and its guarantee of %d sits idle (borrowing is disabled).", t.Guaranteed)
	case t.Demand == 0:
		return head + "no demand this cycle."
	case t.Demand <= t.Guaranteed:
		return head + fmt.Sprintf("its full demand, within its guarantee of %d.", t.Guaranteed)
	case r.Grant >= t.Demand:
		return head + source(t.Guaranteed, borrowed) + ", its full demand."
	}
	var limit, until string
	switch {
	case r.Grant >= t.MaxReplicas:
		limit, until = fmt.Sprintf(", capped at its maximum of %d", t.MaxReplicas), "its demand drops to that maximum"
	case !opt.Borrowing:
		limit, until = " (borrowing is disabled)", "its demand drops to its guarantee"
	case borrowed == 0:
		limit, until = "; no spare capacity was left to borrow", "capacity frees up"
	default:
		until = "capacity frees up"
	}
	return head + source(t.Guaranteed, borrowed) + limit +
		fmt.Sprintf(". It wanted %d, so its sheddable requests get 429 until %s.", t.Demand, until)
}

// source says where a grant above the guarantee step came from.
func source(guaranteed, borrowed int) string {
	switch {
	case borrowed == 0 && guaranteed == 0:
		return "it has no guarantee"
	case borrowed == 0:
		return fmt.Sprintf("its guarantee of %d", guaranteed)
	case guaranteed == 0:
		return fmt.Sprintf("%d borrowed (it has no guarantee)", borrowed)
	default:
		return fmt.Sprintf("guarantee %d plus %d borrowed", guaranteed, borrowed)
	}
}

func replicas(n int) string {
	if n == 1 {
		return "1 replica"
	}
	return fmt.Sprintf("%d replicas", n)
}
