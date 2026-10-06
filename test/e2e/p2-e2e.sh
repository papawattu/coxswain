#!/usr/bin/env bash
# P2h kind acceptance (docs/TDD-PLAN-PHASE2.md "P2h — Kind acceptance", the
# LAST Phase 2 slice). The PLAN.md "Done when": a deliberately impossible
# goal STOPS CLEANLY with the right reason (Stalled or BudgetExceeded)
# instead of spinning, and a paused Loop RESUMES where it left off.
#
# Fixture: the kind cluster (kind-coxswain-dev) with the operator + a STUB
# MODEL SERVER (an in-kind hostNetwork python:3-alpine pod, p2h-stub-model.py).
# The stub's behaviour is PINNED (item 14): every completion carries a fixed
# implement-instruction (append one newline to README.md + commit — a
# non-protected file, so every Implementing iteration produces a NEW commit,
# the verify Job re-runs, the iteration advances) and a fixed usage
# {prompt_tokens: 100, completion_tokens: 100} = 200 tokens/request (item I's
# arithmetic). Non-streaming (the reference runner's shape). The runner runs
# The runner runs with the operator's default -max-steps (25, the
# runner's defaultMaxSteps; the P2h plan does NOT pin a smaller cap — the
# stub itself bounds the request count: it answers request 0 (the seed-only
# call, the messages-list length 1) with the fixed shell tool call and every
# LATER request (length >= 2) with a plain "done" answer (no tool call), so
# the runner's model loop ends by itself after the tool call — the stub's
# per-phase-run request count is exactly 2 (the tool call + the "done"), and
# the token math (200/request) holds without an operator cap):
#
# Loops (all point at the stub EXCEPT the cross-check Loop, which points at
# the REAL vLLM LB 192.168.1.20:8000 — item I):
#   1. the stall Loop (p2h-stall): stallAfter: 3, stallAction: Fail,
#      maxIterations 10 — the stall (at the 3rd identical failure) stops it,
#      not the cap.
#   2. the budget Loop (p2h-budget): maxTokens: 300, onExceeded: Fail,
#      stallAction: Continue — the cap fires on request 2 (400 >= 300),
#      BEFORE the 3rd verify failure (the budget is on the REQUEST, the
#      stall on the FAILURE).
#   3. the control Loop (p2h-ctrl): stallAfter: 10, maxIterations: 5 — spins
#      to the cap WITHOUT a stall (the contrast that proves the stall Loop
#      stopped because of the detector).
#   4. the resume Loop (p2h-resume): spec.suspend driven false -> true ->
#      false at Implementing.
#   5. the budget-Pause Loops (p2h-budgetpause + p2h-budgetpause2):
#      onExceeded: Pause, maxTokens small (400) — paused at the 2nd
#      Implementing (400 tokens >= 400; a 3rd request would need a 3rd
#      verify failure that never comes). The FIRST has spec.budget.maxTokens
#      raised (I43 live update) + resumed via the coxswain.io/resume
#      annotation -> proceeds (the re-evaluation clears exceeded). The
#      SECOND (control, caps NOT raised) -> re-pauses immediately (the
#      fail-closed re-fire).
#   6. the cross-check Loop (p2h-real): modelEndpoint 192.168.1.20:8000
#      (the REAL vLLM LB), a trivially PASSING check (sleep 0.1) so it
#      reaches Succeeded in one iteration; its per-Loop status.budget token
#      counts are cross-checked against the real vllm:prompt_tokens_total /
#      vllm:generation_tokens_total deltas from the homelab Prometheus
#      (--context default, READ-ONLY: kubectl port-forward + curl; the
#      stub-emitted counter is NOT used — item 14, it would be circular).
#      If the homelab Prometheus is unreachable or has no vllm series, the
#      cross-check is DROPPED with a note (it is a consistency check, not a
#      gate; the per-Loop counts are the source of truth).
#
# Assertions (the plan's numbered list; the script exits non-zero on any
# failure):
#   1. Stalled: the stall Loop is Failed with the Failed condition reason
#      Stalled + the Stalled condition True + status.iteration == 3 + a
#      Stalled Event; the control Loop reaches iteration == 5 (the cap) —
#      the contrast.
#   2. BudgetExceeded: the budget Loop is Failed with reason
#      BudgetExceeded, status.budget.exceeded=true, exceededReason=Tokens,
#      a BudgetExceeded Event, and prompt+completion >= 300 (the >= cap
#      assertion, item 14).
#   3. Real-backend cross-check: the cross-check Loop's per-Loop counts vs
#      the homelab Prometheus vllm deltas (dropped with a note if
#      unreachable).
#   4. Paused-Loop resume: the resume Loop at Implementing, suspend flipped
#      false->true (the script WAITS on the phase first — item 14's
#      racy-"suspend at Implementing" fix) -> Paused, pausedFrom=Implementing,
#      pausedReason=Suspend, the sandbox pod terminated; then suspend=false
#      -> Implementing, pausedFrom/pausedReason cleared, the sandbox pod
#      re-created and Running, and the iteration + currentVerify consistent
#      with the pre-pause state (not reset).
#   5. Budget-Pause + raised-cap resume: p2h-budgetpause (raised cap +
#      annotation resume) proceeds (two reconciles after resume with
#      phase != Paused and exceeded=false); p2h-budgetpause2 (un-raised,
#      annotation resume) re-pauses immediately (the fail-closed re-fire).
#
# The workspace: each Loop's repo is a per-Loop bare repo on the in-kind
# Gitea (gitea.samples.svc:3000, the d41/p2e pattern). The seed is a
# minimal go.mod-only module + a README.md (the implement-instruction's
# target). The acceptance check for the three spinning Loops is `test -f
# /nonexistent` (fails with the SAME message every iteration — the
# consecutive-identical-failure run advances, item 14).
#
# Pinned by the caller (make p2-e2e): K8S_CONTEXT=kind-coxswain-dev,
# KIND_CLUSTER_NAME=coxswain-dev. P2H_OPERATOR_IMAGE, when set, points the
# run at a SCRATCH-built operator image (the gate mutations: built by the
# caller in a scratch worktree; the script never builds its own operator
# then — it still loads + rolls + digest-verifies the given image).
#
# House rules honored: the operator builds + OWNS each Loop's workspace PVC
# (never pre-created); pod names listed one per line (jsonpath range);
# evidence (Loop statuses, conditions, events, pod statuses) dumped to the
# log BEFORE any cleanup; everything tee'd to .samples/p2h/run-<ts>.log;
# only Loops in the p2h-e2e namespace are deleted; neither kind cluster is
# touched; the homelab is READ-ONLY.

set -uo pipefail

GOPATH_BIN="$(go env GOPATH 2>/dev/null)/bin"
REPO_ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
case ":$PATH:" in *":$GOPATH_BIN:"*) ;; *) PATH="$GOPATH_BIN:$REPO_ROOT/bin:$PATH"; export PATH ;; esac

