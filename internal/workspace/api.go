package workspace

import (
	"encoding/json"
	"errors"
	"io/fs"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"syscall"

	"github.com/piaobeizu/tether/internal/mcp/builtin"
)

// RegisterAPI wires workspace REST endpoints into mux (s7).
//
//	GET    /api/v1/workspaces               → list all workspaces
//	POST   /api/v1/workspaces               → add workspace {"name":"...","path":"..."};
//	                                          path must be absolute and must already
//	                                          be a directory, else 400 (tether#147)
//	DELETE /api/v1/workspaces/{id}          → remove workspace by ID
//	GET    /api/v1/workspaces/{id}/files    → list files directly under {dir} (default: root);
//	                                          {dir} must name a directory inside the
//	                                          workspace, else 400/404 (tether#159)
//	GET    /api/v1/workspaces/{id}/file     → read one file's content ({"path":..,"content":..,"truncated":..})
//	GET    /api/v1/workspaces/{id}/tree     → flat recursive file list for @-mention ({"files":[..],"truncated":..})
//
// No handler on this file puts err.Error() in a response body. The two mutations
// go through refuse/registryRefusal and the three reads through
// refuseRead/readRefusal; both pick the body from the refusal's identity, and
// both log the detail instead of sending it.
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

// registryInternalErrorBody is what a 500 from this file says when it has nothing
// to add, in place of err.Error().
//
// "Nothing to add" is the whole of the qualifier, and there is exactly one 500
// that does have something: ErrRemoveNotRecorded, whose caller is left with a side
// effect that outlived the failure (see registryRefusal). Every other 500 here is
// a bare write or read failure with no consequence the caller can be told about.
//
// A 500 here means this daemon's own state or filesystem is wrong — saveLocked
// could not write ~/.tether/workspaces.json, or a read hit an EACCES/EIO under a
// registered workspace — and there is nothing in the detail a caller can act on.
// err.Error() in its place put daemon-side paths into an HTTP body for no benefit
// (`open /home/.../workspaces.json.tmp: permission denied`). The detail is
// logged, where an operator who has that filesystem in front of them can use it.
// Same rule, same wording, as skill/api.go's overlayInternalErrorBody
// (tether#147; the skill side took it in tether#156 and this side was left
// behind. tether#159 extended it from the two mutations to the three reads,
// which is why one constant now serves both).
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
//
// The named 500 is tether#162's, and it is a 500 rather than a 409 because what
// went wrong is on this side: the daemon could not write its own registry. What it
// borrows from the 400s and the 409 is the rule — the body comes from the
// sentinel's identity — because the sentence a caller needs after a rolled-back
// removal is not the sentence the generic 500 says. It is the one refusal on this
// switch whose text is about a side effect rather than about the request: the
// overlays are already detached, and no later request will mention it. A retry is
// still the right next move (skill.Disable is idempotent, so the detach re-runs
// harmlessly), which is why the message says what state the workspace is in rather
// than telling the caller to give up.
func registryRefusal(err error) (int, string) {
	switch {
	case errors.Is(err, ErrWorkspacePathNotAbsolute):
		return http.StatusBadRequest, ErrWorkspacePathNotAbsolute.Error()
	case errors.Is(err, ErrWorkspacePathUnusable):
		return http.StatusBadRequest, ErrWorkspacePathUnusable.Error()
	case errors.Is(err, ErrOverlayCleanup):
		return http.StatusConflict, ErrOverlayCleanup.Error()
	case errors.Is(err, ErrRemoveNotRecorded):
		return http.StatusInternalServerError, ErrRemoveNotRecorded.Error()
	default:
		return http.StatusInternalServerError, registryInternalErrorBody
	}
}

// The bodies the three READ handlers send for the refusals they can name.
//
// Constants here rather than the sentinels' own Error() strings — which is what
// registryRefusal does for this package's own sentinels — because two of the
// three come from internal/mcp/builtin, where the text is worded for an MCP tool
// result and is byte-pinned to the fmt.Errorf string it replaced. The rule
// tether#147 established is that the body is chosen by the IDENTITY of the
// refusal and never assembled from the error value; mapping a foreign sentinel
// onto a local constant is that rule, not an exception to it.
const (
	// readMustBeRelativeBody — builtin.ErrAbsolutePath. `dir`/`path` name a
	// location inside the workspace, and the caller does not get to say which
	// workspace by naming an absolute path.
	readMustBeRelativeBody = "workspace: that path must be relative to the workspace root"

	// readOutsideWorkspaceBody — builtin.ErrPathEscapesRoot. Deliberately does not
	// distinguish traversal from a symlink that pointed out: the difference is a
	// property of the daemon's filesystem, and telling an authenticated caller
	// which one it tripped is a probe of that filesystem, not an answer it can act
	// on.
	readOutsideWorkspaceBody = "workspace: that path is outside the workspace"

	// readNotADirectoryBody — builtin.ErrNotDirectory, the leak this wi exists for.
	readNotADirectoryBody = "workspace: that path is not a directory"
)

