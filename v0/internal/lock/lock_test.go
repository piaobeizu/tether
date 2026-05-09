package lock

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeClock is a manually-advanced time source for deterministic
// auto-release tests. atomic.Int64 holds unix-nanos; tests adjust via
// add() / set().
type fakeClock struct {
	nanos atomic.Int64
}

func newFakeClock(start time.Time) *fakeClock {
	c := &fakeClock{}
	c.nanos.Store(start.UnixNano())
	return c
}

func (c *fakeClock) Now() time.Time { return time.Unix(0, c.nanos.Load()) }

func (c *fakeClock) Advance(d time.Duration) {
	c.nanos.Add(d.Nanoseconds())
}

// helper: build a fresh Lock with fake clock + 60s window (the spec
// default). Tests that need a different window override.
func newLockForTest(t *testing.T, start time.Time) (*Lock, *fakeClock) {
	t.Helper()
	clk := newFakeClock(start)
	l := New(WithClock(clk.Now))
	return l, clk
}

var (
	mobileA   = ClientID{Kind: KindMobile, DeviceID: "device-a"}
	mobileB   = ClientID{Kind: KindMobile, DeviceID: "device-b"}
	terminalA = ClientID{Kind: KindTerminal, DeviceID: "term-a"}
	zeroC     = ClientID{}
)

// --- ClientID -----------------------------------------------------------

func TestClientID_Equal(t *testing.T) {
	if mobileA.Equal(mobileB) {
		t.Errorf("different DeviceID should not be Equal")
	}
	if !mobileA.Equal(mobileA) {
		t.Errorf("same ClientID should be Equal")
	}
	// Zero is intentionally not equal to itself — see ClientID.Equal doc.
	if zeroC.Equal(zeroC) {
		t.Errorf("zero ClientID must not be Equal to itself (sentinel rule)")
	}
	if zeroC.Equal(mobileA) || mobileA.Equal(zeroC) {
		t.Errorf("zero ClientID must not match a real client")
	}
}

// --- Initial state ------------------------------------------------------

func TestLock_NewIsUnheld(t *testing.T) {
	l := New()
	if h, held := l.Holder(); held {
		t.Errorf("fresh Lock reported holder=%v held=true", h)
	}
	if !l.LastWriteAt().IsZero() {
		t.Errorf("fresh Lock LastWriteAt should be zero")
	}
	if got := l.AutoReleaseWindow(); got != DefaultAutoReleaseWindow {
		t.Errorf("default AutoReleaseWindow: got %v want %v", got, DefaultAutoReleaseWindow)
	}
}

func TestLock_WithAutoReleaseWindow(t *testing.T) {
	l := New(WithAutoReleaseWindow(5 * time.Second))
	if got := l.AutoReleaseWindow(); got != 5*time.Second {
		t.Errorf("override AutoReleaseWindow: got %v want 5s", got)
	}
	// Negative / zero overrides ignored.
	l2 := New(WithAutoReleaseWindow(0))
	if got := l2.AutoReleaseWindow(); got != DefaultAutoReleaseWindow {
		t.Errorf("zero override should be ignored, got %v", got)
	}
}

// --- TryAcquire: the three acquire paths --------------------------------

func TestTryAcquire_FirstByte(t *testing.T) {
	l, _ := newLockForTest(t, time.Date(2026, 5, 6, 0, 0, 0, 0, time.UTC))
	if err := l.TryAcquire(mobileA); err != nil {
		t.Fatalf("TryAcquire on unheld: %v", err)
	}
	h, held := l.Holder()
	if !held || !h.Equal(mobileA) {
		t.Errorf("after first-byte: holder=%v held=%v want mobileA held=true", h, held)
	}
	hist := l.History()
	if len(hist) != 1 {
		t.Fatalf("History len: got %d want 1", len(hist))
	}
	if hist[0].Reason != AcquireFirstByte {
		t.Errorf("History[0].Reason: got %s want %s", hist[0].Reason, AcquireFirstByte)
	}
	if hist[0].NewHolder != mobileA {
		t.Errorf("History[0].NewHolder: got %v want %v", hist[0].NewHolder, mobileA)
	}
	if hist[0].PrevHolder != zeroC {
		t.Errorf("History[0].PrevHolder: got %v want zero", hist[0].PrevHolder)
	}
}

func TestTryAcquire_HolderRepeatNoOp(t *testing.T) {
	l, _ := newLockForTest(t, time.Now())
	_ = l.TryAcquire(mobileA)
	before := l.History()
	if err := l.TryAcquire(mobileA); err != nil {
		t.Fatalf("repeat TryAcquire by holder: %v", err)
	}
	after := l.History()
	if len(after) != len(before) {
		t.Errorf("repeat TryAcquire by holder must not append history (got %d->%d)", len(before), len(after))
	}
}

