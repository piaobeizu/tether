package agent

// fake cc — an argv-aware stand-in for the `claude` CLI (tether#53).
//
// WHY THIS EXISTS
//
// tether drives cc as a subprocess: it builds an argv, writes stream-json user
// messages to its stdin, and parses stream-json out of its stdout. Two layers
// of test doubles existed before this file and NEITHER covered that seam:
//
//   - internal/session's fakeProvider fakes at the agent.Provider interface, so
//     it never constructs an argv and never parses a stream-json line;
//   - the old writeFakeCCScript here was a two-line `#!/bin/sh` that printed its
//     cwd and exited — enough to assert cmd.Dir, blind to everything else.
//
// So the spawn/resume behaviour that tether#50 is about to rework (mint a uuid,
// pin it with --session-id, reconnect with --resume, fall back to a fresh uuid
// when the resume fails) had no deterministic test surface at all. This is that
// surface.
//
// HOW IT RUNS
//
// The fake is compiled into this package's test binary and re-executed by
// TestMain: fakeCCPath returns the test binary's own path, and Spawn's
// SpawnConfig.Env carries envFakeCC, which makes the re-executed process run
// fakeCCMain instead of the test suite. That keeps the fake in ordinary,
// vet-checked Go (no shell, no `go build` step, no separate module), and means
// `go test ./internal/agent` needs nothing on PATH.
//
// FIDELITY IS THE WHOLE POINT
//
// A stub that is merely *plausible* would make every test built on it a
// self-deception. Every behaviour below is anchored to a real probe of claude
// 2.1.220 (2026-07-25, team memory mem_2ruSlrHR):
//
//	① --session-id <uuid> is ADOPTED — system/init and result both echo the
//	   uuid the caller supplied, so the caller can mint its own session id.
//	② --resume <known uuid> in the SAME cwd succeeds, exit 0, sid does not drift.
//	③ --resume <unknown uuid>, and --resume of a known uuid from a DIFFERENT
//	   cwd, behave identically: exit 1, stdout is EXACTLY ONE line
//	   {"type":"result","subtype":"error_during_execution","is_error":true,
//	    "result":null,"num_turns":0,"session_id":"<requested uuid>"},
//	   NO system/init at all, and stderr carries
//	   "No conversation found with session ID: <uuid>".
//	④ resume is cwd-scoped (real cc keys the transcript directory on the cwd);
//	   modelled here by storing the creating cwd inside the session marker file.
//	⑤ the normal event order is system/hook_started → system/hook_response →
//	   system/init → assistant → result/success. init is NOT the first line —
//	   a stub that emits init first would hide any consumer that assumes it is.
//	⑥ result/success carries a top-level usage{input_tokens,output_tokens}
//	   (tether#48's token badge reads it).
//
// Two further behaviours are not from the probe but from tether's own code and
// are just as load-bearing:
//
//   - in --input-format stream-json mode cc emits nothing until the first user
//     message arrives (Registry.spawnEntry's comment on why the daemon minting the
//     sid is what lets it register a session before cc has said anything), so the
//     fake blocks on stdin before emitting a single line;
//   - cc re-emits system/init on EVERY turn as a metadata refresh, not a new
//     session boundary (Registry.fanOut's ResetTurn comment), so the fake does
//     too.
//
// Fields the probe did NOT measure are deliberately NOT invented: the system
// lines carry only type/subtype/session_id. Only the failure `result` line is
// byte-shaped on purpose (a struct with the measured field order), because that
// exact line is the contract tether#50's fallback path has to recognise.

