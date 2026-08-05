# deploy/

Templates for running `tether` as a resident systemd service, instead of a
foreground process in a terminal.

**Nothing in this directory is installed automatically.** This work item
produced two template files (`tether.service`, `tether.env.example`) and this
document — it did not run `systemctl`, did not write to `/etc/`, and did not
install a service. Putting `tether` under systemd on a real machine is a
deliberate action a human takes by following the steps below.

## Prerequisites

1. **A built `tether` binary.**
   - `make build` (from the repo root) produces `bin/tether` — this is the
     recommended path for anything you're about to install, because it (via
     `scripts/build.sh`) does a full codegen + web + go build and links a
     real version string.
   - Alternatively, `scripts/release.sh [version]` cross-compiles
     `release/*.tar.gz` artifacts for five platforms if you need a
     release-style tarball instead of a local build.
   - See the version-identification limitation below before using a bare
     `go build` for something you intend to deploy.
2. Copy that binary to `/usr/local/bin/tether` (the path `tether.service`'s
   `ExecStart=` expects):
   ```bash
   sudo install -m 0755 bin/tether /usr/local/bin/tether
   ```

## Install steps (run these yourself — this work item did not run them)

```bash
# 1. Binary (see Prerequisites above)
sudo install -m 0755 bin/tether /usr/local/bin/tether

# 2. Secret-bearing env file — 0600, root-owned, never world-readable.
#    Use install -m 0600, not cp + chmod: cp leaves the file at your umask
#    (usually world-readable) for the moment in between.
sudo mkdir -p /etc/tether
sudo install -m 0600 -o root -g root deploy/tether.env.example /etc/tether/env
sudo -e /etc/tether/env   # fill in TETHER_TOKEN and TETHER_HOST
#    (sudo -e, not sudo "$EDITOR": sudo's env_reset usually drops $EDITOR.)

# 3. Unit file
sudo install -m 0644 deploy/tether.service /etc/systemd/system/tether.service

# 4. Reload systemd's view of unit files, then start
sudo systemctl daemon-reload
sudo systemctl enable --now tether

# 5. Check it came up
sudo systemctl status tether
sudo journalctl -u tether -f
```

Before step 4, you may sanity-check the unit's syntax without installing
anything. `systemd-analyze verify` is a read-only parse — it installs nothing:

```bash
systemd-analyze verify deploy/tether.service
```

Run before step 1 this prints `Command /usr/local/bin/tether is not
executable: No such file or directory` and **exits 1**. That is the binary
being absent, not a syntax problem — but note the non-zero exit before you
put this line in CI or under `set -e`. To check the syntax alone, point
`ExecStart=` at something that exists:

```bash
sed 's#/usr/local/bin/tether#/bin/true#' deploy/tether.service > /tmp/tether.service
systemd-analyze verify /tmp/tether.service   # exit 0, no output
```

## Values you must decide

| Value | Where | Decide based on |
|---|---|---|
| `--port` | `ExecStart=` in `tether.service` | The daemon serves the **same port over both TCP and UDP** (HTTP/3 handshake + QUIC/WebTransport data channel). Whatever firewall/security-group rule you write for this port must open **both protocols** — TCP-only leaves the page loading but chat/shell permanently stuck connecting. Default here is 443. |
| `TETHER_HOST` | `/etc/tether/env` | The hostname/IP **as it appears in the browser's address bar** (no scheme, no port). It is added to the daemon's browser-origin allowlist (`originAllowed`, `internal/server/mux.go`). Until it matches, every non-safe-method request from a remote browser — **including the login `POST /api/v1/auth/verify`** and the WebTransport handshake — is rejected with `403 forbidden: origin not allowed`. The page still loads (GET is a safe method), so this presents as "login is broken" rather than as a config error. Also the OAuth issuer host and the hostname in the startup log URL. |
| `--workspace-root` | `ExecStart=` in `tether.service` | Dual meaning, same flag: the MCP sandbox root, **and** the fallback agent cwd for any chat session that hasn't selected a registered workspace. One value governs both; there's no separate flag for the fallback-cwd case. |
| `User=` / `Group=` / `Environment=HOME=` | `tether.service` `[Service]` | `HOME` is where `~/.tether/` (cert, tokens, sessions, hook binary) and `~/.config/claude/settings.json` land. Must match the account `User=` actually runs as. Also sets the OS-level privileges of the coding agent tether drives — see security section below. |
| `TETHER_TOKEN` | `/etc/tether/env` | **Pre-provision it** with `openssl rand -hex 32`. Leaving it empty makes tether generate one on first start and print it once to stdout — which the unit sends to `/dev/null` precisely so the secret does not land in the journal, meaning you would have to read it back out of `$HOME/.tether/access-token` instead. Filling it in avoids that dance entirely. |
| Binary path | `/usr/local/bin/tether` (hardcoded in `ExecStart=`) | If you install elsewhere, edit `ExecStart=` to match — systemd does not search `$PATH`. |

