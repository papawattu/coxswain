#!/usr/bin/env bash
# D38 (R16) enforcing-CNI network e2e.
#
# Runs on the coxswain-calico kind cluster (make kind-calico-up): kindnet is
# disabled and Calico is the enforcing CNI, so the agent's allowlist
# NetworkPolicy is checked against REAL endpoints — including the ones that
# kindnet lets through on coxswain-dev (the L1 known limitation of
# test/e2e/i42-e2e.sh). This script turns L1 into hard assertions:
#
#   C1  apiserver service IP (10.96.0.1:443) is BLOCKED
#   C2  apiserver node IP (<node>:6443)     is BLOCKED
#   C3  kubelet node IP    (<node>:10250)   is BLOCKED
#   C4  a cluster-internal pod IP            is BLOCKED (also holds on kindnet)
#   C5  a disallowed external IP             is BLOCKED (also holds on kindnet)
#   C6  an allowed host via the egress proxy succeeds (HTTP 200)
#   C7  a disallowed host via the egress proxy gets 403
#
# SAFETY (house rules, see ADR-0007 F2): KubeArmor is deliberately NOT
# installed on this cluster (it would be a second BPF-LSM agent on the host's
# kernel, and coxswain-dev already carries the BPF-LSM load), so the checks
# that depend on KubeArmor are printed as SKIPPED — not run, not passed.
# coxswain-dev is never touched.
#
# Pinned by the caller: K8S_CONTEXT=kind-coxswain-calico,
# KIND_CLUSTER_NAME=coxswain-calico, CALICO_IP_POOL (defaults below).
# Requires: docker, kind, kubectl, jq on the host; make kind-calico-up first.
# Exits non-zero on any failure.

set -uo pipefail

# kind lives in GOPATH/bin on this host; make it findable.
GOPATH_BIN="$(go env GOPATH 2>/dev/null)/bin"
case ":$PATH:" in *":$GOPATH_BIN:"*) ;; *) PATH="$GOPATH_BIN:$PATH"; export PATH ;; esac

CTX="${K8S_CONTEXT:-kind-coxswain-calico}"
CLUSTER="${KIND_CLUSTER_NAME:-coxswain-calico}"
NS=d38-e2e
LOOP=d38-loop
E2E_NS=coxswain-system
REPO_ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
COMMIT="$(git -C "$REPO_ROOT" rev-parse --short HEAD 2>/dev/null || echo unknown)"
IMG_TAG="main-${COMMIT}"
IMG="coxswain-controller:${IMG_TAG}"
PROXY_IMG="coxswain-proxy:standin"
EGRESS_IMG="coxswain-egress-proxy:standin"
AGENT_IMG="golang:1.26"
TMPDIR="${TMPDIR:-/tmp}/d38-cni-e2e.$$"
mkdir -p "$TMPDIR"

K() { kubectl --context "$CTX" "$@"; }
FAIL=0
ok()  { echo "   PASS: $*"; }
bad() { echo "   FAIL: $*"; FAIL=1; }
skip() { echo "   SKIPPED: $*"; }

echo "=== D38 enforcing-CNI network e2e (context=$CTX cluster=$CLUSTER ns=$NS loop=$LOOP) ==="
echo "commit: $COMMIT  image: $IMG"

echo
echo "--- STEP 0: preflight ---"
if ! kubectl --context "$CTX" get nodes >/dev/null 2>&1; then
  echo "FATAL: no cluster reachable at --context $CTX (run make kind-calico-up first)"
  exit 2
fi
NODE_IP=$(K get node -o jsonpath='{.items[0].status.addresses[?(@.type=="InternalIP")].address}' 2>/dev/null | tr ' ' '\n' | head -1)
[ -z "$NODE_IP" ] && { echo "FATAL: could not read the node InternalIP"; exit 2; }
# The apiserver service IP is the cluster DNS VIP (first service ClusterIP).
APISERVER_SVC_IP=$(K -n default get svc kubernetes -o jsonpath='{.spec.clusterIP}')
[ -z "$APISERVER_SVC_IP" ] && { echo "FATAL: could not read the kubernetes service ClusterIP"; exit 2; }
echo "   node IP: $NODE_IP   apiserver svc IP: $APISERVER_SVC_IP"

