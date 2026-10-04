#!/usr/bin/env bash
# I51 (R20): fail the deploy targets unless IMG is set explicitly.
#
# An empty IMG makes kustomize render 'controller:latest' (the kustomization
# placeholder), which does not exist in kind; the rollout then stays Pending
# while the old pod keeps running with a stale --runner-image. Fail fast with
# a clear message instead.
#
# Usage: IMG=... TARGET=deploy|deploy-dev hack/require-img.sh
set -euo pipefail

if [ -z "${IMG:-}" ]; then
	printf 'Error: IMG is not set.\n' >&2
	printf 'An empty IMG would render controller:latest (the kustomization placeholder), which does not exist\n' >&2
	printf 'in kind and would strand the rollout (the old pod keeps running with a stale --runner-image).\n' >&2
	printf 'Set the built image tag, e.g.:  IMG=<registry>/<project>:tag make %s\n' "${TARGET:-deploy}" >&2
	exit 1
fi
