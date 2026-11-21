# Release notes: v0.3.2

A patch release on v0.3.1 with one fix: the CNI probe no longer reports CNIUnenforced on k3s clusters that do enforce NetworkPolicy (fence-wait).

**Not proven on kind.** No pre-tag smoke or upgrade run was done for this tag. The entry below was covered by its PR's unit tests and CI, and by a manual run of the fence-wait script on the homelab cluster (k3s/kube-router) before the PR. That run is not a kind proof.

Entries are added per merged PR, each citing the PR and the squash commit it landed as in the heading, like `RELEASE-NOTES-v0.3.0.md`.

## Summary

- **The CNI probe waits for its own NetworkPolicy before it measures** (#513, 4121e52): the policy pass starts with a `fence-wait` init container, so kube-router's 0.05–1.2s programming lag on a new pod no longer reads as CNIUnenforced. The new flag `--cni-fence-wait-budget` (default 30s) sets the wait.

## Upgrade

No CRD, RBAC or admission change since v0.3.1: replacing the operator image is enough. The probe pod's image is unchanged; fence-wait runs that same image with the same restricted security context.

### `--cni-fence-wait-budget` (#513)

Nothing to set: the default is 30s. It must be positive and below `--cni-probe-timeout` (default 120s). A value outside that range is a startup error.

## Fixes

### The CNI probe waits for its own NetworkPolicy before it measures — #513 (4121e52)

**What was wrong:** on k3s, the embedded kube-router programs a new pod's NetworkPolicy rules 0.05–1.2s after its container starts, even when the policy already exists. The probe's policy pass connected at t=0, inside that window. It saw KUBELET_NODE as REACHABLE and reported CNIUnenforced on a cluster that does enforce, which held sandbox creation (D38/D103).

**What changed:** the policy pass pod now starts with a `fence-wait` init container. It connects to the node's kubelet (`NODE_IP:10250`) every 100ms, and exits once two consecutive connects fail, refused or timed out. The probe then runs its full measurement as before. If the kubelet is still reachable when the budget runs out, fence-wait exits 2 and the probe reports CNIUnenforced, with the detail `KUBELET_NODE (still reachable after the 30s fence-wait budget)`. Any other fence-wait failure is ProbeUnavailable, not a verdict. The control pass runs with no policy and does not fence-wait.

fence-wait only decides when the measurement starts, so it cannot cause a false CNIEnforced: a leak still reads REACHABLE.

The "CNI probe result changed" log line now includes the result's detail.

**On upgrade:** nothing to do; the next probe uses fence-wait. On the homelab, the fence came up 0.20–1.21s after start, and no policy row read REACHABLE afterwards (pi2, pi3, pi4, pi9, pi11).

## Known issues

- The sandbox, proxy, verify and memory-writer pods have the same startup window and do not fence-wait yet; later PRs will add it.
- The v0.3.0 known issues (buildx, multi-arch `kind load`) still apply.
