# Phase 1 review, round 37: a claim that does not exist, a shared ref that is not safe, and a length counted in the wrong unit

Round 37, 2026-10-25. It follows R36 (PR #272, not merged) and covers the work
merged since R36 was cut: #273, #274 and #275. Code line numbers are against
main at `347c07c` unless an item says otherwise; #275's citations are against
its head `e83bd21`, which merged unchanged as `8b55779`. The review was built from these input files, which are not committed:
- the input skeleton for this round (`.samples/r37-skeleton.md`);
- the pre-reviews of #273, #274 and #275 (`.samples/prereview-273.md`,
  `-274.md`, `-275.md`);
- the post-merge reviews of #273 and #268 (`.samples/review-postmerge-12.md`)
  and of #274 (`.samples/review-postmerge-13.md`);
- the designs the three PRs followed (`.samples/design-f48h.md`,
  `design-f83d.md`, `design-f89b.md`);
- the reviewer's mutation records (`.samples/mut-273.md`, `-274.md`);
- R36 (`docs/REVIEW-PHASE1-R36.md`, branch `review-phase1-r36`) and the queue
  (`…/scratchpad/queue-b2-after-f30.md`).

A bare `loop_*.go` or `loop_*_test.go` name is under `internal/controller/`;
`describe.go` is under `internal/cli/`.

| PR | Item | Merge |
|---|---|---|
| #273 | F48h: the A2 empty fallback ends the episode; the run-key comments say what the key is | 5d44419 |
| #274 | F83d: the tamper record's last untruths — docs, hold window, lifecycle, OOMKilled surface | 347c07c |
| #275 | F89b: an unstorable kubelet reason, and the guard's blind spot in the comparator | 8b55779 |

**Status:**
- **No P1s and no P2s.** Two merges are the reviewed trees, byte for byte
  (`git rev-parse 5d44419^{tree} aeac42d^{tree}` → `cb28024a…` for both;
  `347c07c^{tree}` = `7066add^{tree}` = `8721faa2…`), and #275's code does what its
  body says; its own evidence notes are on the PR. Three new items, all P3.
- **#273 closed R36 I134.** The A2 empty fallback now ends the episode
  (`loop_restore.go:1089`), and the run-key comment is the one #274's window
  needed. Its four P3s were routed to F48e by the PR's review notes; R37 takes
  one of them back (I138), because it is untrue on main.
- **#274 closed its three P2s.** The per-pin clear is one shared helper
  (`clearTamperRecord`, `loop_verify_job.go:1336`), called at the advance
  (`loop_s4_phase.go:590`) and at the pin clear (`loop_verify_job.go:1541`), so
  the two sites are visibly one decision. One P3 is carried: the `f83-s3` title
  still claims an assertion the clear made unobservable, and F83e's queue line
  does not name it.
- **#275 closes R36 I133 and I135 — and I135's premise was wrong.** Both halves
  of I133 are fixed at the source: `sandboxFailureReason` is called in the
  no-container-status arm (`loop_s4_phase.go:1539` on `e83bd21`) and the
  terminated arm (`:1596`), and the pattern it checks is character-identical to
  all four rendered CRD copies. For I135, the defect as R36 wrote it cannot
  occur through any prober: `HoldsSuspended()` returns true for an empty reason
  (`internal/cni/cni.go:80-87`) and `cniNetworkStatus`'s reason switch then
  returns the constant `ReasonCNIUnenforced` (`loop_controller.go:1318-1324`), so
  the empty reason never reaches the comparator. The queue's spec for I135
  ("a stubbed `Prober` returning `""`", queue `:178`) therefore cannot be
  written, and the proof is a comparator unit spec with a positive control
  (`loop_f89b_unstorable_reason_envtest_test.go:201-229` on `e83bd21`). I135
  should be closed on that spec, with the acceptance corrected.
- **Items:** I138 to I140 are new. I133-I137 and D72-D87 stay open, and their
  state is in the bullets below. D87 is now the round's real cost: two fixed
  items and one merging item, and not one of them can be ticked.
- **PR-level notes** from the pre-review of #275 (four body/evidence notes, no
  code change) live in `.samples/prereview-275.md:§3`; they belong on the PR,
  not in this doc.

**Carried items, state at this cut:**
- I133 (an unstorable reason wedges the write): **fixed by #275** (`8b55779`);
  the tick needs an open review doc (D87).
