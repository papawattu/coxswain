# Phase 1 review, round 31: the ADRs against the code, and what the release P1 fixes left behind

Round 31, 2026-10-24. It follows R30 (PR #211, not yet merged) and covers the
work merged since R30 was drafted. Code line numbers are against main at
`6eb74c2` (re-derived after #213-#216 moved them); ADR line numbers are ADR lines. Source material is not committed:
- an ADR-by-ADR audit (`.samples/adr-audit/REPORT.md`, ADR-0001 to 0009);
- its cross-check (`.samples/prereview-adr-audit.md`);
- the corrections table (`.samples/r31-adr-corrections.md`);
- a post-merge integration review (`.samples/review-postmerge-1.md`);
- a startup and defaults review (`.samples/review-startup/FINDINGS.md`).

| PR | Item | Merge |
|---|---|---|
| #213 | F42: the agent ran unfenced without a NetworkPolicy | b90d1db |
| #214 | F38: an unapproved re-plan was implemented (stale approval on re-plan) | daf4cd6 |
| #215 | F35: restore re-ran on any spec edit (one-shot keyed on generation) | 628fb1a |
| #216 | F36: VolumeSnapshot label write used Update (Forbidden) and restore progress defects | 6eb74c2 |

**Status:**
- **All seven R30 I103 P1s are merged.** R30's I103 is ticked on its branch.
- **The ADRs no longer describe the system.** 31 claims in ADR-0004 to 0008
  are broken, not built or superseded (I107). Eight carry a security
  consequence: two are queued (F53(a), the writable verify checkout; F54,
  the evil-agent e2e), row 16's is closed by F42, and the rest are new
  F-items under I107.
- **F42 widened an existing race, and `make deploy` ships an operator that
  can't run an agent.** Both are queued (F55, F56); see I108.
- **No security gate is off by default.** Missing KubeArmor, snapshot CRDs or
  the CNI probe namespace each fail closed per Loop (the startup review).
- **F36 had no kind lifecycle run:** neither lab cluster has a CSI
  snapshotter (I109, D82).
- **The pre-review layer caught four of the reviewer's own mistakes.** The
  read-only builder now pre-reviews every PR (I110).

---

## P2

- [ ] **I107: the ADRs describe behaviour the code doesn't have**

  **Where:** ADR-0004 to ADR-0008, including ADR-0007's amendment A1. ADR-0009
  has no broken rows. The cited lines are ADR lines.

  **Problem:** one line per claim, as the claim, then what the code does.
  Each line is tagged: `[BROKEN]` (the ADR is wrong), `[NOT BUILT]` (decide:
  build it or drop it), `[SUPERSEDED]`, and `[sec]` where there is a
  security consequence. The number is the row in the corrections table.

  ADR-0004:
  - 1 `0004:20-22` [BROKEN]: `result.json` carries `needs_input` → `Result` has
    no `NeedsInput` (`runner/runner.go:123-129`). `CONTEXT.md:69` repeats it.
  - 2 `0004:23` (and `0005:121-122`, `:127-128`) [BROKEN]: the operator reads `result.json`
    via `kubectl exec cat` → it reads the agent's termination message
    (`loop_s4_phase.go:313`).
  - 3 `0004:63-65` (and `0005:319-322`) [NOT BUILT]: the operator gains
    `pods/exec` → never granted; the pods marker has no exec
    (`loop_controller.go:549`).

  ADR-0005:
  - 4 `0005:117-118` (also `:213-214`) [BROKEN]: a pod error is `VerifyError`,
    re-run once → the reason is `VerifyInfraFailed`, bound 2
    (`loop_verify_job.go:1620,1626`). `CONTEXT.md:57` repeats it.
  - 5 `0005:212` [NOT BUILT]: the verify Job sets `activeDeadlineSeconds` →
    absent from `loop_verify_job.go`, while the deliver and memory-writer Jobs
    set it. **Queued as F53(b).**
  - 6 `0005:163-168` [NOT BUILT] [sec]: the verify Job has resource limits →
    absent (`loop_verify_job.go:699-723`), and the Job runs agent-authored code.
  - 7 `0005:95-100` (D19) [BROKEN] [sec]: the checkout is read-only in every
    check container → the `verify` emptyDir is mounted without `ReadOnly`
    (`loop_verify_job.go:713-714`). `ReadOnlyRootFilesystem` covers the root
    fs only, so agent code run by check N can rewrite what check N+1 builds
    from. **Queued as F53(a).**
  - 8 `0005:196-197` (and `:319-322`) [NOT BUILT]: a `pods/log` grant → none;
    failure text is the termination message (`loop_verify_job.go:1561`).
  - 9 `0005:271-272` (and `:319-322`) [NOT BUILT]: RBAC adds `secrets/get` →
    none; the git credential is a volume in `clone-base` only
    (`loop_verify_job.go:606`).
  - 10 `0005:266-272` (D15) [NOT BUILT]: refs resolved in-process with go-git →
    no go-git; the base SHA comes from the init container's termination
    message (`loop_controller.go:1140`, `readBaseCommitFromTerminationMessage` at `:1152`).
  - 11 `0005:154-156` (D11) [BROKEN]: the operator resolves the branch head
    itself → the pin is the runner's claim (`loop_s4_phase.go:567`). The
    property holds: the pinned SHA is what verify checks out and deliver
    pushes. The mechanism differs, and the ancestry check is F43.
  - 12 `0005:240-247` (D17.1) [NOT BUILT] [sec]: a canary test that must fail
    → absent.
  - 13 `0005:248-255` (D17.2) [NOT BUILT] [sec]: an advisory static scan →
    absent, and a doc comment says it exists (`loop_verify_job.go:47-53`).
  - 14 `0005:156`, `:331-335` [BROKEN]: `history[]` gains `verifiedCommit` →
    there is no `history[]`; the built fields are at `loop_types.go:286,393`.

  ADR-0006:
  - 15 `0006:139-141` [BROKEN]: publish after each iteration → delivery runs
    once, on terminal Succeeded (`loop_deliver_job.go:707-711`). That is
    intended: the Loop is one-shot (ADR-0003).
  - 16 `0006:68-73` [BROKEN] [sec]: egress only from the proxy container → the
    NetworkPolicy is pod-scoped, and agent egress covers the model proxy, the
    egress proxy, DNS, the repo and each tool proxy
    (`loop_controller.go:4213-4274`; `modelProxyEgress` at `:4153`).
  - 17 `0006:53-58` [SUPERSEDED]: a sidecar model proxy → a separate pod (D33,
    `loop_controller.go:2410`).
  - 18 `0006:87`, `:147-148` [NOT BUILT]: defaults from a ConfigMap → none:
    the agent fields have no `+kubebuilder:default` markers; the Loop sets them.
  - 19 `0006:91-99` [NOT BUILT] [sec]: the "evil agent" e2e → absent.
    **Queued as F54.**

  ADR-0007:
  - 20 `0007:54-55` [NOT BUILT]: `ClusterAgentPolicy` → absent. Owner decision (D81).
  - 21 `0007:58-59`, `:255`, `:180-181` [NOT BUILT]: sample `AgentPolicy` YAML
    in `config/samples/policies/` → absent.
  - 22 `0007:113-115`, `:249` [NOT BUILT]: `status.policy.blockedCount` and
    `PolicyBlocked` → the field exists (`loop_types.go:418`), and nothing
    writes it.
  - 23 `0007:250-254` (I32) [NOT BUILT]: `blockedCount` from the engine's
    alert stream → no relay.
  - 24 `0007:97-99` (Q4) [NOT BUILT]: audit records carry `iteration` → they
    carry none (`internal/egress/audit.go:7-12`).
  - 25 `0007:92-93` (Q4) [NOT BUILT] [sec]: the eBPF engine is an audit source
    → no producer, so a blocked attempt is not audited.
  - 26 `0007:131-149` (D29, C5) [BROKEN] [sec]: per-container egress enforced
    by eBPF → the engine policy is a pod-level union; the fence is the D34
    NetworkPolicy, which F42 now guarantees exists.
  - 27 `0007:170-184` (D31) [NOT BUILT]: learn mode and `cox policy suggest`
    → only an unused `ReasonAuditOnly` constant (`internal/engine/enforcer.go:71`).
    Owner decision (D81).
  - 28 `0007:196-198` (D32) [NOT BUILT]: each `history[]` entry records the
    policy hash → the hash is current-value only (`loop_types.go:400-405`).
  - Also: `0007:562-575` says the egress proxy's KubeArmor policy "will" be
    added. It already is (`kubearmor_enforcer.go:143-153`).

  ADR-0007 amendment A1:
  - 29 `a1:142-166` [BROKEN]: a missing fact or an outage reads `Unknown` →
    only `EnforcementUnverified` maps to `Unknown`; `EngineUnavailable` is
    `False` (`loop_controller.go:5198`, `:5209-5210`). It still fails closed.
  - 30 `a1:208-215` [NOT BUILT]: the optional post-start node re-check → absent.

  ADR-0008:
  - 31 `0008:253-265` [NOT BUILT]: a `ToolProxy` kind → absent, as the ADR says.

  **Fix:**
  - **Amend the ADR text** (an amendment section, as ADR-0007 has):
    rows 1-4, 8-11, 14-18, 24, 26, 28, 29 and 30, plus the `0007:562-575`
    line. Fix `CONTEXT.md:57` and `:69` in the same pass.
  - **Build:**
    - rows 5 and 7 are F53;
    - row 19 is F54;
    - rows 6, 12, 13, 21, 22, 23 and 25 are new F-items. Row 13's false doc
      comment is removed now, whatever is decided about building the scan.
  - **Owner decision:** rows 20 and 27 (D81).
  - **No action:** row 31.

  **Acceptance:** each row is resolved by an ADR amendment, a merged F-item,
  or a recorded owner decision.

- [ ] **I108: F42 widened a readiness race, and `make deploy` cannot run an agent**

  **Where / Problem:**
  - **F55:** the sandbox gate waits for the egress proxy only when
    `needsEgressProxy` (`loop_controller.go:2038`), but the agent
    NetworkPolicy sends the workspace clone through the egress proxy whenever
    `workspaceInitNeedsProxy` holds (`:4248`). Before F42, a Loop with an
    external repo, no model endpoint and no allows had no NetworkPolicy and
    cloned directly. Now it can start before the proxy is Ready, and the
    clone fails. That's the shipped sample's shape. Separately, on upgrade the
    new gate reads the cache right after creating the NetworkPolicy, so it can
    suspend a running sandbox for a reconcile.
  - **F56:** `config/manager/manager.yaml:64-71` passes no `--runner-image` or
    proxy image flags. A `make deploy` install (`README.md:39` recommends it
    for production) runs `sleep infinity` as the agent, with no condition
    saying so, and its three proxy pods can't pull their images.

  **Fix:** F55 and F56 in the queue.

  **Acceptance:** both merged with their specs and kind evidence.

## P3

- [ ] **I109: verification gaps the lab can't close yet**

  **Where / Problem:**
  - F36 changed no RBAC: the role already granted `patch` and never
    `update`; the code called `Update`. Its new code path uses `patch
    volumesnapshots`, and AGENTS.md's B1 rule asks for the kind lifecycle run
    that would have caught the original `Update`. Neither lab cluster has a CSI snapshotter: hermes-sim has
    no `volumesnapshots` CRD, and ref has no `VolumeSnapshotClass`. So a fork
    and source-delete run is impossible. The merge rests on an envtest RBAC
    wrapper (patch allowed, update denied) and `can-i` on ref (patch yes,
    update no).
  - Two older items are open (pi3's pre-review of #204):
    - I41 (R14) meets one of four acceptance bullets.
    - I40 (R12) needs the CI checks made required on `main`. Today
      `required_status_checks` is null, so a red test doesn't block a merge.
      That's an owner setting.

  **Fix:** D82 (a snapshotter on a lab cluster). I41's remaining bullets as an
  F-item. I40: the owner sets the required checks.

  **Acceptance:** an F36-class kind run exists, I41 is ticked, and branch
  protection lists `test` and `lint` as required.

- [ ] **I110: process: the pre-review layer, and the slips it caught**

  **Problem:** this round the read-only builder pre-reviewed every PR, and
  caught the reviewer four times:
  - #210: a "deleted test" that wasn't deleted;
  - #211: six citation and status errors in R30 itself;
  - #204: two wrong ticks, I41 and I40;
  - #205: a missing D37 "Views" row that the reviewer had passed.

  Builder slips:
  - #216 was marked ready with a WIP body and no verification recorded.
  - The read-only builder executed generated scripts once, while proving
    F37 (its own report, `.samples/review-gates-delivery/FINDINGS.md`, P1-2).
  - Builder #1 looped twice on code-heavy work and was moved to writing tasks
    (relevant to D72).

  **Fix:** keep the pre-review step on every PR. The reviewer re-runs every
  named mutation by exact string replacement, checked with `git diff`. A PR
  is not reviewed while its body says WIP.

  **Acceptance:** the next round has no reviewer-passed item reopened by a
  later check.

---

## Owner decisions

D72 to D80 are carried from R29 and R30, unanswered. A one-page brief with
copyable replies is in `.samples/owner-decision-brief.md`.

- **D81: build or drop two ADR-0007 features.**
  - `ClusterAgentPolicy` (row 20).
  - Learn mode with `cox policy suggest` (row 27, D31).
  - Options:
    - (a) mark both deferred in the ADR;
    - (b) plan them as slices.
  - **Recommendation: (a).** Neither is needed for a single-team install, and
    both widen the policy surface.
- **D82: a CSI snapshotter on a lab cluster.**
  - Snapshot RBAC and restore fixes (F36, F35, and F57 to come) can't be
    lifecycle-tested on either lab cluster.
  - Options:
    - (a) add the csi-hostpath driver and a `VolumeSnapshotClass` to kind-ref,
      with the owner's OK, as for any lab cluster change;
    - (b) accept envtest plus `can-i` for this class.
  - **Recommendation: (a).** It's the class that hid F36's Forbidden write.
