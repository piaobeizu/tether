package server

import (
	"fmt"
	"io"
	"net/http"

	"github.com/quic-go/webtransport-go"

	"github.com/piaobeizu/tether/internal/session"
)

// buildMux constructs the shared route table used by both the TCP and UDP
// listeners. Routes per §10.B.4:
//
//	/               → SPA (embed.FS or dev proxy)
//	/cert-hash      → 64-char DER hash (wire.HashHex64)
//	/cert-hash-spki → 64-char SPKI hash
//	/api/v1/*       → REST API (stubs for s5+)
//	/wt/chat        → stream-json chat channel (s4)
//	/wt/shell       → PTY shell channel stub (s6)
//	/wt/events      → broadcast events channel (s4)
//	/wt/_smoke      → WT bidi pure-byte echo (D-22 §6 #2 acceptance gate)
func buildMux(cfg *Config, bundle CertBundle, wts *webtransport.Server, reg *session.Registry, ps *PermState) *http.ServeMux {
	mux := http.NewServeMux()

	derHex := HashHex(bundle.DER)
	spkiHex := HashHex(bundle.SPKI)

	mux.HandleFunc("/cert-hash", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		fmt.Fprintln(w, derHex)
	})
	mux.HandleFunc("/cert-hash-spki", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		fmt.Fprintln(w, spkiHex)
	})

	// WT smoke-test echo (D-22 §6 #2): pure byte echo, no prefix, no framing.
	mux.HandleFunc("/wt/_smoke", func(w http.ResponseWriter, r *http.Request) {
		sess, err := wts.Upgrade(w, r)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		go func() {
			defer sess.CloseWithError(0, "")
			stream, err := sess.AcceptStream(sess.Context())
			if err != nil {
				return
			}
			defer stream.Close()
			_, _ = io.Copy(stream, stream)
		}()
	})

	// s4: chat + events WT channels.
	mux.HandleFunc("/wt/chat", handleWTChat(reg, wts))
	mux.HandleFunc("/wt/events", handleWTEvents(reg, wts))

	// s5: permission API.
	registerPermAPI(mux, ps, reg)

	// s6: shell WT channel + session lock API.
	mux.HandleFunc("/wt/shell", handleWTShell(reg, wts))
	mux.HandleFunc("/api/v1/session/", handleLockForce(reg))

	mux.HandleFunc("/api/v1/", func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "not implemented", http.StatusNotImplemented)
	})

	mux.Handle("/", newStaticHandler(cfg.DevFrontendURL))
	return mux
}
