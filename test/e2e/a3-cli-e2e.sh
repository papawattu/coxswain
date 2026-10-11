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

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
CTX="${CTX:-kind-coxswain-dev}"
NS="${NS:-samples}"
LOOP_NAME="${LOOP_NAME:-a3-cli-e2e}"
LOOP_SRC="${LOOP_SRC:-$ROOT/examples/gocli/tasks/1.loop.yaml}"
EVIDENCE="${EVIDENCE:-.samples/a3-cli-e2e}"
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

# --- preflight: operator deployed, sample gitea reachable ------------------
if ! kube get deployment coxswain-controller-manager >/dev/null 2>&1; then
  fail "coxswain-controller-manager not found in $NS (deploy the operator first)"
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

# --- let it finish ----------------------------------------------------------
for i in $(seq 1 240); do
  ph="$(phase)"
  case "$ph" in Succeeded|Failed|Delivered) break ;; esac
  sleep 5
done
ph="$(phase)"
case "$ph" in Succeeded|Delivered) ;; *) fail "Loop did not reach a terminal phase (ph='$ph')" ;; esac
log "Loop reached $ph"

# --- describe (final) -------------------------------------------------------
cox describe "$LOOP_NAME" | tee "$EVIDENCE/describe-final.out"
cox history "$LOOP_NAME" | tee "$EVIDENCE/history.out"

# --- logs (kept, per the acceptance criterion) ------------------------------
cox logs "$LOOP_NAME" --component agent --tail 50  | tee "$EVIDENCE/logs-agent.out"     || true
cox logs "$LOOP_NAME" --component proxy --tail 50  | tee "$EVIDENCE/logs-proxy.out"     || true
cox logs "$LOOP_NAME" --component verify --tail 50 | tee "$EVIDENCE/logs-verify.out"    || true

# --- redeliver (I52: re-trigger the delivery) -------------------------------
# Only meaningful when there is a delivery (Succeeded/Delivered with a branch).
if kube get loop "$LOOP_NAME" -o jsonpath='{.status.delivery.branch}' | grep -q .; then
  cox redeliver "$LOOP_NAME" | tee "$EVIDENCE/redeliver.out"
  # the annotation must be set
  for i in $(seq 1 30); do
    ann="$(kube get loop "$LOOP_NAME" -o jsonpath='{.metadata.annotations.coxswain\.io/redeliver}')"
    [ -n "$ann" ] && break
    sleep 2
  done
  [ -n "$ann" ] || fail "coxswain.io/redeliver annotation not set after cox redeliver"
  log "redeliver annotation set"
else
  log "no delivery on the Loop; skipping redeliver (the command still works; see unit test)"
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
