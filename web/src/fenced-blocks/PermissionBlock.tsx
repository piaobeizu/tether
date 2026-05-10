import { useStore } from '../lib/store'

interface Props {
  toolName: string
  input: unknown
  requestId: string
}

// PermissionBlock — PreToolUse hook callback UI (D-05b §6.2).
// Allow/Deny buttons POST to /api/v1/agent/permission/<id>/decide.
export function PermissionBlock({ toolName, input, requestId }: Props) {
  const decide = async (allow: boolean) => {
    try {
      await fetch(`/api/v1/agent/permission/${requestId}/decide`, {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ allow, remember: false }),
      })
    } finally {
      useStore.getState().setPendingPermission(null)
    }
  }

  return (
    <div style={{ background: '#1e1208', border: '1px solid #5c3a00', borderRadius: 6, padding: 12, marginBottom: 8 }}>
      <div style={{ fontSize: 12, color: '#e8a040', marginBottom: 6 }}>
        Tool request: <strong>{toolName}</strong>
      </div>
      <pre style={{ fontSize: 11, color: '#bbb', marginBottom: 10, maxHeight: 120, overflow: 'auto', background: '#111', padding: 8, borderRadius: 4 }}>
        {JSON.stringify(input, null, 2)}
      </pre>
      <div style={{ display: 'flex', gap: 8 }}>
        <button
          onClick={() => decide(true)}
          style={{ background: '#1a3a1a', border: '1px solid #2e7d32', borderRadius: 4, padding: '5px 14px', color: '#81c784', cursor: 'pointer', fontSize: 12 }}
        >
          Allow
        </button>
        <button
          onClick={() => decide(false)}
          style={{ background: '#3a1a1a', border: '1px solid #7d2e2e', borderRadius: 4, padding: '5px 14px', color: '#e57373', cursor: 'pointer', fontSize: 12 }}
        >
          Deny
        </button>
      </div>
    </div>
  )
}
