package session

import (
	"context"
	"errors"
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
	// reopenSID is the THIRD attachment state (tether#59): non-empty means that if
	// this attachment's session turns out to be dead it may be recovered by
	// RE-OPENING this very sid — `--resume reopenSID` — rather than by spawning a
	// fresh one.
	//
	// The arming sites do NOT establish that equally, and the difference is
	// load-bearing rather than pedantic — see "When it is set" below.
	//
	// # When it is set, and why that is the whole condition (tether#76)
	//
	// tether#59 set it in one place, Attach's reuse branch, and phrased it as "this
	// attachment reused a live session". That description named a sufficient
	// condition and was read as the necessary one, so every OTHER connection — the
	// one that CREATED the session, and the one whose `--resume` succeeded — reached
	// reopen's `sid == ""` gate and got nothing. Since that gate precedes even the
	// sibling-adoption branch, such a connection could not so much as adopt the
	// replacement a sibling had already re-opened. Measured: the creating tab's
	// prompts were dropped in silence while the reusing tab recovered normally.
	//
	// The real condition is not "was it reused" but "is there a transcript to
	// resume". THREE sites answer that, at two different strengths — count them here
	// rather than trusting this list to have stayed short, since the last enumeration
	// went stale the moment a site was added:
	//
	//   - Resolve, on confirmation — the strong one, and the one tether#76 adds. A
	//     non-empty SessionID() means cc emitted system/init, and under
	//     `--input-format stream-json` cc emits nothing until a user message arrives
	//     (see this type's doc; pinned by TestFakeCC_SilentUntilFirstPrompt), so a
	//     confirmed sid has a turn written behind it. This covers the fresh spawn,
	//     the `--resume` that succeeded, and the fresh session a failed resume fell
	//     back to.
	//   - Attach's reuse branch, at attach time — the WEAK one, unchanged from
	//     tether#59. All it observes is liveEntry, i.e. Alive(), which for cc is a
	//     non-blocking poll of the `done` channel: "the process has not exited". That
	//     says nothing about init, so this site can arm an attachment whose session
	//     has NOT confirmed — concretely, a second attach adopting a `--resume` that
	//     is still in flight (Attach's tether#54 note). It is armed early anyway
	//     because a reused session is already answering and can refuse a prompt
	//     before Resolve has run, and the cost of being wrong is bounded by
	//     reopenSpent.
	//   - Attach's adopted-registration path (tether#78), also at attach time — the
	//     SAME weak evidence as the reuse branch, reached one function deeper. When
	//     spawnEntry finds the key already registered to a session liveEntry calls
	//     alive, this attachment is in the reuse branch's position rather than a
	//     tether#60 waiter's, and is armed for the reuse branch's reason. Not stronger
	//     than the reuse site and not weaker: both observe exactly Alive(). Mutually
	//     exclusive with it in program order — the reuse branch returns before
	//     spawnEntry is called — which is what keeps "at most twice" true below.
	//
	// That asymmetry is exactly why reopen's "What it deliberately does not cover"
	// still lists being armed on weaker-than-confirmation evidence. Do not collapse the
	// three sites into "armed ⟹ the transcript exists": it is true of the Resolve site
	// and false of BOTH attach-time sites, and believing the universal form would read
	// that residual as dead text.
	//
	// What tether#76 does NOT do is arm at SPAWN time, which is the placement its own
	// wi sketched. A session armed before it confirms could be re-opened into a
	// transcript that was never written — the strictly worse version of the weak site
	// above, reached on every fresh connection rather than on a rare double-attach.
	//
	// # Still not a synonym for `resuming`
	//
	// The two remain mutually exclusive BY CONSTRUCTION, though the construction has
	// moved: Attach's reuse branch still returns before `resuming` is ever assigned,
	// and the Resolve-time arming clears `resuming` in the same critical section it
	// sets this — accurately, not defensively, because resolveOnce means no fallback
	// can run after that point. So reopenSID != "" ⟹ resuming == false still holds.
	//
	// The exclusivity is what keeps the two recovery paths from interfering, and they
	// must not be merged because they recover opposite situations. A resume that
	// failed means the transcript could not be found, so recovery is a FRESH session
	// (and resuming that sid again would loop). A session that confirmed and then
	// died means the transcript is sitting on disk holding the whole conversation, so
	// recovery is resuming it (and a fresh session would answer the turn while
	// silently throwing the context away — the user keeps their scrollback and their
	// sid, and cc alone has forgotten everything).
	//
	// Written at most twice and never to a different value: once in Attach — by the
	// reuse branch OR by the adopted-registration path, which cannot both run — and
	// once in Resolve on confirmation. Read under mu, so that no reader has to know
	// that.
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
// Since tether#60 the second attach does not merely arrive late, it WAITS.
// spawnEntry claims the key before the process exists, so a caller that reaches it
// while another is spawning blocks on that claim and adopts the resulting entry
// instead of starting a rival — and is told it did not spawn, so it can decline to be
// fallback-eligible, exactly as this branch declines. See Registry.spawnEntry.
//
// And since tether#78 the gate below is no longer what has to be right for that to
// hold. It was: the gate and spawnEntry's claim are two separate critical sections,
// so a caller whose check ran before the winner registered but which did not reach
// the claim until after the winner's reservation was RELEASED found nothing to wait
// for, spawned, and displaced the registration exactly as before tether#60. The claim
// now consults r.sessions itself, under the reservation, so such a caller adopts the
// live registration instead. What that leaves for this gate is the decision it is
// actually for — reuse this session, resume it, or start fresh elsewhere — and one
// property worth keeping in mind while reading it: LOSING the race it appears to
// guard no longer costs a duplicate agent, only a slower path to the same entry.
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
	//     — the NEXT prompt is recovered, by the third attachment state (reopenSID):
	//     Attachment.SendPrompt re-opens the same sid and delivers there. Not a
	//     change to this gate — the gate still decides once. tether#59 armed that
	//     state only in this branch, which left the connection that CREATED the
	//     session, and the one whose `--resume` succeeded, with no recovery at all;
	//     tether#76 arms every confirmed attachment in Resolve instead. See
	//     Attachment.reopenSID.
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
			//
			// This is the EARLY arming, not the only one (tether#76). Every other
			// path is armed when Resolve confirms; this branch is armed here, before
			// the attachment is handed to anyone, because a reused session is already
			// answering and can refuse a prompt before Resolve has run. Resolve's
			// arming then writes the same sid again — liveEntry found this entry under
			// it, so it is the sid the entry reports — which is why the two cannot
			// disagree.
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
	// spawnIfAbsent: an attach with nowhere to land must be able to start a session —
	// adoptOnly exists for a caller with a budget to bound, which is reopen's alone
	// (tether#82).
	e, outcome, err := r.spawnEntry(ctx, providerName, cfg, dec.Binding, spawnIfAbsent)
	if err != nil {
		return nil, err
	}
	a.entry = e
	// Anything but spawnStarted means this attachment did not start the session it
	// is holding, and such an attachment is NOT fallback-eligible — for the same
	// reason the reuse branch above is not: a failed resume is the SPAWNER's to
	// recover from. Two attachments both falling back would spawn two fresh sessions
	// and fork the conversation in two, which is what cc's --fork-session is for and
	// not something to arrive at by losing a race. If the resume it adopted never
	// confirms it is told "agent exited before emitting session id", which is
	// retryable and which the browser answers with a reconnect — the same trade the
	// tether#54 note above already states for the reuse branch.
	a.resuming = outcome.startedProcess() && dec.ResumeSID != ""

	// The two adoptions are NOT the same attachment state, and collapsing them is
	// what a bool did until tether#78:
	//
	//   - adoptedAfterWait (tether#60): the winner has only just started this agent,
	//     so what was adopted is an UNCONFIRMED resume. Arming recovery on it would
	//     break the rule that nothing is armed before Resolve confirms — hence the
	//     window in which a failed SendPrompt from a waiter returns the bare error.
	//   - adoptedRegistration (tether#78): the key was already registered to a
	//     session liveEntry found ALIVE. That is the reuse branch's own evidence,
	//     arrived at one function deeper, so this attachment is in the reuse branch's
	//     position and gets the reuse branch's arming — early, because a session that
	//     is already answering can refuse a prompt before Resolve has run, and the
	//     prompt reader runs in parallel with Resolve (serveChat).
	//
	// dec.ResumeSID is necessarily non-empty here: an empty one makes spawnEntry mint
	// a fresh id, and in practice nothing can already be registered under an id just
	// minted (see GetOrSpawnEntry for the one fixed-uuid fallback that qualifies it) — so
	// this arms the sid the entry was actually found under, exactly as above.
	if outcome == adoptedRegistration {
		a.reopenSID = dec.ResumeSID
	}
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
// Every OTHER non-nil return is a *Refusal carrying ErrCodePromptUndelivered
// (tether#77), because the browser is shown an error frame only for a Refusal
// (promptErrorEnvelope in internal/server/wt_chat.go) and every one of those
// branches is the end of the line for the prompt that reached it. The split is
// not severity: it is whether something else is still going to try. `sid == ""`
// and a cancelled ctx hand the problem to machinery that will (Resolve's replay)
// or to nobody who is still listening; the other five have run out of moves.
//
// All five go through undelivered, including the spawn-failure branch, which
// used to inherit whatever code spawnEntry attached. Uniformity here is not
// tidiness: it is the difference between "classified" being a property of this
// function and being a property of every error every callee might invent. The
// old arrangement failed exactly that way — awaitSpawn's three bare returns
// (tether#60) travelled up this path unclassified and were dropped.
//
// # Why the recovery is the SAME sid
//
// resolve's fallback spawns a fresh session because there the transcript could
// not be found. Here the opposite is expected: this attachment is armed, which in
// the ordinary case means its session confirmed and its transcript holds the whole
// conversation. (The one armed state that does NOT guarantee that is the reuse of a
// still-unconfirmed resume — see reopenSID, and the residual listed below.) A fresh
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
// long-lived connection whose agent dies twice cannot answer the second one.
//
// That cost used to read "hangs the second time", and hanging is what it was
// until tether#59 added a turn-ending frame — after which the spinner stopped
// and the same refusal became invisible instead of merely stuck. tether#77
// re-priced it: the bound is unchanged and still one, but spending it is now
// reported (ErrCodePromptUndelivered) rather than decided behind the user's
// back.
//
// tether#82 then moved WHERE it is charged, without changing the number. A spent
// attachment used to return before spawnEntry, which made "may not spawn" also mean
// "may not find out that a live session is already there" — everything spawnEntry does
// for a late caller sits past the claim. The budget is now handed to spawnEntry as
// adoptOnly, so a spent attachment walks the same road and stops at the one step it may
// not take. Read the bound as what it always said: this attachment may start at most
// one `cc --resume`, and may adopt as often as there is something to adopt.
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
//   - An attachment armed on evidence weaker than confirmation: a reuse of a resume
//     that had not CONFIRMED yet (the second-attach case in Attach's doc), and since
//     tether#78 an adopted registration, which observes exactly the same Alive() one
//     function deeper. Re-opening such a sid spawns a `--resume` of a transcript that
//     may not exist, which dies the same way — bounded to one attempt by the budget
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
		// Never armed, so there is no transcript this attachment may claim. Both
		// shapes that reach here are owned by machinery that a reopen would fight:
		// a `--resume` still in flight or already failed, where Resolve falls back
		// and replays this very prompt from a.pending; and a fresh spawn that has
		// not emitted system/init, where Resolve reports ErrCodeSessionUnconfirmed
		// and the browser reconnects onto the resume path. Returning sendErr
		// unchanged is what hands those back to the machinery that owns them, and
		// is the reason the two recovery paths cannot collide.
		//
		// "Never armed" is not the same as "still resolving": an attachment whose
		// Resolve FAILED stays here permanently (settled, with an empty sid). That
		// is the right answer for it too — nothing confirmed, so there is nothing to
		// resume — but it is a terminal state rather than a window.
		//
		// Before tether#76 this branch also swallowed every CONFIRMED attachment
		// that had not come through Attach's reuse branch, which is the bug that wi
		// exists for. Note where it sits: ahead of the sibling-adoption check below,
		// so such a connection could not even adopt a replacement another attachment
		// had already re-opened for the same sid.
		return sendErr
	}
	if cur != dead {
		// Another prompt already re-opened this attachment while this one was
		// waiting on reopenMu. Deliver onto the replacement instead of spawning a
		// second one — this is what makes two prompts in flight during one death
		// both land, rather than one of them being told the session is broken.
		//
		// Classified if that delivery fails (tether#77): the replacement is this
		// attachment's session now, so a refusal here is the end of the line for
		// this prompt — nothing further retries it.
		return undelivered(cur.Session().SendPrompt(ctx, text),
			"session %s was re-opened by another prompt but would not take this one", sid)
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
	//
	// Two armed attachments whose prompts fail together both reach this check and
	// both find nothing (liveEntry evicts the corpse and answers no), so before
	// tether#60 both went on to spawn `cc --resume <sid>` and the second registration
	// displaced the first — two cc appending to one transcript, each tab holding a
	// different *Entry. reopenMu is per-ATTACHMENT and serialises none of that.
	// spawnEntry's reservation is what makes the second one wait instead; it reports
	// that it did not spawn, which is why the budget below is left unspent for it.
	//
	// Since tether#78 an attachment that LOSES this check no longer starts a rival:
	// this check and spawnEntry's claim are still separate critical sections, but the
	// claim consults r.sessions itself, so a caller preempted between them long enough
	// to miss the winner's reservation adopts the winner's registration one layer down.
	// For an attachment with budget left, the two adoptions then differ only in wording
	// — which log line, and which message a refused prompt carries.
	//
	// Since tether#82 that is true of a SPENT attachment as well, and it took a change
	// rather than following from tether#78: the budget used to return BEFORE spawnEntry,
	// so an attachment that had used its one re-open depended entirely on winning THIS
	// check, and lost it in a way this check cannot avoid — liveEntry reads the map and
	// only then asks Alive(), so what it reports is the state at the read. Lose it and
	// the prompt was refused with a live session registered under the sid. The budget is
	// now passed to spawnEntry as adoptOnly (see below), which is where the same question
	// gets its authoritative answer, so both callers of this check are in the same
	// position: losing it costs a slower path to the same entry, never a refusal that a
	// live session would have answered.
	//
	// What this check therefore IS, now that neither caller depends on it: an
	// optimisation, plus a differently-worded log line and refusal message. Do not read
	// more into the wording than liveEntry can support — it proves only that something
	// live and not `dead` is registered under this sid, never WHO put it there, so
	// "another attachment re-opened it" is this branch's most likely story rather than
	// its finding. It stays for the same reason Attach's own gate does, and not because
	// anything downstream is unsound without it: disabling it entirely leaves this
	// package green, since every caller then arrives at the same entry one layer down.
	if sibling, ok := a.reg.liveEntry(sid); ok && sibling != dead {
		slog.Info("chat: the reused session was already re-opened by another attachment; adopting it",
			"sid", sid, "err", sendErr)
		a.adopt(sibling, dead)
		// Classified if the adopted session refuses it (tether#77). Adoption is
		// the recovery on this branch — there is no second one behind it, and the
		// budget was deliberately left unspent, so the NEXT prompt gets a full
		// reopen. This one does not.
		return undelivered(sibling.Session().SendPrompt(ctx, text),
			"adopted the live session under %s but it would not take the prompt", sid)
	}

	// The budget is asked as a MODE rather than as a gate on the road below, and that
	// swap is the whole of tether#82. What it bounds is SPAWNING; expressing it as an
	// early return meant a spent attachment never reached spawnEntry's claim, so
	// everything the claim does for a late caller — waiting on a replacement that is
	// still inside provider.Spawn, consulting the registration once the corpse is out
	// of the way — was unreachable for precisely the attachment with no second chance
	// left. It depended entirely on winning the sibling check above, and that check
	// answers about the entry it FOUND: liveEntry reads the map and only THEN asks
	// Alive(), so a verdict of "nothing live here" can be a fact about a corpse that
	// has already been replaced. The cost of losing it was a refused prompt with a
	// live session registered under the sid — reachable in four steps, no concurrency
	// beyond a scheduling delay.
	//
	// Both modes go through spawnEntry now. adoptOnly stops at the Spawn it may not
	// perform and reports errNoSessionToAdopt, which is where the bound is paid.
	mode := spawnIfAbsent
	if spent {
		mode = adoptOnly
		// Deliberately NOT the "re-opening it" line below: nothing will be re-opened
		// here, and a log line that says otherwise is worse than none. Info for the
		// reason spawnEntry's adoption line is — nothing in this daemon calls
		// slog.SetDefault, so Debug is unobservable on a real one — and it is the only
		// signal an operator gets that a connection reached its budget at all.
		slog.Info("chat: the reused session stopped accepting prompts and this connection has "+
			"already used its one re-open; looking for a live session under the sid to adopt",
			"sid", sid, "err", sendErr)
	} else {
		slog.Info("chat: the reused session stopped accepting prompts; re-opening it",
			"sid", sid, "err", sendErr)
	}

	// Drop the corpse's registration BEFORE spawning, so that if the spawn fails the
	// dead sid still stops reading as live to the next reconnect. evict is by-value
	// and idempotent, so the replacement registered under this same key a line later
	// cannot be taken out by it, nor by the corpse's own teardown.
	//
	// It has been load-bearing rather than tidy since tether#78 gave spawnEntry a
	// post-claim consult: that consult asks liveEntry, which is free to hand `dead` back
	// — Alive() need not have flipped yet — so without this the caller would "adopt" the
	// corpse it is recovering FROM and write into the same broken pipe. Deleting it fails
	// TestSendPrompt_ReopensEvenWhileTheDeadSessionStillReportsItselfAlive,
	// TestSendPrompt_ReopenSpawnFailureNamesBothCausesAndDropsTheCorpse and
	// TestResolve_AFailedResolveDoesNotDisarmAReusedAttachment, all of which predate
	// tether#82.
	//
	// What tether#82 changes is only WHO reaches it: a SPENT attachment now does, and
	// gets the same protection on its adopt-only consult. The evidence is the same as on
	// the unspent path (one failed write, which is not proof of death — see this
	// function's no-reap section), so this is the same trade one branch over. One
	// consequence that IS new and is not stated anywhere else: when the consult then
	// finds nothing, this attachment refuses and nothing repopulates the key, whereas the
	// unspent path always registers a replacement a line later. For a session that failed
	// a write but is genuinely alive, the sid therefore stops resolving for
	// DeliverAction/InterruptSession/IsLive until the next attach resumes it — which is
	// what un-registering a session believed dead has always meant, arrived at from a
	// branch that does not replace it.
	a.reg.evict(dead)

	// a.ws, not the zero binding — same reason as the fallback in resolve
	// (tether#52), and here it is not merely tidy: cc keys its transcript on cwd, so
	// a `--resume` in any other directory fails exactly like an unknown sid. The
	// reuse branch in Attach only reuses an entry whose workdir already IS
	// workdirFor(a.ws), so this reopens in the directory the dead session lived in.
	fresh, outcome, err := a.reg.spawnEntry(ctx, a.provider, agent.SpawnConfig{ResumeSessionID: sid}, a.ws, mode)
	if errors.Is(err, errNoSessionToAdopt) {
		// Reachable only under adoptOnly, i.e. only when the budget is already spent —
		// and it now means something stronger than the early return it replaces: no live
		// session is registered under this sid AND none is being spawned under it. This
		// is where the one-re-open bound is actually paid, against an authoritative
		// answer rather than against a check this attachment could lose.
		//
		// tether#77's wording, kept verbatim so nothing downstream of the refusal has to
		// change. reopen's doc used to accept this silence outright ("a long-lived
		// connection whose agent dies twice hangs the second time"), and while that was
		// written the cost was at least legible: with no turn-ending frame the spinner
		// kept turning, which is ugly but does say something is wrong. tether#59 added
		// the turn-ender, so the same refusal now leaves a tab that looks idle and
		// healthy while swallowing every prompt typed into it. Same silence, strictly
		// worse symptom.
		//
		// The budget is not what tether#77 or tether#82 revisit — it still bounds how
		// many `cc --resume` this attachment may start, and one is still the right
		// number. tether#77 made spending it something the user is told about instead of
		// a decision made behind their back; tether#82 made being told it depend on
		// there genuinely being nothing to adopt. "already used" rather than "died
		// again": reopenSpent is set both by a replacement that started and by one that
		// failed to (see the spawn-failure branch below), so this is reached in a state
		// where the session may have died only once and simply never been replaced.
		return refuse(wire.ErrCodePromptUndelivered,
			"session %s stopped accepting prompts and this connection has already used its one re-open: %w", sid, sendErr)
	}
	if err != nil {
		// Also reached under adoptOnly, for every error that is not the verdict: a
		// replacement this call waited on and which then failed, the workdir refusal,
		// awaitSpawn's cancelled wait (ctx.Err(), classified here although reopen's own
		// ctx branch stays bare — the tether#77 trade, unchanged), and its "neither an
		// entry nor an error". Not a closed list on purpose: it is whatever spawnEntry
		// and awaitSpawn can produce, which is exactly why this branch classifies rather
		// than inspects.
		//
		// The assignment below is then a no-op — the budget is already spent, which is
		// why this mode was chosen — and the message stays accurate either way: nothing
		// was re-opened.
		a.mu.Lock()
		a.reopenSpent = true
		a.mu.Unlock()
		// Both causes, in one error: "spawn failed" without "the session it was
		// replacing had stopped accepting prompts" loses the half that says which
		// recovery was being attempted (the same reason errorEnvelope keeps the
		// whole wrapped message).
		//
		// Through undelivered like every other give-up branch, which changes the
		// code this used to report (ErrCodeSpawnFailed, borrowed from spawnEntry's
		// own Refusal) to ErrCodePromptUndelivered. The old code was not wrong so
		// much as accidental — it was whatever spawnEntry happened to attach — and
		// relying on it made this branch's classification conditional on a function
		// two layers away: awaitSpawn (tether#60) returns BARE errors for a
		// cancelled wait, a winner that published neither entry nor error, and a
		// workdir mismatch, and every one of those reached this line, escaped
		// promptErrorEnvelope unclassified, and produced exactly the silence this
		// wi is about. Classifying here makes "reopen gave up ⇒ the browser is
		// told" structurally true rather than an invariant maintained at a
		// distance. The specific cause is not lost — it stays in the message, and
		// in the error chain under this wrapper.
		return undelivered(err, "reused session %s stopped accepting prompts (%v) and could not be re-opened", sid, sendErr)
	}

	// The budget bounds SPAWNING, so an entry this call did not start does not
	// consume it — a reservation it merely waited on (tether#60) or a registration it
	// adopted (tether#78). Both are the same rule the sibling-adoption branch above
	// follows, reached one layer down: all three arrive at "a live session exists
	// under this sid and we did not have to start it", and refusing this attachment a
	// later recovery because of a race it lost would be a penalty for timing.
	//
	// Asked as startedProcess() rather than `!= spawnStarted` so that a fourth
	// outcome cannot join the spending side by default.
	if outcome.startedProcess() {
		a.mu.Lock()
		a.reopenSpent = true
		a.mu.Unlock()
	}

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
	// Classified (tether#77): if this call started the replacement its budget has just
	// been spent on it, so a refusal here means the next prompt gets the adopt-only
	// treatment above; if it adopted one, the adoption WAS the recovery. Either way there
	// is nothing left to wait for.
	//
	// The verb follows the outcome, which is not decoration: since tether#78 this line is
	// reached by a caller that adopted rather than spawned, and since tether#82 that
	// includes a SPENT one — for which "re-opened" would name the single thing it is not
	// allowed to have done. The sibling branch above already says it the other way for
	// the same event, so the two now agree.
	what := "re-opened session %s would not take the prompt"
	if !outcome.startedProcess() {
		what = "adopted the live session under %s but it would not take the prompt"
	}
	return undelivered(fresh.Session().SendPrompt(ctx, text), what, sid)
}

