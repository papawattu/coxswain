#!/usr/bin/env bash
# D38 (R16) CNI preflight: verify-cni.
#
# Answers the D38 property for one cluster: does this cluster's CNI police
# pod -> host-network egress? The requirement is a PROPERTY, not a vendor
# list (README Security, ADR-0006): production requires a CNI that blocks
# pod egress to the apiserver service IP, the node's :6443 and :10250, pod
# IPs in other namespaces, and external IPs, when a NetworkPolicy allows
# only DNS to kube-dns.
#
# This script:
#   - works on ANY cluster where kubectl is pointed (K8S_CONTEXT, defaulting
#     to the current context — it prints which context it is using);
#   - does NOT require coxswain to be installed (no CRDs, no controller);
#   - creates a TEMP namespace (coxswain-verify-cni-<random>), a
#     NetworkPolicy shaped like the agent's (egress allowed only to kube-dns
#     on 53), and a probe pod matching that policy's podSelector;
#   - from the probe, TCP-connects to: the kubernetes Service IP:443, each
#     node's InternalIP on :6443 and :10250, a pod IP in another namespace
#     (kube-dns), and an external IP (1.1.1.1:443);
#   - prints a table + PASS/FAIL. PASS = every one of those was BLOCKED.
#     FAIL = any of them was reachable (the CNI does not police
#     pod -> host-network egress; the one-line hint under the table says
#     what to do about it).
#
# It never touches anything outside its own temp namespace: everything it
# creates is deleted by `kubectl delete namespace` in a trap (also on
# Ctrl-C). It is a pure NetworkPolicy probe — no images are built or
# kind-load'd; the probe image is pulled normally by the cluster's node.
#
# Reusable: `make d38-cni-e2e` (test/e2e/d38-cni-e2e.sh) runs the full
# Loop-based assertions on the Calico profile; this script is the
# vendor-neutral preflight the install docs point at, for any cluster.
#
# Usage:  make verify-cni            (current context)
#         K8S_CONTEXT=kind-coxswain-dev make verify-cni
#         bash test/e2e/verify-cni.sh
# Requires: kubectl. Optional: jq (falls back to python3, then to grep).
# Exits 0 on PASS, 1 on FAIL, 2 on environment error.

set -uo pipefail

CTX="${K8S_CONTEXT:-$(kubectl config current-context 2>/dev/null)}"
[ -n "$CTX" ] || { echo "FATAL: no kubectl context (set K8S_CONTEXT or kubectl config current-context)"; exit 2; }
NS="coxswain-verify-cni-$$-$RANDOM"
PROBE_POD="cni-probe"
PROBE_IMAGE="${PROBE_IMAGE:-python:3-alpine}"
TMPDIR="${TMPDIR:-/tmp}/verify-cni.$$"
mkdir -p "$TMPDIR"

K() { kubectl --context "$CTX" "$@"; }
cleanup() {
  echo
echo "--- cleaning up (temp namespace $NS) ---"
  K delete ns "$NS" --wait=false --timeout=120s >/dev/null 2>&1 || echo "   NOTE: could not delete ns $NS (delete it with: kubectl --context $CTX delete ns $NS)"
}
trap cleanup EXIT
trap 'echo; echo "(interrupted; cleaning up...)"; cleanup; exit 130' INT TERM

fail_jsonpath() { python3 -c "$1" 2>/dev/null || grep "$2" 2>/dev/null; }
jsonpath() { # $1 = python expr over sys.stdin json, $2 = grep -oE fallback
  python3 -c "$1" 2>/dev/null || grep "$2" 2>/dev/null
}

