package session

import (
	"context"
	"fmt"
	"log/slog"
	"sync"

	"github.com/piaobeizu/tether/internal/agent"
	"github.com/piaobeizu/tether/internal/wire"
)

// Attachment is one chat client's binding to an agent session, with the
// try-resume-then-fallback behaviour tether#50 adds.
//
// # Why this type exists
//
// Restoring cc's context on reconnect means passing `--resume <sid>`, and a
// failed `--resume` is only observable AFTER the first prompt has been sent.
// That is not an implementation quirk, it is cc's shape: under
// `--input-format stream-json` cc emits nothing at all until a user message
// arrives, so a SUCCESSFUL resume and a FAILED one look identical at spawn time.
// The discriminator (mem_2ruSlrHR ③, and the `done` channel tether#49 added) is
// that a failed resume exits before system/init, so SessionID() returns "".
//
// Waiting for that signal means the first prompt is already gone — written to
// the stdin of a process that exited without reading it. Recovering therefore
// requires holding onto prompts until the session is CONFIRMED, which is the one
// piece of new state this slice introduces. Putting it in a type of its own,
// rather than inline in serveChat, is what makes the whole path testable without
// standing up a WebTransport session.
//
// # Lifecycle
//
//	att, err := reg.Attach(ctx, sid, provider)   // spawns: resume or fresh
//	att.Subscribe(ch)                            // survives a fallback swap
//	go func() { for … { att.SendPrompt(ctx, t) } }
//	res, err := att.Resolve(ctx)                 // confirms, or falls back
//	att.SetOwner(clientID)
//
// Resolve is idempotent; SendPrompt, Subscribe and SetOwner may be called
// concurrently with it.
type Attachment struct {
	reg      *Registry
	provider string
	// reqSID is the sid the client asked to resume; "" when it brought none.
	// Retained after a fallback so the notice gate can ask whether THAT session
	// had history.
	reqSID string

	mu sync.Mutex
	// entry is the live binding. It is REPLACED (not mutated) by a fallback, so
	// every reader must take it under mu rather than caching it.
	entry *Entry
	// resuming is true while entry was spawned with --resume, i.e. while a
	// fallback is still possible.
	resuming bool
	settled  bool
	// pending holds prompts sent but not yet confirmed delivered, in order, for
	// replay onto the fallback session. Emptied (and never refilled) once settled.
	pending []string
	// subs are the channels Subscribe registered, so a fallback can re-attach
	// them to the new Entry. Without this a recovered session would answer into
	// a void: the subscriber is bound to the Entry, and the Entry is gone.
	subs map[chan wire.Envelope]struct{}

	// resolved is closed once sid is final (success, fallback, or failure), so
	// WaitSID can hand every prompt-recording goroutine the sid the prompt
	// actually landed in.
	resolved chan struct{}
	sid      string

	// resolveOnce makes Resolve idempotent. It guards the WHOLE resolution, not
	// just the bookkeeping: two concurrent Resolve calls would otherwise each
	// observe resuming==true and each run the fallback, leaving an orphaned cc
	// subprocess behind. Only serveChat calls Resolve today, once — but "safe
	// because there is one caller" is a property of the callers, not of this
	// type, and it is one refactor away from being false.
	resolveOnce sync.Once
	res         Resolution
	resErr      error
}

// Resolution is the outcome of Attachment.Resolve — what the browser must be
// told once the session is confirmed.
type Resolution struct {
	// SID is the confirmed session id: the resumed one, the minted one, or the
	// one minted by a fallback. Always non-empty when Resolve returns no error.
	SID string
	// Recovered is true when a --resume attempt failed and Resolve fell back to a
	// brand-new session, i.e. the conversation's context is gone even though the
	// connection succeeded. Consumed for logging and to gate Notice; the browser
	// adopts SID from session_ready either way.
	Recovered bool
	// Notice is true when the user should be TOLD that context was lost:
	// Recovered AND the dead session actually had persisted history. See
	// HistoryStore.HasHistory for why the gate exists.
	Notice bool
}

