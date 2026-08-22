import { useState } from 'react'

/**
 * Resolve the post-login `?redirect=` target to a same-origin path, or `/`.
 *
 * The producer is real and must keep working: `redirectToAuth()` in lib/auth.ts
 * puts the current path + query here on a 401 so the owner lands back where they
 * were. (A review pass called this parameter unused — it had grepped only the Go
 * side.)
 *
 * The previous guard was `raw.startsWith('/') && !raw.startsWith('//')`, and it
 * did not hold: WHATWG URL parsing treats `\` as `/` for special schemes, so
 * `/\evil.example` resolves to `https://evil.example/`. Verified in headless
 * Chrome against that exact code — `?redirect=//host` was contained, and
 * `?redirect=/\/host` navigated off-site. `/<TAB>/evil.example` slips through
 * the same way, because tabs are stripped before parsing.
 *
 * So the check is not "does this string look relative" but "where does this
 * actually resolve to", answered by the same parser the browser will use for the
 * navigation.
 *
 * `url.origin === origin` is necessary but NOT sufficient, which is the trap the
 * first attempt at this fix fell into: `url.pathname` is not guaranteed to be a
 * same-origin path. `..` segments collapse during parsing, so
 * `/..//evil.example` yields origin=<ours> with pathname=`//evil.example` — and
 * THAT string, assigned to location.href, is protocol-relative and leaves the
 * site. `/./\evil.example` and `https://<our-host>//evil.example` do the same.
 *
 * So this validates and otherwise bails, rather than trying to repair the input.
 * The guard has been wrong twice by being clever about transforming a string;
 * every branch below is a refusal, and `/` is always a safe answer. The
 * property — that no input can produce an off-origin return value — is asserted
 * over a generated corpus in AuthPage.test.tsx rather than argued here.
 */
export function safeRedirectTarget(raw: string | null, origin: string): string {
  if (!raw) return '/'
  try {
    const url = new URL(raw, origin)
    // Covers off-origin hosts, scheme changes, and opaque schemes such as
    // javascript: and data:, whose origin is the string "null".
    if (url.origin !== origin) return '/'
    const target = url.pathname + url.search + url.hash
    // A path that does not start with a slash is not a path at all (a same-origin
    // blob: URL puts its whole inner URL in pathname); one that starts with two
    // is an authority. Anything else cannot be re-read as an origin, and neither
    // a query nor a fragment can introduce one after a path has begun.
    if (!target.startsWith('/') || target.startsWith('//')) return '/'
    return target
  } catch {
    return '/'
  }
}

export default function AuthPage() {
  const [token, setToken] = useState('')
  const [error, setError] = useState('')
  const [loading, setLoading] = useState(false)

  const submit = async (e: React.FormEvent) => {
    e.preventDefault()
    setLoading(true)
    setError('')
    // Re-use an existing clientId across retries for durable identity.
    // Generated once per browser, persisted before the network call.
    const clientId = localStorage.getItem('tether_client_id') ?? crypto.randomUUID()
    localStorage.setItem('tether_client_id', clientId)
    try {
      const res = await fetch('/api/v1/auth/verify', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ token: token.trim(), clientId }),
      })
      if (res.ok) {
        const params = new URLSearchParams(window.location.search)
        window.location.href = safeRedirectTarget(params.get('redirect'), window.location.origin)
      } else {
        setError('Invalid token. Check ~/.tether/access-token on the server.')
      }
    } catch {
      setError('Network error.')
    } finally {
      setLoading(false)
    }
  }

  return (
    <div style={{ display: 'flex', justifyContent: 'center', alignItems: 'center', height: '100vh', background: 'var(--bg-sunken)' }}>
      <form onSubmit={submit} style={{ background: 'var(--bg-surface)', padding: 32, borderRadius: 8, minWidth: 320, border: '1px solid var(--line)', boxShadow: 'var(--sh-3)' }}>
        <h2 style={{ color: 'var(--ink-primary)', marginTop: 0 }}>tether</h2>
        <p style={{ color: 'var(--ink-tertiary)', fontSize: 13, marginBottom: 16 }}>Enter the access token from the server.</p>
        <input
          type="password"
          value={token}
          onChange={e => setToken(e.target.value)}
          placeholder="Access token"
          autoFocus
          style={{ width: '100%', padding: '8px 10px', background: 'var(--bg-elevated)', border: '1px solid var(--line)', color: 'var(--ink-primary)', borderRadius: 4, boxSizing: 'border-box', fontSize: 14 }}
        />
        {error && <p style={{ color: 'var(--danger)', fontSize: 12, marginTop: 8 }}>{error}</p>}
        <button
          type="submit"
          disabled={loading || !token}
          style={{ marginTop: 16, width: '100%', padding: '8px', background: loading ? 'var(--bg-tint)' : 'var(--accent)', color: '#fff', border: 'none', borderRadius: 4, cursor: loading ? 'default' : 'pointer', fontSize: 14 }}
        >
          {loading ? 'Verifying…' : 'Connect'}
        </button>
      </form>
    </div>
  )
}
