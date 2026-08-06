package session

import (
	"context"
	"errors"
	"fmt"
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
	_, resolveErr := att.Resolve(context.Background())
	if resolveErr == nil {
		t.Fatal("Resolve returned nil error for a fresh session that never emitted init")
	}
	if got := dp.Spawns(); got != 1 {
		t.Errorf("spawns = %d, want 1: a fresh death must not be retried", got)
	}

	// tether#63: classified ErrCodeSessionUnconfirmed, and it MUST be
	// retryable (see resolve's doc comment on this branch) — an ordinary
	// browser reconnect for the same sid is what lets a transient spawn
	// failure recover on the next attempt.
	var ref *Refusal
	if !errors.As(resolveErr, &ref) {
		t.Fatalf("error %v (%T) is not a *Refusal", resolveErr, resolveErr)
	}
	if ref.Code != wire.ErrCodeSessionUnconfirmed {
		t.Errorf("code = %q, want %q", ref.Code, wire.ErrCodeSessionUnconfirmed)
	}
	if ref.Code.Terminal() {
		t.Error("ErrCodeSessionUnconfirmed must be retryable, not terminal")
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

	_, resolveErr := att.Resolve(ctx)
	if resolveErr == nil {
		t.Fatal("Resolve succeeded for a cancelled connection")
	} else if !strings.Contains(resolveErr.Error(), "connection closed") {
		t.Errorf("error = %q, want it to name the closed connection rather than a resume failure", resolveErr)
	}
	if got := dp.Spawns(); got != 1 {
		t.Errorf("spawns = %d, want 1: no fallback should be started for a client that has gone away", got)
	}

	// tether#63: classified ErrCodeConnectionClosed, retryable — the daemon
	// did nothing wrong here, and reconnecting is the entire remedy.
	var ref *Refusal
	if !errors.As(resolveErr, &ref) {
		t.Fatalf("error %v (%T) is not a *Refusal", resolveErr, resolveErr)
	}
	if ref.Code != wire.ErrCodeConnectionClosed {
		t.Errorf("code = %q, want %q", ref.Code, wire.ErrCodeConnectionClosed)
	}
	if ref.Code.Terminal() {
		t.Error("ErrCodeConnectionClosed must be retryable, not terminal")
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

// ─── the third attachment state: a REUSED session that dies (tether#59) ─────

// reopenProvider hands back a session meant to be REUSED first (so it has to be
// registered and alive when the second attach arrives) and a second one for the
// re-open that follows its death.
//
// It is neither of the two doubles above, and the difference is the whole point of
// this section. deadThenLiveProvider models a cc that never emitted init
// (SessionID() == "") — tether#50's failed resume. corpseThenLiveProvider models
// one that died BEFORE it was adopted — tether#55's reuse gate. This one models
// the state neither covers: a session that was alive at attach time, was therefore
// reused, and only then stopped accepting prompts.
type reopenProvider struct {
	// Both are interfaces rather than *fakeSession, only so that a test can hand in a
	// session that holds every prompt at the instant of death (barrierSession) or one
	// that observes the attachment mid-recovery (probeSession). announceInit is in
	// first's set because seedReuse needs it; second is never announced through the
	// provider, so agent.Session is enough.
	first  reusableSession
	second agent.Session
	// third answers the third and later spawns, for the two-tab tests where one
	// session is re-opened twice. nil falls back to second.
	third agent.Session
	// spawnErr, when non-nil, fails every spawn after the first — how a re-open
	// that cannot start its replacement is staged.
	spawnErr error

	mu     sync.Mutex
	cfgs   []agent.SpawnConfig
	spawns int
}

// reusableSession is the double shape seedReuse needs: an agent.Session that can
// also announce the sid it will be reused under.
type reusableSession interface {
	agent.Session
	announceInit()
}

func (p *reopenProvider) Name() string { return "fake" }

func (p *reopenProvider) Spawn(_ context.Context, cfg agent.SpawnConfig) (agent.Session, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.cfgs = append(p.cfgs, cfg)
	p.spawns++
	if p.spawns == 1 {
		return p.first, nil
	}
	if p.spawnErr != nil {
		return nil, p.spawnErr
	}
	if p.spawns > 2 && p.third != nil {
		return p.third, nil
	}
	return p.second, nil
}

func (p *reopenProvider) Configs() []agent.SpawnConfig {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]agent.SpawnConfig(nil), p.cfgs...)
}

func (p *reopenProvider) Spawns() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.spawns
}

// seedReuse stands up the state every test in this section starts from: one live
// registered session, and an Attachment that REUSED it (no second spawn).
//
// The final check is not ceremony — if the reuse gate ever stopped reusing, every
// test below would still pass while testing something else entirely.
func seedReuse(t *testing.T, p *reopenProvider, sid string) (*Registry, *Attachment) {
	t.Helper()
	reg := NewRegistry(p)
	if _, err := reg.GetOrSpawnEntry(context.Background(), "", "fake"); err != nil {
		t.Fatalf("seed spawn: %v", err)
	}
	p.first.announceInit()
	waitForRegistered(t, reg, sid)

	att, err := reg.Attach(context.Background(), sid, "fake", "")
	if err != nil {
		t.Fatalf("Attach: %v", err)
	}
	if got := p.Spawns(); got != 1 {
		t.Fatalf("spawns after attach = %d, want 1: this section needs the REUSE path", got)
	}
	return reg, att
}

// attEntry reads the attachment's current Entry the way every other reader has to
// (under a.mu), so a test cannot accidentally observe a half-done swap.
func attEntry(a *Attachment) *Entry {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.entry
}

// TestSendPrompt_ReusedSessionThatDiesMidTurnIsReopened is the heart of tether#59.
//
// Sequence: a second connection arrives with a live sid and REUSES it (no resume,
// no fresh spawn, resuming == false) → that cc then exits mid-turn → the user's
// next prompt hits a broken pipe. Before this slice nothing downstream converted
// that error into recovery: serveChat logged it, Resolve had no fallback to run
// because resuming was false, and the browser sat on "thinking…" forever.
//
// The assertions are the whole user-visible chain: the prompt is not lost, the
// replacement resumes THIS sid (so the conversation's context comes back — a fresh
// spawn would answer while silently forgetting everything), and it carries no
// --session-id beside --resume (mutually exclusive, mem_2ruSlrHR ⑧).
func TestSendPrompt_ReusedSessionThatDiesMidTurnIsReopened(t *testing.T) {
	reused := &fakeSession{sid: "reused-sid", events: make(chan agent.Event, 8)}
	reopened := &fakeSession{sid: "reused-sid", events: make(chan agent.Event, 8)}
	p := &reopenProvider{first: reused, second: reopened}
	_, att := seedReuse(t, p, "reused-sid")

	// cc exits mid-turn: the entry is still registered, the sid is still cached,
	// and stdin is broken.
	reused.kill()

	if err := att.SendPrompt(context.Background(), "still there?"); err != nil {
		t.Fatalf("SendPrompt after the reused session died = %v; "+
			"an unrecovered error here is the turn wedged in \"thinking…\" forever", err)
	}
	if got := reused.Refused(); got != 1 {
		t.Errorf("the dead session refused %d prompts, want 1: this test is not staging the death", got)
	}
	if got := p.Spawns(); got != 2 {
		t.Fatalf("spawns = %d, want 2 (the reuse, then the re-open)", got)
	}

	cfgs := p.Configs()
	if cfgs[1].ResumeSessionID != "reused-sid" {
		t.Errorf("re-open ResumeSessionID = %q, want \"reused-sid\": recovery must re-open THAT "+
			"conversation, not spawn a blind fresh one that throws its context away", cfgs[1].ResumeSessionID)
	}
	if cfgs[1].SessionID != "" {
		t.Errorf("re-open SessionID = %q passed alongside --resume; the two are mutually exclusive", cfgs[1].SessionID)
	}
	if got := reopened.Prompts(); len(got) != 1 || got[0] != "still there?" {
		t.Errorf("re-opened session prompts = %v, want [\"still there?\"] — the prompt the user "+
			"already sent must be answered by the replacement", got)
	}
}

