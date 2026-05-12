#!/usr/bin/env bash
# bench-e2e.sh — K.9 end-to-end performance baseline measurement
#
# Prerequisites:
#   - tether server running and accessible at TETHER_URL
#   - Valid auth token in TETHER_TOKEN (from ~/.tether/access-token)
#   - curl 8.0+ (HTTP/3 support via --http3-only)
#   - jq
#
# Usage:
#   TETHER_URL=https://gcp-dev.stevenforai.top:8897 \
#   TETHER_TOKEN=$(cat ~/.tether/access-token) \
#   ./scripts/bench-e2e.sh

set -euo pipefail

TETHER_URL="${TETHER_URL:-https://localhost:8898}"
TETHER_TOKEN="${TETHER_TOKEN:-}"
REPEAT="${REPEAT:-5}"

red()   { printf '\033[0;31m%s\033[0m\n' "$*"; }
green() { printf '\033[0;32m%s\033[0m\n' "$*"; }
bold()  { printf '\033[1m%s\033[0m\n' "$*"; }

require() { command -v "$1" >/dev/null 2>&1 || { red "missing: $1"; exit 1; }; }
require curl
require jq
require bc

bold "=== tether K.9 E2E benchmark ==="
echo "  URL:    $TETHER_URL"
echo "  repeat: $REPEAT"
echo ""

auth_header=""
if [[ -n "$TETHER_TOKEN" ]]; then
    auth_header="-H 'Authorization: Bearer $TETHER_TOKEN'"
fi

# ─────────────────────────────────────────────────────────────────────────────
# K.9.1 helper: TCP HTTPS round-trip latency (healthz endpoint)
# Not the full K.9.1 criterion (which requires cc subprocess), but establishes
# the network baseline from which to reason about cc startup overhead.
# ─────────────────────────────────────────────────────────────────────────────
bold "--- TCP/HTTPS latency baseline (GET /healthz) ---"
total=0
for i in $(seq 1 "$REPEAT"); do
    ms=$(curl -sk -o /dev/null -w '%{time_total}' \
        ${TETHER_TOKEN:+-H "Authorization: Bearer $TETHER_TOKEN"} \
        "$TETHER_URL/healthz" | awk '{printf "%.1f", $1*1000}')
    echo "  run $i: ${ms} ms"
    total=$(echo "$total + $ms" | bc)
done
avg=$(echo "scale=1; $total / $REPEAT" | bc)
echo "  avg: ${avg} ms"
echo ""

# ─────────────────────────────────────────────────────────────────────────────
# K.9.1 manual check (requires observing server logs / browser DevTools)
# Automated measurement is not feasible from a shell script because cc startup
# emits the system/init event over the WebTransport stream, not an HTTP endpoint.
# ─────────────────────────────────────────────────────────────────────────────
bold "--- K.9.1: cc subprocess cold start ≤ 5s ---"
echo "  MANUAL: start a new session in the browser and observe the time from"
echo "  page load until the chat pane shows the first assistant prompt."
echo "  Target: ≤ 5 seconds on a warm machine (Go binary already in disk cache)."
echo ""

# ─────────────────────────────────────────────────────────────────────────────
# K.9.2 manual check (requires a running cc session + browser)
# ─────────────────────────────────────────────────────────────────────────────
bold "--- K.9.2: user input → first assistant text ≤ 1s (warm) ---"
echo "  MANUAL: in an active session, type a short prompt ('hi') and measure"
echo "  the time to first assistant text event in browser DevTools → Network →"
echo "  WebTransport stream. Target: ≤ 1 second."
echo ""

# ─────────────────────────────────────────────────────────────────────────────
# K.9.3: HTTP/3 reachability (proxy for WT multi-stream non-blocking)
# Full multi-stream test requires a WebTransport client; this verifies QUIC is up.
# ─────────────────────────────────────────────────────────────────────────────
bold "--- K.9.3: HTTP/3 / QUIC reachability ---"
HOST=$(echo "$TETHER_URL" | sed 's|https://||' | cut -d: -f1)
PORT=$(echo "$TETHER_URL" | sed 's|https://||' | cut -d: -f2)
if nc -u -z -w 3 "$HOST" "$PORT" 2>/dev/null; then
    green "  UDP $HOST:$PORT reachable"
else
    red "  UDP $HOST:$PORT NOT reachable — VPN/firewall likely blocking QUIC"
    echo "  Fix: disconnect VPN (see README K.8.1)"
fi

h3_result=$(curl -sk --http3-only -o /dev/null -w '%{http_code} %{time_total}' \
    ${TETHER_TOKEN:+-H "Authorization: Bearer $TETHER_TOKEN"} \
    "$TETHER_URL/healthz" 2>/dev/null || echo "failed")
if [[ "$h3_result" == failed* ]] || [[ -z "$h3_result" ]]; then
    red "  HTTP/3 request failed (curl may not have HTTP/3 support, or QUIC is blocked)"
else
    code=$(echo "$h3_result" | awk '{print $1}')
    ms=$(echo "$h3_result" | awk '{printf "%.1f", $2*1000}')
    if [[ "$code" == "200" ]]; then
        green "  HTTP/3 GET /healthz → 200 (${ms} ms)"
    else
        red "  HTTP/3 GET /healthz → $code (${ms} ms)"
    fi
fi
echo ""
echo "  K.9.3 multi-stream non-blocking:"
echo "  MANUAL: open tether in the browser, start a chat that produces long streaming"
echo "  output, then simultaneously upload a file or open a shell. Verify both"
echo "  channels flow without the chat stream stalling."
echo ""

bold "=== summary ==="
echo "  K.9.1 cc cold start    — MANUAL (observe browser DevTools, target ≤ 5s)"
echo "  K.9.2 input→text warm  — MANUAL (observe browser DevTools, target ≤ 1s)"
echo "  K.9.3 HTTP/3 reachable — see above"
echo ""
echo "  In-process baselines (go test -bench, see internal/ packages):"
echo "    permission roundtrip: ~20µs (httptest)"
echo "    gateway dispatch:     ~1µs  (in-memory)"
