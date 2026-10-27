// Package control is the controller's decision core. It is pure and
// clock-agnostic: every input, including the time, arrives in a
// model.Observation, so the live controller and the discrete-event simulator
// run exactly the same logic.
//
// One Step is one control cycle: estimate each tenant's demand from its
// token rate (optionally raised by a demand forecast), arbitrate the scarce
// replicas, stabilize the pool size, and build the gateway policy plus one
// explained decision record per tenant. Missing metrics make it fail static.
package control

import (
	"fmt"
	"math"
	"strings"
	"time"

	"tenantscaler/internal/arbiter"
	"tenantscaler/internal/config"
	"tenantscaler/internal/estimate"
	"tenantscaler/internal/forecast"
	"tenantscaler/internal/model"
	"tenantscaler/internal/policy"
	"tenantscaler/internal/stabilize"
)

// PoolRecordTenant is the Tenant value of the pool-level decision record.
const PoolRecordTenant = "(pool)"

// trackerWindow is how many one-step forecasts must be scored before a
// tenant's forecast is trusted.
const trackerWindow = 4

// Decision is the outcome of one control cycle.
type Decision struct {
	Cycle int64     `json:"cycle"`
	Time  time.Time `json:"time"`
	// DesiredReplicas is clamp(sum of grants, MinReplicas, capacity).
	DesiredReplicas int `json:"desiredReplicas"`
	// PoolReplicas is the stabilized replica count the actuator applies.
	PoolReplicas int `json:"poolReplicas"`
	// Policy is nil when gateway coupling is disabled.
	Policy     *policy.Policy         `json:"policy,omitempty"`
	Records    []model.DecisionRecord `json:"records"`
	FailStatic bool                   `json:"failStatic"`
}

// State is the part of the core that survives a controller restart.
type State struct {
	Cycle         int64          `json:"cycle"`
	PolicyVersion int64          `json:"policyVersion"`
	LastPool      int            `json:"lastPool"`
	LastPolicy    *policy.Policy `json:"lastPolicy,omitempty"`
	// LastGrants are the replica grants behind LastPolicy, by tenant.
	LastGrants map[string]int  `json:"lastGrants,omitempty"`
	Stabilizer stabilize.State `json:"stabilizer"`
}

// Core holds the controller's state between cycles. It is not safe for
// concurrent use.
type Core struct {
	cfg        config.Config
	coldStart  time.Duration
	tenants    []*tenant
	stab       *stabilize.Stabilizer
	cycle      int64
	version    int64
	lastPool   int
	lastPolicy *policy.Policy
	lastGrants map[string]int
	// adopted is set once the core has taken the running pool's size as
	// its starting point (first observation that reports a pool size).
	adopted bool
}

// tenant is per-tenant controller state.
type tenant struct {
	spec model.TenantSpec
	fc   *demandForecast
}

// NewCore validates cfg and returns a core. coldStart is the replica load
// time; it sets how far ahead the forecaster looks.
func NewCore(cfg config.Config, coldStart time.Duration) (*Core, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if coldStart < 0 {
		return nil, fmt.Errorf("cold start must be >= 0")
	}
	c := &Core{cfg: cfg, coldStart: coldStart, lastPool: cfg.Pool.MinReplicas}
	c.stab = stabilize.New(c.stabConfig(), cfg.Pool.MinReplicas)
	if err := c.SetTenants(cfg.Tenants); err != nil {
		return nil, err
	}
	return c, nil
}

// SetTenants replaces the tenant set (Kubernetes mode: TenantScaler CRs can
// change). Forecast history is kept for tenants that remain.
func (c *Core) SetTenants(specs []model.TenantSpec) error {
	if err := config.ValidateTenants(specs, c.cfg.Pool.CapacityReplicas); err != nil {
		return err
	}
	old := make(map[string]*tenant, len(c.tenants))
	for _, t := range c.tenants {
		old[t.spec.Name] = t
	}
	next := make([]*tenant, 0, len(specs))
	for _, s := range specs {
		t, ok := old[s.Name]
		if !ok {
			t = &tenant{}
			if f := c.cfg.Controller.Forecast; f.Enabled {
				t.fc = newDemandForecast(f)
			}
		}
		t.spec = s
		next = append(next, t)
	}
	c.tenants = next
	c.cfg.Tenants = specs
	return nil
}

