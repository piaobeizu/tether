// tether#161 — the right-pane Skills tab is a second, independent reader of
// GET /api/v1/skills, and it had its own copy of the `HTTP ${res.status}` throw.
//
// It gets its own case for the reason Settings.test.tsx gives about its three:
// two readers of one endpoint drift, and this repo has the receipts (see
// lib/session.ts's doc). Fixing Settings and leaving this one means the same
// daemon refusal is legible on one screen and a bare status code on the other.
import { afterEach, describe, expect, it, vi } from 'vitest'
import { cleanup, render, screen, waitFor } from '@testing-library/react'
import SkillPane from './index'

afterEach(() => {
  cleanup()
  vi.unstubAllGlobals()
  vi.restoreAllMocks()
})

function mockSkills(res: () => Response) {
  vi.stubGlobal(
    'fetch',
    vi.fn(async (url: string) => {
      if (url === '/api/v1/skills') return res()
      throw new Error(`unexpected fetch: ${url}`)
    }),
  )
}

describe('SkillPane shows what the daemon said (tether#161)', () => {
  it('shows the refusal instead of the status code', async () => {
    // A daemon with no skill registry never registers the route; the request
    // lands on the /api/v1/ stub, which answers `not implemented` in plain text
    // plus the newline http.Error appends.
    mockSkills(() => new Response('not implemented\n', { status: 501 }))
    render(<SkillPane onManage={() => {}} />)

    await waitFor(() => expect(screen.getByText('not implemented')).toBeTruthy())
    // The value the defect produced.
    expect(screen.queryByText('HTTP 501')).toBeNull()
  })

  it('reads the message out of a JSON body', async () => {
    mockSkills(
      () =>
        new Response('{"error":"unauthorized"}\n', {
          status: 401,
          headers: { 'Content-Type': 'application/json' },
        }),
    )
    render(<SkillPane onManage={() => {}} />)

    await waitFor(() => expect(screen.getByText('unauthorized')).toBeTruthy())
    expect(screen.queryByText('HTTP 401')).toBeNull()
  })

  it('falls back to the status when there is no body to show', async () => {
    mockSkills(() => new Response('', { status: 500 }))
    render(<SkillPane onManage={() => {}} />)

    await waitFor(() => expect(screen.getByText('HTTP 500')).toBeTruthy())
  })
})
