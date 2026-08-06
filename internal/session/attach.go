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
//	att, err := reg.Attach(ctx, sid, provider, wsID) // spawns: resume or fresh
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
	// ws is the workspace this attachment's sessions run in — already validated by
	// resolveWorkspace. Carried so the FALLBACK spawns in the same workspace as the
	// resume it replaces; without it, recovering from a failed resume would
	// silently relocate the conversation to the daemon default (tether#52).
	ws WorkspaceBinding
	// rebound records that the sid the client brought was dropped because it
	// belongs to a different workspace (resolveWorkspace row 5). It makes the fresh
	// session report itself as a recovery, so the user is TOLD their context did
	// not come with them instead of finding an unexplained empty transcript.
	rebound bool

	// reopenMu serialises reopen attempts (see reopen). It is separate from mu
	// because a reopen SPAWNS — mu is taken by SendPrompt, Subscribe and Resolve
	// on paths that must not queue behind a subprocess start. Lock order is
	// reopenMu → mu, never the reverse.
	reopenMu sync.Mutex

	mu sync.Mutex
	// entry is the live binding. It is REPLACED (not mutated) by a fallback or by
	// a reopen, so every reader must take it under mu rather than caching it.
	entry *Entry
	// resuming is true while entry was spawned with --resume, i.e. while a
	// fallback is still possible.
	resuming bool
	// reopenSID is the THIRD attachment state (tether#59): non-empty means this
	// attachment REUSED a session that was alive at attach time, and that if that
	// session turns out to be dead it may be recovered by RE-OPENING this very sid
	// — `--resume reopenSID` — rather than by spawning a fresh one.
	//
	// It is not a synonym for `resuming`, and the two are mutually exclusive BY
	// CONSTRUCTION rather than by care: Attach's reuse branch returns before
	// `resuming` is ever assigned, so reopenSID != "" ⟹ resuming == false. That
	// exclusivity is what keeps the two recovery paths from interfering, and they
	// must not be merged because they recover opposite situations. A failed resume
	// means the transcript could not be found, so recovery is a FRESH session (and
	// resuming that sid again would loop). A reused session that dies means the
	// transcript is sitting on disk holding the whole conversation, so recovery is
	// resuming it (and a fresh session would answer the turn while silently
	// throwing the context away — the user keeps their scrollback and their sid,
	// and cc alone has forgotten everything).
	//
	// Written once, in Attach, before the attachment is handed to anyone; read
	// under mu anyway, so that no reader has to know that.
	reopenSID string
	// reopenSpent records that the ONE reopen this attachment is allowed has been
	// used (or attempted). See reopen for why the budget is one.
	reopenSpent bool
	settled     bool
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
	// Recovered is true when the sid the client asked for is NOT the sid that
	// answered, i.e. the connection succeeded but the conversation's context did
	// not come with it. Two ways to get here: a `--resume` attempt failed and
	// Resolve fell back to a brand-new session (tether#50), or the sid belonged to
	// a different workspace and was deliberately dropped (tether#52 — see
	// Rebound). Consumed for logging and to gate Notice; the browser adopts SID
	// from session_ready either way.
	Recovered bool
	// Notice is true when the user should be TOLD that context was lost:
	// Recovered AND the requested session actually had persisted history. See
	// HistoryStore.HasHistory for why the gate exists.
	Notice bool
	// Rebound distinguishes the two Recovered cases, because they need different
	// words. A failed resume means the old conversation is GONE; a rebind means it
	// is intact and still resumable — just not from this workspace. Telling a user
	// their context "could not be restored" when it is sitting safely in workspace A
	// is a lie that would make them stop trusting the notice. serveChat picks the
	// sentence from this flag (tether#52).
	Rebound bool
}

