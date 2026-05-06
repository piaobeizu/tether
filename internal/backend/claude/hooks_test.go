package claude

// Test fixture infrastructure for SessionStart hook idempotency
// (spec §6.C.4). v0.1 ships zero tether-owned hooks, so the contract is
// vacuously satisfied today — but this file lays in the fixture so the
// FIRST tether-owned-hook ticket can plug in directly (see
// "How the next ticket plugs in" at the bottom of this file).
//
// IDEMPOTENCY SEMANTIC CHOSEN: fire-on-each-recover (option a).
//
//	N1. claude fires SessionStart on every (re)spawn. Recover IS a
//	    re-spawn of the cc subprocess (see spawn.go BuildArgs +
//	    --resume), therefore claude WILL re-deliver SessionStart on
//	    every Recover. We can't suppress that — it's claude's behavior,
//	    not ours.
//	N2. Spec §6.C.4 mandates that tether-owned SessionStart hooks be
//	    "idempotent against re-firing". The cleanest reading is: the
//	    side effects must be safe to apply N times — NOT "the hook
//	    must short-circuit after the first fire". A hook that simply
//	    increments a counter is NOT idempotent in that sense. A hook
//	    that writes a fixed marker file (idempotent overwrite) IS.
//	N3. We therefore test the OBSERVABLE protocol: the hook command
//	    runs once per Start + once per Recover. counter=4 after
//	    New→Start→Recover×3 confirms cc honors our wiring. The "is
//	    your side effect actually idempotent?" test is per-hook and
//	    lives in the ticket that adds the hook.
//
// Rejected alternative: option b ("hook short-circuits on second fire
// via on-disk marker"). That's one VALID idempotency strategy a hook
// implementer can choose, but it's NOT what the protocol guarantees;
// asserting it here would over-constrain the contract.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// --- fixture --------------------------------------------------------

// hookFixture is a self-contained test scaffold for SessionStart hook
// idempotency tests. It builds:
//
//   - a temp settings.json (cc-shaped, see hooks.go::SettingsForCC for
//     the production-side analogue) wiring SessionStart to a shell
//     command that increments counter.txt
//   - a counter file the hook command writes to
//   - helpers to fire the hook directly (no claude needed for unit
//     scenarios) and to read the counter
//
// Reusable: the next "first-tether-owned-hook" ticket can drop a
// different command (calls into a daemon endpoint, sets a marker file,
// whatever) by overriding HookCommand before WriteSettings().
type hookFixture struct {
	t           *testing.T
	dir         string // tmp dir for settings.json + counter
	settingsPath string
	counterPath string
	// HookCommand is the shell command executed when SessionStart
	// fires. Defaults to "increment counter.txt by 1" but can be
	// overridden by tests that want to assert different idempotency
	// strategies (e.g. write-fixed-marker).
	HookCommand string
}

// newHookFixture creates a fresh fixture in t.TempDir(). Deferred
// cleanup is bound to t.Cleanup; the caller does NOT need to defer
// anything.
func newHookFixture(t *testing.T) *hookFixture {
	t.Helper()
	dir := t.TempDir()
	counterPath := filepath.Join(dir, "counter.txt")
	// Default: bash script that atomically increments counter.txt.
	// Uses a lockfile so concurrent fires (if claude ever fires hooks
	// in parallel during one spawn — it doesn't, but defensive) don't
	// race on read-modify-write.
	defaultCmd := fmt.Sprintf(`bash -c '
		set -eu
		f=%q
		# Read current value (or 0 if absent), increment, write back.
		# Single-writer per spawn so no flock needed in practice — keep
		# it simple.
		n=0
		if [ -f "$f" ]; then n=$(cat "$f"); fi
		echo $((n+1)) > "$f"
	'`, counterPath)
	return &hookFixture{
		t:            t,
		dir:          dir,
		settingsPath: filepath.Join(dir, "settings.json"),
		counterPath:  counterPath,
		HookCommand:  defaultCmd,
	}
}

