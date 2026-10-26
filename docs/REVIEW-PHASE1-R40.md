# Phase 1 review, round 40: no P1 and no P2 across eight merges, and three small gaps that shipped in comments and one sweep

Round 40, 2026-10-26. It follows R39 (PR #287, merged `8a8c27c`) and covers the
eight merges since R39 was cut: **#288, #289, #290, #291, #292, #293, #294,
#295** — plus the bulk merge of the review stack, which the reviewer did on the
owner's instruction ("review and merge any open PR", ~03:15Z). Code line numbers are
against `main` at `2cd3040` (`2cd3040da043640faa4356694f6d06e89512e4f1`) unless
an item says otherwise. The review was built from these input files, which are
not committed:

- the input skeleton `.samples/r40-skeleton.md`. It was written 00:52Z, when the
  window was `98db918..4869cb9`; this round extends it to the last merge;
- the pre-reviews `.samples/prereview-290.md` (the F49 specs) and
  `.samples/prereview-294.md` (F49b), both mine, which carry the mutation diffs
  and the failing lines for each;
- the evidence I opened: `.samples/f49/m1-drop-approval-hold.diff`,
  `m2-drop-nopin-branch.diff`, and `.samples/f49b-mA.diff`, `-mA.txt`,
  `-mB.diff`, `-mB.txt`, `-make-test.log`, `-make-lint.log`, `-focus.log`;
- R39 (`docs/REVIEW-PHASE1-R39.md`) and #296's body.

A bare `loop_*.go` / `loop_*_test.go` name is under `internal/controller/`.

| PR | Item | Merge |
|---|---|---|
| #288 | F83f: one quiet-window marker implementation, and value-based marker specs | `653d46e` |
| #289 | F87e: the #286 P3s (status constants, the plan doc's verb, the API note's wrap) and the kind leftovers | `23c0d23` |
| #290 | F49: spec the Manual-approval sandbox hold and the deliver no-pin branch | `4869cb9` |
| #291 | F51 (P2-2): an unresolvable CNI probe target is UNRESOLVED, not REACHABLE | `b33cc7a` |
| #292 | F50: sample Loops on the seeded repos, gocli task 2 manifest, dead chart values | `3eaf998` |
| #293 | F51 (P2-3): a nil CNIProber holds the sandbox Suspended (fail-closed) | `f07fee1` |
| #294 | F49b: the Auto-mode approval hold, and the empty-commit no-pin half | `2cd3040` |
| #295 | Docs: release notes for #291, #292, #293 | `852c0ba` |

**Status:**

- **No P1 and no P2 in this window, and the reason is not luck: every merged
  tree is the tree that was reviewed.** Four merges are byte-identical to their
  reviewed heads (`git diff <head> <merge>` is empty for #288 `2462204`→`653d46e`,
  #289 `5fb4b48`→`23c0d23`, #290 `1f17374`→`4869cb9`, #295 `dbeef49`→`852c0ba`),
  and the other four apply the reviewed patch exactly: for #291 `2190172`→`b33cc7a`,
  #292 `9a317a7`→`3eaf998` and #293 `e49a7db`→`f07fee1` the applied patch
  (`git diff <merge>^ <merge>`) and the reviewed one (`git diff <merge-base> <head>`)
  are identical, and #294 `7b1eaef`→`2cd3040` differs only in the two `index`
  lines, because its parent already carried main's F51 test sweep in those files.
  So the three P3s that shipped unfixed (I148-I150 below) were not changes
  between review and merge: two are what the verdicts described (#289's cite,
  #294's unasserted comment), and the third is a gap in #293's sweep, whose
  verdict says merging `main` covers pi3's early P2 about the spec leaning on the
  nil prober arm (`.samples/early-290.md:145-147`).
- **Ten verdicts for eight merges, and both `CHANGES` were fixed inside the
  window.** #291 and #292 each took `Verdict: CHANGES` at 01:10:32Z / 01:10:34Z
  and `Verdict: OK` at 03:16:32Z / 03:16:40Z. #291's item was a message that
  asserted a cause the code had disproved (`pod_prober.go:513-514` at the time);
  #292's P1 was a sample Loop that could not pass its own check
  (`examples/webapi/test/smoke.sh:63` asserts `pong`, the goal asked for
  `{"status":"ok"}`). The other six took one `OK`/`OK + notes` each.
- **Every merge landed 3-5 s after its final verdict** (#288 4 s, #289 3 s,
  #290 4 s, #291 5 s, #292 5 s, #293 5 s, #295 3 s, #294 4 s), and **no PR in the
  window has an inline comment or a review thread**
  (`/pulls/<n>/comments` → `length` 0 for all eight). So "required conversation
  resolution" was never exercised: every finding travelled in a verdict body.
- **CI was green before the ready mark on all five code PRs**, which is I147's
  acceptance met on its checkable clause: #288 `test` 23:15:37Z → ready
  23:21:02Z; #289 23:57:28Z → 23:59:40Z; #290 00:39:13Z → 00:41:41Z; #293
  01:37:05Z → 01:38:16Z; #294 03:25:16Z → 03:34:43Z (`…/commits/<sha>/check-runs`
  and the issue timeline). #291 and #292 were marked ready at 00:55:57Z /
  00:56:06Z, after their then-tip was green, and pushed a third commit after.
  #295 is a docs PR opened ready, so it has no `ready_for_review` event. The
  evidence shape is the per-tip check conclusions quoted in each body, not a
  pasted `gh pr checks`.
- **The bulk merge resolved D87 (a), and the tags are right.** The reviewer
  merged the stack on the owner's instruction. R32-R39
  are eight squash commits, `b38cf30` (03:18:07Z) through `8a8c27c` (03:19:28Z),
  followed by #221 `77acf32`, #223 `296eb5d` and #226 `eba4e80` at
  03:20:05-03:20:16Z. `git rev-parse review/phase1-rNN` equals the merged commit
  for every round 32-39, which is what AGENTS asks (the tag goes on the merged
  commit, not the branch). R40 itself can tick nothing: a tick lands in a
  review-doc PR, and #296 is that PR.
- **#296 is open and now has 14 ticks** (`96d279c`, two commits, created
  03:51:44Z, branch `review-housekeeping-ticks-2`): 15 insertions and 15 deletions
  in 6 docs, no code. The fourteenth (`R30` I105) came as a follow-up commit
  `96d279c` at 04:07:29Z and the title now reads 14.
- **Process note, one line: re-check a tick list against `main` when the PR
  opens.** The first 13 ticks were derived at 03:32:41Z; I105 became closeable at
  03:50:38Z when #294 merged, and its own rows already tracked merges up to
  #292 — so the gap was only the last 18 minutes, and 63 seconds after that
  merge the PR opened without it.
- **The subject slip: two merges lost the `#`.** `b33cc7a` ends `(291)` and
  `3eaf998` ends `(292)`; the other six read `(#288)`, `(#289)`, `(#290)`,
  `(#293)`, `(#294)`, `(#295)`. The cause is the reviewer's merge helper (it
  wrote `($n)` for those two), and it cannot be repaired without rewriting `main`'s history. The
  effect is narrow and worth remembering: `git log --grep='#291'` does not find
  the commit, so a tick or a round that greps by the `#` form misses these two
  and must also try `(291)`.
- **Three code claims I checked came out as the release notes say.** `cmd/main.go`
  always wires the prober (`cni.NewPodProber` at `:374-381`, `CNIProber: cniProber`
  at `:400`), so #293's "no shipped install changes behaviour" holds; no chart
  template reads `roleNamespaces` or `webhook.*` on `main`
  (`git grep -n 'roleNamespaces\|\.Values\.webhook' -- dist/chart/` → nothing;
  control `git grep -c 'Values.metrics' -- dist/chart/templates/` → 3 files), so
  #292's "two dead value groups" holds; and #291's two script arms match its note
  (no address → `UNRESOLVED` at `pod_prober.go:212-216`, refused connection →
  `UNRESOLVED` at `:222-225`, every other exception → `BLOCKED` at `:226`).
- **Release notes grew twice in the window**: #226 created
  `docs/RELEASE-NOTES-next.md` (815 lines) and #295 appended 18 lines for
  #291-#293. Note, not an item: the file's "as of `0070686` (= PR #269)" pin
  (`docs/RELEASE-NOTES-next.md:3-5`) now predates entries in the same file, so
  it understates what the file covers. Each entry cites its own squash hash, so
  no claim is wrong.
- **Runs.** `git fetch --prune`, then `git rev-parse` / `show` / `diff` /
  `merge-base` / `log --format --grep -S` / `grep` / `tag -r` on `origin/` refs,
  and `gh api GET` (pulls, reviews, comments, timeline, check-runs,
  actions/runs). Reads of `.samples/`, `AGENTS.md` and the docs above. Nothing
  built, nothing tested, no cluster touched, no checkout.
- **Read, not run:** the reviewer's mutation records `.samples/mut-290.md` and
  `mut-294.md` were not opened this round. For #290 I rely on the two diffs
  (`m1-drop-approval-hold.diff`, `m2-drop-nopin-branch.diff`) and my own reading
  of them in `.samples/prereview-290.md`; for #294 on the `f49b-*` files above
  and `prereview-294.md`, whose pre-image blobs (`feddab2`, `3bc7956`) I checked
  against both the branch tip and `main`. I re-ran no mutation and no test.

**Carried items, state at this cut** (nothing closed by this window, one now
meetable):

- **I144** (open): the `MemoryPVCUnbound` Event is still the only surface.
  `loop_p5a_memory.go:88`, `:92`, `:242`, `:282` still say one-shot / "once per
  Loop"; `docs/OPERATOR.md:357` and `docs/UPGRADE.md:214` still promise it; no
  memory condition exists (`git grep -c MemoryAvailable -- api/ internal/` → 0).
- **I145** (open): `AGENTS.md:285` still gives the plain `kubectl auth can-i
  <verb> <resource>` form, and `git grep -c resourceAttributes.subresource
  AGENTS.md` → 0.
- **I146** (open): `plantVerifyJob` → 0 sites; 20 bare `job = &batchv1.Job{`
  literals remain in `internal/controller/*_test.go`.
- **I147** (met): the acceptance's checkable clause holds for five code PRs (see
  Status). Hand it to the tick list with the sentence that the evidence is the
  per-tip check conclusions, not a `gh pr checks` paste.
