import { describe, it, expect, beforeEach } from 'vitest'
import { loadRightTab } from '../src/App'

// tether#45 — the right-hand tab (Work/Chat/Skills/Shell) is now persisted so a
// hard-refresh returns to the last tab instead of always dropping onto Work.
// loadRightTab is the pure restore helper; the guard against a garbage/absent
// value is what keeps a corrupted localStorage from breaking the initial render.
describe('loadRightTab (tether#45 tab persistence)', () => {
  beforeEach(() => localStorage.clear())

  it('defaults to work when nothing is saved', () => {
    expect(loadRightTab()).toBe('work')
  })

  it('restores a valid saved tab', () => {
    localStorage.setItem('tether_right_tab', 'chat')
    expect(loadRightTab()).toBe('chat')
    localStorage.setItem('tether_right_tab', 'shell')
    expect(loadRightTab()).toBe('shell')
  })

  it('falls back to work for an unrecognized value', () => {
    localStorage.setItem('tether_right_tab', 'bogus')
    expect(loadRightTab()).toBe('work')
  })
})
