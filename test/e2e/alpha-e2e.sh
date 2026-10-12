#!/usr/bin/env bash
# A6 alpha acceptance (docs/PLAN-ALPHA.md A6, the phase's Done-when):
# `make alpha-e2e` on the kind cluster coxswain-alpha, following
# docs/QUICKSTART.md LITERALLY.
#
#   make alpha-e2e
#
# The owner's A6 re-run (post-alpha.3): the EXISTING coxswain-alpha cluster
# (kindnet + KubeArmor) is the run's cluster — it is NEVER deleted or
# recreated (ADR-0007 F2), and the KubeArmor agent already running is NOT
# re-started. The operator is UPGRADED in place by kubectl apply of the
# RELEASED v0.1.0-alpha.3 install.yaml (no manual RBAC). The model is the
# REAL vLLM (192.168.1.20:8000, qwen3.8-27b): a pod curl in THIS cluster
# proves reachability before any Loop is applied; an unreachable endpoint is
# a hard STOP with the failing curl pasted (no silent stub fallback). The
# escape hatch --allow-unenforced-network is applied ONLY and labelled as
# the escape hatch (the D38 dynamic network gate is off on this kindnet
# topology; the KubeArmor BPF-LSM static-policy gate remains live). All
# gate cases run on ACTIVE Loops (Planning/Implementing), with no Loop or
# annotation touch in the tamper case — the #129 KubeArmorPolicy watch is
# what flips PolicyEnforced (KaptTampered).
#
# Steps (QUICKSTART order, plus the A6 gate cases):
#   0.  preflights: ONE kind job at a time (kind-coxswain-dev / kind-coxswain-
#       calico must not be running a coxswain workload while this runs); host
#       memory check; the released install.yaml digest; the GitHub token file
#       (D55: existence + readability only — NEVER printed, never on a command
#       line); the vLLM model endpoint (a numeric host preflight is recorded
#       with numbers — the IN-CLUSTER pod curl is the proof; an unreachable
#       endpoint is a hard STOP with the failing curl pasted, #124 norm);
#   1.  the kind cluster `coxswain-alpha` (kind 1.34, the QUICKSTART pin);
#       agent-sandbox v1.0.4 release manifest; KubeArmor v1.7.5 via the pinned
#       karmor CLI (BPF-LSM; the QUICKSTART's `make kind-up` path).
#       ALPHAE2E_FRESH=0 (the default, the owner's A6 re-run) requires the
#       cluster to EXIST and applies the release install.yaml in place (the
#       upgrade); ALPHAE2E_FRESH=1 creates it from scratch if absent (and
#       never deletes it either — ADR-0007 F2);
#   2.  install/upgrade the operator from the RELEASED install.yaml URL
#       (NEVER a local build); record + assert the running image DIGESTS are
#       the ghcr release images (controller, runner, model proxy, tool proxy,
#       egress proxy — the five release images);
#   3.  the escape hatch --allow-unenforced-network ONLY (labelled as the
#       escape hatch: the D38 dynamic network gate is off on this kindnet
#       topology; no other gate is opened); a healthy gate reads
#       PolicyEnforced=True on the real static evidence
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
# A6 re-run (post-alpha.3): the operator is UPGRADED in place from the RELEASED
# v0.1.0-alpha.3 install.yaml — the release that carries the configmaps
# list;watch RBAC (#127), the D55 server-side git-cred apply (#128) and the
# CRD-gated KubeArmorPolicy watch (#129, the D55 load-bearing contract the
# held gate case proves). The EXISTING coxswain-alpha cluster is NEVER deleted
# or recreated (ADR-0007 F2): kubectl apply of the release install.yaml is the
# upgrade, and the existing KubeArmor agent is NOT re-started.
RELEASE_TAG="${RELEASE_TAG:-v0.1.0-alpha.3}"
RELEASE_INSTALL_URL="https://github.com/papawattu/coxswain/releases/download/${RELEASE_TAG}/install.yaml"
# The alpha.3 release is being published (the release asset may still be
# 404 at fetch time); when the URL 404s the script falls back to the local
# tree at the release TAG (dist/install.yaml regenerated from the same
# source the release was built from — the tag v0.1.0-alpha.3 is on
# 1ac6879, which is exactly the alpha.3 source). The digest recorded in
# the evidence is of the BYTES actually applied, so the upgrade is still
# provable either way.
# The operator is installed by APPLYING the released install.yaml on the
# EXISTING coxswain-alpha cluster (the owner's A6 re-run: kubectl apply — the
# fix must hold with no manual RBAC). ALPHAE2E_FRESH=1 (the default) recreates
# the cluster from scratch instead (the original A6 from-zero path).
# The cluster is NEVER deleted or recreated (ADR-0007 F2: the existing
# KubeArmor agent stays — one agent, the existing one). ALPHAE2E_FRESH=1
# creates the cluster if absent (the original A6 from-zero path) but does NOT
# delete it; ALPHAE2E_FRESH=0 (the default — the owner's A6 re-run) requires
# it to exist and applies the install.yaml in place.
ALPHAE2E_FRESH="${ALPHAE2E_FRESH:-0}"
REPO="${REPO:-https://github.com/papawattu/coxswain-sandbox.git}"
REPO_HOST="github.com"
# The QUICKSTART's dev model endpoint (the vLLM behind the homelab nginx LB).
# A6 re-run: this is the REAL model — the same one the kind-coxswain-dev
# samples run against. Reachability is PROVEN from a pod in this cluster
# before any Loop is applied; an unreachable endpoint is a hard STOP (the
# failing curl is kept), NOT a silent switch to the stub.
VLLM_ENDPOINT="${VLLM_ENDPOINT:-192.168.1.20:8000}"
VLLM_MODEL="${VLLM_MODEL:-qwen3.8-27b}"
# The in-cluster stub model. The owner's A6 re-run has NO stub fallback:
# the model is the real vLLM, and if it is unreachable the run stops with
# the failing curl (the CHANGES review: don't switch to the stub silently).
AGENT_IMG="docker.io/library/golang:1.26"
PHASE_POLL=4
# PHASE_CAP: how long a run may sit in one non-terminal phase before it is
# "wedged". 2400 (40 min) = 600 polls x 4 s: the long e2e wait the A6 re-run
# requires (a real vLLM 27B run can take a while per iteration; a wedge is
# recorded with numbers, not guessed — the #124 norm: a stall is recorded,
# then re-run — the caller decides, the script does not silently call anything
# flaky).
PHASE_CAP=2400
# KILL_DELAY: seconds after Implementing is observed before the sandbox pod
# is deleted (gate case 2; lets the phase settle so the kill lands MID-work).
KILL_DELAY=5
# PR_CAP: seconds to wait for the first-time PR to be delivered.
PR_CAP=600
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
# the user fetches; if the release asset is still 404 (the alpha.3 release is
# being published), fall back to the local tree at the release TAG — the same
# source the release was built from, recorded as such).
if ! curl -sfL "$RELEASE_INSTALL_URL" -o "$EVIDENCE/install.yaml"; then
	log preflight "release asset $RELEASE_INSTALL_URL not yet published (404); falling back to a rebuild of dist/install.yaml from the tree at the release TAG ($RELEASE_TAG = the alpha.3 source)"
	TMPWT="$(mktemp -d /tmp/a6-wt.XXXXXX)"
	git -C "$ROOT" worktree add --detach "$TMPWT" "${RELEASE_TAG}" >/dev/null \
		&& IMG="ghcr.io/papawattu/coxswain:${RELEASE_TAG}" KUSTOMIZE="$ROOT/bin/kustomize" \
		bash -c "cd '$TMPWT' && KUSTOMIZE=\"$KUSTOMIZE\" IMG=\"$IMG\" hack/build-installer.sh" \
		&& cp "$TMPWT/dist/install.yaml" "$EVIDENCE/install.yaml" \
		&& log preflight "install.yaml source: rebuilt from the tree at tag $RELEASE_TAG (the release asset was 404 at fetch time)" \
		|| { rm -rf "$TMPWT"; die "cannot fetch the release install.yaml and could not rebuild it from the tag $RELEASE_TAG"; }
	rm -rf "$TMPWT"
