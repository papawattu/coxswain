# Phase 1 review, round 38: a cited evidence path that does not resolve, and comments that name the wrong site

Round 38, 2026-10-25. It follows R37 (branch `review-phase1-r37`, PR #276, not
merged) and covers the work merged since R37 was cut: **#277, #278, #279, #280**
plus six doc/process merges. Code line numbers are against `main` at `830741e`
(`830741e91f6276fa8331f2863e8b9643ad5382b7`) unless an item says otherwise. The
review was built from these input files, which are not committed:

- the input skeleton for this round (`.samples/r38-skeleton.md`);
- the pre-reviews of the four PRs (`.samples/prereview-277.md`, `-278.md`,
  `-279.md`, `-280.md`) and the draft-pass looks (`early-278.md`, `early-279.md`,
  `early-280.md`);
- the designs they followed (`.samples/design-f48e.md`, `design-f48e-refresh.md`,
  `design-f83e-f91b.md`) and the refresh for the next small items
  (`.samples/design-f49-f51.md`);
- the reviewer's mutation records (`.samples/mut-277.md`, `-278.md`, `-279.md`);
- R37 (`docs/REVIEW-PHASE1-R37.md`), the queue
  (`…/scratchpad/queue-b2-after-f30.md`), and the builder's evidence
  (`.samples/f48e-mut/`, `f83e-mut/`, `f83e-mut-r2/`, `f83e-verify/`,
  `f48e2-mut/`, `f48e2-verify/`, `worktree-rescue/`).

A bare `loop_*.go` or `loop_*_test.go` name is under `internal/controller/`.

| PR | Item | Merge |
|---|---|---|
| #277 | F84c: an unconditional terminal model-proxy clear, a stable read-error message | 01b7b28 |
| #278 | F48e + F90: the counters' reset rule, 0-based iteration docs, the accumulation specs | a9d8605 |
| #279 | F83e + F91b: the deliver-description pin guard, the dead Tampered field, the run-key comment, the reason length unit | 7251618 |
| #280 | F48e2: the accumulation spec asserts the recreated UID, the planAttempt comment is rewrapped | 830741e |

**Status:**

- **No P1. One P2, two P3s.** All four merges are the reviewed trees: #277's code
  and #278's code took empty diffs from their heads (`2965f84`→`01b7b28`,
  `c46d5b4`→`a9d8605`), and #279 (`d6eb88c`→`7251618`) and #280
  (`8088419`→`830741e`) have equal tree hashes (`c9c06d08…` and `484202a5…`). CI
  was green on all four (`lint` + `test`); I ran nothing.
- **The pre-review caught two defects before merge.** #279's draft run-key comment
  said the rejection re-plan "moves neither" `iteration` nor `desiredPhase`, while
  the rejection moves `desiredPhase` from AwaitingApproval to Planning (fixed in
  `9ea1c62`). #280's new comment named the wrong Get (fixed in `9accaeb`, the
  review's only CHANGES). #277 needed two
  CHANGES rounds; #278 one OK.
- **#279 closes two R37 items** — I138, and I140 on a **corrected acceptance**
  (the I135 precedent; the corrected text is below). The ticks are still blocked
  by D87.
- **Runs:** the reviewer re-ran mutations on each code PR in a scratch worktree
  (`.samples/mut-277.md`, `-278.md`, `-279.md`); #280's was read from its log. This
  round's text re-ran nothing: `make test`, `make lint` and the chart checks are
  taken from the builders' logs and CI.

**Carried items, state at this cut:**

- I133 (an unstorable reason wedges the write), I134 (the A2 fallback keeps the
  count), I135 (the guard is invisible to the comparator): **fixed** (`8b55779`,
  `5d44419`, `8b55779`); ticks blocked by D87.
- **I138 (the run-key comment explains a real behaviour with a claim that does not
  exist): fixed by #279 (`7251618`).** The phrase `PlanApproved claim` matches
  nothing at the tip and once at `a9d8605` (`loop_s4_phase.go`), and the comment now
  names both real ends of the episode (`loop_s4_phase.go:1337-1356`); S9 pins them
  in order (`loop_f48g_episode_boundaries_envtest_test.go:579-589` nothing has
  re-counted and the key *has* moved, `:591-614` the accepted claim clears the
  persisted 2). Tick against `7251618` once an R37 tick can land.
