#!/usr/bin/env bash
# S5b (samples plan section 6): the end-to-end sample driver.
#
#   make sample-run APP=gocli TASK=1
#
# Idempotent. Steps:
#   1. ensure the samples namespace + Gitea is up and seeded (reuses
#      hack/samples-git.sh: up + seed; pinned to kind-coxswain-dev),
#   2. ensure the Loop namespace has the git-credential Secret
#      (samples-git-cred, copied from the seeded samples ns — never from
#      masked output) and the model Secret (vllm-no-auth),
#   3. apply the AgentPolicy and Loop manifests for the task
#      (examples/<app>/tasks/<n>.agentpolicy.yaml, <n>.loop.yaml),
#      deleting a prior Loop first,
#   4. watch status.phase until Succeeded/Failed or TIMEOUT (default 30m),
#   5. write operator-side evidence to .samples/<app>-<n>/EVIDENCE.md.
#
# --dry-run mode: render the task manifests and validate them against the
# live API server with kubectl apply --dry-run=server (CRDs must be
# installed) without creating anything.
#
# Evidence is operator-side only (kubectl): phase events, the per-iteration
# verify Jobs (init-container exit codes + check logs), the tamper result,
# final conditions (including the honest PolicyEnforced/NetworkEnforced
# EnforcementDisabled reasons on coxswain-dev), the model-proxy log, and the
# NetworkPolicies. The agent's claim is recorded for context only — it is
# never the evidence (B3 contract).
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
CTX="${CTX:-kind-coxswain-dev}"
NS="${NS:-samples}"
APP="${APP:-gocli}"
TASK="${TASK:-1}"
TIMEOUT="${TIMEOUT:-1800}"
OUTDIR="${OUTDIR:-$ROOT/.samples/$APP-$TASK}"
MODE="run"

# The seeded git repo is owned by the 'samples' Gitea user.
GIT_USER="samples"

log() { printf '\033[1;34m[sample-run %s/%s]\033[0m %s\n' "$APP" "$TASK" "$*"; }
die() { printf '\033[1;31m[sample-run FATAL]\033[0m %s\n' "$*" >&2; exit 1; }

while [ $# -gt 0 ]; do
	case "$1" in
	--dry-run) MODE="dry-run" ;;
	-h|--help)
		grep '^#' "$0" | sed -n '2,20p'
		exit 0
		;;
	*) die "unknown flag '$1' (supported: --dry-run)" ;;
	esac
	shift
done

KUBECTL=(kubectl --context "$CTX" -n "$NS")

command -v kubectl >/dev/null || die "kubectl is not on PATH"
command -v jq >/dev/null || die "jq is not on PATH"
kubectl --context "$CTX" get nodes >/dev/null 2>&1 \
	|| die "cannot reach cluster context '$CTX' (is the coxswain-dev kind cluster running?)"

LOOP="gocli-task$TASK"
LOOP_YAML="$ROOT/examples/$APP/tasks/$TASK.loop.yaml"
POLICY_YAML="$ROOT/examples/$APP/tasks/$TASK.agentpolicy.yaml"
[ -f "$LOOP_YAML" ] || die "manifest $LOOP_YAML not found (task $TASK not defined)"
[ -f "$POLICY_YAML" ] || die "manifest $POLICY_YAML not found (task $TASK not defined)"

# The policy names referenced by the manifest are applied from the same
# tasks dir; extract them from the manifest so the driver never drifts from
# the checked-in spec.
POLICY_NAMES=$(awk '/^  policyRefs:/ {f=1; next} /^  [a-z]/ {f=0} f && /- / {print $2}' "$LOOP_YAML")

# ---------------------------------------------------------------------------
# dry-run: render + validate against the live API server, create nothing.
# ---------------------------------------------------------------------------
if [ "$MODE" = "dry-run" ]; then
	log "dry-run: validating the task $TASK manifests against --context $CTX (ns $NS)"
	# The Loop manifest carries its own namespace; apply validation still
	# targets the pinned ctx. The namespace must exist for server-side
	# validation of the Secret reference (it is not — the ref is a name
	# only, so a plain dry-run=server is enough).
	kubectl --context "$CTX" apply -f "$POLICY_YAML" -f "$LOOP_YAML" --dry-run=server \
		|| die "server-side validation failed (are the coxswain CRDs installed? 'make install' / config/crd)"
	log "dry-run OK: manifests validate (CRDs accept the shapes; nothing created)"
	exit 0
fi

mkdir -p "$OUTDIR"
log "evidence dir: $OUTDIR"

# ---------------------------------------------------------------------------
# 1. samples-up: Gitea up + seeded (idempotent; pinned to coxswain-dev by
#    hack/samples-git.sh).
# ---------------------------------------------------------------------------
log "ensuring Gitea is up + seeded (hack/samples-git.sh up + seed)..."
GITEA_CTX="$CTX" GITEA_NS="$NS" bash "$ROOT/hack/samples-git.sh" up >/dev/null
GITEA_CTX="$CTX" GITEA_NS="$NS" bash "$ROOT/hack/samples-git.sh" seed
log "Gitea ready"

