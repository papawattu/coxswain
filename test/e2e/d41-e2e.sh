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
TOOL_IMG="${TOOL_PROXY_IMAGE:-coxswain-tool-proxy:standin}"
# The D41e 4c connect-probe image: a tiny static Go net.DialTimeout probe
# built to /usr/local/bin/tool-proxy on distroless (UID 65535, matching the
# real tool-proxy), so the tool proxy's KubeArmorPolicy process allowlist
# (which permits ONLY /usr/local/bin/tool-proxy) permits the labeled fence pod
# to run it. It is NOT the real tool-proxy.
PROBE_IMG="coxswain-tool-proxy:probe"
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
  echo "--- cleaning up tool upstream (docker container + bridge) ---"
  K -n "$NS" delete pod d41-probe --ignore-not-found --wait=false 2>/dev/null || true
  docker rm -f "$UPSTREAM_CONT" >/dev/null 2>&1 || true
  docker network disconnect -f "$UPSTREAM_NET" "$NODE_CONTAINER" 2>/dev/null || true
  docker network rm "$UPSTREAM_NET" >/dev/null 2>&1 || true
  echo "   (upstream container $UPSTREAM_CONT + bridge $UPSTREAM_NET removed)"
  # Verify the cleanup: no $UPSTREAM_NET network, and the kind node lists
  # only the 'kind' network. (Don't touch any other docker network or
  # container — the owner's pixme-dev, gitea, sweep must be untouched.)
  if docker network ls --format '{{.Name}}' 2>/dev/null | grep -qx "$UPSTREAM_NET"; then
    echo "   WARNING: $UPSTREAM_NET still present after cleanup" >&2
  else
    echo "   (cleanup verified: $UPSTREAM_NET is gone)"
  fi
  NODE_NETS=$(docker inspect -f '{{json .NetworkSettings.Networks}}' "$NODE_CONTAINER" 2>/dev/null)
  if [ "$NODE_NETS" = "null" ] || ! echo "$NODE_NETS" | python3 -c "import sys,json; d=json.load(sys.stdin); sys.exit(0 if list(d.keys())==['kind'] else 1)" 2>/dev/null; then
    echo "   WARNING: the kind node $NODE_CONTAINER lists unexpected networks: $NODE_NETS" >&2
  else
    echo "   (cleanup verified: the kind node lists only 'kind')"
  fi
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
# The tool proxy image: built from the repo UNLESS TOOL_PROXY_IMAGE_PREBUILT=1
# (a mutation run pre-builds a scratch-tagged tool-proxy image from a scratch
# worktree and wants the e2e to use it as-is, not rebuild the real one).
if [ "${TOOL_PROXY_IMAGE_PREBUILT:-0}" = "1" ]; then
  echo "   using pre-built tool-proxy image $TOOL_IMG (TOOL_PROXY_IMAGE_PREBUILT=1; not rebuilding)"
  if ! docker image inspect "$TOOL_IMG" >/dev/null 2>&1; then
    echo "FATAL: TOOL_PROXY_IMAGE_PREBUILT=1 but $TOOL_IMG is not present"; exit 2
  fi
else
  echo "   building tool-proxy image $TOOL_IMG ..."
  (cd "$REPO_ROOT" && docker build -q -t "$TOOL_IMG" -f cmd/tool-proxy/Dockerfile .) || { echo "FATAL: tool-proxy build failed"; exit 2; }
fi
echo "   building 4c connect-probe image $PROBE_IMG ..."
(cd "$REPO_ROOT/test/e2e/probe" && docker build -q -t "$PROBE_IMG" .) || { echo "FATAL: probe build failed"; exit 2; }
IMG_DIGEST="$(docker image inspect "$IMG" --format '{{.Id}}' 2>/dev/null)"
TOOL_DIGEST="$(docker image inspect "$TOOL_IMG" --format '{{.Id}}' 2>/dev/null)"
echo "   controller image digest: $IMG_DIGEST"
  echo "   tool-proxy image digest: $TOOL_DIGEST"
