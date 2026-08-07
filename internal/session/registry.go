// Package session manages live cc sessions and multi-attach broadcast (D-08, D-15).
package session

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sort"
	"sync"

	"github.com/piaobeizu/tether/internal/agent"
	"github.com/piaobeizu/tether/internal/wire"
)

// Registry holds all live sessions and the set of event subscribers per session.
type Registry struct {
	mu sync.RWMutex
	// sessions is keyed by sid. Every entry is registered before spawnEntry
	// returns, under the sid it told its agent to use — so for a provider that
	// ADOPTS that id (cc, mem_2ruSlrHR ①) the key is the session's final sid from
	// before the process existed, so no lookup can miss it for want of a re-key
	// (tether#54). Two attaches racing to spawn the SAME sid used to be a separate,
	// much narrower window; tether#60's reservation (see spawning, below) makes the
	// second one wait, and tether#78 made the claim the point at which THIS map is
	// consulted, so a caller that arrives after the winner already finished adopts
	// the registration instead of displacing it. See spawnEntry.
	//
	// Not a universal invariant, and the difference matters: a provider that mints
	// its own id (OpenCodeProvider ignores SpawnConfig.SessionID) is registered
	// under an id nothing else will ever ask about until it announces its own, at
	// which point rekey moves it. Such an entry is addressable — evict and
	// BroadcastAll find it, which the old placeholder also allowed — but NOT by any
	// sid a client holds. See rekey, whose write is also the one registration that
	// happens without holding a reservation (noted as the remaining narrow case in
	// spawnEntry).
	sessions map[string]*Entry
	// spawning holds the reservation for each key a goroutine is currently
	// spawning under (tether#60): a key is claimed here BEFORE provider.Spawn is
	// called and released AFTER the entry lands in sessions, so a second caller for
	// the same key waits for the first instead of starting a rival agent.
	//
	// That release-after-register order is load-bearing beyond tidiness: it is what
	// lets tether#78 treat "no reservation for this key" as "sessions[key] already
	// reflects the last completed spawn", which is the whole basis for consulting
	// sessions at claim time rather than trusting the caller's earlier check.
	//
	// Keyed by the SESSION KEY spawnEntry computed, not by either of the two config
	// fields it is derived from — keying on cfg.ResumeSessionID would collapse every
	// fresh spawn onto "" and hand unrelated clients one agent, and cfg.SessionID
	// would do the same to every resume. See spawnEntry.
	spawning map[string]*spawnReservation
	locks    map[string]*SessionLock
	// shellResize is keyed by the sid a /wt/shell connection was opened with,
	// and holds that PTY's resize func (tether#68). Deliberately NOT part of
	// Entry: a shell can exist for a sid that has no chat Entry at all (the
	// sid may even be ""), so hanging it off sessions would make resize
	// unroutable in exactly the cases where the pane is already on screen.
	// One shell per sid is already the invariant — locks is keyed the same way.
	shellResize map[string]func(cols, rows uint16) error
	providers   map[string]agent.AgentProvider
	// mintedIDIgnored records the providers already observed to report a session id
	// other than the one they were spawned under, so rekey's self-check warns ONCE
	// per provider instead of once per session. Guarded by mu.
	mintedIDIgnored map[string]bool
	PermEndpoint    string        // injected into cc subprocess env if non-empty
	History         *HistoryStore // nil = history disabled
	// Workdir is the DEFAULT agent subprocess cwd — the resolved
	// --workspace-root; "" = daemon cwd. Wired by internal/server/lifecycle.go
	// Step 3b once the workspace root is resolved (tether#51) — Step 1 builds the
	// Registry before wsRoot is known, so it can't be set at construction time.
	//
	// Since tether#52 it is the fallback, not the answer: a session that selected
	// a workspace runs in that workspace's path instead (see workspace.go). This
	// stays the cwd for every session that selected none, which is what keeps a
	// client that sends no `ws` behaving exactly as it did before.
	Workdir string
	// Workspaces resolves a client-supplied workspace id to a path. nil = this
	// daemon cannot honour a `ws` request at all, and says so rather than
	// substituting a directory of its own (tether#52 — see resolveWorkspace).
	// Wired from internal/workspace.Registry in lifecycle.go Step 2b.
	Workspaces WorkspaceLookup
	// Bindings remembers which workspace each session belongs to, across daemon
	// restarts. nil = not remembered, in which case a reconnect can only land in
	// the workspace its client asks for (or the default). Wired in lifecycle.go
	// Step 2a alongside History, which shares its directory.
	Bindings *BindingStore
}

// spawnOutcome says where the *Entry a spawnEntry call returns came from. The
// three cases are not interchangeable at the call sites, which is why this is not
// a bool: two of them mean "no process was started by this call", and the callers
// need that, but only one of them means "and the session was already answering",
// which is a different recovery posture (see Attach).
type spawnOutcome int

const (
	// spawnNoEntry is the ZERO value, and it deliberately means "there is no entry;
	// read the error". Ordering it first is what keeps a caller that forgets the
	// error from reading a failed call as one that started a session: startedProcess
	// answers false here, so no fallback eligibility and no spent budget can be
	// derived from a call that produced nothing.
	spawnNoEntry spawnOutcome = iota
	// spawnStarted: this call is the one that ran provider.Spawn and registered the
	// entry. It owns the session, so it is the one that may fall back if the resume
	// it started never confirms.
	spawnStarted
	// adoptedAfterWait: another caller held the key's reservation and this one
	// waited for it (tether#60). The entry it gets is an UNCONFIRMED spawn — the
	// winner has only just started it — so this caller must not be armed for
	// recovery before Resolve confirms it.
	adoptedAfterWait
	// adoptedRegistration: the key was already registered to a live session when
	// this caller claimed it (tether#78). Unlike the case above, that session has
	// been through Alive() — the same evidence Attach's own reuse gate acts on — so
	// this caller is in the position of a REUSE, not of a waiter.
	adoptedRegistration
)

// startedProcess reports whether this call started the agent behind the entry it
// returned. It is the question both collidable call sites ask (may I fall back?
// have I spent my one re-open?), and asking it by name rather than comparing
// against a constant is what stops a third outcome from silently joining the
// wrong side of a `!= spawnStarted`.
func (o spawnOutcome) startedProcess() bool { return o == spawnStarted }

// String makes the constant legible in a test failure or a log line. Worth having
// rather than reading small integers out of an assertion message: the whole point
// of the type is that three of its values are easy to confuse.
func (o spawnOutcome) String() string {
	switch o {
	case spawnNoEntry:
		return "spawnNoEntry"
	case spawnStarted:
		return "spawnStarted"
	case adoptedAfterWait:
		return "adoptedAfterWait"
	case adoptedRegistration:
		return "adoptedRegistration"
	}
	return fmt.Sprintf("spawnOutcome(%d)", int(o))
}

// spawnReservation is one goroutine's claim on a session key, held from before
// provider.Spawn until after the entry is registered — the window tether#54
// narrowed, tether#60 made callers wait on, and tether#78 closed by making the
// claim the point at which the map is consulted (see spawnEntry).
//
// # Why a reservation rather than a retry or a bigger lock
//
// The window is between "this key is not in sessions" and "this entry is". Two
// callers that both look before either registers both spawn, and the second
// registration OVERWRITES the first, leaving an agent nobody can reach and two cc
// appending to one transcript. Holding r.mu across the spawn would close it and is
// not an option: that is a registry-wide lock held across process creation, so one
// slow exec would stall every unrelated lookup in the daemon.
//
// Claiming the key first inverts the problem. The loser never calls Spawn at all,
// which is what makes this tractable — an OPTIMISTIC design (both spawn, then
// reconcile) has to answer what becomes of the loser's already-running subprocess,
// and that question has no good answer here: Close() is stdin.Close() + cmd.Wait(),
// and calling Wait while readLoop may still be draining a buffered stdout is the
// os/exec race teardown documents at length. A pessimistic claim means there is no
// second subprocess to dispose of.
//
// # Reading the result
//
// done is closed exactly once, by the goroutine holding the reservation, AFTER it
// has written entry and err. Every waiter reads those only after receiving from
// done, so the close is the happens-before edge and no other synchronisation is
// needed. A spawn that fails publishes its error here too, so waiters fail the same
// way rather than piling on a retry of something that just did not work.
type spawnReservation struct {
	done  chan struct{}
	entry *Entry
	err   error
}

