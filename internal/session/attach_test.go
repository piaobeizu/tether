package session

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/piaobeizu/tether/internal/agent"
	"github.com/piaobeizu/tether/internal/wire"
)

// registry_test.go's fakeProvider returns one fixed session for every spawn,
// which cannot express the shape tether#50 is about: a FIRST session that dies
// before init (the failed resume) followed by a SECOND that lives (the fallback).
// deadThenLiveProvider and deadOnlyProvider below are the two doubles that can.

// deadSession models cc dying before system/init — the measured failed-`--resume`
// shape (mem_2ruSlrHR ③): SessionID() returns "" (tether#49's done channel), and
// the only thing on the wire is one empty result. SendPrompt fails like the broken
// pipe a real dead cc produces, so the fallback is exercised against the real
// failure mode rather than a silently-succeeding write.
type deadSession struct {
	events chan agent.Event
	// emitted is closed once dying() has finished putting the terminal event on
	// the stream, so endStream can close the channel without racing that send.
	emitted chan struct{}
	dead    atomic.Bool
	mu      sync.Mutex
	sends   int
	closes  int
}

func newDeadSession() *deadSession {
	return &deadSession{events: make(chan agent.Event, 4), emitted: make(chan struct{})}
}

// die emits the single empty EventResult a failed resume produces and closes the
// stream, exactly as ccSession.readLoop does when the process EOFs.
func (d *deadSession) die() {
	d.dying()
	d.endStream()
}

// dying is the first half of die(): the process is gone and every liveness signal
// says so, but the event stream has not closed yet. Real cc has exactly this gap —
// readLoop closes `done` before it closes `events` — and holding a test inside it
// is what makes a Close() attributable. fanOut cannot reach its teardown reap
// (tether#56) while the stream is open, so a Close() observed here came from
// Attachment.resolve's eager reap (tether#50) and from nothing else.
func (d *deadSession) dying() {
	d.events <- agent.Event{Kind: agent.EventResult, Text: ""}
	d.dead.Store(true)
	close(d.emitted)
}

// endStream closes the event stream, releasing whatever is ranging over it — i.e.
// letting the ordinary teardown run. It waits for dying() to have finished
// emitting first, because dying() runs on its own goroutine and closing a channel
// out from under a send is a panic, not merely a race.
func (d *deadSession) endStream() {
	<-d.emitted
	close(d.events)
}

func (d *deadSession) SessionID() string { return "" }

// Alive follows die(), matching ccSession: the process-exit signal lands before
// the event stream close is observable downstream.
func (d *deadSession) Alive() bool { return !d.dead.Load() }
func (d *deadSession) SendPrompt(_ context.Context, _ string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.sends++
	return errBrokenPipe
}
func (d *deadSession) Events() <-chan agent.Event { return d.events }
func (d *deadSession) Interrupt() error           { return nil }
func (d *deadSession) Close() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.closes++
	return nil
}
func (d *deadSession) Sends() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.sends
}
func (d *deadSession) Closes() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.closes
}

type brokenPipe struct{}

func (brokenPipe) Error() string { return "write |1: broken pipe" }

var errBrokenPipe = brokenPipe{}

// deadProvider spawns a deadSession first and a live fakeSession second — the
// canonical tether#50 sequence.
type deadThenLiveProvider struct {
	dead *deadSession
	live *fakeSession
	// holdDeadStreamOpen leaves the dead session's event stream open, parking its
	// fanOut short of the teardown reap so a test can attribute a Close() to
	// Attachment.resolve alone — see deadSession.dying. Set it only when that
	// attribution is the point; every other test wants the ordinary full death.
	holdDeadStreamOpen bool

	mu     sync.Mutex
	cfgs   []agent.SpawnConfig
	spawns int
}

func (p *deadThenLiveProvider) Name() string { return "fake" }

func (p *deadThenLiveProvider) Spawn(_ context.Context, cfg agent.SpawnConfig) (agent.Session, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.cfgs = append(p.cfgs, cfg)
	p.spawns++
	if p.spawns == 1 {
		// The resume attempt: hand back a cc that is already on its way out.
		if p.holdDeadStreamOpen {
			go p.dead.dying()
		} else {
			go p.dead.die()
		}
		return p.dead, nil
	}
	return p.live, nil
}

func (p *deadThenLiveProvider) Configs() []agent.SpawnConfig {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]agent.SpawnConfig(nil), p.cfgs...)
}

func (p *deadThenLiveProvider) Spawns() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.spawns
}

// ─── Attach: which spawn shape each reconnect state produces ────────────────

// TestAttach_NoSidMintsFreshSessionID — a client with no sid gets a fresh spawn
// whose id the DAEMON minted and pinned, not one cc invented. That pinning is
// what makes the session resumable on the next reconnect at all (mem_2ruSlrHR ①);
// before tether#50 the id was only ever learned from init, so nothing on disk was
// addressable until cc said so.
func TestAttach_NoSidMintsFreshSessionID(t *testing.T) {
	fp := &fakeProvider{sess: &fakeSession{sid: "minted-sid", events: make(chan agent.Event, 8)}}
	reg := NewRegistry(fp)

	if _, err := reg.Attach(context.Background(), "", "fake", ""); err != nil {
		t.Fatalf("Attach: %v", err)
	}
	if fp.lastCfg.ResumeSessionID != "" {
		t.Errorf("ResumeSessionID = %q, want \"\" on a no-sid attach", fp.lastCfg.ResumeSessionID)
	}
	if fp.lastCfg.SessionID == "" {
		t.Fatal("SessionID = \"\"; a fresh spawn must pin a minted uuid (tether#50)")
	}
	if len(fp.lastCfg.SessionID) != 36 {
		t.Errorf("SessionID = %q, want a 36-char uuid", fp.lastCfg.SessionID)
	}
}

