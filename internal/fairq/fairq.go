// Package fairq is the gateway's scheduling core. It decides whether an
// arriving request is admitted and, whenever a replica slot is free, which
// queued request is dispatched next.
//
// Three modes are implemented:
//
//   - ModeFair (TenantScaler): weighted virtual token counters (VTC, Sheng et
//     al., OSDI '24) across tenants with the counter lift for tenants whose
//     queue was empty; priority, then earliest deadline first, inside a
//     tenant; a critical-quota downgrade against priority gaming; and
//     shedding of sheddable traffic that is over its share or cannot meet
//     its deadline.
//   - ModeRoundRobin: a re-implementation, from its public documentation,
//     of llm-d Flow Control's default behaviour: strict priority bands,
//     round robin across tenants inside a band, FIFO inside a tenant.
//   - ModeFCFS: one global first-come-first-served queue.
//
// The scheduler never reads the wall clock: every time-dependent method
// takes the current time as a parameter, so the discrete-event simulator can
// drive the same code in virtual time.
//
// A Scheduler is NOT safe for concurrent use. The caller serialises all
// calls (the gateway holds one mutex around it; the simulator is
// single-threaded).
package fairq

import (
	"fmt"
	"maps"
	"math"
	"math/rand/v2"
	"slices"
	"strings"
	"time"

	"tenantscaler/internal/model"
	"tenantscaler/internal/policy"
)

// Mode selects the scheduling discipline.
type Mode string

// The scheduling modes.
const (
	// ModeFair is TenantScaler's weighted-VTC scheduler.
	ModeFair Mode = "fair"
	// ModeRoundRobin is the llm-d Flow-Control-equivalent baseline.
	ModeRoundRobin Mode = "roundrobin"
	// ModeFCFS is the single global FIFO baseline.
	ModeFCFS Mode = "fcfs"
)

// ParseMode parses "fair", "roundrobin" or "fcfs" (case-insensitive).
func ParseMode(s string) (Mode, error) {
	m := Mode(strings.ToLower(strings.TrimSpace(s)))
	if !m.valid() {
		return "", fmt.Errorf("invalid scheduler mode %q: want fair|roundrobin|fcfs", s)
	}
	return m, nil
}

func (m Mode) valid() bool {
	return m == ModeFair || m == ModeRoundRobin || m == ModeFCFS
}

// Rejection reasons reported in Verdict.Reason.
const (
	// ReasonOverShare: a sheddable request from a tenant whose policy has
	// Shed set (fair mode).
	ReasonOverShare = "over-share"
	// ReasonDeadlineUnreachable: a sheddable request whose predicted queueing
	// delay already exceeds its deadline (fair mode).
	ReasonDeadlineUnreachable = "deadline-unreachable"
	// ReasonSaturated: a sheddable request while the pool is saturated
	// (roundrobin mode).
	ReasonSaturated = "saturated"
	// ReasonQueueFull: the tenant's queue is at MaxQueuePerTenant and the
	// request is not (effectively) critical (all modes).
	ReasonQueueFull = "queue-full"
)

// Defaults that New applies to zero-valued Config fields.
const (
	// DefaultMaxQueuePerTenant is the default per-tenant queue bound.
	DefaultMaxQueuePerTenant = 1000
	// DefaultRetryJitter is the default relative Retry-After jitter.
	DefaultRetryJitter = 0.5
	// DefaultOutputEstimate is the default initial per-tenant output-length
	// estimate, in tokens.
	DefaultOutputEstimate = 128.0
)

const (
	// rateWindow is the window over which a tenant's dispatch rate is
	// measured for the deadline-unreachable check.
	rateWindow = 10 * time.Second
	// estimateAlpha is the EWMA weight of a completed request's output
	// length in the tenant's output-length estimate.
	estimateAlpha = 0.2
)

