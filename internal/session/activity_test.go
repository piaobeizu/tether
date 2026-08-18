package session

// tether#103 — "is a turn in flight on this conversation right now?"
//
// Every fixture here builds its own registry directory, exactly as
// ccregistry_test.go does and for the same reason: nothing in this package may
// read the real ~/.claude, and CCRegistry takes its directory as an argument so
// that staying out of it is a property of the API rather than of anyone's care.
//
// Liveness is REAL, not injected — a "live" record carries os.Getpid() and this
// process's own /proc start token — so these exercise the same kernel parse the
// production path uses.

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/piaobeizu/tether/internal/agent"
)

// statusRecord is a live-registry record carrying an explicit status, which is
// the field this slice added to the reader.
//
// Built from bgRecord (ccregistry_test.go) rather than from a fresh literal so
// that the two files cannot drift about what a record looks like.
func statusRecord(pid int, sid, procStart, kind, status string) map[string]any {
	rec := bgRecord(pid, sid, procStart)
	rec["kind"] = kind
	if status == "" {
		delete(rec, "status")
	} else {
		rec["status"] = status
	}
	return rec
}

// TestActivity_TheFourRowCases is the whole feature, as a table.
//
// The four cases are the wi's, and three of them are ANTI-MISLABEL guards: only
// the first may report "a turn is in flight". That asymmetry is the point. A
// reader that answers `working` too eagerly does not merely lose information, it
// produces the lying list this feature exists to replace — and the failure is
// invisible, because a marker that is on when it should be off looks exactly like
// a marker that is working.
func TestActivity_TheFourRowCases(t *testing.T) {
	requireLinux(t)
	tok := liveToken(t)
	self := os.Getpid()

	cases := []struct {
		name string
		// setup writes the fixture and returns the sid to ask about.
		setup func(t *testing.T, f *ccRegFixture) string
		want  string // "" means the sid must be ABSENT from the map
	}{
		{
			// Case 1. No live record and no tether Entry: nothing holds this sid,
			// so it is NECESSARILY not running. Absence, not a state — see
			// ActivityIndex.States for why absence is the right encoding.
			name: "no live record at all",
			setup: func(t *testing.T, f *ccRegFixture) string {
				f.write(t, statusRecord(deadPid(t), "sid-nothing-alive", tok, "bg", "busy"))
				return "sid-nothing-alive"
			},
			want: "",
		},
		{
			// Case 2. The feature.
			name: "live record, status busy",
			setup: func(t *testing.T, f *ccRegFixture) string {
				f.write(t, statusRecord(self, "sid-busy-000000001", tok, "bg", "busy"))
				return "sid-busy-000000001"
			},
			want: SessionActivityWorking,
		},
		{
			// Case 3. Anti-mislabel. A process holds it and is NOT mid-turn; the
			// row must say so rather than borrowing the holder's presence as
			// evidence of activity.
			name: "live record, status idle",
			setup: func(t *testing.T, f *ccRegFixture) string {
				f.write(t, statusRecord(self, "sid-idle-000000001", tok, "bg", "idle"))
				return "sid-idle-000000001"
			},
			want: SessionActivityIdle,
		},
		{
			// Case 4. Anti-mislabel, and the honest one. A live record with no
			// status key — which is every --print launch, measured 123 of 123 on
			// the reference machine — cannot be classified either way, so it says
			// so instead of guessing.
			name: "live record with no status key",
			setup: func(t *testing.T, f *ccRegFixture) string {
				f.write(t, statusRecord(self, "sid-nostatus-00001", tok, "interactive", ""))
				return "sid-nostatus-00001"
			},
			want: SessionActivityHeld,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newCCRegFixture(t)
			sid := tc.setup(t, f)
			got := (&ActivityIndex{CCJobs: f.reg()}).States()
			if tc.want == "" {
				if state, ok := got[sid]; ok {
					t.Fatalf("States()[%q] = %q, want the sid to be ABSENT — nothing live holds it, so the row must claim nothing rather than claim a state", sid, state)
				}
				return
			}
			if got[sid] != tc.want {
				t.Fatalf("States()[%q] = %q, want %q", sid, got[sid], tc.want)
			}
			// Pinned as a literal, not as "not working" — a property assertion
			// here would be satisfied by any of the other two states (tether#102:
			// a real mutant survived exactly that shape of assertion).
			if tc.want != SessionActivityWorking && got[sid] == SessionActivityWorking {
				t.Fatalf("States()[%q] reported %q for a row that is not mid-turn", sid, SessionActivityWorking)
			}
		})
	}
}