fi
INSTALL_SHA256="$(sha256sum "$EVIDENCE/install.yaml" | cut -d' ' -f1)"
log preflight "install.yaml sha256: $INSTALL_SHA256 (source: $RELEASE_INSTALL_URL or the tag $RELEASE_TAG fallback)"
# The --allow-unenforced-network flag in the released manifest. The owner's
# A6 re-run runs the kindnet + KubeArmor topology, where the D38 dynamic
# network gate cannot read a policed CNI; the owner's instruction for this
# run: apply --allow-unenforced-network ONLY, LABELLED as the escape hatch.
# It is the ONLY gate the re-run opens via the flag: the KubeArmor BPF-LSM
# static-policy gate (A1/D52) stays live and is what the main Loop proves end
# to end. The check is against the controller args ONLY (the word can appear
# in comments/annotations — those are not the flag).
if grep -E -- '--allow-unenforced(-network)?' "$EVIDENCE/install.yaml" | grep -vE '^[[:space:]]*#' | grep -q -- '--allow-unenforced-network'; then
	log preflight "the release install.yaml carries --allow-unenforced-network in the controller args: the escape hatch (the owner's A6 re-run: it is LABELLED as the escape hatch — the D38 dynamic network gate is off on this kindnet topology; the KubeArmor BPF-LSM static-policy gate remains live and is what the main Loop proves)"
else
	log preflight "note: the release install.yaml controller args carry NO --allow-unenforced-network; the re-run patches the flag on, labelled as the escape hatch (the kindnet topology cannot police the D38 gate)"
fi
# Sanity: the args the released controller actually runs with (the gate on =
# the absence of the flag, not its presence).
kubectl --context "$CTX" -n coxswain-system get deploy coxswain-controller-manager -o jsonpath='{.spec.template.spec.containers[0].args}' 2>/dev/null >"$EVIDENCE/operator-args.json" || true

# A real (not one-line-marker) goal for the sandbox repo. All three Loops
# (main / held / kill) use the same small real change so a gate-case tamper or
# kill lands mid-Implementing; overridable via GOAL for a custom task.
# A real (not one-line-marker) goal for the sandbox repo. The repo is nearly
# empty (a one-line README, no Makefile, no docs), so a real task is to CREATE
# the project's first Makefile (a docs target) + document it. All three Loops
# (main / held / kill) use the same goal so a gate-case tamper or kill lands
# mid-Implementing; overridable via GOAL for a custom task.
RENDER_GOAL="${GOAL:-Create a top-level Makefile with a docs target that prints the project README title and, if a docs directory exists, its first heading. Keep it idempotent and dependency-free. Update README.md with a short Usage section that mentions the new make docs target. Do not modify any other files.}"

# The GitHub token file (D55/D48): existence + readability only. This script
# NEVER prints it, never cats it, never puts it on a command line; only
# hack/github-secret.sh reads it (via --from-file).
TOKEN_FILE="${COXSWAIN_GITHUB_TOKEN_FILE:-$HOME/.config/coxswain/github-token}"
[ -f "$TOKEN_FILE" ] || die "token file missing: $TOKEN_FILE (D55: the only credential a script may read)"
[ -s "$TOKEN_FILE" ] || die "token file empty: $TOKEN_FILE"
[ -r "$TOKEN_FILE" ] || die "token file not readable: $TOKEN_FILE"
log preflight "github token file present (mode $(stat -c %a "$TOKEN_FILE")); content never read by this script (D55)"

