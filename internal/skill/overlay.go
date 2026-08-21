// Package skill implements the D-20 cc plugin overlay (symlink farm).
// Skills live in ~/.tether/skills/<id>/ (the canonical source).
// Enabling a skill for a workspace creates a symlink:
//
//	<registered workspace dir>/.claude/plugins/<id> → ~/.tether/skills/<id>/
//
// cc reads plugin manifests via the filesystem; symlinks mean updates to the
// skill source are seen by cc without an explicit copy (D-20 contract).
package skill

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"time"
)

// The overlay's refusals, as sentinels rather than bare strings, because the HTTP
// layer has to turn them into DIFFERENT answers (see overlayStatus in api.go).
// Before tether#142 every one of them was a 500, which is a misdiagnosis for
// three of the four: they are the caller's mistake, or a daemon that is
// temporarily unable to answer, not a daemon that is broken.
var (
	// ErrNoWorkspaceIndex — this daemon has no workspace registry to check a
	// request against, so no overlay write can be authorised at all.
	//
	// Kept distinct from ErrUnknownWorkspace deliberately: "I cannot check" and
	// "that is not one of yours" send an operator looking in different places.
	// tether#52 split those two errors apart for the chat handshake and tether#63
	// gave each its own wire code (workspace/state.go:50-51).
	ErrNoWorkspaceIndex = errors.New("skill: this daemon has no workspace registry")

	// ErrUnknownWorkspace — the id is not in the workspace registry, so there is no
	// directory this daemon is willing to write an overlay into.
	ErrUnknownWorkspace = errors.New("skill: workspace is not registered")

	// ErrUnknownSkill — the id is not an installed skill.
	ErrUnknownSkill = errors.New("skill: skill is not installed")

	// ErrUnsafeSkillID — a registry entry's id is not a single path element, so
	// joining it into the overlay path would escape the plugins directory.
	//
	// Unreachable through the API (ids come from newSkillID, and the only way to
	// name one is to match an installed entry) and therefore mapped to a 500: if
	// this fires, ~/.tether/skills.json was edited by hand and the daemon's own
	// state is what is wrong. It exists because the traversal it blocks was LIVE
	// before tether#142 — a request path of /api/v1/skills/%2e%2e/disable reaches
	// the handler with an id of "..", because net/http's ServeMux cleans the
	// ESCAPED path while the handler reads the DECODED one, so %2e%2e survives
	// cleaning and becomes ".." in r.URL.Path. filepath.Join then collapses it, and
	// the pre-fix Disable removed <workspace>/.claude itself.
	ErrUnsafeSkillID = errors.New("skill: registry entry has an unsafe id")
)

// WorkspaceIndex resolves a client-supplied workspace id to the directory this
// daemon has on record for it. It is the containment truth source for every
// overlay write (tether#142).
//
// Declared HERE, in the consumer, with a single method that takes and returns
// only strings: *workspace.Registry satisfies it as-is, so internal/skill gains
// no import edge on internal/workspace. That is the same arrangement
// session.WorkspaceLookup uses, and internal/workspace's own doc on Registry.Path
// says the method exists precisely to serve a caller that must turn a
// CLIENT-SUPPLIED workspace id into a directory (tether#52) — which is this
// caller exactly.
type WorkspaceIndex interface {
	Path(id string) (string, bool)
}

// Skill represents a plugin skill entry.
type Skill struct {
	ID          string    `json:"id"`
	Name        string    `json:"name"`
	Description string    `json:"description,omitempty"`
	SourcePath  string    `json:"sourcePath"` // ~/.tether/skills/<id>/
	AddedAt     time.Time `json:"addedAt"`
}

// Registry manages installed skills and their workspace symlinks.
type Registry struct {
	mu     sync.RWMutex
	skills []Skill
	path   string // ~/.tether/skills.json
	ws     WorkspaceIndex
}

