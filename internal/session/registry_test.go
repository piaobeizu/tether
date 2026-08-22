package session

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/piaobeizu/tether/internal/agent"
	"github.com/piaobeizu/tether/internal/permission/cchook"
	"github.com/piaobeizu/tether/internal/wire"
)

// fakeSession is a minimal agent.Session for driving Registry.fanOut in
// tests without a real cc subprocess. SendPrompt calls are captured
// (Prompts) rather than discarded so DeliverAction tests (tether#8 T8) can
// assert on exactly what was delivered. Interrupt calls are similarly
// counted (InterruptCalls) so InterruptSession tests (tether#8 T9) can
// assert the call reached the session without a real cc process to observe.
type fakeSession struct {
	sid    string
	events chan agent.Event
	// sidReady, when non-nil, gates SessionID() until it is closed — mimics
	// ccSession.SessionID() blocking on system/init. It exists so a test can choose
	// the instant Resolve is released, which is how the tether#50 ownership race is
	// staged deterministically. Nil (the default) returns the sid immediately,
	// preserving every other test's behavior.
	sidReady chan struct{}
	// dead, when set, makes Alive() report false while SessionID() keeps handing
	// back the cached sid — the registered-corpse state tether#55 is about. It is
	// an atomic because a test flips it from one goroutine while the registry
	// reads it from another (and -race would otherwise flag the reuse tests).
	dead atomic.Bool
	// sendFails, when set, makes SendPrompt refuse like the broken pipe a write to
	// an exited cc's stdin produces, WITHOUT the session losing its cached sid —
	// the mid-turn death tether#59 recovers from. Atomic for the same reason as
	// dead. Default false, so every test written before it keeps describing a
	// session that accepts prompts.
	//
	// A refused prompt is counted (Refused) rather than appended to prompts: the
	// write failed, so the agent never saw it, and a test asking "what did this
	// session receive" must not be told about a prompt that went nowhere.
	sendFails atomic.Bool
	// aliveHold, when non-nil, makes the FIRST Alive() call announce itself on
	// aliveEntered and then block until aliveHold is closed. It is the injection point
	// tether#78 needs and the only one available: a caller has to be held BETWEEN its
	// own liveness check and spawnEntry's claim, and liveEntry is the single function
	// on that path that calls out of the registry.
	//
	// Blocking inside Alive() is what makes such a staging airtight rather than timed.
	// liveEntry looks the sid up BEFORE it asks, so a goroutine parked here is
	// provably holding the entry it found, and when it is released it can only act on
	// THAT entry — it cannot quietly take a reuse branch on whatever registered while
	// it was stopped, which is the false green this staging would otherwise produce.
	//
	// First call only, so a second caller staged behind the first is not held as well.
	// Nil (the default) leaves Alive() exactly as every other test expects it.
	aliveHold    chan struct{}
	aliveEntered chan struct{}
	aliveHeld    atomic.Bool

	mu             sync.Mutex
	prompts        []string
	interruptCalls int
	// refused counts SendPrompt calls rejected because sendFails was set.
	refused int
	// closes counts Close() calls so a test can assert the session's agent was
	// REAPED on teardown and not merely un-registered (tether#56). Counted rather
	// than flagged because "exactly once" is the property that matters: fanOut's
	// teardown and Attachment.resolve can both reach a given session, and the real
	// ccSession absorbs the second call in a sync.Once — a double call here would
	// mean the registry is relying on that absorption instead of owning the reap.
	closes int
}

func (f *fakeSession) SessionID() string {
	if f.sidReady != nil {
		<-f.sidReady
	}
	return f.sid
}

// Alive defaults to TRUE for the zero value, so every pre-tether#55 test that
// builds a fakeSession keeps describing a healthy session. (A double that
// defaulted to dead would silently gut the existing suite rather than fail it.)
//
// Deliberately NOT derived from `close(f.events)`, even though the real
// ccSession's liveness and its stream close are two halves of one event: tests
// here close a fake's events channel to make fanOut return and evict, so
// coupling the two would make "evicted ⟹ Alive() false" true by construction
// and turn the registered-but-dead tests into tautologies. Keeping the flag
// independent is what lets a test hold an entry REGISTERED while its session
// reports dead — the state the fix is about, which cannot otherwise be staged.
func (f *fakeSession) Alive() bool {
	if f.aliveHold != nil && f.aliveHeld.CompareAndSwap(false, true) {
		if f.aliveEntered != nil {
			f.aliveEntered <- struct{}{}
		}
		<-f.aliveHold
	}
	return !f.dead.Load()
}
func (f *fakeSession) SendPrompt(_ context.Context, text string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.sendFails.Load() {
		f.refused++
		return errBrokenPipe
	}
	f.prompts = append(f.prompts, text)
	return nil
}

// kill puts this session in the state an exited cc leaves behind MID-TURN
// (tether#59): Alive() reports false, SendPrompt fails with the broken pipe a
// write to a dead process's stdin produces, and SessionID() keeps handing back the
// id it cached at init.
//
// Both flags together, because that is the only combination a real provider
// produces once its process is gone, and each half alone models something that
// cannot happen for long: a corpse that still takes prompts, or a dead process
// that reports itself healthy forever. (Tests that only need the REUSE gate's view
// still set dead alone — they never send to the corpse, so its stdin never
// matters.)
//
// It deliberately does NOT close the event stream. The entry's fanOut is what
// drains that, and a test held inside "the process is gone, its stream has not
// closed yet" is what makes a Close() attributable — see deadSession.dying for the
// same trick on the failed-resume path.
func (f *fakeSession) kill() {
	f.dead.Store(true)
	f.sendFails.Store(true)
}

// Refused returns how many SendPrompt calls this session rejected.
func (f *fakeSession) Refused() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.refused
}
func (f *fakeSession) Events() <-chan agent.Event { return f.events }
func (f *fakeSession) Interrupt() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.interruptCalls++
	return nil
}
func (f *fakeSession) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closes++
	return nil
}

// Closes returns how many times Close() has been called on this session.
func (f *fakeSession) Closes() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.closes
}

// announceInit emits the system/init this fake's sid arrives on.
//
// The fake models a provider that MINTS ITS OWN id (opencode: Spawn ignores
// SpawnConfig.SessionID and the id shows up later on the event stream), which is
// the only shape that still needs Registry.rekey since tether#54. So a test that
// wants to find this session by its hard-coded sid must let it announce that sid
// first — the registry keyed the entry under the id it PINNED at spawn, and
// nothing but this event tells it otherwise. Real cc needs no equivalent: it
// adopts the pinned id, so the key is right from the start.
func (f *fakeSession) announceInit() {
	f.events <- agent.Event{Kind: agent.EventInit, SessionID: f.sid}
}

// Prompts returns a snapshot of every string passed to SendPrompt so far.
func (f *fakeSession) Prompts() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.prompts...)
}

// InterruptCalls returns how many times Interrupt() has been called so far.
func (f *fakeSession) InterruptCalls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.interruptCalls
}

// fakeProvider hands back a pre-built fakeSession regardless of SpawnConfig,
// but records the last SpawnConfig it received so tests can assert what the
// registry asked for (e.g. ResumeSessionID — tether#49).
type fakeProvider struct {
	sess    *fakeSession
	lastCfg agent.SpawnConfig
	spawns  int
}

func (p *fakeProvider) Name() string { return "fake" }
func (p *fakeProvider) Spawn(_ context.Context, cfg agent.SpawnConfig) (agent.Session, error) {
	p.lastCfg = cfg
	p.spawns++
	return p.sess, nil
}

// erroringProvider always fails to spawn, for exercising spawnEntry's
// ErrCodeSpawnFailed refusal path (tether#63) — none of the other doubles in
// this file ever return an error from Spawn.
type erroringProvider struct{}

func (p *erroringProvider) Name() string { return "fake" }
func (p *erroringProvider) Spawn(_ context.Context, _ agent.SpawnConfig) (agent.Session, error) {
	return nil, errors.New(`exec: "cc": executable file not found in $PATH`)
}

// TestSpawnEntry_RefusalCodes pins the two classified refusals spawnEntry can
// produce (tether#63): an unregistered provider name (terminal — the set of
// registered providers is fixed at daemon startup) and the registered
// provider's own Spawn failing (retryable — a transient exec failure can
// succeed on the very next attempt).
func TestSpawnEntry_RefusalCodes(t *testing.T) {
	reg := NewRegistry(&erroringProvider{})

	_, _, err := reg.spawnEntry(context.Background(), "not-registered", agent.SpawnConfig{}, WorkspaceBinding{}, spawnIfAbsent)
	if err == nil {
		t.Fatal("spawnEntry with an unregistered provider name returned no error")
	}
	var ref *Refusal
	if !errors.As(err, &ref) {
		t.Fatalf("error %v (%T) is not a *Refusal", err, err)
	}
	if ref.Code != wire.ErrCodeUnknownProvider {
		t.Errorf("code = %q, want %q", ref.Code, wire.ErrCodeUnknownProvider)
	}
	if !ref.Code.Terminal() {
		t.Error("ErrCodeUnknownProvider must be terminal")
	}

	_, _, err = reg.spawnEntry(context.Background(), "fake", agent.SpawnConfig{}, WorkspaceBinding{}, spawnIfAbsent)
	if err == nil {
		t.Fatal("spawnEntry returned no error when the provider's Spawn failed")
	}
	ref = nil
	if !errors.As(err, &ref) {
		t.Fatalf("error %v (%T) is not a *Refusal", err, err)
	}
	if ref.Code != wire.ErrCodeSpawnFailed {
		t.Errorf("code = %q, want %q", ref.Code, wire.ErrCodeSpawnFailed)
	}
	if ref.Code.Terminal() {
		t.Error("ErrCodeSpawnFailed must be retryable, not terminal")
	}
}

// TestGetOrSpawnEntry_StaleSidSpawnsFresh — a sid the registry does NOT track
// (daemon restart / post-disconnect eviction / different workspace-cwd) must
// spawn a FRESH session, never `cc --resume <sid>`. Resuming a dead sid makes
// cc exit with "No conversation found" before emitting system/init, which
// parked SessionID() forever and broke-pipe the first prompt — wedging the turn
// in "thinking…" (tether#49). Assert the provider got an empty ResumeSessionID.
func TestGetOrSpawnEntry_StaleSidSpawnsFresh(t *testing.T) {
	fp := &fakeProvider{sess: &fakeSession{sid: "fresh-sid", events: make(chan agent.Event, 8)}}
	reg := NewRegistry(fp)
	if _, err := reg.GetOrSpawnEntry(context.Background(), "stale-dead-sid", "fake"); err != nil {
		t.Fatalf("GetOrSpawnEntry: %v", err)
	}
	if fp.lastCfg.ResumeSessionID != "" {
		t.Errorf("Spawn ResumeSessionID = %q, want \"\" (must not cc --resume a stale sid)", fp.lastCfg.ResumeSessionID)
	}
}

// TestGetOrSpawnEntry_LiveSidReused — a sid the registry DOES track returns the
// existing entry without spawning again (the real reconnect-continuity path,
// unchanged by tether#49: live reconnect reuses the running cc, never resumes).
func TestGetOrSpawnEntry_LiveSidReused(t *testing.T) {
	fp := &fakeProvider{sess: &fakeSession{sid: "live-sid", events: make(chan agent.Event, 8)}}
	reg := NewRegistry(fp)
	e1, err := reg.GetOrSpawnEntry(context.Background(), "", "fake")
	if err != nil {
		t.Fatalf("first spawn: %v", err)
	}
	fp.sess.announceInit()
	waitForRegistered(t, reg, "live-sid")
	e2, err := reg.GetOrSpawnEntry(context.Background(), "live-sid", "fake")
	if err != nil {
		t.Fatalf("reuse: %v", err)
	}
	if e1 != e2 {
		t.Error("a live sid must reuse the existing entry, not create a new one")
	}
	if fp.spawns != 1 {
		t.Errorf("provider.spawns = %d, want 1 (a live sid must not respawn)", fp.spawns)
	}
}

// TestGetOrSpawnEntry_RegisteredButDeadSpawnsFresh — tether#55 on the entry point
// that never resumes. A registered sid whose agent has exited must NOT be handed
// back: this path's contract is always-fresh (tether#49), so the corpse is
// dropped and a brand-new session spawned, with ResumeSessionID still empty.
//
// Reusing it here is the same wedge as on the Attach path but with no recovery at
// all — GetOrSpawnEntry has no prompt buffer to replay, which is exactly why it
// must not adopt a session it cannot verify.
func TestGetOrSpawnEntry_RegisteredButDeadSpawnsFresh(t *testing.T) {
	corpse := &fakeSession{sid: "dead-sid", events: make(chan agent.Event, 8)}
	fp := &fakeProvider{sess: corpse}
	reg := NewRegistry(fp)

	e1, err := reg.GetOrSpawnEntry(context.Background(), "", "fake")
	if err != nil {
		t.Fatalf("seed spawn: %v", err)
	}
	corpse.announceInit()
	waitForRegistered(t, reg, "dead-sid")
	corpse.dead.Store(true)

	e2, err := reg.GetOrSpawnEntry(context.Background(), "dead-sid", "fake")
	if err != nil {
		t.Fatalf("GetOrSpawnEntry: %v", err)
	}
	if e2 == e1 {
		t.Error("returned the dead entry; a registered sid whose agent exited must not be reused")
	}
	if fp.spawns != 2 {
		t.Errorf("provider.spawns = %d, want 2 (seed + the replacement)", fp.spawns)
	}
	if fp.lastCfg.ResumeSessionID != "" {
		t.Errorf("ResumeSessionID = %q, want \"\": this entry point never resumes (tether#49)",
			fp.lastCfg.ResumeSessionID)
	}
	if fp.lastCfg.SessionID == "" {
		t.Error("SessionID not pinned on the replacement spawn; a fresh session must mint one")
	}
}

