package loadgen

import (
	"context"
	"math"
	"time"

	"tenantscaler/internal/workload"
)

// maxWait bounds one sleep of a generator, so that a tiny rate cannot
// overflow a Duration; the generator then re-evaluates its rate.
const maxWait = time.Hour

// minBoundaryWait keeps the generator from spinning when float rounding
// puts a rate boundary a hair before the time it is computed from.
const minBoundaryWait = time.Millisecond

// maxCatchUp bounds, in operational time (about that many arrivals), how
// far a tenant's schedule may fall behind the clock: after a stall the
// generator sends at most this many overdue arrivals back to back and
// drops the rest of the backlog.
const maxCatchUp = 10.0

// waitOperational waits until the integral of the tenant's wall-clock rate
// from t.mark reaches need (operational time) and moves t.mark to that
// instant, the scheduled arrival. Integrating from the previous scheduled
// arrival rather than from when the call starts keeps timer lateness and
// the dispatch work between arrivals out of the gaps, so the realised rate
// is the pattern's rate, as in Replay, which schedules against absolute
// times. The rate is re-evaluated at every known rate boundary and
// whenever the spike state changes. It returns false when ctx is done
// first.
func (l *Live) waitOperational(ctx context.Context, t *liveTenant, need float64) bool {
	for {
		l.mu.Lock()
		changed := l.spikeCh
		l.mu.Unlock()
		from := t.mark
		rate, boundary := l.rateAt(t, from)

		// end closes the stretch [from, end) run at this rate (zero: open
		// until the spike changes); arrival tells whether it ends with the
		// arrival rather than at a rate boundary.
		var end time.Time
		arrival := false
		if rate > 0 {
			if w := need / rate; w <= maxWait.Seconds() {
				end, arrival = from.Add(seconds(w)), true
			} else {
				end = from.Add(maxWait)
			}
		}
		if !boundary.IsZero() {
			if b := laterOf(boundary, from.Add(minBoundaryWait)); end.IsZero() || b.Before(end) {
				end, arrival = b, false
			}
		}
		if !waitUntil(ctx, end, changed) {
			return false
		}
		now := time.Now()
		switch {
		case end.IsZero() || now.Before(end): // the spike changed first
			need -= rate * now.Sub(from).Seconds()
			t.mark = now
		case arrival:
			t.mark = catchUp(end, now, rate)
			return true
		default: // rate boundary
			need -= rate * end.Sub(from).Seconds()
			t.mark = end
		}
	}
}

// waitUntil waits until the time end (zero: no deadline), a spike change
// or ctx is done; it returns false when ctx is done.
func waitUntil(ctx context.Context, end time.Time, changed <-chan struct{}) bool {
	var timeout <-chan time.Time
	if !end.IsZero() {
		d := time.Until(end)
		if d <= 0 {
			return ctx.Err() == nil
		}
		timer := time.NewTimer(d)
		defer timer.Stop()
		timeout = timer.C
	}
	select {
	case <-ctx.Done():
		return false
	case <-changed:
	case <-timeout:
	}
	return true
}

// catchUp returns the mark to keep after an arrival scheduled at arrival
// and sent at now: the scheduled time, unless the tenant fell more than
// maxCatchUp operational time behind (a stall), in which case the backlog
// beyond that is dropped. rate must be > 0.
func catchUp(arrival, now time.Time, rate float64) time.Time {
	return laterOf(arrival, now.Add(-seconds(maxCatchUp/rate)))
}

// laterOf returns the later of a and b.
func laterOf(a, b time.Time) time.Time {
	if a.Before(b) {
		return b
	}
	return a
}

// rateAt returns the tenant's wall-clock request rate at the wall time at
// and the next wall time at which it changes on its own (zero when it
// never does or only Spike can change it).
func (l *Live) rateAt(t *liveTenant, at time.Time) (float64, time.Time) {
	p := t.pattern
	if t.spiked {
		l.mu.Lock()
		until := l.spikeUntil
		l.mu.Unlock()
		if at.Before(until) {
			return l.speed * p.PeakRPS, until
		}
		return l.speed * p.BaseRPS, time.Time{}
	}
	period := l.scenario.Duration.Seconds()
	sc := l.speed * at.Sub(l.s.start).Seconds() // scenario seconds since start
	u := math.Mod(sc, period)                   // position in the current cycle
	rate := l.speed * workload.RateAt(p, u)
	next, ok := nextChange(p, u, period)
	if !ok {
		return rate, time.Time{}
	}
	return rate, l.s.start.Add(seconds((sc - u + next) / l.speed))
}

// nextChange returns the first scenario time after u, within the cycle
// [0, period], at which the pattern's rate may change (the cycle restarts
// at period); ok is false when the rate never changes.
func nextChange(p workload.TenantPattern, u, period float64) (next float64, ok bool) {
	switch p.Kind {
	case workload.KindPeriodic:
		m := math.Mod(u-p.PhaseSeconds, p.PeriodSeconds)
		if m < 0 {
			m += p.PeriodSeconds
		}
		next = u + p.PeriodSeconds - m // next burst start
		if m < p.BurstSeconds {
			next = u + p.BurstSeconds - m // end of the current burst
		}
		return math.Min(next, period), true
	case workload.KindSpike:
		next = period
		for _, b := range []float64{p.SpikeStartSeconds, p.SpikeStartSeconds + p.SpikeDurationSeconds} {
			if b > u && b < next {
				next = b
			}
		}
		return next, true
	}
	return 0, false
}

// seconds converts float seconds to a Duration.
func seconds(s float64) time.Duration { return time.Duration(math.Round(s * float64(time.Second))) }
