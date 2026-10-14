# Operator runbook

This document is the day-to-day reference for running the coxswain operator:
installing it, reading its metrics, reading its logs, and debugging a stuck
Loop. For the one-time setup (agent-sandbox, KubeArmor, the network gate),
see `docs/QUICKSTART.md`.

## Contents

- [Install](#install)
- [Metrics](#metrics)
- [Logs](#logs)
- [Reading Loop status](#reading-loop-status)
- [Debugging a stuck Loop](#debugging-a-stuck-loop)
- [Uninstall](#uninstall)
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
make kind-load IMG=ghcr.io/papawattu/coxswain-controller:dev
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
metrics port (default `:8443` with HTTPS; `:8080` with
`--metrics-secure=false`). The metrics Service is
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

### The B5a metrics

| Metric | Type | Labels | Meaning |
|--------|------|--------|---------|
| `coxswain_loop_iterations_total` | Counter | `phase` | Total Loop phase transitions (iteration boundary crossings), by destination phase. Bumped once per NEW-phase transition (a no-op re-set to the same phase does not bump). |
| `coxswain_loop_stalls_total` | Counter | — | Total times the P2e stall gate fired (a new verify Job reached k consecutive identical check outputs). |
| `coxswain_loop_tokens_total` | Counter | `kind` (prompt\|completion) | Total model tokens consumed, as folded into `status.budget` from the proxy reading, by kind. Bumped by the delta on each same-bootID, no-drop reading. |
| `coxswain_loops_active` | Gauge | `phase` | Loops currently in a non-terminal phase, by phase. Set to 1 for each active Loop; reset to 0 when the Loop reaches a terminal phase. |

**Alerting examples** (PromQL):

```promql
# Loops that have stalled more than once in the last hour.
sum(increase(coxswain_loop_stalls_total[1h])) by (namespace) > 1

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
```

**Dashboard query examples:**

```promql
# Iterations by phase (cumulative, since process start).
sum(coxswain_loop_iterations_total) by (phase)

# Total tokens consumed (prompt + completion, cumulative).
sum(coxswain_loop_tokens_total)

# Active Loops by phase.
sum(coxswain_loops_active) by (phase)
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
(default `info`). To get debug logs for a specific Loop:

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

### Key conditions to watch

| Condition | What it means |
|-----------|---------------|
| `Succeeded=True` | The Loop's goal was met; the verify checks all passed. |
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
- The Loop is in a namespace the operator watches (the operator watches
  all namespaces by default; check `--namespace` if it is set).
- The Loop's finalizer is present (`status.finalizers` includes
  `coxswain.wattu.com/finalizer`).

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

```sh
# Remove the operator (the CRDs, RBAC, the manager Deployment).
kubectl delete -f install.yaml

# Remove any running Loops (this deletes the sandbox pods, the verify Jobs,
# the deliver Jobs, and the workspace PVCs).
kubectl get loops -A
kubectl delete loops --all-namespaces

# Remove the CRDs (if you want a clean cluster).
kubectl delete crd loops.coxswain.wattu.com agentpolicies.coxswain.wattu.com
```

For a Helm release:

```sh
make helm-uninstall   # helm uninstall coxswain -n coxswain-system
```

---

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
