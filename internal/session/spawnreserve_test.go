package session

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/piaobeizu/tether/internal/agent"
)

// gatedProvider holds every Spawn inside the call until the test releases it, so a
// test can stand inside the window tether#60 narrows: the interval between "a caller
// decided to spawn under this key" and "its entry is in r.sessions".
//
// entered is buffered and receives once per Spawn ENTRY, which is the signal that
// matters here — a test asserts on how many callers got INTO Spawn, not on how many
// finished. That distinction is the whole point: without the reservation a second
// caller reaches provider.Spawn, and it is that arrival, not the eventual
// registration, that means a rival agent process was started.
type gatedProvider struct {
	release chan struct{}
	entered chan struct{}
	// mk builds the session handed back for the n-th spawn (1-based), so a test can
	// tell the winner's session from a rival's.
	mk  func(n int) agent.Session
	err error
	// errFrom is the first spawn (1-based) that returns err; the ZERO value means
	// "every spawn", which is what every caller that predates this field expects.
	// It exists for the same reason gateFrom does — a test that needs a real session
	// before it can stage a failure of the NEXT one (tether#82's adopt-only waiter,
	// which inherits a failure it did not cause).
	errFrom int
	// gateFrom is the first spawn (1-based) that waits on release; earlier ones
	// return immediately. It exists so a test can stage a session normally and gate
	// only the RECOVERY spawn — the reopen path cannot be reached without a live
	// session to kill first.
	gateFrom int

	mu     sync.Mutex
	spawns int
	cfgs   []agent.SpawnConfig
}

func newGatedProvider(mk func(n int) agent.Session) *gatedProvider {
	return &gatedProvider{
		release:  make(chan struct{}),
		entered:  make(chan struct{}, 8),
		mk:       mk,
		gateFrom: 1,
	}
}

func (p *gatedProvider) Name() string { return "fake" }

func (p *gatedProvider) Spawn(ctx context.Context, cfg agent.SpawnConfig) (agent.Session, error) {
	p.mu.Lock()
	p.spawns++
	n := p.spawns
	p.cfgs = append(p.cfgs, cfg)
	p.mu.Unlock()

	if n >= p.gateFrom {
		p.entered <- struct{}{}
		select {
		case <-p.release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if p.err != nil && n >= p.errFrom {
		return nil, p.err
	}
	return p.mk(n), nil
}

func (p *gatedProvider) Spawns() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.spawns
}

func (p *gatedProvider) Configs() []agent.SpawnConfig {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]agent.SpawnConfig(nil), p.cfgs...)
}

// waitEntered blocks until one Spawn has been entered, or fails the test.
func (p *gatedProvider) waitEntered(t *testing.T) {
	t.Helper()
	select {
	case <-p.entered:
	case <-time.After(3 * time.Second):
		t.Fatal("no Spawn was entered within 3s; the fixture never opened the window")
	}
}

// assertNoFurtherSpawnEntered is the anti-race assertion, and the reason it is a
// bounded wait rather than a barrier is worth stating: there is no synchronisation
// point inside spawnEntry for a test to join on, so "the second caller is parked on
// the reservation" can only be observed as "it did not arrive at provider.Spawn".
//
// The bound is a DETECTION window, not a hope. Without the reservation the second
// caller reaches Spawn in the time it takes to take an uncontended mutex — six
// orders of magnitude inside this — so the failing direction is reliable. Verified
// by mutation: deleting the reservation makes this fire.
func (p *gatedProvider) assertNoFurtherSpawnEntered(t *testing.T, within time.Duration) {
	t.Helper()
	select {
	case <-p.entered:
		t.Fatalf("a SECOND caller entered provider.Spawn while the first was still inside it: "+
			"that is a rival agent for the same key, and whichever registers last displaces the "+
			"other (spawns so far = %d)", p.Spawns())
	case <-time.After(within):
	}
}

// TestSpawnEntry_ConcurrentCallersForOneKeySpawnOnce is the heart of tether#60.
//
// Two callers that both pass their own pre-check before either registers used to
// both Spawn, and the second `r.sessions[key] = e` displaced the first — leaving an
// agent nothing could reach and two cc appending to one transcript. The reservation
// makes the second caller wait for the first instead.
//
// The assertions are the three things that have to hold together: only one process
// was started, both callers hold the SAME entry (a second entry is the displacement
// itself), and exactly one of them reports having spawned it.
func TestSpawnEntry_ConcurrentCallersForOneKeySpawnOnce(t *testing.T) {
	sessions := []*fakeSession{
		{sid: "shared-sid", events: make(chan agent.Event, 8)},
		{sid: "shared-sid", events: make(chan agent.Event, 8)},
	}
	p := newGatedProvider(func(n int) agent.Session { return sessions[n-1] })
	reg := NewRegistry(p)
	cfg := agent.SpawnConfig{ResumeSessionID: "shared-sid"}

	type result struct {
		e       *Entry
		outcome spawnOutcome
		err     error
	}
	res := make(chan result, 2)
	spawn := func() {
		e, outcome, err := reg.spawnEntry(context.Background(), "fake", cfg, WorkspaceBinding{}, spawnIfAbsent)
		res <- result{e, outcome, err}
	}

	go spawn()
	p.waitEntered(t) // the first caller is INSIDE Spawn: the window is open

	second := make(chan struct{})
	go func() { close(second); spawn() }()
	<-second
	p.assertNoFurtherSpawnEntered(t, 250*time.Millisecond)

	close(p.release)

	var got []result
	for i := 0; i < 2; i++ {
		select {
		case r := <-res:
			got = append(got, r)
		case <-time.After(5 * time.Second):
			t.Fatal("a caller never returned: the waiter is parked on a reservation nobody released")
		}
	}

	for i, r := range got {
		if r.err != nil {
			t.Fatalf("caller %d: %v", i, r.err)
		}
	}
	if n := p.Spawns(); n != 1 {
		t.Errorf("provider.Spawn was called %d times, want 1", n)
	}
	if got[0].e != got[1].e {
		t.Error("the two callers hold DIFFERENT entries; one of them is talking to an agent " +
			"that is no longer registered under this sid")
	}
	// Named outcomes rather than "they differ", because since tether#78 there are two
	// ways NOT to have spawned and this test must keep measuring the reservation. The
	// waiter here has to be adoptedAfterWait: the winner is held inside Spawn, so
	// nothing is registered under the key and the post-claim adoption path cannot be
	// what let it through.
	outcomes := map[spawnOutcome]int{got[0].outcome: 1}
	outcomes[got[1].outcome]++
	if outcomes[spawnStarted] != 1 || outcomes[adoptedAfterWait] != 1 {
		t.Errorf("outcomes = {%v, %v}, want exactly one spawnStarted and one adoptedAfterWait: "+
			"a second spawnStarted is the displacement, and an adoptedRegistration here would mean "+
			"this test is measuring tether#78's path instead of the reservation",
			got[0].outcome, got[1].outcome)
	}

	// The registration must be the entry both callers hold, not a displaced one.
	reg.mu.RLock()
	registered := reg.sessions["shared-sid"]
	inFlight := len(reg.spawning)
	reg.mu.RUnlock()
	if registered != got[0].e {
		t.Error("r.sessions holds an entry neither caller was given")
	}
	if inFlight != 0 {
		t.Errorf("%d reservation(s) left behind; the key is now unspawnable", inFlight)
	}
}

