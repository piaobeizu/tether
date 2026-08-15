// tether#91 — the ONE session row, shared by the chat list and the wi detail.
//
// It exists because the first draft of that slice wrote the row twice and the two
// copies had already diverged before review: one marked the current session and
// one did not, one bypassed sessionLabel, one forgot to switch to the chat tab.
// Each test below pins one of those three, so a future second copy fails rather
// than merely looking slightly different.
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { cleanup, fireEvent, render, screen } from '@testing-library/react'
import { SessionRow, sessionWhen } from './SessionRow'
import { useStore } from './store'
import type { SessionSummary } from './wiSession'

const HOUR_AGO = Date.now() - 3_600_000

const row = (over: Partial<SessionSummary> = {}): SessionSummary =>
  ({ sid: 'sid-abcdefghijklmnop', title: 'a prompt', updatedAt: HOUR_AGO, ...over })

let listeners: (() => void)[] = []
function watch(event: string): () => unknown[] {
  const seen: unknown[] = []
  const on = (e: Event) => seen.push((e as CustomEvent).detail ?? true)
  window.addEventListener(event, on)
  listeners.push(() => window.removeEventListener(event, on))
  return () => seen
}

beforeEach(() => {
  localStorage.clear()
  useStore.setState({ sessionId: 'sid-elsewhere', messages: [], notices: [] })
  vi.stubGlobal('fetch', vi.fn(async () => ({ ok: true, status: 200, json: async () => [] })))
})

afterEach(() => {
  cleanup()
  for (const off of listeners) off()
  listeners = []
  vi.unstubAllGlobals()
  vi.restoreAllMocks()
})

describe('SessionRow', () => {
  it('opens the session through the shared operation AND lands the user on chat', () => {
    const tabs = watch('tether:select-tab')
    const reconnects = watch('tether:retry-connection')
    render(<SessionRow session={row({ sid: 'sid-target-1234' })} />)

    fireEvent.click(screen.getByText('a prompt'))

    // The tab switch is not decoration: this row is also rendered from the MIDDLE
    // column (the wi detail) while the right column may be showing Skills or
    // Shell. Without it the click tears down the WebTransport and repoints the
    // next prompt with nothing visible happening.
    expect(tabs()).toEqual(['chat'])
    // Through openSession — so the channel is rebound and the sid persisted.
    expect(reconnects()).toHaveLength(1)
    expect(useStore.getState().sessionId).toBe('sid-target-1234')
    expect(localStorage.getItem('tether_last_sid')).toBe('sid-target-1234')
  })

  it('marks the session you are already in', () => {
    useStore.setState({ sessionId: 'sid-current-9999' })
    const { container } = render(<SessionRow session={row({ sid: 'sid-current-9999' })} />)

    // Clicking it is a deliberate no-op (openSession returns early). The user
    // should be able to see that before clicking, which is the half the
    // hand-written copy in the wi detail had dropped.
    expect(container.querySelector('.tree-row.active')).toBeTruthy()
    expect(container.querySelector('.ws-dot.live')).toBeTruthy()
  })

  it('leaves a different session unmarked', () => {
    const { container } = render(<SessionRow session={row({ sid: 'sid-other-1234' })} />)
    expect(container.querySelector('.tree-row.active')).toBeNull()
    expect(container.querySelector('.ws-dot.live')).toBeNull()
  })

  it('labels through sessionLabel, and honours omitWorkItem', () => {
    const s = row({ workItem: 'tether#91', title: 'the first prompt' })
    const { rerender } = render(<SessionRow session={s} />)
    expect(screen.getByText('tether#91')).toBeTruthy()

    rerender(<SessionRow session={s} omitWorkItem />)
    expect(screen.getByText('the first prompt')).toBeTruthy()
    expect(screen.queryByText('tether#91')).toBeNull()
  })
})

describe('sessionWhen', () => {
  it('formats a real timestamp', () => {
    expect(sessionWhen(Date.now() - 3 * 3_600_000)).toBe('3h ago')
  })

  // relTime promises it never throws, but `new Date(x).toISOString()` throws
  // RangeError on a non-finite number BEFORE relTime is reached — so calling it
  // naively moves the failure to a place that promise does not cover.
  it('is empty rather than throwing for a non-finite value', () => {
    expect(sessionWhen(Number.NaN)).toBe('')
    expect(sessionWhen(Number.POSITIVE_INFINITY)).toBe('')
  })

  // The daemon leaves updatedAt at 0 when it could not stat the transcript.
  // Formatted naively that is "Jan 1, 1970", which reads as information.
  it('is empty for the daemon zero, not a 1970 date', () => {
    expect(sessionWhen(0)).toBe('')
  })
})
