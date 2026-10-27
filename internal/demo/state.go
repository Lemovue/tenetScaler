package demo

import (
	"math"
	"time"

	"tenantscaler/internal/eval"
	"tenantscaler/internal/gateway"
	"tenantscaler/internal/model"
	"tenantscaler/internal/stack"
)

// Phases of an A/B run (State.Phase).
const (
	// PhaseIdle: no A/B run has started yet.
	PhaseIdle = "idle"
	// PhaseRunning: a stack is starting, replaying traffic or stopping.
	PhaseRunning = "running"
	// PhaseDone: both stacks finished and have metrics.
	PhaseDone = "done"
	// PhaseStopped: the run was stopped (POST /api/ab/stop or shutdown);
	// State.Error is set when shutting the stack down reported an error.
	PhaseStopped = "stopped"
	// PhaseError: a stack failed; State.Error says why.
	PhaseError = "error"
)

// Statuses of one stack (StackState.Status).
const (
	StatusPending  = "pending"  // not started yet
	StatusStarting = "starting" // warm replicas loading
	StatusRunning  = "running"  // replaying the trace
	StatusDraining = "draining" // trace sent, requests still in flight
	StatusStopping = "stopping" // shutting the stack down
	StatusDone     = "done"     // finished, metrics computed
	StatusStopped  = "stopped"  // stopped before finishing
	StatusSkipped  = "skipped"  // never started (the run ended earlier)
	StatusFailed   = "error"    // failed; StackState.Error says why
)

// State is what GET /api/state serves.
type State struct {
	Phase string `json:"phase"`
	// Message says in words what is happening; Error is set in PhaseError,
	// and in PhaseStopped when shutting the stack down reported an error
	// (replicas may have survived).
	Message string `json:"message"`
	Error   string `json:"error,omitempty"`
	// Progress is the share of the A/B run completed, in [0, 1].
	Progress float64 `json:"progress"`
	// Run counts the A/B runs started since the server began (0 = none);
	// Seed is the current or last run's seed.
	Run       int          `json:"run"`
	Seed      int64        `json:"seed"`
	StartedAt *time.Time   `json:"startedAt,omitempty"`
	Backend   BackendInfo  `json:"backend"`
	Scenario  ScenarioInfo `json:"scenario"`
	// WindowSeconds is the window of the live per-tenant statistics.
	WindowSeconds float64      `json:"windowSeconds"`
	Stacks        []StackState `json:"stacks"`
	// Live marks a live run (StartLive): its traffic comes from teammates'
	// laptops, LiveLabel; LiveAddress is its public gateway address.
	Live        bool   `json:"live"`
	LiveAddress string `json:"liveAddress,omitempty"`
}

// StackState is one side of the A/B comparison.
type StackState struct {
	// Key is KeyBaseline or KeyTenantScaler; Setup the evaluation setup
	// (S2-keda-queue, S7-ts-full); Gateway its gateway mode and
	// ControlPlane its autoscaler in words.
	Key          string `json:"key"`
	Setup        string `json:"setup"`
	Title        string `json:"title"`
	Gateway      string `json:"gateway"`
	ControlPlane string `json:"controlPlane"`
	// Status is one of the Status* constants. Error says why the stack
	// failed, or, for a stopped stack, what shutting it down reported.
	Status string `json:"status"`
	Error  string `json:"error,omitempty"`
	// GatewayURL is the stack gateway's address while the stack runs
	// (empty before it starts and once it has stopped), for inspecting
	// its /stats or /healthz directly; it listens on 127.0.0.1 only.
	GatewayURL string `json:"gatewayUrl,omitempty"`
	// Engine is what served the requests, reported once the stack runs;
	// Simulated is true when any response came from simulated compute.
	Engine    *stack.EngineInfo `json:"engine,omitempty"`
	Simulated bool              `json:"simulated"`
	// TrafficStartedAt is when the replay began; ElapsedSeconds the time
	// since then (frozen when the replay ends).
	TrafficStartedAt *time.Time `json:"trafficStartedAt,omitempty"`
	ElapsedSeconds   float64    `json:"elapsedSeconds"`
	// Requests is the number of requests in the trace.
	Requests int `json:"requests"`
	// Samples are the live samples, one per sampling interval.
	Samples []LiveSample `json:"samples"`
	// Metrics are the run's evaluation metrics and Tenants its per-tenant
	// outcomes, both from the client-side records; nil until done.
	Metrics *eval.RunMetrics `json:"metrics"`
	Tenants []TenantResult   `json:"tenants"`
	// DrainTimedOut is true when requests were still running
	// Options.DrainTimeout after the trace ended (they count as failed).
	DrainTimedOut bool `json:"drainTimedOut,omitempty"`
}

