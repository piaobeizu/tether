// tether#161 — the unit half. What a body MEANS lives here; that the meaning
// reaches a screen is asserted per call site in the component tests (Settings,
// SkillPane, WorkspacePane, WorkspaceTree), because a helper that returns the
// right string into a variable nobody renders is the same no-op this wi exists
// to close one layer up.
//
// The corpus is not invented. Every plain-text case below is a literal the daemon
// really writes (internal/workspace/state.go, internal/workspace/api.go,
// internal/skill/overlay.go — all through http.Error, hence the trailing "\n"),
// and the JSON case is the one JSON error body reachable on these routes:
// internal/auth/middleware.go answers 401 with {"error":"unauthorized"}.
import { describe, expect, it } from 'vitest'
import {
  HTTP_ERROR_MAX_CHARS,
  HTTP_ERROR_TRUNCATED,
  httpErrorMessage,
  httpErrorText,
  httpStatusFallback,
} from './httpError'

describe('httpErrorText — plain-text refusals (the tether#147 / #159 wording)', () => {
  // http.Error appends exactly one newline. Every case here carries it, so the
  // trim is exercised by the ordinary path rather than by a case about trimming.
  it.each([
    [400, 'workspace: a workspace path must be absolute\n'],
    [400, 'workspace: a workspace path must be a directory that already exists\n'],
    [400, 'skill: a skill source must be absolute\n'],
    [400, 'workspace: that path is not a directory\n'],
    [400, 'workspace: that path is outside the workspace\n'],
    [400, 'workspace: that path must be relative to the workspace root\n'],
    [400, 'workspace: that path is a directory, not a file\n'],
    [500, 'the daemon could not complete this request\n'],
    [404, 'skill: skill is not installed\n'],
    [501, 'not implemented\n'],
    [404, '404 page not found\n'],
  ])('%i: keeps the daemon sentence and drops the status', (status, body) => {
    expect(httpErrorText(status, body)).toBe(body.trim())
    // The value the defect produced. Named, so this case answers "what did the
    // user see before?" rather than only "what do they see now?".
    expect(httpErrorText(status, body)).not.toBe(httpStatusFallback(status))
  })

  it('collapses an interior newline so a multi-line body still reads as a sentence', () => {
    expect(httpErrorText(400, 'bad request\nsecond line\n')).toBe('bad request second line')
  })
})

describe('httpErrorText — nothing to say falls back to the status', () => {
  it.each([
    ['an empty body — a HEAD, which net/http strips', ''],
    ['the newline http.Error writes for an empty message', '\n'],
    ['whitespace only', '   \t \n '],
  ])('%s', (_why, body) => {
    expect(httpErrorText(404, body)).toBe('HTTP 404')
  })

  it('falls back rather than putting a machine-readable JSON object on screen', () => {
    expect(httpErrorText(409, '{"code":17,"retryable":false}')).toBe('HTTP 409')
  })

  it('falls back for JSON that is not an object and carries no words', () => {
    expect(httpErrorText(500, '[1,2,3]')).toBe('HTTP 500')
    expect(httpErrorText(500, 'null')).toBe('HTTP 500')
  })
})

describe('httpErrorText — JSON bodies', () => {
  it('reads `error`, which is what the auth middleware sends on a 401', () => {
    expect(httpErrorText(401, '{"error":"unauthorized"}\n')).toBe('unauthorized')
  })

  it.each([
    ['{"message":"the daemon could not complete this request"}', 'the daemon could not complete this request'],
    ['{"detail":"that path is outside the workspace"}', 'that path is outside the workspace'],
    ['{"reason":"skill is not installed"}', 'skill is not installed'],
    ['"a bare JSON string is still a sentence"', 'a bare JSON string is still a sentence'],
  ])('reads %s', (body, want) => {
    expect(httpErrorText(400, body)).toBe(want)
  })

  it('prefers `error` over `message` when a body carries both', () => {
    expect(httpErrorText(400, '{"message":"second","error":"first"}')).toBe('first')
  })

  it('skips a field that is present but empty', () => {
    expect(httpErrorText(400, '{"error":"   ","message":"the real one"}')).toBe('the real one')
  })

  it('skips a field that is present but not a string', () => {
    expect(httpErrorText(400, '{"error":42,"message":"the real one"}')).toBe('the real one')
  })

  it('shows a body that only LOOKS like JSON verbatim', () => {
    expect(httpErrorText(400, '{not json after all')).toBe('{not json after all')
  })
})

