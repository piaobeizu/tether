// WorkGraphView — container for the wi relationship map (cards + deterministic
// columns as of tether#25). Hosted in the right Work tab from tether#26; moved
// into the MIDDLE column, behind the left activity bar, in tether#90. Owns the
// graph fetch + poll for the current store.workProject and lazy-loads the
// <ForceGraph> renderer to keep it out of the initial bundle. Clicking a card
// routes to the shared selection (store.select), which opens the DetailDrawer
// over the map; the selected node's block edges are lazily overlaid.
//
// It also owns the SEARCH state (tether#90). The box renders in the filter row
// below, next to the active/all segments — not floating over the map, where it
// covered the leftmost status columns. ForceGraph is controlled from here and
// reports back which match it highlighted; see its `query` / `activeIndex` /
// `onMatchesChange` props.
import { lazy, Suspense, useCallback, useEffect, useRef, useState } from 'react'
import type { KeyboardEvent as ReactKeyboardEvent } from 'react'
import { useStore } from '../../lib/store'
import { AihubError, fetchDeps, fetchGraph } from '../../lib/aihub'
import type { WorkGraph, WorkGraphNode } from '../../lib/wire.gen'
import type { FGEdge, FGNode } from './ForceGraph'

const ForceGraph = lazy(() => import('./ForceGraph'))
const POLL_MS = 8000

function describeError(e: unknown): string {
  if (e instanceof AihubError) {
    if (e.status === 503) return 'aihub not configured'
    if (e.status === 403) return 'not authorized for this project'
    return `error (HTTP ${e.status})`
  }
  return e instanceof Error ? e.message : String(e)
}

// Terminal statuses are hidden by the default 'active' filter (tether#24).
const TERMINAL = new Set(['done', 'wrapped', 'cancelled', 'failed'])
function isTerminal(status: string | undefined): boolean {
  return !!status && TERMINAL.has(status)
}

type FilterMode = 'active' | 'done' | 'all'

