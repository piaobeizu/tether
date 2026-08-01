// Package agent defines the AgentProvider abstraction (D-17a §3).
// v0.1 ships ClaudeCodeProvider only; other providers are stubs.
package agent

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"os"
)

// NewSessionID mints a fresh RFC-4122 version-4 UUID for use as SpawnConfig.
// SessionID — the shape cc uses for its own session ids, and the shape it
// accepts on `--session-id` (mem_2ruSlrHR ①).
//
// Hand-rolled rather than pulled from github.com/google/uuid because this is the
// only uuid the daemon ever needs and the module has no uuid dependency; adding
// one for 8 lines would be a poor trade.
//
// crypto/rand.Read is documented never to return an error (it panics internally
// if the system source is broken), so the branch below is unreachable. It returns
// a fixed, syntactically valid v4 purely so the function has no error in its
// signature; being a constant it would collide with itself if it ever ran, which
// is acceptable only because it cannot.
func NewSessionID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "00000000-0000-4000-8000-000000000000"
	}
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // RFC-4122 variant
	h := hex.EncodeToString(b[:])
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
}

// MCP host integration (internal/mcp/) is orthogonal to AgentProvider.
// The provider abstraction selects which LLM drives the conversation;
// MCP server lifecycle is managed independently of which provider is active.

// AgentProvider manages the lifecycle of AI agent sessions.
type AgentProvider interface {
	Name() string
	Spawn(ctx context.Context, cfg SpawnConfig) (Session, error)
}

// SpawnConfig carries per-session spawn parameters.
//
// SessionID and ResumeSessionID are MUTUALLY EXCLUSIVE — see SessionID's doc and
// ClaudeCodeProvider.Spawn, which rejects a config that sets both rather than
// letting cc exit 1 opaquely.
type SpawnConfig struct {
	// ResumeSessionID, if non-empty, resumes an existing cc JSONL session
	// (`--resume <sid>`). Reconnect path only.
	ResumeSessionID string
	// SessionID, if non-empty, PINS the new session's id (`--session-id <uuid>`)
	// instead of letting cc mint one and reporting it back on system/init. Fresh
	// spawn path only.
	//
	// Measured against claude 2.1.220 (mem_2ruSlrHR ①): cc adopts the caller's
	// uuid verbatim and echoes it on both system/init and result. That is what
	// lets the daemon know a session's id at spawn time rather than having to
	// wait for (and trust) init — which in turn is what makes the tether#50
	// resume/fallback decision expressible at all.
	//
	// It must NOT be combined with ResumeSessionID (mem_2ruSlrHR ⑧): real cc
	// exits 1 with "--session-id can only be used with --continue or --resume if
	// --fork-session is also specified." A resumed session's id does not drift
	// anyway (②), so re-pinning it on reconnect would be redundant even if it
	// were allowed. Wanting "inherit the context under a NEW id" would require
	// passing --fork-session as well, which tether does not do today.
	SessionID string
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