// TestResolve_AfterAReopenTheSidIsUnchangedAndNothingIsReportedLost — re-opening
// the same sid is what keeps the recovery invisible to everything downstream.
//
// Resolve must still answer with the sid the client asked for, and must NOT set
// Recovered: nothing WAS lost, the transcript was resumed. Recovered=true would
// make serveChat consider the "your context could not be restored" notice for a
// conversation that is intact, which is the one thing a notice must never do.
func TestResolve_AfterAReopenTheSidIsUnchangedAndNothingIsReportedLost(t *testing.T) {
	reused := &fakeSession{sid: "reused-sid", events: make(chan agent.Event, 8)}
	reopened := &fakeSession{sid: "reused-sid", events: make(chan agent.Event, 8)}
	p := &reopenProvider{first: reused, second: reopened}
	_, att := seedReuse(t, p, "reused-sid")

	reused.kill()
	if err := att.SendPrompt(context.Background(), "who am I?"); err != nil {
		t.Fatalf("SendPrompt: %v", err)
	}

	res, err := att.Resolve(context.Background())
	if err != nil {
		t.Fatalf("Resolve after a re-open: %v", err)
	}
	if res.SID != "reused-sid" {
		t.Errorf("Resolution.SID = %q, want \"reused-sid\": a re-open resumes the SAME session, so "+
			"the browser's sid, its history and its session_ready all stay valid", res.SID)
	}
	if res.Recovered {
		t.Error("Recovered = true after a re-open; the conversation was resumed, not replaced")
	}
	if got := att.WaitSID(); got != "reused-sid" {
		t.Errorf("WaitSID() = %q, want \"reused-sid\" — history must be written where the answer lives", got)
	}
}

