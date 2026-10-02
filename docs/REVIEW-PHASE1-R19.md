# Phase 1 review, round 19: the observer agent (parked) and the seams to build now

Design round, 2026-10-02, since tag `review/phase1-r18`. No code findings.

**Owner direction (2026-10-02):** plan an **observer agent**: an agent that
supervises other agents' runs by tracking progress, unblocking and summarising
state. **Park the feature** until the current queue (S3–S5, delivery, D44) is
done. But **design its seams into whatever gets built before then**, even as
stubs or empty extension points, so the observer doesn't force rework later.

The observer generalises the reviewer role run by hand on 2026-10-01/02
(watching the builder, checkpointing and handing off, diagnosing loops,
verifying claims, summarising for the owner). That night's lessons are its
requirements. House rules apply (D36 extension points, D39, I43, I45, ADR-0006).

---

## P1: Design decision (parked)

### D45. Observer agent: track, unblock, summarise

- [ ] Owner decision: questions below (parked until after S5 + delivery)
- [ ] Planned as slices O1–O4 (parked)
- [ ] **Seams built into in-flight slices (not parked; see the next section)**

**Requirements, from the 2026-10-01/02 run:**

| Observed | Requirement |
|---|---|
| The worker ran out of context and lost state; checkpoints and handoffs were manual | The **operator owns run state** (plan, progress, open items); a worker can be reset and resumed from it with no loss |
| Worker claims were disproved by kind runs; a fake prober hid 7 bugs | **Verify, don't trust:** observations come from operator-side evidence (status, check exit codes, digests), never the worker's self-report |
| The worker repeated near-identical failing actions | **Stuck detection** (repeated action, same failing check, no progress for N minutes) followed by **root-cause diagnosis** |
| A diagnosis was based on a stale cluster object | Every observation records the **commit/generation** it was taken against |
| Permission prompts waited ~15 min for a human | **Policy-bounded actions:** an explicit allow-list for safe actions; everything else escalates |
| An improvised auto-approver was (rightly) blocked | The observer can **never widen its own powers**: humans set its policy, and it can't approve its own interventions |
| Typed messages were lost; handoffs dropped | Interventions are **delivered and acknowledged through the operator**, not a terminal |
| Morning summary; decision list with pros, cons and a recommendation | First-class **digests** and **decision briefs** |
| A credential leak was found while debugging | The observer runs the security rules (ADR-0006, D39, I45) as standing checks |

**Design:**
1. **Resources:**
   - `Observer`: scope (Loops, a campaign or a label selector), policy, sinks and model endpoint.
   - `Intervention`: an action the observer *proposes*. The operator executes it only if the observer's policy allows it; otherwise it waits for human approval. Every Intervention is recorded in the ledger.
2. **Read path:** read-only, through a scoped *observe* proxy (the D41 tool-proxy pattern). It reads Loop status, conditions and Events, ledger entries, verify-Job exit codes, proxy audit logs and PR/review threads. No exec into agent pods (I45), no credentials.
3. **Detectors** (plug-ins, D36): time in phase, repeated actions, the same failing check, worker context pressure, gates held (D30/D38), claims contradicted by evidence, stale observations.
4. **Interventions, tiered by risk:**
   - **Advisory (automatic):** a diagnosis or hint delivered to the worker's next step.
   - **Bounded (automatic if the policy allows):** checkpoint and reset with a handoff; retry a failed step; approve allow-listed actions.
   - **Human-only, always:** anything touching enforcement (KubeArmor, CNI, policies), credentials or the default branch; deleting anything outside the run's scratch space; any change to the observer's own policy.
5. **Outputs:** an `Observed` condition per Loop, periodic digests, decision briefs when a human choice blocks progress, and pluggable notification sinks (PR comment by default).
6. **Isolation:** its own sandbox and its own model endpoint through the model proxy. It may use a stronger model than the workers.

**Slices (parked):**
- **O1:** read-only tracking and digests.
- **O2:** stuck detection and advisory diagnosis.
- **O3:** bounded interventions (the `Intervention` CRD, policy, human approval path, ledger).
- **O4:** decision briefs and campaign roll-ups (D42).

