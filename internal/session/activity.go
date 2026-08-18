package session

// Is a turn in flight on this conversation right now? (tether#103)
//
// The session list knew nothing about activity. The only dot on a row is
// `.ws-dot.live`, which means "this is the one you are looking at" and reads like
// an activity light; tether#101's `running` badge means "some background agent
// holds this sid", which is a fact about POSSESSION and not about motion. So a
// list a human is expected to choose from could not answer the first question
// anyone asks of it.
//
// # What the answer is allowed to claim, and why it is not "the model is talking"
//
// The request was for "the model is currently emitting output". That precision is
// not available, and the reason is not effort:
//
//   - For a cc process, all tether can read is the registry's `status`, and `busy`
//     there is `isLoading || delegatedActive` — the whole turn, tool execution
//     included. "The model is emitting tokens" is simply FALSE of a row whose
//     agent is three minutes into a `go test`.
//   - For its OWN sessions tether does see the token stream, but only for the one
//     session the browser is attached to, and the two sources cannot be reconciled
//     at that granularity.
//
// "A turn is in flight" is true of both, so it is the one sentence a row can carry
// for every row. sessionlist.go's rule decides the rest: "A row that is right most
// of the time is the lying list this feature exists to avoid."
//
// # Two sources, one vocabulary
//
// cc's live-session registry (CCRegistry, tether#101) answers for every cc process
// on the machine. Registry.LiveTurns answers for the sessions THIS daemon is
// driving — and it has to exist, because the sessions tether spawns are precisely
// the ones cc's registry says nothing about: they are `--print` launches, and a
// `--print` launch writes no `status` at all (123 of the 140 records on the
// reference machine at 2026-08-18T10:45Z, and 123 of 123 of that shape — see
// CCLiveRecord.Status for the census and for why it is timestamped). Without the
// daemon's own count
// this feature would be blank for the majority of the list — which is a FALSE
// NEGATIVE on every row that is actually working, i.e. the failure mode the whole
// design is trying not to reproduce.

// SessionActivityPath is the route the SPA polls, and it is deliberately its own
// TOP-LEVEL path rather than a leaf under /api/v1/sessions/.
//
// # Why a separate endpoint at all
//
// GET /api/v1/sessions costs a directory scan plus, PER SESSION, a stat and a
// bounded transcript read — ~1.4 MB of (page-cached) reading at the ~90 sessions a
// real profile has (see titlePrefixBytes). All of that answers questions whose
// answers do not change: a session's title and its work item are not going to
// differ between two polls three seconds apart. Polling it to refresh a dot would
// pay the whole list's cost for the one field that moves.
//
// What this endpoint costs is ONE registry scan (~3.0 ms / 44 KB over ~140 records,
// measured for tether#101, and unlike the holder scan this one really does read
// /proc once per record — it has to, since it classifies every kind) plus one walk
// of the live-session map under a read lock. No titles, no stat of any transcript. That
// is what makes a 3-second poll defensible, and it leaves List's cost model
// untouched rather than tuning it.
//
// # Why NOT under /api/v1/sessions/
//
// Not because it could not work — measured, an exact ServeMux pattern beats the
// `/api/v1/sessions/` prefix handler, so `/api/v1/sessions/activity` would in fact
// have been routed here. Two better reasons:
//
//   - A top-level path needs no argument about pattern precedence at all. The one
//     under /sessions/ is correct only as long as nobody reads
//     `mux.HandleFunc("/api/v1/sessions/", sessionSub)` and concludes the subtree
//     is owned.
//   - Everything under /api/v1/sessions/ passes ValidSessionID because the sid
//     there BECOMES A FILE PATH (see sessionAPIHandlers). This endpoint takes no
//     sid, so putting it in that subtree would add the one route in it that is not
//     about a session id.
//
// The hazard that is real and easy to miss is the other neighbour: /api/v1/session/
// (singular, handleLockForce) is also a prefix handler, and "session-activity"
// is one hyphen away from being inside it. A routing test pins that negative.
const SessionActivityPath = "/api/v1/session-activity"

