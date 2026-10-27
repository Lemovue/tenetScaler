// Package config loads and validates the TenantScaler configuration used in
// local mode (a YAML file). In Kubernetes mode the same TenantSpec values
// come from TenantScaler custom resources instead.
package config

import (
	"fmt"
	"os"
	"regexp"
	"time"

	"sigs.k8s.io/yaml"

	"tenantscaler/internal/engine"
	"tenantscaler/internal/model"
)

// Config is the whole local-mode configuration.
type Config struct {
	Pool       PoolSpec           `json:"pool"`
	Tenants    []model.TenantSpec `json:"tenants"`
	Controller ControllerSpec     `json:"controller"`
	Gateway    GatewaySpec        `json:"gateway"`
	Engine     EngineSpec         `json:"engine"`
	Backend    BackendSpec        `json:"backend"`
}

// Replica backends.
const (
	// BackendSimulated runs simulated engines in-process (package replica).
	BackendSimulated = "simulated"
	// BackendLlama runs real llama.cpp llama-server processes.
	BackendLlama = "llama"
)

// BackendSpec selects what the pool runs.
type BackendSpec struct {
	// Kind is BackendSimulated (default) or BackendLlama.
	Kind  string    `json:"kind"`
	Llama LlamaSpec `json:"llama"`
}

// LlamaSpec configures real llama-server replicas. Empty Server and Model
// fall back to $TS_LLAMA_SERVER and $TS_LLAMA_MODEL at launch time.
type LlamaSpec struct {
	Server string `json:"server"`
	Model  string `json:"model"`
	// Threads is the CPU threads per replica (-t).
	Threads int `json:"threads"`
	// Parallel is the slots per replica (-np); it is also the gateway's
	// dispatch window unless gateway.slotsPerReplica overrides it.
	Parallel int `json:"parallel"`
	// CtxPerSlot is the context length per slot; -c = Parallel*CtxPerSlot.
	CtxPerSlot int `json:"ctxPerSlot"`
	// CPUMasks are hex affinity masks (-C), one per replica slot, so every
	// replica owns its own cores. Empty = one physical core per replica
	// assuming SMT siblings are adjacent (3, c, 30, c0, ...).
	CPUMasks []string `json:"cpuMasks"`
	// GPULayers is -ngl; 0 keeps inference on the CPU (see PLAN.md M1).
	GPULayers int `json:"gpuLayers"`
	// CacheRAMMiB caps llama-server's host-memory prompt cache (--cache-ram);
	// 0 disables it (the default: benchmark prompts are unique anyway).
	CacheRAMMiB         int      `json:"cacheRamMiB"`
	ReadyTimeoutSeconds float64  `json:"readyTimeoutSeconds"`
	ExtraArgs           []string `json:"extraArgs"`
}

// PoolSpec is the shared replica pool and its fixed budget.
type PoolSpec struct {
	Name string `json:"name"`
	// CapacityReplicas is the GPU budget expressed in replicas.
	CapacityReplicas int `json:"capacityReplicas"`
	// MinReplicas keeps the pool from scaling to zero.
	MinReplicas int `json:"minReplicas"`
}

// ControllerSpec tunes the slow loop.
type ControllerSpec struct {
	CycleSeconds float64 `json:"cycleSeconds"`
	Headroom     float64 `json:"headroom"`
	// ReplicaCapacityTokensPerSec (C) is the weighted tokens/s one replica
	// serves while meeting the SLO; measure it with `tenantscaler profile`.
	ReplicaCapacityTokensPerSec float64 `json:"replicaCapacityTokensPerSec"`
	ScaleDownWindowSeconds      float64 `json:"scaleDownWindowSeconds"`
	MaxStepPerCycle             int     `json:"maxStepPerCycle"`
	// RetryAfterSeconds is the base Retry-After sent with 429s.
	RetryAfterSeconds int `json:"retryAfterSeconds"`
	// Ablation switches (default false = full TenantScaler).
	DisableBorrowing       bool `json:"disableBorrowing"`
	DisableGatewayCoupling bool `json:"disableGatewayCoupling"`
	// DisableSlotReservation sends no slot shares to the gateway, so an
	// over-share tenant may take any free slot (ablation).
	DisableSlotReservation bool `json:"disableSlotReservation"`
	// NaiveDemand estimates demand from admitted (not shed) weighted
	// tokens instead of offered ones: the death-spiral ablation S8
	// (BRIEF-v2 Q11). It needs the gateway metrics source.
	NaiveDemand bool         `json:"naiveDemand"`
	Forecast    ForecastSpec `json:"forecast"`
}

