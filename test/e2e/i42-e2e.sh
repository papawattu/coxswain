#!/usr/bin/env bash
# Full I42 acceptance (docs/TDD-PLAN-PHASE1.md, "Full I42 acceptance"): a
# repeatable kind e2e that builds + loads the images, deploys the controller,
# creates a Loop with a network-allowing AgentPolicy + a model endpoint, and
# asserts EVERY property in that paragraph.
#
# Pinned by the caller: K8S_CONTEXT=kind-coxswain-dev (and CLUSTER=coxswain-dev
# for the kind load). The controller runs the DEV manifest (--allow-unenforced)
# so Loops actually run; the fence properties being asserted (NetworkPolicy,
# the egress proxy application layer, CRD CEL validation, KubeArmor policy
# objects, the KubeArmor exec block on the throwaway pod) are all independent
# of the D30 enforcement gate.
#
# Assertions (all must pass; the script exits non-zero on any failure):
#   1  the egress proxy pod is Ready
#   2  the agent's HTTPS_PROXY is set (to the egress proxy svc)
#   3  an allowed host via the proxy succeeds (audit records it as allowed)
#   4  a disallowed host gets 403 (audit records it as blocked)
#   5  a raw TCP connection to a cluster-internal POD IP is blocked
#      (agent NetworkPolicy; the D38 carve-out is host-NETWORK endpoints only)
#   6  a direct-IP connection to a disallowed external host fails
#      (no raw egress: the agent NetworkPolicy only permits the proxies + DNS)
#   7  a DNS-rebinding allow (an allowed name resolving to a private pod IP)
#      is rejected by the egress proxy (403), and the audit blocked record's
#      detail carries the resolved private IP. Simulated via a CoreDNS hosts
#      override (split-horizon) — the ConfigMap is restored on exit.
#   8  a .svc allow is rejected at validation (CRD CEL rule: the API server
#      rejects the AgentPolicy)
#   9  the KubeArmor policies exist for both proxies (egress + model)
#   10 a disallowed exec in a pod selected by the egress-proxy KubeArmor
#      policy gets Permission denied (throwaway busybox pod carrying the
#      egress-proxy labels; the real egress-proxy image is distroless)
#
# KNOWN LIMITATIONS (printed, not passed):
#   L1  on kindnet the apiserver/kubelet HOST-NETWORK endpoints are reachable
#       from the agent pod (D38 / R16); only pod-IP and external-IP egress is
#       policed by the NetworkPolicy.
#   L2  ephemeral kubectl debug containers are not policed by KubeArmor, so
#       the exec block is proven on a dedicated throwaway pod with the
#       egress-proxy labels, not via an ephemeral container.
#
# House rules honored: KubeArmor is NEVER restarted; no rm -rf (temp files are
# left in place); no python heredocs inside kubectl exec sh -c (probe scripts
# are written as files and kubectl cp'd); the throwaway pod is removed with
# kubectl delete (also on exit via a trap); the CoreDNS ConfigMap is restored
# on exit via a trap.
#
# Requires: docker, kind, kubectl, jq, curl on the host; the kind cluster
# with agent-sandbox + KubeArmor already installed (make kind-up).

set -uo pipefail

# kind lives in GOPATH/bin on this host; make it findable.
GOPATH_BIN="$(go env GOPATH 2>/dev/null)/bin"
case ":$PATH:" in *":$GOPATH_BIN:"*) ;; *) PATH="$GOPATH_BIN:$PATH"; export PATH ;; esac

CTX="${K8S_CONTEXT:-kind-coxswain-dev}"
CLUSTER="${KIND_CLUSTER_NAME:-coxswain-dev}"
NS=i42-e2e
LOOP=i42-loop
E2E_NS=coxswain-system
REPO_ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
COMMIT="$(git -C "$REPO_ROOT" rev-parse --short HEAD 2>/dev/null || echo unknown)"
IMG_TAG="main-${COMMIT}"
IMG="coxswain-controller:${IMG_TAG}"
PROXY_IMG="coxswain-proxy:standin"
EGRESS_IMG="coxswain-egress-proxy:standin"
AGENT_IMG="golang:1.26"
BUSYBOX_IMG="busybox:1.36"
TMPDIR="${TMPDIR:-/tmp}/i42-e2e.$$"
mkdir -p "$TMPDIR"

K() { kubectl --context "$CTX" "$@"; }
FAIL=0
ok()  { echo "   PASS: $*"; }
bad() { echo "   FAIL: $*"; FAIL=1; }

echo "=== Full I42 acceptance (context=$CTX cluster=$CLUSTER ns=$NS loop=$LOOP) ==="
echo "commit: $COMMIT  image: $IMG"

