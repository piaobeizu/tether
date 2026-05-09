// Package claude is tether's backend wrapper around the Claude Code CLI.
//
// It spawns the `claude` ELF binary as a subprocess in stream-json mode and
// communicates via NDJSON over stdio. Session persistence and recovery are
// delegated to CC's own --resume mechanism. See gh-13 spec for the full
// behavior contract (24 items across A/C/D/E/F/G areas).
package claude
