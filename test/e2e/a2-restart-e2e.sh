#!/usr/bin/env bash
# A2 (PLAN-ALPHA.md, D53): restart durability baseline — MEASURE, don't fix.
#
#   make a2-e2e
#
# Runs the gocli sample Loop on kind-coxswain-dev and, in SEPARATE runs, kills
# a component mid-run, then records what happens against an uninterrupted
# control. This is a measurement e2e: it does NOT fix anything. The results
# table (pasted into the PR) is the input for deciding the fixes.
#
# D53 scope: for alpha, durability = resume from the workspace PVC. The e2e
# turns "the CR status and the workspace PVC survive, so many cases MAY resume"
# into evidence. The evidence is captured LIVE (not after the fact), so the
# table can tell "the kill hit mid-work and coxswain recovered" apart from
# "the kill landed when nothing was in flight".
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
# For each case the script records, LIVE:
#   - events.jsonl: kubectl get events --watch-only -o json for the WHOLE case
#     (started before apply, field-selected to the Loop), so the kill-time
#     events are captured the moment they fire (not a post-run table).
#   - kill.txt: the UTC timestamp, the Loop phase + iteration + baseCommit read
#     AT THE MOMENT OF THE KILL, the killed pod's UID, the replacement pod's
#     UID and when it went Ready, and the workspace PVC UID.
#   - proof of real work in flight:
#       * sandbox kill: the agent container's running state + the workspace
#         HEAD (git in the PVC via a read-only peek pod) BEFORE and AFTER the
#         kill, plus a live agent-container log stream during Implementing
#         (the operator recycles the sandbox pod at phase boundaries, so a
#         post-run `kubectl logs` is NotFound — the live stream is the only
#         record of the runner's work).
#       * verify-Job kill: status.verify.infraAttempts + the verify Job/pod
#         UID before and after (the I65 path: a new Job or pod appears).
#   - the terminal phase + reason (or "wedged" after the CAP) and the
#     iteration, compared to the uninterrupted control's terminal outcome.
#   - Delivered: the script WAITS for the Delivered condition to be True or
#     False (not InProgress) before recording the outcome, and reports
#     Delivered + the PR number per case.
#   - workspace continuity (D53): the PVC UID and status.baseCommit are
#     unchanged across the kill, and the final verified commit builds on the
#     pre-kill HEAD (git merge-base --is-ancestor) where there was one.
#
# Norms honoured (AGENTS.md / review docs):
#   - ALL logs are kept (never /dev/null). Operator, sandbox (live stream),
#     verify, deliver and proxy pod logs + events.jsonl are captured to
#     $OUT/<case>/ for every run.
#   - The operator image DIGEST is recorded at the start (and re-read after
#     each operator kill, since the restarted pod may pull the same digest).
#   - docker image prune runs FIRST (the reviewer asked for it).
#   - One kind job at a time: runs are strictly sequential; the script aborts
#     if it detects a non-control coxswain Loop already running.
#   - A CAP (default 600s / 10 min) bounds each run's post-kill phase watch;
#     a separate DELIVERED_CAP (default 120s) bounds the Delivered wait.
#     A run that reaches no terminal phase within the CAP is "wedged".
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
# DELIVERED_CAP: seconds to wait for the Delivered condition to resolve after
# the phase reaches Succeeded (one reconcile pass after the ~10s push).
DELIVERED_CAP="${A2_DELIVERED_CAP:-120}"
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
		Succeeded | Failed | "") ;;
		*) die "Loop $L is in non-terminal phase $PH (another run is live?); aborting to keep one job at a time" ;;
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
utc() { date -u +%Y-%m-%dT%H:%M:%SZ; }
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
# runs 'sleep infinity' and wedges in Planning — a non-A2 failure). The peek
# pod uses this same image (it has git + a shell, and is the faithful reader
# of the repo the runner wrote).
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
RUNNER_IMAGE="${RUNNER_FLAG#--runner-image=}"

# ---------------------------------------------------------------------------
# Helpers.
# ---------------------------------------------------------------------------

