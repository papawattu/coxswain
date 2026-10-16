#!/usr/bin/env bash
# B6 beta acceptance (docs/PLAN-BETA.md, the phase's Done-when, lines 33-42):
# `make beta-e2e` on a FRESH snapshot-capable kind cluster (kind-coxswain-snap,
# the run's scratch cluster), following docs/QUICKSTART.md LITERALLY against
# the CURRENT main build (the v0.1.0 tag is cut by the owner AFTER this
# passes — the v0.1.0 artifacts do not exist yet).
#
#   make beta-e2e
#
# The six Done-when steps, in order:
#   1.  a Manual-approval Loop: the FIRST plan is rejected with a reason and
#       the re-plan's .coxswain/plan-feedback contains the reason (B3);
#   2.  the second plan is approved; the Loop reaches Succeeded and opens a
#       draft PR on GitHub (the live path, D55: hack/github-secret.sh reads
#       ~/.config/coxswain/github-token; the token is never printed and no
#       Secret is dumped);
#   3.  a forced verify failure (the acceptance check `false`) re-plans when
#       onVerifyFail: Plan (B4);
#   4.  the workspace PVC is deleted mid-Implementing and the Loop restores
#       it from its last checkpoint (B2);
#   5.  `cox fork --from iter-N` (the fork CLI's --loop/--iteration form)
#       produces an independent Loop that also succeeds (B2; the fork has its
#       own branch and PR);
#   6.  the Loop metrics are scraped with a coxswain-metrics-reader token:
#       loops_completed_total{result="succeeded"} >= 2 and
#       verify_results_total{result="pass"} >= 2 (B5a).
#
# Cluster: kind-coxswain-snap is deleted and recreated FRESH by this run —
# it is this run's scratch cluster (the B1-B4 kind runs use it; the owner
# said to recreate it). The fresh cluster gets: the snapshot CRDs (the
# install.yaml ships config/crd/external), the snapshot-controller
# (external-snapshotter), the upstream csi-driver-host-path (StorageClass
# hostpath + VolumeSnapshotClass hostpath-snapclass), and — only because the
# tested path needs them — KubeArmor (QUICKSTART: REQUIRED; the tool proxy's
# BPF-LSM DNS fence and the A1 policy gate) . The released install.yaml is
# NOT published for v0.1.0 yet, so the install is `make build-installer`
# output of the CURRENT main tree, applied exactly as QUICKSTART step 2 does
# for a release (kubectl apply of the single file, then rollout status).
# kind-coxswain-dev, kind-coxswain-calico and kind-coxswain-alpha are NEVER
# touched or deleted.
#
# Model: the REAL vLLM endpoint 192.168.1.20:8000 (qwen3.8-27b), as A6 did.
# Reachability is PROVEN from a pod in this cluster before any Loop is
# applied; an unreachable endpoint is a hard STOP with the failing probe
# pasted (no silent stub fallback, #124 norm).
#
# Norms (AGENTS.md): PASS is printed only for a step that was actually
# verified (I84); a release defect is REPORTED, never worked around in the
# script (I90); the token is never printed and Secrets are never dumped
# (D55: name/type/key names only); the D55 leak guard runs over the whole
# evidence dir; logs are kept, never /dev/null.
#
# Evidence: .samples/beta-e2e-<ts>/ (NEVER committed, I68): run.log is the
# script's own stdout (I76: every claim cites a run.log line); digests.txt
# records the build's image digests; per-step files (step1-reject.txt,
# step2-approve.txt, step3-replan.txt, step4-restore.txt, step5-fork.txt,
# step6-metrics.txt).
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
CTX="kind-coxswain-snap"
CLUSTER="coxswain-snap"
NS="${NS:-beta-e2e}"
REPO="${REPO:-https://github.com/papawattu/coxswain-sandbox.git}"
VLLM_ENDPOINT="${VLLM_ENDPOINT:-192.168.1.20:8000}"
VLLM_MODEL="${VLLM_MODEL:-qwen3.8-27b}"
AGENT_IMG="docker.io/library/golang:1.26"
PHASE_POLL=4
# PHASE_CAP: how long a run may sit in one non-terminal phase before it is
# "wedged". 3000 (50 min) covers a real vLLM 27B run with a reject/re-plan
# cycle (a wedge is recorded with numbers, not guessed — the #124 norm).
PHASE_CAP="${PHASE_CAP:-3000}"
# Disk guard (owner, 2026-10-14: ~87% full; stop before 92%): a fresh kind
# cluster + image loads can add ~10 GiB.
DISK_LIMIT_PCT=92
TS="$(date +%s)"
EVIDENCE="$ROOT/.samples/beta-e2e-$TS"
mkdir -p "$EVIDENCE"
export EVIDENCE
log() { printf '\033[1;34m[b6 %s]\033[0m %s\n' "${1:-run}" "${@:2}"; }
die() { printf '\033[1;31m[b6 FATAL]\033[0m %s\n' "$*" >&2; exit 1; }
utc() { date -u +%Y-%m-%dT%H:%M:%SZ; }

# --- tools ------------------------------------------------------------------
GOPATH_BIN="$(go env GOPATH 2>/dev/null)/bin"
case ":$PATH:" in *":$GOPATH_BIN:"*) ;; *) PATH="$GOPATH_BIN:$PATH:$ROOT/bin" ;; esac
export PATH
command -v kubectl >/dev/null || die "kubectl not on PATH"
command -v kind >/dev/null || die "kind not on PATH"
command -v python3 >/dev/null || die "python3 not on PATH"
python3 -c 'import yaml' >/dev/null 2>&1 || die "python3-yaml not importable"
command -v curl >/dev/null || die "curl not on PATH"
command -v helm >/dev/null || die "helm not on PATH (make install-helm)"
[ -f "$ROOT/hack/github-secret.sh" ] || die "hack/github-secret.sh not found"

# The script's own stdout is evidence (I76): every claim cites a run.log line.
exec >"$EVIDENCE/run.log" 2>&1
trap 'echo "[b6] FATAL at $(utc): see $EVIDENCE/run.log"; exit 1' ERR

kc() { kubectl --context "$CTX" "$@"; }
k() { kubectl --context "$CTX" -n "$NS" "$@"; }
COX_BIN=""
build_cox() {
	log cox "building bin/cox from $(git -C "$ROOT" rev-parse --short HEAD) (the CLI is local — the operator is the CURRENT main build)"
	go -C "$ROOT" build -o "$ROOT/bin/cox" ./cmd/cox
	COX_BIN="$ROOT/bin/cox"
}
cox() { "$COX_BIN" --context "$CTX" -n "$NS" "$@"; }

# The loop goal: a real small change on the (near-empty) sandbox repo —
# the first Makefile (docs target) + a README Usage section. A non-trivial
# plan gives the rejection/re-approve a real thing to address.
RENDER_GOAL="${GOAL:-Create a top-level Makefile with a docs target that prints the project README title and, if a docs directory exists, its first heading. Keep it idempotent and dependency-free. Do not modify any other files.}"
# The forced-verify step's check (step 3): the literal `false` — it can never
# pass, which is the point (a FORCED verify failure: the operator's
# onVerifyFail: Plan re-plan path is what is under test, not the agent).
FORCED_CHECK='false'
# The healthy verify check (every other Loop): the make docs target must
# parse (the goal's core artifact exists and is well-formed). A real check
# the agent can pass — not the forced 'false'.
VERIFY_CHECK='make -n docs >/dev/null 2>&1'
# The step-1 rejection reason. It is the one part of the goal the first plan is
# LIKELY to miss (a plan that adds the Makefile without touching README), so
# the rejection is a real steer, not a formality. (I84: the re-plan's
# plan-feedback must contain this reason; the agent may or may not then
# address it — addressing it is the agent's problem, carrying the reason is
# the operator's, and that is what is asserted.)
REJECT_REASON="${REJECT_REASON:-the plan is missing the README.md Usage update: it must document the new make docs target}"

# ---------------------------------------------------------------------------
# 0. Preflights.
# ---------------------------------------------------------------------------
log setup "evidence dir: $EVIDENCE (never committed, I68)"
log setup "host memory: $(free -h | awk '/Mem:/{print $7" available"}')"

# Disk guard (I84/I90): record the usage BEFORE creating the cluster; if the
# run would push / past the limit, STOP and report (the owner said ~87%,
# stop at 92%).
DISK_PCT=$(df -h / | awk 'NR==2{gsub("%","",$5); print $5}')
log preflight "disk usage of /: ${DISK_PCT}% (limit ${DISK_LIMIT_PCT}%; a fresh cluster + image loads add ~10 GiB)"
if [ "$DISK_PCT" -ge "$DISK_LIMIT_PCT" ]; then
	df -h / >"$EVIDENCE/disk.txt"
	die "disk at ${DISK_PCT}% >= ${DISK_LIMIT_PCT}%: a fresh kind cluster would push it past the limit — STOP and report (no cluster created)"
fi
df -h / >"$EVIDENCE/disk-before.txt"

# One kind job at a time: the OTHER clusters must carry no live coxswain
# workload while this runs (host memory ~5-7 GiB free with all four up).
for other in kind-coxswain-dev kind-coxswain-calico kind-coxswain-alpha; do
	PHASES=$(kubectl --context "$other" get loop -A -o jsonpath='{range .items[*]}{.status.phase}{"\n"}{end}' 2>/dev/null | grep -v -e Succeeded -e Failed -e '^$' || true)
	if [ -n "$PHASES" ]; then
		die "$other has a non-terminal coxswain Loop: one kind workload at a time; re-run when it is terminal"
	fi
done
log preflight "kind-coxswain-dev / kind-coxswain-calico / kind-coxswain-alpha carry no live coxswain workload (one kind job at a time)"

# The GitHub token file (D55/D48): existence + readability only. This script
# NEVER prints it, never cats it, never puts it on a command line; only
# hack/github-secret.sh reads it (via --from-file).
TOKEN_FILE="${COXSWAIN_GITHUB_TOKEN_FILE:-$HOME/.config/coxswain/github-token}"
[ -f "$TOKEN_FILE" ] || die "token file missing: $TOKEN_FILE (D55: the only credential a script may read)"
[ -s "$TOKEN_FILE" ] || die "token file empty: $TOKEN_FILE"
[ -r "$TOKEN_FILE" ] || die "token file not readable: $TOKEN_FILE"
log preflight "github token file present (mode $(stat -c %a "$TOKEN_FILE")); content never read by this script (D55)"

# vLLM host preflight (recorded with numbers, #124: a stall is recorded, not
# guessed). The IN-CLUSTER pod proof is the actual check (after the cluster
# exists); this is the host-side warm-up.
vllm_host_preflight() {
	local out
	out=$(curl -s -m 20 -o /dev/null -w "%{http_code} %{time_total}" "http://$VLLM_ENDPOINT/v1/models" 2>/dev/null) || { echo "unreachable"; return 1; }
	local code time
	code="${out%% *}"; time="${out##* }"
	[ "$code" = "200" ] && { echo "ok time=${time}s"; return 0; }
	echo "http=$code time=${time}s"; return 1
}
VLLM_STATE="$(vllm_host_preflight)" || true
log preflight "vLLM host preflight (numbers recorded, #124): $VLLM_ENDPOINT model=$VLLM_MODEL -> $VLLM_STATE"

# ---------------------------------------------------------------------------
# 1. The FRESH snapshot-capable kind cluster (the owner: delete + recreate
#    kind-coxswain-snap ONLY; it is this run's scratch cluster).
# ---------------------------------------------------------------------------
if kind get clusters | grep -qxF "$CLUSTER"; then
	log kind "deleting the EXISTING scratch cluster $CLUSTER (this run's cluster — the owner's instruction; never the dev/calico/alpha clusters)"
	kind delete cluster --name "$CLUSTER" >"$EVIDENCE/kind-delete.log" 2>&1 || { tail -20 "$EVIDENCE/kind-delete.log"; die "kind delete failed (see $EVIDENCE/kind-delete.log)"; }
fi
log kind "creating the FRESH kind cluster $CLUSTER (kind $(kind version 2>/dev/null | head -1), node ${KIND_NODE_IMAGE:-kindest/node:v1.34.0})"
kind create cluster --name "$CLUSTER" --image "${KIND_NODE_IMAGE:-kindest/node:v1.34.0}" \
	>"$EVIDENCE/kind-create.log" 2>&1 || { tail -20 "$EVIDENCE/kind-create.log"; die "kind create failed (see $EVIDENCE/kind-create.log)"; }
kc get nodes -o json >"$EVIDENCE/nodes.json"
log kind "cluster $CLUSTER up (k8s $(kc get nodes -o jsonpath='{.items[0].status.nodeInfo.kubeletVersion}'))"
# agent-sandbox v1.0.4 (QUICKSTART step 1: the RELEASE manifest — the source
# manifest is a ko:// placeholder).
log agent-sandbox "installing v1.0.4 from the release manifest"
curl -fsSL "https://github.com/kubernetes-sigs/agent-sandbox/releases/download/v1.0.4/sandbox.yaml" \
	> "$EVIDENCE/agent-sandbox.yaml"
kc apply -f "$EVIDENCE/agent-sandbox.yaml" >"$EVIDENCE/agent-sandbox-apply.log"
kc rollout status deploy/agent-sandbox-controller -n agent-sandbox-system --timeout=180s \
	>"$EVIDENCE/agent-sandbox-rollout.log" 2>&1 || die "agent-sandbox controller not Ready (see $EVIDENCE/agent-sandbox-rollout.log)"
log agent-sandbox "Ready"

# KubeArmor (QUICKSTART: REQUIRED — the A1 policy gate + the tool proxy's
# BPF-LSM DNS fence). The QUICKSTART's `make kind-up` path: the pinned karmor
# CLI installs the agent.
KUBEARMOR_VERSION="v1.7.5"
KARMOR="$ROOT/bin/karmor"
mkdir -p "$ROOT/bin"
if [ ! -x "$KARMOR" ]; then
	log kubearmor "pinning the karmor CLI (karmor_1.4.9) to bin/karmor"
	curl -sfL "https://github.com/kubearmor/kubearmor-client/releases/download/v1.4.9/karmor_1.4.9_linux_amd64.tar.gz" \
		| tar xz -C "$ROOT/bin" || die "karmor download failed"
