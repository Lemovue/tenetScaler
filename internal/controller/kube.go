package controller

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"tenantscaler/internal/config"
	"tenantscaler/internal/control"
	"tenantscaler/internal/k8s"
	"tenantscaler/internal/model"
)

// KubeTenants binds a Runner to TenantScaler resources (Kubernetes mode):
// BeforeCycle reloads the tenant set into the core every cycle and
// AfterDecision writes each tenant's outcome to its resource's status.
// BeforeCycle changes Core, so call it only from Runner cycles, which
// serialize access to the core; Load and Wait may be called at any time.
type KubeTenants struct {
	Client *k8s.Client
	// Pool selects the TenantScalers whose spec.pool matches.
	Pool string
	// Capacity is the pool's replica budget the guarantees are checked
	// against.
	Capacity int
	// Core receives the tenant set; it must be set before BeforeCycle runs.
	Core *control.Core
	// Logger receives status-write failures; nil means slog.Default().
	Logger *slog.Logger

	mu          sync.Mutex
	generations map[string]int64                  // metadata.generation of the tenants in use
	last        map[string]k8s.TenantScalerStatus // last status written, per tenant
}

// Load lists this pool's TenantScalers and returns their specs, sorted by
// name. An empty or invalid set (e.g. guarantees above Capacity) is an
// error. On success the resources' generations are remembered for
// status.observedGeneration.
func (k *KubeTenants) Load(ctx context.Context) ([]model.TenantSpec, error) {
	specs, gens, err := k.load(ctx)
	if err != nil {
		return nil, err
	}
	k.setGenerations(gens)
	return specs, nil
}

// load lists and validates the tenants without changing any state.
func (k *KubeTenants) load(ctx context.Context) ([]model.TenantSpec, map[string]int64, error) {
	if k.Client == nil {
		return nil, nil, errors.New("kubernetes tenants: no client")
	}
	list, err := k.Client.ListTenants(ctx, k.Pool)
	if err != nil {
		return nil, nil, err
	}
	if len(list) == 0 {
		return nil, nil, fmt.Errorf("no %s resources for pool %q in namespace %s", k8s.Kind, k.Pool, k.Client.Namespace)
	}
	specs := make([]model.TenantSpec, len(list))
	gens := make(map[string]int64, len(list))
	for i, t := range list {
		specs[i] = t.TenantSpec()
		gens[t.Name] = t.Generation
	}
	if err := config.ValidateTenants(specs, k.Capacity); err != nil {
		return nil, nil, fmt.Errorf("%s resources for pool %q: %w", k8s.Kind, k.Pool, err)
	}
	return specs, gens, nil
}

func (k *KubeTenants) setGenerations(gens map[string]int64) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.generations = gens
}

// Wait calls Load until it succeeds, retrying every interval and logging
// each failure, so the controller can start before its tenants exist. It
// returns ctx's error if ctx ends first.
func (k *KubeTenants) Wait(ctx context.Context, every time.Duration) ([]model.TenantSpec, error) {
	for {
		specs, err := k.Load(ctx)
		if err == nil {
			return specs, nil
		}
		k.logger().Warn("waiting for tenants", "pool", k.Pool, "retryIn", every, "err", err)
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(every):
		}
	}
}

// BeforeCycle reloads the tenants into Core. On error the core keeps its
// previous tenants; the Runner reports the error.
func (k *KubeTenants) BeforeCycle(ctx context.Context) error {
	if k.Core == nil {
		return errors.New("kubernetes tenants: no core")
	}
	specs, gens, err := k.load(ctx)
	if err == nil {
		err = k.Core.SetTenants(specs)
	}
	if err != nil {
		return fmt.Errorf("reload tenants (keeping the previous set): %w", err)
	}
	k.setGenerations(gens)
	return nil
}

// AfterDecision writes every tenant record of d to its TenantScaler status.
// A fail-static cycle keeps the last written outcome. Failures are logged;
// they do not affect the cycle.
func (k *KubeTenants) AfterDecision(ctx context.Context, d control.Decision) {
	if k.Client == nil {
		k.logger().Error("write tenant status: kubernetes tenants: no client")
		return
	}
	for _, rec := range d.Records {
		if rec.Tenant == control.PoolRecordTenant {
			continue
		}
		st := k.statusFor(rec)
		if err := k.Client.UpdateStatus(ctx, rec.Tenant, st); err != nil {
			k.logger().Warn("write tenant status", "tenant", rec.Tenant, "cycle", d.Cycle, "err", err)
			continue
		}
		k.mu.Lock()
		if k.last == nil {
			k.last = map[string]k8s.TenantScalerStatus{}
		}
		k.last[rec.Tenant] = st
		k.mu.Unlock()
	}
}

// PolicyVersion returns the newest status.policyVersion among this pool's
// TenantScalers: the last policy version a previous controller published.
func (k *KubeTenants) PolicyVersion(ctx context.Context) (int64, error) {
	if k.Client == nil {
		return 0, errors.New("kubernetes tenants: no client")
	}
	list, err := k.Client.ListTenants(ctx, k.Pool)
	if err != nil {
		return 0, err
	}
	var newest int64
	for _, t := range list {
		newest = max(newest, t.Status.PolicyVersion)
	}
	return newest, nil
}

// statusFor builds a tenant's status from its decision record. A
// fail-static cycle changes nothing, so it keeps the last written outcome
// (grant, shedding, demand, slack) and updates only the explanation: the
// record's own grant comes from the last policy, which does not exist when
// gateway coupling is off.
func (k *KubeTenants) statusFor(rec model.DecisionRecord) k8s.TenantScalerStatus {
	k.mu.Lock()
	defer k.mu.Unlock()
	st := k8s.TenantScalerStatus{
		Grant: rec.Final.Grant, Shedding: rec.Final.Shedding,
		Demand: rec.Inputs.Demand, SLOSlack: rec.Inputs.SLOSlack,
	}
	if prev, ok := k.last[rec.Tenant]; ok && rec.FailStatic {
		st = prev
	}
	st.Reason = rec.Reason
	st.PolicyVersion = rec.PolicyVersion
	st.ObservedGeneration = k.generations[rec.Tenant]
	st.LastUpdate = metav1.NewTime(rec.Time)
	return st
}

func (k *KubeTenants) logger() *slog.Logger {
	if k.Logger != nil {
		return k.Logger
	}
	return slog.Default()
}
