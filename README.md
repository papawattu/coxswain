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

> **CNI preflight (production):** before installing coxswain on a production
> cluster, run `K8S_CONTEXT=<your-ctx> make verify-cni` to confirm the cluster's
> CNI polices pod-to-host-network egress (the D38 requirement). kindnet
> (kind's default) does not — see the [Security](#security-rbac-guidance-for-installers)
> section for details. This step is not needed for a local kind dev cluster
> (kindnet is dev-only).

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

### Delivery

With `spec.delivery.mode: PullRequest`, a successful, verified Loop pushes the
verified commit to the workspace repo on branch `<branchPrefix><loop-name>` and
opens a pull request against `spec.delivery.baseBranch` (default
`spec.workspace.ref`). The PR is a draft by default (`spec.delivery.draft`);
the builder marks it ready explicitly. Drafts are represented differently per
provider: on GitHub the PR is opened as a real draft PR; on Gitea the create-PR
API ignores the draft flag, so the draft is marked by prefixing the PR title
with `WIP: ` (Gitea's draft convention) — the only marker.

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
polices pod → host-network egress. **Production requires a CNI that polices
pod-to-host-network egress** — i.e. a CNI that blocks pod egress to the
apiserver service IP, the node's `:6443` and `:10250`, pod IPs in other
namespaces, and external IPs when a NetworkPolicy allows only DNS to
kube-dns. This is a property, not a vendor list:

| CNI | Status |
|-----|--------|
| Calico v3.30.1 | **Verified** — `make d38-cni-e2e` (kind cluster with Calico as the enforcing CNI; `make verify-cni` also passes) |
| Cilium | To be confirmed — run `make verify-cni` on your cluster |
| GKE Dataplane V2 | To be confirmed — run `make verify-cni` on your cluster |
| EKS VPC CNI (with network policy) | To be confirmed — run `make verify-cni` on your cluster |
| AKS (Azure CNI) | To be confirmed — run `make verify-cni` on your cluster |
| kindnet (kind's default CNI) | **Dev-only** — does NOT police pod → host-network egress (see below) |

kindnet (kind's default CNI) does **not** police pod → host-network egress:
on kindnet the agent pod can reach the apiserver service IP (10.96.0.1:443),
the node's `:6443`, and the kubelet's `:10250` even though pod-IP and
external-IP egress is denied (docs/REVIEW-PHASE1-R16.md, D38; also the
known-limitation line L1 printed by `make i42-e2e`). kindnet is therefore
**dev-only**: it hides the control plane behind authentication (no token is
mounted into the agent) rather than blocking it.

**Preflight check:** `make verify-cni` (optionally `K8S_CONTEXT=<ctx>`)
creates a temp namespace with a NetworkPolicy shaped like the agent's and a
probe pod, then TCP-connects to the apiserver service IP, each node's
`:6443`/`:10250`, a kube-dns pod IP, and `1.1.1.1:443` from the probe. Every
target must be BLOCKED for PASS. It also runs a **hardening section**
(WARN-only, never changes PASS/FAIL) from a second probe pod with no
NetworkPolicy: checks whether the kubelet read-only port (10255) is open and
what the apiserver returns for an unauthenticated request. It works on any
cluster where kubectl is pointed (no coxswain install required) and always
cleans up. Run it on your production cluster before installing coxswain.

**Residual risk on CNIs that don't police pod → host-network egress:**
if your CNI does not pass `make verify-cni`, the agent pod **can** reach the
apiserver service IP, the node's `:6443`, and the kubelet's `:10250`.
The mitigations that remain are:
- the agent pod has `automountServiceAccountToken: false` — no SA token is mounted;
- the apiserver returns 403 for anonymous requests (except public-info endpoints like `/version`);
- the kubelet requires authentication (401 without credentials).

This means the agent can *reach* the control plane but cannot *act* on it
without credentials. It is an unauthenticated attack surface that the design
assumed was closed. To close it, apply the cluster-hardening checklist below.

**Cluster-hardening checklist (recommended for all clusters, required when the CNI does not police pod → host-network egress):**

1. **Disable or restrict apiserver anonymous authentication.**
   Set `--anonymous-auth=false` on the apiserver, or use an
   `AuthenticationConfiguration` with `anonymous: deny`. Without this,
   any pod that can reach the apiserver can make unauthenticated requests to
   public-info endpoints and enumerate some cluster metadata.
2. **Turn off the kubelet read-only port.**
   Set `readOnlyPort: 0` in the kubelet config. The read-only port (10255)
   serves `/metrics` and some stats without authentication. With the port off,
   a pod that reaches the node must go through the authenticated kubelet port
   (10250), which requires a token.
3. **Enable kubelet authentication and authorization webhooks.**
   Set `authenticationTokenWebhook: true` and `authorizationMode: Webhook`
   (not `AlwaysAllow`) in the kubelet config. This ensures that even a pod
   that reaches the kubelet's authenticated port cannot act without valid
   credentials and an authorized RBAC binding.
4. **No service-account token automount for agents.**
   Coxswain already sets `automountServiceAccountToken: false` on the agent
   pod spec. Verify that no other mechanism (a mutating webhook, a
   ClusterRoleBinding, etc.) grants the agent a token.
5. **KubeArmor BPF-LSM for the DNS fence (tool proxies).**
   The tool proxy's KubeArmor `matchDNSQueries` fence (the inner egress fence
   that restricts the proxy's DNS to the upstream host + platform DNS) is
   enforced only on clusters with BPF-LSM active (the `SOCKET_SENDMSG` hook).
   On clusters without BPF-LSM (e.g. kind on certain kernels), the fence is
   **not enforced**, and the only thing keeping the tool proxy's egress on
   the configured upstream is the proxy's own code (it dials the fixed
   upstream host only). This is an accepted limitation (D50, R23 I64);
   see [docs/adr/0008-tool-proxies.md](docs/adr/0008-tool-proxies.md).

See [docs/adr/0006-agent-isolation-and-zero-credentials.md](docs/adr/0006-agent-isolation-and-zero-credentials.md)
for the full rationale.

## Development

```sh
make test   # unit + envtest (operator and runner)
make lint
```

## License

Apache 2.0.