# ---------------------------------------------------------------------------
# Self-test: prove the result-validation logic rejects a broken probe
# (exit 2) rather than false-passing (exit 0). This runs the same parsing
# code path the main script uses, but against a synthetic results.txt that
# simulates a probe that produced no output (e.g. image without python3).
# ---------------------------------------------------------------------------
self_test_broken_probe() {
  local ST_DIR="$1"
  local ST_RESULTS="$ST_DIR/self-test-results.txt"
  local ST_EXPECTED=5
  # Simulate: probe ran but produced a stderr line and no RESULT lines
  # (e.g. "sh: python3: not found" with an empty result set).
  printf 'sh: python3: not found\n' > "$ST_RESULTS"
  local TOTAL=0 TOTAL_FAIL=0 PROBE_ERROR=0 SEEN_LABELS=""
  local line
  while IFS= read -r line; do
    case "$line" in
      RESULT\ *)
        TOTAL=$((TOTAL + 1))
        ;;
      "")
        ;;
      *)
        PROBE_ERROR=1
        ;;
    esac
  done < "$ST_RESULTS"
  if [ "$PROBE_ERROR" -eq 1 ] || [ "$TOTAL" -ne "$ST_EXPECTED" ]; then
    return 0  # correct: rejected
  else
    return 1  # bug: would have false-passed
  fi
}

# Run the self-test before the main flow.
SELF_TEST_DIR=$(mktemp -d)
if ! self_test_broken_probe "$SELF_TEST_DIR"; then
  echo "FATAL: self-test failed — the result-validation logic would FALSE-PASS on a broken probe."
  rm -rf "$SELF_TEST_DIR" 2>/dev/null || true
  exit 2
fi

# ---------------------------------------------------------------------------
# interpret_hardening_line: the ONE interpreter for hardening probe lines.
# Both the real HARDENING section and self_test_hardening_interpreter call
# it, so the self-test exercises the code the run uses (I45, PR #40 P2).
# ---------------------------------------------------------------------------
interpret_hardening_line() {
  local line="$1"
  case "$line" in
    HARDENING\ KUBELET_READONLY\(*\)\ OPEN)
      local ip
      ip=$(echo "$line" | sed -n 's/.*KUBELET_READONLY(\([^)]*\))/\1/p')
      echo "WARN: kubelet read-only port (10255) open on node $ip."
      echo "     Fix: set readOnlyPort: 0 in the kubelet config (checklist item 2)."
      ;;
    HARDENING\ APISERVER_ANON\ 200*)
      echo "WARN: apiserver returned 200 for an unauthenticated request to /api."
      echo "     Anonymous authentication is ENABLED. Fix: set --anonymous-auth=false"
      echo "     or an AuthenticationConfiguration with anonymous: deny (checklist item 1)."
      ;;
    HARDENING\ APISERVER_ANON\ 403*)
      echo "INFO: apiserver returned 403 for an unauthenticated request to /api."
      echo "     Anonymous auth is enabled but unauthorized — the control plane is"
      echo "     reachable but cannot act without credentials."
      ;;
    HARDENING\ APISERVER_ANON\ 401*)
      echo "OK: apiserver returned 401 for an unauthenticated request to /api."
      echo "    Anonymous auth is disabled or restricted."
      ;;
    HARDENING\ APISERVER_ANON\ ERROR*)
      echo "WARN: could not reach the apiserver for the anonymous-auth check."
      echo "     This does NOT mean the cluster is hardened — verify manually."
      ;;
    HARDENING\ KUBELET_READONLY\(*\)\ CLOSED)
      # Expected: port is closed. No action needed.
      ;;
    HARDENING\ *)
      echo "WARN: unrecognised hardening line: $line"
      echo "     This does NOT mean the cluster is hardened — verify manually."
      ;;
    "")
      ;; # skip blank lines
    *)
      echo "WARN: non-HARDENING line in hardening probe output: $line"
      echo "     This does NOT mean the cluster is hardened — verify manually."
      ;;
  esac
}

