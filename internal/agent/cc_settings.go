package agent

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// TetherManagedKey is the sentinel field injected into tether-owned hook entries.
//
// It is written as a fast path for recognising our own entries, and it is NOT
// load-bearing. It cannot be: settings.json is co-written by Claude Code itself,
// whose typed hook-entry schema rebuilds each {matcher, hooks} object literally
// and therefore drops any key it has not heard of. The entry comes back
// otherwise byte-identical, sentinel gone. (Contrast mcpServers, which cc passes
// through by spread — that is why an mcpServers entry keeps its sentinel while a
// hook entry loses it, an asymmetry that was observed before it was explained.)
//
// Treating this key as tether's identity is what produced tether#166: a stripped
// entry was neither recognised on inject (so a second was appended, every daemon
// start, measured at 9 then 18) nor on remove (so graceful shutdown rewrote the
// sediment to disk instead of clearing it). Identity is therefore derived from a
// field cc preserves — the hook command path — with this key as an accelerator
// only. See stripTetherFromHookEntry.
const TetherManagedKey = "_tether_managed"

// permHookBaseName / permHookDirName spell the path internal/server builds and
// passes to InjectPermHook. See permHookPath for why the literal lives here too.
const (
	permHookDirName  = ".tether"
	permHookBaseName = "tether-permission-hook"
)

// HookEntryCommands returns every command named in a Pre/PostToolUse entry's
// nested "hooks" array, in order, with blanks dropped.
//
// Commands are returned whole rather than split on whitespace, and trimmed:
// what tether writes is a single absolute path, $HOME may legitimately contain a
// space, and a trailing newline from a hand-edited file is not part of the path.
// (Those two reasons are lifted verbatim from internal/doctor's private
// managedHookCommand, which this function generalises.)
//
// EVERY command, not just the first: an entry shaped hooks:[{other},{tether}] is
// still tether's entry, and a first-only reading would fail to recognise it and
// duplicate it — the very bug this file is fixing, one level down.
//
// This is exported ahead of its consumer, which is tether#175. internal/doctor
// carries the same sentinel-only blind spot, and closing it there is two edits:
// widening the sentinel-only filter in checkCCSettingsHooks, and reducing the
// private managedHookCommand to a two-line wrapper over this
// (`if c := HookEntryCommands(e); len(c) > 0 { return c[0], true }` — measured
// behaviour-preserving across 16 entry shapes). internal/doctor is outside this
// change's declared file scope, so until tether#175 lands this function has no
// caller outside the package.
func HookEntryCommands(entry map[string]any) []string {
	nested, _ := entry["hooks"].([]any)
	var out []string
	for _, h := range nested {
		hm, isObj := h.(map[string]any)
		if !isObj {
			continue
		}
		if command, _ := hm["command"].(string); strings.TrimSpace(command) != "" {
			out = append(out, strings.TrimSpace(command))
		}
	}
	return out
}

// permHookPath names $HOME/.tether/bin/tether-permission-hook WITHOUT creating
// any directory.
//
// This duplicates a path literal owned by internal/server (lifecycle.go Step 3,
// via the unexported tetherBinDir). That is stated plainly rather than hidden:
// it is a real cost of this fix and code_review should weigh it.
//
// Three things make it the least-bad option here. First, RemovePermHook takes no
// arguments and its only caller is lifecycle.go:538 — which is outside this
// change's declared file scope, so threading the path through the signature is
// not available. Second, internal/doctor already made exactly this trade for
// exactly this path, with the rationale recorded at hookHashPath: server's
// helper is unexported AND creates the directory as a side effect, which a
// caller that merely wants to NAME the file must not do. Third, the alternative
// of caching the injected path in a package variable would make removal depend
// on inject having run in the same process, so a daemon that crashed and
// restarted could never clean up after its predecessor — trading a shared-file
// identity bug for a process-local one, which is the same mistake in a smaller
// scope.
//
// The proper fix is a single exported path helper all three packages call, so
// that nothing short of a compile error can let the definitions drift. That is a
// three-package change and is tracked as tether#175. It deserves more than the
// usual duplication complaint: this copy drives a DELETE decision on a file tether
// does not own, so a silent drift both reinstates tether#166 and leaves doctor
// telling the operator to run the very command that grows the file.
func permHookPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, permHookDirName, "bin", permHookBaseName), nil
}

