import { useState } from 'react'
import type { CSSProperties, FormEvent } from 'react'
import { verifyToken, INVALID_TOKEN_MESSAGE } from './lib/auth'
import { safeRedirectTarget } from './lib/safeRedirectTarget'

// This page's styling is deliberately provisional and self-contained: literal
// inline colour values, no CSS custom properties, no Tailwind classes, no new
// stylesheet. The shadcn-style token vocabulary vendored under
// web/src/vendor/cloudcli is not wired into this build — web/package.json has
// no tailwindcss/postcss/class-variance-authority, and nothing under
// web/src, web/index.html or web/vite.config.ts imports any stylesheet — so
// referencing those tokens today would fail exactly the way the deleted
// pre-#173 page's `var(--x)` references failed: silently, falling back to UA
// defaults, with nothing erroring and nothing reddening. tether#194 lands the
// toolchain and the vendored primitives; the shell wi that follows restyles
// this page then. Do not extend this styling — replace it.

const NETWORK_ERROR_MESSAGE = 'Network error.'

const pageStyle: CSSProperties = {
  minHeight: '100dvh',
  background: '#f5f5f5',
  padding: '1rem',
}

const panelStyle: CSSProperties = {
  maxWidth: '22rem',
  margin: '0 auto',
  padding: '1.5rem',
  background: '#ffffff',
  border: '1px solid #d1d5db',
  borderRadius: '0.5rem',
}

const formStyle: CSSProperties = {
  display: 'flex',
  flexDirection: 'column',
  gap: '0.75rem',
}

const labelStyle: CSSProperties = {
  color: '#6b7280',
  fontSize: '0.875rem',
}

const inputStyle: CSSProperties = {
  width: '100%',
  boxSizing: 'border-box',
  padding: '0.5rem 0.75rem',
  border: '1px solid #d1d5db',
  borderRadius: '0.375rem',
  color: '#1f2937',
}

const buttonStyle: CSSProperties = {
  padding: '0.5rem 0.75rem',
  background: '#2563eb',
  color: '#ffffff',
  border: 'none',
  borderRadius: '0.375rem',
  cursor: 'pointer',
}

const errorStyle: CSSProperties = {
  color: '#b91c1c',
  margin: 0,
}

export default function AuthPage() {
  const [token, setToken] = useState('')
  const [error, setError] = useState<string | null>(null)
  const [loading, setLoading] = useState(false)

  async function handleSubmit(e: FormEvent<HTMLFormElement>) {
    e.preventDefault()
    setError(null)
    setLoading(true)
    try {
      const ok = await verifyToken(token)
      if (ok) {
        const params = new URLSearchParams(window.location.search)
        window.location.href = safeRedirectTarget(params.get('redirect'), window.location.origin)
      } else {
        setError(INVALID_TOKEN_MESSAGE)
      }
    } catch {
      setError(NETWORK_ERROR_MESSAGE)
    } finally {
      // A rejected request must not wedge the button disabled forever.
      setLoading(false)
    }
  }

  return (
    <div style={pageStyle}>
      <div style={panelStyle}>
        <form style={formStyle} onSubmit={handleSubmit}>
          <label style={labelStyle} htmlFor="tether-access-token">
            Access token
          </label>
          <input
            id="tether-access-token"
            style={inputStyle}
            type="password"
            autoComplete="off"
            value={token}
            onChange={e => setToken(e.target.value)}
          />
          <button style={buttonStyle} type="submit" disabled={loading || !token}>
            {loading ? 'Verifying…' : 'Sign in'}
          </button>
          {error && <p style={errorStyle}>{error}</p>}
        </form>
      </div>
    </div>
  )
}
