package realrun

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"time"

	"tenantscaler/internal/config"
	"tenantscaler/internal/model"
	"tenantscaler/internal/pool"
	"tenantscaler/internal/stack"
	"tenantscaler/internal/workload"
)

// Profile defaults (ProfilePlan zero values).
const (
	// DefaultLevelDuration is the measured window of each concurrency level.
	DefaultLevelDuration = 60 * time.Second
	// DefaultWarmup precedes each level's window.
	DefaultWarmup = 5 * time.Second
	// DefaultPrefillRequests is the requests per prompt length of the
	// prefill probe.
	DefaultPrefillRequests = 8
	// DefaultShortPrompt is the short prefill probe's prompt length (words).
	DefaultShortPrompt = 16
	// DefaultCheckLevelDuration is the measured window per level of the
	// calibration check (a SIMULATED replica running the calibrated
	// parameters, driven like the measured one).
	DefaultCheckLevelDuration = 20 * time.Second
	// idleTimeout bounds the wait for the replica to finish aborted
	// requests between levels.
	idleTimeout = 15 * time.Second
	// probeOutputTokens is the answer length of the prefill probes.
	probeOutputTokens = 4
)

// profileTenant names the profile's requests (sent straight to the replica).
const profileTenant = "profile"

// ProfilePlan configures Profile; zero fields take the defaults.
type ProfilePlan struct {
	// Config supplies the backend (one replica of it is launched), the
	// slots per replica, the tenant SLOs (the strictest is used) and the
	// controller's configured capacity for comparison.
	Config config.Config
	// In and Out are the request length distributions (the real
	// scenario's, workload.RealLengths).
	In, Out workload.LengthDist
	// MaxConcurrency is the highest concurrency level (default slots + 2).
	MaxConcurrency int
	// LevelDuration is the measured window of each level and Warmup the
	// time before it (defaults 60 s and 5 s).
	LevelDuration, Warmup time.Duration
	// PrefillRequests is the number of requests per prompt length of the
	// prefill probe (default 8); ShortPrompt and LongPrompt are the two
	// prompt lengths in words (defaults 16 and In.Max).
	PrefillRequests         int
	ShortPrompt, LongPrompt int
	// CheckLevelDuration is the measured window per level of the
	// calibration check, which drives a SIMULATED replica running the
	// calibrated parameters through the same levels (default
	// DefaultCheckLevelDuration; negative skips the check).
	CheckLevelDuration time.Duration
	// Seed keys the request lengths and the unique prompts.
	Seed int64
	// LogDir receives the llama-server log ("" = llama.DefaultLogDir).
	LogDir string
	// Logger receives progress; nil means slog.Default().
	Logger *slog.Logger
}

func (p ProfilePlan) withDefaults() ProfilePlan {
	if p.MaxConcurrency <= 0 {
		p.MaxConcurrency = stack.BackendSlots(p.Config) + 2
	}
	if p.LevelDuration <= 0 {
		p.LevelDuration = DefaultLevelDuration
	}
	if p.Warmup < 0 {
		p.Warmup = 0
	} else if p.Warmup == 0 {
		p.Warmup = DefaultWarmup
	}
	if p.PrefillRequests <= 0 {
		p.PrefillRequests = DefaultPrefillRequests
	}
	if p.ShortPrompt <= 0 {
		p.ShortPrompt = DefaultShortPrompt
	}
	if p.LongPrompt <= 0 {
		p.LongPrompt = p.In.Max
	}
	if p.CheckLevelDuration == 0 {
		p.CheckLevelDuration = DefaultCheckLevelDuration
	}
	if p.Logger == nil {
		p.Logger = slog.Default()
	}
	return p
}

// Level is the measurement at one concurrency level (see measureLevel).
// Latencies are in seconds.
type Level struct {
	Concurrency int
	// Requests counts the requests that arrived and completed inside the
	// measured window, the latency percentiles' sample; Completed counts
	// every request that completed inside it, whatever its arrival, the
	// throughput's basis.
	Requests, Completed int
	// Failed counts requests that failed inside the window, FailedBy
	// by cause (loadgen's failure categories).
	Failed                             int
	FailedBy                           map[string]int
	TTFTP50, TTFTP99, TPOTP50, TPOTP99 float64
	// OutputTokensPerSec and WeightedTokensPerSec (prompt words + 2 x
	// output tokens, the gateway's and controller's accounting) are the
	// work of the requests completed inside the window per second of it.
	OutputTokensPerSec, WeightedTokensPerSec float64
	// PromptTokensPerWord is the server tokenizer's prompt tokens per
	// prompt word.
	PromptTokensPerWord float64
	MeetsSLO            bool
}

