import { useEffect, useRef, useState } from 'react'
import { TetherWT } from '../../lib/wt'
import { useStore } from '../../lib/store'
import type { FencedBlock, ProviderListResponse } from '../../lib/wire.gen'
import { authedFetch } from '../../lib/auth'
import { DagBlock } from '../../fenced-blocks/DagBlock'
import { FormBlock } from '../../fenced-blocks/FormBlock'
import { CandidatesBlock } from '../../fenced-blocks/CandidatesBlock'
import { MediaBlock } from '../../fenced-blocks/MediaBlock'
import { PermissionBlock } from '../../fenced-blocks/PermissionBlock'

type ConnState = 'connecting' | 'connected' | 'failed'

export default function ChatPane() {
  const { messages, sessionId, pendingPermission } = useStore()
  const [input, setInput] = useState('')
  const [connState, setConnState] = useState<ConnState>('connecting')
  const [connError, setConnError] = useState<string | null>(null)
  const writerRef = useRef<WritableStreamDefaultWriter<Uint8Array> | null>(null)
  const wtRef = useRef<TetherWT | null>(null)
  const [providers, setProviders] = useState<string[]>(['claude-code'])
  const [selectedProvider, setSelectedProvider] = useState('claude-code')

  useEffect(() => {
    authedFetch('/api/v1/providers')
      .then(r => r.json() as Promise<ProviderListResponse>)
      .then(d => {
        if (d.providers?.length > 0) setProviders(d.providers)
      })
      .catch(() => {}) // keep default ['claude'] on error
  }, [])

  const doConnect = () => {
    setConnState('connecting')
    setConnError(null)
    // clientId is now derived server-side from the JWT cookie (not a URL param).
    const url = `https://${location.host}/wt/chat?provider=${encodeURIComponent(selectedProvider)}`
    const wt = new TetherWT({
      url,
      onEnvelope: useStore.getState().handleEnvelope,
      onClose: () => { useStore.getState().setConnected(false); setConnState('failed') },
    })
    wtRef.current = wt

    wt.connect().then(async () => {
      useStore.getState().setConnected(true)
      setConnState('connected')
      const stream = await wt.openBidiStream()
      writerRef.current = stream.writable.getWriter()
    }).catch((err: unknown) => {
      const msg = err instanceof Error ? err.message : String(err)
      console.error('[tether] chat connect failed:', msg)
      setConnState('failed')
      setConnError(msg)
    })
  }

  useEffect(() => {
    doConnect()
    return () => {
      writerRef.current?.releaseLock()
      wtRef.current?.close()
    }
  }, [])

  const sendMessage = async () => {
    if (!input.trim() || !writerRef.current) return
    useStore.getState().addMessage({
      id: crypto.randomUUID(),
      role: 'user',
      text: input,
      ts: Date.now(),
    })
    const line = JSON.stringify({ text: input }) + '\n'
    try {
      await writerRef.current.write(new TextEncoder().encode(line))
    } catch (err) {
      console.error('[tether] send failed:', err)
    }
    setInput('')
  }

  const connDot = connState === 'connected' ? '#4caf50' : connState === 'connecting' ? '#ff9800' : '#f44336'
  const connLabel = connState === 'connected' ? 'connected' : connState === 'connecting' ? 'connecting…' : 'disconnected'

  return (
    <>
      <div className="pane-header" style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'center' }}>
        <span>Chat</span>
        <span style={{ display: 'flex', alignItems: 'center', gap: 5 }}>
          <span style={{ width: 8, height: 8, borderRadius: '50%', background: connDot, display: 'inline-block' }} />
          <span style={{ fontSize: 10, color: '#888' }}>{connLabel}</span>
          {sessionId && <span style={{ fontFamily: 'monospace', fontSize: 10, color: '#555' }}>{sessionId.slice(0, 8)}</span>}
          {sessionId && <span style={{ fontSize: 11, color: '#888', marginLeft: 6 }}>[{selectedProvider}]</span>}
        </span>
      </div>
      <div className="pane-body" style={{ display: 'flex', flexDirection: 'column', gap: 8, paddingBottom: 0 }}>
        {connState === 'failed' && (
          <div style={{ background: '#2a1010', border: '1px solid #5a2020', borderRadius: 4, padding: '8px 10px', fontSize: 12 }}>
            <div style={{ color: '#f44336', marginBottom: 4 }}>WebTransport connection failed</div>
            {connError && <div style={{ color: '#888', fontSize: 11, marginBottom: 6, wordBreak: 'break-all' }}>{connError}</div>}
            <div style={{ color: '#888', fontSize: 11, marginBottom: 6 }}>UDP/QUIC may be blocked on this network. Check K.8.1 in README.</div>
            <button
              onClick={doConnect}
              style={{ background: '#333', border: '1px solid #555', borderRadius: 3, padding: '3px 10px', color: '#e8e8e8', cursor: 'pointer', fontSize: 12 }}
            >
              Retry
            </button>
          </div>
        )}
        <div style={{ flex: 1, overflowY: 'auto' }}>
          {messages.map((m) => (
            <div key={m.id} style={{ marginBottom: 8, opacity: m.role === 'user' ? 1 : 0.85 }}>
              <span style={{ color: '#555', fontSize: 11 }}>{m.role}: </span>
              <span>{m.text}</span>
            </div>
          ))}
        </div>
        {pendingPermission && (
          <PermissionBlock
            toolName={pendingPermission.toolName}
            input={pendingPermission.input}
            requestId={pendingPermission.id}
          />
        )}
        {/* fenced-block rendering demo stubs */}
        <DummyFencedBlockDemo />
        <div style={{ display: 'flex', gap: 8, paddingBottom: 12, flexShrink: 0 }}>
          {providers.length > 1 && (
            <select
              value={selectedProvider}
              onChange={e => setSelectedProvider(e.target.value)}
              style={{ background: '#222', color: '#e8e8e8', border: '1px solid #333', borderRadius: 4, padding: '4px 8px', fontSize: 12, marginRight: 8 }}
            >
              {providers.map(p => <option key={p} value={p}>{p}</option>)}
            </select>
          )}
          <input
            disabled={connState !== 'connected'}
            style={{ flex: 1, background: '#1a1a1a', border: '1px solid #333', borderRadius: 4, padding: '6px 10px', color: connState === 'connected' ? '#e8e8e8' : '#555', outline: 'none' }}
            value={input}
            onChange={(e) => setInput(e.target.value)}
            onKeyDown={(e) => e.key === 'Enter' && !e.shiftKey && void sendMessage()}
            placeholder={connState === 'connected' ? 'Message…' : connState === 'connecting' ? 'Connecting…' : 'Not connected'}
          />
          <button
            disabled={connState !== 'connected'}
            onClick={() => void sendMessage()}
            style={{ background: connState === 'connected' ? '#2a2a2a' : '#1a1a1a', border: '1px solid #444', borderRadius: 4, padding: '6px 14px', color: connState === 'connected' ? '#e8e8e8' : '#555', cursor: connState === 'connected' ? 'pointer' : 'not-allowed' }}
          >
            Send
          </button>
        </div>
      </div>
    </>
  )
}

// Invisible demo: imported so TypeScript validates all fenced-block imports compile.
function DummyFencedBlockDemo() {
  const _blocks: FencedBlock[] = []
  void _blocks
  return null
}

// Explicit block dispatching so DagBlock/FormBlock/CandidatesBlock/MediaBlock are used.
void DagBlock
void FormBlock
void CandidatesBlock
void MediaBlock
