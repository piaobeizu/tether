package session

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Every fixture in this file is SYNTHETIC and built under t.TempDir(). Nothing
// here reads, copies or points at a real cc store — see CCStore's doc for why
// that is a safety property and not a preference, and TestCCStoreNeverWrites for
// the assertion that backs it up.

// ccFixture writes one cc transcript and returns the store that can see it.
type ccFixture struct {
	root     string // <tmp>/projects
	workdirs []string
}

func newCCFixture(t *testing.T, workdirs ...string) *ccFixture {
	t.Helper()
	root := filepath.Join(t.TempDir(), "projects")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	return &ccFixture{root: root, workdirs: workdirs}
}

func (f *ccFixture) store() *CCStore {
	return NewCCStore(f.root, func() []string { return f.workdirs })
}

// write puts a transcript at <projects>/<encoded workdir>/<rel>.
func (f *ccFixture) write(t *testing.T, workdir, rel, body string) string {
	t.Helper()
	path := filepath.Join(f.root, EncodeProjectDir(workdir), rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// ccLine renders one transcript record the way cc writes them: compact JSON, one
// object per line.
func ccLine(t *testing.T, fields map[string]any) string {
	t.Helper()
	b, err := json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	return string(b) + "\n"
}

// ccUser builds a plain user turn.
func ccUser(t *testing.T, text string) string {
	return ccLine(t, map[string]any{
		"type":      "user",
		"timestamp": "2026-08-17T03:00:00.000Z",
		"cwd":       "/w",
		"message":   map[string]any{"role": "user", "content": []any{map[string]any{"type": "text", "text": text}}},
	})
}

// ccAssistant builds an assistant turn with one text block and one tool call, so
// every conversion test also exercises the block filter.
func ccAssistant(t *testing.T, text string) string {
	return ccAssistantAt(t, text, "2026-08-17T03:00:01.000Z")
}

// ccAssistantAt is ccAssistant with a caller-chosen stamp, so a test can tell
// WHICH fragment of a merged turn supplied the message's timestamp. ccAssistant
// delegates here rather than repeating the record, so a fixture change cannot
// apply to one of them and not the other.
func ccAssistantAt(t *testing.T, text, ts string) string {
	return ccLine(t, map[string]any{
		"type":      "assistant",
		"timestamp": ts,
		"message": map[string]any{"role": "assistant", "content": []any{
			map[string]any{"type": "text", "text": text},
			map[string]any{"type": "tool_use", "id": "t1", "name": "Bash", "input": map[string]any{"command": "ls"}},
		}},
	})
}

// ccToolResult builds the record cc writes when a tool comes back: type "user",
// content a tool_result block. It is the record that sits BETWEEN two fragments
// of one assistant turn, and the one that makes isUserTurn the wrong boundary —
// see ccMessagesFrom. Every merge fixture in this file contains one.
func ccToolResult(t *testing.T, content string) string {
	return ccLine(t, map[string]any{
		"type":      "user",
		"timestamp": "2026-08-17T03:00:00.000Z",
		"message": map[string]any{"role": "user", "content": []any{
			map[string]any{"type": "tool_result", "tool_use_id": "t1", "content": content},
		}},
	})
}

// ccSidechain builds a sub-agent record of either role. A sub-agent speaking in
// the middle of a turn must neither appear nor split the turn around it.
func ccSidechain(t *testing.T, role, text string) string {
	return ccLine(t, map[string]any{
		"type":        role,
		"isSidechain": true,
		"timestamp":   "2026-08-17T03:00:00.000Z",
		"message":     map[string]any{"role": role, "content": text},
	})
}

// ccCommandStdout builds the record cc writes when it feeds a slash command's
// OUTPUT back in as if the user had said it: type "user", no isMeta, plain
// string content. Dropped by shape (ccCommandStdoutPrefix), so like a tool
// result it must not close an assistant run.
func ccCommandStdout(t *testing.T, text string) string {
	return ccLine(t, map[string]any{
		"type":      "user",
		"timestamp": "2026-08-17T03:00:00.000Z",
		"message":   map[string]any{"role": "user", "content": ccCommandStdoutPrefix + text + "</local-command-stdout>"},
	})
}

// ccPreamble is the kind of record a real transcript opens with — several of
// them, none of them a turn — sized so the fixture behaves like the real thing.
func ccPreamble(t *testing.T, size int) string {
	return ccLine(t, map[string]any{
		"type":       "attachment",
		"cwd":        "/w",
		"userType":   "external",
		"attachment": strings.Repeat("x", size),
	})
}

// ---------------------------------------------------------------------------
// The directory-name encoding
// ---------------------------------------------------------------------------

// TestEncodeProjectDirMatchesRealSamples pins the forward direction against the
// three real directory names this feature was built from.
func TestEncodeProjectDirMatchesRealSamples(t *testing.T) {
	for _, tc := range []struct{ path, want string }{
		{"/root/code/aicoding/gmi-ws", "-root-code-aicoding-gmi-ws"},
		{"/root/code/aicoding/gmi-ws/.repo/tether", "-root-code-aicoding-gmi-ws--repo-tether"},
		{"/root/code/aicoding/gmi-ws/pf.tether-40/tether", "-root-code-aicoding-gmi-ws-pf-tether-40-tether"},
		// A trailing separator must not produce a different directory than the
		// same path without one; a workspace path can arrive either way.
		{"/root/code/aicoding/gmi-ws/", "-root-code-aicoding-gmi-ws"},
	} {
		if got := EncodeProjectDir(tc.path); got != tc.want {
			t.Errorf("EncodeProjectDir(%q) = %q, want %q", tc.path, got, tc.want)
		}
	}
	if got := EncodeProjectDir(""); got != "" {
		t.Errorf("EncodeProjectDir(\"\") = %q, want empty", got)
	}
}

// TestEncodeProjectDirMapsEveryNonAlphanumeric is the regression guard for the
// finding that three real samples could not have produced.
//
// The first version of EncodeProjectDir mapped only '/' and '.'. It agreed with
// all 37 real project directories on the reference machine — because not one of
// those paths contains a character outside [a-zA-Z0-9/.-]. cc's actual encoder
// (read from the installed binary) is `replace(/[^a-zA-Z0-9]/g, "-")`, so a
// workspace with an underscore or a space listed ZERO sessions, silently.
//
// Every case here is a character the old version got wrong.
func TestEncodeProjectDirMapsEveryNonAlphanumeric(t *testing.T) {
	for _, tc := range []struct{ path, want string }{
		{"/root/my_project", "-root-my-project"},
		{"/root/two words", "-root-two-words"},
		{"/root/a+b", "-root-a-b"},
		{"/home/user/repo@v2", "-home-user-repo-v2"},
		{"/srv/x#1", "-srv-x-1"},
		// Alphanumerics survive, including case and digits.
		{"/A1/b2C3", "-A1-b2C3"},
	} {
		if got := EncodeProjectDir(tc.path); got != tc.want {
			t.Errorf("EncodeProjectDir(%q) = %q, want %q", tc.path, got, tc.want)
		}
	}
}

// TestResolveProjectDirFindsALongPathByPrefix — past 200 characters cc stops
// using the plain encoding and appends a hash of the original path, which cannot
// be reproduced from here. Without the prefix lookup such a workspace lists
// nothing, silently.
func TestResolveProjectDirFindsALongPathByPrefix(t *testing.T) {
	long := "/" + strings.Repeat("abcdefghij/", 30) // encodes well past 200
	encoded := EncodeProjectDir(long)
	if len(encoded) <= ccDirNameMaxLen {
		t.Fatalf("fixture is only %d chars; it must exceed %d", len(encoded), ccDirNameMaxLen)
	}
	// What cc would have written: the first 200 characters, a dash, then its hash.
	ccName := encoded[:ccDirNameMaxLen] + "-1a2b3c4d"

	f := newCCFixture(t, long)
	dir := filepath.Join(f.root, ccName)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "cc-session-0001.jsonl"), []byte(ccUser(t, "a long path")), 0o600); err != nil {
		t.Fatal(err)
	}

	rows := f.store().List()
	if len(rows) != 1 || rows[0].Title != "a long path" {
		t.Fatalf("List() = %+v, want the session under the hashed directory", rows)
	}
	if !f.store().Has("cc-session-0001") {
		t.Error("Has() cannot find a session under a hashed directory")
	}
}

