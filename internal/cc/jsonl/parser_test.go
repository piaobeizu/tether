package jsonl

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

func TestParser_BasicSingleLine(t *testing.T) {
	p := IncrementalParser{}
	out := p.Feed([]byte(`{"type":"user","uuid":"u1"}` + "\n"))
	if len(out) != 1 {
		t.Fatalf("got %d records, want 1", len(out))
	}
	if out[0].Type != RecordTypeUser || out[0].UUID != "u1" {
		t.Errorf("decoded record wrong: %+v", out[0])
	}
}

func TestParser_MultiLineSingleFeed(t *testing.T) {
	p := IncrementalParser{}
	chunk := `{"type":"user","uuid":"u1"}` + "\n" +
		`{"type":"assistant","uuid":"a1"}` + "\n" +
		`{"type":"permission-mode","permissionMode":"auto"}` + "\n"
	out := p.Feed([]byte(chunk))
	if len(out) != 3 {
		t.Fatalf("got %d records, want 3", len(out))
	}
	if out[0].Type != RecordTypeUser || out[1].Type != RecordTypeAssistant || out[2].Type != RecordTypePermissionMode {
		t.Errorf("wrong types: %v %v %v", out[0].Type, out[1].Type, out[2].Type)
	}
}

func TestParser_PartialLine_HeldUntilNewline(t *testing.T) {
	p := IncrementalParser{}

	// Feed first half — no record yet.
	out := p.Feed([]byte(`{"type":"user",`))
	if len(out) != 0 {
		t.Errorf("partial line emitted %d records, want 0", len(out))
	}
	if p.PartialLen() == 0 {
		t.Error("expected partial buffer to hold the prefix")
	}

	// Feed remainder with newline — should emit now.
	out = p.Feed([]byte(`"uuid":"u1"}` + "\n"))
	if len(out) != 1 {
		t.Fatalf("got %d records after completing line, want 1", len(out))
	}
	if out[0].UUID != "u1" {
		t.Errorf("uuid mismatch: %q", out[0].UUID)
	}
	if p.PartialLen() != 0 {
		t.Errorf("partial buffer should be drained, got %d", p.PartialLen())
	}
}

func TestParser_NoTrailingNewline_IsPartial(t *testing.T) {
	// Per F-09: torn-write defense. Last bytes without '\n' must
	// NOT be emitted.
	p := IncrementalParser{}
	out := p.Feed([]byte(`{"type":"user","uuid":"u1"}` + "\n" + `{"type":"assistant",`))
	if len(out) != 1 {
		t.Fatalf("got %d records, want 1 (the second is incomplete)", len(out))
	}
	if p.PartialLen() == 0 {
		t.Error("expected the incomplete second line to be in the partial buffer")
	}
}

func TestParser_ManyTinyChunks(t *testing.T) {
	// Stress: feed one byte at a time.
	p := IncrementalParser{}
	full := `{"type":"user","uuid":"u1"}` + "\n" + `{"type":"assistant","uuid":"a1"}` + "\n"
	var got []Record
	for _, b := range []byte(full) {
		got = append(got, p.Feed([]byte{b})...)
	}
	if len(got) != 2 {
		t.Fatalf("got %d records, want 2", len(got))
	}
	if got[0].UUID != "u1" || got[1].UUID != "a1" {
		t.Errorf("uuids = %q, %q", got[0].UUID, got[1].UUID)
	}
}

func TestParser_EmptyLines_Skipped(t *testing.T) {
	p := IncrementalParser{}
	chunk := "\n\n" + `{"type":"user","uuid":"u1"}` + "\n\n"
	out := p.Feed([]byte(chunk))
	if len(out) != 1 {
		t.Fatalf("got %d, want 1", len(out))
	}
}

func TestParser_CorruptUTF8_Reported(t *testing.T) {
	var gotErr error
	var gotLine []byte
	p := IncrementalParser{
		OnError: func(line []byte, err error) {
			gotErr = err
			gotLine = append([]byte(nil), line...)
		},
	}
	// Invalid UTF-8 byte 0xff in the middle of a line.
	bad := append([]byte{'{', '"', 'a', '"', ':', '"'}, 0xff)
	bad = append(bad, '"', '}', '\n')
	out := p.Feed(bad)
	if len(out) != 0 {
		t.Errorf("invalid-UTF8 line should be dropped, got %d records", len(out))
	}
	if !errors.Is(gotErr, ErrInvalidUTF8) {
		t.Errorf("expected ErrInvalidUTF8, got %v", gotErr)
	}
	if !bytes.Contains(gotLine, []byte{0xff}) {
		t.Errorf("expected the bad line to be passed to OnError")
	}
}

func TestParser_DecodeError_Reported(t *testing.T) {
	var gotErr error
	p := IncrementalParser{
		OnError: func(line []byte, err error) { gotErr = err },
	}
	out := p.Feed([]byte("not-json\n"))
	if len(out) != 0 {
		t.Errorf("malformed JSON should be dropped, got %d records", len(out))
	}
	if gotErr == nil || !strings.Contains(gotErr.Error(), "decode") {
		t.Errorf("expected a decode error, got %v", gotErr)
	}
}

func TestParser_LineTooLong(t *testing.T) {
	// Construct a line right at the boundary and feed in pieces.
	var gotErr error
	p := IncrementalParser{
		OnError: func(line []byte, err error) { gotErr = err },
	}
	// 4MB of 'a' with no newline — exceeds MaxLineSize.
	huge := bytes.Repeat([]byte{'a'}, MaxLineSize+1024)
	out := p.Feed(huge)
	if len(out) != 0 {
		t.Errorf("over-long line must not produce records, got %d", len(out))
	}
	if !errors.Is(gotErr, ErrLineTooLong) {
		t.Errorf("expected ErrLineTooLong, got %v", gotErr)
	}
	// The partial buffer should be drained.
	if p.PartialLen() > MaxLineSize {
		t.Errorf("partial buffer not drained after over-long: %d", p.PartialLen())
	}
}

func TestParser_Reset_ClearsPartial(t *testing.T) {
	p := IncrementalParser{}
	p.Feed([]byte(`{"type":"user",`))
	if p.PartialLen() == 0 {
		t.Fatal("partial should have content")
	}
	p.Reset()
	if p.PartialLen() != 0 {
		t.Errorf("Reset() should clear partial, got %d", p.PartialLen())
	}
}

func TestParseLine_RetainsRaw(t *testing.T) {
	line := []byte(`{"type":"user","uuid":"u1"}`)
	rec, err := ParseLine(line)
	if err != nil {
		t.Fatalf("ParseLine: %v", err)
	}
	// Mutate caller's buffer; rec.Raw must be unaffected.
	for i := range line {
		line[i] = 'X'
	}
	if !bytes.Contains(rec.Raw, []byte(`"u1"`)) {
		t.Errorf("ParseLine did not defensive-copy: Raw=%s", string(rec.Raw))
	}
}
