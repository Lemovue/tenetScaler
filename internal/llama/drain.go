package llama

import (
	"context"
	"fmt"
	"time"
)

// BeginDrain takes the replica out of the ready set (State "draining").
// llama-server itself has no drain mode: the pool stops listing it as an
// endpoint, and WaitDrained waits for the requests it is still serving.
func (p *Process) BeginDrain() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.draining {
		p.draining, p.drainBegan = true, time.Now()
	}
}

// WaitDrained returns nil once the process can be stopped without cutting
// off a request:
//   - it has been draining for at least the drain grace, so every gateway
//     has dropped it from its endpoint list and no new request can arrive,
//     and
//   - two consecutive /metrics readings drainPollInterval apart show no
//     processing or deferred request.
//
// If /metrics fails maxDrainFailures times in a row, the process is
// declared drained anyway (logged at Warn), so that a server with a hung or
// disabled /metrics is still stopped. A process that never became ready
// (never an endpoint) or has exited is drained at once. WaitDrained returns
// an error when ctx ends first.
func (p *Process) WaitDrained(ctx context.Context) error {
	tick := time.NewTicker(drainPollInterval)
	defer tick.Stop()
	idle, failures := 0, 0
	for {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("llama-server %s: waiting for drain: %w", p.id, err)
		}
		p.mu.Lock()
		draining, ready, began := p.draining, p.ready, p.drainBegan
		p.mu.Unlock()
		if draining {
			if !ready {
				return nil // never an endpoint: nothing in flight
			}
			rctx, cancel := context.WithTimeout(ctx, requestTimeout)
			s, err := p.fetchStats(rctx)
			cancel()
			switch {
			case err != nil:
				idle, failures = 0, failures+1
			case s.Running == 0 && s.Waiting == 0:
				idle, failures = idle+1, 0
			default:
				idle, failures = 0, 0
			}
			if time.Since(began) >= p.drainGrace {
				if idle >= 2 {
					return nil
				}
				if failures >= maxDrainFailures && ctx.Err() == nil {
					p.log.Warn("llama-server /metrics keeps failing while draining; stopping it anyway",
						"failures", failures, "err", err)
					return nil
				}
			}
		}
		select {
		case <-p.exited:
			return nil
		case <-ctx.Done():
			return fmt.Errorf("llama-server %s: waiting for drain: %w", p.id, ctx.Err())
		case <-tick.C:
		}
	}
}
