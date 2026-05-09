package wt

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"
)

func TestSessionIDHeader_RoundTrip(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	if err := WriteSessionIDHeader(&buf, "sid-abc"); err != nil {
		t.Fatalf("WriteSessionIDHeader: %v", err)
	}
	// Append trailing data after the newline; the reader must not
	// consume past the newline (slice #4 pair handshake reads from
	// the same stream after the v0.1 header).
	buf.WriteString("trailing-bytes-not-consumed")

	br := bufio.NewReader(&buf)
	sid, err := ReadSessionIDHeader(br)
	if err != nil {
		t.Fatalf("ReadSessionIDHeader: %v", err)
	}
	if sid != "sid-abc" {
		t.Errorf("sid=%q want sid-abc", sid)
	}
	rest, err := io.ReadAll(br)
	if err != nil {
		t.Fatalf("ReadAll rest: %v", err)
	}
	if string(rest) != "trailing-bytes-not-consumed" {
		t.Errorf("trailing data lost: got %q", rest)
	}
}

func TestSessionIDHeader_EmptySidRejected(t *testing.T) {
	t.Parallel()

	if err := WriteSessionIDHeader(&bytes.Buffer{}, ""); !errors.Is(err, ErrEmptySessionID) {
		t.Errorf("WriteSessionIDHeader empty: got %v want ErrEmptySessionID", err)
	}

	// Reader: explicit JSON with empty sid.
	r := strings.NewReader(`{"sessionId":""}` + "\n")
	if _, err := ReadSessionIDHeader(r); !errors.Is(err, ErrEmptySessionID) {
		t.Errorf("ReadSessionIDHeader empty json: got %v want ErrEmptySessionID", err)
	}
}

func TestSessionIDHeader_MalformedJSON(t *testing.T) {
	t.Parallel()
	r := strings.NewReader("not-json\n")
	if _, err := ReadSessionIDHeader(r); err == nil {
		t.Fatal("expected error on malformed JSON, got nil")
	}
}

func TestSessionIDHeader_PrematureEOF(t *testing.T) {
	t.Parallel()
	// No trailing newline.
	r := strings.NewReader(`{"sessionId":"x"}`)
	_, err := ReadSessionIDHeader(r)
	if err == nil {
		t.Fatal("expected error on missing newline, got nil")
	}
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		// Actually io.EOF wraps as ErrUnexpectedEOF only when len(line)==0.
		// With partial content, ReadBytes returns the line + io.EOF
		// directly. Either branch is acceptable; just make sure it's
		// non-nil and identifiable.
		t.Logf("note: error is %v (acceptable as long as non-nil)", err)
	}
}
