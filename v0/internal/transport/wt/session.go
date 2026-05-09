// session.go — server-side Session with §3.3.3 channel demux.
//
// Slice #2 replaces slice #1's "first-stream-is-control echo" with a
// real router: a per-Session goroutine accepts every incoming bidi /
// uni stream, reads the leading channel-id byte, and hands the stream
// to a per-channel queue. Callers consume via channel-typed accessors
// (Control, AcceptCatchUp, AcceptAgentBytes...). Server-initiated
// channels (events, agent-bytes) are written by Open* helpers that
// prepend the channel-id before returning the stream to the caller.
//
// API shape on the server side (mirror on Client):
//
//	control     — Control(ctx) blocks until the client opens it.
//	events      — Events(ctx) opens it server→client.
//	agent-bytes — OpenAgentBytes(ctx) opens a uni-stream server→client.
//	catch-up    — AcceptCatchUp(ctx) accepts the client-initiated bidi.
//	datagram    — SendDatagram / RecvDatagram wrap the WT primitives
//	              with the 1-byte tag.
//
// Bad channel-id (leading byte ∉ {0x01..0x05}, or 0x05 on a stream):
// the offending stream is reset with streamErrorCodeBadChannelID and
// the accept loop continues. The session itself is NOT torn down; one
// bad stream from a buggy client should not kill an otherwise-healthy
// session.

package wt

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"sync"
	"time"

	"github.com/quic-go/webtransport-go"
)

// streamCapacity caps the per-channel accept queue. A backed-up
// consumer just stalls new accepts on that channel, which is the right
// behavior — control / catch-up streams should never burst high enough
// to need real buffering.
const streamCapacity = 16

// Session is the daemon-side handle on a single connected client.
//
// Lifetime: created by Server.handleUpgrade, runs until either the
// peer closes the WT session or Server.Close fires. The accept loop
// goroutine demuxes incoming streams onto per-channel queues for the
// lifetime of the session.
type Session struct {
	raw    *webtransport.Session
	logger *log.Logger

	// per-channel bidi accept queues. Buffered; a slow consumer just
	// pauses new accepts on that channel.
	bidiQ map[ChannelID]chan *webtransport.Stream

	// per-channel uni accept queues (only ChannelAgentBytes uses uni
	// streams in v0.1, but the table is keyed for symmetry).
	uniQ map[ChannelID]chan *webtransport.ReceiveStream

	mu     sync.Mutex
	closed bool

	// closeQueues is closed once when the accept loop exits, signaling
	// callers blocked on a per-channel queue to give up.
	closeQueues sync.Once
	queuesDone  chan struct{}
}

// newSession wires a fresh Session around an upgraded
// *webtransport.Session and starts the accept loop. The accept loop
// goroutine runs until the WT session ends.
func newSession(wts *webtransport.Session, logger *log.Logger) *Session {
	s := &Session{
		raw:        wts,
		logger:     logger,
		bidiQ:      make(map[ChannelID]chan *webtransport.Stream, 4),
		uniQ:       make(map[ChannelID]chan *webtransport.ReceiveStream, 1),
		queuesDone: make(chan struct{}),
	}
	for _, c := range []ChannelID{ChannelControl, ChannelEvents, ChannelCatchUp} {
		s.bidiQ[c] = make(chan *webtransport.Stream, streamCapacity)
	}
	s.uniQ[ChannelAgentBytes] = make(chan *webtransport.ReceiveStream, streamCapacity)
	go s.acceptBidiLoop()
	go s.acceptUniLoop()
	return s
}

// Context returns a context that is cancelled when the session closes.
func (s *Session) Context() context.Context { return s.raw.Context() }

// RemoteAddr returns the client's UDP address.
func (s *Session) RemoteAddr() string { return s.raw.RemoteAddr().String() }

// Control accepts the client-initiated control bidi stream. The
// channel-id byte has already been consumed by the accept loop; what
// the caller gets is the post-prefix payload stream.
func (s *Session) Control(ctx context.Context) (*webtransport.Stream, error) {
	return s.acceptBidi(ctx, ChannelControl)
}

