// Wire protocol for the token-login flow the daemon already implements.
//
// The authority for everything below is internal/auth/middleware.go, not this
// file: that Go code decides what a valid request looks like and what every
// response means, and it has not changed since `main`. If this file and that
// one ever need to disagree, changing one without the other is a protocol
// break, not a refactor.
//
// tether#174 (tether#173's phase-1 foundation) deleted the pre-existing
// lib/auth.ts along with the rest of the old SPA; this restores it. The wire
// shape below —
// clientId persisted before the request, the token trimmed before it is
// sent, the request body's field names — is copied verbatim from that
// deleted file, not redesigned.

const CLIENT_ID_KEY = 'tether_client_id'

export const INVALID_TOKEN_MESSAGE =
  'Invalid token. Check ~/.tether/access-token on the server.'

/**
 * Verify a pasted access token against the daemon.
 *
 * Persists a stable client id to localStorage BEFORE issuing the network
 * request — the client id has to be stable across a retried request, so it
 * must exist before the request that might need retrying. Resolves to
 * whether the daemon accepted the token. A thrown error (network failure,
 * body serialization) is left to the caller: whether that is presented as
 * something distinct from an outright rejection is a presentation decision,
 * not a protocol one.
 */
export async function verifyToken(rawToken: string): Promise<boolean> {
  const clientId = localStorage.getItem(CLIENT_ID_KEY) ?? crypto.randomUUID()
  localStorage.setItem(CLIENT_ID_KEY, clientId)
  const res = await fetch('/api/v1/auth/verify', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ token: rawToken.trim(), clientId }),
  })
  return res.ok
}
