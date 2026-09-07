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
//
// Not validated automatically: read the rules above as prose, not as a checked
// contract.
//
// The cert-hash regex assertion sits in web/test/wire-contract.spec.ts under
// describe('contract-1: cert-hash format') and is declared { skip: !hasServer },
// where hasServer reads TETHER_TEST_URL. No workflow sets that variable
// (`git grep -n TETHER_TEST_URL -- .github/` finds nothing), so it is skipped on
// every CI run and fires only against a daemon someone points it at by hand.
//
// The assertion beside it does run in CI but cannot fail: HashHex64 is a plain
// alias, so `as HashHex64` accepts any string and expect(h.length).toBe(64)
// measures a literal the test built itself. `pnpm test` stays green even if
// this alias is retyped.
//
// What does pin the alias to a string is its consumer, web/src/lib/wt.ts, under
// the `tsc -b` that `pnpm build` runs: web/src is in tsconfig.app.json's
// include list while web/test is in no tsconfig project, so the build
// type-checks the caller and never the assertion above. That guards the Go-to-TS
// type, not the format.
//
// The format rules hold by construction and at runtime rather than by any CI
// gate: HashHex in internal/server/cert.go writes the digest through a fixed
// lowercase-hex table, and fetchCertHash in web/src/lib/wt.ts re-tests the
// response against the regex above and discards a value that fails it.
//
// Spec reference: D-22 §6 #1. K.7.1 is not a test id — it is the spec section
// cited by the codegen drift gate in .github/workflows/ci.yml.
type HashHex64 = string

// SessionID is the stable identifier for a cc-managed JSONL session,
// as returned in the system/init stream-json event (D-05a §3).
// Opaque string; format is cc-internal and subject to change.
type SessionID = string