// Events opens the events bidi stream server → client. The
// channel-id byte is written before the stream is handed back, so the
// caller can begin sending payload immediately.
//
// Direction matches slice #1: server opens, client accepts (events
// flow is dominantly server→client, server-initiated keeps the API
// shape stable across slices).
func (s *Session) Events(ctx context.Context) (*webtransport.Stream, error) {
	return s.openBidi(ctx, ChannelEvents)
}

// OpenAgentBytes opens a server-initiated unidirectional stream for
// PTY agent bytes (§3.3.3 stream 3). v0.1 doesn't actually consume
// this remotely (D-14) but the channel exists so the wire stays
// stable.
//
// The returned WriteCloser writes payload bytes; the channel-id byte
// has already been written.
func (s *Session) OpenAgentBytes(ctx context.Context) (io.WriteCloser, error) {
	return s.openUni(ctx, ChannelAgentBytes)
}

// AcceptCatchUp accepts the client-initiated catch-up bidi stream
// (§3.3.3 stream 4). Multiple catch-up streams over the lifetime of a
// session are allowed; each call returns the next one.
func (s *Session) AcceptCatchUp(ctx context.Context) (*webtransport.Stream, error) {
	return s.acceptBidi(ctx, ChannelCatchUp)
}

// SendDatagram sends a datagram tagged with ChannelDatagram. The 1-
// byte tag is prepended to the payload so the receiving side's
// RecvDatagram can validate + strip it.
func (s *Session) SendDatagram(payload []byte) error {
	return sendTaggedDatagram(s.raw, ChannelDatagram, payload)
}

// RecvDatagram blocks until a datagram arrives, validates the channel-
// id prefix, and returns the payload (without the tag byte).
//
// A datagram with a bad / wrong tag returns errBadChannelID; callers
// can choose to log + retry vs treat as fatal. Datagrams are by spec
// unreliable, so a single dropped/garbled one is recoverable.
func (s *Session) RecvDatagram(ctx context.Context) ([]byte, error) {
	return recvTaggedDatagram(s.raw, ctx, ChannelDatagram)
}

// AcceptAgentBytes accepts a server-initiated uni-stream tagged with
// ChannelAgentBytes. Used by the client-side mirror; on the server
// side it would be unusual (server is the writer) but kept for
// symmetry with the Client API.
func (s *Session) AcceptAgentBytes(ctx context.Context) (*webtransport.ReceiveStream, error) {
	return s.acceptUni(ctx, ChannelAgentBytes)
}

// AcceptEvents accepts a server-initiated events bidi stream. On the
// server itself this is rarely used (server opens, client accepts);
// kept for symmetry / multi-server pairing scenarios.
func (s *Session) AcceptEvents(ctx context.Context) (*webtransport.Stream, error) {
	return s.acceptBidi(ctx, ChannelEvents)
}

// OpenControl opens a control bidi stream client → server. Server-side
// callers normally use Control (accept). This exists so a future
// server-initiated control RPC can use the same prefix.
func (s *Session) OpenControl(ctx context.Context) (*webtransport.Stream, error) {
	return s.openBidi(ctx, ChannelControl)
}

// OpenCatchUp opens a catch-up bidi stream client → server. Server-
// side callers normally use AcceptCatchUp.
func (s *Session) OpenCatchUp(ctx context.Context) (*webtransport.Stream, error) {
	return s.openBidi(ctx, ChannelCatchUp)
}

// closeWithError tears the session down. Idempotent.
func (s *Session) closeWithError(code webtransport.SessionErrorCode, msg string) error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	s.mu.Unlock()
	err := s.raw.CloseWithError(code, msg)
	s.closeQueues.Do(func() { close(s.queuesDone) })
	return err
}

