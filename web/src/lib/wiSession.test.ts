// tether#91 — the wi↔session binding, after inverting it and moving it off the
// browser.
//
// The old mapping was two localStorage lines in WorkDetail.tsx. Each test below
// pins one of the four things that were wrong with it, or one of the hazards that
// moving it created.
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import {
  RUNNING_ELSEWHERE_BADGE,
  WI_BOUND_EVENT,
  bindWorkItem,
  fetchSessions,
  isExternalSession,
  isRunningElsewhere,
  migrateLegacyWiSessions,
  putWiBinding,
  resetArmedBinding,
  resetMigrationForTests,
  sessionLabel,
  sessionsForWorkItem,
  type SessionSummary,
} from './wiSession'
import { useStore } from './store'

/** Drain the promise chain. Every hop is a microtask; this is a macrotask. */
const settle = () => new Promise<void>(r => setTimeout(r, 0))

type Call = { url: string; init?: RequestInit }

/** Record every request. `ok` models the daemon's answer. */
function mockFetch(ok = true, status = 204) {
  const calls: Call[] = []
  const fn = vi.fn(async (url: string, init?: RequestInit) => {
    calls.push({ url, init })
    return { ok, status, json: async () => [] }
  })
  vi.stubGlobal('fetch', fn)
  return calls
}

const puts = (calls: Call[]) => calls.filter(c => c.init?.method === 'PUT')
const bodyOf = (c: Call) => JSON.parse(String(c.init?.body)) as { workItem: string }

let listeners: (() => void)[] = []
function watchBound(): () => { sid: string; workItem: string }[] {
  const seen: { sid: string; workItem: string }[] = []
  const on = (e: Event) => seen.push((e as CustomEvent).detail)
  window.addEventListener(WI_BOUND_EVENT, on)
  listeners.push(() => window.removeEventListener(WI_BOUND_EVENT, on))
  return () => seen
}

beforeEach(() => {
  localStorage.clear()
  resetArmedBinding()
  resetMigrationForTests()
  useStore.setState({ sessionId: null })
})

afterEach(() => {
  for (const off of listeners) off()
  listeners = []
  resetArmedBinding()
  vi.unstubAllGlobals()
  vi.restoreAllMocks()
})

describe('putWiBinding', () => {
  it('PUTs the label to the session-scoped route and announces it', async () => {
    const calls = mockFetch()
    const bound = watchBound()

    await expect(putWiBinding('sid-a', 'tether#91')).resolves.toBe('recorded')

    expect(calls).toHaveLength(1)
    expect(calls[0].url).toBe('/api/v1/sessions/sid-a/wi')
    expect(calls[0].init?.method).toBe('PUT')
    expect(bodyOf(calls[0])).toEqual({ workItem: 'tether#91' })
    // The event is what makes the list's label change without a reload.
    expect(bound()).toEqual([{ sid: 'sid-a', workItem: 'tether#91' }])
  })

  it('url-encodes the sid', async () => {
    const calls = mockFetch()
    // The daemon rejects sids outside [A-Za-z0-9_-], so this is defence in depth
    // — but a sid arriving from a legacy localStorage key is not something this
    // module verified.
    await putWiBinding('a/b?c', 'tether#91')
    expect(calls[0].url).toBe('/api/v1/sessions/a%2Fb%3Fc/wi')
  })

  // 'refused' and 'unreachable' are separate outcomes because they need opposite
  // handling: a 4xx will say the same thing on every retry, a 5xx or a dead
  // socket will not. Collapsing them is what made the migration re-send a
  // permanently-rejected key on every page load.
  it('reports a REFUSAL and announces nothing when the daemon says no', async () => {
    mockFetch(false, 400)
    const bound = watchBound()
    await expect(putWiBinding('sid-a', 'tether#91')).resolves.toBe('refused')
    expect(bound()).toHaveLength(0)
  })

  it('reports UNREACHABLE for a daemon-side failure, which is retryable', async () => {
    mockFetch(false, 503)
    await expect(putWiBinding('sid-a', 'tether#91')).resolves.toBe('unreachable')
  })

  it('reports UNREACHABLE when the request never lands', async () => {
    vi.stubGlobal('fetch', vi.fn(async () => { throw new Error('offline') }))
    await expect(putWiBinding('sid-a', 'tether#91')).resolves.toBe('unreachable')
  })

  it('sends nothing for an empty sid or an empty label', async () => {
    const calls = mockFetch()
    // 'refused', not 'unreachable': there is nothing here a retry would fix.
    await expect(putWiBinding('', 'tether#91')).resolves.toBe('refused')
    await expect(putWiBinding('sid-a', '')).resolves.toBe('refused')
    expect(calls).toHaveLength(0)
  })
})