// ForecastSpec configures the optional Holt-Winters demand forecaster.
type ForecastSpec struct {
	Enabled       bool    `json:"enabled"`
	BucketSeconds float64 `json:"bucketSeconds"`
	SeasonBuckets int     `json:"seasonBuckets"`
	Alpha         float64 `json:"alpha"`
	Beta          float64 `json:"beta"`
	Gamma         float64 `json:"gamma"`
}

// GatewaySpec tunes the fast loop.
type GatewaySpec struct {
	// SlotsPerReplica is the dispatch window: max in-flight requests the
	// gateway sends to one replica. Zero means engine.maxNumSeqs.
	SlotsPerReplica int `json:"slotsPerReplica"`
}

// EngineSpec is the YAML form of engine.Params (durations as strings).
type EngineSpec struct {
	PrefillOverhead     string  `json:"prefillOverhead"`
	PrefillPerToken     string  `json:"prefillPerToken"`
	InterTokenLatency   string  `json:"interTokenLatency"`
	MaxNumSeqs          int     `json:"maxNumSeqs"`
	TimeFactorUnderLoad float64 `json:"timeFactorUnderLoad"`
	// PrefillFactorUnderLoad: 0 = timeFactorUnderLoad (engine.Params).
	PrefillFactorUnderLoad float64 `json:"prefillFactorUnderLoad,omitempty"`
	StartupDuration        string  `json:"startupDuration"`
	JitterFraction         float64 `json:"jitterFraction"`
}

// Default returns a complete working configuration: three tenants sharing a
// 10-replica pool with guarantees 3/3/3 (the worked example).
func Default() Config {
	slo := model.SLO{TTFTSeconds: 2, TPOTMillis: 100, TargetFraction: 0.99}
	return Config{
		Pool: PoolSpec{Name: "shared-llm-pool", CapacityReplicas: 10, MinReplicas: 1},
		Tenants: []model.TenantSpec{
			{Name: "A", GuaranteedReplicas: 3, MaxReplicas: 8, Priority: 100, SLO: slo},
			{Name: "B", GuaranteedReplicas: 3, MaxReplicas: 8, Priority: 100, SLO: slo},
			{Name: "C", GuaranteedReplicas: 3, MaxReplicas: 8, Priority: 100, SLO: slo},
		},
		Controller: ControllerSpec{
			CycleSeconds:                15,
			Headroom:                    0.2,
			ReplicaCapacityTokensPerSec: 400,
			ScaleDownWindowSeconds:      120,
			MaxStepPerCycle:             4,
			RetryAfterSeconds:           2,
			Forecast: ForecastSpec{
				BucketSeconds: 15, SeasonBuckets: 8, Alpha: 0.5, Beta: 0.1, Gamma: 0.3,
			},
		},
		Engine: EngineFromParams(engine.DefaultParams()),
		Backend: BackendSpec{
			Kind: BackendSimulated,
			Llama: LlamaSpec{
				Threads: 2, Parallel: 4, CtxPerSlot: 1024, ReadyTimeoutSeconds: 120,
			},
		},
	}
}

var hexMaskRE = regexp.MustCompile(`^(0x)?[0-9A-Fa-f]+$`)

