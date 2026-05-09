// auth.ts — JWT placeholder per D-16 J2 simplified.
// v0.1 is single-user; auth is a stub that always returns an empty token.
// Real OAuth flow is v1.0+.

export function getAuthToken(): string {
  return ''
}

export function authHeaders(): Record<string, string> {
  const token = getAuthToken()
  return token ? { Authorization: `Bearer ${token}` } : {}
}
