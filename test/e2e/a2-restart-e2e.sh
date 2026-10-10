#!/usr/bin/env bash
# A2 (PLAN-ALPHA.md, D53): restart durability baseline — MEASURE, don't fix.
#
#   make a2-e2e
#
# Runs the gocli sample Loop on kind-coxswain-dev and, in SEPARATE runs, kills
# a component mid-run, then records what happens against an uninterrupted
# control. This is a measurement e2e: it does NOT fix anything. The results
# table (past into the PR) is the input for deciding the fixes.
#
# D53 scope: for alpha, durability = resume from the workspace PVC. The e2e
# turns "the CR status and the workspace PVC survive, so many cases MAY resume"
# into evidence.
#
# Kill cases (each a fresh run with a unique Loop name, so the deliver branch
# coxswain/<loop> is fresh — a reused name collides with a prior deliver push):
#   (a) operator pod during Planning
#   (a) operator pod during Implementing
#   (a) operator pod during Verifying
#   (a) operator pod while a deliver Job is in flight (phase Succeeded +
#       deliver Job running)
#   (b) sandbox pod mid-Implementing
#   (c) verify Job pod mid-run
#
# For each case the script records:
#   - the target component + phase at kill time
#   - the terminal phase + reason (or "wedged" after the CAP)
#   - the iteration
#   - any wedge (no phase advance for CAP seconds with no terminal phase)
# and compares it to the uninterrupted control's terminal outcome.
#
# Norms honoured (AGENTS.md / review docs):
#   - ALL logs are kept (never /dev/null). Operator, sandbox, verify, deliver
#     and proxy pod logs are captured to $OUT/<case>/ for every run.
#   - The operator image DIGEST is recorded at the start (and re-read after
#     each operator kill, since the restarted pod may pull the same digest).
#   - docker image prune runs FIRST (the reviewer asked for it).
#   - One kind job at a time: runs are strictly sequential; the script aborts
#     if it detects a non-control coxswain Loop already running.
#   - A CAP (default 600s / 10 min) bounds each run's post-kill watch; a run
#     that reaches no terminal phase within the CAP is recorded as "wedged".
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
CTX="${CTX:-kind-coxswain-dev}"
NS="${NS:-samples}"
APP="gocli"
TASK="1"
# APP is the sample app (used in the log header and for humans reading the
# results); it is intentionally a single value (gocli task 1 is the canonical
# sample) — kept for clarity in the table.
# CAP: seconds to watch after the kill before calling the run "wedged".
CAP="${A2_CAP:-600}"
# KILL_DELAY: seconds after the target phase is observed before the kill fires
# (let the phase settle; 0 kills as soon as the phase is observed).
KILL_DELAY="${A2_KILL_DELAY:-3}"
# PHASE_POLL: seconds between phase polls while waiting for the target phase.
PHASE_POLL="${A2_PHASE_POLL:-2}"
OUTDIR="${A2_OUT:-$ROOT/.samples/a2-restart-$(date -u +%Y%m%d%H%M%S)}"

# The operator deployment + namespace (the kill target for case (a)).
OP_NS="coxswain-system"
OP_DEPLOY="coxswain-controller-manager"

# The source Loop manifest (a unique name is rendered into a temp file per run).
LOOP_SRC="$ROOT/examples/$APP/tasks/$TASK.loop.yaml"

# Kill-case ids (one run each; the control runs first with no kill).
CASES=(
  "control"
  "op-planning"
  "op-implementing"
  "op-verifying"
  "op-deliver"
  "sandbox-implementing"
  "verify-job"
)

