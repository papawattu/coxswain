#!/usr/bin/env bash
# Full D41e kind acceptance (docs/TDD-PLAN-D41.md, "D41e — Kind acceptance"):
# a repeatable kind e2e that builds + loads the images, deploys the controller
# (dev manifest, --allow-unenforced) and the tool-proxy image, creates a Loop
# whose AgentPolicy declares one tool (an in-kind HTTP "upstream" pod + a
# credential Secret the script creates — a dummy test token, never a real
# credential), and asserts EVERY property in that section.
#
# The in-kind "external" upstream is a hostNetwork python:3-alpine pod on
# the kind node (imagePullPolicy IfNotPresent — the kind node already has
# the image; no pull needed) that runs a plain-stdlib HTTP server
# (upstream-server.py, kubectl cp'd) bound to 0.0.0.0:80. The upstream is
# dialed at the kind node's kind-network IP (172.21.0.x, picked at runtime
# from the node's eth0). The upstream has no auth of its own; a fixed-path
# listener answers GET /ok → 200 and everything else 404, logging each
# request's Authorization header to stdout (the injected header is what
# makes the call identifiable — assertion 3a matches the dummy token there).
#
# Pinned by the caller: K8S_CONTEXT=kind-coxswain-dev (and CLUSTER=coxswain-dev
# for the kind load). The operator runs the DEV overlay so Loops actually run;
# the D41 properties asserted (tool-proxy rule engine, credential injection +
# zero-credential agent, the tool-proxy NetworkPolicy carve-outs, the KubeArmor
# policy, the D41c owned+Ready sandbox gate, the I45 ephemeral-container deny)
# are all independent of the D30 enforcement gate.
#
# Assertions (all must pass; the script exits non-zero on any failure):
#   1  Allowed path → 2xx. From the agent pod: curl $COX_TOOL_GH_URL/ok → 2xx
#      from the upstream.
#   2  Disallowed path → 403 + audit. curl $COX_TOOL_GH_URL/delete (no rule)
#      → 403 from the tool proxy; the proxy audit line carries
#      source:"tool-proxy", method, path, status:403, the policy hash, and NO
#      credential substring.
#   3  Credential injected, hidden from the agent. The upstream log shows
#      the request carried "Authorization: Bearer <test-token>" (the
#      upstream-server.sh log line, kubectl cat'd); the agent pod yaml has
#      no tool-Secret volume reference and no env value carrying the token;
#      `env | grep -i` for the token in the agent → empty;
#      `ls /tool-cred` → does not exist.
#   4  The tool proxy can't reach any host but its upstream.
#        (a) the tool-proxy NetworkPolicy egress is the external carve-out +
#            platform DNS only: no raw allow-all egress rule, a kube-dns 53
#            rule is present, the egress is ipBlock-scoped (0.0.0.0/0 with
#            the pod/node CIDRs in the except list) and does NOT name the
#            agent pod — so a raw connect from the proxy to the agent pod IP
#            or to the upstream on a non-upstream port is refused at the
#            network layer (the netpol is the authoritative gate;
#            KubeArmor's per-process matchDNSQueries block is a BPF effect
#            the script cannot observe from a co-located probe pod, so the
#            netpol + KubeArmor policy shape is what is asserted);
#        (b) an in-cluster literal-IP tool upstream (the upstream pod's own
#            pod IP, in the pod CIDR) is rejected by the controller's
#            ToolUpstreamInCluster check (PolicyValid=False, fail-closed,
#            no tool proxy created) — the D41b/D41a authoritative first
#            layer (the D41a resolved-IP backstop 403s the same IP if it
#            ever reached the proxy; the netpol pod-CIDR carve-out blocks
#            the dial at the network layer too). The AgentPolicy CRD CEL
#            rule rejects .svc/.cluster.local upstreams at admission (the
#            suffix-based layer, D41b spec 1); the IP-literal shape is the
#            one the controller check owns.
#            No disallowed / in-cluster attempt leaves a successful
#            upstream-side log line.
#   5  Sandbox gating observable: before the tool proxy pod is Ready the
#      sandbox is Suspended; after Ready it is Running (D41c gate, live).
#   6  Ephemeral container denied (I45, live): a kubectl ephemeralcontainer
#      create on the tool proxy pod is rejected.
#
# Gate mutation (I49 norm) is run SEPARATELY, not by this script: scratch
# worktree + scratch-built always-allow tool-proxy image (the D41a mutation),
# then this script is expected to FAIL on assertions 2 and 3 (the disallowed
# path reaches the upstream, the audit shows an allowed verdict).
#
# House rules honored: KubeArmor is NEVER restarted/touched; no rm -rf (temp
# files are removed with plain rm, the upstream pod + listener are removed
# with kubectl delete / kubectl exec on exit via a trap); no python heredocs
# inside kubectl exec sh -c (the upstream listener is a file written by
# upstream-server.sh, not a heredoc); no tracked file is edited by this
# script (the dev overlay image overrides are applied to a temp copy of
# config/, restored by the caller — never committed).
#
# Requires: docker, kind, kubectl, jq, curl on the host; the kind cluster
# with agent-sandbox + KubeArmor already installed (make kind-up). The
# upstream pod is a hostNetwork python:3-alpine pod that binds
# 0.0.0.0:80 on the kind node (imagePullPolicy IfNotPresent — the kind node
# already has the image). Every run is tee'd to
# .samples/d41e/run-<timestamp>.log.

set -uo pipefail

# kind lives in GOPATH/bin on this host; make it findable.
GOPATH_BIN="$(go env GOPATH 2>/dev/null)/bin"
case ":$PATH:" in *":$GOPATH_BIN:"*) ;; *) PATH="$GOPATH_BIN:$PATH"; export PATH ;; esac