// TestSendPrompt_ReopenKeepsSubscribers — a channel subscribed before the death
// keeps receiving after the re-open.
//
// The subscriber set lives on the Entry and a re-open REPLACES the Entry, so
// without moving them the recovered session streams to nobody: the daemon answers,
// the browser shows "thinking…" forever, and every other assertion in this file
// still passes. Same failure the fallback path has its own test for.
func TestSendPrompt_ReopenKeepsSubscribers(t *testing.T) {
	reused := &fakeSession{sid: "reused-sid", events: make(chan agent.Event, 8)}
	reopened := &fakeSession{sid: "reused-sid", events: make(chan agent.Event, 8)}
	p := &reopenProvider{first: reused, second: reopened}
	_, att := seedReuse(t, p, "reused-sid")

	ch := make(chan wire.Envelope, 8)
	att.Subscribe(ch)
	defer att.Unsubscribe(ch)

	reused.kill()
	if err := att.SendPrompt(context.Background(), "hi"); err != nil {
		t.Fatalf("SendPrompt: %v", err)
	}

	reopened.events <- agent.Event{Kind: agent.EventInit, SessionID: "reused-sid"}
	reopened.events <- agent.Event{Kind: agent.EventText, Text: "back from the dead"}

	select {
	case env := <-ch:
		if env.Kind != wire.KindMessage {
			t.Fatalf("first envelope kind = %q, want %q", env.Kind, wire.KindMessage)
		}
		if s, _ := env.Payload.(string); s != "back from the dead" {
			t.Fatalf("payload = %#v, want the re-opened session's text", env.Payload)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("subscriber received nothing from the re-opened session: the re-subscribe on Entry swap is missing")
	}
}

// TestSendPrompt_ReopenReplaysOnlyTheFailedPrompt — the re-open re-delivers the
// one prompt that failed, never the pending buffer.
//
// The buffer means something different on each path. On a failed resume cc exited
// without reading its stdin, so everything in it is undelivered and all of it must
// be replayed. On THIS path the session was alive and answering, so an earlier
// prompt in that buffer has already been delivered AND answered — and the
// transcript the replacement resumes contains both. Replaying it would ask the
// model a question it can see it has already answered.
func TestSendPrompt_ReopenReplaysOnlyTheFailedPrompt(t *testing.T) {
	reused := &fakeSession{sid: "reused-sid", events: make(chan agent.Event, 8)}
	reopened := &fakeSession{sid: "reused-sid", events: make(chan agent.Event, 8)}
	p := &reopenProvider{first: reused, second: reopened}
	_, att := seedReuse(t, p, "reused-sid")

	// Deliberately BEFORE Resolve, so the attachment is unsettled and this prompt is
	// sitting in the replay buffer when the death happens.
	if err := att.SendPrompt(context.Background(), "answered already"); err != nil {
		t.Fatalf("first SendPrompt: %v", err)
	}
	att.mu.Lock()
	buffered := len(att.pending)
	att.mu.Unlock()
	if buffered != 1 {
		t.Fatalf("pending = %d, want 1: this test needs a prompt in the buffer to be wrongly replayable", buffered)
	}

	reused.kill()
	if err := att.SendPrompt(context.Background(), "and this one is lost"); err != nil {
		t.Fatalf("SendPrompt after the death: %v", err)
	}

	got := reopened.Prompts()
	if len(got) != 1 || got[0] != "and this one is lost" {
		t.Errorf("re-opened session prompts = %v, want exactly [\"and this one is lost\"]: "+
			"replaying the buffer here re-asks a question the resumed transcript already answers", got)
	}
	if delivered := reused.Prompts(); len(delivered) != 1 || delivered[0] != "answered already" {
		t.Errorf("the reused session received %v, want [\"answered already\"]", delivered)
	}
}

// TestSendPrompt_ReopenIsAtMostOncePerAttachment — the budget is one.
//
// If the replacement ALSO refuses the prompt, spawning again would repeat whatever
// killed it, once per prompt, for as long as the user keeps typing — the same
// reason Resolve does not retry a fresh session that died. The second death is
// left to the browser's reconnect, which arrives on the `--resume` path with the
// whole fallback machinery behind it.
func TestSendPrompt_ReopenIsAtMostOncePerAttachment(t *testing.T) {
	reused := &fakeSession{sid: "reused-sid", events: make(chan agent.Event, 8)}
	reopened := &fakeSession{sid: "reused-sid", events: make(chan agent.Event, 8)}
	p := &reopenProvider{first: reused, second: reopened}
	_, att := seedReuse(t, p, "reused-sid")

	reused.kill()
	if err := att.SendPrompt(context.Background(), "first death"); err != nil {
		t.Fatalf("first SendPrompt: %v", err)
	}
	if got := p.Spawns(); got != 2 {
		t.Fatalf("spawns = %d, want 2 after the first re-open", got)
	}

	// The replacement dies too.
	reopened.kill()
	if err := att.SendPrompt(context.Background(), "second death"); err == nil {
		t.Error("SendPrompt after a SECOND death returned nil; the error must surface once the " +
			"one re-open is spent, so the caller logs it instead of the daemon spawning forever")
	}
	if got := p.Spawns(); got != 2 {
		t.Errorf("spawns = %d, want 2: a spent re-open must not spawn again", got)
	}
}

// TestSendPrompt_FailedResumeIsNotReopened is the interference guard between the
// two recovery paths, and the reason they are mutually exclusive by construction
// rather than by care.
//
// A failed `--resume` must keep behaving exactly as it did: SendPrompt returns the
// broken pipe (serveChat warns and carries on — bailing there is the tether#49
// wedge), and Resolve falls back to a FRESH session and replays. If the re-open
// state leaked onto this path it would instead re-resume the sid that just proved
// unresumable, which is a loop, and the spawn count is what catches it.
func TestSendPrompt_FailedResumeIsNotReopened(t *testing.T) {
	dead := newDeadSession()
	live := &fakeSession{sid: "recovered-sid", events: make(chan agent.Event, 8)}
	dp := &deadThenLiveProvider{dead: dead, live: live}
	reg := NewRegistry(dp)

	att, err := reg.Attach(context.Background(), "gone-sid", "fake", "")
	if err != nil {
		t.Fatalf("Attach: %v", err)
	}
	att.mu.Lock()
	reopenSID, resuming := att.reopenSID, att.resuming
	att.mu.Unlock()
	if reopenSID != "" {
		t.Errorf("reopenSID = %q on a --resume attach, want \"\": this path recovers by falling back "+
			"to a FRESH session, and re-resuming the sid that just failed would loop", reopenSID)
	}
	if !resuming {
		t.Fatal("resuming = false on a --resume attach; the test is not on the failed-resume path")
	}

	if err := att.SendPrompt(context.Background(), "remember ALPHA"); err == nil {
		t.Error("SendPrompt on the failed-resume path returned nil; the error must reach the caller " +
			"unchanged so Resolve's fallback owns the recovery")
	}
	if got := dp.Spawns(); got != 1 {
		t.Fatalf("spawns = %d, want 1: no re-open may happen on the failed-resume path", got)
	}

	res, err := att.Resolve(context.Background())
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if res.SID != "recovered-sid" || !res.Recovered {
		t.Errorf("Resolution = %+v, want SID=recovered-sid Recovered=true (the unchanged fallback)", res)
	}
	if got := dp.Spawns(); got != 2 {
		t.Errorf("spawns = %d, want 2 (the resume attempt plus ONE fresh fallback)", got)
	}
	cfgs := dp.Configs()
	if cfgs[1].ResumeSessionID != "" {
		t.Errorf("fallback ResumeSessionID = %q, want \"\": the fallback is FRESH, not another resume", cfgs[1].ResumeSessionID)
	}
	if got := live.Prompts(); len(got) != 1 || got[0] != "remember ALPHA" {
		t.Errorf("fallback prompts = %v, want the replayed [\"remember ALPHA\"]", got)
	}
}

// TestAttach_OnlyTheReusePathIsReopenable pins WHERE the third state is set, which
// is the wiring the rest of this section depends on and which nothing else asserts.
//
// It also pins the exclusivity the two recovery paths rest on: no attach may come
// back both fallback-eligible and re-openable, because the two disagree about what
// recovery means (fresh session vs this session).
func TestAttach_OnlyTheReusePathIsReopenable(t *testing.T) {
	t.Run("reuse → re-openable, not fallback-eligible", func(t *testing.T) {
		reused := &fakeSession{sid: "reused-sid", events: make(chan agent.Event, 8)}
		p := &reopenProvider{first: reused, second: &fakeSession{sid: "reused-sid", events: make(chan agent.Event, 8)}}
		_, att := seedReuse(t, p, "reused-sid")
		att.mu.Lock()
		defer att.mu.Unlock()
		if att.reopenSID != "reused-sid" {
			t.Errorf("reopenSID = %q, want \"reused-sid\"", att.reopenSID)
		}
		if att.resuming {
			t.Error("resuming = true on the reuse path: the fallback would spawn a FRESH session and " +
				"throw away a transcript that is sitting on disk")
		}
	})

	t.Run("dead sid → fallback-eligible, not re-openable", func(t *testing.T) {
		fp := &fakeProvider{sess: &fakeSession{sid: "dead-sid", events: make(chan agent.Event, 8)}}
		reg := NewRegistry(fp)
		att, err := reg.Attach(context.Background(), "dead-sid", "fake", "")
		if err != nil {
			t.Fatalf("Attach: %v", err)
		}
		att.mu.Lock()
		defer att.mu.Unlock()
		if att.reopenSID != "" {
			t.Errorf("reopenSID = %q, want \"\"", att.reopenSID)
		}
		if !att.resuming {
			t.Error("resuming = false for a sid that is not live; the resume is what makes it recoverable")
		}
	})

	// The fourth branch, and the one a review found unpinned: it lives INSIDE the same
	// `if e, ok := r.liveEntry(...)` block as the reuse path, so hoisting the
	// assignment one level — an easy edit, and the shape a careless refactor takes —
	// sets it on the rebind path too and the whole suite stays green.
	//
	// What that would cost: a rebound attachment is one whose sid belongs somewhere
	// ELSE, and it is deliberately given a fresh session in the requested workspace.
	// Making it re-openable points recovery at that foreign sid, so a send failure
	// spawns `--resume <the other workspace's sid>` in THIS directory — the foreign-cwd
	// resume resolveWorkspace's row-3 postmortem says never happens (cc keys its
	// transcript on cwd, so it fails deterministically), while evicting this
	// workspace's own fresh entry from the registry and spending the budget on a spawn
	// that is certain to die.
	t.Run("live session elsewhere → rebound, and NOT re-openable", func(t *testing.T) {
		// Staging note: the live-elsewhere branch needs dec.ResumeSID == sid AND a live
		// entry whose workdir differs from the one this connection resolves to, which
		// per Attach's own comment only happens when the ENTRY's workdir is empty —
		// i.e. it was spawned before the daemon had a directory. So: seed with no
		// Workdir, then give the daemon one.
		live := &fakeSession{sid: "elsewhere-sid", events: make(chan agent.Event, 8)}
		fresh := &fakeSession{sid: "fresh-sid", events: make(chan agent.Event, 8)}
		p := &reopenProvider{first: live, second: fresh}
		reg := NewRegistry(p)
		if _, err := reg.GetOrSpawnEntry(context.Background(), "", "fake"); err != nil {
			t.Fatalf("seed spawn: %v", err)
		}
		live.announceInit()
		waitForRegistered(t, reg, "elsewhere-sid")
		seeded := registeredEntry(reg, "elsewhere-sid")
		if seeded == nil || seeded.workdir != "" {
			t.Fatalf("seeded entry workdir = %q, want \"\": the rebind branch is unreachable otherwise", seeded.workdir)
		}
		reg.Workdir = t.TempDir()

		att, err := reg.Attach(context.Background(), "elsewhere-sid", "fake", "")
		if err != nil {
			t.Fatalf("Attach: %v", err)
		}
		att.mu.Lock()
		defer att.mu.Unlock()
		if !att.rebound {
			t.Fatal("rebound = false; this subtest is not on the live-elsewhere branch and proves nothing")
		}
		if att.reopenSID != "" {
			t.Errorf("reopenSID = %q on a rebound attach, want \"\": recovery would resume a sid that "+
				"belongs to another directory, which fails deterministically", att.reopenSID)
		}
		if !stillRegistered(reg, seeded) {
			t.Error("the live session elsewhere was evicted; a rebind must leave it alone")
		}
	})

	t.Run("no sid → neither", func(t *testing.T) {
		fp := &fakeProvider{sess: &fakeSession{sid: "fresh-sid", events: make(chan agent.Event, 8)}}
		reg := NewRegistry(fp)
		att, err := reg.Attach(context.Background(), "", "fake", "")
		if err != nil {
			t.Fatalf("Attach: %v", err)
		}
		att.mu.Lock()
		defer att.mu.Unlock()
		if att.reopenSID != "" {
			t.Errorf("reopenSID = %q, want \"\": there is no earlier conversation to re-open", att.reopenSID)
		}
		if att.resuming {
			t.Error("resuming = true on a fresh spawn")
		}
	})
}

// TestSendPrompt_NoReopenForAClientThatHasGoneAway — a cancelled connection kills
// the agent, so its sends fail exactly like a mid-turn death. It must not spawn:
// there is nobody left to answer, and the ctx it would be spawned under is already
// cancelled. Same reasoning as Resolve's cancelled-connection branch.
func TestSendPrompt_NoReopenForAClientThatHasGoneAway(t *testing.T) {
	reused := &fakeSession{sid: "reused-sid", events: make(chan agent.Event, 8)}
	reopened := &fakeSession{sid: "reused-sid", events: make(chan agent.Event, 8)}
	p := &reopenProvider{first: reused, second: reopened}
	_, att := seedReuse(t, p, "reused-sid")

	ctx, cancel := context.WithCancel(context.Background())
	reused.kill()
	cancel()

	if err := att.SendPrompt(ctx, "anyone there?"); err == nil {
		t.Error("SendPrompt returned nil for a cancelled connection; nothing was delivered")
	}
	if got := p.Spawns(); got != 1 {
		t.Errorf("spawns = %d, want 1: no agent should be started for a client that has gone away", got)
	}
	if got := reopened.Prompts(); len(got) != 0 {
		t.Errorf("a replacement session received %v for a client that had disconnected", got)
	}
}

// TestSendPrompt_ReopenDoesNotReapTheCorpseButTeardownDoes pins the deliberate
// omission, which is the one place this slice could have introduced a hang.
//
// Attachment.resolve reaps its failed resume, and the argument it uses does not
// transfer: there SessionID() returned "" only because ccSession's `done` closed,
// and readLoop closes it from its own defer, so every read of that process's stdout
// had finished — the precondition os/exec puts on cmd.Wait, which is half of
// Close(). Here the only evidence is a failed WRITE to stdin, which says nothing
// about readLoop. And if the session is not in fact dead, cmd.Wait blocks until the
// child exits — on serveChat's prompt-reader goroutine, i.e. hanging the turn this
// function exists to un-hang.
//
// Nothing leaks: tether#56 put the reap in Registry.teardown, from the entry's own
// fanOut defer. The second half of this test is that guarantee, so "the re-open can
// start reaping again" cannot be a silent change.
func TestSendPrompt_ReopenDoesNotReapTheCorpseButTeardownDoes(t *testing.T) {
	reused := &fakeSession{sid: "reused-sid", events: make(chan agent.Event, 8)}
	reopened := &fakeSession{sid: "reused-sid", events: make(chan agent.Event, 8)}
	p := &reopenProvider{first: reused, second: reopened}
	_, att := seedReuse(t, p, "reused-sid")

	// kill() leaves the event stream OPEN, which parks the entry's fanOut short of
	// its teardown defer — so a Close() observed now could only have come from the
	// re-open path.
	reused.kill()
	if err := att.SendPrompt(context.Background(), "still there?"); err != nil {
		t.Fatalf("SendPrompt: %v", err)
	}
	if got := reused.Closes(); got != 0 {
		t.Errorf("the corpse was Close()d %d times by the re-open path; cmd.Wait here races readLoop, "+
			"and on a session that is not actually dead it blocks the prompt reader indefinitely", got)
	}

	// Now let the stream close: the ordinary teardown must still reap it, which is
	// what makes not reaping above a deferral rather than a leak.
	close(reused.events)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && reused.Closes() == 0 {
		time.Sleep(time.Millisecond)
	}
	if got := reused.Closes(); got != 1 {
		t.Errorf("corpse Close() calls after its stream closed = %d, want 1 (Registry.teardown's reap)", got)
	}
}

// TestSendPrompt_ReopenReplacesTheRegistrationAndTheCorpseCannotTakeItBack.
//
// The replacement is registered under the SAME sid the corpse held, so the corpse's
// own teardown — which runs later, on its own goroutine — is aimed at that very
// key. Two existing invariants are what stop it from evicting the live replacement
// or re-pointing the key at itself, and both are load-bearing here:
//
//   - evict deletes BY VALUE, not by key, so it can only remove the entry it was
//     given (registry.go). A by-key eviction would silently un-register the
//     replacement mid-conversation.
//   - rekey MOVES a registration and never creates one (tether#12's resurrection
//     rule), so a stale system/init still sitting in the corpse's buffered stream
//     cannot put a dead session back under a live sid.
//
// Staged in that order: re-open, then feed the corpse an init, then let its stream
// close so fanOut runs rekey and teardown for real.
func TestSendPrompt_ReopenReplacesTheRegistrationAndTheCorpseCannotTakeItBack(t *testing.T) {
	reused := &fakeSession{sid: "reused-sid", events: make(chan agent.Event, 8)}
	reopened := &fakeSession{sid: "reused-sid", events: make(chan agent.Event, 8)}
	p := &reopenProvider{first: reused, second: reopened}
	reg, att := seedReuse(t, p, "reused-sid")

	corpseEntry := registeredEntry(reg, "reused-sid")
	if corpseEntry == nil {
		t.Fatal("the seeded session is not registered under its sid")
	}

	reused.kill()
	if err := att.SendPrompt(context.Background(), "still there?"); err != nil {
		t.Fatalf("SendPrompt: %v", err)
	}
	fresh := attEntry(att)
	if fresh == corpseEntry {
		t.Fatal("the attachment still points at the corpse after a re-open")
	}
	if got := registeredEntry(reg, "reused-sid"); got != fresh {
		t.Errorf("r.sessions[\"reused-sid\"] is not the replacement; a reconnect would find the corpse again")
	}

	// A stale init from the corpse's own stream, then the stream close that runs its
	// fanOut through rekey and teardown.
	reused.events <- agent.Event{Kind: agent.EventInit, SessionID: "reused-sid"}
	close(reused.events)

	// Wait on the corpse's Close(), NOT on it disappearing from r.sessions: the
	// re-open ALREADY un-registered it, so a "has it been evicted yet" loop exits
	// immediately and the assertions below then run BEFORE the corpse's own teardown
	// has done anything at all — which is a test that cannot observe the thing it is
	// about (measured: it left both the by-key-evict and the rekey-resurrection
	// mutants alive). teardown evicts first and reaps second, so a Close() is proof
	// that its eviction has already happened.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && reused.Closes() == 0 {
		time.Sleep(time.Millisecond)
	}
	if reused.Closes() == 0 {
		t.Fatal("the corpse's fanOut never reached teardown; the assertions below would be vacuous")
	}

	if stillRegistered(reg, corpseEntry) {
		t.Error("the corpse is still registered after its own teardown")
	}
	if got := registeredEntry(reg, "reused-sid"); got != fresh {
		t.Errorf("the corpse's teardown took the sid away from the live replacement "+
			"(registered entry = %p, replacement = %p); every later prompt and every reconnect "+
			"would then miss a session that is right there", got, fresh)
	}
}

