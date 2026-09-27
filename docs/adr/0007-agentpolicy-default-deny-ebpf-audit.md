# ADR-0007: AgentPolicy — default-deny, eBPF-enforced, streamed audit

**Status:** Proposed (records the owner's D28 answers, round 8; to be reviewed
by the reviewer before any C3/C6/C7 code)
**Date:** 2026-09-27
**Supersedes:** ADR-0006's open owner decision 2 (egress/dependency-installs
policy), which Q6 below resolves.
**Amends:** ADR-0006 item 4 (gVisor is no longer a default), per Q3's tension.

## Context

Round 7's D28 introduced the owner's product direction, recorded in
`CONTEXT.md`: *"Agent isolation is the goal, so they can only do what is
allowed by policy. Network isolation, command white lists and auditing."*
Round 8 records the owner's answers to the six questions the reviewer asked.
Those answers are the source of truth here; the items marked *implementation
choice* are the reviewer's recommendations, which the owner can override.

The net effect on ADR-0006: the agent-agnostic, zero-credential, deny-by-
default boundary stays, but the **mechanism** for "what is allowed" becomes an
explicit, owned **Policy** object (default-deny, additive allows) enforced by
an **eBPF/LSM engine outside the agent**, with **streamed** (not stored)
audit.

## Decision

### Q1 — Agent-agnostic, coding is the main (not the only) use case

Isolation, Policy, audit and the runner contract are **agent-agnostic**: no
git, test, or language assumptions in them. The git workspace + acceptance
checks + PR flow stays as the **first (and for v1, the only) Loop workflow**,
but it is written as *one workflow on top of the agnostic layer*, not baked
into it:

- `spec.workspace` and `spec.verify` stay on the **Loop** (they are the
  workflow, not the isolation layer).
- Nothing in `AgentPolicy`, the model proxy, the enforcement engine, or the
  audit stream refers to them.
- `CONTEXT.md` keeps "agents" (not "coding agents") in the definitions of
  Operator, Runner, Policy, and Audit; coding-specific terms (Workspace,
  Acceptance checks, TamperedVerify, PR) are unchanged.

### Q2 — `AgentPolicy`: its own CRD, default-deny, additive allows

A new CRD **`AgentPolicy`** (group `coxswain.wattu.com`).

- **Default deny.** With no policy, an agent may do only the **minimum the
  platform itself needs**: run its entrypoint, read/write its own workspace,
  and talk to the model proxy on `localhost`. Every other capability is
  granted by **adding allow rules**.
- Rules are **only ever allows** — there are no "deny" rules to reason about.
- Multiple policies combine as the **union of allows**.
- *Implementation choice (reviewer rec):* a **namespaced `AgentPolicy`**,
  selected by the Loop (`spec.policyRefs[]`, or a label selector); plus an
  optional **cluster-scoped `ClusterAgentPolicy`** for org-wide allows. Who
  may create policies is plain Kubernetes **RBAC — a different role from who
  may create Loops**, so a Loop author cannot grant themselves more.
- Consequence: the README sample must ship with a **minimal `AgentPolicy`**,
  or the sample agent can do nothing. The C5 evil-agent test's expected
  failures come from the **absence of allows** — which is the property to
  prove.

### Q3 — eBPF enforcement (KubeArmor in BPF-LSM mode)

Command allowlists are enforced by an **eBPF/LSM engine outside the agent**;
in-agent tool allowlists are at most advisory.

- *Implementation choice (reviewer rec):* **KubeArmor** in BPF-LSM mode. Its
  model fits Q2 directly — per-pod `Allow` rules for process, file, and
  network, with a default posture of `block` — and it streams alerts and logs
  (Q4). The operator **translates** each Loop's effective `AgentPolicy` into a
  `KubeArmorPolicy` selecting that Loop's sandbox pod, **owned by the Loop**.
  (Tetragon is the alternative: stronger observability, but allowlists are
  awkward because its enforcement is "kill on match".) Keep the engine behind
  an **internal interface** so it can be swapped.
- **Host check (done at review time):** this dev host runs kernel 6.1 with
  `bpf` in the active LSM list, `CONFIG_BPF_LSM=y`, and BTF present, so
  BPF-LSM enforcement works on kind here. The homelab K3s nodes need the same
  check before production use.
