package session

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/piaobeizu/tether/internal/agent"
	"github.com/piaobeizu/tether/internal/wire"
)

// ─── tether#54: the entry is registered under its own sid, from before its
// agent process existed ─────────────────────────────────────────────────────
//
// registry_test.go's fakeProvider models the OTHER provider shape — one that
// mints its own id and ignores SpawnConfig.SessionID (opencode). Neither shape
// can stand in for the other here, because the whole subject of this file is
// WHICH key an entry is registered under and when.

// pinningProvider models a provider that ADOPTS the id it is spawned with — real
// cc, measured (mem_2ruSlrHR ①: cc echoes the caller's `--session-id` verbatim on
// system/init and result; ② a `--resume`d session's id does not drift either). It
// hands back a distinct session per spawn, so a test can tell a reused entry from
// a duplicated one.
type pinningProvider struct {
	// gate makes every session's SessionID() block until the test closes its
	// sidReady, so Resolve can be released at a chosen moment — the harness the
	// tether#50 review used to make the ownership race reproduce every time.
	gate bool

	mu       sync.Mutex
	cfgs     []agent.SpawnConfig
	sessions []*fakeSession
}

func (p *pinningProvider) Name() string { return "fake" }

func (p *pinningProvider) Spawn(_ context.Context, cfg agent.SpawnConfig) (agent.Session, error) {
	// Exactly what cc does with these two flags, and the reason spawnEntry can
	// know the key before this call: one of them is always set, and whichever it
	// is IS the session's id.
	sid := cfg.SessionID
	if sid == "" {
		sid = cfg.ResumeSessionID
	}
	fs := &fakeSession{sid: sid, events: make(chan agent.Event, 8)}
	if p.gate {
		fs.sidReady = make(chan struct{})
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.cfgs = append(p.cfgs, cfg)
	p.sessions = append(p.sessions, fs)
	return fs, nil
}

func (p *pinningProvider) Spawns() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.cfgs)
}

func (p *pinningProvider) LastCfg() agent.SpawnConfig {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.cfgs) == 0 {
		return agent.SpawnConfig{}
	}
	return p.cfgs[len(p.cfgs)-1]
}

func (p *pinningProvider) Configs() []agent.SpawnConfig {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]agent.SpawnConfig(nil), p.cfgs...)
}

func (p *pinningProvider) Sessions() []*fakeSession {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]*fakeSession(nil), p.sessions...)
}

// closeAll ends every session this provider handed out, so each fanOut returns
// and the registry drains.
func (p *pinningProvider) closeAll() {
	for _, fs := range p.Sessions() {
		if fs.sidReady != nil {
			select {
			case <-fs.sidReady:
			default:
				close(fs.sidReady)
			}
		}
		close(fs.events)
	}
}

// entryOf reads an attachment's current Entry under its own lock — white-box,
// same-package, so a test can assert that two attaches share one session rather
// than inferring it from a spawn count alone.
func entryOf(a *Attachment) *Entry {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.entry
}

// captureLogs redirects the default slog logger into a buffer for the duration of
// one test and hands back a reader for what was written. Used to assert on the
// self-check warning, which is the whole observable of the "did the agent adopt
// the id we pinned?" question — there is nothing else to look at when the answer
// is yes.
func captureLogs(t *testing.T) func() string {
	t.Helper()
	var mu sync.Mutex
	buf := &lockedBuffer{mu: &mu}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return buf.String
}