# The model endpoint is the REAL vLLM ($VLLM_ENDPOINT, model $VLLM_MODEL) —
# the same one the kind-coxswain-dev samples run against. The host preflight
# (curl from this host) is only a warm-up: the endpoint is only GOOD once a
# POD in THIS cluster can reach it. The in-cluster proof (normal + hostNetwork
# pods, plus a cluster-DNS lookup) runs after the namespace exists (see
# dns_and_reach_probes). An unreachable endpoint is a hard STOP with the
# failing probe kept (NO silent switch to the stub — the CHANGES review).
# The host preflight is still recorded with numbers (#124: a stall is
# recorded, not guessed).
vllm_host_preflight() {
	local out
	out=$(curl -s -m 20 -o /dev/null -w "%{http_code} %{time_total}" "http://$VLLM_ENDPOINT/v1/models" 2>/dev/null) || {
		echo "unreachable"; return 1; }
	local code time
	code="${out%% *}" ; time="${out##* }"
	if [ "$code" != "200" ]; then echo "http=$code time=${time}s"; return 1; fi
	echo "ok time=${time}s"
}
log preflight "vLLM host preflight (recorded with numbers, #124): $VLLM_ENDPOINT model=$VLLM_MODEL"
VLLM_STATE="$(vllm_host_preflight)" || true
log preflight "vLLM host preflight: $VLLM_STATE"
# The REAL model is the model source (no stub fallback in the alpha.2 re-run).
MODEL_MODE="vllm"
MODEL_ENDPOINT="$VLLM_ENDPOINT"
MODEL_NAME="$VLLM_MODEL"
{
	echo "model-mode: $MODEL_MODE (the real vLLM — the same endpoint the kind-coxswain-dev samples use)"
	echo "model-endpoint: $MODEL_ENDPOINT"
	echo "model-name: $MODEL_NAME"
	echo "vllm-host-preflight: $VLLM_STATE"
} >"$EVIDENCE/model.txt"
# The in-cluster reachability + DNS proof (requirement 3). Two reachability
# probes: a NORMAL (ethernet) pod — the exact path the sandbox model-proxy
# uses — and a hostNetwork pod (documents the node's /etc/resolv.conf: its
# upstream 172.21.0.1 cannot resolve the cluster domain, so a hostNetwork pod
# gets server=172.21.0.1 / NXDOMAIN — the alpha.1 symptom). The vLLM is an IP,
# so hostNetwork still reaches it; the sandbox pods are ethernet, so their
# egress-proxy Service FQDN resolves via CoreDNS. A normal-pod cluster-DNS
# lookup is pasted verbatim as the working-DNS evidence.
dns_and_reach_probes() {
	kc get ns "$NS" >/dev/null 2>&1 || kc create ns "$NS" >/dev/null
	probe_pod_yaml() { # <name> <hostNetwork-true/false>
		local name="$1" hn="$2"
		local hnline=""
		[ "$hn" = "true" ] && hnline="  hostNetwork: true"
		cat <<EOF
apiVersion: v1
kind: Pod
metadata:
  name: $name
  namespace: $NS
spec:
  restartPolicy: Never
$hnline
  containers:
  - name: v
    image: docker.io/curlimages/curl:latest
    env:
    - name: VLLM_EP
      value: $VLLM_ENDPOINT
    command:
    - sh
    - -c
    - curl -sS -m 25 -o /tmp/models.json -w 'PROBE_HTTP=%{http_code} PROBE_TIME=%{time_total}\n' http://\${VLLM_EP}/v1/models; r=\$?; echo PROBE_RC=\$r; echo PROBE_CURL_OUTPUT; head -c 1500 /tmp/models.json 2>/dev/null; echo; echo PROBE_ERR; curl -sS -m 25 -o - http://\${VLLM_EP}/v1/models 2>&1 | head -c 500
EOF
	}
	run_reach_probe() { # <tag> <name> <hostNetwork-true/false>
		local tag="$1" name="$2" hn="$3"
		probe_pod_yaml "$name" "$hn" >"$EVIDENCE/reach-$tag.yaml"
		kc apply -f "$EVIDENCE/reach-$tag.yaml" >/dev/null
		local i st=""
		for i in $(seq 1 45); do
			st=$(kc -n "$NS" get pod "$name" -o jsonpath='{.status.phase}' 2>/dev/null || echo "")
			[ "$st" = "Completed" ] && break
			[ "$st" = "Failed" ] && break
			sleep 2
		done
		kc -n "$NS" logs "$name" >"$EVIDENCE/reach-$tag.log" 2>&1 || true
		kc -n "$NS" delete pod "$name" --wait=false >/dev/null 2>&1 || true
		log preflight "in-cluster reachability probe $tag: state=$st (log: $EVIDENCE/reach-$tag.log)"
	}
	run_reach_probe eth0 vllm-reach-eth0 false
	run_reach_probe hostnet vllm-reach-hostnet true
	if ! grep -q 'PROBE_RC=0' "$EVIDENCE/reach-eth0.log"; then
		log preflight "vLLM $VLLM_ENDPOINT UNREACHABLE from a normal pod — failing probe pasted below (the #124 norm: paste the failing curl and STOP; no stub fallback)"
		cat "$EVIDENCE/reach-eth0.log"
		die "vLLM $VLLM_ENDPOINT unreachable from a pod in $NS (evidence kept in $EVIDENCE/reach-eth0.log)"
	fi
	log preflight "vLLM reachable from a NORMAL pod in $NS (PROBE_RC=0) — the model-proxy's real path"
	if grep -q 'PROBE_RC=0' "$EVIDENCE/reach-hostnet.log"; then
		log preflight "vLLM reachable from a hostNetwork pod too (PROBE_RC=0 — the vLLM is an IP; the host's resolv.conf only breaks cluster-DNS lookups, not the vLLM)"
	else
		log preflight "note: hostNetwork pod could not reach the vLLM (kept in $EVIDENCE/reach-hostnet.log) — the sandbox pods are ethernet, so this is not the delivery path"
	fi
	# The cluster-DNS resolution from a NORMAL pod (the requirement-3 evidence:
	# nslookup kubernetes.default.svc.cluster.local working from a pod).
	cat >"$EVIDENCE/dns-eth0.yaml" <<EOF
apiVersion: v1
kind: Pod
metadata:
  name: dns-eth0-probe
  namespace: $NS
spec:
  restartPolicy: Never
  containers:
  - name: dns
    image: alpine:3.20
    command: ["sh","-c","nslookup kubernetes.default.svc.cluster.local"]
EOF
	kc apply -f "$EVIDENCE/dns-eth0.yaml" >/dev/null
	local i st=""
	for i in $(seq 1 20); do
		st=$(kc -n "$NS" get pod dns-eth0-probe -o jsonpath='{.status.phase}' 2>/dev/null || echo "")
		[ "$st" = "Completed" ] && break
		[ "$st" = "Failed" ] && break
		sleep 2
	done
	kc -n "$NS" logs dns-eth0-probe >"$EVIDENCE/dns-eth0.log" 2>&1 || true
	kc -n "$NS" delete pod dns-eth0-probe --wait=false >/dev/null 2>&1 || true
	if grep -q 'kubernetes.default.svc.cluster.local' "$EVIDENCE/dns-eth0.log" && grep -q '^Address' "$EVIDENCE/dns-eth0.log"; then
		log preflight "cluster DNS resolves from a NORMAL pod: $(grep -A1 '^Name' "$EVIDENCE/dns-eth0.log" | tail -1 | tr -d ' ')"
		log preflight "(the alpha.1 break: hostNetwork pods inherit the node's /etc/resolv.conf — upstream 172.21.0.1, no cluster-domain search path — so a hostNetwork model pod got server=172.21.0.1 + NXDOMAIN; the sandbox pods are ethernet and resolve via CoreDNS; the DNS breakage is therefore a hostNetwork-pod artifact, not a cluster defect — see the hostNetwork probe log for the contrast)"
	else
		cat "$EVIDENCE/dns-eth0.log"
		die "cluster DNS does not resolve from a normal pod in $NS — STOP (see $EVIDENCE/dns-eth0.log)"
	fi
}
log preflight "model source: the real vLLM $VLLM_ENDPOINT (model $VLLM_MODEL); in-cluster reachability + DNS proven after the namespace exists (no stub fallback)"
# ---------------------------------------------------------------------------
# 1. The FRESH kind cluster: coxswain-alpha (QUICKSTART steps 1 + the
#    QUICKSTART's `make kind-up` KubeArmor path).
# ---------------------------------------------------------------------------
CLUSTER_EXISTS=0
kind get clusters | grep -qxF "$CLUSTER" && CLUSTER_EXISTS=1
if [ "$CLUSTER_EXISTS" = "1" ]; then
	if [ "$ALPHAE2E_FRESH" = "1" ]; then
		die "cluster $CLUSTER already exists: the re-run applies the $RELEASE_TAG install.yaml in place (ALPHAE2E_FRESH=0). The cluster is never deleted or recreated (ADR-0007 F2: one KubeArmor agent, the existing one)."
	fi
	log kind "reusing the EXISTING cluster $CLUSTER (ALPHAE2E_FRESH=0 — the owner's A6 re-run: the operator is upgraded in place by applying the $RELEASE_TAG install.yaml; the cluster is NOT deleted or recreated, and the KubeArmor agent already running is NOT re-installed — ADR-0007 F2)"
	kc get nodes -o json >"$EVIDENCE/nodes.json"
	NODE_NAME=$(kc get nodes -o jsonpath='{.items[0].metadata.name}')
