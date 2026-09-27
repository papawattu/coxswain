#!/usr/bin/env bash
# C6b exec-block e2e: prove that a disallowed exec in the agent container is
# blocked by the KubeArmorPolicy the operator emits, while an allowed one runs.
#
# This is the real-runtime proof that the emitted KubeArmorPolicy is not just
# accepted by the API but actually enforces (D30 / R15 P1 acceptance). It does
# NOT need the I32 alert relay: the operator runs with --allow-unenforced so the
# Loop's sandbox starts, and the test execs directly into the agent container.
#
# Prerequisites (set up by `make kind-up` + the operator deploy):
#   - a kind cluster (KIND_CLUSTER) with the agent-sandbox controller
#   - KubeArmor installed (karmor install --tag v1.7.5)
#   - KubeArmor's default posture set to block (defaultFilePosture: block +
#     process in visibility). KubeArmor v1.7.5 gates the exec allowlist's
#     block-vs-audit on defaultFilePosture (NOT spec.action); karmor install
#     defaults it to audit, so a disallowed exec is logged but ALLOWED unless
#     the posture is block. `make kind-up` sets this; if it is missing here the
#     script fails with a clear posture message (not a confusing "curl ran").
#   - the coxswain operator deployed with --allow-unenforced
#
# What it does:
#   1. Create an AgentPolicy that allows ONLY the "go" binary (exec).
#   2. Create a Loop referencing that policy.
#   3. Wait for the operator to create the Sandbox + the KubeArmorPolicy
#      (process.matchPaths = [{execname: go, path: /usr/local/go/bin/go}], top-level action Block).
#   4. kubectl exec an ALLOWED binary (go) in the agent container -> must succeed.
#   5. kubectl exec a DISALLOWED binary (curl) in the agent container -> must be
#      blocked (non-zero exit / Permission denied).
#
# NOTE (honest limitation): on the dev-box kind 1.34 + KubeArmor v1.7.5, KubeArmor
# is installed and the operator's policy is accepted + loaded on the pod
# (karmor probe shows the pod "Armored Up" with the policy, Active LSM BPFLSM),
# and the allowed exec works. The DISALLOWED exec was observed to still run in
# that environment — a KubeArmor BPF-LSM process-enforcement behavior not
# diagnosed here. This script asserts the disallowed exec is blocked; if it is
# not, the script fails, which is the correct signal that enforcement is not
# active. Treat a green run as proof that enforcement works in that environment.

set -euo pipefail

cd "$(dirname "$0")/../../.."
export PATH="$PATH:$(go env GOPATH)/bin"

KIND_CLUSTER="${KIND_CLUSTER:-coxswain-dev}"
KUBEARMOR_VERSION="${KUBEARMOR_VERSION:-v1.7.5}"   # the v-prefix is mandatory (Docker Hub tags)
NAMESPACE=default
LOOP=execblock
AGENT_POLICY=allow-go
ALLOWED_BINARY=go          # in the AgentPolicy exec allow
DISALLOWED_BINARY=curl     # NOT in the allow -> must be blocked

# The KubeArmor CLI (karmor) is the installer. Download it pinned if not present.
KARMOR_VERSION="${KARMOR_VERSION:-1.4.9}"
karmor() {
  if [ -x ./bin/karmor ]; then ./bin/karmor "$@"; else
    mkdir -p bin
    curl -sfL "https://github.com/kubearmor/kubearmor-client/releases/download/v${KARMOR_VERSION}/karmor_${KARMOR_VERSION}_linux_amd64.tar.gz" \
      | tar xz -C bin 2>/dev/null || true
    [ -x ./bin/karmor ] && ./bin/karmor "$@" || {
      echo "cannot obtain karmor v${KARMOR_VERSION}"; return 1; }
  fi
}

echo "==> cluster: $(kind get clusters | tr '\n' ' ')"
kind get clusters | grep -qx "$KIND_CLUSTER" || { echo "kind cluster $KIND_CLUSTER not found (make kind-up)"; exit 1; }

echo "==> KubeArmor installed?"
kubectl get pods -n kubearmor >/dev/null 2>&1 || { echo "KubeArmor not installed in $KIND_CLUSTER"; exit 1; }
echo "   (karmor install --tag ${KUBEARMOR_VERSION} — pinned in make kind-up)"

echo "==> KubeArmor default posture is block? (required for exec blocking)"
# KubeArmor v1.7.5's BPF-LSM exec block/audit toggle is driven by defaultFilePosture
# (not spec.action). karmor install defaults it to audit, so a disallowed exec is
# logged but ALLOWED. This assertion turns a misconfigured env into a clear failure
# rather than the confusing "curl ran" below.
FILE_POSTURE=$(kubectl -n kubearmor get configmap kubearmor-config -o jsonpath='{.data.defaultFilePosture}' 2>/dev/null || true)
if [ "$FILE_POSTURE" != "block" ]; then
  echo "   FAIL: defaultFilePosture is '${FILE_POSTURE:-unset}', not 'block' — a disallowed exec would be logged but ALLOWED."
  echo "   Re-run kind-up so the posture is set via `karmor install` flags (-b all) BEFORE the agent starts."
  echo "   Do NOT patch the config + rollout-restart the agent: on kernel 6.1 an agent restart can wedge the node's BPF subsystem (ADR-0007 finding F2)."
  echo "   (make kind-up does this automatically)"
  exit 1
