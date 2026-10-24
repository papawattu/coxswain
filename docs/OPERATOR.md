# Operator runbook

This document is the day-to-day reference for running the coxswain operator:
installing it, reading its metrics, reading its logs, and debugging a stuck
Loop. For the one-time setup (agent-sandbox, KubeArmor, the network gate),
see `docs/QUICKSTART.md`.

## Contents

- [Install](#install)
- [Metrics](#metrics)
- [Logs](#logs)
- [Shared memory storage (Phase 5)](#shared-memory-storage-phase-5)
- [Reading Loop status](#reading-loop-status)
- [Debugging a stuck Loop](#debugging-a-stuck-loop)
- [Uninstall](#uninstall)
- [What the sandbox can and cannot reach](#what-the-sandbox-can-and-cannot-reach)
- [Upgrade](#upgrade)

---

## Install

### From a release (recommended)

The `v*` release tags publish the public images and the `install.yaml` that
points at them. Fetch and apply:

```sh
# Pin the tag (the alphas are prereleases — GitHub's releases/latest skips
# them, so releases/latest is not the right URL for the alpha line).
TAG=v0.1.0-alpha.1
curl -sfL "https://github.com/papawattu/coxswain/releases/download/${TAG}/install.yaml" \
  -o install.yaml
kubectl apply -f install.yaml
kubectl rollout status deploy/coxswain-controller-manager -n coxswain-system
```

### From a local checkout (dev)

```sh
# Build the installer manifest with a local image tag.
make build-installer IMG=ghcr.io/papawattu/coxswain-controller:dev
# (This renders dist/install.yaml; it is NOT committed — use the rendered
# file directly, or load the image into kind first.)
kubectl apply -f dist/install.yaml
```

For kind, load the image into the cluster before applying:

```sh
make kind-up
make docker-build IMG=ghcr.io/papawattu/coxswain-controller:dev
make deploy IMG=ghcr.io/papawattu/coxswain-controller:dev
```

### Helm chart (B5a)

The chart is generated with `kubebuilder edit --plugins=helm/v2-alpha` and
committed to `dist/chart/`. To deploy via Helm:

```sh
make install-helm
make helm-deploy IMG=ghcr.io/papawattu/coxswain-controller:<tag>
```

The chart carries the same resources as `install.yaml` (CRDs, manager
Deployment, Service, RBAC, the I85 admission VAP + binding, the D38
coxswain-cni-probe Namespace/RBAC). The release namespace is created by
Helm (`--create-namespace`); the helper Roles (loop-admin/editor/viewer)
are opt-in (`rbac.helpers.enabled=true` in values.yaml; `install.yaml`
ships them enabled).

To verify the chart without a cluster:

```sh
make chart-check   # helm lint + helm template + install.yaml coverage
```

### Verify the install

```sh
# The operator pod is running and Ready.
kubectl -n coxswain-system get pods
kubectl -n coxswain-system rollout status deploy/coxswain-controller-manager

# The CRDs are registered.
kubectl get crd loops.coxswain.wattu.com agentpolicies.coxswain.wattu.com

# The operator log shows the CNI probe result (the network gate).
kubectl -n coxswain-system logs deploy/coxswain-controller-manager | grep -i 'cni\|network'
```

If the CNI does not enforce NetworkPolicy (kind's default `kindnet`), the
operator's log will show the probe failure and all Loops will be held
Suspended with `NetworkEnforced=False reason=CNIUnenforced`. To run anyway
on a non-enforcing CNI (dev only), add the escape hatch (see
`docs/QUICKSTART.md` §1 "Network gate").

---

## Metrics

The operator exposes Prometheus metrics at `/metrics` on the manager's
metrics port (the shipped manifests bind `:8443` with HTTPS; the manager flag
is `--metrics-bind-address`, default `0` = disabled — the `config/default`
kustomize patch and the Helm chart both set `:8443`). The metrics Service is
`coxswain-controller-manager-metrics-service` in the `coxswain-system`
namespace.

### How to scrape

**In-cluster (Prometheus / Prometheus Operator):** enable the
ServiceMonitor in the Helm chart (`prometheus.enabled=true` in values.yaml)
or add a `PodMonitor` for the `coxswain-system` namespace. The Service
selects the manager pod by the `control-plane: controller-manager` label.

**Out-of-cluster (curl):** port-forward the metrics Service:

```sh
kubectl -n coxswain-system port-forward svc/coxswain-controller-manager-metrics-service 8080:8443
curl -sk https://localhost:8080/metrics | grep coxswain_
```

(HTTPS requires the metrics auth token; use `--metrics-secure=false` on the
manager for a plain-HTTP scrape in dev.)

### Reading /metrics with a bound token (reviewer's recipe)

The metrics endpoint uses `filters.WithAuthenticationAndAuthorization`
(controller-runtime). To read it with `curl`, you need a token whose
ServiceAccount is bound to the `coxswain-metrics-reader` **ClusterRole**
(installed by `dist/install.yaml` / the Helm chart). A namespaced Role
cannot grant the `nonResourceURL /metrics` — only a ClusterRole can.

```sh
# 1. Create a ServiceAccount for metrics access
kubectl -n coxswain-system create sa metrics-reader

# 2. Bind the existing coxswain-metrics-reader ClusterRole to the SA
#    (the ClusterRole is installed by dist/install.yaml; it grants
#    nonResourceURLs: ["/metrics"] verbs: ["get"]).
kubectl create clusterrolebinding metrics-reader-binding \
  --clusterrole=coxswain-metrics-reader \
  --serviceaccount=coxswain-system:metrics-reader

# 3. Create a token for the SA (minimum duration: 10 minutes)
TOKEN=$(kubectl -n coxswain-system create token metrics-reader --duration=10m)

# 4. Port-forward and read /metrics
kubectl -n coxswain-system port-forward \
  svc/coxswain-controller-manager-metrics-service 8080:8443 &
PF_PID=$!
sleep 3
curl -sk https://localhost:8080/metrics -H "Authorization: Bearer $TOKEN" | grep coxswain_
kill $PF_PID
```

### The B5a metrics

| Metric | Type | Labels | Meaning |
|--------|------|--------|---------|
| `coxswain_loop_phase_transitions_total` | Counter | `phase` | Total Loop phase transitions (new-phase advances), by destination phase. Bumped once per NEW-phase transition (a no-op re-set to the same phase does not bump). |
| `coxswain_loop_iterations_total` | Counter | — | Total Loop iteration increments (status.iteration advances: the verify-iterate and re-plan paths). This is the "iterations per Loop" metric. |
| `coxswain_verify_results_total` | Counter | `result` (pass\|fail\|infra) | Total verify outcomes, by result. The verify pass rate is computed as `pass / (pass + fail)`. |
| `coxswain_loops_completed_total` | Counter | `result` (succeeded\|failed), `reason` | Total Loops that reached a terminal phase, by result and reason (verify_passed, max_iterations, stalled, tampered, verify_infra). |
| `coxswain_loop_stalls_total` | Counter | — | Total times the P2e stall gate fired (a new verify Job reached k consecutive identical check outputs). |
| `coxswain_loops_active` | Gauge | `phase` | Loops currently in a non-terminal phase, by phase. Counted from the manager's cache at scrape time (custom Collector, not set on reconcile). Terminal phases (Succeeded, Failed) are excluded. |
| `coxswain_loop_tokens_total` | Counter | `kind` (prompt\|completion) | Total model tokens consumed, as folded into `status.budget` from the proxy reading, by kind. Bumped by the delta on each same-bootID, no-drop reading. |
| `coxswain_memory_writes_total` | Counter | `outcome` (written\|failed) | Memory-writer outcomes (F30), bumped once per Loop on the transition that sets the `MemoryWriteDone` condition: `written` when the writer Job succeeded (including a write where every lesson was dropped), `failed` when it failed. A Loop with no write (ineligible, no lessons, or still running) bumps nothing. |
| `coxswain_memory_lessons_total` | Counter | `verdict` (kept\|static-drop\|curator-drop) | Lessons in successful writes' summaries (F30), bumped with `written`: `kept` = written to the PVC; `curator-drop` = the curator dropped it or was unavailable; `static-drop` = any other drop (static write policy, ineligible outcome, duplicate). |

**Alerting examples** (PromQL):

```promql
# Loops that have stalled more than once in the last hour.
sum(increase(coxswain_loop_stalls_total[1h])) by (namespace) > 1

# Memory-write rate (writes per hour) and the failed share.
sum(increase(coxswain_memory_writes_total[1h])) by (outcome)
sum(rate(coxswain_memory_writes_total{outcome="failed"}[1h])) / sum(rate(coxswain_memory_writes_total[1h]))

# Share of proposed lessons the curator or static policy drops.
1 - sum(increase(coxswain_memory_lessons_total{verdict="kept"}[1d])) / sum(increase(coxswain_memory_lessons_total[1d]))

# Token consumption rate (tokens/second) over the last 5 minutes.
sum(rate(coxswain_loop_tokens_total[5m]))

# Loops stuck in Implementing for more than 30 minutes (no phase transition).
coxswain_loops_active{phase="Implementing"} > 0
and
(
  time() - timestamp on (namespace) (
    max by (namespace) (
      kube_pod_created{namespace="coxswain-system"}
    )
  ) > 1800
)

# Verify pass rate (pass / (pass + fail)).
sum(coxswain_verify_results_total{result="pass"})
/
sum(coxswain_verify_results_total{result=~"pass|fail"})

# Tokens per successful Loop completion.
sum(coxswain_loop_tokens_total)
/
sum(coxswain_loops_completed_total{result="succeeded"})
```

**Dashboard query examples:**

```promql
# Phase transitions by phase (cumulative, since process start).
sum(coxswain_loop_phase_transitions_total) by (phase)

# Total iterations (cumulative, since process start).
sum(coxswain_loop_iterations_total)

# Total tokens consumed (prompt + completion, cumulative).
sum(coxswain_loop_tokens_total)

# Active Loops by phase.
sum(coxswain_loops_active) by (phase)

# Loops completed by result and reason.
sum(coxswain_loops_completed_total) by (result, reason)
```

---

## Logs

The operator logs are the first place to look when a Loop misbehaves.

```sh
# Stream the operator log (the controller-manager container).
kubectl -n coxswain-system logs deploy/coxswain-controller-manager -c manager -f

# Filter for a specific Loop.
kubectl -n coxswain-system logs deploy/coxswain-controller-manager -c manager \
  | grep 'loop=<name>'

# The operator logs structured key-value pairs; the Loop name/namespace are
# logged on every reconcile line.
```

### Log levels

The manager's log level is controlled by the `--zap-log-level` flag
(controller-runtime's built-in flag; default `info`). To get debug logs for a
specific Loop:

```sh
kubectl -n coxswain-system patch deploy coxswain-controller-manager \
  --type=json -p '[{"op":"replace","path":"/spec/template/spec/containers/0/args/-","value":"--zap-log-level=debug"}]'
```

(Revert by replacing the flag value with `--zap-log-level=info`.)

### Component logs

The `kubectl cox logs` command streams the **sandbox component** logs
(agent, proxy, verify, deliver) — not the operator log:

```sh
kubectl cox logs <loop> agent    # the runner (agent) log
kubectl cox logs <loop> proxy    # the model proxy sidecar log
kubectl cox logs <loop> verify   # the verify Job log
kubectl cox logs <loop> deliver  # the deliver Job log
```

---

## Shared memory storage (Phase 5)

### Memory flags

- `--memory-writer-image` (required for lesson writes): the memory-writer Job
  runs `/usr/local/bin/coxswain-memory-write`, which exists only in the
  operator's own image. The shipped kustomize base, `install.yaml` and the Helm
  chart set it to the manager's image automatically (F19);
  `hack/memory-writer-image-test.sh` guards that. Only override it with another
  build of the operator image.
- `--memory-storage-class` (optional): an RWX-capable StorageClass for the
  memory PVC (kind's default `rancher.io/local-path` is RWO-only). On the
  kustomize / `install.yaml` path, render with
  `make build-installer IMG=... MEMORY_STORAGE_CLASS=local-shared` (F22): the
  script appends the flag in a temp overlay, no tracked file is edited, and
  unset means no flag. Other flags: `MANAGER_EXTRA_ARGS="--flag[=value] ..."`
  (one line; each token matched whole). `--allow-unenforced` is allowed with a
  WARNING for lab clusters without KubeArmor (a release build must never pass
  it). Refused: `--allow-unenforced-network` (always), any `--*-image` flag (the
  images are pinned by IMG's tag), and any flag already in the rendered args.
  On Helm use `manager.args`.

### RWX storage for the memory PVC (`hack/rwx-storage.sh`)

The memory PVC is ReadWriteMany. kind, k3d and k3s ship an RWO-only default
class, so the PVC would stay Pending. `hack/rwx-storage.sh` installs a second,
pinned rancher local-path-provisioner (own namespace `coxswain-rwx-storage`,
own provisioner name, `sharedFileSystemPath` mode) and a `local-shared`
StorageClass on any cluster, without touching the cluster's own provisioner or
default class:

```sh
hack/rwx-storage.sh --context kind-coxswain          # or --kubeconfig FILE
# then run the operator with --memory-storage-class=local-shared
# (install.yaml: make build-installer IMG=... MEMORY_STORAGE_CLASS=local-shared)
# (import with `cox memory import ... --storage-class local-shared` needs the
# P5d CLI, PR #186; until that merges, only the operator flag applies)
```

It is idempotent and refuses to replace an existing StorageClass that has a
different provisioner. `--shared-path` (default `/var/local-path-shared`) is the
`sharedFileSystemPath`. If one host dir backs several clusters, give each its
own subdirectory (`--shared-path /var/local-path-shared/<cluster>`).

**Prerequisite on a multi-node cluster: the same host directory must be mounted
at that path on EVERY node.** The script mounts nothing itself, and the mount is
a cluster-creation property. Without it each node has its own private copy and a
pod on another node sees an empty volume. (On a single-node cluster nothing is
needed; on a real cluster use NFS/CephFS/etc. at that path.)

```sh
# k3d: mount one host dir into every node at cluster creation
mkdir -p /srv/local-path-shared
k3d cluster create mylab --agents 2 \
  --volume /srv/local-path-shared:/var/local-path-shared@all
```

```sh
# kind: create the host dir first, then mount it on every node
mkdir -p /srv/local-path-shared
```

```yaml
# kind: extraMounts on every node in the cluster config
kind: Cluster
apiVersion: kind.x-k8s.io/v1alpha4
nodes:
- role: control-plane
  extraMounts: [{hostPath: /srv/local-path-shared, containerPath: /var/local-path-shared}]
- role: worker
  extraMounts: [{hostPath: /srv/local-path-shared, containerPath: /var/local-path-shared}]
```

**Posture: this is a dev/lab storage class, not a hardened one.** Upstream
local-path's design: its short-lived helper pods run as root with a hostPath
mount of `--shared-path` to create/remove volume dirs, and volumes are created
mode 0777. The helper pods carry no ServiceAccount token
(`automountServiceAccountToken: false`). Do not use it where tenants are
untrusted; use a real RWX class (NFS/CephFS) in production.

Images (`rancher/local-path-provisioner:v0.0.31`, `busybox:1.36`) must be
pullable or pre-loaded. Tested by `make rwx-storage-test` (real script, stubbed
kubectl).

**First Loop on a new repo:** the first Loop on a brand-new repository may
run without memory (no lessons seeded from prior Loops) while its new memory
PVC binds. This is harmless: there are no prior lessons to seed yet, so the
agent starts with an empty memory. Once the PVC binds (the `local-shared`
class provisions it on the first request), subsequent Loops on the same repo
read the accumulated lessons. A one-shot Warning Event `MemoryPVCUnbound`
(listable via `kubectl get events`) flags the binding wait: the operator emits
it at most once per Loop. It is recorded on the Loop with the
`coxswain.io/memory-pvc-unbound-notified` annotation and is re-emitted only if
that annotation is cleared while the PVC is still unbound.

### Memory PVC lifecycle and removal (I101)

There is one memory PVC per (namespace, repo URL), named
`memory-<first 12 hex of sha256(repo URL as written in spec.workspace.repo)>`.
It is created by the first Loop on that repo and **persists after every Loop on
the repo is deleted**: deleting a namespace's Loops does not clear the lessons.
A PVC lives only in its own namespace, so the same repo in another namespace has
its own.

To start a repo's memory from scratch:

```sh
kubectl cox memory export <repo-url> -o backup.tar.gz   # optional: keep a copy
kubectl cox memory delete <repo-url> -n <namespace>
```

`delete` refuses while any Loop on that repo in the namespace is not
`Succeeded` or `Failed` (including `Paused` and one with no phase yet), or is
`Succeeded` / `Failed:Stalled` but has no `MemoryWriteDone` condition yet (its
memory writer Job starts after the terminal phase and would lose its lessons),
and names the Loops. Otherwise it deletes the PVC. A repo with no PVC prints a note.
The next Loop on the repo creates a fresh, empty PVC. (`kubectl delete pvc
memory-<hash>` also works, but has no Loop check.)

## Reading Loop status

The `kubectl cox describe` command is the primary way to read a Loop's
state:

```sh
kubectl cox describe <loop>
```

It shows:

- **Phase** (Pending / Planning / Implementing / Verifying / Succeeded /
  Failed / Paused) and the desired phase.
- **Iteration** (the current iteration number, 1-indexed).
- **Conditions** (Ready, Succeeded, Stalled, BudgetExceeded,
  ModelAvailable, NetworkEnforced, PolicyEnforced, Paused — each with its
  status, reason, and message).
- **Budget** (token usage, wall-clock time, cost, the cap that was hit).
- **Delivery** (the PR the deliver Job opened, or the failure reason).

The full audit trail (phase + verify-failure history) is:

```sh
kubectl cox history <loop>
```

The plan the agent wrote (`.coxswain/PLAN.md` in the workspace):

```sh
kubectl cox plan <loop>
```

### What `Succeeded` means (P5f, I98)

`Succeeded` means **verified AND delivery is terminal (or delivery is off)**.
With `spec.delivery.mode: PullRequest`, a Loop that passes verify is held at
`status.phase: Verifying` (condition `Verifying=True reason=Delivering`, the
sandbox Suspended) while the `Delivered` condition is in progress. When
`Delivered` becomes True (PR opened) or False (`DeliveryFailed`), both
`status.phase` and `status.desiredPhase` become `Succeeded`. A failed
delivery is therefore `Succeeded` + `Delivered=False` (retry with
`kubectl cox redeliver`). A script that waits for `phase=Succeeded` never
reads the Loop before its PR exists (or delivery has failed). With delivery
off, `Succeeded` is set as soon as verify passes. The memory-writer Job
starts only after the Loop is `Succeeded`, not during the hold.

### Key conditions to watch

| Condition | What it means |
|-----------|---------------|
| `status.phase=Succeeded` (no separate `Succeeded` condition type) | The goal was met: the verify checks passed and delivery is terminal (or delivery is off). |
| `Verifying=True reason=Delivering` | Verify passed and delivery is on: the Loop is held at phase `Verifying` (sandbox Suspended) until the `Delivered` condition is terminal. |
| `Failed=True reason=Stalled` | The P2e stall gate fired (k consecutive identical verify failures). |
| `Failed=True reason=BudgetExceeded` | A budget cap (tokens, wall-clock, cost) was hit. |
| `Paused=True reason=Stall` | The stall gate fired with `stallAction=Pause`; the Loop is held Paused until resumed. |
| `Paused=True reason=Budget` | A budget cap was hit with `onExceeded=Pause`; the Loop is held Paused until resumed. |
| `NetworkEnforced=False reason=CNIUnenforced` | The CNI does not enforce NetworkPolicy; the Loop is held Suspended (fail-closed). |
| `ModelAvailable=False reason=ModelUnavailable` | The model backend is unreachable or returned no response within the timeout. |

---

## Debugging a stuck Loop

A Loop is "stuck" when it is not making progress (no phase change, no
iteration advance) for longer than expected. The steps below walk through
the common causes.

### 1. Check the phase and conditions

```sh
kubectl cox describe <loop>
```

Look at the **Phase** and the **conditions**. The condition's `reason`
field is the most useful diagnostic:

- `ModelUnavailable` → the model backend is unreachable. Check the proxy
  pod (`kubectl -n <ns> get pods | grep <loop>-proxy`), the model endpoint
  (`spec.agent.modelEndpoint`), and the proxy log (`kubectl cox logs <loop>
  proxy`).
- `Stalled` → the verify Job is producing identical failures. Read the
  stall history (`status.stallHistory`) and the verify Job log (`kubectl
  cox logs <loop> verify`). The check output is in the verify Job's
  termination message.
- `BudgetExceeded` → a budget cap was hit. Read `status.budget` (the token
  counts, wall-clock time, cost) and `spec.budget` (the caps).
- `CNIUnenforced` → the network gate is not satisfied. The CNI does not
  enforce NetworkPolicy. Either install a enforcing CNI (Calico) or use the
  `--allow-unenforced-network` escape hatch (dev only).
- `PolicyEnforced=False` → the KubeArmor policy gate is not enforced. Check
  that KubeArmor is installed and the BPF-LSM hooks are active.

### 2. Read the verify Job log

The verify Job runs the acceptance checks. Its log (and the check
container's termination message) is the evidence for why the checks
failed:

```sh
kubectl cox logs <loop> verify
```

If the verify Job is still Running (not yet terminal), the Loop is in the
in-progress state (no decision is made until the Job completes). Check the
Job's status:

```sh
kubectl -n <ns> get jobs | grep <loop>
kubectl -n <ns> describe job <loop>-verify-<n>
```

### 3. Read the agent log

The agent (runner) log shows what the agent is doing (planning,
implementing, the model calls). If the agent is stuck in a phase, the log
will show the last model response or the last action:

```sh
kubectl cox logs <loop> agent
```

### 4. Check the sandbox pod

The sandbox pod (the agent-sandbox workspace) is where the agent runs. If
the pod is not Running, the Loop is stuck at the sandbox level:

```sh
kubectl -n <ns> get pods | grep <loop>
kubectl -n <ns> describe pod <loop>-sandbox
kubectl -n <ns> logs <loop>-sandbox -c agent
```

Common causes:

- **Pod stuck in ContainerCreating**: the PVC is not bound (check the
  PVC's status), or the node is not schedulable.
- **Pod is CrashLooping**: the init container (workspace clone, phase-init)
  is failing. Check the init container logs.
- **Pod is Running but the agent is idle**: the agent may be waiting on a
  model call that never returns. Check the proxy log and the model
  endpoint.

### 5. Check the operator log for the specific Loop

```sh
kubectl -n coxswain-system logs deploy/coxswain-controller-manager -c manager \
  | grep "loop=<name>"
```

The operator log shows the reconcile decisions (the phase advance, the
verify outcome, the budget fold, the stall gate). If the operator is not
reconciling the Loop (no log lines for it), check:

- The operator is running (`kubectl -n coxswain-system get pods`).
- The Loop is in a namespace the operator watches (the operator watches all
  namespaces; there is no `--namespace` flag to scope it — it would need a
  label selector added to the cache to be scoped).
- The Loop's finalizer is present (`status.finalizers` includes
  `coxswain.wattu.com/checkpoints`).

### 6. Pause and resume

To pause a running Loop (stop the phase machine, hold the sandbox
Suspended):

```sh
kubectl cox pause <loop>
```

To resume (set `spec.suspend=false`; for a Stall/Budget pause, also set
the `coxswain.io/resume` annotation to clear the pause condition):

```sh
kubectl cox resume <loop>
```

### 7. Redeliver

If the Loop reached Succeeded but the deliver Job failed (e.g. the
`git-cred` Secret was missing), re-trigger delivery:

```sh
kubectl cox redeliver <loop>
```

This sets the `coxswain.io/redeliver` annotation, which the operator picks
up on the next reconcile and re-runs the deliver Job.

---

## Uninstall

To remove the operator and all its resources:

**Order matters.** Delete the Loops (and wait for them to go) BEFORE removing
the operator. Each Loop carries the `coxswain.wattu.com/checkpoints` finalizer,
which only the running operator clears; with the operator gone, a Loop and its
namespace stay `Terminating` forever. (F25 removed the other wedge, a missing
VolumeSnapshot CRD, but an uninstalled operator still strands Loops.)

**Already stuck?** Re-install the operator (`kubectl apply -f install.yaml`); it
processes the finalizers and the namespaces finish deleting. Do not strip the
finalizer by hand unless you accept leaving checkpoint snapshots behind.

```sh
# 1. Remove any running Loops (this deletes the sandbox pods, the verify Jobs,
# the deliver Jobs, and the workspace PVCs). Wait until none are left.
kubectl get loops -A
kubectl delete loops --all-namespaces

# 2. Remove the operator (the CRDs, RBAC, the manager Deployment).
kubectl delete -f install.yaml

# 3. Remove the CRDs (if you want a clean cluster).
kubectl delete crd loops.coxswain.wattu.com agentpolicies.coxswain.wattu.com
```

For a Helm release:

```sh
make helm-uninstall   # helm uninstall coxswain -n coxswain-system
```

---

## What the sandbox can and cannot reach

This section is the operator's view of what a sandboxed agent is and is not
able to do or see. It records the security posture the operator can rely on
when choosing what to give a Loop, including the accepted MVP widenings and
the known gaps. The code references name functions and fields (not line
numbers, which drift); the merged ADR text under `docs/adr/...` is the stable
citation. Row-level citations to the ADR-0004/0005/0006/0007 amendments as
built (0004-A1, 0005-A1, 0006-A1, 0007-A2) resolve against the amendment PRs
that are still open (#218, #219, #221, #223); until those merge, check the
corresponding ADR section on `main` instead.

### What the sandbox can reach

The model, through the model proxy only. When the Loop declares a model
endpoint (`spec.agent.endpointSecretRef` set), the agent's only route to the
model is the model proxy: `COX_MODEL_BASE_URL` is set (in `agentPodSpec`) to
the proxy's in-cluster Service URL (via `proxyServiceURL`), and the agent's
egress NetworkPolicy names the proxy pod as a peer (built by
`modelProxyEgress`). A Loop with no endpoint has no model route at all —
`modelProxyEgress` adds no peer and no `COX_MODEL_BASE_URL` is set. The
proxy pod, not the agent, holds the model key: the model-credentials Secret
is mounted into the per-Loop proxy pod only, so the agent container never
carries a model key (0006-A1 row 17).

The network allows, through the egress proxy. When the effective policy has
network allows — or when the workspace-init clone must traverse the egress
proxy (an external repo before the first clone, see below) — the agent's
egress to the outside world runs through the per-Loop egress proxy, which
enforces the `host:port` allowlist at the CONNECT / SNI / Host layer
(ADR-0007, "I42 resolution": the per-Loop proxy decision and the CONNECT +
SNI/Host enforcement point; `ensureEgressProxy` creates the proxy for both
cases, and 0006-A1 row 16).
DNS is allowed to the agent pod (the `dnsPeer()` egress rule built in
`ensureNetworkPolicy`; 0006-A1 row 16).

The repo host — for an in-cluster repo only, on a namespace-wide widening.
When the repo is in-cluster (an IP-literal host or a `<svc>.<ns>.svc` host),
the agent pod's egress allowlist gets a repo peer (`repoPeer` in
`internal/controller/loop_controller.go`) so the workspace init container's
first clone can fetch. That peer is a **namespace-wide** rule: for a
`<svc>.<ns>.svc` host it is a `NamespaceSelector` on the repo's namespace, so
the agent container can reach **every pod in that namespace on the repo
port**, not just the git Service; for an IP-literal host it is a `/32`
IPBlock. NetworkPolicy has no per-container scoping, so the rule is
pod-wide, and the in-place "P3 (known limitation, MVP)" comment in
`ensureNetworkPolicy` records it as "Accepted for the MVP". The comment's
phrasing ("the AGENT container can also reach the repo host") understates
what the peer expresses — the widening is to the whole namespace on the
repo port. Note the adjacent "S3a" comment says the repo rule "does NOT
widen the agent's runtime egress" because the init container runs to
completion before the agent starts; that claim is the inaccurate one: the
rule the CNI enforces is pod-wide, so the widening is real (the two comments
disagree in spirit; the P3 one matches what a NetworkPolicy can express). For
an **external** repo (the default in the shipped samples) there is **no** repo
rule at all — NetworkPolicy cannot name a hostname — and the first clone goes
through the egress proxy (`workspaceInitProxyCreated`; the F55 gate above
waits for that proxy to be Ready).

The tool proxies. The allowlist adds one egress peer per tool proxy, one per
tool in the effective policy (the `toolProxyNetpolPeer` rule in
`ensureNetworkPolicy`; 0006-A1 row 16).

### What the sandbox cannot reach

The model endpoint, directly. A direct agent → model-endpoint connection is
dropped by the NetworkPolicy, because the endpoint is not in the agent pod's
allowlist (which is only the model proxy, the egress proxy when it exists —
when network allows exist, or when the external-repo init clone needs it —,
DNS, the in-cluster repo peer when there is one, and the tool proxies). The attempt is
**not** recorded in the activity audit (there is no engine-alert producer
for a CNI/NetworkPolicy drop) and is not metered (the call is dropped)
(0007-A2 row 26).

The ADR-0007 claim that eBPF enforces per-container egress so the agent
container is fenced from the proxy's model-endpoint allow is **not built**:
`emitKubeArmorPolicy` emits one pod-level union of every container's allows,
so the KubeArmor policy cannot distinguish the agent container from the
proxy container (the emitter's own comment says the per-container split is
enforced elsewhere; 0007-A2 row 26; 0006-A1 row 16).

Everything else. The agent pod's egress default-deny stands: anything not in
the allowlist above is dropped by the CNI (0007-A2 row 26; 0006-A1 row 16).
Inbound is closed the same way: the agent pod's NetworkPolicy has an empty
Ingress list, so no pod can reach the sandbox inbound, including another
Loop's agent (the ingress deny-all in `ensureNetworkPolicy`). In-cluster targets cannot be
allowed into the egress proxy at all: a `*.svc` name, a cluster DNS suffix,
or an in-CIDR IP literal is rejected at policy-validation time, and the
proxy's resolved-IP check rejects private/link-local/cluster-CIDR IPs as a
backstop (ADR-0007, "Resolved-IP check (SSRF defence)").

No credentials in the agent container. The only Secret volume the sandbox
pod ever carries is the git credential: it is mounted read-only into the
workspace init container for the first clone only — once the base SHA is
pinned it is omitted from the pod (the "credential volume exists ONLY for
the first clone" comment in `agentPodSpec`; asserted in
`loop_s3_workspace_test.go`), and the agent container never mounts it. The
agent container therefore holds no model key and no git credential at
runtime, and the pod sets `shareProcessNamespace: false`, so the init
container runs in its own PID namespace — the agent cannot see its processes
or read its memory. Two caveats, both recorded in the code:
the Basic-auth header built from the credential is visible in the init
git process's argv for that one command (a known limitation noted in
`workspaceInitContainer`), and the credential is readable in-process by the
init container itself while it runs. It also carries no service-account
token: the pod sets `automountServiceAccountToken: false` explicitly
(asserted in `loop_c1_hardening_test.go`), so the agent has no API-server
identity at all.

### How the operator reads the sandbox's self-report

The runner reports its phase claim through the agent container's
**termination message**, which the operator reads with a plain pod `Get`
(`readPhaseClaimFromTerminationMessage`); there is no `pods/exec` grant and
the operator never execs into the sandbox (0004-A1 rows 2–3; 0005-A1 row 2).
Verify failures feed back to the next iteration via the check container's
termination message (`verifyFailureSummary` on
`status.verify.lastFailureSummary`); the operator has no `pods/log` grant
and reads no pod logs (0005-A1 row 8).

### Known gaps (stated here so they are not missed)

- If the BPF-LSM label is removed from the node the sandbox actually landed
  on **while another labelled node keeps the gate's node facts true**, the
  running sandbox is not re-suspended by the gate; there is no post-start
  re-check anchored to the pod's landed node (0007-A2 A1 row 30). (If no
  labelled node remains, the gate reports `NodeNotEnforcing` and the sandbox
  **is** re-suspended.)
- A NetworkPolicy drop is unaudited: there is no producer that translates
  engine or CNI denials into the Q4 audit envelope (0007-A2 row 26,
  pointing at I107 row 25).
- The policy hash (`status.policy.effectiveHash`) is a current-value field,
  not a per-iteration record: it cannot show what the agent was allowed to
  do in iteration *n* once the policy changed (0007-A2 row 28).
- Domain fronting through an allowlisted CDN hostname is a recorded limit of
  the tunnel-only egress proxy, not solved (ADR-0007, "Known limits of
  tunnel-only mode").

## Upgrade

For upgrade notes (one-time migration steps, breaking changes), see
`docs/UPGRADE.md`. The general upgrade procedure:

```sh
# Build the new installer manifest.
make build-installer IMG=ghcr.io/papawattu/coxswain-controller:<new-tag>

# Apply (the CRDs are updated in place; the manager Deployment is rolled).
kubectl apply -f dist/install.yaml
kubectl rollout status deploy/coxswain-controller-manager -n coxswain-system
```

The operator is backward-compatible within the same API version
(`v1alpha1`): existing Loops are reconciled by the new operator without a
manual migration step (the status fields are additive; the new operator
ignores fields it does not know).