# log <message> | log <tag> <message>: a single-arg call uses the "setup" tag.
log() {
	if [ $# -eq 1 ]; then
		printf '\033[1;34m[a2 setup]\033[0m %s\n' "$1"
	else
		printf '\033[1;34m[a2 %s]\033[0m %s\n' "$1" "$2"
	fi
}
die() { printf '\033[1;31m[a2 FATAL]\033[0m %s\n' "$*" >&2; exit 1; }

command -v kubectl >/dev/null || die "kubectl not on PATH"
command -v docker >/dev/null || die "docker not on PATH (needed for image prune + digest)"
command -v python3 >/dev/null || die "python3 not on PATH"
python3 -c 'import yaml' >/dev/null 2>&1 || die "python3-yaml (PyYAML) not importable"
kubectl --context "$CTX" get nodes >/dev/null 2>&1 \
	|| die "cannot reach cluster context '$CTX' (is the coxswain-dev kind cluster running?)"
[ -f "$LOOP_SRC" ] || die "Loop manifest $LOOP_SRC not found"

mkdir -p "$OUTDIR"
log "out dir: $OUTDIR (all logs kept here, never /dev/null)"

# ---------------------------------------------------------------------------
# Preflight: exactly ONE job at a time. Abort if a non-control coxswain Loop
# is already running (the reviewer: "nothing else is running now" — guard the
# invariant so two people can't run this concurrently and interleave kills).
# ---------------------------------------------------------------------------
PREEXISTING=$(kubectl --context "$CTX" -n "$NS" get loop -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}' 2>/dev/null || true)
if [ -n "$PREEXISTING" ]; then
	# A leftover Loop from a previous run is not fatal if it is terminal
	# (the operator stops it); a non-terminal one means another run is live.
	for L in $PREEXISTING; do
		PH=$(kubectl --context "$CTX" -n "$NS" get loop "$L" -o jsonpath='{.status.phase}' 2>/dev/null || echo "")
		case "$PH" in
		Succeeded|Failed|"") ;;
		*) die "Loop $L is in non-terminal phase $PH (another run is live?); aborting to keep one job at a time";;
		esac
	done
	log "pre-existing terminal Loops present: $(echo "$PREEXISTING" | tr '\n' ' ')(left alone; a2 runs use unique names)"
fi

# ---------------------------------------------------------------------------
# 0. docker image prune FIRST (the reviewer asked for it): drop dangling
#    images so the run does not depend on a stale local image. Kind's docker
#    daemon is the host docker; prune -a is unsafe (it would drop kind's node
#    image and base images), so prune DANGLING only.
# ---------------------------------------------------------------------------
log "docker image prune (dangling only, never -a)..."
docker image prune -f >"$OUTDIR/docker-prune.log" 2>&1 || die "docker image prune failed (see $OUTDIR/docker-prune.log)"

# ---------------------------------------------------------------------------
# 1. Record the operator image DIGEST (the measurement baseline). Re-read
#    after each operator kill: the Deployment should bring back the same
#    digest (a different digest would mean the operator was REDEPLOYED, not
#    just restarted — a different durability story).
# ---------------------------------------------------------------------------
op_pod() { kubectl --context "$CTX" -n "$OP_NS" get pod -l app.kubernetes.io/name=coxswain -o name 2>/dev/null | head -1 | sed 's|pod/||'; }
op_digest() {
	local p
	p=$(op_pod)
	[ -n "$p" ] || { echo "<no-operator-pod>"; return; }
	kubectl --context "$CTX" -n "$OP_NS" get pod "$p" -o jsonpath='{.status.containerStatuses[0].imageID}' 2>/dev/null || echo "<read-failed>"
}
OP_DIGEST_BEFORE=$(op_digest)
log "operator digest before: $OP_DIGEST_BEFORE"
echo "operator-digest-before: $OP_DIGEST_BEFORE" >"$OUTDIR/digest.txt"