# loop_meta <loop>: "phase iteration baseCommit" read atomically at a moment.
loop_meta() {
	kubectl --context "$CTX" -n "$NS" get loop "$1" -o jsonpath='{.status.phase}{" "}{.status.iteration}{" "}{.status.baseCommit}' 2>/dev/null || echo ""
}
# phase_of <loop>: current .status.phase (or "" / "Pending").
phase_of() {
	kubectl --context "$CTX" -n "$NS" get loop "$1" -o jsonpath='{.status.phase}' 2>/dev/null || echo ""
}
iteration_of() {
	kubectl --context "$CTX" -n "$NS" get loop "$1" -o jsonpath='{.status.iteration}' 2>/dev/null || echo ""
}
# baseCommit_of <loop>: status.baseCommit (the D53 continuity anchor; immutable
# once set).
baseCommit_of() {
	kubectl --context "$CTX" -n "$NS" get loop "$1" -o jsonpath='{.status.baseCommit}' 2>/dev/null || echo ""
}
# pvc_uid <loop>: the workspace PVC's UID (D53 continuity anchor).
pvc_uid() {
	kubectl --context "$CTX" -n "$NS" get pvc "$1-workspace" -o jsonpath='{.metadata.uid}' 2>/dev/null || echo "<none>"
}
# pod_uid <ns> <pod>: the pod's UID (empty/gone if already deleted).
pod_uid() {
	kubectl --context "$CTX" -n "$1" get pod "$2" -o jsonpath='{.metadata.uid}' 2>/dev/null || echo ""
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

# delivered_state <loop>: "STATUS|REASON" for the Delivered condition, or
# "absent|" if the condition is not present (mode None / not a PR Loop).
delivered_state() {
	local loop="$1" raw
	raw=$(kubectl --context "$CTX" -n "$NS" get loop "$loop" -o json 2>/dev/null || true)
	printf '%s' "$raw" | python3 -c '
import json,sys
try:
    d=json.load(sys.stdin)
except Exception:
    print("absent|"); sys.exit(0)
s=(d.get("status") or {})
for c in s.get("conditions", []):
    if c.get("type")=="Delivered":
        print(c.get("status","")+"|"+(c.get("reason","") or c.get("message","")))
        break
else:
    print("absent|")
' 2>/dev/null || echo "absent|"
}
# pr_number_of <loop>: status.delivery.prNumber (empty if not delivered).
pr_number_of() { kubectl --context "$CTX" -n "$NS" get loop "$1" -o jsonpath='{.status.delivery.prNumber}' 2>/dev/null; }
# pr_url_of <loop>: status.delivery.prURL.
pr_url_of() { kubectl --context "$CTX" -n "$NS" get loop "$1" -o jsonpath='{.status.delivery.prURL}' 2>/dev/null; }
# delivery_commit_of <loop>: status.delivery.commit (the pushed commit).
delivery_commit_of() { kubectl --context "$CTX" -n "$NS" get loop "$1" -o jsonpath='{.status.delivery.commit}' 2>/dev/null; }
# current_verify_commit <loop>: status.currentVerify.verifiedCommit.
current_verify_commit() { kubectl --context "$CTX" -n "$NS" get loop "$1" -o jsonpath='{.status.currentVerify.verifiedCommit}' 2>/dev/null; }

# verify_meta <loop>: "infraAttempts infraJobUID verifiedCommit" (the I65
# record for the verify-Job kill case).
verify_meta() {
	local loop="$1" raw
	raw=$(kubectl --context "$CTX" -n "$NS" get loop "$loop" -o json 2>/dev/null || true)
	printf '%s' "$raw" | python3 -c '
import json,sys
try:
    d=json.load(sys.stdin)
except Exception:
    print("0 <none> "); sys.exit(0)
v=(d.get("status") or {}).get("verify") or {}
print(str(v.get("infraAttempts",0))+" "+(v.get("infraJobUID","") or "<none>")+" "+(v.get("verifiedCommit","") or ""))
' 2>/dev/null || echo "0 <none> "
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

# verify_job_uid <loop>: the verify Job's UID (the I65 new-Job detector).
verify_job_uid() {
	local loop="$1"
	# The sample runs a single iteration (iteration 0), so the Job is
	# <loop>-verify-1. Find it by name prefix if the iteration differs.
	local j
	j=$(kubectl --context "$CTX" -n "$NS" get jobs -o name 2>/dev/null | sed 's|^job.batch/||' | grep -E "^${loop}-verify-[0-9]+" | sort -V | head -1 || true)
	[ -n "$j" ] || { echo "<none>"; return; }
	kubectl --context "$CTX" -n "$NS" get job "$j" -o jsonpath='{.metadata.uid}' 2>/dev/null || echo "<gone>"
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

# ---------------------------------------------------------------------------
# LIVE evidence plumbing.
# ---------------------------------------------------------------------------

# start_events_watch <loop> <outpath>: start `kubectl get events --watch-only
# -o json` filtered to the Loop, in the background, writing one JSON object
# per line (jsonl) to $2. -o jsonl does NOT exist in kubectl; -o json with
# -w is one object per line. Field-select server-side to the Loop so prior-
# case and non-Loop events are excluded. A hard `timeout` backstop bounds the
# stream (CAP + DELIVERED_CAP + 180s slack).
EV_WATCH_PID=""
start_events_watch() {
	local loop="$1" out="$2"
	local budget=$(( CAP + DELIVERED_CAP + 180 ))
	( timeout "$budget" kubectl --context "$CTX" -n "$NS" get events \
		--watch-only -o json \
		--field-selector "involvedObject.name=$loop" >"$out" 2>>"$OUTDIR/.events-warn.log" ) &
	EV_WATCH_PID=$!
}
# stop_events_watch <outpath>: kill the watch and post-process the output into
# true JSONL. `kubectl get events --watch-only -o json` emits a pretty-printed
# JSON ARRAY (one big array of event objects), not line-delimited JSON — so
# the raw watch file is not usable jsonl. jq is available and handles the
# array: `jq -c '.[]'` emits one compact JSON object per line. If the file was
# truncated mid-write (the watch was killed while the array was open), jq
# fails; in that case salvage the complete top-level event objects with a
# python fallback and note the truncation.
stop_events_watch() {
	local out="$1"
	if [ -n "$EV_WATCH_PID" ]; then
		kill "$EV_WATCH_PID" 2>/dev/null || true
		wait "$EV_WATCH_PID" 2>/dev/null || true
		EV_WATCH_PID=""
	fi
	local raw tmp
	raw="${out}.raw"
	tmp="${out}.tmp"
	mv -f "$out" "$raw" 2>/dev/null || cp "$out" "$raw"
	if jq -c '.[]' "$raw" >"$tmp" 2>/dev/null && [ -s "$tmp" ]; then
		mv -f "$tmp" "$out"
		local n
		n=$(wc -l <"$out")
		log "events" "post-processed events to jsonl: $n events ($raw kept)"
	else
		# jq failed (truncated/incomplete array): salvage the complete event
		# objects (each begins with `{` at the array's top level) and write them
		# as jsonl. A best-effort salvage, marked as such.
		python3 - "$raw" "$out" <<'PY'
import sys, re
raw, out = sys.argv[1], sys.argv[2]
txt = open(raw, errors="replace").read()
# A top-level event object in the array is delimited by the array's commas;
# each object starts at `{"apiVersion"...` and ends at the matching `}`. Use a
# brace-depth scan to extract complete top-level objects.
objs = []
d = 0
start = None
for i, ch in enumerate(txt):
    if ch == "{":
        if d == 0:
            start = i
        d += 1
    elif ch == "}":
        if d > 0:
            d -= 1
            if d == 0 and start is not None:
                objs.append(txt[start:i+1])
                start = None
with open(out, "w") as f:
    for o in objs:
        f.write(o + "\n")
print(f"# events.jsonl salvage: {len(objs)} complete event objects (raw was {len(txt)} bytes; truncated={txt.rstrip().endswith('}') is False})", file=sys.stderr)
PY
		log "events" "salvaged events to jsonl (raw watch was truncated; $raw kept)"
	fi
	# Validate the final jsonl (every non-empty line parses as JSON).
	local final_out="$out"
	python3 - "$final_out" <<'PY' 2>/dev/null || true
import sys, json
n = bad = 0
for l in open(sys.argv[1], errors="replace"):
    l = l.strip()
    if not l:
        continue
    try:
        json.loads(l)
        n += 1
    except Exception:
        bad += 1
print(f"# events.jsonl: {n} valid jsonl lines, {bad} invalid", file=sys.stderr)
PY
	log "events" "validated events.jsonl ($(wc -l <"$out" 2>/dev/null || echo 0) lines)"
}

# A marker-file sentinel for the sandbox log worker (set by start, cleared by
# stop).
SANDBOX_LOG_PID=""

# start_sandbox_log_stream <loop> <outpath>: `kubectl logs -f` on the sandbox
# agent container, in the background, for the whole case. The operator
# RECYCLES the sandbox pod at phase boundaries (it deletes it and creates a
# fresh one), so a post-run `kubectl logs` on the Implementing pod is
# NotFound; the live stream is the only record of the runner's work (the
# runner start line, the model call, the commit). The worker subshell follows
# the pod by NAME across recreations: when `kubectl logs -f` hits EOF (the
# pod was deleted), it restarts on the recreated pod with the same name.
# A marker file tells the worker when to stop (created here, removed by
# stop_sandbox_log_stream).
start_sandbox_log_stream() {
	local loop="$1"
	local out="$2"
	local marker
	marker="$OUTDIR/.sandbox-log-$loop"
	: >"$marker"
	(
		while [ -f "$marker" ]; do
			pod=$(sandbox_pod "$loop")
			if kubectl --context "$CTX" -n "$NS" get pod "$pod" >/dev/null 2>&1; then
				kubectl --context "$CTX" -n "$NS" logs -f "pod/$pod" -c agent >>"$out" 2>>"$OUTDIR/.sandbox-warn.log" || sleep 2
			else
				sleep 2
			fi
		done
	) &
	SANDBOX_LOG_PID=$!
}
stop_sandbox_log_stream() {
	local loop="$1"
	local marker
	marker="$OUTDIR/.sandbox-log-$loop"
	rm -f "$marker"
	# The worker subshell exits on its next loop iteration (<= 2s) when the
	# marker file is gone. Wait for it (and its kubectl logs child) to finish
	# so the last flush lands before we read the file.
	if [ -n "${SANDBOX_LOG_PID:-}" ]; then
		wait "$SANDBOX_LOG_PID" 2>/dev/null || true
		SANDBOX_LOG_PID=""
	fi
	sleep 2
}

# peek_workspace <loop> <outpath> <pre-kill-HEAD-or-empty>: create a READ-ONLY
# pod that mounts the workspace PVC and runs git in /workspace, capturing the
# HEAD + log + (if a pre-kill HEAD is given) the "builds-on" check. The PVC is
# RWO but one-writer/many-reader (the deliver Job already mounts it RO
# concurrently with the sandbox writer), so a RO peek needs no free window —
# it can run any time. It self-cleans (activeDeadlineSeconds, restartPolicy
# Never) and is deleted after.
peek_workspace() {
	local loop="$1" out="$2" prehead="$3"
	local name="peek-$loop"
	local yml
	yml="$(mktemp)"
	# Escape the pre-kill HEAD for the shell (it's a hex SHA, safe).
	cat >"$yml" <<Y
apiVersion: v1
kind: Pod
metadata:
  name: $name
  namespace: $NS
spec:
  activeDeadlineSeconds: 120
  restartPolicy: Never
  containers:
  - name: peek
    image: $RUNNER_IMAGE
    command: ["/bin/sh", "-c"]
    args:
    - |
      set -e
      echo "peek-utc: $(date -u +%Y-%m-%dT%H:%M:%SZ)"
      echo "peek-pvc: $(pvc_uid "$loop")"
      # The repo was written by the sandbox pod (a different UID); git
      # refuses to operate on a repo it doesn't own (dubious ownership).
      # The runner image runs as UID 65532 with HOME=/ (not writable), so
      # git config --global cannot write a safe.directory exception to
      # the home .gitconfig. Use git -c safe.directory=/workspace per
      # command instead (no config file needed).
      git -C /workspace -c safe.directory=/workspace rev-parse HEAD || echo "<no-head>"
      git -C /workspace -c safe.directory=/workspace log -5 --format='%H %s' || true
      git -C /workspace -c safe.directory=/workspace status --short --branch || true
      if [ -n "$prehead" ]; then
        if git -C /workspace -c safe.directory=/workspace merge-base --is-ancestor "$prehead" HEAD; then
          echo "BUILD-ON-pre-kill-HEAD($prehead): yes"
        else
          echo "BUILD-ON-pre-kill-HEAD($prehead): no"
        fi
      fi
    volumeMounts:
    - name: ws
      mountPath: /workspace
      readOnly: true
  volumes:
  - name: ws
    persistentVolumeClaim:
      claimName: $loop-workspace
      readOnly: true
Y
	kubectl --context "$CTX" -n "$NS" apply -f "$yml" >/dev/null 2>&1 || true
	# Wait for the peek pod to finish (exit 0) or fail. The `complete`/`failed`
	# conditions are flaky with restartPolicy: Never + activeDeadlineSeconds
	# (they can lag the container's actual exit), so poll the pod's phase
	# directly: a Succeeded pod is done; a Running pod is still working (up to
	# the activeDeadlineSeconds, bounded here by a 90s wait).
	local pk deadline2
	pk=$(kubectl --context "$CTX" -n "$NS" get pod "$name" -o jsonpath='{.status.phase}' 2>/dev/null || echo "")
	deadline2=$(( $(date +%s) + 90 ))
	while [ "$pk" != "Succeeded" ] && [ "$pk" != "Failed" ]; do
		[ "$(date +%s)" -ge "$deadline2" ] && break
		sleep 2
		pk=$(kubectl --context "$CTX" -n "$NS" get pod "$name" -o jsonpath='{.status.phase}' 2>/dev/null || echo "")
	done
	kubectl --context "$CTX" -n "$NS" logs "pod/$name" >"$out" 2>&1 || true
	kubectl --context "$CTX" -n "$NS" delete pod "$name" --wait=false >/dev/null 2>&1 || true
	rm -f "$yml"
	# Fall back to a placeholder if the peek produced nothing (e.g. PVC not
	# bound, RO blocked) so the evidence file always exists.
	if [ ! -s "$out" ]; then
		echo "peek-utc: $(utc)"
		echo "peek: <no output — PVC not bound or RO mount blocked; see pod events>"
		kubectl --context "$CTX" -n "$NS" get events --field-selector "involvedObject.name=$name" --sort-by=.lastTimestamp >>"$out" 2>&1 || true
		{
			echo "peek-utc: $(utc)"
			echo "peek: <no output — PVC not bound or RO mount blocked; see pod events>"
			kubectl --context "$CTX" -n "$NS" get events --field-selector "involvedObject.name=$name" --sort-by=.lastTimestamp 2>&1 || true
		} >"$out"
	fi
}

# ws_head <peek-outpath>: extract the single HEAD SHA from a peek output file
# (the line after "peek-pvc:" that is a 40-hex SHA, i.e. `git rev-parse HEAD`).
ws_head() {
	local out="$1"
	grep -E '^[0-9a-f]{40}$' "$out" 2>/dev/null | head -1 || echo ""
}

# capture_logs <case> <loop>: keep ALL pod logs (operator, verify, deliver,
# proxy) + the live streams (events.jsonl, sandbox log) + the peek outputs.
# Never /dev/null. The sandbox LOG is the live stream (started in run_case),
# not a post-run read.
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
	# The final Loop object (status + conditions) — captured AFTER the
	# Delivered wait so status.delivery is populated (the post-run loop.json
	# is the authoritative terminal record).
	kubectl --context "$CTX" -n "$NS" get loop "$loop" -o json >"$dir/loop.json" 2>&1 || true
}

# wait_for_delivered <loop> <timeout_s>: wait until the Delivered condition
# is True or False (non-InProgress), the phase goes Failed, or the timeout.
# Sets DELIVERED_STATE (a "STATUS|REASON" string).
DELIVERED_STATE=""
wait_for_delivered() {
	local loop="$1" timeout="$2"
	local dl=$(( $(date +%s) + timeout )) st ph
	DELIVERED_STATE=""
	while [ "$(date +%s)" -lt "$dl" ]; do
		ph=$(phase_of "$loop")
		st=$(delivered_state "$loop")
		case "$ph" in
		Failed)
			DELIVERED_STATE="failed-phase|${st}"
			return 0
			;;
		esac
		case "$st" in
		"True|"*)
			DELIVERED_STATE="$st"
			return 0
			;;
		"False|InProgress" | "False|")
			# Still working (the operator re-reads the push each reconcile).
			sleep "$PHASE_POLL"
			continue
			;;
		"False|"*)
			DELIVERED_STATE="$st"
			return 0
			;;
		"absent|"*)
			# Not a PR Loop (mode None): not a delivery failure; record as n/a
			# once the phase is terminal.
			if [ "$ph" = "Succeeded" ] || [ "$ph" = "Failed" ]; then
				DELIVERED_STATE="absent(no-pr-mode)|"
				return 0
			fi
			sleep "$PHASE_POLL"
			continue
			;;
		esac
		sleep "$PHASE_POLL"
	done
	DELIVERED_STATE="timeout|$(delivered_state "$loop")"
	return 1
}

