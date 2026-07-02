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
| `TETHER_MCP_IDLE_TIMEOUT` | `15m` | Idle time before a per-task MCP instance is hibernated (child servers stopped, revived on next tool call). Set `0` to disable the watchdog. Accepts Go durations (e.g. `30m`). |

## CLI reference

```
tether server [--port PORT] [--cert-file F] [--key-file F] [--dev] [--dev-url URL]
tether doctor [--port PORT] [--verbose] [--json]
tether version
```

## Per-task MCP servers (v0.5)

A task can declare its own MCP servers in `<workspace>/.tether/task-config.json`. `tether` starts
them with the task and stops them on pause/wrap. File entries are merged with any `extra_servers`
passed to the task-MCP REST API — request keys win on collision.

```json
{
  "version": 1,
  "servers": {
    "playwright": {
      "command": ["npx", "-y", "@modelcontextprotocol/server-playwright"],
      "env": { "HEADLESS": "1" },
      "prefix": "pw",
      "inherit_env": ["PATH", "HOME"]
    }
  }
}
```

Idle task instances are hibernated by a watchdog (see `TETHER_MCP_IDLE_TIMEOUT`): their child MCP
servers are stopped to reclaim resources while the loopback and builtin tools keep serving, and the
next tool call transparently revives them.

---

## Troubleshooting

### K.8.1 — VPN / corporate firewall breaks the shell and chat channels

tether uses HTTP/3 (QUIC over UDP) for WebTransport. Many VPNs and corporate firewalls
proxy or drop UDP traffic, causing the WT handshake to fail silently even though the TCP
HTTPS page loads fine.

**Common offenders**:
- **Clash Verge (TUN mode)** on macOS — TUN intercepts all traffic including UDP; the
  QUIC handshake packet is proxied through TCP and dropped on the remote side.
- **Tailscale exit node** — routing all traffic through an exit node often blocks UDP/443.
- **Corporate VPN** — most enterprise VPNs proxy UDP or apply egress firewall rules that
  drop non-TCP traffic.

**Symptoms**: The SPA loads, but the chat/shell panes immediately show "Opening handshake
failed", "connect failed", or hang silently with no progress indicator.

**Fix**: Disconnect the VPN (or disable TUN mode / exit node). tether has no WebSocket/TCP
fallback in the current version (see [upgrade path](docs/upgrade-path.md)).

**Verify UDP reachability** before blaming other causes:

```bash
# Linux / macOS — should NOT return immediately with ICMP unreachable:
nc -u -z -w 3 <host> <port>

# macOS alternative via curl (QUIC):
curl -v --http3-only https://<host>:<port>/healthz 2>&1 | grep -E "Connected|failed"
```

If `nc -u` times out cleanly (no "Connection refused") but tether still fails, the UDP
packet is being dropped mid-path — that's a VPN/firewall, not a tether config issue.

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

> **Important**: `serverCertificateHashes` and CA-signed certs are mutually exclusive — see
> K.8.6 below. If you switch from a self-signed to a Let's Encrypt cert, you must also
> remove the hash from the frontend config.

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

### K.8.6 — `serverCertificateHashes` must not be passed with a CA-signed cert

The WebTransport JS API accepts `serverCertificateHashes` to pin a specific certificate
by hash, bypassing normal CA chain validation. This is designed for self-signed / dev certs.

**Chrome silently refuses** the WebTransport connection if you pass `serverCertificateHashes`
when the server presents a CA-signed cert (e.g. Let's Encrypt). The connection attempt
produces no JS error — it just never resolves, which looks identical to a UDP block.

| Cert type | `serverCertificateHashes` |
|---|---|
| Self-signed (default `~/.tether/cert.pem`) | **Required** — pass the SPKI hash |
| CA-signed (Let's Encrypt / custom CA) | **Must be omitted** — pass an empty array or omit the field |

**How to tell which cert tether is using**:
```bash
tether doctor --verbose   # cert-state check shows "self-signed" or "CA-signed (ACME)"
```

**Frontend config** (in `web/src/lib/transport.ts` — the relevant field):
```ts
// self-signed: include the hash
new WebTransport(url, { serverCertificateHashes: [{ algorithm: 'sha-256', value: hash }] });

// CA-signed: omit the field entirely
new WebTransport(url);
```

**Diagnosis**: if switching from self-signed to Let's Encrypt fixes TCP HTTPS but WT still
hangs, `serverCertificateHashes` is still being passed. Check the browser DevTools →
Network → the WebTransport entry; if it shows state "failed" immediately after "connecting",
this is the cause.
