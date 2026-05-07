# `internal/transport/wt` — WebTransport server (slice #1, skeleton)

This package is the daemon-side **WebTransport-over-HTTP/3** server that
the desktop and mobile apps will dial into per spec
[`2026-04-26-tether-go-quic-design.md`] §3.3 / §11.V (D-13 / D-21).

## Status: SLICE #1 — server skeleton, NOT wired into the daemon

What this slice ships:

- `Server` boots an HTTP/3 + WebTransport listener on a UDP port.
- A single endpoint (`/wt`) accepts WT upgrades.
- Each accepted session runs an **echo handler** on its first
  client-opened bidi stream — read JSON, write the same bytes back.
  This is the smoke surface that proves the wire works without
  committing to channel-specific semantics yet.
- A `dev_cert.go` self-signed cert generator covers local dev. Prod
  callers pass `Cert` + `Key` via `Config` and bypass the dev path.

What this slice intentionally does **not** do:

| Future slice | Scope |
|---|---|
| **#2 — 5-channel multiplex** | Open the spec §3.3.3 channels (control / events / agent-bytes / catch-up / datagram) on top of the same `Session`. Replace the bidi echo with proper newline-delimited JSON envelope framing on each channel. |
| **#3 — envelope dispatch** | Decode §3.3.1 wire envelopes (id / sessionId / fromDeviceId / toDeviceId / kind / nonce / ciphertext), enforce replay window + dedup LRU, route to per-session pubsub. Cross-link with `internal/agent.LocalEnvelope` (wrap the local shape into a wire envelope when a remote subscriber is present). |
| **#4 — pairing + auth** | X25519 ECDH handshake from `internal/crypto`, device pairing flow, derive per-session keys, refuse unauthenticated upgrades. Replace the dev-cert "any origin allowed" with origin pinning. |
| **#5 — daemon wiring** | Mount the WT server as a watchdog-supervised subsystem in `internal/daemon`. Replace `tether attach` (the local UDS path) for the desktop/mobile clients per D-21. The UDS path stays for `tether attach` from the daemon-host shell. |

## Why a skeleton instead of one big PR

- Confidence in the `quic-go` + `webtransport-go` API surface is paid up-front and reused across slices.
- The 5-channel layout, envelope dispatch, and pairing each have their own design surface — coupling them with the listener bootstrap creates a 1500-LOC PR that can't be reviewed.
- Subsequent slices have a stable place to attach without churning the listener.

## Architectural decisions baked in

- **TLS 1.3 floor + ALPN `h3`** — HTTP/3 mandates both. Set explicitly via `tls.Config.MinVersion` + `NextProtos`.
- **Single endpoint `/wt`** — channels multiplex inside one WT session, not across URL paths.
- **Default UDP port `:4444`** — avoids conflict with PoC-2's `:4433` so dev workflows can run both. Configurable via `Config.Addr`.
- **Cert handling** — caller-supplied `Cert`+`Key` wins; fully empty triggers the auto-gen ECDSA P-256 dev cert (7-day validity, loopback SANs by default). Mixing one of cert/key without the other is rejected explicitly.
- **`webtransport.ConfigureHTTP3Server` is mandatory** — without it, the H3 SETTINGS frame doesn't advertise WebTransport and clients fail with "WebTransport not enabled" (PoC-2 step1 caught this).
- **`EnableStreamResetPartialDelivery: true` in QUIC config** — required by webtransport-go ≥ v0.10 (PoC-2 step5 caught this).
- **Server passive on stream open** — by convention the client opens the control stream first; the server `AcceptStream`s it. Server-initiated streams (events / catch-up) are opened from the server side in slice #2.
- **`*webtransport.Stream` exposed directly, not wrapped** — webtransport-go has its own `Stream` type distinct from `quic.Stream`. Wrapping it before slice #2 designs the dispatch surface would be premature.

## Quick local smoke (after slice #1 lands)

```go
ctx, cancel := context.WithCancel(context.Background())
defer cancel()

srv, err := wt.New(ctx, wt.Config{
    Addr:   ":4444",
    Logger: log.New(os.Stderr, "wt: ", log.LstdFlags),
})
if err != nil { /* handle */ }
go srv.Serve(ctx)
// dial via webtransport.Dialer to https://127.0.0.1:4444/wt, OpenStreamSync,
// write JSON, read it back.
```

The integration test in `server_test.go` is the executable form of this.

## Known gotchas (read before extending)

1. **Cert trust on macOS / Android dev devices** — self-signed certs require either trust-store install or the `serverCertificateHashes` browser API (Chromium / WebView). The dev cert generator exposes `SPKISHA256` so future slices can hand the fingerprint to a Tauri command.
2. **Port-binding under unprivileged users** — UDP `:4444` is fine. Anything `< 1024` requires CAP_NET_BIND_SERVICE on Linux or root on other Unixes; production deploys typically front this with a Cloudflare / Cloud Load Balancer that handles HTTPS termination at the edge — but WebTransport requires QUIC end-to-end, so termination must be QUIC-aware (Cloudflare's HTTP/3 product is, plain L4 LBs aren't).
3. **`quic.ErrServerClosed`** is the graceful-shutdown sentinel; `Serve` swallows it and returns nil. `http.ErrServerClosed` from the H3 layer is also swallowed.
4. **Ports + listener handoff for tests** — webtransport-go's `Server` doesn't expose the post-bind port when you `ListenAndServe` on `:0`. Use the `ServeListener` entrypoint with a pre-bound `net.PacketConn` instead — that's the test pattern.

## Spec cross-references

- §3.3 — wire model big picture
- §3.3.1 — envelope schema (slice #3)
- §3.3.3 — 5 channel layout (slice #2)
- §11.V — transport implementation choice
- D-13 / D-21 — desktop is always remote; v0.1 has no UDS direct path for desktop / mobile
- §10 PoC-2 — the listener bring-up smoke (already passed; this package is the production version)
