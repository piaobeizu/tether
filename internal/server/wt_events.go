package server

import (
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/quic-go/webtransport-go"

	"github.com/piaobeizu/tether/internal/session"
	"github.com/piaobeizu/tether/internal/wire"
)

// handleWTEvents handles /wt/events — a unidirectional broadcast channel.
// Browser connects with ?sid=<sessionID> and receives all envelope events for
// that session. Supports multi-attach (D-08): multiple clients see same events.
func handleWTEvents(reg *session.Registry, wts *webtransport.Server) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		wtsess, err := wts.Upgrade(w, r)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		go serveEvents(r, wtsess, reg)
	}
}

func serveEvents(r *http.Request, wtsess *webtransport.Session, reg *session.Registry) {
	defer wtsess.CloseWithError(0, "")

	ccSID := r.URL.Query().Get("sid")
	if ccSID == "" {
		return
	}

	subCh := make(chan wire.Envelope, 64)
	reg.Subscribe(ccSID, subCh)
	defer reg.Unsubscribe(ccSID, subCh)

	for {
		select {
		case <-wtsess.Context().Done():
			return
		case env, ok := <-subCh:
			if !ok {
				return
			}
			env.SessionID = ccSID
			stream, err := wtsess.OpenUniStreamSync(wtsess.Context())
			if err != nil {
				return
			}
			b, _ := json.Marshal(env)
			fmt.Fprintf(stream, "%s\n", b)
			_ = stream.Close()
		}
	}
}
