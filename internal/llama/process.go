package llama

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os/exec"
	"strconv"
	"sync"
	"time"

	"tenantscaler/internal/replica"
)

// Timing of the process watchers.
const (
	// healthPollInterval is the /health polling period while starting; it
	// bounds the cold-start measurement error.
	healthPollInterval = 50 * time.Millisecond
	// statsMaxAge is how long Stats serves a cached /metrics reading.
	statsMaxAge = 250 * time.Millisecond
	// statsWarnEvery rate-limits the warning about failing /metrics reads.
	statsWarnEvery = 10 * time.Second
	// drainPollInterval spaces the /metrics readings of WaitDrained; two
	// consecutive idle readings are required, which gives the HTTP thread
	// time to flush the last response after its slot was released.
	drainPollInterval = 200 * time.Millisecond
	// maxDrainFailures is how many consecutive failed /metrics readings
	// make WaitDrained give up and declare the replica drained, so that a
	// server with a hung or disabled /metrics is still stopped.
	maxDrainFailures = 10
	// requestTimeout bounds one /health or /metrics request.
	requestTimeout = 2 * time.Second
	// exitTimeout bounds the wait for a killed process to be reaped.
	exitTimeout = 10 * time.Second
)

// Process is one REAL llama-server replica. It implements pool.Instance
// and pool.ColdStarter, and is safe for concurrent use.
type Process struct {
	id, url, logPath string
	slot, port, pid  int
	cmd              *exec.Cmd
	log              *slog.Logger
	hc               *http.Client
	launched         time.Time
	readyTimeout     time.Duration
	drainGrace       time.Duration    // minimum time from BeginDrain to drained
	jobErr           error            // orphan-protection failure, if any
	statsClock       func() time.Time // time source of the Stats cache

	exited   chan struct{} // closed once the process has exited and been reaped
	exitErr  error         // cmd.Wait result; read only after exited is closed
	stopPoll context.CancelFunc
	polled   chan struct{} // closed when the readiness poller has returned

	mu         sync.Mutex
	ready      bool          // /health returned 200 at least once
	coldStart  time.Duration // launch to first healthy response
	draining   bool
	drainBegan time.Time // first BeginDrain
	closing    bool      // Close has begun
	failure    error     // why the process was stopped early (ready timeout)

	statsMu     sync.Mutex // serialises /metrics fetches for Stats
	stats       replica.Stats
	statsAt     time.Time
	statsWarnAt time.Time // last warning about a failed fetch

	closeOnce sync.Once
	closeErr  error
}

// processOpts are a started llama-server and its settings.
type processOpts struct {
	id, logPath              string
	slot, port               int
	cmd                      *exec.Cmd // started
	log                      *slog.Logger
	launched                 time.Time
	readyTimeout, drainGrace time.Duration
	jobErr                   error
}

// newProcess wraps a started llama-server and starts its watchers: one
// reaps the process, the other polls /health until it is ready.
func newProcess(o processOpts) *Process {
	ctx, cancel := context.WithCancel(context.Background())
	p := &Process{
		id: o.id, url: "http://127.0.0.1:" + strconv.Itoa(o.port), logPath: o.logPath,
		slot: o.slot, port: o.port, pid: o.cmd.Process.Pid, cmd: o.cmd, log: o.log,
		hc:       &http.Client{Transport: &http.Transport{MaxIdleConnsPerHost: 2}},
		launched: o.launched, readyTimeout: o.readyTimeout, drainGrace: o.drainGrace,
		jobErr: o.jobErr, statsClock: time.Now,
		exited: make(chan struct{}), stopPoll: cancel, polled: make(chan struct{}),
	}
	go p.reap()
	go p.pollReady(ctx)
	return p
}

// ID returns the replica ID.
func (p *Process) ID() string { return p.id }

// URL returns the server's base URL, http://127.0.0.1:<port>.
func (p *Process) URL() string { return p.url }

// PID returns the operating-system process ID.
func (p *Process) PID() int { return p.pid }

// Slot returns the CPU slot the process was launched on.
func (p *Process) Slot() int { return p.slot }

// LogPath returns the file that receives the server's output.
func (p *Process) LogPath() string { return p.logPath }

