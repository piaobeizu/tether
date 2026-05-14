package server

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/piaobeizu/tether/internal/session"
)

// validSID accepts only the alphabet that real cc / opencode session IDs use
// (UUID hex + dashes, or `ses_` / `t-` prefixes with [A-Za-z0-9]). Anything
// else — `..`, slashes, control chars, URL-encoded escapes — is rejected so
// that history.historyPath() can't escape its baseDir.
func validSID(sid string) bool {
	if len(sid) < 8 || len(sid) > 128 {
		return false
	}
	for _, c := range sid {
		switch {
		case c >= 'a' && c <= 'z':
		case c >= 'A' && c <= 'Z':
		case c >= '0' && c <= '9':
		case c == '-' || c == '_':
		default:
			return false
		}
	}
	return true
}

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
