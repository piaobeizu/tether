package agent

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestInjectAndRemoveMCPServer(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	if err := os.MkdirAll(filepath.Join(dir, ".claude"), 0o700); err != nil {
		t.Fatal(err)
	}

	const port = 8899
	const token = "testtoken123"

	if err := InjectMCPServer(port, token, "tether"); err != nil {
		t.Fatalf("InjectMCPServer: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(dir, ".claude", "settings.json"))
	if err != nil {
		t.Fatal(err)
	}
	var settings map[string]any
	if err := json.Unmarshal(data, &settings); err != nil {
		t.Fatal(err)
	}
	mcpServers, ok := settings["mcpServers"].(map[string]any)
	if !ok {
		t.Fatal("mcpServers not found")
	}
	tether, ok := mcpServers["tether"].(map[string]any)
	if !ok {
		t.Fatal("mcpServers.tether not found")
	}
	wantURL := fmt.Sprintf("http://127.0.0.1:%d/mcp", port)
	if tether["url"] != wantURL {
		t.Errorf("url = %q, want %q", tether["url"], wantURL)
	}
	headers, _ := tether["headers"].(map[string]any)
	if headers["Authorization"] != "Bearer "+token {
		t.Errorf("Authorization = %q, want Bearer %s", headers["Authorization"], token)
	}

	// Idempotent re-inject — must not duplicate.
	if err := InjectMCPServer(port, token, "tether"); err != nil {
		t.Fatalf("second InjectMCPServer: %v", err)
	}
	data2, _ := os.ReadFile(filepath.Join(dir, ".claude", "settings.json"))
	var settings2 map[string]any
	json.Unmarshal(data2, &settings2)
	mc2 := settings2["mcpServers"].(map[string]any)
	if len(mc2) != 1 {
		t.Errorf("expected 1 mcpServers entry after re-inject, got %d", len(mc2))
	}

	// Remove.
	if err := RemoveMCPServer("tether"); err != nil {
		t.Fatalf("RemoveMCPServer: %v", err)
	}
	data3, _ := os.ReadFile(filepath.Join(dir, ".claude", "settings.json"))
	var settings3 map[string]any
	json.Unmarshal(data3, &settings3)
	if _, has := settings3["mcpServers"]; has {
		t.Error("mcpServers should be absent after Remove")
	}
}

// ---------------------------------------------------------------------------
// tether#166 — PreToolUse entries multiply once the sentinel is stripped.
//
// The co-writer is Claude Code itself: it rebuilds each hook-matcher entry as a
// literal {matcher, hooks} object, so an unknown key like _tether_managed does
// not survive the round trip, while the entry is otherwise byte-identical. Every
// daemon start then re-injected, and the file was measured at 9 and later 18
// identical entries.
//
// These tests arrange that stripped-but-otherwise-identical entry directly. They
// deliberately do NOT simulate the co-writer by hand-editing a fixture that was
// derived from the sentinel constant: the arrangement below names the shape
// literally, so changing TetherManagedKey cannot quietly move the goalposts.
// ---------------------------------------------------------------------------

// sandboxHome points os.UserHomeDir() — and therefore ccSettingsPath() — at a
// throwaway directory.
//
// The trailing check is not ceremony. The bug under test is about a settings.json
// that other programs write, and the real ~/.claude/settings.json on a developer
// box is exactly such a file: on the host this fix was written on its sentinel was
// already missing, so a test that leaked out of the sandbox would not merely be
// dirty, it would pass or fail according to whose machine it ran on. Asserting the
// resolved path is inside the temp dir is what makes every count below a statement
// about the fixture rather than about the operator's laptop.
func sandboxHome(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	if err := os.MkdirAll(filepath.Join(dir, ".claude"), 0o700); err != nil {
		t.Fatal(err)
	}
	p, err := ccSettingsPath()
	if err != nil {
		t.Fatalf("ccSettingsPath in sandbox: %v", err)
	}
	if !strings.HasPrefix(p, dir+string(os.PathSeparator)) {
		t.Fatalf("sandbox escape: ccSettingsPath() = %q, want a path under %q; "+
			"refusing to run a test that would read or write the real user settings file", p, dir)
	}
	return dir
}

// tetherHookPath is the string internal/server/lifecycle.go Step 3 builds and
// passes to both cchook.EnsureHookBinary and agent.InjectPermHook:
// filepath.Join(tetherBinDir(), "tether-permission-hook") where tetherBinDir is
// $HOME/.tether/bin. There is no environment override for it.
func tetherHookPath(home string) string {
	return filepath.Join(home, ".tether", "bin", "tether-permission-hook")
}

// strippedEntry builds a PreToolUse entry in exactly the shape InjectPermHook
// writes — matcher "*", one nested {type: command, command} — with the sentinel
// key OMITTED. That is precisely what the file contains after the co-writer has
// rewritten it.
func strippedEntry(command string) map[string]any {
	return map[string]any{
		"matcher": "*",
		"hooks": []any{map[string]any{
			"type":    "command",
			"command": command,
		}},
	}
}

func writeSettings(t *testing.T, home string, settings map[string]any) {
	t.Helper()
	b, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".claude", "settings.json"), b, 0o600); err != nil {
		t.Fatal(err)
	}
}

