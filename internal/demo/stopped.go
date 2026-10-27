package demo

import (
	"context"
	"errors"
)

// stoppedError ends a run that was stopped while shutting its stack down
// reported an error, so replicas may have survived. It matches
// context.Canceled.
type stoppedError struct {
	title string // the stack's title
	cause error  // what the shutdown reported
}

// Error says which stack failed to shut down and why.
func (e *stoppedError) Error() string { return e.title + ": " + e.cause.Error() }

// Unwrap returns the cancellation and the shutdown error.
func (e *stoppedError) Unwrap() []error { return []error{context.Canceled, e.cause} }

// beyondCancel returns what err reports besides the run's cancellation:
// nil when err is nil or only wraps context.Canceled, otherwise the parts
// of a joined error that are not cancellations, or err itself when it
// cannot be split.
func beyondCancel(err error) error {
	if err == nil || err == context.Canceled { // the sentinel itself; wrappers are walked below
		return nil
	}
	switch u := err.(type) { // one wrapping level at a time, unlike errors.Is
	case interface{ Unwrap() []error }:
		var rest []error
		for _, e := range u.Unwrap() {
			if r := beyondCancel(e); r != nil {
				rest = append(rest, r)
			}
		}
		return errors.Join(rest...)
	case interface{ Unwrap() error }:
		if beyondCancel(u.Unwrap()) == nil {
			return nil
		}
	}
	return err
}
