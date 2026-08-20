package session

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/piaobeizu/tether/internal/wire"
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

// TestHistory_ThinkingAndTools (tether#44) — thinking + tool activity accumulate
// per turn and flush with the assistant message; a tool_result matches its
// tool_use by id, and a tool_use with no result stays result-less.
func TestHistory_ThinkingAndTools(t *testing.T) {
	dir := t.TempDir()
	h := NewHistoryStore(dir)

	h.AccumulateThinking("s", "let me ")
	h.AccumulateThinking("s", "think")
	h.RecordToolUse("s", "t1", "Read", json.RawMessage(`{"file_path":"a.go"}`))
	h.RecordToolUse("s", "t2", "Bash", json.RawMessage(`{"command":"go test"}`))
	h.RecordToolResult("s", "t2", "PASS", false)
	h.AccumulateAssistant("s", "done")
	h.FinalizeAssistant("s")

	msgs := h.LoadHistory("s")
	if len(msgs) != 1 {
		t.Fatalf("len(msgs) = %d, want 1", len(msgs))
	}
	m := msgs[0]
	if m.Text != "done" || m.Thinking != "let me think" {
		t.Errorf("text=%q thinking=%q, want done / 'let me think'", m.Text, m.Thinking)
	}
	if len(m.Tools) != 2 {
		t.Fatalf("len(tools) = %d, want 2", len(m.Tools))
	}
	if m.Tools[0].Name != "Read" || m.Tools[0].ID != "t1" || m.Tools[0].Result != nil {
		t.Errorf("tools[0] = %+v, want Read/t1/no-result", m.Tools[0])
	}
	if m.Tools[1].Result == nil || m.Tools[1].Result.Content != "PASS" || m.Tools[1].Result.IsError {
		t.Errorf("tools[1].Result = %+v, want {PASS,false}", m.Tools[1].Result)
	}
}

// TestHistory_ThinkingOnlyTurn (tether#44) — a turn with thinking but no answer
// text still persists (the finalize guard must not require text).
func TestHistory_ThinkingOnlyTurn(t *testing.T) {
	dir := t.TempDir()
	h := NewHistoryStore(dir)
	h.AccumulateThinking("s", "pondering")
	h.FinalizeAssistant("s")
	msgs := h.LoadHistory("s")
	if len(msgs) != 1 || msgs[0].Thinking != "pondering" || msgs[0].Text != "" {
		t.Fatalf("msgs = %+v, want one thinking-only entry", msgs)
	}
}

// TestHistory_ToolResultCap (tether#44) — an oversized tool result is capped.
func TestHistory_ToolResultCap(t *testing.T) {
	dir := t.TempDir()
	h := NewHistoryStore(dir)
	h.RecordToolUse("s", "t1", "Bash", nil)
	h.RecordToolResult("s", "t1", strings.Repeat("y", MaxToolResultBytes+5000), false)
	h.AccumulateAssistant("s", "x")
	h.FinalizeAssistant("s")
	msgs := h.LoadHistory("s")
	if len(msgs) != 1 || msgs[0].Tools[0].Result == nil {
		t.Fatalf("expected one entry with a tool result, got %+v", msgs)
	}
	if got := len(msgs[0].Tools[0].Result.Content); got > MaxToolResultBytes+50 {
		t.Errorf("result content %d exceeds cap %d", got, MaxToolResultBytes)
	}
}

// The three overflow paths in this file used to cut a byte slice at the cap and
// hand the result to encoding/json, which re-encodes a half rune as U+FFFD. The
// user then reads a replacement character where their text was cut — and none of
// the three caps is a multiple of 3, so a CJK payload hits it EVERY time it
// overruns rather than occasionally.
//
// Each of the three tests below asserts the absence of U+FFFD rather than the
// length. That distinction is the whole point: TestHistory_ToolResultCap and
// TestHistory_BufferCap above both pass on the broken code, because a
// mid-rune cut is exactly as short as a clean one.
//
// The payload is 好 (U+597D, three bytes) so that the cap always lands inside a
// rune: 16,384 mod 3 == 1 and 4,194,304 mod 3 == 1.