# The controller must run with a non-empty --runner-image (else the sandbox
# runs 'sleep infinity' and wedges in Planning — a non-A2 failure).
RUNNER_FLAG=$(kubectl --context "$CTX" -n "$OP_NS" get deploy "$OP_DEPLOY" -o jsonpath='{.spec.template.spec.containers[0].args}' \
	| python3 -c 'import json,sys
try:
    args=json.load(sys.stdin)
except Exception:
    sys.exit(0)
for a in args:
    if a.startswith("--runner-image="):
        print(a)
' 2>/dev/null || true)
[ -n "$RUNNER_FLAG" ] || die "controller has no --runner-image flag; the sandbox would wedge in Planning (not an A2 measurement)."

# ---------------------------------------------------------------------------
# Helpers.
# ---------------------------------------------------------------------------

# phase_of <loop>: current .status.phase (or "" / "Pending").
phase_of() {
	kubectl --context "$CTX" -n "$NS" get loop "$1" -o jsonpath='{.status.phase}' 2>/dev/null || echo ""
}
iteration_of() {
	kubectl --context "$CTX" -n "$NS" get loop "$1" -o jsonpath='{.status.iteration}' 2>/dev/null || echo ""
}
# reason_of <loop>: the Failed reason (or the last condition reason for a
# non-Succeeded terminal phase). Kubectl output is captured into a variable
# first (no pipeline) so `set -o pipefail` cannot make the `|| echo` fire a
# second time when the python path already printed.
reason_of() {
	local loop="$1" raw
	raw=$(kubectl --context "$CTX" -n "$NS" get loop "$loop" -o json 2>/dev/null || true)
	printf '%s' "$raw" | python3 -c '
import json,sys
try:
    d=json.load(sys.stdin)
except Exception:
    print(""); sys.exit(0)
s=(d.get("status") or {})
for c in s.get("conditions", []):
    if c.get("type")=="Failed" and c.get("status")=="True":
        print(c.get("reason","") or c.get("message",""))
        break
else:
    print("")
' 2>/dev/null || echo ""
}

# deliver_job_state <loop>: "running" | "succeeded" | "failed" | "absent".
# Same pipefail guard as reason_of.
deliver_job_state() {
	local loop="$1" raw
	raw=$(kubectl --context "$CTX" -n "$NS" get job "${loop}-deliver" -o json 2>/dev/null || true)
	printf '%s' "$raw" | python3 -c '
import json,sys
try:
    d=json.load(sys.stdin)
except Exception:
    print("absent"); sys.exit(0)
st=d.get("status") or {}
if st.get("succeeded",0):
    print("succeeded")
elif st.get("failed",0):
    print("failed")
else:
    print("running")
' 2>/dev/null || echo "absent"
}

# sandbox_pod <loop>: the sandbox pod name (the operator names it <loop>-sandbox).
sandbox_pod() { echo "$1-sandbox"; }

# verify_job_pod <loop>: the verify Job's running pod (the latest
# <loop>-verify-<n>-<x> pod).
verify_job_pod() {
	local loop="$1"
	kubectl --context "$CTX" -n "$NS" get pods -o name 2>/dev/null | sed 's|^pod/||' \
		| grep -E "^${loop}-verify-[0-9]+" | sort | head -1 || true
}

# apply_loop <name>: render the task Loop manifest with the given name + the
# current runner image, and apply it (deleting a prior same-name Loop first).
apply_loop() {
	local name="$1" render
	render="$(mktemp)"
	sed "s|name: ${APP}-task${TASK}|name: $name|" "$LOOP_SRC" >"$render"
	if kubectl --context "$CTX" -n "$NS" get loop "$name" >/dev/null 2>&1; then
		kubectl --context "$CTX" -n "$NS" delete loop "$name" --wait=true --timeout=120s >/dev/null 2>&1 \
			|| kubectl --context "$CTX" -n "$NS" delete loop "$name" --wait=false >/dev/null 2>&1
		sleep 3
	fi
	kubectl --context "$CTX" -n "$NS" apply -f "$render" >/dev/null
	rm -f "$render"
}

# capture_logs <case> <loop>: keep ALL pod logs (operator, sandbox, verify,
# deliver, proxy) for the run. Never /dev/null.
capture_logs() {
	local case="$1" loop="$2" dir
	dir="$OUTDIR/$case"
	mkdir -p "$dir"
	# Operator (the running operator pod; after an operator kill this is the
	# REPLACED pod — capture its log so we see the fresh operator's view).
	local op
	op=$(op_pod)
	if [ -n "$op" ]; then
		kubectl --context "$CTX" -n "$OP_NS" logs "$op" >"$dir/operator.log" 2>&1 || true
	fi
	# Sandbox (may be gone after a terminal phase; capture if present).
	kubectl --context "$CTX" -n "$NS" logs "pod/${loop}-sandbox" -c agent >"$dir/sandbox.log" 2>&1 || true
	kubectl --context "$CTX" -n "$NS" logs "pod/${loop}-sandbox" -c init-phase >"$dir/sandbox-init.log" 2>&1 || true
	# Model proxy.
	kubectl --context "$CTX" -n "$NS" logs "pod/${loop}-proxy" >"$dir/proxy.log" 2>&1 || true
	# Verify jobs (all of them, per iteration).
	local jobs j
	jobs=$(kubectl --context "$CTX" -n "$NS" get jobs -o name 2>/dev/null | sed 's|^job.batch/||' | grep "^${loop}-verify-" | sort -V || true)
	for j in $jobs; do
		local vp
		vp=$(kubectl --context "$CTX" -n "$NS" get pods -o name 2>/dev/null | sed 's|^pod/||' | grep "^${j}-" | head -1 || true)
		if [ -n "$vp" ]; then
			kubectl --context "$CTX" -n "$NS" logs "pod/$vp" >"$dir/verify-${j}.log" 2>&1 || true
		fi
	done
	# Deliver job.
	local dp
	dp=$(kubectl --context "$CTX" -n "$NS" get pods -o name 2>/dev/null | sed 's|^pod/||' | grep "^${loop}-deliver-" | head -1 || true)
	if [ -n "$dp" ]; then
		kubectl --context "$CTX" -n "$NS" logs "pod/$dp" >"$dir/deliver.log" 2>&1 || true
	fi
	# The final Loop object (status + conditions) — the terminal outcome.
	kubectl --context "$CTX" -n "$NS" get loop "$loop" -o json >"$dir/loop.json" 2>&1 || true
	# The events for the Loop (the operator's phase machine record).
	kubectl --context "$CTX" -n "$NS" get events --field-selector "involvedObject.name=$loop" --sort-by=.lastTimestamp >"$dir/events.json" 2>&1 || true
}

# watch_for_phase <loop> <phase> <timeout_s>: poll until the phase is seen or
# the timeout. Returns 0 if the phase was observed, 1 on timeout.
watch_for_phase() {
	local loop="$1" target="$2" timeout="$3"
	local deadline=$(( $(date +%s) + timeout ))
	while [ "$(date +%s)" -lt "$deadline" ]; do
		local ph
		ph=$(phase_of "$loop")
		# A terminal phase before the target = the run ended early (recorded
		# by the caller via the outcome function).
		case "$ph" in
		Succeeded|Failed) return 2;;
		esac
		if [ "$ph" = "$target" ]; then
			return 0
		fi
		sleep "$PHASE_POLL"
	done
	return 1
}

