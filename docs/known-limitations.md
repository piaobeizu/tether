# Known Limitations — tether v0.4

## Security

**No end-to-end encryption (D-12 deferred to v1.0+)**
TLS terminates at the tether daemon. Traffic between the browser and daemon is
TLS-protected, but the daemon itself has plaintext access. WebCrypto E2E (D-12)
is a v1.0+ item.

~~**Self-signed certificate**~~
*(Resolved in v0.1.1 / v0.2.0)* — Let's Encrypt ACME is available via
`--acme-domain` / `--acme-email`. For LAN/self-signed dev setups see
[README K.8.3](../README.md#k83--self-signed-cert-chrome-refuses-to-load-the-page).

## Connectivity

**No TCP fallback for WebTransport**
HTTP/3 over UDP is the only transport. VPNs and corporate firewalls that block
UDP will cause chat/shell channels to fail even if the page loads. See
[README K.8.1](../README.md#k81--vpn--corporate-firewall-breaks-the-shell-and-chat-channels).

**Browser support: Chrome/Edge 97+ only**
Firefox and Safari do not support the WebTransport API. See
[README K.8.2](../README.md#k82--browser-version-requirements).

## Multi-user / Multi-device

~~**Single-user, single-machine only (no auth)**~~
*(Partially resolved)* — v0.2.0 added JWT token gate; v0.3.2 added manual API
tokens for external MCP clients; v0.3.3 added OAuth 2.1 PKCE. Full multi-user
session isolation (D-04 `LockUser="default"`) is still v1.0+.

**No QR-pairing (`tether pair`)**
`tether pair` is deferred to v1.0 (D-13).

## Agent providers

~~**Claude Code only**~~
*(Partially resolved)* — opencode provider added in v0.2.0. External MCP
clients (Cursor, Goose) can connect via OAuth PKCE (v0.3.3) or manual Bearer
tokens (v0.3.2). Gemini provider is still v1.0+.

~~**Agent cwd is daemon-global, not per-workspace**~~
*(Resolved in tether#52.)* A chat connection selects a registered workspace with
`?ws=<id>` on `/wt/chat`; the agent runs in that workspace's `Path`, and the PTY
shell pane follows the chat session it resumes
(`session.Registry.WorkdirForSession`). The daemon resolves the **id** through
`~/.tether/workspaces.json` and refuses one it does not know — a client never
supplies a path. A connection that selects no workspace still runs in the
resolved `--workspace-root` (default `~/.tether/workspace`), which remains the
MCP builtin-tools sandbox root as well.

Residuals:

- **A session's workspace is fixed for its lifetime.** cc keys its transcript on
  cwd, so a session created in workspace A cannot be resumed in B. Browsing to a
  different workspace therefore does *not* move a live session — open a new
  session to work elsewhere. The binding is recorded at
  `~/.tether/sessions/<sid>/workspace.json` and honoured on reconnect; a session
  presented under a *different* workspace is answered with a new session there,
  reported like a failed resume (`Recovered`/notice), never resumed in the wrong
  directory.
- **The tether MCP builtin tools do NOT follow the session's workspace.**
  `workspace_read_file` / `workspace_list_files` / `workspace_run_shell`
  (`internal/mcp/builtin`) are rooted at `--workspace-root` for the daemon's whole
  life, because that server is a single instance injected into
  `~/.claude/settings.json`. Before this change agent cwd and that root were the
  same directory; now a workspace-selected session has cc's own Read/Write/Bash
  in `/srv/project-a` while the tether builtins still operate in
  `~/.tether/workspace`, so a relative path means two different things inside one
  session. Per-session builtin roots are a separate slice.
- **A session the daemon has no binding for lands in `--workspace-root`, and
  stays there.** The browser sends `ws` only when it has no sid, so a
  `tether_last_sid` with no recorded workspace (any session created before this
  change, a wiped `~/.tether/sessions`, a sid carried from another machine)
  reconnects into the default directory and its fresh replacement is recorded
  there. Start a new session to move it.
- **A workspace whose directory no longer exists fails the connection, and the
  browser retries it.** No path is ever checked to exist (`workspace.Registry.Add`
  only makes it absolute), so a deleted/renamed/unmounted workspace makes
  `provider.Spawn` fail on `chdir`. Likewise a `ws` id the daemon does not know is
  refused — correctly — but the refusal reaches the browser as a closed
  connection, which the chat pane treats as a transport failure and retries with
  the same URL up to its reconnect cap, ending in a "UDP/QUIC may be blocked"
  message that is the wrong diagnosis. Making a workspace error terminal and
  legible on the client needs a distinguishable error on the wire; not done here.
- **The first session on a fresh browser profile.** The chat pane defers its first
  connect until the workspace list settles (2 s cap) and then uses the selected
  workspace; if the list is empty or unreachable, that session runs in
  `--workspace-root`.
- **Removing a workspace does not end its sessions.** `Remove` deletes a list
  entry and touches no files; existing sessions keep their recorded directory, so
  they stay resumable. Revocation is not implemented.
- **No UI shows which workspace a session belongs to.** The workspace pane
  highlights what you are *browsing*, which after a workspace switch is not
  necessarily where chat is running. The shell pane follows the chat session's
  workspace, but only once chat *has* a session — a shell opened before that
  starts in `--workspace-root`.
- **`POST /api/v1/workspaces` accepts any absolute path**, with no existence or
  confinement check. "The client sends an id, never a path" is true of the chat
  handshake, but an authenticated client can still choose any directory in two
  requests. That endpoint predates this change; what changed is that the workspace
  list now decides where the agent *executes*, not just what the file browser
  shows.
- `Workspace.ActiveSID` remains unwired — it is a workspace→sid pointer, and the
  binding above is the sid→workspace direction the daemon actually needs.

## UI / PWA

**No offline support**
The PWA manifest is minimal. Service worker and offline caching are v0.5+.

**No partial-message streaming**
`--include-partial-messages` is not enabled. The chat pane renders complete
assistant turns only. Deferred to v0.5+.

## Permission hook

**`bypassPermissions` mode skips the hook**
When `claude` is started in `--permission-mode bypassPermissions`, cc does not
fire PreToolUse hooks. tether's permission UI is never shown — tool calls
auto-approve. This is the intended UX for fully autonomous mode.

**Hook requires Go toolchain at runtime — the daemon will not start without it**
The hook is compiled from embedded source into
`~/.tether/bin/tether-permission-hook` on first daemon start
(`cchook.EnsureHookBinary`, called from `internal/server/lifecycle.go`), which
requires `go` on PATH. If `go` is absent the compile fails and that error is
returned straight out of `server.Run`, so `tether server` exits rather than
starting without a permission gate. Set `TETHER_NO_PERMISSION_HOOK=1` to skip
hook setup deliberately (tool calls then auto-approve).

This applies to **every** install method, not just source builds. Release
tarballs do ship a pre-built `tether-permission-hook` next to the binary, but the
daemon never looks for it: `EnsureHookBinary` only ever consults a hash file
under `~/.tether/bin/` and compiles into that directory. So the tarball copy is
not the mitigation it reads like.

(This paragraph previously said the failure was "skipped with a warning" and that
the runtime compile was a `go install` fallback. Both were wrong; the second is
moot anyway, since `go install` stopped being a supported path in tether#81.)
