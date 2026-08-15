import { useEffect, useMemo, useRef, useState } from 'react'
import { TetherWT } from '../../lib/wt'
import { ControlClient } from '../../lib/control'
import { chatURL } from '../../lib/chatUrl'
import { useStore, historyEntryToMessage, mergeTranscript, type HistoryEntry, type ToolCall } from '../../lib/store'
import { CopyButton } from '../../lib/CopyButton'
import { Icon } from '../../lib/icons'
import type { FencedBlock, ProviderListResponse } from '../../lib/wire.gen'
import { ClientFrameAction } from '../../lib/wire.gen'
import { authedFetch } from '../../lib/auth'
import { DagBlock } from '../../fenced-blocks/DagBlock'
import { FormBlock } from '../../fenced-blocks/FormBlock'
import { CandidatesBlock } from '../../fenced-blocks/CandidatesBlock'
import { MediaBlock } from '../../fenced-blocks/MediaBlock'
import { PermissionQueue, postDecide } from '../../fenced-blocks/PermissionBlock'
import Markdown from '../canvas/Markdown'
import SessionList from './SessionList'

type ConnState = 'connecting' | 'connected' | 'reconnecting' | 'failed'

const RECONNECT_BASE_MS = 1_000
const RECONNECT_MAX_MS = 16_000
const RECONNECT_MAX_ATTEMPTS = 5
// tether#52 — how long the FIRST connect waits for the workspace list before
// giving up and connecting without a workspace. A backstop, not the normal path:
// WorkspacePane releases the gate as soon as its fetch settles either way, so this
// only fires if that request hangs — and connecting late is better than a chat pane
// that never connects at all.
const WORKSPACE_GATE_TIMEOUT_MS = 2_000

const SLASH_CMDS = [
  { cmd: '/spec',   desc: 'write a spec for this task' },
  { cmd: '/plan',   desc: 'decompose into ordered steps' },
  { cmd: '/review', desc: 'review pending changes' },
  { cmd: '/diff',   desc: 'show current diff' },
]

function fmtTime(ts: number) {
  return new Date(ts).toLocaleTimeString('en-GB', { hour: '2-digit', minute: '2-digit' })
}

function fmtElapsed(start: number) {
  const mins = Math.floor((Date.now() - start) / 60000)
  if (mins < 1) return 'now'
  if (mins < 60) return `${mins}m`
  return `${Math.floor(mins / 60)}h ${mins % 60}m`
}

// tether#46 — multi-line composer. The composer is a <textarea>: Enter sends,
// Shift+Enter inserts a newline. shouldSendOnEnter and growHeight are extracted
// pure so they unit-test without mounting ChatPane (which opens a WebTransport
// connection). MAX_COMPOSER_LINES / COMPOSER_LINE_PX must match .composer-input
// line-height + max-height in index.css.
const MAX_COMPOSER_LINES = 8
const COMPOSER_LINE_PX = 20

// tether#47 — max @-mention file suggestions shown at once.
const AT_MENU_MAX = 10

// shouldSendOnEnter decides whether an Enter keypress sends the message. It does
// NOT send when: Shift is held (newline), an IME composition is active, a turn is
// streaming (the button is Stop — tether#42), or the slash menu is open (which
// owns Enter). Any non-Enter key never sends.
export function shouldSendOnEnter(o: {
  key: string; shiftKey: boolean; isComposing: boolean; streaming: boolean; slashActive: boolean
}): boolean {
  return o.key === 'Enter' && !o.shiftKey && !o.isComposing && !o.streaming && !o.slashActive
}

// growHeight clamps a textarea's measured scrollHeight to [minLines, maxLines]
// line-heights and reports whether content overflows (so the caller turns on the
// internal scrollbar). Pure — the caller measures scrollHeight and applies the
// result — so it tests without a real DOM.
export function growHeight(
  scrollHeight: number,
  o: { lineHeightPx: number; maxLines: number; minLines?: number },
): { height: number; scroll: boolean } {
  const min = (o.minLines ?? 1) * o.lineHeightPx
  const max = o.maxLines * o.lineHeightPx
  return { height: Math.max(min, Math.min(scrollHeight, max)), scroll: scrollHeight > max }
}

// tether#52 — first-connect ordering. A brand-new session's cwd is pinned at
// spawn (chatUrl.ts), so if the pane connects before the browsed workspace is
// known, a fresh session locks into the daemon's default directory for its
// entire life — there's no fixing it after the fact (see chatUrl.ts's doc
// comment on why `ws` can't just be resent later). WorkspacePane publishes
// `activeWorkspace`/`workspacesLoaded` only once its own GET
// /api/v1/workspaces resolves, which happens strictly AFTER ChatPane mounts,
// so "just connect on mount" races that fetch and — on a cold browser profile
// — normally loses.
//
// The gate applies ONLY to the sid-less path: with a remembered `tether_last_
// sid`, the daemon already knows that session's workspace and ignores
// anything we'd send (chatUrl.ts), so making the overwhelmingly common
// reconnect wait on `workspacesLoaded` would add latency for zero behavioral
// effect. Extracted pure (mirrors shouldSendOnEnter/growHeight above) so this
// ordering decision is unit-testable without mounting the pane (WebTransport).
export function shouldDeferFirstConnect(o: { hasLastSid: boolean; workspacesLoaded: boolean }): boolean {
  return !o.hasLastSid && !o.workspacesLoaded
}

// tether#63 — decides whether a dropped WebTransport connection's onClose
// should schedule the reconnect ladder. Extracted pure (mirrors
// shouldDeferFirstConnect above) so this is unit-testable without mounting
// the pane. Two independent reasons say no:
//   - unmounted: the pane is gone: nothing is left to hand a reconnected
//     socket to, and the cleanup effect has already cancelled any pending
//     timer (pre-existing behaviour, unchanged by tether#63).
//   - fatal: the daemon's LAST word on this connection was a terminal
//     wire.ErrorPayload (session.Refusal with Terminal=true — see
//     wire/errors.go) — e.g. an unknown workspace or a session owned by
//     another tab. The WebTransport handshake itself succeeded (a refusal is
//     sent AFTER wts.Upgrade in wt_chat.go), so retrying reopens the same
//     handshake only to be refused again, once a second, forever — the exact
//     silent-loop bug this slice exists to fix. Both false only when NEITHER
//     condition holds: a live pane whose most recent envelope carried no
//     terminal refusal is the one case an ordinary transient drop should
//     still be retried.
export function shouldReconnectAfterClose(o: { unmounted: boolean; fatal: boolean }): boolean {
  return !o.unmounted && !o.fatal
}

// tether#63 — MIN_USABLE_CONN_MS is how long a connection must stay open before
// it counts as an attempt that WORKED and refunds the reconnect budget.
//
// This is the structural half of the fix, and the reason RECONNECT_MAX_ATTEMPTS
// above was decorative rather than a bound. `wt.connect()` resolving means the
// WebTransport HANDSHAKE succeeded — nothing more. A daemon-side refusal is
// sent AFTER wts.Upgrade has already returned (wt_chat.go), so a refused
// connection completes a perfect handshake and then dies. Refunding the budget
// on that made every cycle attempt #1 again, and the ladder retried once a
// second for as long as the page stayed open. Measured on an unpatched build:
// 27 reconnects in a 30-second window, with no upper bound in sight.
//
// The classification (a terminal wire.ErrorPayload) is what stops such a
// connection on the FIRST attempt, and it is the mechanism that normally runs.
// This threshold is what keeps the loop bounded when that reason does not
// arrive — an older daemon, or a link slow enough to lose the envelope inside
// refusalDrainGrace. It has to be a duration rather than something sharper
// because at close time a refusal and a genuine early drop are the same event;
// what separates them is that a refused session is torn down in well under a
// second (the daemon's own grace is 300ms) while a usable one lives for as long
// as the user is there. 2s sits an order of magnitude above the former and
// orders of magnitude below the latter.
//
// WHO ELSE PAYS, stated rather than left to be discovered: every RETRYABLE
// refusal is also sub-2s by construction (the daemon's grace is the only thing
// holding the session open), so spawn_failed / connection_closed /
// session_unconfirmed now consume budget too. Those used to retry indefinitely;
// they now get five attempts across ~31s and then a Retry button. That is a
// real reduction, and it is the one Attachment.resolve's concurrent-attach note
// leans on — but five spaced retries is a fair allowance for a spawn that keeps
// dying, and "the daemon cannot start an agent" is a state a human should be
// told about rather than one the browser should hammer at forever.
//
// Being wrong in the conservative direction costs a user on a link that cannot
// hold two seconds five quick retries and then a Retry button — which is the
// honest report for a link that cannot hold two seconds.
//
// Kept comfortably above internal/server/wt_chat.go's refusalDrainGrace. That
// relationship is load-bearing and nothing enforces it across the two
// languages; if the grace ever grows towards a second, this must grow with it.
const MIN_USABLE_CONN_MS = 2_000

