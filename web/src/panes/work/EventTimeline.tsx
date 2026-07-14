// EventTimeline — the Activity section of the wi detail drawer (tether#31).
// Renders the wi's agent-event stream (GET /api/v1/work/items/:id/events,
// newest-first) as a compact timeline: each row = a type glyph + a one-line
// summary (+ optional dim subline) + relative time, with cursor-based
// "load more" pagination. Pure read-only — it only consumes the already-
// proxied events endpoint (no write path), completing the queue/wi/TIMELINE
// MVP triad. Not polled (the drawer detail isn't either); reopen to refresh.
import { useEffect, useRef, useState } from 'react'
import { AihubError, fetchEvents } from '../../lib/aihub'
import type { WorkEvent } from '../../lib/wire.gen'
import { relTime } from '../../lib/timefmt'

function describeError(e: unknown): string {
  if (e instanceof AihubError) {
    if (e.status === 503) return 'aihub not configured'
    if (e.status === 403) return 'not authorized'
    if (e.status === 404) return 'not found'
    return `error (HTTP ${e.status})`
  }
  return e instanceof Error ? e.message : String(e)
}

// payload is `unknown` on the wire — read every field defensively (any shape,
// never throw). These helpers keep the per-type formatter readable.
type Payload = Record<string, unknown>
const asObj = (v: unknown): Payload => (v && typeof v === 'object' ? (v as Payload) : {})
const str = (v: unknown): string => (typeof v === 'string' ? v : '')
const firstLine = (s: string): string => s.split('\n', 1)[0].trim()

// pr_opened.url can arrive with a "Warning: 446 uncommitted changes\n<url>"
// prefix (seen verbatim in real tether#30 events) — take from the LAST
// https:// occurrence so the link is clean.
export function cleanUrl(raw: string): string {
  // Require an explicit https:// and cut at the first whitespace/bracket/quote,
  // so a noise prefix ("Warning: …\n<url>"), a trailing "…/pull/1>", or a
  // non-URL string can't ride along — a bare string would otherwise become a
  // live SPA-relative link on click. No match → '' → renders as plain text.
  const i = raw.lastIndexOf('https://')
  if (i < 0) return ''
  return raw.slice(i).match(/^https:\/\/[^\s<>"')\]]+/)?.[0] ?? ''
}

export interface ActivityRow {
  icon: string
  text: string
  sub?: string
  href?: string
}

/** Map one event to its timeline row; unknown types get a generic fallback. */
export function describeEvent(e: WorkEvent): ActivityRow {
  const p = asObj(e.payload)
  switch (e.type) {
    case 'step_started':
      return { icon: '▸', text: str(p.step) || 'step started' }
    case 'step_completed':
      return { icon: '✓', text: str(p.step) || 'step', sub: str(p.artifact_summary) || undefined }
    case 'step_failed':
      return { icon: '✗', text: `${str(p.step) || 'step'} failed`, sub: str(p.artifact_summary) || str(p.error_type) || undefined }
    case 'commit':
      return { icon: '◆', text: `commit ${str(p.sha).slice(0, 7)}`.trim(), sub: firstLine(str(p.message)) || undefined }
    case 'push':
      return { icon: '↑', text: `push ${str(p.branch)}`.trim() }
    case 'pr_opened': {
      const href = cleanUrl(str(p.url))
      return { icon: '↗', text: str(p.title) || 'PR opened', href: href || undefined }
    }
    case 'note':
      return { icon: '✎', text: firstLine(str(p.text)) || 'note' }
    case 'attempt_started':
      return { icon: '●', text: p.is_takeover ? 'taken over' : p.is_resume ? 'resumed' : 'claimed' }
    case 'attempt_completed':
      return { icon: '✦', text: str(p.status) || 'attempt ended' }
    case 'work_item_filed':
      return { icon: '+', text: 'filed', sub: str(p.goal) || undefined }
    case 'memory_created':
      return { icon: '✳', text: 'memory saved', sub: str(p.type) || undefined }
    default:
      return { icon: '·', text: e.type }
  }
}

export default function EventTimeline({ id }: { id: string }) {
  const [events, setEvents] = useState<WorkEvent[]>([])
  const [cursor, setCursor] = useState<string | undefined>(undefined)
  const [loading, setLoading] = useState(true)
  const [loadingMore, setLoadingMore] = useState(false)
  const [error, setError] = useState<string | null>(null)
  // stale-response guard: a newer id supersedes a slower in-flight first fetch.
  const epoch = useRef(0)

  useEffect(() => {
    const ep = ++epoch.current
    setEvents([])
    setCursor(undefined)
    setError(null)
    setLoading(true)
    fetchEvents(id)
      .then((r) => {
        if (ep !== epoch.current) return
        setEvents(r.events)
        setCursor(r.nextCursor)
        setLoading(false)
      })
      .catch((e) => {
        if (ep !== epoch.current) return
        setError(describeError(e))
        setLoading(false)
      })
  }, [id])

  const loadMore = () => {
    if (!cursor || loadingMore) return
    const ep = epoch.current
    setLoadingMore(true)
    fetchEvents(id, cursor)
      .then((r) => {
        if (ep !== epoch.current) return
        setEvents((prev) => [...prev, ...r.events])
        setCursor(r.nextCursor)
        setLoadingMore(false)
      })
      .catch(() => {
        if (ep !== epoch.current) return
        setLoadingMore(false) // best-effort; keep what we have and allow retry
      })
  }

  if (loading) return <div className="work-empty">loading…</div>
  if (error) return <div className="work-error">{error}</div>
  if (events.length === 0) return <div className="work-empty">no activity yet</div>

  return (
    <div className="work-activity-list">
      {events.map((e, i) => {
        const r = describeEvent(e)
        return (
          <div key={`${e.ts}-${i}`} className="work-activity-row">
            <span className="work-activity-icon mono" aria-hidden>
              {r.icon}
            </span>
            <div className="work-activity-main">
              <div className="work-activity-text">
                {r.href ? (
                  <a href={r.href} target="_blank" rel="noreferrer">
                    {r.text}
                  </a>
                ) : (
                  r.text
                )}
              </div>
              {r.sub && <div className="work-activity-sub">{r.sub}</div>}
            </div>
            <span className="work-activity-time mono" title={e.ts}>{relTime(e.ts)}</span>
          </div>
        )
      })}
      {cursor && (
        <button className="work-activity-more" onClick={loadMore} disabled={loadingMore}>
          {loadingMore ? 'loading…' : 'load more'}
        </button>
      )}
    </div>
  )
}