# ---------------------------------------------------------------------------
# 2. Secrets the Loop needs in its namespace (idempotent; values come from
#    the cluster, never from masked output).
# ---------------------------------------------------------------------------
GIT_CRED_SECRET=$(jq -r '.spec.workspace.gitCredentialSecret // "samples-git-cred"' "$LOOP_YAML")
MODEL_SECRET=$(jq -r '.spec.agent.endpointSecretRef // "vllm-no-auth"' "$LOOP_YAML")

if ! kubectl --context "$CTX" -n "$NS" get secret "$GIT_CRED_SECRET" >/dev/null 2>&1; then
	log "copying git credential secret '$GIT_CRED_SECRET' from the seeded samples ns (ns $NS)..."
	kubectl --context "$CTX" -n "$NS" get secret samples-git-cred -o yaml \
		| jq 'del(.metadata.name, .metadata.namespace, .metadata.uid, .metadata.resourceVersion, .metadata.creationTimestamp, .metadata.annotations)' \
		| jq --arg n "$GIT_CRED_SECRET" '.metadata.name = $n' \
		| kubectl --context "$CTX" -n "$NS" apply -f - >/dev/null
fi
[ "$(kubectl --context "$CTX" -n "$NS" get secret "$GIT_CRED_SECRET" -o jsonpath='{.type}')" = "kubernetes.io/basic-auth" ] \
	|| die "secret $GIT_CRED_SECRET is not kubernetes.io/basic-auth"

if ! kubectl --context "$CTX" -n "$NS" get secret "$MODEL_SECRET" >/dev/null 2>&1; then
	# The model Secret (a no-auth vLLM endpoint: the API key is a dummy).
	# This is dev-only configuration for the local vLLM at the Loop's
	# modelEndpoint; the value is fixed and non-secret.
	log "creating model secret '$MODEL_SECRET' (no-auth vLLM) in ns $NS..."
	printf 'api.key: none\nmodel.name: qwen3.8-27b\n' | kubectl --context "$CTX" -n "$NS" create secret generic "$MODEL_SECRET" \
		--from-file=api.key=- --from-file=model.name=- >/dev/null
fi
# The vLLM endpoint must be reachable from the operator node.
VLLM_HOST_PORT=$(jq -r '.spec.agent.modelEndpoint' "$LOOP_YAML")
log "vLLM endpoint: $VLLM_HOST_PORT (must be reachable from the kind node)"

# ---------------------------------------------------------------------------
# 3. AgentPolicy + Loop (delete a prior Loop first; the AgentPolicy is
#    replaced via apply).
# ---------------------------------------------------------------------------
log "applying AgentPolicy manifest(s)..."
kubectl --context "$CTX" apply -f "$POLICY_YAML"

if kubectl --context "$CTX" -n "$NS" get loop "$LOOP" >/dev/null 2>&1; then
	log "deleting prior loop '$LOOP' (fresh run)..."
	kubectl --context "$CTX" -n "$NS" delete loop "$LOOP" --wait=true --timeout=120s \
		|| kubectl --context "$CTX" -n "$NS" delete loop "$LOOP" --wait=false
	sleep 5
fi
log "applying the Loop manifest ($LOOP_YAML)..."
kubectl --context "$CTX" apply -f "$LOOP_YAML"
kubectl --context "$CTX" -n "$NS" get loop "$LOOP"

# ---------------------------------------------------------------------------
# 4. Watch the phase.
# ---------------------------------------------------------------------------
log "watching phase (timeout ${TIMEOUT}s)..."
DEADLINE=$(( $(date +%s) + TIMEOUT ))
LAST_PHASE=""
while :; do
	[ $(date +%s) -lt "$DEADLINE" ] || die "timeout after ${TIMEOUT}s; last phase: ${LAST_PHASE:-<none>}"
	STATE=$(kubectl --context "$CTX" -n "$NS" get loop "$LOOP" -o json 2>/dev/null \
		| jq -r '"\(.status.phase // "Pending") \(.status.iteration // 0)"' 2>/dev/null || echo "")
	PHASE="${STATE%% *}"
	if [ "$PHASE" != "$LAST_PHASE" ]; then
		log "phase: $PHASE (iteration ${STATE#* })"
		LAST_PHASE="$PHASE"
	fi
	case "$PHASE" in
	Succeeded|Failed) break ;;
	esac
	sleep 10
done
log "final phase: $PHASE"

# ---------------------------------------------------------------------------
# 5. Evidence (operator-side only).
# ---------------------------------------------------------------------------
log "collecting evidence..."
EVID="$OUTDIR/EVIDENCE.md"
EV_LOOP_JSON="$OUTDIR/loop.json"
kubectl --context "$CTX" -n "$NS" get loop "$LOOP" -o json > "$EV_LOOP_JSON"

