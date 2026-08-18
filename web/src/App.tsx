import { lazy, Suspense, useEffect, useRef, useState } from 'react'
import { useStore } from './lib/store'
import { Icon, type IconName } from './lib/icons'
import { Settings, type SettingsTab } from './Settings'
import { useAppVersion } from './lib/version'
import { clampRightWidth, loadRightWidth, DEFAULT_LEFT } from './lib/layout'
import WorkspacePane from './panes/workspace'
import SkillPane from './panes/skill'
import ChatPane from './panes/chat'
import WorkPane from './panes/work'
import Canvas from './panes/canvas'

// Shell pulls in xterm (~the bulk of the JS bundle); load it only when the
// Shell tab is first opened so it stays out of the initial download.
const ShellPane = lazy(() => import('./panes/shell'))

// The right pane is a 3-tab surface. 'work' was appended to this type by f29d0a8
// (2026-07-09), three days after the design freeze recorded the 3-tab rule and
// without amending it; it went back to the middle column in tether#90. See
// freeze rule 1 in the design-freeze doc under .claude/ before adding a fourth.
type RightTab = 'chat' | 'skill' | 'shell'

// What the MIDDLE column shows, chosen from the left activity bar (tether#90).
type MainView = 'canvas' | 'work'

const STORAGE_KEY_LEFT  = 'tether_col_left'
const STORAGE_KEY_RIGHT = 'tether_col_right'
const STORAGE_KEY_TAB   = 'tether_right_tab'
const STORAGE_KEY_VIEW  = 'tether_main_view'
const RIGHT_TABS: RightTab[] = ['chat', 'skill', 'shell']
const MAIN_VIEWS: MainView[] = ['canvas', 'work']

const RIGHT_TAB_LABEL: Record<RightTab, string> = { chat: 'Chat', skill: 'Skills', shell: 'Shell' }

// Activity-bar entries, in order. Icon-only with a hover tooltip; the label is
// also the accessible name, and `crumb` is what the middle breadcrumb reads.
// Two on day one — this is the list to extend, not the right-pane tab strip.
const ACTIVITY_ITEMS: { view: MainView; label: string; crumb: string; icon: IconName }[] = [
  { view: 'canvas', label: 'Canvas', crumb: 'workspace', icon: 'file' },
  { view: 'work',   label: 'Work',   crumb: 'work',      icon: 'bolt' },
]

// tether#45 — restore the last-active right tab across reloads. Previously
// rightTab always initialized to 'work', so a hard-refresh dropped you off Chat
// (half the reload-restore complaint). Exported so it unit-tests without
// mounting App (which opens a WebTransport connection).
//
// tether#90 — this is also the migration for browsers that stored 'work' before
// Work left the right pane. What makes a stale value safe is the membership test
// against RIGHT_TABS *plus* a fallback that is itself a member: 'work' now fails
// membership and lands on Chat, exactly like a corrupted value. It is not that
// anything rewrites the stored key — nothing does, until the next selectTab.
export function loadRightTab(): RightTab {
  const saved = localStorage.getItem(STORAGE_KEY_TAB)
  return saved != null && (RIGHT_TABS as string[]).includes(saved) ? (saved as RightTab) : 'chat'
}

/** Restore the last-active middle-column view; same guard shape as loadRightTab. */
export function loadMainView(): MainView {
  const saved = localStorage.getItem(STORAGE_KEY_VIEW)
  return saved != null && (MAIN_VIEWS as string[]).includes(saved) ? (saved as MainView) : 'canvas'
}