// TestTruncateAtRuneBoundaryKeepsAsMuchAsItCan asserts the EXACT number of bytes
// kept, which the three end-to-end tests below deliberately cannot.
//
// It exists because "contains no U+FFFD" is satisfied by a truncation that keeps
// NOTHING, and so is "is a prefix of the input" (every string has the empty
// prefix). Review demonstrated that: a helper rewritten to `return ""` passed the
// entire package, as did `cut := max - 1`, which silently drops one extra rune
// whenever the cap already lands on a boundary. Absence of corruption and
// preservation of content are two properties, and only the first one survives
// being measured by looking for a bad byte.
//
// The 2-byte-rune rows are what cover the exact-boundary shape: both caps are
// even, so a cap-aligned cut is legal there and any off-by-one shows up as a
// deficit of 2.
func TestTruncateAtRuneBoundaryKeepsAsMuchAsItCan(t *testing.T) {
	for _, tc := range []struct {
		name string
		s    string
		max  int
		want int // EXACT bytes expected out
	}{
		{"ascii cuts exactly at the cap", strings.Repeat("a", 100), 10, 10},
		{"two-byte runes on an even cap keep all of it", strings.Repeat("é", 100), 10, 10},
		{"two-byte runes on an odd cap give back one", strings.Repeat("é", 100), 11, 10},
		{"three-byte runes: 10 is 1 mod 3", strings.Repeat("好", 100), 10, 9},
		{"three-byte runes: 11 is 2 mod 3", strings.Repeat("好", 100), 11, 9},
		{"three-byte runes: 12 is a multiple", strings.Repeat("好", 100), 12, 12},
		{"four-byte rune backs over three bytes", strings.Repeat("😀", 100), 7, 4},
		{"max at len is the whole string", "好好", 6, 6},
		{"max past len is the whole string", "好好", 99, 6},
		{"one byte of a three-byte rune keeps nothing", "好", 1, 0},
		{"zero keeps nothing", strings.Repeat("a", 10), 0, 0},
		{"negative keeps nothing rather than panicking", strings.Repeat("a", 10), -5, 0},
		{"empty input", "", 0, 0},
	} {
		got := truncateAtRuneBoundary(tc.s, tc.max)
		if len(got) != tc.want {
			t.Errorf("%s: truncateAtRuneBoundary(%d bytes, max=%d) kept %d bytes, want %d",
				tc.name, len(tc.s), tc.max, len(got), tc.want)
		}
		if !strings.HasPrefix(tc.s, got) {
			t.Errorf("%s: result is not a prefix of the input", tc.name)
		}
		assertNoReplacementChar(t, tc.name, got)
	}
}

// TestHistory_ToolResultCapNeverCutsARune — MaxToolResultBytes, RecordToolResult.
func TestHistory_ToolResultCapNeverCutsARune(t *testing.T) {
	dir := t.TempDir()
	h := NewHistoryStore(dir)
	body := strings.Repeat("好", MaxToolResultBytes) // 3x the cap
	h.RecordToolUse("s", "t1", "Bash", nil)
	h.RecordToolResult("s", "t1", body, false)
	h.AccumulateAssistant("s", "x")
	h.FinalizeAssistant("s")

	msgs := h.LoadHistory("s")
	if len(msgs) != 1 || len(msgs[0].Tools) != 1 || msgs[0].Tools[0].Result == nil {
		t.Fatalf("expected one entry with a tool result, got %+v", msgs)
	}
	got := msgs[0].Tools[0].Result.Content
	assertNoReplacementChar(t, "tool result", got)
	kept := strings.TrimSuffix(got, ccTruncated)
	if kept == got {
		t.Errorf("content does not end in the truncation marker — not truncated at all?")
	}
	// Bounded on BOTH sides. The upper bound alone is what let the pre-existing
	// TestHistory_ToolResultCap pass on the broken code; the lower bound is what
	// stops "keep nothing" from passing this one. The cap is 1 mod 3 and the
	// payload is three-byte runes, so exactly one byte is given back.
	if len(kept) != MaxToolResultBytes-1 {
		t.Errorf("kept %d bytes, want exactly %d (the cap less the one byte a "+
			"three-byte rune gives back)", len(kept), MaxToolResultBytes-1)
	}
	if !strings.HasPrefix(body, kept) {
		t.Error("the kept part is not a prefix of the recorded result")
	}
}

