package loadgen

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"tenantscaler/internal/workload"
)

// liveDrawSeconds is the length of the rate-1 trace each live tenant draws
// its gaps, priorities and lengths from (about this many requests); the
// draws repeat after that, the prompts never do.
const liveDrawSeconds = 1 << 14

// Live generates a scenario's traffic in real time, indefinitely, and sends
// every request to every target at the same moment (same tenant, priority,
// lengths and prompt). It drives the live demo.
//
// The spike tenant (Options.SpikeTenant; C in the default scenario)
// ignores its scenario window: it runs at BaseRPS until Spike is called
// and at PeakRPS while the spike lasts. Every other tenant follows its
// pattern cyclically, including spike tenants other than the spike tenant
// (A in ReclaimScenario): the rate at wall time t is workload.RateAt at
// scenario time (Speed*t) modulo the scenario Duration.
//
// Arrivals of each tenant form a Gamma renewal process with the pattern's
// CV mapped through the time-varying rate (time rescaling), as in
// workload.Generate; the operational-time gaps, priorities and lengths come
// from workload.Generate itself, run on a steady rate-1 copy of the tenant,
// so they follow exactly the distributions of the offline traces. Each
// arrival is scheduled from the previous scheduled arrival, not from when
// the generator woke up, so timer lateness does not lower the rate.
type Live struct {
	scenario workload.Scenario
	speed    float64
	tenants  []*liveTenant
	s        *sender
	seq      atomic.Uint64 // last request ID

	mu         sync.Mutex
	spikeUntil time.Time
	spikeCh    chan struct{} // closed and replaced whenever the spike changes
	started    bool
	stop       context.CancelFunc
	gens       sync.WaitGroup
}

// liveTenant is one tenant's generator state; only its goroutine uses it
// once the generator has started.
type liveTenant struct {
	pattern workload.TenantPattern
	// spiked marks the tenant that Spike controls.
	spiked bool
	draws  []workload.Spec
	next   int
	// mark is the wall time up to which the tenant's rate has been
	// integrated: the last scheduled arrival, or the start.
	mark time.Time
}

// NewLive prepares a live generator for scenario (validated) sending to
// targets. seed keys the arrival gaps, priorities, lengths and prompts
// (opt.Seed is ignored); use a different seed for every run against the
// same long-lived llama-server replicas (see Options.Seed). opt.Speed
// compresses the scenario's pattern time; Spike durations are wall-clock.
// opt.SpikeTenant selects the tenant Spike controls.
func NewLive(scenario workload.Scenario, targets []Target, seed int64, opt Options) (*Live, error) {
	if err := scenario.Validate(); err != nil {
		return nil, fmt.Errorf("loadgen: %w", err)
	}
	speed, err := opt.speed()
	if err != nil {
		return nil, err
	}
	spike, err := spikeTenantOf(scenario, opt.SpikeTenant)
	if err != nil {
		return nil, err
	}
	opt.Seed = seed
	s, err := newSender(targets, opt)
	if err != nil {
		return nil, err
	}
	l := &Live{scenario: scenario, speed: speed, s: s, spikeCh: make(chan struct{})}
	for _, p := range scenario.Tenants {
		steady := p
		steady.Kind, steady.BaseRPS, steady.PeakRPS = workload.KindSteady, 1, 1
		draws := workload.Generate(workload.Scenario{
			Name:     scenario.Name + "-live",
			Duration: liveDrawSeconds * time.Second,
			Tenants:  []workload.TenantPattern{steady},
			Samples:  scenario.Samples,
		}, seed)
		if len(draws) == 0 {
			return nil, fmt.Errorf("loadgen: tenant %q: no request draws generated", p.Tenant)
		}
		l.tenants = append(l.tenants, &liveTenant{pattern: p, spiked: p.Tenant == spike, draws: draws})
	}
	return l, nil
}

// spikeTenantOf resolves Options.SpikeTenant for scenario: the named spike
// tenant, or the only one when name is empty ("" when there is none).
func spikeTenantOf(sc workload.Scenario, name string) (string, error) {
	var spikes []string
	for _, p := range sc.Tenants {
		if p.Kind == workload.KindSpike {
			spikes = append(spikes, p.Tenant)
		}
	}
	switch {
	case name != "" && !slices.Contains(spikes, name):
		return "", fmt.Errorf("loadgen: spike tenant %q is not a spike tenant of scenario %q (spike tenants: %v)",
			name, sc.Name, spikes)
	case name != "":
		return name, nil
	case len(spikes) > 1:
		return "", fmt.Errorf("loadgen: scenario %q has several spike tenants %v: set Options.SpikeTenant", sc.Name, spikes)
	case len(spikes) == 1:
		return spikes[0], nil
	}
	return "", nil
}

// Start begins generating; it returns at once. Requests run under ctx:
// canceling ctx stops the generators and aborts requests in flight, while
// Stop only stops new arrivals. A Live can be started once.
func (l *Live) Start(ctx context.Context) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.started {
		return errors.New("loadgen: live generator already started")
	}
	l.started = true
	genCtx, cancel := context.WithCancel(ctx)
	l.stop = cancel
	l.s.start = time.Now()
	for _, t := range l.tenants {
		t.mark = l.s.start
		l.gens.Add(1)
		go func() {
			defer l.gens.Done()
			l.run(genCtx, ctx, t)
		}()
	}
	return nil
}

// Stop stops generating new requests. Requests already sent run to
// completion unless the context given to Start is canceled.
func (l *Live) Stop() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.stop != nil {
		l.stop()
	}
}

// Wait blocks until the generators have stopped (Stop, or the Start
// context canceled) and every request sent has finished, then logs a
// summary. It returns at once if Start was never called.
func (l *Live) Wait() {
	l.mu.Lock()
	started := l.started
	l.mu.Unlock()
	if !started {
		return
	}
	l.gens.Wait()
	l.s.wg.Wait()
	l.s.logSummary("live generator stopped")
	l.s.closeIdle()
}

// Spike makes the spike tenant (Options.SpikeTenant) send at its PeakRPS
// for d (wall-clock) from now, replacing any spike in progress; d <= 0
// ends a spike. Without a spike tenant it only changes SpikeState.
func (l *Live) Spike(d time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.spikeUntil = time.Now().Add(max(d, 0))
	close(l.spikeCh)
	l.spikeCh = make(chan struct{})
}

// SpikeState reports whether a spike is in progress and when it ends (or
// ended; zero if Spike was never called).
func (l *Live) SpikeState() (active bool, until time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return time.Now().Before(l.spikeUntil), l.spikeUntil
}

// Counts returns the outcomes recorded so far, by target name.
func (l *Live) Counts() map[string]Counts { return l.s.tally.snapshot() }

// run generates one tenant's arrivals until genCtx is done; requests are
// sent under reqCtx.
func (l *Live) run(genCtx, reqCtx context.Context, t *liveTenant) {
	for {
		gap, spec := t.draw()
		if !l.waitOperational(genCtx, t, gap) {
			return
		}
		l.s.dispatch(reqCtx, l.seq.Add(1), spec, nil)
	}
}

// draw returns the next operational-time gap (seconds) and the request
// attributes, cycling through the tenant's draws.
func (t *liveTenant) draw() (float64, workload.Spec) {
	d := t.draws[t.next]
	var prev time.Duration
	if t.next > 0 {
		prev = t.draws[t.next-1].Offset
	}
	t.next = (t.next + 1) % len(t.draws)
	return (d.Offset - prev).Seconds(), d
}
