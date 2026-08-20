// Package workspace manages the workspace registry stored in ~/.tether/workspaces.toml.
package workspace

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Workspace represents a single registered workspace entry.
type Workspace struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Path      string    `json:"path"`
	AddedAt   time.Time `json:"addedAt"`
	ActiveSID string    `json:"activeSid,omitempty"` // last active cc session ID
}

// Registry manages the in-memory + persisted workspace list.
type Registry struct {
	mu         sync.RWMutex
	workspaces []Workspace
	path       string // resolved path to workspaces.toml (stored as JSON for simplicity)
}

// NewRegistry loads (or creates) the workspace registry from ~/.tether/workspaces.json.
//
// A MISSING file is not a failure — it is the first-run state, which an empty
// registry represents exactly. Any OTHER load failure (unreadable file, corrupt
// JSON) IS returned, and that distinction is the whole reason this function has an
// error to return (tether#65).
//
// # Why a corrupt file must not degrade to an empty registry
//
// The caller (server/lifecycle.go Step 2b) leaves session.Registry.Workspaces nil
// when this errors, and resolveWorkspace answers a `?ws=` request against a nil
// registry with ErrCodeNoWorkspaceRegistry ("this daemon has no workspace
// registry") rather than ErrCodeUnknownWorkspace ("that id is not registered").
// Swallowing the error here — which is what `_ = r.load()` did — produced a
// non-nil but EMPTY registry instead, so a corrupt workspaces.json refused a
// `?ws=` request as an UNKNOWN workspace: the browser said "This workspace no
// longer exists on the daemon." and an operator went looking for a workspace
// nobody had deleted. tether#52 split those two errors apart precisely to prevent
// that misdiagnosis, and tether#63 gave each its own wire code; returning the
// error here is what makes the second branch reachable at all instead of dead
// code.
//
// To be precise about how narrow that misdiagnosis window was: a browser that
// LOADS against a corrupt-registry daemon never sends `ws` at all (it has no
// workspace list to choose from), so it does not hit either error. Reaching the
// wrong one took a tab that had already loaded a good list before the file went
// bad and the daemon restarted. Narrow, but it is the case where an operator is
// already confused, and the daemon knew the real answer the whole time.
//
// # What this changes besides the diagnosis
//
// For the /wt/chat handshake, only the diagnosis: an empty registry already
// refused every `?ws=` request (there is no id in it to match), so the verdict is
// unchanged. But server/mux.go gates the whole `/api/v1/workspaces*` family on the
// same non-nil check, so a corrupt file now takes list/files/file/tree/DELETE out
// of the mux and they fall to the unconditional `/api/v1/` stub — HTTP 501 —
// where GET previously answered `[]`. Two consequences worth naming:
//
//   - The left workspace pane surfaces the failure instead of rendering an empty
//     list. That is the intended direction: an empty list is indistinguishable
//     from "you have not added anything yet".
//   - POST /api/v1/workspaces also 501s, and it used to succeed — Add's
//     saveLocked would rewrite the file, so "add a workspace" doubled as an
//     in-UI repair for a corrupt registry. That path is now gone deliberately:
//     silently overwriting a file we could not parse destroys whatever the user
//     might have wanted recovered. Recovery is to fix or remove
//     ~/.tether/workspaces.json and restart; the error returned here is logged
//     with the path and the parse error so the operator knows which file.
//
// # The other two failure modes are unreachable via `tether server`
//
// os.UserHomeDir and MkdirAll are kept because this is a library function with
// other potential callers, but the daemon cannot reach them: lifecycle.go Step 2
// calls tetherDataDir(), which performs the identical UserHomeDir + MkdirAll(
// ~/.tether) and returns on error, and it runs BEFORE Step 2b. So on the daemon
// path a corrupt registry file is the one way this returns an error.
func NewRegistry() (*Registry, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	dir := filepath.Join(home, ".tether")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("mkdir ~/.tether: %w", err)
	}
	path := filepath.Join(dir, "workspaces.json")
	r := &Registry{path: path}
	if err := r.load(); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("load %s: %w", path, err)
	}
	return r, nil
}

