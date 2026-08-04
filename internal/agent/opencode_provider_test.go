package agent

import (
	"context"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// newTestOCSession builds a bare opencodeSession suitable for exercising the
// pure parsing/lifecycle helpers without spawning any real subprocess.
func newTestOCSession() *opencodeSession {
	return &opencodeSession{
		spawnCtx: context.Background(), // emit's terminal branch selects on this; nil would panic
		events:   make(chan Event, 64),
		sidCh:    make(chan struct{}),
	}
}

// TestOpenCodeReadSSE_TextDelta verifies a message.part.delta text frame yields
// a token-level EventText and publishes the session id.
func TestOpenCodeReadSSE_TextDelta(t *testing.T) {
	s := newTestOCSession()
	body := strings.Join([]string{
		`data: {"payload":{"type":"message.part.delta","properties":{"sessionID":"ses_1","field":"text","delta":"Hel"}}}`,
		`data: {"payload":{"type":"message.part.delta","properties":{"sessionID":"ses_1","field":"text","delta":"lo"}}}`,
		// non-text field must be ignored
		`data: {"payload":{"type":"message.part.delta","properties":{"sessionID":"ses_1","field":"reasoning","delta":"x"}}}`,
		"",
	}, "\n")

	var got []Event
	s.readSSE(context.Background(), strings.NewReader(body), func(ev Event) { got = append(got, ev) })

	if len(got) != 2 {
		t.Fatalf("expected 2 text events, got %d: %+v", len(got), got)
	}
	for _, ev := range got {
		if ev.Kind != EventText {
			t.Errorf("Kind = %q, want %q", ev.Kind, EventText)
		}
	}
	if got[0].Text != "Hel" || got[1].Text != "lo" {
		t.Errorf("deltas = %q,%q; want Hel,lo", got[0].Text, got[1].Text)
	}
	if s.SessionID() != "ses_1" {
		t.Errorf("SessionID = %q, want ses_1", s.SessionID())
	}
}

// TestOpenCodeReadSSE_CreatedAndError covers session.created -> EventInit and
// session.error -> EventError.
func TestOpenCodeReadSSE_CreatedAndError(t *testing.T) {
	s := newTestOCSession()
	body := strings.Join([]string{
		`data: {"payload":{"type":"session.created","properties":{"sessionID":"ses_9"}}}`,
		`data: {"payload":{"type":"session.error","properties":{"error":{"message":"boom"}}}}`,
		// unrecognized type is ignored
		`data: {"payload":{"type":"session.idle","properties":{}}}`,
		// non-SSE line ignored
		`: keep-alive`,
		"",
	}, "\n")

	var got []Event
	s.readSSE(context.Background(), strings.NewReader(body), func(ev Event) { got = append(got, ev) })

	if len(got) != 2 {
		t.Fatalf("expected 2 events (init+error), got %d: %+v", len(got), got)
	}
	if got[0].Kind != EventInit || got[0].SessionID != "ses_9" {
		t.Errorf("event0 = %+v, want EventInit/ses_9", got[0])
	}
	if got[1].Kind != EventError || got[1].Err == nil || got[1].Err.Error() != "boom" {
		t.Errorf("event1 = %+v, want EventError/boom", got[1])
	}
}

// TestOpenCodeInterrupt_IdleNoOp — Interrupt() on a session with no in-flight
// prompt must be a no-op (must not touch the nil serve / panic).
func TestOpenCodeInterrupt_IdleNoOp(t *testing.T) {
	s := newTestOCSession() // busy == false, serve == nil
	if err := s.Interrupt(); err != nil {
		t.Fatalf("idle Interrupt() = %v, want nil", err)
	}
	s.mu.RLock()
	dormant := s.dormant
	s.mu.RUnlock()
	if dormant {
		t.Error("idle Interrupt() must not hibernate the serve (dormant=true)")
	}
}

// TestOpenCodeCloseEvents_Idempotent — closeEvents must be safe to call twice.
func TestOpenCodeCloseEvents_Idempotent(t *testing.T) {
	s := newTestOCSession()
	s.closeEvents()
	s.closeEvents() // must not panic on double close
	if !s.closed {
		t.Error("closed flag not set")
	}
	// emit after close is dropped, not a panic.
	s.emit(Event{Kind: EventText, Text: "late"})
}

// TestOpenCodeAlive_TracksStreamNotProcess — Alive() must follow the SESSION's
// event stream, not any `opencode serve` child (tether#55).
//
// The dormant case is the one that would bite a process-shaped implementation:
// Interrupt() kills the serve on purpose and SendPrompt relaunches it against
// the same on-disk conversation, so a session with NO running child is entirely
// usable and must keep reporting alive. Only closeEvents — Close(), or the
// Spawn ctx-done teardown — ends a session.
func TestOpenCodeAlive_TracksStreamNotProcess(t *testing.T) {
	s := newTestOCSession()
	if !s.Alive() {
		t.Fatal("Alive() = false for a fresh session")
	}

	s.mu.Lock()
	s.dormant = true // hibernated by Interrupt(): no serve child, still usable
	s.mu.Unlock()
	if !s.Alive() {
		t.Error("Alive() = false for a dormant session — SendPrompt would have relaunched it")
	}

	s.closeEvents()
	if s.Alive() {
		t.Error("Alive() = true after closeEvents ended the session's stream")
	}
	s.closeEvents() // once-guarded; must not flip the answer back or panic
	if s.Alive() {
		t.Error("Alive() = true after a second closeEvents")
	}
}

// --- tether#58: an unexpected `opencode serve` death must end the session ---

// ocWatchFixture wires a session to the REAL watchServeExit against a real,
// killable child process standing in for `opencode serve`.
//
// A stand-in is honest here rather than a shortcut: watchServeExit only ever
// calls Wait() on the *exec.Cmd, so what it observes about `opencode serve` and
// about `sleep` is the same observation — a process exiting. Using `sleep` is
// what lets these tests run in CI, which has no opencode binary.
//
// WHAT THESE FIXTURES DO NOT COVER, so the file is not read as covering it: they
// hand-wire `armed`/`live`, so they never execute startServe, which is where the
// arming is actually installed. Deleting `s.serveArmed = armed`,
// `armed.Store(true)` or the deferred `close(live)` leaves every test here green
// while tether#58 is fully restored. That half is covered by
// TestOpenCodeStartServeArmsExitWatcher and
// TestOpenCodeInterruptAfterRealStartServe_StaysAlive, which drive the real
// startServe against a fake `opencode` (installFakeOpenCode) and so run in CI —
// unlike TestOpenCodeServeCrash_RealBinary, which skips wherever the real binary
// is absent, CI included.
type ocWatchFixture struct {
	s        *opencodeSession
	proc     *exec.Cmd
	exitDone chan struct{}
	live     chan struct{}
	armed    *atomic.Bool
	sseCtx   context.Context
	cancel   context.CancelFunc
}

func newOCWatchFixture(t *testing.T) *ocWatchFixture {
	t.Helper()
	proc := exec.Command("sleep", "60")
	if err := proc.Start(); err != nil {
		t.Fatalf("start stand-in serve: %v", err)
	}
	sseCtx, cancel := context.WithCancel(context.Background())
	f := &ocWatchFixture{
		s:        newTestOCSession(),
		proc:     proc,
		exitDone: make(chan struct{}),
		live:     make(chan struct{}),
		armed:    new(atomic.Bool),
		sseCtx:   sseCtx,
		cancel:   cancel,
	}
	t.Cleanup(func() {
		cancel()
		_ = proc.Process.Kill()
	})
	return f
}

// wire installs the fixture's incarnation on the session exactly as a
// successful startServe would, so Interrupt()/Close() find it.
func (f *ocWatchFixture) wire(armed bool) {
	f.armed.Store(armed)
	f.s.mu.Lock()
	f.s.serve = f.proc
	f.s.serveExitDone = f.exitDone
	f.s.serveArmed = f.armed
	f.s.sseCancel = f.cancel
	f.s.mu.Unlock()
}

func (f *ocWatchFixture) watch() {
	f.s.watchServeExit(f.proc, f.exitDone, f.live, f.armed, f.cancel)
}

// drainEvents empties the session's event buffer without blocking and reports
// whether the stream is CLOSED. It drains rather than peeking because an
// unexpected death now queues a terminal event AHEAD of the close, which is
// exactly the order a real consumer (Registry.fanOut) sees.
func ocDrainEvents(s *opencodeSession) (evs []Event, closed bool) {
	for {
		select {
		case ev, ok := <-s.events:
			if !ok {
				return evs, true
			}
			evs = append(evs, ev)
		default:
			return evs, false
		}
	}
}

// TestOpenCodeServeDiesUnexpectedly_EndsSession is the tether#58 regression.
//
// The serve dies on its own — not via Interrupt(), not via Close() — which
// before this fix left NOTHING to call closeEvents: the stream stayed open, so
// Registry.fanOut never returned and never ran its deferred evict, and Alive()
// answered true forever, so tether#55's reuse gate kept handing the corpse out
// and every prompt hung. Driven synchronously so there is no sleep to tune: the
// kill makes Wait() return, live is already closed, so watchServeExit runs to
// completion and every assertion below is about settled state.
func TestOpenCodeServeDiesUnexpectedly_EndsSession(t *testing.T) {
	f := newOCWatchFixture(t)
	f.wire(true) // live incarnation, nobody has asked it to stop

	if !f.s.Alive() {
		t.Fatal("Alive() = false before the serve died")
	}

	close(f.live)             // startServe has handed this incarnation over
	_ = f.proc.Process.Kill() // the crash: a death from outside the session
	f.watch()

	if f.s.Alive() {
		t.Error("Alive() = true after the serve died unexpectedly — the corpse is still reusable (tether#58)")
	}
	evs, closed := ocDrainEvents(f.s)
	if !closed {
		t.Error("event stream still open — Registry.fanOut never returns, so the entry is never evicted")
	}
	// The turn in flight when the serve died has to be finalized before the
	// stream shuts, or the browser keeps waiting: evict removes registry keys
	// but never closes a subscriber channel, so the close alone tells it nothing.
	if len(evs) != 1 || !isTerminal(evs[0].Kind) {
		t.Errorf("want exactly one terminal event before the close, got %+v", evs)
	} else if evs[0].Err == nil || !strings.Contains(evs[0].Err.Error(), "unexpectedly") {
		t.Errorf("terminal event does not explain the crash: %+v", evs[0])
	}
	if f.sseCtx.Err() == nil {
		t.Error("SSE loop not cancelled — it would reconnect to a dead port every 500ms forever")
	}
	select {
	case <-f.s.sidCh:
	default:
		t.Error("SessionID() waiters not released — a crash before session.created would park them")
	}
	select {
	case <-f.exitDone:
	default:
		t.Error("exitDone not closed — Interrupt()/Close() would block on it forever")
	}
}

// TestOpenCodeInterruptHibernation_KeepsSessionAlive is the trap: the naive fix
// (end the session on ANY serve exit) passes the test above and breaks this one.
// Interrupt() kills the serve deliberately and the session must stay alive and
// relaunchable, because opencode persisted the conversation to disk and the next
// SendPrompt brings a fresh serve up against it.
func TestOpenCodeInterruptHibernation_KeepsSessionAlive(t *testing.T) {
	f := newOCWatchFixture(t)
	f.wire(false) // what Interrupt() does before it kills

	close(f.live)
	_ = f.proc.Process.Kill()
	f.watch()

	if !f.s.Alive() {
		t.Error("Alive() = false after a hibernating Interrupt() — SendPrompt would have relaunched this session")
	}
	if evs, closed := ocDrainEvents(f.s); closed {
		t.Error("event stream closed by a hibernation — the entry gets evicted and the conversation is lost")
	} else if len(evs) != 0 {
		t.Errorf("hibernation emitted %+v — a relaunchable session must not report a crash", evs)
	}
	if f.sseCtx.Err() != nil {
		t.Error("hibernation cancelled the session-level cancel via the watcher rather than via Interrupt()")
	}
}

// TestOpenCodeInterrupt_DisarmsExitWatcher covers the WIRING that the two tests
// above stub: Interrupt() itself must clear serveArmed before it kills, or the
// watcher sees a live incarnation exit and ends the session.
func TestOpenCodeInterrupt_DisarmsExitWatcher(t *testing.T) {
	f := newOCWatchFixture(t)
	f.wire(true)
	f.s.busy.Store(true) // Interrupt() no-ops on an idle session

	watcherDone := make(chan struct{})
	go func() { defer close(watcherDone); f.watch() }()
	close(f.live)

	if err := f.s.Interrupt(); err != nil {
		t.Fatalf("Interrupt() = %v, want nil", err)
	}
	<-watcherDone // deterministic join: no sleep, no flake

	if f.armed.Load() {
		t.Error("Interrupt() left the exit watcher armed")
	}
	if !f.s.Alive() {
		t.Error("Alive() = false after Interrupt() — hibernation must not end the session")
	}
	if evs, closed := ocDrainEvents(f.s); closed {
		t.Error("Interrupt() closed the event stream")
	} else if len(evs) != 0 {
		t.Errorf("Interrupt() emitted %+v — hibernation is not a crash", evs)
	}
	f.s.mu.RLock()
	dormant := f.s.dormant
	f.s.mu.RUnlock()
	if !dormant {
		t.Error("Interrupt() did not mark the session dormant")
	}
}

// TestOpenCodeClose_EndsSessionOnce — Close() disarms too, and still ends the
// session itself. Also pins that Close() does not deadlock against the watcher:
// it waits on exitDone, which watchServeExit closes before parking on live.
func TestOpenCodeClose_EndsSessionOnce(t *testing.T) {
	f := newOCWatchFixture(t)
	f.wire(true)

	watcherDone := make(chan struct{})
	go func() { defer close(watcherDone); f.watch() }()
	close(f.live)

	_ = f.s.Close() // returns the serve's exit error; SIGKILL makes it non-nil
	<-watcherDone

	if f.armed.Load() {
		t.Error("Close() left the exit watcher armed")
	}
	if f.s.Alive() {
		t.Error("Alive() = true after Close()")
	}
	if _, closed := ocDrainEvents(f.s); !closed {
		t.Error("Close() did not end the event stream")
	}
}

// TestOpenCodeWatcherWaitsForStartServe pins the `live` handoff, which is the
// load-bearing half of the discriminator and the half no other test reaches.
//
// Until startServe has finished with an incarnation, startServe OWNS its failure
// and reports it by returning an error — so the watcher must not act, however
// dead the process already is. Delete the `<-live` park and this goes red while
// every other test in the file stays green: the watcher would read `armed` before
// startServe had finished setting it, which is both the false-negative that lets
// tether#58 survive a death during startup and the false-positive that would
// close the event stream out from under startServe's own error return.
func TestOpenCodeWatcherWaitsForStartServe(t *testing.T) {
	f := newOCWatchFixture(t)
	f.wire(true)

	watcherDone := make(chan struct{})
	go func() { defer close(watcherDone); f.watch() }()

	// The process dies while startServe is still deciding — `live` stays open.
	_ = f.proc.Process.Kill()

	// exitDone is closed strictly BEFORE the watcher parks on live, so once we
	// observe it the watcher has provably reached the park and any teardown it
	// was going to do without waiting would already have happened.
	select {
	case <-f.exitDone:
	case <-time.After(10 * time.Second):
		t.Fatal("watcher never reaped the process")
	}
	time.Sleep(100 * time.Millisecond) // margin for an unsynchronized teardown

	if !f.s.Alive() {
		t.Error("watcher ended the session before startServe handed the incarnation over")
	}
	if _, closed := ocDrainEvents(f.s); closed {
		t.Error("watcher closed the event stream before startServe was done — its error return would be swallowed")
	}

	close(f.live) // startServe returns; NOW the verdict is the watcher's to make
	<-watcherDone
	if f.s.Alive() {
		t.Error("Alive() = true after the handoff completed on a dead armed incarnation")
	}
}

// --- fake `opencode serve`, so startServe's own wiring is covered in CI ---

// envFakeOC marks a re-execution of this test binary as the fake `opencode`.
const envFakeOC = "TETHER_FAKE_OPENCODE"

// installFakeOpenCode puts a fake `opencode` first on PATH and returns nothing:
// the point is the side effect, undone by t.Setenv/t.TempDir at test end.
//
// WHY A FAKE AT ALL, given TestOpenCodeServeCrash_RealBinary exists: that test
// skips wherever `opencode` is absent, which includes CI, so every line of
// startServe's arming (s.serveArmed = armed, armed.Store(true), the deferred
// close(live)) was invisible to `go test ./...` — delete any one of them and
// tether#58 comes straight back with a green build. This closes that.
//
// HOW, without touching the package's shared TestMain: startServe execs the bare
// name "opencode", which exec resolves through the PARENT's PATH, so a shim
// directory is enough. The shim re-executes THIS test binary with -test.run
// pinned to the helper below — Go's own os/exec TestHelperProcess pattern — which
// needs no new TestMain branch and so cannot collide with the fake-cc one.
func installFakeOpenCode(t *testing.T) {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatalf("locate test binary: %v", err)
	}
	dir := t.TempDir()
	shim := filepath.Join(dir, "opencode")
	script := "#!/bin/sh\nexec " + self + " -test.run='^TestHelperOpenCodeServe$' -- \"$@\"\n"
	if err := os.WriteFile(shim, []byte(script), 0o755); err != nil {
		t.Fatalf("write shim: %v", err)
	}
	t.Setenv(envFakeOC, "1")
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// TestHelperOpenCodeServe is the fake `opencode serve` — a process, not a test.
//
// The discriminator is env AND argv, never env alone: environment variables are
// inherited, so a leaked envFakeOC would otherwise turn a real suite run into a
// server that blocks forever (the fake-cc file makes the same point about a
// silently green suite — tether#53). A real `go test` argv never contains the
// bare word "serve", so requiring it separates the two cases outright.
func TestHelperOpenCodeServe(t *testing.T) {
	if os.Getenv(envFakeOC) == "" {
		t.Skip("not a fake-opencode invocation")
	}
	port := ""
	isServe := false
	for i, a := range os.Args {
		if a == "serve" {
			isServe = true
		}
		if a == "--port" && i+1 < len(os.Args) {
			port = os.Args[i+1]
		}
	}
	if !isServe {
		t.Skip("not a fake-opencode invocation (argv has no `serve`)")
	}
	if port == "" {
		t.Fatal("fake opencode serve: no --port in argv")
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/global/health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	// Hold the SSE stream open the way a real serve does, so sseLoop settles
	// instead of reconnect-spinning while the test works.
	mux.HandleFunc("/global/event", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		<-r.Context().Done()
	})
	srv := &http.Server{Addr: "127.0.0.1:" + port, Handler: mux}
	// Blocks until the parent kills us — which is the event under test.
	_ = srv.ListenAndServe()
}

// TestOpenCodeStartServeArmsExitWatcher runs the REAL startServe against the fake
// and then kills its serve from outside Interrupt()/Close(). Unlike the
// hand-wired fixtures above, nothing here stubs the arming, so this is what makes
// startServe's three wiring lines CI-visible.
func TestOpenCodeStartServeArmsExitWatcher(t *testing.T) {
	installFakeOpenCode(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sess, err := NewOpenCodeProvider().Spawn(ctx, SpawnConfig{Workdir: t.TempDir()})
	if err != nil {
		t.Fatalf("spawn against fake opencode: %v", err)
	}
	defer sess.Close()
	oc := sess.(*opencodeSession)

	if !sess.Alive() {
		t.Fatal("Alive() = false right after a successful Spawn")
	}
	oc.mu.RLock()
	armed, proc := oc.serveArmed, oc.serve.Process
	oc.mu.RUnlock()
	if armed == nil {
		t.Fatal("startServe did not publish serveArmed — Interrupt() could never disarm")
	}
	if !armed.Load() {
		t.Fatal("startServe left the exit watcher disarmed — an unexpected death would go unnoticed")
	}

	// Drain like Registry.fanOut does, and record when the stream ends.
	ended := make(chan struct{})
	go func() {
		defer close(ended)
		for range sess.Events() {
		}
	}()

	if err := proc.Kill(); err != nil {
		t.Fatalf("kill fake serve: %v", err)
	}
	select {
	case <-ended:
	case <-time.After(15 * time.Second):
		t.Fatal("event stream never ended after the serve died (tether#58)")
	}
	if sess.Alive() {
		t.Error("Alive() = true after the serve died unexpectedly")
	}
}

// TestOpenCodeInterruptAfterRealStartServe_StaysAlive is the trap, end to end
// through the real startServe: hibernation must survive the arming wired above.
// It is what catches a startServe that forgets `s.serveArmed = armed`, since
// Interrupt() then has nothing to disarm and kills the session it meant to park.
func TestOpenCodeInterruptAfterRealStartServe_StaysAlive(t *testing.T) {
	installFakeOpenCode(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sess, err := NewOpenCodeProvider().Spawn(ctx, SpawnConfig{Workdir: t.TempDir()})
	if err != nil {
		t.Fatalf("spawn against fake opencode: %v", err)
	}
	defer sess.Close()
	oc := sess.(*opencodeSession)

	// Interrupt() no-ops unless a prompt is in flight; the fake has no `run`
	// implementation, so assert the busy precondition directly rather than
	// pretending to drive a turn.
	oc.busy.Store(true)
	if err := sess.Interrupt(); err != nil {
		t.Fatalf("Interrupt() = %v, want nil", err)
	}

	if !sess.Alive() {
		t.Error("Alive() = false after Interrupt() — hibernation must leave the session relaunchable")
	}
	if _, closed := ocDrainEvents(oc); closed {
		t.Error("Interrupt() ended the event stream — the registry entry gets evicted and the conversation is lost")
	}
	oc.mu.RLock()
	dormant := oc.dormant
	oc.mu.RUnlock()
	if !dormant {
		t.Error("Interrupt() did not mark the session dormant")
	}
}

// TestOpenCodeServeCrash_RealBinary is the same regression against the genuine
// `opencode serve`, covering the wiring the fixtures above stub out: that
// startServe really arms the watcher, and that a SIGKILL from outside the
// session really ends it.
//
// Unlike TestOpenCodeInterrupt_Integration this needs no credentials and makes
// no model call — it starts a serve, kills it, and watches the session react —
// so it is gated only on the binary being present rather than behind an env
// var. CI has no opencode and skips it.
func TestOpenCodeServeCrash_RealBinary(t *testing.T) {
	if _, err := exec.LookPath("opencode"); err != nil {
		t.Skip("opencode binary not found in PATH")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	sess, err := NewOpenCodeProvider().Spawn(ctx, SpawnConfig{Workdir: t.TempDir()})
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	defer sess.Close()
	oc := sess.(*opencodeSession)

	if !sess.Alive() {
		t.Fatal("Alive() = false for a freshly spawned session")
	}

	// Drain like Registry.fanOut does, and record when the stream ends.
	streamEnded := make(chan struct{})
	go func() {
		defer close(streamEnded)
		for range sess.Events() {
		}
	}()

	oc.mu.RLock()
	proc := oc.serve.Process
	oc.mu.RUnlock()
	t.Logf("killing real `opencode serve` pid=%d from outside the session", proc.Pid)
	if err := proc.Kill(); err != nil {
		t.Fatalf("kill serve: %v", err)
	}

	select {
	case <-streamEnded:
	case <-time.After(15 * time.Second):
		t.Fatal("event stream never ended after the serve was killed (tether#58)")
	}
	if sess.Alive() {
		t.Error("Alive() = true after the real serve was killed")
	}
}

// TestOpenCodeInterrupt_Integration exercises the real spawn -> prompt ->
// interrupt -> resume flow against an installed `opencode` binary. It is gated
// behind TETHER_OPENCODE_IT (and skipped if opencode is absent) because it
// spawns real subprocesses and makes a model call. Run locally with:
//
//	TETHER_OPENCODE_IT=1 GOWORK=off go test ./internal/agent/ -run Integration -v
//
// The model is pinned to a free opencode model via an opencode.json written
// into the run workdir; override with TETHER_OPENCODE_MODEL.
func TestOpenCodeInterrupt_Integration(t *testing.T) {
	if os.Getenv("TETHER_OPENCODE_IT") == "" {
		t.Skip("set TETHER_OPENCODE_IT=1 to run the opencode integration test")
	}
	if _, err := exec.LookPath("opencode"); err != nil {
		t.Skip("opencode binary not found in PATH")
	}
	model := os.Getenv("TETHER_OPENCODE_MODEL")
	if model == "" {
		model = "opencode/deepseek-v4-flash-free"
	}

	// Pin the model via a project config in the workdir so we don't burn paid
	// credits (the provider doesn't pass --model; opencode reads cwd config).
	work := t.TempDir()
	cfg := `{"$schema":"https://opencode.ai/config.json","model":"` + model + `"}`
	if err := os.WriteFile(filepath.Join(work, "opencode.json"), []byte(cfg), 0o644); err != nil {
		t.Fatalf("write opencode.json: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	sess, err := NewOpenCodeProvider().Spawn(ctx, SpawnConfig{Workdir: work})
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	defer sess.Close()
	oc := sess.(*opencodeSession)

	var counter chanCounter
	go func() {
		for ev := range sess.Events() {
			if ev.Kind == EventText {
				counter.incr()
			}
		}
	}()

	// First prompt: a long generation, so it's still running when we interrupt.
	const longPrompt = "Write an extremely long, exhaustive 1500-word essay on the full history of computing. Use many paragraphs and be thorough."
	if err := sess.SendPrompt(ctx, longPrompt); err != nil {
		t.Fatalf("send: %v", err)
	}
	if !waitFor(30*time.Second, func() bool { return counter.get() > 2 }) {
		t.Fatalf("no token deltas flowed within 30s (n=%d) — check model/creds", counter.get())
	}

	// Interrupt: must hibernate (kill) the serve and stop the delta flow.
	if err := sess.Interrupt(); err != nil {
		t.Fatalf("interrupt: %v", err)
	}
	oc.mu.RLock()
	dormant, exitDone := oc.dormant, oc.serveExitDone
	oc.mu.RUnlock()
	if !dormant {
		t.Error("after Interrupt() dormant should be true")
	}
	select { // Interrupt waits on exitDone before returning, so it's closed.
	case <-exitDone:
	default:
		t.Error("serve exitDone not closed after Interrupt()")
	}
	// Let any in-flight SSE settle, then confirm the flow has actually stopped
	// (this is the whole point: killing the serve stops generation).
	time.Sleep(4 * time.Second)
	afterGrace := counter.get()
	time.Sleep(3 * time.Second)
	if delta := counter.get() - afterGrace; delta > 0 {
		t.Errorf("deltas kept flowing after interrupt+grace (+%d) — serve not stopped", delta)
	}

	// Resume: a fresh prompt must relaunch the serve and produce output again.
	resumeBase := counter.get()
	if err := sess.SendPrompt(ctx, "Reply with the single word: resumed."); err != nil {
		t.Fatalf("resume send: %v", err)
	}
	if !waitFor(40*time.Second, func() bool { return counter.get() > resumeBase }) {
		t.Fatalf("resume produced no new output within 40s (serve relaunch failed)")
	}
	oc.mu.RLock()
	stillDormant := oc.dormant
	oc.mu.RUnlock()
	if stillDormant {
		t.Error("after resume SendPrompt dormant should be cleared")
	}
}

// chanCounter is a tiny concurrency-safe counter for the integration test.
type chanCounter struct {
	mu sync.Mutex
	n  int
}

func (c *chanCounter) incr() { c.mu.Lock(); c.n++; c.mu.Unlock() }
func (c *chanCounter) get() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.n
}

func waitFor(d time.Duration, cond func() bool) bool {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(100 * time.Millisecond)
	}
	return cond()
}