// lockedBuffer is a bytes.Buffer safe for the daemon's own goroutines to write
// while the test reads it.
type lockedBuffer struct {
	mu  *sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// TestSpawn_RegistersUnderTheMintedSidBeforeReturning — the invariant this slice
// exists to establish: a fresh session is in the registry under its FINAL sid by
// the time the spawn call returns.
//
// Note what the assertions deliberately do NOT do: no polling, no event, no
// prompt. Before tether#54 none of this was reachable that early — the entry was
// registered under a `pending-%p` placeholder and only moved to its real sid by a
// goroutine parked in sess.SessionID(), which under stream-json cannot resolve
// until the client's first prompt has been answered. Every sid-keyed lookup in
// this package was therefore answering "no such session" about a session that was
// right there.
func TestSpawn_RegistersUnderTheMintedSidBeforeReturning(t *testing.T) {
	p := &pinningProvider{}
	reg := NewRegistry(p)
	defer p.closeAll()

	e, err := reg.GetOrSpawnEntry(context.Background(), "", "fake")
	if err != nil {
		t.Fatalf("GetOrSpawnEntry: %v", err)
	}

	sid := p.LastCfg().SessionID
	if sid == "" {
		t.Fatal("no SessionID was pinned on the fresh spawn; there is no key to register under")
	}
	if got := regKeys(reg); len(got) != 1 || got[0] != sid {
		t.Fatalf("registry keys = %v, want exactly [%q] — the entry must be keyed by its sid, not a placeholder", got, sid)
	}
	if !reg.IsLive(sid) {
		t.Error("IsLive = false for the sid the session was just spawned under")
	}
	if found, ok := reg.liveEntry(sid); !ok || found != e {
		t.Errorf("liveEntry(%q) = (%p, %v), want the entry spawnEntry returned (%p)", sid, found, ok, e)
	}
}

// TestSpawn_RegistersNoKeyItWasNotGiven — the placeholder is retired, not merely
// bypassed.
//
// Asserted as a SET EQUALITY against the ids the registry handed to the provider,
// not as an absence of the string "pending-". A prefix tripwire is worth very
// little: a review demonstrated that reinstating the old placeholder verbatim but
// renaming it `tmp-%p` walked straight past it. Every key must be an id some
// caller could ask about — a sid we minted, or a sid a client asked to resume.
func TestSpawn_RegistersNoKeyItWasNotGiven(t *testing.T) {
	p := &pinningProvider{}
	reg := NewRegistry(p)
	defer p.closeAll()

	if _, err := reg.GetOrSpawnEntry(context.Background(), "", "fake"); err != nil {
		t.Fatalf("GetOrSpawnEntry: %v", err)
	}
	if _, err := reg.Attach(context.Background(), "some-old-sid", "fake", ""); err != nil {
		t.Fatalf("Attach: %v", err)
	}

	want := []string{}
	for _, cfg := range p.Configs() {
		sid := cfg.SessionID
		if sid == "" {
			sid = cfg.ResumeSessionID
		}
		want = append(want, sid)
	}
	sort.Strings(want)
	got := regKeys(reg)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("registry keys = %v, want exactly the ids handed to Spawn %v — a key that is neither "+
			"a minted sid nor a requested one is a placeholder by another name", got, want)
	}
}

// TestOwnedByOther_DoesNotRejectASessionNobodyOwnsYet — the admission gate
// serveChat runs before Attach.
//
// This is the seam tether#54 broke and had to fix: registering under the real sid
// makes `IsLive(sid)` true from the moment Attach returns, and the gate's old
// composition `IsLive(sid) && !IsOwner(sid, clientID)` reads an unowned session as
// "owned by someone else". Ownership is claimed only after Resolve, which for cc
// waits for the user's first prompt — so a second tab (same credential, same
// client id) would be refused for as long as the first tab took to type, see
// nothing rendered for the error, and silently reconnect-loop. Which is the very
// case tether#54 exists to make work.
func TestOwnedByOther_DoesNotRejectASessionNobodyOwnsYet(t *testing.T) {
	p := &pinningProvider{}
	reg := NewRegistry(p)
	defer p.closeAll()

	att, err := reg.Attach(context.Background(), "", "fake", "")
	if err != nil {
		t.Fatalf("Attach: %v", err)
	}
	sid := p.LastCfg().SessionID

	// Nobody has claimed it: no client may be turned away.
	if reg.OwnedByOther(sid, "client-1") {
		t.Error("an unowned live session was reported as owned by another client")
	}
	if reg.OwnedByOther(sid, "client-2") {
		t.Error("an unowned live session was reported as owned by another client")
	}
	// The composition the gate used to run would have rejected client-2 here: the
	// session is live and IsOwner is false for an empty owner. Asserted so the
	// reason OwnedByOther exists cannot quietly stop being true.
	if !(reg.IsLive(sid) && !reg.IsOwner(sid, "client-2")) {
		t.Error("the old IsLive+!IsOwner composition no longer rejects an unowned session; " +
			"OwnedByOther's reason for existing has changed and its doc needs revisiting")
	}

	// Once claimed, the rule itself is unchanged.
	if !att.SetOwner("client-1") {
		t.Fatal("first claim was rejected")
	}
	if reg.OwnedByOther(sid, "client-1") {
		t.Error("the owner was reported as another client")
	}
	if !reg.OwnedByOther(sid, "client-2") {
		t.Error("a second client was NOT reported as excluded; the ownership gate is now inert")
	}
	// A sid nobody has ever heard of, and a dead one, are both open (tether#55: a
	// session whose agent has exited is nobody's to own).
	if reg.OwnedByOther("never-existed", "client-2") {
		t.Error("an unknown sid was reported as owned")
	}
	p.Sessions()[0].dead.Store(true)
	if reg.OwnedByOther(sid, "client-2") {
		t.Error("a session whose agent has exited was still reported as owned; a second device could not recover it")
	}
}

