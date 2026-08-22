package skill

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
)

// RegisterAPI wires skill REST endpoints into mux (s7).
//
//	GET  /api/v1/skills                           → list all skills
//	POST /api/v1/skills                           → install skill {"name":"...","sourcePath":"..."};
//	                                                sourcePath must already be a directory, else 400 (tether#147)
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
			// The same bound overlayWrite takes, for the same reason. tether#142
			// capped the two overlay endpoints and left this one — the more
			// attractive of the two, since it is a POST whose string the daemon then
			// stores — reading an unbounded body into a json.Decoder (tether#156).
			r.Body = http.MaxBytesReader(w, r.Body, maxSkillBodyBytes)
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
				// Through the same refusal map as everything else on this route file.
				// tether#156 made the overlay endpoints derive their body from the
				// sentinel and left this handler sending err.Error() as a 500 — which is
				// how `skill path not found: stat /some/path: no such file` reached a
				// client (tether#147).
				refuse(w, r, err)
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
			// Through the same refusal map as enable and disable, which is what
			// makes an uninstalled id a 404 here too rather than the 204 it used to
			// be (tether#156 fact 5).
			if err := reg.Remove(id); err != nil {
				refuse(w, r, err)
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
	r.Body = http.MaxBytesReader(w, r.Body, maxSkillBodyBytes)
	var body struct {
		WorkspaceID string `json:"workspaceId"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.WorkspaceID == "" {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if err := apply(skillID, body.WorkspaceID); err != nil {
		refuse(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// maxSkillBodyBytes bounds every JSON body this file decodes. The payloads are a
// short id or two short strings; 4 KiB is the same bound auth/middleware.go and
// server/mcp_tokens.go use.
const maxSkillBodyBytes = 4096

// refuse is the single exit for ANY failed call on this route file — the two
// overlay writes, the delete, and (since tether#147) the install: it picks the
// status AND the body from overlayRefusal, and logs the error rather than sending
// it.
//
// The rule that the body comes from the SENTINEL and never from the error value
// is the fix, not the phrasing of any one message (tether#156). err.Error() is
// assembled from whatever the failure carried — a daemon-side path from an
// os.PathError, the registry's stored value in the non-absolute refusal — and no
// caller can act on any of it. Deriving the body from the identity of the
// refusal instead means the next error to carry a path cannot leak it either.
func refuse(w http.ResponseWriter, r *http.Request, err error) {
	code, body := overlayRefusal(err)
	switch code {
	case http.StatusInternalServerError:
		slog.Error("skill overlay request failed", "method", r.Method, "path", r.URL.Path, "error", err)
	case http.StatusConflict:
		// The two classes whose detail is about the filesystem rather than about
		// the request, and therefore belongs to the operator, who has that
		// filesystem in front of them. Logging the 409s as well is not decoration:
		// their cause was the whole of the old 500 body, and dropping it from the
		// response without putting it anywhere would trade a leak for a blind spot.
		slog.Warn("skill overlay refused", "method", r.Method, "path", r.URL.Path, "error", err)
	}
	http.Error(w, body, code)
}

// overlayInternalErrorBody is what a 500 says, in place of err.Error().
//
// A 500 means this daemon's own state is wrong, so the message describes the
// daemon rather than the request — and there is nothing in it a caller can act
// on. Sending err.Error() instead put daemon-side paths (`mkdir /some/path: ...`)
// into an HTTP body for no benefit; the detail is logged where an operator, who
// has the filesystem in front of them, can use it (tether#156).
const overlayInternalErrorBody = "the daemon could not complete this request"

// overlayRefusal maps the overlay's refusals onto a code AND a body a caller can
// act on.
//
// The first three were 500 before tether#142, which misdescribes every one of
// them: an unregistered workspace id and an uninstalled skill id are the CALLER's
// mistake and no amount of retrying fixes them, while a daemon whose workspace
// registry failed to load is temporarily unable rather than broken — the same
// distinction workspace/state.go argues at length for the chat handshake. 500 on
// all three also sent an operator hunting a daemon bug for a request that was
// simply wrong.
//
// The three 409s are tether#156's, and they are one situation with three causes:
// the request is well formed and names things this daemon has, but the place the
// overlay would go is not in a state it can be served from — a path that leaves
// the workspace, a name already taken by something this daemon did not create, a
// registered directory that is not on disk. Each is visible to the caller and
// fixable by it, and none is a daemon fault, which is why none is a 500.
//
// ErrSkillSourceUnusable is tether#147's, and it is not an overlay refusal at all
// — it belongs to install. It sits here because the alternative was a second
// mapping for one case, which is how the tether#142 asymmetry this file exists to
// undo got started. 400 rather than the 409 its workspace-side cousin
// ErrWorkspaceDirUnusable gets, because that one is about a path the DAEMON
// stored while this one is about a path the caller just sent.
func overlayRefusal(err error) (int, string) {
	switch {
	case errors.Is(err, ErrNoWorkspaceIndex):
		return http.StatusServiceUnavailable, ErrNoWorkspaceIndex.Error()
	case errors.Is(err, ErrUnknownSkill):
		return http.StatusNotFound, ErrUnknownSkill.Error()
	case errors.Is(err, ErrUnknownWorkspace):
		return http.StatusBadRequest, ErrUnknownWorkspace.Error()
	case errors.Is(err, ErrSkillSourceUnusable):
		return http.StatusBadRequest, ErrSkillSourceUnusable.Error()
	case errors.Is(err, ErrOverlayEscapesWorkspace):
		return http.StatusConflict, ErrOverlayEscapesWorkspace.Error()
	case errors.Is(err, ErrOverlayLocationOccupied):
		return http.StatusConflict, ErrOverlayLocationOccupied.Error()
	case errors.Is(err, ErrWorkspaceDirUnusable):
		return http.StatusConflict, ErrWorkspaceDirUnusable.Error()
	default:
		return http.StatusInternalServerError, overlayInternalErrorBody
	}
}

func jsonResp(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