# ---------------------------------------------------------------------------
# Outcome watcher.
# ---------------------------------------------------------------------------
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
		Succeeded | Failed)
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
	# Wedged: the phase has not changed for CAP/2 seconds (a stuck run).
	if [ $(( $(date +%s) - last_change )) -ge $(( CAP / 2 )) ]; then
		OUT_WEDGED="yes"
	fi
	return 3
}

# ---------------------------------------------------------------------------
# Killers (record LIVE evidence: UTC time, phase+iteration+baseCommit at the
# moment of the kill, the killed + replacement pod UIDs, Ready time, PVC UID).
# ---------------------------------------------------------------------------
kill_operator() {
	local case="$1"
	local loop="$2"
	local dir="$OUTDIR/$case"
	local ts before opuid phase_iter pvcuid
	ts=$(utc)
	before=$(op_pod)
	opuid=$(pod_uid "$OP_NS" "$before")
	phase_iter=$(loop_meta "$loop")
	pvcuid=$(pvc_uid "$loop")
	kubectl --context "$CTX" -n "$OP_NS" delete pod "$before" --wait=false >/dev/null 2>&1 || true
	log "$case" "killed operator pod $before (uid $opuid) at $ts [phase/iter/base: $phase_iter]"
	# Wait for the replacement pod to come up AND be Ready, recording the
	# Ready instant.
	local after="" ready_at=""
	local deadline=$(( $(date +%s) + 120 ))
	while [ "$(date +%s)" -lt "$deadline" ]; do
		after=$(op_pod)
		if [ -n "$after" ] && [ "$after" != "$before" ]; then
			if kubectl --context "$CTX" -n "$OP_NS" wait --for=condition=ready "pod/$after" --timeout=120s >/dev/null 2>&1; then
				ready_at=$(utc)
			fi
			break
		fi
		sleep 2
	done
	local rep_uid after_digest
	rep_uid=$(pod_uid "$OP_NS" "$after")
	after_digest=$(op_digest)
	{
		echo "operator-kill: case=$case"
		echo "kill-utc: $ts"
		echo "loop: $loop"
		echo "phase-iteration-baseCommit-at-kill: $phase_iter"
		echo "killed-pod: $before"
		echo "killed-pod-uid: $opuid"
		echo "replaced-pod: $after"
		echo "replaced-pod-uid: $rep_uid"
		echo "replaced-ready-utc: $ready_at"
		echo "digest-before: $OP_DIGEST_BEFORE"
		echo "digest-after: $after_digest"
		echo "workspace-pvc-uid: $pvcuid"
	} >"$dir/kill.txt" 2>&1
	if [ "$OP_DIGEST_BEFORE" != "$after_digest" ]; then
		log "$case" "WARNING: operator digest changed ($OP_DIGEST_BEFORE -> $after_digest) — the operator was REDEPLOYED, not just restarted"
	fi
}

