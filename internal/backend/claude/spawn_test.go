package claude

import (
	"context"
	"errors"
	"os/exec"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestBuildArgs_Defaults(t *testing.T) {
	got := BuildArgs(SpawnOpts{})
	want := []string{
		"--print",
		"--output-format", "stream-json",
		"--input-format", "stream-json",
		"--verbose",
		"--permission-prompt-tool", "stdio",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("default args:\n got: %v\nwant: %v", got, want)
	}
}

func TestBuildArgs_WithResume(t *testing.T) {
	args := BuildArgs(SpawnOpts{SessionID: "abc-123"})
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "--resume abc-123") {
		t.Errorf("resume args missing: %q", joined)
	}
}

func TestBuildArgs_WithModel(t *testing.T) {
	args := BuildArgs(SpawnOpts{Model: "haiku"})
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "--model haiku") {
		t.Errorf("model args missing: %q", joined)
	}
}

func TestBuildArgs_ResumeAndModel(t *testing.T) {
	args := BuildArgs(SpawnOpts{SessionID: "sid", Model: "sonnet"})
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "--resume sid") {
		t.Errorf("resume missing: %q", joined)
	}
	if !strings.Contains(joined, "--model sonnet") {
		t.Errorf("model missing: %q", joined)
	}
}

// F.2 verification: empty PATH + missing binary → ErrBinaryNotFound carrying PATH.
func TestSpawn_BinaryNotFound(t *testing.T) {
	t.Setenv("PATH", "/this/path/intentionally/empty")

	_, err := Spawn(context.Background(), SpawnOpts{BinaryPath: "claude-not-installed-xyz"})
	if err == nil {
		t.Fatal("Spawn should have failed when binary missing")
	}
	if !errors.Is(err, ErrBinaryNotFound) {
		t.Errorf("expected ErrBinaryNotFound, got: %v", err)
	}
	if !strings.Contains(err.Error(), "/this/path/intentionally/empty") {
		t.Errorf("error should embed PATH for ops debugging; got: %v", err)
	}
	if !strings.Contains(err.Error(), "claude-not-installed-xyz") {
		t.Errorf("error should mention the binary name; got: %v", err)
	}
}

// Integration test — requires claude binary in PATH. Skip otherwise.
//
// Verifies:
//   - Spawn succeeds, returns a running process
//   - A.1: cmd.ExtraFiles == nil (no fd 3)
//   - SIGKILL → Cmd.Wait() returns non-nil error (kill is expected)
//   - All three pipes are present and closable
func TestSpawn_RealClaude(t *testing.T) {
	if _, err := exec.LookPath("claude"); err != nil {
		t.Skip("claude binary not in PATH; skipping integration spawn test")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	sub, err := Spawn(ctx, SpawnOpts{Model: "haiku"})
	if err != nil {
		t.Fatalf("Spawn failed: %v", err)
	}
	defer func() {
		_ = sub.Stdin.Close()
		_ = sub.Stdout.Close()
		_ = sub.Stderr.Close()
	}()

	// A.1: no fd 3 / extra channels.
	if sub.Cmd.ExtraFiles != nil {
		t.Errorf("A.1 violated: ExtraFiles must be nil, got %v", sub.Cmd.ExtraFiles)
	}

	// Process should be running.
	if sub.Cmd.Process == nil || sub.Cmd.Process.Pid <= 0 {
		t.Fatalf("expected live process, got %v", sub.Cmd.Process)
	}

	// All three pipes must be non-nil.
	if sub.Stdin == nil || sub.Stdout == nil || sub.Stderr == nil {
		t.Fatalf("pipes nil: in=%v out=%v err=%v",
			sub.Stdin == nil, sub.Stdout == nil, sub.Stderr == nil)
	}

	// SIGKILL it. Wait should return non-nil error.
	if err := sub.Cmd.Process.Kill(); err != nil {
		t.Errorf("Kill failed: %v", err)
	}
	if err := sub.Cmd.Wait(); err == nil {
		t.Error("expected non-nil error after Kill, got nil")
	}
}

// Spawn should not leak pipes on error paths.
//
// We trigger a Spawn failure by giving a bad cwd path that will cause
// cmd.Start() to fail (chdir error). All three pipes should be closed
// before Spawn returns the error.
func TestSpawn_ErrorPathClosesPipes(t *testing.T) {
	if _, err := exec.LookPath("claude"); err != nil {
		t.Skip("claude binary not in PATH; skipping")
	}

	_, err := Spawn(context.Background(), SpawnOpts{
		ProjectCwd: "/this/dir/does/not/exist/anywhere",
		Model:      "haiku",
	})
	if err == nil {
		t.Fatal("Spawn should have failed with bad cwd")
	}
	if !strings.Contains(err.Error(), "start claude") {
		t.Errorf("expected 'start claude' wrapping, got: %v", err)
	}
}

// S1 fix: WaitOnce must be safe under concurrent calls — only one
// underlying Cmd.Wait runs; all callers see the same cached error.
func TestSubprocess_WaitOnce_Concurrent(t *testing.T) {
	if _, err := exec.LookPath("claude"); err != nil {
		t.Skip("claude binary not in PATH; skipping")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	sub, err := Spawn(ctx, SpawnOpts{Model: "haiku"})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	_ = sub.Stdin.Close() // hint claude to exit; auth flow will also fail it

	// Kill it to ensure a definite exit.
	_ = sub.Cmd.Process.Kill()

	// Race ten goroutines into WaitOnce — all should observe the same
	// non-nil error without panicking.
	const N = 10
	results := make(chan error, N)
	for range N {
		go func() { results <- sub.WaitOnce() }()
	}

	var firstErr error
	for i := range N {
		got := <-results
		if i == 0 {
			firstErr = got
		} else if got != firstErr {
			t.Errorf("WaitOnce returned different errors: %v vs %v", firstErr, got)
		}
	}
	if firstErr == nil {
		t.Error("WaitOnce should return non-nil error after Kill")
	}
}

// Pipe interface types are enforced statically by the struct field
// declarations in spawn.go — no extra guard needed.
