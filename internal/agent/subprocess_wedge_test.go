package agent

// Wedge tests (tether#62) — the two ways readLoop and Close can deadlock against
// each other, and the guards that break them.
//
// WHY SHELL SHIMS HERE, WHEN fakecc_test.go DELIBERATELY REPLACED SHELL
//
// tether#53 replaced this package's old `#!/bin/sh` stand-in with a Go fake
// re-executed out of the test binary, because what those tests needed was
// PROTOCOL fidelity: a real argv, real stream-json, cc's measured line shapes.
// These tests need the opposite thing — PROCESS fidelity. The behaviours under
// test are "fork a child that inherits fd 1 and outlive it" and "wedge on a
// stdout pipe nobody is draining", which are properties of fork/exec and pipe
// buffers, not of stream-json. A shell script expresses both in three lines and
// with no ambiguity about which process holds which descriptor; expressing them
// through the Go fake would mean adding process-topology modes to a file whose
// stated purpose is protocol fidelity. So: shell here, Go fake there, and the
// stream-json content below is kept to the single line each case needs.

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// writeShim writes body as an executable /bin/sh script in dir and returns its
// path, for use as SpawnConfig's agent binary.
func writeShim(t *testing.T, dir, name, body string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(body), 0o700); err != nil {
		t.Fatalf("write shim %s: %v", name, err)
	}
	return path
}

// reapOrphan kills the pid recorded in pidFile at test end. Best-effort by
// design: the point is not to assert on the orphan but to keep a failing run
// from leaving a `sleep` behind.
//
// The identity check is not ceremony. A pid read from a file is stale the moment
// it is written, and by cleanup time the kernel may have recycled it onto
// something else on a busy shared machine — so this signals nothing it has not
// first confirmed is still the `sleep` the shim started. If /proc is
// unreadable the kill is skipped entirely: the orphan exits on its own well
// inside the test's own lifetime, so doing nothing is always the safe answer.
func reapOrphan(t *testing.T, pidFile string) {
	t.Helper()
	t.Cleanup(func() {
		raw, err := os.ReadFile(pidFile)
		if err != nil {
			return
		}
		pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
		if err != nil || pid <= 0 {
			return
		}
		comm, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "comm"))
		if err != nil || strings.TrimSpace(string(comm)) != "sleep" {
			return
		}
		if p, err := os.FindProcess(pid); err == nil {
			_ = p.Kill()
		}
	})
}

// eventsClosed waits for sess.Events() to close, returning every event seen and
// whether it closed inside the budget.
//
// collectUntilClosed (claude_provider_test.go) does the same walk but calls
// t.Fatalf itself on timeout, with one message for every caller. Here the
// timeout IS the bug — "this channel never closes" is the whole of tether#62 —
// and each call site has a different wedge to name, so the verdict is returned
// rather than decided, and the diagnosis is written at the call site.
func eventsClosed(sess Session, budget time.Duration) (evs []Event, closed bool) {
	deadline := time.After(budget)
	for {
		select {
		case ev, ok := <-sess.Events():
			if !ok {
				return evs, true
			}
			evs = append(evs, ev)
		case <-deadline:
			return evs, false
		}
	}
}

// closedWithin reports whether f returned inside the budget. f runs on its own
// goroutine and is abandoned (not cancelled) on timeout, so a Close that never
// returns fails the test instead of hanging the suite.
func closedWithin(f func() error, budget time.Duration) (error, bool) {
	res := make(chan error, 1)
	go func() { res <- f() }()
	select {
	case err := <-res:
		return err, true
	case <-time.After(budget):
		return nil, false
	}
}

// shimOrphanHoldsStdout: emit one system/init, hand fd 1 to a child that
// outlives us, then exit. This is the topology the wi calls path 1 — the parent
// agent is gone, but its stdout pipe never reaches EOF because a descriptor for
// the write end is still open somewhere else.
const shimOrphanHoldsStdout = `#!/bin/sh
sleep 30 &
echo $! > "$TETHER_TEST_PIDFILE"
echo '{"type":"system","subtype":"init","session_id":"orphan-sid"}'
exit 0
`