// TestEvict_DoesNotTakeOutALiveReplacementUnderTheSameKey — the property
// Registry.evict's by-value scan exists for, which until now was only argued in
// prose.
//
// Reachable on the ordinary tether#55 path: a corpse is un-registered by the
// reconnect that noticed it, the reconnect resumes that same sid, and only THEN
// does the corpse's own fanOut return and run its deferred evict. If that evict
// deleted `e.regKey` rather than the entry it was given, it would delete the
// replacement — handing the client a session the registry has already forgotten.
func TestEvict_DoesNotTakeOutALiveReplacementUnderTheSameKey(t *testing.T) {
	corpse := &fakeSession{sid: "shared-sid", events: make(chan agent.Event, 4)}
	replacement := &fakeSession{sid: "shared-sid", events: make(chan agent.Event, 4)}
	p := &corpseThenLiveProvider{corpse: corpse, live: replacement}
	reg := NewRegistry(p)
	defer close(replacement.events)

	// Seed the first session and let it announce its sid, so both entries want the
	// same key.
	if _, err := reg.GetOrSpawnEntry(context.Background(), "", "fake"); err != nil {
		t.Fatalf("seed spawn: %v", err)
	}
	corpse.announceInit()
	waitForRegistered(t, reg, "shared-sid")
	corpseEntry := registeredEntry(reg, "shared-sid")
	if corpseEntry == nil {
		t.Fatal("seed session not registered under its sid")
	}

	// Its agent exits; the reconnect notices and un-registers it, then resumes.
	corpse.dead.Store(true)
	att, err := reg.Attach(context.Background(), "shared-sid", "fake", "")
	if err != nil {
		t.Fatalf("Attach: %v", err)
	}
	if entryOf(att).Session() != agent.Session(replacement) {
		t.Fatal("Attach did not spawn a replacement; this test needs the resume path")
	}
	if got := regKeys(reg); len(got) != 1 || got[0] != "shared-sid" {
		t.Fatalf("registry keys after the replacement spawned = %v, want [\"shared-sid\"]", got)
	}

	// NOW the corpse tears down. Run its evict DIRECTLY rather than closing its
	// stream and polling for a while: it is the same call fanOut's defer makes, and
	// invoking it here makes the assertion deterministic instead of "nothing changed
	// during the two seconds I watched".
	reg.evict(corpseEntry)
	if got := regKeys(reg); len(got) != 1 || got[0] != "shared-sid" {
		t.Fatalf("the corpse's late evict removed its live replacement; keys = %v", got)
	}
	if !reg.IsLive("shared-sid") {
		t.Error("the replacement is no longer live after the corpse was evicted")
	}

	// And again through the real teardown path, so this does not only hold for a
	// hand-called evict.
	close(corpse.events)
	waitForCount(t, reg, 1)
	if got := regKeys(reg); len(got) != 1 || got[0] != "shared-sid" {
		t.Fatalf("fanOut's deferred evict removed the live replacement; keys = %v", got)
	}
}

