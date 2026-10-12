#!/usr/bin/env bash
# A3 e2e (PLAN-ALPHA.md A3): drive a Loop through the `cox` CLI's lifecycle
# commands — pause -> resume -> describe -> redeliver — and keep the kept
# `cox logs` output as evidence.
#
# The redeliver step proves its REAL path (I52): it STAGES a deterministic
# FAILED first delivery (by pre-creating the delivery branch coxswain/<loop>
# with unrelated history in the Gitea samples repo), so the first deliver Job
# fails and `cox redeliver` re-delivers it (a NEW Job UID, ending
# Delivered=True). See the redeliver section below. The first delivery is
# never skipped and a successful-first-delivery run never prints PASS here.
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
# The Gitea repo the Loop delivers to (spec.workspace.repo) and the delivery
# branch prefix (spec.delivery.branchPrefix, default "coxswain/"). Used only to
# stage / clean the delivery branch in the in-cluster Gitea (the redeliver
# step); both match the sample manifest.
GIT_REPO="samples/gocli"
BRANCH_PREFIX="coxswain/"

mkdir -p "$EVIDENCE"
log() { printf '[a3] %s\n' "$*"; }
fail() { log "FAIL: $*"; echo; echo "=== FAILED: see $EVIDENCE/run.log ==="; exit 1; }

command -v kubectl >/dev/null || fail "kubectl not found"
command -v go >/dev/null || fail "go not found"
# git + curl are used by the redeliver step to stage / clean the delivery
# branch in the in-cluster Gitea (see below).
command -v git >/dev/null || fail "git not found"
command -v curl >/dev/null || fail "curl not found"

# The e2e's own stdout is captured to $EVIDENCE/run.log (so any "shown working"
# claim can be cited from the run.log lines around the step — I76). Redirecting
# stderr too means a mid-run failure leaves its error in run.log.
exec >"$EVIDENCE/run.log" 2>&1

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
# The Deliver Job's push logs (the push container) — evidence of WHY a delivery
# failed (e.g. the non-fast-forward rejection the staging provokes).
deliver_push_logs() { kube logs "job/${LOOP_NAME}-deliver" -c push --tail=40 2>/dev/null || echo ""; }

# --- Gitea helpers (staging the delivery branch) ----------------------------
# The in-cluster Gitea is only reachable in-cluster, so it is reached host-side
# through a kubectl port-forward (the S2 seed's pattern). The samples
# basic-auth credential (the Loop's own gitCredentialSecret — nothing else)
# is read from the cluster so the staging never disagrees with what the Loop
# uses. Gitea rejects a credential embedded in the URL, so git uses GIT_ASKPASS
# (the same credential) instead of an http://user:pass@ URL.
gitea_port_forward() { # sets $GITEA_API and starts the port-forward (pid in $PF_PID)
  GIT_USER=$(kube get secret samples-git-cred -o jsonpath='{.data.username}' | base64 -d)
  GIT_PASS=$(kube get secret samples-git-cred -o jsonpath='{.data.password}' | base64 -d)
  PF_PORT=$(( (RANDOM % 1000) + 35000 ))
  kubectl --context "$CTX" -n "$NS" port-forward svc/gitea "$PF_PORT:3000" >/dev/null 2>&1 &
  PF_PID=$!
  local ok=0
  for _ in $(seq 1 30); do
    if curl -fsS -m 2 "http://127.0.0.1:$PF_PORT/api/healthz" >/dev/null 2>&1; then ok=1; break; fi
    sleep 0.5
  done
  [ "$ok" = 1 ] || fail "gitea port-forward did not answer on 127.0.0.1:$PF_PORT (gitea pod not Ready?)"
  GITEA_API="http://127.0.0.1:$PF_PORT"
  # git askpass: emit the samples credential for username/password prompts.
  cat >"$EVIDENCE/askpass.sh" <<'EOF'
#!/bin/sh
case "$1" in
  *Username*) printf '%s\n' "$GIT_USER";;
  *Password*) printf '%s\n' "$GIT_PASS";;
