// Package cc collects the cc-binary integration helpers used by the
// ClaudeCodeProvider in internal/agent: version detection, hook
// settings.json generation, hook HTTP server, JSONL path encoding /
// session lookup, JSONL watcher + envelope mapper, and (in later PRs)
// special-command translation.
//
// Files in this package are ports / re-implementations of happy-cli's
// claudeLocal.ts / generateHookSettings.ts / startHookServer.ts /
// claudeFindLastSession.ts / path.ts modules. The agent package depends
// on this package; daemon code paths NEVER import this package directly
// (D-17 seam — they go through internal/agent.AgentProvider).
package cc
