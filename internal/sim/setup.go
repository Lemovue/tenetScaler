package sim

import (
	"fmt"
	"math"
	"time"

	"tenantscaler/internal/autoscale"
	"tenantscaler/internal/config"
	"tenantscaler/internal/workload"
)

// Setup names: the evaluated stacks. Every setup serves the same scenario
// with the same seed, budget, engine and control cycle.
const (
	// SetupCPUHPA is a CPU-style HPA (engine slot occupancy as the CPU
	// proxy) behind a passthrough gateway.
	SetupCPUHPA = "S1-cpu-hpa"
	// SetupKEDAQueue is a KEDA queue-length scaler (gateway hold + replica
	// waiting queues) behind a passthrough gateway.
	SetupKEDAQueue = "S2-keda-queue"
	// SetupStaticPartition gives each tenant its own fixed replicas (the
	// pool stays at capacity; guarantees are the shares).
	SetupStaticPartition = "S3-static-partition"
	// SetupFlowControl is the llm-d Flow-Control-equivalent baseline: a
	// roundrobin gateway plus KEDA on the gateway queue length.
	SetupFlowControl = "S4-flowcontrol-equiv"
	// SetupNoGateway is TenantScaler without gateway coupling: the arbiter
	// sizes the pool, the gateway passes requests straight through.
	SetupNoGateway = "S5-ts-no-gateway"
	// SetupNoBorrowing is TenantScaler without borrowing of idle shares.
	SetupNoBorrowing = "S6-ts-no-borrowing"
	// SetupFull is TenantScaler: arbiter + stabilizer + fair gateway.
	SetupFull = "S7-ts-full"
	// SetupFullForecast is SetupFull plus the Holt-Winters forecaster.
	SetupFullForecast = "S7F-ts-full-forecast"
	// SetupNaiveDemand is SetupFull with naive demand: admitted (not shed)
	// weighted tokens instead of offered ones, the death-spiral ablation
	// (BRIEF-v2 Q11).
	SetupNaiveDemand = "S8-ts-naive-demand"
	// SetupFlowControlOutstanding is a sensitivity variant of
	// SetupFlowControl, not one of the specified setups: the same
	// roundrobin gateway with KEDA on outstanding requests (gateway queue
	// plus dispatched, unfinished requests). SetupFlowControl's metric
	// cannot see requests inside the dispatch window, so it can scale down
	// while the pool is saturated; this variant shows how much of
	// SetupFlowControl's result is due to that.
	SetupFlowControlOutstanding = "S4b-flowcontrol-outstanding"
	// SetupNoReservation is SetupFull without slot reservation: a tenant
	// over its share may take any free slot (as before reservation was
	// added); it shows what the reservation costs and buys.
	SetupNoReservation = "S7nr-ts-no-reservation"
)

// AllSetups lists every specified setup (DESIGN section 14) in report
// order.
var AllSetups = []string{
	SetupCPUHPA, SetupKEDAQueue, SetupStaticPartition, SetupFlowControl,
	SetupNoGateway, SetupNoBorrowing, SetupFull, SetupFullForecast, SetupNaiveDemand,
}

// SensitivitySetups are extra setups that probe a baseline's design
// choice; reports list them after AllSetups and label them as variants.
var SensitivitySetups = []string{SetupFlowControlOutstanding, SetupNoReservation}

// Gateway modes the simulator models (DESIGN section 13).
const (
	// GatewayPassthrough forwards every request at once, round robin over
	// ready replicas; replicas queue FCFS; nothing is shed.
	GatewayPassthrough = "passthrough"
	// GatewayFair is the fairq weighted-VTC scheduler with a dispatch window.
	GatewayFair = "fair"
	// GatewayRoundRobin is the fairq roundrobin scheduler with a dispatch
	// window.
	GatewayRoundRobin = "roundrobin"
	// GatewayPartition is a per-tenant FIFO on the tenant's own replicas.
	GatewayPartition = "partition"
)

// scalerKind selects the control plane of a setup.
type scalerKind uint8

const (
	scalerNone            scalerKind = iota // fixed pool
	scalerHPA                               // autoscale.HPA on slot occupancy
	scalerKEDATotal                         // autoscale.KEDA on all queued requests
	scalerKEDAGateway                       // autoscale.KEDA on the gateway queue
	scalerKEDAOutstanding                   // autoscale.KEDA on gateway queue + dispatched
	scalerCore                              // control.Core
)

