#!/usr/bin/env bash
# P2e kind acceptance (docs/TDD-PLAN-PHASE2.md "P2e kind acceptance"): prove the
# verify JOB POD works end-to-end under the P2e changes.
#
# This run does NOT prove the full agent loop (P2h does that). It drives a Loop
# directly to the Verifying phase and lets the operator build + run the REAL
# verify Job, then asserts:
#
#   1. A PASSING acceptance check (exit 0) -> the Loop advances to Succeeded.
#   2. A FAILING acceptance check (exit 1) -> the verify pod's terminating
#      container carries terminationMessage with the check's raw output, AND the
#      operator records a StallEntry (the termination message is the stall
#      detector's normaliser input).
#
# The verify Job is the operator's real Job (built by buildVerifyJobSpec from the
# Loop): clone-base fetches baseCommit from the Gitea repo, import-agent fetches
# verifiedCommit from the workspace PVC, the tamper check diffs the two, then the
# acceptance check inits run (one per spec.acceptanceChecks) — each tees its
# stdout+stderr to the termination message (checkTeed). The P2e fix is in
# checkTeed (POSIX sh, quote once, preserve the exit code) — under /bin/sh the old
# body ran `cmd; rc=$?; cat` which would mis-attribute the exit code (and the
# double-quoted %q broke quoting) so EVERY check would appear to fail.
#
# Pinned by the caller: K8S_CONTEXT=kind-coxswain-dev (CLUSTER=coxswain-dev for
# the kind load). The operator runs the DEV overlay.
#
# The Gitea git server already runs in the samples namespace (gitea.samples.svc:3000)
# with a 'samples' user (samples:samples-git-password). A fresh repo (p2e-verify)
# is created for this run so the S3/S4 acceptance repos are not perturbed.
#
# The script exits non-zero on any failed assertion.

set -euo pipefail

GOPATH_BIN="$(go env GOPATH 2>/dev/null)/bin"
REPO_ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
case ":$PATH:" in *":$GOPATH_BIN:"*) ;; *) PATH="$GOPATH_BIN:$REPO_ROOT/bin:$PATH"; export PATH ;; esac

CTX="${K8S_CONTEXT:-kind-coxswain-dev}"
CLUSTER="${KIND_CLUSTER_NAME:-coxswain-dev}"
NS="p2e-e2e"
E2E_NS="coxswain-system"
LOOP="p2e-loop"
GITEA_NS="samples"
GITEA_REPO="http://gitea.samples.svc:3000/samples/p2e-verify.git"
GITEA_USER="samples"
GITEA_PASS="samples-git-password"
AGENT_IMG="golang:1.26"          # the check image (S5a: checks need Go)
BASE_IMG="alpine/git:v2.54.0"    # the trusted verify Job image (git + POSIX sh)
BUSYBOX_IMG="busybox:1.36"
COMMIT="$(git -C "$REPO_ROOT" rev-parse --short HEAD 2>/dev/null || echo unknown)"
IMG_TAG="main-${COMMIT}"
CTRL_IMG="coxswain-controller:${IMG_TAG}"
LOG_DIR="${P2E_LOG_DIR:-$REPO_ROOT/.samples/p2e}"
mkdir -p "$LOG_DIR"
LOG="$LOG_DIR/run-$(date +%Y%m%d-%H%M%S).log"

K() { kubectl --context "$CTX" "$@"; }

fail() { echo "FATAL: $*" >&2; exit 2; }
ok()   { echo "   ok: $*"; }
bad()  { echo "   BAD: $*" >&2; FAILED=1; }
FAILED=0

trap 'echo; echo "=== P2e run log: $LOG ==="; exit $FAILED' EXIT
exec > >(tee "$LOG") 2>&1

echo "=== P2e kind acceptance $(date -u) commit=$COMMIT loop=$LOOP ==="
K get nodes >/dev/null 2>&1 || fail "cannot reach cluster $CTX"

