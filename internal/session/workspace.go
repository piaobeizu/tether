package session

// Per-session workspace binding (tether#52).
//
// A chat connection names the workspace it wants by ID — never by path. This
// file is where that ID becomes a directory, and it is the only place in the
// daemon where that conversion happens, so "which directory may a request
// choose?" has one answer with one gate in front of it.
//
// # Why the ID is the whole design
//
// The agent subprocess's cwd is where it reads and writes files. Letting a
// request carry a PATH would hand that choice to whoever opened the connection;
// letting it carry an unvalidated id and falling back to a default on a miss
// would do the same thing more quietly. So an id that is not in the user's
// workspace registry (~/.tether/workspaces.json) is refused, and refusing
// happens before anything is spawned.
//
// # Why the binding is remembered
//
// cc's `--resume` is cwd-scoped: the transcript lives at
// ~/.claude/projects/<encoded-cwd>/<uuid>.jsonl (mem_2ruSlrHR ④), and resuming
// from a different cwd fails exactly like an unknown uuid (③). A session's
// workspace is therefore part of its identity for its whole life, not a
// per-connection preference — so the daemon records it and honours it on
// reconnect, rather than trusting each connection to send it back. That also
// means the browser does not have to: it sends `ws` only when it is starting a
// NEW session (web/src/lib/chatUrl.ts), which is what stops a reconnect from
// being able to relocate a live conversation.

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/piaobeizu/tether/internal/wire"
)

// WorkspaceLookup answers "what absolute path does this workspace id name?".
//
// Declared here, as the single method the registry needs, rather than importing
// internal/workspace: *workspace.Registry satisfies it with its Path method, so
// the dependency stays one-directional and this package keeps testing against a
// two-line fake instead of a JSON file in $HOME.
//
// A false second return MUST mean "not registered" and nothing else. Every
// caller here turns it into a refusal.
type WorkspaceLookup interface {
	Path(id string) (string, bool)
}

// WorkspaceBinding is the workspace a session runs in: the id the user's
// registry knows it by, and the absolute path that id resolved to at the moment
// the session was created.
//
// Both halves are stored, and the path is the one that is USED. The id is kept
// so an operator (and a log line) can tell which registry entry a session came
// from, but a session's directory is deliberately NOT re-derived from the id on
// every reconnect — see resolveWorkspace.
//
// The zero value means "no workspace was selected", which resolves to the
// daemon-global default (Registry.Workdir, i.e. --workspace-root). That is the
// pre-tether#52 behaviour and the answer for any client that sends no `ws`.
type WorkspaceBinding struct {
	WorkspaceID string `json:"workspaceId"`
	Path        string `json:"path"`
}

// BindingStore persists WorkspaceBindings at
// <baseDir>/<sid>/workspace.json — the same per-session directory
// HistoryStore already owns for history.jsonl.
//
// It is a separate type rather than two more methods on HistoryStore because it
// answers a different question: HistoryStore stores what was SAID in a session,
// this stores WHERE the session is. Keeping them apart also keeps "bindings
// disabled" a state a test can construct on its own (Registry.Bindings == nil)
// without also disabling history.
//
// Persisted rather than held in memory because it has to survive a daemon
// restart: a reconnect sends a sid and no workspace, so after a restart this
// file is the ONLY thing that knows where that session lives. An in-memory map
// would send every post-restart reconnect into the default directory, where its
// `--resume` would fail and its context would be lost — the exact thing
// tether#50 bought.
type BindingStore struct {
	baseDir string
}

// NewBindingStore returns a store rooted at baseDir (~/.tether/sessions).
func NewBindingStore(baseDir string) *BindingStore {
	return &BindingStore{baseDir: baseDir}
}