import (
	"bufio"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// Environment variables read ONLY by the fake (i.e. only ever inside a test
// binary that TestMain has diverted into fakeCCMain). Nothing in tether's
// production path reads any of them: the daemon passes them nowhere, and the
// fake is not reachable outside `go test`. They are the "controlled directory /
// environment" that decides which session ids count as resumable.
const (
	// envFakeCC, when non-empty, makes TestMain run the fake instead of tests.
	envFakeCC = "TETHER_FAKE_CC"
	// envFakeCCSessions names a directory of session marker files. Unset means
	// no session is ever resumable, so --resume always takes the failure path.
	envFakeCCSessions = "TETHER_FAKE_CC_SESSIONS"
	// envFakeCCRecord names a JSONL file the fake appends one fakeCCRecord to
	// per invocation — how a test observes the argv/cwd it was actually given.
	envFakeCCRecord = "TETHER_FAKE_CC_RECORD"
	// envFakeCCReply overrides the assistant reply text.
	envFakeCCReply = "TETHER_FAKE_CC_REPLY"
	// envFakeCCExitEarly makes the fake record its invocation and exit 0 without
	// reading stdin or emitting anything — for tests that only care about how
	// the process was launched (e.g. its cwd), and for modelling a cc that dies
	// instantly. This is an explicit knob, not a claim about real cc.
	envFakeCCExitEarly = "TETHER_FAKE_CC_EXIT_IMMEDIATELY"
)

// fakeCCDefaultReply is the assistant text the fake produces. Long enough to
// chunk into several text_deltas; deliberately free of backticks so it can
// never trip the fenced-block parser downstream.
const fakeCCDefaultReply = "clouds drift slowly across a pale morning sky"

// fakeCCNoConversation is the stderr line real cc writes when --resume names a
// session it cannot find (measured; mem_2ruSlrHR ③). The daemon pipes cc's
// stderr straight through today, so this is fidelity for its own sake plus the
// anchor for anything that later wants to classify a resume failure.
const fakeCCNoConversation = "No conversation found with session ID: "

// TestMain diverts this test binary into the fake cc when it was launched AS
// the fake, and otherwise runs the suite normally.
//
// The divert MUST happen before m.Run(): m.Run() parses os.Args as test flags,
// and the re-executed process's argv is cc's (--print, --output-format, …),
// which would abort with "flag provided but not defined".
func TestMain(m *testing.M) {
	if isFakeCCInvocation(os.Args[1:]) {
		os.Exit(fakeCCMain(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
	}
	os.Exit(m.Run())
}

// isFakeCCInvocation reports whether this process is the fake cc rather than the
// test suite.
//
// The envFakeCC marker alone is NOT a safe discriminator: environment variables
// are inherited, so anything that exported it — a debugging shell, a CI step, an
// outer test that spawns `go test` — would divert the SUITE into the fake, which
// exits 0 having run no tests at all. `go test ./internal/agent/` would print a
// cheerful "ok" over zero coverage: a silently green suite, the worst possible
// failure mode for a file whose whole purpose is to stop tests from lying.
//
// So the argv has to agree. `go test` always passes at least one -test.* flag to
// the binary it builds (-test.paniconexit0, -test.timeout, and whatever the user
// asked for), while the fake is only ever launched with cc's own flags. Requiring
// a non-empty argv with no -test.* in it separates the two cases without
// constraining what the fake can be asked to parse.
func isFakeCCInvocation(args []string) bool {
	if os.Getenv(envFakeCC) == "" {
		return false
	}
	if len(args) == 0 {
		return false
	}
	for _, arg := range args {
		if strings.HasPrefix(arg, "-test.") {
			return false
		}
	}
	return true
}

// ─── fake cc: argv ──────────────────────────────────────────────────────────

// fakeCCArgs is the part of cc's command line that changes what the fake DOES.
// The other flags tether passes (--print / --verbose / --output-format /
// --input-format / --permission-mode) are recognised only so their values are
// skipped rather than misread; asserting that they were passed is the job of
// TestSpawn_ArgvContract, which compares the recorded argv verbatim, so keeping
// parsed copies of them here would just be unread state.
type fakeCCArgs struct {
	argv           []string
	includePartial bool
	sessionID      string
	resume         string
}

func parseFakeCCArgs(argv []string) fakeCCArgs {
	a := fakeCCArgs{argv: argv}
	value := func(i int) string {
		if i+1 < len(argv) {
			return argv[i+1]
		}
		return ""
	}
	for i := 0; i < len(argv); i++ {
		switch argv[i] {
		case "--include-partial-messages":
			a.includePartial = true
		case "--session-id":
			a.sessionID = value(i)
			i++
		case "--resume":
			a.resume = value(i)
			i++
		case "--output-format", "--input-format", "--permission-mode":
			i++ // recognised, value skipped, otherwise unused
		}
	}
	return a
}

// ─── fake cc: invocation record ─────────────────────────────────────────────

// fakeCCRecord is one line of the JSONL file named by envFakeCCRecord: what
// argv the fake was launched with, where it ran, and which session id it
// settled on. Appending (rather than overwriting) keeps every invocation of a
// multi-spawn test observable.
type fakeCCRecord struct {
	Argv          []string `json:"argv"`
	Cwd           string   `json:"cwd"`
	SessionIDFlag string   `json:"session_id_flag"`
	ResumeFlag    string   `json:"resume_flag"`
	// SessionID is the id the fake actually used: the --session-id value, the
	// resumed id, or a freshly minted uuid. Empty when the run never got that
	// far (resume failure, or envFakeCCExitEarly).
	SessionID    string `json:"session_id"`
	ResumeFailed bool   `json:"resume_failed"`
}

// appendFakeCCRecord writes rec as one JSONL line. A failure here would
// otherwise surface as the baffling "recorded 0 invocations, want 1", so the
// reason is reported on the fake's stderr — which the tests either assert on or
// print, making the real cause visible.
func appendFakeCCRecord(rec fakeCCRecord) {
	path := os.Getenv(envFakeCCRecord)
	if path == "" {
		return
	}
	b, err := json.Marshal(rec)
	if err != nil {
		fmt.Fprintf(os.Stderr, "fake cc: marshal invocation record: %v\n", err)
		return
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		fmt.Fprintf(os.Stderr, "fake cc: open invocation record %s: %v\n", path, err)
		return
	}
	defer f.Close()
	if _, err := f.Write(append(b, '\n')); err != nil {
		fmt.Fprintf(os.Stderr, "fake cc: write invocation record %s: %v\n", path, err)
	}
}

// readFakeCCRecords decodes every invocation the fake appended to path.
func readFakeCCRecords(t testing.TB, path string) []fakeCCRecord {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read fake cc record %s: %v", path, err)
	}
	var out []fakeCCRecord
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		if line == "" {
			continue
		}
		var rec fakeCCRecord
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("decode fake cc record %q: %v", line, err)
		}
		out = append(out, rec)
	}
	return out
}

