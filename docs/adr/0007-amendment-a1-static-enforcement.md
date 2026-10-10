# ADR-0007 Amendment A1: static enforcement evidence (D52)

**Status:** Proposed (design note only — no code; to be reviewed before the A1
implementation). Records the owner's D52 decision (2026-10-10,
`docs/PLAN-ALPHA.md`).
**Date:** 2026-10-10
**Supersedes:** ADR-0007 Q3's "host check" and D30's "e.g. the engine policy's
status is applied **and** the node reports BPF-LSM" — this amendment makes that
"e.g." the concrete, specified static check (D52 (b)).
**Amends:** ADR-0007 D30 (the fail-closed gate) and Q3 (the KubeArmor engine).

## Context

ADR-0007 D30 requires the operator to let the sandbox run **only once it has
positive evidence that enforcement is active on *that node* for *that Loop's
policy***, and to hold the Loop Suspended (`PolicyEnforced=False`) otherwise.
The KubeArmor enforcer's `Enforcing` seam (ADR-0007 Q3, the engine-agnostic
interface) is the point that reports that evidence. Today it is a stub:

```go
// internal/engine/kubearmor_enforcer.go
func (e *KubeArmorEnforcer) Enforcing(_ context.Context, _ *v1alpha1.Loop) (bool, string) {
	// TODO(I32): consume the KubeArmor relay alert stream for positive evidence.
	// Until then, report that no enforcement probe exists (I46).
	return false, ReasonEnforcementUnverified
}
```

So every Loop runs only with `--allow-unenforced` (the off-by-default escape
hatch), and `PolicyEnforced` is always `False` reason
`EnforcementUnverified`/`EngineUnavailable`.

ADR-0007 Q3 listed **two** candidate evidence sources for the gate:

- **(a)** the KubeArmor **relay alert stream** — strong (it is the engine's own
  "I blocked a disallowed action" evidence) but adds a **runtime dependency**
  and a **trust edge** (the operator reads engine alerts; a relay outage would
  show "count unknown" per the I32 rule, and the gate would have to treat an
  absent stream as not-enforcing).
- **(b)** a **static check**: the KubeArmor **agent pod is Ready** on the
  sandbox's node, the **node reports BPF-LSM** (label
  `kubearmor.io/enforcer=bpf`), and the Loop's **KubeArmorPolicy exists and
  matches** (exists, controlled by the Loop, spec matches the operator's
  rendered policy, selector matches the sandbox pod).

**D52 (owner, 2026-10-10): (b), the static check, for alpha.** It is cheap,
deterministic, and fits the "operator reads facts" model (no new trust edge, no
relay to keep alive). (a) is the later upgrade.

## Decision

### D52 (b): the static check is the D30 gate's evidence for alpha

The `Enforcing` seam reports **`True` only when all three facts hold**, read
from cluster state (envtest-fakeable; on kind, the real objects):

