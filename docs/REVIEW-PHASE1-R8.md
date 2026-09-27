# Phase 1 review, round 8 — owner's Policy decisions (D28)

Records the owner's answers to round 7's D28 questions (2026-09-27), the
reviewer's reading of each, and what they imply for the build. Also covers
`a308080` and `0ca67b0`.

The builder writes these up as **ADR-0007**; the reviewer reviews it against
this document. Anything marked *implementation choice* is the reviewer's
recommendation, which the owner can override. The owner's answers themselves
are not up for reinterpretation.

## Verdicts on reviewed commits

| Commit | Issue | Verdict |
|--------|-------|---------|
| `a308080` | bookkeeping (R5/R6) | OK |
| `0ca67b0` | I31 explicit-path staging rule in `AGENTS.md` | OK |

---

## The owner's decisions (verbatim, then what they mean)

### Q1 — "Don't assume coding agents, but yes that's the main use case."

- Isolation, Policy, audit and the runner contract are **agent-agnostic**:
  no git, test or language assumptions in them.
- The git workspace + acceptance checks + PR flow stays as the first (and for
  v1, only) *Loop workflow*, but must be written as one workflow on top of
  the agnostic layer, not baked into it. Concretely: `spec.workspace` and
  `spec.verify` stay on Loop; nothing in `AgentPolicy`, the proxy, the
  enforcement engine or the audit stream refers to them.
- `CONTEXT.md`: keep "agents" (not "coding agents") in the definitions of
  Operator, Runner, Policy and Audit; coding-specific terms (Workspace,
  Acceptance checks, TamperedVerify, PR) are fine as they are.

### Q2 — "Its own CRD, and should be restrictive and needs adding to by default."

- New CRD **`AgentPolicy`** (group `coxswain.wattu.com`).
- **Default deny.** With no policy, an agent may do nothing beyond the
  minimum the platform itself needs (run its entrypoint, read/write its
  workspace, talk to the model proxy on `localhost`). Every other capability
  is granted by adding **allow** rules. Rules are only ever allows; there are
  no "deny" rules to reason about.
- Multiple policies combine as the **union of allows**.
- *Implementation choice (reviewer rec):* namespaced `AgentPolicy`,
  selected by the Loop (`spec.policyRefs[]`, or a label selector); plus an
  optional cluster-scoped `ClusterAgentPolicy` for org-wide allows. Who may
  create policies is then plain Kubernetes RBAC — which should be a different
  role from who may create Loops, so a Loop author can't grant themselves
  more.
- Consequence: the README sample must ship with a minimal `AgentPolicy`, or
  the sample agent can't do anything. The C5 evil-agent test's expected
  failures come from *absence of allows*, which is the property to prove.

### Q3 — "eBPF."

- Command allowlists are enforced by an **eBPF/LSM engine outside the agent**;
  in-agent tool allowlists are at most advisory.
- *Implementation choice (reviewer rec):* **KubeArmor** in BPF-LSM mode. Its
  model is a direct fit for Q2 — per-pod `Allow` rules for process, file and
  network with a default posture of `block` — and it streams alerts and logs
  (Q4). The operator **translates** each Loop's effective `AgentPolicy` into
  a `KubeArmorPolicy` selecting that Loop's sandbox pod, owned by the Loop.
  (Tetragon is the alternative: stronger observability, but allowlists are
  awkward because enforcement is "kill on match".) Keep the engine behind an
  internal interface so it can be swapped.
- **Host check (done at review time):** this dev host runs kernel 6.1 with
  `bpf` in the active LSM list, `CONFIG_BPF_LSM=y` and BTF present, so BPF-LSM
  enforcement works on kind here. The homelab K3s nodes need the same check
  before production use.
- **Tension to record:** gVisor (`runtimeClassName` in ADR-0006 item 4)
  intercepts syscalls in user space, so host eBPF engines don't see or
  enforce inside gVisor sandboxes. With eBPF chosen, **drop gVisor as a
  default** (keep it as an opt-in where the engine supports it, and say
  plainly that enforcement is then the runtime's, not eBPF's). Update
  ADR-0006 accordingly.

### Q4 — "Stream audit logs so they can be captured if needed."

- Coxswain **emits** agent-activity audit as a structured stream; it does not
  store it. Capture is the cluster's choice (e.g. an existing
  Alloy/Loki pipeline).