// hookEntryNamesCommand reports whether any command in the entry resolves to
// want. Both sides are cleaned so that a hand-edited "/home/me//.tether/bin/x"
// still matches.
//
// Two residuals are left open on purpose. Both are stated here so neither is
// mistaken for an oversight, and both fail in the same safe direction — toward a
// duplicate entry, never toward deleting a hook that is not ours.
//
// First, ARGUMENTS. Comparison is against the command WHOLE, never split on
// whitespace: $HOME may legitimately contain a space, and what tether writes is a
// single absolute path. So an entry whose command carries trailing arguments, or
// wraps ours in `sh -c`, is not recognised. tether never writes such a command, so
// that shape can only come from a hand edit. (Measured: a hand-edited wrapper does
// NOT re-open the growth of tether#166 — tether's own entry is always
// path-identifiable, so five start/strip cycles hold at two entries.)
//
// Second, $HOME RESPELLED. filepath.Clean is lexical, so if $HOME is spelled one
// way on one run and another on the next — a symlink, then its realpath — the
// existing entry is not recognised and a second is appended; RemovePermHook then
// leaves one of them behind permanently. filepath.EvalSymlinks would close this
// and is deliberately NOT used: it stats the path, so it fails precisely on the
// host state internal/doctor documents, where the daemon was SIGKILLed and the
// hook binary has since been cleaned up. Clean is the right floor.
func hookEntryNamesCommand(entry map[string]any, want string) bool {
	if want == "" {
		return false
	}
	target := filepath.Clean(want)
	for _, command := range HookEntryCommands(entry) {
		if filepath.Clean(command) == target {
			return true
		}
	}
	return false
}

// stripTetherFromHookEntry removes tether's own hook from one Pre/PostToolUse
// entry, in place, and reports whether the entry should survive at all.
//
// The rule is a 2x2 over "does a nested command name our hook path" — the durable
// identity, because cc preserves commands — and "is our sentinel on the entry" —
// the accelerator, which cc strips. The PATH is tested first, because the path is
// the half that survives:
//
//  1. A nested command names our hook — prune SURGICALLY. Only the MATCHING
//     nested hooks go; anything else in the entry stays, and the entry itself is
//     dropped only if nothing is left. The sentinel does not change this, and that
//     is the whole point of testing the path first. A whole-entry delete would be
//     wrong here, and not hypothetically: matcher "*" is the obvious matcher for
//     anyone writing a PreToolUse hook, so an operator who opens settings.json,
//     finds tether's matcher "*" block already sitting there and adds their own
//     hook INTO it — the natural edit — produces exactly hooks:[{ours},{theirs}]
//     on a sentinel-marked entry. Deleting that whole would silently drop their
//     hook on every daemon start and every graceful shutdown, which is a worse bug
//     than the duplication being fixed here.
//
//     (tether#166 code_review W1. An earlier draft returned false on the sentinel
//     BEFORE reaching this loop, so the surgery it documented was reachable in
//     only one of the two branches — a comment asserting a property the code held
//     for half its inputs.)
//
//     When an entry survives the pruning, our sentinel is deleted along with our
//     hook. What is left is not ours, and leaving the mark on it would merely
//     defer the data loss by one daemon start: on the next pass no command would
//     match, the entry would still be marked, and rule 2 would delete it — foreign
//     hook and all.
//
//  2. No command matches but the sentinel is present — drop the entry WHOLE. This
//     is the one job the path cannot do: an entry we marked but whose command no
//     longer names our hook (an older build's binary name, a $HOME that moved, or
//     a permHookPath error that left hookPath empty) must still be cleanable, and
//     only the sentinel can say so. Pinned by
//     TestRemovePermHook_SentinelEntryWithStaleCommandIsStillRemoved.
//
//  3. Neither — not ours, leave it completely alone.
//
// "Any nested command", not the first: hooks:[{theirs},{ours}] is still an entry
// that contains ours, and a first-only reading would fail to see it and append a
// duplicate — the same defect one level down.
func stripTetherFromHookEntry(entry map[string]any, hookPath string) (keepEntry bool) {
	if !hookEntryNamesCommand(entry, hookPath) {
		// Rule 2 and rule 3. Nothing here is ours by path, so the sentinel is the
		// only remaining claim — and it claims the entry whole.
		managed, _ := entry[TetherManagedKey].(bool)
		return !managed
	}

	target := filepath.Clean(hookPath)
	nested, _ := entry["hooks"].([]any)
	kept := make([]any, 0, len(nested))
	for _, h := range nested {
		if hm, isObj := h.(map[string]any); isObj {
			if c, _ := hm["command"].(string); filepath.Clean(strings.TrimSpace(c)) == target {
				continue
			}
		}
		kept = append(kept, h)
	}
	if len(kept) == 0 {
		return false // the entry existed only to carry our hook
	}
	entry["hooks"] = kept
	// See rule 1: the survivor is not ours, so our mark must not survive on it.
	delete(entry, TetherManagedKey)
	return true
}