func settingsWithPreToolUse(entries ...any) map[string]any {
	return map[string]any{
		"hooks": map[string]any{
			"PreToolUse": entries,
		},
	}
}

func readPreToolUse(t *testing.T, home string) []any {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(home, ".claude", "settings.json"))
	if err != nil {
		t.Fatalf("read settings: %v", err)
	}
	var settings map[string]any
	if err := json.Unmarshal(data, &settings); err != nil {
		t.Fatalf("parse settings: %v", err)
	}
	hooks, _ := settings["hooks"].(map[string]any)
	list, _ := hooks["PreToolUse"].([]any)
	return list
}

// countNaming reports how many entries carry command anywhere in their nested
// hooks array. "Anywhere", not "first": an entry shaped
// hooks:[{other},{tether}] is still tether's, and a first-only reading would let
// it duplicate.
func countNaming(list []any, command string) int {
	var n int
	for _, h := range list {
		hm, isObj := h.(map[string]any)
		if !isObj {
			continue
		}
		nested, _ := hm["hooks"].([]any)
		for _, nh := range nested {
			nhm, isObj := nh.(map[string]any)
			if !isObj {
				continue
			}
			if c, _ := nhm["command"].(string); strings.TrimSpace(c) == command {
				n++
				break
			}
		}
	}
	return n
}

// stripSentinels rewrites the file the way the co-writer does: every PreToolUse
// entry survives, minus the key the co-writer's typed schema has never heard of.
func stripSentinels(t *testing.T, home string) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(home, ".claude", "settings.json"))
	if err != nil {
		t.Fatalf("read settings: %v", err)
	}
	var settings map[string]any
	if err := json.Unmarshal(data, &settings); err != nil {
		t.Fatalf("parse settings: %v", err)
	}
	hooks, _ := settings["hooks"].(map[string]any)
	for _, kind := range []string{"PreToolUse", "PostToolUse"} {
		list, _ := hooks[kind].([]any)
		for _, h := range list {
			if hm, isObj := h.(map[string]any); isObj {
				delete(hm, TetherManagedKey)
			}
		}
	}
	writeSettings(t, home, settings)
}

// T1. The defect itself. One stripped tether entry is already in the file; a
// single InjectPermHook must leave one, not two.
//
// The assertion is == 1, not >= 1: the pre-fix build satisfies >= 1 too, so a
// lower bound would be green on both sides of the change and measure nothing.
func TestInjectPermHook_StrippedEntryIsRecognisedNotDuplicated(t *testing.T) {
	home := sandboxHome(t)
	hookPath := tetherHookPath(home)

	writeSettings(t, home, settingsWithPreToolUse(strippedEntry(hookPath)))

	if err := InjectPermHook(hookPath); err != nil {
		t.Fatalf("InjectPermHook: %v", err)
	}

	list := readPreToolUse(t, home)
	if len(list) != 1 {
		t.Errorf("PreToolUse has %d entries after one inject over a sentinel-free tether entry, want exactly 1 "+
			"(this is the growth mechanism: the stripped entry was not recognised as tether's, "+
			"so it was kept and a second one appended)", len(list))
	}
	if got := countNaming(list, hookPath); got != 1 {
		t.Errorf("%d entries name %s, want exactly 1", got, hookPath)
	}
}