// TestIsLive_FalseOnceTheAgentExits — IsLive answers the question its name asks.
// serveChat's ownership gate is its one production caller, and a corpse reporting
// "live" there consults the owner recorded on the dead entry: a second device
// reconnecting to a sid whose agent had exited was rejected outright instead of
// recovering the conversation.
func TestIsLive_FalseOnceTheAgentExits(t *testing.T) {
	fs := &fakeSession{sid: "islive-sid", events: make(chan agent.Event, 4)}
	reg := NewRegistry(&fakeProvider{sess: fs})

	if _, err := reg.GetOrSpawnEntry(context.Background(), "", "fake"); err != nil {
		t.Fatalf("GetOrSpawnEntry: %v", err)
	}
	fs.announceInit()
	waitForRegistered(t, reg, "islive-sid") // this itself asserts IsLive is true
	if !reg.IsLive("islive-sid") {
		t.Fatal("IsLive = false for a healthy registered session")
	}

	fs.dead.Store(true)
	if reg.IsLive("islive-sid") {
		t.Error("IsLive = true for a registered session whose agent has exited")
	}
	if reg.IsLive("never-existed") {
		t.Error("IsLive = true for a sid that was never registered")
	}
}

// TestGetOrSpawnEntry_PassesRegistryWorkdir — (tether#51) lifecycle.go wires
// the resolved workspace root onto Registry.Workdir once it's known (Step
// 3b); GetOrSpawnEntry must forward it into SpawnConfig.Workdir so the agent
// subprocess's cwd matches the workspace, not the daemon's own.
func TestGetOrSpawnEntry_PassesRegistryWorkdir(t *testing.T) {
	fp := &fakeProvider{sess: &fakeSession{sid: "sid-workdir", events: make(chan agent.Event, 4)}}
	reg := NewRegistry(fp)
	reg.Workdir = "/some/workspace"

	if _, err := reg.GetOrSpawnEntry(context.Background(), "", "fake"); err != nil {
		t.Fatalf("GetOrSpawnEntry: %v", err)
	}
	if fp.lastCfg.Workdir != "/some/workspace" {
		t.Errorf("Spawn Workdir = %q, want %q", fp.lastCfg.Workdir, "/some/workspace")
	}
}

// TestGetOrSpawnEntry_UnsetRegistryWorkdirYieldsEmpty — an unset
// Registry.Workdir must pass through as "" — the empty-Workdir fallback to
// the process cwd is owned by resolveWorkdir at the provider level (tether#51),
// not the registry.
func TestGetOrSpawnEntry_UnsetRegistryWorkdirYieldsEmpty(t *testing.T) {
	fp := &fakeProvider{sess: &fakeSession{sid: "sid-workdir2", events: make(chan agent.Event, 4)}}
	reg := NewRegistry(fp)

	if _, err := reg.GetOrSpawnEntry(context.Background(), "", "fake"); err != nil {
		t.Fatalf("GetOrSpawnEntry: %v", err)
	}
	if fp.lastCfg.Workdir != "" {
		t.Errorf("Spawn Workdir = %q, want \"\" (registry must not fall back itself)", fp.lastCfg.Workdir)
	}
}

// TestRegistry_FencedBlockSuppressedFromMessageAndHistory drives a session
// through the registry's real fanOut (no direct FenceParser calls) and
// asserts: (1) a KindFenced envelope carries the extracted block, (2) the
// raw fence marker/JSON never appears in any KindMessage envelope, and (3)
// HistoryStore ends up with the SUPPRESSED text, not the raw fence text.
func TestRegistry_FencedBlockSuppressedFromMessageAndHistory(t *testing.T) {
	fs := &fakeSession{sid: "sid1", events: make(chan agent.Event, 64)}
	reg := NewRegistry(&fakeProvider{sess: fs})
	reg.History = NewHistoryStore(t.TempDir())

	entry, err := reg.GetOrSpawnEntry(context.Background(), "", "fake")
	if err != nil {
		t.Fatalf("GetOrSpawnEntry: %v", err)
	}

	subCh := make(chan wire.Envelope, 16)
	entry.Subscribe(subCh)

	// Simulate a turn: plain text, then a fenced dag block on its own lines,
	// then trailing text with no final newline (a realistic last delta),
	// then turn-end.
	fs.events <- agent.Event{Kind: agent.EventInit, SessionID: "sid1"}
	fs.events <- agent.Event{Kind: agent.EventText, Text: "before text\n"}
	fs.events <- agent.Event{Kind: agent.EventText, Text: "```dag:s\n{\"a\":1}\n```\n"}
	fs.events <- agent.Event{Kind: agent.EventText, Text: "after text"}
	fs.events <- agent.Event{Kind: agent.EventResult, Text: "stop"}
	close(fs.events)

	var envs []wire.Envelope
	timeout := time.After(2 * time.Second)
collect:
	for {
		select {
		case env := <-subCh:
			envs = append(envs, env)
			if env.Kind == wire.KindResult {
				break collect
			}
		case <-timeout:
			t.Fatal("timed out waiting for envelopes")
		}
	}

	var messages []string
	var fenced []wire.FencedBlock
	for _, env := range envs {
		switch env.Kind {
		case wire.KindMessage:
			if s, ok := env.Payload.(string); ok {
				messages = append(messages, s)
			}
		case wire.KindFenced:
			if fb, ok := env.Payload.(wire.FencedBlock); ok {
				fenced = append(fenced, fb)
			}
		}
	}

	joined := strings.Join(messages, "")
	if joined != "before text\nafter text" {
		t.Errorf("KindMessage text = %q, want %q", joined, "before text\nafter text")
	}
	if strings.Contains(joined, "dag:s") || strings.Contains(joined, `"a":1`) {
		t.Errorf("raw fence text leaked into KindMessage stream: %q", joined)
	}

	if len(fenced) != 1 {
		t.Fatalf("len(fenced) = %d, want 1", len(fenced))
	}
	if fenced[0].Kind != wire.FencedBlockDag || fenced[0].Skill != "s" || fenced[0].Content != `{"a":1}` {
		t.Errorf("fenced block = %+v, want {dag s {\"a\":1} s-0}", fenced[0])
	}
	if fenced[0].BlockID != "s-0" {
		t.Errorf("BlockID = %q, want s-0", fenced[0].BlockID)
	}

	// History must contain the SUPPRESSED text (fence removed), never the
	// raw fence marker/JSON — the KindResult receive above happens-after
	// FinalizeAssistant runs (same goroutine, program order before the send).
	//
	// Text before and after the block now persist as SEPARATE ordered
	// entries (tether#8 T7: the block is flushed as its own history entry
	// in between, see AppendBlock), so concatenate every assistant-role
	// entry's Text in order rather than expecting a single merged message.
	msgs := reg.History.LoadHistory("sid1")
	var assistantText string
	for _, m := range msgs {
		if m.Role == "assistant" {
			assistantText += m.Text
		}
	}
	if assistantText != "before text\nafter text" {
		t.Errorf("history assistant text = %q, want %q", assistantText, "before text\nafter text")
	}
	if strings.Contains(assistantText, "dag:s") || strings.Contains(assistantText, `"a":1`) {
		t.Errorf("raw fence text leaked into history: %q", assistantText)
	}
}

// TestRegistry_SegmentOrderPreservedAcrossBroadcast drives text-before,
// block, text-after through the real fanOut in a SINGLE EventText delta
// (mirroring how a fenced block that opens and closes within one stream
// chunk actually arrives) and asserts the broadcast envelopes preserve
// exact stream order: KindMessage("before..."), KindFenced, KindMessage
// ("after..."), KindResult — never blocks-then-text or text merged out of
// order (D-19 fix #3, intra-Feed reordering).
func TestRegistry_SegmentOrderPreservedAcrossBroadcast(t *testing.T) {
	fs := &fakeSession{sid: "sid2", events: make(chan agent.Event, 64)}
	reg := NewRegistry(&fakeProvider{sess: fs})

	entry, err := reg.GetOrSpawnEntry(context.Background(), "", "fake")
	if err != nil {
		t.Fatalf("GetOrSpawnEntry: %v", err)
	}

	subCh := make(chan wire.Envelope, 16)
	entry.Subscribe(subCh)

	fs.events <- agent.Event{Kind: agent.EventInit, SessionID: "sid2"}
	fs.events <- agent.Event{Kind: agent.EventText, Text: "before text\n```dag:s\n{\"x\":1}\n```\nafter text\n"}
	fs.events <- agent.Event{Kind: agent.EventResult, Text: "stop"}
	close(fs.events)

	var envs []wire.Envelope
	timeout := time.After(2 * time.Second)
collect:
	for {
		select {
		case env := <-subCh:
			envs = append(envs, env)
			if env.Kind == wire.KindResult {
				break collect
			}
		case <-timeout:
			t.Fatal("timed out waiting for envelopes")
		}
	}

	var kinds []wire.EnvelopeKind
	for _, e := range envs {
		kinds = append(kinds, e.Kind)
	}
	wantKinds := []wire.EnvelopeKind{wire.KindMessage, wire.KindFenced, wire.KindMessage, wire.KindResult}
	if len(kinds) != len(wantKinds) {
		t.Fatalf("envelope kinds = %v, want %v", kinds, wantKinds)
	}
	for i := range wantKinds {
		if kinds[i] != wantKinds[i] {
			t.Fatalf("envelope kinds = %v, want %v", kinds, wantKinds)
		}
	}
	if s, _ := envs[0].Payload.(string); s != "before text\n" {
		t.Errorf("envs[0].Payload = %q, want %q", s, "before text\n")
	}
	if s, _ := envs[2].Payload.(string); s != "after text\n" {
		t.Errorf("envs[2].Payload = %q, want %q", s, "after text\n")
	}
	fb, ok := envs[1].Payload.(wire.FencedBlock)
	if !ok || fb.Content != `{"x":1}` {
		t.Errorf("envs[1].Payload = %+v, want FencedBlock with content {\"x\":1}", envs[1].Payload)
	}
}

// TestRegistry_EventInitResetsStaleOpenFence drives a turn that opens a
// fence and is then interrupted (no EventResult — Flush never runs),
// followed by a NEW turn's EventInit and plain text. It asserts the new
// turn's text is broadcast normally rather than being swallowed by the
// stale open-fence state left behind by the interrupted turn (D-19 fix #4,
// cross-turn stranding). This exercises fanOut's ResetTurn() call on
// EventInit end-to-end, not just the FenceParser unit directly.
func TestRegistry_EventInitResetsStaleOpenFence(t *testing.T) {
	fs := &fakeSession{sid: "sid3", events: make(chan agent.Event, 64)}
	reg := NewRegistry(&fakeProvider{sess: fs})

	entry, err := reg.GetOrSpawnEntry(context.Background(), "", "fake")
	if err != nil {
		t.Fatalf("GetOrSpawnEntry: %v", err)
	}

	subCh := make(chan wire.Envelope, 16)
	entry.Subscribe(subCh)

	// Turn 1: open a fence, then get interrupted — no EventResult follows.
	fs.events <- agent.Event{Kind: agent.EventInit, SessionID: "sid3"}
	fs.events <- agent.Event{Kind: agent.EventText, Text: "```dag:s\n{\"partial\":true\n"}
	// Turn 2: a fresh system/init (same session id, per-turn metadata
	// refresh) followed by ordinary text and a clean turn-end.
	fs.events <- agent.Event{Kind: agent.EventInit, SessionID: "sid3"}
	fs.events <- agent.Event{Kind: agent.EventText, Text: "hello world\n"}
	fs.events <- agent.Event{Kind: agent.EventResult, Text: "stop"}
	close(fs.events)

	var envs []wire.Envelope
	timeout := time.After(2 * time.Second)
collect:
	for {
		select {
		case env := <-subCh:
			envs = append(envs, env)
			if env.Kind == wire.KindResult {
				break collect
			}
		case <-timeout:
			t.Fatal("timed out waiting for envelopes")
		}
	}

	var messages []string
	for _, env := range envs {
		if env.Kind == wire.KindMessage {
			if s, ok := env.Payload.(string); ok {
				messages = append(messages, s)
			}
		}
		if env.Kind == wire.KindFenced {
			t.Errorf("unexpected KindFenced envelope from an interrupted, never-closed fence: %+v", env.Payload)
		}
	}

	joined := strings.Join(messages, "")
	if joined != "hello world\n" {
		t.Errorf("KindMessage text = %q, want %q (turn 2 text must not be swallowed)", joined, "hello world\n")
	}
}

// TestRegistry_BlockPersistedInHistoryOrder — (tether#8 T7) drives a single
// turn of text-before-block-then-text through the real fanOut (same input
// shape as TestRegistry_SegmentOrderPreservedAcrossBroadcast, which asserts
// the LIVE broadcast order) and asserts HistoryStore.LoadHistory returns the
// SAME three entries in the SAME order with the block payload intact — so a
// page reload reconstructs the DAG card exactly where it rendered live,
// instead of losing it (the T6-era bug this task fixes: blocks broadcast
// but never persisted).
func TestRegistry_BlockPersistedInHistoryOrder(t *testing.T) {
	fs := &fakeSession{sid: "sid4", events: make(chan agent.Event, 64)}
	reg := NewRegistry(&fakeProvider{sess: fs})
	reg.History = NewHistoryStore(t.TempDir())

	entry, err := reg.GetOrSpawnEntry(context.Background(), "", "fake")
	if err != nil {
		t.Fatalf("GetOrSpawnEntry: %v", err)
	}

	subCh := make(chan wire.Envelope, 16)
	entry.Subscribe(subCh)

	fs.events <- agent.Event{Kind: agent.EventInit, SessionID: "sid4"}
	fs.events <- agent.Event{Kind: agent.EventText, Text: "before text\n```dag:s\n{\"x\":1}\n```\nafter text\n"}
	fs.events <- agent.Event{Kind: agent.EventResult, Text: "stop"}
	close(fs.events)

	timeout := time.After(2 * time.Second)
collect:
	for {
		select {
		case env := <-subCh:
			if env.Kind == wire.KindResult {
				break collect
			}
		case <-timeout:
			t.Fatal("timed out waiting for envelopes")
		}
	}

	msgs := reg.History.LoadHistory("sid4")
	if len(msgs) != 3 {
		t.Fatalf("len(msgs) = %d, want 3 (text, block, text): %+v", len(msgs), msgs)
	}
	if msgs[0].Role != "assistant" || msgs[0].Text != "before text\n" || msgs[0].Block != nil {
		t.Errorf("msgs[0] = %+v, want assistant/\"before text\\n\", no block", msgs[0])
	}
	if msgs[1].Block == nil {
		t.Fatalf("msgs[1].Block = nil, want a FencedBlock: %+v", msgs[1])
	}
	wantBlock := wire.FencedBlock{Kind: wire.FencedBlockDag, Skill: "s", Content: `{"x":1}`, BlockID: "s-0"}
	if *msgs[1].Block != wantBlock {
		t.Errorf("msgs[1].Block = %+v, want %+v", *msgs[1].Block, wantBlock)
	}
	if msgs[1].Text != "" {
		t.Errorf("msgs[1].Text = %q, want empty (block-only entry)", msgs[1].Text)
	}
	if msgs[2].Role != "assistant" || msgs[2].Text != "after text\n" || msgs[2].Block != nil {
		t.Errorf("msgs[2] = %+v, want assistant/\"after text\\n\", no block", msgs[2])
	}
}