export default function WorkGraphView() {
  const project = useStore((s) => s.workProject)
  const selectedWiId = useStore((s) => s.selectedWiId)
  const select = useStore((s) => s.select)

  const [graph, setGraph] = useState<WorkGraph | null>(null)
  const [graphError, setGraphError] = useState<string | null>(null)
  const [blockEdges, setBlockEdges] = useState<FGEdge[]>([])
  // Scale filters (tether#24): default to active (non-terminal) wi so the map
  // isn't buried under a long tail of finished work.
  const [mode, setMode] = useState<FilterMode>('active')
  const [typeF, setTypeF] = useState<string>('')

  // ── Search (tether#29, lifted here in tether#90) ──────────────────────────
  // `query` is the ONLY copy of what the user typed. It used to live inside
  // ForceGraph with a trimmed mirror up here; the box now sits in the filter row
  // below, so the state came with it and the mirror is gone.
  //
  // `activeIndex` is the caller half of a controlled pair: this component states
  // the intent, ForceGraph clamps it against the ordered match list and reports
  // back what it actually highlighted (`matchInfo`). Display and ↑/↓ both read
  // the REPORTED index, never this one, so the counter and the ringed card cannot
  // disagree and the clamping rule is written once.
  //
  // While a search is active the map spans the WHOLE project — the mode/type
  // filter is bypassed so ANY wi is findable, not just the active-filtered
  // subset (tether#29).
  const [query, setQuery] = useState('')
  const [activeIndex, setActiveIndex] = useState(0)
  const [matchInfo, setMatchInfo] = useState<{ total: number; index: number; id: string | undefined }>(
    { total: 0, index: -1, id: undefined },
  )
  // Stable identity so ForceGraph's reset effect never re-fires on a re-render
  // alone; that effect is keyed on the match SET and must stay that way.
  const resetActiveIndex = useCallback(() => setActiveIndex(0), [])

  // Monotonic token: a newer load/project-switch supersedes a slower in-flight
  // fetch so it can't clobber the current project's graph (stale-response guard).
  const graphEpoch = useRef(0)
  const depsEpoch = useRef(0)
  // Latest graph, read by the block-edge effect without keying it on `graph`
  // (which changes identity every poll) so the effect doesn't re-fire per poll.
  const graphRef = useRef<WorkGraph | null>(null)
  graphRef.current = graph

  // ── Graph: (re)load on project change + poll while the tab is visible ──
  useEffect(() => {
    setGraph(null)
    setGraphError(null)
    setBlockEdges([])
    if (!project) return

    let alive = true
    const load = () => {
      const epoch = ++graphEpoch.current
      fetchGraph(project)
        .then((g) => {
          if (!alive || epoch !== graphEpoch.current) return
          setGraph(g)
          setGraphError(null)
        })
        .catch((e) => {
          if (!alive || epoch !== graphEpoch.current) return
          setGraphError(describeError(e))
        })
    }
    load()
    const timer = setInterval(() => {
      if (document.visibilityState === 'visible') load()
    }, POLL_MS)
    const onVisible = () => {
      if (document.visibilityState === 'visible') load()
    }
    document.addEventListener('visibilitychange', onVisible)
    return () => {
      alive = false
      clearInterval(timer)
      document.removeEventListener('visibilitychange', onVisible)
    }
  }, [project])

  // ── Block-edge overlay for the selected node (best-effort). Keyed on the
  // SELECTION only (not `graph`), so an 8s poll doesn't clear+refetch it and
  // jitter the map; the current node set is read from a ref (tether#23 F1).
  // Block edges are drawn as an overlay only — ForceGraph excludes them from
  // its structural layout key, so overlaying them never reshapes the columns. ──
  useEffect(() => {
    setBlockEdges([])
    const g = graphRef.current
    if (!selectedWiId || !g) return
    const epoch = ++depsEpoch.current
    const nodeIds = new Set(g.nodes.map((n) => n.id))
    fetchDeps(selectedWiId)
      .then((deps) => {
        if (epoch !== depsEpoch.current) return
        const edges: FGEdge[] = []
        for (const b of deps.blockedBy) {
          if (nodeIds.has(b.id)) edges.push({ from: b.id, to: selectedWiId, kind: 'block' })
        }
        for (const b of deps.blocking) {
          if (nodeIds.has(b.id)) edges.push({ from: selectedWiId, to: b.id, kind: 'block' })
        }
        setBlockEdges(edges)
      })
      .catch(() => {
        /* deps overlay is best-effort; leave prior state */
      })
    // eslint-disable-next-line react-hooks/exhaustive-deps -- graph read via ref on purpose
  }, [selectedWiId])

  if (!project) {
    return <div className="work-empty work-graph-hint">select a project</div>
  }
  if (graphError) {
    return <div className="work-error work-graph-hint">{graphError}</div>
  }
  if (!graph) {
    return <div className="work-empty work-graph-hint">loading…</div>
  }
  if (graph.nodes.length === 0) {
    return <div className="work-empty work-graph-hint">no active work items</div>
  }

  // Client-side filter of the already-fetched full graph (no backend change).
  const wiTypes = [...new Set(graph.nodes.map((n) => n.wiType).filter((t): t is string => !!t))].sort()
  // "Is a search active" is derived here from the same raw string ForceGraph
  // derives it from, rather than being passed between them. ForceGraph asks
  // `query.trim().toLowerCase() !== ''`; lowercasing cannot turn a non-empty
  // string empty, so the two conditions are equal for every input — which is the
  // property that has to hold. If this said "not searching" while ForceGraph
  // dimmed the map, the filter below would keep hiding wi out from under an
  // active search, which is the tether#29 bug.
  const searching = query.trim() !== ''
  const shown = graph.nodes.filter((n) => {
    // An active search spans the whole project: show every wi so the search box
    // can find (and center) any of them, then dim the non-matches. Without this
    // the default 'active' filter hides wrapped wi and a search for them finds
    // nothing (tether#29 live-verify). Cleared search restores the filter.
    if (searching) return true
    const terminal = isTerminal(n.status)
    const passMode = mode === 'all' ? true : mode === 'done' ? terminal : !terminal
    const passType = !typeF || n.wiType === typeF
    return passMode && passType
  })
  const shownIds = new Set(shown.map((n) => n.id))
  const fgNodes: FGNode[] = shown.map((n) => ({
    id: n.id,
    label: n.slug,
    status: n.status,
    sub: n.wiType,
    priority: n.priority,
    title: n.goal,
  }))
  const parentEdges: FGEdge[] = shown
    .filter((n): n is WorkGraphNode & { parent: string } => !!n.parent && shownIds.has(n.parent))
    .map((n) => ({ from: n.parent, to: n.id, kind: 'parent' as const }))
  const fgEdges = [
    ...parentEdges,
    ...blockEdges.filter((e) => shownIds.has(e.from) && shownIds.has(e.to)),
  ]

  // ↑/↓/↵/Esc on the search box. Navigation walks `matchInfo.index` — the index
  // ForceGraph reported as actually highlighted — rather than the local
  // `activeIndex`, so pressing ↓ right after the match set shrank steps from what
  // is on screen instead of from a stale intent.
  const onSearchKey = (e: ReactKeyboardEvent<HTMLInputElement>) => {
    const { total, index, id } = matchInfo
    if (e.key === 'ArrowDown') {
      e.preventDefault()
      setActiveIndex(total ? (index + 1) % total : 0)
    } else if (e.key === 'ArrowUp') {
      e.preventDefault()
      setActiveIndex(total ? (index - 1 + total) % total : 0)
    } else if (e.key === 'Enter') {
      e.preventDefault()
      if (id) select({ wiId: id })
    } else if (e.key === 'Escape') {
      // Clear on Esc, but only CONSUME the event when there is something to
      // clear — an empty box lets Esc bubble so it can still close an open
      // detail drawer (tether#26 DetailDrawer's document-level Esc listener).
      // Without the stopPropagation, clearing a search would also slam the
      // drawer shut in the same press.
      if (query) {
        e.preventDefault()
        e.stopPropagation()
        setQuery('')
      }
    }
  }

  return (
    <div className="work-graph-view">
      <div className="fg-filter">
        <div className="fg-seg-group">
          {(['active', 'done', 'all'] as const).map((m) => (
            <button
              key={m}
              type="button"
              className={`fg-seg${mode === m ? ' on' : ''}`}
              // While searching the whole project is shown, so the mode filter is
              // inert — disable it to signal the search override (tether#29).
              disabled={searching}
              title={searching ? 'search shows everything — clear the search to filter again' : undefined}
              onClick={() => setMode(m)}
            >
              {m}
            </button>
          ))}
        </div>
        {wiTypes.length > 0 && (
          <select
            className="fg-type-select"
            value={typeF}
            disabled={searching}
            title={searching ? 'search shows everything — clear the search to filter again' : undefined}
            onChange={(e) => setTypeF(e.target.value)}
          >
            <option value="">all types</option>
            {wiTypes.map((t) => <option key={t} value={t}>{t}</option>)}
          </select>
        )}
        <span style={{ flex: 1 }} />
        {/* Search box (tether#90). It used to float over the map as an absolute
            overlay pinned to top-left, which covered the two leftmost status
            columns at every pane width. Here it is an ordinary flow item pushed
            right by the spacer above — nothing overlaps the graph. */}
        <div className="fg-search">
          <input
            className="fg-search-input"
            type="text"
            value={query}
            placeholder="find wi…"
            aria-label="search work items"
            spellCheck={false}
            onChange={(e) => {
              // Reset the walk in the SAME update that changes the query, not
              // by waiting for ForceGraph's onActiveIndexReset. Both land, but
              // only this one lands in the same render: otherwise a keystroke
              // that narrows the match set commits one frame holding the old
              // index, which shows a counter position that no longer exists and
              // re-centers the map on a card the search does not settle on.
              // ForceGraph's callback still covers the case this cannot see —
              // the match set changing under a stable query, e.g. an 8s poll.
              setQuery(e.target.value)
              setActiveIndex(0)
            }}
            onKeyDown={onSearchKey}
          />
          {searching && (
            <span className="fg-search-count">
              {matchInfo.total ? `${matchInfo.index + 1}/${matchInfo.total}` : '0'}
            </span>
          )}
        </div>
        <span className="fg-filter-count mono">
          {searching ? 'search · all' : `${shown.length}/${graph.nodes.length}`}
        </span>
      </div>
      <div className="fg-graph-slot">
        {fgNodes.length === 0 ? (
          <div className="work-empty work-graph-hint">no work items match the filter</div>
        ) : (
          <Suspense fallback={<div className="work-empty work-graph-hint">loading graph…</div>}>
            <ForceGraph
              nodes={fgNodes}
              edges={fgEdges}
              selectedId={selectedWiId ?? undefined}
              // selecting a card just opens the DetailDrawer over the map — no
              // tab switch needed (tether#26; the map moved to the middle column
              // in tether#90, and the drawer came with it).
              onSelect={(id) => select({ wiId: id })}
              query={query}
              activeIndex={activeIndex}
              onMatchesChange={setMatchInfo}
              onActiveIndexReset={resetActiveIndex}
            />
          </Suspense>
        )}
      </div>
    </div>
  )
}
