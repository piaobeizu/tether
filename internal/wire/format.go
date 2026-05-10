package wire

// HashHex64 is the canonical wire format for SHA-256 fingerprints:
// 64 lowercase hex characters, no separators, no prefix.
//
// Browser parser regex: ^[0-9a-f]{64}$
//
// Accepted:   "4c55656033cc29b05037294b2c1bdd2c294963b57b9238a2a758aec8d87eb288"
// Rejected:   "4c:55:65:..." (colons), "4C55..." (uppercase), "0x4c55..." (prefix)
//
// Both /cert-hash (DER) and /cert-hash-spki (SPKI) return values of this type.
// Validated by wire-contract.spec.ts contract K.7.1 / D-22 §6 #1.
type HashHex64 = string

// SessionID is the stable identifier for a cc-managed JSONL session,
// as returned in the system/init stream-json event (D-05a §3).
// Opaque string; format is cc-internal and subject to change.
type SessionID = string
