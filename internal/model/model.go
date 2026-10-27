// Package model holds the types shared by every TenantScaler component:
// request priority classes, token accounting, SLOs, tenant specs, per-tenant
// observations and decision records. It has no dependencies on other
// TenantScaler packages.
package model

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Priority is a request's priority class. It follows llm-d's
// InferenceObjective convention: an integer where negative values are
// sheddable, zero is standard and positive values are critical.
type Priority int

// Canonical priority values used by TenantScaler.
const (
	PrioritySheddable Priority = -1
	PriorityStandard  Priority = 0
	PriorityCritical  Priority = 2
)

// ParsePriority accepts "critical", "standard", "sheddable" (any case) or an
// integer. An empty string means standard.
func ParsePriority(s string) (Priority, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "standard":
		return PriorityStandard, nil
	case "critical":
		return PriorityCritical, nil
	case "sheddable":
		return PrioritySheddable, nil
	}
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil {
		return 0, fmt.Errorf("invalid priority %q: want critical|standard|sheddable or an integer", s)
	}
	return Priority(n), nil
}

// Class returns "critical", "standard" or "sheddable".
func (p Priority) Class() string {
	switch {
	case p < 0:
		return "sheddable"
	case p > 0:
		return "critical"
	default:
		return "standard"
	}
}

// Sheddable reports whether the gateway may reject this request with 429.
func (p Priority) Sheddable() bool { return p < 0 }

// Critical reports whether the request must never be shed.
func (p Priority) Critical() bool { return p > 0 }

// Token weights from VTC (Sheng et al., OSDI '24): output tokens cost twice
// as much as input tokens.
const (
	InputTokenWeight  = 1.0
	OutputTokenWeight = 2.0
)

// WeightedTokens is the VTC service cost of a request.
func WeightedTokens(in, out float64) float64 {
	return InputTokenWeight*in + OutputTokenWeight*out
}

// SLO is a tenant's latency promise: TargetFraction of requests must meet
// BOTH the time-to-first-token and time-per-output-token bounds.
type SLO struct {
	TTFTSeconds    float64 `json:"ttftSeconds"`
	TPOTMillis     float64 `json:"tpotMillis"`
	TargetFraction float64 `json:"targetFraction"`
}

// TTFT returns the time-to-first-token bound.
func (s SLO) TTFT() time.Duration { return seconds(s.TTFTSeconds) }

// TPOT returns the time-per-output-token bound.
func (s SLO) TPOT() time.Duration { return time.Duration(s.TPOTMillis * float64(time.Millisecond)) }

// Met reports whether a completed request with the given TTFT and mean TPOT
// meets this SLO. Requests that produced at most one token only need TTFT.
func (s SLO) Met(ttft, tpot time.Duration, outputTokens int) bool {
	if ttft > s.TTFT() {
		return false
	}
	return outputTokens <= 1 || tpot <= s.TPOT()
}

// TenantSpec is the configuration of one tenant. It is shared by the YAML
// config (local mode) and the TenantScaler CRD spec (Kubernetes mode).
type TenantSpec struct {
	Name               string `json:"name"`
	GuaranteedReplicas int    `json:"guaranteedReplicas"`
	MaxReplicas        int    `json:"maxReplicas"`
	// Priority orders tenants when spare capacity is lent: higher first.
	Priority int `json:"priority"`
	SLO      SLO `json:"slo"`
}

// TenantObservation is what the controller reads for one tenant each cycle.
// Rates are measured over the observation window ending at Observation.Time.
type TenantObservation struct {
	Tenant string `json:"tenant"`
	// ArrivalWeightedTokensPerSec counts every arriving request (admitted or
	// shed) so that demand reflects true load, not what was let through.
	ArrivalWeightedTokensPerSec float64 `json:"arrivalWeightedTokensPerSec"`
	// AdmittedWeightedTokensPerSec counts only the arrivals that were not
	// shed: the naive demand signal of the S8 ablation (BRIEF-v2 Q11), not
	// what the controller uses by default.
	AdmittedWeightedTokensPerSec float64 `json:"admittedWeightedTokensPerSec"`
	ArrivalRequestsPerSec        float64 `json:"arrivalRequestsPerSec"`
	QueueLength                  int     `json:"queueLength"`
	InFlight                     int     `json:"inFlight"`
	// SLOAttainment is the fraction of requests completed in the window that
	// met the SLO; HasSLOData is false when nothing completed. Admitted
	// requests that failed on the serving side (dispatch timeout, upstream
	// error, broken stream) count as completed misses, with the time until
	// failure as their TTFT (a lower bound).
	SLOAttainment  float64 `json:"sloAttainment"`
	HasSLOData     bool    `json:"hasSloData"`
	P99TTFTSeconds float64 `json:"p99TtftSeconds"`
	Completed      int     `json:"completed"`
	Shed           int     `json:"shed"`
}

// PoolObservation describes the shared replica pool.
type PoolObservation struct {
	ReadyReplicas    int `json:"readyReplicas"`
	StartingReplicas int `json:"startingReplicas"`
	DesiredReplicas  int `json:"desiredReplicas"`
}

// Observation is one controller input snapshot. OK is false when metrics
// could not be read; the controller then fails static.
type Observation struct {
	Time    time.Time           `json:"time"`
	OK      bool                `json:"ok"`
	Error   string              `json:"error,omitempty"`
	Tenants []TenantObservation `json:"tenants"`
	Pool    PoolObservation     `json:"pool"`
}

// DecisionStep is one rule the arbiter applied to a tenant.
type DecisionStep struct {
	Rule  string `json:"rule"`
	Grant int    `json:"grant"`
	Note  string `json:"note,omitempty"`
}

// DecisionInputs are the numbers a decision was based on.
type DecisionInputs struct {
	Demand            int     `json:"demand"`
	ReactiveDemand    int     `json:"reactiveDemand"`
	ForecastDemand    int     `json:"forecastDemand,omitempty"`
	Guaranteed        int     `json:"guaranteed"`
	MaxReplicas       int     `json:"maxReplicas"`
	Priority          int     `json:"priority"`
	SLOSlack          float64 `json:"sloSlack"`
	WeightedTokenRate float64 `json:"weightedTokenRate"`
}

// DecisionFinal is the outcome for one tenant in one cycle.
type DecisionFinal struct {
	Grant    int     `json:"grant"`
	Shedding bool    `json:"shedding"`
	Weight   float64 `json:"weight"`
}

// DecisionRecord explains one tenant's allocation in one control cycle.
type DecisionRecord struct {
	Cycle         int64          `json:"cycle"`
	Time          time.Time      `json:"time"`
	Tenant        string         `json:"tenant"`
	Inputs        DecisionInputs `json:"inputs"`
	Steps         []DecisionStep `json:"steps"`
	Final         DecisionFinal  `json:"final"`
	Reason        string         `json:"reason"`
	PolicyVersion int64          `json:"policyVersion"`
	// FailStatic marks cycles where metrics were missing and nothing changed.
	FailStatic bool `json:"failStatic,omitempty"`
}

func seconds(s float64) time.Duration { return time.Duration(s * float64(time.Second)) }
