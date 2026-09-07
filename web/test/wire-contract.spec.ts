/**
 * D-22 §6 wire-contract tests — the contracts that define the tether wire
 * protocol. Ported back into the phase-1 tree by tether#189 (tether#173 §4);
 * the pre-rewrite copy is at baf7117:web/test/wire-contract.spec.ts.
 *
 * ── What actually holds these contracts up, and what does not ───────────────
 *
 * Read this before adding a contract here, because two DIFFERENT mechanisms are
 * at work and only one of them is a `expect()` call:
 *
 *  1. **The type annotations, at typecheck time.** These are what pin the
 *     GENERATED SHAPES. `const minimal: Envelope = { kind: ... }` compiles only
 *     while `kind` is Envelope's one required field, and an object literal typed
 *     as `ErrorPayload` is rejected for both a missing required field and an
 *     excess one. This half was DEAD in the pre-rewrite copy: web/test/ was in
 *     no tsconfig project (measured on baf7117 — tsconfig.app.json's include was
 *     ["src"], 82 files; the five web/test/*.spec.ts were in 0 projects), so
 *     every annotation and every `as EnvelopeKind` in that file was unchecked
 *     text. tether#174 put web/test/ inside tsconfig.app.json's include, and
 *     .github/workflows/ci.yml asserts that coverage by counting `tsc -b
 *     --listFiles` output rather than by reading the glob. That is why the
 *     annotations below are load-bearing NOW and were not before.
 *
 *  2. **The runtime assertions**, which can only see what survives type
 *     erasure: the `export const` values. Those are enumerated FROM THE MODULE
 *     and compared against a hand-written pin — see the WIRE-8 note below.
 *
 * 🔴 Do NOT let a runtime assertion stand in for (1). The `Object.keys(parsed)`
 * assertion in contract-7 operates on an object THIS FILE built, so it pins the
 * JSON key spelling of a literal and nothing else — in particular it is not an
 * Envelope guard, and tether#174 recorded someone reading it as one. Contract-3's
 * `Envelope` annotation is the Envelope guard.
 *
 * ── WIRE-8: why the enumerations below derive from the module ───────────────
 *
 * The pre-rewrite copy's last test was named "exposes exactly the 8 ErrorCode
 * constants" and built its subject out of the same 8 names it then compared
 * against — there was no counting step anywhere in it. It stayed green while two
 * more load-bearing codes were added to internal/wire/errors.go
 * (ErrCodeSessionHeldByBackgroundAgent, tether#104; ErrCodePromptUndelivered,
 * tether#77), so the file said 8 while the module exported 10. Measured again on
 * this worktree before rewriting it: `grep -c '^export const ErrCode'
 * web/src/lib/wire.gen.ts` = 10.
 *
 * Contract-5 had the identical defect and tether#173 §5 did not list it: it was
 * named "FencedBlockKind enum has exactly 5 values" and asserted
 * `expectedKinds.length === 5` on its own 5-element literal. Its `FencedBlockKind[]`
 * annotation bought nothing either, because tygo emits `FencedBlockKind` as a bare
 * `type ... = string` — so a sixth kind in Go was green in both mechanisms at once.
 *
 * So: the SUBJECT of a set assertion is enumerated from `wire` with Object.entries;
 * the EXPECTATION is a hand-written pin. Both halves are needed and they are not
 * interchangeable — a pin on both sides counts nothing (that was WIRE-8), and a
 * derivation on both sides asserts nothing (`Object.keys(m).length === Object.keys(m).length`).
 * Adding a code or a kind on the Go side is a wire-format change, so turning this
 * red and being updated deliberately is the intended behaviour, not friction.
 */
import { describe, expect, it } from 'vitest'

import type {
  Envelope,
  EnvelopeKind,
  ErrorCode,
  ErrorPayload,
  FencedBlock,
  FencedBlockKind,
  HashHex64,
  SessionID,
} from '../src/lib/wire.gen'
import * as wire from '../src/lib/wire.gen'

// This package deliberately has no @types/node — tsconfig.app.json sets no
// "types" and its lib is ES2022 + DOM only — so `process` is not in scope for
// tsc. Declared module-scoped for the same reason web/vite.config.ts declares
// it: it keeps a whole @types package out for one string read, and being
// module-scoped it does not collide if @types/node ever does arrive.
declare const process: { env: Record<string, string | undefined> }

const testURL = typeof process !== 'undefined' ? process.env['TETHER_TEST_URL'] : undefined
const hasServer = Boolean(testURL)

