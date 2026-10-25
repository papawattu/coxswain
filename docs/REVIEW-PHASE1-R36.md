# Phase 1 review, round 36: a reason the CRD cannot store, a count that survives a rebuilt workspace, and six rounds that never merged

Round 36, 2026-10-25. It follows R35 (PR #267, not merged) and covers the work
merged since R35 was cut (#268 to #271). Code line numbers are against main at
`545afee` (the merge of #268) unless an item says otherwise. The review was
built from these input files, which are not committed:
- the post-merge reviews of #269 (`.samples/review-postmerge-10.md`) and of
  #268 and #271 (`.samples/review-postmerge-11.md`);
- the pre-reviews of #268, #269 and #271 (`.samples/prereview-268.md`,
  `-269.md`, `-271.md`);
- the reviewer's mutation runs for #268, #269 and #271
  (`.samples/mut-268.md`, `-269.md`, `-271.md`);
- the reviewer's round notes for #268 and #271 (`…/scratchpad/rv268.md` and
  `rv271.md`, outside the repo);
- the design notes for the queued fixes (`.samples/design-f48h.md`,
  `design-f88-f89.md`);
- the inputs skeleton for this round (`.samples/r36-skeleton.md`).

A bare `loop_*.go` or `loop_*_test.go` name is under `internal/controller/`;
`describe.go` is under `internal/cli/`.

| PR | Item | Merge |
|---|---|---|
| #268 | F88: `cox describe` prints the wall clock; the condition column is sized | 545afee |
| #269 | F48g: the sandbox-failure count belongs to a run, not to a Sandbox | 0070686 |
| #270 | Docs: a rollback does not revive a `Failed:SandboxFailed` Loop | d3ccfad |
| #271 | F89: a sandbox failure with no kubelet reason wedged the whole status write | 14e019c |

**Status:**
- **No P1s.** Four merges, and each does what its body says.
- **#271 (F89) closed the empty-reason wedge.** The fallback is at the source
  and the guard covers a future empty reason. The reviewer's mutation with the
  guard off fails only the guard spec, so the source fallback is what carries
  the item (`.samples/mut-271.md:3-4`). One input is left open: a reason that is
  non-empty and still cannot be stored (I133).
- **#269 (F48g) closed postmerge-9's P1-1 and P2-1.** The count's key is the
  run. Its mutations show the key is tested (A fails S2 and S3) and the restore
  clears are isolated in round 2 (B2 fails S4b, `mut-269.md:9`). Its own review
  left two boundaries: the A2 empty fallback (I134, F48h) and the pause
  rationale in the `sandboxRunKey` comment (`loop_s4_phase.go:1329-1334`),
  which F48h already owns. The second is a comment, not a defect.
- **#268 (F88) landed clean.** `active:` prints with the cap or `(no cap)`, both
  headings are gated on a printed line, and the condition columns are sized from
  the conditions printed. The test diff only adds: the one removed `want` entry
  is re-added in the same hunk, so no assertion was weakened. Both mutations
  fail the spec they name (`mut-268.md:3-4`).
- **#270 merged (the rollback wording). The runbook names
  `status.agent.infraEpisode`** (`docs/TROUBLESHOOTING.md:542-543`). `docs/UPGRADE.md` does not, and D86's
  acceptance is unmet (I136).
- **Rounds:** #268 one (`Verdict: OK`; three pre-review notes folded in
  `59d9431`), #269 two (CHANGES, then OK + notes), #270 one (OK), #271 one
  (OK + notes; its notes are the queue's F89b).
- **Five items are new — I133 to I137; one owner decision, D87.** I128-I132 and
  D72-D86 stay open, and their state is in the bullets below.
- **The tick chain is stalled.** The newest merged round is R29 (`a02c61e`).
  R30 to R35 are open (#211, #217, #235, #246, #261, #267), and so is the
  housekeeping round (#204). A tick lands in a review-doc PR (AGENTS, I57), so
  nothing in R30-R35 can be closed while its round is open (D87).
- **The stray `rn/f48g` push lost nothing, and is recorded here rather than as
  an item.** Its commit is `fc286dda`, the F48g release-note commit, and it is
  an ancestor of #226's current head (`31eb2190`, which adds the F89 and F88
  notes). The server ref is gone and only a stale remote-tracking ref remains,
  which is why two clones can disagree about whether the branch exists. Folding
  it into I132 would rewrite another round's item text, so it stays a record.

**Carried items, state at this cut:**
- I128 (a credential store read, an unrecorded model switch): open. No newer
  record in the window's PR bodies or notes.
- I129 (a retry bound that could never trip): the fix merged (#266, #269); the
  acceptance still needs F48e, which has no PR. Not ticked.
- I130 (what F48b left behind): open; F48f has no PR.
- I131 (postmerge-8's five P3s): partly routed. #268 took P3-6, and the F88
  design note routes P3-7 to a proposed F91. The routing table R35 asked for
  still does not exist.
- I132 (process slips): open. I137 is the same family and has its own
  acceptance.
- D86 (a mount failure is terminal): open; I136 is its unmet half.
- D72-D85: unchanged in this window.

---

## P2

- [ ] **I133: a reason that is not empty and still cannot be stored wedges the same status write**

  **Where:** `loop_s4_phase.go:1569-1571` (the fallback tests `reason == ""`
  only); `:1552-1558` (the pod-level reason overrides the container's); the
  pass-through at `loop_controller.go:5441-5446` ("Non-empty values are NOT
  sanitised: the CRD pattern accepts every kubelet reason in use"); the CRD
  pattern `^[A-Za-z]([A-Za-z0-9_,:]*[A-Za-z0-9_])?$`
  (`config/crd/bases/coxswain.wattu.com_loops.yaml:886`); the repo's own example
  of a pod-level reason at `loop_s4_phase.go:1811-1812`.

  **Problem:** `pod.status.reason` is a free-text field. The API puts no enum,
  pattern or length on it (`k8s.io/api/core/v1`, `PodStatus.Reason`, a plain
  `+optional string`), and `sandboxFailure` copies it into the condition reason
  whenever it is set (`:1552-1558`). The CRD then requires the reason to match
  the pattern above, so a value with a space or a hyphen cannot be written. The
  code's own comment names one: "Low on ephemeral-storage"
  (`loop_s4_phase.go:1811-1812`). The CRD validates
  `status.conditions[].reason` against that pattern on every status write, so a
  value that fails it fails the update; `14e019c` records the same mechanism for
  the empty case, and the chain that follows: no `AgentFailed`, no
  `status.agent.infraAttempts`, no UID dedupe, and the Sandbox recreated without
  end.

  **What I could not verify:** I did not reproduce a kubelet writing such a
  value. This review is read-only, there was no cluster and `librarian` was
  unavailable, so there is no upstream citation either. The item is the code
  path, not the frequency.

  **Fix:** F89b's first half, already queued: check the reason against the
  pattern in `sandboxFailure` and fall back to `podFailedReason` when it fails,
  keeping the raw text in the message (the message has no pattern). Or scope the
  pass-through comment to the fields it is actually about.

  **Acceptance:** the envtest named in the queue line (`pod.status.reason:
  "Low on ephemeral-storage"` plus a terminated agent container with no reason)
  fails today at the status update and passes after. The comment at
  `loop_controller.go:5441-5443` is then either true or scoped.

- [ ] **I134: the A2 empty-workspace fallback rebuilds the workspace and keeps the count**

  **Where:** `loop_restore.go:1065-1082` (the `b2KindLostPVC` default arm);
  `:1077` (`Restore=False/NoUsableCheckpoint`), `:1078` (`persist`), `:1081`
  (return). The two clears F48g added are at `:869` and `:951`, both on a
  `Restored` completion. The rule is stated at `:861-868`: "the workspace the
  pre-restore run failed against no longer exists".

  **Problem:** this arm runs when the PVC is gone and no checkpoint of the Loop
  is usable. It recreates the PVC empty and continues in the same reconcile,
  which runs before the phase machine (`:749`). `iteration` and `desiredPhase`
  do not move, so the run key cannot see the rebuild, and no clear is called. A
  count of 2 that a pre-restore run accumulated makes the first failure in the
  rebuilt workspace attempt 3; 3 > 2 is terminal, so the Loop fails
  `Failed:SandboxFailed` with no retry (`sandboxInfraRetries = 2` at
  `loop_s4_phase.go:1016`, the test at `:1646`). That is the class F48g exists
  to close, at a third completion site.

  **Fix:** F48h (§1): add `clearSandboxInfraEpisode(loop)` at `:1077` with an
  S4c spec, or state at the site why this completion keeps the count. Whichever
  is chosen should match the merged runbook sentence, which promises the reset
  for a completed rollback (`docs/TROUBLESHOOTING.md:542-543`) and so does not
  settle the empty fallback.

  **Acceptance:** the chosen rule is covered by a spec that fails when the clear
  is removed (the F48h M-E shape), or the site says why not. The runbook
  sentence and the code comment agree.

---

## P3

- [ ] **I135: the new reason guard is invisible to the one comparator that reads a stored reason**

  **Where:** `loop_controller.go:1332-1339` (`networkEnforcedChanged`), its
  callers at `:5264-5267` and `:5353-5356`, and the guard at `:5441-5446`;
  `internal/cni/cni.go:50` (`ReasonUnknown = "Unknown"`).

  **Problem:** `setCondition` rewrites an empty reason to `Unknown` after the
  comparison's input has been taken. `networkEnforcedChanged` compares the
  stored reason to the caller's raw one, and its caller emits an Event when they
  differ. If an empty reason ever reaches that seam, the first pass stores
  `Unknown` and the second compares it to `""`, so the Event fires on every
  reconcile. The substitute is also the probe's own `Unknown`, so an operator
  cannot tell the two apart.

  **Reach:** only a custom `Prober`. The in-repo probers return typed constants
  (`internal/cni/cni.go:35-50`), so this is constructible and not observed.
  postmerge-11 rates both halves P2 and the queue folds them into one P2 item
  (F89b); this half sits at P3 because nothing is lost, only repeated.

  **The other comparators are safe (checked):** `setMemoryWriterCondition`
  compares stored to stored, before and after its own `setCondition` call
  (`loop_memory_writer_job.go:870-893`). The checkpoint and restore conditions
  bypass `setCondition` entirely (`meta.SetStatusCondition` at
  `loop_checkpoint.go:408`, `:512`, `:553`, `loop_restore.go:707`), so the guard
  does not reach them; all four pass constants today.

  **Fix:** one `normalizeReason` used by both `setCondition` and the
  comparator, or normalise at the two call sites. Either is a few lines.

  **Acceptance:** a stubbed `Prober` returning an empty reason, two reconciles,
  and one `NetworkEnforced` Event in total.

- [ ] **I136: the release surface for a terminal mount failure and the new status field**

  **Where:** `docs/UPGRADE.md` (no `SandboxFailed`, `infraAttempts` or
  `infraEpisode` line; its last section is `## Stricter Secret-name validation
  (F48)` at `:258`); D86's acceptance in R35; the new field
  `api/v1alpha1/loop_types.go:1017-1028`; the runbook entry that does name it
  (`docs/TROUBLESHOOTING.md:542-543`).

  **Problem:** two gaps. D86's acceptance asks `docs/UPGRADE.md` to name the
  behaviour change from #266, that a mount wedge now ends `Failed` and needs a
  delete-and-re-apply. UPGRADE does not. F48g also added an optional status
  field and moved the retry count from "since the last accepted claim" to "per
  run", and F48e's queue line already carries an UPGRADE line for the field. The
  release *notes* are current — #226 carries entries for #269 (`:228-229`),
  #271 (`:234`) and #268 (`:258`) — and the runbook now covers the operator
  remedy. So the missing surface is UPGRADE, the file an operator reads before
  installing.

  **Fix:** one UPGRADE section for #266 and #269: the terminal mount failure and
  its remedy, and `status.agent.infraEpisode` as an optional new field. Then
  D86's acceptance is met.

  **Acceptance:** `docs/UPGRADE.md` names the terminal failure, the re-apply
  remedy and the field; D86 can be ticked, or the owner drops the acceptance.

- [ ] **I137: a mutation that removes two same-shaped sites cannot say which one a spec proves**

  **Where:** `.samples/mut-269.md:4-5` (round 1: B1, the lost-PVC clear removed,
  fails S4; B2, the rollback clear removed, **passes**, "no spec") and `:9`
  (round 2 at `4055ddc`: B2 fails S4b); the #269 body's mutation section, where
  M-B2 removed both clears ("removed 3 restore clears") and reported one
  failure, S4; AGENTS.md `:216-217` (a named mutation must be applied exactly)
  and `:295-298` (a mutation claim names the diff, the command and the output).

  **Problem:** `loop_restore.go:869` and `:951` are two lines of the same shape
  in one file. The builder's mutation set removed them together and the suite
  answered with one failure, so the answer could not say which clear S4 proves;
  one of the two had no spec at all. The reviewer's per-site runs did say: B1
  failed S4 and B2 passed, which is what found the gap and produced S4b. The
  body's section header also names one worktree (`a78cfce`) for runs on two
  trees, which is visible in its own outputs (`Ran 5 of 588` for M-B2,
  `Ran 6 of 589` for M-B1, the second tree being the one with S4b).

  **Fix:** a norm beside the two existing mutation rules: when two sites share a
  shape, mutate one site per run, and record the tree and the spec count each
  run used. I132's family, separate acceptance.

  **Acceptance:** the norm exists in AGENTS.md, and the next PR whose diff has
  two same-shaped sites shows one-site mutations with a tree and a count per
  run.

---

## Owner decisions

**D87** below is new. D72-D86 remain open (D72-D74 in R29, D75-D84 in R30-R32,
D85 in R34, D86 in R35).

**D87: six review rounds are unmerged, so no tick can land.**

  **Where:** the open review-doc PRs #211 (R30), #217 (R31), #235 (R32), #246
  (R33), #261 (R34), #267 (R35) and the housekeeping round #204. The newest
  merged round is R29 (`a02c61e`, PR #200). The tick rule is AGENTS, "Closing an
  issue (I57)": a tick joins a review-doc PR (a later round or a housekeeping
  commit) once the fix has merged, and it cites the merge's squash hash.

  **Problem:** R30 to R35 carry I103 to I132 and D75 to D86. Each new round
  restates the same carry-forward table, because no round can be ticked while
  its own doc is open, and #204 covers R7-R22 only. The cost is visible in this
  round: I129's fix merged two rounds ago and the item is still open, so a
  reader cannot tell an open defect from an open bookkeeping item.

  **Decision asked:** (a) merge the six rounds oldest first, then tick the
  closed items in one housekeeping PR (recommended), or (b) keep one round open
  as the tick vehicle and merge the rest into it. Either way the owner merges:
  review docs are owner-merge only.

  **Acceptance (if (a)):** the six review docs are merged, R36 follows them, and
  one housekeeping PR ticks every item whose fix has merged, with its squash
  hash. **If (b):** the round names the single vehicle and the same ticks land
  there.