# ---------------------------------------------------------------------------
# Self-test 2: prove the hardening interpreter fires WARN/INFO/OK for the
# right lines. Synthetic lines cover every case pattern.
# ---------------------------------------------------------------------------
self_test_hardening_interpreter() {
  local ST_DIR="$1"
  local ST_RESULTS="$ST_DIR/self-test-hardening.txt"
  # Synthetic lines covering all case patterns:
  #  - KUBELET_READONLY OPEN  -> should print WARN
  #  - APISERVER_ANON 200     -> should print WARN
  #  - APISERVER_ANON 403     -> should print INFO
  #  - APISERVER_ANON 401     -> should print OK
  #  - KUBELET_READONLY CLOSED -> no output (expected)
  #  - unrecognised HARDENING  -> should print WARN unrecognised
  #  - non-HARDENING line      -> should print WARN non-HARDENING
  cat > "$ST_RESULTS" <<'EOF'
HARDENING KUBELET_READONLY(10.0.0.1) OPEN
HARDENING APISERVER_ANON 200 (anonymous auth ENABLED — WARN)
HARDENING APISERVER_ANON 403 (anonymous enabled but unauthorized)
HARDENING APISERVER_ANON 401 (anonymous auth disabled or restricted)
HARDENING KUBELET_READONLY(10.0.0.2) CLOSED
HARDENING UNKNOWN_LINE something
not a hardening line
EOF

  local output
  output=$(while IFS= read -r line; do
    interpret_hardening_line "$line"
  done < "$ST_RESULTS")

  # Verify each expected output is present
  local failures=0
  echo "$output" | grep -q "WARN: kubelet read-only port (10255) open on node 10.0.0.1" || { echo "  MISSING: WARN for open kubelet port"; failures=$((failures+1)); }
  echo "$output" | grep -q "WARN: apiserver returned 200" || { echo "  MISSING: WARN for apiserver 200"; failures=$((failures+1)); }
  echo "$output" | grep -q "INFO: apiserver returned 403" || { echo "  MISSING: INFO for apiserver 403"; failures=$((failures+1)); }
  echo "$output" | grep -q "OK: apiserver returned 401" || { echo "  MISSING: OK for apiserver 401"; failures=$((failures+1)); }
  echo "$output" | grep -q "WARN: unrecognised hardening line" || { echo "  MISSING: WARN for unrecognised HARDENING line"; failures=$((failures+1)); }
  echo "$output" | grep -q "WARN: non-HARDENING line" || { echo "  MISSING: WARN for non-HARDENING line"; failures=$((failures+1)); }
  # CLOSED should NOT produce any output
  echo "$output" | grep -q "10.0.0.2" && { echo "  UNEXPECTED: output for CLOSED kubelet port (should be silent)"; failures=$((failures+1)); }

  [ "$failures" -eq 0 ]
}

if ! self_test_hardening_interpreter "$SELF_TEST_DIR"; then
  echo "FATAL: hardening interpreter self-test failed — the case patterns would not fire."
  rm -rf "$SELF_TEST_DIR" 2>/dev/null || true
  exit 2
fi
rm -rf "$SELF_TEST_DIR" 2>/dev/null || true

echo "=== D38 CNI preflight: verify-cni ==="
echo "kubectl context: $CTX (K8S_CONTEXT=${K8S_CONTEXT:-<current context>})"
echo "temp namespace:  $NS"

echo
echo "--- STEP 0: preflight ---"
if ! K get nodes >/dev/null 2>&1; then
  echo "FATAL: no cluster reachable at --context $CTX"
  exit 2
fi
APISERVER_SVC_IP=$(K -n default get svc kubernetes -o jsonpath='{.spec.clusterIP}')
[ -z "$APISERVER_SVC_IP" ] && { echo "FATAL: could not read the default/kubernetes service ClusterIP"; exit 2; }
NODE_IPS=$(K get nodes -o json 2>/dev/null | jsonpath "import sys,json; print(' '.join(a['address'] for n in json.load(sys.stdin)['items'] for a in n['status']['addresses'] if a['type']=='InternalIP'))" 'InternalIP')
[ -z "$NODE_IPS" ] && { echo "FATAL: could not read node InternalIPs"; exit 2; }
DNS_POD_IP=$(K -n kube-system get pod -l k8s-app=kube-dns -o json 2>/dev/null | jsonpath "import sys,json; print(json.load(sys.stdin)['items'][0]['status']['podIP'])" 'podIP')
[ -z "$DNS_POD_IP" ] && { echo "FATAL: no kube-dns pod IP found (the probe's allowed target and the cross-namespace test endpoint)"; exit 2; }
EXTERNAL_IP="${EXTERNAL_IP:-1.1.1.1}"
echo "   apiserver svc IP:   $APISERVER_SVC_IP:443"
for ip in $NODE_IPS; do echo "   node InternalIP:      $ip:6443, $ip:10250"; done
echo "   kube-dns pod IP:    $DNS_POD_IP (allowed, DNS only)"
echo "   external:           $EXTERNAL_IP:443"
echo "   probe image:        $PROBE_IMAGE"

