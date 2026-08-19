package server

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"strconv"
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
			// tether#106 — the change signal, on both methods.
			//
			// On the GET it tells the reader WHICH version of the transcript it is
			// now holding, so the first probe afterwards compares against a real
			// baseline instead of having to establish one (and miss whatever landed
			// in between). On the HEAD it is the whole answer.
			//
			// It is read BEFORE the transcript below, and that order is
			// load-bearing rather than incidental. Stat-then-read makes the
			// header a LOWER BOUND on the body's age: a write landing in between
			// produces a body newer than the version beside it, and the reader
			// pays one redundant reload on its next probe. Read-then-stat would
			// produce the opposite — a version newer than the body — and the
			// reader would record it as "this is what I have" and never ask for
			// that write again. One of those costs a request; the other loses
			// content silently.
			//
			// no-store because the point of the probe is that the answer changes;
			// a revalidating cache in front of it would report a version that is
			// as stale as the transcript the reader is complaining about. Same
			// reasoning handleSessionActivity states for the same header. It is
			// also why no Last-Modified is sent: with no-store there is no
			// conditional request for it to serve, so it would be a second copy
			// of this fact at one-second resolution, and the only thing a second
			// copy can do is be believed.
			w.Header().Set("Cache-Control", "no-store")
			if ts := idx.TranscriptUpdatedAt(sid); ts > 0 {
				w.Header().Set(session.TranscriptUpdatedAtHeader, strconv.FormatInt(ts, 10))
			}
			if r.Method == http.MethodHead {
				// Returning HERE is the entire cost argument. Falling through
				// would call idx.Messages — the unbounded os.ReadFile of
				// history.jsonl — and net/http would then throw the body away,
				// i.e. the probe would cost exactly what the fetch it exists to
				// avoid costs, and nothing would show it.
				//
				// Content-Type describes what a GET would return; there is
				// deliberately no Content-Length matching the GET's body, because
				// producing one means producing the body. RFC 9110 makes that
				// field's presence optional for exactly this reason, and nothing
				// on either side of this route reads it.
				//
				// It also has to return before the Info line below: a browser
				// reading a held session probes this route every three seconds,
				// and logging that would be 1,200 lines an hour whose content is
				// "still nothing".
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusOK)
				return
			}
			// tether#107 — the ONE query parameter this route reads.
			//
			// Refused rather than ignored when it is malformed. An ignored cursor
			// serves the newest page instead, which looks to the reader exactly like
			// "there is nothing earlier" — i.e. pagination silently reverting to the
			// ceiling it exists to remove. A 400 is a thing the caller can see.
			//
			// Read AFTER the HEAD return above, because the probe has no use for it:
			// HEAD answers from a stat and never selects a page, so validating it there
			// would only add a way for the probe to fail.
			before, ok := parseTranscriptBefore(r.URL.Query().Get("before"))
			if !ok {
				http.Error(w, "bad before", http.StatusBadRequest)
				return
			}
			// One call, because "which store answers for this sid" is one rule and
			// it lives on the index next to the list that applies the same rule —
			// see session.SessionIndex.MessagePage. This route deliberately does not
			// reach for HistoryStore and then fall back itself: that is how a row
			// labelled "tether" ends up serving cc's transcript. Since tether#107 the
			// same call also answers "how far back does this store go", for the same
			// reason: a cursor minted against one store's file must not be spendable
			// against the other's.
			page := idx.MessagePage(sid, before)
			msgs, source := page.Messages, page.Source
			if msgs == nil {
				msgs = []session.HistoryMessage{}
			}
			// Set before writeJSON, which writes the body. Present only when there IS
			// an earlier page: the header's ABSENCE is what tells the reader it has
			// reached the beginning of this store's record, so emitting a zero would
			// turn the one unambiguous signal into a value to be interpreted.
			if page.HasEarlier {
				w.Header().Set(session.TranscriptEarlierHeader, strconv.FormatInt(page.Earlier, 10))
			}
			if page.OtherRecord != "" {
				w.Header().Set(session.TranscriptOtherRecordHeader, page.OtherRecord)
			}
			// tether#92 — this route logged NOTHING, and the daemon was blind to
			// the exact path a user's bug report was about ("clicking a session
			// does nothing"), so the investigation had to run on indirect evidence.
			// One line, at Info: the sid asked for, how many turns came back, and
			// which store they came from. `source` distinguishes the three answers
			// that look identical from outside — tether had it, cc had it, nobody
			// had it — which is the whole reason 0 messages is not enough to log.
			slog.Info("session transcript served", "sid", sid, "count", len(msgs), "source", source,
				"before", before, "earlier", page.Earlier, "has_earlier", page.HasEarlier)
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

// parseTranscriptBefore reads the transcript route's `before` cursor (tether#107).
//
// Absent is session.TranscriptTail — "the newest page" — and NOT 0. Those are
// different requests: 0 asks for the page ending at byte zero, which is empty.
//
// A negative number is refused rather than clamped. The only cursor a client should
// ever hold came out of TranscriptEarlierHeader, which is emitted only when it is
// positive, so a negative one means the caller computed it, and a caller computing
// cursors is the case worth failing loudly for.
func parseTranscriptBefore(raw string) (int64, bool) {
	if raw == "" {
		return session.TranscriptTail, true
	}
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || n < 0 {
		return 0, false
	}
	return n, true
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
