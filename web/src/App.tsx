import { useEffect, useRef, useState } from 'react'
import { useStore } from './lib/store'
import { Icon } from './lib/icons'
import { Settings, type SettingsTab } from './Settings'
import { APP_VERSION } from './lib/version'
import WorkspacePane from './panes/workspace'
import SkillPane from './panes/skill'
import ShellPane from './panes/shell'
import ChatPane from './panes/chat'

type RightTab = 'chat' | 'skill' | 'shell'

const STORAGE_KEY_LEFT  = 'tether_col_left'
const STORAGE_KEY_RIGHT = 'tether_col_right'
const MIN_LEFT  = 160
const MAX_LEFT  = 480
const MIN_RIGHT = 260
const MAX_RIGHT = 600

function loadWidth(key: string, fallback: number): number {
  const v = localStorage.getItem(key)
  return v ? Number(v) : fallback
}

/** Drag handle between two columns. Calls onDelta(dx) on each mousemove frame. */
function ColResizer({ onDelta }: { onDelta: (dx: number) => void }) {
  const dragging = useRef(false)

  const onMouseDown = (e: React.MouseEvent) => {
    e.preventDefault()
    dragging.current = true
    document.body.style.cursor = 'col-resize'
    document.body.style.userSelect = 'none'
    let lastX = e.clientX

    const onMove = (ev: MouseEvent) => {
      if (!dragging.current) return
      onDelta(ev.clientX - lastX)
      lastX = ev.clientX
    }
    const onUp = () => {
      dragging.current = false
      document.body.style.cursor = ''
      document.body.style.userSelect = ''
      document.removeEventListener('mousemove', onMove)
      document.removeEventListener('mouseup', onUp)
    }
    document.addEventListener('mousemove', onMove)
    document.addEventListener('mouseup', onUp)
  }

  return <div className="col-resizer" onMouseDown={onMouseDown} />
}

