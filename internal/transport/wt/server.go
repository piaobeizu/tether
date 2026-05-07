// Package wt is the WebTransport-over-HTTP/3 server skeleton for the
// tether daemon's cross-device transport (spec §3.3 / §11.V / D-13 /
// D-21).
//
// Status — slice #1 (server skeleton):
//
//   - Boots a webtransport-go server on a configurable UDP port.
//   - Accepts WT sessions at a single endpoint path ("/wt").
//   - Per session, opens ONE control bidi stream that JSON-echoes
//     incoming envelopes back to the client (smoke surface for the v0.1
//     wire).
//   - Generates a self-signed dev cert when no cert is configured;
//     production paths supply Cert/Key and skip the dev cert path.
//
// What this package does NOT do yet (later slices):
//
//   - 5-channel multiplex (control / events / agent-bytes / catch-up /
//     datagram) per §3.3.3 — slice #2 will layer the full channel set
//     on top of this skeleton.
//   - §3.3.1 wire envelope dispatch (encrypted payload, AD-bound kind,
//     replay/dedup) — slice #3.
//   - Pairing / device auth (§ pairing / X25519 ECDH handshake from
//     internal/crypto) — slice #4.
//   - Wiring as a watchdog-supervised subsystem in
//     internal/daemon.daemon.Run() — deliberate: the WT block has to
//     stand alone before it earns a daemon supervisor slot, and the
//     existing UDS attach socket (internal/agent.attach_socket.go) is
//     the v0.1 in-process surface for now (D-21 says desktop should
//     not use UDS, but `tether attach` on the daemon-host shell still
//     does, so we keep that path untouched).
//
// API surface is intentionally minimal:
//
//	srv, err := wt.New(ctx, wt.Config{Addr: ":4444"})
//	go srv.Serve(ctx)  // blocks until ctx cancels or listener errors
//	...
//	srv.Close()        // idempotent shutdown
package wt

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"sync"
	"sync/atomic"

	"github.com/quic-go/quic-go"
	"github.com/quic-go/quic-go/http3"
	"github.com/quic-go/webtransport-go"
)

// DefaultAddr is the default UDP bind address for the WT listener.
// 4444 mirrors the spec's PoC-2 (4433) without colliding with
// developers who keep PoC-2 running locally; production deployments
// override via Config.Addr.
const DefaultAddr = ":4444"

// EndpointPath is the HTTP/3 path the WebTransport upgrade handler
// mounts at. Single endpoint per server — channel multiplexing is
// stream-level inside one session, not separate URL endpoints.
const EndpointPath = "/wt"

// alpnH3 is the only ALPN protocol the WT server advertises.
// HTTP/3 mandates exactly "h3" — no fallback.
const alpnH3 = "h3"

// Config bundles the server's runtime knobs. Zero-valued Config + a
// non-nil Logger gives a usable dev server on DefaultAddr.
type Config struct {
	// Addr is the UDP bind address (host:port). Empty = DefaultAddr.
	Addr string

	// Cert / Key are PEM-encoded TLS material. If both are empty, the
	// server auto-generates a self-signed ECDSA P-256 dev cert at boot.
	// Production deployments MUST supply both.
	Cert []byte
	Key  []byte

	// DevCertExtraIPs / DevCertExtraDNS append SANs to the auto-generated
	// dev cert (only consulted when Cert+Key are empty).
	DevCertExtraIPs []net.IP
	DevCertExtraDNS []string

	// Logger receives operational events. nil = log to a discard logger.
	Logger *log.Logger

	// CheckOrigin controls the WT upgrade Origin check. nil = allow any
	// origin (dev default). Production should pin to expected client
	// origins (the desktop / mobile app's bundle origin).
	CheckOrigin func(*http.Request) bool
}

// Server is the WT listener. Exactly one Serve call per Server; reuse
// after Close is not supported.
type Server struct {
	addr    string
	logger  *log.Logger
	cert    tls.Certificate
	devCert *devCert // nil when caller supplied real cert

	h3 *http3.Server
	wt *webtransport.Server

	// sessions tracked for graceful shutdown.
	mu       sync.Mutex
	sessions map[*Session]struct{}

	closeOnce sync.Once
	closed    atomic.Bool
}

