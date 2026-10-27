// Package stabilize keeps the pool size from flapping. It applies the same
// rule as the Kubernetes HorizontalPodAutoscaler: scale up as soon as more
// replicas are wanted, but scale down only to the largest value wanted
// during a trailing window, and never move more than MaxStep replicas per
// call. Like the HPA, it counts the starting replica count as wanted when it
// first runs, so a (re)started controller does not scale down within its
// first window. It is clock-agnostic: callers pass the current time.
package stabilize

import (
	"slices"
	"time"
)

// Config tunes a Stabilizer.
type Config struct {
	// ScaleDownWindow is how far back Next looks when scaling down: it only
	// scales down to the maximum desired value seen within this window.
	// Zero scales down immediately; a negative value is treated as zero.
	ScaleDownWindow time.Duration
	// MaxStep is the largest change per call, in either direction. Values
	// below 1 are treated as 1.
	MaxStep int
	// Min and Max bound every value Next returns. If Max < Min, Min wins.
	Min, Max int
}

// Sample is one desired value passed to Next, with the time it was seen.
type Sample struct {
	Time    time.Time `json:"time"`
	Desired int       `json:"desired"`
}

// State is everything a Stabilizer remembers, for persistence across
// controller restarts.
type State struct {
	// Current is the last value Next returned (or the initial value).
	Current int `json:"current"`
	// History holds the desired values inside the scale-down window as of
	// the last call to Next, including that call's own sample and, while it
	// is in the window, the starting value seeded by the first call (see
	// Next). A sample counts while its time is strictly after
	// now - ScaleDownWindow. It is empty only before the first call.
	History []Sample `json:"history"`
}

// Stabilizer turns a noisy stream of desired replica counts into a stable
// one. It is not safe for concurrent use.
type Stabilizer struct {
	cfg     Config
	current int
	history []Sample
}

// New returns a Stabilizer whose current value is initial (the replica count
// running now). Initial may lie outside [Min, Max]; the first call to Next
// brings it into range. The first call also records initial as wanted at
// that call's time, so Next does not scale below initial (clamped to
// [Min, Max]) until one ScaleDownWindow has passed.
func New(cfg Config, initial int) *Stabilizer {
	return &Stabilizer{cfg: normalize(cfg), current: initial}
}

// Restore rebuilds a Stabilizer from a State saved by State, possibly under
// a new Config. Samples outside the window are pruned on the next call, so
// the saved history may be in any order; samples dated after that call are
// treated as seen at its time (see Next).
//
// A State with an empty History (saved before the first call, or built by
// hand from a live replica count) behaves like New(cfg, st.Current): scale-
// down is held for one window from the first call. A non-empty History
// continues as if the stabilizer had never stopped, so samples that aged out
// of the window while it was down no longer hold the pool.
func Restore(cfg Config, st State) *Stabilizer {
	return &Stabilizer{cfg: normalize(cfg), current: st.Current, history: slices.Clone(st.History)}
}

// State returns a copy of the stabilizer's memory.
func (s *Stabilizer) State() State {
	return State{Current: s.current, History: slices.Clone(s.history)}
}

// Next records desired at time now and returns the replica count to apply:
//
//   - up: if desired exceeds the current value, move toward desired at once;
//   - down: otherwise move toward the largest desired value seen within
//     ScaleDownWindow (including this one), so a brief dip does not shrink
//     the pool;
//   - each call moves at most MaxStep replicas;
//   - the result is always clamped to [Min, Max]. If the current value lies
//     outside that range (after New or Restore), the clamp may move further
//     than MaxStep to get back in range.
//
// The first call (empty history) seeds the window with the current value at
// now before recording desired, as the HPA seeds its recommendations with
// the current replica count. One transiently low first sample then cannot
// shrink the pool; scale-down waits until the seed leaves the window. With a
// zero window the seed is pruned at once and has no effect.
//
// A remembered sample dated after now (the clock stepped back, or the State
// was saved on a host whose clock ran ahead) is re-dated to now, so it holds
// scale-down for one window rather than until the clock catches up with it.
func (s *Stabilizer) Next(now time.Time, desired int) int {
	if len(s.history) == 0 {
		// Every call leaves its own sample behind, so an empty history means
		// no call since New, or since Restore of a State without history.
		s.history = append(s.history, Sample{Time: now, Desired: s.current})
	}
	for i := range s.history {
		if s.history[i].Time.After(now) {
			s.history[i].Time = now
		}
	}
	cutoff := now.Add(-s.cfg.ScaleDownWindow)
	s.history = slices.DeleteFunc(s.history, func(x Sample) bool { return !x.Time.After(cutoff) })
	s.history = append(s.history, Sample{Time: now, Desired: desired})
	windowMax := desired
	for _, x := range s.history {
		windowMax = max(windowMax, x.Desired)
	}
	target := s.current
	switch {
	case desired > s.current:
		target = desired
	case windowMax < s.current:
		target = windowMax
	}
	target = s.clamp(target)
	step := max(-s.cfg.MaxStep, min(s.cfg.MaxStep, target-s.current))
	s.current = s.clamp(s.current + step)
	return s.current
}

func (s *Stabilizer) clamp(n int) int {
	return max(s.cfg.Min, min(s.cfg.Max, n))
}

func normalize(cfg Config) Config {
	cfg.ScaleDownWindow = max(cfg.ScaleDownWindow, 0)
	cfg.MaxStep = max(cfg.MaxStep, 1)
	return cfg
}