fi
# karmor 1.4.9 has no --context flag: install into the CURRENT kubectl
# context (set to the fresh cluster for the duration, then restored).
PREV_CTX="$(kubectl config current-context 2>/dev/null || echo "")"
kubectl config use-context "$CTX" >/dev/null 2>&1 || true
"$KARMOR" install -b all --viz process,file,network --tag "$KUBEARMOR_VERSION" \
	>"$EVIDENCE/kubearmor-install.log" 2>&1 || { kubectl config use-context "$PREV_CTX" >/dev/null 2>&1 || true; tail -20 "$EVIDENCE/kubearmor-install.log"; die "KubeArmor install failed (see $EVIDENCE/kubearmor-install.log)"; }
kubectl config use-context "$PREV_CTX" >/dev/null 2>&1 || true
for i in $(seq 1 30); do
	ready=$(kc -n kubearmor get pods -o jsonpath='{range .items[*]}{.metadata.name} {.status.containerStatuses[0].ready}{"\n"}{end}' 2>/dev/null | awk '/bpf/ && $2=="true"{print $1}')
	[ -n "$ready" ] && { log kubearmor "BPF agent pod Ready on the node: $ready"; break; }
	[ "$i" -eq 30 ] && { kc -n kubearmor get pods >"$EVIDENCE/kubearmor-pods-debug.txt"; die "the KubeArmor BPF agent pod is not Ready after 150s (see $EVIDENCE/kubearmor-pods-debug.txt)"; }
	sleep 5
done
BPF_NODES=$(kc get nodes -l "kubearmor.io/enforcer=bpf" -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}' 2>/dev/null | tr '\n' ' ')
[ -n "$BPF_NODES" ] || die "no node carries the BPF-LSM label (the A1 fact is absent; see $EVIDENCE/kubearmor-install.log)"
log kubearmor "KubeArmor $KUBEARMOR_VERSION installed (BPF-LSM nodes: $BPF_NODES)"

# snapshot-controller (external-snapshotter master; the B1-B4 runs installed it
# the same way — its Deployment in kube-system + the hostpath plugin's sidecar
# provision the hostpath snapshots). The CRDs the operator's install.yaml ALSO
# ships (config/crd/external) are applied here first so the snapshot controller
# can start; the operator's later apply of the same CRDs is idempotent.
# NOTE (B6 run, 2026-10-14): the old config/crd/standard.yaml +
# deploy/kubernetes/snapshot-controller.yaml layout no longer exists upstream
# (those URLs 404). The controller moved to
# deploy/kubernetes/snapshot-controller/{rbac,setup}-snapshot-controller.yaml
# (SA snapshot-controller in kube-system + Deployment). The CRDs are shipped
# by the OPERATOR's install.yaml (config/crd/external) — no separate CRD
# fetch is needed.
log snapshot "installing the external-snapshotter controller (RBAC + Deployment, SA in kube-system)"
# The snapshot CRDs (VCS/VSC/VolumeSnapshot) must exist BEFORE the controller
# starts — it lists VCS on startup and exits with 'the server could not find
# the requested resource' if they are absent (the 2026-10-14 run's crash-loop:
# 4 restarts in 180s, 'Failed to list v1 volumesnapshots ... not found'). The
# operator ships the same CRDs in its install.yaml (config/crd/external, applied
# later); applying them here from the repo is idempotent with that later apply.
curl -fsSL "https://raw.githubusercontent.com/papawattu/coxswain/slice/b6-beta-e2e/config/crd/external/snapshot.storage.k8s.io_volumesnapshots.yaml" \
	> "$EVIDENCE/snapshot-crd-volumesnapshots.yaml" || die "cannot fetch the VolumeSnapshot CRD from the repo (config/crd/external)"
curl -fsSL "https://raw.githubusercontent.com/papawattu/coxswain/slice/b6-beta-e2e/config/crd/external/snapshot.storage.k8s.io_volumesnapshotcontents.yaml" \
	> "$EVIDENCE/snapshot-crd-contents.yaml" || die "cannot fetch the VolumeSnapshotContent CRD from the repo (config/crd/external)"
curl -fsSL "https://raw.githubusercontent.com/papawattu/coxswain/slice/b6-beta-e2e/config/crd/external/snapshot.storage.k8s.io_volumesnapshotclasses.yaml" \
	> "$EVIDENCE/snapshot-crd-classes.yaml" || die "cannot fetch the VolumeSnapshotClass CRD from the repo (config/crd/external)"
kc apply -f "$EVIDENCE/snapshot-crd-volumesnapshots.yaml" -f "$EVIDENCE/snapshot-crd-contents.yaml" -f "$EVIDENCE/snapshot-crd-classes.yaml" >"$EVIDENCE/snapshot-crd-apply.log" 2>&1 || { cat "$EVIDENCE/snapshot-crd-apply.log"; die "could not apply the snapshot CRDs (the operator needs them for the controller)"; }
kc get crd volumesnapshots.snapshot.storage.k8s.io >/dev/null 2>&1 || die "the VolumeSnapshot CRD is not registered (the controller would crash-loop)"
log snapshot "snapshot CRDs applied (VolumeSnapshot, VolumeSnapshotContent, VolumeSnapshotClass)"
curl -fsSL "https://raw.githubusercontent.com/kubernetes-csi/external-snapshotter/master/deploy/kubernetes/snapshot-controller/rbac-snapshot-controller.yaml" \
	> "$EVIDENCE/snapshot-controller-rbac.yaml" || die "cannot fetch the external-snapshotter controller RBAC (the upstream layout changed; the controller moved to deploy/kubernetes/snapshot-controller/)"
kc apply -f "$EVIDENCE/snapshot-controller-rbac.yaml" >"$EVIDENCE/snapshot-controller-rbac-apply.log" 2>&1 || { cat "$EVIDENCE/snapshot-controller-rbac-apply.log"; die "snapshot-controller RBAC apply failed"; }
curl -fsSL "https://raw.githubusercontent.com/kubernetes-csi/external-snapshotter/master/deploy/kubernetes/snapshot-controller/setup-snapshot-controller.yaml" \
	> "$EVIDENCE/snapshot-controller.yaml" || die "cannot fetch the external-snapshotter controller Deployment"
kc apply -f "$EVIDENCE/snapshot-controller.yaml" >"$EVIDENCE/snapshot-controller-apply.log" 2>&1 || { cat "$EVIDENCE/snapshot-controller-apply.log"; die "snapshot-controller Deployment apply failed"; }
kc -n kube-system rollout status deploy/snapshot-controller --timeout=180s \
	>"$EVIDENCE/snapshot-controller-rollout.log" 2>&1 || die "snapshot-controller not Ready (see $EVIDENCE/snapshot-controller-rollout.log)"
log snapshot "external-snapshotter controller Ready (kube-system)"

# upstream csi-driver-host-path (the B1-B4 runs used the upstream
# kubernetes-csi driver, driver name hostpath.csi.k8s.io): the StatefulSet
# csi-hostpathplugin in default + its RBAC. The operator's workspace PVC uses
# the cluster's DEFAULT StorageClass, so this run creates the hostpath
# StorageClass as default (nil storageClassName on the PVC => the default SC).
#
# The install must apply EXACTLY what the upstream deploy.sh applies
# (deploy/kubernetes-1.34/deploy.sh -> ../util/deploy-hostpath.sh, csi-driver-
# host-path v1.18.0): the five sidecar RBAC manifests at the PINNED sidecar
# versions (the sidecar ClusterRoles external-{provisioner,attacher,
# snapshotter,resizer,snapshot-metadata,health-monitor}-runner that the
# plugin manifest's ClusterRoleBindings bind to — WITHOUT them the sidecars
# log 'persistentvolumeclaims is forbidden ... external-provisioner-runner not
# found' and the PVC stays Pending), plus the hostpath/*.yaml manifests
# (plugin + the CSIDriver object csi-hostpath-driverinfo.yaml — without the
# CSIDriver the kubelet cannot mount the volume and the PVC never binds).
# The B6 run's csi-hostpath-plugin.yaml-only install (the fe0f6dd 404 fix)
# dropped both of those. Pinned, not master: a future upstream layout change
# must not break this run silently.
log snapshot "installing the upstream csi-driver-host-path v1.18.0 (the sidecar RBAC at the pinned sidecar versions + the plugin + the CSIDriver, exactly as the upstream deploy.sh does)"
CSI_HOSTPATH_TAG="v1.18.0"
CSI_HOSTPATH_BASE="https://raw.githubusercontent.com/kubernetes-csi/csi-driver-host-path/$CSI_HOSTPATH_TAG/deploy/kubernetes-1.34/hostpath"
# The sidecar RBAC at the pinned sidecar versions (deploy-hostpath.sh's
# rbac_version() reads the image tag from the plugin manifest and fetches that
# version's rbac.yaml from each sidecar repo; the snapshotter RBAC moved to
# csi-snapshotter/rbac-csi-snapshotter.yaml for snapshotter >= v6, which the
# v8.6.0 pin is):
SIDECAR_RBACS=(
	"external-provisioner|v6.3.0|deploy/kubernetes/rbac.yaml"
	"external-attacher|v4.12.0|deploy/kubernetes/rbac.yaml"
	"external-snapshotter|v8.6.0|deploy/kubernetes/csi-snapshotter/rbac-csi-snapshotter.yaml"
	"external-resizer|v2.2.1|deploy/kubernetes/rbac.yaml"
	"external-health-monitor|v0.15.0|deploy/kubernetes/external-health-monitor-controller/rbac.yaml"
)
for entry in "${SIDECAR_RBACS[@]}"; do
	IFS='|' read -r repo ver path <<<"$entry"
	curl -fsSL "https://raw.githubusercontent.com/kubernetes-csi/$repo/$ver/$path" \
		> "$EVIDENCE/csi-rbac-$repo-$ver.yaml" || die "cannot fetch the $repo $ver RBAC (the upstream layout changed; pin a working version — the sidecar ClusterRoles the plugin binds to)"
done
kc apply -f "$EVIDENCE/csi-rbac-external-provisioner-v6.3.0.yaml" \
	-f "$EVIDENCE/csi-rbac-external-attacher-v4.12.0.yaml" \
	-f "$EVIDENCE/csi-rbac-external-snapshotter-v8.6.0.yaml" \
	-f "$EVIDENCE/csi-rbac-external-resizer-v2.2.1.yaml" \
	-f "$EVIDENCE/csi-rbac-external-health-monitor-v0.15.0.yaml" \
	>"$EVIDENCE/csi-sidecar-rbac-apply.log" 2>&1 || { cat "$EVIDENCE/csi-sidecar-rbac-apply.log"; die "the sidecar RBAC apply failed"; }
kc get clusterrole external-provisioner-runner >/dev/null 2>&1 || die "the external-provisioner-runner ClusterRole is missing (the provisioner sidecar cannot list PVCs — the PVC would stay Pending)"
log snapshot "the sidecar RBAC is in place (external-provisioner-runner present: $(kc get clusterrole external-provisioner-runner -o jsonpath='{.metadata.name}') + 4 more)"
curl -fsSL "$CSI_HOSTPATH_BASE/csi-hostpath-plugin.yaml" \
	> "$EVIDENCE/csi-hostpath.yaml" || die "cannot fetch the csi-driver-host-path $CSI_HOSTPATH_TAG plugin manifest ($CSI_HOSTPATH_BASE)"
curl -fsSL "$CSI_HOSTPATH_BASE/csi-hostpath-driverinfo.yaml" \
	> "$EVIDENCE/csi-hostpath-driverinfo.yaml" || die "cannot fetch the csi-driver-host-path $CSI_HOSTPATH_TAG CSIDriver manifest (the kubelet needs the CSIDriver object to mount the volumes)"
kc apply -f "$EVIDENCE/csi-hostpath.yaml" -f "$EVIDENCE/csi-hostpath-driverinfo.yaml" >"$EVIDENCE/csi-hostpath-apply.log" 2>&1 || { cat "$EVIDENCE/csi-hostpath-apply.log"; die "csi-hostpath apply failed"; }
i=0
while :; do
	ready=$(kc -n default get sts csi-hostpathplugin -o jsonpath='{.status.readyReplicas}' 2>/dev/null || echo "")
	[ "$ready" -ge 1 ] 2>/dev/null && { log snapshot "csi-hostpathplugin Ready"; break; }
	i=$((i + 5)); [ "$i" -gt 180 ] && { kc -n default get pods -l app=csi-hostpathplugin >"$EVIDENCE/csi-hostpath-debug.txt"; kc -n default logs sts/csi-hostpathplugin -c csi-provisioner --tail=30 >>"$EVIDENCE/csi-hostpath-debug.txt" 2>&1; die "csi-hostpathplugin not Ready after 180s (see $EVIDENCE/csi-hostpath-debug.txt)"; }
	sleep 5
done
kc get csidriver hostpath.csi.k8s.io >/dev/null 2>&1 || die "the hostpath.csi.k8s.io CSIDriver object is missing (the kubelet cannot mount hostpath volumes; the workspace PVC would stay Pending)"
log snapshot "the CSIDriver hostpath.csi.k8s.io is registered"
# The hostpath StorageClass (default) + the VolumeSnapshotClass (the upstream
# csi-hostpath-snapclass, driver hostpath.csi.k8s.io — the B1-B4 runs' recipe).
# The plugin manifest does NOT ship a StorageClass (the operator needs the
# workspace PVC to use a snapshot-capable default SC), so it is applied here.
cat >"$EVIDENCE/hostpath-sc-snapclass.yaml" <<'EOF'
apiVersion: storage.k8s.io/v1
kind: StorageClass
metadata:
  name: hostpath
  annotations:
    storageclass.kubernetes.io/is-default-class: "true"
provisioner: hostpath.csi.k8s.io
allowVolumeExpansion: true
reclaimPolicy: Delete
---
apiVersion: snapshot.storage.k8s.io/v1
kind: VolumeSnapshotClass
metadata:
  name: hostpath-snapclass
