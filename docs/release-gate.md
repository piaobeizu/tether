# tether v1.0 Release Gate

> Last updated 2026-05-13. K-criteria = the spec §10.K ship gate.

## K-criteria status

| # | Criterion | Status | Notes |
|---|---|---|---|
| K.1 | Single binary, `go install` | ✅ | v0.1.0+ |
| K.2 | HTTP/3 + WebTransport single-port mux | ✅ | v0.1.0+ |
| K.3 | Chat channel (`/wt/chat`) stream-json | ✅ | v0.1.0+ |
| K.4 | Shell channel (`/wt/shell`) PTY | ✅ | v0.1.0+ |
| K.5 | Permission UI (PreToolUse hook) | ✅ | v0.1.0+ |
| K.6 | `tether doctor` preflight | ✅ | v0.1.0+; MCP checks added v0.3.4 |
| K.7 | CI pipeline (codegen drift, build, test) | ✅ | v0.1.0+ |
| K.8 | Troubleshooting docs (VPN, cert, browser) | ✅ | README K.8.1–K.8.6; PR #71 (2026-05-13) |
| K.9 | Performance baseline | ✅ | In-process benchmarks + E2E script; PR #72 (2026-05-13) |
| K.10 | Dogfood ≥1 week | — | **Explicitly waived** (2026-05-13) |

**All K-criteria met or waived.** This codebase is at the v0.4.0 ship-gate baseline.

---

## What ships at v0.4 ("v1.0 baseline")

- Single-port HTTP/3 + TCP server with ECDSA P-256 cert (self-signed or ACME).
- Chat + Shell + Events WebTransport channels.
- Permission UI (PreToolUse hook gate).
- Auth: JWT cookie + OAuth 2.1 PKCE + manual API tokens.
- MCP Host Core: host/registry/gateway/loopback; builtin workspace tools.
- Per-task MCP lifecycle (`MCPInstance` + `LifecycleManager`).
- `tether doctor` with MCP health checks.
- Troubleshooting docs (K.8.1–K.8.6).
- Performance benchmarks (K.9).

---

## Explicitly deferred to v1.0+

These were known at project start; they do not block the current ship gate.

| Feature | Spec ref | Target |
|---|---|---|
| WebCrypto E2E encryption | D-12 | v1.0+ |
| Multi-user session isolation | D-04 | v1.0+ |
| `tether pair` QR pairing | D-13 | v1.0+ |
| Gemini provider | D-17a §5.3 | v1.0+ |
| Mobile PWA (offline + push) | — | v1.0+ |
| TCP / WebSocket fallback | — | v0.5+ (if QUIC adoption lags) |
| PWA offline + partial-message streaming | — | v0.5+ |

---

## Next milestone: v0.5

See [upgrade-path.md](./upgrade-path.md#v04--v05-planned).

- Per-task external MCP server config
- Watchdog GC for idle MCP instances
