package pair

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
)

// wireFormat: one WireEnvelope per line. ciphertext is base64url-no-pad
// encoded as a string field. This is the seam the daemon's WT control
// channel handler will plug into — at that layer the envelope is
// already framed by the WT-channel codec, so wire.go's job is only to
// (de)serialize the envelope struct.
//
// We keep our own minimal codec rather than reusing internal/transport/wt
// directly so the pair package has zero coupling to the transport
// (the spec calls out that the io.ReadWriter parameter is the seam).

type wireEnvelopeJSON struct {
	Kind       FrameKind `json:"kind"`
	KeyVersion int       `json:"keyVersion"`
	// Ciphertext is base64url-no-pad encoded so the wire is a single
	// line of plain JSON.
	Ciphertext string `json:"ciphertext"`
	TS         int64  `json:"ts"`
}

func encodeEnvelope(env WireEnvelope) ([]byte, error) {
	row := wireEnvelopeJSON{
		Kind:       env.Kind,
		KeyVersion: env.KeyVersion,
		Ciphertext: base64.RawURLEncoding.EncodeToString(env.Ciphertext),
		TS:         env.TS,
	}
	b, err := json.Marshal(row)
	if err != nil {
		return nil, fmt.Errorf("pair: marshal envelope: %w", err)
	}
	return append(b, '\n'), nil
}

func decodeEnvelope(line []byte) (WireEnvelope, error) {
	var row wireEnvelopeJSON
	if err := json.Unmarshal(line, &row); err != nil {
		return WireEnvelope{}, fmt.Errorf("pair: unmarshal envelope: %w", err)
	}
	body, err := base64.RawURLEncoding.DecodeString(row.Ciphertext)
	if err != nil {
		return WireEnvelope{}, fmt.Errorf("pair: decode ciphertext: %w", err)
	}
	return WireEnvelope{
		Kind:       row.Kind,
		KeyVersion: row.KeyVersion,
		Ciphertext: body,
		TS:         row.TS,
	}, nil
}

// streamReader wraps a bufio.Scanner for the inbound side. SetBuffer
// is bumped to 1 MiB to allow large pair frames (push tokens can be
// long, though typical frames are <1 KiB).
type streamReader struct {
	sc *bufio.Scanner
}

func newStreamReader(r io.Reader) *streamReader {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 4096), 1<<20)
	return &streamReader{sc: sc}
}

// readEnvelope blocks until one envelope arrives or the underlying
// reader returns an error / EOF.
func (sr *streamReader) readEnvelope() (WireEnvelope, error) {
	if !sr.sc.Scan() {
		if err := sr.sc.Err(); err != nil {
			return WireEnvelope{}, err
		}
		return WireEnvelope{}, io.EOF
	}
	return decodeEnvelope(sr.sc.Bytes())
}

// streamWriter serializes outbound envelopes. mu protects against
// torn writes from concurrent goroutines.
type streamWriter struct {
	w  io.Writer
	mu sync.Mutex
}

func newStreamWriter(w io.Writer) *streamWriter {
	return &streamWriter{w: w}
}

func (sw *streamWriter) writeFrame(frame Frame) error {
	env, err := EnvelopeWrap(frame)
	if err != nil {
		return err
	}
	line, err := encodeEnvelope(env)
	if err != nil {
		return err
	}
	sw.mu.Lock()
	defer sw.mu.Unlock()
	_, err = sw.w.Write(line)
	return err
}

// readFrameWithCtx wraps streamReader.readEnvelope with context
// cancellation. Because bufio.Scanner doesn't support deadlines, we
// run the read on a goroutine and select against ctx.Done.
func readFrameWithCtx(ctx context.Context, sr *streamReader) (Frame, error) {
	type result struct {
		f   Frame
		err error
	}
	ch := make(chan result, 1)
	go func() {
		env, err := sr.readEnvelope()
		if err != nil {
			ch <- result{nil, err}
			return
		}
		f, err := EnvelopeUnwrap(env)
		ch <- result{f, err}
	}()
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case r := <-ch:
		return r.f, r.err
	}
}

// errCtxOrEOF returns ctx.Err() if cancellation arrived first, else
// the underlying io error. Used by drivers to disambiguate timeouts
// from peer-disconnects.
func errCtxOrEOF(ctx context.Context, ioErr error) error {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxErr
	}
	if ioErr == nil {
		return errors.New("pair: unknown io error")
	}
	return ioErr
}
