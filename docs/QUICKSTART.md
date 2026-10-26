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

### A Kubernetes cluster

You need a reachable cluster (kubectl in `~/.kube/config` pointing at it).
For local dev the Makefile creates one: `make kind-up` (kind 1.34,
agent-sandbox, KubeArmor), so the next two subsections are already done.

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
> (`cniNetworkStatus` in `internal/controller/loop_controller.go`)

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

The install is done when the rollout reports success and the operator pod is
Ready in `coxswain-system`.

## 3. The GitHub token Secret

The deliver Job (the operator-owned Job that opens the Loop's PR) opens the
Loop's PR through the Gitea/GitHub API using a `kubernetes.io/basic-auth`
Secret named **`git-cred`** (username `x-access-token`). The token is a
fine-grained GitHub token scoped to the sandbox repo, and it must never be
printed, `cat`'d, or placed on a command line (D55). In GitHub, create a
fine-grained personal access token with Contents and Pull requests both set
to Read and write on the repo the Loop will use, then save it to `~/.config/coxswain/github-token` (mode 600).
Two ways to create the Secret:

**Option A — the helper (preferred).** Put the token in
`~/.config/coxswain/github-token` (mode `600`; the only credential a script
may read, D55), then:

```sh
# <namespace> = the namespace the Loop runs in (default: the loop's namespace)
hack/github-secret.sh <namespace>            # secret name defaults to git-cred
```

The helper reads the token from the file via `--from-file` (never the
command line) and applies the Secret itself — you do not run the kubectl
command below; it is shown so you can see what the helper does:

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
reads it from the Loop's namespace). If `spec.workspace.gitCredentialSecret`
points at a missing Secret (or is left empty), the deliver Job fails and the
Loop's PR is not opened. With the field left empty the git push is attempted
unauthenticated and the PR create call has no API auth; with a Secret name
that does not exist the credential is mounted as a Secret **volume** (not an
env), so the deliver pod is stuck in `ContainerCreating` with `FailedMount`
events (kubelet: `secret "…" not found`) until the Job's deadline. Either way
the failure is recorded on the Loop as `Delivered=False` reason
`DeliveryFailed` (the operator's deliver read-back, `ensureDeliverReadback`
in `internal/controller/loop_deliver_job.go`), and the supported way back is
the `coxswain.io/redeliver` annotation.

## 4. A first Loop

Apply a minimal Loop CR (a `coxswain.wattu.com/v1alpha1` Loop). The operator
creates the agent-sandbox workspace for it, the runner plans → implements →
verifies inside the sandbox, and on success the Loop's PR is opened by the
deliver Job. The agent needs a model endpoint: create a Secret in the
Loop's namespace holding the model API key under the key `model-key` (§9
Step 7 shows one), then set `spec.agent.endpointSecretRef` to that Secret's name
and `spec.agent.modelEndpoint` to the endpoint's `host:port`. The CRD requires
both together, and both are immutable, so a new endpoint means a new Loop.
`config/samples/loop-fix-failing-test.yaml` shows the two fields as a
commented-out example.

`config/samples` has a working example:

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
   success the deliver Job opens the PR (using the `git-cred` Secret from
   section 3) while the Loop is held at `Verifying`; the phase becomes
   `Succeeded` only once delivery is terminal (the PR is open, or delivery
   failed).
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
kubectl cox logs <loop> --component agent|proxy|verify|deliver  # stream a component's logs (default: agent)
kubectl cox plan <loop>         # read the plan (.coxswain/PLAN.md) from the workspace
kubectl cox diff <loop>         # show the plan diff (current vs. last approved plan)
kubectl cox approve <loop>      # approve the plan (set the PlanApproved condition)
kubectl cox reject <loop> --reason <reason>  # reject the plan (set the PlanRejected condition)
kubectl cox pause <loop>        # set spec.suspend=true
kubectl cox resume <loop>       # clear spec.suspend (set coxswain.io/resume for a Stall/Budget pause)
kubectl cox redeliver <loop>    # re-trigger delivery for a failed Loop (I52 annotation)
kubectl cox fork <name> --loop <source> --iteration <n>  # fork from a checkpoint
kubectl cox memory export <repo-url> -o backup.tar.gz  # back up the memory PVC (import/delete: OPERATOR.md)
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