esac
EOF
  chmod +x "$EVIDENCE/askpass.sh"
  export GIT_USER GIT_PASS GIT_ASKPASS="$EVIDENCE/askpass.sh" GIT_TERMINAL_PROMPT=0
  GIT_URL="$GITEA_API/$GIT_REPO.git"
  log "gitea port-forward ready ($GITEA_API, user $GIT_USER)"
}
gitea_stop() { [ -n "${PF_PID:-}" ] && kill "$PF_PID" 2>/dev/null || true; PF_PID=""; }
trap 'gitea_stop; kube delete loop "$LOOP_NAME" --wait=false >/dev/null 2>&1 || true' EXIT

# stage_delivery_branch BR: create BR in the Gitea repo with a single commit on
# UNRELATED history (a scratch clone: no fetch of the repo, just an empty commit
# pushed to BR). A later deliver push to the same BR is then non-fast-forward and
# rejected ("fetch first") — a deterministic FAILED delivery. Idempotent against
# a stale copy from a prior run (delete BR first).
stage_delivery_branch() {
  local br="$1" d; d=$(mktemp -d)
  # Remove a stale copy of the branch (unique name makes this a no-op normally).
  # `set +e`: deleting a non-existent branch returns 1; that is the success case.
  set +e
  ( cd "$d" \
    && git init -q -b main \
    && git remote add origin "$GIT_URL" \
    && git push -q origin ":$br" 2>/dev/null )
  set -e
  # A fresh empty-commit scratch repo on UNRELATED history (no fetch of the
  # repo), pushed to BR.
  ( cd "$d" \
    && git config user.name "coxswain-a3-e2e" \
    && git config user.email "e2e@coxswain.local" \
    && git commit -q --allow-empty -m "unrelated history: staged by the a3 e2e to force a failed delivery" \
    && git push -q -u origin "HEAD:$br" ) \
    || fail "staging delivery branch $br (git push failed; see $EVIDENCE/run.log)"
  # Verify the branch now exists (a head that is NOT part of the repo's branch
  # history — unrelated history — so a later deliver push is non-fast-forward).
  local head; head=$(git ls-remote "$GIT_URL" "refs/heads/$br" | awk '{print $1}')
  [ -n "$head" ] || fail "delivery branch $br not visible after staging (ls-remote empty)"
  rm -rf "$d"
  log "staged delivery branch $br (head $head) — the first delivery will be rejected"
}
# delete_delivery_branch BR: remove BR (the run's cleanup). Deleting a missing
# branch is a no-op (unique name makes it a no-op normally).
delete_delivery_branch() {
  local br="$1" d; d=$(mktemp -d)
  set +e
  ( cd "$d" \
    && git init -q -b main \
    && git remote add origin "$GIT_URL" \
    && git push -q origin ":$br" 2>/dev/null )
  set -e
  rm -rf "$d"
  log "deleted delivery branch $br"
}

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
# Applied BEFORE staging the delivery branch: the delivery branch name is now
# coxswain/<loop>-<uid8> (R25 I81), and uid8 is the Loop's metadata.uid[:8],
# which only exists once the Loop is applied. The branch must be staged with the
# NEW (uid-suffixed) name so the first delivery's push is rejected (fetch first)
# and the redeliver proof (delete the branch -> re-deliver) still holds — with
# the I81 fix, the old coxswain/<loop> name no longer collides, so staging the
# old name would make the first delivery SUCCEED and the redeliver step would
# fail.
render="$(mktemp)"
sed "s|name: gocli-task1|name: $LOOP_NAME|" "$LOOP_SRC" >"$render"
kube get loop "$LOOP_NAME" >/dev/null 2>&1 && kube delete loop "$LOOP_NAME" --wait=false >/dev/null 2>&1 || true
kube apply -f "$render" >/dev/null
rm -f "$render"
log "applied Loop $LOOP_NAME"

# --- stage a FAILED first delivery (the redeliver path) ---------------------
# Pre-create the delivery branch coxswain/<loop>-<uid8> in the Gitea samples
# repo with UNRELATED history, so the deliver Job's push is non-fast-forward and
# rejected ("fetch first") -> a deterministic FAILED first delivery (Delivered
# =False reason=DeliveryFailed). `cox redeliver` then re-delivers it (a NEW
# Job UID, ending Delivered=True) once the stale branch is deleted. uid8 is the
# Loop's metadata.uid[:8] (I81). Staged via the in-cluster Gitea's existing
# samples credentials (nothing else) — a deterministic failed first delivery,
# not a cross-run leftover.
# The uid is available as soon as the Loop is applied (the API assigns it at
# create); wait a moment for it to be populated, then derive the branch.
for i in $(seq 1 20); do
  LOOP_UID="$(kube get loop "$LOOP_NAME" -o jsonpath='{.metadata.uid}' 2>/dev/null || echo "")"
  [ -n "$LOOP_UID" ] && break
  sleep 1