func TestTryAcquire_ActiveHolderRejects(t *testing.T) {
	l, clk := newLockForTest(t, time.Now())
	_ = l.TryAcquire(mobileA)
	clk.Advance(30 * time.Second) // still within 60s window
	err := l.TryAcquire(mobileB)
	if !errors.Is(err, ErrInUse) {
		t.Fatalf("TryAcquire by other client (active): got %v want ErrInUse", err)
	}
	h, _ := l.Holder()
	if !h.Equal(mobileA) {
		t.Errorf("rejected TryAcquire must not change holder; got %v", h)
	}
	// History must NOT have grown — failed acquires don't audit.
	if len(l.History()) != 1 {
		t.Errorf("rejected TryAcquire should not append history; got %d entries", len(l.History()))
	}
}

func TestTryAcquire_StaleTakeover(t *testing.T) {
	l, clk := newLockForTest(t, time.Now())
	_ = l.TryAcquire(mobileA)
	clk.Advance(61 * time.Second) // past 60s window
	if err := l.TryAcquire(mobileB); err != nil {
		t.Fatalf("stale takeover: %v", err)
	}
	h, _ := l.Holder()
	if !h.Equal(mobileB) {
		t.Errorf("after stale takeover holder=%v want mobileB", h)
	}
	hist := l.History()
	if len(hist) != 2 {
		t.Fatalf("History len: got %d want 2 (acquire + stale-takeover)", len(hist))
	}
	if hist[1].Reason != AcquireStaleTakeover {
		t.Errorf("stale takeover reason: got %s want %s", hist[1].Reason, AcquireStaleTakeover)
	}
	if hist[1].PrevHolder != mobileA || hist[1].NewHolder != mobileB {
		t.Errorf("stale takeover holders: prev=%v new=%v want %v -> %v",
			hist[1].PrevHolder, hist[1].NewHolder, mobileA, mobileB)
	}
}

func TestTryAcquire_ZeroClientRejected(t *testing.T) {
	l := New()
	if err := l.TryAcquire(zeroC); !errors.Is(err, ErrNotHolder) {
		t.Errorf("zero ClientID TryAcquire: got %v want ErrNotHolder", err)
	}
	if _, held := l.Holder(); held {
		t.Errorf("rejected zero TryAcquire should not have granted")
	}
}

// --- ForceTakeover -------------------------------------------------------

func TestForceTakeover_FromUnheld(t *testing.T) {
	l, _ := newLockForTest(t, time.Now())
	if err := l.ForceTakeover(mobileA); err != nil {
		t.Fatalf("ForceTakeover on unheld: %v", err)
	}
	hist := l.History()
	if len(hist) != 1 || hist[0].Reason != AcquireForce {
		t.Errorf("ForceTakeover history: got %+v", hist)
	}
}

func TestForceTakeover_FromActiveHolder(t *testing.T) {
	l, _ := newLockForTest(t, time.Now())
	_ = l.TryAcquire(mobileA)
	if err := l.ForceTakeover(terminalA); err != nil {
		t.Fatalf("force takeover from active holder: %v", err)
	}
	h, _ := l.Holder()
	if !h.Equal(terminalA) {
		t.Errorf("after force takeover holder=%v want terminalA", h)
	}
	hist := l.History()
	if len(hist) != 2 || hist[1].Reason != AcquireForce {
		t.Errorf("force takeover history: got %+v", hist)
	}
	if hist[1].PrevHolder != mobileA {
		t.Errorf("force takeover prevHolder: got %v want mobileA", hist[1].PrevHolder)
	}
}

func TestForceTakeover_BySameHolderSlidesWindow(t *testing.T) {
	l, clk := newLockForTest(t, time.Now())
	_ = l.TryAcquire(mobileA)
	clk.Advance(40 * time.Second)
	beforeLen := len(l.History())
	if err := l.ForceTakeover(mobileA); err != nil {
		t.Fatalf("force by same holder: %v", err)
	}
	if len(l.History()) != beforeLen {
		t.Errorf("force by same holder should not append history; got %d->%d", beforeLen, len(l.History()))
	}
	// Window must have slid: advance 50s more (total 90s past acquire,
	// but only 50s past force) — must still be active.
	clk.Advance(50 * time.Second)
	if err := l.TryAcquire(mobileB); !errors.Is(err, ErrInUse) {
		t.Errorf("force-takeover by same holder must slide window; got %v", err)
	}
}

func TestForceTakeover_ZeroClientRejected(t *testing.T) {
	l := New()
	if err := l.ForceTakeover(zeroC); !errors.Is(err, ErrNotHolder) {
		t.Errorf("zero ForceTakeover: got %v want ErrNotHolder", err)
	}
}