// TestHistory_AssistantOverflowNeverCutsARune — MaxAssistantBufBytes, the
// chunk[:remaining] splice in AccumulateAssistant.
func TestHistory_AssistantOverflowNeverCutsARune(t *testing.T) {
	dir := t.TempDir()
	h := NewHistoryStore(dir)
	// Two chunks of 3 MiB: the second one crosses the 4 MiB cap with
	// remaining == 1,048,576, which is 1 mod 3.
	chunk := strings.Repeat("好", 1<<20)
	h.AccumulateAssistant("s", chunk)
	h.AccumulateAssistant("s", chunk)
	h.FinalizeAssistant("s")

	msgs := h.LoadHistory("s")
	if len(msgs) != 1 {
		t.Fatalf("len(msgs) = %d, want 1", len(msgs))
	}
	assertNoReplacementChar(t, "assistant text", msgs[0].Text)
	marker := "\n\n[... response truncated at " + strconv.Itoa(MaxAssistantBufBytes) + " bytes ...]"
	kept, ok := strings.CutSuffix(msgs[0].Text, marker)
	if !ok {
		t.Fatal("text is missing its truncation marker — the cap did not bind")
	}
	// Exactly the cap less one byte: 3 MiB of the first chunk, then a second cut
	// at remaining == 1,048,576, which is 1 mod 3. Asserting the exact figure
	// rather than an upper bound is what kills a truncation that keeps nothing.
	if len(kept) != MaxAssistantBufBytes-1 {
		t.Errorf("kept %d bytes, want exactly %d", len(kept), MaxAssistantBufBytes-1)
	}
	if !strings.HasPrefix(chunk+chunk, kept) {
		t.Error("the kept text is not a prefix of what was streamed")
	}
}

// TestHistory_ThinkingOverflowNeverCutsARune — MaxThinkingBufBytes, the
// delta[:remaining] splice in AccumulateThinking.
func TestHistory_ThinkingOverflowNeverCutsARune(t *testing.T) {
	dir := t.TempDir()
	h := NewHistoryStore(dir)
	chunk := strings.Repeat("好", 1<<20)
	h.AccumulateThinking("s", chunk)
	h.AccumulateThinking("s", chunk)
	h.FinalizeAssistant("s")

	msgs := h.LoadHistory("s")
	if len(msgs) != 1 {
		t.Fatalf("len(msgs) = %d, want 1", len(msgs))
	}
	assertNoReplacementChar(t, "thinking", msgs[0].Thinking)
	// This path appends no marker, so the length IS the whole assertion, and it is
	// exact for the same reason as the two above. Note the upper bound alone used
	// to FAIL here rather than merely pass weakly: the broken code persisted
	// 4,194,306 bytes against a 4,194,304 cap, because the byte it dropped came
	// back as a three-byte U+FFFD.
	if got := len(msgs[0].Thinking); got != MaxThinkingBufBytes-1 {
		t.Errorf("thinking is %d bytes, want exactly %d (cap %d less the one byte a "+
			"three-byte rune gives back)", got, MaxThinkingBufBytes-1, MaxThinkingBufBytes)
	}
	if !strings.HasPrefix(chunk+chunk, msgs[0].Thinking) {
		t.Error("the kept thinking is not a prefix of what was streamed")
	}
}