// NewRegistry loads (or creates) the skill registry.
//
// A MISSING skills.json is the first-run state, which an empty registry
// represents exactly, so fs.ErrNotExist is not a failure. Every OTHER load
// failure IS returned, and that distinction is the whole reason this function has
// an error to return (tether#142 — the same correction workspace.NewRegistry
// already took in tether#65, for the same reason).
//
// # Why swallowing it was destructive rather than merely lossy
//
// The line was `_ = r.load()`. A corrupt or unreadable skills.json left r.skills
// nil and the registry otherwise healthy, so the daemon served /api/v1/skills as
// an empty list — and then the FIRST Install or Remove called saveLocked, which
// atomically renames a freshly marshalled `[]` over the file. The user's install
// records were gone, nothing had reported a problem, and the empty list the parse
// failure invented had become the truth on disk. Recovery was impossible because
// the only copy was overwritten.
//
// Returning the error instead means lifecycle.go Step 2b leaves cfg.SkillRegistry
// nil, mux.go drops the whole /api/v1/skills family (so it answers 501 from the
// generic /api/v1/ stub instead of lying with `[]`), and no write path can run at
// all — which is what keeps the file recoverable by hand. Recovery is to fix or
// remove ~/.tether/skills.json and restart; the error names the path and the
// parse error so the operator knows which file.
func NewRegistry() (*Registry, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	dir := filepath.Join(home, ".tether")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("mkdir ~/.tether: %w", err)
	}
	skillsDir := filepath.Join(dir, "skills")
	if err := os.MkdirAll(skillsDir, 0o700); err != nil {
		return nil, fmt.Errorf("mkdir ~/.tether/skills: %w", err)
	}
	path := filepath.Join(dir, "skills.json")
	r := &Registry{path: path}
	if err := r.load(); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("load %s: %w", path, err)
	}
	return r, nil
}

// BindWorkspaces installs the containment truth source Enable and Disable check
// every request against.
//
// Not a constructor argument because the daemon's two registries are built at
// different points (lifecycle.go Step 2b) and assembled into a route table later
// (mux.go) — binding at the assembly site is what lets the same *Registry be
// constructed by a caller that has no workspace registry yet.
//
// Binding nothing is a supported state, not an oversight: an unbound registry
// refuses every overlay write with ErrNoWorkspaceIndex. That is fail-closed on
// purpose — the alternative to "no truth source" is not "trust the caller", it is
// "do not write".
//
// A TYPED nil is treated as nothing too. That case earns its code: a nil
// *workspace.Registry stored in this interface is a NON-nil interface holding a
// nil pointer, so a plain `ws == nil` check would sail past it and hand a
// nil-receiver call to Path — which panics on the mutex. lifecycle.go carries a
// whole paragraph about that trap biting session.Registry.Workspaces. Detecting
// it here means a wiring site cannot make the mistake, instead of being asked not
// to.
func (r *Registry) BindWorkspaces(ws WorkspaceIndex) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if isNilIndex(ws) {
		r.ws = nil
		return
	}
	r.ws = ws
}

// isNilIndex reports whether ws holds nothing usable — an untyped nil interface,
// or a non-nil interface wrapping a nil pointer/map/slice/func.
func isNilIndex(ws WorkspaceIndex) bool {
	if ws == nil {
		return true
	}
	v := reflect.ValueOf(ws)
	switch v.Kind() {
	case reflect.Ptr, reflect.Map, reflect.Slice, reflect.Func, reflect.Interface, reflect.UnsafePointer, reflect.Chan:
		return v.IsNil()
	default:
		return false
	}
}

// workspaceDir turns a client-supplied workspace id into the directory this
// daemon stored for it, or refuses.
//
// The returned path is a string the DAEMON wrote (workspace.Registry
// canonicalises the workspace ROOT on Add and on load), never one the client
// sent. So the path the overlay is resolved against is chosen by the registry
// rather than by the caller.
//
// That is a claim about the PATH, not about the directory it lands in, and the
// difference is load-bearing. Nothing here — or anywhere in this package — calls
// Lstat or EvalSymlinks on the path components BELOW the workspace root, so a
// symlink at the overlay's own directory (or at its parent) redirects the write
// outside the workspace; os.MkdirAll and os.Symlink follow it, and Disable's
// os.Remove follows it back out again. Even the root's guarantee is best-effort:
// workspace.canonicalPath falls back to the unresolved absolute path when
// EvalSymlinks fails. Resolving that is a containment strategy in its own right
// and is tracked in tether#147; do not read this function as providing it.
//
// # What this does and does not guarantee — stated precisely
//
// It is NOT "the caller can no longer choose a directory". POST
// /api/v1/workspaces (workspace/api.go) accepts {"path":"<anything>"} with no
// validation — not even that it exists — so a caller holding the same session
// cookie can register a directory of its choosing and then name it here. The
// primitive is reduced from one request to two, not removed.
//
// What the second request buys is worth naming exactly, because it is the whole
// benefit: the target must be declared as a workspace first, which puts it in
// GET /api/v1/workspaces and in the SPA's left pane, so the write is no longer
// invisible. It is a smaller blast radius plus an audit record, and the record is
// itself erasable — DELETE /api/v1/workspaces/{id} removes the entry while the
// symlink it authorised stays on disk.
//
// Closing the rest means constraining what POST /api/v1/workspaces will accept,
// which is a product decision (the workspace pane is a free-text path field) and
// belongs to that endpoint, not to this one. Do not read this function as making
// the class of bug impossible; read it as confining it to the declared set.
//
// The IsAbs check is not redundant with `ok`. A registry entry whose Path field
// is empty — a hand-edited or truncated workspaces.json — resolves to ("", true),
// and filepath.Join("", ".claude", "plugins", id) is a RELATIVE path, which would
// put the overlay under the daemon's own working directory. Refusing a
// non-absolute answer is what keeps that from being a second way in.
func (r *Registry) workspaceDir(workspaceID string) (string, error) {
	r.mu.RLock()
	ws := r.ws
	r.mu.RUnlock()

	if isNilIndex(ws) {
		return "", ErrNoWorkspaceIndex
	}
	dir, ok := ws.Path(workspaceID)
	if !ok {
		return "", fmt.Errorf("%w: %q", ErrUnknownWorkspace, workspaceID)
	}
	if !filepath.IsAbs(dir) {
		return "", fmt.Errorf("%w: %q resolves to a non-absolute path %q", ErrUnknownWorkspace, workspaceID, dir)
	}
	return dir, nil
}