// Attach binds to sid, or spawns a session to bind to.
//
// Three cases, in order of preference:
//
//   - sid names a session that is registered AND whose agent is still alive →
//     reuse it. cc is still running and still holds the context in memory;
//     nothing to resume. "Still alive" is checked rather than assumed from the
//     registration, which is the tether#55 fix.
//   - sid is non-empty but not live — daemon restarted, entry evicted after a
//     disconnect, or registered-but-dead → ATTEMPT `--resume <sid>`. This is what
//     tether#50 adds; before it, this case spawned a fresh session and cc
//     silently forgot everything.
//   - sid is empty → fresh session under a newly minted id.
//
// KNOWN LIMITS, both rooted in the same thing — an entry is registered under the
// `pending-%p` placeholder and only re-keyed to its real sid once cc emits
// system/init, which under stream-json needs the client's FIRST PROMPT. So the
// window in which a session reads as "not live" is not an instant; it lasts until
// the user types.
//
//  1. Two attaches carrying the same dead sid inside that window both miss and
//     both run `cc --resume <sid>` on one transcript. This is reachable from a
//     SINGLE browser: the sid lives in localStorage, which is shared across tabs
//     of the origin, so two tabs reconnecting after a daemon restart is enough.
//     Since tether#55 a REGISTERED-but-dead sid reaches the same window (both
//     attaches judge it dead and both resume), which is a new route into this
//     limit but not a new limit: an evicted dead sid always behaved this way, and
//     the alternative — keeping the corpse so the second attach "finds" it —
//     is the bug tether#55 fixes.
//     Both then re-key to the same map key, so one Entry silently displaces the
//     other in r.sessions and the displaced cc keeps running unreachable
//     (Registry.evict is by-value and only reclaims the pointer it is given).
//  2. Symmetrically, an attach inside the window can miss a session that is
//     genuinely alive and resume ITS transcript.
//
// Closing both properly means keying the entry under its real sid at spawn time —
// which tether#50 makes possible for the first time, since the id is now known
// before provider.Spawn is called — and retiring the placeholder/re-key/evicted
// machinery that tether#12 hardened. That is a focused change to a
// concurrency-sensitive area, tracked separately rather than bolted onto this
// slice. Deliberately allowing concurrent clients on one transcript would want
// cc's `--fork-session` instead of any of this.
func (r *Registry) Attach(ctx context.Context, sid, providerName string) (*Attachment, error) {
	a := &Attachment{
		reg:      r,
		provider: providerName,
		reqSID:   sid,
		subs:     make(map[chan wire.Envelope]struct{}),
		resolved: make(chan struct{}),
	}

	// liveEntry, not a bare r.sessions lookup: an entry that is still registered
	// is not necessarily an agent that is still running, and adopting a corpse
	// here leaves the turn in "thinking…" forever (tether#55 — see
	// Registry.liveEntry for the mechanism). A sid whose agent has exited now
	// falls through to the `--resume` case below, which is both the right answer
	// and the recoverable one: the transcript is on disk, so resuming usually
	// restores the context the corpse was holding in memory, and if it does not,
	// this attach is fallback-eligible and Resolve replays onto a fresh session.
	//
	// HOW BIG THIS IS, stated plainly because the comment it replaces was not:
	// the window where an entry is registered but its agent has gone is bounded —
	// it opens when the agent's stream closes and shuts when fanOut's deferred
	// evict has drained at most a channel-buffer of events. It is a race a
	// reconnect can lose, not a state the daemon sits in. Closing it is worth
	// doing because the cost of losing it is unbounded (the turn never returns)
	// and the check is free, NOT because it is the only road to a hung turn.
	//
	// STILL NOT COVERED — all three of these produce the same "thinking…" and
	// none of them is this gate's business, because the gate is a decision taken
	// once, not a subscription:
	//
	//  1. An agent that dies AFTER being adopted here, e.g. mid-turn. Reuse stays
	//     non-fallback-eligible (resuming stays false) on purpose: such a session
	//     wants recovering by resuming ITS transcript, whereas the fallback
	//     machinery spawns fresh. Expressing that needs a third attachment state
	//     neither this slice nor tether#50 has. Note serveChat only slog.Warns a
	//     SendPrompt error, so nothing downstream converts it into recovery
	//     either.
	//  2. An agent that is running but WEDGED — cc alive, stdin accepted,
	//     nothing ever emitted (the stale-spawn pitfall). Alive() reports true
	//     and should: no non-blocking check can tell "thinking" from "stuck".
	//  3. opencode specifically: if its `serve` child dies outside Interrupt()/
	//     Close(), nothing calls closeEvents, so the session's stream never ends
	//     and Alive() keeps saying true forever. That is a gap in
	//     opencodeSession's own lifecycle, tracked separately — reporting it here
	//     would mean this gate second-guessing the provider.
	if e, ok := r.liveEntry(sid); ok {
		a.entry = e
		return a, nil
	}

	cfg := agent.SpawnConfig{ResumeSessionID: sid}
	e, err := r.spawnEntry(ctx, providerName, cfg)
	if err != nil {
		return nil, err
	}
	a.entry = e
	a.resuming = sid != ""
	return a, nil
}

