# Release notes: v0.3.6

A patch release on v0.3.5. The egress proxy's KubeArmorPolicy now allows every host the proxy serves, including an external repo's host (#537). Before, under KubeArmor's DNS enforcement, a Loop with an external repo and AgentPolicy network allows could not clone.

**Found live.** On the homelab, the first Loop under KubeArmor (operator v1.7.5, coxswain v0.3.5) stuck in the sandbox's `init-workspace`: the clone failed with `CONNECT tunnel failed, response 403`, and KubeArmor logged `MatchedPolicy DefaultPosture Network Block github.com`.

Entries are added per merged PR, each citing the PR and the squash commit it landed as in the heading, like `RELEASE-NOTES-v0.3.0.md`.

## Summary

- **The egress proxy's KubeArmorPolicy allows every host it serves** (#537, f5a833e).

## Upgrade

There is no CRD schema, RBAC or flag change since v0.3.5, so replacing the operator image is enough.

**Workaround to remove:** if you added an external repo's host (for example `github.com:443`) to an AgentPolicy to get past this bug, it can be dropped after the upgrade.

**Behaviour change, only with KubeArmor enforcing:** an egress proxy that serves only an external repo (no AgentPolicy network allows) now gets a KubeArmorPolicy. Before, it had none, so its inner fence was missing.

## Changes

### The egress proxy's KubeArmorPolicy allows every host it serves — #537 (f5a833e)

**What was wrong:** the egress proxy serves the AgentPolicy's network allows and an external repo's hosts (the workspace clone, the verify clone-base and the deliver push, added in #533). Its KubeArmorPolicy was built from the network allows only:
- **With network allows:** the proxy's DNS lookup of the repo host was blocked, and the clone failed.
- **With no network allows:** the clone-only proxy got no KubeArmorPolicy at all.

**What changed:**
- `EffectivePolicy` has a new field, `EgressProxyHosts`: the proxy's whole allowlist. The controller fills it from the same helper that builds the proxy's allowlist. It is not part of `EffectiveHash`. If the helper fails, the field stays empty and the policy falls back to the network allows: stricter, never wider.
- The egress proxy's KubeArmorPolicy is built from `EgressProxyHosts`, and is emitted whenever the proxy exists.
- The agent's KubeArmorPolicy and `KaptSpecHash` are unchanged.

## Known issues

- A model endpoint given as the in-cluster LoadBalancer IP of a Service in the same cluster is unreachable. The model proxy's NetworkPolicy allows that IP as an `ipBlock`, but kube-proxy rewrites the traffic to the Service's pod IP before the policy is checked, so it never matches (`connection refused`). Use the Service's DNS name (`<svc>.<ns>.svc.cluster.local:<port>`) instead; that gets a `namespaceSelector`.
- The v0.3.5 known issues still apply.
