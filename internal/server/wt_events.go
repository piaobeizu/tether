package server

import (
	"encoding/json"
	"fmt"
	"log/slog"
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
	// The subscription is against the SID, not against whichever *Entry that sid
	// resolves to right now (tether#75). It matters here more than anywhere else:
	// this connection is a passive watcher with no prompt of its own to fail, so
	// the only symptom of being left behind on a retired Entry is an absence — and
	// an absence is indistinguishable from a session that is merely quiet.
	//
	// retired closes when the sid is abandoned for one under a different id (a
	// resume that failed and fell back — see session.Registry.retireObservers).
	// Ending the stream is the whole of the signal: the transport closing is
	// something every client already has to handle, so the notice needs no new
	// wire vocabulary. web/src/lib/wt.ts's connectEventsOnly fires its onClose
	// callback on exactly that — though it is worth knowing, before reading this
	// as an end-to-end fix, that NOTHING in web/src calls connectEventsOnly
	// today: this route has no SPA consumer, and what the signal reaches is
	// whichever client connected to it.
	retired := reg.SubscribeObserver(sid, subCh)
	defer reg.UnsubscribeObserver(sid, subCh)

	pumpEvents(wtsess.Context().Done(), retired, subCh, sid, clientID, func(env wire.Envelope) error {
		stream, err := wtsess.OpenUniStreamSync(wtsess.Context())
		if err != nil {
			return err // client disconnected
		}
		b, _ := json.Marshal(env)
		if _, err := fmt.Fprintf(stream, "%s\n", b); err != nil {
			_ = stream.Close()
			return err // write failure = client gone
		}
		return stream.Close()
	})
}

// pumpEvents is the /wt/events read loop, split out from serveEvents so the one
// thing this route can get wrong is reachable from a test.
//
// The split exists for the retirement case specifically (tether#75). Everything
// else this loop does is observable from the client — an envelope arrives or it
// does not — but "the daemon told this observer its session is over and the
// observer kept waiting anyway" is a hop between two correct halves: the
// registry closing the signal, and this loop acting on it. Wiring hops are
// exactly where a fix that is right in both halves still ships broken, so the
// hop gets its own test rather than being covered only end to end.
//
// emit writes one envelope to the client and reports whether the client is
// still there; a non-nil error ends the stream, which is the pre-existing
// treatment of a failed uni-stream open or write (the client is gone, so there
// is nobody left to serve).
func pumpEvents(
	done <-chan struct{},
	retired <-chan struct{},
	subCh <-chan wire.Envelope,
	sid, clientID string,
	emit func(wire.Envelope) error,
) {
	for {
		select {
		case <-done:
			return
		case <-retired:
			// Info rather than Debug: nothing in this daemon calls slog.SetDefault,
			// so a Debug line here would be invisible on a real one — and this is the
			// only record that a read-only attach was ended by the DAEMON rather than
			// by its own client closing the tab.
			slog.Info("events: read-only attach ended because its session was replaced under a new id",
				"sid", sid, "client_id", clientID)
			return
		case env, ok := <-subCh:
			if !ok {
				return
			}
			// Stamp the watched sid onto envelopes that carry none — fanOut
			// builds most of them without one, and an observer that cannot tell
			// which session an envelope belongs to is the reason this field
			// exists. But only onto those: BroadcastAll's notices are
			// daemon-wide and name their own session (a permission request for
			// session Y, from mux.go), and overwriting that told an observer of
			// X that another session's tool prompt was X's. That overwrite was
			// unconditional before tether#75 and reached observers of live sids
			// only; widening the audience to observers of sids with no
			// registration would have widened the mislabelling with it.
			if env.SessionID == "" {
				env.SessionID = sid
			}
			if err := emit(env); err != nil {
				return
			}
		}
	}
}
