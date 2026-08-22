package session

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/piaobeizu/tether/internal/agent"
	"github.com/piaobeizu/tether/internal/wire"
)

// The read-only observer (/wt/events) is the subscriber nothing else in this
// package speaks for. The chat client's channels live on the Attachment, which
// carries them across every Entry replacement it performs; an observer arrives
// through Registry.SubscribeObserver and, before tether#75, was filed on whichever
// *Entry the sid happened to resolve to at that instant. Both recovery paths
// REPLACE that Entry, so the observer was left holding a corpse — with no
// prompt of its own to fail, and therefore no symptom but an absence.
//
// The tests below are grouped by the property each one pins, in this order:
//
//   - the subscription follows the SID — across a re-open, from before the
//     session is registered, and across an end-then-resume of the same id
//   - a SUPERSEDED entry is cut off, an entry that is merely gone is not
//   - the sid that will never come back says so instead of going quiet
//   - housekeeping: BroadcastAll's audience, no map leak, no double close
//   - the whole set under -race
//
// Deliberately unnumbered: an earlier draft numbered them and the numbering was
// stale within the hour, which is the same class of thing this file exists to
// catch.

// waitEnvelope reads one envelope of the given kind off ch, failing the test if
// none arrives in time. Written as "wait for a kind" rather than "read the next
// envelope" because fanOut's fence parser decides how text is chunked, and none
// of these tests is about that.
func waitEnvelope(t *testing.T, ch <-chan wire.Envelope, kind wire.EnvelopeKind, within time.Duration, what string) wire.Envelope {
	t.Helper()
	deadline := time.After(within)
	for {
		select {
		case env := <-ch:
			if env.Kind == kind {
				return env
			}
		case <-deadline:
			t.Fatalf("no %s envelope arrived within %s: %s", kind, within, what)
			return wire.Envelope{}
		}
	}
}

// expectNoEnvelope fails if anything arrives on ch within the window. Used only
// for the suppression property (a superseded entry must not reach the sid's
// observers), where "nothing happened" IS the assertion.
func expectNoEnvelope(t *testing.T, ch <-chan wire.Envelope, within time.Duration, what string) {
	t.Helper()
	select {
	case env := <-ch:
		t.Fatalf("received a %s envelope but expected silence: %s", env.Kind, what)
	case <-time.After(within):
	}
}

// observerCount reports how many channels are observing sid, under the lock
// SubscribeObserver writes with.
func observerCount(r *Registry, sid string) int {
	r.obsMu.RLock()
	defer r.obsMu.RUnlock()
	set, ok := r.observers[sid]
	if !ok {
		return 0
	}
	return len(set.chans)
}

// observedSids lists every sid with a live observer set, so a test can assert
// the map does not accumulate entries for sids nobody watches any more.
func observedSids(r *Registry) []string {
	r.obsMu.RLock()
	defer r.obsMu.RUnlock()
	out := make([]string, 0, len(r.observers))
	for sid := range r.observers {
		out = append(out, sid)
	}
	return out
}

// ─── the subscription follows the sid ───────────────────────────────────────

// TestObserver_ReopenKeepsTheReadOnlyStreamAlive is the core of tether#75, and
// the worse of the two replacement paths.
//
// Attachment.reopen replaces the Entry and KEEPS the sid: from outside, nothing
// about the session the observer named has changed. So there is not even a sid
// change for a client to notice — an observer bound to the old Entry simply
// stops hearing anything, forever, while the conversation carries on under the
// exact id it asked about.
//
// The assertion is deliberately about an envelope the REPLACEMENT produced, not
// about which set the channel is in: what was broken is that events stopped
// arriving, and a test that checked the bookkeeping instead would still pass if
// the delivery path forgot to consult it.
func TestObserver_ReopenKeepsTheReadOnlyStreamAlive(t *testing.T) {
	reused := &fakeSession{sid: "reused-sid", events: make(chan agent.Event, 8)}
	reopened := &fakeSession{sid: "reused-sid", events: make(chan agent.Event, 8)}
	p := &reopenProvider{first: reused, second: reopened}
	reg, att := seedReuse(t, p, "reused-sid")

	obs := make(chan wire.Envelope, 8)
	reg.SubscribeObserver("reused-sid", obs)
	defer reg.UnsubscribeObserver("reused-sid", obs)

	// Sanity: the observer hears the session it asked about BEFORE anything is
	// replaced. Without this the test could pass by never having worked at all.
	reused.events <- agent.Event{Kind: agent.EventText, Text: "before the death\n"}
	if got := waitEnvelope(t, obs, wire.KindMessage, time.Second, "the original session's text"); got.Payload == nil {
		t.Fatal("KindMessage carried no payload")
	}

	reused.kill()
	if err := att.SendPrompt(context.Background(), "recover me"); err != nil {
		t.Fatalf("SendPrompt: %v", err)
	}
	if got := p.Spawns(); got != 2 {
		t.Fatalf("spawns = %d, want 2: this test needs the re-open path", got)
	}
	if got := registeredEntry(reg, "reused-sid"); got != attEntry(att) {
		t.Fatal("the replacement is not the registration for the sid; the staging is wrong")
	}

	reopened.events <- agent.Event{Kind: agent.EventText, Text: "after the re-open\n"}
	env := waitEnvelope(t, obs, wire.KindMessage, time.Second,
		"the re-opened session under the SAME sid must keep reaching this sid's read-only observers")
	if got, _ := env.Payload.(string); got != "after the re-open\n" {
		t.Errorf("payload = %q, want %q", got, "after the re-open\n")
	}
}

