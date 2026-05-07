#!/usr/bin/env bash
# scripts/dev.sh — local dev start/stop/status/logs for the v0.1 dogfood loop.
#
# Manages two long-running processes on your laptop:
#   - daemon  :  `tether daemon -v --auth-broker --wt-addr :4444`
#   - app     :  `npm run tauri:dev` in tether-app/
#
# State (PIDs + logs) lives under .dev/ at the repo root (gitignored).
#
# USAGE:
#   scripts/dev.sh start       # start both daemon and app (in background)
#   scripts/dev.sh stop        # stop both, clean PID files
#   scripts/dev.sh status      # which processes are alive
#   scripts/dev.sh logs        # tail both logs concurrently
#   scripts/dev.sh logs daemon # tail daemon log only
#   scripts/dev.sh logs app    # tail app log only
#   scripts/dev.sh fingerprint # print the daemon's DER cert SHA256 (paste into Settings)
#   scripts/dev.sh sid         # list cc session IDs you can `tether resume`
#   scripts/dev.sh restart     # = stop + start
#
# REQUIRES (one-time):
#   - `tether` binary on PATH (run `go install ./cmd/tether` first)
#   - `claude` binary on PATH (Anthropic Claude Code CLI)
#   - $ANTHROPIC_API_KEY exported in the shell that runs `start`
#   - tether-app at ../tether-app/ with `npm install` already done
#
# ENV OVERRIDES (optional):
#   TETHER_APP_DIR     — path to tether-app worktree (default: ../tether-app)
#   TETHER_WT_PORT     — daemon WT listen port (default: 4444)
#   TETHER_HOOK_PORT   — leave unset; daemon picks a random loopback port
#   ANTHROPIC_API_KEY  — REQUIRED for cc to run; checked in `start`

set -euo pipefail

# Resolve repo root + state dir
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"
DEV_DIR="$REPO_ROOT/.dev"
mkdir -p "$DEV_DIR"

DAEMON_PID="$DEV_DIR/daemon.pid"
APP_PID="$DEV_DIR/app.pid"
DAEMON_LOG="$DEV_DIR/daemon.log"
APP_LOG="$DEV_DIR/app.log"
FINGERPRINT_FILE="$DEV_DIR/cert-fingerprint.txt"

WT_PORT="${TETHER_WT_PORT:-4444}"
APP_DIR="${TETHER_APP_DIR:-$REPO_ROOT/../tether-app}"

# --- helpers ------------------------------------------------------------

color_on() {
  if [[ -t 1 ]]; then printf '\033[%sm' "$1"; fi
}
color_off() {
  if [[ -t 1 ]]; then printf '\033[0m'; fi
}
say() {
  printf '%s[dev.sh]%s %s\n' "$(color_on '0;36')" "$(color_off)" "$1"
}
warn() {
  printf '%s[dev.sh] WARN:%s %s\n' "$(color_on '1;33')" "$(color_off)" "$1" >&2
}
fatal() {
  printf '%s[dev.sh] ERROR:%s %s\n' "$(color_on '1;31')" "$(color_off)" "$1" >&2
  exit 1
}

is_alive() {
  local pidfile="$1"
  [[ -f "$pidfile" ]] && kill -0 "$(cat "$pidfile" 2>/dev/null)" 2>/dev/null
}

# --- start --------------------------------------------------------------

