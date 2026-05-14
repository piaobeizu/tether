// Package agent defines the AgentProvider abstraction (D-17a §3).
// v0.1 ships ClaudeCodeProvider only; other providers are stubs.
package agent

import "context"

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
	// Workdir sets the working directory for the agent subprocess.
	// Defaults to the process cwd if empty.
	Workdir string
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
