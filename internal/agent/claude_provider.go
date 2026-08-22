package agent

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"sync"
	"time"
)

// scanBufInitial and scanBufMax size the bufio.Scanner readLoop drives. cc emits
// one JSON object per line and a single line can carry a whole file (a
// tool_result), so the cap is deliberately huge; the initial size keeps the
// ordinary line off the growth path.
const (
	scanBufInitial = 1 << 20
	scanBufMax     = 100 << 20
)

// defaultStdoutGrace is how long ccSession.guardStdout waits, after the Spawn
// context is done, before it force-closes cc's stdout read end.
//
// Generous on purpose. On every ordinary session the guard never fires at all: a
// cancelled context SIGKILLs cc, cc's write end closes with the process, and
// readLoop reaches EOF within microseconds. The grace only has to exceed the
// time readLoop needs to drain and parse what the pipe still holds — at most one
// pipe buffer of stream-json — so seconds is orders of magnitude of headroom,
// and firing early is the one way this guard could do harm (it would truncate a
// turn's last events). Long is cheap here; short is not.
const defaultStdoutGrace = 2 * time.Second

// ClaudeCodeProvider implements AgentProvider for `claude` CLI (D-05a §4).
type ClaudeCodeProvider struct {
	ccPath string
	// stderr is where the cc subprocess's stderr goes. nil — the only state
	// production ever puts it in, since NewClaudeCodeProvider takes no options
	// at its single call site (internal/server/lifecycle.go) — resolves to
	// os.Stderr in stderrSink, i.e. byte-for-byte the pre-seam behaviour.
	// Redirecting it is a test-only affordance (see withStderr).
	stderr io.Writer
	// stdoutGrace overrides defaultStdoutGrace for sessions this provider
	// spawns; 0 (production's only value) means the default. Test-only, see
	// withStdoutGrace.
	stdoutGrace time.Duration
	// maxLine overrides scanBufMax for sessions this provider spawns; 0
	// (production's only value) means the default. Test-only, see withMaxLine.
	maxLine int
}

// ccOption configures a ClaudeCodeProvider at construction time. The type is
// unexported on purpose: the seam exists so this package's tests can observe a
// spawned process, NOT so an embedder (or an environment variable) can
// reconfigure the daemon's agent at runtime (tether#53).
type ccOption func(*ClaudeCodeProvider)

// withStderr routes the cc subprocess's stderr to w instead of the daemon's own
// os.Stderr, so a test can assert on what cc wrote there — notably the
// "No conversation found with session ID: <uuid>" line a failed `--resume`
// produces, which is otherwise unobservable from inside the process.
//
// That is not the only line a failed `--resume` can produce, and tether#101 found
// the second one the hard way: cc also writes "Error: Session <uuid> is currently
// running as a background agent (<kind>). Use `claude agents` to find and attach
// to it, or add --fork-session to branch off a copy." and exits 1, for a session
// one of its own live non-interactive processes holds (measured, cc 2.1.233,
// 2026-08-18). Both arrive here as stderr and neither is PARSED anywhere: nothing
// in tether classifies a resume failure by matching this text, because the text
// is cc's and moves with its versions. The structured judgement lives in
// session/ccregistry.go, which reads the same registry cc consults; this seam
// remains what it says it is — a way for a test to see what cc said.
func withStderr(w io.Writer) ccOption {
	return func(p *ClaudeCodeProvider) { p.stderr = w }
}

// withStdoutGrace shortens ccSession.guardStdout's grace period. Test-only for
// the same reason withStderr is: the guard exists for a path measured in seconds
// of wall clock, and a test that waited defaultStdoutGrace for each case would
// be too slow to keep. Production never sets it.
func withStdoutGrace(d time.Duration) ccOption {
	return func(p *ClaudeCodeProvider) { p.stdoutGrace = d }
}

// withMaxLine shrinks the per-line scanner cap. Test-only: it is what lets a
// test reach readLoop's bufio.ErrTooLong path — the one failure the cap can
// actually produce — without pushing scanBufMax (100MB) through a pipe and
// allocating that much in a `-race` test binary.
func withMaxLine(n int) ccOption {
	return func(p *ClaudeCodeProvider) { p.maxLine = n }
}

// NewClaudeCodeProvider creates a provider using the given cc binary path.
// Options are package-visible only; passing none (what production does) leaves
// every field at the behaviour that predates the options parameter.
func NewClaudeCodeProvider(ccPath string, opts ...ccOption) *ClaudeCodeProvider {
	p := &ClaudeCodeProvider{ccPath: ccPath}
	for _, opt := range opts {
		opt(p)
	}
	return p
}

// stderrSink resolves the subprocess stderr destination. An unset (nil) sink
// means os.Stderr, so a zero-value ClaudeCodeProvider literal behaves exactly
// like one built before the seam existed.
func (p *ClaudeCodeProvider) stderrSink() io.Writer {
	if p.stderr == nil {
		return os.Stderr
	}
	return p.stderr
}