// Entry is the per-session bundle of agent.Session + subscriber set. Exposed
// so callers can Subscribe BEFORE the agent has emitted its system/init,
// avoiding the first-event drop race in serveChat (see Subscribe docs).
type Entry struct {
	sess          agent.Session
	subs          map[chan wire.Envelope]struct{}
	subsMu        sync.RWMutex
	ownerClientID string
	fenceParser   *FenceParser // D-19 fenced-block extraction (tether#8 T6); one per session
	// regKey is the key this entry is registered under in Registry.sessions —
	// its sid, known before the agent process existed (see spawnEntry). Guarded
	// by Registry.mu (NOT subsMu), because it is part of the map's shape rather
	// than of the entry's own state: Registry.rekey moves a registration and
	// updates this in the same critical section, so the two can never disagree.
	//
	// It exists only for the provider that cannot be told its session's id
	// (tether#54 — see rekey); for cc it is written once and never changes.
	regKey string
	// provider is the AgentProvider name this session was spawned from, carried so
	// rekey's self-check can name it. Immutable after construction.
	provider string
	// workdir is the cwd this session's agent was actually spawned in, and ws the
	// workspace that decided it (zero when none was selected). Both immutable
	// after construction — written before the entry is published under
	// Registry.mu, which is what makes every later read of them race-free.
	//
	// They are recorded on the ENTRY, not merely on disk, because the live
	// registration is the only thing that knows where a running process is: the
	// reconnect decision compares against it (see sessionBinding), and the shell
	// pane resumes a chat session by asking for it (WorkdirForSession).
	workdir string
	ws      WorkspaceBinding
}

// Session returns the underlying agent.Session.
func (e *Entry) Session() agent.Session { return e.sess }

// Subscribe registers ch to receive every wire.Envelope produced by this
// session's fanOut. Safe to call before the session's real sid is known —
// this is the path that closes the first-event drop window between
// "agent emitted its init event" and "serveChat called Subscribe by sid".
func (e *Entry) Subscribe(ch chan wire.Envelope) {
	e.subsMu.Lock()
	e.subs[ch] = struct{}{}
	e.subsMu.Unlock()
}

// Unsubscribe removes ch from the subscriber set.
func (e *Entry) Unsubscribe(ch chan wire.Envelope) {
	e.subsMu.Lock()
	delete(e.subs, ch)
	e.subsMu.Unlock()
}

func NewRegistry(providers ...agent.AgentProvider) *Registry {
	pm := make(map[string]agent.AgentProvider, len(providers))
	for _, p := range providers {
		pm[p.Name()] = p
	}
	return &Registry{
		sessions:        make(map[string]*Entry),
		spawning:        make(map[string]*spawnReservation),
		locks:           make(map[string]*SessionLock),
		shellResize:     make(map[string]func(cols, rows uint16) error),
		providers:       pm,
		mintedIDIgnored: make(map[string]bool),
	}
}

// GetLock returns (or lazily creates) the SessionLock for the given sid.
func (r *Registry) GetLock(sid string) *SessionLock {
	r.mu.Lock()
	defer r.mu.Unlock()
	if l, ok := r.locks[sid]; ok {
		return l
	}
	l := &SessionLock{}
	r.locks[sid] = l
	return l
}

// liveEntry looks sid up and returns its Entry ONLY if the agent behind it is
// still alive. It is the single answer to "may this registered session be
// reused?", shared by every caller that used to ask the weaker question — is sid
// a key in r.sessions.
//
// # Registered is not alive (tether#55)
//
// An Entry outlives its agent. fanOut removes it in a deferred evict that runs
// only after the agent's Events() channel has closed AND drained, so between "cc
// exited" and "the map forgot it" the entry is still there, still answering
// SessionID() with the id it cached at init, still reporting an owner. Every
// signal a reconnect used to consult says healthy. Reusing it hands the client a
// corpse: Resolve confirms the session, session_ready goes out, and each prompt
// after that dies in a broken pipe that only reaches a slog.Warn — "thinking…"
// forever, the exact failure tether#49 set out to end, reached through the one
// door tether#50 left open.
//
// Alive() is asked rather than anything process-shaped on purpose; see
// agent.Session.Alive for why a `kill(pid, 0)` probe would call every corpse
// alive, and why it cannot block.
//
// # It also un-registers what it finds dead
//
// Dropping the corpse here is not tidying, it is what keeps the answer stable:
// otherwise the very next lookup of the same sid — from this same reconnect, or
// a concurrent one — re-finds it and has to re-derive that it is dead, and the
// dead sid keeps reading as live to IsLive/Subscribe/DeliverAction in between.
// Attachment.resolve already does exactly this on the failed-resume path for the
// same reason. evict is idempotent and by-value, so the entry's own teardown
// doing it again is a no-op, and it cannot take out the replacement session that
// re-keys under this sid moments later.
//
// The agent behind a dead entry is NOT reaped here, and that is not an omission:
// this function knows the session is over, but not that readLoop has stopped
// reading its stdout, which is the precondition Session().Close() needs (see
// teardown). Reaping stays with the entry's own fanOut, which is by now unwinding
// toward its teardown defer and will get there whether or not this ran.
// Un-registering early only stops the corpse from being handed to the next
// reconnect.
func (r *Registry) liveEntry(sid string) (*Entry, bool) {
	if sid == "" {
		return nil, false
	}
	r.mu.RLock()
	e, ok := r.sessions[sid]
	r.mu.RUnlock()
	if !ok {
		return nil, false
	}
	// Deliberately outside r.mu: Alive() is documented non-blocking, but holding
	// the registry-wide lock across a call into an agent implementation is how a
	// future non-conforming one would take the whole daemon down with it.
	if !e.sess.Alive() {
		slog.Info("dropping a registered session whose agent has exited", "sid", sid)
		r.evict(e)
		return nil, false
	}
	return e, true
}

// GetOrSpawnEntry returns the *Entry for the given sid, or spawns a new agent
// process and registers a fresh Entry under a newly minted sid. providerName
// selects the AgentProvider; defaults to "claude-code" if empty.
//
// Returning *Entry (rather than just agent.Session) lets the caller call
// Entry.Subscribe BEFORE sending the first prompt — necessary because the
// session ID is only published AFTER cc consumes a prompt, and any text
// events produced in between would otherwise be fanned out to zero
// subscribers.
func (r *Registry) GetOrSpawnEntry(ctx context.Context, sid, providerName string) (*Entry, error) {
	if e, ok := r.liveEntry(sid); ok {
		return e, nil
	}

	// Spawn a FRESH session — this entry point never `cc --resume`s (tether#49).
	// A sid that IS live and whose agent is still running returns early above
	// (the real reconnect-continuity path). Two different states reach here with
	// a non-empty sid, and only the first is the tether#49 story:
	//
	//   - a sid the daemon no longer tracks (daemon restart, post-disconnect
	//     eviction, a different workspace/cwd). Resuming one of these used to
	//     wedge the turn in "thinking…": cc exits with "No conversation found"
	//     BEFORE system/init, which parked the caller forever in SessionID() and
	//     broke-piped the first prompt, until tether#49 taught SessionID() to
	//     watch the process-exit `done` channel.
	//   - a sid it DOES track whose agent has already exited, dropped by
	//     liveEntry (tether#55). This one usually HAS a transcript on disk, so a
	//     `--resume` of it would most likely succeed — which is why Attach sends
	//     it down the resume path and only this always-fresh entry point does
	//     not.
	//
	// tether#50 does NOT change that here. Recovering from a failed resume needs
	// state this function does not have — a buffer of unconfirmed prompts to
	// replay — so the try-resume-then-fallback path lives in Attach/Attachment
	// (attach.go) and is opt-in. Keeping the always-fresh behaviour on this entry
	// point means a caller that has no recovery story cannot accidentally acquire
	// a resume it cannot recover from.
	//
	// The zero WorkspaceBinding is likewise not a placeholder: this entry point has
	// no workspace to select from and no client to have chosen one, so it spawns in
	// the daemon default. Selecting a workspace requires the validation and the
	// remembering that only Attach does (tether#52).
	//
	// The outcome is discarded rather than handled: this is a fresh spawn, so
	// spawnEntry mints an id no other goroutine can be holding — the reservation is
	// uncontended and in practice nothing can already be registered under a just-minted
	// id, so neither adoption is reachable. "In practice" is deliberate:
	// agent.NewSessionID falls back to a FIXED uuid if crypto/rand fails, and two fresh
	// spawns that both took that fallback would collide — unreachable on Linux, latent
	// since tether#60's waiter path, and cheaper to name than to re-derive. That is a property of the empty SpawnConfig, not
	// an assumption about callers — see spawnEntry's tether#60/#78 notes.
	e, _, err := r.spawnEntry(ctx, providerName, agent.SpawnConfig{}, WorkspaceBinding{})
	return e, err
}

