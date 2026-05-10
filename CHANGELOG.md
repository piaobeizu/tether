# Changelog

## v0.2.0 (unreleased)

### Added
- Auth: static token gate (`--token` flag, auto-generated at `~/.tether/access-token`); JWT cookie (`HttpOnly; Secure; SameSite=Strict; Path=/`; 90-day sliding expiry); `/auth` page in SPA
- opencode provider: per-prompt subprocess spawn (D-17a §5.1); select per session via provider dropdown in UI
- `GET /api/v1/providers` — list registered providers (sorted)
- `tether doctor` now checks for `opencode` binary on PATH (optional, always OK)
- Multi-tab attach: second browser tab connects to `/wt/events` for read-only event stream; server-side owner enforcement via `OwnerClientID` (CAS, first-wins)
- Attach/Detach UI in workspace pane

## v0.1.0 — 2026-05-09

### What's new

- **Single-port HTTP/3 + TCP server**: One port serves both the React SPA (TCP HTTPS) and WebTransport channels (UDP HTTP/3). Shared ECDSA P-256 self-signed cert with 14-day auto-rotation.
- **Chat channel (`/wt/chat`)**: Stream-json Claude Code integration. Chat pane renders assistant text, tool_use blocks (via fenced-block components), and error events.
- **Shell channel (`/wt/shell`)**: PTY-based `claude --resume <sid>` subprocess. xterm.js renders raw TUI output including box-drawing and color. Supports `/help`, `/plugin update`, etc.
- **Permission UI (`/wt/events` + PermissionBlock)**: PreToolUse hook intercepts tool calls and presents Allow/Deny UI in the browser. 60s auto-deny timeout. Hook binary compiled on first run from embedded source.
- **Session lock (D-15)**: One client holds input focus at a time. Force-takeover via `POST /api/v1/session/{sid}/lock/force`.
- **Workspace registry**: Manage workspace paths in `~/.tether/workspaces.json`. Accessible via REST and the workspace pane.
- **Skill overlay (D-20)**: Install skill plugins; enable/disable creates OS symlinks in `<workspace>/.claude/plugins/<id>/` so `claude` picks them up without copying.
- **`tether doctor`**: Preflight check command (cc-binary, data-dir, cert-state, port-bindable, cc-settings-hooks). `--json` and `--verbose` flags.
- **Wire-contract tests (D-22 §6)**: 7 vitest tests verifying the TypeScript wire protocol contracts.
- **CI pipeline**: GitHub Actions — codegen drift gate, Go build, web build, vitest.
- **Release pipeline**: Cross-compile for 5 platforms (linux/amd64, linux/arm64, darwin/amd64, darwin/arm64, windows/amd64) with permission hook pre-compiled in the tarball.

### Known limitations

See [docs/known-limitations.md](docs/known-limitations.md).

### Upgrade path

See [docs/upgrade-path.md](docs/upgrade-path.md).