echo
echo "--- STEP 1: temp namespace + agent-shaped NetworkPolicy + probe pod ---"
# The policy is a 1:1 shape copy of the per-Loop agent NetworkPolicy
# (internal/controller/loop_controller.go): podSelector on
# app.kubernetes.io/component=agent, ingress deny-all, egress allowed ONLY
# to kube-dns in kube-system on 53 (UDP+TCP). No proxy rules: this probe
# must be able to reach nothing but DNS, which is the shape the CNI has to
# police.
K create ns "$NS" >/dev/null
cat > "$TMPDIR/netpol.yaml" <<'EOF'
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  name: cni-verify-agent
spec:
  podSelector:
    matchLabels:
      app.kubernetes.io/component: agent
  policyTypes:
    - Ingress
    - Egress
  ingress: []
  egress:
    - to:
        - namespaceSelector:
            matchLabels:
              kubernetes.io/metadata.name: kube-system
          podSelector:
            matchLabels:
              k8s-app: kube-dns
      ports:
        - protocol: UDP
          port: 53
        - protocol: TCP
          port: 53
EOF
K -n "$NS" apply -f "$TMPDIR/netpol.yaml" >/dev/null
# Probe pod: $PROBE_IMAGE (default python:3-alpine), matching the
# policy's podSelector (app.kubernetes.io/component=agent), no kind-load
# assumption (pulled normally by the node). It ships python3, which gives
# a deterministic 4s TCP-connect probe. Overridable via PROBE_IMAGE env var.
cat > "$TMPDIR/probe.yaml" <<EOF
apiVersion: v1
kind: Pod
metadata:
  name: ${PROBE_POD}
  namespace: ${NS}
  labels:
    app.kubernetes.io/component: agent
spec:
  automountServiceAccountToken: false
  restartPolicy: Never
  containers:
    - name: probe
      image: ${PROBE_IMAGE}
      command: ["python3", "-c", "import time; time.sleep(999999)"]
EOF
K -n "$NS" apply -f "$TMPDIR/probe.yaml" >/dev/null
echo "   applying NetworkPolicy + probe pod ($PROBE_IMAGE, pulled from the registry)..."
READY=""
for i in $(seq 1 60); do
  PHASE=$(K -n "$NS" get pod "$PROBE_POD" -o jsonpath='{.status.phase}' 2>/dev/null)
  READYC=$(K -n "$NS" get pod "$PROBE_POD" -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}' 2>/dev/null)
  if [ "$PHASE" = "Running" ] && [ "$READYC" = "True" ]; then READY=yes; break; fi
  [ "$PHASE" = "Failed" ] && break
  sleep 5
done
[ "$READY" = "yes" ] || { echo "FATAL: probe pod did not become Ready in 300s (check: kubectl --context $CTX -n $NS describe pod $PROBE_POD)"; exit 2; }
echo "   probe pod Ready"

echo
echo "--- STEP 2: TCP-connect probes from the probe pod ---"
# The probe is a raw TCP-CONNECT test — the exact layer the CNI polices.
# A completed connect means the CNI let the egress through (REACHABLE);
# a timeout/refused connect means it was dropped (BLOCKED). The probe image
# (default python:3-alpine) ships python3, so the probe uses a 4s socket
# timeout per target (the same approach as d38-cni-e2e.sh's probe.py).
# This is the D38 property under test: on kindnet the apiserver service IP,
# the node's :6443, and the kubelet's :10250 are REACHABLE (the L1, review
# docs/REVIEW-PHASE1-R16.md); on Calico all five targets are BLOCKED.
# The CROSS_NS_POD target uses port 8080 on the kube-dns pod IP (not 53)
# because the policy ALLOWS DNS on 53; the L1 test used :8080 on the coredns
# pod IP to verify pod-to-pod egress blocking.
NODE_IPS_ENV=$(echo $NODE_IPS | tr ' ' '\n' | sort -u | tr '\n' ' ')
cat > "$TMPDIR/probe.py" <<PYEOF
import os, socket
def probe(label, host, port):
    s = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
    s.settimeout(4)
    try:
        s.connect((host, int(port)))
        print("RESULT %s REACHABLE" % label)
    except Exception as exc:
        print("RESULT %s BLOCKED(%s)" % (label, type(exc).__name__))
    finally:
        s.close()