// spawnEntry starts a new agent subprocess, wraps it in an Entry, registers the
// Entry under its sid and starts its fanOut loop. cfg is the caller's spawn
// intent; Env is filled in here (it is registry-wide, not per-call), so callers
// choose between "pin this freshly minted id" and "resume this existing one", and
// — since tether#52 — which workspace to run in.
//
// ws is an ALREADY-VALIDATED workspace, or the zero value for the daemon default.
// This function does not resolve ids and must not be given one: validation lives
// in resolveWorkspace, reached only through Attach, so that a caller cannot spawn
// into a directory of its choosing by skipping the check.
//
// A fresh spawn always pins a minted SessionID (tether#50): cc adopts it and
// echoes it on init (mem_2ruSlrHR ①), so the daemon knows the session's id — and
// therefore its on-disk transcript name — before cc has emitted a single byte.
// Empty-vs-minted is not left to the caller because an unpinned fresh session is
// exactly the un-resumable state this slice exists to eliminate.
//
// # The sid is known BEFORE the process exists, so registration is synchronous
//
// Both spawn intents name the session up front: a fresh one under the id just
// minted for it, a reconnect under the id it is resuming. So the key this entry
// belongs under is computable before provider.Spawn is called, and the entry is
// in the map before this function returns — no window, nothing to re-key
// (tether#54).
//
// What that replaces was a `pending-%p` placeholder key plus a goroutine parked
// in sess.SessionID() to move the entry to its real sid once cc announced it. It
// existed because before tether#50 the daemon did not choose the id: under
// `--input-format stream-json` cc emits nothing at all until the first prompt
// arrives, so "the entry's real key" was genuinely unavailable until the user
// typed. Two defects grew in that window, both now closed by construction rather
// than by care:
//
//   - Attachment.Resolve waits on the SAME SessionID() wakeup the re-key
//     goroutine did, so it routinely returned while the map still held only the
//     placeholder. Any ownership/liveness question asked by sid at that moment
//     answered "no such session" about a session that was right there — which
//     serveChat read as a fatal ownership race and answered by dropping the
//     connection mid-answer.
//   - A second attach carrying the same sid inside the window missed and spawned
//     its own `cc --resume` of the same transcript; both then re-keyed to the same
//     map key, so one entry silently displaced the other and the displaced cc kept
//     running unreachable.
//
// Registering under the known key also makes the SECOND attach find the first
// one (via Attach's liveEntry) instead of duplicating it. That is a reuse of an
// as-yet-unconfirmed resume, and the consequence is deliberate: see the note in
// Attach.
//
// # Two callers for one key wait rather than race (tether#60)
//
// The paragraph above says the key is computable before Spawn. It is — but knowing
// the key is not holding it, and until tether#60 nothing did: two callers that both
// passed their own pre-check (Attach's liveEntry gate, or reopen's sibling-adoption
// check) before either reached the registration below would both Spawn, and the
// second `r.sessions[key] = e` would DISPLACE the first. The displaced entry and its
// agent were exactly as unreachable as under the placeholder scheme tether#54
// retired; what tether#54 changed was how long the door stood open, not what was
// behind it.
//
// A key is now claimed in r.spawning before Spawn is called and released when the
// entry is published, so the second caller blocks on the first's reservation and
// returns its entry with adoptedAfterWait. See spawnReservation for why the claim
// comes FIRST — a design where both spawn and then reconcile has to dispose of the
// loser's running subprocess, and there is no safe way to do that here.
//
// The outcome is returned rather than hidden because the two collidable call sites
// must each decide what it means for them, and a value the compiler forces them to
// accept is what makes that decision visible: Attach uses it to keep an adopter
// non-fallback-eligible (two attachments falling back would fork the conversation in
// two, which is what --fork-session is for), and reopen uses it to leave its
// one-per-attachment reopen budget unspent, since an adopter started no process.
// The two fresh-spawn call sites cannot collide at all — they mint a unique id, so
// no other goroutine can be holding that key.
//
// # The caller's pre-check is now an optimisation, not the gate (tether#78)
//
// Waiting on the reservation left a window, and it was not the obvious one. The
// caller's own pre-check and this claim are separate critical sections, so a caller
// whose check ran before the winner registered but which did not reach the claim
// until after the winner's reservation was RELEASED found nothing to wait for,
// spawned, and displaced the registration exactly as before tether#60. Narrower than
// "arrive anywhere inside the spawn", but reproducible with no concurrency at all:
// two sequential calls for one key were enough.
//
// What closes it is one read — r.sessions[key], taken in the same critical section as
// the claim — plus WHERE the liveness question is asked. Note the shape of the
// argument, because it is not the one the old doc predicted:
//
//   - The winner registers its entry BEFORE the deferred release runs. So "there is
//     no reservation for this key" implies "any registration a completed spawn of this
//     key made is already visible", and the read under the claim cannot be stale in
//     the one direction that matters. (A spawn that FAILED registers nothing, which is
//     why the implication is phrased about registrations rather than about outcomes:
//     the read then returns nil and this caller spawns, which is correct.)
//   - Correctness rests on HOLDING the reservation, not on the check and the claim
//     being atomic. That is what makes the out-of-lock Alive() sound: while this
//     goroutine holds the key, no rival SPAWN can register under it, so the entry
//     cannot be replaced between the read and the verdict — and liveEntry's rule
//     against calling into an agent implementation under r.mu is left intact. Note the
//     word: rekey registers without holding anything, and is the case named below.
//
// So a late caller ADOPTS the live registration (adoptedRegistration) instead of
// starting a rival, and a registration whose agent has died is un-registered by
// liveEntry and replaced by the spawn below. The pre-check at each call site still
// earns its keep — it answers "reuse or resume?" before any of this — but it is no
// longer load-bearing for "will two agents end up under one sid".
//
// What this still does NOT cover, stated rather than implied:
//
//   - It only helps a caller that REACHES the claim. Attachment.reopen's spent gate
//     returns before it does, so an attachment that has already used its one re-open
//     still depends on winning its own sibling-adoption check: lose that and its
//     prompt is refused even though a live session is registered under the sid. No
//     duplicate agent, and not a regression — that ordering predates this — but it is
//     the one cost of a lost pre-check that consulting r.sessions here cannot remove,
//     because nothing here runs. tether#82 tracks it; see reopen for the reproduction.
//   - rekey moves a registration into an arbitrary key WITHOUT holding a reservation,
//     so it could in principle place an entry under a key someone is spawning under.
//     It is not reachable for cc (which adopts the id it is given, so rekey is a
//     no-op) and needs a provider that mints its own id to announce exactly the key in
//     flight; the fresh-spawn case would additionally need it to collide with a just-
//     minted uuid. Strictly narrower than what is closed above, and named here rather
//     than left for the next reader to rediscover.
//   - A waiter receives the winner's entry even if that entry has since been evicted
//     (it can die between registration and the waiter's read). That is the same
//     "adopted a session that then died" shape every other reuse has, and it is
//     recovered by the same machinery (Attachment.reopen, tether#59/#76) rather than
//     by a second check here.
func (r *Registry) spawnEntry(ctx context.Context, providerName string, cfg agent.SpawnConfig, ws WorkspaceBinding) (entry *Entry, outcome spawnOutcome, err error) {
	if providerName == "" {
		providerName = "claude-code"
	}
	provider, ok := r.providers[providerName]
	if !ok {
		return nil, spawnNoEntry, refuse(wire.ErrCodeUnknownProvider, "unknown provider: %s", providerName)
	}

	if r.PermEndpoint != "" {
		// Copy rather than append in place: cfg arrives by value but its Env slice
		// still shares a backing array with the caller's, so appending into spare
		// capacity would mutate a slice we do not own. No current caller passes a
		// non-nil Env, which is exactly why this would be found the hard way.
		env := make([]string, 0, len(cfg.Env)+1)
		env = append(env, cfg.Env...)
		cfg.Env = append(env, "TETHER_DAEMON_PERM_ENDPOINT="+r.PermEndpoint)
	}
	cfg.Workdir = r.workdirFor(ws)
	if cfg.ResumeSessionID == "" && cfg.SessionID == "" {
		cfg.SessionID = agent.NewSessionID()
	}

	// The two fields are mutually exclusive (see agent.SpawnConfig), and the
	// branch above guarantees one of them is set, so exactly one of these is the
	// session's id and key is never empty.
	key := cfg.SessionID
	if key == "" {
		key = cfg.ResumeSessionID
	}

	// Claim the key, or wait for whoever holds it (tether#60). Taken and released
	// under r.mu, but NOT held across the Spawn below — the reservation is a map
	// entry, not a lock, which is the whole point: it excludes a rival spawn for
	// this one key while leaving every other lookup in the daemon unblocked.
	//
	// registered is read in the SAME critical section as the claim, and that is the
	// whole of tether#78 (see the section in this function's doc). It is what the
	// caller's own pre-check could not tell it: whether somebody finished spawning
	// this key while this goroutine was between its check and here.
	r.mu.Lock()
	if res, ok := r.spawning[key]; ok {
		r.mu.Unlock()
		return r.awaitSpawn(ctx, key, res, cfg.Workdir)
	}
	res := &spawnReservation{done: make(chan struct{})}
	r.spawning[key] = res
	// `key`, not either field it was derived from. Substituting cfg.SessionID here is
	// caught (it is "" on every resume, so the consult never fires and the residual is
	// back); substituting cfg.ResumeSessionID is an EQUIVALENT mutant TODAY and no test
	// can catch it — on a resume the two strings are identical, and on a fresh spawn
	// r.sessions[""] is always nil because nothing is ever registered under "". It stops
	// being equivalent the moment a caller sets BOTH fields, which agent.SpawnConfig
	// currently forbids. Written down because "no test failed" is not why this is right.
	registered := r.sessions[key]
	r.mu.Unlock()

	// Release on EVERY path, including the panicking one and the adoption below.
	// Publishing the outcome before the close is what lets waiters read it with no
	// lock of their own (see spawnReservation); the named returns are read here
	// rather than threaded through so that a future early return cannot forget to
	// report itself.
	defer func() {
		r.mu.Lock()
		delete(r.spawning, key)
		r.mu.Unlock()
		res.entry, res.err = entry, err
		close(res.done)
	}()

	// Somebody had already finished spawning this key (tether#78). Ask the one
	// question that decides whether it may be adopted, and note WHERE this happens:
	// under the reservation, not under r.mu. That is what makes the check
	// authoritative without breaking liveEntry's rule — the reservation excludes
	// every rival spawn for this key, so nothing can register here while we are
	// asking, and Alive() is still called with no lock held.
	//
	// A corpse falls through to the spawn below rather than being adopted, which is
	// the whole reason this cannot be a bare map test: adopting a registered-but-dead
	// entry is tether#55, and it is the failure this daemon is worst at reporting
	// (the turn never returns). liveEntry also un-registers it, so the registration
	// this function performs below replaces it rather than racing it.
	//
	// In the ORDINARY case this costs nothing at all — both collidable callers have
	// already evicted whatever they found dead (Attach's gate through liveEntry,
	// reopen through its explicit evict), so registered is nil and no Alive() is
	// asked here. It fires only for the caller that lost the race this closes.
	if registered != nil {
		if e, ok := r.liveEntry(key); ok {
			if e.workdir != cfg.Workdir {
				// Fail closed, for the reason awaitSpawn states at length: adopting an
				// entry from another directory silently relocates a conversation. Note
				// that reopen's sibling-adoption branch does NOT check this and argues it
				// is unreachable; the argument is about today's callers, and this is the
				// same assertion awaitSpawn already makes one branch away, so the two
				// adoption paths agree rather than each trusting a different premise.
				//
				// A waiter parked on our reservation inherits this error even if its own
				// resolved directory would have matched. That is an accepted imprecision
				// in a case believed unreachable: it costs the waiter a reconnect, and
				// the alternative is machinery to republish per-waiter verdicts for a
				// state no call site can currently produce.
				return nil, spawnNoEntry, fmt.Errorf(
					"refusing to adopt session %s: it is registered from %q but this connection resolved %q",
					key, e.workdir, cfg.Workdir)
			}
			// Info, not Debug, and the reason is a fact about this daemon rather than a
			// preference: nothing anywhere calls slog.SetDefault or sets a level, so the
			// default handler drops Debug and a Debug line here would be unobservable on
			// a real daemon — which is exactly how live-verifying this change ran into
			// "the count is right but I cannot see WHICH path produced it". The event
			// also deserves it: it means two clients contended for one sid, it happens
			// only when a race is lost, and reopen's sibling-adoption branch — the same
			// event one layer up — already logs at Info. (awaitSpawn's tether#60 line has
			// the same invisibility problem; left alone, not this wi's to change.)
			slog.Info("adopted a session that was already registered under this key",
				"sid", key, "provider", e.provider)
			return e, adoptedRegistration, nil
		}
	}

	sess, err := provider.Spawn(ctx, cfg)
	if err != nil {
		return nil, spawnNoEntry, refuse(wire.ErrCodeSpawnFailed, "spawn: %w", err)
	}

	e := &Entry{
		sess:        sess,
		subs:        make(map[chan wire.Envelope]struct{}),
		fenceParser: NewFenceParser(),
		regKey:      key,
		provider:    providerName,
		workdir:     cfg.Workdir,
		ws:          ws,
	}

	// Registered BEFORE fanOut starts, and the order is load-bearing rather than
	// tidy: fanOut is what calls rekey, and rekey deliberately moves an existing
	// registration instead of creating one, so an entry whose init were processed
	// before this insert would be left permanently unreachable by its real sid.
	// (Not pinned by a test — staging it needs a hook between these two
	// statements — so treat the order as an invariant, not a preference.)
	r.mu.Lock()
	r.sessions[key] = e
	r.mu.Unlock()

	// Remember where this session lives, so a reconnect that brings only a sid
	// still lands in the right directory — including after a daemon restart, which
	// is the case the ENTRY above cannot answer (tether#52). Recorded under the
	// registration key, i.e. the id the client will come back with. A provider that
	// mints its own id is registered under the pinned one until it announces; rekey
	// re-records it there.
	//
	// Only for a session whose id we just MINTED (cfg.SessionID set ⇒ a fresh
	// spawn; see the mutual exclusion in agent.SpawnConfig). A resume must not write
	// one: the key would be the CLIENT's sid, and the record would assert where that
	// session lives on the strength of a request rather than of having created it
	// there. resolveWorkspace now only ever resumes in the session's own directory,
	// so such a write would at best restate what is already on disk — and at worst,
	// if that reasoning were ever loosened again, silently rebind someone else's
	// session. Writing only what we created keeps the file's meaning exact.
	if cfg.SessionID != "" {
		r.saveBinding(key, ws)
	}

	// background goroutine: fan out events to subscribers
	go r.fanOut(e)

	return e, spawnStarted, nil
}