// TestObserver_SubscribeBeforeTheSessionIsRegistered — the third face of the
// same bug, and the one with no recovery path involved at all.
//
// The old Registry.Subscribe resolved the sid to an Entry and returned silently
// when there was none, so an observer that arrived a few milliseconds before the
// session registered was dropped on the floor: no error, no events, forever —
// this route's analogue of the first-event drop window Entry.Subscribe exists to
// close on the chat path.
func TestObserver_SubscribeBeforeTheSessionIsRegistered(t *testing.T) {
	fs := &fakeSession{sid: "later-sid", events: make(chan agent.Event, 8)}
	reg := NewRegistry(&fakeProvider{sess: fs})

	obs := make(chan wire.Envelope, 8)
	reg.SubscribeObserver("later-sid", obs)
	defer reg.UnsubscribeObserver("later-sid", obs)

	// Attach, not GetOrSpawnEntry: a reconnect RESUMES the sid it brought, so the
	// entry is registered under that very id (tether#54). GetOrSpawnEntry mints a
	// fresh one for a stale sid, which would register the session under an id the
	// observer never named and make this test about something else.
	if _, err := reg.Attach(context.Background(), "later-sid", "fake", ""); err != nil {
		t.Fatalf("attach: %v", err)
	}
	fs.events <- agent.Event{Kind: agent.EventText, Text: "hello\n"}

	env := waitEnvelope(t, obs, wire.KindMessage, time.Second,
		"a subscription made before the session registered must still receive it")
	if got, _ := env.Payload.(string); got != "hello\n" {
		t.Errorf("payload = %q, want %q", got, "hello\n")
	}
}

// TestObserver_SurvivesTheSessionEndingAndBeingResumedUnderTheSameSid — the
// property that makes NOT retiring on an ordinary session end the right call.
//
// A session ends, its entry is evicted, and later a reconnect resumes the same
// sid. An Entry-bound observer was dead from the first eviction onwards; a
// sid-bound one is simply between sessions, and hears the resumed one. This is
// why retireObservers is called from the fallback rather than from evict or
// teardown: those cannot tell "over" from "between".
func TestObserver_SurvivesTheSessionEndingAndBeingResumedUnderTheSameSid(t *testing.T) {
	first := &fakeSession{sid: "durable-sid", events: make(chan agent.Event, 8)}
	second := &fakeSession{sid: "durable-sid", events: make(chan agent.Event, 8)}
	p := &reopenProvider{first: first, second: second}
	reg := NewRegistry(p)

	if _, err := reg.GetOrSpawnEntry(context.Background(), "durable-sid", "fake"); err != nil {
		t.Fatalf("first spawn: %v", err)
	}
	obs := make(chan wire.Envelope, 8)
	reg.SubscribeObserver("durable-sid", obs)
	defer reg.UnsubscribeObserver("durable-sid", obs)

	// End it the ordinary way: the stream closes, fanOut returns, teardown evicts.
	close(first.events)
	waitForCount(t, reg, 0)

	// A later reconnect resumes the same id.
	if _, err := reg.Attach(context.Background(), "durable-sid", "fake", ""); err != nil {
		t.Fatalf("re-attach: %v", err)
	}
	second.events <- agent.Event{Kind: agent.EventText, Text: "second life\n"}

	env := waitEnvelope(t, obs, wire.KindMessage, time.Second,
		"an observer that outlived the session must hear the one that resumes its sid")
	if got, _ := env.Payload.(string); got != "second life\n" {
		t.Errorf("payload = %q, want %q", got, "second life\n")
	}
}

