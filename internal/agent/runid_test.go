package agent

import (
	"bytes"
	"context"
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestEveryEventLiteralCarriesARunID is the guard the Event.RunID contract chose
// instead of making an untagged event impossible to construct.
//
// # Why a test and not the type system
//
// Zero is a LEGAL RunID and has to be — it is how a refused prompt says "no run
// produced me" (opencodeSession.SendPrompt's busy branch) and how a provider that
// cannot attribute at all is handled. So an emit site that forgets to stamp
// compiles, runs, and silently gets tether#103's double-count back for that one
// path: Registry.fanOut applies both of a run's end-of-turn signals, the second
// one takes a later delivery's turn, and the session reports `idle` for the whole
// of a turn the user is waiting on. Nothing else in this repository would notice —
// which is the shape of defect that needs a mechanical guard rather than care.
//
// Forbidding the zero value would mean no Event could be written as a composite
// literal, and composite literals are how all of this package's production emit
// sites and every fixture in internal/session build them. This is the same trade
// (*Entry).sendPrompt's own AST guard makes, and its doc's qualifier applies here
// too: the reach is exactly what it parses.
//
// # What it does NOT cover, so the name is not read as a universal
//
// It matches COMPOSITE LITERALS in this package's non-test files. An Event built
// by any other route — a variable filled field by field, a value forwarded from
// another package, a literal in a _test.go fixture — is invisible to it. cc's
// readLoop emitting `s.emit(ev)` over parseLine's slice is exactly that shape, and
// it is covered because the literals INSIDE parseLine are what this reads.
func TestEveryEventLiteralCarriesARunID(t *testing.T) {
	fset := token.NewFileSet()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}

	checked := 0
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, filepath.Join(".", name), nil, parser.ParseComments)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		ast.Inspect(file, func(n ast.Node) bool {
			lit, ok := n.(*ast.CompositeLit)
			if !ok {
				return true
			}
			for _, target := range eventLiterals(lit) {
				checked++
				kind, hasRun := eventLitFields(target)
				if hasRun {
					continue
				}
				t.Errorf("%s: Event{Kind: %s} literal has no RunID. "+
					"Every event a provider emits must name the run that produced it, and a turn-closer "+
					"without one is applied unconditionally by Registry.fanOut — so a run that reports twice "+
					"takes a later delivery's turn and the session reports idle for the whole of it "+
					"(tether#103/#145/#148). If the answer really is \"no run\", say so with an explicit "+
					"RunID: 0 and a comment, the way the busy rejection does.",
					fset.Position(target.Lbrace), kind)
			}
			return true
		})
	}

	// Not vacuous: a parse that silently matched nothing would pass everything
	// above. 20 is the count at the time of writing (11 in opencode_provider.go, 9
	// in claude_provider.go); the assertion is a floor, so adding emit sites is
	// fine and deleting most of them is what it catches.
	if checked < 20 {
		t.Fatalf("the guard examined only %d Event literals; it is not reading this package's emit sites and would pass anything", checked)
	}
}

// eventLiterals returns the Event composite literals lit itself represents:
// either lit (an `Event{…}`) or lit's elements (the elided inner literals of an
// `[]Event{{…}, {…}}`, whose own Type is nil — invisible to a naive walk).
func eventLiterals(lit *ast.CompositeLit) []*ast.CompositeLit {
	switch typ := lit.Type.(type) {
	case *ast.Ident:
		if typ.Name == "Event" {
			return []*ast.CompositeLit{lit}
		}
	case *ast.ArrayType:
		if id, ok := typ.Elt.(*ast.Ident); ok && id.Name == "Event" {
			var out []*ast.CompositeLit
			for _, el := range lit.Elts {
				// Only the ELIDED elements (`[]Event{{…}}`), whose own Type is nil. An
				// element written out as `[]Event{Event{…}}` is reached by ast.Inspect
				// on its own, and counting it here too would inflate the not-vacuous
				// floor below with a literal it has already seen.
				if inner, ok := el.(*ast.CompositeLit); ok && inner.Type == nil {
					out = append(out, inner)
				}
			}
			return out
		}
	}
	return nil
}