// Load returns the binding recorded for sid, and whether there was one. A
// missing file is the common case (any session created before tether#52, and
// every session that never selected a workspace), so it is not logged.
func (s *BindingStore) Load(sid string) (WorkspaceBinding, bool) {
	// The sid on the reconnect path comes from the client, and this joins it into a
	// filesystem path. Without the guard a `..`-shaped id would read a
	// workspace.json from somewhere unintended and that file's `path` would become
	// an agent's cwd — the caller-supplied-path hole this whole file exists to
	// close, reintroduced through the store. See ValidSessionID (history.go) for
	// why it is one shared allowlist rather than a local `..` check.
	if !ValidSessionID(sid) {
		return WorkspaceBinding{}, false
	}
	data, err := os.ReadFile(s.path(sid))
	if err != nil {
		if !os.IsNotExist(err) {
			slog.Warn("workspace binding: read failed", "sid", sid, "err", err)
		}
		return WorkspaceBinding{}, false
	}
	var b WorkspaceBinding
	if err := json.Unmarshal(data, &b); err != nil {
		slog.Warn("workspace binding: corrupt file, ignoring", "sid", sid, "err", err)
		return WorkspaceBinding{}, false
	}
	if b.Path == "" {
		// A binding with no path names no directory; treating it as present would
		// pin a session to "" and defeat the daemon-default fallback.
		return WorkspaceBinding{}, false
	}
	return b, true
}

// Save records b as sid's workspace, overwriting any previous record.
//
// Written whole via a UNIQUELY-NAMED temp file + rename, so no reader ever sees a
// partial binding and two concurrent writers cannot interleave into a mixed one.
// A fixed `<path>.tmp` would allow exactly that (the spawn race tether#60 tracks
// can put two Saves on one sid), and a torn read here would not cost a transcript
// line — it would put an agent in the wrong directory. Sync before rename so the
// same holds across a crash rather than only across a concurrent read.
func (s *BindingStore) Save(sid string, b WorkspaceBinding) error {
	if !ValidSessionID(sid) {
		return fmt.Errorf("workspace binding: refusing to write under sid %q", sid)
	}
	path := s.path(sid)
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("workspace binding: mkdir: %w", err)
	}
	data, err := json.Marshal(b)
	if err != nil {
		return fmt.Errorf("workspace binding: marshal: %w", err)
	}
	f, err := os.CreateTemp(dir, "workspace-*.json.tmp")
	if err != nil {
		return fmt.Errorf("workspace binding: create temp: %w", err)
	}
	tmp := f.Name()
	// Remove the temp file on every failure path. A no-op once Rename succeeded,
	// because the name no longer exists.
	defer func() { _ = os.Remove(tmp) }()
	if err := f.Chmod(0o600); err != nil {
		_ = f.Close()
		return fmt.Errorf("workspace binding: chmod temp: %w", err)
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return fmt.Errorf("workspace binding: write: %w", err)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return fmt.Errorf("workspace binding: sync: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("workspace binding: close temp: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("workspace binding: rename: %w", err)
	}
	return nil
}

func (s *BindingStore) path(sid string) string {
	return filepath.Join(s.baseDir, sid, "workspace.json")
}

// workspaceDecision is the outcome of resolving one chat connection's requested
// workspace against the session it brought.
type workspaceDecision struct {
	// Binding is the workspace to spawn in. Zero value = the daemon default.
	Binding WorkspaceBinding
	// ResumeSID is the sid to bind to, or "" when this connection must start a
	// brand-new session instead of touching the one it asked for.
	ResumeSID string
	// Rebound is true when a sid was dropped because it belongs to a DIFFERENT
	// workspace than the one requested. It is what makes the fresh session
	// announce itself as a recovery (Resolution.Recovered/Notice) instead of
	// appearing, unexplained, with an empty transcript.
	Rebound bool
}

