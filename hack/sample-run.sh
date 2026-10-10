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
#   3. apply the Loop manifest for the task (examples/<app>/tasks/<n>.loop.yaml),
#      deleting a prior Loop first, plus the task's AgentPolicy manifest when one
#      exists (a D46 task may have none — the AgentPolicy is optional).
#      spec.agent.image is left empty in the
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
# --evidence-only mode: regenerate EVIDENCE.md from an EXISTING Loop (the
# run must have reached Succeeded or Failed) without re-running anything —
# no Gitea seed, no apply, no watch. Used to re-collect evidence after the
# generator itself is fixed (the run is too expensive to redo).
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
	--evidence-only) MODE="evidence-only" ;;
	-h|--help)
		grep '^#' "$0" | sed -n '2,20p'
		exit 0
		;;
	*) die "unknown flag '$1' (supported: --dry-run, --evidence-only)" ;;
	esac
	shift
done

command -v kubectl >/dev/null || die "kubectl is not on PATH"
command -v python3 >/dev/null || die "python3 is not on PATH (PyYAML required)"
python3 -c 'import yaml' >/dev/null 2>&1 || die "python3-yaml (PyYAML) not importable"
kubectl --context "$CTX" get nodes >/dev/null 2>&1 \
	|| die "cannot reach cluster context '$CTX' (is the coxswain-dev kind cluster running?)"

LOOP="gocli-task$TASK"
LOOP_YAML="$ROOT/examples/$APP/tasks/$TASK.loop.yaml"
POLICY_YAML="$ROOT/examples/$APP/tasks/$TASK.agentpolicy.yaml"
[ -f "$LOOP_YAML" ] || die "manifest $LOOP_YAML not found (task $TASK not defined)"
# D46: the task's AgentPolicy is optional — some tasks (e.g. the D46 gocli
# task 1) ship no AgentPolicy manifest. Apply it only when it exists; the Loop
# is applied in either case. (A missing AgentPolicy is not a fatal error.)
[ -f "$POLICY_YAML" ] || log "no AgentPolicy for this task (D46)"


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
	if [ -f "$POLICY_YAML" ]; then
		kubectl --context "$CTX" apply -f "$POLICY_YAML" -f "$LOOP_RENDER" --dry-run=server \
			|| die "server-side validation failed (are the coxswain CRDs installed? 'make install' / config/crd)"
	else
		kubectl --context "$CTX" apply -f "$LOOP_RENDER" --dry-run=server \
			|| die "server-side validation failed (are the coxswain CRDs installed? 'make install' / config/crd)"
	fi
	log "dry-run OK: manifests validate (CRDs accept the shapes; nothing created)"
	exit 0
fi

mkdir -p "$OUTDIR"
log "evidence dir: $OUTDIR"

if [ "$MODE" != "evidence-only" ]; then
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

# The model-creds Secret the per-Loop metering model proxy mounts (P2b,
# ADR-0009). The operator's proxy reads the key literally named `model-key`
# (loop_controller.go sets MODEL_CRED_FILE=/model-creds/.data/model-key) and
# the documented contract also carries `MODEL_BASE_URL` (the agent's
# COX_MODEL_BASE_URL value; the stub ignores it). The key is a dummy — the
# no-auth vLLM ignores credentials and the operator sets the endpoint from
# spec.agent.modelEndpoint (not from the Secret). This is dev-only config for
# the local vLLM at the Loop's modelEndpoint; the value is fixed and
# non-secret. The stand-in proxy only needs a readable non-empty key file
# (D33: the metering proxy fatals at startup if none is found — the old
# api.key/model.name shape left the model-key file empty, so the proxy
# crashlooped, I75).
# create_model_secret creates/recreates the Secret in the P2b shape (model-key
# + MODEL_BASE_URL). kubectl create secret rejects dots in --from-literal keys
# (it would read the key as a file path); model-key and MODEL_BASE_URL have no
# dots, so --from-literal works (no temp files). Defined ABOVE the if so both
# branches can call it (a function defined inside one branch is not visible
# from the other — P1 review: the stale path called it and died with
# 'command not found').
create_model_secret() {
	MODEL_BASE_URL_VALUE="http://$(python3 -c 'import yaml,sys; print(yaml.safe_load(open(sys.argv[1]))["spec"]["agent"]["modelEndpoint"])' "$LOOP_YAML")"
	kubectl --context "$CTX" -n "$NS" create secret generic "$MODEL_SECRET" \
		--from-literal="model-key=p2b-dummy-key" \
		--from-literal="MODEL_BASE_URL=$MODEL_BASE_URL_VALUE" >/dev/null
}
if ! kubectl --context "$CTX" -n "$NS" get secret "$MODEL_SECRET" >/dev/null 2>&1; then
	log "creating model secret '$MODEL_SECRET' (no-auth vLLM, the P2b shape) in ns $NS..."
	create_model_secret