kill_sandbox() {
	local case="$1"
	local loop="$2"
	local dir="$OUTDIR/$case"
	local ts sp uid phase_iter pvcuid agent_state
	ts=$(utc)
	sp="$(sandbox_pod "$loop")"
	uid=$(pod_uid "$NS" "$sp")
	phase_iter=$(loop_meta "$loop")
	pvcuid=$(pvc_uid "$loop")
	# The agent container's running state AT THE KILL (proof of work in
	# flight): the agent container's state (running/waiting/terminated) + its
	# state detail.
	agent_state=$(kubectl --context "$CTX" -n "$NS" get pod "$sp" -o json 2>/dev/null | python3 -c '
import json,sys
try:
    d=json.load(sys.stdin)
except Exception:
    print("<gone>"); sys.exit(0)
cs=(d.get("status") or {}).get("containerStatuses") or []
for c in cs:
    if c.get("name")=="agent":
        st=c.get("state") or {}
        k=list(st.keys())
        det=""
        if k:
            det=str(st.get(k[0],{}))
        print(k[0] if k else "<none>", det)
        break
else:
    print("<no-agent-container>")
' 2>/dev/null || echo "<read-failed>")
	kubectl --context "$CTX" -n "$NS" delete pod "$sp" --wait=false >/dev/null 2>&1 || true
	log "$case" "killed sandbox pod $sp (uid $uid) at $ts [phase/iter/base: $phase_iter] agent-state: $agent_state"
	{
		echo "sandbox-kill: case=$case"
		echo "kill-utc: $ts"
		echo "loop: $loop"
		echo "phase-iteration-baseCommit-at-kill: $phase_iter"
		echo "killed-pod: $sp"
		echo "killed-pod-uid: $uid"
		echo "agent-container-state-at-kill: $agent_state"
		echo "workspace-pvc-uid: $pvcuid"
	} >"$dir/kill.txt" 2>&1
}

kill_verify_job_pod() {
	local case="$1"
	local loop="$2"
	local dir="$OUTDIR/$case"
	local ts vp uid vmeta juid pvcuid
	ts=$(utc)
	vp=$(verify_job_pod "$loop")
	vmeta=$(verify_meta "$loop")
	juid=$(verify_job_uid "$loop")
	pvcuid=$(pvc_uid "$loop")
	if [ -z "$vp" ]; then
		log "$case" "WARNING: no verify Job pod found to kill"
		{
			echo "verify-kill: case=$case"
			echo "kill-utc: $ts"
			echo "loop: $loop"
			echo "phase-iteration-baseCommit-at-kill: $(loop_meta "$loop")"
			echo "verify-pod: <none-found>"
			echo "verify-meta-at-kill: $vmeta"
			echo "verify-job-uid-at-kill: $juid"
			echo "workspace-pvc-uid: $pvcuid"
		} >"$dir/kill.txt" 2>&1
		return
	fi
	uid=$(pod_uid "$NS" "$vp")
	kubectl --context "$CTX" -n "$NS" delete pod "$vp" --wait=false >/dev/null 2>&1 || true
	log "$case" "killed verify Job pod $vp (uid $uid) at $ts [verify-meta: $vmeta]"
	{
		echo "verify-kill: case=$case"
		echo "kill-utc: $ts"
		echo "loop: $loop"
		echo "phase-iteration-baseCommit-at-kill: $(loop_meta "$loop")"
		echo "killed-pod: $vp"
		echo "killed-pod-uid: $uid"
		echo "verify-meta-at-kill: $vmeta"
		echo "verify-job-uid-at-kill: $juid"
		echo "workspace-pvc-uid: $pvcuid"
	} >"$dir/kill.txt" 2>&1
}

# ---------------------------------------------------------------------------
# Case runner.
# ---------------------------------------------------------------------------
# run_case <case> <loop-name> <target-phase|deliver> <kill-fn>:
#   start the LIVE evidence streams, apply the Loop, wait for the target
#   phase, fire the kill, watch the outcome, wait for Delivered, capture
#   everything, and record the outcome.
run_case() {
	local case="$1"
	local name="$2"
	local target="$3"
	local killfn="$4"
	local dir
	dir="$OUTDIR/$case"
	mkdir -p "$dir"
	log "$case" "applying Loop $name (target: $target)"

	# Start the LIVE evidence streams BEFORE apply (the whole case).
	start_events_watch "$name" "$dir/events.jsonl"
	start_sandbox_log_stream "$name" "$dir/sandbox.log"
	# Record the workspace PVC UID + baseCommit BEFORE the kill (D53 anchors).
	# (The PVC is created at apply; read it right after apply, before the
	# kill, and again after — the before value is taken here.)

	apply_loop "$name"

	# Record the workspace PVC UID right after apply (the PVC UID is stable
	# once the PVC is created). baseCommit is set by the operator during
	# workspace init (later than 3s) and is immutable thereafter; it is read
	# at the kill moment (alongside the ws-head-before peek) so the "before"
	# value reflects the initialized workspace, not the pre-init empty state.
	local ws_head_before pvc_before base_before
	sleep 3
	pvc_before=$(pvc_uid "$name")

	local reached=no
	# Wait for the target phase (or the deliver Job running for the deliver
	# case) with a generous pre-kill window (the run must REACH the target).
	local deadline=$(( $(date +%s) + 600 ))
	while [ "$(date +%s)" -lt "$deadline" ]; do
		local ph
		ph=$(phase_of "$name")
		case "$target" in
		deliver)
			# The deliver case: the Loop is Succeeded AND the deliver Job is
			# running (in flight).
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
			case "$ph" in
			Succeeded | Failed)
				reached="early"
				break
				;;
			esac
			;;
		esac
		sleep "$PHASE_POLL"
	done

	# Pre-kill settle (KILL_DELAY), then fire the kill. The deliver case skips
	# the settle: the deliver Job runs only ~10s, so a 3s settle risks missing
	# the window (the Job completes before the kill). Kill it immediately.
	if [ "$reached" = "yes" ]; then
		# Peek the workspace HEAD right before the kill (at the target phase,
		# when work is in flight) — the D53 "before" anchor. This is the
		# workspace HEAD the kill interrupts; the "after" peek (post-run) is
		# checked to build on this. Read baseCommit here too (at the kill
		# moment, when the workspace is initialized), not 3s after apply (when
		# it may not be set yet).
		peek_workspace "$name" "$dir/ws-head-before.txt" ""
		ws_head_before=$(ws_head "$dir/ws-head-before.txt")
		base_before=$(baseCommit_of "$name")
		log "$case" "ws-head-before (at kill): ${ws_head_before:0:8} baseCommit-before: ${base_before:0:8}"
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
		# No kill fired: the "before" anchors are read at the terminal phase
		# (the run completed normally, so the before==after). Read them now so
		# the outcome record is complete.
		peek_workspace "$name" "$dir/ws-head-before.txt" ""
		ws_head_before=$(ws_head "$dir/ws-head-before.txt")
		base_before=$(baseCommit_of "$name")
		echo "kill: not-fired (terminal before target)" >"$dir/kill.txt" 2>&1
	fi

	# Watch the outcome (phase) with the CAP.
	log "$case" "watching outcome (CAP ${CAP}s)..."
	local rc=0
	watch_outcome "$name" "$CAP" || rc=$?
	if [ $rc -eq 3 ]; then
		log "$case" "no terminal phase within ${CAP}s (wedged=$( [ "$OUT_WEDGED" = yes ] && echo yes || echo no ))"
	fi

	# Wait for the Delivered condition to resolve (True/False, not InProgress)
	# BEFORE recording the outcome — this is the reviewer's point 4. Only when
	# the phase reached Succeeded (a Failed phase has no delivery).
	if [ "$OUT_PHASE" = "Succeeded" ]; then
		log "$case" "waiting for Delivered (DELIVERED_CAP ${DELIVERED_CAP}s)..."
		wait_for_delivered "$name" "$DELIVERED_CAP" || true
		log "$case" "Delivered: $DELIVERED_STATE"
	fi

	# Post-kill: record the workspace HEAD + PVC UID + baseCommit (the
	# "after" state, D53 continuity anchors), and check the final verified
	# commit builds on the pre-kill HEAD.
	local ws_head_after pvc_after base_after
	peek_workspace "$name" "$dir/ws-head-after.txt" "$ws_head_before"
	ws_head_after=$(ws_head "$dir/ws-head-after.txt")
	pvc_after=$(pvc_uid "$name")
	base_after=$(baseCommit_of "$name")

	# Stop the LIVE evidence streams (capture the kill-time events).
	stop_events_watch "$dir/events.jsonl"
	stop_sandbox_log_stream "$name"

	# Capture ALL logs (never /dev/null). The loop.json is captured here,
	# AFTER the Delivered wait, so status.delivery is populated.
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
		echo "delivered: ${DELIVERED_STATE}"
		echo "pr-number: $(pr_number_of "$name")"
		echo "pr-url: $(pr_url_of "$name")"
		echo "delivery-commit: $(delivery_commit_of "$name")"
		echo "current-verify-commit: $(current_verify_commit "$name")"
		# D53 workspace continuity.
		echo "pvc-uid-before: $pvc_before"
		echo "pvc-uid-after: $pvc_after"
		echo "pvc-uid-unchanged: $( [ "$pvc_before" = "$pvc_after" ] && echo yes || echo no )"
		echo "base-commit-before: $base_before"
		echo "base-commit-after: $base_after"
		echo "base-commit-unchanged: $( [ "$base_before" = "$base_after" ] && echo yes || echo no )"
		echo "ws-head-before: $ws_head_before"
		echo "ws-head-after: $ws_head_after"
		# The "builds on" check is in ws-head-after.txt (BUILD-ON line) when a
		# pre-kill HEAD existed.
		echo "ws-head-builds-on: $(grep -E '^BUILD-ON' "$dir/ws-head-after.txt" 2>/dev/null | head -1 || echo '<no pre-kill HEAD or peek empty>')"
		echo "operator-digest-after: $(op_digest)"
	} >"$dir/outcome.txt" 2>&1
	log "$case" "outcome: phase=${OUT_PHASE} reason=${OUT_REASON} wedged=${OUT_WEDGED} deliver=${OUT_DELIVER} delivered=${DELIVERED_STATE} pr=$(pr_number_of "$name")"
	log "$case" "D53: pvc-unchanged=$([ "$pvc_before" = "$pvc_after" ] && echo yes || echo no) base-unchanged=$([ "$base_before" = "$base_after" ] && echo yes || echo no) ws-head: ${ws_head_before:0:8} -> ${ws_head_after:0:8}"

	# Clean up this run's Loop + Jobs (unique name, so no collision next run).
	kubectl --context "$CTX" -n "$NS" delete loop "$name" --wait=true --timeout=120s >/dev/null 2>&1 || true
	kubectl --context "$CTX" -n "$NS" delete jobs --all --wait=true --timeout=60s >/dev/null 2>&1 || true
	# Delete the peek pod if it lingers (it self-cleans, but be safe).
	kubectl --context "$CTX" -n "$NS" delete pod "peek-$name" --wait=false >/dev/null 2>&1 || true
	sleep 5
}

