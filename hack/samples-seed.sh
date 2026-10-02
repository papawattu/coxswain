#!/usr/bin/env bash
# S2 (samples plan §2): seed the in-cluster Gitea with the sample app repos.
#
# For each examples/<app>, this script:
#   1. creates the Gitea repo <app> through the admin REST API (deletes a
#      stale repo first, so the target is idempotent),
#   2. ensures the 'samples' Gitea user exists and its password matches the
#      one in the samples-git-cred Secret (the credential the Loops'
#      gitCredentialSecret points at),
#   3. pushes the seed as ONE 'initial' commit, from a TEMP git copy of
#      examples/<app> — never from the coxswain repo itself (the coxswain
#      repo's origin would otherwise leak into the seeded repo; the seeded
#      repos must point at nothing — samples plan §2),
#   4. verifies the result by reading refs/heads/initial back through git
#      with the samples credential (the same credential the Loops will use).
#
# The coxswain repo itself is never modified: the copy is made into a temp
# dir, committed there, and the temp dir is deleted on exit.
#
# Usage: samples-seed.sh <ctx> <ns> <app>...
# (CTX/NS are pinned by the caller, hack/samples-git.sh.)
set -euo pipefail

CTX="${1:?ctx required}"
NS="${2:?ns required}"
shift 2
APPS=("$@")
[ ${#APPS[@]} -gt 0 ] || APPS=(gocli pylib webapi)

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

KUBECTL=(kubectl --context "$CTX" -n "$NS")

log() { printf '\033[1;36m[samples-seed]\033[0m %s\n' "$*"; }
die() { printf '\033[1;31m[samples-seed FATAL]\033[0m %s\n' "$*" >&2; exit 1; }

# --- preflight -----------------------------------------------------------

command -v kubectl >/dev/null || die "kubectl is not on PATH"
command -v jq >/dev/null || die "jq is not on PATH (required for the Gitea API calls)"
command -v git >/dev/null || die "git is not on PATH"
kubectl --context "$CTX" get nodes >/dev/null 2>&1 \
	|| die "cannot reach cluster context '$CTX' (is the coxswain-dev kind cluster up?)"

for app in "${APPS[@]}"; do
	[ -d "$ROOT/examples/$app" ] \
		|| die "examples/$app does not exist (expected the seed source, samples plan §1)"
done

# Pull the credentials out of the cluster so the script and the cluster
# never disagree about what the Loops will use.
ADMIN_USER=$("${KUBECTL[@]}" get secret gitea-admin -o jsonpath='{.data.username}' | base64 -d)
ADMIN_PASS=$("${KUBECTL[@]}" get secret gitea-admin -o jsonpath='{.data.password}' | base64 -d)
GIT_USER=$("${KUBECTL[@]}" get secret samples-git-cred -o jsonpath='{.data.username}' | base64 -d)
GIT_PASS=$("${KUBECTL[@]}" get secret samples-git-cred -o jsonpath='{.data.password}' | base64 -d)
[ -n "$ADMIN_USER" ] && [ -n "$ADMIN_PASS" ] \
	|| die "secret gitea-admin missing/empty in ns $NS (run 'make samples-up' first)"
[ -n "$GIT_USER" ] && [ -n "$GIT_PASS" ] \
	|| die "secret samples-git-cred missing/empty in ns $NS (run 'make samples-up' first)"

log "ctx=$CTX ns=$NS user=$GIT_USER apps=${APPS[*]}"

# Create the admin account (idempotent). The 1.24 image self-installs
# (INSTALL_LOCK=true + sqlite3) but has no admin auto-init, so the account
# is created here via the in-pod CLI. A fresh emptyDir has an empty user
# table, so the first 'admin user create' makes the admin; on a surviving
# emptyDir the user already exists and the CLI's error is ignored.
log "ensuring the Gitea admin account '$ADMIN_USER' (kubectl exec) ..."
"${KUBECTL[@]}" exec deploy/gitea -- sh -c \
	"su -s /bin/sh git -c '/usr/local/bin/gitea admin user create --username $ADMIN_USER --password $ADMIN_PASS --email ${ADMIN_USER}@samples.local --admin --must-change-password=false' 2>&1 | tail -2" \
	|| die "gitea admin user create failed (is the gitea pod Ready? kubectl --context $CTX -n $NS get pod)"

# The Gitea API is reached host-side through a kubectl port-forward (no pod
# scheduling needed; faster than an in-cluster client for a handful of API
# calls). Port-forward to an ephemeral host port.
PF_PORT=$(( (RANDOM % 1000) + 35000 ))
log "port-forwarding svc/gitea -> 127.0.0.1:$PF_PORT"
kubectl --context "$CTX" -n "$NS" port-forward svc/gitea "$PF_PORT:3000" >/dev/null 2>&1 &
PF_PID=$!
trap 'kill "$PF_PID" 2>/dev/null || true' EXIT

# Wait for the port-forward to accept connections (up to ~15s).
for _ in $(seq 1 30); do
	if curl -fsS -m 2 "http://127.0.0.1:$PF_PORT/api/healthz" >/dev/null 2>&1; then
		break
	fi
	if ! kill -0 "$PF_PID" 2>/dev/null; then
		die "kubectl port-forward died early; is the gitea pod Ready? (kubectl --context $CTX -n $NS get pod)"
	fi
	sleep 0.5
done
curl -fsS -m 5 "http://127.0.0.1:$PF_PORT/api/healthz" >/dev/null 2>&1 \
	|| die "gitea did not answer on 127.0.0.1:$PF_PORT (port-forward failed or gitea is not Ready)"

API="http://127.0.0.1:$PF_PORT"
API_AUTH="$ADMIN_USER:$ADMIN_PASS"

# Sanity: the admin credential must work before any seeding (the Gitea image
# auto-creates the admin from GITEA__admin__* on first boot — if this fails,
# the gitea pod started before the env was present; a rollout restart fixes
# it, but fail loudly here instead of seeding with a broken credential).
if ! curl -fsS -m 5 -u "$API_AUTH" "$API/api/v1/version" >/dev/null 2>&1; then
	die "Gitea admin credential (from secret gitea-admin) is rejected — the gitea pod likely booted without the GITEA__admin__* env. Fix: kubectl --context $CTX -n $NS rollout restart deploy/gitea, then re-run."
fi

# --- Gitea admin API helpers ---------------------------------------------

# gitea_api METHOD PATH [JSON_BODY] — an admin REST call; dies with the
# server body on non-2xx.
gitea_api() {
	local method="$1" path="$2" body="${3:-}"
	if [ -n "$body" ]; then
		curl -fsS -m 30 -X "$method" -u "$API_AUTH" \
			-H 'Content-Type: application/json' -d "$body" "$API$path"
	else
		curl -fsS -m 30 -X "$method" -u "$API_AUTH" "$API$path"
	fi
}

# --- ensure the samples user ---------------------------------------------

log "ensuring Gitea user '$GIT_USER' (admin API)..."
EXISTING=$(curl -fsS -m 30 -u "$API_AUTH" "$API/api/v1/users/$GIT_USER" 2>/dev/null || true)
if [ -n "$EXISTING" ]; then
	# Reset the password so the in-cluster Secret and the Gitea account
	# never drift (idempotent seed: safe to re-run after a password change).
	gitea_api PATCH "/api/v1/admin/users/$GIT_USER" \
		"$(jq -nc --arg p "$GIT_PASS" '{password: $p}')" >/dev/null
	log "   user '$GIT_USER' already existed; password reset to match the Secret"
else
	gitea_api POST /api/v1/admin/users \
		"$(jq -nc --arg u "$GIT_USER" --arg p "$GIT_PASS" \
			'{username: $u, password: $p, email: ($u + "@samples.local"), must_change_password: false}')" >/dev/null
	log "   created user '$GIT_USER'"
fi

# --- seed each app -------------------------------------------------------

for app in "${APPS[@]}"; do
	log "seeding repo '$app' from examples/$app ..."

	# 1. A fresh repo every run: delete a stale one (idempotent). The admin
	#    account is the repo owner at creation, so DELETE works.
	curl -fsS -m 30 -X DELETE -u "$API_AUTH" \
		"$API/api/v1/repos/$GIT_USER/$app" >/dev/null 2>&1 || true

	gitea_api POST /api/v1/user/repos \
		"$(jq -nc --arg n "$app" '{name: $n, auto_init: false, private: false, default_branch: "initial"}')" >/dev/null
	log "   created repo $GIT_USER/$app"

	# 2. Temp git copy of examples/<app> — never the coxswain repo (S2
	#    requirement: nothing ever points at github.com).
	workdir=$(mktemp -d)
	cp -a "$ROOT/examples/$app/." "$workdir/"
	rm -rf "$workdir/.git" 2>/dev/null || true
	(
		cd "$workdir"
		# Git identity scoped to this clone only: the commit is the seed
		# commit the Loops pin status.baseCommit against (S3).
		git init -q -b initial
		git config user.name "coxswain-samples-seed"
		git config user.email "samples@coxswain.local"
		git add -A
		# The commit message is the marker the acceptance check greps for:
		# 'git log --oneline' on the clone must show the initial commit.
		git commit -qm "initial: seed state for $app (broken, per SAMPLES-PLAN section 1)"
		git remote add origin "http://$GIT_USER:$GIT_PASS@127.0.0.1:$PF_PORT/$GIT_USER/$app.git"
		git push -q -u origin initial
	)
	rm -rf "$workdir"

	# 3. Verify through the samples credential (the one the Loops use): the
	#    repo must be readable and refs/heads/initial must exist.
	HEAD=$(git ls-remote "http://$GIT_USER:$GIT_PASS@127.0.0.1:$PF_PORT/$GIT_USER/$app.git" refs/heads/initial | awk '{print $1}')
	[ -n "$HEAD" ] \
		|| die "push for '$app' reported success but refs/heads/initial is not visible via the $GIT_USER credential (permission problem?)"
	log "   $GIT_USER/$app ready: refs/heads/initial = $HEAD"
done

log "all ${#APPS[@]} repo(s) seeded. Loops can clone via:"
log "   git clone http://$GIT_USER:***@gitea.${NS}.svc:3000/$GIT_USER/<app>.git"