- Trusted sources only: the **model proxy** (each model request/response
  metadata + token counts), the **eBPF engine** (process exec, file access,
  network connects — allowed and blocked), and the **operator** (its own
  decisions, already events + `status.history`). The agent's own traces are
  not audit.
- *Implementation choice (reviewer rec):* JSON lines on each source's stdout
  with a common envelope — `{time, loop, namespace, iteration, source,
  action, target, verdict, detail}` — so any log collector can capture and
  join them by Loop. OpenTelemetry export can come later.
- `CONTEXT.md`: split **Audit trail** into *decision audit* (operator; events
  + history, as now) and *activity audit* (streamed, above).

### Q5 — "Block and record it."

- A policy violation is **blocked and recorded** in the activity audit. It
  does **not** fail or pause the Loop — no `PolicyViolation` terminal reason.
- Consequences to record: a blocked agent will usually fail to make progress
  and hit `maxIterations` or the Phase 2 stall detector, which is the right
  outcome. Add a count of blocked actions to `status` (e.g.
  `status.policy.blockedCount` + a `PolicyBlocked` condition with the latest
  target) so a stuck Loop's cause is visible without reading logs.

### Q6 — Network allowlist is part of Policy: "write up and review".

- Egress rules live in `AgentPolicy` (allowed hosts/CIDRs + ports), not in
  `spec.agent`. This replaces ADR-0006's open decision 2 (dependency
  installs): a Loop that needs `go mod download` gets it by a policy that
  allows the module proxy — nothing is open by default.
- *Implementation choice:* enforce egress in two layers — a Kubernetes
  `NetworkPolicy` generated from the allows (pod-level, coarse) and the eBPF
  engine's network rules (process-level, and the source of the audit
  records). The model proxy's own egress to the model endpoint is platform
  infrastructure, allowed by the operator, not by the user's policy.

---

## What the builder does next

1. **Write ADR-0007** "AgentPolicy: default-deny, eBPF-enforced, streamed
   audit" covering Q1–Q6 as above; amend ADR-0006 (gVisor no longer default;
   open decision 2 resolved by Q6). Update `CONTEXT.md`: define **AgentPolicy**,
   split **Audit trail**, keep "agents" generic.
2. **Update the plan:** C3 becomes "NetworkPolicy generated from
   `AgentPolicy`"; add **C6** "`AgentPolicy` CRD + translation to the engine
   policy (envtest seam: the generated `KubeArmorPolicy`/`NetworkPolicy` for a
   given set of allows; default-deny when none)", **C7** "activity-audit
   stream (seam: the proxy's JSON lines; the engine's alerts carry the Loop
   labels)", and **C8** "`PolicyBlocked` condition + counter". Extend **C5**
   (evil agent) with a disallowed command and a disallowed host, asserting
   each is blocked, the Loop keeps running, and each block appears in the
   audit stream.
3. Add a `make kind-up` step to install the eBPF engine (version pinned once,
   like agent-sandbox), and a smoke that proves a disallowed `exec` is blocked
   on kind.
4. C1, C2, C4 continue meanwhile.

The reviewer reviews ADR-0007 and the plan diff before any C3/C6/C7 code.

Builder response:
- ADR-0007 written (docs/adr/0007-agentpolicy-default-deny-ebpf-audit.md)
  covering Q1–Q6; ADR-0006 amended (gVisor opt-in per Q3's tension; open
  decision 2 resolved by Q6; `spec.agent.egressAllow?` dropped — network is in
  AgentPolicy).
- CONTEXT.md: **AgentPolicy** defined; **Audit trail** split into *decision
  audit* + *activity audit*; "agents" kept generic.
- TDD-PLAN-PHASE1: C3 is now "NetworkPolicy generated from `AgentPolicy`";
  C6 (AgentPolicy CRD + engine-policy translation), C7 (activity-audit stream),
  C8 (`PolicyBlocked` condition + counter) added; C5 extended with a disallowed
  command + a disallowed host; the R8 gate (no C3/C6/C7 code before ADR-0007
  review) and the `make kind-up` eBPF-engine step recorded.
- No C3/C6/C7 code written yet — held for the reviewer's ADR-0007 review.
  C1/C2/C4 may proceed. Committed as 13a0027.
