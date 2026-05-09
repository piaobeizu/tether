# Known Limitations — tether v0.1

## Security

**No end-to-end encryption (D-12 deferred to v1.0+)**
TLS terminates at the tether daemon. The threat model is equivalent to cloudcli: traffic between the browser and daemon is TLS-protected, but the daemon itself has plaintext access. WebCrypto E2E (D-12) is a v1.0+ item.

**Self-signed certificate**
v0.1 does not integrate with Let's Encrypt or any CA. The daemon generates a 14-day ECDSA P-256 self-signed cert that auto-rotates. Browser access requires a `--ignore-certificate-errors` flag or a per-origin override. See [README K.8.3](../README.md#k83--self-signed-cert-chrome-refuses-to-load-the-page).

## Connectivity

**No TCP fallback for WebTransport (Risk #2)**
HTTP/3 over UDP is the only transport. VPNs and corporate firewalls that block UDP will cause chat/shell channels to fail even if the page loads. No WebSocket fallback in v0.1. See [README K.8.1](../README.md#k81--vpn--corporate-firewall-breaks-the-shell-and-chat-channels).

**Browser support: Chrome/Edge 97+ only**
Firefox and Safari do not support the WebTransport API. See [README K.8.2](../README.md#k82--browser-version-requirements).

## Multi-user / Multi-device

**Single-user, single-machine only**
No authentication (D-16 deferred), no pairing (D-13 deferred). The daemon binds to `127.0.0.1` by default. LAN/remote access requires `TETHER_HOST` and Chrome cert-bypass flags.

**No pair/attach commands**
`tether attach` and `tether pair` are stubs. Multi-device pairing is v1.0+.

## Agent providers

**Claude Code only**
opencode, Cursor, and Gemini providers are stubbed (`Spawn()` returns "not yet implemented"). Multi-provider support is v0.2+.

## UI / PWA

**No offline support**
The PWA manifest is minimal. Service worker and offline caching are v0.1.1 follow-ups.

**No partial-message streaming**
`--include-partial-messages` is not enabled. The chat pane renders complete assistant turns only.

## Permission hook

**`bypassPermissions` mode skips the hook**
When `claude` is started in `--permission-mode bypassPermissions`, cc does not fire PreToolUse hooks. tether's permission UI is never shown — tool calls auto-approve. This is the intended UX for fully autonomous mode.

**Hook requires Go toolchain at runtime (fallback path)**
The permission hook binary is pre-compiled in release tarballs. For `go install` users, the hook is compiled from embedded source on first daemon start, which requires `go` on PATH. If `go` is absent, the hook compilation is skipped and a warning is logged.
