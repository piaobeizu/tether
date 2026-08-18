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
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/piaobeizu/tether/internal/agent"
	"github.com/piaobeizu/tether/internal/wire"
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

// ---------------------------------------------------------------------------
// tether#101 — a resume cc REFUSED is not a resume that failed
// ---------------------------------------------------------------------------
//
// The three shapes of registry record, seen from Attachment.resolve rather than
// from the reader. CCRegistry's own tests pin the classification; these pin what
// resolve DOES with it, which is the part the user feels:
//
//	live + bg          → a Refusal, and NOTHING is spawned
//	dead + bg          → the pre-tether#101 fallback, unchanged
//	live + interactive → the pre-tether#101 fallback, unchanged
//
// The two "unchanged" rows are not filler. A misclassification there does not
// degrade the feature, it INVERTS it — a session the daemon refuses to open at all.
// The sizes are in ccregistry_test.go's three-shape test, measured and counted in
// sids rather than in records; what matters here is that both rows must reach the
// ordinary fallback, because one of them is the shape tether's OWN spawned agent
// registers as.

// ccRegDir builds a registry directory holding one record, and returns the
// directory. Kept separate from ccRegFixture (ccregistry_test.go) because these
// tests care about one record and about resolve's behaviour, not about the
// reader's edges.
func ccRegDir(t *testing.T, rec map[string]any) string {
	t.Helper()
	f := newCCRegFixture(t)
	f.write(t, rec)
	return f.dir
}

// TestResolve_ARefusedResumeIsReportedAndNothingIsSpawned is the payload of
// tether#101.
//
// Before it, this exact situation produced `chat resume failed, starting a fresh
// session` and a brand-new empty conversation — the user clicked a session and
// landed in nothing, with the real one alive and working the whole time. The
// assertion that matters most is therefore the SPAWN COUNT: a build that reports
// the refusal and then falls back anyway has fixed the log line and none of the
// damage.
func TestResolve_ARefusedResumeIsReportedAndNothingIsSpawned(t *testing.T) {
	requireLinux(t)
	dead := newDeadSession()
	// holdDeadStreamOpen, for the reason TestResolve_ReapsTheFailedResumeSubprocess
	// gives: with the stream open, fanOut cannot reach its own teardown reap, so a
	// Close() observed below came from the refusal branch and from nothing else.
	// Without it the count is legitimately 2 (this branch plus the teardown, which
	// is why agent.Session.Close is required to be idempotent) and the assertion
	// would no longer be about this change.
	dp := &deadThenLiveProvider{
		dead:               dead,
		live:               &fakeSession{sid: "fresh-sid", events: make(chan agent.Event, 8)},
		holdDeadStreamOpen: true,
	}
	reg := NewRegistry(dp)
	reg.History = NewHistoryStore(filepath.Join(t.TempDir(), "sessions"))
	reg.CCJobs = NewCCRegistry(ccRegDir(t, bgRecord(os.Getpid(), "held-sid-000001", liveToken(t))))

	att, err := reg.Attach(context.Background(), "held-sid-000001", "fake", "")
	if err != nil {
		t.Fatalf("Attach: %v", err)
	}
	// The user's prompt reaches the refused cc and fails, exactly as it does live:
	// cc wrote its refusal to stderr and exited without reading stdin.
	_ = att.SendPrompt(context.Background(), "carry on where we left off")

	res, err := att.Resolve(context.Background())
	if err == nil {
		t.Fatalf("Resolve returned no error; a refused resume must be reported, got %+v", res)
	}
	var ref *Refusal
	if !errors.As(err, &ref) {
		t.Fatalf("Resolve error is %T (%v), want a *Refusal — an unclassified error reaches the browser as retryable", err, err)
	}
	if ref.Code != wire.ErrCodeSessionHeldByBackgroundAgent {
		t.Errorf("Refusal.Code = %q, want %q", ref.Code, wire.ErrCodeSessionHeldByBackgroundAgent)
	}
	if !ref.Code.Terminal() {
		t.Error("the code is not Terminal; the browser's ladder would retry once a second against a refusal that cannot clear while the job runs")
	}
	// The whole point. One spawn is the resume attempt cc refused; a second would
	// be the empty conversation this change exists to stop handing over.
	if got := dp.Spawns(); got != 1 {
		t.Errorf("spawns = %d, want 1 (the refused resume, and NO fallback) — a second spawn is the silent empty session", got)
	}
	if res != (Resolution{}) {
		t.Errorf("Resolution = %+v, want the zero value; there is no session to report", res)
	}
	// The message has to name the holder, because "which job has my conversation"
	// is the actionable half and cc's own error names it too.
	for _, want := range []string{"held-sid-000001", "bg", "job-held-sid-000001"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("Refusal message %q does not name %q", err.Error(), want)
		}
	}
	// Cleanup still happens: the refused subprocess is reaped and un-registered,
	// or the daemon accumulates a zombie per refusal and the dead sid keeps
	// reading as live to the next reconnect.
	<-dead.emitted
	if got := dead.Closes(); got != 1 {
		t.Errorf("refused session Close() calls = %d, want 1; without the reap the cc subprocess stays a zombie for the daemon's lifetime", got)
	}
	// The registration itself, not liveEntry: liveEntry answers false for a corpse
	// whether or not anyone evicted it, so asserting through it would pass with the
	// evict deleted. Reading the map is what makes this about the evict.
	reg.mu.RLock()
	_, stillRegistered := reg.sessions["held-sid-000001"]
	reg.mu.RUnlock()
	if stillRegistered {
		t.Error("the refused resume is still registered under its sid; the corpse would be handed to the next reconnect")
	}
	// WaitSID must not hang: a prompt-recording goroutine parked in it would leak
	// for the daemon's lifetime, and "" is the documented no-op for recording.
	if sid := att.WaitSID(); sid != "" {
		t.Errorf("WaitSID = %q, want \"\"", sid)
	}
}