// lookup finds an installed skill by id.
//
// It returns a COPY, and that is a fix rather than a style choice: Enable used to
// keep a `*Skill` pointing into r.skills, release the read lock, and then read
// sk.SourcePath — while Remove compacts the slice by assigning to r.skills[n] in
// place under the write lock. Reading a field of an element another goroutine may
// be overwriting is a data race; copying under the lock ends it.
func (r *Registry) lookup(skillID string) (Skill, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for i := range r.skills {
		if r.skills[i].ID == skillID {
			return r.skills[i], nil
		}
	}
	return Skill{}, fmt.Errorf("%w: %q", ErrUnknownSkill, skillID)
}

// overlayLink is the one place the overlay's target path is spelled, so Enable
// and Disable cannot disagree about where a link lives — which is how the
// tether#142 asymmetry arose in the first place.
//
// The single-element check on skillID makes the containment of the FILENAME
// structural rather than a consequence of two other facts. Without it the only
// things keeping a "../.." out of the join are that newSkillID emits 16 hex chars
// and that lookup requires an exact match against an installed entry — true
// today, but neither is a statement about this join, and load() validates nothing
// it reads from disk. One line here means a hand-edited skills.json cannot
// re-arm a traversal that used to be reachable over HTTP (see ErrUnsafeSkillID).
func overlayLink(workspaceDir, skillID string) (string, error) {
	if skillID == "" || skillID == "." || skillID == ".." || skillID != filepath.Base(skillID) {
		return "", fmt.Errorf("%w: %q is not a single path element", ErrUnsafeSkillID, skillID)
	}
	return filepath.Join(workspaceDir, ".claude", "plugins", skillID), nil
}

// List returns all installed skills.
func (r *Registry) List() []Skill {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]Skill, len(r.skills))
	copy(out, r.skills)
	return out
}