// ─── Contract 1: GET /cert-hash returns a 64-char lowercase hex string ────────
//
// 🔴 The live half is NOT a CI gate and never has been. `TETHER_TEST_URL` appears
// 0 times in this repository (workflows, Makefile, scripts/, sources — measured
// in tether#189), so the test below is skipped on every CI run and only fires
// when someone points it at a running daemon by hand. The format convention
// itself ("64 lowercase hex, no colons, no prefix") is enforced nowhere
// automatically; internal/wire/doc.go says so in those words rather than
// claiming this file validates it.

describe('contract-1: cert-hash format', () => {
  it('matches ^[0-9a-f]{64}$', { skip: !hasServer }, async () => {
    const res = await fetch(`${testURL}/cert-hash`)
    expect(res.ok).toBe(true)
    const hash = (await res.text()).trim()
    expect(hash).toMatch(/^[0-9a-f]{64}$/)
  })

  // A typecheck assertion wearing a runtime test's clothes: HashHex64 is erased,
  // so `expect(h.length)` is trivially true and the annotation is the whole
  // content. It pins that tygo still emits HashHex64 as a plain string alias —
  // if internal/wire/format.go turned it into a struct, the line stops compiling.
  it('HashHex64 is still a plain string alias a 64-char hex literal satisfies', () => {
    const h: HashHex64 = 'a'.repeat(64) as HashHex64
    expect(h.length).toBe(64)
  })
})

// ─── Contract 2: WT bidi echo at /wt/_smoke round-trips pure bytes ───────────
//
// Browser-only: WebTransport does not exist in Node, so jsdom cannot reach it.
//
// 🔴 UNCOVERED, and the pre-rewrite copy of this comment pointed at cover that
// no longer exists — it said "Covered by Playwright e2e / web/test/e2e/smoke.spec.ts".
// That file is one of the 84 tether#174 deleted, and `playwright` appears 0 times
// in web/package.json, the Makefile and .github/ (measured, tether#189). There is
// no browser harness in this project to host it.
describe('contract-2: WT bidi echo', () => {
  it.skip('needs a browser WebTransport harness this project does not have', () => {
    // Intentionally empty. Kept as the record that D-22 §6 #2 exists and is
    // unverified, rather than deleted into silence.
  })
})

// ─── Contract 3: Envelope shape {kind, sessionId?, payload?} ─────────────────
//
// The `Envelope` annotations here are the Envelope guard (see the header): a new
// REQUIRED field on wire.Envelope stops `minimal` compiling. An optional one does
// not, by design — optional is a compatible addition.

describe('contract-3: Envelope shape', () => {
  it('kind is required; sessionId and payload are optional', () => {
    const minimal: Envelope = { kind: 'message' as EnvelopeKind }
    expect(minimal.kind).toBe('message')
    expect(minimal.sessionId).toBeUndefined()
    expect(minimal.payload).toBeUndefined()
  })

  it('full Envelope serialises to expected JSON keys', () => {
    const env: Envelope = {
      kind: 'permission' as EnvelopeKind,
      sessionId: 'abc123' as SessionID,
      payload: { id: 'x', toolName: 'Bash', input: {} },
    }
    const json = JSON.stringify(env)
    const parsed = JSON.parse(json) as Record<string, unknown>
    expect(parsed['kind']).toBe('permission')
    expect(parsed['sessionId']).toBe('abc123')
    expect(parsed['payload']).toBeDefined()
  })
})

// ─── Contract 4: SessionID is a plain string type ────────────────────────────

describe('contract-4: SessionID format', () => {
  it('SessionID accepts a non-empty string', () => {
    const sid: SessionID = 'some-uuid-or-hex' as SessionID
    expect(typeof sid).toBe('string')
    expect(sid.length).toBeGreaterThan(0)
  })
})

// ─── Contract 5: the FencedBlockKind constants are exactly these 5 ───────────

describe('contract-5: FencedBlockKind constants', () => {
  // Subject: enumerated from the module. Expectation: the pin. See the header.
  const exported = Object.fromEntries(
    Object.entries(wire).filter(([name]) => name.startsWith('FencedBlock')),
  )

  it('pins every FencedBlock* export the module has, by name and wire string', () => {
    const pinned: Record<string, FencedBlockKind> = {
      FencedBlockDag: 'dag',
      FencedBlockForm: 'form',
      FencedBlockCandidates: 'candidates',
      FencedBlockMedia: 'media',
      FencedBlockPermission: 'permission',
    }
    expect(exported).toEqual(pinned)
  })

  it('FencedBlock round-trips through JSON with correct kind discriminator', () => {
    const kinds = Object.values(exported) as FencedBlockKind[]
    expect(kinds.length).toBeGreaterThan(0)
    for (const kind of kinds) {
      const block: FencedBlock = { kind, skill: 'test', content: '{}' }
      const parsed = JSON.parse(JSON.stringify(block)) as FencedBlock
      expect(parsed.kind).toBe(kind)
    }
  })
})