# watch_outcome <loop> <timeout_s>: poll until a terminal phase or the
# timeout. Writes the outcome to global vars: OUT_PHASE, OUT_ITER, OUT_REASON,
# OUT_WEDGED (yes/no), OUT_DELIVER (deliver Job state).
watch_outcome() {
	local loop="$1" timeout="$2"
	OUT_PHASE="" OUT_ITER="" OUT_REASON="" OUT_WEDGED="no" OUT_DELIVER=""
	local deadline=$(( $(date +%s) + timeout ))
	local last_ph=""
	local last_change
	last_change=$(date +%s)
	while [ "$(date +%s)" -lt "$deadline" ]; do
		local ph
		ph=$(phase_of "$loop")
		if [ "$ph" != "$last_ph" ]; then
			last_ph="$ph"
			last_change=$(date +%s)
		fi
		case "$ph" in
		Succeeded|Failed)
			OUT_PHASE="$ph"
			OUT_ITER="$(iteration_of "$loop")"
			OUT_REASON="$(reason_of "$loop")"
			OUT_DELIVER="$(deliver_job_state "$loop")"
			return 0
			;;
		esac
		sleep "$PHASE_POLL"
	done
	# No terminal phase within the timeout: a wedge candidate. Distinguish a
	# TRUE wedge (no phase change for the whole window) from a slow-but-moving
	# run (phase still changing near the deadline).
	local cur
	cur=$(phase_of "$loop")
	OUT_PHASE="${cur:-Pending}"
	OUT_ITER="$(iteration_of "$loop")"
	OUT_REASON="$(reason_of "$loop")"
	OUT_DELIVER="$(deliver_job_state "$loop")"
	# Wedged: the phase has not changed for CAP/2 seconds (a stuck run), or it
	# is in a phase where progress is expected but the run is idle.
	if [ $(( $(date +%s) - last_change )) -ge $(( CAP / 2 )) ]; then
		OUT_WEDGED="yes"
	fi
	return 3
}

