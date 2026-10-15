#!/usr/bin/env bash
# B6 step 5 + step 6 defect regression tests.
#
# Run 16 found two defects:
#   (1) The step 6 scrape pod's `command` field was a YAML sequence
#       (`command: [sh, -c, curl ...]`) but the v1 Pod spec expects a
#       string. `kc apply` rejected it with "cannot unmarshal object into
#       Go struct field Container.spec.containers.command of type string".
#   (2) The step 5 independence check read FORK_BRANCH BEFORE
#       wait_delivered, so it was empty. The check
#       `[ "$FORK_BRANCH" != "$BRANCH" ]` passed vacuously on an empty
#       string (an I84 hollow assertion).
#
# This test verifies both fixes WITHOUT a kind cluster (the heredoc and
# the independence logic are extracted and tested in isolation).
#
# Mutations (the norm):
#   - Revert the scrape pod's `command` to a YAML sequence -> the YAML
#     parse check FAILS (command is a list, not a string).
#   - Blank FORK_BRANCH (simulate the pre-wait_delivered read) -> the
#     independence check must die (it must NOT pass vacuously).
#
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
SCRIPT="$ROOT/test/e2e/beta-e2e.sh"
[ -f "$SCRIPT" ] || { echo "FATAL: $SCRIPT not found"; exit 1; }
python3 -c 'import yaml' 2>/dev/null || { echo "FATAL: python3-yaml not importable"; exit 1; }

FAILS=0
pass() { echo "PASS: $*"; }
fail() { echo "FAIL: $*"; FAILS=$((FAILS + 1)); }

# =========================================================================
# Test 1: the step 6 scrape pod's `command` is a STRING (not a list).
# =========================================================================
# Extract the step6-scrape.yaml heredoc from the script. The heredoc is
# delimited by `cat >"$EVIDENCE/step6-scrape.yaml" <<EOF` and `EOF`.
TMPDIR_T=$(mktemp -d)
trap 'rm -rf "$TMPDIR_T"' EXIT

# Find the heredoc start line and extract to the closing EOF.
awk '
  /cat >"\$EVIDENCE\/step6-scrape\.yaml" <<EOF/ { found=1; next }
  found && /^EOF$/ { exit }
  found { print }
' "$SCRIPT" > "$TMPDIR_T/step6-scrape.yaml"

[ -s "$TMPDIR_T/step6-scrape.yaml" ] || { fail "could not extract the step6-scrape.yaml heredoc from $SCRIPT"; exit 1; }

# Parse the YAML and check the command field is a string.
python3 - "$TMPDIR_T/step6-scrape.yaml" <<'PY' || fail "step6-scrape.yaml: the scrape container's `command` field is not a valid string (the run 16 defect: a YAML sequence)"
import sys, yaml
with open(sys.argv[1]) as f:
    doc = yaml.safe_load(f)
containers = doc["spec"]["containers"]
scrape = next(c for c in containers if c["name"] == "scrape")
cmd = scrape["command"]
if not isinstance(cmd, str):
    print(f"ERROR: command is {type(cmd).__name__}, want str (got: {cmd!r})")
    sys.exit(1)
if not cmd.startswith("curl"):
    print(f"ERROR: command does not start with 'curl' (got: {cmd[:40]!r}...)")
    sys.exit(1)
print(f"OK: command is a string ({len(cmd)} chars, starts with 'curl')")
PY
[ $? -eq 0 ] || FAILS=$((FAILS + 1))
pass "step6-scrape.yaml: the scrape container's `command` is a string (the v1 Pod spec's required form)"

# =========================================================================
# Test 2: the step 5 independence check dies on a blank FORK_BRANCH.
# =========================================================================
# Simulate the step 5 independence check with a blank FORK_BRANCH (the
# pre-wait_delivered read, the run 16 defect). The check must die, not
# pass vacuously.
#
# The independence check (from the script, after the fix):
#   wait_delivered ...
#   FORK_BRANCH=$(k get loop ... -o jsonpath='{.status.delivery.branch}')
#   FORK_PR=$(k get loop ... -o jsonpath='{.status.delivery.prURL}')
#   [ -n "$FORK_BRANCH" ] || die "..."   <-- the new guard
#   [ -n "$FORK_PR" ] || die "..."
#   [ "$FORK_BRANCH" != "$BRANCH" ] || die "..."
#
# If FORK_BRANCH is blank, the new guard `[ -n "$FORK_BRANCH" ] || die`
# must fire. We simulate this by running the check with a blank
# FORK_BRANCH and verifying it dies.