// TestAttach_DeadSidAttemptsResume — the behavioural inversion of tether#49. A
// sid the registry no longer tracks must now be handed to `cc --resume`, WITHOUT
// a --session-id beside it (the flags are mutually exclusive, mem_2ruSlrHR ⑧;
// agent.ClaudeCodeProvider.Spawn rejects the pair outright, so producing it here
// would fail the spawn instead of resuming).
func TestAttach_DeadSidAttemptsResume(t *testing.T) {
	fp := &fakeProvider{sess: &fakeSession{sid: "dead-sid", events: make(chan agent.Event, 8)}}
	reg := NewRegistry(fp)

	if _, err := reg.Attach(context.Background(), "dead-sid", "fake", ""); err != nil {
		t.Fatalf("Attach: %v", err)
	}
	if fp.lastCfg.ResumeSessionID != "dead-sid" {
		t.Errorf("ResumeSessionID = %q, want \"dead-sid\" (tether#50 restores context by resuming)", fp.lastCfg.ResumeSessionID)
	}
	if fp.lastCfg.SessionID != "" {
		t.Errorf("SessionID = %q passed alongside --resume; the two are mutually exclusive", fp.lastCfg.SessionID)
	}
}

// TestAttach_LiveSidReusesWithoutSpawning — a sid that IS live is reused, with no
// spawn and no resume: cc is still running and still holds the context in memory.
// Resuming a live session would point a second cc at a transcript the first one
// owns.
func TestAttach_LiveSidReusesWithoutSpawning(t *testing.T) {
	fp := &fakeProvider{sess: &fakeSession{sid: "live-sid", events: make(chan agent.Event, 8)}}
	reg := NewRegistry(fp)

	if _, err := reg.GetOrSpawnEntry(context.Background(), "", "fake"); err != nil {
		t.Fatalf("seed spawn: %v", err)
	}
	fp.sess.announceInit()
	waitForRegistered(t, reg, "live-sid")
	spawnsBefore := fp.spawns

	att, err := reg.Attach(context.Background(), "live-sid", "fake", "")
	if err != nil {
		t.Fatalf("Attach: %v", err)
	}
	if fp.spawns != spawnsBefore {
		t.Errorf("spawns = %d, want %d: a live sid must be reused, not respawned", fp.spawns, spawnsBefore)
	}
	res, err := att.Resolve(context.Background())
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if res.SID != "live-sid" {
		t.Errorf("Resolution.SID = %q, want \"live-sid\"", res.SID)
	}
	if res.Recovered {
		t.Error("Recovered = true for a live-session reuse; nothing was lost")
	}
}

// ─── Attach: registered is not alive (tether#55) ─────────────────────────────

// corpseThenLiveProvider hands back a session that will be left for dead first
// and a healthy one second. It is not deadThenLiveProvider: that one models a cc
// that never emitted init (SessionID() == ""), which is tether#50's failed
// resume. tether#55 is the OPPOSITE shape — a session that DID init, so its sid
// is cached and non-empty forever, and only Alive() can tell it is gone.
type corpseThenLiveProvider struct {
	corpse *fakeSession
	live   *fakeSession

	mu     sync.Mutex
	cfgs   []agent.SpawnConfig
	spawns int
}

func (p *corpseThenLiveProvider) Name() string { return "fake" }

func (p *corpseThenLiveProvider) Spawn(_ context.Context, cfg agent.SpawnConfig) (agent.Session, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.cfgs = append(p.cfgs, cfg)
	p.spawns++
	if p.spawns == 1 {
		return p.corpse, nil
	}
	return p.live, nil
}

func (p *corpseThenLiveProvider) Configs() []agent.SpawnConfig {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]agent.SpawnConfig(nil), p.cfgs...)
}

func (p *corpseThenLiveProvider) Spawns() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.spawns
}

// registeredEntry returns the Entry currently registered under sid, or nil.
func registeredEntry(reg *Registry, sid string) *Entry {
	reg.mu.RLock()
	defer reg.mu.RUnlock()
	return reg.sessions[sid]
}

// stillRegistered reports whether target is reachable from r.sessions under ANY
// key — the by-value question Registry.evict answers, so a test can tell
// "unregistered" from "re-keyed".
func stillRegistered(reg *Registry, target *Entry) bool {
	reg.mu.RLock()
	defer reg.mu.RUnlock()
	for _, e := range reg.sessions {
		if e == target {
			return true
		}
	}
	return false
}

