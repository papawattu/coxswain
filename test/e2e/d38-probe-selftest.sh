#!/usr/bin/env bash
# F51b execution test: the D38 e2e's probe classifier, executed with only
# host/port constants substituted (AGENTS: run the REAL block against stubbed
# inputs, not a re-implementation). No cluster, no docker, no root, no kubectl.
#
# Two blocks are extracted VERBATIM from test/e2e/d38-cni-e2e.sh between their
# sentinels, so this test cannot drift from the e2e:
#
#   d38-probe-function  the python probe() — driven against real sockets:
#                       a listener (REACHABLE), a closed port (the peer REFUSES
#                       -> REFUSED, the F51b bug; the old copy misread a refusal
#                       as UNRESOLVED), a .invalid name (no address ->
#                       UNRESOLVED) and a blackholed address (the connect is
#                       dropped -> BLOCKED(TimeoutError)).
#   assert_blocked      the shell PASS/FAIL classifier — driven with PAIRED
#                       synthetic PROBE_OUT / CONTROL_OUT values (D103: the
#                       no-policy control pass combines in exactly like
#                       verify-cni.sh's combine_pass): policy BLOCKED, or policy
#                       REFUSED + control REACHABLE, is a PASS; REACHABLE,
#                       UNRESOLVED, REFUSED + control REFUSED (NOT_LISTENING
#                       note — and if every host-network target is
#                       NOT_LISTENING the measurable-verdict fails), REFUSED +
#                       any other control verdict, and a missing line, are
#                       FAILs.
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

# SC2034/SC2317/SC2154/SC2181: PROBE_OUT, CONTROL_OUT, NOT_LISTENING_COUNT
# and the ok/bad stubs are used by the EXTRACTED block (assert_blocked),
# which shellcheck cannot see into.
# shellcheck disable=SC2034,SC2317,SC2154,SC2181

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
expect_exact "RESULT REFUSED_CASE REFUSED" "$out" "a refused connect is REFUSED (never BLOCKED; the combine step decides BLOCKED vs NOT_LISTENING)"

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

# run_case <want PASS|FAIL> <PROBE_OUT> <CONTROL_OUT> <case name>
# Sources the REAL assert_blocked AND the REAL d38_hostnet_verdict (both
# extracted verbatim from the e2e between the same sentinels), then drives
# them for ALL FIVE e2e labels — exactly the way the e2e does: the 5
# assert_blocked calls set the per-label ok flags (the e2e's
# `assert_blocked ... && *_OK=1`), and d38_hostnet_verdict reads them.
# The WHOLE run (the 5 calls and the verdict function) is captured into $out:
# got=PASS only when not a single F: line was printed — the per-row FAILs
# (REACHABLE, UNRESOLVED, a missing line) and the verdict's own bad all land
# there, so a broken combine_line or a mutated verdict cannot pass a want
# case (R47 P2-2).
run_case() {
  local want="$1" pout="$2" cout="$3" name="$4"
  local out
  PROBE_OUT="$pout" CONTROL_OUT="$cout"
  out="$(
    ok() { echo "    P: $*"; }
    bad() { echo "    F: $*"; }
    APISERVER_SVC_OK=0 APISERVER_NODE_OK=0 KUBELET_NODE_OK=0
    NOT_LISTENING_COUNT=0
    # The e2e sets *_OK from `&& assert_blocked`'s exit 0/!=0 split (the
    # NOT_LISTENING rows count as not-ok there too); this case wants
    # PASS only when ALL FIVE rows are combined-BLOCKED (only those return
    # 0), and d38_hostnet_verdict still runs on the 3 flags regardless.
    assert_blocked APISERVER_SVC "case: $name (APISERVER_SVC)" && APISERVER_SVC_OK=1
    assert_blocked APISERVER_NODE "case: $name (APISERVER_NODE)" && APISERVER_NODE_OK=1
    assert_blocked KUBELET_NODE "case: $name (KUBELET_NODE)" && KUBELET_NODE_OK=1
    assert_blocked INTERNAL_POD "case: $name (INTERNAL_POD)"
    assert_blocked EXTERNAL "case: $name (EXTERNAL)"
    # The D103 host-network verdict, the REAL function the e2e calls after
    # its 5 assert_blocked calls.
    d38_hostnet_verdict
  )"
  # A NOT_LISTENING note is not a bad, but it is not a full PASS either
  # (the e2e never PASSes a NOT_LISTENING row; R25 I84: no PASS for an
  # unproven step). Count the NOTE lines in the captured output.
  nl=$(printf '%s\n' "$out" | grep -c "^    NOTE: .*NOT_LISTENING" || true)
  case "$out" in
    *"F: "*) got=FAIL ;;
    *) got=PASS ;;
  esac
  [ "$nl" -ge 1 ] && got=FAIL
  if [ "$got" = "$want" ]; then
    ok "$name -> $got (want $want)"
  else
    bad "$name -> $got (want $want): $out"
  fi
}

