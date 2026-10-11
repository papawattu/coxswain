#!/usr/bin/env bash
# A3 e2e (PLAN-ALPHA.md A3): drive a Loop through the `cox` CLI's lifecycle
# commands — pause -> resume -> describe -> redeliver — and keep the kept
# `cox logs` output as evidence.
#
# Prerequisites (same as the other e2e): the controller is deployed on
# kind-coxswain-dev with a --runner-image; the gitea sample is running in the
# `samples` namespace with the `samples-git-cred` secret; the `cox` CLI is
# built (make bin/cox).
#
# The driver (kubectl cox) runs from this host with `--context
# kind-coxswain-dev -n samples`. The `cox` binary is built from the branch
# under test.
#
# Usage:
#   test/e2e/a3-cli-e2e.sh            # runs the full lifecycle
#   COX_BIN=<path>                    # override the cox binary (default:
#                                      # ./bin/cox built by the script)
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
CTX="${CTX:-kind-coxswain-dev}"
NS="${NS:-samples}"
# A unique Loop name per run (as A2 does): a recreated Loop with the name of an
# earlier run cannot deliver (its coxswain/<loop> branch already exists remotely
# with unrelated history — I81, filed separately). The timestamp suffix keeps
# the branch fresh so the first delivery is clean.
RUN_TS="$(date +%s)"
LOOP_NAME="${LOOP_NAME:-a3-cli-e2e-$RUN_TS}"
LOOP_SRC="${LOOP_SRC:-$ROOT/examples/gocli/tasks/1.loop.yaml}"
EVIDENCE="${EVIDENCE:-$ROOT/.samples/a3-cli-e2e-$RUN_TS}"
COX_BIN="${COX_BIN:-$ROOT/bin/cox}"

mkdir -p "$EVIDENCE"
log() { printf '[a3] %s\n' "$*"; }
fail() { log "FAIL: $*"; exit 1; }

command -v kubectl >/dev/null || fail "kubectl not found"
command -v go >/dev/null || fail "go not found"

# --- build the cox binary from the branch under test ------------------------
log "building $COX_BIN from $(git -C "$ROOT" rev-parse --short HEAD)"
(cd "$ROOT" && go build -o bin/cox ./cmd/cox)
COX="$COX_BIN"
"$COX" --help >/dev/null 2>&1 || fail "cox binary failed: $COX"

kube() { kubectl --context "$CTX" -n "$NS" "$@"; }
cox() { "$COX" --context "$CTX" -n "$NS" "$@"; }
phase() { kube get loop "$LOOP_NAME" -o jsonpath='{.status.phase}' 2>/dev/null || echo ""; }
# The deliver Job's UID (the <loop>-deliver Job, created on Succeeded +
# delivery expected; re-created by a redeliver). "" when no Job exists.
deliver_job_uid() { kube get job "${LOOP_NAME}-deliver" -o jsonpath='{.metadata.uid}' 2>/dev/null || echo ""; }
# The Delivered condition's status + reason.
delivered_condition() { kube get loop "$LOOP_NAME" -o jsonpath='{range .status.conditions[?(@.type=="Delivered")]}{.status} {.reason}{end}' 2>/dev/null; }

# --- preflight: operator deployed, sample gitea reachable ------------------
OPS_NS="${OPS_NS:-coxswain-system}"
if ! kubectl --context "$CTX" -n "$OPS_NS" get deployment coxswain-controller-manager >/dev/null 2>&1; then
  fail "coxswain-controller-manager not found in $OPS_NS (deploy the operator first)"
fi
if ! kube get secret samples-git-cred >/dev/null 2>&1; then
  fail "samples-git-cred secret not found in $NS (the gitea sample is not set up)"
fi
log "operator + gitea sample present"

# --- apply the Loop ---------------------------------------------------------
render="$(mktemp)"
sed "s|name: gocli-task1|name: $LOOP_NAME|" "$LOOP_SRC" >"$render"
kube get loop "$LOOP_NAME" >/dev/null 2>&1 && kube delete loop "$LOOP_NAME" --wait=false >/dev/null 2>&1 || true
kube apply -f "$render" >/dev/null
rm -f "$render"
log "applied Loop $LOOP_NAME"