// ─── fake cc: session store (cwd-scoped, like real cc) ──────────────────────

// fakeCCSessionPath maps a session id to its marker file under the directory
// named by envFakeCCSessions. It refuses ids that are not a single path element
// so a stray "../x" can never make the fake read outside its own directory.
func fakeCCSessionPath(sid string) (string, bool) {
	dir := os.Getenv(envFakeCCSessions)
	if dir == "" || sid == "" || sid == "." || sid == ".." || sid != filepath.Base(sid) {
		return "", false
	}
	return filepath.Join(dir, sid+".session"), true
}

// fakeCCSessionKnown reports whether sid is resumable FROM cwd. The marker
// file's content is the cwd that created the session, which is how the fake
// models real cc keying its transcript directory on the cwd (mem_2ruSlrHR ④):
// same uuid + different cwd = not found, exactly like the unknown-uuid case.
func fakeCCSessionKnown(sid, cwd string) bool {
	path, ok := fakeCCSessionPath(sid)
	if !ok {
		return false
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	return strings.TrimSpace(string(b)) == cwd
}

// canonicalPath resolves symlinks so the fake's cwd bookkeeping is stable no
// matter which form of a path it is handed. t.TempDir() lives under /tmp, which
// is a symlink on some platforms, and a subprocess's os.Getwd() may report
// either the link or its target depending on the inherited PWD — comparing raw
// strings would make the cwd-scoped resume checks environment-dependent.
// Unresolvable paths (e.g. a directory already removed) pass through unchanged.
func canonicalPath(p string) string {
	if p == "" {
		return ""
	}
	if resolved, err := filepath.EvalSymlinks(p); err == nil {
		return resolved
	}
	return p
}

// fakeCCCwd is the fake's own canonical working directory — the identity a
// session is scoped to.
func fakeCCCwd() string {
	wd, err := os.Getwd()
	if err != nil {
		return ""
	}
	return canonicalPath(wd)
}

// rememberFakeCCSession makes sid resumable from cwd. Callers invoke it when a
// TURN STARTS, not when the process spawns.
//
// MEASURED against real cc (2.1.220, 2026-07-30 — mem_2ruSlrHR ⑦): a session
// that was created but never used is NOT resumable. Spawning with --session-id
// and letting stdin EOF immediately exits 0 after emitting only
// system/hook_started + system/hook_response — no init, and no session jsonl on
// disk — so the follow-up --resume fails with the standard "No conversation
// found" shape. tether#50 sits directly on this branch, and the measurement
// makes its fallback path ordinary rather than exceptional: a client that
// connects and reloads without ever sending a prompt has nothing to resume.
// TestFakeCC_ZeroTurnSessionNotResumable pins the behaviour so it cannot drift.
func rememberFakeCCSession(sid, cwd string) {
	path, ok := fakeCCSessionPath(sid)
	if !ok {
		return
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return
	}
	_ = os.WriteFile(path, []byte(cwd), 0o644)
}

// ─── fake cc: stream-json output ────────────────────────────────────────────

// fakeCCResultLine is the `result` event. It is a struct rather than a map so
// the marshalled field ORDER matches the measured failure line byte for byte:
//
//	{"type":"result","subtype":"error_during_execution","is_error":true,
//	 "result":null,"num_turns":0,"session_id":"<uuid>"}
//
// Result is a *string precisely so the failure case renders `"result":null`
// (the null that makes tether's parseLine produce an EMPTY EventResult — the
// blank-bubble hazard tether#50's fallback has to swallow), while success
// renders the reply text. usage/total_cost_usd are omitempty so they are absent
// on the failure line, as measured.
type fakeCCResultLine struct {
	Type      string       `json:"type"`
	Subtype   string       `json:"subtype"`
	IsError   bool         `json:"is_error"`
	Result    *string      `json:"result"`
	NumTurns  int          `json:"num_turns"`
	SessionID string       `json:"session_id"`
	Usage     *fakeCCUsage `json:"usage,omitempty"`
	TotalCost float64      `json:"total_cost_usd,omitempty"`
}

// fakeCCUsage is the top-level per-turn token accounting on result/success
// (mem_2ruSlrHR ⑥). Real cc also reports cache_* counts; tether ignores them
// (rawUsage in claude_provider.go) so the fake does not model them.
type fakeCCUsage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

// fakeCCSystemLine is a `system` event. Only type/subtype/session_id are
// modelled: the probe measured the ORDER of hook_started/hook_response/init,
// not their full field set, and inventing the rest would fabricate a contract.
type fakeCCSystemLine struct {
	Type      string `json:"type"`
	Subtype   string `json:"subtype"`
	SessionID string `json:"session_id"`
}

type fakeCCEmitter struct {
	enc *json.Encoder
}

func newFakeCCEmitter(w io.Writer) *fakeCCEmitter {
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	return &fakeCCEmitter{enc: enc}
}

func (e *fakeCCEmitter) emit(v any) { _ = e.enc.Encode(v) }

func (e *fakeCCEmitter) system(subtype, sid string) {
	e.emit(fakeCCSystemLine{Type: "system", Subtype: subtype, SessionID: sid})
}

func (e *fakeCCEmitter) streamEvent(sid string, inner map[string]any) {
	e.emit(map[string]any{"type": "stream_event", "session_id": sid, "event": inner})
}

func (e *fakeCCEmitter) assistant(sid, text string) {
	e.emit(map[string]any{
		"type":       "assistant",
		"session_id": sid,
		"message": map[string]any{
			"role":    "assistant",
			"content": []any{map[string]any{"type": "text", "text": text}},
		},
	})
}

// fakeCCChunks splits s into up to three pieces at rune boundaries. Three so a
// consumer can prove token-level streaming (>= 2 deltas); rune boundaries (not
// words) so the concatenation of the chunks is always exactly s, whatever
// whitespace it contains.
func fakeCCChunks(s string) []string {
	if s == "" {
		return nil
	}
	r := []rune(s)
	n := 3
	if len(r) < n {
		n = len(r)
	}
	per := (len(r) + n - 1) / n
	out := make([]string, 0, n)
	for i := 0; i < len(r); i += per {
		end := i + per
		if end > len(r) {
			end = len(r)
		}
		out = append(out, string(r[i:end]))
	}
	return out
}

// emitFakeCCTurn writes one full assistant turn in the measured order
// (mem_2ruSlrHR ⑤): hook_started → hook_response → init → [stream_event…] →
// assistant → result/success. The stream_event block appears only when
// --include-partial-messages was passed, which is exactly what that flag does
// in real cc (see ClaudeCodeProvider.Spawn's comment on it).
func emitFakeCCTurn(e *fakeCCEmitter, a fakeCCArgs, sid, prompt, reply string, turn int) {
	e.system("hook_started", sid)
	e.system("hook_response", sid)
	e.system("init", sid)

	if a.includePartial {
		e.streamEvent(sid, map[string]any{
			"type":    "message_start",
			"message": map[string]any{"role": "assistant", "content": []any{}},
		})
		e.streamEvent(sid, map[string]any{
			"type": "content_block_start", "index": 0,
			"content_block": map[string]any{"type": "text", "text": ""},
		})
		for _, chunk := range fakeCCChunks(reply) {
			e.streamEvent(sid, map[string]any{
				"type": "content_block_delta", "index": 0,
				"delta": map[string]any{"type": "text_delta", "text": chunk},
			})
		}
		e.streamEvent(sid, map[string]any{"type": "content_block_stop", "index": 0})
		e.streamEvent(sid, map[string]any{"type": "message_stop"})
	}

	e.assistant(sid, reply)

	text := reply
	e.emit(fakeCCResultLine{
		Type:      "result",
		Subtype:   "success",
		IsError:   false,
		Result:    &text,
		NumTurns:  turn,
		SessionID: sid,
		Usage:     &fakeCCUsage{InputTokens: len(prompt), OutputTokens: len(reply)},
		TotalCost: 0.001,
	})
}

// ─── fake cc: main ──────────────────────────────────────────────────────────

// fakeCCMain is the fake's entry point; the int it returns is the process exit
// code. Split out from TestMain so its behaviour is plain, testable Go.
func fakeCCMain(argv []string, stdin io.Reader, stdout, stderr io.Writer) int {
	a := parseFakeCCArgs(argv)
	cwd := fakeCCCwd()
	rec := fakeCCRecord{Argv: argv, Cwd: cwd, SessionIDFlag: a.sessionID, ResumeFlag: a.resume}

	if os.Getenv(envFakeCCExitEarly) != "" {
		appendFakeCCRecord(rec)
		return 0
	}

	e := newFakeCCEmitter(stdout)

	// --resume of a session this cwd does not own: exit 1 with a single result
	// line and NO system/init, before touching stdin. Not reading stdin is part
	// of the contract — it is what makes the caller's first prompt hit a broken
	// pipe (tether#49).
	if a.resume != "" && !fakeCCSessionKnown(a.resume, cwd) {
		rec.ResumeFailed = true
		appendFakeCCRecord(rec)
		fmt.Fprintf(stderr, "%s%s\n", fakeCCNoConversation, a.resume)
		e.emit(fakeCCResultLine{
			Type:      "result",
			Subtype:   "error_during_execution",
			IsError:   true,
			Result:    nil,
			NumTurns:  0,
			SessionID: a.resume,
		})
		return 1
	}

	// Session id resolution: a resumed id wins, then a caller-minted
	// --session-id (adopted verbatim — mem_2ruSlrHR ①), else the fake mints one.
	//
	// MEASURED (2.1.220, 2026-07-30 — mem_2ruSlrHR ⑧): real cc REJECTS --resume
	// and --session-id together, exiting 1 with "--session-id can only be used
	// with --continue or --resume if --fork-session is also specified." tether
	// never produces that argv (Spawn emits at most one of the two), so the fake
	// stays permissive and lets a resumed id win instead of growing a rejection
	// path no caller exercises. If tether#50 ever needs both it must also pass
	// --fork-session, and this fake needs the matching branch first.
	sid := a.resume
	if sid == "" {
		sid = a.sessionID
	}
	if sid == "" {
		sid = newFakeCCUUID()
	}
	rec.SessionID = sid
	appendFakeCCRecord(rec)

	reply := fakeCCDefaultReply
	if r := os.Getenv(envFakeCCReply); r != "" {
		reply = r
	}

	// Nothing is emitted until the first user message arrives: that is how cc
	// behaves under --input-format stream-json, and the reason the registry
	// registers a pending placeholder key instead of waiting on SessionID().
	scanner := bufio.NewScanner(stdin)
	scanner.Buffer(make([]byte, 64<<10), 8<<20)
	turn := 0
	for scanner.Scan() {
		var msg struct {
			Type      string `json:"type"`
			RequestID string `json:"request_id"`
			Message   struct {
				Content json.RawMessage `json:"content"`
			} `json:"message"`
		}
		if err := json.Unmarshal(scanner.Bytes(), &msg); err != nil {
			continue
		}
		switch msg.Type {
		case "control_request":
			// Acknowledge an interrupt request the way cc does. Also keeps the
			// loop from mistaking a control frame for a prompt.
			e.emit(map[string]any{"type": "control_response", "response": map[string]any{
				"subtype": "success", "request_id": msg.RequestID,
			}})
		case "user":
			var prompt string
			_ = json.Unmarshal(msg.Message.Content, &prompt)
			turn++
			// The session becomes resumable only once it has a turn — see
			// rememberFakeCCSession for why this is deliberately gated here
			// rather than at spawn.
			rememberFakeCCSession(sid, cwd)
			emitFakeCCTurn(e, a, sid, prompt, reply, turn)
		}
	}
	return 0
}

// newFakeCCUUID returns a random RFC-4122 v4 uuid — the shape cc uses for
// session ids, and the shape tether#50 will mint.
func newFakeCCUUID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "00000000-0000-4000-8000-000000000000"
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	h := hex.EncodeToString(b[:])
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
}