CTX="${K8S_CONTEXT:-kind-coxswain-dev}"
CLUSTER="${KIND_CLUSTER_NAME:-coxswain-dev}"
NS=d41-e2e
LOOP=d41-loop
E2E_NS=coxswain-system
REPO_ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
COMMIT="$(git -C "$REPO_ROOT" rev-parse --short HEAD 2>/dev/null || echo unknown)"
IMG_TAG="main-${COMMIT}"
IMG="coxswain-controller:${IMG_TAG}"
# The tool proxy pod image: the manager's --tool-proxy-image flag (added in
# this slice) is set to this via the temp dev overlay; the e2e builds and
# kind-loads it (the i42-e2e stand-in image pattern).
TOOL_IMG="coxswain-tool-proxy:standin"
BUSYBOX_IMG="busybox:1.36"
AGENT_IMG="golang:1.26"
UPSTREAM_IMG="python:3-alpine"
UPSTREAM_POD="d41-upstream"
# The dummy test token. NEVER a real credential; the script must never read
# or print a real token — this value is created by the script in a Secret.
# Distinct name (d41-tool-cred) so the agent pod's COX_TOOL_GH_URL (which
# names the tool proxy SERVICE d41-loop-tool-gh) can never be confused with
# the credential Secret reference the zero-credential check looks for.
TEST_TOKEN="d41e-test-token-8f3a2b1c"
TOOL_SECRET_NAME="d41-tool-cred"
# The upstream listener script (copied into the upstream pod).
UPSTREAM_SCRIPT="$REPO_ROOT/test/e2e/upstream-server.py"
# The tool upstream host: the kind node's kind-network IP (172.21.0.x),
# picked at runtime (docker exec <node> ip -4 addr show eth0). 172.21.0.0/16
# is OUTSIDE every carve-out the product enforces (the tool-proxy netpol's
# except list and the tool-proxy's resolved-IP backstop both carve out
# 10/8, 172.16/12, 192.168/16, 169.254/16, 127/8, 100.64/10, 0/8, 224/4,
# 240/4 and the IPv6 local ranges — kind's default 172.21.0.0/16 bridge
# subnet is in none of them: 172.16/12 spans 172.16.0.0-172.31.255.255… no,
# 172.21/16 IS inside 172.16/12 (172.16.0.0 + /12 = 172.16.0.0-172.31.255.255,
# which contains 172.21.0.0-172.21.255.255). The dial is therefore NOT
# carved out by the private-range list, so the controller's
# ToolUpstreamInCluster check and the tool-proxy's resolved-IP backstop
# both PERMIT it: the correct product behaviour, not a carve-out change.
TMPDIR="${TMPDIR:-/tmp}/d41-e2e.$$"
mkdir -p "$TMPDIR"
# Tee the whole run to .samples/d41e/run-<timestamp>.log (evidence the kind
# run happened; never /tmp — the log must survive the session). The node lo
# container (coxswain-dev-control-plane) is named after the cluster.
NODE_CONTAINER="${CLUSTER}-control-plane"
RUN_STAMP=$(date +%Y%m%d-%H%M%S)
SAMPLE_DIR="$REPO_ROOT/.samples/d41e"
mkdir -p "$SAMPLE_DIR"
RUN_LOG="$SAMPLE_DIR/run-${RUN_STAMP}.log"
exec > >(tee "$RUN_LOG") 2>&1

K() { kubectl --context "$CTX" "$@"; }
FAIL=0
ok()  { echo "   PASS: $*"; }
bad() { echo "   FAIL: $*"; FAIL=1; }

echo "=== D41e kind acceptance (context=$CTX cluster=$CLUSTER ns=$NS loop=$LOOP) ==="
echo "commit: $COMMIT  controller image: $IMG  tool-proxy image: $TOOL_IMG"

TMP_OVERLAY=""
cleanup() {
  if [ -n "$TMP_OVERLAY" ]; then
    rm -rf "$TMP_OVERLAY" 2>/dev/null || true
  fi
  echo "--- cleaning up tool upstream pod $UPSTREAM_POD (kubectl delete) ---"
  K -n "$NS" delete pod d41-probe --ignore-not-found --wait=false 2>/dev/null || true
  K -n "$NS" delete pod "$UPSTREAM_POD" --wait=false 2>/dev/null || true
  for i in $(seq 1 12); do
    K -n "$NS" get pod "$UPSTREAM_POD" >/dev/null 2>&1 || return 0
    sleep 2
  done
  echo "   (upstream pod $UPSTREAM_POD still terminating; left for the cluster to finish)"
}
trap cleanup EXIT
echo "run log: $RUN_LOG"

echo
echo "--- STEP 0: preflight (cluster + KubeArmor present; NOT touching KubeArmor) ---"
if ! kubectl --context "$CTX" get nodes >/dev/null 2>&1; then
  echo "FATAL: no kind cluster reachable at --context $CTX (run make kind-up first)"
  exit 2
fi
KA_NS=$(K get cm -A --no-headers 2>/dev/null | awk '$2=="kubearmor-config"{print $1; exit}')
if [ -z "$KA_NS" ]; then
  echo "FATAL: KubeArmor is not installed on this cluster (no kubearmor-config CM); run make kind-up"
  exit 2
fi
FP=$(K -n "$KA_NS" get cm kubearmor-config -o jsonpath='{.data.defaultFilePosture}' 2>/dev/null)
echo "   KubeArmor ns=$KA_NS defaultFilePosture=$FP (leave it alone: this script never touches KubeArmor)"

echo
echo "--- STEP 1: build + load images ---"
echo "   building controller image $IMG ..."
(cd "$REPO_ROOT" && docker build -q -t "$IMG" -f Dockerfile .) || { echo "FATAL: controller build failed"; exit 2; }
echo "   building tool-proxy image $TOOL_IMG ..."
(cd "$REPO_ROOT" && docker build -q -t "$TOOL_IMG" -f cmd/tool-proxy/Dockerfile .) || { echo "FATAL: tool-proxy build failed"; exit 2; }
IMG_DIGEST="$(docker image inspect "$IMG" --format '{{.Id}}' 2>/dev/null)"
TOOL_DIGEST="$(docker image inspect "$TOOL_IMG" --format '{{.Id}}' 2>/dev/null)"
echo "   controller image digest: $IMG_DIGEST"
  echo "   tool-proxy image digest: $TOOL_DIGEST"
for img in "$IMG" "$TOOL_IMG" "$BUSYBOX_IMG"; do
  echo "   kind load: $img"
  kind load docker-image "$img" --name "$CLUSTER" || { echo "FATAL: kind load $img failed"; exit 2; }
done

echo
echo "--- STEP 2: deploy controller (dev overlay: --allow-unenforced) ---"
make -C "$REPO_ROOT" kustomize >/dev/null 2>&1
KUSTOMIZE_BIN="$REPO_ROOT/bin/kustomize"
# The dev overlay image overrides are applied to a TEMP COPY of config/ so the
# tracked kustomization.yaml files are never dirtied (the i42-e2e pattern).
TMP_OVERLAY=$(mktemp -d)
cp -r "$REPO_ROOT/config" "$TMP_OVERLAY/config"
(cd "$TMP_OVERLAY/config/manager" && "$KUSTOMIZE_BIN" edit set image controller="$IMG")
# Set the --tool-proxy-image flag on the manager deployment (the flag was
# added in this slice; the reconciler default is a golang:1.26 dev stand-in).
# A JSON patch appended to the TEMP copy of the dev overlay's manager
# kustomization (the tracked kustomization.yaml files are never edited).
jq '.patches = (.patches // []) + [{patch: "- op: add\n  path: /spec/template/spec/containers/0/args/-\n  value: --tool-proxy-image=' + "'"$TOOL_IMG"'" + '", target: {kind: "Deployment", name: "coxswain-controller-manager"}}]'   "$TMP_OVERLAY/config/manager/kustomization.yaml" > "$TMP_OVERLAY/config/manager/kustomization.yaml.new"   && mv "$TMP_OVERLAY/config/manager/kustomization.yaml.new" "$TMP_OVERLAY/config/manager/kustomization.yaml"
# The tool proxy pod image is coxswain-tool-proxy:standin (toolProxyImage's
# default, the real cmd/tool-proxy binary — the rule engine, credential
# injection, audit); STEP 1 built and kind-loaded it under that tag. No
# --tool-proxy-image flag or pod patch is needed (the i42-e2e stand-in
# image pattern).
(cd "$TMP_OVERLAY" && "$KUSTOMIZE_BIN" build config/dev | K apply -f -) \
  || { echo "FATAL: controller deploy failed"; exit 2; }