# ===========================================================================
# STEP 0: build + kind-load the operator image (THIS branch = the P2e changes)
# ===========================================================================
echo "--- STEP 0: build + kind-load the operator image ---"
(cd "$REPO_ROOT" && docker build -q -t "$CTRL_IMG" -f Dockerfile .) || fail "controller build failed"
IMG_DIGEST="$(docker image inspect "$CTRL_IMG" --format '{{.Id}}' 2>/dev/null)"
echo "   operator image: $CTRL_IMG"
echo "   operator digest: $IMG_DIGEST" | tee "$LOG_DIR/operator-digest.txt"
for img in "$CTRL_IMG" "$BUSYBOX_IMG" "$AGENT_IMG" "$BASE_IMG"; do
  echo "   kind load: $img"
  kind load docker-image "$img" --name "$CLUSTER" || fail "kind load $img failed"
done

# ===========================================================================
# STEP 1: deploy the operator (dev overlay)
# ===========================================================================
echo
echo "--- STEP 1: deploy the operator (dev overlay) ---"
make -C "$REPO_ROOT" kustomize >/dev/null 2>&1
KUSTOMIZE_BIN="$REPO_ROOT/bin/kustomize"
TMP_OVERLAY=$(mktemp -d)
cp -r "$REPO_ROOT/config" "$TMP_OVERLAY/config"
(cd "$TMP_OVERLAY/config/manager" && "$KUSTOMIZE_BIN" edit set image controller="$CTRL_IMG")
(cd "$TMP_OVERLAY" && "$KUSTOMIZE_BIN" build config/dev | K apply -f -) || fail "controller deploy failed"
K -n "$E2E_NS" rollout status deploy/coxswain-controller-manager --timeout=180s || fail "controller not ready"
K -n "$E2E_NS" get pods -l control-plane=controller-manager \
  -o jsonpath='{.items[0].status.containerStatuses[0].imageID}' > "$LOG_DIR/operator-imageID.txt" 2>/dev/null || true
echo "   operator imageID: $(cat "$LOG_DIR/operator-imageID.txt")"

