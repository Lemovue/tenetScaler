// Package policy defines the per-tenant policy the controller pushes to the
// gateway every control cycle: share weight, shedding flag, retry-after and
// critical quota. Policies are versioned; a gateway ignores older versions.
package policy

import (
	"fmt"
	"math"
	"time"
)

// MinWeight keeps a tenant with a zero grant schedulable (no division by
// zero, no permanent starvation) while making it the last to be served.
const MinWeight = 0.05

// TenantPolicy is what the gateway enforces for one tenant.
type TenantPolicy struct {
	// Weight is the tenant's share of the pool, in replicas (the arbiter's
	// grant). Token-fair queuing divides served tokens by this weight.
	Weight float64 `json:"weight"`
	// Shed turns on 429 for this tenant's sheddable requests.
	Shed bool `json:"shed"`
	// RetryAfterSeconds is the base Retry-After value sent with a 429.
	RetryAfterSeconds int `json:"retryAfterSeconds"`
	// CriticalSlots caps how many of this tenant's critical requests may be
	// in flight or queued at once; critical traffic above it is treated as
	// standard (anti-gaming). Zero means no cap.
	CriticalSlots int `json:"criticalSlots"`
	// Slots is the tenant's share of the pool's dispatch slots (its grant
	// times slots per replica) and GuaranteedSlots its guarantee in slots.
	// A fair gateway keeps the unused part of other tenants' shares free,
	// so a borrower cannot take every slot that frees up (see fairq).
	// Zero shares reserve nothing.
	Slots           int `json:"slots,omitempty"`
	GuaranteedSlots int `json:"guaranteedSlots,omitempty"`
}

// Policy is a versioned set of tenant policies.
type Policy struct {
	Version  int64                   `json:"version"`
	IssuedAt time.Time               `json:"issuedAt"`
	Tenants  map[string]TenantPolicy `json:"tenants"`
}

// Validate checks that a policy is well formed.
func (p Policy) Validate() error {
	if p.Version <= 0 {
		return fmt.Errorf("policy version must be positive, got %d", p.Version)
	}
	for name, t := range p.Tenants {
		if name == "" {
			return fmt.Errorf("policy has a tenant with an empty name")
		}
		if math.IsNaN(t.Weight) || math.IsInf(t.Weight, 0) || t.Weight < 0 {
			return fmt.Errorf("tenant %q: weight must be a finite non-negative number, got %v", name, t.Weight)
		}
		if t.RetryAfterSeconds < 0 || t.CriticalSlots < 0 || t.Slots < 0 || t.GuaranteedSlots < 0 {
			return fmt.Errorf("tenant %q: retryAfterSeconds, criticalSlots, slots and guaranteedSlots must be >= 0", name)
		}
	}
	return nil
}

// WeightFor returns the tenant's effective weight, falling back to 1 for an
// unknown tenant and clamping to MinWeight.
func (p Policy) WeightFor(tenant string) float64 {
	t, ok := p.Tenants[tenant]
	if !ok {
		return 1
	}
	return math.Max(t.Weight, MinWeight)
}

// Equal returns the fail-safe policy used before the controller has spoken:
// every listed tenant gets weight 1 and nothing is shed.
func Equal(tenants []string) Policy {
	m := make(map[string]TenantPolicy, len(tenants))
	for _, t := range tenants {
		m[t] = TenantPolicy{Weight: 1, RetryAfterSeconds: 1}
	}
	return Policy{Version: 0, Tenants: m}
}
