package stack

import (
	"context"
	"log/slog"
	"math"
	"strings"
	"testing"
	"time"

	"tenantscaler/internal/config"
	"tenantscaler/internal/engine"
	"tenantscaler/internal/fairq"
	"tenantscaler/internal/gateway"
	"tenantscaler/internal/loadgen"
	"tenantscaler/internal/model"
	"tenantscaler/internal/policy"
	"tenantscaler/internal/sim"
	"tenantscaler/internal/workload"
)

// fastConfig is a SIMULATED 3-replica config whose replicas start in 50 ms
// and answer in milliseconds, with a 200 ms control cycle and a tiny
// per-replica capacity, so any traffic makes the controller scale up.
func fastConfig() config.Config {
	cfg := config.Default()
	cfg.Pool.CapacityReplicas, cfg.Pool.MinReplicas = 3, 1
	slo := model.SLO{TTFTSeconds: 2, TPOTMillis: 100, TargetFraction: 0.99}
	cfg.Tenants = []model.TenantSpec{
		{Name: "A", GuaranteedReplicas: 1, MaxReplicas: 3, Priority: 100, SLO: slo},
		{Name: "B", GuaranteedReplicas: 1, MaxReplicas: 3, Priority: 100, SLO: slo},
		{Name: "C", GuaranteedReplicas: 1, MaxReplicas: 3, Priority: 100, SLO: slo},
	}
	cfg.Controller.CycleSeconds = 0.2
	cfg.Controller.ScaleDownWindowSeconds = 1
	cfg.Controller.ReplicaCapacityTokensPerSec = 1
	cfg.Engine = config.EngineFromParams(engine.Params{
		PrefillOverhead: 2 * time.Millisecond, PrefillPerToken: 50 * time.Microsecond,
		InterTokenLatency: 2 * time.Millisecond, MaxNumSeqs: 2, TimeFactorUnderLoad: 1.5,
		StartupDuration: 50 * time.Millisecond, JitterFraction: 0.05,
	})
	return cfg
}

func quiet() *slog.Logger { return slog.New(slog.DiscardHandler) }

// traffic is a few short requests of every tenant.
func traffic() []workload.Spec {
	var specs []workload.Spec
	for i := range 9 {
		specs = append(specs, workload.Spec{
			Offset: time.Duration(i) * 20 * time.Millisecond, Tenant: string(rune('A' + i%3)),
			Priority: model.PriorityStandard, InputTokens: 12, OutputTokens: 6,
		})
	}
	return specs
}

// wantGateway is every supported setup's gateway mode (DESIGN.md 16).
var wantGateway = map[string]string{
	sim.SetupKEDAQueue:       gateway.ModePassthrough,
	sim.SetupStaticPartition: gateway.ModePartition,
	sim.SetupFlowControl:     gateway.ModeRoundRobin,
	sim.SetupNoGateway:       gateway.ModePassthrough,
	sim.SetupNoBorrowing:     gateway.ModeFair,
	sim.SetupFull:            gateway.ModeFair,
}