driver: hostpath.csi.k8s.io
deletionPolicy: Delete
EOF
kc apply -f "$EVIDENCE/hostpath-sc-snapclass.yaml" >/dev/null 2>&1 || { cat "$EVIDENCE/hostpath-sc-snapclass.yaml"; die "could not apply the hostpath StorageClass/VolumeSnapshotClass"; }
# kind (v1.34) ships the local-path 'standard' StorageClass with the
# is-default-class annotation TOO: two default SCs makes the default
# ambiguous for a PVC that does not name a storageClassName (the operator's
# workspace PVC does not — it uses the cluster default), and the B6 run 3's
# workspace PVC went Pending partly because 'standard' was still marked
# default. Unmark every default annotation except hostpath's, and fail the
# run if more than one default SC remains.
# Find every SC that carries the default annotation EXCEPT hostpath, and
# unmark it (kubectl annotate with the trailing dash deletes the key).
kubectl --context "$CTX" get storageclass -o json | python3 -c '
import json, sys
d = json.load(sys.stdin)
for sc in d["items"]:
    name = sc["metadata"]["name"]
    ann = sc["metadata"].get("annotations", {})
    if name != "hostpath" and ann.get("storageclass.kubernetes.io/is-default-class") is not None:
        print(name)
' >"$EVIDENCE/storageclass-unmark-list.txt" 2>&1
while IFS= read -r sc; do
	[ -n "$sc" ] && { kubectl --context "$CTX" annotate sc "$sc" storageclass.kubernetes.io/is-default-class- >/dev/null 2>&1 || true; }
done < "$EVIDENCE/storageclass-unmark-list.txt"
# Re-apply hostpath WITH the default annotation so it is the only one left:
kubectl --context "$CTX" apply -f "$EVIDENCE/hostpath-sc-snapclass.yaml" >/dev/null 2>&1
N_DEFAULT=$(kubectl --context "$CTX" get storageclass --no-headers 2>/dev/null | grep -c "(default)") || N_DEFAULT=0
[ "$N_DEFAULT" -eq 1 ] || { kubectl --context "$CTX" get storageclass >"$EVIDENCE/storageclasses-two-defaults.txt"; die "more than one default StorageClass after the unmark ($N_DEFAULT — see $EVIDENCE/storageclasses-two-defaults.txt): the workspace PVC's default SC is ambiguous"; }
kubectl --context "$CTX" get storageclass >"$EVIDENCE/storageclasses.txt"
kubectl --context "$CTX" get volumesnapshotclass -o json >"$EVIDENCE/snapshotclasses.json"
grep -q "hostpath" "$EVIDENCE/storageclasses.txt" || die "no hostpath StorageClass (see $EVIDENCE/storageclasses.txt)"
grep -q "(default)" "$EVIDENCE/storageclasses.txt" || die "no StorageClass is marked default (see $EVIDENCE/storageclasses.txt) — the workspace PVC would have no default SC"
log snapshot "the default StorageClass is hostpath (unmarked: $(tr "\n" " " < "$EVIDENCE/storageclass-unmark-list.txt" | sed "s/\s\+$//"); VolumeSnapshotClass: hostpath-snapclass (driver hostpath.csi.k8s.io)"

# Pre-load the images the run will pull: the verify/peek pod image and the
# curl pod (the vLLM reachability probe + the metrics scrape — both run on
# the node, which has no internet egress: I31).
log preload "pre-loading $AGENT_IMG and docker.io/curlimages/curl:latest into the kind node"
docker pull "$AGENT_IMG" >"$EVIDENCE/preload-agent.log" 2>&1 \
	&& kind load docker-image "$AGENT_IMG" --name "$CLUSTER" >>"$EVIDENCE/preload-agent.log" 2>&1 \
	|| log preload "warning: could not pre-load $AGENT_IMG (the node will pull it)"
docker pull docker.io/curlimages/curl:latest >>"$EVIDENCE/preload-agent.log" 2>&1 \
	&& kind load docker-image docker.io/curlimages/curl:latest --name "$CLUSTER" >>"$EVIDENCE/preload-agent.log" 2>&1 \
	|| log preload "warning: could not pre-load curlimages/curl (the node will pull it)"

# ---------------------------------------------------------------------------
# 2. Build the install.yaml of the CURRENT main (the v0.1.0 artifacts do not
#    exist yet — the owner tags after this passes) and apply it the way
#    QUICKSTART step 2 applies a release: kubectl apply of the single file +
#    rollout status. No manual RBAC, no in-cluster edits.
# ---------------------------------------------------------------------------
BUILD_TAG="b6-$(git -C "$ROOT" rev-parse --short HEAD)-$TS"
IMG="ghcr.io/papawattu/coxswain-controller:$BUILD_TAG"
log installer "building dist/install.yaml of the CURRENT main (commit $(git -C "$ROOT" rev-parse --short HEAD)) with IMG=$IMG"
make -C "$ROOT" build-installer IMG="$IMG" >"$EVIDENCE/build-installer.log" 2>&1 \
	|| { cat "$EVIDENCE/build-installer.log"; die "make build-installer failed (see $EVIDENCE/build-installer.log)"; }
cp "$ROOT/dist/install.yaml" "$EVIDENCE/install.yaml"
INSTALL_SHA256="$(sha256sum "$EVIDENCE/install.yaml" | cut -d' ' -f1)"
log installer "install.yaml built (sha256 $INSTALL_SHA256; all coxswain images tagged $BUILD_TAG)"

# The five images are built LOCALLY from the CURRENT main tree (the release
# workflow builds the same set from the same Dockerfiles at tag time; the
# v0.1.0 ghcr images do not exist yet — the owner tags after this run). Each
# is tagged with $BUILD_TAG, loaded into the node, and its imageID recorded
# (the digest of the bytes that actually run — the honest equivalent of
# A6's release-image digest record). The install.yaml rendered by
# build-installer already carries the controller image ($IMG) and the four
# proxy images at $BUILD_TAG (the install-args patch), so a plain kubectl
# apply runs the locally-built set — no manifest re-pointing.
# controller: the root Dockerfile; the other four: their cmd/<r>/Dockerfiles
# (all built from the repo root, as the release workflow does).
: >"$EVIDENCE/digests.txt"
echo "build tag: $BUILD_TAG (current main, commit $(git -C "$ROOT" rev-parse --short HEAD))" >>"$EVIDENCE/digests.txt"
build_load_record() { # <name> <dockerfile> [make-arg-image]
	local name="$1" df="$2"
	# $name is a full image reference (registry/namespace/repo); slashes make
	# it a path into directories that do not exist when used in a local
	# filename. Sanitise for the evidence filenames; keep the full ref for
	# the docker/kind commands.
	local base; base=$(printf '%s' "$name" | tr '/:' '__')
	local iid="$EVIDENCE/iid-$base.txt"
	log installer "building $name:$BUILD_TAG from $df"
	docker build -f "$df" -t "$name:$BUILD_TAG" --iidfile "$iid" "$ROOT" \
		>"$EVIDENCE/docker-build-$base.log" 2>&1 \
		|| { tail -30 "$EVIDENCE/docker-build-$base.log"; die "docker build of $name failed (see $EVIDENCE/docker-build-$base.log)"; }
	kind load docker-image "$name:$BUILD_TAG" --name "$CLUSTER" >>"$EVIDENCE/docker-build-$base.log" 2>&1 \
		|| die "could not load $name:$BUILD_TAG into the node"
	local id
	id=$(cat "$iid")
	printf '%s: %s\n' "$name" "$id" >>"$EVIDENCE/digests.txt"
	log installer "$name imageID: $id"
}
build_load_record "ghcr.io/papawattu/coxswain-controller" "Dockerfile"
build_load_record "ghcr.io/papawattu/coxswain-runner" "cmd/runner/Dockerfile"
build_load_record "ghcr.io/papawattu/coxswain-model-proxy" "cmd/model-proxy/Dockerfile"
build_load_record "ghcr.io/papawattu/coxswain-tool-proxy" "cmd/tool-proxy/Dockerfile"
build_load_record "ghcr.io/papawattu/coxswain-egress-proxy" "cmd/egress-proxy/Dockerfile"

INSTALL_SHA256="$(sha256sum "$EVIDENCE/install.yaml" | cut -d' ' -f1)"
echo "install.yaml sha256 (final applied bytes): $INSTALL_SHA256" >>"$EVIDENCE/digests.txt"
log operator "applying the built install.yaml (QUICKSTART step 2, verbatim flow)"
kc apply -f "$EVIDENCE/install.yaml" >"$EVIDENCE/operator-apply.log" 2>&1 \
	|| { cat "$EVIDENCE/operator-apply.log"; die "the install.yaml apply failed (I90: a release/manifest defect — report it, do not work around it; see $EVIDENCE/operator-apply.log)"; }
kc rollout status deploy/coxswain-controller-manager -n coxswain-system --timeout=180s \
	>"$EVIDENCE/operator-rollout.log" 2>&1 || die "controller not Ready (see $EVIDENCE/operator-rollout.log)"
log operator "coxswain-controller-manager Ready (current main build $BUILD_TAG)"
# The network gate: this fresh cluster runs kindnet (no Calico) — the D38
# dynamic gate cannot read a policed CNI, so the sandbox would be held
# Suspended. The escape hatch --allow-unenforced-network is the QUICKSTART's
# documented dev escape hatch (D38); it opens ONLY that gate. The KubeArmor
# BPF-LSM static-policy gate (A1) remains live and is what the main Loop
# proves end to end.
kc -n coxswain-system get deploy coxswain-controller-manager -o jsonpath='{.spec.template.spec.containers[0].args}' 2>/dev/null | grep -q -- '--allow-unenforced-network' \
	&& log operator "the applied controller args already carry --allow-unenforced-network (the install.yaml ships it) — LABELED: the D38 escape hatch (kindnet does not police pod->host egress; the KubeArmor BPF-LSM static-policy gate remains live)" \
	|| { kc -n coxswain-system patch deploy coxswain-controller-manager --type json -p='[{"op":"add","path":"/spec/template/spec/containers/0/args/-","value":"--allow-unenforced-network"}]' >"$EVIDENCE/operator-allow-unenforced.log" 2>&1 \
		|| die "could not add --allow-unenforced-network (see $EVIDENCE/operator-allow-unenforced.log)"
		log operator "patched --allow-unenforced-network ONLY — LABELED: the D38 escape hatch (the KubeArmor BPF-LSM static-policy gate remains live and is what the main Loop proves)"
		kc -n coxswain-system rollout status deploy/coxswain-controller-manager --timeout=120s >"$EVIDENCE/operator-rollout-2.log" 2>&1 || die "controller not Ready after the escape hatch (see $EVIDENCE/operator-rollout-2.log)"; }

# The in-cluster vLLM reachability proof (the actual check, after the
# cluster + namespace exist): a NORMAL pod (the sandbox model-proxy's real
# path) must reach the endpoint. Unreachable => STOP with the failing probe
# pasted (no silent stub fallback).
kc get ns "$NS" >/dev/null 2>&1 || kc create ns "$NS" >/dev/null
cat >"$EVIDENCE/reach-eth0.yaml" <<EOF
apiVersion: v1
kind: Pod
metadata:
  name: vllm-reach-eth0
  namespace: $NS
spec:
  restartPolicy: Never
  containers:
  - name: v
    image: docker.io/curlimages/curl:latest
    command:
    - sh
    - -c
    - curl -sS -m 25 -o /tmp/models.json -w 'PROBE_HTTP=%{http_code} PROBE_TIME=%{time_total}\n' http://$VLLM_ENDPOINT/v1/models; r=\$?; echo PROBE_RC=\$r; head -c 1500 /tmp/models.json 2>/dev/null; echo; curl -sS -m 25 -o - http://$VLLM_ENDPOINT/v1/models 2>&1 | head -c 500
EOF
kc apply -f "$EVIDENCE/reach-eth0.yaml" >/dev/null
i=0
while :; do
	st=$(kc -n "$NS" get pod vllm-reach-eth0 -o jsonpath='{.status.phase}' 2>/dev/null || echo "")
	case "$st" in Succeeded | Failed) break ;; esac
	i=$((i + 2)); [ "$i" -gt 90 ] && die "the reachability probe pod did not finish in 90s (state $st)"
	sleep 2
done
kc -n "$NS" logs vllm-reach-eth0 >"$EVIDENCE/reach-eth0.log" 2>&1 || true
kc -n "$NS" delete pod vllm-reach-eth0 --wait=false >/dev/null 2>&1 || true
if ! grep -q 'PROBE_RC=0' "$EVIDENCE/reach-eth0.log"; then
	cat "$EVIDENCE/reach-eth0.log"
	die "vLLM $VLLM_ENDPOINT UNREACHABLE from a normal pod in $NS — failing probe pasted above (no stub fallback; the #124 norm)"
fi
log preflight "vLLM reachable from a NORMAL pod in $NS (PROBE_RC=0 in $EVIDENCE/reach-eth0.log) — the model-proxy's real path"

# The model Secret (the P2b shape: model-key + MODEL_BASE_URL).
MODEL_SECRET="beta-model-creds"
MODEL_KEY="beta-e2e-dummy-key"   # the vLLM is no-auth; the metering proxy needs a non-empty key file (I75)
kc -n "$NS" create secret generic "$MODEL_SECRET" \
	--from-literal="model-key=$MODEL_KEY" \
	--from-literal="MODEL_BASE_URL=http://$VLLM_ENDPOINT" \
	--dry-run=client -o yaml | kc apply -f - >/dev/null
log secrets "model Secret '$MODEL_SECRET' (vllm: $VLLM_ENDPOINT)"

# The GitHub Secret via hack/github-secret.sh (D55: it reads only the token
# file; the token is never printed).
log secrets "creating the GitHub Secret via hack/github-secret.sh (D55)"
bash "$ROOT/hack/github-secret.sh" "$NS" >"$EVIDENCE/github-secret.log" 2>&1 || { cat "$EVIDENCE/github-secret.log"; die "hack/github-secret.sh failed"; }
# D55: never `get secret -o json` (it dumps the base64 token). Name/type/key
# NAMES only, no values.
GIT_CRED_TYPE=$(kc -n "$NS" get secret git-cred -o jsonpath='{.type}' 2>/dev/null || echo "")
GIT_CRED_KEYS=$(kc -n "$NS" get secret git-cred -o go-template='{{ range $k, $v := .data }}{{ $k }} {{ end }}' 2>/dev/null | tr -s ' ')
{
	echo "name: git-cred"
	echo "type: $GIT_CRED_TYPE"
	echo "keys: $GIT_CRED_KEYS"
} >"$EVIDENCE/git-cred.txt"
log secrets "git-cred present in $NS (type=$GIT_CRED_TYPE, keys=$GIT_CRED_KEYS) — values never dumped (D55)"