// ─── superseded is cut off, merely-gone is not ──────────────────────────────

// TestObserver_ASupersededEntryStopsReachingTheSidsObservers is the constraint
// late binding brings with it, and the reason delivery asks a question rather
// than just looking the sid up.
//
// On the re-open path the corpse's stream is still draining while the
// replacement is already registered under the same sid — Attachment.adopt spells
// out what leaving a subscriber on it costs, in terms of what the user sees: the
// dead session's half-sentence interleaved into the live one's answer. An
// Entry-keyed observer could not meet that (it was on one entry or the other); a
// sid-keyed one can, so the corpse is cut off at delivery.
func TestObserver_ASupersededEntryStopsReachingTheSidsObservers(t *testing.T) {
	reused := &fakeSession{sid: "reused-sid", events: make(chan agent.Event, 8)}
	reopened := &fakeSession{sid: "reused-sid", events: make(chan agent.Event, 8)}
	p := &reopenProvider{first: reused, second: reopened}
	reg, att := seedReuse(t, p, "reused-sid")

	dead := attEntry(att)
	obs := make(chan wire.Envelope, 8)
	reg.SubscribeObserver("reused-sid", obs)
	defer reg.UnsubscribeObserver("reused-sid", obs)

	reused.kill()
	if err := att.SendPrompt(context.Background(), "recover me"); err != nil {
		t.Fatalf("SendPrompt: %v", err)
	}
	if attEntry(att) == dead {
		t.Fatal("the attachment was not re-pointed; the staging is wrong")
	}

	// The corpse coughs up the tail its pipe had buffered. It is no longer the
	// registration for this sid, so nothing of it may reach the sid's observers.
	reused.events <- agent.Event{Kind: agent.EventText, Text: "half a sentence from the corpse\n"}
	expectNoEnvelope(t, obs, 200*time.Millisecond,
		"a superseded entry must not interleave its tail into the replacement's stream")

	// …and the replacement still gets through, so the suppression is aimed at the
	// corpse rather than at the sid.
	reopened.events <- agent.Event{Kind: agent.EventText, Text: "the live answer\n"}
	env := waitEnvelope(t, obs, wire.KindMessage, time.Second, "the replacement must still be delivered")
	if got, _ := env.Payload.(string); got != "the live answer\n" {
		t.Errorf("payload = %q, want %q", got, "the live answer\n")
	}
}

// TestObserver_AnEndedSessionStillDeliversItsTurnEnder is the plain end-to-end
// negative control for the test above: an ordinary session end must still reach
// the observer, turn-ender included, or the browser is left on a spinner.
//
// It does NOT discriminate the "superseded, not absent" half of the guard, and
// saying so is the point of this paragraph — the first version of this comment
// claimed it did, on the strength of "fanOut evicts and THEN broadcasts the
// turn-ender". That ordering is backwards: teardown's evict is a DEFER at the
// top of fanOut, so it runs after the sawInit branch, and the entry is still
// registered while the turn-ender goes past deliverObservers. A mutation that
// widened the guard to suppress on absence survived this test, which is how the
// claim was caught. The test that does discriminate it is the next one.
func TestObserver_AnEndedSessionStillDeliversItsTurnEnder(t *testing.T) {
	fs := &fakeSession{sid: "ending-sid", events: make(chan agent.Event, 8)}
	reg := NewRegistry(&fakeProvider{sess: fs})

	if _, err := reg.GetOrSpawnEntry(context.Background(), "ending-sid", "fake"); err != nil {
		t.Fatalf("spawn: %v", err)
	}
	obs := make(chan wire.Envelope, 8)
	reg.SubscribeObserver("ending-sid", obs)
	defer reg.UnsubscribeObserver("ending-sid", obs)

	// init, so fanOut's sawInit gate lets the terminal result out at all.
	fs.events <- agent.Event{Kind: agent.EventInit, SessionID: "ending-sid"}
	fs.events <- agent.Event{Kind: agent.EventText, Text: "mid-answer\n"}
	waitEnvelope(t, obs, wire.KindMessage, time.Second, "the session's own text")

	close(fs.events) // the agent's stream ends mid-turn

	waitEnvelope(t, obs, wire.KindResult, time.Second,
		"an ordinary session end must still deliver the turn-ender")
}

