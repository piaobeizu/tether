// Package adapter wires installed skills into a workspace for a specific
// agent backend. Spec §11.Z.4: in v0.1 only cc is implemented as a pure
// symlink (~70 LOC); codex/opencode/aider/cursor are stubs that panic so
// the seam is named from day 1 (matches D-17 AgentProvider discipline).
package adapter