# ---------------------------------------------------------------------------
# Run the control first, then each kill case (strictly sequential: one kind
# job at a time). The control uses the same LIVE plumbing (events + sandbox
# log) so it is a fair baseline.
# ---------------------------------------------------------------------------
log "control" "=== A2 restart-durability e2e (measure only) ==="
log "control" "operator digest: $OP_DIGEST_BEFORE (runner flag: $RUNNER_FLAG)"
log "control" "CAP=${CAP}s DELIVERED_CAP=${DELIVERED_CAP}s KILL_DELAY=${KILL_DELAY}s PHASE_POLL=${PHASE_POLL}s"

# The control: apply the Loop, no kill, watch to a terminal phase, wait for
# Delivered. Uses the same LIVE evidence streams as a kill case.
run_control() {
	local case="control"
	local name
	name="a2-control-$(date +%s)"
	local dir="$OUTDIR/$case"
	mkdir -p "$dir"
	log "control" "applying control Loop $name"
	start_events_watch "$name" "$dir/events.jsonl"
	start_sandbox_log_stream "$name" "$dir/sandbox.log"
	apply_loop "$name"
	sleep 3
	local pvc_before base_before ws_head_before
	pvc_before=$(pvc_uid "$name")
	# The control has no kill moment; the D53 "before" anchor is the workspace
	# HEAD once the runner has initialized the repo (cloned + committed the
	# base/seed), which takes ~1-2 min after apply. Wait until the workspace
	# has a HEAD (poll up to 240s) before peeking, so the "before" HEAD is the
	# initialized repo, not an empty PVC. baseCommit is read at this same
	# initialized point (not 3s after apply, when it may not be set yet).
	local wh deadline3
	deadline3=$(( $(date +%s) + 240 ))
	wh=""
	while [ -z "$wh" ]; do
		[ "$(date +%s)" -ge "$deadline3" ] && break
		peek_workspace "$name" "$dir/ws-head-poll.txt" ""
		wh=$(ws_head "$dir/ws-head-poll.txt")
		sleep 10
	done
	if [ -n "$wh" ]; then
		mv -f "$dir/ws-head-poll.txt" "$dir/ws-head-before.txt" 2>/dev/null || cp "$dir/ws-head-poll.txt" "$dir/ws-head-before.txt"
		ws_head_before="$wh"
	else
		peek_workspace "$name" "$dir/ws-head-before.txt" ""
		ws_head_before=$(ws_head "$dir/ws-head-before.txt")
	fi
	base_before=$(baseCommit_of "$name")
	log "control" "ws-head-before (initialized): ${ws_head_before:0:8}"
	local rc=0
	watch_outcome "$name" "$CAP" || rc=$?
	[ $rc -eq 3 ] && log "control" "control run did not reach a terminal phase within CAP (wedged=$( [ "$OUT_WEDGED" = yes ] && echo yes || echo no ))"
	if [ "$OUT_PHASE" = "Succeeded" ]; then
		wait_for_delivered "$name" "$DELIVERED_CAP" || true
	fi
	local pvc_after base_after ws_head_after
	peek_workspace "$name" "$dir/ws-head-after.txt" "$ws_head_before"
	ws_head_after=$(ws_head "$dir/ws-head-after.txt")
	pvc_after=$(pvc_uid "$name")
	base_after=$(baseCommit_of "$name")
	stop_events_watch "$dir/events.jsonl"
	stop_sandbox_log_stream "$name"
	capture_logs "$case" "$name"
	{
		echo "case: control"
		echo "loop: $name"
		echo "target: none (uninterrupted)"
		echo "terminal-phase: ${OUT_PHASE}"
		echo "iteration: ${OUT_ITER}"
		echo "reason: ${OUT_REASON}"
		echo "wedged: ${OUT_WEDGED}"
		echo "deliver-job: ${OUT_DELIVER}"
		echo "delivered: ${DELIVERED_STATE}"
		echo "pr-number: $(pr_number_of "$name")"
		echo "pr-url: $(pr_url_of "$name")"
		echo "delivery-commit: $(delivery_commit_of "$name")"
		echo "current-verify-commit: $(current_verify_commit "$name")"
		echo "pvc-uid-before: $pvc_before"
		echo "pvc-uid-after: $pvc_after"
		echo "pvc-uid-unchanged: $( [ "$pvc_before" = "$pvc_after" ] && echo yes || echo no )"
		echo "base-commit-before: $base_before"
		echo "base-commit-after: $base_after"
		echo "base-commit-unchanged: $( [ "$base_before" = "$base_after" ] && echo yes || echo no )"
		echo "ws-head-before: $ws_head_before"
		echo "ws-head-after: $ws_head_after"
		echo "ws-head-builds-on: $(grep -E '^BUILD-ON' "$dir/ws-head-after.txt" 2>/dev/null | head -1 || echo '<no pre-kill HEAD or peek empty>')"
		echo "operator-digest-after: $(op_digest)"
	} >"$dir/outcome.txt" 2>&1
	log "control" "outcome: phase=${OUT_PHASE} reason=${OUT_REASON} wedged=${OUT_WEDGED} deliver=${OUT_DELIVER} delivered=${DELIVERED_STATE} pr=$(pr_number_of "$name")"
	log "control" "D53: pvc-unchanged=$([ "$pvc_before" = "$pvc_after" ] && echo yes || echo no) base-unchanged=$([ "$base_before" = "$base_after" ] && echo yes || echo no) ws-head: ${ws_head_before:0:8} -> ${ws_head_after:0:8}"
	kubectl --context "$CTX" -n "$NS" delete loop "$name" --wait=true --timeout=120s >/dev/null 2>&1 || true
	kubectl --context "$CTX" -n "$NS" delete jobs --all --wait=true --timeout=60s >/dev/null 2>&1 || true
	kubectl --context "$CTX" -n "$NS" delete pod "peek-$name" --wait=false >/dev/null 2>&1 || true
	sleep 5
}

