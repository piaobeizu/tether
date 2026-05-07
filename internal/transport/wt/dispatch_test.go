package wt

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeLocalEnvelope is a minimal LocalEnvelopeShape for tests — avoids
// importing internal/agent (which imports this package via the
// daemon).
type fakeLocalEnvelope struct {
	Kind      string `json:"kind"`
	SessionID string `json:"sessionId"`
	Body      string `json:"body"`
}

func (f fakeLocalEnvelope) EnvelopeKind() string      { return f.Kind }
func (f fakeLocalEnvelope) EnvelopeSessionID() string { return f.SessionID }

// blockingWriter is an io.Writer + io.Closer wrapping a bytes.Buffer.
// Tests use it to capture frames written by PushEnvelopeStream.
type blockingWriter struct {
	mu  sync.Mutex
	buf bytes.Buffer
	// closed signals "no more writes" — Write returns io.ErrClosedPipe.
	closed bool
}

func (w *blockingWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return 0, io.ErrClosedPipe
	}
	return w.buf.Write(p)
}

func (w *blockingWriter) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.closed = true
	return nil
}

func (w *blockingWriter) Bytes() []byte {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]byte(nil), w.buf.Bytes()...)
}

// TestPushEnvelopeStream_DeliversAndSeals — the core smoke. Push 3
// envelopes via the dispatcher; verify the bytes on the wire decode +
// AEAD-open back to the originals.
func TestPushEnvelopeStream_DeliversAndSeals(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	in := make(chan fakeLocalEnvelope, 3)
	in <- fakeLocalEnvelope{Kind: "output.agent-event", SessionID: "sess-A", Body: "hello-1"}
	in <- fakeLocalEnvelope{Kind: "output.agent-event", SessionID: "sess-A", Body: "hello-2"}
	in <- fakeLocalEnvelope{Kind: "output.hook-event", SessionID: "sess-A", Body: "hello-3"}
	close(in)

	w := &blockingWriter{}
	err := PushEnvelopeStream(ctx, w, in, PushEnvelopeOptions{
		SharedKey:    DevSharedKey[:],
		FromDeviceID: "device-cli-1",
		ToDeviceID:   "device-app-2",
	})
	if err != nil {
		t.Fatalf("PushEnvelopeStream: %v", err)
	}

	// Decode the framed output.
	r := bytes.NewReader(w.Bytes())
	want := []string{"hello-1", "hello-2", "hello-3"}
	wantKinds := []string{"output.agent-event", "output.agent-event", "output.hook-event"}
	for i := 0; i < 3; i++ {
		env, err := ReadFrame(r)
		if err != nil {
			t.Fatalf("ReadFrame[%d]: %v", i, err)
		}
		if env.Kind != wantKinds[i] {
			t.Errorf("frame[%d] Kind=%q want %q", i, env.Kind, wantKinds[i])
		}
		if env.FromDeviceID != "device-cli-1" || env.ToDeviceID != "device-app-2" {
			t.Errorf("frame[%d] device IDs wrong: from=%q to=%q", i, env.FromDeviceID, env.ToDeviceID)
		}

		pt, err := Open(env, OpenOptions{SharedKey: DevSharedKey[:], SessionID: "sess-A"})
		if err != nil {
			t.Fatalf("frame[%d] Open: %v", i, err)
		}
		var got fakeLocalEnvelope
		if err := json.Unmarshal(pt, &got); err != nil {
			t.Fatalf("frame[%d] inner unmarshal: %v", i, err)
		}
		if got.Body != want[i] {
			t.Errorf("frame[%d] body=%q want %q", i, got.Body, want[i])
		}
	}
	// Stream should be at EOF now.
	if _, err := ReadFrame(r); !errors.Is(err, io.EOF) {
		t.Errorf("expected EOF after 3 frames, got %v", err)
	}
}

// TestPushEnvelopeStream_ContextCancelExits — ctx cancel terminates
// the loop cleanly (returns nil).
func TestPushEnvelopeStream_ContextCancelExits(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())

	in := make(chan fakeLocalEnvelope) // never sends
	w := &blockingWriter{}

	done := make(chan error, 1)
	go func() {
		done <- PushEnvelopeStream(ctx, w, in, PushEnvelopeOptions{
			SharedKey:    DevSharedKey[:],
			FromDeviceID: "from",
			ToDeviceID:   "to",
		})
	}()

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("expected nil on ctx cancel, got %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("PushEnvelopeStream did not exit on ctx cancel")
	}
}