for img in "$IMG" "$TOOL_IMG" "$PROBE_IMG" "$BUSYBOX_IMG"; do
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
# If a non-default tool-proxy image is requested (a mutation run), patch the
# controller deployment's args to pass --tool-proxy-image=$TOOL_IMG so the
# controller deploys the tool proxy pod with that image (the controller's
# default is coxswain-tool-proxy:standin). Only applied when TOOL_IMG differs
# from the default.
if [ -n "${TOOL_PROXY_IMAGE:-}" ] && [ "$TOOL_IMG" != "coxswain-tool-proxy:standin" ]; then
  echo "   patching controller to add --tool-proxy-image=$TOOL_IMG (mutation / non-default tool-proxy image)"
  # Append the flag to the controller's existing args (do NOT replace the list,
  # which carries --metrics-bind-address, --leader-elect, --health-probe-bind-address,
  # --allow-unenforced, --allow-unenforced-network, --runner-image).
  K -n "$E2E_NS" patch deploy coxswain-controller-manager --type=json -p "[{\"op\":\"add\",\"path\":\"/spec/template/spec/containers/0/args/-\",\"value\":\"--tool-proxy-image=$TOOL_IMG\"}]" 2>/dev/null \
    || echo "   (warning: could not append --tool-proxy-image; the tool proxy pod may run the default image)"
  K -n "$E2E_NS" rollout status deploy/coxswain-controller-manager --timeout=120s || echo "   (rollout after --tool-proxy-image patch may be pending)"
fi
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
# The tool upstream must be reachable at an address OUTSIDE every
# product-defined carve-out (the 172.16/12 RFC1918 range, 10/8,
# 192.168/16, 169.254/16, 127/8, 100.64/10, 0/8, 224/4, 240/4 — the
# tool-proxy netpol except-list + the tool-proxy's resolved-IP backstop).
# No private/LAN address can be used (they are all carved out), so the
# upstream lives on a temporary DEDICATED docker bridge in the RFC 2544
# benchmarking range 198.18.0.0/15 (in no carve-out, never routed to a
# real host). The kind control-plane container is attached to this bridge,
# and a dedicated upstream container (hostNetwork-equivalent: it runs the
# python listener and is reachable at its bridge IP from the kind cluster)
# serves :80. The bridge + containers are torn down in the EXIT trap.
UPSTREAM_NET="d41-upstream-net"
UPSTREAM_SUBNET="198.18.0.0/24"
# Create the docker bridge (idempotent: disconnect the kind node from a prior
# one first, then remove it — a network rm FAILS while the kind node is still
# attached, so the disconnect must precede the rm).
if docker network inspect "$UPSTREAM_NET" >/dev/null 2>&1; then
  docker network disconnect -f "$UPSTREAM_NET" "$NODE_CONTAINER" 2>/dev/null || true
  docker network rm "$UPSTREAM_NET" >/dev/null 2>&1 || true
fi
docker network create --subnet "$UPSTREAM_SUBNET" "$UPSTREAM_NET" >/dev/null   || { echo "FATAL: could not create the docker bridge $UPSTREAM_NET ($UPSTREAM_SUBNET)"; exit 2; }
echo "   created docker bridge $UPSTREAM_NET ($UPSTREAM_SUBNET, RFC 2544 — in no carve-out)"
# Attach the kind control-plane container to the bridge so a pod on the kind
# cluster can reach the upstream container's bridge IP.
docker network connect "$UPSTREAM_NET" "$NODE_CONTAINER" 2>/dev/null   || { echo "FATAL: could not attach $NODE_CONTAINER to $UPSTREAM_NET"; exit 2; }
# A dedicated upstream container on the bridge (the python:3-alpine listener
# on :80). It is the "hostNetwork-equivalent": a pod on the kind cluster
# reaches it at the bridge IP via the attached control-plane container.
UPSTREAM_CONT="d41-upstream-cont"
docker rm -f "$UPSTREAM_CONT" >/dev/null 2>&1 || true
# Run the upstream container (python:3-alpine) on the bridge with a sleep
# entrypoint (the listener is docker cp'd in after start and nohup'd).
docker run -d --name "$UPSTREAM_CONT" --network "$UPSTREAM_NET" \
  --entrypoint sh python:3-alpine -c "sleep 3600" >/dev/null 2>&1 \
  || { echo "FATAL: could not start the upstream container"; exit 2; }