# The cox CLI (local build — the CLI is not part of the release image set).
build_cox
cox --help >"$EVIDENCE/cox-help.log" 2>&1

# ---------------------------------------------------------------------------
# Loop helpers.
# ---------------------------------------------------------------------------
render_loop() { # <name> [extra-spec-yaml]
	local name="$1" extra="${2:-}"
	cat >"$EVIDENCE/loop-$name.yaml" <<EOF
apiVersion: coxswain.wattu.com/v1alpha1
kind: Loop
metadata:
  name: $name
  namespace: $NS
spec:
  goal: >-
    ${RENDER_GOAL}
  workspace:
    repo: "$REPO"
    ref: "main"
    gitCredentialSecret: git-cred
  verify:
    acceptanceChecks:
      - "$VERIFY_CHECK"
    image: $AGENT_IMG
  agent:
    endpointSecretRef: $MODEL_SECRET
    modelEndpoint: $VLLM_ENDPOINT
    model: $VLLM_MODEL
  delivery:
    mode: PullRequest
    baseBranch: main
    branchPrefix: coxswain/
  loop:
    maxIterations: 5
$extra
EOF
	kc apply -f "$EVIDENCE/loop-$name.yaml" >/dev/null
}
phase_of() { k get loop "$1" -o jsonpath='{.status.phase}' 2>/dev/null || echo ""; }

# Watch a Loop until a terminal phase (or, when $2 is set, any of the listed
# phases); record the live phase trail. Return the observed phase.
watch_loop() { # <loop> <outfile> [stopphases ...]
	local loop="$1" out="$2"; shift 2
	local stopphases="${*:-}" phase i=0
	: >"$out"
	while :; do
		phase=$(phase_of "$loop")
		local iter
		iter=$(k get loop "$loop" -o jsonpath='{.status.iteration}' 2>/dev/null || echo "")
		printf '%s ph=%s iter=%s\n' "$(utc)" "$phase" "$iter" >>"$out"
		case "$phase" in
		Succeeded | Failed | Paused) return 0 ;;
		esac
		if [ -n "$stopphases" ]; then
			local sp
			for sp in $stopphases; do
				[ "$phase" = "$sp" ] && return 0
			done
		fi
		i=$((i + PHASE_POLL))
		[ "$i" -gt "$PHASE_CAP" ] && { log watch "$loop: no terminal phase in $PHASE_CAP s (last: $phase) — recorded, not guessed"; return 1; }
		sleep "$PHASE_POLL"
	done
}

wait_phase() { # <loop> <phase> [cap-seconds] -> 0 if reached
	local loop="$1" want="$2" cap="${3:-600}" i=0 ph
	while :; do
		ph=$(phase_of "$loop")
		case "$ph" in
		"$want") return 0 ;;
		Succeeded | Failed) [ "$ph" = "$want" ] || return 2 ;;
		esac
		i=$((i + 4)); [ "$i" -gt "$cap" ] && return 1
		sleep 4
	done
}

collect_evidence() { # <loop> <tag>
	local loop="${1:-<none>}" tag="${2:-<none>}" d
	d="$EVIDENCE/$tag"
	mkdir -p "$d"
	kc -n "$NS" get loop "$loop" -o json >"$d/loop.json" 2>/dev/null || true
	kc -n "$NS" get events -o json --field-selector "involvedObject.name=$loop" >"$d/events.json" 2>/dev/null || true
	kc -n "$NS" get pods -o json >"$d/pods.json" 2>/dev/null || true
	kc -n "$NS" get pvc -o json >"$d/pvcs.json" 2>/dev/null || true
	kc -n "$NS" get volumesnapshot -o json >"$d/volumesnapshots.json" 2>/dev/null || true
	kc -n coxswain-system logs "deploy/coxswain-controller-manager" >"$d/operator-logs.log" 2>&1 || true
	cox describe "$loop" >"$d/cox-describe.log" 2>&1 || true
	cox plan "$loop" >"$d/cox-plan.log" 2>&1 || true
	cox history "$loop" >"$d/cox-history.log" 2>&1 || true
	log evidence "$loop: evidence kept in $d"
}

delivered_state() { # <loop> -> "STATUS REASON PRNUMBER PRURL"
	kc -n "$NS" get loop "$1" -o json | python3 -c '
import json,sys
d=json.load(sys.stdin)
s=d.get("status") or {}
del_=""
for c in s.get("conditions", []):
    if c.get("type")=="Delivered":
        del_=c.get("status","")+" "+(c.get("reason","") or c.get("message",""))
dv=s.get("delivery") or {}
print(" ".join([del_, str(dv.get("prNumber","") or ""), dv.get("prURL","") or ""]).strip())'
}

wait_delivered() { # <loop> <cap-seconds>
	local loop="$1" cap="${2:-900}" i=0
	while :; do
		local dstate
		dstate=$(delivered_state "$loop")
		case "$dstate" in
		True*) return 0 ;;
		False*DeliveryFailed* ) return 2 ;;
		esac
		i=$((i + 5)); [ "$i" -gt "$cap" ] && return 1
		sleep 5
	done
}

policy_enforced() { # <loop> -> "STATUS REASON"
	kc -n "$NS" get loop "$1" -o json | python3 -c '
import json,sys
d=json.load(sys.stdin)
for c in (d.get("status") or {}).get("conditions", []):
    if c.get("type")=="PolicyEnforced":
        print((c.get("status","")+" "+(c.get("reason","") or c.get("message",""))).strip()); break
else: print("absent")'
}

# The plan bytes from the workspace PVC (a read-only peek pod, the A2/A6
# pattern — the operator recycles the sandbox pod at phase boundaries, so the
# PVC is the only place the plan's exact bytes are readable).
peek_pvc_file() { # <loop> <relpath> <outfile>
	local loop="$1" rel="$2" out="$3"
	cat >"$EVIDENCE/peek-$loop.yaml" <<EOF
apiVersion: v1
kind: Pod
metadata:
  name: b6-peek-$loop
  namespace: $NS
spec:
  restartPolicy: Never
  containers:
  - name: peek
    image: $AGENT_IMG
    command: ["sh","-c","cat /workspace/${rel} 2>/dev/null || echo '<missing>'"]
    volumeMounts:
    - name: ws
      mountPath: /workspace
  volumes:
  - name: ws
    persistentVolumeClaim:
      claimName: ${loop}-workspace
EOF
	kc apply -f "$EVIDENCE/peek-$loop.yaml" >/dev/null 2>&1 || true
	local i
	for i in $(seq 1 40); do
		local st
		st=$(kc -n "$NS" get pod "b6-peek-$loop" -o jsonpath='{.status.phase}' 2>/dev/null || echo "")
		[ "$st" = "Succeeded" ] && break
		[ "$st" = "Failed" ] && break
		sleep 3
	done
	kc -n "$NS" logs "b6-peek-$loop" >"$out" 2>&1 || true
	kc -n "$NS" delete pod "b6-peek-$loop" --wait=false >/dev/null 2>&1 || true
}

# ===========================================================================
# 1+2. Manual approval: reject the first plan with a reason, verify the
#      re-plan's plan-feedback carries the reason; approve the second plan;
#      the Loop reaches Succeeded and opens a DRAFT PR (live GitHub, D55).
# ===========================================================================
MANUAL_LOOP="b6-manual-$TS"
log step1 "applying $MANUAL_LOOP (approval.mode: Manual — the first plan will be REJECTED with a reason)"
render_loop "$MANUAL_LOOP" '  approval:
    mode: Manual
    onReject: Replan
  checkpoints: Required'
wait_phase "$MANUAL_LOOP" AwaitingApproval 900 || die "step1: $MANUAL_LOOP did not reach AwaitingApproval (recorded in $EVIDENCE/loop-$MANUAL_LOOP.yaml — collect and stop)"
log step1 "first plan written; the Loop is held at AwaitingApproval (B3 gate live)"
# The first plan, for the record:
peek_pvc_file "$MANUAL_LOOP" ".coxswain/PLAN.md" "$EVIDENCE/step1-plan-1.md"
# Reject with a reason (B3: the operator applies onReject: Replan — the
# default — and writes the reason into .coxswain/plan-feedback for the
# re-plan's prompt).
cox reject "$MANUAL_LOOP" --reason "$REJECT_REASON" >"$EVIDENCE/step1-reject.txt" 2>&1 || die "step1: cox reject failed"
log step1 "plan REJECTED with the reason (recorded in $EVIDENCE/step1-reject.txt): $REJECT_REASON"
# The operator re-enters Planning with the reason in plan-feedback; wait for
# the RE-PLAN to land (a new plan + a new AwaitingApproval hold).
log step1 "waiting for the re-plan (Planning -> a second AwaitingApproval hold)"
i=0
while :; do
	ph=$(phase_of "$MANUAL_LOOP")
	log step1 "t=${i}s phase=$ph (waiting for the re-plan's hold)"
	case "$ph" in
	AwaitingApproval) break ;;
	Succeeded | Failed) die "step1: $MANUAL_LOOP reached $ph before the re-plan (recorded — stop)" ;;
	esac
	i=$((i + PHASE_POLL)); [ "$i" -gt "$PHASE_CAP" ] && die "step1: no re-plan hold in $PHASE_CAP s (last: $ph)"
	sleep "$PHASE_POLL"
done
# The re-plan's plan-feedback must contain the rejection reason (step 1's
# Done-when: "the re-plan's plan-feedback contains the reason").
peek_pvc_file "$MANUAL_LOOP" ".coxswain/plan-feedback" "$EVIDENCE/step1-plan-feedback.txt"
log step1 "re-plan landed (second AwaitingApproval hold); plan-feedback read from the workspace PVC"
cat "$EVIDENCE/step1-plan-feedback.txt"
if ! grep -qF "$REJECT_REASON" "$EVIDENCE/step1-plan-feedback.txt"; then
	peek_pvc_file "$MANUAL_LOOP" ".coxswain/PLAN.md" "$EVIDENCE/step1-plan-2.md"
	die "step1 FAILED: the re-plan's .coxswain/plan-feedback does NOT contain the rejection reason (I84: the step is not verified — see $EVIDENCE/step1-plan-feedback.txt)"
fi
log step1 "PASS: the re-plan's .coxswain/plan-feedback contains the rejection reason (B3's reason hand-off)"
# I42b/F4: the re-plan must actually differ from the original plan. A stale
# claim re-advance (the F4 defect) would produce an IDENTICAL plan (no new
# inference ran). Capture plan-2 and assert it differs from plan-1.
peek_pvc_file "$MANUAL_LOOP" ".coxswain/PLAN.md" "$EVIDENCE/step1-plan-2.md"
if diff -q "$EVIDENCE/step1-plan-1.md" "$EVIDENCE/step1-plan-2.md" >/dev/null 2>&1; then
	die "step1 FAILED: the re-plan is IDENTICAL to the original plan (no new inference ran — the stale Planning claim re-advanced; see $EVIDENCE/step1-plan-1.md and $EVIDENCE/step1-plan-2.md)"
fi
diff "$EVIDENCE/step1-plan-1.md" "$EVIDENCE/step1-plan-2.md" >"$EVIDENCE/step1-plan-diff.txt" 2>&1 || true
log step1 "PASS: the re-plan differs from the original plan (I42b/F4: the re-plan is a real re-plan, not a stale-claim re-advance; diff: $EVIDENCE/step1-plan-diff.txt)"
{
	echo "reject-reason: $REJECT_REASON"
	echo "plan-feedback-file: $EVIDENCE/step1-plan-feedback.txt"
	echo "plan-feedback contains the reason: yes (grep -qF matched)"
	echo "plan-1-file: $EVIDENCE/step1-plan-1.md"
	echo "plan-2-file: $EVIDENCE/step1-plan-2.md"
	echo "plan-1 != plan-2: yes (diff -q matched)"
} >"$EVIDENCE/step1-verify.txt"
# Approve the SECOND plan (B3: the CLI computes the plan's SHA-256 and sets
# PlanApproved with it; the operator advances to Implementing). The CLI's
# plan peek pod mounts the workspace PVC RO and `cat`s the plan file to
# compute its hash; the PVC must be Bound before the peek pod can schedule.
# The B6 run 5's first approve failed because the peek pod went Pending
# (the PVC was still being provisioned when the reject triggered the
# re-plan). Wait for the PVC to be Bound before approving.
log step2 "approving the second plan (waiting for the workspace PVC to be Bound first — the CLI's plan peek pod mounts it)"
for i in $(seq 1 60); do
	PVC_STATUS=$(kubectl --context "$CTX" -n "beta-e2e" get pvc "${MANUAL_LOOP}-workspace" -o jsonpath='{.status.phase}' 2>/dev/null || echo "")
	[ "$PVC_STATUS" = "Bound" ] && break
	sleep 2