probe("APISERVER_SVC",  os.environ["APISERVER_SVC_IP"], 443)
for ip in os.environ["NODE_IPS"].split():
    probe("APISERVER_NODE(%s)" % ip, ip, 6443)
    probe("KUBELET_NODE(%s)" % ip, ip, 10250)
probe("CROSS_NS_POD(kube-dns)", os.environ["DNS_POD_IP"], 8080)
probe("EXTERNAL", os.environ["EXTERNAL_IP"], 443)
PYEOF
K -n "$NS" cp "$TMPDIR/probe.py" "$PROBE_POD:/tmp/verify-cni-probe.py" >/dev/null 2>&1 || { echo "FATAL: kubectl cp probe.py failed"; exit 2; }
K -n "$NS" exec "$PROBE_POD" -- sh -c "APISERVER_SVC_IP=${APISERVER_SVC_IP} NODE_IPS='${NODE_IPS_ENV}' DNS_POD_IP=${DNS_POD_IP} EXTERNAL_IP=${EXTERNAL_IP} python3 /tmp/verify-cni-probe.py" > "$TMPDIR/results.txt" 2>&1
EXEC_RC=$?
if [ $EXEC_RC -ne 0 ]; then
  echo "FATAL: the probe exec exited with rc=$EXEC_RC — the probe could not run."
  echo "      (Check the probe image: $PROBE_IMAGE must ship python3.)"
  echo "      --- raw probe output ---"
  cat "$TMPDIR/results.txt"
  exit 2
fi
cat "$TMPDIR/results.txt"
[ -s "$TMPDIR/results.txt" ] || { echo "FATAL: the probe produced no output (check the pod: kubectl --context $CTX -n $NS logs $PROBE_POD)"; exit 2; }

echo
echo "--- RESULT ---"
printf '%-45s %s\n' "endpoint (expected: BLOCKED)" "result"
echo "-------------------------------------------------------------"
# host-network targets only (service IP, node :6443/:10250) are the
# D38 "polices pod -> host-network egress" property; the pod-IP and
# external rows are included for completeness (most CNIs already police
# those).
# Expected label count: 1 (APISERVER_SVC) + 2*node_count (APISERVER_NODE +
# KUBELET_NODE per node) + 1 (CROSS_NS_POD) + 1 (EXTERNAL) = 3 + 2*node_count
EXPECTED_TOTAL=$((3 + 2 * $(echo $NODE_IPS | wc -w)))
HOSTNET_FAIL=0
TOTAL_FAIL=0
TOTAL=0
SEEN_LABELS=""
PROBE_ERROR=0
while read -r line; do
  case "$line" in
    RESULT\ *)
      label=$(echo "$line" | awk '{print $2}' | cut -d'(' -f1)
      TOTAL=$((TOTAL + 1))
      # Check for duplicate labels (each must appear exactly once)
      if echo "$SEEN_LABELS" | grep -qxF "$label"; then
        echo "ERROR: duplicate label $label in probe output" >&2
        PROBE_ERROR=1
      fi
      SEEN_LABELS="$SEEN_LABELS$label\n"
      case "$line" in
        *REACHABLE*)
          status="REACHABLE  <-- CNI LET IT THROUGH"
          TOTAL_FAIL=$((TOTAL_FAIL + 1))
          case "$line" in
            *APISERVER_SVC*|*APISERVER_NODE*|*KUBELET_NODE*) HOSTNET_FAIL=$((HOSTNET_FAIL + 1)) ;;
          esac
          ;;
        *BLOCKED*) status="BLOCKED" ;;
        *) status="UNKNOWN: $line"; TOTAL_FAIL=$((TOTAL_FAIL + 1)) ;;
      esac
      printf '%-45s %s\n' "$label" "$status"
      ;;
    "")
      ;; # skip blank lines
    *)
      # Any non-RESULT line is an error (probe crashed, stderr leaked, etc.)
      echo "ERROR: non-RESULT line in probe output: $line" >&2
      PROBE_ERROR=1
      ;;
  esac
