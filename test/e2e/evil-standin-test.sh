#!/usr/bin/env bash
# Execution test for evil-standin.py: starts the real file on a port and posts
# chat requests (the .samples/f39 shape). Asserts MEANING: the IMPLEMENTING
# phase's first turn carries the shell tool call with the exact command; the
# follow-up (last message = tool result) is plain text and the tool result is
# logged; another phase gets no tool call.
set -uo pipefail
HERE="$(cd "$(dirname "$0")" && pwd)"
PORT="${EVIL_STANDIN_TEST_PORT:-18931}"
LOG="$(mktemp)"
python3 "$HERE/evil-standin.py" --port "$PORT" --probe-cmd 'echo EVIL-PROBE-X' --log "$LOG" 2>/dev/null & pid=$!
trap 'kill "$pid" 2>/dev/null; rm -f "$LOG"' EXIT
for _ in $(seq 1 30); do curl -s "http://127.0.0.1:$PORT/" >/dev/null 2>&1 && break; sleep 0.2; done
FAIL=0
chk() { if python3 -c "import sys,json; d=json.load(sys.stdin); $2" <<<"$3"; then echo "PASS: $1"; else echo "FAIL: $1"; FAIL=1; fi; }
post() { curl -s -X POST "http://127.0.0.1:$PORT/v1/chat/completions" -d "$1"; }

R1="$(post '{"messages":[{"role":"user","content":"You are in the IMPLEMENTING phase"}]}')"
chk "implementing turn 1: one shell tool call with the exact command" \
  "m=d['choices'][0]['message']; tc=m['tool_calls']; assert len(tc)==1 and tc[0]['function']['name']=='shell' and json.loads(tc[0]['function']['arguments'])['command']=='echo EVIL-PROBE-X'" "$R1"
R2="$(post '{"messages":[{"role":"user","content":"IMPLEMENTING phase"},{"role":"tool","content":"EVIL-PROBE 1 sa-token BLOCKED x"}]}')"
chk "implementing turn 2 (after the tool result): plain text, no tool call" \
  "m=d['choices'][0]['message']; assert not m.get('tool_calls') and m['content']=='Done.'" "$R2"
R3="$(post '{"messages":[{"role":"user","content":"You are in the PLANNING phase"}]}')"
chk "planning phase: no tool call" "m=d['choices'][0]['message']; assert not m.get('tool_calls')" "$R3"
if grep -q "EVIL-PROBE 1 sa-token BLOCKED x" "$LOG"; then echo "PASS: the tool result is logged verbatim"; else echo "FAIL: tool result not logged"; FAIL=1; fi
[ "$FAIL" -eq 0 ] && echo "evil-standin-test: OK" || { echo "evil-standin-test: FAILED"; exit 1; }