// T2. The negative control that gives T1 its meaning.
//
// An entry in the SAME shape as tether's but naming somebody else's command must
// survive untouched. Without this pair, a "fix" that simply truncated PreToolUse
// to a single entry would pass T1 while silently deleting the operator's hooks.
func TestInjectPermHook_ForeignEntryOfTheSameShapeSurvives(t *testing.T) {
	home := sandboxHome(t)
	hookPath := tetherHookPath(home)
	const foreign = "/usr/local/bin/somebody-elses-hook"

	writeSettings(t, home, settingsWithPreToolUse(strippedEntry(foreign)))

	if err := InjectPermHook(hookPath); err != nil {
		t.Fatalf("InjectPermHook: %v", err)
	}

	list := readPreToolUse(t, home)
	if len(list) != 2 {
		t.Errorf("PreToolUse has %d entries, want exactly 2 (the foreign hook plus tether's)", len(list))
	}
	if got := countNaming(list, foreign); got != 1 {
		t.Errorf("%d entries name the foreign command %s, want exactly 1 — "+
			"a fix must not adopt or delete a hook it does not own", got, foreign)
	}
	if got := countNaming(list, hookPath); got != 1 {
		t.Errorf("%d entries name %s, want exactly 1", got, hookPath)
	}
}

// T3. The mechanism, at the scale that produced the field measurement.
//
// Five start/strip cycles from an empty file. The pre-fix build leaves five
// entries — the count is PROPORTIONAL to the loop, which is what identifies this
// as the process that reached 9 and 18 rather than an off-by-one.
func TestInjectPermHook_StripAndReinjectDoesNotAccumulate(t *testing.T) {
	home := sandboxHome(t)
	hookPath := tetherHookPath(home)

	writeSettings(t, home, map[string]any{})

	const cycles = 5
	for i := 0; i < cycles; i++ {
		if err := InjectPermHook(hookPath); err != nil {
			t.Fatalf("InjectPermHook cycle %d: %v", i, err)
		}
		stripSentinels(t, home)
	}

	list := readPreToolUse(t, home)
	if len(list) != 1 {
		t.Errorf("PreToolUse has %d entries after %d start/strip cycles, want exactly 1 "+
			"(one entry per daemon start is the observed 9-then-18 growth)", len(list), cycles)
	}
	if got := countNaming(list, hookPath); got != 1 {
		t.Errorf("%d entries name %s, want exactly 1", got, hookPath)
	}
}

// T4. The half that is easy to miss: the stripped entry is also UN-REMOVABLE.
//
// Inject and remove share one entry-recognising helper, so a blind read there
// leaves graceful shutdown unable to clean up as well. Worse than a no-op: with
// the stripped entry kept, the filtered list is non-empty, the "nothing left,
// delete the key" branch never fires, and shutdown REWRITES the sediment back to
// disk. (Pre-fix that helper was removeManaged, reading only the sentinel.)
func TestRemovePermHook_RemovesStrippedTetherEntry(t *testing.T) {
	home := sandboxHome(t)
	hookPath := tetherHookPath(home)

	writeSettings(t, home, settingsWithPreToolUse(strippedEntry(hookPath)))

	if err := RemovePermHook(); err != nil {
		t.Fatalf("RemovePermHook: %v", err)
	}

	list := readPreToolUse(t, home)
	if got := countNaming(list, hookPath); got != 0 {
		t.Errorf("%d entries still name %s after RemovePermHook, want 0 — "+
			"a sentinel-free tether entry survives graceful shutdown and is rewritten to disk", got, hookPath)
	}
}

// mixedEntry builds one entry whose nested hooks array carries somebody else's
// command AND tether's — the shape that appears if the co-writer ever merges
// hooks sharing a matcher, "*" being the obvious matcher for both of them.
func mixedEntry(foreign, tetherCmd string) map[string]any {
	return map[string]any{
		"matcher": "*",
		"hooks": []any{
			map[string]any{"type": "command", "command": foreign},
			map[string]any{"type": "command", "command": tetherCmd},
		},
	}
}

