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

// ErrOverlayCleanup — a registration could not be removed because the things it
// authorised to be written inside the workspace could not be detached first
// (tether#156).
//
// The registry keeps the record when this happens. A DELETE that answered 204
// would be promising that the registration and everything it authorised are both
// gone, and only half of that would be true — which is the orphan this wraps
// rather than an improvement on it.
//
// The underlying error is wrapped but never handed to a client: it comes from
// another package's filesystem work and can name daemon-side paths.
var ErrOverlayCleanup = errors.New("workspace: the overlays inside this workspace could not be detached")

// ErrRemoveNotRecorded — the detach succeeded, the record was dropped, and then
// the registry could not be written, so the record was put back (tether#162).
//
// # Why this is not "nothing happened"
//
// Putting the record back is the right end state (see Remove), but it is not a
// state the caller can infer from a bare 500. The detach ran BEFORE the write and
// deleting a symlink is not undone by a failed write, so what the caller is left
// holding is a registration whose overlays are gone — and the registration alone
// is what makes overlays reachable, so nothing tells them so afterwards. A 500
// that only means "the write failed" would leave them to discover from the skill
// pane that the workspace they still have is no longer the one they had.
//
// This is why the refusal is its own identity rather than the generic 500 body:
// the sentence a caller needs here is a different sentence, and registryRefusal
// can only pick it if there is something to pick it BY (tether#147's rule).
//
// "any skill overlays it had" is deliberate. The detach callback reports success
// or failure and not a count, so this cannot promise that a link existed; what it
// can promise exactly is that whatever was there was taken away and not restored.
//
// Raised ONLY when a detach actually ran. With no cleanup bound there are no
// overlays to have detached, so a rolled-back write really is "nothing happened"
// and the generic 500 is the honest answer — see Remove.
var ErrRemoveNotRecorded = errors.New("workspace: the removal could not be recorded, so this workspace is still registered; any skill overlays it had were detached first and have not been put back")