- **Tension to record (amends ADR-0006 item 4):** gVisor
  (`runtimeClassName`) intercepts syscalls in user space, so host eBPF engines
  don't see or enforce inside a gVisor sandbox. With eBPF chosen, **gVisor is
  dropped as a default** (kept as an opt-in where the engine supports it, and
  documented plainly that enforcement is then the runtime's, not eBPF's).

### Q4 — Streamed activity audit (Coxswain emits, never stores)

Coxswain **emits** agent-activity audit as a structured stream; it does **not**
store it. Capture is the cluster's choice (e.g. an existing Alloy/Loki
pipeline).

- **Trusted sources only:** the **model proxy** (each model request/response
  metadata + token counts), the **eBPF engine** (process exec, file access,
  network connects — allowed and blocked), and the **operator** (its own
  decisions, already events + `status.history`). The agent's own traces are
  **not** audit.
- *Implementation choice (reviewer rec):* **JSON lines** on each source's
  stdout with a common envelope —
  `{time, loop, namespace, iteration, source, action, target, verdict,
  detail}` — so any log collector can capture and join them by Loop.
  OpenTelemetry export can come later.
- `CONTEXT.md` splits **Audit trail** into **decision audit** (operator;
  events + history, as now) and **activity audit** (streamed, above).

### Q5 — Block and record (no terminal reason)

A policy violation is **blocked and recorded** in the activity audit. It does
**not** fail or pause the Loop — there is **no `PolicyViolation` terminal
reason**.

- Consequence to record: a blocked agent will usually fail to make progress
  and hit `maxIterations` or the Phase 2 stall detector — the right outcome.
- Add a count of blocked actions to `status`: **`status.policy.blockedCount`**
  plus a **`PolicyBlocked` condition** carrying the latest target, so a stuck
  Loop's cause is visible without reading logs.

### Q6 — Network allowlist is part of `AgentPolicy`

Egress rules live in **`AgentPolicy`** (allowed hosts/CIDRs + ports), not in
`spec.agent`. This **replaces ADR-0006's open decision 2** (dependency
installs): a Loop that needs `go mod download` gets it by a policy that allows
the module proxy — **nothing is open by default**.

- *Implementation choice:* enforce egress in **two layers** — a Kubernetes
  `NetworkPolicy` generated from the allows (pod-level, coarse) **and** the
  eBPF engine's network rules (process-level, and the source of the audit
  records).
- The model proxy's own egress to the model endpoint is **platform
  infrastructure, allowed by the operator, not by the user's policy**.

## Consequences

- New CRD `AgentPolicy` (+ optional `ClusterAgentPolicy`) and a
  `spec.policyRefs[]` on the Loop. The operator translates effective allows →
  `KubeArmorPolicy` (owned by the Loop) + `NetworkPolicy`.
- The eBPF engine is a new dependency installed with the cluster (`make
  kind-up`, version pinned once, like agent-sandbox), behind an internal
  interface.
- `status` gains `policy.blockedCount` + a `PolicyBlocked` condition.
- The README sample ships a minimal `AgentPolicy`. The C5 evil-agent test's
  expected failures are the **absence of allows**.
- ADR-0006: gVisor opt-in (not default); open decision 2 resolved by Q6.
- Plan slices: C3 = NetworkPolicy generated from `AgentPolicy`; new
  **C6** (AgentPolicy CRD + engine-policy translation), **C7** (activity-audit
  stream), **C8** (`PolicyBlocked` condition + counter); C5 extended with a
  disallowed command and a disallowed host.
- Production: the homelab K3s nodes must pass the same BPF-LSM/BTF host check
  before eBPF enforcement is relied on there.

## Open owner decisions (NOT picked by the builder)

- **ADR-0006's open decision 1** (first real agent to adapt) is still open and
  is *not* answered by Q1 (Q1 only fixes the agnostic layer). It is listed,
  not picked.
- Q2/Q3/Q4/Q6's *implementation choices* (namespaced `AgentPolicy` +
  `spec.policyRefs[]`; KubeArmor over Tetragon; JSON-lines envelope; two-layer
  egress) are the **reviewer's** recommendations and are overridable by the
  owner — recorded here as recommendations, not settled.
