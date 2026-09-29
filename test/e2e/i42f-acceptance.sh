#!/usr/bin/env bash
# I42f kind acceptance: KubeArmor policies for the egress proxy + model proxy.
#
# Proves (on the pinned kind-coxswain-dev cluster) that the operator's Enforcer
# emits the two proxy KubeArmorPolicies alongside the existing agent one, with
# the correct selector / process / network shape (the I42f deliverable), that
# the allowed traffic still flows through the fenced proxies (egress curl via
# the egress proxy, model call via the model proxy — i.e. the policy is not
# over-blocked), and characterizes the disallowed-exec block behavior.
#
# Uses the i42d-acceptance Loop (network allows + a model endpoint) as the
# running fixture. Context is pinned by the caller: K8S_CONTEXT=kind-coxswain-dev.
#
# Probe scripts are written to $TMPDIR on the host and copied into the agent
# pod with `kubectl cp`, then run with `kubectl exec sh <file>` (no python
# heredocs inside kubectl exec sh -c).
#
# HONEST LIMITATION (Check 2): the egress-proxy pod's standin image is
# distroless (only /usr/local/bin/egress-proxy exists; no /bin/sh), so a
# "disallowed exec" cannot be produced from a binary that is present in the
# image but absent from the allow list. Moreover, on this kind node the
# BPF-LSM enforcement datapath is not active: the kind node container does not
# expose /sys/fs/bpf as bpffs (it is sysfs), /sys/kernel/security/lsm is
# absent inside the node, and the bpf-agent's own startup log records
# DefaultFilePosture:audit from inside the node. KubeArmor therefore cannot
# emit a BPF-LSM "permission denied" block here (the same limitation documented
# in the C6b exec-block e2e's NOTE: a disallowed exec "was observed to still
# run in that environment"). Check 2 therefore asserts the emitted policy
# (the I42f deliverable — the inner-fence KubeArmorPolicy the Enforcer emits
# for the egress-proxy pod, action Block, process.allow = exactly the egress
# proxy binary, network = the effective allows with udp+tcp) and surfaces the
# allowed-exec control, and records that the block SHAPe (a real EACCES on a
# disallowed binary) is proven by the C6b e2e on a golang:1.26 image that has
# a second binary, not here. See the ADR-0007 F2 / R15 acceptance context.

set -uo pipefail

CTX="${K8S_CONTEXT:-kind-coxswain-dev}"
NS=i42d-acceptance
LOOP=i42d-loop
KUBEA_NS=kubearmor
REPO_ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
TMPDIR="${TMPDIR:-/tmp}/i42f-acceptance.$$"
mkdir -p "$TMPDIR"

K() { kubectl --context "$CTX" "$@"; }
FAIL=0
ok()  { echo "   PASS: $*"; }
bad() { echo "   FAIL: $*"; FAIL=1; }

echo "=== I42f kind acceptance (context=$CTX ns=$NS loop=$LOOP) ==="
echo "controller image: $(K -n coxswain-system get pods -l control-plane=controller-manager -o jsonpath='{.items[0].status.containerStatuses[0].imageID}' 2>/dev/null)"
COMMIT=$(git -C "$REPO_ROOT" rev-parse --short HEAD 2>/dev/null)
echo "commit: $COMMIT"
echo

# ===========================================================================
# CHECK 1: three KubeArmorPolicies exist, owned by the Loop, proxy policies
#          carry udp+tcp in matchProtocols, correct selector + process shape.
# ===========================================================================
echo "--- CHECK 1: KubeArmorPolicy objects (agent, egress proxy, model proxy) ---"
for p in "coxswain-$LOOP" "coxswain-$LOOP-egress-proxy" "coxswain-$LOOP-proxy"; do
  if ! K -n "$NS" get kubearmorpolicy "$p" >/dev/null 2>&1; then
    bad "KubeArmorPolicy $p does not exist"
    continue
  fi
  echo "  == $p (full spec) =="
  K -n "$NS" get kubearmorpolicy "$p" -o yaml | sed -n '/^spec:/,$p' | sed 's/^/    /'
  echo
done

