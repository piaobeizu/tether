package session

// tether#92 — the notice gate, after it learned about cc's store.
//
// This is the guard for the one finding both deep reviewers returned
// independently, from opposite ends of the stack: a session tether never
// recorded could fail its `--resume`, fall back to a brand-new empty
// conversation, and say NOTHING — because the gate asked only whether tether had
// a transcript, and having none is exactly what made such a session a cc row.
//
// The population is not marginal. cc resumes are cwd-scoped, and a cc session's
// directory is whatever the user was standing in; the daemon resumes an unbound
// sid in its own --workspace-root (see resolveWorkspace row 2). Every cc session
// from any other directory therefore fails deterministically, and before this
// change every one of them failed silently.

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/piaobeizu/tether/internal/agent"
)

// TestHadConversation — the predicate on its own, both stores, all four states.
func TestHadConversation(t *testing.T) {
	f := newCCFixture(t, "/w")
	f.write(t, "/w", "cc-only-00000001.jsonl", ccUser(t, "typed in a terminal"))

	h := NewHistoryStore(filepath.Join(t.TempDir(), "sessions"))
	h.RecordUser("tether-only-0001", "typed in the browser")

	reg := &Registry{History: h, CC: f.store()}

	for _, tc := range []struct {
		name string
		sid  string
		want bool
	}{
		{"tether recorded it", "tether-only-0001", true},
		// The case the whole change is about. Before tether#92 this was false, and
		// a resume failure for it was silent.
		{"cc recorded it, tether never saw it", "cc-only-00000001", true},
		{"nobody has it", "never-existed-01", false},
		{"empty sid", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := reg.hadConversation(tc.sid); got != tc.want {
				t.Errorf("hadConversation(%q) = %v, want %v", tc.sid, got, tc.want)
			}
		})
	}

	// A daemon assembled without either store cannot know, and must stay quiet
	// rather than guess — the same rule the single-store version had.
	if (&Registry{}).hadConversation("cc-only-00000001") {
		t.Error("a Registry with no stores claimed a conversation existed")
	}
	var nilReg *Registry
	if nilReg.hadConversation("cc-only-00000001") {
		t.Error("nil Registry claimed a conversation existed")
	}
	// Each store alone is enough; neither is required.
	if !(&Registry{CC: f.store()}).hadConversation("cc-only-00000001") {
		t.Error("cc alone did not answer")
	}
	if !(&Registry{History: h}).hadConversation("tether-only-0001") {
		t.Error("tether alone did not answer")
	}
}

// TestResolve_AFailedResumeOfACCSessionIsNotSilent drives the real fallback path
// and asserts the user is told.
//
// The A/B is the pair of subtests: with the cc store wired the notice fires, and
// with the SAME fixture but no cc store it does not — which is precisely the
// behaviour before this change, so the test would have failed against it.
func TestResolve_AFailedResumeOfACCSessionIsNotSilent(t *testing.T) {
	const sid = "cc-gone-00000001"

	for _, tc := range []struct {
		name       string
		wireCC     bool
		wantNotice bool
	}{
		{name: "cc store wired → the user is told", wireCC: true, wantNotice: true},
		{name: "no cc store → the pre-tether#92 silence", wireCC: false, wantNotice: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newCCFixture(t, "/w")
			f.write(t, "/w", sid+".jsonl", ccUser(t, "a real conversation, recorded elsewhere"))

			dp := &deadThenLiveProvider{
				dead: newDeadSession(),
				live: &fakeSession{sid: "recovered-sid", events: make(chan agent.Event, 8)},
			}
			reg := NewRegistry(dp)
			// tether has a store, and it is EMPTY for this sid — which is the whole
			// point. The old gate asked only this one and therefore always said no.
			reg.History = NewHistoryStore(filepath.Join(t.TempDir(), "sessions"))
			if tc.wireCC {
				reg.CC = f.store()
			}

			att, err := reg.Attach(context.Background(), sid, "fake", "")
			if err != nil {
				t.Fatalf("Attach: %v", err)
			}
			// A resume reports failure only AFTER a prompt has been delivered, so the
			// prompt is not decoration — without it this path is not reached at all.
			_ = att.SendPrompt(context.Background(), "are you still there")
			res, err := att.Resolve(context.Background())
			if err != nil {
				t.Fatalf("Resolve: %v", err)
			}
			if !res.Recovered {
				t.Fatal("Recovered = false; this case falls back to a fresh session")
			}
			if res.Notice != tc.wantNotice {
				t.Errorf("Notice = %v, want %v — a conversation was replaced and the user %s told",
					res.Notice, tc.wantNotice, map[bool]string{true: "must be", false: "is not"}[tc.wantNotice])
			}
		})
	}
}

// TestResolve_StillSilentForASessionNobodyRecorded — the widened gate must not
// become "always notify". A connection that reloaded before saying anything has
// no transcript in EITHER store, and telling those users they lost a
// conversation they never had is the crying-wolf failure the gate exists to
// prevent.
func TestResolve_StillSilentForASessionNobodyRecorded(t *testing.T) {
	f := newCCFixture(t, "/w")
	f.write(t, "/w", "some-other-0001.jsonl", ccUser(t, "a different session entirely"))

	dp := &deadThenLiveProvider{
		dead: newDeadSession(),
		live: &fakeSession{sid: "recovered-sid", events: make(chan agent.Event, 8)},
	}
	reg := NewRegistry(dp)
	reg.History = NewHistoryStore(filepath.Join(t.TempDir(), "sessions"))
	reg.CC = f.store()

	att, err := reg.Attach(context.Background(), "never-said-0001", "fake", "")
	if err != nil {
		t.Fatalf("Attach: %v", err)
	}
	_ = att.SendPrompt(context.Background(), "hello")
	res, err := att.Resolve(context.Background())
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if res.Notice {
		t.Error("Notice = true for a session neither store has ever recorded")
	}
}