// TestAttach_RegisteredButDeadSessionIsNotReused is tether#55.
//
// The reuse branch used to ask only "is sid a key in r.sessions". An entry stays
// in that map from the moment its cc exits until fanOut's deferred evict has
// drained the event stream, and inside that window every signal reads healthy:
// SessionID() hands back the id it cached at init, so Resolve confirms the
// session, session_ready goes out, ownership is claimed — and then every prompt
// dies in a broken pipe that never reaches the browser. "Thinking…" forever.
//
// The assertions below are the whole user-visible chain, in order: the corpse is
// not adopted, the reconnect becomes a `--resume` of the same transcript (so the
// conversation's context is recovered rather than dropped), the prompt lands on a
// session that can answer it, and the corpse is unregistered rather than left to
// catch the next reconnect too.
func TestAttach_RegisteredButDeadSessionIsNotReused(t *testing.T) {
	corpse := &fakeSession{sid: "corpse-sid", events: make(chan agent.Event, 8)}
	live := &fakeSession{sid: "corpse-sid", events: make(chan agent.Event, 8)}
	cp := &corpseThenLiveProvider{corpse: corpse, live: live}
	reg := NewRegistry(cp)

	// Seed a normal session, then let its agent exit while the entry lingers.
	if _, err := reg.GetOrSpawnEntry(context.Background(), "", "fake"); err != nil {
		t.Fatalf("seed spawn: %v", err)
	}
	corpse.announceInit()
	waitForRegistered(t, reg, "corpse-sid")
	corpseEntry := registeredEntry(reg, "corpse-sid")
	if corpseEntry == nil {
		t.Fatal("seed session not registered under its sid")
	}
	corpse.dead.Store(true)

	att, err := reg.Attach(context.Background(), "corpse-sid", "fake", "")
	if err != nil {
		t.Fatalf("Attach: %v", err)
	}

	if got := cp.Spawns(); got != 2 {
		t.Fatalf("spawns = %d, want 2: a registered-but-dead sid must NOT be reused", got)
	}
	if cfgs := cp.Configs(); cfgs[1].ResumeSessionID != "corpse-sid" {
		t.Errorf("second spawn ResumeSessionID = %q, want %q — the transcript is on disk, "+
			"so the dead session's context should be resumed, not discarded",
			cfgs[1].ResumeSessionID, "corpse-sid")
	}

	// The property the user actually feels: the prompt reaches something alive.
	if err := att.SendPrompt(context.Background(), "who am I?"); err != nil {
		t.Fatalf("SendPrompt: %v", err)
	}
	if got := corpse.Prompts(); len(got) != 0 {
		t.Errorf("corpse received %v; a dead session must never be prompted", got)
	}
	if got := live.Prompts(); len(got) != 1 || got[0] != "who am I?" {
		t.Errorf("live session prompts = %v, want [\"who am I?\"]", got)
	}

	res, err := att.Resolve(context.Background())
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if res.SID != "corpse-sid" {
		t.Errorf("Resolution.SID = %q, want \"corpse-sid\" (a successful resume does not drift the id)", res.SID)
	}
	if res.Recovered {
		t.Error("Recovered = true, want false: the resume SUCCEEDED, no context was lost")
	}

	if stillRegistered(reg, corpseEntry) {
		t.Error("the dead entry is still in r.sessions; the next reconnect would find it too")
	}
}

// TestAttach_LiveSessionStillReusedAfterLivenessCheck — the other side of the
// tether#55 gate, and the regression that would matter most if it broke: a
// HEALTHY registered session must still be reused, with no second spawn. Calling
// a live session dead would respawn cc on every reconnect and point a second
// process at a transcript the first one owns — which is worse than the bug being
// fixed, and is what tether#49's always-fresh behaviour cost before tether#50.
func TestAttach_LiveSessionStillReusedAfterLivenessCheck(t *testing.T) {
	live := &fakeSession{sid: "healthy-sid", events: make(chan agent.Event, 8)}
	cp := &corpseThenLiveProvider{corpse: live, live: live}
	reg := NewRegistry(cp)

	if _, err := reg.GetOrSpawnEntry(context.Background(), "", "fake"); err != nil {
		t.Fatalf("seed spawn: %v", err)
	}
	live.announceInit()
	waitForRegistered(t, reg, "healthy-sid")
	seeded := registeredEntry(reg, "healthy-sid")

	att, err := reg.Attach(context.Background(), "healthy-sid", "fake", "")
	if err != nil {
		t.Fatalf("Attach: %v", err)
	}
	if got := cp.Spawns(); got != 1 {
		t.Errorf("spawns = %d, want 1: a live sid must be reused, never respawned", got)
	}
	att.mu.Lock()
	bound := att.entry
	att.mu.Unlock()
	if bound != seeded {
		t.Error("Attach bound a different Entry for a live sid; the reuse path must return the existing one")
	}
	if !stillRegistered(reg, seeded) {
		t.Error("the live entry was evicted; only a dead one may be dropped")
	}
}

// ─── Resolve: the fallback ──────────────────────────────────────────────────