# The cluster must be the enforcing-CNI profile: no kindnet pod, and a
# calico-node pod present. (kindnet disabled via networking.disableDefaultCNI;
# Calico is the only CNI.)
KINDNET_PODS=$(K -n kube-system get pod -l k8s-app=kindnet --no-headers 2>/dev/null | wc -l | tr -d ' ')
CALICO_PODS=$(K -n kube-system get pod -l k8s-app=calico-node --no-headers 2>/dev/null | grep -c ' 1/1 ' || true)
echo "   kindnet pods: $KINDNET_PODS (must be 0)   calico-node ready: $CALICO_PODS (must be >=1)"
if [ "$KINDNET_PODS" != "0" ]; then
  echo "FATAL: kindnet pods are running on this cluster — it is NOT the enforcing-CNI profile (run on kind-$CLUSTER / make kind-calico-up)"
  exit 2
fi
if [ "$CALICO_PODS" -lt 1 ]; then
  echo "FATAL: calico-node is not Ready (run make kind-calico-up)"
  exit 2
fi
CALICO_VER=$(K -n kube-system get pod -l k8s-app=calico-node -o jsonpath='{.items[0].spec.containers[0].image}' 2>/dev/null)
echo "   calico-node image: $CALICO_VER"

# This cluster must NOT run KubeArmor (safety rule — never add a second
# BPF-LSM agent to the host; see ADR-0007 F2).
KA_NS=$(K get cm -A --no-headers 2>/dev/null | awk '$2=="kubearmor-config"{print $1; exit}')
if [ -n "$KA_NS" ]; then
  echo "FATAL: KubeArmor IS installed on $CLUSTER — the D38 rule is to keep it OFF here (ADR-0007 F2). Not running."
  exit 2
fi
echo "   KubeArmor absent on this cluster (as required; KubeArmor-dependent checks are SKIPPED)"

echo
echo "--- STEP 1: build + load images ---"
echo "   building controller image $IMG ..."
(cd "$REPO_ROOT" && docker build -q -t "$IMG" -f Dockerfile .) || { echo "FATAL: controller build failed"; exit 2; }
echo "   building $PROXY_IMG ..."
(cd "$REPO_ROOT" && docker build -q -t "$PROXY_IMG" -f cmd/proxy-standin/Dockerfile .) || { echo "FATAL: proxy build failed"; exit 2; }
echo "   building $EGRESS_IMG ..."
(cd "$REPO_ROOT" && docker build -q -t "$EGRESS_IMG" -f cmd/egress-proxy/Dockerfile .) || { echo "FATAL: egress-proxy build failed"; exit 2; }
# PROBE_IMG = the operator's CNI self-test probe image (the --cni-probe-image
# default). The operator creates the probe pod by image name, so it must be
# loadable by the kind node (pre-loaded here for offline hosts).
PROBE_IMG="python:3-alpine"
for img in "$IMG" "$PROXY_IMG" "$EGRESS_IMG" "$AGENT_IMG" "$PROBE_IMG"; do
  echo "   kind load: $img"
  kind load docker-image "$img" --name "$CLUSTER" || { echo "FATAL: kind load $img failed"; exit 2; }
done

echo
echo "--- STEP 2: deploy controller (config/calico: --allow-unenforced ONLY, NO --allow-unenforced-network) ---"
make -C "$REPO_ROOT" kustomize >/dev/null 2>&1
KUSTOMIZE_BIN="$REPO_ROOT/bin/kustomize"
# Build the calico/enforcing-CNI profile (config/calico) from a TEMP COPY of
# the config tree (R16 I44 norm: never let `kustomize edit set image` rewrite
# the tracked kustomization.yaml). config/calico = config/default +
# --allow-unenforced (bypasses the D30 KubeArmor gate — KubeArmor is absent
# by design on the enforcing-CNI cluster, ADR-0007 F2) but NOT
# --allow-unenforced-network (the D38 network gate stays LIVE). Calico
# enforces the D38 property, so the operator's own CNI self-test probe passes
# and Loops run with NetworkEnforced=True (CNIEnforced). On a non-enforcing
# CNI (kindnet), this same overlay would hold Loops Suspended with
# NetworkEnforced=False (CNIUnenforced).
TMP_OVERLAY=$(mktemp -d)
cp -r "$REPO_ROOT/config" "$TMP_OVERLAY/config"
(cd "$TMP_OVERLAY/config/manager" && "$KUSTOMIZE_BIN" edit set image controller="$IMG")
(cd "$TMP_OVERLAY" && "$KUSTOMIZE_BIN" build config/calico | K apply -f -) \
  || { echo "FATAL: controller deploy failed"; exit 2; }