# kill_operator <case> <loop>: delete the operator pod (the Deployment
# replaces it). Records the digest after the replacement.
kill_operator() {
	local case="$1"
	local loop="$2"
	local dir="$OUTDIR/$case"
	local before after
	before=$(op_pod)
	after_digest_pre=$(op_digest)
	kubectl --context "$CTX" -n "$OP_NS" delete pod "$before" --wait=false >/dev/null 2>&1 || true
	log "$case" "killed operator pod $before (phase $(phase_of "$loop"))"
	# Wait for the replacement pod to come up.
	local deadline=$(( $(date +%s) + 120 ))
	while [ "$(date +%s)" -lt "$deadline" ]; do
		after=$(op_pod)
		if [ -n "$after" ] && [ "$after" != "$before" ]; then
			kubectl --context "$CTX" -n "$OP_NS" wait --for=condition=ready "pod/$after" --timeout=120s >/dev/null 2>&1 || true
			break
		fi
		sleep 2
	done
	after_digest=$(op_digest)
	{
		echo "operator-kill: case=$case"
		echo "killed-pod: $before"
		echo "replaced-pod: $after"
		echo "digest-before: $after_digest_pre"
		echo "digest-after: $after_digest"
	} >"$dir/kill.txt" 2>&1
	if [ "$after_digest_pre" != "$after_digest" ]; then
		log "$case" "WARNING: operator digest changed ($after_digest_pre -> $after_digest) — the operator was REDEPLOYED, not just restarted"
	fi
}

# kill_sandbox <case> <loop>: delete the sandbox pod (the operator recreates
# it; the workspace PVC persists).
kill_sandbox() {
	local case="$1"
	local loop="$2"
	local dir="$OUTDIR/$case"
	local sp
	sp="$(sandbox_pod "$loop")"
	kubectl --context "$CTX" -n "$NS" delete pod "$sp" --wait=false >/dev/null 2>&1 || true
	log "$case" "killed sandbox pod $sp (phase $(phase_of "$loop"))"
	echo "sandbox-kill: pod=$sp" >"$dir/kill.txt" 2>&1
}

# kill_verify_job_pod <case> <loop>: delete the verify Job's running pod (the
# Job controller may recreate the pod or leave the Job failed).
kill_verify_job_pod() {
	local case="$1"
	local loop="$2"
	local dir="$OUTDIR/$case"
	local vp
	vp=$(verify_job_pod "$loop")
	if [ -z "$vp" ]; then
		log "$case" "WARNING: no verify Job pod found to kill"
		echo "verify-kill: pod=<none-found>" >"$dir/kill.txt" 2>&1
		return
	fi
	kubectl --context "$CTX" -n "$NS" delete pod "$vp" --wait=false >/dev/null 2>&1 || true
	log "$case" "killed verify Job pod $vp (phase $(phase_of "$loop"))"
	echo "verify-kill: pod=$vp" >"$dir/kill.txt" 2>&1
}