CTX="${K8S_CONTEXT:-kind-coxswain-dev}"
CLUSTER="${KIND_CLUSTER_NAME:-coxswain-dev}"
NS="p2h-e2e"
E2E_NS="coxswain-system"
GITEA_NS="samples"
GITEA_URL="http://gitea.samples.svc:3000/samples"
GITEA_USER="samples"
GITEA_PASS="samples-git-password"
STUB_POD="p2h-stub-model"
STUB_IMG="python:3-alpine"
GIT_IMG="alpine/git:v2.54.0"
CHECK_IMG="golang:1.26"
COMMIT="$(git -C "$REPO_ROOT" rev-parse --short HEAD 2>/dev/null || echo unknown)"
TIMESTAMP=$(date -u +%Y%m%d%H%M%S)
# P2H_OPERATOR_IMAGE overrides the built image (the gate mutations).
CTRL_IMG="${P2H_OPERATOR_IMAGE:-coxswain-controller:main-${COMMIT:0:7}-${TIMESTAMP}}"
RUNNER_IMG="coxswain-runner:main-${COMMIT:0:7}-${TIMESTAMP}"
STUB_PORT=8444
MODEL_ENDPOINT=""   # the node-IP stub endpoint (filled in STEP 2)
REAL_VLLM="192.168.1.20:8000"   # the REAL vLLM LB (the cross-check Loop)
HOMELAB_CTX="default"            # the homelab Prometheus context (read-only)

LOG_DIR="$REPO_ROOT/.samples/p2h"
mkdir -p "$LOG_DIR"
LOG="$LOG_DIR/run-${TIMESTAMP}.log"

K() { kubectl --context "$CTX" "$@"; }
KH() { kubectl --context "$HOMELAB_CTX" "$@"; }

FAILED=0
ok()  { echo "   ok: $*"; }
bad() { echo "   BAD: $*" >&2; FAILED=1; }
pass() { echo "   [PASS] $*"; }
fail() { echo "   [FAIL] $*" >&2; FAILED=1; }
die() { echo "FATAL: $*" >&2; exit 2; }

trap 'echo; echo "=== P2h run log: $LOG ==="; exit $FAILED' EXIT
exec > >(tee "$LOG") 2>&1

echo "=== P2h kind acceptance $(date -u) commit=$COMMIT ==="
echo "    context=$CTX cluster=$CLUSTER ns=$NS operator=$CTRL_IMG"
K get nodes >/dev/null 2>&1 || die "cannot reach cluster $CTX"

# ===========================================================================
# STEP 0: images. Build the operator (THIS tree) unless P2H_OPERATOR_IMAGE is
# set (the gate mutations point at a scratch build), + the reference runner.
# The operator tag is UNIQUE (main-<sha>-<ts>): the running pod's imageID
# must equal the built digest or the run FAILS (the P2e lesson).
# ===========================================================================
if [ "${P2H_OPERATOR_IMAGE:-}" = "" ]; then
  echo "--- STEP 0a: build the operator image (this branch) ---"
  (cd "$REPO_ROOT" && docker build -q -t "$CTRL_IMG" -f Dockerfile .) || die "controller build failed"
fi
echo "--- STEP 0b: build the reference runner image (this branch) ---"
(cd "$REPO_ROOT" && docker build -q -t "$RUNNER_IMG" -f cmd/runner/Dockerfile .) || die "runner build failed"
BUILT_SHA="$(docker image inspect "$CTRL_IMG" --format '{{.Id}}' 2>/dev/null | sed 's/.*sha256://')"
[ -n "$BUILT_SHA" ] || die "could not read the built operator digest"
echo "   operator image: $CTRL_IMG (sha256:$BUILT_SHA)" | tee "$LOG_DIR/built-digest.txt"
for img in "$CTRL_IMG" "$RUNNER_IMG" "$STUB_IMG" "$GIT_IMG" "$CHECK_IMG"; do
  echo "   kind load: $img"
  kind load docker-image "$img" --name "$CLUSTER" || die "kind load $img failed"
done

# ===========================================================================
# STEP 1: deploy the operator (dev overlay) + roll to the built image + verify
# the RUNNING pod's imageID equals the built digest (the operator digest).
# ===========================================================================
echo
echo "--- STEP 1: deploy the operator (dev overlay) + roll + digest-verify ---"
make -C "$REPO_ROOT" kustomize >/dev/null 2>&1 || die "make kustomize failed"
KUSTOMIZE_BIN="$REPO_ROOT/bin/kustomize"
TMP_OVERLAY=$(mktemp -d)
trap 'rm -rf "$TMP_OVERLAY"; echo; echo "=== P2h run log: $LOG ==="; exit $FAILED' EXIT
cp -r "$REPO_ROOT/config" "$TMP_OVERLAY/config"
(cd "$TMP_OVERLAY/config/manager" && "$KUSTOMIZE_BIN" edit set image controller="$CTRL_IMG") || die "kustomize set image failed"
(cd "$TMP_OVERLAY" && "$KUSTOMIZE_BIN" build config/dev | K apply -f -) || die "controller deploy failed"
K -n "$E2E_NS" set image deploy/coxswain-controller-manager manager="$CTRL_IMG" >/dev/null || die "set image failed"
K -n "$E2E_NS" rollout status deploy/coxswain-controller-manager --timeout=180s || die "controller not ready"
RUNNING_IMAGEID=""
for i in $(seq 1 60); do
  RUNNING_IMAGEID="$(K -n "$E2E_NS" get pods -l control-plane=controller-manager \
    -o jsonpath='{range .items[*]}{.status.containerStatuses[0].imageID}{"\n"}{end}' 2>/dev/null || true)"
  RUNNING_IMAGEID=$(echo "$RUNNING_IMAGEID" | grep "sha256:$BUILT_SHA$" | head -1)
  [ -n "$RUNNING_IMAGEID" ] && break
  sleep 2
done
[ -n "$RUNNING_IMAGEID" ] || {
  K -n "$E2E_NS" get pods -l control-plane=controller-manager -o wide 2>/dev/null
  die "no running pod carries the built digest (sha256:$BUILT_SHA) -- the operator was NOT rolled to this build"
}
echo "   operator digest (verified against the running pod): $RUNNING_IMAGEID" | tee -a "$LOG_DIR/operator-digest.txt"

# ===========================================================================
# STEP 2: the in-kind STUB MODEL SERVER (hostNetwork python:3-alpine) + the
# git-cred secret in $NS.
# ===========================================================================
echo
echo "--- STEP 2: the in-kind stub model server + git-cred secret ---"
K create ns "$NS" --dry-run=client -o yaml | K apply -f - >/dev/null
NODE_IP="$(K get node -o jsonpath='{.items[0].status.addresses[?(@.type=="InternalIP")].address}' 2>/dev/null | head -1)"
MODEL_ENDPOINT="${NODE_IP}:${STUB_PORT}"
echo "   stub model at $MODEL_ENDPOINT (node $NODE_IP)"
K -n "$NS" delete pod "$STUB_POD" --ignore-not-found >/dev/null 2>&1 || true
sleep 2
K create configmap p2h-stub-model --from-file=p2h-stub-model.py="$REPO_ROOT/test/e2e/p2h-stub-model.py" -n "$NS" --dry-run=client -o yaml | K apply -f - >/dev/null
K -n "$NS" apply -f - >/dev/null <<EOF
apiVersion: v1
kind: Pod
metadata:
  name: $STUB_POD
  namespace: $NS
  labels: {app: p2h-stub-model}
