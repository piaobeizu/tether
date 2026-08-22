// tether#161 — the daemon's refusal has to reach the SKILLS PANEL, which is the
// only thing in Settings that shows one.
//
// Three call sites feed that one `<div>` (loadSkills, install, remove), and each
// of the three used to build its own `HTTP ${res.status}` and throw the body
// away. So each gets its own case: a fix applied to the list load alone leaves a
// user who typed a relative path staring at "HTTP 400" while the daemon has
// already told them, in words, that a skill source must be absolute.
//
// Every case asserts BOTH states. `screen.getByText(<the daemon sentence>)` is
// what the fix produces; `queryByText('HTTP 4xx')` being null is what the defect
// produced — without the second half a case would also pass on a build where the
// sentence appears for some unrelated reason, and without the first half it would
// pass on a build that renders nothing at all.
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react'
import { Settings } from './Settings'
import { resetAppVersionCache } from './lib/version'

/** http.Error writes the message plus exactly one newline. Mirror that. */
const textBody = (body: string, status: number) =>
  new Response(`${body}\n`, { status, headers: { 'Content-Type': 'text/plain; charset=utf-8' } })

const jsonBody = (body: unknown, status: number) =>
  new Response(`${JSON.stringify(body)}\n`, {
    status,
    headers: { 'Content-Type': 'application/json' },
  })

interface Routes {
  skillsGet?: () => Response
  skillsPost?: () => Response
  skillsDelete?: () => Response
}

/**
 * Route by URL + method, and throw on anything unrouted rather than quietly
 * answering it — a panel that grows a fetch this file has not thought about
 * should be loud, the way WorkspacePane.test.tsx's mock is.
 */
function mockDaemon(routes: Routes) {
  vi.stubGlobal(
    'fetch',
    vi.fn(async (url: string, init?: { method?: string }) => {
      const method = init?.method ?? 'GET'
      if (url === '/api/v1/version') return jsonBody({ version: 'v0.0.0-test' }, 200)
      if (url === '/api/v1/providers') return jsonBody({ providers: [] }, 200)
      if (url === '/api/v1/skills' && method === 'GET') {
        return routes.skillsGet ? routes.skillsGet() : jsonBody([], 200)
      }
      if (url === '/api/v1/skills' && method === 'POST') {
        if (!routes.skillsPost) throw new Error('unexpected POST /api/v1/skills')
        return routes.skillsPost()
      }
      if (url.startsWith('/api/v1/skills/') && method === 'DELETE') {
        if (!routes.skillsDelete) throw new Error(`unexpected DELETE ${url}`)
        return routes.skillsDelete()
      }
      throw new Error(`unexpected fetch: ${method} ${url}`)
    }),
  )
}

const INSTALLED = [
  { id: 'sk-1', name: 'polyforge', sourcePath: '/w/skills/polyforge', addedAt: '' },
]

beforeEach(() => {
  resetAppVersionCache()
  localStorage.clear()
})

afterEach(() => {
  cleanup()
  vi.unstubAllGlobals()
  vi.restoreAllMocks()
})

function openSkills() {
  render(<Settings onClose={() => {}} initialTab="skills" />)
}

describe('Settings skills panel shows what the daemon said (tether#161)', () => {
  // ── loadSkills — Settings.tsx GET /api/v1/skills ────────────────────────────
  // 501 rather than 400: a daemon started without a skill registry never
  // registers the route, so GET /api/v1/skills falls through to the /api/v1/
  // stub, which answers `not implemented`. That is the reachable refusal on this
  // route, and before this wi it read as a bare "HTTP 501".
  it('shows the refusal when the skill list cannot be served', async () => {
    mockDaemon({ skillsGet: () => textBody('not implemented', 501) })
    openSkills()

    await waitFor(() => expect(screen.getByText('not implemented')).toBeTruthy())
    // What the defect rendered here instead.
    expect(screen.queryByText('HTTP 501')).toBeNull()
  })

  it('shows the auth refusal from a JSON body, not the JSON', async () => {
    // The one JSON error body reachable on these routes: the auth middleware
    // answers 401 with {"error":"unauthorized"} (internal/auth/middleware.go).
    mockDaemon({ skillsGet: () => jsonBody({ error: 'unauthorized' }, 401) })
    openSkills()

    await waitFor(() => expect(screen.getByText('unauthorized')).toBeTruthy())
    expect(screen.queryByText('HTTP 401')).toBeNull()
    expect(screen.queryByText('{"error":"unauthorized"}')).toBeNull()
  })

  // ── install — Settings.tsx POST /api/v1/skills ──────────────────────────────
  // This is the case the whole wi is named after: tether#159 wrote this sentence
  // precisely so the user would know WHY the path was refused, and the user got
  // "HTTP 400".
  it('shows why an install was refused', async () => {
    mockDaemon({
      skillsGet: () => jsonBody([], 200),
      skillsPost: () => textBody('skill: a skill source must be absolute', 400),
    })
    openSkills()
    await waitFor(() => expect(screen.getByText('Installed · 0')).toBeTruthy())

    fireEvent.change(screen.getByPlaceholderText('source path (required)'), {
      target: { value: 'relative/skills/thing' },
    })
    // By role: "Install" is also the section heading above the form.
    fireEvent.click(screen.getByRole('button', { name: 'Install' }))

    await waitFor(() =>
      expect(screen.getByText('skill: a skill source must be absolute')).toBeTruthy(),
    )
    expect(screen.queryByText('HTTP 400')).toBeNull()
  })

  // ── remove — Settings.tsx DELETE /api/v1/skills/{id} ────────────────────────
  it('shows why a removal was refused', async () => {
    vi.stubGlobal('confirm', vi.fn(() => true))
    mockDaemon({
      skillsGet: () => jsonBody(INSTALLED, 200),
      skillsDelete: () => textBody('skill: skill is not installed', 404),
    })
    openSkills()
    await waitFor(() => expect(screen.getByText('polyforge')).toBeTruthy())

    fireEvent.click(screen.getByText('Remove'))

    await waitFor(() => expect(screen.getByText('skill: skill is not installed')).toBeTruthy())
    expect(screen.queryByText('HTTP 404')).toBeNull()
  })

  it('still falls back to the status when the daemon sends no body at all', async () => {
    // Not a regression to tolerate — a statement that the fallback is intact.
    // An empty non-2xx body has to say SOMETHING, and the status is all there is.
    mockDaemon({ skillsGet: () => new Response('', { status: 503 }) })
    openSkills()

    await waitFor(() => expect(screen.getByText('HTTP 503')).toBeTruthy())
  })
})
