import type { PermissionRequest } from '../lib/store'

// postDecide — POST a single approve/deny decision (D-05b §6.2). Keyed by
// requestId, so parallel requests are each decided independently (tether#40).
// The /decide endpoint stays cookie-gated (tether#39), so this same-origin fetch
// carries the auth cookie. Exported so the bulk 全部批准/全部拒绝 toolbar reuses it.
export async function postDecide(requestId: string, allow: boolean): Promise<void> {
  await fetch(`/api/v1/agent/permission/${requestId}/decide`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ allow, remember: false }),
  })
}

interface BlockProps {
  toolName: string
  input: unknown
  onDecide: (allow: boolean) => void
}

// PermissionBlock — one PreToolUse request. Pure: the parent owns the POST +
// queue removal via onDecide, so it renders in jsdom without a store/network
// (tether#40 made it prop-controlled for testability).
export function PermissionBlock({ toolName, input, onDecide }: BlockProps) {
  return (
    <div className="perm-block">
      <div className="perm-title">
        Tool request: <strong>{toolName}</strong>
      </div>
      <pre className="perm-input">
        {JSON.stringify(input, null, 2)}
      </pre>
      <div className="perm-actions">
        <button type="button" className="perm-btn perm-btn-allow" onClick={() => onDecide(true)}>
          Allow
        </button>
        <button type="button" className="perm-btn perm-btn-deny" onClick={() => onDecide(false)}>
          Deny
        </button>
      </div>
    </div>
  )
}

interface QueueProps {
  requests: PermissionRequest[]
  /** Decide one request by id (parent does postDecide + resolvePermission). */
  onDecide: (id: string, allow: boolean) => void
  /** Decide every queued request at once (全部批准/全部拒绝). */
  onDecideAll: (allow: boolean) => void
}

// PermissionQueue — renders ALL pending permission requests (tether#40). A single
// request shows just its block (keeps the minimal look); two or more add a header
// with the count and 全部批准/全部拒绝 bulk shortcuts. Each block decides
// independently by id, so a parallel-tool turn no longer clobbers all-but-one
// request into a timeout (the old single-slot pendingPermission bug).
export function PermissionQueue({ requests, onDecide, onDecideAll }: QueueProps) {
  if (requests.length === 0) return null
  return (
    <div className="perm-queue">
      {requests.length > 1 && (
        <div className="perm-queue-head">
          <span className="perm-queue-count">Tool requests ({requests.length})</span>
          <span className="perm-queue-bulk">
            <button type="button" className="perm-btn perm-btn-allow" onClick={() => onDecideAll(true)}>
              全部批准
            </button>
            <button type="button" className="perm-btn perm-btn-deny" onClick={() => onDecideAll(false)}>
              全部拒绝
            </button>
          </span>
        </div>
      )}
      {requests.map((r) => (
        <PermissionBlock
          key={r.id}
          toolName={r.toolName}
          input={r.input}
          onDecide={(allow) => onDecide(r.id, allow)}
        />
      ))}
    </div>
  )
}
