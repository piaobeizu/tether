package server

import (
	"crypto/tls"
	"net/http"
	"time"

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
func newServer(cfg *Config, bundle CertBundle, pm *permission.Manager, authState *auth.State, mcpSrv *mcp.Server, mcpTokens *apitoken.Store, oauthH *oauth.Handlers) *Server {
	addr := cfg.addr()

	// When ACME is active, certmagic provides a tls.Config with GetCertificate
	// already wired for auto-renewal. Clone it and override ALPN per listener.
	// Otherwise fall back to the static cert from CertBundle.
	makeTLS := func(protos []string) *tls.Config {
		if cfg.acmeTLSBase != nil {
			c := cfg.acmeTLSBase.Clone()
			c.NextProtos = protos
			return c
		}
		return &tls.Config{
			Certificates: []tls.Certificate{bundle.TLS},
			NextProtos:   protos,
		}
	}

	// TCP: HTTP/2 + HTTP/1.1 over TLS.
	// ALPN must be ["h2","http/1.1"] — separate from the UDP h3 TLS config.
	tcpTLS := makeTLS([]string{"h2", "http/1.1"})

	// UDP: HTTP/3 + WebTransport.
	// Both EnableDatagrams flags are required (§10.B.2 #3).
	h3 := &http3.Server{
		Addr:      addr,
		TLSConfig: makeTLS([]string{"h3"}),
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

	mux := buildMux(cfg, bundle, wts, cfg.Registry, pm, authState, mcpSrv, mcpTokens, oauthH)

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
