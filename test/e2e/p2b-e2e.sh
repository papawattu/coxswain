#!/usr/bin/env bash
# P2b kind acceptance (docs/TDD-PLAN-PHASE2.md "P2b" + ADR-0009, "kind run"
# validation section): the metering model proxy end-to-end.
#
# What it proves:
#   1. The metering proxy DIALS SUCCESS: an agent-shaped request to the model
#      endpoint (via the proxy) returns 2xx from the in-kind fake model; the
#      metering proxy's usage endpoint (GET /coxswain/usage on 9090) shows the
#      dial (promptTokens/completionTokens/requests advanced; a bootID present;
#      unmeteredRequests==0 on the success dial).
#   2. The metering proxy read the mounted model credential and injected it
#      (the fake model's own side logs the Authorization header it received).
#   3. The <loop>-proxy NetworkPolicy carries the usage-port (9090) ingress for
#      the operator namespace / controller-manager + the model egress rule; the
#      metering proxy pod carries the usage env + the proxy-usage emptyDir + the
#      metering image.
#   4. Image digests recorded (the operator, the metering proxy, the fake model).
#   5. The metering proxy's audit log + the usage reading are captured (tee'd).
#
# The in-kind fake model is a hostNetwork python:3-alpine pod on the kind node
# (imagePullPolicy IfNotPresent) that serves an OpenAI-compatible
# /v1/chat/completions (JSON usage; an SSE stream with a usage chunk when
# stream=true). It logs each request's Authorization header + Accept-Encoding +
# stream_options.include_usage to stdout so the proxy's auth injection and
# no-Accept-Encoding behaviour are asserted from the fake model's own side.
#
# The agent request is driven by an in-cluster curl pod that POSTs to the metering
# proxy at the model endpoint (the proxy service:8080) — exactly the path the real
# runner takes (COX_MODEL_BASE_URL = the proxy service:8080; the runner appends
# /v1/chat/completions). No real model and no real runner loop are needed: the
# property is the proxy's metering + dial, driven by the same-shaped request the
# runner makes. The dial-block path (dial-failure -> AddUnmetered, no usage
# recorded) is the same meteringRoundTripper fallback, unit-tested in
# internal/proxy/proxy_test.go; the kind e2e proves the success dial end-to-end.
#
# Pinned by the caller: K8S_CONTEXT=kind-coxswain-dev (and CLUSTER=coxswain-dev
# for the kind load). The operator runs the DEV overlay (Loops run).
#
# The script exits non-zero on any failed assertion.

set -euo pipefail

GOPATH_BIN="$(go env GOPATH 2>/dev/null)/bin"
REPO_ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
case ":$PATH:" in *":$GOPATH_BIN:"*) ;; *) PATH="$GOPATH_BIN:$REPO_ROOT/bin:$PATH"; export PATH ;; esac

CTX="${K8S_CONTEXT:-kind-coxswain-dev}"
CLUSTER="${KIND_CLUSTER_NAME:-coxswain-dev}"
NS="p2b-e2e"
E2E_NS="coxswain-system"
LOOP="p2b-loop"
FAKE_POD="p2b-fake-model"
FAKE_IMG="python:3-alpine"
CURL_IMG="python:3-alpine"   # the curler reuses the fake-model image (always present on the kind node)
COMMIT="$(git -C "$REPO_ROOT" rev-parse --short HEAD 2>/dev/null || echo unknown)"
IMG_TAG="main-${COMMIT}"
CTRL_IMG="coxswain-controller:${IMG_TAG}"
PROXY_IMG="coxswain-proxy:metering"   # the reconciler default (proxyImage)
FAKE_PORT=8443
TEST_KEY="p2b-dummy-key-7f3a2b1c"

K() { kubectl --context "$CTX" "$@"; }