describe('bindWorkItem', () => {
  it('binds the current session immediately when there is one', async () => {
    const calls = mockFetch()
    useStore.setState({ sessionId: 'sid-live' })

    bindWorkItem('tether#91')
    await settle()

    expect(puts(calls)).toHaveLength(1)
    expect(calls[0].url).toBe('/api/v1/sessions/sid-live/wi')
  })

  // Problem 4 of the old mapping, and the one that was invisible: it wrote
  // `sessionId ?? ''`, so clicking Start before a session existed recorded a
  // mapping to nothing — after which "Open in chat" fell through to re-injecting
  // the prompt forever, with no sign anything had gone wrong.
  it('records nothing when there is no session yet', async () => {
    const calls = mockFetch()

    bindWorkItem('tether#91')
    await settle()

    expect(calls).toHaveLength(0)
  })

  it('binds the first session that appears afterwards', async () => {
    const calls = mockFetch()

    bindWorkItem('tether#91')
    await settle()
    expect(calls).toHaveLength(0)

    useStore.setState({ sessionId: 'sid-new' })
    await settle()

    expect(puts(calls)).toHaveLength(1)
    expect(calls[0].url).toBe('/api/v1/sessions/sid-new/wi')
    expect(bodyOf(calls[0])).toEqual({ workItem: 'tether#91' })
  })

  it('fires once — a later session change does not re-bind', async () => {
    const calls = mockFetch()

    bindWorkItem('tether#91')
    useStore.setState({ sessionId: 'sid-new' })
    await settle()
    useStore.setState({ sessionId: 'sid-another' })
    await settle()

    expect(puts(calls)).toHaveLength(1)
    expect(calls[0].url).toBe('/api/v1/sessions/sid-new/wi')
  })

  it('the last Start wins while waiting', async () => {
    const calls = mockFetch()

    bindWorkItem('tether#90')
    bindWorkItem('tether#91')
    useStore.setState({ sessionId: 'sid-new' })
    await settle()

    expect(puts(calls)).toHaveLength(1)
    expect(bodyOf(calls[0])).toEqual({ workItem: 'tether#91' })
  })

  // A dropped binding has no symptom of its own — the next "Open in chat" just
  // falls back to injecting the resume prompt, which is also what an unstarted wi
  // does. Silence here would leave the invisibility that made the bug this slice
  // replaces so hard to notice, with only the cause fixed.
  it('says so in the transcript when the daemon refuses the binding', async () => {
    mockFetch(false, 400)
    useStore.setState({ sessionId: 'sid-live', notices: [], messages: [] })

    bindWorkItem('tether#91')
    await settle()

    const notices = useStore.getState().notices
    expect(notices).toHaveLength(1)
    expect(notices[0].text).toContain('tether#91')
    expect(notices[0].text).toContain('rejected')
  })

  it('says so in the transcript when the daemon could not be reached', async () => {
    vi.stubGlobal('fetch', vi.fn(async () => { throw new Error('offline') }))
    useStore.setState({ sessionId: 'sid-live', notices: [], messages: [] })

    bindWorkItem('tether#91')
    await settle()

    const notices = useStore.getState().notices
    expect(notices).toHaveLength(1)
    expect(notices[0].text).toContain('could not be reached')
  })

  it('stays quiet when the binding lands', async () => {
    mockFetch()
    useStore.setState({ sessionId: 'sid-live', notices: [], messages: [] })

    bindWorkItem('tether#91')
    await settle()

    expect(useStore.getState().notices).toHaveLength(0)
  })

  // The hazard the deferred bind knowingly accepts, pinned so it is a recorded
  // decision rather than a latent surprise: while waiting for a session to exist,
  // deliberately OPENING an existing one claims the binding. The window is one
  // click wide (between a Start with no session and the next sid to appear) and
  // closing it would mean a second opinion inside openSession, which is the one
  // function this codebase has already had to consolidate.
  it('KNOWN LIMIT: while armed, opening any other session claims the binding', async () => {
    const calls = mockFetch()

    bindWorkItem('tether#91')
    // What openSession does first: persist the sid.
    useStore.setState({ sessionId: 'sid-some-other-conversation' })
    await settle()

    expect(puts(calls)).toHaveLength(1)
    expect(calls[0].url).toBe('/api/v1/sessions/sid-some-other-conversation/wi')
  })

  it('ignores a null sessionId (App.startNewSession clears it before reloading)', async () => {
    const calls = mockFetch()

    bindWorkItem('tether#91')
    useStore.setState({ sessionId: null })
    await settle()

    expect(calls).toHaveLength(0)
  })
})