// undelivered classifies a failed prompt delivery as ErrCodePromptUndelivered,
// and passes nil through unchanged.
//
// It exists because the three branches that use it all end the same way — the
// prompt is gone and nothing behind them will try again — and because getting
// that wrong is invisible. A bare error returned from reopen is dropped by
// promptErrorEnvelope (internal/server/wt_chat.go), which sends a frame only
// for a *Refusal, so the browser is told nothing at all and the tab goes on
// looking healthy. Wrapping at each site by hand is one forgotten call from
// reintroducing exactly that, in a function where the difference between
// "handed off to machinery that retries" and "given up on" is already the
// hardest thing to keep straight.
//
// Not applied to the two branches that return sendErr bare, which are not
// giving up: `sid == ""` hands the prompt back to Resolve (which replays it, or
// reports ErrCodeSessionUnconfirmed itself), and a cancelled ctx means the
// client is already gone and there is nobody to tell.
func undelivered(err error, format string, a ...any) error {
	if err == nil {
		return nil
	}
	// Both halves in one message, the same shape the spawn-failure branch above
	// uses: what was being attempted, and what the attempt hit.
	return refuse(wire.ErrCodePromptUndelivered, format+": %w", append(a, err)...)
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
//
// Confirming is also what ARMS reopen for every attachment that did not come
// through Attach's reuse branch (tether#76). A confirmed sid is a sid with a
// transcript, which is the entire precondition reopen needs; see reopenSID for why
// that is the right condition and why arming any earlier would be wrong.
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
		// Arm reopen (tether#76). Both halves are set in the SAME critical section
		// that publishes the resolution, so no reader can observe an attachment that
		// is settled but not yet recoverable — a prompt failing in that gap is the
		// hang this fixes, one instruction narrower.
		//
		// Clearing resuming is what keeps `reopenSID != "" ⟹ resuming == false` true
		// by construction on the successful-resume path, and it is accurate rather
		// than defensive: resolveOnce has already run, so resolve's fallback can
		// never fire again, and a.resuming has no other reader.
		//
		// The gate is not "only arm on success", it is "never DISARM". Attach's reuse
		// branch may already have armed this attachment, and resolve can still fail
		// for it — a reuse of a `--resume` that never confirmed returns
		// ErrCodeSessionUnconfirmed — so an unguarded assignment would write "" over
		// a live arming and silently take tether#59's recovery away from exactly the
		// attachments it was built for. Pinned by
		// TestResolve_AFailedResolveDoesNotDisarmAReusedAttachment.
		//
		// Each half alone is redundant (every error path in resolve returns the zero
		// Resolution, so resErr == nil ⟺ res.SID != ""), and a mutant deleting either
		// one survives. It is the CONJUNCTION that is load-bearing, and deleting the
		// whole `if` is not an equivalent mutation — it is the bug above.
		if a.resErr == nil && a.res.SID != "" {
			a.reopenSID = a.res.SID
			a.resuming = false
		}
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
	// The outcome is discarded: the fallback is a FRESH spawn, so spawnEntry mints an
	// id no other goroutine can hold — the tether#60 reservation is uncontended and
	// the tether#78 consult cannot in practice find a registration under an id just
	// minted (same fixed-uuid caveat as GetOrSpawnEntry), so
	// neither adoption is reachable. Same reason GetOrSpawnEntry discards it.
	//
	// spawnIfAbsent: this IS the recovery, so a mode that may not spawn would have
	// nothing to offer it (tether#82). Not the same budget as reopen's either — the
	// fallback is bounded by resolveOnce, not by reopenSpent.
	fresh, _, err := a.reg.spawnEntry(ctx, a.provider, agent.SpawnConfig{}, a.ws, spawnIfAbsent)
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