start_daemon() {
  if is_alive "$DAEMON_PID"; then
    warn "daemon already running (pid $(cat "$DAEMON_PID"))"
    return 0
  fi

  if ! command -v tether >/dev/null 2>&1; then
    fatal "tether binary not found on PATH. Run: cd $REPO_ROOT && go install ./cmd/tether"
  fi

  if [[ -z "${ANTHROPIC_API_KEY:-}" ]]; then
    warn "ANTHROPIC_API_KEY not set — daemon will start but cc tool calls will fail at execution time."
  fi

  : > "$DAEMON_LOG"
  say "starting daemon  → $DAEMON_LOG"

  # nohup + setsid so the daemon survives this shell exiting; redirect both
  # streams into the log so the fingerprint capture below works.
  nohup setsid tether daemon -v --auth-broker --wt-addr ":$WT_PORT" \
    >> "$DAEMON_LOG" 2>&1 < /dev/null &
  echo $! > "$DAEMON_PID"

  # Wait for the dev cert fingerprint line (max 5s).
  local i=0
  local fingerprint=""
  while (( i < 50 )); do
    if grep -qE 'DER sha256=[0-9a-fA-F]+' "$DAEMON_LOG" 2>/dev/null; then
      fingerprint="$(grep -oE 'DER sha256=[0-9a-fA-F]+' "$DAEMON_LOG" | head -1 | sed 's/DER sha256=//')"
      break
    fi
    sleep 0.1
    i=$((i+1))
  done

  if [[ -n "$fingerprint" ]]; then
    echo "$fingerprint" > "$FINGERPRINT_FILE"
    say "daemon up — DER SHA256 = $fingerprint"
    say "  paste this into tether-app Settings → Connection → Pinned cert SHA256"
  else
    warn "daemon started but no DER fingerprint line in log within 5s — check $DAEMON_LOG"
  fi
}

start_app() {
  if is_alive "$APP_PID"; then
    warn "app already running (pid $(cat "$APP_PID"))"
    return 0
  fi

  if [[ ! -d "$APP_DIR" ]]; then
    fatal "tether-app dir not found at $APP_DIR. Set TETHER_APP_DIR=/path/to/tether-app or clone the repo."
  fi
  if [[ ! -d "$APP_DIR/node_modules" ]]; then
    warn "node_modules missing in $APP_DIR — running npm install first..."
    (cd "$APP_DIR" && npm install)
  fi

  : > "$APP_LOG"
  say "starting app     → $APP_LOG"
  nohup setsid bash -c "cd '$APP_DIR' && npm run tauri:dev" \
    >> "$APP_LOG" 2>&1 < /dev/null &
  echo $! > "$APP_PID"
  say "app up — Tauri webview will appear when build finishes (~30s first time)"
}

cmd_start() {
  start_daemon
  start_app
  say "ready. \`scripts/dev.sh logs\` to tail."
}

# --- stop ---------------------------------------------------------------

stop_one() {
  local label="$1"
  local pidfile="$2"
  if ! is_alive "$pidfile"; then
    if [[ -f "$pidfile" ]]; then rm -f "$pidfile"; fi
    say "$label not running"
    return 0
  fi
  local pid
  pid="$(cat "$pidfile")"
  say "stopping $label (pid $pid + group)"
  # Kill the whole process group (setsid above) so children exit too.
  kill -TERM -"$pid" 2>/dev/null || kill -TERM "$pid" 2>/dev/null || true
  # Wait up to 5s for clean exit
  local i=0
  while (( i < 50 )); do
    if ! kill -0 "$pid" 2>/dev/null; then break; fi
    sleep 0.1
    i=$((i+1))
  done
  if kill -0 "$pid" 2>/dev/null; then
    warn "$label did not stop after SIGTERM, sending SIGKILL"
    kill -KILL -"$pid" 2>/dev/null || kill -KILL "$pid" 2>/dev/null || true
  fi
  rm -f "$pidfile"
}

cmd_stop() {
  stop_one "app" "$APP_PID"
  stop_one "daemon" "$DAEMON_PID"
  rm -f "$FINGERPRINT_FILE"
  say "stopped"
}

# --- status -------------------------------------------------------------

