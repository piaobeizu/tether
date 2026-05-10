package permission

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"

	"github.com/piaobeizu/tether/internal/wire"
)

// Broadcaster is satisfied by *session.Registry. Defined here to avoid
// a direct import of internal/session (which would create a cycle when
// internal/mcp/gateway imports internal/permission).
type Broadcaster interface {
	BroadcastAll(env wire.Envelope)
}

// RegisterPermAPI wires permission HTTP API routes into mux.
//
// Canonical routes (v0.3+):
//
//	POST /api/v1/permission/request       — hook/MCP → daemon
//	POST /api/v1/permission/{id}/decide   — UI → daemon
//
// Alias routes (kept for v0.3.x backward compat; deprecated, remove in v0.4):
//
//	POST /api/v1/agent/permission/request
//	POST /api/v1/agent/permission/{id}/decide
func RegisterPermAPI(mux *http.ServeMux, ps *PermState, reg Broadcaster) {
	handleRequest := func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var body struct {
			SessionID  string          `json:"session_id"`
			ToolName   string          `json:"tool_name"`
			Input      json.RawMessage `json:"tool_input"`
			Source     string          `json:"source"`
			SourceMeta json.RawMessage `json:"source_meta,omitempty"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		if body.ToolName == "" {
			http.Error(w, "tool_name required", http.StatusBadRequest)
			return
		}
		if body.Source == "" {
			body.Source = "claude_hook"
		}

		req := &PermRequest{
			ID:       NewID(),
			ToolName: body.ToolName,
			Input:    body.Input,
			Source:   body.Source,
		}
		decideCh, err := ps.Add(req)
		if err != nil {
			slog.Error("permission: duplicate ID", "id", req.ID, "err", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		slog.Info("permission request", "id", req.ID, "tool", body.ToolName,
			"sid", body.SessionID, "source", body.Source)

		reg.BroadcastAll(wire.Envelope{
			Kind:      wire.KindPermission,
			SessionID: wire.SessionID(body.SessionID),
			Payload: map[string]any{
				"id":          req.ID,
				"toolName":    req.ToolName,
				"input":       req.Input,
				"source":      body.Source,
				"source_meta": body.SourceMeta,
			},
		})

		var allow bool
		var reason string
		select {
		case allow = <-decideCh:
			if allow {
				reason = "allowed"
			} else {
				reason = "denied or timeout"
			}
		case <-r.Context().Done():
			ps.Decide(req.ID, false)
			slog.Info("permission request cancelled by client", "id", req.ID)
			return
		}
		slog.Info("permission decided", "id", req.ID, "allow", allow, "reason", reason)

		w.Header().Set("Content-Type", "application/json")
		if allow {
			_ = json.NewEncoder(w).Encode(map[string]any{"allow": true})
		} else {
			_ = json.NewEncoder(w).Encode(map[string]any{"allow": false, "message": reason})
		}
	}

	handleDecide := func(w http.ResponseWriter, r *http.Request, prefix string) {
		parts := strings.Split(strings.TrimPrefix(r.URL.Path, prefix), "/")
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
	}

	// Canonical paths (v0.3+)
	mux.HandleFunc("/api/v1/permission/request", handleRequest)
	mux.HandleFunc("/api/v1/permission/", func(w http.ResponseWriter, r *http.Request) {
		handleDecide(w, r, "/api/v1/permission/")
	})

	// Alias paths (deprecated; remove in v0.4)
	mux.HandleFunc("/api/v1/agent/permission/request", handleRequest)
	mux.HandleFunc("/api/v1/agent/permission/", func(w http.ResponseWriter, r *http.Request) {
		handleDecide(w, r, "/api/v1/agent/permission/")
	})
}