// TestSpawn_OrphanHoldingStdoutStillEndsTheSession — with a descriptor for cc's
// stdout write end held by a process that outlives cc, scanner.Scan() never sees
// EOF, so before tether#62 readLoop parked forever and took the whole teardown
// chain with it: `done` never closed (Alive() kept answering true, so the sid
// still read as a live session on reconnect), `events` never closed (so
// Registry.fanOut never returned), and fanOut's deferred teardown — the only
// thing that evicts the entry and reaps the process — never ran. Cancelling the
// context did not help, which is what made it permanent for the life of the
// daemon.
//
// Asserted here at the agent boundary: after the Spawn context is cancelled, the
// session must actually END — Events() closes, Alive() goes false, and Close()
// returns.
//
// NOT asserted: that the EventError abandon emits arrives. On this path the
// context is already done, so emit's select has both arms ready and Go picks
// between them at random. Asserting it would be a flake.
func TestSpawn_OrphanHoldingStdoutStillEndsTheSession(t *testing.T) {
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "orphan.pid")
	shim := writeShim(t, dir, "cc-orphan.sh", shimOrphanHoldsStdout)
	reapOrphan(t, pidFile)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	p := NewClaudeCodeProvider(shim, withStdoutGrace(300*time.Millisecond))
	sess, err := p.Spawn(ctx, SpawnConfig{
		Workdir: dir,
		Env:     []string{"TETHER_TEST_PIDFILE=" + pidFile},
	})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}

	// The init line proves the shim ran and readLoop is scanning its stdout, so a
	// later timeout is the wedge and not a shim that never started.
	select {
	case ev, ok := <-sess.Events():
		if !ok {
			t.Fatal("Events() closed before the init event")
		}
		if ev.Kind != EventInit {
			t.Fatalf("first event = %v, want %v", ev.Kind, EventInit)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("never saw the shim's init event")
	}

	cancel() // what a client disconnect does

	if _, closed := eventsClosed(sess, 10*time.Second); !closed {
		t.Fatal("Events() never closed after the context was cancelled: readLoop is still " +
			"parked on a stdout pipe an orphan holds open, so fanOut can neither evict nor reap")
	}
	if sess.Alive() {
		t.Error("Alive() = true after Events() closed; false must be reachable on this path")
	}
	if _, returned := closedWithin(sess.Close, 10*time.Second); !returned {
		t.Error("Close() did not return: the process was never reaped")
	}
}

// shimOversizedLine: emit one line far longer than the scanner's cap and NEVER
// terminate it, then stay alive holding fd 1. The reader trips
// bufio.ErrTooLong and stops; this process then fills the pipe buffer and wedges
// in write(). 256 KiB is comfortably more than one pipe buffer, so the wedge is
// not dependent on the exact buffer size.
const shimOversizedLine = `#!/bin/sh
i=0
while [ $i -lt 256 ]; do
  printf '%1024s' '' | tr ' ' 'a'
  i=$((i+1))
done
sleep 30
`