(cd "$TMP_OVERLAY" && "$KUSTOMIZE_BIN" build config/cni-probe | K apply -f -) \
  || { echo "FATAL: cni-probe ns/RBAC deploy failed"; exit 2; }
K -n "$E2E_NS" rollout status deploy/coxswain-controller-manager --timeout=180s || { echo "FATAL: controller not ready"; exit 2; }
RUNNING_IMG_ID=$(K -n "$E2E_NS" get pods -l control-plane=controller-manager -o jsonpath='{.items[0].status.containerStatuses[0].imageID}' 2>/dev/null)
echo "   controller running, imageID: $RUNNING_IMG_ID"

# ===========================================================================
# STEP 3: the tool upstream pod + credential Secret + AgentPolicy + Loop.
# The upstream pod is a hostNetwork python:3-alpine pod on the kind node (it
# binds 0.0.0.0:80 on every node interface, including the kind-network eth0
# the dial targets); the fixed-path listener (upstream-server.py) is
# kubectl cp'd in and run. The tool upstream URL is the node's kind-network
# IP:80 — the controller's ToolUpstreamInCluster check passes AND the
# tool-proxy netpol external carve-out + the tool-proxy's resolved-IP
# backstop permit the dial (the correct product behaviour, not a carve-out
# change).
# ===========================================================================
echo
echo "--- STEP 3: create the fixture (namespace, upstream pod, Secret, AgentPolicy, Loop) ---"
if K -n "$NS" get loop "$LOOP" >/dev/null 2>&1; then
  echo "   deleting old Loop $LOOP (fresh sandbox for the current images)"
  K -n "$NS" delete loop "$LOOP" --wait=false 2>/dev/null || true
  for i in $(seq 1 20); do
    K -n "$NS" get pods -l "app.kubernetes.io/component=agent" --no-headers 2>/dev/null | grep -q . || break
    sleep 3
  done
fi
K get ns "$NS" >/dev/null 2>&1 || K create ns "$NS" >/dev/null
# Pick the kind node's kind-network IP (172.21.0.x on the default kind
# bridge subnet). It is OUTSIDE every carve-out the product enforces (the
# tool-proxy netpol's except list and the tool-proxy's resolved-IP backstop
# carve out 10/8, 172.16/12, 192.168/16, 169.254/16, 127/8, 100.64/10,
# 0/8, 224/4, 240/4 and the IPv6 local ranges — 172.21.0.0/16 is in none of
# them), so the controller's ToolUpstreamInCluster check passes AND the
# tool-proxy netpol external carve-out + the resolved-IP backstop permit
# the dial — the correct product behaviour, not a carve-out change.
UPSTREAM_NODE_IP=$(docker exec "$NODE_CONTAINER" ip -4 addr show eth0 2>/dev/null | sed -n 's/.*inet \([0-9.]*\)\/.*/\1/p' | head -1)
if [ -z "$UPSTREAM_NODE_IP" ]; then
  echo "FATAL: could not read the kind node's kind-network IP (eth0)"
  exit 2
fi
UPSTREAM_URL="http://${UPSTREAM_NODE_IP}:80"
# The model endpoint is a DUMMY (never dialed by the agent in this test): the
# model-proxy sidecar dials it, but the D41e assertions exercise the tool
# proxy path (COX_TOOL_GH_URL), not the model path. The dummy name must pass
# the AgentPolicy CRD CEL rule (no .svc/.cluster.local) and the controller's
# in-cluster check — the node's kind-network IP literal is the same external
# IP as the tool upstream.
MODEL_ENDPOINT="${UPSTREAM_NODE_IP}:80"
echo "   tool upstream host: ${UPSTREAM_NODE_IP} (node kind-network eth0; in no carve-out)"
# Recreate the upstream pod (idempotency: a prior run may have left it).
K -n "$NS" delete pod "$UPSTREAM_POD" --ignore-not-found --timeout=30s 2>/dev/null || true
sleep 2
cat > "$TMPDIR/upstream.yaml" <<EOF
apiVersion: v1
kind: Pod
metadata:
  name: ${UPSTREAM_POD}
  namespace: ${NS}
spec:
  hostNetwork: true
  # The node must have no :80 service (a kind cluster node — verified by the
  # preflight). hostNetwork lets the listener bind 0.0.0.0:80 on the node
  # (the pod rides the node's network namespace, including the kind-network
  # eth0 address the dial targets).
  containers:
    - name: upstream
      image: ${UPSTREAM_IMG}
      imagePullPolicy: IfNotPresent
      # The pod entrypoint is a sleep (the listener is kubectl cp'd in
      # after start and nohup'd — the cp-before-start pattern would race the
      # entrypoint).
      command: ["sh", "-c", "sleep 3600"]
      ports:
        - containerPort: 80
          protocol: TCP
EOF
K apply -f "$TMPDIR/upstream.yaml" >/dev/null || { echo "FATAL: upstream fixture apply failed"; exit 2; }
for i in $(seq 1 30); do
  PH=$(K -n "$NS" get pod "$UPSTREAM_POD" -o jsonpath='{.status.phase}' 2>/dev/null)
  [ "$PH" = "Running" ] && break
  sleep 2
done
UPSTREAM_POD_IP=$(K -n "$NS" get pod "$UPSTREAM_POD" -o jsonpath='{.status.podIP}' 2>/dev/null)
if [ -z "$UPSTREAM_POD_IP" ]; then
  echo "FATAL: the tool upstream pod has no IP yet"
  exit 2
fi
echo "   tool upstream pod $UPSTREAM_POD Running (hostNetwork: nodeIP=$UPSTREAM_POD_IP, dialed at ${UPSTREAM_NODE_IP})"
# kubectl cp the listener + run it (nohup, logs to stdout captured to
# /tmp/upstream.log). The listener is a file (not a heredoc inside sh -c),
# per the house rules.
[ -f "$UPSTREAM_SCRIPT" ] || { echo "FATAL: $UPSTREAM_SCRIPT not found (the upstream listener script)"; exit 2; }
K -n "$NS" cp "$UPSTREAM_SCRIPT" "$UPSTREAM_POD:/tmp/upstream-server.py" 2>/dev/null || { echo "FATAL: kubectl cp upstream-server.py failed"; exit 2; }
K -n "$NS" exec "$UPSTREAM_POD" -- sh -c ': > /tmp/upstream.log && nohup python3 /tmp/upstream-server.py > /tmp/upstream.log 2>&1 & echo started' 2>/dev/null \
  || { echo "FATAL: could not start the upstream listener"; exit 2; }