done
[ "$PVC_STATUS" = "Bound" ] || { kubectl --context "$CTX" -n beta-e2e get pvc >"$EVIDENCE/step2-pvc-stuck.txt"; die "step2: the workspace PVC is not Bound after 120s (the CLI's plan peek pod cannot schedule — see $EVIDENCE/step2-pvc-stuck.txt)"; }
cox approve "$MANUAL_LOOP" >"$EVIDENCE/step2-approve.txt" 2>&1 || die "step2: cox approve failed"
log step2 "second plan APPROVED; watching to a terminal phase"
watch_loop "$MANUAL_LOOP" "$EVIDENCE/step2-phase.log" || die "step2: $MANUAL_LOOP wedged (see $EVIDENCE/step2-phase.log — recorded, not guessed)"
MANUAL_PHASE=$(phase_of "$MANUAL_LOOP")
[ "$MANUAL_PHASE" = "Succeeded" ] || { collect_evidence "$MANUAL_LOOP" manual-wedge; die "step2 FAILED: $MANUAL_LOOP ended $MANUAL_PHASE (not Succeeded) — see $EVIDENCE/manual-wedge"; }
log step2 "Succeeded (phase trail: $EVIDENCE/step2-phase.log)"
# PolicyEnforced must be True on real evidence (the KubeArmor BPF-LSM gate is
# what the main run proves end to end — A6's core, carried into beta).
PE=$(policy_enforced "$MANUAL_LOOP")
log step2 "PolicyEnforced: $PE"
case "$PE" in
True*) : ;;
*) die "step2: Succeeded but PolicyEnforced='$PE' (the BPF-LSM gate was not enforced on real evidence — see $EVIDENCE/step2-phase.log)";;
esac
# The live draft PR (D55): wait for Delivered=True and capture the PR URL.
wait_delivered "$MANUAL_LOOP" 900 || { collect_evidence "$MANUAL_LOOP" manual-delivery; die "step2: delivery did not complete (see $EVIDENCE/manual-delivery)"; }
PR_URL=$(k get loop "$MANUAL_LOOP" -o jsonpath='{.status.delivery.prURL}' 2>/dev/null || echo "")
PR_NUM=$(k get loop "$MANUAL_LOOP" -o jsonpath='{.status.delivery.prNumber}' 2>/dev/null || echo "")
BRANCH=$(k get loop "$MANUAL_LOOP" -o jsonpath='{.status.delivery.branch}' 2>/dev/null || echo "")
[ -n "$PR_URL" ] || { collect_evidence "$MANUAL_LOOP" step2-nopr; die "step2: Delivered but no PR URL in status.delivery (I84: the PR URL is the evidence the step 5 independence check compares against — an empty PR_URL would make the step-5 PR-independence check pass vacuously; see $EVIDENCE/step2-nopr)"; }
[ -n "$BRANCH" ] || { collect_evidence "$MANUAL_LOOP" step2-nobranch; die "step2: Delivered but no branch in status.delivery (I84: the branch is the evidence the step 5 independence check compares against — an empty BRANCH would make the step-5 branch-independence check pass vacuously; see $EVIDENCE/step2-nobranch)"; }
{
	echo "loop: $MANUAL_LOOP"
	echo "pr-number: $PR_NUM"
	echo "pr-url: $PR_URL"
	echo "branch: $BRANCH (I81: unique per Loop instance)"
} >"$EVIDENCE/step2-delivery.txt"
log step2 "PASS: approved the second plan; the Loop reached Succeeded and opened a DRAFT PR on GitHub (the live path, D55): $PR_URL (branch $BRANCH)"
collect_evidence "$MANUAL_LOOP" step2

# ===========================================================================
# 3. A forced verify failure re-plans (onVerifyFail: Plan). The acceptance
#    check is the literal `false` (the forced failure — it cannot pass), so
#    the FIRST verify fails; onVerifyFail: Plan must send the Loop back to
#    Planning (B4), not back to Implementing.
# ===========================================================================
REPLAN_LOOP="b6-replan-$TS"
log step3 "applying $REPLAN_LOOP (acceptance check '$FORCED_CHECK' — the FORCED verify failure; onVerifyFail: Plan must re-plan; approval.mode: Auto so the re-plans run unattended and the Loop ends in a terminal Failed when the budget is exhausted — the step proves the re-plan, not the outcome)"
cat >"$EVIDENCE/loop-$REPLAN_LOOP.yaml" <<EOF
apiVersion: coxswain.wattu.com/v1alpha1
kind: Loop
metadata:
  name: $REPLAN_LOOP
  namespace: $NS
spec:
  goal: >-
    ${RENDER_GOAL}
  workspace:
    repo: "$REPO"
    ref: "main"
    gitCredentialSecret: git-cred
  verify:
    acceptanceChecks:
      - "$FORCED_CHECK"
    onVerifyFail: Plan
    image: $AGENT_IMG
  agent:
    endpointSecretRef: $MODEL_SECRET
    modelEndpoint: $VLLM_ENDPOINT
    model: $VLLM_MODEL
  approval:
    mode: Auto
  delivery:
    mode: PullRequest
    baseBranch: main
    branchPrefix: coxswain/
  loop:
    maxIterations: 3
    stallAfter: 100
    stallAction: Fail
EOF
kc apply -f "$EVIDENCE/loop-$REPLAN_LOOP.yaml" >/dev/null
# Record the applied spec (the evidence for what the Loop actually runs with):
k get loop "$REPLAN_LOOP" -o jsonpath='{.spec.verify}' >"$EVIDENCE/step3-spec-verify.txt"
log step3 "applied spec.verify: $(cat "$EVIDENCE/step3-spec-verify.txt")"
# The loop: Planning -> Implementing -> Verifying -> (verify fails, the check
# is 'false') -> Planning (RE-PLANNING, not Implementing). We observe: (a)
# the Loop reaches Implementing at least once (the verify actually ran); (b)
# after the failed verify the phase goes back to PLANNING (B4). A re-entry
# to Implementing WITHOUT a Planning pass in between would be the default
# (Implement) behaviour — the thing this step proves is NOT happening.
# Poll at 2s: the phases can be short on the stub-free real model; the trail
# file is the I76 record (the operator's own phase field, read each poll).
log step3 "watching: wait for Implementing, then for the post-failure re-plan (Verifying -> Planning); the Loop ends in a terminal Failed when the 3-iteration budget is exhausted (the forced check never passes)"
SAW_IMPLEMENTING=0
SAW_PLANNING_AFTER_VERIFY=0
: >"$EVIDENCE/step3-trail.log"
i=0
while :; do
	ph=$(phase_of "$REPLAN_LOOP")
	iter=$(k get loop "$REPLAN_LOOP" -o jsonpath='{.status.iteration}' 2>/dev/null || echo "")
	printf '%s ph=%s iter=%s\n' "$(utc)" "$ph" "$iter" >>"$EVIDENCE/step3-trail.log"
	[ "$ph" != "${prev_ph:-}" ] && log step3 "phase change: ${prev_ph:-<start>} -> $ph (iter=$iter, t=${i}s)"
	prev_ph="$ph"
	case "$ph" in
	Implementing) SAW_IMPLEMENTING=1 ;;
	esac
	if [ "$ph" = "Planning" ] && [ "$SAW_IMPLEMENTING" = "1" ]; then
		# Planning after at least one Implementing pass: a re-plan (the first
		# Planning pass never coexists with SAW_IMPLEMENTING=1).
		SAW_PLANNING_AFTER_VERIFY=1
		log step3 "observed the re-plan: Planning after Implementing (the onVerifyFail: Plan path)"
		# Keep watching to the terminal outcome (the budget ends it Failed).
		case "$(phase_of "$REPLAN_LOOP")" in Planning) : ;; esac
	fi
	case "$ph" in
	Succeeded) die "step3 FAILED: the Loop Succeeded despite the forced verify failure (the check is 'false' — impossible; the run is wrong) — record and stop" ;;
	Failed) [ "$SAW_PLANNING_AFTER_VERIFY" = "1" ] && break ;;
	esac
	i=$((i + 2)); [ "$i" -gt "$PHASE_CAP" ] && die "step3: no terminal or re-plan phase in $PHASE_CAP s (last: $ph)"
	sleep 2
done
[ "$SAW_PLANNING_AFTER_VERIFY" = "1" ] || { collect_evidence "$REPLAN_LOOP" step3-noreplan; die "step3 FAILED: no post-Implementing Planning pass was observed (the onVerifyFail: Plan re-plan did not happen — see $EVIDENCE/step3-trail.log; I84: not verified)"; }
# The operator's own record: the loop JSON (the stallHistory — one entry per
# failed verify — is the operator's record of the forced failures) + the
# events + the phase trail this script observed.
k get loop "$REPLAN_LOOP" -o json >"$EVIDENCE/step3-loop.json" 2>/dev/null || true
{
	echo "saw-Implementing: $SAW_IMPLEMENTING"
	echo "saw-Planning-after-verify: $SAW_PLANNING_AFTER_VERIFY"
	echo "final-phase: $(phase_of "$REPLAN_LOOP") (a terminal Failed is EXPECTED — the forced check never passes; the step proves the re-plan, not the outcome)"
} >"$EVIDENCE/step3-observe.txt"
kc -n "$NS" get events -o json --field-selector "involvedObject.name=$REPLAN_LOOP" >"$EVIDENCE/step3-events.json" 2>/dev/null || true
# The operator-side corroboration: at least one failed verify recorded (the
# stallHistory is the operator's own ring of failed-verify iterations) and a
# verify-failure event in the trail. The phase trail (the script's 2s poll of
# the operator's status.phase) is the primary evidence; this is the second.
FAILS_RECORDED=$(python3 -c 'import json,sys
d=json.load(open(sys.argv[1]))
sh=(d.get("status") or {}).get("stallHistory") or []
print(len(sh))' "$EVIDENCE/step3-loop.json" 2>/dev/null || echo 0)
log step3 "the operator's stallHistory records $FAILS_RECORDED failed verify(ies); the observed phase trail is in $EVIDENCE/step3-trail.log"
if [ "$SAW_PLANNING_AFTER_VERIFY" = "1" ] && [ "$FAILS_RECORDED" -ge 1 ] 2>/dev/null; then
	log step3 "PASS: the forced verify failure (acceptance check 'false') re-planned with onVerifyFail: Plan (B4) — observed: Planning after Implementing (the trail) + the operator's own $FAILS_RECORDED recorded failed verify(ies) (the stallHistory)"
else
	collect_evidence "$REPLAN_LOOP" step3-weak
	die "step3: the re-plan was observed in the phase trail but the operator's stallHistory recorded $FAILS_RECORDED failed verify(ies) (expected >= 1) — the evidence is weaker than it should be; see $EVIDENCE/step3-weak (I84: not fully verified)"
fi
collect_evidence "$REPLAN_LOOP" step3

# ===========================================================================
# 4. The PVC is deleted mid-Implementing; the Loop restores it from its last
#    checkpoint (B2). The Loop must have a checkpoint first (a successful
#    verify takes one — checkpoints: Required makes the absence a failure,
#    not a silent fallback, D59).
# ===========================================================================
# Step 4's recipe (reviewer-proven on kind, .samples/rv154b-1792027909):
# the Loop runs acceptanceChecks ["false"] (every verify FAILS; the step is
# about the restore, not the agent) with loop {maxIterations: 4, stallAction:
# Continue} and onVerifyFail: Implement (the default iterate path). A FAILED
# verify still records a checkpoint (ensureCheckpointStep runs after
# applyVerifyOutcome and is not gated on the verify outcome — the reviewer's
# trail shows checkpoint-<loop>-iter-0 recorded after the first failed verify,
# with status.iteration already advanced): the restore's "last checkpoint" is
# the iter-0 snapshot after the PVC delete, the restore hold keeps the Loop in
# Implementing while it recreates the PVC, and a LATER verify pass (the
# operator's own record of the iteration having advanced past the restore) is
# the proof the Loop carried on. The final Failed:MaxIterationsExceeded after
# 4 iterations is EXPECTED (as in step 3) — the forced check never passes.
RESTORE_LOOP="b6-restore-$TS"
log step4 "applying $RESTORE_LOOP (checkpoints: Required; acceptanceChecks [\"false\"]; loop {maxIterations: 4, stallAction: Continue}; onVerifyFail: Implement — a failed verify still records a checkpoint, and the restore's continuation is what is asserted)"
cat >"$EVIDENCE/loop-$RESTORE_LOOP.yaml" <<EOF
apiVersion: coxswain.wattu.com/v1alpha1
kind: Loop
metadata:
  name: $RESTORE_LOOP
  namespace: $NS
spec:
  goal: >-
    ${RENDER_GOAL}
  workspace:
    repo: "$REPO"
    ref: "main"
    gitCredentialSecret: git-cred
  verify:
    acceptanceChecks:
      - "$FORCED_CHECK"
    image: $AGENT_IMG
  agent:
    endpointSecretRef: $MODEL_SECRET
    modelEndpoint: $VLLM_ENDPOINT
    model: $VLLM_MODEL
  delivery:
    mode: PullRequest
    baseBranch: main
    branchPrefix: coxswain/
  checkpoints: Required
  loop:
    maxIterations: 4
    stallAction: Continue
EOF
kc apply -f "$EVIDENCE/loop-$RESTORE_LOOP.yaml" >/dev/null
k get loop "$RESTORE_LOOP" -o jsonpath='{.spec.verify}{" \\n"}{.spec.loop}' >"$EVIDENCE/step4-spec.txt"
log step4 "applied spec (verify + loop): $(tr '\n' ' ' < "$EVIDENCE/step4-spec.txt")"
# The pre-delete PVC uid (the restore must REPLACE the PVC with a NEW uid;
# a same-uid object proves the operator just re-labeled it, not restored it).
PVC_NAME="$RESTORE_LOOP-workspace"
# Wait for the preconditions the reviewer's recipe proved: a checkpoint
# recorded (status.checkpoints non-empty) AND the phase in Implementing (the
# iterate path, with the sandbox pod mounting the workspace PVC). The
# checkpoint's iteration is the restore's source (the last checkpoint
# existing at delete time — the reviewer's evidence: iter-0 recorded after
# the first failed verify, status.iteration already at 2).
log step4 "watching for status.checkpoints non-empty AND phase==Implementing (the PVC delete must land after the checkpoint exists and while the sandbox mounts the PVC)"
: >"$EVIDENCE/step4-trail.log"
i=0
while :; do
	CK_LIST=$(k get loop "$RESTORE_LOOP" -o jsonpath='{.status.checkpoints[*].name}' 2>/dev/null || echo "")
	ph=$(phase_of "$RESTORE_LOOP")
	iter=$(k get loop "$RESTORE_LOOP" -o jsonpath='{.status.iteration}' 2>/dev/null || echo "")
	printf '%s phase=%s iter=%s cps=%s\n' "$(utc)" "$ph" "$iter" "${CK_LIST:-[]}" >>"$EVIDENCE/step4-trail.log"
	[ $((i % 8)) -eq 0 ] && log step4 "t=${i}s phase=$ph iter=$iter checkpoints=[${CK_LIST:-}]."
	[ -n "$CK_LIST" ] && [ "$ph" = "Implementing" ] && break
	case "$ph" in
	Failed) die "step4: $RESTORE_LOOP reached Failed before a checkpoint+Implementing window (trail: $EVIDENCE/step4-trail.log — recorded, not guessed)" ;;
	esac
	i=$((i + PHASE_POLL)); [ "$i" -gt "$PHASE_CAP" ] && { cat "$EVIDENCE/step4-trail.log"; die "step4: no checkpoint+Implementing window in $PHASE_CAP s (last: ph=$ph cps=[${CK_LIST:-}])"; }
	sleep "$PHASE_POLL"