// TestRegistry_TextOnlySessionHistoryUnchanged — (tether#8 T7 regression) a
// turn with no fenced blocks at all must persist exactly as it did before
// this change: a single concatenated assistant history entry, no Block
// field set anywhere.
func TestRegistry_TextOnlySessionHistoryUnchanged(t *testing.T) {
	fs := &fakeSession{sid: "sid5", events: make(chan agent.Event, 64)}
	reg := NewRegistry(&fakeProvider{sess: fs})
	reg.History = NewHistoryStore(t.TempDir())

	entry, err := reg.GetOrSpawnEntry(context.Background(), "", "fake")
	if err != nil {
		t.Fatalf("GetOrSpawnEntry: %v", err)
	}

	subCh := make(chan wire.Envelope, 16)
	entry.Subscribe(subCh)

	reg.RecordUserMessage("sid5", "hello")
	fs.events <- agent.Event{Kind: agent.EventInit, SessionID: "sid5"}
	fs.events <- agent.Event{Kind: agent.EventText, Text: "hi "}
	fs.events <- agent.Event{Kind: agent.EventText, Text: "there\n"}
	fs.events <- agent.Event{Kind: agent.EventResult, Text: "stop"}
	close(fs.events)

	timeout := time.After(2 * time.Second)
collect:
	for {
		select {
		case env := <-subCh:
			if env.Kind == wire.KindResult {
				break collect
			}
		case <-timeout:
			t.Fatal("timed out waiting for envelopes")
		}
	}

	msgs := reg.History.LoadHistory("sid5")
	if len(msgs) != 2 {
		t.Fatalf("len(msgs) = %d, want 2 (user, assistant): %+v", len(msgs), msgs)
	}
	if msgs[0].Role != "user" || msgs[0].Text != "hello" {
		t.Errorf("msgs[0] = %+v, want user/hello", msgs[0])
	}
	if msgs[1].Role != "assistant" || msgs[1].Text != "hi there\n" || msgs[1].Block != nil {
		t.Errorf("msgs[1] = %+v, want assistant/\"hi there\\n\", no block", msgs[1])
	}
}

// TestTranslateEvent_Thinking — (tether#34) EventThinking becomes a KindMessage
// with an object payload {type:"thinking", text:...} (same shape family as
// tool_use), so the frontend distinguishes it from plain assistant text.
func TestTranslateEvent_Thinking(t *testing.T) {
	env := translateEvent(agent.Event{Kind: agent.EventThinking, Text: "pondering"})
	if env == nil {
		t.Fatal("translateEvent(EventThinking) = nil, want a KindMessage envelope")
	}
	if env.Kind != wire.KindMessage {
		t.Errorf("Kind = %q, want %q", env.Kind, wire.KindMessage)
	}
	payload, ok := env.Payload.(map[string]any)
	if !ok {
		t.Fatalf("Payload = %T, want map[string]any", env.Payload)
	}
	if payload["type"] != "thinking" {
		t.Errorf(`payload["type"] = %v, want "thinking"`, payload["type"])
	}
	if payload["text"] != "pondering" {
		t.Errorf(`payload["text"] = %v, want "pondering"`, payload["text"])
	}
}

// TestTranslateEvent_ToolResult — (tether#38) a tool_result event becomes a
// KindMessage{type:"tool_result", tool_use_id, content, is_error} so the
// frontend can hang the result under its matching tool_use row.
func TestTranslateEvent_ToolResult(t *testing.T) {
	env := translateEvent(agent.Event{Kind: agent.EventToolResult, ToolResult: &agent.ToolResultEvent{
		ToolUseID: "toolu_7", Content: "142 lines", IsError: false,
	}})
	if env == nil {
		t.Fatal("translateEvent(EventToolResult) = nil, want a KindMessage envelope")
	}
	if env.Kind != wire.KindMessage {
		t.Errorf("Kind = %q, want %q", env.Kind, wire.KindMessage)
	}
	payload, ok := env.Payload.(map[string]any)
	if !ok {
		t.Fatalf("Payload = %T, want map[string]any", env.Payload)
	}
	if payload["type"] != "tool_result" {
		t.Errorf(`payload["type"] = %v, want "tool_result"`, payload["type"])
	}
	if payload["tool_use_id"] != "toolu_7" {
		t.Errorf(`payload["tool_use_id"] = %v, want "toolu_7"`, payload["tool_use_id"])
	}
	if payload["content"] != "142 lines" {
		t.Errorf(`payload["content"] = %v, want "142 lines"`, payload["content"])
	}
	if payload["is_error"] != false {
		t.Errorf(`payload["is_error"] = %v, want false`, payload["is_error"])
	}
}

// TestTranslateEvent_Usage — (tether#48) an EventUsage becomes a
// KindMessage{type:"usage", input, output} object payload (same family as
// thinking/tool_use), and a nil Usage produces no envelope (nil) so a
// malformed event never emits a bogus 0↑/0↓ badge.
func TestTranslateEvent_Usage(t *testing.T) {
	env := translateEvent(agent.Event{Kind: agent.EventUsage, Usage: &agent.UsageEvent{Input: 1234, Output: 856}})
	if env == nil {
		t.Fatal("translateEvent(EventUsage) = nil, want a KindMessage envelope")
	}
	if env.Kind != wire.KindMessage {
		t.Errorf("Kind = %q, want %q", env.Kind, wire.KindMessage)
	}
	payload, ok := env.Payload.(map[string]any)
	if !ok {
		t.Fatalf("Payload = %T, want map[string]any", env.Payload)
	}
	if payload["type"] != "usage" {
		t.Errorf(`payload["type"] = %v, want "usage"`, payload["type"])
	}
	if payload["input"] != 1234 {
		t.Errorf(`payload["input"] = %v, want 1234`, payload["input"])
	}
	if payload["output"] != 856 {
		t.Errorf(`payload["output"] = %v, want 856`, payload["output"])
	}
	if got := translateEvent(agent.Event{Kind: agent.EventUsage, Usage: nil}); got != nil {
		t.Errorf("translateEvent(EventUsage{nil}) = %+v, want nil", got)
	}
}

// TestTranslateEvent_Error — (tether#63) an EventError becomes a classified
// wire.KindError envelope carrying wire.ErrCodeAgent, which is retryable: the
// agent is reporting something about the turn it is mid-way through, not the
// daemon refusing the connection, and the session is still alive.
func TestTranslateEvent_Error(t *testing.T) {
	env := translateEvent(agent.Event{Kind: agent.EventError, Err: errors.New("boom")})
	if env == nil {
		t.Fatal("translateEvent(EventError) = nil, want a KindError envelope")
	}
	if env.Kind != wire.KindError {
		t.Errorf("Kind = %q, want %q", env.Kind, wire.KindError)
	}
	payload, ok := env.Payload.(wire.ErrorPayload)
	if !ok {
		t.Fatalf("Payload = %T, want wire.ErrorPayload", env.Payload)
	}
	if payload.Code != wire.ErrCodeAgent {
		t.Errorf("Code = %q, want %q", payload.Code, wire.ErrCodeAgent)
	}
	if payload.Message != "boom" {
		t.Errorf("Message = %q, want %q", payload.Message, "boom")
	}
	if payload.Terminal {
		t.Error("ErrCodeAgent must be retryable, not terminal")
	}
}

// TestRegistry_ThinkingBroadcastNotPersisted — (tether#34) an EventThinking
// delta must be broadcast to subscribers as a KindMessage{type:"thinking"}
// object payload, must NOT be fence-parsed, and must NOT be accumulated into
// assistant history (thinking stays ephemeral / live-only, spec D3). The
// following EventText is the real answer and IS persisted.
func TestRegistry_ThinkingBroadcastNotPersisted(t *testing.T) {
	fs := &fakeSession{sid: "sid-think", events: make(chan agent.Event, 8)}
	reg := NewRegistry(&fakeProvider{sess: fs})
	reg.History = NewHistoryStore(t.TempDir())

	entry, err := reg.GetOrSpawnEntry(context.Background(), "", "fake")
	if err != nil {
		t.Fatalf("GetOrSpawnEntry: %v", err)
	}
	subCh := make(chan wire.Envelope, 16)
	entry.Subscribe(subCh)

	fs.events <- agent.Event{Kind: agent.EventInit, SessionID: "sid-think"}
	fs.events <- agent.Event{Kind: agent.EventThinking, Text: "let me think "}
	fs.events <- agent.Event{Kind: agent.EventThinking, Text: "about it"}
	fs.events <- agent.Event{Kind: agent.EventText, Text: "the answer\n"}
	fs.events <- agent.Event{Kind: agent.EventResult, Text: "stop"}
	close(fs.events)

	var thinking, answer []string
	timeout := time.After(2 * time.Second)
collect:
	for {
		select {
		case env := <-subCh:
			if env.Kind == wire.KindMessage {
				switch p := env.Payload.(type) {
				case map[string]any:
					if p["type"] == "thinking" {
						if s, ok := p["text"].(string); ok {
							thinking = append(thinking, s)
						}
					}
				case string:
					answer = append(answer, p)
				}
			}
			if env.Kind == wire.KindResult {
				break collect
			}
		case <-timeout:
			t.Fatal("timed out waiting for envelopes")
		}
	}

	if got := strings.Join(thinking, ""); got != "let me think about it" {
		t.Errorf("thinking = %q, want %q", got, "let me think about it")
	}
	if got := strings.Join(answer, ""); got != "the answer\n" {
		t.Errorf("answer = %q, want %q", got, "the answer\n")
	}

	// History must contain ONLY the answer — thinking is never persisted (D3).
	msgs := reg.History.LoadHistory("sid-think")
	var assistantText string
	for _, m := range msgs {
		if m.Role == "assistant" {
			assistantText += m.Text
		}
	}
	if assistantText != "the answer\n" {
		t.Errorf("history assistant text = %q, want %q (thinking must not persist)", assistantText, "the answer\n")
	}
	if strings.Contains(assistantText, "think") {
		t.Errorf("thinking leaked into history: %q", assistantText)
	}
}

// TestRegistry_UsageBroadcastBeforeResult — (tether#48) an EventUsage must be
// broadcast to subscribers as a KindMessage{type:"usage"} BEFORE the turn's
// KindResult (the frontend needs the still-open turn bubble to attach it to),
// and must NOT be accumulated into assistant history (usage is live-only, like
// thinking — absent after a reload).
func TestRegistry_UsageBroadcastBeforeResult(t *testing.T) {
	fs := &fakeSession{sid: "sid-usage", events: make(chan agent.Event, 8)}
	reg := NewRegistry(&fakeProvider{sess: fs})
	reg.History = NewHistoryStore(t.TempDir())

	entry, err := reg.GetOrSpawnEntry(context.Background(), "", "fake")
	if err != nil {
		t.Fatalf("GetOrSpawnEntry: %v", err)
	}
	subCh := make(chan wire.Envelope, 16)
	entry.Subscribe(subCh)

	// Mirror parseLine's ordering for a result-with-usage line: usage first,
	// result (turn-closer) second.
	fs.events <- agent.Event{Kind: agent.EventInit, SessionID: "sid-usage"}
	fs.events <- agent.Event{Kind: agent.EventText, Text: "the answer\n"}
	fs.events <- agent.Event{Kind: agent.EventUsage, Usage: &agent.UsageEvent{Input: 1234, Output: 856}}
	fs.events <- agent.Event{Kind: agent.EventResult, Text: "stop"}
	close(fs.events)

	var order []wire.EnvelopeKind
	var usage map[string]any
	timeout := time.After(2 * time.Second)
collect:
	for {
		select {
		case env := <-subCh:
			if env.Kind == wire.KindMessage {
				if p, ok := env.Payload.(map[string]any); ok && p["type"] == "usage" {
					usage = p
					order = append(order, env.Kind)
				}
			}
			if env.Kind == wire.KindResult {
				order = append(order, env.Kind)
				break collect
			}
		case <-timeout:
			t.Fatal("timed out waiting for envelopes")
		}
	}

	if len(order) != 2 || order[0] != wire.KindMessage || order[1] != wire.KindResult {
		t.Fatalf("envelope order = %v, want [usage-KindMessage, KindResult]", order)
	}
	if usage == nil || usage["input"] != 1234 || usage["output"] != 856 {
		t.Errorf("usage payload = %+v, want {input:1234, output:856}", usage)
	}

	// History must contain ONLY the answer — usage is never persisted.
	msgs := reg.History.LoadHistory("sid-usage")
	for _, m := range msgs {
		if m.Role == "assistant" && m.Text != "the answer\n" {
			t.Errorf("history assistant text = %q, want %q (usage must not persist)", m.Text, "the answer\n")
		}
	}
}