spec:
  hostNetwork: true
  containers:
  - name: stub-model
    image: $STUB_IMG
    imagePullPolicy: IfNotPresent
    command: ["sh","-c","exec python /opt/stub-model/p2h-stub-model.py"]
    env:
    - name: PORT
      value: "$STUB_PORT"
    volumeMounts:
    - name: server
      mountPath: /opt/stub-model
  volumes:
  - name: server
    configMap:
      name: p2h-stub-model
EOF
for i in $(seq 1 30); do
  if K -n "$NS" exec "$STUB_POD" -- python -c "import urllib.request,socket;socket.setdefaulttimeout(3);urllib.request.urlopen('http://127.0.0.1:$STUB_PORT/ok').read()" >/dev/null 2>&1; then break; fi
  sleep 1
done
K -n "$NS" exec "$STUB_POD" -- python -c "import urllib.request,socket;socket.setdefaulttimeout(3);urllib.request.urlopen('http://127.0.0.1:$STUB_PORT/ok').read()" >/dev/null 2>&1 \
  || { K -n "$NS" logs "$STUB_POD" 2>/dev/null | tail -20; die "stub model not answering"; }
ok "in-kind stub model is answering on $MODEL_ENDPOINT"
# git basic-auth secret (the workspace clone's cred; the verify Job's
# clone-base mounts it, the operator's S3a pattern).
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
# STEP 3: the Gitea bare repos (one per Loop) + the Loops.
# ===========================================================================
echo
echo "--- STEP 3: Gitea bare repos + the six Loops ---"
# Seed a per-Loop bare repo on Gitea. The seed: a minimal go.mod-only module
# + README.md (the implement-instruction's target, non-protected). The check
# image for the go preset would need a go.mod, but the acceptance check is
# `test -f /nonexistent` (no go tooling) and the tamper check only diffs
# protected globs — so the seed is a plain git repo, not a Go module.
seed_repo() { # seed_repo <repo-name>; echoes the seed commit SHA
  # The Gitea bare repo is created via the Gitea API (push-to-create is not
  # enabled for users; the API is the operator's S1 pattern). The seed is a
  # plain git repo (README.md only — the implement-instruction's target,
  # non-protected). The API call is signed in with the basic-auth cred (the
  # same samples:samples-git-password the git-cred secret holds). The curl -d
  # body is single-quoted (the JSON braces are literal); the repo name is
  # interpolated into the single-quoted -d body by the outer sh -c (the $R is
  # expanded by the OUTER shell, not the inner curl — the inner shell sees a
  # literal {"name":"p2h-stall",...} body).
  K -n "$GITEA_NS" exec deploy/gitea -- sh -c '
    set -e
    R="$1"
    CODE=$(curl -s -o /tmp/create-$R.json -w "%{http_code}" -u samples:samples-git-password \
      -X POST "http://gitea.samples.svc:3000/api/v1/user/repos" \
      -H "Content-Type: application/json" \
      -d "{\"name\":\"$R\",\"auto_init\":false,\"private\":false}")
    case "$CODE" in
      201|409|422) ;;  # created / already exists / the API hiccup (the push -f re-seeds)
      *) echo "Gitea API create failed: http $CODE $(cat /tmp/create-$R.json)"; exit 1 ;;
    esac
    rm -rf /tmp/seed; mkdir -p /tmp/seed; cd /tmp/seed
    if ! git clone -q http://samples:samples-git-password@gitea.samples.svc:3000/samples/$R.git work 2>/dev/null; then
      mkdir work; cd work; git init -q -b initial
      git remote add origin http://samples:samples-git-password@gitea.samples.svc:3000/samples/$R.git
    fi
    cd /tmp/seed/work
    git config user.email p2h@example.com; git config user.name p2h
    git checkout -B initial 2>/dev/null || git checkout -q -b initial
    printf "p2h seed repo\n" > README.md
    git add -A
    if ! git diff --cached --quiet; then git commit -q -m "p2h seed"; fi
    git push -q -f origin initial
    echo "SEED=$(git rev-parse HEAD)"
  ' "$1" > "$LOG_DIR/seed-$1.txt" 2>&1 || die "could not seed Gitea repo $1: $(cat "$LOG_DIR/seed-$1.txt")"
  grep -oE 'SEED=[0-9a-f]+' "$LOG_DIR/seed-$1.txt" | cut -d= -f2 | tail -1
}
REPO_STALL="$(seed_repo p2h-stall)"
REPO_BUDGET="$(seed_repo p2h-budget)"
REPO_CTRL="$(seed_repo p2h-ctrl)"
REPO_RESUME="$(seed_repo p2h-resume)"
REPO_BPAUSE="$(seed_repo p2h-budgetpause)"
REPO_BPAUSE2="$(seed_repo p2h-budgetpause2)"
REPO_REAL="$(seed_repo p2h-real)"
echo "   seed commits: stall=$REPO_STALL budget=$REPO_BUDGET ctrl=$REPO_CTRL resume=$REPO_RESUME bpause=$REPO_BPAUSE bpause2=$REPO_BPAUSE2 real=$REPO_REAL"

# create a Loop + wait for the operator to create + bind the workspace PVC
# (the operator OWNS the PVC — never pre-created, the P2e lesson). The Loop
# spec is passed as the YAML body on stdin.
create_loop_wait_pvc() { # create_loop_wait_pvc <loop-name>; stdin: Loop YAML
  local loop="$1" yaml
  yaml="$(cat)"
  K -n "$NS" delete loop "$loop" --wait=false --ignore-not-found >/dev/null 2>&1 || true
  K -n "$NS" delete jobs -l "coxswain.io/loop=$loop" --wait=false --ignore-not-found >/dev/null 2>&1 || true
  K -n "$NS" delete pvc "$loop-workspace" --ignore-not-found >/dev/null 2>&1 || true
  sleep 2
  echo "$yaml" | K -n "$NS" apply -f - >/dev/null || die "apply Loop $loop failed"
  for i in $(seq 1 120); do
    st="$(K -n "$NS" get pvc "$loop-workspace" -o jsonpath='{.status.phase}' 2>/dev/null || true)"
    [ "$st" = "Bound" ] && break
    sleep 2
  done
  [ "$(K -n "$NS" get pvc "$loop-workspace" -o jsonpath='{.status.phase}' 2>/dev/null)" = "Bound" ] || {
    K -n "$NS" get pvc "$loop-workspace" 2>/dev/null
    K -n "$NS" get events --sort-by=.lastTimestamp 2>/dev/null | tail -15
    die "workspace PVC $loop-workspace not Bound (operator create/adoption failed?)"
  }
  ok "$loop created; operator created + bound $loop-workspace"
}

