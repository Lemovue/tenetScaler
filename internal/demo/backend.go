package demo

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"tenantscaler/internal/config"
	"tenantscaler/internal/llama"
	"tenantscaler/internal/realrun"
)

// Backend modes accepted by Resolve (the demo's -backend flag).
const (
	// ModeAuto picks llama when the llama-server binary and the model are
	// found, the simulated engine otherwise.
	ModeAuto = "auto"
	// ModeLlama requires REAL llama.cpp replicas.
	ModeLlama = config.BackendLlama
	// ModeSimulated uses the SIMULATED engine.
	ModeSimulated = config.BackendSimulated
)

// DefaultRealConfig is the configuration of the llama backend when no
// config file is given. The simulated backend uses config.Default() then.
const DefaultRealConfig = "config/real.yaml"

// builtinConfig names the built-in default configuration in labels.
const builtinConfig = "built-in default"

// BackendInfo describes the replica backend for the dashboard.
type BackendInfo struct {
	// Kind is config.BackendLlama or config.BackendSimulated; Simulated is
	// true for the simulated engine.
	Kind      string `json:"kind"`
	Simulated bool   `json:"simulated"`
	// Label is the header badge text: "REAL: llama.cpp + <model> on N
	// CPU-pinned replicas" or "SIMULATED engine".
	Label string `json:"label"`
	// Detail says in one sentence what serves the requests.
	Detail string `json:"detail"`
	// Reason says why this backend was chosen.
	Reason string `json:"reason,omitempty"`
	// Replicas is the replica budget (the pool capacity) of each stack.
	Replicas int `json:"replicas"`
	// Model is the model file name, ThreadsPerReplica and SlotsPerReplica
	// the llama-server settings and CPU the machine's processor (real
	// replicas only).
	Model             string `json:"model,omitempty"`
	ThreadsPerReplica int    `json:"threadsPerReplica,omitempty"`
	SlotsPerReplica   int    `json:"slotsPerReplica,omitempty"`
	CPU               string `json:"cpu,omitempty"`
	// ConfigPath is the configuration file, or "built-in default".
	ConfigPath string `json:"configPath"`
}

// Resolve chooses the replica backend for mode (ModeAuto, ModeLlama or
// ModeSimulated) and loads the configuration: configPath, or when it is
// empty DefaultRealConfig for llama and config.Default() for the simulated
// engine. ModeAuto picks llama when the llama-server binary and the model
// exist (backend.llama.server/model of the config, else $TS_LLAMA_SERVER
// and $TS_LLAMA_MODEL). The returned config's backend kind is the chosen
// one, whatever the file said.
func Resolve(mode, configPath string) (config.Config, BackendInfo, error) {
	return resolve(mode, configPath, checkLlama, realrun.CPUModel)
}

// checkLlama reports whether real replicas can be launched with spec.
func checkLlama(spec config.LlamaSpec) error { return (&llama.Launcher{Spec: spec}).Check() }

// resolve is Resolve with the llama check and the CPU probe injected.
func resolve(mode, path string, check func(config.LlamaSpec) error, cpu func() string) (config.Config, BackendInfo, error) {
	var kind, reason string
	switch mode {
	case ModeAuto:
		spec := config.Default().Backend.Llama
		if path != "" {
			cfg, err := config.Load(path)
			if err != nil {
				return config.Config{}, BackendInfo{}, fmt.Errorf("demo: %w", err)
			}
			spec = cfg.Backend.Llama
		}
		if err := check(spec); err != nil {
			kind, reason = ModeSimulated, "llama.cpp not available ("+err.Error()+"), so the simulated engine runs"
		} else {
			kind, reason = ModeLlama, "llama.cpp server and model found"
		}
	case ModeLlama, ModeSimulated:
		kind, reason = mode, "selected with -backend "+mode
	default:
		return config.Config{}, BackendInfo{}, fmt.Errorf("demo: backend must be %q, %q or %q, got %q", ModeAuto, ModeLlama, ModeSimulated, mode)
	}
	if path == "" {
		// The real configuration also suits the simulated engine: its engine
		// section is calibrated to the real replica and the demo trace is
		// sized for its budget. Without it the built-in default is used.
		if _, err := os.Stat(DefaultRealConfig); err == nil || kind == ModeLlama {
			path = DefaultRealConfig
		}
	}
	cfg := config.Default()
	if path != "" {
		var err error
		if cfg, err = config.Load(path); err != nil {
			return config.Config{}, BackendInfo{}, fmt.Errorf("demo: %w", err)
		}
	}
	cfg.Backend.Kind = kind
	if err := cfg.Validate(); err != nil {
		return config.Config{}, BackendInfo{}, fmt.Errorf("demo: config: %w", err)
	}
	cpuName := ""
	if kind == ModeLlama {
		if err := check(cfg.Backend.Llama); err != nil {
			return config.Config{}, BackendInfo{}, fmt.Errorf("demo: real replicas unavailable: %w", err)
		}
		cpuName = cpu()
	}
	info := DescribeBackend(cfg, cpuName)
	info.Reason = reason
	if path != "" {
		info.ConfigPath = filepath.ToSlash(path)
	}
	return cfg, info, nil
}

// DescribeBackend labels cfg's backend. cpu is the machine's CPU model
// for real replicas ("" when unknown).
func DescribeBackend(cfg config.Config, cpu string) BackendInfo {
	info := BackendInfo{Kind: cfg.Backend.Kind, Replicas: cfg.Pool.CapacityReplicas, ConfigPath: builtinConfig}
	if cfg.Backend.Kind != config.BackendLlama {
		info.Kind, info.Simulated = config.BackendSimulated, true
		info.Label = "SIMULATED engine"
		info.Detail = fmt.Sprintf("SIMULATED: %d in-process replicas with modelled latencies (internal/replica), not real inference; "+
			"set TS_LLAMA_SERVER and TS_LLAMA_MODEL for real llama.cpp replicas.", cfg.Pool.CapacityReplicas)
		return info
	}
	l := cfg.Backend.Llama
	info.Model = modelName(l.Model)
	info.ThreadsPerReplica, info.SlotsPerReplica, info.CPU = l.Threads, l.Parallel, cpu
	info.Label = fmt.Sprintf("REAL: llama.cpp + %s on %d CPU-pinned replicas", info.Model, cfg.Pool.CapacityReplicas)
	info.Detail = fmt.Sprintf("REAL inference: each replica is a llama-server process pinned to its own CPU cores "+
		"(%d threads, %d slots, no GPU)", l.Threads, l.Parallel)
	if cpu != "" {
		info.Detail += "; CPU " + cpu
	}
	info.Detail += "."
	return info
}

// modelName is the model file name without its .gguf extension: the
// configured path, else $TS_LLAMA_MODEL; "unknown model" when neither is
// set.
func modelName(configured string) string {
	path := configured
	if path == "" {
		path = os.Getenv(llama.EnvModel)
	}
	if path == "" {
		return "unknown model"
	}
	base := filepath.Base(filepath.FromSlash(strings.ReplaceAll(path, `\`, "/")))
	return strings.TrimSuffix(base, ".gguf")
}