// PrefillPoint is the prefill probe at one prompt length (concurrency 1).
type PrefillPoint struct {
	Words, Requests int
	// PromptTokens is the server's mean prompt token count.
	PromptTokens float64
	// TTFTP50 is the median time to first token in seconds.
	TTFTP50 float64
}

// ProfileResult is one profile measurement and what it implies.
type ProfileResult struct {
	Engine stack.EngineInfo
	CPU    string
	Date   time.Time
	// Pinning says which CPUs the replicas and the load driver used.
	Pinning string
	// Threads is the CPU threads per replica (llama backend).
	Threads int
	Slots   int
	SLO     model.SLO
	// ColdStart is the replica's launch-to-ready time.
	ColdStart time.Duration
	Prefill   []PrefillPoint
	Levels    []Level
	// CapacityTokensPerSec is C, the weighted tokens/s at the highest
	// concurrency level whose p99 TTFT and p99 TPOT meet the SLO (NaN if
	// none does); CapacityConcurrency is that level (0 if none).
	CapacityTokensPerSec float64
	CapacityConcurrency  int
	// Calibrated are simulated-engine parameters calibrated from the
	// measurement (see derive), LoadFit the load-factor fit they use, and
	// Check how closely a simulated replica running them reproduces the
	// measured levels. Configured is the config's
	// replicaCapacityTokensPerSec.
	Calibrated config.EngineSpec
	LoadFit    LoadFit
	Check      CalibrationCheck
	Configured float64
	Plan       ProfilePlan
}

// Profile launches ONE replica of the configured backend, measures its
// cold start, probes prefill time at two prompt lengths, then drives it
// closed-loop at concurrency 1..MaxConcurrency with unique prompts drawn
// from the plan's length distributions, and derives the capacity C and
// calibrated simulated-engine parameters (see ProfileResult). The replica
// is stopped before the calibration check, which then drives a SIMULATED
// replica running the calibrated parameters through the same levels.
// Real replicas must stay on the CPU (backend.llama.gpuLayers 0).
func Profile(ctx context.Context, plan ProfilePlan) (ProfileResult, error) {
	plan = plan.withDefaults()
	res, err := measure(ctx, plan)
	if err != nil {
		return res, err
	}
	res.derive()
	if err := res.checkCalibration(ctx); err != nil {
		return res, err
	}
	return res, nil
}