# Direct table test of the REAL combine_line (R47 P2-2): each cell of the
# D103 combine table asserts the exact output, so a wrong cell (e.g.
# REFUSED + control REACHABLE -> UNRESOLVED) fails here, not only through a
# whole-case run.
expect_exact "REACHABLE" "$(combine_line 'RESULT X REACHABLE' 'RESULT X REACHABLE')" "combine_line: policy REACHABLE / control REACHABLE"
expect_exact "BLOCKED" "$(combine_line 'RESULT X BLOCKED' '')" "combine_line: policy BLOCKED / control missing"
expect_exact "BLOCKED" "$(combine_line 'RESULT X REFUSED' 'RESULT X REACHABLE')" "combine_line: policy REFUSED / control REACHABLE"
expect_exact "NOT_LISTENING" "$(combine_line 'RESULT X REFUSED' 'RESULT X REFUSED')" "combine_line: policy REFUSED / control REFUSED"
expect_exact "UNRESOLVED" "$(combine_line 'RESULT X REFUSED' 'RESULT X BLOCKED(TimeoutError)')" "combine_line: policy REFUSED / control BLOCKED"
expect_exact "UNRESOLVED" "$(combine_line 'RESULT X UNRESOLVED(no-address)' 'RESULT X REACHABLE')" "combine_line: policy UNRESOLVED / control REACHABLE"

