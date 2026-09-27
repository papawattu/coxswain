# Phase 1 review, round 4 — open issues

Review of `36f4b50`..`acd4d84` (2026-09-27), since tag `review/phase1-r3`.
Covers I25, I26, bookkeeping, and slice **B2** (TamperedVerify).

Each issue is self-contained so it can be picked up independently. Work
test-first as in `docs/TDD-PLAN-PHASE1.md`: write the failing test at the
named seam, then fix. Tick the box and add the commit hash when done. Issue
IDs continue project-wide so every ID is unique.

Priority: **P1** = fix before the next slice builds on it. **P2** = decide
during Phase 1. **P3** = cleanup, batch whenever.

## Verdicts on reviewed commits

| Commit | Issue | Verdict | Follow-ups |
|--------|-------|---------|------------|
| `36f4b50` | I25 rename `coxscaf` → `coxswain` | OK | — |
| `cf9c312` | I26 LICENSE | OK | — |
| `f8896fe` | bookkeeping (R3) | OK + note | I30 |
| `acd4d84` | B2 TamperedVerify | **CHANGES** | **D24 (P1)**, **D25 (P1)**, I28, I29 |

B2 gets the decision rule right — a non-zero tamper code is terminal
`TamperedVerify` regardless of any claim, with a pure test and an envtest.
Two gaps would carry straight into B3 and must be closed first.

---

## P1 — Blocking (before B3 builds on B2)

### D24. "No evidence" is indistinguishable from "clean"

- [x] Done (f6ab491)

**Where:** `api/v1alpha1/loop_types.go` `VerifyStatus.TamperExitCode int
\`json:"tamperExitCode,omitempty"\``; `tamperVerdict` in
`loop_controller.go`.

**Problem:** the zero value `0` means "clean", and it's also what the field
holds before any verify Job has run, after a Job that crashed before the
tamper container finished, or if status was lost. Once B3 reads this field
to decide `Succeeded`, **absence of evidence becomes a pass** — the classic
fail-open. `omitempty` on an `int` also drops an explicit `0` from the
serialized status, so an auditor can't tell "checked, clean" from "never
checked".

**Fix:**
1. Red: a Loop in `Verifying` with **no** tamper evidence must not be
   treated as clean — `tamperVerdict` returns "no verdict yet" and B3 can't
   reach `Succeeded`.
2. Green: make it `TamperExitCode *int32 \`json:"tamperExitCode,omitempty"\``
   (nil = no evidence). Only a value the operator copied from a **terminated**
   tamper init container counts; `tamperVerdict` returns a tri-state
   (`Clean` / `Tampered` / `Unknown`) and `Unknown` never advances. Same
   treatment for per-check results in B3 (`lastCheckResults` entries must
   distinguish "exit 0" from "not run" — ties in with I14's `NotRun`).
3. Record which verify Job/attempt the evidence came from
   (`status.verify.jobName` or in the history entry), so evidence from a
   previous iteration's Job can't be reused for the current
   `verifiedCommit`.

**Acceptance:** nil tamper evidence → no transition out of `Verifying`;
explicit `0` → clean; non-zero → `Failed:TamperedVerify`; a status carrying
evidence for a different `verifiedCommit` / Job is treated as nil.

### D25. The Go preset globs miss repo-root files under git's default pathspec

- [x] Done (cb7da6f)

**Where:** the Go preset (`**/*_test.go`, `**/testdata/**`, `go.mod`,
`go.sum` — `loop_types.go:80`) and whatever runs the tamper check's
`git diff --name-only <base> <verified> -- <globs>` (lands with B3's verify
image).

**Problem:** verified with a probe on a real repo: with git's **default**
pathspec, `**/*_test.go` matches `pkg/b_test.go` but **not** a root-level
`add_test.go`. Only `:(glob)**/*_test.go` matches both. Same for
`**/testdata/**` vs a root `testdata/`. A single-package Go repo keeps its
tests at the root — exactly where the round-8 `TestMain` fixture would go —
so the tamper check would pass it.

Relatedly, B2's "three anti-gaming fixtures" are currently **labels on a
pure function test**: `tamperVerdict(nonZero, true)` is asserted, but no
test runs a real `git diff` over a real repo containing those fixtures. The
test names claim coverage the code doesn't have yet.

**Fix:**
1. Put the tamper check in a small Go package (e.g. `internal/tamper`) —
   `Changed(repoDir, base, verified string, globs []string) ([]string, error)`
   — that the verify image's tamper init container runs as a binary. Always
   pass globs with `:(glob)` magic (or `:(glob,top)`), and expand the preset
   in one place.
2. Red: table test over a **real temp git repo**: (a) edit an existing
   root-level `*_test.go`, (b) add root-level `zz_test.go` with `TestMain`,
   (c) add a `replace` to `go.mod`, (d) the same under `pkg/`, (e) add
   `testdata/` at the root, (f) rename a protected file, (g) delete one, and
   (h) a clean change to a non-protected file. (a)–(g) non-empty, (h) empty.
   The current plain-pathspec behavior must fail (a), (b), (e).
3. Rename the pure tests to what they test ("non-zero tamper code is terminal
   regardless of claim"), and point the fixture coverage at the new package.

**Acceptance:** the real-repo table test passes; the preset expansion is
covered; no test name claims fixture coverage it doesn't exercise.

---

## P3 — Cleanup

### I28. `tamperVerdict` takes a claim only to ignore it

- [x] Done (f6ab491)

`tamperVerdict(tamperExitCode int, resultClaimsSuccess bool)` does
`_ = resultClaimsSuccess`. Accepting the claim invites a future edit to use
it. Drop the parameter; the "claim is ignored" property is better shown by
the envtest (runner claims done, evidence says tampered → `Failed`), which
already exists.

### I29. Say who can write `status.verify`

- [x] Done (3dbe4f9)

The evidence now lives in Loop status, so its integrity depends on nothing
but the operator writing `loops/status`. Checked at review time: the
scaffolded `loop_editor_role` and `loop_admin_role` grant `loops/status`
`get` only, so today only the manager can write it. Record that as an
invariant in ADR-0005 D12 ("only the manager role may write `loops/status`;
it holds verify evidence"), so a later RBAC edit doesn't quietly break it.

### I30. Attribution in the R3 bookkeeping

- [x] Done (3dbe4f9)

`REVIEW-PHASE1-R3.md` I27 says "deferred … (per owner: fold I27 into the
e2e work)". That instruction came from the reviewer, not the owner. Change to
"per reviewer". Decisions attributed to the owner should be ones the owner
actually made.

---

## Next round will check

- D24, D25 (P1) before any B3 code.
- B3 — verify outcome from Job pod `initContainerStatuses`; the happy path
  to `Succeeded` now lives here.
