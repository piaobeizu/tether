# Upgrade Path

## Released versions

### v0.1.0 (2026-05-09)

Initial release. See [CHANGELOG](../CHANGELOG.md#v010--2026-05-09).

### v0.1 → v0.2.0 (2026-05-10)

- ~~Let's Encrypt integration~~ — shipped: `--acme-domain` / `--acme-email` (HTTP-01).
- **opencode provider**: D-17a §5.1 — stream-json compatible, requires opencode ≥0.4.
- **Basic authentication**: JWT token gate, auto-generated `~/.tether/access-token`.
- **Multi-tab attach**: `/wt/events` read-only fan-out.
- ~~PWA offline support~~ — deferred to v0.5+.
- ~~Partial-message streaming~~ — deferred to v0.5+.

### v0.2 → v0.3.x (2026-05-10–05-12)

- v0.3.0 — MCP Host Core (host/registry/gateway) + permission API generalization.
- v0.3.1 — MCP loopback (`127.0.0.1:8899/mcp`), builtin workspace tools, CC settings inject.
- v0.3.2 — HTTPS `/mcp` + manual API tokens; dynamic tool list refresh.
- v0.3.3 — OAuth 2.1 PKCE flow; IP rate-limiter on Bearer failures.
- v0.3.4 — `tether doctor` MCP health checks.

### v0.3 → v0.4.0 (2026-05-12)

- Per-task MCP lifecycle (`MCPInstance` + `LifecycleManager` + REST API).
- `workspace_run_shell` builtin tool (exec+pipe, 4 MiB cap, `Setpgid`).
- Design token system + dark-mode body override.
- WebTransport + SPA routing fixes.

### v0.4.0 → v0.4.1 (2026-05-28)

UI/streaming/auth increment on the v0.4 line — drop-in over v0.4.0, no API or schema breaks.

- **v2 UI**: three-tab layout, settings pane, desktop chrome (titlebar/statusbar/chat
  bubbles/workspace tree).
- **Token-level streaming**: cc + opencode token-granularity streaming + per-session history.
- **WebTransport auth ticket**: Chrome WT CONNECT carries no Cookie → short-lived ticket
  exchanged on handshake; `/auth` redirect on 401.
- **Static assets**: `.webmanifest` rename + MIME registration; real 404 for missing static
  paths (no SPA fallback).
- **Docs**: K.8 troubleshooting + K.9 benchmarks + v1.0 release-gate docs.

See [CHANGELOG](../CHANGELOG.md#v041--2026-05-28).

---

## Upcoming

### v0.4 → v0.5 (planned)

- **Per-task external MCP server config**: tasks declare their own servers in
  `.tether/task-config.json`; `LifecycleManager` starts/stops them with the task.
- **Watchdog GC for idle MCP instances**: idle instances stopped after a threshold;
  revived on next tool call.

### v0.5 → v1.0 (roadmap)

Features explicitly deferred from v0.x — see [release-gate.md](./release-gate.md)
for current ship-gate status.

- **WebCrypto E2E (D-12)**: ECDH key exchange; daemon becomes a relay.
- **Multi-user**: per-user session isolation, audit log.
- **`tether pair`**: QR-code-based device pairing (D-13).
- **Gemini provider**: D-17a §5.3. Cursor already works via OAuth PKCE (v0.3.3).
- **Mobile PWA**: full offline + push notifications.
- **CI hardening**: cross-platform build matrix, coverage gate.