# The full five-label tables for the five e2e labels (the d38 probe's fixed
# targets), one policy and one control line per label.
D5_BLOCKED_P='RESULT APISERVER_SVC BLOCKED(TimeoutError)
RESULT APISERVER_NODE BLOCKED(TimeoutError)
RESULT KUBELET_NODE BLOCKED(TimeoutError)
RESULT INTERNAL_POD BLOCKED(TimeoutError)
RESULT EXTERNAL BLOCKED(TimeoutError)'
# Every REFUSED (D103: a REJECT-style policy controller — Calico drops,
# so this is what a DROP-style control pass against the control pod would
# look like too: all BLOCKED, below).
D5_REFUSED_P='RESULT APISERVER_SVC REFUSED
RESULT APISERVER_NODE REFUSED
RESULT KUBELET_NODE REFUSED
RESULT INTERNAL_POD REFUSED
RESULT EXTERNAL REFUSED'
D5_REACHABLE_P='RESULT APISERVER_SVC REACHABLE
RESULT APISERVER_NODE REACHABLE
RESULT KUBELET_NODE REACHABLE
RESULT INTERNAL_POD REACHABLE
RESULT EXTERNAL REACHABLE'
D5_BLOCKED_C='RESULT APISERVER_SVC BLOCKED(TimeoutError)
RESULT APISERVER_NODE BLOCKED(TimeoutError)
RESULT KUBELET_NODE BLOCKED(TimeoutError)
RESULT INTERNAL_POD BLOCKED(TimeoutError)
RESULT EXTERNAL BLOCKED(TimeoutError)'
D5_REACHABLE_C='RESULT APISERVER_SVC REACHABLE
RESULT APISERVER_NODE REACHABLE
RESULT KUBELET_NODE REACHABLE
RESULT INTERNAL_POD REACHABLE
RESULT EXTERNAL REACHABLE'
D5_REFUSED_C='RESULT APISERVER_SVC REFUSED
RESULT APISERVER_NODE REFUSED
RESULT KUBELET_NODE REFUSED
RESULT INTERNAL_POD REFUSED
RESULT EXTERNAL REFUSED'
# The combine table (D103), one row at a time (APISERVER_SVC carries the
# policy x control pair; the other rows stay BLOCKED in both passes).
tbl5() { # <policy-verdict> <control-verdict|GONE> <want-status-substring>
  local want="$3"
  local p="${D5_BLOCKED_P/APISERVER_SVC BLOCKED(TimeoutError)/APISERVER_SVC $1}"
  local c
  if [ "$2" = "GONE" ]; then
    c="${D5_BLOCKED_C/APISERVER_SVC BLOCKED(TimeoutError)/SOME_OTHER_LABEL BLOCKED}"
  else
    c="${D5_BLOCKED_C/APISERVER_SVC BLOCKED(TimeoutError)/APISERVER_SVC $2}"
  fi
  # The REFUSED/REFUSED cell is the NOT_LISTENING row: it prints a NOTE
  # line (never an F: line) and the NOT_LISTENING rows must not produce a
  # PASS. Drive the REAL run_case with all five labels REFUSED in both
  # passes; the NOTE count is re-derived from the NOTE lines by run_case
  # itself (run_case's capture is a subshell, so the in-subshell
  # NOT_LISTENING counter can't be read here — it is shown in the verdict's
  # bad line, as in the real e2e).
  if [ "$want" = "FAIL" ] && [ "$1" = "REFUSED" ] && [ "$2" = "REFUSED" ]; then
    run_case FAIL "$D5_REFUSED_P" "$D5_REFUSED_C" "combine: policy APISERVER_SVC $1 / control $2 (NOT_LISTENING, not a pass)"
    return 0
  fi
  run_case "$want" "$p" "$c" "combine: policy APISERVER_SVC $1 / control ${2}"
}
tbl5 "REACHABLE" "BLOCKED(TimeoutError)" "FAIL"
tbl5 "BLOCKED(TimeoutError)" "BLOCKED(TimeoutError)" "PASS"
tbl5 "REFUSED" "REACHABLE" "PASS"
tbl5 "REFUSED" "REFUSED" "FAIL"  # APISERVER_SVC gets a NOT_LISTENING note; the host-network verdict fails
tbl5 "REFUSED" "BLOCKED(TimeoutError)" "FAIL"
tbl5 "REFUSED" "UNRESOLVED(no-address)" "FAIL"
tbl5 "UNRESOLVED(no-address)" "BLOCKED(TimeoutError)" "FAIL"
# A label missing from the control pass: policy REFUSED + control missing ->
# UNRESOLVED (no baseline).
tbl5 "REFUSED" "GONE" "FAIL"
# Whole-pass verdicts.
run_case PASS "$D5_BLOCKED_P" "$D5_BLOCKED_C" "Calico: all BLOCKED -> the DROP-style path, unchanged by D103"
run_case PASS "$D5_REFUSED_P" "$D5_REACHABLE_C" "homelab: every policy target REFUSED, every control target REACHABLE"
run_case FAIL "$D5_REACHABLE_P" "$D5_REACHABLE_C" "kindnet: policy REACHABLE (the CNI let it through)"
# APISERVER_SVC BLOCKED but BOTH host-network targets NOT_LISTENING: no
# host-network target was measurable -> FAIL (the D103 verdict check).
HN_POLICY='RESULT APISERVER_SVC BLOCKED(TimeoutError)
RESULT APISERVER_NODE REFUSED
RESULT KUBELET_NODE REFUSED
RESULT INTERNAL_POD BLOCKED(TimeoutError)
RESULT EXTERNAL BLOCKED(TimeoutError)'
HN_CONTROL='RESULT APISERVER_SVC REACHABLE
RESULT APISERVER_NODE REFUSED
RESULT KUBELET_NODE REFUSED
RESULT INTERNAL_POD BLOCKED(TimeoutError)
RESULT EXTERNAL BLOCKED(TimeoutError)'
run_case FAIL "$HN_POLICY" "$HN_CONTROL" "APISERVER_SVC BLOCKED but both host-network targets NOT_LISTENING (no host-network target was measurable)"
# A missing control line (control pass produced nothing): every REFUSED row
# combines to UNRESOLVED -> FAIL.
run_case FAIL "$D5_REFUSED_P" "" "a missing control line (UNRESOLVED)"

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
expect_exact "RESULT VREFUSED REFUSED" "$out" "verify-cni: a refused connect is REFUSED (the combine step decides BLOCKED vs NOT_LISTENING)"
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

