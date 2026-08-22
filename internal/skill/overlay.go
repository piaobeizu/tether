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

	// ErrOverlayEscapesWorkspace — the overlay path does not stay inside the
	// registered directory, so this daemon cannot show that a write (or a delete)
	// lands where the registry says it does. tether#156.
	//
	// Two shapes reach it, and they are the same defect at two depths:
	//
	//   - a component BELOW the root (`.claude`, or `.claude/plugins`) is a
	//     symlink, or is not a directory at all. os.MkdirAll, os.Symlink and
	//     os.Remove all follow it, so before this existed an authenticated caller
	//     who could arrange one link inside a registered workspace could put the
	//     overlay anywhere on the host — and take it back out again through
	//     Disable.
	//   - the ROOT is not its own resolution. workspace.canonicalPath resolves
	//     symlinks when it can and silently keeps the unresolved absolute path
	//     when it cannot, so the registry's stored string is only a best-effort
	//     answer to "where is this workspace". Requiring the stored path to
	//     resolve to itself is how this package stops inheriting that maybe.
	ErrOverlayEscapesWorkspace = errors.New("skill: the overlay path leaves the registered workspace directory")

	// ErrOverlayLocationOccupied — the overlay's own name inside the plugins
	// directory is taken by something this daemon did not create and will not
	// destroy (a non-empty directory, most concretely).
	//
	// A caller-visible state rather than a daemon fault, which is the whole point
	// of naming it: `_ = os.Remove(link)` swallowed the failure and the EEXIST
	// from the following os.Symlink surfaced as a 500 carrying the daemon's own
	// path in the body (tether#156).
	ErrOverlayLocationOccupied = errors.New("skill: the overlay location is occupied by something this daemon did not create")

	// ErrWorkspaceDirUnusable — the id IS registered, but the directory recorded
	// for it is not there (or is not a directory), so there is nothing to write
	// an overlay into.
	//
	// Distinct from ErrUnknownWorkspace on purpose: "you named something I do not
	// have" and "I have it and it is not on disk" send an operator to different
	// places, which is the same distinction ErrNoWorkspaceIndex draws above.
	// Enable refuses rather than creating the directory: a registration is a
	// bookmark, and materialising a bookmark's target is not something an overlay
	// write should be able to do.
	ErrWorkspaceDirUnusable = errors.New("skill: the registered workspace directory is not usable")

	// ErrSkillSourceUnusable — the sourcePath an install named is not there, or is
	// there and is not a directory. tether#147.
	//
	// One sentinel for both, and 400 rather than a 500 with the stat error, which
	// is what this replaces. Every doc in this package describes a skill as a
	// DIRECTORY (~/.tether/skills/<id>/) and os.Stat alone accepted regular files,
	// so a plain file could be installed and then linked into a workspace's plugins
	// directory for cc to read.
	//
	// Collapsing "not there" and "not a directory" into one body also narrows what
	// the endpoint tells an authenticated caller about the host: the old 500 body
	// was the stat error itself, which distinguished a missing path from a
	// permission-denied one and quoted the path back. What remains — a 201 for an
	// existing directory, a 400 for anything else — is inherent in an endpoint whose
	// job is to accept existing directories, and it leaves a registry entry behind
	// when it succeeds, so it is on the record rather than silent.
	ErrSkillSourceUnusable = errors.New("skill: a skill source must be a directory that already exists")

	// errNoOverlayDir — a component of the overlay directory is simply not there.
	//
	// Unexported because no caller is left holding it: Enable never sees it (it
	// creates what is missing), and Disable and DisableAll turn it into "the link
	// is already gone", which is what they mean.
	errNoOverlayDir = errors.New("skill: the overlay directory does not exist")
)