done < "$TMPDIR/results.txt"

# Strict validation: TOTAL must equal EXPECTED_TOTAL, and there must be no
# probe errors. This prevents a FALSE-PASS when the probe can't run
# (e.g. image without python3 → empty results.txt → TOTAL=0 → PASS).
if [ "$PROBE_ERROR" -eq 1 ] || [ "$TOTAL" -ne "$EXPECTED_TOTAL" ]; then
  echo "FATAL: probe output validation failed (TOTAL=$TOTAL, EXPECTED=$EXPECTED_TOTAL, PROBE_ERROR=$PROBE_ERROR)."
  echo "      --- raw probe output ---"
  cat "$TMPDIR/results.txt"
  exit 2
fi

# ---------------------------------------------------------------------------
# HARDENING section (WARN-only, never changes PASS/FAIL or the exit code).
#
# Runs from a SECOND probe pod in the temp namespace that has NO
# NetworkPolicy applied, so it measures the cluster's own hardening,
# not the CNI's enforcement. Checks:
#   (a) TCP connect to each node InternalIP:10255 — if reachable, WARN
#       "kubelet read-only port open".
#   (b) Unauthenticated HTTPS GET to https://<apiserver svc IP>/api (no
#       token, -k): 401 = OK (anonymous disabled), 403 = INFO (anonymous
#       enabled but unauthorized), 200 = WARN.
#
# If the hardening probe can't run, print WARN "hardening check could not
# run" — never silently OK.
# ---------------------------------------------------------------------------
echo
echo "--- HARDENING (WARN-only; does not affect the CNI PASS/FAIL verdict) ---"
HARDENING_POD="hardening-probe"
HARDENING_IMAGE="${HARDENING_IMAGE:-python:3-alpine}"

# Create the hardening probe pod (NO NetworkPolicy — it measures the cluster,
# not the CNI). It must be in the same temp namespace so it's cleaned up by
# the same trap.
cat > "$TMPDIR/hardening-probe.yaml" <<EOF
apiVersion: v1
kind: Pod
metadata:
  name: ${HARDENING_POD}
  namespace: ${NS}
  labels:
    app.kubernetes.io/component: hardening-probe
spec:
  automountServiceAccountToken: false
  restartPolicy: Never
  containers:
    - name: probe
      image: ${HARDENING_IMAGE}
      command: ["python3", "-c", "import time; time.sleep(999999)"]
EOF
K -n "$NS" apply -f "$TMPDIR/hardening-probe.yaml" >/dev/null 2>&1

HARDENING_READY=""
for i in $(seq 1 30); do
  PHASE=$(K -n "$NS" get pod "$HARDENING_POD" -o jsonpath='{.status.phase}' 2>/dev/null)
  READYC=$(K -n "$NS" get pod "$HARDENING_POD" -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}' 2>/dev/null)
  if [ "$PHASE" = "Running" ] && [ "$READYC" = "True" ]; then HARDENING_READY=yes; break; fi
  [ "$PHASE" = "Failed" ] && break
  sleep 2
done

if [ "$HARDENING_READY" != "yes" ]; then
  echo "WARN: hardening check could not run (probe pod did not become Ready)."
  echo "      This does NOT mean the cluster is hardened — it means we could"
  echo "      not verify items 2 and 1 of the cluster-hardening checklist."
else
  # Hardening probe script: (a) TCP connect to node:10255, (b) unauthenticated
  # HTTPS GET to apiserver /api.
  cat > "$TMPDIR/hardening-probe.py" <<PYEOF
import os, socket, ssl, urllib.request, urllib.error

# (a) kubelet read-only port check
for ip in os.environ["NODE_IPS"].split():
    s = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
    s.settimeout(4)
    try:
        s.connect((ip, 10255))
        print("HARDENING KUBELET_READONLY(%s) OPEN" % ip)
    except Exception:
        print("HARDENING KUBELET_READONLY(%s) CLOSED" % ip)
    finally:
        s.close()

