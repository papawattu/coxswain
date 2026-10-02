#!/usr/bin/env bash
# smoke.sh — acceptance smoke test for the webapi sample (SAMPLES-PLAN §1).
#
# Starts the server on a free port, curls /healthz and /api/v1/ping, and
# (task 2) POSTs to /api/v1/echo. Exits non-zero if any check fails.
#
# The seed state is red: /api/v1/ping returns 404 until task 1 lands;
# /api/v1/echo returns 404 until task 2 lands. The script reports one line
# per check so a failing Loop can see which one.
set -uo pipefail
cd "$(dirname "$0")/.."

PORT=$(python3 - <<'PY' 2>/dev/null || echo 0
import socket
s = socket.socket()
s.bind(("", 0))
print(s.getsockname()[1])
s.close()
PY
)
[ "$PORT" -gt 0 ] 2>/dev/null || PORT=0
if [ "$PORT" -eq 0 ]; then
  # Fallback: use a random high port.
  PORT=$(( (RANDOM * 20) + 30000 ))
fi

BIN="$(mktemp)"
trap 'kill "${SERVER_PID:-0}" 2>/dev/null; wait "${SERVER_PID:-0}" 2>/dev/null; rm -f "$BIN"' EXIT

go build -o "$BIN" . || { echo "build: FAIL"; exit 1; }
PORT=$PORT "$BIN" >/dev/null 2>&1 &
SERVER_PID=$!

# Wait for the server to come up (healthz must pass first).
ok=""
for i in $(seq 1 50); do
  if curl -sf "http://127.0.0.1:$PORT/healthz" 2>/dev/null | grep -q ok; then
    ok=1
    break
  fi
  sleep 0.1
done
if [ -z "$ok" ]; then
  echo "healthz: FAIL (server did not come up)"
  exit 1
fi
echo "healthz: PASS"

FAIL=0

# Task 1: /api/v1/ping returns 200 with body "pong".
body=$(curl -sf "http://127.0.0.1:$PORT/api/v1/ping" 2>/dev/null || true)
code=$(curl -s -o /dev/null -w '%{http_code}' "http://127.0.0.1:$PORT/api/v1/ping" 2>/dev/null || echo 000)
if [ "$code" = "200" ] && [ "$body" = "pong" ]; then
  echo "ping: PASS"
else
  echo "ping: FAIL (code=$code body=${body:-<empty>})"
  FAIL=1
fi

# Task 2: /api/v1/echo mirrors the request body back as JSON.
# The seed state has no echo route; the check is added to this script by
# tasks/2.patch together with the route itself.
exit "$FAIL"