describe('migrateLegacyWiSessions', () => {
  // The case almost every browser is in. A migration that pings the daemon on
  // every load to discover it has nothing to do is a migration that never ends.
  it('issues NO requests when there are no legacy keys', async () => {
    const calls = mockFetch()
    localStorage.setItem('tether_last_sid', 'sid-a')  // an unrelated key
    localStorage.setItem('tether_ws_id', 'ws-1')

    await expect(migrateLegacyWiSessions()).resolves.toBe(0)

    expect(calls).toHaveLength(0)
    // And it left the unrelated keys alone.
    expect(localStorage.getItem('tether_last_sid')).toBe('sid-a')
    expect(localStorage.getItem('tether_ws_id')).toBe('ws-1')
  })

  it('moves a legacy key to the daemon and deletes it', async () => {
    const calls = mockFetch()
    localStorage.setItem('tether_wi_sid:tether#90', 'sid-old')

    await expect(migrateLegacyWiSessions()).resolves.toBe(1)

    expect(puts(calls)).toHaveLength(1)
    expect(calls[0].url).toBe('/api/v1/sessions/sid-old/wi')
    expect(bodyOf(calls[0])).toEqual({ workItem: 'tether#90' })
    expect(localStorage.getItem('tether_wi_sid:tether#90')).toBeNull()
  })

  it('moves several keys, one request each', async () => {
    const calls = mockFetch()
    localStorage.setItem('tether_wi_sid:tether#90', 'sid-a')
    localStorage.setItem('tether_wi_sid:aihub#7', 'sid-b')

    await expect(migrateLegacyWiSessions()).resolves.toBe(2)

    expect(puts(calls)).toHaveLength(2)
    expect(new Set(puts(calls).map(c => c.url)))
      .toEqual(new Set(['/api/v1/sessions/sid-a/wi', '/api/v1/sessions/sid-b/wi']))
    expect(localStorage.getItem('tether_wi_sid:tether#90')).toBeNull()
    expect(localStorage.getItem('tether_wi_sid:aihub#7')).toBeNull()
  })

  // The old writer produced these whenever Start was clicked with no session.
  // There is nothing to migrate, and sending it would just earn a 400.
  it('drops a legacy key holding the empty string without asking the daemon', async () => {
    const calls = mockFetch()
    localStorage.setItem('tether_wi_sid:tether#90', '')

    await expect(migrateLegacyWiSessions()).resolves.toBe(0)

    expect(calls).toHaveLength(0)
    expect(localStorage.getItem('tether_wi_sid:tether#90')).toBeNull()
  })

  it('keeps the key when the daemon could not record it, so the next load retries', async () => {
    mockFetch(false, 503)
    localStorage.setItem('tether_wi_sid:tether#90', 'sid-old')

    await expect(migrateLegacyWiSessions()).resolves.toBe(0)

    expect(localStorage.getItem('tether_wi_sid:tether#90')).toBe('sid-old')
  })

  // A 400 is a verdict, not a hiccup. Keeping the key would re-send the same
  // rejected request on every page load for the life of the profile.
  it('drops a key the daemon REFUSES, so it is not retried forever', async () => {
    mockFetch(false, 400)
    localStorage.setItem('tether_wi_sid:tether#90', 'sid-old')

    // 0 moved — nothing was recorded — but the key is gone all the same.
    await expect(migrateLegacyWiSessions()).resolves.toBe(0)

    expect(localStorage.getItem('tether_wi_sid:tether#90')).toBeNull()
  })

  it('runs at most once per page — StrictMode mounts twice', async () => {
    const calls = mockFetch()
    localStorage.setItem('tether_wi_sid:tether#90', 'sid-old')

    const [first, second] = await Promise.all([migrateLegacyWiSessions(), migrateLegacyWiSessions()])

    expect(first + second).toBe(1)
    expect(puts(calls)).toHaveLength(1)
  })
})

