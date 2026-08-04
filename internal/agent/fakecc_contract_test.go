package agent

// Contract tests for the fake cc itself (tether#53).
//
// Everything else built on the fake is only as trustworthy as the fake is
// faithful, so these tests pin the fake against the measured claude 2.1.220
// probe (team memory mem_2ruSlrHR) rather than against what the fake happens to
// do. They exec it DIRECTLY, not through ClaudeCodeProvider, because the
// contract is about raw stdout LINES / exit codes / stderr — everything the
// provider's parser deliberately hides.
//
// These are also the mutation-verification anchors: break a discriminating
// behaviour in fakecc_test.go (e.g. emit system/init on an unknown --resume) and
// TestFakeCC_ResumeUnknownSessionFails must go red.

import (
	"reflect"
	"strings"
	"testing"
)

const fakeCCTestUUID = "11111111-2222-4333-8444-555555555555"

// TestFakeCC_NormalEventOrder pins the measured event ORDER: system/hook_started
// → system/hook_response → system/init → assistant → result/success, with the
// stream_event delta block in between because --include-partial-messages was
// passed. The load-bearing assertion is that system/init is NOT the first line:
// a fake that emitted init first would let a consumer that assumes "init arrives
// before anything else" pass here and wedge against real cc.
func TestFakeCC_NormalEventOrder(t *testing.T) {
	h := newFakeCCHarness(t)
	cwd := t.TempDir()

	stdout, stderr, code := h.runFakeCC(t, cwd, fakeCCUserLine("hello"),
		"--print", "--output-format", "stream-json", "--input-format", "stream-json",
		"--verbose", "--include-partial-messages", "--permission-mode", "default",
		"--session-id", fakeCCTestUUID)

	if code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr: %q)", code, stderr)
	}
	lines := decodeStdoutLines(t, stdout)
	kinds := lineKinds(lines)

	// message_start, content_block_start, one text_delta per chunk,
	// content_block_stop, message_stop. The delta count is derived from the
	// chunker so re-chunking the reply cannot masquerade as an ordering
	// regression — ORDER is what this test exists to pin.
	want := []string{"system/hook_started", "system/hook_response", "system/init"}
	for i := 0; i < len(fakeCCChunks(fakeCCDefaultReply))+4; i++ {
		want = append(want, "stream_event")
	}
	want = append(want, "assistant", "result/success")
	if !reflect.DeepEqual(kinds, want) {
		t.Fatalf("event order =\n  %v\nwant\n  %v", kinds, want)
	}
	if kinds[0] == "system/init" {
		t.Error("system/init must not be the first line — hook events precede it (mem_2ruSlrHR ⑤)")
	}
}

// TestFakeCC_SessionIDAdopted — --session-id <uuid> is adopted verbatim: both
// system/init and the result echo the caller's uuid (mem_2ruSlrHR ①). This is
// the property that lets tether#50 mint the session id itself instead of
// learning it from init.
func TestFakeCC_SessionIDAdopted(t *testing.T) {
	h := newFakeCCHarness(t)
	cwd := t.TempDir()

	stdout, _, code := h.runFakeCC(t, cwd, fakeCCUserLine("hi"),
		"--input-format", "stream-json", "--session-id", fakeCCTestUUID)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}

	var sawInit, sawResult bool
	for _, m := range decodeStdoutLines(t, stdout) {
		sid, _ := m["session_id"].(string)
		switch {
		case m["type"] == "system" && m["subtype"] == "init":
			sawInit = true
			if sid != fakeCCTestUUID {
				t.Errorf("init session_id = %q, want the requested %q", sid, fakeCCTestUUID)
			}
		case m["type"] == "result":
			sawResult = true
			if sid != fakeCCTestUUID {
				t.Errorf("result session_id = %q, want the requested %q", sid, fakeCCTestUUID)
			}
		}
	}
	if !sawInit || !sawResult {
		t.Fatalf("expected both init and result lines, got init=%v result=%v", sawInit, sawResult)
	}
}

