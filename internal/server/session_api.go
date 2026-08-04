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
// avoids — see ValidSessionID's doc.
func validSID(sid string) bool { return session.ValidSessionID(sid) }

// sessionAPIHandlers returns HTTP handlers for session history.
func sessionAPIHandlers(history *session.HistoryStore) (listSessions, getMessages http.HandlerFunc) {
	listSessions = func(w http.ResponseWriter, r *http.Request) {
		sids := history.ListSessions()
		if sids == nil {
			sids = []string{}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(sids)
	}

	getMessages = func(w http.ResponseWriter, r *http.Request) {
		// Path: /api/v1/sessions/<sid>/messages
		parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
		// parts = ["api","v1","sessions","<sid>","messages"]
		if len(parts) < 5 {
			http.Error(w, "bad path", http.StatusBadRequest)
			return
		}
		sid := parts[3]
		if !validSID(sid) {
			http.Error(w, "invalid sid", http.StatusBadRequest)
			return
		}
		msgs := history.LoadHistory(sid)
		if msgs == nil {
			msgs = []session.HistoryMessage{}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(msgs)
	}

	return
}