func (p *ClaudeCodeProvider) Name() string { return "claude-code" }

func (p *ClaudeCodeProvider) Spawn(ctx context.Context, cfg SpawnConfig) (Session, error) {
	// ⑧ (mem_2ruSlrHR, measured on 2.1.220): cc rejects --session-id together
	// with --resume unless --fork-session is also passed, exiting 1 before it
	// emits anything. Caught here rather than downstream because that exit looks
	// EXACTLY like a failed resume (no init, SessionID() == ""), so the caller's
	// fallback would quietly paper over the bug by spawning a fresh session and
	// the operator would only see "context was lost, sometimes". Failing the
	// spawn names the real cause.
	if cfg.SessionID != "" && cfg.ResumeSessionID != "" {
		return nil, fmt.Errorf(
			"spawn cc: SessionID (%s) and ResumeSessionID (%s) are mutually exclusive: "+
				"--session-id is fresh-spawn-only, reconnect passes --resume alone",
			cfg.SessionID, cfg.ResumeSessionID)
	}

	args := []string{
		"--print",
		"--output-format", "stream-json",
		"--input-format", "stream-json",
		"--verbose",
		// Emit token-level text deltas via `stream_event` lines (D-05a §3).
		// Without this, `assistant` arrives as one complete block, which the UI
		// renders as a sudden full-message reveal instead of streaming.
		"--include-partial-messages",
		// Force "default" permission mode so PreToolUse hooks ALWAYS fire,
		// regardless of user's ~/.claude/settings.json `defaultMode` setting.
		// Without this, users with `defaultMode: "auto"` or `bypassPermissions`
		// silently skip tether's permission UI.
		"--permission-mode", "default",
	}
	// Exactly one of these can be set (guarded above). Reconnect resumes an
	// existing transcript; a fresh spawn pins the id the daemon minted so the
	// tether sid, the cc sid and the on-disk transcript name are the same string
	// from the very first byte — no "listen for init and hope" step (tether#50).
	switch {
	case cfg.ResumeSessionID != "":
		args = append(args, "--resume", cfg.ResumeSessionID)
	case cfg.SessionID != "":
		args = append(args, "--session-id", cfg.SessionID)
	}

	cmd := exec.CommandContext(ctx, p.ccPath, args...)
	// cc's on-disk conversation and file edits are cwd-scoped, so the
	// subprocess must run in the workspace directory, not wherever the
	// daemon happened to start (tether#51).
	cmd.Dir = ResolveWorkdir(cfg.Workdir)
	cmd.Env = buildEnv(cfg.Env)
	cmd.Stderr = p.stderrSink()

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("stdout pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start cc: %w", err)
	}

	enc := json.NewEncoder(stdin)
	enc.SetEscapeHTML(false)

	sess := &ccSession{
		cmd:         cmd,
		ctx:         ctx,
		stdin:       stdin,
		stdout:      stdout,
		enc:         enc,
		events:      make(chan Event, 64),
		sidReady:    make(chan struct{}),
		done:        make(chan struct{}),
		maxLine:     p.maxLine,
		stdoutGrace: p.resolveStdoutGrace(),
	}
	go sess.readLoop(bufio.NewScanner(stdout))
	// Started here rather than lazily because the thing it guards against —
	// readLoop never reaching EOF — is indistinguishable from a healthy idle
	// session until it is too late to start watching. Costs one parked goroutine
	// per session, released the moment readLoop returns.
	go sess.guardStdout()
	return sess, nil
}

// resolveStdoutGrace resolves the guard's grace period. An unset (0) override
// means defaultStdoutGrace, so a ClaudeCodeProvider built the way production
// builds it — NewClaudeCodeProvider(path) with no options — gets the default.
func (p *ClaudeCodeProvider) resolveStdoutGrace() time.Duration {
	if p.stdoutGrace > 0 {
		return p.stdoutGrace
	}
	return defaultStdoutGrace
}

// buildEnv constructs the subprocess environment. IS_SANDBOX=1 is injected
// when running as root (D-05a §2 fact 5). Extra env vars are appended after.
func buildEnv(extra []string) []string {
	env := os.Environ()
	if os.Geteuid() == 0 {
		env = append(env, "IS_SANDBOX=1")
	}
	return append(env, extra...)
}