sleep 1
# Write the listener into the container (docker cp), then start it (nohup).
docker cp "$UPSTREAM_SCRIPT" "$UPSTREAM_CONT:/usr/local/bin/upstream-server.py" 2>/dev/null   || { echo "FATAL: could not docker cp the upstream listener"; exit 2; }
docker exec "$UPSTREAM_CONT" sh -c ': > /tmp/upstream.log && nohup python3 /usr/local/bin/upstream-server.py > /tmp/upstream.log 2>&1 & echo started' 2>/dev/null   || { echo "FATAL: could not start the upstream listener"; exit 2; }
# The upstream IP is the container's bridge address.
UPSTREAM_NODE_IP=$(docker inspect -f '{{range .NetworkSettings.Networks}}{{.IPAddress}}{{"\n"}}{{end}}' "$UPSTREAM_CONT" 2>/dev/null | grep -E '^[0-9.]+' | head -1)
# The bridge's gateway is 198.18.0.1; the container gets 198.18.0.2+.
if [ -z "$UPSTREAM_NODE_IP" ]; then
  echo "FATAL: could not read the upstream container's bridge IP"
  exit 2
fi
echo "   upstream container $UPSTREAM_CONT running at $UPSTREAM_NODE_IP (RFC 2544 bridge)"
# The tool upstream URL is the bridge container IP:80 (RFC 2544 — in no
# carve-out, so the controller's ToolUpstreamInCluster check passes AND the
# tool-proxy netpol external carve-out + the tool-proxy's resolved-IP
# backstop permit the dial — the correct product behaviour, not a carve-out
# change). The model endpoint is a DUMMY (never dialed by the agent in this
# test: the D41e assertions exercise the tool proxy path, not the model
# path); it must pass the AgentPolicy CRD CEL rule (no .svc/.cluster.local).
UPSTREAM_URL="http://${UPSTREAM_NODE_IP}:80"
MODEL_ENDPOINT="${UPSTREAM_NODE_IP}:80"
# Wait for the listener to bind :80 (poll from the host via docker exec into
# the upstream container).
for i in $(seq 1 30); do
  if docker exec "$UPSTREAM_CONT" python3 -c "import urllib.request; urllib.request.urlopen('http://127.0.0.1:80/ok', timeout=3).read()" 2>/dev/null; then
    break
  fi
  sleep 1
done
if ! docker exec "$UPSTREAM_CONT" python3 -c "import urllib.request; urllib.request.urlopen('http://127.0.0.1:80/ok', timeout=3).read()" 2>/dev/null; then
  echo "FATAL: the upstream listener is not answering on the bridge container (node-side check)"
  exit 2
fi
echo "   upstream listener answering (container-side check)"
# Verify from the HOST that the bridge IP is reachable (the pod will reach it
# via the attached control-plane container).
if ! docker exec "$NODE_CONTAINER" python3 -c "import urllib.request; urllib.request.urlopen('http://${UPSTREAM_NODE_IP}:80/ok', timeout=3).read()" 2>/dev/null; then
  echo "FATAL: the upstream listener is not reachable at ${UPSTREAM_NODE_IP}:80 from the kind control-plane container (the bridge is not routable to the cluster)"
  exit 2
fi
echo "   upstream listener reachable at ${UPSTREAM_NODE_IP}:80 from the kind control-plane container"
# The upstream is now the docker-bridge container (not a k8s pod). The probe
# preflight: a plain busybox pod (no tool-proxy labels) on the kind cluster
# dials the bridge IP. A 200 means the bridge is routable to the cluster and
# the listener answers; anything else is a FATAL (the assertions would be
# meaningless).
cat > "$TMPDIR/probe-pod.yaml" <<EOF
apiVersion: v1
kind: Pod
metadata:
  name: d41-probe
  namespace: ${NS}
spec:
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
PROBE_OUT=$(K -n "$NS" exec d41-probe -- sh -c "wget -q -O /dev/null --timeout=5 http://${UPSTREAM_NODE_IP}:80/ok && echo 200 || echo FAIL" 2>&1)
echo "   probe pod wget http://${UPSTREAM_NODE_IP}:80/ok -> $PROBE_OUT"
case "$PROBE_OUT" in
  200) ok "preflight: the upstream answers 200 at ${UPSTREAM_NODE_IP}:80 from a plain pod (before the assertions)" ;;
  *) bad "preflight: the upstream does NOT answer 200 at ${UPSTREAM_NODE_IP}:80 from a plain pod (got: $PROBE_OUT) — the bridge is not routable to the cluster" ;;
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
UP_LOG=$(docker exec "$UPSTREAM_CONT" sh -c 'cat /tmp/upstream.log 2>/dev/null' 2>/dev/null)
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
UP_LOG=$(docker exec "$UPSTREAM_CONT" sh -c 'cat /tmp/upstream.log 2>/dev/null' 2>/dev/null)
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
# Wait for the controller to create the tool proxy NetworkPolicy (it is
# created when the tool proxy pod is reconciled, which lags the pod's Ready
# slightly). A missing netpol here is a timing flake, not a product failure.
for i in $(seq 1 20); do
  NP_JSON=$(K -n "$NS" get netpol "$NETPOL" -o json 2>/dev/null)
  [ -n "$NP_JSON" ] && break
  sleep 3