// TestFakeCC_ResultSuccessCarriesUsage — result/success carries a top-level
// usage{input_tokens,output_tokens} (mem_2ruSlrHR ⑥). tether#48's token badge
// reads exactly this, so a fake that omitted it would make the badge path
// untestable.
func TestFakeCC_ResultSuccessCarriesUsage(t *testing.T) {
	h := newFakeCCHarness(t)
	h.Reply = "short answer"
	cwd := t.TempDir()

	stdout, _, _ := h.runFakeCC(t, cwd, fakeCCUserLine("prompt"),
		"--input-format", "stream-json")

	for _, m := range decodeStdoutLines(t, stdout) {
		if m["type"] != "result" {
			continue
		}
		usage, ok := m["usage"].(map[string]any)
		if !ok {
			t.Fatalf("result has no top-level usage object: %+v", m)
		}
		if got := usage["input_tokens"]; got != float64(len("prompt")) {
			t.Errorf("usage.input_tokens = %v, want %d", got, len("prompt"))
		}
		if got := usage["output_tokens"]; got != float64(len("short answer")) {
			t.Errorf("usage.output_tokens = %v, want %d", got, len("short answer"))
		}
		return
	}
	t.Fatal("no result line emitted")
}

// TestFakeCC_ResumeKnownSessionSucceeds — resuming a session this cwd owns
// exits 0 and does not let the sid drift (mem_2ruSlrHR ②).
func TestFakeCC_ResumeKnownSessionSucceeds(t *testing.T) {
	h := newFakeCCHarness(t)
	cwd := t.TempDir()
	h.SeedSession(t, fakeCCTestUUID, cwd)

	stdout, stderr, code := h.runFakeCC(t, cwd, fakeCCUserLine("again"),
		"--input-format", "stream-json", "--resume", fakeCCTestUUID)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr: %q)", code, stderr)
	}
	if stderr != "" {
		t.Errorf("stderr = %q, want empty on a successful resume", stderr)
	}
	kinds := lineKinds(decodeStdoutLines(t, stdout))
	if len(kinds) == 0 || kinds[2] != "system/init" {
		t.Fatalf("expected the normal event order on a successful resume, got %v", kinds)
	}
	for _, m := range decodeStdoutLines(t, stdout) {
		if sid, _ := m["session_id"].(string); sid != fakeCCTestUUID {
			t.Errorf("session_id drifted to %q on resume, want %q", sid, fakeCCTestUUID)
		}
	}
}

// TestFakeCC_ResumeUnknownSessionFails is the single most important contract in
// this file — the failure shape tether#49 tripped over and tether#50 has to
// handle. Measured (mem_2ruSlrHR ③): exit 1; stdout is EXACTLY ONE line, a
// result/error_during_execution with is_error true, result null, num_turns 0 and
// the REQUESTED session id; NO system/init anywhere; stderr says
// "No conversation found with session ID: <uuid>".
//
// The `result: null` assertion is not cosmetic: it is why tether's parseLine
// yields an EventResult with empty text (the blank-bubble hazard #50 must
// swallow), and pinning it here is what stops the fake from quietly "fixing"
// the null into an empty string.
func TestFakeCC_ResumeUnknownSessionFails(t *testing.T) {
	h := newFakeCCHarness(t)
	cwd := t.TempDir()

	// Deliberately NOT seeded: no session marker exists for this uuid.
	stdout, stderr, code := h.runFakeCC(t, cwd, fakeCCUserLine("hello"),
		"--print", "--output-format", "stream-json", "--input-format", "stream-json",
		"--resume", fakeCCTestUUID)

	if code != 1 {
		t.Errorf("exit code = %d, want 1", code)
	}
	if want := fakeCCNoConversation + fakeCCTestUUID; !strings.Contains(stderr, want) {
		t.Errorf("stderr = %q, want it to contain %q", stderr, want)
	}

	lines := decodeStdoutLines(t, stdout)
	if len(lines) != 1 {
		t.Fatalf("stdout has %d lines, want exactly 1: %v", len(lines), lineKinds(lines))
	}
	m := lines[0]
	if m["type"] != "result" {
		t.Errorf(`type = %v, want "result"`, m["type"])
	}
	if m["subtype"] != "error_during_execution" {
		t.Errorf(`subtype = %v, want "error_during_execution"`, m["subtype"])
	}
	if m["is_error"] != true {
		t.Errorf("is_error = %v, want true", m["is_error"])
	}
	if v, present := m["result"]; !present || v != nil {
		t.Errorf("result = %#v (present=%v), want a present JSON null", v, present)
	}
	if m["num_turns"] != float64(0) {
		t.Errorf("num_turns = %v, want 0", m["num_turns"])
	}
	if m["session_id"] != fakeCCTestUUID {
		t.Errorf("session_id = %v, want the requested %q", m["session_id"], fakeCCTestUUID)
	}
	if _, hasUsage := m["usage"]; hasUsage {
		t.Errorf("failure result must not carry usage: %+v", m)
	}

	// The absence of system/init is the discriminator the tether#49 guard rests
	// on (SessionID() returning "" because init never came).
	if strings.Contains(stdout, `"subtype":"init"`) {
		t.Errorf("a failed resume must emit NO system/init; stdout = %q", stdout)
	}
}

