package server

import (
	"context"
	"errors"
	"io"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/creack/pty"
)

// tether#121 C1 — what a /wt/shell connection leaves behind when it ends.
//
// # The quantity these tests measure, and why it is the certain one
//
// The leak was three things at once (PTY master fd, the WT→PTY pump goroutine,
// the WebTransport session), and the load-bearing assertion here is the FD — but
// not as a count. Counting /proc/self/fd around the call would be a global
// measurement in a process where the Go runtime, the netpoller and every other
// test's leftovers open and close descriptors, so a delta of zero would prove
// nothing and a delta of one could come from anywhere.
//
// Instead the fd is measured per-object: after pumpShell returns, writing to the
// PTY master must report os.ErrClosed. That answer comes from os.File's OWN state
// — the os package checks the file's closed flag and returns ErrClosed before it
// issues a syscall — so it is a two-valued fact about the exact descriptor this
// shell was given, decided at the moment pumpShell returns. No polling, no
// sleeping, no shared counter. An fd that is still open answers differently
// (EIO from the kernel, once the slave is gone), so the two outcomes cannot be
// confused for one another.
//
// The goroutine is measured the same way rather than with runtime.NumGoroutine:
// the pump's only blocking call is stream.Read, so "Read returned" is necessary
// and sufficient for that goroutine to run to completion. fakeWTStream records
// it.

// fakeWTStream stands in for *webtransport.Stream, and it is written to encode
// two facts about webtransport-go v0.10.0 rather than to be convenient. Both were
// re-verified in the module cache for this fix:
//
//   - Stream.Close() closes the SEND direction only. stream.go:403-405 says it in
//     as many words — "Close closes the send-direction of the stream. It does not
//     close the receive-direction" — implemented as `return s.sendStr.Close()`.
//     So Close() here must NOT wake a blocked Read.
//   - Destroying the SESSION is what wakes a blocked Read. Server.Upgrade builds
//     the session with context.WithoutCancel(r.Context()) (server.go:379) over the
//     hijacked CONNECT stream it takes from http3.HTTPStreamer (server.go:366), so
//     neither the handler returning nor http3 ends it. Only CloseWithError does.
//
// A fake that woke Read on Close() would make the buggy code pass and the leak
// untestable, which is the reason this comment is longer than the type.
type fakeWTStream struct {
	sessionGone chan struct{}

	closeSessionOnce sync.Once
	readReturnedOnce sync.Once
	// readReturned is closed the first time a blocked Read gives up. The WT→PTY
	// pump does nothing else that can block, so this is its liveness probe.
	readReturned chan struct{}

	mu         sync.Mutex
	sendClosed bool
	out        []byte
}

func newFakeWTStream() *fakeWTStream {
	return &fakeWTStream{
		sessionGone:  make(chan struct{}),
		readReturned: make(chan struct{}),
	}
}

func (f *fakeWTStream) Read([]byte) (int, error) {
	<-f.sessionGone
	f.readReturnedOnce.Do(func() { close(f.readReturned) })
	return 0, io.EOF
}

func (f *fakeWTStream) Write(p []byte) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.out = append(f.out, p...)
	return len(p), nil
}

// Close is the send direction and nothing else — see the type's doc.
func (f *fakeWTStream) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sendClosed = true
	return nil
}

func (f *fakeWTStream) sendSideClosed() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.sendClosed
}

// killSession models webtransport.Session.CloseWithError: the session goes away
// and every stream on it stops delivering.
func (f *fakeWTStream) killSession() {
	f.closeSessionOnce.Do(func() { close(f.sessionGone) })
}

type fakeWTSession struct {
	stream *fakeWTStream
	closes atomic.Int32
}

func (s *fakeWTSession) closeWithError() {
	s.closes.Add(1)
	s.stream.killSession()
}

// shellUnderTest wires up one shell's worth of real PTY plus fake transport.
// The PTY is REAL (pty.Open, the same package handleWTShell starts one with) so
// that the fd assertion is about a kernel descriptor and not about a mock that
// counted a method call.
func shellUnderTest(t *testing.T) (ptmx, tty *os.File, stream *fakeWTStream, sess *fakeWTSession) {
	t.Helper()
	ptmx, tty, err := pty.Open()
	if err != nil {
		t.Fatalf("pty.Open: %v", err)
	}
	t.Cleanup(func() {
		_ = ptmx.Close()
		_ = tty.Close()
	})
	stream = newFakeWTStream()
	return ptmx, tty, stream, &fakeWTSession{stream: stream}
}