// List returns a snapshot of all registered workspaces.
func (r *Registry) List() []Workspace {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]Workspace, len(r.workspaces))
	copy(out, r.workspaces)
	return out
}

// Get returns the workspace with the given ID, and whether it was found.
func (r *Registry) Get(id string) (Workspace, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, w := range r.workspaces {
		if w.ID == id {
			return w, true
		}
	}
	return Workspace{}, false
}

// Path resolves a workspace ID to its absolute path, reporting whether the ID is
// registered at all.
//
// It exists so that a caller which must turn a CLIENT-SUPPLIED workspace id into
// a directory (tether#52 — the `?ws=` parameter on /wt/chat) can do so without
// depending on this package: session.WorkspaceLookup is exactly this one method,
// so internal/session declares the interface and *Registry satisfies it, and no
// new import edge is created in either direction.
//
// The (string, bool) shape rather than (string, error) is deliberate: there is
// exactly one way to fail — the id is not in the registry — and the CALLER is
// where that has to become an error, because only the caller knows what refusing
// costs. Handing back "" with no second return would let a caller spend an
// unchecked empty string as "use the default directory", which for the /wt/chat
// route means an unknown id silently selecting the daemon's own workspace root
// instead of being rejected. That is the failure mode this signature makes
// unavailable.
func (r *Registry) Path(id string) (string, bool) {
	if id == "" {
		return "", false
	}
	w, ok := r.Get(id)
	if !ok {
		return "", false
	}
	return w.Path, true
}

// canonicalPath is the ONE rule for turning a user-supplied workspace path into
// the string this package stores, compares and hands out: made absolute, then
// symlink-resolved.
//
// # Why the resolution happens HERE and not at the comparison sites
//
// The stored path is not private to this package. It is handed to
// session.WorkspaceLookup (which makes it an agent subprocess's cwd), to
// server/mux.go's ccWorkdirs (which makes it a transcript directory NAME), and
// to this package's own tree listing. Resolving at each of those sites means the
// rule exists three times, in three packages, and the next consumer added is the
// next one to forget — which is how this got found: the MCP builtin resolves
// (internal/mcp/builtin/workspace.go), and nothing else did, so one half of the
// daemon disagreed with the other half about which directory a workspace is.
// Recording the resolved form makes the stored value mean exactly one thing, and
// the two silent failures below stop being reachable from any consumer at all,
// including the two that live in packages this change does not touch.
//
// # The failure policy is cc's, and it is quoted rather than chosen
//
// cc canonicalises a cwd before encoding it, and on failure keeps the
// un-resolved absolute path (Claude Code 2.1.237, read from the installed
// binary on 2026-08-20):
//
//	function g4m(e){let t=LH.resolve(e??"."),r;
//	                try{r=aGi.realpathSync(t)}catch{r=t}return zu(r)}
//	function W1e(e,t){let r=g4m(e);if(t===void 0)return xN(r);...}
//
// `zu` is the identity function on this platform, `xN` is the encoder
// session.EncodeProjectDir reproduces, and W1e is what the transcript
// list/resume/delete paths call — so the ordering is resolve-then-encode, and
// the fallback on a throw is the absolute path.
//
// So this returns the absolute path when EvalSymlinks fails instead of an error,
// and that is not a convenience fallback: it is the same branch cc takes, which
// keeps the two sides agreeing in the failure case too — the entire reason this
// function exists. It also preserves the pre-existing contract that a path need
// not exist yet to be registered (filepath.Abs never required it), so a POST
// that succeeds today does not start failing because a directory is on a volume
// that is not mounted right now. A registry is a bookmark list; a bookmark to a
// directory that is temporarily absent is worth keeping.
//
// The returned error is filepath.Abs's alone (it fails only when the process has
// no working directory), kept so Add's signature still reports the one condition
// under which no path can be produced at all.
func canonicalPath(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return abs, nil
	}
	return resolved, nil
}