## Security implications of running resident

Running tether under systemd is qualitatively different from running it in a
terminal for a single session, and the risk profile changes accordingly:

- **The port is exposed continuously**, not just for the duration of a
  session you're watching. Any vulnerability in the listener has a much
  larger exposure window.
- **A coding agent is resident in a real workspace with the service user's
  privileges.** tether isn't a passive file server — it drives Claude Code
  against real files with real shell access. Possession of the access token
  is therefore effectively possession of that workspace, and, at the
  `User=root` default this unit ships, of the machine.
- **The token is a single long-lived static credential.** There is no
  rotation or expiry in this version — if it leaks, it's valid until you
  manually replace it (edit `/etc/tether/env`, restart the service).
- **Why the secret must come from `EnvironmentFile=` and never `ExecStart=`:**
  any argument passed on a command line is visible to every local user via
  `ps aux`, `/proc/<pid>/cmdline`, etc. — process arguments are not private
  the way an env file with restrictive permissions is. This is the whole
  reason this unit exists instead of a one-line
  `ExecStart=/usr/local/bin/tether server --token <secret>`: that form
  leaks the secret to any local account.
- **File permissions matter as much as the unit itself.** `/etc/tether/env`
  must be `0600` root-owned (enforced by the install command above, but
  re-check it after any manual edit). The daemon's own runtime state under
  `~/.tether/` — `cert.pem`'s private key, `access-token`, `api-tokens.json`
  — is equally sensitive and inherits the permissions/ownership of whatever
  `HOME` the unit points at.
- **What does and does not reach the journal.** tether logs which precedence
  level supplied the token — `auth: access token loaded token_source=flag|env|file|generated`
  — and never the value, on any of those four paths. Use that line to confirm
  which credential path is live without exposing the secret. There is exactly
  one exception, and the unit closes it: on the **`generated`** path the
  daemon prints the brand-new token once to **stdout** (`tether access token:
  …`), and under systemd stdout *is* the journal — an indexed, persistent log
  readable by every member of `systemd-journal`/`adm`. That would be the same
  class of local exposure as putting the token in `ExecStart`. The unit
  therefore sets `StandardOutput=null`; logs are unaffected because they go to
  stderr. The generated token remains readable at `$HOME/.tether/access-token`
  (mode 0600). Pre-provisioning `TETHER_TOKEN` avoids the path altogether.
- **The token is also kept out of child processes.** tether hands the coding
  agent and every shell-pane command a verbatim copy of its own environment,
  so the daemon unsets `TETHER_TOKEN` immediately after reading it. Without
  that, the credential would sit in the environment of an LLM-driven process,
  one `env` dump away from a transcript that leaves the machine.

## Hardening rationale

Read this first: **as shipped, this unit runs as root with no filesystem,
capability or syscall containment. The access token is the security boundary,
not systemd.** The settings below are mostly reliability and resource tuning;
calling them "hardening" would overstate the posture. The containment
directives are discussed in the second list, where they are deliberately
absent.

**Enabled (reliability / resources):**
- `Restart=on-failure` + `RestartSec=5s` + `StartLimitIntervalSec=300` /
  `StartLimitBurst=5` — recovers from crashes without churning forever on a
  bad config: 5 failures in 5 minutes stops the unit and leaves it visibly
  `failed`. The rate-limit pair is not optional decoration — with systemd's
  defaults (`StartLimitIntervalSec=10s`, burst 5) a 5-second restart interval
  can never reach the burst, so the limiter would never trip. A clean
  `systemctl stop` does not trigger a restart.
- `StandardOutput=null` — keeps a generated token out of the journal (see the
  security section). Logs go to stderr and are unaffected.
- `Type=exec` — `systemctl start` fails loudly if the binary can't be exec'd,
  instead of reporting success and exiting immediately.
- `TimeoutStopSec=15s` — shutdown drains sessions under a 5s budget and then
  closes listeners under a second 5s budget, so the realistic worst case is
  ~10s; 15s leaves real headroom before systemd escalates to `SIGKILL`.
- `LimitNOFILE=65536` — each chat/shell session holds PTYs, QUIC streams,
  and per-task MCP child processes open; the systemd default of 1024 is
  tight under real concurrent use.
