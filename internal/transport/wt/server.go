// Package wt is the WebTransport-over-HTTP/3 server for the tether
// daemon's cross-device transport (spec §3.3 / §11.V / D-13 / D-21).
//
// Status — slice #2 (channel multiplex layered onto slice #1's listener):
//
//   - Boots a webtransport-go server on a configurable UDP port.
//   - Accepts WT sessions at a single endpoint path ("/wt").
//   - Each Session runs a per-session accept loop demuxing incoming
//     bidi/uni streams onto per-channel queues using the §3.3.3 1-byte
//     channel-id wire convention:
//       0x01 control / 0x02 events / 0x03 agent-bytes / 0x04 catch-up /
//       0x05 datagram (tag for unreliable payloads).
//   - Exposes per-channel API on Session: Control, Events,
//     OpenAgentBytes, AcceptCatchUp, SendDatagram, RecvDatagram (plus
//     symmetric counterparts).
//   - A Go-side Client mirrors the same conventions for tests + future
//     internal callers.
//   - Generates a self-signed dev cert when no cert is configured;
//     production paths supply Cert/Key and skip the dev cert path.
//
// What this package does NOT do yet (later slices):
//
//   - §3.3.1 wire envelope dispatch (encrypted payload, AD-bound kind,
//     replay/dedup) — slice #3. Channels currently carry opaque bytes.
//   - Pairing / device auth (X25519 ECDH handshake from
//     internal/crypto) — slice #4.
//   - Replacing UDS attach for desktop / mobile clients — slice #5.
//
// Daemon integration: opt-in via daemon.Config.WTListenAddr (or the
// `--wt-addr` flag on `cmd/tether daemon`); empty == disabled. The
// existing UDS attach socket stays the v0.1 local surface for
// `tether attach` on the daemon-host shell.
//
// Quick API surface:
//
//	srv, err := wt.New(ctx, wt.Config{
//	    Addr: ":4444",
//	    SessionHandler: func(s *wt.Session) {
//	        ctrl, _ := s.Control(ctx)
//	        // ... dispatch envelopes off ctrl + s.Events / etc.
//	    },
//	})
//	go srv.Serve(ctx)
//	// client side:
//	cli, _ := wt.Dial(ctx, wt.ClientConfig{URL: "https://...:4444/wt", ...})
//	str, _ := cli.OpenControl(ctx)
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

	// SessionHandler runs once per accepted WT session. nil → use the
	// default "log + idle until close" handler suitable for the daemon
	// wtSubsystem (slice #2 doesn't yet wire envelope dispatch). Tests
	// inject custom handlers (e.g. per-channel echo) to drive the
	// channel router.
	//
	// The handler runs in its own goroutine; returning from it does
	// NOT close the session. The session lifecycle is owned by the
	// Server (closed on Server.Close).
	SessionHandler func(*Session)
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

	sessionHandler func(*Session)

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
		addr:           addr,
		logger:         logger,
		sessions:       make(map[*Session]struct{}),
		sessionHandler: cfg.SessionHandler,
	}
	if srv.sessionHandler == nil {
		srv.sessionHandler = func(sess *Session) {
			// Default: log accept + sit on the session until it closes.
			// The daemon wtSubsystem uses this — slice #3 will swap in
			// real envelope dispatch.
			<-sess.Context().Done()
		}
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
// the WebTransport upgrade, wires up the channel-demux Session, and
// hands it to the configured session handler.
func (s *Server) handleUpgrade(w http.ResponseWriter, r *http.Request) {
	wtSess, err := s.wt.Upgrade(w, r)
	if err != nil {
		s.logger.Printf("wt: upgrade failed: %v", err)
		// webtransport-go has already written the HTTP error response.
		return
	}

	sess := newSession(wtSess, s.logger)

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
			_ = sess.closeWithError(0, "session done")
		}()
		s.sessionHandler(sess)
	}()
}
