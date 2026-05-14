import { useEffect, useState } from 'react'
import { useStore } from './lib/store'
import { Icon } from './lib/icons'

type Tab = 'connection' | 'appearance' | 'about'

interface Props {
  onClose: () => void
}

export function Settings({ onClose }: Props) {
  const [tab, setTab] = useState<Tab>('connection')
  const { connection } = useStore()
  const [isDark, setIsDark] = useState(
    document.documentElement.getAttribute('data-theme') === 'dark'
  )
  const [providers, setProviders] = useState<string[]>([])
  const [defaultProvider, setDefaultProvider] = useState(
    localStorage.getItem('tether_default_provider') ?? 'claude-code'
  )

  useEffect(() => {
    fetch('/api/v1/providers')
      .then(r => r.ok ? r.json() : null)
      .then((d: { providers?: string[] } | null) => {
        if (d?.providers?.length) setProviders(d.providers)
      })
      .catch(() => {})
  }, [])

  const toggleTheme = () => {
    const next = !isDark
    setIsDark(next)
    if (next) {
      document.documentElement.setAttribute('data-theme', 'dark')
      localStorage.setItem('tether_theme', 'dark')
    } else {
      document.documentElement.removeAttribute('data-theme')
      localStorage.setItem('tether_theme', 'light')
    }
  }

  const setProvider = (p: string) => {
    setDefaultProvider(p)
    localStorage.setItem('tether_default_provider', p)
    window.dispatchEvent(new CustomEvent('tether:provider-changed', { detail: p }))
  }

  const TABS: { id: Tab; label: string }[] = [
    { id: 'connection', label: 'Connection' },
    { id: 'appearance', label: 'Appearance' },
    { id: 'about',      label: 'About' },
  ]

  return (
    <div className="settings-overlay" onClick={onClose}>
      <div className="settings-panel" onClick={e => e.stopPropagation()}>

        {/* Header */}
        <div className="settings-header">
          <button className="icon-btn" onClick={onClose}>
            <Icon name="x" size={15} />
          </button>
          <span style={{ fontWeight: 600, fontSize: 14 }}>Settings</span>
          <span className="pill" style={{ marginLeft: 'auto', fontSize: 10 }}>v0.4.0</span>
        </div>

        {/* Tab nav */}
        <div className="settings-tabs">
          {TABS.map(t => (
            <button
              key={t.id}
              className={`settings-tab${tab === t.id ? ' on' : ''}`}
              onClick={() => setTab(t.id)}
            >{t.label}</button>
          ))}
        </div>

        {/* Content */}
        <div className="settings-body scroll-thin">

          {tab === 'connection' && (
            <>
              <div className="set-section">Connection</div>
              <div className="set-row">
                <span className="set-label">Status</span>
                <span className={`set-value${connection.state === 'live' ? ' success' : ' warn'}`}>
                  {connection.state}
                </span>
              </div>
              <div className="set-row">
                <span className="set-label">Server</span>
                <span className="set-value mono">{location.host}</span>
              </div>
              <div className="set-row">
                <span className="set-label">Protocol</span>
                <span className="set-value mono">WebTransport / HTTP3</span>
              </div>
              {connection.state !== 'live' && (
                <div className="set-row">
                  <span className="set-label">Attempts</span>
                  <span className="set-value mono">{connection.attempt}</span>
                </div>
              )}

              {providers.length > 0 && (
                <>
                  <div className="set-section" style={{ marginTop: 24 }}>Providers</div>
                  {providers.map(p => (
                    <div key={p} className="set-row" style={{ cursor: 'pointer' }} onClick={() => setProvider(p)}>
                      <span className="set-label">{p}</span>
                      <span className="set-value">
                        {p === defaultProvider
                          ? <span style={{ color: 'var(--success)', fontSize: 11 }}>✓ default</span>
                          : <span style={{ color: 'var(--ink-quat)', fontSize: 11 }}>set default</span>}
                      </span>
                    </div>
                  ))}
                </>
              )}
            </>
          )}

          {tab === 'appearance' && (
            <>
              <div className="set-section">Theme</div>
              <div className="set-row">
                <span className="set-label">Dark mode</span>
                <button
                  className={`set-toggle${isDark ? ' on' : ''}`}
                  onClick={toggleTheme}
                >
                  <span className="set-toggle-knob" />
                </button>
              </div>
              <div className="set-hint">Also toggle with <span className="kbd">⌘⇧D</span></div>

              <div className="set-section" style={{ marginTop: 24 }}>Columns</div>
              <div className="set-row">
                <span className="set-label">Reset widths</span>
                <button
                  className="btn-ghost-sm"
                  onClick={() => {
                    localStorage.removeItem('tether_col_left')
                    localStorage.removeItem('tether_col_right')
                    location.reload()
                  }}
                >Reset</button>
              </div>
            </>
          )}

          {tab === 'about' && (
            <>
              <div className="set-section">tether</div>
              <div className="set-row">
                <span className="set-label">Version</span>
                <span className="set-value mono">v0.4.0</span>
              </div>
              <div className="set-row">
                <span className="set-label">Platform</span>
                <span className="set-value mono">Web / HTTP3</span>
              </div>
              <div className="set-row">
                <span className="set-label">Source</span>
                <a
                  href="https://github.com/piaobeizu/tether"
                  target="_blank"
                  rel="noreferrer"
                  className="set-value mono"
                  style={{ color: 'var(--accent)' }}
                >github.com/piaobeizu/tether</a>
              </div>

              <div className="set-section" style={{ marginTop: 24 }}>Session</div>
              <div className="set-row">
                <span className="set-label">Sign out</span>
                <button
                  className="btn-ghost-sm"
                  style={{ color: 'var(--danger)' }}
                  onClick={() => { location.href = '/api/v1/auth/logout' }}
                >Sign out</button>
              </div>
            </>
          )}
        </div>
      </div>
    </div>
  )
}
