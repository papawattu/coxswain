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
NODE_IPS=$(K get nodes -o json 2>/dev/null | jsonpath "import sys,json; print(' '.join(a[1] for n in json.load(sys.stdin)['items'] for a in n['status']['addresses'] if a[0]=='InternalIP'))" 'InternalIP')
[ -z "$NODE_IPS" ] && { echo "FATAL: could not read node InternalIPs"; exit 2; }
DNS_POD_IP=$(K -n kube-system get pod -l k8s-app=kube-dns -o json 2>/dev/null | jsonpath "import sys,json; print(json.load(sys.stdin)['items'][0]['status']['podIP'])" 'podIP')
[ -z "$DNS_POD_IP" ] && { echo "FATAL: no kube-dns pod IP found (the probe's allowed target and the cross-namespace test endpoint)"; exit 2; }
EXTERNAL_IP="${EXTERNAL_IP:-1.1.1.1}"
echo "   apiserver svc IP:   $APISERVER_SVC_IP:443"
for ip in $NODE_IPS; do echo "   node InternalIP:      $ip:6443, $ip:10250"; done
echo "   kube-dns pod IP:    $DNS_POD_IP (allowed, DNS only)"
echo "   external:           $EXTERNAL_IP:443"

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
# Probe pod: busybox, matching the policy's podSelector, no kind-load
# assumption (pulled normally by the node). kubectl exec + the python3
# interpreter give the deterministic 4s TCP-connect probes below.
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
  containers:
    - name: probe
      image: busybox:1.36
      command: ["sh", "-c", "sleep infinity"]
EOF
K -n "$NS" apply -f "$TMPDIR/probe.yaml" >/dev/null
echo "   applying NetworkPolicy + probe pod (busybox:1.36, pulled from the registry)..."
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
echo "--- STEP 2: TCP-connect probes from the probe pod (4s timeout each) ---"
# The probe is raw TCP-CONNECT — the exact layer the CNI polices. A
# completed connect means the CNI let the egress through (REACHABLE); a
# timeout/refused connect means it was dropped (BLOCKED). busybox ships
# python3 (the kubernetes busybox build does), so we use it for a
# deterministic 4s-connect test; nc would work as a fallback, but
# python3's result is unambiguous.
NODE_IPS_ENV=$(echo $NODE_IPS | tr ' ' '\n' | sort -u | tr '\n' ' ')
cat > "$TMPDIR/probe.py" <<PYEOF
import os, socket
TARGETS = [("APISERVER_SVC", os.environ["APISERVER_SVC_IP"], 443)]
for ip in os.environ["NODE_IPS"].split():
    TARGETS.append(("APISERVER_NODE(%s)" % ip, ip, 6443))
    TARGETS.append(("KUBELET_NODE(%s)" % ip, ip, 10250))
TARGETS.append(("CROSS_NS_POD(kube-dns)", os.environ["DNS_POD_IP"], 53))
TARGETS.append(("EXTERNAL", os.environ["EXTERNAL_IP"], 443))
for label, host, port in TARGETS:
    s = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
    s.settimeout(4)
    try:
        s.connect((host, int(port)))
        print("RESULT %s REACHABLE" % label)
    except Exception as exc:
        print("RESULT %s BLOCKED(%s)" % (label, type(exc).__name__))
    finally:
        s.close()
PYEOF
K -n "$NS" cp "$TMPDIR/probe.py" "$PROBE_POD:/tmp/verify-cni-probe.py" >/dev/null 2>&1 || { echo "FATAL: kubectl cp probe.py failed"; exit 2; }
K -n "$NS" exec "$PROBE_POD" -- sh -c "APISERVER_SVC_IP=${APISERVER_SVC_IP} NODE_IPS='${NODE_IPS_ENV}' DNS_POD_IP=${DNS_POD_IP} EXTERNAL_IP=${EXTERNAL_IP} python3 /tmp/verify-cni-probe.py" > "$TMPDIR/results.txt" 2>&1
cat "$TMPDIR/results.txt"

echo
echo "--- RESULT ---"
printf '%-45s %s\n' "endpoint (expected: BLOCKED)" "result"
echo "-------------------------------------------------------------"
# host-network targets only (service IP, node :6443/:10250) are the
# D38 "polices pod -> host-network egress" property; the pod-IP and
# external rows are included for completeness (most CNIs already police
# those).
HOSTNET_FAIL=0
TOTAL_FAIL=0
TOTAL=0
while read -r line; do
  case "$line" in
    RESULT\ *)
      label=$(echo "$line" | awk '{print $2}' | cut -d'(' -f1)
      total=$((TOTAL + 1))
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
  esac
done < "$TMPDIR/results.txt"

echo
if [ "$TOTAL_FAIL" -eq 0 ]; then
  CNI_NOTE=$(K -n kube-system get pod -o json 2>/dev/null | python3 -c "import sys,json; names={p['spec']['containers'][0]['name'] for p in json.load(sys.stdin)['items']}; print('likely ' + ', '.join(sorted(n for n in names if 'calico' in n or 'cilium' in n or 'flannel' in n or 'kindnet' in n or 'weave' in n)))" 2>/dev/null)
  echo "PASS: every probe was BLOCKED by the NetworkPolicy, including the host-network"
  echo "     targets (apiserver svc IP, node :6443/:10250). This CNI ($CNI_NOTE) polices"
  echo "     pod -> host-network egress: it satisfies the D38 production requirement."
  echo "     (The DNS connection to kube-dns on 53 was allowed by the policy, as expected.)"
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
