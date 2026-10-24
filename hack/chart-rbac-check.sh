#!/usr/bin/env bash
# F29: the Helm chart's manager role must grant at least every
# (apiGroup, resource, verb) that config/rbac/role.yaml (generated from the
# kubebuilder markers) grants. P5b's serviceaccounts rule was missing from the
# chart, so memory writes were forbidden on any Helm install.
#
# Usage: hack/chart-rbac-check.sh [chart-dir] [role-yaml]
# Needs helm + python3 (PyYAML). Offline.
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
CHART="${1:-$ROOT/dist/chart}"
ROLE="${2:-$ROOT/config/rbac/role.yaml}"
command -v helm >/dev/null 2>&1 || { echo "Error: helm not found"; exit 1; }
command -v python3 >/dev/null 2>&1 || { echo "Error: python3 not found"; exit 1; }
RENDERED="$(mktemp)"
trap 'rm -f "$RENDERED"' EXIT
helm template coxswain "$CHART" --namespace coxswain-system > "$RENDERED"
python3 - "$ROLE" "$RENDERED" <<'PY'
import sys, yaml

def triples(rules):
    s = set()
    for r in rules or []:
        for g in r.get('apiGroups') or ['']:
            for res in r.get('resources') or []:
                for v in r.get('verbs') or []:
                    s.add((g, res, v))
    return s

want = set()
for d in yaml.safe_load_all(open(sys.argv[1])):
    if d and d.get('kind') in ('ClusterRole', 'Role'):
        want |= triples(d.get('rules'))
if not want:
    print("FAIL: no rules found in", sys.argv[1]); sys.exit(1)

have = set()
found = False
for d in yaml.safe_load_all(open(sys.argv[2])):
    if d and d.get('kind') in ('ClusterRole', 'Role') and str(d['metadata']['name']).endswith('manager-role'):
        found = True
        have |= triples(d.get('rules'))
if not found:
    print("FAIL: no manager-role in the rendered chart"); sys.exit(1)

missing = sorted(want - have)
if missing:
    print("FAIL: the chart manager role lacks %d rule(s) that config/rbac/role.yaml grants:" % len(missing))
    for g, r, v in missing:
        print("  group=%r resource=%s verb=%s" % (g, r, v))
    sys.exit(1)
print("OK: chart manager role covers all %d (group,resource,verb) grants of config/rbac/role.yaml (chart has %d)" % (len(want), len(have)))
PY