// acceptBidi consumes from the per-channel bidi queue.
func (s *Session) acceptBidi(ctx context.Context, c ChannelID) (*webtransport.Stream, error) {
	q, ok := s.bidiQ[c]
	if !ok {
		return nil, fmt.Errorf("wt: no bidi queue for %s", c)
	}
	select {
	case str, ok := <-q:
		if !ok {
			return nil, errors.New("wt: session closed")
		}
		return str, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-s.queuesDone:
		return nil, errors.New("wt: session closed")
	case <-s.raw.Context().Done():
		return nil, errors.New("wt: session closed")
	}
}

// acceptUni consumes from the per-channel uni queue.
func (s *Session) acceptUni(ctx context.Context, c ChannelID) (*webtransport.ReceiveStream, error) {
	q, ok := s.uniQ[c]
	if !ok {
		return nil, fmt.Errorf("wt: no uni queue for %s", c)
	}
	select {
	case str, ok := <-q:
		if !ok {
			return nil, errors.New("wt: session closed")
		}
		return str, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-s.queuesDone:
		return nil, errors.New("wt: session closed")
	case <-s.raw.Context().Done():
		return nil, errors.New("wt: session closed")
	}
}

// openBidi opens a server-initiated bidi stream and writes the
// channel-id prefix before returning.
func (s *Session) openBidi(ctx context.Context, c ChannelID) (*webtransport.Stream, error) {
	str, err := s.raw.OpenStreamSync(ctx)
	if err != nil {
		return nil, err
	}
	if _, err := str.Write([]byte{byte(c)}); err != nil {
		_ = str.Close()
		return nil, fmt.Errorf("wt: write %s tag: %w", c, err)
	}
	return str, nil
}

// openUni opens a server-initiated uni-stream and writes the
// channel-id prefix before returning.
func (s *Session) openUni(ctx context.Context, c ChannelID) (io.WriteCloser, error) {
	str, err := s.raw.OpenUniStreamSync(ctx)
	if err != nil {
		return nil, err
	}
	if _, err := str.Write([]byte{byte(c)}); err != nil {
		_ = str.Close()
		return nil, fmt.Errorf("wt: write %s tag: %w", c, err)
	}
	return str, nil
}

// acceptBidiLoop pulls bidi streams off the WT session, reads their
// 1-byte channel-id, and routes them to the matching queue. Bad
// channel-ids are logged + the stream reset; the loop continues.
//
// Termination: returns when the WT session context closes or
// AcceptStream returns an error. On exit, closes per-channel queues
// so blocked callers wake up with "session closed".
func (s *Session) acceptBidiLoop() {
	defer s.closeQueues.Do(func() { close(s.queuesDone) })
	for {
		str, err := s.raw.AcceptStream(s.raw.Context())
		if err != nil {
			return
		}
		go s.routeIncomingBidi(str)
	}
}

// acceptUniLoop is the uni-stream equivalent.
func (s *Session) acceptUniLoop() {
	for {
		str, err := s.raw.AcceptUniStream(s.raw.Context())
		if err != nil {
			return
		}
		go s.routeIncomingUni(str)
	}
}

// routeIncomingBidi reads one byte of channel-id then enqueues to the
// matching per-channel queue. Bad byte → reset stream + log.
//
// A peer that opens a bidi stream and never writes the leading byte
// would otherwise pin this goroutine forever — readChannelID applies
// defaultChannelIDDeadline so a stalled / hostile peer is evicted
// within ~5s instead of leaking a goroutine per opened stream (M1).
func (s *Session) routeIncomingBidi(str *webtransport.Stream) {
	tag, err := readChannelID(str, defaultChannelIDDeadline)
	if err != nil {
		s.logger.Printf("wt: bidi stream tag read: %v", err)
		str.CancelRead(streamErrorCodeBadChannelID)
		str.CancelWrite(streamErrorCodeBadChannelID)
		return
	}
	if !tag.streamCapable() {
		s.logger.Printf("wt: bidi stream rejected: %s (datagram-only or unknown)", tag)
		str.CancelRead(streamErrorCodeBadChannelID)
		str.CancelWrite(streamErrorCodeBadChannelID)
		return
	}
	q, ok := s.bidiQ[tag]
	if !ok {
		s.logger.Printf("wt: bidi stream %s arrived but no queue (server-only channel?)", tag)
		str.CancelRead(streamErrorCodeBadChannelID)
		str.CancelWrite(streamErrorCodeBadChannelID)
		return
	}
	select {
	case q <- str:
	case <-s.raw.Context().Done():
		str.CancelRead(streamErrorCodeBadChannelID)
		str.CancelWrite(streamErrorCodeBadChannelID)
	}
}

