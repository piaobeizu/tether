package skill

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
)

// RegisterAPI wires skill REST endpoints into mux (s7).
//
//	GET  /api/v1/skills                           → list all skills
//	POST /api/v1/skills                           → install skill {"name":"...","sourcePath":"..."}
//	DELETE /api/v1/skills/{id}                    → remove skill
//	POST /api/v1/skills/{id}/enable               → enable in workspace {"workspaceId":"..."}
//	POST /api/v1/skills/{id}/disable              → disable in workspace {"workspaceId":"..."}
//
// The last two name a WORKSPACE ID, not a directory. They took
// {"workspacePath":"..."} — an unvalidated host path — until tether#142; see
// Registry.Enable for why an id is the containment fix rather than a check on the
// path. The registry must be bound to a WorkspaceIndex (see
// Registry.BindWorkspaces, done in server/mux.go) or both answer 503.
func RegisterAPI(mux *http.ServeMux, reg *Registry) {
	mux.HandleFunc("/api/v1/skills", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			jsonResp(w, reg.List())
		case http.MethodPost:
			var body struct {
				Name       string `json:"name"`
				SourcePath string `json:"sourcePath"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.SourcePath == "" {
				http.Error(w, "bad request", http.StatusBadRequest)
				return
			}
			sk, err := reg.Install(body.Name, body.SourcePath)
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			w.WriteHeader(http.StatusCreated)
			jsonResp(w, sk)
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	})

	mux.HandleFunc("/api/v1/skills/", func(w http.ResponseWriter, r *http.Request) {
		tail := strings.TrimPrefix(r.URL.Path, "/api/v1/skills/")
		parts := strings.SplitN(tail, "/", 2)
		id := parts[0]
		action := ""
		if len(parts) == 2 {
			action = parts[1]
		}

		switch {
		case action == "" && r.Method == http.MethodDelete:
			if err := reg.Remove(id); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			w.WriteHeader(http.StatusNoContent)

		case action == "enable" && r.Method == http.MethodPost:
			overlayWrite(w, r, id, reg.Enable)

		case action == "disable" && r.Method == http.MethodPost:
			overlayWrite(w, r, id, reg.Disable)

		default:
			http.NotFound(w, r)
		}
	})
}

// overlayWrite serves both enable and disable.
//
// One function on purpose. The defect tether#142 fixed was an ASYMMETRY between
// two hand-copied handlers — same body shape, same status codes, but one
// validated its skill id and the other did not — and two copies is exactly how
// that drift happened. Sharing the decode, the argument order and the status
// mapping means the next change to either lands on both.
func overlayWrite(w http.ResponseWriter, r *http.Request, skillID string, apply func(skillID, workspaceID string) error) {
	// A bound on the body, as session_api.go, mcp_tokens.go, task_mcp.go and
	// auth/middleware.go all do. The payload is one short id; without this an
	// authenticated request streams unbounded input into a json.Decoder.
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	var body struct {
		WorkspaceID string `json:"workspaceId"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.WorkspaceID == "" {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if err := apply(skillID, body.WorkspaceID); err != nil {
		http.Error(w, err.Error(), overlayStatus(err))
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// overlayStatus maps the overlay's refusals onto codes a caller can act on.
//
// All three were 500 before tether#142, which misdescribes every one of them: an
// unregistered workspace id and an uninstalled skill id are the CALLER's mistake
// and no amount of retrying fixes them, while a daemon whose workspace registry
// failed to load is temporarily unable rather than broken — the same distinction
// workspace/state.go argues at length for the chat handshake. 500 on all three
// also sent an operator hunting a daemon bug for a request that was simply wrong.
func overlayStatus(err error) int {
	switch {
	case errors.Is(err, ErrNoWorkspaceIndex):
		return http.StatusServiceUnavailable
	case errors.Is(err, ErrUnknownSkill):
		return http.StatusNotFound
	case errors.Is(err, ErrUnknownWorkspace):
		return http.StatusBadRequest
	default:
		return http.StatusInternalServerError
	}
}

func jsonResp(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
