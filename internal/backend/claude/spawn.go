package claude

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"
)

// ErrBinaryNotFound is returned by Spawn when the claude binary cannot be
// located. Per spec §6.F.2 the error message must include the searched PATH
// so deploys can diagnose binary-missing failures quickly.
var ErrBinaryNotFound = errors.New("claude binary not found")

// SpawnOpts configures a single claude subprocess invocation.
type SpawnOpts struct {
	// ProjectCwd is the working directory passed to claude. Sessions are
	// bucketed under ~/.claude/projects/<encoded-cwd>/, so this directly
	// determines where the session jsonl is stored. See spec §6.E.5.
	ProjectCwd string

	// SessionID, if non-empty, instructs claude to resume an existing
	// conversation via --resume <sid>.
	SessionID string

	// Model is the optional model alias passed via --model (e.g. "haiku").
	// Empty means use claude's configured default.
	Model string

	// BinaryPath overrides the default "claude" lookup. For tests / pinned
	// installs.
	BinaryPath string
}

// Subprocess is a running claude subprocess with stdio pipes.
//
// Per spec §6.A.1 the subprocess is started with no extra file descriptors
// (no fd 3 diagnostic channel). Per §6.A.5 the caller is responsible for
// continuously draining Stderr to avoid blocking the subprocess on a full
// pipe buffer.
//
// Concurrency: Cmd.Wait() must be called exactly once per the os/exec
// contract. Use WaitOnce when multiple goroutines may race to clean up
// (typical pattern in session.go: parser-end goroutine + Close()).
type Subprocess struct {
	Cmd    *exec.Cmd
	Stdin  io.WriteCloser
	Stdout io.ReadCloser
	Stderr io.ReadCloser

	waitOnce sync.Once
	waitErr  error
}

// WaitOnce is a safe wrapper for Cmd.Wait(): the underlying call happens
// exactly once even when invoked concurrently from multiple goroutines.
// Subsequent calls return the cached error from the first call.
func (s *Subprocess) WaitOnce() error {
	s.waitOnce.Do(func() {
		s.waitErr = s.Cmd.Wait()
	})
	return s.waitErr
}

// BuildArgs produces the argv (excluding argv[0]) for a given SpawnOpts.
// Pure function — exported for tests so they can assert flag wiring without
// touching the OS.
//
// `--include-partial-messages` is required to receive stream_event token
// deltas + message_delta events; without it claude only emits assistant /
// user / result envelopes and the state machine in session.go can't detect
// ToolPending (which is keyed on message_delta.stop_reason=tool_use).
func BuildArgs(opts SpawnOpts) []string {
	args := []string{
		"--print",
		"--output-format", "stream-json",
		"--input-format", "stream-json",
		"--verbose",
		"--include-partial-messages",
		"--permission-prompt-tool", "stdio",
	}
	if opts.SessionID != "" {
		args = append(args, "--resume", opts.SessionID)
	}
	if opts.Model != "" {
		args = append(args, "--model", opts.Model)
	}
	return args
}

// Spawn starts a claude subprocess wired with three OS pipes
// (stdin / stdout / stderr). On binary-missing returns ErrBinaryNotFound
// with PATH embedded in the wrapped error.
func Spawn(ctx context.Context, opts SpawnOpts) (*Subprocess, error) {
	bin := opts.BinaryPath
	if bin == "" {
		bin = "claude"
	}
	resolved, err := exec.LookPath(bin)
	if err != nil {
		return nil, fmt.Errorf("%w: searched PATH=%q for %q: %v",
			ErrBinaryNotFound, os.Getenv("PATH"), bin, err)
	}

	cmd := exec.CommandContext(ctx, resolved, BuildArgs(opts)...)
	cmd.Dir = opts.ProjectCwd
	cmd.ExtraFiles = nil // A.1: no fd 3 / extra channels

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		_ = stdin.Close()
		return nil, fmt.Errorf("stdout pipe: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		_ = stdin.Close()
		_ = stdout.Close()
		return nil, fmt.Errorf("stderr pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		_ = stdin.Close()
		_ = stdout.Close()
		_ = stderr.Close()
		return nil, fmt.Errorf("start claude: %w", err)
	}

	return &Subprocess{
		Cmd:    cmd,
		Stdin:  stdin,
		Stdout: stdout,
		Stderr: stderr,
	}, nil
}
