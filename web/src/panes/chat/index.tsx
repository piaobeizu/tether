import { useState } from 'react'
import { useStore } from '../../lib/store'
import type { FencedBlock } from '../../lib/wire.gen'
import { DagBlock } from '../../fenced-blocks/DagBlock'
import { FormBlock } from '../../fenced-blocks/FormBlock'
import { CandidatesBlock } from '../../fenced-blocks/CandidatesBlock'
import { MediaBlock } from '../../fenced-blocks/MediaBlock'
import { PermissionBlock } from '../../fenced-blocks/PermissionBlock'

export default function ChatPane() {
  const { messages, sessionId, pendingPermission } = useStore()
  const [input, setInput] = useState('')

  const sendMessage = () => {
    if (!input.trim()) return
    useStore.getState().addMessage({
      id: crypto.randomUUID(),
      role: 'user',
      text: input,
      ts: Date.now(),
    })
    setInput('')
    // actual send via wt.ts bidi stream — wired in s4
  }

  return (
    <>
      <div className="pane-header" style={{ display: 'flex', justifyContent: 'space-between' }}>
        <span>Chat</span>
        {sessionId && <span style={{ fontFamily: 'monospace', fontSize: 10 }}>{sessionId.slice(0, 8)}</span>}
      </div>
      <div className="pane-body" style={{ display: 'flex', flexDirection: 'column', gap: 8, paddingBottom: 0 }}>
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
          <input
            style={{ flex: 1, background: '#1a1a1a', border: '1px solid #333', borderRadius: 4, padding: '6px 10px', color: '#e8e8e8', outline: 'none' }}
            value={input}
            onChange={(e) => setInput(e.target.value)}
            onKeyDown={(e) => e.key === 'Enter' && !e.shiftKey && sendMessage()}
            placeholder="Message…"
          />
          <button
            onClick={sendMessage}
            style={{ background: '#2a2a2a', border: '1px solid #444', borderRadius: 4, padding: '6px 14px', color: '#e8e8e8', cursor: 'pointer' }}
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