// TestSendPrompt_ReopenCarriesTheOwnershipClaim — a recovery must not quietly
// return the conversation to unowned.
//
// serveChat claims ownership once, on the Entry, and the re-open replaces that
// Entry. Losing the claim would let a DIFFERENT client join a session admitChat
// refused it moments earlier — a hole opened by the fix rather than by anything the
// user did.
func TestSendPrompt_ReopenCarriesTheOwnershipClaim(t *testing.T) {
	reused := &fakeSession{sid: "reused-sid", events: make(chan agent.Event, 8)}
	reopened := &fakeSession{sid: "reused-sid", events: make(chan agent.Event, 8)}
	p := &reopenProvider{first: reused, second: reopened}
	reg, att := seedReuse(t, p, "reused-sid")

	if !att.SetOwner("client-1") {
		t.Fatal("first ownership claim was rejected")
	}

	reused.kill()
	if err := att.SendPrompt(context.Background(), "still there?"); err != nil {
		t.Fatalf("SendPrompt: %v", err)
	}

	if !reg.OwnedByOther("reused-sid", "client-2") {
		t.Error("the re-opened session is unowned: a different client could now take over a " +
			"conversation it was refused before the death")
	}
	if reg.OwnedByOther("reused-sid", "client-1") {
		t.Error("the owner lost its own session across the re-open")
	}
	if !att.SetOwner("client-1") {
		t.Error("the owner's own re-claim was rejected after the re-open")
	}
}

// barrierSession is a dead session that holds every prompt until `want` of them
// are inside it at once, then refuses them all together.
//
// It exists because "start N goroutines and hope" does not stage this race. Written
// that way, the first goroutine reliably finished its whole re-open before the
// second one even looked at the attachment's state — so the test passed with the
// serialisation DELETED (measured: the mutant survived). Releasing every caller at
// the same instant puts them all past the entry read and into the recovery path
// simultaneously, which is what the production race actually looks like when one cc
// dies while several prompts are in flight.
type barrierSession struct {
	*fakeSession
	want int
	open chan struct{}

	bmu sync.Mutex
	n   int
}

func (b *barrierSession) SendPrompt(_ context.Context, _ string) error {
	b.bmu.Lock()
	b.n++
	n := b.n
	b.bmu.Unlock()
	if n == b.want {
		close(b.open)
	}
	if n <= b.want {
		select {
		case <-b.open:
		case <-time.After(2 * time.Second):
		}
	}
	// A write to an exited cc's stdin, for every caller — this session is a corpse.
	return errBrokenPipe
}

// TestSendPrompt_ConcurrentPromptsProduceOneReopen — several prompts discovering
// the same death must produce ONE replacement, and every one of them must land.
//
// Without serialisation each failing send spawns its own `--resume` of the same
// transcript, and the later registration displaces the earlier under the same key —
// leaving a cc running that nothing points at, which is the shape tether#54 went to
// some trouble to eliminate. Without the "someone already re-opened, deliver there"
// branch the losers instead return an error and their prompts are simply lost.
//
// The prompt count is the sharper assertion of the two: a lost prompt is a message
// the user sent, that history records, and that nothing ever answers.
func TestSendPrompt_ConcurrentPromptsProduceOneReopen(t *testing.T) {
	const n = 8
	reused := &barrierSession{
		fakeSession: &fakeSession{sid: "reused-sid", events: make(chan agent.Event, 8)},
		want:        n,
		open:        make(chan struct{}),
	}
	reopened := &fakeSession{sid: "reused-sid", events: make(chan agent.Event, 8)}
	p := &reopenProvider{first: reused, second: reopened}
	_, att := seedReuse(t, p, "reused-sid")

	// The process is gone: Alive() says so and every prompt below will be refused.
	reused.dead.Store(true)

	var wg sync.WaitGroup
	errs := make(chan error, n)
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			if err := att.SendPrompt(context.Background(), fmt.Sprintf("p%d", i)); err != nil {
				errs <- err
			}
		}(i)
	}
	close(start)
	wg.Wait()
	close(errs)

	var failed []string
	for err := range errs {
		failed = append(failed, err.Error())
	}
	if len(failed) != 0 {
		t.Errorf("%d of %d concurrent prompts were not delivered: %v", len(failed), n, failed)
	}
	if got := p.Spawns(); got != 2 {
		t.Errorf("spawns = %d, want 2: concurrent deaths must produce ONE replacement, not one per prompt "+
			"(each extra spawn displaces the previous registration and orphans its cc)", got)
	}
	got := reopened.Prompts()
	if len(got) != n {
		t.Fatalf("the replacement received %d prompts, want %d: %v", len(got), n, got)
	}
	seen := map[string]int{}
	for _, s := range got {
		seen[s]++
	}
	for i := 0; i < n; i++ {
		if c := seen[fmt.Sprintf("p%d", i)]; c != 1 {
			t.Errorf("prompt p%d was delivered %d times, want exactly 1", i, c)
		}
	}
}