// eventLitFields reports the literal's Kind (as written) and whether it sets
// RunID at all. A Kind of "" means the literal named no kind.
func eventLitFields(lit *ast.CompositeLit) (kind string, hasRun bool) {
	for _, el := range lit.Elts {
		kv, ok := el.(*ast.KeyValueExpr)
		if !ok {
			continue
		}
		key, ok := kv.Key.(*ast.Ident)
		if !ok {
			continue
		}
		switch key.Name {
		case "Kind":
			if id, ok := kv.Value.(*ast.Ident); ok {
				kind = id.Name
			}
		case "RunID":
			hasRun = true
		}
	}
	return kind, hasRun
}

// TestOpenCodeStampsEveryEventWithARun is the guard for the mutation
// TestEveryEventLiteralCarriesARunID cannot see: an emit site that HAS a RunID and
// has the WRONG one.
//
// A key-presence check catches "somebody added an emit site and forgot". It does
// not catch "somebody stamped the terminal result with 0", or with the session's
// current-run snapshot instead of this run's own id — and either of those puts a
// run's two end-of-turn signals under DIFFERENT identities, which is precisely the
// state fanOut cannot deduplicate. The whole fix would be gone for that path and
// nothing else in this repository would notice, because internal/session's fixtures
// hand-build their events and never ask this provider what it emits.
//
// # The invariant, in three parts
//
// Every Event literal in this file stamps RunID with exactly one of:
//
//   - `runID`, inside SendPrompt — the id minted for THIS accepted delivery. All six
//     of the run's own emit sites (resume-serve failure, stdout pipe, Start, scan,
//     non-zero exit, terminal result) must agree, because sharing an id is what makes
//     "one run settles one turn" mean anything.
//   - the literal `0`, exactly ONCE, in SendPrompt's busy branch — a prompt refused
//     before any run existed. See TestOpenCodeBusyRejectionCarriesNoRunID for why
//     that has to be zero rather than merely happens to be.
//   - `s.runSeq.Load()`, outside SendPrompt — the current-run snapshot, for the two
//     emitters that have no run of their own: watchServeExit's obituary and sseLoop's
//     frames. A `runID` there would not compile; a fresh Add(1) would, and would
//     silently give a stale signal an identity no other signal shares.
//
// # Structural rather than behavioural, and why that is what is affordable
//
// Driving the run goroutine's error-then-result pair for real needs an `opencode`
// binary answering an HTTP health check and an SSE stream, which this package
// deliberately does not require (the one test that does is skipped without the
// binary, so it never runs in CI). readSSE and the busy branch DO have behavioural
// tests below, because both are reachable without a subprocess; the run goroutine
// and watchServeExit have only this. Narrower than a behavioural pin, and not
// offered as equivalent.
func TestOpenCodeStampsEveryEventWithARun(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "opencode_provider.go", nil, 0)
	if err != nil {
		t.Fatalf("parse opencode_provider.go: %v", err)
	}

	// Byte ranges of SendPrompt, so each literal can be attributed to "inside the
	// accepted delivery" or "outside it" without a second walk.
	var lo, hi token.Pos
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if ok && fn.Name.Name == "SendPrompt" && fn.Recv != nil {
			lo, hi = fn.Body.Lbrace, fn.Body.Rbrace
			break
		}
	}
	if lo == token.NoPos {
		t.Fatal("no SendPrompt method found in opencode_provider.go; this guard is reading the wrong thing")
	}

	var inRun, zeros, snapshots int
	ast.Inspect(file, func(n ast.Node) bool {
		lit, ok := n.(*ast.CompositeLit)
		if !ok {
			return true
		}
		for _, target := range eventLiterals(lit) {
			pos := fset.Position(target.Lbrace)
			run := eventLitRunSource(fset, target)
			inside := target.Lbrace > lo && target.Lbrace < hi
			switch {
			case run == "":
				t.Errorf("%s: Event literal has no RunID at all", pos)
			case inside && run == "runID":
				inRun++
			case inside && run == "0":
				zeros++
			case !inside && run == "s.runSeq.Load()":
				snapshots++
			case inside:
				t.Errorf("%s: RunID is %s inside SendPrompt; want the accepted delivery's own `runID` "+
					"(or the literal 0 for the two paths that never start a run). An id that is not this run's puts the run's error "+
					"and its terminal result under different identities, and fanOut deduplicates by identity — the "+
					"doubled end-of-turn would count two turns down again (tether#145/#148)", pos, run)
			default:
				t.Errorf("%s: RunID is %s outside SendPrompt; want s.runSeq.Load(). This emitter has no run of "+
					"its own, so minting one would give a stale or session-level signal an identity nothing else "+
					"shares, and fanOut would apply it against whatever delivery is outstanding", pos, run)
			}
		}
		return true
	})

	// Floors, not equalities, so adding an emit site is not a test edit — but low
	// enough numbers that DELETING the stamping is. Counts at the time of writing:
	// 5 in the run, 2 zeros, 4 snapshots (watchServeExit + three readSSE frames).
	if inRun < 5 {
		t.Errorf("only %d Event literals inside SendPrompt carry `runID`, want at least 5", inRun)
	}
	if zeros != 2 {
		t.Errorf("SendPrompt has %d Event literals stamped RunID: 0, want exactly 2 — the busy rejection and the "+
			"failed serve relaunch, the two paths on which no run is ever started. A THIRD zero means a real run's "+
			"signal is emitted as if no run produced it, so fanOut applies it unconditionally and the run's other "+
			"signal is applied too; a SECOND one going missing means one of those two paths has been given an id, "+
			"which is worse — both release `busy` before they emit, so an id there is the one way to get "+
			"turn-closers onto the channel in DECREASING run order, and fanOut refuses those permanently", zeros)
	}
	if snapshots < 4 {
		t.Errorf("only %d Event literals outside SendPrompt carry s.runSeq.Load(), want at least 4", snapshots)
	}
}