LOG_DIR="${P2B_LOG_DIR:-$REPO_ROOT/.samples/p2b}"
mkdir -p "$LOG_DIR"
PASS=0; FAIL=0
ok()   { echo "   [PASS] $*"; PASS=$((PASS+1)); }
bad()  { echo "   [FAIL] $*"; FAIL=$((FAIL+1)); }
check() { local d="$1"; shift; if "$@" >/dev/null 2>&1; then ok "$d"; else bad "$d"; fi; }

echo "===================================================================="
echo " P2b kind acceptance (metering model proxy)  commit=$COMMIT"
echo " context=$CTX  cluster=$CLUSTER  ns=$NS"
echo "===================================================================="

# ===========================================================================
# STEP 0: images — build the controller + the metering proxy, kind-load both.
# ===========================================================================
echo "--- STEP 0: build + kind-load images (controller, metering proxy) ---"
kubectl --context "$CTX" get nodes >/dev/null 2>&1 || { echo "FATAL: cannot reach cluster $CTX"; exit 2; }
(cd "$REPO_ROOT" && docker build -q -t "$CTRL_IMG" -f Dockerfile .) \
  || { echo "FATAL: controller build failed"; exit 2; }
(cd "$REPO_ROOT" && docker build -q -t "$PROXY_IMG" -f cmd/model-proxy/Dockerfile .) \
  || { echo "FATAL: metering proxy build failed"; exit 2; }
for img in "$CTRL_IMG" "$PROXY_IMG" "$FAKE_IMG" "$CURL_IMG"; do
  echo "   kind load: $img"
  kind load docker-image "$img" --name "$CLUSTER" || { echo "FATAL: kind load $img failed"; exit 2; }
done

# ===========================================================================
# STEP 1: deploy the operator (dev overlay, --allow-unenforced). The
# --proxy-image default is the metering image (the reconciler default) and
# --operator-namespace defaults to $POD_NAMESPACE (coxswain-system) — both
# already wired; no extra flag needed. The proxy-usage usage-port ingress is
# created against that operator namespace.
# ===========================================================================
echo
echo "--- STEP 1: deploy the operator (dev overlay) ---"
make -C "$REPO_ROOT" kustomize >/dev/null 2>&1
KUSTOMIZE_BIN="$REPO_ROOT/bin/kustomize"
TMP_OVERLAY=$(mktemp -d)
trap 'rm -rf "$TMP_OVERLAY"' EXIT
cp -r "$REPO_ROOT/config" "$TMP_OVERLAY/config"
(cd "$TMP_OVERLAY/config/manager" && "$KUSTOMIZE_BIN" edit set image controller="$CTRL_IMG")
(cd "$TMP_OVERLAY" && "$KUSTOMIZE_BIN" build config/dev | K apply -f -) \
  || { echo "FATAL: controller deploy failed"; exit 2; }
(cd "$TMP_OVERLAY" && "$KUSTOMIZE_BIN" build config/cni-probe | K apply -f -) \
  || echo "   (cni-probe ns/RBAC deploy skipped)"
K -n "$E2E_NS" rollout status deploy/coxswain-controller-manager --timeout=180s \
  || { echo "FATAL: controller not ready"; exit 2; }
K -n "$E2E_NS" get pods -l control-plane=controller-manager \
  -o jsonpath='{.items[0].status.containerStatuses[0].imageID}' > "$LOG_DIR/operator-imageID.txt" 2>/dev/null || true
echo "   operator imageID: $(cat "$LOG_DIR/operator-imageID.txt")"

# ===========================================================================
# STEP 2: the in-kind fake model (hostNetwork) + the model-creds Secret.
# ===========================================================================
echo
echo "--- STEP 2: in-kind fake model + model-creds Secret ---"
K create ns "$NS" --dry-run=client -o yaml | K apply -f - >/dev/null
NODE_IP=$(K get node -o jsonpath='{.items[0].status.addresses[?(@.type=="InternalIP")].address}' 2>/dev/null | head -1)
MODEL_ENDPOINT="${NODE_IP}:${FAKE_PORT}"
echo "   fake model at $MODEL_ENDPOINT (node $NODE_IP)"

