// Invariant IDs are <workspace>/docs/tether-ui-invariants.md §3.9; the
// governing rule is §2 R10.
//
// These assertions land on RENDERED TEXT rather than on the return value of
// historyStart(), because owner ruling ④ is about what the reader is told. The
// pure function having the right variant and the screen saying the wrong sentence
// is the exact gap that document's §2 R9 records as "the acceptance must walk to
// the eyes".

import { afterEach, describe, expect, it, vi } from 'vitest'
import { cleanup, fireEvent, render, screen } from '@testing-library/react'
import { HistoryStartMarker } from './HistoryStartMarker'

afterEach(cleanup)

describe('HistoryStartMarker', () => {
  it('SCROLL-1: renders nothing at all in the none state', () => {
    const { container } = render(<HistoryStartMarker state={{ kind: 'none' }} />)
    expect(container.innerHTML).toBe('')
  })

  it('SCROLL-1: offers a way to load an earlier page, and claims nothing else', () => {
    const onLoadEarlier = vi.fn()
    render(<HistoryStartMarker state={{ kind: 'more' }} onLoadEarlier={onLoadEarlier} />)
    const button = screen.getByRole('button', { name: /load earlier/i })
    expect(document.body.textContent ?? '').not.toMatch(/no more history|beginning|cannot tell/i)
    // The control ACTS. Its existence is not the assertion — that is what the
    // previous version of this file pinned, and it pinned it with no handler
    // passed at all.
    fireEvent.click(button)
    expect(onLoadEarlier).toHaveBeenCalledTimes(1)
  })

  // 🔴 R10, and the same standard Shell.tsx's `Resizer` already holds itself to
  // (`if (!columns) return null`, mutation-proven). Before this case, the 'more'
  // state rendered "Load earlier messages" with `onClick={undefined}` and the test
  // above PINNED that the button existed with no handler — a control on screen
  // that cannot act, in the one component whose entire subject is not making
  // claims the code cannot support.
  //
  // Check: drop the `state.kind === 'more' && onLoadEarlier === undefined` guard
  // from HistoryStartMarker.tsx and this case fails.
  it('R10: renders no control at all when it has no way to load a page', () => {
    const { container } = render(<HistoryStartMarker state={{ kind: 'more' }} />)
    expect(screen.queryByRole('button')).toBeNull()
    // Nothing at all, not an empty container: an empty `.sh-history-start` still
    // occupies a grid cell and still says `data-history-start="more"` to anyone
    // reading the DOM.
    expect(container.innerHTML).toBe('')
  })

  it('SCROLL-1: says plainly that there is no more history at the beginning', () => {
    render(<HistoryStartMarker state={{ kind: 'start' }} />)
    const text = document.body.textContent ?? ''
    expect(text).toMatch(/no more history/i)
    expect(text).toMatch(/beginning/i)
  })

  it('SCROLL-1: names the other store, so "no more here" is not read as "no more anywhere"', () => {
    render(<HistoryStartMarker state={{ kind: 'startOf', otherStore: 'cc' }} />)
    const text = document.body.textContent ?? ''
    expect(text).toMatch(/no more history in this record/i)
    expect(text).toMatch(/claude code session log/i)
  })

  it('SCROLL-1: falls back to the raw store name rather than dropping it', () => {
    render(<HistoryStartMarker state={{ kind: 'startOf', otherStore: 'archive' }} />)
    expect(document.body.textContent).toContain('archive')
  })

  // R10, at the surface. The two failure directions are asserted separately
  // because they are different mistakes: claiming an end we have not established,
  // and claiming a continuation we have not established.
  it('R10: the unknown state says it cannot tell, and does NOT say there is no more history', () => {
    render(<HistoryStartMarker state={{ kind: 'unknown' }} />)
    const text = document.body.textContent ?? ''
    expect(text).toMatch(/cannot tell/i)
    expect(text).not.toMatch(/no more history/i)
    expect(text).not.toMatch(/beginning/i)
  })

  it('R10: the unknown state offers no load control — there is nothing known to load', () => {
    render(<HistoryStartMarker state={{ kind: 'unknown' }} />)
    expect(screen.queryByRole('button')).toBeNull()
  })

  // Ruling ④'s actual complaint, stated as an assertion: the tether-source case
  // must not render as an in-progress load. No spinner, no disabled control, no
  // "loading" wording — a UI element that implies arrival is the thing the ruling
  // says is dishonest, because on that store nothing can arrive.
  it('ruling ④: the end-of-history states offer no control that implies a page is coming', () => {
    for (const state of [{ kind: 'start' } as const, { kind: 'startOf', otherStore: 'cc' } as const]) {
      cleanup()
      render(<HistoryStartMarker state={state} />)
      expect(screen.queryByRole('button')).toBeNull()
      expect(document.body.textContent ?? '').not.toMatch(/loading|loading…/i)
    }
  })
})
