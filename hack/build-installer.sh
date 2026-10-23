#!/usr/bin/env bash
# A5 (PLAN-ALPHA): render dist/install.yaml with EVERY coxswain ghcr image
# tagged from IMG. The release workflow (.github/workflows/release.yml)
# packages ghcr.io/<owner>/coxswain-{controller,runner,model-proxy,egress-
# proxy,tool-proxy}; this renders install.yaml so the manager container image
# AND the --runner-image / --proxy-image / --tool-proxy-image / --egress-proxy
# -image args all carry the SAME tag as IMG (no controller:latest, no :standin).
#
# It renders in a TEMP overlay (never the working tree) so the working tree is
# left clean: kustomize v5.8.1 has no `build --images`, so a temp copy +
# `edit set image` + an args patch is the non-dirtying path. The `deploy`
# target keeps its own in-tree edit (local dev leaves the args empty).
#
# Usage: IMG=ghcr.io/<owner>/coxswain-controller:<tag> \
#        [MEMORY_STORAGE_CLASS=<class>] KUSTOMIZE=<path> hack/build-installer.sh
#
# F22: MEMORY_STORAGE_CLASS (optional) appends --memory-storage-class=<class>
# to the manager args in the temp overlay, so an install.yaml can enable the
# RWX memory PVC (see hack/rwx-storage.sh) without hand-patching the
# Deployment. Unset/empty = no flag (the cluster default class).
#
# The five ghcr arg image names are hardcoded to the release workflow's
# public prefix (papawattu/coxswain-*); only the TAG is derived from IMG.
# The args are APPENDED (JSON6902 op: add to /spec/.../args/-) so the base
# args are preserved (P1-1).
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

# Add the five ghcr image args (derived from IMG's tag) to the manager
# container via a JSON6902 patch (op: add, one per flag, appended to the args
# list so the base args survive). Rendered from the tree template with sed (a
# heredoc cannot appear inside a Makefile recipe line).
sed -e "s|__TAG__|$TAG|g" "$ROOT/hack/install-args.patch.tpl" \
	> "$OVERLAY/config/manager/install-args.patch.yaml"

# F22: optional --memory-storage-class. Must be a DNS-1123 subdomain (the same
# rule the operator and `cox memory import` enforce) so a typo cannot inject
# YAML into the patch.
MEMORY_STORAGE_CLASS="${MEMORY_STORAGE_CLASS:-}"
if [ -n "$MEMORY_STORAGE_CLASS" ]; then
	printf '%s' "$MEMORY_STORAGE_CLASS" | grep -Eq '^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$' \
		&& [ "${#MEMORY_STORAGE_CLASS}" -le 253 ] \
		|| { echo "Error: MEMORY_STORAGE_CLASS=$MEMORY_STORAGE_CLASS is not a valid StorageClass name"; exit 1; }
	printf -- '- op: add\n  path: /spec/template/spec/containers/0/args/-\n  value: --memory-storage-class=%s\n' \
		"$MEMORY_STORAGE_CLASS" >> "$OVERLAY/config/manager/install-args.patch.yaml"
fi

# Wire the JSON6902 patch into the overlay's manager kustomization. Remove any
# existing `patches:` block from the copied kustomization (the in-tree file has
# none; this makes a re-run / dirty copy idempotent), then append ours.
sed -i '/^patches:$/d' "$OVERLAY/config/manager/kustomization.yaml" 2>/dev/null || true
printf '\npatches:\n- path: install-args.patch.yaml\n  target:\n    kind: Deployment\n' \
	>> "$OVERLAY/config/manager/kustomization.yaml"

# Set the manager container image to IMG in the overlay.
( cd "$OVERLAY/config/manager" && "$KUSTOMIZE" edit set image controller="$IMG" )

mkdir -p "$ROOT/dist"
"$KUSTOMIZE" build "$OVERLAY/config/default" > "$ROOT/dist/install.yaml"
# I85: the admission (ValidatingAdmissionPolicy + binding) and the cni-probe
# (Namespace + SA + Role + RoleBinding) are standalone kustomizations built
# SEPARATELY from config/default. A `---` separator is prepended so the last
# doc of the previous build and the first doc of the appended build are
# distinct documents (without it, kubectl apply bundles them into one doc
# and fails on strict decoding).
{ echo '---'; "$KUSTOMIZE" build "$OVERLAY/config/admission"; } >> "$ROOT/dist/install.yaml"
{ echo '---'; "$KUSTOMIZE" build "$OVERLAY/config/cni-probe"; } >> "$ROOT/dist/install.yaml"

echo "dist/install.yaml rendered with tag $TAG (controller=$IMG)"