// Step runs one control cycle on obs.
func (c *Core) Step(obs model.Observation) Decision {
	c.cycle++
	if !c.adopted && obs.Pool.DesiredReplicas > 0 {
		// Adopt the pool's current size the first time it is reported (fresh
		// start or restart) so stabilization starts from reality.
		c.adopted = true
		c.lastPool = clamp(obs.Pool.DesiredReplicas, c.cfg.Pool.MinReplicas, c.cfg.Pool.CapacityReplicas)
		c.stab = stabilize.Restore(c.stabConfig(), stabilize.State{Current: c.lastPool})
	}
	if !obs.OK {
		return c.failStatic(obs, obs.Error)
	}

	ctl := c.cfg.Controller
	byName := make(map[string]model.TenantObservation, len(obs.Tenants))
	for _, o := range obs.Tenants {
		byName[o.Tenant] = o
	}
	inputs := make([]arbiter.Input, len(c.tenants))
	details := make([]model.DecisionInputs, len(c.tenants))
	for i, t := range c.tenants {
		o := byName[t.spec.Name]
		// Demand is the OFFERED load: every arrival, shed ones included, so
		// shedding never lowers the demand it responds to (BRIEF-v2 Q11).
		// The S8 ablation counts admitted requests only.
		rate := math.Max(o.ArrivalWeightedTokensPerSec, 0)
		if ctl.NaiveDemand {
			rate = math.Max(o.AdmittedWeightedTokensPerSec, 0)
		}
		reactive := estimate.Replicas(rate, ctl.Headroom, ctl.ReplicaCapacityTokensPerSec)
		demand, forecastDemand := reactive, 0
		if t.fc != nil {
			if peak, ok := t.fc.update(obs.Time, rate, c.horizonBuckets()); ok {
				forecastDemand = estimate.Replicas(peak, ctl.Headroom, ctl.ReplicaCapacityTokensPerSec)
				demand = max(demand, forecastDemand)
			}
		}
		slack := estimate.Slack(o, t.spec.SLO)
		inputs[i] = arbiter.Input{
			Name: t.spec.Name, Demand: demand, Guaranteed: t.spec.GuaranteedReplicas,
			MaxReplicas: t.spec.MaxReplicas, Priority: t.spec.Priority, SLOSlack: slack,
		}
		details[i] = model.DecisionInputs{
			Demand: demand, ReactiveDemand: reactive, ForecastDemand: forecastDemand,
			Guaranteed: t.spec.GuaranteedReplicas, MaxReplicas: t.spec.MaxReplicas,
			Priority: t.spec.Priority, SLOSlack: slack, WeightedTokenRate: rate,
		}
	}

	results, err := arbiter.Arbitrate(c.cfg.Pool.CapacityReplicas, inputs, arbiter.Options{Borrowing: !ctl.DisableBorrowing})
	if err != nil {
		return c.failStatic(obs, "arbiter rejected inputs: "+err.Error())
	}
	sum := 0
	for _, r := range results {
		sum += r.Grant
	}
	desired := clamp(sum, c.cfg.Pool.MinReplicas, c.cfg.Pool.CapacityReplicas)
	pool := c.stab.Next(obs.Time, desired)

	var pol *policy.Policy
	if !ctl.DisableGatewayCoupling {
		c.version++
		pol = &policy.Policy{Version: c.version, IssuedAt: obs.Time, Tenants: make(map[string]policy.TenantPolicy, len(results))}
		slots := c.cfg.SlotsPerReplica()
		for i, r := range results {
			guaranteed := c.tenants[i].spec.GuaranteedReplicas
			pol.Tenants[r.Name] = policy.TenantPolicy{
				Weight:            gatewayWeight(r.Grant, guaranteed),
				Shed:              r.Shedding,
				RetryAfterSeconds: ctl.RetryAfterSeconds,
				CriticalSlots:     guaranteed * slots,
			}
			if !ctl.DisableSlotReservation {
				tp := pol.Tenants[r.Name]
				tp.Slots, tp.GuaranteedSlots = r.Grant*slots, guaranteed*slots
				pol.Tenants[r.Name] = tp
			}
		}
	}

	records := make([]model.DecisionRecord, 0, len(results)+1)
	for i, r := range results {
		weight := 0.0
		if pol != nil {
			weight = pol.Tenants[r.Name].Weight
		}
		records = append(records, model.DecisionRecord{
			Cycle: c.cycle, Time: obs.Time, Tenant: r.Name, Inputs: details[i], Steps: r.Steps,
			Final:         model.DecisionFinal{Grant: r.Grant, Shedding: r.Shedding, Weight: weight},
			Reason:        r.Reason + c.couplingNote(r, pol),
			PolicyVersion: c.version,
		})
	}
	records = append(records, c.poolRecord(obs.Time, sum, desired, pool))

	c.lastPool = pool
	if pol != nil {
		c.lastPolicy = pol
		c.lastGrants = make(map[string]int, len(results))
		for _, r := range results {
			c.lastGrants[r.Name] = r.Grant
		}
	}
	return Decision{
		Cycle: c.cycle, Time: obs.Time, DesiredReplicas: desired, PoolReplicas: pool,
		Policy: pol, Records: records,
	}
}