// eventLitRunSource returns the source text of the expression a literal assigns to
// RunID, or "" if it assigns none.
func eventLitRunSource(fset *token.FileSet, lit *ast.CompositeLit) string {
	for _, el := range lit.Elts {
		kv, ok := el.(*ast.KeyValueExpr)
		if !ok {
			continue
		}
		key, ok := kv.Key.(*ast.Ident)
		if !ok || key.Name != "RunID" {
			continue
		}
		var buf bytes.Buffer
		if err := printer.Fprint(&buf, fset, kv.Value); err != nil {
			return "<unprintable>"
		}
		return buf.String()
	}
	return ""
}

// TestOpenCodeBusyRejectionCarriesNoRunID pins the one deliberate zero in this
// package, and it is load-bearing rather than a shrug.
//
// A prompt the `busy` CAS refuses never became a run, so its EventError belongs to
// none — and it must NOT borrow one. Run ids are strictly increasing over accepted
// runs and Registry.fanOut keeps a high-water mark; a refusal minting the next id
// would put a HIGHER number on a signal that precedes the live run's own terminal
// result, and that result would then be refused as "already settled". The count
// would sit one high on a session opencode keeps alive forever after — which is
// the frozen "working" marker tether#103's error arm exists to prevent, arrived at
// from a new direction.
//
// TestEntryTurnFlag_ARefusedDeliverysErrorDoesNotMuteALiveRunsResult, over in
// internal/session, is the consumer half of this: it drives the same pair of
// events and asserts both turns are counted down.
func TestOpenCodeBusyRejectionCarriesNoRunID(t *testing.T) {
	s := &opencodeSession{
		spawnCtx: context.Background(),
		events:   make(chan Event, 8),
		sidCh:    make(chan struct{}),
	}
	// A run is already in flight — the state the CAS below loses to.
	s.busy.Store(true)

	if err := s.SendPrompt(context.Background(), "second prompt"); err != nil {
		t.Fatalf("SendPrompt on a busy session = %v, want nil (it reports by event)", err)
	}

	var got Event
	select {
	case got = <-s.events:
	default:
		t.Fatal("the busy rejection emitted nothing, so there is no end-of-turn for the delivery Entry.sendPrompt already counted up")
	}
	if got.Kind != EventError {
		t.Fatalf("busy rejection Kind = %q, want %q", got.Kind, EventError)
	}
	if got.RunID != 0 {
		t.Fatalf("busy rejection RunID = %d, want 0. A refused prompt never ran, and an id here would sit ABOVE the live run's — fanOut's high-water mark would then mute that run's own terminal result and leave the count one high for the life of the session", got.RunID)
	}
	if seq := s.runSeq.Load(); seq != 0 {
		t.Fatalf("runSeq = %d after a REFUSED prompt, want 0: minting an id for a prompt that never ran also advances the sequence the live run's successor will draw from", seq)
	}
}