func TestStackEverySetup(t *testing.T) {
	if len(wantGateway) != len(Supported) {
		t.Fatalf("wantGateway covers %d setups, Supported has %d", len(wantGateway), len(Supported))
	}
	for _, setup := range Supported {
		t.Run(setup, func(t *testing.T) {
			ctx := context.Background()
			st, err := Start(ctx, Options{Setup: setup, Config: fastConfig(), InitialReplicas: 1, Seed: 3, Logger: quiet()})
			if err != nil {
				t.Fatal(err)
			}
			defer st.Stop(ctx)
			want := 1
			if setup == sim.SetupStaticPartition {
				want = 3
			}
			if ps := st.PoolStatus(); ps.Ready != want || !ps.Simulated {
				t.Fatalf("pool %+v, want %d ready simulated replicas", ps, want)
			}
			if st.GatewayMode() != wantGateway[setup] || st.InitialReplicas() != want {
				t.Errorf("gateway %q warm pool %d, want %q and %d", st.GatewayMode(), st.InitialReplicas(), wantGateway[setup], want)
			}
			cycle := time.Duration(fastConfig().Controller.CycleSeconds * float64(time.Second))
			if phase, ok := st.ControlPhase(); ok != (setup != sim.SetupStaticPartition) || phase < 0 || phase >= cycle {
				t.Errorf("control phase %v (control plane %v), want one in [0, %v) unless static", phase, ok, cycle)
			}
			if !st.Simulated() || !st.Engine().Simulated || !strings.HasPrefix(st.URL(), "http://127.0.0.1:") {
				t.Fatalf("engine %+v url %q", st.Engine(), st.URL())
			}
			start := time.Now()
			res, err := loadgen.ReplayResult(ctx, traffic(), []loadgen.Target{{Name: "s", URL: st.URL()}},
				loadgen.Options{Seed: 1, Logger: quiet()})
			if err != nil {
				t.Fatal(err)
			}
			for _, r := range res.Records["s"] {
				if !r.Completed() {
					t.Fatalf("request %+v did not complete", r)
				}
			}
			if !res.Simulated("s") {
				t.Error("responses not marked simulated")
			}
			smp := st.Sample(start)
			if smp.T <= 0 || smp.Ready < want || smp.Desired < want || len(smp.Queued) != 3 || len(smp.InFlight) != 3 {
				t.Errorf("sample %+v", smp)
			}
			if gw, _, _ := Describe(setup); gw != gateway.ModeFair && !math.IsNaN(smp.CounterGap) {
				t.Errorf("counter gap %v outside fair mode, want NaN", smp.CounterGap)
			}
			if HasController(setup) != (st.Decisions(0) != nil) {
				t.Errorf("decisions %v for setup %s", st.Decisions(0), setup)
			}
			if err := st.Stop(ctx); err != nil {
				t.Fatalf("stop: %v", err)
			}
			if err := st.Stop(ctx); err != nil {
				t.Fatalf("second stop: %v", err)
			}
			if n := len(st.PoolStatus().Replicas); n != 0 {
				t.Errorf("%d replicas left after Stop", n)
			}
			colds := st.ColdStarts()
			if len(colds) < want {
				t.Errorf("cold starts %v, want one per warm replica (%d)", colds, want)
			}
			for id, c := range colds {
				if c != 0.05 {
					t.Errorf("replica %s cold start %v, want the configured 0.05 s", id, c)
				}
			}
		})
	}
}

// TestStackControllerActs checks the S7 control loop end to end: traffic
// raises demand above the tiny capacity, the controller scales the pool up
// (a protective action) and publishes policies the gateway applies.
func TestStackControllerActs(t *testing.T) {
	ctx := context.Background()
	st, err := Start(ctx, Options{Setup: sim.SetupFull, Config: fastConfig(), InitialReplicas: 1, Seed: 5, Logger: quiet()})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Stop(ctx)
	start := time.Now()
	stopTraffic := make(chan struct{})
	go func() {
		for {
			select {
			case <-stopTraffic:
				return
			default:
			}
			_, _ = loadgen.Replay(ctx, traffic(), []loadgen.Target{{Name: "s", URL: st.URL()}}, loadgen.Options{Logger: quiet()})
		}
	}()
	defer close(stopTraffic)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && (len(st.ActionTimes(start)) == 0 || st.PoolStatus().Ready < 3) {
		time.Sleep(50 * time.Millisecond)
	}
	if len(st.ActionTimes(start)) == 0 || st.PoolStatus().Desired != 3 {
		t.Fatalf("no scale-up: actions %v, pool %+v", st.ActionTimes(start), st.PoolStatus())
	}
	if len(st.PolicyLatencies()) == 0 {
		t.Error("no applied policy recorded")
	}
	if len(st.Decisions(3)) == 0 {
		t.Error("no decision records")
	}
	if got := st.ActionTimes(time.Now().Add(time.Hour)); len(got) != 0 {
		t.Errorf("actions after a future start: %v", got)
	}
}

