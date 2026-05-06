package jsonl

import (
	"bytes"
	"errors"
	"fmt"
	"unicode/utf8"
)

// MaxLineSize bounds an individual JSONL record. cc records routinely
// hit ~50KB (a tool_result containing a Read of a 1KLOC file); we set
// 4MB as the hard ceiling. Anything longer is almost certainly cc bug
// or attacker; the Parser drops the offending partial buffer rather
// than allocate unbounded memory.
const MaxLineSize = 4 * 1024 * 1024

// ErrLineTooLong is returned (via OnError) when an in-flight partial
// line exceeds MaxLineSize before its terminating '\n' arrives. The
// parser drops the buffered bytes and resumes from the next read.
var ErrLineTooLong = errors.New("jsonl: line exceeds MaxLineSize")

// ErrInvalidUTF8 is returned (via OnError) when a complete line
// contains an invalid UTF-8 sequence. The line is dropped; the parser
// continues with the next line. Strict UTF-8 is a contract: cc emits
// JSON which is by spec UTF-8, so anything else is corruption (disk
// flip / partial flush of a different process / etc).
var ErrInvalidUTF8 = errors.New("jsonl: invalid UTF-8 in record")

// IncrementalParser turns a byte stream from cc's JSONL file into
// fully-formed Record values. It is the F-09 "torn-write defense"
// boundary: records are emitted only when their terminating '\n' has
// been observed. State is held in `partial`; callers feed bytes via
// Feed() and drain Records via the returned slice.
//
// Not safe for concurrent use — one IncrementalParser per file.
type IncrementalParser struct {
	partial []byte // buffer holding bytes after the last newline

	// OnError, if non-nil, is invoked for per-line errors (decode
	// failure, invalid UTF-8) AND for the terminal MaxLineSize
	// breach. The parser continues on per-line errors; on
	// ErrLineTooLong it drops the partial buffer and continues.
	OnError func(line []byte, err error)
}

// Feed appends `chunk` to the parser's internal buffer, splits on
// '\n', and returns one Record per complete line. Bytes after the
// last '\n' are retained for the next Feed.
//
// Empty lines are silently skipped (cc occasionally emits trailing
// '\n' on close, producing a zero-length tail). Records that fail
// JSON decode are reported via OnError and skipped — the contract is
// "graceful degrade, never abort the stream", same as the
// stream-json parser in internal/backend/claude.
//
// Returns Records in arrival order. The returned slice references
// memory owned by the parser only via Record.Raw (defensive-copied
// already inside ParseLine), so callers may retain Records freely.
func (p *IncrementalParser) Feed(chunk []byte) []Record {
	// Quick path: empty chunk, nothing to do.
	if len(chunk) == 0 {
		return nil
	}

	// Append-and-search. Avoids growing past MaxLineSize.
	if len(p.partial)+len(chunk) > MaxLineSize {
		// Drop the partial — record was unbounded. Keep the new
		// chunk if it itself is in-bounds (might contain valid
		// records following the runaway).
		if p.OnError != nil {
			p.OnError(p.partial, ErrLineTooLong)
		}
		p.partial = p.partial[:0]
		if len(chunk) > MaxLineSize {
			// Even the chunk alone is too large; drop the leading
			// portion until we find a newline.
			nl := bytes.IndexByte(chunk, '\n')
			if nl < 0 {
				// No newline at all in the chunk. Discard it.
				return nil
			}
			chunk = chunk[nl+1:]
		}
	}
	p.partial = append(p.partial, chunk...)

	var out []Record
	for {
		nl := bytes.IndexByte(p.partial, '\n')
		if nl < 0 {
			break // no complete line yet
		}
		line := p.partial[:nl]
		// Advance past the newline.
		p.partial = p.partial[nl+1:]

		if len(line) == 0 {
			continue
		}
		// Strict UTF-8 — cc JSON is by spec UTF-8.
		if !utf8.Valid(line) {
			if p.OnError != nil {
				// Defensive copy for OnError — `line` aliases
				// p.partial's prior backing array which we'll
				// reslice/append to next iteration.
				cp := append([]byte(nil), line...)
				p.OnError(cp, ErrInvalidUTF8)
			}
			continue
		}
		rec, err := ParseLine(line)
		if err != nil {
			if p.OnError != nil {
				cp := append([]byte(nil), line...)
				p.OnError(cp, fmt.Errorf("decode: %w", err))
			}
			continue
		}
		out = append(out, rec)
	}

	// Compact: if partial got long but its head was consumed, copy
	// the tail to a fresh small buffer so we don't pin a huge
	// backing array forever.
	if cap(p.partial) > 64*1024 && len(p.partial) < 4*1024 {
		fresh := make([]byte, len(p.partial))
		copy(fresh, p.partial)
		p.partial = fresh
	}

	return out
}

// PartialLen reports how many bytes are buffered awaiting a '\n'.
// Useful for tests + for the watcher's "stalled write?" diagnostic.
func (p *IncrementalParser) PartialLen() int {
	return len(p.partial)
}

// Reset clears the partial buffer. Called by the watcher on file
// truncation / rotation — the new file starts with no torn-write
// inheritance.
func (p *IncrementalParser) Reset() {
	p.partial = p.partial[:0]
}
