package watchdog_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/piaobeizu/tether/internal/watchdog"
)

// TestSafeRun_ErrorPassthrough — non-panicking error returns are
// forwarded as-is.
func TestSafeRun_ErrorPassthrough(t *testing.T) {
	t.Parallel()

	want := errors.New("normal error")
	got := watchdog.RunWithSafeRun(context.Background(), func(ctx context.Context) error {
		return want
	})
	if !errors.Is(got, want) {
		t.Errorf("expected %v, got %v", want, got)
	}
	if watchdog.IsPanic(got) {
		t.Error("normal error must not be classified as panic")
	}
}

// TestSafeRun_NilError — nil through nil.
func TestSafeRun_NilError(t *testing.T) {
	t.Parallel()

	got := watchdog.RunWithSafeRun(context.Background(), func(ctx context.Context) error {
		return nil
	})
	if got != nil {
		t.Errorf("expected nil, got %v", got)
	}
}

// TestSafeRun_PanicCaught — panics surface as PanicError with a stack.
func TestSafeRun_PanicCaught(t *testing.T) {
	t.Parallel()

	got := watchdog.RunWithSafeRun(context.Background(), func(ctx context.Context) error {
		panic("boom")
	})
	if got == nil {
		t.Fatal("expected PanicError, got nil")
	}
	if !watchdog.IsPanic(got) {
		t.Errorf("IsPanic = false; want true; err = %v", got)
	}
	if !strings.Contains(got.Error(), "boom") {
		t.Errorf("PanicError should embed the panic value; got %q", got.Error())
	}
	if !strings.Contains(got.Error(), "goroutine") {
		t.Errorf("PanicError should embed a stack trace; got %q", got.Error())
	}
}

// TestSafeRun_PanicError_UnwrapForErrorPanic — panic(err) lets
// errors.Is reach the wrapped error.
func TestSafeRun_PanicError_UnwrapForErrorPanic(t *testing.T) {
	t.Parallel()

	sentinel := errors.New("sentinel panic value")
	got := watchdog.RunWithSafeRun(context.Background(), func(ctx context.Context) error {
		panic(sentinel)
	})
	if !errors.Is(got, sentinel) {
		t.Errorf("errors.Is(got, sentinel) = false; want true; err = %v", got)
	}
}

// TestGoSafe_PanicReportedToCallback — onPanic gets the captured info.
func TestGoSafe_PanicReportedToCallback(t *testing.T) {
	t.Parallel()

	var (
		mu  sync.Mutex
		got *watchdog.PanicError
	)
	done := make(chan struct{})
	watchdog.GoSafe(func() {
		panic("from gosafe")
	}, func(pe *watchdog.PanicError) {
		mu.Lock()
		got = pe
		mu.Unlock()
		close(done)
	})

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("onPanic callback never fired")
	}

	mu.Lock()
	defer mu.Unlock()
	if got == nil {
		t.Fatal("onPanic received nil")
	}
	if !strings.Contains(got.Error(), "from gosafe") {
		t.Errorf("PanicError missing payload: %q", got.Error())
	}
}

// TestGoSafe_NormalPathNoCallback — normal exit does NOT invoke onPanic.
func TestGoSafe_NormalPathNoCallback(t *testing.T) {
	t.Parallel()

	called := make(chan struct{}, 1)
	done := make(chan struct{})
	watchdog.GoSafe(func() {
		close(done)
	}, func(_ *watchdog.PanicError) {
		called <- struct{}{}
	})
	<-done
	select {
	case <-called:
		t.Error("onPanic must not fire on normal goroutine exit")
	case <-time.After(50 * time.Millisecond):
	}
}

// TestGoSafe_NilCallbackSwallowsPanic — passing nil is allowed.
func TestGoSafe_NilCallbackSwallowsPanic(t *testing.T) {
	t.Parallel()

	// We can't observe the recover directly, but we can confirm the
	// goroutine doesn't crash the test process.
	done := make(chan struct{})
	watchdog.GoSafe(func() {
		defer close(done)
		panic("nil-onPanic")
	}, nil)
	select {
	case <-done:
		// goroutine ran far enough to defer close — recover succeeded.
	case <-time.After(time.Second):
		t.Fatal("goroutine did not run to defer close")
	}
}

// TestBackoff_Schedule — verifies the documented sequence.
func TestBackoff_Schedule(t *testing.T) {
	t.Parallel()

	b := watchdog.NewBackoff(100*time.Millisecond, 1*time.Second, 2)
	want := []time.Duration{
		100 * time.Millisecond,
		200 * time.Millisecond,
		400 * time.Millisecond,
		800 * time.Millisecond,
		1 * time.Second, // capped
		1 * time.Second, // capped
	}
	for i, w := range want {
		got := b.Next()
		if got != w {
			t.Errorf("Next %d = %v; want %v", i, got, w)
		}
	}
	b.Reset()
	if got := b.Next(); got != 100*time.Millisecond {
		t.Errorf("after Reset, Next = %v; want 100ms", got)
	}
}

// TestBackoff_DefaultsForZero — zero values fall back to documented
// defaults.
func TestBackoff_DefaultsForZero(t *testing.T) {
	t.Parallel()

	b := watchdog.NewBackoff(0, 0, 0)
	first := b.Next()
	if first != 200*time.Millisecond {
		t.Errorf("default initial = %v; want 200ms", first)
	}
	// Confirm Max is 10s by stepping until plateau.
	last := first
	for range 20 {
		last = b.Next()
	}
	if last != 10*time.Second {
		t.Errorf("eventually capped at %v; want 10s", last)
	}
}
