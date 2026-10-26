#!/usr/bin/env bash
# F56: derive the manager's four component-image args from IMG and print them
# as JSON6902 `op: add` entries for the manager container's args list.
#
# Why a script and not a kustomize `replacements` entry: the four components
# live in DIFFERENT repositories (coxswain-runner, coxswain-model-proxy,
# coxswain-tool-proxy, coxswain-egress-proxy), so a replacement could only copy
# the manager image's tag substring — and kustomize's delimiter/index split is
# not port-safe: with `IMG=localhost:5000/coxswain-controller:v1` a
# `delimiter: ":" index: 1` source yields "5000/coxswain-controller" as the
# "tag". The derivation has to happen where the last-colon split is done
# correctly (awk -F: '$NF'), exactly as hack/build-installer.sh already derives
# dist/install.yaml's tag.
#
# Rule: the component images live BESIDE the manager image — the same
# registry+path prefix (IMG up to its last '/'), each named coxswain-<component>,
# all carrying IMG's tag.
#   IMG=ghcr.io/papawattu/coxswain-controller:v1  -> ghcr.io/papawattu/coxswain-runner:v1
#   IMG=localhost:5000/coxswain-controller:v1     -> localhost:5000/coxswain-runner:v1
#   IMG=coxswain-controller:dev                   -> coxswain-runner:dev
#
# --memory-writer-image is NOT emitted here: config/manager/manager.yaml carries
# it as IMG_PLACEHOLDER and the kustomization's `replacements` entry fills it
# from the manager container's image (F19), so every render path gets it.
#
# Usage: IMG=<registry>/<project>:<tag> hack/image-args.sh
set -euo pipefail

IMG="${IMG:-}"
if [ -z "$IMG" ]; then
	printf 'Error: IMG is not set. Usage: IMG=<registry>/<project>:<tag> hack/image-args.sh\n' >&2
	exit 1
fi

# The tag is IMG's LAST ':' component, so a registry with a port keeps its tag
# (localhost:5000/x:v1 -> v1). If the last component still contains a '/', the
# image has no tag at all (localhost:5000/x -> "5000/x"); refuse it rather than
# derive a nonsense tag.
TAG="$(printf '%s' "$IMG" | awk -F: '{print $NF}')"
case "$TAG" in
*/* | "")
	printf 'Error: IMG=%s has no tag (expected <registry>/<project>:<tag>)\n' "$IMG" >&2
	exit 1
	;;
esac

# Everything before that tag, then everything before the repository's own name
# is the prefix the component images share.
REPO="$(printf '%s' "$IMG" | sed -E 's/:[^:\/]*$//')"
PREFIX="$(printf '%s' "$REPO" | sed -E 's#/[^/]*$##')"
if [ "$PREFIX" = "$REPO" ]; then
	PREFIX=""
fi

emit() {
	local flag="$1" component="$2" image
	if [ -n "$PREFIX" ]; then
		image="$PREFIX/coxswain-$component:$TAG"
	else
		image="coxswain-$component:$TAG"
	fi
	printf -- '- op: add\n  path: /spec/template/spec/containers/0/args/-\n  value: %s=%s\n' "$flag" "$image"
}

emit --runner-image runner
emit --proxy-image model-proxy
emit --tool-proxy-image tool-proxy
emit --egress-proxy-image egress-proxy