// TestAttach_ResumeRegistersUnderTheRequestedSidBeforeReturning — a reconnect
// names its session too: the sid it is resuming. Registering under it up front is
// what makes a second attach on the same sid FIND this one instead of starting a
// second `cc --resume` of the same transcript.
func TestAttach_ResumeRegistersUnderTheRequestedSidBeforeReturning(t *testing.T) {
	p := &pinningProvider{}
	reg := NewRegistry(p)
	defer p.closeAll()

	if _, err := reg.Attach(context.Background(), "old-sid", "fake", ""); err != nil {
		t.Fatalf("Attach: %v", err)
	}

	if got := regKeys(reg); len(got) != 1 || got[0] != "old-sid" {
		t.Fatalf("registry keys = %v, want exactly [\"old-sid\"]", got)
	}
	cfg := p.LastCfg()
	if cfg.ResumeSessionID != "old-sid" {
		t.Errorf("ResumeSessionID = %q, want \"old-sid\"", cfg.ResumeSessionID)
	}
	// Still mutually exclusive — the key came from ResumeSessionID, and pinning
	// both would make real cc exit 1 (mem_2ruSlrHR ⑧).
	if cfg.SessionID != "" {
		t.Errorf("SessionID = %q, want \"\" on a resume", cfg.SessionID)
	}
}

// TestResolve_SidIsRegisteredTheInstantResolveReturns reproduces the race an
// adversarial review of tether#50 measured at 500/500, and pins that it is now
// impossible for a provider that adopts the id it is given.
//
// The race: Resolve returns when sess.SessionID() resolves, and the old re-key
// goroutine was parked on that SAME wakeup, so the sid Resolve hands back was
// routinely not yet a key in the map. serveChat then asked an ownership question
// by that sid, got "no such session", read it as a fatal ownership race and
// dropped the connection while the user's first answer was streaming.
//
// The gate below is what makes it deterministic rather than scheduler-dependent:
// both the caller of Resolve and (under the old code) the re-key goroutine are
// released at the same instant, so the loser was almost always the map.
//
// It is a LOGICAL race, not a memory one — the map was always properly locked, so
// -race never had anything to say about it. This test is the reproducer.
func TestResolve_SidIsRegisteredTheInstantResolveReturns(t *testing.T) {
	const n = 500
	misses := 0
	for i := 0; i < n; i++ {
		p := &pinningProvider{gate: true}
		reg := NewRegistry(p)
		att, err := reg.Attach(context.Background(), "", "fake", "")
		if err != nil {
			t.Fatalf("Attach: %v", err)
		}
		fs := p.Sessions()[0]
		close(fs.sidReady) // release Resolve

		res, err := att.Resolve(context.Background())
		if err != nil {
			t.Fatalf("Resolve: %v", err)
		}
		if !reg.IsLive(res.SID) {
			misses++
		}
		close(fs.events)
	}
	if misses != 0 {
		t.Errorf("the sid Resolve returned was absent from the registry %d/%d times; "+
			"each one is an ownership check that fails against a session that is right there", misses, n)
	}
}

// TestAttach_SameDeadSidResumesOnce — two clients reconnecting with the same sid
// must produce ONE `cc --resume` of that transcript, not two.
//
// Reachable from a single browser: the sid lives in localStorage, which tabs of an
// origin share, so two tabs after a daemon restart is enough. Both used to miss
// (the first was still under its placeholder), both resumed the same transcript,
// and both then re-keyed to the same map key — so one Entry silently displaced the
// other and the displaced cc kept running, unreachable, with the two of them
// interleaving writes into one conversation file.
func TestAttach_SameDeadSidResumesOnce(t *testing.T) {
	p := &pinningProvider{}
	reg := NewRegistry(p)
	defer p.closeAll()

	a1, err := reg.Attach(context.Background(), "shared-sid", "fake", "")
	if err != nil {
		t.Fatalf("first Attach: %v", err)
	}
	a2, err := reg.Attach(context.Background(), "shared-sid", "fake", "")
	if err != nil {
		t.Fatalf("second Attach: %v", err)
	}

	if got := p.Spawns(); got != 1 {
		t.Errorf("provider spawned %d sessions for one sid, want 1 — a duplicated `cc --resume` interleaves two processes into one transcript", got)
	}
	if entryOf(a1) != entryOf(a2) {
		t.Error("the two attaches hold different entries; one of them is unreachable by its own sid")
	}
	if got := regKeys(reg); len(got) != 1 || got[0] != "shared-sid" {
		t.Errorf("registry keys = %v, want exactly [\"shared-sid\"]", got)
	}
}