// TestObserver_AnUnregisteredEntryStillReachesItsObservers is the one that
// separates "superseded" from "absent", and the reason the guard tests
// `registered && cur != e` rather than the shorter `cur != e`.
//
// liveEntry un-registers a corpse OUT OF BAND: any caller asking about a sid
// whose agent has exited drops it, which can happen while that entry's fanOut is
// still draining events the pipe had buffered. Nothing has replaced it, so those
// events — and the turn-ender that stops the browser's spinner — are still this
// sid's news. The shorter guard would swallow every one of them.
//
// Staged deterministically rather than by racing: the session is marked dead and
// a single IsLive call performs the eviction, while its event stream is left
// open so fanOut keeps running.
func TestObserver_AnUnregisteredEntryStillReachesItsObservers(t *testing.T) {
	fs := &fakeSession{sid: "dropped-sid", events: make(chan agent.Event, 8)}
	reg := NewRegistry(&fakeProvider{sess: fs})

	if _, err := reg.Attach(context.Background(), "dropped-sid", "fake", ""); err != nil {
		t.Fatalf("attach: %v", err)
	}
	obs := make(chan wire.Envelope, 8)
	reg.SubscribeObserver("dropped-sid", obs)
	defer reg.UnsubscribeObserver("dropped-sid", obs)

	fs.events <- agent.Event{Kind: agent.EventInit, SessionID: "dropped-sid"}
	fs.events <- agent.Event{Kind: agent.EventText, Text: "first half\n"}
	waitEnvelope(t, obs, wire.KindMessage, time.Second, "the session before it was dropped")

	// The agent has exited but its stream has not closed — the window
	// Registry.teardown spells out. A reconnect asking about the sid drops the
	// registration on the spot.
	fs.dead.Store(true)
	if reg.IsLive("dropped-sid") {
		t.Fatal("IsLive still reports the corpse alive; liveEntry did not drop it")
	}
	if got := registeredEntry(reg, "dropped-sid"); got != nil {
		t.Fatal("the entry is still registered; the staging is wrong")
	}

	// Nothing replaced it, so what it still has to say is still this sid's news.
	fs.events <- agent.Event{Kind: agent.EventText, Text: "second half\n"}
	env := waitEnvelope(t, obs, wire.KindMessage, time.Second,
		"an entry dropped out of band with no replacement must keep reaching the sid's observers")
	if got, _ := env.Payload.(string); got != "second half\n" {
		t.Errorf("payload = %q, want %q", got, "second half\n")
	}

	close(fs.events)
	waitEnvelope(t, obs, wire.KindResult, time.Second,
		"…including the turn-ender, without which the browser's spinner never stops")
}

// ─── the sid that will never come back says so ──────────────────────────────

// TestObserver_FallbackEndsTheStreamRatherThanGoingQuiet covers the half of
// tether#75 that late binding cannot reach.
//
// Attachment.resolve's fallback replaces the Entry with one under a NEW sid: the
// resume failed, so nothing will ever be registered under the old id again by
// this attachment. An observer of it is not between sessions, it is finished —
// and being told so is the difference between a client that reconnects and one
// that sits on a healthy-looking transport that will never speak again.
func TestObserver_FallbackEndsTheStreamRatherThanGoingQuiet(t *testing.T) {
	live := &fakeSession{sid: "fresh-sid", events: make(chan agent.Event, 8)}
	p := &deadThenLiveProvider{dead: newDeadSession(), live: live}
	reg := NewRegistry(p)

	att, err := reg.Attach(context.Background(), "doomed-sid", "fake", "")
	if err != nil {
		t.Fatalf("Attach: %v", err)
	}
	obs := make(chan wire.Envelope, 8)
	retired := reg.SubscribeObserver("doomed-sid", obs)
	defer reg.UnsubscribeObserver("doomed-sid", obs)

	select {
	case <-retired:
		t.Fatal("the sid was reported retired before the resume had even failed")
	default:
	}

	res, err := att.Resolve(context.Background())
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if !res.Recovered || res.SID != "fresh-sid" {
		t.Fatalf("Resolution = %+v, want the fallback's fresh sid; the staging is wrong", res)
	}

	select {
	case <-retired:
	case <-time.After(time.Second):
		t.Fatal("the observed sid was abandoned for a session under a different id and its " +
			"observers were never told: they wait forever on a sid that cannot produce another event")
	}
}