// The three states a row can be in. Absence from the map is the fourth answer and
// the most common one: nothing live holds that sid, so it is NECESSARILY not
// running.
//
// Absence rather than a fourth string, because the two are not the same kind of
// statement. `idle` is something cc or this daemon TOLD us; absence is the
// conclusion that follows from no process holding the id. Encoding it as a value
// would also put a word on the wire for every session on the machine, on every
// poll, to say nothing.
const (
	// SessionActivityWorking — a turn is in flight. The only state that makes a
	// positive claim about motion.
	SessionActivityWorking = "working"

	// SessionActivityIdle — a process has this conversation open and no turn is in
	// flight.
	//
	// "No turn in flight" and not "between turns": this is also the state for cc's
	// `waiting` (mid-conversation but blocked on the user) and `shell` (a shell
	// task running while the agent itself is idle), and "between turns" would be
	// false for both.
	SessionActivityIdle = "idle"

	// SessionActivityHeld — a live cc process has this conversation open, and
	// whether a turn is in flight is NOT OBSERVABLE.
	//
	// Deliberately not called `unknown`. "A coding agent has this open" is a fact
	// we hold; only the inside of it is opaque, so the row has something true to
	// say rather than a shrug. It is also the safe home for a `status` this build
	// has not been taught: a value that cannot be classified degrades to "cannot
	// tell", never to a claim.
	SessionActivityHeld = "held"
)

// ActivityIndex answers "which sessions have a turn in flight right now?".
//
// Both fields are optional, on the same terms as SessionIndex's stores: a daemon
// assembled without one answers from the other, and one without either answers
// `{}` — which is also what a daemon on a host with no /proc answers, and is the
// fail-towards-silence direction CCRegistry's file doc argues for.
type ActivityIndex struct {
	// Reg is the live-session registry of THIS daemon. nil = no row reports on
	// tether's own sessions, which for most rows means no row reports at all (see
	// the file doc).
	//
	// It must be the same *Registry the rest of the daemon uses, for the reason
	// SessionIndex.CCJobs states: two instances are two answers, and the symptom is
	// a marker that disagrees with the transcript on screen.
	Reg *Registry
	// CCJobs is the reader over cc's live-session registry. Must be the SAME
	// instance SessionIndex.CCJobs and Registry.ccLiveJob use — one scan, one
	// answer.
	CCJobs *CCRegistry
}

// States returns one state per session that something live is holding, keyed by
// sid. Sessions nothing holds are ABSENT rather than present-and-idle.
//
// # The merge, stated once
//
// cc's records first, reduced by rank (working > held > idle) because one sid can
// have several live records and they can disagree — measured for tether#101, 7 of
// 103 sids carried more than one record and the worst carried 26. Then tether's
// own registrations, which take PRECEDENCE over `held` — but not over a cc record
// that positively said `busy`.
//
// Both halves of that need their reason.
//
// # Why `held` outranks `idle` among cc records
//
// `idle` is a positive claim that nothing is happening. `held` is a refusal to
// claim. If one live record says "between turns" and another for the same sid
// cannot be read, the row must not paper the second one over with the first — that
// is the mislabel this whole slice exists to remove. `working` still beats both,
// because a refusal to claim must never suppress a fact.
//
// # Why tether's own registration wins over `held`, and NOT over `working`
//
// Because for these sessions the two sources describe the SAME PROCESS, and cc's
// half of it is blind. Every cc tether spawns is a `--print` launch; kind comes
// from `UBe() ?? "interactive"` where UBe only reads CLAUDE_CODE_SESSION_KIND
// (which nothing but cc's own background-job spawners sets), so tether's spawns
// register as kind "interactive" — and they write no `status`, so they classify as
// `held`. Merged as a plain max, that `held` would outrank tether's own `idle` and
// EVERY tether session would render as "cannot tell".
//
// The override stops exactly there. A cc record that positively said `busy` is not
// blind, and a registration that merely has no turn in flight must not overrule
// it: cc refuses a `--resume` only for NON-interactive holders, so a conversation a
// terminal has open can also be registered here, and the two are then genuinely
// two processes. Overriding in that case would replace another process's
// observation with this one's silence — the only way outright precedence turns a
// refusal to claim into a false claim.
//
// Named residual, and it is narrower than the previous sentence made it sound: a
// terminal cc whose record carries NO readable status (an old build, or a
// status-less launch) on a sid this daemon has also registered still reports the
// daemon's answer. The alternative — telling tether's own spawns from a terminal's
// by `entrypoint` — would invent a rule cc itself does not have, which is the
// mistake #101 deliberately avoided in the holder rule.
func (a *ActivityIndex) States() map[string]string {
	// Never nil: the handler encodes this map directly, and a nil map marshals to
	// `null`, which the SPA cannot index.
	out := make(map[string]string)
	if a == nil {
		return out
	}

	if a.CCJobs != nil {
		for sid, recs := range a.CCJobs.LiveRecords() {
			for _, rec := range recs {
				if state := ccStatusActivity(rec.Status); activityRank(state) > activityRank(out[sid]) {
					out[sid] = state
				}
			}
		}
	}

	if a.Reg != nil {
		for sid, inFlight := range a.Reg.LiveTurns() {
			if inFlight {
				out[sid] = SessionActivityWorking
				continue
			}
			// A registration with no turn in flight overrides `held` — that is the
			// whole point of the precedence — but NOT a cc record that positively
			// said `busy`. The precedence's argument is that cc's half is BLIND for
			// these sessions, and a record carrying a status is not blind: a
			// conversation open in a terminal AND registered here (cc refuses a
			// `--resume` only for non-interactive holders, so this daemon can and
			// does resume a sid a terminal has open) would otherwise render `idle` —
			// "no turn in flight" — while the terminal was mid-turn. Reporting the
			// daemon's silence over another process's observation is the one case
			// where outright precedence turns a refusal to claim into a false claim.
			if out[sid] != SessionActivityWorking {
				out[sid] = SessionActivityIdle
			}
		}
	}

	return out
}