// --- Touch ---------------------------------------------------------------

func TestTouch_HolderSlidesWindow(t *testing.T) {
	l, clk := newLockForTest(t, time.Now())
	_ = l.TryAcquire(mobileA)
	clk.Advance(50 * time.Second)
	if err := l.Touch(mobileA); err != nil {
		t.Fatalf("Touch by holder: %v", err)
	}
	clk.Advance(40 * time.Second) // total 90s past acquire, 40s past Touch
	// Holder should still be mobileA — Touch slid window.
	if _, held := l.Holder(); !held {
		t.Errorf("Touch must slide auto-release window")
	}
}

func TestTouch_NonHolderRejected(t *testing.T) {
	l := New()
	_ = l.TryAcquire(mobileA)
	if err := l.Touch(mobileB); !errors.Is(err, ErrNotHolder) {
		t.Errorf("Touch by non-holder: got %v want ErrNotHolder", err)
	}
	if err := l.Touch(zeroC); !errors.Is(err, ErrNotHolder) {
		t.Errorf("Touch by zero: got %v want ErrNotHolder", err)
	}
}

func TestTouch_OnUnheldRejected(t *testing.T) {
	l := New()
	if err := l.Touch(mobileA); !errors.Is(err, ErrNotHolder) {
		t.Errorf("Touch on unheld: got %v want ErrNotHolder", err)
	}
}

// --- Release -------------------------------------------------------------

func TestRelease_Holder(t *testing.T) {
	l, _ := newLockForTest(t, time.Now())
	_ = l.TryAcquire(mobileA)
	if err := l.Release(mobileA); err != nil {
		t.Fatalf("Release by holder: %v", err)
	}
	if _, held := l.Holder(); held {
		t.Errorf("after Release lock should be unheld")
	}
	hist := l.History()
	if len(hist) != 2 {
		t.Fatalf("History len: got %d want 2", len(hist))
	}
	if hist[1].Reason != ReleaseExplicit {
		t.Errorf("Release reason: got %s want %s", hist[1].Reason, ReleaseExplicit)
	}
	if hist[1].PrevHolder != mobileA || hist[1].NewHolder != zeroC {
		t.Errorf("Release event holders: prev=%v new=%v", hist[1].PrevHolder, hist[1].NewHolder)
	}
}

func TestRelease_NonHolderRejected(t *testing.T) {
	l := New()
	_ = l.TryAcquire(mobileA)
	if err := l.Release(mobileB); !errors.Is(err, ErrNotHolder) {
		t.Errorf("Release by non-holder: got %v want ErrNotHolder", err)
	}
	// State unchanged.
	if h, _ := l.Holder(); !h.Equal(mobileA) {
		t.Errorf("rejected Release must not mutate holder")
	}
}

func TestRelease_OnUnheld(t *testing.T) {
	l := New()
	if err := l.Release(mobileA); !errors.Is(err, ErrNotHolder) {
		t.Errorf("Release on unheld: got %v want ErrNotHolder", err)
	}
}

// --- Sweep / Holder lazy reap --------------------------------------------

func TestSweep_FiresOnStaleHolder(t *testing.T) {
	l, clk := newLockForTest(t, time.Now())
	_ = l.TryAcquire(mobileA)
	if l.Sweep() {
		t.Errorf("Sweep on fresh holder should be no-op")
	}
	clk.Advance(61 * time.Second)
	if !l.Sweep() {
		t.Errorf("Sweep on stale holder should fire")
	}
	if _, held := l.Holder(); held {
		t.Errorf("after Sweep lock should be unheld")
	}
	hist := l.History()
	if len(hist) != 2 || hist[1].Reason != ReleaseAuto {
		t.Errorf("Sweep history: got %+v want last reason=%s", hist, ReleaseAuto)
	}
}

func TestSweep_NoOpWhenUnheld(t *testing.T) {
	l := New()
	if l.Sweep() {
		t.Errorf("Sweep on unheld should be no-op")
	}
}

func TestSweep_Idempotent(t *testing.T) {
	l, clk := newLockForTest(t, time.Now())
	_ = l.TryAcquire(mobileA)
	clk.Advance(61 * time.Second)
	if !l.Sweep() {
		t.Fatalf("first Sweep should fire")
	}
	if l.Sweep() {
		t.Errorf("second Sweep should be no-op (already reaped)")
	}
}

func TestHolder_LazyReapsStale(t *testing.T) {
	// Holder() also reaps — needed so callers checking IsHolder before
	// proceeding don't see a stale "yes" right after the window expires.
	l, clk := newLockForTest(t, time.Now())
	_ = l.TryAcquire(mobileA)
	clk.Advance(61 * time.Second)
	if _, held := l.Holder(); held {
		t.Errorf("Holder() must reap stale holder")
	}
	if l.IsHolder(mobileA) {
		t.Errorf("IsHolder() must reap stale holder")
	}
	// Auto-release event recorded.
	hist := l.History()
	if len(hist) != 2 || hist[1].Reason != ReleaseAuto {
		t.Errorf("lazy reap should append auto-release event; got %+v", hist)
	}
}