describe('httpErrorText — the cap, and saying so', () => {
  it('leaves a body exactly at the limit alone', () => {
    const body = 'x'.repeat(HTTP_ERROR_MAX_CHARS)
    expect(httpErrorText(502, body)).toBe(body)
    expect(httpErrorText(502, body)).not.toContain(HTTP_ERROR_TRUNCATED)
  })

  it('cuts one character over the limit and marks the cut', () => {
    const body = 'y'.repeat(HTTP_ERROR_MAX_CHARS + 1)
    const got = httpErrorText(502, body)
    expect(got).toBe('y'.repeat(HTTP_ERROR_MAX_CHARS) + HTTP_ERROR_TRUNCATED)
    // The mark is the point: a body that merely stops mid-word reads as the
    // daemon having said something incoherent.
    expect(got.endsWith(HTTP_ERROR_TRUNCATED)).toBe(true)
  })

  it('caps a proxy HTML page instead of pasting it into the error row', () => {
    const html = `<html><head><title>502 Bad Gateway</title></head><body>${'nginx '.repeat(400)}</body></html>`
    const got = httpErrorText(502, html)
    expect(got.length).toBe(HTTP_ERROR_MAX_CHARS + HTTP_ERROR_TRUNCATED.length)
    expect(got.startsWith('<html><head><title>502 Bad Gateway</title>')).toBe(true)
  })

  it('measures the cap AFTER whitespace collapsing, not before', () => {
    // 300 words of a single letter separated by newlines: 599 raw characters,
    // 599 flattened too — so pick a body whose collapse actually shortens it.
    const body = 'z\n\n\n'.repeat(HTTP_ERROR_MAX_CHARS / 2)
    // Flattens to "z z z …" — 2 chars per repeat minus the final space.
    expect(httpErrorText(502, body)).toBe('z '.repeat(HTTP_ERROR_MAX_CHARS / 2).trim())
  })
})

describe('httpErrorMessage — reading the body off a response', () => {
  it('reads a real Response body', async () => {
    const res = new Response('workspace: that path is not a directory\n', { status: 400 })
    await expect(httpErrorMessage(res)).resolves.toBe('workspace: that path is not a directory')
  })

  it('reads a real JSON Response body', async () => {
    const res = new Response('{"error":"unauthorized"}\n', {
      status: 401,
      headers: { 'Content-Type': 'application/json' },
    })
    await expect(httpErrorMessage(res)).resolves.toBe('unauthorized')
  })

  it('falls back when the stub has no text() at all', async () => {
    // Every fetch stub written before this wi is this shape, and they must keep
    // behaving as they did rather than start rejecting.
    await expect(httpErrorMessage({ status: 500 })).resolves.toBe('HTTP 500')
  })

  it('falls back when reading the body throws', async () => {
    const res = { status: 502, text: () => Promise.reject(new Error('connection reset')) }
    await expect(httpErrorMessage(res)).resolves.toBe('HTTP 502')
  })

  it('never rejects — an error path that can fail hides the error it reports', async () => {
    const res = {
      status: 503,
      text: () => {
        throw new TypeError('body already consumed')
      },
    }
    await expect(httpErrorMessage(res)).resolves.toBe('HTTP 503')
  })

  it('falls back when text() resolves to something that is not a string', async () => {
    const res = { status: 500, text: () => Promise.resolve(undefined as unknown as string) }
    await expect(httpErrorMessage(res)).resolves.toBe('HTTP 500')
  })
})
