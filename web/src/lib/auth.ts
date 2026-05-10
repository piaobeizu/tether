// auth.ts — v0.2 static token gate (D-16 simplified).
// Cookie is set server-side after /api/v1/auth/verify succeeds.
// On 401, redirect to /auth with the current path as ?redirect=.

export function redirectToAuth(): void {
  const redirect = encodeURIComponent(window.location.pathname + window.location.search)
  window.location.href = `/auth?redirect=${redirect}`
}

// Wraps fetch; redirects to /auth on 401.
export async function authedFetch(input: RequestInfo, init?: RequestInit): Promise<Response> {
  const res = await fetch(input, init)
  if (res.status === 401) {
    redirectToAuth()
    throw new Error('unauthenticated')
  }
  return res
}

// Returns the stored client UUID (set during auth verify, used in WT connections).
export function getClientId(): string {
  return localStorage.getItem('tether_client_id') ?? ''
}
