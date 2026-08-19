package session

// tether#92 — the merge: two stores, one list.
//
// Helpers come from sessionlist_test.go (writeTranscript / userLine / sids) and
// ccsessions_test.go (newCCFixture / ccUser / ccAssistant), deliberately, so the
// merge is exercised through the same fixtures each side is tested with alone.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// ccIndex builds a SessionIndex over a tether sessions dir plus a cc fixture.
func ccIndex(t *testing.T, tetherDir string, f *ccFixture) *SessionIndex {
	t.Helper()
	return &SessionIndex{History: NewHistoryStore(tetherDir), CC: f.store()}
}

// TestListMergesBothStoresIntoOneOrder — the whole point of merging rather than
// grouping. The fixture makes the interleaving load-bearing: the cc session is
// NEWER than one tether session and OLDER than the other, so a merge that
// appended cc rows without re-sorting, or that sorted each store separately,
// produces a different answer than this asserts.
func TestListMergesBothStoresIntoOneOrder(t *testing.T) {
	dir := t.TempDir()
	base := time.Now().Add(-time.Hour)
	writeTranscript(t, dir, "tttt1111", base.Add(50*time.Minute), userLine("newest overall"))
	writeTranscript(t, dir, "tttt2222", base.Add(10*time.Minute), userLine("oldest overall"))

	f := newCCFixture(t, "/w")
	ccPath := f.write(t, "/w", "cc-session-0001.jsonl", ccUser(t, "in the middle"))
	mid := base.Add(30 * time.Minute)
	if err := os.Chtimes(ccPath, mid, mid); err != nil {
		t.Fatal(err)
	}

	got := sids(ccIndex(t, dir, f).List())
	want := []string{"tttt1111", "cc-session-0001", "tttt2222"}
	if len(got) != len(want) {
		t.Fatalf("List() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("List() = %v, want %v", got, want)
		}
	}
}

// TestListLabelsEveryRowWithItsStore — the source is what the UI's promise hangs
// off, so a row that came from tether must never be labelled cc and vice versa.
func TestListLabelsEveryRowWithItsStore(t *testing.T) {
	dir := t.TempDir()
	writeTranscript(t, dir, "tttt1111", time.Now(), userLine("tether recorded this"))
	f := newCCFixture(t, "/w")
	f.write(t, "/w", "cc-session-0001.jsonl", ccUser(t, "cc recorded this"))

	byS := map[string]string{}
	for _, r := range ccIndex(t, dir, f).List() {
		byS[r.Sid] = r.Source
	}
	if byS["tttt1111"] != SourceTether {
		t.Errorf("tether row source = %q, want %q", byS["tttt1111"], SourceTether)
	}
	if byS["cc-session-0001"] != SourceCC {
		t.Errorf("cc row source = %q, want %q", byS["cc-session-0001"], SourceCC)
	}
}

// TestListTetherWinsForASharedSid — the sid spaces overlap: on one real profile
// 55 of 90 tether sids had a byte-identical cc file. One conversation must be one
// row, and it must be the one tether recorded, because that is the transcript the
// live session is already reading.
func TestListTetherWinsForASharedSid(t *testing.T) {
	const sid = "shared-session-0001"
	dir := t.TempDir()
	writeTranscript(t, dir, sid, time.Now(), userLine("tether's version"))

	f := newCCFixture(t, "/w")
	f.write(t, "/w", sid+".jsonl", ccUser(t, "cc's version"))

	rows := ccIndex(t, dir, f).List()
	if len(rows) != 1 {
		t.Fatalf("List() = %d rows, want 1 — one conversation is one row: %+v", len(rows), rows)
	}
	if rows[0].Source != SourceTether {
		t.Errorf("Source = %q, want %q", rows[0].Source, SourceTether)
	}
	if rows[0].Title != "tether's version" {
		t.Errorf("Title = %q, want tether's", rows[0].Title)
	}
}