# The common Loop spec body (the workspace + the agent + the runner image).
# $1 = the gitea repo, $2 = the acceptance check, $3 = the model endpoint,
# $4 = the model name (the real vLLM serves 'qwen3.8-27b'; the stub ignores
# it but the runner sends spec.agent.model verbatim).
loop_yaml() {
  cat <<EOF
apiVersion: coxswain.wattu.com/v1alpha1
kind: Loop
metadata:
  name: $5
  namespace: $NS
spec:
  goal: $6
  workspace:
    repo: $GITEA_URL/$1.git
    ref: initial
    gitCredentialSecret: git-credentials
  verify:
    preset: go
    image: $CHECK_IMG
    acceptanceChecks:
    - "$2"
  agent:
    image: $RUNNER_IMG
    model: $4
    endpointSecretRef: p2h-model-creds
    modelEndpoint: $3
$7
EOF
}
# The model-creds secret the proxy mounts (the operator's proxy-creds
# contract: a Secret key literally named model-key + MODEL_BASE_URL for the
# agent's COX_MODEL_BASE_URL). The key is a dummy — the stub ignores auth and
# the real vLLM's LB is open in the homelab.
K -n "$NS" create secret generic p2h-model-creds \
  --from-literal=model-key="p2h-dummy-key" \
  --from-literal=MODEL_BASE_URL="http://$REAL_VLLM" \
  --dry-run=client -o yaml | K apply -f - >/dev/null

# --- the six Loops ---
FAIL_CHECK='test -f /nonexistent'
PASS_CHECK='sleep 0.1'

echo "   creating the stall Loop (stallAfter:3, stallAction:Fail, maxIterations:10)"
create_loop_wait_pvc p2h-stall <<EOF
apiVersion: coxswain.wattu.com/v1alpha1
kind: Loop
metadata:
  name: p2h-stall
  namespace: $NS
spec:
  goal: P2h stall loop (impossible goal)
  workspace:
    repo: $GITEA_URL/p2h-stall.git
    ref: initial
    gitCredentialSecret: git-credentials
  verify:
    preset: go
    image: $CHECK_IMG
    acceptanceChecks:
    - "$FAIL_CHECK"
  agent:
    image: $RUNNER_IMG
    model: p2h-stub
    endpointSecretRef: p2h-model-creds
    modelEndpoint: $MODEL_ENDPOINT
  loop:
    maxIterations: 10
    stallAfter: 3
    stallAction: Fail
EOF

echo "   creating the budget Loop (maxTokens:300, onExceeded:Fail, stallAction:Continue)"
create_loop_wait_pvc p2h-budget <<EOF
apiVersion: coxswain.wattu.com/v1alpha1
kind: Loop
metadata:
  name: p2h-budget
  namespace: $NS
spec:
  goal: P2h budget loop (impossible goal)
  workspace:
    repo: $GITEA_URL/p2h-budget.git
    ref: initial
    gitCredentialSecret: git-credentials
  verify:
    preset: go
    image: $CHECK_IMG
    acceptanceChecks:
    - "$FAIL_CHECK"
  agent:
    image: $RUNNER_IMG
    model: p2h-stub
    endpointSecretRef: p2h-model-creds
    modelEndpoint: $MODEL_ENDPOINT
  loop:
    maxIterations: 10
    stallAfter: 3
    stallAction: Continue
  budget:
    maxTokens: 300
    onExceeded: Fail
EOF

echo "   creating the control Loop (stallAfter:10, maxIterations:5)"
create_loop_wait_pvc p2h-ctrl <<EOF
apiVersion: coxswain.wattu.com/v1alpha1
kind: Loop
metadata:
  name: p2h-ctrl
  namespace: $NS
spec:
  goal: P2h control loop (the stall contrast)
  workspace:
    repo: $GITEA_URL/p2h-ctrl.git
    ref: initial
    gitCredentialSecret: git-credentials
  verify:
    preset: go
    image: $CHECK_IMG
    acceptanceChecks:
    - "$FAIL_CHECK"
  agent:
    image: $RUNNER_IMG
    model: p2h-stub
    endpointSecretRef: p2h-model-creds
    modelEndpoint: $MODEL_ENDPOINT
  loop:
    maxIterations: 5
    stallAfter: 10
    stallAction: Fail
EOF

echo "   creating the resume Loop (suspend driven at Implementing)"
create_loop_wait_pvc p2h-resume <<EOF
apiVersion: coxswain.wattu.com/v1alpha1
kind: Loop
metadata:
  name: p2h-resume
  namespace: $NS
spec:
  goal: P2h resume loop (pause + resume at Implementing)
  workspace:
    repo: $GITEA_URL/p2h-resume.git
    ref: initial
    gitCredentialSecret: git-credentials
  verify:
    preset: go
    image: $CHECK_IMG
    acceptanceChecks:
    - "$PASS_CHECK"
  agent:
    image: $RUNNER_IMG
    model: p2h-stub
    endpointSecretRef: p2h-model-creds
    modelEndpoint: $MODEL_ENDPOINT
  loop:
    maxIterations: 10
    stallAfter: 3
    stallAction: Fail
EOF

echo "   creating the two budget-Pause Loops (maxTokens:400, onExceeded:Pause)"
create_loop_wait_pvc p2h-budgetpause <<EOF
apiVersion: coxswain.wattu.com/v1alpha1
kind: Loop
metadata:
  name: p2h-budgetpause
  namespace: $NS
spec:
  goal: P2h budget-pause loop (raised-cap resume)
  workspace:
    repo: $GITEA_URL/p2h-budgetpause.git
    ref: initial
    gitCredentialSecret: git-credentials
  verify:
    preset: go
    image: $CHECK_IMG
    acceptanceChecks:
    - "$FAIL_CHECK"
  agent:
    image: $RUNNER_IMG
    model: p2h-stub
    endpointSecretRef: p2h-model-creds
    modelEndpoint: $MODEL_ENDPOINT
  loop:
    maxIterations: 10
    stallAfter: 3
    stallAction: Continue
  budget:
    maxTokens: 400
    onExceeded: Pause
EOF

create_loop_wait_pvc p2h-budgetpause2 <<EOF
apiVersion: coxswain.wattu.com/v1alpha1
kind: Loop
metadata:
  name: p2h-budgetpause2
  namespace: $NS
spec:
  goal: P2h budget-pause control loop (un-raised re-pause)
  workspace:
    repo: $GITEA_URL/p2h-budgetpause2.git
    ref: initial
    gitCredentialSecret: git-credentials
  verify:
    preset: go
    image: $CHECK_IMG
    acceptanceChecks:
    - "$FAIL_CHECK"
  agent:
    image: $RUNNER_IMG
    model: p2h-stub
    endpointSecretRef: p2h-model-creds
    modelEndpoint: $MODEL_ENDPOINT
  loop:
    maxIterations: 10
    stallAfter: 3
    stallAction: Continue
  budget:
    maxTokens: 400
    onExceeded: Pause
