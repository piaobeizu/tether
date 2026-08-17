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
import { EXTERNAL_SESSION_PROMISE, isExternalSession, type SessionSummary } from './wiSession'

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

// tether#92 — a session the coding agent recorded and tether never saw.
//
// The list is now allowed to offer conversations tether cannot promise to
// continue, so the row has to say so. These pin the promise itself, not just
// that some markup appeared: the failure this guards against is the one this
// repo keeps producing — the backend does something reasonable and the user is
// never told.
describe('SessionRow marks a session tether did not record', () => {
  const ccRow = (over: Partial<SessionSummary> = {}) =>
    row({ sid: 'sid-cc-0000000001', title: 'typed in a terminal', source: 'cc', ...over })

  it('marks it external on the row, before the click', () => {
    const { container } = render(<SessionRow session={ccRow()} />)
    const marker = container.querySelector('.session-row-src')
    expect(marker?.textContent).toBe('external')
    // NOT "read-only": tether already uses that phrase for a /wt/events observer
    // attach, where it means "you may watch but not send". Here the composer is
    // enabled and a prompt IS delivered.
    expect(marker?.textContent).not.toBe('read-only')
  })

  it('says nothing of the sort on a session tether recorded', () => {
    const { container } = render(<SessionRow session={row({ source: 'tether' })} />)
    expect(container.querySelector('.session-row-src')).toBeNull()
    // An absent source is a tether row: four test files build rows by hand and
    // none of them is about provenance.
    const bare = render(<SessionRow session={row()} />)
    expect(bare.container.querySelector('.session-row-src')).toBeNull()
  })

  it('treats an UNRECOGNISED source as external, not as tether’s own', () => {
    // If the daemon ever grows a third store, a row from it must not render as a
    // fully trusted tether row. Failing safe means erring towards the warning.
    const odd = { ...row(), source: 'someday' } as unknown as SessionSummary
    expect(isExternalSession(odd)).toBe(true)
    const { container } = render(<SessionRow session={odd} />)
    expect(container.querySelector('.session-row-src')?.textContent).toBe('external')
  })

  it('names where it came from on hover, as a bonus rather than as the channel', () => {
    const { container } = render(<SessionRow session={ccRow()} />)
    const title = container.querySelector('.tree-row')?.getAttribute('title') ?? ''
    expect(title).toContain('sid-cc-0000000001')
    expect(title).toContain('typed in a terminal')
    expect(title).toContain('coding agent')
  })

  it('posts NO notice — the promise is state, not an event', () => {
    // Deliberate and load-bearing. A click-scoped notice vanished on reload while
    // tether_last_sid survived, so the user came back to an external conversation
    // with no warning at all. The promise now hangs off the session's own source
    // (see SessionList), which a reload re-derives.
    render(<SessionRow session={ccRow()} />)
    fireEvent.click(screen.getByText('typed in a terminal'))
    expect(useStore.getState().notices).toHaveLength(0)
  })

  it('states every limit it needs to, in one sentence', () => {
    // "may" is the load-bearing word: a failed --resume is only observable after
    // the first prompt has been delivered, so neither "will" nor "will not" is a
    // claim tether can make. Matched case-insensitively on the CONCEPT so a
    // rewording cannot pass by dodging one literal.
    expect(EXTERNAL_SESSION_PROMISE).toMatch(/coding agent/i)
    expect(EXTERNAL_SESSION_PROMISE).toMatch(/recent messages only/i)
    expect(EXTERNAL_SESSION_PROMISE).toMatch(/\bmay\b[^.]*new conversation/i)
    expect(EXTERNAL_SESSION_PROMISE).not.toMatch(/will (not )?(be )?continu/i)
  })

  // tether#97 — this banner told the user the transcript came "without tool
  // activity", and tether#96 put tool cards on the screen directly underneath it.
  //
  // These assert the SHAPE of the claim rather than its words, because a test
  // that swaps one exact sentence for another goes stale the same way the first
  // one did: it was pinning /without tool activity/, so it stayed green while the
  // string it protected turned into a lie about a change in another package.
  it('says the tool activity the daemon now serves is THERE', () => {
    // Asserted in the POSITIVE form deliberately. Enumerating the ways a sentence
    // can deny the calls is a losing game — "without tool activity", "without the
    // calls it made" and "the calls it made are omitted" are three different lies
    // and a blocklist catches whichever one it happened to be written against.
    // Requiring INCLUSION catches all three at once: the noun has to arrive
    // attached to a word that puts it on the screen. (`\bwith\b` does not match
    // "without" — the boundary fails on the following letter.) Either noun and
    // three inclusion words pass, so this pins the CLAIM, not the voice.
    expect(EXTERNAL_SESSION_PROMISE).toMatch(/\b(with|including|plus)\b[^.;]{0,24}\b(calls?|tools?)\b/i)
    // Belt and braces for the exact sentence this wi deleted, which is the one a
    // revert or a bad merge would put back.
    expect(EXTERNAL_SESSION_PROMISE).not.toMatch(/without tool|no tool activity/i)
  })

  it('does not over-correct into promising output the daemon drops', () => {
    // The opposite failure, and the likelier one for whoever rewords this next: a
    // SUCCESSFUL call carries no result at all (ccMessage.errorResults serves only
    // is_error), and a call's arguments are a bounded string-only projection. So
    // "the full output", "everything it did", "the complete record" are fresh lies.
    //
    // Word-bounded and negation-aware, because both matter: without \b this
    // rejects "incomplete", and without the lookbehind it rejects "not the full
    // output" — and those two are honest wordings, so failing them would make this
    // guard an obstacle to fixing the very thing it is guarding.
    expect(EXTERNAL_SESSION_PROMISE)
      .not.toMatch(/(?<!\b(?:not|never|no)\s(?:the\s)?)\b(full|complete|everything|all of)\b/i)
  })

  it('states the output limit together with the exception that makes it true', () => {
    // The two halves are only true as a pair, so they are matched as a pair — one
    // clause, no sentence boundary between them. "no tool output" alone is false
    // (a failure's message IS served, capped at 2 KiB); "shows what it did" alone
    // over-promises the successes it drops. A co-occurrence match passes any
    // rewording that keeps both facts and fails one that keeps only one.
    expect(EXTERNAL_SESSION_PROMISE).toMatch(/output[^.]*fail|fail[^.]*output/i)
  })

  it('still opens the session through the one shared operation', () => {
    // A read-only session is still a session being opened: tether#61's rule does
    // not get an exception here, so the WT channel is rebound and the sid
    // persisted exactly as for any other row.
    const reconnects = watch('tether:retry-connection')
    const tabs = watch('tether:select-tab')
    render(<SessionRow session={ccRow()} />)

    fireEvent.click(screen.getByText('typed in a terminal'))

    expect(tabs()).toEqual(['chat'])
    expect(reconnects()).toHaveLength(1)
    expect(useStore.getState().sessionId).toBe('sid-cc-0000000001')
    expect(localStorage.getItem('tether_last_sid')).toBe('sid-cc-0000000001')
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