// waitForRegistered polls until sid is registered in reg.
//
// Needed only because these fakes model a provider that mints its own id (see
// announceInit): the entry is keyed under the id the registry pinned at spawn and
// moves to the announced one when fanOut processes the init, which is another
// goroutine. A provider that adopts the pinned id — real cc — is registered under
// its final sid before spawnEntry returns, and asserting THAT needs no polling at
// all (see TestSpawn_RegistersUnderTheMintedSidBeforeReturning). Bounded so a
// genuine bug (sid never registered) fails fast instead of hanging.
func waitForRegistered(t *testing.T, reg *Registry, sid string) {
	t.Helper()
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if reg.IsLive(sid) {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("session %q never registered under its real sid", sid)
}

// TestRegistry_DeliverAction_Approve — (tether#8 T8) DeliverAction routes an
// "approve" fenced-block callback to the named session's agent via
// SendPrompt, wrapped in the __tether_action__ control marker documented in
// docs/wire/fenced-contract.md §5.
func TestRegistry_DeliverAction_Approve(t *testing.T) {
	fs := &fakeSession{sid: "sid-approve", events: make(chan agent.Event, 4)}
	reg := NewRegistry(&fakeProvider{sess: fs})

	if _, err := reg.GetOrSpawnEntry(context.Background(), "", "fake"); err != nil {
		t.Fatalf("GetOrSpawnEntry: %v", err)
	}
	fs.announceInit()
	waitForRegistered(t, reg, "sid-approve")

	if err := reg.DeliverAction("sid-approve", "approve", "s-0", "planner"); err != nil {
		t.Fatalf("DeliverAction: %v", err)
	}

	prompts := fs.Prompts()
	if len(prompts) != 1 {
		t.Fatalf("len(prompts) = %d, want 1: %+v", len(prompts), prompts)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(prompts[0]), &got); err != nil {
		t.Fatalf("delivered payload not valid JSON: %v (%q)", err, prompts[0])
	}
	inner, ok := got["__tether_action__"].(map[string]any)
	if !ok {
		t.Fatalf("delivered payload missing __tether_action__ marker: %q", prompts[0])
	}
	if inner["action"] != "approve" || inner["blockId"] != "s-0" || inner["skill"] != "planner" {
		t.Errorf("__tether_action__ = %+v, want {action:approve blockId:s-0 skill:planner}", inner)
	}
}

// TestRegistry_DeliverAction_UnknownSession — an action naming a session the
// registry has never heard of (never existed, or already ended) must be
// dropped with an error, never a panic — the /wt/control channel is not
// otherwise session-scoped, so this is an expected race, not a bug.
func TestRegistry_DeliverAction_UnknownSession(t *testing.T) {
	reg := NewRegistry()
	if err := reg.DeliverAction("does-not-exist", "approve", "s-0", "planner"); err == nil {
		t.Fatal("DeliverAction: want error for unknown session, got nil")
	}
}

// TestRegistry_InterruptSession_CallsAgentInterrupt — (tether#8 T9)
// InterruptSession must reach the named session's agent.Session.Interrupt()
// directly, NOT go through SendPrompt/__tether_action__ (that's
// DeliverAction's job for "approve"; "pause" is a transport-level signal).
func TestRegistry_InterruptSession_CallsAgentInterrupt(t *testing.T) {
	fs := &fakeSession{sid: "sid-pause", events: make(chan agent.Event, 4)}
	reg := NewRegistry(&fakeProvider{sess: fs})

	if _, err := reg.GetOrSpawnEntry(context.Background(), "", "fake"); err != nil {
		t.Fatalf("GetOrSpawnEntry: %v", err)
	}
	fs.announceInit()
	waitForRegistered(t, reg, "sid-pause")

	if err := reg.InterruptSession("sid-pause"); err != nil {
		t.Fatalf("InterruptSession: %v", err)
	}

	if got := fs.InterruptCalls(); got != 1 {
		t.Fatalf("InterruptCalls() = %d, want 1", got)
	}
	// InterruptSession must not fall back to SendPrompt/__tether_action__ —
	// that's a different delivery path (DeliverAction).
	if got := fs.Prompts(); len(got) != 0 {
		t.Fatalf("Prompts() = %v, want none — InterruptSession must not call SendPrompt", got)
	}
}

// TestRegistry_InterruptSession_UnknownSession — an unknown or already-ended
// sid must return an error, never panic; same expected race as
// DeliverAction's unknown-session case.
func TestRegistry_InterruptSession_UnknownSession(t *testing.T) {
	reg := NewRegistry()
	if err := reg.InterruptSession("does-not-exist"); err == nil {
		t.Fatal("InterruptSession: want error for unknown session, got nil")
	}
}

// regLen returns how many entries the registry currently holds, read under
// its lock. Same-package white-box access used by the tether#12 eviction
// tests to assert the map is actually reclaimed — not just that one sid
// lookup misses.
func regLen(reg *Registry) int {
	reg.mu.RLock()
	defer reg.mu.RUnlock()
	return len(reg.sessions)
}

// waitForEvicted polls until sid is no longer registered — i.e. fanOut ran
// its eviction defer after Events() closed. Bounded so a genuine failure
// (entry never evicted) fails fast instead of hanging.
func waitForEvicted(t *testing.T, reg *Registry, sid string) {
	t.Helper()
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if !reg.IsLive(sid) {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("session %q never evicted after its Events() channel closed", sid)
}

// waitForCount polls until the registry holds exactly n entries. Bounded.
func waitForCount(t *testing.T, reg *Registry, n int) {
	t.Helper()
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if regLen(reg) == n {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("registry never reached %d entries; has %d", n, regLen(reg))
}

// regKeys returns the keys the registry currently holds, read under its lock.
// White-box, same-package: used to assert WHICH key an entry is registered under,
// not merely that some lookup succeeds (tether#54).
func regKeys(reg *Registry) []string {
	reg.mu.RLock()
	defer reg.mu.RUnlock()
	keys := make([]string, 0, len(reg.sessions))
	for k := range reg.sessions {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// TestRegistry_EvictsEntryOnSessionEnd — (tether#12) once a session's agent
// Events() channel closes (subprocess exited, or the client disconnected and
// cancelled the ctx that bounds the subprocess), fanOut returns and must
// remove the entry from the registry. Before this fix a long-running daemon
// leaked one map entry — plus its subscriber set and fence parser — for every
// session ever opened.
func TestRegistry_EvictsEntryOnSessionEnd(t *testing.T) {
	fs := &fakeSession{sid: "sid-evict", events: make(chan agent.Event, 8)}
	reg := NewRegistry(&fakeProvider{sess: fs})

	if _, err := reg.GetOrSpawnEntry(context.Background(), "", "fake"); err != nil {
		t.Fatalf("GetOrSpawnEntry: %v", err)
	}
	// Drive system/init so the entry is re-keyed under its real sid.
	fs.events <- agent.Event{Kind: agent.EventInit, SessionID: "sid-evict"}
	waitForRegistered(t, reg, "sid-evict")

	// Session ends: closing Events() unblocks fanOut's range and runs its
	// eviction defer.
	close(fs.events)

	waitForEvicted(t, reg, "sid-evict")
	if n := regLen(reg); n != 0 {
		t.Fatalf("registry holds %d entries after session end, want 0", n)
	}
}

// TestRegistry_ReapsAgentOnSessionEnd — (tether#56) ending a session must REAP
// its agent, not merely un-register it. agent.Session.Close is the only thing in
// the daemon that can wait on a cc subprocess, and until this slice its sole
// production caller was Attachment.resolve's failed-resume path — so every session that ended the
// ORDINARY way left a `[claude] <defunct>` zombie, a goroutine parked in
// exec.CommandContext's watchdog and an unclosed stdin fd behind, held for the
// rest of the daemon's life.
//
// Exactly once, not at-least-once: the real ccSession absorbs a second Close in a
// sync.Once, so a ">= 1" assertion would keep passing if teardown ever started
// leaning on that absorption instead of owning the reap.
func TestRegistry_ReapsAgentOnSessionEnd(t *testing.T) {
	fs := &fakeSession{sid: "sid-reap", events: make(chan agent.Event, 8)}
	reg := NewRegistry(&fakeProvider{sess: fs})

	if _, err := reg.GetOrSpawnEntry(context.Background(), "", "fake"); err != nil {
		t.Fatalf("GetOrSpawnEntry: %v", err)
	}
	fs.events <- agent.Event{Kind: agent.EventInit, SessionID: "sid-reap"}
	waitForRegistered(t, reg, "sid-reap")

	// A live session must not be closed — teardown is a session-END action, and an
	// implementation that reaped eagerly would kill conversations mid-turn.
	if n := fs.Closes(); n != 0 {
		t.Fatalf("Close() called %d times on a LIVE session, want 0", n)
	}

	// Session ends: closing Events() unblocks fanOut's range and runs its
	// teardown defer.
	close(fs.events)
	waitForEvicted(t, reg, "sid-reap")

	// Eviction and the reap are two statements in teardown, deliberately in that
	// order, so poll rather than assume the second landed with the first.
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) && fs.Closes() == 0 {
		time.Sleep(time.Millisecond)
	}
	if n := fs.Closes(); n != 1 {
		t.Fatalf("Close() called %d times after the session ended, want exactly 1 "+
			"(0 = the agent is never reaped, which is tether#56)", n)
	}
}

// TestRegistry_TeardownReapsARealChildProcess makes the same claim as the test
// above, but asks the operating system instead of a counter. "fanOut called
// Close()" is the mechanism; "the child is no longer a zombie" is what tether#56
// was actually about, and the two only coincide for as long as Close really
// waits.
//
// The child exits on its own, so the zombie exists BEFORE the teardown — the test
// asserts state Z first, which is what stops the second half from being vacuous
// (a pid that had already been reaped, or never existed, would sail through a
// bare "it's gone now" check).
func TestRegistry_TeardownReapsARealChildProcess(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("zombie state is read from /proc, which is Linux-only")
	}

	// Exits immediately, with a distinctive status so the harvested ProcessState
	// is recognisably THIS child's and not a default zero value.
	cmd := exec.Command("sh", "-c", "exit 7")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start child: %v", err)
	}
	pid := cmd.Process.Pid
	ps := &procSession{cmd: cmd, events: make(chan agent.Event, 4)}
	t.Cleanup(func() { _ = ps.Close() }) // so a failed test never leaks the zombie itself

	reg := NewRegistry(&procProvider{sess: ps})
	if _, err := reg.GetOrSpawnEntry(context.Background(), "", "proc"); err != nil {
		t.Fatalf("GetOrSpawnEntry: %v", err)
	}
	waitForCount(t, reg, 1)

	// The leak, staged: an exited child with nobody waiting on it sits in the
	// process table as a zombie.
	waitForProcState(t, pid, "Z")

	close(ps.events)
	waitForCount(t, reg, 0)

	// Poll the mutex-guarded counter rather than cmd.ProcessState directly: the
	// Wait happens on fanOut's goroutine, and the mutex is what gives this
	// goroutine a happens-before edge to read what it produced.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && ps.Closes() == 0 {
		time.Sleep(time.Millisecond)
	}
	if n := ps.Closes(); n != 1 {
		t.Fatalf("Close() called %d times after the session ended, want exactly 1", n)
	}
	if got := ps.ExitCode(); got != 7 {
		t.Errorf("harvested exit code = %d, want 7 — the reap did not collect THIS child's status", got)
	}
	if st := waitForProcGone(t, pid); st != "" {
		t.Fatalf("child pid %d is still in the process table as state %q after teardown; "+
			"the session ended without reaping its agent (tether#56)", pid, st)
	}
}

// procSession is an agent.Session backed by a REAL child process — the double
// that lets a registry test ask the OS whether teardown reaped anything, which
// the in-memory fakeSession cannot. Its Close mirrors ccSession.Close's essential
// half (wait for the child) and deliberately does NOT copy the sync.Once: the
// property under test is that the registry closes it exactly once, and a
// once-guard here would hide a second call instead of exposing it.
type procSession struct {
	cmd    *exec.Cmd
	events chan agent.Event

	mu       sync.Mutex
	closes   int
	exitCode int
}

func (p *procSession) SessionID() string                        { return "" }
func (p *procSession) Alive() bool                              { return true }
func (p *procSession) SendPrompt(context.Context, string) error { return nil }
func (p *procSession) Events() <-chan agent.Event               { return p.events }
func (p *procSession) Interrupt() error                         { return nil }

func (p *procSession) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closes > 0 {
		// Counted but not re-waited: a second cmd.Wait would return "exec: Wait was
		// already called", which would make the t.Cleanup safety net look like a
		// failure rather than the no-op it is.
		p.closes++
		return nil
	}
	err := p.cmd.Wait()
	p.closes++
	if p.cmd.ProcessState != nil {
		p.exitCode = p.cmd.ProcessState.ExitCode()
	}
	return err
}

func (p *procSession) Closes() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.closes
}

func (p *procSession) ExitCode() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.exitCode
}

type procProvider struct{ sess *procSession }

func (p *procProvider) Name() string { return "proc" }
func (p *procProvider) Spawn(context.Context, agent.SpawnConfig) (agent.Session, error) {
	return p.sess, nil
}

// procState returns the single-letter state Linux publishes for pid in
// /proc/<pid>/stat — "Z" for a zombie — or "" once the pid is gone, which for a
// child of this process means it has been reaped. The comm field is parenthesised
// and may itself contain spaces, so the state is read from after the LAST ')'
// rather than by splitting the whole line.
func procState(t *testing.T, pid int) string {
	t.Helper()
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if os.IsNotExist(err) {
		return ""
	}
	if err != nil {
		t.Fatalf("read /proc/%d/stat: %v", pid, err)
	}
	i := strings.LastIndex(string(b), ")")
	if i < 0 {
		t.Fatalf("unparseable /proc/%d/stat: %q", pid, b)
	}
	f := strings.Fields(string(b)[i+1:])
	if len(f) == 0 {
		t.Fatalf("unparseable /proc/%d/stat: %q", pid, b)
	}
	return f[0]
}

