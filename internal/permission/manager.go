// internal/permission/manager.go
package permission

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"sync"
	"time"
)

// Timeout is the deadline for a pending permission request.
// Package-level var so tests can override it.
var Timeout = 60 * time.Second

// Request is the generalized permission check request.
type Request struct {
	ID        string          // populated by Manager.Add
	Source    string          `json:"source"`              // "claude_hook" | "mcp:<servername>"
	SessionID string          `json:"session_id,omitempty"`
	ToolName  string          `json:"tool_name"`
	Args      json.RawMessage `json:"tool_input"`
	TaskID    string          `json:"task_id,omitempty"`
}

// Decision is the outcome of a permission check.
type Decision struct {
	Allow  bool
	Reason string
}

type entry struct {
	req      *Request
	decideCh chan Decision
	timer    *time.Timer
}

// Manager manages in-flight permission requests.
type Manager struct {
	mu      sync.Mutex
	pending map[string]*entry
}

// New returns a ready Manager.
func New() *Manager {
	return &Manager{pending: make(map[string]*entry)}
}

// Add registers req, sets req.ID, and returns a channel that yields the decision.
func (m *Manager) Add(req *Request) <-chan Decision {
	req.ID = newID()
	ch := make(chan Decision, 1)
	e := &entry{req: req, decideCh: ch}
	e.timer = time.AfterFunc(Timeout, func() {
		m.mu.Lock()
		if _, ok := m.pending[req.ID]; ok {
			delete(m.pending, req.ID)
			ch <- Decision{Reason: "timeout"}
		}
		m.mu.Unlock()
	})
	m.mu.Lock()
	m.pending[req.ID] = e
	m.mu.Unlock()
	return ch
}

// Decide resolves a pending request. Returns false if id is unknown.
func (m *Manager) Decide(id string, allow bool, reason string) bool {
	m.mu.Lock()
	e, ok := m.pending[id]
	if ok {
		delete(m.pending, id)
	}
	m.mu.Unlock()
	if !ok {
		return false
	}
	e.timer.Stop()
	e.decideCh <- Decision{Allow: allow, Reason: reason}
	return true
}

// GetPending returns the pending request, or nil if id is unknown / already decided.
func (m *Manager) GetPending(id string) *Request {
	m.mu.Lock()
	defer m.mu.Unlock()
	if e, ok := m.pending[id]; ok {
		return e.req
	}
	return nil
}

func newID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// Check implements a blocking permission check for use by the gateway.
// It adds the request and blocks until decided or ctx cancelled.
func (m *Manager) Check(ctx context.Context, req *Request) (*Decision, error) {
	ch := m.Add(req)
	select {
	case d := <-ch:
		return &d, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}