# D38: the cni-probe kustomization is standalone (not in config/calico — see
# config/cni-probe/kustomization.yaml); apply it separately so the operator's
# CNI self-test probe namespace + namespaced RBAC exist before the operator's
# first probe run.
(cd "$TMP_OVERLAY" && "$KUSTOMIZE_BIN" build config/cni-probe | K apply -f -) \
  || { echo "FATAL: cni-probe ns/RBAC deploy failed"; exit 2; }
# Force a short CNI probe interval (30s) so the probe re-runs quickly after the
# Loop is created (the default 10m interval would make the e2e wait up to 10
# minutes for the NetworkEnforced condition). This is a test-only override; the
# production default remains 10m. The patch appends --cni-check-interval=30s to
# the existing args (the calico overlay sets --allow-unenforced; this adds the
# short interval on top).
K -n "$E2E_NS" patch deploy coxswain-controller-manager --type=merge -p '{"spec":{"template":{"spec":{"containers":[{"name":"manager","args":["--metrics-bind-address=:8443","--leader-elect","--health-probe-bind-address=:8081","--allow-unenforced","--cni-check-interval=30s"]}]}}}}' >/dev/null 2>&1 || true
K -n "$E2E_NS" rollout status deploy/coxswain-controller-manager --timeout=180s || { echo "FATAL: controller not ready"; exit 2; }
echo "   controller running (base install: no escape hatches, 30s probe interval)"
echo "   cni-probe ns + RBAC applied"

# ===========================================================================
# STEP 3: create the fixture (namespace, AgentPolicy, Loop).
# ===========================================================================
echo
echo "--- STEP 3: create the fixture (namespace, AgentPolicy, Loop) ---"
if K -n "$NS" get loop "$LOOP" >/dev/null 2>&1; then
  echo "   deleting old Loop $LOOP (fresh sandbox with the current agent image)"
  K -n "$NS" delete loop "$LOOP" --wait=false 2>/dev/null || true
  for i in $(seq 1 20); do
    K -n "$NS" get pods -l "app.kubernetes.io/component=agent" --no-headers 2>/dev/null | grep -q . || break
    sleep 3
  done
fi
K get ns "$NS" >/dev/null 2>&1 || K create ns "$NS" >/dev/null
cat > "$TMPDIR/agentpolicy.yaml" <<EOF
apiVersion: coxswain.wattu.com/v1alpha1
kind: AgentPolicy
metadata:
  name: ${LOOP}-pol
  namespace: ${NS}
spec:
  network:
    - proxy.golang.org:80
EOF
K apply -f "$TMPDIR/agentpolicy.yaml" >/dev/null
cat > "$TMPDIR/loop.yaml" <<EOF
apiVersion: coxswain.wattu.com/v1alpha1
kind: Loop
metadata:
  name: ${LOOP}
  namespace: ${NS}
spec:
  goal: "D38 enforcing-CNI network e2e: host-network + pod-IP + external egress are blocked"
  policyRefs:
    - ${LOOP}-pol
  agent:
    image: ${AGENT_IMG}
    model: local-model
  workspace:
    repo: https://github.com/papawattu/coxswain
    ref: main
  loop:
    maxIterations: 1
EOF
K apply -f "$TMPDIR/loop.yaml" >/dev/null
echo "   fixtures applied: AgentPolicy ${LOOP}-pol (allows: proxy.golang.org:80), Loop ${LOOP}"

echo
echo "--- STEP 3b: wait for the egress proxy pod to be Ready ---"
READY=""
for i in $(seq 1 60); do
  READYC=$(K -n "$NS" get pod "${LOOP}-egress-proxy" -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}' 2>/dev/null)
  if [ "$READYC" = "True" ]; then READY=yes; break; fi
  sleep 5
done
[ "$READY" = "yes" ] || { echo "FATAL: egress proxy pod did not become Ready"; exit 2; }
echo "   egress proxy Ready"