describe('list helpers', () => {
  it('fetchSessions returns the daemon order untouched', async () => {
    const rows: SessionSummary[] = [
      { sid: 'zzz', updatedAt: 3 },
      { sid: 'aaa', updatedAt: 2 },
      { sid: 'mmm', updatedAt: 1 },
    ]
    vi.stubGlobal('fetch', vi.fn(async () => ({ ok: true, status: 200, json: async () => rows })))

    // Not re-sorted and not reversed: the previous list applied
    // `[...sessions].reverse()` to a response that was in UUID-filename order,
    // which reads as "newest first" and is not.
    await expect(fetchSessions()).resolves.toEqual(rows)
  })

  it('fetchSessions rejects on a non-200 rather than pretending the list is empty', async () => {
    vi.stubGlobal('fetch', vi.fn(async () => ({ ok: false, status: 500, json: async () => [] })))
    await expect(fetchSessions()).rejects.toThrow('HTTP 500')
  })

  it('sessionLabel prefers the work item, then the title, then the sid', () => {
    expect(sessionLabel({ sid: 'sid-abcdefghijklmnop-tail', workItem: 'tether#91', title: 'hi', updatedAt: 1 }))
      .toBe('tether#91')
    expect(sessionLabel({ sid: 'sid-abcdefghijklmnop-tail', title: 'first prompt', updatedAt: 1 }))
      .toBe('first prompt')
    expect(sessionLabel({ sid: 'sid-abcdefghijklmnop-tail', updatedAt: 1 }))
      .toBe('sid-abcdefghijkl…')
  })

  it('sessionLabel can be told the caller IS the work item', () => {
    // The wi detail page labels every row; repeating the wi it is already about
    // says nothing. Expressed as an option on the one precedence function rather
    // than a different expression at the call site — the first draft wrote
    // `s.title || sessionLabel(s)`, which also inverted the order for a session
    // with no title.
    const s: SessionSummary = { sid: 'sid-abcdefghijklmnop-tail', workItem: 'tether#91', title: 'first prompt', updatedAt: 1 }
    expect(sessionLabel(s)).toBe('tether#91')
    expect(sessionLabel(s, { omitWorkItem: true })).toBe('first prompt')
    // …and it still falls all the way through to the sid.
    expect(sessionLabel({ sid: 'sid-abcdefghijklmnop-tail', workItem: 'tether#91', updatedAt: 1 }, { omitWorkItem: true }))
      .toBe('sid-abcdefghijkl…')
  })

  it('sessionsForWorkItem filters and preserves order', () => {
    const rows: SessionSummary[] = [
      { sid: 'new', workItem: 'tether#91', updatedAt: 3 },
      { sid: 'other', workItem: 'tether#90', updatedAt: 2 },
      { sid: 'old', workItem: 'tether#91', updatedAt: 1 },
      { sid: 'unbound', updatedAt: 0 },
    ]
    expect(sessionsForWorkItem(rows, 'tether#91').map(s => s.sid)).toEqual(['new', 'old'])
    // An empty work item matches nothing — never the unbound sessions, which is
    // what a naive `s.workItem === wi` on an empty argument would do.
    expect(sessionsForWorkItem(rows, '')).toEqual([])
  })
})