// TestSendPrompt_ReopenSpawnFailureNamesBothCausesAndDropsTheCorpse — when the
// replacement cannot be started there is nothing left to do but say so, in one
// error that names both halves: a caller told only "spawn failed" has lost the half
// that says which recovery was being attempted (the same reason errorEnvelope keeps
// the whole wrapped message rather than the innermost Refusal's).
//
// The corpse must still be un-registered — that is why the eviction happens BEFORE
// the spawn — or the next reconnect finds a dead session under a live sid. And the
// budget must be spent, so a user who keeps typing does not get one doomed spawn
// per keystroke.
func TestSendPrompt_ReopenSpawnFailureNamesBothCausesAndDropsTheCorpse(t *testing.T) {
	reused := &fakeSession{sid: "reused-sid", events: make(chan agent.Event, 8)}
	p := &reopenProvider{
		first:    reused,
		second:   &fakeSession{sid: "reused-sid", events: make(chan agent.Event, 8)},
		spawnErr: errors.New(`exec: "cc": executable file not found in $PATH`),
	}
	reg, att := seedReuse(t, p, "reused-sid")
	corpseEntry := registeredEntry(reg, "reused-sid")

	// sendFails WITHOUT dead, deliberately: the pipe is broken but `done` has not
	// closed, so Alive() still says true. That is the only staging in which the
	// explicit evict below is observable at all — with Alive() already false, the
	// sibling check's liveEntry drops the corpse itself as a side effect, and with a
	// successful spawn the replacement's registration overwrites the key. It is also
	// the case where the evict earns its keep: nothing else will un-register an entry
	// we have positive evidence (a failed write) is dead but whose Alive() has not
	// caught up, so the next reconnect would reuse a corpse — tether#55 again.
	reused.sendFails.Store(true)
	err := att.SendPrompt(context.Background(), "still there?")
	if err == nil {
		t.Fatal("SendPrompt returned nil when the re-open could not spawn")
	}
	for _, want := range []string{"reused-sid", "broken pipe", "executable file not found"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q; both causes and the session have to be in it", err, want)
		}
	}
	if stillRegistered(reg, corpseEntry) {
		t.Error("the corpse is still registered after a failed re-open; the next reconnect would adopt it")
	}
	if err := att.SendPrompt(context.Background(), "again?"); err == nil {
		t.Error("SendPrompt succeeded after a failed re-open")
	}
	if got := p.Spawns(); got != 2 {
		t.Errorf("spawns = %d, want 2: a failed re-open must spend the budget, not retry per prompt", got)
	}
}

// TestSendPrompt_ReopenStaysInTheSameWorkspace — cc keys its transcript on cwd
// (mem_2ruSlrHR ④), so a `--resume` anywhere else fails exactly like an unknown
// sid. A re-open that spawned into the daemon default would therefore not recover
// the conversation at all for every session that selected a workspace — it would
// fail, in the one code path whose entire purpose is recovery (tether#52's lesson,
// on a new path).
func TestSendPrompt_ReopenStaysInTheSameWorkspace(t *testing.T) {
	dir := t.TempDir()
	reused := &fakeSession{sid: "reused-sid", events: make(chan agent.Event, 8)}
	reopened := &fakeSession{sid: "reused-sid", events: make(chan agent.Event, 8)}
	p := &reopenProvider{first: reused, second: reopened}
	reg := NewRegistry(p)
	ws := WorkspaceBinding{WorkspaceID: "ws-1", Path: dir}

	// Seed a session that lives in a workspace, the way an Attach carrying ?ws= would.
	if _, err := reg.spawnEntry(context.Background(), "fake", agent.SpawnConfig{}, ws); err != nil {
		t.Fatalf("seed spawn: %v", err)
	}
	reused.announceInit()
	waitForRegistered(t, reg, "reused-sid")

	// The reconnect sends no ws — the live registration is what knows where it lives.
	att, err := reg.Attach(context.Background(), "reused-sid", "fake", "")
	if err != nil {
		t.Fatalf("Attach: %v", err)
	}
	if got := p.Spawns(); got != 1 {
		t.Fatalf("spawns = %d, want 1: this test needs the reuse path", got)
	}

	reused.kill()
	if err := att.SendPrompt(context.Background(), "still there?"); err != nil {
		t.Fatalf("SendPrompt: %v", err)
	}
	cfgs := p.Configs()
	if got := cfgs[1].Workdir; got != dir {
		t.Errorf("re-open Workdir = %q, want %q: resuming in any other directory fails like an unknown sid", got, dir)
	}
}

// ─── review follow-ups on tether#59 ─────────────────────────────────────────

// probeSession runs a hook on the way into SendPrompt, so a test can observe the
// attachment's state at the exact instant the prompt is delivered — the only way to
// pin an ORDERING between "swap the entry" and "send", rather than merely pinning
// that both happened.
type probeSession struct {
	*fakeSession
	onSend func()
}

func (p *probeSession) SendPrompt(ctx context.Context, text string) error {
	if p.onSend != nil {
		p.onSend()
	}
	return p.fakeSession.SendPrompt(ctx, text)
}

// subCount reports how many channels are subscribed to e, taking the same lock
// Entry.Subscribe writes under.
func subCount(e *Entry) int {
	e.subsMu.RLock()
	defer e.subsMu.RUnlock()
	return len(e.subs)
}

// drainEnvelopes reads everything available on ch until a KindResult arrives or the
// deadline passes, and reports both. Written as a drain rather than "the next
// envelope is X" because the fence parser decides how text is chunked, and a test
// about turn LIFECYCLE must not also encode that.
func drainEnvelopes(t *testing.T, ch <-chan wire.Envelope, within time.Duration) (envs []wire.Envelope, sawResult bool) {
	t.Helper()
	deadline := time.After(within)
	for {
		select {
		case env := <-ch:
			envs = append(envs, env)
			if env.Kind == wire.KindResult {
				return envs, true
			}
		case <-deadline:
			return envs, false
		}
	}
}

