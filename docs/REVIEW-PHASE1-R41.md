# Phase 1 review, round 41: the F92 P2 is fixed across twenty-two merges, and twelve P3s are what is left

Round 41, 2026-10-26. Drafted by pi3 (the read-only pre-reviewer) and verified by the reviewer.
It follows R40 (`docs/REVIEW-PHASE1-R40.md`, PR #297 at `41f32d9` — still open, so R40's tag is not
applied yet; see I158) and covers the **twenty-two merges** after `2cd3040` (R40's last merge):
**#298-#319**. Code line numbers are against `origin/main` at `33595e6`
(`33595e644e5e878c81b5515976eacc29a9cf9e0e`, #319) unless an item pins a sha, and the citation
convention is the one this round is arguing for in I155: a number is as read at the sha named in the
item or the nearest preceding `at <sha>`, with the current `main` line in parentheses where it has
moved. That convention is audited line by line in `.samples/r41-selfcheck2.md`; the first pass,
`.samples/r41-selfcheck.md`, found five wrong citations (a commit-range off-by-one, a moved range, a
wrong function, a wrong doc line and a false "the bullet is still there") — all fixed here. A bare
`loop_*.go` / `loop_*_test.go` is under `internal/controller/`.

The review was built from these input files, which are not committed: the skeleton
`.samples/r41-skeleton.md`; the pre-reviews `.samples/prereview-{298,303,308,311,313,314,316,318}.md`
and the early looks `.samples/early-{313,314,316,318}.md`; the design notes
`.samples/design-f56-f57-f70.md` and `.samples/design-f58-f61.md`; and the evidence they cite
(`.samples/f57-verify/`, `.samples/f57-mut/`, `.samples/f59/`, `.samples/f61/`, `.samples/f70/kind/`,
`.samples/f70-mut/`, `.samples/f92-kind/`, `.samples/f92-mut3/`, `.samples/f92-verify2/`). Facts the
pi3 role could not verify read-only (anything on `kind-coxswain`) say so explicitly.

| PR | Item | Merge / state |
|---|---|---|
| #296 | housekeeping: tick 14 closed items in R15 and R30-R37 | **open** (docs) |
| #297 | review(phase1-r40) — the round plus the seven citation fixes | **open** (docs) |
| #298 | I146: one `plantVerifyJob` helper for every verify-Job fixture | `534af86` |
| #299 | I144: the memory PVC degradation re-announces on the quiet window | `36c3684` |
| #300 | Docs: release notes for #299 (I144) | `09341ae` |
| #301 | I149/I150: assert the phase the Auto flip leaves alone, name the `getNested` callers | `6a18beb` |
| #302 | F52 (restore P3s): the CRD-absent restore reason, a real `changed` flag | `b05c819` |
| #303 | F52 (runner/CLI P3s): the import stores what its name hashes, unknown phases idle | `f534314` |
| #304 | Docs: release notes for #302 | `a23455a` |
| #305 | Docs: release notes for #303 | `c515513` |
| #306 | F53: the verify checkout is read-only to the checks, and the verify Job has a deadline | `08307a8` |
| #307 | Docs: release notes for #306 | `d2135d0` |
| #308 | F92: a missing KubeArmorPolicy CRD is surfaced on the Loop | `7fd171d` |
| #309 | Docs: put F84c's release-notes bullets back under F84c (R41 I153) | `6a0407b` |
| #310 | Docs: release notes for #308 (F92) | `b00eb33` |
| #311 | F56: the deploy render derives the four component-image args, and `RunnerAvailable` | `64f4902` |
| #312 | Docs: release notes for #311 (F56) | `30e61e8` |
| #313 | F57 (restore P3s): an unconfirmable PVC is not a restore, and the fork label stops being re-patched | `3a99f2e` |
| #315 | Docs: release notes for #313 (F57) | `221e012` |
| #314 | F70: a completed fork's sandbox is not held for a workspace-init clone it will never run | `d9f5619` |
| #317 | Docs: release notes for #314 (F70) | `b98f4c9` |
| #316 | F59: delete the verify Job comment that claims an advisory scan exists | `5af3467` |
| #318 | F61: ship the sample AgentPolicies ADR-0007 promises | `46a470a` |
| #319 | Docs: release notes for #318 (F61) | `33595e6` |

**Status:**

- **No P1 and no open P2 at `33595e6`.** The one P2 the window carried — F92, the defect #306's own
  kind run exposed — is fixed and merged as `7fd171d`; the twenty-two merges add no P1 and no new P2,
  and every remaining item below is a P3. Of the fourteen items, **two are fixed by the window**
  (I151 by #308 `7fd171d`, I153 by #309 `6a0407b`) and are recorded with their fixes so the round can
  tick them with the squash hashes; two more are **partly closed** (I160's F56 arm by #311, I162's F70
  bullet by #317), and twelve remain open.
- **Every merged PR carries its squash hash in the table**, re-checked with
  `git log -1 --format='%s' <sha>` for all twenty-two. Merge-tree equality, verdict timestamps, CI
  conclusions and the per-PR review trails are in the skeleton (*per skeleton*); this draft re-checked
  the line-level claims in each item plus the pairs it names (for #313, `git diff 3a99f2e 827c65d` is
  empty, so the squash is the reviewed tree; for #311, the squash's tree is `03b1b70` except
  `docs/RELEASE-NOTES-next.md`, where it kept #310's F92 entry — a two-dot `git diff b00eb33 03b1b70`
  therefore shows an apparent −6 F92 deletion that the merge did not make).
- **#296 and #297 are still open docs PRs**: #296 ticks items whose cited hashes the skeleton verified
  as ancestors of `main`; #297 carries R40 itself. Neither can be reviewed here as merged code, and
  #297 being open is why the `review/phase1-r40` tag does not exist yet (I158).
- **#308 merged after two review rounds** (`a7942a1` → `3f2b448` → `faaf980` → `7fd171d`): the body
  names the final tip `faaf980`, its mutation set at that tip is M1-M5 plus M6/M6b with a named
  assertion and `EXIT=1` each (`.samples/f92-mut3/`), `make test`/`make lint` are `EXIT=0`
  (`.samples/f92-verify2/`), the before/after kind pair has a positive control in each
  (`.samples/f92-kind/`), and the cluster was restored to a `main` build with `--allow-unenforced`
  (`12-restore-main-flag.txt`). It also edits `loop_p5a_memory_script_test.go`'s two
  `reconcileRequeueAfter` call sites to follow the new parameter; no assertion changed. Its one
  acceptance shortfall is in I151.
- **#311 merged as `64f4902`** (`03b1b70` was its merge-time head), with the round-3 verdict's two
  one-clause residues still open — recorded under I155 (the body's `e3cada3`→head sentence) and I160
  (the `docs/TROUBLESHOOTING.md:466` opening). Pi3's three-round pre-review is
  `.samples/prereview-311.md`.
- **#312 (`30e61e8`, docs-only, `1 file changed, 6 insertions(+)`)** adds the F56 "Other changes"
  entry; per I161 it overstates the chart path, and per I162 the "Known issues still open" list still
  names F56.
- **#313 (F57) merged as `3a99f2e`** — its merge-time head was `827c65d`, the round-2 recheck tip,
  and `git diff 3a99f2e 827c65d` is empty, so the squash is the reviewed tree (3 files, +430/−45
  against `30e61e8`: `loop_restore.go` +88/−43, `loop_f57_restore_envtest_test.go` +332,
  `loop_f36_restore_envtest_test.go` +10/−2). Pi3's round-2 verdict suggestion was **OK**, and the
  reviewer reproduced **M1, M4, M5 and M6**. The two round-2 residues are recorded as **I163** (the
  rollback's F57a arm is unspeced) and **I164** (the merged body's "Mutations — all at the branch tip
  `b06b6ce`" heading, which M6 at `827c65d` falsifies). Full trail: `.samples/early-313.md` (draft look
  at `07f2512`), `.samples/prereview-313.md` (round 1 at `b06b6ce`, round 2 at `827c65d`).
- **#315 (`221e012`, docs-only, `1 file changed, 6 insertions(+)`)** adds the F57 "Other changes"
  entry (`docs/RELEASE-NOTES-next.md:220-224` at `221e012`; `:258-262` at `33595e6`). Every claim in it
  resolves at `221e012` — the two no-decision arms (`loop_restore.go:889`, `:1001`), the two
  transition-gated `Rejected` sites (`:896`, `:1008`), the unfiltered confirm-read log (`:517`), and
  the F36 spec re-pin — so the entry needs no correction; but per I162 the "Known issues still open"
  list still names F57, in its umbrella bullet (`:787-791` at `221e012`; `:822-824` at `33595e6`).
- **#314 (F70) merged as `d9f5619`** — ready tip `a629288`; the pre-review
  `.samples/prereview-314.md` suggested **OK + notes**, and the two early-look asks (tip logs, the kind
  run) were both closed, with `make-test-a629288.log` / `make-lint-a629288.log` at the head and a kind
  pair showing the fork release while its egress proxy stays `ImagePullBackOff`. Two residues: the
  pre-F70 contract in `docs/TROUBLESHOOTING.md:541-546` and `docs/UPGRADE.md:201-206` (both still say
  the sandbox waits for the egress proxy, with no fork exception — re-read at `33595e6`), and the
  marker-evidence limit the body declares (the gate trusts `restoreCompletedForSpec`, not
  `workspacePVCPresent`; `loop_restore.go:961` at `33595e6`), which needs a follow-up item so it is not
  lost. Its "known issues" bullet is **gone**: #317 deleted it (it was
  `docs/RELEASE-NOTES-next.md:756-758` at `221e012`), so I162's F70 instance does not exist
  (`git grep -n F70 … | awk -F: '$2>726'` at `33595e6` prints nothing).
- **#316 (F59) merged as `5af3467`** (2 files, +1/−9, no code path; pre-review
  `.samples/prereview-316.md`, **OK + notes**). Both sites are the design's: the B3d paragraph and its
  blank `//` separator deleted from `internal/controller/loop_verify_job.go`, and `CONTEXT.md:61`
  re-worded to "**Not implemented at `d9f5619`; planned (ADR-0005 D17.2).**"; the plan documents
  (`docs/adr/0005…`, `docs/TDD-PLAN-PHASE1.md`, the R10/R11 docs) are deliberately untouched. One
  residue, a docs nit: `CONTEXT.md:61` pins a commit sha that goes stale on the next merge and is the
  only glossary entry that does — drop it in the docs PR that carries I162/I159.
- **#318 (F61) merged as `46a470a`** — ready tip `93459e4`; the pre-review
  `.samples/prereview-318.md` suggested **OK + notes**. Two inert `AgentPolicy` samples
  (`config/samples/policies/{default-deny,go-toolchain}.yaml`), their kustomization entries, one
  `docs/OPERATOR.md` paragraph (`:697`) and a 162-line envtest spec that validates through the
  apiserver (the CRD's CEL rejects an in-cluster network target) and asserts the kustomization both
  ways. Residues, none blocking: ADR-0007's "the README sample must ship with a minimal `AgentPolicy`"
  stays literally unmet (the body says so — README has no YAML to attach one to), the spec's
  `filepath.Join` versus kustomize's `/` is a Linux-only assumption, and its focus log header names
  `a8eb83a` with a working-tree note while the head's `make test` is the proof.
- **#319 (`33595e6`, docs-only)** adds the F61 release-notes entry. **Recorded: the stale-branch
  re-cut.** The entry was drafted by pi1 on a branch pushed from a stale base, which also re-added the
  already-merged F70 entry; the builder cherry-picked only the F61 commit onto current `main`, so the
  merge is the F61 entry alone (additions only, `docs/RELEASE-NOTES-next.md:220-250` at `33595e6`).
  The re-cut is visible in the body and nothing from the stale branch landed — but it is the second
  docs PR this window whose diff had to be re-cut against `main` (#317's predecessor is the other),
  which is I155's class applying to docs branches rather than PR bodies.
- **A review was posted on #316 in error** — the reviewer's `Verdict: OK`, meant for #317, landed on
  #316 at 17:45Z and was then edited to "Posted in error, please disregard … meant for #317". No inline
  comments, no PENDING review (`gh api …/pulls/316/reviews --jq '[.[]|select(.state=="PENDING")|.id]'`
  → `[]`), so #316 was **not** reviewed by that comment; the real review was the pre-review above.
- **The #314 kind run changed a cluster-wide setting and put it back**: to exercise snapshots it
  unmarked `standard` (`rancher.io/local-path`) as the default so `hostpath` (`hostpath.csi.k8s.io`)
  became the cluster's only default StorageClass (`sc-before.txt`, `storageclass-unmark-list.txt`,
  `storageclasses-after.txt`), and reverted it in `29-sc-revert-default.log` (hostpath's default
  annotation removed, `standard` re-annotated, verified by a before/after `kubectl get storageclass`
  and a per-SC default read). The residue is the snapshotter/CSI fixture, which the body discloses;
  the only outstanding ask is that the body say the default was reverted.
- **The `e86f704` `make test` failure was the reviewer's `go clean -cache`, not the code** (the
  reviewer's account). The clean overlapped builder #2's run —
  `.samples/f57-verify/make-test-e86f704.log:67` (`could not import time (open
  ../../.cache/go-build/…: no such file or directory)`), cascading vet/build errors, `:158` `EXIT=2`.
  The green run at the next commit (`.samples/f57-verify/make-test-827c65d.log`: `ok …
  internal/controller 463.505s`, `coverage: 87.5%`, `EXIT=0`) contains `e86f704`, so nothing rests on
  the failed run. The reviewer no longer cleans the Go cache while builder #2 is working. Attribution
  is the reviewer's; pi3 ran nothing.
- **The R40 tag is still missing**: `git tag -l 'review/phase1-r4*'` → `review/phase1-r4` only (round
  4, `6b4deb0`). That is expected while #297 is open (D87 puts the tag on the merged commit); R41 must
  check it once #297 merges, and the glob must not match the r4 prefix.
- **Corrections to the skeleton this draft makes after re-reading `main`**: (a) the skeleton's
  "#299 guard comment names a symbol that does not exist" is **wrong** — `modelProxyHold` exists
  (`loop_controller.go:2047` at `7fd171d`, `:2015` at `d2135d0`) and does refuse a terminal phase
  (`:2055`, `:2023` at `d2135d0`); the live nit is only the Paused half (I152). (b) The skeleton's
  "#301 N1: the test comment lacks the scope" is **wrong at `d2135d0`** —
  `loop_approval_envtest_test.go:286-290` now carries "…at the new phase and let M-A pass while
  proving nothing". Both are dropped as findings.


## P1

- None merged. (For calibration: the window's sharpest code defect, F92, is a P2 — it leaves the
  object empty but runs nothing, so no fence is bypassed.)

## P2

- [ ] **I151: a missing `KubeArmorPolicy` CRD leaves every Loop with an empty status (F92, fixed by #308 — `7fd171d`)**

  **Fixed by #308 (`7fd171d`), merged 12:50Z: tick this box with that hash when the round merges.**
  **Every line number in this paragraph is at `7fd171d`** (the `33595e6` values are in the parenthesis
  below, and the Where block's are at `d2135d0`). The merged shape is the one this item's Fix asked for.
  The tolerance is exactly `meta.IsNoMatchError`
  (`loop_controller.go:5376-5377` at `7fd171d`); the tolerated branch logs and sets `kaptCRDMissing` (`:5381`) and
  falls through to the condition half; the message names the CRD whenever the write path saw it and
  the reason is not already `EngineUnavailable` (`:5433-5434`); the Warning Event is gated on
  `(rs == ReasonEngineUnavailable || kaptCRDMissing)` on the transition (`:5450`); and the bounded
  re-read is `kaptCRDMissingRequeue = time.Minute` (`:1420-1430`), threaded out of
  `applyEffectivePolicyAndConditions` (`:5466`) through its single call site (`:904`) into
  `reconcileRequeueAfter` (`:1083`), guarded on the terminal phase by `f92HeldRequeue`
  (`:1416-1418`). The acceptance's reason question is answered by a spec rather than argued: S3
  (`loop_f92_kubearmor_silent_test.go:175-223`) drives the kind shape (fake reason
  `NodeNotEnforcing`, message must name the CRD, exactly one Event, and a second reconcile that does
  not re-announce); S1 pins `Phase == Planning` (`:171`), the "no phase for ever" half; S6
  (`:283-337`) pins the terminal guard at the call site and inside the helper. (All of these are linear
  in this file: at `33595e6` the F92 group is `:5472-5477`, `:5530`, `:5546`, `:5562`, `f92HeldRequeue`
  `:1423-1425`, `kaptCRDMissingRequeue` `:1437`, call site `:911-914`; the spec file is unchanged.) Mutations M1 (restore
  `return err`), M2 (widen the tolerance), M3 (drop the Event), M5 (drop the flag threading) and
  M6/M6b (drop the terminal guard, either site) each fail a named assertion with `EXIT=1` at
  `faaf980` (`.samples/f92-mut3/`, every log carrying `# sha: faaf980…`). Kind record:
  `.samples/f92-kind/` — `11-before-shape.txt` (the empty `status: {}`) and `06-loop-final.txt`
  (condition, one Event, `Suspended` sandbox) with a positive control in each,
  `12-restore-main-flag.txt` for the `main` build restored afterwards.

  **One acceptance shortfall to record:** the Acceptance above names "M4 (drop the transition
  gate)", but the builder's M4 is the requeue instead (`# M4: delete the bounded kaptCRDMissing
  requeue`, `.samples/f92-mut3/M4.txt`), so **no mutation drops the transition gate** — the gate is
  asserted (S1 `:157-160`, S3 `:218-224`) but mutation-untested. Everything else the Acceptance
  asked for is present.

  **Where (as found at `d2135d0`, before the fix — these lines moved at `7fd171d`):**
  `internal/engine/kubearmor_enforcer.go:315-322` (the loud
  `get KubeArmorPolicy …: KubeArmor CRD not installed (and --allow-unenforced is not set)` error, `%w`-wrapped
  on `:322`) → `applyEnforcementWrites` `loop_controller.go:5254-5256` (only `ErrForeignKapt`
  is mapped, `:5239-5243`) → `:5307-5311` returns it **before** the condition half at
  `:5312-5358` → `Reconcile` `:904-906` returns it before any status write (`:911-914` at `33595e6`).

  **Problem:** on a cluster without the KubeArmor CRD and without `--allow-unenforced` (what
  `config/default` ships, `config/manager/manager.yaml:63-71`), every Loop keeps `status: {}` —
  no phase, no condition, no Event — while the failure lives only in the operator log. The
  `PolicyEnforced=False` branch and the D30 hold (the clause is `:2182-2184` at `d2135d0`,
  `:2214-2216` at `7fd171d`) are both downstream of the failing
  write. Found by the reviewer on kind during #306's run; this breaks the
  `AGENTS.md` R26 I90 norm and the hold `README.md:5` promises. **Correction the fix must carry:**
  on a CRD-absent cluster `Enforcing` returns `ReasonNodeNotEnforcing`, not
  `ReasonEngineUnavailable` (`kubearmor_enforcer.go:369-373` lists the BPF-labelled nodes first and
  returns before the Kapt read at `:404-413`; existing specs pin that order —
  `loop_a1_static_envtest_test.go:211`, `internal/engine/kubearmor_a1_static_test.go:170-200`), so
  a fix that surfaces the cause only on `ReasonEngineUnavailable` shows nothing on this shape. Full
  reading: `.samples/early-308.md` (pi3).

  **Fix:** tolerate exactly the no-match at `:5308` (`meta.IsNoMatchError`; apimachinery v0.37.0
  uses `errors.Is`, so it sees through the enforcer's wrap), let the existing condition half and
  the D30 hold run, key the CRD-absent Warning Event and the message on the tolerated no-match (not
  on the reason), and schedule a bounded re-read so installing KubeArmor is picked up without an
  operator restart (no KubeArmorPolicy watch is registered when the CRD is absent at start —
  `loop_controller.go:6082-6085` at `d2135d0`, `:6181-6184` at `7fd171d`, `:6277-6280` at `33595e6`).

  **Acceptance:** a spec that pins the observed reason (`ReasonNodeNotEnforcing`) as well as a
  negative control where a Forbidden write still errors; mutations M1 (restore `return err`),
  M2 (widen the tolerance), M3 (drop the Event), M4 (drop the transition gate) each failing a named
  assertion; and a kind run showing the Loop's real `PolicyEnforced` reason, whether the Event
  fired, a `Suspended` sandbox, zero reconcile errors — with a `main` build restored afterwards.

## P3

- [ ] **I152: the I144 terminal guard does not cover Paused, and both docs say it does**

  **Where:** `isTerminalPhase` (`loop_controller.go:6616-6618` at `7fd171d`, `:6712-6714` at `33595e6`,
  `:6504-6506` at `d2135d0` — Succeeded/Failed only) used by the memory re-announce guard (`loop_controller.go:1905`
  at `7fd171d`, `:1973` at `33595e6`, `:1873` at `d2135d0`; the shifts are #308's insert and then F56's;
  no behaviour change); the
  promise it contradicts at `docs/OPERATOR.md:357` and `docs/UPGRADE.md:214` ("while the Loop runs
  and the PVC stays unbound") — those two lines are unchanged by #308.

  **Problem:** a Paused Loop is not terminal, so it keeps the requeue and re-announces
  `MemoryPVCUnbound` every quiet window while parked — which is not "while the Loop runs" in the
  sense the two docs promise (a paused Loop does no work and cannot fix the PVC). #299 fixed the
  Succeeded/Failed half and amended both docs to the "while the Loop runs" wording.

  **Fix:** decide the Paused case explicitly — either exclude `loopPaused` in the guard's condition
  or say "while the Loop is not paused or finished" in both docs.

  **Acceptance:** one sentence in both docs matching the code, or a spec asserting a Paused Loop
  schedules no re-announce window; whichever is chosen, a mutation on that clause fails it.

- [ ] **I153: the I144 release-notes section sat inside the F84c entry, orphaning F84c's own bullets — fixed by #309**

  **Fixed by #309 (`6a0407b` = `6a0407ba…`), merged 12:12Z: tick this box with that hash when the
  round merges.**
  The reviewer's #300 cleanup removed a heading pi1 had added to re-head F84c's bullets; #309 puts the
  I144 section after them. At `6a0407b`: `docs/RELEASE-NOTES-next.md:300` the F84c heading, `:302`
  "What was wrong", `:303` "What changed", `:304` "On upgrade", blank line, then `:306` `### I144`. (At
  `33595e6`: `:356`, `:358`, `:359`, `:360`, `:362`.)

  **Where (as found at `d2135d0`):** `docs/RELEASE-NOTES-next.md:300` (the F84c heading, #277), `:302`
  (F84c's "What was wrong"), `:303` (`### I144 …`, #299's entry), `:309` (F84c's "What changed").

  **Problem:** #300 inserted the I144 section between F84c's "What was wrong" and its "What
  changed"/"On upgrade" bullets, so those bullets rendered under the I144 heading and the F84c entry
  described a defect it no longer explained. It shipped in three further docs merges (#304, #305,
  #307) unchanged, and #309 removed it in a docs-only commit (`+3/−3`, one file).

  **Fix:** move the I144 section after F84c's bullets, with a blank line before the heading — done in
  #309.

  **Acceptance:** `git show main:docs/RELEASE-NOTES-next.md` shows F84c's three bullets adjacent
  under its own heading and `### I144` after them, with the blank line (`6a0407b:300-306` holds all
  three).

- [ ] **I154: four comments still describe the crashloop that C2 removed**

  **Where:** `loop_i74_sandbox_terminal_test.go:200` ("…and crashloops"), `:215` ("the crashloop"),
  `:219` (the failure message "would crashloop on the terminal desired-phase") and
  `loop_p2f_paused.go:161-162` ("the one-shot runner would crashloop on the terminal
  desired-phase" — the sentence is on `:161`) — the test file's three lines are unchanged at `33595e6`.

  **Problem:** the runner now idles on an unknown/terminal desired phase (#303's C2), so the
  behaviour these four sentences describe as current no longer exists; the third is a test failure
  message an operator could read. pi3's pre-review of #303 found the first three sites plus this
  file and round 2 fixed the ones it named, leaving these four (*per skeleton*:
  `.samples/prereview-303.md`).

  **Fix:** four comment edits (past tense, "idles now"); no behaviour change.

  **Acceptance:** `git grep -n 'crashloop' -- internal/controller` finds no sentence describing it
  as current behaviour.

- [ ] **I155: two PR bodies still point at a superseded tip, and one of them hides the round's own gate**

  **Where:** #299's merged body — `gh pr view 299` body `:93` ("tip `6a94f16`"), `:103` ("all four
  were run at the final tip"), `:108` ("Focus green at the tip `6a94f16`") — against the merge
  `36c3684`, whose reviewed tip was `8fb0703`; #298's body (`gh pr view 298` body `:107`, `:114`,
  `:160`) cites the pre-`5ae5d4c` line numbers and a baseline the named log does not contain
  (*per skeleton*: `.samples/prereview-298.md`).

  **Problem:** #299's round-3 tip added the terminal guard, its spec and mutation M5, and the body
  never moved — so the record of the PR's own closing round (and the mutation that guards the
  P2 it fixed) exists only in `.samples/i144/` and the two reviews. #298's numbers are one commit
  behind its merged tree. This is the AGENTS R25 I84 / R28 I92 class, now with enough instances
  (six across the window — five per the skeleton, plus #311's below) to state as a rule rather than
  six findings.

  **Sixth, harmless instance — #311's merged body (`03b1b70`, body `:127-129`):** "Between those shas
  only `README.md`, `docs/TROUBLESHOOTING.md` and the F56 spec file's `By` string differ" is true of
  `309d8cb`→`03b1b70` (+17/−9 over exactly those three files) but false of `e3cada3`→`03b1b70`
  (8 files, +99/−19). Nothing rests on it — every mutation was re-run at the head — and a merged body
  cannot be corrected retroactively, so this is recorded, not to be fixed.

  **Fix:** none retroactively possible in a merged body — record the class, and require the next
  builder PR bodies to name the final tip and re-number their citations after the last push.

  **Acceptance:** the next three builder PRs' bodies name the merge-time tip and every cited line
  number resolves at that tip (checked by the reviewer at the ready mark).

- [ ] **I156: the F53 deadline evidence is a unit seam for the no-pod case, and the message names the build constant**

  **Where:** `loop_f53_verify_isolation_envtest_test.go:370-379` (the "no pod at all" case calls
  `verifyOutcome(nil, 1, false, true)` directly, not through `Reconcile`);
  `loop_verify_job.go:1966-1968` formats the deadline message from
  `verifyJobActiveDeadlineSeconds` (`:1861`, 900s), called at `:2083` where the Job is in scope
  (`verifyInfraFailureMessage` is declared at `:1962`) — all as of `d2135d0`; **#316's own −8 shifted this
  file** (now `:1958-1960`, `:1853`, `:2075`, `:1954` at `33595e6`), so re-anchor these before the round merges.
  The spec file's `:370-379`, `:257`, `:291`, `:326` are unchanged.

  **Problem:** every other deadline shape is driven through `Reconcile` (`:257`, `:291`, `:326`),
  so the no-pod decision is pinned only at the unit seam — the shape where the pod was GC'd or
  evicted is exactly the one nobody will re-drive by hand. And the message asserts the operator's
  build constant, not the value on the Job that actually fired (they differed in #306's own
  mutation run, where the constant was rebuilt at 60).

  **Fix:** one Reconcile-level no-pod deadline spec; read the value from the Job's
  `spec.activeDeadlineSeconds` at the call site.

  **Acceptance:** the new spec fails when the deadline branch is removed; the message quotes the
  Job's value (mutation: build the Job with a different value and the message follows it).

- [ ] **I157: the kind evidence blocks say the cluster was restored; four test Loops from three runs are still on `kind-coxswain`**

  **Where — four objects, three runs, all still present:**
  - `samples/f53-hang` — `Failed` (#306's run, the one whose verify Job hit the deadline); `gh pr view 306`'s
    item-6 restore paragraph says "The f53-* Loops are deleted; gitea stays".
  - `samples/f92-kubearmor` — **`Planning`, sandbox `Running`** (#308's run). F92's kind record says the
    sandbox is created and held `Suspended` (`11-before-shape.txt` is the empty-status half,
    `06-loop-final.txt` the held half), so this object is not the held shape: either it is a Loop from the
    run's before/mutation half that was never deleted, or the hold on it changed after the record was taken.
    The evidence cannot tell which — the object's own description can.
  - `f48b-mount/f48b-mount` — `Failed` (older run, no PR body in this window claims it).

  Observed by the reviewer's read-only `kubectl get loop -A` on `kind-coxswain` at **15:47Z**. **Not verified
  by pi3** — this role runs no `kubectl`; the observation is the reviewer's, and the contradiction is between
  that observation and the merged bodies' restore paragraphs. Cause unknown.

  **Problem:** the evidence block is the only durable record of a kind run; if the cleanup sentence is wrong, a
  reader cannot tell whether the cluster is in the state the body describes. Three runs in a row leave their
  objects behind, so this is a norm rather than one typo: the restore paragraph names the operator image, not
  the objects. `f92-kubearmor` is the one with live impact — a `Running` sandbox means pods, a workspace PVC
  and (if the shape creates them) proxy objects still on the cluster, and a Loop at `Planning` that no
  evidence block accounts for. It is also the shape a later reviewer tests the F92 hold against, so a stale
  object is a trap, not just clutter.

  **Fix:** none in code. Delete the four test Loops and record the command with its output;
  `kubectl delete loop -n samples f53-hang f92-kubearmor` and the `f48b-mount` object in its own namespace. If
  one will not delete, record its `deletionTimestamp`/`finalizers` (the `f53-hang` case is the one where a
  finalizer is the plausible cause). Then make "restored" in a PR body mean the objects as well as the image.

  **Acceptance:** `kubectl get loops -A` on `kind-coxswain` shows no Loop left by a review's kind run (or the
  reply names the ones that cannot be deleted and why, with `deletionTimestamp`/`finalizers`), the delete
  command and its output are in the record, and each merged body that claims a restored cluster names that
  command.

- [ ] **I158: the R40 tag is not applied yet**

  **Where:** `git tag -l 'review/phase1-r4*'` at `d2135d0` → `review/phase1-r4` (round 4,
  `6b4deb0`) only; #297 is open at `41f32d9`, not merged.

  **Problem:** D87 puts each round's tag on its merged commit, so this is a carry-forward, not yet
  a defect — but the glob also matches round 4, which is the same prefix trap R40 warned about.

  **Fix:** tag `review/phase1-r40` on #297's squash the moment it merges; use the exact name.

  **Acceptance:** `git tag -l 'review/phase1-r40'` names the squash commit of #297, and
  `git rev-list -1 review/phase1-r40` is that commit.

- [ ] **I159: the F92 TROUBLESHOOTING sentence states the held Loop's phase as universal**

  **Where:** `docs/TROUBLESHOOTING.md:896-897` at `7fd171d` (`:930-931` at `33595e6` — F56's entry above it
  moved the section): "`kubectl cox describe` shows the Loop at
  `Planning`, and — unlike the pre-F92 shape …".

  **Problem:** `Planning` is the *fresh* Loop shape. The phase advance is
  `if loop.Status.Phase == LoopPhasePending { … = LoopPhasePlanning }` (`loop_controller.go:677-684`),
  so a Loop that was already at `Implementing`, `Verifying` … when the CRD went missing keeps that
  phase under the same hold, and the sentence is then wrong. The one spec that pins the phase uses a
  fresh Loop (`loop_f92_kubearmor_silent_test.go:171`), so nothing catches the wider claim. Recorded
  as a residual note in pi3's round-2 pre-review of #308 (`.samples/prereview-308.md`) and left
  unfixed at the merge.

  **Fix:** scope the clause — "a fresh Loop shows `Planning`" (or "the Loop's phase is unchanged; a
  fresh Loop shows `Planning`"). Docs only, no behaviour change.

  **Acceptance:** the sentence says which shape it describes, and a reader can check it against the
  phase advance at `loop_controller.go:677-684` and S1's fresh-Loop spec.

- [ ] **I160: a transition-gated Warning re-fires on every retry when the pass errors before the status write**

  **Where — three sites, one mechanism** ("a surface sets the condition in memory and emits on the *observed*
  transition; a pass that writes no status leaves the transition unrecorded, so the next pass observes it
  again"):
  - **F56 `RunnerUnavailable`** (merged as `64f4902`; these citations were re-read at `30e61e8` and are
    unchanged): `surfaceRunnerAvailable` is called at `loop_controller.go:902`; the gate is
    `was := conditionFalse(…)` (`:1535`, helper `:1551-1558`) and the
    Event fires when `was` is false (`:1538-1541`). The enforcement write returns the unmapped error
    (`applyEnforcementWrites` `:5354-5356`; only `ErrForeignKapt` is translated, `:5339-5342`), so
    `applyEffectivePolicyAndConditions` returns at `:5444-5446` and `Reconcile` at `:911-914` — before any
    status write, so the condition never persists.
  - **F92 `PolicyEnforcementUnavailable`** (merged, `7fd171d`): the Event gate is
    `(rs == ReasonEngineUnavailable || kaptCRDMissing) && (prevStatus != status || prevReason != rs)` with
    `prev*` read from the *fetched* status (`priorPolicyEnforced`, `:5388-5396` at `309d8cb`, called `:5471`,
    gate `:5518`; `:5450` at `7fd171d`). The tolerated no-match path falls through and the caller persists, so
    that shape is genuinely one-shot; an error at `:923-926` (restore), `:938-940` (bootstrap), `:1019-1020`
    (memory writer) or `:1030-1032` (deliver) drops it and the retry re-emits.
  - **F84 `ModelProxyNotReady`**: `surfaceModelProxyNotReady` gates on `conditionTruePresent` (`:2178`) and
    emits at `:2180-2186`, reached from the D35a gate inside `ensureSandbox`, so any error after that step
    loses it the same way.

  **Problem:** the code already knows the shape and treats "they retry" as the remedy —
  `clearModelProxyHoldForTerminal`'s comment (`:2189-2197`) says "Only the two genuine error returns
  (applyEffectivePolicyAndConditions, the restore step) leave it in memory, which is correct — they retry".
  That is right for a *clear* (idempotent) and wrong for a *transition-gated Warning*: the retry re-observes
  the same transition and emits again, so "one Warning per transition, not per reconcile" (F84's release note,
  the F92 `docs/TROUBLESHOOTING.md` section, and F56's `(not repeatedly)` — the claim F56's own caveat at
  `docs/TROUBLESHOOTING.md:468-471` now qualifies) is false for a Loop
  whose every pass fails before the status write. **Bounded, not a storm** (quantified in
  `.samples/prereview-311.md` finding 3): retries are controller-runtime's per-item exponential (5ms → …
  → 1000s cap; `cmd/main.go` sets no `RateLimiter`), and `mgr.GetEventRecorderFor("loop-controller")`
  (`cmd/main.go:421`) aggregates identical Events into one object with a rising `count` — so the cost is one
  PATCH per retry and a misleading count, with no status churn and no fence impact.

  **Fix — two options, either closes it:**
  **(a) Wording** (what #311 took for F56, merged in `64f4902`/`30e61e8`): one clause wherever the claim
  appears — "one per *observed*
  transition; a Loop whose pass fails before the status write re-observes it on each retry".
  **(b) Persistence:** write the changed status before returning the error. The machinery is already in
  `Reconcile` (`condsBefore` `:885-886`, `conditionsChanged` `:1054`), the precedent is in-file (the
  foreign-Kapt branch returns `r.Status().Update`, `:5342` at `30e61e8` / `:5370` at `33595e6`), and the
  proxy/NetworkPolicy stage already returns exactly that changed flag (`ensureProxyAndNetPolicies`,
  `:5629-5649` at `30e61e8` / `:5657-5677` at `33595e6`) — note `applyEffectivePolicyAndConditionsBestEffort`
  returns no value at all, so a persistence fix threads the flag out of it rather than reusing one. With (b)
  the gate becomes correct for every shape and (a) is unnecessary.

  **Acceptance:** for (a), the three claims read as above and no text says "not per reconcile" unqualified.
  For (b), one spec per surface in the F92-S2/F56-S5 shape (non-enforcing enforcer + `ApplyEnforcementWritesFn`
  returning `apierrors.NewForbidden`, two reconciles each `Expect(err).To(HaveOccurred())`): the Warning is
  emitted **once total**, plus a control where the second pass succeeds and still emits zero.

  **F56's instance is closed; the F92 and F84 instances are not.** At `30e61e8` the caveat is in three
  places — `docs/TROUBLESHOOTING.md:468-471` ("one per observed transition, not one per reconcile. (A pass
  that fails before its status write re-observes the shape on every retry …)"), the spec's `By(…)`
  (`loop_f56_runner_available_envtest_test.go:150`), and the PR body. **One clause of that section is the
  residue:** `docs/TROUBLESHOOTING.md:466` still opens "The Loop carries `RunnerAvailable=False` with reason
  `RunnerImageUnset`, and a Warning … Event was emitted", while `:468-471` covers the shape where the
  condition is never recorded; one clause would join them ("the condition is recorded once a pass reaches
  its status write; the Events appear either way") in the docs PR that carries this item.

  **Attribution:** confirmed from the code by pi3 at `309d8cb` (read-only, nothing run) while pre-reviewing
  #311 — `.samples/prereview-311.md` finding 3. The F56 citations were first read at `309d8cb` and re-checked
  at `30e61e8`: `:902`, `:1535`, `:1538-1541`, `:1551-1558`, `:5339-5342`, `:5354-5356`, `:5444-5446`,
  `:911-914`.

- [ ] **I161: #312's F56 release note says the Helm chart derives the four component images from `IMG`; it does not**

  **Where:** `docs/RELEASE-NOTES-next.md:223` at `30e61e8` (`:267` at `33595e6`) — "Every production render (`make deploy`,
  `dist/install.yaml`, the Helm chart) now derives the runner and the three proxy images from `IMG` — same
  registry+path prefix as the manager image, `coxswain-<component>`, IMG's tag".

  **Problem:** true of the two kustomize renders — `make deploy`'s `hack/render-deploy.sh` and
  `dist/install.yaml`'s `hack/build-installer.sh`, both through `hack/image-args.sh` (port-safe tag split,
  untagged/digest refusal) — and false of the chart. The chart's four args carry a hard-coded registry
  (`dist/chart/values.yaml:39-42`: `ghcr.io/papawattu/coxswain-runner:placeholder` and the three siblings);
  the release workflow only stamps the **tag** (`sed -i "s/:placeholder\b/:${GITHUB_REF_NAME}/g; …"`,
  `.github/workflows/release.yml:94-99`); `make helm-deploy` sets only `manager.image.repository` and
  `manager.image.tag` (`Makefile:697-698`); and the render guard asserts **presence**, not derivation
  (`hack/image-args-test.sh:131-138` — "the chart ships the four args with :placeholder defaults that its
  values override"). So `make helm-deploy IMG=localhost:5000/coxswain-controller:v1` puts the manager on that
  registry while the four components still name `ghcr.io/papawattu/…` — F56's shape, on the one render path
  the design note put out of scope (`.samples/design-f56-f57-f70.md:85-86`: "the chart's `values.yaml` image
  knobs are F45 territory. Keep this PR to `config/` + the `hack/` guard + README").

  **Fix:** one clause in the release note — the two kustomize renders derive the args from `IMG`; the chart
  ships them as `:placeholder` defaults whose tag the release workflow stamps, and a chart user on another
  registry overrides `manager.args`. (Whether the chart *should* derive them is F45's question, not this
  item's.)

  **Acceptance:** the sentence names which renders derive and which does not, and a reader can check it
  against `dist/chart/values.yaml:39-42`, `.github/workflows/release.yml:94-99` and
  `hack/image-args-test.sh:131-138`.

- [ ] **I162: three fixed items are still listed under "Known issues still open"**

  **Where:** `docs/RELEASE-NOTES-next.md` at `221e012`, all under `## Known issues still open` (`:688`; `:726`
  at `33595e6`) whose lead-in says "In plain words, the items still open" (`:712`; `:750` at `33595e6`):
  - `:737-738` F53 — "the verify step's code folder is writable by the agent's own code between checks, and the
    verify step has no time limit";
  - `:739-740` F56 — "a standard deployment ships no runner image and unpullable proxy images, so the agent
    quietly runs 'do nothing'";
  - `:787-791` the umbrella "**F50/F57/F52**: small and low items — …" bullet, which still names F57 after #315.

  (At `33595e6` those three are `:775-776`, `:777-778` and `:822-824` — every docs insert above them moved the
  list; #317's F70 entry and #319's F61 entry account for the last two shifts. F70 is **not** in the list:
  #317 deleted its bullet.)

  **Problem:** all three are fixed in this window — F53 by `08307a8` (its release note at `d2135d0` says "Each
  `check-*` container now mounts `/verify` read-only … The verify Job now runs with
  `activeDeadlineSeconds: 900`"), F56 by `64f4902`/`30e61e8`, F57 by `3a99f2e` (its own entry at `:220-224` at
  `221e012`, and at `:258-262` at `33595e6` — F61's and F70's entries took `:220` and `:252`),
  yet the bullets stand, because each docs PR is a six-line insert that never prunes this list
  (`git show --stat d2135d0 30e61e8 221e012` → "1 file changed, 6 insertions(+)" three times). A reader of one
  file therefore sees F56 and F57 reported as both fixed and open. The umbrella bullet is the weakest of the
  three (it bundles F50/F52 leftovers with F57), so dropping "F57" from its name may be the whole fix there.

  **Fix:** delete the two bullets and drop F57 from the umbrella's name (or re-cut the umbrella) in the docs PR
  that carries this item; extend the closed-items enumeration in the lead-in paragraph if the round wants that
  too.

  **Acceptance:** no bullet under `## Known issues still open` names a fixed item:
  `git show main:docs/RELEASE-NOTES-next.md | sed -n '688,$p' | grep -nE 'F53|F56|F57'` prints nothing (or only
  the F50/F52 leftovers, with F57 gone from the umbrella's name), and each item's own `###` entry remains the
  only other place it is named.

- [ ] **I163: the rollback's F57a arm has no spec, so deleting it would keep the suite green**

  **Where:** `internal/controller/loop_restore.go:1001-1004` at `221e012` (the rollback branch's
  `errors.Is(err, errWorkspacePVCUnconfirmed)` → `return false, true, true, nil`). The round-2 spec that covers
  this branch (`loop_f57_restore_envtest_test.go:228-264`, the "ROLLBACK create that keeps failing" `It`) drives
  the **create-failure** path only — its fake client fails `Create` with a plain error and its two assertions are
  the Warning's one-then-zero (`:257`, `:263`) — and the F57a table (`:145-164`) drives the fork path.

  **Problem:** the rollback's unconfirmed arm is the one part of F57a that round 2 touched but did not pin. By
  reading, no spec drives it (nothing injects `confirmErr` on the rollback shape), so deleting those four lines
  would leave all seven specs green and the rollback would record `Restore=False/Rejected` instead of no
  decision. That is not a fail-open — the sentinel comes from the helper, so `Restored` is never recorded — which
  is why this is a P3. It is the arm mutation M6 stops one step short of. **pi3 ran nothing; the acceptance below
  is what turns this reading into a claim.**

  **Fix:** a second entry in the rollback spec (or the F57a table) with `confirmErr` injected on the rollback
  shape: `changed=false`, `requeue=true`, no Restore condition, no PVC.

  **Acceptance:** the new assertion fails when `:1001-1004` is removed; record the mutation and its log.

- [ ] **I164: #313's merged body says every mutation ran at `b06b6ce`; M6 ran at `827c65d`**

  **Where:** `gh pr view 313` body `:75` ("## Mutations — all at the branch tip `b06b6ce`, one site each"),
  against M6, which the body's own review-round section places at `827c65d` (`:101-102`), and the M6 log
  (`.samples/f57-mut/M6-827c65d.log`, `# sha: 827c65d…`).

  **Problem:** M1-M5 did run at `b06b6ce`, M6 did not; nothing rests on the heading (the round section names
  M6's sha and the log carries it), and a merged body cannot be corrected retroactively. Same class as I155,
  with the narrow addition that a heading saying "all" should be dropped when a later round moves the tip.

  **Fix:** none retroactively; for the next builder PR, name the sha per mutation (or drop the "all" heading)
  when a review round adds one at a later tip.

  **Acceptance:** the next builder PR body's mutation table names each mutation's tip, or its review-round
  section does (as #313's does), and no heading claims a single tip for all of them.

## Owner decisions

**D89 (new, and already shipped): how a permanently-misinstalled engine should present.** F92 left
the owner one choice and #308's shape depended on it; #308 merged as `7fd171d` implementing **(a)**, so
what the round needs is the owner confirming (a) or redirecting, not a fresh build. Options: **(a)**
hold the Loop `Suspended` with the cause on the object and re-read on a bounded window, so installing
KubeArmor recovers every held Loop within that window (what `7fd171d` does: a `kaptCRDMissing` flag
threaded out of `applyEffectivePolicyAndConditions` `:5333-5466`, a one-minute `kaptCRDMissingRequeue`
`:1420-1430`, and the CRD cause named even when the observed reason is `NodeNotEnforcing`
`:5433-5434`); **(b)** hold,
surface, and require an operator restart or an unrelated event to recover; **(c)** fail the Loop
terminally once the CRD is known absent. **Recommendation: (a)** — it keeps `README.md:5`'s
hold promise, makes the cause visible without the log, and costs one minute of reconcile per held
Loop. **The decision is the owner's** because (c) contradicts the D30 fail-closed-hold semantics
and (b) contradicts the D30 recovery a restart-free deploy relies on.

**D88 stands** — `git grep -c D88 -- AGENTS.md` at `d2135d0` prints nothing, so the norm is still
absent and the class recurred (I155).

**D84 is still open** (F74/F77 depend on it): D84's own box at `docs/REVIEW-PHASE1-R32.md:180` is unticked
(`:100` is I113's box, a different item) and R33-R37 each list D72-D84 as open. Not exercised by this window's
merges.

**D86 is still unanswered** (*per R40*): R40 records that its (a) acceptance is met and the choice
between (a) and (b) is not written down anywhere.

**New: D89 only** — and it no longer gates a fix: #308 merged (a). No other item in this window needs
a choice rather than a fix.