// ccStatusActivity classifies one cc `status` value.
//
// # The vocabulary is cc's, read from the binary rather than from this machine
//
// cc validates status against an enum and DROPS anything outside it on read
// (installed cc 2.1.233):
//
//	XB_ = ["busy","shell","idle","waiting"];
//	function JB_(e){ return XB_.includes(e) ? e : void 0 }    // status:JB_(c.status)
//
// and the function that computes it says which one is a turn:
//
//	function k2h(e){ let t=aTw(e);
//	  if(t!==void 0) return {status:"waiting", waitingFor:t, working:!1};
//	  return {status: e.isLoading||e.delegatedActive ? "busy":"idle",
//	          waitingFor:void 0, working:e.isQueryActive} }
//	// and downstream:  zd = Fp === "idle" && Kg ? "shell" : Fp
//
// So `busy` is the ONLY value that means a turn is in flight. `waiting` is
// labelled `working: false` by cc itself — mid-conversation but blocked on the
// user (its `waitingFor` names "input needed", "dialog open", "sandbox request",
// "worker request"). `shell` is an overlay applied ONLY where the base was already
// `idle`, i.e. a shell task running while the agent is not in a turn.
//
// Taking this from the source matters: the wi described the vocabulary as "only
// busy / idle", which would have made `busy` the complement of `idle` and left
// `shell` and `waiting` to fall through to whichever branch the code happened to
// end with. `shell` is not hypothetical — one record on the reference machine
// carried it on 2026-08-18.
//
// # Keyed on the status, NOT on the kind
//
// The tempting rule is "read status only when kind != interactive", inherited from
// LiveJobs. It is wrong here, and the argument is READ FROM THE BINARY — which
// matters, because the reference machine cannot settle it and an earlier draft of
// this comment claimed it could:
//
//   - The status write is not gated on kind. It is one unconditional effect inside
//     the shared App/REPL component (quoted above), and nothing in the record
//     writer consults `kind`.
//   - `kind` defaults to "interactive" for anything that is not one of cc's own
//     background-job spawns, so a TERMINAL launch and a `--print` launch are the
//     same kind while only the terminal one mounts the component that writes a
//     status.
//
// NOT observed here, and the distinction is the point: on the reference machine
// (2026-08-18) kind predicts status presence perfectly — every record with a status
// is `bg` and every `interactive` one has none — and there were ZERO live
// interactive records, so the sample cannot express the disagreement. That is
// precisely the fallacy ccInteractiveKind's own doc warns about fifty lines away
// ("Agreement with a sample is not evidence when the sample cannot express the
// disagreement"), so the rule rests on the source and the sample is corroboration
// of one half of it only: all 123 interactive records are `entrypoint: "sdk-cli"`,
// i.e. SDK/`--print` launches.
//
// So the discriminator is "did cc tell us", and a kind gate would discard the one
// row where cc's answer is ground truth. The holder rule in LiveJobs keeps its
// kind filter, because there the kind IS the question cc answers.
//
// # Why an unrecognised value is `held` and not `idle`
//
// `held` is the only answer that is TRUE without knowing what the word means. A
// future cc adding a fifth status must make the row say "a process has this open,
// we cannot tell" — not "nothing is happening", which is a claim, and not "a turn
// is in flight", which is a different claim.
func ccStatusActivity(status string) string {
	switch status {
	case "busy":
		return SessionActivityWorking
	case "idle", "shell", "waiting":
		return SessionActivityIdle
	default:
		// Includes the empty string, which is the common case: cc did not write a
		// status for this record at all.
		return SessionActivityHeld
	}
}

