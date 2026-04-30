# package claude

tether's backend wrapper around the Claude Code CLI.

It spawns the `claude` ELF binary as a subprocess in stream-json mode
and communicates via NDJSON over stdio. Session persistence and
recovery are delegated to CC's own `--resume` mechanism.

Spec: [`<ticket-dir>/.specs/2026-04-30-cc-sdk-route.md`](../../../.specs/2026-04-30-cc-sdk-route.md) — 24
behavior contracts across A (interface) / C (recovery) / D
(deployment) / E (storage) / F (impl path) / G (backend abstraction).

## Quick start

```go
import "github.com/piaobeizu/tether/internal/backend/claude"

// 1. Optional — define a tool authorizer.
auth := claude.ToolAuthorizerFunc(func(ctx context.Context, name string, input json.RawMessage) (claude.PermissionResult, error) {
    // approve or reject the tool call
    return claude.PermissionResult{Behavior: claude.PermissionAllow}, nil
})

// 2. Spawn a session. ProjectCwd defaults to $HOME (spec §10.4
//    strategy a) so all sessions land in ~/.claude/projects/-root/.
sess, err := claude.New(ctx, claude.SpawnOpts{Model: "haiku"}, auth)
if err != nil { return err }
defer sess.Close()

// 3. Drain events in a goroutine for UI consumption.
go func() {
    for env := range sess.Events() {
        // route to UI
    }
}()

// 4. Send the first user message — Start blocks until system/init
//    arrives (or 10s ErrInitTimeout).
if err := sess.Start(ctx, "Hello, claude!"); err != nil { return err }

// 5. Subsequent turns just call Send.
sess.Send("How are you?")
```

For the recovery primitive (plugin-reload / pod-restart / daemon-crash
/ WT-reconnect):

```go
// Idempotent restart with --resume <SessionID>; refused if state isn't
// idle (unless stuck for >120s).
if err := sess.Recover(ctx); err != nil {
    if errors.Is(err, claude.ErrUnsafeToReload) {
        // wait for a result event before retrying
    }
}
// Caller must Send a new message after Recover to drive the next turn.
```

See `cmd/spike/` for the end-to-end driver covering all 8 scenarios
from spec §8.

## Verified behavior contracts (v0.1 spike)

| contract | mechanism |
|---|---|
| A.1 no fd 3 | `cmd.ExtraFiles == nil` (TestSpawn_RealClaude) |
| A.2 graceful-degrade | unknown type/subtype passes through; verified across 4 cc versions |
| A.3 typed `ErrSubprocessGone` | TestSession_SendAfterKill_RealClaude |
| A.4 `ErrInitTimeout` 10s | TestSession_InitTimeout |
| A.5 stderr drained continuously | 64KB ring buffer; TestSession_StderrDrain_RealClaude |
| C.1–C.3 safe-to-reload gate | recover_test.go state-guard tests |
| C.6 AbortPending on Recover | TestSession_Recover_AbortsPendingControl |
| C.5 reload cost ~$0.05–0.15 | Haiku $0.11, Sonnet $0.05 (verification-report.md) |
| D.1 JuiceFS path | cwd-default → bucket verified locally |
| D.2 unified Recover primitive | TestSession_Recover_RealClaude_Scenario5 |
| E.5 cwd-policy strategy (a) | TestSpawn_DefaultCwdLandsInHomeBucket_RealClaude |
| F.2 typed `ErrBinaryNotFound` | TestSpawn_BinaryNotFound |
| §10.5 multi-session independence | TestSessions_TwoConcurrent_Independent |

## v0.1 follow-up sub-tickets (per plan §10 Step 10)

These are deliberately out of scope for the gh-13 spike but each is a
v0.1 milestone item. Open as GitHub issues under Epic #3 milestone v0.1.

1. **cwd policy (c) wrapper** — `tether resume <sid>` CLI that
   abstracts the bucket lookup. Alternative to the current strategy
   (a). Spec §10.4.

2. **`Session.Recover` 4-caller integration** — wire the four
   recovery callers (plugin-reload, pod-restart, daemon-crash,
   WT-reconnect) to the existing primitive. Currently only the
   primitive is implemented; production callers are deferred to their
   own tickets.

3. **hook idempotency verification** — when tether starts shipping
   any of its own `SessionStart` hooks, verify the C.4 contract holds
   (idempotent under multi-trigger via `Recover`). v0.1 spike ships
   no tether-owned hooks, so this contract is vacuously satisfied —
   but the test infra should be in place for the next ticket that
   adds one.

4. **JuiceFS PV deployment manifest** — k8s YAML for mounting
   JuiceFS-backed PVCs at `~/.claude/{projects,plugins,auth}` and
   `~/.claude/settings.json`. Spec §6.D.1. Includes the deferred
   C'' verification (pod-restart with PV remount).

5. **GC stub-session sweeper** — background goroutine in tether
   daemon that periodically scans `~/.claude/projects/<dir>/` and
   removes failed/auth-error stubs (size <5KB AND result.is_error or
   error="authentication_failed"). Spec §6.E.3.

6. **tool authorization UX → WT protocol** — UI mapping for
   `control_request can_use_tool` (mid-stream cancel? batch
   approve? deny + ask-followup?). Depends on Epic #6 design;
   daemon-side `Controller` handshake is already complete.

7. **C.5 reload UX feedback** — "正在重载会话…" indicator while
   `Session.Recover` is running. Depends on Epic #6 UI.

8. **credentials injection** — `ANTHROPIC_API_KEY` / OAuth flow
   from user → tether daemon → cc subprocess env. Depends on Epic
   #5 (E2E auth) / deploy ticket.

9. **operational concerns** — log volume cap, OOM circuit-breaker
   (avoid infinite Recover loop on exit-137), claude version drift
   detection, pre-flight API balance check. Single ops ticket.

## Source-of-truth

- spec: `<ticket-dir>/.specs/2026-04-30-cc-sdk-route.md`
- plan: `<ticket-dir>/.plans/2026-04-30-cc-sdk-route.md`
- spike driver: `cmd/spike/`
- verification report: `cmd/spike/spike-out/2026-04-30-verification-report.md`
- happy-cli reference (read-only):
  `github.com/slopus/happy-cli` — `src/claude/sdk/query.ts`,
  `src/claude/claudeLocal.ts` (fd 3 — DEPRECATED for tether per
  spec §6.A.1), `src/claude/session.ts`, `src/claude/loop.ts`