- `AmbientCapabilities=CAP_NET_BIND_SERVICE` — a no-op while `User=root`
  (root can already bind privileged ports), but present and correct for the
  day you switch `User=` to a dedicated non-root account that still needs to
  bind `:443`. Caveat for that day: ambient capabilities are inherited by
  every descendant, so all agent-issued commands would carry it. If you want
  none of that, socket activation (a companion `.socket` unit with
  `ListenStream=443`) is the cleaner answer than a capability.

**Deliberately NOT enabled** (see the matching comment blocks in
`tether.service` for the full reasoning — summarized here):
- `ProtectSystem=` / `ProtectHome=` / `ReadOnlyPaths=` — tether's entire
  purpose is running a coding agent inside the operator's *real* workspace
  with the operator's *real* toolchain. Filesystem namespacing directly
  breaks that. The security boundary here is the access token plus "the agent
  runs as the service user," not systemd sandboxing.
- `PrivateTmp=` — omitted for its own reason, not the one above. tether
  itself would tolerate a private `/tmp` (its MCP loopback is a 127.0.0.1 TCP
  port, not a `/tmp` socket). What breaks is the agent's hand-offs with the
  rest of your session through `/tmp`: tmux/screen sockets, `/tmp/.X11-unix`,
  ssh/gpg agent sockets, scratch files you then open in another terminal.
  Enable it if you never touch the agent's `/tmp` from outside the service.
- `CapabilityBoundingSet=` — the agent executes arbitrary user commands
  (package managers, container tooling); narrowing the bounding set breaks
  legitimate work in ways that surface as confusing tool failures, not
  clear permission errors.
- `NoNewPrivileges=yes` — commented out in the unit. It breaks `sudo`/setuid
  inside agent-driven shells ("effective uid is not 0") while buying very
  little on a service that's already root by default. Recommended to
  **enable it** if you switch to a dedicated non-root `User=` — at that
  point `sudo` wasn't going to work for that account anyway, and the
  directive gives real protection against privilege-escalation exploits.

## Known limitation: version identification in worktrees

`tether version` resolves the version string in tiers: an ldflags-injected
value (`-X main.version=...`, done by `scripts/release.sh`) takes precedence
if present; otherwise it falls back to Go's automatic VCS build-info
stamping (`runtime/debug.ReadBuildInfo`, module pseudo-version +
`vcs.revision`/`vcs.modified`).

Go's automatic VCS stamping does **not** activate inside a git *linked
worktree* — where `.git` is a file pointing at the real repo rather than a
directory — because the toolchain's VCS detection doesn't follow that
indirection. A bare `go build` run from inside such a worktree therefore
still prints the plain compile-time default (`v0.1.0-dev`), even though
`ReadBuildInfo` itself reports success.

This does not affect `make build` (`scripts/build.sh`) or
`scripts/release.sh`, both of which inject a real version via `-X
main.version=`, nor does it affect a bare `go build` from a normal (non-
worktree) clone. **Use `make build` for anything you intend to install** —
it sidesteps this entirely regardless of what kind of checkout you're
building from.

## Troubleshooting

- **Service starts then immediately exits.** `journalctl -u tether -f` for
  the failure reason. Look for the line
  `auth: access token loaded token_source=...` to confirm which credential
  path was actually used (`flag`/`env`/`file`/`generated`) — a wrong source
  here (e.g. `generated` when you expected `env`) usually means
  `/etc/tether/env` wasn't loaded or `TETHER_TOKEN` was blank/whitespace
  after trimming.
- **Page loads, but logging in always fails with 403.** Look for
  `forbidden: origin not allowed`. This is `TETHER_HOST` not matching the
  host in the browser's address bar — see the decision table above. Set it
  (no scheme, no port) and restart. It is not a cert, firewall or OAuth
  problem, all of which it superficially resembles.
- **A stale `TETHER_TOKEN` in your shell changes which token the daemon
  serves.** `scripts/bench-e2e.sh` uses the same variable name for the
  *client* credential. If you export it for a benchmark and then start the
  daemon from that shell, the daemon adopts it — with only
  `token_source=env` in the log as a hint.
- **Port already in use.** `sudo ss -tulpn | grep :443` (or your chosen
  port) to find the conflicting process; either stop it or change `--port`
  in `ExecStart=` and restart.
- **Page loads but chat/shell never connect.** This is the signature of UDP
  being blocked while TCP gets through — the HTTPS page load only needs
  TCP, but the WebTransport/QUIC data channel needs UDP on the *same* port.
  Confirm with `nc -u -z -w 3 <host> <port>` from a remote box, and open UDP
  on that port in whatever firewall/security-group sits in front of this
  host. See the root `README.md`'s "K.8.1 — VPN / corporate firewall"
  section for the full diagnostic recipe.
