package agent

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// OpenCodeProvider implements AgentProvider for opencode (D-17a A2).
// It starts a long-lived `opencode serve` child process and subscribes to its
// SSE /global/event stream for token-level streaming (message.part.delta).
// Prompts are delivered via `opencode run --attach <url> --format json`.
type OpenCodeProvider struct{}

func NewOpenCodeProvider() *OpenCodeProvider { return &OpenCodeProvider{} }

func (p *OpenCodeProvider) Name() string { return "opencode" }

func (p *OpenCodeProvider) Spawn(ctx context.Context, cfg SpawnConfig) (Session, error) {
	workdir := ResolveWorkdir(cfg.Workdir)

	sess := &opencodeSession{
		workdir:   workdir,
		env:       cfg.Env,
		spawnCtx:  ctx,
		events:    make(chan Event, 64),
		sidCh:     make(chan struct{}),
		resumeSID: cfg.ResumeSessionID,
	}

	if err := sess.startServe(ctx); err != nil {
		return nil, err
	}

	// When the session context ends, release any SessionID() waiters and close
	// the event stream. Each per-serve sseLoop deliberately does NOT own these
	// (a hibernated/relaunched serve must not tear down the session's event
	// stream), so this single session-scoped goroutine covers the ctx-cancel
	// teardown paths that don't go through Close(). A serve that dies without
	// being asked to is the remaining path, and belongs to watchServeExit — it
	// is per-incarnation, so it cannot be folded in here.
	go func() {
		<-ctx.Done()
		sess.unblockSID()
		sess.closeEvents()
	}()

	return sess, nil
}

// startServe launches (or relaunches) the `opencode serve` child on a fresh
// port and starts a per-incarnation SSE loop bound to it. Called once by Spawn
// and again by SendPrompt after Interrupt() hibernated the serve. Each call
// installs a fresh serveExitDone channel and sseCancel so the previous
// incarnation's goroutines are fully superseded.
func (s *opencodeSession) startServe(ctx context.Context) error {
	// Pick a random free TCP port for the opencode server.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return fmt.Errorf("opencode: find free port: %w", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()

	baseURL := fmt.Sprintf("http://127.0.0.1:%d", port)

	serve := exec.CommandContext(ctx, "opencode", "serve", "--port", fmt.Sprint(port))
	serve.Dir = s.workdir
	serve.Env = buildEnv(s.env)
	serve.Stderr = os.Stderr
	if err := serve.Start(); err != nil {
		return fmt.Errorf("opencode serve: %w", err)
	}

	// Per-serve SSE loop cancel. Its own cancel lets Interrupt()/Close() stop
	// just this incarnation without spinning on a dead port after the serve is
	// killed; the next relaunch starts a fresh loop bound to the new port. It is
	// created up here, before the loop it stops, only so the exit watcher below
	// can hold it — an unexpected death has to stop the loop too.
	sseCtx, sseCancel := context.WithCancel(ctx)

	// armed/live hand this incarnation over to the exit watcher; see
	// watchServeExit for what each one means and why they are per-incarnation
	// rather than session-scoped.
	armed := new(atomic.Bool)
	live := make(chan struct{})

	// One goroutine owns serve.Wait() for this incarnation; waitReady checks
	// exitDone non-blocking, Close()/Interrupt() read it blocking. Catches the
	// TOCTOU port-grab window between net.Listen() and opencode binding — if the
	// port was stolen, opencode exits ~immediately and we surface a clear error.
	exitDone := make(chan struct{})
	go s.watchServeExit(serve, exitDone, live, armed, sseCancel)
	// EVERY return below this point must release the watcher, which parks on
	// `live` holding this incarnation's teardown decision. Deferred rather than
	// written out per-path so a future early return cannot leak the goroutine.
	defer close(live)

	s.mu.Lock()
	s.baseURL = baseURL
	s.serve = serve
	s.serveExitDone = exitDone
	s.serveArmed = armed
	s.sseCancel = sseCancel
	s.mu.Unlock()

	if err := s.waitReady(ctx, baseURL, exitDone, 10*time.Second); err != nil {
		// Leave `armed` false. A serve that never came up is startServe's OWN
		// failure and is reported by this error return — which SendPrompt's
		// relaunch path turns into an EventError. Letting the watcher end the
		// session here instead would close the event stream first and swallow
		// that error, hanging the consumer's turn: the exact symptom this file
		// exists to prevent. The session stays alive and dormant, so the next
		// prompt retries the relaunch.
		//
		// The fields published above are deliberately left pointing at this dead
		// incarnation. Every one of them is a no-op against a reaped process
		// (Kill returns ErrProcessDone, exitDone is closed, sseCancel and armed are
		// already spent), and `dormant` stays true, so the next SendPrompt runs
		// startServe again and replaces the lot.
		sseCancel()
		_ = serve.Process.Kill()
		<-exitDone
		return fmt.Errorf("opencode serve not ready: %w", err)
	}

	go s.sseLoop(sseCtx, baseURL)

	// This incarnation is live: from here on nobody is checking on it except the
	// watcher, so arm it. The store lands before the deferred close(live), which
	// is what makes the handoff race-free — a serve that died between waitReady's
	// last successful health poll and this line is still torn down, because the
	// watcher cannot read `armed` until startServe has finished deciding.
	armed.Store(true)
	return nil
}