# The fake model server, carried in a ConfigMap (avoids a nested-heredoc YAML
# pitfall and a kubectl cp race): the pod mounts it and runs it.
K -n "$NS" delete pod "$FAKE_POD" --ignore-not-found >/dev/null 2>&1 || true
sleep 2
K create configmap p2b-fake-model --from-file=p2b-fake-model.py="$REPO_ROOT/test/e2e/p2b-fake-model.py" -n "$NS" --dry-run=client -o yaml | K apply -f - >/dev/null
K apply -f - >/dev/null <<EOF
apiVersion: v1
kind: Pod
metadata:
  name: $FAKE_POD
  namespace: $NS
  labels: {app: p2b-fake-model}
spec:
  hostNetwork: true
  containers:
  - name: fake-model
    image: $FAKE_IMG
    imagePullPolicy: IfNotPresent
    command: ["sh","-c","exec python /opt/fake-model/p2b-fake-model.py"]
    env:
    - name: PORT
      value: "$FAKE_PORT"
    volumeMounts:
    - name: server
      mountPath: /opt/fake-model
  volumes:
  - name: server
    configMap:
      name: p2b-fake-model
EOF
K -n "$NS" wait --for=condition=Ready pod/"$FAKE_POD" --timeout=120s 2>/dev/null || true
for i in $(seq 1 30); do
  if K -n "$NS" exec "$FAKE_POD" -- python -c "import urllib.request,socket;socket.setdefaulttimeout(3);urllib.request.urlopen('http://127.0.0.1:$FAKE_PORT/ok').read()" >/dev/null 2>&1; then break; fi
  sleep 1
done
K -n "$NS" exec "$FAKE_POD" -- python -c "import urllib.request,socket;socket.setdefaulttimeout(3);urllib.request.urlopen('http://127.0.0.1:$FAKE_PORT/ok').read()" >/dev/null 2>&1 \
  || { echo "FATAL: fake model not answering"; K -n "$NS" logs "$FAKE_POD" 2>/dev/null | tail -20; exit 2; }
ok "in-kind fake model is answering on $MODEL_ENDPOINT"

# The metering proxy reads /model-creds/.data/model-key (a Secret key literally named
# model-key, the operator's proxy-creds contract). MODEL_BASE_URL carries the endpoint for
# the agent's COX_MODEL_BASE_URL; the proxy dials the Loop spec's modelEndpoint.
K -n "$NS" create secret generic p2b-model-creds \
  --from-literal=model-key="$TEST_KEY" \
  --from-literal=MODEL_BASE_URL="http://$MODEL_ENDPOINT" \
  --dry-run=client -o yaml | K apply -f - >/dev/null

# ===========================================================================
# STEP 3: the Loop (the operator builds the metering proxy pod).
# ===========================================================================
echo
echo "--- STEP 3: create the Loop (operator builds the metering proxy pod) ---"
K -n "$NS" delete loop "$LOOP" --wait=false --ignore-not-found 2>/dev/null || true
sleep 3
K apply -f - >/dev/null <<EOF
apiVersion: coxswain.wattu.com/v1alpha1
kind: Loop
metadata:
  name: $LOOP
  namespace: $NS
spec:
  goal: P2b metering model proxy e2e
  workspace:
    repo: https://github.com/example/repo
    ref: main
  agent:
    image: golang:1.26
    model: fake
    endpointSecretRef: p2b-model-creds
    modelEndpoint: $MODEL_ENDPOINT
  loop:
    maxIterations: 1
EOF
for i in $(seq 1 60); do
  if K -n "$NS" get pod "$LOOP-proxy" >/dev/null 2>&1 && \
     [ "$(K -n "$NS" get pod "$LOOP-proxy" -o jsonpath='{.status.phase}' 2>/dev/null)" = "Running" ]; then
    break
  fi
  sleep 2
done
check "metering proxy pod $LOOP-proxy is Running" \
  test "$(K -n "$NS" get pod "$LOOP-proxy" -o jsonpath='{.status.phase}' 2>/dev/null)" = "Running"