// TestFanOut_AdoptsTheSidItsAgentReports — the one path that still cannot be keyed
// at spawn time: OpenCodeProvider ignores SpawnConfig.SessionID and mints its own
// id inside `opencode serve`, so the entry has to move to the id that arrives on
// the event stream. It moves ONCE, and the id it was spawned under does not linger
// as a second key.
func TestFanOut_AdoptsTheSidItsAgentReports(t *testing.T) {
	fs := &fakeSession{sid: "agent-minted-sid", events: make(chan agent.Event, 4)}
	fp := &fakeProvider{sess: fs}
	reg := NewRegistry(fp)
	defer close(fs.events)

	if _, err := reg.GetOrSpawnEntry(context.Background(), "", "fake"); err != nil {
		t.Fatalf("GetOrSpawnEntry: %v", err)
	}
	pinned := fp.lastCfg.SessionID
	if got := regKeys(reg); len(got) != 1 || got[0] != pinned {
		t.Fatalf("registry keys before init = %v, want exactly [%q]", got, pinned)
	}

	fs.announceInit()
	waitForRegistered(t, reg, "agent-minted-sid")

	if got := regKeys(reg); len(got) != 1 || got[0] != "agent-minted-sid" {
		t.Fatalf("registry keys after init = %v, want exactly [\"agent-minted-sid\"] — the pinned key must be released, not kept alongside", got)
	}
}