// TestOpenCodeResumeServeFailureCarriesNoRunID pins the second deliberate zero, and
// it is the one that is easy to get wrong because this delivery WAS accepted.
//
// It won the `busy` CAS, Entry.sendPrompt has counted it up, and its EventError is
// the only end-of-turn it will ever get — so it looks exactly like a delivery that
// deserves a run id. It must not have one, because it is the single path in this
// provider that releases `busy` BEFORE emitting: another SendPrompt can win the CAS
// while this error is still in flight, mint the NEXT id, and reach fanOut first.
// fanOut keeps a high-water mark and refuses anything at or below it, so an id here
// is the one way to get a run's only signal permanently refused — on a path that
// deliberately keeps the session alive and dormant, where teardown never runs to
// reset the count. The row would read "working" until the daemon restarted.
//
// The failure is driven, not faked: with no `opencode` on PATH, startServe's
// serve.Start() returns exec.ErrNotFound before it launches anything, which is the
// same early return a stolen port or a missing binary produces in production.
func TestOpenCodeResumeServeFailureCarriesNoRunID(t *testing.T) {
	// An empty directory as the whole PATH, so exec resolves nothing. Set before
	// SendPrompt, because exec.CommandContext does the lookup at construction time.
	t.Setenv("PATH", t.TempDir())

	s := &opencodeSession{
		workdir:  t.TempDir(),
		spawnCtx: context.Background(),
		events:   make(chan Event, 8),
		sidCh:    make(chan struct{}),
	}
	// A prior Interrupt() hibernated the serve. This is what sends SendPrompt down
	// the relaunch path.
	s.dormant = true

	if err := s.SendPrompt(context.Background(), "first"); err != nil {
		t.Fatalf("SendPrompt = %v, want nil (a failed relaunch reports by event)", err)
	}

	var got Event
	select {
	case got = <-s.events:
	default:
		t.Fatal("the failed relaunch emitted nothing, so the delivery Entry.sendPrompt counted up has no end-of-turn at all")
	}
	if got.Kind != EventError {
		t.Fatalf("Kind = %q, want %q", got.Kind, EventError)
	}
	if got.Err == nil || !strings.Contains(got.Err.Error(), "opencode resume serve") {
		t.Fatalf("Err = %v, want the resume-serve failure — this test may be reaching a different emit site", got.Err)
	}
	if got.RunID != 0 {
		t.Fatalf("failed-relaunch error RunID = %d, want 0. This path releases `busy` before it emits, so a "+
			"concurrent SendPrompt can mint a HIGHER id and reach fanOut first — and fanOut refuses anything at or "+
			"below its high-water mark, so this run's only signal would be discarded and the count would sit one "+
			"high for the rest of a session opencode keeps alive", got.RunID)
	}
	if seq := s.runSeq.Load(); seq != 0 {
		t.Fatalf("runSeq = %d after a delivery that never started a run, want 0: the mint has to sit after the "+
			"last statement that can release `busy` without emitting first, or the ordering the high-water mark "+
			"depends on is not constructed at all", seq)
	}
	if s.busy.Load() {
		t.Fatal("`busy` was left set after a failed relaunch; every later prompt on this session would be refused")
	}
}

