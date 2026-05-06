package lock

import (
	"bufio"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// helper: read all JSONL lines from path.
func readJSONL(t *testing.T, path string) []TakeoverEvent {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open audit log %q: %v", path, err)
	}
	defer func() { _ = f.Close() }()
	var out []TakeoverEvent
	sc := bufio.NewScanner(f)
	// Each row is well under default 64 KB but pre-grow to be safe.
	sc.Buffer(make([]byte, 0, 4096), 1<<20)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			t.Fatalf("blank line in JSONL audit log — torn write?")
		}
		ev, err := DecodeTakeoverEvent(line)
		if err != nil {
			t.Fatalf("decode JSONL line %q: %v", line, err)
		}
		out = append(out, ev)
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("scan: %v", err)
	}
	return out
}

func TestJSONLLogSink_AppendBasic(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "users", "default", "sessions", "sess-x", "lock.log")
	sink, err := NewJSONLLogSink(path)
	if err != nil {
		t.Fatalf("NewJSONLLogSink: %v", err)
	}
	t.Cleanup(func() { _ = sink.Close() })

	const N = 10
	base := time.Date(2026, 5, 6, 1, 2, 3, 0, time.UTC)
	for i := 0; i < N; i++ {
		ev := TakeoverEvent{
			At:         base.Add(time.Duration(i) * time.Millisecond),
			Reason:     AcquireFirstByte,
			PrevHolder: ClientID{},
			NewHolder:  ClientID{Kind: KindMobile, DeviceID: "d-" + string(rune('a'+i))},
		}
		if err := sink.Append(ev); err != nil {
			t.Fatalf("Append #%d: %v", i, err)
		}
	}
	got := readJSONL(t, path)
	if len(got) != N {
		t.Fatalf("audit log line count: got %d want %d", len(got), N)
	}
	// Timestamps monotonic (non-decreasing).
	for i := 1; i < len(got); i++ {
		if got[i].At.Before(got[i-1].At) {
			t.Errorf("non-monotonic timestamps at %d: %v < %v", i, got[i].At, got[i-1].At)
		}
	}
	// Round-trip: parsed events should decode to the same shape.
	for i, ev := range got {
		if ev.Reason != AcquireFirstByte {
			t.Errorf("ev[%d].Reason: got %s want %s", i, ev.Reason, AcquireFirstByte)
		}
		if ev.NewHolder.Kind != KindMobile {
			t.Errorf("ev[%d].NewHolder.Kind: got %q want %q", i, ev.NewHolder.Kind, KindMobile)
		}
		if !ev.PrevHolder.Equal(ClientID{}) && (ev.PrevHolder.Kind != "" || ev.PrevHolder.DeviceID != "") {
			t.Errorf("ev[%d].PrevHolder should be zero ClientID: %v", i, ev.PrevHolder)
		}
	}
}

func TestJSONLLogSink_FileModeAndDirs(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "users", "default", "sessions", "sess-y", "lock.log")
	sink, err := NewJSONLLogSink(path)
	if err != nil {
		t.Fatalf("NewJSONLLogSink: %v", err)
	}
	t.Cleanup(func() { _ = sink.Close() })

	// File mode 0600.
	st, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %q: %v", path, err)
	}
	if mode := st.Mode().Perm(); mode != 0o600 {
		t.Errorf("file mode: got %o want 0600", mode)
	}
	// Parent dirs 0700.
	for _, d := range []string{
		filepath.Dir(path),                  // sessions/sess-y
		filepath.Dir(filepath.Dir(path)),    // sessions
		filepath.Dir(filepath.Dir(filepath.Dir(path))), // users/default
	} {
		ds, err := os.Stat(d)
		if err != nil {
			t.Fatalf("stat dir %q: %v", d, err)
		}
		if mode := ds.Mode().Perm(); mode != 0o700 {
			t.Errorf("dir %q mode: got %o want 0700", d, mode)
		}
	}
}

