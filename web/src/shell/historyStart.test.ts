// Invariant IDs are docs/tether-ui-invariants.md §3.9; the governing rule is §2 R10.

import { describe, expect, it } from 'vitest'
import {
  TRANSCRIPT_EARLIER_HEADER,
  TRANSCRIPT_OTHER_RECORD_HEADER,
} from '../lib/transcriptWatch'
import { historyStart } from './historyStart'

// Header names come from lib/transcriptWatch, which mirrors the Go constants and
// is pinned to them by a Go test. Writing the literals here instead would make
// this suite agree with a copy of the daemon's vocabulary rather than with the
// daemon's vocabulary — a rename on the Go side would then leave both green.
const withEarlier = (cursor: string) => new Headers({ [TRANSCRIPT_EARLIER_HEADER]: cursor })
const withOther = (store: string) => new Headers({ [TRANSCRIPT_OTHER_RECORD_HEADER]: store })
const nonEmpty = { entryCount: 3, localEntryCount: 0 }

describe('historyStart', () => {
  it('SCROLL-1: says an earlier page can be loaded when the daemon sent a cursor', () => {
    expect(historyStart(withEarlier('4096'), nonEmpty)).toEqual({ kind: 'more' })
  })

  // POLL-14's distinction on the response side: the daemon expresses "there is
  // none" by OMITTING the header, so a cursor of '0' is a real cursor. Reading it
  // as absent would strand a reader one page from the top of a cc-source session.
  it('SCROLL-1: a zero cursor is a cursor, not an absence', () => {
    expect(historyStart(withEarlier('0'), nonEmpty)).toEqual({ kind: 'more' })
  })

  it('SCROLL-1: says this is the beginning when the daemon omitted both headers', () => {
    expect(historyStart(new Headers(), nonEmpty)).toEqual({ kind: 'start' })
  })

  // SCROLL-1's third state, and the reason ruling ④ needs more than a boolean:
  // tether's own store has no pagination at all, so "no more here" on a session
  // the cc store also holds would read as "these are all the messages" when it is
  // not. Naming the other store is the difference between an end and a boundary.
  it('SCROLL-1: names the other store when one also holds records for this session', () => {
    expect(historyStart(withOther('cc'), nonEmpty)).toEqual({ kind: 'startOf', otherStore: 'cc' })
  })

  it('SCROLL-1: a cursor wins over the other-store header — there is still a page to fetch', () => {
    const h = withEarlier('12')
    h.set(TRANSCRIPT_OTHER_RECORD_HEADER, 'cc')
    expect(historyStart(h, nonEmpty)).toEqual({ kind: 'more' })
  })

  it('SCROLL-1: an empty other-store header is an absence, not a store named ""', () => {
    expect(historyStart(withOther(''), nonEmpty)).toEqual({ kind: 'start' })
  })

  it('SCROLL-1: an empty transcript renders no marker at all', () => {
    expect(historyStart(new Headers(), { entryCount: 0, localEntryCount: 0 })).toEqual({
      kind: 'none',
    })
    expect(historyStart(withEarlier('9'), { entryCount: 0, localEntryCount: 0 })).toEqual({
      kind: 'none',
    })
  })

  // SCROLL-2. Same rule as the empty case: the marker describes server-side
  // history, and locally-generated entries are not that. A "this is the beginning"
  // label above nothing but a session notice describes a list the reader cannot
  // see.
  it('SCROLL-2: a transcript holding only local notices renders no marker', () => {
    expect(historyStart(new Headers(), { entryCount: 2, localEntryCount: 2 })).toEqual({
      kind: 'none',
    })
  })

  it('SCROLL-2: one server entry among local notices is enough to render the marker', () => {
    expect(historyStart(new Headers(), { entryCount: 3, localEntryCount: 2 })).toEqual({
      kind: 'start',
    })
  })

  // R10, and the whole reason 'unknown' is a variant rather than `hasMore?:
  // boolean`. A probe that has not answered is not the same fact as a daemon that
  // said there is nothing earlier, and collapsing the two is precisely the
  // substitution ruling ④ exists to stop: a UI that has not asked yet must not
  // announce the end of the conversation.
  it('R10: no response yet is "cannot tell", never "this is the beginning"', () => {
    const state = historyStart(null, nonEmpty)
    expect(state).toEqual({ kind: 'unknown' })
    expect(state.kind).not.toBe('start')
  })

  it('R10: a failed probe is "cannot tell" even on a transcript with plenty in it', () => {
    expect(historyStart(null, { entryCount: 500, localEntryCount: 1 })).toEqual({ kind: 'unknown' })
  })
})
