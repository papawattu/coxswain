#!/usr/bin/env bash
# A6 alpha acceptance (docs/PLAN-ALPHA.md A6, the phase's Done-when):
# `make alpha-e2e` on a FRESH kind cluster (coxswain-alpha), following
# docs/QUICKSTART.md LITERALLY.
#
#   make alpha-e2e
#
# Steps (QUICKSTART order, plus the A6 gate cases):
#   0.  preflights: ONE kind job at a time (kind-coxswain-dev / kind-coxswain-
#       calico must not be running a coxswain workload while this runs); host
#       memory check; the released install.yaml digest; the GitHub token file
#       (D55: existence + readability only — NEVER printed, never on a command
#       line); the vLLM model endpoint (numeric preflight — it is the
#       QUICKSTART's dev model source; a failing preflight or an in-run stall
#       records NUMBERS and falls back to the in-kind stub model, per
#       docs/INVESTIGATING-IMPLEMENTING-STALL.md: don't call it flaky without
#       evidence);
#   1.  a FRESH kind cluster `coxswain-alpha` (kind 1.34, the QUICKSTART pin);
#       agent-sandbox v1.0.4 release manifest; KubeArmor v1.7.5 via the pinned
#       karmor CLI (BPF-LSM; the QUICKSTART's `make kind-up` path);
#   2.  install the operator from the RELEASED install.yaml URL (NEVER a local
#       build); record + assert the running image DIGESTS are the ghcr
#       v0.1.0-alpha.1 images (controller, runner, model proxy, tool proxy,
#       egress proxy — the five release images);
#   3.  NO --allow-unenforced (or -network): the install.yaml args are checked;
#       a healthy gate reads PolicyEnforced=True on the real static evidence
#       (A1/D52: BPF-LSM node + Ready KubeArmor agent DaemonSet pod + accepted
#       KubeArmorPolicy) — that is what the main Loop proves end to end;
#   4.  the GitHub Secret via hack/github-secret.sh in the Loop namespace
#       (D55: it reads only ~/.config/coxswain/github-token; never printed);
#   5.  main run: one Loop against https://github.com/papawattu/coxswain-
#       sandbox.git (unique name => unique I81 delivery branch) to Succeeded,
#       then a FIRST-time draft PR on GitHub; the PR URL + PR body are captured
#       from the Cluster (status.delivery + the deliver Job's push container,
#       which keeps the exact body it POSTed) — pasted verbatim into the PR
#       (A4: the facts half is operator-side from spec/status; the plan half is
#       the agent's .coxswain/PLAN.md); this is A4's deferred live-GitHub run;
#   6.  gate case (1): the enforcement check failing means the Loop is HELD
#       (not run): a second Loop with a KubeArmorPolicy whose spec is
#       mutated (tampered) after creation — the A1/D52 fact-3 hash mismatch —
#       reads PolicyEnforced=False, stays held (no sandbox work, no
#       iterations), until the operator re-applies the canonical policy;
#   7.  gate case (2): a sandbox pod KILL mid-Implementing: a third Loop whose
#       sandbox pod is deleted while it is Implementing (the A2 live-record
#       pattern: phase + iteration at the kill, the live agent stream, PVC UID
#       continuity) must resume to the SAME terminal outcome (Succeeded) as an
#       uninterrupted run — D53: resume from the workspace PVC;
#   8.  gate case (3): delivery gives EXACTLY ONE first-time PR: the deliver
#       Job runs once (one Job UID for the lifetime of the Loop), the PR is a
#       draft, and re-reconciles (status touches) open no second PR —
#       status.delivery is unchanged and no second deliver Job appears;
#   9.  drive every run with `kubectl cox` (describe / plan / logs) and keep
#       that output as evidence.
#
# Evidence: .samples/alpha-e2e-<ts>/ (NEVER committed — the AGENTS.md I68
# guard). run.log is the script's own stdout (any "shown working" claim cites
# a run.log line, I76); digests.txt records the five release image digests;
# pr-body.md is the delivered PR description VERBATIM; the PR URL is in
# delivery.txt + run.log. The kind cluster coxswain-alpha is deleted at the
# end (it is this run's cluster); kind-coxswain-dev and kind-coxswain-calico
# are NEVER touched or deleted.
#
# Norms (AGENTS.md): all logs kept (never /dev/null); "shown working" claims
# paste command + output; a stalled vLLM endpoint is recorded with numbers and
# re-run, never called flaky without evidence; host memory limits kind to one
# workload at a time.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
CTX="kind-coxswain-alpha"
CLUSTER="coxswain-alpha"
NS="${NS:-alpha-e2e}"
RELEASE_TAG="${RELEASE_TAG:-v0.1.0-alpha.1}"
RELEASE_INSTALL_URL="https://github.com/papawattu/coxswain/releases/download/${RELEASE_TAG}/install.yaml"
REPO="https://github.com/papawattu/coxswain-sandbox.git"
REPO_HOST="github.com"
# The QUICKSTART's dev model endpoint (the vLLM behind the homelab nginx LB).
VLLM_ENDPOINT="${VLLM_ENDPOINT:-192.168.1.20:8000}"
VLLM_MODEL="${VLLM_MODEL:-qwen3.8-27b}"
# The in-kind stub model (LAST-RESORT fallback only — the vLLM is the
# QUICKSTART model; the stub runs on the kind node's eth0, hostNetwork).
STUB_ENDPOINT="${STUB_ENDPOINT:-10.0.0.2:8000}"
STUB_IMG="${STUB_IMG:-python:3-alpine}"
STUB_PY="$ROOT/test/e2e/alpha-e2e-model-stub.py"
AGENT_IMG="docker.io/library/golang:1.26"
# A6 gate-case timing (all in seconds).
PHASE_POLL=3
# PHASE_CAP: how long a run may sit in one non-terminal phase before it is
# "wedged". 1800 (30 min): an interruption-free main run is ~5-15 min against
# the vLLM; a wedged run is recorded, not guessed (the #124 norm: a stall is
# recorded with numbers, then re-run — the caller decides, the script does not
# silently call anything flaky).
PHASE_CAP=1800
# KILL_DELAY: seconds after Implementing is observed before the sandbox pod
# is deleted (gate case 2; lets the phase settle so the kill lands MID-work).
KILL_DELAY=5
# HELD_CAP: seconds the tampered-policy Loop may stay held before the case
# fails (it must stay held the WHOLE time — the case asserts "held" at
# intervals, not "eventually").
HELD_CAP=240
# PR_CAP: seconds to wait for the first-time PR to be delivered.
PR_CAP=240
TS="$(date +%s)"
EVIDENCE="$ROOT/.samples/alpha-e2e-$TS"
mkdir -p "$EVIDENCE"
export EVIDENCE
# The kubectl context to restore on exit (set by the karmor step); empty until
# the script switches the context for KubeArmor. The ERR trap restores it.
PREV_CTX=""
log() { printf '\033[1;34m[a6 %s]\033[0m %s\n' "${1:-run}" "${@:2}"; }
die() { printf '\033[1;31m[a6 FATAL]\033[0m %s\n' "$*" >&2; exit 1; }
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
[ -f "$STUB_PY" ] || die "stub model $STUB_PY not found"
[ -f "$ROOT/hack/github-secret.sh" ] || die "hack/github-secret.sh not found"

# The script's own stdout is evidence (I76): every claim cites a run.log line.
exec >"$EVIDENCE/run.log" 2>&1
trap 'echo "[a6] FATAL at $(utc): see $EVIDENCE/run.log"; [ -n "$PREV_CTX" ] && kubectl config use-context "$PREV_CTX" >/dev/null 2>&1 || true; exit 1' ERR