- I134 (the A2 fallback keeps the count): **fixed by `5d44419`**; tick blocked by
  D87 — the same position R36's I129 was in at R36's cut.
- I135 (the guard is invisible to the comparator): **fixed by #275** (`8b55779`), on a
  substituted proof (see above). Close it with the correction.
- I136 (the UPGRADE surface): **open**. The release notes are current, including
  an F89b line for #275 (`docs/RELEASE-NOTES-next.md:234`, branch
  `origin/docs/release-notes-next`), so the missing surface is still UPGRADE
  only.
- I137 (a two-site mutation cannot say which site a spec proves): **open**, and
  now with three more data points: #273 and #274 each ran per-site mutations
  (`mut-273.md:1-3`, `mut-274.md`), and #275 is a partial — it mutated one of
  its two `normalizeReason` sites (`loop_controller.go:1339` on `e83bd21`) and
  left the second (`:1360`) to the spec's assertion
  (`loop_f89b_unstorable_reason_envtest_test.go:228`). By reading the assertion
  is load-bearing; no run says so.
- F48e (four P3s + the UPGRADE line) and F90 (docs) — **open**, queued
  (`:174`, `:179`, `:182`). F83e (the renderer's unguarded reads and the dead
  `Tampered` field) — **queued at `:185`**, and the queue line does not name the
  carried `f83-s3` title.
- D72-D86: unchanged in this window.

---

## P3

- [ ] **I138: the run-key comment explains a real behaviour with a claim that does not exist**

  **Where:** `loop_s4_phase.go:1343-1345` — "the rejection re-plan … is ended by
  the claim clear in `advancePhaseFromClaim` (the **PlanApproved claim** that
  produced the plan cleared it first)"; the approval channel
  (`loop_approval.go:19-20`, "the user (via cox approve / cox reject) is the sole
  actor on the two approval conditions (PlanApproved, PlanRejected)"; `:25`, the
  user sets `PlanApproved`); `PlanApproved` as a condition type
  (`api/v1alpha1/loop_types.go:1302`); the clear the comment means
  (`loop_s4_phase.go:948`, inside `advancePhaseFromClaim`).

  **Problem:** there is no `PlanApproved` claim. Claims are the runner's file
  (`PhaseClaim`), accepted through the stale/consumed guards; `PlanApproved` is
  a condition the **user** sets. The mechanism the comment does describe —
  `clearSandboxInfraEpisode` at `:948` — is the accepted-claim clear F48b added,
  and it does end the episode on the first claim of the new run. So the
  behaviour is right and the explanation names a channel nobody can find. This
  is the class worth a round item: the comment is the run key's only record (it
  says so itself at `:1360-1362`), and a reader chasing the "PlanApproved
  claim" finds nothing there.

  **Origin:** pi3's pre-review of #273 (`.samples/prereview-273.md:123-145`),
  merged by design when the #273 review routed pi3's four P3s to F48e (review
  5643553745). R37 promotes it out of that batch because it is not a wording
  preference: it is a cause that does not exist, in the comment that carries the
  invariant.

  **Fix:** replace the parenthetical with the site that does the work — the
  accepted-claim clear at `loop_s4_phase.go:948` — or, if the re-plan clears on
  its own, name that call. Do not describe a claim type that is not in the code.

  **Acceptance:** every mechanism the comment names exists in the tree. The
  phrase `PlanApproved claim` matches only this comment today (grep on `347c07c`),
  and matches nothing after the fix, while the sentence still explains how the
  rejection re-plan's count ends.

