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

**Per-container egress is enforced by the eBPF layer, not the NetworkPolicy
(D29, round 9).** A Kubernetes `NetworkPolicy` selects **pods**, not
containers. The proxy container must reach the model endpoint, so a pod-scoped
NetworkPolicy has to allow it for the **whole sandbox pod** — and the
**agent** container then reaches it too, bypassing the proxy (and, if the
endpoint needs no key — the homelab vLLM is the likely first target — the agent
gets unmetered, unaudited model access). The **eBPF layer** is what enforces
**per-container** egress; the NetworkPolicy is only the coarse outer fence
(the union of both containers' allows). The engine policy scopes by
**container** in the pod (KubeArmor can do this):
- the **agent container's** policy allows network connects only to
  `localhost` (the proxy) **plus the user's `AgentPolicy` allows** — nothing
  else;
- the **proxy container's** policy allows only the model endpoint.

C5 gains a case: the evil agent calls the model endpoint **directly** (not
through the proxy) → **blocked by eBPF**, recorded in the activity audit, and
**absent from the proxy's metering**.

**Fail closed when the engine isn't enforcing (D30, round 9).** If the eBPF
engine isn't installed, isn't running on the node the sandbox lands on, lacks
BPF-LSM there (e.g. a K3s node without it), or the translated policy was
rejected, the agent would run **unrestricted** and nothing would say so
(KubeArmor can fall back to AppArmor or audit-only depending on the node). The
operator lets the sandbox run (`OperatingMode: Running` / the agent container
started) **only once it has positive evidence that enforcement is active on
*that node* for *that Loop's policy*** — e.g. the engine policy's status is
applied **and** the node reports BPF-LSM enforcement. Otherwise it sets a
**`PolicyEnforced=False` condition** with a reason — `EngineUnavailable` |
`NodeNotEnforcing` | `PolicyRejected` — and the Loop **waits** (no terminal
reason, consistent with Q5's "never fail the Loop"; *the owner may prefer a
terminal reason — flag, don't pick*). Seam: envtest with the engine's status
objects faked; e2e on kind with the engine removed → the sandbox **never
runs**.

## Learn mode (D31, round 9)

Default-deny exec is hard to author by hand: real agents (Claude Code, pi,
Codex) exec a lot (`node`, `bash`, `git`, toolchains, their own helpers), so a
working per-agent-image policy is trial and error. Add a **learn mode**:

- `AgentPolicy` (or the Loop) can request an **audit-only posture** — the
  engine records instead of blocking;
- a small tool (`kubectl cox policy suggest <loop>` later) turns the recorded
  activity into a candidate `AgentPolicy` for a human to review;
- **reference policies** per supported agent image ship under
  `config/samples/policies/`.

Learn mode must be **explicit**, **visible in status** (`PolicyEnforced=False`,
reason `AuditOnly`), and **never the default**.

## Which pods an `AgentPolicy` covers (D32, round 9)

The user's `AgentPolicy` selects **the Loop's sandbox pod** (agent + proxy
containers, per D29). It does **not** cover:

- **Verify Job pods** (ADR-0005 D12) run the agent's committed code. They get a
  **fixed, operator-owned *verify* policy** (checkout tools + the check
  commands' toolchain, no egress) — **not** the user's `AgentPolicy`.
- **Publish step / trusted sidecars** (C4) are **operator-owned, not
  user-policed** — kept out of the agent container's policy selector.
- **Policy changes mid-Loop** apply at the **next iteration boundary**, and each
  `status.history[]` entry records the **effective policy's hash/generation**,
  so the decision audit shows what the agent was allowed to do in each
  iteration.

## Consequences

- New CRD `AgentPolicy` (+ optional `ClusterAgentPolicy`) and a
  `spec.policyRefs[]` on the Loop. The operator translates effective allows →
  `KubeArmorPolicy` (owned by the Loop) + `NetworkPolicy`.
- The eBPF engine is a new dependency installed with the cluster (`make
  kind-up`, version pinned once, like agent-sandbox), behind an internal
  interface.
- `status` gains `policy.blockedCount` + a `PolicyBlocked` condition.
- **`blockedCount` source (I32, round 9):** the operator derives it by
  **consuming the engine's alert stream** (the KubeArmor relay) — a new runtime
  dependency and a new **trust edge** (the operator reads engine alerts). C8's
  seam names this mechanism; a **relay outage must show "count unknown", not
  zero** (so a stuck Loop's cause is never masked as "nothing was blocked").
- The README sample ships a minimal `AgentPolicy`. The C5 evil-agent test's
  expected failures are the **absence of allows**.
- ADR-0006: gVisor opt-in (not default); open decision 2 resolved by Q6.
- Plan slices: C3 = NetworkPolicy generated from `AgentPolicy`; new
  **C6** (AgentPolicy CRD + engine-policy translation), **C7** (activity-audit
  stream), **C8** (`PolicyBlocked` condition + counter); C5 extended with a
  disallowed command and a disallowed host **and a direct model-endpoint call
  (blocked by eBPF, per D29)**.
- Production: the homelab K3s nodes must pass the same BPF-LSM/BTF host check
  before eBPF enforcement is relied on there.

## Findings (observed, 2026-09-27, dev host kernel 6.1.0-53, KubeArmor v1.7.5)

These are runtime observations that shape how the engine is installed and
operated. They amend the Q3 install story, not the ADR's decisions.

- **F1 — the exec allowlist's block posture is `defaultFilePosture`, not
  `spec.action`.** KubeArmor v1.7.5's BPF-LSM gates the process allowlist's
  block-vs-audit choice on the default posture (the block sentinel is keyed on
  `defaultPosture.FileAction` in the agent's `rulesHandling.go`); the policy's
  top-level `action: Block` is not what does it. `karmor install` defaults
  `defaultFilePosture: audit`, so a disallowed exec is *evaluated and logged*
  ("Armored Up" + an audit alert) but **allowed**. Implication: the posture
  must be `block` (plus `process` visibility) **before the node agent first
  starts** — hence `make kind-up` passes `-b all --viz process,file,network`
  to `karmor install` rather than editing the config afterwards (karmor v1.4.9
  rejects the `-viz` shorthand; only `-b`/`-a`/`-i` have shorthands).
- **F2 — a KubeArmor agent stop/restart can wedge the node's BPF subsystem
  (kernel 6.1).** Observed: an agent process exiting through SIGKILL hung in
  D state at `bpf_trampoline_unlink_prog → unregister_ftrace_direct_multi →
  ftrace_shutdown → synchronize_rcu_tasks` (with `rcu_tasks_kthread` also D),
  holding the BPF trampoline lock; every subsequent `bpf_prog_load` on the
  host then blocked (a fresh cluster's snitch `bpf_check` sat in
  `bpf_trampoline_get`), so no new BPF-LSM program could load until the host
  rebooted. Consequences:
  - `make kind-up` **must not** `rollout restart` the KubeArmor agent after
    changing the posture (the old edit-config + restart flow is both
    unnecessary per F1 and host-disruptive per F2).
  - **Production installs must treat KubeArmor agent restarts/upgrades as
    node-disruptive operations: drain the node first** (a hung agent
    teardown takes the node's BPF subsystem down with it — not just the
    agent). Same applies to `kind delete` of a cluster whose agent has hung
    (it can hang the control-plane container's teardown); the only reliable
    recovery is a host reboot.

## Open owner decisions (NOT picked by the builder)

- **ADR-0006's open decision 1** (first real agent to adapt) is still open and
  is *not* answered by Q1 (Q1 only fixes the agnostic layer). It is listed,
  not picked. **D31's reference policies should start with whichever agent the
  owner picks** (I33, round 9).
- Q2/Q3/Q4/Q6's *implementation choices* (namespaced `AgentPolicy` +
  `spec.policyRefs[]`; KubeArmor over Tetragon; JSON-lines envelope; two-layer
  egress) are the **reviewer's** recommendations and are overridable by the
  owner — recorded here as recommendations, not settled.
