#!/usr/bin/env bash
# F29: execution test for hack/chart-rbac-check.sh and hack/chart-crd-check.sh.
# The real chart passes both; a copy with the serviceaccounts rule deleted, and
# a copy with status.progress.lessonsAvailable deleted from the CRD, each FAIL.
#
# F87 adds the same treatment for hack/chart-check.sh (the F45 one-effective-
# VAP-binding guard) and hack/chart-notes-check.sh (the approval NOTES text),
# and asserts that `make chart-drift-test` actually runs both guards — that is
# the property that was missing, since neither had a caller in CI.
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

# F87(a): the F45 non-default-release render guard (hack/chart-check.sh) also
# has to bite: a copy with an inert second ValidatingAdmissionPolicyBinding (the
# F45 defect) FAILS and names the cause. (The real chart's PASS is not repeated
# here: the chart-drift-test recipe runs chart-check.sh on the real chart just
# before this harness, and chart-check.sh is two helm renders plus two
# build-installer runs, so a third is waste - F87 review P3-3.)
rm -rf "$TMP/chart3"
cp -r "$ROOT/dist/chart" "$TMP/chart3"
mkdir -p "$TMP/chart3/templates/extras"
cat >"$TMP/chart3/templates/extras/dup-binding.yaml" <<'YAML'
apiVersion: admissionregistration.k8s.io/v1
kind: ValidatingAdmissionPolicyBinding
metadata:
  name: my-cox-coxswain-deny-ephemeral-containers
spec:
  policyName: my-cox-coxswain-deny-ephemeral-containers
  validationActions: ["Deny"]
YAML
if out="$("$ROOT/hack/chart-check.sh" "$TMP/chart3" 2>&1)"; then
	echo "FAIL: the render guard passed a chart with an inert second VAP binding"; exit 1
fi
echo "$out" | grep -q "expected exactly 1 ValidatingAdmissionPolicyBinding" || {
	echo "FAIL: the render guard failed but did not name the binding count:"; echo "$out"; exit 1
}
echo "OK: render guard fails (naming the binding count) on a chart with an inert second VAP binding"

# F87(b), F87(d): the chart NOTES guard. The pre-F87 RBAC text (a bare
# `--clusterrole loop-editor`, no pods/log, no --role= branch) FAILS and names
# the causes; the real NOTES passing is covered by the recipe line (F87 review
# P3-3). The write VERB is deliberately not asserted — F87(d) made it a status
# merge patch — so this fixture says "PATCH" and still fails, on the RBAC
# defects the guard does pin.
rm -rf "$TMP/chart4"
cp -r "$ROOT/dist/chart" "$TMP/chart4"
cat >"$TMP/chart4/templates/NOTES.txt" <<'NOTES'
Approval note:

  `cox approve <loop>` and `cox reject <loop> --reason <reason>` PATCH the
  Loop's status conditions (PlanApproved / PlanRejected).

    kubectl create rolebinding <name> --clusterrole loop-editor \
      --user=<user> -n coxswain-system
NOTES
if out="$("$ROOT/hack/chart-notes-check.sh" "$TMP/chart4" 2>&1)"; then
	echo "FAIL: the NOTES guard passed the pre-F87 NOTES text"; exit 1
fi
echo "$out" | grep -q "names a role that does not exist" || {
	echo "FAIL: the NOTES guard failed but did not name the role defect:"; echo "$out"; exit 1
}
echo "$out" | grep -q "pods/log" || {
	echo "FAIL: the NOTES guard did not name the missing peek-pod RBAC:"; echo "$out"; exit 1
}
echo "OK: NOTES guard fails (naming the role name and the missing pods/log rule) on the pre-F87 text"

# F87(a)/(b): the wiring. The guards above are called directly by this harness,
# so nothing else proves `make test` (what CI runs) calls them. `make -n`
# prints the chart-drift-test recipe without running it.
echo "=== wiring: make chart-drift-test must run the chart guards (F87) ==="
for guard in hack/chart-check.sh hack/chart-notes-check.sh; do
	if ! make -C "$ROOT" -n chart-drift-test 2>&1 | grep -q -- "$guard"; then
		echo "FAIL: chart-drift-test does not run $guard (the guard is dead in CI)"; exit 1
	fi
done
echo "OK: chart-drift-test runs hack/chart-check.sh and hack/chart-notes-check.sh"