// watchServeExit owns serve.Wait() for ONE serve incarnation and decides whether
// that incarnation's exit ends the SESSION.
//
// # Why the distinction is the entire point
//
// A session outlives any single serve: Interrupt() kills one deliberately and
// SendPrompt relaunches another against the same on-disk conversation, so "no
// serve process" is a perfectly alive session. Before tether#58 this goroutine
// therefore did nothing but record the exit — which left a serve that died on
// its OWN (crash, OOM, an outside kill, a port stolen after startup) with nobody
// to call closeEvents. The consequences ran the whole way down: the event stream
// never ended, so Registry.fanOut never returned, so its deferred evict never
// ran and the Entry sat in the registry forever; `dead` was never stored, so
// Alive() answered true forever and tether#55's reuse gate adopted the corpse;
// every later prompt then attached to a dead port and serveChat only logged it,
// producing a "thinking…" that never returns. That is strictly worse than the
// bounded registered-but-dead window tether#55 closed, because nothing ever
// ended it.
//
// # Why not the `dormant` flag
//
// `dormant` can be made to work — it is set under mu before Interrupt() kills,
// and lifeMu serialises the killers against the relaunch, so reading it BEFORE
// close(exitDone) would be sound. It was not chosen because it makes the safety
// argument non-local: you have to hold the lifeMu serialisation, the
// set-before-kill ordering, and the read-before-close ordering in your head at
// once, and two of its edges are already slightly wrong in ways that only happen
// to be harmless — it stays true for the whole of a relaunch (SendPrompt clears
// it only after startServe returns), and Close() never sets it, so a Close would
// take the crash branch and duplicate its own teardown (benign, since closeEvents
// is once-guarded). Per-incarnation state answers the per-incarnation question
// directly instead:
//
//   - live is closed by startServe once it has finished with this incarnation.
//     Waiting on it is what makes the verdict race-free: until startServe
//     returns, IT owns the failure and reports one by returning an error, so the
//     watcher must not act. This cannot deadlock — exitDone is already closed
//     before we park here, and exitDone is the only thing startServe waits for.
//   - armed is set by startServe only for an incarnation that went live, and
//     cleared by Interrupt()/Close() before they kill.
//
// So `armed` still true here means "no CALLER asked for this exit" — deliberately
// not "nobody did". The Spawn ctx-cancel path also kills the child, because serve
// is built with exec.CommandContext, and nothing disarms on the way; that exit
// takes the branch below. It is the right outcome by luck rather than by design:
// the teardown is exactly what the ctx-done goroutine in Spawn does anyway, and
// every step of it is idempotent. Worth knowing before treating the branch below
// as proof that a crash occurred.
func (s *opencodeSession) watchServeExit(serve *exec.Cmd, exitDone chan struct{}, live <-chan struct{}, armed *atomic.Bool, sseCancel context.CancelFunc) {
	werr := serve.Wait()
	s.mu.Lock()
	s.serveExitErr = werr
	s.mu.Unlock()
	// Closed before the verdict, never after, so that no killer waiting on
	// exitDone can be held up behind the teardown below.
	close(exitDone)

	<-live
	if !armed.Load() {
		// Asked for: an Interrupt() hibernation, a Close(), or a startup failure
		// startServe is reporting itself. Not the session's end.
		return
	}

	// Unexpected death — end the session.
	// 1. Stop this incarnation's SSE loop. Its ctx derives from the session's, so
	//    without this it reconnects to a dead port every 500ms forever.
	sseCancel()
	// 2. Release SessionID() waiters: a crash before session.created would
	//    otherwise park them until the whole session ctx is cancelled.
	s.unblockSID()
	// 3. Finalize the in-flight turn BEFORE closing the stream, because closing it
	//    is the point after which nothing can be said: emit drops every event once
	//    `closed` is set, terminal ones included (the `if s.closed` return precedes
	//    the isTerminal branch). Nothing downstream would cover for us — evict only
	//    deletes registry keys and does not close subscriber channels, so without
	//    this a mid-turn crash leaves the browser on "thinking…" until its
	//    transport dies. That is the symptom this whole file is about, and honest
	//    liveness alone does not cure it: Alive() is only consulted on the NEXT
	//    attach, so it saves the next turn, not this one.
	//
	//    Blocking here is safe, and for the same reason emit's terminal branch is
	//    safe in general: fanOut is still ranging over Events() at this instant
	//    precisely because we have not closed it yet, so the send makes progress.
	//    We also hold no lock — closeEvents takes eventsMu after emit released it.
	msg := "opencode serve exited unexpectedly"
	if werr != nil {
		msg += ": " + werr.Error()
	}
	s.emit(Event{Kind: EventError, Err: errors.New(msg)})
	// 4. End the stream. This is what turns Alive() false and lets fanOut's
	//    deferred evict drop the registry entry.
	s.closeEvents()
}