// activityRank orders the states so the merge in States is one expression.
//
// working > held > idle > (absent). A rank rather than a chain of ifs at each
// call site, because the ordering is a JUDGEMENT (see States's doc for why `held`
// sits above `idle`) and a judgement written in two places is a judgement that
// will differ in one of them.
//
// The zero value is the empty string, i.e. "no state yet", which must rank below
// everything so that the first record for a sid always lands.
func activityRank(state string) int {
	switch state {
	case SessionActivityWorking:
		return 3
	case SessionActivityHeld:
		return 2
	case SessionActivityIdle:
		return 1
	default:
		return 0
	}
}

// LiveTurns reports, for every session this daemon currently has registered,
// whether a turn is in flight on it.
//
// Registered-and-not-in-a-turn is `false`, and it is NOT the same as absent: it is
// how a row gets SessionActivityIdle, which is a positive statement that tether is
// holding the conversation and nothing is running in it.
//
// A bool per sid rather than the count itself, because "how many" is nobody's
// business out here — only "any". Keeping the count inside the package is what lets
// Entry.turnsInFlight change shape (it already has, from a bool) without every
// consumer relearning its discipline.
//
// # It deliberately does not ask Alive()
//
// Every other consumer of this map goes through liveEntry, which calls
// agent.Session.Alive() and UNREGISTERS what it finds dead. This one must not: it
// is reached from an HTTP handler the browser polls every three seconds, and
// evicting sessions as a side effect of a status read is a mutation nothing asked
// for (IsLive's own doc flags the same side effect as something to know about
// before using it).
//
// What that costs is bounded, and in the safe direction. An Entry outlives its
// agent — the window liveEntry's doc describes — so a corpse can still appear
// here. But teardown resets the count before fanOut's deferred evict removes the
// entry, so a dead session reads `idle`, never `working`. Reporting a corpse as
// "tether is holding this, nothing running" for a few milliseconds is a far
// cheaper wrong answer than reporting a live one as finished.
//
// # Why the registry and not the entries themselves
//
// One RLock for the whole answer, and the copy is made inside it. Handing out
// *Entry values would make the caller responsible for reading a field whose
// concurrency story it cannot see; handing out a plain map of the one bit that
// matters keeps Entry.turnsInFlight's discipline inside this package.
func (r *Registry) LiveTurns() map[string]bool {
	out := make(map[string]bool)
	if r == nil {
		return out
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	for sid, e := range r.sessions {
		// A provider that mints its own id is registered under a key no client
		// holds until rekey moves it — for a fresh spawn that key is "" (see
		// Registry.sessions and spawnEntry). Reporting it would put a sid on the
		// wire that names no row, and the same rule SessionIndex.List applies to
		// directory names is the one to apply here: a sid this daemon would refuse
		// to serve has no business appearing in an answer keyed by sid.
		if !ValidSessionID(sid) {
			continue
		}
		out[sid] = e.turnsInFlight.Load() > 0
	}
	return out
}
