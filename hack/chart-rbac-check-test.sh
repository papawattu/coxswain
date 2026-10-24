#!/usr/bin/env bash
# F29: execution test for hack/chart-rbac-check.sh. The real chart passes; a
# copy of the chart with the serviceaccounts rule deleted FAILS (and names it).
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
echo "OK: guard passes the real chart and fails (naming serviceaccounts) on a chart without the rule"
