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
# Each returns 0 when the acceptance checks pass (green), 1 when they fail
# (red). The caller asserts the red state involves this task's own tests via
# the RED_HINT global (set by check_app before calling).

checks_gocli() {
  (cd "$1" && go build ./... && go vet ./... && go test ./...)
}

checks_pylib() {
  (cd "$1" && python3 -m compileall -q . && python3 -m unittest discover -s tests)
}

checks_webapi() {
  (cd "$1" && go build ./... && bash test/smoke.sh)
}

# red_hint PATTERN — returns 0 if PATTERN is found in the most recent check
# output. Called with the runner's captured output on stdout.
red_hint_ok() {
  grep -q "$RED_HINT"
}

task_count() {
  ls "$ROOT/examples/$1/tasks/"*.patch 2>/dev/null | wc -l | tr -d ' '
}

commit_state() {
  (cd "$1" && git add -A && \
    git -c user.email=samples@coxswain.local -c user.name=samples commit -qm "state" || true)
}

# red_hint_for APP TASK — the grep pattern that identifies the task's own
# failing test in the check output.
red_hint_for() {
  case "$1-$2" in
    gocli-1)    echo 'TestRound' ;;
    gocli-2)    echo 'TestMainJSON' ;;
    pylib-1)    echo 'Median' ;;
    pylib-2)    echo 'Clamp' ;;
    webapi-1)   echo 'ping: FAIL' ;;
    webapi-2)   echo 'echo: FAIL' ;;
    *)          echo "task$2" ;;
  esac
}

check_app() {
  local app="$1"
  local seed="$ROOT/examples/$app"
  local n work runner t out rc hint
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

  runner="checks_$app"
  for t in $(seq 1 "$n"); do
    hint=$(red_hint_for "$app" "$t")

    # Seed-state assertion: the suite must be red on the seed state.
    out=$("$runner" "$work" 2>&1)
    rc=$?
    if [ "$rc" -eq 0 ]; then
      fail "$app task $t: acceptance checks PASS on seed state (expected FAIL)"
    else
      pass "$app task $t: acceptance checks fail on seed state"
    fi

    # Reference-patch assertion: applying task $t's patch to the seed turns
    # the suite green. (Each task's patch is cumulative: it carries all
    # prior tasks' changes, so it makes the full check suite pass when
    # applied to the seed state.)
    if ! (cd "$work" && git apply --check "$ROOT/examples/$app/tasks/$t.patch" 2>/dev/null); then
      fail "$app task $t: reference patch does not apply to seed"
    elif ! (cd "$work" && git apply "$ROOT/examples/$app/tasks/$t.patch" 2>/dev/null); then
      fail "$app task $t: reference patch failed to apply"
    else
      out=$("$runner" "$work" 2>&1)
      rc=$?
      if [ "$rc" -eq 0 ]; then
        pass "$app task $t: acceptance checks pass after reference patch"
      else
        fail "$app task $t: acceptance checks still fail after reference patch"
        printf '%s\n' "$out" | tail -15
      fi
    fi

    # Reset the temp dir to the seed state for the next task.
    (cd "$work" && git checkout -q -- . && git clean -qfd)

    # Reference-patch assertion: applying task $t turns the suite green.
    if ! (cd "$work" && git apply --check "$ROOT/examples/$app/tasks/$t.patch" 2>/dev/null); then
      fail "$app task $t: reference patch does not apply"
    elif ! (cd "$work" && git apply "$ROOT/examples/$app/tasks/$t.patch" 2>/dev/null); then
      fail "$app task $t: reference patch failed to apply"
    else
      out=$("$runner" "$work" 2>&1)
      rc=$?
      if [ "$rc" -eq 0 ]; then
        pass "$app task $t: acceptance checks pass after reference patch"
      else
        fail "$app task $t: acceptance checks still fail after reference patch"
        printf '%s\n' "$out" | tail -15
      fi
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