// TestSpawn_OversizedLineDoesNotWedgeTheReap — the wi's path 3, and the one
// failure the 100MB per-line cap can actually produce. readLoop stops with
// bufio.ErrTooLong while cc is still ALIVE and has just lost the only reader of
// its stdout, so cc blocks in write() on a full pipe while Close()'s cmd.Wait
// blocks in wait4() waiting for cc to exit. Neither moves.
//
// The context is deliberately live for the whole assertion. That is not
// incidental: cancellation is the ONLY thing that broke this deadlock before
// tether#62 (the connection context eventually SIGKILLs cc), so a test that
// cancelled early would pass against the unfixed code and prove nothing. The
// deferred cancel exists solely so a failing run does not leave the shim behind.
//
// It is also why cmd.WaitDelay cannot be the fix for this path: WaitDelay's timer
// does not start until the context is done, and here it never is.
func TestSpawn_OversizedLineDoesNotWedgeTheReap(t *testing.T) {
	dir := t.TempDir()
	shim := writeShim(t, dir, "cc-flood.sh", shimOversizedLine)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel() // cleanup only — every assertion below runs with ctx live

	p := NewClaudeCodeProvider(shim, withMaxLine(4096))
	sess, err := p.Spawn(ctx, SpawnConfig{Workdir: dir})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}

	evs, closed := eventsClosed(sess, 15*time.Second)
	if !closed {
		t.Fatal("Events() never closed; readLoop should have given up on the oversized line")
	}

	// Kill-then-reap must be what happens, and it must happen without the context
	// ever being cancelled.
	err, returned := closedWithin(sess.Close, 15*time.Second)
	if !returned {
		t.Fatal("Close() did not return with the context still live: cmd.Wait is deadlocked " +
			"against a child wedged on its own unread stdout pipe")
	}
	if ctx.Err() != nil {
		t.Fatalf("context was cancelled during the test (%v) — the assertion above proved nothing", ctx.Err())
	}
	// A killed child, so an error is expected; what matters is only that Wait
	// reported at all.
	t.Logf("Close() = %v", err)

	// The consumer has to learn the turn ended, or the frontend spinner stays up
	// forever. Here the context is live, so emit's delivery is deterministic.
	var found error
	for _, ev := range evs {
		if ev.Kind == EventError && ev.Err != nil {
			found = ev.Err
		}
	}
	if found == nil {
		t.Fatalf("no EventError among %v; an abandoned stream must close the turn", eventKinds(evs))
	}
	if !errors.Is(found, bufio.ErrTooLong) {
		t.Errorf("EventError = %v, want it to wrap bufio.ErrTooLong", found)
	}
}

// shimCleanExit: the ordinary session — one init, one result, exit 0, no orphan,
// no oversized line.
const shimCleanExit = `#!/bin/sh
echo '{"type":"system","subtype":"init","session_id":"clean-sid"}'
echo '{"type":"result","result":"done"}'
exit 0
`

// TestSpawn_CleanExitIsNeitherKilledNorForced is a NEGATIVE CONTROL: it passes
// both before and after tether#62, and pins that readLoop's new error branch
// stays off the ordinary path. A kill shows up as a non-nil Close() (Wait would
// report "signal: killed" rather than exit 0) and an abandon shows up as an
// EventError where the turn should just have ended.
//
// It says nothing about guardStdout, and the distinction is worth stating
// because the name invites the broader claim: this context is never cancelled,
// so the guard's arming condition is never even reached here. Deleting the grace
// period leaves this test green — the test that catches that is
// TestGuardStdout_LeavesThePipeAloneWhenReadLoopFinishesInTime.
func TestSpawn_CleanExitIsNeitherKilledNorForced(t *testing.T) {
	dir := t.TempDir()
	shim := writeShim(t, dir, "cc-clean.sh", shimCleanExit)

	p := NewClaudeCodeProvider(shim)
	sess, err := p.Spawn(context.Background(), SpawnConfig{Workdir: dir})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}

	evs, closed := eventsClosed(sess, 10*time.Second)
	if !closed {
		t.Fatal("Events() never closed on a clean exit")
	}
	kinds := eventKinds(evs)
	for _, ev := range evs {
		if ev.Kind == EventError {
			t.Errorf("EventError %v on a clean exit — a guard fired when it should not have", ev.Err)
		}
	}
	if len(evs) == 0 || evs[len(evs)-1].Kind != EventResult {
		t.Errorf("events = %v, want the turn to end in %v", kinds, EventResult)
	}
	if err := sess.Close(); err != nil {
		t.Errorf("Close() = %v, want nil — the subprocess must exit 0, i.e. never be killed", err)
	}
}

