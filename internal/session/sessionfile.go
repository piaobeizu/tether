package session

// Per-session sidecar files (tether#91).
//
// A session directory (~/.tether/sessions/<sid>/) holds its transcript plus a
// small number of JSON sidecars, one per question somebody needs answered after
// a restart: workspace.json says WHERE the session runs (tether#52), wi.json
// says WHAT it is working on (tether#91).
//
// The transcript is append-only JSONL and has its own writer. The sidecars are
// whole-file records, and every one of them needs the same three properties:
//
//   - the sid is validated before it becomes a path segment, so a client-supplied
//     id cannot address a file outside the sessions directory;
//   - a reader never sees a partially-written record;
//   - a missing or corrupt file reads as "absent", not as an error and not as a
//     half-populated value.
//
// This file is the ONE implementation of those three, and it exists because the
// alternative was visible: BindingStore.Save had grown the temp-file + chmod +
// write + sync + rename dance and its reasoning as a private method, so the
// second sidecar would have started life as a copy of it. Two copies of an
// atomic-write contract drift the same way two copies of a security guard do —
// which is the argument ValidSessionID's own doc makes about the sid check that
// used to exist twice. So the dance moved here and both stores call it.
//
// Moved with one difference, stated because "unchanged" is the kind of claim
// that ages badly: the temp file's name prefix went from `workspace-*.json.tmp`
// to `sidecar-*.json.tmp`, since it is no longer only workspaces. Nothing reads
// those names — no cleanup sweep, no test, and both listing paths walk
// DIRECTORIES — so a `workspace-*.json.tmp` left behind by a crash before this
// change is as inert as one left behind after it. Everything else is identical:
// same syscall order, same permissions, same error strings.
//
// The helpers are package-private free functions rather than methods on a shared
// embedded struct: what the two stores share is a FILE FORMAT, not an identity.
// Keeping them functions means each store still owns its own type, its own doc
// about what an absent record means for its caller, and its own error strings.

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
)

// sessionFilePath is where sidecar `name` lives for `sid`.
//
// It does NOT validate, and its only two callers are readSessionJSON and
// writeSessionJSON in this file, both of which validate first. That is a
// property of the current code, not a guarantee of the function, so it has to be
// re-checked rather than assumed: this change originally left BindingStore.path
// calling it directly — dead, but unguarded and one edit away from being live —
// and review caught it. Anything that needs a sidecar path goes through the two
// functions below.
func sessionFilePath(baseDir, sid, name string) string {
	return filepath.Join(baseDir, sid, name)
}

// readSessionJSON decodes <baseDir>/<sid>/<name> into T, reporting whether a
// usable record was found.
//
// False, with no error to the caller, for all four ways there can fail to be one:
// a sid we refuse to turn into a path, no file, an unreadable file, and a file
// whose contents are not the JSON we expect. That collapse is deliberate — every
// caller's answer to all four is the same ("this session has no such record"),
// and a store that returned an error would push each of them into inventing a
// policy for a case that has exactly one sensible outcome.
//
// A MISSING file is the ordinary case (any session predating the sidecar, and
// every session that never acquired one) so it is silent. A file that exists but
// cannot be read or parsed is not ordinary, and is logged at warn — losing it
// quietly is how a corrupt record becomes a mystery months later.
//
// `label` names the record in those logs (e.g. "workspace binding"); it is the
// store's own name for itself, so the log line reads the way the store's caller
// thinks about the data rather than the way this file does.
func readSessionJSON[T any](baseDir, sid, name, label string) (T, bool) {
	var zero T
	// The sid on the reconnect path comes from the client, and this joins it into
	// a filesystem path. Without the guard a `..`-shaped id would read a sidecar
	// from somewhere unintended, and its contents would then be trusted as this
	// session's — an agent's cwd, in workspace.json's case. See ValidSessionID
	// (history.go) for why it is one shared allowlist rather than a local `..`
	// check.
	if !ValidSessionID(sid) {
		return zero, false
	}
	data, err := os.ReadFile(sessionFilePath(baseDir, sid, name))
	if err != nil {
		if !os.IsNotExist(err) {
			slog.Warn(label+": read failed", "sid", sid, "err", err)
		}
		return zero, false
	}
	var v T
	if err := json.Unmarshal(data, &v); err != nil {
		slog.Warn(label+": corrupt file, ignoring", "sid", sid, "err", err)
		return zero, false
	}
	return v, true
}

// writeSessionJSON records v at <baseDir>/<sid>/<name>, overwriting any previous
// record.
//
// Written whole via a UNIQUELY-NAMED temp file + rename, so no reader ever sees a
// partial record and two concurrent writers cannot interleave into a mixed one.
// A fixed `<path>.tmp` would allow exactly that, and two writes can genuinely land
// on one sid: for workspace.json, rekey re-records a binding without holding the
// spawn reservation (see Registry.spawnEntry); for wi.json, the writer is the
// browser, so two tabs are enough. A torn read costs more than a lost transcript
// line — it is a session pointed at the wrong directory, or labelled with half of
// one work item's id and half of another's. Sync before rename so that holds
// across a crash and not only across a concurrent read.
//
// Unlike readSessionJSON this DOES return its errors: a failed write means the
// record the caller believes it just made does not exist, and only the caller
// knows whether that is worth failing a request over (the HTTP handler behind
// wi.json answers 500) or worth a warning (saveBinding must not refuse to start a
// session over a metadata file).
func writeSessionJSON(baseDir, sid, name, label string, v any) error {
	if !ValidSessionID(sid) {
		return fmt.Errorf("%s: refusing to write under sid %q", label, sid)
	}
	path := sessionFilePath(baseDir, sid, name)
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("%s: mkdir: %w", label, err)
	}
	data, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("%s: marshal: %w", label, err)
	}
	f, err := os.CreateTemp(dir, "sidecar-*.json.tmp")
	if err != nil {
		return fmt.Errorf("%s: create temp: %w", label, err)
	}
	tmp := f.Name()
	// Remove the temp file on every failure path. A no-op once Rename succeeded,
	// because the name no longer exists.
	defer func() { _ = os.Remove(tmp) }()
	if err := f.Chmod(0o600); err != nil {
		_ = f.Close()
		return fmt.Errorf("%s: chmod temp: %w", label, err)
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return fmt.Errorf("%s: write: %w", label, err)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return fmt.Errorf("%s: sync: %w", label, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("%s: close temp: %w", label, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("%s: rename: %w", label, err)
	}
	return nil
}
