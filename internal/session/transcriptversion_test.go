package session

// tether#106 — the cheap "has this transcript changed?" answer.
//
// The whole feature rests on one property: the version a reader is given must
// describe the file a reader would be SERVED. If those can come apart, the failure
// is silent and total — a probe watching a file nobody reads reports "nothing
// changed" forever, i.e. the pane degrades to exactly the frozen transcript this
// slice exists to fix, with a poller running to prove it is trying.
//
// So the fixtures below deliberately arrange for the two stores to hold the same
// sid with DIFFERENT mtimes. A test where they agree cannot tell a correct store
// choice from a coin flip.
//
// Helpers come from sessionlist_test.go (writeTranscript / userLine) and
// ccsessions_test.go (newCCFixture / ccUser), the same ones the merge is tested
// with, so this cannot pass against a fixture shape List has never seen.

import (
	"os"
	"testing"
	"time"
)

// transcriptWatchTSPath is the hand-written TypeScript side of the header contract.
// Relative to this package's directory, which is where `go test` runs.
const transcriptWatchTSPath = "../../web/src/lib/transcriptWatch.ts"

// TestTranscriptUpdatedAtPrefersTheStoreThatServes — both stores hold the sid, and
// the timestamps differ by an hour, so answering from the wrong one is visible
// rather than merely possible.
func TestTranscriptUpdatedAtPrefersTheStoreThatServes(t *testing.T) {
	dir := t.TempDir()
	tetherAt := time.Now().Add(-2 * time.Hour).Truncate(time.Millisecond)
	ccAt := time.Now().Add(-1 * time.Hour).Truncate(time.Millisecond)

	const sid = "shared-sid-0001"
	writeTranscript(t, dir, sid, tetherAt, userLine("tether recorded this"))
	f := newCCFixture(t, "/w")
	ccPath := f.write(t, "/w", sid+".jsonl", ccUser(t, "cc recorded this too"))
	if err := os.Chtimes(ccPath, ccAt, ccAt); err != nil {
		t.Fatal(err)
	}

	idx := &SessionIndex{History: NewHistoryStore(dir), CC: f.store()}

	// The premise: Messages serves tether's copy for this sid. Asserted rather than
	// assumed, because if the preference ever flips, the expectation below flips
	// with it and this test must fail loudly instead of quietly testing nothing.
	if _, src := idx.Messages(sid); src != SourceTether {
		t.Fatalf("Messages source = %q, want %q — the premise of this test", src, SourceTether)
	}

	got := idx.TranscriptUpdatedAt(sid)
	if want := tetherAt.UnixMilli(); got != want {
		t.Errorf("TranscriptUpdatedAt = %d, want %d (tether's mtime); cc's is %d — a version read off the store that is NOT serving never moves when the served file does",
			got, want, ccAt.UnixMilli())
	}
}

// TestTranscriptUpdatedAtFallsThroughToCC — the other half of the same rule.
func TestTranscriptUpdatedAtFallsThroughToCC(t *testing.T) {
	dir := t.TempDir()
	at := time.Now().Add(-30 * time.Minute).Truncate(time.Millisecond)

	f := newCCFixture(t, "/w")
	ccPath := f.write(t, "/w", "cc-session-0001.jsonl", ccUser(t, "only cc has this"))
	if err := os.Chtimes(ccPath, at, at); err != nil {
		t.Fatal(err)
	}
	idx := &SessionIndex{History: NewHistoryStore(dir), CC: f.store()}

	if _, src := idx.Messages("cc-session-0001"); src != SourceCC {
		t.Fatalf("Messages source = %q, want %q — the premise of this test", src, SourceCC)
	}
	if got, want := idx.TranscriptUpdatedAt("cc-session-0001"), at.UnixMilli(); got != want {
		t.Errorf("TranscriptUpdatedAt = %d, want %d (cc's mtime)", got, want)
	}
}

// TestTranscriptUpdatedAtIsZeroWhenNothingHasIt — 0 is the "unknown" the route uses
// to decide not to send the header at all, and the SPA reads a missing header as
// "no baseline". A non-zero answer for a session neither store has would put a
// version on the wire for a transcript that does not exist.
func TestTranscriptUpdatedAtIsZeroWhenNothingHasIt(t *testing.T) {
	// Only sids nothing has. The traversal case is deliberately NOT here: it would
	// answer 0 with or without the sid guard, so putting it in this list would read as
	// a security assertion while asserting nothing. That guard is pinned where it can
	// fail — TestHasHistory and TestFindRejectsATraversingSid.
	idx := &SessionIndex{History: NewHistoryStore(t.TempDir()), CC: newCCFixture(t, "/w").store()}
	for _, sid := range []string{"absent-sid-0001", ""} {
		if got := idx.TranscriptUpdatedAt(sid); got != 0 {
			t.Errorf("TranscriptUpdatedAt(%q) = %d, want 0", sid, got)
		}
	}
}

