package gateway

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"tenantscaler/internal/fairq"
	"tenantscaler/internal/model"
	"tenantscaler/internal/policy"
)

// indexTenants checks tenant specs and indexes them by name.
func indexTenants(specs []model.TenantSpec) (map[string]model.TenantSpec, []string, error) {
	if len(specs) == 0 {
		return nil, nil, fmt.Errorf("gateway: at least one tenant is required")
	}
	m := make(map[string]model.TenantSpec, len(specs))
	order := make([]string, 0, len(specs))
	for _, s := range specs {
		switch {
		case strings.TrimSpace(s.Name) == "":
			return nil, nil, fmt.Errorf("gateway: tenant with an empty name")
		case s.SLO.TTFTSeconds < 0 || s.SLO.TPOTMillis < 0:
			return nil, nil, fmt.Errorf("gateway: tenant %q: SLO bounds must be >= 0", s.Name)
		}
		if _, dup := m[s.Name]; dup {
			return nil, nil, fmt.Errorf("gateway: duplicate tenant %q", s.Name)
		}
		m[s.Name] = s
		order = append(order, s.Name)
	}
	return m, order, nil
}

// SetTenants replaces the known tenants, e.g. after the TenantScaler
// resources changed in Kubernetes mode. New requests of a removed tenant
// get 400. Its admitted requests are still served, except queued ones in
// partition mode: the tenant owns no endpoints there any more, so they are
// answered 503 at once instead of after the dispatch timeout.
func (g *Gateway) SetTenants(specs []model.TenantSpec) error {
	tenants, order, err := indexTenants(specs)
	if err != nil {
		return err
	}
	g.mu.Lock()
	g.setTenantsLocked(tenants, order)
	g.mu.Unlock()
	g.kick()
	return nil
}

func (g *Gateway) setTenantsLocked(tenants map[string]model.TenantSpec, order []string) {
	g.tenants, g.order = tenants, order
	for _, t := range order {
		g.metrics.initTenant(t)
		g.countsLocked(t)
	}
	if g.cfg.Mode == ModePartition {
		for t, s := range g.parts {
			if _, ok := tenants[t]; !ok {
				g.withdrawLocked(s)
				delete(g.parts, t) // its in-flight requests keep their scheduler
			}
		}
		for _, t := range order {
			g.partitionSchedulerLocked(t)
		}
		g.owners = partition(g.endpoints, order, g.sharesLocked())
	}
}

// tenantSpecs returns the known tenants in their configured order.
func (g *Gateway) tenantSpecs() []model.TenantSpec {
	g.mu.Lock()
	defer g.mu.Unlock()
	out := make([]model.TenantSpec, len(g.order))
	for i, t := range g.order {
		out[i] = g.tenants[t]
	}
	return out
}

// FollowTenants keeps the known tenants in step with an external source,
// such as the TenantScaler resources in Kubernetes mode: every `every`
// (at least 10 ms) it calls list and applies a changed tenant set with
// SetTenants. A failed or empty read, or an invalid set, keeps the current
// tenants; failures are logged once per streak. It returns when ctx is
// done.
func (g *Gateway) FollowTenants(ctx context.Context, list func(context.Context) ([]model.TenantSpec, error), every time.Duration) {
	t := time.NewTicker(max(every, 10*time.Millisecond))
	defer t.Stop()
	failing := false
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		specs, err := list(ctx)
		if err == nil && len(specs) == 0 {
			err = errors.New("the tenant source lists no tenants")
		}
		if err == nil && !slices.Equal(specs, g.tenantSpecs()) {
			if err = g.SetTenants(specs); err == nil {
				g.log.Info("tenants updated", "tenants", len(specs))
			}
		}
		if err != nil && !failing && ctx.Err() == nil {
			g.log.Warn("cannot update the tenants; keeping the current ones", "err", err)
		}
		failing = err != nil
	}
}

// sharesLocked returns each tenant's partition share.
func (g *Gateway) sharesLocked() map[string]int {
	shares := make(map[string]int, len(g.order))
	for _, t := range g.order {
		n, ok := g.cfg.PartitionShares[t]
		if !ok {
			n = g.tenants[t].GuaranteedReplicas
		}
		shares[t] = max(n, 0)
	}
	return shares
}

// partitionSchedulerLocked returns the tenant's FIFO in partition mode,
// creating it on first use.
func (g *Gateway) partitionSchedulerLocked(tenant string) *fairq.Scheduler {
	s, ok := g.parts[tenant]
	if !ok {
		s = fairq.New(fairq.Config{Mode: fairq.ModeFCFS, Seed: g.cfg.Seed + int64(len(g.parts))})
		s.SetPolicy(ownPolicy(g.pol, tenant))
		g.parts[tenant] = s
	}
	return s
}

// ownPolicy restricts p to one tenant. A partition scheduler serves a single
// tenant; given the whole policy it would also register (and report in its
// Snapshot) every other tenant the policy names.
func ownPolicy(p policy.Policy, tenant string) policy.Policy {
	own := policy.Policy{Version: p.Version, IssuedAt: p.IssuedAt, Tenants: map[string]policy.TenantPolicy{}}
	if tp, ok := p.Tenants[tenant]; ok {
		own.Tenants[tenant] = tp
	}
	return own
}

// countsLocked returns the tenant's live counts, creating them on first use.
func (g *Gateway) countsLocked(tenant string) *tenantCounts {
	c, ok := g.counts[tenant]
	if !ok {
		c = &tenantCounts{}
		g.counts[tenant] = c
	}
	return c
}

// tenantSpec returns the spec of a known tenant.
func (g *Gateway) tenantSpec(tenant string) (model.TenantSpec, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	s, ok := g.tenants[tenant]
	return s, ok
}

// SetPolicy applies p as POST /policy does and reports whether it was
// applied: an invalid policy or one older than the current version is
// ignored.
func (g *Gateway) SetPolicy(p policy.Policy) bool {
	if p.Validate() != nil {
		return false
	}
	applied, _, _ := g.applyPolicy(p, g.now())
	return applied
}

// applyPolicy installs p unless it is older than the current policy. It
// returns whether it did, the version in force afterwards and, when
// applied, the propagation latency (received minus p.IssuedAt). Without an
// IssuedAt the latency is unknown: it is reported as 0 and not observed in
// the propagation histogram.
func (g *Gateway) applyPolicy(p policy.Policy, received time.Time) (bool, int64, time.Duration) {
	g.mu.Lock()
	if p.Version < g.pol.Version {
		v := g.pol.Version
		g.mu.Unlock()
		return false, v, 0
	}
	p.Tenants = maps.Clone(p.Tenants)
	g.pol = p
	if g.sched != nil {
		g.sched.SetPolicy(p)
	}
	for t, s := range g.parts {
		s.SetPolicy(ownPolicy(p, t))
	}
	var lat time.Duration
	if !p.IssuedAt.IsZero() {
		lat = max(received.Sub(p.IssuedAt), 0)
	}
	g.polLat = lat
	g.mu.Unlock()

	g.metrics.policyVersion.Set(float64(p.Version))
	if !p.IssuedAt.IsZero() {
		g.metrics.policyLatency.Observe(lat.Seconds())
	}
	g.log.Info("policy applied", "version", p.Version, "tenants", len(p.Tenants), "propagationMs", ms(lat))
	g.kick()
	return true, p.Version, lat
}

// Policy returns a copy of the policy in force.
func (g *Gateway) Policy() policy.Policy {
	g.mu.Lock()
	defer g.mu.Unlock()
	p := g.pol
	p.Tenants = maps.Clone(p.Tenants)
	return p
}
