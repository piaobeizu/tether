import { useEffect, useRef, useState } from 'react'
import { useStore } from './lib/store'
import { Icon } from './lib/icons'

// Fired after a skill is installed/removed so other views (right-pane
// SkillPane) can refetch. Mirrors the existing tether:provider-changed event.
const SKILLS_CHANGED = 'tether:skills-changed'

export type SettingsTab = 'account' | 'skills' | 'appearance' | 'connection' | 'about'

const APP_VERSION = 'v0.5.0'

interface Skill {
  id: string
  name: string
  description?: string
  sourcePath: string
  addedAt: string
}

interface Props {
  onClose: () => void
  initialTab?: SettingsTab
}

export function Settings({ onClose, initialTab = 'connection' }: Props) {
  const [tab, setTab] = useState<SettingsTab>(initialTab)
  const { connection } = useStore()
  const [isDark, setIsDark] = useState(
    document.documentElement.getAttribute('data-theme') === 'dark'
  )
  const [providers, setProviders] = useState<string[]>([])
  const [defaultProvider, setDefaultProvider] = useState(
    localStorage.getItem('tether_default_provider') ?? 'claude-code'
  )

  // Skills are managed here (moved out of the right-pane Skills tab).
  const [skills, setSkills] = useState<Skill[]>([])
  const [skillErr, setSkillErr] = useState<string | null>(null)
  const [newName, setNewName] = useState('')
  const [newPath, setNewPath] = useState('')
  const [installing, setInstalling] = useState(false)

  // Guards against setState after the overlay is closed mid-request.
  const mounted = useRef(true)

  const loadSkills = async () => {
    try {
      const res = await fetch('/api/v1/skills')
      if (!res.ok) throw new Error(`HTTP ${res.status}`)
      const list = await res.json()
      if (!mounted.current) return
      setSkills(list)
      setSkillErr(null)
    } catch (e) {
      if (mounted.current) setSkillErr(e instanceof Error ? e.message : String(e))
    }
  }

  useEffect(() => {
    fetch('/api/v1/providers')
      .then(r => r.ok ? r.json() : null)
      .then((d: { providers?: string[] } | null) => {
        if (mounted.current && d?.providers?.length) setProviders(d.providers)
      })
      .catch(() => {})
    void loadSkills()
    return () => { mounted.current = false }
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

  const install = async () => {
    const sourcePath = newPath.trim()
    if (!sourcePath) return
    const name = newName.trim() || (sourcePath.split('/').pop() ?? sourcePath)
    setInstalling(true)
    try {
      const res = await fetch('/api/v1/skills', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ name, sourcePath }),
      })
      if (!res.ok) throw new Error(`HTTP ${res.status}`)
      window.dispatchEvent(new Event(SKILLS_CHANGED))
      if (mounted.current) {
        setNewName('')
        setNewPath('')
      }
      await loadSkills()
    } catch (e) {
      if (mounted.current) setSkillErr(e instanceof Error ? e.message : String(e))
    } finally {
      if (mounted.current) setInstalling(false)
    }
  }

  const remove = async (id: string, name: string) => {
    if (!confirm(`Remove skill "${name}"?`)) return
    try {
      const res = await fetch(`/api/v1/skills/${id}`, { method: 'DELETE' })
      if (!res.ok && res.status !== 204) throw new Error(`HTTP ${res.status}`)
      window.dispatchEvent(new Event(SKILLS_CHANGED))
      await loadSkills()
    } catch (e) {
      if (mounted.current) setSkillErr(e instanceof Error ? e.message : String(e))
    }
  }

  const NAV: { id: SettingsTab; label: string; sub: string }[] = [
    { id: 'account',    label: 'account',    sub: 'session' },
    { id: 'skills',     label: 'skills',     sub: `${skills.length} installed` },
    { id: 'appearance', label: 'appearance', sub: isDark ? 'dark' : 'light' },
    { id: 'connection', label: 'connection', sub: connection.state },
    { id: 'about',      label: 'about',      sub: APP_VERSION },
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
          <span className="pill" style={{ marginLeft: 'auto', fontSize: 10 }}>{APP_VERSION}</span>
        </div>

        <div className="settings-grid">
          {/* Left sub-page nav */}
          <nav className="settings-nav">
            {NAV.map(n => (
              <button
                key={n.id}
                className={`settings-nav-btn${tab === n.id ? ' on' : ''}`}
                onClick={() => setTab(n.id)}
              >
                <span className="settings-nav-name">{n.label}</span>
                <span className="settings-nav-sub mono">{n.sub}</span>
              </button>
            ))}
          </nav>

          {/* Content */}
          <div className="settings-body scroll-thin">

            {tab === 'account' && (
              <>
                <div className="set-section">Session</div>
                <div className="set-row">
                  <span className="set-label">Signed in</span>
                  <span className="set-value">browser session</span>
                </div>
                <div className="set-row">
                  <span className="set-label">Sign out</span>
                  <button
                    className="btn-ghost-sm"
                    style={{ color: 'var(--danger)' }}
                    onClick={() => { location.href = '/api/v1/auth/logout' }}
                  >Sign out</button>
                </div>
                <div className="set-hint">
                  Access is gated by the daemon token; token rotation is managed by the daemon.
                </div>
              </>
            )}

            {tab === 'skills' && (
              <>
                <div className="set-section">Install</div>
                <div className="skill-install">
                  <input
                    className="skill-input"
                    placeholder="source path (required)"
                    value={newPath}
                    onChange={e => setNewPath(e.target.value)}
                    onKeyDown={e => { if (e.key === 'Enter') void install() }}
                  />
                  <input
                    className="skill-input"
                    placeholder="name (optional)"
                    value={newName}
                    onChange={e => setNewName(e.target.value)}
                    onKeyDown={e => { if (e.key === 'Enter') void install() }}
                  />
                  <button
                    className="btn-primary-sm"
                    disabled={installing || !newPath.trim()}
                    onClick={() => void install()}
                  >{installing ? '…' : 'Install'}</button>
                </div>

                <div className="set-section" style={{ marginTop: 24 }}>
                  Installed · {skills.length}
                </div>
                {skillErr && (
                  <div style={{ color: 'var(--danger)', fontSize: 11, marginBottom: 8 }}>{skillErr}</div>
                )}
                {skills.length === 0 && (
                  <div style={{ color: 'var(--ink-tertiary)', fontSize: 12 }}>No skills installed.</div>
                )}
                {skills.map(sk => (
                  <div key={sk.id} className="set-row">
                    <div style={{ minWidth: 0 }}>
                      <div className="set-label" style={{ fontWeight: 500 }}>{sk.name}</div>
                      {sk.description && (
                        <div style={{ fontSize: 11, color: 'var(--ink-tertiary)' }}>{sk.description}</div>
                      )}
                      <div style={{ fontSize: 10, color: 'var(--ink-quat)', fontFamily: 'var(--font-mono)', wordBreak: 'break-all' }}>{sk.sourcePath}</div>
                    </div>
                    <button
                      className="btn-ghost-sm"
                      style={{ color: 'var(--danger)', flexShrink: 0 }}
                      onClick={() => void remove(sk.id, sk.name)}
                    >Remove</button>
                  </div>
                ))}
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
                  <span className="set-label">Latency</span>
                  <span className="set-value mono">{connection.latency ? `${connection.latency}ms` : '–'}</span>
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

            {tab === 'about' && (
              <>
                <div className="set-section">tether</div>
                <div className="set-row">
                  <span className="set-label">Version</span>
                  <span className="set-value mono">{APP_VERSION}</span>
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
              </>
            )}
          </div>
        </div>
      </div>
    </div>
  )
}