// overlayComponents are the two path elements between a workspace root and the
// overlay link. They are a list rather than a joined string because containment
// is decided one component at a time — see containedPluginsDir.
var overlayComponents = []string{".claude", "plugins"}

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
// That is a claim about the PATH and not yet about the directory it lands in.
// Until tether#156 the difference was the whole hole: nothing in this package
// called Lstat or EvalSymlinks on the components BELOW the root, so a symlink at
// the overlay's own directory (or at its parent) redirected the write outside the
// workspace — os.MkdirAll and os.Symlink followed it, and Disable's os.Remove
// followed it back out again. containedPluginsDir is where that is now decided;
// this function is only the first half, and its result is not a place to write
// until that one has agreed.
//
// # What this does and does not guarantee — stated precisely
//
// It is NOT "the caller can no longer choose a directory". POST
// /api/v1/workspaces (workspace/api.go) accepts any absolute path that is
// already a directory, with no allow-list and no root — so a caller holding the
// same session cookie can register a directory of its choosing and then name it
// here. The primitive is reduced from one request to two, not removed.
//
// What the second request buys is worth naming exactly, because it is the whole
// benefit: the target must be declared as a workspace first, which puts it in
// GET /api/v1/workspaces and in the SPA's left pane, so the write is no longer
// invisible. It is a smaller blast radius plus an audit record — and since
// tether#156 the record is no longer erasable while its effects stay: DELETE
// /api/v1/workspaces/{id} detaches the overlays the registration authorised
// before it drops the entry (see DisableAll).
//
// tether#147 asked whether to close the rest, and answered no, deliberately. It
// tightened what that endpoint accepts to "an absolute path that is already a
// directory" — which is what makes the audit record above mean something, and
// what moves the failure onto the request that named the path — and it stopped
// there, because the credential that reaches these endpoints also reaches
// /wt/shell, where the PTY runs an interactive coding agent as this daemon's
// user. A narrower registry would raise the bar without moving the boundary; the
// boundary is the permission hook on that path (tether#149). Do not read this
// function as making the class of bug impossible; read it as confining it to the
// declared set.
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