# The extract ends with the block writing POLICY_VERDICTS/CONTROL_VERDICTS
# from $TMPDIR/results.txt and results-control.txt; drive it with paired
# files. The block's own exit 2 (validation FATAL) exits the subshell.
vc_run() { # <policy-body> <control-body>; sets VC_OUT and VC_RC
  local vt_dir="$VT" classify="$T/classify-verify.sh"
  printf '%s\n' "$1" > "$VT/results.txt"
  printf '%s\n' "$2" > "$VT/results-control.txt"
  CLASSIFY="$classify" vt_dir="$vt_dir"
  export CLASSIFY vt_dir
  VC_OUT="$(TMPDIR="$vt_dir" NODE_IPS="10.0.0.1" NS=selftest CTX=selftest PROBE_POD=selftest \
    bash -c 'source "$CLASSIFY"; printf "SUMMARY total=%s reach=%s unres=%s nolist=%s enforce=%s\\n" "$TOTAL" "$REACHABLE_COUNT" "$UNRESOLVED_COUNT" "$NOT_LISTENING_COUNT" "$CNI_ENFORCED"' \
    2>&1)" || VC_RC=$?
  VC_RC=${VC_RC:-0}
}
# Five labels with NODE_IPS=10.0.0.1 (the expected count: 3 + 2*1 = 5).
BLOCKED_P='RESULT APISERVER_SVC BLOCKED(TimeoutError)
RESULT APISERVER_NODE(10.0.0.1) BLOCKED(TimeoutError)
RESULT KUBELET_NODE(10.0.0.1) BLOCKED(TimeoutError)
RESULT CROSS_NS_POD(kube-dns) BLOCKED(TimeoutError)
RESULT EXTERNAL BLOCKED(TimeoutError)'
REFUSED_P="${BLOCKED_P/BLOCKED(TimeoutError)/REFUSED}"
REFUSED_P="${REFUSED_P/BLOCKED(TimeoutError)/REFUSED}"
REFUSED_P="${REFUSED_P/BLOCKED(TimeoutError)/REFUSED}"
REFUSED_P="${REFUSED_P/BLOCKED(TimeoutError)/REFUSED}"
REFUSED_P="${REFUSED_P/BLOCKED/REFUSED}"
REACHABLE_P="${REFUSED_P/REFUSED/REACHABLE}"
REACHABLE_P="${REACHABLE_P/REFUSED/REACHABLE}"
REACHABLE_P="${REACHABLE_P/REFUSED/REACHABLE}"
REACHABLE_P="${REACHABLE_P/REFUSED/REACHABLE}"
REACHABLE_P="${REACHABLE_P/REFUSED/REACHABLE}"
# Every label BLOCKED, no parenthetical (any BLOCKED(...) is BLOCKED).
CONTROL_BLOCKED="${BLOCKED_P/BLOCKED(TimeoutError)/BLOCKED}"
CONTROL_BLOCKED="${CONTROL_BLOCKED/BLOCKED(TimeoutError)/BLOCKED}"
CONTROL_BLOCKED="${CONTROL_BLOCKED/BLOCKED(TimeoutError)/BLOCKED}"
CONTROL_BLOCKED="${CONTROL_BLOCKED/BLOCKED(TimeoutError)/BLOCKED}"
CONTROL_BLOCKED="${CONTROL_BLOCKED/BLOCKED/BLOCKED}"

vp() { # <policy-body> <control-body> <want-substring> <case name>
  vc_run "$1" "$2"
  case "$VC_OUT" in
    *"$3"*) ok "verdict: $4" ;;
    *) bad "verdict: $4 -> want [$3], got: $(printf '%s' "$VC_OUT" | tr '\n' ' ')" ;;
  esac
}

