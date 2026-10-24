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
# PF3: a multi-line command is sent one line per tool call, in order
python3 "$HERE/evil-standin.py" --port "$((PORT+1))" --probe-cmd $'echo ONE\necho TWO' 2>/dev/null & pid2=$!
trap 'kill "$pid" "$pid2" 2>/dev/null; rm -f "$LOG"' EXIT
for _ in $(seq 1 30); do curl -s "http://127.0.0.1:$((PORT+1))/" >/dev/null 2>&1 && break; sleep 0.2; done
post2() { curl -s -X POST "http://127.0.0.1:$((PORT+1))/v1/chat/completions" -d "$1"; }
M1="$(post2 '{"messages":[{"role":"user","content":"IMPLEMENTING phase"}]}')"
M2="$(post2 '{"messages":[{"role":"user","content":"IMPLEMENTING phase"},{"role":"assistant","tool_calls":[{}]},{"role":"tool","content":"r1"}]}')"
M3="$(post2 '{"messages":[{"role":"user","content":"IMPLEMENTING phase"},{"role":"tool","content":"r1"},{"role":"tool","content":"r2"}]}')"
chk "multi-line: call 1 is line 1" "tc=d['choices'][0]['message']['tool_calls']; assert json.loads(tc[0]['function']['arguments'])['command']=='echo ONE'" "$M1"
chk "multi-line: after one result, call 2 is line 2" "tc=d['choices'][0]['message']['tool_calls']; assert json.loads(tc[0]['function']['arguments'])['command']=='echo TWO'" "$M2"
chk "multi-line: after both results, plain text" "m=d['choices'][0]['message']; assert not m.get('tool_calls') and m['content']=='Done.'" "$M3"
R3="$(post '{"messages":[{"role":"user","content":"You are in the PLANNING phase"}]}')"
chk "planning phase: no tool call" "m=d['choices'][0]['message']; assert not m.get('tool_calls')" "$R3"
if grep -q "EVIL-PROBE 1 sa-token BLOCKED x" "$LOG"; then echo "PASS: the tool result is logged verbatim"; else echo "FAIL: tool result not logged"; FAIL=1; fi
curl -s "http://127.0.0.1:$PORT/v1/models" >/dev/null; sleep 0.3
if grep -q '^GET /v1/models from 127.0.0.1$' "$LOG"; then echo "PASS: a GET is logged with its client IP (probe 8 evidence)"; else echo "FAIL: GET not logged with the client"; FAIL=1; fi
[ "$FAIL" -eq 0 ] && echo "evil-standin-test: OK" || { echo "evil-standin-test: FAILED"; exit 1; }