// Validate checks the backend selection. File paths are checked when a
// replica is launched, so a simulated config validates on any machine.
func (b BackendSpec) Validate(capacity int) error {
	switch b.Kind {
	case BackendSimulated:
		return nil
	case BackendLlama:
	default:
		return fmt.Errorf("backend.kind must be %q or %q, got %q", BackendSimulated, BackendLlama, b.Kind)
	}
	l := b.Llama
	switch {
	case l.Threads < 1:
		return fmt.Errorf("backend.llama.threads must be >= 1")
	case l.Parallel < 1:
		return fmt.Errorf("backend.llama.parallel must be >= 1")
	case l.CtxPerSlot < 128:
		return fmt.Errorf("backend.llama.ctxPerSlot must be >= 128")
	case l.GPULayers < 0:
		return fmt.Errorf("backend.llama.gpuLayers must be >= 0")
	case l.CacheRAMMiB < 0 || l.CacheRAMMiB > 1024:
		return fmt.Errorf("backend.llama.cacheRamMiB must be in [0, 1024] (machine-safety cap)")
	case l.ReadyTimeoutSeconds <= 0:
		return fmt.Errorf("backend.llama.readyTimeoutSeconds must be > 0")
	case len(l.CPUMasks) > 0 && len(l.CPUMasks) < capacity:
		return fmt.Errorf("backend.llama.cpuMasks has %d masks but the pool capacity is %d", len(l.CPUMasks), capacity)
	}
	for _, m := range l.CPUMasks {
		if !hexMaskRE.MatchString(m) {
			return fmt.Errorf("backend.llama.cpuMasks: %q is not a hex mask", m)
		}
	}
	return nil
}

// Load reads a YAML file on top of Default() and validates the result.
func Load(path string) (Config, error) {
	cfg := Default()
	data, err := os.ReadFile(path)
	if err != nil {
		return Config{}, fmt.Errorf("read config: %w", err)
	}
	if err := yaml.UnmarshalStrict(data, &cfg); err != nil {
		return Config{}, fmt.Errorf("parse config %s: %w", path, err)
	}
	if err := cfg.Validate(); err != nil {
		return Config{}, fmt.Errorf("invalid config %s: %w", path, err)
	}
	return cfg, nil
}