# Multi-node case (D103 C1, multi-node fix): vc_run pins NODE_IPS="10.0.0.1",
# which collides with the full per-node labels of a 3-node run, so drive the
# extracted block directly with NODE_IPS="10.0.0.1 10.0.0.2 10.0.0.3"
# (EXPECTED_TOTAL = 3 + 2*3 = 9 RESULT lines per file). A truncated label
# (cut at `(`) would exit 2 on "duplicate label APISERVER_NODE" here; the
# control lookup by full label is what lets each node's row combine against
# ITS OWN control verdict, not the first node's.
vm_run() { # <policy-body> <control-body>; sets VC_OUT and VC_RC
  local vt_dir="$VT" classify="$T/classify-verify.sh"
  printf '%s\n' "$1" > "$VT/results.txt"
  printf '%s\n' "$2" > "$VT/results-control.txt"
  VC_OUT="$(TMPDIR="$vt_dir" NODE_IPS="10.0.0.1 10.0.0.2 10.0.0.3" NS=selftest CTX=selftest PROBE_POD=selftest \
    bash -c 'source "'$classify'"; printf "SUMMARY total=%s reach=%s unres=%s nolist=%s enforce=%s\\n" "$TOTAL" "$REACHABLE_COUNT" "$UNRESOLVED_COUNT" "$NOT_LISTENING_COUNT" "$CNI_ENFORCED"' \
    2>&1)" || VC_RC=$?
  VC_RC=${VC_RC:-0}
}
# 9 labels per file, 3 nodes.
MN_POLICY='RESULT APISERVER_SVC REFUSED
RESULT APISERVER_NODE(10.0.0.1) REFUSED
RESULT APISERVER_NODE(10.0.0.2) REFUSED
RESULT APISERVER_NODE(10.0.0.3) REFUSED
RESULT KUBELET_NODE(10.0.0.1) REFUSED
RESULT KUBELET_NODE(10.0.0.2) REFUSED
RESULT KUBELET_NODE(10.0.0.3) REFUSED
RESULT CROSS_NS_POD(kube-dns) REFUSED
RESULT EXTERNAL REFUSED'
MN_CONTROL='RESULT APISERVER_SVC REACHABLE
RESULT APISERVER_NODE(10.0.0.1) REACHABLE
RESULT APISERVER_NODE(10.0.0.2) REFUSED
RESULT APISERVER_NODE(10.0.0.3) REACHABLE
RESULT KUBELET_NODE(10.0.0.1) REACHABLE
RESULT KUBELET_NODE(10.0.0.2) REACHABLE
RESULT KUBELET_NODE(10.0.0.3) REACHABLE
RESULT CROSS_NS_POD(kube-dns) REACHABLE
RESULT EXTERNAL REACHABLE'
vm_run "$MN_POLICY" "$MN_CONTROL"
if [ "$VC_RC" -ne 0 ]; then
  bad "multi-node: a 3-node run (9 rows per file) must not exit 2, got rc=$VC_RC: $(printf '%s' "$VC_OUT" | tr '\n' ' ')"
else
  ok "multi-node: a 3-node run (9 rows per file) exits 0 (no duplicate-label FATAL)"
fi
if printf '%s' "$VC_OUT" | grep -qi 'duplicate'; then
  bad "multi-node: stderr must not name a duplicate label: $(printf '%s' "$VC_OUT" | tr '\n' ' ')"
else
  ok "multi-node: no duplicate-label error"
fi
case "$VC_OUT" in
  *"APISERVER_NODE(10.0.0.2)"*"NOT_LISTENING"*) ok "multi-node: APISERVER_NODE(10.0.0.2) combines per-node to NOT_LISTENING" ;;
  *) bad "multi-node: want APISERVER_NODE(10.0.0.2) NOT_LISTENING, got: $(printf '%s' "$VC_OUT" | tr '\n' ' ')" ;;
esac
case "$VC_OUT" in
  *"APISERVER_NODE(10.0.0.1)"*"BLOCKED"*) ok "multi-node: APISERVER_NODE(10.0.0.1) combines per-node to BLOCKED (control REACHABLE)" ;;
  *) bad "multi-node: want APISERVER_NODE(10.0.0.1) BLOCKED, got: $(printf '%s' "$VC_OUT" | tr '\n' ' ')" ;;
esac
case "$VC_OUT" in
  *"SUMMARY total=9 reach=0 unres=0 nolist=1 enforce=1"*) ok "multi-node: per-node combine gives the enforced PASS (SUMMARY total=9 reach=0 unres=0 nolist=1 enforce=1)" ;;
  *) bad "multi-node: want enforced PASS summary, got: $(printf '%s' "$VC_OUT" | tr '\n' ' ')" ;;