// The two refusals Add can produce, as sentinels rather than bare strings,
// because api.go has to turn them into a status AND a body that carries no
// daemon-side value (tether#147). Both are 400: the caller sent the path, and no
// amount of retrying the same request fixes either.
var (
	// ErrWorkspacePathNotAbsolute — the registration named a relative path.
	//
	// filepath.Abs would resolve it against the DAEMON's working directory, which
	// the caller does not know and cannot see: the same request answers with a
	// different registration depending on where the daemon happens to have been
	// started. Refusing is the only answer that means one thing.
	ErrWorkspacePathNotAbsolute = errors.New("workspace: a workspace path must be absolute")

	// ErrWorkspacePathUnusable — the registration named a path that is not there,
	// or is there and is not a directory.
	//
	// One sentinel for both on purpose. They are the same caller mistake ("that is
	// not a directory I can use") and keeping them apart would hand an
	// authenticated caller a finer probe of the daemon's filesystem than a
	// registration endpoint has any reason to give — see Add's doc.
	ErrWorkspacePathUnusable = errors.New("workspace: a workspace path must be a directory that already exists")
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

	// detachOverlays is installed by BindOverlayCleanup; nil until then.
	detachOverlays func(workspaceID string) error
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
// function exists.
//
// # This function does NOT decide what may be registered
//
// It used to argue the opposite as well: that the fallback also preserved a
// contract in which a path need not exist yet to be registered. tether#147
// reversed that contract, and deliberately did NOT touch this function to do it
// (see Add). The reason to keep the two apart is that they answer different
// questions. This one answers "what is the one string that names this
// directory", and for that question a best-effort answer that matches cc is the
// right one — it is used by load() on entries that are ALREADY registered, where
// a directory on a currently-unmounted volume must keep its entry rather than
// make the whole registry unloadable. Whether a NEW registration is acceptable
// is Add's question, and Add is where it is now asked.
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
//
// # A path must be absolute, and must already be a directory (tether#147)
//
// This REVERSES a written design intent, and the intent is worth stating before
// the reason it was dropped. canonicalPath's doc used to argue that a path need
// not exist to be registered — a registry is a bookmark list, and a bookmark to
// a directory on a volume that is not mounted right now is worth keeping.
//
// What changed is that the benefit turned out to be zero while the costs stayed.
// tether#156 made Enable refuse a registration whose directory is not on disk
// (skill.ErrWorkspaceDirUnusable): it stats the recorded path first and will not
// create it, on the grounds that materialising a bookmark's target is not
// something an overlay write should be able to do. So "register it now, create
// it later" already cannot be followed by anything that USES the registration
// until the directory exists — the early registration buys nothing that
// registering after mkdir would not also buy.
//
// The costs it left behind were three:
//
//   - The registry accepts arbitrary strings. It is the daemon's audit record of
//     which directories an agent may execute in (workspaceDir in internal/skill,
//     and the `?ws=` chat handshake), and a record that can hold anything is a
//     weaker record.
//   - A relative path was silently resolved against the DAEMON's working
//     directory. Nothing told the caller which directory it got, and the caller
//     generally cannot know.
//   - The failure happened at the wrong request. A path that cannot work was
//     reported by a later enable, with a worse message, instead of by the POST
//     that named it.
//
// Two things this deliberately does NOT do, because they were considered and
// refused rather than missed: it does not confine the path to any root or
// allow-list (the credential that reaches this endpoint also reaches /wt/shell,
// so the strong boundary is the permission hook on that path, tether#149 — a
// weak boundary made narrower here would raise the bar without moving it), and
// it does not apply to load(). Entries already in the file keep the
// bookmark-survives-an-unmounted-volume behaviour; only new registrations are
// gated. Making load() drop them would turn one absent directory into "this
// daemon has no workspace registry" for every request (see NewRegistry).
//
// # Order, and why each refusal has one cause
//
// IsAbs is checked on the string the CALLER sent, before canonicalPath, because
// canonicalPath's own filepath.Abs is exactly the silent resolution being
// refused — after it, the relative input is indistinguishable from an absolute
// one. The directory check then runs on the CANONICAL path, which is the value
// that gets stored, handed out, and stat'd again by
// skill.containedPluginsDir: checking anything else would leave those two able
// to disagree.
//
// # A write that failed registers nothing (tether#159)
//
// The append used to happen before saveLocked and stay there when saveLocked
// failed, so a POST that answered 500 left a registration in memory that no file
// had ever held. That is worse than either half of it on its own. The caller was
// told the request failed, and meanwhile GET /api/v1/workspaces listed the
// workspace, Path resolved its id for the /wt/chat handshake, and skill.Enable
// would write an overlay into it — all of it gone at the next restart, with
// nothing on disk to say it had been there. A 500 that also has to be read as
// "and something was registered anyway" is not a report of the write failure, it
// is a second failure.
//
// So the append is undone and the zero Workspace returned. Truncating by one is
// exact rather than approximate: the write lock is held across everything below,
// so nothing else can have appended in between. Returning Workspace{} rather than
// the entry that did not survive matters for the same reason — api.go discards it,
// but a value that names a workspace nobody registered is exactly the thing this
// paragraph is about.
//
// Remove has the mirror shape and takes the same policy for the same reason
// (tether#162). What it has to say when it rolls back is not the same, and that
// difference lives on ErrRemoveNotRecorded rather than here.
func (r *Registry) Add(name, path string) (Workspace, error) {
	if !filepath.IsAbs(path) {
		return Workspace{}, fmt.Errorf("%w: %q", ErrWorkspacePathNotAbsolute, path)
	}
	abs, err := canonicalPath(path)
	if err != nil {
		return Workspace{}, err
	}
	// Not there, and there-but-not-a-directory, collapse into one refusal. A
	// dangling symlink lands here too: canonicalPath cannot resolve it and hands
	// back the link itself, which os.Stat then follows to nothing.
	if fi, statErr := os.Stat(abs); statErr != nil || !fi.IsDir() {
		return Workspace{}, fmt.Errorf("%w: %q", ErrWorkspacePathUnusable, abs)
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
	if err := r.saveLocked(); err != nil {
		r.workspaces = r.workspaces[:len(r.workspaces)-1]
		return Workspace{}, err
	}
	return w, nil
}

// BindOverlayCleanup installs the callback Remove runs before it drops a
// registration, so that whatever the registration authorised inside the
// workspace goes with it (tether#156). Binding nothing leaves Remove as it was.
//
// # Why a func and not an interface
//
// This package's other join in the same direction — skill.WorkspaceIndex — is an
// interface declared in the consumer, and the price of that shape is documented
// at length in server/lifecycle.go and skill.BindWorkspaces: a nil *T stored in
// an interface is a NON-nil interface whose first call is a nil-receiver call, so
// every such binding has to carry a reflect-based guard. A method value taken
// from a pointer the caller has already tested cannot have that shape at all.
// Removing the failure mode is better than detecting it in a fourth place.
//
// The dependency direction is unchanged either way: this package still names no
// type from internal/skill, and internal/skill names none from this one. The one
// site that has both registries in scope (server/mux.go) is where they meet.
func (r *Registry) BindOverlayCleanup(clean func(workspaceID string) error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.detachOverlays = clean
}

// Remove removes a workspace by ID, after detaching what its registration
// authorised to be written inside it.
//
// # Why the detach is here and not left to the caller
//
// The registration is the ONLY thing that makes those files reachable. The skill
// registry resolves a workspace through this one, so the instant this record is
// gone the id it needs stops existing: a symlink left behind by this deletion
// cannot be removed through any endpoint afterwards. Deleting the record without
// unwinding it is therefore not "untidy but recoverable" — it is a leak by
// construction (tether#156).
//
// # Three orderings that are each load-bearing
//
//   - The detach runs BEFORE the record is dropped, because it resolves the id
//     through this registry.
//   - It runs OUTSIDE the write lock. It calls back into Path, which takes the
//     read lock, and sync.RWMutex is not reentrant — under the write lock this
//     would deadlock rather than fail.
//   - A detach that FAILS keeps the record. A 204 here promises that the
//     registration and everything it authorised are both gone; if only half
//     happened, the honest answer is a refusal the caller can retry, not the
//     orphan this change exists to remove with an error message on top.
//
// An id this registry does not hold is still a silent no-op, which is what the
// handler's 204 has always rested on: there is no registration to unwind, so
// there is nothing to call and nothing to refuse.
//
// # A write that failed removes nothing (tether#162)
//
// The record used to be dropped from memory before saveLocked and stay dropped
// when saveLocked failed, which is Add's tether#159 defect pointing the other
// way: memory said the workspace was gone, the file still held it, and a restart
// brought it back — with its overlays already detached, because that step ran
// first and a deleted symlink does not return.
//
// So the drop is undone. The reason this is better than leaving it is NOT that it
// recovers anything — it cannot, the detach is final either way, and both roads
// end at the same place: a registration whose overlays are gone. The difference is
// WHEN. Rolling back arrives there now; leaving it arrives there at the next
// restart and spends the time in between with GET /api/v1/workspaces and
// workspaces.json disagreeing about what is registered. There is no third state
// on offer, and a window of disagreement is worth nothing.
//
// Transactional ordering — write first, then detach — is not the missing option.
// It just swaps this failure for the opposite one: the record gone and the
// symlinks left behind with no id that can reach them, which is precisely the leak
// tether#156 added the detach to close.
//
// # Why the refusal changes identity only when a detach ran
//
// The rolled-back state is not the state before the call, and ErrRemoveNotRecorded
// exists to say the part a bare 500 cannot. But with no cleanup bound (server/mux.go
// binds one only when there is a skill registry) nothing was detached, so the
// rollback really does restore the state before the call and there is nothing extra
// to report. Claiming overlays were detached there would be a false sentence, so the
// error stays the plain write failure and api.go's generic 500 answers it.
func (r *Registry) Remove(id string) error {
	r.mu.RLock()
	detach := r.detachOverlays
	known := false
	for _, w := range r.workspaces {
		if w.ID == id {
			known = true
			break
		}
	}
	r.mu.RUnlock()

	if !known {
		return nil
	}
	if detach != nil {
		if err := detach(id); err != nil {
			return fmt.Errorf("%w: %w", ErrOverlayCleanup, err)
		}
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	// A fresh slice rather than the in-place compaction this used to do, because
	// compacting overwrites the entries it shifts down and there is then nothing
	// left to put back. `before` is read under the same write lock that spans the
	// save, so it is the exact list to restore — including an entry some other
	// goroutine appended between the read lock above and this one.
	before := r.workspaces
	kept := make([]Workspace, 0, len(before))
	for _, w := range before {
		if w.ID != id {
			kept = append(kept, w)
		}
	}
	r.workspaces = kept
	if err := r.saveLocked(); err != nil {
		r.workspaces = before
		if detach != nil {
			return fmt.Errorf("%w: %w", ErrRemoveNotRecorded, err)
		}
		return err
	}
	return nil
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
//
// That is a deliberate asymmetry with Add, which since tether#147 refuses a NEW
// registration whose path is not an existing directory. It is not an oversight to
// be tidied up later: the cost of refusing a new registration is one 400 the
// caller can act on, and the cost of dropping a loaded entry is the sentence
// above.
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
