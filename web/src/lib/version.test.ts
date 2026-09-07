import { describe, it, expect, beforeEach, afterEach, vi } from 'vitest'
import { fetchAppVersion, resetAppVersionCache, UNKNOWN_VERSION } from './version'

/**
 * The whole point of tether#70 is that the UI no longer keeps its own copy of
 * the version, so these assert that what the daemon says is what comes back —
 * and that a failure degrades to a visible gap rather than to a stale number.
 */

const realFetch = globalThis.fetch

function mockFetch(impl: (url: string) => Promise<Response> | Response) {
  const spy = vi.fn((url: unknown) => Promise.resolve(impl(String(url))))
  globalThis.fetch = spy as unknown as typeof fetch
  return spy
}

const jsonResponse = (body: unknown, status = 200) =>
  new Response(JSON.stringify(body), { status, headers: { 'Content-Type': 'application/json' } })

describe('fetchAppVersion', () => {
  beforeEach(() => resetAppVersionCache())
  afterEach(() => {
    globalThis.fetch = realFetch
    vi.restoreAllMocks()
  })

  it('returns the version the daemon reports, verbatim', async () => {
    mockFetch(() => jsonResponse({ version: 'v0.5.1-0.20260806071712-6ce6c7453229' }))
    await expect(fetchAppVersion()).resolves.toBe('v0.5.1-0.20260806071712-6ce6c7453229')
  })

  it('asks /api/v1/version', async () => {
    const spy = mockFetch(() => jsonResponse({ version: 'v1' }))
    await fetchAppVersion()
    expect(spy).toHaveBeenCalledWith('/api/v1/version')
  })

  it('fetches once no matter how many callers ask', async () => {
    const spy = mockFetch(() => jsonResponse({ version: 'v1' }))
    const [a, b, c] = await Promise.all([fetchAppVersion(), fetchAppVersion(), fetchAppVersion()])
    expect(spy).toHaveBeenCalledTimes(1)
    expect([a, b, c]).toEqual(['v1', 'v1', 'v1'])
  })

  it('returns empty on a non-ok status rather than throwing', async () => {
    mockFetch(() => jsonResponse({ error: 'nope' }, 501))
    await expect(fetchAppVersion()).resolves.toBe('')
  })

  it('returns empty when the request fails outright', async () => {
    globalThis.fetch = vi.fn(() => Promise.reject(new Error('offline'))) as unknown as typeof fetch
    await expect(fetchAppVersion()).resolves.toBe('')
  })

  it('returns empty when the body is not shaped as expected', async () => {
    mockFetch(() => jsonResponse({ version: 42 }))
    await expect(fetchAppVersion()).resolves.toBe('')
  })
})

describe('UNKNOWN_VERSION', () => {
  // Guards the intent: the fallback must not be mistakable for a real version,
  // because a plausible-looking wrong version reads as fact — which is how the
  // old hard-coded constant misled in the first place.
  it('is not a version-like string', () => {
    expect(UNKNOWN_VERSION).not.toMatch(/\d/)
    expect(UNKNOWN_VERSION.startsWith('v')).toBe(false)
  })
})