// TestSendPrompt_SecondAttachmentAdoptsTheFirstsReopen is the BLOCKER a review of
// this slice found: two attachments on one sid each spending their own re-open.
//
// Two live /wt/chat attachments on one sid is supported, not exotic — tabs of one
// browser share the sid in localStorage and the client id, and Attach's reuse branch
// deliberately hands the second one the first one's session. Each holds its own
// entry and its own budget, so before the sibling check the second tab's next prompt
// spawned a SECOND `cc --resume <sid>` and its registration displaced the first
// replacement: two cc appending to one transcript, both fanOuts accumulating into
// one history buffer, and every sid-keyed route (DeliverAction, InterruptSession,
// /wt/events) reaching one of them while the other tab talked to an orphan. That is
// verbatim the state tether#54 exists to prevent, with a window that lasts until the
// user types in the other tab rather than tether#60's single provider.Spawn.
func TestSendPrompt_SecondAttachmentAdoptsTheFirstsReopen(t *testing.T) {
	reused := &fakeSession{sid: "reused-sid", events: make(chan agent.Event, 8)}
	reopened := &fakeSession{sid: "reused-sid", events: make(chan agent.Event, 8)}
	p := &reopenProvider{first: reused, second: reopened}
	reg, att1 := seedReuse(t, p, "reused-sid")

	// The second tab: same sid, still live, so it reuses the same entry.
	att2, err := reg.Attach(context.Background(), "reused-sid", "fake", "")
	if err != nil {
		t.Fatalf("second Attach: %v", err)
	}
	if got := p.Spawns(); got != 1 {
		t.Fatalf("spawns = %d, want 1: both attachments must be on the SAME session", got)
	}
	ch2 := make(chan wire.Envelope, 8)
	att2.Subscribe(ch2)
	defer att2.Unsubscribe(ch2)

	reused.kill()

	// Tab 1 types: this is the re-open.
	if err := att1.SendPrompt(context.Background(), "from tab one"); err != nil {
		t.Fatalf("att1.SendPrompt: %v", err)
	}
	fresh := attEntry(att1)
	if got := p.Spawns(); got != 2 {
		t.Fatalf("spawns after the first re-open = %d, want 2", got)
	}

	// Tab 2 types later. Its entry is still the corpse and its own budget is
	// unspent — it must ADOPT rather than spawn.
	if err := att2.SendPrompt(context.Background(), "from tab two"); err != nil {
		t.Fatalf("att2.SendPrompt: %v", err)
	}
	if got := p.Spawns(); got != 2 {
		t.Errorf("spawns = %d, want 2: the second attachment must adopt the first's replacement, "+
			"not start a second cc --resume of the same transcript", got)
	}
	if got := attEntry(att2); got != fresh {
		t.Error("the second attachment is not bound to the replacement after adopting it")
	}
	if got := registeredEntry(reg, "reused-sid"); got != fresh {
		t.Error("the sid no longer names the first replacement: a second spawn displaced it, " +
			"leaving a cc running that nothing can reach")
	}
	if got := reopened.Prompts(); len(got) != 2 || got[0] != "from tab one" || got[1] != "from tab two" {
		t.Errorf("replacement prompts = %v, want both tabs' prompts in order", got)
	}

	// Adoption spawns nothing, so it must not spend the budget: the adopted session
	// can die too, and this attachment has not yet used its own recovery.
	att2.mu.Lock()
	spent := att2.reopenSpent
	att2.mu.Unlock()
	if spent {
		t.Error("adopting spent the second attachment's re-open budget; a later death of the " +
			"ADOPTED session would then be unrecoverable for it")
	}

	// The adopting attachment's subscriber must have been moved too, or tab 2 watches
	// a session that answers somewhere else.
	reopened.events <- agent.Event{Kind: agent.EventInit, SessionID: "reused-sid"}
	reopened.events <- agent.Event{Kind: agent.EventText, Text: "answering both tabs\n"}
	select {
	case env := <-ch2:
		if s, _ := env.Payload.(string); s != "answering both tabs\n" {
			t.Errorf("payload = %#v, want the replacement's text", env.Payload)
		}
	case <-time.After(2 * time.Second):
		t.Error("the adopting attachment's subscriber received nothing from the replacement: " +
			"tab 2 would watch a session that is answering somewhere else")
	}
}

// TestSendPrompt_MidAnswerDeathClosesTheTurnWithoutAFollowUpPrompt is acceptance
// criterion #1, and the half of tether#59 that SendPrompt cannot reach.
//
// cc dies mid-answer and the user types nothing more. No send fails, so no recovery
// is triggered — and before this, nothing closed the turn either: readLoop just
// closes its channel with no terminal event, evict only deletes registry keys, and
// the subscriber channel is never closed, so the browser sat on "thinking…" until
// its transport died. tether#58 shipped this same answer for opencode inside the
// provider (watchServeExit emits a terminal EventError before closeEvents); cc has
// no equivalent, so Registry.fanOut's stream-end is where it belongs.
//
// The flushed tail is asserted alongside it because it is the same four lines: text
// the agent produced but never terminated with a newline is held by the fence
// parser, and without the Flush it dies with the entry.
func TestSendPrompt_MidAnswerDeathClosesTheTurnWithoutAFollowUpPrompt(t *testing.T) {
	reused := &fakeSession{sid: "reused-sid", events: make(chan agent.Event, 8)}
	p := &reopenProvider{first: reused, second: &fakeSession{sid: "reused-sid", events: make(chan agent.Event, 8)}}
	_, att := seedReuse(t, p, "reused-sid")

	ch := make(chan wire.Envelope, 16)
	att.Subscribe(ch)
	defer att.Unsubscribe(ch)

	// Mid-answer: text has streamed and no EventResult has arrived, so the turn is
	// open — which is the state the browser is stuck in.
	reused.events <- agent.Event{Kind: agent.EventText, Text: "the answer so far\n"}

	// cc is killed: the process goes, then readLoop closes the stream. NOTHING else
	// happens — no follow-up prompt, which is the whole point.
	reused.kill()
	close(reused.events)

	envs, sawResult := drainEnvelopes(t, ch, 2*time.Second)
	if !sawResult {
		t.Errorf("no KindResult after a mid-answer death (%d envelopes: %v); the browser stays on "+
			"\"thinking…\" for as long as the connection lives", len(envs), envs)
	}
}

// TestFanOut_StreamEndFlushesAFenceTheAgentDiedInsideOf — the other half of the
// stream-end handler, and the half a mutation run showed unpinned.
//
// FenceParser HOLDS an in-fence body until the closing marker, so an agent that dies
// mid-block has produced content that exists only inside the parser. Flushing at
// stream end surfaces it as text — the parser's own documented answer for a
// truncated fence ("never silently lost, an empty screen") — while dropping the
// Flush loses the whole block body with nothing to show the user it existed.
//
// A first attempt at this used a trailing partial LINE instead, which proved nothing:
// Feed emits everything it consumes outside a fence, so that text had already been
// broadcast and deleting the Flush left the test green.
func TestFanOut_StreamEndFlushesAFenceTheAgentDiedInsideOf(t *testing.T) {
	reused := &fakeSession{sid: "reused-sid", events: make(chan agent.Event, 8)}
	p := &reopenProvider{first: reused, second: &fakeSession{sid: "reused-sid", events: make(chan agent.Event, 8)}}
	_, att := seedReuse(t, p, "reused-sid")

	ch := make(chan wire.Envelope, 16)
	att.Subscribe(ch)
	defer att.Unsubscribe(ch)

	// A fenced block that never gets its closing marker, because cc dies inside it.
	reused.events <- agent.Event{Kind: agent.EventText, Text: "```dag:myskill\n{\"nodes\":[\"a\"]}\n"}
	reused.kill()
	close(reused.events)

	envs, sawResult := drainEnvelopes(t, ch, 2*time.Second)
	if !sawResult {
		t.Fatalf("no turn-ender after the death (%d envelopes)", len(envs))
	}
	var text string
	for _, env := range envs {
		if s, ok := env.Payload.(string); ok && env.Kind == wire.KindMessage {
			text += s
		}
	}
	if !strings.Contains(text, "{\"nodes\":[\"a\"]}") {
		t.Errorf("broadcast text = %q, want the truncated fence body flushed at stream end; "+
			"without it the block the agent was mid-way through vanishes entirely", text)
	}
}

// TestFanOut_StreamEndStaysSilentForASessionThatNeverInited — the other side of that
// gate, and the reason it is `sawInit` rather than "the stream ended".
//
// A failed `--resume` closes its stream without ever emitting init, and its turn
// must stay OPEN: Attachment.resolve is about to fall back to a fresh session,
// replay the prompt, and let the REPLACEMENT close the turn. A turn-ender here would
// undo tether#50's recovery by telling the browser the turn finished moments before
// the real answer starts — and it would also resurrect the artifact the empty-result
// suppression exists to hide.
func TestFanOut_StreamEndStaysSilentForASessionThatNeverInited(t *testing.T) {
	never := &fakeSession{sid: "never-inits", events: make(chan agent.Event, 4)}
	reg := NewRegistry(&fakeProvider{sess: never})
	e, err := reg.spawnEntry(context.Background(), "fake", agent.SpawnConfig{}, WorkspaceBinding{})
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	ch := make(chan wire.Envelope, 8)
	e.Subscribe(ch)

	// The measured failed-resume shape: one empty result, no init, then the stream
	// closes (mem_2ruSlrHR ③).
	never.events <- agent.Event{Kind: agent.EventResult, Text: ""}
	close(never.events)

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && never.Closes() == 0 {
		time.Sleep(time.Millisecond)
	}
	if never.Closes() == 0 {
		t.Fatal("fanOut never reached teardown; the assertion below would be vacuous")
	}
	if n := len(ch); n != 0 {
		env := <-ch
		t.Errorf("a session that never init'd broadcast %d envelope(s), first = %+v; both the empty "+
			"result and the stream-end turn-ender must stay suppressed so the fallback owns the turn", n, env)
	}
}

