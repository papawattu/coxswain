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
# Tag the build UNIQUELY (main-<shortsha>-<timestamp>): the build includes
# UNCOMMITTED working-tree changes (the fix is uncommitted until the very end),
# so the digest must be read from the BUILT image and the running pod verified
# against it. A unique tag avoids a stale cached image masquerading as the new
# build (the run-064348 root cause: the operator kept running main-519b6ff from
# a previous build because the Deployment was never actually rolled to the new
# image).
TIMESTAMP=$(date -u +%Y%m%d%H%M%S)
CTRL_IMG="coxswain-controller:main-${COMMIT:0:7}-${TIMESTAMP}"
echo "--- STEP 0: build + kind-load the operator image ---"
(cd "$REPO_ROOT" && docker build -q -t "$CTRL_IMG" -f Dockerfile .) || fail "controller build failed"
IMG_DIGEST="$(docker image inspect "$CTRL_IMG" --format '{{.Id}}' 2>/dev/null)"
echo "   operator image: $CTRL_IMG"
echo "   built image digest: $IMG_DIGEST" | tee "$LOG_DIR/built-digest.txt"
for img in "$CTRL_IMG" "$BUSYBOX_IMG" "$AGENT_IMG" "$BASE_IMG"; do
  echo "   kind load: $img"
  kind load docker-image "$img" --name "$CLUSTER" || fail "kind load $img failed"
done

# ===========================================================================
# STEP 1: deploy the operator (dev overlay) + ROLL the Deployment to the new
# image + verify the RUNNING pod's imageID equals the built digest
# ===========================================================================
echo
echo "--- STEP 1: deploy the operator (dev overlay) ---"
make -C "$REPO_ROOT" kustomize >/dev/null 2>&1
KUSTOMIZE_BIN="$REPO_ROOT/bin/kustomize"
TMP_OVERLAY=$(mktemp -d)
cp -r "$REPO_ROOT/config" "$TMP_OVERLAY/config"
(cd "$TMP_OVERLAY/config/manager" && "$KUSTOMIZE_BIN" edit set image controller="$CTRL_IMG")
(cd "$TMP_OVERLAY" && "$KUSTOMIZE_BIN" build config/dev | K apply -f -) || fail "controller deploy failed"
# Explicitly roll the Deployment to the freshly built image (the kustomize
# apply above sets the image, but a stale cached image or a pending old replica
# can leave the running pod on an older build -- `set image` forces the
# update). Then wait for the rollout.
K -n "$E2E_NS" set image deploy/coxswain-controller-manager manager="$CTRL_IMG" >/dev/null || fail "set image failed"
K -n "$E2E_NS" rollout status deploy/coxswain-controller-manager --timeout=180s || fail "controller not ready"
# Read the RUNNING pod's imageID (NOT the local image's digest) and FAIL unless
# it equals the built digest. This is the operator digest that the run actually
# exercised. During the rollout there may be TWO pods matching the label (the
# old one pending termination + the new one) -- poll until a pod whose imageID
# matches the built digest is present (the new pod), rather than reading the
# first pod (which may be the old one). This is the operator digest the run
# actually exercised.
BUILT_SHA="${IMG_DIGEST##*sha256:}"
RUNNING_IMAGEID=""
for i in $(seq 1 60); do
  # Find a pod (new RS) whose imageID matches the built digest.
  RUNNING_IMAGEID="$(K -n "$E2E_NS" get pods -l control-plane=controller-manager -o jsonpath='{range .items[*]}{.status.containerStatuses[0].imageID}{"\n"}{end}' 2>/dev/null || true)"
  RUNNING_IMAGEID=$(echo "$RUNNING_IMAGEID" | grep "sha256:$BUILT_SHA$" | head -1)
  [ -n "$RUNNING_IMAGEID" ] && break
  sleep 2
