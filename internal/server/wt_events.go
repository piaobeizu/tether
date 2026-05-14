package server

import (
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/quic-go/webtransport-go"

	"github.com/piaobeizu/tether/internal/auth"
	"github.com/piaobeizu/tether/internal/session"
	"github.com/piaobeizu/tether/internal/wire"
)

// handleWTEvents handles /wt/events — a unidirectional broadcast channel.
// Browser connects with ?sid=<sessionID> and receives all envelope events for
// that session. Supports multi-attach (D-08): multiple clients see same events.
func handleWTEvents(reg *session.Registry, wts *webtransport.Server, authState *auth.State) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		clientID := authState.ClientIDFromTicket(r)
		if clientID == "" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "unauthorized"})
			return
		}
		wtsess, err := wts.Upgrade(w, r)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		go serveEvents(r, wtsess, reg, clientID)
	}
}

func serveEvents(r *http.Request, wtsess *webtransport.Session, reg *session.Registry, clientID string) {
	defer wtsess.CloseWithError(0, "")

	sid := r.URL.Query().Get("sid")
	if sid == "" {
		return
	}

	// Owner must use /wt/chat, not /wt/events — silently close.
	if reg.IsOwner(sid, clientID) {
		return
	}

	subCh := make(chan wire.Envelope, 64)
	reg.Subscribe(sid, subCh)
	defer reg.Unsubscribe(sid, subCh)

	for {
		select {
		case <-wtsess.Context().Done():
			return
		case env, ok := <-subCh:
			if !ok {
				return
			}
			env.SessionID = sid
			stream, err := wtsess.OpenUniStreamSync(wtsess.Context())
			if err != nil {
				return // client disconnected
			}
			b, _ := json.Marshal(env)
			if _, err := fmt.Fprintf(stream, "%s\n", b); err != nil {
				_ = stream.Close()
				return // write failure = client gone
			}
			_ = stream.Close()
		}
	}
}
