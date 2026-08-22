// tether#163 — the "reaches the eyes" half.
//
// Canvas.test.tsx mocks lib/aihub wholesale, so nothing in it can tell whether a
// refusal body survives the trip from `fetch` to the DOM: with fetchFile stubbed,
// getJSON never runs. That is exactly the hop this wi is about, and a fix
// verified only where the module is mocked out is a fix verified nowhere.
//
// So this file mocks nothing. It stubs `fetch` with the bytes the daemon really
// puts on the wire and asserts on rendered text, which makes the whole chain
// load-bearing: getJSON → httpErrorMessage → AihubError → describeError →
// <div className="work-error">. Break any one link and this goes red.
import { afterEach, describe, expect, it, vi } from 'vitest'
import { cleanup, render, screen } from '@testing-library/react'
import Canvas from './index'
import { useStore } from '../../lib/store'

// GET /api/v1/workspaces/{id}/file, verbatim: a body written by http.Error (one
// trailing newline) and, for the 400s, the status readRefusal derives from the
// sentinel named beside it.
const DIRECTORY_NOT_A_FILE = 'workspace: that path is a directory, not a file'
const OUTSIDE_WORKSPACE = 'workspace: that path is outside the workspace'
const MUST_BE_RELATIVE = 'workspace: that path must be relative to the workspace root'

function stubRefusal(status: number, body: string) {
  const fetchMock = vi.fn(async (_url: string) => ({
    ok: false,
    status,
    text: async () => body + '\n',
    json: async () => {
      throw new Error('the error path must not parse the body as JSON')
    },
  }))
  vi.stubGlobal('fetch', fetchMock)
  return fetchMock
}

// @testing-library's auto-cleanup needs a global afterEach, and vitest's
// `globals` option is off here — same reason Canvas.test.tsx cleans up by hand.
afterEach(() => {
  cleanup()
  useStore.getState().select(null)
  vi.unstubAllGlobals()
  vi.clearAllMocks()
})

describe('Canvas file preview shows the daemon refusal, not its status (tether#163)', () => {
  it('renders the directory-not-a-file sentence a user could never reach before', async () => {
    const fetchMock = stubRefusal(400, DIRECTORY_NOT_A_FILE)
    useStore.getState().select({ file: { wsId: 'ws1', path: 'internal' } })

    const { container } = render(<Canvas />)

    const shown = await screen.findByText(DIRECTORY_NOT_A_FILE)
    // The pane's error slot specifically, not merely somewhere in the tree.
    expect(shown.className).toBe('work-error')
    // What this pane rendered for the same response before the fix. Naming it is
    // what makes the assertion above a gate rather than a restatement of the
    // fixture: `error (HTTP 400)` is what a body-discarding getJSON produces, and
    // it is still what an empty body produces today.
    expect(container.querySelector('.work-error')?.textContent).not.toBe('error (HTTP 400)')

    // It really was this route, reached through the real fetcher.
    expect(fetchMock).toHaveBeenCalledTimes(1)
    expect(String(fetchMock.mock.calls[0][0])).toBe(
      '/api/v1/workspaces/ws1/file?path=internal',
    )
  })

  it.each([OUTSIDE_WORKSPACE, MUST_BE_RELATIVE])('renders %s', async (body) => {
    stubRefusal(400, body)
    useStore.getState().select({ file: { wsId: 'ws1', path: '/etc/passwd' } })

    const { container } = render(<Canvas />)

    expect(await screen.findByText(body)).toBeTruthy()
    expect(container.querySelector('.work-error')?.textContent).not.toBe('error (HTTP 400)')
  })

  // The other side of the precedence rule, asserted where it is visible: a 404 on
  // this route is net/http's own `404 page not found` boilerplate, and the pane
  // keeps saying `not found` instead of putting that on screen.
  it('keeps its own wording for the 404 boilerplate body', async () => {
    stubRefusal(404, '404 page not found')
    useStore.getState().select({ file: { wsId: 'ws1', path: 'gone.txt' } })

    const { container } = render(<Canvas />)

    expect(await screen.findByText('not found')).toBeTruthy()
    expect(container.querySelector('.work-error')?.textContent).not.toBe('404 page not found')
  })

  // A refusal with no body at all still says the status and invents nothing. This
  // held before the fix too — it is the regression pin for the fallback, so that
  // "show the body" cannot quietly become "show an empty error box".
  it('still shows error (HTTP n) when the refusal carried no body', async () => {
    const fetchMock = vi.fn(async (_url: string) => ({
      ok: false,
      status: 500,
      text: async () => '',
      json: async () => ({}),
    }))
    vi.stubGlobal('fetch', fetchMock)
    useStore.getState().select({ file: { wsId: 'ws1', path: 'a.txt' } })

    render(<Canvas />)

    expect(await screen.findByText('error (HTTP 500)')).toBeTruthy()
  })
})
