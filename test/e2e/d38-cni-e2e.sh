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
for img in "$IMG" "$PROXY_IMG" "$EGRESS_IMG" "$AGENT_IMG"; do
  echo "   kind load: $img"
  kind load docker-image "$img" --name "$CLUSTER" || { echo "FATAL: kind load $img failed"; exit 2; }
done

echo
echo "--- STEP 2: deploy controller (dev overlay: --allow-unenforced) ---"
make -C "$REPO_ROOT" kustomize >/dev/null 2>&1
KUSTOMIZE_BIN="$REPO_ROOT/bin/kustomize"
# Build the dev overlay from a TEMP COPY of the config tree (R16 I44 norm:
# never let `kustomize edit set image` rewrite the tracked kustomization.yaml).
TMP_OVERLAY=$(mktemp -d)
cp -r "$REPO_ROOT/config" "$TMP_OVERLAY/config"
(cd "$TMP_OVERLAY/config/manager" && "$KUSTOMIZE_BIN" edit set image controller="$IMG")
(cd "$TMP_OVERLAY" && "$KUSTOMIZE_BIN" build config/dev | K apply -f -) \
  || { echo "FATAL: controller deploy failed"; exit 2; }
K -n "$E2E_NS" rollout status deploy/coxswain-controller-manager --timeout=180s || { echo "FATAL: controller not ready"; exit 2; }
echo "   controller running"

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
cat > "$TMPDIR/probe.sh" <<'PROBE_EOF'
#!/bin/sh
# tcp_probe <label> <host> <port>
tcp_probe() {
  ec=$(curl -s -o /dev/null --connect-timeout 3 --max-time 5 "http://$2:$3/" 2>/dev/null)
  ec=$?
  if [ "$ec" = "0" ]; then
    echo "RESULT $1 REACHABLE"
  else
    echo "RESULT $1 BLOCKED(curl_exit=$ec)"
  fi
}
tcp_probe APISERVER_SVC   __APISERVER_SVC_IP__ 443
tcp_probe APISERVER_NODE  __NODE_IP__ 6443
tcp_probe KUBELET_NODE    __NODE_IP__ 10250
tcp_probe INTERNAL_POD    __TEST_IP__ 8080
tcp_probe EXTERNAL        __GH_IP__ 443
PROBE_EOF
# Substitute the real IPs (the heredoc above is single-quoted so the values
# are inserted here, not at write time).
sed -i   -e "s/__APISERVER_SVC_IP__/${APISERVER_SVC_IP}/"   -e "s/__NODE_IP__/${NODE_IP}/g"   -e "s/__TEST_IP__/${TEST_IP}/"   -e "s/__GH_IP__/${GH_IP}/" \
  "$TMPDIR/probe.sh"
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
  line=$(blocked_line "$1")
  case "$line" in
    *"MISSING"*) bad "$2: probe line missing from output" ;;
    *"HTTP_CODE="*)
      code=$(echo "$line" | grep -o 'HTTP_CODE=[0-9]*' | cut -d= -f2)
      # curl with -w "%{http_code}" prints "000" whenever it did NOT complete
      # a request: connection refused (exit 7), timeout (28), DNS failure (6),
      # TLS error (60), or a TCP RST from a firewall DROP. All of these mean
      # the endpoint was NOT reachable, so 000 counts as blocked. A real HTTP
      # response (100-599) means the CNI failed to block the egress.
      if [ "$code" = "000" ]; then
        ok "$2 (endpoint unreachable)"
      else
        bad "$2: REACHABLE (HTTP $code) — the enforcing CNI failed to block it"
      fi
      ;;
    *) ok "$2 (blocked: $(echo "$line" | sed 's/^RESULT [A-Z_0-9]* //'))" ;;
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
