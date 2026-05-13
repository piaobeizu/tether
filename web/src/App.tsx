import { useState } from 'react'
import { useStore } from './lib/store'
import { Icon } from './lib/icons'
import WorkspacePane from './panes/workspace'
import SkillPane from './panes/skill'
import ShellPane from './panes/shell'
import ChatPane from './panes/chat'

export default function App() {
  const [midTab, setMidTab] = useState<'skill' | 'shell'>('skill')
  const [drawerOpen, setDrawerOpen] = useState(false)
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

  return (
    <div className="dt-root">

      {/* ── Titlebar ──────────────────────────────────────────── */}
      <div className="dt-titlebar">
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
          <button className="icon-btn" title="Settings">
            <Icon name="settings" size={14} />
          </button>
        </div>
      </div>

      {/* ── Error banner ──────────────────────────────────────── */}
      {showBanner && (
        <div className="dt-error-banner">
          <span className="dt-error-pulse" />
          <span style={{ fontWeight: 600 }}>daemon unreachable</span>
          <span style={{ color: 'var(--ink-secondary)' }}>
            {connection.state === 'reconnecting'
              ? `retrying · attempt ${connection.attempt}…`
              : 'check connection'}
          </span>
        </div>
      )}

      {/* ── Main grid ─────────────────────────────────────────── */}
      <div className="dt-grid">

        {/* Left: Workspace tree */}
        <aside className="dt-left">
          <WorkspacePane />
        </aside>

        {/* Middle: Skill / Shell */}
        <main className="dt-mid">
          <div className="dt-mid-head">
            <div className="dt-breadcrumb">
              <span className="mono crumb-faint">tether</span>
              <span className="mono crumb-sep">/</span>
              <span className="mono crumb">
                {midTab === 'skill' ? 'skills' : 'shell'}
              </span>
              {sessionId && (
                <span className="pill" style={{ marginLeft: 10 }}>
                  <span className="dot live" />
                  {sessionId.slice(0, 8)}
                </span>
              )}
            </div>
            <div className="dt-mid-actions">
              <button
                className={`btn-ghost-sm${midTab === 'skill' ? ' on' : ''}`}
                onClick={() => setMidTab('skill')}
              >Skills</button>
              <button
                className={`btn-ghost-sm${midTab === 'shell' ? ' on' : ''}`}
                onClick={() => setMidTab('shell')}
              >Shell</button>
            </div>
          </div>
          <div className="dt-mid-body scroll-thin">
            {midTab === 'skill' ? <SkillPane /> : <ShellPane />}
          </div>
        </main>

        {/* Right: Chat */}
        <aside className="dt-right">
          <ChatPane onMenuClick={() => setDrawerOpen(true)} />
        </aside>
      </div>

      {/* ── Mobile drawer ─────────────────────────────────────── */}
      {drawerOpen && (
        <div className="m-drawer-overlay" onClick={() => setDrawerOpen(false)}>
          <div className="m-drawer-panel" onClick={e => e.stopPropagation()}>
            <WorkspacePane />
          </div>
        </div>
      )}

      {/* ── Statusbar ─────────────────────────────────────────── */}
      <div className="dt-statusbar">
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
