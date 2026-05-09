package claude

import (
	"bufio"
	"context"
	"io"
)

// Default scanner buffer sizes. Captured fixture had max line ~13KB; 256KB
// gives ~20× headroom. If we ever need bigger (e.g. attached files in
// content blocks), bump InitialBufSize and MaxBufSize together.
const (
	InitialBufSize = 64 * 1024
	MaxBufSize     = 256 * 1024
)

// ParseOpts tunes the parser's behavior.
type ParseOpts struct {
	// OnError, if set, is invoked under two conditions:
	//   - per-line parse failure: line holds the original bytes; the parser
	//     continues with the next line
	//   - tokenizer / scanner failure (e.g. bufio.ErrTooLong): line is nil;
	//     the stream ends after this callback
	//
	// IMPORTANT: line points into bufio.Scanner's internal buffer and is
	// only valid for the duration of this callback. If you need to retain
	// it (e.g. async logging), copy it first.
	OnError func(line []byte, err error)

	// BufferSize overrides InitialBufSize. Zero means use the default.
	BufferSize int

	// MaxLineSize overrides MaxBufSize. Zero means use the default.
	MaxLineSize int
}

// Parse reads NDJSON lines from r, decodes each into an Envelope, and sends
// successfully parsed events on the returned channel. The channel closes
// when r reaches EOF or ctx is canceled.
//
// Per spec §6.A.2 graceful-degrade: malformed lines and unknown event types
// never abort the stream — malformed lines invoke OnError (if set) and are
// skipped; unknown types pass through with their original Type preserved
// for caller-side filtering.
func Parse(ctx context.Context, r io.Reader, opts ParseOpts) <-chan Envelope {
	bufSize := opts.BufferSize
	if bufSize == 0 {
		bufSize = InitialBufSize
	}
	maxSize := opts.MaxLineSize
	if maxSize == 0 {
		maxSize = MaxBufSize
	}

	out := make(chan Envelope, 16)
	go func() {
		defer close(out)

		scanner := bufio.NewScanner(r)
		scanner.Buffer(make([]byte, bufSize), maxSize)

		for scanner.Scan() {
			line := scanner.Bytes()
			if len(line) == 0 {
				continue
			}
			env, err := ParseLine(line)
			if err != nil {
				if opts.OnError != nil {
					opts.OnError(line, err)
				}
				continue
			}
			select {
			case out <- env:
			case <-ctx.Done():
				return
			}
		}
		// Surface tokenizer failures (e.g. bufio.ErrTooLong) — these would
		// otherwise silently truncate the stream and the caller would see
		// the channel close as if EOF. Per ParseOpts.OnError contract, line
		// is nil for scanner-level errors.
		if err := scanner.Err(); err != nil && opts.OnError != nil {
			opts.OnError(nil, err)
		}
	}()
	return out
}