- **I140 (the guard's length limit counts bytes while the schema's counts
  characters): fixed by #279 (`7251618`) on a corrected acceptance.**
- I136, I137, I139: as R37.

**I140's acceptance, corrected.** R37 asked for "a spec with a multi-byte reason
[that] asserts the **stored** value — either the reason is stored unchanged (rune
count) or the message says the operator's bound, not the schema's". That cannot be
written: the reason schema is ASCII-only — `maxLength: 1024`, `minLength: 1`,
`pattern: ^[A-Za-z]([A-Za-z0-9_,:]*[A-Za-z0-9_])?$`
(`config/crd/bases/coxswain.wattu.com_loops.yaml:891-893`, copied in
`loop_controller.go:5514-5516`) — so for every reason the guard accepts, the byte
count and the rune count are the same number, and every input where they differ is
rejected by the pattern first. The acceptance becomes:

> The guard's unit is exact for every reason the schema accepts, and the tests pin
> **which term** rejects what: the 1024/1025 rows are length-only, and the
> kubelet-text and multi-byte rows assert both that `storableConditionReason` is
> false and that the regex is the term that rejects them
> (`loop_f89b_unstorable_reason_envtest_test.go:210-221`, the rows at `:219-220`).
> A mutation that loosens the pattern to `^.+$` must fail those two rows.

`M-I140-2` is that mutation and fails exactly those rows
(`.samples/f83e-mut-r2/M-I140-2.txt`). The item closes as "premise corrected, rule
pinned"; no rune count is required.

## P2

