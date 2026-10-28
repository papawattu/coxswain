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
#                       connect is dropped -> BLOCKED(TimeoutError)).
#   assert_blocked      the shell PASS/FAIL classifier — driven with synthetic
#                       PROBE_OUT values: BLOCKED is the only PASS.
#
# …and the same two blocks out of test/e2e/verify-cni.sh (the operator-facing
# D38 preflight, P2-1 of #342's pre-review):
#
#   verify-cni-probe-function   its probe() — the same four socket cases
#   verify-cni-classify         its RESULT-table loop + validation — driven with
#                               synthetic results.txt files: BLOCKED alone is a
#                               pass, UNRESOLVED is a FAIL, and a short/empty
#                               table is the FATAL (exit 2) validation.
#
# Exits non-zero on any failed case. Logs to stdout.

# SC2034/SC2317: PROBE_OUT and the ok/bad stubs are used by the EXTRACTED block
# (assert_blocked), which shellcheck cannot see into.
# shellcheck disable=SC2034,SC2317

set -uo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
# D38_E2E_SRC / VERIFY_CNI_SRC let a mutation harness point these at scratch
# copies of the scripts (a mutation is applied INSIDE the extracted blocks, so
# the test must still read them from a real file). The diffs and the exact
# commands are recorded in .samples/f51b/ (P3-4 of #342's pre-review).
D38_E2E_SRC_DEFAULT="$HERE/d38-cni-e2e.sh"
VERIFY_CNI_SRC_DEFAULT="$HERE/verify-cni.sh"
SRC="${D38_E2E_SRC:-$D38_E2E_SRC_DEFAULT}"
VSRC="${VERIFY_CNI_SRC:-$VERIFY_CNI_SRC_DEFAULT}"
[ -f "$SRC" ] || { echo "FATAL: $SRC not found"; exit 2; }
[ -f "$VSRC" ] || { echo "FATAL: $VSRC not found"; exit 2; }
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