// TestActivity_TheWholeStatusVocabularyIsClassified walks every value cc's own
// enum permits, plus the two shapes outside it.
//
// The vocabulary is EXHAUSTIVE and was read out of the installed binary
// (2.1.233) rather than inferred from what this machine happens to have:
//
//	XB_ = ["busy","shell","idle","waiting"];
//	function JB_(e){ return XB_.includes(e) ? e : void 0 }   // status:JB_(c.status)
//
// and the function that produces it says which one is a turn:
//
//	function k2h(e){ let t=aTw(e);
//	  if(t!==void 0) return {status:"waiting", waitingFor:t, working:!1};
//	  return {status: e.isLoading||e.delegatedActive ? "busy":"idle",
//	          waitingFor:void 0, working:e.isQueryActive} }
//	// downstream: zd = Fp === "idle" && Kg ? "shell" : Fp
//
// So `busy` is the ONLY value that means a turn is in flight: cc labels `waiting`
// working:false itself, and `shell` is an overlay applied only where the base was
// already `idle`. The wi body said the registry gives "only busy / idle", which
// would have made `busy` the complement of `idle` and silently swept `shell` and
// `waiting` into whichever branch the implementation happened to fall through to.
// `shell` is not hypothetical — one record on the reference machine carried it on
// 2026-08-18.
func TestActivity_TheWholeStatusVocabularyIsClassified(t *testing.T) {
	requireLinux(t)
	tok := liveToken(t)
	self := os.Getpid()

	for _, tc := range []struct {
		status string
		want   string
		why    string
	}{
		{"busy", SessionActivityWorking, "isLoading || delegatedActive — the turn is being processed"},
		{"idle", SessionActivityIdle, "between turns"},
		{"shell", SessionActivityIdle, "an overlay on an ALREADY-idle base (zd = Fp===\"idle\" && Kg)"},
		{"waiting", SessionActivityIdle, "cc returns working:!1 for it — blocked on the user, not mid-turn"},
		{"scheduled", SessionActivityHeld, "not in cc's enum: a value from a future build must degrade to \"cannot tell\", never to a claim"},
	} {
		t.Run(tc.status, func(t *testing.T) {
			f := newCCRegFixture(t)
			sid := "sid-" + tc.status + strings.Repeat("0", 12)
			f.write(t, statusRecord(self, sid, tok, "bg", tc.status))

			got := (&ActivityIndex{CCJobs: f.reg()}).States()[sid]
			if got != tc.want {
				t.Fatalf("status %q classified as %q, want %q — %s", tc.status, got, tc.want, tc.why)
			}
		})
	}
}

// TestActivity_ReadsStatusWhateverTheKindIs kills two mutants at once, and the
// second of them is the one this slice had to argue AGAINST inheriting.
//
// The obvious mutant is `kind == "bg"`: only interactive and bg appear on the
// reference machine, so it agrees with every record in existence there while
// missing "daemon" and "daemon-worker" (cc validates kind against all four —
// KB_ = ["interactive","bg","daemon","daemon-worker"]).
//
// The second is `kind != "interactive"`, which is the rule #101's HOLDER check
// uses and which is correct THERE — it is the condition cc itself refuses a
// resume on. It is wrong here, and both halves of why are measured:
//
//   - the status write is not gated on kind anywhere in the binary
//     (`useEffect(()=>{Cvn({status:zd,waitingFor:zl},Y)},[zd,zl,Y])`, inside the
//     shared App/REPL component);
//   - `--print` is not distinguished at the kind level either — kind comes from
//     `UBe() ?? "interactive"` where UBe reads CLAUDE_CODE_SESSION_KIND, which
//     only cc's own background-job spawners ever set. So tether's own spawns and
//     a real TTY session are the SAME kind, and only the TTY one writes a status.
//
// Hence the discriminator is "is there a readable status", not the kind. A
// kind-gated implementation would throw away the one row where cc's answer is
// ground truth: a live interactive session typed at by a human.
func TestActivity_ReadsStatusWhateverTheKindIs(t *testing.T) {
	requireLinux(t)
	tok := liveToken(t)
	self := os.Getpid()

	for _, kind := range []string{"bg", "daemon", "daemon-worker", "interactive"} {
		t.Run(kind, func(t *testing.T) {
			f := newCCRegFixture(t)
			sid := "sid-kind-" + kind
			f.write(t, statusRecord(self, sid, tok, kind, "busy"))

			if got := (&ActivityIndex{CCJobs: f.reg()}).States()[sid]; got != SessionActivityWorking {
				t.Fatalf("kind %q with status busy read as %q, want %q", kind, got, SessionActivityWorking)
			}
		})
	}
}

