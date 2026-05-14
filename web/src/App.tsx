import { useRef, useState } from 'react'
import { useStore } from './lib/store'
import { Icon } from './lib/icons'
import { Settings } from './Settings'
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
  const [drawerOpen, setDrawerOpen] = useState(false)
  const [settingsOpen, setSettingsOpen] = useState(false)
  const [leftW,  setLeftW]  = useState(() => loadWidth(STORAGE_KEY_LEFT,  240))
  const [rightW, setRightW] = useState(() => loadWidth(STORAGE_KEY_RIGHT, 380))
  const { connection, sessionId } = useStore()

  const connPillClass =
    connection.state === 'live' ? 'live' :
    connection.state === 'reconnecting' ? 'warn' : ''

  const connLabel =
    connection.state === 'live' ? 'daemon · live' :
    connection.state === 'reconnecting' ? `reconnecting · attempt ${connection.attempt}` :
    connection.state === 'connecting' ? 'connecting…' :
    'dropped'

  const showBanner = connection.state === 'reconnecting' ||
    (connection.state === 'dropped' && sessionId !== null)

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
          <button className="icon-btn" title="Settings" onClick={() => setSettingsOpen(true)}>
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
            <div style={{ color: 'var(--ink-quat)', fontSize: 12, fontFamily: 'var(--font-mono)', padding: 8 }}>
              — workspace artifacts will appear here —
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
                onClick={() => setRightTab(t)}
              >
                {t === 'chat' ? 'Chat' : t === 'skill' ? 'Skills' : 'Shell'}
              </button>
            ))}
          </div>
          <div className="dt-right-body">
            <div style={{ display: rightTab === 'chat' ? 'flex' : 'none', flexDirection: 'column', flex: '1 1 0', minHeight: 0 }}>
              <ChatPane onMenuClick={() => setDrawerOpen(true)} />
            </div>
            {rightTab === 'skill' && <SkillPane />}
            {rightTab === 'shell' && <ShellPane />}
          </div>
        </section>
      </div>

      {/* ── Settings panel ────────────────────────────────────── */}
      {settingsOpen && <Settings onClose={() => setSettingsOpen(false)} />}

      {/* ── Mobile drawer ─────────────────────────────────────── */}
      {drawerOpen && (
        <div className="m-drawer-overlay" onClick={() => setDrawerOpen(false)}>
          <div className="m-drawer-panel" onClick={e => e.stopPropagation()}>
            <WorkspacePane />
          </div>
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
        <span className="sb-cell mono">v0.4.0</span>
      </div>
    </div>
  )
}