# --- throwaway pod + CoreDNS ConfigMap cleanup (rule: kubectl delete, no force)
THROWAWAY_POD=""
COREDNS_CM_BACKUP="$TMPDIR/coredns-configmap-backup.json"
cleanup() {
  # restore the CoreDNS ConfigMap (the DNS-rebinding hosts override is reverted)
  if [ -f "$COREDNS_CM_BACKUP" ]; then
    echo "--- restoring kube-system/coredns ConfigMap (reverting the DNS-rebinding hosts override) ---"
    # Extract the original Corefile from the backup and patch the current object.
    # (apply/replace fail on resourceVersion conflict; patch only updates the
    # data field, which is what we changed.)
    ORIG_COREFILE=$(jq -r '.data.Corefile' "$COREDNS_CM_BACKUP" 2>/dev/null)
    if [ -n "$ORIG_COREFILE" ]; then
      if kubectl --context "$CTX" -n kube-system get cm coredns -o json \
        | jq --arg cf "$ORIG_COREFILE" '.data.Corefile = $cf' \
        | kubectl --context "$CTX" replace -f - 2>/dev/null; then
        echo "   ConfigMap restored"
      else
        echo "   (could not restore the coredns ConfigMap; check manually)"
      fi
    else
      echo "   (could not read the original Corefile from the backup; check manually)"
    fi
    # restart coredns so it picks up the restored config (NOT KubeArmor)
    kubectl --context "$CTX" -n kube-system rollout restart deploy/coredns 2>/dev/null || true
    kubectl --context "$CTX" -n kube-system rollout status deploy/coredns --timeout=180s 2>/dev/null || true
    echo "   coredns restored."
  fi
  # delete the throwaway exec pod
  if [ -n "$THROWAWAY_POD" ]; then
    echo "--- cleaning up throwaway pod $THROWAWAY_POD (kubectl delete) ---"
    K -n "$NS" delete pod "$THROWAWAY_POD" --wait=false 2>/dev/null || true
    for i in $(seq 1 12); do
      K -n "$NS" get pod "$THROWAWAY_POD" >/dev/null 2>&1 || return 0
      sleep 2
    done
    echo "   (throwaway pod $THROWAWAY_POD still terminating; left for the cluster to finish)"
  fi
}
trap cleanup EXIT

echo
echo "--- STEP 0: preflight (cluster + KubeArmor present; NOT restarting KubeArmor) ---"
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
echo "   building $PROXY_IMG ..."
(cd "$REPO_ROOT" && docker build -q -t "$PROXY_IMG" -f cmd/proxy-standin/Dockerfile .) || { echo "FATAL: proxy build failed"; exit 2; }
echo "   building $EGRESS_IMG ..."
(cd "$REPO_ROOT" && docker build -q -t "$EGRESS_IMG" -f cmd/egress-proxy/Dockerfile .) || { echo "FATAL: egress-proxy build failed"; exit 2; }
IMG_DIGEST="$(docker image inspect "$IMG" --format '{{.Id}}' 2>/dev/null)"
EGRESS_DIGEST="$(docker image inspect "$EGRESS_IMG" --format '{{.Id}}' 2>/dev/null)"
PROXY_DIGEST="$(docker image inspect "$PROXY_IMG" --format '{{.Id}}' 2>/dev/null)"
echo "   controller image digest: $IMG_DIGEST"
echo "   egress-proxy image digest: $EGRESS_DIGEST"
echo "   model-proxy image digest: $PROXY_DIGEST"
for img in "$IMG" "$PROXY_IMG" "$EGRESS_IMG" "$BUSYBOX_IMG" "$AGENT_IMG"; do
  echo "   kind load: $img"
  kind load docker-image "$img" --name "$CLUSTER" || { echo "FATAL: kind load $img failed"; exit 2; }
done

echo
echo "--- STEP 2: deploy controller (dev overlay: --allow-unenforced) ---"
make -C "$REPO_ROOT" kustomize >/dev/null 2>&1
KUSTOMIZE_BIN="$REPO_ROOT/bin/kustomize"
(cd "$REPO_ROOT/config/manager" && "$KUSTOMIZE_BIN" edit set image controller="$IMG")
(cd "$REPO_ROOT" && "$KUSTOMIZE_BIN" build config/dev | K apply -f -) \
  || { echo "FATAL: controller deploy failed"; exit 2; }
K -n "$E2E_NS" rollout status deploy/coxswain-controller-manager --timeout=180s || { echo "FATAL: controller not ready"; exit 2; }
RUNNING_IMG_ID=$(K -n "$E2E_NS" get pods -l control-plane=controller-manager -o jsonpath='{.items[0].status.containerStatuses[0].imageID}' 2>/dev/null)
echo "   controller running, imageID: $RUNNING_IMG_ID"

# ===========================================================================
# STEP 3a: pick a cluster-internal pod IP for the DNS-rebinding check.
#          We use the first non-coreDNS pod in the cluster (or coredns itself
#          if nothing else is running yet). The IP is used both as the target
#          for check 5 (raw TCP block) and as the resolved IP for check 7.
# ===========================================================================
echo
echo "--- STEP 3a: pick a cluster-internal pod IP (for checks 5 and 7) ---"
# Prefer a non-system pod; fall back to coredns.
TEST_IP=$(K get pods -A -o json 2>/dev/null \
  | jq -r '.items[] | select(.metadata.namespace != "kube-system" and .metadata.namespace != "kubearmor" and .metadata.namespace != "agent-sandbox-system" and .metadata.namespace != "coxswain-system") | .status.podIP' 2>/dev/null \
  | head -1)