// T5. Identity must look at EVERY nested command, not the first.
//
// A first-only reading would not see tether's hook behind the foreign one, so
// each cycle would append another entry — the same defect one level down. Run as
// a repeat so the assertion discriminates: on the pre-fix build the count grows
// with the loop (1 + 3 = 4), while a correct build holds at two entries however
// many times the daemon restarts.
func TestInjectPermHook_TetherHookNestedBehindForeignHookIsFound(t *testing.T) {
	home := sandboxHome(t)
	hookPath := tetherHookPath(home)
	const foreign = "/usr/local/bin/somebody-elses-hook"

	writeSettings(t, home, settingsWithPreToolUse(mixedEntry(foreign, hookPath)))

	const cycles = 3
	for i := 0; i < cycles; i++ {
		if err := InjectPermHook(hookPath); err != nil {
			t.Fatalf("InjectPermHook cycle %d: %v", i, err)
		}
		stripSentinels(t, home)
	}

	list := readPreToolUse(t, home)
	if len(list) != 2 {
		t.Errorf("PreToolUse has %d entries after %d cycles over a mixed entry, want exactly 2 "+
			"(the foreign hook, plus one of tether's)", len(list), cycles)
	}
	if got := countNaming(list, hookPath); got != 1 {
		t.Errorf("%d entries name %s, want exactly 1 — tether's hook was not recognised "+
			"when it sat behind another command in the same entry", got, hookPath)
	}
	// The data-preservation half. Recognising the entry must not become a licence
	// to delete it: whole-entry removal would drop the operator's hook here, which
	// is a worse failure than the duplication being fixed.
	if got := countNaming(list, foreign); got != 1 {
		t.Errorf("%d entries name the foreign command %s, want exactly 1 — "+
			"pruning tether's nested hook must leave the co-resident hook intact", got, foreign)
	}
}

// T6. The removal side of T5: shutdown prunes tether's nested hook out of a
// shared entry and leaves the entry, and the foreign hook in it, standing.
func TestRemovePermHook_PrunesOnlyTetherHookFromSharedEntry(t *testing.T) {
	home := sandboxHome(t)
	hookPath := tetherHookPath(home)
	const foreign = "/usr/local/bin/somebody-elses-hook"

	writeSettings(t, home, settingsWithPreToolUse(mixedEntry(foreign, hookPath)))

	if err := RemovePermHook(); err != nil {
		t.Fatalf("RemovePermHook: %v", err)
	}

	list := readPreToolUse(t, home)
	if got := countNaming(list, hookPath); got != 0 {
		t.Errorf("%d entries still name %s after RemovePermHook, want 0", got, hookPath)
	}
	if got := countNaming(list, foreign); got != 1 {
		t.Errorf("%d entries name the foreign command %s after RemovePermHook, want exactly 1 — "+
			"removal must not take a co-resident hook with it", got, foreign)
	}
}

// A sentinel-marked entry stays removable whole even when its command no longer
// matches — an older build's binary name, or a $HOME that moved. This is the one
// job the sentinel still does that the path cannot, and it is the pre-existing
// behaviour this change must not regress.
func TestRemovePermHook_SentinelEntryWithStaleCommandIsStillRemoved(t *testing.T) {
	home := sandboxHome(t)

	stale := strippedEntry(filepath.Join(home, ".tether", "bin", "tether-permission-hook-from-an-older-build"))
	stale[TetherManagedKey] = true
	writeSettings(t, home, settingsWithPreToolUse(stale))

	if err := RemovePermHook(); err != nil {
		t.Fatalf("RemovePermHook: %v", err)
	}

	if list := readPreToolUse(t, home); len(list) != 0 {
		t.Errorf("PreToolUse has %d entries after removing a sentinel-marked entry whose command "+
			"no longer matches, want 0", len(list))
	}
}