# wait for the sandbox to start (Planning or later)
for i in $(seq 1 60); do
  ph="$(phase)"
  case "$ph" in
    ""|Pending) ;; # still starting
    *) break ;;
  esac
  sleep 5
done
ph="$(phase)"
[ -n "$ph" ] || fail "Loop never left Pending (ph='$ph')"
log "Loop reached phase $ph"

# --- pause ------------------------------------------------------------------
# Pause while the Loop is still active (not yet terminal).
cox pause "$LOOP_NAME" | tee "$EVIDENCE/pause.out"
# spec.suspend must be true
for i in $(seq 1 30); do
  susp="$(kube get loop "$LOOP_NAME" -o jsonpath='{.spec.suspend}')"
  [ "$susp" = "true" ] && break
  sleep 2
done
[ "$susp" = "true" ] || fail "spec.suspend not true after cox pause (got '$susp')"
# the operator must have paused it (phase Paused)
for i in $(seq 1 60); do
  ph="$(phase)"
  [ "$ph" = "Paused" ] && break
  case "$ph" in Succeeded|Failed|Delivered) fail "Loop went $ph before the pause took effect" ;; esac
  sleep 5
done
[ "$(phase)" = "Paused" ] || fail "Loop not Paused after cox pause (ph='$(phase)')"
log "paused (phase Paused, spec.suspend true)"

# --- describe (while paused) ------------------------------------------------
cox describe "$LOOP_NAME" | tee "$EVIDENCE/describe-paused.out"

# --- resume -----------------------------------------------------------------
cox resume "$LOOP_NAME" | tee "$EVIDENCE/resume.out"
# spec.suspend must be cleared
for i in $(seq 1 30); do
  susp="$(kube get loop "$LOOP_NAME" -o jsonpath='{.spec.suspend}')"
  [ "$susp" != "true" ] && break
  sleep 2
done
[ "$susp" != "true" ] || fail "spec.suspend still true after cox resume"
# the Loop must resume out of Paused
for i in $(seq 1 60); do
  ph="$(phase)"
  [ "$ph" != "Paused" ] && break
  sleep 5
done
[ "$(phase)" != "Paused" ] || fail "Loop still Paused after cox resume"
log "resumed (phase $(phase))"

# --- component logs + plan (kept, per the acceptance criterion) --------------
# Capture each component's logs WHILE its pod is running (the pods are cleaned
# up once the Loop is terminal, so an end-of-run capture would be empty). The
# runner writes .coxswain/PLAN.md at the start of the Implementing phase, so the
# plan is captured in the phase-poll loop (once Implementing), not right after
# resume (still Planning). The peek pod is a read-only cat (UID 65532); the PVC
# is not modified. No `|| true`: a missing/non-empty capture fails the run.
capture_logs() {
  # $1 = component, $2 = out file. The pod is assumed to exist now (the caller
  # gates on the phase); a short poll covers a brief not-ready window.
  local component="$1" out="$2" i
  for i in $(seq 1 45); do
    if cox logs "$LOOP_NAME" --component "$component" --tail 40 2>/dev/null | tee "$out" | grep -q .; then
      return 0
    fi
    sleep 4
  done
  test -s "$out"
}

# The agent and proxy sandbox pods exist right now (post-resume, the sandbox
# pod exists from Planning through the end) — capture them immediately.
capture_logs agent "$EVIDENCE/logs-agent.out"
capture_logs proxy "$EVIDENCE/logs-proxy.out"

# Now drive the Loop to completion, capturing plan (Implementing phase — the
# runner writes PLAN.md there), verify (Verifying phase) and deliver (Succeeded
# phase, the deliver Job runs before Delivered) as each phase is entered. A
# single poll loop watches the phase; each is captured once, the first time its
# phase is seen.
plan_done=false
verify_done=false
deliver_done=false
for i in $(seq 1 300); do
  ph="$(phase)"
  case "$ph" in
    Implementing)
      if [ "$plan_done" = false ]; then
        log "in Implementing; capturing plan"
        cox plan "$LOOP_NAME" | tee "$EVIDENCE/plan.out"
        plan_done=true
      fi
      ;;
    Verifying)
      if [ "$verify_done" = false ]; then
        log "in Verifying; capturing verify logs"
        capture_logs verify "$EVIDENCE/logs-verify.out"
        verify_done=true
      fi
      ;;
    Succeeded)
      if [ "$deliver_done" = false ]; then
        log "in Succeeded (deliver Job running); capturing deliver logs"
        capture_logs deliver "$EVIDENCE/logs-deliver.out"
        deliver_done=true
      fi
      ;;
    Delivered|Failed)
      break
      ;;
  esac
  sleep 4