// ccSession is a live ClaudeCode stream-json session.
type ccSession struct {
	cmd   *exec.Cmd
	ctx   context.Context // Spawn ctx; escape for the blocking terminal-event send
	stdin interface{ Close() error }
	// stdout is the PARENT's read end of cc's stdout pipe — the very file
	// readLoop's scanner reads. Held only so guardStdout can force it closed;
	// nothing else in this package closes it (cmd.Wait does, at the end of
	// Close, which is exactly the path guardStdout exists to make reachable).
	stdout   io.Closer
	enc      *json.Encoder
	events   chan Event
	mu       sync.Mutex
	sid      string
	sidReady chan struct{}
	sidOnce  sync.Once
	done     chan struct{} // closed when readLoop returns (cc process fully exited)
	reqSeq   int           // control_request id counter (T9 pause/interrupt), guarded by mu
	// maxLine caps one stream-json line; 0 means scanBufMax. See withMaxLine.
	maxLine int
	// stdoutGrace is guardStdout's post-cancellation grace period. Always
	// resolved (never 0) for a session Spawn built.
	stdoutGrace time.Duration
	// closeOnce/closeErr make Close idempotent — see Close for why it has to be.
	closeOnce sync.Once
	closeErr  error
	// runSeq/runOpen carry the Event.RunID contract for this provider (tether#148).
	// Plain fields with NO guard, deliberately, and the enumeration is the whole
	// argument: they are WRITTEN only by openRun/closeRun, which are called only
	// from parseLine, which is called only from readLoop's scan loop. The one other
	// reader is abandon — a read and nothing more, which is the point of it (see
	// there) — and readLoop calls that straight-line after the same loop. One
	// goroutine, so a mutex or an atomic here would be documenting a concurrency
	// story that does not exist. A future write from anywhere else invalidates this
	// paragraph, not just extends it. See openRun for what the fields mean.
	runSeq  int64
	runOpen bool
}

// openRun returns the id of the run this stream position belongs to, starting a
// new one if the previous run has already reported its end (tether#148).
//
// # Why the boundary is "the first event after a turn-closer" and not "init"
//
// cc gives no correlation id: nothing on a `result` line says which prompt it
// answers. What the stream does give is ORDER, and one turn's events never
// interleave with another's — cc finishes a turn before starting the next, which
// is what the tether#83 measurement in Entry.turnsInFlight records ("that turn's
// `result` at 19209ms; only then a fresh system/init at 19246ms"). So a run
// boundary is derivable: the first event after a turn-closer opens the next run.
//
// `system/init` is the obvious candidate boundary and is deliberately NOT used.
// cc re-emitting init per turn is cc's behaviour, not tether's contract with it,
// and a turn that emitted no init would then share the previous turn's id — which
// would make fanOut refuse a legitimate turn-closer and freeze the row on
// "working" for the rest of the session, the exact failure tether#103 exists to
// prevent. Advancing on ANY event cannot do that: two turn-closers can never
// collide, because the first one closes the run and the second one opens a fresh
// id even with nothing in between.
//
// What this buys over "a fresh id per turn-closer" is abandon — see there.
func (s *ccSession) openRun() int64 {
	if !s.runOpen {
		s.runSeq++
		s.runOpen = true
	}
	return s.runSeq
}

// closeRun returns this run's id and records that it has now reported its end, so
// the next event opens a new run.
//
// For the turn-closers parseLine emits, which today is the `result` line and only
// that. cc's other terminal event, abandon's EventError, deliberately does NOT come
// through here — it is the stream dying rather than a run reporting, and reusing the
// id without closing anything is exactly what makes its three cases come out right.
func (s *ccSession) closeRun() int64 {
	id := s.openRun()
	s.runOpen = false
	return id
}

// SessionID blocks until cc emits system/init (which resolves s.sid) OR the
// session dies first — a cc that exits before ever emitting init (e.g. a failed
// `--resume`, or a fresh cc that crashes on startup) would otherwise park this
// caller forever, since sidReady never closes. Selecting on `done` too lets it
// return "" on premature death so the caller can surface an error / spawn fresh
// instead of hanging the turn (tether#49).
func (s *ccSession) SessionID() string {
	select {
	case <-s.sidReady:
	case <-s.done:
	}
	return s.sid
}

// Alive reports whether the cc subprocess is still reading its stdin, by polling
// the same `done` channel SessionID() selects on (tether#49). No new state: done
// is closed by readLoop's own defer, i.e. when readLoop has stopped scanning cc's
// stdout — normally because the pipe EOF'd (the process is gone and the stdin
// this session writes prompts to has no reader), and also on a scanner error such
// as a line past the 100MB cap, which is equally terminal for this session
// because nothing restarts readLoop.
//
// The poll cannot block: a select with a default over an already-created channel
// touches no lock, no syscall and no I/O, so it answers in the caller's own
// goroutine no matter what state cc is in — the property tether#55 needs, since
// this runs on the reconnect path whose failure mode is a hang.
//
// One consequence worth stating rather than discovering: done closes when
// readLoop NOTICES the exit, not at the instant of exit, so there is a
// scheduler-wakeup-sized window in which a dead cc still reports Alive. That
// window is inherent to any non-blocking answer and is not the window tether#55
// is about — the reported one is between done closing (readLoop returned) and
// Registry.fanOut's deferred evict draining the buffered events, which this
// closes completely because done closes BEFORE events (defer order in readLoop:
// close(events) is registered first, so it runs last). That defer order is a
// hand-checked invariant, NOT one the tests pin — swapping the two defers leaves
// the suite green, because the two closes are adjacent enough that an observer
// almost never lands between them. Keep them in that order anyway: it is what
// makes "Events() closed ⟹ Alive() already false" true for fanOut.
func (s *ccSession) Alive() bool {
	select {
	case <-s.done:
		return false
	default:
		return true
	}
}

