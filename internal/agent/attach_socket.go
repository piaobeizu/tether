// Package agent — local Unix socket listener (spec §4.4 ctrlListener +
// §11.U attach entry point).
//
// The attach socket lives at ~/.tether/attach.sock by default and
// accepts connections from `tether attach` (or any other local client
// running as the same user). Wire framing:
//
//	1. Client connects, sends a single newline-terminated JSON header
//	   { "sessionId": "<sid>", "mode": "ro" | "rw", "client": {...} }
//
//	2. Server replies with one length-prefixed JSON envelope per cc
//	   event (4-byte big-endian length + payload). Frame 0 is the
//	   accept ack:
//
//	     {"type":"attach.ack", "mode":"<granted-mode>", "sessionId":"..."}
//
//	   Subsequent frames are wire envelopes from the JSONL watcher
//	   (output.agent-event / output.hook-event / session.state).
//
//	3. If mode == "rw", server reads length-prefixed user-input frames
//	   from the same connection:
//
//	     [4-byte BE length][payload bytes]
//
//	   payload is opaque bytes destined for the PTY. Server passes
//	   through PTYSession.SendInput AFTER acquiring the lock; on
//	   ErrInUse the server replies with one length-prefixed
//	   {"type":"attach.lock-denied"} frame and ignores the input.
//
// v0.1 keeps this minimal — no auth (Unix socket perms = 0600 = trusted
// local user only), no compression, no resume cursor. Reconnect logic
// lives in the client; the daemon teardown on disconnect is clean.

package agent

import (
	"bufio"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/piaobeizu/tether/internal/lock"
)

// DefaultAttachSocketPath is the conventional path. Daemon resolves
// $HOME at startup.
const DefaultAttachSocketPath = ".tether/attach.sock"

// AttachMode names the read-only vs read-write attach affordance.
type AttachMode string

const (
	AttachModeReadOnly  AttachMode = "ro"
	AttachModeReadWrite AttachMode = "rw"
)

// AttachHeader is the first frame the client sends.
type AttachHeader struct {
	SessionID string `json:"sessionId"`
	Mode      string `json:"mode"`
	// Client identifies the attach client for lock attribution.
	// Kind = "terminal" / "mobile"; DeviceID = stable per-pairing id.
	Client struct {
		Kind     string `json:"kind"`
		DeviceID string `json:"deviceId"`
	} `json:"client"`
}

// AckFrame is the first server-to-client frame (mode confirmation).
type AckFrame struct {
	Type      string `json:"type"`
	SessionID string `json:"sessionId"`
	Mode      string `json:"mode"`
	Message   string `json:"message,omitempty"`
}

// LockDeniedFrame is sent in response to a write that the server can't
// authorize because the lock is held.
type LockDeniedFrame struct {
	Type   string `json:"type"`
	Reason string `json:"reason"`
	Holder struct {
		Kind     string `json:"kind"`
		DeviceID string `json:"deviceId"`
	} `json:"holder"`
}

// AttachServerConfig configures NewAttachServer.
type AttachServerConfig struct {
	// SocketPath is the absolute path to the listening socket. Caller
	// resolves $HOME / cleanup on prior run. Required.
	SocketPath string

	// Emitter provides per-session envelope subscriptions.
	Emitter *EnvelopeEmitter

	// Lock is the per-session writer-lock. v0.1 has one global lock
	// shared across all sessions on this daemon (cc model: one user,
	// one cc instance). Callers building multi-session daemons can
	// pass a different Lock per accept handler — but that's later.
	Lock *lock.Lock

	// InputSink, when non-nil, is called with raw input bytes from
	// rw-mode clients AFTER lock acquisition + Touch. It is the wire
	// to PTYSession.SendInput in the daemon topology; tests inject a
	// channel-recording sink. nil InputSink rejects rw mode (clients
	// that ask for rw get auto-downgraded to ro and warned in ack
	// message).
	InputSink func(sessionID string, data []byte) error

	// OnError surfaces accept-loop / per-conn errors to the daemon
	// log. Optional.
	OnError func(err error)

	// SocketPerm lets tests dial without root; production uses
	// 0600 (owner-only). Zero = use 0600.
	SocketPerm os.FileMode
}

