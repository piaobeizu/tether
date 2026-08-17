package session

// tether#92 — the merge: two stores, one list.
//
// Helpers come from sessionlist_test.go (writeTranscript / userLine / sids) and
// ccsessions_test.go (newCCFixture / ccUser / ccAssistant), deliberately, so the
// merge is exercised through the same fixtures each side is tested with alone.

import (
	"os"
	"path/filepath"
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
