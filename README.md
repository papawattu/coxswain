# Coxswain

A Kubernetes operator that runs long-lived **plan → implement → verify** loops for coding agents. Each loop runs in an isolated [agent-sandbox](https://github.com/kubernetes-sigs/agent-sandbox) workspace, stops on success, budget or stall, and hands back a pull request.

> **Status: pre-alpha.** Today the operator accepts a `Loop`, creates its hardened sandbox (zero-credential agent pod, ADR-0006), and tracks the loop's phase. The agent runner, verification and pull requests are being built in Phase 1 — see [docs/PLAN.md](docs/PLAN.md). **Agent image stand-in (I37):** when `spec.agent.image` is omitted the sandbox runs `docker.io/library/golang:1.26` + `sleep infinity` as a Phase 0 stand-in — it is hardened but not an agent. The reference runner image lands with the A-slices; until then treat the default as a stand-in, not the hardened agent-agnostic default. **Dev escape hatch:** `make deploy-dev` (the `config/dev` kustomize overlay) passes `--allow-unenforced` so Loops run before the eBPF enforcement-evidence seam (the I32 relay) is wired — they run with `PolicyEnforced=False` reason `EnforcementDisabled`. `make deploy` (and the production bundle `dist/install.yaml`) do NOT carry the flag: the D30 gate then holds the sandbox Suspended until enforcement is proven (fail-closed).

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
# Dev/kind only: deploy-dev adds --allow-unenforced so Loops run before the eBPF
# enforcement-evidence seam (the I32 relay) is wired. For a production install
# use `make deploy` (fail-closed; the D30 gate holds the sandbox Suspended until
# enforcement is proven).
make deploy-dev IMG=coxswain:dev

# The proxy stand-in image is built and loaded by `make kind-up` (the
# default proxyImage for a Loop with a model endpoint is coxswain-proxy:standin).
# If you build it manually: make proxy-build && kind load docker-image coxswain-proxy:standin --name coxswain

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

## Security (RBAC guidance for installers)

Coxswain deploys a `ValidatingAdmissionPolicy` + binding (see
`config/admission/validating_admission_policy.yaml`, wired into
`config/default`) that **denies adding ephemeral containers**
(`kubectl debug`) to coxswain component pods (agent, model-proxy,
egress-proxy). KubeArmor does not police ephemeral containers, so one added
to a fenced pod would escape the fence and share its network namespace.

**RBAC rules for Loop namespaces:**

- Do **not** grant `pods/ephemeralcontainers` to any non-admin principal in a
  Loop namespace. The admission policy is a backstop, not a substitute for
  least-privilege RBAC.
- **Restrict `pods` update/patch too.** The admission policy matches by
  label, so a principal who can patch a pod can strip the
  `app.kubernetes.io/component` label and then add an ephemeral container to
  the now-unlabelled pod. Restricting `pods` update/patch is required in Loop
  namespaces as well. (Relabelling also detaches the pod from its
  NetworkPolicy and KubeArmor selectors, which is independently dangerous.)

See [docs/TDD-PLAN-PHASE1.md](docs/TDD-PLAN-PHASE1.md) §RBAC guidance for the
full rationale.

**CNI requirement (production):** the agent's allowlist NetworkPolicy leaves
host-network destinations to the CNI's implementation, and not every CNI
polices pod → host-network egress. **kindnet (kind's default CNI) does not**
— on kindnet the agent pod can reach the apiserver service IP (10.96.0.1:443),
the node's :6443, and the kubelet's :10250 even though pod-IP and external-IP
egress is denied (docs/REVIEW-PHASE1-R16.md, D38; also the known-limitation
line L1 printed by `make i42-e2e`). kindnet is therefore **dev-only**: it
hides the control plane behind authentication (no token is mounted into the
agent) rather than blocking it. **Production clusters must run a CNI that
enforces NetworkPolicy egress against host-network and node destinations —
Calico or Cilium** (see [docs/adr/0006-agent-isolation-and-zero-credentials.md](docs/adr/0006-agent-isolation-and-zero-credentials.md)).
The property is tested, not just documented: `make kind-calico-up` +
`make d38-cni-e2e` run the blocked-endpoint checks for real on a second kind
cluster whose Calico CNI enforces them.

## Development

```sh
make test   # unit + envtest (operator and runner)
make lint
```

## License

Apache 2.0.