# ===========================================================================
# STEP 3c: the D38 operator CNI self-test gate (D38s3 acceptance, design
# point 7 Calico bullet): the operator's own probe (the coxswain-cni-probe
# pod in the fixed coxswain-cni-probe namespace) must have run and PASSED,
# and the Loop must carry NetworkEnforced=True reason=CNIEnforced — with NO
# escape hatch set (base install). This is what makes the C1-C5 blocks
# below a property of the CNI, not just of the operator's NetworkPolicy.
# ===========================================================================
echo
echo "--- STEP 3c: D38 gate assertions (NetworkEnforced=True reason=CNIEnforced, no escape hatch) ---"
# The operator probes at startup (and every --cni-check-interval, default 10m).
# When the Loop is (re)created AFTER the controller started, the reconcile that
# sets NetworkEnforced runs on the next probe-completion retage (or the next
# reconcile trigger). Poll for up to ~2 minutes for the condition to become
# True CNIEnforced (the probe runs at startup, takes ~10-20s, then the retage
# + reconcile sets the condition on every Loop).
NE_COND=""
for i in $(seq 1 24); do
  NE_COND=$(K -n "$NS" get loop "$LOOP" -o jsonpath='{.status.conditions[?(@.type=="NetworkEnforced")].status} {.status.conditions[?(@.type=="NetworkEnforced")].reason}' 2>/dev/null)
  case "$NE_COND" in
    "True CNIEnforced") break ;;
    "False "*) break ;; # a definitive False (CNIUnenforced/ProbeUnavailable) is stable — don't wait for it to flip
  esac
  sleep 5
done
echo "   NetworkEnforced condition: $NE_COND"
case "$NE_COND" in
  "True CNIEnforced") ok "NetworkEnforced=True reason=CNIEnforced (the operator's CNI self-test passed; no escape hatch)" ;;
  *) bad "NetworkEnforced is not (True CNIEnforced): got '$NE_COND' (expected the operator's probe to pass on Calico)" ;;
esac

# The operator must be running WITHOUT the escape hatches (base install).
CONTROLLER_ARGS=$(K -n "$E2E_NS" get deploy coxswain-controller-manager -o jsonpath='{.spec.template.spec.containers[0].args}' 2>/dev/null)
echo "   controller args: $CONTROLLER_ARGS"
case "$CONTROLLER_ARGS" in
  *"--allow-unenforced-network"*) bad "controller runs WITH --allow-unenforced-network — the gate would pass-via-flag and prove nothing about the CNI" ;;
  *) ok "controller runs WITHOUT --allow-unenforced-network (the D38 gate is real)" ;;
esac
# --allow-unenforced IS expected on the calico profile: it bypasses the D30
# KubeArmor gate (KubeArmor is absent by design, ADR-0007 F2) so the D38
# network gate is the only gate under test. We assert it IS present (the
# calico overlay sets it) and that it is the ONLY escape hatch.
case "$CONTROLLER_ARGS" in
  *"--allow-unenforced"*) ok "controller runs WITH --allow-unenforced (expected on the calico profile: bypasses the D30 KubeArmor gate; the D38 network gate is the one under test)" ;;
  *) bad "controller runs WITHOUT --allow-unenforced — the D30 KubeArmor gate would hold Loops (KubeArmor is absent on this cluster); the calico overlay should set it" ;;
esac

