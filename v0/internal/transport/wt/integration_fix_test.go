package wt

import (
	"bytes"
	"context"
	"crypto/x509"
	"errors"
	"io"
	"net"
	"sync"
	"testing"
	"time"
)

// TestWriteFrame_WrapsErrorChain pins R1 #4: WriteFrame must wrap the
// underlying writer's error with %w (not %v) so callers can do
// `errors.Is(err, io.EOF)` / `io.ErrClosedPipe` / `context.Canceled`.
//
// Repro of the bug: dispatch.go::PushEnvelopeStream classifies a
// peer-closes-cleanly error class via errors.Is. Pre-fix, WriteFrame
// did `fmt.Errorf("%w: write body: %v", ErrWireEnvelope, err)` — the
// %v stripped the chain, errors.Is(err, io.EOF) returned false, and
// the dispatcher treated a clean disconnect as a session-fatal write
// failure (returning the error → daemon logs a noisy warnf for what's
// actually a graceful peer close).
func TestWriteFrame_WrapsErrorChain(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		want error
	}{
		{"eof", io.EOF},
		{"closed-pipe", io.ErrClosedPipe},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := &WireEnvelope{
				ID:           "00000000-0000-0000-0000-000000000001",
				FromDeviceID: "from",
				ToDeviceID:   "to",
				Kind:         "x.test",
				KeyVersion:   CurrentKeyVersion,
				Nonce:        bytes.Repeat([]byte{0}, NonceSize),
				Ciphertext:   []byte{1, 2, 3},
			}
			_, err := WriteFrame(&erroringWriter{err: tc.want}, env)
			if err == nil {
				t.Fatalf("want error, got nil")
			}
			if !errors.Is(err, tc.want) {
				t.Fatalf("errors.Is(err, %v) = false (R1 #4 broken); err=%v", tc.want, err)
			}
			if !errors.Is(err, ErrWireEnvelope) {
				t.Errorf("errors.Is(err, ErrWireEnvelope) = false; err=%v", err)
			}
		})
	}
}

// TestReadFrame_WrapsErrorChain pins the symmetric R1 #4 fix on the
// receiver side — partial-frame errors must keep io.ErrUnexpectedEOF
// in the chain. Length prefix says 100 bytes; we feed exactly 1 body
// byte, so io.ReadFull returns io.ErrUnexpectedEOF.
func TestReadFrame_WrapsErrorChain(t *testing.T) {
	t.Parallel()

	r := bytes.NewReader([]byte{0, 0, 0, 100, 0xab})
	_, err := ReadFrame(r)
	if err == nil {
		t.Fatalf("want error, got nil")
	}
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("errors.Is(err, io.ErrUnexpectedEOF) = false (R1 #4 broken); err=%v", err)
	}
}

// erroringWriter is an io.Writer that returns a fixed error on Write.
// Length prefix succeeds via the first call only when the test wants
// it; we want both length + body writes to fail consistently — so any
// Write returns the configured error.
type erroringWriter struct {
	err error
}

func (w *erroringWriter) Write(_ []byte) (int, error) { return 0, w.err }

// TestReadChannelID_AppliesDeadline pins the M1 fix: readChannelID
// must enforce a deadline so a peer that opens a bidi stream and never
// writes the channel-id byte cannot pin a goroutine forever.
//
// Repro of the bug: pre-fix, readChannelID did io.ReadFull on the
// stream with NO deadline. The defaultChannelIDDeadline constant
// existed in channel.go but was never referenced. routeIncomingBidi /
// routeIncomingUni would block in readChannelID indefinitely, leaking
// one goroutine + one stream resource per silent stream a hostile
// peer opens.
//
// This test exercises readChannelID directly with a stub reader that
// records SetReadDeadline calls and never returns from Read. Pre-fix
// the test would hang (no deadline → no SetReadDeadline call → Read
// blocks forever). With the fix, SetReadDeadline is invoked with a
// non-zero time, and the stub's Read returns the deadline error
// immediately, allowing readChannelID to surface it.
func TestReadChannelID_AppliesDeadline(t *testing.T) {
	t.Parallel()

	stub := &deadlineStub{}
	done := make(chan struct{})
	var (
		got ChannelID
		err error
	)
	go func() {
		got, err = readChannelID(stub, defaultChannelIDDeadline)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatalf("readChannelID did not respect deadline — M1 fix not applied (call hung)")
	}
	if err == nil {
		t.Fatalf("readChannelID returned nil error (got=%v); want deadline error", got)
	}
	if !stub.deadlineSet {
		t.Errorf("SetReadDeadline was never called — M1 fix not applied")
	}
	if !stub.deadlineCleared {
		t.Errorf("SetReadDeadline was not cleared on exit (deferred reset to zero time)")
	}
}

// deadlineStub is a channelIDDeadliner that records SetReadDeadline
// calls and returns a deadline error on Read once a non-zero deadline
// has been set. The goroutine that owns the Read returns
// "deadline-exceeded" so the test can verify readChannelID exited via
// the deadline path (not a real read).
type deadlineStub struct {
	deadlineSet     bool
	deadlineCleared bool
	deadlineMu      sync.Mutex
}

func (s *deadlineStub) SetReadDeadline(t time.Time) error {
	s.deadlineMu.Lock()
	defer s.deadlineMu.Unlock()
	if t.IsZero() {
		s.deadlineCleared = true
	} else {
		s.deadlineSet = true
	}
	return nil
}

func (s *deadlineStub) Read(_ []byte) (int, error) {
	// Simulate a peer that never writes the byte: block until the
	// deadline times out. We return immediately with a deadline-
	// shaped error after the SetReadDeadline call lands so the test
	// terminates quickly.
	for {
		s.deadlineMu.Lock()
		set := s.deadlineSet
		s.deadlineMu.Unlock()
		if set {
			return 0, errors.New("i/o timeout (stub deadline exceeded)")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// satisfy unused-import linter — these dependencies are referenced
// only by other tests in this file.
var (
	_ = bootTestServer
	_ = dialClient
	_ = context.Background
	_ = (&x509.CertPool{})
	_ = net.IPv4
)