func (s *ccSession) SendPrompt(_ context.Context, text string) error {
	msg := map[string]any{
		"type": "user",
		"message": map[string]any{
			"role":    "user",
			"content": text,
		},
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.enc.Encode(msg)
}

func (s *ccSession) Events() <-chan Event { return s.events }

// Interrupt aborts the in-flight turn WITHOUT killing the cc subprocess
// (tether#8 T9). Earlier this sent SIGINT to s.cmd.Process, which cc treats
// like a Ctrl-C from a real terminal — acceptable for a bare `claude` REPL,
// but tether drives cc with `--input-format stream-json` and holds its
// stdin open across turns (see Spawn/SendPrompt above), so the process is
// meant to stay alive and resumable across the whole session lifetime, not
// just the current turn. Instead we write a stream-json control_request on
// the same mu-guarded encoder SendPrompt uses:
//
//	{"type":"control_request","request_id":"<unique>","request":{"subtype":"interrupt"}}
//
// cc aborts the current turn and stays running; the caller (Registry.
// InterruptSession) treats the session as immediately resumable via a
// subsequent SendPrompt — no respawn, no --resume flag needed.
//
// request_id only needs to be unique per-session (cc doesn't require a
// global namespace); a mutex-guarded counter avoids pulling in time/rand
// for something this local. We do not wait for the matching
// control_response — this is fire-and-remember, like SendPrompt; readLoop
// (see parseLine) recognizes and drops the control_response line so it
// never surfaces as a spurious event.
//
// VERIFY: this shape is confirmed against tether v1's production
// implementation (v0/internal/backend/claude/message.go
// OutboundControlRequest + session.go SendInterrupt, which shipped this
// exact interrupt flow), not against upstream `claude` CLI docs directly —
// re-confirm against the pinned cc version if this ever misbehaves.
func (s *ccSession) Interrupt() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reqSeq++
	req := map[string]any{
		"type":       "control_request",
		"request_id": fmt.Sprintf("tether-interrupt-%d", s.reqSeq),
		"request": map[string]any{
			"subtype": "interrupt",
		},
	}
	return s.enc.Encode(req)
}

// Close reaps the cc subprocess. It closes the stdin this session writes prompts
// to and then waits for the process, which is what releases the three things a
// session otherwise leaves behind for the whole life of the daemon (tether#56):
// the `[claude] <defunct>` zombie, the stdin fd, and the goroutine
// exec.CommandContext starts to watch ctx — that one parks on an internal
// unbuffered channel whose only receiver is inside Wait, so with no cmd.WaitDelay
// set (tether sets none) it is Wait, and nothing else, that lets it finish. Note
// that a ctx cancellation does NOT release it on its own; it merely moves it from
// one park to the next.
//
// tether sets no WaitDelay by decision rather than omission, and setting one
// would not change the sentence above: for a cmd whose stdio is caller-owned
// pipes, WaitDelay is measurably inert, and even where it fires it still leaves
// that watchdog goroutine parked on a channel only Wait receives from.
// guardStdout writes out why, and implements the behaviour WaitDelay would
// otherwise have provided (tether#62).
//
// # Only safe once every read of cc's stdout has finished
//
// os/exec documents cmd.Wait as incorrect to call while a read from StdoutPipe
// is still in flight — Wait closes that pipe, out from under whoever is reading
// it. readLoop is the sole reader, so each caller has to establish that readLoop
// has stopped scanning BEFORE calling this. The two production callers do, by
// different routes, and both spell the argument out at the call site:
//
//   - Registry.teardown (internal/session/registry.go), from fanOut's defer:
//     Events() has closed and drained, and close(s.events) is readLoop's LAST
//     deferred statement.
//   - Attachment.resolve (internal/session/attach.go), on the failed-resume
//     path: SessionID() returned "" only because `done` closed, and
//     close(s.done) is likewise a readLoop defer.
//
// A third caller owes the same argument. "The process looks dead" is NOT it:
// readLoop can still be draining bytes the pipe buffered before the process
// exited, which is exactly the window the os/exec warning is about.
//
// # Idempotent on purpose
//
// The two callers above are not mutually exclusive — a failed resume reaps
// eagerly from the serveChat goroutine while that same entry's fanOut is
// independently unwinding toward its teardown defer — and cmd.Wait is not
// re-entrant: a second call answers "exec: Wait was already called" and cannot
// re-report the exit status. Guarding it here rather than asking every caller to
// coordinate keeps the reap at exactly one per session and gives every caller
// the same answer, instead of handing the loser an error about bookkeeping it
// did not do.
func (s *ccSession) Close() error {
	s.closeOnce.Do(func() {
		_ = s.stdin.Close()
		s.closeErr = s.cmd.Wait()
	})
	return s.closeErr
}