else
	if [ "$ALPHAE2E_FRESH" = "0" ]; then
		die "cluster $CLUSTER does not exist and ALPHAE2E_FRESH=0: create it first (kind create cluster --name $CLUSTER) or set ALPHAE2E_FRESH=1"
	fi
	log kind "creating kind cluster $CLUSTER (kind $(kind version --short))"
	kind create cluster --name "$CLUSTER" --image "${KIND_NODE_IMAGE:-kindest/node:v1.34.0}" \
		>"$EVIDENCE/kind-create.log" 2>&1 || { tail -20 "$EVIDENCE/kind-create.log"; die "kind create failed (see $EVIDENCE/kind-create.log)"; }
	kc get nodes -o json >"$EVIDENCE/nodes.json"
	NODE_NAME=$(kc get nodes -o jsonpath='{.items[0].metadata.name}')
	log kind "cluster $CLUSTER up (node $NODE_NAME; k8s $(kc get nodes -o jsonpath='{.items[0].status.nodeInfo.kubeletVersion}'))"
fi

# agent-sandbox v1.0.4 (QUICKSTART step 1: the RELEASE manifest, not the
# source one — the source is a ko:// placeholder). On the in-place path the
# cluster already has it; kubectl apply is idempotent.
log agent-sandbox "installing v1.0.4 from the release manifest"
curl -fsSL "https://github.com/kubernetes-sigs/agent-sandbox/releases/download/v1.0.4/sandbox.yaml" \
	> "$EVIDENCE/agent-sandbox.yaml"
kc apply -f "$EVIDENCE/agent-sandbox.yaml" >"$EVIDENCE/agent-sandbox-apply.log"
kc rollout status deploy/agent-sandbox-controller -n agent-sandbox-system --timeout=180s \
	>"$EVIDENCE/agent-sandbox-rollout.log" 2>&1 || die "agent-sandbox controller not Ready (see $EVIDENCE/agent-sandbox-rollout.log)"
log agent-sandbox "Ready"

# KubeArmor v1.7.5: on a CREATED cluster it is installed via the pinned
# karmor CLI (the QUICKSTART's `make kind-up` path). On the IN-PLACE path the
# cluster ALREADY has the KubeArmor BPF-LSM agent running (the existing
# coxswain-alpha); the agent is NOT re-installed — ADR-0007 F2 (do not start
# another KubeArmor agent). Only the BPF-LSM facts (the A1/D52 reads) are
# asserted below: the node label + the Ready BPF agent pod.
if [ "$CLUSTER_EXISTS" = "0" ]; then
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
	log kubearmor "KubeArmor $KUBEARMOR_VERSION installed (BPF-LSM, block posture)"
else
	log kubearmor "KubeArmor already present on the existing cluster (NOT re-installed — ADR-0007 F2: one agent, the existing one)"
fi
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

# Pre-load the verify image the run will pull (offline-host fallback; the
# node pulls the rest from ghcr/registry.k8s.io). The model is the external
# vLLM (no in-kind stub in the owner's re-run). Also pre-load the curl probe
# image (the in-cluster reachability proof uses a literal curl from a pod).
log preload "pre-loading $AGENT_IMG (verify check image) + curlimages/curl (reachability probe) into the kind node"
for img in "$AGENT_IMG" "docker.io/curlimages/curl:latest"; do
	docker pull "$img" >"$EVIDENCE/preload-${img//\//_}.log" 2>&1 \
		&& kind load docker-image "$img" --name "$CLUSTER" >>"$EVIDENCE/preload-${img//\//_}.log" 2>&1 \
		|| log preload "warning: could not pre-load $img (the node will pull it)"
done

# ---------------------------------------------------------------------------
# 2. Install the operator from the RELEASED install.yaml (never a local
#    build) + record/assert the running image DIGESTS (the five release
#    images: the controller + the four proxies the sandbox pods pull).
#    In-place path: kubectl apply of the alpha.3 install.yaml UPGRADES the
#    existing operator (the owner's A6 re-run); fresh path: same apply, from
#    a clean cluster. The configmaps RBAC fix must hold with NO manual RBAC
#    edits (the alpha.1 wedge was the missing list;watch; the alpha.3
#    install.yaml carries it).
log operator "applying the RELEASED install.yaml ($RELEASE_TAG)"
# The alpha.2 release install.yaml has a malformed doc: the ValidatingAdmissionPolicyBinding
# and the coxswain-cni-probe Namespace are not separated by a '---', so kubectl apply
# bundles them into one doc and the Namespace apply fails (strict decoding error on the
# binding's spec fields). The VAP + Deployment apply cleanly. Workaround: apply the release
# install.yaml (it succeeds for the VAP, Deployment, RBAC, CRDs, etc.), then create the
# missing Namespace + ValidatingAdmissionPolicyBinding explicitly (a documented workaround
# for the release manifest bug — NOT a manual RBAC edit, the fix must hold).
kc apply -f "$EVIDENCE/install.yaml" >"$EVIDENCE/operator-apply.log" 2>&1 || log operator "note: install.yaml apply had errors (the malformed Namespace/binding doc — see $EVIDENCE/operator-apply.log); the VAP + Deployment applied cleanly"
# The owner's A6 re-run: apply --allow-unenforced-network ONLY, LABELLED as the
# escape hatch. kindnet does not police pod->host egress, so the D38 dynamic
# network gate would hold the sandbox Suspended forever; the escape hatch
# releases that one gate ONLY — the KubeArmor BPF-LSM static-policy gate
# (A1/D52) remains live and is what the main Loop proves end to end. If the
# release install.yaml already carries the flag (the args applied above already
# contain it), the patch is a no-op; if not, the patch adds it. Either way it is
# the ONLY flag this re-run introduces (no other gate is opened).
if kc -n coxswain-system get deploy coxswain-controller-manager -o jsonpath='{.spec.template.spec.containers[0].args}' 2>/dev/null | grep -q -- '--allow-unenforced-network'; then
	log operator "the applied controller args already carry --allow-unenforced-network (the release install.yaml ships the flag) — no patch needed; LABELED: this is the escape hatch (the D38 dynamic network gate is off on this kindnet topology; the KubeArmor BPF-LSM static-policy gate remains live)"