// TestSpawnEntry_ReservationsAreIndependentPerKey pins WHICH string the claim is
// keyed by, which every other test in this file is structurally unable to check:
// they all contend on one key, so a reservation that over-shares looks identical to
// one that works.
//
// The mutants this exists for are one-token slips, and `key`, `cfg.SessionID` and
// `cfg.ResumeSessionID` are all in scope at the claim:
//
//   - keying on cfg.ResumeSessionID makes every FRESH spawn share the "" key, so two
//     unrelated clients connecting at the same moment are handed the SAME agent
//     session — someone else's conversation.
//   - keying on cfg.SessionID makes every RESUME share "", so a reconnect for one sid
//     is handed another sid's session and its own is never registered.
//
// Neither is caught by the workdir assertion: two callers in one workspace resolve
// the same directory.
//
// The assertion is that BOTH spawns get inside provider.Spawn at once. That is the
// direct consequence of not sharing a reservation, and it is a barrier rather than a
// timer — an over-sharing build makes the second caller wait, so the second entry
// never arrives and this fails by timing out rather than by guessing.
func TestSpawnEntry_ReservationsAreIndependentPerKey(t *testing.T) {
	run := func(t *testing.T, cfgs [2]agent.SpawnConfig) {
		t.Helper()
		made := []*fakeSession{
			{sid: "a-sid", events: make(chan agent.Event, 8)},
			{sid: "b-sid", events: make(chan agent.Event, 8)},
		}
		p := newGatedProvider(func(n int) agent.Session { return made[n-1] })
		reg := NewRegistry(p)

		type result struct {
			e   *Entry
			err error
		}
		res := make(chan result, 2)
		for _, cfg := range cfgs {
			go func(cfg agent.SpawnConfig) {
				e, _, err := reg.spawnEntry(context.Background(), "fake", cfg, WorkspaceBinding{}, spawnIfAbsent)
				res <- result{e, err}
			}(cfg)
		}

		// Both must reach Spawn. Under an over-sharing key the second one is parked
		// on the first's reservation and this times out.
		p.waitEntered(t)
		p.waitEntered(t)
		close(p.release)

		seen := map[*Entry]bool{}
		for i := 0; i < 2; i++ {
			select {
			case r := <-res:
				if r.err != nil {
					t.Fatalf("caller %d: %v", i, r.err)
				}
				seen[r.e] = true
			case <-time.After(5 * time.Second):
				t.Fatal("a caller never returned")
			}
		}
		if len(seen) != 2 {
			t.Error("two callers with DIFFERENT keys were handed the same entry: they are sharing " +
				"one agent, i.e. one client is reading another's conversation")
		}
		if n := p.Spawns(); n != 2 {
			t.Errorf("provider.Spawn was called %d times, want 2: independent keys need independent agents", n)
		}
	}

	// Fresh spawns: distinct because spawnEntry mints an id for each. A claim keyed
	// on cfg.ResumeSessionID collapses them onto "".
	t.Run("two fresh spawns", func(t *testing.T) {
		run(t, [2]agent.SpawnConfig{{}, {}})
	})

	// Resumes of two different sids. A claim keyed on cfg.SessionID collapses them
	// onto "" the same way.
	t.Run("two resumes of different sids", func(t *testing.T) {
		run(t, [2]agent.SpawnConfig{{ResumeSessionID: "a-sid"}, {ResumeSessionID: "b-sid"}})
	})
}