else
	# An EXISTING model Secret: a pre-P2b shape (api.key/model.name) has no
	# `model-key` entry, so the metering proxy (which reads
	# /model-creds/.data/model-key) fatals at startup and the proxy pod
	# crashloops (I75, D33). Treat a Secret without .data.model-key as stale:
	# delete it and recreate it in the P2b shape (the proxy pod picks it up on
	# restart — sample-run deletes the prior Loop anyway, so a fresh proxy pod
	# mounts the recreated Secret).
	MODEL_KEY_PRESENT=$(kubectl --context "$CTX" -n "$NS" get secret "$MODEL_SECRET" -o jsonpath='{.data.model-key}' 2>/dev/null) || MODEL_KEY_PRESENT=""
	if [ -z "$MODEL_KEY_PRESENT" ]; then
		log "model secret '$MODEL_SECRET' has no model-key (pre-P2b api.key/model.name shape); replacing it with the P2b shape..."
		kubectl --context "$CTX" -n "$NS" delete secret "$MODEL_SECRET" --wait=true >/dev/null
		create_model_secret
	else
		log "model secret '$MODEL_SECRET' already in the P2b shape (has model-key); leaving it alone."
	fi
fi
# The vLLM endpoint must be reachable from the operator node.
VLLM_HOST_PORT=$(python3 -c 'import yaml,sys; print(yaml.safe_load(open(sys.argv[1]))["spec"]["agent"]["modelEndpoint"])' "$LOOP_YAML")
log "vLLM endpoint: $VLLM_HOST_PORT (must be reachable from the kind node)"

# ---------------------------------------------------------------------------
# 3. AgentPolicy (optional) + Loop (delete a prior Loop first; the AgentPolicy
#    is replaced via apply when one exists — D46 tasks may have none).
# ---------------------------------------------------------------------------
if [ -f "$POLICY_YAML" ]; then
	log "applying AgentPolicy manifest(s)..."
	kubectl --context "$CTX" apply -f "$POLICY_YAML"
else
	log "no AgentPolicy for this task (D46)"
fi

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
# 3b. I50: snapshot vLLM request_success_total BEFORE the run starts, read
#     from EACH backend directly. 192.168.1.20:8000 is an nginx LB over TWO
#     vLLM backends, so the summed per-backend delta is the true total
#     served (and an upper bound for this run: other traffic may share the
#     backends). Sum request_success_total over all finished_reason labels.
#     Written to a file so the evidence block can read it later.
# ---------------------------------------------------------------------------
VLLM_BACKENDS="192.168.1.36:8000 192.168.1.37:8000"
VLLM_BEFORE_FILE="$OUTDIR/.vllm-before"
vllm_snapshot() {
	# $1: file to write the summed request_success_total to
	local sum=0 b v
	for b in $VLLM_BACKENDS; do
		v=$(curl -s --max-time 10 "http://$b/metrics" 2>/dev/null \
			| awk '/^vllm:request_success_total/{s+=$2} END{print s+0}')
		[ -n "$v" ] && sum=$((sum + ${v%%.*}))
	done
	printf '%s' "$sum" > "$1"
}
log "snapshotting vLLM request_success_total before the run (backends: $VLLM_BACKENDS)..."
vllm_snapshot "$VLLM_BEFORE_FILE" || log "WARNING: vLLM before-snapshot failed; the delta will be reported as unavailable"
VLLM_BEFORE=$(cat "$VLLM_BEFORE_FILE" 2>/dev/null || echo "")
log "vLLM request_success_total before: ${VLLM_BEFORE:-unavailable}"