// TestObserver_RetiringOneSidLeavesAnotherSidsObserversAlone — the signal is
// aimed at one sid, not broadcast at every observer in the daemon. Cheap to
// assert and the obvious way for a future refactor of retireObservers to go
// wrong quietly.
func TestObserver_RetiringOneSidLeavesAnotherSidsObserversAlone(t *testing.T) {
	reg := NewRegistry()

	doomed := make(chan wire.Envelope, 1)
	bystander := make(chan wire.Envelope, 1)
	doomedSig := reg.SubscribeObserver("doomed-sid", doomed)
	bystanderSig := reg.SubscribeObserver("other-sid", bystander)

	reg.retireObservers("doomed-sid")

	select {
	case <-doomedSig:
	default:
		t.Error("the retired sid's observers were not signalled")
	}
	select {
	case <-bystanderSig:
		t.Error("an unrelated sid's observers were signalled too")
	default:
	}
	if got := observerCount(reg, "other-sid"); got != 1 {
		t.Errorf("other-sid observers = %d, want 1: retiring one sid dropped another's set", got)
	}
}

// TestObserver_RetireLeavesASidThatSomethingHasSinceRegistered — the gap between
// "this caller abandoned the sid" and "the sid is abandoned", which a deep review
// found and which is wide enough to walk through.
//
// Attachment.resolve reaches retireObservers a long way after evicting the failed
// resume: Session().Close() waits on a child process and spawnEntry starts one,
// so the window spans two process operations. A second connection attaching with
// the same sid inside it finds nothing live, takes the `--resume` path, and
// registers a session under that sid — a supported state (two clients on one sid
// is what Attach's reuse branch is for) and the very recovery resolve's own doc
// says a retryable failure is expected to get. Retiring on the first caller's
// intent alone would end the streams of observers whose sid had just come back.
func TestObserver_RetireLeavesASidThatSomethingHasSinceRegistered(t *testing.T) {
	fs := &fakeSession{sid: "revived-sid", events: make(chan agent.Event, 8)}
	reg := NewRegistry(&fakeProvider{sess: fs})

	obs := make(chan wire.Envelope, 8)
	sig := reg.SubscribeObserver("revived-sid", obs)
	defer reg.UnsubscribeObserver("revived-sid", obs)

	// Somebody re-registered the sid while the abandoning caller was between its
	// own evict and this call.
	if _, err := reg.Attach(context.Background(), "revived-sid", "fake", ""); err != nil {
		t.Fatalf("attach: %v", err)
	}

	reg.retireObservers("revived-sid")

	select {
	case <-sig:
		t.Fatal("a sid with a live registration was retired: its observers' streams were ended " +
			"while the session they name is answering")
	default:
	}
	if got := observerCount(reg, "revived-sid"); got != 1 {
		t.Fatalf("observers = %d, want 1: the set was dropped for a live sid", got)
	}

	// …and they are still wired up, which is the property the count alone does not
	// prove.
	fs.events <- agent.Event{Kind: agent.EventText, Text: "still here\n"}
	waitEnvelope(t, obs, wire.KindMessage, time.Second,
		"an observer of a re-registered sid must still be receiving")
}