var nameRE = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9._-]{0,61}[A-Za-z0-9])?$`)

// ValidateTenants checks tenant specs against a pool capacity. It rejects a
// configuration whose guarantees add up to more than the pool can hold:
// you cannot promise more than you have.
func ValidateTenants(tenants []model.TenantSpec, capacity int) error {
	if len(tenants) == 0 {
		return fmt.Errorf("at least one tenant is required")
	}
	seen := map[string]bool{}
	sum := 0
	for _, t := range tenants {
		if !nameRE.MatchString(t.Name) {
			return fmt.Errorf("tenant name %q is not a valid identifier", t.Name)
		}
		if seen[t.Name] {
			return fmt.Errorf("duplicate tenant %q", t.Name)
		}
		seen[t.Name] = true
		if t.GuaranteedReplicas < 0 {
			return fmt.Errorf("tenant %q: guaranteedReplicas must be >= 0", t.Name)
		}
		if t.MaxReplicas < 1 || t.MaxReplicas < t.GuaranteedReplicas {
			return fmt.Errorf("tenant %q: maxReplicas must be >= 1 and >= guaranteedReplicas", t.Name)
		}
		if t.SLO.TTFTSeconds <= 0 || t.SLO.TPOTMillis <= 0 {
			return fmt.Errorf("tenant %q: slo.ttftSeconds and slo.tpotMillis must be > 0", t.Name)
		}
		if t.SLO.TargetFraction <= 0 || t.SLO.TargetFraction > 1 {
			return fmt.Errorf("tenant %q: slo.targetFraction must be in (0, 1]", t.Name)
		}
		if t.GuaranteedReplicas > capacity-sum {
			// Checked before adding, so a huge guarantee cannot overflow sum.
			return fmt.Errorf("guaranteed replicas add up to more than pool capacity %d: refusing to promise more than exists", capacity)
		}
		sum += t.GuaranteedReplicas
	}
	return nil
}

// Validate checks the whole configuration.
func (c Config) Validate() error {
	if c.Pool.CapacityReplicas < 1 {
		return fmt.Errorf("pool.capacityReplicas must be >= 1")
	}
	if c.Pool.MinReplicas < 0 || c.Pool.MinReplicas > c.Pool.CapacityReplicas {
		return fmt.Errorf("pool.minReplicas must be in [0, capacityReplicas]")
	}
	if err := ValidateTenants(c.Tenants, c.Pool.CapacityReplicas); err != nil {
		return err
	}
	ctl := c.Controller
	switch {
	case ctl.CycleSeconds <= 0:
		return fmt.Errorf("controller.cycleSeconds must be > 0")
	case ctl.Headroom < 0:
		return fmt.Errorf("controller.headroom must be >= 0")
	case ctl.ReplicaCapacityTokensPerSec <= 0:
		return fmt.Errorf("controller.replicaCapacityTokensPerSec must be > 0")
	case ctl.ScaleDownWindowSeconds < 0:
		return fmt.Errorf("controller.scaleDownWindowSeconds must be >= 0")
	case ctl.MaxStepPerCycle < 1:
		return fmt.Errorf("controller.maxStepPerCycle must be >= 1")
	case ctl.RetryAfterSeconds < 0:
		return fmt.Errorf("controller.retryAfterSeconds must be >= 0")
	}
	if f := ctl.Forecast; f.Enabled {
		if f.BucketSeconds <= 0 || f.SeasonBuckets < 2 {
			return fmt.Errorf("controller.forecast: bucketSeconds must be > 0 and seasonBuckets >= 2")
		}
		for _, v := range []float64{f.Alpha, f.Beta, f.Gamma} {
			if v <= 0 || v >= 1 {
				return fmt.Errorf("controller.forecast: alpha, beta and gamma must be in (0, 1)")
			}
		}
	}
	if c.Gateway.SlotsPerReplica < 0 {
		return fmt.Errorf("gateway.slotsPerReplica must be >= 0")
	}
	if err := c.Backend.Validate(c.Pool.CapacityReplicas); err != nil {
		return err
	}
	p, err := c.Engine.Params()
	if err != nil {
		return err
	}
	return p.Validate()
}

// SlotsPerReplica returns the effective dispatch window per replica: the
// explicit gateway setting, else the backend's concurrent sequences.
func (c Config) SlotsPerReplica() int {
	if c.Gateway.SlotsPerReplica > 0 {
		return c.Gateway.SlotsPerReplica
	}
	if c.Backend.Kind == BackendLlama {
		return c.Backend.Llama.Parallel
	}
	return c.Engine.MaxNumSeqs
}

// TenantNames lists tenant names in configuration order.
func (c Config) TenantNames() []string {
	names := make([]string, len(c.Tenants))
	for i, t := range c.Tenants {
		names[i] = t.Name
	}
	return names
}

// Params converts the YAML engine spec to engine.Params.
func (e EngineSpec) Params() (engine.Params, error) {
	var p engine.Params
	durations := []struct {
		field string
		src   string
		dst   *time.Duration
	}{
		{"prefillOverhead", e.PrefillOverhead, &p.PrefillOverhead},
		{"prefillPerToken", e.PrefillPerToken, &p.PrefillPerToken},
		{"interTokenLatency", e.InterTokenLatency, &p.InterTokenLatency},
		{"startupDuration", e.StartupDuration, &p.StartupDuration},
	}
	for _, d := range durations {
		v, err := time.ParseDuration(d.src)
		if err != nil {
			return engine.Params{}, fmt.Errorf("engine.%s: %w", d.field, err)
		}
		*d.dst = v
	}
	p.MaxNumSeqs = e.MaxNumSeqs
	p.TimeFactorUnderLoad = e.TimeFactorUnderLoad
	p.PrefillFactorUnderLoad = e.PrefillFactorUnderLoad
	p.JitterFraction = e.JitterFraction
	return p, nil
}

// EngineFromParams converts engine.Params to its YAML form.
func EngineFromParams(p engine.Params) EngineSpec {
	return EngineSpec{
		PrefillOverhead:        p.PrefillOverhead.String(),
		PrefillPerToken:        p.PrefillPerToken.String(),
		InterTokenLatency:      p.InterTokenLatency.String(),
		MaxNumSeqs:             p.MaxNumSeqs,
		TimeFactorUnderLoad:    p.TimeFactorUnderLoad,
		PrefillFactorUnderLoad: p.PrefillFactorUnderLoad,
		StartupDuration:        p.StartupDuration.String(),
		JitterFraction:         p.JitterFraction,
	}
}
