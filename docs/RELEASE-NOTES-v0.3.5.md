# Release notes: v0.3.5

A patch release on v0.3.4. With `--loop-node-selector` set, the policy gate checks KubeArmor agents only on the Loop pool's nodes (#535). That lets a cluster run KubeArmor agents on the Loop pool only.

**Lab proof.** On k3d-hermes-sim (k3s v1.36, kernel 7.0), the setup mirrored the homelab:
- the Loop pool was labelled and tainted `coxswain.io/sandbox=true`, with one BPF-LSM node outside it;
- KubeArmor operator v1.7.5 ran agents on the pool only.

v0.3.4 held Loop `cox-test/gocli-gitea` with `PolicyEnforced=False/NodeNotEnforcing`. #535's controller ran the same Loop to Succeeded with `PolicyEnforced=True/Enforcing` and 0 blocks.

Entries are added per merged PR, each citing the PR and the squash commit it landed as in the heading, like `RELEASE-NOTES-v0.3.0.md`.

## Summary

- **The policy gate's agent check is scoped to the Loop pool** (#535, 487a238).

## Upgrade

There is no CRD schema, RBAC or flag change since v0.3.4, so replacing the operator image is enough.

**Behaviour change, only with `--loop-node-selector` set:** `PolicyEnforced` needs a Ready KubeArmor agent only on nodes labelled `kubearmor.io/enforcer=bpf` **and** matching the selector. A BPF-LSM node outside the pool no longer holds Loops, with or without an agent. Without the flag, every BPF-LSM node is checked, as before.

**Running KubeArmor agents on the pool only:** in the KubeArmor operator's KubeArmorConfig, set `globalNodeSelector` and `globalTolerations` to the pool's label and taint. The operator's node probe still runs on, and labels, every node. `docs/OPERATOR.md` (the loop pool section) has the details.

## Changes

### The policy gate's agent check is scoped to the Loop pool — #535 (487a238)

**What was wrong:** the gate's first fact needs a Ready agent on every node labelled `kubearmor.io/enforcer=bpf`. KubeArmor's node probe labels every BPF-LSM node, whatever the agents' placement. So agents limited to the Loop pool held every Loop with `NodeNotEnforcing`.

**What changed:**
- With `--loop-node-selector` set, the gate lists only the BPF-LSM nodes that match the selector.
- Every pod with a KubeArmorPolicy is already pinned to the pool: the sandbox, the model, Judge, egress and tool proxies, and the verify, Judge, deliver and memory-writer Jobs.
- A pool with no BPF-LSM node is still `NodeNotEnforcing`.
- `docs/OPERATOR.md`'s "What is pinned" list now includes the Judge proxy and the Judge Job, which were already pinned.

## Known issues

- `docs/QUICKSTART.md` patches `LOGPATH=stdout` into the KubeArmorConfig after install. On k3s, that update made the KubeArmor operator (seen with v1.7.7) re-render the agent DaemonSet without its containerd socket, and every agent crash-looped. On k3s, set `LOGPATH` in the KubeArmorConfig before it is first applied.
- A Loop whose workspace is the coxswain repo itself fails verify with `Failed:VerifyInfraFailed`. The canary builds `./cmd`, which needs more than the verify Job's 2Gi ephemeral-storage limit.
- The v0.3.3 and v0.3.0 known issues still apply.
