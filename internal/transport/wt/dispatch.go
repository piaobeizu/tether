// dispatch.go — events-channel envelope dispatcher (slice #3).
//
// The dispatcher is the bridge between the daemon's per-session
// LocalEnvelope source (an `agent.EnvelopeEmitter` subscription) and
// the WT events channel (channel-id 0x02). Per-session lifetime:
//
//	1. WT session is accepted (Server.handleUpgrade)
//	2. Operator code calls PushEnvelopeStream(ctx, sess, in, opts)
//	3. PushEnvelopeStream opens the events bidi (server side via
//	   sess.Events) and loops forever:
//	     a. read one LocalEnvelope from `in`
//	     b. JSON-marshal it as the inner plaintext
//	     c. Seal into a WireEnvelope (random nonce + AD-bound kind)
//	     d. WriteFrame on the events stream
//	   On stream close OR ctx cancel OR `in` close, the loop exits.
//
// Errors that should kill the session (write fails after established)
// are surfaced via the returned error; errors that can be tolerated
// (single envelope marshal/seal failure) are logged and skipped — the
// daemon must NOT die because one bad payload arrived from cc.
//
// Key management — see DevSharedKey in envelope.go. Slice #3 hardcodes
// the key; slice #4 (pairing) replaces it with a per-session ECDH-
// negotiated value.

package wt

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"sync/atomic"
)

// LocalEnvelopeShape is the duck-typed shape PushEnvelopeStream consumes.
// Defined here as a generic JSON-marshalable interface so `transport/wt`
// does not have to import `internal/agent` (avoids the dependency
// inversion — agent already imports transport/wt for client side; the
// reverse import would risk a cycle).
//
// Any type with a `kind` JSON field whose marshaled form is the inner
// plaintext for a §3.3.2 op satisfies this. `agent.LocalEnvelope` is
// the canonical implementer.
type LocalEnvelopeShape interface {
	// EnvelopeKind returns the §3.3.2 op kind (e.g. "output.agent-event").
	// Used as the AD-bound `kind` of the wire envelope.
	EnvelopeKind() string

	// EnvelopeSessionID returns the cc session id (mixed into AD).
	EnvelopeSessionID() string
}

// PushEnvelopeOptions configures a PushEnvelopeStream call.
type PushEnvelopeOptions struct {
	// SharedKey is the 32-byte AEAD key. Use DevSharedKey[:] in v0.1
	// dev / smoke-test paths until slice #4 lands.
	SharedKey []byte

	// FromDeviceID / ToDeviceID — routing IDs stamped into every
	// envelope this stream emits. Both required.
	FromDeviceID string
	ToDeviceID   string

	// Logger for skip-and-continue events (single bad envelope). nil
	// → discard. Hard errors (stream write failure) are still
	// returned; the logger only sees recoverable per-envelope skips.
	Logger *log.Logger
}

// EventsWriter is the subset of *webtransport.Stream PushEnvelopeStream
// needs. Defined as an interface so tests can inject an io.Writer-only
// stub; in production the caller passes the result of Session.Events.
type EventsWriter interface {
	io.Writer
	io.Closer
}