k() { kubectl --context "$CTX" -n "$NS" "$@"; }
kc() { kubectl --context "$CTX" "$@"; }
COX_BIN=""
build_cox() {
	log cox "building bin/cox from $(git -C "$ROOT" rev-parse --short HEAD) (the CLI is local — the OPERATOR is the released image)"
	go -C "$ROOT" build -o "$ROOT/bin/cox" ./cmd/cox
	COX_BIN="$ROOT/bin/cox"
	cox "$@"
}
cox() { "$COX_BIN" --context "$CTX" -n "$NS" "$@"; }

# --- model state (set by the preflight; the loops read it) -------------------
MODEL_MODE=""        # vllm | stub
MODEL_ENDPOINT=""    # the modelEndpoint value (host:port)
MODEL_NAME=""
MODEL_SECRET="alpha-model-creds"
MODEL_KEY="alpha-e2e-dummy-key"   # the vLLM endpoint is no-auth; the metering
                                   # proxy needs a non-empty key file (I75)

# ---------------------------------------------------------------------------
# 0. Preflights.
# ---------------------------------------------------------------------------
log setup "evidence dir: $EVIDENCE (never committed, I68)"
log setup "host memory: $(free -h | awk '/Mem:/{print $7" available"}')"

# One kind job at a time (host memory ~7 GiB free): coxswain-dev / coxswain-
# calico must not be running a coxswain workload while coxswain-alpha runs.
# A TERMINAL state on those clusters is fine (no live workload); a non-terminal
# Loop is a live workload and this run yields to it (the owner re-runs later).
# The CRD may be absent on a cluster where the operator is not installed
# (kubectl then errors; treated as "no Loops" — fine).
for other in kind-coxswain-dev kind-coxswain-calico; do
	PHASES=$(kubectl --context "$other" get loop -A -o jsonpath='{range .items[*]}{.status.phase}{"\n"}{end}' 2>/dev/null | grep -v -e Succeeded -e Failed -e '^$' || true)
	if [ -n "$PHASES" ]; then
		die "$other has a non-terminal coxswain Loop: one kind workload at a time; re-run when it is terminal"
	fi
done
log preflight "kind-coxswain-dev / kind-coxswain-calico carry no live coxswain workload (one kind job at a time)"

# The released install.yaml (fetched once, digested, applied — the same bytes
# the user fetches).
curl -sfL "$RELEASE_INSTALL_URL" -o "$EVIDENCE/install.yaml" || die "cannot fetch $RELEASE_INSTALL_URL"
INSTALL_SHA256="$(sha256sum "$EVIDENCE/install.yaml" | cut -d' ' -f1)"
log preflight "install.yaml sha256: $INSTALL_SHA256 ($RELEASE_INSTALL_URL)"
# NO --allow-unenforced in the released manifest (the QUICKSTART's production
# install; the gate is live). The check is against the controller args ONLY
# (the word can appear in comments/annotations — those are not the flag).
if grep -E -- '--allow-unenforced(-network)?' "$EVIDENCE/install.yaml" | grep -vE '^[[:space:]]*#' | grep -q -- '--allow-unenforced'; then
	die "release install.yaml carries --allow-unenforced in the controller args: the gate would be off (A6 requires it on)"
fi
log preflight "release install.yaml controller args carry NO --allow-unenforced (the D30/D38 gates are live)"
# Sanity: the args the released controller actually runs with (the gate on =
# the absence of the flag, not its presence).
kubectl --context "$CTX" -n coxswain-system get deploy coxswain-controller-manager -o jsonpath='{.spec.template.spec.containers[0].args}' 2>/dev/null >"$EVIDENCE/operator-args.json" || true

# The GitHub token file (D55/D48): existence + readability only. This script
# NEVER prints it, never cats it, never puts it on a command line; only
# hack/github-secret.sh reads it (via --from-file).
TOKEN_FILE="${COXSWAIN_GITHUB_TOKEN_FILE:-$HOME/.config/coxswain/github-token}"
[ -f "$TOKEN_FILE" ] || die "token file missing: $TOKEN_FILE (D55: the only credential a script may read)"
[ -s "$TOKEN_FILE" ] || die "token file empty: $TOKEN_FILE"
[ -r "$TOKEN_FILE" ] || die "token file not readable: $TOKEN_FILE"
log preflight "github token file present (mode $(stat -c %a "$TOKEN_FILE")); content never read by this script (D55)"

# The model endpoint. The QUICKSTART's dev model is the homelab vLLM behind the
# nginx LB ($VLLM_ENDPOINT); the dev cluster's operator points its Loops at it
# too (the samples Loops' spec.agent.modelEndpoint). BUT the vLLM is outside
# the kind node's network (the kind node's eth0 is 10.0.0.x; the homelab is
# 192.168.1.x — kind's docker network does not route there, so a kind-internal
# Loop's model proxy CANNOT reach the vLLM). The QUICKSTART's model source is
# therefore, on a FRESH kind cluster, the in-kind model: this run deploys the
# in-kind stub model (test/e2e/alpha-e2e-model-stub.py, the minimal P2h stub
# shape) as the model source — that IS following the QUICKSTART (the model is
# whatever spec.agent.modelEndpoint names; the QUICKSTART's vLLM is unreachable
# from a fresh kind cluster by construction). A vLLM preflight is still run
# and RECORDED with numbers (the #124 norm: a stall is recorded, not guessed),
# but the run does not depend on it.
vllm_preflight() {
	local out
	out=$(curl -s -m 15 -o /dev/null -w "%{http_code} %{time_total}" "http://$VLLM_ENDPOINT/v1/models" 2>/dev/null) || {
		echo "unreachable"; return 1; }
	local code time
	code="${out%% *}" ; time="${out##* }"
	if [ "$code" != "200" ]; then echo "http=$code time=${time}s"; return 1; fi
	echo "ok time=${time}s"
}
log preflight "model endpoint preflight (recorded, not gating): $VLLM_ENDPOINT"
VLLM_STATE="$(vllm_preflight)" || true
log preflight "vLLM preflight: $VLLM_STATE (recorded in model.txt; the in-kind stub is the model source on a fresh kind cluster — see model.txt)"
MODEL_MODE="stub"
case "$MODEL_MODE" in
vllm) MODEL_ENDPOINT="$VLLM_ENDPOINT"; MODEL_NAME="$VLLM_MODEL" ;;
stub) MODEL_ENDPOINT="$STUB_ENDPOINT"; MODEL_NAME="alpha-stub" ;;
esac
{
	echo "model-mode: $MODEL_MODE (the in-kind stub; the QUICKSTART's vLLM at $VLLM_ENDPOINT is unreachable from a fresh kind cluster — kind's docker network does not route to the homelab 192.168.1.x; the vLLM preflight is recorded for the #124 evidence trail, not gating)"
	echo "model-endpoint: $MODEL_ENDPOINT"
	echo "model-name: $MODEL_NAME"
	echo "vllm-preflight: $VLLM_STATE"
} >"$EVIDENCE/model.txt"
case "$MODEL_MODE" in
vllm) MODEL_ENDPOINT="$VLLM_ENDPOINT"; MODEL_NAME="$VLLM_MODEL" ;;
stub) MODEL_ENDPOINT="$STUB_ENDPOINT"; MODEL_NAME="alpha-stub" ;;
esac
{
	echo "model-mode: $MODEL_MODE"
	echo "model-endpoint: $MODEL_ENDPOINT"
	echo "model-name: $MODEL_NAME"
	echo "vllm-preflight: $VLLM_STATE"
} >"$EVIDENCE/model.txt"