// TestEncodeProjectDirCollides is the point of the whole encoding rule: the map
// is LOSSY, so no correct decoder exists.
//
// A test that round-trips one well-behaved path would pass while a decoder was
// silently wrong, which is exactly why this asserts the collision directly
// instead. Two different real directories, one directory name.
func TestEncodeProjectDirCollides(t *testing.T) {
	a := EncodeProjectDir("/root/code/aicoding/gmi-ws")
	b := EncodeProjectDir("/root/code/aicoding/gmi/ws")
	if a != b {
		t.Fatalf("expected the encoding to collide: %q vs %q — if it no longer does, "+
			"the doc claiming it is lossy is now wrong and must be corrected", a, b)
	}
	c := EncodeProjectDir("/root/x.y")
	d := EncodeProjectDir("/root/x/y")
	e := EncodeProjectDir("/root/x_y")
	if c != d || d != e {
		t.Errorf("every non-alphanumeric must map to the same character: %q / %q / %q", c, d, e)
	}
}

// TestListDeduplicatesCollidingWorkdirs — the consequence of the collision above
// reaching List. Two workspaces whose paths encode identically read the SAME
// file; without a dedupe that is two rows for one conversation, and the "current
// session" highlight then lands on whichever the sort happened to put first.
func TestListDeduplicatesCollidingWorkdirs(t *testing.T) {
	f := newCCFixture(t, "/root/code/aicoding/gmi-ws", "/root/code/aicoding/gmi/ws")
	f.write(t, "/root/code/aicoding/gmi-ws", "cc-session-0001.jsonl", ccUser(t, "hello there"))

	rows := f.store().List()
	if len(rows) != 1 {
		t.Fatalf("List() returned %d rows, want 1: %+v", len(rows), rows)
	}
	if rows[0].Sid != "cc-session-0001" {
		t.Errorf("sid = %q", rows[0].Sid)
	}
}

// ---------------------------------------------------------------------------
// Sub-agent exclusion
// ---------------------------------------------------------------------------

// TestListExcludesSubAgentTranscripts — 96% of the .jsonl files under a busy
// project directory are sub-agent transcripts nested one level deeper. The
// fixture puts one there, so a change that started recursing (WalkDir, or a
// filter on names instead of on depth) fails this rather than quietly tripling
// the list.
func TestListExcludesSubAgentTranscripts(t *testing.T) {
	f := newCCFixture(t, "/w")
	f.write(t, "/w", "cc-parent-0001.jsonl", ccUser(t, "the real conversation"))
	f.write(t, "/w", filepath.Join("cc-parent-0001", "cc-subagent-0001.jsonl"), ccUser(t, "a sub-agent said this"))

	rows := f.store().List()
	if len(rows) != 1 {
		t.Fatalf("List() returned %d rows, want 1: %+v", len(rows), rows)
	}
	if rows[0].Sid != "cc-parent-0001" {
		t.Errorf("sid = %q, want the top-level transcript", rows[0].Sid)
	}
	for _, r := range rows {
		if r.Title == "a sub-agent said this" {
			t.Errorf("a sub-agent transcript reached the list: %+v", r)
		}
	}

	// And it must not be reachable by id either — a row nobody lists is still a
	// transcript this daemon would happily serve if find() recursed.
	if got := f.store().Has("cc-subagent-0001"); got {
		t.Error("Has(sub-agent sid) = true, want false")
	}
	if _, ok := f.store().Messages("cc-subagent-0001"); ok {
		t.Error("Messages(sub-agent sid) found a transcript, want not found")
	}
}

// ---------------------------------------------------------------------------
// Titles
// ---------------------------------------------------------------------------