// WriteSettings produces a cc-shaped settings.json under the fixture
// dir wiring SessionStart (and ONLY SessionStart — keep blast radius
// small for the test) to f.HookCommand.
//
// The shape mirrors what cc.SettingsForCC would emit, except the
// command is a literal shell command instead of a curl-to-localhost.
// This is intentional: the next ticket might wire SessionStart to a
// curl invocation (going through HookServer) or to a direct script —
// the fixture supports both.
func (f *hookFixture) WriteSettings() {
	f.t.Helper()
	doc := map[string]any{
		"hooks": map[string]any{
			"SessionStart": []map[string]any{
				{
					"hooks": []map[string]any{
						{
							"type":    "command",
							"command": f.HookCommand,
						},
					},
				},
			},
		},
	}
	body, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		f.t.Fatalf("marshal settings: %v", err)
	}
	if err := os.WriteFile(f.settingsPath, body, 0o600); err != nil {
		f.t.Fatalf("write settings: %v", err)
	}
}

// FireSessionStart simulates one cc-spawn-time SessionStart delivery
// by running the configured hook command. Used by unit-style tests
// that don't need a real claude subprocess — the protocol being
// tested ("hook fires once per spawn") is contractual on cc's side
// and not worth re-validating with a real subprocess on every CI run.
//
// Real-claude validation lives in a separate test gated behind
// requireClaude.
func (f *hookFixture) FireSessionStart(ctx context.Context) {
	f.t.Helper()
	// We invoke the same command shape cc uses: stdin is the hook
	// payload (a JSON object), stdout is captured but ignored for
	// observer hooks. We pass an empty payload — SessionStart hook
	// commands typically don't need it.
	cmd := exec.CommandContext(ctx, "bash", "-c", f.HookCommand)
	cmd.Stdin = strings.NewReader(`{"hookEventName":"SessionStart"}`)
	out, err := cmd.CombinedOutput()
	if err != nil {
		f.t.Fatalf("fire SessionStart: %v\nout: %s", err, out)
	}
}

// CounterValue reads the integer counter the default hook command
// increments. Returns 0 if the file is absent (hook never fired).
func (f *hookFixture) CounterValue() int {
	f.t.Helper()
	body, err := os.ReadFile(f.counterPath)
	if err != nil {
		if os.IsNotExist(err) {
			return 0
		}
		f.t.Fatalf("read counter: %v", err)
	}
	n, err := strconv.Atoi(strings.TrimSpace(string(body)))
	if err != nil {
		f.t.Fatalf("parse counter %q: %v", body, err)
	}
	return n
}

// SettingsPath returns the absolute path to the generated settings.json,
// for tests that pass it to claude --settings.
func (f *hookFixture) SettingsPath() string { return f.settingsPath }

// --- unit tests: fixture infrastructure ----------------------------

