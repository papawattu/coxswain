#!/usr/bin/env bash
# I51 (R20): fail the build/deploy targets unless IMG is set explicitly.
# I63: the error text is TARGET-NEUTRAL — it mentions the rollout even for
# docker-build/docker-push (which don't roll out anything), so the message no
# longer names a specific deployment target.
#
# An empty IMG makes kustomize render 'controller:latest' (the kustomization
# placeholder), which does not exist in kind; a deployment then stays Pending
# while the old pod keeps running with a stale --runner-image. Fail fast with
# a clear message instead.
#
# Usage: IMG=... TARGET=deploy|deploy-dev|docker-build|docker-push hack/require-img.sh
set -euo pipefail

if [ -z "${IMG:-}" ]; then
	printf 'Error: IMG is not set.\n' >&2
	printf 'An empty IMG renders the kustomization placeholder (controller:latest) or builds an\n' >&2
	printf 'untagged image, neither of which exists in the cluster — a deployment would stay\n' >&2
	printf 'Pending while the old pod keeps running with a stale --runner-image.\n' >&2
	printf 'Set the built image tag, e.g.:  IMG=<registry>/<project>:tag make %s\n' "${TARGET:-deploy}" >&2
	exit 1
fi