// setupSpec is the static description of one setup.
type setupSpec struct {
	gateway, control string
	scaler           scalerKind
	// adjust edits the controller config for control.Core setups.
	adjust func(c *config.ControllerSpec)
}

var setupSpecs = map[string]setupSpec{
	SetupCPUHPA:          {GatewayPassthrough, "CPU-style HPA on engine slot occupancy (target 0.7)", scalerHPA, nil},
	SetupKEDAQueue:       {GatewayPassthrough, "KEDA on total queued requests (4 per replica)", scalerKEDATotal, nil},
	SetupStaticPartition: {GatewayPartition, "none: pool fixed at capacity, guarantees as shares", scalerNone, nil},
	SetupFlowControl:     {GatewayRoundRobin, "KEDA on the gateway queue length (4 per replica)", scalerKEDAGateway, nil},
	SetupNoGateway: {GatewayPassthrough, "TenantScaler controller, no gateway policy", scalerCore,
		func(c *config.ControllerSpec) { c.DisableGatewayCoupling = true }},
	SetupNoBorrowing: {GatewayFair, "TenantScaler controller without borrowing", scalerCore,
		func(c *config.ControllerSpec) { c.DisableBorrowing = true }},
	SetupFull:         {GatewayFair, "TenantScaler controller", scalerCore, func(*config.ControllerSpec) {}},
	SetupFullForecast: {GatewayFair, "TenantScaler controller + Holt-Winters forecaster", scalerCore, func(*config.ControllerSpec) {}},
	SetupNaiveDemand: {GatewayFair, "TenantScaler controller with naive demand (admitted tokens only; death-spiral ablation)", scalerCore,
		func(c *config.ControllerSpec) { c.NaiveDemand = true }},
	SetupFlowControlOutstanding: {GatewayRoundRobin,
		"sensitivity variant of S4: KEDA on outstanding requests, gateway queue + dispatched (4 per replica)", scalerKEDAOutstanding, nil},
	SetupNoReservation: {GatewayFair, "sensitivity variant of S7: TenantScaler controller without slot reservation", scalerCore,
		func(c *config.ControllerSpec) { c.DisableSlotReservation = true }},
}

// Defaults applied by Resolve to zero-valued Options fields.
const (
	// DefaultInitialReplicas is the warm pool every scaling setup starts
	// with (clamped to [MinReplicas, capacity]).
	DefaultInitialReplicas = 6
	// DefaultSampleEvery is the timeline resolution.
	DefaultSampleEvery = time.Second
	// DefaultPolicyDelay is the simulated controller-to-gateway policy
	// propagation delay.
	DefaultPolicyDelay = 20 * time.Millisecond
	// DefaultDispatchTimeout is the live gateway's default dispatch
	// timeout (internal/gateway DefaultDispatchTimeout).
	DefaultDispatchTimeout = 120 * time.Second
	// DrainLimit is how long after the scenario ends unfinished requests
	// may still complete; anything unfinished then is recorded as Failed.
	DrainLimit = 300 * time.Second
)

// Forecaster hyperparameters of SetupFullForecast, fixed a priori (not
// tuned on any evaluation data).
const (
	ForecastAlpha = 0.5
	ForecastBeta  = 0.1
	ForecastGamma = 0.3
)

// Settings are the effective parameters of one run after defaults.
type Settings struct {
	Setup string
	// Gateway is one of the Gateway* modes; ControlPlane describes the
	// scaler in words.
	Gateway, ControlPlane string
	InitialReplicas       int
	ColdStart             time.Duration
	SampleEvery           time.Duration
	PolicyDelay           time.Duration
	DispatchTimeout       time.Duration
	SlotsPerReplica       int
	// HPA and KEDA are the baseline scaler configurations (used by S1, and
	// S2/S4/S4b respectively); Controller is the control.Core
	// configuration (used by S5-S7F) after the setup's ablation switches.
	HPA        autoscale.HPAConfig
	KEDA       autoscale.KEDAConfig
	Controller config.ControllerSpec
}

// Info describes a setup's gateway mode and control plane. ok is false for
// an unknown setup name.
func Info(setup string) (gateway, controlPlane string, ok bool) {
	s, ok := setupSpecs[setup]
	return s.gateway, s.control, ok
}