// waitReady polls /global/health until the serve subprocess is responsive,
// the deadline elapses, or the subprocess exits early.
func (s *opencodeSession) waitReady(ctx context.Context, baseURL string, exitDone chan struct{}, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	url := baseURL + "/global/health"
	client := &http.Client{Timeout: 500 * time.Millisecond}
	for time.Now().Before(deadline) {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		select {
		case <-exitDone:
			s.mu.Lock()
			werr := s.serveExitErr
			s.mu.Unlock()
			return fmt.Errorf("opencode serve exited during startup (port grab? missing binary?): %w", werr)
		default:
		}
		resp, err := client.Get(url)
		if err == nil && resp.StatusCode == http.StatusOK {
			resp.Body.Close()
			return nil
		}
		if resp != nil {
			resp.Body.Close()
		}
		time.Sleep(200 * time.Millisecond)
	}
	return fmt.Errorf("timeout after %s", timeout)
}

type opencodeSession struct {
	// workdir/env/spawnCtx are captured at Spawn and reused to relaunch the
	// serve after Interrupt(): opencode persists session state to disk, so a
	// fresh serve resumes the same conversation via `--session <sid>`.
	workdir  string
	env      []string
	spawnCtx context.Context

	busy     atomic.Bool
	events   chan Event
	eventsMu sync.RWMutex // guards closed
	closed   bool
	// dead mirrors `closed` for Alive(), which must answer without taking
	// eventsMu. Not redundant state for its own sake: emit() holds eventsMu.RLock
	// while a TERMINAL event blocks on a full events buffer, and Go's RWMutex
	// makes a waiting writer (closeEvents) park every subsequent reader behind
	// it — so an Alive() that read `closed` under RLock could be held up by the
	// consumer's drain rate, which is precisely the blocking a liveness probe is
	// not allowed to do (tether#55). Written only by closeEvents, which is
	// once-guarded, so the flag and the channel close cannot disagree.
	dead atomic.Bool

	// lifeMu serializes serve-lifecycle transitions — the SendPrompt() resume
	// swap, Interrupt(), and Close() — against each other. SendPrompt runs on
	// the chat-stream goroutine while Interrupt runs on the control-channel
	// goroutine (registry.InterruptSession), so without this the read-dormant →
	// startServe → clear-dormant sequence could interleave with Interrupt's
	// kill → set-dormant and clobber state / orphan a fresh serve. Held across
	// the (blocking) kill+wait and relaunch; never held with s.mu.
	lifeMu sync.Mutex

	// mu guards every field below: the serve incarnation (baseURL/serve/
	// serveExitErr/serveExitDone/serveArmed/sseCancel) is replaced across
	// Interrupt()+SendPrompt() restarts, and dormant/curRunCancel/sid are
	// touched from both the caller and the run goroutine.
	mu           sync.RWMutex
	sid          string
	baseURL      string
	serve        *exec.Cmd
	serveExitErr error
	// serveArmed is the CURRENT incarnation's exit-watcher arming flag — mu
	// guards the pointer (which incarnation), the atomic guards the value (who
	// won the race between our kill and its death). Interrupt()/Close() clear it
	// before killing so a deliberate exit is not mistaken for a crash; see
	// watchServeExit.
	serveArmed    *atomic.Bool
	serveExitDone chan struct{}
	sseCancel     context.CancelFunc
	dormant       bool
	curRunCancel  context.CancelFunc

	sidCh     chan struct{}
	sidOnce   sync.Once
	resumeSID string
}

