# Phase 1 review, round 9 — ADR-0007 review

Review of `13a0027` (2026-09-27), since tag `review/phase1-r8`: ADR-0007
(AgentPolicy), the ADR-0006 amendment, `CONTEXT.md` (AgentPolicy, decision
vs activity audit) and plan slices C3/C6/C7/C8.

Each issue is self-contained so it can be picked up independently. Tick the
box and add the commit hash when done. Issue IDs continue project-wide.

Priority: **P1** = fix before the next slice builds on it. **P2** = decide
during Phase 1. **P3** = cleanup, batch whenever.

## Verdict

| Commit | Issue | Verdict | Follow-ups |
|--------|-------|---------|------------|
| `13a0027` | D28 → ADR-0007 | OK + notes | **D29, D30 (P1)**, D31, D32, I32, I33 |

ADR-0007 records the owner's six answers faithfully and keeps the reviewer's
implementation choices clearly marked as overridable. The ADR-0006 amendment
(gVisor opt-in, `egressAllow` removed, open decision 2 resolved) and the
`CONTEXT.md` split into decision vs activity audit are right. The items below
are gaps in the design that C3/C6 would otherwise build in; fold the answers
into ADR-0007 before C3/C6 code.

---

## P1 — Blocking (before C3/C6 code)

### D29. Pod-level egress can't keep the agent away from the model endpoint

- [ ] Decided (ADR-0007 amendment)

**Problem:** a Kubernetes `NetworkPolicy` selects **pods**, not containers.
The proxy container must reach the model endpoint, so the generated
NetworkPolicy has to allow it for the whole sandbox pod — and the **agent
container** then can reach it too, bypassing the proxy. If the endpoint needs
no key (the homelab vLLM is the likely first target), the agent gets
unmetered, unaudited model access. The same applies to anything else only the
proxy should reach.

**Fix:** state that per-container egress is enforced by the **eBPF layer**,
not the NetworkPolicy: the engine policy for the **agent container** allows
network connects only to `localhost` (the proxy) plus the user's
`AgentPolicy` allows; the **proxy container's** policy allows only the
model endpoint. (KubeArmor can scope a policy to a container in a pod.)
The NetworkPolicy remains the coarse outer fence (union of both). C5 gains a
case: the evil agent calls the model endpoint directly → blocked by eBPF,
recorded in the activity audit, and absent from the proxy's metering.

### D30. Enforcement must fail closed when the engine isn't enforcing

**Problem:** if the eBPF engine isn't installed, isn't running on the node
the sandbox lands on, lacks BPF-LSM there (e.g. a K3s node without it), or
the translated policy was rejected, the agent runs **unrestricted** and
nothing says so. KubeArmor in particular can fall back to AppArmor or to
audit-only depending on the node.

- [ ] Decided (ADR-0007 amendment)

**Fix:** the operator only lets the sandbox run (sandbox `OperatingMode:
Running`, or the agent container started) once it has positive evidence
that enforcement is active **on that node** for **that Loop's policy** —
e.g. the engine policy's status is applied and the node reports BPF-LSM
enforcement. Otherwise: condition `PolicyEnforced=False` with a reason
(`EngineUnavailable`, `NodeNotEnforcing`, `PolicyRejected`) and the Loop
waits (no terminal reason, consistent with Q5's "never fail the Loop"; the
owner may prefer a terminal reason — flag it). Seam: envtest with the
engine's status objects faked; e2e on kind with the engine removed → sandbox
never runs.

---

## P2 — Design

### D31. Default-deny exec needs a way to discover the allows an agent needs

- [ ] Decided

Real agents (Claude Code, pi, Codex) exec a lot: `node`, `bash`, `git`,
language toolchains, their own helpers. With default-deny, writing a working
policy by hand per agent image is trial and error. Add a **learn mode**:
`AgentPolicy` (or the Loop) can request an **audit-only posture** — the
engine records instead of blocking — and a small tool (`kubectl cox policy
suggest <loop>` later) turns the recorded activity into a candidate
`AgentPolicy` for a human to review. Ship **reference policies** per
supported agent image under `config/samples/policies/`. Learn mode must be
explicit, visible in status (`PolicyEnforced=False`, reason `AuditOnly`), and
never the default.

### D32. Say which pods an AgentPolicy covers

- [ ] Decided

ADR-0007 says the policy selects "that Loop's sandbox pod". Also decide:
- **Verify Job pods** run the agent's committed code (ADR-0005 D12). They
  need an engine policy too — probably a fixed, operator-owned *verify*
  policy (checkout tools + the check commands' toolchain, no egress), not the
  user's `AgentPolicy`.
- **Publish step / trusted sidecars** — operator-owned, not user-policed; say
  so, and keep them out of the agent container's policy selector.
- **Policy changes mid-Loop** — apply at the next iteration boundary, and
  record the effective policy's hash/generation in each `status.history[]`
  entry, so the decision audit shows what the agent was allowed to do in each
  iteration.

---

## P3 — Cleanup

### I32. Where `blockedCount` comes from

C8's `status.policy.blockedCount` + `PolicyBlocked` condition need the
operator to consume the engine's alert stream (KubeArmor relay), which is a
new runtime dependency and a new trust edge (the operator reads engine
alerts). Name the mechanism in C8's seam, and make sure a relay outage shows
as "count unknown", not zero.

### I33. First real agent is still open

ADR-0006 open decision 1 (which external agent to adapt first) remains the
owner's. D31's reference policies should start with whichever agent the owner
picks. Keep it listed in ADR-0007's "Open owner decisions".

---

## Next round will check

- ADR-0007 amendments for D29, D30 (P1), D31, D32.
- C1, C2, C4 as they land.