// TestOpenCodeSSEErrorCarriesTheCurrentRun covers the gap tether#145's guard could
// not even see: a LATE EventError.
//
// The SSE `session.error` frame is emitted on sseLoop's goroutine, which has no
// ordering relation to the run goroutine's terminal EventResult. So an error about
// a run can be applied AFTER that run's result, and tether#145's flag only ever
// refused a RESULT — the error counted down whatever delivery had arrived in the
// meantime. Stamping it with the current run is what lets fanOut recognise it as
// the same run reporting twice.
//
// The assertion is the id and not merely "non-zero": zero would make fanOut apply
// it unconditionally, which is the defect, and any OTHER value would attribute it
// to a run that did not produce it.
func TestOpenCodeSSEErrorCarriesTheCurrentRun(t *testing.T) {
	s := &opencodeSession{
		spawnCtx: context.Background(),
		events:   make(chan Event, 8),
		sidCh:    make(chan struct{}),
	}
	// Three runs have been accepted on this session; the third is the one an error
	// read now belongs to.
	s.runSeq.Store(3)

	body := strings.Join([]string{
		`data: {"payload":{"type":"session.error","properties":{"error":{"message":"boom"}}}}`,
		"",
	}, "\n")

	var got []Event
	s.readSSE(context.Background(), strings.NewReader(body), func(ev Event) { got = append(got, ev) })

	if len(got) != 1 {
		t.Fatalf("readSSE emitted %d events, want 1: %+v", len(got), got)
	}
	if got[0].Kind != EventError {
		t.Fatalf("Kind = %q, want %q", got[0].Kind, EventError)
	}
	if got[0].RunID != 3 {
		t.Fatalf("session.error RunID = %d, want 3 (the run current when the frame was read). "+
			"0 would make fanOut apply it unconditionally — the late-error theft this stamp exists to stop", got[0].RunID)
	}
}

// TestCCRunIDsAdvanceOnTurnBoundaries pins the cc half of the contract: one run id
// per turn, and turn-closers that can never collide.
//
// cc gives no correlation id, so the boundary is derived from ORDER — the first
// event after a turn-closer opens the next run (see openRun for why the boundary
// is not `system/init`, which would let a turn with no init share the previous
// turn's id and get its legitimate result refused).
//
// The sequence below is cc's measured one for two queued prompts (tether#83, quoted
// in Entry.turnsInFlight): a turn's init, its text, its usage and result, then a
// fresh init and the second turn's result.
func TestCCRunIDsAdvanceOnTurnBoundaries(t *testing.T) {
	s := &ccSession{sidReady: make(chan struct{})}

	lines := []string{
		`{"type":"system","subtype":"init","session_id":"ses-a"}`,
		`{"type":"stream_event","event":{"type":"content_block_delta","delta":{"type":"text_delta","text":"hi"}}}`,
		`{"type":"result","result":"done one","usage":{"input_tokens":1,"output_tokens":2}}`,
		`{"type":"system","subtype":"init","session_id":"ses-a"}`,
		`{"type":"result","result":"done two"}`,
	}

	type stamped struct {
		kind EventKind
		run  int64
	}
	var got []stamped
	for _, line := range lines {
		for _, ev := range s.parseLine([]byte(line)) {
			got = append(got, stamped{ev.Kind, ev.RunID})
		}
	}

	want := []stamped{
		{EventInit, 1},
		{EventText, 1},
		{EventUsage, 1},  // same turn as the result it precedes, so the badge attaches to it
		{EventResult, 1}, // closes run 1
		{EventInit, 2},   // the first event after a closer opens the next run
		{EventResult, 2},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d events, want %d: %+v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("event %d = {%s run %d}, want {%s run %d}", i, got[i].kind, got[i].run, want[i].kind, want[i].run)
		}
	}
	// The property the ids exist for, read off what production ACTUALLY returned and
	// not off the table above — `want[3].run == want[5].run` compares two constants
	// and could never fail, which is a dead line dressed as an assertion. Two
	// turn-closers never sharing a run is what lets fanOut refuse duplicates without
	// ever refusing one of cc's legitimate turn-ends; that is the failure direction
	// cc must not have, because it would freeze the row on "working" for the rest of
	// the session.
	if len(got) == len(want) && got[3].run == got[5].run {
		t.Fatalf("both results carry run %d; fanOut would refuse the second and the row would read working forever", got[3].run)
	}
}