K -n "$NS" get pod "$LOOP-proxy" -o jsonpath='{.spec.containers[0].image}' > "$LOG_DIR/proxy-image.txt" 2>/dev/null || true
K -n "$NS" get pod "$LOOP-proxy" -o jsonpath='{.status.containerStatuses[0].imageID}' > "$LOG_DIR/proxy-imageID.txt" 2>/dev/null || true
K -n "$E2E_NS" logs deploy/coxswain-controller-manager --tail=200 > "$LOG_DIR/operator.log" 2>&1 || true
echo "   proxy pod image:   $(cat "$LOG_DIR/proxy-image.txt")"
echo "   proxy pod imageID: $(cat "$LOG_DIR/proxy-imageID.txt")"

# ===========================================================================
# STEP 4: assertions.
# ===========================================================================
echo
echo "--- STEP 4: assertions ---"

# 4a. The metering proxy pod spec: the usage env + the proxy-usage volume + the
#     metering image.
PROXY_ENV=$(K -n "$NS" get pod "$LOOP-proxy" -o jsonpath='{.spec.containers[0].env}' 2>/dev/null || true)
for key in PROXY_PORT_USAGE PROXY_USAGE_FILE LOOP_NAME LOOP_NAMESPACE MODEL_CRED_FILE; do
  if echo "$PROXY_ENV" | grep -q "\"name\":\"$key\""; then ok "proxy pod env $key present"; else bad "proxy pod env $key missing"; fi
done
check "proxy pod runs the metering image" test "$(cat "$LOG_DIR/proxy-image.txt")" = "$PROXY_IMG"
if K -n "$NS" get pod "$LOOP-proxy" -o jsonpath='{.spec.volumes}' 2>/dev/null | grep -q "proxy-usage"; then
  ok "proxy pod has the proxy-usage volume"
else
  bad "proxy pod missing the proxy-usage volume"
fi

# 4b. The <loop>-proxy NetworkPolicy: the usage-port (9090) ingress for the
#     operator namespace / controller-manager + the model egress rule. (The
#     netpol is created in the same reconcile as the proxy pod; a brief retry
#     avoids a race if the operator's netpol write lags the pod readiness.)
NP_JSON="{}"
for _ in 1 2 3 4 5 6 7 8; do
  NP_JSON=$(K -n "$NS" get networkpolicy "$LOOP-proxy-netpol" -o json 2>/dev/null || echo "{}")
  if echo "$NP_JSON" | grep -q '"9090"\|9090' 2>/dev/null; then break; fi
  sleep 2
done
echo "$NP_JSON" > "$LOG_DIR/netpol.json" 2>/dev/null || true
# A simpler, robust check: the netpol JSON must contain the 9090 port AND the
# operator namespace selector AND the controller-manager pod selector.
if echo "$NP_JSON" | grep -q '9090' 2>/dev/null \
   && echo "$NP_JSON" | grep -q "kubernetes.io/metadata.name.*$E2E_NS\|$E2E_NS.*kubernetes.io/metadata.name" 2>/dev/null \
   && echo "$NP_JSON" | grep -q 'controller-manager' 2>/dev/null; then
  ok "proxy netpol has the usage-port (9090) ingress for the operator/controller-manager"
else
  bad "proxy netpol missing the usage-port (9090) operator ingress"
fi
if echo "$NP_JSON" | grep -q "ipBlock"; then
  ok "proxy netpol has the model egress rule (ipBlock)"
else
  bad "proxy netpol missing the model egress rule"
fi

# 4c. The metering proxy DIALS SUCCESS: drive the agent-shaped request to the
#     proxy (the runner POSTs $COX_MODEL_BASE_URL/v1/chat/completions; the proxy
#     service:8080). An in-cluster curl pod (the agent's network position) does
#     the POST.
K apply -f - >/dev/null <<EOF
apiVersion: v1
kind: Pod
metadata:
  name: p2b-curler
  namespace: $NS
  labels: {app.kubernetes.io/component: agent, coxswain.io/loop: $LOOP}
