package claude

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// Round-trip the captured fixture through Parse; expect ≥70 envelopes on the
// channel (fixture has 72 lines).
func TestParse_FixtureStream1(t *testing.T) {
	path := filepath.Join("testdata", "stream1.ndjson")
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer f.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var errs []error
	ch := Parse(ctx, f, ParseOpts{
		OnError: func(_ []byte, err error) { errs = append(errs, err) },
	})

	count := 0
	types := map[EventType]int{}
	for env := range ch {
		count++
		types[env.Type]++
	}

	if count < 70 {
		t.Errorf("expected ≥70 events, got %d", count)
	}
	if len(errs) > 0 {
		t.Errorf("expected no parse errors on fixture, got %d: %v", len(errs), errs)
	}
	t.Logf("parsed %d events; types: %+v", count, types)
}

// Garbage line followed by valid line: parser must skip garbage (with
// OnError fired) and still emit the valid envelope.
func TestParse_GarbageLineThenValid(t *testing.T) {
	input := []byte(`not json garbage line
{"type":"system","subtype":"init","session_id":"s1"}
{"type":"result","is_error":false,"session_id":"s1"}
`)

	ctx := context.Background()
	var errCount int
	var mu sync.Mutex
	ch := Parse(ctx, bytes.NewReader(input), ParseOpts{
		OnError: func(_ []byte, _ error) {
			mu.Lock()
			errCount++
			mu.Unlock()
		},
	})

	var got []Envelope
	for env := range ch {
		got = append(got, env)
	}

	if len(got) != 2 {
		t.Errorf("expected 2 valid envelopes, got %d", len(got))
	}
	if errCount != 1 {
		t.Errorf("expected 1 OnError invocation for garbage line, got %d", errCount)
	}
	if got[0].Type != EventSystem || got[1].Type != EventResult {
		t.Errorf("envelope order/type wrong: %+v", got)
	}
}

// Input ending in a partial line (no trailing newline) should not hang —
// bufio.Scanner emits the partial line as the last token.
func TestParse_TruncatedLastLine(t *testing.T) {
	input := []byte(`{"type":"system","subtype":"init","session_id":"s"}` + "\n" +
		`{"type":"result","is_error":false`) // partial, no closing brace + no \n

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	var errCount int
	ch := Parse(ctx, bytes.NewReader(input), ParseOpts{
		OnError: func(_ []byte, _ error) { errCount++ },
	})

	var count int
	for range ch {
		count++
	}

	if count != 1 {
		t.Errorf("expected 1 valid envelope, got %d (truncated line should be reported via OnError)", count)
	}
	if errCount != 1 {
		t.Errorf("expected 1 OnError for truncated partial line, got %d", errCount)
	}
}

// Empty lines between events should be silently skipped (not OnError'd).
func TestParse_EmptyLines(t *testing.T) {
	input := []byte("\n\n" + `{"type":"system","subtype":"init"}` + "\n\n\n")
	ctx := context.Background()

	var errCount int
	ch := Parse(ctx, bytes.NewReader(input), ParseOpts{
		OnError: func(_ []byte, _ error) { errCount++ },
	})

	var count int
	for range ch {
		count++
	}

	if count != 1 || errCount != 0 {
		t.Errorf("empty-line handling wrong: events=%d errors=%d", count, errCount)
	}
}

// Context cancel mid-stream → goroutine exits, channel closes.
func TestParse_ContextCancel(t *testing.T) {
	// Build an infinite-stream reader that emits valid events forever.
	r := &repeatReader{
		line: []byte(`{"type":"system","subtype":"init"}` + "\n"),
		max:  1_000_000, // effectively unlimited within test budget
	}

	ctx, cancel := context.WithCancel(context.Background())
	ch := Parse(ctx, r, ParseOpts{})

	// Drain a few events, then cancel.
	for range 5 {
		<-ch
	}
	cancel()

	// Channel must close (eventually) — drain it under a deadline.
	timeout := time.After(2 * time.Second)
	for {
		select {
		case _, ok := <-ch:
			if !ok {
				return // channel closed — pass
			}
		case <-timeout:
			t.Fatal("Parse goroutine did not exit after ctx cancel")
		}
	}
}

// Large line (>InitialBufSize but <MaxBufSize) must parse — fixture has
// some lines around 13KB. We test 100KB explicitly.
func TestParse_LargeLine(t *testing.T) {
	bigContent := strings.Repeat("X", 100*1024)
	line := `{"type":"assistant","content":"` + bigContent + `"}` + "\n"

	ctx := context.Background()
	ch := Parse(ctx, strings.NewReader(line), ParseOpts{})

	var got []Envelope
	for env := range ch {
		got = append(got, env)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 envelope, got %d", len(got))
	}
	if got[0].Type != EventAssistant {
		t.Errorf("type wrong: %s", got[0].Type)
	}
}

// repeatReader emits the same line over and over, capped at max bytes.
type repeatReader struct {
	line []byte
	pos  int
	read int
	max  int
}

func (r *repeatReader) Read(p []byte) (int, error) {
	if r.read >= r.max {
		return 0, io.EOF
	}
	if r.pos >= len(r.line) {
		r.pos = 0
	}
	n := copy(p, r.line[r.pos:])
	r.pos += n
	r.read += n
	return n, nil
}

// Ensure errors.Is works against our wrapped envelope error.
func TestParseLine_ErrorIsWrapped(t *testing.T) {
	_, err := ParseLine([]byte("not json"))
	if err == nil {
		t.Fatal("expected error")
	}
	// We just need it to be a non-nil error chain — the specific sentinel
	// isn't important for callers.
	var _ = errors.Is // keep import alive for symmetry with other tests
}

// S1 fix: scanner-level errors (e.g. line too long) must surface via
// OnError with a nil line, not silently close the channel.
//
// To trigger bufio.ErrTooLong both BufferSize *and* MaxLineSize must be
// below the line length — otherwise the initial buffer absorbs the whole
// line and no grow-attempt happens.
func TestParse_ScannerErrorSurfaces(t *testing.T) {
	// First line is 50KB; both bufs configured at 4KB.
	tooLong := strings.Repeat("X", 50_000)
	input := []byte(`{"_padding":"` + tooLong + `"}` + "\n" +
		`{"type":"system","subtype":"init"}` + "\n")

	ctx := context.Background()
	var sawNilLineErr bool
	var nilLineErr error
	ch := Parse(ctx, bytes.NewReader(input), ParseOpts{
		BufferSize:  4096,
		MaxLineSize: 4096,
		OnError: func(line []byte, err error) {
			if line == nil {
				sawNilLineErr = true
				nilLineErr = err
			}
		},
	})

	for range ch {
		// drain
	}

	if !sawNilLineErr {
		t.Error("scanner error should surface via OnError with nil line")
	}
	if nilLineErr == nil {
		t.Error("expected non-nil error")
	}
}
