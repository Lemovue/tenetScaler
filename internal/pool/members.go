package pool

import (
	"fmt"
	"time"

	"tenantscaler/internal/replica"
)

// Reconcile timing.
const (
	// DefaultReconcileInterval is how often a pool checks, between Scale
	// calls, for replicas that stopped on their own and for missing ones.
	DefaultReconcileInterval = time.Second
	// maxReconcileBackoff caps the wait between rounds that keep having to
	// repair something (a replica that crashes at start-up).
	maxReconcileBackoff = 30 * time.Second
)

// member is one hosted instance.
type member struct {
	inst     Instance
	slot     int
	draining bool // chosen for removal; guarded by Pool.mu
}

// reconcileLoop runs reconcile rounds until Close, so that crashed or
// never-launched replicas are replaced even when no caller invokes Scale
// again (a baseline autoscaler that only scales when its target changes,
// or a standalone pool).
func (p *Pool) reconcileLoop(interval time.Duration) {
	defer p.wg.Done()
	wait := interval
	timer := time.NewTimer(wait)
	defer timer.Stop()
	for {
		select {
		case <-p.ctx.Done():
			return
		case <-timer.C:
		}
		wait = nextReconcileWait(wait, interval, p.reconcile())
		timer.Reset(wait)
	}
}

// nextReconcileWait is the wait before the next round: interval after a
// clean round, else the current wait doubled and capped at
// max(maxReconcileBackoff, interval).
func nextReconcileWait(cur, interval time.Duration, clean bool) time.Duration {
	if clean {
		return interval
	}
	return min(2*cur, max(maxReconcileBackoff, interval))
}

// reconcile replaces replicas that stopped on their own and launches
// missing ones. It reports whether the round was clean: nothing had
// stopped and every launch succeeded.
func (p *Pool) reconcile() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return true
	}
	reaped := p.reapStoppedLocked()
	if err := p.fillLocked(); err != nil {
		p.log.Warn("launch replica failed; retrying later", "pool", p.cfg.Name, "err", err)
		return false
	}
	return reaped == 0
}

// activeLocked lists the members not chosen for removal, oldest first.
func (p *Pool) activeLocked() []*member {
	var active []*member
	for _, m := range p.members {
		if !m.draining {
			active = append(active, m)
		}
	}
	return active
}

// fillLocked launches replicas until desired ones are active.
func (p *Pool) fillLocked() error {
	for n := len(p.activeLocked()); n < p.desired; n++ {
		if err := p.startLocked(); err != nil {
			return err
		}
	}
	return nil
}

// trimLocked drains the active replicas beyond the desired count, in
// drainOrder.
func (p *Pool) trimLocked() {
	active := p.activeLocked()
	if surplus := len(active) - p.desired; surplus > 0 {
		for _, m := range drainOrder(active)[:surplus] {
			p.retireLocked(m)
		}
	}
}

// reapStoppedLocked removes active members whose instance stopped on its
// own (a crashed process, one killed by its ready timeout) and closes them
// in the background, freeing their slots for replacements. It returns how
// many it removed.
func (p *Pool) reapStoppedLocked() int {
	kept := p.members[:0]
	var dead []*member
	for _, m := range p.members {
		if !m.draining && m.inst.State() == replica.StateStopped {
			dead = append(dead, m)
			continue
		}
		kept = append(kept, m)
	}
	clear(p.members[len(kept):])
	p.members = kept
	for _, m := range dead {
		attrs := []any{"pool", p.cfg.Name, "replica", m.inst.ID(), "slot", m.slot}
		if f, ok := m.inst.(Failer); ok {
			if cause := f.Err(); cause != nil {
				attrs = append(attrs, "cause", cause)
			}
		}
		p.log.Warn("replica stopped on its own; replacing it", attrs...)
		p.wg.Add(1)
		go func() {
			defer p.wg.Done()
			if err := m.inst.Close(); err != nil {
				p.log.Warn("close stopped replica", "replica", m.inst.ID(), "err", err)
			}
		}()
	}
	return len(dead)
}

// drainOrder lists active members in removal order: starting replicas
// first, then the others, newest first within each group.
func drainOrder(active []*member) []*member {
	var starting, rest []*member
	for i := len(active) - 1; i >= 0; i-- {
		if active[i].inst.State() == replica.StateStarting {
			starting = append(starting, active[i])
		} else {
			rest = append(rest, active[i])
		}
	}
	return append(starting, rest...)
}

// pickSlot returns the CPU slot for a new instance: the lowest slot in
// [0, max(maxReplicas, 1)) that no member holds; else the lowest slot held
// only by draining members, which the new instance shares until they
// stop.
func pickSlot(members []*member, maxReplicas int) int {
	holders := map[int]int{} // slot -> members holding it
	active := map[int]bool{} // slot -> held by a non-draining member
	for _, m := range members {
		holders[m.slot]++
		if !m.draining {
			active[m.slot] = true
		}
	}
	slots := max(maxReplicas, 1)
	for s := range slots {
		if holders[s] == 0 {
			return s
		}
	}
	for s := range slots {
		if !active[s] {
			return s
		}
	}
	return 0 // unreachable while active members < maxReplicas
}

// startLocked launches the next replica on a free slot.
func (p *Pool) startLocked() error {
	id := fmt.Sprintf("%s-%d", p.cfg.Name, p.seq+1)
	slot := pickSlot(p.members, p.cfg.MaxReplicas)
	inst, err := p.backend.Launch(id, slot)
	if err != nil {
		return fmt.Errorf("pool %s: launch replica %s: %w", p.cfg.Name, id, err)
	}
	p.seq++
	p.members = append(p.members, &member{inst: inst, slot: slot})
	return nil
}

// retireLocked starts draining m and closes it in the background once it
// is idle.
func (p *Pool) retireLocked(m *member) {
	m.draining = true
	m.inst.BeginDrain()
	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		if m.inst.WaitDrained(p.ctx) != nil {
			return // pool closed: Close stops the instance
		}
		if err := m.inst.Close(); err != nil {
			p.log.Warn("close drained replica", "pool", p.cfg.Name, "replica", m.inst.ID(), "err", err)
		}
		p.mu.Lock()
		defer p.mu.Unlock()
		for i, x := range p.members {
			if x == m {
				p.members = append(p.members[:i], p.members[i+1:]...)
				break
			}
		}
	}()
}
