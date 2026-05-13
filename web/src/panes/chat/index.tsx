import { useEffect, useRef, useState } from 'react'
import { TetherWT } from '../../lib/wt'
import { useStore } from '../../lib/store'
import { Icon } from '../../lib/icons'
import type { FencedBlock, ProviderListResponse } from '../../lib/wire.gen'
import { authedFetch } from '../../lib/auth'
import { DagBlock } from '../../fenced-blocks/DagBlock'
import { FormBlock } from '../../fenced-blocks/FormBlock'
import { CandidatesBlock } from '../../fenced-blocks/CandidatesBlock'
import { MediaBlock } from '../../fenced-blocks/MediaBlock'
import { PermissionBlock } from '../../fenced-blocks/PermissionBlock'

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

export default function ChatPane({ onMenuClick }: Props) {
  const { messages, sessionId, pendingPermission, streaming } = useStore()
  const [input, setInput] = useState('')
  const [connState, setConnState] = useState<ConnState>('connecting')
  const [connError, setConnError] = useState<string | null>(null)
  const [reconnectIn, setReconnectIn] = useState(0)
  const [sessionStart, setSessionStart] = useState<number | null>(null)
  const [elapsed, setElapsed] = useState('')
  const [slashOpen, setSlashOpen] = useState(false)
  const writerRef = useRef<WritableStreamDefaultWriter<Uint8Array> | null>(null)
  const wtRef = useRef<TetherWT | null>(null)
  const attemptRef = useRef(0)
  const reconnectTimerRef = useRef<ReturnType<typeof setTimeout> | null>(null)
  const countdownRef = useRef<ReturnType<typeof setInterval> | null>(null)
  const unmountedRef = useRef(false)
  const chatRef = useRef<HTMLDivElement>(null)
  const [providers, setProviders] = useState<string[]>(['claude-code'])
  const [selectedProvider, setSelectedProvider] = useState('claude-code')

  useEffect(() => {
    authedFetch('/api/v1/providers')
      .then(r => r.json() as Promise<ProviderListResponse>)
      .then(d => { if (d.providers?.length > 0) setProviders(d.providers) })
      .catch(() => {})
  }, [])

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

  useEffect(() => {
    if (chatRef.current) chatRef.current.scrollTop = chatRef.current.scrollHeight
  }, [messages.length, streaming])

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

    const url = `https://${location.host}/wt/chat?provider=${encodeURIComponent(selectedProvider)}`
    const wt = new TetherWT({
      url,
      onEnvelope: useStore.getState().handleEnvelope,
      onClose: () => {
        useStore.getState().setConnected(false)
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
    }).catch((err: unknown) => {
      const msg = err instanceof Error ? err.message : String(err)
      console.error('[tether] chat connect failed:', msg)
      setConnError(msg)
      if (!unmountedRef.current) scheduleReconnect()
    })
  }

  const manualRetry = () => { attemptRef.current = 0; doConnect() }

  useEffect(() => {
    unmountedRef.current = false
    doConnect()
    return () => {
      unmountedRef.current = true
      cancelPendingReconnect()
      writerRef.current?.releaseLock()
      wtRef.current?.close()
    }
  }, [])

  const sendMessage = async () => {
    const text = input.trim()
    if (!text || !writerRef.current) return
    setSlashOpen(false)
    useStore.getState().addMessage({ id: crypto.randomUUID(), role: 'user', text, ts: Date.now() })
    const line = JSON.stringify({ text }) + '\n'
    try { await writerRef.current.write(new TextEncoder().encode(line)) } catch (err) { console.error('[tether] send failed:', err) }
    setInput('')
  }

  const handleInputChange = (v: string) => {
    setInput(v)
    setSlashOpen(v.startsWith('/') && v.length > 0)
  }

  const filteredSlash = SLASH_CMDS.filter(c => c.cmd.startsWith(input.split(' ')[0]))

  return (
    <>
      {/* ── Right-pane header ─────────────────────────────── */}
      <div className="dt-right-head">
        {onMenuClick && (
          <button className="icon-btn m-only" onClick={onMenuClick}>
            <Icon name="menu" size={16} />
          </button>
        )}
        <span className="section-label">CHAT</span>
        <span style={{ flex: 1 }} />
        {elapsed && <span className="pill">session · {elapsed}</span>}
        <span style={{ display: 'flex', alignItems: 'center', gap: 5 }}>
          <span className={`dot${connState === 'connected' ? ' live' : ''}`} />
          <span style={{ fontSize: 10, color: 'var(--ink-tertiary)', fontFamily: 'var(--font-mono)' }}>
            {connState === 'connected' ? 'live' :
             connState === 'connecting' ? 'connecting…' :
             connState === 'reconnecting' ? `reconnecting` : 'dropped'}
          </span>
        </span>
      </div>

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
              </div>
            )
          }
          return (
            <div key={m.id} className="msg-ai">
              <div className="msg-ai-header">
                <span className="msg-ai-avatar">
                  <Icon name="tether" size={10} style={{ color: 'white' }} />
                </span>
                <span className="msg-ai-name">tether</span>
                <span className="msg-ai-time">{fmtTime(m.ts)}</span>
              </div>
              <div className="msg-ai-body">{m.text}</div>
            </div>
          )
        })}

        {streaming && (
          <div className="msg-ai">
            <div className="msg-ai-header">
              <span className="msg-ai-avatar">
                <Icon name="tether" size={10} style={{ color: 'white' }} />
              </span>
              <span className="msg-ai-name">tether</span>
            </div>
            <div className="msg-ai-body">
              <span className="tether-cursor" aria-label="Claude is responding" />
            </div>
          </div>
        )}

        {pendingPermission && (
          <PermissionBlock
            toolName={pendingPermission.toolName}
            input={pendingPermission.input}
            requestId={pendingPermission.id}
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
                className={`slash-row${i === 0 ? ' on' : ''}`}
                onClick={() => { setInput(c.cmd + ' '); setSlashOpen(false) }}
              >
                <span className="slash-cmd">{c.cmd}</span>
                <span className="slash-desc">{c.desc}</span>
                <span className="kbd">↵</span>
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
              disabled={connState !== 'connected' || streaming}
              value={input}
              onChange={e => handleInputChange(e.target.value)}
              onKeyDown={e => {
                if (e.key === 'Enter' && !e.shiftKey) { e.preventDefault(); void sendMessage() }
                if (e.key === 'Escape') setSlashOpen(false)
              }}
              placeholder={
                connState !== 'connected'
                  ? connState === 'connecting' ? 'connecting…' : 'not connected'
                  : streaming ? 'Claude is thinking…' : 'message tether…'
              }
            />
            <button
              className="send-btn"
              disabled={connState !== 'connected' || streaming}
              onClick={() => void sendMessage()}
            >
              <Icon name="arrow-up" size={13} />
            </button>
          </div>
          <div className="composer-foot">
            <span className="mono" style={{ fontSize: 10.5, color: 'var(--ink-tertiary)' }}>⌘↵ send · shift+↵ newline</span>
            {sessionId && (
              <span className="mono" style={{ fontSize: 10.5, color: 'var(--ink-tertiary)', marginLeft: 'auto' }}>
                {selectedProvider}
              </span>
            )}
          </div>
        </div>
      </div>

      {/* Validate fenced-block imports compile */}
      <DummyFencedBlockDemo />
    </>
  )
}

function DummyFencedBlockDemo() {
  const _: FencedBlock[] = []
  void _
  return null
}
void DagBlock; void FormBlock; void CandidatesBlock; void MediaBlock