done
CK_PRE_DELETE=$(k get loop "$RESTORE_LOOP" -o jsonpath='{.status.checkpoints[-1:].name}' 2>/dev/null || echo "")
PVC_UID_BEFORE=$(kc -n "$NS" get pvc "$PVC_NAME" -o jsonpath='{.metadata.uid}' 2>/dev/null || echo "")
log step4 "preconditions met: checkpoint [$(echo "$CK_LIST" | tr '\n' ' ')] (last: $CK_PRE_DELETE) and the Loop is in Implementing — deleting the workspace PVC NOW (B2: the restore must recreate it from the last checkpoint)"
log step4 "the PVC delete lands while the sandbox mounts it (mid-Implementing, by construction — the precondition above)"
kc -n "$NS" delete pvc "$PVC_NAME" --wait=false >"$EVIDENCE/step4-pvc-delete.log" 2>&1 || true
# The operator's B2 path (reviewer-verified on kind, #154's fix): the lost
# PVC is detected, the Restore condition goes False/StoppingWorkloads and then
# True/Restored, the workspace PVC is RECREATED from the last checkpoint
# (spec.dataSource.name = checkpoint-<loop>-iter-N — a NEW uid), and the Loop
# carries on (a later verify run at a higher iteration; the phase trail shows
# the Implementing -> Verifying pass after the restore). Watch to the terminal
# outcome; the FINAL phase must be Failed (MaxIterationsExceeded) — the
# forced check never passes, as in step 3 (that is expected and is said so in
# the PASS log; it is NOT a failure of the step).
log step4 "PVC deleted; watching for the restore + continuation to the terminal outcome (a final Failed:MaxIterationsExceeded is EXPECTED — the forced check never passes)"
: >"$EVIDENCE/step4-trail-after.log"
i=0
while :; do
	ph=$(phase_of "$RESTORE_LOOP")
	iter=$(k get loop "$RESTORE_LOOP" -o jsonpath='{.status.iteration}' 2>/dev/null || echo "")
	CK_LIST=$(k get loop "$RESTORE_LOOP" -o jsonpath='{.status.checkpoints[*].name}' 2>/dev/null || echo "")
	printf '%s phase=%s iter=%s cps=%s\n' "$(utc)" "$ph" "$iter" "${CK_LIST:-[]}" >>"$EVIDENCE/step4-trail-after.log"
	[ $((i % 8)) -eq 0 ] && log step4 "t=${i}s (post-delete) phase=$ph iter=$iter checkpoints=[${CK_LIST:-}]."
	case "$ph" in
	Succeeded) die "step4: $RESTORE_LOOP Succeeded — impossible (the forced check 'false' never passes; the run is wrong) — record and stop" ;;
	Failed) break ;;
	esac
	i=$((i + PHASE_POLL)); [ "$i" -gt "$PHASE_CAP" ] && { tail -20 "$EVIDENCE/step4-trail-after.log"; die "step4: no terminal phase in $PHASE_CAP s after the PVC delete (last: ph=$ph) — the restore did not bring the Loop back to a terminal outcome"; }
	sleep "$PHASE_POLL"
