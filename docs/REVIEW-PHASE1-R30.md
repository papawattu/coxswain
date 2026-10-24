# Phase 1 review, round 30: a whole-codebase read-only review finds P1s in released code

Round 30, 2026-10-24. It covers the work merged after the R29 doc was drafted (the tag `review/phase1-r29` is on #200, which landed after #201-#208), plus what merged since. Line numbers are against main at `04472d5`, the tree the reviews read; F37 and F39 have since moved some of them. The owner asked for all
three builder sessions to be kept busy. The reviewer used the third (read-only)
session for eleven focused, read-only reviews of the released code, then
verified each finding before queueing it. The reviews are in `.samples/review-*/FINDINGS.md` and
indexed in `.samples/review-index.md` (not committed). Every P1 below was
checked by the reviewer against the code, and the one marked *reproduced*
was run.

| PR | Item | Merge |
|---|---|---|
| #201 | I99: join wrapped LESSON lines; drop over-cap lessons | cfffcc2 |
| #202 | I101: `cox memory delete` and the memory PVC lifecycle | 8a5940b |
| #203 | I101: delete refuses while a terminal Loop's write is pending | 04472d5 |
| #206 | F30: memory-write metrics (`PLAN.md:98`) | dcd79da |
| #207 | F37: `shellQuote` escaped an apostrophe wrongly (command injection) | 7dcf262 |
| #208 | Docs: fix drift found by the docs-vs-code audit | 2a3208f |
| #209 | Docs: fix drift in samples, examples and chart docs | 9178e51 |
| #210 | F39: tamper check passed all globs as one pathspec and failed open | dc628a5 |

**Closed in this round** (ticked in their own docs): I99 (cfffcc2), I101
(8a5940b, 04472d5).

**Status:**
- **Seven P1s were found in released code** (I103). Two (F37, F39) are
  merged, F40 is in review, and the rest are queued for builder #2 in order of risk.
- **The reviewer missed one of them earlier.** The D71 memory cap was decided
  but never built, and the Phase 5 acceptance did not check for it (I104).
- **The specs hid two of the P1s** (I105).
- **D72, D73 and D74 are still unanswered.** R29 merged without answers; they
  carry over below.

---

## P1

- [ ] **I103: security and integrity defects in released code**

  **Where / Problem:** each row was confirmed by the reviewer in the code.
  The row marked *reproduced* was also run.

  | F-item | Defect | Where | State |
  |---|---|---|---|
  | F37 | `shellQuote` wrote `'` as `'''`: an apostrophe broke the script, and two injected commands. It reached the deliver Job, which holds the git credential, via `PR_TITLE` from `spec.goal`. Wrong since S3 (e7148d4). | `loop_controller.go:5540` | merged 7dcf262 |
  | F39 | The tamper check is a no-op with 2+ globs, including the default `go` preset: the globs were joined into ONE newline-separated pathspec. A failing `git diff` also read as clean. *Reproduced* with real git. | `loop_verify_job.go:522,636,644` | merged dc628a5 (#210) |
  | F40 | The egress proxy's plain-HTTP client follows redirects, and the dial re-checks nothing, so the allowlist and SSRF carve-outs are bypassed. | `cmd/egress-proxy/main.go:371-375,396-416` | PR #212 (Verdict OK) |
  | F42 | A Loop with no `endpointSecretRef` and no policy network allows gets **no** NetworkPolicy: its agent has open ingress and egress. A missing NetworkPolicy also does not hold the sandbox Suspended. | `loop_controller.go:5266-5270`, `:4596-4619` | queued |
  | F38 | Manual approval + `onVerifyFail: Plan`: the re-plan keeps the stale `PlanApproved`, and the gate reads it before it is cleared, so an unapproved re-plan is implemented and delivered. The B4 spec asserts the bug. | `loop_s4_phase.go:898,922`; `loop_approval.go:491-505` | queued |
  | F35 | The rollback one-shot is keyed on `metadata.generation`, so any spec edit re-runs the restore. `cox pause` patches `spec.suspend` (`internal/cli/pause.go:52`), so pausing after a rollback wipes all work since. `restoreFrom` is documented immutable but has no CEL rule. | `loop_restore.go:222-228,618-630` | queued |
  | F36 | The fork-retention label is written with `Update`, but RBAC grants only `patch` on volumesnapshots. It is Forbidden on every real cluster, logged at V(1), so the source's GC can delete a snapshot a fork is restoring from. | `loop_controller.go:548`; `loop_restore.go:1016,1053` | queued |

  **Fix:** one PR per F-item. Each needs: an execution test for any changed
  script; a named mutation that makes a spec fail; and kind evidence on an
  enforcing cluster (kind-ref) wherever the fix touches RBAC, NetworkPolicy
  or the sandbox pod.

  **Acceptance:** every row merged, and the reviewer has re-run each named
  mutation. Then a patch release: the owner decides the tag.

## P2

- [ ] **I104: the D71 memory cap was decided and never built, and the reviewer's
  Phase 5 acceptance missed it**

  **Where:** `docs/PLAN-PHASE5.md:419-424` and `:479` (D71 (a): evict the
  oldest; "enforced by the writer Job (P5b) with a spec").
  `cmd/memory-writer/main.go:455-500` has no cap and no eviction.

  **Problem:** memory grows without bound. Past about 28 lessons, the runner
  silently drops the WHOLE memory section from the prompt
  (`runner/phase.go:1315-1319`, 16 KiB). So on a busy repo the feature quietly
  stops working. R29 accepted Phase 5 without checking every owner decision
  in the plan against the code. That is the reviewer's miss, the same class
  as F7 in R28.

  **Fix:** F32 (the cap and eviction, the 16 KiB truncation made visible,
  and a reader per-file cap ≥ the writer's). Reviewer process: before
  accepting a phase, check every decided D-item in its plan against the
  code, with a grep or a spec citation for each.

  **Acceptance:** F32 merged with a spec in which a 101st lesson evicts the
  oldest. The next phase acceptance lists each D-item with its code citation.

- [ ] **I105: specs that cannot fail, or that assert the bug**

  **Where:** `.samples/review-tests/FINDINGS.md` (25 gate mutations judged).

  **Problem:**
  - 22 of 25 gate mutations would be caught. The three misses:
    - the tamper script had no execution test (F39);
    - the Manual-approval sandbox hold has no spec (`loop_approval.go:520-535`);
    - the deliver "no pin" branch has no spec (`loop_deliver_job.go:655`).
  - Separately, `loop_b4_replan_envtest_test.go:415-431` codifies F38's
    bug: its own comment calls the advance to Implementing the normal path,
    and it asserts only that `PlanApproved` is absent.
  - The AGENTS.md norm ("every gate spec must FAIL when the gate is
    disabled") was being met per PR, but nothing checks it across the codebase.

  **Fix:** F39, F38 and F49.

  **Acceptance:** each named mutation recorded in its PR, and re-run by the reviewer.

## P3

- [ ] **I106: drift between docs/samples and the code**

  **Where:** `.samples/doc-drift/REPORT.md`, `.samples/doc-drift-2/REPORT.md`.

  **Problem:** the user docs named a failure reason, a CLI form, Make targets,
  a flag, a finalizer and line numbers that do not exist (fixed in #208,
  #209). Code gaps remain:
  - `ModelTimeout` is documented but never wired (F46).
  - All three task samples default to a repo with none of the files they
    test (F50).
  - The chart has dead values and an inert second admission binding (F45, F50).

  **Fix:** F45, F46, F50.

  **Acceptance:** those merged.

---

## Owner decisions

The reviewer recommends; the owner decides. D72–D74 are carried from R29,
unanswered.

- **D72 (from R29): builder #1's role.**
  - Recommendation: (b) docs and evidence only.
  - Today's evidence fits (b): its docs work (an audit of every unticked box
    in REVIEW-PHASE0 to R27, two docs drift audits, ADR-0010) was useful.
  - But it looped once and degraded near 80% context twice, so it now resets
    at 70%.
- **D73 (from R29): curator re-screening.**
  - Recommendation: (a) for import, (b) for re-screening.
- **D74 (from R29): housekeeping.**
  - Recommendation: delete all three.
- **D75: should Loop editors keep write access to `loops/status`?**
  - **Background:** B3 granted `update`/`patch` on `loops/status` to
    `loop-editor-role` (`config/rbac/loop_editor_role.yaml:31-43`) so that
    `cox approve`/`reject` work. That contradicts ADR-0005 D12 as written at
    `api/v1alpha1/loop_types.go:276-279` ("only the manager role may write
    loops/status").
  - **Problem:** an editor can forge verify evidence (`status.verify`), and
    can make the operator delete or restore from any snapshot it names in
    `status.checkpoints`. F47 closes the second part regardless.
  - **Options:**
    - (a) A ValidatingAdmissionPolicy that lets non-operator users change
      only the two approval conditions.
    - (b) Keep the grant, and validate everything the operator reads from
      status.
  - **Recommendation: (a).** Editors are not the trust boundary the gates assume.
- **D76: the operator mounts any Secret a Loop author names**
  (`spec.workspace.gitCredentialSecret`, `spec.agent.endpointSecretRef`) and
  presents it to a host the same author names.
  - **Problem:** that is a confused deputy for users who can create Loops but
    cannot read Secrets.
  - **Options:**
    - (a) Require a label on the Secret (e.g. `coxswain.io/usable: "true"`).
    - (b) Document that Loop-create implies Secret-read in the namespace.
  - **Recommendation: (a).**
- **D77: budgets when the usage reading is missing or stale.**
  - **Problem:** an unmetered response, or a failed or stale usage read,
    silently skips the token and cost caps (`.samples/review-budget/` P2-1,
    P2-2). F44 makes it visible.
  - **Options:**
    - (a) Surface only.
    - (b) Also hold the Loop until a fresh reading.
  - **Recommendation: (b) for `maxCost`, (a) for `maxTokens`.**
- **D78: short colon-form credentials in lessons.**
  - **Problem:** `password: hunter2` (7 characters) passes the static screen
    (`pkg/memorywrite/policy.go:121`, `{8,}`). The 8-character minimum is
    there to keep prose such as "the token: parser" eligible.
  - **Options:**
    - (a) Keep it.
    - (b) Drop any `credkey: value` with no space in the value.
  - **Recommendation: (b).** The curator is the second gate, not the first.
- **D79: stall and wall-clock semantics.**
  - **Problem:**
    - The stall detector hashes the check output, which the agent
      influences, so varying the failure text dodges it. `maxIterations` is
      the only bound.
    - The wall clock runs while a Manual Loop waits at `AwaitingApproval`.
  - **Options:**
    - (a) Leave both.
    - (b) Also stall on N iterations with no diff to protected-path-excluded
      files, and pause the wall clock during `AwaitingApproval`.
  - **Recommendation: (b).**
- **D80: CNI evidence is from one probe pod on one node.**
  - **Problem:** the verdict is cluster-wide, and a node that joins later is
    never sampled (`.samples/review-cni/` P2-1).
  - **Options:**
    - (a) Accept it, documented.
    - (b) Probe per node and gate a sandbox on its own node's result.
  - **Recommendation: (a) now, (b) when multi-node clusters are a target.**

## Still open from earlier rounds (no change)

- OS5's stable Event reasons `GateHeld`, `GateReleased` and `VerifyFailed`
  do not exist (`docs/REVIEW-PHASE1-R19.md:91`).
- D37 (the flow console) and D42/D43 (fan-out, multi-tenancy) are approved
  in direction but not planned.
- I62 (the CI cache key and the e2e runner) needs a workflow edit, which is
  an owner push.
- D36's ADR is PR #205, waiting for the owner.