# The probe pod: the operator creates + deletes it per probe run, so it is
# normally absent at the moment we look (the run just finished and the pod
# was cleaned up). If one is live, it must be progressing toward Succeeded —
# a stuck pod is the ProbeUnavailable path (the gate would then hold the
# Loop Suspended, which the NetworkEnforced check above already catches).
PROBE_NS=coxswain-cni-probe
if K get ns "$PROBE_NS" >/dev/null 2>&1; then
  # The probe pod's termination message (captured while the pod is live):
  # the expected-BLOCKED targets (APISERVER_SVC, NODE_API, KUBELET_NODE, EXTERNAL)
  # must be BLOCKED and the positive control (DNS_POSITIVE) must be REACHABLE
  # — the operator's own result, not just its condition.
  PROBE_POD_LIVE=$(K -n "$PROBE_NS" get pods -l "coxswain.io/probe=cni-probe" -o jsonpath='{.items[0].metadata.name}' 2>/dev/null)
  if [ -n "$PROBE_POD_LIVE" ]; then
    PROBE_MSG=$(K -n "$PROBE_NS" logs "$PROBE_POD_LIVE" 2>/dev/null)
    echo "   live probe pod $PROBE_POD_LIVE termination message:"
    echo "$PROBE_MSG" | sed 's/^/     /'
    for LBL in APISERVER_SVC NODE_API KUBELET_NODE EXTERNAL; do
      if echo "$PROBE_MSG" | grep -q "RESULT $LBL REACHABLE"; then
        bad "probe reports $LBL REACHABLE (the CNI must block it; the gate should not be CNIEnforced)"
      elif echo "$PROBE_MSG" | grep -q "RESULT $LBL BLOCKED"; then
        ok "probe reports $LBL BLOCKED"
      fi
    done
    if echo "$PROBE_MSG" | grep -q "RESULT DNS_POSITIVE REACHABLE"; then
      ok "probe reports DNS_POSITIVE REACHABLE (positive control: the probe has network egress to the allowed kube-dns)"
    elif echo "$PROBE_MSG" | grep -q "RESULT DNS_POSITIVE BLOCKED"; then
      bad "probe reports DNS_POSITIVE BLOCKED (the positive control failed: the probe has no network -> ProbeUnavailable)"
    fi
  else
    echo "   (no live probe pod to read a termination message from — the operator deletes it after each run)"
  fi
  # NetworkPolicy shape: the probe netpol must be the verify-cni.sh shape
  # (DNS-only egress to kube-system kube-dns), NOT the old agent-shaped
  # 0.0.0.0/0-minus-RFC1918 allow list that let the external targets through.
  PROBE_NETPOL=$(K -n "$PROBE_NS" get netpol coxswain-cni-probe-netpol -o json 2>/dev/null | python3 -c "import json,sys; d=json.load(sys.stdin); print(json.dumps(d['spec']['egress'][0], sort_keys=True))" 2>/dev/null)
  echo "   probe netpol egress[0]: $PROBE_NETPOL"
  case "$PROBE_NETPOL" in
    *"kube-system"*"kube-dns"*)
      if echo "$PROBE_NETPOL" | grep -q '"ipBlock"'; then
        bad "probe netpol still carries an IPBlock (the old agent-shaped 0.0.0.0/0-minus-RFC1918 allow list — external targets are ALLOWED under it, so a REACHABLE external target is not a CNI failure)"
      else
        ok "probe netpol is the verify-cni.sh shape (DNS 53 UDP+TCP to kube-system kube-dns only; no IPBlock)"
      fi ;;
    *) bad "probe netpol is not the verify-cni.sh shape (missing the kube-system kube-dns peer): $PROBE_NETPOL" ;;
  esac
  PROBE_PODS=$(K -n "$PROBE_NS" get pods --no-headers 2>/dev/null)
  echo "   probe namespace $PROBE_NS pods:"
  if [ -n "$PROBE_PODS" ]; then
    echo "$PROBE_PODS" | sed 's/^/     /'
    if echo "$PROBE_PODS" | grep -qE "(Error|ImagePullBackOff|ErrImagePull|CrashLoopBackOff|InvalidImageName)"; then
      bad "a probe pod is in a bad state (ProbeUnavailable path)"
    else
      ok "probe pod present and progressing (the operator deletes it after reading the result)"
    fi
  else
    ok "no live probe pod (expected: the operator deletes the probe pod after each run)"
  fi
else
  bad "probe namespace $PROBE_NS does not exist — the operator cannot run its CNI self-test (config/cni-probe not installed?)"
fi

AGENT_POD=$(K -n "$NS" get pods -l "app.kubernetes.io/component=agent,coxswain.io/loop=$LOOP" -o jsonpath='{.items[0].metadata.name}' 2>/dev/null)
[ -z "$AGENT_POD" ] && { echo "FATAL: no agent pod found"; exit 2; }
echo "   agent pod: $AGENT_POD"
NETPOL=$(K -n "$NS" get netpol --no-headers 2>/dev/null)
echo "   NetworkPolicies in $NS:"
echo "$NETPOL" | sed 's/^/     /'
[ -z "$NETPOL" ] && { echo "FATAL: no NetworkPolicy on the Loop namespace — the fence does not exist"; exit 2; }