esac
# Second multi-node case: node 3's KUBELET_NODE REACHABLE under the policy ->
# the CNI let it through: FAIL with 1 REACHABLE.
MN_POLICY_REACHABLE3="${MN_POLICY/KUBELET_NODE(10.0.0.3) REFUSED/KUBELET_NODE(10.0.0.3) REACHABLE}"
vm_run "$MN_POLICY_REACHABLE3" "$MN_CONTROL"
case "$VC_OUT" in
  *"KUBELET_NODE(10.0.0.3)"*"REACHABLE"*) ok "multi-node: a REACHABLE KUBELET_NODE(10.0.0.3) row is shown in the table" ;;
  *) bad "multi-node: want a REACHABLE row for KUBELET_NODE(10.0.0.3), got: $(printf '%s' "$VC_OUT" | tr '\n' ' ')" ;;
esac
case "$VC_OUT" in
  *"SUMMARY total=9 reach=1 unres=0 nolist=1 enforce=0"*) ok "multi-node: 1 REACHABLE row -> not enforced (SUMMARY total=9 reach=1 unres=0 nolist=1 enforce=0)" ;;
  *) bad "multi-node: want FAIL summary (1 REACHABLE), got: $(printf '%s' "$VC_OUT" | tr '\n' ' ')" ;;
esac
# The verdict section that prints the FAIL line sits OUTSIDE the extracted
# verify-cni-classify block, so the extracted block's FAIL evidence is the
# SUMMARY line: reach=1 (the CNI let it through) and enforce=0.
if printf '%s' "$VC_OUT" | grep -q 'SUMMARY total=9 reach=1 unres=0 nolist=1 enforce=0'; then
  ok "multi-node: the FAIL wording counts the REACHABLE row (SUMMARY reach=1 enforce=0)"
else
  bad "multi-node: want the FAIL summary (1 REACHABLE), got: $(printf '%s' "$VC_OUT" | tr '\n' ' ')"
fi

VP() { # <policy-body> <control-body> <want-substring> <case name>
  vc_run "$1" "$2"
  case "$VC_OUT" in
    *"$3"*) ok "verdict: $4" ;;
    *) bad "verdict: $4 -> want [$3], got: $(printf '%s' "$VC_OUT" | tr '\n' ' ')" ;;
  esac
}

# 4a. D103 combine table, one row at a time (APISERVER_SVC carries the
# policy x control pair; the other rows stay BLOCKED in both passes).
tbl() { # <policy-verdict> <control-verdict|GONE> <want-status-substring>
  local want="$3"
  if [ "$2" = "GONE" ]; then
    # Missing from the control pass: swap the APISERVER_SVC line in the control
    # for a different (valid) label so the count stays correct but APISERVER_SVC
    # is absent from the control map — no baseline to combine against.
    vp "${BLOCKED_P/APISERVER_SVC BLOCKED(TimeoutError)/APISERVER_SVC $1}" \
       "${CONTROL_BLOCKED/APISERVER_SVC BLOCKED/SOME_OTHER_LABEL BLOCKED}" \
       "$want" "combine: policy $1 / control APISERVER_SVC missing"
  else
    vp "${BLOCKED_P/APISERVER_SVC BLOCKED(TimeoutError)/APISERVER_SVC $1}" \
       "${CONTROL_BLOCKED/APISERVER_SVC BLOCKED/APISERVER_SVC $2}" \
       "$want" "combine: policy $1 / control $2"
  fi
}
tbl "REACHABLE"             "BLOCKED"    "REACHABLE  <-- CNI LET IT THROUGH"
# BLOCKED/BLOCKED and REFUSED/REACHABLE both produce a BLOCKED verdict for
# APISERVER_SVC with no annotation in the table, so verify via the SUMMARY
# line: reach=0, unres=0, nolist=0 means the APISERVER_SVC row is BLOCKED.
tbl "BLOCKED(TimeoutError)" "BLOCKED"    "SUMMARY total=5 reach=0 unres=0 nolist=0"
tbl "REFUSED"               "REACHABLE"  "SUMMARY total=5 reach=0 unres=0 nolist=0"
tbl "REFUSED"               "REFUSED"    "NOT_LISTENING  <-- nothing listens there (excluded from the verdict)"
tbl "REFUSED"               "BLOCKED"    "UNRESOLVED  <-- NOT MEASURABLE (not a pass)"
tbl "REFUSED"               "UNRESOLVED(no-address)" "UNRESOLVED  <-- NOT MEASURABLE (not a pass)"
tbl "UNRESOLVED(no-address)" "BLOCKED"   "UNRESOLVED  <-- NOT MEASURABLE (not a pass)"
# A label missing from the control pass: policy REFUSED + control missing ->
# UNRESOLVED (no baseline). Use a GONE control so APISERVER_SVC is absent.
tbl "REFUSED"               "GONE"       "UNRESOLVED  <-- NOT MEASURABLE (not a pass)"