if [ -z "$TEST_IP" ]; then
  TEST_IP=$(K -n kube-system get pods -l k8s-app=kube-dns -o jsonpath='{.items[0].status.podIP}' 2>/dev/null)
fi
if [ -z "$TEST_IP" ]; then
  echo "FATAL: could not find any pod IP to use as the cluster-internal target"
  exit 2
fi
echo "   cluster-internal pod IP for checks 5+7: $TEST_IP"

# The DNS-rebinding name (allowed by the AgentPolicy, resolves to TEST_IP via
# the CoreDNS hosts override in step 3d).
REBIND_NAME="i42-rebind.example.test"
REBIND_ALLOW="${REBIND_NAME}:80"

# ===========================================================================
# STEP 3b: create the fixture (namespace, AgentPolicy, fake model, Loop).
#          The AgentPolicy includes the rebinding name so the egress proxy's
#          allow check passes; the CoreDNS hosts override (step 3d) makes it
#          resolve to the private pod IP, triggering the resolved-IP carve-out.
# ===========================================================================
echo
echo "--- STEP 3b: create the fixture (namespace, AgentPolicy, fake model, Loop) ---"
# Clean up any old Loop from a previous run (the controller won't change the
# pod image in-place; we need a fresh sandbox with the new agent image).
if K -n "$NS" get loop "$LOOP" >/dev/null 2>&1; then
  echo "   deleting old Loop $LOOP (will be recreated with the current agent image)"
  K -n "$NS" delete loop "$LOOP" --wait=false 2>/dev/null || true
  # Wait for the old pods to go away.
  for i in $(seq 1 20); do
    K -n "$NS" get pods -l "app.kubernetes.io/component=agent" --no-headers 2>/dev/null | grep -q . || break
    sleep 3
  done
  K -n "$NS" get pods -l "app.kubernetes.io/component=agent" --no-headers 2>/dev/null | grep -q . && \
    echo "   (old agent pod still terminating; continuing)"
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
    - ${REBIND_ALLOW}
EOF
K apply -f "$TMPDIR/agentpolicy.yaml" >/dev/null

echo "   creating throwaway fake-model pod + Service (model endpoint target) ..."
cat > "$TMPDIR/fake-model.yaml" <<EOF
apiVersion: v1
kind: Pod
metadata:
  name: fake-model
  namespace: ${NS}
spec:
  containers:
    - name: fake-model
      image: ${AGENT_IMG}
      command: ["sh", "-c", "sleep 3600"]
---
apiVersion: v1
kind: Service
metadata:
  name: model-endpoint
  namespace: ${NS}
spec:
  selector:
    name: fake-model
  ports:
    - name: http
      port: 8000
      targetPort: 80
EOF
K apply -f "$TMPDIR/fake-model.yaml" >/dev/null
cat > "$TMPDIR/model-secret.yaml" <<EOF
apiVersion: v1
kind: Secret
metadata:
  name: ${LOOP}-model
  namespace: ${NS}
type: Opaque
data:
  MODEL_BASE_URL: $(echo -n "http://model-endpoint.${NS}.svc.cluster.local:8000" | base64 -w0)
  MODEL_API_KEY: $(echo -n "k" | base64 -w0)
EOF
K apply -f "$TMPDIR/model-secret.yaml" >/dev/null

cat > "$TMPDIR/loop.yaml" <<EOF
apiVersion: coxswain.wattu.com/v1alpha1
kind: Loop
metadata:
  name: ${LOOP}
  namespace: ${NS}
spec:
  goal: "Full I42 acceptance (e2e): egress proxy + network fence properties"
  policyRefs:
    - ${LOOP}-pol
  agent:
    image: ${AGENT_IMG}
    model: local-model
    modelEndpoint: "model-endpoint:8000"
    endpointSecretRef: ${LOOP}-model
  workspace:
    repo: https://github.com/papawattu/coxswain
    ref: main
  loop:
    maxIterations: 1
EOF
K apply -f "$TMPDIR/loop.yaml" >/dev/null
echo "   fixtures applied: AgentPolicy ${LOOP}-pol (allows: proxy.golang.org:80, ${REBIND_ALLOW}), Loop ${LOOP}, fake-model svc"

echo
echo "--- STEP 3c: wait for the egress proxy pod to be Ready ---"
READY=""
for i in $(seq 1 60); do
  READYC=$(K -n "$NS" get pod "${LOOP}-egress-proxy" -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}' 2>/dev/null)
  if [ "$READYC" = "True" ]; then READY=yes; break; fi
  sleep 5
done
echo "   egress proxy pod Ready after ~$((i*5))s"
PROXY_IP=$(K -n "$NS" get pod "${LOOP}-egress-proxy" -o jsonpath='{.status.podIP}' 2>/dev/null)
echo "   egress proxy pod IP: $PROXY_IP"

