# `internal/transport/wt` — WebTransport server (slice #2, channel multiplex)

This package is the daemon-side **WebTransport-over-HTTP/3** server that
the desktop and mobile apps will dial into per spec
[`2026-04-26-tether-go-quic-design.md`] §3.3 / §11.V (D-13 / D-21).

## Status: SLICE #2 — channel multiplex layered onto the slice #1 listener

Slice #1 shipped the listener skeleton + a single-stream JSON echo handler.
Slice #2 replaces that with the real **5-channel router** from §3.3.3.

### What this slice ships

- A 1-byte channel-id wire convention on every stream + datagram.
- Per-`Session` accept loop demuxing incoming streams onto per-channel
  queues (control / events / agent-bytes / catch-up / datagram).
- Server `Session` API: `Control` / `Events` / `OpenAgentBytes` /
  `AcceptCatchUp` / `SendDatagram` / `RecvDatagram`, plus the symmetric
  Open/Accept variants for direction parity.
- A Go-side `Client` mirroring the same conventions (used by tests +
  future internal callers).
- A watchdog-supervised `wtSubsystem` in `internal/daemon` that owns
  the listener lifecycle. Opt-in via `daemon.Config.WTListenAddr` (or
  `cmd/tether daemon --wt-addr`); the v0.1 default is empty (UDS attach
  remains the local surface).

### What this slice intentionally does NOT do

| Future slice | Scope |
|---|---|
| **#3 — envelope dispatch** | Decode §3.3.1 wire envelopes (id / sessionId / fromDeviceId / toDeviceId / kind / nonce / ciphertext), enforce replay window + dedup LRU, route to per-session pubsub. The events channel carries opaque bytes today; slice #3 layers JSON-NL framing + envelope routing on top. |
| **#4 — pairing + auth** | X25519 ECDH handshake from `internal/crypto`, device pairing flow, derive per-session keys, refuse unauthenticated upgrades. Replace the dev-cert "any origin allowed" with origin pinning. |
| **#5 — replace UDS attach** | Switch desktop / mobile clients off the local Unix socket onto WT entirely; the local `tether attach` shell stays on UDS by design. |

## Wire convention

Every stream and every datagram carries a one-byte **channel-id** as
its first byte:

```
0x01 = control      (bidi, client → server initiated)
0x02 = events       (bidi, server → client initiated)
0x03 = agent-bytes  (uni,  server → client only — D-14: not consumed remotely in v0.1)
0x04 = catch-up     (bidi, client → server initiated)
0x05 = datagram     (tag for unreliable health-probe payloads; never appears on a stream)
```

The receiving side reads the byte off any newly-accepted stream and
routes it to the per-channel queue. **Bad / unknown tags trigger a
stream-level reset (`streamErrorCodeBadChannelID = 0x01`); the session
itself is NOT torn down** — one buggy or hostile stream cannot poison
a healthy session.

A stream that opens but never sends the tag would otherwise pin a
goroutine; the receiving side's `readChannelID` issues an `io.ReadFull`
with no explicit deadline today (the WT session context cancels it on
close), and the **5-second eviction budget tracked in `defaultChannelIDDeadline` is reserved for slice #3 hardening** — see "Known
gotchas" below.

## API surface

### Server-side (`Session`)

```go
sess.Control(ctx)            // accept client-initiated bidi  (0x01)
sess.Events(ctx)             // open server-initiated bidi    (0x02)
sess.OpenAgentBytes(ctx)     // open server-initiated uni     (0x03)
sess.AcceptCatchUp(ctx)      // accept client-initiated bidi  (0x04)
sess.SendDatagram(payload)   // unreliable                    (0x05)
sess.RecvDatagram(ctx)       // unreliable                    (0x05)
```

Symmetric helpers also exist (`OpenControl`, `AcceptEvents`,
`AcceptAgentBytes`, `OpenCatchUp`) so both ends can speak the inverse
direction when needed (e.g. server-initiated control RPC in a future
slice).

### Client-side (`Client`)

The mirror image. The client opens control + catch-up; accepts events
+ agent-bytes; sends/receives datagrams. `OpenRawTagged(tag byte)` is
an escape hatch for negative tests that need to drive bad tags into
the server's accept loop.

### Session handler injection

