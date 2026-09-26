# Phase 0 — e2e verification

## What was proven (2026-09-26)

On a fresh **kind** cluster (`coxswain`, k8s 1.34.0), the operator:

1. Started the `loop` controller with event sources for `Loop` and `Sandbox`.
2. Reconciled a sample `Loop` and **created its `Sandbox`** object
   (`operation: created`), then logged `ensured loop sandbox`.
3. Was **idempotent**: a re-reconcile of the same Loop produced
   `operation: unchanged` (no duplicate sandbox).
4. Set the Loop's `status.phase` to `Pending`.

This satisfies the Phase 0 "done when": *apply a Loop → the controller creates
the Loop's sandbox and logs it.*

## Environment

- kind cluster `coxswain` (node `coxswain-control-plane`, v1.34.0)
- agent-sandbox CRD applied (from `config/crd/external/`) so the `Sandbox` type
  exists. The agent-sandbox **controller was intentionally not run** (Phase 0
  literal) — so the `Sandbox` object has no backing Pod yet. That wiring is
  Phase 1.
- operator image `coxswain-operator:dev` built from `Dockerfile`, `kind load`ed.
- deployed into `coxswain-system` with the Loop CRD + RBAC + leader-election RBAC.

## Commands

```bash
# cluster
kind create cluster --name coxswain
kubectl config use-context kind-coxswain

# types
kubectl apply -f config/crd/external/agents.x-k8s.io_sandboxes.yaml
kubectl apply -f config/crd/bases/coxswain.wattu.com_loops.yaml

# operator image
docker build -t coxswain-operator:dev -f Dockerfile .
kind load docker-image coxswain-operator:dev --name coxswain

# ns + rbac + manager (override the scaffold's hardcoded namespace: system →
# coxswain-system; the leader-election binding's subject namespace must be
# patched to match, or the manager can't create its lease)
kubectl create ns coxswain-system
# ... apply config/rbac/{service_account,role,role_binding}.yaml with ns override
kubectl apply -f config/rbac/leader_election_role.yaml -n coxswain-system
kubectl apply -f config/rbac/leader_election_role_binding.yaml -n coxswain-system
kubectl patch rolebinding leader-election-rolebinding -n coxswain-system \
  --type='json' -p='[{"op":"replace","path":"/subjects/0/namespace","value":"coxswain-system"}]'
# manager with our image (override image + namespace in config/manager/manager.yaml)
kubectl apply -f <patched manager.yaml>

# the smoke Loop
kubectl apply -f - <<EOF
apiVersion: coxswain.wattu.com/v1alpha1
kind: Loop
metadata: {name: smoke, namespace: coxswain-system}
spec:
  goal: "Phase 0 smoke: prove the operator creates my sandbox"
  workspace: {repo: "https://github.com/papawattu/coxswain.git", ref: "main"}
  verify: {acceptanceChecks: ["go test ./..."]}
  loop: {maxIterations: 3}
EOF

# verify
kubectl get loop smoke -n coxswain-system        # PHASE: Pending
kubectl get sandbox -n coxswain-system           # smoke-sandbox exists
kubectl logs -n coxswain-system -l control-plane=controller-manager | grep 'ensured loop sandbox'
```

## Gotchas hit

- **Dockerfile build line** was `go build -o manager cmd/main.go` (builds a file
  relative to the module root incorrectly) → fixed to `go build -o manager ./cmd`.
- **`.dockerignore`** used `**` ignore + `!**/*.go` re-include, which copies the
  `.go` files but not the `cmd`/`api`/`internal` *directory* entries, so
  `go build ./cmd` failed with "directory not found". Fixed by adding
  `!cmd !api !internal`.
- **Scaffold hardcodes `namespace: system`** in `config/rbac/*` and
  `config/manager/manager.yaml`. When deploying without the `config/default`
  kustomize overlay (which normally injects the namespace via transformers), you
  must override every `namespace: system` → your namespace, **and** patch the
  leader-election `RoleBinding`'s subject namespace, or the manager loops on
  `cannot get resource "leases"`.