# extract <sentinel-name> [file]: the lines between "# >>> <name>" and
# "# <<< <name>" (file defaults to $SRC).
extract() {
  local file="${2:-$SRC}"
  awk -v s="$1" '
    index($0, "# >>> " s) == 1 { on = 1; next }
    index($0, "# <<< " s) == 1 { on = 0 }
    on { print }
  ' "$file"
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
# Accept several connections: the d38 probe and the verify-cni probe both run
# their REACHABLE case against this one listener.
s.settimeout(20)
for _ in range(4):
    try:
        conn, _addr = s.accept()
        conn.close()
    except Exception:
        break
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
# dropped — the one verdict that is evidence of enforcement. Pinned to
# BLOCKED(TimeoutError): with only `BLOCKED(.*)` a host with no route at all
# (ENETUNREACH) would pass this case without exercising the timeout path
# (P3-2 of #342's pre-review).
out="$(python3 "$T/drive.py" "$T/probe.py" DROPPED_CASE 10.255.255.1 443 2>&1)"
expect_exact "RESULT DROPPED_CASE BLOCKED(TimeoutError)" "$out" "a dropped connect is BLOCKED(TimeoutError)"

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

# ===========================================================================
# 3. verify-cni.sh's probe function (the operator-facing D38 preflight), run
#    for real on the same four socket cases. P2-1 of #342's pre-review: its
#    copy classified every exception as BLOCKED, and its table accepted BLOCKED
#    as a pass, so `make verify-cni` could PASS a non-enforcing cluster.
# ===========================================================================
extract verify-cni-probe-function "$VSRC" > "$T/vprobe.py"
if [ ! -s "$T/vprobe.py" ]; then
  echo "FATAL: could not extract the verify-cni-probe-function block from $VSRC"
  exit 2
fi
if [ -n "$LISTEN_PORT" ]; then
  out="$(python3 "$T/drive.py" "$T/vprobe.py" VREACHABLE 127.0.0.1 "$LISTEN_PORT" 2>&1)"
  expect_exact "RESULT VREACHABLE REACHABLE" "$out" "verify-cni: a listening port is REACHABLE"
fi
out="$(python3 "$T/drive.py" "$T/vprobe.py" VREFUSED 127.0.0.1 "$CLOSED_PORT" 2>&1)"
expect_exact "RESULT VREFUSED UNRESOLVED(refused)" "$out" "verify-cni: a refused connect is UNRESOLVED"
out="$(python3 "$T/drive.py" "$T/vprobe.py" VNOADDR verify-cni-selftest.invalid 443 2>&1)"
expect_exact "RESULT VNOADDR UNRESOLVED(no-address)" "$out" "verify-cni: an unresolvable target is UNRESOLVED"
out="$(python3 "$T/drive.py" "$T/vprobe.py" VDROPPED 10.255.255.1 443 2>&1)"
expect_exact "RESULT VDROPPED BLOCKED(TimeoutError)" "$out" "verify-cni: a dropped connect is BLOCKED(TimeoutError)"

# ===========================================================================
# 4. verify-cni.sh's RESULT table + validation, run for real on synthetic probe
#    output. The block is sourced in a subshell; it leaves TOTAL/TOTAL_FAIL/
#    HOSTNET_UNPROVEN behind and exits 2 on the validation FATAL, so the
#    harness reads the summary line and the subshell's status.
# ===========================================================================
extract verify-cni-classify "$VSRC" > "$T/classify-verify.sh"
if [ ! -s "$T/classify-verify.sh" ]; then
  echo "FATAL: could not extract the verify-cni-classify block from $VSRC"
  exit 2
fi
VT="$T/verify-classify"
mkdir -p "$VT"

cat > "$VT/run.sh" <<'RUNNER'
# Source the REAL extracted block, then print what it decided. The block's own
# exit 2 (the validation FATAL) exits this runner, so the harness can see it.
. "$1"
printf 'SUMMARY total=%s fail=%s hostnet=%s unproven=%s\n' \
  "$TOTAL" "$TOTAL_FAIL" "$HOSTNET_FAIL" "$HOSTNET_UNPROVEN"
RUNNER

# vrun <want-exit> <results.txt body> <case name>; sets VERIFY_OUT.
vrun() {
  local want="$1" body="$2" name="$3" rc=0 out=""
  printf '%s\n' "$body" > "$VT/results.txt"
  out="$(cd "$VT" && TMPDIR="$VT" NODE_IPS="10.0.0.1" NS=selftest CTX=selftest PROBE_POD=selftest \
    bash "$VT/run.sh" "$T/classify-verify.sh" 2>&1)" || rc=$?
  VERIFY_OUT="$out"
  if [ "$rc" -ne "$want" ]; then
    bad "verify-cni classify: $name -> exit $rc (want $want): $(printf '%s' "$out" | tr '\n' ' ')"
    return
  fi
  ok "verify-cni classify: $name -> exit $rc"
}

ALL_BLOCKED='RESULT APISERVER_SVC BLOCKED(TimeoutError)
RESULT APISERVER_NODE(x) BLOCKED(TimeoutError)
RESULT KUBELET_NODE(x) BLOCKED(TimeoutError)
RESULT CROSS_NS_POD BLOCKED(TimeoutError)
RESULT EXTERNAL BLOCKED(TimeoutError)'

# The pass path: every row BLOCKED, nothing counted as a failure.
vrun 0 "$ALL_BLOCKED" "all blocked (the pass path)"
if ! printf '%s' "$VERIFY_OUT" | grep -q "SUMMARY total=5 fail=0"; then
  bad "verify-cni classify: all blocked -> $(printf '%s' "$VERIFY_OUT" | tr '\n' ' ') (want total=5 fail=0)"
fi

# A reachable target is a failure, and it is counted as reached.
vrun 0 "${ALL_BLOCKED/APISERVER_SVC BLOCKED(TimeoutError)/APISERVER_SVC REACHABLE}" "one reachable target"
if ! printf '%s' "$VERIFY_OUT" | grep -q "REACHABLE  <-- CNI LET IT THROUGH"; then
  bad "verify-cni classify: a reachable target must print the LET-IT-THROUGH status: $(printf '%s' "$VERIFY_OUT" | tr '\n' ' ')"
fi
if ! printf '%s' "$VERIFY_OUT" | grep -q "SUMMARY total=5 fail=1 hostnet=1 unproven=0"; then
  bad "verify-cni classify: a reachable target must count as reached: $(printf '%s' "$VERIFY_OUT" | tr '\n' ' ')"
fi

# P2-1's case: a REFUSED host-network target. It was a PASS before this PR; it
# must now be a failure the table calls NOT MEASURABLE, and it must NOT be
# counted as a host-network target that was reached.
vrun 0 "${ALL_BLOCKED/APISERVER_SVC BLOCKED(TimeoutError)/APISERVER_SVC UNRESOLVED(refused)}" "host-network refused (P2-1: was a pass)"
if ! printf '%s' "$VERIFY_OUT" | grep -q "UNRESOLVED  <-- NOT MEASURABLE (not a pass)"; then
  bad "verify-cni classify: a refused host-network target must print NOT MEASURABLE: $(printf '%s' "$VERIFY_OUT" | tr '\n' ' ')"
fi
if ! printf '%s' "$VERIFY_OUT" | grep -q "SUMMARY total=5 fail=1 hostnet=0 unproven=1"; then
  bad "verify-cni classify: a refused host-network target must be counted unproven: $(printf '%s' "$VERIFY_OUT" | tr '\n' ' ')"
fi

# The validation FATAL: a table that is short of the expected labels exits 2
# (never a PASS with no probe output).
vrun 2 "RESULT APISERVER_SVC BLOCKED(TimeoutError)" "a short table (the probe did not run)"

echo
if [ "$FAIL" -ne 0 ]; then
  echo "d38-probe-selftest: FAILED (a probe copy or a classifier would misreport a fence)"
  exit 1
fi
echo "d38-probe-selftest: OK (both probe copies and both classifiers agree with the operator's pod_prober verdicts)"