// resolveWorkspace decides which directory a chat connection's agent runs in,
// and whether the sid it brought may be used there.
//
// ONE rule, applied to five cases: a sid is resumed only in a directory we have
// positive evidence that session lived in. Anything else starts a fresh session
// in the requested workspace.
//
//	sid | remembered | requested | workdir     | sid
//	----+------------+-----------+-------------+---------------
//	""  | –          | any       | req/default | fresh
//	set | absent     | ""        | default     | --resume sid
//	set | absent     | set       | requested   | FRESH (dropped)
//	set | present    | ""        | REMEMBERED  | --resume sid
//	set | present    | == it     | remembered  | --resume sid
//	set | present    | != it     | requested   | FRESH (dropped)
//
// Row 4 is not a courtesy, it is the main path: the browser sends no `ws` once it
// has a sid, so the remembered binding is the only thing that knows where to
// reconnect. It is also why "requested wins" is not the rule — an empty request
// means "you know better", not "use the default".
//
// Row 2 keeps the pre-tether#52 behaviour for a sid nothing is remembered about
// (every session created before this slice, and every session that selected no
// workspace): the daemon default is where such a session almost certainly ran, so
// it is the one directory whose `--resume` can succeed.
//
// Row 3 is the case an earlier version of this function got wrong, and it is worth
// stating why. It honoured the request AND resumed — `cc --resume <sid>` in a
// directory that session had never lived in, which the whole design says never
// happens. Worse, spawnEntry would then record the requested workspace UNDER THAT
// SID, permanently rebinding a session that had actually run in the default
// directory: every later reconnect would resume into the wrong place and fail, and
// the shell pane would follow it there. A recoverable session became
// unrecoverable, from one request no UI sends. Absent evidence is not evidence, so
// this row is treated exactly like row 6.
//
// Rows 3 and 6 start a fresh session rather than refusing. Refusing would brick
// the connection: the browser retries the same URL, so a refusal is a silent
// reconnect loop (the failure mode Registry.OwnedByOther was introduced to end,
// tether#54). And a fresh session is what would happen ANYWAY three steps later —
// `--resume` in a foreign cwd fails deterministically (mem_2ruSlrHR ③) and
// Attachment.resolve answers that by spawning fresh and replaying — so this
// reaches the same end state without first starting a subprocess that is certain
// to die. What it must never do, and does not, is run the requested sid in the
// requested directory.
//
// # The remembered binding is not re-validated against the registry
//
// A session bound to a workspace the user has since REMOVED keeps its directory.
// That is deliberate. The binding is state the daemon wrote after validating the
// id; re-deriving authorization from a registry that has changed since would make
// a live session's directory depend on later UI bookkeeping, which is precisely
// the immutability this design rests on. workspace.Registry.Remove deletes a list
// entry and touches no files — it is not a revocation primitive, and treating it
// as one here would silently make every session of a removed workspace
// unresumable. Wanting removal to end its sessions is a feature, not a check.
func (r *Registry) resolveWorkspace(sid, wsID string) (workspaceDecision, error) {
	var req WorkspaceBinding
	if wsID != "" {
		// Fail closed, both ways: an unknown id AND a daemon with no registry to
		// ask. Falling back to the default directory on either would turn a
		// rejected request into a silently redirected one.
		//
		// The nil case is kept a separate branch rather than folded into a
		// deny-everything WorkspaceLookup, because the two states deserve different
		// operator-facing errors: collapsing them would send someone hunting a
		// deleted workspace when the truth is that the registry failed to load at
		// startup (lifecycle.go Step 2b logs a warning and leaves this field nil).
		if r.Workspaces == nil {
			return workspaceDecision{}, refuse(wire.ErrCodeNoWorkspaceRegistry, "workspace %q requested but this daemon has no workspace registry", wsID)
		}
		path, ok := r.Workspaces.Path(wsID)
		if !ok {
			return workspaceDecision{}, refuse(wire.ErrCodeUnknownWorkspace, "unknown workspace %q", wsID)
		}
		req = WorkspaceBinding{WorkspaceID: wsID, Path: path}
	}

	if sid == "" {
		return workspaceDecision{Binding: req}, nil
	}

	own, ok := r.sessionBinding(sid)
	if !ok {
		// Rows 2 and 3. With no request there is nothing to disagree with, so keep
		// the pre-tether#52 behaviour and resume in the daemon default. With a
		// request, we have no evidence this session ever ran there — so treat it as
		// row 6 rather than resuming somewhere it has never been.
		if req.Path == "" {
			return workspaceDecision{ResumeSID: sid}, nil
		}
		return workspaceDecision{Binding: req, Rebound: true}, nil
	}
	// Compared by PATH, not by id: the path is what cc keys its transcript on, and
	// workspace.Registry.Add deduplicates by path, so two ids for one directory
	// only arise from a remove-then-re-add — where the same path genuinely is the
	// same workspace and re-binding the session would be pure loss.
	if req.Path == "" || req.Path == own.Path {
		return workspaceDecision{Binding: own, ResumeSID: sid}, nil
	}
	return workspaceDecision{Binding: req, Rebound: true}, nil
}