// W1a. The shape the surgery was written for, on the branch that used to skip it.
//
// The operator opens settings.json, sees tether's matcher "*" block sitting there
// — sentinel and all — and adds their own hook into THAT block's hooks array
// rather than writing a second matcher "*" entry. That is the natural edit when a
// matcher "*" block already exists, and it produces a sentinel-marked entry with a
// co-resident foreign hook.
//
// Before this test existed, stripTetherFromHookEntry returned false on the
// sentinel before ever reaching the pruning loop, so this entry was deleted whole
// and the operator's hook went with it — on every daemon start and every graceful
// shutdown, silently. The assertion that discriminates is the FOREIGN count, not
// tether's: a whole-entry delete gets tether's count right for the wrong reason.
func TestRemovePermHook_SentinelEntrySharedWithForeignHookIsPrunedNotDestroyed(t *testing.T) {
	home := sandboxHome(t)
	hookPath := tetherHookPath(home)
	const foreign = "/usr/local/bin/somebody-elses-hook"

	shared := mixedEntry(foreign, hookPath)
	shared[TetherManagedKey] = true
	writeSettings(t, home, settingsWithPreToolUse(shared))

	if err := RemovePermHook(); err != nil {
		t.Fatalf("RemovePermHook: %v", err)
	}

	list := readPreToolUse(t, home)
	if got := countNaming(list, foreign); got != 1 {
		t.Errorf("%d entries name the foreign command %s after RemovePermHook, want exactly 1 — "+
			"a sentinel-marked entry carrying a co-resident hook was deleted whole instead of "+
			"being pruned, destroying a hook that is not tether's", got, foreign)
	}
	if got := countNaming(list, hookPath); got != 0 {
		t.Errorf("%d entries still name %s after RemovePermHook, want 0", got, hookPath)
	}
}

// W1b. The other half: pruning a marked entry must also take our MARK off it.
//
// Three daemon starts with no co-writer in between. If the sentinel is left on the
// entry that survives the pruning, then on the next pass no command matches, the
// entry is still marked, and the stale-command rule deletes it whole — so the
// data loss is not prevented, only postponed by one start. This test fails on the
// pre-fix build at cycle 1 (entry destroyed immediately) and on a
// forgot-to-clear-the-sentinel build at cycle 2, which is why it runs a loop
// rather than a single call.
func TestInjectPermHook_SentinelEntrySharedWithForeignHookSurvivesRepeatedStarts(t *testing.T) {
	home := sandboxHome(t)
	hookPath := tetherHookPath(home)
	const foreign = "/usr/local/bin/somebody-elses-hook"

	shared := mixedEntry(foreign, hookPath)
	shared[TetherManagedKey] = true
	writeSettings(t, home, settingsWithPreToolUse(shared))

	const cycles = 3
	for i := 0; i < cycles; i++ {
		if err := InjectPermHook(hookPath); err != nil {
			t.Fatalf("InjectPermHook cycle %d: %v", i, err)
		}
	}

	list := readPreToolUse(t, home)
	if got := countNaming(list, foreign); got != 1 {
		t.Errorf("%d entries name the foreign command %s after %d daemon starts, want exactly 1 — "+
			"the co-resident hook did not survive re-injection over a sentinel-marked shared entry",
			got, foreign, cycles)
	}
	if got := countNaming(list, hookPath); got != 1 {
		t.Errorf("%d entries name %s after %d daemon starts, want exactly 1", got, hookPath, cycles)
	}
	if len(list) != 2 {
		t.Errorf("PreToolUse has %d entries after %d daemon starts, want exactly 2 "+
			"(the operator's pruned entry, plus tether's fresh one)", len(list), cycles)
	}
}

// N2. A narrowed matcher on TETHER'S OWN entry is deliberately not preserved.
//
// This records a policy rather than an accident. The permission hook is the gate
// that mediates every tool call the daemon brokers; a matcher narrowed from "*" to
// "Bash" is not a preference tether should honour but a gate that has silently
// stopped firing for Edit, Write and everything else. Re-asserting "*" is tether
// repairing its own configuration, the same way it rewrites a stale command path.
// Preserving it is also not well defined: once the sentinel is gone and the entry
// is shared, there is no way to tell whether a narrowing was aimed at tether's
// hook or at the co-resident one.
//
// The boundary is the second half of the test: the policy applies to entries that
// are ours. A foreign entry's matcher is none of tether's business, narrowed or
// not.
func TestInjectPermHook_NarrowedMatcherIsResetOnOursAndLeftOnTheirs(t *testing.T) {
	home := sandboxHome(t)
	hookPath := tetherHookPath(home)
	const foreign = "/usr/local/bin/somebody-elses-hook"

	theirs := strippedEntry(foreign)
	theirs["matcher"] = "Bash"
	ours := strippedEntry(hookPath)
	ours["matcher"] = "Bash"
	writeSettings(t, home, settingsWithPreToolUse(theirs, ours))

	if err := InjectPermHook(hookPath); err != nil {
		t.Fatalf("InjectPermHook: %v", err)
	}

	list := readPreToolUse(t, home)
	if len(list) != 2 {
		t.Fatalf("PreToolUse has %d entries, want exactly 2 (theirs, plus tether's re-injected one)", len(list))
	}
	for _, h := range list {
		hm, isObj := h.(map[string]any)
		if !isObj {
			t.Fatalf("PreToolUse entry is %T, want an object", h)
		}
		switch {
		case countNaming([]any{hm}, hookPath) == 1:
			if hm["matcher"] != "*" {
				t.Errorf("tether's own entry has matcher %q, want \"*\" — a narrowed matcher on "+
					"tether's gate is reset, not preserved", hm["matcher"])
			}
		case countNaming([]any{hm}, foreign) == 1:
			if hm["matcher"] != "Bash" {
				t.Errorf("the foreign entry has matcher %q, want \"Bash\" — tether must not "+
					"rewrite the matcher of a hook it does not own", hm["matcher"])
			}
		default:
			t.Errorf("unexpected PreToolUse entry: %v", hm)
		}
	}
}