// waitForProcState polls until pid reports state want. Bounded — a child that
// never gets there fails the test instead of hanging it.
func waitForProcState(t *testing.T, pid int, want string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	var got string
	for time.Now().Before(deadline) {
		if got = procState(t, pid); got == want {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("child pid %d state = %q, want %q", pid, got, want)
}

// waitForProcGone polls until pid leaves the process table, returning the last
// state observed ("" on success) so the caller can report what it got stuck as.
func waitForProcGone(t *testing.T, pid int) string {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	var got string
	for time.Now().Before(deadline) {
		if got = procState(t, pid); got == "" {
			return ""
		}
		time.Sleep(time.Millisecond)
	}
	return got
}

// TestRegistry_EvictedEntryNotResurrectedByRekey — (tether#12, carried into
// tether#54) an entry that has already been un-registered must not come back when
// its agent announces a session id.
//
// tether#12 met this as "fanOut evicted the pending-keyed entry while a goroutine
// was still parked in SessionID()", and guarded it with an `evicted` flag. That
// goroutine is gone: re-keying now happens inside fanOut, so the ORDINARY
// teardown cannot race itself. What remains is the two evictors that run on other
// goroutines — liveEntry dropping a corpse, Attachment.resolve dropping a failed
// resume — either of which can un-register the entry before fanOut gets to an init
// still sitting in the channel buffer. Registry.rekey therefore MOVES a
// registration and never creates one; this stages exactly that.
func TestRegistry_EvictedEntryNotResurrectedByRekey(t *testing.T) {
	fs := &fakeSession{sid: "sid-race", events: make(chan agent.Event, 4)}
	reg := NewRegistry(&fakeProvider{sess: fs})

	e, err := reg.GetOrSpawnEntry(context.Background(), "", "fake")
	if err != nil {
		t.Fatalf("GetOrSpawnEntry: %v", err)
	}
	keys := regKeys(reg)
	if len(keys) != 1 {
		t.Fatalf("registry keys after spawn = %v, want exactly one (the minted sid)", keys)
	}
	// Subscribing gives the assertion below a BARRIER rather than a sleep: an
	// envelope on this channel proves fanOut has already processed everything
	// queued ahead of the event that produced it.
	ch := make(chan wire.Envelope, 8)
	e.Subscribe(ch)

	// A concurrent evictor un-registers the entry: the agent is dead, and the
	// reconnect path notices before fanOut has looked at anything.
	fs.dead.Store(true)
	if reg.IsLive(keys[0]) {
		t.Fatal("IsLive = true for a session whose agent reports dead")
	}
	waitForCount(t, reg, 0)

	// The init was already in flight. Processing it must not re-register.
	fs.announceInit()
	fs.events <- agent.Event{Kind: agent.EventText, Text: "past the init\n"}
	select {
	case <-ch:
	case <-time.After(2 * time.Second):
		t.Fatal("fanOut never processed the in-flight init; the assertion below would be vacuous")
	}

	// Read the map directly. IsLive would hide a resurrection here: it re-evicts
	// whatever it finds dead, so it would report "not live" about an entry it had
	// just found registered.
	if got := regKeys(reg); len(got) != 0 {
		t.Fatalf("an evicted entry was resurrected by the re-key, under %v; want an empty registry", got)
	}
	close(fs.events)
}

// ─── tether#50: the failed-resume empty `result` must never reach the browser ──

// TestFanOut_SuppressesInitlessEmptyResult — a `result` with no preceding
// system/init and no text is DROPPED.
//
// This is the exact wire shape of a failed `cc --resume` (mem_2ruSlrHR ③): cc
// exits 1 having printed one line,
// {"type":"result","subtype":"error_during_execution","result":null,…}, and no
// init at all. parseLine turns that `null` into an EventResult with an empty Text.
//
// Forwarding it would close a turn the user never started — the frontend's
// 'result' branch clears streaming/curTurnId and resets `stopped`, so the
// thinking indicator for the prompt still in flight blinks out just before the
// Attachment fallback respawns and answers for real in a fresh bubble.
func TestFanOut_SuppressesInitlessEmptyResult(t *testing.T) {
	fs := &fakeSession{sid: "", events: make(chan agent.Event, 4)}
	reg := NewRegistry(&fakeProvider{sess: fs})

	e, err := reg.GetOrSpawnEntry(context.Background(), "", "fake")
	if err != nil {
		t.Fatalf("GetOrSpawnEntry: %v", err)
	}
	ch := make(chan wire.Envelope, 8)
	e.Subscribe(ch)

	// The single line a failed resume produces, then EOF.
	fs.events <- agent.Event{Kind: agent.EventResult, Text: ""}
	close(fs.events)

	// An empty registry now means fanOut RETURNED: the entry is registered under
	// the id spawnEntry minted for it, and the only thing that removes it is
	// fanOut's own deferred evict (tether#54 — before it, a re-key goroutine could
	// empty the map on its own, so this had to gate on a separate signal, and a
	// "nothing was broadcast" assertion gated on the count would have passed by
	// scheduling luck).
	waitForCount(t, reg, 0)

	select {
	case env := <-ch:
		t.Fatalf("an init-less empty result reached the browser as %q/%#v; it must be swallowed", env.Kind, env.Payload)
	default:
	}
}

// TestFanOut_ForwardsEmptyResultAfterInit — the positive control for the guard
// above, and the reason it tests !sawInit rather than just "is the text empty".
//
// A real turn always emits system/init first (mem_2ruSlrHR ⑤). Once init has been
// seen, a result closes the turn even when its text is empty — the frontend needs
// that KindResult to stop the streaming cursor. A suppression keyed only on
// "empty text" would eat this one and hang the turn forever.
func TestFanOut_ForwardsEmptyResultAfterInit(t *testing.T) {
	fs := &fakeSession{sid: "sid-init-then-empty", events: make(chan agent.Event, 4)}
	reg := NewRegistry(&fakeProvider{sess: fs})

	e, err := reg.GetOrSpawnEntry(context.Background(), "", "fake")
	if err != nil {
		t.Fatalf("GetOrSpawnEntry: %v", err)
	}
	ch := make(chan wire.Envelope, 8)
	e.Subscribe(ch)

	fs.events <- agent.Event{Kind: agent.EventInit, SessionID: "sid-init-then-empty"}
	fs.events <- agent.Event{Kind: agent.EventResult, Text: ""}

	deadline := time.After(2 * time.Second)
	for {
		select {
		case env := <-ch:
			if env.Kind == wire.KindResult {
				return // turn closed, as it must be
			}
		case <-deadline:
			t.Fatal("a post-init empty result was swallowed; the turn would never close")
		}
	}
}

// TestFanOut_ForwardsInitlessResultCarryingText — the other half of the guard's
// conjunction. An init-less result that DOES carry text has never been observed
// from cc, but if one ever appears its content is real and must not vanish; the
// guard is deliberately narrow enough to let it through.
func TestFanOut_ForwardsInitlessResultCarryingText(t *testing.T) {
	fs := &fakeSession{sid: "", events: make(chan agent.Event, 4)}
	reg := NewRegistry(&fakeProvider{sess: fs})

	e, err := reg.GetOrSpawnEntry(context.Background(), "", "fake")
	if err != nil {
		t.Fatalf("GetOrSpawnEntry: %v", err)
	}
	ch := make(chan wire.Envelope, 8)
	e.Subscribe(ch)

	fs.events <- agent.Event{Kind: agent.EventResult, Text: "something real"}

	select {
	case env := <-ch:
		if env.Kind != wire.KindResult {
			t.Fatalf("envelope kind = %q, want %q", env.Kind, wire.KindResult)
		}
		if s, _ := env.Payload.(string); s != "something real" {
			t.Fatalf("payload = %#v, want \"something real\"", env.Payload)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("an init-less result WITH text was swallowed; the guard is too broad")
	}
}

// ─── tether#124: a dropped envelope must leave a mark ────────────────────────

// gapNoticeText pulls the text out of an envelope if it is the gap notice, so
// the tests below assert on the SHAPE web/src/lib/store.ts consumes (a
// KindMessage whose payload object has type "notice" and a non-empty text)
// rather than on an internal constant. The second return distinguishes "this is
// not a notice" from "a notice with no words", which is a notice the frontend
// silently discards.
func gapNoticeTextOf(env wire.Envelope) (string, bool) {
	if env.Kind != wire.KindMessage {
		return "", false
	}
	obj, ok := env.Payload.(map[string]any)
	if !ok {
		return "", false
	}
	if t, _ := obj["type"].(string); t != "notice" {
		return "", false
	}
	text, _ := obj["text"].(string)
	return text, true
}

// lossySubscriber gives a test a subscriber whose channel is already FULL, and
// returns the channel plus a drain function.
//
// The capacity is 2 and not 32: what is under test is the behaviour at the
// boundary, and the real 32 in serveChat is not a number this package knows or
// should depend on (see the subCh declaration in internal/server/wt_chat.go for
// why that number is what it is and why nobody has measured it).
func lossySubscriber(t *testing.T, reg *Registry, e *Entry) (chan wire.Envelope, func(n int)) {
	t.Helper()
	ch := make(chan wire.Envelope, 2)
	e.Subscribe(ch)
	return ch, func(n int) {
		t.Helper()
		for i := 0; i < n; i++ {
			select {
			case <-ch:
			default:
				t.Fatalf("drain: channel empty after %d of %d", i, n)
			}
		}
	}
}

// TestBroadcast_MarksTheGapLeftByADroppedEnvelope — the whole of tether#124's
// claim, in one sequence.
//
// A subscriber whose channel fills loses envelopes (Registry.broadcast drops
// rather than blocking, deliberately — a session's fanOut must not be held up by
// one slow browser). Until this change the ONLY record of that was a server-side
// slog.Warn, so the tab rendered an answer with a hole in it and said nothing:
// see the subCh declaration in internal/server/wt_chat.go for the mechanism that
// makes a stalled browser lose exactly this way.
//
// The repair the notice asks for is real and is the only one available. The text
// is on disk before it is ever broadcast (emitSegments writes HistoryStore first),
// and ChatPane's mount effect refetches GET /messages for the restored sid — so a
// reload shows the full answer. A RECONNECT does not: session_ready re-confirms
// the same sid, which is a no-op for the `[sessionId]` effect that owns the
// refetch. That asymmetry is why the notice says "reload".
func TestBroadcast_MarksTheGapLeftByADroppedEnvelope(t *testing.T) {
	reg := &Registry{}
	e := &Entry{subs: make(map[chan wire.Envelope]struct{})}
	ch, drain := lossySubscriber(t, reg, e)

	reg.broadcast(e, wire.Envelope{Kind: wire.KindMessage, Payload: "one"})
	reg.broadcast(e, wire.Envelope{Kind: wire.KindMessage, Payload: "two"})
	// Full now: these two are the ones the user never sees.
	reg.broadcast(e, wire.Envelope{Kind: wire.KindMessage, Payload: "lost-a"})
	reg.broadcast(e, wire.Envelope{Kind: wire.KindMessage, Payload: "lost-b"})

	// The browser catches up and the loop starts draining again.
	drain(2)
	reg.broadcast(e, wire.Envelope{Kind: wire.KindMessage, Payload: "three"})

	var env wire.Envelope
	select {
	case env = <-ch:
	default:
		t.Fatal("nothing queued after the subscriber caught up")
	}
	text, ok := gapNoticeTextOf(env)
	if !ok {
		t.Fatalf("first envelope after the gap = %q/%#v; want the gap notice, so the tab is told its view is incomplete", env.Kind, env.Payload)
	}
	if text == "" {
		t.Fatal("gap notice carries no text; store.ts drops a notice with an empty text, so this reaches nobody")
	}

	// And the notice is IN BAND, not instead of the traffic: the envelope that
	// did fit still arrives, immediately after the marker.
	select {
	case env = <-ch:
		if s, _ := env.Payload.(string); s != "three" {
			t.Fatalf("envelope after the notice = %#v, want \"three\"", env.Payload)
		}
	default:
		t.Fatal("the gap notice replaced the envelope it was supposed to precede")
	}
}

// TestBroadcast_OneNoticePerGapNotPerLostEnvelope — the notice describes a GAP,
// so two lost envelopes in a row are one notice and not two.
//
// This is load-bearing rather than tidiness. store.ts collapses a repeat of the
// session banner already showing, so a notice per lost envelope would mostly be
// swallowed there — mostly, because tether#80 made that dedupe compare against
// the last SESSION-class notice, so any other line arriving in between un-gates
// the next identical one. Bounding the emission here rather than relying on the
// frontend's collapse keeps that out of the picture entirely.
func TestBroadcast_OneNoticePerGapNotPerLostEnvelope(t *testing.T) {
	reg := &Registry{}
	e := &Entry{subs: make(map[chan wire.Envelope]struct{})}
	ch, drain := lossySubscriber(t, reg, e)

	reg.broadcast(e, wire.Envelope{Kind: wire.KindMessage, Payload: "one"})
	reg.broadcast(e, wire.Envelope{Kind: wire.KindMessage, Payload: "two"})
	for i := 0; i < 5; i++ {
		reg.broadcast(e, wire.Envelope{Kind: wire.KindMessage, Payload: "lost"})
	}
	drain(2)

	// Room for exactly the notice and one payload, so a second notice would have
	// to displace the payload and would be caught either way.
	reg.broadcast(e, wire.Envelope{Kind: wire.KindMessage, Payload: "three"})

	notices, payloads := 0, 0
	for {
		select {
		case env := <-ch:
			if _, ok := gapNoticeTextOf(env); ok {
				notices++
			} else {
				payloads++
			}
			continue
		default:
		}
		break
	}
	if notices != 1 {
		t.Fatalf("notices after a run of %d lost envelopes = %d, want exactly 1", 5, notices)
	}
	if payloads != 1 {
		t.Fatalf("payloads delivered = %d, want 1 (the notice must not displace traffic)", payloads)
	}

	// Having reported the gap, it goes QUIET: a subscriber that is keeping up gets
	// traffic and nothing else.
	//
	// This is the half a mutation battery had to find, and the reason it is worth
	// naming: the "one notice per gap" count above passes whether the counter is
	// cleared when it is reported or never cleared at all, because that sequence
	// ends immediately after the report. A counter that only ever grows would
	// prefix EVERY later envelope with a notice, for the whole life of the
	// connection, and every other assertion in this file would still be green.
	for i := 0; i < 3; i++ {
		reg.broadcast(e, wire.Envelope{Kind: wire.KindMessage, Payload: fmt.Sprintf("calm-%d", i)})
		select {
		case env := <-ch:
			if _, ok := gapNoticeTextOf(env); ok {
				t.Fatalf("envelope %d after the gap was reported is another notice; the counter is never cleared, so the tab is told forever", i)
			}
		default:
			t.Fatalf("nothing queued for envelope %d", i)
		}
	}

	// A second gap is a second notice: the counter reports and RE-ARMS. Without
	// this the tab is told once and then goes quiet for the rest of a long
	// session, however many more times it stalls.
	//
	// Three sends into a channel of two: the third is the one that is lost. Two
	// would FIT, and a version of this that sent only two would assert nothing —
	// it would be asking about a gap that never happened.
	reg.broadcast(e, wire.Envelope{Kind: wire.KindMessage, Payload: "four"})
	reg.broadcast(e, wire.Envelope{Kind: wire.KindMessage, Payload: "four-b"})
	reg.broadcast(e, wire.Envelope{Kind: wire.KindMessage, Payload: "lost again"})
	drain(2)
	reg.broadcast(e, wire.Envelope{Kind: wire.KindMessage, Payload: "five"})
	select {
	case env := <-ch:
		if _, ok := gapNoticeTextOf(env); !ok {
			t.Fatalf("first envelope after the SECOND gap = %q/%#v; the notice does not re-arm", env.Kind, env.Payload)
		}
	default:
		t.Fatal("nothing queued after the second gap")
	}
}

// TestBroadcast_NoticeGoesOnlyToTheSubscriberThatLost — the notice must be
// per-connection, and this is the assertion that makes that a property rather
// than an accident of there usually being one subscriber.
//
// A session can have several (the same human on a phone and a laptop, both
// attached to one sid). A tab that received everything must not be told its view
// has a hole in it: this file's own neighbours argue that a notice a user has
// caught lying is one they stop reading, and a false one here would spend exactly
// that credit.
func TestBroadcast_NoticeGoesOnlyToTheSubscriberThatLost(t *testing.T) {
	reg := &Registry{}
	e := &Entry{subs: make(map[chan wire.Envelope]struct{})}
	slow, drainSlow := lossySubscriber(t, reg, e)
	fast := make(chan wire.Envelope, 64)
	e.Subscribe(fast)

	for i := 0; i < 6; i++ {
		reg.broadcast(e, wire.Envelope{Kind: wire.KindMessage, Payload: fmt.Sprintf("tok-%d", i)})
	}
	drainSlow(2)
	reg.broadcast(e, wire.Envelope{Kind: wire.KindMessage, Payload: "tok-6"})

	select {
	case env := <-slow:
		if _, ok := gapNoticeTextOf(env); !ok {
			t.Fatalf("the subscriber that LOST envelopes was told nothing; got %q/%#v", env.Kind, env.Payload)
		}
	default:
		t.Fatal("nothing queued for the slow subscriber")
	}

	for {
		select {
		case env := <-fast:
			if _, ok := gapNoticeTextOf(env); ok {
				t.Fatal("a subscriber that received every envelope was told its view is incomplete")
			}
			continue
		default:
		}
		break
	}
}

// captureWarnings redirects slog to a buffer for the duration of one test and
// returns a reader for what was logged.
//
// A test that asserts on log output is a poor second choice and is used here
// because it is the only choice: the whole of the BroadcastAll fix is that a drop
// which used to leave NO trace anywhere now leaves one, and "no trace anywhere"
// is by definition not observable through any other surface. The alternative —
// asserting the drop through Entry.lost — would assert the opposite of what
// deliverOutOfBand is supposed to do.
func captureWarnings(t *testing.T) *strings.Builder {
	t.Helper()
	var buf strings.Builder
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &buf
}

// TestBroadcastAll_RecordsItsDropsButPromisesNothing — BroadcastAll is a SECOND
// drop site on the same channels, and it needs the opposite treatment to the
// first one.
//
// Two assertions, and they pull in opposite directions on purpose:
//
//   - It must leave a RECORD. Registry.broadcast at least warned; BroadcastAll's
//     loop over e.subs was a bare `select { case ch <- env: default: }` with no
//     log at all, so a daemon-wide envelope vanished from a stalled tab without
//     leaving a trace anywhere.
//   - It must NOT emit the gap notice, and must not raise the counter that does.
//     The notice tells the reader to reload, and for these three producers a
//     reload does not get the content back — for a KindPermission request it is
//     actively destructive, since loadHistory clears pendingPermissions and
//     nothing refills it. See deliverOutOfBand.
func TestBroadcastAll_RecordsItsDropsButPromisesNothing(t *testing.T) {
	logs := captureWarnings(t)
	e := &Entry{subs: make(map[chan wire.Envelope]struct{}), regKey: "sid-all"}
	reg := &Registry{sessions: map[string]*Entry{"sid-all": e}}
	ch, drain := lossySubscriber(t, reg, e)

	for i := 0; i < 4; i++ {
		reg.BroadcastAll(wire.Envelope{Kind: wire.KindPermission, Payload: "wide"})
	}
	if got := logs.String(); !strings.Contains(got, "envelope dropped") {
		t.Fatalf("BroadcastAll dropped two envelopes and logged nothing; captured warnings were %q", got)
	}
	if got := logs.String(); !strings.Contains(got, "repairable_by_reload=false") {
		t.Fatalf("the log does not say the drop is unrecoverable, which is the one fact an operator needs here; got %q", got)
	}

	// Nothing it dropped may be announced, now or later: the counter the notice
	// reports must still be zero, so the next TURN envelope makes no claim about
	// a permission request a reload cannot bring back.
	drain(2)
	reg.broadcast(e, wire.Envelope{Kind: wire.KindMessage, Payload: "a real turn envelope"})
	select {
	case env := <-ch:
		if _, ok := gapNoticeTextOf(env); ok {
			t.Fatal("a BroadcastAll drop produced a notice telling the reader to reload; a reload cannot recover any of BroadcastAll's payloads")
		}
	default:
		t.Fatal("nothing queued after the subscriber caught up")
	}
}

// TestUnsubscribe_ForgetsTheDropAccounting — Subscribe and Unsubscribe must keep
// the accounting map in step with the audience map, or a long-lived daemon leaks
// one counter per chat connection that ever existed.
//
// Asserted on the map because there is nothing observable to assert on: a
// forgotten counter is invisible until the process runs out of memory. This is
// also the only test that would catch the accounting being keyed to an Entry that
// Attachment.Subscribe has already swapped away from.
func TestUnsubscribe_ForgetsTheDropAccounting(t *testing.T) {
	e := &Entry{subs: make(map[chan wire.Envelope]struct{})}
	ch := make(chan wire.Envelope, 1)
	e.Subscribe(ch)
	e.subsMu.RLock()
	tracked := len(e.lost)
	e.subsMu.RUnlock()
	if tracked != 1 {
		t.Fatalf("counters after Subscribe = %d, want 1 (a subscriber with no counter can never be told about a gap)", tracked)
	}
	e.Unsubscribe(ch)
	e.subsMu.RLock()
	tracked = len(e.lost)
	e.subsMu.RUnlock()
	if tracked != 0 {
		t.Fatalf("counters after Unsubscribe = %d, want 0 (one leaked per connection for the daemon's lifetime)", tracked)
	}
}

// ============================================================================
// tether#132 — a permission request BroadcastAll dropped must still reach the
// next client that attaches.
// ============================================================================

// permReq builds the envelope internal/server/mux.go's permissionEnvelope
// produces for one outstanding permission request. Hand-rolled here rather than
// imported because internal/server imports this package, so the shape is
// duplicated on purpose and the field names are what the frontend reads.
func permReq(id, sid string) wire.Envelope {
	return wire.Envelope{
		Kind:      wire.KindPermission,
		SessionID: wire.SessionID(sid),
		Payload:   map[string]any{"id": id, "toolName": "Bash", "input": map[string]any{}},
	}
}

// permIDsOf drains ch and returns the ids of the KindPermission envelopes it
// held, in order. It returns after the channel is empty rather than after a
// timeout: everything this file's fixtures deliver is enqueued synchronously by
// the call under test, so "empty" is a complete answer and a wait would only
// make a failure slow.
func permIDsOf(ch chan wire.Envelope) []string {
	var ids []string
	for {
		select {
		case env := <-ch:
			if env.Kind != wire.KindPermission {
				continue
			}
			if p, ok := env.Payload.(map[string]any); ok {
				if id, ok := p["id"].(string); ok {
					ids = append(ids, id)
				}
			}
		default:
			return ids
		}
	}
}

// TestBackfill_ADroppedPermissionRequestReachesTheNextSubscriber is the whole of
// tether#132 in one sequence, and it crosses the drop.
//
// The failure it pins: a permission request is announced by exactly one
// BroadcastAll envelope, deliverOutOfBand drops that envelope onto a full
// channel, and the frontend has no other way to learn the request exists — so
// the tool call waits for a decision nobody can be asked for. The reader's only
// move is a reload, which is a fresh subscribe, and this asserts that the fresh
// subscribe is where the request comes back.
//
// The window is WIDENED rather than raced: the stalled subscriber's channel is
// filled by ordinary turn envelopes first, so the drop is a property of the
// fixture and not of how fast this machine is. `outstanding` stands in for
// permission.Manager.Pending() — the daemon's own record, which the drop never
// touched, and which is the reason this repair needs no new persistence.
func TestBackfill_ADroppedPermissionRequestReachesTheNextSubscriber(t *testing.T) {
	e := &Entry{subs: make(map[chan wire.Envelope]struct{}), regKey: "sid-perm"}
	reg := &Registry{sessions: map[string]*Entry{"sid-perm": e}}
	e.reg = reg

	var outstanding []wire.Envelope
	reg.PendingBackfill = func() []wire.Envelope { return outstanding }

	// The tab that is about to stall. Nothing is outstanding yet, so it gets no
	// backfill — which also means the two fillers below really do fill it.
	stalled := make(chan wire.Envelope, 2)
	e.Subscribe(stalled)

	// It stalls: serveChat's drain loop is parked in OpenUniStreamSync and the
	// channel fills with token increments.
	reg.broadcast(e, wire.Envelope{Kind: wire.KindMessage, Payload: "tok-1"})
	reg.broadcast(e, wire.Envelope{Kind: wire.KindMessage, Payload: "tok-2"})

	// The hook posts a permission request. The daemon registers it and announces
	// it daemon-wide; the announcement does not fit.
	req := permReq("req-1", "sid-perm")
	outstanding = append(outstanding, req)
	reg.BroadcastAll(req)

	if ids := permIDsOf(stalled); len(ids) != 0 {
		t.Fatalf("fixture is not staging the bug: the stalled subscriber received %v, so the envelope was never dropped", ids)
	}

	// Another client attaches to the same live session — a second device, a
	// second tab, or Attachment.adopt migrating the channel after a failed resume —
	// and subscribes a fresh channel.
	//
	// NOT a reload of the only open tab, which is measured NOT to work and is said
	// so in Entry.backfill's doc: closing that connection kills the agent, so the
	// tool call is gone before anything can be replayed to it. The fixture here is
	// deliberately the case the change does cover.
	fresh := make(chan wire.Envelope, 32)
	e.Subscribe(fresh)

	ids := permIDsOf(fresh)
	if len(ids) != 1 || ids[0] != "req-1" {
		t.Fatalf("permission ids delivered to the reconnecting client = %v, want [req-1]; "+
			"a dropped request is still invisible to every client, and the tool call is still waiting", ids)
	}
}

// TestBackfill_ADecidedRequestIsNotReplayed — the backfill must be a live view of
// what is outstanding, not a log of what was dropped.
//
// Replaying a request that has since been answered (from another device, or by
// the timeout in permission.Manager.Add) would put a prompt on screen for a tool
// call that is already running, and a second decision on it is a 404 the UI does
// not explain. This is why nothing here remembers what BroadcastAll dropped: the
// question "what is outstanding" only has a correct answer at the moment it is
// asked.
func TestBackfill_ADecidedRequestIsNotReplayed(t *testing.T) {
	e := &Entry{subs: make(map[chan wire.Envelope]struct{}), regKey: "sid-perm"}
	reg := &Registry{sessions: map[string]*Entry{"sid-perm": e}}
	e.reg = reg

	outstanding := []wire.Envelope{permReq("answered", "sid-perm"), permReq("still-waiting", "sid-perm")}
	reg.PendingBackfill = func() []wire.Envelope { return outstanding }

	// The first is decided before anyone reconnects, so the manager drops it and
	// the backfill stops offering it.
	outstanding = outstanding[1:]

	fresh := make(chan wire.Envelope, 32)
	e.Subscribe(fresh)
	if ids := permIDsOf(fresh); len(ids) != 1 || ids[0] != "still-waiting" {
		t.Fatalf("replayed %v, want [still-waiting] only", ids)
	}
}

// TestBackfill_RegistersTheChannelBeforeReplayingIt pins the ORDER of the two
// halves of Subscribe, which is a real choice with a real losing side.
//
// Between adding the channel to the audience and replaying the snapshot there is
// a window, and a request broadcast inside it is either delivered twice (register
// first) or lost outright (replay first) — and losing it is precisely the defect
// tether#132 exists to remove, recreated one instruction wide.
//
// Staged deterministically rather than by racing: the backfill function itself
// broadcasts the new request, so "inside the window" is where it happens by
// construction. That is only possible because backfill runs with subsMu RELEASED;
// a version holding the lock across both halves would deadlock here, which is the
// second thing this test pins.
func TestBackfill_RegistersTheChannelBeforeReplayingIt(t *testing.T) {
	e := &Entry{subs: make(map[chan wire.Envelope]struct{}), regKey: "sid-perm"}
	reg := &Registry{sessions: map[string]*Entry{"sid-perm": e}}
	e.reg = reg

	reg.PendingBackfill = func() []wire.Envelope {
		// A request arriving in the window: announced live, to whoever is
		// subscribed at this instant.
		reg.BroadcastAll(permReq("arrived-mid-attach", "sid-perm"))
		return []wire.Envelope{permReq("was-outstanding", "sid-perm")}
	}

	fresh := make(chan wire.Envelope, 32)
	e.Subscribe(fresh)

	got := permIDsOf(fresh)
	var sawMid bool
	for _, id := range got {
		if id == "arrived-mid-attach" {
			sawMid = true
		}
	}
	if !sawMid {
		t.Fatalf("delivered %v: a request broadcast while this client was attaching never reached it, "+
			"which is the drop this whole change is about", got)
	}
}

// TestBackfill_ReachesASubscriberOfARealSpawnedSession — the same repair, but
// through the Entry the registry actually builds.
//
// The three tests above hand-build an Entry and set `reg` on it, so every one of
// them passes with spawnEntry's `reg: r` deleted — the field would be nil for
// every session a daemon ever serves and the backfill would be silently inert.
// This is the test that fails for that mutation, and it is why it goes through
// GetOrSpawnEntry rather than a literal.
func TestBackfill_ReachesASubscriberOfARealSpawnedSession(t *testing.T) {
	fp := &fakeProvider{sess: &fakeSession{sid: "spawned-sid", events: make(chan agent.Event, 8)}}
	reg := NewRegistry(fp)
	reg.PendingBackfill = func() []wire.Envelope {
		return []wire.Envelope{permReq("req-spawned", "spawned-sid")}
	}

	e, err := reg.GetOrSpawnEntry(context.Background(), "", "fake")
	if err != nil {
		t.Fatalf("GetOrSpawnEntry: %v", err)
	}

	ch := make(chan wire.Envelope, 8)
	e.Subscribe(ch)
	if ids := permIDsOf(ch); len(ids) != 1 || ids[0] != "req-spawned" {
		t.Fatalf("a subscriber of a registry-spawned session got %v, want [req-spawned]; "+
			"the Entry the daemon builds cannot reach the backfill at all", ids)
	}
}

// TestBackfill_IsInertWithoutAWiredSource — a Registry nobody wired (every
// daemon before tether#132, and most of this file's fixtures) must behave exactly
// as it did. Cheap, and it is the assertion that would catch a backfill that
// synthesises envelopes of its own rather than reporting what it is given.
func TestBackfill_IsInertWithoutAWiredSource(t *testing.T) {
	e := &Entry{subs: make(map[chan wire.Envelope]struct{}), regKey: "sid-perm"}
	reg := &Registry{sessions: map[string]*Entry{"sid-perm": e}}
	e.reg = reg

	ch := make(chan wire.Envelope, 4)
	e.Subscribe(ch)
	if got := len(ch); got != 0 {
		t.Fatalf("an unwired Registry queued %d envelopes onto a new subscriber, want 0", got)
	}

	reg.PendingBackfill = func() []wire.Envelope { return nil }
	ch2 := make(chan wire.Envelope, 4)
	e.Subscribe(ch2)
	if got := len(ch2); got != 0 {
		t.Fatalf("an empty backfill queued %d envelopes, want 0", got)
	}
}

// ============================================================================
// tether#137 — a session whose agent is gone stops offering its pending
// permission requests. Registry.WithdrawPending is the seam; teardown is the
// only caller, and WHEN it is called is the whole safety argument.
// ============================================================================

// withdrawSpy records the sids teardown reported, under a mutex because the call
// happens on the session's own fanOut goroutine.
type withdrawSpy struct {
	mu   sync.Mutex
	sids []string
}

func (w *withdrawSpy) hook() func(string) {
	return func(sid string) {
		w.mu.Lock()
		w.sids = append(w.sids, sid)
		w.mu.Unlock()
	}
}

func (w *withdrawSpy) seen() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]string(nil), w.sids...)
}

