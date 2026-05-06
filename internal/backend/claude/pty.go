// PTY spawn mode for cc. Complements the existing NDJSON stdio spawn
// (spawn.go) used by the recover/control protocol path.
//
// Spec §5.1 requires the daemon-side cc instance to run under a PTY so
// the TUI can paint its raw rendering and so submit-on-`\r` (F-01) works
// the same way it does when a human types into a terminal. Daemon-side
// reads continue to be tailing the cc-managed JSONL file (which is the
// authoritative event source per §5.6 / F-04); the PTY byte stream is
// an opaque view-state pipe used only by the local `tether attach`
// raw-mode TTY (§11.U). This module therefore exposes two affordances:
//
//   - SpawnPTY: start cc inside a PTY pair, returning the (subprocess,
//     master FD).
//   - PTYSession: a thin wrapper that runs the input gate (F-01
//     sanitizeAndFormat + F-07 cancel sequence) and a fan-out ring
//     buffer (last-N readers see the same byte stream; new subscribers
//     resume from the ring's current contents).
//
// The wrapper is intentionally minimal — it does NOT speak NDJSON, run
// a control protocol, observe the cc state machine, or claim to be a
// drop-in replacement for *Session. The daemon spawns BOTH a
// PTYSession (for visual fidelity) AND can later orchestrate cc via
// the JSONL watcher; *Session (NDJSON stdio) remains the orchestration
// surface used by the agent.AgentProvider.

package claude

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/creack/pty"
)

// DefaultPTYRingSize is the byte capacity of the per-PTY fan-out ring.
// 64 KB is enough to cover several screens of TUI output so a late-
// joining attach client sees a recognizable redraw, while keeping the
// per-session memory cost bounded. Ring is overwritten in-place when
// full (drop oldest).
const DefaultPTYRingSize = 64 * 1024

// SpawnMode names which subprocess flavor a caller wants for cc.
//
//   - SpawnModeStdio: the existing NDJSON stream-json mode. Used by the
//     orchestration / control / Recover paths (gh-13).
//   - SpawnModePTY: cc inside a PTY pair, byte-stream output going to
//     the local attach socket. Used by the daemon as the "what the user
//     would see if they were sitting at the terminal" surface.
//
// v0.1 daemon spawns both: a PTY-backed cc for the operator session, and
// independent stdio-backed *Session instances for any orchestration RPCs
// (none yet — listed here for future-compat).
type SpawnMode string

const (
	SpawnModeStdio SpawnMode = "stdio"
	SpawnModePTY   SpawnMode = "pty"
)

// PTYSpawnOpts configures a PTY-backed cc subprocess. Independent from
// SpawnOpts (NDJSON) on purpose — the PTY path doesn't pass
// stream-json flags and accepts a different argv shape (the user's cc
// command-line, often `claude` with no flags).
type PTYSpawnOpts struct {
	// BinaryPath overrides the default "claude" lookup. Tests use
	// fake binaries here.
	BinaryPath string

	// Args are extra argv after the binary name. Empty = launch cc
	// with no flags (its default TUI mode).
	Args []string

	// Cwd is the working directory for the subprocess. Empty = inherit
	// parent. The cc process buckets sessions by cwd; the daemon
	// supplies the ProjectCwd here.
	Cwd string

	// Env merges into the parent env (parent keys win when not in
	// overrides; overrides win for present keys). Nil = inherit
	// wholesale. Same semantics as SpawnOpts.Env.
	Env map[string]string

	// RingSize is the fan-out ring buffer capacity in bytes. Zero =
	// DefaultPTYRingSize.
	RingSize int

	// InitialWinsize is the PTY winsize at start. Zero values for any
	// dimension fall back to a sensible default (80x24).
	InitialWinsize *pty.Winsize
}

// PTYSubprocess is a running cc subprocess attached to a PTY pair.
type PTYSubprocess struct {
	Cmd    *exec.Cmd
	Master *os.File // PTY master (read PTY output, write user input)

	waitOnce sync.Once
	waitErr  error
}

// WaitOnce mirrors Subprocess.WaitOnce — Cmd.Wait is a single-shot
// per os/exec contract.
func (p *PTYSubprocess) WaitOnce() error {
	p.waitOnce.Do(func() {
		p.waitErr = p.Cmd.Wait()
	})
	return p.waitErr
}

