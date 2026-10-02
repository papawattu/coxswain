#!/usr/bin/env bash
# S2 acceptance (samples plan §7, slice S2): from a THROWAWAY pod in the
# samples namespace, clone each seeded repo with the samples basic-auth
# credential and assert:
#   1. the clone succeeded (git is present in the image, network path works),
#   2. 'git log --oneline' shows the initial commit (exactly one commit,
#      message 'initial: seed state for <app> ...'),
#   3. 'git remote -v' names NO github.com remote (the seeded repos point
#      only at the in-cluster Gitea; nothing in them references coxswain's
#      GitHub remote — the samples plan §2 defence).
#
# The pod is always deleted on exit (trap).
#
# Pinning: kind-coxswain-dev only (GITEA_CTX / GITEA_NS override for local
# debugging). The throwaway image must carry git + curl; golang:1.26 does.
set -euo pipefail

# Usage: samples-accept.sh <ctx> <ns> <app>...
# (CTX/NS are pinned by the caller, hack/samples-git.sh — coxswain-dev only.)
CTX="${1:?ctx required}"
NS="${2:?ns required}"
shift 2
APPS=("$@")
[ ${#APPS[@]} -gt 0 ] || APPS=(gocli pylib webapi)

KUBECTL=(kubectl --context "$CTX" -n "$NS")
POD="s2-acceptance-$$"
IMAGE="${S2_ACCEPT_IMAGE:-gitea/gitea:1.24}"

log() { printf '\033[1;36m[samples-accept]\033[0m %s\n' "$*"; }
die() { printf '\033[1;31m[samples-accept FATAL]\033[0m %s\n' "$*" >&2; exit 1; }

trap 'kubectl --context "$CTX" -n "$NS" delete pod "$POD" --wait=false >/dev/null 2>&1 || true' EXIT

log "creating throwaway pod $POD (image $IMAGE) in ns $NS ..."
"${KUBECTL[@]}" run "$POD" --image="$IMAGE" --restart=Never \
	--overrides='{"spec":{"terminationGracePeriodSeconds":0,"containers":[{"name":"main","image":"'"$IMAGE"'"}]}}' \
	--command -- sh -c 'sleep 3600' >/dev/null
# Wait for the pod to be Running (image pull + container start).
for _ in $(seq 1 60); do
	phase=$("${KUBECTL[@]}" get pod "$POD" -o jsonpath='{.status.phase}' 2>/dev/null || echo Pending)
	[ "$phase" = "Running" ] && break
	[ "$phase" = "Failed" ] && die "pod $POD failed to start (check image $IMAGE pull; pre-load with: kind load docker-image $IMAGE --name coxswain-dev)"
	sleep 2
done
[ "$phase" = "Running" ] || die "pod $POD did not reach Running in 120s"
sleep 2  # give the shell loop a beat for the container to be fully up

GIT_USER=$("${KUBECTL[@]}" get secret samples-git-cred -o jsonpath='{.data.username}' | base64 -d)
GIT_PASS=$("${KUBECTL[@]}" get secret samples-git-cred -o jsonpath='{.data.password}' | base64 -d)
[ -n "$GIT_USER" ] && [ -n "$GIT_PASS" ] || die "secret samples-git-cred missing in ns $NS"

# One exec per app (keeps each check's evidence line clean and makes a
# failure attributable to a specific repo).
FAIL=0
for app in "${APPS[@]}"; do
	log "clone check: samples/$app"
	set +e
	out=$("${KUBECTL[@]}" exec "$POD" -- sh -c "
		set -e
		cd /tmp
		rm -rf $app
		git clone -q http://${GIT_USER}:${GIT_PASS}@gitea.${NS}.svc:3000/samples/${app}.git $app 2>&1
		echo 'CLONE_OK'
	" 2>&1)
	rc=$?
	set -e
	if [ $rc -ne 0 ] || ! printf '%s' "$out" | grep -q '^CLONE_OK$'; then
		die "clone of samples/$app failed (rc=$rc):
$out"
	fi
	# git log --oneline: exactly one commit, the seed's 'initial'.
	logline=$("${KUBECTL[@]}" exec "$POD" -- sh -c "cd /tmp/$app && git log --oneline" 2>&1)
	ncommits=$(printf '%s\n' "$logline" | grep -c . || true)
	if [ "$ncommits" != "1" ]; then
		die "samples/$app has $ncommits commits, expected exactly 1 (the seed 'initial'):
$logline"
	fi
	printf '%s\n' "$logline" | grep -q 'initial: seed state for' \
		|| die "samples/$app initial commit message missing:
$logline"
	log "   $app: $(printf '%s' "$logline" | head -1)"
	# git remote -v: NO github.com.
	remotes=$("${KUBECTL[@]}" exec "$POD" -- sh -c "cd /tmp/$app && git remote -v" 2>&1)
	if printf '%s' "$remotes" | grep -q 'github.com'; then
		die "samples/$app remote references github.com (the seeded repo must never point at coxswain's GitHub):
$remotes"
	fi
	log "   $app remotes: $(printf '%s' "$remotes" | head -1)"
	# git ls-files: the seed must contain ONLY the app's own files — no
	# reference answers. The S1 reference answers (tasks/*.patch, tasks.md)
	# live in the coxswain repo under examples/<app>/ but must NOT be in the
	# seeded repo: the Loops are expected to produce them, so shipping them
	# leaks the answers. 'git archive HEAD:examples/<app>' materialises the
	# committed tree; the tar --exclude filters in samples-seed.sh drop
	# tasks/ and tasks.md. A *.patch anywhere (top-level or nested) is also
	# a leak (the answers are all .patch files). The set must be non-empty
	# (the app files themselves are present), so an empty ls-files is not a
	# vacuous pass.
	lsfiles=$("${KUBECTL[@]}" exec "$POD" -- sh -c "cd /tmp/$app && git ls-files" 2>&1)
	if printf '%s\n' "$lsfiles" | grep -qE '^tasks/|^tasks\.md$|\.patch$'; then
		die "samples/$app seeded tree contains reference-answer files (tasks/, tasks.md, or *.patch):
$lsfiles"
	fi
	nfiles=$(printf '%s\n' "$lsfiles" | grep -c . || true)
	[ "$nfiles" -gt 0 ] || die "samples/$app git ls-files is empty (the app files are missing)"
	log "   $app ls-files: $nfiles file(s), no tasks/ or tasks.md or *.patch"
done

log "S2 acceptance PASSED for: ${APPS[*]} (clone + single 'initial' commit + no github.com remote + no reference-answer files, all from a throwaway pod in ns $NS)"
