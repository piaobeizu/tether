package session

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// Every fixture in this file is SYNTHETIC and built under t.TempDir() — see
// CCStore's doc for why that is a safety property and not a preference, and
// TestCCStoreNeverWrites for the assertion that backs it up.
//
// ONE test reads a real store, and only when told to: TestEnumerateUserRecordShapes
// takes a census when TETHER_CC_CENSUS_DIR is set, skips otherwise, and opens files
// for reading and nothing else. This paragraph exists because the sentence above it
// used to end "…nothing here reads or points at a real cc store", and tether#95 made
// that false. A file header that is 95% true is how a reader ends up trusting the
// wrong half.

// ---------------------------------------------------------------------------
// Fixtures
// ---------------------------------------------------------------------------

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

// ccAssistantWords builds a text-ONLY assistant record — the shape cc actually
// writes, measured: a record's blocks are `text`, `tool_use` or `thinking`, each
// alone, never mixed.
//
// ccAssistant above is deliberately the mixed shape instead, so that the block
// filter is exercised by every conversion test. This one exists because a fixture
// that mixes them cannot be split into a text-only control arm — see
// ccStripToolCalls — and because tether#96's merge rule is about records that hold
// one kind or the other.
func ccAssistantWords(t *testing.T, text, ts string) string {
	return ccLine(t, map[string]any{
		"type":      "assistant",
		"timestamp": ts,
		"message": map[string]any{"role": "assistant", "content": []any{
			map[string]any{"type": "text", "text": text},
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

// ---------------------------------------------------------------------------
// The four shapes tether#95 found. One builder each, so a test that omits one
// omits it visibly.
// ---------------------------------------------------------------------------

// ccTaskNotification builds the record the harness writes when a background task
// finishes and the completion is fed back to cc as a user turn: type "user", no
// isMeta, plain string content. 364 of these in the census — 84% of all the noise
// — and 355 of them sat between two assistant fragments, so this is also the
// record the merge seam is tested with.
//
// The body carries the fields a real one carries, including the enormous <result>
// that made these the biggest of the four by bytes as well as by count.
func ccTaskNotification(t *testing.T, summary, result string) string {
	return ccLine(t, map[string]any{
		"type":      "user",
		"timestamp": "2026-08-17T03:00:00.000Z",
		"message": map[string]any{"role": "user", "content": ccTaskNotificationPrefix + "\n" +
			"<task-id>a3d641c4201f0866a</task-id>\n" +
			"<tool-use-id>toolu_01JYfrdErMDTRUQG8P62d91c</tool-use-id>\n" +
			"<output-file>/tmp/cc/tasks/a3d641c4201f0866a.output</output-file>\n" +
			"<status>completed</status>\n" +
			"<summary>" + summary + "</summary>\n" +
			"<result>" + result + "</result>\n</task-notification>"},
	})
}

// ccBashInput builds the record cc writes when the user runs a shell command with
// the `!` prefix. Real records carry a leading space inside the tag, which is why
// every one of them is written that way here: a fixture without it would not
// notice a renderer that forgot to trim.
func ccBashInput(t *testing.T, cmd string) string {
	return ccLine(t, map[string]any{
		"type":      "user",
		"timestamp": "2026-08-17T03:00:00.000Z",
		"message":   map[string]any{"role": "user", "content": ccBashInputPrefix + " " + cmd + ccBashInputSuffix},
	})
}

// ccBashOutput builds that command's output. Real records carry stdout and stderr
// in one record, stderr always second and often empty — 22 of 22 — so the fixture
// does too.
func ccBashOutput(t *testing.T, stdout, stderr string) string {
	return ccLine(t, map[string]any{
		"type":      "user",
		"timestamp": "2026-08-17T03:00:00.000Z",
		"message": map[string]any{"role": "user", "content": ccBashStdoutPrefix + stdout + "</bash-stdout>" +
			ccBashStderrPrefix + stderr + "</bash-stderr>"},
	})
}

// ccInterrupt builds cc's interrupt marker. qualifier is "" for the common form
// and "for tool use" for the one the census saw twice.
//
// The content is a text-block ARRAY, not a plain string, and that is measured
// rather than stylistic: of the six shapes ccUserShapes knows, this is the only
// one cc writes that way — 25 of 25 — while the other five and 2,713 genuine
// human turns all arrive as plain strings. This builder had it as a string until
// that count was taken, which would have left the array path for this shape
// unexercised by any fixture.
func ccInterrupt(t *testing.T, qualifier string) string {
	body := ccInterruptPrefix
	if qualifier != "" {
		body += " " + qualifier
	}
	return ccLine(t, map[string]any{
		"type":      "user",
		"timestamp": "2026-08-17T03:00:00.000Z",
		"message": map[string]any{"role": "user", "content": []any{
			map[string]any{"type": "text", "text": body + "]"},
		}},
	})
}

// ccSlashCommandRecord builds a slash command the user typed — the NEGATIVE
// CONTROL for every drop above. It is markup too, and it is a real user turn:
// #95's first enumeration counted 221 of these as defects before it re-measured
// through userText.
func ccSlashCommandRecord(t *testing.T, name, args string) string {
	return ccLine(t, map[string]any{
		"type":      "user",
		"timestamp": "2026-08-17T03:00:00.000Z",
		"message": map[string]any{"role": "user", "content": "<command-message>" + strings.TrimPrefix(name, "/") +
			"</command-message>\n" + ccCommandNameTag + name + "</command-name>\n<command-args>" + args + "</command-args>"},
	})
}

// ---------------------------------------------------------------------------
// Tool activity (tether#96). One builder per real shape, including the BROKEN
// pairings — a fixture of well-formed pairs exercises none of the failure modes
// this reader actually meets.
// ---------------------------------------------------------------------------

// ccToolUse builds the record cc writes when the agent calls something: an
// assistant record whose content is tool_use blocks and NOTHING ELSE.
//
// That is measured, not simplified. Across a real store the block combinations on
// a record are `text`, `tool_use` and `thinking`, each of them alone — cc never
// mixes text and a call in one record. It is the reason a tool record produced no
// message at all before tether#96, and the reason the merge has to absorb one.
//
// Several blocks in one record is the parallel-call shape, so the signature takes
// as many as the caller wants.
func ccToolUse(t *testing.T, ts string, calls ...map[string]any) string {
	t.Helper()
	blocks := make([]any, 0, len(calls))
	for _, c := range calls {
		b := map[string]any{"type": "tool_use"}
		for k, v := range c {
			b[k] = v
		}
		blocks = append(blocks, b)
	}
	return ccLine(t, map[string]any{
		"type":      "assistant",
		"timestamp": ts,
		"message":   map[string]any{"role": "assistant", "content": blocks},
	})
}

// ccCall is one tool_use block, in the field shape real records carry (`caller` is
// on 3,603 of 3,897 measured and is ignored by this reader, so it is here to prove
// that).
func ccCall(id, name string, input map[string]any) map[string]any {
	return map[string]any{
		"id": id, "name": name, "input": input,
		"caller": map[string]any{"type": "direct"},
	}
}

// ccToolFailure builds a FAILED tool result: type:"user", a tool_result block with
// is_error. 1,699 of 3,887 measured results carry the flag at all, so both the
// present and absent forms are real and the fixtures use both (ccToolResult is the
// successful one).
func ccToolFailure(t *testing.T, toolUseID, content string) string {
	return ccLine(t, map[string]any{
		"type":      "user",
		"timestamp": "2026-08-17T03:00:00.000Z",
		"message": map[string]any{"role": "user", "content": []any{
			map[string]any{"type": "tool_result", "tool_use_id": toolUseID,
				"content": content, "is_error": true},
		}},
	})
}

// ccToolResultBlocks builds a result whose content is an ARRAY of blocks rather
// than a string — 1,237 of 3,887 measured, carrying `text`, `tool_reference` and
// `image` sub-blocks. A fixture that only used the string form would leave the
// flattening rule unexercised, and a reader that served the array raw would put a
// JSON literal in front of the user.
func ccToolResultBlocks(t *testing.T, toolUseID string, texts ...string) string {
	blocks := []any{map[string]any{"type": "tool_reference", "tool_name": "Bash"}}
	for _, s := range texts {
		blocks = append(blocks, map[string]any{"type": "text", "text": s})
	}
	return ccLine(t, map[string]any{
		"type":      "user",
		"timestamp": "2026-08-17T03:00:00.000Z",
		"message": map[string]any{"role": "user", "content": []any{
			map[string]any{"type": "tool_result", "tool_use_id": toolUseID,
				"content": blocks, "is_error": true},
		}},
	})
}

// ccThinkingRecord builds an extended-thinking record in the shape the real store
// holds: a `thinking` block whose text is EMPTY, with the reasoning encrypted into
// `signature`. 22,350 of 22,350 across all 93 top-level transcripts of one store
// (2026-08-17) are exactly this, which is why nothing here serves thinking.
func ccThinkingRecord(t *testing.T, thinking string) string {
	return ccLine(t, map[string]any{
		"type":      "assistant",
		"timestamp": "2026-08-17T03:00:00.500Z",
		"message": map[string]any{"role": "assistant", "content": []any{
			map[string]any{"type": "thinking", "thinking": thinking,
				"signature": "CAIStwYKhwEIEBgCKkAtCLIm44jNGjGEr95Lhkbr"},
		}},
	})
}

// ccStripToolCalls removes every record that carries a tool_use block, which
// reproduces EXACTLY the population the reader served before tether#96: such a
// record had no text in it, so the old reader dropped it. It is the control arm of
// the A/B in TestToolsDoNotShrinkTheVisibleTranscript — a text-only baseline taken
// from the same fixture, so the comparison needs no second build of the daemon.
func ccStripToolCalls(body string) string {
	var out strings.Builder
	for _, line := range strings.SplitAfter(body, "\n") {
		if strings.Contains(line, `"tool_use"`) {
			continue
		}
		out.WriteString(line)
	}
	return out.String()
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

// TestMessagesServesToolActivityWithoutItsPayload (tether#96, replacing
// TestMessagesDropsToolActivity) — a tool call reaches the turn as a
// ToolCallRecord, and a SUCCESSFUL result's output does not reach it at all.
//
// Both halves matter and they pull opposite ways. The first is the feature. The
// second is the byte decision: a successful result is 3.33 MB of the 4.85 MB of raw
// tool payload inside the 39 real 1 MiB windows of one store, and serving it
// truncated would make summarizeToolResult report a wrong line count. The old test
// asserted that NEITHER arrived, which is why its name had to go rather than its
// fixture.
func TestMessagesServesToolActivityWithoutItsPayload(t *testing.T) {
	f := newCCFixture(t, "/w")
	f.write(t, "/w", "cc-session-0001.jsonl",
		ccUser(t, "run it")+ccAssistant(t, "running")+ccToolResult(t, "SECRET-TOOL-OUTPUT"))

	msgs, _ := f.store().Messages("cc-session-0001")
	if len(msgs) != 2 {
		t.Fatalf("got %d messages, want 2 (the tool result is not a turn): %+v", len(msgs), msgs)
	}
	for _, m := range msgs {
		if strings.Contains(m.Text, "SECRET-TOOL-OUTPUT") || strings.Contains(m.Text, `"tool_use"`) {
			t.Errorf("tool activity leaked into a turn's TEXT: %q", m.Text)
		}
	}
	if len(msgs[0].Tools) != 0 {
		t.Errorf("the user's turn carries tools: %+v", msgs[0].Tools)
	}
	tools := msgs[1].Tools
	if len(tools) != 1 {
		t.Fatalf("the answer carries %d tools, want 1: %+v", len(tools), tools)
	}
	if tools[0].Name != "Bash" || tools[0].ID != "t1" {
		t.Errorf("tool = %q/%q, want Bash/t1", tools[0].Name, tools[0].ID)
	}
	if got := string(tools[0].Input); got != `{"command":"ls"}` {
		t.Errorf("input = %s, want the projected command", got)
	}
	// The successful result is on disk and matched this very call by id. It is not
	// served, and that is the decision under test — not an accident of the fixture.
	if tools[0].Result != nil {
		t.Errorf("a SUCCESSFUL result was served: %+v", tools[0].Result)
	}
}

// TestToolsDoNotShrinkTheVisibleTranscript is the A/B this change is most able to
// fool itself on: tool cards appear while the conversation silently gets shorter,
// which looks like success in a screenshot.
//
// The control arm is the SAME fixture with its tool_use records removed, which
// reproduces exactly what the reader served before tether#96 — such a record had no
// text in it, so the old reader dropped it. So the comparison needs no second build
// of the daemon, and it compares the two things that can shrink: how many messages
// come back, and what words are in them.
//
// The fixture is built to contain every case that could add or lose a message: a
// call BEFORE the turn's first words, a call after them, a turn that is nothing but
// calls, and a second user turn afterwards so the boundary is exercised too.
func TestToolsDoNotShrinkTheVisibleTranscript(t *testing.T) {
	body := ccUser(t, "把这个查清楚") +
		ccToolUse(t, "2026-08-17T04:57:00.000Z", ccCall("t1", "Read", map[string]any{"file_path": "a.go"})) +
		ccToolResult(t, "package main") +
		ccAssistantWords(t, "查清楚:", "2026-08-17T04:57:30.000Z") +
		ccToolUse(t, "2026-08-17T04:58:00.000Z", ccCall("t2", "Bash", map[string]any{"command": "go test ./..."})) +
		ccToolResult(t, "ok") +
		ccAssistantWords(t, "先看进度:", "2026-08-17T04:59:00.000Z") +
		ccUser(t, "继续") +
		// A turn that says nothing at all: only calls. This is the one shape that
		// GAINS a message, which is the direction that is fine.
		ccToolUse(t, "2026-08-17T05:00:00.000Z", ccCall("t3", "Grep", map[string]any{"pattern": "TODO"})) +
		ccToolResult(t, "none")

	f := newCCFixture(t, "/w")
	f.write(t, "/w", "cc-session-0001.jsonl", body)
	f.write(t, "/w", "cc-session-0002.jsonl", ccStripToolCalls(body))

	after, _ := f.store().Messages("cc-session-0001")
	before, _ := f.store().Messages("cc-session-0002")

	if len(before) == 0 {
		t.Fatal("the control arm produced nothing — the stripped fixture is not a baseline")
	}
	if len(after) < len(before) {
		t.Fatalf("VISIBLE HISTORY SHRANK: %d messages with tools, %d without\nwith:    %+v\nwithout: %+v",
			len(after), len(before), after, before)
	}
	// Every message the control produced must still be there, in order, word for
	// word. Comparing only the counts would pass a change that replaced a turn's
	// text with a tool card.
	var withWords []HistoryMessage
	for _, m := range after {
		if m.Text != "" {
			withWords = append(withWords, m)
		}
	}
	if len(withWords) != len(before) {
		t.Fatalf("%d messages carry words, control had %d\nwith words: %+v\ncontrol:    %+v",
			len(withWords), len(before), withWords, before)
	}
	for i := range before {
		if withWords[i].Text != before[i].Text {
			t.Errorf("message %d text = %q, control had %q", i, withWords[i].Text, before[i].Text)
		}
		if withWords[i].Role != before[i].Role {
			t.Errorf("message %d role = %q, control had %q", i, withWords[i].Role, before[i].Role)
		}
	}
	// And the change has to have DONE something, or the assertions above are
	// satisfied by a no-op.
	served := 0
	for _, m := range after {
		served += len(m.Tools)
	}
	if served != 3 {
		t.Errorf("%d tool calls served, want 3 — the A/B above passes trivially for a no-op", served)
	}
	for _, m := range before {
		if len(m.Tools) != 0 {
			t.Errorf("the control arm carries tools, so it is not a text-only baseline: %+v", m.Tools)
		}
	}
}

// TestToolsMergeWithoutALeadingBlankLine — a turn's FIRST record is a tool call,
// because cc never puts text and a call in the same record. So the bubble is
// created with an empty Text and the words arrive afterwards; joining with
// ccTurnJoin unconditionally would open every such turn with a blank paragraph.
func TestToolsMergeWithoutALeadingBlankLine(t *testing.T) {
	f := newCCFixture(t, "/w")
	f.write(t, "/w", "cc-session-0001.jsonl", ccUser(t, "go")+
		ccToolUse(t, "2026-08-17T03:00:01.000Z", ccCall("t1", "Read", map[string]any{"file_path": "a.go"}))+
		ccToolResult(t, "ok")+
		ccAssistantWords(t, "done", "2026-08-17T03:00:02.000Z"))

	msgs, _ := f.store().Messages("cc-session-0001")
	if len(msgs) != 2 {
		t.Fatalf("got %d messages, want 2: %+v", len(msgs), msgs)
	}
	if msgs[1].Text != "done" {
		t.Errorf("answer text = %q, want %q — a leading join makes the bubble open blank", msgs[1].Text, "done")
	}
	// The stamp must still be the FIRST fragment's, which is now the tool record:
	// that is when the response began. Same rule as tether#94.
	if want := ccTimestampMillis("2026-08-17T03:00:01.000Z"); msgs[1].Ts != want {
		t.Errorf("Ts = %d, want %d (the first fragment's, which is the tool call)", msgs[1].Ts, want)
	}
}

// TestToolFailureAttachesToItsCall — a failed result is the one result that IS
// served, because "the agent tried X and it broke" is what explains the next thing
// it did. It arrives in a LATER record than its call, so this also pins the id
// lookup across records.
func TestToolFailureAttachesToItsCall(t *testing.T) {
	f := newCCFixture(t, "/w")
	f.write(t, "/w", "cc-session-0001.jsonl", ccUser(t, "build it")+
		ccToolUse(t, "2026-08-17T03:00:01.000Z",
			ccCall("ok1", "Read", map[string]any{"file_path": "a.go"}),
			ccCall("bad1", "Bash", map[string]any{"command": "make"}))+
		ccToolResult(t, "package main")+
		ccToolFailure(t, "bad1", "make: *** No rule to make target")+
		ccAssistantWords(t, "the build is broken", "2026-08-17T03:00:03.000Z"))

	msgs, _ := f.store().Messages("cc-session-0001")
	if len(msgs) != 2 {
		t.Fatalf("got %d messages, want 2: %+v", len(msgs), msgs)
	}
	tools := msgs[1].Tools
	if len(tools) != 2 {
		t.Fatalf("got %d tools, want 2 (both blocks of one parallel record): %+v", len(tools), tools)
	}
	if tools[0].Result != nil {
		t.Errorf("the succeeding call carries a result: %+v", tools[0].Result)
	}
	if tools[1].Result == nil {
		t.Fatal("the FAILING call carries no result — a failure is the one result served")
	}
	if !tools[1].Result.IsError {
		t.Error("Result.IsError = false on a failure")
	}
	if !strings.Contains(tools[1].Result.Content, "No rule to make target") {
		t.Errorf("Result.Content = %q, want the failure message", tools[1].Result.Content)
	}
}

// TestToolFailureFromBlockContent — cc writes a result's content as a plain string
// on 2,650 of 3,887 measured records and as an ARRAY of blocks on the other 1,237,
// carrying text / tool_reference / image sub-blocks. A reader that only handled the
// string form would put a JSON literal in front of the user.
func TestToolFailureFromBlockContent(t *testing.T) {
	f := newCCFixture(t, "/w")
	f.write(t, "/w", "cc-session-0001.jsonl", ccUser(t, "go")+
		ccToolUse(t, "2026-08-17T03:00:01.000Z", ccCall("t1", "Bash", map[string]any{"command": "false"}))+
		ccToolResultBlocks(t, "t1", "exit status 1", "nothing else worked"))

	msgs, _ := f.store().Messages("cc-session-0001")
	if len(msgs) != 2 {
		t.Fatalf("got %d messages, want 2: %+v", len(msgs), msgs)
	}
	if len(msgs[1].Tools) != 1 {
		t.Fatalf("the answer carries %d tools, want 1: %+v", len(msgs[1].Tools), msgs[1].Tools)
	}
	got := msgs[1].Tools[0].Result
	if got == nil {
		t.Fatal("no result served for a block-shaped failure")
	}
	if want := "exit status 1\nnothing else worked"; got.Content != want {
		t.Errorf("Content = %q, want %q — the blocks are flattened, not serialised", got.Content, want)
	}
	if strings.Contains(got.Content, "tool_reference") {
		t.Errorf("a non-text sub-block reached the user: %q", got.Content)
	}
}

// TestBrokenToolPairingsSurviveTheWindow — a tail is cut wherever the window falls,
// so BOTH halves of a pair go missing in the ordinary case. Measured on one real
// transcript: 95 calls against 96 results inside the window, i.e. one orphan
// result, and an interrupted run leaves a call that never came back.
//
// Neither is a defect and neither may produce a phantom row.
func TestBrokenToolPairingsSurviveTheWindow(t *testing.T) {
	f := newCCFixture(t, "/w")
	f.write(t, "/w", "cc-session-0001.jsonl", ccUser(t, "go")+
		// A result whose call is not in this window at all.
		ccToolFailure(t, "call-from-last-week", "boom")+
		// A call the run was interrupted before answering.
		ccToolUse(t, "2026-08-17T03:00:01.000Z", ccCall("t9", "Bash", map[string]any{"command": "sleep 600"}))+
		ccInterrupt(t, ""))

	msgs, _ := f.store().Messages("cc-session-0001")
	// user "go", the tool-only assistant turn, the interrupt marker.
	if len(msgs) != 3 {
		t.Fatalf("got %d messages, want 3: %+v", len(msgs), msgs)
	}
	for _, m := range msgs {
		if strings.Contains(m.Text, "boom") {
			t.Errorf("an orphan result reached a turn's text: %q", m.Text)
		}
		for _, tc := range m.Tools {
			if tc.Name == "" {
				t.Errorf("an orphan result was served as a nameless call: %+v", tc)
			}
			if tc.ID == "call-from-last-week" {
				t.Errorf("an orphan result invented a call: %+v", tc)
			}
		}
	}
	if len(msgs[1].Tools) != 1 {
		t.Fatalf("the tool-only turn carries %d tools, want 1: %+v", len(msgs[1].Tools), msgs[1].Tools)
	}
	if msgs[1].Tools[0].Result != nil {
		t.Errorf("the interrupted call has a result: %+v — 'never came back' must stay visible as absent",
			msgs[1].Tools[0].Result)
	}
	if msgs[2].Text != "(interrupted)" {
		t.Errorf("msgs[2] = %q, want the interrupt marker", msgs[2].Text)
	}
}

// TestToolResultRecordsAreStillNotTurns — a tool_result arrives wearing
// type:"user", so the record that reports a FAILURE is a record that must emit no
// user bubble. tether#96 made such a record carry information for the first time,
// which is exactly when "and it also emits nothing" stops being automatic.
func TestToolResultRecordsAreStillNotTurns(t *testing.T) {
	f := newCCFixture(t, "/w")
	f.write(t, "/w", "cc-session-0001.jsonl",
		ccToolUse(t, "2026-08-17T03:00:01.000Z", ccCall("t1", "Bash", map[string]any{"command": "false"}))+
			ccToolFailure(t, "t1", "exit 1")+
			ccToolResult(t, "and a successful one"))

	msgs, _ := f.store().Messages("cc-session-0001")
	if len(msgs) != 1 {
		t.Fatalf("got %d messages, want 1 (the assistant's call): %+v", len(msgs), msgs)
	}
	if msgs[0].Role != "assistant" {
		t.Errorf("a tool result produced a %q message", msgs[0].Role)
	}
}

// TestThinkingIsNotServedFromCC pins the decision AND the measurement it rests on.
//
// Measured across all 93 top-level transcripts of one real store (2026-08-17, cc
// 2.1.233): 22,350 of 22,350 thinking blocks carry `"thinking": ""`, with the
// reasoning encrypted into the block's `signature`. So there is no text to serve
// and converting the block would give every turn a "thought" toggle that expands to
// nothing.
//
// The fixture also passes a NON-empty thinking block, because a test built only
// from empty ones would keep passing if the reader started serving thinking
// verbatim — it would just have nothing to show. This asserts the decision, not the
// data.
func TestThinkingIsNotServedFromCC(t *testing.T) {
	f := newCCFixture(t, "/w")
	f.write(t, "/w", "cc-session-0001.jsonl", ccUser(t, "go")+
		ccThinkingRecord(t, "")+
		ccThinkingRecord(t, "SECRET-REASONING")+
		ccAssistantWords(t, "answer", "2026-08-17T03:00:02.000Z"))

	msgs, _ := f.store().Messages("cc-session-0001")
	if len(msgs) != 2 {
		t.Fatalf("got %d messages, want 2 (a thinking record is not a turn): %+v", len(msgs), msgs)
	}
	for i, m := range msgs {
		if m.Thinking != "" {
			t.Errorf("msgs[%d].Thinking = %q, want empty — see ccMessagesFrom for the measurement", i, m.Thinking)
		}
		if strings.Contains(m.Text, "SECRET-REASONING") {
			t.Errorf("msgs[%d] served thinking as text: %q", i, m.Text)
		}
	}
}

// TestToolInputIsProjected — the bound on what a call's arguments cost.
func TestToolInputIsProjected(t *testing.T) {
	long := strings.Repeat("a", ccToolInputValueRunes+50)
	cjk := strings.Repeat("查", ccToolInputValueRunes+50)
	many := map[string]any{}
	for i := 0; i < ccToolInputMaxKeys+5; i++ {
		many[fmt.Sprintf("k%02d", i)] = "v"
	}

	for _, tc := range []struct {
		name  string
		input any
		check func(t *testing.T, got json.RawMessage)
	}{
		{
			name:  "a value past the cap is a prefix of itself",
			input: map[string]any{"command": long},
			check: func(t *testing.T, got json.RawMessage) {
				var m map[string]string
				mustUnmarshal(t, got, &m)
				if n := utf8.RuneCountInString(m["command"]); n != ccToolInputValueRunes+1 {
					t.Errorf("kept %d runes, want %d + the ellipsis", n, ccToolInputValueRunes)
				}
				if !strings.HasPrefix(long, strings.TrimSuffix(m["command"], "…")) {
					t.Error("the kept value is not a prefix of the original")
				}
			},
		},
		{
			// A byte cap would keep a third as many characters here as it does for
			// ASCII — and the transcript that prompted this whole change is in CJK.
			name:  "the cap counts runes, not bytes",
			input: map[string]any{"command": cjk},
			check: func(t *testing.T, got json.RawMessage) {
				var m map[string]string
				mustUnmarshal(t, got, &m)
				if n := utf8.RuneCountInString(m["command"]); n != ccToolInputValueRunes+1 {
					t.Errorf("kept %d runes of CJK, want %d + the ellipsis", n, ccToolInputValueRunes)
				}
				if !utf8.ValidString(m["command"]) {
					t.Error("the cut landed mid-rune")
				}
			},
		},
		{
			name: "non-string values are dropped, whole",
			input: map[string]any{
				"file_path": "a.go",
				"edits":     []any{map[string]any{"old_string": strings.Repeat("z", 4096)}},
				"limit":     2000,
				"deep":      map[string]any{"content": strings.Repeat("y", 4096)},
			},
			check: func(t *testing.T, got json.RawMessage) {
				if s := string(got); s != `{"file_path":"a.go"}` {
					t.Errorf("projection = %s, want only the string argument", s)
				}
			},
		},
		{
			name:  "the key cap binds, deterministically",
			input: many,
			check: func(t *testing.T, got json.RawMessage) {
				var m map[string]string
				mustUnmarshal(t, got, &m)
				if len(m) != ccToolInputMaxKeys {
					t.Fatalf("kept %d keys, want %d", len(m), ccToolInputMaxKeys)
				}
				// Alphabetical, so two runs over the same record agree.
				if _, ok := m["k00"]; !ok {
					t.Error("k00 was dropped, so the surviving keys are not the alphabetically first")
				}
				if _, ok := m[fmt.Sprintf("k%02d", ccToolInputMaxKeys)]; ok {
					t.Error("a key past the cap survived")
				}
			},
		},
		{
			name:  "an input with no strings in it is omitted entirely",
			input: map[string]any{"limit": 10, "offset": 20},
			check: func(t *testing.T, got json.RawMessage) {
				if got != nil {
					t.Errorf("projection = %s, want nil so omitempty drops the field", got)
				}
			},
		},
		{
			name:  "a non-object input is omitted rather than passed through",
			input: []any{"not", "an", "object"},
			check: func(t *testing.T, got json.RawMessage) {
				if got != nil {
					t.Errorf("projection = %s, want nil", got)
				}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw, err := json.Marshal(tc.input)
			if err != nil {
				t.Fatal(err)
			}
			got := ccProjectToolInput(raw)
			if got != nil && !json.Valid(got) {
				t.Fatalf("projection is not valid JSON: %s", got)
			}
			tc.check(t, got)
		})
	}
}

// mustUnmarshal fails the test rather than letting a decode error be read as an
// empty map, which would make every assertion above vacuously true.
func mustUnmarshal(t *testing.T, raw json.RawMessage, into any) {
	t.Helper()
	if err := json.Unmarshal(raw, into); err != nil {
		t.Fatalf("decoding %s: %v", raw, err)
	}
}

// TestToolFailureContentIsCappedOnARuneBoundary — the failure message is bounded in
// BYTES, because it is bounding a response. Cutting mid-rune would send the user a
// replacement character where their error message was truncated.
func TestToolFailureContentIsCappedOnARuneBoundary(t *testing.T) {
	// 查 is three bytes, so a cap that is not a multiple of three lands inside one.
	body := strings.Repeat("查", ccToolErrorBytes)
	f := newCCFixture(t, "/w")
	f.write(t, "/w", "cc-session-0001.jsonl",
		ccToolUse(t, "2026-08-17T03:00:01.000Z", ccCall("t1", "Bash", map[string]any{"command": "false"}))+
			ccToolFailure(t, "t1", body))

	msgs, _ := f.store().Messages("cc-session-0001")
	if len(msgs) != 1 || len(msgs[0].Tools) != 1 {
		t.Fatalf("got %d messages with %v tools, want 1 message with 1 tool: %+v",
			len(msgs), func() int {
				if len(msgs) == 0 {
					return 0
				}
				return len(msgs[0].Tools)
			}(), msgs)
	}
	got := msgs[0].Tools[0].Result
	if got == nil {
		t.Fatal("no result served")
	}
	if !utf8.ValidString(got.Content) {
		t.Error("the cut landed mid-rune")
	}
	if len(got.Content) > ccToolErrorBytes+len(ccTruncated) {
		t.Errorf("Content is %d bytes, cap is %d + the marker", len(got.Content), ccToolErrorBytes)
	}
	if !strings.HasSuffix(got.Content, ccTruncated) {
		t.Errorf("a truncated result does not say so: %q", got.Content[max(0, len(got.Content)-40):])
	}
	if !strings.HasPrefix(body, strings.TrimSuffix(got.Content, ccTruncated)) {
		t.Error("the kept content is not a prefix of the original")
	}
}

// ccServedCallCeiling is the most one call may cost on the wire, derived from the
// caps rather than observed: at most ccToolInputMaxKeys values, each at most
// ccToolInputValueRunes characters, each character at worst ccMaxEscapeBytesPerRune
// bytes once the response encoder has escaped it, plus a generous allowance for
// keys, the id, the name and the JSON around them.
//
// Written down as arithmetic so the ceiling cannot quietly become "whatever the code
// currently produces".
const ccServedCallCeiling = ccToolInputMaxKeys*(ccToolInputValueRunes*ccMaxEscapeBytesPerRune+64) + 256

// TestToolPayloadIsBoundedByItsCaps replaces an earlier test that asserted the
// served record was strictly SMALLER in bytes than the transcript record it came
// from. That is false, and review proved it: Go's encoder turns `<`, `>` and `&` into
// six-byte escapes where cc's writer leaves them as one byte, so a call whose
// arguments are full of ampersands serves more bytes than it was read from.
//
// Worse, the test could never have failed for that reason, because it built its
// "source" with the SAME escaping encoder — the asymmetry cancelled by construction.
// A test that is immune to the failure mode it names is a claim with a green light
// next to it.
//
// So the property asserted here is the one that is actually true and actually
// load-bearing: every call fits a ceiling the caps imply, whatever the input. The
// adversarial rows are the ones that broke the old claim.
func TestToolPayloadIsBoundedByItsCaps(t *testing.T) {
	amp := strings.Repeat("&", 4096)    // 1 byte on disk, 6 on the wire
	tags := strings.Repeat("<x>", 4096) // the shape a human pastes
	for _, tc := range []struct {
		name  string
		block map[string]any
		// cheap, when non-zero, is a TIGHTER ceiling this case must also meet —
		// unlike the derived one, it fails if the projection is removed.
		cheap int
	}{
		{name: "a small call", block: ccCall("t1", "Read", map[string]any{"file_path": "a.go"}), cheap: 128},
		{
			name: "a call whose input is a whole file",
			block: ccCall("t2", "Write", map[string]any{
				"file_path": "a.go", "content": strings.Repeat("line\n", 4096)}),
			// The renderer shows Write's file_path and nothing else, so a 20 KB
			// input has to cost about as much as its path.
			cheap: 512,
		},
		{
			name: "a call with many long arguments",
			block: ccCall("t3", "mcp__x__y", map[string]any{
				"a": strings.Repeat("a", 5000), "b": strings.Repeat("b", 5000),
				"c": strings.Repeat("c", 5000), "d": strings.Repeat("d", 5000)}),
			cheap: 4 * (ccToolInputValueRunes + 32),
		},
		// The two rows that falsify "smaller than its source". No `cheap` ceiling:
		// escaped, these legitimately cost several bytes per kept character, and the
		// point is that the DERIVED ceiling still holds.
		{name: "arguments that the encoder escapes six-fold", block: ccCall("t4", "Bash",
			map[string]any{"command": amp, "description": amp})},
		{name: "arguments full of pasted markup", block: ccCall("t5", "Write",
			map[string]any{"file_path": "a.html", "content": tags})},
		// Both caps at once, which is the only case the DERIVED ceiling is tight
		// against: enough keys that the key cap has to bind, each value long enough
		// and escaped enough that the value cap has to bind too. Added because a
		// mutation that deleted the key cap SURVIVED the first version of this test —
		// no row here had more than four arguments, so the ceiling was never
		// approached and the arithmetic it is built from was never exercised.
		{name: "more escaped arguments than the key cap allows",
			block: ccCall("t8", "mcp__x__y", func() map[string]any {
				in := map[string]any{}
				for i := 0; i < ccToolInputMaxKeys*3; i++ {
					in[fmt.Sprintf("a%02d", i)] = amp
				}
				return in
			}())},
		// No `caller` field: 294 of 3,897 measured records have none, so this is the
		// record shape with the least slack in it.
		{name: "a call with no caller field", block: map[string]any{
			"id": "t6", "name": "Bash", "input": map[string]any{"command": "ls"}}},
		{name: "a name and id and nothing else", block: ccCall("t7", "Bash", map[string]any{})},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.block["type"] = "tool_use"
			source, err := json.Marshal(tc.block)
			if err != nil {
				t.Fatal(err)
			}
			m := &ccMessage{Content: json.RawMessage("[" + string(source) + "]")}
			calls := m.toolCalls()
			if len(calls) != 1 {
				t.Fatalf("got %d calls, want 1", len(calls))
			}
			// Marshalled the way the route marshals it — writeJSON uses an encoder
			// with HTML escaping ON, which is exactly the expansion under test.
			served, err := json.Marshal(calls[0])
			if err != nil {
				t.Fatal(err)
			}
			if len(served) > ccServedCallCeiling {
				t.Errorf("served %d bytes from a %d-byte source, derived ceiling is %d\n  served: %s",
					len(served), len(source), ccServedCallCeiling, served)
			}
			if tc.cheap > 0 && len(served) > tc.cheap {
				t.Errorf("served %d bytes from a %d-byte source, this case's ceiling is %d — the"+
					" projection is not doing its job\n  served: %s",
					len(served), len(source), tc.cheap, served)
			}
		})
	}
}

// TestServedValuesArePrefixesOfTheSource is the property the byte inequality was
// reaching for and getting wrong: nothing is INVENTED. Every character served for a
// call appears, in order, at the start of a value cc wrote — so the characters served
// are a subset of the characters inside the read window, whatever the encoder then
// does to their byte count.
//
// Asserted over characters rather than bytes precisely because escaping makes bytes
// the wrong unit here.
func TestServedValuesArePrefixesOfTheSource(t *testing.T) {
	inputs := []map[string]any{
		{"command": strings.Repeat("&", 4096), "description": "run it"},
		{"file_path": "a.go", "content": strings.Repeat("查询", 4096)},
		{"pattern": "<x>", "path": "."},
		{"command": strings.Repeat("q", ccToolInputValueRunes-1)}, // just under the cap
		{"command": strings.Repeat("q", ccToolInputValueRunes)},   // exactly at it
	}
	for i, in := range inputs {
		raw, err := json.Marshal(in)
		if err != nil {
			t.Fatal(err)
		}
		got := ccProjectToolInput(raw)
		if got == nil {
			t.Fatalf("input %d: projection is nil", i)
		}
		var kept map[string]string
		mustUnmarshal(t, got, &kept)
		for k, v := range kept {
			src, ok := in[k].(string)
			if !ok {
				t.Errorf("input %d: served key %q that is not a source string", i, k)
				continue
			}
			// The ellipsis is the one character this reader adds, and only where it
			// cut. Everything before it must be the source, verbatim.
			body := strings.TrimSuffix(v, "…")
			if body != v && !strings.HasPrefix(src, body) {
				t.Errorf("input %d key %q: %q is not a prefix of the source", i, k, body)
			}
			if body == v && v != src {
				t.Errorf("input %d key %q: served %q but source is %q, and nothing was cut", i, k, v, src)
			}
			if n := utf8.RuneCountInString(body); n > ccToolInputValueRunes {
				t.Errorf("input %d key %q: kept %d runes, cap is %d", i, k, n, ccToolInputValueRunes)
			}
		}
	}
}

// TestServedFailureIsBoundedByItsCap — the other half of the payload. Bytes, not
// runes, because ccToolErrorBytes bounds a response; the encoder's expansion on top
// of it is accounted for the same way as for a call.
func TestServedFailureIsBoundedByItsCap(t *testing.T) {
	for _, body := range []string{
		"",
		"exit 1",
		strings.Repeat("e", ccToolErrorBytes-1),
		strings.Repeat("e", ccToolErrorBytes),
		strings.Repeat("e", ccToolErrorBytes*4),
		strings.Repeat("&", ccToolErrorBytes*4), // six bytes each once escaped
		strings.Repeat("查", ccToolErrorBytes),   // three bytes each on disk
	} {
		block := map[string]any{"type": "tool_result", "tool_use_id": "t1",
			"content": body, "is_error": true}
		source, err := json.Marshal(block)
		if err != nil {
			t.Fatal(err)
		}
		m := &ccMessage{Content: json.RawMessage("[" + string(source) + "]")}
		got := m.errorResults()
		if len(got) != 1 {
			t.Fatalf("%d-byte failure: got %d results, want 1", len(body), len(got))
		}
		if len(got[0].content) > ccToolErrorBytes+len(ccTruncated) {
			t.Errorf("%d-byte failure: kept %d bytes, cap is %d + the marker",
				len(body), len(got[0].content), ccToolErrorBytes)
		}
		if !utf8.ValidString(got[0].content) {
			t.Errorf("%d-byte failure: the cut landed mid-rune", len(body))
		}
		served, err := json.Marshal(ToolResultRecord{Content: got[0].content, IsError: true})
		if err != nil {
			t.Fatal(err)
		}
		if ceiling := (ccToolErrorBytes+len(ccTruncated))*ccMaxEscapeBytesPerRune + 64; len(served) > ceiling {
			t.Errorf("%d-byte failure: served %d bytes, derived ceiling is %d", len(body), len(served), ceiling)
		}
	}
}

// TestFailureWithNoMessageStillReportsTheError pins a judgement rather than a
// mechanism, which is why it exists: cc can write `is_error` with content that
// flattens to nothing, and this reader serves the failure anyway, because the
// alternative — dropping the result — makes a failed call read as a successful one.
// The row still says so: summarizeToolResult keys on the flag, not on the text.
//
// tether#96 shipped this at the price of a dead click, ToolCallList's `hasResult`
// having admitted `|| isError`; tether#97 narrowed that flag to non-whitespace
// content, so the price is no longer paid and nothing HERE had to change to stop
// paying it — which is the point of pinning the judgement rather than the symptom.
// See ccMessage.errorResults.
func TestFailureWithNoMessageStillReportsTheError(t *testing.T) {
	for _, content := range []any{
		[]any{},
		[]any{map[string]any{"type": "tool_reference", "tool_name": "Bash"}},
		[]any{map[string]any{"type": "image", "source": map[string]any{"data": "iVBOR"}}},
		"",
	} {
		block := map[string]any{"type": "tool_result", "tool_use_id": "t1",
			"content": content, "is_error": true}
		raw, err := json.Marshal([]any{block})
		if err != nil {
			t.Fatal(err)
		}
		got := (&ccMessage{Content: raw}).errorResults()
		if len(got) != 1 {
			t.Fatalf("content %v: got %d results, want 1 — a failure with no message is still a failure",
				content, len(got))
		}
		if got[0].content != "" {
			t.Errorf("content %v: flattened to %q, want empty", content, got[0].content)
		}
	}
}

// TestToolBlocksSurviveWhitespaceInTheJSON — the reader rejects a line without a tool
// block by looking for the literal `"tool_` before parsing it, the same trick the
// `"user"` reject uses. That is safe against an encoder that puts spaces around
// punctuation only because the needle sits INSIDE a quoted value; a needle like
// `"type":"tool_use"` would match nothing and turn every tool call invisible with no
// error anywhere. TestTitleSurvivesWhitespaceInTheJSON makes the same point for
// titles; a tool block needs its own, because it is a different needle.
func TestToolBlocksSurviveWhitespaceInTheJSON(t *testing.T) {
	spaced := `{ "type" : "assistant" , "timestamp" : "2026-08-17T03:00:01.000Z" , ` +
		`"message" : { "role" : "assistant" , "content" : [ { "type" : "tool_use" , ` +
		`"id" : "t1" , "name" : "Bash" , "input" : { "command" : "ls" } } ] } }` + "\n" +
		`{ "type" : "user" , "timestamp" : "2026-08-17T03:00:02.000Z" , "message" : ` +
		`{ "role" : "user" , "content" : [ { "type" : "tool_result" , "tool_use_id" : "t1" , ` +
		`"content" : "boom" , "is_error" : true } ] } }` + "\n"

	f := newCCFixture(t, "/w")
	f.write(t, "/w", "cc-session-0001.jsonl", spaced)

	msgs, _ := f.store().Messages("cc-session-0001")
	if len(msgs) != 1 {
		t.Fatalf("got %d messages, want 1: %+v", len(msgs), msgs)
	}
	if len(msgs[0].Tools) != 1 {
		t.Fatalf("got %d tools from whitespace-formatted JSON, want 1: %+v", len(msgs[0].Tools), msgs[0].Tools)
	}
	if msgs[0].Tools[0].Name != "Bash" {
		t.Errorf("tool name = %q, want Bash", msgs[0].Tools[0].Name)
	}
	if r := msgs[0].Tools[0].Result; r == nil || !r.IsError {
		t.Errorf("the failure did not survive whitespace: %+v", r)
	}
}

// TestMessagesCapBoundsTheElementCount — ccMessagesMax now counts messages that
// carry WORDS (see ccTrimFront), so the question "is the response still bounded"
// needs an answer that is not an argument in a comment. The bound is 2*max+1: a new
// assistant bubble only starts after a message that is not an assistant one, and a
// user message with no words is never emitted.
//
// The fixture is the adversarial shape for that bound — every user turn followed by a
// turn that says nothing but calls things, so every possible tool-only bubble exists.
func TestMessagesCapBoundsTheElementCount(t *testing.T) {
	var b strings.Builder
	for i := 0; i < ccMessagesMax*3; i++ {
		b.WriteString(ccUser(t, fmt.Sprintf("turn-%04d", i)))
		b.WriteString(ccToolUse(t, "2026-08-17T03:00:00.000Z",
			ccCall(fmt.Sprintf("t%04d", i), "Bash", map[string]any{"command": "ls"})))
	}
	f := newCCFixture(t, "/w")
	f.write(t, "/w", "cc-session-0001.jsonl", b.String())

	msgs, _ := f.store().Messages("cc-session-0001")
	words := 0
	for _, m := range msgs {
		if m.Text != "" {
			words++
		}
	}
	if words != ccMessagesMax {
		t.Errorf("%d messages carry words, want exactly the cap of %d", words, ccMessagesMax)
	}
	if len(msgs) > 2*ccMessagesMax+1 {
		t.Errorf("%d messages, bound is 2*%d+1 = %d — the cap no longer bounds the response",
			len(msgs), ccMessagesMax, 2*ccMessagesMax+1)
	}
	// And the newest turn is still the last one, which is what the tail is for.
	if want := fmt.Sprintf("turn-%04d", ccMessagesMax*3-1); msgs[len(msgs)-1].Text != want &&
		msgs[len(msgs)-2].Text != want {
		t.Errorf("the newest turn is not at the end: last two = %q / %q, want %q",
			msgs[len(msgs)-2].Text, msgs[len(msgs)-1].Text, want)
	}
}

// TestToolsDoNotShrinkTheVisibleTranscriptAtTheCap is the A/B again, run where the
// count cap BINDS — which is where review broke the first version of this change:
// with the cap counting all messages, 180 text bubbles in became 150 out, so ten real
// exchanges fell off the front in exchange for showing tool cards.
//
// Not reachable on today's store (largest transcript: 27 messages against a cap of
// 200), and that is exactly why it needs a fixture rather than a census.
func TestToolsDoNotShrinkTheVisibleTranscriptAtTheCap(t *testing.T) {
	var b strings.Builder
	// Each group: a user turn, an answer, another user turn, then a turn that says
	// nothing but calls things. Three text bubbles and one tool-only bubble per group.
	for i := 0; i < ccMessagesMax; i++ {
		b.WriteString(ccUser(t, fmt.Sprintf("ask-%04d", i)))
		b.WriteString(ccAssistantWords(t, fmt.Sprintf("answer-%04d", i), "2026-08-17T03:00:01.000Z"))
		b.WriteString(ccUser(t, fmt.Sprintf("again-%04d", i)))
		b.WriteString(ccToolUse(t, "2026-08-17T03:00:02.000Z",
			ccCall(fmt.Sprintf("t%04d", i), "Bash", map[string]any{"command": "ls"})))
	}
	body := b.String()

	f := newCCFixture(t, "/w")
	f.write(t, "/w", "cc-session-0001.jsonl", body)
	f.write(t, "/w", "cc-session-0002.jsonl", ccStripToolCalls(body))

	after, _ := f.store().Messages("cc-session-0001")
	before, _ := f.store().Messages("cc-session-0002")

	if len(before) != ccMessagesMax {
		t.Fatalf("control produced %d messages, want the cap of %d — the fixture does not reach it",
			len(before), ccMessagesMax)
	}
	var withWords []HistoryMessage
	for _, m := range after {
		if m.Text != "" {
			withWords = append(withWords, m)
		}
	}
	if len(withWords) != len(before) {
		t.Fatalf("AT THE CAP, visible history shrank: %d messages carry words, control had %d",
			len(withWords), len(before))
	}
	for i := range before {
		if withWords[i].Text != before[i].Text {
			t.Fatalf("message %d text = %q, control had %q — the trim kept a different window",
				i, withWords[i].Text, before[i].Text)
		}
	}
	served := 0
	for _, m := range after {
		served += len(m.Tools)
	}
	if served == 0 {
		t.Error("no tools served, so the comparison above is between two identical runs")
	}
}

// TestMessagesWidensPastAWallOfToolCalls — the trap this whole wi could ship.
//
// The widen retry exists because the byte window is on RAW JSONL and the tail of a
// tool-heavy transcript can hold no conversation. Its trigger used to be "the window
// produced no messages". Once tool records produce bubbles that stops being true,
// the retry stops firing, and the user gets a chat pane full of tool cards INSTEAD
// of the conversation widening would have found — a change that looks like a success
// in a screenshot while showing strictly less than before.
//
// TestMessagesWidensWhenTheTailIsAllToolPayload does NOT catch this: its wall is
// made of tool_RESULT records, which still emit nothing. The wall here is made of
// tool_USE records, which do.
func TestMessagesWidensPastAWallOfToolCalls(t *testing.T) {
	call := func(i int) string {
		return ccToolUse(t, "2026-08-17T03:00:00.000Z",
			ccCall(fmt.Sprintf("t%06d", i), "Bash", map[string]any{"command": strings.Repeat("z", 16<<10)}))
	}
	var b strings.Builder
	b.WriteString(ccUser(t, "BURIED-BEHIND-THE-CALLS"))
	b.WriteString(ccAssistantWords(t, "ANSWER-BEHIND-THE-CALLS", "2026-08-17T02:00:00.000Z"))
	for i := 0; b.Len() < ccMessagesTailBytes+(256<<10); i++ {
		b.WriteString(call(i))
	}

	f := newCCFixture(t, "/w")
	f.write(t, "/w", "cc-session-0001.jsonl", b.String())

	msgs, ok := f.store().Messages("cc-session-0001")
	if !ok {
		t.Fatal("Messages reported the transcript missing")
	}
	if !ccHasConversation(msgs) {
		t.Fatalf("the window did not widen past a wall of tool CALLS: %d messages, none with words",
			len(msgs))
	}
	var texts []string
	for _, m := range msgs {
		if m.Text != "" {
			texts = append(texts, m.Text)
		}
	}
	if len(texts) != 2 || texts[0] != "BURIED-BEHIND-THE-CALLS" || texts[1] != "ANSWER-BEHIND-THE-CALLS" {
		t.Errorf("conversation = %q, want both buried turns", texts)
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

// ---------------------------------------------------------------------------
// The shape axis (tether#95)
// ---------------------------------------------------------------------------

// TestUserRecordShapesAreClassifiedOneWayEach walks every row of ccUserShapes
// through ccClassifyUserText, one case per row, and asserts BOTH halves of the
// answer: the shape it was recognised as, and the text a human ends up reading.
//
// Two of the four shapes #95 found are KEPT, and that is the whole reason this is
// a table of expected texts rather than a list of things that vanish. A blanket
// "drop the records that look like markup" rule would satisfy a test that only
// checked the drops, and would be silently deleting 47 real user actions per 39
// transcripts: the commands the user ran with `!`, and the moments they hit
// interrupt.
//
// The two negative controls at the end are what make the drops mean anything. A
// slash command is markup AND a user turn; a pasted <div> is a user turn that
// merely looks like markup. #95's first enumeration counted 221 of the former as
// defects, and re-measuring through this function is what corrected it.
//
// The final check closes the loop the table exists for: every row of
// ccUserShapes has to appear here. Add a row for batch five without a case and
// this fails, which is the only thing standing between "the table is the one
// place" and a table with an untested row in it.
func TestUserRecordShapesAreClassifiedOneWayEach(t *testing.T) {
	cases := []struct{ name, shape, in, want string }{
		{
			name:  "task notification is dropped",
			shape: ccTaskNotificationPrefix,
			in: ccTaskNotificationPrefix + "\n<task-id>a3d</task-id>\n<status>completed</status>\n" +
				"<summary>Agent \"L1\" finished</summary>\n<result>a whole report</result>\n</task-notification>",
			want: "",
		},
		{
			name:  "command stdout is dropped",
			shape: ccCommandStdoutPrefix,
			in:    ccCommandStdoutPrefix + "\x1b[1mtotal 48\x1b[0m\ndrwxr-xr-x 3 root root</local-command-stdout>",
			want:  "",
		},
		{
			name:  "bash output is dropped",
			shape: ccBashStdoutPrefix,
			in:    ccBashStdoutPrefix + "daemonset.apps/gw restarted</bash-stdout>" + ccBashStderrPrefix + "</bash-stderr>",
			want:  "",
		},
		{
			// Registered on the mechanism rather than on an observation — 0 of the
			// 22 real records led with stderr. Asserted anyway, because "we also
			// handle the stderr-first form" is otherwise a claim with nothing
			// behind it.
			name:  "bash output leading with stderr is dropped too",
			shape: ccBashStdoutPrefix,
			in:    ccBashStderrPrefix + "no such context</bash-stderr>",
			want:  "",
		},
		{
			// KEPT. The user typed `!` and ran this.
			name:  "bash input keeps the command the user ran",
			shape: ccBashInputPrefix,
			in:    ccBashInputPrefix + " kubectl -n ieops-system rollout restart ds/gw" + ccBashInputSuffix,
			want:  "! kubectl -n ieops-system rollout restart ds/gw",
		},
		{
			// Plain text, no backticks: a user bubble renders as {m.text}, not
			// through <Markdown> (web/src/panes/chat/index.tsx).
			name:  "bash input renders without markdown",
			shape: ccBashInputPrefix,
			in:    ccBashInputPrefix + "ls -la" + ccBashInputSuffix,
			want:  "! ls -la",
		},
		{
			name:  "an empty bash input has no action to show",
			shape: ccBashInputPrefix,
			in:    ccBashInputPrefix + "   " + ccBashInputSuffix,
			want:  "",
		},
		{
			// The two KEPT shapes match the WHOLE record, so a record carrying the
			// user's own words after the markup is NOT the shape and survives
			// intact. Before this was anchored, the renderer cut at the closing tag
			// and " and then I typed this" was deleted — the same defect as the four
			// shapes this table exists for, one level down, and invisible to the
			// census because a matched shape is never reported as a candidate.
			name:  "a bash input with anything after it is not the shape and is kept whole",
			shape: ccShapeHumanText,
			in:    ccBashInputPrefix + " ls" + ccBashInputSuffix + " and then I typed this",
			want:  ccBashInputPrefix + " ls" + ccBashInputSuffix + " and then I typed this",
		},
		{
			// KEPT. 19 of the 25 sit between the answer the user cut off and what
			// they typed next, so this record is the only thing that says why that
			// answer stops mid-sentence.
			name:  "interrupt becomes an action a human reads",
			shape: ccInterruptPrefix,
			in:    "[Request interrupted by user]",
			want:  "(interrupted)",
		},
		{
			// The qualifier is carried VERBATIM — what cc means by it has not been
			// read out of cc.
			name:  "the interrupt qualifier is carried through, not interpreted",
			shape: ccInterruptPrefix,
			in:    "[Request interrupted by user for tool use]",
			want:  "(interrupted for tool use)",
		},
		{
			// Interrupts are the one shape cc writes as a text-block array, and
			// ccMessage.text joins blocks with "\n" — so this is the structurally
			// reachable way for a real message to arrive attached to a marker. It
			// must survive whole rather than becoming a bare "(interrupted)".
			name:  "an interrupt with a message after it is not the shape and is kept whole",
			shape: ccShapeHumanText,
			in:    "[Request interrupted by user]\ndo the other thing instead",
			want:  "[Request interrupted by user]\ndo the other thing instead",
		},
		{
			// NEGATIVE CONTROL. Markup, and a real user turn: this is what #95's
			// first enumeration got wrong by measuring before this function ran.
			name:  "a slash command still renders as the command",
			shape: ccCommandNameTag,
			in: "<command-message>polyforge:pf-work</command-message>\n" +
				ccCommandNameTag + "/polyforge:pf-work</command-name>\n<command-args>tether#95</command-args>",
			want: "/polyforge:pf-work tether#95",
		},
		{
			name:  "a slash command with no args renders as the bare command",
			shape: ccCommandNameTag,
			in:    ccCommandNameTag + "/clear</command-name><command-message>clear</command-message><command-args></command-args>",
			want:  "/clear",
		},
		{
			// NEGATIVE CONTROL. People paste code, and genuine human messages in
			// the census contain <svg>, <div> and <style>, so a rule about angle
			// brackets in general would eat them.
			name:  "a human pasting markup is left alone",
			shape: ccShapeHumanText,
			in:    "compare <div> and <span> for me",
			want:  "compare <div> and <span> for me",
		},
		{
			// The slash-command row is anchored for the same reason the others are.
			// Until tether#95 it matched <command-name> ANYWHERE, so this sentence
			// was replaced wholesale by "/foo" — the row held up as proof that the
			// table is not a blanket rule was itself deleting user text.
			name:  "a human quoting command markup keeps their sentence",
			shape: ccShapeHumanText,
			in:    "please add " + ccCommandNameTag + "/foo</command-name> to the docs",
			want:  "please add " + ccCommandNameTag + "/foo</command-name> to the docs",
		},
		{
			// ccIsSlashCommand requires a command to be EXTRACTABLE, not just the
			// tag to be present. Without this the row would claim a shape that
			// ccRenderCommand declines to render, and the census — which reports
			// only records matching NO row — could never surface it.
			name:  "an empty command name is not a shape at all",
			shape: ccShapeHumanText,
			in:    ccCommandNameTag + "</command-name>",
			want:  ccCommandNameTag + "</command-name>",
		},
		{
			// The promise that unmatched text comes back UNCHANGED includes its
			// whitespace: this function is not the place that reformats what a
			// human typed.
			name:  "unmatched text keeps its own whitespace",
			shape: ccShapeHumanText,
			in:    "  leading and trailing  ",
			want:  "  leading and trailing  ",
		},
		{
			// The safety margin of every drop above is that they match a PREFIX. A
			// Contains rule would delete this sentence.
			name:  "a human writing a tag name mid-sentence is left alone",
			shape: ccShapeHumanText,
			in:    "why is <task-notification> showing up as a bubble?",
			want:  "why is <task-notification> showing up as a bubble?",
		},
		{
			name:  "ordinary words are left exactly alone",
			shape: ccShapeHumanText,
			in:    "把这个查清楚",
			want:  "把这个查清楚",
		},
	}

	covered := make(map[string]bool)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			shape, got := ccClassifyUserText(tc.in)
			if shape != tc.shape {
				t.Errorf("shape = %q, want %q", shape, tc.shape)
			}
			if got != tc.want {
				t.Errorf("text = %q, want %q", got, tc.want)
			}
		})
		covered[tc.shape] = true
	}
	for _, s := range ccUserShapes {
		if !covered[s.name] {
			t.Errorf("ccUserShapes has a row for %q that no case in this table exercises. "+
				"A row without a case is a filter that can be broken silently — which is "+
				"how the four shapes of tether#95 got shipped in the first place.", s.name)
		}
	}
}

// TestMessagesDropsMachineNoiseAndKeepsRealUserActions runs all six shapes
// through the REAL chain — isUserTurn, userText, ccClassifyUserText, and the
// `text != ""` gate — in one transcript, and asserts the exact message list.
//
// End to end rather than on ccClassifyUserText alone, because #95's whole
// measurement lesson is that a claim about "what this reader produces" has to be
// taken over what it produces. The unit table above can pass while the classifier
// is wired into nothing.
func TestMessagesDropsMachineNoiseAndKeepsRealUserActions(t *testing.T) {
	body := ccUser(t, "把这个查清楚") +
		ccAssistantAt(t, "查清楚:", "2026-08-17T04:57:00.000Z") +
		ccTaskNotification(t, `Agent "L1: survey" finished`, "SECRET-AGENT-REPORT") +
		ccAssistantAt(t, "先看进度:", "2026-08-17T05:01:00.000Z") +
		ccBashInput(t, "kubectl get pods") +
		ccBashOutput(t, "SECRET-POD-LIST", "") +
		ccAssistantAt(t, "that matches", "2026-08-17T05:06:00.000Z") +
		ccInterrupt(t, "") +
		ccSlashCommandRecord(t, "/polyforge:pf-work", "tether#95") +
		ccUser(t, "and compare <div> with <span>")

	f := newCCFixture(t, "/w")
	f.write(t, "/w", "cc-session-0001.jsonl", body)

	msgs, ok := f.store().Messages("cc-session-0001")
	if !ok {
		t.Fatal("Messages reported the transcript missing")
	}
	want := []struct{ role, text string }{
		{"user", "把这个查清楚"},
		// The two fragments joined: the task notification between them is gone,
		// so nothing closes the run. That is the tether#94 seam.
		{"assistant", "查清楚:" + ccTurnJoin + "先看进度:"},
		// KEPT, and it closes the run — a real user action ends the turn before it.
		{"user", "! kubectl get pods"},
		{"assistant", "that matches"},
		{"user", "(interrupted)"},
		{"user", "/polyforge:pf-work tether#95"},
		{"user", "and compare <div> with <span>"},
	}
	if len(msgs) != len(want) {
		t.Fatalf("got %d messages, want %d:\n%+v", len(msgs), len(want), msgs)
	}
	for i, w := range want {
		if msgs[i].Role != w.role || msgs[i].Text != w.text {
			t.Errorf("msgs[%d] = %s/%q, want %s/%q", i, msgs[i].Role, msgs[i].Text, w.role, w.text)
		}
	}
	// Nothing that was dropped may reach the pane in any form — not the markup,
	// not the payload it wrapped. Asserted separately from the list above because
	// an assertion on the whole list can be satisfied by a renderer that leaks
	// into a message this test does not name.
	for _, m := range msgs {
		for _, forbidden := range []string{
			"task-notification", "SECRET-AGENT-REPORT", "toolu_", "output-file",
			"bash-stdout", "bash-stderr", "SECRET-POD-LIST",
			"<bash-input>", "Request interrupted by user",
			"command-name", "command-args", "command-message",
		} {
			if strings.Contains(m.Text, forbidden) {
				t.Errorf("%q reached the pane in %s/%q", forbidden, m.Role, m.Text)
			}
		}
	}
}

// TestMessagesMergesAcrossDroppedNoise pins the HOP between tether#95 and #94,
// which is the easiest thing in either wi to leave untested: the fix lands in
// userText, and the behaviour the reporter actually sees comes out of
// ccMessagesFrom.
//
// 355 of the 364 task notifications in the census sat between two assistant
// fragments. Each one was doing double damage — showing up as a bubble of raw
// markup, AND closing the assistant run, so the reporter got the row of
// colon-terminated bubbles that #94 was supposed to have removed. Dropping the
// record is only half a fix if the two fragments do not then join.
//
// On the code before #95 the first two subtests report 4 messages instead of 2:
// the noise arrived as a user bubble in the middle. That is the A/B, and it is why
// the assertion is on the merged TEXT rather than on the count alone — a count of
// 2 would also be produced by a merge that lost a fragment.
//
// The third subtest is NOT an A/B: main already dropped <local-command-stdout>, so
// it passes there too and overlaps TestMessagesMergesATurnSplitByToolCalls. It is
// here anyway, because what this test asserts is a property of the mechanism —
// anything userText drops stops closing a run — and a case that already held is
// evidence the mechanism is what changed rather than the four shapes being
// special-cased somewhere.
func TestMessagesMergesAcrossDroppedNoise(t *testing.T) {
	for _, tc := range []struct{ name, noise string }{
		{"task notification", ccTaskNotification(t, "sub-agent finished", "REPORT-BODY")},
		{"bash output", ccBashOutput(t, "restarted", "")},
		{"command stdout", ccCommandStdout(t, "\x1b[1mtotal 48\x1b[0m")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newCCFixture(t, "/w")
			f.write(t, "/w", "cc-session-0001.jsonl",
				ccUser(t, "go")+
					ccAssistantAt(t, "第一段:", "2026-08-17T04:57:00.000Z")+
					tc.noise+
					ccAssistantAt(t, "第二段:", "2026-08-17T05:01:00.000Z"))

			msgs, ok := f.store().Messages("cc-session-0001")
			if !ok {
				t.Fatal("Messages reported the transcript missing")
			}
			if len(msgs) != 2 {
				t.Fatalf("got %d messages, want 2 — the noise record still closed the "+
					"assistant run, so the reporter still sees two bubbles:\n%+v", len(msgs), msgs)
			}
			want := "第一段:" + ccTurnJoin + "第二段:"
			if msgs[1].Role != "assistant" || msgs[1].Text != want {
				t.Fatalf("msgs[1] = %s/%q, want assistant/%q", msgs[1].Role, msgs[1].Text, want)
			}
			// The separator asserted literally as well as through the constant: a
			// lone "\n" is a CommonMark soft break and would render the two
			// fragments on one line. See ccTurnJoin.
			if !strings.Contains(msgs[1].Text, "第一段:\n\n第二段:") {
				t.Errorf("fragments are not separated by a BLANK line: %q", msgs[1].Text)
			}
			// The stamp is the turn's START, so a merge unlocked by #95 labels the
			// bubble the same way one unlocked by #94 does.
			if wantTs := time.Date(2026, 8, 17, 4, 57, 0, 0, time.UTC).UnixMilli(); msgs[1].Ts != wantTs {
				t.Errorf("Ts = %d, want %d (the FIRST fragment's stamp)", msgs[1].Ts, wantTs)
			}
		})
	}
}

// TestTitleUsesTheSameClassifierAsTheTranscript — the row label and the
// transcript have to agree about what the user said. They consume one definition
// (userText) precisely so they cannot disagree, but they reach it through
// DIFFERENT scans: a title comes from ccFirstUserText, one bounded pass that
// stops at the first hit, and the transcript from ccMessagesFrom. So "they share
// a definition" is a claim worth a fixture rather than an assumption, and
// tether#95 gave that definition four new answers.
//
// Two halves, and the second is the one that would be missed. A dropped shape
// must not become the title — otherwise the list goes back to showing markup,
// which is what tether#92 fixed. And a KEPT shape must be ALLOWED to become the
// title: a session whose first act is `! kubectl …` really did start there, and
// labelling that row with a bare sid is the defect tether#91 existed to remove.
func TestTitleUsesTheSameClassifierAsTheTranscript(t *testing.T) {
	for _, tc := range []struct{ name, body, want string }{
		{
			name: "noise ahead of the first real turn does not become the title",
			body: ccPreamble(t, 512) +
				ccTaskNotification(t, "sub-agent finished", "REPORT-BODY") +
				ccBashOutput(t, "NOISE-OUTPUT", "") +
				ccCommandStdout(t, "total 48") +
				ccUser(t, "what the human actually asked"),
			want: "what the human actually asked",
		},
		{
			name: "a command the user ran is a real first turn",
			body: ccPreamble(t, 512) + ccBashInput(t, "kubectl get pods"),
			want: "! kubectl get pods",
		},
		{
			name: "so is an interrupt, rendered the same way as in the transcript",
			body: ccPreamble(t, 512) + ccInterrupt(t, ""),
			want: "(interrupted)",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newCCFixture(t, "/w")
			f.write(t, "/w", "cc-session-0001.jsonl", tc.body)
			rows := f.store().List()
			if len(rows) != 1 {
				t.Fatalf("List() = %d rows, want 1: %+v", len(rows), rows)
			}
			if rows[0].Title != tc.want {
				t.Errorf("Title = %q, want %q", rows[0].Title, tc.want)
			}
		})
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
// JSONL, and cc's bytes are mostly tool payload, of which this reader serves only a
// bounded summary. So the last 1 MiB of a tool-heavy transcript can hold no
// conversation at all, and the naive answer is an empty chat for a row that has a
// title and is listed — the original bug, one layer down. Found by review, not by the
// first test pass.
//
// (This sentence said "tool payloads this reader then drops" until tether#96, which is
// verbatim the claim that change had to falsify elsewhere. A wall of tool RESULTS
// still emits nothing, which is why this test kept passing and why
// TestMessagesWidensPastAWallOfToolCalls had to be written: a wall of tool CALLS does
// emit, and that is the case that breaks the old trigger.)
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
// The census — the enumeration method itself (tether#95)
// ---------------------------------------------------------------------------

// ccCensusEnv points the census at a real cc store. Absent, it skips.
const ccCensusEnv = "TETHER_CC_CENSUS_DIR"

// ccCompleteTagRe matches text that OPENS with a complete tag — the signal that
// found batch four. Machine-injected records always lead with their tag; a human
// pasting markup usually leads with a sentence.
//
// Deliberately wider than the four shapes cc writes today. The first version was
// `^<[a-zA-Z][-a-zA-Z0-9]*>`, which matches every current shape and therefore
// looked sufficient — but a census is only worth running if it catches forms
// nobody has seen, and review demonstrated three plausible ones sailing past it:
// `<hook-result status="ok">` (an attribute), `<task_notification_v2>` (an
// underscore) and `<x:thing>` (a namespace). Tuning a detector to the examples
// that prompted it is how it ends up detecting only them.
//
// Still blind to a shape that is PROSE rather than markup, which is what cc's
// interrupt marker is — see ccShortBracketRe for the weaker signal that covers
// the bracketed family, and accept that a batch five in plain English would be
// caught by neither. Stated because the alternative is a reader assuming
// otherwise.
var ccCompleteTagRe = regexp.MustCompile(`^<[a-zA-Z][-_.:a-zA-Z0-9]*(\s[^>\n]*)?>`)

// ccShortBracketRe matches text that opens with a short bracketed marker — the
// family cc's interrupt marker belongs to. Weaker than the tag signal and it has
// known false positives (a user pasting `[wt] recv message` from a browser
// console, 3 records in the first census), so it reports rather than fails.
var ccShortBracketRe = regexp.MustCompile(`^\[[^\]\n]{1,60}\]`)

// TestEnumerateUserRecordShapes is the CENSUS: a re-runnable count of what a real
// cc store actually contains, run through the real classifier, reporting anything
// this reader does not recognise.
//
// # Why this ships instead of a sentence claiming the list is complete
//
// Four batches of records that merely wear type:"user" have been found so far, and
// every one of them was found the same way: a user saw markup in a bubble and
// said so. The list in ccUserShapes was complete when it was written, and so was
// each of the three before it. So the deliverable of tether#95 is not only the
// four rows, it is this: batch five should be found by RUNNING something.
//
//	export GOWORK=off
//	go test ./internal/session/ -run TestEnumerateUserRecordShapes -v \
//	  -timeout 20m -count=1 \
//	  TETHER_CC_CENSUS_DIR=$HOME/<the coding agent's data dir>/projects
//
// (as an env assignment before `go test`, or exported — either way). Point it at
// the store's `projects` directory to census everything, or at one project
// directory inside it. Skipped when unset, because CI has no real store and the
// owner's transcripts must never be copied into this repo — see the file header.
//
// # It is READ ONLY
//
// os.Open and nothing else, on a directory that is the user's actual work and a
// host mount. TestCCStoreNeverWrites guards the production reader the same way;
// this test walks the store directly, so it says so here.
//
// # The measurement rule this encodes, which is the part that is easy to get wrong
//
// Every count here is taken AFTER ccClassifyUserText, over what the reader
// EMITS. #95's first enumeration regex-matched the raw record text instead and
// reported 6 shapes, 654 records and 19.5% — of which 221 were slash commands
// that ccRenderCommand had already turned into `/pf-work tether#95`, i.e. real
// user turns being scored as defects. Measured through the chain, the same store
// gave 4 shapes, 433 records, 12.9%. A statistic about what a pipeline produces
// has to be taken at the end of the pipeline.
//
// # What it fails on
//
// An unrecognised COMPLETE TAG at the start of an emitted message. That is the
// signal that found batch four, and the fix for it is one row in ccUserShapes.
// The weaker bracket signal reports without failing, because it has known false
// positives; read the report, then judge.
func TestEnumerateUserRecordShapes(t *testing.T) {
	root := os.Getenv(ccCensusEnv)
	if root == "" {
		t.Skipf("set %s to a cc projects directory (or one project directory in it) to take a census", ccCensusEnv)
	}
	files, err := ccCensusFiles(root)
	if err != nil {
		t.Fatalf("walking %s: %v", root, err)
	}
	if len(files) == 0 {
		t.Fatalf("no top-level .jsonl transcripts under %s", root)
	}

	var (
		lines, userRecords, structuralDrops, emitted int
		byShape                                      = map[string]int{}
		droppedByShape                               = map[string]int{}
		tagCandidates                                = map[string]int{}
		bracketCandidates                            = map[string]int{}
		sample                                       = map[string]string{}
	)
	for _, path := range files {
		f, err := os.Open(path) // READ ONLY. See the doc above.
		if err != nil {
			t.Fatalf("opening %s: %v", path, err)
		}
		br := bufio.NewReader(f)
		for {
			line, rerr := br.ReadBytes('\n')
			if bytes.HasSuffix(line, []byte("\n")) {
				lines++
				// The same cheap reject the production readers use, so the census
				// walks the same records they do.
				if bytes.Contains(line, []byte(`"user"`)) {
					var e ccEntry
					if json.Unmarshal(line, &e) == nil && e.Type == "user" {
						userRecords++
						if !e.isUserTurn() {
							structuralDrops++
						} else {
							shape, text := ccClassifyUserText(e.Message.text())
							byShape[shape]++
							if text == "" {
								droppedByShape[shape]++
								continue
							}
							emitted++
							if shape != ccShapeHumanText {
								continue
							}
							// Emitted as a human's own words. Does it still look
							// like something a machine wrote?
							trimmed := strings.TrimSpace(text)
							if tag := ccCompleteTagRe.FindString(trimmed); tag != "" {
								tagCandidates[tag]++
								ccCensusSample(sample, tag, path, trimmed)
							} else if mk := ccShortBracketRe.FindString(trimmed); mk != "" {
								bracketCandidates[mk]++
								ccCensusSample(sample, mk, path, trimmed)
							}
						}
					}
				}
			}
			if rerr != nil {
				break
			}
		}
		f.Close()
	}

	t.Logf("census of %d transcripts under %s", len(files), root)
	t.Logf("  %d lines, %d type:\"user\" records", lines, userRecords)
	t.Logf("  %d dropped structurally (isMeta / isSidechain)", structuralDrops)
	// The population every percentage below is taken over: records that got past
	// the structural filter AND had text to classify. Named and computed once,
	// because "% of the user messages" is the exact phrase that made #95's first
	// enumeration wrong — a percentage of "what the reader emits" is relative to
	// whichever shapes were already being dropped when it was taken, so it cannot
	// be compared across versions. This denominator can.
	noise := 0
	for _, s := range ccUserShapes {
		noise += droppedByShape[s.name]
	}
	classified := emitted + noise
	t.Logf("  %d EMITTED user messages; %d dropped by shape; %d classified with text (the denominator below)",
		emitted, noise, classified)
	t.Logf("  %d had no text at all — tool results, images, thinking (ccMessage.text returned \"\")",
		droppedByShape[ccShapeHumanText])
	t.Logf("  recognised shapes, in ccUserShapes order:")
	for _, s := range ccUserShapes {
		pct := 0.0
		if classified > 0 {
			pct = 100 * float64(byShape[s.name]) / float64(classified)
		}
		t.Logf("    %-28s seen %5d (%5.2f%%), dropped %5d, rendered %5d",
			s.name, byShape[s.name], pct, droppedByShape[s.name], byShape[s.name]-droppedByShape[s.name])
		if byShape[s.name] == 0 {
			t.Logf("      ^ this row matched NOTHING. Either this store has none, or cc changed "+
				"the literal and the row is now dead. Check %q against the store by hand.", s.name)
		}
	}
	t.Logf("    %-28s      %5d (the user's own words, emitted unchanged)",
		"(no shape)", byShape[ccShapeHumanText]-droppedByShape[ccShapeHumanText])
	if classified > 0 {
		t.Logf("  noise dropped by shape: %d of %d classified, %.2f%%", noise, classified,
			100*float64(noise)/float64(classified))
	}

	if len(bracketCandidates) > 0 {
		t.Logf("  UNRECOGNISED bracketed markers — judge these by hand, some are real pastes:")
		for _, k := range ccCensusSorted(bracketCandidates) {
			t.Logf("    %5d  %s\n           e.g. %s", bracketCandidates[k], k, sample[k])
		}
	}
	for _, k := range ccCensusSorted(tagCandidates) {
		t.Errorf("%d emitted user messages OPEN with %s, which ccUserShapes does not know about.\n"+
			"  This is what batch five looks like. THREE possible verdicts, and the third is a real\n"+
			"  one that needs no code:\n"+
			"    - the harness wrote it   -> add a row that drops it\n"+
			"    - the user did it        -> add a row that renders it into something a human reads\n"+
			"    - a human PASTED markup  -> nothing to do; angle brackets are not by themselves a\n"+
			"                                defect, and this detector is deliberately wide enough to\n"+
			"                                catch pastes (see ccCompleteTagRe)\n"+
			"  For the first two, that is ONE row in ccUserShapes plus one case in\n"+
			"  TestUserRecordShapesAreClassifiedOneWayEach, which will not let you add the row alone.\n"+
			"  e.g. %s", tagCandidates[k], k, sample[k])
	}

	ccCensusCapReport(t, files)
}

// ccCensusFiles lists the transcripts a census should read, using List's rule:
// only files directly inside a project directory, never one level deeper.
//
// That exclusion is not cosmetic — 888 of 926 .jsonl files under one real project
// directory were sub-agent transcripts, so a naive walk censuses mostly sub-agents
// and reports percentages for a population the reader never serves.
//
// root may be the `projects` directory or a single project directory inside it,
// because both are things someone taking a census reaches for. Which one it is is
// inferred from whether root holds transcripts directly — and an ambiguous root,
// holding both, is an ERROR rather than a guess: one stray .jsonl beside the
// project directories would otherwise silently census that one file and report a
// clean bill of health for a population of one. A census that quietly measures the
// wrong population is the failure this file documents twice already.
func ccCensusFiles(root string) ([]string, error) {
	direct, err := ccCensusJSONL(root)
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, err
	}
	var out []string
	subdirs := 0
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		sub, err := ccCensusJSONL(filepath.Join(root, e.Name()))
		if err != nil {
			return nil, err
		}
		if len(sub) > 0 {
			subdirs++
		}
		out = append(out, sub...)
	}
	switch {
	case len(direct) > 0 && subdirs > 0:
		return nil, fmt.Errorf("ambiguous census root %s: it holds %d transcript(s) directly AND "+
			"%d subdirectories that hold transcripts, so 'is this one project or all of them' has "+
			"no answer. Point at a single project directory, or at a projects directory with no "+
			"stray .jsonl in it", root, len(direct), subdirs)
	case len(direct) > 0:
		return direct, nil
	default:
		return out, nil
	}
}

func ccCensusJSONL(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		out = append(out, filepath.Join(dir, e.Name()))
	}
	return out, nil
}