// tether#101 — isRunningElsewhere, and why its polarity is the OPPOSITE of
// isExternalSession's.
//
// The two predicates sit next to each other and read alike, which is exactly why
// the difference is worth pinning: an UNKNOWN source must count as external
// (silence would be the dangerous answer — a row from a third store would render as
// fully trusted), while an unknown runningAs value must count as HELD (the daemon
// has already excluded the one kind that is not a holder, so a value it sends is
// informative and only its absence says nothing).
describe('isRunningElsewhere', () => {
  const s = (over: Partial<SessionSummary> = {}): SessionSummary =>
    ({ sid: 'sid-000000000001', updatedAt: 1, ...over })

  it('is true for any non-empty kind, including one this build predates', () => {
    for (const kind of ['bg', 'daemon', 'daemon-worker', 'swarm-worker']) {
      expect(isRunningElsewhere(s({ runningAs: kind }))).toBe(true)
    }
  })

  it('is false for absent and for an explicit empty string', () => {
    // Absent = this fixture has no opinion (most of the rows in this repo's tests).
    // Empty = the daemon looked and saw nothing, or has no registry reader at all.
    // Both mean "no observation", and neither may be read as "resumable".
    expect(isRunningElsewhere(s())).toBe(false)
    expect(isRunningElsewhere(s({ runningAs: '' }))).toBe(false)
  })

  it('is independent of the source — the two facts do not imply each other', () => {
    // A tether-recorded session can be held (a background job may resume a sid
    // tether once recorded), and a cc-recorded one need not be.
    expect(isRunningElsewhere(s({ source: 'tether', runningAs: 'bg' }))).toBe(true)
    expect(isRunningElsewhere(s({ source: 'cc' }))).toBe(false)
    expect(isExternalSession(s({ source: 'tether', runningAs: 'bg' }))).toBe(false)
  })

  it('reads the OPPOSITE way from isExternalSession on an unknown value', () => {
    const oddSource = { ...s(), source: 'someday' } as unknown as SessionSummary
    expect(isExternalSession(oddSource)).toBe(true) // unknown ⇒ warn
    expect(isRunningElsewhere(s({ runningAs: 'someday' }))).toBe(true) // value ⇒ inform
    // …and the asymmetry is only in the ABSENT case, which is what makes it safe:
    // an absent source warns nothing and an absent runningAs asserts nothing.
    expect(isExternalSession(s())).toBe(false)
    expect(isRunningElsewhere(s())).toBe(false)
  })

  it('the badge word is `running`, and it is not the agent’s status vocabulary', () => {
    // The agent records a job's status as busy / idle / shell / waiting. An IDLE
    // held session refuses a resume exactly like a busy one, so a badge that said
    // `busy` would be wrong for half of them while looking like a quotation.
    expect(RUNNING_ELSEWHERE_BADGE).toBe('running')
    for (const status of ['busy', 'idle', 'shell', 'waiting']) {
      expect(RUNNING_ELSEWHERE_BADGE).not.toBe(status)
    }
  })
})