done
RESTORE_PHASE=$(phase_of "$RESTORE_LOOP")
# (1) The Restore condition: True, reason Restored (the operator's own record
# it restored the PVC from a checkpoint — not just that a PVC exists).
RESTORE_COND_STATUS=$(k get loop "$RESTORE_LOOP" -o jsonpath='{.status.conditions[?(@.type=="Restore")].status}' 2>/dev/null || echo "")
RESTORE_COND_REASON=$(k get loop "$RESTORE_LOOP" -o jsonpath='{.status.conditions[?(@.type=="Restore")].reason}' 2>/dev/null || echo "")
RESTORE_COND_MSG=$(k get loop "$RESTORE_LOOP" -o jsonpath='{.status.conditions[?(@.type=="Restore")].message}' 2>/dev/null || echo "")
log step4 "Restore condition: status=$RESTORE_COND_STATUS reason=$RESTORE_COND_REASON message=${RESTORE_COND_MSG:-absent}"
[ "$RESTORE_COND_STATUS" = "True" ] && [ "$RESTORE_COND_REASON" = "Restored" ] || {
	collect_evidence "$RESTORE_LOOP" restore-nocond
	die "step4 FAILED: the Restore condition is not True/Restored (got status=$RESTORE_COND_STATUS reason=$RESTORE_COND_REASON — see $EVIDENCE/restore-nocond)"
}
# (2) The new PVC: spec.dataSource.name must equal the checkpoint name and the
# uid must be NEW (a restore RECREATES the PVC from the snapshot; a same-uid
# object would be the operator re-labeling the old PVC, not restoring it).
kc -n "$NS" get pvc "$PVC_NAME" -o json >"$EVIDENCE/step4-pvc-after.json" 2>/dev/null || die "step4: the workspace PVC $PVC_NAME is ABSENT after the restore (the restore did not recreate it)"
PVC_UID_AFTER=$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1])).get("metadata",{}).get("uid",""))' "$EVIDENCE/step4-pvc-after.json")
PVC_DS_NAME=$(python3 -c 'import json,sys; print((json.load(open(sys.argv[1])).get("spec",{}) or {}).get("dataSource",{}).get("name",""))' "$EVIDENCE/step4-pvc-after.json")
PVC_PHASE=$(python3 -c 'import json,sys; print((json.load(open(sys.argv[1])).get("status",{}) or {}).get("phase",""))' "$EVIDENCE/step4-pvc-after.json")
log step4 "the restored PVC $PVC_NAME: uid=$PVC_UID_AFTER (was $PVC_UID_BEFORE) dataSource.name=$PVC_DS_NAME phase=$PVC_PHASE"
[ "$PVC_UID_AFTER" != "$PVC_UID_BEFORE" ] || die "step4 FAILED: the PVC after the restore has the SAME uid ($PVC_UID_AFTER) as before the delete — the restore did not recreate the PVC from the checkpoint (see $EVIDENCE/step4-pvc-after.json)"
[ "$PVC_DS_NAME" != "" ] || die "step4 FAILED: the restored PVC has no spec.dataSource.name (it was not created FROM a VolumeSnapshot — see $EVIDENCE/step4-pvc-after.json)"
case "$CK_PRE_DELETE" in
*"$PVC_DS_NAME"*) : ;; # the restored-from checkpoint existed BEFORE the delete (retention GCs non-kept checkpoints DURING the run — e.g. iter-0 is GC'd after iter-3's checkpoint exists — so it is NOT necessarily in the post-terminal status.checkpoints; asserting against the post-terminal list would be a script bug, not an operator defect)
*) die "step4 FAILED: the restored PVC's dataSource.name ($PVC_DS_NAME) was not one of the checkpoints that existed before the delete ([${CK_PRE_DELETE:-}] — the restore picked a checkpoint that did not exist; see $EVIDENCE/step4-pvc-after.json)" ;;
esac
case "$PVC_DS_NAME" in
checkpoint-"$RESTORE_LOOP"-iter-*) : ;;
*) die "step4 FAILED: the restored PVC's dataSource.name ($PVC_DS_NAME) is not a checkpoint of $RESTORE_LOOP (expected checkpoint-$RESTORE_LOOP-iter-N — see $EVIDENCE/step4-pvc-after.json)" ;;
esac
# (3) The Loop carried on past the restore: the phase trail after the delete
# must show the phase REACHING Verifying (a later verify ran — the iteration
# advanced past the restore hold; the reviewer's evidence: Implementing ->
# Verifying -> a new checkpoint at a higher iteration), OR a NEW checkpoint at
# an iteration higher than the pre-delete one must exist. Either proves the
# restore hold released and the Loop continued its phase machine. (The trail
# check is primary: a later Verifying phase proves the phase machine kept
# running after the restore hold, which is exactly what "carried on" means.
# The new-checkpoint check is the fallback proof, in case the 4s poll missed
# a short Verifying window.)
CK_NEW=$(grep -E 'phase=(Verifying|Succeeded|Failed).*' "$EVIDENCE/step4-trail-after.log" | head -1 || true)
CK_NEW_2=$(python3 -c '
import json,sys,subprocess
# the highest iteration among the recorded checkpoints, read fresh
out=subprocess.run(["kubectl","--context","kind-coxswain-snap","-n","beta-e2e","get","loop","'$RESTORE_LOOP'","-o","json"],capture_output=True,text=True).stdout
try: d=json.loads(out)
except Exception: print(-1); raise SystemExit
cps=(d.get("status") or {}).get("checkpoints") or []
print(max([int(c.get("iteration",-1)) for c in cps], default=-1))' 2>/dev/null || echo -1)
PRE_DELETE_ITER=$(python3 -c '
import json,sys,subprocess
out=subprocess.run(["kubectl","--context","kind-coxswain-snap","-n","beta-e2e","get","loop","'$RESTORE_LOOP'","-o","json"],capture_output=True,text=True).stdout
try: d=json.loads(out)
except Exception: print(-1); raise SystemExit
print(max([int(c.get("iteration",-1)) for c in ((d.get("status") or {}).get("checkpoints") or [])], default=-1))' 2>/dev/null || echo -1)
log step4 "continuation check: saw a post-delete phase in {Verifying,Succeeded,Failed} trail: ${CK_NEW:-<none>}; highest checkpoint iteration now: $CK_NEW_2 (pre-delete checkpoints went up to $PRE_DELETE_ITER)"
if [ -z "$CK_NEW" ] && [ "${CK_NEW_2:--1}" -le "${PRE_DELETE_ITER:--1}" ] 2>/dev/null; then
	collect_evidence "$RESTORE_LOOP" restore-nocarry
	die "step4 FAILED: the Loop did not carry on past the restore (no post-delete Verifying phase in the trail AND no new checkpoint at a higher iteration — see $EVIDENCE/step4-trail-after.log, $EVIDENCE/restore-nocarry)"
fi
# (4) The expected terminal outcome: Failed (the forced check never passes;
# the budget of 4 iterations exhausts the Loop). Say so, as in step 3.
log step4 "final phase: $RESTORE_PHASE — a final Failed (MaxIterationsExceeded) is EXPECTED: the forced check 'false' never passes and the 4-iteration budget exhausted it (as in step 3; the step proves the restore + continuation, not the outcome)"
# (5) The operator's own corroboration: the Restore events (count per reason)
# and the operator log (zero 'forbidden' — the B2 restore path reads the
# snapshot and recreates the PVC with no RBAC hole; #154's evidence form).
kc -n "$NS" get events -o json --field-selector "involvedObject.name=$RESTORE_LOOP" >"$EVIDENCE/step4-events.json" 2>/dev/null || true
python3 -c '
import json,sys,collections
evs=json.load(open(sys.argv[1])).get("items") or []
c=collections.Counter()
for e in evs:
    if (e.get("reason") or "").startswith("Restore") or "Restore=" in (e.get("message") or ""):
        c[e.get("reason","?" or "?")]+=1
print("restore-event-counts:", dict(c))' "$EVIDENCE/step4-events.json" >"$EVIDENCE/step4-restore-events.txt" 2>/dev/null || true
cat "$EVIDENCE/step4-restore-events.txt"
kc -n coxswain-system logs deploy/coxswain-controller-manager >"$EVIDENCE/step4-operator-logs.log" 2>&1 || true
FORBIDDEN=$(grep -c forbidden "$EVIDENCE/step4-operator-logs.log" || true)
[ -n "$FORBIDDEN" ] && [ "$FORBIDDEN" -gt 0 ] 2>/dev/null && FORBIDDEN=$FORBIDDEN || FORBIDDEN=0
log step4 "operator log 'forbidden' count: $FORBIDDEN (must be 0: the restore path reads the snapshot + recreates the PVC without an RBAC hole — #154's evidence form)"
[ "$FORBIDDEN" -eq 0 ] || die "step4 FAILED: the operator log carries $FORBIDDEN 'forbidden' error(s) during the run (see $EVIDENCE/step4-operator-logs.log)"
{
	echo "checkpoints-pre-delete: $CK_LIST (last: $CK_PRE_DELETE)"
	echo "pvc-uid-before: $PVC_UID_BEFORE"
	echo "pvc-uid-after: $PVC_UID_AFTER (NEW: the restore recreated the PVC)"
	echo "pvc-dataSource.name: $PVC_DS_NAME (the checkpoint the PVC was restored from)"
	echo "pvc-phase: $PVC_PHASE"
	echo "restore-condition: status=$RESTORE_COND_STATUS reason=$RESTORE_COND_REASON message=${RESTORE_COND_MSG:-absent}"
	echo "final-phase: $RESTORE_PHASE (a final Failed is EXPECTED — the forced check never passes)"
	echo "operator-forbidden-count: $FORBIDDEN"
} >"$EVIDENCE/step4-verify.txt"
log step4 "PASS: the PVC was deleted mid-Implementing (after its checkpoint existed) and the Loop restored it from the last checkpoint (Restore True/Restored; the new PVC's spec.dataSource.name=$PVC_DS_NAME with a NEW uid $PVC_UID_AFTER) and CARRIED ON (post-delete phase trail: $EVIDENCE/step4-trail-after.log; highest checkpoint iteration $CK_NEW_2) — the final Failed (MaxIterationsExceeded) is EXPECTED (the forced check never passes)"
collect_evidence "$RESTORE_LOOP" step4

# ===========================================================================
# 5. `cox fork --from iter-N` (the fork CLI: `cox fork <name> --loop
#    <source> --iteration N`) produces an independent Loop that also
#    succeeds (B2: its own PVC, branch, PR). Fork from the manual Loop's
#    final checkpoint (it Succeeded — its last checkpoint is the one after
#    the successful verify).
# ===========================================================================
# Step 5 forks from STEP 4's checkpoint (the handoff's instruction: the fork
# seeds from the Loop that just proved the restore, from the iteration the
# reviewer's recipe names). Step 4's Loop is Failed (the forced check never
# passes) but its checkpoints survive on the Loop object until it is cleaned
# up in the tidy phase, so the fork reads the fork source (the step-4 Loop)
# and seeds from its last recorded checkpoint. The fork runs a PASSING check
# (the default make -n docs), so the fork SUCCEEDS (unlike the source) and
# opens its OWN draft PR (a URL different from step 2's).
FORK_SRC="$RESTORE_LOOP"
SRC_ITER=$(k get loop "$FORK_SRC" -o jsonpath='{.status.checkpoints[-1:].iteration}' 2>/dev/null || echo "")
SRC_SNAP=$(k get loop "$FORK_SRC" -o jsonpath='{.status.checkpoint.name}' 2>/dev/null || echo "")
[ -n "$SRC_ITER" ] && [ -n "$SRC_SNAP" ] || { collect_evidence "$FORK_SRC" fork-nosrc; die "step5: the fork source $FORK_SRC (step 4's Loop) has no recorded checkpoint (status.checkpoint absent — see $EVIDENCE/fork-nosrc)"; }
log step5 "fork source: $FORK_SRC (step 4's Loop, its final Failed phase is EXPECTED and does not block the fork — its checkpoints are recorded on the Loop object and survive until the tidy phase deletes the Loop)"
# The source snapshot must be ReadyToUse before the fork starts (the operator
# defers the restore otherwise — a long poll the fork does not need to
# sit in). Wait for it.
i=0
while :; do
	ready=$(kc -n "$NS" get volumesnapshot "$SRC_SNAP" -o jsonpath='{.status.readyToUse}' 2>/dev/null || echo "")
	[ "$ready" = "true" ] && { log step5 "the source snapshot $SRC_SNAP is ReadyToUse (iteration $SRC_ITER)"; break; }
	i=$((i + 5)); [ "$i" -gt 300 ] && die "step5: the source snapshot $SRC_SNAP is not ReadyToUse in 300s"
	sleep 5
done
FORK_LOOP="b6-fork-$TS"
log step5 "forking: cox fork $FORK_LOOP --loop $FORK_SRC --iteration $SRC_ITER --check 'make -n docs' (the QUICKSTART step-7 form of 'fork --from iter-N' with --check: the fork INHERITS the source's spec (goal, agent, delivery, loop, checkpoints) and the source's verify (preset, protectedPaths), but the --check flag REPLACES spec.verify.acceptanceChecks with a PASSING check ('make -n docs') — the fork SUCCEEDS where the source's forced 'false' check cannot)"
cox fork "$FORK_LOOP" --loop "$FORK_SRC" --iteration "$SRC_ITER" --check "make -n docs" >"$EVIDENCE/step5-fork.txt" 2>&1 || { cat "$EVIDENCE/step5-fork.txt"; die "step5: cox fork failed"; }
log step5 "the fork Loop $FORK_LOOP is applied (spec.forkOf loop=$FORK_SRC iteration=$SRC_ITER; spec.verify.acceptanceChecks=[make -n docs] (the --check override); the rest of verify (preset, protectedPaths) is inherited from the source); watching to a terminal phase"
watch_loop "$FORK_LOOP" "$EVIDENCE/step5-phase.log" || die "step5: the fork $FORK_LOOP wedged (see $EVIDENCE/step5-phase.log — recorded, not guessed)"
FORK_PHASE=$(phase_of "$FORK_LOOP")
[ "$FORK_PHASE" = "Succeeded" ] || { collect_evidence "$FORK_LOOP" fork-wedge; die "step5 FAILED: the fork $FORK_LOOP ended $FORK_PHASE (not Succeeded) — see $EVIDENCE/fork-wedge"; }
# The fork's Restore condition: True/Restored (the operator restored the
# fork's OWN PVC from the source's checkpoint — the fork's independence is
# proven by the restore + its own branch + its own PR, all three asserted).
FORK_RESTORE_STATUS=$(k get loop "$FORK_LOOP" -o jsonpath='{.status.conditions[?(@.type=="Restore")].status}' 2>/dev/null || echo "")
FORK_RESTORE=$(k get loop "$FORK_LOOP" -o jsonpath='{.status.conditions[?(@.type=="Restore")].message}' 2>/dev/null || echo "")
[ "$FORK_RESTORE_STATUS" = "True" ] || { collect_evidence "$FORK_LOOP" fork-norrestore; die "step5 FAILED: the fork's Restore condition is not True (got status=$FORK_RESTORE_STATUS message=${FORK_RESTORE:-absent} — the fork's PVC was not restored from the source's checkpoint; see $EVIDENCE/fork-norrestore)"; }
log step5 "the fork's Restore condition is True (reason $(k get loop "$FORK_LOOP" -o jsonpath='{.status.conditions[?(@.type=="Restore")].reason}' 2>/dev/null || echo '?')) — the operator restored the fork's OWN PVC from the source's snapshot $SRC_SNAP (iteration $SRC_ITER)"
# Independence: the fork has its OWN branch (a different I81 branch name than
# step 2's Loop's delivery) — ADR-0003. The source here is step 2's Loop
# (the fork's branch must differ from the STEP 2 Loop's branch — the fork is
# an independent Loop, its own branch and PR, not a re-delivery of step 2).
# The fork's branch + PR are read AFTER wait_delivered (the delivery must
# complete before the branch/PR are set on status.delivery; reading them
# before wait_delivered would yield an empty string and the independence
# checks would pass vacuously — the I84 hollow-assertion finding from the
# run 16 review). The step-2 BRANCH and PR_URL are asserted non-empty in
# step 2 (a guard added in this fix), so the comparison below is against
# real values, not empty strings.
wait_delivered "$FORK_LOOP" 900 || { collect_evidence "$FORK_LOOP" fork-delivery; die "step5: the fork's delivery did not complete (see $EVIDENCE/fork-delivery)"; }
FORK_BRANCH=$(k get loop "$FORK_LOOP" -o jsonpath='{.status.delivery.branch}' 2>/dev/null || echo "")
FORK_PR=$(k get loop "$FORK_LOOP" -o jsonpath='{.status.delivery.prURL}' 2>/dev/null || echo "")
[ -n "$FORK_BRANCH" ] || { collect_evidence "$FORK_LOOP" fork-nobranch; die "step5: the fork Delivered but has no branch in status.delivery (I84: an empty FORK_BRANCH would make the branch-independence check pass vacuously; see $EVIDENCE/fork-nobranch)"; }
[ -n "$FORK_PR" ] || { collect_evidence "$FORK_LOOP" fork-nopr; die "step5: the fork Delivered but has no PR URL (independence not proven — I84)"; }
[ "$FORK_BRANCH" != "$BRANCH" ] || die "step5 FAILED: the fork's branch ($FORK_BRANCH) equals step 2's branch — the fork is not independent (ADR-0003)"
[ "$FORK_PR" != "$PR_URL" ] || die "step5 FAILED: the fork's PR URL ($FORK_PR) equals step 2's PR URL — the fork did not open its OWN draft PR (the handoff's step-5 PASS condition requires a URL different from step 2's)"
{
	echo "source: $FORK_SRC iteration=$SRC_ITER snapshot=$SRC_SNAP"
	echo "fork: $FORK_LOOP"
	echo "fork-branch: $FORK_BRANCH (independent of the source's $BRANCH)"
	echo "fork-pr: $FORK_PR"
	echo "fork-restore: ${FORK_RESTORE:-absent}"
} >"$EVIDENCE/step5-verify.txt"
log step5 "PASS: cox fork --from iter-$SRC_ITER produced an independent Loop ($FORK_LOOP) that Succeeded with its own branch ($FORK_BRANCH) and its own PR ($FORK_PR)"
collect_evidence "$FORK_LOOP" step5

# ===========================================================================
# 6. The Loop metrics scraped with a coxswain-metrics-reader token:
#    loops_completed_total{result="succeeded"} >= 2 and
#    verify_results_total{result="pass"} >= 2 (B5a). The endpoint is the
#    operator's secure port 8443 (coxswain-controller-manager-metrics-service),
#    behind the standard kubebuilder authn/authz filter: a TokenReview +
#    SubjectAccessReview per request. The token belongs to a ServiceAccount
#    bound ONLY to coxswain-metrics-reader (no admin, no other role) — the
#    Done-when's "scraped via a coxswain-metrics-reader token".
# ===========================================================================
log step6 "creating a ServiceAccount in $NS bound ONLY to coxswain-metrics-reader (ClusterRoleBinding: /metrics is a nonResourceURL which a RoleBinding cannot grant), and minting its token"
kc -n "$NS" create serviceaccount b6-metrics-sa >"$EVIDENCE/step6-sa.log" 2>&1 || { cat "$EVIDENCE/step6-sa.log"; die "step6: could not create the metrics ServiceAccount"; }
kc create clusterrolebinding "b6-metrics-reader-crb-$TS" \
	--clusterrole=coxswain-metrics-reader \
	--serviceaccount="$NS:b6-metrics-sa" >"$EVIDENCE/step6-crb.log" 2>&1 || { cat "$EVIDENCE/step6-crb.log"; die "step6: could not create the ClusterRoleBinding (the /metrics nonResourceURL requires cluster scope)"; }
kc -n "$NS" create token b6-metrics-sa --duration=1h >"$EVIDENCE/step6-token.txt" 2>&1 || die "step6: could not create the metrics-reader token"
# D55/I84: the token is NEVER written to the evidence. It goes straight from
# kubectl's output into a 600-mode temp file (OUTSIDE the evidence dir) that
# kubectl reads via --from-file to build the scrape pod's Secret; the temp
# file and the kubectl output file are both deleted immediately. Only the
# token's presence (a non-empty line) is recorded — never its value.
[ -s "$EVIDENCE/step6-token.txt" ] || die "step6: the token file is empty"
TOKEN_TMP="$(mktemp)"; chmod 600 "$TOKEN_TMP"
cat "$EVIDENCE/step6-token.txt" >"$TOKEN_TMP"
kc -n "$NS" create secret generic b6-metrics-secret --from-file=tok="$TOKEN_TMP" --dry-run=client -o yaml | kc apply -f - >/dev/null
rm -f "$TOKEN_TMP" "$EVIDENCE/step6-token.txt"
_SECTYPE=$(kc -n "$NS" get secret b6-metrics-secret -o jsonpath='{.type}' 2>/dev/null || echo "?")
_SECKEYS=$(kc -n "$NS" get secret b6-metrics-secret -o go-template='{{range $k, $v := .data}}{{$k}} {{end}}' 2>/dev/null || echo "?")
log step6 "token minted and loaded into the b6-metrics-secret Secret via --from-file (the token value is never written to the evidence; Secret: b6-metrics-secret type=$_SECTYPE keys=$_SECKEYS)"
ACTIVE_LOOP="b6-active-$TS"
kc -n "$NS" apply -f - >/dev/null <<EOF
apiVersion: coxswain.wattu.com/v1alpha1
kind: Loop
metadata:
  name: $ACTIVE_LOOP
  namespace: $NS
spec:
  goal: "echo b6-active"
  workspace:
    repo: "https://github.com/papawattu/coxswain-sandbox.git"
    ref: main
    gitCredentialSecret: git-cred
  agent:
    modelEndpoint: "$VLLM_ENDPOINT"
    model: "$VLLM_MODEL"
    endpointSecretRef: beta-model-creds
  approval:
    mode: Manual
EOF
log step6 "applied $ACTIVE_LOOP (approval.mode=Manual) to make coxswain_loops_active deterministic; waiting for AwaitingApproval"
i=0
while :; do
	_ph=$(kc -n "$NS" get loop "$ACTIVE_LOOP" -o jsonpath='{.status.phase}' 2>/dev/null || echo "")
	case "$_ph" in AwaitingApproval) break ;; esac
	i=$((i + 2)); [ "$i" -gt 120 ] && { kc -n "$NS" get loop "$ACTIVE_LOOP" -o yaml >"$EVIDENCE/step6-active-loop.yaml"; die "step6: $ACTIVE_LOOP did not reach AwaitingApproval in 120s (phase $_ph)"; }
	sleep 2
done
log step6 "$ACTIVE_LOOP is at AwaitingApproval (a non-terminal phase the loops_active collector will see)"

# The scrape pod curls the secure endpoint with the token (mounted from the
# Secret) and streams the full /metrics body to stdout — the pod's log IS the
# scrape (I84: the bytes are kept where the claim reads them).
# The scrape pod curls the secure endpoint with the token (mounted from the
# Secret) and streams the full /metrics body to stdout — the pod's log IS the
# scrape (I84: the bytes are kept where the claim reads them).
# The token is at /tok/tok (NOT /tok): a Secret volume is a directory with one
# file per key, and the key is 'tok' (from --from-file=tok=...).
# The command is a YAML block literal (|): the shell runs the multi-line
# command as-is, with $(cat /tok/tok) and $? expanded by the pod's shell at
# runtime (not by the script's shell — the heredoc is quoted <<'EOF').
cat >"$EVIDENCE/step6-scrape.yaml" <<'EOF'
apiVersion: v1
kind: Pod
metadata:
  name: b6-metrics-scrape
  namespace: $NS
spec:
  restartPolicy: Never
  containers:
  - name: scrape
    image: docker.io/curlimages/curl:latest
    command:
    - /bin/sh
    - -c
    - |
      curl -skS --max-time 30 --header "Authorization: Bearer $(cat /tok/tok)" \
        https://coxswain-controller-manager-metrics-service.coxswain-system.svc:8443/metrics \
        -o /tmp/metrics.txt -w 'SCRAPE_HTTP=%{http_code}\n'
      r=$?
      echo "SCRAPE_RC=$r"
      wc -c </tmp/metrics.txt
      echo METRICS_BEGIN
      cat /tmp/metrics.txt
      echo METRICS_END
    volumeMounts:
    - name: tok
      mountPath: /tok
      readOnly: true
  volumes:
  - name: tok
    secret:
      secretName: b6-metrics-secret
EOF
sed -i "s/\$NS/${NS}/g" "$EVIDENCE/step6-scrape.yaml"
kc apply -f "$EVIDENCE/step6-scrape.yaml" >/dev/null
i=0
while :; do
	st=$(kc -n "$NS" get pod b6-metrics-scrape -o jsonpath='{.status.phase}' 2>/dev/null || echo "")
	case "$st" in Succeeded | Failed) break ;; esac
	i=$((i + 2)); [ "$i" -gt 120 ] && die "step6: the scrape pod did not finish in 120s (state $st)"
	sleep 2