// InjectPermHook merges the tether-managed PreToolUse hook entry into
// ~/.claude/settings.json, preserving any existing user hooks.
// Uses atomic rename to avoid partial writes (D-05b §5.1, §10 row 1).
//
// It takes the hook path and NOTHING ELSE, deliberately. It used to also accept
// a daemonEndpoint that no line of the body ever read, and that dead parameter
// was actively misleading: it made settings.json look like the channel the
// permission endpoint travels on. It is not — the endpoint and the
// TETHER_DAEMON_MANAGED mark both reach the hook through the ENVIRONMENT of the
// cc subprocess, built by cchook.Gate.Env() at each spawn path. Reading the
// old signature as the truth is how "the shell pane cannot see the endpoint"
// became a believable diagnosis of a hole that was somewhere else entirely
// (tether#149).
func InjectPermHook(hookBinPath string) error {
	path, err := ccSettingsPath()
	if err != nil {
		return err
	}

	settings, err := loadSettings(path)
	if err != nil {
		settings = map[string]any{}
	}

	// Remove any stale tether entries before adding a fresh one.
	//
	// hookBinPath, not permHookPath(): the argument is authoritative for what this
	// call is about to write, so "remove what I am about to add" is exactly the
	// right identity here, and a test harness aimed at a temp path stays honest.
	removeTetherHookEntries(settings, hookBinPath)

	hooks := getHookList(settings, "PreToolUse")
	hooks = append(hooks, map[string]any{
		TetherManagedKey: true,
		"hooks": []any{map[string]any{
			"type":    "command",
			"command": hookBinPath,
		}},
		"matcher": "*",
	})
	setHookList(settings, "PreToolUse", hooks)

	return saveSettings(path, settings)
}

// RemovePermHook removes all tether PreToolUse entries from settings.json.
// Called on graceful shutdown (D-05b §5.2).
//
// It recomputes the hook path instead of being handed one because its caller
// (internal/server/lifecycle.go:538) passes no arguments. A permHookPath error is
// not fatal: removal then proceeds on the sentinel alone, which still clears
// entries this process wrote itself in the common case.
func RemovePermHook() error {
	path, err := ccSettingsPath()
	if err != nil {
		return err
	}
	settings, err := loadSettings(path)
	if err != nil {
		return nil // file absent = nothing to clean up
	}
	hookPath, err := permHookPath()
	if err != nil {
		hookPath = ""
	}
	removeTetherHookEntries(settings, hookPath)
	return saveSettings(path, settings)
}

// removeTetherHookEntries clears tether's hook out of every Pre/PostToolUse
// entry, per stripTetherFromHookEntry.
//
// PostToolUse is swept as well as PreToolUse even though inject only ever writes
// PreToolUse — that predates this change and is kept: a build that once wrote
// PostToolUse must still be cleanable by a build that does not.
//
// Note what removing the RIGHT entries unlocks: with the stripped entry finally
// gone, filtered goes empty and the deleteHookList branch below fires for the
// first time. Before this fix that branch was dead whenever a stripped entry was
// present — the entry was kept, filtered stayed non-empty, and shutdown wrote
// the accumulated sediment straight back to disk instead of clearing it.
func removeTetherHookEntries(settings map[string]any, hookPath string) {
	for _, kind := range []string{"PreToolUse", "PostToolUse"} {
		hooks := getHookList(settings, kind)
		filtered := hooks[:0]
		for _, h := range hooks {
			if hm, ok := h.(map[string]any); ok {
				if !stripTetherFromHookEntry(hm, hookPath) {
					continue
				}
			}
			filtered = append(filtered, h)
		}
		if len(filtered) == 0 {
			deleteHookList(settings, kind)
		} else {
			setHookList(settings, kind, filtered)
		}
	}
}

func ccSettingsPath() (string, error) {
	// Claude Code reads ~/.claude/settings.json (user-level), NOT
	// ~/.config/claude/settings.json. The latter is unread; writing there
	// makes the PreToolUse hook silently no-op. Verified against running
	// `claude --setting-sources=project,user,local` invocation.
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(home, ".claude")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	return filepath.Join(dir, "settings.json"), nil
}

func loadSettings(path string) (map[string]any, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var m map[string]any
	return m, json.Unmarshal(data, &m)
}

