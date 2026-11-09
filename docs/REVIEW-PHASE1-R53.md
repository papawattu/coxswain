# Phase 1 review, round 53: the review backlog is cleared; Phase 6 is done but for C1

**Scope:** main 96bd609 (R52) → fcd71ce, 11-09 ~08:00Z-11:00Z. Merged in this window:
- #446: D120 decided, plus the ticks;
- #447: I219;
- #448: I249's relay test.

R52's addendum covers #439-#445.

## Closed

- **R48 I219** (`769c6ee`): a stall entry named `-verify-0`, but the Job was `-verify-1`. Two functions named the verify Job, and only the one that creates it mapped iteration 0 to 1 (F90). The stall gate now uses that rule.
- **R52 I249** (`1d73ff9`, `fcd71ce`): `Holder.Forget` prunes the learn summary and the snapshot, and a unit test with both mutations shows it.
- **R49 I221, I222, I228 and R51 I236:** ticked in #446. I221 and I222 are closed on kind-ref run 14 evidence, I228 on D120.

**State:** every Phase 6 slice is merged except C1, which needs the owner's push (`workflow` scope). The open boxes left in R45-R52 are slip records, whose acceptance is "does not recur", plus I206, I207, I211 and I213 (homelab k3s and C1).

## P2

- [ ] **I256: the reviewer passed a computed head sha to the guarded merge, twice**

  **Where:** the merges of #443 and #447 (`scratchpad/overnight-20261009.md`).

  **Problem:** the merge function's guard compares the PR's live head with the sha it is given. For #443 that sha came from `$(gh api …/pulls/443 --jq .head.sha)`, and for #447 from `$(git rev-parse origin/fix/…)`. Both read the live head, so the guard compared the head with itself and could not catch a push made after the review. In both cases the deleted ref showed the reviewed commit (`was a1f817e`, `was 0adb777`), so nothing unreviewed merged. The second time was after the first had been logged.

  **Fix:** the merge call takes the literal 40-character sha copied from the review run's output, never a command substitution.

  **Acceptance:** no merge in R54's window uses a command substitution for the sha.

## P3

- [ ] **I257: a stall test fixture names a Job that can no longer exist**

  **Where:** `internal/controller/loop_p2e_stall_test.go:47`, `p2eStallSeededJob0 = "lp-verify-0"`.

  **Problem:** after I219, no `-verify-0` Job exists (F90). The fixture still works as a distinct prior-Job name, but it is described as "a prior-iteration Job".

  **Fix:** rename it to a reachable prior name (for example `lp-verify-2`, with the gate at iteration 3), or say plainly that it is a synthetic distinct name.

  **Acceptance:** the fixture's comment matches what it is.

## Owner decisions

**Decided in this window:**
- **D120** (11-09): close R49 I228 on "no overflow, stable paths, dry-run valid".
- **R52** merged and tagged (11-09).

**Open:**
- **D121:** built and merged on the overnight approval (#445). The owner may reverse it.
- **C1:** the owner pushes `slice/c1-multiarch` from `../coxswain-c1`.
- **Phase 7:** design first. The Judge sidecar (D95) and the out-of-band canary channel (D97) need a plan and the owner's decisions before any slice.

## What I did not verify

- The learn summary's class-ordered eviction on kind (run 14 dropped 0).
- Whether KubeArmor's `matchDirectories {recursive: true}` also covers the directory itself.
- The C1 images on arm64.
