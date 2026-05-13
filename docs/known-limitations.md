# Known Limitations — tether v0.4

## Security

**No end-to-end encryption (D-12 deferred to v1.0+)**
TLS terminates at the tether daemon. Traffic between the browser and daemon is
TLS-protected, but the daemon itself has plaintext access. WebCrypto E2E (D-12)
is a v1.0+ item.

~~**Self-signed certificate**~~
*(Resolved in v0.1.1 / v0.2.0)* — Let's Encrypt ACME is available via
`--acme-domain` / `--acme-email`. For LAN/self-signed dev setups see
[README K.8.3](../README.md#k83--self-signed-cert-chrome-refuses-to-load-the-page).

## Connectivity

**No TCP fallback for WebTransport**
HTTP/3 over UDP is the only transport. VPNs and corporate firewalls that block
UDP will cause chat/shell channels to fail even if the page loads. See
[README K.8.1](../README.md#k81--vpn--corporate-firewall-breaks-the-shell-and-chat-channels).

**Browser support: Chrome/Edge 97+ only**
Firefox and Safari do not support the WebTransport API. See
[README K.8.2](../README.md#k82--browser-version-requirements).

## Multi-user / Multi-device

~~**Single-user, single-machine only (no auth)**~~
*(Partially resolved)* — v0.2.0 added JWT token gate; v0.3.2 added manual API
tokens for external MCP clients; v0.3.3 added OAuth 2.1 PKCE. Full multi-user
session isolation (D-04 `LockUser="default"`) is still v1.0+.

**No QR-pairing (`tether pair`)**
`tether pair` is deferred to v1.0 (D-13).

## Agent providers

~~**Claude Code only**~~
*(Partially resolved)* — opencode provider added in v0.2.0. External MCP
clients (Cursor, Goose) can connect via OAuth PKCE (v0.3.3) or manual Bearer
tokens (v0.3.2). Gemini provider is still v1.0+.

## UI / PWA

**No offline support**
The PWA manifest is minimal. Service worker and offline caching are v0.5+.

**No partial-message streaming**
`--include-partial-messages` is not enabled. The chat pane renders complete
assistant turns only. Deferred to v0.5+.

## Permission hook

**`bypassPermissions` mode skips the hook**
When `claude` is started in `--permission-mode bypassPermissions`, cc does not
fire PreToolUse hooks. tether's permission UI is never shown — tool calls
auto-approve. This is the intended UX for fully autonomous mode.

**Hook requires Go toolchain at runtime (fallback path)**
The permission hook binary is pre-compiled in release tarballs. For `go install`
users the hook is compiled from embedded source on first daemon start, which
requires `go` on PATH. If `go` is absent, the hook compilation is skipped and a
warning is logged.