// awaitSpawn blocks until the goroutine holding key's reservation has published its
// outcome, and returns that outcome as this caller's own (tether#60).
//
// # Why the waiter adopts instead of retrying
//
// The winner is spawning under the SAME key, which for the two call sites that can
// collide means the same sid and therefore the same transcript. Retrying after it
// lands would spawn a second `cc --resume <sid>` — precisely the state being closed.
// Waiting is not a fallback here, it is the answer.
//
// A failed spawn is republished rather than retried for the same reason resolve does
// not retry a fresh session that died: whatever stopped the winner (a missing binary,
// a resource limit) is about to stop the waiter too, and one report is more useful
// than N.
//
// # ctx, and why the wait is bounded by it rather than by a timer
//
// The waiter's ctx is its client connection. A browser that goes away must not stay
// parked on someone else's exec, and there is nothing to recover for it. The
// reservation itself is NOT abandoned on this path — it belongs to the winner, whose
// own defer releases it — so leaving early costs nothing and cannot strand the key.
//
// # The workdir check is an assertion, not a policy
//
// Adopting an entry spawned in a different directory would silently relocate this
// connection's conversation, which is the failure resolveWorkspace exists to prevent.
// It is BELIEVED unreachable, and the belief is one step short of a proof, which is
// why the check exists rather than an assumption. Both collidable call sites derive
// their workspace from the session's own binding (resolveWorkspace for Attach, a.ws —
// itself set from that same decision — for reopen), and a connection that rebinds
// gets ResumeSID "" and therefore a freshly minted key, so it never contends here at
// all. What is not proven is that the binding cannot CHANGE between the two callers'
// reads: rekey writes one mid-life for a provider that mints its own id. For cc no
// binding is ever written for a resumed sid, so no such change is reachable today —
// but that is a fact about cc and about saveBinding's current callers, not about this
// function. Failing closed costs a reconnect; being wrong costs a conversation moved
// to another directory without anyone being told.
func (r *Registry) awaitSpawn(ctx context.Context, key string, res *spawnReservation, wantWorkdir string) (*Entry, spawnOutcome, error) {
	select {
	case <-res.done:
	case <-ctx.Done():
		return nil, spawnNoEntry, ctx.Err()
	}
	if res.err != nil {
		return nil, spawnNoEntry, res.err
	}
	e := res.entry
	if e == nil {
		// "No error and no entry" is not a state spawnEntry can reach today — it
		// publishes the named returns, and every return with a nil entry carries an
		// error. It IS what a PANIC inside provider.Spawn publishes, because the
		// deferred release still runs while the named returns are zero, and it is
		// what a future `return nil, spawnNoEntry, nil` would publish. Neither should be
		// answered by dereferencing nil in the waiter: the winner's panic is the
		// winner's problem, and a waiter that reports it is far easier to read than
		// a second goroutine faulting on the same instruction.
		return nil, spawnNoEntry, fmt.Errorf("the spawn of session %s reported neither an entry nor an error", key)
	}
	if e.workdir != wantWorkdir {
		return nil, spawnNoEntry, fmt.Errorf("refusing to adopt session %s: it was spawned in %q but this connection resolved %q",
			key, e.workdir, wantWorkdir)
	}
	slog.Debug("adopted a session another connection was already spawning",
		"sid", key, "provider", e.provider)
	return e, adoptedAfterWait, nil
}

