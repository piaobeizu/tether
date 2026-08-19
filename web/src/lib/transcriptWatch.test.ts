// tether#106 — the transcript change probe.
//
// The defect this module fixes is invisible in a render test: a transcript frozen at
// the moment it was fetched renders exactly like one that is up to date. So the
// assertions here are about the MECHANISM — that a probe happens on a clock, that it
// is a HEAD and not a GET, that a change fires exactly once, that an unchanged answer
// fires nothing, and that the timer is actually gone after the stop.
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import {
  TRANSCRIPT_POLL_MS,
  TRANSCRIPT_UPDATED_AT_HEADER,
  fetchTranscriptVersion,
  noteTranscriptVersion,
  readTranscriptBounds,
  readTranscriptVersion,
  resetTranscriptWatchForTests,
  transcriptPath,
  transcriptWatchState,
  watchTranscript,
} from './transcriptWatch'

/**
 * daemon stubs fetch with a version the test can move between probes, and records the
 * method and url of every request that was actually issued.
 *
 * The header is served through a real `Headers` object rather than a hand-rolled
 * `{get}`: the name is looked up case-insensitively by the platform, and a hand-rolled
 * map would make a test that passes for a name the browser would not match.
 */
function daemon(version: () => number | null) {
  const seen: { method: string; url: string }[] = []
  const fn = vi.fn(async (url: string, init?: { method?: string }) => {
    seen.push({ method: init?.method ?? 'GET', url })
    const v = version()
    const headers = new Headers()
    if (v !== null) headers.set(TRANSCRIPT_UPDATED_AT_HEADER, String(v))
    return { ok: true, status: 200, headers, json: async () => [] }
  })
  vi.stubGlobal('fetch', fn)
  return { fn, seen, calls: () => seen.length }
}

/** Put the tab in the background (or back), the way the browser reports it. */
function setVisibility(state: 'visible' | 'hidden') {
  Object.defineProperty(document, 'visibilityState', { value: state, configurable: true })
  document.dispatchEvent(new Event('visibilitychange'))
}

beforeEach(() => {
  resetTranscriptWatchForTests()
  Object.defineProperty(document, 'visibilityState', { value: 'visible', configurable: true })
  vi.useFakeTimers()
})

afterEach(() => {
  resetTranscriptWatchForTests()
  vi.useRealTimers()
  vi.unstubAllGlobals()
  vi.restoreAllMocks()
})

describe('the wire contract', () => {
  it('pins the header name as the daemon spells it', () => {
    // A literal, not "some non-empty string". This IS the contract, and the Go side
    // has a ratchet (TestTranscriptUpdatedAtHeaderIsMirroredInTypeScript) that reads
    // this file for exactly it; pinning it here too is what makes a one-sided rename
    // fail on BOTH sides rather than only in a guard a hurried author might delete.
    expect(TRANSCRIPT_UPDATED_AT_HEADER).toBe('X-Tether-Transcript-Updated-At')
  })

  it('addresses the same route the transcript is loaded from', () => {
    expect(transcriptPath('sid-a')).toBe('/api/v1/sessions/sid-a/messages')
    expect(transcriptPath('a/b?c')).toBe('/api/v1/sessions/a%2Fb%3Fc/messages')
  })
})

describe('readTranscriptVersion', () => {
  it('reads the header', () => {
    const headers = new Headers()
    headers.set(TRANSCRIPT_UPDATED_AT_HEADER, '1755500000000')
    expect(readTranscriptVersion({ headers })).toBe(1755500000000)
  })

  it('is 0 — not NaN, not a throw — for every way the header can be missing or junk', () => {
    // 0 means UNKNOWN, and unknown must compare unequal to every real version so the
    // reader refreshes once and learns the truth. NaN would compare unequal to
    // itself and refresh on every single tick; a throw would take the probe down.
    const with_ = (v: string) => { const h = new Headers(); h.set(TRANSCRIPT_UPDATED_AT_HEADER, v); return { headers: h } }
    expect(readTranscriptVersion({ headers: new Headers() })).toBe(0)
    expect(readTranscriptVersion({})).toBe(0)
    expect(readTranscriptVersion(with_(''))).toBe(0)
    expect(readTranscriptVersion(with_('nonsense'))).toBe(0)
    expect(readTranscriptVersion(with_('-5'))).toBe(0)
  })
})