// LiveSample is the state of a running stack at one instant.
type LiveSample struct {
	// T is the time since the replay began, in seconds.
	T float64 `json:"t"`
	// Ready (including draining), Starting and Desired replicas.
	Ready    int `json:"ready"`
	Starting int `json:"starting"`
	Desired  int `json:"desired"`
	// Tenants are the per-tenant statistics over the live window, as the
	// stack's gateway measured them; nil when the gateway could not be read.
	Tenants map[string]TenantWindow `json:"tenants"`
}

// TenantWindow is one tenant's live statistics over the window.
type TenantWindow struct {
	// P99TTFTSeconds and Attainment (0-1) cover the requests completed in
	// the window (failed admitted requests count as misses); nil when none
	// completed.
	P99TTFTSeconds *float64 `json:"p99TtftSeconds"`
	Attainment     *float64 `json:"attainment"`
	// RPS is the arrival rate (admitted and shed); Shed and Completed count
	// requests in the window.
	RPS       float64 `json:"rps"`
	Shed      int     `json:"shed"`
	Completed int     `json:"completed"`
	// Queued and InFlight are the tenant's requests right now.
	Queued   int `json:"queued"`
	InFlight int `json:"inFlight"`
}

// TenantResult is one tenant's outcome over a whole run, from the
// client-side records.
type TenantResult struct {
	Tenant    string `json:"tenant"`
	Requests  int    `json:"requests"`
	Completed int    `json:"completed"`
	Shed      int    `json:"shed"`
	Failed    int    `json:"failed"`
	// AttainmentPct and ShedPct are percentages (0-100) and P99TTFTSeconds
	// the nearest-rank p99 over completed requests; nil without data.
	AttainmentPct  *float64 `json:"attainmentPct"`
	ShedPct        *float64 `json:"shedPct"`
	P99TTFTSeconds *float64 `json:"p99TtftSeconds"`
}

// liveSample combines a stack sample with the gateway's windowed
// statistics (nil gs when they could not be read).
func liveSample(smp eval.Sample, gs *gateway.Stats) LiveSample {
	ls := LiveSample{T: seconds(smp.T), Ready: smp.Ready, Starting: smp.Starting, Desired: smp.Desired}
	if gs == nil {
		return ls
	}
	ls.Tenants = make(map[string]TenantWindow, len(gs.Tenants))
	for _, o := range gs.Tenants {
		tw := TenantWindow{RPS: o.ArrivalRequestsPerSec, Shed: o.Shed, Completed: o.Completed,
			Queued: smp.Queued[o.Tenant], InFlight: smp.InFlight[o.Tenant]}
		if o.HasSLOData {
			tw.P99TTFTSeconds, tw.Attainment = finite(o.P99TTFTSeconds), finite(o.SLOAttainment)
		}
		ls.Tenants[o.Tenant] = tw
	}
	return ls
}

// tenantResults summarizes every tenant's records over the whole run:
// attainment is the share of its non-shed requests that completed within
// the SLO (failed and unfinished requests count as misses), shed the share
// of all its requests answered 429.
func tenantResults(tenants []string, recs []eval.Record, slos map[string]model.SLO, m eval.RunMetrics) []TenantResult {
	out := make([]TenantResult, 0, len(tenants))
	for _, name := range tenants {
		tr := TenantResult{Tenant: name}
		met := 0
		for _, r := range recs {
			if r.Tenant != name {
				continue
			}
			tr.Requests++
			switch {
			case r.Shed:
				tr.Shed++
			case r.Completed():
				tr.Completed++
				if slo, ok := slos[name]; ok && r.MetSLO(slo) {
					met++
				}
			default:
				tr.Failed++
			}
		}
		if served := tr.Requests - tr.Shed; served > 0 {
			tr.AttainmentPct = finite(100 * float64(met) / float64(served))
		}
		if tr.Requests > 0 {
			tr.ShedPct = finite(100 * float64(tr.Shed) / float64(tr.Requests))
		}
		if v, ok := m.P99TTFT[name]; ok {
			tr.P99TTFTSeconds = finite(v)
		}
		out = append(out, tr)
	}
	return out
}

// finite returns &v, or nil when v is NaN or infinite.
func finite(v float64) *float64 {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return nil
	}
	return &v
}
