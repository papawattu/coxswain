# QUICKSTART

Get a Loop running end-to-end on a cluster: install agent-sandbox and
KubeArmor, install the operator from a release's `install.yaml`, create the
GitHub token Secret, run a first Loop, and read the result with `kubectl cox`.

The alpha ships `dist/install.yaml` (D54: ghcr.io + install.yaml; a Helm chart
is Phase 7 work). A `v*` release tag publishes the public images and the
`install.yaml` that points at them.

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

### KubeArmor (BPF-LSM for the DNS fence)

The operator enforces egress with KubeArmor `KubeArmorPolicy` objects. The
tool proxy's `matchDNSQueries` fence (the inner egress fence that restricts
the proxy's DNS to the upstream host + platform DNS) needs **BPF-LSM active**
(the `SOCKET_SENDMSG` hook). On a cluster without BPF-LSM the fence is **not
enforced** and the only thing keeping the tool proxy on the configured upstream
is the proxy's own code (it dials the fixed upstream host only) — an accepted
limitation (D50, R23 I64; see `docs/adr/0008-tool-proxies.md`). For a fully
fenced run, use a node/kernel with BPF-LSM enabled (the dev cluster installs
KubeArmor v1.7.5 via the pinned `karmor` CLI; `make kind-up` does this for
kind).

### A Kubernetes cluster

A reachable cluster (kubectl in `~/.kube/config` pointing at it). For local
dev the Makefile creates one: `make kind-up` (kind 1.34, agent-sandbox,
KubeArmor).

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
kubectl cox pause <loop>        # set spec.suspend=true
kubectl cox resume <loop>       # clear spec.suspend (set coxswain.io/resume for a Stall/Budget pause)
kubectl cox redeliver <loop>    # re-trigger delivery for a failed Loop (I52 annotation)
```

`describe` is the one to run first after a Loop finishes: it shows the
terminal phase (Succeeded / the budget or stall condition), the iteration
count, and the delivery outcome (the PR the deliver Job opened, or the
delivery failure reason).

## 6. Model timeout and tuning

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