echo "CHECK 1 assertions (machine-readable):"
for p in "coxswain-$LOOP" "coxswain-$LOOP-egress-proxy" "coxswain-$LOOP-proxy"; do
  [ "$(K -n "$NS" get kubearmorpolicy "$p" -o name 2>/dev/null)" = "" ] && { bad "$p: not found"; continue; }
  owner=$(K -n "$NS" get kubearmorpolicy "$p" -o jsonpath='{.metadata.ownerReferences[0].kind}/{.metadata.ownerReferences[0].name}' 2>/dev/null)
  action=$(K -n "$NS" get kubearmorpolicy "$p" -o jsonpath='{.spec.action}' 2>/dev/null)
  protos=$(K -n "$NS" get kubearmorpolicy "$p" -o jsonpath='{.spec.network.matchProtocols[*].protocol}' 2>/dev/null | tr ' ' ',' )
  dns=$(K -n "$NS" get kubearmorpolicy "$p" -o jsonpath='{.spec.network.matchDNSQueries[*].domain}' 2>/dev/null | tr '\n' ' ')
  paths=$(K -n "$NS" get kubearmorpolicy "$p" -o jsonpath='{.spec.process.matchPaths[*].path}' 2>/dev/null | tr '\n' ' ')
  sel=$(K -n "$NS" get kubearmorpolicy "$p" -o jsonpath='{.spec.selector.matchLabels}' 2>/dev/null)
  echo "  $p owner=$owner action=$action protos=[$protos] dns=[$dns] paths=[$paths] selector=$sel"
  # expected process path per policy (agent policy has none -> "")
  WANT_PROC=""
  case "$p" in
    "coxswain-$LOOP-egress-proxy") WANT_PROC="/usr/local/bin/egress-proxy" ;;
    "coxswain-$LOOP-proxy")        WANT_PROC="/usr/local/bin/proxy" ;;
  esac
  case "$owner" in
    Loop/$LOOP) ;;
    *) bad "$p: owner is '$owner', expected Loop/$LOOP" ;;
  esac
  case "$action" in
    Block) ;;
    *) bad "$p: spec.action='$action', expected Block (default-deny)" ;;
  esac
  case ",$protos," in
    *,tcp,*udp,*|*,udp,*tcp,*)
      ;;
    *) bad "$p: matchProtocols=[$protos], expected both tcp and udp" ;;
  esac
  if [ -n "${WANT_PROC:-}" ]; then
    case " $paths " in
      *" $WANT_PROC "*)
        # exactly one path, equal to the expected binary
        npaths=$(echo "$paths" | tr ' ' '\n' | grep -c .)
        if [ "$npaths" -eq 1 ]; then ok "$p: process.matchPaths is exactly $WANT_PROC"; else bad "$p: process.matchPaths has $npaths entries [$paths], expected exactly 1 ($WANT_PROC)"; fi
        ;;
      *) bad "$p: process.matchPaths=[$paths], expected $WANT_PROC" ;;
    esac
  fi
done

echo
echo "CHECK 1b: pods are NRI-annotated (kubearmor-policy:enabled) so the engine applies the policy:"
for lbl in "app.kubernetes.io/component=egress-proxy,coxswain.io/egress-proxy-for=$LOOP" \
           "app.kubernetes.io/component=model-proxy,coxswain.io/proxy-for=$LOOP" \
           "app.kubernetes.io/component=agent,coxswain.io/loop=$LOOP"; do
  pod=$(K -n "$NS" get pods -l "$lbl" -o jsonpath='{.items[0].metadata.name}' 2>/dev/null)
  [ -z "$pod" ] && { bad "no pod for selector $lbl"; continue; }
  ann=$(K -n "$NS" get pod "$pod" -o jsonpath='{.metadata.annotations.kubearmor-policy}' 2>/dev/null)
  vis=$(K -n "$NS" get pod "$pod" -o jsonpath='{.metadata.annotations.kubearmor-visibility}' 2>/dev/null)
  echo "  $pod kubearmor-policy=$ann visibility=$vis"
  case "$ann" in
    enabled) ;;
    *) bad "$pod: kubearmor-policy annotation is '$ann', expected enabled" ;;
  esac
done

# ===========================================================================
# CHECK 2: a disallowed exec in the egress-proxy pod is blocked.
#
# Honest scoping (see header + C6b e2e NOTE): the standin image is distroless
# (no /bin/sh; only /usr/local/bin/egress-proxy), and on this kind node the
# BPF-LSM datapath is not active (node does not expose /sys/fs/bpf as bpffs;
# /sys/kernel/security/lsm absent in-node; the bpf-agent's own startup log
# shows DefaultFilePosture:audit). So a real "permission denied" block cannot
# be produced here. We instead:
#   (a) assert the emitted egress-proxy KubeArmorPolicy is the inner fence:
#       spec.action=Block, process.allow = exactly the egress proxy binary,
#       network = the effective allows + udp+tcp (already asserted in CHECK 1);
#   (b) prove the allowed-exec control works (exec the egress proxy binary —
#       it is on the allow list — and it runs: its own bind failure is a
#       deterministic non-network failure proving the exec machinery works);
#   (c) surface the desired posture (kubearmor-config) + the in-node BPF-LSM
#       availability so the record is honest: block posture is set, but the
#       node's BPF-LSM datapath is what gates the actual EACCES.
#   The block SHAPE (a real EACCES on a disallowed binary) is proven by the
#   C6b exec-block e2e on a golang:1.26 image that ships a second binary.
# ===========================================================================
echo
echo "--- CHECK 2: disallowed exec in the egress-proxy pod (inner fence) ---"
EGRESS_POD=$(K -n "$NS" get pods -l "app.kubernetes.io/component=egress-proxy,coxswain.io/egress-proxy-for=$LOOP" -o jsonpath='{.items[0].metadata.name}' 2>/dev/null)
[ -z "$EGRESS_POD" ] && { bad "no egress-proxy pod found"; }
echo "   egress-proxy pod: $EGRESS_POD"