// assertNoReplacementChar fails if s carries a U+FFFD, which is what a
// mid-rune byte cut becomes once it has been through encoding/json.
//
// utf8.ValidString is checked too, and it is NOT redundant: it would catch a
// half rune that reached the caller without a JSON round trip, whereas
// ContainsRune catches the one that already went through the file. Only the
// second can happen on these paths today; asserting both means a future caller
// that skips the file is covered by the same helper.
func assertNoReplacementChar(t *testing.T, what, s string) {
	t.Helper()
	if !utf8.ValidString(s) {
		t.Errorf("%s is not valid UTF-8 — a byte slice cut inside a rune", what)
	}
	if i := strings.IndexRune(s, utf8.RuneError); i >= 0 {
		lo := max(0, i-24)
		hi := min(len(s), i+24)
		t.Errorf("%s carries U+FFFD at byte %d of %d: ...%q... — the truncation cut "+
			"inside a rune and encoding/json replaced the fragment", what, i, len(s), s[lo:hi])
	}
}

// TestHistory_BackwardCompat (tether#44) — a pre-#44 line without thinking/tools
// still parses (new fields default to zero).
func TestHistory_BackwardCompat(t *testing.T) {
	dir := t.TempDir()
	sid := "old"
	path := filepath.Join(dir, sid, "history.jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{"role":"assistant","text":"hi","ts":1}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	msgs := NewHistoryStore(dir).LoadHistory(sid)
	if len(msgs) != 1 || msgs[0].Text != "hi" || msgs[0].Thinking != "" || len(msgs[0].Tools) != 0 {
		t.Fatalf("msgs = %+v, want backward-compatible parse", msgs)
	}
}

// TestHistory_ThinkingToolsAcrossBlockBoundary (tether#44 review nit) — the top
// concern: a turn [thinking + t1 + text][block][t2 + text] must attach pre-block
// thinking+t1 to segment A and post-block t2 to segment B, with NO duplication
// onto the later segment (FinalizeAssistant deletes the buf). Mirrors how
// Registry.fanOut's emitSegments drives the store at a fenced-block boundary.
func TestHistory_ThinkingToolsAcrossBlockBoundary(t *testing.T) {
	dir := t.TempDir()
	h := NewHistoryStore(dir)

	h.AccumulateThinking("s", "planning")
	h.RecordToolUse("s", "t1", "Read", nil)
	h.RecordToolResult("s", "t1", "file contents", false)
	h.AccumulateAssistant("s", "before block")
	h.FinalizeAssistant("s") // segment A flushed before the block
	h.AppendBlock("s", wire.FencedBlock{Kind: wire.FencedBlockDag, Skill: "sk", Content: "{}", BlockID: "sk-0"})
	h.RecordToolUse("s", "t2", "Bash", nil)
	h.AccumulateAssistant("s", "after block")
	h.FinalizeAssistant("s") // segment B after the block

	msgs := h.LoadHistory("s")
	if len(msgs) != 3 {
		t.Fatalf("len(msgs) = %d, want 3 (segA / block / segB): %+v", len(msgs), msgs)
	}
	// Segment A: pre-block thinking + t1(with result).
	if msgs[0].Text != "before block" || msgs[0].Thinking != "planning" {
		t.Errorf("msgs[0] = %+v, want 'before block' + 'planning'", msgs[0])
	}
	if len(msgs[0].Tools) != 1 || msgs[0].Tools[0].ID != "t1" || msgs[0].Tools[0].Result == nil {
		t.Errorf("msgs[0].Tools = %+v, want [t1 with result]", msgs[0].Tools)
	}
	// Block entry: pure block, no rich content.
	if msgs[1].Block == nil || msgs[1].Thinking != "" || len(msgs[1].Tools) != 0 {
		t.Errorf("msgs[1] = %+v, want pure block", msgs[1])
	}
	// Segment B: post-block t2 only — thinking/t1 NOT duplicated.
	if msgs[2].Text != "after block" || msgs[2].Thinking != "" {
		t.Errorf("msgs[2] = %+v, want 'after block', no thinking", msgs[2])
	}
	if len(msgs[2].Tools) != 1 || msgs[2].Tools[0].ID != "t2" {
		t.Errorf("msgs[2].Tools = %+v, want [t2 only]", msgs[2].Tools)
	}
}

// TestHistory_OrphanToolResultDropped (tether#44 review nit) — a tool_result
// with no matching recorded tool_use is dropped (no crash, no phantom tool).
func TestHistory_OrphanToolResultDropped(t *testing.T) {
	dir := t.TempDir()
	h := NewHistoryStore(dir)
	h.AccumulateAssistant("s", "hi")
	h.RecordToolResult("s", "nonexistent", "orphan", false)
	h.FinalizeAssistant("s")
	msgs := h.LoadHistory("s")
	if len(msgs) != 1 || len(msgs[0].Tools) != 0 {
		t.Fatalf("msgs = %+v, want one entry with no tools (orphan dropped)", msgs)
	}
}

// TestHistory_AppendBlockPreservesOrder — (tether#8 T7) a session that
// streams text, then a fenced block, then more text must persist all three
// as separate ordered entries — never collapsed into one concatenated
// assistant message — with the block payload intact, so a page reload can
// reconstruct the DAG card in the same position it rendered live. Mirrors
// how Registry.fanOut's emitSegments drives HistoryStore (FinalizeAssistant
// before AppendBlock).
func TestHistory_AppendBlockPreservesOrder(t *testing.T) {
	dir := t.TempDir()
	h := NewHistoryStore(dir)

	h.RecordUser("sid-block", "do the thing")
	h.AccumulateAssistant("sid-block", "before text\n")
	h.FinalizeAssistant("sid-block")
	h.AppendBlock("sid-block", wire.FencedBlock{
		Kind:    wire.FencedBlockDag,
		Skill:   "s",
		Content: `{"a":1}`,
		BlockID: "s-0",
	})
	h.AccumulateAssistant("sid-block", "after text")
	h.FinalizeAssistant("sid-block")

	msgs := h.LoadHistory("sid-block")
	if len(msgs) != 4 {
		t.Fatalf("len(msgs) = %d, want 4: %+v", len(msgs), msgs)
	}
	if msgs[0].Role != "user" || msgs[0].Text != "do the thing" {
		t.Errorf("msgs[0] = %+v, want user/do the thing", msgs[0])
	}
	if msgs[1].Role != "assistant" || msgs[1].Text != "before text\n" || msgs[1].Block != nil {
		t.Errorf("msgs[1] = %+v, want assistant/before text, no block", msgs[1])
	}
	if msgs[2].Block == nil {
		t.Fatalf("msgs[2].Block = nil, want a FencedBlock: %+v", msgs[2])
	}
	want := wire.FencedBlock{Kind: wire.FencedBlockDag, Skill: "s", Content: `{"a":1}`, BlockID: "s-0"}
	if *msgs[2].Block != want {
		t.Errorf("msgs[2].Block = %+v, want %+v", *msgs[2].Block, want)
	}
	if msgs[2].Text != "" {
		t.Errorf("msgs[2].Text = %q, want empty (block-only entry)", msgs[2].Text)
	}
	if msgs[3].Role != "assistant" || msgs[3].Text != "after text" || msgs[3].Block != nil {
		t.Errorf("msgs[3] = %+v, want assistant/after text, no block", msgs[3])
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

// TestLoadHistoryNumbersMessagesByTheirLine pins the tether store's half of
// HistoryMessage.Ord: 1-based, in file order, and numbered by the LINE rather than by
// the accepted message.
//
// The line is the right unit because a corrupt line then leaves a GAP in the sequence
// instead of shifting every position after it. Only the ORDER of these numbers is ever
// read (mergeHistory compares them; nothing indexes by them), so a gap costs nothing —
// whereas renumbering would make the same message report a different position before and
// after a line went bad, which is exactly the instability the field exists to remove.
func TestLoadHistoryNumbersMessagesByTheirLine(t *testing.T) {
	dir := t.TempDir()
	h := NewHistoryStore(dir)

	path := filepath.Join(dir, "sid-ordered", "history.jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	body := []byte(`{"role":"user","text":"first","ts":1}` + "\n" +
		`not valid json` + "\n" +
		`{"role":"assistant","text":"reply","ts":2}` + "\n" +
		`{"role":"user","text":"again","ts":3}` + "\n")
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}

	msgs := h.LoadHistory("sid-ordered")
	if len(msgs) != 3 {
		t.Fatalf("len(msgs) = %d, want 3", len(msgs))
	}
	// 1, then 3 and 4: line 2 was the corrupt one.
	want := []int64{1, 3, 4}
	for i := range want {
		if msgs[i].Ord != want[i] {
			t.Fatalf("message %d (%q) has Ord %d, want %d — ords %d/%d/%d",
				i, msgs[i].Text, msgs[i].Ord, want[i], msgs[0].Ord, msgs[1].Ord, msgs[2].Ord)
		}
	}
	// Never zero: zero is what omitempty deletes, and a deleted position reads on the
	// frontend as "this daemon does not report positions", i.e. every refresh of a
	// paged-back reader would take the visible-reset path.
	if msgs[0].Ord == 0 {
		t.Fatal("the first message's Ord is 0 — it would not survive JSON encoding")
	}
}

// TestLoadHistoryDoesNotBelieveAPersistedOrd — the position is a fact about the file as
// it is NOW, so it is assigned after the decode and overwrites anything a line happens
// to carry.
//
// Nothing this daemon writes puts an `ord` in a line (see the test below), so the only
// way one gets there is a hand-edited file or a future writer — and a believed value
// would let a file dictate an order the file itself contradicts.
func TestLoadHistoryDoesNotBelieveAPersistedOrd(t *testing.T) {
	dir := t.TempDir()
	h := NewHistoryStore(dir)

	path := filepath.Join(dir, "sid-lying", "history.jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	body := []byte(`{"role":"user","text":"first","ts":1,"ord":9999}` + "\n" +
		`{"role":"assistant","text":"reply","ts":2,"ord":1}` + "\n")
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}

	msgs := h.LoadHistory("sid-lying")
	if len(msgs) != 2 {
		t.Fatalf("len(msgs) = %d, want 2", len(msgs))
	}
	if msgs[0].Ord != 1 || msgs[1].Ord != 2 {
		t.Fatalf("ords %d/%d, want 1/2 — the persisted values were believed, and they invert the file's own order",
			msgs[0].Ord, msgs[1].Ord)
	}
}

// TestOrdIsNotWrittenToTheHistoryFile is what the `omitempty` on HistoryMessage.Ord is
// FOR, asserted rather than argued.
//
// HistoryMessage is both the wire shape of the transcript route and the shape this store
// appends. tether#109 added a field to it, and the requirement was that an appended line
// stay byte-identical: history.jsonl is read by this daemon's own LoadHistory and by
// SessionIndex.firstUserText, and it is a file that outlives the binary that wrote it.
// A position is meaningless in it — it would describe the line's own place, which the
// line's place already describes.
func TestOrdIsNotWrittenToTheHistoryFile(t *testing.T) {
	dir := t.TempDir()
	h := NewHistoryStore(dir)

	h.RecordUser("sid-clean", "a question")
	h.AccumulateAssistant("sid-clean", "an answer")
	h.FinalizeAssistant("sid-clean")

	raw, err := os.ReadFile(filepath.Join(dir, "sid-clean", "history.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), `"ord"`) {
		t.Fatalf("an appended line carries an ord:\n%s", raw)
	}
	// The lines still load, and they load numbered — so "not persisted" is not the same
	// as "not available", which is the pair of facts this design rests on.
	msgs := h.LoadHistory("sid-clean")
	if len(msgs) != 2 || msgs[0].Ord != 1 || msgs[1].Ord != 2 {
		t.Fatalf("loaded %d messages with ords %v, want 2 with 1/2", len(msgs), func() []int64 {
			var o []int64
			for _, m := range msgs {
				o = append(o, m.Ord)
			}
			return o
		}())
	}
}
