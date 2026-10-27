package demo

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"tenantscaler/internal/sim"
)

func TestBeyondCancel(t *testing.T) {
	closeErr := errors.New("pool close: kill llama-server 4242: access is denied")
	waitCanceled := fmt.Errorf("stack S2-keda-queue: waiting for 2 ready replicas: %w", context.Canceled)
	cases := []struct {
		name string
		err  error
		want error // nil: only the cancellation
		// whole: err is returned as is (a single wrapper cannot be split,
		// so its cancellation stays in the message).
		whole bool
	}{
		{"nil", nil, nil, false},
		{"canceled", context.Canceled, nil, false},
		{"wrapped cancel", waitCanceled, nil, false},
		{"joined cancel and nil", errors.Join(context.Canceled, nil), nil, false},
		{"start canceled, cleanup failed", errors.Join(waitCanceled, closeErr), closeErr, false},
		{"stop failed after cancel", errors.Join(context.Canceled, fmt.Errorf("stop S7-ts-full: %w", closeErr)), closeErr, false},
		{"wrapped join", fmt.Errorf("stack: %w", errors.Join(context.Canceled, closeErr)), closeErr, true},
		{"shutdown timeout", fmt.Errorf("gateway shutdown: %w", context.DeadlineExceeded), context.DeadlineExceeded, true},
		{"plain error", closeErr, closeErr, true},
	}
	for _, c := range cases {
		got := beyondCancel(c.err)
		switch {
		case c.want == nil && got != nil:
			t.Errorf("%s: %v, want nil", c.name, got)
		case c.want != nil && !errors.Is(got, c.want):
			t.Errorf("%s: %v, want it to report %v", c.name, got, c.want)
		case c.whole && got != c.err:
			t.Errorf("%s: %v, want the error itself", c.name, got)
		case c.want != nil && !c.whole && errors.Is(got, context.Canceled):
			t.Errorf("%s: %v still reports the cancellation", c.name, got)
		}
	}
}

// TestStoppedRunReportsShutdownError checks that a stop whose stack
// shutdown failed is reported as such, never as "every replica was shut
// down", and that a clean stop still says so.
func TestStoppedRunReportsShutdownError(t *testing.T) {
	opt, err := testOptions(t).normalize()
	if err != nil {
		t.Fatal(err)
	}
	stopped, cancel := context.WithCancel(t.Context())
	cancel()
	closeErr := errors.New("pool close: kill llama-server 4242: access is denied")

	for _, simulated := range []bool{true, false} {
		o := opt
		o.Backend.Simulated = simulated
		r := newRun(1, 7, &o, sim.Roles{}, func() {})
		got := r.fail(stopped, 0, errors.Join(context.Canceled, fmt.Errorf("stop %s: %w", sim.SetupKEDAQueue, closeErr)))
		var se *stoppedError
		if !errors.As(got, &se) || !errors.Is(got, context.Canceled) || !errors.Is(got, closeErr) {
			t.Fatalf("fail: %v, want a stoppedError carrying the shutdown error", got)
		}
		r.end(got, o.Logger)
		if r.phase != PhaseStopped || !strings.Contains(r.err, "access is denied") || strings.Contains(r.message, "every replica") {
			t.Errorf("simulated %v: phase %q error %q message %q", simulated, r.phase, r.err, r.message)
		}
		if strings.Contains(r.message, "tenantscaler cleanup") == simulated {
			t.Errorf("simulated %v: message %q (the cleanup hint is for real replicas only)", simulated, r.message)
		}
		if s := r.stacks[0]; s.status != StatusStopped || !strings.Contains(s.err, "stop "+sim.SetupKEDAQueue) {
			t.Errorf("stack 0: %q %q", s.status, s.err)
		}
		if r.stacks[1].status != StatusSkipped {
			t.Errorf("stack 1: %q, want skipped", r.stacks[1].status)
		}
	}

	r := newRun(2, 7, &opt, sim.Roles{}, func() {})
	err = r.fail(stopped, 1, errors.Join(context.Canceled, nil))
	if se := new(*stoppedError); !errors.Is(err, context.Canceled) || errors.As(err, se) {
		t.Fatalf("clean stop: %v, want context.Canceled alone", err)
	}
	r.end(err, opt.Logger)
	if r.phase != PhaseStopped || r.err != "" || r.stacks[1].err != "" || !strings.Contains(r.message, "every replica") {
		t.Errorf("clean stop: phase %q error %q stack error %q message %q", r.phase, r.err, r.stacks[1].err, r.message)
	}
}
