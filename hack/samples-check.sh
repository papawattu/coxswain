#!/usr/bin/env bash
# samples-check.sh — verify the SDLC sample apps (docs/SAMPLES-PLAN.md, S1).
#
# For each app in examples/ and each task in examples/<app>/tasks/<n>.patch:
#   1. Copy the seed state into a fresh temp dir (a git repo so git apply
#      works), run the task's scoped checks — they must FAIL on the seed.
#   2. Apply the task's reference patch (against the seed), re-run the
#      checks — they must all PASS.
#   3. Assert tasks.md's acceptanceChecks for this task equal <n>.checks.
#
# Each task runs against a FRESH seed copy, so the checks are scoped to
# that task's own tests only (no cumulative state). No cluster needed.
set -uo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
FAILURES=0

say()    { printf '%s\n' "$*"; }
pass()   { say "PASS: $*"; }
fail()   { say "FAIL: $*"; FAILURES=$((FAILURES + 1)); }

# run_checks DIR — run each line of DIR/.checks (one shell command per line,
# scoped to this task's tests). Stop at the first failure. Exit 0 if all pass.
run_checks() {
  local dir="$1" line
  while IFS= read -r line; do
    [ -z "$line" ] && continue
    if ! (cd "$dir" && eval "$line") >/dev/null 2>&1; then
      return 1
    fi
  done < "$dir/.checks"
  return 0
}

# md_checks APP TASK — print the acceptanceChecks list from tasks.md for this
# task (the YAML block between 'acceptanceChecks:' and the next non-list key).
md_checks() {
  local app="$1" task="$2"
  awk -v n="$task" '
    /^## Task [0-9]+/ { in_task = (index($0, "Task " n) > 0) }
    in_task && /acceptanceChecks:/ { grab = 1; next }
    grab && /^      - / { sub(/^      - /, ""); print; next }
    grab { grab = 0 }
  ' "$ROOT/examples/$app/tasks.md"
}

for app in gocli pylib webapi; do
  seed="$ROOT/examples/$app"
  say "== $app =="
  for patch in "$seed"/tasks/*.patch; do
    task=$(basename "$patch" .patch)
    checks="$seed/tasks/${task}.checks"
    if [ ! -f "$checks" ]; then
      fail "$app task $task: missing $checks"
      continue
    fi

    # Copy the seed into a fresh git repo so git apply works.
    work=$(mktemp -d)
    cp -r "$seed"/* "$work"/
    (cd "$work" && git init -q && git add -A && \
     git -c user.email=samples@coxswain.local -c user.name=samples \
     commit -qm seed || true)

    # Install the scoped checks for this task.
    cp "$checks" "$work/.checks"

    # 1. Seed state: the task's scoped checks must FAIL.
    if ! run_checks "$work"; then
      pass "$app task $task: seed FAILS (expected)"
    else
      fail "$app task $task: seed PASSES (expected FAIL)"
    fi

    # 2. Apply the reference patch (against the seed), checks must PASS.
    if ! (cd "$work" && git apply -- "$seed/tasks/${task}.patch" 2>/dev/null); then
      fail "$app task $task: reference patch does not apply to the seed"
    elif run_checks "$work"; then
      pass "$app task $task: reference patch PASSES"
    else
      fail "$app task $task: reference patch does not pass the checks"
    fi

    # 3. tasks.md acceptanceChecks for this task must equal <n>.checks.
    md_list=$(md_checks "$app" "$task")
    checks_list=$(sed '/^$/d' "$checks")
    if [ -z "$md_list" ]; then
      fail "$app task $task: tasks.md has no acceptanceChecks"
    elif [ "$md_list" = "$checks_list" ]; then
      pass "$app task $task: tasks.md acceptanceChecks match <n>.checks"
    else
      fail "$app task $task: tasks.md acceptanceChecks differ from <n>.checks"
      say "  tasks.md:    $md_list"
      say "  <n>.checks:  $checks_list"
    fi

    rm -rf "$work"
  done
done

if [ "$FAILURES" -eq 0 ]; then
  say "samples-check: OK"
  exit 0
else
  say "samples-check: $FAILURES failure(s)"
  exit 1
fi