for case in "${CASES[@]}"; do
	case "$case" in
	control) run_control ;;
	op-planning) run_case "$case" "a2-opplan-$(date +%s)" "Planning" kill_operator ;;
	op-implementing) run_case "$case" "a2-opimpl-$(date +%s)" "Implementing" kill_operator ;;
	op-verifying) run_case "$case" "a2-opver-$(date +%s)" "Verifying" kill_operator ;;
	op-deliver) run_case "$case" "a2-opdel-$(date +%s)" "deliver" kill_operator ;;
	sandbox-implementing) run_case "$case" "a2-sandbox-impl-$(date +%s)" "Implementing" kill_sandbox ;;
	verify-job) run_case "$case" "a2-verify-$(date +%s)" "Verifying" kill_verify_job_pod ;;
	*) die "unknown case $case" ;;
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
	echo "- runner image: \`$RUNNER_IMAGE\`"
	echo "- CAP: ${CAP}s, DELIVERED_CAP: ${DELIVERED_CAP}s, KILL_DELAY: ${KILL_DELAY}s, PHASE_POLL: ${PHASE_POLL}s"
	echo "- generated: $(utc)"
	echo
	echo "The control (uninterrupted) outcome is the baseline each kill case is compared against. Evidence is captured LIVE: events.jsonl (a kubectl-get-events-watch stream for the whole case), kill.txt (UTC time + phase/iteration/baseCommit at the kill + killed/replacement pod UIDs + Ready time), the sandbox live log stream, and RO peek pods of the workspace PVC HEAD before/after the kill. Delivered is waited on (True/False, not InProgress) and the PR number is recorded."
	echo
	echo "| case | target | phase | reason | wedged? | deliver job | Delivered | PR# | PVC unchanged | baseCommit unchanged | ws HEAD before→after | log dir |"
	echo "|------|--------|-------|--------|---------|-------------|-----------|-----|---------------|----------------------|----------------------|---------|"
	for case in "${CASES[@]}"; do
		dir="$OUTDIR/$case"
		[ -f "$dir/outcome.txt" ] || { echo "| $case | ? | ? | ? | ? | ? | ? | ? | ? | ? | ? | (no outcome) |"; continue; }
		local_target=$(sed -n 's/^target: //p' "$dir/outcome.txt" | head -1)
		phase=$(sed -n 's/^terminal-phase: //p' "$dir/outcome.txt" | head -1)
		reason=$(sed -n 's/^reason: //p' "$dir/outcome.txt" | head -1)
		wedged=$(sed -n 's/^wedged: //p' "$dir/outcome.txt" | head -1)
		deliver=$(sed -n 's/^deliver-job: //p' "$dir/outcome.txt" | head -1)
		delivered=$(sed -n 's/^delivered: //p' "$dir/outcome.txt" | head -1)
		prn=$(sed -n 's/^pr-number: //p' "$dir/outcome.txt" | head -1)
		pvcu=$(sed -n 's/^pvc-uid-unchanged: //p' "$dir/outcome.txt" | head -1)
		baseu=$(sed -n 's/^base-commit-unchanged: //p' "$dir/outcome.txt" | head -1)
		wsb=$(sed -n 's/^ws-head-before: //p' "$dir/outcome.txt" | head -1)
		wsa=$(sed -n 's/^ws-head-after: //p' "$dir/outcome.txt" | head -1)
		[ "$case" = "control" ] && local_target="none"
		[ -z "$prn" ] && prn="-"
		echo "| $case | ${local_target} | ${phase} | ${reason} | ${wedged} | ${deliver} | ${delivered} | ${prn} | ${pvcu} | ${baseu} | \`${wsb:0:8}\`→\`${wsa:0:8}\` | [logs]($case/) |"
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
		echo "  logs kept in \`.samples/a2-restart-*/$case/\` (events.jsonl, sandbox.log live stream, ws-head-before/after.txt peek pods, operator.log, proxy.log, verify-*.log, deliver.log, loop.json)."
	done
} >"$OUTDIR/RESULTS.md"

log "table" "RESULTS written to $OUTDIR/RESULTS.md"
log "table" "all logs kept under $OUTDIR/ (never /dev/null)"
echo ""
echo "================ A2 RESULTS TABLE ================"
cat "$OUTDIR/RESULTS.md"
echo "=================================================="
log "table" "done. Paste the table above into the PR. No fixes in this PR."
