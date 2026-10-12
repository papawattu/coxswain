#!/usr/bin/env bash
# D55 (REVIEW-PHASE1-R25.md, owner decision 2026-10-10): a sanctioned GitHub
# token file (a narrow exception to D48). The owner's fine-grained token,
# scoped to papawattu/coxswain-sandbox only, lives at
# ~/.config/coxswain/github-token (mode 600). This is the ONLY credential a
# script may read (D55). The token is piped into a kubernetes.io/basic-auth
# Secret (username=x-access-token) via --from-file — it is NEVER printed,
# echoed, cat'd, logged, or placed on a command line.
#
#   hack/github-secret.sh <namespace> [secret-name]
#
#   <namespace>   the namespace to create the Secret in (required)
#   [secret-name] the Secret name (default: git-cred)
#
# Override the token file with COXSWAIN_GITHUB_TOKEN_FILE (the test uses a
# fake file in a temp dir; never read the real file in a test).
set -euo pipefail

TOKEN_FILE="${COXSWAIN_GITHUB_TOKEN_FILE:-$HOME/.config/coxswain/github-token}"
USERNAME="x-access-token"
DEFAULT_SECRET_NAME="git-cred"

die() { printf 'github-secret FATAL: %s\n' "$*" >&2; exit 1; }

# ---------------------------------------------------------------------------
# Argument handling.
# ---------------------------------------------------------------------------
if [ "$#" -lt 1 ] || [ -z "${1:-}" ]; then
	die "usage: hack/github-secret.sh <namespace> [secret-name]"
fi
NS="$1"
SECRET_NAME="${2:-$DEFAULT_SECRET_NAME}"

# ---------------------------------------------------------------------------
# The token file: the ONLY credential this script may read (D55). It must
# exist and be non-empty. The path is checked (readability); the CONTENT is
# never read, printed, or echoed by this script — kubectl reads it via
# --from-file.
# ---------------------------------------------------------------------------
[ -f "$TOKEN_FILE" ] || die "token file missing"
# Non-empty: a zero-byte file would produce an empty password (a broken
# credential). -s is true only when the file exists AND has size > 0.
[ -s "$TOKEN_FILE" ] || die "token file missing"

# ---------------------------------------------------------------------------
# Render the Secret manifest (client-side, --dry-run) and apply it. The token
# is passed via --from-file (file contents -> the password key), never on the
# command line and never echoed. set +x is the default (the script has no
# 'set -x'); the manifest is piped to kubectl apply, not printed.
#
# D55 (owner, 2026-10-12): apply with --server-side --field-manager so that
# NO kubectl.kubernetes.io/last-applied-configuration annotation is stored on
# the object. The client-side apply writes that annotation, and because this
# Secret carries the base64 GitHub token, the annotation is a SECOND copy of
# the token on the cluster. Server-side apply with an explicit field-manager
# records only the managed fields (no last-applied snapshot).
# ---------------------------------------------------------------------------
kubectl create secret generic "$SECRET_NAME" \
	-n "$NS" \
	--type=kubernetes.io/basic-auth \
	--from-literal=username="$USERNAME" \
	--from-file=password="$TOKEN_FILE" \
	--dry-run=client -o yaml \
	| kubectl apply --server-side --field-manager=coxswain-github-secret -f -
