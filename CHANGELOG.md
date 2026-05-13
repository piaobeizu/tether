# Changelog

## v0.4.0 — 2026-05-12 (PR #68, #69, #70)

### Added
- **Per-task MCP lifecycle**: `MCPInstance` + `LifecycleManager` — spawn/stop external MCP
  servers on task start/pause; REST API `POST/DELETE /api/v1/mcp/lifecycle/{task_id}`.
- **`workspace_run_shell` builtin tool**: exec + pipe (no PTY), 4 MiB output cap,
  `Setpgid` process-group kill, `timeout_secs` parameter.
- **End-to-end smoke test**: REST POST → MCPInstance → HTTP MCP → tool call
  (`waitForPort` replaces flaky `sleep`).
- **Polyforge lifecycle hooks**: `.claude/settings.json` PostToolUse intercepts
  `pf2_task_start/pause/wrap` → curl tether REST, bridging workspace events into
  tether's MCP lifecycle.
- **Design token system**: CSS custom properties in `tokens.css`; dark-mode body
  background override (`[data-theme="dark"] body { background: #0F0E0C }`).
- **WebTransport + SPA routing fixes**: Alt-Svc rewrite, `/wt/*` path conflicts resolved.

### Notes
- `workspace_run_shell` is exec+pipe only; PTY is `/wt/shell` (interactive terminal).
- `CallToolParams.Arguments` in go-sdk is `any`; pass `map[string]any`, not
  `json.RawMessage`, to avoid base64-encoding in tests.

## v0.3.4 — 2026-05-12 (PR #69)

### Added
- Three new `tether doctor` checks for MCP subsystem health:
  - `mcp-settings-inject` — verifies `~/.claude/settings.json` contains the
    tether-managed `mcpServers.tether` entry; fails with actionable message if
    absent (run `tether server` to inject)
  - `mcp-api-tokens` — reports count of external API tokens in
    `~/.tether/api-tokens.json`; informational (OK) when file is absent or empty
  - `mcp-loopback` — TCP-connects to the MCP loopback port (default 8899,
    read from settings); informational (OK) when server is not running

### Fixed
- `checkCCSettingsHooks` was reading `~/.config/claude/settings.json` instead of
  `~/.claude/settings.json`; hooks check now reads the correct file so it no
  longer always reports "hook not found" on a correctly configured system

## v0.3.3 — 2026-05-11/12 (PR #68)

### Added
- `internal/auth/oauth/` — OAuth 2.1 Authorization Code + PKCE (S256) flow
  - `GET /.well-known/oauth-authorization-server` — RFC 8414 discovery metadata
  - `GET /oauth/authorize` — PKCE validation + `html/template` approval page (XSS-safe)
  - `POST /oauth/authorize` — allow/deny form handler
  - `POST /oauth/token` — auth code exchange; issues 24h Bearer token via `apitoken.Store`
- `apitoken.TokenSource` typed constant (`TokenSourceManual` / `TokenSourceOAuth`), `ExpiresAt` field, `CreateWithTTL`, `StartEviction` background eviction goroutine
- `MCPHTTPSHandler` — IP rate limiter (5 failures/60s → 429 for 5 min, bounded map)
- Structured audit log events: `oauth.authorize.{allowed,denied,invalid_req_id}`, `oauth.token.{issued,invalid_grant}`, `mcp.bearer.{auth_ok,auth_failed,rate_limited}`

### Security
- `redirect_uri` restricted to loopback (localhost / 127.0.0.1) — prevents exfiltration
- Approval page uses `html/template` (XSS-safe `client_id` rendering)
- PKCE `plain` method rejected per OAuth 2.1 §7.5.1
- Auth codes: single-use, 256-bit entropy, deleted on mismatch or expiry

### Changed
- `auth.State.isExempt` — exact-path exemptions for `/oauth/authorize`, `/oauth/token`, `/.well-known/oauth-authorization-server`

## v0.3.2 — 2026-05-11 (PR #67)

### Added
- HTTPS `/mcp` endpoint on the main port for external MCP clients (Cursor, Goose).
  Protected by manually-issued Bearer tokens.
- REST API `POST/GET/DELETE /api/v1/mcp/tokens` for managing external client tokens.
  Tokens stored hashed (SHA-256) in `~/.tether/api-tokens.json` (mode 0600).
- Audit log events `mcp.apitoken.{created,revoked,auth_ok,auth_failed}` (slog).
- Dynamic tool list refresh: external clients and CC immediately see tools
  added or removed by supervisor reconnects without needing to reconnect.

### Changed
- `/mcp` HTTPS endpoint no longer returns 501.
- `BuildMCPServer` now takes a `*registry.Registry` to subscribe live tool updates.

### Deferred to v0.3.3
- Full OAuth 2.1 PKCE flow (`/oauth/authorize`, `/oauth/token`).
- OAuth discovery endpoint (`/.well-known/oauth-authorization-server` — currently 404).
- Token TTL, scope, dynamic client registration, web UI for token management.
- Rate limiting on `/mcp` Bearer-validation failures.

### Migration
- Existing v0.3.1 deployments: no migration needed. CC loopback at
  `127.0.0.1:8899/mcp` continues to work unchanged.
- New file `~/.tether/api-tokens.json` is created lazily on first
  `POST /api/v1/mcp/tokens`; include in backups if external clients are expected.

### Security
- Manual tokens are long-lived and all-scope. Use one token per external client;
  revoke and recreate on suspected leak. See spec
  `tether-doc/wiki/specs/2026-05-11-mcp-v032-design.md` §9 for the rotation
  workflow.
- Token-CRUD endpoints `/api/v1/mcp/tokens` are protected by the existing JWT
  cookie middleware and `WithOriginGuard` CSRF posture (rejects POST/DELETE with
  mismatched `Origin` header).

## v0.3.1 — 2026-05-11 (PR #66)

### Added
- MCP loopback endpoint at `127.0.0.1:8899/mcp` for CC subprocess integration.
- Builtin workspace tools (`workspace_read_file`, `workspace_list_files`, etc.).
- CC settings injection into `~/.claude/settings.json` on daemon startup.

## v0.3.0 — 2026-05-10 (PR #64/#65)

### Changed
- Permission API: canonical paths are now `/api/v1/permission/request` and `/api/v1/permission/{id}/decide`. Old paths `/api/v1/agent/permission/*` are kept as aliases through v0.3.x and will be removed in v0.4.
- Permission request envelope gains a `source` field (`"claude_hook"` for existing hook flow; `"mcp:<server>"` for future MCP gateway). Clients omitting `source` default to `"claude_hook"`.
- `internal/agent/permhook/` renamed to `internal/permission/cchook/` (import path change; binary behavior unchanged).
- `/mcp` and `/api/v1/mcp/*` reserved — return 501 until MCP host is implemented later in v0.3.

## v0.2.0 — 2026-05-10 (PR #63)

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