// measure runs the profile's measurements on one replica of the plan's
// backend and stops it.
func measure(ctx context.Context, plan ProfilePlan) (ProfileResult, error) {
	cfg := plan.Config
	if err := cfg.Validate(); err != nil {
		return ProfileResult{}, fmt.Errorf("profile: config: %w", err)
	}
	if err := cpuOnly(cfg); err != nil {
		return ProfileResult{}, fmt.Errorf("profile: %w", err)
	}
	pinning, err := pinServices(cfg, plan.Logger)
	if err != nil {
		return ProfileResult{}, fmt.Errorf("profile: %w", err)
	}
	lengthTrace := profileScenario(plan.In, plan.Out)
	if err := lengthTrace.Validate(); err != nil {
		return ProfileResult{}, fmt.Errorf("profile: lengths: %w", err)
	}
	res := ProfileResult{
		CPU: CPUModel(), Date: time.Now(), Slots: stack.BackendSlots(cfg), SLO: strictestSLO(cfg.Tenants),
		Configured: cfg.Controller.ReplicaCapacityTokensPerSec, CapacityTokensPerSec: math.NaN(), Plan: plan,
		Pinning: pinning,
	}
	if cfg.Backend.Kind == config.BackendLlama {
		res.Threads = cfg.Backend.Llama.Threads
	}
	backend, err := stack.NewBackend(cfg, plan.LogDir, plan.Seed, plan.Logger)
	if err != nil {
		return res, fmt.Errorf("profile: %w", err)
	}
	p := pool.New(pool.Config{Name: profileTenant, MaxReplicas: 1, Seed: plan.Seed, Backend: backend, Logger: plan.Logger})
	defer p.Close()
	url, err := startReplica(ctx, p, cfg, &res)
	if err != nil {
		return res, err
	}
	res.Engine = stack.EngineInfo{Backend: backend.Name(), Simulated: backend.Simulated()}
	if !backend.Simulated() {
		if res.Engine, err = stack.ProbeEngine(ctx, url); err != nil {
			plan.Logger.Warn("could not read the engine build from the replica", "err", err)
		}
	}
	// One fresh connection per request: llama-server closes idle
	// keep-alive connections after a few seconds, and a POST sent on a
	// connection it is closing fails ("server closed idle connection")
	// without having reached the engine, which would count as a failure.
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.DisableKeepAlives = true
	d := &driver{url: url, hc: &http.Client{Transport: tr}, seed: plan.Seed, warn: slog.New(warnOnly{plan.Logger.Handler()})}
	defer d.hc.CloseIdleConnections()
	for _, words := range []int{plan.ShortPrompt, plan.LongPrompt} {
		pt, err := d.prefill(ctx, words, plan.PrefillRequests)
		if err != nil {
			return res, err
		}
		res.Prefill = append(res.Prefill, pt)
	}
	lengths := workload.Generate(lengthTrace, plan.Seed)
	for c := 1; c <= plan.MaxConcurrency; c++ {
		lv := d.level(ctx, c, plan.Warmup, plan.LevelDuration, lengths, res.SLO)
		res.Levels = append(res.Levels, lv)
		plan.Logger.Info("profile level done", "concurrency", c, "requests", lv.Requests, "p99TTFT", lv.TTFTP99,
			"p99TPOT", lv.TPOTP99, "weightedTokPerSec", lv.WeightedTokensPerSec, "meetsSLO", lv.MeetsSLO)
		if err := ctx.Err(); err != nil {
			return res, err
		}
		waitIdle(ctx, p)
	}
	return res, nil
}

// startReplica launches the replica, waits until it is ready and records
// its cold start; it returns the replica's URL.
func startReplica(ctx context.Context, p *pool.Pool, cfg config.Config, res *ProfileResult) (string, error) {
	timeout, err := stack.ReadyTimeout(cfg)
	if err != nil {
		return "", err
	}
	if err := p.Scale(1); err != nil {
		return "", fmt.Errorf("profile: launch replica: %w", err)
	}
	if err := stack.WaitReady(ctx, p, 1, timeout); err != nil {
		return "", fmt.Errorf("profile: %w", err)
	}
	st := p.Status()
	if len(st.Replicas) == 0 || len(p.Endpoints()) == 0 {
		return "", errors.New("profile: the replica disappeared after becoming ready")
	}
	res.ColdStart = time.Duration(st.Replicas[0].ColdStartSeconds * float64(time.Second)).Round(time.Millisecond)
	return p.Endpoints()[0], nil
}

// waitIdle waits (bounded) until the replica runs nothing, so requests
// aborted at the end of a level do not load the next one.
func waitIdle(ctx context.Context, p *pool.Pool) {
	deadline := time.Now().Add(idleTimeout)
	for time.Now().Before(deadline) && ctx.Err() == nil {
		busy := 0
		for _, r := range p.Status().Replicas {
			busy += r.Running + r.Waiting
		}
		if busy == 0 {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// profileScenario is a long steady trace whose request lengths the driver
// cycles through: about 2000 draws from the length distributions.
func profileScenario(in, out workload.LengthDist) workload.Scenario {
	return workload.Scenario{Name: "profile-lengths", Duration: 2000 * time.Second, Tenants: []workload.TenantPattern{{
		Tenant: profileTenant, Kind: workload.KindSteady, BaseRPS: 1, PeakRPS: 1, CV: 1,
		Mix: workload.PriorityMix{Standard: 1}, In: in, Out: out,
	}}}
}

// strictestSLO combines the tenants' tightest TTFT and TPOT bounds.
func strictestSLO(tenants []model.TenantSpec) model.SLO {
	s := tenants[0].SLO
	for _, t := range tenants[1:] {
		s.TTFTSeconds = math.Min(s.TTFTSeconds, t.SLO.TTFTSeconds)
		s.TPOTMillis = math.Min(s.TPOTMillis, t.SLO.TPOTMillis)
		s.TargetFraction = math.Max(s.TargetFraction, t.SLO.TargetFraction)
	}
	return s
}