# ===========================================================================
# STEP 3d: CoreDNS hosts override for the DNS-rebinding check.
#          Save the current coredns ConfigMap, add a hosts block mapping
#          i42-rebind.example.test -> $TEST_IP, apply it, and restart coredns.
#          The trap restores the ConfigMap on exit.
# ===========================================================================
echo
echo "--- STEP 3d: CoreDNS hosts override (${REBIND_NAME} -> ${TEST_IP}) ---"
# Save the original ConfigMap (for the trap restore).
K -n kube-system get cm coredns -o json > "$COREDNS_CM_BACKUP" || { echo "FATAL: could not read coredns ConfigMap"; exit 2; }
echo "   saved coredns ConfigMap to $COREDNS_CM_BACKUP"

# Build the modified Corefile: add a hosts block after the first line (the .:53 header).
COREFILE=$(K -n kube-system get cm coredns -o jsonpath='{.data.Corefile}')
# The hosts block: "hosts { fallthrough <name> <ip> }"
HOSTS_LINE="hosts { fallthrough ${REBIND_NAME} ${TEST_IP} }"
NEW_COREFILE=$(echo "$COREFILE" | awk -v hline="    ${HOSTS_LINE}" 'NR==1 {print; print hline; next} {print}')
# Verify the hosts line was inserted.
if ! echo "$NEW_COREFILE" | grep -q "$REBIND_NAME"; then
  echo "FATAL: could not insert the hosts block into the Corefile (sed failed)"
  exit 2
fi
echo "   modified Corefile (first 5 lines):"
echo "$NEW_COREFILE" | head -5 | sed 's/^/     /'

# Apply the modified ConfigMap.
K -n kube-system get cm coredns -o json | jq --arg corefile "$NEW_COREFILE" '.data.Corefile = $corefile' | kubectl --context "$CTX" apply -f - >/dev/null || { echo "FATAL: could not apply modified coredns ConfigMap"; exit 2; }
echo "   coredns ConfigMap updated"

# Restart coredns so it picks up the new config (this is NOT KubeArmor).
kubectl --context "$CTX" -n kube-system rollout restart deploy/coredns 2>/dev/null || true
kubectl --context "$CTX" -n kube-system rollout status deploy/coredns --timeout=180s 2>/dev/null || { echo "FATAL: coredns did not become ready after restart"; exit 2; }
echo "   coredns restarted"
# Wait for the agent pod to be Ready (it was just recreated from the Loop
# deletion in step 3b). Then wait for DNS to settle: the agent pod's DNS
# resolver may be stale after the coredns restart. Poll from the agent pod
# until the proxy FQDN resolves.
AGENT_POD_3D=$(K -n "$NS" get pods -l "app.kubernetes.io/component=agent,coxswain.io/loop=$LOOP" -o jsonpath='{.items[0].metadata.name}' 2>/dev/null)
if [ -n "$AGENT_POD_3D" ]; then
  echo "   waiting for agent pod $AGENT_POD_3D to be Ready..."
  for i in $(seq 1 30); do
    R=$(K -n "$NS" get pod "$AGENT_POD_3D" -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}' 2>/dev/null)
    [ "$R" = "True" ] && { echo "   agent pod Ready after ~$((i*3))s"; break; }
    sleep 3
  done
  echo "   waiting for DNS to settle (curl --proxy can resolve the proxy FQDN)..."
  for i in $(seq 1 20); do
    # Use curl --proxy (not direct URL) for the DNS check: the checks use
    # curl --proxy, which resolves the proxy FQDN via a different code path
    # than direct URL resolution. Poll with the same tool+mode.
    if K -n "$NS" exec "$AGENT_POD_3D" -- sh -c "curl -s -o /dev/null --max-time 5 --proxy http://${PROXY_IP}:3128 http://example.com/" 2>/dev/null; then
      echo "   DNS settled (curl --proxy via pod IP reached the proxy) after ~$((i*3))s"
      break
    fi
    sleep 3
  done
  # Extra settle time: the coredns rollout may still be settling DNS
  # responses. Wait 15s to be safe.
  echo "   waiting 15s for DNS to fully settle..."
  sleep 15
fi

# Verify the override works: the egress proxy pod should now resolve the name.
# (We don't exec into the proxy — it's distroless. Instead we verify via the
# agent pod, which resolves via the same coredns.)
AGENT_POD_TMP=$(K -n "$NS" get pods -l "app.kubernetes.io/component=agent,coxswain.io/loop=$LOOP" -o jsonpath='{.items[0].metadata.name}' 2>/dev/null)
if [ -n "$AGENT_POD_TMP" ]; then
  RESOLVED=$(K -n "$NS" exec "$AGENT_POD_TMP" -- sh -c "getent hosts ${REBIND_NAME} 2>&1 || echo 'no-resolver'" 2>&1 || echo "no-exec")
  echo "   resolution check from agent pod: $(echo "$RESOLVED" | head -3 | tr '\n' ' ')"
