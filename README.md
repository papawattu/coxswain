# Coxswain

A Kubernetes operator that runs long-lived **plan → implement → verify** loops for coding agents. Each loop runs in an isolated [agent-sandbox](https://github.com/kubernetes-sigs/agent-sandbox) workspace, stops on success, budget or stall, and hands back a pull request.

> **Status: pre-alpha.** Today the operator accepts a `Loop`, creates its hardened sandbox (zero-credential agent pod, ADR-0006), and tracks the loop's phase. The agent runner, verification and pull requests are being built in Phase 1 — see [docs/PLAN.md](docs/PLAN.md). **Agent image stand-in (I37):** when `spec.agent.image` is omitted the sandbox runs `docker.io/library/golang:1.26` + `sleep infinity` as a Phase 0 stand-in — it is hardened but not an agent. The reference runner image lands with the A-slices; until then treat the default as a stand-in, not the hardened agent-agnostic default. **Dev escape hatch:** the dev deployment passes `--allow-unenforced` so Loops run before the eBPF enforcement-evidence seam (the I32 relay) is wired — they run with `PolicyEnforced=False` reason `EnforcementDisabled`. In production, remove that flag: the D30 gate then holds the sandbox Suspended until enforcement is proven (fail-closed).

## Getting started

Run Coxswain on a local [kind](https://kind.sigs.k8s.io/) cluster in about five minutes.

**You need:** Docker, [kind](https://kind.sigs.k8s.io/docs/user/quick-start/#installation), `kubectl`, Go 1.26+, and `make`.

### 1. Create a cluster with agent-sandbox

```sh
kind create cluster --name coxswain --image kindest/node:v1.34.0

kubectl apply -f https://github.com/kubernetes-sigs/agent-sandbox/releases/download/v1.0.4/sandbox.yaml
kubectl rollout status deploy/agent-sandbox-controller -n agent-sandbox-system
```

### 2. Build and deploy the operator

```sh
git clone https://github.com/papawattu/coxswain.git
cd coxswain

make docker-build IMG=coxswain:dev
kind load docker-image coxswain:dev --name coxswain
make deploy IMG=coxswain:dev

kubectl rollout status deploy/coxswain-controller-manager -n coxswain-system
```

### 3. Start a loop

A `Loop` names a goal, a repository to work on, and the checks that decide when the work is done:

```yaml
apiVersion: coxswain.wattu.com/v1alpha1
kind: Loop
metadata:
  name: loop-sample
spec:
  goal: "make the failing test pass"
  workspace:
    repo: "https://github.com/papawattu/coxswain.git"
    ref: "main"
  verify:
    acceptanceChecks:
      - "go test ./..."
  loop:
    maxIterations: 3
```

```sh
kubectl apply -f config/samples/coxswain_v1alpha1_loop.yaml
```

### 4. Watch it

```sh
kubectl get loops
# NAME          PHASE     ITERATION   AGE
# loop-sample   Pending   0           10s

kubectl get sandboxes,pods
# sandbox.agents.x-k8s.io/loop-sample-sandbox   True   DependenciesReady
# pod/loop-sample-sandbox                       1/1    Running
```

The operator created an isolated sandbox for the loop, and agent-sandbox started its pod. That's as far as a loop goes today; the agent runner that plans, implements and verifies inside the sandbox lands in Phase 1.

### Clean up

```sh
kind delete cluster --name coxswain
```

## How it works

The design, most of which is still being built:

- **Loop** — one goal, one repo, run until it succeeds or fails. New direction means a new Loop.
- **Operator** — a deterministic state machine. It decides whether a loop succeeded or failed from evidence it gathers itself, never from what the agent claims.
- **Runner** — the agent inside the sandbox. It plans and writes code; it can't mark its own work as done.
- **Acceptance checks** — your commands (like `go test ./...`). The operator runs them in a separate, locked-down pod against a pinned commit, and fails the loop if the agent changed the tests. A person reviewing the draft pull request is the final gate.

The vocabulary is defined in [CONTEXT.md](CONTEXT.md); the design decisions are in [docs/adr/](docs/adr/).

## Development

```sh
make test   # unit + envtest (operator and runner)
make lint
```

## License

Apache 2.0.