// emit sends ev to s.events. Terminal events (isTerminal) block until delivered
// so a full buffer can't drop them — a lost EventResult/EventError leaves the
// consumer's turn open forever (tether#14). fanOut drains Events() until close,
// so the send always makes progress; s.ctx is the escape when the session is
// torn down (client disconnect cancels ctx, which also kills the subprocess).
// readLoop is the sole sender and closes s.events only after it returns, so
// there is no concurrent send/close here. Non-terminal events (token deltas
// etc.) keep the non-blocking backpressure drop.
func (s *ccSession) emit(ev Event) {
	if isTerminal(ev.Kind) {
		select {
		case s.events <- ev:
		case <-s.ctx.Done():
		}
		return
	}
	select {
	case s.events <- ev:
	default:
	}
}

// readLoop consumes stream-json lines from cc stdout and emits Events.
func (s *ccSession) readLoop(scanner *bufio.Scanner) {
	limit := s.scanMax()
	initial := scanBufInitial
	if limit < initial {
		initial = limit
	}
	scanner.Buffer(make([]byte, initial), limit)
	defer close(s.events)
	// Unblock any SessionID() waiter when the process exits — critical for a cc
	// that dies before emitting system/init (tether#49), where sidReady never
	// closes.
	defer close(s.done)

	for scanner.Scan() {
		// A single stream-json line can yield MULTIPLE events — e.g. a `user`
		// event batching the parallel tool_results for several tool_use blocks
		// (tether#38). Emit each so none is dropped.
		for _, ev := range s.parseLine(scanner.Bytes()) {
			s.emit(ev)
		}
	}
	// Straight-line, so it runs BEFORE the two defers above — which is required,
	// not tidy: close(s.events) is what releases Registry.fanOut into the
	// teardown that reaps this process, and abandon's whole job is to make that
	// reap terminate. The defers themselves stay untouched, and so does the order
	// they run in (Alive() depends on it — see there).
	if err := scanner.Err(); err != nil {
		s.abandon(err)
	}
}

// scanMax resolves the per-line cap: production's scanBufMax unless a test
// shrank it with withMaxLine.
func (s *ccSession) scanMax() int {
	if s.maxLine > 0 {
		return s.maxLine
	}
	return scanBufMax
}

// pid reports the subprocess pid for logging, or 0 if there is no process (a
// hand-built ccSession in a unit test).
func (s *ccSession) pid() int {
	if s.cmd == nil || s.cmd.Process == nil {
		return 0
	}
	return s.cmd.Process.Pid
}

// abandon handles readLoop stopping for a reason OTHER than end-of-stream. Two
// errors can put it here: bufio.ErrTooLong, when cc emits a single line past the
// scanBufMax cap, and the "file already closed" from guardStdout pulling the
// pipe out from under the scan. Either way this session is over — nothing
// restarts readLoop, which is the fact Alive() already documents — but at this
// instant readLoop is the only thing that knows.
//
// # Why it kills the process
//
// The end of this session runs Registry.teardown, whose reap is Close(), which
// is stdin.Close() followed by cmd.Wait(). On an ErrTooLong the child is still
// ALIVE and has just lost the only reader of its stdout: it fills the pipe
// buffer and blocks in write(), while Wait blocks in wait4() waiting for it to
// exit. Neither can move. That deadlock outlasts the turn — it breaks only when
// the connection context finally SIGKILLs the child — and because Close carries
// a sync.Once, a second reaper (Attachment.resolve, on the failed-resume path)
// RENDEZVOUS with the stuck first one and blocks for exactly as long. Killing
// here removes the deadlock's precondition instead of waiting it out: by the time
// anything calls Wait, the child is already on its way out.
//
// cmd.WaitDelay does not substitute, and not only for the shape reason
// guardStdout sets out: WaitDelay's timer does not start until the context is
// done, and the whole point of this case is that the context is still live.
//
// # Ordering
//
// Kill first, then emit. Emitting is the step that can block — a terminal event
// is delivered rather than dropped (see emit) — so emitting first would let a
// slow consumer delay the very kill that unwedges the reap. Both must precede
// readLoop's close(s.events), for the reason stated at the call site.
func (s *ccSession) abandon(err error) {
	// Warn, not Debug: the daemon wires no log level, so Debug is invisible in
	// production, and this line is the only evidence a session ended this way
	// rather than normally.
	slog.Warn("agent stdout read ended in an error; killing the subprocess so it cannot wedge the reap",
		"err", err, "too_long", errors.Is(err, bufio.ErrTooLong), "pid", s.pid())
	if s.cmd != nil && s.cmd.Process != nil {
		// ErrProcessDone when it already exited (the guardStdout route) — the
		// only error worth distinguishing, and nothing would act on it.
		_ = s.cmd.Process.Kill()
	}
	// Tell the consumer, so the turn closes instead of leaving a spinner up: the
	// frontend clears "thinking…" on the error envelope as well as the result one
	// (see isTerminal). This mirrors what the opencode run path already does with
	// its own scan error.
	// s.runSeq directly and NOT openRun (tether#148): the stream dying is not a run
	// of its own, so this must not mint an id. Reading the field gives the right
	// answer in all three states, which is the whole reason openRun advances lazily:
	// a turn still open owns runSeq, so this error closes it; a turn that already
	// reported its result owns runSeq too, so fanOut refuses this one and the
	// non-zero count survives to reach fanOut's end-of-stream arm, which is what
	// makes markTurnInterrupted report the delivery that really was cut off; and a
	// stream that died before any event at all leaves runSeq at zero, which
	// Event.RunID defines as "no run" and fanOut always applies.
	s.emit(Event{Kind: EventError, RunID: s.runSeq, Err: fmt.Errorf("agent output stream ended in an error: %w", err)})
}