done
if [ -z "$RUNNING_IMAGEID" ]; then
  K -n "$E2E_NS" get pods -l control-plane=controller-manager -o wide 2>/dev/null | tee -a "$LOG"
  fail "no running pod carries the built digest ($IMG_DIGEST) -- the operator was NOT rolled to this build"
fi
echo "   running pod imageID: $RUNNING_IMAGEID" | tee "$LOG_DIR/operator-digest.txt"
echo "   operator digest (verified against the running pod): $RUNNING_IMAGEID" | tee -a "$LOG_DIR/operator-digest.txt"

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
    ph=$(K -n "$NS" get pod "$pop_pod" -o jsonpath='{.status.phase}' 2>/dev/null || true)
    ready=$(K -n "$NS" get pod "$pop_pod" -o jsonpath='{.status.containerStatuses[0].ready}' 2>/dev/null || true)
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
  # it — we must never pre-create it). `|| true` guards a transient kind-API
  # error inside the loop so set -e keeps polling instead of dying on one
  # flaky get (an empty result is treated as "not Bound yet", keep waiting).
  for i in $(seq 1 90); do
    st=$(K -n "$NS" get pvc "$loop-workspace" -o jsonpath='{.status.phase}' 2>/dev/null || true)
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
    ph=$(K -n "$NS" get loop "$loop" -o jsonpath='{.status.phase}' 2>/dev/null || true)
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
# Dump the verify pod's initContainerStatuses (terminated.message) + the
# Loop's status.stallHistory into the log + evidence file. Called on the
# FAILED path (and at the end) so the evidence SURVIVES even if a later step
# fails -- the reviewer's requirement: the log must show the check's output in
# terminated.message plus a StallEntry with a hash BEFORE any cleanup.
dump_verify_evidence() {
  local loop="$1" pod="$2" tag="${3:-final}"
  local out="$LOG_DIR/evidence-$loop-$tag.txt"
  {
    echo "=== P2e verify evidence ($tag): loop=$loop pod=$pod ==="
    echo "operator digest: $IMG_DIGEST"
    if [ -n "$pod" ]; then
      echo "--- verify pod $pod initContainerStatuses (name / exitCode / terminated.message) ---"
      K -n "$NS" get pod "$pod" -o json 2>/dev/null | python3 -c "
import json,sys
try:
    pod=json.load(sys.stdin)
except Exception as e:
    print('  (no pod JSON:', e, ')'); raise SystemExit
for ic in pod.get('status',{}).get('initContainerStatuses',[]):
    t=ic.get('state',{}).get('terminated',{})
    print('  ', ic.get('name'), '-> exit', t.get('exitCode'), t.get('reason'))
    if t.get('message'):
        print('       terminated.message:', repr(t.get('message')))
" || echo "  (pod $pod gone or unreadable)"
    fi
    echo "--- Loop $loop status (phase/iteration/currentVerify/stallHistory) ---"
    K -n "$NS" get loop "$loop" -o json 2>/dev/null | python3 -c "
import json,sys
try:
    d=json.load(sys.stdin).get('status',{})
except Exception as e:
    print('  (no loop JSON:', e, ')'); raise SystemExit
print('  phase:', d.get('phase'), 'iteration:', d.get('iteration'))
print('  currentVerify:', json.dumps(d.get('currentVerify')))
sh=d.get('stallHistory') or []
print('  stallHistory ('+str(len(sh))+' entries):')
for e in sh:
    print('   ', json.dumps(e))
print('  Stalled condition:', json.dumps([c for c in d.get('conditions',[]) if c.get('type')=='Stalled']))
" || echo "  (loop $loop gone or unreadable)"
    echo "=== end P2e verify evidence ($tag) ==="
  } | tee "$out"
}