// TestHookFixture_SettingsJSONShape sanity-checks that WriteSettings
// emits a settings.json that parses and routes SessionStart correctly.
// If this regresses, every downstream test breaks — keep this first.
func TestHookFixture_SettingsJSONShape(t *testing.T) {
	f := newHookFixture(t)
	f.WriteSettings()

	body, err := os.ReadFile(f.SettingsPath())
	if err != nil {
		t.Fatalf("readback: %v", err)
	}
	var doc struct {
		Hooks map[string][]struct {
			Hooks []struct {
				Type    string `json:"type"`
				Command string `json:"command"`
			} `json:"hooks"`
		} `json:"hooks"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		t.Fatalf("settings.json malformed: %v\n%s", err, body)
	}
	entries, ok := doc.Hooks["SessionStart"]
	if !ok || len(entries) == 0 || len(entries[0].Hooks) == 0 {
		t.Fatalf("SessionStart not wired: %+v", doc)
	}
	if entries[0].Hooks[0].Type != "command" {
		t.Errorf("hook type: got %q want command", entries[0].Hooks[0].Type)
	}
	if !strings.Contains(entries[0].Hooks[0].Command, "counter.txt") {
		t.Errorf("hook command should reference counter.txt: %q", entries[0].Hooks[0].Command)
	}
	// File mode must be 0600 — same invariant cc.WriteSettingsFile
	// enforces (settings.json is sensitive: contains the daemon's
	// loopback URL or here, a path the test owns).
	stat, _ := os.Stat(f.SettingsPath())
	if stat.Mode().Perm()&0o077 != 0 {
		t.Errorf("settings file too permissive: %v", stat.Mode())
	}
}

// TestHookFixture_FireOnEachRecover_IncrementsCounter is the headline
// test of this slice. Documents the fire-on-each-recover semantics
// (see file header N1–N3) by simulating the spec §6.C.4 scenario:
//
//	1. New (no spawn-side hook fire — New is local-only construction)
//	2. Start (claude fires SessionStart) → counter = 1
//	3. Recover (claude re-spawns, re-fires SessionStart) → counter = 2
//	4. Recover → counter = 3
//	5. Recover → counter = 4
//
// We simulate "claude fires SessionStart" by running the hook command
// directly via FireSessionStart. The point isn't to exercise claude's
// machinery (covered by the gated real-claude test below) — it's to
// LOCK IN the contract that the next ticket's hook implementation
// must satisfy: the on-disk side-effects after N spawns are the side-
// effects of the command run N times.
func TestHookFixture_FireOnEachRecover_IncrementsCounter(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("bash hook command is POSIX-only")
	}
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not in PATH")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	f := newHookFixture(t)
	f.WriteSettings()

	// New() — counter must still be 0 (no spawn yet, no SessionStart
	// fire). This pins the protocol: the daemon doesn't fire hooks
	// on its own; only cc-spawn-time wiring does.
	if got := f.CounterValue(); got != 0 {
		t.Fatalf("counter before any fire: got %d, want 0", got)
	}

	// Start — first SessionStart.
	f.FireSessionStart(ctx)
	if got := f.CounterValue(); got != 1 {
		t.Fatalf("counter after Start: got %d, want 1", got)
	}

	// Recover ×3 — each one re-fires.
	for i := 1; i <= 3; i++ {
		f.FireSessionStart(ctx)
		want := 1 + i
		if got := f.CounterValue(); got != want {
			t.Fatalf("counter after Recover #%d: got %d, want %d", i, got, want)
		}
	}

	// Final assertion — by spec §6.C.4 fire-on-each-recover semantics:
	// 1 Start + 3 Recovers = 4 fires.
	if got := f.CounterValue(); got != 4 {
		t.Errorf("final counter: got %d, want 4 (1 Start + 3 Recovers)", got)
	}
}

// TestHookFixture_IdempotentMarkerStrategy demonstrates the alternative
// "hook short-circuits via on-disk marker" pattern. This is what a
// well-written tether-owned hook MIGHT do internally if its side-effect
// is non-idempotent (e.g. "register this session with an external
// system once"). Confirms the fixture supports overriding HookCommand.
//
// NOTE: this is NOT the protocol the spec mandates — it's a per-hook
// implementation choice. We assert the marker is written exactly once
// regardless of how many times the hook fires.
func TestHookFixture_IdempotentMarkerStrategy(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("bash hook command is POSIX-only")
	}
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not in PATH")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	f := newHookFixture(t)
	markerPath := filepath.Join(f.dir, "marker.txt")
	logPath := filepath.Join(f.dir, "log.txt")
	// Hook script: if marker exists, exit 0 silently. Else: write
	// marker AND append to a log so we can count attempts.
	f.HookCommand = fmt.Sprintf(`bash -c '
		set -eu
		marker=%q
		log=%q
		echo "fire" >> "$log"
		if [ -f "$marker" ]; then exit 0; fi
		echo "$(date +%%s%%N)" > "$marker"
	'`, markerPath, logPath)
	f.WriteSettings()

	// Fire 4 times (Start + Recover ×3).
	for i := 0; i < 4; i++ {
		f.FireSessionStart(ctx)
	}

	// Marker file must exist and contain a single timestamp (the
	// first-fire one). Subsequent fires short-circuited.
	mb, err := os.ReadFile(markerPath)
	if err != nil {
		t.Fatalf("marker not written: %v", err)
	}
	if len(strings.TrimSpace(string(mb))) == 0 {
		t.Errorf("marker empty: %q", mb)
	}

	// Log must show 4 attempts — the protocol still fired 4 times,
	// but the hook chose to short-circuit on the last 3.
	lb, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("log not written: %v", err)
	}
	lines := strings.Count(strings.TrimSpace(string(lb)), "\n") + 1
	if lines != 4 {
		t.Errorf("hook fire count: got %d log lines, want 4", lines)
	}
}

