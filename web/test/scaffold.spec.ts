import { describe, expect, it } from 'vitest'

import * as wire from '../src/lib/wire.gen'

// Phase-1 scaffold (tether#174). This file exists to keep web/test/ a LIVE root,
// for two measured reasons.
//
// 1. web/test/ was a type-checking blind spot, and this wi closed it. Measured with
//    `tsc -p <project> --listFiles` on this worktree at baf7117, reproducing
//    tether#165's numbers exactly:
//
//        tsconfig.app.json   include ["src"]              -> 82 files under web/src
//        tsconfig.node.json  include ["vite.config.ts"]   ->  1 file
//        web/test/*.spec.ts  in NEITHER                   ->  0 files   (5 files existed)
//
//    tsconfig.app.json's include is now ["src", "test"], and ci.yml asserts the
//    result by counting `tsc -b --listFiles` output rather than by reading the glob.
//    A directory with no files in it cannot make that assertion fail, so an empty
//    web/test/ would leave the gate green while blind — which is the exact failure
//    class the gate was added to close.
//
// 2. tether#173 §4 ports three A-grade specs back into this directory
//    (test/wire-contract.spec.ts, test/latency.spec.ts, test/workspace-tree.spec.ts)
//    in the NEXT wi. They import across the root boundary, e.g. from `../src/lib/...`.
//    So the import below is the point of the test, not incidental: it is the hop
//    those files depend on, checked here before anything is ported onto it.
//
// Replace this file when that port lands. It has no reason to outlive it.
describe('phase-1 scaffold: web/test/ as a test root', () => {
  it('resolves imports from web/test into web/src', () => {
    expect(typeof wire.KindMessage).toBe('string')
  })

  it('sees the envelope kinds the ported contract specs assert on', () => {
    const kindKeys = Object.keys(wire).filter((k) => k.startsWith('Kind'))
    expect(kindKeys.length).toBeGreaterThan(0)
  })
})
