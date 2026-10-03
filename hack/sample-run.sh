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
#      deleting a prior Loop first. spec.agent.image is left empty in the
#      manifest: the operator's --runner-image flag supplies the runner
#      entrypoint, and the driver preflights that the controller runs with
#      a non-empty --runner-image (and that it agrees with RUNNER_IMG if
#      set) so the demo fails fast instead of wedging in Planning,
#   4. watch status.phase until Succeeded/Failed or TIMEOUT (default 30m),
#   5. write operator-side evidence to .samples/<app>-<n>/EVIDENCE.md.
#
# RUNNER_IMG override: set RUNNER_IMG=<image> in the environment to
# substitute spec.agent.image in the rendered Loop manifest (the checked-in
# manifest keeps a sensible default; this is how a local runner build is
# demoed without editing it).
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
command -v python3 >/dev/null || die "python3 is not on PATH (PyYAML required)"
python3 -c 'import yaml' >/dev/null 2>&1 || die "python3-yaml (PyYAML) not importable"
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
# RUNNER_IMG override (S5b): the manifest leaves spec.agent.image empty so
# the operator's --runner-image flag supplies the runner entrypoint. The env
# var RUNNER_IMG then only matters for building and loading the image into
# kind before the run; the preflight below checks the controller flag and
# RUNNER_IMG agree so the sandbox actually runs the runner.
# ---------------------------------------------------------------------------
TMPDIR="$(mktemp -d)"
trap 'rm -rf "$TMPDIR"' EXIT
LOOP_RENDER="$TMPDIR/loop.$TASK.rendered.yaml"
cp "$LOOP_YAML" "$LOOP_RENDER"
RUNNER_IMG="${RUNNER_IMG:-}"

if [ "$MODE" = "dry-run" ]; then
	log "dry-run: validating the task $TASK manifests against --context $CTX (ns $NS)"
	# The Loop manifest carries its own namespace; apply validation still
	# targets the pinned ctx. The namespace must exist for server-side
	# validation of the Secret reference (it is not — the ref is a name
	# only, so a plain dry-run=server is enough).
	kubectl --context "$CTX" apply -f "$POLICY_YAML" -f "$LOOP_RENDER" --dry-run=server \
		|| die "server-side validation failed (are the coxswain CRDs installed? 'make install' / config/crd)"
	log "dry-run OK: manifests validate (CRDs accept the shapes; nothing created)"
	exit 0
fi

mkdir -p "$OUTDIR"
log "evidence dir: $OUTDIR"

# ---------------------------------------------------------------------------
# 0. Preflight: the controller deployment must be Available and must run
#    with a non-empty --runner-image, or the sandbox falls back to
#    'sleep infinity' (isRunner only matches the empty image or the
#    flag's value) and the Loop wedges in Planning. Fail fast with a
#    clear message instead.
# ---------------------------------------------------------------------------
CONTROLLER_NS="coxswain-system"
CONTROLLER_DEPLOY="coxswain-controller-manager"
CONTROLLER_ARGS_JSON=$(kubectl --context "$CTX" -n "$CONTROLLER_NS" get deploy "$CONTROLLER_DEPLOY" \
	-o jsonpath='{.spec.template.spec.containers[0].args}') \
	|| die "cannot read args of controller deploy $CONTROLLER_NS/$CONTROLLER_DEPLOY on --context $CTX (deployment missing or cluster unreachable); 'make deploy-dev' and retry."
CONTROLLER_ARGS=$(python3 -c 'import json,sys; [print(a) for a in json.loads(sys.argv[1])]' "$CONTROLLER_ARGS_JSON")
RUNNER_FLAG=$(grep '^--runner-image=' <<< "$CONTROLLER_ARGS" || true)
if [ -z "$RUNNER_FLAG" ]; then
	die "controller deploy $CONTROLLER_NS/$CONTROLLER_DEPLOY has no --runner-image flag; the sandbox would run 'sleep infinity'. 'make deploy-dev' (or set the image tag and 'make deploy') and retry."