// New wires a WT server with the given Config. It does NOT start the
// listener — call Serve. The provided ctx is currently used only for
// future hooks (cert reload, etc.) and not retained.
func New(_ context.Context, cfg Config) (*Server, error) {
	addr := cfg.Addr
	if addr == "" {
		addr = DefaultAddr
	}

	logger := cfg.Logger
	if logger == nil {
		logger = log.New(io.Discard, "", 0)
	}

	srv := &Server{
		addr:     addr,
		logger:   logger,
		sessions: make(map[*Session]struct{}),
	}

	// Cert: caller-supplied wins; otherwise auto-gen dev cert.
	switch {
	case len(cfg.Cert) > 0 && len(cfg.Key) > 0:
		c, err := tls.X509KeyPair(cfg.Cert, cfg.Key)
		if err != nil {
			return nil, fmt.Errorf("wt: X509KeyPair: %w", err)
		}
		srv.cert = c
	case len(cfg.Cert) == 0 && len(cfg.Key) == 0:
		dc, err := generateDevCert(devCertOptions{
			ExtraIPs: cfg.DevCertExtraIPs,
			ExtraDNS: cfg.DevCertExtraDNS,
		})
		if err != nil {
			return nil, fmt.Errorf("wt: generate dev cert: %w", err)
		}
		srv.cert = dc.TLS
		srv.devCert = &dc
		logger.Printf("wt: dev cert auto-generated (SPKI sha256=%x)", dc.SPKISHA256)
	default:
		return nil, errors.New("wt: Cert and Key must both be set or both be empty")
	}

	mux := http.NewServeMux()
	srv.h3 = &http3.Server{
		Addr: addr,
		TLSConfig: &tls.Config{
			Certificates: []tls.Certificate{srv.cert},
			NextProtos:   []string{alpnH3},
			MinVersion:   tls.VersionTLS13, // HTTP/3 floor
		},
		Handler:         mux,
		EnableDatagrams: true, // §3.3.3 "datagram" channel for health.*
		QUICConfig: &quic.Config{
			EnableDatagrams: true,
			// Required by webtransport-go ≥ v0.10 — without this the
			// WT layer's stream-reset path returns errors instead of
			// graceful trailers. See PoC-2 step5 notes.
			EnableStreamResetPartialDelivery: true,
		},
	}
	// CRITICAL — ConfigureHTTP3Server flips the H3 SETTINGS frame to
	// announce SETTINGS_ENABLE_WEBTRANSPORT=1 and turns on datagrams.
	// webtransport.Server does NOT do this implicitly; missing this
	// causes browser/client upgrades to fail with "WebTransport not
	// enabled". Discovered the hard way in PoC-2 step1.
	webtransport.ConfigureHTTP3Server(srv.h3)

	checkOrigin := cfg.CheckOrigin
	if checkOrigin == nil {
		checkOrigin = func(*http.Request) bool { return true }
	}
	srv.wt = &webtransport.Server{
		H3:          srv.h3,
		CheckOrigin: checkOrigin,
	}

	mux.HandleFunc(EndpointPath, srv.handleUpgrade)

	return srv, nil
}

// Addr returns the configured bind address (post-default resolution).
// Useful for tests that pick port 0 and then need to know the actual
// listening port — but note that webtransport.Server doesn't expose
// the post-bind addr; tests use the ServeListener path below.
func (s *Server) Addr() string { return s.addr }

// DevCertSPKISHA256 returns the SPKI fingerprint of the auto-generated
// dev cert, or zero value if the server was constructed with operator-
// supplied certs. Useful for tests + future browser handoff via
// `serverCertificateHashes`.
func (s *Server) DevCertSPKISHA256() [32]byte {
	if s.devCert == nil {
		return [32]byte{}
	}
	return s.devCert.SPKISHA256
}