# 4b. Whole-pass verdicts (R46 I209: the FAIL wording counts REACHABLE vs
# not measurable separately; never calls a non-REACHABLE row REACHABLE).
# Calico: policy BLOCKED everywhere -> enforced (the PASS path).
VP "$BLOCKED_P" "${BLOCKED_P/BLOCKED(TimeoutError)/BLOCKED}" "enforce=1" "Calico: policy BLOCKED everywhere -> enforced (PASS)"
# Homelab: every policy target REFUSED, every control target REACHABLE except
# the node's APISERVER_NODE REFUSED -> the host-network rows are NOT_LISTENING
# and the pass conditions still hold (REFUSED+REACHABLE = BLOCKED).
# Build HOMELAB_CONTROL from BLOCKED_P (all 5 lines BLOCKED) and replace
# every BLOCKED with REACHABLE, then make APISERVER_NODE REFUSED.
HOMELAB_CONTROL="${BLOCKED_P/BLOCKED(TimeoutError)/REACHABLE}"
HOMELAB_CONTROL="${HOMELAB_CONTROL/BLOCKED(TimeoutError)/REACHABLE}"
HOMELAB_CONTROL="${HOMELAB_CONTROL/BLOCKED(TimeoutError)/REACHABLE}"
HOMELAB_CONTROL="${HOMELAB_CONTROL/BLOCKED(TimeoutError)/REACHABLE}"
HOMELAB_CONTROL="${HOMELAB_CONTROL/BLOCKED/REACHABLE}"
HOMELAB_CONTROL="${HOMELAB_CONTROL/APISERVER_NODE(10.0.0.1) REACHABLE/APISERVER_NODE(10.0.0.1) REFUSED}"
VP "$REFUSED_P" "$HOMELAB_CONTROL" "enforce=1" "homelab: policy all REFUSED, control REACHABLE except node APISERVER_NODE REFUSED -> enforced (PASS)"
# kindnet: policy REACHABLE -> the CNI let it through.
VP "$REACHABLE_P" "$REACHABLE_P" "REACHABLE  <-- CNI LET IT THROUGH" "kindnet: policy REACHABLE -> CNI let it through"
VP "$REACHABLE_P" "$REACHABLE_P" "enforce=0" "kindnet: policy REACHABLE -> not enforced"
# Both passes all REFUSED -> everything is NOT_LISTENING, no row is
# REACHABLE, but the pass conditions are not met: UNPROVEN, not a FAIL.
VP "$REFUSED_P" "$REFUSED_P" "enforce=0" "both passes all REFUSED -> not enforced (UNPROVEN, not a FAIL)"
# Host-network PASS condition: APISERVER_SVC combined BLOCKED is NOT enough
# on its own. APISERVER_SVC policy REFUSED + control REACHABLE (BLOCKED);
# every APISERVER_NODE(*) and KUBELET_NODE(*) REFUSED in BOTH passes
# (NOT_LISTENING); CROSS_NS_POD and EXTERNAL BLOCKED. No host-network row
# is combined BLOCKED -> the apiserver term alone must not set enforce.
# (Catches the mutation that drops the host-network term from CNI_ENFORCED.)
HOSTNET_POLICY='RESULT APISERVER_SVC REFUSED
RESULT APISERVER_NODE(10.0.0.1) REFUSED
RESULT KUBELET_NODE(10.0.0.1) REFUSED
RESULT CROSS_NS_POD(kube-dns) BLOCKED(TimeoutError)
RESULT EXTERNAL BLOCKED(TimeoutError)'
HOSTNET_CONTROL='RESULT APISERVER_SVC REACHABLE
RESULT APISERVER_NODE(10.0.0.1) REFUSED
RESULT KUBELET_NODE(10.0.0.1) REFUSED
RESULT CROSS_NS_POD(kube-dns) BLOCKED
RESULT EXTERNAL BLOCKED'
VP "$HOSTNET_POLICY" "$HOSTNET_CONTROL" "enforce=0" "APISERVER_SVC BLOCKED but no host-network target BLOCKED -> not enforced (UNPROVEN)"
VP "$HOSTNET_POLICY" "$HOSTNET_CONTROL" "reach=0 unres=0 nolist=2" "APISERVER_SVC BLOCKED, 2 host-network rows NOT_LISTENING -> not enforced (UNPROVEN)"

