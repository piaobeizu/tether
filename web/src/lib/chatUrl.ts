// chatUrl.ts — builds the /wt/chat WebTransport connect URL (tether#52).
//
// tether#52 adds a workspace selector to the daemon's chat handshake: the
// daemon spawns the cc agent subprocess with its cwd pinned to a workspace,
// and `?ws=<workspace-id>` is how the browser tells it which one for a BRAND
// NEW session. The daemon resolves the id through its own on-disk registry
// and rejects anything it doesn't recognize — it never accepts a path from
// the client, so this file only ever carries an opaque id, same as `sid`.

/**
 * chatURL builds the `/wt/chat?...` URL ChatPane's doConnect opens a
 * WebTransport session against. Extracted as a pure function (mirrors
 * shouldSendOnEnter/growHeight/parseAtQuery in panes/chat/index.tsx) so the
 * one rule that actually matters here — see below — is unit-testable without
 * standing up a WebTransport connection.
 *
 * Params: `provider` is always present. `sid` is included when non-empty
 * (resume that session). `ws` is included ONLY when `sid` is empty/absent —
 * this is the load-bearing rule, and getting it backwards can silently
 * abandon a live session mid-turn. Read on.
 *
 * WHY `ws` travels only in the absence of `sid`:
 *
 * A session's workspace is fixed at spawn and CANNOT change afterward — cc's
 * `--resume` is cwd-scoped (the transcript lives under
 * `~/.claude/projects/<encoded-cwd>/<uuid>.jsonl`), so a session created in
 * workspace A can never be resumed in workspace B. Because of that, the
 * daemon remembers each session's workspace on disk and honours it on
 * reconnect — and its rule for "the client sent a `sid` AND a `ws` that
 * disagrees with that sid's remembered workspace" is to DROP the sid and
 * start a brand-new session in the requested workspace. That rule has to
 * exist for SOME case to reach it (a deliberate "reopen this conversation in
 * a different workspace" action) — but the daemon has no way to distinguish
 * that deliberate case from an accidental one, because both look identical
 * on the wire: an existing sid plus a ws that doesn't match it.
 *
 * `activeWorkspace` (store.ts) tracks whatever the user is currently BROWSING
 * in the left WorkspacePane (tether#47) — a read with no relationship to which
 * workspace the LIVE session actually runs in, and one that keeps changing as
 * the user clicks around while chatting. If every reconnect resent it, an
 * ordinary network blip — a WebTransport drop, a laptop sleep, a flaky UDP
 * path — that happened to land after the user had merely clicked a different
 * workspace in the sidebar would present the daemon with exactly the signal
 * it's told to treat as "abandon and start over": existing sid, disagreeing
 * ws. The live session — mid-turn, with real state — would be silently
 * dropped for a fresh empty one. No error, no user action, just bad timing.
 *
 * Sending `ws` only when `sid` is empty closes that hole structurally rather
 * than by convention: there is no sid yet for the browsed workspace to
 * disagree with, so it can only ever decide a NEW session's directory at
 * creation time, never relocate one that already exists. (ChatPane also
 * reads `activeWorkspace` via `getState()` rather than subscribing to it, for
 * the same reason from the other direction — see index.tsx's mount effect.)
 */
export function chatURL(opts: { host: string; provider: string; sid?: string; wsID?: string }): string {
  let url = `https://${opts.host}/wt/chat?provider=${encodeURIComponent(opts.provider)}`
  if (opts.sid) {
    url += `&sid=${encodeURIComponent(opts.sid)}`
  } else if (opts.wsID) {
    url += `&ws=${encodeURIComponent(opts.wsID)}`
  }
  return url
}
