package loadgen

import (
	"maps"
	"sort"
	"sync"

	"tenantscaler/internal/eval"
)

// Counts summarises the outcomes recorded for one target.
type Counts struct {
	Completed, Shed, Failed int
	// FailedBy counts failures by category (e.g. "http 503 Service
	// Unavailable", "transport error", "client in-flight limit").
	FailedBy map[string]int
	// Simulated counts the responses that carried the header
	// X-TenantScaler-Simulated: true (replica.SimulatedHeader), which a
	// simulated replica sets and the gateway relays. When it is > 0 the
	// target's latencies come from simulated compute, not real inference,
	// and must be reported as simulated.
	Simulated int
}

// IsSimulated reports whether any response of the target came from a
// simulated replica.
func (c Counts) IsSimulated() bool { return c.Simulated > 0 }

// tally counts outcomes per target. It is safe for concurrent use.
type tally struct {
	mu       sync.Mutex
	byTarget map[string]*Counts
}

// add counts rec for target; category is empty unless rec failed, and
// simulated reports a response marked simulated. It reports whether this is
// the first record of its kind (category) for the target, so callers can
// log the first failure of each kind only.
func (t *tally) add(target string, rec eval.Record, category string, simulated bool) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.byTarget == nil {
		t.byTarget = map[string]*Counts{}
	}
	c := t.byTarget[target]
	if c == nil {
		c = &Counts{FailedBy: map[string]int{}}
		t.byTarget[target] = c
	}
	if simulated {
		c.Simulated++
	}
	switch {
	case category != "":
		c.Failed++
		c.FailedBy[category]++
		return c.FailedBy[category] == 1
	case rec.Completed():
		c.Completed++
	default:
		c.Shed++
	}
	return false
}

// snapshot returns a deep copy of the counts.
func (t *tally) snapshot() map[string]Counts {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make(map[string]Counts, len(t.byTarget))
	for name, c := range t.byTarget {
		cp := *c
		cp.FailedBy = maps.Clone(c.FailedBy)
		out[name] = cp
	}
	return out
}

// logSummary logs one line per target with its outcome counts.
func (s *sender) logSummary(msg string) {
	counts := s.tally.snapshot()
	names := make([]string, 0, len(counts))
	for n := range counts {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		c := counts[n]
		s.logger.Info("loadgen: "+msg, "target", n, "simulated", c.IsSimulated(),
			"completed", c.Completed, "shed", c.Shed, "failed", c.Failed, "failedBy", c.FailedBy)
	}
}
