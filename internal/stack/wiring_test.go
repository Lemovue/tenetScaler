package stack

import (
	"context"
	"testing"
	"time"

	"tenantscaler/internal/config"
	"tenantscaler/internal/engine"
	"tenantscaler/internal/loadgen"
	"tenantscaler/internal/model"
	"tenantscaler/internal/pool"
	"tenantscaler/internal/sim"
	"tenantscaler/internal/workload"
)

// TestSetupWiring pins every setup's control plane and ablation switches
// (DESIGN.md 16): S2 KEDA on replica waiting + gateway queue, S3 none at
// full capacity, S4 KEDA on the gateway queue, S5 controller without
// gateway coupling, S6 without borrowing, S7 the full controller.
func TestSetupWiring(t *testing.T) {
	tests := []struct {
		setup                   string
		control                 controlKind
		fullPool                bool
		noCoupling, noBorrowing bool
	}{
		{sim.SetupKEDAQueue, controlKEDATotal, false, false, false},
		{sim.SetupStaticPartition, controlNone, true, false, false},
		{sim.SetupFlowControl, controlKEDAGateway, false, false, false},
		{sim.SetupNoGateway, controlCore, false, true, false},
		{sim.SetupNoBorrowing, controlCore, false, false, true},
		{sim.SetupFull, controlCore, false, false, false},
	}
	for _, tc := range tests {
		sp := specs[tc.setup]
		if sp.control != tc.control || sp.fullPool != tc.fullPool || HasController(tc.setup) != (tc.control == controlCore) {
			t.Errorf("%s: control %v fullPool %v, want %v and %v", tc.setup, sp.control, sp.fullPool, tc.control, tc.fullPool)
		}
		if tc.control != controlCore {
			continue
		}
		var c config.ControllerSpec
		sp.adjust(&c)
		if c.DisableGatewayCoupling != tc.noCoupling || c.DisableBorrowing != tc.noBorrowing {
			t.Errorf("%s: adjust gives coupling disabled %v, borrowing disabled %v; want %v and %v",
				tc.setup, c.DisableGatewayCoupling, c.DisableBorrowing, tc.noCoupling, tc.noBorrowing)
		}
	}
}

// TestControllerPolicyPublishing checks that S5 decides without
// publishing any gateway policy, while S6 and S7 publish policies the
// gateway applies.
func TestControllerPolicyPublishing(t *testing.T) {
	for _, setup := range []string{sim.SetupNoGateway, sim.SetupNoBorrowing, sim.SetupFull} {
		t.Run(setup, func(t *testing.T) {
			ctx := context.Background()
			st, err := Start(ctx, Options{Setup: setup, Config: fastConfig(), InitialReplicas: 1, Seed: 9, Logger: quiet()})
			if err != nil {
				t.Fatal(err)
			}
			defer st.Stop(ctx)
			deadline := time.Now().Add(5 * time.Second)
			for time.Now().Before(deadline) && len(st.Decisions(0)) < 3 {
				time.Sleep(50 * time.Millisecond)
			}
			if n := len(st.Decisions(0)); n < 3 {
				t.Fatalf("%d decisions after 5 s of 200 ms cycles", n)
			}
			published := len(st.PolicyLatencies()) > 0
			if want := setup != sim.SetupNoGateway; published != want {
				t.Errorf("policies applied: %v, want %v", published, want)
			}
		})
	}
}

// slowConfig is fastConfig with one-request replicas taking about 0.6 s
// per 30-token answer, so a burst queues.
func slowConfig() config.Config {
	cfg := fastConfig()
	cfg.Engine = config.EngineFromParams(engine.Params{
		PrefillOverhead: time.Millisecond, InterTokenLatency: 20 * time.Millisecond, MaxNumSeqs: 1,
		TimeFactorUnderLoad: 1, StartupDuration: 50 * time.Millisecond,
	})
	return cfg
}

