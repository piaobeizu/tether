package workspace

import (
	"encoding/json"
	"errors"
	"io/fs"
	"net/http"
	"strconv"
	"strings"

	"github.com/piaobeizu/tether/internal/mcp/builtin"
)

// RegisterAPI wires workspace REST endpoints into mux (s7).
//
//	GET    /api/v1/workspaces               → list all workspaces
//	POST   /api/v1/workspaces               → add workspace {"name":"...","path":"..."}
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
				http.Error(w, err.Error(), http.StatusInternalServerError)
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
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
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