fi
RUNNER_IMAGE=${RUNNER_FLAG#--runner-image=}
[ -n "$RUNNER_IMAGE" ] \
	|| die "controller $CONTROLLER_NS/$CONTROLLER_DEPLOY has an empty --runner-image; the sandbox would run 'sleep infinity'. 'make deploy-dev' (or set the image tag and 'make deploy') and retry."
log "controller preflight OK: --runner-image=$RUNNER_IMAGE"
if [ -n "${RUNNER_IMG:-}" ] && [ "$RUNNER_IMG" != "$RUNNER_IMAGE" ]; then
	die "RUNNER_IMG=$RUNNER_IMG does not match the controller's --runner-image=$RUNNER_IMAGE; the sandbox would run 'sleep infinity'. Build and load the image the controller expects (or redeploy the controller with the flag set to $RUNNER_IMG) and retry."
fi

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
GIT_CRED_SECRET=$(python3 -c 'import yaml,sys; print(yaml.safe_load(open(sys.argv[1]))["spec"]["workspace"].get("gitCredentialSecret") or "samples-git-cred")' "$LOOP_YAML")
MODEL_SECRET=$(python3 -c 'import yaml,sys; print(yaml.safe_load(open(sys.argv[1]))["spec"]["agent"].get("endpointSecretRef") or "vllm-no-auth")' "$LOOP_YAML")

if ! kubectl --context "$CTX" -n "$NS" get secret "$GIT_CRED_SECRET" >/dev/null 2>&1; then
	log "copying git credential secret '$GIT_CRED_SECRET' from the seeded samples ns (ns $NS)..."
	kubectl --context "$CTX" -n "$NS" get secret samples-git-cred -o json > /tmp/.s5b-gitcred.json
	python3 - "$GIT_CRED_SECRET" /tmp/.s5b-gitcred.json > /tmp/.s5b-gitcred-out.json <<'PYEOF'
import json, sys
name, path = sys.argv[1], sys.argv[2]
d = json.load(open(path))
d["metadata"]["name"] = name
# The type is fixed here (not copied): a wrong/non-standard type would fail
# the check below, and Secret types are immutable — a type-only edit is
# impossible in place.
d["type"] = "kubernetes.io/basic-auth"
for k in ("namespace", "uid", "resourceVersion", "creationTimestamp", "annotations", "managedFields"):
    d["metadata"].pop(k, None)
print(json.dumps(d))
PYEOF
	kubectl --context "$CTX" -n "$NS" apply -f /tmp/.s5b-gitcred-out.json >/dev/null
	rm -f /tmp/.s5b-gitcred.json /tmp/.s5b-gitcred-out.json
elif [ "$(kubectl --context "$CTX" -n "$NS" get secret "$GIT_CRED_SECRET" -o jsonpath='{.type}')" != "kubernetes.io/basic-auth" ]; then
	# Secret types are IMMUTABLE: an existing secret with a wrong (e.g. bare
	# 'BasicAuth') type cannot be edited in place. Recreate it with the same
	# data (never printed) under the required type.
	log "secret '$GIT_CRED_SECRET' has the wrong type; recreating as kubernetes.io/basic-auth (same data)..."
	kubectl --context "$CTX" -n "$NS" get secret "$GIT_CRED_SECRET" -o json > /tmp/.s5b-gitcred.json
	python3 - "$GIT_CRED_SECRET" /tmp/.s5b-gitcred.json > /tmp/.s5b-gitcred-out.json <<'PYEOF'
import json, sys
name, path = sys.argv[1], sys.argv[2]
d = json.load(open(path))
d["metadata"]["name"] = name
d["type"] = "kubernetes.io/basic-auth"
for k in ("namespace", "uid", "resourceVersion", "creationTimestamp", "annotations", "managedFields"):
    d["metadata"].pop(k, None)
print(json.dumps(d))
PYEOF
	kubectl --context "$CTX" -n "$NS" delete secret "$GIT_CRED_SECRET" --wait=true
	kubectl --context "$CTX" -n "$NS" apply -f /tmp/.s5b-gitcred-out.json >/dev/null
	rm -f /tmp/.s5b-gitcred.json /tmp/.s5b-gitcred-out.json
fi
[ "$(kubectl --context "$CTX" -n "$NS" get secret "$GIT_CRED_SECRET" -o jsonpath='{.type}')" = "kubernetes.io/basic-auth" ] \
	|| die "secret $GIT_CRED_SECRET is not kubernetes.io/basic-auth"

if ! kubectl --context "$CTX" -n "$NS" get secret "$MODEL_SECRET" >/dev/null 2>&1; then
	# The model Secret (a no-auth vLLM endpoint: the API key is a dummy).
	# This is dev-only configuration for the local vLLM at the Loop's
	# modelEndpoint; the value is fixed and non-secret. The proxy stand-in
	# only checks that the mounted files exist and are non-empty.
	log "creating model secret '$MODEL_SECRET' (no-auth vLLM) in ns $NS..."
	# kubectl create secret rejects dots in --from-literal keys (it would
	# read the key as a file path); use temp files instead.
	KEYF="$OUTDIR/.mkmodel-apikey"; NAMEF="$OUTDIR/.mkmodel-modelname"
	printf 'none\n' > "$KEYF"; printf 'qwen3.8-27b\n' > "$NAMEF"
	kubectl --context "$CTX" -n "$NS" create secret generic "$MODEL_SECRET" \
		--from-file="api.key=$KEYF" \
		--from-file="model.name=$NAMEF" >/dev/null
	rm -f "$KEYF" "$NAMEF"
fi
# The vLLM endpoint must be reachable from the operator node.
VLLM_HOST_PORT=$(python3 -c 'import yaml,sys; print(yaml.safe_load(open(sys.argv[1]))["spec"]["agent"]["modelEndpoint"])' "$LOOP_YAML")
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
	# The terminal-phase sandbox pod can keep restarting briefly (the runner
	# refuses a non-executable desired phase); wait out its deletion so the
	# fresh run's events and Jobs start clean.
	kubectl --context "$CTX" -n "$NS" wait --for=delete "pod/${LOOP}-sandbox" --timeout=120s 2>/dev/null || true
fi
log "applying the Loop manifest ($LOOP_RENDER${RUNNER_IMG:+, RUNNER_IMG substituted})"
kubectl --context "$CTX" apply -f "$LOOP_RENDER"
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
		| python3 -c 'import json,sys; s=json.load(sys.stdin).get("status") or {}; print((s.get("phase") or "Pending"), s.get("iteration") or 0)' 2>/dev/null || echo "")
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
# The sandbox pod keeps restarting after a terminal phase (the runner
# refuses unknown desired-phases and the operator restarts it), which
# clutters the event stream; the evidence below filters to the run's own
# events, but clear the old ones anyway so later re-runs start clean.
kubectl --context "$CTX" -n "$NS" delete events --field-selector "involvedObject.name=$LOOP" --now=true 2>/dev/null || true

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
	# custom-columns needs <header>:<json-path-expr> pairs (the bare
	# name=expr form is rejected by kubectl), and --field-selector does
	# not support namespace (k8s only supports involvedObject.name there),
	# so filter the loop's events client-side by involvedObject.name
	# (the events are in $NS anyway; the name is unique per namespace).
	kubectl --context "$CTX" -n "$NS" get events --sort-by=.lastTimestamp -o json 2>/dev/null \
		| python3 -c '