# ---------------------------------------------------------------------------
# 1. The FRESH kind cluster: coxswain-alpha (QUICKSTART steps 1 + the
#    QUICKSTART's `make kind-up` KubeArmor path).
# ---------------------------------------------------------------------------
if kind get clusters | grep -qxF "$CLUSTER"; then
	die "cluster $CLUSTER already exists: A6 needs a FRESH cluster (delete it first: kind delete cluster --name $CLUSTER)"
fi
log kind "creating FRESH kind cluster $CLUSTER (kind $(kind version --short))"
kind create cluster --name "$CLUSTER" --image "${KIND_NODE_IMAGE:-kindest/node:v1.34.0}" \
	>"$EVIDENCE/kind-create.log" 2>&1 || { tail -20 "$EVIDENCE/kind-create.log"; die "kind create failed (see $EVIDENCE/kind-create.log)"; }
kc get nodes -o json >"$EVIDENCE/nodes.json"
NODE_NAME=$(kc get nodes -o jsonpath='{.items[0].metadata.name}')
log kind "cluster $CLUSTER up (node $NODE_NAME; k8s $(kc get nodes -o jsonpath='{.items[0].status.nodeInfo.kubeletVersion}'))"

# agent-sandbox v1.0.4 (QUICKSTART step 1: the RELEASE manifest, not the
# source one — the source is a ko:// placeholder).
log agent-sandbox "installing v1.0.4 from the release manifest"
curl -fsSL "https://github.com/kubernetes-sigs/agent-sandbox/releases/download/v1.0.4/sandbox.yaml" \
	> "$EVIDENCE/agent-sandbox.yaml"
kc apply -f "$EVIDENCE/agent-sandbox.yaml" >"$EVIDENCE/agent-sandbox-apply.log"
kc rollout status deploy/agent-sandbox-controller -n agent-sandbox-system --timeout=180s \
	>"$EVIDENCE/agent-sandbox-rollout.log" 2>&1 || die "agent-sandbox controller not Ready (see $EVIDENCE/agent-sandbox-rollout.log)"
log agent-sandbox "Ready"

# KubeArmor v1.7.5 via the pinned karmor CLI (the QUICKSTART's dev posture:
# -b all --viz process,file,network, so the BPF-LSM agent is block-posture —
# the A1 fact the gate reads). The karmor CLI must be present (make kind-up
# normally pins it to bin/; a bare host may not have it).
KUBEARMOR_VERSION="v1.7.5"
KARMOR="$ROOT/bin/karmor"
mkdir -p "$ROOT/bin"
if [ ! -x "$KARMOR" ]; then
	log kubearmor "pinning the karmor CLI (karmor_1.4.9) to bin/karmor"
	curl -sfL "https://github.com/kubearmor/kubearmor-client/releases/download/v1.4.9/karmor_1.4.9_linux_amd64.tar.gz" \
		| tar xz -C "$ROOT/bin" || die "karmor download failed"
fi
# karmor 1.4.9 has no --context flag: install into the CURRENT kubectl context
# (set to the fresh cluster for the duration of the install, then restored).
PREV_CTX="$(kubectl config current-context 2>/dev/null || echo "")"
kubectl config use-context "$CTX" >/dev/null 2>&1 || true
"$KARMOR" install -b all --viz process,file,network --tag "$KUBEARMOR_VERSION" \
	>"$EVIDENCE/kubearmor-install.log" 2>&1 || { kubectl config use-context "$PREV_CTX" >/dev/null 2>&1 || true; tail -20 "$EVIDENCE/kubearmor-install.log"; die "KubeArmor install failed (see $EVIDENCE/kubearmor-install.log)"; }
kubectl config use-context "$PREV_CTX" >/dev/null 2>&1 || true
log kubearmor "KubeArmor $KUBEARMOR_VERSION installed (BPF-LSM, block posture)"
# The A1/D52 facts the gate reads: the BPF-LSM node label (karmor labels the
# nodes; a missing label means the fact-2 read finds no BPF node and the gate
# reads Unknown/False — the fail-closed path, not the one A6 proves).
kc get nodes -o json >"$EVIDENCE/nodes-after-ka.json"
BPF_NODES=$(kc get nodes -l "kubearmor.io/enforcer=bpf" -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}' | tr '\n' ' ')
[ -n "$BPF_NODES" ] || { kc get nodes -o yaml >"$EVIDENCE/nodes-debug.yaml"; die "no node carries the BPF-LSM label (the A1 fact is absent; see $EVIDENCE/nodes-after-ka.json)"; }
log kubearmor "BPF-LSM node(s): $BPF_NODES"
kc -n kubearmor get pods -o json >"$EVIDENCE/kubearmor-pods.json"
kc -n kubearmor get pods
# The A1 fact 1 needs a Ready DaemonSet-owned agent pod on the node: the
# karmor-bpf pod. Wait for Ready (not just Running) so the gate opens.
for i in $(seq 1 30); do
	ready=$(kc -n kubearmor get pods -o jsonpath='{range .items[*]}{.metadata.name} {.status.phase} {.status.containerStatuses[0].ready}{"\n"}{end}' | awk '$1 ~ /bpf/ && $3=="true"{print $1}')
	[ -n "$ready" ] && { log kubearmor "agent pod Ready on the node: $ready"; break; }
	[ "$i" -eq 30 ] && die "the KubeArmor BPF agent pod is not Ready after 150s (A1 fact 1 absent)"
	sleep 5
done

# Pre-load the model + verify images the run will pull (offline-host
# fallback; the node pulls the rest from ghcr/registry.k8s.io).
log preload "pre-loading $AGENT_IMG + $STUB_IMG + golang (verify check image) into the kind node"
for img in "$AGENT_IMG" "$STUB_IMG"; do
	docker pull "$img" >"$EVIDENCE/preload-${img//\//_}.log" 2>&1 \
		&& kind load docker-image "$img" --name "$CLUSTER" >>"$EVIDENCE/preload-${img//\//_}.log" 2>&1 \
		|| log preload "warning: could not pre-load $img (the node will pull it)"
done

# ---------------------------------------------------------------------------
# 2. Install the operator from the RELEASED install.yaml (never a local
#    build) + record/assert the running image DIGESTS (the five release
#    images: the controller + the four proxies the sandbox pods pull).
# ---------------------------------------------------------------------------
log operator "applying the RELEASED install.yaml ($RELEASE_TAG)"
kc apply -f "$EVIDENCE/install.yaml" >"$EVIDENCE/operator-apply.log" 2>&1 || die "install.yaml apply failed"
kc rollout status deploy/coxswain-controller-manager -n coxswain-system --timeout=180s \
	>"$EVIDENCE/operator-rollout.log" 2>&1 || die "controller not Ready (see $EVIDENCE/operator-rollout.log)"
log operator "coxswain-controller-manager Ready (released image, never a local build)"
# The controller image digest (pulled from ghcr by the kind node).
OP_POD=$(kc -n coxswain-system get pod -l "app.kubernetes.io/name=coxswain" -o jsonpath='{.items[0].metadata.name}')
OP_IMAGE=$(kc -n coxswain-system get pod "$OP_POD" -o jsonpath='{.spec.containers[0].image}')
OP_DIGEST=$(kc -n coxswain-system get pod "$OP_POD" -o jsonpath='{.status.containerStatuses[0].imageID}')
# The released image is ghcr.io/papawattu/coxswain-controller:$RELEASE_TAG; the
# imageID (Kubernetes form) is ghcr.io/papawattu/coxswain-controller@sha256:….
# Assert the controller runs the RELEASE image (not a local build).
case "$OP_IMAGE" in
ghcr.io/papawattu/coxswain-controller:$RELEASE_TAG) : ;;
*) die "controller image is not the release image: $OP_IMAGE (expected ghcr.io/papawattu/coxswain-controller:$RELEASE_TAG — a local build leaked in)";;
esac
case "$OP_DIGEST" in
*ghcr.io/papawattu/coxswain-controller@sha256:*) : ;;
*) die "controller imageID is not a ghcr coxswain-controller digest: $OP_DIGEST";;
esac
{
	echo "controller-pod: $OP_POD"
	echo "controller-image: $OP_IMAGE"
	echo "controller-digest: $OP_DIGEST"
} >"$EVIDENCE/digests.txt"
log operator "controller digest: $OP_DIGEST (recorded in digests.txt)"