# (c) posture + BPF-LSM datapath availability (honest record).
FILE_POSTURE=$(K -n "$KUBEA_NS" get configmap kubearmor-config -o jsonpath='{.data.defaultFilePosture}' 2>/dev/null)
VIS=$(K -n "$KUBEA_NS" get configmap kubearmor-config -o jsonpath='{.data.visibility}' 2>/dev/null)
echo "   kubearmor-config: defaultFilePosture=$FILE_POSTURE visibility=$VIS"
case "$FILE_POSTURE" in
  block) echo "   (posture is block: the exec allowlist WOULD block, not audit, if the datapath is live)" ;;
  *) bad "defaultFilePosture='$FILE_POSTURE' (expected block)" ;;
esac
echo "   bpf-agent in-node posture (from its startup log):"
K -n "$KUBEA_NS" logs ds/kubearmor-bpf-containerd-98c2c --since=12h 2>/dev/null \
  | grep -oE 'defaultFilePosture:[a-z]+' | head -1 | sed 's/^/     /' || echo "     (not captured)"
NODE_BPF=$(docker exec coxswain-dev-control-plane sh -c 'stat -f -c %T /sys/fs/bpf 2>/dev/null; ls /sys/kernel/security/lsm 2>&1 | head -1' 2>/dev/null)
echo "   node /sys/fs/bpf type + /sys/kernel/security/lsm: $(echo "$NODE_BPF" | tr '\n' ' ')"

# (b) the allowed-exec control: exec the egress proxy binary (on the allow list).
#     It must RUN — its own "address already in use" bind failure is a
#     deterministic, non-network proof that the exec machinery works and the
#     binary is not blocked.
ALLOWED_OUT=$(K -n "$NS" exec "$EGRESS_POD" -- /usr/local/bin/egress-proxy 2>&1 || true)
echo "   allowed exec (/usr/local/bin/egress-proxy) output: $ALLOWED_OUT"
case "$ALLOWED_OUT" in
  *"address already in use"*)
    ok "the egress-proxy binary is on the allow list and RUNS (its own bind failure proves the exec machinery works; the policy does not block the allowed binary)"
    ;;
  "")
    bad "the egress-proxy binary produced no output (exec may have failed — it should be allowed)"
    ;;
  *"permission denied"*)
    bad "the egress-proxy binary was BLOCKED (permission denied) — it is on the allow list and should run"
    ;;
  *)
    ok "the egress-proxy binary ran (first line: $(echo "$ALLOWED_OUT" | head -1))"
    ;;
esac

# (a) the inner-fence policy shape (the I42f deliverable) — re-asserted here
#     for the record so the CHECK 2 block is self-contained.
KAP_EGRESS=$(K -n "$NS" get kubearmorpolicy "coxswain-$LOOP-egress-proxy" -o json 2>/dev/null)
[ -z "$KAP_EGRESS" ] && bad "egress-proxy KubeArmorPolicy not readable"
[ -n "$KAP_EGRESS" ] && echo "   emitted egress-proxy KubeArmorPolicy (the inner fence KubeArmor emits for this pod):" \
  && echo "$KAP_EGRESS" | jq -c '{action:.spec.action, process:.spec.process, network:.spec.network, selector:.spec.selector}' | sed 's/^/     /'

# The disallowed-exec block SHAPE: cannot be produced here (distroless image +
# BPF-LSM datapath not live on this node). Record it as a documented
# limitation rather than a fabricated pass.
echo
echo "   NOTE (honest limitation): a real disallowed-exec 'permission denied' block"
echo "   cannot be produced on this node: the standin image is distroless (no /bin/sh,"
echo "   only the egress proxy binary, which is allowed) and the node's BPF-LSM"
echo "   datapath is not active (/sys/fs/bpf is sysfs not bpffs; /sys/kernel/"
echo "   security/lsm absent in-node; bpf-agent startup log shows DefaultFilePosture:"
echo "   audit). The block SHAPE is proven by the C6b exec-block e2e on a"
echo "   golang:1.26 image that ships a second, disallowed binary. The emitted"
echo "   inner-fence policy above (action Block, process.allow = the egress proxy"
echo "   binary only, network = effective allows + udp+tcp) is the I42f deliverable."

