package server

import (
	"fmt"
	"io"
	"net/http"

	"github.com/quic-go/webtransport-go"
)

// buildMux constructs the shared route table used by both the TCP and UDP
// listeners. Routes per §10.B.4:
//
//	/             → SPA (embed.FS or dev proxy)
//	/cert-hash    → 64-char DER hash (wire.HashHex64)
//	/cert-hash-spki → 64-char SPKI hash
//	/api/v1/*     → 501 stubs (implemented s4+)
//	/wt/chat      → 501 stub (s4)
//	/wt/shell     → 501 stub (s6)
//	/wt/events    → 501 stub (s4)
//	/wt/_smoke    → WT bidi pure-byte echo (D-22 §6 #2 acceptance gate)
func buildMux(cfg *Config, bundle CertBundle, wts *webtransport.Server) *http.ServeMux {
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

	// §10.B.4 stubs — full implementation in s4/s5/s6.
	for _, path := range []string{
		"/api/v1/",
		"/wt/chat",
		"/wt/shell",
		"/wt/events",
	} {
		mux.HandleFunc(path, func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "not implemented", http.StatusNotImplemented)
		})
	}

	mux.Handle("/", newStaticHandler(cfg.DevFrontendURL))
	return mux
}