// TestObserver_RekeyRetiresTheOldKeysObservers covers the THIRD way a sid stops
// naming the session an observer asked about, alongside the two Attachment
// recovery paths — and the only one where the registration MOVES rather than
// being replaced or abandoned.
//
// Registry.rekey relocates an entry from the id it was spawned under to the one
// its agent announced. Late binding cannot help an observer of the old key,
// because nothing will ever be registered under it again: it is left waiting
// forever with no signal, which is verbatim the symptom tether#75 exists to end.
//
// Reachable when the client legitimately holds the spawned-under id — i.e. the
// situation rekey's own Warn exists to announce, a cc that has stopped adopting
// `--session-id`. The fake provider models exactly that shape (it mints its own
// id and announces it on the stream).
func TestObserver_RekeyRetiresTheOldKeysObservers(t *testing.T) {
	fs := &fakeSession{sid: "announced-sid", events: make(chan agent.Event, 8)}
	reg := NewRegistry(&fakeProvider{sess: fs})

	// Spawn under a key the client is holding; the agent will announce a different
	// one, which is what makes rekey move the registration.
	if _, err := reg.Attach(context.Background(), "spawned-under-sid", "fake", ""); err != nil {
		t.Fatalf("attach: %v", err)
	}
	obs := make(chan wire.Envelope, 8)
	sig := reg.SubscribeObserver("spawned-under-sid", obs)
	defer reg.UnsubscribeObserver("spawned-under-sid", obs)

	fs.announceInit()
	waitForRegistered(t, reg, "announced-sid")

	select {
	case <-sig:
	case <-time.After(time.Second):
		t.Fatal("the registration moved off the sid these observers named and they were never " +
			"told: nothing will be registered under it again, so they wait forever")
	}
	if got := observerCount(reg, "spawned-under-sid"); got != 0 {
		t.Errorf("observers of the old key = %d, want 0: the set must be dropped with the signal", got)
	}
}

// ─── housekeeping ───────────────────────────────────────────────────────────

// TestObserver_BroadcastAllReachesAnObserverWithNoLiveSession — BroadcastAll's
// two callers (the permission request fan-out and the permissions-withdrawn
// batch, both in server/mux.go) address the whole daemon, and an observer is part
// of it on the strength of being connected. This sentence said "three (the
// permission fan-out and the two shell lock events)" and had drifted twice by the
// time it was corrected: it was right when the tether#75 work wrote it (PR #167),
// tether#137 made it four by adding the permissions-withdrawn batch without
// touching it, and tether#121 took the two shell lock events away with the shell
// input lock. Hence two, and hence the callers named rather than counted.
//
// Before tether#75 such an observer had never been subscribed at all
// when its sid had no registration, so it missed daemon-wide notices for a
// reason that had nothing to do with them.
func TestObserver_BroadcastAllReachesAnObserverWithNoLiveSession(t *testing.T) {
	reg := NewRegistry()

	obs := make(chan wire.Envelope, 4)
	reg.SubscribeObserver("no-session-here", obs)
	defer reg.UnsubscribeObserver("no-session-here", obs)

	reg.BroadcastAll(wire.Envelope{Kind: wire.KindPermission, Payload: "may I"})

	waitEnvelope(t, obs, wire.KindPermission, time.Second,
		"a daemon-wide notice must reach every connected observer")
}

// TestObserver_BroadcastAllDeliversOnceToAnObserverOfALiveSession pins that
// widening the audience did not also duplicate it: the observer is no longer
// reachable through the Entry's own subscriber set, so it must be delivered to
// exactly once, from the observers map alone.
func TestObserver_BroadcastAllDeliversOnceToAnObserverOfALiveSession(t *testing.T) {
	fs := &fakeSession{sid: "one-copy-sid", events: make(chan agent.Event, 8)}
	reg := NewRegistry(&fakeProvider{sess: fs})
	if _, err := reg.GetOrSpawnEntry(context.Background(), "one-copy-sid", "fake"); err != nil {
		t.Fatalf("spawn: %v", err)
	}

	obs := make(chan wire.Envelope, 8)
	reg.SubscribeObserver("one-copy-sid", obs)
	defer reg.UnsubscribeObserver("one-copy-sid", obs)

	reg.BroadcastAll(wire.Envelope{Kind: wire.KindPermission, Payload: "may I"})
	waitEnvelope(t, obs, wire.KindPermission, time.Second, "the notice itself")

	select {
	case env := <-obs:
		t.Fatalf("a second copy of the daemon-wide notice arrived (%s); one observer, one delivery", env.Kind)
	case <-time.After(100 * time.Millisecond):
	}
}