// guardStdout force-closes cc's stdout read end once the Spawn context has been
// done for stdoutGrace and readLoop still has not returned. Spawn starts one per
// session; it exits as soon as readLoop finishes, which on an ordinary session
// means before it has done anything at all.
//
// # The cycle it breaks
//
// readLoop's only exit is scanner.Scan() returning false, and for a pipe that
// needs EOF, and EOF needs EVERY write end closed — not just cc's. A cc that
// forks a child which inherits fd 1 and outlives it therefore leaves this
// session's Scan() parked after cc itself is gone, and the entire teardown chain
// parks behind it: `done` never closes (so Alive() keeps answering true and
// SessionID() waiters stay parked), `events` never closes (so Registry.fanOut
// never returns), so fanOut's deferred teardown never runs — the registry entry
// is never evicted and the process is never reaped. Permanently: cancelling the
// context does not break it either, because the only thing that closes this pipe
// from os/exec's side is cmd.Wait, and Wait is reached only through that same
// teardown (tether#62).
//
// # Why cmd.WaitDelay is not this (measured on go1.26.3)
//
// WaitDelay reads like the built-in answer — os/exec offers it for "a child
// process that exits but leaves its I/O pipes unclosed" — and tether v1 did set
// one. It does not work for this cmd, and the reason is the cmd's SHAPE rather
// than the delay's value:
//
//   - both places WaitDelay closes pipes (exec.go's watchCtx and
//     awaitGoroutines) guard the closeDescriptors call behind
//     `c.goroutineErr != nil`, i.e. behind os/exec owning at least one goroutine
//     that COPIES a pipe;
//   - StdinPipe/StdoutPipe hand the pipe to the caller instead — they append to
//     c.parentIOPipes and add no goroutine — and cmd.Stderr is os.Stderr, already
//     an *os.File, so it adds none either. c.goroutine is therefore empty for
//     this cmd, c.goroutineErr stays nil, and neither closeDescriptors call is
//     reachable at all.
//
// Measured on exactly this shape: with an orphan holding the write end and the
// context cancelled, Scan() stays parked identically at WaitDelay=0 and at
// WaitDelay=200ms; what frees the reader in that experiment is cmd.Wait's own
// closing of parentIOPipes on the way out, and Wait is the thing we cannot reach.
// v1's setting was effective because it sat on a `cmd.Output()` call
// (v0/internal/cc/version.go), where c.Stdout is a *bytes.Buffer and the copying
// goroutine WaitDelay needs does exist. Setting WaitDelay on THIS cmd would buy
// nothing and leave behind a comment that lies, so the delay is implemented here
// instead, over the pipe this package actually owns.
//
// # Why a grace period rather than closing at once
//
// A cancelled context SIGKILLs cc, so on the ordinary path the write end closes
// with the process and readLoop reaches EOF within microseconds — before this
// goroutine gets its second look at `done`. Closing the pipe the instant ctx
// fired would instead race readLoop for the bytes the pipe still holds and
// truncate the turn's last events, trading a rare leak for common data loss.
//
// # What this does NOT cover
//
// The arming condition is the CONTEXT being done, not cc being gone, so the
// residual case is a cc that dies (leaving an orphan on fd 1) while the client
// stays connected: nothing here fires, readLoop is still parked, and the entry
// is still neither evicted nor reaped until the client eventually disconnects.
// So this bounds the leak by the connection's lifetime rather than eliminating
// it — which is the whole of the improvement, since before tether#62 not even
// disconnecting helped and the leak lasted as long as the daemon.
//
// Closing that residual needs an independent "the child has exited" signal, and
// there is no cheap correct one here: kill(pid,0) succeeds for the unreaped
// zombie cc becomes, and the obvious alternative — a goroutine that owns Wait
// from the start — is forbidden, because Wait closes the stdout pipe under
// readLoop (the precondition Registry.teardown spells out). Left open
// deliberately: tether#62's own scope review measured the common case (a chat
// session's MCP servers get socketpairs, not this pipe) and found the orphan
// topology does not arise there; the unmeasured remainder is subprocesses of
// cc's own Bash tool.
func (s *ccSession) guardStdout() {
	select {
	case <-s.done:
		return // readLoop finished on its own — every ordinary session
	case <-s.ctx.Done():
	}
	select {
	case <-s.done:
		return // the SIGKILL that cancellation implies got there first
	case <-time.After(s.stdoutGrace):
	}
	// Info, not Debug: Debug is invisible on the real daemon (no log level is
	// wired anywhere), and this line is the only evidence that a session had to
	// be forced rather than ending on its own.
	slog.Info("agent stdout did not reach EOF after the session was cancelled; force-closing "+
		"the pipe so the session can be evicted and the process reaped",
		"grace", s.stdoutGrace, "pid", s.pid())
	// Unblocks the parked Scan with a "file already closed" read error, which
	// takes readLoop into abandon and then into its two defers. Safe to do
	// concurrently with that read: this is an os.File over a pipe, so it is
	// poller-registered and Close evicts the blocked reader rather than racing it.
	//
	// Spawn always sets stdout, so the guard is for a hand-built ccSession — the
	// shape unit tests use — where a panic would land on this goroutine and take
	// the process down rather than failing a test.
	if s.stdout != nil {
		_ = s.stdout.Close()
	}
}

