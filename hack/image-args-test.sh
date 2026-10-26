#!/usr/bin/env bash
# F56: every PRODUCTION-shaped render must pass all five image args to the
# manager, and the four component images must be derived from IMG port-safely.
#
# Before F56 the `make deploy` render (config/default) passed only
# --memory-writer-image: a deployed Loop ran the operator's `sleep infinity`
# stand-in (the runner image was unset) and the three proxy pods pulled
# `…:standin`/`…:metering` tags that were never pushed. The failure was silent
# on the object (no condition, no Event); F56 also adds the RunnerAvailable
# condition for the case where the args are still missing.
#
# Renders checked (each in a temp tree; the working tree is never edited):
#   - config/default via hack/render-deploy.sh (the `make deploy` path), for
#     three IMG shapes: the release shape, a bare local name, and a registry
#     WITH A PORT (the shape a kustomize `replacements` tag split gets wrong);
#   - dist/install.yaml via the real hack/build-installer.sh;
#   - dist/chart via `helm template` (the chart ships the four args with
#     :placeholder defaults that its values override; the release workflow
#     fails on a leftover placeholder).
#
# Usage: [KUSTOMIZE=<path>] hack/image-args-test.sh
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
KUSTOMIZE="${KUSTOMIZE:-$(command -v kustomize || echo "$ROOT/bin/kustomize")}"
command -v "$KUSTOMIZE" >/dev/null 2>&1 || { echo "Error: kustomize not found (KUSTOMIZE=$KUSTOMIZE)"; exit 1; }
KUSTOMIZE="$(cd "$(dirname "$KUSTOMIZE")" && pwd)/$(basename "$KUSTOMIZE")"
command -v helm >/dev/null 2>&1 || { echo "Error: helm not found"; exit 1; }

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

# check_five <label> <file> <manager-image> <component-prefix>
# Asserts exactly the five image args, each with the expected value, and that
# the manager container's image is the manager image itself.
check_five() {
	local label="$1" file="$2" manager="$3" prefix="$4" tag want n
	tag="$(printf '%s' "$manager" | awk -F: '{print $NF}')"
	for pair in \
		"--runner-image=coxswain-runner" \
		"--proxy-image=coxswain-model-proxy" \
		"--tool-proxy-image=coxswain-tool-proxy" \
		"--egress-proxy-image=coxswain-egress-proxy"; do
		flag="${pair%%=*}"
		repo="${pair#*=}"
		if [ -n "$prefix" ]; then want="$flag=$prefix/$repo:$tag"; else want="$flag=$repo:$tag"; fi
		n="$(grep -Fc -- "$want" "$file" || true)"
		if [ "$n" != "1" ]; then
			echo "FATAL[$label]: expected exactly one '$want' (found $n)"
			grep -n -- "- $flag=" "$file" || echo "  (no $flag arg in the render)"
			exit 1
		fi
	done
	n="$(grep -Fc -- "--memory-writer-image=$manager" "$file" || true)"
	[ "$n" = "1" ] || { echo "FATAL[$label]: expected exactly one '--memory-writer-image=$manager' (found $n)"; exit 1; }
	# Compare the rendered image value EXACTLY (no regex built from the image
	# string: dots, ports and '@' would all be regex metacharacters).
	grep -E "^[[:space:]]+image: " "$file" \
		| sed -E 's/^[[:space:]]+image: //; s/^"//; s/"$//' \
		| grep -Fxq -- "$manager" \
		|| { echo "FATAL[$label]: no manager 'image: $manager' in the render"; exit 1; }
	# The base args must survive (P1-1: the patch appends; it must not replace).
	for base in --metrics-bind-address=:8443 --leader-elect --health-probe-bind-address=:8081; do
		grep -Fqc -- "- $base" "$file" >/dev/null \
			|| { echo "FATAL[$label]: base arg '$base' missing (the patch must append, not replace)"; exit 1; }
	done
	echo "OK[$label]: five image args + base args"
}

# 1. The `make deploy` render (config/default) for the three IMG shapes.
render_deploy() { IMG="$1" KUSTOMIZE="$KUSTOMIZE" bash "$ROOT/hack/render-deploy.sh"; }

IMG_REL="ghcr.io/papawattu/coxswain-controller:v9.9.9-f56"
render_deploy "$IMG_REL" > "$TMP/default-rel.yaml"
check_five "deploy (release IMG)" "$TMP/default-rel.yaml" "$IMG_REL" "ghcr.io/papawattu"

