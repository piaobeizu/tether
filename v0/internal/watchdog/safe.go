package watchdog

import (
	"context"
	"errors"
	"fmt"
	"runtime/debug"
)

// PanicError wraps a recovered panic in error form so superviseLoop can
// uniformly report it to the operator. Unwrap returns the original
// panic value when it implemented error; otherwise nil.
type PanicError struct {
	Value any
	Stack []byte
}

func (e *PanicError) Error() string {
	return fmt.Sprintf("panic: %v\n%s", e.Value, e.Stack)
}

func (e *PanicError) Unwrap() error {
	if err, ok := e.Value.(error); ok {
		return err
	}
	return nil
}

// IsPanic reports whether err originates from a recovered panic in
// safeRun / GoSafe. Useful in tests / metrics that want to distinguish
// "subsystem returned a normal error" from "subsystem panicked".
func IsPanic(err error) bool {
	var p *PanicError
	return errors.As(err, &p)
}

// safeRun executes fn(ctx) under a deferred recover. A panic inside fn
// is captured as a *PanicError with the goroutine stack trace; an
// ordinary returned error is forwarded as-is. ctx is forwarded
// untouched — fn is responsible for honoring cancellation.
//
// safeRun is the spec §4.4 helper; supervised work always reaches it
// through superviseLoop. It's also re-exported for tests (and any
// future caller that wants the bare panic-recovery primitive without
// the rest of the supervisor).
func safeRun(ctx context.Context, fn func(context.Context) error) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = &PanicError{Value: r, Stack: debug.Stack()}
		}
	}()
	return fn(ctx)
}

// RunWithSafeRun is the exported wrapper around safeRun for callers
// who want panic-as-error semantics outside the supervisor (notably
// tests). Equivalent to:
//
//	defer func() { if r := recover(); r != nil { /* wrap */ } }()
//	return fn(ctx)
//
// Subsystems supervised by Watchdog never need to call this — the
// supervisor wraps every Run automatically.
func RunWithSafeRun(ctx context.Context, fn func(context.Context) error) error {
	return safeRun(ctx, fn)
}

// GoSafe launches fn in a fresh goroutine wrapped in a deferred recover.
// Panics are reported through onPanic (if non-nil) or, if nil, swallowed
// silently — caller is then responsible for surfacing them via a side
// channel (a result channel, an error promise, etc.).
//
// Spec §4.3 mandates that every goroutine the daemon spawns goes
// through this helper — a CI analyzer enforces it. Unsupervised
// `go fn()` is a lint violation.
//
// onPanic runs in the GoSafe goroutine BEFORE the goroutine exits, so
// callers may safely read from any channel onPanic publishes to without
// racing the goroutine teardown.
func GoSafe(fn func(), onPanic func(*PanicError)) {
	go func() {
		defer func() {
			if r := recover(); r != nil {
				if onPanic != nil {
					onPanic(&PanicError{Value: r, Stack: debug.Stack()})
				}
			}
		}()
		fn()
	}()
}
