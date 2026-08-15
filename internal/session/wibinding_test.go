package session

// tether#91 — the session -> wi binding. Its whole reason to exist is that the
// answer survives things the browser does not: a daemon restart, a different
// device, a cleared cache. So the tests are about what a SECOND reader sees, not
// about what the writer remembers.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

const wiSidFixture = "633e5ed8-cada-422a-aee1-c7a3502eb4fd"

// TestWIBindingStore_RoundTripSurvivesANewStore — the point of the file. A store
// constructed fresh over the same directory (i.e. a restarted daemon) must read
// back what a previous process wrote.
func TestWIBindingStore_RoundTripSurvivesANewStore(t *testing.T) {
	dir := t.TempDir()
	before := time.Now().UnixMilli()
	if err := NewWIBindingStore(dir).Save(wiSidFixture, "tether#91"); err != nil {
		t.Fatalf("Save: %v", err)
	}
	after := time.Now().UnixMilli()

	got, ok := NewWIBindingStore(dir).Load(wiSidFixture)
	if !ok {
		t.Fatalf("Load after Save: ok = false, want true")
	}
	if got.WorkItem != "tether#91" {
		t.Errorf("WorkItem = %q, want %q", got.WorkItem, "tether#91")
	}
	// Stamped by the daemon, not supplied by the client — so it must be a real
	// clock reading from the Save call, not a zero value.
	if got.BoundAt < before || got.BoundAt > after {
		t.Errorf("BoundAt = %d, want within [%d, %d]", got.BoundAt, before, after)
	}
}

// TestWIBindingStore_SaveOverwrites — one session's wi can be corrected. (The
// one-to-MANY this design buys is on the other axis: many sessions naming one
// wi. A single session still has exactly one work item.)
func TestWIBindingStore_SaveOverwrites(t *testing.T) {
	dir := t.TempDir()
	s := NewWIBindingStore(dir)
	if err := s.Save(wiSidFixture, "tether#90"); err != nil {
		t.Fatalf("Save 1: %v", err)
	}
	if err := s.Save(wiSidFixture, "tether#91"); err != nil {
		t.Fatalf("Save 2: %v", err)
	}
	got, ok := s.Load(wiSidFixture)
	if !ok || got.WorkItem != "tether#91" {
		t.Errorf("Load = %+v, %v; want tether#91", got, ok)
	}
}

// TestWIBindingStore_AbsentIsNotAnError — the ordinary case. Every session that
// predates this slice, and every session nobody associated with a work item.
func TestWIBindingStore_AbsentIsNotAnError(t *testing.T) {
	if b, ok := NewWIBindingStore(t.TempDir()).Load(wiSidFixture); ok {
		t.Errorf("Load of an unsaved sid = %+v, true; want zero, false", b)
	}
}