// AttachServer accepts attach socket connections.
type AttachServer struct {
	cfg AttachServerConfig
	lis net.Listener

	mu     sync.Mutex
	closed bool
	conns  map[*attachConn]struct{}
}

// attachConn tracks one connected client.
type attachConn struct {
	c        net.Conn
	sid      string
	mode     AttachMode
	client   lock.ClientID
	cancelEm func() // tear down envelope subscription
	done     chan struct{}
}

// NewAttachServer creates the Unix socket listener. The socket file is
// ensured-removed before listen (recover from a previous unclean
// shutdown). Returns an error on bind failure.
func NewAttachServer(cfg AttachServerConfig) (*AttachServer, error) {
	if cfg.SocketPath == "" {
		return nil, errors.New("agent: AttachServerConfig.SocketPath required")
	}
	if cfg.Emitter == nil {
		return nil, errors.New("agent: AttachServerConfig.Emitter required")
	}
	if cfg.Lock == nil {
		return nil, errors.New("agent: AttachServerConfig.Lock required")
	}
	perm := cfg.SocketPerm
	if perm == 0 {
		perm = 0o600
	}

	if err := os.MkdirAll(filepath.Dir(cfg.SocketPath), 0o700); err != nil {
		return nil, fmt.Errorf("agent: mkdir socket dir: %w", err)
	}
	// Best-effort cleanup of stale socket.
	_ = os.Remove(cfg.SocketPath)

	lis, err := net.Listen("unix", cfg.SocketPath)
	if err != nil {
		return nil, fmt.Errorf("agent: listen unix %s: %w", cfg.SocketPath, err)
	}
	if err := os.Chmod(cfg.SocketPath, perm); err != nil {
		_ = lis.Close()
		_ = os.Remove(cfg.SocketPath)
		return nil, fmt.Errorf("agent: chmod socket: %w", err)
	}

	return &AttachServer{
		cfg:   cfg,
		lis:   lis,
		conns: make(map[*attachConn]struct{}),
	}, nil
}

// SocketPath returns the listening path.
func (s *AttachServer) SocketPath() string { return s.cfg.SocketPath }

// Serve runs the accept loop until ctx is canceled or the listener
// closes. Per-connection handlers run in their own goroutines; Serve
// blocks until they all exit.
//
// Designed to plug straight into a watchdog Subsystem: callers wrap
// Serve in a wrapper that calls hb() periodically.
func (s *AttachServer) Serve(ctx context.Context) error {
	// Accept loop.
	connCh := make(chan net.Conn, 16)
	errCh := make(chan error, 1)
	go func() {
		for {
			c, err := s.lis.Accept()
			if err != nil {
				select {
				case errCh <- err:
				default:
				}
				return
			}
			connCh <- c
		}
	}()

	for {
		select {
		case <-ctx.Done():
			return s.shutdown()
		case err := <-errCh:
			// Listener closed (ctx canceled by another path) or real error.
			if isClosedConn(err) {
				return s.shutdown()
			}
			s.reportError(err)
			return s.shutdown()
		case c := <-connCh:
			s.handleNewConn(ctx, c)
		}
	}
}

func (s *AttachServer) reportError(err error) {
	if s.cfg.OnError != nil {
		s.cfg.OnError(err)
	}
}

// shutdown closes all open conns + the listener. Idempotent.
func (s *AttachServer) shutdown() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	conns := make([]*attachConn, 0, len(s.conns))
	for c := range s.conns {
		conns = append(conns, c)
	}
	s.mu.Unlock()

	for _, c := range conns {
		_ = c.c.Close()
		<-c.done
	}
	err := s.lis.Close()
	_ = os.Remove(s.cfg.SocketPath)
	return err
}