// TestTranscriptUpdatedAtIsZeroOnAnEmptyTranscript — an existing but zero-length
// history.jsonl is "no conversation" everywhere else in this package (HasHistory,
// List), and it has to be here too: a session directory is created by the work-item
// binding before anything is said, so this is a real state and not a contrived one.
func TestTranscriptUpdatedAtIsZeroOnAnEmptyTranscript(t *testing.T) {
	dir := t.TempDir()
	writeTranscript(t, dir, "empty-sid-0001", time.Now())
	idx := &SessionIndex{History: NewHistoryStore(dir)}
	if got := idx.TranscriptUpdatedAt("empty-sid-0001"); got != 0 {
		t.Errorf("TranscriptUpdatedAt on an empty transcript = %d, want 0", got)
	}
}

// TestHasHistoryAndModTimeAnswerTheSameQuestion pins the ANSWERS, per case, against
// values written out here.
//
// Asserting only that the two agree would be a tautology today — both read
// transcriptStat, so any mutation moves them together — and a test that cannot fail is
// worse than none, because its name promises a guard. Writing the expected answer for
// each case gives it something to lose: it fails if either reader drifts AND if the
// shared predicate itself changes (the empty file, the rejected sid).
//
// It matters because HasHistory is what SessionIndex.Messages branches on and ModTime
// is what SessionIndex.TranscriptUpdatedAt branches on, and one case of disagreement is
// enough to serve one store's transcript with the other store's version.
func TestHasHistoryAndModTimeAnswerTheSameQuestion(t *testing.T) {
	dir := t.TempDir()
	writeTranscript(t, dir, "good-sid-00001", time.Now(), userLine("hello"))
	writeTranscript(t, dir, "empty-sid-0001", time.Now())
	h := NewHistoryStore(dir)

	for _, tc := range []struct {
		sid  string
		want bool
		why  string
	}{
		{"good-sid-00001", true, "a non-empty transcript"},
		{"empty-sid-0001", false, "a zero-length file is not a conversation"},
		{"absent-sid-001", false, "no file"},
		{"../etc/passwd", false, "rejected by ValidSessionID before any path is built"},
		{"", false, "empty sid"},
	} {
		if got := h.HasHistory(tc.sid); got != tc.want {
			t.Errorf("HasHistory(%q) = %v, want %v — %s", tc.sid, got, tc.want, tc.why)
		}
		if _, got := h.ModTime(tc.sid); got != tc.want {
			t.Errorf("ModTime(%q) ok = %v, want %v — %s", tc.sid, got, tc.want, tc.why)
		}
	}
}

// TestTranscriptUpdatedAtHeaderIsMirroredInTypeScript — the same guard, and the same
// argument, as TestSessionActivityContractIsMirroredInTypeScript.
//
// A header name is a plain string on both sides. Rename it in Go alone and every Go
// test still passes; rename it in TypeScript alone and `tsc -b` exits 0, every typed
// fixture stays green, and the probe reads `undefined` from every response forever —
// which does not error, it just never reports a change. The feature is then dead in
// precisely the way that looks alive.
func TestTranscriptUpdatedAtHeaderIsMirroredInTypeScript(t *testing.T) {
	src, err := os.ReadFile(transcriptWatchTSPath)
	if err != nil {
		t.Fatalf("read the hand-written mirror at %s: %v", transcriptWatchTSPath, err)
	}
	// tsHasStringLiteral (activity_test.go) requires a QUOTED literal in code, so a
	// name surviving only in a doc comment — what a half-done rename leaves behind —
	// does not satisfy this.
	if !tsHasStringLiteral(string(src), TranscriptUpdatedAtHeader) {
		t.Errorf("TranscriptUpdatedAtHeader = %q does not appear as a quoted literal in %s; the daemon would set a header the SPA never reads, and nothing in either language fails",
			TranscriptUpdatedAtHeader, transcriptWatchTSPath)
	}
}