// recordingCloser stands in for the stdout read end so a guardStdout unit test
// can observe whether it was force-closed, without a subprocess.
type recordingCloser struct {
	once   sync.Once
	closed chan struct{}
}

func newRecordingCloser() *recordingCloser {
	return &recordingCloser{closed: make(chan struct{})}
}

func (c *recordingCloser) Close() error {
	c.once.Do(func() { close(c.closed) })
	return nil
}

func (c *recordingCloser) closedWithin(d time.Duration) bool {
	select {
	case <-c.closed:
		return true
	case <-time.After(d):
		return false
	}
}

// guardSession builds the minimum ccSession guardStdout touches.
func guardSession(grace time.Duration) (*ccSession, *recordingCloser, context.CancelFunc) {
	ctx, cancel := context.WithCancel(context.Background())
	rc := newRecordingCloser()
	return &ccSession{
		ctx:         ctx,
		stdout:      rc,
		done:        make(chan struct{}),
		stdoutGrace: grace,
	}, rc, cancel
}

// TestGuardStdout_ForcesTheePipeWhenReadLoopIsStuck — cancellation plus the grace
// elapsing, with readLoop still running: the pipe MUST be closed, because that
// close is the only thing in the process that can unpark scanner.Scan().
func TestGuardStdout_ForcesThePipeWhenReadLoopIsStuck(t *testing.T) {
	s, rc, cancel := guardSession(50 * time.Millisecond)
	go s.guardStdout()
	cancel()
	if !rc.closedWithin(5 * time.Second) {
		t.Fatal("guardStdout never closed the stdout read end; a parked scanner.Scan() " +
			"has nothing else that can free it")
	}
}

// TestGuardStdout_LeavesThePipeAloneWhenReadLoopFinishesInTime — the ordinary
// cancellation: cc is SIGKILLed, its write end closes with it, and readLoop
// reaches EOF well inside the grace. The guard must then do NOTHING.
//
// This is what makes the grace period load-bearing rather than decorative: with
// a zero (or absent) grace the guard would close the pipe out from under a
// readLoop that was still draining the buffered tail of the turn, turning a rare
// leak into routine loss of a turn's last events plus a spurious EventError.
func TestGuardStdout_LeavesThePipeAloneWhenReadLoopFinishesInTime(t *testing.T) {
	s, rc, cancel := guardSession(30 * time.Second)
	go s.guardStdout()
	cancel()
	time.Sleep(50 * time.Millisecond) // guard is now inside its grace wait
	close(s.done)                     // readLoop returned on its own
	if rc.closedWithin(500 * time.Millisecond) {
		t.Fatal("guardStdout force-closed the pipe even though readLoop had already " +
			"finished — on the ordinary disconnect this truncates the turn")
	}
}

// TestGuardStdout_ExitsWhenTheSessionEndsWithoutCancellation — a session that
// ends normally, with no cancellation at all, must not leave the guard parked
// and must not touch the pipe.
func TestGuardStdout_ExitsWhenTheSessionEndsWithoutCancellation(t *testing.T) {
	s, rc, cancel := guardSession(30 * time.Second)
	defer cancel()
	returned := make(chan struct{})
	go func() { s.guardStdout(); close(returned) }()
	close(s.done)
	select {
	case <-returned:
	case <-time.After(5 * time.Second):
		t.Fatal("guardStdout stayed parked after the session ended")
	}
	if rc.closedWithin(100 * time.Millisecond) {
		t.Error("guardStdout closed the pipe on a session that ended by itself")
	}
}