// TestWIBindingStore_CorruptOrEmptyFileIsIgnored — a truncated write, a
// hand-edited file, or a record naming no work item all read as "no binding".
//
// The empty-workItem case is the bug this store replaces, in file form: the old
// localStorage writer stored the current sid or the empty string, so "no session
// yet" was recorded as a real-looking mapping. A record that names nothing must
// not be a record.
func TestWIBindingStore_CorruptOrEmptyFileIsIgnored(t *testing.T) {
	dir := t.TempDir()
	s := NewWIBindingStore(dir)

	cases := []struct {
		name    string
		content string
	}{
		{"truncated json", `{"workItem":"teth`},
		{"not json at all", "wat"},
		{"empty file", ""},
		{"json but not an object", `["tether#91"]`},
		{"object with no workItem", `{"boundAt":123}`},
		{"object with an empty workItem", `{"workItem":"","boundAt":123}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			sidDir := filepath.Join(dir, wiSidFixture)
			if err := os.MkdirAll(sidDir, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(sidDir, "wi.json"), []byte(c.content), 0o600); err != nil {
				t.Fatal(err)
			}
			if b, ok := s.Load(wiSidFixture); ok {
				t.Errorf("Load(%s) = %+v, true; want zero, false", c.name, b)
			}
		})
	}
}

// TestWIBindingStore_RefusesTraversalSID — the sid reaches this store from an
// HTTP path segment. Both halves are asserted: the call fails, AND nothing was
// written outside the store's own directory. A Save that errored after creating
// the file would pass a check on the error alone.
func TestWIBindingStore_RefusesTraversalSID(t *testing.T) {
	base := t.TempDir()
	outside := filepath.Join(base, "outside")
	if err := os.MkdirAll(outside, 0o700); err != nil {
		t.Fatal(err)
	}
	s := NewWIBindingStore(filepath.Join(base, "sessions"))

	for _, sid := range []string{"../outside", "..", "a/../../outside", "sess/with/slash", ""} {
		if err := s.Save(sid, "tether#91"); err == nil {
			t.Errorf("Save(%q) = nil error, want refusal", sid)
		}
		if b, ok := s.Load(sid); ok {
			t.Errorf("Load(%q) = %+v, true; want refusal", sid, b)
		}
	}

	entries, err := os.ReadDir(outside)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("refused Saves left %d entries in %s, want 0", len(entries), outside)
	}
}

// TestWIBindingStore_RefusesUnstorableLabel — the label is opaque but not
// unbounded, and it must stay a single renderable line. Enforced in the store as
// well as at the HTTP edge, so a second caller cannot bypass the rule.
func TestWIBindingStore_RefusesUnstorableLabel(t *testing.T) {
	dir := t.TempDir()
	s := NewWIBindingStore(dir)

	bad := []struct {
		label  string
		reason string
	}{
		{"", "empty"},
		{strings.Repeat("x", MaxWorkItemLen+1), "over the length cap"},
		{"tether#91\nX-Injected: yes", "newline"},
		{"tether\x0091", "null byte"},
		// "Single line" has to mean single line in Unicode too: these break a line
		// in a browser exactly like \n does, and an ASCII-only control check waves
		// them through.
		{"tether#91\u2028X-Injected: yes", "U+2028 line separator"},
		{"tether#91\u2029second paragraph", "U+2029 paragraph separator"},
		{"tether#91\u0085next line", "C1 control (NEL)"},
		{"tether#91\xff", "invalid UTF-8"},
	}
	for _, c := range bad {
		if err := s.Save(wiSidFixture, c.label); err == nil {
			t.Errorf("Save(%q) = nil error, want refusal (%s)", c.label, c.reason)
		}
	}
	// And the refusals wrote nothing: a later Load still finds no binding.
	if b, ok := s.Load(wiSidFixture); ok {
		t.Errorf("Load after refused Saves = %+v, true; want zero, false", b)
	}

	// The boundary itself is storable — the cap rejects MaxWorkItemLen+1, not
	// MaxWorkItemLen.
	if err := s.Save(wiSidFixture, strings.Repeat("x", MaxWorkItemLen)); err != nil {
		t.Errorf("Save of a %d-char label: %v, want nil", MaxWorkItemLen, err)
	}
}

// TestWIBindingStore_ConcurrentSavesNeverTear — the writer is a browser, so two
// tabs (or a migration racing a Start click) can Save the same sid at once.
//
// The assertion is deliberately "a reader ALWAYS finds one of the whole labels",
// not merely "never finds a mixture". Once a binding exists, a concurrent rewrite
// must not make it momentarily VANISH either, and vanishing is the failure a
// non-atomic write actually produces: truncate-then-write leaves a window where
// the file is empty or half a JSON object, which Load reports as "no binding" —
// indistinguishable, to every caller, from a session nobody ever bound. Asserting
// only "no mixed value" would pass against that, because a half-written record
// does not parse into a plausible one. The record is therefore written once up
// front, so from then on absence is always wrong.
//
// Readers run concurrently with the writers so this is about what is observable
// DURING the race, not only after it.
func TestWIBindingStore_ConcurrentSavesNeverTear(t *testing.T) {
	dir := t.TempDir()
	s := NewWIBindingStore(dir)
	labels := []string{"tether#90", "tether#91", "tether#92", "aihub#7"}
	valid := map[string]bool{}
	for _, l := range labels {
		valid[l] = true
	}
	// A binding exists before any racing starts, so every later read must find
	// one. This is what gives the test its power against a non-atomic write.
	if err := s.Save(wiSidFixture, labels[0]); err != nil {
		t.Fatalf("seed Save: %v", err)
	}

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for n := 0; n < 25; n++ {
				if err := s.Save(wiSidFixture, labels[(i+n)%len(labels)]); err != nil {
					t.Errorf("Save: %v", err)
					return
				}
			}
		}(i)
	}
	var readErr sync.Once
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for n := 0; n < 100; n++ {
				b, ok := s.Load(wiSidFixture)
				if !ok {
					readErr.Do(func() { t.Errorf("binding vanished mid-rewrite: Load reported absent") })
					return
				}
				if !valid[b.WorkItem] {
					readErr.Do(func() { t.Errorf("torn read: WorkItem = %q", b.WorkItem) })
					return
				}
			}
		}()
	}
	wg.Wait()

	// No temp files left behind by the losers of the rename race.
	entries, err := os.ReadDir(filepath.Join(dir, wiSidFixture))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name() != "wi.json" {
			t.Errorf("leftover file in session dir: %s", e.Name())
		}
	}
}

// TestWIBindingStore_FilePermissions — the file sits in the user's home next to a
// transcript, and is written by an HTTP handler. 0600, like every other sidecar.
func TestWIBindingStore_FilePermissions(t *testing.T) {
	dir := t.TempDir()
	if err := NewWIBindingStore(dir).Save(wiSidFixture, "tether#91"); err != nil {
		t.Fatalf("Save: %v", err)
	}
	fi, err := os.Stat(filepath.Join(dir, wiSidFixture, "wi.json"))
	if err != nil {
		t.Fatal(err)
	}
	if got := fi.Mode().Perm(); got != 0o600 {
		t.Errorf("mode = %04o, want 0600", got)
	}
}

// TestWIBindingStore_OnDiskShape pins the JSON keys. The file is read by a
// restarted daemon and, in a pinch, by a human with `cat`; renaming a key is a
// silent data loss, not a compile error.
func TestWIBindingStore_OnDiskShape(t *testing.T) {
	dir := t.TempDir()
	if err := NewWIBindingStore(dir).Save(wiSidFixture, "tether#91"); err != nil {
		t.Fatalf("Save: %v", err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, wiSidFixture, "wi.json"))
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("unmarshal %q: %v", raw, err)
	}
	if m["workItem"] != "tether#91" {
		t.Errorf("workItem = %v, want tether#91 (raw=%s)", m["workItem"], raw)
	}
	if _, ok := m["boundAt"]; !ok {
		t.Errorf("missing boundAt key (raw=%s)", raw)
	}
}

// TestWIBindingStore_SharesTheSessionDirectory — wi.json lands beside
// workspace.json and history.jsonl rather than in a registry of its own. That
// co-location is what makes "a wi's sessions" answerable by reading the session
// directories, with no index to keep consistent.
func TestWIBindingStore_SharesTheSessionDirectory(t *testing.T) {
	dir := t.TempDir()
	if err := NewBindingStore(dir).Save(wiSidFixture, WorkspaceBinding{WorkspaceID: "ws-1", Path: "/w/tether"}); err != nil {
		t.Fatalf("workspace Save: %v", err)
	}
	if err := NewWIBindingStore(dir).Save(wiSidFixture, "tether#91"); err != nil {
		t.Fatalf("wi Save: %v", err)
	}

	// Both readable, neither clobbered by the other's write.
	ws, ok := NewBindingStore(dir).Load(wiSidFixture)
	if !ok || ws.Path != "/w/tether" {
		t.Errorf("workspace binding = %+v, %v; want /w/tether", ws, ok)
	}
	wi, ok := NewWIBindingStore(dir).Load(wiSidFixture)
	if !ok || wi.WorkItem != "tether#91" {
		t.Errorf("wi binding = %+v, %v; want tether#91", wi, ok)
	}
}
