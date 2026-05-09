# tether

AI workspace daemon — single Go binary + React SPA over HTTP/3 + WebTransport.

Drives [Claude Code](https://claude.ai/code) via stream-json (chat) and PTY (shell), with a
permission UI for PreToolUse hooks.

## Quick start

```bash
# Install
go install github.com/piaobeizu/tether/cmd/tether@latest

# Start daemon (default port 8898)
tether server

# Or with a custom port
tether server --port 9000

# Dev mode (proxies SPA to a running Vite server)
tether server --dev
```

On first start:
- A self-signed ECDSA P-256 cert is generated at `~/.tether/cert.pem` (valid 14 days, auto-rotated).
- The permission hook binary is compiled to `~/.tether/bin/tether-permission-hook`.
- A `_tether_managed` PreToolUse entry is injected into `~/.config/claude/settings.json`.

## Preflight checks

```bash
tether doctor           # summary: [OK] / [ERR] per check
tether doctor --verbose # adds extra detail per check
tether doctor --json    # machine-readable JSON output
```

Checks: `cc-binary`, `data-dir`, `cert-state`, `port-bindable`, `cc-settings-hooks`.

## Environment variables

| Variable | Default | Purpose |
|---|---|---|
| `TETHER_NO_PERMISSION_HOOK` | `""` | Set to `1` to skip hook injection (auto-allows all tools) |
| `TETHER_HOST` | `127.0.0.1` | Hostname for startup log URL |
| `IS_SANDBOX` | auto-set if root | Injected into cc subprocess when running as root |

## CLI reference

```
tether server [--port PORT] [--cert-file F] [--key-file F] [--dev] [--dev-url URL]
tether doctor [--port PORT] [--verbose] [--json]
tether version
```

---

## Troubleshooting

### K.8.1 — VPN / corporate firewall breaks the shell and chat channels

tether uses HTTP/3 (QUIC over UDP) for WebTransport. Many VPNs and corporate firewalls
proxy or drop UDP traffic, causing the WT handshake to fail even though the TCP HTTPS page
loads fine.

**Symptoms**: The page loads, but the chat/shell panes show "connect failed" or hang silently.

**Fix**: Disconnect the VPN. tether has no TCP fallback in v0.1 (see [upgrade path](docs/upgrade-path.md)).

**Verify**: `udp:<host>:<port>` must be reachable from the browser. On Linux:
```bash
nc -u <host> <port>    # should not hang immediately
```

### K.8.2 — Browser version requirements

| Browser | Minimum version | Notes |
|---|---|---|
| Chrome / Chromium | 97+ | Full WebTransport support |
| Edge | 97+ | Same as Chrome |
| Firefox | Not supported | WebTransport API absent; see [MDN](https://developer.mozilla.org/en-US/docs/Web/API/WebTransport) |
| Safari | Not supported | WT not yet shipped |

**Recommendation**: Use Chrome 120+ for the best experience.

### K.8.3 — Self-signed cert: Chrome refuses to load the page

tether uses a self-signed ECDSA P-256 cert for v0.1. Chrome will show
`NET::ERR_CERT_AUTHORITY_INVALID` when connecting to a non-loopback address.

**Option A (recommended for localhost)**: Enable the insecure-localhost flag:
```
chrome://flags/#allow-insecure-localhost
```
Set to "Enabled" and relaunch Chrome. This only applies to `localhost`/`127.0.0.1`.

**Option B (LAN / remote host)**: Pass the SPKI hash on the Chrome command line:
```bash
# Get the SPKI hash from the running daemon:
curl -sk https://127.0.0.1:8898/cert-hash-spki

# Launch Chrome with cert bypass for that origin:
google-chrome --ignore-certificate-errors-spki-list=<hash> \
              --user-data-dir=/tmp/tether-chrome-profile \
              https://<host>:<port>/
```

**Option C (LAN / remote host — flag-based)**:
```
chrome://flags/#unsafely-treat-insecure-origin-as-secure
```
Add `https://<host>:<port>` to the list and relaunch.

> Note: WebTransport `serverCertificateHashes` still works for the WT channels regardless
> of TCP cert trust; the above is only needed to load the initial HTML page over TCP HTTPS.

### K.8.4 — Permission hook not firing

If the Claude Code chat window auto-approves tool calls without showing the tether UI:

1. Check `tether doctor --verbose` → `cc-settings-hooks` check.
2. If failing: restart `tether server` to re-inject the hook.
3. If cc was started before `tether server`, restart cc.
4. In `bypassPermissions` mode, cc intentionally skips hooks — this is expected behaviour.

### K.8.5 — Port 8898 already in use

```bash
# Find what's using the port:
lsof -i :8898

# Run tether on a different port:
tether server --port 9000
```