// rawStreamEvent is the top-level stream-json event shape (D-05a §3).
type rawStreamEvent struct {
	Type      string             `json:"type"`
	Subtype   string             `json:"subtype,omitempty"`
	SessionID string             `json:"session_id,omitempty"`
	Message   *rawAssistMsg      `json:"message,omitempty"`
	Result    string             `json:"result,omitempty"`
	Usage     *rawUsage          `json:"usage,omitempty"` // populated on the `result` event (tether#48)
	Event     *rawPartialMessage `json:"event,omitempty"` // populated when type=="stream_event"
}

// rawUsage is cc's per-turn token accounting on the `result` event. cc also
// reports cache_creation_input_tokens / cache_read_input_tokens and
// total_cost_usd here, but tether#48 surfaces only the plain input/output
// counts (owner scope: in/out tokens, no cache detail, no cost).
type rawUsage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

type rawAssistMsg struct {
	Content []rawContentBlock `json:"content"`
}

type rawContentBlock struct {
	Type  string          `json:"type"`
	Text  string          `json:"text,omitempty"`
	ID    string          `json:"id,omitempty"`
	Name  string          `json:"name,omitempty"`
	Input json.RawMessage `json:"input,omitempty"`
	// tool_result block fields (tether#38), present on a `user` event's content[].
	ToolUseID string          `json:"tool_use_id,omitempty"`
	Content   json.RawMessage `json:"content,omitempty"`
	IsError   bool            `json:"is_error,omitempty"`
}

// rawPartialMessage is the Anthropic-native SSE event embedded in stream_event
// lines when --include-partial-messages is on. We care about the
// content_block_delta variants with delta.type=="text_delta" (assistant text)
// and "thinking_delta" (extended-thinking tokens, tether#34).
type rawPartialMessage struct {
	Type  string `json:"type"` // "content_block_delta", "message_start", etc.
	Delta struct {
		Type     string `json:"type"` // "text_delta", "thinking_delta", "input_json_delta", "signature_delta"
		Text     string `json:"text"`
		Thinking string `json:"thinking"` // populated when Type=="thinking_delta"
	} `json:"delta"`
}