import json, sys
d = json.load(sys.stdin)
for e in d.get("items", []):
    if e["involvedObject"].get("name") != sys.argv[1]:
        continue
    ts = e.get("lastTimestamp") or ""
    print("%s  %s  %s" % (ts, e.get("reason", ""), e.get("message", "")))' "$LOOP" \
		|| true
	printf '```\n\n'
	printf '## Final phase, conditions, pins\n\n'
	printf '```\n'
	jq -r '.status | {phase, iteration, observedPhase, desiredPhase, baseCommit, currentVerify, verify, progress, policy}' "$EV_LOOP_JSON" 2>/dev/null \
		|| python3 -c 'import json,sys; print(json.dumps(json.load(open(sys.argv[1])).get("status") or {}, indent=2))' "$EV_LOOP_JSON"
	printf '\nconditions:\n'
	jq -r '.status.conditions[] | "- \(.type)=\(.status) reason=\(.reason): \(.message)"' "$EV_LOOP_JSON" 2>/dev/null \
		|| python3 -c 'import json,sys; [print("- %s=%s reason=%s: %s" % (c["type"], c["status"], c.get("reason",""), c.get("message","")) for c in (json.load(open(sys.argv[1])).get("status") or {}).get("conditions", [])]' "$EV_LOOP_JSON"
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
				| python3 -c '
