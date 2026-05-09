// Package lock implements the per-session writer-lock state machine that
// gates "who can write PTY bytes" for a tether session (spec §8 + §11.D).
//
// PoC-1 (F-10/F-11) showed cc itself enforces a 3-layer permission model
// (plan / safe-cmd allowlist / mode+hook+TUI; see spec §5.5). That means
// tether's lock does NOT need to also gate "what mode cc is in" — the lock
// is single-layered and only governs writes to the PTY.
//
// Concrete contract (spec §11.D table):
//
//   - Holder is *ClientID; nil means "no holder" (anyone may write next).
//   - First byte from any client when holder=nil → that client takes the
//     lock implicitly (Acquire with reason=AcquireFirstByte).
//   - First byte from a non-holder when the current holder has been idle
//     longer than the auto-release window (default 60s) → the new client
//     takes over implicitly (auto-release fires before grant).
//   - First byte from a non-holder when the holder is active → rejected.
//     The caller is expected to surface an "in use" banner / 4xx-style
//     hint and offer ForceTakeover.
//   - ForceTakeover (terminal Ctrl+\ Ctrl+T or Mobile App "take over"
//     button) always succeeds for the requesting client.
//   - The current holder MUST call Touch on every PTY write so the
//     auto-release timer slides forward. The package does NOT assume any
//     ambient timer; the caller's input gate (or a periodic Sweep call)
//     drives auto-release.
//   - Every state change appends a TakeoverEvent to History so callers can
//     persist an audit log (spec §11.D "Audit log" requirement; the
//     write-to-disk side lives in the daemon, this package only collects
//     the in-memory record).
//
// Concurrency: Lock is safe for concurrent use. All mutating methods take
// an internal mutex; History returns a defensive copy.
//
// Persistence (spec §11.D "audit log" row): callers wire a LogSink via
// WithLogSink to persist every TakeoverEvent past process lifetime.
// The package ships NewJSONLLogSink which implements the on-disk JSONL
// format at `~/.tether/users/<user>/sessions/<sid>/lock.log` (path
// resolution is the caller's responsibility). Sink Append errors are
// best-effort — the in-memory state machine never blocks or rolls back
// on a sink failure; callers can route the error via
// WithSinkErrorHandler. v0.1 has no rotation (write forever); rotation
// is a §11.D follow-up (operator-managed for now).
//
// Out-of-scope (deferred):
//   - mode switching RPC (separate from lock; only the current holder is
//     allowed but that policy lives at the daemon RPC handler — this
//     package exposes IsHolder so the handler can enforce it cleanly)
//   - view-only attach / multi-holder fan-out (v0.1 not in scope per
//     §11.D)
//   - audit-log rotation/retention (none in v0.1; operator-managed)
package lock