// TestSpawnEntry_TheWaiterGetsTheSpawnFailure — a failed spawn must be reported to
// the waiter too, and must NOT leave it retrying.
//
// Retrying would spawn a second `cc --resume <sid>` for the transcript this exists to
// protect, and whatever stopped the winner (a missing binary, a resource limit) is
// about to stop the waiter as well. So both callers fail, and the process count stays
// at one attempt.
func TestSpawnEntry_TheWaiterGetsTheSpawnFailure(t *testing.T) {
	boom := errors.New(`exec: "cc": executable file not found in $PATH`)
	p := newGatedProvider(nil)
	p.err = boom
	reg := NewRegistry(p)
	cfg := agent.SpawnConfig{ResumeSessionID: "doomed-sid"}

	errs := make(chan error, 2)
	spawn := func() {
		_, _, err := reg.spawnEntry(context.Background(), "fake", cfg, WorkspaceBinding{}, spawnIfAbsent)
		errs <- err
	}

	go spawn()
	p.waitEntered(t)
	go spawn()
	p.assertNoFurtherSpawnEntered(t, 250*time.Millisecond)
	close(p.release)

	for i := 0; i < 2; i++ {
		select {
		case err := <-errs:
			if err == nil {
				t.Fatalf("caller %d succeeded against a provider that always fails", i)
			}
			if !errors.Is(err, boom) {
				t.Errorf("caller %d error = %v, want it to carry the spawn failure", i, err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("a caller never returned after a failed spawn: the reservation was not released")
		}
	}
	if n := p.Spawns(); n != 1 {
		t.Errorf("provider.Spawn was called %d times, want 1: the waiter must not retry", n)
	}

	// A failed spawn must leave the key spawnable again — otherwise one bad exec
	// wedges that sid for the lifetime of the daemon.
	reg.mu.RLock()
	inFlight := len(reg.spawning)
	reg.mu.RUnlock()
	if inFlight != 0 {
		t.Errorf("%d reservation(s) left behind after a failed spawn; the sid is now permanently stuck", inFlight)
	}
}

// TestSpawnEntry_AWaiterThatGivesUpDoesNotStrandTheKey — the waiter's ctx is its
// client connection, and a browser that goes away must not park on someone else's
// exec. Leaving early must cost nothing: the reservation belongs to the SPAWNER, so
// the winner still lands and the key is still usable afterwards.
func TestSpawnEntry_AWaiterThatGivesUpDoesNotStrandTheKey(t *testing.T) {
	live := &fakeSession{sid: "shared-sid", events: make(chan agent.Event, 8)}
	p := newGatedProvider(func(int) agent.Session { return live })
	reg := NewRegistry(p)
	cfg := agent.SpawnConfig{ResumeSessionID: "shared-sid"}

	winner := make(chan *Entry, 1)
	go func() {
		e, _, err := reg.spawnEntry(context.Background(), "fake", cfg, WorkspaceBinding{}, spawnIfAbsent)
		if err != nil {
			t.Errorf("winner: %v", err)
		}
		winner <- e
	}()
	p.waitEntered(t)

	ctx, cancel := context.WithCancel(context.Background())
	gaveUp := make(chan error, 1)
	go func() {
		_, _, err := reg.spawnEntry(ctx, "fake", cfg, WorkspaceBinding{}, spawnIfAbsent)
		gaveUp <- err
	}()
	p.assertNoFurtherSpawnEntered(t, 250*time.Millisecond)

	cancel()
	select {
	case err := <-gaveUp:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("the waiter returned %v, want context.Canceled", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the waiter stayed parked after its own context was cancelled")
	}

	// The winner is untouched by the waiter leaving.
	close(p.release)
	select {
	case e := <-winner:
		if e == nil {
			t.Fatal("the winner produced no entry")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the winner never finished")
	}
	reg.mu.RLock()
	inFlight := len(reg.spawning)
	reg.mu.RUnlock()
	if inFlight != 0 {
		t.Errorf("%d reservation(s) left behind", inFlight)
	}
}

// TestAwaitSpawn_RefusesToAdoptASessionFromAnotherDirectory pins the assertion in
// awaitSpawn rather than a reachable behaviour, and says so.
//
// It should be unreachable through the two call sites that can contend — both derive
// their workspace from the session's own binding, so two callers holding one sid
// resolved one directory, and a connection that rebinds gets a freshly minted key and
// never contends at all. It is checked because adopting an entry spawned elsewhere
// would silently relocate the conversation, which is the exact failure
// resolveWorkspace exists to prevent, and "unreachable" is a property of today's
// callers rather than of this function.
func TestAwaitSpawn_RefusesToAdoptASessionFromAnotherDirectory(t *testing.T) {
	reg := NewRegistry()
	res := &spawnReservation{
		done:  make(chan struct{}),
		entry: &Entry{workdir: "/somewhere/else"},
	}
	close(res.done)

	e, outcome, err := reg.awaitSpawn(context.Background(), "sid", res, "/the/asked/for/one")
	if err == nil {
		t.Fatalf("adopted an entry from %q while this connection resolved %q", "/somewhere/else", "/the/asked/for/one")
	}
	// spawnNoEntry, not merely "not adoptedAfterWait": the zero value is what a caller
	// that ignores the error reads, and it must not let one derive that a session was
	// started here (see spawnOutcome).
	if e != nil || outcome != spawnNoEntry {
		t.Errorf("returned e=%v outcome=%v alongside an error, want a nil entry and spawnNoEntry", e, outcome)
	}
}

// ── the wiring hop: the two call sites that can actually contend ───────────────
//
// The reservation being correct proves nothing about Attach and reopen using it.
// Both tests below drive the REAL entry point rather than spawnEntry, because a
// version of this change that fixed the primitive and left either call site
// unchanged would pass every test above (team lesson: sample by call site, and
// include "the consumer does not reach this on the path I care about").

// TestAttach_ConcurrentAttachesForOneSidSpawnOnce is the first of the two windows
// tether#60 names: two reconnects carrying the same sid, both past Attach's
// liveEntry gate before either registered.
//
// The second assertion is the one that is easy to leave out. A waiter must not be
// fallback-eligible: if the resume it adopted fails, TWO attachments each spawning a
// fresh fallback forks the conversation in two, which is what cc's --fork-session is
// for and not something to arrive at by losing a race.
func TestAttach_ConcurrentAttachesForOneSidSpawnOnce(t *testing.T) {
	sessions := []*fakeSession{
		{sid: "reconnect-sid", events: make(chan agent.Event, 8)},
		{sid: "reconnect-sid", events: make(chan agent.Event, 8)},
	}
	p := newGatedProvider(func(n int) agent.Session { return sessions[n-1] })
	reg := NewRegistry(p)

	type result struct {
		a   *Attachment
		err error
	}
	res := make(chan result, 2)
	attach := func() {
		a, err := reg.Attach(context.Background(), "reconnect-sid", "fake", "")
		res <- result{a, err}
	}

	go attach()
	p.waitEntered(t)
	second := make(chan struct{})
	go func() { close(second); attach() }()
	<-second
	p.assertNoFurtherSpawnEntered(t, 250*time.Millisecond)
	close(p.release)

	var got []result
	for i := 0; i < 2; i++ {
		select {
		case r := <-res:
			got = append(got, r)
		case <-time.After(5 * time.Second):
			t.Fatal("an Attach never returned")
		}
	}
	for i, r := range got {
		if r.err != nil {
			t.Fatalf("Attach %d: %v", i, r.err)
		}
	}
	if n := p.Spawns(); n != 1 {
		t.Errorf("provider.Spawn was called %d times, want 1: two reconnects for one sid must not "+
			"start two `cc --resume` of the same transcript", n)
	}
	if attEntry(got[0].a) != attEntry(got[1].a) {
		t.Error("the two attachments hold different entries; the later registration displaced the earlier")
	}

	var resuming int
	var waiterReopenSID string
	var sawWaiter bool
	for _, r := range got {
		r.a.mu.Lock()
		if r.a.resuming {
			resuming++
		} else {
			waiterReopenSID, sawWaiter = r.a.reopenSID, true
		}
		r.a.mu.Unlock()
	}
	if resuming != 1 {
		t.Errorf("%d of 2 attachments are fallback-eligible, want exactly 1 (the one that spawned): "+
			"two fallbacks would fork the conversation into two fresh sessions", resuming)
	}

	// The waiter must NOT be armed for recovery yet, and this is the ONLY guard on the
	// single behavioural difference between the two ways of not having spawned — i.e.
	// on the whole reason tether#78 replaced a bool with named outcomes. Without it, a
	// change that armed BOTH adoptions passes the entire package.
	//
	// The rule it protects is tether#76's: nothing is armed before Resolve confirms. A
	// waiter holds a resume the winner has only just STARTED, so nothing has confirmed
	// it. An adopted REGISTRATION is armed early instead, and the difference is not
	// "confirmed vs unconfirmed" — either can be unconfirmed — but that the
	// registration has been through Alive(), which is the reuse gate's own evidence.
	if !sawWaiter {
		t.Fatal("neither attachment was the waiter, so the count above is measuring something else")
	}
	if waiterReopenSID != "" {
		t.Errorf("the waiter's reopenSID = %q, want empty: it adopted a resume nobody has "+
			"confirmed, and arming it here breaks the rule that recovery is armed only once "+
			"Resolve confirms the session", waiterReopenSID)
	}
}

// TestSendPrompt_ConcurrentReopensAcrossAttachmentsSpawnOnce is the second window,
// and the one tether#76 widened from "needs three tabs" to "needs two": two
// attachments sharing one session, both recovering from its death at once.
//
// reopenMu is per-ATTACHMENT, so it serialises nothing here — before tether#60 both
// passed reopen's sibling-adoption check (liveEntry evicts the corpse and answers
// "no") and both spawned.
func TestSendPrompt_ConcurrentReopensAcrossAttachmentsSpawnOnce(t *testing.T) {
	seed := &fakeSession{sid: "shared-sid", events: make(chan agent.Event, 8)}
	replacements := []*fakeSession{
		{sid: "shared-sid", events: make(chan agent.Event, 8)},
		{sid: "shared-sid", events: make(chan agent.Event, 8)},
	}
	p := newGatedProvider(func(n int) agent.Session {
		if n == 1 {
			return seed
		}
		return replacements[n-2]
	})
	p.gateFrom = 2 // let the seed spawn through; gate only the recovery
	reg := NewRegistry(p)

	if _, err := reg.GetOrSpawnEntry(context.Background(), "", "fake"); err != nil {
		t.Fatalf("seed spawn: %v", err)
	}
	seed.announceInit()
	waitForRegistered(t, reg, "shared-sid")

	tab1, err := reg.Attach(context.Background(), "shared-sid", "fake", "")
	if err != nil {
		t.Fatalf("tab1 Attach: %v", err)
	}
	tab2, err := reg.Attach(context.Background(), "shared-sid", "fake", "")
	if err != nil {
		t.Fatalf("tab2 Attach: %v", err)
	}
	if n := p.Spawns(); n != 1 {
		t.Fatalf("spawns after both attaches = %d, want 1: both tabs must REUSE the seed", n)
	}

	seed.kill()

	errs := make(chan error, 2)
	started := make(chan struct{}, 2)
	for _, a := range []*Attachment{tab1, tab2} {
		go func(a *Attachment) {
			started <- struct{}{}
			errs <- a.SendPrompt(context.Background(), "still there?")
		}(a)
	}
	for i := 0; i < 2; i++ {
		select {
		case <-started:
		case <-time.After(3 * time.Second):
			t.Fatal("a prompt goroutine never started")
		}
	}
	p.waitEntered(t)
	p.assertNoFurtherSpawnEntered(t, 250*time.Millisecond)

	// Neither prompt may have completed yet, and this is the assertion that makes the
	// three below mean what they say. While the winner is held inside provider.Spawn
	// nothing is registered under this sid, so liveEntry answers "no" and the second
	// tab CANNOT take tether#76's sibling-adoption branch — it can only be parked on
	// the tether#60 reservation. A completed prompt here would mean this test had
	// measured the sibling branch instead, under which all three post-conditions also
	// hold and the reservation could be deleted with the test still green.
	if n := len(errs); n != 0 {
		t.Fatalf("%d prompt(s) completed while the winner was still inside Spawn: this test is "+
			"measuring the sibling-adoption branch, not the reservation", n)
	}
	close(p.release)

	for i := 0; i < 2; i++ {
		select {
		case err := <-errs:
			if err != nil {
				t.Fatalf("prompt %d: %v", i, err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("a prompt never completed")
		}
	}
	if n := p.Spawns(); n != 2 {
		t.Errorf("provider.Spawn was called %d times, want 2 (the seed, then ONE replacement): "+
			"a second `cc --resume shared-sid` is two agents appending to one transcript", n)
	}
	if attEntry(tab1) != attEntry(tab2) {
		t.Error("the two tabs hold different entries after the recovery; every sid-keyed route " +
			"(DeliverAction, InterruptSession, /wt/events) can only point at one of them")
	}

	// Only the tab that actually spawned spends its one-per-attachment budget. The
	// other adopted, which starts no process, so charging it would penalise it for
	// losing a race it never entered.
	var spent int
	for _, a := range []*Attachment{tab1, tab2} {
		a.mu.Lock()
		if a.reopenSpent {
			spent++
		}
		a.mu.Unlock()
	}
	if spent != 1 {
		t.Errorf("%d of 2 attachments spent their reopen budget, want exactly 1 (the spawner)", spent)
	}
}

// ── tether#78: the residual, and it needs no concurrency ──────────────────────
//
// Every test above holds the winner INSIDE provider.Spawn, because that is where the
// reservation does its work. The residual is the OPPOSITE staging: the winner has
// already finished and released, so a late caller finds no reservation at all, spawns,
// and its `r.sessions[key] = e` displaces the winner. Two sequential calls demonstrate
// it — which is worth knowing, because the wi described it as a race needing a
// scheduling delay, and at the primitive it is not one.

// staged returns a provider that gates nothing, for the tests below that stage by
// ORDER rather than by parking a caller inside Spawn.
func staged(t *testing.T, sessions ...*fakeSession) *gatedProvider {
	t.Helper()
	p := newGatedProvider(func(n int) agent.Session {
		if n > len(sessions) {
			// Reached only in the FAILING direction, where an extra rival is spawned.
			// A nil session would panic inside the registry and bury the real verdict.
			t.Errorf("provider.Spawn called %d times, but only %d sessions were staged: "+
				"an unexpected extra agent was started", n, len(sessions))
			return &fakeSession{sid: "unexpected", events: make(chan agent.Event, 8)}
		}
		return sessions[n-1]
	})
	p.gateFrom = len(sessions) + 99 // nothing waits on release
	return p
}

// TestSpawnEntry_ALateCallerAdoptsTheRegistrationInsteadOfDisplacingIt is tether#78 at
// the primitive.
//
// What makes the post-claim consult sound is an ORDERING this test also pins: the
// winner registers before its deferred release runs, so "no reservation for this key"
// implies "r.sessions[key] already reflects the winner". The staging assertion in the
// middle is that ordering, checked rather than assumed — if the release ever moved
// ahead of the registration, this test would report which of the two broke instead of
// only that the outcome changed.
func TestSpawnEntry_ALateCallerAdoptsTheRegistrationInsteadOfDisplacingIt(t *testing.T) {
	winnerSess := &fakeSession{sid: "shared-sid", events: make(chan agent.Event, 8)}
	rivalSess := &fakeSession{sid: "shared-sid", events: make(chan agent.Event, 8)}
	p := staged(t, winnerSess, rivalSess)
	reg := NewRegistry(p)
	cfg := agent.SpawnConfig{ResumeSessionID: "shared-sid"}

	first, outcome, err := reg.spawnEntry(context.Background(), "fake", cfg, WorkspaceBinding{}, spawnIfAbsent)
	if err != nil {
		t.Fatalf("winner: %v", err)
	}
	if outcome != spawnStarted {
		t.Fatalf("winner outcome = %v, want spawnStarted", outcome)
	}

	reg.mu.RLock()
	inFlight := len(reg.spawning)
	registered := reg.sessions["shared-sid"]
	reg.mu.RUnlock()
	if inFlight != 0 {
		t.Fatalf("%d reservation(s) still in flight: this test is measuring the tether#60 "+
			"wait, not the residual", inFlight)
	}
	if registered != first {
		t.Fatal("the reservation was released before the entry was registered: the whole basis " +
			"for consulting r.sessions at claim time is that ordering")
	}

	second, outcome, err := reg.spawnEntry(context.Background(), "fake", cfg, WorkspaceBinding{}, spawnIfAbsent)
	if err != nil {
		t.Fatalf("late caller: %v", err)
	}
	if outcome != adoptedRegistration {
		t.Errorf("late caller outcome = %v, want adoptedRegistration", outcome)
	}
	if second != first {
		t.Error("the late caller was handed a DIFFERENT entry: it started a rival " +
			"`cc --resume shared-sid`, so two agents are appending to one transcript and " +
			"whichever registered last is the only one reachable by sid")
	}
	if n := p.Spawns(); n != 1 {
		t.Errorf("provider.Spawn was called %d times, want 1", n)
	}
	reg.mu.RLock()
	registered = reg.sessions["shared-sid"]
	reg.mu.RUnlock()
	if registered != first {
		t.Error("r.sessions no longer holds the winner's entry — the registration was displaced")
	}
}

// TestSpawnEntry_ALateCallerReplacesARegisteredCorpseRatherThanAdoptingIt is the
// NEGATIVE control for the test above, and the reason the new consult cannot be a bare
// map test.
//
// An Entry outlives its agent (tether#55): between "cc exited" and "fanOut's deferred
// evict ran" the key is still registered and every signal a reconnect used to consult
// says healthy. Adopting that hands the client a corpse — prompts die in a broken pipe
// that only reaches a slog.Warn, i.e. "thinking…" forever, which is the failure this
// daemon is worst at reporting. So the fix has to ask liveEntry, not the map, and a
// dead registration must still be REPLACED.
func TestSpawnEntry_ALateCallerReplacesARegisteredCorpseRatherThanAdoptingIt(t *testing.T) {
	corpse := &fakeSession{sid: "shared-sid", events: make(chan agent.Event, 8)}
	replacement := &fakeSession{sid: "shared-sid", events: make(chan agent.Event, 8)}
	p := staged(t, corpse, replacement)
	reg := NewRegistry(p)
	cfg := agent.SpawnConfig{ResumeSessionID: "shared-sid"}

	first, _, err := reg.spawnEntry(context.Background(), "fake", cfg, WorkspaceBinding{}, spawnIfAbsent)
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	// Dead but still REGISTERED: kill() deliberately leaves the event stream open, so
	// fanOut has not evicted it. That is the state this test is about.
	corpse.kill()
	reg.mu.RLock()
	stillThere := reg.sessions["shared-sid"] == first
	reg.mu.RUnlock()
	if !stillThere {
		t.Fatal("the corpse was already un-registered: there is nothing here to be tempted to adopt")
	}

	second, outcome, err := reg.spawnEntry(context.Background(), "fake", cfg, WorkspaceBinding{}, spawnIfAbsent)
	if err != nil {
		t.Fatalf("replacement: %v", err)
	}
	if outcome != spawnStarted {
		t.Errorf("outcome = %v, want spawnStarted: a registered-but-dead session must be "+
			"replaced, and reporting an adoption would also tell the caller not to spend its "+
			"reopen budget on a recovery it did perform", outcome)
	}
	if second == first {
		t.Fatal("adopted the corpse: this connection now holds a session whose agent has exited, " +
			"and every prompt it sends will die in a broken pipe with the spinner still turning")
	}
	if n := p.Spawns(); n != 2 {
		t.Errorf("provider.Spawn was called %d times, want 2 (the seed, then its replacement)", n)
	}
	reg.mu.RLock()
	registered := reg.sessions["shared-sid"]
	reg.mu.RUnlock()
	if registered != second {
		t.Error("r.sessions does not hold the replacement")
	}
}

// TestSpawnEntry_ALateCallerWillNotAdoptARegistrationFromAnotherDirectory pins the
// assertion on the new adoption path, matching the one awaitSpawn already makes.
//
// Believed unreachable through both collidable call sites (each derives its workspace
// from the session's own binding, and a connection that rebinds gets a freshly minted
// key and cannot contend at all), and checked anyway: adopting an entry from another
// directory silently relocates a conversation, which is the failure resolveWorkspace
// exists to prevent. Failing closed costs a reconnect.
func TestSpawnEntry_ALateCallerWillNotAdoptARegistrationFromAnotherDirectory(t *testing.T) {
	live := &fakeSession{sid: "shared-sid", events: make(chan agent.Event, 8)}
	p := staged(t, live)
	reg := NewRegistry(p)
	cfg := agent.SpawnConfig{ResumeSessionID: "shared-sid"}

	if _, _, err := reg.spawnEntry(context.Background(), "fake", cfg, WorkspaceBinding{Path: "/ws/one"}, spawnIfAbsent); err != nil {
		t.Fatalf("seed: %v", err)
	}

	e, outcome, err := reg.spawnEntry(context.Background(), "fake", cfg, WorkspaceBinding{Path: "/ws/two"}, spawnIfAbsent)
	if err == nil {
		t.Fatal("adopted a session registered in /ws/one for a connection that resolved /ws/two")
	}
	if e != nil || outcome != spawnNoEntry {
		t.Errorf("returned e=%v outcome=%v alongside an error, want a nil entry and spawnNoEntry", e, outcome)
	}
	if n := p.Spawns(); n != 1 {
		t.Errorf("provider.Spawn was called %d times, want 1: refusing must not also start an agent", n)
	}
}

// ── the wiring hop for tether#78 ───────────────────────────────────────────────
//
// The primitive being right proves nothing about the two call sites reaching it on
// the path that matters. Both tests below drive the REAL entry point, and both stage
// the residual by parking a caller inside its own liveness check — the one place on
// that path where the registry calls out (see fakeSession.aliveHold). Parking there
// rather than sleeping is what makes the staging airtight: liveEntry looks the sid up
// BEFORE it asks Alive(), so the parked caller is provably holding the entry it found
// and cannot, on release, quietly take a reuse/sibling branch on whatever registered
// while it was stopped. Without that, "no duplicate agent" would also be satisfied by
// the caller never reaching spawnEntry at all — a false green.

// TestAttach_AnAttachHeldInItsOwnGateAdoptsTheRegistration is the first call site:
// two reconnects carrying one sid, where the second finishes its gate so late that the
// first has already registered AND released.
//
// It also pins the arming decision. An adopted registration is a session liveEntry
// found ALIVE — the reuse branch's own evidence — so this attachment gets the reuse
// branch's early arming rather than a tether#60 waiter's "wait for Resolve". That
// matters because serveChat runs its prompt reader in parallel with Resolve, so a
// prompt can arrive at a session that is already answering before Resolve confirms it.
func TestAttach_AnAttachHeldInItsOwnGateAdoptsTheRegistration(t *testing.T) {
	corpse := &fakeSession{sid: "shared-sid", events: make(chan agent.Event, 8)}
	replacement := &fakeSession{sid: "shared-sid", events: make(chan agent.Event, 8)}
	rival := &fakeSession{sid: "shared-sid", events: make(chan agent.Event, 8)}
	p := staged(t, corpse, replacement, rival)
	reg := NewRegistry(p)

	if _, err := reg.GetOrSpawnEntry(context.Background(), "", "fake"); err != nil {
		t.Fatalf("seed spawn: %v", err)
	}
	corpse.announceInit()
	waitForRegistered(t, reg, "shared-sid")
	// Registered, and its agent is gone: both attaches below must fall through their
	// gate to a `--resume`. Armed AFTER waitForRegistered, which polls through IsLive
	// and would otherwise consume the hold.
	corpse.kill()
	corpse.aliveEntered = make(chan struct{}, 1)
	corpse.aliveHold = make(chan struct{})

	type result struct {
		a   *Attachment
		err error
	}
	late := make(chan result, 1)
	go func() {
		a, err := reg.Attach(context.Background(), "shared-sid", "fake", "")
		late <- result{a, err}
	}()

	select {
	case <-corpse.aliveEntered:
	case <-time.After(3 * time.Second):
		t.Fatal("the late attach never reached its liveness check; the fixture never opened the window")
	}

	// The winner now runs to completion — spawn, register, release — while the late
	// caller is still inside its gate holding the corpse.
	winner, err := reg.Attach(context.Background(), "shared-sid", "fake", "")
	if err != nil {
		t.Fatalf("winner Attach: %v", err)
	}
	reg.mu.RLock()
	inFlight := len(reg.spawning)
	reg.mu.RUnlock()
	if inFlight != 0 {
		t.Fatalf("%d reservation(s) still held: the late caller would WAIT on it, which is "+
			"tether#60's path and not the residual this test is for", inFlight)
	}

	close(corpse.aliveHold)
	var got result
	select {
	case got = <-late:
	case <-time.After(5 * time.Second):
		t.Fatal("the late attach never returned")
	}
	if got.err != nil {
		t.Fatalf("late Attach: %v", got.err)
	}

	if n := p.Spawns(); n != 2 {
		t.Errorf("provider.Spawn was called %d times, want 2 (the seed, then ONE replacement): "+
			"a third is a rival `cc --resume shared-sid` for a transcript that already has one", n)
	}
	if attEntry(got.a) != attEntry(winner) {
		t.Error("the two attachments hold different entries: the late one displaced the winner's " +
			"registration, so the winner's agent is now unreachable by its own sid")
	}
	reg.mu.RLock()
	registered := reg.sessions["shared-sid"]
	reg.mu.RUnlock()
	if registered != attEntry(winner) {
		t.Error("r.sessions does not hold the winner's entry")
	}

	winner.mu.Lock()
	winnerResuming := winner.resuming
	winner.mu.Unlock()
	got.a.mu.Lock()
	lateResuming, lateReopenSID := got.a.resuming, got.a.reopenSID
	got.a.mu.Unlock()

	if !winnerResuming {
		t.Error("the attachment that actually spawned the resume is not fallback-eligible; " +
			"nothing can recover it if the resume never confirms")
	}
	if lateResuming {
		t.Error("the adopting attachment is fallback-eligible: if the resume it adopted fails, " +
			"BOTH attachments spawn a fresh session and the conversation forks in two")
	}
	if lateReopenSID != "shared-sid" {
		t.Errorf("the adopting attachment's reopenSID = %q, want %q: it holds a session that is "+
			"already answering, so a prompt refused before Resolve confirms must still be "+
			"recoverable — exactly as in the reuse branch this adoption stands in for",
			lateReopenSID, "shared-sid")
	}
}

// TestSendPrompt_AReopenHeldInItsSiblingCheckAdoptsTheRegistration is the second call
// site, and the route the tether#60 reviewer reproduced by hand (spawns=3, with tab1's
// entry no longer the registration).
//
// Two attachments share one session; it dies; both recover. The second is held inside
// the sibling-adoption check until the first has spawned, registered and released, so
// it finds no sibling AND no reservation — the state in which it used to start a rival.
func TestSendPrompt_AReopenHeldInItsSiblingCheckAdoptsTheRegistration(t *testing.T) {
	seed := &fakeSession{sid: "shared-sid", events: make(chan agent.Event, 8)}
	replacement := &fakeSession{sid: "shared-sid", events: make(chan agent.Event, 8)}
	rival := &fakeSession{sid: "shared-sid", events: make(chan agent.Event, 8)}
	p := staged(t, seed, replacement, rival)
	reg := NewRegistry(p)

	if _, err := reg.GetOrSpawnEntry(context.Background(), "", "fake"); err != nil {
		t.Fatalf("seed spawn: %v", err)
	}
	seed.announceInit()
	waitForRegistered(t, reg, "shared-sid")

	tab1, err := reg.Attach(context.Background(), "shared-sid", "fake", "")
	if err != nil {
		t.Fatalf("tab1 Attach: %v", err)
	}
	tab2, err := reg.Attach(context.Background(), "shared-sid", "fake", "")
	if err != nil {
		t.Fatalf("tab2 Attach: %v", err)
	}
	if n := p.Spawns(); n != 1 {
		t.Fatalf("spawns after both attaches = %d, want 1: both tabs must REUSE the seed", n)
	}

	seed.kill()
	seed.aliveEntered = make(chan struct{}, 1)
	seed.aliveHold = make(chan struct{})

	lateErr := make(chan error, 1)
	go func() { lateErr <- tab2.SendPrompt(context.Background(), "from tab2") }()
	select {
	case <-seed.aliveEntered:
	case <-time.After(3 * time.Second):
		t.Fatal("tab2 never reached reopen's sibling check")
	}

	if err := tab1.SendPrompt(context.Background(), "from tab1"); err != nil {
		t.Fatalf("tab1 prompt: %v", err)
	}
	reg.mu.RLock()
	inFlight := len(reg.spawning)
	reg.mu.RUnlock()
	if inFlight != 0 {
		t.Fatalf("%d reservation(s) still held: tab2 would WAIT, which is tether#60's path", inFlight)
	}

	close(seed.aliveHold)
	select {
	case err := <-lateErr:
		if err != nil {
			t.Fatalf("tab2 prompt: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("tab2's prompt never completed")
	}

	if n := p.Spawns(); n != 2 {
		t.Errorf("provider.Spawn was called %d times, want 2 (the seed, then ONE replacement): "+
			"a second `cc --resume shared-sid` is two agents appending to one transcript", n)
	}
	if attEntry(tab1) != attEntry(tab2) {
		t.Error("the two tabs hold different entries after the recovery; every sid-keyed route " +
			"(DeliverAction, InterruptSession, /wt/events) can only point at one of them")
	}
	if got := len(replacement.Prompts()); got != 2 {
		t.Errorf("the replacement received %d prompts, want 2: a prompt that landed on a rival "+
			"agent is answered into a transcript nobody is reading", got)
	}

	var spent int
	for _, a := range []*Attachment{tab1, tab2} {
		a.mu.Lock()
		if a.reopenSpent {
			spent++
		}
		a.mu.Unlock()
	}
	if spent != 1 {
		t.Errorf("%d of 2 attachments spent their reopen budget, want exactly 1 (the spawner): "+
			"charging the adopter would refuse it a later recovery over a race it never entered", spent)
	}
}

// ── tether#82: the SPENT path reaches the claim too ────────────────────────────
//
// The two tests above are the tether#78 wiring hop for an attachment with budget left.
// The residual they leave is the one below: reopen's spent budget used to return BEFORE
// spawnEntry, so an attachment that had used its one re-open never reached any of the
// machinery those tests exercise. Its only chance to notice a live session under the sid
// was its own sibling check — and that check answers about the entry it FOUND, because
// liveEntry reads the map and only THEN asks Alive(). Lose it and the prompt is refused
// with a live session registered under that very sid.
//
// Both tests here therefore stage the SAME two schedulings as the pair above (a caller
// held inside its own liveness check; a caller arriving while a replacement is still
// inside provider.Spawn) with one difference: the caller is out of budget. That is the
// whole point — the difference between the two pairs is a mode, not a mechanism.

// stageSpentReopenRace brings the registry to the four-step state the tether#82
// reproduction needs and hands back the attachments standing in it:
//
//   - tab2 has SPENT its one re-open (it recovered the first death) and is now PARKED
//     inside reopen's sibling check, holding the corpse it found there.
//   - tab1 still had budget, and while tab2 was parked it completed a full re-open:
//     its replacement is spawned, registered, and its reservation released.
//
// The caller releases tab2 by closing first.aliveHold, and reads its verdict from the
// returned channel. Parking rather than sleeping is what makes this airtight, for the
// reason fakeSession.aliveHold documents: liveEntry looks the sid up BEFORE it asks
// Alive(), so the parked goroutine is provably holding `first` and cannot, on release,
// quietly take the sibling branch on whatever registered while it was stopped.
func stageSpentReopenRace(t *testing.T, p *gatedProvider, seed, first *fakeSession) (*Registry, *Attachment, *Attachment, chan error) {
	t.Helper()
	reg := NewRegistry(p)
	if _, err := reg.GetOrSpawnEntry(context.Background(), "", "fake"); err != nil {
		t.Fatalf("seed spawn: %v", err)
	}
	seed.announceInit()
	waitForRegistered(t, reg, "shared-sid")

	tab1, err := reg.Attach(context.Background(), "shared-sid", "fake", "")
	if err != nil {
		t.Fatalf("tab1 Attach: %v", err)
	}
	tab2, err := reg.Attach(context.Background(), "shared-sid", "fake", "")
	if err != nil {
		t.Fatalf("tab2 Attach: %v", err)
	}
	if n := p.Spawns(); n != 1 {
		t.Fatalf("spawns after both attaches = %d, want 1: both tabs must REUSE the seed", n)
	}

	// First death. tab2 recovers it and spends its budget doing so; tab1 is untouched
	// and still points at the seed, so its own budget is intact for the second death.
	seed.kill()
	if err := tab2.SendPrompt(context.Background(), "tab2 spends its budget"); err != nil {
		t.Fatalf("tab2's first prompt: %v", err)
	}
	tab2.mu.Lock()
	spent := tab2.reopenSpent
	tab2.mu.Unlock()
	if !spent {
		t.Fatal("tab2's budget is not spent: this staging proves nothing about the spent path")
	}
	if n := p.Spawns(); n != 2 {
		t.Fatalf("spawns after tab2's recovery = %d, want 2 (the seed, then tab2's re-open)", n)
	}

	// Second death, with the scheduling that makes it the residual: tab2 is held inside
	// its own sibling check for as long as it takes tab1 to spawn, register and release.
	first.kill()
	first.aliveEntered = make(chan struct{}, 1)
	first.aliveHold = make(chan struct{})

	late := make(chan error, 1)
	go func() { late <- tab2.SendPrompt(context.Background(), "tab2 after the second death") }()
	select {
	case <-first.aliveEntered:
	case <-time.After(3 * time.Second):
		t.Fatal("tab2 never reached reopen's sibling check; the fixture never opened the window")
	}

	if err := tab1.SendPrompt(context.Background(), "tab1 recovers the second death"); err != nil {
		t.Fatalf("tab1's prompt: %v", err)
	}
	reg.mu.RLock()
	inFlight := len(reg.spawning)
	reg.mu.RUnlock()
	if inFlight != 0 {
		t.Fatalf("%d reservation(s) still held: tab2 would WAIT on it, which is tether#60's path "+
			"and not the residual this stages", inFlight)
	}
	return reg, tab1, tab2, late
}

// TestSendPrompt_ASpentAttachmentHeldInItsSiblingCheckStillAdoptsTheRegistration is
// tether#82 itself: the reviewer's reproduction, with the same state reached under two
// schedulings and only one of them refusing the prompt.
func TestSendPrompt_ASpentAttachmentHeldInItsSiblingCheckStillAdoptsTheRegistration(t *testing.T) {
	seed := &fakeSession{sid: "shared-sid", events: make(chan agent.Event, 8)}
	first := &fakeSession{sid: "shared-sid", events: make(chan agent.Event, 8)}
	second := &fakeSession{sid: "shared-sid", events: make(chan agent.Event, 8)}
	rival := &fakeSession{sid: "shared-sid", events: make(chan agent.Event, 8)}
	p := staged(t, seed, first, second, rival)

	reg, tab1, tab2, late := stageSpentReopenRace(t, p, seed, first)

	close(first.aliveHold)
	select {
	case err := <-late:
		if err != nil {
			t.Fatalf("tab2's prompt was refused: %v\n\ntab2 lost its sibling check to a scheduling "+
				"delay and its budget was already spent, so it was told the session had stopped "+
				"accepting prompts — while tab1's replacement was registered under that very sid and "+
				"ready to answer. The budget bounds SPAWNING; spending it to refuse an ADOPTION "+
				"charges this connection for a race it never entered.", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("tab2's prompt never completed")
	}

	if n := p.Spawns(); n != 3 {
		t.Errorf("provider.Spawn was called %d times, want 3 (the seed, tab2's re-open, tab1's): "+
			"a spent attachment must ADOPT, never start a rival `cc --resume shared-sid`", n)
	}
	if attEntry(tab1) != attEntry(tab2) {
		t.Error("the two tabs hold different entries after the recovery; every sid-keyed route " +
			"(DeliverAction, InterruptSession, /wt/events) can only point at one of them")
	}
	// Order is deterministic: tab1's prompt is sent before tab2 is released.
	want := []string{"tab1 recovers the second death", "tab2 after the second death"}
	got := second.Prompts()
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("the replacement received %v, want %v", got, want)
	}
	tab2.mu.Lock()
	stillSpent := tab2.reopenSpent
	tab2.mu.Unlock()
	if !stillSpent {
		t.Error("tab2's budget came back: adopting is not re-opening, so it must neither spend " +
			"the budget nor refund it — a refund would let one connection start two agents")
	}
	reg.mu.RLock()
	registered := reg.sessions["shared-sid"]
	reg.mu.RUnlock()
	if registered != attEntry(tab1) {
		t.Error("r.sessions does not hold the replacement tab1 spawned")
	}
}

// TestSendPrompt_AnAdoptionThroughTheClaimThatRefusesSaysItAdopted covers the wording of
// the give-up message on the path tether#82 makes newly reachable.
//
// promptsilence_test.go already pins this for reopen's own sibling branch. The same
// event reached one layer down — through spawnEntry's consult — used to report
// "re-opened session %s would not take the prompt", which for a SPENT attachment names
// the single thing it is not allowed to have done. The message is a diagnostic, so
// getting it wrong costs nobody a prompt; it costs the next person reading a log a wrong
// account of which recovery was attempted, which in this function is the hardest thing to
// keep straight.
func TestSendPrompt_AnAdoptionThroughTheClaimThatRefusesSaysItAdopted(t *testing.T) {
	seed := &fakeSession{sid: "shared-sid", events: make(chan agent.Event, 8)}
	first := &fakeSession{sid: "shared-sid", events: make(chan agent.Event, 8)}
	second := &fakeSession{sid: "shared-sid", events: make(chan agent.Event, 8)}
	rival := &fakeSession{sid: "shared-sid", events: make(chan agent.Event, 8)}
	p := staged(t, seed, first, second, rival)

	_, _, _, late := stageSpentReopenRace(t, p, seed, first)

	// The replacement breaks its pipe AFTER tab1 has used it and BEFORE tab2 is released,
	// so tab2 adopts a session that is registered and alive and then refuses it. Not
	// kill(): a dead one would not be adopted at all.
	second.sendFails.Store(true)
	close(first.aliveHold)

	var err error
	select {
	case err = <-late:
	case <-time.After(5 * time.Second):
		t.Fatal("tab2's prompt never completed")
	}
	wantUndelivered(t, err, errBrokenPipe, "adopted through the claim, then refused")
	if !strings.Contains(err.Error(), "adopted the live session under") {
		t.Errorf("error = %q, want it to say the session was ADOPTED: this attachment was out of "+
			"budget, so \"re-opened\" would name the one thing it could not have done", err)
	}
	if n := p.Spawns(); n != 3 {
		t.Errorf("provider.Spawn was called %d times, want 3: a refused adoption must not spawn", n)
	}
}

// TestSendPrompt_ASpentAttachmentsAdoptionCheckDoesNotClaimTheKey pins the one design
// decision inside the fix that has a failure mode of its own.
//
// An adoptOnly call takes NO reservation. It could: the claim is what makes spawnEntry's
// ordinary consult authoritative. But the release publishes the holder's outcome to every
// waiter (see spawnReservation), and a call that registers nothing has no outcome worth
// inheriting — a would-be spawner parked behind it would be handed awaitSpawn's "neither
// an entry nor an error", a branch documented as unreachable for a call that did not
// panic. Declining the reservation keeps tether#60's protocol a conversation between
// callers that actually spawn.
//
// Staged by parking tab2 inside the adopt-only consult itself — the replacement's Alive()
// — and then asking, from the test goroutine, for the same key with budget available. It
// must be answered while tab2 is still parked.
func TestSendPrompt_ASpentAttachmentsAdoptionCheckDoesNotClaimTheKey(t *testing.T) {
	seed := &fakeSession{sid: "shared-sid", events: make(chan agent.Event, 8)}
	first := &fakeSession{sid: "shared-sid", events: make(chan agent.Event, 8)}
	second := &fakeSession{sid: "shared-sid", events: make(chan agent.Event, 8)}
	rival := &fakeSession{sid: "shared-sid", events: make(chan agent.Event, 8)}
	// Armed before anything runs, because the FIRST Alive() on the replacement is tab2's
	// adopt-only consult: tab1 spawns it and never polls it, and the sibling check that
	// runs before the consult asks about `first`, not this one.
	second.aliveEntered = make(chan struct{}, 1)
	second.aliveHold = make(chan struct{})
	p := staged(t, seed, first, second, rival)

	reg, tab1, tab2, late := stageSpentReopenRace(t, p, seed, first)
	close(first.aliveHold)

	select {
	case <-second.aliveEntered:
	case err := <-late:
		t.Fatalf("tab2 answered (err=%v) without consulting the registration: a spent attachment "+
			"that stops at its own sibling check is the tether#82 bug", err)
	case <-time.After(3 * time.Second):
		t.Fatal("tab2 never reached its adopt-only consult")
	}

	type result struct {
		outcome spawnOutcome
		err     error
	}
	spawner := make(chan result, 1)
	go func() {
		_, outcome, err := reg.spawnEntry(context.Background(), "fake",
			agent.SpawnConfig{ResumeSessionID: "shared-sid"}, WorkspaceBinding{}, spawnIfAbsent)
		spawner <- result{outcome, err}
	}()
	select {
	case got := <-spawner:
		if got.err != nil {
			t.Fatalf("a caller with budget left was refused while a spent one was consulting: %v", got.err)
		}
		if got.outcome != adoptedRegistration {
			t.Errorf("outcome = %v, want adoptedRegistration", got.outcome)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("a caller with budget left is parked behind the spent attachment's adopt-only " +
			"consult: the consult claimed the key, so this caller is waiting to inherit a verdict " +
			"from a call that will never register anything")
	}

	close(second.aliveHold)
	select {
	case err := <-late:
		if err != nil {
			t.Fatalf("tab2's prompt: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("tab2's prompt never completed")
	}
	if n := p.Spawns(); n != 3 {
		t.Errorf("provider.Spawn was called %d times, want 3: neither the spent attachment nor "+
			"the caller that adopted behind it may start an agent", n)
	}
	if attEntry(tab1) != attEntry(tab2) {
		t.Error("the two tabs hold different entries after the recovery")
	}
}

// stageSpentReopenAgainstAnInFlightSpawn parks a SPENT attachment so that it reaches its
// adopt-only consult while ANOTHER attachment is inside provider.Spawn holding the key's
// reservation — the one state in which a replacement exists and is registered nowhere.
//
// Two things had to be got right here, and the first version of this staging got neither.
//
// ORDER. An earlier version released tab1's spawn on a 300ms timer and had tab2 arrive
// whenever it arrived. That is reliable in the failing direction — a pre-fix tab2 refuses
// in the time it takes to take an uncontended mutex — but VACUOUS in the other one:
// release early and tab2 just finds the replacement registered and adopts it through the
// consult, which passes on the pre-fix build too. The assertion would have stayed true
// and the A/B would have silently gone. So tab2 is parked inside its own sibling check
// (fakeSession.aliveHold on `first`) and released only once tab1 is PROVABLY inside
// Spawn, and the caller — not a timer — decides when the spawn completes.
//
// EVIDENCE. Being released into the right state is not the same as reaching it, and the
// gap is real: between the release and the reservation tab2 runs a few lock-free steps
// during which the caller's close(p.release) could overtake it. So the staging ASSERTS
// that tab2 is still in flight before handing back (see the bottom of this function)
// rather than assuming it. That assertion is what found this: the coverage test below
// failed with the "nothing to adopt" verdict, which is what a tab2 that overtook the
// window gets, and it would have been read as a defect in the code under test.
//
// The caller releases the spawn (close(p.release)) and reads both results.
func stageSpentReopenAgainstAnInFlightSpawn(t *testing.T, p *gatedProvider, seed, first *fakeSession) (*Registry, *Attachment, *Attachment, chan error, chan error) {
	t.Helper()
	reg := NewRegistry(p)
	if _, err := reg.GetOrSpawnEntry(context.Background(), "", "fake"); err != nil {
		t.Fatalf("seed spawn: %v", err)
	}
	seed.announceInit()
	waitForRegistered(t, reg, "shared-sid")

	tab1, err := reg.Attach(context.Background(), "shared-sid", "fake", "")
	if err != nil {
		t.Fatalf("tab1 Attach: %v", err)
	}
	tab2, err := reg.Attach(context.Background(), "shared-sid", "fake", "")
	if err != nil {
		t.Fatalf("tab2 Attach: %v", err)
	}
	if n := p.Spawns(); n != 1 {
		t.Fatalf("spawns after both attaches = %d, want 1: both tabs must REUSE the seed", n)
	}

	// First death: tab2 recovers it and spends its one re-open doing so.
	seed.kill()
	if err := tab2.SendPrompt(context.Background(), "tab2 spends its budget"); err != nil {
		t.Fatalf("tab2's first prompt: %v", err)
	}
	tab2.mu.Lock()
	spent := tab2.reopenSpent
	tab2.mu.Unlock()
	if !spent {
		t.Fatal("tab2's budget is not spent: this staging proves nothing about the spent path")
	}
	if n := p.Spawns(); n != 2 {
		t.Fatalf("spawns after tab2's recovery = %d, want 2", n)
	}

	// Second death. tab2 goes FIRST and is parked in its sibling check, so that by the time
	// it is released tab1 already holds the reservation.
	first.kill()
	first.aliveEntered = make(chan struct{}, 1)
	first.aliveHold = make(chan struct{})

	tab2Err := make(chan error, 1)
	go func() {
		tab2Err <- tab2.SendPrompt(context.Background(), "tab2 while the replacement is mid-spawn")
	}()
	select {
	case <-first.aliveEntered:
	case <-time.After(3 * time.Second):
		t.Fatal("tab2 never reached reopen's sibling check; the fixture never opened the window")
	}

	tab1Err := make(chan error, 1)
	go func() { tab1Err <- tab1.SendPrompt(context.Background(), "tab1 recovers") }()
	p.waitEntered(t)
	reg.mu.RLock()
	held := len(reg.spawning)
	reg.mu.RUnlock()
	if held != 1 {
		t.Fatalf("%d reservation(s) in flight, want 1: tab1 must be INSIDE its spawn, which is "+
			"the only state in which a replacement exists and is registered nowhere", held)
	}

	// Released into exactly that state. What follows is the part that makes the staging
	// checked rather than hoped for: tab2 now walks a handful of lock-free steps between
	// its sibling check and the reservation, and the ONLY unbounded wait on that route is
	// awaitSpawn. So after a margin many orders of magnitude larger than those steps
	// need, "tab2 has not returned" IS "tab2 is parked on tab1's reservation" — and if it
	// has returned, this fails loudly instead of letting the caller assert against a
	// state that never happened. That is the direction the failure has to point: a margin
	// that turned out to be too short must make the test RED, never quietly green.
	close(first.aliveHold)
	time.Sleep(100 * time.Millisecond)
	select {
	case err := <-tab2Err:
		t.Fatalf("tab2 answered (err=%v) instead of waiting for the in-flight spawn: it never "+
			"reached tab1's reservation, so nothing below is being staged", err)
	default:
	}
	return reg, tab1, tab2, tab1Err, tab2Err
}

// TestSendPrompt_ASpentAttachmentWaitsForAReplacementThatIsStillSpawning is the half of
// tether#82 that no pre-check could ever have covered, spent budget or not.
//
// A replacement that is still inside provider.Spawn is registered NOWHERE, so
// liveEntry has nothing to find — yet it is milliseconds away from being exactly the
// session this attachment wanted to adopt. tether#60's reservation is the only thing
// that can see it, and reaching the reservation is what the spent budget used to
// prevent.
func TestSendPrompt_ASpentAttachmentWaitsForAReplacementThatIsStillSpawning(t *testing.T) {
	seed := &fakeSession{sid: "shared-sid", events: make(chan agent.Event, 8)}
	first := &fakeSession{sid: "shared-sid", events: make(chan agent.Event, 8)}
	second := &fakeSession{sid: "shared-sid", events: make(chan agent.Event, 8)}
	rival := &fakeSession{sid: "shared-sid", events: make(chan agent.Event, 8)}
	p := staged(t, seed, first, second, rival)
	// The seed and tab2's own re-open run straight through; tab1's re-open — the third
	// spawn — is held INSIDE provider.Spawn, which is where its reservation is live.
	p.gateFrom = 3

	_, tab1, tab2, tab1Err, tab2Err := stageSpentReopenAgainstAnInFlightSpawn(t, p, seed, first)
	close(p.release)

	select {
	case err := <-tab2Err:
		if err != nil {
			t.Fatalf("tab2's prompt was refused: %v\n\nits budget was spent, so it never reached the "+
				"reservation tab1 was holding — and refused a prompt that the replacement being spawned "+
				"under that very sid was about to be able to answer", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("tab2's prompt never completed")
	}
	select {
	case err := <-tab1Err:
		if err != nil {
			t.Fatalf("tab1's prompt: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("tab1's prompt never completed")
	}

	if n := p.Spawns(); n != 3 {
		t.Errorf("provider.Spawn was called %d times, want 3: waiting for the replacement must "+
			"not also start one", n)
	}
	if attEntry(tab1) != attEntry(tab2) {
		t.Error("the two tabs hold different entries after the recovery")
	}
	// Order is NOT deterministic here: tab1's reservation is released when its spawnEntry
	// returns, which is before tab1 delivers, so tab2 can reach the replacement first.
	got := second.Prompts()
	seen := map[string]bool{}
	for _, s := range got {
		seen[s] = true
	}
	if len(got) != 2 || !seen["tab1 recovers"] || !seen["tab2 while the replacement is mid-spawn"] {
		t.Errorf("the replacement received %v, want both tabs' prompts", got)
	}
}

// TestSendPrompt_ASpentAttachmentReportsAReplacementItOnlyWaitedFor covers the one path
// tether#82 makes newly reachable and that nothing else exercises: adoptOnly reaching
// reopen's GENERIC error branch.
//
// The waiter did not spawn and has no budget to spend, yet what it inherits from
// awaitSpawn is a spawn FAILURE, not the "nothing to adopt" verdict. The two must not be
// confused: the verdict says the sid is empty, this says the replacement could not be
// started — and the second must keep the cause, because it is the only thing that says
// why. Classified either way, because a bare error out of reopen is dropped by
// promptErrorEnvelope and the tab goes on looking healthy (tether#77).
func TestSendPrompt_ASpentAttachmentReportsAReplacementItOnlyWaitedFor(t *testing.T) {
	seed := &fakeSession{sid: "shared-sid", events: make(chan agent.Event, 8)}
	first := &fakeSession{sid: "shared-sid", events: make(chan agent.Event, 8)}
	// Only two sessions are ever built: the third spawn fails instead, which is why
	// staged's over-count guard is not tripped.
	p := staged(t, seed, first)
	p.gateFrom = 3
	p.errFrom = 3
	p.err = errors.New(`exec: "cc": executable file not found in $PATH`)

	_, _, tab2, tab1Err, tab2Err := stageSpentReopenAgainstAnInFlightSpawn(t, p, seed, first)
	close(p.release)

	select {
	case err := <-tab1Err:
		wantUndelivered(t, err, p.err, "the attachment whose own re-open failed")
	case <-time.After(5 * time.Second):
		t.Fatal("tab1's prompt never completed")
	}

	var err2 error
	select {
	case err2 = <-tab2Err:
	case <-time.After(5 * time.Second):
		t.Fatal("tab2's prompt never completed")
	}
	wantUndelivered(t, err2, p.err, "the adopt-only waiter")
	if strings.Contains(err2.Error(), "already used its one re-open") {
		t.Errorf("error = %q: the waiter was handed the nothing-to-adopt verdict for a spawn that "+
			"FAILED — two different states, and only one of them says the sid is empty", err2)
	}
	if !strings.Contains(err2.Error(), "could not be re-opened") {
		t.Errorf("error = %q, want the generic re-open failure", err2)
	}
	tab2.mu.Lock()
	stillSpent := tab2.reopenSpent
	tab2.mu.Unlock()
	if !stillSpent {
		t.Error("tab2's budget was refunded by a failure it only waited for")
	}
	if n := p.Spawns(); n != 3 {
		t.Errorf("provider.Spawn was called %d times, want 3: inheriting a failure must not make "+
			"the waiter retry it", n)
	}
}