// refuseRead is the single exit for a failed READ on this route file — /files,
// /file and /tree: it picks the status AND the body from readRefusal, and logs
// the error rather than sending it.
//
// It is a second function rather than more cases in refuse because the two sets
// of refusals do not overlap at all: a read cannot fail the way a registry
// mutation fails, and the 404 below has no counterpart on the mutation side. What
// the two DO share is the rule, and they share it by construction — the body
// comes from the identity of the refusal in both.
//
// Only the 500 is logged. Every 400 here is fully determined by the caller's own
// query string, so there is nothing in one an operator does not already have; the
// mutation side logs its 409 because that one's cause was filesystem state the
// response no longer carries.
func refuseRead(w http.ResponseWriter, r *http.Request, err error) {
	code, body := readRefusal(err)
	switch code {
	case http.StatusNotFound:
		// net/http's own 404 body, which is what these routes have always sent for
		// a target that is not there and what their tests pin.
		http.NotFound(w, r)
		return
	case http.StatusInternalServerError:
		slog.Error("workspace read request failed", "method", r.Method, "path", r.URL.Path, "error", err)
	}
	http.Error(w, body, code)
}

// readRefusal maps a read's refusals onto a code AND a body a caller can act on.
//
// The 404 is checked FIRST and is the oldest mapping on these routes: a
// well-formed path whose target is not there is not a bad request. Both /files
// and /file have tests pinning it, and tether#159 was explicitly not allowed to
// change it. It is in this switch rather than in refuseRead so that the mapping
// is total — a caller passing an fs.ErrNotExist cannot fall through to a 500 —
// and its body is the one thing this function does not choose, hence the empty
// string and refuseRead's special case.
//
// The four 400s are all "you named the wrong thing", and none of them is fixed by
// retrying the same request: an absolute path, a path that leaves the workspace,
// a non-directory where /files needs a directory, a directory where /file needs a
// file. The last two are the pair tether#159 had to decide together, and they got
// the same status because they are the same mistake from the two ends of one file
// browser — and 400 specifically because /file already answered a directory with
// 400 and had a test saying so, so the alternative was to change a behaviour
// nobody had complained about in order to match one that was broken.
//
// Everything else is a 500 with no detail. That is the whole of the fix for the
// three sites that used to send err.Error(): an *fs.PathError from os.ReadDir,
// os.Stat, os.Open or a read carries the daemon's absolute path, and there is no
// way to classify those individually that does not amount to handing an
// authenticated caller a filesystem probe.
func readRefusal(err error) (int, string) {
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return http.StatusNotFound, ""
	case errors.Is(err, builtin.ErrAbsolutePath):
		return http.StatusBadRequest, readMustBeRelativeBody
	case errors.Is(err, builtin.ErrPathEscapesRoot):
		return http.StatusBadRequest, readOutsideWorkspaceBody
	// Two identities, one refusal. The first is SafeJoinDir's, which /files
	// resolves through. The second is for /file, which resolves through plain
	// SafeJoin because it needs a FILE — so when its `path` names something under a
	// regular file (`a.txt/b`), EvalSymlinks' bare syscall.ENOTDIR arrives
	// unclassified, and a caller mistake would otherwise be reported as a daemon
	// fault. Inert on Windows, which reports a path error instead.
	case errors.Is(err, builtin.ErrNotDirectory), errors.Is(err, syscall.ENOTDIR):
		return http.StatusBadRequest, readNotADirectoryBody
	case errors.Is(err, ErrPathIsDirectory):
		return http.StatusBadRequest, ErrPathIsDirectory.Error()
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

	// SafeJoinDir, not SafeJoin: the shape check belongs to the resolver, so this
	// handler never sees the os.ReadDir error whose *fs.PathError was the leak
	// (tether#159 — see builtin.Registry.SafeJoinDir for why it is a sibling and
	// not a check inside SafeJoin).
	dir := r.URL.Query().Get("dir")
	absDir, err := root.SafeJoinDir(dir)
	if err != nil {
		refuseRead(w, r, err)
		return
	}

	entries, err := listFiles(absDir)
	if err != nil {
		// Reachable only if the directory SafeJoinDir just stat'd stops being a
		// readable directory before os.ReadDir gets to it, or is unreadable to this
		// process — so in practice a 404 (it was removed) or a 500 (EACCES/EIO).
		// Routed through the same exit anyway: "no raw filesystem error leaves this
		// handler" is worth more as a property of the handler than as a property of
		// the branches someone could enumerate today.
		refuseRead(w, r, err)
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
// 400 (bad path) or 404 (not found). A path the daemon cannot READ does 500,
// which is the one status this handler gained in tether#159 — it used to answer
// those 400 with the *fs.PathError's text, absolute path and all.
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
		refuseRead(w, r, err)
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
		// Unreachable as written, and converted anyway. listFilesRecursive's WalkDir
		// callback swallows every walk error and the only non-nil error it can
		// return is its own stop sentinel, which it filters before returning — so no
		// test can drive this branch and none pretends to (tether#159). It sent
		// err.Error() before, and leaving one such site behind on the grounds that
		// it is currently dead is how the next change to that walk turns a listing
		// bug into a leak.
		refuseRead(w, r, err)
		return
	}
	jsonResp(w, treeResponse{Files: files, Truncated: truncated})
}

func jsonResp(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