// TestFanOut_StreamEndFlushesTheHalfAnswerSoItCannotGlueOntoTheNextTurn.
//
// HistoryStore.pending is keyed by SID, not by entry, and a corpse's fanOut keeps
// that sid: whatever readLoop was still draining accumulates into the very buffer
// the replacement then writes into, so the next FinalizeAssistant flushes the
// concatenation as ONE assistant message and a reload shows the dead session's
// half-sentence glued to the front of the recovered answer. New with tether#59 —
// on the failed-resume path the corpse's sid is "" and it wrote nothing.
//
// Finalizing at stream end makes the fragment its own message instead. Note what
// this does NOT claim: the two fanOuts can still interleave while the corpse is
// draining (see the residual noted in the report), it is the ordinary case that is
// made clean.
func TestFanOut_StreamEndFlushesTheHalfAnswerSoItCannotGlueOntoTheNextTurn(t *testing.T) {
	dying := &fakeSession{sid: "shared-sid", events: make(chan agent.Event, 8)}
	replacement := &fakeSession{sid: "shared-sid", events: make(chan agent.Event, 8)}
	p := &reopenProvider{first: dying, second: replacement}
	reg := NewRegistry(p)
	reg.History = NewHistoryStore(filepath.Join(t.TempDir(), "sessions"))

	if _, err := reg.GetOrSpawnEntry(context.Background(), "", "fake"); err != nil {
		t.Fatalf("seed spawn: %v", err)
	}
	dying.announceInit()
	waitForRegistered(t, reg, "shared-sid")

	// A half-answer, then death.
	dying.events <- agent.Event{Kind: agent.EventText, Text: "half an ans"}
	dying.kill()
	close(dying.events)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && dying.Closes() == 0 {
		time.Sleep(time.Millisecond)
	}

	// The replacement answers under the SAME sid and completes its turn.
	e2, err := reg.spawnEntry(context.Background(), "fake", agent.SpawnConfig{ResumeSessionID: "shared-sid"}, WorkspaceBinding{})
	if err != nil {
		t.Fatalf("replacement spawn: %v", err)
	}
	_ = e2
	replacement.events <- agent.Event{Kind: agent.EventInit, SessionID: "shared-sid"}
	replacement.events <- agent.Event{Kind: agent.EventText, Text: "the real answer"}
	replacement.events <- agent.Event{Kind: agent.EventResult, Text: "stop"}

	var msgs []HistoryMessage
	deadline = time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		msgs = reg.History.LoadHistory("shared-sid")
		if len(msgs) >= 2 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if len(msgs) != 2 {
		t.Fatalf("history has %d messages, want 2 (the dead session's fragment, then the real answer): %+v", len(msgs), msgs)
	}
	if msgs[0].Text != "half an ans" {
		t.Errorf("first message = %q, want the dead session's fragment on its own", msgs[0].Text)
	}
	if msgs[1].Text != "the real answer" {
		t.Errorf("second message = %q, want ONLY the replacement's answer — a concatenation here is "+
			"the dead session's half-sentence glued onto the front of the live one", msgs[1].Text)
	}
}

// TestSendPrompt_ReopenSwapsTheEntryBeforeItSendsThePrompt pins an ORDERING two
// separate correctness arguments rest on, and that nothing else can see.
//
// The subscriber set lives on the Entry, and a prompt is what makes cc emit. Sending
// before the swap leaves a window in which the replacement's init and first text
// deltas broadcast to an entry with no subscribers — and broadcast DROPS rather than
// queues, so they are gone and the recovered turn looks exactly like the hang this
// slice exists to end. The fakes cannot catch it by accident, because they only emit
// when a test pushes an event, i.e. always after the send has returned.
//
// The same order is what makes the ownership carry effective (nothing can be
// streaming while the entry is still unowned), so it is load-bearing twice.
func TestSendPrompt_ReopenSwapsTheEntryBeforeItSendsThePrompt(t *testing.T) {
	reused := &fakeSession{sid: "reused-sid", events: make(chan agent.Event, 8)}
	inner := &fakeSession{sid: "reused-sid", events: make(chan agent.Event, 8)}
	probe := &probeSession{fakeSession: inner}
	p := &reopenProvider{first: reused, second: probe}
	reg, att := seedReuse(t, p, "reused-sid")

	ch := make(chan wire.Envelope, 8)
	att.Subscribe(ch)
	defer att.Unsubscribe(ch)
	corpse := attEntry(att)

	var boundAtSend *Entry
	subsAtSend := -1
	ownerAtSend := "not-recorded"
	probe.onSend = func() {
		boundAtSend = attEntry(att)
		// spawnEntry publishes the replacement under the sid before Spawn returns, so
		// it is findable from here — which is what lets this observe the ENTRY's state
		// at send time rather than the attachment's alone.
		if fresh := registeredEntry(reg, "reused-sid"); fresh != nil {
			subsAtSend = subCount(fresh)
			ownerAtSend = fresh.owner()
		}
	}

	if !att.SetOwner("client-1") {
		t.Fatal("ownership claim was rejected")
	}
	reused.kill()
	if err := att.SendPrompt(context.Background(), "still there?"); err != nil {
		t.Fatalf("SendPrompt: %v", err)
	}

	if boundAtSend == corpse {
		t.Error("the prompt was delivered while the attachment still pointed at the corpse: the " +
			"replacement's init and first deltas would broadcast to nobody")
	}
	if boundAtSend != attEntry(att) {
		t.Errorf("the attachment was re-pointed after the send, not before")
	}
	if subsAtSend != 1 {
		t.Errorf("the replacement had %d subscribers when the prompt was sent, want 1: broadcast "+
			"drops rather than queues, so anything it emits before the move is lost", subsAtSend)
	}
	if ownerAtSend != "client-1" {
		t.Errorf("the replacement's owner at send time = %q, want \"client-1\": an answer must not "+
			"stream while the session is still unowned", ownerAtSend)
	}
}

// TestSendPrompt_ReopenUnsubscribesFromTheCorpse — the other half of the swap, and
// the one with a visible symptom.
//
// The corpse's readLoop may still be draining bytes the pipe buffered before the
// process died — the same window reopen's no-reap argument turns on — and its
// fanOut now also broadcasts a turn-ender when that stream ends. Left subscribed,
// that stale tail arrives at the browser AFTER the replacement has started
// answering: the dead session's half-sentence interleaved into the live one, and a
// turn-ender that closes a turn which is still streaming.
func TestSendPrompt_ReopenUnsubscribesFromTheCorpse(t *testing.T) {
	reused := &fakeSession{sid: "reused-sid", events: make(chan agent.Event, 8)}
	reopened := &fakeSession{sid: "reused-sid", events: make(chan agent.Event, 8)}
	p := &reopenProvider{first: reused, second: reopened}
	_, att := seedReuse(t, p, "reused-sid")

	ch := make(chan wire.Envelope, 16)
	att.Subscribe(ch)
	defer att.Unsubscribe(ch)

	reused.kill()
	if err := att.SendPrompt(context.Background(), "still there?"); err != nil {
		t.Fatalf("SendPrompt: %v", err)
	}

	// What readLoop was still draining when the process died, then the stream close
	// that runs the corpse's fanOut to its end (turn-ender included).
	reused.events <- agent.Event{Kind: agent.EventText, Text: "stale half-answer from the corpse\n"}
	close(reused.events)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && reused.Closes() == 0 {
		time.Sleep(time.Millisecond)
	}
	if reused.Closes() == 0 {
		t.Fatal("the corpse's fanOut never finished; the assertion below would be vacuous")
	}

	if n := len(ch); n != 0 {
		var got []wire.Envelope
		for len(ch) > 0 {
			got = append(got, <-ch)
		}
		t.Errorf("the browser received %d envelope(s) from the corpse after the recovery: %+v", n, got)
	}
}