fi

# ===========================================================================
# CHECK 1: the egress proxy pod is Ready.
# ===========================================================================
echo
echo "--- CHECK 1: egress proxy pod is Ready ---"
K -n "$NS" get pod "${LOOP}-egress-proxy"
if [ "$READY" = "yes" ]; then
  ok "egress proxy pod ${LOOP}-egress-proxy is Ready"
else
  bad "egress proxy pod ${LOOP}-egress-proxy is NOT Ready"
fi

# ===========================================================================
# CHECK 2: the agent's HTTPS_PROXY is set to the egress proxy service.
# ===========================================================================
echo
echo "--- CHECK 2: agent HTTPS_PROXY is set ---"
AGENT_POD=$(K -n "$NS" get pods -l "app.kubernetes.io/component=agent,coxswain.io/loop=$LOOP" -o jsonpath='{.items[0].metadata.name}' 2>/dev/null)
[ -z "$AGENT_POD" ] && { bad "no agent pod found"; }
WANT_PROXY="http://${LOOP}-egress-proxy.${NS}.svc.cluster.local:3128"
GOT_PROXY=$(K -n "$NS" get pod "$AGENT_POD" -o jsonpath='{.spec.containers[0].env[?(@.name=="HTTPS_PROXY")].value}' 2>/dev/null)
echo "   HTTPS_PROXY=$GOT_PROXY (expected $WANT_PROXY)"
if [ "$GOT_PROXY" = "$WANT_PROXY" ]; then
  ok "HTTPS_PROXY is set to the egress proxy svc ($GOT_PROXY)"
else
  bad "HTTPS_PROXY='$GOT_PROXY', expected '$WANT_PROXY'"
fi
NO_PROXY=$(K -n "$NS" get pod "$AGENT_POD" -o jsonpath='{.spec.containers[0].env[?(@.name=="NO_PROXY")].value}' 2>/dev/null)
case "$NO_PROXY" in
  *"$LOOP-proxy.$NS.svc"*) ok "NO_PROXY contains the model proxy svc ($NO_PROXY)" ;;
  *) bad "NO_PROXY='$NO_PROXY' does not contain the model proxy svc" ;;
esac

# ===========================================================================
# CHECK 3: an allowed host via the proxy succeeds; audit records it.
#          (proxy.golang.org:80 is in the allows; the egress proxy port-matches
#          so plain HTTP on port 80 is permitted.)
# ===========================================================================
echo
echo "--- CHECK 3: allowed host via the proxy succeeds (audit: allowed) ---"
EGRESS_POD="$LOOP-egress-proxy"
PROXY_FQDN="${LOOP}-egress-proxy.${NS}.svc.cluster.local"
cat > "$TMPDIR/probe-allowed.sh" <<EOF
#!/bin/sh
echo "-- HTTP proxy.golang.org (via explicit proxy) --"
curl -s -o /dev/null -w "%{http_code}" --max-time 30 --retry 2 --retry-delay 3 \
  --proxy http://${PROXY_IP}:3128 http://proxy.golang.org/
ec=\$?
echo ""
echo "curl-exit=\$ec"
EOF
K -n "$NS" cp "$TMPDIR/probe-allowed.sh" "$AGENT_POD:/tmp/probe-allowed.sh" 2>/dev/null || bad "kubectl cp probe-allowed.sh failed"
P3=$(K -n "$NS" exec "$AGENT_POD" -- sh /tmp/probe-allowed.sh 2>&1)
echo "$P3"
case "$P3" in
  *"200"*) ok "HTTP proxy.golang.org via the proxy succeeded (curl HTTP 200)" ;;
  *"curl-exit=0"*) ok "HTTP proxy.golang.org via the proxy succeeded (curl exit 0)" ;;
  *) bad "HTTP proxy.golang.org via the proxy did NOT succeed (output: $P3)" ;;
esac
sleep 2
AUDIT3=$(K -n "$NS" logs "$EGRESS_POD" --tail=10 2>/dev/null)
echo "$AUDIT3" | tail -4 | sed 's/^/     /'
if echo "$AUDIT3" | grep -F '"target":"proxy.golang.org' | tail -3 | grep -q '"verdict":"allowed"'; then
  ok "audit records the allowed proxy.golang.org attempt"
else
  bad "audit has no recent 'verdict:allowed' record for proxy.golang.org"
fi