export function shouldRefundAttemptBudget(uptimeMs: number): boolean {
  return uptimeMs >= MIN_USABLE_CONN_MS
}

// transcriptTextLength is the autoscroll effect's "did any answer grow?" signal
// (tether#88). Pure, and extracted for the same reason as its neighbours above:
// the effect itself needs a mounted pane and a scrollable element, so the only
// part that can be pinned by a test is the decision of when to re-run it.
//
// It sums EVERY message rather than reading the last one, and that is the whole
// change. The old dep was `transcript[length-1].text`, which is the same thing
// while the growing bubble is last — and until tether#88 it always was, because
// sending a prompt ended the open turn, so whatever streamed next opened a bubble
// below the user's message. Now the running turn keeps its bubble ABOVE the
// prompt the user just sent, and text arriving into it changed neither the array
// length nor the last element: the effect stopped firing, the view stopped
// following the answer, and the growing bubble pushed the rest down past the
// viewport. Nothing about the fix is visible if you cannot see it happen.
//
// Summing is safe as a change-detector here because `messages` is append-only
// per turn and a message's text only ever grows; the one in-place replacement
// (a re-emitted fenced block, store.ts's 'fenced' branch) swaps `block` and
// leaves `text` at '', so it neither triggers nor suppresses a scroll — exactly
// as before. It is O(messages) on each render of an already-memoised array,
// against the markdown render the same commit is doing.
export function transcriptTextLength(messages: { text: string }[]): number {
  let n = 0
  for (const m of messages) n += m.text.length
  return n
}

// tether#63 — code→sentence map for the failed-connection card. Only the
// four codes wire.ErrorCode currently classifies Terminal=true (errors.go's
// terminalCodes) need an entry; any other code (including one this frontend
// build predates — see wire.ErrorCode.Terminal's "unclassified defaults
// false" doc comment, which is about retryability, not this map) falls back
// to FATAL_GENERIC_MESSAGE below rather than rendering `undefined`.
export const FATAL_CODE_MESSAGES: Record<string, string> = {
  unknown_workspace: 'This workspace no longer exists on the daemon.',
  no_workspace_registry: "The daemon's workspace registry failed to load.",
  unknown_provider: 'The requested agent provider is not available on this daemon.',
  // NOT "another tab": clientID is per browser profile (persisted in
  // localStorage by AuthPage) and every tab shares it, so admitChat admits
  // them all. Reaching this means a different DEVICE holds the session — see
  // wire.ErrCodeSessionOwned's doc comment.
  session_owned_by_other: 'This session is open on another device.',
}
const FATAL_GENERIC_MESSAGE = 'This connection was refused and cannot be retried automatically.'

// tether#47 — @-file mention. parseAtQuery locates the @token the caret is
// currently inside: scanning back from the caret, the token is valid only if an
// `@` is reached with no whitespace in between AND that `@` sits at the start of
// text or right after whitespace (so `a@b` — an email — is NOT a mention).
// Returns the `@` position and the query (text between `@` and the caret; empty
// right after typing `@`), or null when the caret isn't in a mention token.
export function parseAtQuery(text: string, caret: number): { atPos: number; query: string } | null {
  for (let i = caret - 1; i >= 0; i--) {
    const ch = text[i]
    if (ch === '@') {
      if (i === 0 || /\s/.test(text[i - 1])) return { atPos: i, query: text.slice(i + 1, caret) }
      return null // @ preceded by a non-space → not a mention (e.g. email)
    }
    if (/\s/.test(ch)) return null // whitespace before any @ → caret isn't in a mention
  }
  return null
}

// subseqScore returns -1 if q is not a case-insensitive subsequence of s, else a
// score where SMALLER is a better match: matches within the basename beat
// directory-only matches, then tighter spans, then earlier starts.
function subseqScore(s: string, q: string): number {
  let si = 0, first = -1, last = -1
  for (let qi = 0; qi < q.length; qi++) {
    const found = s.indexOf(q[qi], si)
    if (found < 0) return -1
    if (first < 0) first = found
    last = found
    si = found + 1
  }
  const base = s.lastIndexOf('/') + 1
  const inBasenamePenalty = first >= base ? 0 : 1000
  return inBasenamePenalty + (last - first) * 10 + first
}

// fuzzyRankFiles filters `files` to fuzzy (subsequence) matches of `query` and
// returns the best `limit`, ranked by subseqScore. An empty query returns the
// first `limit` files unchanged (show-all on a bare `@`). Pure — no DOM/fetch.
export function fuzzyRankFiles(files: string[], query: string, limit: number): string[] {
  const q = query.trim().toLowerCase()
  if (!q) return files.slice(0, limit)
  const scored: { f: string; score: number }[] = []
  for (const f of files) {
    const score = subseqScore(f.toLowerCase(), q)
    if (score >= 0) scored.push({ f, score })
  }
  scored.sort((a, b) => a.score - b.score || a.f.length - b.f.length || (a.f < b.f ? -1 : 1))
  return scored.slice(0, limit).map(x => x.f)
}

interface Props {
  onMenuClick?: () => void
}