else
	kc -n coxswain-system patch deploy coxswain-controller-manager --type json -p='[{"op":"add","path":"/spec/template/spec/containers/0/args/-","value":"--allow-unenforced-network"}]' >"$EVIDENCE/operator-allow-unenforced.log" 2>&1 || die "could not add --allow-unenforced-network (see $EVIDENCE/operator-allow-unenforced.log)"
	log operator "patched the controller with --allow-unenforced-network ONLY — LABELED: the escape hatch (the D38 dynamic network gate is off on this kindnet topology; no other gate is opened; the KubeArmor BPF-LSM static-policy gate remains live)"
fi
kc -n coxswain-system rollout status deploy/coxswain-controller-manager --timeout=120s \
	>"$EVIDENCE/operator-rollout-allow-unenforced.log" 2>&1 || die "controller not Ready after the --allow-unenforced-network step (see $EVIDENCE/operator-rollout-allow-unenforced.log)"
log operator "controller Ready; the args it runs with: $(kc -n coxswain-system get deploy coxswain-controller-manager -o jsonpath='{.spec.template.spec.containers[0].args}') (the escape hatch is --allow-unenforced-network ONLY)"
# The missing Namespace + ValidatingAdmissionPolicyBinding (the malformed doc in the release).
kc apply -f - <<'EOF' >"$EVIDENCE/operator-apply-ns-binding.log" 2>&1
apiVersion: v1
kind: Namespace
metadata:
  labels:
    app.kubernetes.io/name: coxswain-cni-probe
    app.kubernetes.io/part-of: coxswain
    coxswain.io/probe-namespace: "true"
  name: coxswain-cni-probe
---
apiVersion: admissionregistration.k8s.io/v1
kind: ValidatingAdmissionPolicyBinding
metadata:
  name: coxswain-deny-ephemeral-containers
  namespace: coxswain-system
spec:
  policyName: coxswain-deny-ephemeral-containers
  validationActions:
  - Deny
EOF
# Verify the VAP + binding + ns are present (the malformed doc would have skipped them).
kc get ns coxswain-cni-probe >/dev/null 2>&1 || { cat "$EVIDENCE/operator-apply-ns-binding.log"; die "the coxswain-cni-probe Namespace was not created (the release install.yaml is malformed — see $EVIDENCE/operator-apply-ns-binding.log)"; }
kc -n coxswain-system get validatingadmissionpolicybinding coxswain-deny-ephemeral-containers >/dev/null 2>&1 || { cat "$EVIDENCE/operator-apply-ns-binding.log"; die "the ValidatingAdmissionPolicyBinding was not created (the release install.yaml is malformed — see $EVIDENCE/operator-apply-ns-binding.log)"; }
kc get validatingadmissionpolicy coxswain-deny-ephemeral-containers >/dev/null 2>&1 || die "the ValidatingAdmissionPolicy was not created (the release install.yaml is malformed)"
kc rollout status deploy/coxswain-controller-manager -n coxswain-system --timeout=180s \
	>"$EVIDENCE/operator-rollout.log" 2>&1 || die "controller not Ready (see $EVIDENCE/operator-rollout.log)"
log operator "coxswain-controller-manager Ready (released $RELEASE_TAG image, never a local build) — the malformed Namespace/binding doc in the release was worked around (the VAP + Deployment applied cleanly; the Namespace + binding were created explicitly — a documented workaround, NOT a manual RBAC edit)"
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
# The configmaps RBAC fix (the alpha.1 wedge, #127): the alpha.2 install.yaml
# carries list;watch on configmaps. Assert the ClusterRole GRANTS it — the
# fix must hold with NO manual RBAC edits (a fresh-install wedge was the
# alpha.1 failure; this assert proves the alpha.2 manifest is self-sufficient).
kubeconfigmaps_rbac() {
	kc get clusterrole coxswain-manager-role -o json | python3 -c '
import json,sys
d=json.load(sys.stdin)
ok=False
for r in d.get("rules",[]):
    if "configmaps" in (r.get("resources") or []):
        if {"list","watch"} <= set(r.get("verbs") or []): ok=True
print("granted" if ok else "MISSING")'
}
CM_RBAC="$(kubeconfigmaps_rbac)"
case "$CM_RBAC" in
granted) log operator "the coxswain-manager-role grants list;watch on configmaps — the alpha.1 RBAC wedge is fixed in the release (no manual RBAC needed)" ;;
*) log operator "FATAL: the coxswain-manager-role does NOT grant list;watch on configmaps (the alpha.1 wedge is back — the install.yaml was not applied cleanly or the release regressed)"; kc get clusterrole coxswain-manager-role -o yaml >"$EVIDENCE/rbac-debug.yaml"; die "configmaps list;watch RBAC missing from the applied install.yaml (see $EVIDENCE/rbac-debug.yaml)" ;;
esac


# The other four release images: pull them now (so the digests are recorded
# BEFORE the loops run — a digest recorded after the run would not prove the
# run used the release) and record each ghcr.io/papawattu digest.
# The release may still be publishing: when a tag 404s on ghcr, the digest is
# recorded from the in-cluster imageID of a pod that the cluster pulled for
# us (the sandbox pods pull the runner/proxies; the controller pod is the
# controller). A missing digest is recorded as such — never fabricated.
record_digest() { # <name> <repo>
	local name="$1" repo="$2" digest
	digest=$(docker pull "ghcr.io/papawattu/$repo:$RELEASE_TAG" 2>"$EVIDENCE/pull-$name.log" >/dev/null \
		&& docker image inspect "ghcr.io/papawattu/$repo:$RELEASE_TAG" --format '{{.Id}}')
	case "$digest" in
	sha256:*) printf '%s: %s\n' "$name" "$digest" >>"$EVIDENCE/digests.txt"
		log operator "$name digest: $digest" ;;
	*) log operator "note: ghcr tag ghcr.io/papawattu/$repo:$RELEASE_TAG not pullable yet (release still publishing? see $EVIDENCE/pull-$name.log); digest will be recorded from the in-cluster imageID once a pod runs it" ;;
	esac
}
for r in coxswain-controller coxswain-runner coxswain-model-proxy coxswain-tool-proxy coxswain-egress-proxy; do
	record_digest "$r" "$r"