// parseLine decodes one stream-json line into zero or more events. It returns a
// slice (not a single *Event) because a `user` / `assistant` message can carry
// several tool_result / tool_use blocks in one line (parallel tool calls), all
// of which must surface (tether#38). nil/empty = nothing to forward.
func (s *ccSession) parseLine(line []byte) []Event {
	var raw rawStreamEvent
	if err := json.Unmarshal(line, &raw); err != nil {
		return nil
	}

	// system/init — captures SessionID once; subsequent inits per turn are
	// metadata refreshes only, NOT new-session boundaries (D-05a §2 fact 2).
	if raw.Type == "system" && raw.Subtype == "init" && raw.SessionID != "" {
		s.sidOnce.Do(func() {
			s.sid = raw.SessionID
			close(s.sidReady)
		})
		return []Event{{Kind: EventInit, RunID: s.openRun(), SessionID: raw.SessionID}}
	}

	// stream_event lines carry token-level deltas (--include-partial-messages).
	// We forward text_delta as EventText and thinking_delta as EventThinking
	// (tether#34). Everything else is skipped: signature deltas for thinking
	// blocks carry no user-visible content; partial JSON for tool_use args is
	// handled via the final `assistant` event below, which carries the
	// complete input.
	if raw.Type == "stream_event" && raw.Event != nil {
		if raw.Event.Type == "content_block_delta" &&
			raw.Event.Delta.Type == "text_delta" &&
			raw.Event.Delta.Text != "" {
			return []Event{{Kind: EventText, RunID: s.openRun(), Text: raw.Event.Delta.Text}}
		}
		// Extended-thinking tokens (tether#34). Forwarded as EventThinking;
		// registry.fanOut routes it through translateEvent (bypassing the
		// fence parser), so it is broadcast to the browser but never
		// accumulated into assistant history — thinking stays ephemeral.
		if raw.Event.Type == "content_block_delta" &&
			raw.Event.Delta.Type == "thinking_delta" &&
			raw.Event.Delta.Thinking != "" {
			return []Event{{Kind: EventThinking, RunID: s.openRun(), Text: raw.Event.Delta.Thinking}}
		}
		return nil
	}

	// `assistant` events arrive after all deltas have streamed. Text blocks are
	// redundant (already streamed via stream_event); only tool_use blocks carry
	// information we haven't emitted yet. An assistant message can hold several
	// tool_use blocks (parallel tool calls) — emit one event per block.
	if raw.Type == "assistant" && raw.Message != nil {
		var evs []Event
		for _, block := range raw.Message.Content {
			if block.Type == "tool_use" {
				// tool_use is a content block, NOT a top-level event (D-05a §3, Risk #4).
				evs = append(evs, Event{
					Kind:  EventToolUse,
					RunID: s.openRun(),
					ToolUse: &ToolUseEvent{
						ID:    block.ID,
						Name:  block.Name,
						Input: block.Input,
					},
				})
			}
		}
		return evs
	}

	// `user` events carry tool_result blocks — the output of a tool cc ran
	// (tether#38). tool_result is a content block, not a top-level event; other
	// user content (e.g. the prompt echo) is not forwarded. The Anthropic
	// Messages API batches ALL results for a set of parallel tool_use calls into
	// ONE user message, so emit one event per tool_result block — returning only
	// the first would silently drop the rest (the common parallel-tools case).
	if raw.Type == "user" && raw.Message != nil {
		var evs []Event
		for _, block := range raw.Message.Content {
			if block.Type == "tool_result" {
				evs = append(evs, Event{
					Kind:  EventToolResult,
					RunID: s.openRun(),
					ToolResult: &ToolResultEvent{
						ToolUseID: block.ToolUseID,
						Content:   toolResultText(block.Content),
						IsError:   block.IsError,
					},
				})
			}
		}
		return evs
	}

	if raw.Type == "result" {
		// Surface the turn's token usage (tether#48) BEFORE the result. The
		// result is the turn-closer (finalizes the frontend turn); emitting usage
		// first means the frontend still has the open turn bubble to attach it to.
		var evs []Event
		if raw.Usage != nil {
			evs = append(evs, Event{Kind: EventUsage, RunID: s.openRun(), Usage: &UsageEvent{
				Input:  raw.Usage.InputTokens,
				Output: raw.Usage.OutputTokens,
			}})
		}
		return append(evs, Event{Kind: EventResult, RunID: s.closeRun(), Text: raw.Result})
	}

	if raw.Type == "rate_limit_event" {
		return []Event{{Kind: EventRateLimit, RunID: s.openRun()}}
	}

	// control_response is cc's reply to a control_request WE sent (currently
	// only the T9 interrupt request from ccSession.Interrupt). It carries no
	// user-visible content and Interrupt() doesn't correlate/await it, so
	// this is intentionally a no-op — matched explicitly (rather than
	// relying on the catch-all `return nil` below) so a future contributor
	// adding a new fallthrough Event doesn't accidentally turn cc's ack into
	// a spurious EventError/bad event in the chat stream.
	if raw.Type == "control_response" {
		return nil
	}

	return nil
}

// toolResultText flattens a tool_result block's `content` to plain text. cc
// sends it either as a JSON string or as an array of content blocks
// ([{type:"text",text:"…"}]) — handle both; anything else yields "" (tether#38).
func toolResultText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &blocks); err == nil {
		var out string
		for _, b := range blocks {
			out += b.Text
		}
		return out
	}
	return ""
}

// Stub providers for future providers (D-17a §5).

// CursorProvider — stub; see D-17a §5.2.
type CursorProvider struct{}

func (CursorProvider) Name() string { return "cursor" }
func (CursorProvider) Spawn(_ context.Context, _ SpawnConfig) (Session, error) {
	return nil, fmt.Errorf("cursor provider: not yet implemented")
}