spec:
  containers:
  - name: curler
    image: $CURL_IMG
    command: ["python","-c","import time; time.sleep(600)"]
    imagePullPolicy: IfNotPresent
EOF
K -n "$NS" wait --for=condition=Ready pod/p2b-curler --timeout=120s 2>/dev/null || true
sleep 2
HTTP_CODE=$(K -n "$NS" exec p2b-curler -- python -c "
import urllib.request,json,socket
socket.setdefaulttimeout(10)
req=urllib.request.Request('http://$LOOP-proxy:8080/v1/chat/completions',
  data=json.dumps({'model':'fake','messages':[{'role':'user','content':'hi'}]}).encode(),
  headers={'Content-Type':'application/json','Accept-Encoding':'gzip'})
try:
    r=urllib.request.urlopen(req)
    open('/tmp/resp.json','wb').write(r.read())
    print(r.status)
except urllib.error.HTTPError as e:
    open('/tmp/resp.json','wb').write(e.read())
    print(e.code)
except Exception as e:
    print(0)
" 2>/dev/null || echo "000")
K -n "$NS" exec p2b-curler -- cat /tmp/resp.json 2>/dev/null > "$LOG_DIR/agent-resp.json" || true
echo "   agent request via proxy -> HTTP $HTTP_CODE"
[ "$HTTP_CODE" = "200" ] && ok "dial SUCCESS: agent request via the metering proxy returns 2xx from the fake model" || bad "dial: agent request via the proxy returned HTTP $HTTP_CODE (expected 200)"

# 4d. The fake model saw the injected credential (the proxy read it from the
#     mounted Secret file and injected the Authorization header).
FAKE_POD_LOG=$(K -n "$NS" logs "$FAKE_POD" 2>/dev/null || true)
if echo "$FAKE_POD_LOG" | grep -q "Bearer $TEST_KEY"; then
  ok "the fake model saw the injected credential (Bearer <dummy key>) — the proxy read it from the mounted Secret file"
else
  bad "the fake model did NOT see the injected credential (Authorization header)"
  echo "     fake model log tail:"; echo "$FAKE_POD_LOG" | tail -10 | sed 's/^/       /'
fi

# The CNI decides whether the netpol dial checks can be asserted: an enforcing
# CNI (Calico) polices pod->pod, so the netpol is enforced; kindnet does not,
# so on kindnet the netpol dial checks report NOT RUN (never FAIL) — the netpol
# spec assertion (4b) is the property kindnet does prove.
CNI_NAME=$(K get pods -n kube-system -o jsonpath='{.items[*].metadata.name}' 2>/dev/null | tr ' ' '\n' | grep -iE 'calico|cilium' | head -1 || true)
ENFORCING_CNI=0; case "$CNI_NAME" in *calico*|*cilium*) ENFORCING_CNI=1;; esac
echo "   CNI: ${CNI_NAME:-<none found>}; enforcing netpol: $ENFORCING_CNI"

# The proxy pod IP (the dial target for the real pod-network netpol checks).
PROXY_POD_IP=$(K -n "$NS" get pod "$LOOP-proxy" -o jsonpath='{.status.podIP}' 2>/dev/null || echo "")
echo "   proxy pod IP: $PROXY_POD_IP"

# 4e. The operator reads the metering usage endpoint via a REAL pod-network
# dial: a short-lived probe pod in the OPERATOR namespace (coxswain-system) with
# the controller-manager labels dials http://<proxy-pod-ip>:9090/coxswain/usage.
# A pod-network dial (unlike kubectl port-forward, which goes through the
# kubelet and BYPASSES NetworkPolicy) respects the netpol. (curlimages/curl is
# not always present; the fake-model python is, so the probe reuses it.)
K apply -f - >/dev/null <<EOF
apiVersion: v1
kind: Pod
metadata:
  name: p2b-usage-probe
  namespace: $E2E_NS
  labels:
    control-plane: controller-manager
    app.kubernetes.io/name: coxswain