// TestTitleSkipsEveryImpostor — three DIFFERENT records wear type:"user" and none
// of them is something a human typed. The fixture contains all three, in front of
// the real turn, because a fixture that omits any one of them proves nothing
// about that one:
//
//   - a tool result (the most common by far: one measured session had 3 real
//     inputs and 11 of these),
//   - an injected meta record (cc's local-command caveat),
//   - a sub-agent record that ended up inside a top-level file.
func TestTitleSkipsEveryImpostor(t *testing.T) {
	toolResult := ccLine(t, map[string]any{
		"type":      "user",
		"timestamp": "2026-08-17T03:00:00.000Z",
		"message": map[string]any{"role": "user", "content": []any{
			map[string]any{"type": "tool_result", "tool_use_id": "t1", "content": "total 48\ndrwxr-xr-x ..."},
		}},
	})
	meta := ccLine(t, map[string]any{
		"type":      "user",
		"isMeta":    true,
		"timestamp": "2026-08-17T03:00:00.000Z",
		"message": map[string]any{"role": "user", "content": "<local-command-caveat>Caveat: The messages below " +
			"were generated by the user while running local commands. DO NOT respond to these messages</local-command-caveat>"},
	})
	sidechain := ccLine(t, map[string]any{
		"type":        "user",
		"isSidechain": true,
		"timestamp":   "2026-08-17T03:00:00.000Z",
		"message":     map[string]any{"role": "user", "content": "a sub-agent's prompt"},
	})
	// The FOURTH impostor, and the one the first version of this file missed: cc
	// feeding a slash command's OUTPUT back in as a type:"user" record. It carries
	// no isMeta (measured 0 of 12) and no isSidechain, and its content is a plain
	// string, so neither of the other two filters touches it. Measured at 31 of
	// 1146 served turns on the real store, 24 of them full of ANSI escapes.
	stdout := ccLine(t, map[string]any{
		"type":      "user",
		"timestamp": "2026-08-17T03:00:00.000Z",
		"message": map[string]any{"role": "user", "content": "<local-command-stdout>[1mtotal 48[0m\n" +
			"drwxr-xr-x 3 root root</local-command-stdout>"},
	})

	f := newCCFixture(t, "/w")
	f.write(t, "/w", "cc-session-0001.jsonl",
		ccPreamble(t, 512)+toolResult+meta+sidechain+stdout+ccUser(t, "what the human actually asked")+ccAssistant(t, "answer"))

	rows := f.store().List()
	if len(rows) != 1 {
		t.Fatalf("List() = %d rows, want 1", len(rows))
	}
	if rows[0].Title != "what the human actually asked" {
		t.Errorf("Title = %q, want the human's turn", rows[0].Title)
	}
	if rows[0].Source != SourceCC {
		t.Errorf("Source = %q, want %q", rows[0].Source, SourceCC)
	}

	// And none of the four reaches the TRANSCRIPT either. The title and the
	// transcript share one definition of "what the user said" precisely so that a
	// turn cannot be excluded from one and kept in the other.
	msgs, ok := f.store().Messages("cc-session-0001")
	if !ok {
		t.Fatal("Messages reported the transcript missing")
	}
	if len(msgs) != 2 {
		t.Fatalf("got %d turns, want 2 (the human's and the assistant's): %+v", len(msgs), msgs)
	}
	for _, m := range msgs {
		for _, forbidden := range []string{"tool_result", "local-command-caveat", "sub-agent", "local-command-stdout", "\x1b["} {
			if strings.Contains(m.Text, forbidden) {
				t.Errorf("an impostor reached the transcript (%q): %q", forbidden, m.Text)
			}
		}
	}
}

// TestTitleRendersSlashCommands — cc records a slash command as three XML-ish
// tags. Untreated that was 13% of one real profile's rows rendering as visible
// markup, in a list whose entire purpose is to be readable.
func TestTitleRendersSlashCommands(t *testing.T) {
	for _, tc := range []struct{ name, content, want string }{
		{
			name: "name and args",
			content: "<command-message>polyforge:pf-work</command-message>" +
				"<command-name>/polyforge:pf-work</command-name><command-args>silgrid#123</command-args>",
			want: "/polyforge:pf-work silgrid#123",
		},
		{
			name:    "no args",
			content: "<command-name>/clear</command-name><command-message>clear</command-message><command-args></command-args>",
			want:    "/clear",
		},
		{
			// Text with no command markup must come through untouched. Stripping
			// tags in general would silently rewrite a prompt that legitimately
			// contains markup.
			name:    "not a command",
			content: "compare <div> and <span> for me",
			want:    "compare <div> and <span> for me",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newCCFixture(t, "/w")
			f.write(t, "/w", "cc-session-0001.jsonl", ccLine(t, map[string]any{
				"type":      "user",
				"timestamp": "2026-08-17T03:00:00.000Z",
				"message":   map[string]any{"role": "user", "content": tc.content},
			}))
			rows := f.store().List()
			if len(rows) != 1 || rows[0].Title != tc.want {
				t.Errorf("Title = %q, want %q", rows[0].Title, tc.want)
			}
		})
	}
}

// TestTitleIsCondensedLikeTetherRows — one truncation rule for both stores, so a
// cc row and a tether row cannot disagree about how long a label may be.
func TestTitleIsCondensedLikeTetherRows(t *testing.T) {
	long := strings.Repeat("ä", maxTitleLen+40)
	f := newCCFixture(t, "/w")
	f.write(t, "/w", "cc-session-0001.jsonl", ccUser(t, "line one\n\n  line   two "+long))
	rows := f.store().List()
	if len(rows) != 1 {
		t.Fatalf("List() = %d rows", len(rows))
	}
	got := []rune(rows[0].Title)
	if len(got) != maxTitleLen+1 || got[maxTitleLen] != '…' {
		t.Errorf("Title not condensed to maxTitleLen+ellipsis: %d runes, %q", len(got), rows[0].Title)
	}
	if strings.Contains(rows[0].Title, "\n") {
		t.Errorf("Title still contains a newline: %q", rows[0].Title)
	}
}

// TestTitleSurvivesWhitespaceInTheJSON — the cheap pre-filter that skips the
// preamble without parsing it must key on the quoted VALUE, not on the
// key-colon-value pair.
//
// cc writes compact JSON today, so a needle of `"type":"user"` passes every
// other test in this file. Its failure mode is not a wrong title, it is EVERY
// title silently becoming a bare sid the day the producer adds a space — a
// regression with no error and no log line. This fixture writes the space in.
func TestTitleSurvivesWhitespaceInTheJSON(t *testing.T) {
	spaced := `{"type": "user", "timestamp": "2026-08-17T03:00:00.000Z", ` +
		`"message": {"role": "user", "content": "spaced out"}}` + "\n"

	if got := ccFirstUserText(strings.NewReader(spaced), ccTitlePrefixBytes); got != "spaced out" {
		t.Errorf("ccFirstUserText = %q, want %q — the pre-filter is too tight", got, "spaced out")
	}
	// And the negative control: a key that merely STARTS with user must not drag
	// a non-turn through the filter into the parser's lap.
	notATurn := `{"type":"attachment","userType":"external","cwd":"/w"}` + "\n"
	if got := ccFirstUserText(strings.NewReader(notATurn), ccTitlePrefixBytes); got != "" {
		t.Errorf("ccFirstUserText = %q, want empty", got)
	}
}