// TestListAttachesWorkItemsToCCRowsToo — a binding is a note a human attached to
// a SESSION; it knows nothing about which store the transcript landed in. When
// the lookup lived inside the tether loop, "bind a work item, see the label"
// worked for half the list and silently not the other half.
func TestListAttachesWorkItemsToCCRowsToo(t *testing.T) {
	dir := t.TempDir()
	wis := NewWIBindingStore(dir)
	if err := wis.Save("cc-session-0001", "tether#92"); err != nil {
		t.Fatal(err)
	}
	f := newCCFixture(t, "/w")
	f.write(t, "/w", "cc-session-0001.jsonl", ccUser(t, "some prompt"))

	idx := &SessionIndex{History: NewHistoryStore(dir), WI: wis, CC: f.store()}
	rows := idx.List()
	var row *SessionSummary
	for i := range rows {
		if rows[i].Sid == "cc-session-0001" {
			row = &rows[i]
			break
		}
	}
	if row == nil {
		t.Fatal("the cc session is not in the list")
	}
	if row.WorkItem != "tether#92" {
		t.Errorf("WorkItem = %q, want tether#92", row.WorkItem)
	}
}

// TestMessagesRoutesToTheStoreThatOwnsTheSid — the same precedence List uses,
// asserted from the other end. If these two ever disagree, a row says it came
// from one store while its transcript comes from the other.
func TestMessagesRoutesToTheStoreThatOwnsTheSid(t *testing.T) {
	const shared = "shared-session-0001"
	dir := t.TempDir()
	writeTranscript(t, dir, "tttt1111", time.Now(), userLine("tether only"))
	writeTranscript(t, dir, shared, time.Now(), userLine("tether's version"))

	f := newCCFixture(t, "/w")
	f.write(t, "/w", "cc-session-0001.jsonl", ccUser(t, "cc only"))
	f.write(t, "/w", shared+".jsonl", ccUser(t, "cc's version"))

	idx := ccIndex(t, dir, f)

	for _, tc := range []struct {
		name, sid, wantText, wantSource string
	}{
		{"tether only", "tttt1111", "tether only", SourceTether},
		{"cc only", "cc-session-0001", "cc only", SourceCC},
		{"both — tether wins", shared, "tether's version", SourceTether},
	} {
		t.Run(tc.name, func(t *testing.T) {
			msgs, source := idx.Messages(tc.sid)
			if source != tc.wantSource {
				t.Errorf("source = %q, want %q", source, tc.wantSource)
			}
			if len(msgs) == 0 || msgs[0].Text != tc.wantText {
				t.Errorf("messages = %+v, want first text %q", msgs, tc.wantText)
			}
		})
	}

	// A session neither store has is not an error — openSession asks for exactly
	// this the moment a new session is created.
	msgs, source := idx.Messages("nobody-has-this-1")
	if msgs != nil || source != SourceNone {
		t.Errorf("Messages(unknown) = %+v, %q; want nil, %q", msgs, source, SourceNone)
	}
	if msgs, source := (*SessionIndex)(nil).Messages("tttt1111"); msgs != nil || source != SourceNone {
		t.Errorf("nil index Messages = %+v, %q", msgs, source)
	}
}

// TestListWithoutACCStoreIsUnchanged — a daemon assembled without the cc reader
// behaves exactly as it did before tether#92. The optional-field convention the
// other three stores follow.
func TestListWithoutACCStoreIsUnchanged(t *testing.T) {
	dir := t.TempDir()
	writeTranscript(t, dir, "tttt1111", time.Now(), userLine("only mine"))

	rows := (&SessionIndex{History: NewHistoryStore(dir)}).List()
	if len(rows) != 1 || rows[0].Sid != "tttt1111" || rows[0].Source != SourceTether {
		t.Fatalf("List() = %+v", rows)
	}
}

// TestListWorksWithNoTetherHistoryAtAll — the case the feature is FOR: a machine
// where every conversation happened in a terminal, so tether's own directory is
// empty and the whole list comes from cc. A merge that early-returned on an
// unreadable tether directory would show nothing here, which is the pre-existing
// behaviour this change has to move off.
func TestListWorksWithNoTetherHistoryAtAll(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "never-created")
	f := newCCFixture(t, "/w")
	f.write(t, "/w", "cc-session-0001.jsonl", ccUser(t, "typed in a terminal"))

	rows := (&SessionIndex{History: NewHistoryStore(missing), CC: f.store()}).List()
	if len(rows) != 1 || rows[0].Sid != "cc-session-0001" || rows[0].Title != "typed in a terminal" {
		t.Fatalf("List() = %+v, want the cc session", rows)
	}
}

// ---------------------------------------------------------------------------
// tether#101 — RunningAs on the rows
// ---------------------------------------------------------------------------
//
// B is a HINT, not the authority. These tests pin what the list says; what
// happens on the click is Attachment.resolve's, and attach_cc_test.go pins that.
// The two must not be confused, which is why the field is named for an
// observation ("running as") rather than for a promise.