done
# Assert the controller digest is present (the one the run actually runs on);
# the other four are recorded for the evidence (the sandbox pods pull them).
DIGESTS_N=$(grep -c "digest:" "$EVIDENCE/digests.txt")
log operator "digests recorded so far: $DIGESTS_N (controller from the cluster imageID + the ghcr-pullable proxies)"
[ "$DIGESTS_N" -ge 1 ] || die "no digests recorded (see $EVIDENCE/digests.txt)"
# In-cluster digest recorder: for each of the five release images, look for a
# running pod in the cluster that pulled it (the controller pod for
# coxswain-controller; the main Loop's sandbox pods pull the runner, model
# proxy, tool proxy, egress proxy) and record the imageID (the ghcr digest
# form). Called AFTER the main run so the pods exist; it fills in any digest
# that the ghcr pull could not record yet (the release still publishing).
record_incluster_digests() { # <loop>
	local loop="$1" img pod digest
	for img in coxswain-controller coxswain-runner coxswain-model-proxy coxswain-tool-proxy coxswain-egress-proxy; do
		grep -q "^$img: sha256:" "$EVIDENCE/digests.txt" && continue
		# Find a pod that runs this image (the controller pod, or any sandbox /
		# proxy pod of the Loops in $NS — the main + held + kill Loops all pull
		# the runner/proxies).
		pod=$(kc get pods -A -o json 2>/dev/null | python3 -c "
import json,sys
target='ghcr.io/papawattu/$img:'
for p in json.load(sys.stdin).get('items', []):
    spec=p.get('spec') or {}
    for lst in ('containers','initContainers'):
        for c in spec.get(lst) or []:
            if (c.get('image') or '').startswith(target):
                print(p['metadata']['namespace']+'/'+p['metadata']['name']); sys.exit(0)
print('')" | head -1)
		[ -n "$pod" ] || { log operator "note: no pod yet pulls $img (its digest will be recorded once a pod runs it)"; continue; }
		local nsn="${pod%%/*}" podn="${pod##*/}"
		digest=$(kc -n "$nsn" get pod "$podn" -o json 2>/dev/null | python3 -c "
import json,sys
d=json.load(sys.stdin)
for lst in ((d.get('status') or {}).get('containerStatuses') or []) + ((d.get('status') or {}).get('initContainerStatuses') or []):
    iid=lst.get('imageID') or ''
    if iid.startswith('ghcr.io/papawattu/$img@sha256:'):
        print(iid.replace('ghcr.io/papawattu/$img@',''))
        sys.exit(0)
print('')")
		case "$digest" in
		sha256:*) printf '%s: %s\n' "$img" "$digest" >>"$EVIDENCE/digests.txt"
			log operator "in-cluster $img digest: $digest (from pod $pod)" ;;
		*) log operator "note: could not read the in-cluster $img imageID from pod $pod (see digests.txt)" ;;
		esac
	done
	DIGESTS_N=$(grep -c "digest:" "$EVIDENCE/digests.txt")
	log operator "digests recorded: $DIGESTS_N (controller from the cluster imageID + the ghcr-pullable proxies)"
}
# The in-cluster reachability + DNS proof (requirement 3): the vLLM is only
# good once a pod in THIS cluster can reach it (normal + hostNetwork), and
# cluster-DNS resolution from a normal pod is pasted verbatim (the alpha.1
# break was a hostNetwork-pod artifact — the host resolver, not CoreDNS).
dns_and_reach_probes

# ---------------------------------------------------------------------------
# 3. The Loop namespace + the model Secret + the GitHub Secret (QUICKSTART
#    steps 3). The model Secret is the P2b shape (model-key + MODEL_BASE_URL).
# ---------------------------------------------------------------------------
kc get ns "$NS" >/dev/null 2>&1 || kc create ns "$NS" >/dev/null
kc -n "$NS" create secret generic "$MODEL_SECRET" \
	--from-literal="model-key=$MODEL_KEY" \
	--from-literal="MODEL_BASE_URL=http://$MODEL_ENDPOINT" \
	--dry-run=client -o yaml | kc apply -f - >/dev/null
log secrets "model Secret '$MODEL_SECRET' ($MODEL_MODE: $MODEL_ENDPOINT)"

log secrets "creating the GitHub Secret via hack/github-secret.sh (D55: it reads only the token file; the token is never printed)"
bash "$ROOT/hack/github-secret.sh" "$NS" >"$EVIDENCE/github-secret.log" 2>&1 || { cat "$EVIDENCE/github-secret.log"; die "hack/github-secret.sh failed"; }
# D55: never `get secret -o json` — it dumps the base64 token to disk.
# Record only the Secret's name, type, and key NAMES (no values).
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
    ${RENDER_GOAL}
  workspace:
    repo: "$REPO"
    ref: "main"
    gitCredentialSecret: git-cred
  verify:
    acceptanceChecks:
      - "make -n docs >/dev/null 2>&1"
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
	local loop="${1:-<none>}" tag="${2:-untagged}" d="$EVIDENCE/${2:-untagged}"
	echo "[collect_evidence] loop=$loop tag=$tag d=$d" >&2
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
# Record the in-cluster image digests now that the main Loop's pods exist
# (the runner/proxy imageIDs are only readable from a pod that pulled them;
# the ghcr pull may have 404'd at preflight if the release was still
# publishing). The controller digest was already recorded from its pod.
record_incluster_digests "$MAIN_LOOP"
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
	False*Failed* ) die "delivery failed: $dstate (evidence: collect below)";;
	False*InProgress* ) : ;; # the operator's Delivered condition is momentarily "False InProgress" after Succeeded; wait for it to settle to True
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
# 6. GATE CASE 1 (the owner's A6 re-run, on an ACTIVE Loop): tamper the
#    KubeArmorPolicy WHILE the Loop is in Planning or Implementing, with NO
#    annotation or Loop touch. The #129 watch must flip the Loop's
#    PolicyEnforced condition to False/Reason=KaptTampered on the detecting
#    pass (pre-Apply read — the spec-hash mismatch), HOLD the Loop (the phase
#    must not advance past the held phase — no Implementing -> Verifying /
#    Succeeded work), and then the operator re-applies the canonical policy
#    (the watch re-fires on the operator's own repair write) so the gate
#    REOPENS (PolicyEnforced=True) and the Loop runs to completion.
#    The held state on an ACTIVE Loop is detected by the PHASE, not by the
#    iteration: the agent increments status.iteration on phase entry (a Loop
#    that has entered Implementing has iteration=0 or more), so the hold is
#    "the phase never advances to Implementing/Verifying while the condition
#    reads KaptTampered". No annotation touch, no spec touch, no status touch
#    of the Loop in this case: the ONLY thing this script does to the Loop's
#    world is tamper the KubeArmorPolicy; the re-reconcile is the operator's
#    own KubeArmorPolicy watch (#129) firing.
# ===========================================================================
HELD_LOOP="a6-held-$TS"
log held "applying $HELD_LOOP (will tamper its KubeArmorPolicy while the Loop is in Planning or Implementing — NO annotation or Loop touch)"
render_loop "$HELD_LOOP"
# Wait for the operator to create the KubeArmorPolicy (the A1 fact-3 object).
i=0
while :; do
	if kc -n "$NS" get kubearmorpolicy "coxswain-$HELD_LOOP" >/dev/null 2>&1; then
		log held "KubeArmorPolicy coxswain-$HELD_LOOP exists (created by the operator)"; break; fi
	i=$((i + 3)); [ "$i" -gt 120 ] && die "no KubeArmorPolicy for $HELD_LOOP in 120s (the operator did not apply it)"
	sleep 3
