package demo

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"time"

	"tenantscaler/internal/gateway"
	"tenantscaler/internal/stack"
)

// LiveLabel marks live-demo traffic wherever the dashboard shows it
// (decision Q16): it comes from teammates' laptops, not from the replayed
// experiment trace, so its numbers are not results.
const LiveLabel = "live demo, not the experiment"

// liveMode is what a live run adds to an abRun.
type liveMode struct {
	addr   string // requested public address, e.g. 0.0.0.0:8081
	keys   map[string]gateway.KeyGrant
	public string // the address actually listening, once started
}

// StartLive starts a live run in the background: the TenantScaler stack
// only, whose gateway also listens on addr for teammates' laptops (tenant
// API keys required, PublicHandler), sampled until StopAB. The traffic comes
// from outside (tenantscaler loadgen -gateway ...), so there is no replay
// and no end-of-run metrics. It returns ErrBusy while a run is in progress
// and ErrClosed after Close.
func (s *Server) StartLive(addr string, keys map[string]gateway.KeyGrant) (int, error) {
	if addr == "" || len(keys) == 0 {
		return 0, errors.New("demo: a live run needs a public address and tenant API keys ($TS_TENANT_KEYS)")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return 0, ErrClosed
	}
	if s.run != nil && !s.run.finished() {
		return 0, ErrBusy
	}
	s.runs++
	ctx, cancel := context.WithCancel(context.Background())
	r := newRun(s.runs, s.opt.Seed, &s.opt, s.roles, cancel)
	r.liveMode = &liveMode{addr: addr, keys: keys}
	r.stacks[0].status = StatusSkipped // the baseline does not run in a live demo
	r.message = "Starting the live TenantScaler stack (" + LiveLabel + ")"
	s.run = r
	go r.executeLive(ctx)
	return s.runs, nil
}

// executeLive runs the live stack until the run is stopped.
func (r *abRun) executeLive(ctx context.Context) {
	defer close(r.done)
	defer r.cancel()
	log := r.opt.Logger.With("liveRun", r.id)
	log.Info("live run started", "addr", r.liveMode.addr, "tenantKeys", len(r.liveMode.keys))
	r.end(r.runLive(ctx, log), log)
}

// runLive starts the TenantScaler stack with its public listener, samples
// it until ctx is canceled, then stops it.
func (r *abRun) runLive(ctx context.Context, log *slog.Logger) error {
	const i = 1 // the TenantScaler panel
	sr := r.stacks[i]
	r.setStatus(i, StatusStarting, fmt.Sprintf("%s: starting %d warm replicas (%s)", sr.plan.title, r.opt.InitialReplicas, r.opt.Backend.Label))
	if r.opt.Guard != nil {
		if err := r.opt.Guard(ctx); err != nil {
			return r.fail(ctx, i, fmt.Errorf("machine-safety guard: %w", err))
		}
	}
	opt := stack.Options{Setup: sr.plan.setup, Config: r.opt.Config, InitialReplicas: r.opt.InitialReplicas,
		LogDir: r.opt.LogDir, Seed: r.seed, Logger: r.opt.Logger.With("liveRun", r.id),
		PublicAddr: r.liveMode.addr, TenantKeys: r.liveMode.keys}
	if r.opt.LogDir != "" {
		opt.DecisionsFile = filepath.Join(r.opt.LogDir, r.decisionsFileName())
	}
	st, err := stack.Start(ctx, opt)
	if err != nil {
		return r.fail(ctx, i, err)
	}
	start := time.Now()
	r.trafficStarted(i, st, start, 0)
	r.mu.Lock()
	r.liveMode.public = st.PublicAddr()
	r.message = fmt.Sprintf("Live (%s): teammates send traffic to http://%s with tenantscaler loadgen -gateway", LiveLabel, st.PublicAddr())
	r.mu.Unlock()
	log.Info("live stack running", "public", st.PublicAddr(), "internal", st.URL())
	r.sample(ctx, i, st, start) // until the run is stopped

	r.setStatus(i, StatusStopping, "Stopping the live stack")
	stopCtx, cancel := context.WithTimeout(context.Background(), stopTimeout)
	stopErr := st.Stop(stopCtx)
	cancel()
	r.stackStopped(i, st, start)
	return r.fail(ctx, i, errors.Join(ctx.Err(), stopErr))
}
