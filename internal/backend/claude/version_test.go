package claude

import (
	"context"
	"fmt"
	"testing"
	"time"
)

// makeSystemInit synthesizes a system/init Envelope with the given session_id
// and claude_code_version, going through the real ParseLine pipeline so the
// test exercises the same decode path as production.
func makeSystemInit(t *testing.T, sid, ver string) Envelope {
	t.Helper()
	raw := []byte(fmt.Sprintf(
		`{"type":"system","subtype":"init","session_id":%q,"uuid":"u-1","model":"claude-haiku","claude_code_version":%q}`,
		sid, ver,
	))
	env, err := ParseLine(raw)
	if err != nil {
		t.Fatalf("ParseLine(system/init): %v", err)
	}
	return env
}

// newSessionStub spins a Session backed by /bin/cat (dispatcher will not see
// system/init from cat — we drive recordSystemInit() directly). Caller MUST
// defer sess.Close().
func newSessionStub(t *testing.T) *Session {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	sess, err := New(ctx, SpawnOpts{BinaryPath: "/bin/cat"}, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	go drainEvents(sess)
	return sess
}

// First system/init records claude_code_version + signals initCh.
func TestSession_VersionDrift_FirstInitRecords(t *testing.T) {
	sess := newSessionStub(t)
	defer sess.Close()

	signaled := sess.recordSystemInit(makeSystemInit(t, "sid-1", "2.1.123"))
	if !signaled {
		t.Errorf("first recordSystemInit should signal initCh (returned false)")
	}
	if got := sess.ClaudeCodeVersion(); got != "2.1.123" {
		t.Errorf("ClaudeCodeVersion: want %q, got %q", "2.1.123", got)
	}
	if drifts := sess.VersionDrifts(); len(drifts) != 0 {
		t.Errorf("expected no drifts after first init, got %d: %+v", len(drifts), drifts)
	}
	if got := sess.SessionID(); got != "sid-1" {
		t.Errorf("SessionID: want %q, got %q", "sid-1", got)
	}
}

// Second system/init with the SAME version (e.g., post-Recover happy path)
// must not record drift. SessionID is allowed to update (resume can mint a
// new server-side session_id per spec §7.7).
func TestSession_VersionDrift_SameVersionNoDrift(t *testing.T) {
	sess := newSessionStub(t)
	defer sess.Close()

	sess.recordSystemInit(makeSystemInit(t, "sid-1", "2.1.123"))
	signaled := sess.recordSystemInit(makeSystemInit(t, "sid-1-resumed", "2.1.123"))
	if signaled {
		t.Errorf("second recordSystemInit should NOT signal initCh again")
	}
	if got := sess.ClaudeCodeVersion(); got != "2.1.123" {
		t.Errorf("recorded version should remain unchanged, got %q", got)
	}
	if drifts := sess.VersionDrifts(); len(drifts) != 0 {
		t.Errorf("expected 0 drifts after same-version reinit, got %d: %+v", len(drifts), drifts)
	}
	// SessionID allowed to update on resume.
	if got := sess.SessionID(); got != "sid-1-resumed" {
		t.Errorf("SessionID should update to new value, got %q", got)
	}
}

// Drift case: claude binary upgraded mid-session, second init reports a
// different claude_code_version. Drift recorded; recordedClaudeVersion stays
// at the FIRST version (so the daemon can decide policy based on the original).
func TestSession_VersionDrift_DifferentVersionRecorded(t *testing.T) {
	sess := newSessionStub(t)
	defer sess.Close()

	sess.recordSystemInit(makeSystemInit(t, "sid-1", "2.1.123"))
	sess.recordSystemInit(makeSystemInit(t, "sid-1-resumed", "2.1.124"))

	if got := sess.ClaudeCodeVersion(); got != "2.1.123" {
		t.Errorf("recordedClaudeVersion must NOT update on drift, got %q (expected first-wins %q)", got, "2.1.123")
	}
	drifts := sess.VersionDrifts()
	if len(drifts) != 1 {
		t.Fatalf("expected exactly 1 drift, got %d: %+v", len(drifts), drifts)
	}
	if drifts[0].From != "2.1.123" || drifts[0].To != "2.1.124" {
		t.Errorf("drift From/To: want (%q→%q), got (%q→%q)", "2.1.123", "2.1.124", drifts[0].From, drifts[0].To)
	}
	if drifts[0].At.IsZero() {
		t.Errorf("drift.At should be non-zero wall clock")
	}
}

// Multiple successive drifts each get recorded; recordedClaudeVersion stays
// at the original.
func TestSession_VersionDrift_MultipleDriftsAccumulate(t *testing.T) {
	sess := newSessionStub(t)
	defer sess.Close()

	sess.recordSystemInit(makeSystemInit(t, "sid-1", "2.1.123"))
	sess.recordSystemInit(makeSystemInit(t, "sid-2", "2.1.124"))
	sess.recordSystemInit(makeSystemInit(t, "sid-3", "2.1.125"))

	if got := sess.ClaudeCodeVersion(); got != "2.1.123" {
		t.Errorf("recordedClaudeVersion: want %q, got %q", "2.1.123", got)
	}
	drifts := sess.VersionDrifts()
	if len(drifts) != 2 {
		t.Fatalf("expected 2 drifts (2 mismatches), got %d", len(drifts))
	}
	want := []struct{ from, to string }{{"2.1.123", "2.1.124"}, {"2.1.123", "2.1.125"}}
	for i, d := range drifts {
		if d.From != want[i].from || d.To != want[i].to {
			t.Errorf("drifts[%d]: want %q→%q, got %q→%q", i, want[i].from, want[i].to, d.From, d.To)
		}
	}
}

// First init has empty claude_code_version (defensive); second init has a
// real one. recordedClaudeVersion adopts the first NON-EMPTY observation and
// no drift is recorded.
func TestSession_VersionDrift_EmptyFirstInitDoesNotCount(t *testing.T) {
	sess := newSessionStub(t)
	defer sess.Close()

	sess.recordSystemInit(makeSystemInit(t, "sid-1", "")) // claude omitted version
	if got := sess.ClaudeCodeVersion(); got != "" {
		t.Errorf("recorded should be empty after empty-version init, got %q", got)
	}
	sess.recordSystemInit(makeSystemInit(t, "sid-2", "2.1.123"))

	if got := sess.ClaudeCodeVersion(); got != "2.1.123" {
		t.Errorf("recorded should adopt first non-empty version, got %q", got)
	}
	if drifts := sess.VersionDrifts(); len(drifts) != 0 {
		t.Errorf("empty→non-empty is NOT drift, got %d: %+v", len(drifts), drifts)
	}
}

// Drifts() returns a copy — caller mutation must not affect Session state.
func TestSession_VersionDrift_DriftsReturnsCopy(t *testing.T) {
	sess := newSessionStub(t)
	defer sess.Close()

	sess.recordSystemInit(makeSystemInit(t, "sid-1", "2.1.123"))
	sess.recordSystemInit(makeSystemInit(t, "sid-2", "2.1.124"))

	d1 := sess.VersionDrifts()
	if len(d1) != 1 {
		t.Fatalf("expected 1 drift, got %d", len(d1))
	}
	d1[0].From = "tampered"

	d2 := sess.VersionDrifts()
	if d2[0].From != "2.1.123" {
		t.Errorf("Session.VersionDrifts() must return a copy; caller mutation leaked: %q", d2[0].From)
	}
}