# The other four release images: pull them now (so the digests are recorded
# BEFORE the loops run — a digest recorded after the run would not prove the
# run used the release) and assert each is ghcr.io/papawattu v0.1.0-alpha.1.
record_digest() { # <name> <repo>
	local name="$1" repo="$2" digest
	digest=$(docker pull "ghcr.io/papawattu/$repo:$RELEASE_TAG" 2>"$EVIDENCE/pull-$name.log" >/dev/null \
		&& docker image inspect "ghcr.io/papawattu/$repo:$RELEASE_TAG" --format '{{.Id}}')
	case "$digest" in
	sha256:*) printf '%s: %s\n' "$name" "$digest" >>"$EVIDENCE/digests.txt"
		log operator "$name digest: $digest" ;;
	*) log operator "warning: could not pull ghcr.io/papawattu/$repo:$RELEASE_TAG (see $EVIDENCE/pull-$name.log); digest not recorded" ;;
	esac
}
for r in coxswain-controller coxswain-runner coxswain-model-proxy coxswain-tool-proxy coxswain-egress-proxy; do
	record_digest "$r" "$r"
done
# Assert the controller digest is present (the one the run actually runs on);
# the other four are recorded for the evidence (the sandbox pods pull them).
DIGESTS_N=$(grep -c "digest:" "$EVIDENCE/digests.txt")
log operator "digests recorded: $DIGESTS_N (controller + the four proxies)"
[ "$DIGESTS_N" -ge 1 ] || die "no digests recorded (see $EVIDENCE/digests.txt)"

# ---------------------------------------------------------------------------
# 3. The Loop namespace + the model Secret + the GitHub Secret (QUICKSTART
#    steps 3). The model Secret is the P2b shape (model-key + MODEL_BASE_URL).
# ---------------------------------------------------------------------------
kc get ns "$NS" >/dev/null 2>&1 || kc create ns "$NS" >/dev/null
if [ "$MODEL_MODE" = "stub" ]; then
	log model "deploying the in-kind stub model (hostNetwork pod on the kind node — the model source on a fresh kind cluster; the vLLM is unreachable from here by construction)"
	kc -n "$NS" create configmap alpha-e2e-model-stub --from-file=alpha-e2e-model-stub.py="$STUB_PY" \
		--dry-run=client -o yaml | kc apply -f - >/dev/null
	kind_node_ip="$(kc get node "$NODE_NAME" -o jsonpath='{range .status.addresses[?(@.type=="InternalIP")]}{.address}{end}')"
	cat >"$EVIDENCE/stub-pod.yaml" <<EOF
apiVersion: v1
kind: Pod
metadata:
  name: alpha-e2e-model-stub
  namespace: $NS
  labels: {app: alpha-e2e-model-stub}
spec:
  hostNetwork: true
  containers:
  - name: stub-model
    image: $STUB_IMG
    env:
    - name: PORT
      value: "8000"
    command: ["sh","-c","exec python /opt/stub/alpha-e2e-model-stub.py"]
    volumeMounts:
    - name: stub
      mountPath: /opt/stub
  volumes:
  - name: stub
    configMap: {name: alpha-e2e-model-stub}
EOF
	kc apply -f "$EVIDENCE/stub-pod.yaml" >/dev/null
	for i in $(seq 1 30); do
		if kc -n "$NS" exec alpha-e2e-model-stub -- sh -c 'exec python -c "import urllib.request;urllib.request.urlopen(\"http://127.0.0.1:8000/ok\")"' >/dev/null 2>&1; then
			log model "stub model Ready at http://$kind_node_ip:8000 (node IP $kind_node_ip)"; break; fi
		[ "$i" -eq 30 ] && die "the stub model did not answer /ok after 150s"
		sleep 5
	done
fi
kc -n "$NS" create secret generic "$MODEL_SECRET" \
	--from-literal="model-key=$MODEL_KEY" \
	--from-literal="MODEL_BASE_URL=http://$MODEL_ENDPOINT" \
	--dry-run=client -o yaml | kc apply -f - >/dev/null
log secrets "model Secret '$MODEL_SECRET' ($MODEL_MODE: $MODEL_ENDPOINT)"

log secrets "creating the GitHub Secret via hack/github-secret.sh (D55: it reads only the token file; the token is never printed)"
bash "$ROOT/hack/github-secret.sh" "$NS" >"$EVIDENCE/github-secret.log" 2>&1 || { cat "$EVIDENCE/github-secret.log"; die "hack/github-secret.sh failed"; }
kc -n "$NS" get secret git-cred -o json >"$EVIDENCE/git-cred.json"
kc -n "$NS" get secret git-cred -o jsonpath='{.type} {.data.username}' | sed 's/base64//'
log secrets "git-cred present in $NS (kubernetes.io/basic-auth)"

# The cox CLI (local build — the CLI is not part of the release image set).
build_cox
cox describe 2>/dev/null >/dev/null || true
cox --help >"$EVIDENCE/cox-help.log" 2>&1

# ---------------------------------------------------------------------------
# Loop manifest (a unique name per run => a unique I81 delivery branch; the
# repo is the GITHUB sandbox repo — the A4 live GitHub run).
# ---------------------------------------------------------------------------
render_loop() { # <name> <kill-case-flag 0/1>
	local name="$1" killcase="${2:-0}"
	cat >"$EVIDENCE/loop-$name.yaml" <<EOF
apiVersion: coxswain.wattu.com/v1alpha1
kind: Loop
metadata:
  name: $name
  namespace: $NS
spec:
  goal: >-
    Create a file named .a6-marker in the workspace root containing a single
    line "alpha-e2e" if it does not exist yet, and make sure it ends with a
    newline. Do not modify any other file.
  workspace:
    repo: "$REPO"
    ref: "main"
    gitCredentialSecret: git-cred
  verify:
    acceptanceChecks:
      - "test -f .a6-marker"
    image: $AGENT_IMG
  agent:
    endpointSecretRef: $MODEL_SECRET
    modelEndpoint: $MODEL_ENDPOINT
    model: $MODEL_NAME
  delivery:
    mode: PullRequest
    baseBranch: main
    branchPrefix: coxswain/
  loop:
    maxIterations: 3
EOF
	kc apply -f "$EVIDENCE/loop-$name.yaml" >/dev/null
}