1. **The KubeArmor agent pod is Ready on the sandbox's node.** The KubeArmor
   agent is a DaemonSet in namespace **`kubearmor`** whose name carries a
   config hash (e.g. `kubearmor-bpf-containerd-98c2c`) and the KubeArmor
   operator can run **several** of them (one per node-config), so the evidence
   is the **pod**, not the DaemonSet by name. Select the agent pod on the
   sandbox's node:

   - namespace **`kubearmor`** (operator config),
   - label **`kubearmor-app=kubearmor`** (operator config),
   - `spec.nodeName == <sandbox node>` (`spec.nodeName` of the sandbox pod),
   - owner kind **`DaemonSet`** (owned by a DaemonSet, not a Deployment),
   - `Ready=True` (the pod's `Ready` condition is True).

   The namespace and label are **operator config** (the Enforcer interface
   stays engine-agnostic — the operator passes the engine's selectors, not
   hard-coded ones). The sandbox may land on any node, so the evidence is
   per-node, not per-DaemonSet.

2. **The node reports BPF-LSM.** The sandbox's node carries the label
   **`kubearmor.io/enforcer=bpf`** (set by KubeArmor's node probe after it loads
   the BPF programs). A node whose label is `apparmor` or **absent** means the
   engine is **not** enforcing BPF-LSM there (e.g. a K3s node without it, per
   ADR-0007 Q3). **Limit, stated plainly:** this label is a **static,
   install-time claim** — it records that the node is *configured* for BPF-LSM,
   not live proof that BPF programs are *currently loaded and enforcing*. The
   relay alert stream (a) is what gives live evidence; the static check
   accepts the install-time claim as its D52 trade-off.

3. **The Loop's KubeArmorPolicy exists and matches.** The `KubeArmorPolicy` the
   operator emits for the Loop (owned by the Loop, ADR-0007 Q3) satisfies **all
   four** of the following (read from the policy object itself, not from any
   status field):

   - **exists:** the `KubeArmorPolicy` object is present in the cluster.
   - **controlled by this Loop:** `metav1.IsControlledBy(policy, loop)` is true
     (the Loop owns the policy, so a stale policy from a deleted Loop is not
     counted).
   - **spec matches the operator's rendered policy:** the policy's `spec` (its
     selector + rules) matches what the operator rendered for this Loop — i.e.
     the effective-policy hash the operator already records for the Loop (the
     operator computes the hash when it emits the policy; the check re-computes
     it from the policy's `spec` and compares).
   - **selector matches the sandbox pod's labels:** the policy's
     `spec.selector.matchLabels` match the sandbox pod's labels (so the policy
     *would* apply to the sandbox pod if it ran).

   **Limit, stated plainly:** `KubeArmorPolicy` has **no status field** on the
   pinned version (`.status` is empty on `samples/coxswain-gocli-task1`), so
   this fact **cannot** confirm that the KubeArmor agent has *loaded* the
   policy. Static evidence proves the policy is *correctly specified* and
   *would match the sandbox pod*; the relay alert stream (a) is what proves the
   agent *enforced* it. This is the accepted D52 trade-off.

**`PolicyEnforced=True` only when all three are `True`.** The gate (D30) opens
— the sandbox is allowed `OperatingMode: Running` — **only on `True`**.

### Missing fact or outage reads `Unknown`, never `True`

The check is **fail-closed and read-from-facts**: every fact that **cannot be
read** (the object is absent, the API is unreachable, the field is missing) is
reported as **`Unknown`**, and `Unknown` is **never `True`** — the gate stays
Suspended. Specifically:

- The sandbox pod is **not yet bound to a node** (`spec.nodeName` empty) → the
  agent-pod-Ready-on-node fact is **Unknown** (no node to check) → not
  enforcing.
- The agent pod (ns `kubearmor`, label `kubearmor-app=kubearmor`, on the
  sandbox's node) is **absent or not Ready** → **Unknown**/not-enforcing. An
  agent pod that is **rolling** (not yet Ready) is **in-progress**, not `True`
  (see below).
- The node's BPF-LSM fact **cannot be read** (the field is absent, the node
  object is gone) → **Unknown** → not-enforcing.
- The `KubeArmorPolicy` is **absent** (not yet emitted by the operator) →
  **Unknown** → not-enforcing. The policy exists but is **not controlled by
  this Loop** (stale policy from a deleted Loop) → **Unknown** → not-enforcing.
  The policy's `spec` does **not match** the operator's rendered policy (hash
  mismatch) → **Unknown** → not-enforcing. The policy's selector does **not
  match** the sandbox pod's labels → **Unknown** → not-enforcing.
- An **API outage** (the operator cannot read any of the three facts) →
  **Unknown** → not-enforcing. The gate must **never** infer `True` from an
  absent read.

This is the I46/I32 discipline applied to the gate: **a missing fact is never
evidence of enforcement.** `True` requires all three facts positively read;
anything short of that is `Unknown` (the condition message names which fact is
missing, e.g. `EngineUnavailable` | `NodeNotEnforcing` | `PolicyRejected` | a
new `EnforcementUnverified`/`EvidencePending` reason).

### In-progress states hold the gate (I49 norm)

The per-input **in-progress** states are the ones where a fact is **present but
not yet satisfied** — they hold the gate (Suspended), requeueing, exactly as the
I49 norm requires (no decision while in progress):

- **Agent pod rolling:** the agent pod exists on the node (ns `kubearmor`,
  label `kubearmor-app=kubearmor`, `spec.nodeName == <sandbox node>`), but it is
  **not yet Ready** (ImagePullBackOff, CrashLoopBackOff, or mid-rollout) →
  in-progress → gate held.
- **Policy not yet created:** the operator has not yet emitted the
  `KubeArmorPolicy` (the policy object is **absent**) → in-progress → gate
  held. (This is the observable in-progress state for fact 3, since the policy
  has no status field — there is no "pending" status to read.)
- **Node BPF-LSM label not yet set:** the node is known, the agent pod is
  Ready, but the `kubearmor.io/enforcer=bpf` label has not yet been set (the
  agent is still loading BPF programs and has not yet reported to the node
  object) → in-progress → gate held.

A decision that reads any of these (DaemonSet pod readiness, node BPF-LSM
status, KubeArmorPolicy status) gets a spec for **each in-progress state** as
well as each terminal state, asserting **no decision** (the gate stays
Suspended) while in progress — the R20 I49 norm.

### Consequences for the A1 implementation (no code in this note)

- `Enforcing` is replaced by the static check: it reads the three facts from
  cluster state (via the operator's client, envtest-fakeable) and returns
  `(true, Enforcing)` only when all three are `True`; otherwise
  `(false, <reason>)` where the reason names the missing/pending fact.
- The `PolicyEnforced` condition carries the **reason** (which fact failed), so
  a held Loop's cause is visible without reading logs (consistent with
  ADR-0007 Q5).
- The gate (D30) is unchanged in shape: it opens only on `True`; `Unknown`/
  in-progress holds Suspended; `--allow-unenforced` is the off-by-default
  escape hatch (it runs with `PolicyEnforced=False`, never `True`).
- The **relay alert stream (a)** is the later upgrade: when it lands,
  `Enforcing` can consult it for the strongest evidence (a blocked-alert count)
  in addition to the static check. The static check remains the alpha baseline.

## Acceptance (the A1 implementation)

- **On kind-coxswain-dev**, a Loop with its policy applied reads
  `PolicyEnforced=True` **without** `--allow-unenforced`, and its sandbox runs.
- **Mutation:** make the check **always fail** (e.g. hard-code the
  `Enforcing` reason to `EngineUnavailable`) → the Loop is held
  **Suspended** (the gate opens only on `True`). Each of the three facts is
  independently mutable: dropping each one makes the corresponding spec fail.
- **Per-input in-progress states** (agent pod rolling, policy not yet created,
  node BPF-LSM label not yet set) hold the gate — the I49 norm, with
  no-decision specs for each.
- **Missing fact / outage = Unknown, never True:** each fact removed (the agent
  pod deleted, the node's `kubearmor.io/enforcer` label cleared, the
  KubeArmorPolicy deleted, the policy's owner reference removed, the policy's
  spec corrupted) → the condition is `Unknown`/not-`True` and the gate stays
  Suspended.
- **The D30 gate opens only on `True`:** a spec with all three facts `True`
  (agent pod Ready on the node, node label `kubearmor.io/enforcer=bpf`, policy
  exists + controlled + spec-matches + selector-matches) →
  `PolicyEnforced=True`, sandbox Running; a spec with any one fact
  `Unknown`/in-progress → the gate held.
- **Every fact names a field or object that exists on the pinned KubeArmor
  version** (checked on kind), and none relies on a status field that's never
  set (the KubeArmorPolicy has no status; the node label is the BPF-LSM
  source; the agent pod is selected by ns+label+nodeName+owner+Ready).

## What this amendment does NOT change

- The **relay alert stream (a)** (the later upgrade) — D52 defers it; it is not
  part of the alpha gate.
- **D29** (per-container egress via the eBPF layer) and **D34/D42** (the egress
  proxy) — the static check gates the *engine's* enforcement evidence; the
  egress proxy is a separate, independent gate (D38, the CNI/NetworkPolicy
  evidence).
- **Learn mode (D31)**, **D46** (no agent exec fencing), **D35** (per-container
  KubeArmor policies) — unchanged; the static check reads the same
  `KubeArmorPolicy` those decisions produce.
- The **engine-agnostic seam** (`Enforcer`) — the static check lives in the
  KubeArmor implementation; the operator's gate reads it through the interface,
  so the engine stays swappable.