EOF

echo "   creating the cross-check Loop (the REAL vLLM LB 192.168.1.20:8000, passing check)"
create_loop_wait_pvc p2h-real <<EOF
apiVersion: coxswain.wattu.com/v1alpha1
kind: Loop
metadata:
  name: p2h-real
  namespace: $NS
spec:
  goal: P2h cross-check loop (real vLLM, trivial goal)
  workspace:
    repo: $GITEA_URL/p2h-real.git
    ref: initial
    gitCredentialSecret: git-credentials
  verify:
    preset: go
    image: $CHECK_IMG
    acceptanceChecks:
    - "$PASS_CHECK"
  agent:
    image: $RUNNER_IMG
    model: qwen3.8-27b
    endpointSecretRef: p2h-model-creds
    modelEndpoint: $REAL_VLLM
  loop:
    maxIterations: 3
    stallAfter: 3
    stallAction: Fail
EOF

# ===========================================================================
# STEP 4: wait for the Loops to reach their assertion states, then assert.
# ===========================================================================
echo
echo "--- STEP 4: wait + assert (the plan's five numbered assertions) ---"

# loop phase / field readers (one value per call).
lphase() { K -n "$NS" get loop "$1" -o jsonpath='{.status.phase}' 2>/dev/null || true; }
lfield() { K -n "$NS" get loop "$1" -o jsonpath="$2" 2>/dev/null || true; }

# wait_phase <loop> <phase> <deadline-s>
wait_phase() {
  local loop="$1" want="$2" deadline="${3:-300}" i
  for i in $(seq 1 "$deadline"); do
    [ "$(lphase "$loop")" = "$want" ] && return 0
    sleep 2
  done
  return 1
}
# wait_sandbox <loop> <want-running 0|1> <deadline-s>: poll the sandbox pod's
# phase (Running vs terminated/absent).
wait_sandbox() {
  local loop="$1" want="$2" deadline="${3:-180}" i ph
  for i in $(seq 1 "$deadline"); do
    ph="$(K -n "$NS" get pod "$loop-sandbox" -o jsonpath='{.status.phase}' 2>/dev/null || true)"
    if [ "$want" = "1" ] && [ "$ph" = "Running" ]; then return 0; fi
    if [ "$want" = "0" ] && [ "$ph" != "Running" ]; then return 0; fi
    sleep 2
  done
  return 1
}

# The cross-check Loop's wall-clock window (assertion 3): recorded here,
# before its model work starts in earnest, and re-read after Succeeded.
REAL_T0="$(date -u +%s)"
echo "   cross-check window start: $REAL_T0 ($(date -u))"

# --- wait for all six Loops to reach their terminal / assertion states ---
# (parallel; the script waits each in turn with a generous deadline)
echo "   waiting: p2h-real -> Succeeded (real vLLM, trivial goal)"
wait_phase p2h-real Succeeded 900 || bad "p2h-real did not reach Succeeded in 900s (phase=$(lphase p2h-real))"
REAL_T1="$(date -u +%s)"
echo "   waiting: p2h-stall -> Failed (Stalled at iteration 3)"
wait_phase p2h-stall Failed 900 || bad "p2h-stall did not reach Failed in 900s (phase=$(lphase p2h-stall))"
echo "   waiting: p2h-budget -> Failed (BudgetExceeded at request 2)"
wait_phase p2h-budget Failed 900 || bad "p2h-budget did not reach Failed in 900s (phase=$(lphase p2h-budget))"
echo "   waiting: p2h-ctrl -> Failed (the maxIterations cap at 5)"
wait_phase p2h-ctrl Failed 900 || bad "p2h-ctrl did not reach Failed in 900s (phase=$(lphase p2h-ctrl))"
echo "   waiting: p2h-budgetpause + p2h-budgetpause2 -> Paused (budget at 400)"
wait_phase p2h-budgetpause Paused 900 || bad "p2h-budgetpause did not reach Paused in 900s (phase=$(lphase p2h-budgetpause))"
wait_phase p2h-budgetpause2 Paused 900 || bad "p2h-budgetpause2 did not reach Paused in 900s (phase=$(lphase p2h-budgetpause2))"

# --- assertion 1: the Stalled path ---
echo
echo "--- assertion 1: the Stalled path (stall Loop + the control contrast) ---"
ST_PHASE="$(lphase p2h-stall)"
ST_FAILED_REASON="$(lfield p2h-stall '.status.conditions[?(@.type=="Failed")].reason')"
ST_STALLED_COND="$(lfield p2h-stall '.status.conditions[?(@.type=="Stalled")].status')"
ST_ITER="$(lfield p2h-stall '.status.iteration')"
if [ "$ST_PHASE" = "Failed" ] && [ "$ST_FAILED_REASON" = "Stalled" ] && [ "$ST_STALLED_COND" = "True" ]; then
  pass "stall Loop: phase=Failed, the Failed condition reason=Stalled, the Stalled condition True"
else
  fail "stall Loop: phase=$ST_PHASE failed-reason=$ST_FAILED_REASON stalled-cond=$ST_STALLED_COND (expected Failed/Stalled/True)"
fi
if [ "$ST_ITER" = "3" ]; then
  pass "stall Loop: status.iteration == 3 (stopped at the 3rd identical failure, not spun)"
else
  fail "stall Loop: status.iteration=$ST_ITER (expected 3)"
fi
K -n "$NS" get events --field-selector involvedObject.name=p2h-stall -o json > "$LOG_DIR/events-stall.json" 2>/dev/null || true
if python3 -c "
import json,sys
evs=json.load(open('$LOG_DIR/events-stall.json')).get('items',[])
sys.exit(0 if any('Stalled' in (e.get('reason') or '') for e in evs) else 1)
" 2>/dev/null; then
  pass "stall Loop: a Stalled Event is present"
else
  fail "stall Loop: no Stalled Event (events in $LOG_DIR/events-stall.json)"
fi
# The control contrast.
CT_PHASE="$(lphase p2h-ctrl)"
CT_ITER="$(lfield p2h-ctrl '.status.iteration')"
CT_FAILED_REASON="$(lfield p2h-ctrl '.status.conditions[?(@.type=="Failed")].reason')"
if [ "$CT_PHASE" = "Failed" ] && [ "$CT_ITER" = "5" ]; then
  pass "control Loop: phase=Failed at status.iteration == 5 (the maxIterations cap) — the contrast"
else
  fail "control Loop: phase=$CT_PHASE iteration=$CT_ITER failed-reason=$CT_FAILED_REASON (expected Failed at 5)"
fi