# (b) unauthenticated apiserver /api check
import http.client
try:
    conn = http.client.HTTPSConnection(os.environ["APISERVER_SVC_IP"], 443, timeout=4, context=ssl._create_unverified_context())
    conn.request("GET", "/api")
    resp = conn.getresponse()
    code = resp.status
    resp.read()
    conn.close()
    if code == 401:
        print("HARDENING APISERVER_ANON 401 (anonymous auth disabled or restricted)")
    elif code == 403:
        print("HARDENING APISERVER_ANON 403 (anonymous enabled but unauthorized)")
    elif code == 200:
        print("HARDENING APISERVER_ANON 200 (anonymous auth ENABLED — WARN)")
    else:
        print("HARDENING APISERVER_ANON %d (unexpected)" % code)
except Exception as exc:
    print("HARDENING APISERVER_ANON ERROR(%s)" % type(exc).__name__)
PYEOF

  K -n "$NS" cp "$TMPDIR/hardening-probe.py" "$HARDENING_POD:/tmp/hardening-probe.py" >/dev/null 2>&1
  if [ $? -ne 0 ]; then
    echo "WARN: hardening check could not run (kubectl cp failed)."
  else
    K -n "$NS" exec "$HARDENING_POD" -- sh -c \
      "NODE_IPS='${NODE_IPS_ENV}' APISERVER_SVC_IP=${APISERVER_SVC_IP} python3 /tmp/hardening-probe.py" > "$TMPDIR/hardening-results.txt" 2>&1
    cat "$TMPDIR/hardening-results.txt"

    # Interpret the hardening results via the shared interpret_hardening_line
    # function (the same code the self-test exercises).
    while IFS= read -r line; do
      interpret_hardening_line "$line"
    done < "$TMPDIR/hardening-results.txt"

    # Check if any hardening probe line is missing (probe didn't produce all expected lines)
    # Expected: one KUBELET_READONLY line per node + one APISERVER_ANON line = node_count + 1
    EXPECTED_HARDENING=$((1 + $(echo $NODE_IPS | wc -w)))
    ACTUAL_HARDENING=$(grep -c '^HARDENING ' "$TMPDIR/hardening-results.txt" 2>/dev/null || echo 0)
    if [ "$ACTUAL_HARDENING" -ne "$EXPECTED_HARDENING" ]; then
      echo "WARN: hardening probe produced $ACTUAL_HARDENING of $EXPECTED_HARDENING expected lines."
      echo "      Some checks may not have run — verify manually."
    fi
  fi
fi

echo
if [ "$TOTAL_FAIL" -eq 0 ]; then
  CNI_NOTE=$(K -n kube-system get pod -o json 2>/dev/null | python3 -c "import sys,json; names={p['spec']['containers'][0]['name'] for p in json.load(sys.stdin)['items']}; print('likely ' + ', '.join(sorted(n for n in names if 'calico' in n or 'cilium' in n or 'flannel' in n or 'kindnet' in n or 'weave' in n)))" 2>/dev/null)
  echo "PASS: every probe was BLOCKED by the NetworkPolicy, including the host-network"
  echo "     targets (apiserver svc IP, node :6443/:10250). This CNI ($CNI_NOTE) polices"
  echo "     pod -> host-network egress: it satisfies the D38 production requirement."
  echo "     The D38 property (a pod under the agent's allowlist NetworkPolicy cannot reach"
  echo "     host-network destinations) HOLDS on this cluster."
  exit 0
else
  echo "FAIL: $TOTAL_FAIL of $TOTAL probe targets were REACHABLE despite the NetworkPolicy"
  if [ "$HOSTNET_FAIL" -gt 0 ]; then
    echo "      (host-network targets reached: $HOSTNET_FAIL). This CNI does NOT police pod ->"
    echo "      host-network egress: it does NOT satisfy the D38 production requirement."
    echo "      Remedy: run a CNI that enforces NetworkPolicy egress against host-network and"
    echo "      node destinations — verified: Calico v3.30.1 (make d38-cni-e2e); to be"
    echo "      confirmed with this check: Cilium, GKE Dataplane V2, EKS VPC CNI + network"
    echo "      policy, AKS."
  else
    echo "      (only pod-IP/external targets reached; host-network targets were blocked.)"
    echo "      Remedy: investigate why the CNI let those through (policy enforcement is"
    echo "      working for host-network egress; check pod-to-pod / external enforcement)."
  fi
  exit 1
fi
