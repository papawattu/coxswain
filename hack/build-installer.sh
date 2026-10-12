#!/usr/bin/env bash
# A5 (PLAN-ALPHA): render dist/install.yaml with EVERY coxswain ghcr image
# tagged from IMG. The release workflow (.github/workflows/release.yml)
# packages ghcr.io/<owner>/coxswain-{controller,runner,model-proxy,tool-proxy};
# this renders install.yaml so the manager container image AND the
# --runner-image / --proxy-image / --tool-proxy-image args all carry the SAME
# tag as IMG (no controller:latest, no :standin).
#
# It renders in a TEMP overlay (never the working tree) so the working tree is
# left clean: kustomize v5.8.1 has no `build --images`, so a temp copy +
# `edit set image` + an args patch is the non-dirtying path. The `deploy`
# target keeps its own in-tree edit (local dev leaves the args empty).
#
# Usage: IMG=ghcr.io/<owner>/coxswain-controller:<tag> \
#        KUSTOMIZE=<path> hack/build-installer.sh
#
# The three ghcr arg image names are hardcoded to the release workflow's
# public prefix (papawattu/coxswain-*); only the TAG is derived from IMG.
# The egress proxy has no manager flag (cmd/main.go: --egress-proxy-image does
# not exist), so only the three flags that exist are tagged.
set -euo pipefail

# I51: fail unless IMG is set (the in-tree require-img.sh already runs in the
# Makefile target; keep the guard here so the script is safe standalone).
IMG="${IMG:-}"
if [ -z "$IMG" ]; then
	printf 'Error: IMG is not set. Usage: IMG=<registry>/coxswain-controller:<tag> KUSTOMIZE=<path> hack/build-installer.sh\n' >&2
	exit 1
fi
KUSTOMIZE="${KUSTOMIZE:-kustomize}"
command -v "$KUSTOMIZE" >/dev/null 2>&1 || { echo "Error: kustomize not found (KUSTOMIZE=$KUSTOMIZE)"; exit 1; }

# Derive the tag from IMG's last ':' component (awk, not ${##} — portable and
# immune to make's comment/`##` handling if ever inlined).
TAG="$(printf '%s' "$IMG" | awk -F: '{print $NF}')"
[ -n "$TAG" ] || { echo "Error: could not derive a tag from IMG=$IMG"; exit 1; }

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
OVERLAY="$(mktemp -d)"
trap 'rm -rf "$OVERLAY"' EXIT

cp -r "$ROOT/config" "$OVERLAY/config"

# Add the three ghcr image args (derived from IMG's tag) to the manager
# container via a strategic-merge patch. Rendered from the tree template with
# sed (a heredoc cannot appear inside a Makefile recipe line).
sed -e "s|__TAG__|$TAG|g" "$ROOT/hack/install-args.patch.tpl" \
	> "$OVERLAY/config/manager/install-args.patch.yaml"

# Wire the patch into the overlay's manager kustomization. Remove any existing
# `patches:` block from the copied kustomization (the in-tree file has none;
# this makes a re-run / dirty copy idempotent), then append ours.
sed -i '/^patches:$/d' "$OVERLAY/config/manager/kustomization.yaml" 2>/dev/null || true
# Drop any dangling `- path:` continuation lines from a removed block (none in
# the clean in-tree file; belt-and-suspenders for idempotency).
printf '\npatches:\n- path: install-args.patch.yaml\n  target:\n    kind: Deployment\n' \
	>> "$OVERLAY/config/manager/kustomization.yaml"

# Set the manager container image to IMG in the overlay.
( cd "$OVERLAY/config/manager" && "$KUSTOMIZE" edit set image controller="$IMG" )

mkdir -p "$ROOT/dist"
"$KUSTOMIZE" build "$OVERLAY/config/default" > "$ROOT/dist/install.yaml"
"$KUSTOMIZE" build "$OVERLAY/config/cni-probe" >> "$ROOT/dist/install.yaml"

echo "dist/install.yaml rendered with tag $TAG (controller=$IMG)"