// routeIncomingUni mirrors routeIncomingBidi for uni-streams. Uses the
// same defaultChannelIDDeadline so an abandoned uni stream cannot leak
// a goroutine (M1).
func (s *Session) routeIncomingUni(str *webtransport.ReceiveStream) {
	tag, err := readChannelID(str, defaultChannelIDDeadline)
	if err != nil {
		s.logger.Printf("wt: uni stream tag read: %v", err)
		str.CancelRead(streamErrorCodeBadChannelID)
		return
	}
	if !tag.streamCapable() {
		s.logger.Printf("wt: uni stream rejected: %s", tag)
		str.CancelRead(streamErrorCodeBadChannelID)
		return
	}
	q, ok := s.uniQ[tag]
	if !ok {
		s.logger.Printf("wt: uni stream %s arrived but no queue", tag)
		str.CancelRead(streamErrorCodeBadChannelID)
		return
	}
	select {
	case q <- str:
	case <-s.raw.Context().Done():
		str.CancelRead(streamErrorCodeBadChannelID)
	}
}

// channelIDDeadliner is the subset of *webtransport.Stream /
// *webtransport.ReceiveStream we touch to bound readChannelID. Both
// concrete types satisfy this; tests can stub with an in-memory pipe
// that no-ops SetReadDeadline.
type channelIDDeadliner interface {
	io.Reader
	SetReadDeadline(time.Time) error
}

// readChannelID reads exactly one byte from r and validates it as a
// known channel-id. The returned ChannelID may still be ChannelDatagram
// — caller must check streamCapable().
//
// deadline > 0 caps how long we wait for the byte; a misbehaving peer
// that opens a stream and never writes the tag would otherwise pin a
// goroutine forever (M1). Pass 0 to disable the deadline (e.g. tests
// against an io.Reader that doesn't support SetReadDeadline).
func readChannelID(r channelIDDeadliner, deadline time.Duration) (ChannelID, error) {
	if deadline > 0 {
		// Best-effort — if SetReadDeadline isn't supported by this
		// reader (test stubs), fall through and read without a cap.
		_ = r.SetReadDeadline(time.Now().Add(deadline))
		defer func() { _ = r.SetReadDeadline(time.Time{}) }()
	}
	var buf [1]byte
	if _, err := io.ReadFull(r, buf[:]); err != nil {
		return 0, err
	}
	c := ChannelID(buf[0])
	if !c.valid() {
		return c, fmt.Errorf("%w: 0x%02x", errBadChannelID, buf[0])
	}
	return c, nil
}

// sendTaggedDatagram prepends the channel-id byte before SendDatagram.
// Allocates a single-shot buffer; datagrams are tiny (health-probe
// payloads <100B) so the alloc is in the noise.
func sendTaggedDatagram(raw *webtransport.Session, c ChannelID, payload []byte) error {
	buf := make([]byte, 1+len(payload))
	buf[0] = byte(c)
	copy(buf[1:], payload)
	return raw.SendDatagram(buf)
}

// recvTaggedDatagram is the inverse of sendTaggedDatagram. The expected
// channel-id is supplied so a stray tag returns a typed error rather
// than silently mis-routing.
func recvTaggedDatagram(raw *webtransport.Session, ctx context.Context, expect ChannelID) ([]byte, error) {
	b, err := raw.ReceiveDatagram(ctx)
	if err != nil {
		return nil, err
	}
	if len(b) < 1 {
		return nil, fmt.Errorf("%w: empty datagram", errBadChannelID)
	}
	got := ChannelID(b[0])
	if got != expect {
		return nil, fmt.Errorf("%w: got %s, want %s", errBadChannelID, got, expect)
	}
	return b[1:], nil
}