func TestStartErrors(t *testing.T) {
	ctx := context.Background()
	if _, err := Start(ctx, Options{Setup: sim.SetupCPUHPA, Config: fastConfig()}); err == nil || !strings.Contains(err.Error(), "unsupported setup") {
		t.Errorf("S1: %v, want unsupported setup", err)
	}
	cfg := fastConfig()
	cfg.Backend.Kind = config.BackendLlama
	cfg.Backend.Llama.Server = t.TempDir() + "/missing-llama-server.exe"
	cfg.Backend.Llama.Model = t.TempDir() + "/missing.gguf"
	if _, err := Start(ctx, Options{Setup: sim.SetupFull, Config: cfg, Logger: quiet()}); err == nil || !strings.Contains(err.Error(), "does not exist") {
		t.Errorf("missing llama-server: %v, want a clear error", err)
	}
	bad := fastConfig()
	bad.Pool.CapacityReplicas = 0
	if _, err := Start(ctx, Options{Setup: sim.SetupFull, Config: bad}); err == nil {
		t.Error("invalid config accepted")
	}
}

func TestRecorderShedActions(t *testing.T) {
	r := newRecorder()
	at := time.Unix(100, 0)
	pol := func(shed ...string) policy.Policy {
		p := policy.Policy{Version: 1, Tenants: map[string]policy.TenantPolicy{"A": {Weight: 1}, "C": {Weight: 1}}}
		for _, t := range shed {
			p.Tenants[t] = policy.TenantPolicy{Weight: 1, Shed: true}
		}
		return p
	}
	steps := []struct {
		p      policy.Policy
		action bool
	}{
		{pol(), false},
		{pol("C"), true},  // C newly shed
		{pol("C"), false}, // still shed: not new
		{pol(), false},
		{pol("C", "A"), true},
	}
	for i, s := range steps {
		before := len(r.actions)
		r.policyApplied(at.Add(time.Duration(i)*time.Second), s.p, time.Millisecond)
		if got := len(r.actions) > before; got != s.action {
			t.Errorf("step %d: action %v, want %v", i, got, s.action)
		}
	}
	if len(r.latencies) != len(steps) {
		t.Errorf("%d latencies, want %d", len(r.latencies), len(steps))
	}
}

func TestCounterGap(t *testing.T) {
	tests := []struct {
		name   string
		states []fairq.TenantState
		want   float64
	}{
		{"nobody backlogged", []fairq.TenantState{{Tenant: "A", Counter: 5}}, math.NaN()},
		{"one backlogged", []fairq.TenantState{{Tenant: "A", Counter: 5, Queued: 1}, {Tenant: "B", Counter: 9}}, 0},
		{"two backlogged", []fairq.TenantState{{Tenant: "A", Counter: 5, Queued: 1}, {Tenant: "B", Counter: 9, Queued: 2}}, 4},
	}
	for _, tc := range tests {
		got := counterGap(tc.states)
		if got != tc.want && !(math.IsNaN(got) && math.IsNaN(tc.want)) {
			t.Errorf("%s: %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestInitialReplicas(t *testing.T) {
	cfg := fastConfig()
	tests := []struct {
		setup     string
		requested int
		want      int
	}{
		{sim.SetupFull, 0, 1}, // min replicas
		{sim.SetupFull, 2, 2},
		{sim.SetupFull, 9, 3}, // clamped to capacity
		{sim.SetupStaticPartition, 1, 3},
	}
	for _, tc := range tests {
		if got := initialReplicas(cfg, specs[tc.setup], tc.requested); got != tc.want {
			t.Errorf("%s requested %d: %d, want %d", tc.setup, tc.requested, got, tc.want)
		}
	}
}

func TestEngineInfoLabel(t *testing.T) {
	real := EngineInfo{Backend: config.BackendLlama, Build: "b11898-abc", ModelPath: `C:\m\qwen.gguf`}
	if got := real.Label(); got != "llama.cpp b11898-abc, model qwen.gguf" {
		t.Errorf("label %q", got)
	}
	if got := (EngineInfo{Simulated: true}).Label(); !strings.Contains(got, "SIMULATED") {
		t.Errorf("simulated label %q", got)
	}
}
