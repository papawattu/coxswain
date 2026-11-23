# Release notes: v0.3.3

A patch release on v0.3.2. Every pod that gets a NetworkPolicy now waits for that policy before it runs anything (fence-wait): the Loop sandbox, the model, Judge, egress and tool proxies, and the verify, memory-writer, Judge and deliver Jobs. It also contains a CNI probe fix (I281).

**Kind proof (partial).** Review rounds R61 (`3aff005`) and R62 (`fe46e76`, then `7ab11ec`) ran fence-wait on kind-ref (Calico):
- fence-wait is `initContainers[0]` and exits `FENCE_UP` in the probe, the sandbox pods, the model proxy, and the verify, Judge, deliver and memory-writer Job pods;
- a no-policy control times out (exit 2) and a deny-all control passes (exit 0);
- `forbidden` = 0.

#525 (I281) landed after those runs and is covered by its unit tests and CI only.

Entries are added per merged PR, each citing the PR and the squash commit it landed as in the heading, like `RELEASE-NOTES-v0.3.0.md`.

## Summary

- **The Loop sandbox pod waits for its own fence** (#515, e142030): the workspace clone and the agent no longer start inside kube-router's 0.05–1.2s programming window. A budget that runs out holds the sandbox, with the new condition `SandboxFenceNotReady`.
- **Every proxy pod waits for its own fence** (#521, 0d8814c): this covers the model, Judge, egress and tool proxies.
- **Every Job pod waits for its own fence** (#522, 7ab11ec): this covers the verify, memory-writer, Judge and deliver Jobs.
- **The CNI probe waits for stale probe pods to go, and retries an unavailable result on a backoff** (I281, #525, 76b5d4d).

## Upgrade

There is no CRD schema, RBAC or admission change since v0.3.2, so replacing the operator image is enough. `SandboxFenceNotReady` is a new condition type on the existing `status.conditions`.

**On upgrade:**
- Each proxy pod is recreated once, because its spec hash changes.
- The operator updates each Sandbox's pod template but does not delete a running sandbox pod. A sandbox pod starts with fence-wait the next time it is created.
- No flag needs setting. `--cni-fence-wait-budget` (default 30s) now also bounds the sandbox, proxy and Job fence-waits. `--cni-probe-image` (default `python:3.13-alpine`, pinned by digest) is the image they run.
- Under `--allow-unenforced-network`, no pod gets fence-wait: it could never pass.

**Air-gapped clusters:** every node that runs Loop pods now pulls `--cni-probe-image`, not just the nodes that ran the probe.

**Clusters running KubeArmor** (or another LSM): fence-wait runs `python3` in every operator pod. A policy that blocks `python3` or its connect makes fence-wait fail closed. EPERM never counts as fenced, so the pod is held and never runs unfenced. Allow it before upgrading.

**ResourceQuota:** fence-wait requests 10m CPU and 16Mi memory, with limits of 100m CPU and 64Mi memory, in every pod.

## Changes

### The Loop sandbox pod waits for its own fence — #515 (e142030)

**What was wrong:** k3s's kube-router programs a new pod's NetworkPolicy 0.05–1.2s after the pod starts (D38). The sandbox's workspace clone holds the git credential, and it could start inside that window, with the node's kubelet and the rest of the cluster reachable.

**What changed:**
- The sandbox pod's first init container is fence-wait, the same container the CNI probe has run since #513 (v0.3.2).
- fence-wait connects to the node's kubelet (`NODE_IP:10250`) every 100ms, and exits 0 once two connects in a row are refused or time out.
- If the budget runs out, it exits 2 and the kubelet retries the init container. The agent never starts.
- While fence-wait is out of budget, the Loop has `SandboxFenceNotReady=True` (reason `FenceNotReady`) and one Warning Event. The condition returns to `False` (reason `FenceUp`) once fence-wait passes or the pod is gone.

Only NetworkPolicy-shaped failures count as fenced: ECONNREFUSED, a timeout, or EHOSTUNREACH. EPERM (an LSM) or ENETUNREACH resets the streak, so fence-wait can never pass early. This also applies to the CNI probe.

### Every proxy pod waits for its own fence — #521 (0d8814c)

**What changed:**
- The model proxy, Judge proxy, egress proxy and tool proxy pods each start with fence-wait, running as the proxy's own non-root user.
- The proxies restart Always, so a timed-out fence-wait is retried by the kubelet, and the proxy is never Ready unfenced.
- When fence-wait has run out of budget, the model proxy's not-ready reason says so: "its fence-wait ran out of budget (the node's kubelet stayed reachable under its NetworkPolicy: check that the CNI enforces NetworkPolicy)".

**Public node IPs:** the egress and tool proxies allow `0.0.0.0/0` minus the private ranges. So on a node whose InternalIP is public, the kubelet stays reachable, and fence-wait never passes there. This is fail-closed and covered in `docs/TROUBLESHOOTING.md`.

### Every Job pod waits for its own fence — #522 (7ab11ec)

**What changed:** the verify, memory-writer, Judge and deliver Job pods start with fence-wait. A verify pod for a Loop with no repo gets none, because it has no NetworkPolicy. These Jobs don't restart (`RestartPolicy: Never`, `backoffLimit: 0`), so a fence-wait timeout fails the pod. It never counts as a verdict on the agent's work:

| Job | A fence-wait timeout shows up as |
|---|---|
| verify | an infra failure naming fence-wait (bounded retry, then `Failed:VerifyInfraFailed`) |
| deliver | `DeliveryFailed`; re-run with the `coxswain.io/redeliver` annotation |
| Judge | `JudgeUnavailable` (advisory; the outcome is unchanged) |
| memory writer | a failed write: that run's lessons are not saved |

fence-wait now has CPU and memory requests and limits in every pod (F58).

### The CNI probe waits for stale probe pods to go, and retries an unavailable result on a backoff — I281, #525 (76b5d4d)

**What was wrong:** after an upgrade, the first probe found the previous `coxswain-cni-probe-control` pod still terminating and reported `ProbeUnavailable`. The Loop was held, fail-closed, until the next tick: 10 min 18 s in R62.

**What changed:**
- After cleanup, the probe waits until its pods are gone. If they are still there when the probe times out, the result is `ProbeUnavailable` with the detail "stale cleanup failed".
- After a `ProbeUnavailable` result, the next probe runs on a backoff that starts at 15s and doubles, up to the probe interval. Any other result resets the delay to the interval.

## Known issues

- On a node with a public InternalIP, the egress and tool proxies' fence-wait never passes (see #521 above).
- I278: the probe says "still reachable" for any fence-wait timeout.
- I279: `SandboxFenceNotReady` is not refreshed while a Loop is Paused or terminal.
- The v0.3.0 known issues (buildx, multi-arch `kind load`) still apply.
