package stack

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	"tenantscaler/internal/config"
	"tenantscaler/internal/llama"
	"tenantscaler/internal/pool"
)

// simulatedReadyMargin is added to the simulated cold start when waiting
// for simulated replicas to become ready.
const simulatedReadyMargin = 10 * time.Second

// readyPoll is how often WaitReady checks the pool.
const readyPoll = 50 * time.Millisecond

// NewBackend returns the replica backend cfg.Backend.Kind selects: the
// in-process SIMULATED engine with cfg.Engine's parameters and the given
// jitter seed, or REAL llama.cpp llama-server processes whose logs go to
// logDir ("" means llama.DefaultLogDir). For llama it checks that the
// server binary and the model exist before anything is launched.
func NewBackend(cfg config.Config, logDir string, seed int64, logger *slog.Logger) (pool.Backend, error) {
	switch cfg.Backend.Kind {
	case config.BackendSimulated:
		params, err := cfg.Engine.Params()
		if err != nil {
			return nil, err
		}
		if err := params.Validate(); err != nil {
			return nil, fmt.Errorf("engine: %w", err)
		}
		return pool.NewSimulatedBackend(params, seed), nil
	case config.BackendLlama:
		if err := cfg.Backend.Validate(cfg.Pool.CapacityReplicas); err != nil {
			return nil, fmt.Errorf("config: %w", err)
		}
		l := &llama.Launcher{Spec: cfg.Backend.Llama, LogDir: logDir, Logger: logger}
		if err := l.Check(); err != nil {
			return nil, err
		}
		return llama.NewBackend(l), nil
	default:
		return nil, fmt.Errorf("backend.kind must be %q or %q, got %q", config.BackendSimulated, config.BackendLlama, cfg.Backend.Kind)
	}
}

// ReadyTimeout is the longest Start waits for its initial replicas:
// backend.llama.readyTimeoutSeconds for real replicas, the simulated cold
// start plus 10 s for simulated ones.
func ReadyTimeout(cfg config.Config) (time.Duration, error) {
	if cfg.Backend.Kind == config.BackendLlama {
		return time.Duration(cfg.Backend.Llama.ReadyTimeoutSeconds * float64(time.Second)), nil
	}
	params, err := cfg.Engine.Params()
	if err != nil {
		return 0, err
	}
	return params.StartupDuration + simulatedReadyMargin, nil
}

// BackendSlots is the number of requests one replica runs concurrently:
// llama.cpp's parallel slots (-np) or the simulated engine's maxNumSeqs.
func BackendSlots(cfg config.Config) int {
	if cfg.Backend.Kind == config.BackendLlama {
		return cfg.Backend.Llama.Parallel
	}
	return cfg.Engine.MaxNumSeqs
}

// WaitReady blocks until the pool has at least n ready replicas, ctx ends
// or timeout elapses.
func WaitReady(ctx context.Context, p *pool.Pool, n int, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	tick := time.NewTicker(readyPoll)
	defer tick.Stop()
	for {
		st := p.Status()
		if st.Ready >= n {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("only %d of %d replicas ready after %s (starting %d)", st.Ready, n, timeout, st.Starting)
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("waiting for %d ready replicas: %w", n, ctx.Err())
		case <-tick.C:
		}
	}
}

// EngineInfo identifies what serves the requests, for result labels.
type EngineInfo struct {
	// Backend is "simulated" or "llama"; Simulated is true for the
	// simulated engine.
	Backend   string `json:"backend"`
	Simulated bool   `json:"simulated"`
	// Build is llama-server's build_info and ModelPath its model_path, as
	// reported by a running replica's GET /props (llama only; empty when
	// the replica did not report them). Slots is its total_slots.
	Build     string `json:"build,omitempty"`
	ModelPath string `json:"modelPath,omitempty"`
	Slots     int    `json:"slots,omitempty"`
}

// Model returns the model file name, or "" when unknown.
func (e EngineInfo) Model() string {
	if e.ModelPath == "" {
		return ""
	}
	return filepath.Base(strings.ReplaceAll(e.ModelPath, `\`, "/"))
}

// Label describes the engine in one line: "llama.cpp <build>, model <file>"
// for real replicas, "SIMULATED engine" otherwise.
func (e EngineInfo) Label() string {
	if e.Simulated {
		return "SIMULATED engine (internal/replica, modelled latencies)"
	}
	build, model := e.Build, e.Model()
	if build == "" {
		build = "unknown build"
	}
	if model == "" {
		model = "unknown model"
	}
	return fmt.Sprintf("llama.cpp %s, model %s", build, model)
}

// propsReply is the part of llama-server's GET /props answer used here.
type propsReply struct {
	BuildInfo  string `json:"build_info"`
	ModelPath  string `json:"model_path"`
	TotalSlots int    `json:"total_slots"`
}

// ProbeEngine reads build, model and slot count from a running llama-server
// replica's GET /props.
func ProbeEngine(ctx context.Context, replicaURL string) (EngineInfo, error) {
	info := EngineInfo{Backend: config.BackendLlama}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	url := strings.TrimRight(replicaURL, "/") + "/props"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return info, fmt.Errorf("probe engine: %w", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return info, fmt.Errorf("probe engine: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return info, fmt.Errorf("probe engine: GET %s: %s", url, resp.Status)
	}
	var p propsReply
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&p); err != nil {
		return info, fmt.Errorf("probe engine: decode GET %s: %w", url, err)
	}
	info.Build, info.ModelPath, info.Slots = p.BuildInfo, p.ModelPath, p.TotalSlots
	return info, nil
}

// engineInfo describes the backend of a started pool: for real replicas it
// probes the first ready one (a failed probe is logged and leaves the build
// and model empty).
func engineInfo(ctx context.Context, b pool.Backend, endpoints []string, logger *slog.Logger) EngineInfo {
	if b.Simulated() || len(endpoints) == 0 {
		return EngineInfo{Backend: b.Name(), Simulated: b.Simulated()}
	}
	info, err := ProbeEngine(ctx, endpoints[0])
	if err != nil {
		logger.Warn("could not read the engine build from the replica", "err", err)
	}
	info.Backend = b.Name()
	return info
}

// tokenCounter is the gateway's token counter: the replicas' own tokenizer
// for real replicas (decision Q17), nil (whitespace words, the simulated
// engine's tokenizer) for simulated ones.
func tokenCounter(cfg config.Config) func(context.Context, string, string) (int, error) {
	if cfg.Backend.Kind == config.BackendLlama {
		return llama.TokenCounter()
	}
	return nil
}