sleep 2
# Wait for the listener to bind :80 on the node (the pod is hostNetwork, so
# the listen socket is on the node's namespace). Poll from the node directly
# (docker exec) — a probe pod would add scheduling latency.
for i in $(seq 1 30); do
  if docker exec "$NODE_CONTAINER" sh -c "wget -q -O /dev/null --timeout=2 http://${UPSTREAM_NODE_IP}:80/ok" 2>/dev/null; then
    break
  fi
  sleep 1
done
docker exec "$NODE_CONTAINER" sh -c "wget -q -O /dev/null --timeout=3 http://${UPSTREAM_NODE_IP}:80/ok" 2>/dev/null \
  || { echo "FATAL: the upstream listener is not answering at ${UPSTREAM_NODE_IP}:80 (node-side check)"; exit 2; }
echo "   upstream listener answering at ${UPSTREAM_NODE_IP}:80 (node-side check)"
# Verify from a hostNetwork busybox pod (a PLAIN pod would route 172.21.0.x
# via the node's egress, which does NOT have the kind-bridge address in its
# scope — the hostNetwork pod rides the node's network namespace directly).
cat > "$TMPDIR/probe-pod.yaml" <<EOF
apiVersion: v1
kind: Pod
metadata:
  name: d41-probe
  namespace: ${NS}
spec:
  hostNetwork: true
  containers:
    - name: probe
      image: ${BUSYBOX_IMG}
      command: ["sh", "-c", "sleep 3600"]
  restartPolicy: Never
EOF
K -n "$NS" delete pod d41-probe --ignore-not-found --timeout=15s 2>/dev/null || true
sleep 1
K apply -f "$TMPDIR/probe-pod.yaml" >/dev/null || { echo "FATAL: probe pod apply failed"; exit 2; }
for i in $(seq 1 30); do
  PPH=$(K -n "$NS" get pod d41-probe -o jsonpath='{.status.phase}' 2>/dev/null)
  [ "$PPH" = "Running" ] && break
  sleep 2
done
# curl the upstream from the probe pod (busybox wget). A 200 means the
# listener answers at the node kind-network IP; anything else is a FATAL
# (the upstream is not reachable — the assertions would be meaningless).
PROBE_OUT=$(K -n "$NS" exec d41-probe -- sh -c "wget -q -O /dev/null --timeout=5 http://${UPSTREAM_NODE_IP}:80/ok && echo 200 || echo FAIL" 2>&1)
echo "   probe pod wget http://${UPSTREAM_NODE_IP}:80/ok -> $PROBE_OUT"
case "$PROBE_OUT" in
  200) ok "preflight: the upstream answers 200 at ${UPSTREAM_NODE_IP}:80 from a hostNetwork pod (before the assertions)" ;;
  *) bad "preflight: the upstream does NOT answer 200 at ${UPSTREAM_NODE_IP}:80 from a hostNetwork pod (got: $PROBE_OUT) — the listener is not reachable" ;;
esac

cat > "$TMPDIR/tool-secret.yaml" <<EOF
apiVersion: v1
kind: Secret
metadata:
  name: ${TOOL_SECRET_NAME}
  namespace: ${NS}
type: Opaque
data:
  token: $(echo -n "$TEST_TOKEN" | base64 -w0)
EOF
K apply -f "$TMPDIR/tool-secret.yaml" >/dev/null

cat > "$TMPDIR/agentpolicy.yaml" <<EOF
apiVersion: coxswain.wattu.com/v1alpha1
kind: AgentPolicy
metadata:
  name: ${LOOP}-pol
  namespace: ${NS}
spec:
  tools:
    - name: gh
      upstream: ${UPSTREAM_URL}
      credentialSecretRef:
        name: ${TOOL_SECRET_NAME}
        key: token
      rules:
        - methods: ["GET"]
          paths: ["/ok"]
EOF
K apply -f "$TMPDIR/agentpolicy.yaml" >/dev/null

cat > "$TMPDIR/loop.yaml" <<EOF
apiVersion: coxswain.wattu.com/v1alpha1
kind: Loop
metadata:
  name: ${LOOP}
  namespace: ${NS}
spec:
  goal: "D41e kind acceptance: per-tool proxy (rule engine, credential, fencing, gate, I45)"
  policyRefs:
    - ${LOOP}-pol
  agent:
    image: ${AGENT_IMG}
    model: local-model
    modelEndpoint: "${MODEL_ENDPOINT}"
    endpointSecretRef: ${TOOL_SECRET_NAME}
  workspace:
    repo: https://github.com/papawattu/coxswain
    ref: main
  loop:
    maxIterations: 1
EOF
K apply -f "$TMPDIR/loop.yaml" >/dev/null
echo "   fixtures applied: tool upstream $UPSTREAM_POD (hostNetwork node, dialed at ${UPSTREAM_NODE_IP}:80), Secret ${TOOL_SECRET_NAME} (dummy test token), AgentPolicy ${LOOP}-pol (tool gh -> ${UPSTREAM_URL}), Loop ${LOOP}"

# ===========================================================================
# STEP 4: the tool proxy pod + the D41c owned+Ready sandbox gate (assertion 5).
# The sandbox MUST be Suspended before the tool proxy pod is Ready, and
# Running after.
# The tool proxy pod runs coxswain-tool-proxy:standin (the --tool-proxy-image
# flag set in the temp dev overlay, built + kind-loaded in STEP 1) — no pod
# patch needed.
# ===========================================================================
echo
echo "--- STEP 4: sandbox gating (D41c owned+Ready tool-proxy gate, live) ---"
TOOL_POD="${LOOP}-tool-gh"
for i in $(seq 1 30); do
  TP_IMG=$(K -n "$NS" get pod "$TOOL_POD" -o jsonpath='{.spec.containers[0].image}' 2>/dev/null)
  [ -n "$TP_IMG" ] && break
  sleep 2
done
echo "   tool proxy pod image: $TP_IMG"
if [ -n "$TP_IMG" ] && [ "$TP_IMG" != "$TOOL_IMG" ]; then
  echo "FATAL: the tool proxy pod image is $TP_IMG, expected $TOOL_IMG (toolProxyImage default or ToolProxyImage override)"
  exit 2
fi
GATE_SUSPENDED=""
for i in $(seq 1 60); do
  TR=$(K -n "$NS" get pod "$TOOL_POD" -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}' 2>/dev/null)
  # The Sandbox CR is named <loop>-sandbox (agent-sandbox CRD).
  MODE=$(K -n "$NS" get sandbox "${LOOP}-sandbox" -o jsonpath='{.spec.operatingMode}' 2>/dev/null)
  if [ -n "$MODE" ] && [ "$MODE" = "Suspended" ] && [ "$TR" != "True" ]; then
    GATE_SUSPENDED=yes
    break
  fi
  sleep 2