// State returns the restart-surviving state.
func (c *Core) State() State {
	return State{
		Cycle: c.cycle, PolicyVersion: c.version, LastPool: c.lastPool,
		LastPolicy: c.lastPolicy, LastGrants: c.lastGrants, Stabilizer: c.stab.State(),
	}
}

// Restore loads state saved by State (e.g. from TenantScaler status).
func (c *Core) Restore(s State) {
	c.cycle, c.version, c.lastPool, c.lastPolicy = s.Cycle, s.PolicyVersion, s.LastPool, s.LastPolicy
	c.lastGrants = s.LastGrants
	c.stab = stabilize.Restore(c.stabConfig(), s.Stabilizer)
	// A state saved after at least one cycle already describes the running
	// pool; a blank one (e.g. only the policy version was seeded) does not.
	c.adopted = s.Cycle > 0
}

func (c *Core) failStatic(obs model.Observation, why string) Decision {
	if why == "" {
		why = "no data"
	}
	reason := fmt.Sprintf("Metrics unavailable (%s): holding %d replicas and policy v%d.", why, c.lastPool, c.version)
	records := make([]model.DecisionRecord, 0, len(c.tenants))
	for _, t := range c.tenants {
		final := model.DecisionFinal{}
		if c.lastPolicy != nil {
			p := c.lastPolicy.Tenants[t.spec.Name]
			final = model.DecisionFinal{Grant: c.lastGrants[t.spec.Name], Shedding: p.Shed, Weight: p.Weight}
		}
		records = append(records, model.DecisionRecord{
			Cycle: c.cycle, Time: obs.Time, Tenant: t.spec.Name, Final: final,
			Reason: reason, PolicyVersion: c.version, FailStatic: true,
		})
	}
	return Decision{
		Cycle: c.cycle, Time: obs.Time, DesiredReplicas: c.lastPool, PoolReplicas: c.lastPool,
		Policy: c.lastPolicy, Records: records, FailStatic: true,
	}
}

// gatewayWeight is a tenant's VTC weight at the gateway: its grant, but
// never less than its guarantee. A tenant whose demand fell to zero this
// cycle lends its guaranteed replicas, yet keeps its guaranteed share at
// the gateway: when it returns, its requests are charged tokens/guarantee,
// not tokens/MinWeight, which would push its counter far ahead of the
// borrower's and starve it until the next cycle. The gateway thus reclaims
// at once; the next cycle re-provisions the replicas.
func gatewayWeight(grant, guaranteed int) float64 {
	return math.Max(float64(max(grant, guaranteed)), policy.MinWeight)
}

func (c *Core) couplingNote(r arbiter.Result, pol *policy.Policy) string {
	if pol == nil {
		return " Gateway coupling is off (ablation): the gateway does not enforce this share."
	}
	if r.Shedding {
		return fmt.Sprintf(" Gateway policy v%d: weight %.0f, sheddable requests get 429.", pol.Version, pol.Tenants[r.Name].Weight)
	}
	return fmt.Sprintf(" Gateway policy v%d: weight %.2g, no shedding.", pol.Version, pol.Tenants[r.Name].Weight)
}