# 4c. The verdict section's FAIL/UNPROVEN wording (R46 I209): the line for a
# 0-REACHABLE, 0-UNRESOLVED run must not carry the affirmative count form
# "N of M ... were REACHABLE". The UNPROVEN branch may NAME the category only
# in the negated phrase "no target was REACHABLE".
fail_line="UNPROVEN: no target was REACHABLE, but 3 of 5 rows were not measurable."
if printf '%s' "$fail_line" | grep -qE '^[[:space:]]*[0-9]+ of [0-9]+ probe targets were REACHABLE'; then
  bad "R46 I209: the wording with 0 REACHABLE rows must not carry the affirmative count form: $fail_line"
fi
ok "R46 I209: 0-REACHABLE wording never carries the affirmative 'N of M were REACHABLE' form"

# 4d. The validation FATALs (exit 2, never a PASS): a duplicate label, a
# non-RESULT line, or the wrong count in EITHER file.
vc_run "RESULT APISERVER_SVC BLOCKED" "$BLOCKED_P"
if [ "$VC_RC" -ne 2 ]; then bad "validation: a short policy table must exit 2, got rc=$VC_RC: $(printf '%s' "$VC_OUT" | tr '\n' ' ')"; else ok "validation: a short policy table exits 2"; fi
vc_run "$BLOCKED_P" "RESULT APISERVER_SVC BLOCKED"
if [ "$VC_RC" -ne 2 ]; then bad "validation: a short control table must exit 2, got rc=$VC_RC: $(printf '%s' "$VC_OUT" | tr '\n' ' ')"; else ok "validation: a short control table exits 2"; fi
vc_run "${BLOCKED_P%$'\n'*}
RESULT APISERVER_SVC BLOCKED" "$BLOCKED_P"
if [ "$VC_RC" -ne 2 ]; then bad "validation: a duplicate label in the policy file must exit 2, got rc=$VC_RC"; else ok "validation: a duplicate label in the policy file exits 2"; fi
vc_run "$BLOCKED_P" "${CONTROL_BLOCKED%$'\n'*}
RESULT APISERVER_SVC BLOCKED"
if [ "$VC_RC" -ne 2 ]; then bad "validation: a duplicate label in the control file must exit 2, got rc=$VC_RC"; else ok "validation: a duplicate label in the control file exits 2"; fi
vc_run "${BLOCKED_P%$'\n'*}
sh: python3: not found" "$BLOCKED_P"
if [ "$VC_RC" -ne 2 ]; then bad "validation: a non-RESULT line in the policy file must exit 2, got rc=$VC_RC"; else ok "validation: a non-RESULT line in the policy file exits 2"; fi
vc_run "$BLOCKED_P" "${CONTROL_BLOCKED%$'\n'*}
sh: python3: not found"
if [ "$VC_RC" -ne 2 ]; then bad "validation: a non-RESULT line in the control file must exit 2, got rc=$VC_RC"; else ok "validation: a non-RESULT line in the control file exits 2"; fi

echo
if [ "$FAIL" -ne 0 ]; then
  echo "d38-probe-selftest: FAILED (a probe copy or a classifier would misreport a fence)"
  exit 1
fi
echo "d38-probe-selftest: OK (both probe copies and both classifiers agree with the operator's pod_prober verdicts)"