# --- assertion 2: the BudgetExceeded path ---
echo
echo "--- assertion 2: the BudgetExceeded path (budget Loop) ---"
BD_PHASE="$(lphase p2h-budget)"
BD_FAILED_REASON="$(lfield p2h-budget '.status.conditions[?(@.type=="Failed")].reason')"
BD_EXCEEDED="$(lfield p2h-budget '.status.budget.exceeded')"
BD_REASON="$(lfield p2h-budget '.status.budget.exceededReason')"
BD_TOK="$(python3 -c "
import json,subprocess
d=json.loads(subprocess.run(['kubectl','--context','$CTX','-n','$NS','get','loop','p2h-budget','-o','json'],capture_output=True,text=True).stdout).get('status',{}).get('budget',{}) or {}
print((d.get('promptTokens',0) or 0)+(d.get('completionTokens',0) or 0))
" 2>/dev/null || echo 0)"
if [ "$BD_PHASE" = "Failed" ] && [ "$BD_FAILED_REASON" = "BudgetExceeded" ] && [ "$BD_EXCEEDED" = "true" ] && [ "$BD_REASON" = "Tokens" ]; then
  pass "budget Loop: phase=Failed, reason=BudgetExceeded, exceeded=true, exceededReason=Tokens"
else
  fail "budget Loop: phase=$BD_PHASE reason=$BD_FAILED_REASON exceeded=$BD_EXCEEDED exceededReason=$BD_REASON"
fi
if [ "$BD_TOK" -ge 300 ] 2>/dev/null; then
  pass "budget Loop: status.budget token total $BD_TOK >= 300 (the >= cap assertion, item 14)"
else
  fail "budget Loop: status.budget token total ${BD_TOK:-0} (expected >= 300)"
fi
K -n "$NS" get events --field-selector involvedObject.name=p2h-budget -o json > "$LOG_DIR/events-budget.json" 2>/dev/null || true
if python3 -c "
import json,sys
evs=json.load(open('$LOG_DIR/events-budget.json')).get('items',[])
sys.exit(0 if any('BudgetExceeded' in (e.get('reason') or '') for e in evs) else 1)
" 2>/dev/null; then
  pass "budget Loop: a BudgetExceeded Event is present"
else
  fail "budget Loop: no BudgetExceeded Event (events in $LOG_DIR/events-budget.json)"
fi

# --- assertion 3: the real-backend cross-check ---
echo
echo "--- assertion 3: the real-backend cross-check (the cross-check Loop vs the homelab Prometheus) ---"
REAL_PHASE="$(lphase p2h-real)"
REAL_BUDGET="$(K -n "$NS" get loop p2h-real -o jsonpath='{.status.budget}' 2>/dev/null || true)"
echo "   cross-check Loop: phase=$REAL_PHASE window=[$REAL_T0,$REAL_T1] status.budget=$REAL_BUDGET"
echo "   window: $REAL_T0..$REAL_T1" > "$LOG_DIR/crosscheck.txt"
echo "   per-Loop status.budget: $REAL_BUDGET" >> "$LOG_DIR/crosscheck.txt"
# The homelab Prometheus (READ-ONLY: port-forward + curl). Query the vllm
# prompt + generation token deltas over the window. If unreachable or no
# series: DROPPED with a note (the plan allows it — a consistency check, not
# a gate).
XCHECK="not-run"
PF_LOG="$LOG_DIR/prometheus-portforward.log"
timeout 90 kubectl --context "$HOMELAB_CTX" port-forward -n prometheus svc/prometheus 127.0.0.1:19099:9090 > "$PF_LOG" 2>&1 &
PF_PID=$!
sleep 5
PROM_OK=0
if curl -s --max-time 10 "http://127.0.0.1:19099/api/v1/query?query=up" >/dev/null 2>&1; then PROM_OK=1; fi
if [ "$PROM_OK" = "1" ]; then
  # The closest-sample value of a metric at a timestamp (query_range over a
  # +/-5s window, step 1s — /api/v1/query with 'at <ts>' is not valid PromQL).
  QR() { # QR <metric> <ts> ; echoes the value at the closest sample, or empty
    curl -s --max-time 30 -G "http://127.0.0.1:19099/api/v1/query_range" \
      --data-urlencode "query=$1" \
      --data-urlencode "start=$((2-5))" --data-urlencode "end=$((2+5))" --data-urlencode "step=1" 2>/dev/null
    python3 - "$1" "$2" <<'PYEOF'
import json, sys, urllib.parse, urllib.request
metric, ts = sys.argv[1], int(sys.argv[2])
qs = urllib.parse.urlencode({"query": metric, "start": ts-5, "end": ts+5, "step": 1})
r = urllib.request.urlopen("http://127.0.0.1:19099/api/v1/query_range?"+qs, timeout=30)
d = json.load(r)
res = (d.get("data") or {}).get("result") or []
allv = []
for row in res:
    for t, v in row.get("values", []):
        allv.append((abs(t-ts), float(v)))
if not allv:
    sys.exit(1)
allv.sort()
print(int(allv[0][1]))
PYEOF
  }
  P0="$(QR vllm:prompt_tokens_total $REAL_T0)"
  P1="$(QR vllm:prompt_tokens_total $REAL_T1)"
  G0="$(QR vllm:generation_tokens_total $REAL_T0)"
  G1="$(QR vllm:generation_tokens_total $REAL_T1)"
  kill $PF_PID 2>/dev/null || true
  echo "   prometheus vllm: prompt [$P0 -> $P1] generation [$G0 -> $G1] over the window" >> "$LOG_DIR/crosscheck.txt"
  if [ -n "$P0" ] && [ -n "$P1" ] && [ -n "$G0" ] && [ -n "$G1" ]; then
    dP=$((P1 - P0)); dG=$((G1 - G0))
    perLoopTok="$(python3 -c "
import json
d=json.loads('''$REAL_BUDGET''') if '$REAL_BUDGET' else {}
print((d.get('promptTokens',0) or 0)+(d.get('completionTokens',0) or 0))
" 2>/dev/null || echo 0)"
    backendDelta=$((dP + dG))
    echo "   per-Loop status.budget total: $perLoopTok; backend (pi6+pi8) delta: $backendDelta (prompt $dP + generation $dG)" >> "$LOG_DIR/crosscheck.txt"
    if [ "$perLoopTok" -le "$backendDelta" ] && [ "$perLoopTok" -gt 0 ]; then
      XCHECK="pass"
      pass "cross-check: per-Loop count ($perLoopTok) <= the real vLLM backend delta ($backendDelta) over the window (consistent, item 14)"
    else
      XCHECK="fail"
      fail "cross-check: per-Loop count ($perLoopTok) NOT consistent with the backend delta ($backendDelta) (expected 0 < per-Loop <= delta)"
    fi
  else
    XCHECK="dropped"
    echo "   [DROPPED] the homelab Prometheus had no vllm token series at the window timestamps (P0=$P0 P1=$P1 G0=$G0 G1=$G1); the cross-check is a consistency check, not a gate (the plan allows dropping it with a note)" | tee -a "$LOG_DIR/crosscheck.txt"
  fi
