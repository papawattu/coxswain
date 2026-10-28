# Phase 1 review, round 43: sixteen drafts merged on CI alone, and the one spec that cannot see its own mutation

Round 43, 2026-10-28. Drafted by pi3 (the read-only pre-reviewer); verified, corrected and
published by the reviewer at ~03:30Z (the reviewer's changes are listed under "Reviewer's edits" at the end).
It follows R42 (`docs/REVIEW-PHASE1-R42.md`, merged as **`be52c6f`** — PR #354, 02:22:27Z) and
covers the **seventeen merges after `38222eb`** (R42's last code merge): **#323–#355**, i.e.
`git log origin/main --oneline 38222eb..origin/main`. Code line numbers are against
`origin/main` at **`d6fd33a`** (`d6fd33a1a2dd09620a3db442e3a44aa3204deac5`, #355) unless an item
pins a sha. A bare `loop_*.go` / `loop_*_test.go` is under `internal/controller/`.

The review was built from these input files, which are not committed: `.samples/post-merge/`
(`INDEX.md`, `b2-handoff.md`, and the 20 run logs), `.samples/r42-mutations/`,
`.samples/m346/`, my own `.samples/post-merge-owed.md` and `.samples/post-merge-owed-2.md`,
the reviewer's verification comments on #339–#347 (`gh api repos/papawattu/coxswain/issues/<n>/comments`),
the merge list (`gh pr list --state merged`), and the current builder brief
(`/tmp/…/scratchpad/handoff-b2-session-1028.md`). Facts the pi3 role could not verify read-only
(anything on `kind-coxswain`) say so explicitly. Every citation in this draft is audited in
`.samples/r43-selfcheck.md`.

| PR | Item | Merge |
|---|---|---|
| #354 | review(phase1-r42) — the previous round's doc | `be52c6f` 02:22:27 |
| #352 | OWNER: `CONTEXT.md` glossary entry stops pinning a sha | `6e2ee51` 02:22:34 |
| #346 | F92 findings 2+5: the CRD-missing window takes part in the earlier-of comparison | `78299a5` 02:24:22 |
| #344 | F53 notes 4+5: name the Job's own deadline, decide it with no pod via Reconcile | `d872d6d` 02:24:26 |
| #343 | F52 B1+B2: failed delivery is not in flight; the hold keeps the verify record | `7233293` 02:24:31 |
| #342 | F51b: the D38 e2e's probe reports REFUSED as UNRESOLVED, like pod_prober | `2bc4be6` 02:24:36 |
| #341 | F82: spec the boot-change unmetered fold and the unmetered counter | `1c97613` 02:24:41 |
| #339 | F80: bound the memory writer's termination-message summary | `f36a8d2` 02:24:45 |
| #338 | F79: the runner retries a transport failure (measurement PENDING) | `2917675` 02:34:55 |
| #336 | F73: the memory-writer outcome is decided from the Job before its pod | `83060bd` 02:35:02 |
| #335 | F72: the evil-agent e2e's exit code must mean every probe was proved | `1a64123` 02:35:06 |
| #333 | F69: the delivered PR body's last-verify line is operator-derived | `9f836ba` 02:35:11 |
| #331 | F67: import-agent reads the agent's object store instead of fetching | `d4fe61f` 02:35:19 |
| #330 | F66: no ServiceAccount token in the verify and deliver Job pods | `26a831f` 02:35:24 |
| #329 | I152: a Paused Loop does not re-announce MemoryPVCUnbound | `8c46987` 02:35:29 |
| #328 | I163: spec the rollback's F57a unconfirmed-create arm | `67aefc0` 02:35:34 |
| #324 | F62/F63: relay the engine's blocked-action count into status + Q4 audit lines | `b3e2e00` 02:35:42 |
| #323 | F65: pin the operator's default images by digest | `7723ffd` 02:35:47 |
| #355 | release notes for #339, #343, #344, #346 | `d6fd33a` 02:47:22 |

**Status:**

- **No P1 in merged code.** The window is 16 merged drafts plus three non-draft PRs (#354 the
  previous round's doc, #352 an `OWNER:` glossary edit, #355 release notes). Every merged head's
  `lint` and `test` concluded **success**, which proves the specs compile and pass as authored and
  says nothing about any mutation. The round's four P2s are **verification state**: one merged
  spec that cannot detect the mutation its body says it detects (I177), the merge wave itself
  (I178), the remaining debt (I179), and a B1 command form that answers `yes` regardless (I180).
- **Sixteen drafts merged in thirteen minutes, with 41 mutation claims unrun.** Merge times
  02:24:22–02:24:45Z (the six of `.samples/post-merge-owed.md:1`) and 02:34:55–02:35:47Z (the ten
  of `.samples/post-merge-owed-2.md:1`). At merge time 41 named mutations and the cluster items in
  I179 had **no output**, exactly as R42's D90 pause predicted
  (`docs/REVIEW-PHASE1-R42.md:385-395`; the disk then read 14G free, `:386`).
- **The pause is over and 18 of the 41 have now been run: 17 fail as claimed, one does not.**
  `df -h /` at 03:06:31Z → `/dev/sda1 197G 131G 59G 70% /`. The 18 are #346's one
  (`.samples/m346/baseline.log:1`, `mutation.log:1`) and #339/#341/#343/#344's seventeen
  (`.samples/post-merge/INDEX.md:9-27`); all are `EXIT=1` except **#340's F11 mutation, which
  passes under mutation** (`INDEX.md:9`) — R42 I165's second item, now measured as a **failed**
  claim rather than an unrun one. #347's half of I165 reproduces
  (`.samples/r42-mutations/347-f92-work-requeue-branch.log:66`, `MUTATED EXIT=1`).
- **23 mutations and 5 cluster items are still owed** (I179). They are wave 2's ten PRs
  (`.samples/post-merge-owed-2.md:27-38`, totals `:40`) — unchanged by this round — plus #342's
  two Calico runs (`.samples/post-merge-owed.md:659-660`).
- **#324's B1 proof is complete** (finished ~03:16Z, after pi3's draft): `--subresource=log`
  reads `no` before the upgrade and `yes` after, with `list`/`create` controls at `no`
  (`324-rbac-can-i-pods-log.log:5-11`, `:37-45`); with the rule removed the operator log shows
  exactly **1** `forbidden` line (the positive control, `324-rbac-forbidden-control.log:23`), the rule
  is restored (`:30` `RESTORE_EXIT=0`, `:31` `yes`) and the live ClusterRole matches the helm release
  (`:33` `DIFF_EXIT=0`); the whole-session audit has that control line as the **only** `forbidden`
  line (`324-session-forbidden-audit.log:3-6`); the redeploy record names the image
  `coxswain-controller:r43-d6fd33a` (`324-redeploy-main.log:13`, `:22` `DIFF_EXIT=0`). The earlier
  `RESTORE_EXIT=1` (`324-rbac-real-pods-log-read.log:31`) was followed by a successful restore and
  diff (`:59-63`). Reviewer's own read-only check at ~03:22Z: `coxswain-manager-role` carries
  `pods/log` again. (I180 still stands: the AGENTS command form would have answered `yes` either way.)
- **#344 is the one body whose own focus command cannot reproduce its claim** (I181): the detector
  is the pre-existing seam spec at `loop_f53_verify_isolation_envtest_test.go:370`, the body's focus
  matches only the Reconcile spec at `:388`, and the measured negative control is
  `344-M-5a-body-focus.log:31-32` (`EXIT=0`) against `344-M-5a.log:60` (`EXIT=1`) under the widened
  focus.
- **The four drafts marked ready at 02:35Z are all still conflicting** (I185): `#322 #325 #332 #337`
  report `"mergeable": false, "mergeable_state": "dirty"`, and their reviews belong on the PRs after
  the owner's rebase — #322's is being done in the shared tree (I184).

## P1

- None merged. (Calibration: the window's sharpest defects were all caught before merge in earlier
  rounds — #337's PVC-deleting upgrade path (R42 I172) is still a draft at `dc744a7` — and this
  window's own worst finding, I177, is a test that stopped discriminating, not a behaviour change.)

## P2

- [ ] **I177: #340's egress-proxy spec cannot detect the mutation its own body says it detects**

  **Where:** the spec is `It("a no-op reconcile leaves the egress proxy Service resourceVersion
  UNCHANGED (F11, no reconcile storm)", …)` at `internal/controller/loop_i42b_egress_proxy_test.go:262`;
  it drives `r.Reconcile` twice (`:307`, `:318`), keeps the first pass's `rv1` (`:311`) and asserts
  `svc.ResourceVersion` equals it on the second (`:321-322`), plus the ClusterIP (`:312-313`,
  `:323-324`). The guard the mutation removes is `internal/controller/loop_controller.go:3308`
  (`if svcDesired.ResourceVersion == "" || svcDesired.Spec.ClusterIP == "None" {`; the model-proxy
  twin is `:3353`). The mutation, exactly as #340's body writes it (`if true` at `:3308` only):
  `.samples/r42-mutations/340-f11-egress-svc-guard.log:27-37`. Measured:
  baseline `1 Passed` `BASELINE EXIT=0` (`:25`); mutated `1 Passed` **`MUTATED EXIT=0`** (`:53`).

  **Problem:** the claim is false as merged, and the reason is measured rather than inferred. With
  a probe added to the same throwaway worktree
  (`.samples/post-merge/340-f11-mutation-diagnosis.log`), both of the spec's `Reconcile` calls reach
  the egress Service and `CreateOrUpdate` returns **`op=updated`** both times (`:47-48`), yet
  `rvAfter=[243]` on the second pass as well as the first: the API accepted the redundant update
  and the stored object's `resourceVersion` did not move, with `clusterIP` preserved and
  `IPFamilies` re-defaulted (`:60-72`). So the pass the spec asserts is one where the mutated write
  changes nothing observable through the assertions it makes, and the `resourceVersion` comparison
  cannot tell the fixed guard from the removed one. The remaining question — whether some *other*
  spec in the 689 catches the guard's removal — was not measured either way: the run used the
  single-spec focus, and the "no spec in the suite catches it" line in
  `.samples/post-merge/b2-handoff.md:65-66` is a claim, not a run. (For calibration on what the
  spec was for: the guard's own comment records the storm it prevents — 140 `op:updated` in 2 min
  on the live cluster, `loop_controller.go:3303-3307`.)

  **Fix:** assert something the mutation actually changes. The spec cannot see
  `CreateOrUpdate`'s `OperationResult` today because it calls `r.Reconcile`, so the fix is one of:
  (a) a unit spec that calls `controllerutil.CreateOrUpdate` with `mutateEgressProxyService`
  against the envtest client twice and requires `controllerutil.OperationResultNone` on the second
  call (`OperationResultUpdated` under the mutation); or (b) wrap the reconciler's client with a
  controller-runtime interceptor that counts `Update` calls on Services and assert the second
  `Reconcile` issues none. Both are independent of the apiserver's RV behaviour, which is what
  made the current assertion blind. Do **not** replace it with another `resourceVersion` or
  `ClusterIP` assertion — the measurement above shows both are stable under the mutation.
  This is R42 I165's #340 half continued: I165's acceptance conjoins #340 and #347
  (`docs/REVIEW-PHASE1-R42.md:112-115`), so its box must **stay unticked**; #347's half is met.

  **Acceptance:** on the fix branch (`fix/f11-spec-op-result`), the new assertion **fails** under
  exactly `if true` at `loop_controller.go:3308` (the log pastes the count/`OperationResult`: 2
  Updates or `updated` on pass 2 mutated, 1 or `none` on main) and **passes** on `main`; both
  outputs are in the body, and `#340`'s claim is corrected in place (R41 I155/I164 precedent) or
  explicitly superseded by the new PR.

- [ ] **I178: sixteen draft PRs merged with 41 mutation claims unrun, and the owed list is now the only record**

  **Where:** `gh pr list --state merged` for 2026-10-28 shows 16 drafts merged at
  02:24:22–02:24:45Z and 02:34:55–02:35:47Z (the table above), against #354/#352/#355 which were
  not drafts. What each owed at merge is `.samples/post-merge-owed.md:1` (six drafts) and
  `.samples/post-merge-owed-2.md:1`, `:27-38`, `:40` (ten drafts, "23 mutation items and 5 cluster
  items outstanding"). The pause that explains the state is R42's D90
  (`docs/REVIEW-PHASE1-R42.md:385-395`). What has output at 03:09Z:

  | PR | owed at merge | run now | where |
  |---|---|---|---|
  | #346 | 1 mutation | 1 — **fails** (`EXIT=1`) | `.samples/m346/mutation.log:1-28` |
  | #339 | 7 (M1–M7) | 7 — all fail | `.samples/post-merge/339-M*.log` (`INDEX.md:10-16`) |
  | #341 | 3 | 3 — all fail | `341-M1.log`, `341-M1a.log`, `341-M2.log` |
  | #343 | 3 | 3 — all fail | `343-M-B1.log`, `343-M-B2.log`, `343-M-B2b.log` |
  | #344 | 4 | 4 — all fail (M-5a only under the widened focus) | `INDEX.md:23-25` |
  | #342 | 4 selftests (already run) + **2 cluster runs** | 0 of the 2 | `.samples/post-merge-owed.md:657-661` |
  | #323 | 3 mutations + N2 + 1 optional | 0 | `.samples/post-merge-owed-2.md:27-38` |
  | #324 | 8 mutations + B1 | B1 done (status above); 0 of 8 mutations | I179 |
  | #328 | 1 | 0 | `owed-2.md:27-38` |
  | #329 | 1 | 0 | same |
  | #330 | 2 + 1 optional | 0 | same |
  | #331 | 2 | 0 | same |
  | #333 | 2 (M-F3 already had output) | 0 | same |
  | #335, #336 | 0 (5 + 3 already had output) | — | `owed-2.md:36-37` |
  | #338 | 4 + **the measurement** | 0 | same |

  Totals: **18 run, 23 owed, 1 of the 18 a failed claim**; plus #342's two cluster runs, #324's B1,
  #323's N2 and #338's measurement.

  **Problem:** R42's I165 fix proposed the rule "a body whose primary evidence is a mutation does
  not leave draft until that mutation has an output line — 'unrun' is a draft state, not a merged
  one" (`docs/REVIEW-PHASE1-R42.md:109-110`). It was not in force: 16 bodies left draft with 41
  claims unrun in 13 minutes. The owner owns merges and D90 gave the options
  (`:385-393`), so this is a **record**, not a blame item — but the record now matters twice: 17
  of the 18 claims held, and one (#340, I177) did not, which is exactly the outcome the rule is
  meant to prevent. It also means the merged record is wrong in more places (I181, I182, I183) and
  the only durable place a reader can learn the true state is the uncommitted
  `.samples/post-merge*/` pair, which AGENTS I68 forbids committing.

  **Fix:** (a) adopt the rule as written and make the owed ledger part of the merge decision: a PR
  whose body names a mutation merges only with that output, unless the owner records the override
  in the round doc (D90's option (a) made explicit); (b) because the ledger cannot be committed,
  the round doc carries it (this item and I179 are that record); (c) treat a merge wave as a
  checkpoint: the reviewer's verification comments on #339–#347 (02:29Z, 02:54–02:55Z) are the model
  — one comment per PR, naming the log per claim.

  **Acceptance:** the next merge window's review doc can cite an output file for every mutation
  claim in every merged body (`grep -n 'PENDING\|NOT executed\|specified, NOT run' <body>` is empty
  for mutation-carrying bodies), and any merge that waives it carries the owner's override line in
  this round's doc.

- [ ] **I179: 23 mutations and five cluster items are still owed on ten merged PRs**

  **Where:** `.samples/post-merge-owed-2.md:27-38` (the by-PR table), the per-item sections
  (`#323` M-P1/M-P2/N6, `#324` M1–M8, `#328`, `#329`, `#330`, `#331`, `#333` M-F69/M-F2, `#338`
  M1–M4), the three flagged items F1 `:55` (#324 B1), F2 `:89` (#323 N2), F3 `:113` (#338's
  measurement is the gate). #342's two cluster runs are `.samples/post-merge-owed.md:659-660`.

  **Problem:** these are the verification state of ten merged PRs, and two of them gate more than
  themselves: **#338's measurement** decides whether F79's retry stands at all (the body's own
  "BLOCKED ON THE MEASUREMENT", `owed-2.md:113-127`), and **#324's B1 proof** is the AGENTS rule's
  own gate for a cluster-wide `pods/log` grant (`AGENTS.md:276-288`). That proof is now **complete** (see the status bullet: positive control, restore,
  helm-render diff and redeploy record are all on disk), so #324's remaining debt is its eight
  mutations.

  **Fix:** finish the queue as briefed (`handoff-b2-session-1028.md:20-38`): (1) #324 B1 — done; (2) #323's N2 control; (3) the 23 owed mutations; (4) #342's two
  Calico runs are the **owner's** (the calico cluster is off limits to builders and to pi3,
  `handoff-b2-session-1028.md:31`). Optionals (#323's preload smoke, #330's `ls` in a verify pod)
  are owed-but-not-gates.

  **Acceptance:** each of the 23 has a log opening with `# sha:` and ending `EXIT=`, cited from the
  round doc; #324's log shows `auth can-i … --subresource=log` = `yes`, zero `forbidden` over a
  Loop create+delete (with a positive control for the count), the ClusterRole restored and diffed
  against the helm render, and the redeploy record; #338's measurement is either the gate's evidence
  or an owner decision to close F79 without the retry; #342's two runs are recorded or explicitly
  deferred by the owner.

- [ ] **I180: AGENTS' own B1 command form answers `yes` whether or not the subresource rule exists**

  **Where:** `AGENTS.md:285-288` — "confirm `kubectl auth can-i <verb> <resource> --as=<the
  operator's ServiceAccount>` returns yes for every rule the new code reads". Measured in the #324
  proof: **before** the main build was deployed, with `grep -c 'pods/log'` = **0** in
  `coxswain-manager-role` (`324-rbac-can-i-pods-log.log:10-11`), `kubectl auth can-i get pods/log
  --as=…` printed **`yes`** (`:64-65`; the GOTCHA note at `:61-63` records the cause as kubectl's
  parse: `log` is taken as a resource *name*, so the query is about `get pods`).
  The subresource form discriminates: `kubectl auth can-i get pods --subresource=log` printed
  **`no`** before the upgrade (`:7-8`) and **`yes`** after (`:38-39`; negative controls `list`/`create`
  on the same subresource print `no`, `:42-45`).

  **Problem:** the norm's example command, copied into an owed list and into a proof log, returns
  `yes` in the state the rule is meant to catch — a gate whose pass is unconditional. This is the
  same class as `AGENTS.md:289-294` ("a count or grep used as evidence needs a positive control")
  and it is the only reason the in-flight proof noticed: the log has to record the wrong form
  printing `yes` pre-fix to see it. The owed text this round inherited used exactly the vacuous
  form (`.samples/post-merge/b2-handoff.md:116-118`, `owed-2.md:70-75`).

  **Fix:** owner edit to `AGENTS.md:285-288`: the command becomes
  `kubectl auth can-i <verb> <resource> [--subresource=<sub>] --as=…`, with a sentence saying a
  subresource grant (`pods/log`, `pods/exec`, `deployments/scale`) is checked with
  `--subresource=`, and that the check needs the pre-fix `no` as its positive control.

  **Acceptance:** `git grep -n -- '--subresource' AGENTS.md` finds the rule; the next B1 proof in a
  round doc shows the before/after `no`/`yes` pair for the exact rule it adds, with the wrong form
  either absent or marked as wrong.

## P3

- [ ] **I181: #344's published focus command never selects the spec its mutation fails**

  **Where:** `gh pr view 344` body, the focus block for all four mutations:
  `-ginkgo.focus='deadline-killed Job whose pod is GONE'`. The detector for M-5a is the
  **pre-existing seam spec** `It("decides an infra failure for a deadline-killed Job with NO pod at
  all (the pod is GC'd or evicted first)")` at `loop_f53_verify_isolation_envtest_test.go:370`, with
  the deciding assertion at `:375-376`; the body's focus matches only the new Reconcile-driven spec
  at `:388`. Measured: same diff, body's focus → `ok … 13.602s`, **`EXIT=0`**
  (`344-M-5a-body-focus.log:31-32`); widened focus `'deadline-killed Job'` → `FAIL … EXIT=1`
  (`344-M-5a.log:60`), with the log's own note at `:62-66`.

  **Problem:** the body's mutation table row for M-5a names the right spec (`:370-382`) — the spec is
  correct and has teeth — but the one command a reviewer would re-run selects the spec that passes.
  A reviewer following the body sees no failure and could conclude the mutation has no detector;
  AGENTS R28 I92 makes the exact focus part of the claim. Same class as #331's `-ginkgo.focus='F67'`
  (`.samples/post-merge-owed-2.md`, cross-cutting 1). The comment posted on the PR does record the
  correction (`issues/344/comments`, 02:55:18Z), so the record exists — just not where the body is.

  **Fix:** correct the body's focus line in place (merged bodies were corrected this way for R41
  I155/I164): either widen it to `'deadline-killed Job'` or, better, state that the detector is the
  seam spec and give its own focus (`-ginkgo.focus='decides an infra failure for a deadline-killed
  Job with NO pod at all'`), keeping the two exits in the section.

  **Acceptance:** the focus string in `gh pr view 344`'s body is a substring of the detector It at
  `loop_f53_verify_isolation_envtest_test.go:370` (a `grep -F` of the body's focus against the spec
  file matches), and the mutation section quotes both `EXIT=0` (narrow focus) and `EXIT=1` (widened).

- [ ] **I182: three filed mutation hunks do not apply or do not compile as published**

  **Where:** (a) `.samples/f80/mutations/M2-no-truncation-marker.diff:7-9` deletes
  `fmt.Fprintln(&buf, summaryTruncationMarker(len(lines)-kept))` — the **pre-F80b** one-argument
  marker; the merged call takes two arguments (`cmd/memory-writer/main.go:511-513`, the
  `lessonsTotal-lessonsKept` form), so the filed hunk does not apply, and the run used a re-derived
  diff whose context names both arguments (`339-M2.log:4-16`). (b)
  `.samples/f80/mutations/M3-byte-cut-instead-of-line-cut.diff:7` writes
  `buf.Write(line[:limit-buf.Len()])`, which does not compile on a `string`; the run applied
  `buf.WriteString(...)` instead (`339-M3.log:35-38`). (c) #343's body publishes M-B2 as a deletion
  loop (`gh pr view 343` body, the M-B2 block); as written it leaves `verifyReason`/`verifyMessage`
  unused, and the run added `_, _ = verifyReason, verifyMessage` above the loop
  (`343-M-B2.log:80-83`).

  **Problem:** AGENTS R28 I92 asks for "the exact diff and the exact focus command, with its
  output", and the reviewer re-runs both. Two of the three filed artefacts cannot be applied as
  published (one patch failure, one build failure) and the third is not the diff that was run, so
  "the named artefact reproduces" is false for all three even though the **detector** result is
  sound: each applied change is the same mutation and each failed the right spec
  (`339-M2.log:21-27`, `339-M3.log:19-29`, `343-M-B2.log:46-49`, `:58-61`). It matters for the record, not
  for the verdict — and it matters more for the 23 items still owed (I179), whose diffs will be
  applied by whoever runs them.

  **Fix:** replace `M2-no-truncation-marker.diff` with the applied hunk from `339-M2.log:4-16`, add
  the `WriteString` line (or a `# NOTE` header naming the compile fix) to M3's file, and put the
  `_, _ =` line into #343's body M-B2 block. The evidence files are uncommitted (AGENTS I68), so
  the body text is the only part that needs a review-visible edit.

  **Acceptance:** `patch --dry-run -p0` applies every hunk under `.samples/*/mutations/*.diff`
  against `origin/main` (scripted: `for f in …; do patch --dry-run -p0 -i "$f" || echo REJECT $f; done`
  prints no `REJECT`), and every applied deviation is stated in the body that publishes the hunk.

- [ ] **I183: the verification comments name the wrong sha for 18 of the 20 run logs**

  **Where:** the reviewer's comments posted 02:54–02:55Z on #339, #341, #343 and #344 each open
  "each was run on `main` `f36a8d2`" (`gh api repos/papawattu/coxswain/issues/339/comments`,
  02:55:14Z). The logs carry their own header, and it says otherwise: the 14 logs for
  #339/#343/#344 open `sha: 7723ffd7957092c6e9897828a5d5aecc14b8c4d7` (`339-M2.log:2`,
  `343-M-B1.log:2`, `344-M-4.log:2`) and #341's three open
  `sha: d6fd33a1a2dd09620a3db442e3a44aa3204deac5` (`341-M1.log:2`). Only #347's and #340's logs name
  `f36a8d2` (`.samples/r42-mutations/347-…log:3`, `340-…log:3`), and the diagnosis log is at
  `7723ffd` (`340-f11-mutation-diagnosis.log:2`).

  **Problem:** the citation is the whole evidence chain for a mutation claim (R25 I84: name the file
  and line), and the sha line is the header the round's own convention requires (R42 I168). Here the
  comments' sha is the merge commit of #339, not the tree the run used, so a reader reconciling the
  two has to distrust one of them. The run is not in question — the logs name their sha — the claim's header is. Note that the
  files those mutations touch **did** change between `f36a8d2` and `7723ffd`
  (`git diff --stat f36a8d2 7723ffd` lists `loop_deliver_job.go`, `loop_verify_job.go`,
  `loop_memory_writer_job.go`: the F66, F67 and F73 merges), so the logs prove the specs
  discriminate on the later tree, which contains each PR plus every later merge.

  **Reviewer's correction (done ~03:25Z):** correcting comments on #339, #341, #343, #344 and #340
  naming each log's own sha. That correction itself added an **unverified** sentence ("files the
  mutations touch are unchanged between that sha and `f36a8d2`"), which the diff above disproves; it
  was retracted by a further comment on the same five PRs a few minutes later. Same class as I183
  and AGENTS I76: a claim written from memory instead of from the command that would check it.

  **Fix:** one correcting comment per affected PR naming each log's own sha (or a single note in the
  round doc stating that the logs' `# sha:` headers are authoritative and the comments' `f36a8d2` is
  the #339 merge sha). Either way, the next verification comment is written from
  `head -3 <log>` rather than from memory of which merge was current.

  **Acceptance:** for every log under `.samples/post-merge/` and `.samples/r42-mutations/`,
  `head -3 <log> | grep -o 'sha: [0-9a-f]*'` matches the sha the comment names for that file (a
  scripted comparison over the 20 logs), or the round doc carries the correction line.

- [ ] **I184: the shared tree is off `main` for a hand rebase while builders and reviewers share it**

  **Where:** (at ~03:10Z; by ~03:17Z the shared tree had moved to a clean `tmp/rebase-325`, i.e. the
  owner's #325 rebase, with an owner `go test` running there) `git branch --show-current` → `tmp/rebase-322`; `git status --short` →
  ` M internal/controller/loop_f83d_tamper_record_envtest_test.go`; `git worktree list` →
  `/home/jamie/projects/coxswain 7344623 [tmp/rebase-322]`, while the builder session runs in
  `/home/jamie/projects/coxswain-b2-324` (detached at `d6fd33a`). The commits on that branch are
  F58 work (`7344623`, `a1a1557`, …). The brief has to compensate for it in its own words:
  "The shared tree /home/jamie/projects/coxswain is the OWNER's right now (branch tmp/rebase-322, an
  in-progress hand rebase of #322, uncommitted). Never checkout/reset/stash/commit/run tests there"
  (`/tmp/…/scratchpad/handoff-b2-session-1028.md:8-11`).

  **Problem (observation, not a rule change):** AGENTS' review protocol assumes the shared checkout
  is a reader's view of `main` — every builder and pi session is told to look there, and `/new`
  warns about uncommitted files in it. For the length of this rebase it is not: `git grep`,
  `git log`, `make test` and any working-tree read answer about an in-progress F58 branch, so a
  reader must use `git show origin/main:<path>`/`origin/main` refs (this draft did, and the two
  owed-list documents say the same at `owed-2.md:10-12`). Nothing was lost this round; the cost is
  a class of mistake that a reader cannot see from inside the tree.

  **Fix (a question for the owner, since AGENTS is owner-only):** should a rebase happen in a
  worktree (`git worktree add` on the branch) leaving the shared checkout on `main`, with the shared
  tree returned to `main` when the rebase ends? If the current pattern is intended, the interim
  mitigation is the one already used this round: every brief names `origin/main` explicitly and
  every command in it is ref-qualified.

  **Acceptance:** the owner answers in this round's doc; either `git -C /home/jamie/projects/coxswain
  branch --show-current` reads `main` (or detached at `origin/main`) whenever a builder is working,
  recorded once at the end of the rebase, or the mitigation is written into the next brief.

- [ ] **I185: four drafts were marked ready for review while conflicting with `main`**

  **Where:** `#322`, `#325`, `#332`, `#337`. `gh api repos/papawattu/coxswain/pulls/<n>` at 03:07Z
  returns `"mergeable": false, "mergeable_state": "dirty"` for all four, and the timeline's
  `ready_for_review` event times are 02:35:49Z, 02:35:36Z, 02:35:13Z, 02:34:56Z
  (`issues/<n>/timeline`). Their recorded base shas are R42-era merges (`05ca5bf`, `05ca5bf`,
  `818c7ba`, `eeb91dc`).

  **Problem:** the ready mark is the signal that review may start, and a conflicting branch is not
  reviewable as a merge candidate: the diff against `main` is not what would land, and `gh pr
  merge` cannot succeed. The four landed in the same minute as the merge wave (02:34:56–02:35:49Z
  against the ten merges at 02:34:55–02:35:47Z), so the likely cause is the same unblocking rather
  than a review decision — unproven, and not needed for the record. Nothing bad happened: all four
  are still open and unmerged, and the brief tells the builders to leave them alone
  (`handoff-b2-session-1028.md:37`).

  **Fix:** the ready mark waits for one read — `gh api …/pulls/<n> --jq .mergeable` is `true` — or
  the PR says "ready for content review; blocked on the rebase of #NNN". Reviews happen on the PRs
  after the owner's rebase, not in this doc.

  **Acceptance:** at the next ready mark, `mergeable` reads `true` or the PR carries the
  blocked-on-rebase sentence; after the rebases, each of the four has a review verdict
  (`Verdict: OK`, `OK + notes` or `CHANGES`).

## Owner decisions

**D91 (new): the rebase pattern, and whether the merge wave's debt is paid before the next merge.**
Two questions this round cannot settle itself. (a) I184: is a hand rebase *in the shared checkout*
the intended pattern, or should it happen in a worktree so the shared tree keeps reading `main`?
(b) The disk pause that made 16 drafts merge on CI alone is over — `df -h /` now reads 59G free
(03:06:31Z) against 14G at R42 D90 (`docs/REVIEW-PHASE1-R42.md:386`) — so D90's option **(b)**,
"unpause for a bounded clearance run, merging nothing new until the backlog's evidence exists", is
now available and is the recommendation: 23 mutations and five cluster items remain (I179), and the
one claim that has been checked and failed (#340, I177) is exactly the class the pause was trading
against. **The decision is the owner's** because it trades review throughput against the R28 I92
evidence rule.

**D90 is exercised, not closed.** The window is D90 option **(c)** in the event ("keep the pause for
behaviour merges but allow docs/test-only merges"): most of the 16 are test-only or additive specs,
but #324 (an RBAC subresource grant), #323 (image pins), #331 (a Job script change) and #338 (runner
retry behaviour) are not docs/test-only, so (c) as worded did not cover the wave either. Worth
stating in the answer to D91.

**R42 I165 is half-closed, and the box must not be ticked.** Its acceptance conjoins both PRs
(`docs/REVIEW-PHASE1-R42.md:112-115`): #347 reproduces (`MUTATED EXIT=1`,
`.samples/r42-mutations/347-f92-work-requeue-branch.log:66`, failing at
`loop_f92_kubearmor_silent_test.go:130` with `60000000000` vs `5000000000`), #340 does not (I177).
I177 carries the #340 half.

**D88 still stands** — `git grep -c D88 -- AGENTS.md` prints nothing (R42's check), and I177/I178
are two more instances of the "a claim without its output" class it was meant to name.

**D84 / D86 are still open** (*per R40/R41*), untouched by this window.

**Release notes are current:** #355 (`d6fd33a`) and #356 (`526ceb7`) cover every behaviour change in the
window; a Known-issues prune for the items they fixed (F62, F63, F65, F66, F67, F69, F73, F80, F82) is
in review. #320/#297/#296 are the older review PRs, still owner-merge.

**Tags:** `review/phase1-r42` was pushed by the reviewer on R42's merge commit `be52c6f` (02:23Z).
`review/phase1-r40` and `review/phase1-r41` wait for #297 and #320 to merge. R43's tag goes on this
round's merge commit; the glob must not match `r4` (R42 I158).

## What I did not verify

No execution of any kind: no `go build`/`go test`/`make`/`bash` of a project script, no mutation,
no `kind`, no `kubectl`, nothing checked out or written in the shared tree. The only commands beyond
`git`/`gh` reads were `sed`/`grep`/`nl`/`head`/`tail`/`wc` on files already on disk, and `df -h /`.

Every "fails"/"passes" here is a reading of a log that already exists, cited by file and line. Where
a claim is *not* measured I say so rather than filling it in: whether any other spec in the 689
catches #340's guard removal (I177), why the apiserver accepted the redundant Service update without
moving `resourceVersion` (I177 — the diagnosis log itself says the cause is not established), and (at
draft time) the cluster state of #324's ClusterRole after `RESTORE_EXIT=1` — since resolved: the
reviewer checked it read-only and the builder's later logs show the restore and diff (status bullet). Facts about `kind-coxswain` are taken from the logs the
builder wrote, with the timestamp the log carries.

## Reviewer's edits

pi3's draft (`.samples/r43-draft.md`, self-checked in `.samples/r43-selfcheck.md`, 15 WRONG fixed)
was verified line by line against the logs it cites. The reviewer changed: the #324 status, I178's
table row and I179 (the B1 proof finished after the draft); I183 (the draft repeated the
"files unchanged" sentence, which is false, and now records the reviewer's corrections and the
retraction); I184 (the tree moved to `tmp/rebase-325`); the release-notes and tag notes under Owner
decisions; and the closing "did not verify" note. Every other sentence is pi3's.