done
[ -n "$LOOP_UID" ] || fail "could not read the Loop's metadata.uid (needed for the uid-suffixed delivery branch)"
# uid8 = first 8 chars of the uid (the same truncation the operator uses).
UID8="${LOOP_UID:0:8}"
[ "${#UID8}" -eq 8 ] || fail "uid8 must be 8 chars (got '${UID8}' from uid '${LOOP_UID}')"
DELIVER_BRANCH="${BRANCH_PREFIX}${LOOP_NAME}-${UID8}"
gitea_port_forward
stage_delivery_branch "$DELIVER_BRANCH"
gitea_stop
log "staged the uid-suffixed delivery branch (I81) — the first delivery will be rejected (fetch first)"

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
    *)
      # A FAILED first delivery is expected here (the branch was staged): the
      # phase is Succeeded with Delivered=False reason=DeliveryFailed. The
      # Delivered condition is what decides the redeliver; the phase alone
      # (Succeeded) is not a terminal signal, so break on the delivery outcome.
      ;;
  esac
  # Break once the first delivery has FAILED (the staged outcome) so the
  # redeliver step can act — a clean Succeeded (Delivered InProgress/True) is
  # not a failed delivery, and waiting for a terminal phase would block the
  # run (the operator never leaves the first failed deliver Job).
  dc="$(delivered_condition)"
  case "$dc" in
    False*DeliveryFailed*|*DeliveryFailed*) break ;;
  esac
  sleep 4
done
ph="$(phase)"
# The Loop must be Succeeded (the work passed verify) — delivery failed, which
# is the staged outcome the redeliver step re-runs.
case "$ph" in Succeeded) ;; *) fail "Loop did not reach Succeeded before redeliver (ph='$ph')" ;; esac
# The first delivery MUST have failed (the branch was staged): assert the
# Deliver condition is False with reason DeliveryFailed (no successful first
# delivery — otherwise there would be nothing to redeliver).
case "$(delivered_condition)" in
  False*DeliveryFailed*) log "first delivery FAILED as staged (Delivered=$(delivered_condition))" ;;
  *) fail "first delivery did not fail as staged (Delivered='$(delivered_condition)'); the redeliver step cannot be proven" ;;
esac
log "Loop reached $ph with a FAILED first delivery (staged)"

# Capture the deliver Job's push logs (the non-fast-forward rejection — the
# evidence that the staging worked). Kept, not discarded.
log "capturing the failed deliver Job's push logs (the rejection)"
deliver_push_logs | tee "$EVIDENCE/logs-deliver-failed.out"
if [ ! -s "$EVIDENCE/logs-deliver-failed.out" ]; then
  fail "the failed deliver Job's push logs are empty (the staging did not produce a visible rejection)"
fi

# --- describe (final, before redeliver) ----------------------------------------
cox describe "$LOOP_NAME" | tee "$EVIDENCE/describe-pre-redeliver.out"
cox history "$LOOP_NAME" | tee "$EVIDENCE/history.out"

# --- assert the kept log/plan evidence is non-empty (no `|| true`) ------------
# The agent/proxy/verify captures and the plan are produced while the Loop is
# active; the deliver capture is the FAILED Job's push logs (captured above).
for f in logs-agent.out logs-proxy.out logs-verify.out plan.out logs-deliver-failed.out; do
  test -s "$EVIDENCE/$f" || fail "evidence $f is empty"
done
# The in-run deliver capture (logs-deliver.out) is only present if the Loop
# reached Succeeded while the deliver Job was still running; a staged failure
# lands it terminal before the in-run window, so it is optional (the failed
# Job's logs are in logs-deliver-failed.out, asserted above).
if [ "$deliver_done" = true ]; then
  test -s "$EVIDENCE/logs-deliver.out" || fail "evidence logs-deliver.out is empty"