// ─── test-side harness ──────────────────────────────────────────────────────

// fakeCCPath returns the path to this test binary, which doubles as the fake cc
// executable (see TestMain). Pass it to NewClaudeCodeProvider, or exec it
// directly to assert the fake's own contract.
func fakeCCPath(t testing.TB) string {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	return exe
}

// fakeCCHarness is one hermetic fake-cc setup: a session directory and an
// invocation-record file under t.TempDir(), plus the environment that points
// the fake at them.
type fakeCCHarness struct {
	SessionsDir string
	RecordPath  string
	// Reply overrides the assistant reply text when non-empty.
	Reply string
	// ExitEarly makes the fake record its invocation and exit without emitting.
	ExitEarly bool
}

func newFakeCCHarness(t testing.TB) *fakeCCHarness {
	t.Helper()
	dir := t.TempDir()
	sessions := filepath.Join(dir, "sessions")
	if err := os.MkdirAll(sessions, 0o755); err != nil {
		t.Fatalf("mkdir sessions: %v", err)
	}
	return &fakeCCHarness{
		SessionsDir: sessions,
		RecordPath:  filepath.Join(dir, "invocations.jsonl"),
	}
}

// Env is the SpawnConfig.Env / exec.Cmd.Env fragment that activates the fake.
//
// It also pins GORACE for the child, which is worth a word. The fake IS this
// test binary, so under `go test -race` the child is race-instrumented too — and
// a race-instrumented Go program that exits with status 0 calls racefini(), whose
// ThreadSanitizer atexit_sleep_ms defaults to 1000ms. That is a flat one-second
// tax on every successful fake-cc run (and, tellingly, none at all on the
// exit-1 resume-failure path, since the runtime only calls racefini when the
// exit code is 0). Zeroing it takes the package from ~16s back to ~1s. Race
// detection inside the child is unaffected in practice: reports are printed as
// they are found, and the atexit sleep only buys time for late reports from
// other threads — the fake is single-goroutine. Appending (rather than
// replacing) keeps any GORACE flags the caller set, since tsan takes the last
// occurrence of a repeated flag.
func (h *fakeCCHarness) Env() []string {
	env := []string{
		envFakeCC + "=1",
		envFakeCCSessions + "=" + h.SessionsDir,
		envFakeCCRecord + "=" + h.RecordPath,
		"GORACE=" + strings.TrimSpace(os.Getenv("GORACE")+" atexit_sleep_ms=0"),
	}
	if h.Reply != "" {
		env = append(env, envFakeCCReply+"="+h.Reply)
	}
	if h.ExitEarly {
		env = append(env, envFakeCCExitEarly+"=1")
	}
	return env
}