// TestResolve_ADeadRecordStillFallsBack — the residue case, and the residue is
// permanent rather than transient: cc's sweep of stale records is switched off in
// tether's environment (see CCRegistry's file doc for the gate), so records outlive
// their processes indefinitely. 132 of the 138 on the reference profile already
// did, the oldest 3.2 days old. cc lets those resumes through, so tether must too,
// and must keep doing so as the pile grows — which is the half of this that no
// snapshot count captures.
func TestResolve_ADeadRecordStillFallsBack(t *testing.T) {
	requireLinux(t)
	dp := &deadThenLiveProvider{
		dead: newDeadSession(),
		live: &fakeSession{sid: "fresh-sid", events: make(chan agent.Event, 8)},
	}
	reg := NewRegistry(dp)
	reg.History = NewHistoryStore(filepath.Join(t.TempDir(), "sessions"))
	reg.CCJobs = NewCCRegistry(ccRegDir(t, bgRecord(deadPid(t), "stale-sid-00001", "1")))

	att, err := reg.Attach(context.Background(), "stale-sid-00001", "fake", "")
	if err != nil {
		t.Fatalf("Attach: %v", err)
	}
	_ = att.SendPrompt(context.Background(), "hello again")

	res, err := att.Resolve(context.Background())
	if err != nil {
		t.Fatalf("Resolve: %v — a stale registry record must not refuse a resume", err)
	}
	if res.SID != "fresh-sid" {
		t.Errorf("Resolution.SID = %q, want \"fresh-sid\" (the fallback)", res.SID)
	}
	if !res.Recovered {
		t.Error("Recovered = false; the fallback still has to tell the browser its sid moved")
	}
	if got := dp.Spawns(); got != 2 {
		t.Errorf("spawns = %d, want 2 (the resume attempt, then the fallback)", got)
	}
}

// TestResolve_ALiveInteractiveRecordStillFallsBack — the anti-mislabel case with
// the sharpest consequence, and it is about TETHER'S OWN SESSIONS.
//
// tether spawns cc with --print --output-format stream-json --input-format
// stream-json --verbose, and cc registers that as kind "interactive" (measured,
// 2.1.233, 2026-08-18: 124 of 124 sdk-cli records were interactive). A build that
// classified interactive records as holders would refuse to resume any session
// tether had started and not yet torn down — i.e. it would break the daemon's
// daily path in the name of fixing an edge of it.
func TestResolve_ALiveInteractiveRecordStillFallsBack(t *testing.T) {
	requireLinux(t)
	dp := &deadThenLiveProvider{
		dead: newDeadSession(),
		live: &fakeSession{sid: "fresh-sid", events: make(chan agent.Event, 8)},
	}
	reg := NewRegistry(dp)
	reg.History = NewHistoryStore(filepath.Join(t.TempDir(), "sessions"))
	reg.CCJobs = NewCCRegistry(ccRegDir(t, interactiveRecord(os.Getpid(), "tether-own-000002", liveToken(t))))

	att, err := reg.Attach(context.Background(), "tether-own-000002", "fake", "")
	if err != nil {
		t.Fatalf("Attach: %v", err)
	}
	_ = att.SendPrompt(context.Background(), "still here?")

	res, err := att.Resolve(context.Background())
	if err != nil {
		t.Fatalf("Resolve: %v — a live INTERACTIVE record must not refuse a resume; that is the shape tether's own cc registers as", err)
	}
	if res.SID != "fresh-sid" {
		t.Errorf("Resolution.SID = %q, want \"fresh-sid\"", res.SID)
	}
	if got := dp.Spawns(); got != 2 {
		t.Errorf("spawns = %d, want 2", got)
	}
}