// TestResolve_FailedResumeFallsBackAndReplays is the heart of tether#50.
//
// Sequence: the client reconnects with a sid whose transcript is gone → the
// resume attempt spawns a cc that exits before system/init → Resolve must notice
// (SessionID() == ""), spawn a FRESH session under a newly minted id, and replay
// the prompt the user already sent, which the dead cc never read.
//
// Without the replay the user's message is answered by nobody: they typed it, the
// UI shows it, and the turn hangs — the tether#49 wedge in a new costume.
func TestResolve_FailedResumeFallsBackAndReplays(t *testing.T) {
	dead := newDeadSession()
	live := &fakeSession{sid: "recovered-sid", events: make(chan agent.Event, 8)}
	dp := &deadThenLiveProvider{dead: dead, live: live}
	reg := NewRegistry(dp)

	att, err := reg.Attach(context.Background(), "gone-sid", "fake", "")
	if err != nil {
		t.Fatalf("Attach: %v", err)
	}

	// The user's prompt reaches the dead cc and fails, exactly as it does live.
	if err := att.SendPrompt(context.Background(), "remember the word ALPHA"); err == nil {
		t.Fatal("SendPrompt to a dead session returned nil; the test double is not modelling the broken pipe")
	}

	res, err := att.Resolve(context.Background())
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	if res.SID != "recovered-sid" {
		t.Errorf("Resolution.SID = %q, want \"recovered-sid\" (the fallback session)", res.SID)
	}
	if !res.Recovered {
		t.Error("Recovered = false; a failed resume that fell back MUST report it so the browser replaces its sid")
	}
	if got := dp.Spawns(); got != 2 {
		t.Fatalf("spawns = %d, want 2 (the resume attempt, then the fallback)", got)
	}

	cfgs := dp.Configs()
	if cfgs[0].ResumeSessionID != "gone-sid" {
		t.Errorf("first spawn ResumeSessionID = %q, want \"gone-sid\"", cfgs[0].ResumeSessionID)
	}
	if cfgs[1].ResumeSessionID != "" {
		t.Errorf("fallback spawn ResumeSessionID = %q, want \"\" — retrying the resume that just failed would loop", cfgs[1].ResumeSessionID)
	}
	if cfgs[1].SessionID == "" {
		t.Error("fallback spawn SessionID = \"\"; the recovered session must pin a NEW minted id so it is itself resumable")
	}
	if cfgs[1].SessionID == cfgs[0].ResumeSessionID {
		t.Error("fallback reused the dead sid as its minted id")
	}

	// The replay is the payload of this whole slice.
	prompts := live.Prompts()
	if len(prompts) != 1 || prompts[0] != "remember the word ALPHA" {
		t.Errorf("fallback session prompts = %v, want exactly [\"remember the word ALPHA\"] replayed", prompts)
	}
}

// TestResolve_ReplaysEveryBufferedPrompt — every prompt sent before the session
// was confirmed is replayed, in order, not just the first.
//
// A dead cc exits without reading its stdin, so NOTHING sent before the failure
// was delivered; replaying only the first would leave later prompts recorded in
// history (WaitSID resolves them to the new sid) yet answered by no one — a
// transcript that lies about what the model saw.
func TestResolve_ReplaysEveryBufferedPrompt(t *testing.T) {
	dead := newDeadSession()
	live := &fakeSession{sid: "recovered-sid", events: make(chan agent.Event, 8)}
	dp := &deadThenLiveProvider{dead: dead, live: live}
	reg := NewRegistry(dp)

	att, err := reg.Attach(context.Background(), "gone-sid", "fake", "")
	if err != nil {
		t.Fatalf("Attach: %v", err)
	}
	for _, p := range []string{"first", "second", "third"} {
		_ = att.SendPrompt(context.Background(), p)
	}
	if _, err := att.Resolve(context.Background()); err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	want := []string{"first", "second", "third"}
	got := live.Prompts()
	if len(got) != len(want) {
		t.Fatalf("replayed %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("replayed %v, want %v (order matters)", got, want)
		}
	}
}

// TestResolve_PromptsAfterSettlingAreNotReplayed — the buffer is for UNCONFIRMED
// prompts only. Once a session is settled, a later prompt must not be retained
// (it was delivered), or a long conversation would accumulate every message it
// ever sent and any future fallback would re-ask all of them.
func TestResolve_PromptsAfterSettlingAreNotReplayed(t *testing.T) {
	live := &fakeSession{sid: "live-sid", events: make(chan agent.Event, 8)}
	fp := &fakeProvider{sess: live}
	reg := NewRegistry(fp)

	att, err := reg.Attach(context.Background(), "", "fake", "")
	if err != nil {
		t.Fatalf("Attach: %v", err)
	}
	if _, err := att.Resolve(context.Background()); err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if err := att.SendPrompt(context.Background(), "after settling"); err != nil {
		t.Fatalf("SendPrompt: %v", err)
	}

	att.mu.Lock()
	pending := len(att.pending)
	att.mu.Unlock()
	if pending != 0 {
		t.Errorf("pending = %d after settling, want 0 (nothing more can need replaying)", pending)
	}
}

