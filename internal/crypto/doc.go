// Package crypto implements the C1 (精简版) end-to-end crypto layer specified in
// spec §11.C / D-12 — the "server-compromise confidentiality" guarantee for
// envelopes flowing daemon ↔ Mobile App through an honest-but-curious server.
//
// The server is a dumb relay: it sees only routing metadata
// (sessionId / fromDeviceId / toDeviceId / nonce / keyVersion / ts / opaque
// ciphertext) and never observes plaintext. Compromise of the server does not
// reveal envelope contents. See spec §11.C ("Server-compromise confidentiality
// guarantee", post P0-5 review rename).
//
// # Layering
//
//	┌────────────────────────────────────────────────────────────────────┐
//	│ pairing.go     One-time QR pairing: X25519 ECDH → device_pair      │
//	│                secret → HKDF-SHA256("tether-pair-v1") → wrap_key.  │
//	│                SAS fallback (6-digit) for server-mediated pairing  │
//	│                paths (only enabled in dev builds per spec §11.C).  │
//	├────────────────────────────────────────────────────────────────────┤
//	│ session.go     Per-cc-session ephemeral session_key (32B random),  │
//	│                wrapped under wrap_key for transport. Persisted to  │
//	│                disk encrypted under wrap_key for cc-session-length │
//	│                lifetime (spec §11.B B4: must survive daemon        │
//	│                restart so server-spooled ciphertext stays          │
//	│                decryptable). Rotated on cc-session start.          │
//	├────────────────────────────────────────────────────────────────────┤
//	│ xchacha20poly1305.go  Per-envelope AEAD seal/open. 24-byte random  │
//	│                nonce, AD = sessionId||fromDeviceId||toDeviceId     │
//	│                ||keyVersion. XChaCha20-Poly1305 — see spec §11.M.  │
//	├────────────────────────────────────────────────────────────────────┤
//	│ replay.go      Receive-side defenses: 5-minute timestamp skew      │
//	│                rejection + LRU envelope-id dedup window.           │
//	└────────────────────────────────────────────────────────────────────┘
//
// hkdf.go and x25519.go provide the lower-level primitives (HKDF-SHA256
// derivation, X25519 keypair gen + ECDH) reused by the rest of the layer.
//
// # Threat model (per spec §11.C)
//
//   - Server alone compromised: plaintext is safe. Server sees ciphertext +
//     metadata only.
//   - Device long-term wrap_key leak: future sessions exposed; already-ended
//     cc sessions remain safe (their session_key was wiped on cc kill).
//   - Daemon disk cold attack while cc session is alive: attacker can decrypt
//     that one session's spool (acknowledged cost — see §11.B / B4).
//
// True message-level forward secrecy (Signal-style ratchet) is explicitly
// out of scope for v0.1.
//
// # Cross-language interop (Tauri Mobile, Rust side)
//
// The Mobile App is a Tauri 2 application; its Rust core consumes/produces
// the same wire artifacts this Go package produces. The Rust counterpart
// uses:
//
//   - x25519-dalek for X25519 keypair + ECDH (matches RFC 7748)
//   - chacha20poly1305 crate for XChaCha20-Poly1305
//   - hkdf crate for HKDF-SHA256
//
// Wire compatibility is byte-for-byte: the Envelope struct in
// xchacha20poly1305.go documents the exact serialization layout that Rust
// must encode/decode. All multi-byte integers are big-endian; identifiers
// are length-prefixed UTF-8 strings; Additional Data (AD) is constructed
// with the exact deterministic concatenation defined in BuildAD.
//
// The HKDF info-strings ("tether-pair-v1", "tether-session-wrap-v1",
// "tether-session-key-v1", "tether-pair-sas") are stable wire constants —
// do not rename without a keyVersion bump (spec §11.C).
package crypto
