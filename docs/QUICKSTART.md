# QUICKSTART

Get a Loop running end-to-end on a cluster: install agent-sandbox and
KubeArmor, install the operator from a release's `install.yaml`, create the
GitHub token Secret, run a first Loop, and read the result with `kubectl cox`.

The alpha ships `dist/install.yaml` (D54: ghcr.io + install.yaml; a Helm chart
is Phase 7 work). A `v*` release tag publishes the public images and the
`install.yaml` that points at them.

When something is not working, start with `docs/TROUBLESHOOTING.md` — the
symptoms alpha users have hit (ModelUnavailable, fresh-kind DNS, the
configmaps RBAC wedge, kubectl debug denials, NetworkEnforced, the #124
Implementing stall), each with its documented cause and fix.

## 1. Prerequisites

Coxswain needs three things on the cluster. Dev/CI pins **k8s 1.34**
(`kindest/node:v1.34.0`; `KIND_NODE_IMAGE` in the Makefile) — a cluster that
runs the alpha is expected to be at least that. The production floor is
expected to be higher than the dev pin (see `docs/PLAN.md` for the tracked
requirement), so use a recent stable Kubernetes.

### agent-sandbox

The operator runs each Loop in an isolated agent-sandbox workspace. Install
the v1.0.4 release manifest (the controller image
`registry.k8s.io/agent-sandbox/agent-sandbox-controller:v1.0.4`; the source
`k8s/controller.yaml` is a `ko://` placeholder, so use the **release**
manifest, not the source one):

```sh
kubectl apply -f https://github.com/kubernetes-sigs/agent-sandbox/releases/download/v1.0.4/sandbox.yaml
kubectl rollout status deploy/agent-sandbox-controller -n agent-sandbox-system
```

### KubeArmor (BPF-LSM for the policy gate) — REQUIRED

**KubeArmor with BPF-LSM is required for the alpha** (D57: the alpha ships
KubeArmor-only). The operator enforces egress with KubeArmor
`KubeArmorPolicy` objects (the A1 policy gate, ADR-0007): without KubeArmor
the `PolicyEnforced` gate cannot be enforced and the alpha is not running
as designed. The tool proxy's `matchDNSQueries` fence (the inner egress
fence that restricts the proxy's DNS to the upstream host + platform DNS)
needs **BPF-LSM active** (the `SOCKET_SENDMSG` hook). On a cluster without
BPF-LSM the fence is **not enforced** and the only thing keeping the tool
proxy on the configured upstream is the proxy's own code (it dials the fixed
upstream host only) — an accepted limitation (D50, R23 I64; see
`docs/adr/0008-tool-proxies.md`). For a fully fenced run, use a node/kernel
with BPF-LSM enabled (the dev cluster installs KubeArmor v1.7.5 via the
pinned `karmor` CLI; `make kind-up` does this for kind).

### A Kubernetes cluster

A reachable cluster (kubectl in `~/.kube/config` pointing at it). For local
dev the Makefile creates one: `make kind-up` (kind 1.34, agent-sandbox,
KubeArmor).

### Network gate (D38)

