# Phase 1 review, round 39: three P1s caught before merge, and a RBAC proof that never asked about the subresource

Round 39, 2026-10-25. It follows R38 (branch `review-phase1-r38`, PR #281, not
merged) and covers the five code merges since R38's cut: **#282, #283, #284,
#285, #286**. Code line numbers are against `main` at `98db918`
(`98db918d6923b7709e0c529074bee4cdb33d6536`) unless an item says otherwise. The
review was built from these input files, which are not committed:

- the input skeleton for this round (`.samples/r39-skeleton.md`, including the
  `pi3` extension section that covers #284 and #286);
- the pre-reviews and draft looks: `.samples/early-282.md`, `prereview-283.md`,
  `early-284.md`, `early-285.md`, `prereview-285.md`, `early-286.md`,
  `prereview-286.md`;
- the designs they followed (`.samples/design-f48f-f83b.md`, `design-f87.md`,
  `design-f86-f87-refresh2.md`);
- the reviewer's mutation records (`.samples/mut-282.md`, `-283.md`, `-284.md`,
  `-285.md`, `-286.md`);
- the builders' evidence (`.samples/f48f-mut/`, `f83b-mut/`, `f83b-verify/`,
  `f87a/`, `f87c/`, `f87d/`, `f48f-verify/`);
- R38 (`docs/REVIEW-PHASE1-R38.md`) and the queue
  (`…/scratchpad/queue-b2-after-f30.md`).

A bare `loop_*.go` or `loop_*_test.go` name is under `internal/controller/`.

| PR | Item | Merge |
|---|---|---|
| #282 | F48f: the mount-event read filters server-side, the unreadable read re-arms, the mount claim guard has a spec | d7c421d |
| #283 | F83b: one Job snapshot per verify decision, and a verify Job that is not ours is never trusted or deleted | b81f067 |
| #284 | F87a/F87b: wire the chart render guard into `make test`, fix the chart NOTES approval RBAC | 058529c |
| #285 | F87c: a `status.checkpoints` entry only suppresses a checkpoint when it is trusted | defc770 |
| #286 | F87d: `cox approve`/`reject` patch `status.conditions` instead of PUTting the whole object | 98db918 |

**Status:**

- **No P1 in `main`. One P2 and three P3s below.** The window's three P1s were
  all caught **before merge** by the pre-reviewer's early looks (#285 had
  already been marked ready when its P1 was found):
  - #283's new foreign-Job guard refused the Job shape ~10 existing fixtures
    create, so the P2e family would have stopped deciding. Fixed in `d9e4626`;
    the red run is `make test` exit **2** with 9 failures
    (`.samples/f83b-verify/make-test-73f541e.log:161`, `:182`, `:202`) and the
    green run at the tip is `MAKE_TEST_EXIT=0` (`:63` of the `d9e4626` log).
  - #285 read the VolumeSnapshot GVK **before** the `snapshotCRDPresent` gate, so
    a CRD-less install would have read an absent type every reconcile and dropped
    a genuine `status.checkpoints` entry. Fixed in `9a38b9c`;
    `.samples/f87c/m0-p1-nocrd-gate.txt:3-6` fails the new spec against the
    pre-gate production file (`[FAILED] … loop_checkpoint_envtest_test.go:891`,
    "the entry must not be dropped: with no CRD there is no snapshot to judge",
    `exit=1`), and the reviewer's own gate-removal mutation
    (`.samples/mut-285.md`) fails it against the fixed one.
  - #286's first commit left the chart README prescribing `update` on
    `loops/status` for a write that had become a patch — a user who follows the
    doc gets Forbidden. Fixed in `f4c2143`; `dist/chart/README.md:76-81` now says
    `get` on `loops` + `patch` on `loops/status`.
- **All five merged trees are the reviewed trees.** Empty `git diff --stat` for
  #283 (`d9e4626`→`b81f067`), #282 (`62df40f`→`d7c421d`, `internal/` + `docs/`),
  #285 (`0d91a91`→`defc770`), #284 (`3d3ed68`→`058529c`) and #286
  (`90628a2`→`98db918`). So each verdict describes what shipped.