// burst is n requests of tenant A sent at once.
func burst(n int) []workload.Spec {
	specs := make([]workload.Spec, n)
	for i := range specs {
		specs[i] = workload.Spec{Tenant: "A", Priority: model.PriorityStandard, InputTokens: 8, OutputTokens: 30}
	}
	return specs
}

// TestKEDAScalesOnQueue checks that the KEDA baselines act on a queue:
// S2's builds inside the replica (passthrough gateway), S4's in the
// gateway (round-robin dispatch window).
func TestKEDAScalesOnQueue(t *testing.T) {
	for _, setup := range []string{sim.SetupKEDAQueue, sim.SetupFlowControl} {
		t.Run(setup, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			st, err := Start(ctx, Options{Setup: setup, Config: slowConfig(), InitialReplicas: 1, Seed: 2, Logger: quiet()})
			if err != nil {
				t.Fatal(err)
			}
			defer st.Stop(context.Background())
			start := time.Now()
			done := make(chan struct{})
			go func() {
				defer close(done)
				_, _ = loadgen.Replay(ctx, burst(16), []loadgen.Target{{Name: "s", URL: st.URL()}}, loadgen.Options{Logger: quiet()})
			}()
			defer func() { cancel(); <-done }()
			deadline := time.Now().Add(5 * time.Second)
			for time.Now().Before(deadline) && len(st.ActionTimes(start)) == 0 {
				time.Sleep(50 * time.Millisecond)
			}
			if len(st.ActionTimes(start)) == 0 || st.PoolStatus().Desired < 2 {
				t.Fatalf("no scale-up on a queue of 15: actions %v, pool %+v", st.ActionTimes(start), st.PoolStatus())
			}
		})
	}
}

// TestKEDAQueueView checks the KEDA baselines' view of the replicas'
// waiting requests: S2 counts them, S4 (gateway queue only) does not.
func TestKEDAQueueView(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	params, err := slowConfig().Engine.Params()
	if err != nil {
		t.Fatal(err)
	}
	p := pool.New(pool.Config{Name: "q", MaxReplicas: 1, Backend: pool.NewSimulatedBackend(params, 1), Logger: quiet()})
	defer p.Close()
	if err := p.Scale(1); err != nil {
		t.Fatal(err)
	}
	if err := WaitReady(ctx, p, 1, 5*time.Second); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = loadgen.Replay(ctx, burst(3), []loadgen.Target{{Name: "r", URL: p.Endpoints()[0]}}, loadgen.Options{Logger: quiet()})
	}()
	defer func() { cancel(); <-done }()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && p.Status().Replicas[0].Waiting == 0 {
		time.Sleep(20 * time.Millisecond)
	}
	for setup, wantWaiting := range map[string]bool{sim.SetupKEDAQueue: true, sim.SetupFlowControl: false} {
		s := &Stack{pool: p, rec: newRecorder(), spec: specs[setup]}
		st, err := s.kedaPool().Status(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if got := st.Replicas[0].Waiting > 0; got != wantWaiting {
			t.Errorf("%s: KEDA sees %d waiting requests, want waiting counted: %v", setup, st.Replicas[0].Waiting, wantWaiting)
		}
	}
}

func TestControlPhase(t *testing.T) {
	const cycle = 10 * time.Second
	seen := map[time.Duration]bool{}
	for seed := range int64(20) {
		ph := controlPhase(seed, cycle)
		if ph < 0 || ph >= cycle || ph%time.Millisecond != 0 {
			t.Fatalf("seed %d: phase %v, want whole milliseconds in [0, %v)", seed, ph, cycle)
		}
		if again := controlPhase(seed, cycle); again != ph {
			t.Fatalf("seed %d: phase %v then %v", seed, ph, again)
		}
		seen[ph] = true
	}
	if len(seen) < 15 {
		t.Errorf("only %d distinct phases over 20 seeds", len(seen))
	}
	if ph := controlPhase(1, 0); ph != 0 {
		t.Errorf("phase %v for a zero cycle", ph)
	}
}