// countingReader records how many bytes were actually pulled through it.
type countingReader struct {
	r io.Reader
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

// TestFirstUserTextNeverReadsPastTheLimit — the bound is a CORRECTNESS property
// here, not an optimisation: real transcripts reach 138 MB and this runs once per
// session on every list request.
//
// Asserted by counting bytes rather than by eye or by a timer.
func TestFirstUserTextNeverReadsPastTheLimit(t *testing.T) {
	const limit = 8 << 10
	body := ccPreamble(t, 64<<10) + ccUser(t, "buried past the bound")
	cr := &countingReader{r: strings.NewReader(body)}

	if got := ccFirstUserText(cr, limit); got != "" {
		t.Errorf("ccFirstUserText = %q, want empty — the turn is past the bound", got)
	}
	if cr.n > limit {
		t.Errorf("read %d bytes, limit is %d", cr.n, limit)
	}
}

// TestFirstUserTextStopsAtTheAnswer — the other half of the bound. Reading the
// full window on every session would be sessions × window per request; the scan
// exists to stop at the first turn.
func TestFirstUserTextStopsAtTheAnswer(t *testing.T) {
	body := ccUser(t, "the very first thing") + ccPreamble(t, 4<<20)
	cr := &countingReader{r: strings.NewReader(body)}

	if got := ccFirstUserText(cr, ccTitlePrefixBytes); got != "the very first thing" {
		t.Fatalf("ccFirstUserText = %q", got)
	}
	if cr.n > 64<<10 {
		t.Errorf("read %d bytes to find a turn in the first line — it did not stop early", cr.n)
	}
}

// TestFirstUserTextIgnoresATruncatedTrailingLine — the window almost always ends
// mid-record. A fragment that merely fails to parse is harmless; one that happens
// to parse would put half a record in the list.
func TestFirstUserTextIgnoresATruncatedTrailingLine(t *testing.T) {
	full := ccUser(t, "complete turn")
	body := ccPreamble(t, 128) + full
	// Cut one byte before the final newline, so the last line is a complete JSON
	// object with no terminator.
	truncated := body[:len(body)-1]
	if got := ccFirstUserText(strings.NewReader(truncated), int64(len(truncated))); got != "" {
		t.Errorf("ccFirstUserText = %q, want empty — the only turn was unterminated", got)
	}
	if got := ccFirstUserText(strings.NewReader(body), int64(len(body))); got != "complete turn" {
		t.Errorf("control: ccFirstUserText = %q, want the turn", got)
	}
}

// TestCCTitlePrefixCoversTheMeasuredWorstCase — the constant is a MEASUREMENT,
// and this is the guard on the measurement rather than on the mechanism.
//
// It exists because of a specific near-miss: the plan for this feature said to
// reuse tether's own titlePrefixBytes (16 KiB), which is right for a transcript
// that opens with the user's first line and wrong for one that opens with a
// 40–60 KB preamble. At 16 KiB, 36 of 38 real transcripts would have produced NO
// title and rendered as bare sids — a silent, total regression of the thing
// tether#91 built the list for, with no error anywhere.
//
// The neighbouring behaviour test cannot catch that: its fixture is sized in
// terms of this constant, so it scales with any change to it and stays green.
// This one pins the number against the world it was measured in.
func TestCCTitlePrefixCoversTheMeasuredWorstCase(t *testing.T) {
	// Deepest first-user-turn offset over the 38 top-level transcripts of one
	// real workspace, measured 2026-08-17. The range was 319 … 60,103 bytes.
	const measuredWorstCaseOffset = 60103
	if ccTitlePrefixBytes < measuredWorstCaseOffset {
		t.Fatalf("ccTitlePrefixBytes = %d, which is below the %d-byte worst case actually observed. "+
			"Lowering it does not fail anything loudly — it silently turns titles into bare sids. "+
			"If the store's layout has genuinely changed, re-measure and update BOTH the constant "+
			"and this number.", ccTitlePrefixBytes, measuredWorstCaseOffset)
	}
	// tether's own constant is deliberately NOT reused. If they are ever made
	// equal, one of the two is wrong for its store.
	if ccTitlePrefixBytes == titlePrefixBytes {
		t.Errorf("the cc bound and the tether bound are the same value (%d); "+
			"they describe two different file layouts and were measured separately", ccTitlePrefixBytes)
	}
}

// TestTitleBeyondTheBoundLosesTheTitleNotTheRow — a session whose opening turn is
// past the window is still listable and clickable. That is the same promise
// SessionIndex.title makes for tether's own store.
func TestTitleBeyondTheBoundLosesTheTitleNotTheRow(t *testing.T) {
	f := newCCFixture(t, "/w")
	f.write(t, "/w", "cc-session-0001.jsonl", ccPreamble(t, ccTitlePrefixBytes+1024)+ccUser(t, "too late"))

	rows := f.store().List()
	if len(rows) != 1 {
		t.Fatalf("List() = %d rows, want the row to survive", len(rows))
	}
	if rows[0].Title != "" {
		t.Errorf("Title = %q, want empty — the turn is past the bound", rows[0].Title)
	}
	if rows[0].Sid != "cc-session-0001" {
		t.Errorf("sid = %q", rows[0].Sid)
	}
}

// ---------------------------------------------------------------------------
// Transcripts
// ---------------------------------------------------------------------------

func TestMessagesConvertsTheConversation(t *testing.T) {
	f := newCCFixture(t, "/w")
	f.write(t, "/w", "cc-session-0001.jsonl",
		ccPreamble(t, 128)+ccUser(t, "first")+ccAssistant(t, "second")+ccUser(t, "third"))

	msgs, ok := f.store().Messages("cc-session-0001")
	if !ok {
		t.Fatal("Messages reported the transcript missing")
	}
	want := []struct{ role, text string }{{"user", "first"}, {"assistant", "second"}, {"user", "third"}}
	if len(msgs) != len(want) {
		t.Fatalf("got %d messages, want %d: %+v", len(msgs), len(want), msgs)
	}
	for i, w := range want {
		if msgs[i].Role != w.role || msgs[i].Text != w.text {
			t.Errorf("msgs[%d] = %s/%q, want %s/%q", i, msgs[i].Role, msgs[i].Text, w.role, w.text)
		}
	}
	// The stamp is cc's ISO-8601 turned into the Unix milliseconds the wire type
	// uses — not zero, and not a re-read of the file's mtime.
	wantTs := time.Date(2026, 8, 17, 3, 0, 0, 0, time.UTC).UnixMilli()
	if msgs[0].Ts != wantTs {
		t.Errorf("Ts = %d, want %d", msgs[0].Ts, wantTs)
	}
}

// TestMessagesDropsToolActivity — the assistant fixture carries a tool_use block
// alongside its text, and a tool_result arrives as a type:"user" record. Neither
// becomes a turn: tool payloads are what make a transcript 138 MB, and the row
// that renders this says so.
func TestMessagesDropsToolActivity(t *testing.T) {
	toolResult := ccLine(t, map[string]any{
		"type":      "user",
		"timestamp": "2026-08-17T03:00:00.000Z",
		"message": map[string]any{"role": "user", "content": []any{
			map[string]any{"type": "tool_result", "tool_use_id": "t1", "content": "SECRET-TOOL-OUTPUT"},
		}},
	})
	f := newCCFixture(t, "/w")
	f.write(t, "/w", "cc-session-0001.jsonl", ccUser(t, "run it")+ccAssistant(t, "running")+toolResult)

	msgs, _ := f.store().Messages("cc-session-0001")
	if len(msgs) != 2 {
		t.Fatalf("got %d messages, want 2 (the tool result is not a turn): %+v", len(msgs), msgs)
	}
	for _, m := range msgs {
		if strings.Contains(m.Text, "SECRET-TOOL-OUTPUT") || strings.Contains(m.Text, "\"tool_use\"") {
			t.Errorf("tool activity leaked into a turn: %q", m.Text)
		}
	}
}

// ---------------------------------------------------------------------------
// One bubble per turn (tether#94)
// ---------------------------------------------------------------------------

// TestMessagesMergesATurnSplitByToolCalls is the bug, in the shape it was
// reported: one turn, no user message anywhere inside it, arriving as a row of
// separate timestamped bubbles each ending on a colon because that is where the
// agent stopped to call a tool.
//
// The fixture carries every record type that sits BETWEEN two fragments of a
// real turn, because the failure mode of this change is a boundary detector that
// is itself fooled by them. A fixture of bare assistant records would merge
// under both the right rule and the wrong one and prove nothing:
//
//   - tool results — type:"user", so isUserTurn() is TRUE of them. Using that as
//     the boundary ends the turn at every tool call, i.e. at exactly the seams
//     this merge exists to remove.
//   - <local-command-stdout> — also type:"user", also not isMeta, dropped only
//     by shape.
//   - sub-agent chatter (isSidechain), in both roles.
func TestMessagesMergesATurnSplitByToolCalls(t *testing.T) {
	body := ccUser(t, "把这个查清楚") +
		ccAssistantAt(t, "查清楚:", "2026-08-17T04:57:00.000Z") +
		ccToolResult(t, "SECRET-TOOL-OUTPUT") +
		ccAssistantAt(t, "先看进度:", "2026-08-17T05:01:00.000Z") +
		ccSidechain(t, "user", "a sub-agent's prompt") +
		ccSidechain(t, "assistant", "a sub-agent's answer") +
		ccToolResult(t, "MORE-TOOL-OUTPUT") +
		ccCommandStdout(t, "\x1b[1mtotal 48\x1b[0m") +
		ccAssistantAt(t, "读 baseline 脚本剩余部分:", "2026-08-17T05:06:00.000Z")

	f := newCCFixture(t, "/w")
	f.write(t, "/w", "cc-session-0001.jsonl", body)

	msgs, ok := f.store().Messages("cc-session-0001")
	if !ok {
		t.Fatal("Messages reported the transcript missing")
	}
	if len(msgs) != 2 {
		t.Fatalf("got %d messages, want 2 (one user turn, one answer): %+v", len(msgs), msgs)
	}
	if msgs[0].Role != "user" || msgs[0].Text != "把这个查清楚" {
		t.Errorf("msgs[0] = %s/%q, want the user's turn", msgs[0].Role, msgs[0].Text)
	}
	want := "查清楚:" + ccTurnJoin + "先看进度:" + ccTurnJoin + "读 baseline 脚本剩余部分:"
	if msgs[1].Role != "assistant" || msgs[1].Text != want {
		t.Errorf("msgs[1] = %s/%q,\nwant assistant/%q", msgs[1].Role, msgs[1].Text, want)
	}
	// The separator asserted LITERALLY as well as through the constant. A lone
	// "\n" would satisfy every assertion written in terms of ccTurnJoin while
	// rendering as a space: the pane runs this through react-markdown with no
	// remark-breaks, so a soft break joins two lines. "查清楚: 先看进度:" on one
	// line is a worse answer than the split bubbles this replaces, and nothing
	// else in the suite would notice.
	if !strings.Contains(msgs[1].Text, "查清楚:\n\n先看进度:") {
		t.Errorf("fragments are not separated by a BLANK line — markdown would run them "+
			"together on one line: %q", msgs[1].Text)
	}
	// The stamp is the turn's START — when the response began — which is also
	// what tether's own path records (history.go stamps the accumulator on
	// creation). Taking the last fragment's instead would label a nine-minute
	// turn with the moment it ended.
	if wantTs := time.Date(2026, 8, 17, 4, 57, 0, 0, time.UTC).UnixMilli(); msgs[1].Ts != wantTs {
		t.Errorf("Ts = %d, want %d (the FIRST fragment's stamp)", msgs[1].Ts, wantTs)
	}
	for _, m := range msgs {
		for _, forbidden := range []string{"SECRET-TOOL-OUTPUT", "MORE-TOOL-OUTPUT", "sub-agent", "local-command-stdout", "\x1b["} {
			if strings.Contains(m.Text, forbidden) {
				t.Errorf("a dropped record reached the merged turn (%q): %q", forbidden, m.Text)
			}
		}
	}
}

// TestMessagesDoesNotMergeAcrossAUserTurn — the other half of the boundary. The
// merge must join what cc split and nothing else: two real turns stay two turns,
// and two things the user said in a row stay two bubbles, because that is what
// the terminal shows.
func TestMessagesDoesNotMergeAcrossAUserTurn(t *testing.T) {
	body := ccUser(t, "first question") +
		ccAssistantAt(t, "first-a", "2026-08-17T03:00:01.000Z") +
		ccToolResult(t, "x") +
		ccAssistantAt(t, "first-b", "2026-08-17T03:00:02.000Z") +
		ccUser(t, "second question") +
		ccAssistantAt(t, "second-a", "2026-08-17T03:00:03.000Z") +
		ccToolResult(t, "y") +
		ccAssistantAt(t, "second-b", "2026-08-17T03:00:04.000Z") +
		// Two things the user said back to back, with nothing between them: a
		// queued prompt, or simply typing twice. These are two bubbles in the
		// terminal and must stay two here — a merge written as "collapse adjacent
		// same-role messages" would swallow one of them.
		ccUser(t, "and another thing") +
		ccUser(t, "one more")

	f := newCCFixture(t, "/w")
	f.write(t, "/w", "cc-session-0001.jsonl", body)

	msgs, _ := f.store().Messages("cc-session-0001")
	want := []struct{ role, text string }{
		{"user", "first question"},
		{"assistant", "first-a" + ccTurnJoin + "first-b"},
		{"user", "second question"},
		{"assistant", "second-a" + ccTurnJoin + "second-b"},
		{"user", "and another thing"},
		{"user", "one more"},
	}
	if len(msgs) != len(want) {
		t.Fatalf("got %d messages, want %d: %+v", len(msgs), len(want), msgs)
	}
	for i, w := range want {
		if msgs[i].Role != w.role || msgs[i].Text != w.text {
			t.Errorf("msgs[%d] = %s/%q, want %s/%q", i, msgs[i].Role, msgs[i].Text, w.role, w.text)
		}
	}
}

// TestMessagesCapCountsTurnsNotRecords pins the UNIT of ccMessagesMax.
//
// It exists because that constant's doc comment claimed to cap "turns" while the
// trim 400-odd lines below it counted one element per converted cc record — two
// things that differ by a factor of 7.07 on the real store, with nothing in the
// build to notice. A doc comment is what let them drift apart, so the unit is
// asserted here instead.
//
// The fixture reads far more RECORDS than the cap and exactly the cap's worth of
// MESSAGES, so the front is trimmed under the old unit and kept under the new
// one. The very first thing the user said is the assertion.
func TestMessagesCapCountsTurnsNotRecords(t *testing.T) {
	const fragments = 5
	turns := ccMessagesMax / 2 // one user + one merged assistant message each
	records := turns * (1 + fragments)
	if records <= ccMessagesMax {
		t.Fatalf("fixture reads %d records, which the %d cap would not bind on — "+
			"it cannot tell the two units apart", records, ccMessagesMax)
	}

	var b strings.Builder
	for i := 0; i < turns; i++ {
		b.WriteString(ccUser(t, fmt.Sprintf("turn-%04d asks", i)))
		for j := 0; j < fragments; j++ {
			b.WriteString(ccAssistantAt(t, fmt.Sprintf("turn-%04d frag-%d", i, j), "2026-08-17T03:00:01.000Z"))
			b.WriteString(ccToolResult(t, "tool output"))
		}
	}
	f := newCCFixture(t, "/w")
	f.write(t, "/w", "cc-session-0001.jsonl", b.String())

	msgs, _ := f.store().Messages("cc-session-0001")
	if len(msgs) != ccMessagesMax {
		t.Fatalf("got %d messages from %d records, want %d — the cap's unit is messages, not records",
			len(msgs), records, ccMessagesMax)
	}
	if msgs[0].Role != "user" || msgs[0].Text != "turn-0000 asks" {
		t.Errorf("msgs[0] = %s/%q, want the FIRST turn — %d records fit under a cap of %d "+
			"messages, so nothing should have been trimmed", msgs[0].Role, msgs[0].Text, records, ccMessagesMax)
	}
	// And every fragment of every turn is present: merging is what makes the
	// record count fit, so a merge that dropped fragments would also pass the
	// count assertion above.
	for i := 0; i < turns; i++ {
		got := msgs[i*2+1].Text
		for j := 0; j < fragments; j++ {
			if frag := fmt.Sprintf("turn-%04d frag-%d", i, j); !strings.Contains(got, frag) {
				t.Fatalf("turn %d lost %q: %q", i, frag, got)
			}
		}
	}
}

// TestMessagesCapDoesNotShrinkTheScrollback guards the VALUE of ccMessagesMax,
// which the tests above deliberately do not.
//
// Same shape as TestCCTitlePrefixCoversTheMeasuredWorstCase, and for the same
// reason: every mechanism test here sizes its fixture FROM the constant, so it
// scales with any change to it and stays green — 200, 400 and 20 all pass them.
// A constant that encodes a measurement has to be checked against the world it
// was measured in, not against itself.
//
// The specific regression: tether#94 HALVED this number while making each
// message ~3.9x larger, and the entire argument for that was that 200 messages
// is MORE scrollback than 400 records was delivering. Someone later reading only
// the number could lower it further, inverting the argument and truncating
// people's history with no error and no failing test.
//
// The arithmetic, over 39 real transcripts measured 2026-08-17: 400 records
// delivered ~57 turns (3,350 user + 20,325 assistant messages = 7.07 records per
// turn). Post-merge the same store averages 1.81 messages per turn (23,675 ->
// 6,072), so ~57 turns is ~104 messages. Below that, this change stops being a
// fix and becomes a quiet reduction in what the user can scroll back to.
func TestMessagesCapDoesNotShrinkTheScrollback(t *testing.T) {
	// Messages needed to hold the ~57 turns the pre-tether#94 cap delivered.
	const equivalentOfTheOldCap = 104
	if ccMessagesMax < equivalentOfTheOldCap {
		t.Fatalf("ccMessagesMax = %d, below the %d messages that the OLD cap of 400 "+
			"records was delivering (~57 turns at 1.81 messages/turn). Lowering it does not "+
			"fail anything loudly — it silently serves less history than before the merge, "+
			"which is the opposite of what tether#94 claimed. If the store's shape has "+
			"genuinely changed, re-measure and update BOTH the constant and this number.",
			ccMessagesMax, equivalentOfTheOldCap)
	}
}

// TestMessagesCapKeepsTheNewestTurnsInOrder — both directions of the trim, on a
// merged transcript.
//
// "Keeps the newest" and "keeps the oldest" produce the same LENGTH, and a test
// that only checks the tail passes against a slice that was reversed as well as
// trimmed. So this asserts the first surviving turn, the last one, and strictly
// increasing turn numbers through the middle.
func TestMessagesCapKeepsTheNewestTurnsInOrder(t *testing.T) {
	turns := ccMessagesMax // twice as many messages as the cap allows
	var b strings.Builder
	for i := 0; i < turns; i++ {
		b.WriteString(ccUser(t, fmt.Sprintf("turn-%04d", i)))
		b.WriteString(ccAssistantAt(t, fmt.Sprintf("answer-%04d a", i), "2026-08-17T03:00:01.000Z"))
		b.WriteString(ccToolResult(t, "tool output"))
		b.WriteString(ccAssistantAt(t, fmt.Sprintf("answer-%04d b", i), "2026-08-17T03:00:02.000Z"))
	}
	f := newCCFixture(t, "/w")
	f.write(t, "/w", "cc-session-0001.jsonl", b.String())

	msgs, _ := f.store().Messages("cc-session-0001")
	if len(msgs) != ccMessagesMax {
		t.Fatalf("got %d messages, want the cap of %d", len(msgs), ccMessagesMax)
	}
	firstKept := turns - ccMessagesMax/2
	for i := 0; i < ccMessagesMax/2; i++ {
		u, a := msgs[i*2], msgs[i*2+1]
		wantU := fmt.Sprintf("turn-%04d", firstKept+i)
		if u.Role != "user" || u.Text != wantU {
			t.Fatalf("msgs[%d] = %s/%q, want user/%q — the surviving turns must be the "+
				"NEWEST %d, in order", i*2, u.Role, u.Text, wantU, ccMessagesMax/2)
		}
		wantA := fmt.Sprintf("answer-%04d a", firstKept+i) + ccTurnJoin + fmt.Sprintf("answer-%04d b", firstKept+i)
		if a.Role != "assistant" || a.Text != wantA {
			t.Fatalf("msgs[%d] = %s/%q, want assistant/%q", i*2+1, a.Role, a.Text, wantA)
		}
	}
}

// TestMessagesReadsTheTail — a transcript larger than the window yields its most
// recent turns, and the record the window opened in the middle of is dropped
// rather than half-parsed.
func TestMessagesReadsTheTail(t *testing.T) {
	var b strings.Builder
	b.WriteString(ccUser(t, "OLDEST-TURN"))
	for b.Len() < ccMessagesTailBytes+(64<<10) {
		b.WriteString(ccPreamble(t, 8<<10))
	}
	b.WriteString(ccUser(t, "NEWEST-TURN"))

	f := newCCFixture(t, "/w")
	path := f.write(t, "/w", "cc-session-0001.jsonl", b.String())
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Size() <= ccMessagesTailBytes {
		t.Fatalf("fixture is %d bytes, needs to exceed the %d-byte window", fi.Size(), ccMessagesTailBytes)
	}

	msgs, ok := f.store().Messages("cc-session-0001")
	if !ok {
		t.Fatal("Messages reported the transcript missing")
	}
	if len(msgs) != 1 || msgs[0].Text != "NEWEST-TURN" {
		t.Fatalf("got %+v, want only the newest turn", msgs)
	}
}

// TestMessagesWidensWhenTheTailIsAllToolPayload — the byte window is on RAW
// JSONL, and cc's bytes are mostly tool payloads this reader then drops. So the
// last 1 MiB of a tool-heavy transcript can hold no conversation at all, and the
// naive answer is an empty chat for a row that has a title and is listed — the
// original bug, one layer down. Found by review, not by the first test pass.
//
// The fixture is built so the FIRST window is guaranteed to yield nothing: over a
// megabyte of tool results after the last real turn.
func TestMessagesWidensWhenTheTailIsAllToolPayload(t *testing.T) {
	toolResult := func() string {
		return ccLine(t, map[string]any{
			"type":      "user",
			"timestamp": "2026-08-17T03:00:00.000Z",
			"message": map[string]any{"role": "user", "content": []any{
				map[string]any{"type": "tool_result", "tool_use_id": "t1", "content": strings.Repeat("z", 32<<10)},
			}},
		})
	}
	var b strings.Builder
	b.WriteString(ccUser(t, "BURIED-BEHIND-THE-TOOLS"))
	b.WriteString(ccAssistant(t, "ANSWER-BEHIND-THE-TOOLS"))
	for b.Len() < ccMessagesTailBytes+(256<<10) {
		b.WriteString(toolResult())
	}

	f := newCCFixture(t, "/w")
	f.write(t, "/w", "cc-session-0001.jsonl", b.String())

	msgs, ok := f.store().Messages("cc-session-0001")
	if !ok {
		t.Fatal("Messages reported the transcript missing")
	}
	if len(msgs) != 2 {
		t.Fatalf("got %d turns, want 2 — the window did not widen past the tool payload: %+v", len(msgs), msgs)
	}
	if msgs[0].Text != "BURIED-BEHIND-THE-TOOLS" {
		t.Errorf("msgs[0] = %q", msgs[0].Text)
	}
}

// TestMessagesCapsTheCount — a transcript of very short records fits the byte
// window many times over; the count cap is the second bound, and it keeps the
// NEWEST turns for the same reason the window is the tail.
func TestMessagesCapsTheCount(t *testing.T) {
	var b strings.Builder
	for i := 0; i < ccMessagesMax+50; i++ {
		b.WriteString(ccUser(t, fmt.Sprintf("turn-%04d", i)))
	}
	f := newCCFixture(t, "/w")
	f.write(t, "/w", "cc-session-0001.jsonl", b.String())

	msgs, _ := f.store().Messages("cc-session-0001")
	if len(msgs) != ccMessagesMax {
		t.Fatalf("got %d messages, want the cap of %d", len(msgs), ccMessagesMax)
	}
	if want := fmt.Sprintf("turn-%04d", ccMessagesMax+49); msgs[len(msgs)-1].Text != want {
		t.Errorf("last message = %q, want %q — the cap must keep the newest turns", msgs[len(msgs)-1].Text, want)
	}
}

// TestMessagesDistinguishesMissingFromEmpty — "cc never heard of this session"
// and "cc has it and nothing in the window is a turn" are different answers, and
// the route above uses the difference.
func TestMessagesDistinguishesMissingFromEmpty(t *testing.T) {
	f := newCCFixture(t, "/w")
	f.write(t, "/w", "cc-session-0001.jsonl", ccPreamble(t, 64))

	if msgs, ok := f.store().Messages("cc-session-0001"); !ok || len(msgs) != 0 {
		t.Errorf("Messages(present but turnless) = %+v, %v; want empty, true", msgs, ok)
	}
	if msgs, ok := f.store().Messages("cc-session-9999"); ok || msgs != nil {
		t.Errorf("Messages(absent) = %+v, %v; want nil, false", msgs, ok)
	}
}

// ---------------------------------------------------------------------------
// Safety
// ---------------------------------------------------------------------------

// TestFindRejectsATraversingSid — sid comes straight off the URL of
// GET /api/v1/sessions/<sid>/messages and is about to be joined into a path.
func TestFindRejectsATraversingSid(t *testing.T) {
	f := newCCFixture(t, "/w")
	f.write(t, "/w", "cc-session-0001.jsonl", ccUser(t, "hello"))
	// A readable .jsonl one level up from the project directory: reachable by
	// traversal, and by nothing else.
	if err := os.WriteFile(filepath.Join(f.root, "secret.jsonl"), []byte(ccUser(t, "not yours")), 0o600); err != nil {
		t.Fatal(err)
	}

	for _, sid := range []string{"../secret", "..%2Fsecret", "/etc/passwd", "cc/session", "a", ""} {
		if got := f.store().Has(sid); got {
			t.Errorf("Has(%q) = true, want false", sid)
		}
		if _, ok := f.store().Messages(sid); ok {
			t.Errorf("Messages(%q) found something", sid)
		}
	}
	// Control: the guard is not simply refusing everything.
	if !f.store().Has("cc-session-0001") {
		t.Error("Has(valid sid) = false — the guard rejects legitimate sids too")
	}
}

// TestCCStoreNeverWrites is the assertion behind this package's central safety
// claim. The store is pointed at a real directory tree and every read path is
// exercised; afterwards the tree must be byte-for-byte and entry-for-entry what
// it was.
//
// It would fail today for one specific mistake that is easy to make and silent
// when made: reusing agent.ccSettingsPath (which MkdirAll's what it returns) to
// locate this store, so that merely listing a workspace cc has never been used in
// creates a directory inside the user's data.
func TestCCStoreNeverWrites(t *testing.T) {
	f := newCCFixture(t, "/w", "/never-used-by-cc")
	f.write(t, "/w", "cc-session-0001.jsonl", ccUser(t, "hello")+ccAssistant(t, "hi"))
	f.write(t, "/w", filepath.Join("cc-session-0001", "cc-subagent-0001.jsonl"), ccUser(t, "nested"))

	before := snapshotTree(t, f.root)

	s := f.store()
	_ = s.List()
	_, _ = s.Messages("cc-session-0001")
	_, _ = s.Messages("cc-session-9999")
	_ = s.Has("cc-session-0001")
	_ = s.Has("../escape")

	after := snapshotTree(t, f.root)
	if len(before) != len(after) {
		t.Fatalf("the tree changed shape: %d entries before, %d after\nbefore=%v\nafter=%v",
			len(before), len(after), before, after)
	}
	for path, sum := range before {
		got, ok := after[path]
		if !ok {
			t.Errorf("%s disappeared", path)
			continue
		}
		if got != sum {
			t.Errorf("%s changed: %s -> %s", path, sum, got)
		}
	}
}

// snapshotTree maps every path under root to a digest of what it is. Directories
// are recorded too, so a directory that did not exist before cannot be created
// unnoticed.
func snapshotTree(t *testing.T, root string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if d.IsDir() {
			out[rel] = "dir"
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(data)
		out[rel] = hex.EncodeToString(sum[:])
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// ---------------------------------------------------------------------------
// Degenerate configurations
// ---------------------------------------------------------------------------

// TestCCStoreWithoutConfigurationListsNothing — a daemon that cannot tell where
// the store is, or which directories are the user's, must produce an empty list
// rather than guessing at either.
func TestCCStoreWithoutConfigurationListsNothing(t *testing.T) {
	var nilStore *CCStore
	if rows := nilStore.List(); rows != nil {
		t.Errorf("nil store List() = %+v", rows)
	}
	if nilStore.Has("cc-session-0001") {
		t.Error("nil store Has() = true")
	}
	if rows := NewCCStore("", func() []string { return []string{"/w"} }).List(); rows != nil {
		t.Errorf("no projects dir: List() = %+v", rows)
	}
	if rows := NewCCStore(t.TempDir(), nil).List(); rows != nil {
		t.Errorf("no workdirs: List() = %+v", rows)
	}
	if rows := NewCCStore(t.TempDir(), func() []string { return nil }).List(); rows != nil {
		t.Errorf("empty workdirs: List() = %+v", rows)
	}
}

// TestCCProjectsDir keeps the one path-building rule in one place.
func TestCCProjectsDir(t *testing.T) {
	if got, want := CCProjectsDir("/home/u/.cc"), filepath.Join("/home/u/.cc", "projects"); got != want {
		t.Errorf("CCProjectsDir = %q, want %q", got, want)
	}
	if got := CCProjectsDir(""); got != "" {
		t.Errorf("CCProjectsDir(\"\") = %q, want empty — an unknown config dir is not the filesystem root", got)
	}
}

// TestListSkipsFilesThatAreNotTranscripts — the project directory holds other
// things; only <valid sid>.jsonl is a session.
func TestListSkipsFilesThatAreNotTranscripts(t *testing.T) {
	f := newCCFixture(t, "/w")
	f.write(t, "/w", "cc-session-0001.jsonl", ccUser(t, "real"))
	f.write(t, "/w", "notes.txt", "not a transcript")
	f.write(t, "/w", "short.jsonl", ccUser(t, "sid too short to be valid"))
	f.write(t, "/w", "cc-session-0002.jsonl", "") // empty: nothing to show

	rows := f.store().List()
	if len(rows) != 1 || rows[0].Sid != "cc-session-0001" {
		t.Errorf("List() = %+v, want only cc-session-0001", rows)
	}
}
