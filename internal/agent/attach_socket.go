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
//
// Wire shape pinned in spec §11.D.1 "attach.lock-denied frame shape".
// Field changes here MUST be mirrored there (and vice versa) — this is
// the stable contract third-party / mobile attach clients code against.
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

	// AuthBroker, when non-nil, intercepts rw-mode input frames whose
	// payload parses as an auth.tool-decision JSON envelope and routes
	// them to the broker instead of the PTY InputSink. All other input
	// frames are passed through unchanged. Optional — leaving nil
	// disables tool-authorization decisions over the attach socket
	// (the PreToolUse handler then either has no UI surface or uses
	// some other transport; v0.1 wires it here only).
	AuthBroker *AuthBroker

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
	heldLock bool // true once Lock.TryAcquire has succeeded for this client
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
			// On rw disconnect, release the lock only if THIS conn
			// actually acquired it (heldLock=true). Otherwise Release
			// would return ErrNotHolder (silently dropped today, but
			// noisy when we eventually log lock errors).
			if conn.mode == AttachModeReadWrite && conn.heldLock {
				_ = s.cfg.Lock.Release(conn.client)
			}
		}()
		s.serveConn(ctx, conn)
	}()
}

// MaxHeaderBytes caps the size of the newline-terminated JSON header
// line a client may send on connect. The legitimate header is well
// under 1KB (sessionId + mode + client kind/deviceId); 64KB is a
// generous ceiling that still bounds memory growth from a malicious
// or buggy local client that opens a connection and streams bytes
// without ever sending '\n'. Without this bound, bufio.Reader's
// ReadBytes('\n') grows its internal slice unboundedly via repeated
// ReadSlice + concat, OOMing the daemon.
const MaxHeaderBytes = 64 * 1024

// serveConn implements the framed protocol for one client.
func (s *AttachServer) serveConn(ctx context.Context, conn *attachConn) {
	br := bufio.NewReader(conn.c)
	// Bound the header read at MaxHeaderBytes. We use a LimitReader
	// wrapped around the same underlying conn for just the header
	// read — once we're past the header we go back to using br
	// directly, so this doesn't constrain frame reads (which have
	// their own ErrFrameTooLarge cap below).
	headerLine, err := readBoundedLine(br, MaxHeaderBytes)
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

	// Spawn input reader for rw, OR for ro when an AuthBroker is wired.
	//
	// Why ro+AuthBroker still needs an input reader:
	// `auth.tool-decision` frames are CONTROL-PLANE (see readInputs's
	// pre-lock intercept). When the daemon has no PTY input sink (the
	// v0.1 default — daemon doesn't spawn cc itself), rw mode auto-
	// downgrades to ro via the InputSink-nil check above. But the UI
	// MUST still be able to send auth decisions back, otherwise every
	// PreToolUse hook hits the broker's 60s timeout and fail-closed
	// deny — making the auth flow architecturally inert.
	//
	// In ro+AuthBroker mode, readInputs's existing branching only fires
	// the auth-decision intercept; non-decision frames fall through to
	// the lock-gated PTY path, where TryAcquire succeeds (no holder
	// yet) but the InputSink-nil call would panic — so we also gate
	// that block on InputSink presence inside readInputs (see below).
	inputErr := make(chan error, 1)
	if desired == AttachModeReadWrite || s.cfg.AuthBroker != nil {
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

		// Auth decision frames are control plane — intercept BEFORE the
		// lock check so a UI tab that doesn't hold the writer lock can
		// still answer "may I run this tool?" prompts. The decision
		// router is keyed by requestId so cross-tab traffic is fine.
		if s.cfg.AuthBroker != nil && looksLikeAuthDecisionFrame(buf) {
			var frame AuthDecisionFrame
			if err := json.Unmarshal(buf, &frame); err == nil &&
				frame.Type == KindAuthToolDecision {
				if subErr := s.cfg.AuthBroker.SubmitDecision(frame); subErr != nil {
					s.reportError(fmt.Errorf("attach: auth decision: %w", subErr))
				}
				continue
			}
			// JSON-shaped but not a valid decision frame — fall through
			// to PTY input so we don't accidentally swallow payloads
			// that legitimately start with '{'.
		}

		// PTY input path. Skipped when the conn is ro (no InputSink
		// wired, OR client requested ro). In ro+AuthBroker mode we
		// still reach this point for non-auth-decision frames; drop
		// them silently — a ro client has no business sending PTY
		// bytes, and there's no InputSink to receive them anyway.
		if conn.mode != AttachModeReadWrite || s.cfg.InputSink == nil {
			continue
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
		// Mark held so the disconnect path's Release fires only when
		// we actually acquired (avoids noisy ErrNotHolder for clients
		// that never sent a byte).
		conn.heldLock = true

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

// looksLikeAuthDecisionFrame is a fast pre-filter that avoids json.Unmarshal
// cost on every PTY input keystroke. We only attempt JSON parsing when the
// payload looks like a top-level JSON object that contains the literal
// "auth.tool-decision". A malicious user typing the substring into a PTY
// would still hit the unmarshal path but fall through (Type field check)
// without consequence.
func looksLikeAuthDecisionFrame(buf []byte) bool {
	if len(buf) < 2 || buf[0] != '{' {
		return false
	}
	// Cheap substring scan; bounded — buf is ≤1MB by frame cap.
	const needle = "\"auth.tool-decision\""
	if len(buf) < len(needle) {
		return false
	}
	// Tiny inlined search to avoid pulling in `bytes` here for one match.
	for i := 0; i+len(needle) <= len(buf); i++ {
		if buf[i] == '"' && string(buf[i:i+len(needle)]) == needle {
			return true
		}
	}
	return false
}

// ErrHeaderTooLarge is returned by serveConn when the newline-
// terminated header read exceeds MaxHeaderBytes.
var ErrHeaderTooLarge = errors.New("agent: attach header > MaxHeaderBytes")

// readBoundedLine reads bytes up to and including the next '\n', or
// up to limit bytes (whichever comes first). Returns ErrHeaderTooLarge
// if limit is hit without seeing a newline. Implementation pulls one
// byte at a time from the bufio.Reader so we don't grow an unbounded
// internal slice the way bufio.Reader.ReadBytes('\n') would.
func readBoundedLine(br *bufio.Reader, limit int) ([]byte, error) {
	buf := make([]byte, 0, 256)
	for len(buf) < limit {
		b, err := br.ReadByte()
		if err != nil {
			return buf, err
		}
		buf = append(buf, b)
		if b == '\n' {
			return buf, nil
		}
	}
	return buf, ErrHeaderTooLarge
}

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
