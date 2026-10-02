#!/usr/bin/env bash
# samples-check.sh — verify the SDLC sample apps (docs/SAMPLES-PLAN.md, S1).
#
# For each app in examples/ that has tasks, and each task in
# tasks/<n>.patch:
#   1. Copy the seed state to a temp dir, git-init it, run the app's
#      acceptance checks — they must FAIL on the seed state.
#   2. Apply the task's reference patch, re-run the checks — they must PASS.
#
# Multi-task apps are cumulative: task N's checks run in the state where
# tasks 1..N-1 are applied, so each reference patch is validated against the
# state the Loop would actually see. The per-app check functions assert RED
# only on this task's own tests (a suite red on a different task's tests is
# not a valid seed state for this task).
#
# No cluster needed. The repo tree is never touched: every run happens in a
# fresh mktemp copy (removed on exit).
set -uo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
FAILURES=0
CUR_APP=""
CUR_TASK=0

say()  { printf '%s\n' "$*"; }
fail() { say "FAIL: $*"; FAILURES=$((FAILURES + 1)); }
pass() { say "PASS: $*"; }

# --- acceptance checks per app (the ONLY gate to Succeeded, SAMPLES-PLAN §1) ---
# Each returns 0 when its assertions hold, 1 otherwise.
#
# NOTE: `local` resets $?, so we capture rc on a separate line after the
# command-substitution assignment, and declare locals on their own line.

checks_gocli() {
  local d="$1" out rc
  d="$1"
  out=$(cd "$d" && { go build ./... && go vet ./... && go test ./...; } 2>&1)
  rc=$?
  if [ "$rc" -eq 0 ]; then return 0; fi
  case "$CUR_APP-$CUR_TASK" in
    gocli-1) printf '%s' "$out" | grep -q 'TestRound' ;;
    gocli-2) printf '%s' "$out" | grep -q 'TestMainJSON' ;;
    *) return 1 ;;
  esac
}

checks_pylib() {
  local d="$1" out rc
  d="$1"
  out=$(cd "$d" && { python3 -m compileall -q . && python3 -m unittest discover -s tests; } 2>&1)
  rc=$?
  if [ "$rc" -eq 0 ]; then return 0; fi
  case "$CUR_APP-$CUR_TASK" in
    pylib-1) printf '%s' "$out" | grep -q 'Median\|test_median' ;;
    pylib-2) printf '%s' "$out" | grep -q 'Clamp\|test_clamp' ;;
    *) return 1 ;;
  esac
}

checks_webapi() {
  local d="$1" out rc
  d="$1"
  out=$(cd "$d" && { go build ./... && bash test/smoke.sh; } 2>&1)
  rc=$?
  if [ "$rc" -eq 0 ]; then return 0; fi
  case "$CUR_APP-$CUR_TASK" in
    webapi-1) printf '%s' "$out" | grep -q 'ping: FAIL' ;;
    webapi-2) printf '%s' "$out" | grep -q 'echo: FAIL' ;;
    *) return 1 ;;
  esac
}

task_count() {
  ls "$ROOT/examples/$1/tasks/"*.patch 2>/dev/null | wc -l | tr -d ' '
}

commit_state() {
  (cd "$1" && git add -A && \
    git -c user.email=samples@coxswain.local -c user.name=samples commit -qm "state" || true)
}

check_app() {
  local app="$1"
  local seed="$ROOT/examples/$app"
  local n work runner t
  n=$(task_count "$app")
  say "== $app: $n task(s) =="
  if [ "$n" -eq 0 ]; then
    fail "$app: no tasks/*.patch found"
    return
  fi

  work=$(mktemp -d)
  rm -rf "$work"; mkdir -p "$work"
  cp -r "$seed/." "$work/"
  find "$work" -type d -name '__pycache__' -exec rm -rf {} + 2>/dev/null
  (cd "$work" && git init -q && git add -A && \
    git -c user.email=samples@coxswain.local -c user.name=samples commit -qm seed)

  CUR_APP="$app"
  runner="checks_$app"
  for t in $(seq 1 "$n"); do
    CUR_TASK=$t

    # Seed-state assertion: the suite must be red in a way that involves
    # this task's own tests.
    if "$runner" "$work"; then
      fail "$app task $t: acceptance checks PASS on seed state (expected FAIL)"
    else
      pass "$app task $t: acceptance checks fail on seed state"
    fi

    # Reference-patch assertion: applying task $t turns the suite green.
    if ! (cd "$work" && git apply --check "$ROOT/examples/$app/tasks/$t.patch" 2>/dev/null); then
      fail "$app task $t: reference patch does not apply"
    elif ! (cd "$work" && git apply "$ROOT/examples/$app/tasks/$t.patch" 2>/dev/null); then
      fail "$app task $t: reference patch failed to apply"
    elif "$runner" "$work"; then
      pass "$app task $t: acceptance checks pass after reference patch"
    else
      fail "$app task $t: acceptance checks still fail after reference patch"
      "$runner" "$work" 2>&1 | tail -15 || true
    fi

    commit_state "$work"
  done
}

main() {
  local app
  for app in gocli pylib webapi; do
    if [ ! -d "$ROOT/examples/$app" ]; then
      say "== $app: not present, skipping"
      continue
    fi
    check_app "$app"
  done
  say
  if [ "$FAILURES" -eq 0 ]; then
    say "samples-check: OK"
  else
    say "samples-check: $FAILURES failure(s)"
    exit 1
  fi
}

main "$@"
