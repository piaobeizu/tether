/**
 * createFileTreeCache — the client half of GET /api/v1/workspaces/{id}/files.
 *
 * ── What this file is FOR (tether#188) ──────────────────────────────────────
 *
 * Not "the bit of WorkspaceTree that was easy to unit-test". This module is the
 * client of a live HTTP contract that NOTHING ELSE CHECKS, so the literals in
 * the assertions below are the only thing holding the two sides together:
 *
 *   - the URL shape `?dir=<encodeURIComponent(dir)>`, with `''` meaning the
 *     workspace root
 *   - the field names `isDir` / `dirty`, which must stay camelCase to match
 *     `internal/workspace/files.go`'s `FileEntry` json tags (verified against
 *     that struct on this branch)
 *   - a non-2xx body being shown to the user rather than dropped (tether#161)
 *
 * 🔴 That contract is NOT in web/src/lib/wire.gen.ts — grep it for `FileEntry`,
 * `isDir` or `dirty` and you get 0 hits — so the codegen drift gate in CI does
 * NOT cover it. Two hand-written sides, and these assertions in between. Change
 * a literal here only together with internal/workspace/files.go.
 *
 * Two server guarantees this client relies on and does NOT reproduce: listFiles
 * sorts directories-first-then-name, and `.git` is never listed
 * (internal/workspace/files.go). They are asserted server-side, not here.
 *
 * ── The filename ───────────────────────────────────────────────────────────
 *
 * `workspace-tree.spec.ts` is a misleading name — nothing here touches a tree,
 * and WorkspaceTree.tsx was deleted in tether#174. It is kept ANYWAY because
 * web/test/scaffold.spec.ts names this file as a literal string, that file is
 * outside this wi's declared resources, and three sibling ports are in flight
 * against the same branch. Rename it together with scaffold.spec.ts, not alone.
 *
 * ── AC-R9 is NOT satisfied by this file ────────────────────────────────────
 *
 * tether#173's AC-R9 wants the refusal wording asserted where a user would read
 * it — on rendered text. This file asserts it on `Error.message`, one layer
 * short, because phase 1 has no rendering consumer yet: the only one there ever
 * was, WorkspaceTree.tsx, is deleted, and the shell that replaces it is blocked
 * on the three open owner questions in tether#173 §9. A component written here
 * purely to host the assertion would be a fixture asserting against itself.
 * ⇒ The `fetch → wording → screen` hop is a KNOWN GAP, owed by whichever wi
 *   first renders a file tree. Same gap is recorded in src/lib/httpError.test.ts.
 */
import { afterEach, describe, expect, it, vi } from 'vitest'
import { createFileTreeCache, type FileEntry } from '../src/lib/fileTreeCache'
import { httpStatusFallback } from '../src/lib/httpError'

function mockFetchOnce(entries: FileEntry[]) {
  return vi.fn(async () => ({
    ok: true,
    json: async () => entries,
  })) as unknown as typeof fetch
}

/**
 * The message of the Error `p` rejects with, or a failure if it resolves.
 *
 * Used instead of `rejects.toThrow(...)` for the two tether#161 arms so they can
 * assert the message EXACTLY. `toThrow('x')` is a substring match, which would
 * also accept a build that buried the daemon's sentence inside `HTTP 400: …`,
 * and the anchored-regex alternative had to build a RegExp out of the expected
 * value — safe only while that value contains no regex metacharacters. Exact
 * equality needs neither escape hatch.
 */
async function rejectionMessage(p: Promise<unknown>): Promise<string> {
  const resolvedMarker = Symbol('resolved')
  const outcome = await p.then(() => resolvedMarker, (e: unknown) => e)
  if (outcome === resolvedMarker) throw new Error('expected a rejection, but the promise resolved')
  return (outcome as Error).message
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

  // tether#161's whole point — the daemon's own sentence reaching Error.message
  // — was NEVER asserted by this spec before tether#188. The stub below used to
  // be `{ ok:false, status:500, json: async () => [] }`, i.e. it had no `text()`,
  // and httpErrorMessage DELIBERATELY degrades such a source to `HTTP <status>`;
  // the assertion was a bare `rejects.toThrow()` with no argument, which checks
  // no message at all. So the passthrough could have been deleted outright with
  // this file still green. The one test that did cover it, WorkspaceTree.test.tsx,
  // was deleted in tether#174.
  //
  // Both arms are needed and they are each other's negative control: arm 1 fails
  // if the passthrough is removed, arm 2 fails if the degradation is removed.
  // Neither alone distinguishes those two builds.
  it('passes the daemon refusal through to Error.message, and does not cache the failure (tether#161, arm 1)', async () => {
    // A real internal/workspace/api.go refusal, reachable ONLY on this route —
    // http.Error writes the trailing newline, which the wording must survive.
    const fetchMock = vi.fn()
      .mockResolvedValueOnce({
        ok: false,
        status: 400,
        text: async () => 'workspace: that path is not a directory\n',
      })
      .mockResolvedValueOnce({ ok: true, json: async () => [{ name: 'ok.txt', isDir: false, dirty: false }] })
    const cache = createFileTreeCache('ws-1', fetchMock as unknown as typeof fetch)

    // Exact, not a substring: "the wording appears somewhere in the message" is
    // a weaker claim than "the wording IS the message", and only the second one
    // rules out a build that prefixed it with the status.
    expect(await rejectionMessage(cache.load('x'))).toBe('workspace: that path is not a directory')

    // Still the failure-is-not-cached assertion this test started life as.
    const result = await cache.load('x')
    expect(result[0]?.name).toBe('ok.txt')
    expect(fetchMock).toHaveBeenCalledTimes(2)
  })

  it('degrades to the status when the response carries no body reader (tether#161, arm 2)', async () => {
    // The pre-tether#161 stub shape, kept deliberately: every fetch stub written
    // before that wi looks like this, and they must keep degrading rather than
    // start rejecting.
    const fetchMock = vi.fn(async () => ({
      ok: false,
      status: 500,
      json: async () => [],
    })) as unknown as typeof fetch
    const cache = createFileTreeCache('ws-1', fetchMock)

    // Derived from httpStatusFallback rather than restating 'HTTP 500' — a gate
    // whose expected value is a COPY of the thing under test stops moving when
    // the thing moves (tether#173 §5).
    expect(await rejectionMessage(cache.load('x'))).toBe(httpStatusFallback(500))
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
