package server

import (
	"net/http"

	"github.com/piaobeizu/tether/internal/session"
)

// handleSessionActivity serves GET session.SessionActivityPath: one state per
// session something live is holding, keyed by sid (tether#103).
//
//	{"<sid>":"working", "<sid>":"idle", "<sid>":"held"}
//
// A sid nothing holds is ABSENT rather than present-and-idle — see
// session.ActivityIndex.States for why absence is a different statement from
// `idle`, and session.SessionActivityPath for why this is a route of its own
// instead of a field on GET /api/v1/sessions.
//
// # Why the handler is this thin
//
// Everything it could usefully add is a decision about what to tell a human, and
// those live in internal/session next to the two readers that supply the facts.
// This function's whole job is the HTTP shape, which is the part the routing test
// can see.
//
// # no-store, and it is not decoration
//
// The whole point of the endpoint is that the answer changes. A cached response —
// from a proxy, or from the browser's own heuristics on a GET with no cache
// headers — would produce exactly the failure this slice exists to prevent: a
// marker that keeps reporting a turn that finished minutes ago, refreshed
// diligently every three seconds from a copy. Same reasoning handleCertHash
// states for the same header.
func handleSessionActivity(idx *session.ActivityIndex) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Enforced rather than ignored, so this route agrees with listSessions —
		// which had to be fixed for the same thing in tether#91, having answered a
		// DELETE with 200 and the whole list.
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		// States never returns nil, which is load-bearing on this route: a nil map
		// marshals to `null`, and the SPA indexes the result by sid.
		writeJSON(w, idx.States())
	}
}
