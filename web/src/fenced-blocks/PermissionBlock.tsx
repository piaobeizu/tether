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
    <div className="perm-block">
      <div className="perm-title">
        Tool request: <strong>{toolName}</strong>
      </div>
      <pre className="perm-input">
        {JSON.stringify(input, null, 2)}
      </pre>
      <div className="perm-actions">
        <button className="perm-btn perm-btn-allow" onClick={() => decide(true)}>
          Allow
        </button>
        <button className="perm-btn perm-btn-deny" onClick={() => decide(false)}>
          Deny
        </button>
      </div>
    </div>
  )
}