// TestRekey_WarnsOnlyWhenTheAgentIgnoredThePinnedSid — the self-check.
//
// The daemon minting its own uuid (tether#50) rests on a MEASURED cc behaviour:
// `--session-id` is adopted verbatim (mem_2ruSlrHR ①). Nothing used to compare the
// two, so a regression in that behaviour would have degraded silently back to
// pre-tether#50 semantics — history written under an id the entry is not keyed by,
// a transcript that truncates on reload, and Resolution.Recovered false so the
// user is not even told. The mismatch branch is where that assumption becomes an
// observable invariant, so it must be silent when the id WAS adopted and loud when
// it was not.
func TestRekey_WarnsOnlyWhenTheAgentIgnoredThePinnedSid(t *testing.T) {
	t.Run("adopted: silent", func(t *testing.T) {
		logs := captureLogs(t)
		p := &pinningProvider{}
		reg := NewRegistry(p)
		defer p.closeAll()

		e, err := reg.GetOrSpawnEntry(context.Background(), "", "fake")
		if err != nil {
			t.Fatalf("GetOrSpawnEntry: %v", err)
		}
		fs := p.Sessions()[0]
		announceAndWait(t, e, fs)

		if strings.Contains(logs(), rekeyWarn) {
			t.Errorf("cc adopting the id it was pinned to logged a re-key warning:\n%s", logs())
		}
	})

	t.Run("ignored: warns", func(t *testing.T) {
		logs := captureLogs(t)
		fs := &fakeSession{sid: "an-id-of-my-own", events: make(chan agent.Event, 4)}
		reg := NewRegistry(&fakeProvider{sess: fs})
		defer close(fs.events)

		e, err := reg.GetOrSpawnEntry(context.Background(), "", "fake")
		if err != nil {
			t.Fatalf("GetOrSpawnEntry: %v", err)
		}
		announceAndWait(t, e, fs)

		if !strings.Contains(logs(), rekeyWarn) {
			t.Errorf("an agent that ignored the pinned id did not warn; the minted-uuid assumption stays unobservable:\n%s", logs())
		}
		if !strings.Contains(logs(), "provider=fake") {
			t.Errorf("the warning does not name the provider, so an operator cannot tell a known limitation "+
				"from a cc regression:\n%s", logs())
		}
	})

	// The noise half of the same decision: a provider that mints its own id trips
	// the mismatch on EVERY session, and a warning per session is a warning nobody
	// reads. Only the first one from a given provider is a Warn.
	t.Run("ignored twice: warns once", func(t *testing.T) {
		logs := captureLogs(t)
		p := &perSpawnFakeProvider{}
		reg := NewRegistry(p)
		defer p.closeAll()

		for i := 0; i < 3; i++ {
			e, err := reg.GetOrSpawnEntry(context.Background(), "", "fake")
			if err != nil {
				t.Fatalf("spawn %d: %v", i, err)
			}
			announceAndWait(t, e, p.Sessions()[i])
		}

		if n := strings.Count(logs(), "level=WARN"); n != 1 {
			t.Errorf("three sessions from a self-minting provider produced %d WARN lines, want exactly 1 "+
				"(one per provider) — a per-session warning trains operators to ignore the only signal "+
				"that would catch cc regressing:\n%s", n, logs())
		}
	})

	// The granularity of that "once" is load-bearing and is asserted separately: a
	// single global flag would also satisfy the subtest above while letting a
	// known-noisy provider SILENCE the first real warning from another one — which
	// is the cc regression this whole check exists to catch, suppressed by opencode
	// having booted first.
	t.Run("once per provider, not once per daemon", func(t *testing.T) {
		logs := captureLogs(t)
		noisy := &namedSelfMintingProvider{name: "opencode-ish", sid: "its-own-id"}
		other := &namedSelfMintingProvider{name: "claude-code-ish", sid: "some-other-id"}
		reg := NewRegistry(noisy, other)
		defer noisy.closeAll()
		defer other.closeAll()

		for _, p := range []*namedSelfMintingProvider{noisy, other} {
			e, err := reg.GetOrSpawnEntry(context.Background(), "", p.name)
			if err != nil {
				t.Fatalf("spawn from %s: %v", p.name, err)
			}
			announceAndWait(t, e, p.Sessions()[0])
		}

		// Matched per LINE, and only on WARN lines. `logs()` is captured at Debug
		// level so the downgraded second re-key is in there too, naming its provider
		// — a whole-buffer Contains check therefore passes even with the granularity
		// collapsed, which is how this assertion was hollow on its first attempt.
		for _, name := range []string{"opencode-ish", "claude-code-ish"} {
			warned := false
			for _, line := range strings.Split(logs(), "\n") {
				if strings.Contains(line, "level=WARN") && strings.Contains(line, "provider="+name) {
					warned = true
					break
				}
			}
			if !warned {
				t.Errorf("no WARN line names provider %q — a provider that warned earlier has silenced it, "+
					"which is exactly how a real cc regression would go unreported:\n%s", name, logs())
			}
		}
	})
}

// namedSelfMintingProvider is a self-minting provider with a choosable Name(), so a
// test can register two of them in one registry and assert the self-check's
// per-provider granularity.
type namedSelfMintingProvider struct {
	name string
	sid  string

	mu       sync.Mutex
	sessions []*fakeSession
}

func (p *namedSelfMintingProvider) Name() string { return p.name }

func (p *namedSelfMintingProvider) Spawn(_ context.Context, _ agent.SpawnConfig) (agent.Session, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	fs := &fakeSession{
		sid:    fmt.Sprintf("%s-%d", p.sid, len(p.sessions)),
		events: make(chan agent.Event, 8),
	}
	p.sessions = append(p.sessions, fs)
	return fs, nil
}

func (p *namedSelfMintingProvider) Sessions() []*fakeSession {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]*fakeSession(nil), p.sessions...)
}

func (p *namedSelfMintingProvider) closeAll() {
	for _, fs := range p.Sessions() {
		close(fs.events)
	}
}

// perSpawnFakeProvider is fakeProvider with a fresh self-minting session per spawn,
// so a test can run several sessions of the same provider through rekey.
type perSpawnFakeProvider struct {
	mu       sync.Mutex
	sessions []*fakeSession
}

func (p *perSpawnFakeProvider) Name() string { return "fake" }

func (p *perSpawnFakeProvider) Spawn(_ context.Context, _ agent.SpawnConfig) (agent.Session, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	fs := &fakeSession{
		sid:    fmt.Sprintf("its-own-id-%d", len(p.sessions)),
		events: make(chan agent.Event, 8),
	}
	p.sessions = append(p.sessions, fs)
	return fs, nil
}