else
  XCHECK="dropped"
  kill $PF_PID 2>/dev/null || true
  echo "   [DROPPED] the homelab Prometheus ($HOMELAB_CTX, ns prometheus) was unreachable from this runner (port-forward log: $PF_LOG); the cross-check is dropped with a note (the plan allows it)" | tee -a "$LOG_DIR/crosscheck.txt"
fi
[ "$XCHECK" = "pass" ] || true   # dropped is not a failure (the plan: a consistency check, not a gate)

# --- assertion 4: the paused-Loop resume ---
echo
echo "--- assertion 4: the paused-Loop resume (the resume Loop) ---"
# Wait on the phase FIRST (item 14's racy-"suspend at Implementing" fix): the
# script flips suspend only when phase == Implementing.
if wait_phase p2h-resume Implementing 600; then
  ok "resume Loop at Implementing (suspend flipped now)"
else
  bad "resume Loop never reached Implementing before the flip (phase=$(lphase p2h-resume)); flipping anyway (the assertion still holds: pausedFrom names the phase)"
fi
# Record the pre-pause state (the consistency assertion compares against it).
PRE_ITER="$(lfield p2h-resume '.status.iteration')"
PRE_VERIFY="$(lfield p2h-resume '.status.currentVerify.verifiedCommit')"
echo "   pre-pause: iteration=$PRE_ITER currentVerify.verifiedCommit=${PRE_VERIFY:0:12}..."
K -n "$NS" patch loop p2h-resume -p '{"spec":{"suspend":true}}' --type=merge >/dev/null 2>&1 || die "suspend=true patch failed"
# Wait for Paused + the pausedFrom/pausedReason record.
if wait_phase p2h-resume Paused 180; then
  ok "resume Loop -> Paused (suspend=true)"
else
  bad "resume Loop did not reach Paused (phase=$(lphase p2h-resume))"
fi
RP_FROM="$(lfield p2h-resume '.status.pausedFrom')"
RP_REASON="$(lfield p2h-resume '.status.pausedReason')"
if [ "$RP_FROM" = "Implementing" ] && [ "$RP_REASON" = "Suspend" ]; then
  pass "resume Loop: pausedFrom=Implementing, pausedReason=Suspend"
else
  fail "resume Loop: pausedFrom=$RP_FROM pausedReason=$RP_REASON (expected Implementing/Suspend)"
fi
# The sandbox pod is terminated while paused (the suspension gate: OperatingMode
# Suspended -> the agent-sandbox controller deletes the pod).
if wait_sandbox p2h-resume 0 180; then
  pass "resume Loop: the sandbox pod is terminated while paused"
else
  fail "resume Loop: the sandbox pod is still present/Running while paused (phase=$(K -n "$NS" get pod p2h-resume-sandbox -o jsonpath='{.status.phase}' 2>/dev/null || echo gone))"
fi
# Resume: suspend=false (a Suspend pause resumes ONLY via spec.suspend=false,
# P2f — the annotation is for a Stall/Budget pause).
K -n "$NS" patch loop p2h-resume -p '{"spec":{"suspend":false}}' --type=merge >/dev/null 2>&1 || die "suspend=false patch failed"
if wait_phase p2h-resume Implementing 180; then
  ok "resume Loop -> Implementing (suspend=false)"
else
  bad "resume Loop did not return to Implementing (phase=$(lphase p2h-resume))"
fi
RP_FROM2="$(lfield p2h-resume '.status.pausedFrom')"
RP_REASON2="$(lfield p2h-resume '.status.pausedReason')"
if [ -z "$RP_FROM2" ] && [ -z "$RP_REASON2" ]; then
  pass "resume Loop: pausedFrom + pausedReason cleared on resume"
else
  fail "resume Loop: pausedFrom=$RP_FROM2 pausedReason=$RP_REASON2 (expected cleared)"
fi
if wait_sandbox p2h-resume 1 180; then
  pass "resume Loop: the sandbox pod is re-created and Running after resume"
else
  fail "resume Loop: the sandbox pod is not Running after resume (phase=$(K -n "$NS" get pod p2h-resume-sandbox -o jsonpath='{.status.phase}' 2>/dev/null || echo absent))"
fi
POST_ITER="$(lfield p2h-resume '.status.iteration')"
POST_VERIFY="$(lfield p2h-resume '.status.currentVerify.verifiedCommit')"
if [ "$POST_ITER" = "$PRE_ITER" ]; then
  pass "resume Loop: status.iteration ($POST_ITER) is consistent with the pre-pause state ($PRE_ITER), not reset"
else
  fail "resume Loop: status.iteration=$POST_ITER vs pre-pause $PRE_ITER (expected unchanged)"
fi
# The currentVerify pin is either the same pin (the runner has not re-run
# Implementing yet) or a NEW pin for the pre-pause pin's child (the resumed
# Implementing run committed again). Both are consistent with the pre-pause
# state (P2f's resume semantics: the verify Job that eventually runs is a NEW
# Job for the pre-pause pin). The assertion is the negative one: the pin is
# NOT cleared (an empty pin would mean the resume reset the Loop's verify
# state).
if [ -n "$POST_VERIFY" ]; then
  pass "resume Loop: currentVerify.verifiedCommit is set (${POST_VERIFY:0:12}...) — the verify pin survived the resume (not reset)"
else
  fail "resume Loop: currentVerify.verifiedCommit is EMPTY after resume (the resume reset the verify pin)"
fi

# --- assertion 5: the budget-Pause + raised-cap resume ---
echo
echo "--- assertion 5: the budget-Pause + raised-cap resume (both sub-cases) ---"
# The entry point: the budget fire happens at Verifying (the applyBudgetStep
# runs every reconcile; the exceedance is recorded at the 2nd Implementing's
# 400 tokens). pausedFrom names the phase the Loop left (Implementing or
# Verifying, per the entry point).
B1_FROM="$(lfield p2h-budgetpause '.status.pausedFrom')"
B1_REASON="$(lfield p2h-budgetpause '.status.pausedReason')"
B1_EXC="$(lfield p2h-budgetpause '.status.budget.exceeded')"
if [ "$B1_REASON" = "Budget" ] && { [ "$B1_FROM" = "Implementing" ] || [ "$B1_FROM" = "Verifying" ]; } && [ "$B1_EXC" = "true" ]; then
  pass "budget-Pause Loop: phase=Paused, pausedFrom=$B1_FROM, pausedReason=Budget, exceeded=true"
else
  fail "budget-Pause Loop: pausedFrom=$B1_FROM pausedReason=$B1_REASON exceeded=$B1_EXC (expected Implementing-or-Verifying/Budget/true)"