// Request is one inference request as the scheduler sees it. The caller
// fills the first group of fields; the scheduler sets the second group.
// A Request must not be admitted again while it is queued or in flight.
type Request struct {
	ID       uint64
	Tenant   string
	Priority model.Priority
	// InputTokens is the prompt length. MaxOutputTokens bounds the output
	// length; zero or negative means unbounded.
	InputTokens, MaxOutputTokens int
	// Deadline is Arrival plus the tenant's TTFT SLO; zero means none.
	Arrival, Deadline time.Time

	// EffectivePriority is Priority after the critical-quota downgrade.
	EffectivePriority model.Priority
	// EstOutputTokens is the output-length estimate at dispatch: the
	// output tokens charged to the tenant's counter in fair mode.
	EstOutputTokens float64
	// Dispatched is the time Next returned the request.
	Dispatched time.Time

	owner      *Scheduler
	ten        *tenant // state of the tenant the request was admitted for
	state      reqState
	q          *queue  // queue holding the request while it is queued
	slot       int     // heap index inside q
	weight     float64 // tenant weight at dispatch; corrections use it
	chargedOut float64 // output tokens charged so far (fair mode)
}

type reqState uint8

const (
	stateNew reqState = iota
	stateQueued
	stateInFlight
	stateDone
)

// Verdict is the outcome of Admit. RetryAfter and Reason are set only when
// the request is rejected.
type Verdict struct {
	Admitted   bool
	RetryAfter time.Duration
	// Reason is one of the Reason* constants.
	Reason string
}

// Config configures a Scheduler.
type Config struct {
	// Mode is the scheduling discipline; empty or unknown means ModeFair.
	// Use ParseMode to validate user input.
	Mode Mode
	// MaxQueuePerTenant bounds each tenant's queue for non-critical
	// requests; zero or negative means DefaultMaxQueuePerTenant.
	MaxQueuePerTenant int
	// RetryJitter spreads Retry-After: RetryAfter = base*(1+RetryJitter*U),
	// U uniform in [0,1). Zero (or non-finite) means DefaultRetryJitter; a
	// negative value disables jitter.
	RetryJitter float64
	// Seed seeds the jitter generator, so runs are reproducible.
	Seed int64
	// DefaultOutputTokens is the initial per-tenant output-length estimate;
	// zero, negative or non-finite means DefaultOutputEstimate.
	DefaultOutputTokens float64
}

func (c Config) withDefaults() Config {
	if !c.Mode.valid() {
		c.Mode = ModeFair
	}
	if c.MaxQueuePerTenant <= 0 {
		c.MaxQueuePerTenant = DefaultMaxQueuePerTenant
	}
	switch {
	case c.RetryJitter == 0 || math.IsNaN(c.RetryJitter) || math.IsInf(c.RetryJitter, 0):
		c.RetryJitter = DefaultRetryJitter
	case c.RetryJitter < 0:
		c.RetryJitter = 0
	}
	if !(c.DefaultOutputTokens > 0) || math.IsInf(c.DefaultOutputTokens, 0) {
		c.DefaultOutputTokens = DefaultOutputEstimate
	}
	return c
}

// TenantState is a read-only view of one tenant's scheduler state.
type TenantState struct {
	Tenant string
	// Queued and InFlight count admitted requests waiting and dispatched.
	// CriticalActive counts those of them with a critical effective priority.
	Queued, InFlight, CriticalActive int
	// Counter is the tenant's virtual token counter (fair mode; zero in the
	// baseline modes). EstOutputTokens is its output-length estimate.
	Counter, EstOutputTokens float64
	// Weight and Shed echo the current policy; only fair mode acts on them.
	Weight float64
	Shed   bool
	// Admitted counts admitted requests; Rejected counts rejections by reason.
	Admitted int64
	Rejected map[string]int64
}

// tenant is the scheduler's per-tenant state.
type tenant struct {
	name  string
	fair  *queue           // ModeFair: priority, then EDF, then ID
	bands [numBands]*queue // ModeRoundRobin: FIFO per priority band

	queued, inFlight, criticalActive int
	counter, estOut                  float64
	admitted                         int64
	rejected                         map[string]int64
	// dispatches holds this tenant's dispatch times inside rateWindow
	// (fair mode), oldest first.
	dispatches []time.Time
}