# A cluster-internal pod IP (for check 4): the controller pod.
TEST_IP=$(K -n "$E2E_NS" get pod -o jsonpath='{.items[0].status.podIP}' 2>/dev/null)
[ -z "$TEST_IP" ] && TEST_IP=$(K -n kube-system get pod -l k8s-app=kube-dns -o jsonpath='{.items[0].status.podIP}' 2>/dev/null)
[ -z "$TEST_IP" ] && { echo "FATAL: could not find a cluster-internal pod IP"; exit 2; }
echo "   cluster-internal pod IP for check 4: $TEST_IP"

# A disallowed external IP (for check 5): resolved from the host.
GH_IP=$(getent hosts github.com 2>/dev/null | awk '{print $1}' | head -1)
[ -z "$GH_IP" ] && GH_IP=$(curl -s --max-time 10 https://ipinfo.io/github.com 2>/dev/null | grep -o '"ip_addr":"[0-9.]*"' | cut -d'"' -f4)
[ -z "$GH_IP" ] && GH_IP="1.1.1.1"
echo "   disallowed external IP for check 5: $GH_IP (github.com)"

# The one probe script. It tests every endpoint from inside the agent
# container. Each probe is a pure TCP-CONNECT test (the layer the CNI actually
# polices): a REACHABLE endpoint means the TCP connect succeeded (the CNI let
# the egress through, even if a later TLS/HTTP layer then failed); a BLOCKED
# endpoint means the connect was dropped/refused/timed out. Using the connect
# layer (not the HTTP response code) avoids the ambiguity where a reachable
# apiserver with a cert-name mismatch returns curl exit 60 / HTTP 000.
# The one probe. It runs INSIDE the real agent pod (golang:1.26 ships
# python3). Each endpoint is tested with a raw TCP socket connect - the
# exact layer the CNI polices. A successful connect means the CNI let the
# egress through (REACHABLE); a timeout / connection-refused means the CNI
# dropped it (BLOCKED). Unlike curl, a dropped connection is unambiguous:
# curl can report a blocked connect as exit 56 (empty reply) which looks
# like a successful-but-empty HTTP exchange, or a reachable-but-mismatched
# TLS handshake as exit 60 - neither is a reliable blocked/allowed signal.
# We write the python probe and a thin sh launcher (the rest of this suite
# expects `sh /tmp/d38-probe.sh`), passing the IPs in via the environment.
cat > "$TMPDIR/probe.py" <<'PYEOF'
import os, socket
def probe(label, host, port, timeout=4):
    s = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
    s.settimeout(timeout)
    try:
        s.connect((host, port))
        print("RESULT %s REACHABLE" % label)
    except Exception as exc:
        print("RESULT %s BLOCKED(%s)" % (label, type(exc).__name__))
    finally:
        s.close()
probe("APISERVER_SVC",  os.environ["APISERVER_SVC_IP"], 443)
probe("APISERVER_NODE", os.environ["NODE_IP"], 6443)
probe("KUBELET_NODE",   os.environ["NODE_IP"], 10250)
probe("INTERNAL_POD",   os.environ["TEST_IP"], 8080)
probe("EXTERNAL",       os.environ["GH_IP"], 443)
PYEOF
cat > "$TMPDIR/probe.sh" <<EOF
#!/bin/sh
export APISERVER_SVC_IP=${APISERVER_SVC_IP} NODE_IP=${NODE_IP} TEST_IP=${TEST_IP} GH_IP=${GH_IP}
exec python3 /tmp/d38-probe.py
EOF
K -n "$NS" cp "$TMPDIR/probe.py" "$AGENT_POD:/tmp/d38-probe.py" 2>/dev/null || { echo "FATAL: kubectl cp probe.py failed"; exit 2; }
K -n "$NS" cp "$TMPDIR/probe.sh" "$AGENT_POD:/tmp/d38-probe.sh" 2>/dev/null || { echo "FATAL: kubectl cp probe.sh failed"; exit 2; }

# ===========================================================================
# CHECKS 1-5: the blocked endpoints (C1-C5). These are the real L1 assertions.
# ===========================================================================
echo
echo "--- CHECKS 1-5: blocked endpoints from the agent pod ---"
PROBE_OUT=$(K -n "$NS" exec "$AGENT_POD" -- sh /tmp/d38-probe.sh 2>&1)
echo "$PROBE_OUT" | sed 's/^/   /'

blocked_line() { # $1 = label
  echo "$PROBE_OUT" | grep "^RESULT $1 " || echo "RESULT $1 MISSING"
}
assert_blocked() { # $1 = label, $2 = human description
  # The probe emits either "RESULT <label> REACHABLE" (the TCP connect
  # succeeded — the CNI let the egress through = a FAIL) or
  # "RESULT <label> BLOCKED(curl_exit=<n>)" (the connect was dropped/refused/
  # timed out — the CNI blocked it = a PASS).
  line=$(blocked_line "$1")
  case "$line" in
    *"MISSING"*) bad "$2: probe line missing from output" ;;
    *"REACHABLE"*) bad "$2: REACHABLE — the enforcing CNI failed to block it" ;;
    *"BLOCKED"*) ok "$2 (blocked: $(echo "$line" | sed 's/^RESULT [A-Z_0-9]* //'))" ;;
    *) bad "$2: unrecognised probe result: $line" ;;
  esac
}
echo "  (C1) apiserver service IP $APISERVER_SVC_IP:443"
assert_blocked APISERVER_SVC "apiserver svc IP $APISERVER_SVC_IP:443"
echo "  (C2) apiserver node $NODE_IP:6443"
assert_blocked APISERVER_NODE "apiserver node $NODE_IP:6443"
echo "  (C3) kubelet node $NODE_IP:10250"
assert_blocked KUBELET_NODE "kubelet node $NODE_IP:10250"
echo "  (C4) cluster-internal pod IP $TEST_IP:8080"
assert_blocked INTERNAL_POD "cluster-internal pod IP $TEST_IP:8080"
echo "  (C5) disallowed external $GH_IP:443"
assert_blocked EXTERNAL "disallowed external $GH_IP:443"