export default function ChatPane({ onMenuClick: _onMenuClick }: Props) {
  const { messages, notices, sessionId, pendingPermissions, resolvePermission, streaming, streamingMsgId, curTurnId, fatal } = useStore()
  // tether#57 — what the pane actually renders: server-truth `messages` and
  // locally-originated `notices` recombined here, at render time. They are kept
  // apart in the store precisely so the history refetch that session_ready
  // triggers cannot replace a notice out of existence.
  const transcript = useMemo(() => mergeTranscript(messages, notices), [messages, notices])
  const [input, setInput] = useState('')
  const [connState, setConnState] = useState<ConnState>('connecting')
  const [connError, setConnError] = useState<string | null>(null)
  const [reconnectIn, setReconnectIn] = useState(0)
  const [sessionStart, setSessionStart] = useState<number | null>(null)
  const [_elapsed, setElapsed] = useState('')
  const [slashOpen, setSlashOpen] = useState(false)
  const [slashIndex, setSlashIndex] = useState(0)
  const [isComposing, setIsComposing] = useState(false)
  // tether#47 — @-file mention menu (workspace file fuzzy picker).
  const [atOpen, setAtOpen] = useState(false)
  const [atItems, setAtItems] = useState<string[]>([])
  const [atIndex, setAtIndex] = useState(0)
  const [atTruncated, setAtTruncated] = useState(false) // workspace file list hit the cap
  const activeWorkspace = useStore(s => s.activeWorkspace)
  // Per-workspace file-list cache, keyed by workspace id and holding the fetch
  // PROMISE (not the resolved array) so concurrent onChange+onSelect calls share
  // one request; resolves to {files,truncated}. Filtered client-side thereafter.
  const treeCacheRef = useRef<Map<string, Promise<{ files: string[]; truncated: boolean }>>>(new Map())
  const [showEmpty, setShowEmpty] = useState(false)
  // Which message ids have their fenced block expanded to the full variant.
  const [expandedBlocks, setExpandedBlocks] = useState<Set<string>>(() => new Set())
  const toggleBlock = (id: string) => setExpandedBlocks(prev => {
    const next = new Set(prev)
    if (next.has(id)) next.delete(id); else next.add(id)
    return next
  })
  // Which message ids have their collapsed thinking block expanded (tether#34).
  // Live thinking (before the answer) always renders expanded; once the answer
  // starts it collapses to a one-line "thought Xs" summary that this Set re-opens.
  const [expandedThinking, setExpandedThinking] = useState<Set<string>>(() => new Set())
  const toggleThinking = (id: string) => setExpandedThinking(prev => {
    const next = new Set(prev)
    if (next.has(id)) next.delete(id); else next.add(id)
    return next
  })
  const writerRef = useRef<WritableStreamDefaultWriter<Uint8Array> | null>(null)
  const wtRef = useRef<TetherWT | null>(null)
  const controlRef = useRef<ControlClient | null>(null)
  const attemptRef = useRef(0)
  // tether#63 — when the current connection's handshake completed, or 0 when
  // none is open. Read once in onClose to decide whether that connection lasted
  // long enough to refund the attempt budget (shouldRefundAttemptBudget).
  const connectedAtRef = useRef(0)
  const reconnectTimerRef = useRef<ReturnType<typeof setTimeout> | null>(null)
  const countdownRef = useRef<ReturnType<typeof setInterval> | null>(null)
  const unmountedRef = useRef(false)
  // tether#52 — guards the mount effect's deferred-first-connect path so the
  // store-subscription and the 2s fallback timer (both of which can fire)
  // trigger doConnect at most once. Reset at the top of that effect on each
  // run (relevant under StrictMode's dev double-invoke).
  const firstConnectedRef = useRef(false)
  const chatRef = useRef<HTMLDivElement>(null)
  const taRef = useRef<HTMLTextAreaElement>(null)
  const [providers, setProviders] = useState<string[]>(['claude-code'])
  const [selectedProvider, setSelectedProvider] = useState(
    () => localStorage.getItem('tether_default_provider') ?? 'claude-code'
  )

  useEffect(() => {
    authedFetch('/api/v1/providers')
      .then(r => r.json() as Promise<ProviderListResponse>)
      .then(d => { if (d.providers?.length > 0) setProviders(d.providers) })
      .catch(() => {})
  }, [])

  // Sync default provider when changed from Settings panel (same-window custom event)
  useEffect(() => {
    const onProviderChange = (e: Event) => {
      const p = (e as CustomEvent<string>).detail
      if (p) setSelectedProvider(p)
    }
    window.addEventListener('tether:provider-changed', onProviderChange)
    return () => window.removeEventListener('tether:provider-changed', onProviderChange)
  }, [])

  // tether#45 — restore the last session on (re)mount so history loads from
  // /messages IMMEDIATELY, without waiting for session_ready. session_ready is
  // sent only after cc emits system/init, which in stream-json input mode needs
  // a fresh prompt (wt_chat.go) and is unreliable under cc --resume contention
  // (zombie spawn, mem_ruSB7HHI) — so a plain reload otherwise showed an empty
  // "new" session. Setting sessionId here fires the history-load effect below,
  // which fetches /messages over HTTP (independent of cc). A later session_ready
  // re-confirms the same sid (cc --resume keeps its id) as a no-op; a different
  // sid re-fires the effect, but its msgs.length>0 guard drops an EMPTY /messages
  // so it can't wipe restored history (a non-empty payload for that sid replaces,
  // intentionally). A DELIBERATE switch is the other case and does not come
  // through here — see lib/session.ts openSession, which owns that load and
  // explains why its guards differ from this effect's (tether#61).
  useEffect(() => {
    if (!useStore.getState().sessionId) {
      const last = localStorage.getItem('tether_last_sid')
      if (last) useStore.getState().setSessionId(last)
    }
  }, [])

  // Load chat history when session ID is first established.
  useEffect(() => {
    if (!sessionId) return
    fetch(`/api/v1/sessions/${encodeURIComponent(sessionId)}/messages`)
      .then(r => r.ok ? r.json() : [])
      .then((msgs: HistoryEntry[]) => {
        // Don't clobber an in-flight turn (tether#42 fix). On the FIRST send of
        // a new session, session_ready sets sessionId and fires this effect;
        // /messages already has the just-persisted user msg, so loadHistory
        // would run and reset streaming/curTurn — wiping the optimistic
        // "thinking…" indicator (streaming set in sendMessage) during the gap
        // before the first token. While a turn is streaming the live stream is
        // authoritative, so skip the reload.
        if (msgs.length > 0 && !useStore.getState().streaming) {
          useStore.getState().loadHistory(msgs.map(historyEntryToMessage))
        }
      })
      .catch(() => {})
  }, [sessionId])

  useEffect(() => {
    if (sessionId) setSessionStart(Date.now())
    else setSessionStart(null)
  }, [sessionId])

  useEffect(() => {
    if (!sessionStart) { setElapsed(''); return }
    setElapsed(fmtElapsed(sessionStart))
    const id = setInterval(() => setElapsed(fmtElapsed(sessionStart)), 30_000)
    return () => clearInterval(id)
  }, [sessionStart])

  // Scroll to bottom on new messages AND when streaming text accumulates
  const grown = transcriptTextLength(transcript)
  useEffect(() => {
    const el = chatRef.current
    if (!el) return
    // Always scroll during streaming; otherwise only if already near bottom
    const nearBottom = el.scrollHeight - el.scrollTop - el.clientHeight < 120
    if (streaming || nearBottom) {
      el.scrollTop = el.scrollHeight
    }
  }, [transcript.length, grown, streaming])

  // tether#46 — auto-grow the composer textarea to fit its content, up to
  // MAX_COMPOSER_LINES then scroll internally. Reset to 'auto' first so the
  // measured scrollHeight shrinks when text is deleted (and after send clears
  // `input`, this floors it back to one line). growHeight owns the clamp.
  const growComposer = () => {
    const ta = taRef.current
    if (!ta) return
    ta.style.height = 'auto'
    const { height, scroll } = growHeight(ta.scrollHeight, { lineHeightPx: COMPOSER_LINE_PX, maxLines: MAX_COMPOSER_LINES })
    ta.style.height = `${height}px`
    ta.style.overflowY = scroll ? 'auto' : 'hidden'
  }
  useEffect(() => { growComposer() }, [input]) // eslint-disable-line react-hooks/exhaustive-deps
  // Recompute on WIDTH changes (right column is user-resizable via ColResizer;
  // sidebar/drawer toggle; window resize) so a multi-line draft rewrapping taller
  // doesn't clip under overflow-y:hidden until the next keystroke. Width-guarded
  // so our own height writes (which ResizeObserver would otherwise echo) can't
  // feedback-loop. jsdom lacks ResizeObserver → guard keeps tests/SSR safe.
  useEffect(() => {
    const ta = taRef.current
    if (!ta || typeof ResizeObserver === 'undefined') return
    let lastW = ta.clientWidth
    const ro = new ResizeObserver(() => {
      if (ta.clientWidth !== lastW) { lastW = ta.clientWidth; growComposer() }
    })
    ro.observe(ta)
    return () => ro.disconnect()
  }, []) // eslint-disable-line react-hooks/exhaustive-deps

  // Empty-state hint, debounced so it doesn't flash on session resume before
  // history arrives (connState flips to 'connected' before /messages loads).
  useEffect(() => {
    const empty = transcript.length === 0 && connState === 'connected' && !streaming && pendingPermissions.length === 0
    if (!empty) { setShowEmpty(false); return }
    const t = setTimeout(() => setShowEmpty(true), 500)
    return () => clearTimeout(t)
  }, [transcript.length, connState, streaming, pendingPermissions.length])

  const cancelPendingReconnect = () => {
    if (reconnectTimerRef.current !== null) { clearTimeout(reconnectTimerRef.current); reconnectTimerRef.current = null }
    if (countdownRef.current !== null) { clearInterval(countdownRef.current); countdownRef.current = null }
  }

  const scheduleReconnect = () => {
    if (unmountedRef.current) return
    // tether#63 — clear any timer/countdown already pending before arming new
    // ones. Since a close can now be reported by either the `closed` handler or
    // connect()'s own chain (wt.ts), two calls in quick succession are possible,
    // and the second used to overwrite countdownRef without clearing the first
    // interval — a leaked setInterval double-decrementing the countdown.
    cancelPendingReconnect()
    attemptRef.current += 1
    useStore.getState().setConnection({ state: 'reconnecting', attempt: attemptRef.current })
    if (attemptRef.current > RECONNECT_MAX_ATTEMPTS) {
      setConnState('failed')
      useStore.getState().setConnection({ state: 'dropped' })
      return
    }
    const delayMs = Math.min(RECONNECT_BASE_MS * 2 ** (attemptRef.current - 1), RECONNECT_MAX_MS)
    setConnState('reconnecting')
    setReconnectIn(Math.ceil(delayMs / 1000))
    countdownRef.current = setInterval(() => setReconnectIn(prev => Math.max(0, prev - 1)), 1000)
    reconnectTimerRef.current = setTimeout(() => { cancelPendingReconnect(); if (!unmountedRef.current) doConnect() }, delayMs)
  }

  // eslint-disable-next-line react-hooks/exhaustive-deps
  const doConnect = () => {
    // tether#52 — mark the first connect as done HERE, not only in the mount
    // effect's startOnce. manualRetry (the `tether:retry-connection` event, which
    // openSession, the error banner and the WT pill all dispatch) calls doConnect
    // directly, so a retry that lands inside the deferred window would otherwise
    // still be followed by the gate's own connect — tearing down the connection
    // the user's action just opened.
    firstConnectedRef.current = true
    cancelPendingReconnect()
    setConnState('connecting')
    setConnError(null)
    useStore.getState().setConnection({ state: 'connecting' })
    // tether#63 — a fresh attempt deserves a fresh chance: whatever refused
    // the PREVIOUS connection (e.g. a since-closed other tab holding the
    // session) may no longer apply, and this is a deliberate new handshake,
    // not the reconnect ladder retrying the same one automatically.
    useStore.getState().clearFatal()

    const old = wtRef.current
    wtRef.current = null
    writerRef.current?.releaseLock()
    writerRef.current = null
    old?.close()

    controlRef.current?.stop()
    controlRef.current = null

    // Resume last session if available — keeps history consistent across refreshes.
    const lastSid = localStorage.getItem('tether_last_sid') ?? ''
    // tether#52 — read the browsed workspace via getState(), NOT the reactive
    // `activeWorkspace` selector below (that one exists for the @-mention
    // picker's re-renders). A plain read here means a workspace switch while
    // connected can never retrigger this closure — the only thing that can
    // start a new connection is a fresh mount or an explicit retry/reconnect,
    // which is exactly what must stay true: a live session's workspace is
    // immutable, so browsing elsewhere must not tear down the WebTransport
    // (see the mount effect below and chatUrl.ts's doc comment for why).
    const wsID = useStore.getState().activeWorkspace?.id ?? ''
    const url = chatURL({ host: location.host, provider: selectedProvider, sid: lastSid, wsID })
    const wt = new TetherWT({
      url,
      onEnvelope: useStore.getState().handleEnvelope,
      onClose: () => {
        useStore.getState().setConnected(false)
        controlRef.current?.stop()
        controlRef.current = null
        // tether#63 — refund the attempt budget only for a connection that
        // lasted long enough to have been usable, THEN decide about retrying.
        // Order matters: refunding after the decision would let a refused
        // connection hand its successor a full budget.
        const upMs = connectedAtRef.current === 0 ? 0 : Date.now() - connectedAtRef.current
        connectedAtRef.current = 0
        if (shouldRefundAttemptBudget(upMs)) attemptRef.current = 0
        // A terminal refusal recorded by handleEnvelope's 'error' case (see
        // store.ts) means retrying THIS connection is pointless.
        // shouldReconnectAfterClose is the one place that decision is made, so
        // it can be pinned by a unit test without mounting the pane.
        const isFatal = useStore.getState().fatal !== null
        if (!shouldReconnectAfterClose({ unmounted: unmountedRef.current, fatal: isFatal })) {
          if (isFatal) {
            // Stop the ladder outright rather than let scheduleReconnect fire
            // once more and immediately refuse again — same daemon, same
            // workspace/session, same answer. 'dropped' (not 'reconnecting')
            // reflects that nothing is scheduled to retry.
            cancelPendingReconnect()
            setConnState('failed')
            useStore.getState().setConnection({ state: 'dropped' })
          }
          return
        }
        scheduleReconnect()
      },
    })
    wtRef.current = wt

    wt.connect().then(async () => {
      // tether#63 — the attempt budget is NOT refunded here. See
      // shouldRefundAttemptBudget: a handshake is not evidence that the
      // connection is usable, and refunding on one is what made the bounded
      // ladder unbounded. connectedAtRef starts the clock the close handler
      // measures against.
      connectedAtRef.current = Date.now()
      useStore.getState().setConnected(true)
      setConnState('connected')
      const stream = await wt.openBidiStream()
      writerRef.current = stream.writable.getWriter()

      // Start the control channel (ping/pong RTT) only after the main
      // connection is live — setConnected(true) already reset latency:0,
      // so pushing samples now won't be immediately clobbered.
      const control = new ControlClient()
      controlRef.current = control
      void control.start()
    }).catch((err: unknown) => {
      const msg = err instanceof Error ? err.message : String(err)
      console.error('[tether] chat connect failed:', msg)
      setConnError(msg)
      // tether#63 — the same terminal check as onClose, because this chain can
      // report the death first: before the daemon held refused sessions open
      // (refusalDrainGrace), `openBidiStream()` threw here in the same tick the
      // refusal killed the session, and that — not onClose — is what drove the
      // endless loop. Gating only onClose would have left this path retrying a
      // refusal the daemon has already explained.
      const isFatal = useStore.getState().fatal !== null
      if (!shouldReconnectAfterClose({ unmounted: unmountedRef.current, fatal: isFatal })) {
        // Only an unmounted pane gets no bookkeeping — there is no UI left to
        // put a state on, and the cleanup effect already cancelled the timer.
        if (isFatal && !unmountedRef.current) {
          cancelPendingReconnect()
          setConnState('failed')
          useStore.getState().setConnection({ state: 'dropped' })
        }
        return
      }
      scheduleReconnect()
    })
  }

  const manualRetry = () => { attemptRef.current = 0; doConnect() }

  // Keep a live ref to manualRetry so the window listener (attached once) always
  // invokes the latest closure without re-binding on every render.
  const manualRetryRef = useRef(manualRetry)
  manualRetryRef.current = manualRetry

  // App-level error UI (banner "retry now", catch-up modal "reconnect",
  // WT pill) asks this pane — owner of the WT connection — to retry.
  useEffect(() => {
    const onRetry = () => manualRetryRef.current()
    window.addEventListener('tether:retry-connection', onRetry)
    return () => window.removeEventListener('tether:retry-connection', onRetry)
  }, [])

  // tether#52 — first-connect ordering (see shouldDeferFirstConnect above).
  // Only the sid-less path defers, and only until `workspacesLoaded` flips
  // true or a 2s fallback elapses — whichever comes first — so a hung/failed
  // /api/v1/workspaces degrades to today's behaviour (connect with no `ws`)
  // rather than never connecting. `activeWorkspace` itself is intentionally
  // NOT in this effect's reactive surface (no dependency, no selector) — this
  // effect decides WHEN the first connect fires, never RE-fires it, which is
  // what guarantees browsing a different workspace later can't tear down a
  // live WebTransport (see doConnect's wsID comment / chatUrl.ts).
  useEffect(() => {
    unmountedRef.current = false
    firstConnectedRef.current = false
    const startOnce = () => {
      if (firstConnectedRef.current) return // double-connect guard
      firstConnectedRef.current = true
      doConnect()
    }
    const cleanupConnection = () => {
      unmountedRef.current = true
      cancelPendingReconnect()
      writerRef.current?.releaseLock()
      wtRef.current?.close()
      controlRef.current?.stop()
      controlRef.current = null
    }

    const hasLastSid = !!(localStorage.getItem('tether_last_sid') ?? '')
    if (!shouldDeferFirstConnect({ hasLastSid, workspacesLoaded: useStore.getState().workspacesLoaded })) {
      startOnce()
      return cleanupConnection
    }

    // Deferred: wait for WorkspacePane's fetch to settle, or bail out after
    // WORKSPACE_GATE_TIMEOUT_MS so a hung request can't block chat forever.
    //
    // The store update that opens the gate ALSO carries the workspace
    // (store.ts settleWorkspaces) — this listener runs synchronously inside it and
    // connects immediately, so a gate that opened one update before the value was
    // published would connect with no workspace. That was this slice's original bug;
    // see settleWorkspaces.
    let fallback: ReturnType<typeof setTimeout> | undefined
    const unsub = useStore.subscribe((s) => {
      if (s.workspacesLoaded) { unsub(); clearTimeout(fallback); startOnce() }
    })
    fallback = setTimeout(() => { unsub(); startOnce() }, WORKSPACE_GATE_TIMEOUT_MS)

    return () => {
      unsub()
      clearTimeout(fallback)
      cleanupConnection()
    }
  }, [])

  const sendMessage = async () => {
    const text = input.trim()
    if (!text || !writerRef.current) return
    setSlashOpen(false)
    setAtOpen(false) // tether#47 review MINOR-1 — don't leave a stale @ menu after send
    useStore.getState().addMessage({ id: crypto.randomUUID(), role: 'user', text, ts: Date.now() })
    // Light up the "thinking" indicator immediately: `streaming` otherwise
    // only flips true on the first agent event, leaving a blind gap after send
    // where the user can't tell whether the agent is working or stalled.
    // streamingMsgId stays null so the thinking-dots (not a text cursor) show.
    useStore.setState({ streaming: true, streamingMsgId: null })
    const line = JSON.stringify({ text }) + '\n'
    try { await writerRef.current.write(new TextEncoder().encode(line)) } catch (err) { console.error('[tether] send failed:', err) }
    setInput('')
  }

  // T12 click-to-work (tether#20) — programmatic send, bypassing the `input`
  // React state entirely (it's async; setInput() then sendMessage() would
  // race and send the PREVIOUS value). Mirrors sendMessage's write path.
  const doInjectAndSend = (text: string) => {
    if (!writerRef.current) return
    useStore.getState().addMessage({ id: crypto.randomUUID(), role: 'user', text, ts: Date.now() })
    useStore.setState({ streaming: true, streamingMsgId: null })
    const line = JSON.stringify({ text }) + '\n'
    writerRef.current.write(new TextEncoder().encode(line))
      .catch(err => console.error('[tether] inject send failed:', err))
  }

  // Queued text waiting for a live writer — set whenever injectAndSend is
  // called before the WT connection (and its writer) is ready. Flushed by
  // the connState effect below, with a bounded retry loop for the narrow
  // race where connState flips to 'connected' just BEFORE writerRef.current
  // is assigned in doConnect (see the .then() there).
  const pendingInjectRef = useRef<string | null>(null)
  const pendingInjectDeadlineRef = useRef(0)

  const tryFlushPendingInject = () => {
    const text = pendingInjectRef.current
    if (text === null) return
    if (writerRef.current) {
      pendingInjectRef.current = null
      doInjectAndSend(text)
      return
    }
    if (Date.now() > pendingInjectDeadlineRef.current) {
      console.error('[tether] inject-prompt timed out waiting for connection')
      pendingInjectRef.current = null
      return
    }
    setTimeout(tryFlushPendingInject, 150)
  }

  useEffect(() => {
    if (connState === 'connected') tryFlushPendingInject()
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [connState])

  const injectAndSend = (text: string) => {
    const trimmed = text.trim()
    if (!trimmed) return
    if (writerRef.current && connState === 'connected') {
      doInjectAndSend(trimmed)
      return
    }
    // Not connected (or writer not yet assigned) — queue it and start
    // polling; up to ~5s, same budget as the composer disabling itself
    // while reconnecting.
    pendingInjectRef.current = trimmed
    pendingInjectDeadlineRef.current = Date.now() + 5_000
    tryFlushPendingInject()
  }

  // Live ref so the once-attached window listener always calls the latest
  // closure (mirrors manualRetryRef below).
  const injectAndSendRef = useRef(injectAndSend)
  injectAndSendRef.current = injectAndSend

  useEffect(() => {
    const onInject = (e: Event) => injectAndSendRef.current((e as CustomEvent<string>).detail)
    window.addEventListener('tether:inject-prompt', onInject)
    return () => window.removeEventListener('tether:inject-prompt', onInject)
  }, [])

  // tether#61 — ChatPane used to own "switch to session X" (switchSession) and
  // publish it as a `tether:switch-session` window event for WorkDetail's
  // click-to-work to call. That operation now lives in lib/session.ts
  // openSession, which its callers import directly, so both the local copy and
  // the event relay are gone: one implementation, reached one way. ChatPane's
  // remaining part in a switch is the reconnect, which arrives on the
  // pre-existing `tether:retry-connection` channel above — it owns the WT.

  // D-19 §5 / tether#8 T8 — DagBlock's approve button. Sends an "action"
  // ClientFrame on the /wt/control channel, which is not otherwise
  // session-scoped, so the current sessionId travels in the frame itself;
  // the daemon routes it to that session's agent (Registry.DeliverAction).
  // Best-effort like the ping/pong RTT probe: no ack is awaited, and if
  // sessionId or blockId aren't known yet the click is a no-op.
  const sendApprove = (block: FencedBlock) => {
    const sessionId = useStore.getState().sessionId
    if (!sessionId || !block.blockId) return
    void controlRef.current?.sendAction({
      kind: ClientFrameAction,
      sessionId,
      blockId: block.blockId,
      action: 'approve',
      skill: block.skill,
    })
  }

  // D-19 §5 / tether#8 T9 — DagBlock's pause button. Mirrors sendApprove
  // exactly (same frame shape, same best-effort no-ack semantics); only the
  // `action` value differs. The daemon routes "pause" to
  // Registry.InterruptSession (agent.Session.Interrupt) instead of
  // DeliverAction/SendPrompt — see docs/wire/fenced-contract.md §5.
  const sendPause = (block: FencedBlock) => {
    const sessionId = useStore.getState().sessionId
    if (!sessionId || !block.blockId) return
    void controlRef.current?.sendAction({
      kind: ClientFrameAction,
      sessionId,
      blockId: block.blockId,
      action: 'pause',
      skill: block.skill,
    })
  }

  // tether#42 — session-level interrupt (stop the streaming turn). Unlike
  // sendPause (DAG-card scoped, needs blockId), the daemon's "pause" action
  // routes by SessionID alone (control.go handleActionFrame → InterruptSession
  // → cc control_request{interrupt}), so no blockId. cc aborts the turn and
  // stays resumable; it emits no EventResult, so we finalize locally too.
  const sendStop = () => {
    const sessionId = useStore.getState().sessionId
    if (sessionId) {
      void controlRef.current?.sendAction({ kind: ClientFrameAction, sessionId, action: 'pause' })
    }
    useStore.getState().stopTurn()
  }

  const handleInputChange = (v: string) => {
    setInput(v)
    // Only while typing the command token itself (no space yet). Once args begin,
    // close the menu so Enter sends the message instead of re-picking the command.
    setSlashOpen(v.startsWith('/') && !v.includes(' '))
    setSlashIndex(0)
    refreshAtMenu() // tether#47 — recompute the @-mention menu from the new value + caret
  }

  const filteredSlash = SLASH_CMDS.filter(c => c.cmd.startsWith(input.split(' ')[0]))

  const pickSlash = (c: { cmd: string }) => {
    setInput(c.cmd + ' ')
    setSlashOpen(false)
    setSlashIndex(0)
  }

  // tether#47 — fetch the active workspace's flat file list for the @-mention
  // picker, memoized by workspace as a PROMISE so onChange+onSelect firing in the
  // same tick share ONE request (review MINOR-2 in-flight dedup). Resolves to
  // {files,truncated}; on error resolves empty AND drops the cache so a later @
  // retries. Successful results stay cached for the session.
  const ensureTree = (wsId: string): Promise<{ files: string[]; truncated: boolean }> => {
    const existing = treeCacheRef.current.get(wsId)
    if (existing) return existing
    const p = (async () => {
      try {
        const r = await fetch(`/api/v1/workspaces/${encodeURIComponent(wsId)}/tree`)
        if (!r.ok) { treeCacheRef.current.delete(wsId); return { files: [], truncated: false } }
        const data = (await r.json()) as { files?: string[]; truncated?: boolean }
        return { files: data.files ?? [], truncated: data.truncated === true }
      } catch { treeCacheRef.current.delete(wsId); return { files: [], truncated: false } }
    })()
    treeCacheRef.current.set(wsId, p)
    return p
  }

  // refreshAtMenu recomputes the @ menu from the textarea's live value + caret.
  // Called on input and on caret moves (onSelect). No active @token / no browsed
  // workspace → menu closes. First @ in a workspace awaits one fetch; after that
  // the promise cache resolves synchronously-fast. Re-parses the query when the
  // fetch resolves so late-arriving files rank against the CURRENT query, not the
  // one captured when the fetch started (review MINOR-2 stale-query race).
  const refreshAtMenu = () => {
    const ta = taRef.current
    const ws = useStore.getState().activeWorkspace
    if (!ta || !ws) { setAtOpen(false); return }
    if (!parseAtQuery(ta.value, ta.selectionStart ?? ta.value.length)) { setAtOpen(false); return }
    void ensureTree(ws.id).then(({ files, truncated }) => {
      // Re-read the query NOW (the user may have typed on during the fetch).
      const t = taRef.current
      const q = t ? parseAtQuery(t.value, t.selectionStart ?? t.value.length) : null
      if (!q) { setAtOpen(false); return }
      const ranked = fuzzyRankFiles(files, q.query, AT_MENU_MAX)
      setAtItems(ranked); setAtIndex(0); setAtTruncated(truncated); setAtOpen(ranked.length > 0)
    })
  }

  // pickAt inserts the chosen file as an absolute @<path> mention, splicing it
  // over the active @query and restoring the caret after the inserted token.
  // Absolute so cc resolves it regardless of its (decoupled) cwd (tether#47).
  const pickAt = (rel: string) => {
    const ta = taRef.current
    const ws = useStore.getState().activeWorkspace
    if (!ta || !ws) return
    const caret = ta.selectionStart ?? ta.value.length
    const q = parseAtQuery(ta.value, caret)
    if (!q) return
    const token = '@' + ws.path.replace(/\/+$/, '') + '/' + rel + ' '
    const next = ta.value.slice(0, q.atPos) + token + ta.value.slice(caret)
    setInput(next)
    setAtOpen(false)
    const newCaret = q.atPos + token.length
    requestAnimationFrame(() => {
      const t = taRef.current
      if (t) { t.focus(); t.setSelectionRange(newCaret, newCaret) }
    })
  }

  return (
    <>
      {/* ── Session list (tether#91) ──────────────────────────
          Moved here from the bottom of the file tree, where it was a category
          error: the left column is about files. Collapsed by default so it costs
          the transcript no height until asked for. Its own component because this
          file is long enough. */}
      <SessionList />

      {/* ── Message list ──────────────────────────────────── */}
      <div className="dt-chat scroll-thin" ref={chatRef}>

        {connState === 'reconnecting' && (
          <div className="reconnect-banner">
            <span style={{ width: 6, height: 6, borderRadius: 999, background: 'var(--warn)', flexShrink: 0 }} />
            <span>reconnecting in {reconnectIn}s</span>
            <span
              onClick={manualRetry}
              style={{ marginLeft: 'auto', color: 'var(--ink-tertiary)', cursor: 'pointer', textDecoration: 'underline', fontSize: 11 }}
            >retry now</span>
          </div>
        )}

        {connState === 'failed' && (
          <div className="failed-card">
            {fatal ? (
              // tether#63 — the daemon told us WHY, and it was terminal: lead
              // with the code→sentence translation (falling back to a generic
              // sentence for a code this frontend build doesn't recognize —
              // see FATAL_CODE_MESSAGES' doc comment), then the daemon's own
              // message text for anyone who wants the raw detail.
              <>
                <div style={{ color: 'var(--danger)', fontWeight: 600, marginBottom: 4 }}>
                  {FATAL_CODE_MESSAGES[fatal.code] ?? FATAL_GENERIC_MESSAGE}
                </div>
                <div style={{ color: 'var(--ink-tertiary)', fontSize: 11, marginBottom: 8, wordBreak: 'break-all' }}>{fatal.message}</div>
              </>
            ) : (
              <>
                <div style={{ color: 'var(--danger)', fontWeight: 600, marginBottom: 4 }}>WebTransport connection failed</div>
                {connError && <div style={{ color: 'var(--ink-tertiary)', fontSize: 11, marginBottom: 6, wordBreak: 'break-all' }}>{connError}</div>}
                <div style={{ color: 'var(--ink-tertiary)', fontSize: 11, marginBottom: 8 }}>UDP/QUIC may be blocked — see K.8.1 in README.</div>
              </>
            )}
            <button onClick={manualRetry} className="btn-ghost-sm">Retry</button>
          </div>
        )}

        {transcript.map((m) => {
          if (m.role === 'user') {
            return (
              <div key={m.id} className="msg-user">
                <div className="msg-user-bubble">{m.text}</div>
                <div className="msg-user-time">you · {fmtTime(m.ts)}</div>
                <CopyButton className="msg-copy" getText={() => m.text} label="Copy message" />
              </div>
            )
          }
          if (m.role === 'system') {
            // tether#50 — a daemon notice (e.g. "the previous context could not
            // be restored"). Rendered as its own quiet centred line rather than
            // an assistant bubble: it did not come from the model, and dressing
            // it up with the tether avatar would read as the agent saying it.
            return (
              <div key={m.id} className="msg-system">
                <span className="msg-system-text">{m.text}</span>
              </div>
            )
          }
          return (
            <div key={m.id} className="msg-ai">
              <div className="msg-ai-header">
                <span className="msg-ai-avatar">
                  <Icon name="tether" size={10} style={{ color: 'white' }} />
                </span>
                <AnswerMeta ts={m.ts} answerMs={m.answerMs} usage={m.usage} />
                {m.text && <CopyButton className="msg-copy" getText={() => m.text} label="Copy answer" />}
              </div>
              {m.thinking && (
                <ThinkingBlock
                  thinking={m.thinking}
                  thinkingMs={m.thinkingMs}
                  live={streaming && m.id === curTurnId && !m.text}
                  expanded={expandedThinking.has(m.id)}
                  onToggle={() => toggleThinking(m.id)}
                />
              )}
              {m.tools && m.tools.length > 0 && <ToolCallList tools={m.tools} />}
              {m.block && (
                <div className="msg-ai-block">
                  <FencedBlockView
                    block={m.block}
                    expanded={expandedBlocks.has(m.id)}
                    onToggle={() => toggleBlock(m.id)}
                    onApprove={sendApprove}
                    onPause={sendPause}
                  />
                </div>
              )}
              {(m.text || (!m.block && streaming && m.id === streamingMsgId)) && (
                <AnswerBody text={m.text} streaming={streaming && m.id === streamingMsgId} />
              )}
            </div>
          )
        })}

        {showEmpty && (
          <div className="chat-empty mono">message tether to start a session</div>
        )}

        {/* Thinking indicator: animated dots while waiting for the first token —
            suppressed once the turn has a bubble (tether#34: thinking block or
            answer text), since that is itself the "working" signal. */}
        {streaming && !streamingMsgId && !curTurnId && (
          <div className="msg-ai">
            <div className="msg-ai-header">
              <span className="msg-ai-avatar">
                <Icon name="tether" size={10} style={{ color: 'white' }} />
              </span>
              <span className="msg-ai-name">tether</span>
            </div>
            <div className="msg-ai-body">
              <span className="thinking-dots" aria-label="Claude is thinking" />
            </div>
          </div>
        )}

        {pendingPermissions.length > 0 && (
          <PermissionQueue
            requests={pendingPermissions}
            onDecide={(id, allow) => { void postDecide(id, allow); resolvePermission(id) }}
            onDecideAll={(allow) => {
              // Snapshot ids first (resolvePermission mutates the queue as we go).
              for (const id of pendingPermissions.map((p) => p.id)) {
                void postDecide(id, allow)
                resolvePermission(id)
              }
            }}
          />
        )}
      </div>

      {/* ── Composer ──────────────────────────────────────── */}
      <div className="dt-composer">
        {slashOpen && filteredSlash.length > 0 && (
          <div className="slash-pop">
            <div className="slash-head">
              <span className="mono">/ commands</span>
              <span className="kbd">esc</span>
            </div>
            {filteredSlash.map((c, i) => (
              <div
                key={c.cmd}
                className={`slash-row${i === slashIndex ? ' on' : ''}`}
                onMouseEnter={() => setSlashIndex(i)}
                onClick={() => pickSlash(c)}
              >
                <span className="slash-cmd">{c.cmd}</span>
                <span className="slash-desc">{c.desc}</span>
                {i === slashIndex && <span className="kbd">↵</span>}
              </div>
            ))}
          </div>
        )}

        {/* tether#47 — @-mention file picker (reuses .slash-pop styling). */}
        {atOpen && atItems.length > 0 && (
          <div className="slash-pop at-pop">
            <div className="slash-head">
              <span className="mono">@ files{activeWorkspace ? ` · ${activeWorkspace.id.slice(0, 6)}` : ''}</span>
              <span className="kbd">esc</span>
            </div>
            {atItems.map((f, i) => (
              <div
                key={f}
                className={`slash-row${i === atIndex ? ' on' : ''}`}
                onMouseEnter={() => setAtIndex(i)}
                onClick={() => pickAt(f)}
              >
                <span className="slash-cmd at-file">{f}</span>
                {i === atIndex && <span className="kbd">↵</span>}
              </div>
            ))}
            {atTruncated && (
              <div className="at-more mono">workspace has more files — refine your query</div>
            )}
          </div>
        )}

        <div className="composer-box">
          {providers.length > 1 && (
            <div style={{ padding: '0 4px 6px', display: 'flex', alignItems: 'center', gap: 6 }}>
              <span style={{ fontSize: 11, color: 'var(--ink-quat)', fontFamily: 'var(--font-mono)' }}>provider</span>
              <select
                value={selectedProvider}
                onChange={e => setSelectedProvider(e.target.value)}
                style={{ background: 'transparent', color: 'var(--ink-secondary)', border: '1px solid var(--line-soft)', borderRadius: 3, padding: '2px 4px', fontSize: 11, fontFamily: 'var(--font-mono)' }}
              >
                {providers.map(p => <option key={p} value={p}>{p}</option>)}
              </select>
            </div>
          )}
          <div className="composer-row">
            <span className="composer-prefix">/</span>
            <textarea
              ref={taRef}
              rows={1}
              className="composer-input"
              disabled={connState !== 'connected'}
              value={input}
              onChange={e => handleInputChange(e.target.value)}
              onSelect={refreshAtMenu}
              onCompositionStart={() => setIsComposing(true)}
              onCompositionEnd={() => setIsComposing(false)}
              onKeyDown={e => {
                const slashActive = slashOpen && filteredSlash.length > 0
                const atActive = atOpen && atItems.length > 0
                // tether#47 — @-mention menu owns nav keys while open (checked
                // before the slash menu; they can't both be active).
                if (atActive && e.key === 'ArrowDown') {
                  e.preventDefault(); setAtIndex(i => (i + 1) % atItems.length); return
                }
                if (atActive && e.key === 'ArrowUp') {
                  e.preventDefault(); setAtIndex(i => (i - 1 + atItems.length) % atItems.length); return
                }
                if (atActive && (e.key === 'Tab' || e.key === 'Enter') && !isComposing) {
                  e.preventDefault(); pickAt(atItems[Math.min(atIndex, atItems.length - 1)]); return
                }
                if (atActive && e.key === 'Escape') { e.preventDefault(); setAtOpen(false); return }
                if (slashActive && e.key === 'ArrowDown') {
                  e.preventDefault(); setSlashIndex(i => (i + 1) % filteredSlash.length); return
                }
                if (slashActive && e.key === 'ArrowUp') {
                  e.preventDefault(); setSlashIndex(i => (i - 1 + filteredSlash.length) % filteredSlash.length); return
                }
                if (slashActive && (e.key === 'Tab' || e.key === 'Enter') && !isComposing) {
                  e.preventDefault(); pickSlash(filteredSlash[Math.min(slashIndex, filteredSlash.length - 1)]); return
                }
                // tether#46 — Enter sends, Shift+Enter inserts a newline (the
                // textarea handles the newline natively when we don't
                // preventDefault). shouldSendOnEnter also refuses to send during
                // IME composition, while streaming (the button is Stop then —
                // tether#42 review N1), or while a menu (slash/@) is open.
                if (shouldSendOnEnter({ key: e.key, shiftKey: e.shiftKey, isComposing, streaming, slashActive: slashActive || atActive })) {
                  e.preventDefault(); void sendMessage()
                } else if (e.key === 'Enter' && !e.shiftKey && !isComposing && streaming) {
                  // While a turn streams the button is Stop; swallow plain Enter
                  // so it neither sends nor inserts a stray newline (parity with
                  // the old single-line input; tether#46 review MINOR-1).
                  // Shift+Enter still adds a newline for composing the next msg.
                  e.preventDefault()
                }
                if (e.key === 'Escape') { setSlashOpen(false); setAtOpen(false) }
              }}
              placeholder={
                connState !== 'connected'
                  ? connState === 'connecting' ? 'connecting…' : 'not connected'
                  : streaming ? 'Claude is thinking…' : 'message tether…'
              }
            />
            {streaming ? (
              // tether#42 — while a turn streams, the send button becomes a stop
              // button (cc/ChatGPT-style) that interrupts the current turn.
              <button
                type="button"
                className="send-btn stop-btn"
                onClick={() => sendStop()}
                aria-label="Stop generating"
                title="Stop generating"
              >
                <span className="stop-glyph" aria-hidden="true" />
              </button>
            ) : (
              <button
                type="button"
                className="send-btn"
                disabled={connState !== 'connected'}
                onClick={() => void sendMessage()}
                aria-label="Send message"
                title="Send message"
              >
                <Icon name="arrow-up" size={13} />
              </button>
            )}
          </div>
          <div className="composer-foot">
            <span className="mono" style={{ fontSize: 10.5, color: 'var(--ink-tertiary)' }}>↵ send · ⇧↵ newline · / for commands</span>
            {sessionId && (
              <span className="mono" style={{ fontSize: 10.5, color: 'var(--ink-tertiary)', marginLeft: 'auto' }}>
                {selectedProvider}
              </span>
            )}
          </div>
        </div>
      </div>

    </>
  )
}

// fmtThinkMs formats a thinking duration for the collapsed summary: whole
// seconds as "8s", sub-10s with one decimal ("1.2s", "0.5s"), and >=1min as
// "Xm Ys". Empty string for undefined/negative (no duration to show yet).
export function fmtThinkMs(ms: number | undefined): string {
  if (ms == null || ms < 0) return ''
  if (ms < 60000) {
    const s = ms / 1000
    // >= ~10s (incl. 9.95–9.999 that would otherwise render "10.0s") and whole
    // seconds show without a decimal; otherwise one decimal ("1.2s", "0.5s").
    const str = s >= 9.95 || Number.isInteger(s) ? String(Math.round(s)) : s.toFixed(1)
    return `${str}s`
  }
  // Round to whole seconds FIRST, then split — avoids "1m 60s" at the boundary.
  const totalSec = Math.round(ms / 1000)
  return `${Math.floor(totalSec / 60)}m ${totalSec % 60}s`
}

// fmtTokens renders a token count compactly for the usage badge (tether#48):
// under 1k verbatim ("856"), then "k" ("1.2k", "46k"), then "M" ("1.4M").
// BOTH tiers apply fmtThinkMs's decimal rule (>= ~10 of a unit, or a whole
// value, drops the decimal — "10.0k"→"10k"). The k-tier stops at 999_500, not
// 1_000_000: above that, round(n/1000) would hit 1000 and render an ugly
// "1000k", so those roll into the M-tier ("1.0M") instead. With 1M-context
// models, near-1M input counts are realistic, so this seam matters. Empty
// string for undefined/negative.
export function fmtTokens(n: number | undefined): string {
  if (n == null || n < 0) return ''
  if (n < 1000) return String(n)
  if (n < 999_500) {
    const k = n / 1000
    return `${k >= 9.95 || Number.isInteger(k) ? String(Math.round(k)) : k.toFixed(1)}k`
  }
  const m = n / 1_000_000
  return `${m >= 9.95 || Number.isInteger(m) ? String(Math.round(m)) : m.toFixed(1)}M`
}

interface ThinkingBlockProps {
  thinking: string
  thinkingMs?: number
  /** True while this message is still actively accumulating thinking deltas
   *  (it is the store's curTurnId). Goes false the moment the answer starts OR
   *  the turn ends (result/error) — either way the block collapses, so a
   *  thinking-only turn (e.g. thinking → tool_use with no answer text) does not
   *  get stuck showing "thinking…" forever.
   *
   *  "the turn ends (error)" is why the call site conjoins `streaming`
   *  (tether#83). A non-terminal error no longer clears curTurnId — the turn it
   *  lands on may still be streaming — so curTurnId alone stopped answering this
   *  question on that path, and a cc turn killed mid-thinking (ccSession.abandon)
   *  would have sat on "thinking…" until its stream-end result arrived, or
   *  forever if that result were dropped as a slow-subscriber envelope.
   *
   *  It is therefore NOT monotonic within a turn, and tether#88 is where that
   *  became reachable: sending a prompt no longer ends the open turn, and
   *  sendMessage sets `streaming` back to true, so a thinking-only bubble whose
   *  block collapsed on a non-terminal error re-animates when the user types
   *  again. On the path that error actually describes — the turn is still
   *  running, which is tether#83's whole premise — that is the truth: that turn
   *  IS still thinking. It reads wrong only where the pointer is stale, which is
   *  the case store.ts's addMessage enumerates and accepts. */
  live: boolean
  expanded: boolean
  onToggle: () => void
}

// AnswerMeta — assistant bubble header meta (tether#36): name + time, plus an
// answer-duration badge once the turn completes (answerMs is stamped at result).
// Exported as a pure component so ChatPane.test.tsx tests the badge directly
// without mounting ChatPane (WebTransport).
export function AnswerMeta({ ts, answerMs, usage }: {
  ts: number
  answerMs?: number
  /** The turn's token usage (tether#48); renders a "⇅ in↑/out↓" badge when present. */
  usage?: { input: number; output: number }
}) {
  return (
    <>
      <span className="msg-ai-name">tether</span>
      <span className="msg-ai-time">{fmtTime(ts)}</span>
      {answerMs != null && <span className="msg-ai-dur">· {fmtThinkMs(answerMs)}</span>}
      {usage && (
        <span className="msg-ai-usage" title={`${usage.input} input / ${usage.output} output tokens`}>
          · ⇅ {fmtTokens(usage.input)}↑/{fmtTokens(usage.output)}↓
        </span>
      )}
    </>
  )
}

// AnswerBody — assistant answer text rendered as markdown (tether#35). Exported
// as a pure, prop-controlled component so ChatPane.test.tsx tests it directly
// without the WebTransport wiring. While streaming it gets a `.streaming` class;
// index.css paints the blinking cursor via .md-body::after (a block-level markdown
// tree can't host the old inline <span> cursor at the text tail).
export function AnswerBody({ text, streaming }: { text: string; streaming: boolean }) {
  return (
    <div className={streaming ? 'msg-ai-body streaming' : 'msg-ai-body'} aria-busy={streaming}>
      <Markdown text={text} />
    </div>
  )
}

// tether#37 — tool-call visibility. The daemon already forwards each tool_use as
// {name,input} (registry.go translateEvent); the store keeps them on the turn's
// bubble; this renders them as a compact activity log above the answer — one
// line per call: icon + name + a best-effort one-line arg summary. A turn can
// fire 10+ tools, so beyond TOOL_FOLD_THRESHOLD they collapse behind a
// "used N tools" toggle. No tool result (that needs daemon tool_result parsing —
// a later slice).
const TOOL_FOLD_THRESHOLD = 5

// The input field worth showing per known tool; unknown tools show name only.
const TOOL_ARG_FIELD: Record<string, string> = {
  Read: 'file_path', Write: 'file_path', Edit: 'file_path', NotebookEdit: 'notebook_path',
  Bash: 'command', Grep: 'pattern', Glob: 'pattern', Task: 'description',
  WebFetch: 'url', WebSearch: 'query',
}

const TOOL_ICON: Record<string, string> = {
  Read: '📖', Write: '📝', Edit: '✏️', NotebookEdit: '✏️', Bash: '⚡',
  Grep: '🔍', Glob: '🔍', Task: '🧩', WebFetch: '🌐', WebSearch: '🌐',
}

// summarizeToolInput derives the one-line arg summary from a tool_use input
// object. Best-effort + defensive: unknown tools, non-object input, or a missing/
// non-string field all yield '' (the row then shows the tool name alone).
// Exported so ChatPane.test.tsx covers it without rendering.
export function summarizeToolInput(name: string, input: unknown): string {
  if (!input || typeof input !== 'object') return ''
  const field = TOOL_ARG_FIELD[name]
  if (!field) return ''
  const val = (input as Record<string, unknown>)[field]
  if (typeof val !== 'string') return ''
  const s = val.trim().replace(/\s+/g, ' ')
  return s.length > 60 ? s.slice(0, 60) + '…' : s
}

// summarizeToolResult derives the one-line RESULT preview at the tool row tail
// (tether#38): Read/Write/Edit → line count, Grep/Glob → match count, errors → a
// short marker, else the first non-empty output line (truncated). Best-effort +
// defensive; '' when there's nothing useful to preview. Exported for tests.
export function summarizeToolResult(name: string, result: { content: string; isError: boolean }): string {
  if (result.isError) return 'error'
  const c = result.content ?? ''
  if (!c) return ''
  if (name === 'Read' || name === 'Write' || name === 'Edit' || name === 'NotebookEdit') {
    const n = c.replace(/\n+$/, '').split('\n').length
    return n === 1 ? '1 line' : `${n} lines`
  }
  if (name === 'Grep' || name === 'Glob') {
    const n = c.split('\n').filter(l => l.trim()).length
    return n === 1 ? '1 match' : `${n} matches`
  }
  const first = c.split('\n').find(l => l.trim()) ?? ''
  const s = first.trim().replace(/\s+/g, ' ')
  return s.length > 48 ? s.slice(0, 48) + '…' : s
}

const RESULT_MAX_LINES = 20
const RESULT_MAX_CHARS = 2000

// truncateResult clamps the expanded result block so a huge file / long stdout
// can't flood the chat; a trailing marker signals truncation. Exported for tests.
export function truncateResult(s: string): string {
  let out = s
  let cut = false
  if (out.length > RESULT_MAX_CHARS) { out = out.slice(0, RESULT_MAX_CHARS); cut = true }
  const lines = out.split('\n')
  if (lines.length > RESULT_MAX_LINES) { out = lines.slice(0, RESULT_MAX_LINES).join('\n'); cut = true }
  return cut ? out + '\n…(truncated)' : out
}

// ToolCallList — the per-turn tool activity log. Each row shows the call
// (icon + name + arg, tether#37); once its result arrives (tether#38) the row
// also shows a one-line result preview at the tail and becomes clickable to
// expand the full (truncated) result block below it. Exported + prop-controlled
// so ChatPane.test.tsx renders it directly (no WebTransport). List-fold (>5) and
// per-tool result-expand are both local state, not in the store.
export function ToolCallList({ tools }: { tools: ToolCall[] }) {
  const [open, setOpen] = useState(false)
  const [expanded, setExpanded] = useState<Set<string>>(() => new Set())
  const toggle = (key: string) => setExpanded(prev => {
    const next = new Set(prev)
    if (next.has(key)) next.delete(key); else next.add(key)
    return next
  })
  if (tools.length === 0) return null
  const foldable = tools.length > TOOL_FOLD_THRESHOLD
  const rowsHidden = foldable && !open
  return (
    <div className="msg-tools">
      {foldable && (
        <button type="button" className="msg-tool-fold" onClick={() => setOpen(o => !o)} aria-expanded={open}>
          <span className="msg-thinking-chevron">{open ? '⌄' : '›'}</span>
          <span>{open ? `${tools.length} tools` : `used ${tools.length} tools`}</span>
        </button>
      )}
      {!rowsHidden && tools.map((t, i) => {
        const key = t.id || String(i)
        const arg = summarizeToolInput(t.name, t.input)
        const preview = t.result ? summarizeToolResult(t.name, t.result) : ''
        const isOpen = expanded.has(key)
        // Only clickable/expandable when the result has something to show — a
        // present-but-empty result (e.g. a command with no stdout) would be a
        // dead click with a blank block otherwise (review MINOR).
        const hasResult = !!t.result && (t.result.content.length > 0 || t.result.isError)
        return (
          <div key={key}>
            <div
              className={hasResult ? 'msg-tool-row clickable' : 'msg-tool-row'}
              onClick={hasResult ? () => toggle(key) : undefined}
            >
              <span className="msg-tool-icon">{TOOL_ICON[t.name] ?? '🔧'}</span>
              <span className="msg-tool-name">{t.name}</span>
              {arg && <span className="msg-tool-arg">{arg}</span>}
              {preview && (
                <span className={t.result?.isError ? 'msg-tool-preview err' : 'msg-tool-preview'}>{preview}</span>
              )}
              {hasResult && <span className="msg-tool-caret">{isOpen ? '⌄' : '▸'}</span>}
            </div>
            {hasResult && isOpen && (
              <pre className={t.result!.isError ? 'msg-tool-result err' : 'msg-tool-result'}>{truncateResult(t.result!.content)}</pre>
            )}
          </div>
        )
      })}
    </div>
  )
}

// Extended-thinking display (tether#34). While thinking is live it renders
// expanded ("thinking…"); once it stops being live (answer began, or turn ended)
// it collapses to a one-line "thought Xs" summary that clicking re-expands.
// Exported and prop-controlled so it unit-tests directly, without the ChatPane
// WebTransport wiring.
export function ThinkingBlock({ thinking, thinkingMs, live, expanded, onToggle }: ThinkingBlockProps) {
  if (live) {
    return (
      <div className="msg-thinking msg-thinking-live">
        <div className="msg-thinking-label">thinking…</div>
        <div className="msg-thinking-text"><Markdown text={thinking} /></div>
      </div>
    )
  }
  const dur = fmtThinkMs(thinkingMs)
  return (
    <div className="msg-thinking msg-thinking-done">
      <button type="button" className="msg-thinking-toggle" onClick={onToggle} aria-expanded={expanded}>
        <span className="msg-thinking-chevron">{expanded ? '⌄' : '›'}</span>
        <span className="msg-thinking-summary">thought{dur ? ` ${dur}` : ''}</span>
      </button>
      {expanded && <div className="msg-thinking-text"><Markdown text={thinking} /></div>}
    </div>
  )
}

interface FencedBlockViewProps {
  block: FencedBlock
  expanded: boolean
  onToggle: () => void
  /** D-19 §5 approve callback (tether#8 T8); only 'dag' wires it so far. */
  onApprove: (block: FencedBlock) => void
  /** D-19 §5 pause callback (tether#8 T9); only 'dag' wires it so far. */
  onPause: (block: FencedBlock) => void
}

// Dispatch a FencedBlock to its renderer by `kind` (D-19 §10.B.4).
// Unknown kinds fall back to a compact raw view rather than throwing.
function FencedBlockView({ block, expanded, onToggle, onApprove, onPause }: FencedBlockViewProps) {
  switch (block.kind) {
    case 'dag':        return <DagBlock block={block} expanded={expanded} onToggle={onToggle} onApprove={() => onApprove(block)} onPause={() => onPause(block)} />
    case 'form':       return <FormBlock block={block} expanded={expanded} onToggle={onToggle} />
    case 'candidates': return <CandidatesBlock block={block} expanded={expanded} onToggle={onToggle} />
    case 'media':      return <MediaBlock block={block} expanded={expanded} onToggle={onToggle} />
    default:
      return <div className="fb-fallback mono">unknown block: {block.kind}</div>
  }
}
