#!/usr/bin/env bash
# F51b execution test: the D38 e2e's probe classifier, executed with only
# host/port constants substituted (AGENTS: run the REAL block against stubbed
# inputs, not a re-implementation). No cluster, no docker, no root, no kubectl.
#
# Two blocks are extracted VERBATIM from test/e2e/d38-cni-e2e.sh between their
# sentinels, so this test cannot drift from the e2e:
#
#   d38-probe-function  the python probe() — driven against real local sockets:
#                       a listener (REACHABLE), a closed port (the peer REFUSES
#                       -> UNRESOLVED, the F51b bug), a .invalid name (no
#                       address -> UNRESOLVED) and a blackholed address (the
#                       connect is dropped -> BLOCKED).
#   assert_blocked      the shell PASS/FAIL classifier — driven with synthetic
#                       PROBE_OUT values: BLOCKED is the only PASS.
#
# Exits non-zero on any failed case. Logs to stdout.

# SC2034/SC2317: PROBE_OUT and the ok/bad stubs are used by the EXTRACTED block
# (assert_blocked), which shellcheck cannot see into.
# shellcheck disable=SC2034,SC2317

set -uo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
# D38_E2E_SRC lets a mutation harness point this at a scratch copy of the
# script (the mutation is applied INSIDE the extracted blocks, so the test must
# still read them from a real file).
SRC="${D38_E2E_SRC:-$HERE/d38-cni-e2e.sh}"
[ -f "$SRC" ] || { echo "FATAL: $SRC not found"; exit 2; }
T="$(mktemp -d)"
LISTENER_PID=""
cleanup() {
  [ -n "$LISTENER_PID" ] && kill "$LISTENER_PID" 2>/dev/null
  rm -rf "$T"
}
trap cleanup EXIT

FAIL=0
ok() { echo "PASS: $*"; }
bad() { echo "FAIL: $*"; FAIL=1; }

# extract <sentinel-name>: the lines between "# >>> <name>" and "# <<< <name>".
extract() {
  awk -v s="$1" '
    index($0, "# >>> " s) == 1 { on = 1; next }
    index($0, "# <<< " s) == 1 { on = 0 }
    on { print }
  ' "$SRC"
}

# expect_line <want: exact line | regex: pattern> <got> <case name>
expect_exact() { # $1 = want, $2 = got, $3 = case name
  if [ "$2" = "$1" ]; then
    ok "$3 -> $2"
  else
    bad "$3 -> $2 (want $1)"
  fi
}

# ===========================================================================
# 1. The python probe function, run for real.
# ===========================================================================
extract d38-probe-function > "$T/probe.py"
if [ ! -s "$T/probe.py" ]; then
  echo "FATAL: could not extract the d38-probe-function block from $SRC"
  exit 2
fi

# The driver: exec the REAL extracted function, then call it for one target.
# Only the host/port constants are substituted (the e2e passes pod/node IPs
# and fixed ports in the same way, via probe()'s arguments).
cat > "$T/drive.py" <<'PYEOF'
import socket
import sys

src, label, host, port = sys.argv[1], sys.argv[2], sys.argv[3], int(sys.argv[4])
with open(src) as fh:
    exec(compile(fh.read(), src, "exec"), globals())
probe(label, host, port, 2)
PYEOF

# A listener on a free port: the connect must succeed.
python3 - "$T/port" >"$T/listener.log" 2>&1 <<'PYEOF' &
import socket
import sys

s = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
s.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
s.bind(("127.0.0.1", 0))
s.listen(8)
with open(sys.argv[1], "w") as fh:
    fh.write(str(s.getsockname()[1]))
s.settimeout(20)
try:
    s.accept()
except Exception:
    pass
PYEOF
LISTENER_PID=$!
for _ in $(seq 1 100); do
  [ -s "$T/port" ] && break
  sleep 0.1
done
LISTEN_PORT="$(cat "$T/port" 2>/dev/null || true)"
if [ -z "$LISTEN_PORT" ]; then
  bad "could not start the local listener fixture ($(cat "$T/listener.log" 2>/dev/null))"