spec:
  containers:
  - name: probe
    image: $FAKE_IMG
    imagePullPolicy: IfNotPresent
    command: ["python","-c","import time; time.sleep(600)"]
EOF
K -n "$E2E_NS" wait --for=condition=Ready pod/p2b-usage-probe --timeout=120s 2>/dev/null || true
sleep 2
USAGE=$(K -n "$E2E_NS" exec p2b-usage-probe -- python -c "
import urllib.request,socket
socket.setdefaulttimeout(5)
try:
    print(urllib.request.urlopen('http://$PROXY_POD_IP:9090/coxswain/usage').read().decode(),end='')
except Exception as e:
    print('DIAL_FAILED:'+str(e),end='')
" 2>/dev/null || echo "DIAL_FAILED:exec")
echo "   usage endpoint: $USAGE"
echo "$USAGE" > "$LOG_DIR/usage.json"
if echo "$USAGE" | grep -q "^{"; then
  ok "operator-namespace/controller-manager pod reads the usage endpoint (real pod-network dial, HTTP 200)"
else
  bad "operator-namespace/controller-manager pod could NOT read the usage endpoint: $USAGE"
fi
REQS=$(echo "$USAGE" | python3 -c 'import json,sys; print(json.load(sys.stdin).get("requests",0))' 2>/dev/null || echo 0)
TOKS=$(echo "$USAGE" | python3 -c 'import json,sys; d=json.load(sys.stdin); print(d.get("promptTokens",0)+d.get("completionTokens",0))' 2>/dev/null || echo 0)
BOOT=$(echo "$USAGE" | python3 -c 'import json,sys; print(json.load(sys.stdin).get("bootID",""))' 2>/dev/null)
UNMETERED=$(echo "$USAGE" | python3 -c 'import json,sys; print(json.load(sys.stdin).get("unmeteredRequests",0))' 2>/dev/null || echo 0)
[ "${REQS:-0}" -ge 1 ] && ok "usage endpoint shows a dial (requests=$REQS)" || bad "usage endpoint shows no dial (requests=$REQS)"
[ "${TOKS:-0}" -ge 1 ] && ok "usage endpoint shows prompt+completion tokens ($TOKS)" || bad "usage endpoint shows no tokens"
[ -n "$BOOT" ] && ok "usage endpoint shows a bootID ($BOOT)" || bad "usage endpoint shows no bootID"
[ "$UNMETERED" = "0" ] && ok "the success dial was not counted unmetered (unmeteredRequests=0)" || bad "success dial counted unmetered ($UNMETERED)"

# 4e-netpol. REAL pod-network dials of the usage-port netpol (the plan's netpol
# assertion). Enforcing CNI only: on kindnet these are NOT RUN (never FAIL) —
# kindnet does not police pod->pod, so a blocked dial cannot be observed (and an
# allowed dial proves nothing). The netpol SPEC is asserted in 4b (the property
# kindnet does prove).
dial_usage() { # dial_usage <ns> <pod>
  K -n "$1" exec "$2" -- python -c "
import urllib.request,socket
socket.setdefaulttimeout(5)
try:
    urllib.request.urlopen('http://$PROXY_POD_IP:9090/coxswain/usage')
    print('OK',end='')
except Exception as e:
    print('BLOCKED:'+str(e),end='')
" 2>/dev/null || echo "DIAL_FAILED:exec"
}
if [ "$ENFORCING_CNI" = "1" ]; then
  # (b) the agent (a pod with the Loop agent labels in the Loop namespace) dials
  # the usage endpoint -> BLOCKED (the agent has no SA token; the netpol only
  # allows the operator's controller-manager).
  K apply -f - >/dev/null <<EOF
apiVersion: v1
kind: Pod
metadata:
  name: p2b-agent-probe
  namespace: $NS
  labels: {app.kubernetes.io/component: agent, coxswain.io/loop: $LOOP}
spec:
  containers:
  - name: probe
    image: $FAKE_IMG
    imagePullPolicy: IfNotPresent
    command: ["python","-c","import time; time.sleep(600)"]
EOF
  K -n "$NS" wait --for=condition=Ready pod/p2b-agent-probe --timeout=120s 2>/dev/null || true
  sleep 2
  AGENT_DIAL=$(dial_usage "$NS" p2b-agent-probe)
  if echo "$AGENT_DIAL" | grep -q "^OK"; then
    bad "netpol: the agent pod COULD read the usage endpoint ($AGENT_DIAL) — should be BLOCKED"
  else
    ok "netpol: the agent pod is BLOCKED from the usage endpoint ($AGENT_DIAL)"
  fi
  # (c) a pod in the operator namespace WITHOUT the controller-manager labels
  # dials the usage endpoint -> BLOCKED (proves the podSelector half: the
  # namespaceSelector alone is not enough; the control-plane label is required).
  K apply -f - >/dev/null <<EOF
apiVersion: v1
kind: Pod
metadata:
  name: p2b-nolabel-probe
  namespace: $E2E_NS
  labels: {app.kubernetes.io/name: coxswain}
spec:
  containers:
  - name: probe
    image: $FAKE_IMG
    imagePullPolicy: IfNotPresent
    command: ["python","-c","import time; time.sleep(600)"]
EOF
  K -n "$E2E_NS" wait --for=condition=Ready pod/p2b-nolabel-probe --timeout=120s 2>/dev/null || true
  sleep 2
  NOLEBEL_DIAL=$(dial_usage "$E2E_NS" p2b-nolabel-probe)
  if echo "$NOLEBEL_DIAL" | grep -q "^OK"; then
    bad "netpol: a non-controller-manager pod in the operator namespace COULD read the usage endpoint ($NOLEBEL_DIAL) — should be BLOCKED (podSelector half)"
  else
    ok "netpol: a non-controller-manager pod in the operator namespace is BLOCKED ($NOLEBEL_DIAL) — the podSelector half is enforced"
  fi
else
  echo "   [NOT RUN] netpol dial checks (b agent blocked / c podSelector) — the CNI ($CNI_NAME) does not police pod->pod; the netpol SPEC is asserted in 4b instead (kindnet cannot prove enforcement)"
  K -n "$NS" delete pod p2b-agent-probe --ignore-not-found >/dev/null 2>&1 || true
  K -n "$E2E_NS" delete pod p2b-nolabel-probe --ignore-not-found >/dev/null 2>&1 || true
fi

# 4f. The metering proxy's audit log (one JSON line per metered request) was
#     captured (tee'd to the proxy pod log).
PROXY_LOG=$(K -n "$NS" logs "$LOOP-proxy" 2>/dev/null || true)
echo "$PROXY_LOG" > "$LOG_DIR/proxy.log"
if echo "$PROXY_LOG" | grep -q '"action":"usage"' && echo "$PROXY_LOG" | grep -q 'promptTokens'; then
  ok "the metering proxy audit line (a metered POST) is in the proxy pod log"
else
  bad "the metering proxy audit line is not in the proxy pod log"
fi

# 4g. Image digests (the operator, the metering proxy, the fake model) recorded.
echo "   image digests:"
echo "     operator: $(cat "$LOG_DIR/operator-imageID.txt")"
echo "     proxy:    $(cat "$LOG_DIR/proxy-imageID.txt")"
echo "     fake:     $(K -n "$NS" get pod "$FAKE_POD" -o jsonpath='{.status.containerStatuses[0].imageID}' 2>/dev/null || echo '')"

# ===========================================================================
# Summary.
# ===========================================================================
echo
echo "===================================================================="
echo " P2b kind acceptance: $PASS passed, $FAIL failed"
echo " logs: $LOG_DIR (operator.log, proxy.log, usage.json, *-imageID.txt)"
echo "===================================================================="
trap - EXIT   # keep the log dir for inspection (the validation section wants the logs recorded)
if [ "$FAIL" -gt 0 ]; then
  echo "   (log dir preserved at $LOG_DIR)"
  exit 1
fi
echo "   (log dir preserved at $LOG_DIR)"
exit 0
