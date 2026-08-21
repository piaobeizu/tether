// internal/permission/manager.go
package permission

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"sort"
	"sync"
	"time"
)

// Timeout is the deadline for a pending permission request.
// Package-level var so tests can override it. Tests override this; not parallel-safe.
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
	// seq is the order Add saw this request in. It exists so Pending can hand
	// back a STABLE order: the map gives none, and the consumer (a chat client
	// being handed the outstanding requests it never saw — tether#132) renders
	// them as a queue, so a random order would shuffle the cards a reload
	// restores. Arrival order is the one the live path already produced.
	seq uint64
}

// Manager manages in-flight permission requests.
type Manager struct {
	mu      sync.Mutex
	pending map[string]*entry
	// seq numbers requests in the order they were added; see entry.seq. A
	// counter rather than a timestamp because two requests from one parallel
	// tool batch can share a millisecond, and "which of these two came first"
	// is exactly what would then be lost.
	seq uint64
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
	m.mu.Lock()
	m.seq++
	e.seq = m.seq
	m.pending[req.ID] = e
	// Armed AFTER the insert and inside the same critical section the callback
	// takes, so the timer cannot fire against a map that does not hold this
	// entry yet. Armed before the insert (which is what this was) the callback
	// found nothing, did nothing, and left an entry with no deadline at all —
	// unreachable at the production Timeout of a minute, and reachable by any
	// test that shortens it, which is how the expiry rule would end up pinned by
	// a test whose fixture is the one thing that cannot happen in production.
	e.timer = time.AfterFunc(Timeout, func() {
		m.mu.Lock()
		if _, ok := m.pending[req.ID]; ok {
			delete(m.pending, req.ID)
			ch <- Decision{Reason: "timeout"}
		}
		m.mu.Unlock()
	})
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

// Pending returns every request still awaiting a decision, oldest first.
//
// # Why this exists (tether#132)
//
// A permission request reaches the browser as ONE broadcast envelope and
// nothing else. That broadcast is a non-blocking send onto each chat client's
// channel (session.Registry.BroadcastAll), so a tab that has stalled loses it
// outright — and then the tool call sits waiting for a decision nobody can see
// a prompt for. The requests themselves were never lost; they are right here,
// in `pending`. What was missing was any way to ASK, so a reconnecting client
// could be handed what it never saw. This is that way.
//
// # Expiry needs no filter here, and that is a fact about Add, not a hope
//
// Every route out of `pending` is a DELETE: Add's time.AfterFunc removes the
// entry when Timeout elapses, Decide removes it when someone answers, and both
// the blocking Check and http.go's request handler call Decide when their
// caller goes away. So membership in this map IS "still awaiting a decision",
// and a request this returns is one a browser can still usefully answer. A
// second `expiresAt > now` test here would be a copy of the timer's job, able
// to disagree with it.
//
// The snapshot is of the POINTERS, taken under the lock and read outside it.
// A *Request is not mutated after Add sets its ID (Decide and the timer only
// remove the entry that holds it), so the caller cannot observe a torn one.
func (m *Manager) Pending() []*Request {
	m.mu.Lock()
	es := make([]*entry, 0, len(m.pending))
	for _, e := range m.pending {
		es = append(es, e)
	}
	m.mu.Unlock()
	sort.Slice(es, func(i, j int) bool { return es[i].seq < es[j].seq })
	out := make([]*Request, len(es))
	for i, e := range es {
		out[i] = e.req
	}
	return out
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
		m.Decide(req.ID, false, "context cancelled")
		return nil, ctx.Err()
	}
}