`Config.SessionHandler func(*Session)` lets the integrator wire a
per-session handler. Default: log accept + sit on the session until
close (used by the daemon `wtSubsystem` in slice #2). Tests inject
per-channel echo handlers to drive the router end-to-end.

## Architectural decisions baked in

- **1-byte channel-id (not varint)** — 5 channels today, 251 spare;
  fixed width keeps the read path allocation-free and matches the
  spec's use of "stream / datagram tag" not "stream type id".
- **Default session handler is "sit on the session"** — not a stream
  echo; envelope dispatch is a slice #3 concern. Tests inject
  channel-aware echo handlers.
- **Server runs the accept loop, callers consume from queues** — keeps
  the demux off the hot path and means `Control(ctx)` etc. are simple
  receives. Per-channel queue capacity is 16 (small constant; no
  channel needs deep buffering at v0.1 traffic levels).
- **Bad channel-id resets the offending stream, not the session** —
  one buggy / hostile stream cannot poison a healthy session, matching
  the wire-level "stream is the failure unit" model of QUIC.
- **`MaxIncomingStreams` defaults from the underlying QUIC config** —
  webtransport-go inherits these from the H3 server. The dev-cert
  client picks 64 each (uni + bidi); production overrides via
  `Config.QUIC*` knobs once added in slice #4.
- **Go-side `Client` is exported** — needed for tests + future
  internal callers (cross-host pairing, internal probes). Tauri /
  mobile clients re-implement the wire convention in
  TypeScript / Kotlin.
- **`agent-bytes` is uni only** — server-only writer per §3.3.3
  (PTY raw bytes flow server→client). The reverse direction has no
  v0.1 consumer; if a slice #5 ever needs it, the convention extends
  cleanly (just open a uni from the client side with `0x03`).
- **`wtSubsystem` is opt-in** — `WTListenAddr == "" && WTListener == nil`
  means the watchdog never spawns it. Keeps the v0.1 default surface
  identical to pre-slice-#2 (UDS attach only).

## Quick local smoke

```go
ctx, cancel := context.WithCancel(context.Background())
defer cancel()

srv, err := wt.New(ctx, wt.Config{
    Addr:   ":4444",
    Logger: log.New(os.Stderr, "wt: ", log.LstdFlags),
    SessionHandler: func(sess *wt.Session) {
        ctrl, _ := sess.Control(ctx)
        defer ctrl.Close()
        io.Copy(ctrl, ctrl)
    },
})
if err != nil { /* handle */ }
go srv.Serve(ctx)
// dial via wt.Dial → cli.OpenControl(ctx) → write+close → ReadAll
```

The integration tests in `server_test.go` are the executable form of
the full 5-channel surface.

## Known gotchas (read before extending)

1. **Cert trust on macOS / Android dev devices** — self-signed certs
   require either trust-store install or the `serverCertificateHashes`
   browser API (Chromium / WebView). The dev cert generator exposes
   `DERSHA256` (sha256 of the DER-encoded leaf cert, matching the W3C
   `serverCertificateHashes.value` algorithm) so future slices can
   hand the fingerprint to a Tauri command. The Rust client pins by
   the same algorithm via
   `web-transport-quinn::ClientBuilder::with_server_certificate_hashes`.
   Slice #3 renamed this from `SPKISHA256` after discovering the SPKI
   form did not interop cross-stack.
2. **Port-binding under unprivileged users** — UDP `:4444` is fine.
   Anything `< 1024` requires CAP_NET_BIND_SERVICE on Linux or root on
   other Unixes.
3. **`quic.ErrServerClosed`** is the graceful-shutdown sentinel;
   `Serve` swallows it and returns nil. `http.ErrServerClosed` from
   the H3 layer is also swallowed.
4. **Listener handoff for tests** — webtransport-go's `Server` doesn't
   expose the post-bind port when you `ListenAndServe` on `:0`. Use
   the `ServeListener` entrypoint with a pre-bound `net.PacketConn`
   instead — that's the test pattern (`bootTestServer` in
   `server_test.go`).
5. **Stream-tag eviction is "best-effort" today** — a stream that
   opens and never writes its 1-byte tag pins a routing goroutine
   until the session-level context closes. v0.1 trusts paired peers;
   slice #4 (auth) will tighten this with a per-stream
   `SetReadDeadline(now + defaultChannelIDDeadline)`.
6. **Datagrams require `EnableDatagrams: true` on BOTH sides** —
   already on in the server's `quic.Config`; the test client and
   `wt.Dial` set it explicitly. Forgetting it on a custom client
   silently turns `SendDatagram` into a no-op.

## Spec cross-references

- §3.3 — wire model big picture
- §3.3.1 — envelope schema (slice #3)
- §3.3.3 — 5 channel layout (this slice)
- §11.V — transport implementation choice
- D-13 / D-21 — desktop is always remote; v0.1 has no UDS direct path for desktop / mobile
- D-14 — agent-bytes channel reserved but not consumed remotely in v0.1
- §10 PoC-2 — the listener bring-up smoke (already passed; this package is the production version)