// rekey MOVES e's registration to sid. It never creates one, and that is the
// whole safety argument: an entry evicted concurrently (by liveEntry finding it
// dead, or by Attachment.resolve dropping a failed resume) is no longer under
// its own key, so this becomes a no-op and cannot resurrect it. tether#12 needed
// an `evicted` flag for that because its re-key ran on a goroutine of its own
// with no relationship to eviction; this one is called from fanOut, i.e. from the
// same goroutine whose deferred evict ends the session — so "re-keyed after
// eviction" is not merely guarded against, it is unreachable in program order
// for the ordinary teardown, and the move-not-create rule covers the two
// concurrent evictors.
//
// # Who needs it at all
//
// Nobody, for cc: spawnEntry already registered the entry under the id it told
// cc to adopt, and cc echoes that id back on every system/init (mem_2ruSlrHR ①),
// so sid == e.regKey and this returns without touching the map. The call is kept
// on that path ANYWAY, as the self-check the id-minting never had: if cc's
// `--session-id` adoption ever regresses, the daemon says so in the log and ends
// up correctly keyed, instead of silently reverting to pre-tether#50 semantics
// (an orphaned history directory and a transcript that truncates on reload, with
// Resolution.Recovered false so the user is not even told).
//
// It IS load-bearing for OpenCodeProvider, which ignores SpawnConfig.SessionID
// and mints its own id inside `opencode serve`. Such a provider cannot be keyed
// correctly at spawn time by anyone, so the id has to be adopted from the event
// that announces it. Note the consequence, which is the residual of tether#54
// rather than a new defect: between Resolve returning (opencode publishes its sid
// to SessionID() waiters from the SSE loop) and fanOut processing the same
// EventInit, a lookup BY SID can still miss. That is why serveChat claims
// ownership through Attachment.SetOwner's *Entry and not by sid.
//
// # The self-check is once per PROVIDER, not once per session
//
// A provider that mints its own id trips the mismatch on every single session, so
// warning each time would bury the signal it exists for under the noise it cannot
// avoid — an operator who has learned to ignore the line cannot use it to notice cc
// regressing. The first mismatch from a given provider is therefore a Warn naming
// that provider (actionable: `provider=opencode` is a known limitation,
// `provider=claude-code` means `--session-id` adoption has broken and sessions are
// silently reverting to pre-tether#50 semantics); every later one is a Debug.
//
// The logging happens AFTER r.mu is released. Writing to a log handler is I/O, and
// holding the registry-wide lock across I/O is the same mistake liveEntry
// deliberately avoids with Alive().
func (r *Registry) rekey(e *Entry, sid string) {
	if sid == "" {
		return
	}
	r.mu.Lock()
	if e.regKey == sid {
		r.mu.Unlock()
		return
	}
	if r.sessions[e.regKey] != e {
		// Already evicted (or displaced by a newer session under the same key):
		// there is no registration to move, and creating one would be a
		// resurrection.
		r.mu.Unlock()
		return
	}
	from := e.regKey
	delete(r.sessions, from)
	r.sessions[sid] = e
	e.regKey = sid
	firstForProvider := !r.mintedIDIgnored[e.provider]
	r.mintedIDIgnored[e.provider] = true
	r.mu.Unlock()

	// The workspace binding follows the registration (tether#52). Without this, a
	// provider that mints its own id would have its binding filed under the id
	// nobody will ever ask about, and every reconnect of that session — the only
	// caller that reads the file — would miss it and fall back to the default
	// directory. Written after the unlock for the same reason the logging below is:
	// this is I/O, and r.mu is registry-wide.
	r.saveBinding(sid, e.ws)

	const msg = "agent reported a session id it was not spawned under; re-keying"
	if firstForProvider {
		// Both readings are spelled out because this line cannot tell them apart —
		// only the provider field can, and printing one diagnosis would print the
		// reassuring one at the exact moment the alarming one is true.
		slog.Warn(msg+" — expected once per session from a provider that mints its own id; "+
			"from one that accepts --session-id it means sessions are silently losing "+
			"their pinned id (pre-tether#50 semantics: orphaned history, truncated reloads)",
			"provider", e.provider, "spawned_under", from, "reported", sid)
		return
	}
	slog.Debug(msg, "provider", e.provider, "spawned_under", from, "reported", sid)
}

// GetOrSpawn is a thin wrapper that hides the Entry behind the Session.
//
// It currently has NO production caller — /wt/events attaches read-only via
// Registry.Subscribe, not through here — so this is API surface kept for callers
// that don't need pre-init subscription, not a path anything exercises. Worth
// knowing before treating it as a constraint on the two spawn paths.
func (r *Registry) GetOrSpawn(ctx context.Context, sid, providerName string) (agent.Session, error) {
	e, err := r.GetOrSpawnEntry(ctx, sid, providerName)
	if err != nil {
		return nil, err
	}
	return e.sess, nil
}

// evict removes e from the sessions map after its session has terminated. On the
// ordinary path it is reached through teardown (which fanOut defers), i.e. once
// the agent's Events() channel has closed — un-registering is only HALF of ending
// a session, and teardown is where the other half, reaping the agent, lives and
// where the argument for its safety is written down. serveChat binds the agent
// subprocess to the client-connection
// context (exec.CommandContext with wtsess.Context()), and both providers
// close Events() when that context is cancelled — cc via readLoop's
// `defer close(events)` after the SIGKILL'd subprocess EOFs, opencode via its
// ctx-done goroutine (closeEvents). So a client disconnect (ctx cancel) closes
// Events() and lands here, the same path as a session that ends on its own.
// That single trigger covers BOTH "session ended" and "client disconnected",
// which is why no idle timer or grace period is needed — the connection
// context already bounds the subprocess lifetime (tether#12).
//
// Eviction is by value: it deletes whatever key maps to e rather than trusting
// e.regKey. Since tether#54 an entry is registered under its sid from before its
// process existed and only Registry.rekey ever moves it, so the two agree — the
// by-value scan is kept because it is what makes this idempotent and
// order-independent for its three callers (teardown, from fanOut's defer;
// liveEntry dropping a corpse; Attachment.resolve dropping a failed resume), and
// because it cannot
// take out a DIFFERENT entry that has since been registered under the same key.
// Deleting during range is safe per the Go spec, and this runs once per session
// lifetime — not on a hot path.
//
// r.locks is deliberately NOT cleaned here: the per-sid SessionLock is shared
// with shell sessions (handleWTShell calls GetLock for the same sid), so its
// lifetime is not bound to this chat Entry — reclaiming it safely needs a
// cross-surface refcount, tracked as a separate follow-up.
func (r *Registry) evict(e *Entry) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for k, v := range r.sessions {
		if v == e {
			delete(r.sessions, k)
		}
	}
}