// TestAbandon_KillsBeforeItCanBlockOnTheEmit drives readLoop's error exit
// directly against a real, LIVE child and pins BOTH halves of abandon's
// contract: that it kills at all, and that it kills BEFORE it emits.
//
// The ordering is what the setup here is for. abandon emits an EventError, and a
// terminal event is delivered rather than dropped (see emit), so the emit is a
// step that CAN block — and its only escape is the context, which on the
// ErrTooLong path is still live. So this session is given an unbuffered, undrained
// events channel and a context that never completes: the emit is then guaranteed
// to park. Kill-then-emit still reaps the child; emit-then-kill never reaches the
// kill, and cmd.Wait stays blocked in wait4 behind a child that never dies.
//
// Without this arrangement the ordering is untestable: the production buffer is
// 64 deep and every other test drains it, so both orders look identical.
func TestAbandon_KillsBeforeItCanBlockOnTheEmit(t *testing.T) {
	dir := t.TempDir()
	shim := writeShim(t, dir, "cc-sleeper.sh", "#!/bin/sh\nsleep 30\n")

	cmd := exec.Command(shim)
	cmd.Dir = dir
	if err := cmd.Start(); err != nil {
		t.Fatalf("start shim: %v", err)
	}
	s := &ccSession{
		cmd:      cmd,
		ctx:      context.Background(), // never done: emit's escape hatch is closed off
		events:   make(chan Event),     // unbuffered and undrained: the emit WILL park
		sidReady: make(chan struct{}),
		done:     make(chan struct{}),
	}
	go s.abandon(fmt.Errorf("synthetic scan failure: %w", bufio.ErrTooLong))

	waited := make(chan error, 1)
	go func() { waited <- cmd.Wait() }()
	select {
	case err := <-waited:
		t.Logf("Wait() = %v", err)
	case <-time.After(10 * time.Second):
		_ = cmd.Process.Kill()
		t.Fatal("cmd.Wait() blocked after abandon: the subprocess was not killed before " +
			"abandon parked on its emit")
	}

	// Release the parked emit so its goroutine does not outlive the test.
	select {
	case <-s.events:
	case <-time.After(2 * time.Second):
		t.Log("abandon's emit was not parked when we looked — harmless, the assertion above stands")
	}
}

// TestSpawn_ThreadsTheGuardKnobsIntoTheSession pins the four hand-written
// assignments that carry Spawn's decisions into the session the guards read.
// Every one of them is a silent failure if dropped: an unthreaded grace becomes
// 0, which turns guardStdout from a last resort into something that fires on
// every cancellation and truncates the turn; an unthreaded stdout leaves the
// guard with nothing to close. Behavioural tests do not see either, because a
// grace of 0 still ends the session (faster, even) and the truncation it causes
// needs a readLoop caught mid-drain to be observable at all. So this asserts the
// values, at the seam, against the same-named source fields.
func TestSpawn_ThreadsTheGuardKnobsIntoTheSession(t *testing.T) {
	dir := t.TempDir()
	shim := writeShim(t, dir, "cc-clean.sh", shimCleanExit)

	t.Run("production defaults", func(t *testing.T) {
		p := NewClaudeCodeProvider(shim) // exactly how lifecycle.go builds it
		sess, err := p.Spawn(context.Background(), SpawnConfig{Workdir: dir})
		if err != nil {
			t.Fatalf("Spawn: %v", err)
		}
		defer func() { _ = sess.Close() }()
		cs, ok := sess.(*ccSession)
		if !ok {
			t.Fatalf("Spawn returned %T, want *ccSession", sess)
		}
		if cs.stdoutGrace != defaultStdoutGrace {
			t.Errorf("stdoutGrace = %v, want defaultStdoutGrace (%v)", cs.stdoutGrace, defaultStdoutGrace)
		}
		if got := cs.scanMax(); got != scanBufMax {
			t.Errorf("scanMax() = %d, want scanBufMax (%d)", got, scanBufMax)
		}
		if cs.stdout == nil {
			t.Error("stdout is nil — guardStdout would have nothing to close")
		}
	})

	t.Run("options override", func(t *testing.T) {
		const grace = 1234 * time.Millisecond
		const maxLine = 4321
		p := NewClaudeCodeProvider(shim, withStdoutGrace(grace), withMaxLine(maxLine))
		sess, err := p.Spawn(context.Background(), SpawnConfig{Workdir: dir})
		if err != nil {
			t.Fatalf("Spawn: %v", err)
		}
		defer func() { _ = sess.Close() }()
		cs := sess.(*ccSession)
		if cs.stdoutGrace != grace {
			t.Errorf("stdoutGrace = %v, want the withStdoutGrace value %v", cs.stdoutGrace, grace)
		}
		if got := cs.scanMax(); got != maxLine {
			t.Errorf("scanMax() = %d, want the withMaxLine value %d", got, maxLine)
		}
	})
}

