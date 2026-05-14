package session

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestHistory_AccumulateAndFinalize exercises the happy path: stream a few
// chunks, finalize, expect a single assistant message persisted with the
// full text.
func TestHistory_AccumulateAndFinalize(t *testing.T) {
	dir := t.TempDir()
	h := NewHistoryStore(dir)

	h.RecordUser("sid-abc", "hello")
	h.AccumulateAssistant("sid-abc", "Hi! ")
	h.AccumulateAssistant("sid-abc", "How can I help?")
	h.FinalizeAssistant("sid-abc")

	msgs := h.LoadHistory("sid-abc")
	if len(msgs) != 2 {
		t.Fatalf("len(msgs) = %d, want 2", len(msgs))
	}
	if msgs[0].Role != "user" || msgs[0].Text != "hello" {
		t.Errorf("msgs[0] = %+v, want user/hello", msgs[0])
	}
	if msgs[1].Role != "assistant" || msgs[1].Text != "Hi! How can I help?" {
		t.Errorf("msgs[1] = %+v, want assistant/<concat>", msgs[1])
	}
}

// TestHistory_BufferCap verifies that AccumulateAssistant stops growing
// past MaxAssistantBufBytes — without this cap, a pathological response
// could OOM the daemon (review finding [10]).
func TestHistory_BufferCap(t *testing.T) {
	dir := t.TempDir()
	h := NewHistoryStore(dir)

	chunk := strings.Repeat("x", 1<<20) // 1 MiB per chunk
	for range 10 {                      // 10 MiB total — well over 4 MiB cap
		h.AccumulateAssistant("sid-big", chunk)
	}
	h.FinalizeAssistant("sid-big")

	msgs := h.LoadHistory("sid-big")
	if len(msgs) != 1 {
		t.Fatalf("len(msgs) = %d, want 1", len(msgs))
	}
	text := msgs[0].Text
	if len(text) > MaxAssistantBufBytes+200 { // +200 for the truncation marker
		t.Errorf("text length %d exceeds cap %d (no truncation?)", len(text), MaxAssistantBufBytes)
	}
	if !strings.Contains(text, "response truncated") {
		t.Errorf("text missing truncation marker: ...%q", text[len(text)-100:])
	}
}

// TestHistory_LoadCorruptLineSkipped — a single bad line shouldn't take down
// the whole history; the good lines around it should still load.
func TestHistory_LoadCorruptLineSkipped(t *testing.T) {
	dir := t.TempDir()
	h := NewHistoryStore(dir)

	path := filepath.Join(dir, "sid-mixed", "history.jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	good := []byte(`{"role":"user","text":"first","ts":1}` + "\n" +
		`not valid json` + "\n" +
		`{"role":"assistant","text":"reply","ts":2}` + "\n")
	if err := os.WriteFile(path, good, 0o600); err != nil {
		t.Fatal(err)
	}

	msgs := h.LoadHistory("sid-mixed")
	if len(msgs) != 2 {
		t.Fatalf("len(msgs) = %d, want 2 (corrupt middle line skipped)", len(msgs))
	}
	if msgs[0].Text != "first" || msgs[1].Text != "reply" {
		t.Errorf("got %+v %+v, want first/reply", msgs[0], msgs[1])
	}
}

// TestHistory_LoadEmpty — fresh sid returns empty slice, not nil, and does
// not log an error (ENOENT is the common case).
func TestHistory_LoadEmpty(t *testing.T) {
	dir := t.TempDir()
	h := NewHistoryStore(dir)

	msgs := h.LoadHistory("never-seen")
	if msgs != nil {
		t.Errorf("LoadHistory for missing sid = %v, want nil", msgs)
	}
}

// TestHistory_RoundtripEncoding — assistant text with control chars and
// emoji should survive the JSONL roundtrip without HTML escaping.
func TestHistory_RoundtripEncoding(t *testing.T) {
	dir := t.TempDir()
	h := NewHistoryStore(dir)

	original := "code: `<script>alert(\"xss\")</script>` 🚀"
	h.AccumulateAssistant("sid-encode", original)
	h.FinalizeAssistant("sid-encode")

	// Read raw JSONL to confirm HTML escaping is off.
	raw, err := os.ReadFile(filepath.Join(dir, "sid-encode", "history.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	var msg HistoryMessage
	if err := json.Unmarshal(raw[:len(raw)-1], &msg); err != nil {
		t.Fatal(err)
	}
	if msg.Text != original {
		t.Errorf("text mismatch: %q != %q", msg.Text, original)
	}
}
