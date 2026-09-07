import { describe, it, expect, beforeEach, afterEach, vi } from 'vitest'
import { render, screen, fireEvent, waitFor, cleanup } from '@testing-library/react'
import AuthPage from './AuthPage'
import { safeRedirectTarget } from './lib/safeRedirectTarget'

// safeRedirectTarget already has its own security-property tests
// (safeRedirectTarget.test.ts, oauthSignInRedirect.test.ts); this file tests
// the wiring hop into AuthPage, not the guard's own behaviour, so it is
// mocked rather than exercised for real.
vi.mock('./lib/safeRedirectTarget', () => ({
  safeRedirectTarget: vi.fn(() => '/mock-target'),
}))

const realFetch = globalThis.fetch
const originalLocation = window.location

function mockFetch(impl: () => Promise<Response> | Response) {
  globalThis.fetch = vi.fn(() => Promise.resolve(impl())) as unknown as typeof fetch
}

const jsonResponse = (body: unknown, status = 200) =>
  new Response(JSON.stringify(body), { status, headers: { 'Content-Type': 'application/json' } })

function submitWithToken(value: string) {
  fireEvent.change(screen.getByLabelText(/access token/i), { target: { value } })
  fireEvent.click(screen.getByRole('button', { name: /sign in/i }))
}

describe('AuthPage', () => {
  beforeEach(() => {
    Object.defineProperty(window, 'location', {
      value: { ...originalLocation, href: '', search: '?redirect=/somewhere', origin: originalLocation.origin },
      writable: true,
    })
  })

  afterEach(() => {
    cleanup()
    globalThis.fetch = realFetch
    vi.mocked(safeRedirectTarget).mockClear()
    Object.defineProperty(window, 'location', { value: originalLocation, writable: true })
  })

  it('renders a token input and a submit control', () => {
    render(<AuthPage />)
    expect(screen.getByLabelText(/access token/i)).not.toBeNull()
    expect(screen.getByRole('button', { name: /sign in/i })).not.toBeNull()
  })

  it('disables the submit button while the token field is empty', () => {
    render(<AuthPage />)
    const button = screen.getByRole('button', { name: /sign in/i }) as HTMLButtonElement
    expect(button.disabled).toBe(true)
    fireEvent.change(screen.getByLabelText(/access token/i), { target: { value: 'abc' } })
    expect(button.disabled).toBe(false)
  })

  it('disables the submit button and shows an in-flight label while the request is pending, then re-enables after it settles', async () => {
    let resolveFetch: (res: Response) => void = () => {}
    globalThis.fetch = vi.fn(
      () => new Promise<Response>(resolve => { resolveFetch = resolve })
    ) as unknown as typeof fetch
    render(<AuthPage />)
    submitWithToken('sometoken')

    await waitFor(() => {
      expect(screen.getByRole('button', { name: /verifying/i })).not.toBeNull()
    })
    expect((screen.getByRole('button', { name: /verifying/i }) as HTMLButtonElement).disabled).toBe(true)

    resolveFetch(jsonResponse({ status: 'ok' }))

    await waitFor(() => {
      expect(screen.getByRole('button', { name: /sign in/i })).not.toBeNull()
    })
    expect((screen.getByRole('button', { name: /sign in/i }) as HTMLButtonElement).disabled).toBe(false)
  })

  it('re-enables the submit button after a network error instead of staying wedged', async () => {
    globalThis.fetch = vi.fn(() => Promise.reject(new Error('offline'))) as unknown as typeof fetch
    render(<AuthPage />)
    submitWithToken('anything')
    await waitFor(() => {
      expect(screen.getByText('Network error.')).not.toBeNull()
    })
    expect((screen.getByRole('button', { name: /sign in/i }) as HTMLButtonElement).disabled).toBe(false)
  })

  it('shows the exact invalid-token message when the daemon rejects the token', async () => {
    mockFetch(() => jsonResponse({ error: 'unauthorized' }, 401))
    render(<AuthPage />)
    submitWithToken('bad-token')
    await waitFor(() => {
      expect(screen.getByText('Invalid token. Check ~/.tether/access-token on the server.')).not.toBeNull()
    })
  })

  it('shows a distinct message when the request fails outright (network error)', async () => {
    globalThis.fetch = vi.fn(() => Promise.reject(new Error('offline'))) as unknown as typeof fetch
    render(<AuthPage />)
    submitWithToken('anything')
    await waitFor(() => {
      expect(screen.getByText('Network error.')).not.toBeNull()
    })
  })

  it('redirects to the safeRedirectTarget-resolved target when the token is accepted', async () => {
    mockFetch(() => jsonResponse({ status: 'ok' }))
    render(<AuthPage />)
    submitWithToken('good-token')
    await waitFor(() => {
      expect(window.location.href).toBe('/mock-target')
    })
    expect(safeRedirectTarget).toHaveBeenCalledWith('/somewhere', originalLocation.origin)
  })
})