done
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
      upstream: "http://${AGENT_POD_IP}:80"
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
# Wait for the netpol's podSelector to be readable (the controller may be
# mid-reconcile between 4a and 4c; the netpol exists, this is a read-timing
# guard so the fence pod can be labelled).
for i in $(seq 1 10); do
  [ -n "$NETPOL_SELECTOR_JSON" ] && break
  NETPOL_SELECTOR_JSON=$(K -n "$NS" get netpol "$NETPOL" -o jsonpath='{.spec.podSelector.matchLabels}' 2>/dev/null)
  sleep 2
done
# 4c (reviewer R23 note 5): a LIVE blocked connect from the tool proxy's
# network position, using a faithful connect-probe. The tool proxy's
# KubeArmorPolicy process allowlist permits ONLY /usr/local/bin/tool-proxy, so
# a labeled stand-in "fence" pod can run only that binary. The probe
# ($PROBE_IMG) is a tiny static Go net.DialTimeout connect-probe built to that
# exact path (distroless, UID 65535 — matching the real tool-proxy), so the
# allowlist permits it. The fence pod carries the tool proxy's FULL label set
# (the netpol's podSelector), image $PROBE_IMG, and command
# ["/usr/local/bin/tool-proxy", <host>, <port>]. The probe dials host:port and
# prints "connected" (exit 0) or an error (exit 1).
#
# Four checks, each a separate fence pod (the probe exits after one dial):
#   (a) agent pod IP:8080            -> FAIL  (the netpol pod-CIDR carve-out refuses it)
#   (b) upstream IP:81 (non-upstream) -> FAIL  (the netpol allows the external IP, but the upstream container only listens on :80, so :81 is refused)
#   (c) a DNS name to a non-upstream external host -> FAIL (the connect to an unroutable / non-upstream external host fails at the network layer)
#   (d) upstream IP:80 (control)      -> SUCCEED (the upstream is live on :80; proves the probe works, so the failures mean something)
#
# A check is NEVER reported as PASS if the probe did not run (the fence pod
# never produced output) — it is reported as NOT RUN.
FENCE_BASE="d41-fence"
# Build the fence pod's label YAML from the netpol's podSelector matchLabels
# (the tool proxy's FULL label set, so the netpol's podSelector matches).
LABELS_YAML=""
if [ -n "$NETPOL_SELECTOR_JSON" ]; then
  LABELS_YAML=$(echo "$NETPOL_SELECTOR_JSON" | python3 -c 'import json,sys; d=json.load(sys.stdin); print("\n".join("    %s: %s" % (k,v) for k,v in d.items()))' 2>/dev/null)
fi
if [ -z "$LABELS_YAML" ]; then
  echo "FATAL: could not read the tool proxy netpol podSelector (the fence pod cannot be labelled)"
  exit 2
fi
probe_connect() {
  # probe_connect <name> <host> <port> -> sets PROBE_RESULT="connected"|"error: ..." and PROBE_RC
  local name="$1" host="$2" port="$3"
  local pod="${FENCE_BASE}-${name}"
  local apply_err=""
  PROBE_RESULT=""
  PROBE_RC=""
  K -n "$NS" delete pod "$pod" --ignore-not-found --timeout=15s 2>/dev/null || true
  sleep 1
  cat > "$TMPDIR/fence-${name}.yaml" <<EOF
apiVersion: v1
kind: Pod
metadata:
  name: ${pod}
  namespace: ${NS}
  labels:
${LABELS_YAML}
spec:
  containers:
    - name: fence
      image: ${PROBE_IMG}
      command:
        - /usr/local/bin/tool-proxy
        - "${host}"
        - "${port}"
  restartPolicy: Never
EOF
  apply_err=$(K apply -f "$TMPDIR/fence-${name}.yaml" 2>&1 >/dev/null)
  if [ -n "$apply_err" ]; then
    echo "   (fence pod ${name} apply failed: $apply_err)"
    return 1
  fi
  # The probe exits after the single dial (connected -> 0, refused -> 1). Wait
  # for the pod to reach a terminal phase (Succeeded/Failed).
  local phase=""
  for i in $(seq 1 20); do
    phase=$(K -n "$NS" get pod "$pod" -o jsonpath='{.status.phase}' 2>/dev/null)
    if [ "$phase" = "Succeeded" ] || [ "$phase" = "Failed" ]; then break; fi
    sleep 2
  done
  PROBE_RESULT=$(K -n "$NS" logs "$pod" 2>/dev/null || true)
  PROBE_RC=$(K -n "$NS" get pod "$pod" -o jsonpath='{.status.containerStatuses[0].state.terminated.exitCode}' 2>/dev/null)
  K -n "$NS" delete pod "$pod" --ignore-not-found --wait=false 2>/dev/null || true
  echo "   probe ${name}: host=${host} port=${port} phase=${phase} rc=${PROBE_RC} result=[$PROBE_RESULT]"
  return 0
}
# (a) agent pod IP:8080 must FAIL (netpol pod-CIDR carve-out)
probe_connect a "$AGENT_POD_IP" 8080
if [ -z "$PROBE_RESULT" ]; then
  echo "   NOT RUN: assertion 4c(a): the probe produced no output (the live blocked-connect could not run)"