**Questions for the owner (when unparked):**
1. Scope: coxswain Loops only, or also external agents through an adapter?
2. Autonomy ceiling: which actions may it take unattended?
3. Model: the workers' local vLLM, or a stronger model?
4. Channel: PR comments, chat, email or a dashboard?

**Recommendation:** O1–O2 first (low risk, most of the value); O3 only after
O2's diagnoses have been seen in practice.

---

## P1: Seams to build now (not parked)

Each seam below is added by the slice that is **already building that area**,
as a stub, a reserved field or an interface with a no-op default. No observer
behaviour ships. The reviewer checks each seam as part of the named slice's
acceptance.

| # | Seam | Built in | Stub shape |
|---|---|---|---|
| OS1 | **Structured progress record:** the ADR-0004 `result.json` reader writes more than `observedPhase`: a `status.progress` block with `phase`, `lastActivityTime`, `iteration`, `lastResultStatus` (success/blocked), `blockedReason`, and the `observedGeneration`/`baseCommit` it was read against | **S4** (the ADR-0004 reader) | Fields populated from the claim, size-limited, never a gate input (ADR-0005) |
| OS2 | **Runner heartbeat and action log:** `result.json` gains `lastActivityTime` and a bounded `toolTrace` (already present) with action fingerprints, so repeated actions are detectable | **S4** (the runner phase driver) | Emitted by the runner; the operator copies counts and timestamps only, not content |
| OS3 | **Hint inbox, the advisory delivery channel:** the runner reads `.coxswain/hints/*.md` (operator-written, read-only to the agent) at the start of each step and includes them in its prompt | **S4** (the runner phase driver) | The runner reads the directory if it exists; empty by default. The operator has a `HintWriter` interface with a **no-op** implementation |
| OS4 | **Handoff state:** at each phase boundary the runner writes `.coxswain/HANDOFF.md` (done / next / open items, ≤4 KB), so a reset worker can resume | **S4** | Written by the runner; nothing consumes it yet |
| OS5 | **Consistent Events:** the operator emits a Kubernetes Event on every phase transition and gate hold/release, with stable reasons (`PhaseAdvanced`, `GateHeld`, `GateReleased`, `VerifyFailed`) | **S4** (phase transitions) + **S5** (verify) | Events only; reasons documented as an API |
| OS6 | **Machine-readable evidence:** `make sample-run` writes `evidence.json` alongside `EVIDENCE.md` (phases with timestamps, check exit codes, digests, policy hash, gate conditions) | **S5** | The JSON is the source; the Markdown is rendered from it |
| OS7 | **Notification sink interface:** delivery's "open a PR" goes through a `Notifier`/`Sink` interface (PR comment as the built-in default) that the observer will reuse for digests | **Delivery** (after S5) | Interface plus the one built-in implementation |
| OS8 | **Phase-gate interface:** keep the phase table's gate check behind an interface (`PhaseGate`), so `AwaitingApproval` (deferred, option B) and later observer-proposed holds plug in without rewriting `nextPhase` | **S4** | One implementation: auto-approve (option B) |
| OS9 | **Ledger entries carry freshness:** every ledger record includes `observedGeneration` and the commit SHA it refers to | Whichever slice next touches the ledger (S4 or S5) | A field addition |
| OS10 | **Reserved condition type:** document `Observed` as a reserved Loop condition type (no writer yet) | **S4** (API types) | A doc comment and constant only |

**Rules for the seams:**
- No new privileges. Stubs don't widen RBAC; the hint inbox is written by the operator and read-only to the agent.
- Size-limited and untrusted. Everything the worker emits (OS1, OS2, OS4) is a claim: bounded, schema-validated, never a gate input.
- Tested as plumbing only. Each seam has a unit or envtest proving the field, file or interface exists and is wired; no observer behaviour is tested because none exists.

**Acceptance:** each listed slice's PR states which seams it adds, and the
reviewer checks them as part of that slice's verdict.
