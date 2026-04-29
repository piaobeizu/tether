# Step 3: Browser ↔ Go WebTransport

## What this validates

Chromium's native WebTransport API can speak to our `webtransport-go`
v0.10 server end-to-end. Validates the v0.1 main path under D-13:
Android Tauri Mobile uses Chromium WebView with native WT.

Desktop Chrome and Android Chromium share the same WT implementation
core, so testing in desktop Chrome covers Android too (modulo minor
version skew on older Android devices).

## Run

Two processes:

```bash
cd poc/go-quic-wt
go build -tags step1 -o bin-step1 .   # WT echo server (UDP/4433)
go build -tags step3 -o bin-step3 .   # static HTTP server (TCP/8002)

# Terminal A
./bin-step1
# Terminal B
./bin-step3
```

Then open the page:

- **PC localhost** (always works): http://127.0.0.1:8002/
- **Phone via LAN**: http://&lt;PC-IP&gt;:8002/ (see Secure Context below)

Click "Run tests" in the page. Expect 5 ✓ and "STEP 3 PASS".

## Secure Context caveat (important for phone)

WebTransport API requires the page to be a **secure context**:

- ✅ `localhost` / `127.0.0.1` — automatic
- ✅ `https://...` (any host) — works after cert click-through
- ❌ `http://192.168.x.x:8002/` — **WT API will refuse to dial**

### Workarounds for phone testing

**Option A: Chrome desktop with origin allowlist** (PC test only)

```
chrome://flags/#unsafely-treat-insecure-origin-as-secure
→ add http://<PC-IP>:8002 → relaunch Chrome
```

**Option B: HTTPS via tunneling service** (phone friendly)

Run a tunnel to expose 8002 as public HTTPS:

```bash
# Cloudflare Tunnel (recommended)
cloudflared tunnel --url http://localhost:8002
# → prints https://<random>.trycloudflare.com

# OR Tailscale Funnel (if you have Tailscale)
tailscale funnel 8002
```

Phone Chrome → that HTTPS URL. Page is now secure context.

**Option C: Native HTTPS with self-signed cert + click-through** (advanced)

Run `step3_static.go` over HTTPS instead of HTTP (would need to extend
this PoC). Phone visits `https://<PC-IP>:8002/`, taps "Advanced →
Proceed", page loads with cert warning **but is treated as secure
context** by WT API.

For PoC step 3 we recommend **Option B (Cloudflare Tunnel)** — fastest
to phone, no flag fiddling.

## What the page does

1. Auto-fetches the current `webtransport-go` server cert SHA-256 hash
   from `/cert-hash` (file written by `bin-step1`).
2. Constructs `new WebTransport(url, { serverCertificateHashes: ... })`
   — Chrome's API for self-signed certs (requires ECDSA + ≤14d validity,
   our `certs.go` complies).
3. Runs 5 tests:
   - **dial**: session opens, `await session.ready` resolves
   - **bidi stream**: open, write, half-close, read echo, byte-equal
   - **datagram**: write, recv with 3s timeout, byte-equal
   - **multi stream**: 3 bidi streams concurrent, all echo
   - **close clean**: explicit `session.close()` + `await session.closed`

## Pass criteria

All 5 ✓ and "STEP 3 PASS" badge. If any fails, the per-test row shows
the error string from the WT API.

## Reverse-validating Android

Once desktop Chrome passes, the same HTML loaded inside a Tauri Android
WebView should behave identically (same Chromium WT core). When v0.1
Mobile App development starts, mount this exact HTML inside the Tauri
WebView for a smoke check before doing real React UI.

## Caveats this DOES NOT cover

- Real cellular carrier-grade NAT / 4G UDP path (→ step 4)
- Long-running connection stability (→ step 6)
- Tauri-specific shell ↔ WebView bridge (deferred to actual mobile dev)
- iOS WKWebView (D-13: iOS推迟到 v0.1.x，需要 quinn polyfill plugin)