// emit safely sends ev to s.events; drops if the channel has been closed by
// closeEvents, preventing the closed-channel send panic.
//
// Terminal events (isTerminal) block until delivered instead of being dropped:
// a lost EventResult/EventError leaves the consumer's turn open forever
// (tether#14). This is safe against a permanent hang because fanOut drains
// Events() unconditionally until close, so the send always makes progress; the
// spawnCtx guard is the escape when the session is torn down mid-send (the
// same ctx the Spawn teardown goroutine uses to call closeEvents, so on cancel
// this returns and releases eventsMu before closeEvents needs the write lock —
// no deadlock). Non-terminal events keep the non-blocking backpressure drop.
func (s *opencodeSession) emit(ev Event) {
	s.eventsMu.RLock()
	defer s.eventsMu.RUnlock()
	if s.closed {
		return
	}
	if isTerminal(ev.Kind) {
		select {
		case s.events <- ev:
		case <-s.spawnCtx.Done():
		}
		return
	}
	select {
	case s.events <- ev:
	default:
	}
}

// closeEvents marks the session closed and shuts events down exactly once.
// Must be the sole caller of close(s.events).
func (s *opencodeSession) closeEvents() {
	s.eventsMu.Lock()
	defer s.eventsMu.Unlock()
	if s.closed {
		return
	}
	s.closed = true
	// These two statements are ordered, and the order is an invariant no test
	// pins: a consumer ranging over Events() observes the close without holding
	// eventsMu, so if the store came second there would be a window where the
	// stream is visibly over and Alive() still says true — the exact
	// registered-but-dead illusion tether#55 removes. Reversing them keeps the
	// suite green (measured), so KEEP THESE ADJACENT AND IN THIS ORDER; what the
	// tests do pin is that the store happens at all.
	s.dead.Store(true)
	close(s.events)
}

// Alive reports whether this session's event stream is still open. See
// agent.Session.Alive for the contract; the note there about NOT probing the OS
// matters most here, because Interrupt() deliberately kills the `opencode serve`
// child and marks the session dormant while SendPrompt is still able to relaunch
// it and continue the same conversation — a dormant session has no process and is
// entirely alive. The stream close is the only event that ends a session, which
// is why the flag lives in closeEvents. Three things reach it: Close(), the Spawn
// ctx-done teardown, and — since tether#58 — watchServeExit noticing a serve that
// died without being asked to. That third one is still not a process probe: it is
// the session's own lifecycle learning that this incarnation will never produce
// another event, which is precisely what a dormant serve has not done.
func (s *opencodeSession) Alive() bool { return !s.dead.Load() }

// unblockSID closes s.sidCh exactly once. Use to either publish a real sid
// (after also setting s.sid under s.mu) or release waiters when opencode
// exited before emitting session.created (SessionID() then returns "").
func (s *opencodeSession) unblockSID() {
	s.sidOnce.Do(func() {
		close(s.sidCh)
	})
}

func (s *opencodeSession) SessionID() string {
	<-s.sidCh
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.sid
}

func (s *opencodeSession) Events() <-chan Event { return s.events }