# ===========================================================================
# CHECK 4: a disallowed host gets 403; audit records it as blocked.
#          (github.com is NOT in the allows.)
# ===========================================================================
echo
echo "--- CHECK 4: disallowed host gets 403 (audit: blocked) ---"
cat > "$TMPDIR/probe-disallowed.sh" <<EOF
#!/bin/sh
echo "-- HTTP github.com (NOT in the allows) via explicit proxy --"
code=$(curl -s -o /dev/null -w "%{http_code}" --max-time 30 --retry 2 --retry-delay 3 \
  --proxy http://${PROXY_IP}:3128 http://github.com/ 2>&1)
ec=$?
echo "curl-exit=$ec http=$code"
if [ "$code" = "403" ]; then
  echo "curl got 403 (blocked as expected)"
elif [ "$code" = "200" ] || [ "$code" = "301" ] || [ "$code" = "302" ]; then
  echo "curl SUCCEEDED (UNEXPECTED for a disallowed host)"
else
  echo "curl got http=$code exit=$ec"
fi
EOF
K -n "$NS" cp "$TMPDIR/probe-disallowed.sh" "$AGENT_POD:/tmp/probe-disallowed.sh" 2>/dev/null || bad "kubectl cp probe-disallowed.sh failed"
P4=$(K -n "$NS" exec "$AGENT_POD" -- sh /tmp/probe-disallowed.sh 2>&1)
echo "$P4"
case "$P4" in
  *"UNEXPECTED"*) bad "disallowed host via the proxy SUCCEEDED (should be 403)" ;;
  *"403 (blocked as expected)"*) ok "disallowed host via the proxy was 403'd (curl HTTP 403)" ;;
  *) bad "unexpected output for disallowed host (output: $P4)" ;;
esac
sleep 2
AUDIT4=$(K -n "$NS" logs "$EGRESS_POD" --tail=10 2>/dev/null)
echo "$AUDIT4" | tail -4 | sed 's/^/     /'
if echo "$AUDIT4" | grep -F '"target":"github.com' | tail -3 | grep -q '"verdict":"blocked"'; then
  ok "audit records the blocked github.com attempt"
else
  bad "audit has no recent 'verdict:blocked' record for github.com"
fi

# ===========================================================================
# CHECK 5: raw TCP to a cluster-internal POD IP is blocked (agent netpol).
# ===========================================================================
echo
echo "--- CHECK 5: raw TCP to cluster-internal pod IP $TEST_IP is blocked ---"
cat > "$TMPDIR/probe-internal.sh" <<EOF
#!/bin/sh
echo "-- raw TCP to internal pod IP $TEST_IP:8080 (no proxy) --"
curl -s -o /dev/null --max-time 12 --noproxy '*' "http://$TEST_IP:8080/"
ec=\$?
echo "raw-internal-exit=\$ec"
case \$ec in
  0) echo "raw-internal SUCCEEDED (UNEXPECTED — should be blocked)" ;;
  127) echo "curl binary not present" ;;
  *) echo "raw-internal did not connect (exit \$ec, blocked as expected)" ;;
esac
EOF
K -n "$NS" cp "$TMPDIR/probe-internal.sh" "$AGENT_POD:/tmp/probe-internal.sh" 2>/dev/null || bad "kubectl cp probe-internal.sh failed"
P5=$(K -n "$NS" exec "$AGENT_POD" -- sh /tmp/probe-internal.sh 2>&1)
echo "$P5"
case "$P5" in
  *"SUCCEEDED"*) bad "raw TCP to internal pod IP $TEST_IP SUCCEEDED (should be blocked)" ;;
  *"did not connect"*) ok "raw TCP to internal pod IP $TEST_IP was blocked (agent NetworkPolicy)" ;;
  *"wget binary not present"*) bad "wget not found in the agent container" ;;
  *) bad "unexpected output for raw internal TCP (output: $P5)" ;;
esac

# ===========================================================================
# CHECK 6: direct-IP connection to a disallowed external host fails.
# ===========================================================================
echo
echo "--- CHECK 6: direct-IP to disallowed external host fails (no raw egress) ---"
GH_IP=$(getent hosts github.com 2>/dev/null | awk '{print $1}' | head -1)
[ -z "$GH_IP" ] && GH_IP=$(curl -s --max-time 10 https://ipinfo.io/github.com 2>/dev/null | grep -o '"ip_addr":"[0-9.]*"' | cut -d'"' -f4)
[ -z "$GH_IP" ] && { bad "could not resolve github.com from the host"; GH_IP="1.1.1.1"; }
echo "   target: github.com -> $GH_IP (direct, no proxy)"
cat > "$TMPDIR/probe-direct.sh" <<EOF
#!/bin/sh
echo "-- raw TCP to disallowed external IP $GH_IP:443 (no proxy) --"
curl -s -o /dev/null --max-time 12 --noproxy '*' -k "https://$GH_IP/"
ec=\$?
echo "raw-external-exit=\$ec"
case \$ec in
  0) echo "raw-external SUCCEEDED (UNEXPECTED — raw egress must be impossible)" ;;
  127) echo "curl binary not present" ;;
  *) echo "raw-external did not connect (exit \$ec, blocked as expected)" ;;