// TestCCRunIDsBackToBackResultsDoNotCollide is the pathological version of the
// case above, and it is here because it is the ONE way cc could lose a legitimate
// turn-end: two `result` lines with nothing at all between them.
//
// Not believed reachable against a real cc (a turn emits at least an init, and
// tether#48's `usage` rides on the result line itself), which is exactly why it is
// asserted rather than argued — openRun advancing on ANY event, including a
// closer, is what makes the collision impossible instead of unlikely.
func TestCCRunIDsBackToBackResultsDoNotCollide(t *testing.T) {
	s := &ccSession{sidReady: make(chan struct{})}
	line := []byte(`{"type":"result","result":"done"}`)

	first := s.parseLine(line)
	second := s.parseLine(line)
	if len(first) != 1 || len(second) != 1 {
		t.Fatalf("parseLine returned %d then %d events, want 1 each", len(first), len(second))
	}
	if first[0].RunID == second[0].RunID {
		t.Fatalf("two adjacent results both carry run %d; the second would be refused as a duplicate and the count would never come back down", first[0].RunID)
	}
}

// TestCCAbandonReusesTheRunItFound covers the reason cc derives a boundary at all
// rather than stamping a fresh id per turn-closer.
//
// `abandon` is the stream dying, not a run — so it reads s.runSeq without opening
// one, and the answer differs in exactly the way it should:
//
//   - mid-turn, the open run's id, so the error CLOSES the turn cc will never
//     answer (nothing else would: readLoop's close(s.events) carries no event);
//   - after a turn's result, that same (now closed) run's id, so fanOut refuses it
//     — leaving the count non-zero for the end-of-stream arm to report as an
//     interrupted turn instead of spending it here;
//   - before any event at all, zero, which Event.RunID defines as "no run" and
//     fanOut always applies.
//
// What each subtest can and cannot separate, because "three subtests" reads as
// "three times the cover" and it is not. The FIRST one distinguishes "abandon
// reuses" from "abandon MINTS" (a fresh id would read 2), which is the mutation
// worth catching — but it cannot distinguish `s.runSeq` from `s.openRun()`, because
// mid-turn those are equal by definition. The second and third are where that
// distinction lives: openRun would read 2 and 1 there against the 1 and 0 asserted.
func TestCCAbandonReusesTheRunItFound(t *testing.T) {
	newSession := func() *ccSession {
		return &ccSession{
			ctx:      context.Background(),
			events:   make(chan Event, 4),
			sidReady: make(chan struct{}),
		}
	}

	t.Run("mid-turn closes the open run", func(t *testing.T) {
		s := newSession()
		s.parseLine([]byte(`{"type":"system","subtype":"init","session_id":"ses-a"}`))
		s.abandon(context.Canceled)
		if got := (<-s.events).RunID; got != 1 {
			t.Fatalf("RunID = %d, want 1 — the turn cc was in the middle of, which nothing else will close", got)
		}
	})

	t.Run("after a result shares the closed run", func(t *testing.T) {
		s := newSession()
		s.parseLine([]byte(`{"type":"system","subtype":"init","session_id":"ses-a"}`))
		s.parseLine([]byte(`{"type":"result","result":"done"}`))
		s.abandon(context.Canceled)
		if got := (<-s.events).RunID; got != 1 {
			t.Fatalf("RunID = %d, want 1 — the run that already reported, so fanOut refuses this and the interrupted-turn notice survives", got)
		}
	})

	t.Run("before any event there is no run", func(t *testing.T) {
		s := newSession()
		s.abandon(context.Canceled)
		if got := (<-s.events).RunID; got != 0 {
			t.Fatalf("RunID = %d, want 0 — a stream that died before saying anything ran no turn of its own", got)
		}
	})
}
