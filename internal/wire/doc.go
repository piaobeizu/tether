// Package wire defines the shared schema between the Go server and the
// TypeScript browser frontend. tygo generates a 1:1 TS file (wire.gen.ts)
// from this package; both sides import the same interface shape.
//
// Format conventions that types alone cannot express (e.g. hash with no
// colons, echo without prefix) are documented per-type in godoc and are
// enforced by NOTHING automatic. Read them as prose, not as a checked contract.
//
// 🔴 This paragraph used to end "and validated by web/test/wire-contract.spec.ts
// in CI (D-22 §6)", which was false in two independent ways, and it is corrected
// here rather than deleted because a claim that a gate exists is the one thing
// nobody re-checks. (1) That file was one of the 84 tether#174 deleted, so from
// that commit until tether#189 ported it back the named gate did not exist at
// all. (2) Even with it back, its two FORMAT contracts are inert in CI, which is
// the half the old sentence got wrong even before the deletion: contract-1's
// cert-hash regex only runs against a live daemon named by TETHER_TEST_URL, and
// NOTHING IN THIS REPOSITORY SETS that variable — `git grep` finds it only in
// the spec line that reads it and in this sentence, and zero times in .github/,
// the Makefile or scripts/; contract-2's /wt/_smoke echo needs a browser
// WebTransport harness, and playwright is in neither dependencies nor
// devDependencies of web/package.json, has no tracked config, and is not
// installed. (Both measured in tether#189.)
//
// What that file DOES hold — and .github/workflows/ci.yml runs it, as `cd web &&
// pnpm test` — is the generated surface: the ErrCode* and FencedBlock* constant
// sets pinned by name and wire string (enumerated from the module, so a code
// added here goes red there), and Envelope's / ErrorPayload's required fields,
// pinned by TypeScript annotations that only became load-bearing when tether#174
// put web/test/ inside the typecheck project. Change a json tag or add a const
// in this package and that spec is what tells you.
//
// ⚠️ format.go's HashHex64 godoc still carries the same overclaim ("Validated by
// wire-contract.spec.ts contract K.7.1 / D-22 §6 #1"). It is left alone here on
// purpose, not overlooked: tygo renders that comment verbatim into
// web/src/lib/wire.gen.ts, so editing it is a codegen change gated by CI's drift
// step, and tether#189's declared scope is this file. Read it with the paragraph
// above, not on its own.
//
// Rule: only types that cross the wire belong here. Daemon-internal types
// live in internal/agent/event.go and are explicitly NOT present in
// wire.gen.ts (D-17a §7, D-22 §2.2).
package wire
