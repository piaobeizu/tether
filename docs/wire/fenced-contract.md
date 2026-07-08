# D-19 Fenced-Block Contract (tether#8, DAG lane)

The cross-repo interface for tether's structured-output "fenced blocks" (DAG progress,
forms, etc.). **tether** (daemon + web) parses the fence markers and renders the blocks;
an **emitter** (a polyforge skill in `polyforge-coding`, running inside the agent) produces
them. This document is the contract both sides honor. It pins the open items from the
tether#8 spec (`mem_3PE2qeew`) before the emitter sibling wi is created.

## 1. Fence marker syntax

A block is a fenced code region in the assistant's text stream whose info-string is
`<kind>:<skill>` with an optional `#<blockId>`:

````
```<kind>:<skill>[#<blockId>]
{ ...kind-specific JSON... }
```
````

- `<kind>` ∈ `dag | form | candidates | media | permission` (`wire.FencedBlockKind`).
- `<skill>` — the emitting skill id (free-form, no whitespace), for display/debug.
- `#<blockId>` — **optional, stable id** (see §3).

The daemon parses **only the markers** (open info-string + closing ` ``` `). The JSON body
is **opaque** to the daemon (invariant **D-20**, `internal/wire/types.go`); only the browser
parses it (`web/src/fenced-blocks/`).

## 2. Wire representation

On recognizing a complete block the daemon emits one
`wire.Envelope{Kind: "fenced", Payload: wire.FencedBlock{...}}`:

```go
type FencedBlock struct {
    Kind    FencedBlockKind // "dag" | "form" | ...
    Skill   string          // emitting skill id
    Content string          // verbatim JSON body (opaque)
    BlockID string          // resolved id (see §3)
}
```

The fenced text is **suppressed** from the `message` (assistant-text) stream so the raw
JSON never renders as a chat bubble alongside the card.

## 3. BlockID — the live-replace key

`BlockID` is how a re-emitted block **updates the same card** instead of appending a new one.

- If the info-string supplies `#<blockId>`, that is the `BlockID` verbatim.
- If omitted, the daemon assigns a stable id `<skill>-<n>` (n = 0-based index of that skill's
  blocks within the turn).

**Emitter rule:** to animate progress (e.g. a DAG advancing), **re-emit the block with the
same `#<blockId>` each time**. The browser replaces the card keyed by `BlockID`
(`web/src/lib/store.ts`) and keeps its expand/collapse state. Emitting a *new* `blockId`
creates a *new* card.

## 4. `dag` content schema

The `dag` body MUST match the frontend `DagContent` (`web/src/fenced-blocks/types.ts`):

```jsonc
{
  "title": "spec → plan → …",        // optional headline
  "nodes": [                           // required; the graph nodes
    { "id": "spec", "label": "spec", "status": "done",    "ms": 4200 },
    { "id": "plan", "label": "plan", "status": "running" }
  ],
  "edges": [["spec", "plan"]],         // optional [fromId, toId]; defaults to a linear chain
  "paused": false,                     // optional; renders a "paused" pill
  "elapsedMs": 12000                   // optional; total wall-clock
}
```

- `status` ∈ `done | running | queued | failed` (unknown → `queued`).
- `ms` — per-node duration once finished; omit while queued/running.
- Every field a component reads is optional-guarded at parse time; a malformed/partial body
  degrades to a fallback, never throws (`parseContent`).

The other four kinds (`form` / `candidates` / `media` / `permission`) have their own schemas
in `types.ts`; this contract only pins `dag` (the tether#8 driver). They light up for free
once the daemon parser (T6) lands.

## 5. Action control frame (button callbacks)

The DAG card's side-panel buttons send a client→server frame on **`/wt/control`**
(`wire.ClientFrame`, added in F1; `SessionID` added in T8):

```jsonc
{ "kind": "action", "sessionId": "<SessionID>", "blockId": "<BlockID>", "action": "pause" | "approve", "skill": "<skill>" }
```

`SessionID` is required for routing: `/wt/control` is **not** otherwise session-scoped (unlike
`/wt/chat`), so it is the only way `serveControl` (`internal/server/control.go`) knows which
live session an action targets. The frontend reads it straight from `useStore.getState().sessionId`
at click time (`web/src/panes/chat/index.tsx`, `sendApprove`/`sendPause`) and the write goes out over
`ControlClient.sendAction` (`web/src/lib/control.ts`), which reuses the same retained stream
writer as the ping/pong RTT probe.

The daemon does **not** interpret DAG semantics (D-20). `handleActionFrame`
(`internal/server/control.go`) routes the action by `SessionID`, looked up in the `session.Registry`:

- **`approve`** (**wired, T8**) → `Registry.DeliverAction(sid, "approve", blockId, skill)` calls
  the target session's `agent.Session.SendPrompt` with a single-line JSON control payload,
  wrapped in a `__tether_action__` marker so the emitting skill can distinguish it from an
  ordinary user message:

  ```jsonc
  {"__tether_action__":{"action":"approve","blockId":"<BlockID>","skill":"<skill>"}}
  ```

  This is delivered verbatim as the session's next stream-json user message (same path as a
  typed chat message) — **the skill decides** what "approve" means (proceed to the next step /
  resolve its gate). It is **not** mapped to the PreToolUse permission channel. An unknown or
  already-ended `SessionID` is logged and dropped, never a crash — the daemon does not (and
  cannot) send a reply frame for `action`, so the browser has no ack to wait for.
- **`pause`** (**wired, T9**) → `Registry.InterruptSession(sid)` calls the target session's
  `agent.Session.Interrupt()` **directly** — NOT `DeliverAction`/`SendPrompt`, because pause is a
  transport-level signal to the agent process, not a chat message. For `claude-code`, `Interrupt()`
  (`internal/agent/claude_provider.go`, `ccSession.Interrupt`) writes a stream-json
  `control_request` on the same mu-guarded stdin encoder `SendPrompt` uses:

  ```jsonc
  {"type":"control_request","request_id":"<unique>","request":{"subtype":"interrupt"}}
  ```

  This is **not** a SIGINT to the subprocess (the earlier implementation signaled
  `s.cmd.Process`, which this replaces). cc aborts the in-flight turn but the process stays
  alive — tether holds its stdin open for the whole session (`--input-format stream-json`), so
  the session is immediately resumable via a subsequent `SendPrompt`/`DeliverAction`, no respawn
  or `--resume` round-trip needed. The daemon does not correlate or await the resulting
  `control_response` (fire-and-forget, like the interrupt call itself); `ccSession.parseLine`
  recognizes `type:"control_response"` explicitly and drops it so cc's ack never surfaces as a
  spurious chat event. `opencode`'s `Interrupt()` stays a documented no-op — T9 targets cc only
  (`internal/agent/opencode_provider.go`); this asymmetry is intentional scope, not a gap.
  An unknown or already-ended `SessionID` behaves like `approve`'s: `InterruptSession`'s error is
  logged and dropped, never a crash. For polyforge consistency the **skill** should call
  `pf_pause_attempt` once the emitter sibling wires it, so the wi is not left `in_progress`. The
  frontend pause button is now **enabled** (rollback stays disabled — no aihub primitive).
- **`rollback`** → **not wired** (non-goal; aihub has no rollback primitive). `handleActionFrame`
  ignores it. The button stays disabled.

## 6. Responsibilities

| Concern | Owner |
|---|---|
| Parse fence markers → `fenced` envelope; suppress from chat; replace-by-`BlockID`; persist in history | **tether** (this repo — T6/T7/T8) |
| Route `action` frames to the session/agent | **tether** (T8/T9) |
| **Emit / update** `dag` blocks from the scenario step-graph (nodes = `## Step:` names, status from aihub); honor `action` semantics (`pause`→`pf_pause_attempt`, `approve`→proceed) | **emitter skill** in `polyforge-coding` (sibling wi) |

End-to-end "live DAG" is complete only when both the tether half (tether#8) and the emitter
sibling land. tether#8 validates its half with a **test `dag` block**.