else
  if echo "$PROBE_RESULT" | grep -q '^connected$'; then
    bad "assertion 4c(a): a connect from the tool proxy's network position to the agent pod IP ($AGENT_POD_IP):8080 SUCCEEDED (the netpol pod-CIDR carve-out must refuse it)"
  else
    ok "assertion 4c(a): a connect from the tool proxy's network position to the agent pod IP ($AGENT_POD_IP):8080 was refused (the netpol pod-CIDR carve-out blocks it): $PROBE_RESULT"
  fi
fi
# (b) upstream IP:81 (non-upstream port) must FAIL (the upstream only listens on :80)
probe_connect b "$UPSTREAM_NODE_IP" 81
if [ -z "$PROBE_RESULT" ]; then
  echo "   NOT RUN: assertion 4c(b): the probe produced no output (the live blocked-connect could not run)"
else
  if echo "$PROBE_RESULT" | grep -q '^connected$'; then
    bad "assertion 4c(b): a connect from the tool proxy's network position to the upstream IP ($UPSTREAM_NODE_IP):81 SUCCEEDED (the upstream only serves :80; a non-upstream port must be refused)"
  else
    ok "assertion 4c(b): a connect from the tool proxy's network position to the upstream IP ($UPSTREAM_NODE_IP):81 (a non-upstream port) was refused: $PROBE_RESULT"
  fi
fi
# (c) a DNS name to a non-upstream external host must FAIL (network-layer refusal / unroutable)
probe_connect c "non-upstream-external.invalid" 80
if [ -z "$PROBE_RESULT" ]; then
  echo "   NOT RUN: assertion 4c(c): the probe produced no output (the live blocked-connect could not run)"
else
  if echo "$PROBE_RESULT" | grep -q '^connected$'; then
    bad "assertion 4c(c): a connect from the tool proxy's network position to a non-upstream external host SUCCEEDED (a non-upstream external host must not be reachable)"
  else
    ok "assertion 4c(c): a connect from the tool proxy's network position to a non-upstream external host (non-upstream-external.invalid:80) was refused / failed: $PROBE_RESULT"
  fi
fi
# (d) control: upstream IP:80 must SUCCEED (the upstream is live on :80; proves the probe works)
probe_connect d "$UPSTREAM_NODE_IP" 80
if [ -z "$PROBE_RESULT" ]; then
  echo "   NOT RUN: assertion 4c(d): the probe produced no output (the control connect could not run) — the 4c(a-c) failures are UNVERIFIED"
else
  if echo "$PROBE_RESULT" | grep -q '^connected$'; then
    ok "assertion 4c(d): control — a connect from the tool proxy's network position to the upstream IP ($UPSTREAM_NODE_IP):80 SUCCEEDED (the upstream is live; the probe works, so the 4c(a-c) refusals are meaningful)"
  else
    bad "assertion 4c(d): control FAILED — a connect to the upstream IP ($UPSTREAM_NODE_IP):80 did NOT succeed (the upstream is not live, or the probe is broken; the 4c(a-c) failures are UNVERIFIED): $PROBE_RESULT"
  fi
fi

# Each 4a/4b/4c attempt must NOT have left a successful upstream-side log line
# for a disallowed / in-cluster request.
UP_LOG=$(docker exec "$UPSTREAM_CONT" sh -c 'cat /tmp/upstream.log 2>/dev/null' 2>/dev/null)
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