// saveSettings serialises the whole map back over the file via a temp file and an
// atomic rename.
//
// Two hazards live here, both pre-existing, both left for tether#176 rather than
// widened into this change. First, every caller does an UNLOCKED load -> mutate ->
// rename on a file Claude Code writes concurrently — the co-writer of tether#166 is
// now confirmed, so the lost-update window is a measured property of the system and
// not a theoretical one, and the rename replaces the file whole. Second, the
// rewrite is UNCONDITIONAL: RemovePermHook re-serialises a settings.json that
// contains nothing of tether's at all (measured 199 bytes in, 312 bytes out — keys
// reordered alphabetically, indentation normalised), so every daemon start and
// every graceful shutdown churns the operator's file whether or not anything of
// ours was ever in it.
func saveSettings(path string, settings map[string]any) error {
	b, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return fmt.Errorf("write settings tmp: %w", err)
	}
	return os.Rename(tmp, path)
}

func getHookList(settings map[string]any, kind string) []any {
	hooks, _ := settings["hooks"].(map[string]any)
	if hooks == nil {
		return nil
	}
	list, _ := hooks[kind].([]any)
	return list
}

func setHookList(settings map[string]any, kind string, list []any) {
	hooks, _ := settings["hooks"].(map[string]any)
	if hooks == nil {
		hooks = map[string]any{}
		settings["hooks"] = hooks
	}
	hooks[kind] = list
}

// deleteHookList drops the whole Pre/PostToolUse key, and drops the enclosing
// "hooks" object too once it is empty — otherwise clearing the last hook leaves a
// {"hooks": {}} husk in the operator's file. removeMCPServerByName has always done
// the same for "mcpServers"; this only makes the two sides of the file agree.
func deleteHookList(settings map[string]any, kind string) {
	hooks, _ := settings["hooks"].(map[string]any)
	if hooks == nil {
		return
	}
	delete(hooks, kind)
	if len(hooks) == 0 {
		delete(settings, "hooks")
	}
}

// InjectMCPServer adds an MCP server entry under the given name key to
// ~/.claude/settings.json.  name is typically "tether" for the global
// singleton or "tether-<slug>" for a per-task instance.
//
// The function removes any existing entry with the same name first so the
// operation is idempotent.  token is the per-daemon bearer token.
func InjectMCPServer(port int, token, name string) error {
	path, err := ccSettingsPath()
	if err != nil {
		return err
	}
	settings, err := loadSettings(path)
	if err != nil {
		settings = map[string]any{}
	}
	removeMCPServerByName(settings, name)

	mcpServers, _ := settings["mcpServers"].(map[string]any)
	if mcpServers == nil {
		mcpServers = map[string]any{}
		settings["mcpServers"] = mcpServers
	}
	mcpServers[name] = map[string]any{
		TetherManagedKey: true,
		"type":           "http",
		"url":            fmt.Sprintf("http://127.0.0.1:%d/mcp", port),
		"headers": map[string]any{
			"Authorization": "Bearer " + token,
		},
	}
	return saveSettings(path, settings)
}

// RemoveMCPServer removes the named tether-managed mcpServers entry from
// settings.json.  name should match what was passed to InjectMCPServer.
func RemoveMCPServer(name string) error {
	path, err := ccSettingsPath()
	if err != nil {
		return err
	}
	settings, err := loadSettings(path)
	if err != nil {
		return nil // absent = nothing to clean up
	}
	removeMCPServerByName(settings, name)
	return saveSettings(path, settings)
}

// removeManagedMCPServer removes ALL tether-managed mcpServers entries
// (used only by the legacy path; prefer removeMCPServerByName for targeted removal).
func removeManagedMCPServer(settings map[string]any) {
	mcpServers, _ := settings["mcpServers"].(map[string]any)
	if mcpServers == nil {
		return
	}
	for k, v := range mcpServers {
		if m, ok := v.(map[string]any); ok {
			if managed, _ := m[TetherManagedKey].(bool); managed {
				delete(mcpServers, k)
			}
		}
	}
	if len(mcpServers) == 0 {
		delete(settings, "mcpServers")
	}
}

// removeMCPServerByName removes the single named entry if it carries
// TetherManagedKey=true.  Does nothing if absent or not managed.
func removeMCPServerByName(settings map[string]any, name string) {
	mcpServers, _ := settings["mcpServers"].(map[string]any)
	if mcpServers == nil {
		return
	}
	if m, ok := mcpServers[name].(map[string]any); ok {
		if managed, _ := m[TetherManagedKey].(bool); managed {
			delete(mcpServers, name)
		}
	}
	if len(mcpServers) == 0 {
		delete(settings, "mcpServers")
	}
}
