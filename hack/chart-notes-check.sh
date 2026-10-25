#!/usr/bin/env bash
# F87(b): the Helm chart's approval NOTES must teach the RBAC and the write the
# CLI actually uses. #249 (F45 P3-2) corrected dist/chart/README.md but not
# templates/NOTES.txt, so a Helm user still read: `cox approve`/`reject` "PATCH
# the Loop's status conditions", and a rolebinding command naming a role called
# `loop-editor` — the rendered helper role is `<fullname>-loop-editor-role`
# (templates/rbac/loop-editor-role.yaml via coxswain.resourceName), so the
# printed `--clusterrole loop-editor` failed with NotFound.
#
# `helm template` does NOT render NOTES.txt (notes are printed by helm
# install/upgrade), and `helm install --dry-run` needs a cluster, so this guard
# reads the template SOURCE and asserts stable facts, not sentences:
#
#  1. every --role=/--clusterrole= value is a Helm action (a templated name),
#     never a bare literal role name like `loop-editor`;
#  2. they name the `loops/status` subresource and the two conditions the CLI
#     writes (`PlanApproved`, `PlanRejected`);
#  3. they mention the peek pod's `pods/log` read;
#  4. they carry both forms — `--role=` and `--clusterrole=` — because
#     rbac.namespaced renders a Role or a ClusterRole.
#
# Deliberately NOT asserted: the write's verb. F87(d) changed it from a
# whole-object status PUT to a status merge patch (`patch` on loops/status), and
# a guard that pinned either verb would have had to be reworded to stay green —
# which is how a doc ends up teaching a write the CLI no longer makes. The
# stable claims are the subresource and the conditions.
#
# Template SYNTAX is not this guard's job and does not need one: `helm lint`
# parses NOTES.txt and exits 1 on a broken action (verified: a broken
# `{{- if }}` gives "parse error at (coxswain/templates/NOTES.txt:24)" and
# `helm lint` exit 1), and `make test` runs `helm lint` through
# hack/chart-check.sh.
#
# Usage: hack/chart-notes-check.sh [chart-dir]   (default: dist/chart)
# Needs only bash + python3. Offline.
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
CHART="${1:-$ROOT/dist/chart}"
NOTES="$CHART/templates/NOTES.txt"
command -v python3 >/dev/null 2>&1 || { echo "Error: python3 not found"; exit 1; }
[ -f "$NOTES" ] || { echo "FAIL: $NOTES does not exist (the chart must render an approval note)"; exit 1; }
python3 - "$NOTES" <<'PY'
import re
import sys

path = sys.argv[1]
text = open(path).read()
problems = []

# 1. No bare role name: a --role/--clusterrole value that is not templated.
for m in re.finditer(r'--(?:cluster)?role(?:=| )(.*)', text):
    argv = m.group(0).strip()
    value = m.group(1).strip()
    if not value.startswith('{{'):
        problems.append("NOTES names a role that does not exist (%s is a literal role name; the "
                        "rendered helper role is <fullname>-loop-editor-role, so name it with the "
                        "coxswain.resourceName helper)" % argv)

# 2. The subresource and the conditions the CLI writes.
if 'loops/status' not in text:
    problems.append("NOTES does not name the loops/status subresource")
for cond in ('PlanApproved', 'PlanRejected'):
    if cond not in text:
        problems.append("NOTES does not name the %s condition it sets" % cond)

# 3. The peek pod's log read.
if 'pods/log' not in text:
    problems.append("NOTES does not mention the peek pod RBAC (get on pods/log)")

# 4. Both rbac.namespaced branches.
if '--role=' not in text:
    problems.append("NOTES has no --role= form (the rbac.namespaced=true Role branch)")
if '--clusterrole=' not in text:
    problems.append("NOTES has no --clusterrole= form (the default ClusterRole branch)")

if problems:
    print("FAIL: chart NOTES approval RBAC (%s):" % path)
    for p in problems:
        print("   ", p)
    sys.exit(1)
print("OK: chart NOTES teach the loops/status write of the PlanApproved/PlanRejected "
      "conditions, the templated helper role name, the pods/log peek-pod rule, and both "
      "rbac.namespaced branches")
PY