func (s *opencodeSession) SendPrompt(ctx context.Context, text string) error {
	if !s.busy.CompareAndSwap(false, true) {
		s.emit(Event{Kind: EventError, Err: fmt.Errorf("busy: another prompt is running")})
		return nil
	}

	// If a prior Interrupt() hibernated the serve, bring it back before running.
	// opencode persisted the session to disk, so `--session <sid>` (appended
	// below) resumes the same conversation on the fresh serve. lifeMu makes the
	// read-dormant → startServe → clear-dormant swap atomic against a concurrent
	// Interrupt()/Close() (see lifeMu doc).
	s.lifeMu.Lock()
	s.mu.RLock()
	dormant := s.dormant
	s.mu.RUnlock()
	if dormant {
		if err := s.startServe(s.spawnCtx); err != nil {
			s.lifeMu.Unlock()
			s.busy.Store(false)
			s.emit(Event{Kind: EventError, Err: fmt.Errorf("opencode resume serve: %w", err)})
			return nil
		}
		s.mu.Lock()
		s.dormant = false
		s.mu.Unlock()
	}
	s.lifeMu.Unlock()

	// Derive a cancelable ctx for this run so Interrupt() can stop the client
	// cleanly: cancellation suppresses the run's exit error (see below) and the
	// goroutine still emits EventResult to close the turn.
	runCtx, runCancel := context.WithCancel(ctx)
	s.mu.Lock()
	s.curRunCancel = runCancel
	s.mu.Unlock()

	go func() {
		defer s.busy.Store(false)
		// Always release SessionID() waiters when this goroutine exits — if
		// opencode crashed before emitting session.created, sidCh would
		// otherwise stay open forever.
		defer s.unblockSID()
		defer func() {
			s.mu.Lock()
			s.curRunCancel = nil
			s.mu.Unlock()
			runCancel()
		}()

		s.mu.RLock()
		baseURL := s.baseURL
		args := []string{"run", "--attach", baseURL, "--format", "json"}
		if s.sid != "" {
			args = append(args, "--session", s.sid)
		} else if s.resumeSID != "" {
			args = append(args, "--session", s.resumeSID)
		}
		s.mu.RUnlock()
		args = append(args, text)

		cmd := exec.CommandContext(runCtx, "opencode", args...)
		cmd.Dir = s.workdir
		cmd.Env = buildEnv(nil)
		cmd.Stderr = os.Stderr

		stdout, err := cmd.StdoutPipe()
		if err != nil {
			s.emit(Event{Kind: EventError, Err: fmt.Errorf("opencode run: stdout pipe: %w", err)})
			return
		}
		if err := cmd.Start(); err != nil {
			s.emit(Event{Kind: EventError, Err: fmt.Errorf("opencode run: start: %w", err)})
			return
		}

		// Parse run output to capture session ID and detect step_finish.
		sc := bufio.NewScanner(stdout)
		sc.Buffer(make([]byte, 1<<16), 1<<20)
		for sc.Scan() {
			var ev struct {
				Type      string `json:"type"`
				SessionID string `json:"sessionID"`
			}
			if err := json.Unmarshal(sc.Bytes(), &ev); err != nil {
				continue
			}
			if ev.SessionID != "" {
				s.sidOnce.Do(func() {
					s.mu.Lock()
					s.sid = ev.SessionID
					s.mu.Unlock()
					close(s.sidCh)
				})
			}
		}
		// killedByUs records that the exit status cmd.Wait is about to report is
		// one WE caused, on a path where neither context is cancelled — so the
		// usual "was this cancellation?" test below cannot recognise it.
		killedByUs := false
		if err := sc.Err(); err != nil {
			// Same wedge ccSession.abandon describes, same shape (StdoutPipe +
			// a caller-owned scan + cmd.Wait): the scan gave up while the child
			// may still be alive, and the cmd.Wait below then blocks in wait4()
			// while the child blocks in write() on a stdout pipe nobody drains.
			// Kill before the emit, because a terminal event is delivered rather
			// than dropped, so emitting first would let a slow consumer delay
			// the kill (tether#62).
			if cmd.Process != nil {
				_ = cmd.Process.Kill()
				killedByUs = true
			}
			s.emit(Event{Kind: EventError, Err: fmt.Errorf("opencode run: stdout scan: %w", err)})
		}
		// A non-zero exit that is the result of an Interrupt() (runCtx cancelled),
		// session teardown (ctx cancelled) or the scan-error kill just above is
		// expected — don't surface it. Without the killedByUs arm the kill would
		// come back as a SECOND EventError blaming opencode ("run exited: signal:
		// killed") for a signal tether sent, and the two context checks cannot
		// catch it: on that path both contexts are deliberately still live.
		if err := cmd.Wait(); err != nil && !killedByUs && ctx.Err() == nil && runCtx.Err() == nil {
			s.emit(Event{Kind: EventError, Err: fmt.Errorf("opencode run exited: %w", err)})
		}
		// Emit EventResult after opencode run exits (or is interrupted) — closes
		// the turn for the consumer. More reliable than the session.idle SSE
		// (which can fire spuriously before text is done).
		s.emit(Event{Kind: EventResult, Text: "stop"})
	}()
	return nil
}