// TestFakeCC_ResumeCrossCwdFails — resume is cwd-scoped: a session id that IS
// known, resumed from a different directory, fails identically to an unknown one
// (mem_2ruSlrHR ③/④). Without this, a test could "prove" cross-workspace resume
// works when real cc would refuse.
func TestFakeCC_ResumeCrossCwdFails(t *testing.T) {
	h := newFakeCCHarness(t)
	ownerCwd := t.TempDir()
	otherCwd := t.TempDir()
	h.SeedSession(t, fakeCCTestUUID, ownerCwd)

	stdout, stderr, code := h.runFakeCC(t, otherCwd, fakeCCUserLine("hello"),
		"--input-format", "stream-json", "--resume", fakeCCTestUUID)

	if code != 1 {
		t.Errorf("exit code = %d, want 1 (resume must be cwd-scoped)", code)
	}
	if !strings.Contains(stderr, fakeCCNoConversation+fakeCCTestUUID) {
		t.Errorf("stderr = %q, want the not-found line", stderr)
	}
	if lines := decodeStdoutLines(t, stdout); len(lines) != 1 || lines[0]["subtype"] != "error_during_execution" {
		t.Fatalf("stdout = %q, want the single error_during_execution line", stdout)
	}
}

// TestFakeCC_FreshSessionBecomesResumable — a fresh run (no --resume) registers
// its session id as owned by its cwd, so a later --resume from the same cwd
// succeeds. This is the round trip tether#50's happy path depends on, and it
// also proves the fake mints an RFC-4122-shaped uuid when no --session-id is
// given, like real cc.
func TestFakeCC_FreshSessionBecomesResumable(t *testing.T) {
	h := newFakeCCHarness(t)
	cwd := t.TempDir()

	if _, _, code := h.runFakeCC(t, cwd, fakeCCUserLine("first"),
		"--input-format", "stream-json"); code != 0 {
		t.Fatalf("fresh run exit code = %d, want 0", code)
	}

	recs := h.Records(t)
	if len(recs) != 1 {
		t.Fatalf("recorded %d invocations, want 1", len(recs))
	}
	minted := recs[0].SessionID
	if len(minted) != 36 || strings.Count(minted, "-") != 4 {
		t.Errorf("minted session id %q is not uuid-shaped", minted)
	}
	if recs[0].SessionIDFlag != "" || recs[0].ResumeFlag != "" {
		t.Errorf("fresh run recorded flags %+v, want both empty", recs[0])
	}

	if _, stderr, code := h.runFakeCC(t, cwd, fakeCCUserLine("second"),
		"--input-format", "stream-json", "--resume", minted); code != 0 {
		t.Fatalf("resume of the minted sid exit code = %d, want 0 (stderr %q)", code, stderr)
	}
}