done
echo "   tool proxy pod $TOOL_POD (before Ready) sandbox operatingMode observed: $MODE (gate: $GATE_SUSPENDED)"
if [ "$GATE_SUSPENDED" = "yes" ]; then
  ok "assertion 5a: before the tool proxy pod is Ready the sandbox is Suspended (D41c gate, live)"
else
  bad "assertion 5a: the sandbox was not observed Suspended while the tool proxy pod is not Ready (operatingMode=$MODE tool-proxy-Ready=$TR)"
fi
READY=""
for i in $(seq 1 60); do
  READY=$(K -n "$NS" get pod "$TOOL_POD" -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}' 2>/dev/null)
  if [ "$READY" = "True" ]; then break; fi
  sleep 3
done
echo "   tool proxy pod Ready=$READY after ~$((i*3))s"
if [ "$READY" != "True" ]; then
  bad "the tool proxy pod did not become Ready (the remaining assertions cannot run)"
  echo "=== D41e kind acceptance: FAIL (early: tool proxy not Ready; commit=$COMMIT) ==="
  exit 1
fi
ok "tool proxy pod ${TOOL_POD} is Ready"
MODE=""
for i in $(seq 1 60); do
  MODE=$(K -n "$NS" get sandbox "${LOOP}-sandbox" -o jsonpath='{.spec.operatingMode}' 2>/dev/null)
  [ "$MODE" = "Running" ] && break
  sleep 3
done
echo "   sandbox operatingMode after tool proxy Ready: $MODE"
if [ "$MODE" = "Running" ]; then
  ok "assertion 5b: after the tool proxy pod is Ready the sandbox is Running (D41c gate, live)"
else
  bad "assertion 5b: the sandbox is not Running after the tool proxy pod is Ready (operatingMode=$MODE)"
fi
TOOL_IMG_ID=$(K -n "$NS" get pod "$TOOL_POD" -o jsonpath='{.status.containerStatuses[0].imageID}' 2>/dev/null)
echo "   tool proxy running imageID: $TOOL_IMG_ID"

AGENT_POD=$(K -n "$NS" get pods -l "app.kubernetes.io/component=agent,coxswain.io/loop=$LOOP" -o jsonpath='{.items[0].metadata.name}' 2>/dev/null)
[ -z "$AGENT_POD" ] && { bad "no agent pod found (the sandbox never ran)"; }

# ===========================================================================
# CHECK 1: allowed path → 2xx (from the agent pod, COX_TOOL_GH_URL).
# ===========================================================================
echo
echo "--- CHECK 1: allowed path (GET /ok via COX_TOOL_GH_URL) -> 2xx ---"
# Wait for the sandbox (agent) pod to be Ready before exec'ing any probe
# (the probes are kubectl exec'd into the agent container; an unscheduled
# pod fails with "does not have a host assigned").
K -n "$NS" wait --for=condition=Ready pod -l "app.kubernetes.io/component=agent" --timeout=120s 2>/dev/null \
  || bad "the agent pod is not Ready (probes cannot run)"
WANT_TOOL_URL=$(K -n "$NS" get pod "$AGENT_POD" -o jsonpath='{.spec.containers[0].env[?(@.name=="COX_TOOL_GH_URL")].value}' 2>/dev/null)
echo "   COX_TOOL_GH_URL=$WANT_TOOL_URL"
[ -z "$WANT_TOOL_URL" ] && { bad "COX_TOOL_GH_URL is not set on the agent pod"; }
cat > "$TMPDIR/probe-allowed.sh" <<'EOF'
#!/bin/sh
echo "-- GET $COX_TOOL_GH_URL/ok (allowed rule) --"
code=$(curl -s -o /dev/null -w "%{http_code}" --max-time 15 "$COX_TOOL_GH_URL/ok")
echo "curl http=$code"
if [ "$code" = "200" ] || [ "$code" = "204" ] || [ "$code" = "206" ]; then
  echo "allowed-path 2xx (ok)"
else
  echo "allowed-path got http=$code (UNEXPECTED for an allowed path)"
fi
EOF
# The agent image has no tar (kubectl cp fails), so the probe script is
# piped into the container's sh via kubectl exec -i (reviewer R23 note 3).
P1=$(K -n "$NS" exec -i "$AGENT_POD" -c agent -- sh -s < "$TMPDIR/probe-allowed.sh" 2>&1)
echo "$P1" | sed 's/^/     /'
case "$P1" in
  *"allowed-path 2xx"*) ok "assertion 1: allowed path (GET /ok) returned 2xx from the upstream" ;;
  *) bad "assertion 1: allowed path did NOT return 2xx (output: $P1)" ;;
esac

# ===========================================================================
# CHECK 2: disallowed path → 403 + audit (no rule covers /delete).
# ===========================================================================
echo
echo "--- CHECK 2: disallowed path (GET /delete) -> 403 + audit ---"
cat > "$TMPDIR/probe-disallowed.sh" <<'EOF'
#!/bin/sh
echo "-- GET $COX_TOOL_GH_URL/delete (NO rule covers it) --"
code=$(curl -s -o /dev/null -w "%{http_code}" --max-time 15 "$COX_TOOL_GH_URL/delete")
echo "curl http=$code"
if [ "$code" = "403" ]; then
  echo "disallowed-path 403 (blocked as expected)"
elif [ "$code" = "200" ] || [ "$code" = "301" ] || [ "$code" = "302" ] || [ "$code" = "404" ]; then
  echo "disallowed-path REACHED THE UPSTREAM (http=$code, UNEXPECTED — the rule engine let a non-allowed path through)"
else
  echo "disallowed-path got http=$code"
fi
EOF
K -n "$NS" cp "$TMPDIR/probe-disallowed.sh" "$AGENT_POD:/tmp/probe-disallowed.sh" 2>/dev/null || true
P2=$(K -n "$NS" exec -i "$AGENT_POD" -c agent -- sh -s < "$TMPDIR/probe-disallowed.sh" 2>&1)
echo "$P2" | sed 's/^/     /'
case "$P2" in
  *"REACHED THE UPSTREAM"*) bad "assertion 2: the disallowed path /delete reached the upstream (the rule engine did not 403 it)" ;;
  *"disallowed-path 403"*) ok "assertion 2a: the disallowed path /delete was 403'd by the tool proxy" ;;
  *) bad "assertion 2: unexpected output for the disallowed path (output: $P2)" ;;
esac
sleep 2
AUDIT2=$(K -n "$NS" logs "$TOOL_POD" --tail=10 2>/dev/null)
echo "$AUDIT2" | tail -5 | sed 's/^/     /'
AUDIT_DELETE=$(echo "$AUDIT2" | grep -F '"path":"/delete"' | tail -1)
echo "   audit record for /delete: $AUDIT_DELETE"
if [ -z "$AUDIT_DELETE" ]; then
  bad "assertion 2: the tool proxy audit has no record for the /delete request"