const MIN_LEFT  = 160
const MAX_LEFT  = 480

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
  const [rightTab, setRightTab] = useState<RightTab>(loadRightTab)
  const [mainView, setMainView] = useState<MainView>(loadMainView)
  // Keep panes mounted after first visit so switching tabs doesn't tear down the
  // PTY (Shell) or refetch (Skills). Chat is always mounted. tether#45: seed the
  // restored tab as already-visited so a reload onto Skills/Shell renders it
  // (those panes are gated behind visitedTabs) instead of a blank body.
  const [visitedTabs, setVisitedTabs] = useState<Record<RightTab, boolean>>(() => {
    const t = loadRightTab()
    return { chat: true, skill: t === 'skill', shell: t === 'shell' }
  })
  const selectTab = (t: RightTab) => {
    setRightTab(t)
    localStorage.setItem(STORAGE_KEY_TAB, t) // tether#45 — remember across reloads
    setVisitedTabs(v => (v[t] ? v : { ...v, [t]: true }))
    // No Shell refit nudge here any more: ShellPane observes its own container
    // (tether#68), so revealing the pane — 0×0 while display:none, sized once
    // it is flex — fires that observer directly. The synthetic window resize
    // this replaced only ever covered the tab-switch path, never a divider drag.
  }
  // Same keep-alive rule as the right pane, and for the same reason: WorkPane is
  // NOT cheap to remount. WorkGraphView holds the status filter, the wi-type
  // filter, the search query and the fetched graph in local state, and ForceGraph
  // holds the pan/zoom viewBox; unmounting drops all of it and costs a projects
  // fetch plus a graph fetch on the way back. Canvas is always mounted (it is the
  // default), Work once it has been visited.
  const [visitedViews, setVisitedViews] = useState<Record<MainView, boolean>>(() => {
    const v = loadMainView()
    return { canvas: true, work: v === 'work' }
  })
  const selectView = (v: MainView) => {
    setMainView(v)
    localStorage.setItem(STORAGE_KEY_VIEW, v)
    setVisitedViews(s => (s[v] ? s : { ...s, [v]: true }))
  }
  const [drawerOpen, setDrawerOpen] = useState(false)
  const [settingsTab, setSettingsTab] = useState<SettingsTab | null>(null)
  const [leftW,  setLeftW]  = useState(() => loadWidth(STORAGE_KEY_LEFT, DEFAULT_LEFT))
  // tether#69 — clamped against the window being restored into, not just against
  // constants: a width persisted on a wide monitor would otherwise crush the
  // middle pane every time the app loads on a narrower one.
  //
  // tether#102 — the third argument is the TREE width and nothing else.
  // layout.ts's rules are arithmetic on what is left for the middle column, so
  // the quantity they need is all the chrome to the middle's left, which is the
  // activity bar plus the tree. Between tether#90 and tether#102 the addition
  // happened HERE, at both call sites, and forgetting it at either one produced
  // a plausible width with nothing red (that is tether#90's bug, tether#99's two
  // wrong numbers, and tether#100's documented-but-open gap). layout.ts adds the
  // bar itself now — do not add it here. You cannot: ACTIVITY_W is no longer
  // exported, so `+ ACTIVITY_W` on this line does not compile.
  const [rightW, setRightW] = useState(() =>
    loadRightWidth(
      localStorage.getItem(STORAGE_KEY_RIGHT),
      window.innerWidth,
      loadWidth(STORAGE_KEY_LEFT, DEFAULT_LEFT),
    ),
  )
  // Local-only UI dismissals; reset whenever the underlying connection state
  // changes so a fresh failure/reconnect re-surfaces the affordance.
  const [bannerDismissed, setBannerDismissed] = useState(false)
  const [modalDismissed, setModalDismissed] = useState(false)
  // tether#63 — `fatal` is read here for one reason: to stay OUT of the way.
  // Everything below that explains a dead connection does so in the language of
  // an exhausted reconnect ladder, which is the wrong story when the daemon
  // named a cause. See showBanner / showCatchupFailed.
  const { connection, sessionId, fatal } = useStore()
  // Read here for one reason: to bring the file view back when a file is picked.
  const selectedFile = useStore(s => s.selectedFile)
  const appVersion = useAppVersion()

  useEffect(() => {
    setBannerDismissed(false)
    setModalDismissed(false)
  }, [connection.state])

  // Picking a file in the left tree shows that file. Canvas is the only reader of
  // store.selectedFile (panes/canvas/index.tsx), so once the middle column could
  // show something else, a tree click while Work was up set state that nothing
  // rendered — a dead click on the primary desktop path. The tree writes a fresh
  // object per click (store.select spreads a patch), so re-picking the file you
  // are already on still brings you back. Selecting a WI does NOT come through
  // here: store.select only touches the fields present in its argument, and the
  // Work map passes `{ wiId }` alone.
  // eslint-disable-next-line react-hooks/exhaustive-deps -- selectView only closes over setState + localStorage
  useEffect(() => {
    if (selectedFile) selectView('canvas')
  }, [selectedFile])

  // T12 click-to-work (tether#20): WorkDetail asks the shell to bring Chat to
  // front before injecting/switching a session — selectTab/selectView only
  // depend on stable setState functions, so they're safe to omit from deps.
  //
  // tether#90 — this listener is now a ROUTER over surface names, not a setter
  // for the right tab. 'work' names a surface that moved to the middle column,
  // and panes/canvas still dispatches it from its "Pick a wi" home action; that
  // file is outside this change, so the name is honoured here instead of being
  // left to select a tab that no longer exists. Unknown names are ignored rather
  // than trusted into a cast.
  // eslint-disable-next-line react-hooks/exhaustive-deps
  useEffect(() => {
    const onSelectSurface = (e: Event) => {
      const name = (e as CustomEvent<string>).detail
      if (name === 'work') { selectView('work'); return }
      if ((RIGHT_TABS as string[]).includes(name)) selectTab(name as RightTab)
    }
    window.addEventListener('tether:select-tab', onSelectSurface)
    return () => window.removeEventListener('tether:select-tab', onSelectSurface)
  }, [])

  const connPillClass =
    connection.state === 'live' ? 'live' :
    connection.state === 'reconnecting' ? 'warn' : ''

  const connLabel =
    connection.state === 'live' ? 'daemon · live' :
    connection.state === 'reconnecting' ? `reconnecting · attempt ${connection.attempt}` :
    connection.state === 'connecting' ? 'connecting…' :
    // tether#63 — "dropped" reads as "it fell over"; a refusal is the daemon
    // declining on purpose, and the pane's card has the reason.
    fatal !== null ? 'refused' :
    'dropped'

  // Ask ChatPane (owner of the WT connection) to retry immediately.
  const retryConnection = () => window.dispatchEvent(new CustomEvent('tether:retry-connection'))

  // tether#63 — `fatal === null` on the dropped branch. "daemon unreachable ·
  // check connection" is a diagnosis, and it is the wrong one for a daemon that
  // answered promptly with a reason; there is also nothing for the user to check.
  const showBanner = !bannerDismissed && (
    connection.state === 'reconnecting' ||
    (connection.state === 'dropped' && sessionId !== null && fatal === null))

  // Catch-up-failed = a resumable session that the daemon could not re-attach
  // after exhausting reconnect attempts (ChatPane sets state → 'dropped').
  //
  // tether#63 — NOT when a terminal refusal is what dropped us. This modal is a
  // fixed-position, full-viewport overlay (index.css .dt-catchup-overlay,
  // z-index 300) and it renders a specific claim — "the daemon dropped after the
  // reconnect attempts were exhausted", "at reconnect (attempt N)". Both are
  // false for a refusal: the ladder stopped on purpose after ONE attempt because
  // the daemon said retrying was pointless. Left ungated it would cover
  // ChatPane's failed-card with a worse explanation of the same event, which is
  // the whole bug this slice exists to fix, reintroduced one layer up. `fatal`
  // rather than a new ConnState because it is already the single flag meaning
  // "we know why this connection ended", and a fifth ConnState would have to be
  // handled correctly by every reader of connection.state to buy the same thing.
  const showCatchupFailed = !modalDismissed &&
    connection.state === 'dropped' && sessionId !== null && fatal === null

  // WT transport is actively re-establishing.
  const showWtPill = connection.state === 'reconnecting'

  // "new session" — drop the resumable session id and reload so ChatPane
  // opens a fresh WT session instead of trying to catch up the dead one.
  const startNewSession = () => {
    localStorage.removeItem('tether_last_sid')
    location.reload()
  }

  // Esc dismisses the catch-up-failed modal — but not while Settings is open on top
  // of it (Settings owns Esc then, so one Esc closes only the topmost dialog).
  useEffect(() => {
    if (!showCatchupFailed) return
    const onKey = (e: KeyboardEvent) => { if (e.key === 'Escape' && settingsTab === null) setModalDismissed(true) }
    document.addEventListener('keydown', onKey)
    return () => document.removeEventListener('keydown', onKey)
  }, [showCatchupFailed, settingsTab])

  const resizeLeft = (dx: number) => {
    setLeftW(w => {
      const next = Math.max(MIN_LEFT, Math.min(MAX_LEFT, w + dx))
      localStorage.setItem(STORAGE_KEY_LEFT, String(next))
      return next
    })
  }
  const resizeRight = (dx: number) => {
    setRightW(w => {
      // The tree width alone — layout.ts charges the activity bar. See the
      // rightW initializer.
      const next = clampRightWidth(w - dx, window.innerWidth, leftW)
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
            {connection.state === 'live' && connection.latency > 0 && ` · ${connection.latency}ms`}
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
      <div className={`dt-grid mv-${mainView}`} style={{ gridRow: 3 }}>

        {/* Far left: activity bar — picks what the MIDDLE column shows (tether#90).
            Fixed 48px, so no ColResizer beside it. */}
        <nav className="dt-activity" aria-label="Main views">
          {ACTIVITY_ITEMS.map(item => (
            <button
              key={item.view}
              className={`dt-activity-btn${mainView === item.view ? ' on' : ''}`}
              title={item.label}
              aria-label={item.label}
              aria-current={mainView === item.view ? 'page' : undefined}
              onClick={() => selectView(item.view)}
            >
              <Icon name={item.icon} size={18} />
            </button>
          ))}
        </nav>

        {/* Left: Workspace tree — unchanged by tether#90, including its background */}
        <aside className="dt-left" style={{ width: leftW }}>
          <WorkspacePane />
        </aside>

        <ColResizer onDelta={resizeLeft} />

        {/* Middle: whichever main view the activity bar selected */}
        <main className="dt-mid">
          <div className="dt-mid-head">
            <div className="dt-breadcrumb">
              <span className="mono crumb-faint">tether</span>
              <span className="mono crumb-sep">/</span>
              <span className="mono crumb">
                {ACTIVITY_ITEMS.find(i => i.view === mainView)?.crumb ?? mainView}
              </span>
              {sessionId && (
                <span className="pill" style={{ marginLeft: 10 }}>
                  <span className="dot live" />
                  {sessionId.slice(0, 8)}
                </span>
              )}
            </div>
          </div>
          {/* Both views occupy the same grid cell (.dt-mid-stack) and the
              inactive one is display:none, so switching keeps each view's state
              instead of tearing it down. */}
          <div className="dt-mid-stack">
            <div
              className="dt-mid-body scroll-thin"
              style={{ display: mainView === 'canvas' ? 'block' : 'none' }}
            >
              <Canvas />
            </div>
            {visitedViews.work && (
              // Deliberately NOT .dt-mid-body: that class pads 16px a side, and
              // the Work map sizes its cards from the measured container width.
              // `active` gates DetailDrawer's document-level Esc — load-bearing
              // again now that WorkPane stays mounted behind Canvas, which is the
              // exact hazard tether#26 F1 added the prop for.
              <div
                className="dt-mid-work"
                style={{ display: mainView === 'work' ? 'flex' : 'none' }}
              >
                <WorkPane active={mainView === 'work'} />
              </div>
            )}
          </div>
        </main>

        <ColResizer onDelta={resizeRight} />

        {/* Right: Chat / Skills / Shell tabs */}
        <section className="dt-right" style={{ width: rightW }}>
          <div className="dt-right-tabs">
            {/* RIGHT_TABS, not a second literal list: the two used to be written
                out separately and agreed only by hand. */}
            {RIGHT_TABS.map(t => (
              <button
                key={t}
                className={`dt-right-tab${rightTab === t ? ' on' : ''}`}
                onClick={() => selectTab(t)}
              >
                {RIGHT_TAB_LABEL[t]}
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
                <Suspense fallback={<div className="pane-body mono" style={{ color: 'var(--ink-quat)', fontSize: 12 }}>loading shell…</div>}>
                  <ShellPane />
                </Suspense>
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
        <span className="sb-cell mono">{appVersion}</span>
      </div>
    </div>
  )
}