# run_case <case> <loop-name> <target-phase|deliver> <kill-fn>:
#   apply the Loop, wait for the target phase (or Succeeded + deliver running
#   for the deliver case), fire the kill, then watch the outcome with the CAP.
run_case() {
	local case="$1"
	local name="$2"
	local target="$3"
	local killfn="$4"
	local dir
	dir="$OUTDIR/$case"
	mkdir -p "$dir"
	log "$case" "applying Loop $name (target: $target)"
	apply_loop "$name"

	local waited=0
	# Wait for the target phase (or the deliver Job running for the deliver
	# case) with a generous pre-kill window (the run must REACH the target).
	# A2 uses a 600s pre-kill wait (the phases are slow with a real vLLM).
	local deadline=$(( $(date +%s) + 600 ))
	local reached=no
	while [ "$(date +%s)" -lt "$deadline" ]; do
		local ph
		ph=$(phase_of "$name")
		case "$target" in
		deliver)
			# The deliver case: the Loop is Succeeded AND the deliver Job is
			# running (in flight). Succeeded + no deliver Job yet = wait for
			# the operator to create it.
			if [ "$ph" = "Succeeded" ]; then
				local dj
				dj=$(deliver_job_state "$name")
				if [ "$dj" = "running" ]; then
					reached=yes
					break
				fi
			fi
			;;
		*)
			if [ "$ph" = "$target" ]; then
				reached=yes
				break
			fi
			# A terminal phase before the target: the run ended early (the kill
			# window was missed). Record and stop waiting.
			case "$ph" in
			Succeeded|Failed)
				reached="early"
				break
				;;
			esac
			;;
		esac
		sleep "$PHASE_POLL"
		waited=$((waited + PHASE_POLL))
	done

	# Pre-kill settle (KILL_DELAY), then fire the kill. The deliver case skips
	# the settle: the deliver Job runs only ~10s, so a 3s settle risks missing
	# the window (the Job completes before the kill). Kill it immediately.
	if [ "$reached" = "yes" ]; then
		if [ "$target" = "deliver" ]; then
			log "$case" "deliver Job in flight; killing immediately (no settle)"
			"$killfn" "$case" "$name"
		else
			log "$case" "reached $target; settling ${KILL_DELAY}s before the kill"
			sleep "$KILL_DELAY"
			"$killfn" "$case" "$name"
		fi
	elif [ "$reached" = "early" ]; then
		log "$case" "run reached a terminal phase before $target (missed the kill window); no kill fired"
		echo "kill: not-fired (terminal before target)" >"$dir/kill.txt" 2>&1
	fi

	# Watch the outcome with the CAP.
	log "$case" "watching outcome (CAP ${CAP}s)..."
	local rc=0
	watch_outcome "$name" "$CAP" || rc=$?
	if [ $rc -eq 3 ]; then
		log "$case" "no terminal phase within ${CAP}s (wedged=$( [ "$OUT_WEDGED" = yes ] && echo yes || echo no ))"
	fi

	# Capture ALL logs (never /dev/null).
	capture_logs "$case" "$name"
	# Record the outcome.
	{
		echo "case: $case"
		echo "loop: $name"
		echo "target: $target"
		echo "reached-target: $reached"
		echo "terminal-phase: ${OUT_PHASE}"
		echo "iteration: ${OUT_ITER}"
		echo "reason: ${OUT_REASON}"
		echo "wedged: ${OUT_WEDGED}"
		echo "deliver-job: ${OUT_DELIVER}"
		echo "operator-digest-after: $(op_digest)"
	} >"$dir/outcome.txt" 2>&1
	log "$case" "outcome: phase=${OUT_PHASE} iter=${OUT_ITER} reason=${OUT_REASON} wedged=${OUT_WEDGED} deliver=${OUT_DELIVER}"

	# Clean up this run's Loop + Jobs (unique name, so no collision next run).
	kubectl --context "$CTX" -n "$NS" delete loop "$name" --wait=true --timeout=120s >/dev/null 2>&1 || true
	kubectl --context "$CTX" -n "$NS" delete jobs --all --wait=true --timeout=60s >/dev/null 2>&1 || true
	sleep 5
}

# ---------------------------------------------------------------------------
# Run the control first, then each kill case (strictly sequential: one kind
# job at a time).
# ---------------------------------------------------------------------------
log "control" "=== A2 restart-durability e2e (measure only) ==="
log "control" "operator digest: $OP_DIGEST_BEFORE (runner flag: $RUNNER_FLAG)"
log "control" "CAP=${CAP}s KILL_DELAY=${KILL_DELAY}s PHASE_POLL=${PHASE_POLL}s"