done
ph="$(phase)"
case "$ph" in Succeeded|Delivered) ;; *) fail "Loop did not reach a terminal phase (ph='$ph')" ;; esac
log "Loop reached $ph"

# --- describe (final) ---------------------------------------------------------
cox describe "$LOOP_NAME" | tee "$EVIDENCE/describe-final.out"
cox history "$LOOP_NAME" | tee "$EVIDENCE/history.out"

# --- assert the kept log/plan evidence is non-empty (no `|| true`) ------------
# The deliver capture only runs if the Loop had a delivery (task 1 does); if the
# Loop reached Succeeded with no deliver Job, deliver_done stays false and the
# deliver file is absent. Assert the files that the run actually produced.
for f in logs-agent.out logs-proxy.out logs-verify.out plan.out; do
  test -s "$EVIDENCE/$f" || fail "evidence $f is empty"
done
if [ "$deliver_done" = true ]; then
  test -s "$EVIDENCE/logs-deliver.out" || fail "evidence logs-deliver.out is empty"
fi
log "component log + plan evidence captured (non-empty)"

# --- redeliver (I52: re-trigger the delivery) -------------------------------
# The redeliver step ALWAYS runs (never skipped): it asserts an observable
# effect. A unique Loop name keeps the first delivery clean (no stale-branch
# poisoning — I81, filed separately). The deliver Job is <loop>-deliver; its
# UID changes when the Job is re-created (a redeliver deletes + re-creates it
# for a FAILED delivery). For a delivery that ALREADY SUCCEEDED (Delivered=True
# for the current verifiedCommit), the operator does NOT re-create the Job —
# it just removes the annotation (redeliverEligible returns false; loop_
# deliver_job.go). Both outcomes are valid, non-skipped redeliver results:
#   (a) first delivery succeeded  -> annotation removed, delivery kept,
#       Job UID unchanged;
#   (b) first delivery failed      -> the operator deletes the failed Job and
#       re-creates a fresh one (a NEW Job UID) for the same verifiedCommit.
# The before/after UIDs and the final Delivered condition are pasted in the
# evidence index. We NEVER require observing the annotation mid-flight: the
# operator removes it within one reconcile, so a poll can miss it; the
# observable is the operator's FINAL state (annotation gone + delivery intact,
# or a new Job).
JOB_UID_BEFORE="$(deliver_job_uid)"
DELIVERED_BEFORE="$(delivered_condition)"
log "deliver Job UID before redeliver: ${JOB_UID_BEFORE:-<none>}" | tee "$EVIDENCE/redeliver-uid-before.txt"
log "Delivered condition before redeliver: ${DELIVERED_BEFORE:-<none>}" | tee "$EVIDENCE/delivered-before.txt"

# The Loop must be in a delivery-eligible state (Succeeded with delivery
# expected) for redeliver to have any effect. It reached Succeeded/Delivered in
# the phase loop above.
ph="$(phase)"
case "$ph" in Succeeded|Delivered) ;; *) fail "Loop not Succeeded/Delivered before redeliver (ph='$ph')" ;; esac

# cox redeliver sets the coxswain.io/redeliver annotation (I52). The command
# itself confirms the annotation was set (its stdout is the evidence); we do
# NOT poll for the annotation mid-flight (the operator removes it within one
# reconcile, so a poll would race).
cox redeliver "$LOOP_NAME" | tee "$EVIDENCE/redeliver.out"
# The command must have succeeded (it exits non-zero if the annotation write
# failed). `set -e` would already abort on a failure, so reaching here means the
# annotation write succeeded.
log "cox redeliver succeeded (the command set the annotation; see redeliver.out)"