// teardown ends a session for good: it un-registers the entry, then reaps the
// agent behind it. Called from fanOut's defer and nowhere else — so once per
// session, on the session's own goroutine, after Events() has closed AND drained.
//
// # Why the reap lives here
//
// agent.Session.Close is the only thing in the daemon that waits on the CC
// subprocess (other cmd.Wait calls exist — the PTY shell, the workspace MCP tool,
// opencode's own serve reaper — but none of them can reap a cc), and until
// tether#56 its only production caller was Attachment.resolve, on the
// failed-resume path. Every session that ended the ORDINARY way — the
// overwhelming majority — therefore left behind a `[claude] <defunct>` zombie, a
// goroutine parked in exec.CommandContext's ctx watchdog, and an unclosed stdin
// fd, all held until the daemon itself exited. Measured on a live daemon at one of
// each per chat session.
//
// # Why not in evict, the other obvious candidate
//
// Two independent reasons, either one sufficient:
//
//   - evict runs under r.mu, and cmd.Wait blocks until the child is reaped.
//     Holding the registry-wide lock across a wait on a child process is how one
//     agent that is slow to die stops every OTHER session from being looked up —
//     the same objection liveEntry states for calling Alive() under the lock.
//   - evict has three callers and is deliberately idempotent, and only this one
//     can prove the os/exec precondition below. liveEntry and Attachment.resolve
//     evict a session they know is DEAD; dead is not the same as "nothing is
//     reading its stdout any more".
//
// # The os/exec precondition, spelled out
//
// cmd.Wait must not run while anything is still reading the process's stdout: it
// closes the pipe under the reader, which os/exec documents as incorrect and
// which costs at minimum the last line. Nothing is, and here is the chain:
//
//	the `range e.sess.Events()` in fanOut returned
//	  ⟹ that channel is closed
//	  ⟹ ccSession.readLoop ran its `defer close(s.events)`
//	  ⟹ readLoop's `for scanner.Scan()` loop is over
//	  ⟹ every read of that process's stdout has completed.
//
// That chain is INDEPENDENT of the order readLoop's two defers run in: both of
// them run after the scan loop, and the scan loop ending is the whole of what Wait
// cares about. So this is not a second thing balanced on that hand-checked
// invariant — worth saying, because the invariant is real and nearby, and a reader
// who assumed this depended on it would also assume swapping the defers had to be
// re-argued here. What the order IS load-bearing for is Alive() ("Events() closed
// ⟹ Alive() already false", claude_provider.go); leave it alone for that reason,
// not for this one.
//
// The order does buy one thing here, though: because `events` closes LAST, this is
// the LATER of the two available signals, which makes it a strictly stronger
// precondition than the one Attachment.resolve argues from (`done`). Both are
// sound; the difference is only how much slack each leaves.
//
// What neither argument is, and what a third call site must not be: "the process
// exited, so Wait must be safe". readLoop can still be draining bytes the pipe
// buffered before the process died, and that is precisely the window os/exec warns
// about.
//
// # What it means for the other provider
//
// For OpenCodeProvider this call is a confirmation rather than a reap: its Close
// kills the `opencode serve` child and then waits on the goroutine that already
// owns that child's cmd.Wait, and its events channel is closed only by
// closeEvents — which either Close itself or the Spawn ctx-done teardown has
// already run by the time we get here. Arriving second is therefore free, but NOT
// because Close is once-guarded: it is not. It is idempotent by construction —
// killing an already-reaped process returns ErrProcessDone, which it discards;
// receiving from the already-closed serve-exit channel returns immediately; and
// closeEvents is itself the once-guard. Worth spelling out, because a maintainer
// who reads "idempotent" here and goes looking for a sync.Once in
// opencode_provider.go will not find one and may conclude this call is unsafe.
//
// Note also the case that never reaches here at all: Interrupt() kills the serve
// WITHOUT closing Events(), because a hibernated opencode session is still alive
// and the next SendPrompt relaunches it — so a dormant session is not torn down
// by mistake.
func (r *Registry) teardown(e *Entry) {
	r.mu.RLock()
	sid := e.regKey
	r.mu.RUnlock()

	// Evict FIRST. It is what stops the dead sid from reading as live to the next
	// reconnect, and it must not queue behind a Wait on a child that is slow to
	// die. The reap has no such urgency — nothing observes it but the OS.
	r.evict(e)

	// A non-nil error here is the normal case, not an alarm: the overwhelmingly
	// common teardown is a client disconnect, which cancels the Spawn ctx, which
	// SIGKILLs cc, so Wait reports "signal: killed". Debug, and logged only so an
	// operator chasing a stuck session can see the reap happened at all.
	if err := e.sess.Close(); err != nil {
		slog.Debug("reaped the agent behind an ended session", "sid", sid, "err", err)
	}
}

// RecordUserMessage persists a user-sent message to session history.
func (r *Registry) RecordUserMessage(sid, text string) {
	if r.History != nil && sid != "" {
		r.History.RecordUser(sid, text)
	}
}

// BroadcastAll sends env to every subscriber across all sessions.
func (r *Registry) BroadcastAll(env wire.Envelope) {
	r.mu.RLock()
	entries := make([]*Entry, 0, len(r.sessions))
	for _, e := range r.sessions {
		entries = append(entries, e)
	}
	r.mu.RUnlock()
	for _, e := range entries {
		e.subsMu.RLock()
		for ch := range e.subs {
			select {
			case ch <- env:
			default:
			}
		}
		e.subsMu.RUnlock()
	}
}

// Subscribe registers a channel to receive broadcast envelopes for a session.
// Call Unsubscribe when done.
func (r *Registry) Subscribe(sid string, ch chan wire.Envelope) {
	r.mu.RLock()
	e, ok := r.sessions[sid]
	r.mu.RUnlock()
	if !ok {
		return
	}
	e.subsMu.Lock()
	e.subs[ch] = struct{}{}
	e.subsMu.Unlock()
}

// Unsubscribe removes the channel from broadcast.
func (r *Registry) Unsubscribe(sid string, ch chan wire.Envelope) {
	r.mu.RLock()
	e, ok := r.sessions[sid]
	r.mu.RUnlock()
	if !ok {
		return
	}
	e.subsMu.Lock()
	delete(e.subs, ch)
	e.subsMu.Unlock()
}

// setOwner is the compare-and-set that records ownership. Reached only through
// Attachment.SetOwner, i.e. from a caller that already holds the *Entry.
//
// There is deliberately no sid-keyed Registry.SetOwner wrapper. One existed until
// tether#54 and had no callers left: the chat path stopped using it when its
// lookup turned out to lose a race with the placeholder re-key, and while
// retiring the placeholder makes such a lookup sound for cc, it stays racy for a
// provider that mints its own id (see Registry.rekey). Ownership is asked about
// the entry you are holding; re-deriving it from the map is the shape of the bug.
//
// Returns false only when a DIFFERENT client already owns the session;
// re-claiming by the same client is idempotent.
func (e *Entry) setOwner(clientID string) bool {
	e.subsMu.Lock()
	defer e.subsMu.Unlock()
	if e.ownerClientID != "" && e.ownerClientID != clientID {
		return false
	}
	e.ownerClientID = clientID
	return true
}

// owner returns the client id recorded on this entry, or "" if nobody has claimed
// it. It exists for Attachment.reopen, which carries an existing claim onto the
// session it re-opens: a recovery that silently returned the conversation to unowned
// would let a different client join what it was refused moments earlier.
//
// Read under subsMu, the lock setOwner writes it under — and the guard is not
// decoration even though ownerClientID is a single word: setOwner runs from
// serveChat's own goroutine while reopen reads this from the prompt reader, so an
// unguarded read here is a data race in the ordinary two-goroutine case, not an
// exotic one. It is pinned by TestEntryOwner_ReadIsGuardedAgainstAConcurrentClaim,
// which needs -race to fail (this repo's baseline).
//
// Note what this deliberately is NOT: a sid-keyed Registry.Owner. Ownership is asked
// of the entry you are already holding — re-deriving it from the map is the shape of
// the bug tether#54 removed (see setOwner above).
func (e *Entry) owner() string {
	e.subsMu.RLock()
	defer e.subsMu.RUnlock()
	return e.ownerClientID
}

// IsLive returns true if the session exists AND its agent has not exited.
//
// NOT a pure predicate: like every liveEntry caller it UNREGISTERS a session it
// finds dead (see liveEntry for why the answer has to be made stable rather than
// merely reported). Benign for the polling caller in tests — evicting an already
// dead entry is idempotent and cannot touch a live one — but worth knowing before
// calling this in a loop and expecting it to observe without disturbing.
//
// It goes through liveEntry (not a bare map lookup) so it cannot disagree with
// the reuse decision Attach makes microseconds later on the same sid.
//
// Since tether#54 it has NO production caller: serveChat's admission gate used to
// compose it with IsOwner and now asks OwnedByOther instead, which folds the same
// liveness check in (and is why a dead sid is still nobody's to own — under
// tether's one-human-many-devices model, reconnecting a sid whose agent has exited
// from a SECOND device must recover the conversation, not be told the corpse owns
// it). What remains is a liveness PROBE for tests, including tests in other
// packages, which is the only reason it stays exported.
func (r *Registry) IsLive(sid string) bool {
	_, ok := r.liveEntry(sid)
	return ok
}

// OwnedByOther reports whether sid names a live session that a DIFFERENT client
// has already claimed. It is the question serveChat's admission gate actually
// wants, and asking it in one call is what stops the gate from rejecting a
// session that simply has no owner yet.
//
// # Why this exists rather than IsLive() && !IsOwner()
//
// That composition reads "reject unless this client owns it", and an UNOWNED
// session fails it: IsOwner compares against an empty ownerClientID and says no.
// Ownership is claimed only after Attachment.Resolve confirms the session, and
// for cc confirmation needs the user's first prompt — so "live but not yet
// owned" is a state that lasts as long as the user takes to type.
//
// Before tether#54 the gate got away with it: the entry sat under a placeholder
// key, so IsLive(sid) answered false for that whole window and the gate simply
// did not fire. Registering under the real sid closes that window and would have
// converted it into a false rejection — a second tab of the SAME browser (the
// client id is per-credential, so tabs share it) told "session owned by another
// client", a message the frontend renders as nothing at all before its automatic
// reconnect tries again with the same sid, i.e. a silent reconnect loop until the
// first turn finishes. Which is also exactly the case tether#54 set out to make
// WORK, by letting the second attach reuse the first session.
//
// So the gate's expression is corrected to match its documented intent (#83):
// reject only a session that exists AND is owned by someone else. Two clients on
// one unowned session now proceed into Attach, where the liveEntry gate hands the
// second one the session the first is already using.
// Not a pure predicate: it goes through liveEntry, so asking it about a sid whose
// agent has exited UNREGISTERS that session as a side effect (see liveEntry for why
// the answer is made stable rather than merely reported).
func (r *Registry) OwnedByOther(sid, clientID string) bool {
	e, ok := r.liveEntry(sid)
	if !ok {
		return false
	}
	e.subsMu.RLock()
	defer e.subsMu.RUnlock()
	return e.ownerClientID != "" && e.ownerClientID != clientID
}