// shimOpenCodeRunOversized is a fake `opencode` for the run path only: emit one
// line past that scanner's 1MB cap and never terminate it, then hold fd 1. head
// and tr keep it to a single write burst rather than a fork per kilobyte.
const shimOpenCodeRunOversized = `#!/bin/sh
head -c 1200000 /dev/zero | tr '\0' 'a'
sleep 30
`

// TestOpenCodeRun_OversizedLineDoesNotWedgeTheTurn covers the sibling of
// ccSession.abandon: the `opencode run` child has the same shape (StdoutPipe, a
// caller-owned scan, then cmd.Wait), so a scan that gives up while that child is
// alive wedges the same way — cmd.Wait in wait4, the child in write() on a pipe
// nobody drains — and takes the turn's EventResult with it, leaving the caller's
// spinner up and the run goroutine leaked.
//
// The session is hand-built rather than spawned: reaching SendPrompt through
// Spawn needs a fake that serves HTTP, while the run child only ever receives
// baseURL as an --attach argument it is free to ignore. dormant is false, so
// SendPrompt goes straight to the run goroutine.
func TestOpenCodeRun_OversizedLineDoesNotWedgeTheTurn(t *testing.T) {
	dir := t.TempDir()
	// The run child is exec'd by the bare name "opencode", resolved through this
	// process's PATH, so a shim directory in front is the whole install.
	writeShim(t, dir, "opencode", shimOpenCodeRunOversized)
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel() // cleanup only — the assertions below run with ctx live

	s := &opencodeSession{
		workdir:  dir,
		spawnCtx: ctx,
		events:   make(chan Event, 64),
		sidCh:    make(chan struct{}),
		baseURL:  "http://127.0.0.1:1/ignored-by-the-shim",
	}
	if err := s.SendPrompt(ctx, "hello"); err != nil {
		t.Fatalf("SendPrompt: %v", err)
	}

	var (
		sawResult bool
		errs      []error
		deadline  = time.After(20 * time.Second)
	)
	for !sawResult {
		select {
		case ev := <-s.events:
			switch ev.Kind {
			case EventResult:
				sawResult = true
			case EventError:
				errs = append(errs, ev.Err)
			}
		case <-deadline:
			t.Fatal("the run goroutine never reached EventResult: cmd.Wait is deadlocked " +
				"against a child wedged on its own unread stdout pipe, so the turn never closes")
		}
	}
	if ctx.Err() != nil {
		t.Fatalf("context was cancelled during the test (%v) — the assertion above proved nothing", ctx.Err())
	}

	// Exactly one, and it must be the scan error. The count is the load-bearing
	// half: the kill we just issued makes cmd.Wait report "signal: killed", and
	// the exit-error branch below it suppresses only CANCELLED runs — but neither
	// context is cancelled here, by construction. Without killedByUs the caller
	// gets a second EventError blaming opencode for a signal tether sent.
	if len(errs) != 1 {
		t.Fatalf("got %d EventErrors, want exactly 1 (the scan error): %v", len(errs), errs)
	}
	if !errors.Is(errs[0], bufio.ErrTooLong) {
		t.Errorf("EventError = %v, want it to wrap bufio.ErrTooLong", errs[0])
	}
}
