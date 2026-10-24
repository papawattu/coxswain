#!/usr/bin/env bash
# F29: the Helm chart's CRDs must equal config/crd/bases (controller-gen
# output) in spec. A stale chart CRD makes the API server prune new fields on
# a Helm install (status.progress.lessonsAvailable was dropped, so the memory
# writer was skipped). Fix a failure with hack/chart-crd-sync.sh.
#
# Usage: hack/chart-crd-check.sh [chart-dir] [crd-bases-dir]
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
CHART="${1:-$ROOT/dist/chart}"
BASES="${2:-$ROOT/config/crd/bases}"
command -v helm >/dev/null 2>&1 || { echo "Error: helm not found"; exit 1; }
RENDERED="$(mktemp)"
trap 'rm -f "$RENDERED"' EXIT
helm template coxswain "$CHART" --namespace coxswain-system > "$RENDERED"
python3 - "$BASES" "$RENDERED" <<'PY'
import sys, glob, yaml

def find_diffs(a, b, path=""):
    """First differing paths between two YAML values (a = generated, b = chart)."""
    out = []
    if isinstance(a, dict) and isinstance(b, dict):
        for k in sorted(set(a) | set(b)):
            if k not in b:
                out.append("%s.%s missing from the chart" % (path, k))
            elif k not in a:
                out.append("%s.%s only in the chart" % (path, k))
            else:
                out += find_diffs(a[k], b[k], "%s.%s" % (path, k))
    elif isinstance(a, list) and isinstance(b, list) and len(a) == len(b):
        for i, (x, y) in enumerate(zip(a, b)):
            out += find_diffs(x, y, "%s[%d]" % (path, i))
    elif a != b:
        out.append("%s differs" % path)
    return out

gen = {}
for f in glob.glob(sys.argv[1] + "/*.yaml"):
    for d in yaml.safe_load_all(open(f)):
        if d and d.get("kind") == "CustomResourceDefinition":
            gen[d["metadata"]["name"]] = d
chart = {}
for d in yaml.safe_load_all(open(sys.argv[2])):
    if d and d.get("kind") == "CustomResourceDefinition":
        chart[d["metadata"]["name"]] = d
if not gen:
    print("FAIL: no CRDs under", sys.argv[1]); sys.exit(1)
bad = False
for name, g in sorted(gen.items()):
    c = chart.get(name)
    if c is None:
        print("FAIL: CRD %s is not rendered by the chart" % name); bad = True; continue
    diffs = find_diffs(g["spec"], c["spec"], "spec")
    if diffs:
        bad = True
        print("FAIL: chart CRD %s differs from config/crd/bases (%d diff(s)); first:" % (name, len(diffs)))
        for x in diffs[:8]:
            print("  " + x)
if bad:
    print("Run hack/chart-crd-sync.sh to regenerate the chart CRDs."); sys.exit(1)
print("OK: chart CRDs equal config/crd/bases (%s)" % ", ".join(sorted(gen)))
PY