// TestSendPrompt_UnrecoverableReopenIsClassifiedAndARecoverableSendIsNot — the
// discriminator serveChat uses to decide which SendPrompt failures the browser has
// to be told about.
//
// A failed-resume send must stay unclassified: Resolve is about to replay it, so
// surfacing an error would show one for a turn that then answers normally. A
// re-open the daemon could not complete must be classified: nothing will retry it,
// the budget is spent, and if it only reached the log the user would sit on a
// spinner while every later prompt failed silently.
func TestSendPrompt_UnrecoverableReopenIsClassifiedAndARecoverableSendIsNot(t *testing.T) {
	t.Run("failed resume → unclassified, the caller logs and Resolve recovers", func(t *testing.T) {
		dp := &deadThenLiveProvider{dead: newDeadSession(), live: &fakeSession{sid: "recovered-sid", events: make(chan agent.Event, 8)}}
		reg := NewRegistry(dp)
		att, err := reg.Attach(context.Background(), "gone-sid", "fake", "")
		if err != nil {
			t.Fatalf("Attach: %v", err)
		}
		err = att.SendPrompt(context.Background(), "remember ALPHA")
		if err == nil {
			t.Fatal("SendPrompt to a dead resume returned nil")
		}
		var ref *Refusal
		if errors.As(err, &ref) {
			t.Errorf("error %v carries a Refusal (%q); serveChat would surface an error envelope for "+
				"a prompt Resolve is about to replay successfully", err, ref.Code)
		}
	})

	t.Run("re-open that cannot spawn → classified, so the browser hears about it", func(t *testing.T) {
		reused := &fakeSession{sid: "reused-sid", events: make(chan agent.Event, 8)}
		p := &reopenProvider{
			first:    reused,
			second:   &fakeSession{sid: "reused-sid", events: make(chan agent.Event, 8)},
			spawnErr: errors.New(`exec: "cc": executable file not found in $PATH`),
		}
		_, att := seedReuse(t, p, "reused-sid")
		reused.kill()
		err := att.SendPrompt(context.Background(), "still there?")
		if err == nil {
			t.Fatal("SendPrompt returned nil when the re-open could not spawn")
		}
		var ref *Refusal
		if !errors.As(err, &ref) {
			t.Fatalf("error %v (%T) carries no Refusal; serveChat cannot tell it from a send that "+
				"recovers itself, so the user gets a silent spinner forever", err, err)
		}
		if ref.Code != wire.ErrCodeSpawnFailed {
			t.Errorf("code = %q, want %q", ref.Code, wire.ErrCodeSpawnFailed)
		}
		if ref.Code.Terminal() {
			t.Error("ErrCodeSpawnFailed must stay retryable: a reconnect re-resumes and may well work")
		}
	})
}

// TestEntryOwner_ReadIsGuardedAgainstAConcurrentClaim — Entry.owner() reads a field
// serveChat's goroutine writes through setOwner while the prompt reader reads it
// through Attachment.reopen. That is an ordinary two-goroutine overlap, not an
// exotic one, so the read takes subsMu; this fails under -race without it.
func TestEntryOwner_ReadIsGuardedAgainstAConcurrentClaim(t *testing.T) {
	e := &Entry{subs: make(map[chan wire.Envelope]struct{})}
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			for j := 0; j < 200; j++ {
				if i%2 == 0 {
					e.setOwner("client-1")
				} else {
					_ = e.owner()
				}
			}
		}(i)
	}
	close(start)
	wg.Wait()
	if got := e.owner(); got != "client-1" {
		t.Errorf("owner() = %q, want \"client-1\"", got)
	}
}

// TestSendPrompt_ReopensEvenWhileTheDeadSessionStillReportsItselfAlive pins the
// design decision reopen's doc argues for, which a mutation run showed was
// unpinned: the trigger is the SendPrompt error, never Alive().
//
// This is the ordinary cc shape, not a corner: the write to a dead process's stdin
// fails as soon as the kernel closes its end, while Alive() answers from `done`,
// which readLoop closes only when it NOTICES the exit (ccSession.Alive says so in
// as many words). So a recovery gated on !Alive() would decline in exactly the
// window the failure is discovered in, and the turn would hang — the bug, reached
// through the fix.
//
// It is also what makes the `sibling != dead` test in the adoption branch
// load-bearing: here liveEntry DOES return the corpse, because it is still
// registered and still calls itself alive, and adopting it would send the prompt
// straight back into the broken pipe.
func TestSendPrompt_ReopensEvenWhileTheDeadSessionStillReportsItselfAlive(t *testing.T) {
	reused := &fakeSession{sid: "reused-sid", events: make(chan agent.Event, 8)}
	reopened := &fakeSession{sid: "reused-sid", events: make(chan agent.Event, 8)}
	p := &reopenProvider{first: reused, second: reopened}
	reg, att := seedReuse(t, p, "reused-sid")

	// The pipe is broken but `done` has not closed: Alive() still says true.
	reused.sendFails.Store(true)
	if !reused.Alive() {
		t.Fatal("the double is not staging the window: Alive() must still be true here")
	}
	if _, ok := reg.liveEntry("reused-sid"); !ok {
		t.Fatal("the corpse must still be registered AND live for this test to mean anything")
	}

	if err := att.SendPrompt(context.Background(), "still there?"); err != nil {
		t.Fatalf("SendPrompt = %v; recovery must not wait for Alive() to flip, or the turn hangs "+
			"for exactly as long as readLoop takes to notice", err)
	}
	if got := p.Spawns(); got != 2 {
		t.Errorf("spawns = %d, want 2: the send error alone must trigger the re-open", got)
	}
	if got := reopened.Prompts(); len(got) != 1 || got[0] != "still there?" {
		t.Errorf("replacement prompts = %v, want [\"still there?\"]", got)
	}
	if got := attEntry(att); got == registeredEntryByValue(reg, reused) {
		t.Error("the attachment re-adopted its own corpse instead of replacing it")
	}
}

// registeredEntryByValue returns the entry whose session is sess, or nil.
func registeredEntryByValue(reg *Registry, sess agent.Session) *Entry {
	reg.mu.RLock()
	defer reg.mu.RUnlock()
	for _, e := range reg.sessions {
		if e.sess == sess {
			return e
		}
	}
	return nil
}

// TestSendPrompt_ASpentAttachmentStillAdoptsALiveReplacement — the ordering inside
// reopen: adoption is checked BEFORE the budget, because adoption spawns nothing.
//
// Two tabs, and the agent dies twice. Tab 1 recovers the first death (its budget is
// now spent). Tab 2 recovers the second. Tab 1's next prompt then finds its own entry
// dead and its budget gone — but there is a live session under that sid, right there,
// which tab 2 started. Refusing it because of a budget that exists to bound SPAWNING
// would lose a prompt a healthy agent was ready to answer.
func TestSendPrompt_ASpentAttachmentStillAdoptsALiveReplacement(t *testing.T) {
	s1 := &fakeSession{sid: "reused-sid", events: make(chan agent.Event, 8)}
	s2 := &fakeSession{sid: "reused-sid", events: make(chan agent.Event, 8)}
	s3 := &fakeSession{sid: "reused-sid", events: make(chan agent.Event, 8)}
	p := &reopenProvider{first: s1, second: s2, third: s3}
	reg, tab1 := seedReuse(t, p, "reused-sid")
	tab2, err := reg.Attach(context.Background(), "reused-sid", "fake", "")
	if err != nil {
		t.Fatalf("second Attach: %v", err)
	}

	// First death: tab 1 re-opens and spends its budget.
	s1.kill()
	if err := tab1.SendPrompt(context.Background(), "tab1 first"); err != nil {
		t.Fatalf("tab1 first send: %v", err)
	}
	// Second death: tab 2 re-opens (its own budget was never spent — it adopted).
	s2.kill()
	if err := tab2.SendPrompt(context.Background(), "tab2 recovers"); err != nil {
		t.Fatalf("tab2 send: %v", err)
	}
	if got := p.Spawns(); got != 3 {
		t.Fatalf("spawns = %d, want 3 (reuse, tab1's re-open, tab2's re-open)", got)
	}

	// Tab 1: entry dead, budget spent, but a live session exists under the sid.
	tab1.mu.Lock()
	spent := tab1.reopenSpent
	tab1.mu.Unlock()
	if !spent {
		t.Fatal("tab1's budget is not spent; this test proves nothing about the ordering")
	}
	if err := tab1.SendPrompt(context.Background(), "tab1 again"); err != nil {
		t.Errorf("SendPrompt = %v; a spent budget bounds SPAWNING, and adopting the live session "+
			"another attachment already started spawns nothing", err)
	}
	if got := p.Spawns(); got != 3 {
		t.Errorf("spawns = %d, want 3: adopting must not spawn", got)
	}
	if got := s3.Prompts(); len(got) != 2 || got[1] != "tab1 again" {
		t.Errorf("live session prompts = %v, want tab2's then tab1's", got)
	}
}
