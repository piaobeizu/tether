import { describe, it, expect, beforeEach } from 'vitest'
import { loadMainView, loadRightTab } from '../src/App'

// tether#45 — the right-hand tab is persisted so a hard-refresh returns to the
// last tab instead of always dropping onto one. loadRightTab is the pure restore
// helper; the guard against a garbage/absent value is what keeps a corrupted
// localStorage from breaking the initial render.
//
// tether#90 — the right pane went back to three tabs (Chat / Skills / Shell) and
// Work moved to the middle column, so 'work' is now exactly as unrecognized as
// any other stale string, and the fallback moved to Chat. That guard IS the
// migration for browsers that stored 'work' before the change: nothing rewrites
// the key, it simply stops matching. Both halves are pinned below, because a
// fallback that was itself not a member would render a right pane with no tab
// selected — the failure mode this helper exists to prevent.
describe('loadRightTab (tether#45 persistence, tether#90 3-tab restore)', () => {
  beforeEach(() => localStorage.clear())

  it('defaults to chat when nothing is saved', () => {
    expect(loadRightTab()).toBe('chat')
  })

  it('restores a valid saved tab', () => {
    localStorage.setItem('tether_right_tab', 'chat')
    expect(loadRightTab()).toBe('chat')
    localStorage.setItem('tether_right_tab', 'shell')
    expect(loadRightTab()).toBe('shell')
    localStorage.setItem('tether_right_tab', 'skill')
    expect(loadRightTab()).toBe('skill')
  })

  it('falls back to chat for an unrecognized value', () => {
    localStorage.setItem('tether_right_tab', 'bogus')
    expect(loadRightTab()).toBe('chat')
  })

  it('migrates a persisted legacy "work" to chat (tether#90)', () => {
    localStorage.setItem('tether_right_tab', 'work')
    expect(loadRightTab()).toBe('chat')
  })

  it('whatever it falls back to is itself a real tab', () => {
    localStorage.setItem('tether_right_tab', 'work')
    expect(['chat', 'skill', 'shell']).toContain(loadRightTab())
  })
})

// tether#90 — the activity bar's choice is persisted the same way, with the same
// guard shape, so an unknown stored view cannot leave the middle column empty.
describe('loadMainView (tether#90 activity bar persistence)', () => {
  beforeEach(() => localStorage.clear())

  it('defaults to canvas when nothing is saved', () => {
    expect(loadMainView()).toBe('canvas')
  })

  it('restores a valid saved view', () => {
    localStorage.setItem('tether_main_view', 'work')
    expect(loadMainView()).toBe('work')
    localStorage.setItem('tether_main_view', 'canvas')
    expect(loadMainView()).toBe('canvas')
  })

  it('falls back to canvas for an unrecognized value', () => {
    localStorage.setItem('tether_main_view', 'bogus')
    expect(loadMainView()).toBe('canvas')
    // and specifically not for a right-pane tab name that leaked into this key
    localStorage.setItem('tether_main_view', 'chat')
    expect(loadMainView()).toBe('canvas')
  })
})