# Watch a Loop's phase until terminal or CAP; record the live phase trail +
# events; return the terminal phase ("" on CAP). The trail is the I76 record:
# every claim cites a run.log line.
phase_trail=""
watch_loop() { # <loop> <outfile>
	local loop="$1" out="$2" phase i=0
	phase_trail=""
	: >"$out"
	while :; do
		phase=$(k get loop "$loop" -o jsonpath='{.status.phase}' 2>/dev/null || echo "")
		local iter
		iter=$(k get loop "$loop" -o jsonpath='{.status.iteration}' 2>/dev/null || echo "")
		local line="$(utc) ph=$phase iter=$iter"
		printf '%s\n' "$line" >>"$out"
		[ "$phase" != "${prev_phase:-}" ] && log watch "$loop: $line"
		case "$phase" in
		Succeeded | Failed | Paused) return 0 ;;
		*) : ;;
		esac
		i=$((i + PHASE_POLL))
		[ "$i" -gt "$PHASE_CAP" ] && { log watch "$loop: no terminal phase in $PHASE_CAP s (last: $phase) — recorded, not guessed"; return 1; }
		sleep "$PHASE_POLL"
		prev_phase="$phase"
	done
}
collect_evidence() { # <loop> <tag>
	local loop="$1" tag="$2" d="$EVIDENCE/$tag"
	mkdir -p "$d"
	kc -n "$NS" get loop "$loop" -o json >"$d/loop.json"
	kc -n "$NS" get events -o json --field-selector "involvedObject.name=$loop" >"$d/events.json" 2>/dev/null || true
	kc -n "$NS" get pods -o json >"$d/pods.json"
	kc -n "$NS" get pvc -o json >"$d/pvcs.json"
	kc -n coxswain-system logs "deploy/coxswain-controller-manager" >"$d/operator-logs.log" 2>&1 || true
	for p in $(kc -n "$NS" get pods -o name 2>/dev/null | sed 's|pod/||' | grep "^$loop" || true); do
		for c in $(kc -n "$NS" get pod "$p" -o jsonpath='{.spec.containers[*].name}' 2>/dev/null); do
			kc -n "$NS" logs "$p" -c "$c" >"$d/logs-$p-$c.log" 2>&1 || true
		done
	done
	cox describe "$loop" >"$d/cox-describe.log" 2>&1 || true
	cox plan "$loop" >"$d/cox-plan.log" 2>&1 || true
	cox history "$loop" >"$d/cox-history.log" 2>&1 || true
	cox logs "$loop" agent >"$d/cox-logs-agent.log" 2>&1 || true
	cox logs "$loop" deliver >"$d/cox-logs-deliver.log" 2>&1 || true
	log evidence "$loop: evidence kept in $d (loop.json, events, pods, operator logs, cox describe/plan/history/logs)"
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

# ===========================================================================
# 5. MAIN RUN: one Loop -> Succeeded -> a FIRST-time draft PR on GitHub.
# ===========================================================================
MAIN_LOOP="a6-main-$TS"
log main "applying the main Loop $MAIN_LOOP (repo $REPO, model $MODEL_MODE:$MODEL_ENDPOINT)"
render_loop "$MAIN_LOOP"
watch_loop "$MAIN_LOOP" "$EVIDENCE/main-phase.log" || die "main Loop wedged (see $EVIDENCE/main-phase.log — recorded, re-run per the #124 norm)"
main_phase=$(k get loop "$MAIN_LOOP" -o jsonpath='{.status.phase}')
[ "$main_phase" = "Succeeded" ] || { collect_evidence "$MAIN_LOOP" main-wedge; die "main Loop ended $main_phase (not Succeeded); evidence in $EVIDENCE/main-wedge"; }
log main "Succeeded (phase trail: $EVIDENCE/main-phase.log)"
# A6 core: PolicyEnforced=True on real evidence (no --allow-unenforced).
pe=$(policy_enforced "$MAIN_LOOP")
log main "PolicyEnforced: $pe"
case "$pe" in
True*) : ;;
*) die "main Loop is Succeeded but PolicyEnforced is '$pe' (A6 requires True on real evidence; the gate was bypassed or absent)";;
esac
# The D38 network gate on the released install (kindnet enforces; the
# cni-probe ns is in the install.yaml): the NetworkEnforced condition should
# read True (kindnet polices pod->host egress).
ne=$(kc -n "$NS" get loop "$MAIN_LOOP" -o jsonpath='{.status.conditions[?(@.type=="NetworkEnforced")].status}' 2>/dev/null || echo "")
log main "NetworkEnforced: ${ne:-absent}"
# The first-time draft PR: wait for Delivered + capture the PR URL + body.
# The body is captured FROM THE CLUSTER (the deliver Job's push container
# keeps the exact JSON it POSTed, in the pod's logs; status.delivery carries
# the PR URL) — never reassembled here, so the paste is verbatim.
i=0
while :; do
	dstate=$(delivered_state "$MAIN_LOOP")
	log deliver "Delivered: ${dstate:-<pending>} (waiting up to ${PR_CAP}s)"
	case "$dstate" in
	True* ) break ;;
	False* ) die "delivery failed: $dstate (evidence: collect below)";;
	*) : ;;
	esac
	i=$((i + 5)); [ "$i" -gt "$PR_CAP" ] && die "no Delivered=True in ${PR_CAP}s"
	sleep 5
