package permission

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sync"
	"time"
)

const PermTimeout = 60 * time.Second

// PermRequest represents a pending PreToolUse (or MCP tool-call) permission request.
type PermRequest struct {
	ID       string          `json:"id"`
	ToolName string          `json:"tool_name"`
	Input    json.RawMessage `json:"tool_input"`
	// Source identifies the caller: "claude_hook" for cc PreToolUse, "mcp:<server>" for MCP gateway.
	Source   string          `json:"source,omitempty"`
	decideCh chan bool
	timer    *time.Timer
}

// PermState manages in-flight permission requests.
type PermState struct {
	mu      sync.Mutex
	pending map[string]*PermRequest
}

func NewPermState() *PermState {
	return &PermState{pending: make(map[string]*PermRequest)}
}

// Add registers a new pending request and returns a channel that receives
// the allow/deny decision. Returns error if ID is already in-flight.
func (ps *PermState) Add(req *PermRequest) (<-chan bool, error) {
	ps.mu.Lock()
	if _, dup := ps.pending[req.ID]; dup {
		ps.mu.Unlock()
		return nil, fmt.Errorf("permission: duplicate request ID %q", req.ID)
	}
	req.decideCh = make(chan bool, 1)
	req.timer = time.AfterFunc(PermTimeout, func() {
		ps.mu.Lock()
		if _, ok := ps.pending[req.ID]; ok {
			delete(ps.pending, req.ID)
			req.decideCh <- false
		}
		ps.mu.Unlock()
	})
	ps.pending[req.ID] = req
	ps.mu.Unlock()
	return req.decideCh, nil
}

// Decide resolves a pending request. Returns false if the request ID is unknown.
func (ps *PermState) Decide(id string, allow bool) bool {
	ps.mu.Lock()
	req, ok := ps.pending[id]
	if ok {
		delete(ps.pending, id)
	}
	ps.mu.Unlock()
	if !ok {
		return false
	}
	req.timer.Stop()
	req.decideCh <- allow
	return true
}

// GetPending returns the pending request (may be nil).
func (ps *PermState) GetPending(id string) *PermRequest {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	return ps.pending[id]
}

// NewID returns a random 8-byte hex ID. Panics on crypto/rand failure (unrecoverable).
func NewID() string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		panic("permission: crypto/rand unavailable: " + err.Error())
	}
	return hex.EncodeToString(b)
}