// Resolve validates o and returns the effective settings of the run.
func Resolve(o Options) (Settings, error) {
	spec, ok := setupSpecs[o.Setup]
	if !ok {
		return Settings{}, fmt.Errorf("unknown setup %q (want one of %v or %v)", o.Setup, AllSetups, SensitivitySetups)
	}
	if err := o.Config.Validate(); err != nil {
		return Settings{}, fmt.Errorf("config: %w", err)
	}
	if err := o.Scenario.Validate(); err != nil {
		return Settings{}, err
	}
	known := map[string]bool{}
	for _, t := range o.Config.Tenants {
		known[t.Name] = true
	}
	for _, p := range o.Scenario.Tenants {
		if !known[p.Tenant] {
			return Settings{}, fmt.Errorf("scenario tenant %q is not in the config (the gateway would answer 400)", p.Tenant)
		}
	}
	switch {
	case o.ColdStart < 0:
		return Settings{}, fmt.Errorf("cold start must be >= 0, got %v", o.ColdStart)
	case o.InitialReplicas < 0:
		return Settings{}, fmt.Errorf("initial replicas must be >= 0, got %d", o.InitialReplicas)
	case o.SampleEvery < 0 || o.PolicyDelay < 0 || o.DispatchTimeout < 0:
		return Settings{}, fmt.Errorf("sample interval, policy delay and dispatch timeout must be >= 0")
	}
	pool := o.Config.Pool
	st := Settings{
		Setup: o.Setup, Gateway: spec.gateway, ControlPlane: spec.control,
		InitialReplicas: o.InitialReplicas, ColdStart: o.ColdStart,
		SampleEvery: o.SampleEvery, PolicyDelay: o.PolicyDelay, DispatchTimeout: o.DispatchTimeout,
		SlotsPerReplica: o.Config.SlotsPerReplica(),
		HPA:             autoscale.DefaultHPAConfig(pool.MinReplicas, pool.CapacityReplicas),
		KEDA:            autoscale.DefaultKEDAConfig(pool.MinReplicas, pool.CapacityReplicas),
		Controller:      o.Config.Controller,
	}
	if st.InitialReplicas == 0 {
		st.InitialReplicas = DefaultInitialReplicas
	}
	st.InitialReplicas = min(max(st.InitialReplicas, pool.MinReplicas), pool.CapacityReplicas)
	if spec.scaler == scalerNone {
		st.InitialReplicas = pool.CapacityReplicas
	}
	if st.ColdStart == 0 {
		p, err := o.Config.Engine.Params()
		if err != nil {
			return Settings{}, err
		}
		st.ColdStart = p.StartupDuration
	}
	if st.SampleEvery == 0 {
		st.SampleEvery = DefaultSampleEvery
	}
	if st.PolicyDelay == 0 {
		st.PolicyDelay = DefaultPolicyDelay
	}
	if st.DispatchTimeout == 0 {
		st.DispatchTimeout = DefaultDispatchTimeout
	}
	st.Controller.Forecast.Enabled = false
	if spec.adjust != nil {
		spec.adjust(&st.Controller)
	}
	if o.Setup == SetupFullForecast {
		st.Controller.Forecast = ForecastSpecFor(o.Config.Controller, o.Scenario)
	}
	return st, nil
}

// ForecastSpecFor returns the forecaster configuration of
// SetupFullForecast: one bucket per control cycle, a season equal to the
// first periodic tenant's period in buckets (at least 2; the configured
// season when the scenario has no periodic tenant), and the fixed
// hyperparameters ForecastAlpha/Beta/Gamma.
func ForecastSpecFor(ctl config.ControllerSpec, sc workload.Scenario) config.ForecastSpec {
	season := max(ctl.Forecast.SeasonBuckets, 2)
	for _, p := range sc.Tenants {
		if p.Kind == workload.KindPeriodic && ctl.CycleSeconds > 0 {
			season = max(2, int(math.Round(p.PeriodSeconds/ctl.CycleSeconds)))
			break
		}
	}
	return config.ForecastSpec{
		Enabled: true, BucketSeconds: ctl.CycleSeconds, SeasonBuckets: season,
		Alpha: ForecastAlpha, Beta: ForecastBeta, Gamma: ForecastGamma,
	}
}
