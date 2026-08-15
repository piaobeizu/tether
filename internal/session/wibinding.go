package session

// Per-session work-item binding (tether#91).
//
// # The direction is the design
//
// The browser used to remember which session belonged to which work item in
// localStorage, keyed `tether_wi_sid:<slug>` — one entry per wi, holding one sid.
// That is the FORWARD direction, and three of its four problems come straight
// from the direction rather than from the storage:
//
//   - it can only answer "which session was this wi last worked in", so nothing
//     can look at a session and say what it is about — which is precisely the
//     question a session LIST has to answer to be readable;
//   - one wi maps to one session, and starting work again overwrites the
//     previous answer;
//   - the daemon, which is the only party that survives a browser profile, knew
//     nothing about any of it.
//
// Storing session -> wi instead answers both questions with one record: a
// session's wi is a file read, and a wi's sessions are every file that names it.
// One-to-many is free, and there is no index to keep consistent.
//
// # What the daemon does and does not know
//
// WorkItem is an OPAQUE label. The daemon never parses it, never resolves it,
// and never asks aihub about it — it is written by whoever is driving the UI and
// handed back unchanged. tether does have an aihub client and a
// /api/v1/work/* proxy, and deliberately none of it is reachable from here: a
// binding must keep working when aihub is unreachable, unconfigured, or replaced,
// because its whole job is to still be true after a restart.
//
// # Why it is on disk
//
// Same argument BindingStore makes about workspace.json, one step further out.
// The workspace binding has to survive a DAEMON restart; this has to survive that
// plus a browser profile — a different device, a different browser, a cleared
// cache. An in-memory map (or localStorage) makes the answer a property of the
// client that happened to create it, and the answer is about the session.

import (
	"fmt"
	"time"
	"unicode/utf8"
)

// wi.json's sidecar name, and how this store names itself in log lines/errors.
const (
	wiBindingFile  = "wi.json"
	wiBindingLabel = "wi binding"
)

// MaxWorkItemLen bounds the stored label, in RUNES.
//
// Not a syntax check: the daemon has no opinion on what a work item id looks like
// (polyforge slugs, Jira keys and GitHub refs all pass through here). It is a
// bound on a file whose writer is a browser request, plus the reason the HTTP
// layer can reject junk early instead of discovering it at render time.
//
// Runes rather than bytes so the limit means the same thing for every alphabet —
// a 200-character CJK label is 600 bytes and would fail a byte cap for no reason
// anyone could act on. The same choice as maxTitleLen in sessionlist.go, which
// cuts by rune for the same reason.
const MaxWorkItemLen = 200

// WIBinding is the work item a session is working on.
//
// BoundAt is recorded for the operator, not for the UI: it distinguishes "this
// session was started for that wi" from "somebody labelled it afterwards" when
// the two disagree with the transcript. Nothing reads it today, which is stated
// here so the next person does not go looking for the consumer.
type WIBinding struct {
	WorkItem string `json:"workItem"`
	BoundAt  int64  `json:"boundAt"` // Unix milliseconds
}

// WIBindingStore persists WIBindings at <baseDir>/<sid>/wi.json — the same
// per-session directory HistoryStore and BindingStore already own.
//
// A separate type from BindingStore rather than a second field on
// WorkspaceBinding, because they are written by different parties at different
// times and mean different things: the daemon writes the workspace at spawn and
// never changes it (a session's directory is part of its identity), while this is
// written by the UI whenever a human says "this session is for that wi" and may
// be rewritten. Folding them together would also make "no workspace registry"
// and "no wi binding" the same nil, and BindingStore's doc explains why that
// nil-ness has to stay separately constructible in a test.
//
// The file format contract — sid validation, atomic whole-file write, missing and
// corrupt files reading as absent — is shared with workspace.json; see
// sessionfile.go.
type WIBindingStore struct {
	baseDir string
}

// NewWIBindingStore returns a store rooted at baseDir (~/.tether/sessions).
func NewWIBindingStore(baseDir string) *WIBindingStore {
	return &WIBindingStore{baseDir: baseDir}
}

// Load returns the work item recorded for sid, and whether there was one.
//
// A missing file is the ordinary case — every session that predates tether#91,
// and every session nobody has associated with a work item — so it is silent.
//
// An empty WorkItem reads as ABSENT even though the file exists. That is the
// same rule BindingStore applies to a path-less binding, and here it also closes
// the bug this store replaces: the old localStorage writer stored
// the current session id or the empty string, so "no session yet" and "this
// session" were the same recorded value. A record that names no work item is not
// a record.
func (s *WIBindingStore) Load(sid string) (WIBinding, bool) {
	b, ok := readSessionJSON[WIBinding](s.baseDir, sid, wiBindingFile, wiBindingLabel)
	if !ok || b.WorkItem == "" {
		return WIBinding{}, false
	}
	return b, true
}

// Save records workItem as sid's work item, overwriting any previous record.
//
// It refuses an empty label rather than writing one, so Load's "empty means
// absent" rule can never be reached through this path — only through a file
// somebody hand-edited or a pre-existing one. Callers that want to know a
// session's wi is gone should delete the file; nothing needs that yet, so no
// Delete exists.
//
// BoundAt is stamped here rather than taken from the caller: it is a fact about
// when the daemon wrote the file, and a client-supplied timestamp would be a
// claim about the client's clock.
func (s *WIBindingStore) Save(sid, workItem string) error {
	if !ValidWorkItem(workItem) {
		return fmt.Errorf("%s: refusing to record work item %q", wiBindingLabel, workItem)
	}
	return writeSessionJSON(s.baseDir, sid, wiBindingFile, wiBindingLabel, WIBinding{
		WorkItem: workItem,
		BoundAt:  time.Now().UnixMilli(),
	})
}

// ValidWorkItem reports whether label may be stored as a work item id.
//
// One line of policy, in one place, because both the HTTP handler (which wants to
// answer 400) and the store (which must not write junk) need the same answer.
// It accepts anything printable and single-line within MaxWorkItemLen: the daemon
// is not the authority on work-item syntax, it is only the thing that has to keep
// the value renderable and the file bounded. Newlines and control characters are
// rejected because a label is a label — the moment one can contain a newline it
// can also impersonate a log line.
//
// "Single line" is enforced for Unicode, not just for ASCII: C0 and DEL are the
// obvious half, but U+2028 LINE SEPARATOR and U+2029 PARAGRAPH SEPARATOR break a
// line in a browser too, and the C1 block is a control range that no work-item id
// has any business containing. Invalid UTF-8 is refused outright rather than
// silently becoming U+FFFD in the stored JSON.
func ValidWorkItem(label string) bool {
	if label == "" || !utf8.ValidString(label) {
		return false
	}
	if utf8.RuneCountInString(label) > MaxWorkItemLen {
		return false
	}
	for _, c := range label {
		switch {
		case c < 0x20, c == 0x7f: // C0 controls and DEL
			return false
		case c >= 0x80 && c <= 0x9f: // C1 controls
			return false
		case c == '\u2028' || c == '\u2029': // Unicode line/paragraph separators
			return false
		}
	}
	return true
}
