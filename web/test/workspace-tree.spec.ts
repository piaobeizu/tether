/**
 * T4 — WorkspaceTree fetch/cache logic tests.
 *
 * No jsdom/React Testing Library is configured in this project (vitest runs
 * in the default `node` environment — see web/package.json), so per the F2
 * brief we test the data/fetch-caching logic directly by extracting it into
 * a small testable module (fileTreeCache.ts) rather than rendering the DOM.
 * This mirrors store.spec.ts's style: direct calls, mocked fetch, no network.
 */
import { afterEach, describe, expect, it, vi } from 'vitest'
import { createFileTreeCache, type FileEntry } from '../src/panes/workspace/fileTreeCache'

function mockFetchOnce(entries: FileEntry[]) {
  return vi.fn(async () => ({
    ok: true,
    json: async () => entries,
  })) as unknown as typeof fetch
}

describe('createFileTreeCache', () => {
  afterEach(() => {
    vi.restoreAllMocks()
  })

  it('fetches entries for a directory on first load', async () => {
    const entries: FileEntry[] = [
      { name: 'src', isDir: true, dirty: false },
      { name: 'README.md', isDir: false, dirty: false },
    ]
    const fetchMock = mockFetchOnce(entries)
    const cache = createFileTreeCache('ws-1', fetchMock)

    const result = await cache.load('')

    expect(result).toEqual(entries)
    expect(fetchMock).toHaveBeenCalledTimes(1)
    expect(fetchMock).toHaveBeenCalledWith('/api/v1/workspaces/ws-1/files?dir=')
  })

  it('marks a dirty entry from the response', async () => {
    const entries: FileEntry[] = [
      { name: 'dirty.txt', isDir: false, dirty: true },
      { name: 'clean.txt', isDir: false, dirty: false },
    ]
    const fetchMock = mockFetchOnce(entries)
    const cache = createFileTreeCache('ws-1', fetchMock)

    const result = await cache.load('')

    expect(result.find(e => e.name === 'dirty.txt')?.dirty).toBe(true)
    expect(result.find(e => e.name === 'clean.txt')?.dirty).toBe(false)
  })

  it('caches a directory after first load and does not re-fetch', async () => {
    const entries: FileEntry[] = [{ name: 'a.txt', isDir: false, dirty: false }]
    const fetchMock = mockFetchOnce(entries)
    const cache = createFileTreeCache('ws-1', fetchMock)

    const first = await cache.load('sub')
    const second = await cache.load('sub')

    expect(fetchMock).toHaveBeenCalledTimes(1)
    expect(second).toBe(first) // same cached reference
  })

  it('fetches distinct directories independently', async () => {
    const fetchMock = vi.fn(async (url: string) => ({
      ok: true,
      json: async () =>
        url.includes('dir=a')
          ? [{ name: 'a-child', isDir: false, dirty: false }]
          : [{ name: 'b-child', isDir: false, dirty: false }],
    })) as unknown as typeof fetch
    const cache = createFileTreeCache('ws-1', fetchMock)

    const a = await cache.load('a')
    const b = await cache.load('b')

    expect(fetchMock).toHaveBeenCalledTimes(2)
    expect(a[0]?.name).toBe('a-child')
    expect(b[0]?.name).toBe('b-child')
  })

  it('encodes the dir query parameter', async () => {
    const fetchMock = mockFetchOnce([])
    const cache = createFileTreeCache('ws-1', fetchMock)

    await cache.load('sub dir/nested')

    expect(fetchMock).toHaveBeenCalledWith(
      '/api/v1/workspaces/ws-1/files?dir=' + encodeURIComponent('sub dir/nested')
    )
  })

  it('throws on non-ok response and does not cache the failure', async () => {
    const fetchMock = vi.fn()
      .mockResolvedValueOnce({ ok: false, status: 500, json: async () => [] })
      .mockResolvedValueOnce({ ok: true, json: async () => [{ name: 'ok.txt', isDir: false, dirty: false }] })
    const cache = createFileTreeCache('ws-1', fetchMock as unknown as typeof fetch)

    await expect(cache.load('x')).rejects.toThrow()

    const result = await cache.load('x')
    expect(result[0]?.name).toBe('ok.txt')
    expect(fetchMock).toHaveBeenCalledTimes(2)
  })

  it('invalidate() clears the cache for a directory so it re-fetches', async () => {
    const fetchMock = mockFetchOnce([{ name: 'a.txt', isDir: false, dirty: false }])
    const cache = createFileTreeCache('ws-1', fetchMock)

    await cache.load('sub')
    cache.invalidate('sub')
    await cache.load('sub')

    expect(fetchMock).toHaveBeenCalledTimes(2)
  })
})