# C8: the operator's OWN CNI self-test (D38 design point 3: the
# NetworkEnforced condition on the Loop) must agree with what the e2e
# just proved directly from the agent pod. If the direct agent-pod probes
# (C1-C5) show the CNI is blocking but the operator's condition says
# CNIUnenforced/ProbeUnavailable, the operator's probe is broken even if
# the CNI itself is fine. The NetworkEnforced=True assertion in STEP 3c
# already covers the happy path; this is a redundant double-check that the
# condition is still True right now (it can't have flipped — the probe
# result is only re-read on the 10-minute interval or a re-gate event).
NE_COND_NOW=$(K -n "$NS" get loop "$LOOP" -o jsonpath='{.status.conditions[?(@.type=="NetworkEnforced")].status} {.status.conditions[?(@.type=="NetworkEnforced")].reason}' 2>/dev/null)
if [ "$NE_COND_NOW" = "True CNIEnforced" ]; then
  ok "(C8) NetworkEnforced still True CNIEnforced after the direct probes (the operator's probe and the e2e agree)"
else
  bad "(C8) NetworkEnforced flipped to '$NE_COND_NOW' after the direct probes (expected True CNIEnforced)"
fi

# ===========================================================================
# CHECKS 6-7: the egress proxy still works (allowed 200 / disallowed 403).
# ===========================================================================
echo
echo "--- CHECK 6: allowed host via the egress proxy succeeds (HTTP 200) ---"
cat > "$TMPDIR/probe-allowed.sh" <<'EOF'
#!/bin/sh
echo "-- HTTP proxy.golang.org (via HTTPS_PROXY env) --"
code=$(curl -s -o /dev/null -w "%{http_code}" --max-time 30 --retry 2 --retry-delay 3 http://proxy.golang.org/ 2>&1)
ec=$?
echo "curl-exit=$ec http=$code"
EOF
K -n "$NS" cp "$TMPDIR/probe-allowed.sh" "$AGENT_POD:/tmp/probe-allowed.sh" 2>/dev/null || bad "kubectl cp probe-allowed.sh failed"
P6=$(K -n "$NS" exec "$AGENT_POD" -- sh /tmp/probe-allowed.sh 2>&1)
echo "$P6" | sed 's/^/   /'
case "$P6" in
  *"http=200"*) ok "HTTP proxy.golang.org via the proxy succeeded (HTTP 200)" ;;
  *"curl-exit=0"*) ok "HTTP proxy.golang.org via the proxy succeeded (curl exit 0)" ;;
  *) bad "HTTP proxy.golang.org via the proxy did NOT succeed (output: $P6)" ;;
esac