// TestFakeCC_SilentUntilFirstPrompt — under --input-format stream-json cc emits
// nothing until a user message arrives. That is why Registry.spawnEntry registers
// the entry under the sid it MINTED rather than waiting on SessionID(): there is
// nothing to wait for until the client has typed. A fake that emitted init at
// startup would erase that constraint from every test.
func TestFakeCC_SilentUntilFirstPrompt(t *testing.T) {
	h := newFakeCCHarness(t)
	cwd := t.TempDir()

	const noStdin = "" // stdin closes immediately, before any user message
	stdout, _, code := h.runFakeCC(t, cwd, noStdin,
		"--input-format", "stream-json", "--session-id", fakeCCTestUUID)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0 on immediate stdin EOF", code)
	}
	if strings.TrimSpace(stdout) != "" {
		t.Errorf("stdout = %q, want empty until the first user message", stdout)
	}
}

// TestFakeCC_MultiTurnReEmitsInit — cc re-emits system/init on every turn (a
// metadata refresh, not a new session), which is what Registry.fanOut's
// per-init ResetTurn relies on. Two prompts must therefore produce two init
// lines carrying the SAME session id, and a result whose num_turns advances.
func TestFakeCC_MultiTurnReEmitsInit(t *testing.T) {
	h := newFakeCCHarness(t)
	cwd := t.TempDir()

	stdin := fakeCCUserLine("one") + fakeCCUserLine("two")
	stdout, _, code := h.runFakeCC(t, cwd, stdin,
		"--input-format", "stream-json", "--session-id", fakeCCTestUUID)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}

	var inits int
	var turns []float64
	for _, m := range decodeStdoutLines(t, stdout) {
		if m["type"] == "system" && m["subtype"] == "init" {
			inits++
			if sid, _ := m["session_id"].(string); sid != fakeCCTestUUID {
				t.Errorf("turn %d init session_id = %q, want the stable %q", inits, sid, fakeCCTestUUID)
			}
		}
		if m["type"] == "result" {
			n, _ := m["num_turns"].(float64)
			turns = append(turns, n)
		}
	}
	if inits != 2 {
		t.Errorf("system/init count = %d, want 2 (one per turn)", inits)
	}
	if !reflect.DeepEqual(turns, []float64{1, 2}) {
		t.Errorf("result num_turns = %v, want [1 2]", turns)
	}
}

// TestFakeCC_PartialMessagesFlagGatesDeltas — the stream_event delta block
// appears ONLY with --include-partial-messages, mirroring what the real flag
// does (see ClaudeCodeProvider.Spawn's comment: without it, `assistant` arrives
// as one complete block). Without this gate the fake would stream even when the
// argv says not to, hiding a whole class of argv regression.
func TestFakeCC_PartialMessagesFlagGatesDeltas(t *testing.T) {
	h := newFakeCCHarness(t)
	cwd := t.TempDir()

	stdout, _, _ := h.runFakeCC(t, cwd, fakeCCUserLine("hi"),
		"--input-format", "stream-json", "--session-id", fakeCCTestUUID)

	for _, kind := range lineKinds(decodeStdoutLines(t, stdout)) {
		if kind == "stream_event" {
			t.Fatalf("emitted stream_event without --include-partial-messages: %q", stdout)
		}
	}
}