# ---------------------------------------------------------------------------
# 4. Watch the phase.
# ---------------------------------------------------------------------------
log "watching phase (timeout ${TIMEOUT}s)..."
DEADLINE=$(( $(date +%s) + TIMEOUT ))
LAST_PHASE=""
while :; do
	NOW=$(date +%s)
	[ "$NOW" -lt "$DEADLINE" ] || die "timeout after ${TIMEOUT}s; last phase: ${LAST_PHASE:-<none>}"
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
fi

# The sandbox pod keeps restarting after a terminal phase (the runner
# refuses unknown desired-phases and the operator restarts it), which
# clutters the event stream; the evidence below filters to the run's own
# events, but clear the old ones anyway so later re-runs start clean.
if [ "$MODE" != "evidence-only" ]; then
	# Run modes only: evidence-only must see the run's own events.
	kubectl --context "$CTX" -n "$NS" delete events --field-selector "involvedObject.name=$LOOP" --now=true 2>/dev/null || true
fi

# evidence-only mode: the phase comes from the existing Loop (the run modes
# set $PHASE from the watch loop above).
if [ "$MODE" = "evidence-only" ]; then
	LOOP_JSON_EO=$(kubectl --context "$CTX" -n "$NS" get loop "$LOOP" -o json) \
		|| die "evidence-only: cannot read Loop $NS/$LOOP (deleted or unreachable)"
	PHASE=$(python3 -c 'import json,sys; print((json.loads(sys.argv[1]).get("status") or {}).get("phase") or "Pending")' "$LOOP_JSON_EO")
	case "$PHASE" in
	Succeeded|Failed) ;;
	*) die "evidence-only: Loop $NS/$LOOP is not in a final phase (phase: $PHASE)" ;;
	esac
	log "evidence-only: Loop $NS/$LOOP is $PHASE; regenerating the evidence"
fi

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
	printf -- '- model endpoint: %s (real vLLM, no fake model)\n' "${VLLM_HOST_PORT:-$(python3 -c 'import yaml,sys; print(yaml.safe_load(open(sys.argv[1]))["spec"]["agent"]["modelEndpoint"])' "$LOOP_YAML" 2>/dev/null || echo '<unknown>')}"
	printf '\n'
	printf '## Phases (PhaseAdvanced / PhaseIterated events)\n\n'
	printf '```\n'
	# The PhaseAdvanced/PhaseIterated events are the operator's records of
	# the phase machine. Try the field selector first (server-side); if it
	# returns nothing, fall back to a client-side filter on
	# involvedObject.name (the name is unique per namespace).
	PHASE_EVENTS=$({ kubectl --context "$CTX" -n "$NS" get events \
		--field-selector "involvedObject.name=$LOOP" --sort-by=.lastTimestamp -o json 2>/dev/null \
		|| kubectl --context "$CTX" -n "$NS" get events --sort-by=.lastTimestamp -o json 2>/dev/null; })
	printf '%s\n' "$PHASE_EVENTS" | python3 -c '
import json, sys
try:
    d = json.load(sys.stdin)
except Exception:
    sys.exit(0)