esac
EOF
K -n "$NS" cp "$TMPDIR/probe-direct.sh" "$AGENT_POD:/tmp/probe-direct.sh" 2>/dev/null || bad "kubectl cp probe-direct.sh failed"
P6=$(K -n "$NS" exec "$AGENT_POD" -- sh /tmp/probe-direct.sh 2>&1)
echo "$P6"
case "$P6" in
  *"SUCCEEDED"*) bad "direct-IP to disallowed external $GH_IP SUCCEEDED (raw egress must be impossible)" ;;
  *"did not connect"*) ok "direct-IP to disallowed external $GH_IP failed (no raw egress)" ;;
  *"wget binary not present"*) bad "wget not found in the agent container" ;;
  *) bad "unexpected output for raw external TCP (output: $P6)" ;;
esac

# ===========================================================================
# CHECK 7: DNS-rebinding allow is rejected by the proxy; resolved IP in
#          audit detail.
#          The CoreDNS hosts override (step 3d) makes $REBIND_NAME resolve
#          to $TEST_IP (a private pod IP). The AgentPolicy allows
#          $REBIND_NAME:80, so the allow check passes. But the egress proxy's
#          resolved-IP carve-out rejects the private IP → 403 + audit blocked
#          with the resolved IP in the detail.
# ===========================================================================
echo
echo "--- CHECK 7: DNS-rebinding allow (${REBIND_NAME} -> ${TEST_IP}) rejected by proxy ---"
PROXY_FQDN7="${LOOP}-egress-proxy.${NS}.svc.cluster.local"
cat > "$TMPDIR/probe-rebind.sh" <<EOF
#!/bin/sh
echo "-- HTTP ${REBIND_NAME} (allowed name, resolves to private ${TEST_IP}) via explicit proxy --"
code=\$(curl -s -o /dev/null -w "%{http_code}" --max-time 30 --retry 2 --retry-delay 3 \
  --proxy http://${PROXY_IP}:3128 http://${REBIND_NAME}/ 2>&1)
ec=\$?
echo "curl-exit=\$ec http=\$code"
if [ "\$code" = "403" ]; then
  echo "curl got 403 (blocked by resolved-IP carve-out as expected)"
elif [ "\$code" = "200" ] || [ "\$code" = "301" ] || [ "\$code" = "302" ]; then
  echo "curl SUCCEEDED (UNEXPECTED — the proxy should have blocked the private IP)"
else
  echo "curl got http=\$code exit=\$ec"
fi
EOF
K -n "$NS" cp "$TMPDIR/probe-rebind.sh" "$AGENT_POD:/tmp/probe-rebind.sh" 2>/dev/null || bad "kubectl cp probe-rebind.sh failed"
P7=$(K -n "$NS" exec "$AGENT_POD" -- sh /tmp/probe-rebind.sh 2>&1)
echo "$P7"
case "$P7" in
  *"SUCCEEDED"*) bad "the rebinding name SUCCEEDED via the proxy (the resolved-IP carve-out failed to block $TEST_IP)" ;;
  *"403 (blocked by resolved-IP carve-out as expected)"*) ok "the rebinding name was 403'd by the egress proxy (resolved-IP carve-out blocked $TEST_IP)" ;;
  *) bad "unexpected output for rebinding check (output: $P7)" ;;
esac
sleep 2
AUDIT7=$(K -n "$NS" logs "$EGRESS_POD" --tail=10 2>/dev/null)
echo "$AUDIT7" | tail -4 | sed 's/^/     /'
REBIND_AUDIT=$(echo "$AUDIT7" | grep -F "\"target\":\"${REBIND_NAME}\"" | tail -1)
echo "   audit record for ${REBIND_NAME}: $REBIND_AUDIT"
if [ -z "$REBIND_AUDIT" ]; then
  bad "audit has no record for the rebinding name ${REBIND_NAME}"
else
  if echo "$REBIND_AUDIT" | grep -q '"verdict":"blocked"'; then
    ok "audit records the rebinding attempt as blocked"
  else
    bad "audit does NOT record the rebinding attempt as blocked (got: $REBIND_AUDIT)"
  fi
  # The detail must carry the resolved IP (the egress proxy's
  # resolved-ip-rejected detail now includes the offending IP).
  if echo "$REBIND_AUDIT" | grep -q "ip=${TEST_IP}"; then
    ok "audit detail carries the resolved private IP (ip=${TEST_IP})"
  else
    bad "audit detail does NOT carry the resolved private IP (expected ip=${TEST_IP}, got: $REBIND_AUDIT)"
  fi
fi

# ===========================================================================
# CHECK 8: a .svc allow is rejected at validation (CRD CEL rule).
# ===========================================================================
echo
echo "--- CHECK 8: .svc allow rejected at validation (CRD CEL rule) ---"
REJ_OUT=$(K create -f - 2>&1 <<EOF
apiVersion: coxswain.wattu.com/v1alpha1
kind: AgentPolicy
metadata:
  name: ${LOOP}-svc-reject
  namespace: ${NS}
spec:
  network:
    - my-svc.default.svc:443
EOF
)
echo "   create result: $REJ_OUT"
if echo "$REJ_OUT" | grep -qi "denied\|Invalid\|rejected"; then
  ok "the API server rejected the .svc AgentPolicy at validation (CEL rule)"