- **Three PRs took a single `OK`/`OK + notes` on their first review round**
  (#283, #284, #286); #282 and #285 each took a `CHANGES` first and were fixed in
  11 and 29 minutes. Every merge landed **3 seconds** after its verdict ([20:47:02→:05], [21:28:31→:34], [22:08:56→:59],
  [22:34:07→:10]Z), so an `OK + notes` P3 is carried by the queue, never by the
  branch. That is why this round's P3s and the queue's `F87e` describe the same
  three `#286` leftovers.
- **#286 broke the "mark ready when CI is green" rule and nothing else in the
  window did.** `ready_for_review` 22:25:11Z; the tip's `test` check finished
  22:33:22Z. #284 waited (ready 21:23:13Z, CI green 21:16:09Z). Recorded as I147
  because the round's check should be an artefact, not a memory (I141/I143 shape).
- **The pre-review's checks caught their first live I141 case** (#284's body cited
  a `.samples/f87a/02-*` file that did not exist) and a second stale render. Both
  closed before the merge; `02-make-chart-drift-test.txt` is in the shared
  `.samples/f87a/` now. That is the doctrine working, not a new item.
- **Runs.** The reviewer re-ran mutations on #282, #283, #285 and #286 in scratch
  worktrees, and checked #284's NOTES guard; those records are `.samples/mut-282.md`,
  `-283.md`, `-284.md`, `-285.md` and `-286.md`. This round's text ran nothing that
  builds, tests or touches a cluster. Everything
  here is `git fetch --prune`, `git rev-parse`/`show`/`diff`/`grep`/`log` on
  `origin/` refs and `gh api GET` (timeline, check-runs, reviews), plus reads of
  `.samples/`, `AGENTS.md` and two `client-go` files in the module cache. The
  CI timings and merge/tree equalities for #284 and #286 are the only facts this
  round measured itself; the three other merges' evidence is taken from the named
  `.samples` logs and the skeleton's citations, and each is marked where used.

**Carried items, state at this cut:**

- **I141 (evidence paths that resolve only after a copy): still open, but its
  check worked twice this round** — #284's missing `02-*` (fixed before merge) and
  #286's evidence, which ran in the *builder's* worktree
  (`.samples/f87d/10-make-test-90628a2.log` says `coxswain-f87d`) but was in the
  shared `.samples/f87d/` first: that log's mtime is 22:22:34 and the lint log's
  22:24:20, both before `ready_for_review` at 22:25:11Z. The rule is still a
  clipboard rule; no tick can land (D87).
- **I142 (a comment names a site that does not do the work): two more instances,
  both fixed pre-merge** — #282's "at most once per Loop" survived at a const and
  in the runbook after the reviewer named the call site, and #283's
  `readVerifyJobPod` doc claimed the APIReader for a Job read that uses `r.Get`.
  D88 (below) is the fix that has now been earned twice.
- **I143 (an assertion argued, not shown): one improvement, no new case** — #282
  and #283 both recorded a mutation per new assertion, and #286 ran a mutation at
  the tip *after* the code moved (`m4-drop-resourceversion-tip`), which is the
  practice the item asked for.
- **I144–I147: new, below.** F83f (the three copies of the quiet-window marker) is
  queued (`queue-b2-after-f30.md:191`) and interacts with I144; F87e
  (`:193`) carries #286's P3 leftovers, including `docs/CLI-PLAN.md:45-46`.

## P2

