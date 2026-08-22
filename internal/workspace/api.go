package workspace

import (
	"encoding/json"
	"errors"
	"io/fs"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"github.com/piaobeizu/tether/internal/mcp/builtin"
)

// RegisterAPI wires workspace REST endpoints into mux (s7).
//
//	GET    /api/v1/workspaces               → list all workspaces
//	POST   /api/v1/workspaces               → add workspace {"name":"...","path":"..."};
//	                                          path must be absolute and must already
//	                                          be a directory, else 400 (tether#147)
//	DELETE /api/v1/workspaces/{id}          → remove workspace by ID
//	GET    /api/v1/workspaces/{id}/files    → list files directly under {dir} (default: root)
//	GET    /api/v1/workspaces/{id}/file     → read one file's content ({"path":..,"content":..,"truncated":..})
//	GET    /api/v1/workspaces/{id}/tree     → flat recursive file list for @-mention ({"files":[..],"truncated":..})
func RegisterAPI(mux *http.ServeMux, reg *Registry) {
	mux.HandleFunc("/api/v1/workspaces", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			jsonResp(w, reg.List())
		case http.MethodPost:
			// A bound on the body, as auth/middleware.go, server/mcp_tokens.go and
			// the skill endpoints all have. The payload is two short strings;
			// without this an authenticated request streams unbounded input into a
			// json.Decoder (tether#156). 4 KiB is the bound the others use.
			r.Body = http.MaxBytesReader(w, r.Body, maxWorkspaceBodyBytes)
			var body struct {
				Name string `json:"name"`
				Path string `json:"path"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Path == "" {
				http.Error(w, "bad request", http.StatusBadRequest)
				return
			}
			ws, err := reg.Add(body.Name, body.Path)
			if err != nil {
				refuse(w, r, err)
				return
			}
			w.WriteHeader(http.StatusCreated)
			jsonResp(w, ws)
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	})

	mux.HandleFunc("/api/v1/workspaces/", func(w http.ResponseWriter, r *http.Request) {
		rest := strings.TrimPrefix(r.URL.Path, "/api/v1/workspaces/")
		if rest == "" {
			http.NotFound(w, r)
			return
		}

		if id, ok := strings.CutSuffix(rest, "/files"); ok {
			if id == "" || strings.Contains(id, "/") {
				http.NotFound(w, r)
				return
			}
			if r.Method != http.MethodGet {
				http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
				return
			}
			handleFiles(w, r, reg, id)
			return
		}

		if id, ok := strings.CutSuffix(rest, "/file"); ok {
			if id == "" || strings.Contains(id, "/") {
				http.NotFound(w, r)
				return
			}
			if r.Method != http.MethodGet {
				http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
				return
			}
			handleFile(w, r, reg, id)
			return
		}

		if id, ok := strings.CutSuffix(rest, "/tree"); ok {
			if id == "" || strings.Contains(id, "/") {
				http.NotFound(w, r)
				return
			}
			if r.Method != http.MethodGet {
				http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
				return
			}
			handleTree(w, r, reg, id)
			return
		}

		if strings.Contains(rest, "/") {
			http.NotFound(w, r)
			return
		}
		if r.Method != http.MethodDelete {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if err := reg.Remove(rest); err != nil {
			refuse(w, r, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
}

// maxWorkspaceBodyBytes bounds the one JSON body this file decodes.
const maxWorkspaceBodyBytes = 4096

// registryInternalErrorBody is what a 500 from a registry mutation says, in
// place of err.Error().
//
// A 500 here means this daemon's own state is wrong — saveLocked could not write
// ~/.tether/workspaces.json, concretely — and there is nothing in the detail a
// caller can act on. err.Error() in its place put daemon-side paths into an HTTP
// body for no benefit (`open /home/.../workspaces.json.tmp: permission denied`).
// The detail is logged, where an operator who has that filesystem in front of
// them can use it. Same rule, same wording, as skill/api.go's
// overlayInternalErrorBody (tether#147; the skill side took it in tether#156 and
// this side was left behind).
const registryInternalErrorBody = "the daemon could not complete this request"

// refuse is the single exit for a failed registry mutation: it picks the status
// AND the body from registryRefusal, and logs the error rather than sending it.
//
// The rule that the body comes from the SENTINEL and never from the error value
// is the fix, not the phrasing of any one message. err.Error() is assembled from
// whatever the failure carried — an os.PathError's daemon-side path, or the
// candidate path in Add's two refusals — and no caller can act on any of it.
// Deriving the body from the identity of the refusal means the next error to
// carry a path cannot leak it either.
func refuse(w http.ResponseWriter, r *http.Request, err error) {
	code, body := registryRefusal(err)
	switch code {
	case http.StatusInternalServerError:
		slog.Error("workspace registry request failed", "method", r.Method, "path", r.URL.Path, "error", err)
	case http.StatusConflict:
		// The detach failure's cause was the whole of the old 500 body, and dropping
		// it from the response without putting it anywhere would trade a leak for a
		// blind spot. It is about the filesystem rather than about the request, so it
		// belongs to the operator, who has that filesystem in front of them.
		slog.Warn("workspace registry request refused", "method", r.Method, "path", r.URL.Path, "error", err)
	}
	http.Error(w, body, code)
}

// registryRefusal maps a registry mutation's refusals onto a code AND a body a
// caller can act on.
//
// The two 400s are the caller's mistake and no retry of the same request fixes
// either: it named a relative path, or it named something that is not a directory
// on this host (tether#147). 400 rather than the 409 the skill package gives its
// own ErrWorkspaceDirUnusable, because there the caller sent an ID and the
// unusable path was the DAEMON's stored value; here the caller sent the path
// itself.
//
// The 409 is tether#156's: the registration is still registered because the
// overlays inside it could not be detached, the filesystem state that stopped
// that is visible to the caller, and a retry finishes the job.
func registryRefusal(err error) (int, string) {
	switch {
	case errors.Is(err, ErrWorkspacePathNotAbsolute):
		return http.StatusBadRequest, ErrWorkspacePathNotAbsolute.Error()
	case errors.Is(err, ErrWorkspacePathUnusable):
		return http.StatusBadRequest, ErrWorkspacePathUnusable.Error()
	case errors.Is(err, ErrOverlayCleanup):
		return http.StatusConflict, ErrOverlayCleanup.Error()
	default:
		return http.StatusInternalServerError, registryInternalErrorBody
	}
}

// handleFiles serves GET /api/v1/workspaces/{id}/files?dir=<rel>.
func handleFiles(w http.ResponseWriter, r *http.Request, reg *Registry, id string) {
	ws, ok := reg.Get(id)
	if !ok {
		http.NotFound(w, r)
		return
	}

	root, err := builtin.New(ws.Path)
	if err != nil {
		http.Error(w, "workspace root not accessible", http.StatusInternalServerError)
		return
	}

	dir := r.URL.Query().Get("dir")
	absDir, err := root.SafeJoin(dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			http.NotFound(w, r) // well-formed path, target dir just doesn't exist
			return
		}
		http.Error(w, "invalid dir: "+err.Error(), http.StatusBadRequest)
		return
	}

	entries, err := listFiles(absDir)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	jsonResp(w, entries)
}

// fileContentResponse is the JSON body for GET /api/v1/workspaces/{id}/file.
type fileContentResponse struct {
	Path      string `json:"path"`
	Content   string `json:"content"`
	Truncated bool   `json:"truncated"`
}

// handleFile serves GET /api/v1/workspaces/{id}/file?path=<rel>: it reads
// one file's content (capped at 1 MiB, see ReadFileContent), mirroring
// handleFiles' workspace-resolution and error-mapping (tether#20 Task 6).
// A bad path (traversal, missing, or a directory) never 500s: it maps to
// 400 (bad path) or 404 (not found).
func handleFile(w http.ResponseWriter, r *http.Request, reg *Registry, id string) {
	ws, ok := reg.Get(id)
	if !ok {
		http.NotFound(w, r)
		return
	}

	path := r.URL.Query().Get("path")
	if path == "" {
		http.Error(w, "path is required", http.StatusBadRequest)
		return
	}

	content, truncated, err := ReadFileContent(ws.Path, path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			http.NotFound(w, r) // well-formed path, target just doesn't exist
			return
		}
		http.Error(w, "invalid path: "+err.Error(), http.StatusBadRequest)
		return
	}
	jsonResp(w, fileContentResponse{Path: path, Content: content, Truncated: truncated})
}

// treeResponse is the JSON body for GET /api/v1/workspaces/{id}/tree.
type treeResponse struct {
	Files     []string `json:"files"`
	Truncated bool     `json:"truncated"`
}

// handleTree serves GET /api/v1/workspaces/{id}/tree?limit=N: a flat, recursive
// list of file paths (relative, forward-slash) under the workspace root, for the
// @-mention fuzzy file picker (tether#47). Heavy/VCS dirs are skipped and the
// list is capped (default 5000, hard max 20000) so a huge repo can't flood the
// response. The frontend does the fuzzy match client-side over this list.
func handleTree(w http.ResponseWriter, r *http.Request, reg *Registry, id string) {
	ws, ok := reg.Get(id)
	if !ok {
		http.NotFound(w, r)
		return
	}
	limit := 5000
	if q := r.URL.Query().Get("limit"); q != "" {
		if n, err := strconv.Atoi(q); err == nil && n > 0 {
			if n > 20000 {
				n = 20000
			}
			limit = n
		}
	}
	files, truncated, err := listFilesRecursive(ws.Path, limit)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	jsonResp(w, treeResponse{Files: files, Truncated: truncated})
}

func jsonResp(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
