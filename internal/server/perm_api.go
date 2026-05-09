package server

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/piaobeizu/tether/internal/session"
	"github.com/piaobeizu/tether/internal/wire"
)

// registerPermAPI wires the permission HTTP API routes into mux.
// Routes per D-05b §3.1–3.2:
//
//	POST /api/v1/agent/permission/request   — hook → daemon (blocks until decided)
//	POST /api/v1/agent/permission/{id}/decide — UI → daemon
func registerPermAPI(mux *http.ServeMux, ps *PermState, reg *session.Registry) {
	mux.HandleFunc("/api/v1/agent/permission/request", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var body struct {
			SessionID string          `json:"session_id"`
			ToolName  string          `json:"tool_name"`
			Input     json.RawMessage `json:"tool_input"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}

		req := &PermRequest{
			ID:       newID(),
			ToolName: body.ToolName,
			Input:    body.Input,
		}
		decideCh := ps.Add(req)

		// Broadcast to browser subscribers so the UI can render the Allow/Deny prompt.
		reg.BroadcastAll(wire.Envelope{
			Kind:      wire.KindPermission,
			SessionID: wire.SessionID(body.SessionID),
			Payload: map[string]any{
				"id":       req.ID,
				"toolName": req.ToolName,
				"input":    req.Input,
			},
		})

		// Block until decided or timeout (hook has 65s client timeout; we use 60s).
		allow := <-decideCh

		w.Header().Set("Content-Type", "application/json")
		if allow {
			_ = json.NewEncoder(w).Encode(map[string]any{"allow": true})
		} else {
			_ = json.NewEncoder(w).Encode(map[string]any{"allow": false, "message": "denied or timeout"})
		}
	})

	mux.HandleFunc("/api/v1/agent/permission/", func(w http.ResponseWriter, r *http.Request) {
		// Route: /api/v1/agent/permission/{id}/decide
		parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/api/v1/agent/permission/"), "/")
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
			Allow bool `json:"allow"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		if !ps.Decide(id, body.Allow) {
			http.Error(w, "unknown request id", http.StatusNotFound)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
}

func newID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