func TestJSONLLogSink_AppendOnly(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "lock.log")
	// Pre-seed file with a "previous run" line.
	if err := os.WriteFile(path, []byte(`{"prev":"line"}`+"\n"), 0o600); err != nil {
		t.Fatalf("seed file: %v", err)
	}
	sink, err := NewJSONLLogSink(path)
	if err != nil {
		t.Fatalf("NewJSONLLogSink: %v", err)
	}
	t.Cleanup(func() { _ = sink.Close() })
	if err := sink.Append(TakeoverEvent{
		At:        time.Now().UTC(),
		Reason:    ReleaseExplicit,
		NewHolder: ClientID{},
	}); err != nil {
		t.Fatalf("Append: %v", err)
	}
	// First line must be the seed (no truncation).
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("readfile: %v", err)
	}
	if want := `{"prev":"line"}`; string(b[:len(want)]) != want {
		t.Errorf("seed line truncated; got prefix %q want %q", string(b[:len(want)]), want)
	}
}

func TestJSONLLogSink_ConcurrentAppendNoTearing(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "lock.log")
	sink, err := NewJSONLLogSink(path)
	if err != nil {
		t.Fatalf("NewJSONLLogSink: %v", err)
	}
	t.Cleanup(func() { _ = sink.Close() })

	const Workers = 16
	const PerWorker = 32
	var wg sync.WaitGroup
	wg.Add(Workers)
	var failures atomic.Int64
	base := time.Date(2026, 5, 6, 0, 0, 0, 0, time.UTC)
	for w := 0; w < Workers; w++ {
		w := w
		go func() {
			defer wg.Done()
			for i := 0; i < PerWorker; i++ {
				ev := TakeoverEvent{
					At:         base.Add(time.Duration(w*PerWorker+i) * time.Microsecond),
					Reason:     AcquireForce,
					PrevHolder: ClientID{},
					NewHolder:  ClientID{Kind: KindTerminal, DeviceID: "w-" + idForN(w)},
				}
				if err := sink.Append(ev); err != nil {
					failures.Add(1)
					t.Errorf("Append: %v", err)
				}
			}
		}()
	}
	wg.Wait()
	if failures.Load() != 0 {
		t.Fatalf("had %d Append failures", failures.Load())
	}
	got := readJSONL(t, path)
	if want := Workers * PerWorker; len(got) != want {
		t.Fatalf("line count: got %d want %d (concurrent torn writes?)", len(got), want)
	}
}

func TestJSONLLogSink_CloseRejectsAppend(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "lock.log")
	sink, err := NewJSONLLogSink(path)
	if err != nil {
		t.Fatalf("NewJSONLLogSink: %v", err)
	}
	if err := sink.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := sink.Close(); err != nil {
		t.Errorf("second Close should be idempotent, got %v", err)
	}
	err = sink.Append(TakeoverEvent{At: time.Now()})
	if err == nil {
		t.Errorf("Append on closed sink: got nil want error")
	}
}

func TestJSONLLogSink_EmptyPathRejected(t *testing.T) {
	if _, err := NewJSONLLogSink(""); err == nil {
		t.Errorf("empty path: got nil want error")
	}
}

// recordingSink is a test fake LogSink implementation — used to drive
// the Lock-side wiring tests without touching the filesystem.
type recordingSink struct {
	mu      sync.Mutex
	events  []TakeoverEvent
	failOn  int // index >=0 → return ErrSink on the Nth (0-based) Append
	calls   atomic.Int64
}

var errSinkInjected = errors.New("recordingSink: injected error")

func (r *recordingSink) Append(ev TakeoverEvent) error {
	n := int(r.calls.Add(1)) - 1
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.failOn >= 0 && r.failOn == n {
		return errSinkInjected
	}
	r.events = append(r.events, ev)
	return nil
}

func (r *recordingSink) snapshot() []TakeoverEvent {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]TakeoverEvent, len(r.events))
	copy(out, r.events)
	return out
}