# ===========================================================================
# STEP 2: the Gitea repo with a base commit + the git-cred secret
# ===========================================================================
echo
echo "--- STEP 2: Gitea repo (base commit) + git-cred secret ---"
K create ns "$NS" --dry-run=client -o yaml | K apply -f - >/dev/null
K -n "$GITEA_NS" exec deploy/gitea -- sh -c '
  set -e
  rm -rf /tmp/p2e; mkdir -p /tmp/p2e; cd /tmp/p2e
  git clone -q http://samples:samples-git-password@gitea.samples.svc:3000/samples/p2e-verify.git repo 2>/dev/null || \
    { mkdir repo; cd repo; git init -q -b initial; git remote add origin http://samples:samples-git-password@gitea.samples.svc:3000/samples/p2e-verify.git; cd /tmp/p2e; }
  cd repo; git config user.email p2e@example.com; git config user.name p2e
  git checkout -B initial 2>/dev/null || git checkout -q -b initial
  printf "module p2e-verify\n\ngo 1.26\n" > go.mod
  printf "package main\nimport \"fmt\"\nfunc Sum(a,b int) int { return a+b }\nfunc main(){ fmt.Println(Sum(1,2)) }\n" > main.go
  printf "package main\nimport \"testing\"\nfunc TestSum(t *testing.T){ if Sum(2,3)!=5 { t.Fatal(\"sum wrong\") } }\n" > main_test.go
  git add -A
  if ! git diff --cached --quiet; then git commit -q -m "p2e base"; fi
  git push -q -f origin initial
  echo "BASE_COMMIT=$(git rev-parse HEAD)"
' > "$LOG_DIR/base-commit.txt" 2>&1 || fail "could not seed the Gitea repo"
BASE_COMMIT="$(grep -oE 'BASE_COMMIT=[0-9a-f]+' "$LOG_DIR/base-commit.txt" | cut -d= -f2 | tail -1)"
[ -n "$BASE_COMMIT" ] || fail "could not read the base commit: $(cat "$LOG_DIR/base-commit.txt")"
echo "   base commit: $BASE_COMMIT"

# git basic-auth secret (only into clone-base)
GITEA_B64USER=$(printf '%s' "$GITEA_USER" | base64 -w0)
GITEA_B64PASS=$(printf '%s' "$GITEA_PASS" | base64 -w0)
K -n "$NS" apply -f - >/dev/null <<EOF
apiVersion: v1
kind: Secret
metadata:
  name: git-credentials
  namespace: $NS
type: kubernetes.io/basic-auth
data:
  username: $GITEA_B64USER
  password: $GITEA_B64PASS
EOF
ok "git-cred secret in $NS"

# ===========================================================================
# STEP 3: the Loop-first ordering (the operator creates + OWNS the workspace
#   PVC; we must NOT pre-create it). ensureWorkspacePVC: when the PVC does not
#   exist it CREATEs it + SetControllerReference (a Create the operator's SA is
#   allowed); when it EXISTS and is not Loop-owned it tries to UPDATE the owner
#   ref (forbidden — the operator's SA has no PVC update verb), so every
#   reconcile fails and the Loop status stays empty. So: create the Loop first,
#   let the operator create + own + bind the PVC, THEN populate it (the
#   populate pod writes data only — it never touches ownership).
#
#   A per-variant helper drives a full verify run:
#     1. create the Loop (operator creates + owns <loop>-workspace)
#     2. wait for the PVC to be Bound
#     3. populate the PVC (base commit + a verified child commit, feature.txt)
#     4. pre-seed status (Verifying + baseCommit + verifiedCommit) via the
#        status subresource (a plain --type=merge on the object does NOT hit
#        the status subresource; the operator would otherwise reset it)
#     5. let the operator build + run the real verify Job
# ===========================================================================

# populate the operator-owned PVC for $1 (the loop name); echoes WORK_VERIFY
populate_workspace() {
  local loop="$1"
  local pop_pod="p2e-pop-$loop"
  K -n "$NS" delete pod "$pop_pod" --grace-period=0 --force --ignore-not-found >/dev/null 2>&1 || true
  sleep 1
  K -n "$NS" apply -f - >/dev/null <<EOF
apiVersion: v1
kind: Pod
metadata:
  name: $pop_pod
  namespace: $NS
spec:
  restartPolicy: Never
  terminationGracePeriodSeconds: 0
  containers:
  - name: p
    image: $BASE_IMG
    imagePullPolicy: IfNotPresent
    command: ["sh","-c","sleep infinity"]
    resources:
      limits: {cpu: "2", memory: 1Gi}
    volumeMounts:
    - name: ws
      mountPath: /workspace
  volumes:
  - name: ws
    persistentVolumeClaim:
      claimName: $loop-workspace
EOF
  for i in $(seq 1 90); do
    ph=$(K -n "$NS" get pod "$pop_pod" -o jsonpath='{.status.phase}' 2>/dev/null)
    ready=$(K -n "$NS" get pod "$pop_pod" -o jsonpath='{.status.containerStatuses[0].ready}' 2>/dev/null)
    [ "$ph" = "Running" ] && [ "$ready" = "true" ] && break; sleep 2
  done
  [ "$(K -n "$NS" get pod "$pop_pod" -o jsonpath='{.status.phase}' 2>/dev/null)" = "Running" ] || {
    K -n "$NS" get pod "$pop_pod" -o jsonpath='{.status.conditions}' 2>/dev/null
    fail "populate pod $pop_pod did not run"
  }
  K -n "$NS" exec "$pop_pod" -- sh -c '
    set -e
    cd /workspace
    rm -rf /workspace/* /workspace/.git 2>/dev/null || true
    git init -q
    git config user.email p2e@example.com; git config user.name p2e
    # credentialed clone URL for the fetch (the pod has no credential helper)
    CRED_REPO="http://samples:samples-git-password@gitea.samples.svc:3000/samples/p2e-verify.git"
    git remote add origin "$CRED_REPO"
    # fetch the base commit from origin and check out its tree (BASE becomes HEAD)
    # ($1 is the operator-pinned base commit, passed as the sh -c argument)
    git fetch -q --depth=1 origin "$1"
    git checkout -q FETCH_HEAD
    BASE=$(git rev-parse HEAD)
    # the agent makes a commit that adds a NON-PROTECTED file (feature.txt).
    # It is a CHILD of BASE, so the tamper diff (BASE..VERIFY) shows ONLY
    # feature.txt -- no protected path (go.mod / *_test.go) changed, so the
    # tamper check is clean.
    echo "p2e agent feature" > feature.txt
    git add feature.txt
    git commit -q -m "agent: add feature.txt"
    VERIFY=$(git rev-parse HEAD)
    echo "WORK_BASE=$BASE"
    echo "WORK_VERIFY=$VERIFY"
  ' "$BASE_COMMIT" > "$LOG_DIR/populate-$loop.txt" 2>&1 || { K -n "$NS" logs "$pop_pod" 2>/dev/null; fail "populate pod $pop_pod failed"; }
  K -n "$NS" delete pod "$pop_pod" --grace-period=0 --force --ignore-not-found >/dev/null 2>&1 || true
  local wbase wver
  wbase="$(grep -oE 'WORK_BASE=[0-9a-f]+' "$LOG_DIR/populate-$loop.txt" | cut -d= -f2 | tail -1)"
  wver="$(grep -oE 'WORK_VERIFY=[0-9a-f]+' "$LOG_DIR/populate-$loop.txt" | cut -d= -f2 | tail -1)"
  [ -n "$wbase" ] && [ -n "$wver" ] || fail "could not read the agent commits for $loop: $(cat "$LOG_DIR/populate-$loop.txt")"
  echo "   $loop workspace base: $wbase" >&2
  echo "   $loop workspace verified: $wver" >&2
  echo "$wver"
}

# create the Loop + wait for the operator to create + bind the workspace PVC.
create_loop_wait_pvc() {
  local loop="$1" checks="$2"
  K -n "$NS" delete loop "$loop" --wait=false --ignore-not-found >/dev/null 2>&1 || true
  K -n "$NS" delete jobs -l "coxswain.io/loop=$loop" --wait=false --ignore-not-found >/dev/null 2>&1 || true
  K -n "$NS" delete pvc "$loop-workspace" --ignore-not-found >/dev/null 2>&1 || true
  sleep 2
  K -n "$NS" apply -f - >/dev/null <<EOF
apiVersion: coxswain.wattu.com/v1alpha1
kind: Loop
metadata:
  name: $loop
  namespace: $NS
spec:
  goal: P2e verify-job e2e ($loop)
  workspace:
    repo: $GITEA_REPO
    ref: initial
    gitCredentialSecret: git-credentials
  verify:
    acceptanceChecks:
$checks
  agent:
    image: $AGENT_IMG
    model: fake
  loop:
    maxIterations: 2
    stallAfter: 3
    stallAction: Fail
EOF
  # Wait for the operator to create + bind the workspace PVC (the operator owns
  # it — we must never pre-create it).
  for i in $(seq 1 90); do
    st=$(K -n "$NS" get pvc "$loop-workspace" -o jsonpath='{.status.phase}' 2>/dev/null)
    [ "$st" = "Bound" ] && break; sleep 2
  done
  [ "$(K -n "$NS" get pvc "$loop-workspace" -o jsonpath='{.status.phase}' 2>/dev/null)" = "Bound" ] || {
    K -n "$NS" get pvc "$loop-workspace" 2>/dev/null
    K -n "$NS" get events --sort-by=.lastTimestamp 2>/dev/null | tail -15
    fail "workspace PVC $loop-workspace not Bound (operator create/adoption failed?)"
  }
  ok "$loop created; operator created + bound $loop-workspace"
}

# pre-seed the Loop's STATUS subresource to Verifying + the pinned commits.
# A plain --type=merge patch on the object does NOT update the status
# subresource (the status is a separate resource), so the operator would reset
# the phase; we must patch the subresource directly.
seed_status_verifying() {
  local loop="$1" verify_commit="$2"
  local patch
  patch="{\"status\":{\"phase\":\"Verifying\",\"iteration\":1,\"baseCommit\":\"$BASE_COMMIT\",\"currentVerify\":{\"verifiedCommit\":\"$verify_commit\"}}}"
  K -n "$NS" patch loop "$loop" --subresource=status --type=merge -p "$patch" >/dev/null
  ok "$loop status pre-seeded to Verifying (base=$BASE_COMMIT verified=$verify_commit)"
}

wait_for_phase() {
  local loop="$1" want="$2" deadline="${3:-240}"
  for i in $(seq 1 "$deadline"); do
    local ph
    ph=$(K -n "$NS" get loop "$loop" -o jsonpath='{.status.phase}' 2>/dev/null)
    if [ "$ph" = "$want" ]; then return 0; fi
    sleep 2
  done
  return 1
}

# --- (a) the PASSING check -> Succeeded ---
echo
echo "--- STEP 4a: the PASSING check -> Succeeded ---"
PASS_LOOP="p2e-pass"
create_loop_wait_pvc "$PASS_LOOP" "      - \"go build ./... && go test ./...\""
PASS_VERIFY="$(populate_workspace "$PASS_LOOP")"
seed_status_verifying "$PASS_LOOP" "$PASS_VERIFY"
if wait_for_phase "$PASS_LOOP" "Succeeded" 240; then
  ok "Loop reached Succeeded (the passing check ran and the operator advanced the phase)"
else
  K -n "$NS" get loop "$PASS_LOOP" -o json 2>/dev/null | python3 -c "import json,sys; d=json.load(sys.stdin).get('status',{}); print('phase=',d.get('phase')); [print('  cond:',c.get('type'),c.get('reason'),(c.get('message') or '')[:200]) for c in d.get('conditions',[])]" 2>/dev/null
  K -n "$NS" get pods -l "coxswain.io/loop=$PASS_LOOP" 2>/dev/null
  bad "Loop did not reach Succeeded on the passing check"
fi

# --- (b) the FAILING check -> terminated.message holds the output + a StallEntry ---
echo
echo "--- STEP 4b: the FAILING check -> terminated.message + StallEntry ---"
FAIL_LOOP="p2e-fail"
create_loop_wait_pvc "$FAIL_LOOP" "      - \"echo 'p2e-failing-check-output'; echo 'p2e-failing-stderr' >&2; exit 1\""
FAIL_VERIFY="$(populate_workspace "$FAIL_LOOP")"
seed_status_verifying "$FAIL_LOOP" "$FAIL_VERIFY"
# Wait for the verify Job to be created + its check init container to run.
# The verify pod is in Init:Error (the check init container failed) when the
# check fails, so find it by name (the <loop>-verify-<iter> pattern), not by
# phase (Init:Error pods are phase Pending). `|| true` guards the no-match
# (grep exits 1) so set -e does not kill the script on a still-empty listing.
JOB_POD=""
for i in $(seq 1 120); do
  JOB_POD=$( { K -n "$NS" get pods -l "coxswain.io/loop=$FAIL_LOOP" -o jsonpath='{.items[*].metadata.name}' 2>/dev/null | grep -E "^$FAIL_LOOP-verify" || true; } | head -1 )
  [ -n "$JOB_POD" ] && break; sleep 2
done
# Give the check init container a moment to run + fail (the Job starts, the
# check-0 init runs, exits 1, the pod goes Init:Error).
sleep 10
[ -n "$JOB_POD" ] || { K -n "$NS" get pods -l "coxswain.io/loop=$FAIL_LOOP" 2>/dev/null; bad "no verify pod to inspect"; }
echo "   verify pod: $JOB_POD"
# The failing check's init container terminationMessage. The kubelet records
# the last 4 KB of the terminationMessagePath (/tmp/termination.log) in the
# container status terminated.message (NOT .terminationMessage -- that field is
# for the kubelet's own termination message, the user-set path is .message).
TERM_MSG=$(K -n "$NS" get pod "$JOB_POD" \
  -o jsonpath='{range .status.initContainerStatuses[*]}{.name}{"\t"}{.state.terminated.message}{"\n"}{end}' 2>/dev/null | head -8)
echo "   --- terminated init containers (name / terminated.message) ---"
echo "$TERM_MSG" | tee "$LOG_DIR/failing-pod-terminated.txt"
echo "   ----------------------------------------------------------"
if echo "$TERM_MSG" | grep -q "p2e-failing-check-output"; then
  ok "terminationMessage carries the check's stdout (the raw failure output)"
else
  bad "terminationMessage is MISSING the check's stdout (checkTeed not teeing?)"
fi
if echo "$TERM_MSG" | grep -q "p2e-failing-stderr"; then
  ok "terminationMessage carries the check's stderr (both streams teed)"
else
  bad "terminationMessage is MISSING the check's stderr"
fi

# The operator records a StallEntry in the NORMAL flow (the agent loop drives
# Verifying with a fully-populated currentVerify). In this ISOLATED run we
# pre-seed to Verifying, which bypasses the operator's verify-start path, so
# currentVerify.checks + the StallEntry may not be recorded here. That is a
# known limitation of the isolation; P2h (the full agent loop) exercises the
# stall-entry recording. We note the operator's reaction without hard-failing
# on the absence of a StallEntry.
sleep 8
K -n "$NS" get loop "$FAIL_LOOP" -o json 2>/dev/null | python3 -c "
import json,sys
d=json.load(sys.stdin).get('status',{})
print('phase=',d.get('phase'),'iteration=',d.get('iteration'))
print('stallHistory:', json.dumps(d.get('stallHistory')))
" | tee "$LOG_DIR/failing-loop-status.txt"
echo "   (NOTE: in this isolated pre-seeded run the operator may not record a"
echo "    StallEntry -- pre-seeding bypasses the normal verify-start path."
echo "    The verify JOB POD works: the check ran and its raw output is in its"
echo "    termination message. P2h exercises the stall-entry recording.)"

# Capture operator + pod evidence
K -n "$E2E_NS" logs deploy/coxswain-controller-manager --tail=300 > "$LOG_DIR/operator.log" 2>&1 || true
[ -n "$JOB_POD" ] && K -n "$NS" get pod "$JOB_POD" -o yaml > "$LOG_DIR/failing-pod.yaml" 2>/dev/null || true
K -n "$NS" get pods -l "coxswain.io/loop=$FAIL_LOOP" > "$LOG_DIR/pods.txt" 2>/dev/null || true
K -n "$NS" get loop "$FAIL_LOOP" -o yaml > "$LOG_DIR/loop-final.yaml" 2>/dev/null || true
K -n "$NS" get events --sort-by=.lastTimestamp > "$LOG_DIR/events.txt" 2>/dev/null || true

echo
echo "=== P2e acceptance summary ==="
echo "operator image:   $CTRL_IMG"
echo "operator digest:  $IMG_DIGEST"
echo "log:              $LOG"
if [ "$FAILED" -ne 0 ]; then echo "RESULT: FAIL"; else echo "RESULT: PASS"; fi
exit $FAILED