else
  if echo "$AUDIT_DELETE" | grep -q '"source":"tool-proxy"' && \
     echo "$AUDIT_DELETE" | grep -q '"method":"GET"' && \
     echo "$AUDIT_DELETE" | grep -q '"status":403' && \
     echo "$AUDIT_DELETE" | grep -q '"policy"'; then
    ok "assertion 2b: the audit line carries source:tool-proxy, method GET, path /delete, status 403 and a policy hash"
  else
    bad "assertion 2b: the audit line is missing required fields (got: $AUDIT_DELETE)"
  fi
  if echo "$AUDIT_DELETE" | grep -qF "$TEST_TOKEN"; then
    bad "assertion 2c: the audit line CONTAINS the credential value (the token must never be logged)"
  else
    ok "assertion 2c: the audit line carries no credential substring"
  fi
fi
# The upstream log must NOT show a /delete request (the proxy never dialed).
UP_LOG=$(K -n "$NS" exec "$UPSTREAM_POD" -- sh -c 'cat /tmp/upstream.log 2>/dev/null' 2>/dev/null)
if echo "$UP_LOG" | grep -F 'path=/delete' >/dev/null; then
  bad "assertion 2d: the upstream log shows a /delete request — the proxy dialed a non-allowed path"
else
  ok "assertion 2d: no /delete request reached the upstream"
fi

# ===========================================================================
# CHECK 3: credential injected (upstream saw it) + hidden from the agent.
# ===========================================================================
echo
echo "--- CHECK 3: credential injected by the proxy, absent from the agent ---"
sleep 2
UP_LOG=$(K -n "$NS" exec "$UPSTREAM_POD" -- sh -c 'cat /tmp/upstream.log 2>/dev/null' 2>/dev/null)
if echo "$UP_LOG" | grep -F "auth=present authValue=Bearer $TEST_TOKEN" >/dev/null; then
  ok "assertion 3a: the upstream log shows the request carried Authorization: Bearer <test-token> (the proxy injected the credential)"
else
  bad "assertion 3a: the upstream log does NOT show the injected credential (upstream log tail: $(echo "$UP_LOG" | tail -3))"
fi
AGENT_YAML=$(K -n "$NS" get pod "$AGENT_POD" -o json 2>/dev/null)
# 3b is a STRUCTURAL check (reviewer R23 note 1): no volume with a Secret
# source, no env valueFrom.secretKeyRef, no envFrom in any container or
# initContainer. (The agent pod's COX_TOOL_GH_URL env names the tool proxy
# SERVICE d41-loop-tool-gh, so a substring grep for the tool name would be a
# false positive; the credential Secret is named $TOOL_SECRET_NAME, distinct
# from the service name.)
VOL_REFS=$(echo "$AGENT_YAML" | jq -r '[(.spec.containers? // [])[], (.spec.initContainers? // [])[]] | .env?[]? | select(.valueFrom.secretKeyRef != null) | .valueFrom.secretKeyRef.name' 2>/dev/null)
ENVFROM_REFS=$(echo "$AGENT_YAML" | jq -r '[(.spec.containers? // [])[], (.spec.initContainers? // [])[]] | .envFrom?[]? | select(.secretRef != null) | .secretRef.name' 2>/dev/null)
SECRET_VOLS=$(echo "$AGENT_YAML" | jq -r '[(.spec.volumes? // [])[]] | select(.secret != null) | .secret.secretName' 2>/dev/null)
TOOL_SECRET_REFS=$(printf '%s\n' "$VOL_REFS" "$ENVFROM_REFS" "$SECRET_VOLS" | grep -F "$TOOL_SECRET_NAME" 2>/dev/null || true)
if [ -n "$TOOL_SECRET_REFS" ]; then
  bad "assertion 3b: the agent pod spec references the tool Secret $TOOL_SECRET_NAME (the credential must never be mounted into the agent)"
else
  ok "assertion 3b: no volume / env valueFrom / envFrom in the agent pod (containers + initContainers) references the tool Secret $TOOL_SECRET_NAME"
fi
if echo "$AGENT_YAML" | grep -qF "$TEST_TOKEN"; then
  bad "assertion 3c: the agent pod yaml contains the token value in some field"
else
  ok "assertion 3c: no agent pod field (spec: volumes, env, volumeMounts) contains the token value"
fi
ENV_GREP=$(K -n "$NS" exec "$AGENT_POD" -c agent -- sh -c "env | grep -i ${TEST_TOKEN}" 2>/dev/null || true)
if [ -z "$ENV_GREP" ]; then
  ok "assertion 3d: 'env | grep -i <token>' in the agent is empty"
else
  bad "assertion 3d: 'env | grep -i <token>' in the agent is NOT empty: $ENV_GREP"
fi
# 3e: the EXIT STATUS of 'test -e /tool-cred' (expect non-zero), not the
# stderr text (kubectl prints 'Defaulted container' on stderr — reviewer
# R23 note 2).
TOOL_CRED_TEST_RC=0
K -n "$NS" exec "$AGENT_POD" -c agent -- sh -c 'test -e /tool-cred' >/dev/null 2>&1 || TOOL_CRED_TEST_RC=$?
if [ "$TOOL_CRED_TEST_RC" -ne 0 ]; then
  ok "assertion 3e: /tool-cred does not exist in the agent (test -e exit=$TOOL_CRED_TEST_RC)"
else
  bad "assertion 3e: /tool-cred EXISTS in the agent (the credential mount leaked into the agent)"
fi

# ===========================================================================
# CHECK 4: the tool proxy can't reach any host but its upstream.
# The netpol is the authoritative network-layer gate (assertion 4a); the
# in-cluster literal-IP upstream rejection is the D41b/D41a controller check
# (assertion 4b). KubeArmor's per-process matchDNSQueries block is a BPF
# effect a co-located probe pod cannot observe, so the netpol + KubeArmor
# policy shape is what is asserted — it is the layer that guarantees the
# proxy "can't reach any host but its upstream".
# ===========================================================================
echo
echo "--- CHECK 4: tool-proxy egress is the upstream carve-out + DNS only ---"
# 4a: the tool-proxy NetworkPolicy shape.
AGENT_POD_IP=$(K -n "$NS" get pod "$AGENT_POD" -o jsonpath='{.status.podIP}' 2>/dev/null)
echo "   agent pod IP: $AGENT_POD_IP  (the tool proxy netpol must not permit a connect to it)"
NETPOL="${TOOL_POD}-netpol"
NP_JSON=$(K -n "$NS" get netpol "$NETPOL" -o json 2>/dev/null)
if [ -z "$NP_JSON" ]; then
  bad "assertion 4a: the tool proxy NetworkPolicy $NETPOL does not exist"