// ColdStart returns the measured time from launch to the first healthy
// /health response (resolution healthPollInterval), or 0 until then.
func (p *Process) ColdStart() time.Duration {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.coldStart
}

// Err explains why the process stopped: nil while it runs or after Close,
// else the readiness timeout or the exit status of a crash.
func (p *Process) Err() error {
	select {
	case <-p.exited:
	default:
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	switch {
	case p.failure != nil:
		return p.failure
	case p.closing:
		return nil
	case p.exitErr != nil:
		return fmt.Errorf("llama-server %s exited: %w (see %s)", p.id, p.exitErr, p.logPath)
	default:
		return fmt.Errorf("llama-server %s exited unexpectedly (see %s)", p.id, p.logPath)
	}
}

// State is "starting" until /health first answers 200, then "ready";
// "draining" after BeginDrain; "stopped" once the process has exited, on
// its own or through Close.
func (p *Process) State() string {
	select {
	case <-p.exited:
		return replica.StateStopped
	default:
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	switch {
	case p.draining:
		return replica.StateDraining
	case p.ready:
		return replica.StateReady
	default:
		return replica.StateStarting
	}
}

// Close kills the process (in-flight requests abort) and waits until it
// has been reaped. It is idempotent and safe for concurrent use.
func (p *Process) Close() error {
	p.closeOnce.Do(func() {
		p.mu.Lock()
		p.closing = true
		p.mu.Unlock()
		p.stopPoll()
		killErr := p.kill()
		select {
		case <-p.exited:
		case <-time.After(exitTimeout):
			p.closeErr = errors.Join(
				fmt.Errorf("llama-server %s (pid %d) still running %s after kill", p.id, p.pid, exitTimeout), killErr)
		}
		<-p.polled
		p.hc.CloseIdleConnections()
	})
	return p.closeErr
}

// kill terminates the process unless it has already exited. Its error is
// only meaningful when the process then fails to exit.
func (p *Process) kill() error {
	select {
	case <-p.exited:
		return nil
	default:
	}
	if err := p.cmd.Process.Kill(); err != nil {
		return fmt.Errorf("kill llama-server %s (pid %d): %w", p.id, p.pid, err)
	}
	return nil
}

// reap waits for the process to exit and records why.
func (p *Process) reap() {
	err := p.cmd.Wait()
	p.mu.Lock()
	p.exitErr = err
	closing, failure := p.closing, p.failure
	p.mu.Unlock()
	releasePort(p.port)
	switch {
	case closing:
		p.log.Info("llama-server stopped")
	case failure != nil:
		p.log.Warn("llama-server stopped", "err", failure, "log", p.logPath)
	default:
		p.log.Warn("llama-server exited on its own", "err", err, "log", p.logPath)
	}
	close(p.exited)
}

// pollReady polls /health until it answers 200, recording the cold start.
// It kills a process that is not ready within readyTimeout.
func (p *Process) pollReady(ctx context.Context) {
	defer close(p.polled)
	deadline := time.NewTimer(p.readyTimeout)
	defer deadline.Stop()
	tick := time.NewTicker(healthPollInterval)
	defer tick.Stop()
	for {
		if p.healthy(ctx) {
			cold := time.Since(p.launched)
			p.mu.Lock()
			p.ready, p.coldStart = true, cold
			p.mu.Unlock()
			p.log.Info("llama-server ready", "url", p.url, "coldStart", cold.Round(time.Millisecond))
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-p.exited:
			return
		case <-deadline.C:
			p.mu.Lock()
			p.failure = fmt.Errorf("llama-server %s not ready within %s (see %s)", p.id, p.readyTimeout, p.logPath)
			p.mu.Unlock()
			if err := p.kill(); err != nil {
				p.log.Warn("kill unready llama-server", "err", err)
			}
			return
		case <-tick.C:
		}
	}
}

// healthy reports whether GET /health answers 200 (llama-server answers
// 503 while the model loads).
func (p *Process) healthy(ctx context.Context) bool {
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.url+"/health", nil)
	if err != nil {
		return false
	}
	resp, err := p.hc.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	drain(resp)
	return resp.StatusCode == http.StatusOK
}