// SpawnPTY starts a cc subprocess attached to a fresh PTY pair. The
// caller owns the returned Master FD: read from it for output,
// write to it for input (after passing through sanitizeAndFormat —
// see PTYSession.SendInput). Closing Master triggers the subprocess
// to exit.
//
// On binary-missing returns ErrBinaryNotFound (same as Spawn).
func SpawnPTY(ctx context.Context, opts PTYSpawnOpts) (*PTYSubprocess, error) {
	bin := opts.BinaryPath
	if bin == "" {
		bin = "claude"
	}
	resolved, err := exec.LookPath(bin)
	if err != nil {
		return nil, fmt.Errorf("%w: searched PATH=%q for %q: %v",
			ErrBinaryNotFound, os.Getenv("PATH"), bin, err)
	}

	cmd := exec.CommandContext(ctx, resolved, opts.Args...)
	if opts.Cwd != "" {
		cmd.Dir = opts.Cwd
	}
	if env := effectiveEnv(opts.Env); env != nil {
		cmd.Env = env
	}
	// Setsid so the child PTY becomes a controlling terminal; matches
	// what an interactive shell would do and avoids signal-leak from
	// the parent's controlling tty.
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Setsid:  true,
		Setctty: true,
	}

	winsize := opts.InitialWinsize
	if winsize == nil {
		winsize = &pty.Winsize{Rows: 24, Cols: 80}
	}

	master, err := pty.StartWithSize(cmd, winsize)
	if err != nil {
		return nil, fmt.Errorf("pty.StartWithSize: %w", err)
	}

	return &PTYSubprocess{
		Cmd:    cmd,
		Master: master,
	}, nil
}

// PTYSession ties a PTYSubprocess to:
//
//   - a sanitize-and-format input gate (F-01),
//   - a "cancel in flight" sequence helper (F-07),
//   - a byte-stream ring buffer + fan-out to N subscriber channels.
//
// Lifecycle:
//
//	sess, _ := NewPTYSession(ctx, opts)
//	defer sess.Close()
//	ch := sess.Subscribe()             // late-joiner gets ring contents
//	sess.SendInput([]byte("hi"))        // F-01-sanitized + LF→CR
//	sess.CancelInFlight()               // F-07 — Esc / Ctrl+U
//
// Concurrency: SendInput / Subscribe / Unsubscribe / Close are safe
// from any goroutine. There is exactly one goroutine reading from
// the PTY master (the read loop owned by the PTYSession itself).
type PTYSession struct {
	sub *PTYSubprocess

	mu          sync.Mutex
	ring        *byteRing
	subscribers map[*ptySubscriber]struct{}
	closed      bool

	closeOnce sync.Once
	closeErr  error
	doneCh    chan struct{}
}

// ptySubscriber is one consumer registered via Subscribe. The buffered
// channel decouples slow consumers from the read loop.
type ptySubscriber struct {
	ch      chan []byte
	dropped atomic.Int64
}

// NewPTYSession spawns a PTY-backed cc subprocess and starts the
// read-loop + ring buffer. Returns the wrapper or an error from
// SpawnPTY.
func NewPTYSession(ctx context.Context, opts PTYSpawnOpts) (*PTYSession, error) {
	sub, err := SpawnPTY(ctx, opts)
	if err != nil {
		return nil, err
	}
	ringSize := opts.RingSize
	if ringSize <= 0 {
		ringSize = DefaultPTYRingSize
	}
	s := &PTYSession{
		sub:         sub,
		ring:        newByteRing(ringSize),
		subscribers: make(map[*ptySubscriber]struct{}),
		doneCh:      make(chan struct{}),
	}
	go s.readLoop()
	return s, nil
}

// Master returns the underlying PTY master file. Callers that need
// raw access (e.g. winsize ioctl) reach through here. Most callers
// should NOT read from Master directly — the PTYSession's read loop
// already owns it; doing parallel reads will scramble the byte stream.
func (s *PTYSession) Master() *os.File { return s.sub.Master }

// PID returns the subprocess PID, or 0 if not running.
func (s *PTYSession) PID() int {
	if s.sub.Cmd.Process == nil {
		return 0
	}
	return s.sub.Cmd.Process.Pid
}

// Subscribe returns a channel that delivers PTY output bytes. The
// channel is buffered (4 slots = ~256KB worst case headroom for ring-
// sized chunks) and is closed when Close() runs.
//
// Late joiners ALSO receive the current ring contents on first read
// (delivered as one chunk before any new bytes), so a tether attach
// client connecting mid-session sees the recent screen state.
func (s *PTYSession) Subscribe() <-chan []byte {
	s.mu.Lock()
	defer s.mu.Unlock()

	sub := &ptySubscriber{ch: make(chan []byte, 4)}
	if s.closed {
		close(sub.ch)
		return sub.ch
	}
	// Snapshot the ring as the "catch-up" first chunk.
	snap := s.ring.Snapshot()
	if len(snap) > 0 {
		// Non-blocking; ch has cap 4 so this always fits. If the
		// ring snapshot exceeds 256KB it's been chunked already by
		// Snapshot() into <=64KB pieces but we deliver as one — the
		// receiver's buffer (network) is the natural boundary.
		select {
		case sub.ch <- snap:
		default:
			// Should not happen given freshly-created ch, but treat
			// like any other dropped delivery rather than block.
			sub.dropped.Add(1)
		}
	}
	s.subscribers[sub] = struct{}{}
	return sub.ch
}