// N3. Clearing the last hook must not leave a {"hooks": {}} husk behind.
// removeMCPServerByName has always deleted "mcpServers" when it empties; this
// pins the same for "hooks". A sibling key under "hooks" must keep it alive.
func TestRemovePermHook_LeavesNoEmptyHooksHusk(t *testing.T) {
	t.Run("last entry removed", func(t *testing.T) {
		home := sandboxHome(t)
		hookPath := tetherHookPath(home)
		writeSettings(t, home, settingsWithPreToolUse(strippedEntry(hookPath)))

		if err := RemovePermHook(); err != nil {
			t.Fatalf("RemovePermHook: %v", err)
		}

		var settings map[string]any
		data, err := os.ReadFile(filepath.Join(home, ".claude", "settings.json"))
		if err != nil {
			t.Fatalf("read settings: %v", err)
		}
		if err := json.Unmarshal(data, &settings); err != nil {
			t.Fatalf("parse settings: %v", err)
		}
		if v, has := settings["hooks"]; has {
			t.Errorf("settings still carries hooks = %v after the last entry was removed, "+
				"want the key gone", v)
		}
	})

	t.Run("sibling hook kind survives", func(t *testing.T) {
		home := sandboxHome(t)
		hookPath := tetherHookPath(home)
		writeSettings(t, home, map[string]any{
			"hooks": map[string]any{
				"PreToolUse":   []any{strippedEntry(hookPath)},
				"SessionStart": []any{strippedEntry("/usr/local/bin/somebody-elses-hook")},
			},
		})

		if err := RemovePermHook(); err != nil {
			t.Fatalf("RemovePermHook: %v", err)
		}

		var settings map[string]any
		data, err := os.ReadFile(filepath.Join(home, ".claude", "settings.json"))
		if err != nil {
			t.Fatalf("read settings: %v", err)
		}
		if err := json.Unmarshal(data, &settings); err != nil {
			t.Fatalf("parse settings: %v", err)
		}
		hooks, ok := settings["hooks"].(map[string]any)
		if !ok {
			t.Fatalf("hooks = %v, want it kept alive by the SessionStart sibling", settings["hooks"])
		}
		if _, has := hooks["SessionStart"]; !has {
			t.Errorf("SessionStart was dropped; tether must only clear the hook kinds it writes")
		}
		if _, has := hooks["PreToolUse"]; has {
			t.Errorf("PreToolUse survived, want it removed once tether's only entry was cleared")
		}
	})
}

// T4's negative control. Removal must stay narrow: a third-party hook is not
// tether's to delete, however much it resembles one.
func TestRemovePermHook_LeavesForeignEntryAlone(t *testing.T) {
	home := sandboxHome(t)
	const foreign = "/usr/local/bin/somebody-elses-hook"

	writeSettings(t, home, settingsWithPreToolUse(strippedEntry(foreign)))

	if err := RemovePermHook(); err != nil {
		t.Fatalf("RemovePermHook: %v", err)
	}

	list := readPreToolUse(t, home)
	if got := countNaming(list, foreign); got != 1 {
		t.Errorf("%d entries name the foreign command %s after RemovePermHook, want exactly 1", got, foreign)
	}
}
