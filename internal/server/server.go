package server

import (
	"crypto/tls"
	"net/http"
	"time"

	"github.com/mholt/acmez/v3"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/quic-go/quic-go"
	"github.com/quic-go/quic-go/http3"
	"github.com/quic-go/webtransport-go"

	"github.com/piaobeizu/tether/internal/auth"
	"github.com/piaobeizu/tether/internal/auth/apitoken"
	"github.com/piaobeizu/tether/internal/auth/oauth"
	"github.com/piaobeizu/tether/internal/permission"
)

// Server holds the TCP and UDP listeners plus the WebTransport server.
type Server struct {
	tcp *http.Server
	wts *webtransport.Server
	h3  *http3.Server
}

// newServer constructs (but does not start) the dual-listener server.
// Call Start() to bind and serve.
func newServer(cfg *Config, certs *certHolder, pm *permission.Manager, authState *auth.State, mcpSrv *mcp.Server, mcpTokens *apitoken.Store, oauthH *oauth.Handlers) *Server {
	addr := cfg.addr()

	// When ACME is active, certmagic provides a tls.Config with GetCertificate
	// already wired for auto-renewal. Clone it and set ALPN per listener.
	// Otherwise serve the managed cert through the holder.
	//
	// solvesACMEChallenge appends certmagic's TLS-ALPN-01 protocol to this
	// listener's ALPN list. Getting this wrong does not break the first
	// issuance — it breaks every RENEWAL, silently, sixty days later
	// (tether#79):
	//
	//   - First issuance runs in lifecycle.go Step 4b, BEFORE any listener
	//     binds. certmagic's tlsALPNSolver binds :443 itself (certmagic
	//     solvers.go, tlsALPNSolver.Present -> robustTryListen), answers the
	//     challenge on its own listener, and releases the port. Nothing here
	//     participates, which is exactly why a broken ALPN list still starts
	//     up clean and looks healthy.
	//   - Renewal runs ~30 days before expiry, in the background, while this
	//     process is already serving. If tether is the one holding :443 — the
	//     production deployment, though not the 8898 default; see
	//     warnACMEPortMismatch — the solver's own bind fails with EADDRINUSE,
	//     and robustTryListen then *dials* the port; because the dial succeeds
	//     it assumes whoever holds the socket can answer, and returns
	//     (nil, nil) — no error, no listener. The challenge is now ours to
	//     answer, and we can only answer it if this ALPN list offers
	//     acme-tls/1: certmagic's GetCertificate serves the challenge cert only
	//     when the ClientHello carries an SNI and its SupportedProtos is
	//     exactly [acme-tls/1] (certmagic handshake.go). Without it the
	//     handshake dies at ALPN negotiation, the CA reports `tls: no
	//     application protocol`, and renewal fails on every attempt until the
	//     cert expires at day 90.
	//
	// certmagic's own docs state the requirement on Config.TLSConfig: "you
	// will likely need to customize the NextProtos field by prepending your
	// application's protocols... Be sure to leave the acmez.ACMETLS1Protocol
	// value intact, however, or TLS-ALPN challenges will fail."
	//
	// Only the TCP listener gets it. TLS-ALPN-01 is defined over TCP (RFC
	// 8737); a CA never validates over QUIC, so advertising it on h3 would be
	// dead configuration that reads as though UDP were a second solving path.
	//
	// Position is not load-bearing. Go selects the first *server* protocol the
	// client also offers (crypto/tls negotiateALPN), and RFC 8737 requires the
	// CA to offer acme-tls/1 and nothing else, so it cannot outrank h2 for a
	// browser at any position. Appended rather than prepended only so that the
	// pre-fix list stays a prefix of this one, which keeps the negotiated
	// protocol provably unchanged for every client that is not a CA.
	//
	// Nor is this list quite what goes on the wire: net/http rewrites it per
	// listener (adjustNextProtos in net/http/server.go — it drops "h2" or
	// "http/1.1" when that protocol is disabled on the Server and appends
	// either back when missing). Protocols it does not recognise, acme-tls/1
	// among them, are left untouched and in place; that is the property this
	// fix rests on, and TestServeTLS_ACMEListenerNegotiatesTheChallengeProtocol
	// exercises the real ServeTLS path rather than trusting it.
	makeTLS := func(protos []string, solvesACMEChallenge bool) *tls.Config {
		if cfg.acmeTLSBase == nil {
			return certTLSConfig(certs, protos)
		}
		c := cfg.acmeTLSBase.Clone()
		// Copy rather than alias: appending to the caller's literal would be
		// safe today only because its cap equals its len.
		c.NextProtos = append([]string(nil), protos...)
		if solvesACMEChallenge {
			c.NextProtos = append(c.NextProtos, acmez.ACMETLS1Protocol)
		}
		return c
	}

	// TCP: HTTP/2 + HTTP/1.1 over TLS.
	// ALPN must be ["h2","http/1.1"] — separate from the UDP h3 TLS config.
	tcpTLS := makeTLS([]string{"h2", "http/1.1"}, true)

	// UDP: HTTP/3 + WebTransport.
	// Both EnableDatagrams flags are required (§10.B.2 #3).
	h3 := &http3.Server{
		Addr:      addr,
		TLSConfig: makeTLS([]string{"h3"}, false),
		EnableDatagrams: true,
		QUICConfig: &quic.Config{
			EnableDatagrams:                  true,
			EnableStreamResetPartialDelivery: true,
			// Keep NAT entry alive across user typing pauses. Without this,
			// home-router/carrier NAT (typical UDP timeout 30s) silently drops
			// the binding and the next stream write fails with "Connection lost".
			KeepAlivePeriod: 15 * time.Second,
			MaxIdleTimeout:  90 * time.Second,
		},
	}
	// MANDATORY: without this the browser WT handshake fails (§10.B.2 #1).
	webtransport.ConfigureHTTP3Server(h3)

	wts := &webtransport.Server{
		H3:          h3,
		CheckOrigin: func(r *http.Request) bool { return originAllowed(r.Header.Get("Origin"), cfg.Port) },
	}

	mux := buildMux(cfg, certs, wts, cfg.Registry, pm, authState, mcpSrv, mcpTokens, oauthH, cfg.MCPLifecycle)

	tcpServer := &http.Server{
		Addr:      addr,
		Handler:   altSvcMiddleware(addr, mux),
		TLSConfig: tcpTLS,
	}
	h3.Handler = mux

	return &Server{tcp: tcpServer, wts: wts, h3: h3}
}

// altSvcMiddleware injects Alt-Svc: h3=":<port>"; ma=86400 on every TCP
// response so the browser learns to upgrade to HTTP/3 (§10.B.1).
func altSvcMiddleware(addr string, h http.Handler) http.Handler {
	altSvc := `h3="` + addr + `"; ma=86400`
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Alt-Svc", altSvc)
		h.ServeHTTP(w, r)
	})
}