// IsOwner returns true if clientID is the recorded owner of sid.
func (r *Registry) IsOwner(sid, clientID string) bool {
	r.mu.RLock()
	e, ok := r.sessions[sid]
	r.mu.RUnlock()
	if !ok {
		return false
	}
	e.subsMu.RLock()
	defer e.subsMu.RUnlock()
	return e.ownerClientID == clientID
}

// tetherAction is the inner payload of a __tether_action__ control message
// (see tetherActionPayload).
type tetherAction struct {
	Action  string `json:"action"`
	BlockID string `json:"blockId"`
	Skill   string `json:"skill"`
}

// tetherActionPayload is the SendPrompt-delivered control payload for a
// fenced-block action callback (D-19 §5, tether#8 T8). It is wrapped in the
// "__tether_action__" marker so the emitting skill can recognize it as a
// control input rather than ordinary user text — documented verbatim in
// docs/wire/fenced-contract.md §5. The daemon (D-20) never interprets DAG
// semantics itself; only the skill decides what "approve" means.
type tetherActionPayload struct {
	Action tetherAction `json:"__tether_action__"`
}

// DeliverAction routes a fenced-block action callback (D-19 §5) to sid's
// underlying agent session by calling SendPrompt with a single-line JSON
// tetherActionPayload. Delivery is generic — DeliverAction does not itself
// decide which actions are meaningful; callers (serveControl) choose which
// actions to deliver at all (only "approve" goes through here; "pause"
// routes to InterruptSession below instead, "rollback" is never wired).
//
// Returns an error (never panics) if sid names no live session. The
// /wt/control channel is not otherwise session-scoped, so an action frame
// naming an unknown or already-ended session is an expected race, not a
// bug — callers should log and drop, not crash.
func (r *Registry) DeliverAction(sid, action, blockID, skill string) error {
	r.mu.RLock()
	e, ok := r.sessions[sid]
	r.mu.RUnlock()
	if !ok {
		return fmt.Errorf("deliver action %q: unknown session %q", action, sid)
	}
	b, err := json.Marshal(tetherActionPayload{
		Action: tetherAction{Action: action, BlockID: blockID, Skill: skill},
	})
	if err != nil {
		return fmt.Errorf("deliver action %q: marshal: %w", action, err)
	}
	return e.sess.SendPrompt(context.Background(), string(b))
}

// InterruptSession routes a DAG-card "pause" click (D-19 §5, tether#8 T9) to
// sid's underlying agent session by calling its agent.Session.Interrupt().
// Unlike DeliverAction, this does NOT go through SendPrompt/
// __tether_action__ — it signals the agent transport directly. For cc that
// means a stream-json control_request{subtype:"interrupt"} written to
// stdin (ccSession.Interrupt in claude_provider.go), which aborts the
// current turn but leaves the subprocess running — the session stays
// resumable via a subsequent SendPrompt/DeliverAction, no respawn needed.
//
// Returns an error (never panics) if sid names no live session — the same
// expected race as DeliverAction (an unknown or already-ended SessionID);
// callers (handleActionFrame) log and drop, they don't crash.
func (r *Registry) InterruptSession(sid string) error {
	r.mu.RLock()
	e, ok := r.sessions[sid]
	r.mu.RUnlock()
	if !ok {
		return fmt.Errorf("interrupt session: unknown session %q", sid)
	}
	return e.sess.Interrupt()
}

// RegisterShellResize records how to resize the PTY behind sid's /wt/shell
// connection (tether#68). handleWTShell calls this right after starting the
// PTY and unregisters on the way out.
//
// A second registration for the same sid replaces the first rather than
// erroring: the shell lock (GetLock) already guarantees one live shell per
// sid, so the only way to reach here twice is a reconnect whose predecessor
// has not finished its deferred unregister — and in that race the newer PTY
// is the one a resize should reach.
func (r *Registry) RegisterShellResize(sid string, fn func(cols, rows uint16) error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.shellResize[sid] = fn
}

// UnregisterShellResize drops sid's PTY resize func. Safe to call for a sid
// that was never registered.
func (r *Registry) UnregisterShellResize(sid string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.shellResize, sid)
}

// ResizeShell applies a client-reported terminal size to sid's PTY (tether#68).
//
// Without this the PTY keeps whatever size it was started with while the
// browser's xterm fits itself to the pane, so the remote TUI renders for one
// width and is displayed at another — the wrapped/clipped Shell tab this
// slice exists to fix.
//
// Returns an error (never panics) when sid has no registered shell — same
// expected race as DeliverAction/InterruptSession, since /wt/control is not
// session-scoped and a resize can arrive after the shell closed. Callers log
// and drop.
func (r *Registry) ResizeShell(sid string, cols, rows uint16) error {
	r.mu.RLock()
	fn, ok := r.shellResize[sid]
	r.mu.RUnlock()
	if !ok {
		return fmt.Errorf("resize shell: no shell for session %q", sid)
	}
	return fn(cols, rows)
}