`--check` replaces the fork's whole acceptance-check list (repeat the flag
to set several); without it the fork inherits the source's checks.

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

- The operator restores the fork's OWN workspace PVC from the source's
  checkpoint snapshot at the fork's start (the controller's `spec.forkOf`
  handling).
- The source's snapshot for that iteration must exist (an entry in the
  source's `status.checkpoints`) and be ReadyToUse. Not ready: the Restore
  condition is `False` (reason `RestoreSourceNotReady`) and the operator
  retries. Absent: the Restore condition is `False` (reason
  `RestoreSourceMissing`) and the fork is terminal. On a cluster without the
  VolumeSnapshot CRD the reason is `VolumeSnapshotCRDMissing` (no checkpoint
  can exist there), with the same terminal outcome.

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

**Tuning:** set `spec.loop.modelTimeoutSeconds` (1-3600) to change the
per-request timeout; the operator passes it to the agent as
`COX_MODEL_TIMEOUT` (F46). Absent keeps the 120s default. The worst case is
then 4 x the value + 14s of backoff. For a local 27B-class model under load,
a legitimate response can approach 120s (#124 saw a 60s worst case); if you
see spurious `ModelUnavailable` conditions on a healthy backend, raise the
timeout or reduce the load or the context size. The retry count and backoff base are currently constants
(`defaultModelRetries = 3`, `defaultModelRetryBackoff = 2s` in
`runner/runner.go`).

## 9. Calico + KubeArmor VM (both gates enforcing)

This section sets up a dedicated VM where both gates (Calico NetworkPolicy
+ KubeArmor BPF-LSM) are live with no escape hatches, as a production
deployment requires. (For comparison: the e2e runs use kind's default
kindnet CNI plus the `--allow-unenforced` / `--allow-unenforced-network`
escape hatches.) Both gates live means:

- **NetworkEnforced=True** — an enforcing CNI (Calico) polices pod →
  host-network egress. The operator's CNI probe confirms this.
- **PolicyEnforced=True** — KubeArmor (BPF-LSM) enforces the Loop's
  effective policy (file, network, process, capabilities). The operator's
  eBPF enforcer confirms this.

This section walks through setting up a dedicated VM with Calico +
KubeArmor and running a Loop with both gates enforcing.

### Prerequisites

- A dedicated VM on the LAN (e.g. Proxmox LXC or VM) with:
  - **BPF-LSM enabled in the kernel** (the host kernel must have
    `CONFIG_BPF_LSM=y`). Verify: `cat /sys/kernel/security/lsm` must
    include `bpf` in the comma-separated list.
  - **Docker** (or a container runtime) for kind.
  - **SSH access** from the devbox.
  - **Network reachability** to the vLLM model endpoint (e.g.
    `192.168.1.20:8000`).
- The coxswain operator built from `main` (the release commit).

### Step 1: Install tools on the VM

```bash
# kind (pinned v0.30.0; v0.34.0 does not exist)
curl -sfSL https://github.com/kubernetes-sigs/kind/releases/download/v0.30.0/kind-linux-amd64 \
  | sudo tee /usr/local/bin/kind >/dev/null && sudo chmod +x /usr/local/bin/kind

# kubectl (pinned v1.34.0)
curl -sfSL https://dl.k8s.io/release/v1.34.0/bin/linux/amd64/kubectl \
  | sudo tee /usr/local/bin/kubectl >/dev/null && sudo chmod +x /usr/local/bin/kubectl
```

### Step 2: Create the kind cluster (Calico CNI)

```bash
# The config disables kindnet; Calico is installed separately.
# The extraMounts give EVERY node the same host dir for the RWX memory PVC
# (see "RWX memory", after Step 6); drop them if you do not use Loop memory. Create the
# host dir first: sudo mkdir -p /srv/local-path-shared && sudo chmod 0777 /srv/local-path-shared
cat > /tmp/kind-calico.yaml <<'EOF'
kind: Cluster
apiVersion: kind.x-k8s.io/v1alpha4
networking:
  disableDefaultCNI: true
  podSubnet: 10.244.0.0/16
nodes:
  - role: control-plane
    extraMounts: [{hostPath: /srv/local-path-shared, containerPath: /var/local-path-shared}]
  - role: worker
    extraMounts: [{hostPath: /srv/local-path-shared, containerPath: /var/local-path-shared}]
  - role: worker
    extraMounts: [{hostPath: /srv/local-path-shared, containerPath: /var/local-path-shared}]
EOF

kind create cluster --name coxswain-ref --image kindest/node:v1.34.0 --config /tmp/kind-calico.yaml

# Calico v3.30.1 (pinned)
kubectl --context kind-coxswain-ref apply -f \
  https://raw.githubusercontent.com/projectcalico/calico/v3.30.1/manifests/calico.yaml

# Wait for the node to become Ready (Calico must be up before anything else)
kubectl --context kind-coxswain-ref get nodes -w
# (wait until READY=Ready; the node transitions NotReady → Ready once Calico is up)
```

### Step 3: Install KubeArmor (BPF-LSM)

```bash
# karmor CLI (pinned v1.4.9; the installer lives in kubearmor-client, not
# KubeArmor, whose install.sh is a 404)
curl -sfSL https://raw.githubusercontent.com/kubearmor/kubearmor-client/main/install.sh | \
  sudo bash -s -- -b /usr/local/bin v1.4.9

# KubeArmor v1.7.5 with BPF-LSM enforcement + network/process/file viz
sudo karmor install -b all --viz process,file,network --tag v1.7.5

# Verify the enforcer is BPF-LSM
karmor probe
# Expected: "Active LSM: BPFLSM" and node label kubearmor.io/enforcer: bpf
```

### Step 4: Install agent-sandbox

```bash
# The v1.0.4 release manifest (as the Makefile); it creates the namespace.
kubectl --context kind-coxswain-ref apply -f \
  https://github.com/kubernetes-sigs/agent-sandbox/releases/download/v1.0.4/sandbox.yaml
kubectl --context kind-coxswain-ref -n agent-sandbox-system rollout status deploy/agent-sandbox-controller --timeout=120s
```

### Step 5: Build and load the coxswain images

On the devbox (where the build dependencies live):

```bash
BUILD_TAG="d61-$(git rev-parse --short HEAD)-$(date -u +%Y%m%d%H%M%S)"

make proxy-build PROXY_IMG=coxswain-tool-proxy:$BUILD_TAG \
     EGRESS_IMG=coxswain-egress-proxy:$BUILD_TAG \
     PROXY_IMG=coxswain-model-proxy:$BUILD_TAG
make runner-build RUNNER_IMG=coxswain-runner:$BUILD_TAG
make docker-build IMG=coxswain-controller:$BUILD_TAG

# Transfer to the VM and load into the kind cluster
for img in controller runner egress-proxy model-proxy tool-proxy; do
  docker tag coxswain-$img:$BUILD_TAG ghcr.io/papawattu/coxswain-$img:$BUILD_TAG
done
docker save ghcr.io/papawattu/coxswain-{controller,runner,egress-proxy,model-proxy,tool-proxy}:$BUILD_TAG \
  | ssh <vm> "sudo docker load"

ssh <vm> "
  for img in controller runner egress-proxy model-proxy tool-proxy; do
    kind load docker-image ghcr.io/papawattu/coxswain-$img:$BUILD_TAG --name coxswain-ref
  done
"
```

### Step 6: Install the operator (NO escape hatches)

```bash
# Render install.yaml with the build tag
make build-installer IMG=ghcr.io/papawattu/coxswain-controller:$BUILD_TAG
# With RWX memory (see "RWX memory", after Step 6): add MEMORY_STORAGE_CLASS=local-shared
#   make build-installer IMG=ghcr.io/papawattu/coxswain-controller:$BUILD_TAG MEMORY_STORAGE_CLASS=local-shared

# Copy to the VM and apply (NO --allow-unenforced, NO --allow-unenforced-network)
scp dist/install.yaml <vm>:/tmp/install-d61.yaml
ssh <vm> "kubectl --context kind-coxswain-ref apply -f /tmp/install-d61.yaml"
ssh <vm> "kubectl --context kind-coxswain-ref -n coxswain-system rollout status deploy/coxswain-controller-manager --timeout=120s"
```

### RWX memory (optional; Loop memory across nodes)

Loop memory is a ReadWriteMany PVC, and kind's default class is RWO-only. Three
pieces make it work:

1. **The same host dir mounted at the same path on EVERY node**, a
   cluster-creation property (the `extraMounts` in Step 2). Without it each node
   has a private copy and a Loop on another node sees none of the lessons.
2. **`hack/rwx-storage.sh`** installs a second, pinned local-path provisioner
   and the `local-shared` StorageClass (volumeBindingMode `Immediate`):

   ```bash
   hack/rwx-storage.sh --context kind-coxswain-ref
   ```

   Re-running it is safe. See `docs/OPERATOR.md` ("RWX storage for the memory PVC").
3. **Point the operator at the class.** On the `install.yaml` path, render it
   in Step 6 with `MEMORY_STORAGE_CLASS=local-shared` (below); on Helm use
   `manager.args`.

### Step 7: Create Secrets

```bash
ssh <vm> "
  NS=d61
  kubectl --context kind-coxswain-ref create namespace $NS
  # Model secret (vLLM is no-auth; the metering proxy needs a non-empty key)
  kubectl --context kind-coxswain-ref -n $NS create secret generic beta-model-creds \
    --from-literal='model-key=d61-dummy-key' \
    --from-literal='MODEL_BASE_URL=http://192.168.1.20:8000'
  # GitHub secret (the token file must be on the VM at ~/.config/coxswain/github-token)
  bash hack/github-secret.sh $NS
"
```

### Step 8: Run a Loop and verify both gates

```bash
ssh <vm> "
  kubectl --context kind-coxswain-ref apply -f /tmp/d61-loop.yaml
  # Watch the Loop
  kubectl --context kind-coxswain-ref -n d61 get loop d61-loop -w
"
```

**Expected conditions (both gates live, no escape hatches):**

```
PolicyEnforced=True    reason=Enforcing     message=the eBPF engine is enforcing the Loop's effective policy
NetworkEnforced=True   reason=CNIEnforced   message=every target was BLOCKED by the NetworkPolicy (pod -> host-network egress is policed)
ModelAvailable=True    reason=Recovered     message=model backend reachable
```

**Expected Loop outcome:** `phase=Succeeded` (the agent implements the goal,
the verify Job runs the acceptance checks, and the Loop reaches Succeeded).

### This run's evidence (post-#155 merge)

A fresh proof ran on the dedicated VM (host `192.168.1.61`) from HEAD
`68b9ac8` (#155 merged to main) — operator installed with **no**
`--allow-unenforced` / `--allow-unenforced-network` flag (images tagged
`d61-68b9ac8-20261015182601`). The Loop `d61-proof` reached `Succeeded`
and `Delivered` (draft PR on the sandbox repo). Evidence:

- **BPF-LSM enforcer** (`karmor probe`): `Active LSM: BPFLSM`, node label
  `kubearmor.io/enforcer=bpf`, kernel `7.0.0-28-generic`. (Not AppArmor.)
- **Gates live**: `PolicyEnforced=True (Enforcing)`, `NetworkEnforced=True
  (CNIEnforced)`, `ModelAvailable=True (Recovered)`, `Delivered=True`.
- **PolicyEnforced = the KubeArmor-enforced DNS/network fence** (the exact
  thing the gate claims, shown enforcing): the operator's KubeArmorPolicy for
  the agent is `action: Block` (default-deny) with a DNS Allow for **only**
  `<loop>-proxy.<ns>.svc` + `model-endpoint`. From inside a running agent
  container, a direct DNS lookup for a non-allowlisted name is **BLOCKED by
  KubeArmor (BPF-LSM)**:
  - `getent hosts github.com` → **exit 2** (no output)
  - `curl -m 10 https://github.com` → `curl: (6) Could not resolve host: github.com`, **exit 6**
  - the KubeArmor audit emits the Block alert: `Enforcer: BPFLSM`, `Action:
    Block`, `Result: "Permission denied"`, `Operation: Network`, `Resource:
    github.com` (see `karmor-dns-fence-block.txt` / `karmor-dns-fence-logs.json`)
  - **allowed control**: `getent hosts <loop>-proxy.<ns>.svc` → resolves to the
    model proxy's ClusterIP, **exit 0** (the in-fence name resolves)
  This is the KubeArmor DNS fence — **not** an exec-deny: agent exec is
  **unrestricted by design** (a `kubectl exec` of a non-allowlisted binary is
  not blocked — see `karmor-exec-test-honest.txt` and the `Known limitations`
  note).
- **Blocked egress (CNI gate)**: a pod matching the verify selector attempts a
  **direct** egress to `github.com:443` (bypassing the proxy) — **BLOCKED by
  Calico** (no route).
- **Allowed egress via proxy**: the verify NetworkPolicy allows egress **only**
  to kube-dns (53) and the egress proxy (3128) — no direct egress. The
  deliver Job's git push + GitHub API calls went **through the egress proxy**
  (SNI log: `github.com:443`, `api.github.com:443`, all `verdict=allowed`).

Full evidence (the pinned versions + probe output, the Loop's full status +
gate conditions, the KubeArmorPolicy, the verify NetworkPolicy, the
blocked-egress and egress-proxy SNI logs) is under
`.samples/d61-156-<timestamp>/` (see the PR body for the exact path).

### Pinned versions

| Component     | Version | Source |
|---------------|---------|--------|
| kind          | v0.30.0 | `github.com/kubernetes-sigs/kind/releases` (v0.34.0 does not exist) |
| kindest/node  | v1.34.0 | `kindest/node:v1.34.0`, passed with `--image` |
| kubectl       | v1.34.0 | `dl.k8s.io` (client) |
| Calico        | v3.30.1 | `projectcalico/calico/manifests/calico.yaml` |
| KubeArmor     | v1.7.5  | `karmor install --tag v1.7.5` |
| karmor CLI    | v1.4.9  | `kubearmor/kubearmor-client/main/install.sh` |
| agent-sandbox | v1.0.4  | release `sandbox.yaml`; Deployment `agent-sandbox-controller` (the original VM proof ran v0.5.2) |

### Known limitations

- **BPF-LSM wedge risk (ADR-0007 F2):** never restart/kill the KubeArmor
  agent while Loops run. A BPF wedge locks the node's LSM enforcement and
  requires a host reboot to clear.
- **`PolicyEnforced` is the KubeArmor-enforced DNS/network fence, not an
  exec-deny:** the operator's KubeArmorPolicy for the agent is `action: Block`
  (default-deny) with a DNS Allow for **only** `<loop>-proxy.<ns>.svc` +
  `model-endpoint`. The eBPF engine (BPF-LSM) enforces that fence: a direct DNS
  lookup for a non-allowlisted name (e.g. `github.com`) is **blocked**
  (`getent`/`curl` fail, and the KubeArmor audit emits an `Action: Block` /
  `Result: "Permission denied"` alert for `Operation: Network`). Agent **exec**
  is **unrestricted by design** — the policy has no exec-deny rule, so a
  `kubectl exec` of a non-allowlisted binary is **not** blocked (observed: exit
  0, 0 exec-related Block alerts). Arbitrary-host network egress is blocked both
  by this fence (DNS denied) and by Calico's NetworkPolicy (the separate
  `NetworkEnforced` gate). See the evidence (`karmor-dns-fence-block.txt`,
  `karmor-exec-test-honest.txt`).