// Install registers a skill from sourcePath and assigns an ID.
//
// # RESIDUAL GAP (tether#142): sourcePath is not contained
//
// os.Stat is the only check, so an authenticated caller can register ANY path on
// the host — including a file rather than a directory, and including one it
// cannot otherwise read. tether#142 contained where the overlay symlink is
// CREATED; it did not contain what that symlink POINTS AT. Enable will happily
// link a registered workspace's plugin entry to /etc, and cc — running as the
// daemon's user — is what then reads it. So the write path is half-narrowed, and
// this is the half that is left.
//
// Two further consequences of the same os.Stat, recorded so they are not
// rediscovered as new bugs:
//
//   - It is an existence oracle. A missing path comes back as a 500 whose body is
//     the stat error, so an authenticated caller can probe the filesystem for what
//     is there.
//   - It accepts regular files, though every doc in this package describes a
//     skill as a directory (~/.tether/skills/<id>/).
//
// Not fixed here on purpose rather than by omission: narrowing this means deciding
// where skills are allowed to come from, and the Settings pane installs from a
// free-text path field the user types (web/src/Settings.tsx). Restricting it to
// ~/.tether/skills/ would be correct and would remove a feature, which is a
// product call and its own change.
func (r *Registry) Install(name, sourcePath string) (Skill, error) {
	abs, err := filepath.Abs(sourcePath)
	if err != nil {
		return Skill{}, err
	}
	if _, err := os.Stat(abs); err != nil {
		return Skill{}, fmt.Errorf("skill path not found: %w", err)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, s := range r.skills {
		if s.SourcePath == abs {
			return s, nil
		}
	}
	s := Skill{
		ID:         newSkillID(),
		Name:       name,
		SourcePath: abs,
		AddedAt:    time.Now().UTC(),
	}
	r.skills = append(r.skills, s)
	return s, r.saveLocked()
}

// Remove uninstalls a skill by ID (does NOT remove workspace symlinks).
func (r *Registry) Remove(id string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, s := range r.skills {
		if s.ID != id {
			r.skills[n] = s
			n++
		}
	}
	r.skills = r.skills[:n]
	return r.saveLocked()
}

// Enable creates a symlink for skillID inside the workspace registered under
// workspaceID (D-20 §3):
//
//	<registered workspace dir>/.claude/plugins/<skillID> → skill.SourcePath
//
// # Why this takes a workspace ID and not a workspace path
//
// It used to take the path, straight off the request body with nothing but an
// emptiness check — so an authenticated caller picked any directory on the host
// and this created `.claude/plugins/` inside it, plus a symlink to any source
// that had been installed (tether#142). Containment needs a truth source for
// "what counts as a workspace", and this daemon already has exactly one.
//
// Taking the ID is better than keeping the path and checking it: a check on a
// client-supplied path is a list of the escapes someone thought of, whereas an id
// lookup can only ever return a directory the daemon itself recorded. It is NOT
// total containment, and workspaceDir's doc says why — a caller can register a
// directory of its choosing first. The gain is that the target must be a declared
// workspace, which is a smaller and an auditable set.
//
// The narrowing is free: at 9968aed nothing calls this endpoint. web/src reaches
// /api/v1/skills for list, install and remove only, so there is no client whose
// request body has to keep working.
func (r *Registry) Enable(skillID, workspaceID string) error {
	sk, err := r.lookup(skillID)
	if err != nil {
		return err
	}
	dir, err := r.workspaceDir(workspaceID)
	if err != nil {
		return err
	}
	link, err := overlayLink(dir, skillID)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
		return fmt.Errorf("mkdir plugins: %w", err)
	}
	_ = os.Remove(link) // remove stale link
	return os.Symlink(sk.SourcePath, link)
}

// Disable removes the symlink for skillID from the workspace registered under
// workspaceID.
//
// BOTH arguments are validated, and that symmetry with Enable is the tether#142
// fix. Enable resolved skillID through the registry and this did not, so both
// components of the joined path were raw caller strings feeding an os.Remove: an
// authenticated caller could delete any `<any dir>/.claude/plugins/<any name>` on
// the host. Nothing about that was a deliberate difference between the two
// endpoints — they were hand-copied handlers that drifted, which is why api.go
// now serves them through one function.
//
// # What validating skillID costs, stated rather than hidden
//
// A link this refuses to delete is a link that stays on disk forever, and there
// are TWO ways to get one, both reachable from the shipped UI:
//
//   - DELETE /api/v1/skills/{id}. Remove() deliberately does not clear workspace
//     symlinks, so the id stops resolving while the link remains.
//   - DELETE /api/v1/workspaces/{id}. The workspace leaves the registry, so
//     workspaceDir can no longer resolve it — the same dead end by the other
//     argument.
//
// The orphan is accepted anyway, and the reason is not "no client does this
// today": this endpoint's threat model is an authenticated caller, so an argument
// from what the SPA happens to call would be answering a different question. It
// is accepted because the alternative is to act on an id nothing vouches for,
// which is the hole itself. A caller that is going to delete the workspace
// registration should disable first, and cleaning up on Remove is the real fix —
// it needs the set of workspaces a skill is enabled in, state this registry does
// not keep. Worth its own change, not a silent widening here.
func (r *Registry) Disable(skillID, workspaceID string) error {
	if _, err := r.lookup(skillID); err != nil {
		return err
	}
	dir, err := r.workspaceDir(workspaceID)
	if err != nil {
		return err
	}
	link, err := overlayLink(dir, skillID)
	if err != nil {
		return err
	}
	if err := os.Remove(link); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func (r *Registry) load() error {
	data, err := os.ReadFile(r.path)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, &r.skills)
}

func (r *Registry) saveLocked() error {
	b, err := json.MarshalIndent(r.skills, "", "  ")
	if err != nil {
		return err
	}
	tmp := r.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, r.path)
}

func newSkillID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