else
  bad "the .svc AgentPolicy was NOT rejected at validation (output: $REJ_OUT)"
  K -n "$NS" delete agentpolicy "${LOOP}-svc-reject" --ignore-not-found=true 2>/dev/null || true
fi

# ===========================================================================
# CHECK 9: KubeArmor policies exist for both proxies.
# ===========================================================================
echo
echo "--- CHECK 9: KubeArmor policies exist for the egress + model proxies ---"
for p in "coxswain-${LOOP}-egress-proxy" "coxswain-${LOOP}-proxy"; do
  if K -n "$NS" get kubearmorpolicy "$p" >/dev/null 2>&1; then
    act=$(K -n "$NS" get kubearmorpolicy "$p" -o jsonpath='{.spec.action}' 2>/dev/null)
    echo "   $p exists (spec.action=$act)"
    ok "KubeArmorPolicy $p exists"
  else
    bad "KubeArmorPolicy $p does not exist"
  fi
done

# ===========================================================================
# CHECK 10: a disallowed exec in a pod selected by the egress-proxy
#           KubeArmor policy gets Permission denied.
# ===========================================================================
echo
echo "--- CHECK 10: disallowed exec in a pod selected by the egress-proxy KubeArmor policy ---"
SEL_JSON=$(K -n "$NS" get kubearmorpolicy "coxswain-${LOOP}-egress-proxy" -o jsonpath='{.spec.selector.matchLabels}' 2>/dev/null)
echo "   egress-proxy KubeArmorPolicy selector: $SEL_JSON"
if [ -z "$SEL_JSON" ]; then
  bad "could not read the egress-proxy KubeArmorPolicy selector"
else
  THROWAWAY_POD="i42-e2e-exec"
  SEL_LABELS=$(echo "$SEL_JSON" | jq -r 'to_entries | map(.key + "=" + .value) | join(",")')
  echo "   creating throwaway busybox pod $THROWAWAY_POD with labels: $SEL_LABELS"
  cat > "$TMPDIR/throwaway.yaml" <<EOF
apiVersion: v1
kind: Pod
metadata:
  name: $THROWAWAY_POD
  namespace: $NS
  labels:
$(echo "$SEL_JSON" | jq -r 'to_entries | .[] | "    " + .key + ": " + .value')
spec:
  containers:
    - name: busybox
      image: $BUSYBOX_IMG
      command: ["sh", "-c", "sleep 3600"]
EOF
  K apply -f "$TMPDIR/throwaway.yaml" 2>&1 || bad "creating the throwaway pod failed"
  READYTW=""
  for i in $(seq 1 30); do
    READYTW=$(K -n "$NS" get pod "$THROWAWAY_POD" -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}' 2>/dev/null)
    [ "$READYTW" = "True" ] && break
    sleep 3
  done
  if [ "$READYTW" != "True" ]; then
    bad "throwaway pod $THROWAWAY_POD did not become Ready"
  else
    EXECCMD=$(K -n "$NS" exec "$THROWAWAY_POD" -- /bin/sh -c 'echo RAN-OK' 2>&1 || true)
    echo "   disallowed exec (/bin/sh) output: $EXECCMD"
    if echo "$EXECCMD" | grep -qi "permission denied\|operation not permitted"; then
      ok "the disallowed exec (/bin/sh) was BLOCKED (Permission denied) by the egress-proxy KubeArmorPolicy"
    elif echo "$EXECCMD" | grep -q "RAN-OK"; then
      bad "the disallowed exec (/bin/sh) RAN ('RAN-OK') — the egress-proxy KubeArmorPolicy did NOT block it"
    elif [ -z "$EXECCMD" ]; then
      bad "disallowed exec produced no output (could not exec)"
    else
      bad "unexpected exec output (neither blocked nor clean run): $EXECCMD"
    fi
  fi
fi

# ===========================================================================
# KNOWN LIMITATIONS (printed, not passed)
# ===========================================================================
echo
echo "============================================================"
echo "KNOWN LIMITATIONS (documented, not asserted as passes):"
echo "  L1 (D38 / R16): on kindnet the apiserver/kubelet HOST-NETWORK endpoints"
echo "     (kube-apiserver svc 10.96.0.1:443, node :6443/:10250) ARE reachable from"
echo "     the agent pod; the agent NetworkPolicy polices pod-IP + external-IP egress,"
echo "     not pod->host-network. Check 5 targets a POD IP, which IS blocked."
echo "  L2: ephemeral kubectl debug containers are NOT policed by KubeArmor, so"
echo "     check 10 proves the exec block on a dedicated throwaway busybox pod"
echo "     carrying the egress-proxy labels, not via an ephemeral container."
echo "============================================================"

echo
if [ "$FAIL" -eq 0 ]; then
  echo "=== Full I42 acceptance: PASS (commit=$COMMIT image=$IMG digest=$IMG_DIGEST) ==="
else
  echo "=== Full I42 acceptance: FAIL (commit=$COMMIT image=$IMG) ==="
fi
exit $FAIL
