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
	// (tether#54). Two attaches racing to spawn the SAME sid is a separate, much
	// narrower window — see the RESIDUAL note in Attach and tether#60.
	//
	// Not a universal invariant, and the difference matters: a provider that mints
	// its own id (OpenCodeProvider ignores SpawnConfig.SessionID) is registered
	// under an id nothing else will ever ask about until it announces its own, at
	// which point rekey moves it. Such an entry is addressable — evict and
	// BroadcastAll find it, which the old placeholder also allowed — but NOT by any
	// sid a client holds. See rekey, and the RESIDUAL note in Attach.
	sessions  map[string]*Entry
	locks     map[string]*SessionLock
	providers map[string]agent.AgentProvider
	// mintedIDIgnored records the providers already observed to report a session id
	// other than the one they were spawned under, so rekey's self-check warns ONCE
	// per provider instead of once per session. Guarded by mu.
	mintedIDIgnored map[string]bool
	PermEndpoint    string        // injected into cc subprocess env if non-empty
	History         *HistoryStore // nil = history disabled
	// Workdir is the agent subprocess cwd (workspace root); "" = daemon cwd.
	// Wired by internal/server/lifecycle.go Step 3b once the workspace root
	// is resolved (tether#51) — Step 1 builds the Registry before wsRoot is
	// known, so it can't be set at construction time.
	Workdir string
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
		locks:           make(map[string]*SessionLock),
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
// A dead entry is left UNREAPED (no Session().Close()), which leaks the zombie
// tether#56 is about — deliberately, because it leaks that zombie identically
// today and reaping is that slice's job, not a behaviour to smuggle in here.
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
	return r.spawnEntry(ctx, providerName, agent.SpawnConfig{})
}

// spawnEntry starts a new agent subprocess, wraps it in an Entry, registers the
// Entry under its sid and starts its fanOut loop. cfg is the caller's spawn
// intent; Env and Workdir are filled in here (they are registry-wide, not
// per-call), so callers only choose between "pin this freshly minted id" and
// "resume this existing one".
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
func (r *Registry) spawnEntry(ctx context.Context, providerName string, cfg agent.SpawnConfig) (*Entry, error) {
	if providerName == "" {
		providerName = "claude-code"
	}
	provider, ok := r.providers[providerName]
	if !ok {
		return nil, fmt.Errorf("unknown provider: %s", providerName)
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
	cfg.Workdir = r.Workdir
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

	sess, err := provider.Spawn(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("spawn: %w", err)
	}

	e := &Entry{
		sess:        sess,
		subs:        make(map[chan wire.Envelope]struct{}),
		fenceParser: NewFenceParser(),
		regKey:      key,
		provider:    providerName,
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

	// background goroutine: fan out events to subscribers
	go r.fanOut(e)

	return e, nil
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

// evict removes e from the sessions map after its session has terminated —
// called from fanOut's defer, i.e. once the agent's Events() channel has
// closed. serveChat binds the agent subprocess to the client-connection
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
// order-independent for its three callers (fanOut's defer, liveEntry dropping a
// corpse, Attachment.resolve dropping a failed resume), and because it cannot
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
	// cancelled the Spawn context that bounds the subprocess). Evict the entry
	// so long-running daemons don't leak dead sessions in r.sessions (tether#12).
	defer r.evict(e)

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
		return &wire.Envelope{Kind: wire.KindError, Payload: ev.Err.Error()}
	}
	return nil
}
