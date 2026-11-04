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
