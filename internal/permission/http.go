// internal/permission/http.go
package permission

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
)

// BroadcastFn is called when a new permission request is registered,
// allowing the server layer to push it to connected browser clients.
// May be nil (no broadcast).
type BroadcastFn func(req *Request)

// RegisterAPI mounts permission endpoints on mux.
//
// Canonical paths:
//
//	POST /api/v1/permission/request
//	POST /api/v1/permission/{id}/decide
//
// Alias paths (kept for v0.3.x compat):
//
//	POST /api/v1/agent/permission/request
//	POST /api/v1/agent/permission/{id}/decide
func RegisterAPI(mux *http.ServeMux, m *Manager, broadcast BroadcastFn) {
	requestHandler := func(defaultSource string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodPost {
				http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
				return
			}
			var body struct {
				Source    string          `json:"source"`
				SessionID string          `json:"session_id"`
				ToolName  string          `json:"tool_name"`
				Input     json.RawMessage `json:"tool_input"`
				TaskID    string          `json:"task_id,omitempty"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				http.Error(w, "bad request", http.StatusBadRequest)
				return
			}
			src := body.Source
			if src == "" {
				src = defaultSource
			}
			req := &Request{
				Source:    src,
				SessionID: body.SessionID,
				ToolName:  body.ToolName,
				Args:      body.Input,
				TaskID:    body.TaskID,
			}
			decideCh := m.Add(req)
			slog.Info("permission request", "id", req.ID, "tool", req.ToolName, "source", req.Source)
			if broadcast != nil {
				broadcast(req)
			}
			dec := <-decideCh
			slog.Info("permission decided", "id", req.ID, "allow", dec.Allow)
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"allow":   dec.Allow,
				"message": dec.Reason,
			})
		}
	}

	decideHandler := func(pathPrefix string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			parts := strings.Split(strings.TrimPrefix(r.URL.Path, pathPrefix), "/")
			if len(parts) != 2 || parts[1] != "decide" {
				http.NotFound(w, r)
				return
			}
			id := parts[0]
			if r.Method != http.MethodPost {
				http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
				return
			}
			var body struct {
				Allow  bool   `json:"allow"`
				Reason string `json:"message,omitempty"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				http.Error(w, "bad request", http.StatusBadRequest)
				return
			}
			if !m.Decide(id, body.Allow, body.Reason) {
				http.Error(w, "unknown request id", http.StatusNotFound)
				return
			}
			w.WriteHeader(http.StatusNoContent)
		}
	}

	// Canonical
	mux.HandleFunc("/api/v1/permission/request", requestHandler("unknown"))
	mux.HandleFunc("/api/v1/permission/", decideHandler("/api/v1/permission/"))

	// Alias (v0.3.x compat)
	mux.HandleFunc("/api/v1/agent/permission/request", requestHandler("claude_hook"))
	mux.HandleFunc("/api/v1/agent/permission/", decideHandler("/api/v1/agent/permission/"))
}
