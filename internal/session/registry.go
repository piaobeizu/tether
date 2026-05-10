// Package session manages live cc sessions and multi-attach broadcast (D-08, D-15).
package session

import (
	"context"
	"fmt"
	"log/slog"
	"sync"

	"github.com/piaobeizu/tether/internal/agent"
	"github.com/piaobeizu/tether/internal/wire"
)

// Registry holds all live sessions and the set of event subscribers per session.
type Registry struct {
	mu           sync.RWMutex
	sessions     map[string]*entry // keyed by cc SessionID
	locks        map[string]*SessionLock
	provider     agent.AgentProvider
	PermEndpoint string // injected into cc subprocess env if non-empty
}

type entry struct {
	sess   agent.Session
	subs   map[chan wire.Envelope]struct{}
	subsMu sync.RWMutex
}

func NewRegistry(provider agent.AgentProvider) *Registry {
	return &Registry{
		sessions: make(map[string]*entry),
		locks:    make(map[string]*SessionLock),
		provider: provider,
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

// GetOrSpawn returns the existing session for the given ccSID, or spawns a new
// ClaudeCode process and registers it. If ccSID is empty, a new process is spawned
// and registered once its system/init provides the real SessionID.
func (r *Registry) GetOrSpawn(ctx context.Context, ccSID string) (agent.Session, error) {
	if ccSID != "" {
		r.mu.RLock()
		e, ok := r.sessions[ccSID]
		r.mu.RUnlock()
		if ok {
			return e.sess, nil
		}
	}

	var extraEnv []string
	if r.PermEndpoint != "" {
		extraEnv = append(extraEnv, "TETHER_DAEMON_PERM_ENDPOINT="+r.PermEndpoint)
	}
	sess, err := r.provider.Spawn(ctx, agent.SpawnConfig{ResumeSessionID: ccSID, Env: extraEnv})
	if err != nil {
		return nil, fmt.Errorf("spawn: %w", err)
	}

	e := &entry{sess: sess, subs: make(map[chan wire.Envelope]struct{})}

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
		r.sessions[sid] = e
		r.mu.Unlock()
	}()

	return sess, nil
}

// BroadcastAll sends env to every subscriber across all sessions.
func (r *Registry) BroadcastAll(env wire.Envelope) {
	r.mu.RLock()
	entries := make([]*entry, 0, len(r.sessions))
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

func truncStr(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// fanOut translates agent.Events into wire.Envelopes and broadcasts to all subscribers.
func (r *Registry) fanOut(e *entry) {
	for ev := range e.sess.Events() {
		slog.Info("fanOut: agent event", "kind", ev.Kind, "text_preview", truncStr(ev.Text, 60))
		env := translateEvent(ev)
		if env == nil {
			continue
		}
		e.subsMu.RLock()
		nsub := len(e.subs)
		e.subsMu.RUnlock()
		slog.Info("fanOut: broadcasting", "wire_kind", env.Kind, "nsub", nsub)
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
	case agent.EventError:
		return &wire.Envelope{Kind: wire.KindError, Payload: ev.Err.Error()}
	}
	return nil
}