// waitForWithdrawn polls until the spy has been called n times, bounded.
func waitForWithdrawn(t *testing.T, w *withdrawSpy, n int) {
	t.Helper()
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if len(w.seen()) >= n {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("WithdrawPending was called %v, want %d call(s)", w.seen(), n)
}

// TestTeardown_WithdrawsThePendingRequestsOfTheEndedSession — the affirmative
// direction of tether#137's core assertion, taken at the seam teardown owns.
//
// Driven by CLOSING Events(), which is the one signal that means the agent is
// really gone, and it is closed by hand rather than by racing a disconnect: the
// point is not how fast the reap is (tether#134 §2.4 measured that at anywhere
// between 0.03s and ~43s) but that the withdrawal hangs off the reap and not off
// the disconnect.
func TestTeardown_WithdrawsThePendingRequestsOfTheEndedSession(t *testing.T) {
	fs := &fakeSession{sid: "sid-withdraw", events: make(chan agent.Event, 8)}
	reg := NewRegistry(&fakeProvider{sess: fs})
	spy := &withdrawSpy{}
	reg.WithdrawPending = spy.hook()

	if _, err := reg.GetOrSpawnEntry(context.Background(), "", "fake"); err != nil {
		t.Fatalf("GetOrSpawnEntry: %v", err)
	}
	fs.events <- agent.Event{Kind: agent.EventInit, SessionID: "sid-withdraw"}
	waitForRegistered(t, reg, "sid-withdraw")

	// A live session's requests are still worth answering, so nothing may have been
	// withdrawn yet. Without this the test would also pass on an implementation
	// that withdrew at spawn time.
	if got := spy.seen(); len(got) != 0 {
		t.Fatalf("WithdrawPending was called %v while the session was LIVE", got)
	}

	close(fs.events)
	waitForEvicted(t, reg, "sid-withdraw")
	waitForWithdrawn(t, spy, 1)

	if got := spy.seen(); len(got) != 1 || got[0] != "sid-withdraw" {
		t.Fatalf("WithdrawPending calls = %v, want exactly [sid-withdraw]", got)
	}
}

// TestTeardown_DoesNotWithdrawFromASessionWhoseAgentIsStillAlive — the other
// direction, and the one the spec singles out as this change's own failure mode
// (§5-F, "misfire mode").
//
// tether#134 §2.4 measured an accidental grace period of 0 to ~43 seconds: a
// reconnect that lands inside it ADOPTS the live Entry (§2.3 connection 3 — no
// new pid, `SessionID resolved` logged before the prompt) and the turn keeps
// running. A withdrawal driven from the client going away instead of from the
// reap would take away a prompt that was still answerable in exactly that
// window, and the user would watch a card vanish for a tool call that is still
// waiting for it.
//
// So this drives the whole disconnect-and-adopt sequence on one live entry —
// subscriber leaves, subscriber count reaches zero, a new subscriber attaches to
// the SAME entry — and asserts nothing was withdrawn at any point. The positive
// control at the end is what stops that from being vacuous: the identical hook
// does fire once the agent's Events() really closes.
func TestTeardown_DoesNotWithdrawFromASessionWhoseAgentIsStillAlive(t *testing.T) {
	fs := &fakeSession{sid: "sid-adopted", events: make(chan agent.Event, 8)}
	reg := NewRegistry(&fakeProvider{sess: fs})
	spy := &withdrawSpy{}
	reg.WithdrawPending = spy.hook()

	e, err := reg.GetOrSpawnEntry(context.Background(), "", "fake")
	if err != nil {
		t.Fatalf("GetOrSpawnEntry: %v", err)
	}
	fs.events <- agent.Event{Kind: agent.EventInit, SessionID: "sid-adopted"}
	waitForRegistered(t, reg, "sid-adopted")

	// The connection that raised the request.
	first := make(chan wire.Envelope, 32)
	e.Subscribe(first)

	// It goes away — a reload, or a phone that lost signal. This is the moment a
	// naive implementation would call "the session ended".
	e.Unsubscribe(first)
	e.subsMu.RLock()
	subs := len(e.subs)
	e.subsMu.RUnlock()
	if subs != 0 {
		t.Fatalf("fixture: %d subscribers left, want 0 — the disconnect is not staged", subs)
	}
	if got := spy.seen(); len(got) != 0 {
		t.Fatalf("WithdrawPending was called %v when the last CLIENT left; the agent "+
			"is still alive and its requests are still answerable", got)
	}

	// The reconnect lands inside the window and adopts the live entry: same *Entry,
	// no respawn. Asserted, because "adoption happened" is the premise of the whole
	// case.
	if got := entryFor(reg, "sid-adopted"); got != e {
		t.Fatalf("fixture: the sid no longer resolves to the live entry (%p vs %p)", got, e)
	}
	second := make(chan wire.Envelope, 32)
	e.Subscribe(second)
	if got := spy.seen(); len(got) != 0 {
		t.Fatalf("WithdrawPending was called %v after the reconnect adopted a LIVE agent", got)
	}

	// The turn that survived can still be answered, which is what "still
	// answerable" means in practice.
	fs.events <- agent.Event{Kind: agent.EventText, Text: "still answering"}
	waitForEnvelope(t, second)

	// Positive control. Every assertion above also holds for a hook that is wired
	// to nothing, so prove the same hook does fire on the signal that means the
	// agent is really gone.
	close(fs.events)
	waitForEvicted(t, reg, "sid-adopted")
	waitForWithdrawn(t, spy, 1)
	if got := spy.seen(); len(got) != 1 || got[0] != "sid-adopted" {
		t.Fatalf("WithdrawPending calls after the agent died = %v, want [sid-adopted]", got)
	}
}

// TestTeardown_WithdrawIsInertWithoutAWiredHook — every daemon before tether#137
// and every fixture in this file that does not set the field. Cheap, and it is
// what would catch a teardown that dereferenced the hook unconditionally.
func TestTeardown_WithdrawIsInertWithoutAWiredHook(t *testing.T) {
	fs := &fakeSession{sid: "sid-unwired", events: make(chan agent.Event, 8)}
	reg := NewRegistry(&fakeProvider{sess: fs})

	if _, err := reg.GetOrSpawnEntry(context.Background(), "", "fake"); err != nil {
		t.Fatalf("GetOrSpawnEntry: %v", err)
	}
	fs.events <- agent.Event{Kind: agent.EventInit, SessionID: "sid-unwired"}
	waitForRegistered(t, reg, "sid-unwired")

	close(fs.events)
	waitForEvicted(t, reg, "sid-unwired")
}

// entryFor is a test-only lookup of the entry registered under sid, so a case can
// assert that a reconnect ADOPTED a live entry rather than that some entry exists.
// A plain function and not a method, like regLen above: the production type's
// method set is not the place for a fixture's convenience.
func entryFor(reg *Registry, sid string) *Entry {
	reg.mu.RLock()
	defer reg.mu.RUnlock()
	return reg.sessions[sid]
}

// waitForEnvelope polls until ch holds at least one envelope, bounded.
func waitForEnvelope(t *testing.T, ch chan wire.Envelope) {
	t.Helper()
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if len(ch) > 0 {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("the adopting subscriber received nothing from the still-live session")
}

// ============================================================================
// tether#137 F2 — say once, truthfully, that the turn did not survive.
//
// tether#134 §2.4 measured a reload that keeps the in-flight turn some of the
// time and kills it the rest, decided by whether a UDP close got out — and
// tether says nothing in either case, so "does a reload cost me my answer" has no
// learnable answer. This does not change which happens; it reports which did.
// ============================================================================

// registerEntry publishes a hand-built Entry under sid, which is how these cases
// model THE NEXT connection: the reload attaches to a brand-new Entry for the same
// sid, and what has to survive the gap is a fact held by the Registry, not by the
// corpse. Building it by hand rather than spawning again keeps the case about the
// hand-off and not about the provider.
func registerEntry(reg *Registry, sid string) *Entry {
	e := &Entry{subs: make(map[chan wire.Envelope]struct{}), regKey: sid, reg: reg}
	reg.mu.Lock()
	reg.sessions[sid] = e
	reg.mu.Unlock()
	return e
}

// regKeyOfForTest reads the key an entry is registered under, so a fixture can
// assert its own premise instead of assuming it.
func regKeyOfForTest(reg *Registry, e *Entry) string { return reg.regKeyOf(e) }

// noticeTexts returns the text of every `notice` payload queued on ch, and drains
// it. Reading the payload rather than counting envelopes, because the whole point
// of F2 is WHAT is said.
func noticeTexts(ch chan wire.Envelope) []string {
	var out []string
	for {
		select {
		case env := <-ch:
			p, ok := env.Payload.(map[string]any)
			if !ok || p["type"] != "notice" {
				continue
			}
			if s, ok := p["text"].(string); ok {
				out = append(out, s)
			}
		default:
			return out
		}
	}
}

// killTurnMidAnswer drives one session to the state this whole block is about: it
// init'd, a prompt is in flight, and then the agent's event stream closes with no
// result — which is what a client disconnect does, via the ctx cancel that SIGKILLs
// the subprocess.
//
// The stream is closed BY HAND rather than by cancelling a context and hoping. The
// reap latency is the one thing tether#134 measured as unpredictable (0.03s to
// ~43s), so a case that waited for it would be timing the transport rather than
// testing this code.
func killTurnMidAnswer(t *testing.T, reg *Registry, fs *fakeSession, sid string, complete bool) {
	t.Helper()
	if _, err := reg.GetOrSpawnEntry(context.Background(), "", "fake"); err != nil {
		t.Fatalf("GetOrSpawnEntry: %v", err)
	}
	fs.events <- agent.Event{Kind: agent.EventInit, SessionID: sid}
	waitForRegistered(t, reg, sid)

	e := entryFor(reg, sid)
	if e == nil {
		t.Fatalf("fixture: %q is not registered", sid)
	}
	if err := e.sendPrompt(context.Background(), "how do I..."); err != nil {
		t.Fatalf("sendPrompt: %v", err)
	}
	if n := e.turnsInFlight.Load(); n != 1 {
		t.Fatalf("fixture: turnsInFlight = %d after one prompt, want 1", n)
	}
	if complete {
		// The turn ends properly first, so the loss this block reports never happened.
		fs.events <- agent.Event{Kind: agent.EventResult, Text: "done"}
		deadline := time.Now().Add(500 * time.Millisecond)
		for time.Now().Before(deadline) && e.turnsInFlight.Load() != 0 {
			time.Sleep(time.Millisecond)
		}
		if n := e.turnsInFlight.Load(); n != 0 {
			t.Fatalf("fixture: turnsInFlight = %d after the result, want 0", n)
		}
	}
	close(fs.events)
	waitForEvicted(t, reg, sid)
}

// TestInterruptedTurn_TellsTheNextClientOnce — the affirmative direction.
func TestInterruptedTurn_TellsTheNextClientOnce(t *testing.T) {
	fs := &fakeSession{sid: "sid-cut", events: make(chan agent.Event, 8)}
	reg := NewRegistry(&fakeProvider{sess: fs})
	killTurnMidAnswer(t, reg, fs, "sid-cut", false)

	// The reload: a new connection on the same sid, subscribing a fresh channel.
	next := registerEntry(reg, "sid-cut")
	first := make(chan wire.Envelope, 8)
	next.Subscribe(first)

	got := noticeTexts(first)
	if len(got) != 1 || got[0] != interruptedTurnNotice {
		t.Fatalf("notices delivered to the reconnecting client = %v, want exactly one %q; "+
			"without it a reload silently drops the answer the user was waiting for",
			got, interruptedTurnNotice)
	}

	// ONCE. A second tab, or the reconnect ladder trying again, must not re-report a
	// loss that has already been explained.
	second := make(chan wire.Envelope, 8)
	next.Subscribe(second)
	if got := noticeTexts(second); len(got) != 0 {
		t.Fatalf("a second subscriber was told again: %v", got)
	}
}

// TestInterruptedTurn_SaysNothingWhenTheTurnCompleted — the other direction, and
// the one that keeps this from being a line printed on every disconnect.
//
// Closing a session whose turn ENDED is the overwhelmingly common teardown: the
// user read the answer and closed the tab. Nothing was lost, so nothing may be
// claimed. Same fixture, one event different.
func TestInterruptedTurn_SaysNothingWhenTheTurnCompleted(t *testing.T) {
	fs := &fakeSession{sid: "sid-done", events: make(chan agent.Event, 8)}
	reg := NewRegistry(&fakeProvider{sess: fs})
	killTurnMidAnswer(t, reg, fs, "sid-done", true)

	next := registerEntry(reg, "sid-done")
	ch := make(chan wire.Envelope, 8)
	next.Subscribe(ch)
	if got := noticeTexts(ch); len(got) != 0 {
		t.Fatalf("a session whose turn completed still reported a loss: %v", got)
	}
}

// TestInterruptedTurn_SaysNothingForAResumeThatNeverInitialised — the false
// positive that would be a lie, not merely noise (tether#50).
//
// A failed `cc --resume` produces an event stream that closes without ever
// emitting system/init, and its turn is left OPEN on purpose: Attachment.resolve
// is about to respawn and answer that same prompt for real. Reporting "the rest
// was never generated" moments before the answer arrives is exactly the
// caught-out notice this change exists to remove, one layer along.
//
// The open turn is asserted BEFORE the stream closes, which is the only place it
// can be: teardown's clearTurns resets the count, so a check afterwards reads zero
// whatever happened and would turn this into a case that passes for the wrong
// reason. Nothing between the assertion and the close can move the count — the
// only decrementers are endTurn (reached from the result and error branches, and
// the init-less result is dropped one line above it by fanOut's own guard) and
// clearTurns. So "1 here" is "1 when the stream closed", and the discriminator
// under test is sawInit and nothing else.
func TestInterruptedTurn_SaysNothingForAResumeThatNeverInitialised(t *testing.T) {
	fs := &fakeSession{sid: "sid-noinit", events: make(chan agent.Event, 8)}
	reg := NewRegistry(&fakeProvider{sess: fs})

	// Spawned WITH a sid, which is what a failed resume is: a reconnect carrying
	// one. What that buys the case is the thing that makes it a gate rather than a
	// tautology — the entry ends up registered under a NON-EMPTY key even though no
	// init ever arrives, so an implementation that read the entry's key instead of
	// fanOut's `sid` (which is what a teardown placement has to do, since teardown
	// cannot see sawInit) has a real sid to get wrong.
	//
	// The key is READ, not assumed: GetOrSpawnEntry mints a fresh id for a sid it
	// does not track (tether#49), so it is not the one asked for here. Only its
	// non-emptiness is load-bearing.
	e, err := reg.GetOrSpawnEntry(context.Background(), "sid-noinit", "fake")
	if err != nil {
		t.Fatalf("GetOrSpawnEntry: %v", err)
	}
	key := regKeyOfForTest(reg, e)
	if key == "" {
		t.Fatal("fixture: the entry has no registration, so this case cannot " +
			"distinguish the sawInit gate from an empty-sid guard")
	}
	if err := e.sendPrompt(context.Background(), "the prompt resolve will replay"); err != nil {
		t.Fatalf("sendPrompt: %v", err)
	}
	if n := e.turnsInFlight.Load(); n != 1 {
		t.Fatalf("fixture: turnsInFlight = %d, want 1 — this case is only meaningful "+
			"with a turn still open when the init-less stream ends", n)
	}

	// cc's failed-resume artefact: one init-less, text-less result, then EOF.
	fs.events <- agent.Event{Kind: agent.EventResult}
	close(fs.events)
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) && fs.Closes() == 0 {
		time.Sleep(time.Millisecond)
	}
	if fs.Closes() == 0 {
		t.Fatal("fixture: the session was never torn down")
	}

	// Attached under the SAME key the dead entry held, because that is the key a
	// wrong implementation would have marked.
	next := registerEntry(reg, key)
	ch := make(chan wire.Envelope, 8)
	next.Subscribe(ch)
	if got := noticeTexts(ch); len(got) != 0 {
		t.Fatalf("a failed resume reported the loss of an answer that is about to be "+
			"replayed and answered: %v", got)
	}
}

// TestInterruptedTurn_IsDeliveredWithoutAWiredBackfill — F2 rides Subscribe's
// backfill hop, and PendingBackfill (tether#132) is nil on any daemon that wires
// no permission API. The two must not share an off switch.
func TestInterruptedTurn_IsDeliveredWithoutAWiredBackfill(t *testing.T) {
	fs := &fakeSession{sid: "sid-nobackfill", events: make(chan agent.Event, 8)}
	reg := NewRegistry(&fakeProvider{sess: fs})
	if reg.PendingBackfill != nil {
		t.Fatal("fixture: PendingBackfill must be unwired for this case")
	}
	killTurnMidAnswer(t, reg, fs, "sid-nobackfill", false)

	next := registerEntry(reg, "sid-nobackfill")
	ch := make(chan wire.Envelope, 8)
	next.Subscribe(ch)
	if got := noticeTexts(ch); len(got) != 1 {
		t.Fatalf("notices = %v, want one; a nil PendingBackfill must not switch this off", got)
	}
}

// TestInterruptedTurn_TellsOnlyTheSessionItHappenedTo — the list is keyed by sid
// because a reader attaching to a different conversation has lost nothing.
func TestInterruptedTurn_TellsOnlyTheSessionItHappenedTo(t *testing.T) {
	fs := &fakeSession{sid: "sid-mine", events: make(chan agent.Event, 8)}
	reg := NewRegistry(&fakeProvider{sess: fs})
	killTurnMidAnswer(t, reg, fs, "sid-mine", false)

	other := registerEntry(reg, "sid-theirs")
	ch := make(chan wire.Envelope, 8)
	other.Subscribe(ch)
	if got := noticeTexts(ch); len(got) != 0 {
		t.Fatalf("a subscriber of an unrelated session was told: %v", got)
	}

	// Positive control: the fact is still there for the session it belongs to.
	mine := registerEntry(reg, "sid-mine")
	mineCh := make(chan wire.Envelope, 8)
	mine.Subscribe(mineCh)
	if got := noticeTexts(mineCh); len(got) != 1 {
		t.Fatalf("notices for the session that lost the turn = %v, want one", got)
	}
}

// TestInterruptedTurn_RememberedSetIsBounded — a user who abandons a half-answered
// session never collects the notice, so the list has to be bounded by the code and
// not by the traffic. tether#56 is this repo's own record of what "one per session,
// held until the daemon exits" costs.
func TestInterruptedTurn_RememberedSetIsBounded(t *testing.T) {
	reg := NewRegistry()
	for i := 0; i < maxInterruptedTurns*3; i++ {
		reg.markTurnInterrupted(fmt.Sprintf("sid-%d", i))
	}
	reg.interruptedTurnsMu.Lock()
	n := len(reg.interruptedTurns)
	reg.interruptedTurnsMu.Unlock()
	if n > maxInterruptedTurns {
		t.Fatalf("interruptedTurns holds %d sids, want at most %d", n, maxInterruptedTurns)
	}
	// The ones kept are the most recent, which are the sessions somebody might
	// still come back to.
	if !reg.takeTurnInterrupted(fmt.Sprintf("sid-%d", maxInterruptedTurns*3-1)) {
		t.Fatal("the most recent sid was evicted")
	}
	if reg.takeTurnInterrupted("sid-0") {
		t.Fatal("the oldest sid survived the cap")
	}
}

// TestInterruptedTurn_MarkIsIdempotentAndIgnoresAnEmptySid — a sid can be torn
// down more than once, and an entry that never registered has none.
func TestInterruptedTurn_MarkIsIdempotentAndIgnoresAnEmptySid(t *testing.T) {
	reg := NewRegistry()
	reg.markTurnInterrupted("sid-x")
	reg.markTurnInterrupted("sid-x")
	reg.markTurnInterrupted("")

	// Read the list itself for the empty sid, not takeTurnInterrupted(""): that has
	// a guard of its own, so asking it would answer "false" whether or not the empty
	// sid was ever recorded. An unaddressable entry cannot be delivered to anyone
	// and just occupies one of the bounded slots.
	reg.interruptedTurnsMu.Lock()
	held := append([]string(nil), reg.interruptedTurns...)
	reg.interruptedTurnsMu.Unlock()
	if len(held) != 1 || held[0] != "sid-x" {
		t.Fatalf("interruptedTurns = %q, want exactly [sid-x]", held)
	}

	if !reg.takeTurnInterrupted("sid-x") {
		t.Fatal("the sid was not remembered")
	}
	if reg.takeTurnInterrupted("sid-x") {
		t.Fatal("the sid was remembered twice, so the reader would be told twice")
	}
	if reg.takeTurnInterrupted("") {
		t.Fatal(`an empty sid matched`)
	}
}

// The tests below cover the CHAT half of the permission-gate wiring
// (tether#149): what spawnEntry puts into agent.SpawnConfig.Env, which both
// providers append to os.Environ() before exec.
//
// They are deliberately narrow. Nothing in this package can see the shell path
// (internal/server/wt_shell.go builds a second cc environment from the same
// cchook.Gate), and a gate entry present here and missing there fails OPEN on
// the shell path with this file still green. The test that spans both is
// TestBothCCSpawnPathsCarryTheGate in internal/server.

// permGateEnv runs one fresh spawn with the given gate and returns the Env the
// chat path handed the provider.
func permGateEnv(t *testing.T, gate cchook.Gate) []string {
	t.Helper()
	fp := &fakeProvider{sess: &fakeSession{sid: "gate-sid", events: make(chan agent.Event, 8)}}
	reg := NewRegistry(fp)
	reg.PermGate = gate
	if _, err := reg.GetOrSpawnEntry(context.Background(), "", "fake"); err != nil {
		t.Fatalf("GetOrSpawnEntry: %v", err)
	}
	return fp.lastCfg.Env
}

// TestSpawnEntry_ChatChildCarriesTheWholeGate — an armed gate must reach the cc
// subprocess as BOTH of its entries.
//
// Before tether#149 this path injected the endpoint and nothing else, so every
// cc the daemon spawned for chat arrived unmarked. Unmarked is not a neutral
// state: it is the exact input for which the hook exits 0 on purpose, to avoid
// breaking the owner's own standalone cc. A daemon-spawned cc that lost its
// endpoint was therefore indistinguishable from one the daemon never spawned,
// and the gate resolved as allow (tether#117 A4b).
func TestSpawnEntry_ChatChildCarriesTheWholeGate(t *testing.T) {
	const ep = "https://127.0.0.1:8443/api/v1/permission/request"
	gate := cchook.Gate{Managed: true, Endpoint: ep}

	got := permGateEnv(t, gate)
	want := gate.Env()
	if len(got) != len(want) {
		t.Fatalf("SpawnConfig.Env = %v, want exactly the gate's %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("SpawnConfig.Env[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

// TestSpawnEntry_ChatChildIsMarkedEvenWithNoEndpoint — the mark does NOT depend
// on there being an endpoint, and that is the whole difference between the two
// states the hook has to tell apart. A gate that only injected something when it
// had an endpoint would leave "daemon-spawned, endpoint missing" looking exactly
// like "not daemon-spawned", which is the fail-open shape again with a new
// variable in it.
func TestSpawnEntry_ChatChildIsMarkedEvenWithNoEndpoint(t *testing.T) {
	got := permGateEnv(t, cchook.Gate{Managed: true})
	if len(got) != 1 || got[0] != cchook.EnvManaged+"="+cchook.ManagedValue {
		t.Errorf("SpawnConfig.Env = %v, want just the %s mark", got, cchook.EnvManaged)
	}
}

// TestSpawnEntry_UnmanagedGateInjectsNothing — a Registry built outside the
// daemon, or a daemon started with TETHER_NO_PERMISSION_HOOK=1, must leave its
// children completely unmarked. Marking them would turn a hook entry surviving
// in ~/.claude/settings.json from a harmless leftover into a deny on every tool
// call. The stray-endpoint row is there because Managed is the decision, not
// "an endpoint happens to be set".
func TestSpawnEntry_UnmanagedGateInjectsNothing(t *testing.T) {
	for _, gate := range []cchook.Gate{
		{},
		{Endpoint: "https://127.0.0.1:8443/api/v1/permission/request"},
	} {
		if got := permGateEnv(t, gate); len(got) != 0 {
			t.Errorf("gate %+v: SpawnConfig.Env = %v, want nothing injected", gate, got)
		}
	}
}

// TestSpawnEntry_DoesNotAppendIntoTheCallersEnvArray — cfg arrives by value but
// its Env slice still shares a backing array with the caller's, so appending the
// gate's entries in place would write into a slice spawnEntry does not own. No
// caller passes a non-nil Env today, which is exactly why this would be found
// the hard way; the sentinel below is what the caller would silently lose.
func TestSpawnEntry_DoesNotAppendIntoTheCallersEnvArray(t *testing.T) {
	backing := make([]string, 2, 8)
	backing[0] = "CALLER_SUPPLIED=1"
	backing[1] = "MUST_SURVIVE=yes"

	fp := &fakeProvider{sess: &fakeSession{sid: "alias-sid", events: make(chan agent.Event, 8)}}
	reg := NewRegistry(fp)
	reg.PermGate = cchook.Gate{Managed: true, Endpoint: "https://127.0.0.1:8443/x"}

	// Hand over only the first element, leaving spare capacity that still holds
	// the second — the shape an append-in-place would overwrite.
	_, _, err := reg.spawnEntry(context.Background(), "fake",
		agent.SpawnConfig{Env: backing[:1]}, WorkspaceBinding{}, spawnIfAbsent)
	if err != nil {
		t.Fatalf("spawnEntry: %v", err)
	}
	if backing[1] != "MUST_SURVIVE=yes" {
		t.Errorf("caller's env backing array element 1 = %q, want %q — spawnEntry appended in place",
			backing[1], "MUST_SURVIVE=yes")
	}
	if n := len(fp.lastCfg.Env); n != 3 {
		t.Errorf("SpawnConfig.Env has %d entries (%v), want the caller's 1 plus the gate's 2",
			n, fp.lastCfg.Env)
	}
}