IMG_LOCAL="coxswain-controller:dev"
render_deploy "$IMG_LOCAL" > "$TMP/default-local.yaml"
check_five "deploy (bare local IMG)" "$TMP/default-local.yaml" "$IMG_LOCAL" ""

# F56: the shape a kustomize `replacements` tag split gets wrong — the tag must
# stay the tag, never "5000/coxswain-controller".
IMG_PORTED="localhost:5000/coxswain-controller:v1"
render_deploy "$IMG_PORTED" > "$TMP/default-ported.yaml"
check_five "deploy (ported registry IMG)" "$TMP/default-ported.yaml" "$IMG_PORTED" "localhost:5000"
# The mangled form a `delimiter: ":" index: 1` source produces puts the
# registry PATH where the tag belongs (…coxswain-runner:5000/coxswain-controller).
# Check the component args' TAG itself (text after the last ':') contains no
# '/' — the manager's own image legitimately has a port in its path, so the
# check is per component flag.
for flag in --runner-image --proxy-image --tool-proxy-image --egress-proxy-image; do
	line="$(grep -- "- $flag=" "$TMP/default-ported.yaml" || true)"
	[ -n "$line" ] || { echo "FATAL[deploy ported]: $flag missing"; exit 1; }
	value="${line##*"$flag="}"
	value="${value%% *}"
	argtag="${value##*:}"
	case "$argtag" in
	*/*)
		echo "FATAL[deploy ported]: $flag tag is '$argtag' (the registry path leaked into the tag)"
		exit 1
		;;
	esac
	[ "$argtag" = "v1" ] || { echo "FATAL[deploy ported]: $flag tag is '$argtag', expected v1"; exit 1; }
done

# F56 review: an IMG with no tag must be refused, not rendered with a nonsense
# tag. Three shapes, each of which produced (or could produce) a bogus tag:
#   - a ported registry with no tag: the "tag" would be "5000/coxswain-controller"
#   - a BARE name with no tag: coxswain-controller would become
#     coxswain-runner:coxswain-controller (the last ':' component is the whole
#     string, so the tag check alone does not see it)
#   - a digest reference: its ":' component is part of the digest
for bad in "localhost:5000/coxswain-controller" "coxswain-controller" \
	"ghcr.io/papawattu/coxswain-controller@sha256:0123456789abcdef"; do
	if render_deploy "$bad" >"$TMP/bad.yaml" 2>"$TMP/bad.err"; then
		echo "FATAL: IMG='$bad' (no tag / digest) was rendered"; head -3 "$TMP/bad.yaml"; exit 1
	fi
	[ -s "$TMP/bad.yaml" ] && { echo "FATAL: IMG='$bad' was refused but still rendered output"; exit 1; }
	grep -qE "has no tag|digest reference" "$TMP/bad.err" \
		|| { echo "FATAL: the refusal for IMG='$bad' did not explain itself"; cat "$TMP/bad.err"; exit 1; }
done
echo "OK[refusals]: untagged (ported, bare) and digest IMGs are refused with a message"

# 2. dist/install.yaml via the real script (temp tree, working tree untouched).
mkdir -p "$TMP/inst/hack"
cp -r "$ROOT/config" "$TMP/inst/config"
cp "$ROOT/hack/build-installer.sh" "$ROOT/hack/image-args.sh" "$TMP/inst/hack/"
KUSTOMIZE="$KUSTOMIZE" IMG="$IMG_REL" bash "$TMP/inst/hack/build-installer.sh" >/dev/null
check_five "install.yaml" "$TMP/inst/dist/install.yaml" "$IMG_REL" "ghcr.io/papawattu"

# 3. The chart: the four args must be present in its default render (the chart's
#    values default them to :placeholder for the release workflow to override).
helm template coxswain "$ROOT/dist/chart" --namespace coxswain-system > "$TMP/chart.yaml"
for flag in --runner-image --proxy-image --tool-proxy-image --egress-proxy-image; do
	n="$(grep -Fc -- "- $flag=" "$TMP/chart.yaml" || true)"
	[ "$n" = "1" ] || { echo "FATAL[chart]: expected exactly one '$flag=' arg (found $n)"; grep -n -- "$flag" "$TMP/chart.yaml" || true; exit 1; }
done
echo "OK[chart]: the four component args are present"

echo "OK: every production-shaped render passes all five image args, derived port-safely"