describe('fetchTranscriptVersion', () => {
  it('asks with HEAD, so the daemon answers from a stat instead of the whole transcript', async () => {
    // The reason this module exists at all. SessionIndex.Messages prefers an
    // os.ReadFile of the whole history.jsonl with no tail and no cap, so a GET here
    // would cost, every three seconds, exactly what the load it is meant to avoid
    // costs — and every other assertion in this file would still pass.
    const d = daemon(() => 1755500000000)
    await expect(fetchTranscriptVersion('sid-a')).resolves.toBe(1755500000000)
    expect(d.seen).toEqual([{ method: 'HEAD', url: '/api/v1/sessions/sid-a/messages' }])
  })

  it('throws on a daemon error so the probe keeps the previous answer', async () => {
    vi.stubGlobal('fetch', vi.fn(async () => ({ ok: false, status: 503, headers: new Headers() })))
    await expect(fetchTranscriptVersion('sid-a')).rejects.toThrow(/503/)
  })
})

describe('watchTranscript', () => {
  it('probes on its own clock and fires ONLY when the version moved', async () => {
    // The heart of it. Two mutations die here and they are different mutations:
    // delete the interval and the second assertion fails; drop the comparison and
    // fire on every tick and the third one does.
    let version = 100
    const d = daemon(() => version)
    noteTranscriptVersion('sid-a', 100)
    const fired: number[] = []

    watchTranscript('sid-a', () => { fired.push(d.calls()) })
    await vi.advanceTimersByTimeAsync(0)
    expect(d.calls()).toBe(1)   // the immediate probe on start
    expect(fired).toEqual([])   // nothing changed, so nothing to do

    await vi.advanceTimersByTimeAsync(TRANSCRIPT_POLL_MS)
    expect(d.calls()).toBe(2)
    expect(fired).toEqual([])

    version = 200
    await vi.advanceTimersByTimeAsync(TRANSCRIPT_POLL_MS)
    expect(d.calls()).toBe(3)
    expect(fired).toEqual([3])  // fired exactly once, on the probe that saw the change

    // …and not again while it stays at 200. A callback that fires on every tick
    // reloads the whole transcript every three seconds, which is the cost this
    // module was built to avoid.
    await vi.advanceTimersByTimeAsync(TRANSCRIPT_POLL_MS * 3)
    expect(fired).toEqual([3])
  })

  it('probes immediately on start, not one interval later', async () => {
    // The state that turns the watch on — the pane learning its attach was refused —
    // arrives seconds after the transcript was fetched, so whatever the other agent
    // wrote in that window is already missing. Waiting out the first interval would
    // make the reader stare at a transcript that is known-stale on arrival.
    const d = daemon(() => 100)
    noteTranscriptVersion('sid-a', 50)
    const fired: string[] = []
    watchTranscript('sid-a', () => fired.push('x'))
    await vi.advanceTimersByTimeAsync(0)
    expect(d.calls()).toBe(1)
    expect(fired).toEqual(['x'])
  })

  it('fires when it has no baseline, so a reload lands on a known version', async () => {
    // No noteTranscriptVersion first: this is the page-reload path, where the
    // transcript was loaded by ChatPane's own [sessionId] effect and this module
    // never saw which version it was. Unknown (0) differs from any real version, so
    // the reader refreshes once and the module learns the truth.
    daemon(() => 100)
    const fired: string[] = []
    watchTranscript('sid-a', () => fired.push('x'))
    await vi.advanceTimersByTimeAsync(0)
    expect(fired).toEqual(['x'])
    // And having recorded it, it does NOT keep firing.
    await vi.advanceTimersByTimeAsync(TRANSCRIPT_POLL_MS * 3)
    expect(fired).toEqual(['x'])
  })

  it('does not fire when the daemon sends no version at all', async () => {
    // Unknown on both sides. Firing here would be an unbounded reload loop against a
    // daemon that cannot tell us anything.
    daemon(() => null)
    const fired: string[] = []
    watchTranscript('sid-a', () => fired.push('x'))
    await vi.advanceTimersByTimeAsync(TRANSCRIPT_POLL_MS * 3)
    expect(fired).toEqual([])
  })

  it('stops the TIMER on stop, not just the callbacks', async () => {
    // Asserting only "the callback stopped firing" would miss a leaked interval: it
    // keeps issuing requests, and on a phone it keeps a radio busy. The handle is the
    // thing to assert, which is why transcriptWatchState exposes it.
    const d = daemon(() => 100)
    const stop = watchTranscript('sid-a', () => {})
    await vi.advanceTimersByTimeAsync(0)
    expect(transcriptWatchState()).toEqual({ running: true, sid: 'sid-a', version: 100 })

    stop()
    expect(transcriptWatchState().running).toBe(false)
    const after = d.calls()
    await vi.advanceTimersByTimeAsync(TRANSCRIPT_POLL_MS * 5)
    expect(d.calls()).toBe(after)
  })

  it('survives a stop from a superseded watch (StrictMode runs effect, cleanup, effect)', async () => {
    // React 18 dev mounts an effect, runs its cleanup, then runs the effect again.
    // A stop that blindly cleared the module state would tear down the watch its own
    // re-run had just installed — and the symptom is the frozen transcript again,
    // in dev only.
    const d = daemon(() => 100)
    const stopFirst = watchTranscript('sid-a', () => {})
    watchTranscript('sid-a', () => {})
    stopFirst()
    await vi.advanceTimersByTimeAsync(0)
    expect(transcriptWatchState().running).toBe(true)
    const before = d.calls()
    await vi.advanceTimersByTimeAsync(TRANSCRIPT_POLL_MS)
    expect(d.calls()).toBe(before + 1)
  })

  it('drops an answer whose watch has been replaced', async () => {
    // A probe in flight when the reader switches sessions. Without the guard the late
    // answer is not merely useless, it is wrong twice: `onChanged` is REASSIGNED by the
    // new watch (not queued), so firing reloads the session on screen because a
    // DIFFERENT one changed, and the version gets recorded against the old sid — which
    // leaves the session actually on screen with no baseline, so its next probe reloads
    // it again. Measured: without this case, replacing the guard with `if (false)`
    // survives every other test in this file.
    let releaseA!: () => void
    const gateA = new Promise<void>(r => { releaseA = r })
    vi.stubGlobal('fetch', vi.fn(async (url: string) => {
      const headers = new Headers()
      if (url.includes('sid-a')) {
        await gateA
        headers.set(TRANSCRIPT_UPDATED_AT_HEADER, '999')
      } else {
        headers.set(TRANSCRIPT_UPDATED_AT_HEADER, '200')
      }
      return { ok: true, status: 200, headers }
    }))

    watchTranscript('sid-a', () => {})   // its probe hangs
    await vi.advanceTimersByTimeAsync(0)
    watchTranscript('sid-b', () => {})   // supersedes it
    await vi.advanceTimersByTimeAsync(0)
    expect(transcriptWatchState()).toEqual({ running: true, sid: 'sid-b', version: 200 })

    releaseA()
    await vi.advanceTimersByTimeAsync(0)

    // sid-a's answer landed and changed nothing.
    expect(transcriptWatchState()).toEqual({ running: true, sid: 'sid-b', version: 200 })
  })

  it('pauses while the tab is hidden and checks immediately on return', async () => {
    let version = 100
    const d = daemon(() => version)
    noteTranscriptVersion('sid-a', 100)
    watchTranscript('sid-a', () => {})
    await vi.advanceTimersByTimeAsync(0)
    const atHide = d.calls()

    setVisibility('hidden')
    expect(transcriptWatchState().running).toBe(false)
    await vi.advanceTimersByTimeAsync(TRANSCRIPT_POLL_MS * 5)
    expect(d.calls()).toBe(atHide)

    version = 200
    setVisibility('visible')
    await vi.advanceTimersByTimeAsync(0)
    // Exactly one extra request, right away: without the immediate probe, coming back
    // to a tab would show a transcript up to one whole interval stale — which looks
    // exactly like the freeze the pause was traded for.
    expect(d.calls()).toBe(atHide + 1)
    expect(transcriptWatchState().running).toBe(true)
  })

  it('does not pile up probes when the daemon is slower than the interval', async () => {
    let release!: () => void
    const gate = new Promise<void>(r => { release = r })
    let calls = 0
    vi.stubGlobal('fetch', vi.fn(async () => {
      calls++
      await gate
      return { ok: true, status: 200, headers: new Headers() }
    }))

    watchTranscript('sid-a', () => {})
    await vi.advanceTimersByTimeAsync(TRANSCRIPT_POLL_MS * 4)
    expect(calls).toBe(1)
    release()
  })

  it('recovers when a probe never settles, instead of freezing itself', async () => {
    // The dedupe above is also a way to deadlock: `inFlight` is released only when a
    // request settles, so one that never does would make every later tick a no-op
    // while the timer kept running. The deadline is what stops that, and a deadline
    // that cannot be asserted is the one thing this module cannot leave on trust.
    let calls = 0
    vi.stubGlobal('fetch', vi.fn((_url: string, init?: { signal?: AbortSignal }) => {
      calls++
      return new Promise((_resolve, reject) => {
        init?.signal?.addEventListener('abort', () => reject(new Error('aborted')))
      })
    }))

    watchTranscript('sid-a', () => {})
    await vi.advanceTimersByTimeAsync(TRANSCRIPT_POLL_MS)
    expect(calls).toBe(1)          // still stuck in the first one
    await vi.advanceTimersByTimeAsync(TRANSCRIPT_POLL_MS * 2)
    expect(calls).toBeGreaterThan(1) // the deadline fired and the module resumed
  })
})