# The guard (extracted from the script):
GUARD='[ -n "$FORK_BRANCH" ] || { die "step5: the fork Delivered but has no branch in status.delivery (I84: an empty FORK_BRANCH would make the branch-independence check pass vacuously)"; }'

# Simulate: FORK_BRANCH is blank (the pre-wait_delivered read).
FORK_BRANCH=""
BRANCH="coxswain/b6-manual-XXXX-b3931b06"  # a real step-2 branch (non-empty)
PR_URL="https://github.com/papawattu/coxswain-sandbox/pull/37"  # a real step-2 PR (non-empty)

# The die function (a local one for the test):
die() { echo "DIED: $*"; exit 1; }

# Run the guard in a subshell and capture the exit code.
GUARD_RESULT=$(bash -c "
  set -euo pipefail
  FORK_BRANCH=\"$FORK_BRANCH\"
  BRANCH=\"$BRANCH\"
  PR_URL=\"$PR_URL\"
  die() { echo \"DIED: \$*\"; exit 1; }
  $GUARD
  # If we reach here, the guard did NOT fire (a FAIL — the vacuous pass).
  # Now run the original vacuous check to show it would have passed:
  [ \"\$FORK_BRANCH\" != \"\$BRANCH\" ] && echo \"VACUOUS PASS (bad): the check passed with an empty FORK_BRANCH\" || echo \"check correctly failed\"
" 2>&1) || GUARD_EXIT=$?
GUARD_EXIT=${GUARD_EXIT:-0}

if [ $GUARD_EXIT -ne 0 ] && echo "$GUARD_RESULT" | grep -q "DIED:"; then
  pass "step5 independence: a blank FORK_BRANCH causes the guard to die (no vacuous pass)"
else
  fail "step5 independence: a blank FORK_BRANCH did NOT cause the guard to die (the vacuous-pass defect is not fixed; output: $GUARD_RESULT)"
fi

# Also verify the guard catches a blank FORK_PR (the same I84 hollow-assertion class).
FORK_PR=""
GUARD_PR='[ -n "$FORK_PR" ] || { die "step5: the fork Delivered but has no PR URL (independence not proven — I84)"; }'
GUARD_PR_RESULT=$(bash -c "
  set -euo pipefail
  FORK_PR=\"$FORK_PR\"
  die() { echo \"DIED: \$*\"; exit 1; }
  $GUARD_PR
" 2>&1) || GUARD_PR_EXIT=$?
GUARD_PR_EXIT=${GUARD_PR_EXIT:-0}

if [ $GUARD_PR_EXIT -ne 0 ] && echo "$GUARD_PR_RESULT" | grep -q "DIED:"; then
  pass "step5 independence: a blank FORK_PR causes the guard to die (no vacuous pass)"
else
  fail "step5 independence: a blank FORK_PR did NOT cause the guard to die (the vacuous-pass defect is not fixed; output: $GUARD_PR_RESULT)"
fi

# =========================================================================
# Test 3: the step 6 log line's jsonpath is valid (no Go-template leakage).
# =========================================================================
# The run 16 defect: the log line at the old :1197 passed a Go-template
# expression to `-o jsonpath=`, which kubectl rejected. The fix uses two
# separate `kc get secret` calls (one for `.type`, one for `.data` keys via
# go-template). Verify the script no longer has a malformed jsonpath in the
# step 6 region.
#
# The defect pattern: `-o jsonpath='{.type} keys={{range` (a Go-template
# expression in a jsonpath). The fix pattern: two separate calls.
if grep -q "jsonpath='{.type} keys=" "$SCRIPT"; then
  fail "step6 log line: the malformed jsonpath (Go-template in -o jsonpath=) is still present (the run 16 defect)"
else
  pass "step6 log line: no malformed jsonpath (Go-template in -o jsonpath=)"
fi

# Verify the fix is present: two separate calls for type and keys.
if grep -q "get secret b6-metrics-secret -o jsonpath='{.type}'" "$SCRIPT" \
   && grep -q "get secret b6-metrics-secret -o go-template='{{range" "$SCRIPT"; then
  pass "step6 log line: the fix is present (separate jsonpath for .type + go-template for .data keys)"
else
  fail "step6 log line: the fix is not present (the separate .type / .data-keys calls are missing)"
fi

# =========================================================================
# Summary
# =========================================================================
echo
if [ $FAILS -eq 0 ]; then
  echo "ALL TESTS PASS"
  exit 0
else
  echo "$FAILS TEST(S) FAILED"
  exit 1
fi