func ccCensusSample(into map[string]string, key, path, text string) {
	if _, ok := into[key]; ok {
		return
	}
	if len(text) > 160 {
		text = text[:160] + "…"
	}
	into[key] = fmt.Sprintf("%s: %q", filepath.Base(path), text)
}

// ccCensusSorted orders a candidate report by count, descending, breaking ties on
// the key so two runs over the same store produce the same report — a diffable
// report is the point of taking a census twice.
func ccCensusSorted(m map[string]int) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Slice(out, func(i, j int) bool {
		if m[out[i]] != m[out[j]] {
			return m[out[i]] > m[out[j]]
		}
		return out[i] < out[j]
	})
	return out
}

// ccCensusCapReport re-checks ccMessagesMax against the store, because dropping
// records changes how many messages a transcript produces and therefore what that
// cap MEANS.
//
// It exists because the same constant's doc once claimed a unit the code two lines
// away did not implement (see ccMessagesMax), and #95 changes the emitted count
// again. A number in a comment cannot notice that; this can.
//
// It reads each transcript through the real ccMessagesTailBytes window — the same
// window Messages serves from — but not the widen retry, so a transcript whose
// tail is all tool payload shows up here as 0 rather than as what the user sees.
func ccCensusCapReport(t *testing.T, files []string) {
	t.Helper()
	var total, maxMsgs, atCap, empty, bytesServed int
	// tether#96. withWords is the population the reader served BEFORE tool activity
	// was carried, and that identity is not a guess: the old reader emitted a
	// message only when it had text, and a tool-only message has role "assistant",
	// so it never breaks the assistant-to-assistant adjacency the merge runs on.
	// Two numbers to compare against a run of the old build, plus the tool payload
	// that is the price of the change.
	var withWords, toolCalls, maxToolsInATurn, withFailure, emptyFailure, toolBytes int
	var noConversation int
	// The WIRE size, both arms, in one run: what the route writes now, and what it
	// wrote before tether#96. The "before" arm is the same messages with the tool
	// activity removed and the wordless ones dropped, which is exactly the population
	// the old reader emitted — so this needs no second build, and unlike a sum of
	// field lengths it is the bytes the client receives, escaping included.
	var wireBefore, wireAfter int
	// Characters versus bytes, so the encoder's `<`/`>`/`&` expansion is a reported
	// number rather than a footnote. The bound this change relies on is on
	// CHARACTERS; see ccToolInputValueRunes.
	var toolChars int
	// The worst SINGLE transcript, because that is what one page load pays. A
	// store-wide total answers "how much does the store cost", which nobody waits
	// for.
	var worstBefore, worstAfter int
	var worstName string
	for _, path := range files {
		f, err := os.Open(path)
		if err != nil {
			t.Fatalf("opening %s: %v", path, err)
		}
		fi, err := f.Stat()
		if err != nil {
			t.Fatalf("stat %s: %v", path, err)
		}
		// tether#107 renamed ccReadTail to ccReadWindow: the tail is the window that
		// ends at the file size, so the census asks for exactly what it asked for
		// before and the figures below stay comparable to the ones in ccMessagesMax's
		// doc comment.
		page, err := ccReadWindow(f, fi.Size(), ccMessagesTailBytes)
		f.Close()
		if err != nil {
			t.Fatalf("reading %s: %v", path, err)
		}
		msgs := page.Messages
		total += len(msgs)
		if len(msgs) > maxMsgs {
			maxMsgs = len(msgs)
		}
		if len(msgs) == ccMessagesMax {
			atCap++
		}
		if len(msgs) == 0 {
			empty++
		}
		if !ccHasConversation(msgs) {
			noConversation++
		}
		var stripped []HistoryMessage
		for _, m := range msgs {
			bytesServed += len(m.Text)
			if m.Text != "" {
				withWords++
				stripped = append(stripped, HistoryMessage{Role: m.Role, Text: m.Text, Ts: m.Ts})
			}
			toolCalls += len(m.Tools)
			if len(m.Tools) > maxToolsInATurn {
				maxToolsInATurn = len(m.Tools)
			}
			for _, tc := range m.Tools {
				// What the response actually carries for this call, measured by
				// encoding it — the same escaping encoder the HTTP handler uses,
				// rather than a sum of field lengths that forgets both the JSON
				// around them and what the encoder does to `<`, `>` and `&`.
				enc, err := json.Marshal(tc)
				if err != nil {
					t.Fatalf("re-encoding a served call from %s: %v", path, err)
				}
				toolBytes += len(enc)
				toolChars += len(tc.ID) + len(tc.Name) + len(tc.Input)
				if tc.Result != nil {
					withFailure++
					toolChars += len(tc.Result.Content)
					if tc.Result.Content == "" {
						emptyFailure++
					}
				}
			}
		}
		after, err := json.Marshal(msgs)
		if err != nil {
			t.Fatalf("re-encoding the response for %s: %v", path, err)
		}
		before, err := json.Marshal(stripped)
		if err != nil {
			t.Fatalf("re-encoding the control response for %s: %v", path, err)
		}
		wireAfter += len(after)
		wireBefore += len(before)
		if len(after) > worstAfter {
			worstAfter, worstBefore, worstName = len(after), len(before), filepath.Base(path)
		}
	}
	t.Logf("ccMessagesMax = %d, re-checked against the same store:", ccMessagesMax)
	t.Logf("  %d messages across %d transcripts through the real %d-byte window, %.2f MB of text",
		total, len(files), ccMessagesTailBytes, float64(bytesServed)/(1<<20))
	// "reached the cap" rather than "was trimmed": a transcript that produces
	// exactly ccMessagesMax messages is indistinguishable from one that produced
	// more and lost the front, and claiming to tell them apart would be a
	// measurement this cannot make.
	t.Logf("  largest transcript: %d messages; %d of %d reached the cap; %d serve nothing "+
		"from the primary window (Messages widens for those)", maxMsgs, atCap, len(files), empty)
	if atCap == 0 {
		t.Logf("  nothing reached the cap, so the BYTE window is what bounds the response — "+
			"%d is still only a bound on a transcript of very short lines, which is what it is for", ccMessagesMax)
	}

	// tether#96 — the A/B, and the price.
	t.Logf("tool activity (tether#96), same store, same window:")
	t.Logf("  %d messages carry words; %d carry only tool calls", withWords, total-withWords)
	t.Logf("  ^ the first number and the %.2f MB above are what a run of the PRE-#96 build reports as"+
		" its message count and its text — compare them, and if either has fallen, visible history shrank",
		float64(bytesServed)/(1<<20))
	t.Logf("  RESPONSE bytes as the route writes them: %.2f MB before, %.2f MB after, %.2fx",
		float64(wireBefore)/(1<<20), float64(wireAfter)/(1<<20),
		float64(wireAfter)/float64(max(wireBefore, 1)))
	t.Logf("  worst SINGLE transcript: %.1f KB before -> %.1f KB after (%.2fx) [%s] — this is what one"+
		" page load pays, and the store-wide totals are not",
		float64(worstBefore)/1024, float64(worstAfter)/1024,
		float64(worstAfter)/float64(max(worstBefore, 1)), worstName)
	t.Logf("  %d tool calls served: %.2f MB of tool JSON over %.2f MB of characters (%.2fx — the"+
		" encoder's <>& escaping, which applies to the text too and is not new), %d carry a failure, %d of"+
		" those with no message in it",
		toolCalls, float64(toolBytes)/(1<<20), float64(toolChars)/(1<<20),
		float64(toolBytes)/float64(max(toolChars, 1)), withFailure, emptyFailure)
	t.Logf("  largest tool count in ONE merged turn: %d (the renderer folds above 5, so this is one"+
		" DOM node until clicked; MaxToolsPerTurn = %d is the LIVE path's cap and does not apply here)",
		maxToolsInATurn, MaxToolsPerTurn)
	t.Logf("  %d of %d transcripts serve no conversation from the primary window, so Messages widens"+
		" for those — this is the number the widen trigger keys on, NOT the %d empty ones above",
		noConversation, len(files), empty)
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

// ---------------------------------------------------------------------------
// Paging BACKWARDS through a bounded transcript (tether#107)
// ---------------------------------------------------------------------------

// ccBigTranscript builds a synthetic transcript comfortably past `atLeast` bytes as
// an alternating user/assistant conversation, and returns it plus every text it
// wrote in order.
//
// Every turn is UNIQUELY numbered, which is what lets the walk test below detect a
// gap and a duplicate as different failures. A fixture of identical turns would make
// a lost record and a repeated one look the same (the count would be right either
// way once, and wrong in the same direction otherwise) — and losing exactly one
// record per page seam is the specific defect the cursor design is built against.
//
// # The padding is sized so the fixture stays UNDER ccMessagesMax
//
// The walk test's reference is a whole-file conversion, and ccMessagesFrom applies
// the count cap to that too. A fixture of many small turns therefore produces a
// reference that is itself trimmed to 200, which the walk then "disagrees" with for a
// reason that has nothing to do with cursors — measured, the first version of this
// fixture walked 744 texts against a 200-text reference. 48 KiB per record puts a
// multi-megabyte fixture at a few dozen turns, so the BYTE window is the only cap in
// play and the reference means what the test needs it to mean.
func ccBigTranscript(t *testing.T, atLeast int) (string, []string) {
	t.Helper()
	var b strings.Builder
	var texts []string
	pad := strings.Repeat("x", 48<<10)
	for i := 0; b.Len() < atLeast; i++ {
		u := fmt.Sprintf("USER-%05d %s", i, pad)
		a := fmt.Sprintf("ASSISTANT-%05d %s", i, pad)
		b.WriteString(ccUser(t, u))
		b.WriteString(ccAssistantWords(t, a, "2026-08-17T03:00:01.000Z"))
		texts = append(texts, u, a)
	}
	return b.String(), texts
}

func ccTexts(msgs []HistoryMessage) []string {
	var out []string
	for _, m := range msgs {
		if m.Text != "" {
			out = append(out, m.Text)
		}
	}
	return out
}

// TestMessagePageWalksBackWithoutGapOrDuplicate is THE property this cursor exists
// to have, and the only one that can catch the off-by-one-record seam.
//
// It walks a transcript larger than the byte window page by page, concatenates the
// pages oldest-first, and requires the result to equal a whole-file conversion. Both
// halves of the equality matter:
//
//   - a GAP is what reporting the WINDOW START as the cursor produces — the leading
//     fragment this page dropped is the same record the next page would truncate.
//   - a DUPLICATE is what reporting the window start PLUS the fragment of the
//     *previous* read would produce, or any cursor that lands mid-record.
//
// It also pins that the walk TERMINATES: an unbounded loop here would hang CI rather
// than fail it, so the page budget is explicit and the assertion is that the walk
// finished well inside it.
func TestMessagePageWalksBackWithoutGapOrDuplicate(t *testing.T) {
	body, _ := ccBigTranscript(t, 3*ccMessagesTailBytes)
	f := newCCFixture(t, "/w")
	f.write(t, "/w", "cc-session-0001.jsonl", body)
	store := f.store()

	whole := ccTexts(ccMessagesFrom(strings.NewReader(body)))
	if len(whole) < 40 {
		t.Fatalf("the fixture is too small to be paging anything: %d texts", len(whole))
	}
	// The reference must not itself be trimmed, or this compares a paged walk against
	// a capped baseline and fails for a reason that is not about cursors. See
	// ccBigTranscript.
	if len(whole) >= ccMessagesMax {
		t.Fatalf("the whole-file reference has %d texts and the cap is %d — the reference is capped and this test would be vacuous",
			len(whole), ccMessagesMax)
	}

	var walked []string
	before := TranscriptTail
	pages := 0
	const maxPages = 64
	for {
		page, ok := store.MessagePage("cc-session-0001", before)
		if !ok {
			t.Fatal("MessagePage reported the transcript missing")
		}
		pages++
		if pages > maxPages {
			t.Fatalf("the walk did not terminate in %d pages; last cursor %d", maxPages, page.Earlier)
		}
		walked = append(ccTexts(page.Messages), walked...)
		if !page.HasEarlier {
			break
		}
		if before != TranscriptTail && page.Earlier >= before {
			t.Fatalf("cursor did not move back: %d then %d", before, page.Earlier)
		}
		before = page.Earlier
	}

	if pages < 3 {
		t.Fatalf("the fixture spans %d bytes but paged in %d page(s) — the window is not binding, so this test is vacuous",
			len(body), pages)
	}
	if len(walked) != len(whole) {
		t.Fatalf("walked %d texts over %d pages, whole-file conversion has %d — a gap or a duplicate at a seam",
			len(walked), pages, len(whole))
	}
	for i := range whole {
		if walked[i] != whole[i] {
			t.Fatalf("page walk diverges from the whole file at index %d:\n walked %.40q\n  whole %.40q",
				i, walked[i], whole[i])
		}
	}
}

// TestMessagePageNewestPageIsUnchanged — the no-cursor request must be what it was
// before tether#107, because ~74 of the 93 top-level transcripts on the reference
// machine never reach the ceiling and must not pay for a feature they never use.
func TestMessagePageNewestPageIsUnchanged(t *testing.T) {
	body, _ := ccBigTranscript(t, 2*ccMessagesTailBytes)
	f := newCCFixture(t, "/w")
	f.write(t, "/w", "cc-session-0001.jsonl", body)
	store := f.store()

	page, ok := store.MessagePage("cc-session-0001", TranscriptTail)
	if !ok {
		t.Fatal("MessagePage reported the transcript missing")
	}
	msgs, ok := store.Messages("cc-session-0001")
	if !ok {
		t.Fatal("Messages reported the transcript missing")
	}
	if len(msgs) != len(page.Messages) {
		t.Fatalf("Messages served %d, the newest page served %d — they are meant to be one implementation",
			len(msgs), len(page.Messages))
	}
	if !page.HasEarlier {
		t.Fatal("a transcript twice the window reports nothing earlier")
	}
	if page.Earlier <= 0 || page.Earlier >= int64(len(body)) {
		t.Errorf("Earlier = %d, want inside (0, %d)", page.Earlier, len(body))
	}
}

// TestMessagePageOnAWholeSmallTranscriptOffersNothingEarlier — the other half of the
// signal, and the one a false positive would ruin: a transcript that fits reports NO
// cursor, so the pane can say "the beginning of this conversation" and mean it.
//
// The fixture deliberately OPENS on records that produce no message — a tool result
// is a type:"user" record with no text — because that is the case where returning
// "the first message's offset" instead of zero would report a cursor above zero on a
// complete file, offer a "load earlier" button, and answer the click with nothing.
func TestMessagePageOnAWholeSmallTranscriptOffersNothingEarlier(t *testing.T) {
	body := ccToolResult(t, "orphan output") +
		ccUser(t, "the very first thing said") +
		ccAssistantWords(t, "the answer", "2026-08-17T03:00:01.000Z")

	f := newCCFixture(t, "/w")
	f.write(t, "/w", "cc-session-0001.jsonl", body)

	page, ok := f.store().MessagePage("cc-session-0001", TranscriptTail)
	if !ok {
		t.Fatal("MessagePage reported the transcript missing")
	}
	if page.HasEarlier {
		t.Errorf("a whole transcript reports Earlier = %d; the pane would offer a page that does not exist",
			page.Earlier)
	}
	if got := ccTexts(page.Messages); len(got) != 2 || got[0] != "the very first thing said" {
		t.Errorf("texts = %q", got)
	}
}

// TestMessagePageCursorSurvivesTheCOUNTCap — the second regime, which the byte
// window hides.
//
// A transcript of very short lines fits inside ccMessagesTailBytes and is still
// trimmed, by ccTrimFront. So the window reaches byte 0 while the PAGE does not, and
// a cursor computed from the window start would be 0 — reporting "the beginning of
// the conversation" while 200 turns of it are unreachable. Nothing in the byte-window
// tests can see this: on the reference machine the count cap binds on 0 of 93
// transcripts, exactly as ccMessagesMax's doc says.
func TestMessagePageCursorSurvivesTheCOUNTCap(t *testing.T) {
	var b strings.Builder
	var want []string
	total := ccMessagesMax + 40
	for i := 0; i < total; i++ {
		u := fmt.Sprintf("SHORT-%04d", i)
		b.WriteString(ccUser(t, u))
		want = append(want, u)
	}
	body := b.String()
	if len(body) >= ccMessagesTailBytes {
		t.Fatalf("the fixture is %d bytes — the BYTE window would bind and this test would be about the wrong cap", len(body))
	}

	f := newCCFixture(t, "/w")
	f.write(t, "/w", "cc-session-0001.jsonl", body)
	store := f.store()

	page, ok := store.MessagePage("cc-session-0001", TranscriptTail)
	if !ok {
		t.Fatal("MessagePage reported the transcript missing")
	}
	if got := len(ccTexts(page.Messages)); got != ccMessagesMax {
		t.Fatalf("newest page served %d texts, want the cap %d", got, ccMessagesMax)
	}
	if !page.HasEarlier {
		t.Fatal("the count cap trimmed 40 turns and the page reports nothing earlier")
	}

	earlier, ok := store.MessagePage("cc-session-0001", page.Earlier)
	if !ok {
		t.Fatal("MessagePage reported the transcript missing on the earlier page")
	}
	got := ccTexts(earlier.Messages)
	if len(got) != 40 {
		t.Fatalf("the earlier page served %d texts, want the 40 the cap trimmed", len(got))
	}
	if got[0] != want[0] || got[39] != want[39] {
		t.Errorf("earlier page = [%q … %q], want [%q … %q]", got[0], got[len(got)-1], want[0], want[39])
	}
	if earlier.HasEarlier {
		t.Errorf("the earlier page reaches turn 0 and still reports Earlier = %d", earlier.Earlier)
	}
}

// TestMessagePageClampsACursorPastTheEnd — a client holding an offset for a file
// that has since been truncated and rewritten. The tail is the honest answer;
// refusing would leave that reader with no transcript at all.
func TestMessagePageClampsACursorPastTheEnd(t *testing.T) {
	body := ccUser(t, "only turn")
	f := newCCFixture(t, "/w")
	f.write(t, "/w", "cc-session-0001.jsonl", body)

	page, ok := f.store().MessagePage("cc-session-0001", int64(len(body))*100)
	if !ok {
		t.Fatal("MessagePage reported the transcript missing")
	}
	if got := ccTexts(page.Messages); len(got) != 1 || got[0] != "only turn" {
		t.Errorf("texts = %q, want the whole (tiny) transcript", got)
	}
	if page.HasEarlier {
		t.Errorf("clamped to the whole file and still reports Earlier = %d", page.Earlier)
	}
}

// TestMessagePageAtZeroIsEmpty — `before=0` asks for the range [0,0). Absent is
// TranscriptTail and means the opposite; conflating them would make a client that
// sends its cursor unconditionally show the newest page when it asked for the oldest.
func TestMessagePageAtZeroIsEmpty(t *testing.T) {
	f := newCCFixture(t, "/w")
	f.write(t, "/w", "cc-session-0001.jsonl", ccUser(t, "only turn"))

	page, ok := f.store().MessagePage("cc-session-0001", 0)
	if !ok {
		t.Fatal("MessagePage reported the transcript missing")
	}
	if len(page.Messages) != 0 || page.HasEarlier {
		t.Errorf("page = %+v, want empty with nothing earlier", page)
	}
}

// TestMessagePageWidensAnEarlierPagePastAWallOfToolCalls — the widen retry applies to
// a page the reader asked for, not only to the one the pane opens on.
//
// One rule, and the reason is the one ccMessagesTailBytes already gives: a window
// that is all tool payload yields nothing to READ, and handing that to a reader who
// just clicked "load earlier" is the same defect as handing it to one who just opened
// the pane. The fixture buries the conversation more than one narrow window behind a
// wall of calls, so only a widened read can reach it.
func TestMessagePageWidensAnEarlierPagePastAWallOfToolCalls(t *testing.T) {
	call := func(i int) string {
		return ccToolUse(t, "2026-08-17T03:00:00.000Z",
			ccCall(fmt.Sprintf("t%06d", i), "Bash", map[string]any{"command": strings.Repeat("z", 16<<10)}))
	}
	var b strings.Builder
	b.WriteString(ccUser(t, "BURIED-BEHIND-THE-CALLS"))
	for i := 0; b.Len() < ccMessagesTailBytes+(256<<10); i++ {
		b.WriteString(call(i))
	}
	// The newest page is ordinary conversation, so the reader has to page back to
	// reach the wall at all.
	wallEnds := int64(b.Len())
	b.WriteString(ccUser(t, "RECENT-AND-READABLE"))

	f := newCCFixture(t, "/w")
	f.write(t, "/w", "cc-session-0001.jsonl", b.String())

	page, ok := f.store().MessagePage("cc-session-0001", wallEnds)
	if !ok {
		t.Fatal("MessagePage reported the transcript missing")
	}
	if !ccHasConversation(page.Messages) {
		t.Fatalf("an earlier page did not widen past a wall of tool CALLS: %d messages, none with words",
			len(page.Messages))
	}
	if got := ccTexts(page.Messages); len(got) != 1 || got[0] != "BURIED-BEHIND-THE-CALLS" {
		t.Errorf("texts = %q, want the buried turn", got)
	}
}

// TestMessagePageCursorSurvivesAMergedTurnAtTheTrimBoundary — the gap a mutation
// battery found, and it is exactly the kind this file's other comments warn about.
//
// `starts` is index-aligned with `out`, which means it must be appended to when a line
// OPENS a message and not when a line MERGES into the previous assistant turn. Every
// other cursor test here uses a fixture with no merges — TestMessagePageCursorSurvives-
// TheCOUNTCap is all user turns, and the walk fixture alternates user/assistant so no
// two assistant records are adjacent — so appending on the merge path as well left every
// one of them green while the cursor pointed at the wrong record.
//
// This fixture is three records per turn (user, assistant, assistant) so that every turn
// contributes TWO messages from THREE lines, and it goes past the count cap so that
// starts[drop] is the value under test rather than a constant zero. Misalignment then
// puts the cursor too early and the earlier page serves more than the cap trimmed.
func TestMessagePageCursorSurvivesAMergedTurnAtTheTrimBoundary(t *testing.T) {
	const turnCount = 120
	const overflow = 2*turnCount - ccMessagesMax // texts the cap must trim
	var b strings.Builder
	var want []string
	for i := 0; i < turnCount; i++ {
		u := fmt.Sprintf("U-%04d", i)
		a1 := fmt.Sprintf("A-%04d-first", i)
		a2 := fmt.Sprintf("A-%04d-second", i)
		b.WriteString(ccUser(t, u))
		b.WriteString(ccAssistantWords(t, a1, "2026-08-17T03:00:01.000Z"))
		b.WriteString(ccAssistantWords(t, a2, "2026-08-17T03:00:02.000Z"))
		// The two assistant records MERGE into one bubble, joined by ccTurnJoin.
		want = append(want, u, a1+ccTurnJoin+a2)
	}
	body := b.String()
	if len(body) >= ccMessagesTailBytes {
		t.Fatalf("the fixture is %d bytes — the BYTE window would bind and the count cap would not", len(body))
	}

	f := newCCFixture(t, "/w")
	f.write(t, "/w", "cc-session-0001.jsonl", body)
	store := f.store()

	page, ok := store.MessagePage("cc-session-0001", TranscriptTail)
	if !ok {
		t.Fatal("MessagePage reported the transcript missing")
	}
	// The precondition that makes this test about merging at all: two assistant records
	// per turn really did become ONE message.
	if got := len(ccTexts(page.Messages)); got != ccMessagesMax {
		t.Fatalf("newest page served %d texts, want the cap %d (if it is ~%d the merge did not happen)",
			got, ccMessagesMax, 3*turnCount-ccMessagesMax)
	}
	if !page.HasEarlier {
		t.Fatal("the cap trimmed and the page reports nothing earlier")
	}

	earlier, ok := store.MessagePage("cc-session-0001", page.Earlier)
	if !ok {
		t.Fatal("MessagePage reported the transcript missing on the earlier page")
	}
	got := ccTexts(earlier.Messages)
	// EXACT, and exactness is the whole guard: a cursor computed from a misaligned
	// starts[] lands on an earlier record, so the earlier page serves MORE than the cap
	// trimmed — which a length-or-more assertion would accept.
	if len(got) != overflow {
		t.Fatalf("the earlier page served %d texts, want exactly the %d the cap trimmed", len(got), overflow)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("earlier page diverges at index %d:\n got %.40q\nwant %.40q", i, got[i], want[i])
		}
	}
	if earlier.HasEarlier {
		t.Errorf("the earlier page reaches turn 0 and still reports Earlier = %d", earlier.Earlier)
	}
}