- **I141, I142, I143** (open): no AGENTS norm for the evidence path, for grepping
  a cited name, or for naming a carrier. I150 is a fresh I142 instance; I149 is a
  fresh I143 instance.
- **D88** (open): not in `AGENTS.md` (`git grep -c D88 AGENTS.md` → 0).

## P1

- None. The window's two P1/P2 findings (#292's sample goal, #291's message) were
  both caught in review and fixed before the merge.

## P2

- None.

## P3

- [ ] **I148: #293 gave every spec reconciler a fake prober except one, and that one is the F49 no-pin spec**

  **Where:** `internal/controller/loop_s6_delivery_envtest_test.go:450`
  (`r := &LoopReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(),
  apiReader: k8sClient}`), inside the It that #290 added and #294 extended at
  `:485-502`. The gate it now trips: `cniNetworkStatus`
  (`loop_controller.go:1314-1322`, the nil seam #293 added) called from
  `sandboxSuspendedByGate` (`:2237-2238`).

  **Problem:** #293's change is that a nil `CNIProber` holds the sandbox
  `Suspended`, so the PR swept every reconciler literal that does not test the
  gate and gave it `CNIProber: testCNIProber()` — six in this file alone
  (`git diff 4869cb9 f07fee1 -- internal/controller/loop_s6_delivery_envtest_test.go`
  → 6 added lines), 84 files in total. This literal is the one it missed:
  `git grep -n '&LoopReconciler{' -- 'internal/controller/*_test.go'` returns 265
  openers; of the 18 that do not carry `CNIProber` on the opening line, 16 set it
  on a following line, one is the deliberate nil-prober spec
  (`loop_f51_nil_prober_envtest_test.go:47`), and one is this literal. No
  assertion changes today, and that is worth saying plainly: the spec's Loop is at
  `Succeeded`, where `sandboxOperatingMode` suspends the sandbox for the terminal
  phase anyway (`loop_p2f_paused.go:166-168`), and delivery does not depend on the
  sandbox. The cost is that the rule #293 encodes ("a spec that does not test the
  gate gets the fake prober") is untrue as written, and the next spec that reuses
  this reconciler at a running phase inherits a fail-closed hold it did not ask
  for.

  **Fix:** add `CNIProber: testCNIProber(),` to that one literal, as the other six
  in the file got. (The builder had this one-line fix uncommitted in its #293
  worktree; it is being folded into the I146 fixture-helper PR.)

  **Acceptance:** every `&LoopReconciler{` literal in `internal/controller/*_test.go`
  either sets a `CNIProber` or is the nil-prober spec, checked by grep; the S6
  focus stays green.

- [ ] **I149: the Auto-mode step's comment promises the phase and the spec never asserts it**

  **Where:** `internal/controller/loop_approval_envtest_test.go:286-287` ("the
  sandbox must run at the SAME phase — this is the step that fails if only that
  conjunct is deleted"), the assertion it introduces at `:292-293` (sandbox
  operating mode only), and the gate it mutates,
  `internal/controller/loop_approval.go:539-542`.

  **Problem:** the step's power rests on the phase not moving: `applyApprovalGate`
  returns early in Auto (`loop_approval.go:289-292`) and the spec's claim seam
  returns `nil, nil` (`:224-226`), so the Loop stays at `AwaitingApproval` and the
  mutated gate (`phase == AwaitingApproval`) still holds — which is why M-A fails
  at `:292`. Nothing in the spec asserts the phase, so the sentence is a claim the
  spec does not make. If a later change let Auto advance the phase from
  `AwaitingApproval` (an auto-approve, a replayed claim), the mutated gate would be
  false at the new phase, M-A would pass, and the step would still look green
  while proving nothing. That is R38 I142's class inside a mutation's premise, and
  it is the one note #294 shipped with (`Verdict: OK + notes`, "one unasserted
  comment (pi3's pre-review)").

  **Fix:** assert the phase after the flip's `settle()`:
  `Expect(got.Status.Phase).To(Equal(coxv1alpha1.LoopPhaseAwaitingApproval))`, with
  a fresh `Get`.

  **Acceptance:** the spec asserts the phase is still `AwaitingApproval` after the
  flip; `f49b-mA` still fails at `:292`; a mutation that makes Auto advance the
  phase fails the new assertion instead of passing silently.

- [ ] **I150: a comment names `describe.go` as the home of a literal six of whose fourteen callers live elsewhere**

  **Where:** `internal/cli/approval.go:35-37` — "The read-only `getNested*(u,
  "status", …)` helpers in describe.go keep the literal". The call sites on
  `main`: `describe.go:43`, `:44`, `:55`, `:67`, `:70`, `:75`, `:114`, `:128`
  (8) and `history.go:35`, `:36`, `:42`, `pause.go:65`, `redeliver.go:50`, `:54`
  (6), from `git grep -n 'getNested[A-Za-z]*(.*"status"' -- 'internal/cli/*.go'
  ':!*_test.go'`.

  **Problem:** the comment exists to explain why the `"status"` literal is kept
  after F87(d)/F87(e) introduced `statusSubresource`, and it names the wrong file
  set: six of the fourteen callers are not in `describe.go`, so a reader who greps
  that one file finds 8 of 14 and a reader who trusts the sentence looks in the
  wrong place for the rest. It shipped in #289, whose merged tree is the reviewed
  tree byte for byte — so this is #289's own pre-review P3, merged unfixed, not a
  merge-time change. Same class as R38 I142 case 3 and R37 I138: a comment that
  names a site that does not carry the work.

  **Fix:** name the set, e.g. "the read-only `getNested*` callers in `internal/cli`
  (`describe.go`, `history.go`, `pause.go`, `redeliver.go`) keep the literal", or
  drop the count and say "the read path's callers keep the literal".

  **Acceptance:** the sentence names a set that matches the grep;
  `git grep -n 'keep the literal' internal/cli/` names the four files.

## Owner decisions

**D87 is resolved, (a).** Every review round R32-R39 is on `main` with its tag on
the merged commit, and #296 is the housekeeping PR that ticks the closed items. So
the decision R37-R39 asked for has an answer and no successor is needed; R40
records it rather than asking again.

**D86's (a) acceptance is now met, and the decision itself is still unanswered.**
R35's D86 offered (a) accept the terminal mount failure and the runbook remedy, or
(b) add a retry path, and its acceptance for (a) was that `docs/UPGRADE.md` names
the behaviour change. `docs/UPGRADE.md:280-304` now does — the F48g no-run-key
section, `Failed:SandboxFailed`, and "delete and re-apply" — via #278 (`a9d8605`,
I136). The owner's choice between (a) and (b) is not recorded anywhere, so the
decision stays open on the record and answerable in one line.

**D88 stands.** It is not in `AGENTS.md` (`git grep -c D88 AGENTS.md` → 0). The
class recurred this window (I150) and the check it asks for is cheap.

**D72-D85 are unchanged.** No commit, PR body or doc in this window records an
owner answer: the only mentions are the rounds that asked and the two
"unchanged" carry-forwards (`docs/REVIEW-PHASE1-R36.md:78`,
`docs/REVIEW-PHASE1-R37.md:84`), and `config/rbac/loop_editor_role.yaml:28-46`
still grants `update`+`patch` on `loops/status` (D85).

**New: none.** The window added no decision that needs the owner; I148-I150 are
fixes, not choices.
