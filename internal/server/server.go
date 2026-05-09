package server

import (
	"crypto/tls"
	"net/http"

	"github.com/quic-go/quic-go"
	"github.com/quic-go/quic-go/http3"
	"github.com/quic-go/webtransport-go"
)

// Server holds the TCP and UDP listeners plus the WebTransport server.
type Server struct {
	tcp *http.Server
	wts *webtransport.Server
	h3  *http3.Server
}

// newServer constructs (but does not start) the dual-listener server.
// Call Start() to bind and serve.
func newServer(cfg *Config, bundle CertBundle) *Server {
	addr := cfg.addr()

	// TCP: HTTP/2 + HTTP/1.1 over TLS.
	// ALPN must be ["h2","http/1.1"] — separate from the UDP h3 TLS config.
	tcpTLS := &tls.Config{
		Certificates: []tls.Certificate{bundle.TLS},
		NextProtos:   []string{"h2", "http/1.1"},
	}

	// UDP: HTTP/3 + WebTransport.
	// Both EnableDatagrams flags are required (§10.B.2 #3).
	h3 := &http3.Server{
		Addr: addr,
		TLSConfig: &tls.Config{
			Certificates: []tls.Certificate{bundle.TLS},
			NextProtos:   []string{"h3"},
		},
		EnableDatagrams: true,
		QUICConfig: &quic.Config{
			EnableDatagrams:                  true,
			EnableStreamResetPartialDelivery: true,
		},
	}
	// MANDATORY: without this the browser WT handshake fails (§10.B.2 #1).
	webtransport.ConfigureHTTP3Server(h3)

	wts := &webtransport.Server{
		H3:          h3,
		CheckOrigin: func(*http.Request) bool { return true },
	}

	mux := buildMux(cfg, bundle, wts)

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