The alpha enforces the agent's egress at two layers. The KubeArmor policy
gate (above) is the alpha's enforcement layer. The **network gate (D38)** is
a second, network-layer gate: it needs a CNI that **enforces NetworkPolicy**
(e.g. Calico; the tested profile is the `coxswain-calico` cluster from
`test/e2e/d38-cni-e2e.sh` — kindnet is disabled and Calico polices the agent's
allowlist NetworkPolicy against real endpoints). On a CNI that does **not**
enforce NetworkPolicy (kind's default `kindnet` included), the network gate
cannot be satisfied.

The released `install.yaml` is **fail-closed** on this gate. On a CNI that
doesn't enforce NetworkPolicy, Loops are **held Suspended** with the
`NetworkEnforced` condition `False` (reason `CNIUnenforced` — "the CNI does
not police pod -> host-network egress; sandbox held Suspended",
`internal/controller/loop_controller.go:1008`). The operator probes the CNI
with a self-test pod + NetworkPolicy in the `coxswain-cni-probe` namespace
(`cmd/main.go`, `--cni-probe-*` flags) and re-gates every Loop when the result
changes. Until the first probe succeeds, the condition is `Unknown` and the
sandbox is held Suspended (fail-closed from startup, not just from the first
probe failure).

**To run anyway on a non-enforcing CNI** (dev/kind only — never in
production), add `--allow-unenforced-network` to the manager's args. This is
a **dev escape hatch** (D38), separate from `--allow-unenforced` (the eBPF
policy gate). Loops run with `NetworkEnforced=False` reason
`EnforcementDisabled` and the operator's own condition message:

> "--allow-unenforced-network is set: the Loop runs but the CNI does NOT
> police pod -> host-network egress (dev escape hatch)"
> (`internal/controller/loop_controller.go:992,1000`)

What that gives up, plainly: **the CNI does not police pod → host-network
egress.** The agent can still reach any host-network target the NetworkPolicy
would have blocked; the only egress control left on that path is the proxy's
own code (it dials the fixed upstream host only) — the same accepted
limitation as the D50 DNS-fence note (see `docs/adr/0008-tool-proxies.md`,
"Limitation: DNS fence unenforced on clusters without BPF-LSM (D50, I64)").

Do **not** use `--allow-unenforced` to work around the network gate: that
flag disables the KubeArmor **policy** gate (the A1 enforcement), not the
network gate. The two flags are independent (`cmd/main.go:93-104`).

To add the network escape hatch to a deployed manager, patch the Deployment's
args (the base manifest carries `--leader-elect` and
`--health-probe-bind-address=:8081` as the manager's args, so this appends):

```sh
kubectl -n coxswain-system patch deploy coxswain-controller-manager --type=json \
  -p '[{"op":"add","path":"/spec/template/spec/containers/0/args/-","value":"--allow-unenforced-network"}]'
kubectl -n coxswain-system rollout status deploy/coxswain-controller-manager
```

## 2. Install the operator

Fetch the release's `install.yaml` and apply it. It installs the controller
into the `coxswain-system` namespace (the manifest is namespaced + prefixed
`coxswain-`). From a release tag (the alphas are prereleases — GitHub's
`releases/latest` skips them, so pin the tag; the first alpha release is
`v0.1.0-alpha.1`):

```sh
curl -sfL "https://github.com/papawattu/coxswain/releases/download/v0.1.0-alpha.1/install.yaml" \
  -o install.yaml
kubectl apply -f install.yaml
kubectl rollout status deploy/coxswain-controller-manager -n coxswain-system
```

(From a local checkout, the same file is built with
`make build-installer IMG=ghcr.io/papawattu/coxswain-controller:<tag>`, which
tags every coxswain image in `dist/install.yaml` with `<tag>` — see
`hack/build-installer.sh`.)

`install.yaml` also includes the operator's CNI self-test probe resources
(`config/cni-probe`, applied separately from the default kustomization).

## 3. The GitHub token Secret

The deliver Job (S6) opens the Loop's PR through the Gitea/GitHub API using a
`kubernetes.io/basic-auth` Secret named **`git-cred`** (username
`x-access-token`). The token is a fine-grained GitHub token scoped to the
sandbox repo, and it must never be printed, `cat`'d, or placed on a command
line (D55). Two ways to create it:

**Option A — the helper (preferred).** Put the token in
`~/.config/coxswain/github-token` (mode `600`; the only credential a script
may read, D55), then:

```sh
# <namespace> = the namespace the Loop runs in (default: the loop's namespace)
hack/github-secret.sh <namespace>            # secret name defaults to git-cred
```

The helper reads the token via `--from-file` (never the command line) and
applies the Secret:

```sh
kubectl create secret generic git-cred -n <namespace> \
  --type=kubernetes.io/basic-auth \
  --from-literal=username=x-access-token \
  --from-file=password=~/.config/coxswain/github-token \
  --dry-run=client -o yaml | kubectl apply -f -
```

**Option B — manual `kubectl`** (if you already have the token in a file):

```sh
kubectl create secret generic git-cred -n <namespace> \
  --type=kubernetes.io/basic-auth \
  --from-literal=username=x-access-token \
  --from-file=password=<path-to-token-file> \
  --dry-run=client -o yaml | kubectl apply -f -
```

Create the Secret in **every namespace** where a Loop may run (the deliver Job
reads it from the Loop's namespace). If the Secret is missing, the deliver Job
fails with a `deliver/secret-missing` failure and the Loop's PR is not opened.

## 4. A first Loop

Apply a minimal Loop CR (a `coxswain.wattu.com/v1alpha1` Loop). The operator
creates the agent-sandbox workspace for it, the runner plans → implements →
verifies inside the sandbox, and on success the Loop's PR is opened by the
deliver Job. `config/samples` has a working example:

```sh
kubectl apply -f config/samples/coxswain_v1alpha1_loop.yaml
```

That sample is:

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

What you will see, in order:

1. The operator creates a `Sandbox` (and the agent-sandbox controller starts
   the pod).
2. The runner clones the workspace, plans (`.coxswain/PLAN.md`), implements,
   and verifies the acceptance checks.
3. Each iteration is recorded in the Loop's `status.iterations` ring; on
   success the Loop's `Succeeded` condition flips to `True` and the deliver
   Job opens the PR (using the `git-cred` Secret from step 3).
4. If the verify keeps failing, the Loop hits its `maxIterations` budget and
   stops with the iteration history in `status` (read it back with
   `kubectl cox history <loop>`).

## 5. Reading results with `kubectl cox`

The `cox` CLI (also installed as the `kubectl cox` plugin — `cmd/cox` and
`cmd/kubectl-cox` share one command tree, `internal/cli`) reads Loop results.
The subcommands (from `internal/cli`):

```sh
kubectl cox describe <loop>     # phase, iteration, conditions, budget, delivery
kubectl cox history <loop>      # the audit trail (phase + verify-failure history ring)
kubectl cox logs <loop> [agent|proxy|verify|deliver]  # stream a component's logs
kubectl cox plan <loop>         # read the plan (.coxswain/PLAN.md) from the workspace
kubectl cox diff <loop>         # show the plan diff (current vs. last approved plan)
kubectl cox approve <loop>      # approve the plan (set the PlanApproved condition)
kubectl cox reject <loop> --reason <reason>  # reject the plan (set the PlanRejected condition)
kubectl cox pause <loop>        # set spec.suspend=true
kubectl cox resume <loop>       # clear spec.suspend (set coxswain.io/resume for a Stall/Budget pause)
kubectl cox redeliver <loop>    # re-trigger delivery for a failed Loop (I52 annotation)
kubectl cox fork <name> --loop <source> --iteration <n>  # fork from a checkpoint
```

`describe` is the one to run first after a Loop finishes: it shows the
terminal phase (Succeeded / the budget or stall condition), the iteration
count, and the delivery outcome (the PR the deliver Job opened, or the
delivery failure reason).

## 6. Plan approval (manual approval gate)

By default the runner drives the phases without an approval gate (the
owner's "option B" decision). To enable the **manual approval gate** (B3),
set `spec.approval.mode: Manual` on the Loop. The Loop then pauses at
`AwaitingApproval` after Planning; the user reads the plan and either
approves or rejects it:

```sh
# Read the plan the agent wrote.
kubectl cox plan <loop>

# Show the diff (the current plan vs. the last approved plan, if any).
kubectl cox diff <loop>

# Approve: sets the PlanApproved condition with the plan's SHA-256.
# The operator's approval gate sees the condition True and advances the
# Loop to Implementing.
kubectl cox approve <loop>

# Reject: sets the PlanRejected condition with a reason. The operator's
# approval gate applies the onReject action (Replan: re-enter Planning
# with the reason; Fail: terminal). The reason is shown to the agent on
# the next Planning pass.
kubectl cox reject <loop> --reason "the plan does not address the error handling; add retry logic"
```

A Loop with `approval.mode: Manual` is held at `AwaitingApproval` until
the user runs `cox approve` or `cox reject`. The plan's hash (SHA-256) is
computed by the CLI from the plan's exact bytes; a plan change after
approval invalidates the approval (the operator requires a re-approval
when the hash differs).

## 7. Forking a Loop

A **fork** seeds a new one-shot Loop from an existing Loop's checkpoint
(ADR-0003: forks, not re-tasks). The fork inherits the source Loop's spec
(goal, agent, verify, delivery, loop, checkpoints) and is seeded from the
source Loop's checkpoint at the given iteration. The fork runs a normal
one-shot Loop (own branch, own deliver, own PR) — it does NOT re-task the
source Loop.

```sh
# Fork from iteration 2 of the source Loop (inherits the source's spec
# as-is, including its acceptance checks).
kubectl cox fork my-fork --loop <source-loop> --iteration 2

# Fork with a different acceptance check (replaces
# spec.verify.acceptanceChecks; the rest of verify — preset,
# protectedPaths, etc. — is inherited from the source).
kubectl cox fork my-fork --loop <source-loop> --iteration 2 --check "make -n docs"

# Fork with a different goal (replaces spec.goal; the rest of the spec is
# inherited from the source).
kubectl cox fork my-fork --loop <source-loop> --iteration 2 --goal "a new objective"
```

The `--check` flag (repeatable) replaces the fork's acceptance checks
(`spec.verify.acceptanceChecks`) while keeping the rest of verify intact
(`preset`, `protectedPaths`, `onVerifyFail`, etc. are inherited from the
source). A fork WITHOUT `--check` inherits the source's acceptance checks
(a user forks to try another path toward the SAME acceptance). The `--goal`
flag (when given) replaces the fork's goal (`spec.goal`); a fork WITHOUT
`--goal` inherits the source's goal.

The operator does the fork's PVC restore (the controller's
`spec.forkOf` handling restores the fork's OWN workspace PVC from the
source's checkpoint snapshot at the fork's start). The source Loop's
checkpoint must exist (the source's `status.checkpoints` must have an
entry for the given iteration) and must be ReadyToUse (the source's
VolumeSnapshot must be ReadyToUse) — the operator defers the fork's
restore when the snapshot is not ReadyToUse (the Restore condition is
False, reason `RestoreSourceNotReady`, the requeue retries) and rejects
it when the snapshot is absent (the Restore condition is False, reason
`RestoreSourceMissing`, the fork is terminal).

## 8. Model timeout and tuning

The runner's model client uses a 120-second per-request timeout (I89). If
the model backend hangs (no response within 120s), the runner retries up to
3 times with exponential backoff (2s, 4s, 8s). After all retries are
exhausted, the runner surfaces `ModelUnavailable` in its claim and exits
with a blocked status. The operator maps this to a `ModelAvailable=False`
condition (reason `ModelUnavailable`) on the Loop, visible via
`kubectl cox describe`.

**Worst case:** with the defaults (120s timeout, 3 retries), a fully hung
model backend surfaces `ModelUnavailable` after about 8 minutes
(4×120s + 14s of backoff). This is an improvement over the pre-I89
behaviour (a single 10-minute timeout followed by kubelet back-off churn
with no named reason).

**Tuning:** the timeout is configurable via the runner's `ModelTimeout`
field (set in the sandbox pod's agent container env or the Loop's spec). For
a local 27B-class model under load, a legitimate response can approach 120s
(#124 saw a 60s worst case); increase `ModelTimeout` if you see spurious
`ModelUnavailable` conditions on a healthy backend. The retry count and
backoff base are currently constants (`defaultModelRetries = 3`,
`defaultModelRetryBackoff = 2s` in `runner/runner.go`).
