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
  - [ClusterAgentPolicy (P6a)](#clusteragentpolicy-p6a)
  - [Learn mode: auditOnly policies (P6b)](#learn-mode-auditonly-policies-p6b)
  - [How the network probe measures enforcement (D103)](#how-the-network-probe-measures-enforcement-d103)
- [Reading the egress proxy log](#reading-the-egress-proxy-log)
- [Pinning Loops to a node pool (C2)](#pinning-loops-to-a-node-pool-c2)
- [k3s / flannel / SMB profile (C3)](#k3s--flannel--smb-profile-c3)
- [Upgrade](#upgrade)

---

## Install

### From a release (recommended)

The `v*` release tags publish the public images and the `install.yaml` that
points at them. Fetch and apply:

```sh
# Pin the tag. v0.2.0 is the latest release.
TAG=v0.2.0
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
Helm (`--create-namespace`); the helper Roles (loop-admin, loop-editor,
loop-viewer) are included in `install.yaml` by default; in the Helm chart
they are opt-in (`rbac.helpers.enabled=true` in values.yaml).

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
metrics port. The shipped manifests bind `:8443` with HTTPS. By default the
port is disabled (`--metrics-bind-address` defaults to `0`); the
`config/default` kustomize patch and the Helm chart both set it to `:8443`. The metrics Service is
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
(installed by `dist/install.yaml` / the Helm chart). The metrics endpoint is
a cluster-scoped URL (not namespaced), so access to it can only be granted by
a ClusterRole binding; a namespaced Role cannot grant it.

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
| `coxswain_loop_tokens_total` | Counter | `kind` (prompt\|completion) | Total model tokens consumed, as folded into `status.budget` from the proxy reading, by kind. Bumped by the token delta on each proxy reading that comes from the same proxy process (same boot ID) and did not drop (a dropped reading is not counted). |
| `coxswain_unmetered_requests_total` | Counter | none | Total model requests the proxy forwarded without a usage object, as folded into `status.budget` (they do not count toward the token or cost caps). The per-Loop view is the `MeteringDegraded` condition. |
| `coxswain_memory_writes_total` | Counter | `outcome` (written\|failed) | Memory-writer outcomes (F30), bumped once per Loop on the transition that sets the `MemoryWriteDone` condition: `written` when the writer Job succeeded (including a write where every lesson was dropped), `failed` when it failed. A Loop with no write (ineligible, no lessons, or still running) bumps nothing. |
| `coxswain_memory_lessons_total` | Counter | `verdict` (kept\|static-drop\|curator-drop) | Lessons in successful writes' summaries (F30), bumped with `written`: `kept` = written to the PVC; `curator-drop` = the curator dropped it or was unavailable; `static-drop` = any other drop (static write policy, ineligible outcome, duplicate). When the writer capped the summary (its marker `… N more lines (M lesson lines)` names the cut per-lesson lines, F80b), `kept` and the total dropped still come from the counts line, but the drop split covers only the lessons whose lines survived — the cut ones are left unattributed instead of being counted as `static-drop` (the Job log has the full list). |

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
kubectl cox logs <loop> --component agent    # the runner (agent) log
kubectl cox logs <loop> --component proxy    # the model proxy log (its own pod)
kubectl cox logs <loop> --component verify   # the verify Job log
kubectl cox logs <loop> --component deliver  # the deliver Job log
```

---

## Shared memory storage (Phase 5)

### Memory flags

- `--memory-writer-image` (required for lesson writes): the memory-writer Job
  runs a binary that exists only in the operator's own image, so this flag
  should point at the operator image — which the shipped kustomize base,
  `install.yaml` and the Helm chart already do; `hack/memory-writer-image-test.sh`
  guards that. Only override it with another build of the operator image.
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
read the accumulated lessons. A Warning Event `MemoryPVCUnbound`
(listable via `kubectl get events`) flags the binding wait. The Event is not
a one-shot (R39 I144): while the Loop runs and the PVC stays unbound it
re-announces every 5 minutes, because a single Event is invisible once the
cluster's `--event-ttl` (default 1h) has passed — and a class without
ReadWriteMany leaves the PVC unbound for far longer than that. The operator
schedules a reconcile for that window, so an otherwise idle Loop still
repeats the Event. A paused or completed (Succeeded or Failed) Loop is not
warned about its memory PVC at all: it will not read the PVC again (a paused
Loop not until it resumes), and warning forever would be a permanent 5-minute
alarm with nothing to fix. The operator records
the last Event's time on the Loop with the
`coxswain.io/memory-pvc-unbound-notified` annotation, so it can tell a fresh
degradation from one it has already announced.

### Memory PVC lifecycle and removal (I101)

There is one memory PVC per (namespace, repo URL), named
`memory-<first 12 hex of sha256(repo URL as written in spec.workspace.repo)>`.
It is created by the first Loop on that repo and **persists after every Loop on
the repo is deleted**: deleting a namespace's Loops does not clear the lessons.
A PVC lives only in its own namespace, so the same repo in another namespace gets
its own separate memory PVC.

To start a repo's memory from scratch:

```sh
kubectl cox memory export <repo-url> -o backup.tar.gz   # optional: keep a copy
kubectl cox memory delete <repo-url> -n <namespace>
```

`delete` refuses if any Loop on that repo in the namespace is still active
(not yet `Succeeded` or `Failed` — this includes `Paused` Loops and Loops
with no phase yet), or has reached `Succeeded` or `Failed:Stalled` but has
no `MemoryWriteDone` condition yet (its memory-writer Job starts after the
terminal phase and would lose its lessons). It names the blocking Loops.
Otherwise it deletes the PVC. A repo with no PVC prints a note.
The next Loop on the repo creates a fresh, empty PVC. (`kubectl delete pvc
memory-<hash>` also works, but has no Loop check.)

## Reading Loop status

The `kubectl cox describe` command is the primary way to read a Loop's
state:

```sh
kubectl cox describe <loop>
```

It shows:

- **Phase and the desired phase** (the phase is one of the `status.phase`
  enum values: `Pending`, `Planning`, `AwaitingApproval`, `Implementing`,
  `Verifying`, `Succeeded`, `Failed`, `Paused` — `CleaningUp` is declared in
  the enum but the operator never writes it; the desired phase is what the
  operator has asked the runner to be in, and it is half of the run key the
  sandbox attempt counters use, so a budget- or stall-paused Loop shows
  `Phase: Paused` with `DesiredPhase: Implementing`).
- **Iteration** (`status.iteration`: 0 for the first run, then 2, 3 … — 1 is
  skipped, the iterate advances to `max(iteration+1, 2)`).
- **Conditions** (every condition in `status.conditions` — each with its
  status, reason, and message). The set grows as gates are added, so the
  output is what the Loop actually carries, not a fixed list.
- **Budget** (`active:` — the wall-clock time counted toward
  `spec.budget.maxWallClock`, with the cap or `(no cap)`, then token usage,
  requests, unmetered requests, cost, the last successful usage read,
  consecutive read failures, and the cap that was hit, `exceeded:`).
- **Delivery** (the branch, PR number and URL the deliver Job recorded;
  omitted until there is one). A failed delivery is not shown here — it is the
  `Delivered` condition with reason `DeliveryFailed`.

The full audit trail (phase + verify-failure history) is:

```sh
kubectl cox history <loop>
```

The plan the agent wrote (`.coxswain/PLAN.md` in the workspace):

```sh
kubectl cox plan <loop>
```

### Who may write Loop status (P6c, D114)

Only the operator's own ServiceAccount may write `loops/status`: a
ValidatingAdmissionPolicy (`coxswain-deny-loop-status-writes`) denies every
other writer — including cluster admins. To approve or reject a plan, use
`cox approve` / `cox reject`; those commands write a one-shot annotation on
the Loop, and the operator consumes it and writes the status itself.

**Break-glass:** to hand-edit `loops/status`, delete the binding first,
then re-apply it afterwards:

```bash
kubectl delete validatingadmissionpolicybinding coxswain-deny-loop-status-writes
# … make the hand edit …
kubectl apply -f dist/install.yaml   # or a `helm upgrade` for a chart install
```

### What `Succeeded` means (P5f, I98)

`Succeeded` means the verify checks passed AND delivery has finished (a PR
was opened, delivery failed, or delivery is turned off — "terminal" delivery
in the conditions table below). With `spec.delivery.mode: PullRequest`, a Loop that passes verify is held at
`status.phase: Verifying` (condition `Verifying=True reason=Delivering`, the
sandbox Suspended) while the `Delivered` condition is in progress. When
`Delivered` becomes True (PR opened) or False (`DeliveryFailed`), both
`status.phase` and `status.desiredPhase` become `Succeeded`. A failed
delivery is therefore `Succeeded` + `Delivered=False` (retry with
`kubectl cox redeliver`). A script that waits for `phase=Succeeded` never
reads the Loop before its PR exists (or delivery has failed). With delivery
off, `Succeeded` is set as soon as verify passes. The memory-writer Job
starts only after the Loop is `Succeeded`, not during the hold.

Settling the hold does not delete the `Verifying` condition: it becomes
`Verifying=False` with the delivery outcome as its reason (`Delivered`, or
`DeliveryFailed`), so `kubectl cox describe` still shows that verify passed
and why the hold ended.

### Key conditions to watch

| Condition | What it means |
|-----------|---------------|
| `status.phase=Succeeded` (no separate `Succeeded` condition type) | The goal was met: the verify checks passed and delivery is terminal (or delivery is off). |
| `Verifying=True reason=Delivering` | Verify passed and delivery is on: the Loop is held at phase `Verifying` (sandbox Suspended) until the `Delivered` condition is terminal. |
| `Failed=True reason=Stalled` | The P2e stall gate fired: k consecutive identical verify failures, or k consecutive failures with no real diff (the same agent diff, D79(b)). The Stalled condition names which. |
| `Failed=True reason=BudgetExceeded` | A budget cap (tokens, wall-clock, cost) was hit. |
| `Paused=True reason=Stall` | The stall gate fired with `stallAction=Pause`; the Loop is held Paused until resumed. |
| `Paused=True reason=Budget` | A budget cap was hit with `onExceeded=Pause`; the Loop is held Paused until resumed. |
| `NetworkEnforced=False reason=CNIUnenforced` | The CNI does not enforce NetworkPolicy; the Loop is held Suspended (fail-closed). |
| `ModelAvailable=False reason=ModelUnavailable` | The model backend is unreachable or returned no response within the timeout. |
| `ModelConfigValid=False reason=NoModelEndpoint` | A runner Loop (the operator's `--runner-image`, with `spec.agent.image` empty or equal to it) has no `spec.agent.endpointSecretRef` and no `spec.agent.modelEndpoint`. The runner needs the operator-set model proxy URL, so the Loop is held with no sandbox (I212); recreate the Loop with the model fields set. |
| `SecretsUsable=False reason=NamespaceNotEnabled` | The namespace has no D102 `RoleBinding` for the `coxswain-secret-reader` ClusterRole; the Loop is held (no sandbox) until it is added (Secret opt-in, below). |
| `SecretsUsable=False reason=SecretNotOptedIn` | A named Secret is missing the `coxswain.io/usable: "true"` label; the message names the Secret and the field that references it. |
| `SecretsUsable=False reason=SecretNotFound` | A named Secret does not exist in the Loop's namespace. |
| `PodUnschedulable=True reason=Unschedulable` | A Loop pod (proxy or sandbox) is rejected by the scheduler (`PodScheduled=False` with reason `Unschedulable`). The message names the stuck pod and the scheduler's own message. The Loop is held while the stuck state lasts less than 10 minutes; past the bound, the Loop **fails** with `reason=PodUnschedulable` (phase `Failed`). While the Loop is Paused, the bound's clock is reset (a paused Loop does not fail on resume). |

### Secret opt-in (D76/D102)

A Loop can name Secrets in three places — `spec.workspace.gitCredentialSecret`,
`spec.agent.endpointSecretRef`, and the AgentPolicy tools' `credentialSecretRef`
(the tool proxy). The operator never gets a cluster-wide Secret read: its
ClusterRole has no `secrets` rule. A Secret is usable only when the NAMESPACE
is enabled (a `RoleBinding` from the shipped `coxswain-secret-reader`
ClusterRole — `get` on secrets only — to the operator's ServiceAccount) AND
the Secret carries the label `coxswain.io/usable: "true"`.

Enable a namespace and opt in its Secrets:

```bash
# Enable the namespace (defaults: the operator's namespace/serviceaccount).
# --clusterrole=coxswain-secret-reader for a Helm chart install; a
# `kubectl apply -f dist/install.yaml` install renders the ClusterRole as
# coxswain-coxswain-secret-reader (the kustomize namePrefix doubles the
# prefix) — use that name in --clusterrole= for such an install.
kubectl -n <namespace> create rolebinding coxswain-secret-reader \
  --clusterrole=coxswain-secret-reader \
  --serviceaccount=coxswain-system:coxswain-controller-manager

# Opt in the Secrets the Loops name.
kubectl -n <namespace> label secret <git-credential-secret> coxswain.io/usable=true
kubectl -n <namespace> label secret <model-endpoint-secret> coxswain.io/usable=true
kubectl -n <namespace> label secret <tool-credential-secret> coxswain.io/usable=true
```

The check is METADATA-only (the operator reads the Secret's labels, never its
data) and is re-run on every reconcile: a Loop in a disabled namespace, or
naming an unlabelled or missing Secret, is held with `SecretsUsable=False`
before the sandbox is created (no sandbox, no verify/deliver Job, no proxy
objects) until the label or the binding is added; labelling the Secret or
creating the RoleBinding releases the SAME Loop on its next reconcile (no
restart). A Loop that names no Secret is unaffected (`SecretsUsable=True
reason=NoSecrets`). Unlabelling a Secret, deleting it, or removing the
RoleBinding re-holds the Loop on its next reconcile (fail-closed). See
`docs/UPGRADE.md` (the D76/D102 section) and `docs/TROUBLESHOOTING.md`.

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
  --component proxy`).
- `Stalled` → the verify Job is producing identical failures. Read the
  stall history (`status.stallHistory`) and the verify Job log (`kubectl
  cox logs <loop> --component verify`). The check output is in the verify Job's
  termination message.
- `BudgetExceeded` → a budget cap was hit. Read `status.budget` (the token
  counts, wall-clock time, cost) and `spec.budget` (the caps).
- `CNIUnenforced` → the network gate is not satisfied. The CNI does not
  enforce NetworkPolicy. Either install an enforcing CNI (Calico) or use the
  `--allow-unenforced-network` escape hatch (dev only).
- `ProbeUnavailable` → the network probe ran but could not measure the fence
  because a row came out **UNRESOLVED** (a target could not be resolved, or it
  was refused while the no-policy control pod was BLOCKED or UNRESOLVED for it),
  or the DNS positive control was not REACHABLE in both passes, or no
  host-network target was measured BLOCKED, or a pass could not run (timed
  out, went not-Ready, or exited non-zero). A target refused by **both** the
  probe pod and the control pod combines to NOT_LISTENING — that is shown, not
  counted, and is not a `ProbeUnavailable` by itself. The condition's message
  names what was not measurable. Re-run the standalone check to see the
  per-target table: run `make verify-cni`.
- `PolicyEnforced=False` → the KubeArmor policy gate is not enforced. Check
  that KubeArmor is installed and the BPF-LSM hooks are active.
- `PolicyEnforced=False` reason `AuditOnly` → learn mode (P6b): every policy the Loop references has `spec.auditOnly: true`, so KubeArmor records the agent's activity but blocks nothing. Remove `auditOnly` (or reference an enforcing policy) to enforce.

### 2. Read the verify Job log

The verify Job runs the acceptance checks. Its log (and the check
container's termination message) is the evidence for why the checks
failed:

```sh
kubectl cox logs <loop> --component verify
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
kubectl cox logs <loop> --component agent
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

### 8. Retry a Loop that failed with SandboxFailed

A `Failed:SandboxFailed` Loop (e.g. a Secret deleted or changed after the Loop started; a missing one is held earlier with `SecretsUsable=False`) can be
retried once the cause is fixed, without deleting and re-applying the Loop
and losing its history:

```sh
kubectl cox retry <loop>
```

It sets the one-shot `coxswain.io/retry` annotation. The operator resumes the phase the Loop failed in, from its current checkpoint, resets the sandbox-attempt count, deletes the failed Sandbox, and emits a Normal `Retried` Event. Any other Failed reason is refused (the CLI names it; the operator ignores and removes an annotation it did not accept).

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
rule the CNI enforces is pod-wide, so the widening is real. The two in-code
comments contradict each other; the current enforcement is the pod-wide rule
(the init-container-only claim in the S3a comment is inaccurate). In practice:
while the agent runs, it can reach every pod in the repo's namespace on the
repo port. For
an **external** repo (the default in the shipped samples) there is **no** repo
rule at all — NetworkPolicy cannot name a hostname — and the first clone goes
through the egress proxy (`workspaceInitProxyCreated`; the F55 gate above
waits for that proxy to be Ready).

The tool proxies. The allowlist adds one egress peer per tool proxy, one per
tool in the effective policy (the `toolProxyNetpolPeer` rule in
`ensureNetworkPolicy`; 0006-A1 row 16).

Reference policies ship in `config/samples/policies/`: a minimal `default-deny`
and a `go-toolchain` network allow list. They are ordinary AgentPolicy objects
(inert until a Loop references one via `spec.policyRefs`) and are included in
`kubectl apply -k config/samples`.

### How the network probe measures enforcement (D103)

The operator's CNI probe (`internal/cni/pod_prober.go`, run by the Regate
source that sets the `CNIEnforced` / `CNIUnenforced` /
`ProbeUnavailable` result) measures the cluster's NetworkPolicy enforcement
by running **TWO pods** in the fixed `coxswain-cni-probe` namespace, both
within the probe timeout (`--cni-probe-timeout`, default 120s, `cmd/main.go`):

1. `coxswain-cni-probe-control` — **no NetworkPolicy applies** to it (its
   label is not selected by the probe netpol). It runs first and establishes
   the baseline: what each target looks like with no policy at all. Its node
   assignment is read back, and
2. `coxswain-cni-probe` — the same spec under the probe's allow-list
   NetworkPolicy (egress to DNS only), **pinned to the control pod's node**
   so both passes measure the same node's host-network targets.

Each pass probes a fixed set of targets and reports a per-target verdict. The
passes are combined per target (`combineVerdicts`) and the D103 rules apply:

| Policy pass | Control pass | Combined |
|---|---|---|
| REACHABLE | — | **REACHABLE** (the CNI let it through) |
| BLOCKED | — | **BLOCKED** (a real drop) |
| REFUSED | REACHABLE | **BLOCKED** (the policy made the CNI reject; the controller enforces) |
| REFUSED | REFUSED | **NOT_LISTENING** (nothing listens there at all) |
| otherwise (UNRESOLVED, or a REFUSED against a BLOCKED control) | | **UNRESOLVED** |

The verdict (`decide`):

- **`CNIEnforced`** needs the apiserver Service (ClusterIP :443) measured
  **BLOCKED** and at least one node port (node :6443 or :10250) measured
  **BLOCKED** — a service route AND a host-network route fenced. Rows that
  combined to **NOT_LISTENING** are shown in the detail but **not counted**:
  some policy controllers refuse a policy-denied connection instead of
  silently dropping it — the homelab k3s run of 2026-11-04 observed every
  target refused, including 1.1.1.1:443, which is consistent with such a
  controller, though the RST cause was not verified — and a no-policy baseline
  is what separates a policy-induced refusal from a port nothing listens on.
  That is exactly why the second, no-policy pod exists.
- **`CNIUnenforced`** if any expected-BLOCKED target combined REACHABLE.
- **`ProbeUnavailable`** if any row is UNRESOLVED (the detail names the
  target), the DNS positive control was not REACHABLE in both passes, or no
  host-network target was measurable at all.

The same two-pass shape is what `make verify-cni`
(`test/e2e/verify-cni.sh`, control pod `cni-probe-control`) and `make
d38-cni-e2e` check on demand.

### ClusterAgentPolicy (P6a)

A `ClusterAgentPolicy` is the cluster-scoped companion to `AgentPolicy`: the
same spec (`AgentPolicySpec` is reused), one object, reference-able by Loops in
**any** namespace — for an org-wide allow that would otherwise be copied into
every namespace. It is inert until a Loop references it, exactly like an
`AgentPolicy`.

```yaml
apiVersion: coxswain.wattu.com/v1alpha1
kind: ClusterAgentPolicy
metadata:
  name: org-go-toolchain
spec:
  network:
    - api.golang.org:443
```

**Resolution order.** A `spec.policyRefs` name is resolved in the Loop's
namespace first, then cluster-wide (`resolvePolicyRefs`):

1. a namespaced `AgentPolicy` of that name in the Loop's namespace, or
2. else a `ClusterAgentPolicy` of that name (the fallback), or
3. else `PolicyValid=False` with reason `PolicyNotFound` (fail closed).

The effective policy is the union of the allows across every referenced
policy, namespaced and cluster-scoped alike.

**Name collision (as implemented).** When a ref name matches **both** a
namespaced `AgentPolicy` (in the Loop's namespace) and a `ClusterAgentPolicy`,
the **namespaced one wins** (the author's explicit choice); the operator never
rejects the Loop for a collision. The operator fires ONE `PolicyNameCollision`
Warning Event per change of the collision set (not per reconcile pass) and
records the sorted set in the Loop's `coxswain.io/policy-collisions`
annotation (removed when the set becomes empty). See it with:

```sh
kubectl -n <ns> get events --field-selector reason=PolicyNameCollision
kubectl -n <ns> get loop <name> -o jsonpath='{.metadata.annotations.coxswain\.io/policy-collisions}'
```

Which scope wins on a collision is D98 (owner decision, 2026-11-04):
the namespaced policy wins.

**Operator RBAC.** The operator's ClusterRole has `get;list;watch` on
`clusteragentpolicies` (read-only — the operator never writes a policy), and
it **watches** the kind: an edit to a `ClusterAgentPolicy` re-reconciles every
Loop in every namespace that names it, including a narrowing edit (the
watch's map, `clusterAgentPolicyToLoopRequests`, lists all namespaces).

### Learn mode: auditOnly policies (P6b)

`spec.auditOnly: true` on an `AgentPolicy` or a `ClusterAgentPolicy` (the
same spec) is learn mode. The field is never defaulted: an unset or `false`
policy enforces, as before.

Learn mode applies only when **every** policy the Loop references is
`auditOnly`. Then the Loop's pod-level KubeArmorPolicy for the agent renders
with `spec.action: Audit` — KubeArmor records the agent's activity but never
blocks. The model and egress proxies' policies stay `Block`. The Loop shows
`PolicyEnforced=False` with reason `AuditOnly`. The D30 gate is unchanged: a
learn-mode Loop still needs a working engine (KubeArmor installed and the node
enforcing), otherwise the usual hold applies (e.g. `NodeNotEnforcing`).

**What learn mode records.** In learn mode the agent's KubeArmorPolicy carries
catch-all **Audit** rules for process and file — every directory, recursive —
so KubeArmor is asked to record every exec and file access. Without
them (the agent
policy is only a DNS/network fence, D46) Audit recorded nothing; a
kind-ref run before the catch-all rules landed got 0 records. The egress
proxy also runs in learn mode even when no policy has network allows: its
allowlist is then empty, so it blocks every host and records each connect, and
`cox policy suggest` turns those connects into network allows. Enforcing mode
is unchanged: no catch-all rules, and no egress proxy without network allows.

**Known gap (#407 kind-ref runs).** KubeArmor must run with
`logPath=stdout` (QUICKSTART Step 3) for the relay to see any alert, and
#408 makes the relay parse those lines. On kind-ref, KubeArmor reported no
events from the agent's sandbox pod in one run (cause under investigation),
so `cox policy suggest` may still come back empty.

**Mixed Loops (D108).** If an enforcing policy is also referenced, the Loop
enforces the union of every policy's allows (`spec.action: Block`); the
`auditOnly` flags have no effect, `status.policy.auditOnlyIgnored` lists the
ignored policies, and one `AuditOnlyIgnored` Warning Event fires each time
that list changes.

```yaml
apiVersion: coxswain.wattu.com/v1alpha1
kind: AgentPolicy
metadata:
  name: learn-go
  namespace: default
spec:
  auditOnly: true
  exec:
    - /usr/local/go/bin/go
```

**Getting a starting policy.** Run the Loop in learn mode (every policy it
references `auditOnly: true`) for a while, then build a candidate from what
the agent actually used:

```bash
kubectl cox policy suggest <loop> -n <ns>
```

By default the command reads the Loop's **learn summary**: the operator keeps
one per learn-mode Loop in the ConfigMap `<loop>-learn-summary`, in the Loop's
namespace and owned by the Loop (data `records.jsonl`, one record per distinct
allow, plus a `dropped` count). It survives operator restarts and log
rotation — every distinct allow recorded during the run is there, not only
what happens to be in the current log. When the summary is at its size bound
and dropped records (`dropped > 0`), the command warns on stderr. `--name`
defaults to `<loop>-suggested`; the namespace is the CLI's `--namespace` (the
`-n` flag). It prints a candidate `AgentPolicy`. Review it, apply it as a new
policy with `auditOnly: true` added (the candidate is written without it) to
confirm it covers what the agent needs, then drop `auditOnly` to enforce.

**Trust:** the summary is accepted only when it carries a controller
ownerReference to this Loop (kind, name, and uid); that stops stray or
foreign ConfigMaps. It does not stop a deliberate namespace writer, because a
Loop's uid is readable by anyone who can get Loops — anyone who can create
ConfigMaps in the namespace can forge that ownerReference. The summary is
therefore only as trustworthy as the namespace's ConfigMap writers; the
candidate policy is always reviewed by a human before it is applied.

Alternative: read the operator's Q4 audit lines directly (`-f <file>`, or
`-f -` to pipe them on stdin):

```bash
kubectl logs -n coxswain-system deploy/coxswain-controller-manager \
  | kubectl cox policy suggest <loop> -f -
```

Lines that are not audit records, and other Loops' records, are skipped.
Warning: `kubectl logs` holds only the lines since the operator's last log
rotation (a learn run rotates it within minutes — kind-ref run 12), so a log
capture may miss most of the run's activity; prefer the summary.

What the candidate holds is only the allows the agent **used** —
deduplicated and sorted — and it never contains a deny:

- only the agent sandbox pod's KubeArmor records count (`pod=<loop>-sandbox`
  in `detail`); init containers' records in that pod still count, while the
  verify pod and the proxies do not (D112);
- KubeArmor process records become `exec`, keeping only the executable's
  absolute path (the command's arguments are dropped); a path under a
  writable mount (/workspace, /scratch, /tmp) or not canonical is listed as
  a comment instead, because the CRD rejects it (R15);
- KubeArmor file records become `files`: a directory with more than 5
  distinct paths becomes one `dir/` entry (recursive, D113); otherwise
  the exact paths are kept;
- egress-proxy connects (`host:port`) become `network`;
- KubeArmor network records are skipped, because the egress proxy's record
  carries the `host:port` an `AgentPolicy` needs;
- each list holds at most 64 entries (the AgentPolicy limit), the most-used
  first; the rest are printed as a comment under the list.

An AgentPolicy `files` entry ending in "/" grants that directory recursively
(KubeArmor matchDirectories); any other entry is an exact path (D113).

The candidate has no `auditOnly`: it is meant to enforce once reviewed. A
header comment on the candidate lists the platform minimum the operator adds
itself (the model-proxy and egress-proxy Service FQDNs, DNS, the agent
entrypoint), which is **not** written as allows (D109). Audit records carry
KubeArmor's operation first in `detail` (`operation=Process|File|Network`,
then `pod=<pod>`).

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
init container itself while it runs. The agent container also carries no
service-account token: the pod sets `automountServiceAccountToken: false`
explicitly (asserted in `loop_c1_hardening_test.go`), so the agent has no API-server
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

## Reading the egress proxy log

Not every Loop has an egress proxy: the operator creates it only when the
effective policy has network allows, or when the workspace-init clone of an
external repo must traverse it (and for the verify/deliver Jobs' external-repo
traffic). A Loop with an in-cluster repo and no network allows has no egress
proxy pod and therefore no log.

Each Loop with an egress proxy gets one long-lived proxy pod. In the common
case it is named `<loop>-egress-proxy`, but a long Loop name is truncated and
hashed to fit the pod-name budget, so the literal name can fail with
NotFound. The robust form is the pod's own label (it deliberately does not
carry the Loop's usual `coxswain.io/loop` label):

```
kubectl logs -n <ns> -l coxswain.io/egress-proxy-for=<loop> --tail=20
```

`cox logs` has no egress component: its `proxy` component is the **model**
proxy pod, not this one. Use `kubectl logs` for the egress proxy.

Every connection attempt — allowed **and** blocked — emits one Q4 audit
record as a single JSON line on the pod's stdout; the workspace-init clone's
first-fetch attempts appear in the same log, since the repo host is appended
to the proxy's allowlist for it. A 403 the agent sees from the proxy has a
matching `blocked` record here. Records are written to the container's stdout
only — there is no persistent audit store. The pod is deleted and recreated
when the proxy's spec drifts, and the spec includes the effective policy's
network allows, so **changing an AgentPolicy's network allows deletes the
pod and its audit history with it**. Save the log before changing a policy:

```
kubectl logs -n <ns> -l coxswain.io/egress-proxy-for=<loop> > egress-audit.log
```

A recreated pod starts a fresh log.

### Record fields

Each line is a JSON object with these keys (see
`internal/egress/audit.go`, `AuditRecord`):

- `time` — the attempt time, UTC, RFC 3339.
- `loop`, `namespace` — the Loop and its namespace.
- `source` — always `egress-proxy`.
- `action` — `connect` for the HTTPS path, `http` for plain HTTP.
- `target` — the host (and port) the client asked to reach.
- `verdict` — `allowed` or `blocked`.
- `detail` — free-form key=value pairs, listed below.

The record deliberately carries **no** `iteration` field: the proxy pod is
long-lived with an immutable env, so it cannot read `status.iteration`.
The relay that joins records into the Q4 stream fills the iteration by
time window; the policy hash in `detail` disambiguates a mid-iteration
policy change.

### `allowed` vs `blocked`

- For `connect` (HTTPS), `allowed` is emitted once the allow check, the
  resolved-IP check, and the SNI check all pass. A dial failure on the
  CONNECT tunnel is `blocked` with `reason=dial-failed, ip=<ip>`.
- For `http` (plain HTTP), `allowed` is emitted only after the upstream
  responds. A dial/upstream failure is `blocked` with
  `reason=upstream-error, policy=<hash>` — that value appears only on the
  plain-HTTP path; on `connect` the equivalent failure is
  `reason=dial-failed`.
- `blocked` is every other outcome. The `detail` values are:
  `reason=bad-host` / `reason=bad-port` (unparseable CONNECT target);
  `reason=not-allowed, policy=<hash>` (host:port not in the effective
  network allows); `dns=nxdomain` or `reason=resolved-ip-rejected, ip=<ip>`
  (the SSRF backstop: the resolved IP does not exist or lands in a
  carved-out range); `reason=no-hijack` / `reason=hijack-failed` /
  `reason=read-deadline-failed` / `reason=dial-failed, ip=<ip>` (proxy-
  internal failures); `sni=absent` / `sni=absent-in-hello` / `sni=<value>`
  (the SNI check on CONNECT); `reason=upstream-error` (plain HTTP).
- `detail` also carries `ip=<resolved IP>` — the exact address the proxy
  dialed, so a DNS rebind is visible after the fact — and `policy=<hash>`,
  correlating with `status.policy.effectiveHash`.

### Example lines

An allowed HTTPS connection. The record shape (the keys, `verdict`, and
`ip=` in `detail`) is asserted against a live proxy pod by
`test/e2e/i42-e2e.sh`, which fetches the log by pod name
(`<loop>-egress-proxy`); no test fetches it by label. The values shown here
are illustrative, taken from the record-shape unit test:

```
{"time":"2026-09-28T12:00:00Z","loop":"loop-x","namespace":"ns-x",
 "source":"egress-proxy","action":"connect",
 "target":"proxy.golang.org:443","verdict":"allowed",
 "detail":"sni=proxy.golang.org, proto=https, ip=151.101.0.223, policy=abc123"}
```

A blocked attempt to a host not in the network allows:

```
{"time":"...","loop":"loop-x","namespace":"ns-x",
 "source":"egress-proxy","action":"connect",
 "target":"evil.example.net:443","verdict":"blocked",
 "detail":"reason=not-allowed, policy=abc123"}
```

Note: non-JSON lines in the same stream (the startup
`egress-proxy: listening on :3128, ...` line and audit-write error log
lines) are operator logs, not audit records — filter for JSON lines when
grepping.

## Pinning Loops to a node pool (C2)

Two operator flags pin every Loop's operator-owned workloads to a dedicated
node pool. Empty (the default) means no placement — today's behaviour. A
malformed token is a STARTUP error that names the bad token (the operator
refuses to start with a silently different placement than configured).

- `--loop-node-selector` — comma-separated `key=value` node labels, e.g.
  `--loop-node-selector coxswain.io/pool=loops`.
- `--loop-tolerations` — comma-separated
  `key:op:effect[:value[:seconds]]` (the op field is ALWAYS field 2, so a
  token is never ambiguous). `op` is `Equal` or `Exists` (NotEqual is not a
  Kubernetes toleration operator and is rejected); effect is `NoSchedule`,
  `PreferNoSchedule` or `NoExecute`. `Equal` requires a value; `Exists` must
  not have one — a seconds-only Exists token keeps the value slot empty:
  `key:Exists:NoExecute::30`.

### Worked example: a dedicated loop pool

```sh
# 1. Mark the node(s).
kubectl label node <n> coxswain.io/pool=loops

# 2. Taint the node(s) so nothing else lands there.
kubectl taint node <n> coxswain.io/pool=loops:NoSchedule

# 3. Run the operator with the matching flags.
--loop-node-selector=coxswain.io/pool=loops \
--loop-tolerations=coxswain.io/pool:Equal:NoSchedule:loops
```

**What is pinned (D101):** the sandbox pod, the model proxy pod, the egress
and tool proxy pods, and the verify, deliver and memory-writer Jobs.

**What happens when the flags change:** the sandbox template is updated in
place (and cleared when the flags are emptied); the proxy pods are recreated
(their spec hash covers placement); existing Jobs keep their old placement,
and new Jobs use the new flags.

**A note on default tolerations:** Kubernetes automatically adds the
not-ready/unreachable default tolerations (DefaultTolerationSeconds) to every
pod, so `kubectl get pod -o yaml` shows them alongside the ones from the
flag.

**A note on a pool with no matching node:** with the flags set but no node
matching them, the model proxy and egress proxy pods sit Pending with
FailedScheduling events; the Loop holds at `Planning` with
`ModelProxyNotReady=True`; no sandbox pod is created (the template update
suspends the Sandbox, and with no Ready proxy pod it is not restarted), so
`SandboxNotStarting` never appears; and the Loop fails after 10 minutes
with `reason=PodUnschedulable` (the F93a bound — docs/OPERATOR.md,
"PodUnschedulable=True" in Key conditions, and docs/TROUBLESHOOTING.md).
Check `kubectl get pod -o wide` and the pods' FailedScheduling events. Changing the flags also recreates the proxy
pods of every Loop that has one — terminal Loops included (their spec hash
covers placement), while existing Jobs keep their old placement.

## k3s / flannel / SMB profile (C3)

This section documents the k3s + flannel + SMB (smb.csi.k8s.io) profile for
running coxswain on a homelab-class k3s cluster. It is based on a real k3s
v1.36.2 cluster (17 nodes, 3 arm64 + 14 amd64) run on 2026-11-03. The
homelab plan (`papawattu/homelab` PR #149) tracks the cluster side; this
documents what coxswain needs and what was actually observed.

**Status: the SMB memory class is proven (H3 evidence below). The network
gate works via the k3s embedded network-policy controller (H4 evidence
below). Checkpoints are not supported on this cluster today (no
VolumeSnapshot CRDs installed — see "k3s deltas" below). agent-sandbox is
not installed yet (H1, tracked in the homelab plan).**

### Network gate (D38) on flannel

**The CNI is flannel; it does NOT enforce NetworkPolicy on its own.**
The D38 network gate requires an enforcing CNI to police pod → host-network
egress. On a flannel-only setup, flannel's VXLAN provides no enforcement.

**k3s embeds a separate network-policy controller.** k3s ships an embedded
network-policy controller (not a separate pod — it runs inside the k3s
server process). It is **on by default**; the only way to turn it off is
the `--disable-network-policy` flag on the k3s server (or
`disable-network-policy: true` in `/etc/rancher/k3s/config.yaml`). There
is no `--network-policy` flag and no `network-policy: true` config key.
When enabled, it enforces `networking.k8s.io/v1` NetworkPolicy objects
independently of the CNI — flannel + k3s embedded network-policy
controller is a working enforcing setup.

**The homelab k3s has this on by default (verified by the H4 probe below).**
Nothing in config.yaml, the unit, or the cmdline disables it, so nothing
turns it off — the absence is the confirmation. The empirical proof is the
BLOCKED/REACHABLE probe result.

**H4 evidence — Ingress pod-to-pod (2026-11-03, pi6, namespace c3-h4):**

```
Setup: target Deployment (httpd:2.4) pinned to pi6, Service target.c3-h4:80.
Probe pod (python:3.13-alpine) pinned to pi6, TCP connect to
target.c3-h4.svc.cluster.local:80.
deny-all NetworkPolicy: podSelector app=target, policyTypes [Ingress],
ingress: []

=== BLOCKED (deny-all active, pod probe4) ===
TARGET_SVC BLOCKED ([Errno 111] Connection refused)
probe4 Completed on pi6

=== REACHABLE (no netpol, positive control, pod probe3) ===
TARGET_SVC REACHABLE
probe3 Completed on pi6

Cluster NetworkPolicies at time of probe: 32
```

**Interpretation:** the same probe pod, same service, only the NetworkPolicy
differing → the embedded controller enforces the policy. The probe got
`Connection refused`.

**What this means for coxswain's D38 gate:** the operator's CNI probe
(`internal/cni/pod_prober.go`) runs its own probe pods ("How the network
probe measures enforcement (D103)" above) and checks whether the CNI
enforces NetworkPolicy. H4 above proves the controller enforces Ingress
pod-to-pod; the D38 probe tests Egress to host-network destinations
(apiserver ClusterIP :443, node :6443, node :10250).

**2026-11-04 result (pre-D103 probe):** `make verify-cni` on this cluster
gave all 37 probe targets `UNRESOLVED(refused)` — every target, including
the external 1.1.1.1:443, was connection-refused rather than dropped. The
probe had no no-policy baseline, so a refusal could not be told apart from
a policy rejection: the run was **UNPROVEN**, not a pass or a failure. That
is exactly what D103's two-pass probe fixes: the no-policy control pod
(`coxswain-cni-probe-control`) is what lets a REJECT-style policy controller
be measured (a target that refuses under policy but the control pod reaches
combines to BLOCKED, i.e. enforcement).

**2026-11-04 result (D103 probe, owner-approved re-run, merged main
`a6f469b`):** the re-run of `make verify-cni` on this cluster with the D103
two-pass probe exited 1, i.e. **UNPROVEN overall** — but because of ONE
node, not the probe. Of the 37 targets: **22 BLOCKED** (the apiserver
Service, the 16 kubelet node rows other than pi12, the 3 control-plane
apiserver node rows, the cross-namespace pod, and the external target —
each refused under the policy and reached by the no-policy control pod),
**13 NOT_LISTENING** (the workers' apiserver node rows; shown, not counted),
and **2 UNRESOLVED**: pi12 (`192.168.1.52`, cordoned), where the no-policy
control pod got `BLOCKED(OSError)` and so there is no baseline. The
REJECT-style controller is thus measured as enforcing on every reachable
node; `verify-cni` checks every node, so that one unreachable node keeps the
cluster-wide verdict UNPROVEN. Evidence: the uncommitted evidence file
`.samples/verify-cni-20261104/homelab-k3s-d103.txt`. Run `make verify-cni
K8S_CONTEXT=<ctx>` before a coxswain run and expect `NetworkEnforced=True`;
if it reports `CNIUnenforced`, the operator holds Loops Suspended.

**Check the k3s embedded controller is on:**

The most reliable check is the empirical one: apply a deny-all
NetworkPolicy to a test pod and verify that connections to that pod are
blocked (the H4 probe above). For a config check: confirm
`disable-network-policy` is **absent** in config.yaml, the k3s unit, and
the k3s cmdline — its absence everywhere is what keeps the controller on:

```sh
# On the k3s control-plane node(s):
# Check config.yaml (may not be present on all k3s installs):
grep disable-network-policy /etc/rancher/k3s/config.yaml 2>/dev/null || echo "not in config.yaml (expected — controller stays on)"
sudo systemctl cat k3s 2>/dev/null | grep disable-network-policy || echo "not in the k3s unit (expected — controller stays on)"
sudo -n cat /proc/$(pgrep -x k3s | head -1)/cmdline 2>/dev/null | tr '\0' '\n' | grep disable-network-policy || echo "not in cmdline (expected — controller stays on)"
```

If the probe reports `CNIUnenforced`, the embedded controller was
disabled. Turn it back on (requires k3s restart on each node): remove
`disable-network-policy` from `/etc/rancher/k3s/config.yaml` or the k3s
unit, then

```sh
sudo systemctl restart k3s
```

### SMB memory StorageClass (P5a, `--memory-storage-class`)

**The `smb` StorageClass (smb.csi.k8s.io) is proven as a RWX memory class.**
The homelab cluster ships an in-cluster samba share exposed via the
`csi-driver-smb` (smb.csi.k8s.io) CSI driver.

**H3 evidence (2026-11-03, namespace c3-h3):**

```
PVC: c3-h3-mem, 1Gi, RWX, storageClassName=smb
  → Bound (immediate) on creation.

Writer pod on pi6 (busybox:1.36, uid 1000):
  echo "c3-h3-write-<epoch>" > /data/mem.txt
  → /data/mem.txt written, uid 1000, mode 0777

Reader pod on pi2 (busybox:1.36, readOnly, different node):
  cat /data/mem.txt
  → "c3-h3-write-<epoch>" read successfully on pi2

File content identical on both nodes; PVC deleted and namespace removed
after the test.
```

**Posture: this is a shared SMB share with `noperm` + `0777` mount
options.** Every pod that can mount the PVC can write to it. The memory
PVC is per (namespace, git URL); the memory-writer Job is the only intended
writer. Do not use this class where tenants are untrusted. For a
hardened setup, use a CephFS or NFS class.

**SMB semantics note:** with `noperm` + `0777`, file ownership and
permissions are not enforced by the SMB server — the CIFS mount options
override them. The homelab plan's H0 decision recommends `smb` as the
first-choice memory class (try it first, move to CephFS only if it fails).
H3 passed, so `smb` is the recommendation for the homelab coxswain run.

**Run the operator with the SMB class:**

```sh
# install.yaml path (render with MEMORY_STORAGE_CLASS):
make build-installer IMG=<registry>/coxswain-controller:<tag> \
  MEMORY_STORAGE_CLASS=smb
kubectl apply -f dist/install.yaml

# OR: patch the manager Deployment args after install:
kubectl -n coxswain-system patch deploy coxswain-controller-manager \
  --type=json \
  -p '[{"op":"add","path":"/spec/template/spec/containers/0/args/-","value":"--memory-storage-class=smb"}]'
```

### k3s deltas vs. kind (what to check before a coxswain run)

| Item | Status on the homelab k3s (2026-11-03) | Action |
|---|---|---|
| agent-sandbox controller (v1.0.4) | **Not installed** | H1: `kubectl apply -f https://github.com/kubernetes-sigs/agent-sandbox/releases/download/v1.0.4/sandbox.yaml` |
| KubeArmor BPF-LSM (policy gate, ADR-0007) | **Not installed; BPF-LSM requires `lsm=…,bpf` on kernel cmdline** | H5: enable BPF-LSM on pool nodes only, then install KubeArmor v1.7.5 with a nodeSelector to the pool |
| VolumeSnapshot CRDs + snapshot-controller + VolumeSnapshotClass (checkpoints, fork, rollback) | **Not installed** (no snapshot CRDs found; ceph-csi's csi-snapshotter sidecar is present but there is no controller or class) | H1: install external-snapshotter CRDs + controller + a VolumeSnapshotClass for `rbd.csi.ceph.com` (the default StorageClass). Until this lands, `spec.checkpoints: Required` Loops will fail at start with `Failed:CheckpointUnavailable`; the default (best-effort) path sets `CheckpointsAvailable=False` and carries on |
| flannel + k3s embedded network-policy controller (D38 gate) | **On by default** (H4 evidence above; 32 NetworkPolicies in use) | Confirm `disable-network-policy` is absent from the k3s config / unit / cmdline; the 2026-11-04 D103 two-pass re-run of `make verify-cni` gave 22 BLOCKED, 13 NOT_LISTENING, 2 UNRESOLVED (pi12, `192.168.1.52`, cordoned) — overall UNPROVEN only because of that one node; the REJECT-style controller measured as enforcing on every reachable node (`.samples/verify-cni-20261104/homelab-k3s-d103.txt`, uncommitted) — run `make verify-cni K8S_CONTEXT=<ctx>` before a coxswain run |
| Multi-arch images (C1) | **Not yet** — release images are amd64-only | C1 (`.github/workflows/release.yml`) is an owner push. Until it lands, arm64 nodes cannot pull the release images. The homelab plan (H2) recommends tainting pi13 `kubernetes.io/arch=arm64:NoSchedule` until C1 merges |
| `--loop-node-selector` / `--loop-tolerations` (C2) | **Merged** (main) | Use the C2 flags to pin Loops to a dedicated amd64 pool (the H2 pool) |
| NetworkPolicy count | 32 existing NetworkPolicies | The D38 probe applies its own netpol in `coxswain-cni-probe`; the existing 32 do not interfere |

### What the homelab H slices must verify before a coxswain run

The homelab plan (`papawattu/homelab` PR #149) tracks these; this doc
records what was verified from the coxswain side on 2026-11-03:

- **H3 (SMB memory):** ✅ Verified (evidence above). The `smb` StorageClass
  works as a RWX class for the memory PVC on this cluster.
- **H4 (flannel network gate):** ⚠️ measured enforcing on every reachable
  node; pi12 unreachable from pods (cause not investigated). The k3s
  embedded network-policy controller enforces Ingress pod-to-pod NetworkPolicy
  (H4 evidence above). The D103 two-pass re-run of `make verify-cni` on
  2026-11-04 gave 22 BLOCKED, 13 NOT_LISTENING, 2 UNRESOLVED (pi12,
  `192.168.1.52`, cordoned — the no-policy control pod got `BLOCKED(OSError)`
  there, so no baseline): overall UNPROVEN, only because of that one node
  (`.samples/verify-cni-20261104/homelab-k3s-d103.txt`, uncommitted).
  Run `make verify-cni K8S_CONTEXT=<ctx>` before a coxswain run.
- **H1 (agent-sandbox + VolumeSnapshot):** ❌ Not yet. The agent-sandbox
  controller is not installed; the VolumeSnapshot CRDs and
  VolumeSnapshotClass are not installed. Checkpoints and fork/rollback are
  unavailable until H1 lands. The memory-writer Job (P5b) runs regardless
  (it does not depend on snapshots), but `spec.checkpoints: Required` Loops
  will fail at start.
- **H5 (KubeArmor BPF-LSM):** ❌ Not yet. The policy gate (ADR-0007 A1)
  cannot be enforced without KubeArmor + BPF-LSM. The D57 decision says
  the alpha ships KubeArmor-only — a cluster without BPF-LSM is not
  running coxswain as designed.

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