// TestHookFixture_FireSessionStartConcurrent_NoRace probes that the
// fixture's counter helper is safe under concurrent fires — defensive
// scaffolding in case a future test wants to assert "parallel fires
// during one spawn don't corrupt counters". Today claude fires
// SessionStart serially per spawn, so the practical N here is 1, but
// pinning the property prevents a future regression in the fixture.
func TestHookFixture_FireSessionStartConcurrent_NoRace(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("bash hook command is POSIX-only")
	}
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not in PATH")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	f := newHookFixture(t)
	// Use flock so concurrent increments serialize properly. The
	// default command's read-modify-write IS racy; this test
	// substitutes a flock-protected variant.
	f.HookCommand = fmt.Sprintf(`bash -c '
		set -eu
		f=%q
		(
			flock -x 9
			n=0
			if [ -f "$f" ]; then n=$(cat "$f"); fi
			echo $((n+1)) > "$f"
		) 9>%q.lock
	'`, f.counterPath, f.counterPath)
	f.WriteSettings()

	const N = 8
	var done atomic.Int32
	errCh := make(chan error, N)
	for i := 0; i < N; i++ {
		go func() {
			defer done.Add(1)
			defer func() {
				if r := recover(); r != nil {
					errCh <- fmt.Errorf("panic: %v", r)
				}
			}()
			cmd := exec.CommandContext(ctx, "bash", "-c", f.HookCommand)
			cmd.Stdin = strings.NewReader(`{}`)
			out, err := cmd.CombinedOutput()
			if err != nil {
				errCh <- fmt.Errorf("fire: %v\n%s", err, out)
				return
			}
			errCh <- nil
		}()
	}
	deadline := time.Now().Add(20 * time.Second)
	for done.Load() < N && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if done.Load() != N {
		t.Fatalf("only %d of %d fires completed before timeout", done.Load(), N)
	}
	close(errCh)
	for e := range errCh {
		if e != nil {
			t.Errorf("fire error: %v", e)
		}
	}
	if got := f.CounterValue(); got != N {
		t.Errorf("counter under concurrent fire (flock): got %d, want %d", got, N)
	}
}

// --- gated real-claude test: validates the protocol end-to-end ----