fi
log "component log + plan + failed-deliver evidence captured (non-empty)"

# --- redeliver (I52: re-trigger the delivery) -------------------------------
# The first delivery is FAILED (asserted above). `cox redeliver` sets the
# coxswain.io/redeliver annotation; the operator deletes the failed Job,
# re-creates a fresh one for the SAME pinned verifiedCommit, and removes the
# annotation. The stale branch (the staged conflicting history) must be
# deleted BEFORE the re-delivery so the fresh push is clean and Delivered ends
# True. The observable, on its REAL path: a NEW deliver Job UID (#2 != #1) and
# a final Delivered=True. A successful-first-delivery run can never reach here
# (the first-delivery assert above fails first), so this step is never skipped.
JOB_UID_BEFORE="$(deliver_job_uid)"        # deliver Job UID #1 (the FAILED Job)
DELIVERED_BEFORE="$(delivered_condition)"
log "deliver Job UID #1 (before redeliver): ${JOB_UID_BEFORE:-<none>}" | tee "$EVIDENCE/redeliver-uid-before.txt"
log "Delivered condition before redeliver: ${DELIVERED_BEFORE:-<none>}" | tee "$EVIDENCE/delivered-before.txt"
# The first deliver Job must exist (it failed, UID #1 recorded). Without it
# there is no re-delivery to prove.
[ -n "$JOB_UID_BEFORE" ] || fail "no failed deliver Job (UID #1) to redeliver (the first delivery did not run)"

# cox redeliver sets the coxswain.io/redeliver annotation (I52). The command's
# stdout confirms the annotation write; `set -e` aborts on failure, so reaching
# here means the annotation was set.
cox redeliver "$LOOP_NAME" | tee "$EVIDENCE/redeliver.out"
log "cox redeliver succeeded (the command set the annotation; see redeliver.out)"

# The operator deletes the FAILED Job (UID #1) and, on a later pass, creates a
# fresh one (UID #2). We only need UID #1 to be gone; we do NOT require the
# Job to be ABSENT in a visible window (the operator can create the fresh Job
# before our first poll sees the delete — that is the success we want, a NEW
# Job). So: wait until UID #1 is no longer present (absent, or a new UID), then
# proceed; the fresh Job (UID #2 != #1) is what the later assertion records.
for i in $(seq 1 75); do
  uid_now="$(deliver_job_uid)"
  # UID #1 gone when the Job is absent or is a NEW Job (a different UID).
  if [ -z "$uid_now" ] || [ "$uid_now" != "$JOB_UID_BEFORE" ]; then break; fi
  sleep 4
done
uid_now="$(deliver_job_uid)"
{ [ -z "$uid_now" ] || [ "$uid_now" != "$JOB_UID_BEFORE" ]; } \
  || fail "the failed deliver Job (UID #1 $JOB_UID_BEFORE) was not deleted by the operator after redeliver (Job still present: $uid_now)"
log "operator deleted the failed deliver Job (UID #1 gone; the re-delivery Job is ${uid_now:-<not created yet>})"

# Delete the stale staged branch so the fresh deliver push is clean (otherwise
# the re-delivery hits the same non-fast-forward rejection and Delivered would
# stay False). Uses the same in-cluster Gitea + samples credentials as the
# staging.
gitea_port_forward
delete_delivery_branch "$DELIVER_BRANCH"
gitea_stop
log "stale delivery branch $DELIVER_BRANCH deleted (the re-delivery push is now clean)"

# Wait for the fresh deliver Job (UID #2, != #1) to be created AND for the
# re-delivery to SUCCEED (Delivered=True). The fresh Job runs the same push
# (now against a clean branch) and records the delivery.
JOB_UID_AFTER=""
for i in $(seq 1 75); do
  JOB_UID_AFTER="$(deliver_job_uid)"
  DELIVERED_FINAL="$(delivered_condition)"
  # Success: a NEW Job (UID #2 != #1) exists and the delivery is True.
  if [ -n "$JOB_UID_AFTER" ] && [ "$JOB_UID_AFTER" != "$JOB_UID_BEFORE" ]; then
    case "$DELIVERED_FINAL" in True*) break ;; esac
  fi
  sleep 4