// Providers returns the names of all registered providers, sorted.
func (r *Registry) Providers() []string {
	names := make([]string, 0, len(r.providers))
	for name := range r.providers {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func truncStr(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// fanOut translates agent.Events into wire.Envelopes and broadcasts to all subscribers.
// It also writes messages to HistoryStore when available.
//
// `sid` is captured from EventInit on the same goroutine, so it is never
// read under a race; both providers (cc, opencode) emit EventInit before
// any EventText / EventResult.
//
// EventText and EventResult get special handling here (rather than going
// through translateEvent) because they run every assistant text chunk
// through e.fenceParser (D-19, tether#8 T6): fenced ```<kind>:<skill>
// blocks are extracted as KindFenced envelopes and suppressed from the
// KindMessage stream — HistoryStore.AccumulateAssistant is fed the
// SUPPRESSED (passthrough) text only, so raw fence JSON never pollutes
// history, EXCEPT the bounded-buffer bail path (D-19 fix #1): a fence body
// that overruns FenceParser's cap is surfaced as ordinary text and DOES
// reach both chat and history, by design (see fenceparser.go doc comment).
// A completed Block is itself ALSO persisted to history (tether#8 T7,
// HistoryStore.AppendBlock), in stream order relative to the surrounding
// text — see emitSegments below — so a page reload can reconstruct the
// same DAG cards the live broadcast rendered, instead of losing them.
// EventResult flushes any text the parser is still holding (e.g. a
// trailing partial line, or an unclosed fence which is discarded) before
// the turn-end KindResult envelope goes out.
//
// EventInit additionally calls e.fenceParser.ResetTurn() on every turn
// (claude-code's cc re-emits system/init per turn — a metadata refresh, not
// a new-session boundary — while it still carries the (unchanged) session
// id every time; opencode's EventInit only fires once at session creation).
// This is a defensive backstop for turns whose EventResult never arrives
// (dropped, or the turn was interrupted): without it a stale open fence
// from the previous turn would swallow the next turn's text forever.
func (r *Registry) fanOut(e *Entry) {
	// When the range below returns, the agent's Events() channel has closed —
	// the session has ended (subprocess exited, or the client disconnected and
	// cancelled the Spawn context that bounds the subprocess). Un-register the
	// entry so long-running daemons don't leak dead sessions in r.sessions
	// (tether#12), and reap the agent so they don't leak its process either
	// (tether#56). Both live in teardown, which is also where the argument for
	// why the reap is SAFE at this exact point is written down.
	defer r.teardown(e)

	var sid string
	// sawInit records whether this session ever produced a system/init. It is set
	// in the same branch as sid and cannot currently diverge from `sid != ""`, so
	// it carries no extra information today — it is a NAME for the condition the
	// empty-result suppression below actually depends on. sid doubles as the
	// history-write gate ("is history addressable"), and a reader of
	// `!sawInit && ev.Text == ""` should not have to work out that the session
	// lifecycle is what is being tested there.
	var sawInit bool

	emitSegments := func(segs []Segment) {
		for _, seg := range segs {
			switch {
			case seg.Block != nil:
				if r.History != nil && sid != "" {
					// Flush any buffered assistant text first so the JSONL
					// order matches the live broadcast order: text-before-
					// block, block, text-after-block (tether#8 T7).
					r.History.FinalizeAssistant(sid)
					r.History.AppendBlock(sid, *seg.Block)
				}
				r.broadcast(e, wire.Envelope{Kind: wire.KindFenced, Payload: *seg.Block})
			case seg.Text != "":
				if r.History != nil && sid != "" {
					r.History.AccumulateAssistant(sid, seg.Text)
				}
				r.broadcast(e, wire.Envelope{Kind: wire.KindMessage, Payload: seg.Text})
			}
		}
	}

	for ev := range e.sess.Events() {
		if ev.Kind == agent.EventInit && ev.SessionID != "" {
			sid = ev.SessionID
			sawInit = true
			// Adopt the id the agent actually reports. A no-op for cc, which was
			// spawned under this very id; the one path that needs it is a provider
			// that mints its own (see rekey). Done HERE, on the loop's own
			// goroutine, so it is ordered before every envelope this session
			// broadcasts and strictly before the deferred evict above.
			r.rekey(e, ev.SessionID)
			e.fenceParser.ResetTurn()
		}
		slog.Debug("fanOut: agent event", "kind", ev.Kind, "text_preview", truncStr(ev.Text, 60))

		switch ev.Kind {
		case agent.EventText:
			emitSegments(e.fenceParser.Feed(ev.Text))
			continue

		case agent.EventResult:
			// A `result` that arrives without ANY preceding system/init, carrying
			// no text, is not a turn ending — it is cc's failed-`--resume`
			// artifact (mem_2ruSlrHR ③): exit 1, stdout exactly one line
			// {"type":"result","subtype":"error_during_execution","result":null,…},
			// no init at all. parseLine turns that `null` into an EventResult with
			// an empty Text.
			//
			// Forwarding it does NOT paint a blank bubble — the frontend's
			// 'result' branch only calls finalizeTurn and never appends a message.
			// What it does is close a turn the user never started: it clears
			// streaming/streamingMsgId/curTurnId and resets `stopped`, so the
			// thinking indicator the browser is showing for the prompt in flight
			// blinks out, moments before the Attachment fallback (attach.go)
			// respawns and answers for real in a new bubble.
			//
			// It is dropped here because fanOut is the single place that knows
			// whether this session ever init'd — the Attachment could suppress it
			// for the chat path, but only after the envelope had already been
			// queued into the subscriber channel, and only for that one consumer.
			//
			// Both halves of the condition are load-bearing, and each is pinned by
			// its own test. ⑤ guarantees a real turn emits init before result, so
			// requiring !sawInit cannot swallow a genuine turn-end (and opencode's
			// EventResult always carries "stop"); requiring an empty Text keeps a
			// hypothetical init-less result that DID carry text visible rather than
			// vanishing. Nothing is buffered in the fence parser at this point
			// either (no init means no text was ever fed), so skipping the Flush
			// loses nothing.
			if !sawInit && ev.Text == "" {
				continue
			}
			emitSegments(e.fenceParser.Flush())
			if r.History != nil && sid != "" {
				r.History.FinalizeAssistant(sid)
			}
			r.broadcast(e, wire.Envelope{Kind: wire.KindResult, Payload: ev.Text})
			continue
		}

		// tether#44 — persist thinking + tool activity so a reload reconstructs
		// the turn's rich content (attached to the turn's assistant history entry
		// at the next FinalizeAssistant). These were broadcast-only until now.
		if r.History != nil && sid != "" {
			switch ev.Kind {
			case agent.EventThinking:
				r.History.AccumulateThinking(sid, ev.Text)
			case agent.EventToolUse:
				if ev.ToolUse != nil {
					r.History.RecordToolUse(sid, ev.ToolUse.ID, ev.ToolUse.Name, ev.ToolUse.Input)
				}
			case agent.EventToolResult:
				if ev.ToolResult != nil {
					r.History.RecordToolResult(sid, ev.ToolResult.ToolUseID, ev.ToolResult.Content, ev.ToolResult.IsError)
				}
			}
		}

		env := translateEvent(ev)
		if env == nil {
			continue
		}
		r.broadcast(e, *env)
	}

	// The stream has ENDED, and if this session ever init'd it may have died in the
	// middle of a turn. Close that turn, because nothing else will (tether#59):
	// ccSession.readLoop just runs `defer close(s.events)` with no terminal event,
	// evict only deletes registry keys, and the subscriber channel is never closed —
	// so a mid-answer death leaves the browser on "thinking…" until its transport
	// dies, which for a live daemon is never.
	//
	// tether#58 already shipped this answer for the OTHER provider, inside the
	// provider: opencodeSession.watchServeExit emits a terminal EventError before
	// closeEvents, and its comment states this same symptom and why honest liveness
	// does not cure it ("Alive() is only consulted on the NEXT attach, so it saves the
	// next turn, not this one"). cc cannot be given the same treatment from inside —
	// readLoop learns of the death by its scan loop simply ending — so the daemon's
	// own seam is where it belongs, and this is the seam: one goroutine per Entry
	// whose whole job is the session's event stream, already deferring teardown on
	// exactly this signal.
	//
	// sawInit is the entire discriminator, and it is what keeps this from undoing
	// tether#50. A FAILED `--resume` produces a stream that closes without ever
	// emitting init, and its turn must stay OPEN so Attachment.resolve can fall back,
	// replay, and let the REPLACEMENT close it — the same condition, for the same
	// reason, as the empty-result suppression above.
	//
	// Flush and FinalizeAssistant, not merely the envelope, so the half-answer the
	// agent did produce is not abandoned in two places: the fence parser (harmless —
	// it dies with the entry) and, load-bearing, HistoryStore's per-SID pending
	// buffer, where the next FinalizeAssistant for that sid would glue it onto the
	// front of whatever answers next under the same id — a recovered session
	// (tether#59) or any later resume. Finalizing here makes the dead session's
	// fragment its own assistant message instead.
	if sawInit {
		emitSegments(e.fenceParser.Flush())
		if r.History != nil && sid != "" {
			r.History.FinalizeAssistant(sid)
		}
		// No payload: KindResult's payload is a stop reason and there is no honest one
		// to report — the agent did not say why it stopped, it stopped saying anything.
		// The frontend's 'result' branch only calls finalizeTurn and paints nothing
		// (web/src/lib/store.ts), and finalizeTurn is idempotent, so this is also free
		// on the ordinary path where a completed turn already closed itself.
		r.broadcast(e, wire.Envelope{Kind: wire.KindResult})
	}
}

// broadcast sends env to every current subscriber of e, dropping (with a
// warning) on any subscriber whose channel is full rather than blocking
// the whole session's fanOut loop.
func (r *Registry) broadcast(e *Entry, env wire.Envelope) {
	e.subsMu.RLock()
	defer e.subsMu.RUnlock()
	slog.Debug("fanOut: broadcasting", "wire_kind", env.Kind, "nsub", len(e.subs))
	for ch := range e.subs {
		select {
		case ch <- env:
		default:
			slog.Warn("fanOut: slow subscriber, envelope dropped", "kind", env.Kind)
		}
	}
}

// translateEvent converts an agent.Event to a wire.Envelope.
// Returns nil for events that don't need to be forwarded to the browser.
// EventText and EventResult are handled directly in fanOut (fence-parser
// passthrough) and never reach here.
func translateEvent(ev agent.Event) *wire.Envelope {
	switch ev.Kind {
	case agent.EventThinking:
		// Extended-thinking delta (tether#34): a KindMessage with an object
		// payload (same shape family as tool_use / session_ready) so it flows
		// through here rather than fanOut's EventText fence-parser passthrough —
		// thinking is never fence-parsed and never accumulated into history.
		return &wire.Envelope{Kind: wire.KindMessage, Payload: map[string]any{
			"type": "thinking",
			"text": ev.Text,
		}}
	case agent.EventToolUse:
		if ev.ToolUse != nil {
			return &wire.Envelope{Kind: wire.KindMessage, Payload: map[string]any{
				"type":  "tool_use",
				"id":    ev.ToolUse.ID,
				"name":  ev.ToolUse.Name,
				"input": ev.ToolUse.Input,
			}}
		}
	case agent.EventToolResult:
		// tether#38: the output of a tool cc ran; the frontend matches it to its
		// tool_use by tool_use_id and hangs it under the corresponding tool row.
		if ev.ToolResult != nil {
			return &wire.Envelope{Kind: wire.KindMessage, Payload: map[string]any{
				"type":        "tool_result",
				"tool_use_id": ev.ToolResult.ToolUseID,
				"content":     ev.ToolResult.Content,
				"is_error":    ev.ToolResult.IsError,
			}}
		}
	case agent.EventUsage:
		// tether#48: the turn's token usage (from cc's result event), forwarded
		// as an object-payload KindMessage in the same family as thinking/
		// tool_use. Emitted just before KindResult, so the frontend attaches it
		// to the still-open turn bubble. Live-only — NOT accumulated into history
		// (see the fanOut switch above), so it's absent after a reload.
		if ev.Usage != nil {
			return &wire.Envelope{Kind: wire.KindMessage, Payload: map[string]any{
				"type":   "usage",
				"input":  ev.Usage.Input,
				"output": ev.Usage.Output,
			}}
		}
	case agent.EventError:
		// tether#63: classified so the browser can tell this — the agent
		// reporting an error about the turn it is mid-way through, on a
		// session that is still alive and usable — apart from a daemon-side
		// refusal that ends the connection. ErrCodeAgent is retryable for
		// exactly that reason: there is nothing here for a reconnect to fix,
		// but there is also nothing here that should make the browser stop
		// trying.
		env := wire.NewErrorEnvelope(wire.ErrCodeAgent, ev.Err.Error())
		return &env
	}
	return nil
}