// Unsubscribe removes a subscriber registered via Subscribe. The
// channel is closed exactly once. Idempotent.
//
// Callers normally just drain the returned channel to EOF (Close drains
// for them); explicit Unsubscribe is for short-lived attach sessions
// that want to drop without waiting for the daemon shutdown.
func (s *PTYSession) Unsubscribe(ch <-chan []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for sub := range s.subscribers {
		// Channel identity comparison: same underlying chan
		if (<-chan []byte)(sub.ch) == ch {
			delete(s.subscribers, sub)
			close(sub.ch)
			return
		}
	}
}

// SendInput writes bytes to the PTY master, after F-01 sanitization
// (strip ANSI control sequences from caller input + normalize newlines
// from LF to CR — cc TUI in raw mode submits on `\r`, not `\n`).
//
// p must NOT contain partial UTF-8 sequences split across calls —
// caller is responsible for boundary handling. SendInput accepts an
// already-decoded byte slice; the network layer (attach socket) is
// expected to deliver complete chunks.
//
// F-07 cancel-in-flight is NOT routed through here; use
// CancelInFlight which writes the raw cancel sequence directly.
func (s *PTYSession) SendInput(p []byte) error {
	if s.isClosed() {
		return ErrSessionClosed
	}
	clean := sanitizeAndFormat(p)
	if len(clean) == 0 {
		return nil
	}
	_, err := s.sub.Master.Write(clean)
	return err
}

// CancelInFlight writes the F-07 input-cancel sequence to the PTY:
// Ctrl+U (clear current line) followed by Esc (cancel any pending
// composition). Matches what a human pressing Esc in cc would do.
//
// Returns ErrSessionClosed if the session is closed.
func (s *PTYSession) CancelInFlight() error {
	if s.isClosed() {
		return ErrSessionClosed
	}
	// Ctrl+U = 0x15, Esc = 0x1b.
	_, err := s.sub.Master.Write([]byte{0x15, 0x1b})
	return err
}

// Resize updates the PTY winsize. Idempotent on identical inputs.
func (s *PTYSession) Resize(rows, cols uint16) error {
	if s.isClosed() {
		return ErrSessionClosed
	}
	return pty.Setsize(s.sub.Master, &pty.Winsize{Rows: rows, Cols: cols})
}

// Done returns a channel that is closed when the read loop exits
// (subprocess EOF or Close).
func (s *PTYSession) Done() <-chan struct{} { return s.doneCh }

// Close terminates the PTY-backed subprocess and tears down all
// subscriber channels. Safe to call multiple times.
//
// Close ordering matters: closing s.sub.Master makes the read loop
// see EOF, which causes it to exit and close s.doneCh; we then
// SIGTERM the subprocess (PTY close alone does not always reach the
// child — cc traps SIGHUP differently between versions). Wait is
// bounded by 2 seconds, after which we SIGKILL.
func (s *PTYSession) Close() error {
	s.closeOnce.Do(func() {
		s.mu.Lock()
		s.closed = true
		s.mu.Unlock()

		// Shut the master pipe so readLoop hits EOF.
		_ = s.sub.Master.Close()

		// Best-effort SIGTERM, then SIGKILL after CloseTimeout.
		if proc := s.sub.Cmd.Process; proc != nil {
			_ = proc.Signal(syscall.SIGTERM)
		}

		done := make(chan struct{})
		go func() {
			_ = s.sub.WaitOnce()
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			if proc := s.sub.Cmd.Process; proc != nil {
				_ = proc.Kill()
			}
			<-done
		}
		// Wait for read loop to fully drain.
		<-s.doneCh

		// Close all subscriber channels.
		s.mu.Lock()
		for sub := range s.subscribers {
			close(sub.ch)
		}
		s.subscribers = nil
		s.mu.Unlock()
	})
	return s.closeErr
}

func (s *PTYSession) isClosed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closed
}

// readLoop is the single owner of the PTY master read side. Each chunk
// is appended to the ring buffer and fanned out to all current
// subscribers. Exits on master EOF (subprocess died or we closed it).
func (s *PTYSession) readLoop() {
	defer close(s.doneCh)
	buf := make([]byte, 4096)
	for {
		n, err := s.sub.Master.Read(buf)
		if n > 0 {
			chunk := make([]byte, n)
			copy(chunk, buf[:n])
			s.fanout(chunk)
		}
		if err != nil {
			// EOF / closed pipe / read error — all are terminal here.
			// Caller observes via Done() / Subscribe channel close on
			// Close().
			return
		}
	}
}