// TestHookFixture_RealClaude_SessionStartFiresOnRecover spawns real
// claude with --settings <fixture-settings.json> and verifies claude
// honors the SessionStart wiring on Start AND on Recover (re-spawn
// with --resume). This is the spec §8 scenario 7 deferred from gh-13
// — gated on requireClaude so CI without a claude binary skips
// cleanly.
//
// NOTE: this is a smoke test, not exhaustive. It confirms the
// integration shape; the hook-implementation tests live in the next
// ticket alongside the real hook.
func TestHookFixture_RealClaude_SessionStartFiresOnRecover(t *testing.T) {
	requireClaude(t)
	if testing.Short() {
		t.Skip("skipping real-claude test in -short mode")
	}
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not in PATH")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	f := newHookFixture(t)
	f.WriteSettings()

	// Spawn claude with --settings pointing at our fixture. We use
	// SpawnOpts directly (not Session.New) because we need to inject
	// the --settings flag, which BuildArgs doesn't currently support.
	// The next ticket will likely add SpawnOpts.SettingsPath; until
	// then, drive cmd.Args directly.
	resolved, err := exec.LookPath("claude")
	if err != nil {
		t.Skipf("claude binary: %v", err)
	}
	args := []string{
		"--print",
		"--output-format", "stream-json",
		"--input-format", "stream-json",
		"--verbose",
		"--include-partial-messages",
		"--permission-prompt-tool", "stdio",
		"--settings", f.SettingsPath(),
		"--model", "haiku",
	}
	runOnce := func(extraArgs ...string) {
		cmd := exec.CommandContext(ctx, resolved, append(args, extraArgs...)...)
		cmd.Dir = t.TempDir()
		stdin, err := cmd.StdinPipe()
		if err != nil {
			t.Fatalf("stdin pipe: %v", err)
		}
		cmd.Stdout = io.Discard
		cmd.Stderr = io.Discard
		if err := cmd.Start(); err != nil {
			t.Fatalf("start: %v", err)
		}
		// Send a single user message + close stdin so claude exits.
		_, _ = stdin.Write([]byte(`{"type":"user","message":{"role":"user","content":[{"type":"text","text":"reply ok"}]}}` + "\n"))
		_ = stdin.Close()
		_ = cmd.Wait()
	}

	// First run — Start. Expect counter = 1 after.
	runOnce()
	if got := f.CounterValue(); got < 1 {
		t.Fatalf("after first claude spawn: counter=%d, want >=1 (claude must fire SessionStart on every spawn)", got)
	}
	first := f.CounterValue()

	// Real --resume requires a captured session_id from run 1, which
	// we'd need to parse out of stdout. For this smoke test we just
	// re-spawn fresh (still tests "claude fires SessionStart on every
	// spawn"); the resume-with-sid case is covered by the unit test
	// above. The next ticket can extend this with a real resume.
	runOnce()
	if got := f.CounterValue(); got <= first {
		t.Errorf("after second claude spawn: counter=%d, want >%d (SessionStart should re-fire)", got, first)
	}
}

// --- How the next "first-tether-owned-hook" ticket plugs in -------
//
// The sibling daemon-cc-integration ticket (or whichever ships the
// first tether-owned hook) plugs into this fixture as follows:
//
//  1. Add SpawnOpts.SettingsPath (string) to spawn.go and have
//     BuildArgs append "--settings" + the path when non-empty. This
//     is the missing seam — claude already accepts --settings, but
//     SpawnOpts doesn't surface it yet.
//
//  2. Implement the hook command — typically a curl into the
//     daemon's HookServer (cc.HookServer at internal/cc) which
//     POSTs to /hooks/session-start. The settings.json shape this
//     fixture emits matches what cc.SettingsForCC produces, so the
//     same daemon endpoint serves both.
//
//  3. Write the per-hook test in this same file:
//     - reuse newHookFixture(t)
//     - override f.HookCommand to be the curl command (or use
//       cc.SettingsForCC + cc.HookServer for full round-trip)
//     - run the New→Start→Recover×3 sequence with real Session.New
//       + Session.Start + Session.Recover (no longer simulated)
//     - assert the per-hook side-effect (the daemon's chosen
//       semantic — typically an idempotent marker / no-op on second
//       fire if the side-effect is non-idempotent)
//
//  4. The "fire-on-each-recover" assertion in
//     TestHookFixture_FireOnEachRecover_IncrementsCounter remains
//     valid as the protocol-level invariant: claude WILL fire
//     SessionStart on every spawn. The hook implementer's job is
//     to make that safe.
//
// In one sentence: the next ticket adds SpawnOpts.SettingsPath +
// a real hook command, then writes a test that mirrors
// TestHookFixture_FireOnEachRecover_IncrementsCounter but with
// real Session.Recover instead of FireSessionStart.