done
kc -n "$NS" logs b6-metrics-scrape >"$EVIDENCE/step6-scrape.log" 2>&1 || true
kc -n "$NS" delete pod b6-metrics-scrape --wait=false >/dev/null 2>&1 || true
# I84: assert SCRAPE_HTTP=200 (NOT SCRAPE_RC=0 — curl exits 0 on a 403
# without -f, so SCRAPE_RC=0 passes vacuously on a 403; the I84 hollow
# assertion). The 200 proves the token was accepted by the /metrics
# endpoint (the RBAC the token carries is exactly coxswain-metrics-reader
# — no admin). The ClusterRoleBinding (not a RoleBinding) is required
# because /metrics is a nonResourceURL.
grep -q "SCRAPE_HTTP=200" "$EVIDENCE/step6-scrape.log" || { cat "$EVIDENCE/step6-scrape.log"; die "step6: the metrics endpoint did not return 200 to the coxswain-metrics-reader token (see $EVIDENCE/step6-scrape.log; the ClusterRoleBinding may be missing or the token expired)"; }
# SCRAPE_RC=0 is implied by SCRAPE_HTTP=200 but assert it explicitly for the evidence trail.
grep -q "SCRAPE_RC=0" "$EVIDENCE/step6-scrape.log" || { cat "$EVIDENCE/step6-scrape.log"; die "step6: the metrics scrape failed (see $EVIDENCE/step6-scrape.log)"; }
# The metric lines (between the BEGIN/END markers) are the scrape itself.
sed -n '/^METRICS_BEGIN$/,/^METRICS_END$/p' "$EVIDENCE/step6-scrape.log" | sed '1d;$d' >"$EVIDENCE/step6-metrics.log"
grep -q "^coxswain_" "$EVIDENCE/step6-metrics.log" || { cat "$EVIDENCE/step6-scrape.log"; die "step6: the scrape carried no coxswain_ metric lines (see $EVIDENCE/step6-scrape.log)"; }
log step6 "the metrics endpoint answered the coxswain-metrics-reader token (HTTP 200; the RBAC the token carries is exactly coxswain-metrics-reader — no admin)"
# coxswain_loops_active is a scrape-time collector (loops_active_collector.go)
# that counts Loops per NON-TERMINAL phase from the cache at scrape time.
# At step 6, all of THIS run's Loops are terminal (Succeeded/Failed), so the
# metric is absent by design. To make it deterministic: apply a small
# approval.mode=Manual Loop, wait for AwaitingApproval (a non-terminal phase),
# scrape (the collector sees the AwaitingApproval Loop and emits the metric),
# then delete that Loop. The scrape must show
# coxswain_loops_active{phase="AwaitingApproval"} >= 1.

# I84: a validation asserts the CONTENT'S MEANING, not just that it parses.
# The handoff's step 6: the coxswain_* metrics must EXIST in the scrape with
# a SENSIBLE value (not absent, not NaN, and where the run's own Loops should
# have bumped them, actually > 0). The named metrics (B5a):
#   coxswain_loop_phase_transitions_total  (counter by destination phase)
#   coxswain_loop_iterations_total         (counter, iteration increments)
#   coxswain_verify_results_total          (counter by result pass|fail|infra)
#   coxswain_loops_completed_total         (counter by result + reason)
#   coxswain_loops_active                  (gauge by phase, scrape-time)
# The first four were bumped by THIS run's own Loops; coxswain_loops_active is
# made deterministic by the $ACTIVE_LOOP Loop (AwaitingApproval) applied above.
METRICS_CHECK=$(python3 - "$EVIDENCE/step6-metrics.log" <<'PY'
import sys
lines = open(sys.argv[1]).read().splitlines()
def val(name, labels=None):
    for l in lines:
        if labels is not None:
            if l.startswith(name + "{") and all(p in l for p in labels):
                try:
                    return float(l.rsplit(" ", 1)[1])
                except (ValueError, IndexError):
                    continue
        else:
            # Match labeled form (name{...} value) or unlabeled form (name value)
            if l.startswith(name + "{"):
                try:
                    return float(l.rsplit(" ", 1)[1])
                except (ValueError, IndexError):
                    continue
            if l.startswith(name + " ") and not l.startswith(name + "_"):
                try:
                    return float(l.rsplit(" ", 1)[1])
                except (ValueError, IndexError):
                    continue
    return None
ok = True
for name, want, labels in [
    ("coxswain_loop_phase_transitions_total", "present", None),
    ("coxswain_loop_iterations_total", " > 0", None),
    ("coxswain_verify_results_total", " > 0", None),
    ("coxswain_loops_completed_total", " > 0", None),
    ("coxswain_loops_active", " >= 1", ['phase="AwaitingApproval"']),
]:
    v = val(name, labels)
    if v is None:
        lbl = "{" + ",".join(labels) + "}" if labels else ""
        print(f"FAIL {name}{lbl}: ABSENT from the scrape (I84: the metric did not exist)")
        ok = False
        continue
    if want == " > 0" and not (v > 0):
        print(f"FAIL {name}: present but {v} (not > 0 — this run's Loops should have bumped it)")
        ok = False
        continue
    if want == " >= 1" and not (v >= 1):
        lbl = "{" + ",".join(labels) + "}" if labels else ""
        print(f"FAIL {name}{lbl}: present but {v} (not >= 1 — the active Loop should be counted)")
        ok = False
        continue
    lbl = "{" + ",".join(labels) + "}" if labels else ""
    print(f"OK   {name}{lbl}: {v} {want}")
print("RESULT:", "OK" if ok else "FAIL")
sys.exit(0 if ok else 1)
PY
) || { echo "$METRICS_CHECK"; cat "$EVIDENCE/step6-metrics.log" | grep -E '^coxswain_' ; die "step6 FAILED: one of the coxswain_* metrics is absent or not > 0 (see above; I84: the scrape's content, not just its parse, is the assertion)"; }
# Delete the active Loop now (the scrape is done; the metric was captured).
kc -n "$NS" delete loop "$ACTIVE_LOOP" --wait=false >/dev/null 2>&1 || true
log step6 "the coxswain_* metrics are present with sensible values (including coxswain_loops_active{phase=\"AwaitingApproval\"} >= 1 from the deterministic active Loop): $(printf '%s\n' "$METRICS_CHECK" | tr '\n' ' ')"
# Delete the metrics Secret now (the scrape is done; do not leave the token
# on the cluster — and never dump it to the evidence).
kc -n "$NS" delete secret b6-metrics-secret >/dev/null 2>&1 || true
log step6 "the metrics endpoint answered the coxswain-metrics-reader token (the /metrics scrape, RBAC: coxswain-metrics-reader only — no admin)"
# The two Done-when assertions:
SUCCEEDED_COUNT=$(grep -E '^coxswain_loops_completed_total\{[^}]*result="succeeded"[^}]*\}' "$EVIDENCE/step6-metrics.log" | awk '{print $NF}' | head -1 || echo "")
PASS_COUNT=$(grep -E '^coxswain_verify_results_total\{[^}]*result="pass"[^}]*\}' "$EVIDENCE/step6-metrics.log" | awk '{print $NF}' | head -1 || echo "")
# The metric lines are in the log (keep them as the evidence):
grep -E '^coxswain_(loops_completed_total|verify_results_total)' "$EVIDENCE/step6-metrics.log" >"$EVIDENCE/step6-assert-lines.txt" || true
{
	echo "loops_completed_total{result=\"succeeded\"}: ${SUCCEEDED_COUNT:-0} (required >= 2: the manual Loop + the fork Loop, both Succeeded)"
	echo "verify_results_total{result=\"pass\"}: ${PASS_COUNT:-0} (required >= 2: the manual Loop's successful verify + the fork Loop's successful verify)"
	echo "four-metrics-check: $(printf '%s' "$METRICS_CHECK" | tr '\n' ' ')"
	echo "assert-lines: $EVIDENCE/step6-assert-lines.txt"
} >"$EVIDENCE/step6-verify.txt"
cat "$EVIDENCE/step6-assert-lines.txt"
if [ "${SUCCEEDED_COUNT:-0}" -ge 2 ] 2>/dev/null && [ "${PASS_COUNT:-0}" -ge 2 ] 2>/dev/null; then
	log step6 "PASS: loops_completed_total{result=\"succeeded\"}=$SUCCEEDED_COUNT (>= 2) and verify_results_total{result=\"pass\"}=$PASS_COUNT (>= 2) — scraped with the coxswain-metrics-reader token (B5a)"
else
	die "step6 FAILED: the metrics do not meet the Done-when (loops_completed_total{result=\"succeeded\"}=${SUCCEEDED_COUNT:-absent} needs >= 2; verify_results_total{result=\"pass\"}=${PASS_COUNT:-absent} needs >= 2 — see $EVIDENCE/step6-metrics.log; I84: the step is not verified)"
fi

# ---------------------------------------------------------------------------
# Tidy: delete the Loops (their PRs live on GitHub; the cluster is this run's
# scratch cluster — the owner said to delete/recreate it, so leave it FRESH-
# built and empty of Loops for the next run; do NOT delete the cluster here,
# so the owner can inspect it).
# ---------------------------------------------------------------------------
for L in "$MANUAL_LOOP" "$REPLAN_LOOP" "$RESTORE_LOOP" "$FORK_LOOP"; do
	kc -n "$NS" delete loop "$L" --wait=false >/dev/null 2>&1 || true
done
kc -n "$NS" delete secret b6-metrics-secret >/dev/null 2>&1 || true
kc delete clusterrolebinding "b6-metrics-reader-crb-$TS" >/dev/null 2>&1 || true
kc -n "$NS" delete serviceaccount b6-metrics-sa >/dev/null 2>&1 || true
# shellcheck disable=SC1010 # 'done' here is the log tag, not the keyword
log done "Loops deleted (their PRs live on GitHub); the scratch cluster $CLUSTER is kept for inspection (the owner deletes it when done)"

# ---------------------------------------------------------------------------
# D55 leak guard: fail if any evidence file contains the decoded or base64
# GitHub token. Names only, never content.
# ---------------------------------------------------------------------------
_d55_guard() {
	local tok tok_b64 f
	tok=$(tr -d '\n' < "$TOKEN_FILE" 2>/dev/null) || return 0
	[ -n "$tok" ] || return 0
	tok_b64=$(printf '%s' "$tok" | base64 | tr -d '\n')
	local found=0
	for f in $(grep -rlF "$tok_b64" "$EVIDENCE"/ 2>/dev/null; grep -rlF "$tok" "$EVIDENCE"/ 2>/dev/null); do
		[ -n "$f" ] || continue
		log FATAL "D55 LEAK GUARD: evidence file $f contains the GitHub token (base64 or decoded)"
		found=1
	done
	if [ "$found" = 1 ]; then
		die "leak guard: at least one evidence file contains a credential (named above, content NOT printed). Shred them and fix the script."
	fi
	log d55 "leak guard: no evidence file contains the GitHub token — D55 holds"
}
_d55_guard

echo
echo "=== B6 beta-e2e summary ($EVIDENCE) ==="
echo "cluster: $CLUSTER (FRESH, snapshot-capable: hostpath SC + hostpath-snapclass + external-snapshotter + upstream csi-driver-host-path)"
echo "install: current main build $BUILD_TAG (install.yaml sha256 $INSTALL_SHA256) + the D38 escape hatch --allow-unenforced-network (kindnet; the KubeArmor BPF-LSM gate remains live)"
echo "model: $VLLM_ENDPOINT (model $VLLM_MODEL; vllm-preflight: $VLLM_STATE; in-cluster reachability PROBE_RC=0 in reach-eth0.log)"
echo "step1 (reject + reason in re-plan feedback): PASS (plan-feedback grep -qF matched the reason; $EVIDENCE/step1-verify.txt)"
echo "step2 (approve second plan -> Succeeded + draft PR): PASS (PR $PR_URL, branch $BRANCH; PolicyEnforced=$PE)"
echo "step3 (forced verify failure re-plans, onVerifyFail: Plan): PASS ($EVIDENCE/step3-observe.txt)"
echo "step4 (PVC deleted mid-Implementing -> restored from last checkpoint): PASS (Restore condition: status=$RESTORE_COND_STATUS reason=$RESTORE_COND_REASON)"
echo "step5 (cox fork --from iter-$SRC_ITER -> independent Succeeded Loop): PASS (fork PR $FORK_PR, branch $FORK_BRANCH)"
echo "step6 (metrics via coxswain-metrics-reader token): PASS (loops_completed_total{result=\"succeeded\"}=$SUCCEEDED_COUNT >= 2; verify_results_total{result=\"pass\"}=$PASS_COUNT >= 2)"
echo "digests:"
cat "$EVIDENCE/digests.txt"
echo
# shellcheck disable=SC1010 # 'done' here is the log tag, not the keyword
log done "DONE: B6 beta acceptance (all six Done-when steps) on the FRESH snapshot-capable kind cluster $CLUSTER, from the CURRENT main build, following docs/QUICKSTART.md. Live GitHub (D55), real vLLM ($VLLM_ENDPOINT, $VLLM_MODEL). Evidence: $EVIDENCE (never committed, I68). kind-coxswain-dev / kind-coxswain-calico / kind-coxswain-alpha were never touched. No release defect was worked around in this script (I90); the owner tags v0.1.0 after this passes."