// Records returns every invocation of the fake so far.
func (h *fakeCCHarness) Records(t testing.TB) []fakeCCRecord {
	t.Helper()
	return readFakeCCRecords(t, h.RecordPath)
}

// SeedSession marks sid as resumable from cwd, without having to run a fresh
// turn first. cwd is canonicalised the same way the fake canonicalises its own,
// so callers pass the directory they will hand to SpawnConfig.Workdir verbatim.
func (h *fakeCCHarness) SeedSession(t testing.TB, sid, cwd string) {
	t.Helper()
	path := filepath.Join(h.SessionsDir, sid+".session")
	if err := os.WriteFile(path, []byte(canonicalPath(cwd)), 0o644); err != nil {
		t.Fatalf("seed session %s: %v", sid, err)
	}
}

// runFakeCC execs the fake directly (bypassing ClaudeCodeProvider) with the
// given argv, cwd and stdin, and returns its stdout, stderr and exit code.
// Used by the harness's own contract tests: they must be able to observe raw
// stdout LINES, which the provider's parser hides.
func (h *fakeCCHarness) runFakeCC(t testing.TB, cwd string, stdin string, args ...string) (stdout, stderr string, exitCode int) {
	t.Helper()
	cmd := exec.Command(fakeCCPath(t), args...)
	cmd.Dir = cwd
	cmd.Env = append(os.Environ(), h.Env()...)
	cmd.Stdin = strings.NewReader(stdin)
	var outBuf, errBuf strings.Builder
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf
	if err := cmd.Run(); err != nil {
		code, ok := exitCodeOf(err)
		if !ok {
			t.Fatalf("run fake cc %v: %v", args, err)
		}
		return outBuf.String(), errBuf.String(), code
	}
	return outBuf.String(), errBuf.String(), 0
}