func TestLock_WithLogSink_ReceivesAllStateChanges(t *testing.T) {
	rec := &recordingSink{failOn: -1}
	clk := newFakeClock(time.Date(2026, 5, 6, 0, 0, 0, 0, time.UTC))
	l := New(WithClock(clk.Now), WithLogSink(rec))

	if err := l.TryAcquire(mobileA); err != nil {
		t.Fatalf("TryAcquire: %v", err)
	}
	if err := l.ForceTakeover(terminalA); err != nil {
		t.Fatalf("ForceTakeover: %v", err)
	}
	if err := l.Release(terminalA); err != nil {
		t.Fatalf("Release: %v", err)
	}

	// Now drive an auto-release path.
	_ = l.TryAcquire(mobileB)
	clk.Advance(61 * time.Second)
	if !l.Sweep() {
		t.Fatalf("Sweep should have fired")
	}

	got := rec.snapshot()
	wantReasons := []AcquireReason{
		AcquireFirstByte,
		AcquireForce,
		ReleaseExplicit,
		AcquireFirstByte, // mobileB took over (was unheld after Release)
		ReleaseAuto,
	}
	if len(got) != len(wantReasons) {
		t.Fatalf("sink received %d events, want %d (%v)", len(got), len(wantReasons), got)
	}
	for i, want := range wantReasons {
		if got[i].Reason != want {
			t.Errorf("event[%d].Reason: got %s want %s", i, got[i].Reason, want)
		}
	}
	// The lock's own History must match (sink is a side channel,
	// not a replacement).
	if hist := l.History(); len(hist) != len(got) {
		t.Errorf("History/sink length mismatch: %d vs %d", len(hist), len(got))
	}
}

func TestLock_WithLogSink_ErrorRoutedToHandler(t *testing.T) {
	rec := &recordingSink{failOn: 0}
	var sinkErr error
	var sinkErrMu sync.Mutex
	l := New(WithLogSink(rec), WithSinkErrorHandler(func(err error) {
		sinkErrMu.Lock()
		defer sinkErrMu.Unlock()
		sinkErr = err
	}))
	if err := l.TryAcquire(mobileA); err != nil {
		t.Fatalf("TryAcquire (sink errors must NOT block state machine): %v", err)
	}
	sinkErrMu.Lock()
	defer sinkErrMu.Unlock()
	if !errors.Is(sinkErr, errSinkInjected) {
		t.Errorf("sink-error handler: got %v want %v", sinkErr, errSinkInjected)
	}
	// State machine still progressed.
	if h, held := l.Holder(); !held || !h.Equal(mobileA) {
		t.Errorf("sink error must not roll back transition; got holder=%v held=%v", h, held)
	}
}

func TestLock_NilSinkDefault(t *testing.T) {
	// Back-compat: lock.New() with no WithLogSink must not panic on
	// state changes (sink == nil branch).
	l := New()
	if err := l.TryAcquire(mobileA); err != nil {
		t.Fatalf("nil sink TryAcquire: %v", err)
	}
	if err := l.Release(mobileA); err != nil {
		t.Fatalf("nil sink Release: %v", err)
	}
}

// TestLock_WithJSONLSink_EndToEnd is the on-disk integration test:
// fire N events through a real Lock + JSONLLogSink and assert the
// file contains exactly N parseable lines with monotonic timestamps.
func TestLock_WithJSONLSink_EndToEnd(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "users", "default", "sessions", "sess-z", "lock.log")
	sink, err := NewJSONLLogSink(path)
	if err != nil {
		t.Fatalf("NewJSONLLogSink: %v", err)
	}
	defer func() { _ = sink.Close() }()

	clk := newFakeClock(time.Date(2026, 5, 6, 0, 0, 0, 0, time.UTC))
	l := New(WithClock(clk.Now), WithLogSink(sink))

	const N = 8
	for i := 0; i < N; i++ {
		clk.Advance(time.Millisecond)
		c := ClientID{Kind: KindTerminal, DeviceID: "term-" + idForN(i)}
		if err := l.ForceTakeover(c); err != nil {
			t.Fatalf("ForceTakeover #%d: %v", i, err)
		}
	}
	got := readJSONL(t, path)
	if len(got) != N {
		t.Fatalf("audit log lines: got %d want %d", len(got), N)
	}
	// Monotonic timestamps.
	for i := 1; i < len(got); i++ {
		if got[i].At.Before(got[i-1].At) {
			t.Errorf("non-monotonic at i=%d: %v < %v", i, got[i].At, got[i-1].At)
		}
	}
	// Each entry parses as TakeoverEvent with reason=force.
	for i, ev := range got {
		if ev.Reason != AcquireForce {
			t.Errorf("event[%d].Reason: got %s want %s", i, ev.Reason, AcquireForce)
		}
	}
}