// TestListMarksASessionALiveBackgroundAgentIsHolding — the feature.
//
// The fixture deliberately holds only ONE of two rows, because "every row gets
// the badge" and "the right row gets the badge" are different assertions and only
// the second one is worth anything.
func TestListMarksASessionALiveBackgroundAgentIsHolding(t *testing.T) {
	requireLinux(t)
	dir := t.TempDir()
	base := time.Now().Add(-time.Hour)
	writeTranscript(t, dir, "held-row-0000001", base.Add(20*time.Minute), userLine("the one a job is using"))
	writeTranscript(t, dir, "free-row-0000001", base.Add(10*time.Minute), userLine("the one nothing is using"))

	reg := newCCRegFixture(t)
	reg.write(t, bgRecord(os.Getpid(), "held-row-0000001", liveToken(t)))

	idx := &SessionIndex{History: NewHistoryStore(dir), CCJobs: reg.reg()}
	rows := idx.List()
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want 2", len(rows))
	}
	byID := map[string]SessionSummary{}
	for _, r := range rows {
		byID[r.Sid] = r
	}
	if got := byID["held-row-0000001"].RunningAs; got != "bg" {
		t.Errorf("held row RunningAs = %q, want \"bg\" — cc's own kind value, quoted rather than derived", got)
	}
	if got := byID["free-row-0000001"].RunningAs; got != "" {
		t.Errorf("free row RunningAs = %q, want \"\"; an unheld row must assert nothing", got)
	}
	// Still listed, still ordinary in every other way: the row is a hint, and the
	// UI is required not to disable it (see SessionSummary.RunningAs).
	if byID["held-row-0000001"].Title != "the one a job is using" {
		t.Errorf("held row Title = %q; marking a row must not change anything else about it", byID["held-row-0000001"].Title)
	}
}

// TestListMarksARowFromEitherStore — the badge is a fact about the SESSION, not
// about which store its transcript came from, so it has to be attached over every
// row.
//
// A cc-source row is the common case (those are the sessions a terminal `claude -p`
// job created). A tether-source row can be held too: nothing stops a background job
// resuming a sid tether once recorded. Marking only the cc rows would be right for
// most of the list and quietly wrong for the rest — the same mistake tether#92 made
// with work items, where the binding loop lived inside the tether pass.
func TestListMarksARowFromEitherStore(t *testing.T) {
	requireLinux(t)
	dir := t.TempDir()
	writeTranscript(t, dir, "tether-held-0001", time.Now().Add(-time.Hour), userLine("recorded by tether"))

	f := newCCFixture(t, "/w")
	f.write(t, "/w", "cc-held-00000001.jsonl", ccUser(t, "recorded by cc"))

	reg := newCCRegFixture(t)
	reg.write(t, bgRecord(os.Getpid(), "tether-held-0001", liveToken(t)))
	// Only the TETHER row is held, deliberately. A record is named after its pid, so
	// two live holders would need two live pids, and inventing a second live process
	// would buy nothing: the direction that could be wrong is "the loop skipped the
	// tether pass's rows", and one held tether row next to one unheld cc row is
	// exactly the fixture that catches it.
	idx := &SessionIndex{History: NewHistoryStore(dir), CC: f.store(), CCJobs: reg.reg()}
	rows := idx.List()
	var tetherRow, ccRow SessionSummary
	for _, r := range rows {
		switch r.Sid {
		case "tether-held-0001":
			tetherRow = r
		case "cc-held-00000001":
			ccRow = r
		}
	}
	if tetherRow.Source != SourceTether || ccRow.Source != SourceCC {
		t.Fatalf("fixture did not produce one row per store: tether %+v, cc %+v", tetherRow, ccRow)
	}
	if tetherRow.RunningAs != "bg" {
		t.Errorf("a TETHER-source row held by a background agent has RunningAs %q, want \"bg\"; the badge is a fact about the session, not about the store", tetherRow.RunningAs)
	}
	if ccRow.RunningAs != "" {
		t.Errorf("cc row RunningAs = %q, want \"\" — nothing in the registry holds it", ccRow.RunningAs)
	}
}