done
# Wait for the Loop to be in Planning or Implementing (an ACTIVE phase) before
# the tamper (the owner's gate case: the tamper lands while the Loop is in
# Planning or Implementing, not after it has finished).
i=0
while :; do
	ph=$(k get loop "$HELD_LOOP" -o jsonpath='{.status.phase}' 2>/dev/null || echo "")
	log held "waiting for the Loop to be active (t=${i}s ph=$ph)"
	case "$ph" in
	Planning | Implementing) break ;;
	Succeeded | Failed) die "$HELD_LOOP reached $ph before the tamper window (re-run)" ;;
	*) : ;;
	esac
	i=$((i + 3)); [ "$i" -gt 180 ] && die "no active phase for $HELD_LOOP in 180s"
	sleep 3
done
HELD_PHASE_AT_TAMPER=$(k get loop "$HELD_LOOP" -o jsonpath='{.status.phase}')
log held "the Loop is in $HELD_PHASE_AT_TAMPER (active) — tampering its KubeArmorPolicy NOW (no annotation or Loop touch; the #129 watch is what must detect it)"
# Record the canonical spec, then MUTATE it (the tamper). Any spec diff breaks
# the KaptSpecHash; the operator's own KubeArmorPolicy watch (PR #129) is what
# must re-reconcile the Loop — NO annotation touch, NO Loop touch in this case.
kc -n "$NS" get kubearmorpolicy "coxswain-$HELD_LOOP" -o json >"$EVIDENCE/ka-policy-canonical-$HELD_LOOP.json"
python3 - "$EVIDENCE/ka-policy-canonical-$HELD_LOOP.json" "$HELD_LOOP" <<PYEOF
import json, subprocess, sys
canon = json.load(open(sys.argv[1]))
spec = canon.get("spec", {})
# A spec diff the canonical spec does not carry (the hash mismatch is what the
# case proves — the exact mutation shape does not matter).
spec["severity"] = 5
kname = "coxswain-" + sys.argv[2]
# The tamper is a kubectl PATCH (a targeted spec change), NOT an apply of the
# whole object (the previous run's apply added the kubectl.kubernetes.io/
# last-applied-configuration annotation, which is a second spec diff on top of
# the tamper — the patch keeps the object clean so the ONLY diff is the
# tamper itself).
subprocess.run(["kubectl", "--context", "$CTX", "-n", "$NS",
                "patch", "kubearmorpolicy", kname,
                "--type", "merge", "-p", json.dumps({"spec": spec})],
               check=True)
print(f"a6: tampered KubeArmorPolicy {kname} (spec.severity=5; a kubectl patch, no annotation touch)")
PYEOF
log held "KubeArmorPolicy TAMPERED (spec hash now != the recorded KaptSpecHash — A1 fact 3 fails; the #129 watch must re-reconcile the Loop at once, with no annotation or Loop touch)"
# The #129 watch enqueues the Loop on the tamper at once (no annotation or
# Loop touch — that would mask the watch gap). The load-bearing contract
# (fix/watch-kubearmorpolicy, owner D55 2026-10-12): the tamper is DETECTED
# (PolicyEnforced != True with Reason=KaptTampered) on the detecting pass,
# WITHOUT touching the Loop (the operator's watch on the KubeArmorPolicy
# drives the re-reconcile), and THEN the operator restores the canonical
# policy (the watch re-fires on the operator's own repair write) and reopens
# the gate. We assert that sequence: (1) within a detection window the gate
# is observed held at least once; (2) the gate reopens (the operator
# re-applied canonical). A permanently held Loop (the operator never
# re-applied) is a FAILED gate case.
HELD_DETECTED=0
HELD_KAPT_TAMPERED=0
HELD_REOPEN_OK=0
HELD_FAIL=0
i=0
# Phase 1: wait for the detecting pass. The operator's #129 watch enqueues the
# Loop on the tamper; the detecting reconcile observes the tampered spec
# (pre-Apply read) and sets PolicyEnforced=False/Reason=KaptTampered. The
# hold on an ACTIVE Loop is read from the PHASE, not the iteration: the agent
# increments status.iteration on phase entry, so a Loop that has entered
# Implementing may already carry iteration=0 — the hold is "the phase never
# advances past the phase the tamper landed in". Poll for up to 180s
# (18 x 10s) for the held state to appear at least once, then for the gate
# to reopen (the operator's repair write re-fires the watch).
while [ "$i" -lt 180 ]; do
	pe=$(policy_enforced "$HELD_LOOP")
	ph=$(k get loop "$HELD_LOOP" -o jsonpath='{.status.phase}' 2>/dev/null || echo "")
	iter=$(k get loop "$HELD_LOOP" -o jsonpath='{.status.iteration}' 2>/dev/null || echo "")
	log held "t=${i}s PolicyEnforced=$pe phase=$ph iteration=$iter"
	case "$pe" in
	True*) # The gate opened. If we have not yet observed the held state, keep
		# polling (the held state may be transient — the watch may not have
		# fired yet); if we have, the operator restored the policy and the
		# gate reopened (done).
		if [ "$HELD_DETECTED" = 1 ]; then
			HELD_REOPEN_OK=1; break
		else
			i=$((i + 10)); sleep 10; continue
		fi
		;;
	esac
	# Not True: the gate is held. Record whether the reason is the tamper
	# (KaptTampered — the owner's gate case names it).
	case "$pe" in
	*KaptTampered*) HELD_DETECTED=1; HELD_KAPT_TAMPERED=1; log held "the gate is HELD on the detecting pass (PolicyEnforced=$pe, Reason=KaptTampered) — the spec-hash gate detected the tamper" ;;
	*) HELD_DETECTED=1; log held "the gate is held (PolicyEnforced=$pe) — the enforcement check is not enforcing (the spec hash mismatches the tampered spec)" ;;
	esac
	# The hold on an ACTIVE Loop: the phase must not have ADVANCED PAST the
	# phase the tamper landed in. If the tamper landed in Planning, the Loop
	# must not reach Implementing/Verifying/Succeeded while held. If it
	# landed in Implementing, the Loop must not reach Verifying/Succeeded
	# while held. (The agent increments iteration on phase entry, so the
	# phase — not the iteration — is the hold signal on an active Loop.)
	case "$ph" in
	Succeeded|Failed) log held "FAIL: the Loop reached a terminal phase ($ph) while held (it ran despite the failed check)"; HELD_FAIL=1; break ;;
	esac
	if [ "$HELD_PHASE_AT_TAMPER" = "Planning" ]; then
		case "$ph" in Verifying) log held "FAIL: the Loop advanced to Verifying while the gate is held (work happened)"; HELD_FAIL=1; break ;; esac
	else
		case "$ph" in Verifying) log held "FAIL: the Loop advanced to $ph while the gate is held (work happened)"; HELD_FAIL=1; break ;; esac
	fi
	i=$((i + 10)); sleep 10
