# Troubleshooting

Symptoms alpha users have hit, with the documented cause and the documented
fix. Every entry cites the review doc, PR, or evidence file it comes from.
Install steps are in `docs/QUICKSTART.md`; one-off upgrade issues are in
`docs/UPGRADE.md`.

## `ModelAvailable=False` / `ModelUnavailable` on the Loop

**What you see:** `kubectl cox describe` shows the `ModelAvailable`
condition `False` with reason `ModelUnavailable` ("model backend
unavailable: …"), and the Loop is making little or no progress.

**Cause:** the runner's model call timed out and its bounded retries were
exhausted. The runner uses a 120-second per-request timeout with 3 retries
and exponential backoff (2s, 4s, 8s) — `defaultModelTimeout` /
`defaultModelRetries` / `defaultModelRetryBackoff` in `runner/runner.go`
(I89, merged as e5af830 / PR #135). A non-timeout error (HTTP 500,
connection refused) is **not** retried: the server responded, so the error
is definitive. When all attempts fail, the runner surfaces `ModelUnavailable`
in its claim and exits blocked; the operator maps the claim to the
`ModelAvailable=False` condition (reason `ModelUnavailable`) so the state is
visible — `internal/controller/loop_s4_phase.go:620-633`
(`applyModelAvailableCondition`, I89, merged as 06a0157 / PR #136). The
condition is diagnostic only; it does not gate the phase machine.

**Worst case:** about 8 minutes before the condition appears — 4×120s of
timeouts plus 14s of backoff (QUICKSTART §6, written with I89). Pre-I89 the
same hang meant a single 10-minute timeout plus kubelet back-off restarts
with no named reason (the finding I89 in `docs/REVIEW-PHASE1-R26.md:138` —
"in #124, the vLLM backends went silent for about 24 minutes. Each runner
call hung for its full 10-minute timeout before the runner exited").

**Tuning:** if you see `ModelUnavailable` on a healthy backend, the model is
slow, not dead. A legitimate response on a local 27B-class model under load
can approach 120s (PR #124 observed a 60s single-call tail on the healthy
endpoint). Raise the runner's `ModelTimeout` (a `runConfig.ModelTimeout` in
`runner/runner.go:108`, defaulting to `defaultModelTimeout` when unset;
QUICKSTART §6 covers the tuning) so a slow-but-healthy backend stops being
classified as unavailable. The retry count and backoff base are constants
(`defaultModelRetries = 3`, `defaultModelRetryBackoff = 2s` in
`runner/runner.go`) and not user-tunable. A success claim resets the
condition to `True` (reason `Recovered`) — so the condition returning to
`True` means the backend recovered, not that it was fixed by tuning.

**Note for the original hang:** the I89 finding's root cause (the vLLM
backends behind the nginx LB stopped serving model calls for about 24
minutes) was an endpoint-side fault, not a coxswain defect — the operator
digest was byte-identical to the release and the identical e2e path
completed in ~4 min on healthy backends (PR #124, merged 4653e9a; evidence
`.samples/implementing-stall-*` and the PR's evidence tables). I89 changed
the *symptom*, not that cause: the hang now surfaces as a named, bounded
`ModelUnavailable` instead of a 10-minute silent timeout.

## Service DNS NXDOMAIN from a fresh kind cluster

**What you see:** on a freshly created kind cluster, lookups of
`<service>.<ns>.svc.cluster.local` fail (NXDOMAIN) from inside the cluster,
and `nslookup` reports the node IP (`172.21.0.1`) as the DNS server instead
of CoreDNS.

**What the alpha runs showed (observation; the cause was not established):**
- **The alpha.1 A6 run** (`.samples/alpha-e2e-1791784606/summary.txt`,
  "BLOCKERS FOUND ON THE FRESH CLUSTER" item 2): on the fresh `coxswain-alpha`
  cluster, "Service DNS broken on the fresh kind node: kube-dns.<ns>.svc
  does not resolve (nslookup server = node IP 172.21.0.1, not CoreDNS
  10.96.0.10; NXDOMAIN for all svc FQDNs). CoreDNS pods are 2/2 Running." The
  deliver Job's `clone-base` step uses
  `HTTPS_PROXY=http://<loop>-egress-proxy.<ns>.svc…`, which could not be
  resolved, so the deliver Job failed (`DeliveryFailed`). The Loop agent
  cycle still reached Succeeded only because the model endpoint is an IP.
- **The alpha.3 A6 run** (`.samples/alpha-e2e-1791788025/dns-eth0.log`, the
  a6 preflight probe `dns-eth0` run from a pod via
  `test/e2e/alpha-e2e.sh:358-383`): `nslookup
  kubernetes.default.svc.cluster.local` returned `Address: 10.96.0.1` via
  `Server: 10.96.0.10` — cluster DNS resolved from a pod on that run.

**Cause:** not established. The two runs differ (one NXDOMAIN, one resolved)
and the alpha.1 evidence does not isolate which component is at fault, so
no cause is asserted here.

**What to check when you hit it:**
1. The failing pod's `/etc/resolv.conf`:
   `kubectl -n <ns> exec <pod> -- cat /etc/resolv.conf` — is the nameserver
   the node IP or CoreDNS, and is the `cluster.local` search path present?
2. The kubelet `clusterDNS` setting (on the node, `kubelet --cluster-dns`
   or the `--cluster-dns` flag in the kubelet args) — it must point at the
   in-cluster DNS service IP (`10.96.0.10` by default).
3. The `kube-dns` Service and its endpoints in `kube-system`:
   `kubectl -n kube-system get svc,endpoints kube-dns` — the Service must
   have the CoreDNS pod IPs as endpoints.
4. CoreDNS logs: `kubectl -n kube-system logs -l k8s-app=kube-dns` — look
   for resolution errors during the failing window.

## `cannot list resource configmaps at the cluster scope` on a pre-alpha.2 install

**What you see:** the operator wedges after a fresh install of
`v0.1.0-alpha.1`; the controller log repeats `cannot list resource
configmaps at the cluster scope` and Loops do not start.

**Cause (I86, `docs/REVIEW-PHASE1-R26.md:83`):** A4 added a ConfigMap read
through the manager cache, but the RBAC marker granted
`get;create;update;delete` without `list;watch` — and the
controller-runtime cache needs `list;watch` on every type it caches.

**Fix:** upgrade to `v0.1.0-alpha.2` or later (I86, merged b72ece1 / PR
#127, adds `list;watch` to the marker; `rbac_coverage_test.go` asserts
`get;list;watch` for every type read through the cache). A live workaround
used during the A6 run was to add `list;watch` to the
`coxswain-manager-role` ClusterRole by hand (`.samples/alpha-e2e-1791784606/summary.txt`,
"BLOCKERS FOUND ON THE FRESH CLUSTER" item 1) — but that is a release
defect worked around in place, not the supported path (see also AGENTS.md:
"a release defect found by an e2e is reported and fixed in the release,
never worked around in the script").

## `kubectl debug` denied on a coxswain pod

**What you see:** `kubectl debug` on a coxswain component pod (agent,
model-proxy, egress-proxy, tool-proxy) is rejected:

```
The pods "<pod>" is invalid: : ValidatingAdmissionPolicy
'coxswain-deny-ephemeral-containers' with binding
'coxswain-deny-ephemeral-containers' denied request: Adding ephemeral
containers to coxswain component pods (agent, model-proxy, egress-proxy,
tool-proxy) is denied: the container would run outside the KubeArmor fence
and share the pod's network namespace.
```

**Cause: expected, by design (I85, `docs/REVIEW-PHASE1-R26.md:60`).** The
alpha.4 release ships a `ValidatingAdmissionPolicy`
(`coxswain-deny-ephemeral-containers`) and its binding, so an ephemeral
container added to a coxswain pod would run **outside** the KubeArmor fence
and share the pod's network namespace — the same pod's egress controls would
no longer apply to it. The alpha.4 smoke test recorded the denial
(`.samples/alpha4-smoke-1791809703/summary.md`, "kubectl debug DENIED").
alpha.1–.3 shipped the policy without a working binding (the gate was off
in those releases; I85, merged 41635e9 / PR #130, fixed the rendered
`install.yaml`). To inspect a coxswain pod's contents use `kubectl cp` /
`kubectl exec` from a sidecar that is part of the pod, or attach via the
agent's own tooling; ephemeral containers are intentionally unavailable.

## `NetworkEnforced=False` with `--allow-unenforced-network`

**What you see:** the Loop runs, but `NetworkEnforced` is `False` (reason
`EnforcementDisabled`) and the condition message reads:
"--allow-unenforced-network is set: the Loop runs but the CNI does NOT
police pod → host-network egress (dev escape hatch)"
(`internal/controller/loop_controller.go:992-1000`).

**What it means:** the operator's CNI gate (D38) needs an **enforcing CNI**
(e.g. Calico) to actually police pod → host-network egress. kindnet does
not. `--allow-unenforced-network` is the dev escape hatch that lets the Loop
run anyway on a non-enforcing CNI; the condition records that the CNI does
not police that egress. It is separate from `--allow-unenforced` (the
KubeArmor gate); the two flags are independent.

**The supported topology is decided (D57).** R26 Q2 is answered: the alpha
ships **KubeArmor-only**, with the network gate documented as requiring an
enforcing CNI. The KubeArmor policy gate (A1, ADR-0007) is the alpha's
enforcement layer and is **required**; the D38 network gate is a second,
network-layer gate that needs a CNI that enforces NetworkPolicy (e.g.
Calico — the tested profile is `test/e2e/d38-cni-e2e.sh`). On a CNI that
doesn't enforce NetworkPolicy (kindnet included), the released
`install.yaml` is fail-closed: Loops are held Suspended with
`NetworkEnforced=False` (reason `CNIUnenforced`). To run anyway on a
non-enforcing CNI (dev/kind only), add `--allow-unenforced-network` to the
manager args — see the QUICKSTART "Network gate (D38)" section for the exact
`kubectl patch` command and what that gives up (the CNI does not police pod →
host-network egress; the only egress control left on that path is the
proxy's own code, the same accepted limitation as the D50 DNS-fence note).

**Why the alpha used it:** A6 proved the KubeArmor policy gate but ran the
D38 network gate with this flag on a kindnet cluster — "the network gate was
not exercised: A6 ran with `--allow-unenforced-network`, labelled as the
escape hatch. See Q2." (`docs/REVIEW-PHASE1-R26.md`). That open question (R26
Q2) is now answered by D57 (see above). Note: `config/calico/kustomization.yaml`
is the **D38 test profile** — Calico without KubeArmor, so it sets
`--allow-unenforced` to keep the operator from holding Loops on the KubeArmor
policy while the CNI enforcement is tested. It is **not a supported
configuration**: that `--allow-unenforced` disables the KubeArmor policy gate
that D57 makes required. A supported enforcing setup is **Calico plus
KubeArmor, with neither flag** (`--allow-unenforced` nor
`--allow-unenforced-network`).

## A Loop stuck in Implementing with no model traffic

**What you see:** the phase sits in Implementing for a long time, the
operator is healthy, but the egress/model proxy shows little or no model
traffic. (This is the #124 investigation; the finding it produced is I89
above.)

**Documented diagnosis (PR #124, merged 4653e9a — "Investigate: A3 e2e
stall in Implementing (vLLM backends, environmental)"):**
- Three consecutive e2e runs failed at the 25-minute phase-poll with
  `ph=Implementing` (`FAIL: Loop did not reach Succeeded before redeliver
  (ph=Implementing)`), each with the same signature.
- The runner had made only 4–5 model calls (proxy usage lines, all HTTP 200)
  before going quiet for ~24 minutes. The agent log shows only the runner's
  start line.
- Cluster events show the runner's `blocked → restart` cycle
  (`Back-off restarting failed container agent in pod <loop>-sandbox`):
  a model-loop failure exits 1, `restartPolicy: Always` restarts it, and
  the kubelet backs off.
- The cause was **the vLLM backends behind the nginx LB
  (`192.168.1.20:8000`) stopped serving model calls** while the LB itself
  stayed healthy. The two backends were asymmetric (one handled ~4% of
  completed requests); the vLLM host was not reachable from the dev host
  (SSH to `192.168.1.20` times out), so the LB's per-backend health config
  and the backends' logs for the failure window could not be read from the
  dev host.
- The fix was not in coxswain: on a build of `main` with healthy backends
  the identical e2e path completed in ~4 min (runs 1 and 2 in the PR;
  operator digest byte-identical to the release). The coxswain-side change
  was I89: a hung model call no longer waits the full 10-minute timeout —
  it is bounded to ~8 minutes with a named `ModelUnavailable` condition.

**What to check if you hit this:**
1. Is the model endpoint healthy from a normal pod (not just from the
   host)? A6's preflight probes a pod's reachability to the endpoint
   (`test/e2e/alpha-e2e.sh`), which separates a broken backend from a
   broken DNS path (see the DNS entry above).
2. If the endpoint is an LB over multiple backends, check **each** backend
   directly (PR #124 read `request_success_total` /
   `num_requests_running` / `num_requests_waiting` from each backend, not
   the LB, "because 192.168.1.20:8000 is an nginx LB over TWO vLLM
   backends").
3. Once I89 is deployed, the Loop should surface `ModelAvailable=False`
   (reason `ModelUnavailable`) within ~8 minutes of the hang instead of
   going quiet — so the absence of that condition while the Loop is stuck
   is itself evidence that the runner is not even reaching the model client
   (look at the agent log and the proxy usage lines, as #124 did).
