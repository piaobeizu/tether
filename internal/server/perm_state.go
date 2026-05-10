package server

import (
	"encoding/json"
	"sync"
	"time"
)

const permTimeout = 60 * time.Second

// PermRequest represents a pending PreToolUse hook request.
type PermRequest struct {
	ID       string          `json:"id"`
	ToolName string          `json:"tool_name"`
	Input    json.RawMessage `json:"tool_input"`
	decideCh chan bool
	timer    *time.Timer
}

// PermState manages in-flight permission requests (D-05b §3.3, §3.4).
type PermState struct {
	mu       sync.Mutex
	pending  map[string]*PermRequest
}

func NewPermState() *PermState {
	return &PermState{pending: make(map[string]*PermRequest)}
}

// Add registers a new pending request and returns a channel that receives
// the allow/deny decision. The channel fires once within permTimeout (60s);
// if no decision is made before the timeout the channel receives false.
func (ps *PermState) Add(req *PermRequest) <-chan bool {
	req.decideCh = make(chan bool, 1)
	req.timer = time.AfterFunc(permTimeout, func() {
		ps.mu.Lock()
		if _, ok := ps.pending[req.ID]; ok {
			delete(ps.pending, req.ID)
			req.decideCh <- false
		}
		ps.mu.Unlock()
	})

	ps.mu.Lock()
	ps.pending[req.ID] = req
	ps.mu.Unlock()

	return req.decideCh
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

// GetPending returns the pending request for broadcast (may be nil).
func (ps *PermState) GetPending(id string) *PermRequest {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	return ps.pending[id]
}
