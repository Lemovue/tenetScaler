package demo

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"path/filepath"
	"sync"
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
	addr     string // requested public address, e.g. 0.0.0.0:8081
	keys     map[string]gateway.KeyGrant
	baseline bool   // side by side: the baseline stack gets a copy of every request
	public   string // the address actually listening, once started
}

// StartLive starts a live run in the background: the TenantScaler stack,
// whose gateway also listens on addr for teammates' laptops (tenant API keys
// required, PublicHandler), sampled until StopAB. With baseline, the
// baseline stack runs too, side by side: addr is then served by a mirror
// that sends every request to TenantScaler (the client gets its answer) and
// a copy to the baseline, so both panels show the same live traffic. The
// traffic comes from outside (tenantscaler loadgen -gateway ...), so there
// is no replay and no end-of-run metrics. It returns ErrBusy while a run is
// in progress and ErrClosed after Close.
func (s *Server) StartLive(addr string, keys map[string]gateway.KeyGrant, baseline bool) (int, error) {
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
	r.liveMode = &liveMode{addr: addr, keys: keys, baseline: baseline}
	r.message = "Starting the live baseline and TenantScaler stacks, side by side (" + LiveLabel + ")"
	if !baseline {
		r.stacks[0].status = StatusSkipped // the baseline does not run in a plain live demo
		r.message = "Starting the live TenantScaler stack (" + LiveLabel + ")"
	}
	s.run = r
	go r.executeLive(ctx)
	return s.runs, nil
}

// ErrNoLive is returned by GoLive when the server was not given a live
// address (demo -live).
var ErrNoLive = errors.New("demo: no live address: start the dashboard with -live to enable Go live")

// GoLive starts the live run the server was configured with
// (Options.LiveAddr), as `demo -live` did at start: after a stop or an A/B
// run, the dashboard's Go live button returns to the teammates' traffic
// without restarting the server.
func (s *Server) GoLive() (int, error) {
	if s.opt.LiveAddr == "" {
		return 0, ErrNoLive
	}
	return s.StartLive(s.opt.LiveAddr, s.opt.LiveKeys, s.opt.LiveSideBySide)
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
	if r.liveMode.baseline {
		return r.runLiveSideBySide(ctx, log)
	}
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

// runLiveSideBySide starts both stacks with the run's tenant keys, serves
// the public address with a mirror (TenantScaler answers, the baseline gets
// a copy), samples both until ctx is canceled, then stops everything.
func (r *abRun) runLiveSideBySide(ctx context.Context, log *slog.Logger) error {
	if r.opt.Guard != nil {
		if err := r.opt.Guard(ctx); err != nil {
			return r.fail(ctx, 1, fmt.Errorf("machine-safety guard: %w", err))
		}
	}
	stopAll := func(sts []*stack.Stack, start time.Time) error {
		stopCtx, cancel := context.WithTimeout(context.Background(), stopTimeout)
		defer cancel()
		var err error
		for i, st := range sts {
			if st == nil {
				continue
			}
			r.setStatus(i, StatusStopping, "Stopping the live stacks")
			err = errors.Join(err, st.Stop(stopCtx))
			r.stackStopped(i, st, start)
		}
		return err
	}
	sts := make([]*stack.Stack, len(r.stacks))
	for i, sr := range r.stacks {
		r.setStatus(i, StatusStarting, fmt.Sprintf("%s: starting %d warm replicas (%s)", sr.plan.title, r.opt.InitialReplicas, r.opt.Backend.Label))
		opt := stack.Options{Setup: sr.plan.setup, Config: r.opt.Config, InitialReplicas: r.opt.InitialReplicas,
			LogDir: r.opt.LogDir, Seed: r.seed, Logger: r.opt.Logger.With("liveRun", r.id, "stack", sr.plan.key),
			TenantKeys: r.liveMode.keys}
		if r.opt.LogDir != "" && stack.HasController(sr.plan.setup) {
			opt.DecisionsFile = filepath.Join(r.opt.LogDir, r.decisionsFileName())
		}
		st, err := stack.Start(ctx, opt)
		if err != nil {
			_ = stopAll(sts, time.Now())
			return r.fail(ctx, i, err)
		}
		sts[i] = st
	}
	const base, ts = 0, 1
	ln, err := net.Listen("tcp", r.liveMode.addr)
	if err != nil {
		_ = stopAll(sts, time.Now())
		return r.fail(ctx, ts, fmt.Errorf("listen on the public address %s: %w", r.liveMode.addr, err))
	}
	pub := &http.Server{Handler: newMirror(ctx, sts[ts].URL(), sts[base].URL(), log), ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = pub.Serve(ln) }()
	start := time.Now()
	for i, st := range sts {
		r.trafficStarted(i, st, start, 0)
	}
	r.mu.Lock()
	r.liveMode.public = ln.Addr().String()
	r.message = fmt.Sprintf("Live, side by side (%s): teammates send traffic to http://%s; TenantScaler answers, the baseline gets a copy of every request",
		LiveLabel, ln.Addr())
	r.mu.Unlock()
	log.Info("live side-by-side stacks running", "public", ln.Addr().String(),
		"baseline", sts[base].URL(), "tenantscaler", sts[ts].URL())
	var wg sync.WaitGroup
	for i, st := range sts {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r.sample(ctx, i, st, start) // until the run is stopped
		}()
	}
	wg.Wait()

	shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	_ = pub.Shutdown(shutCtx)
	cancel()
	stopErr := stopAll(sts, start)
	_ = r.fail(ctx, base, errors.Join(ctx.Err(), stopErr))
	return r.fail(ctx, ts, errors.Join(ctx.Err(), stopErr))
}
