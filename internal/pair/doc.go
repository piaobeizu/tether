// Package pair implements the device-pair protocol specified in
// `tether-doc/wiki/specs/2026-05-07-pairing-protocol.md` (§11.AB of the
// main design doc). This is the slice-#4 implementation that turns the
// spec's prose into a concrete state machine + frame codec + persistence
// layer.
//
// # Scope
//
// Two devices that have never met (e.g. user's desktop + user's phone)
// run this protocol over the control channel of a WebTransport session
// and emerge with:
//
//   - A 32-byte long-term key (the input to §11.C envelope encryption's
//     wrap_key role)
//   - A 32-byte transport-binding key (reserved for v0.1.x channel
//     binding; derived now to avoid future on-disk migration)
//   - A persisted device record at
//     `~/.tether/users/<user>/devices/<deviceId>.json` (mode 0600)
//
// Active-MITM resistance during the pair window is provided by a Short
// Authentication String (SAS) that the user compares out-of-band — both
// endpoints derive the same 6-character base32 code only if the same
// X25519 ephemeral pubkeys and transcript reached both sides.
//
// # Layering
//
//	┌────────────────────────────────────────────────────────────────────┐
//	│ pair.go            Frame structs + envelope wrapping (§3.3.1).     │
//	│ fsm.go             Pure Mealy state machine: (state, event) →      │
//	│                    (newState, outFrames). No I/O. Anti-replay      │
//	│                    (per-direction strictly-monotonic ts) + per-    │
//	│                    state allowed-frame matrix from spec §6.        │
//	│ sas.go             SAS derivation (HKDF → 30 bits → base32        │
//	│                    alphabet) + ConfirmMAC + LongTermKey            │
//	│                    derivation. Pinned algorithm per spec §4 / §8.  │
//	│ transcript.go      Append-only running-SHA256 over canonicalized   │
//	│                    frames. JCS (RFC 8785) + 4-byte big-endian      │
//	│                    length prefix per concatenated frame.           │
//	│ client.go          Initiator-side driver — runs the FSM against an │
//	│                    io.ReadWriter control stream, returns a Result  │
//	│                    on `paired`.                                    │
//	│ server.go          Daemon/responder-side driver — same shape.      │
//	│ registry.go        Persistence: per-device JSON files at 0600,     │
//	│                    parent dirs at 0700, atomic write via tmp +     │
//	│                    rename. Re-pair default = reject (Q2).          │
//	│ audit.go           Append-only JSONL audit log at                  │
//	│                    `~/.tether/users/<user>/audit.log` for          │
//	│                    pair.success / pair.fail / pair.repair-rejected │
//	│                    / pair.force-rotated events.                    │
//	└────────────────────────────────────────────────────────────────────┘
//
// # Design choices
//
//   - Canonical frame encoding for transcript: deterministic JSON (sorted
//     keys, no whitespace, UTF-8) per RFC 8785 / JCS, length-prefixed by
//     4-byte big-endian. Implemented inline rather than pulling a JCS
//     library because our frame fields are flat and well-typed; sorted-
//     key marshaling is one pass over a map[string]any.
//
//   - HKDF info strings (one per derived key, all distinct, all pinned
//     in code with comments matching the spec):
//
//     * "tether-sas-v1"            — sas_key (also MAC key)
//     * "tether-sas-display-v1"    — sas_bits (30 bits → 6 base32 chars)
//     * "tether-pair-confirm-v1|"  — HMAC label prefix for SAS-confirm
//     * "tether-ltk-v1"            — long_term_key (32B)
//     * "tether-tbk-v1"            — transport_binding_key (32B)
//
//   - Re-pair default = reject (spec §14 Q2 ratification): Registry.Save
//     returns ErrAlreadyPaired if a record for the deviceId exists.
//     Operators must explicitly call ForceSave to overwrite, which also
//     emits a pair.force-rotated audit-log line.
//
//   - Audit log: shares the §11.D file at
//     `~/.tether/users/<user>/audit.log`. Append-only JSONL, file mode
//     0600. Pair events use the kinds documented on AuditKind below.
//
// # Out of scope (handled by separate slices)
//
//   - Daemon integration (dispatch of pair.* envelopes into Run()).
//   - WT transport details (the io.ReadWriter parameter is the seam).
//   - UI / Tauri side.
//   - FCM/APNs token sending (v0.1 stores the token verbatim only).
package pair