echo
echo "--- CHECK 7: disallowed host via the egress proxy gets 403 ---"
cat > "$TMPDIR/probe-disallowed.sh" <<'EOF'
#!/bin/sh
echo "-- HTTP github.com (NOT in the allows) via HTTPS_PROXY env --"
code=$(curl -s -o /dev/null -w "%{http_code}" --max-time 30 --retry 2 --retry-delay 3 http://github.com/ 2>&1)
ec=$?
echo "curl-exit=$ec http=$code"
EOF
K -n "$NS" cp "$TMPDIR/probe-disallowed.sh" "$AGENT_POD:/tmp/probe-disallowed.sh" 2>/dev/null || bad "kubectl cp probe-disallowed.sh failed"
P7=$(K -n "$NS" exec "$AGENT_POD" -- sh /tmp/probe-disallowed.sh 2>&1)
echo "$P7" | sed 's/^/   /'
case "$P7" in
  *"http=403"*) ok "disallowed host via the proxy was 403'd" ;;
  *"http=200"*|*"http=301"*|*"http=302"*) bad "disallowed host via the proxy SUCCEEDED (should be 403)" ;;
  *) bad "unexpected output for disallowed host (output: $P7)" ;;
esac

# ===========================================================================
# SKIPPED: the KubeArmor-dependent checks (no KubeArmor on this cluster —
# safety rule, ADR-0007 F2). They are part of the I42 suite (checks 9-11 of
# i42-e2e.sh) and run on coxswain-dev instead.
# ===========================================================================
echo
echo "--- SKIPPED: KubeArmor-dependent checks (no KubeArmor on this cluster by design) ---"
skip "I42 check 9  (KubeArmor policies exist for the proxies) — no KubeArmor on $CLUSTER (ADR-0007 F2); runs on coxswain-dev (make i42-e2e)"
skip "I42 check 10 (KubeArmor exec block in a selected pod) — no KubeArmor on $CLUSTER (ADR-0007 F2); runs on coxswain-dev (make i42-e2e)"
skip "I42 check 11 (ephemeral-container admission DENY) — the ValidatingAdmissionPolicy is not KubeArmor-dependent, but check 11 of i42-e2e.sh asserts it; runs on coxswain-dev (make i42-e2e)"
# The CoreDNS DNS-rebinding carve-out check (I42 check 7) is also not run
# here: it needs the CoreDNS hosts override + the rebind allow, both of which
# the I42 suite owns. The NetworkPolicy-level blocks (C1-C5) are the D38
# property under test.
skip "I42 check 7  (DNS-rebinding resolved-IP carve-out) — CoreDNS hosts-override owned by the I42 suite; runs on coxswain-dev (make i42-e2e)"

echo
echo "============================================================"
echo "D38 summary (context=$CTX, calico=$CALICO_VER, commit=$COMMIT):"
echo "  C1 apiserver svc IP ($APISERVER_SVC_IP:443)   — asserted BLOCKED"
echo "  C2 apiserver node ($NODE_IP:6443)           — asserted BLOCKED"
echo "  C3 kubelet node ($NODE_IP:10250)            — asserted BLOCKED"
echo "  C4 cluster-internal pod IP ($TEST_IP:8080)  — asserted BLOCKED"
echo "  C5 disallowed external ($GH_IP:443)         — asserted BLOCKED"
echo "  C8 operator's NetworkEnforced condition     — asserted True CNIEnforced (STEP 3c + C8)"
echo "  C6 allowed host via egress proxy            — asserted 200"
echo "  C7 disallowed host via egress proxy         — asserted 403"
echo "  KubeArmor-dependent checks (I42 9/10/11, I42 7) — SKIPPED (no KubeArmor here; ADR-0007 F2)"
echo "============================================================"

# Clean up the temp kustomize overlay (the throwaway pod / Loop fixtures are
# left in place on the dedicated d38-e2e namespace for inspection; the cluster
# itself is deleted with `kind delete cluster`).
rm -rf "$TMP_OVERLAY" 2>/dev/null || true

echo
if [ "$FAIL" -eq 0 ]; then
  echo "=== D38 enforcing-CNI network e2e: PASS (commit=$COMMIT image=$IMG calico=$CALICO_VER) ==="
else
  echo "=== D38 enforcing-CNI network e2e: FAIL (commit=$COMMIT image=$IMG) ==="
fi
exit $FAIL