export default function App() {
  const [rightTab, setRightTab] = useState<RightTab>('chat')
  // Keep panes mounted after first visit so switching tabs doesn't tear down the
  // PTY (Shell) or refetch (Skills). Chat is always mounted.
  const [visitedTabs, setVisitedTabs] = useState<Record<RightTab, boolean>>({ chat: true, skill: false, shell: false })
  const selectTab = (t: RightTab) => {
    setRightTab(t)
    setVisitedTabs(v => (v[t] ? v : { ...v, [t]: true }))
    // xterm measures 0 while display:none; nudge a refit once Shell is visible again.
    if (t === 'shell') requestAnimationFrame(() => window.dispatchEvent(new Event('resize')))
  }
  const [drawerOpen, setDrawerOpen] = useState(false)
  const [settingsTab, setSettingsTab] = useState<SettingsTab | null>(null)
  const [leftW,  setLeftW]  = useState(() => loadWidth(STORAGE_KEY_LEFT,  240))
  const [rightW, setRightW] = useState(() => loadWidth(STORAGE_KEY_RIGHT, 380))
  // Local-only UI dismissals; reset whenever the underlying connection state
  // changes so a fresh failure/reconnect re-surfaces the affordance.
  const [bannerDismissed, setBannerDismissed] = useState(false)
  const [modalDismissed, setModalDismissed] = useState(false)
  const { connection, sessionId } = useStore()

  useEffect(() => {
    setBannerDismissed(false)
    setModalDismissed(false)
  }, [connection.state])

  const connPillClass =
    connection.state === 'live' ? 'live' :
    connection.state === 'reconnecting' ? 'warn' : ''

  const connLabel =
    connection.state === 'live' ? 'daemon · live' :
    connection.state === 'reconnecting' ? `reconnecting · attempt ${connection.attempt}` :
    connection.state === 'connecting' ? 'connecting…' :
    'dropped'

  // Ask ChatPane (owner of the WT connection) to retry immediately.
  const retryConnection = () => window.dispatchEvent(new CustomEvent('tether:retry-connection'))

  const showBanner = !bannerDismissed && (
    connection.state === 'reconnecting' ||
    (connection.state === 'dropped' && sessionId !== null))

  // Catch-up-failed = a resumable session that the daemon could not re-attach
  // after exhausting reconnect attempts (ChatPane sets state → 'dropped').
  const showCatchupFailed = !modalDismissed &&
    connection.state === 'dropped' && sessionId !== null

  // WT transport is actively re-establishing.
  const showWtPill = connection.state === 'reconnecting'

  // "new session" — drop the resumable session id and reload so ChatPane
  // opens a fresh WT session instead of trying to catch up the dead one.
  const startNewSession = () => {
    localStorage.removeItem('tether_last_sid')
    location.reload()
  }

  const resizeLeft = (dx: number) => {
    setLeftW(w => {
      const next = Math.max(MIN_LEFT, Math.min(MAX_LEFT, w + dx))
      localStorage.setItem(STORAGE_KEY_LEFT, String(next))
      return next
    })
  }
  const resizeRight = (dx: number) => {
    setRightW(w => {
      const next = Math.max(MIN_RIGHT, Math.min(MAX_RIGHT, w - dx))
      localStorage.setItem(STORAGE_KEY_RIGHT, String(next))
      return next
    })
  }

  return (
    <div className="dt-root">

      {/* ── Titlebar ──────────────────────────────────────────── */}
      <div className="dt-titlebar" style={{ gridRow: 1 }}>
        <div className="dt-traffic">
          <span style={{ background: '#E27A6F' }} />
          <span style={{ background: '#E5C36A' }} />
          <span style={{ background: '#7DB87E' }} />
        </div>
        <div className="dt-tabs">
          <span className="dt-tab on">
            <Icon name="tether" size={12} style={{ color: 'var(--accent)' }} />
            tether
          </span>
        </div>
        <div className="dt-titlebar-right">
          <span className={`pill ${connPillClass}`}>
            <span className="dot" />
            {connLabel}
          </span>
          <button className="icon-btn" title="Settings" aria-label="Settings" onClick={() => setSettingsTab('connection')}>
            <Icon name="settings" size={14} />
          </button>
        </div>
      </div>

      {/* ── Error banner ──────────────────────────────────────── */}
      {showBanner && (
        <div className="dt-error-banner" style={{ gridRow: 2 }}>
          <span className="dt-error-pulse" />
          <span style={{ fontWeight: 600 }}>daemon unreachable</span>
          <span style={{ color: 'var(--ink-secondary)' }}>
            {connection.state === 'reconnecting'
              ? `retrying · attempt ${connection.attempt}…`
              : 'check connection'}
          </span>
          <button className="btn-ghost-sm dt-error-retry" onClick={retryConnection}>
            retry now
          </button>
          <button
            className="icon-btn-sm"
            title="Dismiss"
            aria-label="Dismiss banner"
            onClick={() => setBannerDismissed(true)}
          >
            <Icon name="x" size={13} />
          </button>
        </div>
      )}

      {/* ── Main columns (flex, resizable) ────────────────────── */}
      <div className="dt-grid" style={{ gridRow: 3 }}>

        {/* Left: Workspace tree */}
        <aside className="dt-left" style={{ width: leftW }}>
          <WorkspacePane />
        </aside>

        <ColResizer onDelta={resizeLeft} />

        {/* Middle: Workspace artifact display */}
        <main className="dt-mid">
          <div className="dt-mid-head">
            <div className="dt-breadcrumb">
              <span className="mono crumb-faint">tether</span>
              <span className="mono crumb-sep">/</span>
              <span className="mono crumb">workspace</span>
              {sessionId && (
                <span className="pill" style={{ marginLeft: 10 }}>
                  <span className="dot live" />
                  {sessionId.slice(0, 8)}
                </span>
              )}
            </div>
          </div>
          <div className="dt-mid-body scroll-thin">
            <div className="dt-mid-empty">
              <Icon name="folder-open" size={26} />
              <div className="serif dt-mid-empty-title">no artifacts yet</div>
              <div className="dt-mid-empty-sub">
                Files, diffs, and skill output from this workspace show up here as tether works.
              </div>
            </div>
          </div>
        </main>

        <ColResizer onDelta={resizeRight} />

        {/* Right: Chat / Skills / Shell tabs */}
        <section className="dt-right" style={{ width: rightW }}>
          <div className="dt-right-tabs">
            {(['chat', 'skill', 'shell'] as RightTab[]).map(t => (
              <button
                key={t}
                className={`dt-right-tab${rightTab === t ? ' on' : ''}`}
                onClick={() => selectTab(t)}
              >
                {t === 'chat' ? 'Chat' : t === 'skill' ? 'Skills' : 'Shell'}
              </button>
            ))}
          </div>
          <div className="dt-right-body">
            <div style={{ display: rightTab === 'chat' ? 'flex' : 'none', flexDirection: 'column', flex: '1 1 0', minHeight: 0 }}>
              <ChatPane onMenuClick={() => setDrawerOpen(true)} />
            </div>
            {visitedTabs.skill && (
              <div style={{ display: rightTab === 'skill' ? 'flex' : 'none', flexDirection: 'column', flex: '1 1 0', minHeight: 0 }}>
                <SkillPane onManage={() => setSettingsTab('skills')} />
              </div>
            )}
            {visitedTabs.shell && (
              <div style={{ display: rightTab === 'shell' ? 'flex' : 'none', flexDirection: 'column', flex: '1 1 0', minHeight: 0 }}>
                <ShellPane />
              </div>
            )}
          </div>
        </section>
      </div>

      {/* ── Settings panel ────────────────────────────────────── */}
      {settingsTab && <Settings initialTab={settingsTab} onClose={() => setSettingsTab(null)} />}

      {/* ── Mobile drawer ─────────────────────────────────────── */}
      {drawerOpen && (
        <div className="m-drawer-overlay" onClick={() => setDrawerOpen(false)}>
          <div className="m-drawer-panel" onClick={e => e.stopPropagation()}>
            <WorkspacePane />
          </div>
        </div>
      )}

      {/* ── Catch-up-failed modal ─────────────────────────────── */}
      {showCatchupFailed && (
        <div className="dt-catchup-overlay">
          <div className="dt-catchup-card" role="alertdialog" aria-modal="true">
            <div className="dt-catchup-icon">
              <Icon name="x" size={22} />
            </div>
            <div className="serif dt-catchup-title">session catch-up failed</div>
            <div className="dt-catchup-body">
              The catch-up snapshot from{' '}
              <span className="mono">daemon@{sessionId?.slice(0, 4)}</span>{' '}
              couldn't be replayed — the daemon dropped after the reconnect
              attempts were exhausted.
            </div>
            <div className="dt-catchup-trace mono">
              <div className="dt-catchup-trace-err">err: session.catchup_failed</div>
              <div>at reconnect (attempt {connection.attempt})</div>
              <div>sid={sessionId?.slice(0, 8)}  state={connection.state}</div>
            </div>
            <div className="dt-catchup-actions">
              <button
                className="btn-ghost-sm"
                disabled
                title="No raw-log endpoint yet — inspect the browser console for WT errors"
              >
                view raw log
              </button>
              <span style={{ flex: 1 }} />
              <button className="btn-ghost-sm" onClick={startNewSession}>
                new session
              </button>
              <button
                className="btn-primary-sm"
                onClick={() => { setModalDismissed(true); retryConnection() }}
              >
                reconnect
              </button>
            </div>
          </div>
        </div>
      )}

      {/* ── WT reconnecting pill ──────────────────────────────── */}
      {showWtPill && (
        <div className="dt-wt-pill">
          <span className="dt-wt-dot" />
          <span className="mono">WT · reconnecting</span>
          <button
            className="icon-btn-sm"
            title="Retry now"
            aria-label="Retry connection"
            onClick={retryConnection}
          >
            <Icon name="arrow-up" size={12} />
          </button>
        </div>
      )}

      {/* ── Statusbar ─────────────────────────────────────────── */}
      <div className="dt-statusbar" style={{ gridRow: 4 }}>
        <span className="sb-cell">
          <span className={`dot${connection.state === 'live' ? ' live' : ''}`} />
          {connection.state}
        </span>
        <span className="sb-cell mono">main</span>
        <span style={{ flex: 1 }} />
        <span className="sb-cell mono">{APP_VERSION}</span>
      </div>
    </div>
  )
}
