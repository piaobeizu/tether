import { describe, it, expect } from 'vitest'
import { applyLatencySample } from '../src/lib/latency'

describe('applyLatencySample', () => {
  it('seeds with the sample when prevMs <= 0', () => {
    expect(applyLatencySample(0, 42)).toBe(42)
    expect(applyLatencySample(-1, 42)).toBe(42)
  })

  it('applies EWMA with alpha=0.3 towards the new sample', () => {
    // next = prev + alpha * (sample - prev) = 100 + 0.3 * (200 - 100) = 130
    expect(applyLatencySample(100, 200)).toBeCloseTo(130, 5)
  })

  it('moves down towards a lower sample', () => {
    // next = 100 + 0.3 * (50 - 100) = 85
    expect(applyLatencySample(100, 50)).toBeCloseTo(85, 5)
  })

  it('is a no-op when sample equals prev', () => {
    expect(applyLatencySample(75, 75)).toBeCloseTo(75, 5)
  })

  it('converges towards a steady stream of identical samples', () => {
    let v = applyLatencySample(0, 100)
    for (let i = 0; i < 50; i++) v = applyLatencySample(v, 100)
    expect(v).toBeCloseTo(100, 3)
  })
})
