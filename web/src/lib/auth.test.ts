import { describe, it, expect, beforeEach, afterEach, vi } from 'vitest'
import { verifyToken } from './auth'

// Fetch-mock idiom copied from ./version.test.ts.

const realFetch = globalThis.fetch

function mockFetch(impl: (url: string, init?: RequestInit) => Promise<Response> | Response) {
  const spy = vi.fn((url: unknown, init?: RequestInit) => Promise.resolve(impl(String(url), init)))
  globalThis.fetch = spy as unknown as typeof fetch
  return spy
}

const jsonResponse = (body: unknown, status = 200) =>
  new Response(JSON.stringify(body), { status, headers: { 'Content-Type': 'application/json' } })

function sentBody(init: RequestInit | undefined): { token: string; clientId: string } {
  return JSON.parse(String(init?.body))
}

describe('verifyToken', () => {
  beforeEach(() => {
    localStorage.clear()
  })
  afterEach(() => {
    globalThis.fetch = realFetch
    vi.restoreAllMocks()
  })

  it('sends the token trimmed of surrounding whitespace', async () => {
    const spy = mockFetch(() => jsonResponse({ status: 'ok' }))
    await verifyToken('  abc123  \n')
    const [, init] = spy.mock.calls[0] as [string, RequestInit]
    expect(sentBody(init).token).toBe('abc123')
  })

  it('persists the client id to localStorage before the network request is made', async () => {
    let seenDuringFetch: string | null = null
    mockFetch(() => {
      seenDuringFetch = localStorage.getItem('tether_client_id')
      return jsonResponse({ status: 'ok' })
    })
    await verifyToken('sometoken')
    expect(seenDuringFetch).not.toBeNull()
    expect(seenDuringFetch).toBe(localStorage.getItem('tether_client_id'))
  })

  it('reuses an existing client id across calls instead of minting a new one', async () => {
    const spy = mockFetch(() => jsonResponse({ status: 'ok' }))
    await verifyToken('first')
    await verifyToken('second')
    const [firstCall, secondCall] = spy.mock.calls as [string, RequestInit][]
    const firstClientId = sentBody(firstCall[1]).clientId
    const secondClientId = sentBody(secondCall[1]).clientId
    // Non-degenerate on purpose: a client that sends no clientId at all would
    // have both sides `undefined`, and `expect(undefined).toBe(undefined)`
    // would pass without a client id ever existing. Pin "is a real id" before
    // pinning "is the same id".
    expect(typeof firstClientId).toBe('string')
    expect(firstClientId.length).toBeGreaterThan(0)
    expect(firstClientId).toBe(secondClientId)
  })

  it('pins the wire protocol: POST to /api/v1/auth/verify, JSON content-type, a clientId field', async () => {
    const spy = mockFetch(() => jsonResponse({ status: 'ok' }))
    await verifyToken('sometoken')
    const [url, init] = spy.mock.calls[0] as [string, RequestInit]
    expect(url).toBe('/api/v1/auth/verify')
    expect(init.method).toBe('POST')
    expect(init.headers).toEqual({ 'Content-Type': 'application/json' })
    const body = JSON.parse(String(init.body)) as Record<string, unknown>
    expect(Object.keys(body)).toContain('clientId')
  })

  it('resolves true when the daemon accepts the token', async () => {
    mockFetch(() => jsonResponse({ status: 'ok' }))
    await expect(verifyToken('good-token')).resolves.toBe(true)
  })

  it('resolves false when the daemon rejects the token', async () => {
    mockFetch(() => jsonResponse({ error: 'unauthorized' }, 401))
    await expect(verifyToken('bad-token')).resolves.toBe(false)
  })

  it('propagates a network failure to the caller rather than swallowing it', async () => {
    globalThis.fetch = vi.fn(() => Promise.reject(new Error('offline'))) as unknown as typeof fetch
    await expect(verifyToken('anything')).rejects.toThrow('offline')
  })
})
