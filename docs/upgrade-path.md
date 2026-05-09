# Upgrade Path

## v0.1 → v0.1.1 (planned)

- Let's Encrypt integration: DNS-challenge or HTTP-01 challenge cert, no more self-signed warnings.
- PWA offline support: service worker + cache strategy.
- Partial-message streaming: `--include-partial-messages` mode for lower-latency chat.

## v0.1 → v0.2 (planned)

- **opencode provider**: D-17a §5.1 — stream-json compatible, requires opencode ≥0.4.
- Basic authentication: JWT (D-16 simplified), token-per-device.
- WebSocket fallback: for VPN/corporate environments that block UDP.
- `tether attach <sid>`: attach to an already-running session from a second device.

## v0.2 → v1.0 (roadmap)

- **WebCrypto E2E (D-12)**: ECDH key exchange, encrypted payloads. Daemon becomes a relay, not a decryption point.
- **Multi-user**: per-user session isolation, audit log.
- **`tether pair`**: QR-code-based device pairing (D-13).
- **Gemini / Cursor providers**: D-17a §5.2–5.3.
- **Let's Encrypt auto-renewal**: cert rotation without daemon restart.
- **Mobile PWA**: full offline + push notifications.