// Add registers a new workspace (deduplicated by path).
//
// The path is canonicalised (see canonicalPath) before it is compared or stored,
// which fixed two failures that looked unrelated and were the same bug:
//
//   - A workspace registered THROUGH A SYMLINK listed zero cc sessions. cc
//     resolves a cwd before encoding it into a transcript directory name, so an
//     un-resolved path encodes to a name cc never wrote and CCStore.List /
//     find / findStat all miss — no error, no log line.
//   - The @-mention file tree for such a workspace returned exactly `["."]`.
//     filepath.WalkDir lstats its root, so a symlink root is not a directory to
//     it and lands in the file branch, appending Rel(root, root).
//
// Dedup stays correct across the change because load canonicalises the entries
// it reads, so both sides of this comparison are canonical: a registry written
// before this change does not grow a second entry for a directory it already
// has. That is the whole reason the normalisation is in load as well as here.
func (r *Registry) Add(name, path string) (Workspace, error) {
	abs, err := canonicalPath(path)
	if err != nil {
		return Workspace{}, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, w := range r.workspaces {
		if w.Path == abs {
			return w, nil
		}
	}
	w := Workspace{
		ID:      newID(),
		Name:    name,
		Path:    abs,
		AddedAt: time.Now().UTC(),
	}
	r.workspaces = append(r.workspaces, w)
	return w, r.saveLocked()
}

// Remove removes a workspace by ID.
func (r *Registry) Remove(id string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, w := range r.workspaces {
		if w.ID != id {
			r.workspaces[n] = w
			n++
		}
	}
	r.workspaces = r.workspaces[:n]
	return r.saveLocked()
}

// load reads the registry file, then canonicalises every path it read IN MEMORY.
//
// # Why existing entries are canonicalised at all
//
// An entry written before canonicalPath existed holds whatever filepath.Abs
// produced, so a workspace someone registered through a symlink is carrying both
// silent failures described on Add right now. Canonicalising only on Add would
// fix that workspace the next time the user re-added it and not before — and
// worse, it would leave Add comparing a canonical candidate against a stored
// un-canonical path, so re-adding is exactly when the directory would acquire a
// SECOND registry entry. Doing it here is what makes both sides of Add's dedup
// canonical, which is what keeps that from happening.
//
// # Why the file is not rewritten
//
// This is a read path, and NewRegistry's doc above argues at length that this
// function must not write over a file whose contents it did not fully
// understand. Rewriting here would also change the on-disk meaning of every
// entry during startup, before the operator has done anything. Instead the
// in-memory form is canonical, every consumer sees canonical paths, and the file
// converges the next time a user-initiated Add or Remove calls saveLocked. Until
// then the file is a faithful record of what the user last did, which is the
// property that makes it recoverable by hand.
//
// # Why a failure to resolve is not a failure to load
//
// canonicalPath already falls back to the absolute path (see its doc), so an
// entry whose directory is currently missing or on an unmounted volume keeps the
// value it had and stays in the list. A registry must not become unloadable —
// which server/lifecycle.go turns into "this daemon has no workspace registry" —
// because one bookmark points somewhere temporarily unreachable.
func (r *Registry) load() error {
	data, err := os.ReadFile(r.path)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(data, &r.workspaces); err != nil {
		return err
	}
	for i := range r.workspaces {
		if r.workspaces[i].Path == "" {
			continue
		}
		if c, cerr := canonicalPath(r.workspaces[i].Path); cerr == nil {
			r.workspaces[i].Path = c
		}
	}
	return nil
}

func (r *Registry) saveLocked() error {
	b, err := json.MarshalIndent(r.workspaces, "", "  ")
	if err != nil {
		return err
	}
	tmp := r.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, r.path)
}

func newID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