// Close stops the server. Equivalent to canceling Serve's ctx.
func (s *AttachServer) Close() error {
	return s.shutdown()
}

func (s *AttachServer) handleNewConn(ctx context.Context, c net.Conn) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		_ = c.Close()
		return
	}
	conn := &attachConn{c: c, done: make(chan struct{})}
	s.conns[conn] = struct{}{}
	s.mu.Unlock()

	go func() {
		defer close(conn.done)
		defer func() {
			s.mu.Lock()
			delete(s.conns, conn)
			s.mu.Unlock()
			_ = c.Close()
			if conn.cancelEm != nil {
				conn.cancelEm()
			}
			// On rw disconnect, release the lock if we still hold it.
			if conn.mode == AttachModeReadWrite && conn.client != (lock.ClientID{}) {
				_ = s.cfg.Lock.Release(conn.client)
			}
		}()
		s.serveConn(ctx, conn)
	}()
}

// serveConn implements the framed protocol for one client.
func (s *AttachServer) serveConn(ctx context.Context, conn *attachConn) {
	br := bufio.NewReader(conn.c)
	headerLine, err := br.ReadBytes('\n')
	if err != nil {
		s.reportError(fmt.Errorf("attach: read header: %w", err))
		return
	}
	var hdr AttachHeader
	if err := json.Unmarshal(headerLine, &hdr); err != nil {
		s.reportError(fmt.Errorf("attach: parse header: %w", err))
		return
	}
	if hdr.SessionID == "" {
		s.reportError(errors.New("attach: empty sessionId in header"))
		return
	}

	conn.sid = hdr.SessionID
	conn.client = lock.ClientID{Kind: hdr.Client.Kind, DeviceID: hdr.Client.DeviceID}
	if conn.client.Kind == "" {
		// Attribute as terminal by default — local socket is the
		// `tether attach` raw-mode entry point (§11.U).
		conn.client.Kind = lock.KindTerminal
	}
	if conn.client.DeviceID == "" {
		// Synthesize a per-conn device id so lock.History remains
		// useful even when the client doesn't supply one.
		conn.client.DeviceID = fmt.Sprintf("local-%d", time.Now().UnixNano())
	}

	desired := AttachMode(hdr.Mode)
	if desired != AttachModeReadOnly && desired != AttachModeReadWrite {
		desired = AttachModeReadOnly
	}
	if desired == AttachModeReadWrite && s.cfg.InputSink == nil {
		desired = AttachModeReadOnly
	}
	conn.mode = desired

	// Subscribe to the session's envelope stream BEFORE writing the
	// ack — that way the consumer can observe events that arrive in
	// the same fsnotify wakeup as the connect.
	envCh, cancelEm, err := s.cfg.Emitter.Subscribe(hdr.SessionID)
	if err != nil {
		s.reportError(fmt.Errorf("attach: emitter subscribe: %w", err))
		return
	}
	conn.cancelEm = cancelEm

	ack := AckFrame{
		Type:      "attach.ack",
		SessionID: hdr.SessionID,
		Mode:      string(desired),
	}
	if AttachMode(hdr.Mode) == AttachModeReadWrite && desired == AttachModeReadOnly {
		ack.Message = "rw downgraded to ro: daemon has no input sink wired"
	}
	if err := writeFrame(conn.c, ack); err != nil {
		s.reportError(fmt.Errorf("attach: write ack: %w", err))
		return
	}

	// Spawn input reader if rw.
	inputErr := make(chan error, 1)
	if desired == AttachModeReadWrite {
		go func() { inputErr <- s.readInputs(ctx, conn, br) }()
	}

	// Write loop.
	for {
		select {
		case <-ctx.Done():
			return
		case env, ok := <-envCh:
			if !ok {
				return
			}
			if err := writeFrame(conn.c, env); err != nil {
				if isClosedConn(err) {
					return
				}
				s.reportError(fmt.Errorf("attach: write envelope: %w", err))
				return
			}
		case err := <-inputErr:
			if err != nil && !isClosedConn(err) && !errors.Is(err, io.EOF) {
				s.reportError(fmt.Errorf("attach: input loop: %w", err))
			}
			return
		}
	}
}