# Wait for the operator to act on the annotation: either (a) it removes the
# annotation (an already-delivered Loop keeps its delivery, no new Job) or (b)
# it deletes the failed Job and re-creates a fresh one (a new Job UID). The
# observable we assert: the annotation is eventually REMOVED (the operator
# processed it) AND the delivery state is valid (intact, or a fresh Job for
# the re-delivery). Poll up to ~5 min.
JOB_UID_AFTER=""
ann_final="$(kube get loop "$LOOP_NAME" -o jsonpath='{.metadata.annotations.coxswain\.io/redeliver}')"
for i in $(seq 1 75); do
  ann_final="$(kube get loop "$LOOP_NAME" -o jsonpath='{.metadata.annotations.coxswain\.io/redeliver}')"
  JOB_UID_AFTER="$(deliver_job_uid)"
  # The operator has responded when the annotation is gone (removed).
  if [ -z "$ann_final" ]; then break; fi
  sleep 4
done
JOB_UID_AFTER="$(deliver_job_uid)"
log "deliver Job UID after redeliver: ${JOB_UID_AFTER:-<none>}" | tee "$EVIDENCE/redeliver-uid-after.txt"

# The annotation must be gone (the operator processed it) in every case.
[ -z "$ann_final" ] || fail "coxswain.io/redeliver annotation still present after the operator should have processed it (value '$ann_final')"
log "redeliver annotation removed by the operator"

# The final Delivered condition (re-decided: True/Delivered for a kept delivery,
# or a fresh DeliveryFailed with a new Job for a re-delivery).
DELIVERED_FINAL="$(delivered_condition)"
log "Delivered condition after redeliver: ${DELIVERED_FINAL:-<none>}" | tee "$EVIDENCE/delivered-final.txt"
# cox describe / history to capture the final Loop state (the Delivered
# condition is in describe's conditions block).
cox describe "$LOOP_NAME" | tee "$EVIDENCE/describe-after-redeliver.out"

# Assert the observable effect. The redeliver did SOMETHING observable: the
# annotation was set (the command confirmed) and the operator processed it
# (annotation removed + a valid final state). Two valid outcomes:
#   (a) NEW Job UID (re-delivery: the first delivery had failed, so the
#       operator re-created the Job) -> assert the UID changed.
#   (b) SAME Job UID (already-delivered: the operator kept the delivery) ->
#       assert the Job UID is unchanged and the delivery is intact (Delivered
#       True).
if [ -n "$JOB_UID_AFTER" ] && [ "$JOB_UID_AFTER" != "$JOB_UID_BEFORE" ]; then
  log "redeliver re-created the deliver Job (UID $JOB_UID_BEFORE -> $JOB_UID_AFTER); Delivered=$DELIVERED_FINAL"
elif [ -n "$JOB_UID_BEFORE" ] && [ "$JOB_UID_AFTER" = "$JOB_UID_BEFORE" ]; then
  # The Job was not re-created: the Loop was already delivered for this
  # verifiedCommit, so the annotation was removed and the delivery kept.
  case "$DELIVERED_FINAL" in
    True*) log "redeliver on an already-delivered Loop: annotation removed, delivery kept (Job UID unchanged: $JOB_UID_AFTER)" ;;
    *) fail "already-delivered case but Delivered is not True (got: '$DELIVERED_FINAL')" ;;
  esac
else
  # No Job before or after: the delivery never ran (should not happen for a
  # Succeeded Loop with delivery expected — a real failure).
  fail "redeliver produced no observable effect (Job UID before: ${JOB_UID_BEFORE:-<none>}, after: ${JOB_UID_AFTER:-<none>})"
fi

# --- evidence index ----------------------------------------------------------
{
  echo "A3 CLI e2e: Loop $LOOP_NAME"
  echo "operator image: $(kube get deployment coxswain-controller-manager -o jsonpath='{.spec.template.spec.containers[0].image}' 2>/dev/null || echo unknown)"
  echo "cox binary: $COX (built from $(git -C "$ROOT" rev-parse --short HEAD))"
  echo "final phase: $(phase)"
  echo
  for f in "$EVIDENCE"/*.out; do
    echo "=== $(basename "$f") ==="
    cat "$f"
    echo
  done
} >"$EVIDENCE/index.txt" 2>&1 || true
log "evidence -> $EVIDENCE"
log "PASS: A3 CLI lifecycle (pause -> resume -> describe -> redeliver) completed"