// Interrupt stops the in-flight turn for an opencode session. Unlike cc — which
// writes a stream-json control_request on a long-lived stdin (see
// ccSession.Interrupt) — opencode's generation is owned by the persistent
// `opencode serve` process and streamed over SSE independently of the
// short-lived `opencode run` client. Killing the run subprocess therefore does
// NOT stop generation: the serve keeps producing tokens and the SSE stream
// keeps flowing (verified empirically, tether#11 — killing the run process,
// even its whole process group, left the serve generating). The only effective
// interrupt is to kill the serve, which owns the work.
//
// The session stays resumable: opencode persists session state to disk, so the
// next SendPrompt relaunches a fresh serve (the dormant path in SendPrompt) and
// continues the same conversation via `--session <sid>`.
func (s *opencodeSession) Interrupt() error {
	// Nothing running -> nothing to interrupt (avoid needlessly hibernating an
	// idle serve).
	if !s.busy.Load() {
		return nil
	}

	// Serialize against a concurrent SendPrompt-resume / Close (see lifeMu doc).
	s.lifeMu.Lock()
	defer s.lifeMu.Unlock()

	s.mu.Lock()
	runCancel := s.curRunCancel
	sseCancel := s.sseCancel
	serve := s.serve
	exitDone := s.serveExitDone
	armed := s.serveArmed
	s.dormant = true
	s.mu.Unlock()

	// 0. Disarm this incarnation's exit watcher FIRST, before anything that can
	//    take time. Hibernating is not the session's end — it stays alive and
	//    relaunchable — so the watcher must not tear it down (watchServeExit).
	//    Ordering is what makes it sound, and doing it here rather than just
	//    before the kill is what keeps the window tight: for as long as this flag
	//    is still set, a serve dying on its own is read as a crash and would end a
	//    session the caller only wanted to hibernate. Nothing below is a
	//    precondition for it (sseCancel cancels a CHILD of the serve's ctx, so it
	//    cannot stop the serve), so there is no reason to wait. The residue —
	//    "the serve died before Interrupt was even called" — is unavoidable, and
	//    is correctly classified as a crash either way.
	if armed != nil {
		armed.Store(false)
	}
	// 1. Cancel the in-flight `opencode run` client so its goroutine winds down
	//    without surfacing a spurious exit error and emits EventResult.
	if runCancel != nil {
		runCancel()
	}
	// 2. Stop this serve's SSE loop so it doesn't spin reconnecting to a port
	//    that is about to die; SendPrompt starts a fresh loop on relaunch.
	if sseCancel != nil {
		sseCancel()
	}
	// 3. Kill the serve — it owns generation, so this actually stops it. The exit
	//    watcher was disarmed in step 0, so this exit hibernates the session
	//    rather than ending it.
	if serve != nil && serve.Process != nil {
		_ = serve.Process.Kill()
		if exitDone != nil {
			<-exitDone
		}
	}
	return nil
}