// TLSCertificate returns a copy of the server's TLS certificate.
// Useful for tests that need to set up a client trust pool.
func (s *Server) TLSCertificate() tls.Certificate { return s.cert }

// Serve blocks running the WT listener until ctx is cancelled or the
// underlying listener fails. The returned error is nil for graceful
// shutdown (ctx cancellation), non-nil for listener / accept failures.
func (s *Server) Serve(ctx context.Context) error {
	if s.closed.Load() {
		return errors.New("wt: server already closed")
	}
	errCh := make(chan error, 1)
	go func() { errCh <- s.wt.ListenAndServe() }()

	select {
	case <-ctx.Done():
		_ = s.Close()
		return nil
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) || errors.Is(err, quic.ErrServerClosed) {
			return nil
		}
		return err
	}
}

// ServeListener runs the WT server on a pre-bound packet conn. This is
// the test-friendly entrypoint — bind UDP "127.0.0.1:0" externally,
// learn the port via conn.LocalAddr(), then hand off here.
func (s *Server) ServeListener(ctx context.Context, conn net.PacketConn) error {
	if s.closed.Load() {
		return errors.New("wt: server already closed")
	}
	errCh := make(chan error, 1)
	go func() { errCh <- s.wt.Serve(conn) }()

	select {
	case <-ctx.Done():
		_ = s.Close()
		return nil
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) || errors.Is(err, quic.ErrServerClosed) {
			return nil
		}
		return err
	}
}

// Close shuts the server down. Idempotent.
func (s *Server) Close() error {
	var err error
	s.closeOnce.Do(func() {
		s.closed.Store(true)
		// Close active sessions first so handlers can drain.
		s.mu.Lock()
		for sess := range s.sessions {
			_ = sess.closeWithError(0, "server shutdown")
		}
		s.sessions = nil
		s.mu.Unlock()
		err = s.wt.Close()
	})
	return err
}

// handleUpgrade is the HTTP handler for the /wt endpoint. It performs
// the WebTransport upgrade and dispatches accepted sessions into the
// session goroutine.
func (s *Server) handleUpgrade(w http.ResponseWriter, r *http.Request) {
	wtSess, err := s.wt.Upgrade(w, r)
	if err != nil {
		s.logger.Printf("wt: upgrade failed: %v", err)
		// webtransport-go has already written the HTTP error response.
		return
	}

	sess := &Session{
		raw:    wtSess,
		logger: s.logger,
	}

	s.mu.Lock()
	if s.sessions == nil {
		// Server closed mid-upgrade.
		s.mu.Unlock()
		_ = wtSess.CloseWithError(0, "server shutdown")
		return
	}
	s.sessions[sess] = struct{}{}
	s.mu.Unlock()

	s.logger.Printf("wt: session accepted from %s", r.RemoteAddr)

	go func() {
		defer func() {
			s.mu.Lock()
			delete(s.sessions, sess)
			s.mu.Unlock()
		}()
		s.runEchoSession(sess)
	}()
}

// runEchoSession is the v0.1 smoke handler: open the control stream,
// JSON-echo whatever envelopes the client sends. Slice #2 will replace
// this with the real 5-channel dispatch.
func (s *Server) runEchoSession(sess *Session) {
	defer func() { _ = sess.closeWithError(0, "session done") }()

	// The CLIENT opens the control stream — we accept it. This matches
	// the spec direction: clients drive session setup; the server is
	// passive at the WT layer.
	ctrl, err := sess.acceptControlStream()
	if err != nil {
		s.logger.Printf("wt: accept control stream: %v", err)
		return
	}
	defer ctrl.Close()

	// Echo loop: read until the client half-closes, write back byte-
	// for-byte. JSON framing is the client's responsibility for v0.1
	// (newline-delimited JSON is the planned framing in slice #2).
	if _, err := io.Copy(ctrl, ctrl); err != nil && !errors.Is(err, io.EOF) {
		s.logger.Printf("wt: control echo: %v", err)
	}
}
