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
// that variable appears 0 times anywhere in this repository (measured,
// tether#189); contract-2's /wt/_smoke echo needs a browser WebTransport
// harness, and this project has no Playwright and no browser runner.
//
// What that file DOES hold, and it runs on every CI job via web's `pnpm test`,
// is the generated surface: the ErrCode* and FencedBlock* constant sets pinned
// by name and wire string (enumerated from the module, so a code added here goes
// red there), and Envelope's / ErrorPayload's required fields, pinned by
// TypeScript annotations that only became load-bearing when tether#174 put
// web/test/ inside the typecheck project. Change a json tag or add a const in
// this package and that spec is what tells you.
//
// Rule: only types that cross the wire belong here. Daemon-internal types
// live in internal/agent/event.go and are explicitly NOT present in
// wire.gen.ts (D-17a §7, D-22 §2.2).
package wire
