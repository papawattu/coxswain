#!/usr/bin/env bash
# F29: execution test for hack/chart-rbac-check.sh and hack/chart-crd-check.sh.
# The real chart passes both; a copy with the serviceaccounts rule deleted, and
# a copy with status.progress.lessonsAvailable deleted from the CRD, each FAIL.
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT
"$ROOT/hack/chart-rbac-check.sh" >/dev/null || { echo "FAIL: the real chart must pass"; exit 1; }
cp -r "$ROOT/dist/chart" "$TMP/chart"
python3 - "$TMP/chart/templates/rbac/manager-role.yaml" <<'PY'
import sys, re
p = sys.argv[1]
s = open(p).read()
blk = "- apiGroups:\n  - \"\"\n  resources:\n  - serviceaccounts\n  verbs:\n  - create\n  - get\n  - list\n  - watch\n"
assert blk in s, "serviceaccounts rule not found in the chart role"
open(p, "w").write(s.replace(blk, ""))
PY
if out="$("$ROOT/hack/chart-rbac-check.sh" "$TMP/chart" 2>&1)"; then
	echo "FAIL: the guard passed a chart without the serviceaccounts rule"; exit 1
fi
echo "$out" | grep -q "resource=serviceaccounts" || { echo "FAIL: guard failed but did not name serviceaccounts:"; echo "$out"; exit 1; }
echo "OK: RBAC guard passes the real chart and fails (naming serviceaccounts) on a chart without the rule"

# CRD guard: the real chart passes; a copy with status.progress.lessonsAvailable
# deleted FAILS (and names it).
"$ROOT/hack/chart-crd-check.sh" >/dev/null || { echo "FAIL: the real chart CRDs must pass"; exit 1; }
rm -rf "$TMP/chart2"
cp -r "$ROOT/dist/chart" "$TMP/chart2"
python3 - "$TMP/chart2/templates/crd/loops.coxswain.wattu.com.yaml" <<'PY'
import sys, re
p = sys.argv[1]
s = open(p).read()
# delete the lessonsAvailable property block (a key at 18 spaces, with its indented body)
new, n = re.subn(r"\n {18}lessonsAvailable:\n(?: {19,}.*\n)+", "\n", s)
assert n == 1, "lessonsAvailable block not found exactly once (%d)" % n
open(p, "w").write(new)
PY
if out="$("$ROOT/hack/chart-crd-check.sh" "$TMP/chart2" 2>&1)"; then
	echo "FAIL: the CRD guard passed a chart without lessonsAvailable"; exit 1
fi
echo "$out" | grep -q "lessonsAvailable" || { echo "FAIL: CRD guard failed but did not name lessonsAvailable:"; echo "$out"; exit 1; }
echo "OK: CRD guard passes the real chart and fails (naming lessonsAvailable) on a chart without it"