- [ ] **I139: `FETCH_HEAD` is not a safe revision in a clone several agents share**

  **Where:** `/home/jamie/projects/coxswain/.git/FETCH_HEAD`, written by every
  `git fetch` in that clone; the reports: `.samples/mut-273.md:1` (the
  reviewer's first attempt read `FETCH_HEAD`, which another fetch had
  overwritten), `.samples/r37-skeleton.md:145-151`
  (the pre-reviewer's: `fatal: ambiguous argument 'origin/main...FETCH_HEAD'` although
  `origin/main` resolved), and `.samples/prereview-275.md:188-193` (this round's: a
  mid-review fetch repointed `FETCH_HEAD` from `e83bd21` to `347c07c`, so a
  `git show FETCH_HEAD:…` read **main** and `git diff origin/main...FETCH_HEAD`
  printed nothing).

  **Problem:** the file is a shared mutable path, not a per-caller ref, and when
  it holds several entries `FETCH_HEAD` as a revision resolves to the first one.
  The failure has two shapes: a loud one (`ambiguous argument`) and a silent one
  — a pre-review of a PR reads main and reports on code it did not look at. This
  round hit the silent shape, and only a `git rev-parse` check caught it.

  **Fix:** a norm beside the mutation rules in AGENTS: fetch the branch into an
  explicit tracking ref (`git fetch origin <branch>:refs/remotes/origin/<branch>`),
  then read `origin/<branch>` — and resolve it to a sha (`git rev-parse`) before
  quoting the sha in a note. Never `FETCH_HEAD`, and never a bare branch name
  whose meaning depends on when it was last fetched.

  **Acceptance:** the norm is in AGENTS.md, and the next pre-review or mutation
  record names the sha it read and the ref it fetched, so the two can be
  checked against each other.

- [ ] **I140: the guard's length limit counts bytes while the schema's counts characters**

  **Where:** `loop_controller.go:5462-5466` on `e83bd21`
  (`storableConditionReason`: `len(reason) <= conditionReasonMaxLen`) and
  `loop_s4_phase.go:1872-1877` (the substituted message: "the kubelet recorded
  the failure reason …, **which the condition schema cannot store**"); the CRD's
  `maxLength: 1024` (`config/crd/bases/coxswain.wattu.com_loops.yaml:886`).

  **Problem:** `len()` counts bytes; `maxLength` in OpenAPI counts characters.
  For a reason that is long but multi-byte — 500 CJK characters is 1500 bytes —
  the guard substitutes, and the condition then tells the reader the schema
  cannot store a value the schema would have accepted. The substitution itself is
  safe (I133 stays closed); the **message** asserts a cause that is false for
  that input. No spec covers a multi-byte reason, so nothing observes it.

  **What I could not verify:** I did not run the API server on such a value. The
  byte/character split is from Go's `len` and OpenAPI's `maxLength` definition,
  not from a kind run, and `pod.status.reason` values of that shape are
  speculative.

  **Fix:** count characters (`utf8.RuneCountInString`) so the guard matches the
  schema, or word the message as the operator's own bound ("longer than this
  operator will store") rather than the schema's.

  **Acceptance:** a spec with a multi-byte reason asserts the **stored** value —
  either the reason is stored unchanged (rune count) or the message says the
  operator's bound, not the schema's.

---

## Owner decisions

**D87** remains the only open decision and is now worse. D72-D86 remain open
(D72-D74 in R29, D75-D84 in R30-R32, D85 in R34, D86 in R35).

**D87: seven review rounds are unmerged, so no tick can land.**

  **Where:** the open review-doc PRs #204 (housekeeping), #211 (R30), #217 (R31),
  #235 (R32), #246 (R33), #261 (R34), #267 (R35) and #272 (R36). The newest
  merged round is still R29 (`a02c61e`, PR #200). R37 would be the eighth open
  round. The tick rule is AGENTS, "Closing an issue (I57)": a tick joins a
  review-doc PR once the fix has merged, and it cites the merge's squash hash.

  **Problem:** R37 can tick nothing, and now has three candidates at once —
  I134 (fixed by `5d44419`), I133 and I135 (fixed by #275, `8b55779`). Each round
  restates the same carry-forward table because no round can be ticked while its
  own doc is open (#204 covers R7-R22 only). A reader still cannot tell an open
  defect from an open bookkeeping item, and the fixes a builder can no longer
  see pile up.

  **Decision asked:** (a) merge the seven rounds oldest first, then tick in one
  housekeeping PR (recommended, unchanged from R36), or (b) let one housekeeping
  PR carry the ticks **ahead** of the unmerged rounds, taking the squash hashes
  from `main` — this needs the owner's reading of I57, because the tick would
  land before its round's doc. Either way the owner merges: review docs are
  owner-merge only.

  **Acceptance (if (a)):** the seven review docs are merged, R37 follows them,
  and one housekeeping PR ticks I133-I135 with their squash hashes. **If (b):**
  the vehicle PR is named, it ticks I133-I135 against `main`'s hashes, and I57
  records that ticks may precede their round's merge.
