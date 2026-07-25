// Package agent defines the AgentProvider abstraction (D-17a §3).
// v0.1 ships ClaudeCodeProvider only; other providers are stubs.
package agent

import (
	"context"
	"os"
)

// MCP host integration (internal/mcp/) is orthogonal to AgentProvider.
// The provider abstraction selects which LLM drives the conversation;
// MCP server lifecycle is managed independently of which provider is active.

// AgentProvider manages the lifecycle of AI agent sessions.
type AgentProvider interface {
	Name() string
	Spawn(ctx context.Context, cfg SpawnConfig) (Session, error)
}

// SpawnConfig carries per-session spawn parameters.
type SpawnConfig struct {
	// ResumeSessionID, if non-empty, resumes an existing cc JSONL session.
	ResumeSessionID string
	// Env holds additional environment variables passed to the subprocess.
	Env []string
	// Workdir sets the working directory for the agent subprocess. Defaults
	// to the process cwd if empty. The daemon wires this to the resolved
	// --workspace-root (internal/server/lifecycle.go Step 3b, via
	// session.Registry.Workdir) so that the agent's cwd is a declared,
	// deterministic directory instead of wherever the daemon happened to be
	// launched from — which matters because cc keys its on-disk conversation
	// (and therefore `--resume`) on cwd (tether#51).
	//
	// Note this is ONE daemon-global directory, not a per-workspace one: the
	// chat wire protocol carries no workspace selector, so it cannot yet be
	// the `Path` of a specific internal/workspace.Registry entry.
	Workdir string
}

// ResolveWorkdir returns the working directory an agent subprocess should run
// in. An empty workdir keeps the pre-tether#51 behaviour — the daemon's own
// cwd — but resolves it explicitly so the effective directory is observable
// (and assertable) rather than implied by exec's empty-Dir default. If the cwd
// cannot be determined the result is "", which exec treats as "inherit".
//
// Exported because the PTY shell pane (internal/server/wt_shell.go) execs cc
// directly rather than through a provider, and both spawn paths MUST land in
// the same directory — cc's `--resume <sid>` only finds a conversation when
// the cwd matches the one that created it. Keeping the fallback defined here
// once is what stops the two paths from drifting again (the drift between
// ClaudeCodeProvider and OpenCodeProvider was this bug's root cause).
func ResolveWorkdir(workdir string) string {
	if workdir != "" {
		return workdir
	}
	wd, err := os.Getwd()
	if err != nil {
		return ""
	}
	return wd
}

// Session is a live connection to an agent subprocess.
type Session interface {
	// SessionID returns the cc-managed JSONL session ID (available after
	// the first system/init event; blocks if not yet received).
	SessionID() string
	// SendPrompt sends a user prompt as a stream-json message.
	SendPrompt(ctx context.Context, text string) error
	// Events returns the channel of daemon-internal events (read-only).
	Events() <-chan Event
	// Interrupt sends a SIGINT to the subprocess.
	Interrupt() error
	// Close shuts down the session cleanly.
	Close() error
}
