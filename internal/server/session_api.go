package server

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/piaobeizu/tether/internal/session"
)

// validSID is session.ValidSessionID, kept as a name this package's handler reads
// naturally. The implementation moved into internal/session in tether#52: the sid
// reaches a path through HistoryStore AND BindingStore, so the guard belongs with
// the types that build those paths rather than with one of the two HTTP routes
// that happens to feed them. Two copies with different contracts is what this
// avoids — see ValidSessionID's doc. tether#91 added a third path-building store
// (WIBindingStore) and a third route; all of them still come through here.
func validSID(sid string) bool { return session.ValidSessionID(sid) }

// maxWIBodyBytes caps the PUT .../wi request body. The payload is one short label
// in a JSON object; anything larger is a mistake or an attempt, and reading it to
// find out is the only cost worth avoiding.
const maxWIBodyBytes = 4 << 10

// sessionAPIHandlers returns the two HTTP handlers behind /api/v1/sessions.
//
// `list` serves the collection; `sub` serves everything under it, on the subtree
// pattern "/api/v1/sessions/" that mux.go already registered for the messages
// route before tether#91.
//
// # Why `sub` routes by hand
//
// Not because it has to. This module targets go 1.25 and mcp_tokens.go registers
// `DELETE /api/v1/mcp/tokens/{id}` a few files away, so
// `PUT /api/v1/sessions/{sid}/wi` is perfectly registrable and would give the
// 405 handling for free. Two reasons to keep the split explicit anyway:
//
//   - The sid is client-supplied and becomes a FILE PATH. Here, every request
//     under this subtree passes the same session.ValidSessionID before any leaf
//     is chosen, and any path that is not exactly five segments is refused
//     outright — one place, visible, independent of how the router happens to
//     treat percent-encoded separators and dot segments (ServeMux cleans and
//     redirects; that behaviour is fine but it is not this file's to reason
//     about).
//   - The messages route's parsing is byte-for-byte what it was before this
//     change, so "did the transcript endpoint move?" has the answer "no".
//
// wis may be nil (a daemon assembled without a wi store): the route then reports
// the feature as unavailable rather than pretending the write succeeded.
func sessionAPIHandlers(idx *session.SessionIndex, wis *session.WIBindingStore) (list, sub http.HandlerFunc) {
	list = func(w http.ResponseWriter, r *http.Request) {
		// Enforced rather than ignored, so the two handlers in this file agree:
		// before tether#91 this one answered a DELETE with 200 and the whole list.
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		// A SessionSummary per session, newest first — see session.SessionIndex.
		//
		// This route used to return a bare []string of sids, which is what let the
		// old list render `sid.slice(0,16)…` in an order derived from UUID
		// filenames. The shape changed in place rather than growing a second
		// endpoint: the daemon and the SPA it serves ship as ONE binary
		// (web/embed.go), so there is no version of this frontend that can be
		// talking to a version of this daemon that disagrees, and the only other
		// consumer of the old shape was deleted in the same change.
		rows := idx.List()
		if rows == nil {
			rows = []session.SessionSummary{}
		}
		writeJSON(w, rows)
	}

	sub = func(w http.ResponseWriter, r *http.Request) {
		// Path: /api/v1/sessions/<sid>/<leaf>
		parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
		// parts = ["api","v1","sessions","<sid>","<leaf>"]
		if len(parts) != 5 {
			http.Error(w, "bad path", http.StatusBadRequest)
			return
		}
		sid, leaf := parts[3], parts[4]
		if !validSID(sid) {
			http.Error(w, "invalid sid", http.StatusBadRequest)
			return
		}
		switch leaf {
		case "messages":
			if r.Method != http.MethodGet && r.Method != http.MethodHead {
				http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
				return
			}
			msgs := idx.History.LoadHistory(sid)
			if msgs == nil {
				msgs = []session.HistoryMessage{}
			}
			writeJSON(w, msgs)
		case "wi":
			if r.Method != http.MethodPut {
				http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
				return
			}
			putSessionWI(w, r, wis, sid)
		default:
			http.NotFound(w, r)
		}
	}

	return
}

// putSessionWI records which work item a session belongs to.
//
// The daemon stores the label and does not interpret it: it never resolves it,
// never asks aihub about it (tether has a client and a proxy for that, and this
// route deliberately reaches neither), and has no opinion on its syntax beyond
// session.ValidWorkItem's "a single printable line, bounded". The browser is the
// party that knows what a work item is; the daemon is the party that is still
// here after the browser's storage is cleared.
//
// The direction is session -> wi, which is what makes one wi's sessions readable
// off the disk with no index: they are every session whose wi.json names it.
//
// # It does NOT require the session to exist, and that is deliberate
//
// The write creates <sid>/ if it is not there, so a well-formed sid that names
// nothing still produces a directory. The obvious gate — refuse unless the
// session has a transcript — was considered and rejected, because the MAIN path
// would fail it: the browser binds at the moment the user says "work on this",
// which is normally before the session has said anything, and a session that
// selected no workspace has no directory yet either. A gate there would drop
// exactly the bindings this feature exists to record, silently.
//
// What that leaves is an authenticated client able to make empty directories
// under ~/.tether/sessions. Bounded, and small next to what the same credential
// already opens (this daemon offers a PTY): the directories hold one 45-byte
// file, and SessionIndex.List ignores any session with no transcript, so they
// never reach the UI. The asymmetry with the list — it refuses to SHOW a
// transcript-less session while this route will CREATE one — is the same rule
// read from two ends: a conversation is what makes a session listable, a
// binding is a note about a session that may not have started talking yet.
func putSessionWI(w http.ResponseWriter, r *http.Request, wis *session.WIBindingStore, sid string) {
	if wis == nil {
		http.Error(w, "session work-item bindings unavailable", http.StatusServiceUnavailable)
		return
	}
	var body struct {
		WorkItem string `json:"workItem"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxWIBodyBytes)).Decode(&body); err != nil {
		http.Error(w, "bad body", http.StatusBadRequest)
		return
	}
	// Checked here as well as in the store, so a malformed label is a 400 the
	// caller can act on rather than a 500 from a refused write. The store keeps
	// its own copy of the check not because it has another caller today — it does
	// not — but because it is the thing that writes the file: a rule enforced only
	// at the edge is a rule the next entry point does not inherit.
	if !session.ValidWorkItem(body.WorkItem) {
		http.Error(w, "invalid workItem", http.StatusBadRequest)
		return
	}
	if err := wis.Save(sid, body.WorkItem); err != nil {
		http.Error(w, "could not record work item", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