func (p *perSpawnFakeProvider) Sessions() []*fakeSession {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]*fakeSession(nil), p.sessions...)
}

func (p *perSpawnFakeProvider) closeAll() {
	for _, fs := range p.Sessions() {
		close(fs.events)
	}
}

// rekeyWarn is the distinguishing fragment of Registry.rekey's warning. Kept as a
// constant so a reworded log line fails the test that depends on it rather than
// silently making it assert nothing.
const rekeyWarn = "was not spawned under"

// announceAndWait emits fs's system/init and returns only once fanOut has
// PROCESSED it.
//
// The barrier is the point. Waiting for "the sid became a registry key" does not
// work for the case that matters here — a provider that adopted the pinned id is
// already registered under it, so such a wait returns before fanOut has looked at
// anything and any log assertion after it is vacuous (this cost one round of
// mutation testing to notice). A subscriber envelope, on the other hand, can only
// appear after fanOut has passed the events queued ahead of it.
func announceAndWait(t *testing.T, e *Entry, fs *fakeSession) {
	t.Helper()
	ch := make(chan wire.Envelope, 8)
	e.Subscribe(ch)
	defer e.Unsubscribe(ch)
	fs.announceInit()
	fs.events <- agent.Event{Kind: agent.EventText, Text: "past the init\n"}
	select {
	case <-ch:
	case <-time.After(2 * time.Second):
		t.Fatal("fanOut never processed the init")
	}
}

// TestResolve_FailedResumeUnregistersTheRequestedSid — the obligation that comes
// WITH registering a resume under the sid it is attempting.
//
// The requested sid is now a live key from the moment the attempt starts, so when
// the attempt fails it has to stop being one immediately. Left behind, it reads as
// live to every sid-keyed question until its own fanOut drains, and the next
// reconnect adopts a corpse — the tether#55 failure, re-introduced by the
// tether#54 fix, if resolve did not drop it.
func TestResolve_FailedResumeUnregistersTheRequestedSid(t *testing.T) {
	dead := &lingeringDeadSession{events: make(chan agent.Event, 4)}
	live := &fakeSession{sid: "recovered-sid", events: make(chan agent.Event, 8)}
	p := &lingeringThenLiveProvider{dead: dead, live: live}
	reg := NewRegistry(p)
	defer close(live.events)
	defer close(dead.events)

	att, err := reg.Attach(context.Background(), "gone-sid", "fake", "")
	if err != nil {
		t.Fatalf("Attach: %v", err)
	}
	if got := regKeys(reg); len(got) != 1 || got[0] != "gone-sid" {
		t.Fatalf("registry keys during the resume attempt = %v, want exactly [\"gone-sid\"]", got)
	}

	if _, err := att.Resolve(context.Background()); err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	// resolve must have dropped it ITSELF. The double is why that is assertable:
	// its event stream stays open, so its own fanOut has not returned and cannot be
	// the thing that removed the key (with a double whose stream closes, this
	// assertion passes either way — the other lesson from mutation testing).
	for _, k := range regKeys(reg) {
		if k == "gone-sid" {
			t.Error("the failed resume's sid is still registered; the next reconnect would adopt a dead session")
		}
	}
	if got := regKeys(reg); len(got) != 1 {
		t.Errorf("registry keys after the fallback = %v, want exactly one (the fresh session)", got)
	}
}

// lingeringDeadSession is a failed `cc --resume` whose EVENT STREAM IS STILL OPEN:
// SessionID() is "" (the tether#49 discriminator) and Alive() is false, but fanOut
// is still ranging, so nothing but an explicit evict can unregister it.
//
// attach_test.go's deadSession closes its stream in die(), which is faithful to cc
// and right for the tests that use it — but it means its own fanOut evicts it
// within microseconds, so it cannot distinguish "resolve dropped the dead entry"
// from "the dead entry's teardown got there first".
type lingeringDeadSession struct {
	events chan agent.Event
}