// PushEnvelopeStream subscribes to envelopes from `in`, seals each as
// a WireEnvelope, and pushes them down `events`. Blocks until ctx is
// cancelled, `in` is closed, OR a stream-write error occurs.
//
// Returns nil for clean exits (ctx done, in closed); returns the
// underlying error for stream-write failures. Per-envelope marshal /
// seal errors are logged + skipped (the cc side may produce a
// pathological payload now and then; one bad envelope must not kill
// the session).
func PushEnvelopeStream[T LocalEnvelopeShape](ctx context.Context, events EventsWriter, in <-chan T, opts PushEnvelopeOptions) error {
	if events == nil {
		return fmt.Errorf("%w: nil events writer", ErrWireEnvelope)
	}
	if len(opts.SharedKey) != SharedKeySize {
		return fmt.Errorf("%w: shared key must be %d bytes (got %d)", ErrWireEnvelope, SharedKeySize, len(opts.SharedKey))
	}
	if opts.FromDeviceID == "" || opts.ToDeviceID == "" {
		return fmt.Errorf("%w: fromDeviceId and toDeviceId required", ErrWireEnvelope)
	}
	logger := opts.Logger
	if logger == nil {
		logger = log.New(io.Discard, "", 0)
	}

	for {
		select {
		case <-ctx.Done():
			return nil
		case env, ok := <-in:
			if !ok {
				// Subscriber channel closed by the upstream emitter.
				return nil
			}
			plaintext, err := json.Marshal(env)
			if err != nil {
				logger.Printf("wt: dispatch: marshal envelope: %v (skip)", err)
				continue
			}
			wire, err := Seal(SealOptions{
				SharedKey:    opts.SharedKey,
				FromDeviceID: opts.FromDeviceID,
				ToDeviceID:   opts.ToDeviceID,
				Kind:         env.EnvelopeKind(),
				Plaintext:    plaintext,
				SessionID:    env.EnvelopeSessionID(),
			})
			if err != nil {
				logger.Printf("wt: dispatch: seal envelope (kind=%s): %v (skip)", env.EnvelopeKind(), err)
				continue
			}
			if _, err := WriteFrame(events, wire); err != nil {
				// Stream write failures are session-fatal — the peer is
				// gone or the QUIC layer is broken. Returning the error
				// signals the caller to tear the session down.
				if errors.Is(err, io.EOF) || errors.Is(err, io.ErrClosedPipe) || errors.Is(err, context.Canceled) {
					return nil
				}
				return fmt.Errorf("wt: dispatch: write frame: %w", err)
			}
		}
	}
}

// EnvelopeFrameReader is the receiver-side helper: pulls
// length-prefixed frames off a stream, decrypts each, and returns the
// inner plaintext bytes (caller json-unmarshals into the appropriate
// type). Used by the Go-side test client + by future internal Go
// callers; the Tauri / mobile client re-implements this loop in Rust.
//
// Concurrency-safe: a single FrameReader may be Read by exactly one
// goroutine at a time. Multiple FrameReaders against the same stream
// is undefined.
type EnvelopeFrameReader struct {
	r         io.Reader
	sharedKey []byte
	sessionID string
	closed    atomic.Bool
}

// NewEnvelopeFrameReader wires a reader. `sessionID` is the cc session
// id used in the AD construction (must match what the sender uses).
// Pass empty string if AD only binds device IDs.
func NewEnvelopeFrameReader(r io.Reader, sharedKey []byte, sessionID string) (*EnvelopeFrameReader, error) {
	if r == nil {
		return nil, fmt.Errorf("%w: nil reader", ErrWireEnvelope)
	}
	if len(sharedKey) != SharedKeySize {
		return nil, fmt.Errorf("%w: shared key must be %d bytes (got %d)", ErrWireEnvelope, SharedKeySize, len(sharedKey))
	}
	keyCopy := make([]byte, SharedKeySize)
	copy(keyCopy, sharedKey)
	return &EnvelopeFrameReader{r: r, sharedKey: keyCopy, sessionID: sessionID}, nil
}

// Next blocks reading one frame, decrypts the WireEnvelope, and
// returns the (envelope-metadata, inner-plaintext) pair. Returns
// io.EOF cleanly when the stream is closed at a frame boundary.
//
// AEAD failures, malformed frames, and unsupported keyVersion all
// return an error (wrapped ErrWireEnvelope). Caller policy:
//
//   - hard error path: tear the session down (suggests the peer is
//     compromised or the AD construction is out of sync).
//   - soft error path: log + Next() again — but the `n + 1`th frame
//     starts wherever the underlying stream cursor landed, which is
//     undefined after a malformed length-prefix. Default to hard.
func (er *EnvelopeFrameReader) Next() (*WireEnvelope, []byte, error) {
	if er.closed.Load() {
		return nil, nil, io.ErrClosedPipe
	}
	env, err := ReadFrame(er.r)
	if err != nil {
		return nil, nil, err
	}
	pt, err := Open(env, OpenOptions{SharedKey: er.sharedKey, SessionID: er.sessionID})
	if err != nil {
		return env, nil, err
	}
	return env, pt, nil
}

// Close marks the reader as done. Subsequent Next() returns
// io.ErrClosedPipe. Does NOT close the underlying io.Reader — that
// remains the caller's responsibility.
func (er *EnvelopeFrameReader) Close() error {
	er.closed.Store(true)
	return nil
}