// TestActivity_HolderRuleIsUntouched is the other side of the previous test: the
// activity question dropping the kind gate must not drag #101's holder rule with
// it.
//
// They answer different questions and cc treats them differently — it refuses a
// resume on `kind && kind !== "interactive"` and says nothing about status there
// — so the same record has to read as "a turn is in flight" HERE and "not a
// holder" THERE. Asserted as a pair in one test, because the failure mode is a
// well-meant unification of the two.
func TestActivity_HolderRuleIsUntouched(t *testing.T) {
	requireLinux(t)
	f := newCCRegFixture(t)
	f.write(t, statusRecord(os.Getpid(), "sid-interactive-busy", liveToken(t), "interactive", "busy"))

	if got := (&ActivityIndex{CCJobs: f.reg()}).States()["sid-interactive-busy"]; got != SessionActivityWorking {
		t.Errorf("activity for a live interactive/busy record = %q, want %q", got, SessionActivityWorking)
	}
	if job, held := f.reg().LiveJob("sid-interactive-busy"); held {
		t.Errorf("LiveJob reported a live INTERACTIVE record as a holder (%+v); cc refuses only on kind != \"interactive\", and tether's own spawns are that kind — this would refuse every session tether started", job)
	}
}

// TestActivity_HeldOutranksIdleAcrossRecordsForOneSid.
//
// One sid accumulates many records (#101 measured 7 of 103 sids with more than
// one, worst case 26), so two live ones can disagree. The merge is a max, and the
// ranking puts "cannot tell" ABOVE "not running" on purpose: `idle` is a positive
// claim that nothing is happening, and papering an opaque holder over with it is
// the mislabel this whole slice is about. `working` still wins over both — a
// refusal to claim must not suppress a fact.
func TestActivity_HeldOutranksIdleAcrossRecordsForOneSid(t *testing.T) {
	requireLinux(t)
	tok := liveToken(t)
	self := os.Getpid()
	// Two live records for one sid needs two live pids. os.Getpid() is one; the
	// other is this process's parent, which is alive for as long as the test runs
	// under `go test`.
	other := os.Getppid()
	if other <= 1 {
		t.Skip("no usable second live pid: this process's parent is init or gone")
	}
	otherTok, ok := ccProcStartToken(other)
	if !ok {
		t.Skipf("could not read /proc/%d/stat for a second live pid", other)
	}

	t.Run("held wins over idle", func(t *testing.T) {
		f := newCCRegFixture(t)
		f.write(t, statusRecord(self, "sid-two-records-01", tok, "bg", "idle"))
		f.write(t, statusRecord(other, "sid-two-records-01", otherTok, "interactive", ""))

		if got := (&ActivityIndex{CCJobs: f.reg()}).States()["sid-two-records-01"]; got != SessionActivityHeld {
			t.Fatalf("one idle record + one opaque record = %q, want %q — reporting %q would be a positive claim that nothing is running, contradicted by the record we cannot read", got, SessionActivityHeld, SessionActivityIdle)
		}
	})

	t.Run("working wins over held", func(t *testing.T) {
		f := newCCRegFixture(t)
		f.write(t, statusRecord(self, "sid-two-records-02", tok, "bg", "busy"))
		f.write(t, statusRecord(other, "sid-two-records-02", otherTok, "interactive", ""))

		if got := (&ActivityIndex{CCJobs: f.reg()}).States()["sid-two-records-02"]; got != SessionActivityWorking {
			t.Fatalf("one busy record + one opaque record = %q, want %q — a refusal to claim must not suppress a fact", got, SessionActivityWorking)
		}
	})
}