func (d *lingeringDeadSession) SessionID() string                        { return "" }
func (d *lingeringDeadSession) Alive() bool                              { return false }
func (d *lingeringDeadSession) SendPrompt(context.Context, string) error { return errBrokenPipe }
func (d *lingeringDeadSession) Events() <-chan agent.Event               { return d.events }
func (d *lingeringDeadSession) Interrupt() error                         { return nil }
func (d *lingeringDeadSession) Close() error                             { return nil }

type lingeringThenLiveProvider struct {
	dead *lingeringDeadSession
	live *fakeSession

	mu     sync.Mutex
	spawns int
}

func (p *lingeringThenLiveProvider) Name() string { return "fake" }

func (p *lingeringThenLiveProvider) Spawn(_ context.Context, _ agent.SpawnConfig) (agent.Session, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.spawns++
	if p.spawns == 1 {
		return p.dead, nil
	}
	return p.live, nil
}

// TestRegistry_ConcurrentAttachResolveOwnAndEnd_NoRace hammers the paths that
// mutate r.sessions from different goroutines, so `go test -race` has something to
// say about the registration order tether#54 introduces.
//
// The four mutators are named explicitly because an earlier version of this test
// claimed to exercise them and did not — a review instrumented them with panics
// and proved three were never entered (the provider fed no events, so no init ever
// reached rekey; nothing was ever marked dead, so liveEntry never evicted; and
// SessionID() never returned "", so resolve never took its fallback). A fuzz test
// whose interesting paths are unreachable is a green light wired to nothing. This
// version reaches all four:
//
//	spawnEntry's insert   — every goroutine attaches
//	rekey                 — half the sessions announce a DIFFERENT id (opencode's shape)
//	liveEntry's evict     — a third of the sessions are marked dead mid-flight
//	fanOut's defer evict  — closeAll at the end
//
// (Attachment.resolve's evict has its own deterministic test; staging it here would
// need a per-goroutine failing-resume double and would not add coverage.)
//
// It asserts no per-goroutine outcome: the interleavings are genuinely
// nondeterministic, so anything sharper would be a flake rather than a guard. What
// it asserts is that nothing races, nothing panics, and the map fully drains.
func TestRegistry_ConcurrentAttachResolveOwnAndEnd_NoRace(t *testing.T) {
	const goroutines = 24
	p := &pinningProvider{}
	reg := NewRegistry(p)

	var wg sync.WaitGroup
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			sid := ""
			if i%3 == 0 {
				sid = "contended-sid" // several goroutines fight over one sid
			}
			att, err := reg.Attach(context.Background(), sid, "fake", "")
			if err != nil {
				t.Errorf("Attach: %v", err)
				return
			}
			ch := make(chan wire.Envelope, 8)
			att.Subscribe(ch)
			res, err := att.Resolve(context.Background())
			if err != nil {
				return // a lost race on the contended sid is a legitimate outcome here
			}
			e := entryOf(att)

			// Drive rekey concurrently with the lookups below: announce an id this
			// session was NOT spawned under, which is what a self-minting provider
			// does on every session.
			if i%2 == 0 {
				e.Session().(*fakeSession).events <- agent.Event{
					Kind: agent.EventInit, SessionID: "announced-" + res.SID,
				}
			}
			att.SetOwner("client-1")
			reg.IsLive(res.SID)
			reg.IsOwner(res.SID, "client-1")
			reg.OwnedByOther(res.SID, "client-2")
			reg.Subscribe(res.SID, ch)
			_ = reg.DeliverAction(res.SID, "approve", "b-0", "planner")
			reg.BroadcastAll(wire.Envelope{Kind: wire.KindMessage, Payload: "ping"})
			// Drive liveEntry's out-of-band evict: mark the agent dead, then ask a
			// question that consults liveness — the drop happens inside the lookup.
			if i%3 == 1 {
				e.Session().(*fakeSession).dead.Store(true)
				reg.IsLive(res.SID)
				reg.IsLive("announced-" + res.SID)
			}
			reg.Unsubscribe(res.SID, ch)
			att.Unsubscribe(ch)
		}(i)
	}
	wg.Wait()

	// End every session; each fanOut must return and evict, leaving nothing.
	p.closeAll()
	waitForCount(t, reg, 0)
}