// ─── Contract 6: tool_use input is opaque (unknown/any in TS) ────────────────

describe('contract-6: tool_use input shape', () => {
  it('Envelope payload can carry a tool_use object with unknown input', () => {
    const env: Envelope = {
      kind: 'message' as EnvelopeKind,
      payload: {
        type: 'tool_use',
        id: 'toolu_01abc',
        name: 'Bash',
        input: { command: 'echo hello' }, // opaque — no fixed schema
      },
    }
    const tool = env.payload as { type: string; name: string; input: unknown }
    expect(tool.type).toBe('tool_use')
    expect(tool.name).toBe('Bash')
    expect(tool.input).toBeDefined()
  })
})

// ─── Contract 7: KindError payload carries {code, message, terminal} (tether#63) ──
// Before this contract, a KindError envelope's payload was a bare string with
// no disposition — the browser could not tell a permanent refusal from a
// transient drop without pattern-matching text. This pins the shape tygo
// generates from internal/wire/errors.go so a future Go-side change to
// ErrorPayload's json tags is caught here, in the one place both sides of the
// wire boundary are checked against each other.
describe('contract-7: ErrorPayload shape', () => {
  it('KindError is the string "error"', () => {
    expect(wire.KindError).toBe('error')
  })

  it('ErrorPayload round-trips through JSON with its three lower_snake_case keys', () => {
    // 🔴 The `ErrorPayload` annotation on `payload` is what carries this test: it
    // rejects a missing required field and an excess one. The Object.keys check
    // below re-reads a literal written three lines up, so on its own it would pin
    // nothing — and it is NOT a guard on Envelope (header, point 1).
    const payload: ErrorPayload = {
      code: wire.ErrCodeUnknownWorkspace,
      message: 'unknown workspace "foo"',
      terminal: true,
    }
    const json = JSON.stringify(payload)
    const parsed = JSON.parse(json) as Record<string, unknown>
    expect(Object.keys(parsed).sort()).toEqual(['code', 'message', 'terminal'])
    expect(parsed['code']).toBe('unknown_workspace')
    expect(parsed['message']).toBe('unknown workspace "foo"')
    expect(parsed['terminal']).toBe(true)
  })

  it('an Envelope of kind error carries an ErrorPayload', () => {
    const env: Envelope = {
      kind: wire.KindError,
      payload: {
        code: wire.ErrCodeSessionOwned,
        message: 'session owned by another client',
        terminal: true,
      } as ErrorPayload,
    }
    expect(env.kind).toBe('error')
    const p = env.payload as ErrorPayload
    expect(p.terminal).toBe(true)
  })

  // ─── WIRE-8, rebuilt ──────────────────────────────────────────────────────
  //
  // The subject is enumerated from the module; the expectation is the pin. Header
  // has the full history of why the previous version of this test counted nothing.
  //
  // What this catches, all four directions: an ErrCode* export ADDED on the Go
  // side (extra key on the left), REMOVED (missing key), RENAMED (both at once),
  // and a CHANGED wire string (same keys, different value). Each of those is a
  // wire-format change and each has to be made deliberately on both sides.
  describe('the ErrorCode constants', () => {
    const exported = Object.fromEntries(
      Object.entries(wire).filter(([name]) => name.startsWith('ErrCode')),
    )

    it('pins every ErrCode* export the module has, by name and wire string', () => {
      const pinned: Record<string, ErrorCode> = {
        ErrCodeUnknownWorkspace: 'unknown_workspace',
        ErrCodeNoWorkspaceRegistry: 'no_workspace_registry',
        ErrCodeUnknownProvider: 'unknown_provider',
        ErrCodeSessionOwned: 'session_owned_by_other',
        ErrCodeSpawnFailed: 'spawn_failed',
        ErrCodeConnectionClosed: 'connection_closed',
        ErrCodeSessionUnconfirmed: 'session_unconfirmed',
        ErrCodeSessionHeldByBackgroundAgent: 'session_held_by_background_agent',
        ErrCodeAgent: 'agent_error',
        ErrCodePromptUndelivered: 'prompt_undelivered',
      }
      // The pin being NON-EMPTY is also what stops the enumeration step from
      // reintroducing WIRE-8: if the filter above ever matched nothing (renamed
      // prefix, codegen stops emitting runtime consts, a namespace object this
      // cannot walk), `exported` is {} and `toEqual` against 10 entries is red.
      // A vacuously-green empty-vs-empty comparison needs BOTH sides derived,
      // and the right-hand side here cannot become derived without someone
      // deleting this literal.
      expect(exported).toEqual(pinned)
    })
  })
})