// exitCodeOf extracts a subprocess exit status from an error returned by
// cmd.Run / cmd.Wait (the latter is what ccSession.Close returns).
func exitCodeOf(err error) (int, bool) {
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode(), true
	}
	return 0, false
}

// fakeCCUserLine is a stream-json user message, the shape ccSession.SendPrompt
// writes — used to feed the fake in direct-exec tests.
func fakeCCUserLine(text string) string {
	b, _ := json.Marshal(map[string]any{
		"type":    "user",
		"message": map[string]any{"role": "user", "content": text},
	})
	return string(b) + "\n"
}

// decodeStdoutLines decodes each non-empty line of the fake's stdout into a
// generic map, failing the test on the first line that is not valid JSON.
func decodeStdoutLines(t testing.TB, stdout string) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(stdout), "\n") {
		if line == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("fake cc emitted a non-JSON stdout line %q: %v", line, err)
		}
		out = append(out, m)
	}
	return out
}

// lineKinds renders decoded stdout lines as "type/subtype" strings so an
// expected event ORDER can be asserted (and printed) in one comparison.
func lineKinds(lines []map[string]any) []string {
	kinds := make([]string, 0, len(lines))
	for _, m := range lines {
		typ, _ := m["type"].(string)
		if sub, ok := m["subtype"].(string); ok && sub != "" {
			typ += "/" + sub
		}
		kinds = append(kinds, typ)
	}
	return kinds
}

// syncBuffer is an io.Writer safe for the os/exec stderr-copying goroutine to
// write while the test goroutine reads (needed once cmd.Stderr is not an
// *os.File — see the withStderr seam).
type syncBuffer struct {
	mu  sync.Mutex
	buf strings.Builder
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}