- [ ] **I141: the body cites evidence at a path that resolves only after someone copies it**

  **Where:** each builder PR ran in its own worktree — the logs say where: `.samples/worktree-rescue/f84c/evidence-277-round3-baseline.txt:2`
  (`…/coxswain-f84c/internal/controller`),
  `.samples/worktree-rescue/f48e/test.txt:1-2` (`…/coxswain-f48e/bin/…`),
  `.samples/f83e-verify/make-test-d6eb88c.log:1` (`…/coxswain-f83e/bin/controller-gen`),
  `.samples/f48e2-verify/make-test-e527ed4.log:64`
  (`make[1]: Leaving directory '/home/jamie/projects/coxswain-f48e2'`). The bodies
  cite `.samples/…` in the **shared clone**. Two cited paths did not exist there at
  review time: #277's round-3 evidence (`evidence-277-round3-*`, now only in
  `.samples/worktree-rescue/f84c/`; the shared `evidence-277-*-01b7b28` files were
  written after the merge), and #279's `.samples/f83e-mut-r2/` and
  `.samples/f83e-verify/` (written under `coxswain-f83e/.samples`; the reviewer
  copied them before merging). #280 wrote its evidence to the shared `.samples`
  before it was marked ready, after the builder was told to.

  **Problem:** each builder PR runs in its own worktree and writes its evidence to
  that worktree's `.samples`; the citation is written as if it were in the shared
  one. The pre-review cannot open the evidence, so the mutation and gate claims can
  only be taken on trust — the thing R28 I92 exists to prevent ("a cited path the
  reviewer cannot open is not an output"). Both cases were fixed by a
  copy after the review had already been written. #278's cited mutation diffs were
  in the shared `.samples/f48e-mut/` (the reviewer applied them from there).

  **Fix:** the builder writes evidence to the shared
  `/home/jamie/projects/coxswain/.samples/…` (or copies it there before marking
  ready), and the pre-review `stat`s every `.samples/…` path the body cites before
  giving a verdict. Put the rule in AGENTS beside the mutation rules — this is a
  clipboard rule today, enforced by whoever remembers.

  **Acceptance:** the AGENTS rule names the path and the check, and the next two
  builder PRs' pre-reviews each paste the `stat`/`ls` for every cited evidence
  path; none of the four in this round needs a post-review copy.

## P3

- [ ] **I142: a comment names a site, or a detector, that does not do the work**

  **Where:** three cases in one round.
  1. `loop_s4_phase.go:1337-1356` (fixed in `9ea1c62`) — #279's draft said the
     rejection re-plan is "a run change that moves neither" `iteration` nor
     `desiredPhase`. The rejection sets `desiredPhase = Planning` from
     AwaitingApproval in `loop_approval.go`, next to `PlanAttempt++`. (The text it
     replaced, R37 I138, named a "PlanApproved claim" that does not exist.)
  2. `loop_f48e_accumulation_envtest_test.go:236-241` (fixed in `9accaeb`) — the new
     comment said "the Get above fails if there is no Sandbox at all". The Get
     above is `f48eGet` (the Loop) at `:222`; the Sandbox read is `f48eSandbox`
     at `:242`.
  3. **Live, unshown:** `loop_controller.go:5506-5513` and `:5524-5526` credit the
     F89b envtest as the pattern's drift detector, including for a CRD that
     "loosens" the shape. No recorded run shows the envtest failing on a loosened
     CRD: its reasons are ASCII (`loop_f89b_unstorable_reason_envtest_test.go:43`,
     `:68`), and the operator substitutes before it writes. The F91b unit table's
     kubelet-text and multi-byte rows (`:219-220`) fail when the **Go** pattern is
     loosened (`M-I140-2`), which is a different drift.

  **Problem:** a comment about a guard that names a mechanism, a site, or a test
  that does not carry the claim. Every test passes; only a reader catches it. Two
  of these were caught by the pre-review one round apart (#279's early P2-1, and
  #280's clause that stopped the merge), so the check works — it just is not
  written down, and case 3 is in main today.

  **Fix:** reword case 3 so it claims only what a run shows: name which test
  catches a change to the Go pattern, and either show a run that catches a CRD
  drift or say none does. Make the
  check explicit: when a comment names a function, claim or test, the pre-review
  greps that name and confirms the site does the work.

  **Acceptance:** case 3's two clauses name the right detectors, and the next two
  PRs' pre-reviews record the grep for each comment that names a site.

- [ ] **I143: an assertion whose ability to fail is argued, not shown**

  **Where:** #277's deleting-path spec (mutation F left it passing; R3 pinned it
  with the reviewer's mutation E, `.samples/mut-277.md`); #278's
  `Expect(sandboxName(name)).NotTo(BeEmpty())` (`loop_f48e_accumulation_envtest_test.go:236`
  at `a9d8605`, un-failable by construction); #280's replacement at `:242-244`,
  whose only recorded mutation (`M-F48e2`, `.samples/f48e2-mut/M-F48e2.txt`) fails
  both specs at `:125` — one call before the new assertion — so no run shows
  `:243` failing.

  **Problem:** the same shape three rounds running: a spec assertion credited with
  a behaviour no recorded run shows it detects. #280 disclosed this plainly ("not a
  new detector"), which is the right thing to write — but the spec's power then
  rests on a neighbouring line, and a future reader cannot tell the two apart.

  **Fix:** for each new or changed assertion, record one mutation that turns *that
  line* red; when that cannot be arranged, say in the PR which neighbouring
  assertion carries the behaviour instead.

  **Acceptance:** the next PR that adds or changes an assertion records the failing
  line (or the carrier), with the command and its output.

## Owner decisions

**D87** stands, and this round adds a doc to the pile. The owner merged #204
(housekeeping, R7-R22), #211 (R30) and #217 (R31) on 2026-10-25. Still open are
#235, #246, #261, #267, #272 (R32-R36) and #276 (R37); the newest merged round is
R31 (`a30262e`). R38 can tick nothing itself, and now
carries five candidates: I133-I135 (`8b55779`, `5d44419`, `8b55779`) and I138/I140
(`7251618`).

**Decision asked:** unchanged from R37 — (a) merge the open review docs oldest
first and tick in one housekeeping PR (recommended), or (b) let a housekeeping PR
tick ahead of its round's doc against `main`'s hashes, which needs the owner's
reading of I57.

**Acceptance:** under (a), the open docs merge, R38 follows, and one housekeeping
PR ticks I133-I135 and I138/I140 with their squash hashes; under (b), the vehicle
PR is named and I57 records that a tick may precede its round's merge.