// TestListLeavesRowsUnmarkedWithoutARegistryReader — a daemon assembled without
// CCJobs (every daemon before tether#101) serves the same list it always did. The
// absent value is the fail-open direction: no badge asserts nothing, so a daemon
// that cannot see cc's registry cannot mislead anyone.
func TestListLeavesRowsUnmarkedWithoutARegistryReader(t *testing.T) {
	dir := t.TempDir()
	writeTranscript(t, dir, "plain-row-000001", time.Now().Add(-time.Hour), userLine("nothing special"))

	for _, tc := range []struct {
		name string
		idx  *SessionIndex
	}{
		{"nil CCJobs", &SessionIndex{History: NewHistoryStore(dir)}},
		{"missing registry dir", &SessionIndex{
			History: NewHistoryStore(dir),
			CCJobs:  NewCCRegistry(filepath.Join(t.TempDir(), "gone", "sessions")),
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rows := tc.idx.List()
			if len(rows) != 1 {
				t.Fatalf("rows = %d, want 1", len(rows))
			}
			if rows[0].RunningAs != "" {
				t.Errorf("RunningAs = %q, want \"\"", rows[0].RunningAs)
			}
		})
	}
}

// TestListDoesNotMarkARowForADeadOrInteractiveRecord — the anti-mislabel pair, at
// the LIST level rather than the reader level.
//
// Asserted here as well as in ccregistry_test.go because the two failures are
// different KINDS, not different sizes. In the reader a mislabel is a wrong map
// entry; here it is a badge a user acts on — and a badge that appears on rows
// nothing is holding trains them to ignore it, after which the badge on the row
// that IS held says nothing either.
//
// Deliberately no count. The sid figures live in ccregistry_test.go's three-shape
// test (measured: 2 sids mismarked today without the liveness check, growing) and
// repeating them here is how the two copies drift; what matters at this layer is
// the direction — a warning that fires when it should not is worse than no warning,
// because it costs the credibility of the one that fires correctly.
func TestListDoesNotMarkARowForADeadOrInteractiveRecord(t *testing.T) {
	requireLinux(t)
	for _, tc := range []struct {
		name string
		rec  func(t *testing.T, sid string) map[string]any
	}{
		{"dead pid, kind bg", func(t *testing.T, sid string) map[string]any {
			return bgRecord(deadPid(t), sid, "1")
		}},
		{"live pid, kind interactive", func(t *testing.T, sid string) map[string]any {
			return interactiveRecord(os.Getpid(), sid, liveToken(t))
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			writeTranscript(t, dir, "row-unmarked-001", time.Now().Add(-time.Hour), userLine("hello"))
			reg := newCCRegFixture(t)
			reg.write(t, tc.rec(t, "row-unmarked-001"))

			rows := (&SessionIndex{History: NewHistoryStore(dir), CCJobs: reg.reg()}).List()
			if len(rows) != 1 {
				t.Fatalf("rows = %d, want 1", len(rows))
			}
			if rows[0].RunningAs != "" {
				t.Errorf("RunningAs = %q, want \"\"", rows[0].RunningAs)
			}
		})
	}
}

// TestSessionSummaryRunningAsIsOmittedWhenEmpty pins the wire shape, because the
// field's doc comment argues for omitempty on the strength of it and a doc comment
// is not a struct tag.
//
// Both directions: absent when there is nothing to say (which is most rows), and
// present under exactly the key the hand-written TypeScript mirror reads.
func TestSessionSummaryRunningAsIsOmittedWhenEmpty(t *testing.T) {
	plain, err := json.Marshal(SessionSummary{Sid: "s", Source: SourceTether})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(plain), "runningAs") {
		t.Errorf("an unheld row serialised %s; the empty value is the common case and carries no information", plain)
	}
	held, err := json.Marshal(SessionSummary{Sid: "s", Source: SourceTether, RunningAs: "bg"})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(held), `"runningAs":"bg"`) {
		t.Errorf("a held row serialised %s, want a `\"runningAs\":\"bg\"` member", held)
	}
}

// ---------------------------------------------------------------------------
// Which kind of "top" is this? (tether#107)
// ---------------------------------------------------------------------------

