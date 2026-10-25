#!/usr/bin/env bash
# B5a: verify the generated Helm chart (dist/chart) without a cluster.
#
# 1. helm lint dist/chart
# 2. helm template coxswain dist/chart (release namespace coxswain-system)
# 3. every (kind, name) the kustomize install.yaml carries (rendered by
#    hack/build-installer.sh) is present in the chart's rendered output —
#    so the chart is a superset of install.yaml (the chart may add
#    release-namespace templating, but must not drop a resource the
#    kustomize install ships).
#
# It runs offline (no kind, no cluster) so CI can run it before a helm
# binary exists in the image (it only needs helm + go + python3).
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
command -v helm >/dev/null 2>&1 || { echo "Error: helm not found (make install-helm)"; exit 1; }
command -v python3 >/dev/null 2>&1 || { echo "Error: python3 not found"; exit 1; }
BUILD_ERR="$(mktemp)"
trap 'rm -f "${BUILD_ERR:-}" "${RENDERED:-}"' EXIT

echo "=== helm lint dist/chart ==="
helm lint "$ROOT/dist/chart"

echo "=== helm template (release coxswain, namespace coxswain-system) ==="
RENDERED="$(mktemp)"
helm template coxswain "$ROOT/dist/chart" --namespace coxswain-system > "$RENDERED"

echo "=== install.yaml coverage (every install.yaml resource must be in the chart) ==="
# The B5a review follow-up: do not swallow build-installer's stderr — a build
# failure was previously hidden by '/dev/null' (only the rendered YAML went to
# stdout, which the coverage check then compared against a STALE install.yaml).
# Now the stderr is captured and shown on failure.
if ! IMG="${IMG:-ghcr.io/papawattu/coxswain-controller:placeholder}" \
   KUSTOMIZE="$(command -v kustomize 2>/dev/null || echo "$ROOT/bin/kustomize")" \
   "$ROOT/hack/build-installer.sh" 2>"$BUILD_ERR" ; then
	echo "Error: hack/build-installer.sh failed; its stderr:" >&2
	cat "$BUILD_ERR" >&2
	exit 1
fi

python3 - "$ROOT/dist/install.yaml" "$RENDERED" <<'EOF'
import sys, yaml

def docs(f):
    return [d for d in yaml.safe_load_all(open(f).read()) if d]

def keyset(ds):
    s = set()
    for d in ds:
        k = d.get('kind')
        n = (d.get('metadata') or {}).get('name')
        if k and n:
            s.add((k, n))
    return s

inst = keyset(docs(sys.argv[1]))
chart = keyset(docs(sys.argv[2]))
missing = sorted(inst - chart)
extra = sorted(chart - inst)

print(f"install.yaml resources: {len(inst)}, chart resources: {len(chart)}")
if missing:
    # The kubebuilder helm/v2-alpha generator skips the kustomize
    # `namespace` transformer (the chart assumes the release namespace
    # exists) and the helper Roles are opt-in (values.yaml
    # rbac.helpers.enabled defaults to false, while install.yaml ships
    # them enabled). Those four are the ONLY expected differences — the
    # chart's render must not drop anything else install.yaml carries.
    expected = {
        ('Namespace', 'coxswain-system'),  # release namespace: helm --create-namespace
        ('ClusterRole', 'coxswain-loop-admin-role'),   # rbac.helpers.enabled=false in the chart
        ('ClusterRole', 'coxswain-loop-editor-role'),  # rbac.helpers.enabled=false in the chart
        ('ClusterRole', 'coxswain-loop-viewer-role'),  # rbac.helpers.enabled=false in the chart
    }
    unexpected = [x for x in missing if x not in expected]
    if unexpected:
        print("FAIL: in install.yaml but NOT in the chart (beyond the expected release-namespace/helper differences):")
        for x in unexpected:
            print("   ", x)
        sys.exit(1)
    print("OK: every install.yaml (kind, name) is present in the chart render, or in the expected difference set:")
    for x in sorted(expected):
        print("   (expected)", x)
for x in extra:
    print("  (chart-only, expected: release-namespace templating)", x)
EOF

echo "=== second render at a non-default release name/namespace (F45) ==="
# A binding whose policyName matches no VAP is accepted by the API server and
# is silently inert; a cluster-scoped object carrying a namespace is ignored.
# Both only show at a release name other than "coxswain", so render one.
RENDERED2="$(mktemp)"
trap 'rm -f "${BUILD_ERR:-}" "${RENDERED:-}" "${RENDERED2:-}"' EXIT
helm template my-cox "$ROOT/dist/chart" --namespace ops > "$RENDERED2"
python3 - "$RENDERED2" <<'PYEOF'
import sys, yaml
from collections import Counter

docs = [d for d in yaml.safe_load_all(open(sys.argv[1]).read()) if d]
CLUSTER_SCOPED = {
    'Namespace', 'ClusterRole', 'ClusterRoleBinding', 'CustomResourceDefinition',
    'ValidatingAdmissionPolicy', 'ValidatingAdmissionPolicyBinding',
}
bad = []
vaps = {d['metadata']['name'] for d in docs if d.get('kind') == 'ValidatingAdmissionPolicy'}
for d in docs:
    if d.get('kind') == 'ValidatingAdmissionPolicyBinding':
        pn = d['spec']['policyName']
        if pn not in vaps:
            bad.append("binding %s names policy %s, not a VAP in this render (%s)" % (d['metadata']['name'], pn, sorted(vaps)))
dups = [k for k, n in Counter((d['kind'], d['metadata']['name'], d['metadata'].get('namespace', '')) for d in docs).items() if n > 1]
for k in dups:
    bad.append("duplicate object in one render: %s" % (k,))
for d in docs:
    if d.get('kind') in CLUSTER_SCOPED and 'namespace' in d['metadata']:
        bad.append("cluster-scoped %s/%s carries metadata.namespace=%s" % (d['kind'], d['metadata']['name'], d['metadata']['namespace']))
if not vaps:
    bad.append("no ValidatingAdmissionPolicy in the render (the guard would pass vacuously)")
if bad:
    print("FAIL: non-default-release render:")
    for b in bad:
        print("   ", b)
    sys.exit(1)
print("OK: %d objects; every VAP binding names a VAP in the render; no duplicates; no namespaced cluster-scoped object" % len(docs))
PYEOF

echo "chart-check: PASS"
