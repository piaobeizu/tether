import { afterEach, describe, expect, it, vi } from 'vitest'
import { cleanup, render, fireEvent, screen } from '@testing-library/react'
import EventTimeline, { describeEvent, cleanUrl } from './EventTimeline'
import { AihubError, fetchEvents } from '../../lib/aihub'
import type { WorkEvent, WorkEvents } from '../../lib/wire.gen'

// EventTimeline fetches its own page(s); mock the fetcher so tests never touch
// the network (same pattern as DetailDrawer.test).
vi.mock('../../lib/aihub', async () => {
  const actual = await vi.importActual<typeof import('../../lib/aihub')>('../../lib/aihub')
  return { ...actual, fetchEvents: vi.fn() }
})
const mockEvents = vi.mocked(fetchEvents)

afterEach(() => {
  cleanup()
  vi.clearAllMocks()
})

const ev = (type: string, payload?: unknown, ts = '2026-07-14T12:00:00Z'): WorkEvent => ({ ts, type, payload })
const page = (events: WorkEvent[], nextCursor?: string): WorkEvents => ({ events, nextCursor })

describe('describeEvent (tether#31)', () => {
  it('formats known event types', () => {
    expect(describeEvent(ev('step_started', { step: 'spec' })).text).toBe('spec')
    const done = describeEvent(ev('step_completed', { step: 'plan', artifact_summary: 'did the plan' }))
    expect(done.icon).toBe('✓')
    expect(done.text).toBe('plan')
    expect(done.sub).toBe('did the plan')
    const c = describeEvent(ev('commit', { sha: 'abcdef1234567', message: 'feat: x\nbody' }))
    expect(c.text).toBe('commit abcdef1')
    expect(c.sub).toBe('feat: x')
    expect(describeEvent(ev('push', { branch: 'polyforge/x' })).text).toBe('push polyforge/x')
    expect(describeEvent(ev('note', { text: 'hello\nworld' })).text).toBe('hello')
    expect(describeEvent(ev('attempt_completed', { status: 'wrapped' })).text).toBe('wrapped')
  })
  it('falls back to a generic row for unknown types and a null payload', () => {
    const r = describeEvent(ev('mystery_event', null))
    expect(r.text).toBe('mystery_event')
    expect(r.icon).toBe('·')
  })
  it('never throws on a non-object payload (string / number / array)', () => {
    expect(() => describeEvent(ev('note', 'a raw string'))).not.toThrow()
    expect(() => describeEvent(ev('commit', [1, 2, 3]))).not.toThrow()
    expect(() => describeEvent(ev('pr_opened', 42))).not.toThrow()
    expect(describeEvent(ev('note', 'x')).text).toBe('note') // no .text field → safe default
    expect(describeEvent(ev('work_item_filed', { goal: 'do the thing' })).sub).toBe('do the thing')
  })
  it('cleans a pr_opened url that carries a Warning prefix', () => {
    const r = describeEvent(
      ev('pr_opened', { title: 'my PR', url: 'Warning: 446 uncommitted changes\nhttps://github.com/x/y/pull/1' }),
    )
    expect(r.href).toBe('https://github.com/x/y/pull/1')
    expect(r.text).toBe('my PR')
  })
})

describe('cleanUrl (tether#31)', () => {
  it('takes from the last https:// occurrence and trims', () => {
    expect(cleanUrl('Warning: junk\nhttps://a/b')).toBe('https://a/b')
    expect(cleanUrl('https://a/b')).toBe('https://a/b')
    expect(cleanUrl('  https://a/b  ')).toBe('https://a/b')
  })
  it('requires https, rejects non-urls, and cuts trailing junk', () => {
    expect(cleanUrl('totally not a url')).toBe('')
    expect(cleanUrl('')).toBe('')
    expect(cleanUrl('http://insecure/x')).toBe('') // only https is linkable
    expect(cleanUrl('junk https://a/b/pull/1>')).toBe('https://a/b/pull/1') // trailing > cut
  })
})

describe('EventTimeline (tether#31)', () => {
  it('renders the first page newest-first with summaries', async () => {
    mockEvents.mockResolvedValueOnce(
      page([
        ev('attempt_completed', { status: 'wrapped' }),
        ev('step_completed', { step: 'spec', artifact_summary: 'spec done' }),
      ]),
    )
    const { container } = render(<EventTimeline id="wi_x" />)
    expect(await screen.findByText('wrapped')).toBeTruthy()
    expect(screen.getByText('spec')).toBeTruthy()
    expect(screen.getByText('spec done')).toBeTruthy()
    expect(container.querySelectorAll('.work-activity-row').length).toBe(2)
  })

  it('shows "load more" only with a nextCursor and appends the next page', async () => {
    mockEvents
      .mockResolvedValueOnce(page([ev('note', { text: 'first' })], 'cur2'))
      .mockResolvedValueOnce(page([ev('note', { text: 'second' })]))
    render(<EventTimeline id="wi_x" />)
    const more = await screen.findByText('load more')
    fireEvent.click(more)
    expect(await screen.findByText('second')).toBeTruthy()
    expect(screen.getByText('first')).toBeTruthy() // page 1 kept
    expect(screen.queryByText('load more')).toBeNull() // page 2 had no cursor
    expect(mockEvents).toHaveBeenNthCalledWith(2, 'wi_x', 'cur2')
  })

  it('renders the empty state when there are no events', async () => {
    mockEvents.mockResolvedValueOnce(page([]))
    render(<EventTimeline id="wi_x" />)
    expect(await screen.findByText('no activity yet')).toBeTruthy()
  })

  it('renders an error state on fetch failure', async () => {
    mockEvents.mockRejectedValueOnce(new AihubError(503))
    render(<EventTimeline id="wi_x" />)
    expect(await screen.findByText('aihub not configured')).toBeTruthy()
  })
})