// TestFakeCC_ZeroTurnSessionNotResumable pins behaviour MEASURED against real
// cc (2.1.220, 2026-07-30 — mem_2ruSlrHR ⑦): a session created but never given
// a prompt is NOT resumable. Real cc never even reaches init when stdin EOFs
// first, and writes no session jsonl, so the later --resume fails.
//
// It matters because tether#50's mint → pin with --session-id → drop → --resume
// path runs straight through this branch, and because a client that reloads
// before sending anything hits it in normal use. Pinning it means the choice
// cannot drift silently: if someone later moves rememberFakeCCSession back to
// spawn time, this test fails.
func TestFakeCC_ZeroTurnSessionNotResumable(t *testing.T) {
	h := newFakeCCHarness(t)
	cwd := t.TempDir()

	// Spawn with a pinned session id but never send a user message.
	if _, _, code := h.runFakeCC(t, cwd, "",
		"--input-format", "stream-json", "--session-id", fakeCCTestUUID); code != 0 {
		t.Fatalf("zero-turn run exit code = %d, want 0", code)
	}

	stdout, stderr, code := h.runFakeCC(t, cwd, fakeCCUserLine("hello"),
		"--input-format", "stream-json", "--resume", fakeCCTestUUID)
	if code != 1 {
		t.Errorf("resume of a zero-turn session: exit code = %d, want 1", code)
	}
	if !strings.Contains(stderr, fakeCCNoConversation+fakeCCTestUUID) {
		t.Errorf("stderr = %q, want the not-found line", stderr)
	}
	if lines := decodeStdoutLines(t, stdout); len(lines) != 1 || lines[0]["subtype"] != "error_during_execution" {
		t.Fatalf("stdout = %q, want the single error_during_execution line", stdout)
	}
}

// TestFakeCC_TestMainGuardNeedsCCArgv guards the guard. envFakeCC is inherited by
// every child of a process that exported it, so if TestMain keyed only on the env
// var, `TETHER_FAKE_CC=1 go test ./internal/agent/` would divert the SUITE into
// the fake and exit 0 having run nothing — a silently green test package, which
// for a file whose entire job is to stop tests from lying is the worst available
// outcome. isFakeCCInvocation therefore also requires an argv that looks like
// cc's, and this pins both directions.
func TestFakeCC_TestMainGuardNeedsCCArgv(t *testing.T) {
	t.Setenv(envFakeCC, "1") // simulate an ambient/exported marker

	for _, tc := range []struct {
		name string
		args []string
		want bool
	}{
		{"cc argv diverts", []string{"--print", "--output-format", "stream-json"}, true},
		{"resume argv diverts", []string{"--resume", fakeCCTestUUID}, true},
		{"go test argv does NOT divert", []string{"-test.paniconexit0", "-test.timeout=10m0s"}, false},
		{"go test -run argv does NOT divert", []string{"-test.run=TestFakeCC", "-test.v=true"}, false},
		{"mixed argv does NOT divert", []string{"--print", "-test.timeout=10m0s"}, false},
		{"empty argv does NOT divert", nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := isFakeCCInvocation(tc.args); got != tc.want {
				t.Errorf("isFakeCCInvocation(%v) = %v, want %v", tc.args, got, tc.want)
			}
		})
	}
}

// TestFakeCC_TestMainGuardNeedsMarker — the converse: cc-shaped argv alone must
// NOT divert, or a plain `go test` run would become the fake the moment someone
// passed an unrecognised flag.
func TestFakeCC_TestMainGuardNeedsMarker(t *testing.T) {
	t.Setenv(envFakeCC, "")
	if isFakeCCInvocation([]string{"--print", "--output-format", "stream-json"}) {
		t.Error("isFakeCCInvocation = true without the env marker; the marker must be required")
	}
}

// TestFakeCC_ChunksReassembleToReply — the text_delta chunks must concatenate
// back to exactly the assistant reply, so a streaming test can assert the
// assembled text without the fake silently dropping or duplicating characters.
func TestFakeCC_ChunksReassembleToReply(t *testing.T) {
	for _, reply := range []string{
		fakeCCDefaultReply,
		"x",
		"two words",
		"with\nnewline and  double  spaces",
		"unicode 云朵飘过",
	} {
		chunks := fakeCCChunks(reply)
		if got := strings.Join(chunks, ""); got != reply {
			t.Errorf("chunks(%q) reassemble to %q", reply, got)
		}
		if len(reply) > 3 && len(chunks) < 2 {
			t.Errorf("chunks(%q) = %d chunk(s), want >= 2 so streaming is observable", reply, len(chunks))
		}
	}
}