// TestResolve_NoRegistryReaderBehavesExactlyAsBefore — a daemon with no CCJobs
// (or one whose registry directory does not exist) must take the pre-tether#101
// path. This is the degradation the reader is designed for: it fails towards "not
// held", so a daemon that cannot see cc's registry keeps working instead of
// refusing everything.
func TestResolve_NoRegistryReaderBehavesExactlyAsBefore(t *testing.T) {
	for _, tc := range []struct {
		name  string
		build func(t *testing.T) *CCRegistry
	}{
		{"nil reader", func(*testing.T) *CCRegistry { return nil }},
		{"missing directory", func(t *testing.T) *CCRegistry {
			return NewCCRegistry(filepath.Join(t.TempDir(), "gone", "sessions"))
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dp := &deadThenLiveProvider{
				dead: newDeadSession(),
				live: &fakeSession{sid: "fresh-sid", events: make(chan agent.Event, 8)},
			}
			reg := NewRegistry(dp)
			reg.History = NewHistoryStore(filepath.Join(t.TempDir(), "sessions"))
			reg.CCJobs = tc.build(t)

			att, err := reg.Attach(context.Background(), "any-old-sid-01", "fake", "")
			if err != nil {
				t.Fatalf("Attach: %v", err)
			}
			_ = att.SendPrompt(context.Background(), "hello")
			res, err := att.Resolve(context.Background())
			if err != nil {
				t.Fatalf("Resolve: %v", err)
			}
			if res.SID != "fresh-sid" || !res.Recovered {
				t.Errorf("Resolution = %+v, want the fallback (SID \"fresh-sid\", Recovered true)", res)
			}
			if got := dp.Spawns(); got != 2 {
				t.Errorf("spawns = %d, want 2", got)
			}
		})
	}
}

// TestResolve_ARefusalDoesNotRetireTheSidsObservers pins a decision that is
// invisible until it is wrong.
//
// The fallback path retires the read-only observers of the old sid, because a
// fresh sid replaces it and nothing will ever be registered under the old one by
// that attachment. A REFUSAL is the opposite situation: the sid is not replaced,
// it is left alone because something else is using it, and a later attach may
// resume it once the job finishes. Retiring observers there would tell them the
// session is finished, which is the one thing this branch knows to be false.
func TestResolve_ARefusalDoesNotRetireTheSidsObservers(t *testing.T) {
	requireLinux(t)
	dp := &deadThenLiveProvider{
		dead: newDeadSession(),
		live: &fakeSession{sid: "fresh-sid", events: make(chan agent.Event, 8)},
	}
	reg := NewRegistry(dp)
	reg.History = NewHistoryStore(filepath.Join(t.TempDir(), "sessions"))
	reg.CCJobs = NewCCRegistry(ccRegDir(t, bgRecord(os.Getpid(), "held-sid-000002", liveToken(t))))

	obs := make(chan wire.Envelope, 4)
	retired := reg.SubscribeObserver("held-sid-000002", obs)
	defer reg.UnsubscribeObserver("held-sid-000002", obs)

	att, err := reg.Attach(context.Background(), "held-sid-000002", "fake", "")
	if err != nil {
		t.Fatalf("Attach: %v", err)
	}
	_ = att.SendPrompt(context.Background(), "hello")
	if _, err := att.Resolve(context.Background()); err == nil {
		t.Fatal("Resolve returned no error for a held sid")
	}

	select {
	case <-retired:
		t.Error("the sid's observers were retired by a REFUSAL; nothing has replaced that sid, and a later attach may still resume it")
	default:
	}
}