// ────────────────────────────────────────────────────────────────────────────
// tether#107 — the two BOUNDARY headers, and the URL that spends the cursor.
//
// These are the smallest units of the change, and the reason they get their own
// assertions is that both of their failure modes are silent-and-confident rather
// than loud: a cursor that reads as null makes the pane state that a truncated
// transcript is complete, and a `before` that is dropped from the URL makes every
// "load earlier" serve the newest page again.
describe('readTranscriptBounds (tether#107)', () => {
  const res = (h: Record<string, string>) => ({ headers: new Headers(h) })

  it('reads a positive cursor and the other-record store name', () => {
    const b = readTranscriptBounds(res({
      'X-Tether-Transcript-Earlier': '1048576',
      'X-Tether-Transcript-Other-Record': 'cc',
    }))
    expect(b.earlier).toBe(1048576)
    expect(b.otherRecord).toBe('cc')
  })

  it('reads BOTH as null when neither header is there — the daemon omits them to mean "no"', () => {
    const b = readTranscriptBounds(res({}))
    expect(b.earlier).toBeNull()
    expect(b.otherRecord).toBeNull()
  })

  it('reads null from a response with no headers at all', () => {
    // A stub Response (this repo's fetch mocks) or a non-browser import.
    expect(readTranscriptBounds({}).earlier).toBeNull()
    expect(readTranscriptBounds({}).otherRecord).toBeNull()
  })

  // The strictness is the point. Every one of these would otherwise become a
  // cursor the button sends, and `?before=NaN` is a 400 — a "load earlier" that
  // can never succeed, offered forever.
  it.each([
    ['not a number', 'soon'],
    ['zero', '0'],
    ['negative', '-8'],
    ['fractional', '1.5'],
    ['infinite', 'Infinity'],
    ['empty', ''],
  ])('reads %s (%j) as no earlier page', (_name, raw) => {
    expect(readTranscriptBounds(res({ 'X-Tether-Transcript-Earlier': raw })).earlier).toBeNull()
  })
})

describe('transcriptPath carries the cursor (tether#107)', () => {
  it('omits `before` entirely when there is none', () => {
    // Not `?before=` and not `?before=0`: the daemon reads an absent parameter as
    // "the newest page" and 0 as "the page ending at byte zero", which is EMPTY. A
    // caller that always appended it would blank every transcript it opened.
    expect(transcriptPath('sid-abcdefgh')).toBe('/api/v1/sessions/sid-abcdefgh/messages')
  })

  it('appends the cursor when there is one', () => {
    expect(transcriptPath('sid-abcdefgh', 1048576)).toBe('/api/v1/sessions/sid-abcdefgh/messages?before=1048576')
  })

  it('still encodes the sid with a cursor present', () => {
    expect(transcriptPath('a/b', 12)).toBe('/api/v1/sessions/a%2Fb/messages?before=12')
  })

  it('sends 0 when asked for 0 — absent and zero are different requests', () => {
    expect(transcriptPath('sid-abcdefgh', 0)).toBe('/api/v1/sessions/sid-abcdefgh/messages?before=0')
  })
})