// containedPluginsDir resolves <wsDir>/.claude/plugins and refuses anything it
// cannot show to be inside wsDir. It is tether#156's first and main fix.
//
// # Verified, not sanitised
//
// It says what a legal overlay directory IS — the registered path, which must
// already be its own resolution, followed by components that are real
// directories — instead of listing the ways a path can be made to point
// elsewhere. A sanitiser is only ever as complete as its author's list of tricks;
// a definition of the legal form refuses everything that is not it, including the
// shapes nobody thought of. (tether#117 shipped the other kind in its first draft
// and had to be corrected: a necessary condition was written as if it were
// sufficient.)
//
// # Why the components are walked instead of MkdirAll plus a check afterwards
//
// os.MkdirAll follows links, so by the time a post-hoc check could look, the
// directories exist on the far side of one and the escape has already happened —
// a check that fires after the write is a report, not a refusal. Creating each
// missing component here with os.Mkdir means a component this daemon made cannot
// be a link, and a component it did not make is inspected before it is used.
// Nothing is created outside the workspace even on the refused path.
//
// For the same reason there is no second, post-hoc containment check: it would be
// a redundant mechanism for one condition (and this file already argues that
// three checks for one condition is not depth), and it could not close the gap
// below anyway.
//
// # What it does not close, stated rather than implied
//
// Between the Lstat here and the os.Symlink that follows, something already able
// to write inside the workspace could swap a component for a link. Closing that
// needs openat2/O_NOFOLLOW rather than os and path/filepath. It is a strictly
// smaller window than the one this removes — arranging a link and then racing the
// request, versus arranging a link and taking as long as you like.
//
// # The root
//
// The root's own symlinks are RESOLVED and then required to have been resolved
// already: EvalSymlinks must succeed and must return wsDir unchanged. That is
// what stops this package inheriting workspace.canonicalPath's silent fallback —
// it keeps the unresolved absolute path when resolution fails, correctly (it
// matches cc's behaviour, and it is what lets an entry ALREADY in
// workspaces.json survive its directory being temporarily absent), which leaves
// the stored string a best-effort answer to "where is this workspace". A stored
// path that resolves somewhere else is refused here rather than followed, so the
// directory written into is the exact string the registry hands out and the exact
// string GET /api/v1/workspaces shows.
//
// Note that the check stays load-bearing even though tether#147 made
// workspace.Registry.Add require an existing directory. Add gates NEW
// registrations; load() still accepts whatever the file holds, by design, so the
// stored string is still only best-effort at the moment this function reads it.
//
// Where the root may itself POINT is a different question. tether#147 answered
// it: anywhere, as long as it is an absolute path that is already a directory.
// Confinement is not this daemon's boundary — see workspaceDir above.
func containedPluginsDir(wsDir string, create bool) (string, error) {
	// Ordered so that each refusal has one cause. "Is it there, and is it a
	// directory" first, because a missing or non-directory root has nothing to do
	// with containment and should not be reported as an escape; only then "is it
	// the directory the registry named", which is the containment question.
	if fi, err := os.Stat(wsDir); err != nil || !fi.IsDir() {
		return "", fmt.Errorf("%w: %q", ErrWorkspaceDirUnusable, wsDir)
	}
	resolved, err := filepath.EvalSymlinks(wsDir)
	if err != nil {
		// Near-unreachable: the os.Stat above already followed every component of
		// this path. Kept rather than ignored because "I cannot check" has to be a
		// refusal — a fall-through here would be the silent best-effort this
		// function exists to stop inheriting.
		return "", fmt.Errorf("%w: %q cannot be resolved: %w", ErrWorkspaceDirUnusable, wsDir, err)
	}
	if resolved != wsDir {
		return "", fmt.Errorf("%w: the registered path %q resolves to %q", ErrOverlayEscapesWorkspace, wsDir, resolved)
	}

	dir := wsDir
	for _, name := range overlayComponents {
		next := filepath.Join(dir, name)
		fi, err := os.Lstat(next)
		switch {
		case err == nil && fi.Mode().IsDir():
			// A real directory. Lstat does not follow, and Mode().IsDir() is false
			// for the link itself, so this branch is exactly "not a symlink".
		case err == nil:
			return "", fmt.Errorf("%w: %q is a %s, not a directory",
				ErrOverlayEscapesWorkspace, next, fi.Mode().Type())
		case errors.Is(err, fs.ErrNotExist):
			if !create {
				return "", fmt.Errorf("%w: %q", errNoOverlayDir, next)
			}
			// 0o700, the mode NewRegistry gives ~/.tether and ~/.tether/skills.
			// This is daemon state that happens to live in a user's directory, its
			// entries name every skill enabled for that workspace, and its only
			// reader is cc running as this daemon's user. Directories that already
			// exist are left exactly as they are — cc creates .claude itself.
			if mkErr := os.Mkdir(next, 0o700); mkErr != nil {
				return "", fmt.Errorf("mkdir %q: %w", next, mkErr)
			}
		default:
			return "", fmt.Errorf("lstat %q: %w", next, err)
		}
		dir = next
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

// overlayLink is the one place the overlay's target path is spelled AND the one
// place it is checked, so Enable and Disable cannot disagree about where a link
// lives or about whether they are allowed to touch it — which is how the
// tether#142 asymmetry arose in the first place.
//
// The single-element check on skillID makes the containment of the FILENAME
// structural rather than a consequence of two other facts. Without it the only
// things keeping a "../.." out of the join are that newSkillID emits 16 hex chars
// and that lookup requires an exact match against an installed entry — true
// today, but neither is a statement about this join, and load() validates nothing
// it reads from disk. One line here means a hand-edited skills.json cannot
// re-arm a traversal that used to be reachable over HTTP (see ErrUnsafeSkillID).
// The skill id is checked FIRST, before the workspace is resolved and therefore
// before containedPluginsDir can create anything. A request refused on its id
// must not leave directories behind that only that request caused.
func (r *Registry) overlayLink(skillID, workspaceID string, create bool) (string, error) {
	if skillID == "" || skillID == "." || skillID == ".." || skillID != filepath.Base(skillID) {
		return "", fmt.Errorf("%w: %q is not a single path element", ErrUnsafeSkillID, skillID)
	}
	dir, err := r.workspaceDir(workspaceID)
	if err != nil {
		return "", err
	}
	plugins, err := containedPluginsDir(dir, create)
	if err != nil {
		return "", err
	}
	return filepath.Join(plugins, skillID), nil
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
// # RESIDUAL GAP (tether#142): sourcePath is still not contained
//
// The source must now be an existing DIRECTORY, and that is all it must be: an
// authenticated caller can still register any directory on the host. tether#142
// contained where the overlay symlink is CREATED; it did not contain what that
// symlink POINTS AT. Enable will happily link a registered workspace's plugin
// entry to /etc, and cc — running as the daemon's user — is what then reads it.
// So the write path is half-narrowed, and this is the half that is left.
//
// It is left open on purpose rather than by omission, and tether#147 re-confirmed
// the decision rather than inheriting it. Narrowing it means deciding where
// skills are allowed to come from, and the Settings pane installs from a
// free-text path field the user types (web/src/Settings.tsx). Restricting it to
// ~/.tether/skills/ would be correct and would remove a feature. It would also
// buy less than it looks: the credential that reaches this endpoint reaches
// /wt/shell too, where the PTY runs an interactive coding agent as this daemon's
// user, so the strong boundary on that path is the permission hook (tether#149),
// not an allow-list here.
//
// # What tether#147 DID change
//
// os.Stat alone was the check, and it left two consequences that were recorded
// here as accepted and are now closed:
//
//   - It accepted regular files, though every doc in this package describes a
//     skill as a directory (~/.tether/skills/<id>/). IsDir is now required.
//   - Its error was returned as `skill path not found: <stat error>` and the HTTP
//     layer sent that verbatim as a 500 body, which made the endpoint a
//     filesystem probe and named daemon-side paths. Both refusals now share one
//     sentinel, ErrSkillSourceUnusable, and api.go derives the body from it (the
//     rule tether#156 established for the overlay endpoints and did not reach
//     this one).
//
// The relative-path case is deliberately NOT refused here, and that is a
// difference from workspace.Registry.Add, which does refuse it. Worth naming so
// it reads as a decision: filepath.Abs resolves a relative sourcePath against the
// daemon's working directory, silently, exactly as it did there. It is a smaller
// problem in this direction — the resolved value is stored and shown in GET
// /api/v1/skills, and the IsDir check below means a relative path that resolves
// nowhere useful is refused rather than recorded — and tightening it was outside
// what tether#147 was scoped to decide.
func (r *Registry) Install(name, sourcePath string) (Skill, error) {
	abs, err := filepath.Abs(sourcePath)
	if err != nil {
		return Skill{}, err
	}
	if fi, statErr := os.Stat(abs); statErr != nil || !fi.IsDir() {
		return Skill{}, fmt.Errorf("%w: %q", ErrSkillSourceUnusable, abs)
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
//
// An id that is not installed is an error rather than a silent success, which is
// tether#156 fact 5: DELETE /api/v1/skills/{id} answered 204 for an id this
// registry had never heard of while enable and disable answered 404 for the same
// id, and nothing asserted either. 404 is the answer the rest of the route file
// already gives, for the reason its own tests state — the id in the URL is not a
// skill, and 404 is about the addressed resource — so the outlier moves.
func (r *Registry) Remove(id string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	found := false
	for _, s := range r.skills {
		if s.ID == id {
			found = true
			continue
		}
		r.skills[n] = s
		n++
	}
	if !found {
		return fmt.Errorf("%w: %q", ErrUnknownSkill, id)
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
	link, err := r.overlayLink(skillID, workspaceID, true)
	if err != nil {
		return err
	}
	// Clear a stale entry, and refuse rather than guess when what is there is not
	// one. os.Remove unlinks a SYMLINK itself (it does not follow it) and deletes
	// an empty directory; anything else — a non-empty directory, concretely —
	// fails. That failure was discarded (`_ = os.Remove(link)`), so os.Symlink
	// answered EEXIST and the HTTP layer reported a caller-visible, caller-fixable
	// state as a broken daemon, in a body carrying this daemon's own paths
	// (tether#156).
	if rmErr := os.Remove(link); rmErr != nil && !errors.Is(rmErr, fs.ErrNotExist) {
		return fmt.Errorf("%w: %q: %w", ErrOverlayLocationOccupied, link, rmErr)
	}
	if err := os.Symlink(sk.SourcePath, link); err != nil {
		return fmt.Errorf("symlink the overlay: %w", err)
	}
	return nil
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
// were TWO ways to get one, both reachable from the shipped UI:
//
//   - DELETE /api/v1/skills/{id}. Remove() deliberately does not clear workspace
//     symlinks, so the id stops resolving while the link remains. STILL OPEN, for
//     the reason below.
//   - DELETE /api/v1/workspaces/{id}. The workspace left the registry, so
//     workspaceDir could no longer resolve it — the same dead end by the other
//     argument. CLOSED by tether#156: that endpoint now detaches the overlays its
//     registration authorised before it drops the record (see DisableAll), which
//     it can do because the workspace id is in hand at exactly that moment.
//
// The first is accepted, and the reason is not "no client does this today": this
// endpoint's threat model is an authenticated caller, so an argument from what
// the SPA happens to call would be answering a different question. It is accepted
// because the alternative is to act on an id nothing vouches for, which is the
// hole itself. Cleaning it up properly needs the set of workspaces a skill is
// enabled in — state this registry does not keep. Worth its own change, not a
// silent widening here.
func (r *Registry) Disable(skillID, workspaceID string) error {
	if _, err := r.lookup(skillID); err != nil {
		return err
	}
	link, err := r.overlayLink(skillID, workspaceID, false)
	switch {
	case err == nil:
	case errors.Is(err, errNoOverlayDir), errors.Is(err, ErrWorkspaceDirUnusable):
		// Nothing to remove: the tree the link would live in is not there, so the
		// link is not there either. Disable has always been idempotent, and a
		// caller tidying up after the workspace directory itself went away is
		// exactly when that has to keep holding — an error would make the tidying
		// impossible. Note this deliberately does NOT create the tree on its way to
		// finding that out, which os.MkdirAll would have.
		return nil
	default:
		return err
	}
	if rmErr := os.Remove(link); rmErr != nil && !errors.Is(rmErr, fs.ErrNotExist) {
		return fmt.Errorf("%w: %q: %w", ErrOverlayLocationOccupied, link, rmErr)
	}
	return nil
}

// DisableAll removes every overlay this daemon created inside the workspace
// registered under workspaceID. DELETE /api/v1/workspaces/{id} runs it before it
// drops the registration; server/mux.go is where the two registries are joined.
//
// # Why the deletion had to grow a teardown at all
//
// The registration is what authorised each of these links, and dropping it used
// to leave them on disk — which is not merely untidy. Disable resolves its
// workspace THROUGH the registry, so the moment the record is gone the id it
// needs stops existing and nothing can reach the links again. Disable's doc
// called that an accepted orphan and said the real fix needed state this registry
// does not keep; that is true of the OTHER orphan (below) and was not true of
// this one — here the workspace id is in hand at exactly the right moment.
//
// # What it covers, stated exactly
//
// One link per INSTALLED skill, at the name this package would have created, and
// only where the containment rule holds. That is precisely the set Enable can
// produce and Disable can remove, which is why it is the set that is unwound —
// and it is why a name this daemon never issued is left alone, whether it is a
// file or a link someone else put in the plugins directory.
//
// It does NOT cover a link whose skill was uninstalled first: DELETE
// /api/v1/skills/{id} deliberately leaves workspace symlinks alone, so that name
// no longer resolves here either. Cleaning that one up needs the set of
// workspaces a skill is enabled in — state this registry does not keep, and a
// separate change.
//
// # Why an escaping or missing directory is success rather than an error
//
// Both mean there is nothing here this daemon would have created: it refuses to
// write through a link, and in the second case there is no tree at all. Erroring
// would make such a workspace impossible to unregister, which is a worse state
// than the one this prevents — and following the link to clean up would BE the
// escape this change exists to stop.
func (r *Registry) DisableAll(workspaceID string) error {
	for _, sk := range r.List() {
		err := r.Disable(sk.ID, workspaceID)
		switch {
		case err == nil:
		case errors.Is(err, ErrOverlayEscapesWorkspace):
			// See the paragraph above: nothing of ours is on the far side.
		case errors.Is(err, ErrUnknownSkill):
			// Uninstalled between List and here. Nothing this call can do about a
			// name that has just stopped being ours.
		default:
			return err
		}
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