- [ ] **I144: the memory PVC degradation is an Event with a TTL, and the marker makes sure it is never repeated**

  **Where:** `loop_p5a_memory.go:84-91` (`memoryPVCUnboundReason`, "Emitted at
  most once per Loop" at `:88`), `:92-99` (the marker is the boolean `"true"`,
  cleared only when the PVC binds — `:285-292`), `:210-219`
  (`emitMemoryPVCUnboundEvent`'s doc, "AT MOST ONCE per Loop"), the call site
  `loop_controller.go:1856` with the marker write at `:1857` and the clear only
  on bind at `:1847`; the sentences that promise it: `docs/OPERATOR.md:357`,
  `docs/UPGRADE.md:214`.

  **Problem.** The degradation is surfaced **only** as an Event. Events expire —
  the sibling marker's own runbook sentence says the operator is "blind again"
  after the cluster's `--event-ttl` (default 1h, `docs/TROUBLESHOOTING.md:555`).
  The marker is set on the first reconcile and cleared only when the PVC binds,
  so it suppresses every later attempt. While a PVC stays unbound for longer than
  the TTL, **no** surface records the degradation:
  - no condition — `api/v1alpha1/loop_types.go:1323-1447` lists every condition
    constant (`PlanApproved`, `Paused`, `ModelAvailable`, `CheckpointsAvailable`,
    …) and none is about memory; `git grep -ni memory api/v1alpha1/loop_types.go`
    returns one unrelated hit at `:1138`;
  - no status field — the API types carry no memory field at all;
  - no `cox describe` line — `git grep -ni memory internal/cli/` finds the
    `cox memory` reader (`memory.go:3-4`) and the `Deps` seams (`cli.go:76-82`),
    not a state line.

  This is the F48f §F3 defect that #282 fixed for the sandbox Events surface
  ("a persistent failure would otherwise go silent when the Event expires",
  `loop_s4_phase.go:1070-1072`), left in the neighbouring marker — which the same
  PR edited the sibling runbook sentence of.

  **Note:** the #282 review called the `loop_controller.go:1854` comment correct,
  because it describes what the code does. This item is about the behaviour that
  comment describes, not about the comment.

  **Caveat, stated because it is not verified:** I read this path; I did not run
  it, and I did not check how long a non-ReadWriteMany class leaves the PVC
  unbound on a lab cluster. The docs name that case themselves (`docs/UPGRADE.md:210`,
  "the class is not ReadWriteMany"), and the argument only needs "longer than
  1h", so the defect stands on the code. Confirm the duration before writing the
  spec's clock.

  **Fix (either, builder's call):** (a) the stronger surface — a `MemoryAvailable`
  condition set `False` while the PVC is unbound and cleared when it binds, so the
  record outlives the Event (the R26 I90 shape; `ModelAvailable` is the precedent,
  `loop_types.go:1350`); or (b) the cheap one — re-arm this marker on the F48f
  quiet window. If F83f lands first, (b) is "use the shared helper", not a fourth
  copy.

  **Acceptance.** A spec shows the degradation is still visible past the Event TTL
  (condition asserted, or a second Event after the window), and the same spec
  fails when the new surface is removed (record the mutation). The five places
  that state the one-shot rule (`:87-88`, `:210-219`, `loop_controller.go:1854`,
  `docs/OPERATOR.md:357`, `docs/UPGRADE.md:214`) match the new one.

## P3

- [ ] **I145: the RBAC check AGENTS names answers a different question for a subresource**

  **Where:** `AGENTS.md:285` — "confirm `kubectl auth can-i <verb> <resource>
  --as=<the operator's ServiceAccount>` returns yes for every rule the new code
  reads". Against the form that actually tests a subresource, in this round's own
  evidence: `.samples/f87d/05-kind-cani-patch-tip-corrected.txt` §1 (plain
  `kubectl auth can-i`) and §2 (a SelfSubjectAccessReview with a real
  `subresource: status` field). For `f87d-tip-updater` the two sections disagree:

  ```
  §1  f87d-tip-updater  update  loops/status                  -> no
  §2  f87d-tip-updater  update  loops/status (subresource=status) -> True
  ```

  **Problem.** `can-i`'s `resource/subresource` spelling is a **name**, not a
  subresource: the file records that it sends `resource=loops,name=status`, so the
  question asked is "may I update the Loop called `status`". That is the same
  shape as the B5a lesson (a rule under the wrong group passes envtest and wedges
  a kind install), one level down: the mandated command passes, or fails, for a
  reason unrelated to the rule. Here it returns **no** where the grant exists —
  a false negative that would send a builder hunting a missing grant that is
  present. The build's `patch` verdicts happened to agree with §2, so nothing was
  decided wrongly this time; the command was simply not the check.

  The two runbook lines that use the form are **correct as written**, and are the
  positive control that this is a subresource-specific trap:
  `docs/TROUBLESHOOTING.md:478` (networkpolicies) and `:641` (volumesnapshots).

  **Fix.** Name the subresource form in AGENTS: an SSAR with
  `resourceAttributes.subresource`, or `kubectl auth can-i … --subresource=<sub>`
  after confirming it sends the field (the file's §2 is the shape that worked).
  Add one clause: `can-i <resource>/<subresource>` does not test the subresource.

  **Acceptance.** AGENTS carries the subresource form and the trap; the next PR
  whose rules include a subresource pastes the SSAR (or the `--subresource`
  output) for it, and the reviewer can read the request body in the paste.

- [ ] **I146: #283 fixed the two fixtures that failed; thirteen sites still plant a verify Job the new guard would refuse**

  **Where:** the fix set the controller reference at two sites
  (`loop_p2e_stall_plan_test.go:289`, `loop_f83_tamper_evidence_envtest_test.go:58`),
  and the guard is `foreignVerifyJob` (`loop_verify_job.go:1844`), read before the
  decision at `applyVerifyOutcome` (`:1385`). `git grep -n 'job = &batchv1.Job{' 98db918 -- internal/controller/` returns 21
  hits. By reading, thirteen of them create a bare Job with only the verify labels
  and no controller reference (the count is the pre-reviewer's reading, not a run):

  `loop_b4_replan_envtest_test.go:91`, `:130`, `:824`;
  `loop_b5a_metrics_envtest_test.go:101`; `loop_checkpoint_envtest_test.go:513`;
  `loop_f83d_tamper_record_envtest_test.go:160`;
  `loop_verify_job_envtest_test.go:790`, `:854`, `:984`, `:1040`, `:1103`,
  `:1223`, `:1302`.

  Two of those carry a comment saying the Job must exist *for the operator to
  read it* — "the fixture Job must exist (else `readVerifyJobPod`'s UID filter
  skips the pod)" (`loop_verify_job_envtest_test.go:785-787`, `:849-851`) — which
  is exactly the read the trust guard now precedes.

  **Problem.** The guard is correct; its **precondition** is now an obligation on
  every fixture, written by hand at two of fifteen sites, with a comment asking
  future authors to keep it. The only evidence the other thirteen are safe is one
  green full run. That evidence is thin in both directions: if a site fires and a
  spec reconciles to a decision, the spec fails on a phase assertion far from the
  cause (as the 9 failures at `loop_p2e_stall_plan_test.go` showed, `.samples/f83b-verify/make-test-73f541e.log:161`),
  and a new spec that asserts "no decision" would pass **vacuously** on a foreign
  Job. I could not tell, by reading, whether any of the thirteen branches ever
  fires; that is the point.

  **Fix.** One helper, `plantVerifyJob(t, loop, job)` (set the reference, create,
  keep any pin annotation), used at all fifteen sites. The `p2g` precedent is the
  same two lines (`loop_p2g_audit_test.go:384-385`).

  **Acceptance.** The grep above returns no bare
  `job = &batchv1.Job{ObjectMeta: … verifyJobLabels(…)}`; the P2e/F83/I65/B4/S5a/
  checkpoint focus runs stay green; the F83b foreign-Job spec still fails under
  `M-B2-trust` (so the guard is still guarded).

- [ ] **I147: #286 was marked ready eight minutes before its `test` check finished**

  **Where:** `gh api repos/papawattu/coxswain/issues/286/timeline` →
  `ready_for_review` at **22:25:11Z**; `…/commits/90628a2/check-runs` → `lint`
  success 22:27:37Z, `test` success **22:33:22Z** (started 22:24:31Z). Merge
  22:34:10Z. The previous tip `6c346e8` was still a draft with `lint` **failure**
  (22:04:09Z), which the body discloses.

  **Problem.** `AGENTS.md:16-18` says to mark a PR ready "when CI (`make test`,
  `make lint`, and the kind e2e once it exists) is green". #286 was ready 40 s into a
  9-minute run. Nothing went wrong — the reviewer's verdict came at 22:34:07Z,
  after `test` — but the rule is unverifiable after the fact unless someone pulls
  the two timestamps, which is what I just did. #284 got it right in the same
  window (ready 21:23:13Z, CI green 21:16:09Z), so this is a slip, not a pattern.

  **Fix.** Make it an artefact: the review request (or the PR body's Verification
  section) pastes `gh pr checks <n>` for the tip sha, and `ready_for_review` comes
  after the `test` conclusion. No AGENTS prose change is needed — the rule exists;
  what is missing is the paste that lets a reader check it.

  **Acceptance.** The next two builder PRs' review requests carry the tip's check
  conclusions, and a reader can confirm `ready_for_review` is later than `test`'s
  `completed_at` from the timeline. A pre-review that finds it earlier records the
  two timestamps instead of a complaint.

## Owner decisions

**D87** stands, and it is now the whole backlog's gate. Still unmerged are #235,
#246, #261, #267, #272, #276 (R32-R37) and #281 (R38); the newest merged round is
R31 (`a30262e`). R39 can tick nothing itself and adds no candidate ticks (I144-I147
are open), so the pile is unchanged: I133-I135 (`8b55779`, `5d44419`, `8b55779`),
I138/I140 (`7251618`), and now I142's two fixed cases if the owner wants them
recorded as closed against `d7c421d`/`b81f067`.

**Decision asked:** unchanged from R37/R38 — (a) merge the open review docs oldest
first and tick in one housekeeping PR (recommended), or (b) let a housekeeping PR
tick ahead of its round's doc against `main`'s hashes, which needs the owner's
reading of I57.

**D88 (new, doctrine).** *When a change moves the meaning of a marker, a gate, a
window or a read path, grep the old wording across `internal/` and `docs/` before
the verdict, and paste the grep with a positive control.*

**Where:** the class is now five instances in three rounds (R38 I142 cases 1-2,
#282's three-site "once per Loop", #283's `readVerifyJobPod` doc, and #286's
`docs/CLI-PLAN.md`). R38 I142's Fix already says the pre-review should grep the
name a comment cites, and that check worked three times. What it does not say is
to grep the **claim's wording across the tree**, which is what found #282's other
two sites and what found #286's missed doc.

**Problem:** each round re-derives the same grep and records it in a different
file. Today's demonstration, both run by me at `98db918` (#282's other two sites
were caught by the *builder's* sweep after the reviewer named one; #286's was
caught by mine, after a `OK + notes` verdict):

```
$ git grep -n 'at most once per Loop' 98db918 -- internal/ docs/
docs/OPERATOR.md:357            <- memory marker (the one legitimate hit)
docs/UPGRADE.md:214             <- memory marker
internal/controller/loop_controller.go:1854  <- memory marker
$ git grep -n '`Update`' 98db918 -- docs/
docs/CLI-PLAN.md:45   <- the write, #286's sweep missed it
docs/CLI-PLAN.md:46   <- the write
docs/CLI-PLAN.md:86   <- C5's test seam ("Fake client records an `Update`")
docs/CLI-PLAN.md:90   <- the interface list (still true: Update is in the contract)
docs/CLI-PLAN.md:134  <- the interface list
docs/REVIEW-PHASE1-R30.md:55, docs/REVIEW-PHASE1-R31.md:175, :177
                      <- positive control: review records of a past `Update`
```

**Fix:** add the sentence to AGENTS beside the mutation rules; the pre-review
pastes the grep before giving a verdict, with the legitimate hit shown as its
positive control.

**Acceptance:** the rule is in AGENTS; the next two rounds' docs paste the
old-wording grep with one legitimate hit shown; no "the comment in another file is
now false" finding turns up after a pre-review verdict.