# ===========================================================================
# CHECK 3: from the agent, curl to an allowed host through the egress proxy
#          returns 200 (the proxy resolved it under its policy).
# ===========================================================================
echo
echo "--- CHECK 3: agent curl https://proxy.golang.org via the egress proxy ---"
AGENT_POD=$(K -n "$NS" get pods -l "app.kubernetes.io/component=agent,coxswain.io/loop=$LOOP" -o jsonpath='{.items[0].metadata.name}' 2>/dev/null)
[ -z "$AGENT_POD" ] && AGENT_POD=$(K -n "$NS" get pods -l "coxswain.io/loop=$LOOP" -o jsonpath='{.items[0].metadata.name}' 2>/dev/null)
[ -z "$AGENT_POD" ] && { bad "no agent pod found"; }
echo "   agent pod: $AGENT_POD"

cat > "$TMPDIR/probe-egress.sh" <<'EOF'
#!/bin/sh
echo "HTTPS_PROXY=$HTTPS_PROXY"
echo "NO_PROXY=$NO_PROXY"
echo "-- curl https://proxy.golang.org/ (via egress proxy) --"
curl -sS -o /dev/null -w 'http_code=%{http_code}\n' https://proxy.golang.org/
echo "curl-exit=$?"
EOF
K -n "$NS" cp "$TMPDIR/probe-egress.sh" "$AGENT_POD:/tmp/probe-egress.sh" 2>&1 || bad "kubectl cp probe-egress.sh failed"
EGRESS_OUT=$(K -n "$NS" exec "$AGENT_POD" -- sh /tmp/probe-egress.sh 2>&1)
echo "$EGRESS_OUT"
case "$EGRESS_OUT" in
  *"http_code=200"*)
    ok "curl to proxy.golang.org via the egress proxy returned 200 (the proxy resolved + served it under its policy)"
    ;;
  *)
    bad "curl to proxy.golang.org did NOT return 200 (output: $EGRESS_OUT)"
    ;;
esac
echo "   egress-proxy audit log (the allowed attempts):"
K -n "$NS" logs "$EGRESS_POD" --tail=8 | sed 's/^/     /'

# ===========================================================================
# CHECK 4: a model call via COX_MODEL_BASE_URL returns 200 (the model proxy
#          resolved its endpoint under its policy).
# ===========================================================================
echo
echo "--- CHECK 4: model call via COX_MODEL_BASE_URL ---"
cat > "$TMPDIR/probe-model.sh" <<'EOF'
#!/bin/sh
echo "COX_MODEL_BASE_URL=$COX_MODEL_BASE_URL"
echo "-- curl $COX_MODEL_BASE_URL/ (direct; NO_PROXY bypasses the egress proxy) --"
curl -sS -o /dev/null -w 'http_code=%{http_code}\n' "$COX_MODEL_BASE_URL/"
echo "curl-exit=$?"
EOF
K -n "$NS" cp "$TMPDIR/probe-model.sh" "$AGENT_POD:/tmp/probe-model.sh" 2>&1 || bad "kubectl cp probe-model.sh failed"
MODEL_OUT=$(K -n "$NS" exec "$AGENT_POD" -- sh /tmp/probe-model.sh 2>&1)
echo "$MODEL_OUT"
case "$MODEL_OUT" in
  *"http_code=200"*)
    ok "model call via COX_MODEL_BASE_URL returned 200 (the model proxy resolved its endpoint under its policy)"
    ;;
  *)
    bad "model call via COX_MODEL_BASE_URL did NOT return 200 (output: $MODEL_OUT)"
    ;;
esac
MODEL_POD=$(K -n "$NS" get pods -l "app.kubernetes.io/component=model-proxy,coxswain.io/proxy-for=$LOOP" -o jsonpath='{.items[0].metadata.name}' 2>/dev/null)
echo "   model-proxy pod: $MODEL_POD"
echo "   model-proxy log (startup + forwarding target):"
K -n "$NS" logs "$MODEL_POD" --tail=5 | sed 's/^/     /'

# ===========================================================================
echo
if [ "$FAIL" -eq 0 ]; then
  echo "=== I42f kind acceptance: PASS (checks 1,3,4 green; check 2: emitted inner-fence policy asserted + allowed-exec control green; block SHAPE documented as a node-datapath limitation, proven by the C6b e2e) ==="
else
  echo "=== I42f kind acceptance: FAIL ==="
fi
exit $FAIL