// readInputs reads length-prefixed input frames from the client and
// routes them to the InputSink, gated by the lock.
func (s *AttachServer) readInputs(_ context.Context, conn *attachConn, br *bufio.Reader) error {
	for {
		var lenBuf [4]byte
		if _, err := io.ReadFull(br, lenBuf[:]); err != nil {
			return err
		}
		n := binary.BigEndian.Uint32(lenBuf[:])
		if n > 1<<20 { // 1 MB cap per frame
			return fmt.Errorf("attach: input frame too large (%d bytes)", n)
		}
		buf := make([]byte, n)
		if _, err := io.ReadFull(br, buf); err != nil {
			return err
		}

		// TryAcquire — first byte from this client wins if lock is
		// free; touches forward the auto-release timer otherwise.
		if err := s.cfg.Lock.TryAcquire(conn.client); err != nil {
			holder, _ := s.cfg.Lock.Holder()
			frame := LockDeniedFrame{Type: "attach.lock-denied", Reason: err.Error()}
			frame.Holder.Kind = holder.Kind
			frame.Holder.DeviceID = holder.DeviceID
			_ = writeFrame(conn.c, frame)
			// Drop input — the contract is "writes need the lock";
			// dropping rather than buffering is the spec §11.D
			// behavior (caller should ForceTakeover via control
			// frame; v0.1 of this socket wires that as a follow-up).
			continue
		}

		if err := s.cfg.InputSink(conn.sid, buf); err != nil {
			s.reportError(fmt.Errorf("attach: input sink: %w", err))
			// Don't terminate the connection — caller may want to
			// retry. But return to avoid infinite tight loop on
			// persistent failure.
			return err
		}
		// Touch forwards the auto-release timer.
		_ = s.cfg.Lock.Touch(conn.client)
	}
}

// writeFrame serializes v as JSON and writes a 4-byte BE length
// prefix + payload.
func writeFrame(w io.Writer, v any) error {
	body, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("marshal frame: %w", err)
	}
	var hdr [4]byte
	binary.BigEndian.PutUint32(hdr[:], uint32(len(body)))
	if _, err := w.Write(hdr[:]); err != nil {
		return err
	}
	_, err = w.Write(body)
	return err
}

// ReadFrame is the client-side helper exposed for `tether attach` (and
// for the integration tests in this package): reads a 4-byte BE length
// prefix + payload from r. Returns ErrFrameTooLarge for >1MB frames.
func ReadFrame(r io.Reader) ([]byte, error) {
	var hdr [4]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return nil, err
	}
	n := binary.BigEndian.Uint32(hdr[:])
	if n > 1<<20 {
		return nil, ErrFrameTooLarge
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(r, buf); err != nil {
		return nil, err
	}
	return buf, nil
}

// WriteInputFrame is the client-side helper for sending an input frame
// (rw-mode attach client → daemon → PTY). Same 4-byte BE length-
// prefix shape as the server-to-client frames; symmetry keeps both
// codecs simple.
func WriteInputFrame(w io.Writer, payload []byte) error {
	var hdr [4]byte
	binary.BigEndian.PutUint32(hdr[:], uint32(len(payload)))
	if _, err := w.Write(hdr[:]); err != nil {
		return err
	}
	_, err := w.Write(payload)
	return err
}

// ErrFrameTooLarge is returned by ReadFrame when the length prefix
// exceeds the 1MB safety cap.
var ErrFrameTooLarge = errors.New("agent: attach frame > 1MB")

// isClosedConn matches typical "use of closed network connection"
// error variants we want to swallow rather than log.
func isClosedConn(err error) bool {
	if err == nil {
		return false
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return false
	}
	if errors.Is(err, net.ErrClosed) {
		return true
	}
	return false
}
