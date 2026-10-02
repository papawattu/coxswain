#!/usr/bin/env bash
# S2 (samples plan §7, slice S2): the top-level driver for the in-cluster
# git server on coxswain-dev. Modes:
#
#   up     — apply config/samples-git to kind-coxswain-dev (context is
#            PINNED: this target never touches any other cluster) and wait
#            for the Gitea pod to be Ready.
#   seed   — create the 'samples' Gitea user + the three repos (one
#            'initial' commit each) from examples/<app>. Idempotent.
#   accept — the acceptance check: a throwaway pod in ns samples clones
#            each repo with the samples basic-auth credential and asserts
#            the single 'initial' commit + no github.com remote.
#
# Never deletes clusters, never touches KubeArmor or the operator.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
CTX="${GITEA_CTX:-kind-coxswain-dev}"
NS="${GITEA_NS:-samples}"
KUSTOMIZE="${ROOT}/bin/kustomize"

log() { printf '\033[1;34m[samples-git]\033[0m %s\n' "$*"; }
die() { printf '\033[1;31m[samples-git FATAL]\033[0m %s\n' "$*" >&2; exit 1; }

command -v kubectl >/dev/null || die "kubectl is not on PATH"
command -v jq >/dev/null || die "jq is not on PATH"
kubectl --context "$CTX" get nodes >/dev/null 2>&1 \
	|| die "cannot reach cluster context '$CTX' (is the coxswain-dev kind cluster running?)"

MODE="${1:-up}"

case "$MODE" in
up)
	log "applying config/samples-git to --context $CTX (samples-up targets coxswain-dev only)..."
	if [ ! -x "$KUSTOMIZE" ]; then
		log "bin/kustomize missing; running 'make kustomize'..."
		( cd "$ROOT" && make kustomize )
	fi
	"$KUSTOMIZE" build "$ROOT/config/samples-git" | kubectl --context "$CTX" apply -f -
	log "waiting for the gitea deployment to be ready in ns $NS (up to ~180s) ..."
	for i in $(seq 1 36); do
		# The Deployment's readyReplicas (not a pod-list probe): during a
		# rollout the old (unready) pod can still be items[0] of the pod list.
		ready=$(kubectl --context "$CTX" -n "$NS" get deploy gitea \
			-o jsonpath='{.status.readyReplicas}' 2>/dev/null | tr -d ' ')
		[ "$ready" = "1" ] && break
		[ $i -eq 36 ] && {
			kubectl --context "$CTX" -n "$NS" get pod -l app.kubernetes.io/name=gitea
			kubectl --context "$CTX" -n "$NS" logs deploy/gitea --tail=15
			die "gitea did not reach ready in 180s (see the pod logs above)"
		}
		sleep 5
	done
	kubectl --context "$CTX" -n "$NS" get pod -l app.kubernetes.io/name=gitea
	kubectl --context "$CTX" -n "$NS" get svc gitea
	log "samples-up complete: Gitea Ready at http://gitea.${NS}.svc:3000"
	;;
seed)
	log "seeding repos from examples/{gocli,pylib,webapi} ..."
	bash "$ROOT/hack/samples-seed.sh" "$CTX" "$NS" gocli pylib webapi
	;;
accept)
	log "running the S2 acceptance check (throwaway pod in ns $NS) ..."
	bash "$ROOT/hack/samples-accept.sh" "$CTX" "$NS" gocli pylib webapi
	;;
all)
	"$0" up
	"$0" seed
	"$0" accept
	;;
*)
	die "unknown mode '$MODE' (expected: up|seed|accept|all)"
	;;
esac
