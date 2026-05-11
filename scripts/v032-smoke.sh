#!/usr/bin/env bash
# scripts/v032-smoke.sh — local validation of v0.3.2 endpoints.
#
# Required env:
#   TETHER_HOST  (default 127.0.0.1)
#   TETHER_PORT  (default 8898)
#
# Optional env for end-to-end token flow:
#   TETHER_JWT   tether_session cookie value — obtain from browser dev tools
#                after completing the /auth flow. Without it, only unauth
#                checks run.
set -euo pipefail

HOST="${TETHER_HOST:-127.0.0.1}"
PORT="${TETHER_PORT:-8898}"
BASE="https://${HOST}:${PORT}"

echo "smoke: connecting to ${BASE}"

# 1. Daemon liveness.
curl -sS -k "${BASE}/cert-hash" >/dev/null && echo "✓ daemon reachable"

# 2. OAuth discovery must be 404 in v0.3.2.
code=$(curl -sS -k -o /dev/null -w '%{http_code}' "${BASE}/.well-known/oauth-authorization-server")
[[ "$code" == "404" ]] || { echo "✗ expected 404 from well-known, got $code"; exit 1; }
echo "✓ /.well-known/oauth-authorization-server returns 404 (deferred to v0.3.3)"

# 3. /mcp without Bearer must be 401.
code=$(curl -sS -k -o /dev/null -w '%{http_code}' -X POST "${BASE}/mcp")
[[ "$code" == "401" ]] || { echo "✗ expected 401 from /mcp without Bearer, got $code"; exit 1; }
echo "✓ /mcp returns 401 without Bearer"

# 4. End-to-end token flow (only if TETHER_JWT provided).
if [[ -n "${TETHER_JWT:-}" ]]; then
  TOKEN_RESP=$(curl -sS -k \
    -H "Cookie: tether_session=${TETHER_JWT}" \
    -H "Content-Type: application/json" \
    -d '{"name":"v032-smoke"}' \
    "${BASE}/api/v1/mcp/tokens")
  RAW=$(echo "$TOKEN_RESP" | python3 -c 'import json,sys; print(json.load(sys.stdin)["token"])')
  ID=$(echo "$TOKEN_RESP"  | python3 -c 'import json,sys; print(json.load(sys.stdin)["id"])')
  echo "✓ created token id=${ID}"

  curl -sS -k \
    -H "Authorization: Bearer ${RAW}" \
    -H "Content-Type: application/json" \
    -d '{"jsonrpc":"2.0","method":"tools/list","id":1}' \
    -X POST "${BASE}/mcp" | grep -q '"jsonrpc"' \
    && echo "✓ /mcp tools/list succeeded with Bearer token"

  curl -sS -k -X DELETE \
    -H "Cookie: tether_session=${TETHER_JWT}" \
    "${BASE}/api/v1/mcp/tokens/${ID}" -o /dev/null -w '  revoke HTTP status: %{http_code}\n'
else
  echo "ⓘ skipping token flow (set TETHER_JWT to run end-to-end)"
fi

echo "v0.3.2 smoke OK"
