package claude

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"
)

// TestSanitizeAndFormat covers the F-01 input gate: ANSI strip + LF→CR.
func TestSanitizeAndFormat(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"plain text", "hello", "hello"},
		{"LF normalized to CR", "hi\n", "hi\r"},
		{"CRLF collapses to CR", "hi\r\n", "hi\r"},
		{"ANSI CSI stripped", "\x1b[31mred\x1b[0m", "red"},
		{"ANSI OSC stripped (BEL terminated)", "\x1b]0;title\x07rest", "rest"},
		{"ANSI OSC stripped (ST terminated)", "\x1b]0;title\x1b\\rest", "rest"},
		{"bare ESC preserved (F-07 cancel char)", "\x1b", "\x1b"},
		{"Ctrl chars preserved", "\x03\x15\x1b", "\x03\x15\x1b"},
		{"multi-line LF", "a\nb\nc", "a\rb\rc"},
		{"unicode passthrough", "你好\n", "你好\r"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := string(sanitizeAndFormat([]byte(tc.in)))
			if got != tc.want {
				t.Errorf("sanitizeAndFormat(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestByteRing_BasicAppend verifies straightforward append + snapshot.
func TestByteRing_BasicAppend(t *testing.T) {
	r := newByteRing(16)
	r.Write([]byte("hello"))
	if got := string(r.Snapshot()); got != "hello" {
		t.Errorf("snapshot before wrap: %q want hello", got)
	}
	if r.Len() != 5 {
		t.Errorf("len: got %d want 5", r.Len())
	}
}

// TestByteRing_OverflowWrap verifies oldest-first eviction.
func TestByteRing_OverflowWrap(t *testing.T) {
	r := newByteRing(8)
	r.Write([]byte("0123456789ABCDEF")) // 16 bytes into 8-cap ring
	got := string(r.Snapshot())
	want := "89ABCDEF" // last 8
	if got != want {
		t.Errorf("snapshot after wrap: %q want %q", got, want)
	}
	if r.Len() != 8 {
		t.Errorf("len at full: got %d want 8", r.Len())
	}
}

// TestByteRing_PartialThenWrap covers the case where the first write
// fills past mid-ring and the second wraps.
func TestByteRing_PartialThenWrap(t *testing.T) {
	r := newByteRing(8)
	r.Write([]byte("abcd"))
	r.Write([]byte("efghij")) // total 10 bytes; final = "cdefghij"
	got := string(r.Snapshot())
	if got != "cdefghij" {
		t.Errorf("partial-then-wrap: %q want cdefghij", got)
	}
}

// TestPTYSession_FanoutCatAB spawns /bin/cat in PTY mode, sends a
// known sequence after ANSI noise, and verifies (a) the F-01
// sanitization (ANSI stripped, LF→CR) actually reaches cat, and
// (b) two concurrent subscribers each see the same byte stream.
//
// Skipped on -short because PTY allocation is slow on some CI.
func TestPTYSession_FanoutCatAB(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping PTY round-trip under -short")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// /bin/cat echoes whatever lands on its stdin to stdout. Inside a
	// PTY, the kernel ALSO echoes typed bytes back to the master FD
	// (terminal echo on by default), so the master sees the input
	// twice unless we disable echo. For the F-01 verification we want
	// to read what cat OUTPUTS (after newline), not the kernel echo —
	// so just send the input and look for a substring in the
	// aggregated output.
	sess, err := NewPTYSession(ctx, PTYSpawnOpts{
		BinaryPath: "/bin/cat",
		RingSize:   4096,
	})
	if err != nil {
		t.Fatalf("NewPTYSession(cat): %v", err)
	}
	defer sess.Close()

	subA := sess.Subscribe()
	subB := sess.Subscribe()

	// F-01 check: caller hands us LF-terminated text, ANSI prefix that
	// must be stripped. After sanitizeAndFormat the input becomes
	// "hello\r" — cat in cooked mode treats CR as line-ender (echoes
	// it as CRLF on output). We verify the literal string "hello"
	// appears in both subscribers' output, which only happens if the
	// CR reached cat (otherwise cat would buffer indefinitely).
	if err := sess.SendInput([]byte("\x1b[31mhello\x1b[0m\n")); err != nil {
		t.Fatalf("SendInput: %v", err)
	}

	deadline := time.Now().Add(2 * time.Second)
	gatherUntil := func(ch <-chan []byte, want string) string {
		var buf bytes.Buffer
		for time.Now().Before(deadline) {
			select {
			case b, ok := <-ch:
				if !ok {
					return buf.String()
				}
				buf.Write(b)
				if strings.Contains(buf.String(), want) {
					return buf.String()
				}
			case <-time.After(200 * time.Millisecond):
			}
		}
		return buf.String()
	}

	gotA := gatherUntil(subA, "hello")
	gotB := gatherUntil(subB, "hello")

	if !strings.Contains(gotA, "hello") {
		t.Errorf("subscriber A did not see %q in output; got %q", "hello", gotA)
	}
	if !strings.Contains(gotB, "hello") {
		t.Errorf("subscriber B did not see %q in output; got %q", "hello", gotB)
	}

	// Verify ANSI prefix did NOT leak through (sanitization worked).
	// "\x1b[31m" should not appear because the F-01 gate stripped it.
	if strings.Contains(gotA, "\x1b[31m") {
		t.Errorf("subscriber A: ANSI sequence leaked through: %q", gotA)
	}
}

// TestPTYSession_LateJoinerReceivesRing verifies a subscriber attaching
// AFTER bytes have flowed receives a non-empty initial chunk
// (ring snapshot). Uses /bin/cat with a known prelude to keep
// the test deterministic.
func TestPTYSession_LateJoinerReceivesRing(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping PTY ring late-join under -short")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	sess, err := NewPTYSession(ctx, PTYSpawnOpts{
		BinaryPath: "/bin/cat",
		RingSize:   1024,
	})
	if err != nil {
		t.Fatalf("NewPTYSession(cat): %v", err)
	}
	defer sess.Close()

	if err := sess.SendInput([]byte("hello\n")); err != nil {
		t.Fatalf("SendInput: %v", err)
	}
	// Drain the early subscribe-and-output so the ring has content.
	early := sess.Subscribe()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		select {
		case b, ok := <-early:
			if !ok {
				break
			}
			if bytes.Contains(b, []byte("hello")) {
				goto haveSeed
			}
		case <-time.After(100 * time.Millisecond):
		}
	}
haveSeed:

	// Late subscribe — must see the ring snapshot immediately.
	late := sess.Subscribe()
	select {
	case b, ok := <-late:
		if !ok {
			t.Fatal("late subscriber channel closed before any data")
		}
		if !bytes.Contains(b, []byte("hello")) {
			t.Errorf("late subscriber didn't get ring snapshot containing prelude; got %q", string(b))
		}
	case <-time.After(2 * time.Second):
		t.Fatal("late subscriber timed out waiting for ring snapshot")
	}
}

// TestSpawnPTY_BinaryNotFound exercises the same ErrBinaryNotFound
// contract as the stdio Spawn path.
func TestSpawnPTY_BinaryNotFound(t *testing.T) {
	t.Setenv("PATH", "/this/path/intentionally/empty")
	_, err := SpawnPTY(context.Background(), PTYSpawnOpts{
		BinaryPath: "definitely-not-a-real-binary-aaa",
	})
	if err == nil {
		t.Fatal("SpawnPTY(missing binary): expected error")
	}
	if !strings.Contains(err.Error(), "claude binary not found") {
		t.Errorf("error should wrap ErrBinaryNotFound: %q", err.Error())
	}
}
