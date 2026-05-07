// session.go — Session wraps a webtransport.Session.
//
// In slice #1 the Session exposes only the control + events stream
// accessors + a close primitive. The other 3 channels from §3.3.3
// (agent-bytes, catch-up, datagram) are deliberately NOT yet
// surfaced; slice #2 will add them.
//
// API note: webtransport-go returns its own `*webtransport.Stream`
// type for bidi streams (it is NOT the same as `quic.Stream`, despite
// having a near-identical interface). We expose `*webtransport.Stream`
// directly here rather than wrap it — the spec talks about WT streams
// at the v0.1 abstraction level, and adding our own wrapper before
// the dispatch slice is decided would be premature.

package wt

import (
	"context"
	"errors"
	"log"
	"sync"

	"github.com/quic-go/webtransport-go"
)

// Session is the daemon-side handle on a single connected client. One
// WT session per connected client device; multiple bidi streams (the
// 5-channel model in §3.3.3) are multiplexed inside one session
// natively by WebTransport.
type Session struct {
	raw    *webtransport.Session
	logger *log.Logger

	mu          sync.Mutex
	controlOpen bool
	eventsOpen  bool

	closed bool
}

// Context returns a context that is cancelled when the session closes.
func (s *Session) Context() context.Context { return s.raw.Context() }

// RemoteAddr returns the client's UDP address.
func (s *Session) RemoteAddr() string { return s.raw.RemoteAddr().String() }

// Control is the convenience accessor for the control bidi stream
// (§3.3.3 stream 1: session.* / control.* / input.*). Slice #1 echoes
// it; slice #2 will route envelopes off it.
//
// Convention: the CLIENT opens the control stream first; the server
// accepts it. Calling Control() on the server therefore blocks until
// the client opens its first stream.
func (s *Session) Control(ctx context.Context) (*webtransport.Stream, error) {
	return s.acceptControlStreamCtx(ctx)
}

// Events is the convenience accessor for the events bidi stream
// (§3.3.3 stream 2: output.agent-event / output.hook-event). Server
// opens this one (server → client direction is the dominant traffic
// flow).
//
// Slice #2 will rewire so the events stream is a server-initiated
// uni-stream (it's only ever server→client per the spec); for slice
// #1 we keep it as a bidi for symmetry with the echo smoke.
func (s *Session) Events(ctx context.Context) (*webtransport.Stream, error) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil, errors.New("wt: session closed")
	}
	if s.eventsOpen {
		s.mu.Unlock()
		return nil, errors.New("wt: events stream already opened")
	}
	s.eventsOpen = true
	s.mu.Unlock()

	str, err := s.raw.OpenStreamSync(ctx)
	if err != nil {
		s.mu.Lock()
		s.eventsOpen = false
		s.mu.Unlock()
		return nil, err
	}
	return str, nil
}

// acceptControlStream is the package-internal "accept the first stream
// the client opens" helper. The control stream is always client-
// initiated by convention.
func (s *Session) acceptControlStream() (*webtransport.Stream, error) {
	return s.acceptControlStreamCtx(s.raw.Context())
}

func (s *Session) acceptControlStreamCtx(ctx context.Context) (*webtransport.Stream, error) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil, errors.New("wt: session closed")
	}
	if s.controlOpen {
		s.mu.Unlock()
		return nil, errors.New("wt: control stream already accepted")
	}
	s.controlOpen = true
	s.mu.Unlock()

	str, err := s.raw.AcceptStream(ctx)
	if err != nil {
		s.mu.Lock()
		s.controlOpen = false
		s.mu.Unlock()
		return nil, err
	}
	return str, nil
}

// closeWithError tears the session down with a WebTransport-level
// session error code + message.
func (s *Session) closeWithError(code webtransport.SessionErrorCode, msg string) error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	s.mu.Unlock()
	return s.raw.CloseWithError(code, msg)
}
