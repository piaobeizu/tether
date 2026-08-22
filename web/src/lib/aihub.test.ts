// tether#163 — getJSON was the eleventh copy of tether#161's defect and the one
// its sweep missed: `throw new AihubError(res.status)` dropped the body, so every
// refusal these eleven fetchers can receive arrived at the panes as a bare status.
//
// The corpus is not invented. Every body below is a literal the daemon really
// writes on a route this module fetches:
//
//   internal/workspace/api.go     readMustBeRelativeBody, readOutsideWorkspaceBody,
//                                 readNotADirectoryBody, registryInternalErrorBody
//   internal/workspace/files.go   ErrPathIsDirectory
//   internal/server/aihub_proxy.go  "aihub not configured", "forbidden",
//                                 "aihub upstream error", "project is required"
//   internal/auth/middleware.go   {"error":"unauthorized"} — the one JSON error
//                                 body reachable here, and the reason getJSON
//                                 cannot just show res.text() raw
//
// Bodies written through http.Error carry exactly one trailing newline, so they
// are spelled with it here and the trimming is exercised by the ordinary path.
import { afterEach, describe, expect, it, vi } from 'vitest'
import {
  AihubError,
  describeError,
  fetchFile,
  fetchProjects,
  fetchWorkspaces,
} from './aihub'
import { httpStatusFallback } from './httpError'

/** A Response-shaped stub: what `fetch` really hands getJSON on a refusal. */
function refusal(status: number, body: string) {
  return {
    ok: false,
    status,
    text: async () => body,
    json: async () => {
      throw new Error('getJSON must not parse a refusal body as JSON')
    },
  }
}

function stubFetch(res: unknown) {
  const fn = vi.fn(async (_url: string) => res)
  vi.stubGlobal('fetch', fn)
  return fn
}

/**
 * The AihubError a fetcher rejected with.
 *
 * A helper rather than `.catch(e => e as AihubError)` at each site: the cast
 * would also swallow a fetcher that RESOLVED, and a test whose subject silently
 * turns into `undefined.message` is not a test. This fails loudly instead, and
 * hands back a properly typed error.
 */
async function refusalFrom(pending: Promise<unknown>): Promise<AihubError> {
  try {
    await pending
  } catch (e) {
    if (e instanceof AihubError) return e
    throw e
  }
  throw new Error('the fetcher resolved on a non-2xx response')
}

/**
 * The string a pane rendered for this refusal BEFORE the fix — status only, body
 * discarded. Every case below names it, so each assertion answers "what did the
 * user see when the defect was present?" rather than only "what do they see
 * now?": an assertion that merely finds the new wording somewhere would pass on
 * the old build the moment the wording appears for any other reason.
 */
function defectValue(status: number): string {
  return `error (HTTP ${status})`
}

afterEach(() => {
  vi.unstubAllGlobals()
  vi.clearAllMocks()
})

describe('getJSON carries the response body into AihubError (tether#163)', () => {
  // The sentence this wi exists for. GET /workspaces/{id}/file is the ONLY route
  // that emits it (workspace.ErrPathIsDirectory, raised by ReadFileContent), and
  // fetchFile is its only caller in this SPA — so before this fix it was
  // unreachable for a user by any path at all.
  it('keeps the directory-not-a-file refusal, which only this route can send', async () => {
    stubFetch(refusal(400, 'workspace: that path is a directory, not a file\n'))

    const err = await refusalFrom(fetchFile('ws1', 'internal'))

    expect(err.status).toBe(400)
    expect(err.message).toBe('workspace: that path is a directory, not a file')
    // What the message used to be. Without this the assertion above could be
    // satisfied by a fixture rather than by the fix.
    expect(err.message).not.toBe(httpStatusFallback(400))
  })

  it.each([
    ['workspace: that path is outside the workspace\n', 400],
    ['workspace: that path must be relative to the workspace root\n', 400],
    ['workspace: that path is not a directory\n', 400],
    ['the daemon could not complete this request\n', 500],
  ])('keeps %s', async (body, status) => {
    stubFetch(refusal(status, body))
    const err = await refusalFrom(fetchFile('ws1', 'x'))
    expect(err.message).toBe(body.trim())
    expect(err.message).not.toBe(httpStatusFallback(status))
  })

  // The daemon's one JSON error body. Shown as its `error` field and not as
  // `{"error":"unauthorized"}`, which is the whole reason getJSON routes through
  // httpErrorMessage instead of using res.text() directly.
  it('reads a JSON body as its message, not as its bytes', async () => {
    stubFetch(refusal(401, '{"error":"unauthorized"}\n'))
    const err = await refusalFrom(fetchProjects())
    expect(err.message).toBe('unauthorized')
    expect(err.message).not.toBe(httpStatusFallback(401))
  })

  // A body-less refusal must still say the status and nothing invented. Note this
  // case passed BEFORE the fix too — it is a regression pin on the old behaviour,
  // not a gate on the defect.
  it('falls back to the status when the body is empty', async () => {
    stubFetch(refusal(418, ''))
    const err = await refusalFrom(fetchWorkspaces())
    expect(err.message).toBe(httpStatusFallback(418))
  })

  // The success path must be untouched: still parsed as JSON, and the body read
  // exactly once — a getJSON that consumed the stream on both branches would
  // reject on a real Response with "body stream already read".
  it('still parses a 200 as JSON and never reads it as text', async () => {
    const text = vi.fn(async () => '')
    stubFetch({ ok: true, status: 200, text, json: async () => [{ name: 'tether' }] })
    await expect(fetchProjects()).resolves.toEqual([{ name: 'tether' }])
    expect(text).not.toHaveBeenCalled()
  })
})

describe('describeError precedence: body vs. the wording written for the screen', () => {
  // The three statuses where this SPA's own wording wins. Each body here is what
  // the daemon really sends, which is what makes the choice defensible: a generic
  // token in every case, never one of the tether#147 / tether#159 sentences.
  //
  // These pass on the old build as well (the wording is unchanged) — they are
  // here as the counterweight to the cases below, so a later edit that decides
  // "the body always wins" cannot land silently.
  it.each([
    [503, 'aihub not configured\n', 'aihub not configured'],
    [403, 'forbidden\n', 'not authorized for this project'],
    [404, '404 page not found\n', 'not found'],
  ])('%i: prefers this SPA\'s wording over the body', (status, body, want) => {
    expect(describeError(new AihubError(status, body.trim()))).toBe(want)
  })

  // Every other status: the body wins, because it is the only thing that says
  // WHICH mistake was made. Left column is what the user saw before.
  it.each([
    [400, 'workspace: that path is a directory, not a file'],
    [400, 'workspace: that path is outside the workspace'],
    [400, 'project is required'],
    [500, 'the daemon could not complete this request'],
    [502, 'aihub upstream error'],
  ])('%i: shows the daemon sentence instead of %s', (status, body) => {
    const got = describeError(new AihubError(status, body))
    expect(got).toBe(body)
    expect(got).not.toBe(defectValue(status))
  })

  it('keeps error (HTTP n) when the body said nothing readable', () => {
    // Constructed without a message, which is what a body-less refusal produces.
    expect(describeError(new AihubError(400))).toBe(defectValue(400))
    expect(describeError(new AihubError(502))).toBe(defectValue(502))
  })

  it('passes a non-AihubError through by its own message', () => {
    expect(describeError(new TypeError('Failed to fetch'))).toBe('Failed to fetch')
    expect(describeError('a string nobody threw as an Error')).toBe(
      'a string nobody threw as an Error',
    )
  })
})