// TestActivity_NoReaderAtAllIsAnEmptyMap — a daemon assembled without either
// source answers "{}" rather than panicking or claiming everything is idle. That
// is the same fail-open direction every optional store in this package takes.
func TestActivity_NoReaderAtAllIsAnEmptyMap(t *testing.T) {
	for _, tc := range []struct {
		name string
		idx  *ActivityIndex
	}{
		{"nil index", nil},
		{"zero index", &ActivityIndex{}},
		{"registry reader over an empty path", &ActivityIndex{CCJobs: NewCCRegistry("")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.idx.States()
			if got == nil {
				t.Fatal("States() returned a nil map; the handler encodes it directly and nil marshals to `null`, which the SPA cannot index")
			}
			if len(got) != 0 {
				t.Fatalf("States() = %v, want an empty map", got)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// The cross-language contract.
// ---------------------------------------------------------------------------

// sessionActivityTSPath is the hand-written TypeScript side of this endpoint's
// contract.
const sessionActivityTSPath = "../../web/src/lib/sessionActivity.ts"

// TestSessionActivityContractIsMirroredInTypeScript is this slice's equivalent of
// TestSessionSummaryIsMirroredInTypeScript, and it exists because THAT one does
// not cover this change.
//
// #101 closed the "a field added to SessionSummary and forgotten in wiSession.ts
// is a silent no-op" hole. This slice adds no field to SessionSummary — the state
// travels on its own endpoint (the pinned D3 decision), so the shape of the
// cross-language contract is different: an endpoint PATH and three state STRINGS,
// none of which any compiler on either side checks.
//
// The failure it guards is the measured one from #101 (mem_mlugObEv): a frontend
// rename that is internally consistent passes `tsc -b` with exit 0 and leaves
// every typed-fixture test green, because a typed fixture is immune to the NAME.
// So this reads both sides' source.
//
// Collected with go/ast rather than from a hand-kept list, so a fourth state
// added later is covered without anyone remembering this test exists — the same
// technique, for the same reason, as internal/wire/errors_test.go's
// terminalCodes exhaustiveness check.
func TestSessionActivityContractIsMirroredInTypeScript(t *testing.T) {
	src, err := os.ReadFile(sessionActivityTSPath)
	if err != nil {
		t.Fatalf("read the hand-written mirror at %s: %v", sessionActivityTSPath, err)
	}
	ts := string(src)

	consts := activityContractConsts(t)
	if len(consts) == 0 {
		t.Fatal("found zero SessionActivity* consts while parsing this package — the collecting half of this guard is broken, so it would pass against anything")
	}
	// The three states plus the path. A count check, not just a loop, because the
	// loop alone would keep passing if the AST walk silently started matching
	// fewer declarations.
	if len(consts) < 4 {
		t.Fatalf("collected %d SessionActivity* consts (%v); expected at least the three states and the endpoint path", len(consts), consts)
	}

	for name, val := range consts {
		if !tsHasStringLiteral(ts, val) {
			t.Errorf("%s = %q is part of the wire contract but that literal does not appear in %s; the daemon would speak a word the SPA never matches, and nothing else in either language fails", name, val, sessionActivityTSPath)
		}
	}
}

// TestSessionActivityMirrorGuardRejectsACommentOnlyMention is the guard on the
// guard.
//
// A test that reads a file and asserts something about it can pass because its
// matching found the string somewhere harmless, and then it is worse than no
// test. The dangerous near-miss here is a state name that survives only in a doc
// comment after the code stopped using it — which is exactly what a rename
// leaves behind. So the matcher requires a QUOTED literal, and that requirement
// is exercised against inputs whose answer is known.
func TestSessionActivityMirrorGuardRejectsACommentOnlyMention(t *testing.T) {
	const sample = `
/** The state used to be called 'stirring' — see tether#103. */
export const SESSION_ACTIVITY_WORKING = 'working'
export const SESSION_ACTIVITY_PATH = "/api/v1/session-activity"
// export const SESSION_ACTIVITY_DEAD = 'dead'
`
	for _, tc := range []struct {
		lit  string
		want bool
		why  string
	}{
		{"working", true, "a quoted single-quote literal in code"},
		{"/api/v1/session-activity", true, "a quoted double-quote literal in code"},
		{"stirring", false, "named only inside a doc comment — a rename's leftovers must not satisfy the guard"},
		{"tether#103", false, "prose in a comment, unquoted"},
	} {
		if got := tsHasStringLiteral(sample, tc.lit); got != tc.want {
			t.Errorf("tsHasStringLiteral(sample, %q) = %v, want %v — %s", tc.lit, got, tc.want, tc.why)
		}
	}
}

// activityContractConsts collects every `SessionActivity*` string const this
// package declares, by parsing its own source.
func activityContractConsts(t *testing.T) map[string]string {
	t.Helper()
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, ".", nil, 0)
	if err != nil {
		t.Fatalf("parse the session package source: %v", err)
	}
	out := map[string]string{}
	for _, pkg := range pkgs {
		for _, file := range pkg.Files {
			for _, decl := range file.Decls {
				gd, ok := decl.(*ast.GenDecl)
				if !ok || gd.Tok != token.CONST {
					continue
				}
				for _, spec := range gd.Specs {
					vs, ok := spec.(*ast.ValueSpec)
					if !ok {
						continue
					}
					for i, name := range vs.Names {
						if !strings.HasPrefix(name.Name, "SessionActivity") {
							continue
						}
						if i >= len(vs.Values) {
							t.Fatalf("%s has no explicit value; this guard assumes every SessionActivity* const is `Name = \"literal\"`", name.Name)
						}
						lit, ok := vs.Values[i].(*ast.BasicLit)
						if !ok || lit.Kind != token.STRING {
							t.Fatalf("%s is not a string literal; its value cannot be checked against the TypeScript side statically", name.Name)
						}
						val, err := strconv.Unquote(lit.Value)
						if err != nil {
							t.Fatalf("unquote %s's value %s: %v", name.Name, lit.Value, err)
						}
						out[name.Name] = val
					}
				}
			}
		}
	}
	return out
}

// tsHasStringLiteral reports whether src contains lit as a QUOTED string, in
// either quote style, outside a comment.
//
// Quoting is the whole of the check: an unquoted mention is prose, and prose is
// what a rename leaves behind in the doc comment above the constant it renamed.
// Comment stripping is line-based and deliberately simple — it only has to be
// right about the shape this file uses (`//`, and `/** … */` blocks whose inner
// lines start with `*`), and being over-eager would only ever make the guard
// stricter.
func tsHasStringLiteral(src, lit string) bool {
	for _, line := range strings.Split(src, "\n") {
		trimmed := strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(trimmed, "//"),
			strings.HasPrefix(trimmed, "*"),
			strings.HasPrefix(trimmed, "/*"):
			continue
		}
		if strings.Contains(line, `'`+lit+`'`) || strings.Contains(line, `"`+lit+`"`) {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// tether's own sessions — the turn-in-flight flag (D4).
// ---------------------------------------------------------------------------

// TestEveryPromptDeliveryGoesThroughTheEntryWrapper is the structural guard on
// the seam this slice exists to close, and it is the one that makes the seam a
// property of the package rather than of six people's memory.
//
// A turn starts when a prompt is written to the agent. Verified on e3eda21, that
// happens at SIX call sites and only one of them is the obvious one:
//
//	attach.go:523    e.Session().SendPrompt(ctx, text)          Attachment.SendPrompt
//	attach.go:715    cur.Session().SendPrompt(ctx, text)        reopen -> a replacement already there
//	attach.go:796    sibling.Session().SendPrompt(ctx, text)     reopen -> a sibling registration
//	attach.go:986    fresh.Session().SendPrompt(ctx, text)       reopen -> the respawn
//	attach.go:1338   fresh.Session().SendPrompt(ctx, text)       resolve -> the fallback spawn
//	registry.go:1673 e.sess.SendPrompt(ctx, string(b))           DeliverAction
//
// Marking the turn at the first one only would leave four RECOVERY paths — the
// paths a user reaches precisely when things are going wrong — with the flag in
// the wrong state. All six deliver through an *Entry, so the fix is one method on
// *Entry and six calls to it, and this test is what keeps it that way: a seventh
// delivery path added anywhere in this package fails here instead of quietly
// shipping a session whose marker never lights.
//
// go/ast rather than grep, and the enclosing FUNCTION rather than the file, so
// the assertion is about the program and not about where the lines happen to sit.
// The technique is this repo's own — internal/wire/errors_test.go parses its
// package to keep terminalCodes exhaustive, for the same reason: an invariant no
// compiler enforces has to be enforced by a test that reads the source.
//
// It fails on ZERO matches. A source-reading test that finds nothing is worse
// than no test, because it reports green while the thing it guards is gone
// (mem_mlugObEv rule 3).
func TestEveryPromptDeliveryGoesThroughTheEntryWrapper(t *testing.T) {
	const wrapper = "sendPrompt" // (*Entry).sendPrompt — the one permitted caller

	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, ".", func(fi os.FileInfo) bool {
		return !strings.HasSuffix(fi.Name(), "_test.go")
	}, 0)
	if err != nil {
		t.Fatalf("parse the session package source: %v", err)
	}

	type site struct {
		pos      string
		enclosed string
	}
	var sites []site
	for _, pkg := range pkgs {
		for _, file := range pkg.Files {
			for _, decl := range file.Decls {
				fn, ok := decl.(*ast.FuncDecl)
				if !ok {
					continue
				}
				name := fn.Name.Name
				ast.Inspect(fn.Body, func(n ast.Node) bool {
					call, ok := n.(*ast.CallExpr)
					if !ok {
						return true
					}
					sel, ok := call.Fun.(*ast.SelectorExpr)
					if !ok || sel.Sel.Name != "SendPrompt" {
						return true
					}
					sites = append(sites, site{pos: fset.Position(call.Pos()).String(), enclosed: name})
					return true
				})
			}
		}
	}

	if len(sites) == 0 {
		t.Fatal("found zero SendPrompt call sites while parsing the session package — the AST half of this guard is not seeing the source, so it would pass against anything")
	}

	for _, s := range sites {
		if s.enclosed != wrapper {
			t.Errorf("%s: SendPrompt is called from %s, but the only function allowed to call it is (*Entry).%s.\n"+
				"Every prompt write is the start of a turn, and the session-activity marker is set there. A delivery path that bypasses the wrapper ships a session whose row never lights up — and the four reopen/resolve paths are exactly the ones a user reaches when something has already gone wrong.",
				s.pos, s.enclosed, wrapper)
		}
	}
}

// TestEntryTurnFlag_SetOnDeliveryAndClearedOnResult drives the ordinary turn all
// the way round, through the real registry and the real fanOut.
func TestEntryTurnFlag_SetOnDeliveryAndClearedOnResult(t *testing.T) {
	fs := &fakeSession{sid: "sid-turn-roundtrip", events: make(chan agent.Event, 8)}
	reg := NewRegistry(&fakeProvider{sess: fs})
	e, err := reg.GetOrSpawnEntry(context.Background(), "sid-turn-roundtrip", "fake")
	if err != nil {
		t.Fatalf("GetOrSpawnEntry: %v", err)
	}
	// The fake models a provider that MINTS its own id, so the entry is keyed
	// under a placeholder until its init announces the real sid and rekey moves
	// it. LiveTurns is keyed by that registration, so the announcement has to
	// happen before it can be asked about this sid at all. (Real cc adopts the
	// pinned id and needs no equivalent — see fakeSession.announceInit.)
	fs.announceInit()
	waitForRegistered(t, reg, "sid-turn-roundtrip")

	if inFlight, known := reg.LiveTurns()["sid-turn-roundtrip"]; !known {
		t.Fatal("a registered session is absent from LiveTurns; absence means \"nothing holds this sid\", which would make the row claim it is definitely not running")
	} else if inFlight {
		t.Fatal("a session with no prompt sent reported a turn in flight")
	}

	if err := e.sendPrompt(context.Background(), "go"); err != nil {
		t.Fatalf("sendPrompt: %v", err)
	}
	if got := reg.LiveTurns()["sid-turn-roundtrip"]; !got {
		t.Fatal("no turn in flight straight after a delivered prompt; the flag has to be set BEFORE the write, because the result can arrive on the fanOut goroutine before SendPrompt has even returned")
	}

	fs.events <- agent.Event{Kind: agent.EventResult, Text: "stop"}

	waitForTurnFlag(t, reg, "sid-turn-roundtrip", false,
		"the turn-end result did not clear the flag")
}

// TestEntryTurnFlag_InitlessEmptyResultDoesNotClearIt is the exception, and it is
// load-bearing rather than a curiosity.
//
// A `result` with no preceding system/init carrying no text is not a turn ending —
// it is the artefact of a FAILED `--resume` (registry.go's EventResult branch,
// mem_2ruSlrHR ③): cc exits 1 having printed exactly one line and never emitted
// init. fanOut already refuses to forward it, for a reason stated in terms of what
// the user sees: it would close a turn the user never started.
//
// The same argument applies one layer down. The prompt that triggered that failed
// resume is STILL IN FLIGHT — Attachment.resolve is about to respawn and answer it
// for real — so clearing the marker here would blink it out for a turn that has
// not happened yet. The frontend store draws the same distinction at the same
// point, which is why the two must agree.
func TestEntryTurnFlag_InitlessEmptyResultDoesNotClearIt(t *testing.T) {
	fs := &fakeSession{sid: "sid-failed-resume", events: make(chan agent.Event, 8)}
	reg := NewRegistry(&fakeProvider{sess: fs})
	e, err := reg.GetOrSpawnEntry(context.Background(), "sid-failed-resume", "fake")
	if err != nil {
		t.Fatalf("GetOrSpawnEntry: %v", err)
	}
	if err := e.sendPrompt(context.Background(), "go"); err != nil {
		t.Fatalf("sendPrompt: %v", err)
	}

	// The single line a failed resume produces. No init, no text.
	fs.events <- agent.Event{Kind: agent.EventResult, Text: ""}

	// Give fanOut time to have processed it and (wrongly) cleared. A negative
	// assertion needs a settling window or it passes by being early.
	drainOneEvent(t, fs)

	if !e.turnInFlight.Load() {
		t.Fatal("an init-less empty result cleared the turn flag. That envelope is a failed --resume's artefact, not a turn end: the prompt is still in flight and Attachment.resolve is about to respawn and answer it. Clearing here extinguishes the marker for a turn that has not started.")
	}
}

// TestEntryTurnFlag_ClearedOnTeardown — a session that dies MID-TURN must not
// leave a row reading "working" forever.
//
// This is not belt-and-braces over the eviction. An Entry OUTLIVES its agent:
// liveEntry's doc spells out the window ("between 'cc exited' and 'the map forgot
// it' the entry is still there"), and a reader over Registry.sessions sees the
// entry throughout it. Worse, with a process that is hung rather than exited
// Events() never closes at all, so the window is unbounded. teardown is the one
// place that runs exactly once per session, on the session's own goroutine, after
// the stream has closed and drained.
func TestEntryTurnFlag_ClearedOnTeardown(t *testing.T) {
	fs := &fakeSession{sid: "sid-dies-mid-turn", events: make(chan agent.Event, 8)}
	reg := NewRegistry(&fakeProvider{sess: fs})
	e, err := reg.GetOrSpawnEntry(context.Background(), "sid-dies-mid-turn", "fake")
	if err != nil {
		t.Fatalf("GetOrSpawnEntry: %v", err)
	}
	fs.announceInit()
	waitForRegistered(t, reg, "sid-dies-mid-turn")
	if err := e.sendPrompt(context.Background(), "go"); err != nil {
		t.Fatalf("sendPrompt: %v", err)
	}

	// The process dies mid-answer: the stream ends with no terminal result.
	close(fs.events)
	waitForCount(t, reg, 0)

	if e.turnInFlight.Load() {
		t.Fatal("a session that died mid-turn kept its turn flag set; while the entry is still registered — and for a HUNG process that is until the daemon restarts — its row reads \"working\" forever")
	}
}

// TestEntryTurnFlag_ClearedWhenTheWriteFails.
//
// A refused write means the prompt never reached the agent, so no EventResult
// will ever arrive to close the turn. Without this clear, a terminal delivery
// failure — the reopen budget spent, or a respawn that also failed — leaves the
// row stuck on "working": the frozen marker this whole slice exists to prevent,
// in miniature and on the one path where the user is already having a bad time.
func TestEntryTurnFlag_ClearedWhenTheWriteFails(t *testing.T) {
	fs := &fakeSession{sid: "sid-write-refused", events: make(chan agent.Event, 8)}
	reg := NewRegistry(&fakeProvider{sess: fs})
	e, err := reg.GetOrSpawnEntry(context.Background(), "sid-write-refused", "fake")
	if err != nil {
		t.Fatalf("GetOrSpawnEntry: %v", err)
	}
	fs.sendFails.Store(true)

	if err := e.sendPrompt(context.Background(), "go"); err == nil {
		t.Fatal("sendPrompt reported success against a session whose write fails")
	}
	if e.turnInFlight.Load() {
		t.Fatal("a REFUSED write left the turn flag set; nothing will ever clear it, because no result is coming for a prompt the agent never received")
	}
}

// TestActivity_TetherOwnRegistrationOverridesAnOpaqueCCRecord.
//
// Both sources describe the SAME process here, and that is the normal case rather
// than a corner: every cc tether spawns is a `--print` launch, which registers as
// kind "interactive" (kind comes from `UBe() ?? "interactive"`, and UBe only reads
// CLAUDE_CODE_SESSION_KIND, which nothing but cc's own background-job spawners
// sets) and writes NO status field — measured 123 of 137 records on the reference
// machine, 123 of 123 of that shape, 2026-08-18.
//
// So cc's registry contributes "cannot tell" for exactly the sessions tether knows
// the most about. If that were merged as a plain max it would outrank tether's own
// "idle" and every tether session would render as a shrug — the feature would be
// blank for the majority of the list. Hence the precedence, and hence this test.
func TestActivity_TetherOwnRegistrationOverridesAnOpaqueCCRecord(t *testing.T) {
	requireLinux(t)
	const sid = "sid-tether-own-0001"
	f := newCCRegFixture(t)
	// The record tether's own spawn writes: live, interactive, no status.
	f.write(t, statusRecord(os.Getpid(), sid, liveToken(t), "interactive", ""))

	fs := &fakeSession{sid: sid, events: make(chan agent.Event, 8)}
	reg := NewRegistry(&fakeProvider{sess: fs})
	e, err := reg.GetOrSpawnEntry(context.Background(), sid, "fake")
	if err != nil {
		t.Fatalf("GetOrSpawnEntry: %v", err)
	}
	fs.announceInit()
	waitForRegistered(t, reg, sid)
	idx := &ActivityIndex{Reg: reg, CCJobs: f.reg()}

	// Registered, no prompt sent: tether KNOWS there is no turn in flight, and
	// says so instead of deferring to the record it cannot read.
	if got := idx.States()[sid]; got != SessionActivityIdle {
		t.Fatalf("with a registered entry and an opaque cc record, States()[%q] = %q, want %q — tether's own bookkeeping is the better answer for a process tether is driving", sid, got, SessionActivityIdle)
	}

	if err := e.sendPrompt(context.Background(), "go"); err != nil {
		t.Fatalf("sendPrompt: %v", err)
	}
	if got := idx.States()[sid]; got != SessionActivityWorking {
		t.Fatalf("after a delivered prompt, States()[%q] = %q, want %q", sid, got, SessionActivityWorking)
	}
}

// TestEntryTurnFlag_ConcurrentSetAndReadIsRaceFree.
//
// The new field is the first mutable state on Entry without a named mutex, so its
// concurrency story has to be exercised rather than asserted in a comment. The
// writers really are separate goroutines in production — the prompt reader
// (serveChat), fanOut's event loop, and teardown — and the reader is an HTTP
// handler on a fourth. Needs -race to fail, which is this repository's baseline
// for ./... (the same arrangement TestEntryOwner_ReadIsGuardedAgainstAConcurrentClaim
// relies on).
func TestEntryTurnFlag_ConcurrentSetAndReadIsRaceFree(t *testing.T) {
	fs := &fakeSession{sid: "sid-race-flag-0001", events: make(chan agent.Event, 64)}
	reg := NewRegistry(&fakeProvider{sess: fs})
	e, err := reg.GetOrSpawnEntry(context.Background(), "sid-race-flag-0001", "fake")
	if err != nil {
		t.Fatalf("GetOrSpawnEntry: %v", err)
	}

	var wg sync.WaitGroup
	stop := make(chan struct{})
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
				_ = reg.LiveTurns()
			}
		}
	}()
	for i := 0; i < 200; i++ {
		if err := e.sendPrompt(context.Background(), "go"); err != nil {
			t.Fatalf("sendPrompt: %v", err)
		}
		e.clearTurn()
	}
	close(stop)
	wg.Wait()
}

// waitForTurnFlag polls the registry's answer for one sid. Bounded.
func waitForTurnFlag(t *testing.T, reg *Registry, sid string, want bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if reg.LiveTurns()[sid] == want {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("%s: LiveTurns()[%q] never became %v", msg, sid, want)
}

// drainOneEvent waits until the fake's event channel has been consumed, which is
// the observable proof that fanOut has PROCESSED what was put on it.
//
// It exists so the negative assertion above ("the flag is still set") cannot pass
// by running before fanOut got there. A sleep would do the same job less
// reliably; a channel-length check is the actual happens-after signal available
// here.
func drainOneEvent(t *testing.T, fs *fakeSession) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if len(fs.events) == 0 {
			// Consumed. One more scheduling slice so the branch that follows the
			// receive has run too.
			time.Sleep(20 * time.Millisecond)
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("fanOut never consumed the event; the test cannot tell what it decided")
}