// sessionBinding reports the workspace sid is already bound to, preferring the
// live registration over the file.
//
// The registration wins because it is the directory the process is ACTUALLY in,
// and because it is current: the file is written once per session, while an entry
// exists only while its agent does.
//
// The lookup is a bare map read and NOT liveEntry, deliberately — but the reason
// differs per caller, so both are stated rather than one being generalised:
//
//   - WorkdirForSession (the PTY shell pane asking where a chat session lives)
//     must not EVICT the session it is about to `--resume`. liveEntry un-registers
//     what it finds dead as a side effect (see its doc), so asking through it
//     would make a question destructive.
//   - resolveWorkspace does not need that protection — Attach re-asks through
//     liveEntry a few lines later and evicts the corpse itself. What it needs is
//     the DEAD entry's answer: a session that ran in the daemon default has no
//     binding file, so if liveEntry dropped it first this function would fall
//     through to disk, find nothing, and report "no workspace" about a session
//     whose directory is sitting right there in the entry.
//
// Either way a dead-but-registered entry's workdir is still the right answer to
// "where does this session live".
func (r *Registry) sessionBinding(sid string) (WorkspaceBinding, bool) {
	if sid == "" {
		return WorkspaceBinding{}, false
	}
	r.mu.RLock()
	e, ok := r.sessions[sid]
	r.mu.RUnlock()
	// e.workdir/e.ws are written before the entry is published under r.mu and never
	// mutated after, so acquiring r.mu above is what makes reading them race-free.
	if ok && e.workdir != "" {
		return WorkspaceBinding{WorkspaceID: e.ws.WorkspaceID, Path: e.workdir}, true
	}
	if r.Bindings == nil {
		return WorkspaceBinding{}, false
	}
	return r.Bindings.Load(sid)
}

// workdirFor is the one place the zero binding becomes a directory: a workspace's
// own path, or the daemon-global default when no workspace was selected.
func (r *Registry) workdirFor(b WorkspaceBinding) string {
	if b.Path != "" {
		return b.Path
	}
	return r.Workdir
}

// saveBinding records ws as sid's workspace, best-effort.
//
// A session that selected NO workspace is not recorded. Its answer — the daemon
// default — is what an absent binding already produces, so writing it would only
// add a file whose contents are "whatever --workspace-root happens to be next
// time", which is worse than nothing: it would pin today's default across a
// restart that changed the flag.
//
// Failure is logged, not returned. The alternative is refusing to start a session
// because a metadata file could not be written, and the cost of the missing file
// is bounded and known: after a restart that session reconnects into the default
// directory, its `--resume` fails, and tether#50's fallback gives it a fresh one.
func (r *Registry) saveBinding(sid string, ws WorkspaceBinding) {
	if r.Bindings == nil || ws.WorkspaceID == "" || ws.Path == "" || sid == "" {
		return
	}
	if err := r.Bindings.Save(sid, ws); err != nil {
		slog.Warn("could not record which workspace a session belongs to",
			"sid", sid, "workspace_id", ws.WorkspaceID, "err", err)
	}
}

// WorkdirForSession returns the directory sid's agent runs in, for a caller that
// spawns cc outside the provider abstraction.
//
// Its one caller is the PTY shell pane (internal/server/wt_shell.go), which is
// handed the CHAT session's sid and resumes it with `--resume`. Since tether#52
// the chat session may be in any registered workspace, so passing
// Registry.Workdir there — as it did before — would resume in a directory the
// conversation was never created in and drop the user into an empty one instead
// (buildPTYCommand's doc has the full argument). This keeps the two spawn paths
// in the same directory, which is the invariant tether#51 established.
//
// An unknown sid answers with the daemon default rather than an error: a shell
// opened before any chat session exists is ordinary, and so is a sid from a
// previous daemon that left no binding.
func (r *Registry) WorkdirForSession(sid string) string {
	if b, ok := r.sessionBinding(sid); ok {
		return b.Path
	}
	return r.Workdir
}
