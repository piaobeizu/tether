import { describe, expect, it } from 'vitest'
import { buildStepEdges } from './WorkDetail'

describe('buildStepEdges', () => {
  it('uses prev-derived edges when present, ignoring the chain fallback', () => {
    const edges = buildStepEdges([{ id: 'a' }, { id: 'b', prev: ['a'] }])
    expect(edges).toEqual([{ from: 'a', to: 'b', kind: 'step' }])
  })

  it('synthesizes a record-order chain when no node has prev (degraded)', () => {
    const edges = buildStepEdges([{ id: 'a' }, { id: 'b' }, { id: 'c' }])
    expect(edges).toEqual([
      { from: 'a', to: 'b', kind: 'step' },
      { from: 'b', to: 'c', kind: 'step' },
    ])
  })

  it('returns no edges for fewer than two prev-less nodes', () => {
    expect(buildStepEdges([])).toEqual([])
    expect(buildStepEdges([{ id: 'only' }])).toEqual([])
  })

  it('does not synthesize a chain once any prev edge exists', () => {
    // 'b' has prev; 'c' does not. prevEdges is non-empty → short-circuit, so
    // 'c' is left unconnected rather than chained from 'b' (a real, if partial,
    // DAG is never overwritten by the degraded fallback).
    const edges = buildStepEdges([{ id: 'a' }, { id: 'b', prev: ['a'] }, { id: 'c' }])
    expect(edges).toEqual([{ from: 'a', to: 'b', kind: 'step' }])
  })
})