fi
echo "   defaultFilePosture=block (exec allowlist will actually block)"

echo "==> creating AgentPolicy ${AGENT_POLICY} (exec: [${ALLOWED_BINARY}]) + Loop ${LOOP}"
kubectl apply -f - <<EOF
apiVersion: coxswain.wattu.com/v1alpha1
kind: AgentPolicy
metadata:
  name: ${AGENT_POLICY}
  namespace: ${NAMESPACE}
spec:
  exec:
    - ${ALLOWED_BINARY}
---
apiVersion: coxswain.wattu.com/v1alpha1
kind: Loop
metadata:
  name: ${LOOP}
  namespace: ${NAMESPACE}
spec:
  goal: "exec block e2e"
  workspace:
    repo: "https://github.com/papawattu/coxswain.git"
    ref: "main"
  policyRefs:
    - ${AGENT_POLICY}
  verify:
    acceptanceChecks:
      - "go test ./..."
  loop:
    maxIterations: 1
EOF

echo "==> waiting for the KubeArmorPolicy coxswain-${LOOP} (process.matchPaths execname ${ALLOWED_BINARY} + absolute path, action Block)"
for i in $(seq 1 30); do
  if kubectl get kubearmorpolicy "coxswain-${LOOP}" -n "$NAMESPACE" >/dev/null 2>&1; then
    break
  fi
  sleep 2
done
KAP=$(kubectl get kubearmorpolicy "coxswain-${LOOP}" -n "$NAMESPACE" -o json 2>/dev/null || true)
[ -n "$KAP" ] || { echo "KubeArmorPolicy coxswain-${LOOP} not created (operator not running with RBAC?)"; exit 1; }
echo "   spec.action = $(echo "$KAP" | python3 -c 'import json,sys; print(json.load(sys.stdin)["spec"].get("action"))')"
echo "   process.matchPaths = $(echo "$KAP" | python3 -c 'import json,sys; print(json.load(sys.stdin)["spec"].get("process",{}).get("matchPaths"))')"

echo "==> waiting for the sandbox pod to be Ready"
POD=""
for i in $(seq 1 40); do
  POD=$(kubectl get pods -n "$NAMESPACE" -l "coxswain.io/loop=${LOOP}" \
        -o jsonpath='{.items[?(@.status.phase=="Running")].metadata.name}' 2>/dev/null | head -1 || true)
  [ -n "$POD" ] || { sleep 5; continue; }
  ready=$(kubectl get pod "$POD" -n "$NAMESPACE" -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}' 2>/dev/null || true)
  if [ "$ready" = "True" ]; then break; fi
  sleep 5
done
[ -n "$POD" ] || { echo "sandbox pod never became Ready"; exit 1; }
echo "   pod: $POD"

# KubeArmor applies policies at pod creation (NRI). Recreate the pod once so the
# policy is definitely applied to a fresh pod (avoids a policy-after-pod race).
echo "==> recreating the pod so KubeArmor applies the policy to a fresh pod"
kubectl delete pod "$POD" -n "$NAMESPACE" >/dev/null 2>&1 || true
POD=""
for i in $(seq 1 40); do
  POD=$(kubectl get pods -n "$NAMESPACE" -l "coxswain.io/loop=${LOOP}" \
        -o jsonpath='{.items[?(@.status.phase=="Running")].metadata.name}' 2>/dev/null | head -1 || true)
  [ -n "$POD" ] || { sleep 5; continue; }
  ready=$(kubectl get pod "$POD" -n "$NAMESPACE" -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}' 2>/dev/null || true)
  if [ "$ready" = "True" ]; then break; fi
  sleep 5
done
[ -n "$POD" ] || { echo "recreated sandbox pod never became Ready"; exit 1; }
echo "   fresh pod: $POD"

echo "==> ALLOWED: exec ${ALLOWED_BINARY} in the agent (must succeed)"
if kubectl exec "$POD" -n "$NAMESPACE" -c agent -- "$ALLOWED_BINARY" version >/dev/null 2>&1; then
  echo "   PASS: ${ALLOWED_BINARY} ran (allowed by policy)"
else
  echo "   FAIL: ${ALLOWED_BINARY} did NOT run (should be allowed)"; exit 1
fi

echo "==> DISALLOWED: exec ${DISALLOWED_BINARY} in the agent (must be BLOCKED)"
set +e
kubectl exec "$POD" -n "$NAMESPACE" -c agent -- "$DISALLOWED_BINARY" --version >/dev/null 2>&1
disallowed_rc=$?
set -e
if [ "$disallowed_rc" -ne 0 ]; then
  echo "   PASS: ${DISALLOWED_BINARY} was BLOCKED (exit ${disallowed_rc}) — enforcement is active"
else
  echo "   FAIL: ${DISALLOWED_BINARY} RAN (exit 0) — KubeArmor is NOT enforcing the policy in this environment"
  echo "   (the policy is accepted + loaded on the pod, but BPF-LSM process enforcement did not block the exec)"
  exit 1
fi

echo "==> cleanup"
kubectl delete loop "$LOOP" -n "$NAMESPACE" >/dev/null 2>&1 || true
kubectl delete agentpolicy "$AGENT_POLICY" -n "$NAMESPACE" >/dev/null 2>&1 || true

echo "PASS: the operator's KubeArmorPolicy enforces (allowed exec runs, disallowed exec blocked)"