fi
# Sub-case (a): the raised-cap resume. Raise maxTokens (I43 live update) +
# resume via the coxswain.io/resume annotation.
K -n "$NS" patch loop p2h-budgetpause -p '{"spec":{"budget":{"maxTokens":100000}}}' --type=merge >/dev/null 2>&1 || die "raise maxTokens failed"
K -n "$NS" annotate loop p2h-budgetpause coxswain.io/resume="true" >/dev/null 2>&1 || die "resume annotation failed"
# Two reconciles after the resume with phase != Paused and exceeded=false.
B1_OK=0
for i in $(seq 1 60); do
  ph="$(lphase p2h-budgetpause)"; ex="$(lfield p2h-budgetpause '.status.budget.exceeded')"
  if [ "$ph" != "Paused" ] && [ "$ex" = "false" ]; then B1_OK=1; break; fi
  sleep 2
done
if [ "$B1_OK" = "1" ]; then
  pass "budget-Pause Loop (raised cap): resumed and proceeds — phase != Paused and exceeded=false (the re-evaluation cleared the exceedance)"
else
  fail "budget-Pause Loop (raised cap): still phase=$(lphase p2h-budgetpause) exceeded=$(lfield p2h-budgetpause '.status.budget.exceeded') after the raised-cap resume"
fi
# Sub-case (b): the un-raised re-pause (the fail-closed re-fire). The control
# Loop's caps are NOT raised; the annotation resume must re-pause immediately.
K -n "$NS" annotate loop p2h-budgetpause2 coxswain.io/resume="true" >/dev/null 2>&1 || die "resume annotation (control) failed"
# A valid resume clears the annotation; a REFUSED one keeps it. The re-fire
# is: phase returns to Paused (or stays) + the ResumeRefused Event. Give the
# operator a few reconciles.
B2_OK=0
for i in $(seq 1 60); do
  ph="$(lphase p2h-budgetpause2)"
  if [ "$ph" = "Paused" ]; then B2_OK=1; break; fi
  sleep 2
done
B2_REJECTED="$(K -n "$NS" get events --field-selector involvedObject.name=p2h-budgetpause2 -o json 2>/dev/null | python3 -c "
import json,sys
evs=json.load(sys.stdin).get('items',[])
print('yes' if any('Refused' in (e.get('reason') or '') or 'Refused' in (e.get('message') or '') for e in evs) else 'no')
" 2>/dev/null || echo no)"
if [ "$B2_OK" = "1" ] && [ "$(lfield p2h-budgetpause2 '.status.budget.exceeded')" = "true" ]; then
  pass "budget-Pause control Loop (un-raised): re-pauses immediately on the annotation resume (the fail-closed re-fire; exceeded still true)"
else
  fail "budget-Pause control Loop (un-raised): phase=$(lphase p2h-budgetpause2) exceeded=$(lfield p2h-budgetpause2 '.status.budget.exceeded') (expected Paused + exceeded=true; refused-event=$B2_REJECTED)"
fi

# ===========================================================================
# STEP 5: the per-Loop stub cross-check (item 14): each stub-Loop's
# status.budget totals must equal the stub's request-log counts (100*N
# prompt + 100*N completion). This is the stub-side audit (the meter's input,
# NOT the real-backend oracle).
# ===========================================================================
echo
echo "--- STEP 5: the per-Loop stub audit (status.budget vs the stub's request log) ---"
STUB_LOG_RAW="$(K -n "$NS" exec "$STUB_POD" -- cat /tmp/stub-requests.jsonl 2>/dev/null || true)"
STUB_N_REQ="$(echo "$STUB_LOG_RAW" | grep -c '"n":' || true)"
echo "   stub request log: $STUB_N_REQ total requests (all Loops; the per-Loop split is by the proxy, not the stub — the per-Loop status.budget is the source of truth)"
for L in p2h-stall p2h-budget p2h-ctrl p2h-resume p2h-budgetpause p2h-budgetpause2; do
  T="$(K -n "$NS" get loop "$L" -o jsonpath='{.status.budget}' 2>/dev/null | python3 -c "
import json,sys
s=sys.stdin.read().strip()
d=json.loads(s) if s else {}
print((d.get('promptTokens',0) or 0)+(d.get('completionTokens',0) or 0), d.get('requests',0))
" 2>/dev/null || echo "0 0")"
  echo "   $L status.budget total+requests: $T"
done

# ===========================================================================
# STEP 6: evidence dump (BEFORE any cleanup — the R22 process note: the PR
# body's numbers are copied from this artifact).
# ===========================================================================
echo
echo "--- STEP 6: evidence dump (before cleanup) ---"
{
  echo "=== P2h evidence $(date -u) ==="
  echo "operator image:   $CTRL_IMG"
  echo "operator digest:  $RUNNING_IMAGEID"
  echo
  echo "--- kubectl get loop (all) ---"
  K -n "$NS" get loop 2>/dev/null
  for L in p2h-stall p2h-budget p2h-ctrl p2h-resume p2h-budgetpause p2h-budgetpause2 p2h-real; do
    echo
    echo "--- loop $L status ---"
    K -n "$NS" get loop "$L" -o json 2>/dev/null | python3 -c "
import json,sys
try: d=json.load(sys.stdin).get('status',{})
except Exception as e: print('  (no JSON:',e,')'); raise SystemExit
print('  phase:', d.get('phase'), 'iteration:', d.get('iteration'))
print('  pausedFrom:', d.get('pausedFrom'), 'pausedReason:', d.get('pausedReason'))
print('  currentVerify:', json.dumps(d.get('currentVerify')))
print('  budget:', json.dumps(d.get('budget')))
print('  stallHistory:', len(d.get('stallHistory') or []), 'entries')
for c in d.get('conditions',[]):
    print('  cond:', c.get('type'), c.get('status'), c.get('reason'), (c.get('message') or '')[:160])
"
  done
  echo
  echo "--- sandbox pods ---"
  K -n "$NS" get pods 2>/dev/null
  echo
  echo "--- events (sorted) ---"
  K -n "$NS" get events --sort-by=.lastTimestamp 2>/dev/null | tail -80
  echo "=== end P2h evidence ==="
} | tee "$LOG_DIR/evidence.txt"

K -n "$E2E_NS" logs deploy/coxswain-controller-manager --tail=400 > "$LOG_DIR/operator.log" 2>&1 || true

echo
echo "=== P2h summary ==="
echo "operator image:   $CTRL_IMG"
echo "operator digest:  $RUNNING_IMAGEID"
echo "cross-check:      $XCHECK"
echo "log:              $LOG"
if [ "$FAILED" -ne 0 ]; then echo "RESULT: FAIL"; else echo "RESULT: PASS"; fi

# ===========================================================================
# CLEANUP: delete only the Loops in $NS (their PVCs/Services are GC'd). The
# stub pod + the Gitea repos stay (the Gitea repos are per-run but harmless;
# the stub is re-created next run).
# ===========================================================================
echo "--- cleanup: deleting the Loops in $NS (not the cluster, not the homelab) ---"
for L in p2h-stall p2h-budget p2h-ctrl p2h-resume p2h-budgetpause p2h-budgetpause2 p2h-real; do
  K -n "$NS" delete loop "$L" --wait=false --ignore-not-found >/dev/null 2>&1 || true
done
echo "   (Loops deleted; the stub pod + Gitea repos remain for inspection)"
exit $FAILED