func (s *opencodeSession) Close() error {
	s.lifeMu.Lock()
	defer s.lifeMu.Unlock()

	s.mu.Lock()
	runCancel := s.curRunCancel
	sseCancel := s.sseCancel
	serve := s.serve
	exitDone := s.serveExitDone
	armed := s.serveArmed
	s.mu.Unlock()

	// Cancel any in-flight `opencode run` client too: killing the serve alone
	// may not make an attached run client exit, which would block its goroutine
	// on cmd.Wait() forever (goroutine leak + busy stuck true).
	if runCancel != nil {
		runCancel()
	}
	if sseCancel != nil {
		sseCancel()
	}
	// Disarm before killing: this exit is asked for, and Close() ends the session
	// itself below. Placement is don't-care here, unlike in Interrupt() — leaving
	// it armed entirely would only duplicate a teardown we are about to do, and
	// every step of that is idempotent. Disarming anyway keeps one meaning for the
	// flag: exactly one party ends an incarnation.
	if armed != nil {
		armed.Store(false)
	}
	if serve != nil && serve.Process != nil {
		_ = serve.Process.Kill()
	}
	if exitDone != nil {
		<-exitDone
	}
	s.closeEvents()

	s.mu.RLock()
	err := s.serveExitErr
	s.mu.RUnlock()
	return err
}

// sseLoop consumes the serve's SSE /global/event stream for one serve
// incarnation, bound to baseURL and stopped via ctx (see startServe). It does
// NOT close the session's event stream on exit — the session outlives any
// single serve incarnation (Interrupt() kills one, SendPrompt relaunches
// another); closeEvents is owned by Close(), the Spawn ctx-done goroutine, and
// watchServeExit. Note the loop cannot detect its own serve's death for us
// anyway: a dead port fails the request, which is indistinguishable from a
// restart in progress, so it retries — which is why the exit watcher, not this
// loop, is what ends the session (tether#58).
func (s *opencodeSession) sseLoop(ctx context.Context, baseURL string) {
	client := &http.Client{}
	url := baseURL + "/global/event"

	for {
		if ctx.Err() != nil {
			return
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return
		}
		req.Header.Set("Accept", "text/event-stream")

		resp, err := client.Do(req)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			time.Sleep(500 * time.Millisecond)
			continue
		}
		s.readSSE(ctx, resp.Body, s.emit)
		resp.Body.Close()

		if ctx.Err() != nil {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
}

type ssePayload struct {
	Payload struct {
		Type       string          `json:"type"`
		Properties json.RawMessage `json:"properties"`
	} `json:"payload"`
}

func (s *opencodeSession) readSSE(ctx context.Context, r io.Reader, emit func(Event)) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 1<<16), 1<<20)

	for sc.Scan() {
		if ctx.Err() != nil {
			return
		}
		line := sc.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		data := strings.TrimPrefix(line, "data: ")

		var wrapper ssePayload
		if err := json.Unmarshal([]byte(data), &wrapper); err != nil {
			continue
		}

		switch wrapper.Payload.Type {
		case "message.part.delta":
			var props struct {
				SessionID string `json:"sessionID"`
				Field     string `json:"field"`
				Delta     string `json:"delta"`
			}
			if err := json.Unmarshal(wrapper.Payload.Properties, &props); err != nil {
				continue
			}
			if props.Field == "text" && props.Delta != "" {
				if props.SessionID != "" {
					s.sidOnce.Do(func() {
						s.mu.Lock()
						s.sid = props.SessionID
						s.mu.Unlock()
						close(s.sidCh)
					})
				}
				emit(Event{Kind: EventText, Text: props.Delta})
			}

		case "session.created":
			var props struct {
				SessionID string `json:"sessionID"`
			}
			if err := json.Unmarshal(wrapper.Payload.Properties, &props); err != nil {
				continue
			}
			if props.SessionID != "" {
				s.sidOnce.Do(func() {
					s.mu.Lock()
					s.sid = props.SessionID
					s.mu.Unlock()
					close(s.sidCh)
				})
				emit(Event{Kind: EventInit, SessionID: props.SessionID})
			}

		case "session.error":
			var props struct {
				Error struct {
					Message string `json:"message"`
				} `json:"error"`
			}
			if err := json.Unmarshal(wrapper.Payload.Properties, &props); err == nil {
				if props.Error.Message != "" {
					emit(Event{Kind: EventError, Err: errors.New(props.Error.Message)})
				}
			}
		}
	}
}
