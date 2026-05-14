// Package session manages live cc sessions and multi-attach broadcast (D-08, D-15).
package session

import (
	"context"
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

// GetLock returns (or lazily creates) the SessionLock for the given ccSID.
func (r *Registry) GetLock(ccSID string) *SessionLock {
	r.mu.Lock()
	defer r.mu.Unlock()
	if l, ok := r.locks[ccSID]; ok {
		return l
	}
	l := &SessionLock{}
	r.locks[ccSID] = l
	return l
}

// GetOrSpawnEntry returns the *Entry for the given ccSID, or spawns a new
// agent process and registers a fresh Entry. If ccSID is empty, a new
// process is spawned and re-keyed once its system/init provides the real
// SessionID. providerName selects the AgentProvider; defaults to
// "claude-code" if empty.
//
// Returning *Entry (rather than just agent.Session) lets the caller call
// Entry.Subscribe BEFORE sending the first prompt — necessary because the
// session ID is only published AFTER cc consumes a prompt, and any text
// events produced in between would otherwise be fanned out to zero
// subscribers.
func (r *Registry) GetOrSpawnEntry(ctx context.Context, ccSID, providerName string) (*Entry, error) {
	if ccSID != "" {
		r.mu.RLock()
		e, ok := r.sessions[ccSID]
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
	sess, err := provider.Spawn(ctx, agent.SpawnConfig{ResumeSessionID: ccSID, Env: extraEnv})
	if err != nil {
		return nil, fmt.Errorf("spawn: %w", err)
	}

	e := &Entry{sess: sess, subs: make(map[chan wire.Envelope]struct{})}

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
		if sid != "" {
			r.sessions[sid] = e
		}
		r.mu.Unlock()
	}()

	return e, nil
}

// GetOrSpawn is a thin wrapper that hides the Entry behind the Session;
// retained for /wt/events and any callers that don't need pre-init
// subscription.
func (r *Registry) GetOrSpawn(ctx context.Context, ccSID, providerName string) (agent.Session, error) {
	e, err := r.GetOrSpawnEntry(ctx, ccSID, providerName)
	if err != nil {
		return nil, err
	}
	return e.sess, nil
}

// RecordUserMessage persists a user-sent message to session history.
func (r *Registry) RecordUserMessage(ccSID, text string) {
	if r.History != nil && ccSID != "" {
		r.History.RecordUser(ccSID, text)
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
func (r *Registry) Subscribe(ccSID string, ch chan wire.Envelope) {
	r.mu.RLock()
	e, ok := r.sessions[ccSID]
	r.mu.RUnlock()
	if !ok {
		return
	}
	e.subsMu.Lock()
	e.subs[ch] = struct{}{}
	e.subsMu.Unlock()
}

// Unsubscribe removes the channel from broadcast.
func (r *Registry) Unsubscribe(ccSID string, ch chan wire.Envelope) {
	r.mu.RLock()
	e, ok := r.sessions[ccSID]
	r.mu.RUnlock()
	if !ok {
		return
	}
	e.subsMu.Lock()
	delete(e.subs, ch)
	e.subsMu.Unlock()
}

// SetOwner records ownerClientID using compare-and-set. Returns true if this call set the owner.
func (r *Registry) SetOwner(ccSID, clientID string) bool {
	r.mu.RLock()
	e, ok := r.sessions[ccSID]
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
func (r *Registry) IsLive(ccSID string) bool {
	r.mu.RLock()
	_, ok := r.sessions[ccSID]
	r.mu.RUnlock()
	return ok
}

// IsOwner returns true if clientID is the recorded owner of ccSID.
func (r *Registry) IsOwner(ccSID, clientID string) bool {
	r.mu.RLock()
	e, ok := r.sessions[ccSID]
	r.mu.RUnlock()
	if !ok {
		return false
	}
	e.subsMu.RLock()
	defer e.subsMu.RUnlock()
	return e.ownerClientID == clientID
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
func (r *Registry) fanOut(e *Entry) {
	var sid string

	for ev := range e.sess.Events() {
		if ev.Kind == agent.EventInit && ev.SessionID != "" {
			sid = ev.SessionID
		}
		slog.Debug("fanOut: agent event", "kind", ev.Kind, "text_preview", truncStr(ev.Text, 60))

		// Persist to history.
		if r.History != nil && sid != "" {
			switch ev.Kind {
			case agent.EventText:
				r.History.AccumulateAssistant(sid, ev.Text)
			case agent.EventResult:
				r.History.FinalizeAssistant(sid)
			}
		}

		env := translateEvent(ev)
		if env == nil {
			continue
		}
		e.subsMu.RLock()
		nsub := len(e.subs)
		e.subsMu.RUnlock()
		slog.Debug("fanOut: broadcasting", "wire_kind", env.Kind, "nsub", nsub)
		e.subsMu.RLock()
		for ch := range e.subs {
			select {
			case ch <- *env:
			default:
				slog.Warn("fanOut: slow subscriber, envelope dropped", "kind", env.Kind)
			}
		}
		e.subsMu.RUnlock()
	}
}

// translateEvent converts an agent.Event to a wire.Envelope.
// Returns nil for events that don't need to be forwarded to the browser.
func translateEvent(ev agent.Event) *wire.Envelope {
	switch ev.Kind {
	case agent.EventText:
		return &wire.Envelope{Kind: wire.KindMessage, Payload: ev.Text}
	case agent.EventToolUse:
		if ev.ToolUse != nil {
			return &wire.Envelope{Kind: wire.KindMessage, Payload: map[string]any{
				"type":  "tool_use",
				"id":    ev.ToolUse.ID,
				"name":  ev.ToolUse.Name,
				"input": ev.ToolUse.Input,
			}}
		}
	case agent.EventResult:
		return &wire.Envelope{Kind: wire.KindResult, Payload: ev.Text}
	case agent.EventError:
		return &wire.Envelope{Kind: wire.KindError, Payload: ev.Err.Error()}
	}
	return nil
}