// Subscribe registers ch for this attachment's event stream, and keeps
// registering it across a fallback swap.
//
// If a fallback lands between reading the entry and subscribing to it, ch ends up
// on both the dead and the fresh entry; harmless, because the dead entry's fanOut
// has already returned and it is out of r.sessions.
func (a *Attachment) Subscribe(ch chan wire.Envelope) {
	a.mu.Lock()
	a.subs[ch] = struct{}{}
	e := a.entry
	a.mu.Unlock()
	e.Subscribe(ch)
}

// Unsubscribe removes ch from the CURRENT entry and from the replay set.
func (a *Attachment) Unsubscribe(ch chan wire.Envelope) {
	a.mu.Lock()
	delete(a.subs, ch)
	e := a.entry
	a.mu.Unlock()
	e.Unsubscribe(ch)
}

// SetOwner claims this session for clientID, returning false only if a DIFFERENT
// client already owns it.
//
// It resolves ownership against the Entry this attachment holds instead of going
// through Registry.SetOwner's sid lookup, and that is the whole point. The entry
// is keyed under `pending-%p` until the re-key goroutine wakes from SessionID() —
// the same wakeup Resolve races — so a sid-keyed lookup here returns false
// whenever Resolve gets there first, and serveChat treats false as a fatal
// ownership race: it sends an error envelope and drops the connection while the
// user's first answer is still in flight. That is worse on the tether#50 reload
// path than it was before, because session_ready never goes out, so the browser
// keeps the DEAD sid in localStorage and its automatic reconnect repeats the
// resume that just failed — while the prompt that WAS persisted (under the fresh
// sid, via WaitSID) sits in a history file the browser will never ask for.
//
// Going through the pointer sidesteps the map entirely: the attachment already
// knows which Entry it is talking to, so there is nothing to look up and no race
// to lose.
func (a *Attachment) SetOwner(clientID string) bool {
	a.mu.Lock()
	e := a.entry
	a.mu.Unlock()
	return e.setOwner(clientID)
}

// SendPrompt forwards text to the bound session, buffering it for replay while
// the session is still unconfirmed.
//
// The error is returned as-is, and on the failed-resume path it WILL be a broken
// pipe — cc exited without reading stdin. That is expected, not fatal: the
// caller logs it and Resolve replays the buffer onto a fresh session. Treating
// it as fatal here is precisely the tether#49 wedge.
func (a *Attachment) SendPrompt(ctx context.Context, text string) error {
	a.mu.Lock()
	if !a.settled {
		a.pending = append(a.pending, text)
	}
	e := a.entry
	a.mu.Unlock()
	return e.Session().SendPrompt(ctx, text)
}

// WaitSID blocks until the session is settled and returns its final id, or ""
// if the session never produced one. It exists so history records the user's
// prompts under the id they were actually answered in: on a fallback the
// original session's SessionID() is "", and a prompt recorded against "" is
// silently dropped by RecordUserMessage — which is how the replayed first prompt
// would otherwise vanish from the transcript the browser reloads.
func (a *Attachment) WaitSID() string {
	<-a.resolved
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.sid
}

// Resolve waits for the session to confirm itself and, if a resume attempt
// failed, falls back to a fresh session and replays the buffered prompts.
// Idempotent: repeat calls return the first outcome without re-resolving.
func (a *Attachment) Resolve(ctx context.Context) (Resolution, error) {
	a.resolveOnce.Do(func() {
		a.res, a.resErr = a.resolve(ctx)
		// Unblock WaitSID on EVERY exit path, including the error ones — a
		// prompt-recording goroutine parked in WaitSID would otherwise leak for the
		// lifetime of the daemon. Recording against "" is a documented no-op.
		a.mu.Lock()
		a.sid = a.res.SID
		a.settled = true
		a.pending = nil
		a.mu.Unlock()
		close(a.resolved)
	})
	return a.res, a.resErr
}