cmd_status() {
  local fp="(none — daemon not started)"
  [[ -f "$FINGERPRINT_FILE" ]] && fp="$(cat "$FINGERPRINT_FILE")"

  printf '\n%s== tether dev status ==%s\n' "$(color_on '1;36')" "$(color_off)"
  if is_alive "$DAEMON_PID"; then
    printf '  daemon : %sup%s   pid=%s   wt=:%s\n' \
      "$(color_on '1;32')" "$(color_off)" "$(cat "$DAEMON_PID")" "$WT_PORT"
  else
    printf '  daemon : %sdown%s\n' "$(color_on '1;31')" "$(color_off)"
  fi
  if is_alive "$APP_PID"; then
    printf '  app    : %sup%s   pid=%s   dir=%s\n' \
      "$(color_on '1;32')" "$(color_off)" "$(cat "$APP_PID")" "$APP_DIR"
  else
    printf '  app    : %sdown%s\n' "$(color_on '1;31')" "$(color_off)"
  fi
  printf '  cert   : %s\n' "$fp"
  printf '  logs   : %s\n' "$DEV_DIR/{daemon,app}.log"
  printf '\n'
}

# --- logs ---------------------------------------------------------------

cmd_logs() {
  local which="${1:-both}"
  case "$which" in
    daemon) tail -F "$DAEMON_LOG" ;;
    app)    tail -F "$APP_LOG" ;;
    both|"") tail -F "$DAEMON_LOG" "$APP_LOG" ;;
    *) fatal "unknown logs target: $which (want: daemon|app|both)" ;;
  esac
}

# --- fingerprint --------------------------------------------------------

cmd_fingerprint() {
  if [[ ! -f "$FINGERPRINT_FILE" ]]; then
    fatal "no fingerprint cached. Start the daemon first: scripts/dev.sh start"
  fi
  cat "$FINGERPRINT_FILE"
}

# --- sid ----------------------------------------------------------------

cmd_sid() {
  local proj="${HOME}/.claude/projects"
  if [[ ! -d "$proj" ]]; then
    fatal "$proj not found — have you run claude at least once?"
  fi
  printf '%scc session IDs (under %s):%s\n\n' "$(color_on '1;36')" "$proj" "$(color_off)"
  find "$proj" -maxdepth 2 -name '*.jsonl' -printf '%T@ %p\n' 2>/dev/null \
    | sort -rn \
    | head -20 \
    | while read -r ts path; do
        local sid; sid="$(basename "$path" .jsonl)"
        local bucket; bucket="$(basename "$(dirname "$path")")"
        local mtime; mtime="$(date -d "@${ts%.*}" '+%Y-%m-%d %H:%M' 2>/dev/null || echo "?")"
        printf '  %s  %s  (bucket: %s)\n' "$mtime" "$sid" "$bucket"
      done
  printf '\nResume with:  tether resume <sid>\n\n'
}

# --- main ---------------------------------------------------------------

cmd="${1:-}"
shift || true

case "$cmd" in
  start)        cmd_start ;;
  stop)         cmd_stop ;;
  restart)      cmd_stop; cmd_start ;;
  status)       cmd_status ;;
  logs)         cmd_logs "${1:-}" ;;
  fingerprint)  cmd_fingerprint ;;
  sid)          cmd_sid ;;
  ""|help|-h|--help)
    cat <<'EOF'
tether dev start/stop helper

  scripts/dev.sh start         # boot daemon + tauri-app dev
  scripts/dev.sh stop          # graceful shutdown
  scripts/dev.sh restart       # stop + start
  scripts/dev.sh status        # which processes are alive
  scripts/dev.sh logs          # tail both logs
  scripts/dev.sh logs daemon   # tail daemon only
  scripts/dev.sh logs app      # tail app only
  scripts/dev.sh fingerprint   # print daemon DER cert SHA256
                               # (paste into Settings → Pinned cert)
  scripts/dev.sh sid           # list cc session IDs (most recent first)

State + logs live under .dev/ at the tether repo root (gitignored).

EOF
    ;;
  *) fatal "unknown command: $cmd (try: scripts/dev.sh help)" ;;
esac