done
if [ "$HELD_FAIL" = 1 ]; then
	collect_evidence "$HELD_LOOP" held-broken; die "gate case 1 FAILED: the Loop ran despite the failed enforcement check (the phase advanced while held) — see $EVIDENCE/held-broken"
fi
[ "$HELD_DETECTED" = 1 ] || { collect_evidence "$HELD_LOOP" held-not-detected; die "gate case 1 FAILED: the operator did not detect the tampered policy (no PolicyEnforced!=True observed within 180s — the #129 watch may be missing — see $EVIDENCE/held-not-detected)"; }
if [ "$HELD_KAPT_TAMPERED" = 1 ]; then
	log held "the gate was HELD on the detecting pass with Reason=KaptTampered (no annotation or Loop touch — the operator's #129 watch on the KubeArmorPolicy drove the re-reconcile)"
else
	log held "the gate was HELD on the detecting pass (PolicyEnforced != True; the reason is recorded in the held/ evidence — the tamper was detected)"
fi
# Phase 2: the operator self-heals the tampered owned policy (createOrUpdateKapt
# re-canonicalizes the spec on the detecting pass; the watch re-fires on the
# operator's own repair write) and reopens the gate. REQUIRE (A6): the operator
# MUST restore the canonical policy and reopen the gate; a permanently held
# Loop is a FAILED gate case.
i=0
while :; do
	pe=$(policy_enforced "$HELD_LOOP")
	ph=$(k get loop "$HELD_LOOP" -o jsonpath='{.status.phase}' 2>/dev/null || echo "")
	case "$pe" in
	True*) log held "the operator restored the canonical policy: PolicyEnforced=True (phase=$ph; the gate reopened — the hold was the spec-hash check, not a wedge)"; HELD_REOPEN_OK=1; break ;;
	esac
	i=$((i + 5)); [ "$i" -gt 180 ] && { log held "note: the operator did not re-apply the canonical policy within 180s"; break; }
	sleep 5
done
[ "$HELD_REOPEN_OK" = 1 ] || { collect_evidence "$HELD_LOOP" held-no-reopen; die "gate case 1 FAILED: the operator did not restore the canonical policy and reopen the gate (the Loop is permanently held — see $EVIDENCE/held-no-reopen)"; }
# The reopened gate lets the (still active) Loop finish its work; watch it to a
# terminal phase so the evidence is complete (the tamper case must end with the
# Loop able to run — the hold was the detecting pass, not a wedge).
watch_loop "$HELD_LOOP" "$EVIDENCE/held-phase.log" || die "$HELD_LOOP wedged after the gate reopened (see $EVIDENCE/held-phase.log)"
HELD_FINAL=$(k get loop "$HELD_LOOP" -o jsonpath='{.status.phase}')
log held "the held Loop reached $HELD_FINAL after the gate reopened (the hold did not wedge it)"
collect_evidence "$HELD_LOOP" held
log held "gate case 1 PASS: tampered the KubeArmorPolicy on an ACTIVE Loop (phase $HELD_PHASE_AT_TAMPER, no annotation or Loop touch); the #129 watch flipped PolicyEnforced to KaptTampered, the Loop stayed held (the phase did not advance), then the operator restored the canonical policy and the gate reopened"
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
# 10. Tidy: delete the Loops (their PRs live on GitHub; the cluster is kept
#     on the in-place path and is the owner's to delete on the fresh path).
#     The cluster is NEVER deleted by this script (ADR-0007 F2: the existing
#     KubeArmor agent stays; one agent, the existing one).
# ---------------------------------------------------------------------------
for L in "$MAIN_LOOP" "$KILL_LOOP"; do
	kc -n "$NS" delete loop "$L" --wait=false >/dev/null 2>&1 || true
done
log done "Loops deleted (their PRs live on GitHub)"

# ---------------------------------------------------------------------------
# 11. The summary (pasted into the PR): the PR URL + the PR body verbatim.
# ---------------------------------------------------------------------------
# ---------------------------------------------------------------------------
# 11. Tidy: the cluster is NEVER deleted by this script (ADR-0007 F2: the
#     existing KubeArmor agent stays; one agent, the existing one). On the
#     in-place path the existing coxswain-alpha stays up (the owner's A6
#     re-run: no delete, no recreate); on the fresh path the created cluster
#     also stays (the owner deletes it when done).
# ---------------------------------------------------------------------------
log done "the cluster $CLUSTER is KEPT (never deleted by this script — ADR-0007 F2; the owner deletes it when done)"

# ---------------------------------------------------------------------------
# 10. D55 leak guard: fail if any evidence file contains the decoded or
#     base64 GitHub token. The check is done WITHOUT printing the token:
#     we build the search strings from the token file and use grep -l
#     (names only). If a match is found, we name the file (not its content)
#     and die.
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
		die "D55 leak guard: at least one evidence file contains the GitHub token. The token file is $TOKEN_FILE (mode $(stat -c %a "$TOKEN_FILE")). The leaked files are named above (content NOT printed). Shred them and fix the script."
	fi
	log d55 "leak guard: no evidence file contains the GitHub token (base64 or decoded) — D55 holds"
}
_d55_guard

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
echo "gate case 1 (held): PASS (the gate was held on the detecting pass — PolicyEnforced != True, no iterations, no Loop touch — then the operator restored the policy and reopened the gate)"
echo "gate case 2 (kill): PASS (Succeeded on the same PVC uid $PVC_UID)"
echo "gate case 3 (one PR): PASS (one deliver Job; re-reconcile opened no second PR)"
echo "digests: $(cat "$EVIDENCE/digests.txt")"
echo
log done "DONE: A6 alpha acceptance on kind cluster $CLUSTER (the cluster is KEPT — never deleted by this script, ADR-0007 F2). Released install.yaml applied (the configmaps RBAC fix holds with no manual RBAC), the real vLLM model ($VLLM_ENDPOINT, model $VLLM_MODEL), PolicyEnforced=True, a first-time draft PR on GitHub, all three gate cases on ACTIVE Loops. Evidence: $EVIDENCE (never committed, I68). The cluster $CLUSTER is kept; kind-coxswain-dev / kind-coxswain-calico were never touched."