for case in "${CASES[@]}"; do
	case "$case" in
	control)
		# The control: apply the Loop, no kill, watch to a terminal phase.
		name="a2-control-$(date +%s)"
		log "control" "applying control Loop $name"
		apply_loop "$name"
		rc=0
		watch_outcome "$name" "$CAP" || rc=$?
		[ $rc -eq 3 ] && log "control" "control run did not reach a terminal phase within CAP (wedged=$( [ "$OUT_WEDGED" = yes ] && echo yes || echo no ))"
		capture_logs "control" "$name"
		{
			echo "case: control"
			echo "loop: $name"
			echo "target: none (uninterrupted)"
			echo "terminal-phase: ${OUT_PHASE}"
			echo "iteration: ${OUT_ITER}"
			echo "reason: ${OUT_REASON}"
			echo "wedged: ${OUT_WEDGED}"
			echo "deliver-job: ${OUT_DELIVER}"
		} >"$OUTDIR/control/outcome.txt" 2>&1
		log "control" "outcome: phase=${OUT_PHASE} iter=${OUT_ITER} reason=${OUT_REASON} wedged=${OUT_WEDGED} deliver=${OUT_DELIVER}"
		kubectl --context "$CTX" -n "$NS" delete loop "$name" --wait=true --timeout=120s >/dev/null 2>&1 || true
		kubectl --context "$CTX" -n "$NS" delete jobs --all --wait=true --timeout=60s >/dev/null 2>&1 || true
		sleep 5
		;;
	op-planning)
		run_case "$case" "a2-opplan-$(date +%s)" "Planning" kill_operator
		;;
	op-implementing)
		run_case "$case" "a2-opimpl-$(date +%s)" "Implementing" kill_operator
		;;
	op-verifying)
		run_case "$case" "a2-opver-$(date +%s)" "Verifying" kill_operator
		;;
	op-deliver)
		run_case "$case" "a2-opdel-$(date +%s)" "deliver" kill_operator
		;;
	sandbox-implementing)
		run_case "$case" "a2-sandbox-impl-$(date +%s)" "Implementing" kill_sandbox
		;;
	verify-job)
		run_case "$case" "a2-verify-$(date +%s)" "Verifying" kill_verify_job_pod
		;;
	*)
		die "unknown case $case"
		;;
	esac
done

# ---------------------------------------------------------------------------
# Build the results table (markdown, for the PR).
# ---------------------------------------------------------------------------
log "table" "building the results table..."
{
	echo "# A2 restart durability — results"
	echo
	echo "- operator digest (before): \`$OP_DIGEST_BEFORE\`"
	echo "- runner flag: \`$RUNNER_FLAG\`"
	echo "- CAP: ${CAP}s, KILL_DELAY: ${KILL_DELAY}s, PHASE_POLL: ${PHASE_POLL}s"
	echo "- generated: $(date -u +%Y-%m-%dT%H:%M:%SZ)"
	echo
	echo "The control (uninterrupted) outcome is the baseline each kill case is compared against."
	echo
	echo "| case | target | terminal phase | iter | reason | wedged? | deliver job | log dir |"
	echo "|------|--------|----------------|------|--------|---------|-------------|---------|"
	for case in "${CASES[@]}"; do
		dir="$OUTDIR/$case"
		[ -f "$dir/outcome.txt" ] || { echo "| $case | ? | ? | ? | ? | ? | ? | (no outcome) |"; continue; }
		target=$(sed -n 's/^target: //p' "$dir/outcome.txt" | head -1)
		phase=$(sed -n 's/^terminal-phase: //p' "$dir/outcome.txt" | head -1)
		iter=$(sed -n 's/^iteration: //p' "$dir/outcome.txt" | head -1)
		reason=$(sed -n 's/^reason: //p' "$dir/outcome.txt" | head -1)
		wedged=$(sed -n 's/^wedged: //p' "$dir/outcome.txt" | head -1)
		deliver=$(sed -n 's/^deliver-job: //p' "$dir/outcome.txt" | head -1)
		reached=$(sed -n 's/^reached-target: //p' "$dir/outcome.txt" | head -1)
		[ "$case" = "control" ] && target="none"
		echo "| $case | ${target} | ${phase} | ${iter} | ${reason} | ${wedged} | ${deliver} | [logs]($case/) |"
	done
	echo
	echo "## Per-case detail"
	for case in "${CASES[@]}"; do
		dir="$OUTDIR/$case"
		echo
		echo "### $case"
		if [ -f "$dir/outcome.txt" ]; then
			sed 's/^/- /' "$dir/outcome.txt"
		else
			echo "- (no outcome captured)"
		fi
		if [ -f "$dir/kill.txt" ]; then
			echo
			echo "  kill record:"
			sed 's/^/  - /' "$dir/kill.txt"
		fi
		echo
		echo "  logs kept in \`.samples/a2-restart-*/$case/\` (operator.log, sandbox.log, proxy.log, verify-*.log, deliver.log, loop.json, events.json)."
	done
} >"$OUTDIR/RESULTS.md"

log "table" "RESULTS written to $OUTDIR/RESULTS.md"
log "table" "all logs kept under $OUTDIR/ (never /dev/null)"
echo ""
echo "================ A2 RESULTS TABLE ================"
cat "$OUTDIR/RESULTS.md"
echo "=================================================="
log "table" "done. Paste the table above into the PR. No fixes in this PR."