// TestResolve_FreshSessionDeathIsNotRetried — a FRESH session that dies before
// init is a real error, not a fallback trigger. There is nothing to fall back
// *to*: the cause is environmental (missing/broken cc binary, unusable workdir)
// and respawning would repeat it, so an unbounded retry here would spin instead
// of surfacing a broken install.
func TestResolve_FreshSessionDeathIsNotRetried(t *testing.T) {
	dp := &deadOnlyProvider{dead: newDeadSession()}
	reg := NewRegistry(dp)

	att, err := reg.Attach(context.Background(), "", "fake", "")
	if err != nil {
		t.Fatalf("Attach: %v", err)
	}
	if _, err := att.Resolve(context.Background()); err == nil {
		t.Fatal("Resolve returned nil error for a fresh session that never emitted init")
	}
	if got := dp.Spawns(); got != 1 {
		t.Errorf("spawns = %d, want 1: a fresh death must not be retried", got)
	}
}

// deadOnlyProvider always hands back a dying session.
type deadOnlyProvider struct {
	dead *deadSession

	mu     sync.Mutex
	spawns int
}

func (p *deadOnlyProvider) Name() string { return "fake" }
func (p *deadOnlyProvider) Spawn(_ context.Context, _ agent.SpawnConfig) (agent.Session, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.spawns++
	if p.spawns == 1 {
		go p.dead.die()
		return p.dead, nil
	}
	return newDeadSession(), nil
}
func (p *deadOnlyProvider) Spawns() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.spawns
}

// TestResolve_SubscriberSurvivesFallback — a channel subscribed BEFORE the
// fallback keeps receiving after it.
//
// The subscriber set lives on the Entry, and the fallback replaces the Entry, so
// without the re-subscribe in Attachment.resolve the recovered session would
// answer into a void: the browser would sit on "thinking…" while a perfectly
// healthy cc streamed to nobody. That failure is invisible to every other
// assertion in this file, which is why it gets its own test.
func TestResolve_SubscriberSurvivesFallback(t *testing.T) {
	dead := newDeadSession()
	live := &fakeSession{sid: "recovered-sid", events: make(chan agent.Event, 8)}
	dp := &deadThenLiveProvider{dead: dead, live: live}
	reg := NewRegistry(dp)

	att, err := reg.Attach(context.Background(), "gone-sid", "fake", "")
	if err != nil {
		t.Fatalf("Attach: %v", err)
	}
	ch := make(chan wire.Envelope, 8)
	att.Subscribe(ch)
	defer att.Unsubscribe(ch)

	_ = att.SendPrompt(context.Background(), "hi")
	if _, err := att.Resolve(context.Background()); err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	// Drive the RECOVERED session and prove the pre-existing subscriber sees it.
	live.events <- agent.Event{Kind: agent.EventInit, SessionID: "recovered-sid"}
	live.events <- agent.Event{Kind: agent.EventText, Text: "hello from the new session"}

	select {
	case env := <-ch:
		if env.Kind != wire.KindMessage {
			t.Fatalf("first envelope kind = %q, want %q", env.Kind, wire.KindMessage)
		}
		if s, _ := env.Payload.(string); s != "hello from the new session" {
			t.Fatalf("payload = %#v, want the recovered session's text", env.Payload)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("subscriber received nothing from the fallback session: the re-subscribe on Entry swap is broken")
	}
}

// ─── the "new session" notice gate ──────────────────────────────────────────

// TestResolve_NoticeOnlyWhenTheDeadSessionHadHistory — the gate from
// mem_2ruSlrHR ⑦.
//
// A cc session with zero completed turns is NOT resumable — no transcript is
// written — so "connected, then reloaded before saying anything" ALWAYS lands in
// the fallback. That is the ordinary first-run path, and telling those users they
// lost a conversation they never had is crying wolf. The notice is therefore
// gated on the daemon's own history for the dead sid.
func TestResolve_NoticeOnlyWhenTheDeadSessionHadHistory(t *testing.T) {
	for _, tc := range []struct {
		name       string
		seed       bool
		wantNotice bool
	}{
		{name: "had history → tell the user", seed: true, wantNotice: true},
		{name: "zero-turn session → stay silent", seed: false, wantNotice: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dead := newDeadSession()
			live := &fakeSession{sid: "recovered-sid", events: make(chan agent.Event, 8)}
			dp := &deadThenLiveProvider{dead: dead, live: live}
			reg := NewRegistry(dp)
			reg.History = NewHistoryStore(filepath.Join(t.TempDir(), "sessions"))

			if tc.seed {
				reg.History.RecordUser("gone-sid", "something I said earlier")
			}

			att, err := reg.Attach(context.Background(), "gone-sid", "fake", "")
			if err != nil {
				t.Fatalf("Attach: %v", err)
			}
			_ = att.SendPrompt(context.Background(), "are you still there")
			res, err := att.Resolve(context.Background())
			if err != nil {
				t.Fatalf("Resolve: %v", err)
			}
			if !res.Recovered {
				t.Fatal("Recovered = false; both cases fall back")
			}
			if res.Notice != tc.wantNotice {
				t.Errorf("Notice = %v, want %v (seeded history: %v)", res.Notice, tc.wantNotice, tc.seed)
			}
		})
	}
}