// Scheduler is the gateway scheduling core. It is not goroutine-safe: the
// caller must serialise every method call.
type Scheduler struct {
	cfg     Config
	pol     policy.Policy
	rng     *rand.Rand
	tenants map[string]*tenant
	names   []string // sorted tenant names, for deterministic iteration
	queued  int
	// fifo is the global queue (ModeFCFS).
	fifo *queue
	// rrLast is the tenant served last in each band (ModeRoundRobin).
	rrLast [numBands]string
	// lastDrained is the tenant whose queue emptied last; its counter is the
	// lift reference when no tenant has queued work (ModeFair).
	lastDrained *tenant
}

// New returns an empty scheduler. Zero-valued Config fields take their
// defaults. Until SetPolicy is called the policy is empty (version 0): every
// tenant has weight 1, nothing is shed and there is no critical quota.
func New(cfg Config) *Scheduler {
	cfg = cfg.withDefaults()
	s := &Scheduler{
		cfg:     cfg,
		pol:     policy.Policy{Tenants: map[string]policy.TenantPolicy{}},
		rng:     rand.New(rand.NewPCG(uint64(cfg.Seed), 0x9e3779b97f4a7c15)),
		tenants: map[string]*tenant{},
	}
	if cfg.Mode == ModeFCFS {
		s.fifo = newQueue(fifoLess)
	}
	return s
}

// Config returns the effective configuration (defaults applied).
func (s *Scheduler) Config() Config { return s.cfg }

// SetPolicy installs p and reports whether it was accepted. A policy older
// than the current one (p.Version < current version) is ignored, as is one
// with a non-finite weight, which would corrupt the counters. Equal versions
// are accepted so a resent policy is idempotent. Tenants named by the policy
// appear in Snapshot even before they send traffic.
func (s *Scheduler) SetPolicy(p policy.Policy) bool {
	if p.Version < s.pol.Version {
		return false
	}
	for _, tp := range p.Tenants {
		if math.IsNaN(tp.Weight) || math.IsInf(tp.Weight, 0) {
			return false
		}
	}
	s.pol = clonePolicy(p)
	for name := range s.pol.Tenants {
		s.tenant(name)
	}
	return true
}

// Policy returns a copy of the current policy.
func (s *Scheduler) Policy() policy.Policy { return clonePolicy(s.pol) }

func clonePolicy(p policy.Policy) policy.Policy {
	p.Tenants = maps.Clone(p.Tenants)
	return p
}

// Len returns the number of queued (admitted, not yet dispatched) requests.
func (s *Scheduler) Len() int { return s.queued }

// Snapshot returns every known tenant's state, sorted by tenant name.
func (s *Scheduler) Snapshot() []TenantState {
	out := make([]TenantState, 0, len(s.names))
	for _, name := range s.names {
		t := s.tenants[name]
		out = append(out, TenantState{
			Tenant:          name,
			Queued:          t.queued,
			InFlight:        t.inFlight,
			CriticalActive:  t.criticalActive,
			Counter:         t.counter,
			EstOutputTokens: t.estOut,
			Weight:          s.pol.WeightFor(name),
			Shed:            s.pol.Tenants[name].Shed,
			Admitted:        t.admitted,
			Rejected:        maps.Clone(t.rejected),
		})
	}
	return out
}

// tenant returns the state for name, creating it on first use.
func (s *Scheduler) tenant(name string) *tenant {
	if t, ok := s.tenants[name]; ok {
		return t
	}
	t := &tenant{name: name, estOut: s.cfg.DefaultOutputTokens, rejected: map[string]int64{}}
	switch s.cfg.Mode {
	case ModeFair:
		t.fair = newQueue(fairLess)
	case ModeRoundRobin:
		for b := range t.bands {
			t.bands[b] = newQueue(fifoLess)
		}
	}
	s.tenants[name] = t
	i, _ := slices.BinarySearch(s.names, name)
	s.names = slices.Insert(s.names, i, name)
	return t
}