// fanout appends to ring + sends to all subscribers (non-blocking;
// drop-newest on full subscriber buffer).
func (s *PTYSession) fanout(chunk []byte) {
	s.mu.Lock()
	s.ring.Write(chunk)
	subs := make([]*ptySubscriber, 0, len(s.subscribers))
	for sub := range s.subscribers {
		subs = append(subs, sub)
	}
	s.mu.Unlock()

	for _, sub := range subs {
		select {
		case sub.ch <- chunk:
		default:
			sub.dropped.Add(1)
		}
	}
}

// --- byteRing ------------------------------------------------------------

// byteRing is a fixed-capacity in-memory ring of bytes. Last-N writers
// preserved; older content overwritten in place when full. Snapshot
// returns a coherent copy for late-joiners.
//
// Not exported — callers reach the ring exclusively through
// PTYSession.Subscribe.
type byteRing struct {
	cap  int
	buf  []byte
	w    int  // next write index in buf
	full bool // whether we've wrapped at least once
}

func newByteRing(cap int) *byteRing {
	return &byteRing{cap: cap, buf: make([]byte, cap)}
}

// Write appends p, overwriting oldest bytes when at capacity.
func (r *byteRing) Write(p []byte) {
	for len(p) > 0 {
		n := copy(r.buf[r.w:], p)
		r.w += n
		if r.w >= r.cap {
			r.w = 0
			r.full = true
		}
		p = p[n:]
	}
}

// Snapshot returns a copy of the ring's current contents in
// chronological order (oldest first). Empty when nothing has been
// written.
func (r *byteRing) Snapshot() []byte {
	if !r.full {
		out := make([]byte, r.w)
		copy(out, r.buf[:r.w])
		return out
	}
	out := make([]byte, r.cap)
	n := copy(out, r.buf[r.w:])
	copy(out[n:], r.buf[:r.w])
	return out
}

// Len reports the current number of bytes stored (≤ cap).
func (r *byteRing) Len() int {
	if r.full {
		return r.cap
	}
	return r.w
}

// --- F-01 input sanitization -------------------------------------------

// sanitizeAndFormat strips ANSI escape sequences (CSI / OSC) from caller
// input and normalizes newlines from LF to CR (cc TUI in raw mode
// submits on `\r`, not `\n` — F-01).
//
// Pure function. Does NOT strip raw control bytes other than the ANSI
// escape introducer; legitimate Ctrl+C / Ctrl+U / Esc bytes are
// preserved (callers reach those via dedicated helpers like
// CancelInFlight).
//
// Implementation notes:
//
//   - ANSI CSI ("\x1b[...<final>") is stripped — fragments where final
//     byte never arrives are dropped (defensive; caller should send
//     complete sequences).
//   - ANSI OSC ("\x1b]...BEL or ST") is stripped similarly.
//   - LF (0x0a) → CR (0x0d).
//   - CRLF collapses to a single CR.
//   - All other bytes pass through.
func sanitizeAndFormat(p []byte) []byte {
	out := make([]byte, 0, len(p))
	for i := 0; i < len(p); i++ {
		b := p[i]
		// ESC introducer: try to skip a CSI / OSC sequence.
		if b == 0x1b && i+1 < len(p) {
			next := p[i+1]
			switch next {
			case '[':
				// CSI: ESC [ params final
				j := i + 2
				for j < len(p) {
					c := p[j]
					if c >= 0x40 && c <= 0x7e {
						j++ // include final byte in skip
						break
					}
					j++
				}
				i = j - 1
				continue
			case ']':
				// OSC: ESC ] payload (BEL or ESC \).
				j := i + 2
				for j < len(p) {
					c := p[j]
					if c == 0x07 { // BEL
						j++
						break
					}
					if c == 0x1b && j+1 < len(p) && p[j+1] == '\\' { // ST
						j += 2
						break
					}
					j++
				}
				i = j - 1
				continue
			}
			// Bare ESC stays — that's the F-07 cancel char path.
		}
		// CRLF → CR (consume LF after CR).
		if b == 0x0d && i+1 < len(p) && p[i+1] == 0x0a {
			out = append(out, 0x0d)
			i++
			continue
		}
		// LF → CR (F-01).
		if b == 0x0a {
			out = append(out, 0x0d)
			continue
		}
		out = append(out, b)
	}
	return out
}

// io.Closer guard.
var _ io.Closer = (*PTYSession)(nil)

// errPtyClosed is unused; preserved as named sentinel in case a future
// caller needs to distinguish PTY-specific close from session-wide.
var errPtyClosed = errors.New("claude: PTY session closed")

// silence unused warning under go vet for the named sentinel above.
var _ = errPtyClosed
