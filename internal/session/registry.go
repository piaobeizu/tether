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
	mu           sync.RWMutex
	sessions     map[string]*Entry // keyed by sid (real or pending placeholder)
	locks        map[string]*SessionLock
	providers    map[string]agent.AgentProvider
	PermEndpoint string        // injected into cc subprocess env if non-empty
	History      *HistoryStore // nil = history disabled
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
	// evicted is set true by Registry.evict once this session's fanOut loop
	// has returned (session ended / client disconnected). Guarded by
	// Registry.mu (NOT subsMu). It exists so the re-key goroutine in
	// GetOrSpawnEntry, if it wins the race and runs AFTER eviction, refuses to
	// re-insert this dead entry under its real sid (tether#12).
	evicted bool
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
		sessions:  make(map[string]*Entry),
		locks:     make(map[string]*SessionLock),
		providers: pm,
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

// GetOrSpawnEntry returns the *Entry for the given sid, or spawns a new
// agent process and registers a fresh Entry. If sid is empty, a new
// process is spawned and re-keyed once its system/init provides the real
// SessionID. providerName selects the AgentProvider; defaults to
// "claude-code" if empty.
//
// Returning *Entry (rather than just agent.Session) lets the caller call
// Entry.Subscribe BEFORE sending the first prompt — necessary because the
// session ID is only published AFTER cc consumes a prompt, and any text
// events produced in between would otherwise be fanned out to zero
// subscribers.
func (r *Registry) GetOrSpawnEntry(ctx context.Context, sid, providerName string) (*Entry, error) {
	if sid != "" {
		r.mu.RLock()
		e, ok := r.sessions[sid]
		r.mu.RUnlock()
		if ok {
			return e, nil
		}
	}

	if providerName == "" {
		providerName = "claude-code"
	}
	provider, ok := r.providers[providerName]
	if !ok {
		return nil, fmt.Errorf("unknown provider: %s", providerName)
	}

	var extraEnv []string
	if r.PermEndpoint != "" {
		extraEnv = append(extraEnv, "TETHER_DAEMON_PERM_ENDPOINT="+r.PermEndpoint)
	}
	sess, err := provider.Spawn(ctx, agent.SpawnConfig{ResumeSessionID: sid, Env: extraEnv})
	if err != nil {
		return nil, fmt.Errorf("spawn: %w", err)
	}

	e := &Entry{sess: sess, subs: make(map[chan wire.Envelope]struct{}), fenceParser: NewFenceParser()}

	// Register entry under a synthetic placeholder key BEFORE waiting for
	// SessionID. This breaks the deadlock: cc's stream-json `--input-format`
	// mode does NOT emit system/init until the first user prompt arrives,
	// but serveChat needs the entry registered to call Subscribe and start
	// reading user prompts. Solution: register under a temp key now, re-key
	// once cc emits system/init asynchronously.
	tempKey := fmt.Sprintf("pending-%p", e)
	r.mu.Lock()
	r.sessions[tempKey] = e
	r.mu.Unlock()

	// background goroutine: fan out events to subscribers
	go r.fanOut(e)

	// Re-key once cc emits system/init asynchronously.
	go func() {
		sid := sess.SessionID()
		r.mu.Lock()
		delete(r.sessions, tempKey)
		// Skip the re-key if the session already ended and fanOut evicted the
		// entry while we were parked in SessionID() (tether#12). Without this
		// guard a fast session teardown could be resurrected here and leak.
		if sid != "" && !e.evicted {
			r.sessions[sid] = e
		}
		r.mu.Unlock()
	}()

	return e, nil
}

// GetOrSpawn is a thin wrapper that hides the Entry behind the Session;
// retained for /wt/events and any callers that don't need pre-init
// subscription.
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
// Eviction is by value: it deletes whatever key maps to e, catching the entry
// whether it is keyed under its real sid or still under the pending-%p
// placeholder from GetOrSpawnEntry. The placeholder case matters — a session
// that dies before emitting system/init parks the re-key goroutine forever in
// ccSession.SessionID() (sidReady never closes), so this by-value scan is the
// only path that reclaims that map slot. Setting e.evicted first makes a
// late-waking re-key goroutine refuse to re-insert the entry (see
// GetOrSpawnEntry). Deleting during range is safe per the Go spec, and this
// runs once per session lifetime — not on a hot path.
//
// r.locks is deliberately NOT cleaned here: the per-sid SessionLock is shared
// with shell sessions (handleWTShell calls GetLock for the same sid), so its
// lifetime is not bound to this chat Entry — reclaiming it safely needs a
// cross-surface refcount, tracked as a separate follow-up.
func (r *Registry) evict(e *Entry) {
	r.mu.Lock()
	defer r.mu.Unlock()
	e.evicted = true
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

// SetOwner records ownerClientID using compare-and-set. Returns true if this call set the owner.
func (r *Registry) SetOwner(sid, clientID string) bool {
	r.mu.RLock()
	e, ok := r.sessions[sid]
	r.mu.RUnlock()
	if !ok {
		return false
	}
	e.subsMu.Lock()
	defer e.subsMu.Unlock()
	if e.ownerClientID != "" && e.ownerClientID != clientID {
		return false
	}
	e.ownerClientID = clientID
	return true
}

// IsLive returns true if the session exists and is actively tracked.
func (r *Registry) IsLive(sid string) bool {
	r.mu.RLock()
	_, ok := r.sessions[sid]
	r.mu.RUnlock()
	return ok
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
			e.fenceParser.ResetTurn()
		}
		slog.Debug("fanOut: agent event", "kind", ev.Kind, "text_preview", truncStr(ev.Text, 60))

		switch ev.Kind {
		case agent.EventText:
			emitSegments(e.fenceParser.Feed(ev.Text))
			continue

		case agent.EventResult:
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
