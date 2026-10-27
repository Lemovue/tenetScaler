package stack

import (
	"tenantscaler/internal/config"
	"tenantscaler/internal/gateway"
	"tenantscaler/internal/sim"
)

// controlKind selects the control plane of a setup.
type controlKind uint8

const (
	// controlNone keeps the pool at its initial size (static partition).
	controlNone controlKind = iota
	// controlKEDATotal is scalerun KEDA on the requests waiting inside the
	// replicas plus the gateway queue (setup S2).
	controlKEDATotal
	// controlKEDAGateway is scalerun KEDA on the gateway queue only (S4).
	controlKEDAGateway
	// controlCore is control.Core inside controller.Runner (S5-S7).
	controlCore
)

// setupSpec is the static description of one live setup.
type setupSpec struct {
	gateway string
	control controlKind
	// fullPool starts (and keeps) the pool at capacity.
	fullPool bool
	// describe says in words what controls the pool.
	describe string
	// adjust applies the setup's ablation switches to the controller
	// configuration (controlCore only).
	adjust func(c *config.ControllerSpec)
}

// Supported lists the setups Start accepts, in report order. The names are
// shared with the simulator (internal/sim).
var Supported = []string{
	sim.SetupKEDAQueue, sim.SetupStaticPartition, sim.SetupFlowControl,
	sim.SetupNoGateway, sim.SetupNoBorrowing, sim.SetupFull,
}

var specs = map[string]setupSpec{
	sim.SetupKEDAQueue: {gateway: gateway.ModePassthrough, control: controlKEDATotal,
		describe: "KEDA (scalerun) on queued requests: waiting inside the replicas + gateway queue, 4 per replica"},
	sim.SetupStaticPartition: {gateway: gateway.ModePartition, control: controlNone, fullPool: true,
		describe: "none: pool fixed at capacity, guaranteed replicas as each tenant's slice"},
	sim.SetupFlowControl: {gateway: gateway.ModeRoundRobin, control: controlKEDAGateway,
		describe: "KEDA (scalerun) on the gateway queue length, 4 per replica"},
	sim.SetupNoGateway: {gateway: gateway.ModePassthrough, control: controlCore,
		describe: "TenantScaler controller, no gateway policy (coupling disabled)",
		adjust:   func(c *config.ControllerSpec) { c.DisableGatewayCoupling = true }},
	sim.SetupNoBorrowing: {gateway: gateway.ModeFair, control: controlCore,
		describe: "TenantScaler controller without borrowing",
		adjust:   func(c *config.ControllerSpec) { c.DisableBorrowing = true }},
	sim.SetupFull: {gateway: gateway.ModeFair, control: controlCore,
		describe: "TenantScaler controller (arbiter + stabilizer) pushing policy to the fair gateway",
		adjust:   func(*config.ControllerSpec) {}},
}

// Describe returns a supported setup's gateway mode and, in words, its
// control plane; ok is false for any other name.
func Describe(setup string) (gatewayMode, controlPlane string, ok bool) {
	s, ok := specs[setup]
	return s.gateway, s.describe, ok
}

// HasController reports whether the setup runs the TenantScaler controller
// (and therefore keeps a decision log).
func HasController(setup string) bool { return specs[setup].control == controlCore }