// TestObserver_UnsubscribeDropsTheSidsSet — a daemon that runs for weeks must
// not accumulate one map entry per sid ever observed. The empty set is dropped,
// which is also what makes a later Subscribe to the same sid get a fresh
// (unclosed) signal rather than inheriting a stale one.
func TestObserver_UnsubscribeDropsTheSidsSet(t *testing.T) {
	reg := NewRegistry()

	a := make(chan wire.Envelope, 1)
	b := make(chan wire.Envelope, 1)
	reg.SubscribeObserver("shared-sid", a)
	reg.SubscribeObserver("shared-sid", b)
	if got := observerCount(reg, "shared-sid"); got != 2 {
		t.Fatalf("observers = %d, want 2", got)
	}

	reg.UnsubscribeObserver("shared-sid", a)
	if got := observerCount(reg, "shared-sid"); got != 1 {
		t.Errorf("observers after one Unsubscribe = %d, want 1", got)
	}
	reg.UnsubscribeObserver("shared-sid", b)
	if got := observedSids(reg); len(got) != 0 {
		t.Errorf("observed sids = %v, want none: the empty set must be dropped", got)
	}

	// Unsubscribing an unknown sid, and a channel that is no longer there, are
	// both no-ops rather than panics — serveEvents unsubscribes in a defer that
	// runs after retirement has already removed the set.
	reg.UnsubscribeObserver("shared-sid", a)
	reg.UnsubscribeObserver("never-seen", a)
}

// TestObserver_RetireIsIdempotentAndSurvivesALateUnsubscribe — retirement
// closes a channel, so "exactly once" has to be a property of the code rather
// than of the call order. Both orders a real serveEvents can produce are
// exercised: the retire itself twice (two fallbacks on one sid, however
// unlikely), and the observer's deferred Unsubscribe arriving after it.
func TestObserver_RetireIsIdempotentAndSurvivesALateUnsubscribe(t *testing.T) {
	reg := NewRegistry()

	obs := make(chan wire.Envelope, 1)
	sig := reg.SubscribeObserver("doomed-sid", obs)

	reg.retireObservers("doomed-sid")
	reg.retireObservers("doomed-sid") // must not close the signal a second time
	reg.UnsubscribeObserver("doomed-sid", obs)

	select {
	case <-sig:
	default:
		t.Error("the signal was not closed")
	}

	// A client that reconnects naming the same sid gets a NEW set with its own
	// unclosed signal — the registry keeps no tombstone, which Subscribe's doc
	// states as the residual rather than hiding it.
	again := make(chan wire.Envelope, 1)
	sig2 := reg.SubscribeObserver("doomed-sid", again)
	select {
	case <-sig2:
		t.Error("a fresh subscription inherited the retired signal")
	default:
	}
}

// ─── under -race ────────────────────────────────────────────────────────────

// TestObserver_ConcurrentSubscribeBroadcastAndRetire_NoRace hammers every
// mutator of the observers map from different goroutines at once, which is what
// `go test -race` needs to have anything to say about a subscriber set that
// moved out from behind the Entry's own lock.
//
// It asserts no per-goroutine outcome — the interleavings are genuinely
// nondeterministic and anything sharper would be a flake. What it asserts is
// that nothing races, nothing panics (a close of an already-closed signal, or a
// send on a retired one, would), and the map fully drains.
func TestObserver_ConcurrentSubscribeBroadcastAndRetire_NoRace(t *testing.T) {
	const goroutines = 24
	fs := &fakeSession{sid: "hot-sid", events: make(chan agent.Event, 64)}
	reg := NewRegistry(&fakeProvider{sess: fs})
	e, err := reg.GetOrSpawnEntry(context.Background(), "hot-sid", "fake")
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}

	var wg sync.WaitGroup
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			sid := "hot-sid"
			if i%3 == 0 {
				sid = "cold-sid" // a sid with no registration at all
			}
			ch := make(chan wire.Envelope, 8)
			sig := reg.SubscribeObserver(sid, ch)
			// Drain, so a full channel never turns this into a test of the drop
			// policy. Stops when the goroutine unsubscribes below.
			done := make(chan struct{})
			go func() {
				for {
					select {
					case <-ch:
					case <-done:
						return
					}
				}
			}()

			reg.BroadcastAll(wire.Envelope{Kind: wire.KindMessage, Payload: "ping"})
			reg.broadcast(e, wire.Envelope{Kind: wire.KindMessage, Payload: "fan"})
			if i%4 == 1 {
				reg.retireObservers(sid)
			}
			select {
			case <-sig:
			default:
			}
			reg.UnsubscribeObserver(sid, ch)
			close(done)
		}(i)
	}
	wg.Wait()

	for _, sid := range observedSids(reg) {
		if got := observerCount(reg, sid); got != 0 {
			t.Errorf("sid %q still has %d observers after every goroutine unsubscribed", sid, got)
		}
	}
}