{
	printf '# S5b evidence — %s task %s\n' "$APP" "$TASK"
	printf -- '- generated: %s\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)"
	printf -- '- cluster: --context %s, ns %s\n' "$CTX" "$NS"
	printf -- '- loop: %s\n' "$LOOP"
	printf -- '- runner image flag: %s\n' \
		"$(kubectl --context "$CTX" -n coxswain-system get deploy coxswain-controller-manager -o jsonpath='{.spec.template.spec.containers[0].args}' | tr ' ' '\n' | grep runner-image || echo '<not found>')"
	printf -- '- model endpoint: %s (real vLLM, no fake model)\n' "$VLLM_HOST_PORT"
	printf '\n'
	printf '## Phases (PhaseAdvanced / PhaseIterated events)\n\n'
	printf '```\n'
	kubectl --context "$CTX" -n "$NS" get events --field-selector "involvedObject.name=$LOOP" --sort-by=.lastTimestamp \
		-o custom-columns=TIME=.lastTimestamp,REASON=.reason,MESSAGE=.message --no-headers || true
	printf '```\n\n'
	printf '## Final phase, conditions, pins\n\n'
	printf '```\n'
	jq -r '.status | {phase, iteration, observedPhase, desiredPhase, baseCommit, currentVerify, verify, progress, policy}' "$EV_LOOP_JSON"
	printf '\nconditions:\n'
	jq -r '.status.conditions[] | "- \(.type)=\(.status) reason=\(.reason): \(.message)"' "$EV_LOOP_JSON"
	printf '```\n\n'

	printf '## Verify Jobs (per iteration: init exit codes + check logs)\n\n'
	# The Job names are <loop>-verify-<n>.
	JOBS=$(kubectl --context "$CTX" -n "$NS" get jobs -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}' | grep "^$LOOP-verify-" | sort -V || true)
	for job in $JOBS; do
		pod=$(kubectl --context "$CTX" -n "$NS" get pods -o name | sed 's|^pod/||' | grep "^${job}-" | head -1)
		printf '### %s\n\n' "$job"
		if [ -n "${pod:-}" ]; then
			printf 'init container exit codes (kubelet-recorded):\n\n```\n'
			kubectl --context "$CTX" -n "$NS" get pod "$pod" -o json \
				| jq -r '.status.initContainerStatuses[]? | "- \(.name): exit=\(.state.terminated.exitCode // "n/a") (\(.state.terminated.reason // "?"))"'
			printf '```\n\n'
			for c in $(kubectl --context "$CTX" -n "$NS" get pod "$pod" -o json | jq -r '.status.initContainerStatuses[]? | select(.name|startswith("check-")) | .name'); do
				printf 'check log (%s, tail 15):\n\n```\n' "$c"
				kubectl --context "$CTX" -n "$NS" logs "$pod" -c "$c" --tail=15 2>&1 || true
				printf '```\n\n'
			done
			printf 'tamper log (tail 5):\n\n```\n'
			kubectl --context "$CTX" -n "$NS" logs "$pod" -c tamper --tail=5 2>&1 || true
			printf '```\n\n'
		fi
	done

	printf '## Model proxy\n\n'
	printf 'forwarded-request line(s) from the proxy pod log (dev stand-in: one line per pod start, not per request):\n\n```\n'
	kubectl --context "$CTX" -n "$NS" logs "pod/${LOOP}-proxy" --tail=20 2>&1 || true
	printf '```\n\n'

	printf '## NetworkPolicies\n\n```\n'
	kubectl --context "$CTX" -n "$NS" get netpol -l "coxswain.io/loop=$LOOP" -o name 2>/dev/null || kubectl --context "$CTX" -n "$NS" get netpol -o name | grep "$LOOP" || true
	kubectl --context "$CTX" -n "$NS" get netpol -o yaml 2>/dev/null | grep -A 40 "name: $LOOP" || true
	printf '```\n\n'

	printf '## Agent claim (CONTEXT ONLY — never the evidence)\n\n'
	printf 'The runner claim (status.progress / observedPhase) is an agent statement; the evidence above (verify Job exit codes, tamper, checks) is the B3 contract.\n\n```\n'
	jq -r '.status | {observedPhase, progress}' "$EV_LOOP_JSON"
	printf '```\n\n'

	printf '## No external pushes\n\n'
	printf 'The workspace repo is the in-cluster Gitea (http://gitea.%s.svc:3000/%s/%s.git). Nothing was pushed to the coxswain GitHub remote; the seeded repos have no github.com remote (S2 acceptance, hack/samples-accept.sh).\n' "$NS" "$GIT_USER" "$APP"
} > "$EVID"

log "evidence written to $EVID"
case "$PHASE" in
Succeeded) log "RESULT: SUCCEEDED (task $TASK)";;
*) log "RESULT: $PHASE (task $TASK) — see $EVID"; exit 2;;
esac
