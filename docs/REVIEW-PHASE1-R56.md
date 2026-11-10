# Phase 1 review, round 56: v0.2.0 is proven on kind; arm64 waits on one workflow push

**Scope:** main 9753f8a (R55's scope end) → a3c5324, 11-10 03:36Z to 16:44Z. Merged in this window:

| PR | What | Squash |
|---|---|---|
| #471 | I268: the Judge's unavailable cause keeps the error, not the URL | `bb4c0b8` |
| #472 | I265: a structural test that the proxy admits every caller | `b40ed9d` |
| #470 | R55 (owner merge and tag `review/phase1-r55`) | `338f38c` |
| #473 | C1: multi-arch release images, without the workflow change (owner merge) | `92afc0d` |
| #474 | R1: v0.2.0 notes cover the Judge and the v0.1.3 delivery defect | `c0db1e3` |
| #475 | I263c: a pod-script change on a shared volume needs a kind run | `a3c5324` |

## Closed

R55's boxes are ticked in `docs/REVIEW-PHASE1-R55.md` in this commit, each with its evidence:

- **I263** (v0.1.3 shipped with delivery broken):
  - (a) The R1 smoke ran three `spec.delivery.mode: PullRequest` Loops. Two showed `Delivered=True` with a PR number: r1-across PR 4 and r1-smoke PR 5 (`.samples/r1-v020/lab-run18.log:126,:155`).
  - The third, r1-v013, ran wholly on v0.1.3 built from its tag as a control. It failed with `fatal: detected dubious ownership in repository at '/deliver'` and `Delivered=False DeliveryFailed` (`.samples/r1-v020/r1-v013-deliver.txt`). This shows the defect on the real release, not only on main before F95.
  - (b) `c0db1e3`: "Known defect in v0.1.3: PR delivery fails on kind (F67)".
  - (c) `a3c5324`: the AGENTS.md test norm.
- **I264** (two Loops, one PR): `9753f8a`. The acceptance holds: each R1 delivery logged `deliver push: created PR N for <its own branch>`, and N matched `status.delivery.prNumber` (lab-run18.log:126 PR 4, :155 PR 5). The cause on Gitea before F96 is still **not shown**, and stays so (D48: the reviewer does not read the Gitea API).
- **I265** (the proxy ingress): `b40ed9d`. The reviewer's mutation MC (the agent peer dropped) failed the spec at `loop_i265_proxy_ingress_callers_test.go:206`.
- **I266** (the J1d script's three runs): run 18 printed what each Loop must show and the plan lines it proves (docs/PLAN-PHASE7.md:37-38) before it touched the cluster (lab-run18.log:4-12). It needed one run.
- **I267** (a missed report): every builder report in this window fired its watcher within one 30-second interval: b4 r1notes, b4 r1notes-fix and pi3 i263c.
- **I268** (the cut cause): `bb4c0b8`.

## R1: v0.2.0 on kind-ref (lab run 18, 338f38c)

**Where:** the scratchpad `lab-run18-r1.sh`. The evidence is in `.samples/r1-v020/`.

| Loop | Operator | Result |
|---|---|---|
| r1-v013 (control) | v0.1.3 from the tag | Succeeded, `Delivered=False DeliveryFailed`, "dubious ownership" |
| r1-across | v0.1.3, upgraded to 338f38c mid-run (Planning before, :72; Implementing after, :88) | Succeeded, `Delivered=True`, PR 4 |
| r1-smoke | 338f38c, `spec.verify.judge` on, the sample's model fields set | `JudgeVerdict=True` score 100, `Delivered=True`, PR 5 |

- **Upgrade:** the v0.1.3 CRD had no judge field (:18); after the upgrade it has one (:78). The status VAP `coxswain-deny-loop-status-writes` was applied (:158).
- **RBAC:** `can-i` is yes for every read the new code makes. The candidate's operator log has `forbidden=0` (`control=1`) over 13277 lines (:165).
- **Restore:** kind-ref is back on `f54main` with its old args, the status VAP is removed (it was absent before), and 0 Loops are left (:181 onward).
- **arm64 (C1):** the operator, runner, model-proxy, egress-proxy and tool-proxy images all cross-built for `linux/arm64` on the devbox from the C1 branch. The operator and runner binaries are `ELF 64-bit LSB executable, ARM aarch64`. The release manifests are not shown: `release.yml` is still amd64-only on main (see D129).

## P2

- [ ] **I269: the reviewer stated a cause it had not checked: "the script took its evidence before v0.1.3's delivery finished"**

  **Where:** the reviewer's messages to the owner, its log, and the first version of its R1 evidence comment on #474 (11-10 ~16:21Z to 16:35Z).

  **Problem:**
  - **The claim:** the reviewer saw `container "push" ... is waiting to start: PodInitializing` in the r1-v013 block. It said the script had snapshotted before delivery finished, and logged that as its own slip.
  - **What was true:** the same block already held `Delivered=False DeliveryFailed` and the import-agent "dubious ownership" lines (lab-run18.log, under "evidence before upgrade"). The push container never started because import-agent had failed. Both v0.1.3 and main hold Succeeded until `Delivered` is terminal (P5f, `a84b7c9`, `settleDeliveryHold` in `internal/controller/loop_deliver_job.go`).
  - **The rule it broke:** R25 I84 and I76: a cause is stated only when shown. The extra hand capture (`r1-v013-deliver.txt`) did no harm, but the claim reached the owner.

  **Fix (done):** the #474 comment carries a correction, the log has a CORRECTION line, and the R1 evidence stands as captured.

  **Acceptance:** before the reviewer reports a cause, including its own fault, it quotes the line that shows it.

## P3

- [ ] **I270: the reviewer's slips in this window**

  **Where:** the reviewer's brief for #474, and one diff command.

  **Problem:**
  - **A wrong API path in a brief:** the brief named `spec.judge`, but the field is `spec.verify.judge` (`api/v1alpha1/loop_types.go:235,:368`). b4 copied it into the notes four times. The review caught it before merge (#474, aa084d7 → 4838869).
  - **FETCH_HEAD:** one read-only `git diff` against FETCH_HEAD, against the reviewer's own rule. It was redone against a named ref.
  - **A count that could not reach its target:** the fix brief asked for `grep -c` ≥ 5. That counts lines, and the field sits on 3 lines. b4 reported 3. The reviewer then counted matches (5) itself.

  **Fix:** a brief that names an API field, a flag or a path takes it from a grep of the source, pasted into the brief (the I68 rule, applied to the reviewer). Counts in briefs say whether they count lines or matches.

  **Acceptance:** none of these in R57's window.

## Housekeeping

- **PLAN.md:132** ("Check the CNCF landscape for an existing 'Coxswain' entry") is ticked in this commit.
  - Checked `cncf/landscape` `landscape.yml` at `278d59082b41d2453a7717532162d24def1ffdef` (11-10 00:21Z), 1162761 bytes.
  - `grep -i coxswain` matches 0 lines. Positive control: `name: Kubernetes` matches 2.
- **#472 note:** a future Job that calls the model proxy is caught by the I265 spec only once its `ensure…NetworkPolicy` call is added there. The `HaveLen(3)` positive control also fails if the first Reconcile creates a fourth caller. A new caller's slice adds its ensure call to that spec.

## Owner decisions

**Answered in this window:**

- **D128 = (a)** (owner, 11-10): the v0.1.3 delivery fix ships in v0.2.0, whose notes name the defect (`c0db1e3`). There is no v0.1.4.
- **R55:** merged and tagged `review/phase1-r55` on `338f38c`.

**Open:**

- **D129: tag v0.2.0 with or without arm64.**
  - (a) Push the `release.yml` commit first (`3accc27`, branch `c1-workflow` in `../coxswain-c1s`, 4 lines: a QEMU step and `platforms: linux/amd64,linux/arm64`), then tag. The reviewer opens the PR and merges it once its CI is green.
  - (b) Tag now, amd64 only. Then the notes' Summary line "Multi-arch images (C1) … pending the release workflow push" is the true state, and arm64 comes in v0.2.1.

  **Recommendation: (a).** The images are proven to cross-build, and the change is 4 lines. Then the Summary line drops "pending" in a one-line notes PR before the tag.
- **D130: what comes after Phase 7.** PLAN.md ends at Phase 7, and the v0.2.0 tag completes it:
  - J2 is not needed (D122 = advisory);
  - K1 is dropped (D125);
  - the deferred items (docs/PLAN-PHASE7.md:87-91) have no slice.

  The reviewer needs a direction for the next plan: harden what exists, or one of the deferred items. **No recommendation** until the owner says what v0.2.0 is for.

## What I did not verify

- The PR titles on Gitea (D48: no credentials), as in R55.
- The cause of I264 before F96.
- The Judge on arm64: no arm64 cluster.
- The release workflow itself with `platforms:`. It runs only on a tag push.