func (c *Core) poolRecord(now time.Time, sum, desired, pool int) model.DecisionRecord {
	steps := []model.DecisionStep{
		{Rule: "sum-of-grants", Grant: sum},
		{Rule: "clamp-to-budget", Grant: desired, Note: fmt.Sprintf("min %d, capacity %d", c.cfg.Pool.MinReplicas, c.cfg.Pool.CapacityReplicas)},
		{Rule: "stabilize", Grant: pool},
	}
	var why []string
	switch {
	case pool < desired:
		why = append(why, fmt.Sprintf("scale-up limited to %d replicas per cycle", c.cfg.Controller.MaxStepPerCycle))
	case pool > desired:
		why = append(why, fmt.Sprintf("scale-down waits for demand to stay low for %gs", c.cfg.Controller.ScaleDownWindowSeconds))
	}
	reason := fmt.Sprintf("Pool: grants add up to %d, desired %d of %d, applying %d.", sum, desired, c.cfg.Pool.CapacityReplicas, pool)
	if len(why) > 0 {
		reason += " " + strings.ToUpper(why[0][:1]) + why[0][1:] + "."
	}
	return model.DecisionRecord{
		Cycle: c.cycle, Time: now, Tenant: PoolRecordTenant, Steps: steps,
		Final: model.DecisionFinal{Grant: pool}, Reason: reason, PolicyVersion: c.version,
	}
}

func (c *Core) stabConfig() stabilize.Config {
	ctl := c.cfg.Controller
	return stabilize.Config{
		ScaleDownWindow: time.Duration(ctl.ScaleDownWindowSeconds * float64(time.Second)),
		MaxStep:         ctl.MaxStepPerCycle,
		Min:             c.cfg.Pool.MinReplicas,
		Max:             c.cfg.Pool.CapacityReplicas,
	}
}

// horizonBuckets is how many forecast buckets ahead a new replica would be
// useful: one cold start plus one control cycle.
func (c *Core) horizonBuckets() int {
	bucket := c.cfg.Controller.Forecast.BucketSeconds
	lead := c.coldStart.Seconds() + c.cfg.Controller.CycleSeconds
	return max(1, int(math.Ceil(lead/bucket)))
}

func clamp(v, lo, hi int) int { return min(max(v, lo), hi) }

// demandForecast buckets a tenant's observed token rate and forecasts it
// with Holt-Winters, trusting the forecast only after it has been accurate.
type demandForecast struct {
	bucket      time.Duration
	hw          *forecast.HoltWinters
	tracker     *forecast.Tracker
	start       time.Time
	sum         float64
	n           int
	pending     float64 // one-step forecast for the bucket being filled
	havePending bool
}

func newDemandForecast(f config.ForecastSpec) *demandForecast {
	return &demandForecast{
		bucket:  time.Duration(f.BucketSeconds * float64(time.Second)),
		hw:      forecast.NewHoltWinters(f.Alpha, f.Beta, f.Gamma, f.SeasonBuckets),
		tracker: forecast.NewTracker(trackerWindow),
	}
}

// update adds one rate sample. When a bucket closes it is scored against the
// previous one-step forecast and fed to Holt-Winters. It returns the peak
// forecast over the next horizon buckets, and ok=false while the forecaster
// is not ready or not trusted.
func (d *demandForecast) update(now time.Time, rate float64, horizon int) (float64, bool) {
	if d.n > 0 && now.Sub(d.start) >= d.bucket {
		avg := d.sum / float64(d.n)
		if d.havePending {
			d.tracker.Record(d.pending, avg)
		}
		d.hw.Observe(avg)
		d.sum, d.n = 0, 0
		d.havePending = d.hw.Ready()
		if d.havePending {
			d.pending = d.hw.Forecast(1)
		}
	}
	if d.n == 0 {
		d.start = now
	}
	d.sum += rate
	d.n++
	if !d.hw.Ready() || !d.tracker.Trusted() {
		return 0, false
	}
	peak := 0.0
	for h := 1; h <= horizon; h++ {
		peak = math.Max(peak, d.hw.Forecast(h))
	}
	return peak, true
}
