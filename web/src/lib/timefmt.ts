// timefmt.ts — tiny relative-time formatter for the Work activity timeline
// (tether#31). `now` is injectable so tests don't depend on the wall clock.

const MIN = 60_000
const HOUR = 60 * MIN
const DAY = 24 * HOUR

/**
 * Relative-time label for an ISO-8601 timestamp: "just now", "5m ago",
 * "3h ago", "2d ago", or a short local date beyond a week. Returns '' for a
 * missing/unparseable input and clamps future timestamps (clock skew) to
 * "just now" — never throws, so one bad event ts can't break a row.
 */
export function relTime(iso: string, now: number = Date.now()): string {
  if (!iso) return ''
  const t = Date.parse(iso)
  if (Number.isNaN(t)) return ''
  const diff = now - t
  if (diff < MIN) return 'just now'
  if (diff < HOUR) return `${Math.floor(diff / MIN)}m ago`
  if (diff < DAY) return `${Math.floor(diff / HOUR)}h ago`
  if (diff < 7 * DAY) return `${Math.floor(diff / DAY)}d ago`
  return new Date(t).toLocaleDateString(undefined, { year: 'numeric', month: 'short', day: 'numeric' })
}
