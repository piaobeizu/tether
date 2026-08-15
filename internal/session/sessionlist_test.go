package session

// tether#91 — the readable session list.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// writeTranscript creates <dir>/<sid>/history.jsonl with the given lines and
// stamps its mtime, so a test can arrange an ORDER independently of the names.
func writeTranscript(t *testing.T, dir, sid string, mtime time.Time, lines ...string) {
	t.Helper()
	sidDir := filepath.Join(dir, sid)
	if err := os.MkdirAll(sidDir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(sidDir, "history.jsonl")
	body := ""
	for _, l := range lines {
		body += l + "\n"
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, mtime, mtime); err != nil {
		t.Fatal(err)
	}
}

func userLine(text string) string { return `{"role":"user","text":` + quote(text) + `,"ts":1}` }
func aiLine(text string) string   { return `{"role":"assistant","text":` + quote(text) + `,"ts":2}` }

// quote encodes via encoding/json rather than by hand: a fixture built with a
// hand-rolled quoter silently produces INVALID transcript lines the moment the
// text contains a newline, and the code under test skips corrupt lines — so the
// bug shows up as a missing title, i.e. as a failure of the thing being tested.
func quote(s string) string {
	b, err := json.Marshal(s)
	if err != nil {
		panic(err)
	}
	return string(b)
}

func sids(rows []SessionSummary) []string {
	out := make([]string, len(rows))
	for i, r := range rows {
		out[i] = r.Sid
	}
	return out
}

// TestSessionIndex_OrdersByTimeNotByName is the regression this list exists for.
//
// The pre-tether#91 order was os.ReadDir (i.e. sorted by FILENAME, and the
// filenames are UUIDs) with the browser reversing it — which looks like "newest
// first" and is reverse-lexicographic order of random strings.
//
// So the fixture is built so the correct answer is NEITHER name order NOR its
// reverse: names sort a < b < c, times sort b(newest) > a > c. A test that only
// asserted "some deterministic order" would pass against the bug; this one can
// only pass if mtime is what is read.
func TestSessionIndex_OrdersByTimeNotByName(t *testing.T) {
	dir := t.TempDir()
	base := time.Now().Add(-time.Hour)
	writeTranscript(t, dir, "aaaa1111", base.Add(20*time.Minute), userLine("middle"))
	writeTranscript(t, dir, "bbbb2222", base.Add(40*time.Minute), userLine("newest"))
	writeTranscript(t, dir, "cccc3333", base.Add(5*time.Minute), userLine("oldest"))

	idx := &SessionIndex{History: NewHistoryStore(dir)}
	got := sids(idx.List())

	want := []string{"bbbb2222", "aaaa1111", "cccc3333"}
	if len(got) != len(want) {
		t.Fatalf("List() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("List() = %v, want %v (mtime order, which is neither the name order nor its reverse)", got, want)
		}
	}

	// And the timestamps themselves are exported, strictly descending, in ms.
	rows := idx.List()
	for i := 1; i < len(rows); i++ {
		if rows[i-1].UpdatedAt <= rows[i].UpdatedAt {
			t.Errorf("UpdatedAt not strictly descending: %d then %d", rows[i-1].UpdatedAt, rows[i].UpdatedAt)
		}
	}
	if want := base.Add(40 * time.Minute).UnixMilli(); rows[0].UpdatedAt != want {
		t.Errorf("UpdatedAt = %d, want %d (transcript mtime in ms)", rows[0].UpdatedAt, want)
	}
}

// TestSessionIndex_TieBreaksBySid — equal mtimes must still produce a stable
// order, or the list reshuffles between two requests that returned the same data.
func TestSessionIndex_TieBreaksBySid(t *testing.T) {
	dir := t.TempDir()
	at := time.Now().Add(-time.Hour)
	writeTranscript(t, dir, "zzzz9999", at, userLine("z"))
	writeTranscript(t, dir, "aaaa1111", at, userLine("a"))

	idx := &SessionIndex{History: NewHistoryStore(dir)}
	got := sids(idx.List())
	if len(got) != 2 || got[0] != "aaaa1111" || got[1] != "zzzz9999" {
		t.Errorf("List() = %v, want [aaaa1111 zzzz9999] (sid tie-break)", got)
	}
}

// TestSessionIndex_LabelPrecedence — a row's label is the work item when there is
// one, and the session's own first prompt when there is not. The two are separate
// fields on the wire, so the UI (not the daemon) owns the preference; what the
// daemon owes is that both are populated when available.
func TestSessionIndex_LabelPrecedence(t *testing.T) {
	dir := t.TempDir()
	at := time.Now().Add(-time.Hour)
	writeTranscript(t, dir, "aaaa1111", at.Add(time.Minute), aiLine("hello"), userLine("bind me"))
	writeTranscript(t, dir, "bbbb2222", at, userLine("  just   a   prompt \n"))

	if err := NewWIBindingStore(dir).Save("aaaa1111", "tether#91"); err != nil {
		t.Fatalf("Save: %v", err)
	}

	idx := &SessionIndex{History: NewHistoryStore(dir), WI: NewWIBindingStore(dir)}
	rows := idx.List()
	if len(rows) != 2 {
		t.Fatalf("List() returned %d rows, want 2: %+v", len(rows), rows)
	}
	if rows[0].Sid != "aaaa1111" || rows[0].WorkItem != "tether#91" {
		t.Errorf("rows[0] = %+v, want aaaa1111 bound to tether#91", rows[0])
	}
	// The title is still derived even when a work item exists — losing the binding
	// must not leave the row with nothing to show. It also proves the first USER
	// turn is picked, not the assistant line that precedes it.
	if rows[0].Title != "bind me" {
		t.Errorf("rows[0].Title = %q, want %q", rows[0].Title, "bind me")
	}
	if rows[1].WorkItem != "" {
		t.Errorf("rows[1].WorkItem = %q, want empty (no binding)", rows[1].WorkItem)
	}
	// Whitespace runs collapse, so a multi-line prompt is one readable line.
	if rows[1].Title != "just a prompt" {
		t.Errorf("rows[1].Title = %q, want %q", rows[1].Title, "just a prompt")
	}
}

// TestSessionIndex_TitleFallsBackToTheWorkspacePath — a session whose transcript
// opens with something other than a user turn still gets a label, from the
// workspace binding that is already on disk.
func TestSessionIndex_TitleFallsBackToTheWorkspacePath(t *testing.T) {
	dir := t.TempDir()
	writeTranscript(t, dir, "aaaa1111", time.Now(), aiLine("only the model spoke"))
	if err := NewBindingStore(dir).Save("aaaa1111", WorkspaceBinding{WorkspaceID: "ws-1", Path: "/w/tether"}); err != nil {
		t.Fatalf("Save: %v", err)
	}

	idx := &SessionIndex{History: NewHistoryStore(dir), Bindings: NewBindingStore(dir)}
	rows := idx.List()
	if len(rows) != 1 || rows[0].Title != "/w/tether" {
		t.Errorf("List() = %+v, want one row titled /w/tether", rows)
	}
}

// TestSessionIndex_TitleIsEmptyWhenNothingIsKnown — no user turn and no workspace
// binding leaves Title empty rather than inventing one. The UI falls back to the
// sid; a placeholder string here would be a fake the UI could not tell apart from
// a real title.
func TestSessionIndex_TitleIsEmptyWhenNothingIsKnown(t *testing.T) {
	dir := t.TempDir()
	writeTranscript(t, dir, "aaaa1111", time.Now(), aiLine("only the model spoke"))

	rows := (&SessionIndex{History: NewHistoryStore(dir)}).List()
	if len(rows) != 1 || rows[0].Title != "" || rows[0].WorkItem != "" {
		t.Errorf("List() = %+v, want one row with no title and no work item", rows)
	}
}

// TestSessionIndex_SkipsSessionsWithNoTranscript — the rule ListSessions
// established and this list inherits: the sidecar stores create <sid>/ before any
// message exists, so a directory is not evidence of a conversation. A session
// that only ever got a wi binding must not appear as a clickable row with an
// empty transcript.
func TestSessionIndex_SkipsSessionsWithNoTranscript(t *testing.T) {
	dir := t.TempDir()
	writeTranscript(t, dir, "aaaa1111", time.Now(), userLine("real"))
	if err := NewWIBindingStore(dir).Save("bbbb2222", "tether#91"); err != nil {
		t.Fatalf("Save: %v", err)
	}
	// An empty transcript file is also not a conversation.
	writeTranscript(t, dir, "cccc3333", time.Now())

	got := sids((&SessionIndex{History: NewHistoryStore(dir), WI: NewWIBindingStore(dir)}).List())
	if len(got) != 1 || got[0] != "aaaa1111" {
		t.Errorf("List() = %v, want [aaaa1111]", got)
	}
}

// TestSessionIndex_SkipsNamesTheDaemonWouldRefuse — a directory name that
// ValidSessionID rejects is skipped. Not a defence against an attacker (these
// come from ReadDir) but consistency: /api/v1/sessions/<sid>/messages would
// refuse that sid, so a row the user cannot open must not be offered.
func TestSessionIndex_SkipsNamesTheDaemonWouldRefuse(t *testing.T) {
	dir := t.TempDir()
	writeTranscript(t, dir, "aaaa1111", time.Now(), userLine("real"))
	writeTranscript(t, dir, "short", time.Now(), userLine("too short an id"))
	writeTranscript(t, dir, "has.a.dot", time.Now(), userLine("outside the alphabet"))

	got := sids((&SessionIndex{History: NewHistoryStore(dir)}).List())
	if len(got) != 1 || got[0] != "aaaa1111" {
		t.Errorf("List() = %v, want [aaaa1111]", got)
	}
}

// TestSessionIndex_CorruptTranscriptLinesAreSkipped — a bad line costs its own
// title, not the row and not the list. LoadHistory already treats individual bad
// lines this way.
func TestSessionIndex_CorruptTranscriptLinesAreSkipped(t *testing.T) {
	dir := t.TempDir()
	writeTranscript(t, dir, "aaaa1111", time.Now(),
		"{not json at all",
		`{"role":"user","text":"`, // truncated
		userLine("the good one"))

	rows := (&SessionIndex{History: NewHistoryStore(dir)}).List()
	if len(rows) != 1 || rows[0].Title != "the good one" {
		t.Errorf("List() = %+v, want one row titled %q", rows, "the good one")
	}
}

// TestSessionIndex_TitleReadIsBounded — the title scan reads a fixed prefix, so a
// huge opening turn costs that session its title and nothing else. The list must
// still contain the row, and the following sessions must be unaffected.
func TestSessionIndex_TitleReadIsBounded(t *testing.T) {
	dir := t.TempDir()
	huge := aiLine(strings.Repeat("x", titlePrefixBytes+1024))
	writeTranscript(t, dir, "aaaa1111", time.Now(), huge, userLine("past the window"))
	writeTranscript(t, dir, "bbbb2222", time.Now().Add(-time.Minute), userLine("fine"))

	rows := (&SessionIndex{History: NewHistoryStore(dir)}).List()
	if len(rows) != 2 {
		t.Fatalf("List() returned %d rows, want 2", len(rows))
	}
	if rows[0].Sid != "aaaa1111" || rows[0].Title != "" {
		t.Errorf("rows[0] = %+v, want aaaa1111 listed with no title", rows[0])
	}
	if rows[1].Title != "fine" {
		t.Errorf("rows[1].Title = %q, want %q", rows[1].Title, "fine")
	}
}

// TestSessionIndex_TitleIsCutByRuneNotByte — a long CJK prompt must not be sliced
// mid-rune into invalid UTF-8, which the JSON encoder would turn into replacement
// characters.
func TestSessionIndex_TitleIsCutByRuneNotByte(t *testing.T) {
	dir := t.TempDir()
	writeTranscript(t, dir, "aaaa1111", time.Now(), userLine(strings.Repeat("会", maxTitleLen+20)))

	rows := (&SessionIndex{History: NewHistoryStore(dir)}).List()
	if len(rows) != 1 {
		t.Fatalf("List() returned %d rows, want 1", len(rows))
	}
	got := []rune(rows[0].Title)
	if len(got) != maxTitleLen+1 { // maxTitleLen runes plus the ellipsis
		t.Fatalf("title is %d runes, want %d", len(got), maxTitleLen+1)
	}
	for i, r := range got[:maxTitleLen] {
		if r != '会' {
			t.Fatalf("rune %d = %q, want 会 (cut mid-rune?)", i, r)
		}
	}
	if got[maxTitleLen] != '…' {
		t.Errorf("title does not end with an ellipsis: %q", rows[0].Title)
	}
}

// TestSessionIndex_MissingDirIsAnEmptyList — a daemon that has never run has no
// sessions directory, and that is not an error.
func TestSessionIndex_MissingDirIsAnEmptyList(t *testing.T) {
	idx := &SessionIndex{History: NewHistoryStore(filepath.Join(t.TempDir(), "nope"))}
	if rows := idx.List(); len(rows) != 0 {
		t.Errorf("List() = %+v, want empty", rows)
	}
}

// TestSessionIndex_NoHistoryStoreIsAnEmptyList — mux.go only registers the route
// when reg.History is non-nil, but the type must not depend on that being
// remembered somewhere else.
func TestSessionIndex_NoHistoryStoreIsAnEmptyList(t *testing.T) {
	if rows := (&SessionIndex{}).List(); rows != nil {
		t.Errorf("List() = %+v, want nil", rows)
	}
	var nilIdx *SessionIndex
	if rows := nilIdx.List(); rows != nil {
		t.Errorf("nil index List() = %+v, want nil", rows)
	}
}