// --- LastWriteAt ---------------------------------------------------------

func TestLastWriteAt_TracksTouch(t *testing.T) {
	l, clk := newLockForTest(t, time.Now())
	_ = l.TryAcquire(mobileA)
	t0 := l.LastWriteAt()
	if t0.IsZero() {
		t.Fatalf("LastWriteAt zero after acquire")
	}
	clk.Advance(10 * time.Second)
	if err := l.Touch(mobileA); err != nil {
		t.Fatalf("Touch: %v", err)
	}
	t1 := l.LastWriteAt()
	if !t1.After(t0) {
		t.Errorf("Touch must advance LastWriteAt; got t0=%v t1=%v", t0, t1)
	}
}

func TestLastWriteAt_ZeroWhenUnheld(t *testing.T) {
	l := New()
	if !l.LastWriteAt().IsZero() {
		t.Errorf("unheld LastWriteAt should be zero")
	}
	_ = l.TryAcquire(mobileA)
	_ = l.Release(mobileA)
	if !l.LastWriteAt().IsZero() {
		t.Errorf("after Release LastWriteAt should be zero")
	}
}

// --- History defensiveness -----------------------------------------------

func TestHistory_ReturnsCopy(t *testing.T) {
	l := New()
	_ = l.TryAcquire(mobileA)
	hist := l.History()
	if len(hist) != 1 {
		t.Fatalf("History len: got %d want 1", len(hist))
	}
	hist[0].Reason = "tamper"
	again := l.History()
	if again[0].Reason != AcquireFirstByte {
		t.Errorf("History return must be a copy; mutation leaked: got %s", again[0].Reason)
	}
}

// --- Concurrency --------------------------------------------------------

// TestConcurrentAcquire stresses the contended-acquire path under -race.
// One winner, all losers see ErrInUse or stale-takeover (never panic /
// inconsistent state).
func TestConcurrentAcquire(t *testing.T) {
	l := New()
	const N = 32
	var wg sync.WaitGroup
	wg.Add(N)
	results := make(chan error, N)
	for i := 0; i < N; i++ {
		i := i
		go func() {
			defer wg.Done()
			c := ClientID{Kind: KindMobile, DeviceID: idForN(i)}
			results <- l.TryAcquire(c)
		}()
	}
	wg.Wait()
	close(results)
	wins := 0
	losses := 0
	for err := range results {
		switch {
		case err == nil:
			wins++
		case errors.Is(err, ErrInUse):
			losses++
		default:
			t.Errorf("unexpected error from concurrent TryAcquire: %v", err)
		}
	}
	if wins != 1 {
		t.Errorf("concurrent TryAcquire: got %d winners, want exactly 1", wins)
	}
	if wins+losses != N {
		t.Errorf("counts mismatch: wins=%d losses=%d total=%d", wins, losses, N)
	}
	// State consistent.
	if _, held := l.Holder(); !held {
		t.Errorf("after concurrent TryAcquire lock should be held")
	}
	hist := l.History()
	// Exactly one acquire entry — no spurious extras from contention.
	if len(hist) != 1 {
		t.Errorf("concurrent: history len=%d want 1", len(hist))
	}
}

func idForN(i int) string {
	const hex = "0123456789abcdef"
	return string([]byte{hex[(i>>4)&0xf], hex[i&0xf]})
}

// TestConcurrentForceTakeover validates force-takeover is well-ordered:
// final holder must equal one of the participants and history length =
// 1 (initial acquire) + N (each force-takeover) since each force always
// records.
func TestConcurrentForceTakeover(t *testing.T) {
	l := New()
	_ = l.TryAcquire(mobileA)
	const N = 16
	var wg sync.WaitGroup
	wg.Add(N)
	for i := 0; i < N; i++ {
		i := i
		go func() {
			defer wg.Done()
			c := ClientID{Kind: KindTerminal, DeviceID: idForN(i)}
			_ = l.ForceTakeover(c)
		}()
	}
	wg.Wait()
	hist := l.History()
	// 1 initial acquire + N force takeovers = N+1.
	if len(hist) != N+1 {
		t.Errorf("force-takeover history: got %d want %d", len(hist), N+1)
	}
	// All entries after the first must be AcquireForce.
	for i, ev := range hist[1:] {
		if ev.Reason != AcquireForce {
			t.Errorf("history[%d].Reason: got %s want %s", i+1, ev.Reason, AcquireForce)
		}
	}
}
