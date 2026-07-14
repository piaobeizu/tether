import { describe, expect, it } from 'vitest'
import { relTime } from './timefmt'

const NOW = Date.parse('2026-07-14T12:00:00Z')

describe('relTime (tether#31)', () => {
  it('returns "just now" for sub-minute and future/zero diffs', () => {
    expect(relTime('2026-07-14T11:59:30Z', NOW)).toBe('just now')
    expect(relTime('2026-07-14T12:00:00Z', NOW)).toBe('just now')
    expect(relTime('2026-07-14T12:05:00Z', NOW)).toBe('just now') // future ts → clamp
  })
  it('formats minutes, hours, and days', () => {
    expect(relTime('2026-07-14T11:45:00Z', NOW)).toBe('15m ago')
    expect(relTime('2026-07-14T09:00:00Z', NOW)).toBe('3h ago')
    expect(relTime('2026-07-12T12:00:00Z', NOW)).toBe('2d ago')
  })
  it('falls back to a local date beyond a week', () => {
    const s = relTime('2026-06-01T12:00:00Z', NOW)
    expect(s).toBeTruthy()
    expect(s.endsWith('ago')).toBe(false) // a date, not a relative label
  })
  it('returns "" for missing or unparseable input', () => {
    expect(relTime('', NOW)).toBe('')
    expect(relTime('not-a-date', NOW)).toBe('')
  })
})