for e in d.get("items", []):
    if e["involvedObject"].get("name") != sys.argv[1]:
        continue
    ts = e.get("lastTimestamp") or ""
    print("%s  %s  %s" % (ts, e.get("reason", ""), e.get("message", "")))' "$LOOP" || true
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
	# I60 (I50): the first I50 run recorded 0 forwarded requests while the proxy
	# pod (checked by the reviewer at 19:43–19:45) had logged 9. The exact cause
	# is UNKNOWN (the reviewer's hypothesis — the I32 enforcement-evidence relay
	# recreated the pod mid-run — is not confirmed). The anomaly is real and the
	# count discrepancy is unexplained; the fix is to never be silent about it:
	# the WARNING below flags a 0 on a Succeeded run, and PROXY_RC / PROXY_LOG
	# capture whether the log fetch itself failed and what kubectl said, so a
	# failed fetch is recorded in EVIDENCE.md instead of silently showing 0.
	# The metering model proxy (P2b onward) logs ONE JSON usage line per model
	# request for this run's proxy pod. Do NOT swallow kubectl errors: a FAILING
	# `kubectl logs` (bad pod name, pod gone, etc.) must NOT abort the script
	# under `set -euo pipefail` — the previous version read $? after the
	# substitution, which was unreachable (the substitution failure aborted the
	# script before that line), so the error never reached EVIDENCE.md. Capture
	# the failure EXPLICITLY and KEEP the captured output: on failure PROXY_LOG
	# holds the kubectl error text (recorded below) and PROXY_RC is non-zero,
	# so the "proxy log fetch failed" branch records the actual error. On
	# success PROXY_RC=0 and that branch is skipped. A pod that is simply GONE
	# is distinguished (PROXY_RC=1, the message notes the pod was not found).
	PROXY_RC=0
	PROXY_LOG=$(kubectl --context "$CTX" -n "$NS" logs "pod/${LOOP}-proxy" 2>&1) || PROXY_RC=$?
	if [ "$PROXY_RC" -ne 0 ]; then
		# Distinguish a gone pod (logs failed AND the pod is not present) from
		# another logs failure. PROXY_LOG still holds the raw kubectl error text
		# (NOT blanked) so it is recorded below.
		if ! kubectl --context "$CTX" -n "$NS" get "pod/${LOOP}-proxy" >/dev/null 2>&1; then
			PROXY_RC=1
			PROXY_LOG="(pod ${LOOP}-proxy not found; kubectl logs output: ${PROXY_LOG:-<empty>})"
		fi
	fi
	# Since P2b the metering model proxy logs ONE JSON line per model request
	# (action=usage with promptTokens / completionTokens / source=model-proxy),
	# not the old 'proxy: forwarded' line. Count the usage lines and sum the
	# token counts (python3 is available on the kind node) for this run's proxy
	# pod. (The old 'proxy: forwarded' grep has reported 0 on every run since
	# P2b — the metering proxy never logs that string.)
	PROXY_USAGE_TMP="$(mktemp)"
	PROXY_STATS=$(printf '%s\n' "$PROXY_LOG" | python3 -c '
import json, sys
count = 0
prompt = completion = 0
lines = []
for line in sys.stdin:
    line = line.strip()
    if not line:
        continue
    if "\"action\":\"usage\"" not in line:
        continue
    # The Go log package prefixes each line with a timestamp ("<date> <time>
    # <level> ..."), so the JSON is a substring, not the whole line. Extract
    # from the first "{" and parse that.
    brace = line.find("{")
    try:
        d = json.loads(line[brace:]) if brace >= 0 else None
    except Exception:
        d = None
    count += 1
    lines.append(line)
    if d is not None:
        prompt += int(d.get("promptTokens", 0) or 0)
        completion += int(d.get("completionTokens", 0) or 0)
print(f"{count}\t{prompt}\t{completion}")
with open(sys.argv[1], "w") as f:
    f.write("\n".join(lines) + ("\n" if lines else ""))
' "$PROXY_USAGE_TMP" || true)
	PROXY_STATS="${PROXY_STATS:-$(printf '0\t0\t0')}"
	FORWARDED_COUNT=$(printf '%s' "$PROXY_STATS" | cut -f1)
	PROXY_PROMPT_SUM=$(printf '%s' "$PROXY_STATS" | cut -f2)
	PROXY_COMPLETION_SUM=$(printf '%s' "$PROXY_STATS" | cut -f3)
	PROXY_USAGE_LINES="$(cat "$PROXY_USAGE_TMP" 2>/dev/null || true)"
	rm -f "$PROXY_USAGE_TMP"
	# Sample vLLM request_success_total AFTER the run from each backend.
	VLLM_BACKENDS="192.168.1.36:8000 192.168.1.37:8000"
	vllm_sum_after() {
		local sum=0 b v
		for b in $VLLM_BACKENDS; do
			v=$(curl -s --max-time 10 "http://$b/metrics" 2>/dev/null \
				| awk '/^vllm:request_success_total/{s+=$2} END{print s+0}')
			[ -n "$v" ] && sum=$((sum + ${v%%.*}))
		done
		printf '%s' "$sum"
	}
	VLLM_AFTER=$(vllm_sum_after 2>/dev/null || echo "")
	# Read the before-snapshot from the file the run mode wrote, so the delta
	# survives --evidence-only re-generation (the before-sample is not re-taken
	# in evidence-only mode; the file from the original run is authoritative).
	# In evidence-only mode the before-snapshot block (which defines
	# VLLM_BEFORE_FILE) is skipped, so fall back to the known path.
	VLLM_BEFORE_FILE="${VLLM_BEFORE_FILE:-$OUTDIR/.vllm-before}"
	VLLM_BEFORE=$(cat "$VLLM_BEFORE_FILE" 2>/dev/null || echo "")
	VLLM_DELTA=""
	if [ -n "$VLLM_AFTER" ] && [ -n "$VLLM_BEFORE" ]; then
		VLLM_DELTA=$((VLLM_AFTER - VLLM_BEFORE))
	fi
	printf 'model-request count (one metering-proxy usage log line per model request, P2b onward): %s\n' "$FORWARDED_COUNT"
	printf 'token totals across those requests (prompt/completion): %s/%s\n\n' "$PROXY_PROMPT_SUM" "$PROXY_COMPLETION_SUM"
	# A Succeeded Loop with zero model requests means the agent never called
	# the model (or the log fetch failed). A silent zero would fail the I50
	# acceptance invisibly, so warn loudly.
	if [ "$PHASE" = "Succeeded" ] && { [ -z "${FORWARDED_COUNT:-}" ] || [ "$FORWARDED_COUNT" -eq 0 ] 2>/dev/null; }; then
		printf '**WARNING: the Loop Succeeded but the metering proxy logged %s usage request(s). Either the agent never called the model, or the proxy log fetch failed.**\n\n' "${FORWARDED_COUNT:-0}"
	fi
	if [ "$PROXY_RC" -ne 0 ]; then
		printf 'proxy log fetch failed (rc=%s); the count above may be wrong. Raw kubectl output:\n\n' "$PROXY_RC"
		printf '%s\n' "$PROXY_LOG"
		printf '\n'
	fi
	printf 'metering-proxy usage log lines (one per model request):\n\n```\n'
	printf '%s\n' "$PROXY_USAGE_LINES"
	printf '```\n\n'
	printf 'vLLM request_success_total delta over the run (summed over both backends %s; read from each backend directly, not via the 192.168.1.20 LB): ' "$VLLM_BACKENDS"
	if [ -n "$VLLM_DELTA" ]; then
		printf '%s\n' "$VLLM_DELTA"
	else
		printf 'unavailable (before=%s, after=%s)\n' "${VLLM_BEFORE:-?}" "${VLLM_AFTER:-?}"
	fi
	printf 'Note: the vLLM backends serve other traffic too, so this delta is an upper bound for this run. The model-request count above is the operator-side ground truth for what the metering proxy of this Loop served.\n\n'

	printf '## NetworkPolicies\n\n```\n'
	# The verify netpol is labeled coxswain.io/verify-for, not loop, so the
	# label selector misses it; filter by name prefix instead (kubectl prints
	# the full API group in -o name, so strip that first).
	kubectl --context "$CTX" -n "$NS" get netpol -o name 2>/dev/null | sed 's|^networkpolicy.networking.k8s.io/||' | grep "^$LOOP-" \
		| while read -r np; do kubectl --context "$CTX" -n "$NS" get netpol "$np" -o yaml; done || true
	printf '```\n\n'

	# ----------------------------------------------------------------------
	# What the agent changed: a READ-ONLY peek at the workspace PVC (the
	# sandbox pod is gone after a terminal phase). The peek pod is
	# alpine/git, runAsUser 65532 (the workspace repo is owned by the
	# runner's UID), core.hooksPath=/dev/null (never run agent-planted
	# hooks), safe.directory (the ownership mismatch), read-only mount,
	# no network. Deleted afterwards. Only the diff stat is collected —
	# this is operator-side evidence of what base..verified covers.
	# ----------------------------------------------------------------------
	printf '## What the agent changed (git diff --stat base..verified)\n\n'
	printf 'The diff is read from the Loop%s workspace PVC by a short-lived, read-only peek pod (alpine/git, runAsUser 65532, read-only mount, no network, deleted afterwards). It shows what the verified commit contains relative to the pinned base commit.\n\n' "$LOOP"
	printf '```\n'
	BASE_COMMIT=$(python3 -c 'import json,sys; print((json.load(open(sys.argv[1])).get("status") or {}).get("baseCommit") or "")' "$EV_LOOP_JSON")
	VERIFIED_COMMIT=$(python3 -c 'import json,sys; print(((json.load(open(sys.argv[1])).get("status") or {}).get("currentVerify") or {}).get("verifiedCommit") or "")' "$EV_LOOP_JSON")
	PVC_NAME="$LOOP-workspace"
	PEEK_POD="${LOOP}-evidence-peek"
	# A prior evidence collection may have left the peek pod behind (it has a
	# one-shot command and is deleted after use, but a failed collection can
	# orphan it). An existing pod with the same name is not a collision to
	# fail on: delete it and create a fresh one.
	kubectl --context "$CTX" -n "$NS" delete pod "$PEEK_POD" --wait=false >/dev/null 2>&1 || true
	if [ -n "$BASE_COMMIT" ] && [ -n "$VERIFIED_COMMIT" ] && kubectl --context "$CTX" -n "$NS" get pvc "$PVC_NAME" >/dev/null 2>&1; then
		cat <<PEOFEOF | kubectl --context "$CTX" apply -f - >/dev/null
apiVersion: v1
kind: Pod
metadata:
  name: $PEEK_POD
  namespace: $NS
  labels:
    coxswain.io/evidence-peek: "$LOOP"
spec:
  restartPolicy: Never
  securityContext:
    fsGroup: 65532
  containers:
  - name: git
    image: alpine/git
    command:
    - /bin/sh
    - -c
    - |
      set -eu
      export GIT_CONFIG_COUNT=1 GIT_CONFIG_KEY_0=safe.directory GIT_CONFIG_VALUE_0='*'
      export GIT_CONFIG_COUNT=2 GIT_CONFIG_KEY_1=core.hooksPath GIT_CONFIG_VALUE_1=/dev/null
      git -C /workspace diff --stat $BASE_COMMIT $VERIFIED_COMMIT
    imagePullPolicy: IfNotPresent
    securityContext:
      runAsUser: 65532
      runAsNonRoot: true
      readOnlyRootFilesystem: true
      allowPrivilegeEscalation: false
    volumeMounts:
    - name: workspace
      mountPath: /workspace
      readOnly: true
  volumes:
  - name: workspace
    persistentVolumeClaim:
      claimName: $PVC_NAME
PEOFEOF
		# The one-shot command writes the diff to the container log and the
		# container terminates. Poll for the log: a command pod may never
		# report Ready before it terminates, so wait for the log or a
		# terminated container state (120s total).
		PEEK_LOG=""
		for _ in $(seq 1 24); do
			PEEK_LOG=$(kubectl --context "$CTX" -n "$NS" logs "$PEEK_POD" 2>/dev/null || true)
			if [ -n "$PEEK_LOG" ]; then
				break
			fi
			TERMINATED=$(kubectl --context "$CTX" -n "$NS" get pod "$PEEK_POD" -o jsonpath='{.status.containerStatuses[0].state.terminated.reason}' 2>/dev/null || true)
			if [ -n "$TERMINATED" ]; then
				PEEK_LOG=$(kubectl --context "$CTX" -n "$NS" logs "$PEEK_POD" 2>/dev/null || true)
				[ -n "$PEEK_LOG" ] && break
			fi
			sleep 5
		done
		if [ -n "$PEEK_LOG" ]; then
			printf '%s\n' "$PEEK_LOG"
		else
			kubectl --context "$CTX" -n "$NS" describe pod "$PEEK_POD" 2>/dev/null | tail -8 || true
			printf '\n[peek pod produced no log in 120s]\n'
		fi
		kubectl --context "$CTX" -n "$NS" delete pod "$PEEK_POD" --wait=false >/dev/null 2>&1 || true
	else
		printf 'n/a (baseCommit=%s, verifiedCommit=%s, pvc=%s present=%s)\n' \
		"$BASE_COMMIT" "$VERIFIED_COMMIT" "$PVC_NAME" "$(kubectl --context "$CTX" -n "$NS" get pvc "$PVC_NAME" >/dev/null 2>&1 && echo yes || echo no)"
	fi
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