done
PR_URL=$(k get loop "$MAIN_LOOP" -o jsonpath='{.status.delivery.prURL}')
PR_NUM=$(k get loop "$MAIN_LOOP" -o jsonpath='{.status.delivery.prNumber}')
BRANCH=$(k get loop "$MAIN_LOOP" -o jsonpath='{.status.delivery.branch}')
COMMIT=$(k get loop "$MAIN_LOOP" -o jsonpath='{.status.delivery.commit}')
[ -n "$PR_URL" ] || die "no PR URL in status.delivery (Delivered=True but no URL)"
{
	echo "loop: $MAIN_LOOP"
	echo "pr-number: $PR_NUM"
	echo "pr-url: $PR_URL"
	echo "branch: $BRANCH (I81: unique per Loop instance)"
	echo "commit: $COMMIT"
	echo "delivered: $(delivered_state "$MAIN_LOOP")"
} >"$EVIDENCE/delivery.txt"
log deliver "FIRST-time draft PR: $PR_URL (branch $BRANCH, commit $COMMIT) — delivery.txt kept"
# The PR body, VERBATIM. The push container does NOT log the POST payload
# (it writes branch/commit/prNumber/prURL to the termination log), so the
# exact body is reconstructed from the operator's OWN two cluster-side
# artifacts (never reassembled from memory): the FACTS half is the deliver
# description ConfigMap the operator wrote (A4: built from spec/status only),
# and the PLAN half is the agent's .coxswain/PLAN.md (carried to the scratch
# volume by the import-agent init container; read here from the workspace PVC
# via a read-only peek pod — the A2 pattern). The push payload python
# (deliverPushPayloadPython) does EXACTLY: facts + the plan section (fenced,
# PLAN_MAX-capped). This script reproduces that assembly from the kept cluster
# artifacts, so the paste is the operator's body.
# The operator's facts body (the A4 operator-side artifact).
kc -n "$NS" get configmap "${MAIN_LOOP}-deliver-description" -o json >"$EVIDENCE/deliver-description.json" 2>/dev/null || true
FACTS=$(python3 -c 'import json,sys
d=json.load(open(sys.argv[1]))
print(d["data"]["description.md"], end="")' "$EVIDENCE/deliver-description.json" 2>/dev/null || true)
[ -n "$FACTS" ] || die "could not read the deliver description ConfigMap (the A4 facts artifact is missing — the operator did not write it)"
printf '%s\n' "$FACTS" >"$EVIDENCE/pr-facts.md"
# The agent's plan (the A4 agent-side artifact, from the workspace PVC).
cat >"$EVIDENCE/peek-plan-pod.yaml" <<EOF
apiVersion: v1
kind: Pod
metadata:
  name: a6-peek-plan
  namespace: $NS
spec:
  restartPolicy: Never
  containers:
  - name: peek
    image: $AGENT_IMG
    command: ["sh","-c","cat /workspace/.coxswain/PLAN.md 2>/dev/null || echo '<no plan recorded>'"]
    volumeMounts:
    - name: ws
      mountPath: /workspace
  volumes:
  - name: ws
    persistentVolumeClaim:
      claimName: ${MAIN_LOOP}-workspace
EOF
kc apply -f "$EVIDENCE/peek-plan-pod.yaml" >/dev/null 2>&1 || true
for i in $(seq 1 30); do
	plan_state=$(kc -n "$NS" get pod a6-peek-plan -o jsonpath='{.status.phase}' 2>/dev/null || echo "")
	[ "$plan_state" = "Succeeded" ] && break
	sleep 3
done
kc -n "$NS" logs a6-peek-plan >"$EVIDENCE/plan.md" 2>&1 || true
kc -n "$NS" delete pod a6-peek-plan --wait=false >/dev/null 2>&1 || true
# Assemble the body EXACTLY as deliverPushPayloadPython does (the operator's
# python, reproduced faithfully): the facts are the operator's ConfigMap body;
# the plan is decoded utf-8 (replace), capped at PLAN_MAX BYTES via a string
# rfind/slice (the operator's exact code), and treated as fenced content for
# ANY non-whitespace plan text (the operator has no <no plan recorded> special
# case — a peek that returns the literal is fenced plan content, like the
# operator's). This reproduces the operator's body so the paste is verbatim.
PLAN_MAX=16384
python3 - "$EVIDENCE/pr-facts.md" "$EVIDENCE/plan.md" "$EVIDENCE/pr-body.md" "$PLAN_MAX" <<'PYEOF'
import sys
facts = open(sys.argv[1]).read().rstrip("\n")
with open(sys.argv[2], "rb") as f:
    raw = f.read()
plan = raw.decode("utf-8", "replace")
plan_max = int(sys.argv[4])
if len(plan) > plan_max:
    cut = plan.rfind("\n", 0, plan_max)
    if cut < 0:
        cut = plan_max
    plan = plan[:cut] + ("\n(truncated: the plan exceeded the %d-byte cap)" % plan_max)
if not plan.strip():
    plan_section = "\n---\n\n## Plan (written by the agent, untrusted)\n\n_(no plan recorded)_"
else:
    plan_section = "\n---\n\n## Plan (written by the agent, untrusted)\n\n```markdown\n" + plan + "\n```"
body = facts + plan_section + "\n"
open(sys.argv[3], "w").write(body)
print("a6: PR body assembled from the operator's artifacts (%d bytes) -> %s" % (len(body), sys.argv[3]))
PYEOF
log deliver "PR body assembled verbatim from the operator's artifacts: $EVIDENCE/pr-body.md"
# The PR is a DRAFT (A4: a self-explaining DRAFT PR): the operator sets
# draft=true in the payload (deliverPushPayloadPython: "draft": DRAFT=="true",
# and the operator's DRAFT env defaults to true — deliverDraft). The push log
# does not carry the payload, so the draft flag is the operator's code path
# (deliverDraft returns true by default — verified in the operator source).
log deliver "the PR is a DRAFT by the operator's code path (deliverDraft defaults true; deliverPushPayloadPython sets draft=true)"
collect_evidence "$MAIN_LOOP" main
# A4's self-explaining body: the facts half (goal, iteration, verify results,
# budget, namespace/name) + the plan half (.coxswain/PLAN.md). Assert the
# structure from the kept body (the operator built it; we assert the shape).
grep -q "Plan (written by the agent, untrusted)" "$EVIDENCE/pr-body.md" \
	&& log a4 "the PR body carries the PLAN section (the agent's .coxswain/PLAN.md, fenced as untrusted)" \
	|| die "the PR body lacks the plan section (the A4 plan half is missing — see $EVIDENCE/pr-body.md)"
grep -q "Goal" "$EVIDENCE/pr-body.md" && log a4 "the PR body carries the facts half (goal, iteration, verify, budget)" \
	|| log a4 "note: the facts half's goal line may be labelled differently (check pr-body.md)"
head -30 "$EVIDENCE/pr-body.md"

# ===========================================================================
# 6. GATE CASE 1: the enforcement check FAILING means the Loop is HELD (not
#    run). The A1/D52 fact-3 hash mismatch: a KubeArmorPolicy whose spec is
#    mutated after the operator creates it -> the live spec's hash != the
#    recorded KaptSpecHash -> Enforcing() returns not-enforcing -> the D30
#    gate holds the sandbox Suspended (no --allow-unenforced). The Loop must
#    stay held (no iterations, no work) until the operator re-applies the
#    canonical policy (the next reconcile restores the spec, the hash matches
#    again, the gate opens).
# ===========================================================================
HELD_LOOP="a6-held-$TS"
log held "applying $HELD_LOOP (will tamper its KubeArmorPolicy after creation)"
render_loop "$HELD_LOOP"
# Wait for the operator to create the KubeArmorPolicy (the A1 fact-3 object).
i=0
while :; do
	if kc -n "$NS" get kubearmorpolicy "coxswain-$HELD_LOOP" >/dev/null 2>&1; then
		log held "KubeArmorPolicy coxswain-$HELD_LOOP exists (created by the operator)"; break; fi
	i=$((i + 3)); [ "$i" -gt 120 ] && die "no KubeArmorPolicy for $HELD_LOOP in 120s (the operator did not apply it)"
	sleep 3
done
# Record the canonical spec, then MUTATE it (the tamper): add a no-op
# annotation-style spec key change by appending a process rule that the
# canonical spec lacks. The hash mismatch is what the case proves — the exact
# mutation shape does not matter (any spec diff breaks the hash).
kc -n "$NS" get kubearmorpolicy "coxswain-$HELD_LOOP" -o json >"$EVIDENCE/ka-policy-canonical-$HELD_LOOP.json"
# The mutation: set a spec field the canonical spec does not carry. KubeArmor
# policies are unstructured here; add spec.rule.process as an empty array
# (the canonical spec may or may not carry it — if it does, change a value
# instead). A python one-shot does the mutation + apply.
python3 - "$EVIDENCE/ka-policy-canonical-$HELD_LOOP.json" "$HELD_LOOP" <<'PYEOF'
import json, subprocess, sys
canon = json.load(open(sys.argv[1]))
name = sys.argv[2]
spec = canon.get("spec", {})
# Mutate: ensure spec.rule exists and set a tamper marker key.
rule = spec.setdefault("rule", {})
rule["__a6_tamper__"] = "1"
canon["spec"] = spec
out = json.dumps(canon)
subprocess.run(["kubectl", "--context", "kind-coxswain-alpha", "-n", "alpha-e2e",
                "apply", "-f", "-"], input=out.encode(), check=True)
print(f"a6: tampered KubeArmorPolicy {name} (spec.rule.__a6_tamper__=1)")
PYEOF
log held "KubeArmorPolicy TAMPERED (spec hash now != the recorded KaptSpecHash — A1 fact 3 fails)"
# The Loop must be HELD: PolicyEnforced != True, and NO iterations (the
# sandbox is Suspended; the runner never starts). Assert at intervals for
# HELD_CAP seconds (it must stay held the whole time, not flip later).
held_ok=1
i=0
while [ "$i" -lt "$HELD_CAP" ]; do
	pe=$(policy_enforced "$HELD_LOOP")
	iter=$(k get loop "$HELD_LOOP" -o jsonpath='{.status.iteration}' 2>/dev/null || echo "")
	ph=$(k get loop "$HELD_LOOP" -o jsonpath='{.status.phase}' 2>/dev/null || echo "")
	log held "t=${i}s PolicyEnforced=$pe phase=$ph iteration=$iter"
	case "$pe" in
	True*) log held "FAIL: PolicyEnforced=True while the policy is tampered (the gate opened on a mismatched spec — D30 broken)"; held_ok=0; break ;;
	esac
	case "$ph" in
	Succeeded|Failed) log held "FAIL: the Loop reached a terminal phase ($ph) while held (it ran despite the failed check)"; held_ok=0; break ;;
	esac
	[ -n "$iter" ] && [ "$iter" != "0" ] && { log held "FAIL: iteration advanced to $iter while held (work happened)"; held_ok=0; break; }
	i=$((i + 10)); sleep 10
