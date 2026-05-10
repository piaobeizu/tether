import { useState } from 'react'

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
        body: JSON.stringify({ token, clientId }),
      })
      if (res.ok) {
        const params = new URLSearchParams(window.location.search)
        const raw = params.get('redirect') ?? '/'
        // Prevent open redirect: only allow same-origin relative paths.
        const safe = raw.startsWith('/') && !raw.startsWith('//') ? raw : '/'
        window.location.href = safe
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
    <div style={{ display: 'flex', justifyContent: 'center', alignItems: 'center', height: '100vh', background: '#111' }}>
      <form onSubmit={submit} style={{ background: '#1a1a1a', padding: 32, borderRadius: 8, minWidth: 320 }}>
        <h2 style={{ color: '#e8e8e8', marginTop: 0 }}>tether</h2>
        <p style={{ color: '#888', fontSize: 13, marginBottom: 16 }}>Enter the access token from the server.</p>
        <input
          type="password"
          value={token}
          onChange={e => setToken(e.target.value)}
          placeholder="Access token"
          autoFocus
          style={{ width: '100%', padding: '8px 10px', background: '#222', border: '1px solid #333', color: '#e8e8e8', borderRadius: 4, boxSizing: 'border-box', fontSize: 14 }}
        />
        {error && <p style={{ color: '#f87171', fontSize: 12, marginTop: 8 }}>{error}</p>}
        <button
          type="submit"
          disabled={loading || !token}
          style={{ marginTop: 16, width: '100%', padding: '8px', background: loading ? '#333' : '#2563eb', color: '#fff', border: 'none', borderRadius: 4, cursor: loading ? 'default' : 'pointer', fontSize: 14 }}
        >
          {loading ? 'Verifying…' : 'Connect'}
        </button>
      </form>
    </div>
  )
}