// TestHasHistory — the notice gate's own predicate, independent of the fallback.
func TestHasHistory(t *testing.T) {
	h := NewHistoryStore(filepath.Join(t.TempDir(), "sessions"))
	if h.HasHistory("") {
		t.Error("HasHistory(\"\") = true, want false")
	}
	if h.HasHistory("never-existed") {
		t.Error("HasHistory of an unknown sid = true, want false")
	}
	// Realistic ids: HasHistory now shares ValidSessionID with the /api/v1/sessions
	// route (it is the one HistoryStore entry point reached with a RAW client sid),
	// and that guard bounds length and alphabet — so a 5-character stand-in would
	// fail here while every real sid passes.
	h.RecordUser("sid-alpha-0001", "hello")
	if !h.HasHistory("sid-alpha-0001") {
		t.Error("HasHistory after RecordUser = false, want true")
	}
	if h.HasHistory("sid-bravo-0002") {
		t.Error("HasHistory leaked across sids")
	}

	// The traversal guard, staged so it can actually FAIL. HasHistory is the one
	// HistoryStore entry point reached with a RAW client sid (Attachment.resolve asks
	// it about a.reqSID straight off `/wt/chat?sid=`), so an unguarded version is a
	// stat oracle for any file named history.jsonl.
	//
	// The planted file is the point: asking about a traversal path that does not
	// exist answers false whether or not the guard is there, which is an assertion
	// that cannot fail and therefore is not evidence.
	base := t.TempDir()
	outside := filepath.Join(base, "outside")
	if err := os.MkdirAll(outside, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(outside, "history.jsonl"), []byte("{\"role\":\"user\"}\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	h2 := NewHistoryStore(filepath.Join(base, "sessions"))
	if h2.HasHistory("../outside") {
		t.Error("HasHistory answered about a file OUTSIDE its own directory; a traversal-shaped sid must be refused")
	}
}

// ─── WaitSID: the replayed prompt must reach history ────────────────────────

// TestWaitSID_ReturnsTheFallbackSid — history has to be written under the sid
// that ANSWERED a prompt, not the one the client asked for.
//
// serveChat records each user message in a goroutine. Before tether#50 it used
// the session's own SessionID(), which on the fallback path is "" — and
// RecordUserMessage silently drops "" — so the user's own message would have gone
// missing from the transcript a reload replays: they would see the assistant's
// answer with nothing above it. WaitSID hands out the settled sid instead.
func TestWaitSID_ReturnsTheFallbackSid(t *testing.T) {
	dead := newDeadSession()
	live := &fakeSession{sid: "recovered-sid", events: make(chan agent.Event, 8)}
	dp := &deadThenLiveProvider{dead: dead, live: live}
	reg := NewRegistry(dp)

	att, err := reg.Attach(context.Background(), "gone-sid", "fake", "")
	if err != nil {
		t.Fatalf("Attach: %v", err)
	}

	// Mirror serveChat: the recorder parks in WaitSID while the prompt is in
	// flight, so it must not be reachable only after Resolve has been called.
	got := make(chan string, 1)
	go func() { got <- att.WaitSID() }()

	_ = att.SendPrompt(context.Background(), "hello")
	if _, err := att.Resolve(context.Background()); err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	select {
	case sid := <-got:
		if sid != "recovered-sid" {
			t.Errorf("WaitSID() = %q, want \"recovered-sid\"; recording under anything else loses the message", sid)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("WaitSID never returned: a prompt-recording goroutine would leak for the daemon's lifetime")
	}
}

// TestWaitSID_UnblocksWhenResolveFails — WaitSID must be released on the ERROR
// path too. serveChat spawns one recorder goroutine per prompt; if Resolve's
// failure left them parked, every failed connection would leak goroutines for as
// long as the daemon runs. "" is the right answer there — RecordUserMessage
// treats it as a no-op, which is correct for a prompt nothing ever answered.
func TestWaitSID_UnblocksWhenResolveFails(t *testing.T) {
	dp := &deadOnlyProvider{dead: newDeadSession()}
	reg := NewRegistry(dp)

	att, err := reg.Attach(context.Background(), "", "fake", "")
	if err != nil {
		t.Fatalf("Attach: %v", err)
	}
	got := make(chan string, 1)
	go func() { got <- att.WaitSID() }()

	if _, err := att.Resolve(context.Background()); err == nil {
		t.Fatal("Resolve succeeded; this test needs the failure path")
	}
	select {
	case sid := <-got:
		if sid != "" {
			t.Errorf("WaitSID() = %q on a failed resolve, want \"\"", sid)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("WaitSID never returned on the error path — goroutine leak")
	}
}

// ─── review follow-ups ──────────────────────────────────────────────────────

// TestSetOwner_WorksBeforeTheEntryIsRekeyed is a regression guard for a race an
// adversarial review measured at 500/500 in a tight harness.
//
// spawnEntry USED TO register each entry under a `pending-%p` placeholder and
// re-key it to its real sid from a goroutine parked in SessionID(). Resolve waits
// on that SAME wakeup, so it routinely returned before the re-key had taken the
// lock; a sid-keyed ownership lookup therefore found nothing and answered false —
// which serveChat treats as a fatal ownership race, sending an error envelope and
// dropping the connection while the user's first answer is in flight.
//
// tether#54 removed the placeholder, so for cc the lookup would no longer lose.
// This test stays, and stays worth having, because it pins the reason ownership is
// resolved through the *Entry regardless. The fake here mints its own id (like
// opencode), and it emits NO init — so at the moment Resolve returns, the entry is
// still registered under the id the registry pinned, NOT under the res.SID this
// call is about. A sid-keyed lookup would miss it, exactly as it would in the
// production window between opencode publishing its sid and fanOut adopting it.
// SetOwner must not care.
//
// The assertion is deliberately "0 losses out of many": one loss is a user-visible
// dropped connection.
func TestSetOwner_WorksBeforeTheEntryIsRekeyed(t *testing.T) {
	const n = 300
	losses := 0
	for i := 0; i < n; i++ {
		ready := make(chan struct{})
		fs := &fakeSession{sid: "sid-race", events: make(chan agent.Event, 4), sidReady: ready}
		reg := NewRegistry(&fakeProvider{sess: fs})
		att, err := reg.Attach(context.Background(), "", "fake", "")
		if err != nil {
			t.Fatalf("Attach: %v", err)
		}
		// Release Resolve. (Under the old code this also released the re-key
		// goroutine, which is what made the two collide every single time.)
		close(ready)
		if _, err := att.Resolve(context.Background()); err != nil {
			t.Fatalf("Resolve: %v", err)
		}
		if !att.SetOwner("client-1") {
			losses++
		}
		close(fs.events)
	}
	if losses != 0 {
		t.Errorf("SetOwner returned false %d/%d times; each one is a connection dropped on the user's first message", losses, n)
	}
}

// TestSetOwner_StillRejectsADifferentClient — going through the Entry must not
// weaken the actual ownership rule; only the lookup changed.
func TestSetOwner_StillRejectsADifferentClient(t *testing.T) {
	fs := &fakeSession{sid: "sid-owned", events: make(chan agent.Event, 4)}
	reg := NewRegistry(&fakeProvider{sess: fs})
	att, err := reg.Attach(context.Background(), "", "fake", "")
	if err != nil {
		t.Fatalf("Attach: %v", err)
	}
	if !att.SetOwner("client-1") {
		t.Fatal("first claim was rejected")
	}
	if !att.SetOwner("client-1") {
		t.Error("re-claiming by the same client must be idempotent")
	}
	if att.SetOwner("client-2") {
		t.Error("a second client was allowed to take ownership")
	}
}

// TestResolve_IsIdempotent — two Resolve calls must not each run the fallback.
// Without the sync.Once around the whole resolution, both observe resuming==true,
// both respawn, and one cc subprocess is orphaned with no entry pointing at it.
func TestResolve_IsIdempotent(t *testing.T) {
	dead := newDeadSession()
	live := &fakeSession{sid: "recovered-sid", events: make(chan agent.Event, 8)}
	dp := &deadThenLiveProvider{dead: dead, live: live}
	reg := NewRegistry(dp)

	att, err := reg.Attach(context.Background(), "gone-sid", "fake", "")
	if err != nil {
		t.Fatalf("Attach: %v", err)
	}
	_ = att.SendPrompt(context.Background(), "only once please")

	res1, err1 := att.Resolve(context.Background())
	res2, err2 := att.Resolve(context.Background())
	if err1 != nil || err2 != nil {
		t.Fatalf("Resolve errors: %v / %v", err1, err2)
	}
	if res1 != res2 {
		t.Errorf("Resolve returned different outcomes: %+v vs %+v", res1, res2)
	}
	if got := dp.Spawns(); got != 2 {
		t.Errorf("spawns = %d, want 2 (resume attempt + ONE fallback); a second fallback orphans a cc subprocess", got)
	}
	if got := live.Prompts(); len(got) != 1 {
		t.Errorf("replayed prompts = %v, want exactly one", got)
	}
}

// TestResolve_CancelledConnectionIsNotReportedAsAFailedResume — a client that
// disappears mid-attach kills the subprocess, so SessionID() returns "" and it
// looks identical to a failed resume. It must not be counted as one: the
// "chat resume failed" log line is the only signal an operator has for how often
// resumes really fail, and every closed tab would pollute it. Nor should a fresh
// session be spawned for a client that has gone away.
func TestResolve_CancelledConnectionIsNotReportedAsAFailedResume(t *testing.T) {
	dp := &deadThenLiveProvider{dead: newDeadSession(), live: &fakeSession{sid: "unused", events: make(chan agent.Event, 4)}}
	reg := NewRegistry(dp)

	ctx, cancel := context.WithCancel(context.Background())
	att, err := reg.Attach(ctx, "gone-sid", "fake", "")
	if err != nil {
		t.Fatalf("Attach: %v", err)
	}
	cancel()

	if _, err := att.Resolve(ctx); err == nil {
		t.Fatal("Resolve succeeded for a cancelled connection")
	} else if !strings.Contains(err.Error(), "connection closed") {
		t.Errorf("error = %q, want it to name the closed connection rather than a resume failure", err)
	}
	if got := dp.Spawns(); got != 1 {
		t.Errorf("spawns = %d, want 1: no fallback should be started for a client that has gone away", got)
	}
}

// TestResolve_FallbackWhenSessionIDBlocks exercises the interleaving production
// actually has, which every other test in this file skips.
//
// The fakes return from SessionID() immediately; real cc BLOCKS there until
// system/init, which under stream-json needs the user's first prompt. So in
// production the fallback's own `fresh.Session().SessionID()` is a genuine wait,
// and a prompt has to arrive for it to finish. Modelled here with fakeSession's
// sidReady gate: Resolve must stay parked, then complete correctly once the fresh
// session announces itself.
func TestResolve_FallbackWhenSessionIDBlocks(t *testing.T) {
	ready := make(chan struct{})
	dead := newDeadSession()
	live := &fakeSession{sid: "recovered-sid", events: make(chan agent.Event, 8), sidReady: ready}
	dp := &deadThenLiveProvider{dead: dead, live: live}
	reg := NewRegistry(dp)

	att, err := reg.Attach(context.Background(), "gone-sid", "fake", "")
	if err != nil {
		t.Fatalf("Attach: %v", err)
	}
	_ = att.SendPrompt(context.Background(), "remember ALPHA")

	done := make(chan Resolution, 1)
	errc := make(chan error, 1)
	go func() {
		res, err := att.Resolve(context.Background())
		if err != nil {
			errc <- err
			return
		}
		done <- res
	}()

	// Resolve must NOT have completed: the fresh session has not announced itself.
	select {
	case res := <-done:
		t.Fatalf("Resolve returned %+v before the fresh session emitted its init", res)
	case err := <-errc:
		t.Fatalf("Resolve failed early: %v", err)
	case <-time.After(150 * time.Millisecond):
	}

	// The replay must already have been delivered — that is what makes a real cc
	// emit init in the first place, so it cannot be gated behind init.
	if got := live.Prompts(); len(got) != 1 || got[0] != "remember ALPHA" {
		t.Fatalf("prompts delivered before init = %v, want [\"remember ALPHA\"]; a real cc would never init", got)
	}

	close(ready)
	select {
	case res := <-done:
		if res.SID != "recovered-sid" || !res.Recovered {
			t.Errorf("Resolution = %+v, want SID=recovered-sid Recovered=true", res)
		}
	case err := <-errc:
		t.Fatalf("Resolve: %v", err)
	case <-time.After(2 * time.Second):
		t.Fatal("Resolve never completed after the fresh session announced itself")
	}
}

// TestResolve_ReapsTheFailedResumeSubprocess — the abandoned cc must be Close()d,
// which is the only thing in the daemon that waits on a cc subprocess and
// therefore the only thing that reaps one.
//
// Measured live when this was added (tether#50): driving three successive dead-sid
// reconnects against one daemon left 5 zombie `claude` children without this call
// and 2 with it — exactly one leaked process per failed resume. It mattered
// because tether#50 makes a failed resume an ORDINARY reload event rather than a
// rare one, so the leak rate scaled with how often users reload.
//
// Read those numbers as history, not as current behaviour: the residual 2 were the
// ordinary-teardown leak, and tether#56 closed that by adding a SECOND reaper in
// Registry.teardown. This test is now about the EAGER one specifically — see the
// holdDeadStreamOpen comment below for how the two are told apart.
//
// Safety of calling Close() here (i.e. cmd.Wait() while readLoop may still be
// reading stdout) rests on WHY we are on this path: SessionID() returned ""
// only because the `done` channel closed, and readLoop closes it from its own
// defer, so readLoop has already returned. See the comment at the call site.
func TestResolve_ReapsTheFailedResumeSubprocess(t *testing.T) {
	dead := newDeadSession()
	live := &fakeSession{sid: "recovered-sid", events: make(chan agent.Event, 8)}
	// Hold the dead session's event stream open. tether#56 added a SECOND reaper —
	// Registry.teardown, from the same entry's fanOut defer — and it fires on its
	// own goroutine the instant that stream closes, so with an ordinary death the
	// count below is 1 or 2 depending on scheduling (measured: it fails within ~50
	// iterations of -count). Parking fanOut short of its defer is what keeps this
	// test about the EAGER reap: it is the only thing that can have closed the
	// session while the stream is still open.
	dp := &deadThenLiveProvider{dead: dead, live: live, holdDeadStreamOpen: true}
	reg := NewRegistry(dp)

	att, err := reg.Attach(context.Background(), "gone-sid", "fake", "")
	if err != nil {
		t.Fatalf("Attach: %v", err)
	}
	_ = att.SendPrompt(context.Background(), "hello")
	if _, err := att.Resolve(context.Background()); err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	if got := dead.Closes(); got != 1 {
		t.Errorf("dead session Close() calls = %d, want 1; without it the cc subprocess stays a zombie for the daemon's lifetime", got)
	}

	// Now let the stream close, i.e. let the ordinary teardown run too. The two
	// reapers deliberately overlap and the real ccSession absorbs the second call
	// in a sync.Once (tether#56) — asserted here so that "resolve can stop reaping,
	// teardown covers it" is a visible change to this test rather than a silent one.
	dead.endStream()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && dead.Closes() < 2 {
		time.Sleep(time.Millisecond)
	}
	if got := dead.Closes(); got != 2 {
		t.Errorf("dead session Close() calls after its stream closed = %d, want 2 "+
			"(the eager reap plus Registry.teardown's)", got)
	}
}
