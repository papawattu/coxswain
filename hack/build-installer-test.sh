#!/usr/bin/env bash
# A5 execution test: render dist/install.yaml via the REAL build-installer
# script for a given tag and assert EVERY coxswain image in it carries that
# tag (no :standin, no :dev, no :latest, no untagged). Runs the real
# hack/build-installer.sh (temp overlay, kustomize v5.8.1) against a temp
# kustomize so the working tree's dist/ is untouched.
#
# Asserts, for the rendered install.yaml:
#   1. the manager container image is <controller-with-tag>;
#   2. --runner-image is ghcr.io/<prefix>/coxswain-runner:<tag>;
#   3. --proxy-image is ghcr.io/<prefix>/coxswain-model-proxy:<tag>;
#   4. --tool-proxy-image is ghcr.io/<prefix>/coxswain-tool-proxy:<tag>;
#   5. --egress-proxy-image is ghcr.io/<prefix>/coxswain-egress-proxy:<tag>;
#   6. the manager's BASE args are still present (P1-1: the patch must
#      APPEND, not replace, the container args — a strategic-merge patch on
#      args would drop --health-probe-bind-address and crash-loop the operator);
#   7. no coxswain-* image in the file is :standin, :dev, :latest, or an
#      untagged dev image (scoped to coxswain component images, not
#      third-party images like python:3-alpine in the cni-probe).
#
# Usage: KUSTOMIZE=<path> [TAG=<tag>] [PREFIX=<prefix>] hack/build-installer-test.sh
# TAG defaults to a clearly-fake value so a real tag never leaks in; PREFIX
# defaults to the release workflow's public prefix (papawattu).
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
KUSTOMIZE="${KUSTOMIZE:-$(command -v kustomize || echo "$ROOT/bin/kustomize")}"
command -v "$KUSTOMIZE" >/dev/null 2>&1 || { echo "Error: kustomize not found (KUSTOMIZE=$KUSTOMIZE)"; exit 1; }
# The script re-derives its ROOT to the temp dir, so a relative kustomize path
# would break there; absolutize it first.
KUSTOMIZE="$(cd "$(dirname "$KUSTOMIZE")" && pwd)/$(basename "$KUSTOMIZE")"

TAG="${TAG:-v9.9.9-test}"
PREFIX="${PREFIX:-papawattu}"

# Render into a TEMP output (do not touch the working tree's dist/).
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT
cp -r "$ROOT/config" "$TMP/config"
# Run the real build-installer script but redirected to the temp tree by
# pointing it at a temp ROOT. The script derives ROOT from its own location,
# so copy the script + template into the temp tree and run them there.
mkdir -p "$TMP/hack"
cp "$ROOT/hack/build-installer.sh" "$TMP/hack/build-installer.sh"
cp "$ROOT/hack/install-args.patch.tpl" "$TMP/hack/install-args.patch.tpl"
KUSTOMIZE="$KUSTOMIZE" IMG="ghcr.io/$PREFIX/coxswain-controller:$TAG" bash "$TMP/hack/build-installer.sh" >/dev/null
# The script writes dist/install.yaml under ITS OWN root ($TMP).
OUT="$TMP/dist/install.yaml"
[ -s "$OUT" ] || { echo "FATAL: no rendered install.yaml produced"; exit 1; }

# 1. manager container image carries the tag.
want_img="ghcr.io/$PREFIX/coxswain-controller:$TAG"
[ "$(grep -c "image: $want_img" "$OUT")" -ge 1 ] \
	|| { echo "FATAL: manager container image '$want_img' not found in install.yaml"; grep -n 'image: ghcr.io' "$OUT" || true; exit 1; }

# 2-5. the five ghcr arg images carry the tag.
for img in \
	"--runner-image=ghcr.io/$PREFIX/coxswain-runner:$TAG" \
	"--proxy-image=ghcr.io/$PREFIX/coxswain-model-proxy:$TAG" \
	"--tool-proxy-image=ghcr.io/$PREFIX/coxswain-tool-proxy:$TAG" \
	"--egress-proxy-image=ghcr.io/$PREFIX/coxswain-egress-proxy:$TAG"; do
	[ "$(grep -Fc -- "$img" "$OUT")" -ge 1 ] \
		|| { echo "FATAL: expected arg '$img' not found in install.yaml"; grep -n -- "$img" "$OUT" || true; exit 1; }
done

