#!/usr/bin/env bash
# F19: every shipped manifest must pass --memory-writer-image set to the
# operator's OWN image (the memory-writer Job runs /usr/local/bin/coxswain-
# memory-write, which exists only in the operator image; without the flag the
# operator falls back to alpine/git and every lesson write fails at exec).
#
# Renders, in a temp copy (the tree is never edited):
#   - config/default, config/dev, config/calico after `kustomize edit set
#     image controller=<IMG>` (the `make deploy` path): the flag must equal
#     the manager container's image;
#   - dist/install.yaml via the real hack/build-installer.sh;
#   - dist/chart via `helm template` (default tag, and --set manager.image.tag).
# Each render is compared with ITS OWN manager image (not a hardcoded string),
# so dropping the flag, leaving IMG_PLACEHOLDER, or pointing at another image
# fails.
#
# Usage: [KUSTOMIZE=<path>] hack/memory-writer-image-test.sh
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
KUSTOMIZE="${KUSTOMIZE:-$(command -v kustomize || echo "$ROOT/bin/kustomize")}"
command -v "$KUSTOMIZE" >/dev/null 2>&1 || { echo "Error: kustomize not found (KUSTOMIZE=$KUSTOMIZE)"; exit 1; }
KUSTOMIZE="$(cd "$(dirname "$KUSTOMIZE")" && pwd)/$(basename "$KUSTOMIZE")"
command -v helm >/dev/null 2>&1 || { echo "Error: helm not found"; exit 1; }

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT
IMG="example.test/coxswain-controller:v0.0.0-f19"

# check <label> <rendered-file> <expected-image>: exactly one
# --memory-writer-image=<expected-image> arg, and the manager image matches.
check() {
	local label="$1" file="$2" want="$3" n
	n="$(grep -Fc -- "- --memory-writer-image=$want" "$file" || true)"
	if [ "$n" != "1" ]; then
		echo "FATAL[$label]: expected exactly one '--memory-writer-image=$want' (found $n)"
		grep -n -- "memory-writer-image" "$file" || echo "  (no --memory-writer-image in the render)"
		exit 1
	fi
	grep -Eq "^[[:space:]]+image: \"?${want}\"?$" "$file" \
		|| { echo "FATAL[$label]: no manager 'image: $want' in the render"; exit 1; }
	echo "OK[$label]: --memory-writer-image=$want"
}

# kustomize paths (make deploy / deploy-dev / the calico profile).
cp -r "$ROOT/config" "$TMP/config"
( cd "$TMP/config/manager" && "$KUSTOMIZE" edit set image controller="$IMG" )
for overlay in default dev calico; do
	"$KUSTOMIZE" build "$TMP/config/$overlay" > "$TMP/$overlay.yaml"
	check "kustomize config/$overlay" "$TMP/$overlay.yaml" "$IMG"
done

# dist/install.yaml via the real script (renders into $ROOT/dist; use a temp
# copy of the script tree so the working tree's dist/ is untouched).
mkdir -p "$TMP/inst/hack"
cp -r "$ROOT/config" "$TMP/inst/config"
cp "$ROOT/hack/build-installer.sh" "$ROOT/hack/install-args.patch.tpl" "$TMP/inst/hack/"
KUSTOMIZE="$KUSTOMIZE" IMG="$IMG" bash "$TMP/inst/hack/build-installer.sh" >/dev/null
check "install.yaml" "$TMP/inst/dist/install.yaml" "$IMG"

# F22: MEMORY_STORAGE_CLASS knob. Unset: no --memory-storage-class (default
# unchanged). Set: exactly one --memory-storage-class=<value>. Bad value: fails.
n="$(grep -c -- "--memory-storage-class" "$TMP/inst/dist/install.yaml" || true)"
[ "$n" = "0" ] || { echo "FATAL[F22]: unset knob still rendered $n --memory-storage-class args"; exit 1; }
KUSTOMIZE="$KUSTOMIZE" IMG="$IMG" MEMORY_STORAGE_CLASS=local-shared bash "$TMP/inst/hack/build-installer.sh" >/dev/null
n="$(grep -Fc -- "- --memory-storage-class=local-shared" "$TMP/inst/dist/install.yaml" || true)"
[ "$n" = "1" ] || { echo "FATAL[F22]: expected exactly one '--memory-storage-class=local-shared' (found $n)"; exit 1; }
check "install.yaml (MEMORY_STORAGE_CLASS=local-shared)" "$TMP/inst/dist/install.yaml" "$IMG"
if KUSTOMIZE="$KUSTOMIZE" IMG="$IMG" MEMORY_STORAGE_CLASS='a..b' bash "$TMP/inst/hack/build-installer.sh" >/dev/null 2>&1; then
	echo "FATAL[F22]: invalid MEMORY_STORAGE_CLASS a..b was accepted"; exit 1
fi
echo "OK[F22]: MEMORY_STORAGE_CLASS renders exactly one --memory-storage-class; unset renders none"

# the Helm chart.
helm template coxswain "$ROOT/dist/chart" --namespace coxswain-system > "$TMP/chart.yaml"
want="$(grep -E '^[[:space:]]+image: "ghcr.io/.*coxswain-controller:' "$TMP/chart.yaml" | head -1 | sed -E 's/.*image: "([^"]+)".*/\1/')"
[ -n "$want" ] || { echo "FATAL[chart]: could not find the manager image"; exit 1; }
check "helm dist/chart" "$TMP/chart.yaml" "$want"
helm template coxswain "$ROOT/dist/chart" --namespace coxswain-system --set manager.image.tag=v1.2.3 > "$TMP/chart2.yaml"
check "helm dist/chart (tag v1.2.3)" "$TMP/chart2.yaml" "ghcr.io/papawattu/coxswain-controller:v1.2.3"

echo "OK: every shipped manifest passes --memory-writer-image = the operator's own image"