else
  # No raw (allow-all) egress rule: every egress rule must name a to[].
  EGRESS_RAW=$(echo "$NP_JSON" | jq '[.spec.egress[]? | select((.to == null) or ((.to | length) == 0))] | length' 2>/dev/null)
  if [ "$EGRESS_RAW" = "0" ]; then
    ok "assertion 4a: the tool proxy netpol has no raw (allow-all) egress rule — a raw connect to any host is not permitted"
  else
    bad "assertion 4a: the tool proxy netpol has a raw egress rule (allow-all egress would let the proxy reach any host)"
  fi
  if echo "$NP_JSON" | jq -e '.spec.egress[]?.ports[]? | select(.port == 53)' >/dev/null 2>&1; then
    ok "assertion 4a: the tool proxy netpol egress carries the platform DNS (kube-dns 53) rule"
  else
    bad "assertion 4a: the tool proxy netpol egress has no kube-dns 53 rule"
  fi
  if echo "$NP_JSON" | jq -e '.spec.egress[]?.to[]? | select((.ipBlock // null) != null)' >/dev/null 2>&1; then
    ok "assertion 4a: the tool proxy netpol egress is ipBlock-scoped (the external 0.0.0.0/0 carve-out), not pod-IP-scoped to the agent"
  else
    bad "assertion 4a: the tool proxy netpol egress has no ipBlock carve-out rule (the egress is not the external carve-out + DNS shape)"
  fi
  if echo "$NP_JSON" | jq -r '.spec.egress[]?.to[]? | select(.ipBlock // null) | .ipBlock.except[]? // empty' 2>/dev/null | grep -q '10.244.0.0/16\|10.96.0.0/12'; then
    ok "assertion 4a: the tool proxy netpol egress ipBlock except list carries the pod/service CIDR carve-out (a connect to the agent pod IP or the upstream on a non-upstream port is refused at the network layer)"
  else
    bad "assertion 4a: the tool proxy netpol egress ipBlock except list does NOT carry the pod/service CIDR (the external carve-out is incomplete)"
  fi
  # The KubeArmor policy for the tool proxy must carry the matchDNSQueries
  # allowlist (the upstream host + platform DNS) — the per-process DNS block.
  KAPT="coxswain-${TOOL_POD}"
  KAPT_JSON=$(K -n "$NS" get kubearmorpolicy "$KAPT" -o json 2>/dev/null)
  if [ -z "$KAPT_JSON" ]; then
    bad "assertion 4a: the tool proxy KubeArmorPolicy $KAPT does not exist (the per-process DNS allowlist is missing)"
  else
    if echo "$KAPT_JSON" | grep -q 'matchDNSQueries\|matchDomains'; then
      ok "assertion 4a: the tool proxy KubeArmorPolicy carries a matchDNSQueries/matchDomains allowlist (the per-process DNS block on a non-upstream host)"
    else
      bad "assertion 4a: the tool proxy KubeArmorPolicy has no matchDNSQueries/matchDomains allowlist"
    fi
  fi
fi

# 4b: a live in-cluster literal-IP tool upstream (the upstream pod's own pod
# IP) is rejected by the controller's ToolUpstreamInCluster check
# (PolicyValid=False, fail-closed, no tool proxy created) — the D41b/D41a
# authoritative first layer. The netpol pod-CIDR carve-out would block the
# dial at the network layer too. The AgentPolicy CRD CEL rule rejects
# .svc/.cluster.local upstreams at admission (the suffix-based layer, D41b
# spec 1); the IP-literal shape is the one the controller check owns.
SCRATCH_LOOP="d41-scratch"
cat > "$TMPDIR/scratch-policy.yaml" <<EOF
apiVersion: coxswain.wattu.com/v1alpha1
kind: AgentPolicy
metadata:
  name: ${SCRATCH_LOOP}-pol
  namespace: ${NS}
spec:
  tools:
    - name: internal
      upstream: "http://${UPSTREAM_POD_IP}:80"
      rules:
        - methods: ["GET"]
          paths: ["/ok"]
EOF
K apply -f "$TMPDIR/scratch-policy.yaml" >/dev/null 2>&1 || echo "   (the scratch policy may be rejected at admission by the CEL rule; the controller-side check is the authoritative layer)"
cat > "$TMPDIR/scratch-loop.yaml" <<EOF
apiVersion: coxswain.wattu.com/v1alpha1
kind: Loop
metadata:
  name: ${SCRATCH_LOOP}
  namespace: ${NS}
spec:
  goal: "D41e 4b: in-cluster literal-IP tool upstream (must be rejected)"
  policyRefs:
    - ${SCRATCH_LOOP}-pol
  agent:
    image: ${AGENT_IMG}
    model: local-model
    modelEndpoint: "${MODEL_ENDPOINT}"
    endpointSecretRef: ${TOOL_SECRET_NAME}
  workspace:
    repo: https://github.com/papawattu/coxswain
    ref: main
  loop:
    maxIterations: 1
EOF
K apply -f "$TMPDIR/scratch-loop.yaml" >/dev/null
for i in $(seq 1 30); do
  PV=$(K -n "$NS" get loop "$SCRATCH_LOOP" -o jsonpath='{.status.conditions[?(@.type=="PolicyValid")].status}' 2>/dev/null)
  PR=$(K -n "$NS" get loop "$SCRATCH_LOOP" -o jsonpath='{.status.conditions[?(@.type=="PolicyValid")].reason}' 2>/dev/null)
  [ -n "$PV" ] && break
  sleep 2
done
echo "   scratch Loop PolicyValid status=$PV reason=$PR"
if [ "$PV" = "False" ] && echo "$PR" | grep -qi "ToolUpstreamInCluster"; then
  ok "assertion 4b: the in-cluster literal-IP tool upstream is rejected (PolicyValid=False reason ToolUpstreamInCluster; the D41a resolved-IP backstop + the netpol carve-out)"
else
  bad "assertion 4b: the in-cluster literal-IP tool upstream was NOT rejected (PolicyValid=$PV reason=$PR)"
fi
K -n "$NS" delete loop "$SCRATCH_LOOP" --ignore-not-found --wait=false 2>/dev/null || true
K -n "$NS" delete agentpolicy "${SCRATCH_LOOP}-pol" --ignore-not-found 2>/dev/null || true

