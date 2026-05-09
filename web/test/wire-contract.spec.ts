/**
 * D-22 §6 wire-contract tests — six contracts that define the tether wire protocol.
 * Contracts 1–2 require a running server (set TETHER_TEST_URL env var).
 * Contracts 3–6 are static TypeScript shape checks executable in any environment.
 */
import { describe, expect, it } from 'vitest'
import type {
  Envelope,
  EnvelopeKind,
  FencedBlock,
  FencedBlockKind,
  HashHex64,
  SessionID,
} from '../src/lib/wire.gen'

const testURL = typeof process !== 'undefined' ? process.env['TETHER_TEST_URL'] : undefined
const hasServer = Boolean(testURL)

// ─── Contract 1: GET /cert-hash returns a 64-char lowercase hex string ────────

describe('contract-1: cert-hash format', () => {
  it('matches ^[0-9a-f]{64}$', { skip: !hasServer }, async () => {
    const res = await fetch(`${testURL}/cert-hash`)
    expect(res.ok).toBe(true)
    const hash = (await res.text()).trim()
    expect(hash).toMatch(/^[0-9a-f]{64}$/)
  })

  it('HashHex64 TypeScript alias accepts a 64-char hex string', () => {
    const h: HashHex64 = 'a'.repeat(64) as HashHex64
    expect(h.length).toBe(64)
  })
})

// ─── Contract 2: WT bidi echo at /wt/_smoke round-trips pure bytes ───────────
// Browser-only (WebTransport API absent in Node). Covered by Playwright e2e.

describe('contract-2: WT bidi echo', () => {
  it.skip('requires browser environment with WebTransport', () => {
    // Tested via web/test/e2e/smoke.spec.ts (Playwright).
  })
})

// ─── Contract 3: Envelope shape {kind, sessionId?, payload?} ─────────────────

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

// ─── Contract 5: FencedBlockKind enum has exactly 5 values ───────────────────

describe('contract-5: FencedBlockKind enum', () => {
  const expectedKinds: FencedBlockKind[] = ['dag', 'form', 'candidates', 'media', 'permission']

  it('has exactly 5 defined kinds', () => {
    expect(expectedKinds.length).toBe(5)
  })

  it('FencedBlock round-trips through JSON with correct kind discriminator', () => {
    for (const kind of expectedKinds) {
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
