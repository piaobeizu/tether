package claude

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// ErrBinaryNotFound is returned by Spawn when the claude binary cannot be
// located. Per spec §6.F.2 the error message must include the searched PATH
// so deploys can diagnose binary-missing failures quickly.
var ErrBinaryNotFound = errors.New("claude binary not found")

// CwdEnvHome is the environment variable consulted when SpawnOpts.ProjectCwd
// is empty. cc encodes the cwd into ~/.claude/projects/<encoded>/, so the
// tether-wide default of $HOME makes every tether-spawned session land in
// a single bucket — the "(a) fixed cwd" choice from spec §10.4. v0.1.
//
// Sub-ticket #1 (per plan §9 v0.1 follow-up) tracks the alternative (c)
// `tether resume` wrapper that lets users address sessions by id with
// per-project cwd transparency.
const CwdEnvHome = "HOME"

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

	// OOMCircuitMaxExits is the maximum number of SIGKILL-induced exits
	// (attributed to the OOM-killer or external `kill -9`) tolerated within
	// OOMCircuitWindow before Recover returns ErrOOMCircuitOpen. Zero (the
	// default) means use DefaultOOMCircuitMaxExits (=3). Negative disables
	// the circuit-breaker entirely (Recover will keep re-spawning).
	//
	// ops-concerns subitem #2 (gh-13 round-1 Tier-4).
	OOMCircuitMaxExits int

	// OOMCircuitWindow is the rolling window in which OOMCircuitMaxExits is
	// counted. Zero means use DefaultOOMCircuitWindow (=60s).
	OOMCircuitWindow time.Duration

	// Env adds / overrides environment variables for the spawned subprocess
	// on top of the parent process's environment. Nil (or empty map)
	// inherits the parent's env unchanged — the v0 default.
	//
	// Per spec §10.3 (creds-injection / piaobeizu/tether#21): v0.1 cc
	// authentication flows via env-var injection (ANTHROPIC_API_KEY or
	// equivalent) at spawn time. The caller (daemon, when it exists; CLI
	// for now) decides where the credential comes from — direct env, k8s
	// Secret, vault, OAuth-derived token. Session.Spawn does NOT enforce
	// any credential storage policy; it only forwards what the caller
	// passes here.
	//
	// v0.2+ may add OAuth flow + secret store + token rotation; that work
	// lives daemon-side and ultimately funnels into this same Env field —
	// the shape is forward-compatible.
	//
	// Semantics: a key in Env replaces any same-named entry from the
	// parent env. Keys not in Env are inherited as-is. To explicitly set
	// an inherited variable to empty (vs unset), include it with value
	// "" — subprocess sees the empty string.
	Env map[string]string
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
	cmd.Dir = effectiveCwd(opts.ProjectCwd)
	cmd.ExtraFiles = nil // A.1: no fd 3 / extra channels
	if env := effectiveEnv(opts.Env); env != nil {
		cmd.Env = env
	}
	// nil cmd.Env preserves the historical default: cmd inherits the
	// parent's env wholesale. Only callers who pass non-empty Env opt
	// into the merge path.

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

// effectiveCwd resolves SpawnOpts.ProjectCwd into the cwd actually passed
// to the subprocess. Empty → $HOME (spec §10.4 strategy "a"); otherwise
// the caller-supplied path is used verbatim.
//
// If $HOME is also unset (highly unusual on linux/mac), fall back to
// /tmp — better than passing "" which exec.Cmd interprets as "inherit
// parent cwd" (which in our case would be the daemon's own cwd, jumbling
// sessions across deployments).
func effectiveCwd(opt string) string {
	if opt != "" {
		return opt
	}
	if h := os.Getenv(CwdEnvHome); h != "" {
		return h
	}
	return "/tmp"
}

// effectiveEnv builds the cmd.Env slice from parent env + caller overrides
// per SpawnOpts.Env semantics (creds-injection / piaobeizu/tether#21):
//
//   - nil / empty map → return nil (caller of Spawn leaves cmd.Env as nil
//     so cmd.Run inherits the parent env wholesale; preserves v0 default).
//   - non-empty map → start from os.Environ(); for each (k,v) in
//     overrides, replace the existing "k=*" entry if present, else append.
//
// Pure function; testable without a subprocess.
func effectiveEnv(overrides map[string]string) []string {
	if len(overrides) == 0 {
		return nil
	}
	base := os.Environ()
	// Index existing keys for O(1) replace.
	idx := make(map[string]int, len(base))
	for i, kv := range base {
		eq := strings.IndexByte(kv, '=')
		if eq <= 0 {
			continue // malformed entry; leave alone
		}
		idx[kv[:eq]] = i
	}
	for k, v := range overrides {
		entry := k + "=" + v
		if i, ok := idx[k]; ok {
			base[i] = entry
		} else {
			base = append(base, entry)
		}
	}
	return base
}