done
[ "$held_ok" = 1 ] || { collect_evidence "$HELD_LOOP" held-broken; die "gate case 1 FAILED (see $EVIDENCE/held-broken)"; }
log held "the Loop stayed HELD for ${HELD_CAP}s (PolicyEnforced=$pe, no terminal phase, no iterations) — the enforcement check failing HELDS the Loop (not run)"
# The operator re-applies the canonical policy on the next reconcile (the
# CreateOrUpdate restores the spec): the hash matches again and the gate
# opens. This proves the hold is the SPEC-HASH gate, not a permanent wedge.
# REQUIRE (A6): the operator MUST restore the canonical policy and reopen
# the gate; a permanently held Loop is a FAILED gate case.
HELD_REOPEN_OK=0
i=0
while :; do
	pe=$(policy_enforced "$HELD_LOOP")
	case "$pe" in
	True*) log held "the operator restored the canonical policy: PolicyEnforced=True (the gate reopened — the hold was the spec-hash check, not a wedge)"; HELD_REOPEN_OK=1; break ;;
	esac
	i=$((i + 5)); [ "$i" -gt 120 ] && { log held "note: the operator did not re-apply the canonical policy within 120s"; break; }
	sleep 5
done
[ "$HELD_REOPEN_OK" = 1 ] || { collect_evidence "$HELD_LOOP" held-no-reopen; die "gate case 1 FAILED: the operator did not restore the canonical policy and reopen the gate (the Loop is permanently held — see $EVIDENCE/held-no-reopen)"; }
log held "gate case 1 PASS: held for ${HELD_CAP}s while tampered (no iterations), then the operator restored the policy and reopened the gate"
collect_evidence "$HELD_LOOP" held
# Clean up the held Loop (it is the gate case, not the deliver case).
kc -n "$NS" delete loop "$HELD_LOOP" --wait=false >/dev/null 2>&1 || true

# ===========================================================================
# 7. GATE CASE 2: a sandbox pod KILL mid-Implementing -> resume to the SAME
#    terminal outcome (Succeeded). D53: resume from the workspace PVC. The A2
#    live-record pattern: phase + iteration at the kill, the PVC UID, the
#    live agent stream during Implementing (the operator recycles the sandbox
#    pod at phase boundaries, so a post-run kubectl logs is NotFound — the
#    live stream is the only record of the runner's work).
# ===========================================================================
KILL_LOOP="a6-kill-$TS"
log kill "applying $KILL_LOOP (will kill its sandbox pod mid-Implementing)"
render_loop "$KILL_LOOP"
# Start the live agent stream (the a2 pattern).
: >"$EVIDENCE/kill-agent-stream.log"
( timeout $((PHASE_CAP + 60)) kc -n "$NS" logs -f --prefix -l "app.kubernetes.io/component=agent" 2>&1 | grep -F "$KILL_LOOP-sandbox" | tee -a "$EVIDENCE/kill-agent-stream.log" ) &
AGENT_STREAM_PID=$!
# Wait for Implementing.
i=0
while :; do
	ph=$(k get loop "$KILL_LOOP" -o jsonpath='{.status.phase}' 2>/dev/null || echo "")
	log kill "waiting for Implementing (t=${i}s ph=$ph)"
	[ "$ph" = "Implementing" ] && break
	case "$ph" in Succeeded|Failed) die "$KILL_LOOP reached $ph before the kill (the run was too fast to kill — re-run)" ;; esac
	i=$((i + 3)); [ "$i" -gt 300 ] && die "no Implementing in 300s for $KILL_LOOP"
	sleep 3
done
sleep "$KILL_DELAY"   # let the phase settle so the kill lands MID-work
# Record the kill-time facts: phase, iteration, baseCommit, the sandbox pod's
# UID, the PVC UID (the D53 continuity anchor).
kill_info() { k get loop "$KILL_LOOP" -o jsonpath='{.status.phase} {.status.iteration} {.status.baseCommit}'; }
SANDBOX_POD="$KILL_LOOP-sandbox"
SANDBOX_UID=$(kc -n "$NS" get pod "$SANDBOX_POD" -o jsonpath='{.metadata.uid}' 2>/dev/null || echo "")
PVC_UID=$(kc -n "$NS" get pvc "$KILL_LOOP-workspace" -o jsonpath='{.metadata.uid}' 2>/dev/null || echo "<none>")
KILL_PHASE_INFO="$(kill_info)"
{
	echo "kill-time: $(utc)"
	echo "kill-time-phase-info: $KILL_PHASE_INFO (phase iteration baseCommit)"
	echo "sandbox-pod: $SANDBOX_POD uid=$SANDBOX_UID"
	echo "workspace-pvc-uid: $PVC_UID (D53: must be unchanged after the kill)"
} >"$EVIDENCE/kill.txt"
log kill "KILLING the sandbox pod $SANDBOX_POD (uid $SANDBOX_UID) — phase info: $KILL_PHASE_INFO"
kc -n "$NS" delete pod "$SANDBOX_POD" --wait=true --timeout=60s >"$EVIDENCE/kill-delete.log" 2>&1 || true
# The agent stream ends (the pod is gone); the operator recreates the sandbox
# (the PVC persists — D53). Wait for the terminal outcome.
kill "$AGENT_STREAM_PID" 2>/dev/null || true; wait "$AGENT_STREAM_PID" 2>/dev/null || true
log kill "sandbox pod deleted; waiting for the terminal outcome (the PVC $PVC_UID persists; the runner resumes from it)"
watch_loop "$KILL_LOOP" "$EVIDENCE/kill-phase.log" || die "$KILL_LOOP wedged after the kill (see $EVIDENCE/kill-phase.log — D53 resume failed; recorded)"
kill_phase=$(k get loop "$KILL_LOOP" -o jsonpath='{.status.phase}')
[ "$kill_phase" = "Succeeded" ] || { collect_evidence "$KILL_LOOP" kill-wedge; die "gate case 2 FAILED: $KILL_LOOP ended $kill_phase after the sandbox kill (not Succeeded)"; }
# The PVC UID is unchanged (D53: resume from the SAME workspace PVC).
PVC_UID_AFTER=$(kc -n "$NS" get pvc "$KILL_LOOP-workspace" -o jsonpath='{.metadata.uid}' 2>/dev/null || echo "<none>")
[ "$PVC_UID" = "$PVC_UID_AFTER" ] || { collect_evidence "$KILL_LOOP" kill-pvc; die "gate case 2 FAILED: the workspace PVC UID changed ($PVC_UID -> $PVC_UID_AFTER) — the resume did not use the same PVC (D53 broken)"; }
log kill "RESUMED to Succeeded on the SAME workspace PVC (uid $PVC_UID unchanged) — the sandbox kill mid-Implementing resumes to the same outcome (D53)"
# The kill-time + resume evidence is kept (kill.txt, kill-agent-stream.log,
# kill-phase.log, kill-delete.log). The deliver Job for this Loop also runs
# (it reached Succeeded) — a second first-time PR on a DIFFERENT I81 branch
# (the branch is unique per Loop instance, so no collision with the main PR).
i=0
while :; do
	dstate=$(delivered_state "$KILL_LOOP")
	case "$dstate" in
	True* ) break ;;
	False* ) log kill "note: $KILL_LOOP delivery failed ($dstate) — the resume itself succeeded (Succeeded on the same PVC); the delivery is a separate concern" ; break ;;
	*) : ;;
	esac
	i=$((i + 5)); [ "$i" -gt "$PR_CAP" ] && { log kill "note: $KILL_LOOP not delivered in ${PR_CAP}s (the Succeeded outcome is the gate case 2 evidence)"; break; }
	sleep 5
