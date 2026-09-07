import { describe, expect, it } from 'vitest'

import * as wire from './lib/wire.gen'

// Phase-1 scaffold (tether#174). Two structural jobs, neither of them about the UI:
//
//  1. `pnpm test` is `vitest run`, which exits NON-ZERO when it finds no test files
//     at all, and ci.yml runs it. So the branch cannot be left with an empty suite,
//     and the scaffold has to keep it green without pretending to test a UI that
//     does not exist yet.
//  2. web/src/lib/wire.gen.ts is the only file this wi keeps out of the old
//     web/src, because it is the Go-to-frontend contract surface (tether#173
//     decision 4) and the object the codegen-drift gate in ci.yml diffs. A gate
//     that diffs a file nothing imports would still pass with the file corrupted
//     into something untypable, so something has to actually load it.
//
// The enumeration comes from the MODULE, via Object.keys — not from a hand-written
// list of the codes. That is the WIRE-8 lesson (tether#173 §5): the assertion in
// web/test/wire-contract.spec.ts:164 was named "exposes exactly the 8 ErrorCode
// constants" but built its expectation out of the same 8 names it then compared
// against, so it never counted anything and stayed green while two more load-bearing
// codes were added.
//
// This is deliberately NOT the WIRE-8 fix. It asserts a SHAPE (every ErrCode* export
// carries a non-empty string) and not a COUNT, because pinning the count is a
// contract test, contract tests belong to the ported A-grade suite, and that is
// tether#173 §4's wi — the next one. Adding a count here would put two competing
// enumerations of the same fact in the repo, which is how the original defect got in.
describe('wire.gen.ts (codegen contract surface)', () => {
  const errCodeKeys = Object.keys(wire).filter((k) => k.startsWith('ErrCode'))

  it('is importable and exposes ErrCode* runtime constants', () => {
    expect(errCodeKeys.length).toBeGreaterThan(0)
  })

  it('gives every ErrCode* export a non-empty string value', () => {
    const bad = errCodeKeys.filter((k) => {
      const v = (wire as Record<string, unknown>)[k]
      return typeof v !== 'string' || v.length === 0
    })
    expect(bad).toEqual([])
  })
})