// TestPushEnvelopeStream_WriteErrorPropagates — once the events
// stream is closed under us, the dispatcher returns the write error.
func TestPushEnvelopeStream_WriteErrorPropagates(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	in := make(chan fakeLocalEnvelope, 1)
	in <- fakeLocalEnvelope{Kind: "output.agent-event", SessionID: "sess", Body: "x"}
	// brokenWriter returns a non-clean-close error (io.ErrShortWrite) so
	// PushEnvelopeStream's classifier (R1 #4) treats it as session-fatal
	// and returns the wrapped error to the caller. io.EOF / ErrClosedPipe
	// / ctx.Canceled are now intentionally swallowed as clean shutdown
	// signals — the regression bar for this test is "non-clean errors
	// still propagate".
	w := &brokenWriter{err: io.ErrShortWrite}

	err := PushEnvelopeStream(ctx, w, in, PushEnvelopeOptions{
		SharedKey:    DevSharedKey[:],
		FromDeviceID: "from",
		ToDeviceID:   "to",
	})
	if err == nil {
		t.Fatal("expected write error")
	}
	if !strings.Contains(err.Error(), "write frame") {
		t.Errorf("expected 'write frame' in error, got %v", err)
	}
	if !errors.Is(err, io.ErrShortWrite) {
		t.Errorf("expected errors.Is(err, io.ErrShortWrite), got %v", err)
	}
}

// TestPushEnvelopeStream_CleanCloseSwallowed pins the R1 #4 fix from
// the dispatcher's perspective: when WriteFrame fails with io.EOF /
// io.ErrClosedPipe / context.Canceled (a clean peer disconnect), the
// dispatcher must return nil instead of treating it as a session-fatal
// error. Pre-fix WriteFrame wrapped the inner error with %v so
// errors.Is never matched and the dispatcher returned a noisy error.
func TestPushEnvelopeStream_CleanCloseSwallowed(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		err  error
	}{
		{"eof", io.EOF},
		{"closed-pipe", io.ErrClosedPipe},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			in := make(chan fakeLocalEnvelope, 1)
			in <- fakeLocalEnvelope{Kind: "output.agent-event", SessionID: "sess", Body: "x"}
			w := &brokenWriter{err: tc.err}
			err := PushEnvelopeStream(ctx, w, in, PushEnvelopeOptions{
				SharedKey:    DevSharedKey[:],
				FromDeviceID: "from",
				ToDeviceID:   "to",
			})
			if err != nil {
				t.Fatalf("PushEnvelopeStream returned %v on clean peer-close error %v; want nil (R1 #4)", err, tc.err)
			}
		})
	}
}

// brokenWriter is an EventsWriter that always fails Write with a
// caller-supplied error.
type brokenWriter struct{ err error }

func (b *brokenWriter) Write(_ []byte) (int, error) { return 0, b.err }
func (b *brokenWriter) Close() error                { return nil }

// TestEnvelopeFrameReader_RoundTrip — Reader-side helper consumes a
// framed stream produced by PushEnvelopeStream + decrypts each.
func TestEnvelopeFrameReader_RoundTrip(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	in := make(chan fakeLocalEnvelope, 2)
	in <- fakeLocalEnvelope{Kind: "output.agent-event", SessionID: "sess", Body: "one"}
	in <- fakeLocalEnvelope{Kind: "output.agent-event", SessionID: "sess", Body: "two"}
	close(in)

	w := &blockingWriter{}
	if err := PushEnvelopeStream(ctx, w, in, PushEnvelopeOptions{
		SharedKey:    DevSharedKey[:],
		FromDeviceID: "from",
		ToDeviceID:   "to",
	}); err != nil {
		t.Fatalf("Push: %v", err)
	}

	er, err := NewEnvelopeFrameReader(bytes.NewReader(w.Bytes()), DevSharedKey[:], "sess")
	if err != nil {
		t.Fatalf("NewEnvelopeFrameReader: %v", err)
	}
	got := make([]string, 0, 2)
	for {
		env, pt, err := er.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
		if env.Kind != "output.agent-event" {
			t.Errorf("kind=%q", env.Kind)
		}
		var inner fakeLocalEnvelope
		if err := json.Unmarshal(pt, &inner); err != nil {
			t.Fatalf("inner unmarshal: %v", err)
		}
		got = append(got, inner.Body)
	}
	if len(got) != 2 || got[0] != "one" || got[1] != "two" {
		t.Errorf("got %v want [one two]", got)
	}
}