func runPumpShell(t *testing.T, ctx context.Context, stream *fakeWTStream, ptmx *os.File, sess *fakeWTSession) (returned <-chan struct{}) {
	t.Helper()
	ch := make(chan struct{})
	go func() {
		defer close(ch)
		pumpShell(ctx, stream, ptmx, func() error { return nil }, sess.closeWithError)
	}()
	return ch
}

// assertShellFullyTornDown is the whole of C1, stated once.
func assertShellFullyTornDown(t *testing.T, ptmx *os.File, stream *fakeWTStream, sess *fakeWTSession) {
	t.Helper()

	// (a) the PTY master fd. os.ErrClosed comes from os.File's own state, so this
	// is a fact about THIS descriptor and not about a global count. A still-open
	// master answers with EIO instead, which is a different error entirely.
	_, err := ptmx.Write([]byte("x"))
	if !errors.Is(err, os.ErrClosed) {
		t.Errorf("writing to the PTY master after teardown returned %v, want os.ErrClosed:\n"+
			"the master fd was never closed, so every shell a tab opens costs the daemon one\n"+
			"descriptor for as long as that tab holds its QUIC connection", err)
	}

	// (c) the WebTransport session. Not asserted as exactly-one: CloseWithError is
	// idempotent by construction (session.go:397 returns early when it was not the
	// first caller), so a belt-and-braces second call is harmless and the fact
	// worth pinning is that SOMEBODY closed it.
	if n := sess.closes.Load(); n < 1 {
		t.Errorf("the WebTransport session was closed %d times, want at least once:\n"+
			"Upgrade decouples the session from the request context on purpose, so a session\n"+
			"nobody closes stays registered with the webtransport server until the browser\n"+
			"drops the whole QUIC connection", n)
	}

	// (b) the WT→PTY pump. Bounded wait rather than a sleep: the pass condition is
	// a channel close on the pump's own code path, so a green run has observed the
	// pump finish, and the timeout only bounds how long a red run takes to admit it.
	select {
	case <-stream.readReturned:
	case <-time.After(5 * time.Second):
		t.Errorf("the WT→PTY pump is still parked in stream.Read:\n" +
			"Stream.Close() closes the send direction only, so closing the stream cannot wake\n" +
			"it — one leaked goroutine, holding the PTY master alive with it, per shell opened")
	}

	if !stream.sendSideClosed() {
		t.Error("the stream's send side was never closed")
	}
}

// TestPumpShell_PTYExitTearsEverythingDown is the ordinary end of a shell: the
// user types `exit`, or cc quits on its own. Before tether#121 this was the one
// exit that closed NOTHING — the `case <-done:` arm of the select had an empty
// body — so it is the case the leak was reported against.
func TestPumpShell_PTYExitTearsEverythingDown(t *testing.T) {
	ptmx, tty, stream, sess := shellUnderTest(t)

	// Closing the slave is what the kernel sees when the last process holding it
	// exits, and it is what makes reads on the master fail. That failure is the
	// exact edge that ends the PTY→WT pump and closes pumpShell's `done`.
	if err := tty.Close(); err != nil {
		t.Fatalf("close the PTY slave: %v", err)
	}

	select {
	case <-runPumpShell(t, context.Background(), stream, ptmx, sess):
	case <-time.After(10 * time.Second):
		t.Fatal("pumpShell never returned after the PTY exited")
	}

	assertShellFullyTornDown(t, ptmx, stream, sess)
}

// TestPumpShell_ClientDisconnectTearsEverythingDown is the other exit: the
// browser tab closed, so the WebTransport session's context fired while the PTY
// was still alive. This path already closed the PTY before tether#121; it is here
// because the same teardown now has to serve both arms, and a fix that only
// covered the arm in the bug report would leave the two out of step again.
func TestPumpShell_ClientDisconnectTearsEverythingDown(t *testing.T) {
	// tty stays OPEN: nothing has exited, so reads on the master block and `done`
	// never fires.
	ptmx, _, stream, sess := shellUnderTest(t)

	ctx, cancel := context.WithCancel(context.Background())
	returned := runPumpShell(t, ctx, stream, ptmx, sess)

	// With both a live PTY and a live client there is no exit to take. This is the
	// behavioural half of "the 60-second guillotine is gone" — see
	// TestShellLifetimeHasNoClock for the half that can actually prove it.
	select {
	case <-returned:
		t.Fatal("pumpShell returned with the PTY alive and the client still connected")
	case <-time.After(200 * time.Millisecond):
	}

	cancel()
	select {
	case <-returned:
	case <-time.After(10 * time.Second):
		t.Fatal("pumpShell never returned after the client disconnected")
	}

	assertShellFullyTornDown(t, ptmx, stream, sess)
}