# Wait for the verify Job to be created + its check init container to run.
# The verify pod is in Init:Error (the check init container failed) when the
# check fails, so find it by NAME (the <loop>-verify-<iter> pattern), not by
# phase (Init:Error pods are phase Pending). The jsonpath is ONE NAME PER LINE
# ({range .items[*]}{.metadata.name}{"\n"}{end}) so grep -E "^$FAIL_LOOP-verify"
# can match; {.items[*].metadata.name} prints all names on ONE line and the
# anchored grep never matches. `|| true` guards the no-match (grep exits 1) so
# set -e does not kill the script on a still-empty listing.
JOB_POD=""
for i in $(seq 1 120); do
  JOB_POD=$( { K -n "$NS" get pods -l "coxswain.io/loop=$FAIL_LOOP" -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}' 2>/dev/null | grep -E "^$FAIL_LOOP-verify" || true; } | head -1 )
  [ -n "$JOB_POD" ] && break; sleep 2
done
# Give the check init container a moment to run + fail (the Job starts, the
# check-0 init runs, exits 1, the pod goes Init:Error).
sleep 10
# Give the operator a moment to read the verify outcome + append the StallEntry.
sleep 10
if [ -z "$JOB_POD" ]; then
  dump_verify_evidence "$FAIL_LOOP" "" "failed-no-pod"
  bad "no verify pod to inspect"
fi
echo "   verify pod: $JOB_POD"
# The failing check's init container terminationMessage. The kubelet records
# the last 4 KB of the terminationMessagePath (/tmp/termination.log) in the
# container status terminated.message (NOT .terminationMessage -- that field is
# for the kubelet's own termination message, the user-set path is .message).
TERM_MSG=$(K -n "$NS" get pod "$JOB_POD" \
  -o jsonpath='{range .status.initContainerStatuses[*]}{.name}{"\t"}{.state.terminated.message}{"\n"}{end}' 2>/dev/null | head -8 || true)
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

# The operator reads the verify outcome (applyVerifyOutcome -> verifyOutcome ->
# verifyIterate) and applyStallGate APPENDS a StallEntry on every TERMINAL
# verify failure (a check-* container Terminated non-zero) -- the entry is
# appended BEFORE the stallDecision fire check, so a single failure yields ONE
# entry (with a hash) even when the stall does not yet fire (stallAfter=3).
# The check's raw output (the termination message) is the normaliser input; the
# entry's hash is the SHA-256 of the normalised output.
# The operator's reconcile that reads the terminated check-0 + appends the
# entry + persists the status happens asynchronously AFTER the check fails,
# so POLL for the entry (the operator requeues on the verify Job's status
# change and re-reconciles within seconds). A single read races that reconcile.
STALL_JSON=""
for i in $(seq 1 30); do
  STALL_JSON="$(K -n "$NS" get loop "$FAIL_LOOP" -o jsonpath='{.status.stallHistory}' 2>/dev/null || true)"
  # Present = a non-empty, non-null JSON array.
  if [ -n "$STALL_JSON" ] && [ "$STALL_JSON" != "" ] && [ "$STALL_JSON" != "null" ]; then
    break
  fi
  sleep 2
done
if [ -n "$STALL_JSON" ] && [ "$STALL_JSON" != "" ] && [ "$STALL_JSON" != "null" ]; then
  # A StallEntry with a 64-hex hash (the SHA-256 of the normalised check output).
  if echo "$STALL_JSON" | grep -qE '"hash"[[:space:]]*:[[:space:]]*"[0-9a-f]{64}"'; then
    ok "a StallEntry with a hash was recorded (the check's output reached the stall detector)"
  else
    bad "the StallEntry has NO 64-hex hash: $STALL_JSON"
  fi
else
  bad "NO StallEntry recorded on the failing check (expected one on a terminal verify failure)"
fi
echo "   stallHistory: $STALL_JSON"

# Dump the full verify evidence (the check's terminated.message + the Loop's
# stallHistory) to the log + evidence file BEFORE any cleanup, so it survives
# even if a later step fails. This is the reviewer's required evidence.
dump_verify_evidence "$FAIL_LOOP" "$JOB_POD" "final"

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
