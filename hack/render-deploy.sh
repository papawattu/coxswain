#!/usr/bin/env bash
# F56: render the config/default overlay for `make deploy`, with the manager's
# four component-image args set from IMG (hack/image-args.sh).
#
# Before F56 `make deploy` passed NONE of --runner-image / --proxy-image /
# --tool-proxy-image / --egress-proxy-image, so a deployed Loop ran the
# operator's `sleep infinity` stand-in instead of the runner, and the three
# proxy pods pulled `…:standin`/`…:metering` tags that were never pushed.
#
# The render happens in a TEMP overlay (never the working tree): the in-tree
# `cd config/manager && kustomize edit set image controller=$IMG` step that the
# old deploy target ran left the tree dirty, and the args patch cannot live
# in-tree (it is derived from IMG at render time).
#
# Usage: IMG=<registry>/<project>:<tag> [KUSTOMIZE=<path>] hack/render-deploy.sh
# Output: the rendered config/default YAML on stdout.
set -euo pipefail

IMG="${IMG:-}"
if [ -z "$IMG" ]; then
	printf 'Error: IMG is not set. Usage: IMG=<registry>/<project>:<tag> hack/render-deploy.sh\n' >&2
	exit 1
fi

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
KUSTOMIZE="${KUSTOMIZE:-$(command -v kustomize || echo "$ROOT/bin/kustomize")}"
command -v "$KUSTOMIZE" >/dev/null 2>&1 || { echo "Error: kustomize not found (KUSTOMIZE=$KUSTOMIZE)"; exit 1; }

OVERLAY="$(mktemp -d)"
trap 'rm -rf "$OVERLAY"' EXIT
cp -r "$ROOT/config" "$OVERLAY/config"

# The four component args, derived from IMG (port-safe; see the script).
IMG="$IMG" bash "$ROOT/hack/image-args.sh" > "$OVERLAY/config/manager/install-args.patch.yaml"

# Wire the JSON6902 patch into the overlay's manager kustomization. Removing any
# existing `patches:` block first keeps a re-run idempotent.
sed -i '/^patches:$/d' "$OVERLAY/config/manager/kustomization.yaml" 2>/dev/null || true
printf '\npatches:\n- path: install-args.patch.yaml\n  target:\n    kind: Deployment\n' \
	>> "$OVERLAY/config/manager/kustomization.yaml"

# The manager container's image (and, via the F19 replacement, its
# --memory-writer-image).
( cd "$OVERLAY/config/manager" && "$KUSTOMIZE" edit set image controller="$IMG" )

"$KUSTOMIZE" build "$OVERLAY/config/default"
