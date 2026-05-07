package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestResolveCCSettings_EnvWins covers the highest-precedence rung: a
// non-empty TETHER_HOOK_SETTINGS env var short-circuits, regardless of
// whether $HOME/.tether/cc-settings/settings.json exists.
func TestResolveCCSettings_EnvWins(t *testing.T) {
	t.Setenv("TETHER_HOOK_SETTINGS", "/some/explicit/path.json")
	got := resolveCCSettings(func() (string, error) {
		// Also create a real default file to prove env still wins.
		return t.TempDir(), nil
	})
	if got != "/some/explicit/path.json" {
		t.Errorf("env should win; got %q", got)
	}
}

// TestResolveCCSettings_DefaultPathExists exercises the fallback chain:
// no env var, but the canonical $HOME/.tether/cc-settings/settings.json
// is present on disk → return that path.
func TestResolveCCSettings_DefaultPathExists(t *testing.T) {
	t.Setenv("TETHER_HOOK_SETTINGS", "")
	home := t.TempDir()
	dir := filepath.Join(home, ".tether", "cc-settings")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(dir, "settings.json")
	if err := os.WriteFile(want, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	got := resolveCCSettings(func() (string, error) { return home, nil })
	if got != want {
		t.Errorf("default path: got %q, want %q", got, want)
	}
}

// TestResolveCCSettings_DefaultMissing covers the graceful-fallback path:
// no env, default path doesn't exist on disk → empty string. Empty must
// be interpreted by callers as "do not pass --settings to cc".
func TestResolveCCSettings_DefaultMissing(t *testing.T) {
	t.Setenv("TETHER_HOOK_SETTINGS", "")
	home := t.TempDir() // tempdir is empty — no settings.json under it
	got := resolveCCSettings(func() (string, error) { return home, nil })
	if got != "" {
		t.Errorf("missing default should resolve to empty; got %q", got)
	}
}

// TestResolveCCSettings_HomeProviderError: a homeProvider that errs out
// should cause empty (rather than the resolver propagating the error).
// Empty is the safe fallback — we don't want a CLI crash because the OS
// can't introspect $HOME.
func TestResolveCCSettings_HomeProviderError(t *testing.T) {
	t.Setenv("TETHER_HOOK_SETTINGS", "")
	got := resolveCCSettings(func() (string, error) { return "", errors.New("no home") })
	if got != "" {
		t.Errorf("homeProvider error should resolve to empty; got %q", got)
	}
}

// TestResolveCCSettings_HomeProviderEmpty: empty home with nil error
// also resolves to empty (defensive — distinct path from error).
func TestResolveCCSettings_HomeProviderEmpty(t *testing.T) {
	t.Setenv("TETHER_HOOK_SETTINGS", "")
	got := resolveCCSettings(func() (string, error) { return "", nil })
	if got != "" {
		t.Errorf("empty home should resolve to empty; got %q", got)
	}
}

// TestResolveCCSettings_EnvHonoredEvenWhenMissing: the env-var path is
// honored verbatim, no stat. Operators may legitimately pre-create the
// file out-of-band; "your env points nowhere" is loud but actionable —
// silent drop would be worse.
func TestResolveCCSettings_EnvHonoredEvenWhenMissing(t *testing.T) {
	t.Setenv("TETHER_HOOK_SETTINGS", "/definitely/not/a/real/path.json")
	got := resolveCCSettings(func() (string, error) { return t.TempDir(), nil })
	if got != "/definitely/not/a/real/path.json" {
		t.Errorf("env should be honored verbatim; got %q", got)
	}
}

// TestRunResume_ExecPassesSettings is the integration test from the spec:
// stub the execve syscall, invoke runResume with a real default
// settings.json on disk, and assert the captured argv contains
// `--settings <expected-path>`.
//
// We can't let the real syscall.Exec fire (it'd replace the test
// process); the package-level execClaudeSyscall var lets tests substitute
// a recorder.
func TestRunResume_ExecPassesSettings(t *testing.T) {
	root := t.TempDir()
	// Real cwd that ExecClaude will chdir into; pair it with a bucket
	// derived from EncodeBucket so ResolveSession can find the jsonl.
	cwd := filepath.Join(t.TempDir(), "work")
	if err := os.MkdirAll(cwd, 0o755); err != nil {
		t.Fatal(err)
	}
	bucket := EncodeBucket(cwd)
	if err := os.MkdirAll(filepath.Join(root, bucket), 0o755); err != nil {
		t.Fatal(err)
	}
	sid := "settings-on-sid"
	jsonl := filepath.Join(root, bucket, sid+".jsonl")
	if err := os.WriteFile(jsonl, []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Stand up a fake settings.json under a fake $HOME that the resolver
	// can find via defaultHomeProvider() — set HOME for this test.
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("TETHER_HOOK_SETTINGS", "")
	settingsDir := filepath.Join(home, ".tether", "cc-settings")
	if err := os.MkdirAll(settingsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	wantSettings := filepath.Join(settingsDir, "settings.json")
	if err := os.WriteFile(wantSettings, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}

	// Fake claude binary: must exist on PATH-or-absolute for exec.LookPath
	// to resolve. Script content doesn't matter — execClaudeSyscall is
	// stubbed.
	fakeDir := t.TempDir()
	fakeBin := filepath.Join(fakeDir, "claude-fake")
	if err := os.WriteFile(fakeBin, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	// Stub the execve and capture argv.
	var gotArgv []string
	orig := execClaudeSyscall
	execClaudeSyscall = func(argv0 string, argv []string, env []string) error {
		gotArgv = argv
		return nil // pretend exec succeeded
	}
	defer func() { execClaudeSyscall = orig }()

	// Restore cwd after ExecClaude's os.Chdir mutates it.
	origCwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chdir(origCwd) }()

	code := runResume([]string{
		"--projects-dir", root,
		"--binary", fakeBin,
		"--cwd", cwd,
		sid,
	})
	if code != 0 {
		t.Fatalf("runResume exit=%d", code)
	}
	got := strings.Join(gotArgv, " ")
	if !strings.Contains(got, "--settings "+wantSettings) {
		t.Errorf("argv should contain --settings %s; got: %s", wantSettings, got)
	}
	if !strings.Contains(got, "--resume "+sid) {
		t.Errorf("argv should contain --resume %s; got: %s", sid, got)
	}
}

// TestRunResume_ExecOmitsSettingsWhenAbsent is the negative complement:
// when no env var, no CLI flag, and no on-disk default, argv must NOT
// contain `--settings`. This pins the graceful-fallback contract for
// users running tether without `--auth-broker`.
func TestRunResume_ExecOmitsSettingsWhenAbsent(t *testing.T) {
	root := t.TempDir()
	cwd := filepath.Join(t.TempDir(), "work")
	if err := os.MkdirAll(cwd, 0o755); err != nil {
		t.Fatal(err)
	}
	bucket := EncodeBucket(cwd)
	if err := os.MkdirAll(filepath.Join(root, bucket), 0o755); err != nil {
		t.Fatal(err)
	}
	sid := "no-settings-sid"
	jsonl := filepath.Join(root, bucket, sid+".jsonl")
	if err := os.WriteFile(jsonl, []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// $HOME pointed at a tempdir with NO cc-settings/settings.json.
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("TETHER_HOOK_SETTINGS", "")

	fakeDir := t.TempDir()
	fakeBin := filepath.Join(fakeDir, "claude-fake")
	if err := os.WriteFile(fakeBin, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	var gotArgv []string
	orig := execClaudeSyscall
	execClaudeSyscall = func(argv0 string, argv []string, env []string) error {
		gotArgv = argv
		return nil
	}
	defer func() { execClaudeSyscall = orig }()

	origCwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chdir(origCwd) }()

	code := runResume([]string{
		"--projects-dir", root,
		"--binary", fakeBin,
		"--cwd", cwd,
		sid,
	})
	if code != 0 {
		t.Fatalf("runResume exit=%d", code)
	}
	got := strings.Join(gotArgv, " ")
	if strings.Contains(got, "--settings") {
		t.Errorf("argv should NOT contain --settings when no source resolves; got: %s", got)
	}
	if !strings.Contains(got, "--resume "+sid) {
		t.Errorf("argv should still contain --resume %s; got: %s", sid, got)
	}
}

// TestRunResume_CLIFlagOverridesEnv pins the precedence ordering:
// --settings flag > TETHER_HOOK_SETTINGS env > default-path-on-disk.
func TestRunResume_CLIFlagOverridesEnv(t *testing.T) {
	root := t.TempDir()
	cwd := filepath.Join(t.TempDir(), "work")
	if err := os.MkdirAll(cwd, 0o755); err != nil {
		t.Fatal(err)
	}
	bucket := EncodeBucket(cwd)
	if err := os.MkdirAll(filepath.Join(root, bucket), 0o755); err != nil {
		t.Fatal(err)
	}
	sid := "prec-sid"
	jsonl := filepath.Join(root, bucket, sid+".jsonl")
	if err := os.WriteFile(jsonl, []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	t.Setenv("HOME", t.TempDir())
	t.Setenv("TETHER_HOOK_SETTINGS", "/from/env/settings.json")

	fakeDir := t.TempDir()
	fakeBin := filepath.Join(fakeDir, "claude-fake")
	if err := os.WriteFile(fakeBin, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	var gotArgv []string
	orig := execClaudeSyscall
	execClaudeSyscall = func(argv0 string, argv []string, env []string) error {
		gotArgv = argv
		return nil
	}
	defer func() { execClaudeSyscall = orig }()

	origCwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chdir(origCwd) }()

	wantFlag := "/from/cli/settings.json"
	code := runResume([]string{
		"--projects-dir", root,
		"--binary", fakeBin,
		"--cwd", cwd,
		"--settings", wantFlag,
		sid,
	})
	if code != 0 {
		t.Fatalf("runResume exit=%d", code)
	}
	got := strings.Join(gotArgv, " ")
	if !strings.Contains(got, "--settings "+wantFlag) {
		t.Errorf("CLI flag should win over env; argv: %s", got)
	}
	if strings.Contains(got, "/from/env/settings.json") {
		t.Errorf("env path should be shadowed by --settings flag; argv: %s", got)
	}
}

// TestBuildResumeArgv_ShapeWithSettings + WithoutSettings pin the pure
// helper's output. Belt-and-suspenders against accidental drift in the
// argv ordering (cc parses positionally for some flags).
func TestBuildResumeArgv_ShapeWithSettings(t *testing.T) {
	got := BuildResumeArgv("/usr/bin/claude", "abc", "/path/to/settings.json")
	want := []string{"/usr/bin/claude", "--resume", "abc", "--settings", "/path/to/settings.json"}
	if len(got) != len(want) {
		t.Fatalf("len: got %d, want %d (%v vs %v)", len(got), len(want), got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("argv[%d]: got %q, want %q", i, got[i], want[i])
		}
	}
}

func TestBuildResumeArgv_ShapeWithoutSettings(t *testing.T) {
	got := BuildResumeArgv("/usr/bin/claude", "abc", "")
	want := []string{"/usr/bin/claude", "--resume", "abc"}
	if len(got) != len(want) {
		t.Fatalf("len: got %d, want %d (%v vs %v)", len(got), len(want), got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("argv[%d]: got %q, want %q", i, got[i], want[i])
		}
	}
}
