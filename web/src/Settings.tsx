import { useEffect, useRef, useState, useSyncExternalStore } from 'react'
import { useStore } from './lib/store'
import { Icon } from './lib/icons'
import { useAppVersion } from './lib/version'

// Fired after a skill is installed/removed so other views (right-pane
// SkillPane) can refetch. Mirrors the existing tether:provider-changed event.
const SKILLS_CHANGED = 'tether:skills-changed'

export type SettingsTab = 'account' | 'skills' | 'appearance' | 'connection' | 'about'

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

// ── The theme (tether#129) ───────────────────────────────────────────────────
//
// `data-theme` on <html> IS the theme. Not a cache of it — index.css keys every
// dark-mode rule off that attribute, so whatever it says is what the user sees,
// by construction. localStorage's `tether_theme` is only the persistence of it,
// read once by main.tsx before React mounts.
//
// This component used to keep a COPY:
//
//   const [isDark, setIsDark] = useState(
//     document.documentElement.getAttribute('data-theme') === 'dark')
//
// which is a useState INITIALIZER — sampled on open and never re-read. The other
// writer is main.tsx's document-level ⌘⇧D / Ctrl+Shift+D handler, which sets the
// attribute directly and knows nothing about this panel. So pressing the shortcut
// with Settings open really did flip the theme while this copy went stale: the
// appearance row's sub-label read the opposite of the screen, and `toggleTheme`
// computed `!isDark` from the stale value, so the switch could ask for the theme
// that was already in effect and appear to do nothing.
//
// The fix reads the attribute instead of copying it, and subscribes to it so
// React re-renders when it moves. Two alternatives were considered:
//
//   · A tether:theme-changed event both writers dispatch. That is the existing
//     house pattern (tether:provider-changed, tether:skills-changed) and it would
//     work today — but it is a notification channel every writer has to remember
//     to use, so the next one to set the attribute puts this bug straight back.
//     Observing the ATTRIBUTE observes the effect rather than trusting the cause,
//     which covers writers that have never heard of this file.
//   · Promoting the theme into the zustand store as the "single source of truth".
//     It would not be one: main.tsx must keep setting the attribute BEFORE first
//     paint (see its `savedTheme` block) or the app flashes light on every dark
//     load, so a store field would be a third artifact to keep in step with the
//     attribute rather than a replacement for it — the same defect one layer up.
//
// Polling was not considered.
const THEME_ATTR = 'data-theme'

function subscribeToTheme(onStoreChange: () => void): () => void {
  const observer = new MutationObserver(onStoreChange)
  observer.observe(document.documentElement, { attributes: true, attributeFilter: [THEME_ATTR] })
  return () => observer.disconnect()
}

/** Whether dark mode is on, right now, according to the document itself. */
function isDarkNow(): boolean {
  return document.documentElement.getAttribute(THEME_ATTR) === 'dark'
}

/**
 * Dark mode, as a value that follows the document.
 *
 * Exported so a second reader of the theme subscribes rather than sampling. A
 * boolean snapshot is safe to return from getSnapshot without memoisation —
 * useSyncExternalStore compares with Object.is, and two `false`s are the same
 * value where two freshly-built objects would not be.
 */
export function useIsDark(): boolean {
  return useSyncExternalStore(subscribeToTheme, isDarkNow)
}

/** Write the theme. The only writer besides main.tsx's keyboard shortcut. */
function applyTheme(dark: boolean): void {
  if (dark) {
    document.documentElement.setAttribute(THEME_ATTR, 'dark')
    localStorage.setItem('tether_theme', 'dark')
  } else {
    document.documentElement.removeAttribute(THEME_ATTR)
    localStorage.setItem('tether_theme', 'light')
  }
}

export function Settings({ onClose, initialTab = 'connection' }: Props) {
  const [tab, setTab] = useState<SettingsTab>(initialTab)
  const { connection } = useStore()
  const appVersion = useAppVersion()
  const isDark = useIsDark()
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
  const panelRef = useRef<HTMLDivElement>(null)
  // Latest onClose without re-running the mount-only focus/Esc effect.
  const onCloseRef = useRef(onClose)
  onCloseRef.current = onClose

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

  // Esc closes; focus moves into the panel on open and back to the opener on close.
  // Mount/unmount only — Settings mounts when opened and unmounts when closed, so we
  // must NOT depend on onClose (an inline arrow from App that changes every render,
  // which would re-run this effect and yank focus back to the panel mid-typing).
  useEffect(() => {
    const opener = document.activeElement as HTMLElement | null
    const onKey = (e: KeyboardEvent) => { if (e.key === 'Escape') onCloseRef.current() }
    document.addEventListener('keydown', onKey)
    panelRef.current?.focus()
    return () => { document.removeEventListener('keydown', onKey); opener?.focus?.() }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [])

  // No setState: `isDark` is derived from the attribute applyTheme writes, so the
  // MutationObserver behind useIsDark is what re-renders this panel. That the
  // writer no longer has to also tell React is the point — a writer that forgot
  // was the whole defect. It also makes `!isDark` correct again: it is now
  // negating what the document says rather than a snapshot from open time.
  const toggleTheme = () => applyTheme(!isDark)

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
    { id: 'about',      label: 'about',      sub: appVersion },
  ]

  return (
    <div className="settings-overlay" onClick={onClose}>
      <div
        className="settings-panel"
        ref={panelRef}
        tabIndex={-1}
        role="dialog"
        aria-modal="true"
        aria-label="Settings"
        onClick={e => e.stopPropagation()}
      >

        {/* Header */}
        <div className="settings-header">
          <button className="icon-btn" onClick={onClose}>
            <Icon name="x" size={15} />
          </button>
          <span style={{ fontWeight: 600, fontSize: 14 }}>Settings</span>
          <span className="pill" style={{ marginLeft: 'auto', fontSize: 10 }}>{appVersion}</span>
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
                  <span className="set-value mono">{appVersion}</span>
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