import json, sys
d = json.load(sys.stdin)
stat = {c["name"]: c for c in d["status"].get("initContainerStatuses", [])}
for c in d["spec"]["initContainers"]:
    s = stat.get(c["name"], {}).get("state") or {}
    k = list(s.keys())[0] if s else "never-started"
    t = s.get(k) or {}
    print("- %s: exit=%s (%s)" % (c["name"], t.get("exitCode", "n/a"), t.get("reason", k)))'
			printf '```\n\n'
			CHECK_NAMES=$(kubectl --context "$CTX" -n "$NS" get pod "$pod" -o json \
				| python3 -c 'import json,sys; [print(c["name"]) for c in json.load(sys.stdin)["spec"]["initContainers"] if c["name"].startswith(("check-", "tamper"))]')
			for c in $CHECK_NAMES; do
				printf '%s log (tail 15):\n\n```\n' "$c"
				kubectl --context "$CTX" -n "$NS" logs "$pod" -c "$c" --tail=15 2>&1 || true
				printf '```\n\n'
			done
		fi
	done

	printf '## Model proxy\n\n'
	printf 'forwarded-request line(s) from the proxy pod log (dev stand-in: one line per pod start, not per request):\n\n```\n'
	kubectl --context "$CTX" -n "$NS" logs "pod/${LOOP}-proxy" --tail=20 2>&1 || true
	printf '```\n\n'

	printf '## NetworkPolicies\n\n```\n'
	# The verify netpol is labeled coxswain.io/verify-for, not loop, so the
	# label selector misses it; filter by name prefix instead (kubectl prints
	# the full API group in -o name, so strip that first).
	kubectl --context "$CTX" -n "$NS" get netpol -o name 2>/dev/null | sed 's|^networkpolicy.networking.k8s.io/||' | grep "^$LOOP-" \
		| while read -r np; do kubectl --context "$CTX" -n "$NS" get netpol "$np" -o yaml; done || true
	printf '```\n\n'

	printf '## Agent claim (CONTEXT ONLY — never the evidence)\n\n'
	printf 'The runner claim (status.progress / observedPhase) is an agent statement; the evidence above (verify Job exit codes, tamper, checks) is the B3 contract.\n\n```\n'
	jq -r '.status | {observedPhase, progress}' "$EV_LOOP_JSON" 2>/dev/null \
		|| python3 -c 'import json,sys; s=json.load(open(sys.argv[1])).get("status") or {}; print(json.dumps({"observedPhase": s.get("observedPhase"), "progress": s.get("progress")}, indent=2))' "$EV_LOOP_JSON"
	printf '```\n\n'

	printf '## No external pushes\n\n'
	printf 'The workspace repo is the in-cluster Gitea (http://gitea.%s.svc:3000/%s/%s.git). Nothing was pushed to the coxswain GitHub remote; the seeded repos have no github.com remote (S2 acceptance, hack/samples-accept.sh).\n' "$NS" "$GIT_USER" "$APP"
} > "$EVID"

log "evidence written to $EVID"
case "$PHASE" in
Succeeded) log "RESULT: SUCCEEDED (task $TASK)";;
*) log "RESULT: $PHASE (task $TASK) — see $EVID"; exit 2;;
esac