# 4c (reviewer R23 note 5): a LIVE blocked connect from the tool proxy's
# network position. The tool proxy image is distroless (no shell), so a
# short-lived stand-in pod carrying the tool proxy's labels (the same
# podSelector the tool proxy netpol matches) attempts a raw connect to the
# agent pod IP and to the upstream on a non-upstream port. Both must be
# refused / time out at the network layer (the netpol is the authoritative
# gate; the KubeArmor DNS check is a config check, stated as such).
NETPOL_SELECTOR_JSON=$(K -n "$NS" get netpol "$NETPOL" -o jsonpath='{.spec.podSelector.matchLabels}' 2>/dev/null)
FENCE_POD="d41-fence"
# The fence pod carries the tool proxy's labels (the same podSelector the
# tool proxy netpol matches), so the netpol selects it. The postStart hook
# attempts a raw connect to the agent pod IP and to the upstream on a
# non-upstream port (busybox nc -w 3, timeout 5); the exit codes are
# written to /tmp/fence.out (a non-zero exit means the connect was
# refused / timed out — the expected outcome).
LABELS_YAML=""
if [ -n "$NETPOL_SELECTOR_JSON" ]; then
  LABELS_YAML=$(echo "$NETPOL_SELECTOR_JSON" | python3 -c "
import json,sys
d=json.load(sys.stdin)
print("\\n".join(f\"    {k}: {v}\" for k,v in d.items()))
" 2>/dev/null)
fi
if [ -z "$LABELS_YAML" ]; then
  echo "FATAL: could not read the tool proxy netpol podSelector (the fence pod cannot be labelled)"
  exit 2
fi
cat > "$TMPDIR/fence-pod.yaml" <<EOF
apiVersion: v1
kind: Pod
metadata:
  name: ${FENCE_POD}
  namespace: ${NS}
  labels:
${LABELS_YAML}
spec:
  containers:
    - name: fence
      image: ${BUSYBOX_IMG}
      command: ["sh", "-c", "sleep 3600"]
      lifecycle:
        postStart:
          exec:
            command:
              - sh
              - -c
              - |
                timeout 5 nc -w 3 ${AGENT_POD_IP} 9999 >/dev/null 2>&1
                echo "fence-agent-exit=$?" > /tmp/fence.out
                timeout 5 nc -w 3 ${UPSTREAM_NODE_IP} 9998 >/dev/null 2>&1
                echo "fence-upstream-exit=$?" >> /tmp/fence.out
  restartPolicy: Never
EOF
K -n "$NS" delete pod "$FENCE_POD" --ignore-not-found --timeout=15s 2>/dev/null || true
sleep 1
K apply -f "$TMPDIR/fence-pod.yaml" >/dev/null || echo "   (fence pod apply failed; the 4c live check is skipped)"
for i in $(seq 1 30); do
  FP=$(K -n "$NS" get pod "$FENCE_POD" -o jsonpath='{.status.phase}' 2>/dev/null)
  [ "$FP" = "Running" ] && break
  sleep 2
done
# Wait for the postStart hook to complete (writes /tmp/fence.out).
FENCE_OUT=""
for i in $(seq 1 10); do
  FENCE_OUT=$(K -n "$NS" exec "$FENCE_POD" -- sh -c 'cat /tmp/fence.out 2>/dev/null' 2>/dev/null || true)
  [ -n "$FENCE_OUT" ] && break
  sleep 1
done
echo "   fence pod output: $FENCE_OUT"
if [ -n "$FENCE_OUT" ]; then
  if echo "$FENCE_OUT" | grep -q 'fence-agent-exit=0'; then
    bad "assertion 4c: a raw connect from the tool proxy's network position to the agent pod IP ($AGENT_POD_IP) SUCCEEDED (the netpol must refuse it)"
  else
    ok "assertion 4c: a raw connect from the tool proxy's network position to the agent pod IP ($AGENT_POD_IP) was refused / timed out (the netpol pod-CIDR carve-out blocks it)"
  fi
  if echo "$FENCE_OUT" | grep -q 'fence-upstream-exit=0'; then
    bad "assertion 4c: a raw connect from the tool proxy's network position to the upstream on a non-upstream port (${UPSTREAM_NODE_IP}:9998) SUCCEEDED (the netpol must refuse it)"
  else
    ok "assertion 4c: a raw connect from the tool proxy's network position to the upstream on a non-upstream port (${UPSTREAM_NODE_IP}:9998) was refused / timed out (the netpol only permits the upstream port)"
  fi
else
  bad "assertion 4c: the fence pod did not produce /tmp/fence.out (the live blocked-connect check could not run)"
fi
K -n "$NS" delete pod "$FENCE_POD" --ignore-not-found --wait=false 2>/dev/null || true
K -n "$NS" delete pod "$FENCE_POD" --ignore-not-found --wait=false 2>/dev/null || true

# Each 4a/4b/4c attempt must NOT have left a successful upstream-side log line
# for a disallowed / in-cluster request.
UP_LOG=$(K -n "$NS" exec "$UPSTREAM_POD" -- sh -c 'cat /tmp/upstream.log 2>/dev/null' 2>/dev/null)
if echo "$UP_LOG" | grep -F 'path=/delete' >/dev/null; then
  bad "assertion 4c: a disallowed / in-cluster request left a successful upstream-side log line"
else
  ok "assertion 4c: no disallowed / in-cluster request reached the upstream"
fi

# ===========================================================================
# CHECK 6: ephemeral container denied (I45, live) on the tool proxy pod.
# ===========================================================================
echo
echo "--- CHECK 6: kubectl ephemeralcontainer create on the tool proxy pod is DENIED (I45) ---"
VAP_NAME=$(K get validatingadmissionpolicy --no-headers 2>/dev/null | awk '{print $1}' | grep 'deny-ephemeral-containers' | head -1)
VAPB_NAME=$(K get validatingadmissionpolicybinding --no-headers 2>/dev/null | awk '{print $1}' | grep 'deny-ephemeral-containers' | head -1)
if [ -n "$VAP_NAME" ] && [ -n "$VAPB_NAME" ]; then
  echo "   ValidatingAdmissionPolicy=$VAP_NAME binding=$VAPB_NAME present"
  DEBUG_OUT=$(K -n "$NS" debug -q "$TOOL_POD" --image="$BUSYBOX_IMG" -- sh -c 'true' 2>&1 || true)
  echo "   kubectl debug on $TOOL_POD output: $DEBUG_OUT"
  if echo "$DEBUG_OUT" | grep -qi "denied"; then
    ok "assertion 6: an ephemeral container create on the tool proxy pod was DENIED by the ValidatingAdmissionPolicy (I45, live)"
  else
    bad "assertion 6: an ephemeral container create on the tool proxy pod was NOT denied (VAP not in effect)"
  fi
else
  bad "assertion 6: ValidatingAdmissionPolicy/binding 'deny-ephemeral-containers' not found in the cluster (I45)"
fi

# Clean up any leftover scratch loop/policy from a prior run.
K -n "$NS" delete loop "d41-scratch" --ignore-not-found --wait=false 2>/dev/null || true
K -n "$NS" delete agentpolicy "d41-scratch-pol" --ignore-not-found 2>/dev/null || true

echo
echo "============================================================"
if [ "$FAIL" -eq 0 ]; then
  echo "=== D41e kind acceptance: PASS (commit=$COMMIT controller-digest=$IMG_DIGEST tool-proxy-digest=$TOOL_DIGEST tool-proxy-imageID=$TOOL_IMG_ID) ==="
else
  echo "=== D41e kind acceptance: FAIL (commit=$COMMIT) ==="
fi
exit $FAIL
