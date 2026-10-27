package fairq

import (
	"math"
	"testing"
	"time"

	"tenantscaler/internal/model"
	"tenantscaler/internal/policy"
)

// t0 is the fixed virtual start time of every test.
var t0 = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

// harness builds requests with increasing IDs and arrival times.
type harness struct {
	t   *testing.T
	s   *Scheduler
	id  uint64
	now time.Time
}

func newHarness(t *testing.T, cfg Config) *harness {
	t.Helper()
	return &harness{t: t, s: New(cfg), now: t0}
}

// req builds a request with 100 input and at most 100 output tokens,
// arriving at h.now; opts adjust it.
func (h *harness) req(tenant string, p model.Priority, opts ...func(*Request)) *Request {
	h.id++
	r := &Request{
		ID: h.id, Tenant: tenant, Priority: p,
		InputTokens: 100, MaxOutputTokens: 100, Arrival: h.now,
	}
	for _, o := range opts {
		o(r)
	}
	return r
}

// add admits a new request and fails the test if it is rejected.
func (h *harness) add(tenant string, p model.Priority, opts ...func(*Request)) *Request {
	h.t.Helper()
	r := h.req(tenant, p, opts...)
	if v := h.s.Admit(r, h.now, false); !v.Admitted {
		h.t.Fatalf("request %d (%s, %s) rejected: %+v", r.ID, tenant, p.Class(), v)
	}
	return r
}

// next dispatches one request and fails the test when none is queued.
func (h *harness) next() *Request {
	h.t.Helper()
	r := h.s.Next(h.now)
	if r == nil {
		h.t.Fatal("Next returned nil with work queued")
	}
	return r
}

func withDeadline(d time.Duration) func(*Request) {
	return func(r *Request) { r.Deadline = r.Arrival.Add(d) }
}

func withTokens(in, maxOut int) func(*Request) {
	return func(r *Request) { r.InputTokens, r.MaxOutputTokens = in, maxOut }
}

// state returns one tenant's snapshot entry.
func state(t *testing.T, s *Scheduler, tenant string) TenantState {
	t.Helper()
	for _, ts := range s.Snapshot() {
		if ts.Tenant == tenant {
			return ts
		}
	}
	t.Fatalf("tenant %q not in snapshot", tenant)
	return TenantState{}
}

func setPolicy(t *testing.T, s *Scheduler, version int64, tenants map[string]policy.TenantPolicy) {
	t.Helper()
	if !s.SetPolicy(policy.Policy{Version: version, Tenants: tenants}) {
		t.Fatalf("policy v%d rejected", version)
	}
}

func approx(a, b float64) bool { return math.Abs(a-b) <= 1e-9*math.Max(1, math.Abs(b)) }

func ids(rs []*Request) []uint64 {
	out := make([]uint64, len(rs))
	for i, r := range rs {
		out[i] = r.ID
	}
	return out
}

// drain dispatches everything queued, in order.
func drain(h *harness) []*Request {
	var out []*Request
	for h.s.Len() > 0 {
		out = append(out, h.next())
	}
	if r := h.s.Next(h.now); r != nil {
		h.t.Fatalf("Next returned request %d from an empty scheduler", r.ID)
	}
	return out
}