// TestMessagePageNamesTheOtherRecordWhenBothStoresHaveTheSid — the field that stops
// the top of a transcript claiming to be the beginning of the conversation when it is
// only the beginning of tether's record.
//
// This combination is EMPTY on the reference machine (tether#107 measured 0 sessions
// with both a tether record and a cc side over the window), so it can only be pinned
// synthetically, and the mechanism is real: TranscriptUpdatedAt's second residual is
// the same combination — a sid tether once recorded that a `claude -p` job later
// resumed into cc.
//
// It is deliberately NOT an offer to serve cc's record. Which store wins is the rule
// Messages states, and tether#107 left it exactly as it was.
func TestMessagePageNamesTheOtherRecordWhenBothStoresHaveTheSid(t *testing.T) {
	const shared = "shared-session-0001"
	dir := t.TempDir()
	writeTranscript(t, dir, "tttt1111", time.Now(), userLine("tether only"))
	writeTranscript(t, dir, shared, time.Now(), userLine("tether's short record"))

	f := newCCFixture(t, "/w")
	f.write(t, "/w", "cc-session-0001.jsonl", ccUser(t, "cc only"))
	f.write(t, "/w", shared+".jsonl", ccUser(t, "cc's much longer record"))

	idx := ccIndex(t, dir, f)

	both := idx.MessagePage(shared, TranscriptTail)
	if both.Source != SourceTether {
		t.Fatalf("source = %q, want %q — tether#107 must not have changed which store wins", both.Source, SourceTether)
	}
	if both.OtherRecord != SourceCC {
		t.Errorf("OtherRecord = %q, want %q; the pane would claim this is the beginning of the conversation",
			both.OtherRecord, SourceCC)
	}
	if len(both.Messages) == 0 || both.Messages[0].Text != "tether's short record" {
		t.Errorf("messages = %+v, want tether's copy", both.Messages)
	}

	// Only tether has it: there is no other record, so the top of the transcript
	// really is the beginning of the conversation.
	if only := idx.MessagePage("tttt1111", TranscriptTail); only.OtherRecord != "" {
		t.Errorf("OtherRecord = %q for a sid only tether has, want empty", only.OtherRecord)
	}
	// Only cc has it. The symmetric case is unreachable rather than unimplemented —
	// tether having the sid is what selects tether — so this pins "" and says why.
	if only := idx.MessagePage("cc-session-0001", TranscriptTail); only.OtherRecord != "" {
		t.Errorf("OtherRecord = %q for a cc-only sid, want empty", only.OtherRecord)
	}
}

// TestMessagePageOnTetherHistoryHasNothingEarlier — tether's own store has no
// ceiling to remove: LoadHistory is an os.ReadFile of the whole file.
//
// And a cursor spent against that branch returns an EMPTY page, not the file again.
// Serving the whole transcript for a `before` would append a second copy of every
// message already on screen, which is a worse answer than nothing and would look like
// the transcript had doubled.
func TestMessagePageOnTetherHistoryHasNothingEarlier(t *testing.T) {
	dir := t.TempDir()
	writeTranscript(t, dir, "tttt1111", time.Now(), userLine("first"), aiLine("second"))
	idx := &SessionIndex{History: NewHistoryStore(dir)}

	newest := idx.MessagePage("tttt1111", TranscriptTail)
	if newest.Source != SourceTether || len(newest.Messages) != 2 {
		t.Fatalf("newest page = %+v", newest)
	}
	if newest.HasEarlier {
		t.Errorf("HasEarlier = true on an unbounded store; Earlier = %d", newest.Earlier)
	}

	paged := idx.MessagePage("tttt1111", 4096)
	if len(paged.Messages) != 0 {
		t.Errorf("a cursor on tether's branch served %d messages, want 0 — the transcript would double on screen",
			len(paged.Messages))
	}
	if paged.Source != SourceTether {
		t.Errorf("source = %q, want %q even for an empty page", paged.Source, SourceTether)
	}
	if paged.HasEarlier {
		t.Errorf("HasEarlier = true on the empty page; Earlier = %d", paged.Earlier)
	}
}

// TestMessagePageOnAnUnknownSidIsNotAnError — openSession asks for exactly this the
// moment a session is created, and the answer must carry no cursor and no other
// record, or the pane would render a top-of-transcript marker for a transcript that
// does not exist.
func TestMessagePageOnAnUnknownSidIsNotAnError(t *testing.T) {
	f := newCCFixture(t, "/w")
	idx := ccIndex(t, t.TempDir(), f)

	page := idx.MessagePage("nobody-has-this-1", TranscriptTail)
	if page.Source != SourceNone || len(page.Messages) != 0 || page.HasEarlier || page.OtherRecord != "" {
		t.Errorf("page = %+v, want an empty %q page", page, SourceNone)
	}
	if page := (*SessionIndex)(nil).MessagePage("tttt1111", TranscriptTail); page.Source != SourceNone {
		t.Errorf("nil index = %+v", page)
	}
}
