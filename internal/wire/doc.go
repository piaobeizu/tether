// Package wire defines the shared schema between the Go server and the
// TypeScript browser frontend. tygo generates a 1:1 TS file (wire.gen.ts)
// from this package; both sides import the same interface shape.
//
// Format conventions that types alone cannot express (e.g. hash with no
// colons, echo without prefix) are documented per-type in godoc and
// validated by web/test/wire-contract.spec.ts in CI (D-22 §6).
//
// Rule: only types that cross the wire belong here. Daemon-internal types
// live in internal/agent/event.go and are explicitly NOT present in
// wire.gen.ts (D-17a §7, D-22 §2.2).
package wire