done
JOB_UID_AFTER="$(deliver_job_uid)"
DELIVERED_FINAL="$(delivered_condition)"
log "deliver Job UID #2 (after redeliver): ${JOB_UID_AFTER:-<none>}" | tee "$EVIDENCE/redeliver-uid-after.txt"
log "Delivered condition after redeliver: ${DELIVERED_FINAL:-<none>}" | tee "$EVIDENCE/delivered-final.txt"
# Capture the fresh deliver Job's push logs (the successful re-delivery).
log "capturing the re-delivery deliver Job's push logs (the successful push)"
deliver_push_logs | tee "$EVIDENCE/logs-deliver-redeliver.out" || true
# cox describe / history for the final Loop state (the Delivered condition is in
# describe's conditions block).
cox describe "$LOOP_NAME" | tee "$EVIDENCE/describe-after-redeliver.out"

# --- assert the redeliver's REAL path ----------------------------------------
# (1) a NEW deliver Job UID (#2 != #1): the operator deleted the failed Job and
#     re-created a fresh one for the same pinned verifiedCommit.
[ -n "$JOB_UID_AFTER" ] || fail "no fresh deliver Job (UID #2) after redeliver (the re-delivery did not create a Job)"
[ "$JOB_UID_AFTER" != "$JOB_UID_BEFORE" ] \
  || fail "the deliver Job UID did not change (UID #1 = UID #2 = $JOB_UID_BEFORE): the operator did not re-create the Job — the re-delivery path was not proven"
log "redeliver re-created the deliver Job (UID #1 $JOB_UID_BEFORE -> UID #2 $JOB_UID_AFTER)"
# (2) the re-delivery SUCCEEDED: the final Delivered condition is True (the
#     stale branch was deleted, so the fresh push is clean and the delivery is
#     recorded).
case "$DELIVERED_FINAL" in
  True*) log "re-delivery succeeded (Delivered=$DELIVERED_FINAL)" ;;
  *) fail "the re-delivery did not end Delivered=True (got: '$DELIVERED_FINAL') — the redeliver path was not proven" ;;
esac
log "redeliver proven: a failed first delivery (UID #1) was re-delivered (UID #2) and ended Delivered=True"

# --- evidence index ----------------------------------------------------------
{
  echo "A3 CLI e2e: Loop $LOOP_NAME"
  echo "operator image: $(kube get deployment coxswain-controller-manager -o jsonpath='{.spec.template.spec.containers[0].image}' 2>/dev/null || echo unknown)"
  echo "cox binary: $COX (built from $(git -C "$ROOT" rev-parse --short HEAD))"
  echo "final phase: $(phase)"
  echo "final Delivered condition: $(delivered_condition)"
  echo "deliver Job UID #1 (failed, before redeliver): $JOB_UID_BEFORE"
  echo "deliver Job UID #2 (fresh, after redeliver): $JOB_UID_AFTER"
  echo
  for f in "$EVIDENCE"/*.out; do
    echo "=== $(basename "$f") ==="
    cat "$f"
    echo
  done
} >"$EVIDENCE/index.txt" 2>&1 || true
log "evidence -> $EVIDENCE"

# --- cleanup (the run's own Loop + the staged/re-delivered Gitea branch) ------
# The EXIT trap already deletes the Loop and stops the port-forward if the run
# aborted mid-run; on success we clean up explicitly (and idempotently) so the
# cluster and the Gitea repo are left as we found them. The re-delivery left an
# open PR on the (now-deleted) branch; the branch itself is the deliverable the
# run created and is removed here.
gitea_port_forward
delete_delivery_branch "$DELIVER_BRANCH"
gitea_stop
kube delete loop "$LOOP_NAME" --wait=true --timeout=120s >/dev/null 2>&1 || kube delete loop "$LOOP_NAME" --wait=false >/dev/null 2>&1 || true
log "cleaned up: Loop $LOOP_NAME deleted; delivery branch $DELIVER_BRANCH removed from the Gitea repo"
log "PASS: A3 CLI lifecycle (pause -> resume -> describe -> redeliver) completed — redeliver proven on its real path (failed UID #1 -> fresh UID #2, Delivered=True)"
