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

# F22: the MEMORY_STORAGE_CLASS / MANAGER_EXTRA_ARGS knobs. Every render clears
# both from the ambient env first (env -u), so an exported knob cannot skew the
# "unset" case.
inst() { env -u MEMORY_STORAGE_CLASS -u MANAGER_EXTRA_ARGS KUSTOMIZE="$KUSTOMIZE" IMG="$IMG" "$@" bash "$TMP/inst/hack/build-installer.sh"; }
count() { grep -Fc -- "$1" "$TMP/inst/dist/install.yaml" || true; }
# must_fail <label> <VAR=value>...: the render exits non-zero.
must_fail() {
	local label="$1"; shift
	if inst "$@" >/dev/null 2>&1; then echo "FATAL[F22]: $label was accepted"; exit 1; fi
}

# Unset: no --memory-storage-class (the default is unchanged).
inst >/dev/null
n="$(count "--memory-storage-class")"
[ "$n" = "0" ] || { echo "FATAL[F22]: unset knob rendered $n --memory-storage-class args"; exit 1; }
[ "$(count "--allow-unenforced")" = "0" ] || { echo "FATAL[F22]: a default (release-shaped) render carries --allow-unenforced*"; exit 1; }
# Set: exactly one.
inst MEMORY_STORAGE_CLASS=local-shared >/dev/null
n="$(count "- --memory-storage-class=local-shared")"
[ "$n" = "1" ] || { echo "FATAL[F22]: expected exactly one '--memory-storage-class=local-shared' (found $n)"; exit 1; }
check "install.yaml (MEMORY_STORAGE_CLASS=local-shared)" "$TMP/inst/dist/install.yaml" "$IMG"
must_fail "MEMORY_STORAGE_CLASS a..b" MEMORY_STORAGE_CLASS='a..b'
# P1: a value with embedded newlines whose FIRST line is valid (grep -E '^..$'
# matched per line) must be refused, not written into the patch.
inj=$'local-shared\n- op: replace\n  path: /spec/template/spec/containers/0/image\n  value: evil/img:x'
must_fail "MEMORY_STORAGE_CLASS with embedded newlines" MEMORY_STORAGE_CLASS="$inj"
must_fail "MEMORY_STORAGE_CLASS with a CR" MEMORY_STORAGE_CLASS=$'local-shared\r'
# MANAGER_EXTRA_ARGS.
inst MANAGER_EXTRA_ARGS="--allow-unenforced" >/dev/null 2>"$TMP/warn.txt"
n="$(count "- --allow-unenforced")"
[ "$n" = "1" ] || { echo "FATAL[F22]: expected exactly one '--allow-unenforced' (found $n)"; exit 1; }
[ "$(count "--allow-unenforced-network")" = "0" ] || { echo "FATAL[F22]: --allow-unenforced-network rendered"; exit 1; }
grep -q "^WARNING: rendering --allow-unenforced" "$TMP/warn.txt" || { echo "FATAL[F22]: no WARNING for --allow-unenforced"; exit 1; }
must_fail "MANAGER_EXTRA_ARGS '--x; rm'" MANAGER_EXTRA_ARGS='--x; rm'
must_fail "MANAGER_EXTRA_ARGS with a newline" MANAGER_EXTRA_ARGS=$'--allow-unenforced\n- op: remove'
must_fail "MANAGER_EXTRA_ARGS --allow-unenforced-network" MANAGER_EXTRA_ARGS='--allow-unenforced-network'
must_fail "MANAGER_EXTRA_ARGS --allow-unenforced-network=true" MANAGER_EXTRA_ARGS='--allow-unenforced --allow-unenforced-network=true'
must_fail "MANAGER_EXTRA_ARGS --runner-image" MANAGER_EXTRA_ARGS='--runner-image=evil/img:x'
must_fail "MANAGER_EXTRA_ARGS --memory-writer-image" MANAGER_EXTRA_ARGS='--memory-writer-image=evil/img:x'
must_fail "MANAGER_EXTRA_ARGS duplicate of a base flag" MANAGER_EXTRA_ARGS='--leader-elect'
must_fail "MANAGER_EXTRA_ARGS duplicate of the memory knob" MEMORY_STORAGE_CLASS=a MANAGER_EXTRA_ARGS='--memory-storage-class=b'
must_fail "MANAGER_EXTRA_ARGS repeated token" MANAGER_EXTRA_ARGS='--allow-unenforced --allow-unenforced'
# A glob in a token is matched literally (refused by the charset), never expanded.
touch "$TMP/inst/--x=zz"
( cd "$TMP/inst" && must_fail "MANAGER_EXTRA_ARGS glob" MANAGER_EXTRA_ARGS='--x=*' )
rm -f "$TMP/inst/--x=zz"
echo "OK[F22]: knobs render exactly one flag when set and none when unset; injection, network, image and duplicate tokens are refused"

# the Helm chart.
helm template coxswain "$ROOT/dist/chart" --namespace coxswain-system > "$TMP/chart.yaml"
want="$(grep -E '^[[:space:]]+image: "ghcr.io/.*coxswain-controller:' "$TMP/chart.yaml" | head -1 | sed -E 's/.*image: "([^"]+)".*/\1/')"
[ -n "$want" ] || { echo "FATAL[chart]: could not find the manager image"; exit 1; }
check "helm dist/chart" "$TMP/chart.yaml" "$want"
helm template coxswain "$ROOT/dist/chart" --namespace coxswain-system --set manager.image.tag=v1.2.3 > "$TMP/chart2.yaml"
check "helm dist/chart (tag v1.2.3)" "$TMP/chart2.yaml" "ghcr.io/papawattu/coxswain-controller:v1.2.3"

echo "OK: every shipped manifest passes --memory-writer-image = the operator's own image"