# 6. (P1-1) the manager's BASE args must still be present — the patch APPENDS
#    to the args list, it must not replace it. Dropping --health-probe-bind-
#    address would make the liveness/readiness probes on :8081 fail (the
#    released operator would crash-loop); dropping --leader-elect changes HA
#    behaviour. Assert each base arg is present.
for base in \
	"--metrics-bind-address=:8443" \
	"--leader-elect" \
	"--health-probe-bind-address=:8081"; do
	[ "$(grep -Fc -- "$base" "$OUT")" -ge 1 ] \
		|| { echo "FATAL: base manager arg '$base' was REPLACED/LOST by the patch (P1-1: the patch must append, not replace, the container args)"; grep -n -- "args:" "$OUT" || true; exit 1; }
done

# 7. no coxswain component image is :standin, :dev, :latest, or untagged.
#    Scope to lines that reference a coxswain component image (the assertion
#    is on the coxswain component images, not third-party images like
#    python:3-alpine in the cni-probe).
if grep -nE 'coxswain-(controller|runner|model-proxy|egress-proxy|tool-proxy)[[:space:]]*:$' "$OUT" >/dev/null; then
	echo "FATAL: an untagged coxswain image (no tag) is present in install.yaml"; grep -nE 'coxswain-(controller|runner|model-proxy|egress-proxy|tool-proxy)[[:space:]]*:$' "$OUT"; exit 1
fi
if grep -nE 'coxswain-(controller|runner|model-proxy|egress-proxy|tool-proxy):(latest|standin|dev)[[:space:]]*$' "$OUT" >/dev/null; then
	echo "FATAL: a coxswain image tagged :latest, :standin, or :dev is present in install.yaml"; grep -nE 'coxswain-(controller|runner|model-proxy|egress-proxy|tool-proxy):(latest|standin|dev)[[:space:]]*$' "$OUT"; exit 1
fi

# 8. (I85) the rendered install.yaml contains BOTH the ValidatingAdmissionPolicy
#    AND its ValidatingAdmissionPolicyBinding, and the binding's policyName
#    matches the VAP's name. The VAP is cluster-scoped (no namespace field);
#    the binding is namespaced (namespace: coxswain-system). A missing binding
#    means the admission gate is off in every release (the policy exists but
#    is not bound, so it never fires).
i85_check=$(python3 - "$OUT" <<'I85PY'
import yaml, sys
path = sys.argv[1]
VAP_NAME = "coxswain-deny-ephemeral-containers"
docs = list(yaml.safe_load_all(open(path)))
vap = None
binding = None
for d in docs:
    if not d:
        continue
    if d.get("kind") == "ValidatingAdmissionPolicy" and d.get("metadata", {}).get("name") == VAP_NAME:
        vap = d
    if d.get("kind") == "ValidatingAdmissionPolicyBinding" and d.get("metadata", {}).get("name") == VAP_NAME:
        binding = d
if vap is None:
    print("FATAL: ValidatingAdmissionPolicy '%s' is missing from install.yaml" % VAP_NAME)
    sys.exit(1)
ns = vap.get("metadata", {}).get("namespace", "")
if ns:
    print("FATAL: VAP %s has namespace=%s (cluster-scoped resources must not have a namespace)" % (VAP_NAME, ns))
    sys.exit(1)
if binding is None:
    print("FATAL: ValidatingAdmissionPolicyBinding '%s' is missing from install.yaml (the admission gate is off - the policy exists but is not bound)" % VAP_NAME)
    sys.exit(1)
binding_ns = binding.get("metadata", {}).get("namespace", "")
if binding_ns != "coxswain-system":
    print("FATAL: the binding's namespace is '%s' (expected 'coxswain-system')" % binding_ns)
    sys.exit(1)
policy_name = binding.get("spec", {}).get("policyName", "")
if policy_name != VAP_NAME:
    print("FATAL: the binding's policyName=%s does not match the VAP's name=%s" % (policy_name, VAP_NAME))
    sys.exit(1)
print("OK: VAP (cluster-scoped, no namespace) + binding (namespaced, policyName matches) both present")
sys.exit(0)
I85PY
) || { echo "$i85_check"; exit 1; }

echo "OK: build-installer renders install.yaml with tag $TAG on all five coxswain images (controller + runner + model-proxy + tool-proxy + egress-proxy), preserves the base manager args, and includes BOTH the ValidatingAdmissionPolicy (cluster-scoped, no namespace) and its ValidatingAdmissionPolicyBinding (namespaced, policyName matches) (I85): $i85_check"
