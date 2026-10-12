# Next phase: beta (v0.1.0)

**Status:** accepted (PR #144). D58–D62 decided by the owner on 2026-10-12:
all recommendations accepted (see the table at the end).

Status 2026-10-12, `main` at 5cc1a97. The alpha is released as
`v0.1.0-alpha.4`. Its acceptance (A6, `docs/PLAN-ALPHA.md`) holds on a fresh
kind cluster from the released `install.yaml`:
- real KubeArmor policy enforcement;
- a self-explaining first-time draft PR on GitHub;
- recovery from operator and sandbox kills;
- `kubectl cox` to follow and steer a Loop.

D57 ships it KubeArmor-only, with the D38 network gate documented as needing
an enforcing CNI. R25–R27 are closed.

**What stops it being a v0.1.0:**
- **Restart durability is shallow.** A2 resumes from the live workspace PVC
  (D53). A lost or corrupted PVC loses the work, and there is no rollback or
  fork (PLAN.md Phase 3).
- **No human gate.** A Loop runs from plan to PR with no chance to approve or
  reject the plan. `AwaitingApproval` exists only as a phase name
  (PLAN.md Phase 4).
- **Verify failure always means "implement again".** There is no re-plan
  path (`onVerifyFail: Plan`, PLAN.md Phase 6).
- **No operational signal or packaging.** There are no Loop metrics and no
  Helm chart, and the network gate has never been proven together with
  KubeArmor (PLAN.md Phase 7; D57).

This phase takes PLAN.md Phases 3, 4, the remainder of 6, and 7 to a tagged
**v0.1.0**. **Phase 5 (shared memory) is proposed for after v0.1.0** (D58).

**Done when (beta acceptance, B6):** on a fresh cluster with a snapshot-capable
StorageClass, a user installs v0.1.0 from Helm or `install.yaml`, runs a Loop
with `approval.mode: Manual`, and:
1. rejects the first plan with a reason, and sees a re-plan that addresses it;
2. approves the second plan; the Loop reaches Succeeded and opens a draft PR;
3. a verify failure (forced in the run) re-plans when `onVerifyFail: Plan`;
4. the PVC is deleted mid-Implementing and the Loop restores from its last
   checkpoint;
5. `cox fork --from iter-N` produces an independent Loop that also succeeds;
6. the Loop metrics are scraped.

## Slices (in order; one PR each, TDD per AGENTS.md test norms)

### B1. Checkpoints (PLAN.md Phase 3, part 1). Needs **D59**
- **What:**
  - a VolumeSnapshot of the workspace PVC after each successful verify,
    named `checkpoint-<loop>-iter-<N>`, recorded in `status.checkpoint`;
  - retention: the last 3 plus every milestone checkpoint; GC the rest;
  - finalizers clean up snapshots and sandboxes in `CleaningUp`.
- **Test bed:** kind + csi-hostpath-driver (D6) on a **new** cluster. The
  existing clusters stay untouched (never delete kind clusters).
- **Acceptance:**
  - envtest for create, retention and GC, with a same-Loop update spec (I43);
  - kind: three iterations leave the right snapshots.

### B2. Restore, rollback and fork (PLAN.md Phase 3, part 2)
- **Restore:** when the workspace PVC is lost, restore it from the last
  checkpoint. This extends the A2 PVC-resume path.
- **Rollback:** `spec.restoreFrom: iter-N`.
- **Fork:** a new Loop seeded from another Loop's checkpoint, plus
  `cox fork --from iter-N`. Forks are one-shot Loops (ADR-0003).
- **Acceptance:** the A2 kill e2e gains a PVC-delete case that ends in the same
  outcome. The forked Loop is independent: its own PVC, branch
  (`coxswain/<loop>-<uid8>`) and PR.

### B3. Plan approval (PLAN.md Phase 4)
- **What:**
  - `spec.approval.mode: Manual` holds the Loop in `AwaitingApproval` until
    `PlanApproved`;
  - the condition carries the plan's SHA-256 and clears if the plan changes;
  - `PlanRejected` with a reason: `onReject: Replan` (the default) feeds the
    reason into the next plan prompt, `onReject: Fail` stops the Loop;
  - `cox approve`, `cox reject --reason`, `cox diff`.
- **Acceptance:**
  - envtest per state: approved, rejected → re-plan, plan changed after
    approval;
  - a mutation that ignores the hash must fail;
  - kind: reject, re-plan, approve.

### B4. Re-plan on verify failure (PLAN.md Phase 6 remainder)
- **What:** `spec.verify.onVerifyFail: Plan` sends a failed verify back to
  Planning with the failure summary, instead of back to Implementing.
- **Judge:** the optional Judge sidecar is proposed for after v0.1.0 (D60).
- **Acceptance:**
  - envtest: a failed verify with `Plan` goes to Planning, and with the
    default goes to Implementing;
  - the stall detector still counts iterations across re-plans.

### B5. Hardening and packaging (PLAN.md Phase 7). Needs **D61, D62**
- **Metrics:** iterations per Loop, pass rate, tokens per success, stall rate
  (memory-write rate waits for Phase 5).
- **Helm chart:** generated with `kubebuilder edit --plugins=helm/v2-alpha`,
  built and published by the release workflow. That needs a workflow edit,
  which is an owner push.
- **Network gate with KubeArmor (D61):** prove Calico plus KubeArmor together
  on a host with no other KubeArmor agent (a VM), per ADR-0007 F2. On success,
  QUICKSTART's supported enforcing setup is backed by evidence.
- **Docs and examples:** three example Loops, plus a QUICKSTART refresh for
  approval and fork.
- **Name check:** confirm the `coxswain.wattu.com` API group against the CNCF
  landscape.
- **Acceptance:** each item above is shown with its pasted output (AGENTS.md).

### B6. Beta acceptance and v0.1.0
- **What:** `make beta-e2e` on a **fresh** snapshot-capable cluster, following
  the refreshed QUICKSTART literally (the Done-when above). The live GitHub run
  uses D55.
- **Then:** tag `v0.1.0`.
- **Acceptance:**
  - all six Done-when steps, with kept evidence and digests;
  - no release defect worked around in the script (I90).

## Explicitly deferred (keep their PLAN.md homes)
- **Phase 5:** shared memory and the Memory Curator (D58).
- **Phase 6:** the Judge sidecar (D60).
- **Risks:** the oscillation detector (PLAN.md Risks), still flagged and
  unscheduled.
- **Carried:** R22 I59 (design), I62 (workflow edits are an owner push).

## Owner decisions needed
| ID | Question | Recommendation |
|---|---|---|
| D58 (**decided: recommendation accepted, owner 2026-10-12**) | Is Phase 5 (shared memory) in v0.1.0? | **No.** It carries the highest risk (memory pollution; ADR-0002 curator) and isn't needed for a usable release. Do it after v0.1.0, in its own phase. |
| D59 (**decided: recommendation accepted, owner 2026-10-12**) | PLAN.md says checkpoints are **mandatory** (fail at start with `CheckpointUnavailable` without snapshot support). That would block every install on a cluster without a snapshot CSI, including default kind. | **Required only when the Loop asks for it** (`restoreFrom`, fork, or a new `spec.checkpoints: Required`). Otherwise take checkpoints when possible, fall back to A2 PVC-resume, and surface a `CheckpointsAvailable=False` condition (I90: fallbacks are visible). |
| D60 (**decided: recommendation accepted, owner 2026-10-12**) | The Judge sidecar in v0.1.0? | **No.** It's optional, can never be the only gate, and adds a second model. Defer it. |
| D61 (**decided: recommendation accepted, owner 2026-10-12**) | Prove the network gate together with KubeArmor before v0.1.0? | **Yes, on a dedicated VM** (no other KubeArmor agent on the kernel, ADR-0007 F2). Otherwise v0.1.0 keeps D57's KubeArmor-only wording. |
| D62 (**decided: recommendation accepted, owner 2026-10-12**) | Helm in v0.1.0 (PLAN.md Phase 7; D54 deferred it there)? | **Yes.** Generated chart plus a release-workflow step. The workflow edit is an owner push (one PR). |

## Estimate
In plan-days at the alpha's pace. The alpha's 2–3 week estimate took about 3
days, with fixes driven by A6's findings.
- B1: 2 days. New snapshot CSI test bed.
- B2: 2 days. Fork touches delivery and naming.
- B3: 1–2 days.
- B4: 1 day.
- B5: 2–3 days. The VM proof depends on host setup.
- B6: 1–2 days, plus fixes it finds.

**Roughly 9–12 plan-days to v0.1.0.** The likely slips are B6 findings (as A6
showed), the D61 VM setup, and the shared host's memory (about 5 GiB free with
three kind clusters up).