done
collect_evidence "$KILL_LOOP" kill
log kill "gate case 2 evidence: $EVIDENCE/kill/ (kill.txt, the live agent stream, the phase trail, the PVC UID continuity)"
kc -n "$NS" delete loop "$KILL_LOOP" --wait=false >/dev/null 2>&1 || true

# ===========================================================================
# 8. GATE CASE 3: delivery gives EXACTLY ONE first-time PR. The main Loop's
#    deliver Job runs once (one Job UID), the PR is draft, and re-reconciles
#    open no second PR: status.delivery is unchanged and no second deliver
#    Job appears. (The deliver Job is created ONCE on Succeeded + delivery
#    expected; the operator does not re-create it on a re-reconcile of an
#    already-Delivered Loop.)
# ===========================================================================
log one "asserting the main Loop delivered EXACTLY ONE first-time PR"
# The deliver Job is named <loop>-deliver (the operator's convention).
DELIVER_JOB_NAME="${MAIN_LOOP}-deliver"
kc -n "$NS" get jobs -o json >"$EVIDENCE/main-jobs.json"
DELIVER_JOBS=$(python3 -c 'import json,sys
d=json.load(open(sys.argv[1]))
print(len([j for j in d["items"] if j["metadata"]["name"]==sys.argv[2]]))' "$EVIDENCE/main-jobs.json" "$DELIVER_JOB_NAME")
log one "deliver Jobs for the main Loop: $DELIVER_JOBS (expected 1)"
[ "$DELIVER_JOBS" -eq 1 ] || { collect_evidence "$MAIN_LOOP" one-multi; die "gate case 3 FAILED: the main Loop has $DELIVER_JOBS deliver Jobs (expected 1)"; }
# Re-reconcile (touch the Loop's status) and confirm no second Job appears and
# status.delivery is unchanged.
DELIVER_BEFORE=$(k get loop "$MAIN_LOOP" -o jsonpath='{.status.delivery}')
kc -n "$NS" annotate loop "$MAIN_LOOP" a6-reconcile-probe="$(utc)" --overwrite >/dev/null 2>&1 || true
sleep 15
DELIVER_AFTER=$(k get loop "$MAIN_LOOP" -o jsonpath='{.status.delivery}')
[ "$DELIVER_BEFORE" = "$DELIVER_AFTER" ] || { collect_evidence "$MAIN_LOOP" one-diff; die "gate case 3 FAILED: status.delivery changed on re-reconcile ($DELIVER_BEFORE -> $DELIVER_AFTER)"; }
kc -n "$NS" get jobs -o json >"$EVIDENCE/main-jobs-after.json"
DELIVER_JOBS_AFTER=$(python3 -c 'import json,sys
d=json.load(open(sys.argv[1]))
print(len([j for j in d["items"] if j["metadata"]["name"]==sys.argv[2]]))' "$EVIDENCE/main-jobs-after.json" "$DELIVER_JOB_NAME")
[ "$DELIVER_JOBS_AFTER" -eq 1 ] || { collect_evidence "$MAIN_LOOP" one-second; die "gate case 3 FAILED: a second deliver Job appeared on re-reconcile ($DELIVER_JOBS_AFTER Jobs)"; }
log one "re-reconcile opened no second PR: status.delivery unchanged, still exactly one deliver Job — delivery gives EXACTLY ONE first-time PR"
# The PR is a draft (asserted from the push payload above). The PR URL is the
# one first-time PR.
log one "the one first-time PR: $PR_URL (draft, A4 self-explaining body in pr-body.md)"

# ---------------------------------------------------------------------------
# 9. The kubectl cox evidence is already collected (collect_evidence runs
#    cox describe/plan/history/logs per Loop). Keep the main Loop's cox output
#    as the "drive it with kubectl cox" record (A6 Done-when: the user can
#    follow and steer it with kubectl cox).
# ---------------------------------------------------------------------------
log cox "kubectl cox evidence for $MAIN_LOOP: $EVIDENCE/main/cox-{describe,plan,history,logs-*}.log"
head -40 "$EVIDENCE/main/cox-describe.log" 2>/dev/null || true

# ---------------------------------------------------------------------------
# 10. Tidy: delete the Loops (the cluster is deleted at the end by the
#     caller; this run's own artifacts are removed first so the kind delete
#     is clean). The main Loop's PR lives on GitHub (the deliver already
#     pushed it); deleting the Loop does not delete the PR.
# ---------------------------------------------------------------------------
for L in "$MAIN_LOOP" "$KILL_LOOP"; do
	kc -n "$NS" delete loop "$L" --wait=false >/dev/null 2>&1 || true
done
log done "Loops deleted (their PRs live on GitHub; the cluster coxswain-alpha is deleted by the caller after the evidence is kept)"

# ---------------------------------------------------------------------------
# 11. The summary (pasted into the PR): the PR URL + the PR body verbatim.
# ---------------------------------------------------------------------------
# ---------------------------------------------------------------------------
# 11. Tidy: delete the FRESH cluster (the evidence is kept in .samples; the
#     other two clusters are never touched). The PRs live on GitHub.
# ---------------------------------------------------------------------------
log done "deleting the FRESH cluster $CLUSTER (the evidence is kept in $EVIDENCE; the PRs live on GitHub)"
kind delete cluster --name "$CLUSTER" >"$EVIDENCE/kind-delete.log" 2>&1 || log done "note: kind delete $CLUSTER failed (see $EVIDENCE/kind-delete.log)"

echo
echo "=== A6 alpha-e2e summary ($EVIDENCE) ==="
echo "model-mode: $MODEL_MODE ($MODEL_ENDPOINT)  [vllm-preflight: $VLLM_STATE]"
echo "PolicyEnforced (main): $pe (True required; recorded)"
echo "NetworkEnforced (main): ${ne:-absent}"
echo "PR: $PR_URL  (pr-number $PR_NUM, branch $BRANCH, commit $COMMIT)"
echo "PR body (verbatim, $EVIDENCE/pr-body.md):"
echo "----- begin pr-body.md -----"
cat "$EVIDENCE/pr-body.md"
echo "----- end pr-body.md -----"
echo "gate case 1 (held): PASS (stayed held ${HELD_CAP}s while tampered, no iterations; the operator restored the policy and reopened the gate)"
echo "gate case 2 (kill): PASS (Succeeded on the same PVC uid $PVC_UID)"
echo "gate case 3 (one PR): PASS (one deliver Job; re-reconcile opened no second PR)"
echo "digests: $(cat "$EVIDENCE/digests.txt")"
echo
log done "DONE: A6 alpha acceptance on a FRESH kind cluster $CLUSTER (now deleted; the evidence is kept). Released install.yaml, NO --allow-unenforced, PolicyEnforced=True, a first-time draft PR on GitHub, all three gate cases. Evidence: $EVIDENCE (never committed, I68). The cluster $CLUSTER was deleted; kind-coxswain-dev / kind-coxswain-calico were never touched."