func (a *Attachment) resolve(ctx context.Context) (Resolution, error) {
	a.mu.Lock()
	e, resuming := a.entry, a.resuming
	a.mu.Unlock()

	// Blocks until system/init lands (the session is real) or the process exits
	// without one (tether#49's done channel), which yields "".
	if sid := e.Session().SessionID(); sid != "" {
		return Resolution{SID: sid}, nil
	}

	// A cancelled connection also kills the subprocess, so it arrives here looking
	// exactly like a failed resume. Distinguishing them matters for one concrete
	// reason: the log line below is the only signal an operator has for "how often
	// do resumes actually fail", and counting every closed tab as a resume failure
	// would make it worthless. There is also nothing worth recovering for a client
	// that has gone away.
	if ctx.Err() != nil {
		return Resolution{}, fmt.Errorf("connection closed before the session confirmed: %w", ctx.Err())
	}

	if !resuming {
		// A FRESH session died before init. There is nothing to fall back to —
		// respawning would just repeat whatever killed it (a missing/broken cc
		// binary, a bad workdir) and could spin. Surface it, exactly as before.
		return Resolution{}, fmt.Errorf("agent exited before emitting session id")
	}

	// The resume failed. Decide about the notice BEFORE spawning, while reqSID is
	// unambiguously the session that just died. A daemon running without a
	// HistoryStore can never notify — it has no way to know a conversation ever
	// existed, so it stays quiet rather than guessing.
	notice := a.reg.History != nil && a.reg.History.HasHistory(a.reqSID)
	slog.Info("chat resume failed, starting a fresh session",
		"requested_sid", a.reqSID, "had_history", notice)

	// Drop the dead entry now rather than waiting for its own fanOut to notice
	// the closed Events() channel, so the dead sid stops reading as live the
	// moment we know it is gone. evict is idempotent and by-value, so the
	// concurrent teardown doing it again is harmless.
	a.reg.evict(e)

	// Reap the corpse. Close() is stdin.Close() + cmd.Wait(), and calling Wait
	// while another goroutine is still reading the process's stdout is a
	// documented os/exec race — so this is only safe because of WHY we are here:
	// SessionID() returned "" only because ccSession's `done` channel closed, and
	// `done` is closed by readLoop's own defer, so readLoop has already returned
	// and every read from that pipe is finished. Skipping it leaks a zombie plus
	// an exec watchdog goroutine per failed resume, and a failed resume is an
	// ordinary reload event rather than a rare one.
	if err := e.Session().Close(); err != nil {
		slog.Debug("reaped the failed-resume subprocess", "requested_sid", a.reqSID, "err", err)
	}

	fresh, err := a.reg.spawnEntry(ctx, a.provider, agent.SpawnConfig{})
	if err != nil {
		return Resolution{}, fmt.Errorf("resume %s failed and fresh spawn failed: %w", a.reqSID, err)
	}

	// Re-point the attachment and move the subscribers over BEFORE replaying, so
	// no event from the replayed turn can be emitted into a void. Events the fresh
	// session emits before this point — only reachable if it dies during startup —
	// do broadcast to nobody; the caller learns about that from the error returned
	// at the bottom of this function, not from an envelope.
	a.mu.Lock()
	a.entry = fresh
	a.resuming = false
	pending := append([]string(nil), a.pending...)
	for ch := range a.subs {
		e.Unsubscribe(ch)
		fresh.Subscribe(ch)
	}
	a.mu.Unlock()

	// Replay every prompt the dead session never got to answer. All of them
	// failed: cc exited without reading its stdin, so anything "written" went
	// into a pipe with no reader. Replaying only the first would leave a prompt
	// that the user sent, that history records, and that nothing ever answers.
	//
	// Done outside the lock so a blocked pipe cannot stall SendPrompt/Subscribe.
	// The cost is that a prompt typed during the replay could reach cc ahead of
	// the replayed ones; that needs a keystroke inside a window measured in
	// microseconds, and the composer clears on send, so it is accepted rather
	// than paid for by holding a mutex across I/O.
	for _, text := range pending {
		if err := fresh.Session().SendPrompt(ctx, text); err != nil {
			return Resolution{}, fmt.Errorf("replay prompt onto fresh session: %w", err)
		}
	}

	sid := fresh.Session().SessionID()
	if sid == "" {
		return Resolution{}, fmt.Errorf("fresh session after failed resume %s exited before emitting session id", a.reqSID)
	}
	return Resolution{SID: sid, Recovered: true, Notice: notice}, nil
}
