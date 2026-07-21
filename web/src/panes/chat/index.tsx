import { useEffect, useRef, useState } from 'react'
import { TetherWT } from '../../lib/wt'
import { ControlClient } from '../../lib/control'
import { useStore, historyEntryToMessage, type HistoryEntry, type ToolCall } from '../../lib/store'
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

type ConnState = 'connecting' | 'connected' | 'reconnecting' | 'failed'

const RECONNECT_BASE_MS = 1_000
const RECONNECT_MAX_MS = 16_000
const RECONNECT_MAX_ATTEMPTS = 5

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

interface Props {
  onMenuClick?: () => void
}

export default function ChatPane({ onMenuClick: _onMenuClick }: Props) {
  const { messages, sessionId, pendingPermissions, resolvePermission, streaming, streamingMsgId, curTurnId } = useStore()
  const [input, setInput] = useState('')
  const [connState, setConnState] = useState<ConnState>('connecting')
  const [connError, setConnError] = useState<string | null>(null)
  const [reconnectIn, setReconnectIn] = useState(0)
  const [sessionStart, setSessionStart] = useState<number | null>(null)
  const [_elapsed, setElapsed] = useState('')
  const [slashOpen, setSlashOpen] = useState(false)
  const [slashIndex, setSlashIndex] = useState(0)
  const [isComposing, setIsComposing] = useState(false)
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
  // starts it collapses to a one-line "思考 Xs" summary that this Set re-opens.
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
  const reconnectTimerRef = useRef<ReturnType<typeof setTimeout> | null>(null)
  const countdownRef = useRef<ReturnType<typeof setInterval> | null>(null)
  const unmountedRef = useRef(false)
  const chatRef = useRef<HTMLDivElement>(null)
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
  const lastMsgText = messages.length > 0 ? messages[messages.length - 1].text : ''
  useEffect(() => {
    const el = chatRef.current
    if (!el) return
    // Always scroll during streaming; otherwise only if already near bottom
    const nearBottom = el.scrollHeight - el.scrollTop - el.clientHeight < 120
    if (streaming || nearBottom) {
      el.scrollTop = el.scrollHeight
    }
  }, [messages.length, lastMsgText, streaming])

  // Empty-state hint, debounced so it doesn't flash on session resume before
  // history arrives (connState flips to 'connected' before /messages loads).
  useEffect(() => {
    const empty = messages.length === 0 && connState === 'connected' && !streaming && pendingPermissions.length === 0
    if (!empty) { setShowEmpty(false); return }
    const t = setTimeout(() => setShowEmpty(true), 500)
    return () => clearTimeout(t)
  }, [messages.length, connState, streaming, pendingPermissions.length])

  const cancelPendingReconnect = () => {
    if (reconnectTimerRef.current !== null) { clearTimeout(reconnectTimerRef.current); reconnectTimerRef.current = null }
    if (countdownRef.current !== null) { clearInterval(countdownRef.current); countdownRef.current = null }
  }

  const scheduleReconnect = () => {
    if (unmountedRef.current) return
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
    cancelPendingReconnect()
    setConnState('connecting')
    setConnError(null)
    useStore.getState().setConnection({ state: 'connecting' })

    const old = wtRef.current
    wtRef.current = null
    writerRef.current?.releaseLock()
    writerRef.current = null
    old?.close()

    controlRef.current?.stop()
    controlRef.current = null

    // Resume last session if available — keeps history consistent across refreshes.
    const lastSid = localStorage.getItem('tether_last_sid') ?? ''
    const url = `https://${location.host}/wt/chat?provider=${encodeURIComponent(selectedProvider)}${lastSid ? `&sid=${encodeURIComponent(lastSid)}` : ''}`
    const wt = new TetherWT({
      url,
      onEnvelope: useStore.getState().handleEnvelope,
      onClose: () => {
        useStore.getState().setConnected(false)
        controlRef.current?.stop()
        controlRef.current = null
        if (!unmountedRef.current) scheduleReconnect()
      },
    })
    wtRef.current = wt

    wt.connect().then(async () => {
      attemptRef.current = 0
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
      if (!unmountedRef.current) scheduleReconnect()
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

  useEffect(() => {
    unmountedRef.current = false
    doConnect()
    return () => {
      unmountedRef.current = true
      cancelPendingReconnect()
      writerRef.current?.releaseLock()
      wtRef.current?.close()
      controlRef.current?.stop()
      controlRef.current = null
    }
  }, [])

  const sendMessage = async () => {
    const text = input.trim()
    if (!text || !writerRef.current) return
    setSlashOpen(false)
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

  // T12 click-to-work — switch to an existing session (e.g. a wi already
  // being driven by another tab/browser visit): persist it as the "last
  // session", load its history the same way WorkspacePane's session list
  // does, and reconnect the WT so it resumes that sid.
  const switchSession = (sid: string) => {
    if (!sid) return
    localStorage.setItem('tether_last_sid', sid)
    useStore.getState().setSessionId(sid)
    fetch(`/api/v1/sessions/${encodeURIComponent(sid)}/messages`)
      .then(r => r.ok ? r.json() : [])
      .then((msgs: HistoryEntry[]) => {
        if (msgs.length > 0) useStore.getState().loadHistory(msgs.map(historyEntryToMessage))
      })
      .catch(() => {})
    manualRetryRef.current()
  }

  const switchSessionRef = useRef(switchSession)
  switchSessionRef.current = switchSession

  useEffect(() => {
    const onSwitchSession = (e: Event) => switchSessionRef.current((e as CustomEvent<string>).detail)
    window.addEventListener('tether:switch-session', onSwitchSession)
    return () => window.removeEventListener('tether:switch-session', onSwitchSession)
  }, [])

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
  }

  const filteredSlash = SLASH_CMDS.filter(c => c.cmd.startsWith(input.split(' ')[0]))

  const pickSlash = (c: { cmd: string }) => {
    setInput(c.cmd + ' ')
    setSlashOpen(false)
    setSlashIndex(0)
  }

  return (
    <>
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
            <div style={{ color: 'var(--danger)', fontWeight: 600, marginBottom: 4 }}>WebTransport connection failed</div>
            {connError && <div style={{ color: 'var(--ink-tertiary)', fontSize: 11, marginBottom: 6, wordBreak: 'break-all' }}>{connError}</div>}
            <div style={{ color: 'var(--ink-tertiary)', fontSize: 11, marginBottom: 8 }}>UDP/QUIC may be blocked — see K.8.1 in README.</div>
            <button onClick={manualRetry} className="btn-ghost-sm">Retry</button>
          </div>
        )}

        {messages.map((m) => {
          if (m.role === 'user') {
            return (
              <div key={m.id} className="msg-user">
                <div className="msg-user-bubble">{m.text}</div>
                <div className="msg-user-time">you · {fmtTime(m.ts)}</div>
                <CopyButton className="msg-copy" getText={() => m.text} label="Copy message" />
              </div>
            )
          }
          return (
            <div key={m.id} className="msg-ai">
              <div className="msg-ai-header">
                <span className="msg-ai-avatar">
                  <Icon name="tether" size={10} style={{ color: 'white' }} />
                </span>
                <AnswerMeta ts={m.ts} answerMs={m.answerMs} />
                {m.text && <CopyButton className="msg-copy" getText={() => m.text} label="Copy answer" />}
              </div>
              {m.thinking && (
                <ThinkingBlock
                  thinking={m.thinking}
                  thinkingMs={m.thinkingMs}
                  live={m.id === curTurnId && !m.text}
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
            <input
              className="composer-input"
              disabled={connState !== 'connected'}
              value={input}
              onChange={e => handleInputChange(e.target.value)}
              onCompositionStart={() => setIsComposing(true)}
              onCompositionEnd={() => setIsComposing(false)}
              onKeyDown={e => {
                const slashActive = slashOpen && filteredSlash.length > 0
                if (slashActive && e.key === 'ArrowDown') {
                  e.preventDefault(); setSlashIndex(i => (i + 1) % filteredSlash.length); return
                }
                if (slashActive && e.key === 'ArrowUp') {
                  e.preventDefault(); setSlashIndex(i => (i - 1 + filteredSlash.length) % filteredSlash.length); return
                }
                if (slashActive && (e.key === 'Tab' || e.key === 'Enter') && !isComposing) {
                  e.preventDefault(); pickSlash(filteredSlash[Math.min(slashIndex, filteredSlash.length - 1)]); return
                }
                // Don't send while a turn is streaming (tether#42 review N1):
                // the button is a Stop button then, so Enter must not send a new
                // message (which would reset curTurnId and drop the streaming
                // turn's answerMs badge). Use the Stop button to interrupt.
                if (e.key === 'Enter' && !e.shiftKey && !isComposing && !streaming) { e.preventDefault(); void sendMessage() }
                if (e.key === 'Escape') setSlashOpen(false)
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
            <span className="mono" style={{ fontSize: 10.5, color: 'var(--ink-tertiary)' }}>↵ send · / for commands</span>
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

interface ThinkingBlockProps {
  thinking: string
  thinkingMs?: number
  /** True while this message is still actively accumulating thinking deltas
   *  (it is the store's thinkingBufId). Goes false the moment the answer starts
   *  OR the turn ends (result/error) — either way the block collapses, so a
   *  thinking-only turn (e.g. thinking → tool_use with no answer text) does not
   *  get stuck showing "思考中…" forever. */
  live: boolean
  expanded: boolean
  onToggle: () => void
}

// AnswerMeta — assistant bubble header meta (tether#36): name + time, plus an
// answer-duration badge once the turn completes (answerMs is stamped at result).
// Exported as a pure component so ChatPane.test.tsx tests the badge directly
// without mounting ChatPane (WebTransport).
export function AnswerMeta({ ts, answerMs }: { ts: number; answerMs?: number }) {
  return (
    <>
      <span className="msg-ai-name">tether</span>
      <span className="msg-ai-time">{fmtTime(ts)}</span>
      {answerMs != null && <span className="msg-ai-dur">· {fmtThinkMs(answerMs)}</span>}
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
// "用了 N 个工具" toggle. No tool result (that needs daemon tool_result parsing —
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
  if (result.isError) return '出错'
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
  return cut ? out + '\n…（已截断）' : out
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
          <span>{open ? `${tools.length} 个工具` : `用了 ${tools.length} 个工具`}</span>
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
// expanded ("思考中…"); once it stops being live (answer began, or turn ended)
// it collapses to a one-line "思考 Xs" summary that clicking re-expands.
// Exported and prop-controlled so it unit-tests directly, without the ChatPane
// WebTransport wiring.
export function ThinkingBlock({ thinking, thinkingMs, live, expanded, onToggle }: ThinkingBlockProps) {
  if (live) {
    return (
      <div className="msg-thinking msg-thinking-live">
        <div className="msg-thinking-label">思考中…</div>
        <div className="msg-thinking-text"><Markdown text={thinking} /></div>
      </div>
    )
  }
  const dur = fmtThinkMs(thinkingMs)
  return (
    <div className="msg-thinking msg-thinking-done">
      <button type="button" className="msg-thinking-toggle" onClick={onToggle} aria-expanded={expanded}>
        <span className="msg-thinking-chevron">{expanded ? '⌄' : '›'}</span>
        <span className="msg-thinking-summary">思考{dur ? ` ${dur}` : ''}</span>
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
