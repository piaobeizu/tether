package server

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/piaobeizu/tether/internal/session"
)

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
		msgs := history.LoadHistory(sid)
		if msgs == nil {
			msgs = []session.HistoryMessage{}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(msgs)
	}

	return
}