// Attach binds to sid, or spawns a session to bind to, in the workspace named by
// wsID.
//
// # wsID is a workspace ID, never a path (tether#52)
//
// It is resolved here — through Registry.Workspaces, i.e. the user's own
// workspace registry — and an id that is not registered is an ERROR, before
// anything is spawned. This is the only entry point that resolves one, which is
// the point: the agent's cwd is where it reads and writes files, so "which
// directory may a request choose?" has exactly one gate rather than one per
// caller. An empty wsID means "no workspace selected" and keeps the pre-tether#52
// behaviour, the daemon-global Registry.Workdir.
//
// The workspace also decides whether sid may be used at all — see
// resolveWorkspace for the table and for why an existing sid presented under a
// DIFFERENT workspace becomes a fresh session rather than a refusal or a resume
// in the wrong directory.
//
// Three cases, in order of preference:
//
//   - sid names a session that is registered AND whose agent is still alive →
//     reuse it. cc is still running and still holds the context in memory;
//     nothing to resume. "Still alive" is checked rather than assumed from the
//     registration, which is the tether#55 fix. Since tether#59 such an
//     attachment is also REOPENABLE — alive at this instant is not alive for the
//     rest of the turn, so it remembers the sid it reused and re-opens it if a
//     prompt is ever refused (Attachment.reopenSID, Attachment.reopen).
//   - sid is non-empty but not live — daemon restarted, entry evicted after a
//     disconnect, or registered-but-dead → ATTEMPT `--resume <sid>`. This is what
//     tether#50 adds; before it, this case spawned a fresh session and cc
//     silently forgot everything.
//   - sid is empty → fresh session under a newly minted id.
//
// # Concurrent attaches on one sid (tether#54)
//
// An entry is registered under the sid it is resuming (or the one minted for it)
// from before its process exists, so a second attach carrying the same sid FINDS
// the first one through the liveEntry gate above instead of starting a second
// `cc --resume` of the same transcript. That matters more than it sounds: the sid
// lives in localStorage, which tabs of an origin share, so two tabs reconnecting
// after a daemon restart used to be enough to duplicate a resume, silently
// displace one of the two entries in r.sessions, and leave the displaced cc
// running unreachable.
//
// The reused entry may be a resume that has not CONFIRMED yet, and the second
// attach adopts it non-fallback-eligible (resuming stays false), exactly as it
// adopts any other live session. So if that resume then fails, the first attach
// recovers by falling back and replaying, while the second gets "agent exited
// before emitting session id". For the one caller that exists that means a
// reconnect: serveChat surfaces the error and closes, and the browser's automatic
// reconnect resumes, fails, and falls back on its own. A future caller that does
// NOT reconnect would instead LOSE that turn's prompts, which is the sharper way
// to state the trade — accepted because the alternative is both clients writing
// into one transcript in the common case where the resume succeeds. Making the
// second attach fallback-eligible instead would fork the conversation in two,
// which is what cc's `--fork-session` is for and not something to arrive at by
// accident.
//
// RESIDUAL, stated rather than implied — the window is narrowed, not closed:
//
//   - It is now the duration of one provider.Spawn call, rather than "until the
//     user types". For cc that is an exec.Start.
//   - Inside it, both attaches still spawn, and the second registration
//     OVERWRITES the first under the same key — so the displaced entry, and the cc
//     it owns, are exactly as unreachable as before. What changed is how long the
//     door is open, not what is behind it.
//   - Closing it needs a reservation: the key claimed before the process exists,
//     released if the spawn fails, with the loser waiting on the winner's Entry
//     rather than racing it — plus an answer for the loser's already-started
//     subprocess, which cannot simply be Closed (see the os/exec note in resolve).
//     That is a self-contained piece of work, not a line, and it is deliberately
//     not smuggled in here; tether#60 tracks it.
func (r *Registry) Attach(ctx context.Context, sid, providerName, wsID string) (*Attachment, error) {
	// FIRST, before any spawn: an unknown workspace id must cost nothing. Returning
	// here is what makes "the daemon does not start anything in an unintended
	// directory" a property of program order rather than of care downstream.
	dec, err := r.resolveWorkspace(sid, wsID)
	if err != nil {
		return nil, err
	}

	a := &Attachment{
		reg:      r,
		provider: providerName,
		reqSID:   sid,
		ws:       dec.Binding,
		subs:     make(map[chan wire.Envelope]struct{}),
		resolved: make(chan struct{}),
	}
	want := r.workdirFor(dec.Binding)
	// Set only when a LIVE session had to be left alone below; carried so the single
	// rebind log line can say which of the two cases it is.
	var liveElsewhere string

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
	// STILL NOT COVERED — both of these produce the same "thinking…" and
	// neither is this gate's business, because the gate is a decision taken
	// once, not a subscription:
	//
	//  1. An agent that dies AFTER being adopted here, e.g. mid-turn. Reuse is
	//     non-fallback-eligible (resuming stays false) on purpose, because such a
	//     session wants recovering by resuming ITS transcript whereas the fallback
	//     machinery spawns fresh. tether#59 covers this in two halves, and the split
	//     is worth stating exactly because neither half does the other's job:
	//
	//     — the NEXT prompt is recovered, by the third attachment state this branch
	//     now sets (reopenSID): Attachment.SendPrompt re-opens the same sid and
	//     delivers there. Not a change to this gate — the gate still decides once.
	//     — the IN-FLIGHT turn is closed rather than answered, by Registry.fanOut
	//     broadcasting a terminal KindResult when an init'd session's stream ends.
	//     The half-answer stays truncated and the prompt that was already delivered
	//     is not re-asked (nothing knows it went unanswered); what ends is the
	//     browser's "thinking…". Answering it too would mean re-sending a prompt cc
	//     may have half-answered, which is a different decision from recovery.
	//  2. An agent that is running but WEDGED — cc alive, stdin accepted,
	//     nothing ever emitted (the stale-spawn pitfall). Alive() reports true
	//     and should: no non-blocking check can tell "thinking" from "stuck".
	//  3. opencode specifically: if its `serve` child dies outside Interrupt()/
	//     Close(), nothing calls closeEvents, so the session's stream never ends
	//     and Alive() keeps saying true forever. That is a gap in
	//     opencodeSession's own lifecycle, tracked separately — reporting it here
	//     would mean this gate second-guessing the provider.
	if e, ok := r.liveEntry(dec.ResumeSID); ok {
		if e.workdir == want {
			a.entry = e
			// The third state (tether#59). liveEntry answered "alive" about an
			// instant that is already over — the check is a snapshot, and cc can exit
			// during the very next turn — so record the sid being reused, which is
			// what lets Attachment.SendPrompt re-open THIS conversation rather than
			// either hanging on it or replacing it with a stranger. dec.ResumeSID is
			// the key the entry was just found under, i.e. exactly the transcript it
			// is working in, so it is the id to resume; taking e.regKey instead would
			// mean reading a field guarded by r.mu to learn the same string.
			a.reopenSID = dec.ResumeSID
			return a, nil
		}
		// A live session whose cwd is not the one this connection resolved to.
		// resolveWorkspace normally rules this out — it derives the session's own
		// workspace FROM this entry, so the two agree — but not when the entry's
		// workdir is empty, which happens when neither a workspace nor
		// --workspace-root gave the daemon a directory. Handled as row 5 rather
		// than by reuse or by resuming: reusing would put this connection in a
		// directory it did not ask for, and `--resume` of a still-live sid would
		// duplicate the session that tether#54 went to some trouble to make
		// findable. Leave the live one alone and start fresh here.
		liveElsewhere = e.workdir
		dec.ResumeSID = ""
		dec.Rebound = true
	}
	if dec.Rebound {
		a.rebound = true
		// One line for one event: `live_workdir` is empty unless the branch above
		// fired, which is how an operator tells "its recorded workspace differs" from
		// "it is running right now, elsewhere".
		slog.Info("chat: the requested session does not belong to the requested workspace; starting a fresh session there",
			"requested_sid", a.reqSID, "workspace_id", dec.Binding.WorkspaceID,
			"workdir", want, "live_workdir", liveElsewhere)
	}

	cfg := agent.SpawnConfig{ResumeSessionID: dec.ResumeSID}
	e, err := r.spawnEntry(ctx, providerName, cfg, dec.Binding)
	if err != nil {
		return nil, err
	}
	a.entry = e
	a.resuming = dec.ResumeSID != ""
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
// It resolves ownership against the Entry this attachment holds rather than
// looking the sid up in the registry, and that is the whole point: the attachment
// already knows which Entry it is talking to, so there is nothing to look up and
// no race to lose. A sid-keyed variant existed (Registry.SetOwner) and is gone —
// see Entry.setOwner.
//
// The lookup was not merely redundant, it lost: until tether#54 the entry sat
// under a `pending-%p` placeholder until a goroutine parked in SessionID() moved
// it, and Resolve waits on that SAME wakeup, so a sid lookup here returned false
// whenever Resolve got there first — which serveChat reads as a fatal ownership
// race and answers by sending an error envelope and dropping the connection while
// the user's first answer is in flight. An adversarial review of tether#50
// measured that at 500/500 in a tight harness. Retiring the placeholder removes
// the window for cc (the entry is keyed under its real sid from before the
// process existed), but NOT for a provider that mints its own id and can only be
// keyed correctly once it announces it (Registry.rekey). Holding the pointer is
// what makes this correct for both.
func (a *Attachment) SetOwner(clientID string) bool {
	a.mu.Lock()
	e := a.entry
	a.mu.Unlock()
	return e.setOwner(clientID)
}

// SendPrompt forwards text to the bound session, buffering it for replay while
// the session is still unconfirmed, and — on a REUSED session that turns out to
// be dead — re-opening that session and delivering the prompt there (tether#59).
//
// The error is returned as-is on the two paths that have no reopen state, and on
// the failed-resume path it WILL be a broken pipe — cc exited without reading
// stdin. That is expected, not fatal: the caller logs it and Resolve replays the
// buffer onto a fresh session. Treating it as fatal here is precisely the
// tether#49 wedge.
//
// A reused session is the one case where nothing downstream can convert the
// error into recovery — resuming stays false, so Resolve has no fallback to run —
// and it is therefore handled HERE rather than by the caller. See reopen for why
// it belongs on this path and not in serveChat, and for why it is reachable only
// for the claude-code provider.
//
// An error this function returns is therefore one of two things, and serveChat tells
// them apart by whether it carries a *Refusal: an ordinary transport error on a path
// that recovers itself (the failed resume — log it and let Resolve replay), or a
// recovery this attachment could not complete (classified, nothing left to retry).
func (a *Attachment) SendPrompt(ctx context.Context, text string) error {
	a.mu.Lock()
	if !a.settled {
		a.pending = append(a.pending, text)
	}
	e := a.entry
	a.mu.Unlock()
	if err := e.Session().SendPrompt(ctx, text); err != nil {
		return a.reopen(ctx, e, text, err)
	}
	return nil
}

// reopen recovers a REUSED session that has stopped accepting prompts, by
// spawning `--resume <the same sid>` and delivering text there. It returns nil
// once the prompt has landed on the replacement, and sendErr unchanged whenever
// recovery is not this attachment's to do — so the failed-resume and fresh-spawn
// paths behave exactly as they did before tether#59.
//
// # Why the recovery is the SAME sid
//
// resolve's fallback spawns a fresh session because there the transcript could
// not be found. Here the opposite is true: this session was alive when it was
// adopted, so its transcript exists and holds the whole conversation. A fresh
// spawn would answer the turn and lose the context — the browser keeps its sid,
// the user keeps their scrollback, and cc alone has forgotten everything, which
// is a worse failure than the hang because it is silent. Resuming has a second,
// load-bearing consequence: the sid does not change, so Resolve's answer, the
// session_ready the browser may already hold, and every history write parked in
// WaitSID stay correct with no new signalling anywhere.
//
// # What triggers it, and why not Alive()
//
// A SendPrompt error and nothing else. agent.Session.Alive is explicitly NOT a
// promise that the next SendPrompt succeeds and cannot be one (see its doc), so
// gating recovery on !Alive() would gate it on a signal the interface refuses to
// give: for cc, `done` closes when readLoop NOTICES the exit, which can be after
// the write that discovered it, and inside that window a corpse still reports
// Alive. Gating there would leave exactly the hang this exists to end. The error
// is the authoritative answer to the only question being asked — will this
// session take the user's words — and Alive's own doc says callers must handle it.
//
// A cancelled ctx is the one error that is NOT death: the client has gone away, so
// there is nothing to recover for and nobody to answer. Same reasoning as
// resolve's ctx.Err() branch, and the same reason it must come before the spawn.
//
// # The corpse is un-registered but deliberately NOT reaped
//
// resolve reaps its failed resume, and the argument it uses does not transfer:
// there, SessionID() returned "" only because ccSession's `done` channel closed,
// and `done` is closed by readLoop's own defer, so every read of that process's
// stdout had finished — which is the precondition os/exec puts on cmd.Wait, and
// Close() is stdin.Close() + cmd.Wait(). Here the only evidence is a failed write
// to stdin, which says nothing about readLoop: it may still be draining bytes the
// pipe buffered before the process died, and that is precisely the window os/exec
// warns about ("the process looks dead" is explicitly not the argument — see
// ccSession.Close).
//
// The second reason is sharper than a documentation race. If the session is not
// in fact dead — SendPrompt can fail for reasons that leave a process running —
// cmd.Wait blocks until the child exits, and the goroutine we would block is
// serveChat's prompt reader. Reaping here could therefore hang the very turn this
// function exists to un-hang.
//
// Nothing leaks by not reaping: tether#56 put the reap in Registry.teardown, from
// this entry's own fanOut defer, which runs as soon as its Events() channel closes
// — for an exited cc that is readLoop's next statement. Un-registering here is the
// same trade liveEntry makes for the same reason (see its doc): it stops the
// corpse being handed to the next reconnect, and leaves the reap to the goroutine
// that can prove it is safe. The residual is a session whose stream never closes
// at all, which is tether#62's subject and not reachable through this path any
// more often than through the others.
//
// # One reopen per attachment
//
// reopenSpent bounds this to a single recovery per connection, and the bound is
// the same argument resolve makes for not retrying a fresh session that died: if
// the replacement ALSO refuses the prompt, spawning again would repeat whatever
// killed it, once per prompt, forever. One reopen covers the case this is for — a
// healthy session that died — and a second death inside one connection is left to
// the browser's reconnect, which arrives at the `--resume` path with the full
// fallback machinery behind it. The cost is stated rather than hidden: a
// long-lived connection whose agent dies twice hangs the second time.
//
// # It is claude-code-only, by the provider's shape rather than by choice
//
// The trigger is a non-nil SendPrompt error, and opencodeSession.SendPrompt never
// returns one: every failure it has — busy, a serve that will not relaunch, a run
// that exits — is reported as emit(EventError) and returns nil. So this function is
// unreachable for that provider. It loses nothing, because opencode does not need
// it: tether#58 made its watchServeExit emit a terminal EventError before closing
// the stream, which is the in-flight half of the same problem, and its next attach
// goes through Attach's liveEntry gate like any other. Worth knowing before reading
// this as provider-neutral machinery.
//
// # What it deliberately does not cover
//
//   - A reuse of a resume that had not CONFIRMED yet (the second-attach case in
//     Attach's doc). Re-opening its sid spawns a `--resume` of a transcript that may
//     not exist, which dies the same way — bounded to one attempt by the budget
//     above. Where that ends depends on whether Resolve has already settled, and
//     only one of the two is tidy: BEFORE it settles, Resolve reports
//     ErrCodeSessionUnconfirmed and the browser reconnects onto the resume path;
//     AFTER it has settled (the ordinary case — the death is on turn 3, Resolve ran
//     on turn 1) the prompt goes to a process that exits without reading its stdin,
//     the write may well succeed into a pipe nobody will read, and SendPrompt
//     returns nil having recovered nothing. Nor does fanOut's turn-ender cover it:
//     that replacement never emits init either, so sawInit is false and it stays
//     silent by design. The turn hangs, and nothing reports it. Distinguishing this
//     case up front needs a non-blocking "has this session confirmed" signal, which
//     SessionID() is not.
//   - Prompts that do NOT come through this attachment. Registry.DeliverAction and
//     InterruptSession reach the session by sid and hold no recovery state, so a
//     fenced-block approve landing on a session that has just died still fails as
//     it did. That is the same shape as this fix, one route further out, and it is
//     not smuggled in here.
//   - A session that is WEDGED rather than dead (cc alive, stdin accepted, nothing
//     ever emitted). No send fails, so nothing here fires — item 2 of Attach's
//     STILL NOT COVERED list, unchanged.
func (a *Attachment) reopen(ctx context.Context, dead *Entry, text string, sendErr error) error {
	// Serialise reopens: two prompts failing at once must produce ONE replacement,
	// and the loser must observe the winner's swap rather than race it.
	a.reopenMu.Lock()
	defer a.reopenMu.Unlock()

	a.mu.Lock()
	sid, spent, cur := a.reopenSID, a.reopenSpent, a.entry
	a.mu.Unlock()

	if sid == "" {
		// Not a reuse: this is the failed-resume path (Resolve falls back and
		// replays) or a fresh spawn (nothing to recover to). Unchanged behaviour,
		// and the reason the two recovery paths cannot collide.
		return sendErr
	}
	if cur != dead {
		// Another prompt already re-opened this attachment while this one was
		// waiting on reopenMu. Deliver onto the replacement instead of spawning a
		// second one — this is what makes two prompts in flight during one death
		// both land, rather than one of them being told the session is broken.
		return cur.Session().SendPrompt(ctx, text)
	}
	if ctx.Err() != nil {
		return sendErr
	}

	// Before spawning anything: has ANOTHER attachment on this sid already re-opened
	// it? Two live /wt/chat attachments on one sid is a supported state, not a
	// pathology — tabs of one browser share the sid in localStorage and the client id
	// (see Registry.OwnedByOther), and Attach's reuse branch deliberately hands the
	// second one the first one's session. Each attachment holds its OWN entry and its
	// own budget, so without this the second tab's next prompt spawns a second
	// `cc --resume <sid>` whose registration DISPLACES the first replacement: two cc
	// appending to one transcript, both fanOuts accumulating into one history buffer,
	// and every sid-keyed route (DeliverAction, InterruptSession, /wt/events) pointing
	// at one of them while the other tab talks to an orphan. That is verbatim the
	// state tether#54 exists to prevent, and reached this way the window is not
	// tether#60's "one provider.Spawn" — it lasts until the other tab is typed in.
	//
	// Adopting is not a weaker recovery, it is the SAME decision Attach's reuse branch
	// makes about the same sid, one turn later.
	//
	// Asked through liveEntry, not a bare map read, for the reason every other caller
	// uses it: registered is not alive. Two consequences worth stating rather than
	// discovering — it can return `dead` itself, because Alive() may not have flipped
	// yet (hence the `!= dead` test and the explicit evict below, which this call does
	// NOT make redundant), and it EVICTS what it finds dead, which is why the common
	// single-attachment case usually arrives at the evict below already un-registered.
	//
	// The adopted entry's directory needs no check. An entry registered under this sid
	// was spawned either by a sibling that reused the same entry this attachment did —
	// so Attach's `e.workdir == want` gate already agreed with ours — or by a
	// reconnect whose workdir resolveWorkspace derived from that same registration.
	// No path registers a foreign directory under a sid.
	//
	// Deliberately BEFORE the spent check: adoption spawns nothing, so a spent budget
	// is no reason to refuse a prompt a live session is right there to answer.
	if sibling, ok := a.reg.liveEntry(sid); ok && sibling != dead {
		slog.Info("chat: the reused session was already re-opened by another attachment; adopting it",
			"sid", sid, "err", sendErr)
		a.adopt(sibling, dead)
		return sibling.Session().SendPrompt(ctx, text)
	}

	if spent {
		return sendErr
	}

	slog.Info("chat: the reused session stopped accepting prompts; re-opening it",
		"sid", sid, "err", sendErr)

	// Drop the corpse's registration BEFORE spawning, so that if the spawn fails the
	// dead sid still stops reading as live to the next reconnect. evict is by-value
	// and idempotent, so the replacement registered under this same key a line later
	// cannot be taken out by it, nor by the corpse's own teardown.
	a.reg.evict(dead)

	// a.ws, not the zero binding — same reason as the fallback in resolve
	// (tether#52), and here it is not merely tidy: cc keys its transcript on cwd, so
	// a `--resume` in any other directory fails exactly like an unknown sid. The
	// reuse branch in Attach only reuses an entry whose workdir already IS
	// workdirFor(a.ws), so this reopens in the directory the dead session lived in.
	fresh, err := a.reg.spawnEntry(ctx, a.provider, agent.SpawnConfig{ResumeSessionID: sid}, a.ws)
	if err != nil {
		a.mu.Lock()
		a.reopenSpent = true
		a.mu.Unlock()
		// Both causes, in one error: "spawn failed" without "the session it was
		// replacing had stopped accepting prompts" loses the half that says which
		// recovery was being attempted (the same reason errorEnvelope keeps the
		// whole wrapped message).
		return fmt.Errorf("reused session %s stopped accepting prompts (%v) and could not be re-opened: %w", sid, sendErr, err)
	}

	a.mu.Lock()
	a.reopenSpent = true
	a.mu.Unlock()

	// Carry the ownership claim across. The dead entry may already have been claimed
	// by this connection (serveChat calls SetOwner once, after Resolve), and a
	// recovery that quietly returned the session to unowned would let a DIFFERENT
	// client join a conversation it was refused a moment earlier — a hole opened by
	// the fix rather than by anything the user did.
	//
	// The CAS result is ignored because a `false` is the right outcome, NOT because it
	// cannot happen: fresh is published under this sid by spawnEntry before this line
	// runs, so another connection could in principle reuse and claim it first — and if
	// one has, that claim is not ours to overwrite. What makes the carry effective is
	// its POSITION: it precedes the prompt send below, so no answer can be streaming
	// while the entry is still unowned.
	if owner := dead.owner(); owner != "" {
		fresh.setOwner(owner)
	}

	// Re-point the attachment and move the subscribers over BEFORE the prompt goes
	// out — see adopt for why that order is the whole of this function's correctness.
	a.adopt(fresh, dead)

	// Exactly ONE prompt is re-delivered: the one that just failed. NOT a.pending,
	// which is the failed-resume buffer and means something different here — on that
	// path cc exited without reading stdin, so every buffered prompt is known
	// undelivered; on this one the session was alive and answering, so the earlier
	// prompts in that buffer have been delivered AND answered, and re-sending them
	// would ask the model questions the resumed transcript already contains. Every
	// other prompt that failed is recovered by its own SendPrompt call, through the
	// cur != dead branch above.
	//
	// Sent outside the lock so a blocked pipe cannot stall SendPrompt/Subscribe.
	if err := fresh.Session().SendPrompt(ctx, text); err != nil {
		return fmt.Errorf("re-opened session %s would not take the prompt: %w", sid, err)
	}
	return nil
}

// adopt re-points this attachment at next and moves the subscriber set off prev.
//
// Both recovery paths in reopen go through it, and both must call it BEFORE they
// deliver a prompt to next — which is the only thing about this function that is
// hard, so it is stated here rather than at each call site:
//
//   - The subscriber set lives on the Entry (see Attachment.subs), and a prompt is
//     what makes cc emit. Sending first leaves a window in which the replacement's
//     system/init and its first text deltas are broadcast to whatever subscribers the
//     entry has, which is none — and broadcast DROPS rather than queues, so those
//     events are gone. The turn then looks exactly like the hang this whole slice
//     exists to end, on the recovery path, which is the worst place to have it.
//   - Un-subscribing from prev is the other half and not tidying: prev is a corpse
//     whose readLoop may still be draining bytes the pipe buffered before the process
//     died (the same window reopen's no-reap argument turns on). Left subscribed, that
//     stale tail — and the turn-ender fanOut broadcasts when the corpse's stream ends
//     — arrives at the browser AFTER the replacement has started answering, i.e. the
//     dead session's half-sentence interleaved into the live one.
//
// Under a.mu because entry is REPLACED and every reader takes it there.
func (a *Attachment) adopt(next, prev *Entry) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.entry = next
	for ch := range a.subs {
		prev.Unsubscribe(ch)
		next.Subscribe(ch)
	}
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
	e, resuming, rebound := a.entry, a.resuming, a.rebound
	a.mu.Unlock()

	// Blocks until system/init lands (the session is real) or the process exits
	// without one (tether#49's done channel), which yields "".
	if sid := e.Session().SessionID(); sid != "" {
		// A rebound attachment (resolveWorkspace row 5) confirmed a session the
		// client did not ask for — its sid belonged to another workspace and was
		// dropped. Reported through the SAME two flags a failed resume uses, because
		// it is the same thing from the user's side: they reconnected and their
		// context did not come with them. Saying so is what stops the fresh session
		// from arriving as an unexplained empty transcript. The notice is still gated
		// on there having BEEN a conversation to lose (see HistoryStore.HasHistory).
		if rebound {
			notice := a.reg.History != nil && a.reg.History.HasHistory(a.reqSID)
			return Resolution{SID: sid, Recovered: true, Notice: notice, Rebound: true}, nil
		}
		return Resolution{SID: sid}, nil
	}

	// A cancelled connection also kills the subprocess, so it arrives here looking
	// exactly like a failed resume. Distinguishing them matters for one concrete
	// reason: the log line below is the only signal an operator has for "how often
	// do resumes actually fail", and counting every closed tab as a resume failure
	// would make it worthless. There is also nothing worth recovering for a client
	// that has gone away.
	if ctx.Err() != nil {
		return Resolution{}, refuse(wire.ErrCodeConnectionClosed, "connection closed before the session confirmed: %w", ctx.Err())
	}

	if !resuming {
		// A FRESH session died before init. There is nothing to fall back to —
		// respawning would just repeat whatever killed it (a missing/broken cc
		// binary, a bad workdir) and could spin. Surface it, exactly as before.
		//
		// tether#63: classified ErrCodeSessionUnconfirmed, and it MUST stay
		// retryable — see this function's own doc comment and Attach's: an
		// ordinary browser reconnect for the same sid takes the `--resume`
		// path next time, which is precisely how a transient spawn failure
		// (a bad binary path, a momentary resource limit) recovers without
		// the user doing anything. Marking this terminal would turn that
		// recoverable hiccup into a dead end the ladder refuses to retry out
		// of.
		return Resolution{}, refuse(wire.ErrCodeSessionUnconfirmed, "agent exited before emitting session id")
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
	// and every read from that pipe is finished.
	//
	// Since tether#56 this entry's own fanOut reaps it too, from Registry.teardown,
	// so the reap is no longer at risk of being skipped — but it is still done HERE
	// rather than left to that defer. The fresh spawn happens on the next line, and
	// bounding "how long can two cc processes for one attachment overlap" to this
	// call rather than to whenever another goroutine gets scheduled is worth one
	// idempotent call. agent.Session.Close is required to be idempotent precisely
	// so these two can coexist (see the interface doc); whichever arrives second
	// gets the same answer instead of "Wait was already called".
	if err := e.Session().Close(); err != nil {
		slog.Debug("reaped the failed-resume subprocess", "requested_sid", a.reqSID, "err", err)
	}

	// a.ws, not the zero binding: the replacement must run in the SAME workspace as
	// the resume it replaces (tether#52). Spawning into the daemon default here
	// would move the user's conversation to a different directory as the price of
	// recovering it — and every subsequent reconnect would then resume it there,
	// making the relocation permanent and invisible.
	fresh, err := a.reg.spawnEntry(ctx, a.provider, agent.SpawnConfig{}, a.ws)
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
			// tether#63 — the fallback session exists but would not take the
			// user's words. Retryable, and classified rather than left to
			// default so that the enumeration in wire/errors.go is the whole
			// list and not merely most of it: the next reconnect re-resumes,
			// and a pipe that broke once is exactly the kind of failure a
			// second attempt clears.
			return Resolution{}, refuse(wire.ErrCodeSpawnFailed, "replay prompt onto fresh session: %w", err)
		}
	}

	sid := fresh.Session().SessionID()
	if sid == "" {
		// tether#63 — the twin of the `!resuming` branch above, and retryable
		// for the same reason stated there. Same code deliberately: from the
		// browser's side these are one situation ("the agent never confirmed a
		// session"), and the messages already say which of the two it was.
		return Resolution{}, refuse(wire.ErrCodeSessionUnconfirmed, "fresh session after failed resume %s exited before emitting session id", a.reqSID)
	}
	return Resolution{SID: sid, Recovered: true, Notice: notice}, nil
}