else
  out="$(python3 "$T/drive.py" "$T/probe.py" REACHABLE_CASE 127.0.0.1 "$LISTEN_PORT" 2>&1)"
  expect_exact "RESULT REACHABLE_CASE REACHABLE" "$out" "a listening port is REACHABLE"
fi

# A closed loopback port: the peer REFUSES. This is the F51b bug — a refusal
# proves the packet arrived, so it must be UNRESOLVED, never BLOCKED.
CLOSED_PORT="$(python3 - <<'PYEOF'
import socket
s = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
s.bind(("127.0.0.1", 0))
port = s.getsockname()[1]
s.close()  # nothing listens on port any more
print(port)
PYEOF
)"
out="$(python3 "$T/drive.py" "$T/probe.py" REFUSED_CASE 127.0.0.1 "$CLOSED_PORT" 2>&1)"
expect_exact "RESULT REFUSED_CASE UNRESOLVED(refused)" "$out" "a refused connect is UNRESOLVED (never BLOCKED)"

# A name with no address (RFC 6761 .invalid): no measurement of the fence.
out="$(python3 "$T/drive.py" "$T/probe.py" NOADDR_CASE d38-probe-selftest.invalid 443 2>&1)"
expect_exact "RESULT NOADDR_CASE UNRESOLVED(no-address)" "$out" "an unresolvable target is UNRESOLVED"

# A blackholed address: the connect is neither accepted nor refused, so it is
# dropped — the one verdict that is evidence of enforcement.
out="$(python3 "$T/drive.py" "$T/probe.py" DROPPED_CASE 10.255.255.1 443 2>&1)"
if printf '%s\n' "$out" | grep -q '^RESULT DROPPED_CASE BLOCKED(.*)$'; then
  ok "a dropped connect is BLOCKED -> $out"
else
  bad "a dropped connect must be BLOCKED(<reason>) -> $out"
fi

# ===========================================================================
# 2. The shell classifier, run for real on synthetic probe output.
# ===========================================================================
extract assert_blocked > "$T/classify.sh"
if [ ! -s "$T/classify.sh" ]; then
  echo "FATAL: could not extract the assert_blocked block from $SRC"
  exit 2
fi
# shellcheck source=/dev/null
. "$T/classify.sh"

# run_case <want PASS|FAIL> <PROBE_OUT> <case name>
run_case() {
  local want="$1" line="$2" name="$3" out got
  PROBE_OUT="$line"
  out="$(
    ok() { echo "   PASS: $*"; }
    bad() { echo "   FAIL: $*"; }
    assert_blocked APISERVER_SVC "case: $name"
  )"
  case "$out" in
    *"PASS:"*) got=PASS ;;
    *"FAIL:"*) got=FAIL ;;
    *) got=UNKNOWN ;;
  esac
  if [ "$got" = "$want" ]; then
    ok "$name -> $got (want $want)"
  else
    bad "$name -> $got (want $want): $out"
  fi
}

run_case PASS "RESULT APISERVER_SVC BLOCKED(TimeoutError)" "a dropped connect is a PASS"
run_case PASS "RESULT APISERVER_SVC BLOCKED(OSError)" "any dropped-connect reason is a PASS"
run_case FAIL "RESULT APISERVER_SVC REACHABLE" "a reachable expected-BLOCKED target is a FAIL"
run_case FAIL "RESULT APISERVER_SVC UNRESOLVED(refused)" "a REFUSED connect is a FAIL, not a pass (F51b)"
run_case FAIL "RESULT APISERVER_SVC UNRESOLVED(no-address)" "an unresolvable target is a FAIL, not a pass (F51b)"
run_case FAIL "RESULT OTHER_LABEL BLOCKED(TimeoutError)" "a missing label line is a FAIL"
run_case FAIL "RESULT APISERVER_SVC FOO" "an unrecognised result is a FAIL"

echo
if [ "$FAIL" -ne 0 ]; then
  echo "d38-probe-selftest: FAILED (the D38 e2e's probe classifier would misreport a fence)"
  exit 1
fi
echo "d38-probe-selftest: OK (the D38 e2e's probe classifier agrees with the operator's pod_prober verdicts)"
