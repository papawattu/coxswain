# Phase 0 review, round 9 — open issues

Review of `2622d62`..`ba9b2cd` (2026-09-26), since tag `review/phase0-r8`.
Baseline at review time: runner `go test -count=1 ./...` green, runner
golangci-lint 0 issues.

Each issue is self-contained so it can be picked up independently. Work
test-first as in `docs/TDD-PLAN.md`: write the failing test at the named seam,
then fix. Tick the box and add the commit hash when done. Issue IDs continue
from earlier rounds so every ID is unique.

Priority: **P1** = fix before Phase 1 starts. **P2** = decide during Phase 1
(design). **P3** = cleanup, batch whenever.

## Verdicts on reviewed commits

| Commit | Issue | Verdict | Follow-ups |
|--------|-------|---------|------------|
| `2622d62` | I13 runner nits | OK | — |
| `3acf804` | bookkeeping (I13, R1 scratch file) | OK | — |
| `b58a548` | D17, D18, D19, I14 | OK + notes | I15 |
| `ba9b2cd` | bookkeeping (R8) | OK | — |

ADR-0005 now carries the D17 residual-risk section and all three
mitigations, D18's restart semantics, D19's unconditional read-only checkout,
and the I14 note. CONTEXT.md's "only *automated* gate; human reviewing the
draft PR is the final gate" wording is right.

---

## P2 — Plan drift

### I15. `TDD-PLAN-PHASE1.md` doesn't have the D17–D19 slices the ADR promises

- [x] Done — commit `6f67407` (+ `SubvertedVerify` added to CONTEXT.md "Failure reason")

  - Added B3a (canary/negative control, D17 → `Failed:SubvertedVerify`, round-8
    probe as e2e fixture), B3b (restart semantics, D18 → `restartPolicy: Never`,
    `backoffLimit: 0`, single pod by `controller-uid` else `VerifyError`, re-run =
    new Job with attempt suffix), B3c (read-only checkout, D19 → every check
    container `readOnly: true` + own scratch `emptyDir`), and B4's `NotRun`
    history (I14). Slice order updated to B1-B2-B3-B3a-B3b-B3c-B4-B5-B6.
  - `grep -ic 'canary|backoffLimit|restartPolicy|read-only|readOnly|SubvertedVerify'` on
    the plan returns 16 (was 0).

**Where:** `docs/TDD-PLAN-PHASE1.md` (B3 and the slice order);
ADR-0005 says "B3 gains a slice for the canary" and lists restart-semantics
and read-only cases, but `b58a548` touched only the ADR and CONTEXT.md —
`grep -i 'canary\|backoffLimit\|restartPolicy\|read-only'` on the plan finds
nothing.

**Problem:** the plan is what gets built, test-first. Anything only in the
ADR won't get a red test.

**Fix:** add to the Phase 1 plan, each with its seam:
- **B3a — canary / negative control (D17).** Seam: the verify Job spec the
  operator builds (envtest: assert a canary check container exists, built from
  the trusted image, over a separate copy); plus the controller decision:
  canary container exit 0 ⇒ `Failed:TamperedVerify` (or `SubvertedVerify` —
  pick one and add it to CONTEXT.md "Failure reason"). Include the round-8
  probe (`init(){ if testing.Testing() { os.Exit(0) } }`) as the e2e fixture
  for the real verify image.
- **B3b — restart semantics (D18).** Seam: Job spec has `restartPolicy:
  Never`, `backoffLimit: 0`, `activeDeadlineSeconds`; controller: zero or two
  pods for the Job's `controller-uid` ⇒ `VerifyError`; `VerifyError` re-run
  creates a new Job (name includes an attempt suffix), never reuses one.
- **B3c — read-only checkout (D19).** Seam: every check container mounts the
  checkout `readOnly: true` and has its own scratch `emptyDir` for `HOME`,
  `GOCACHE`, `GOPATH`, `TMPDIR`.
- **History (I14):** B4's history assertion includes checks recorded as
  `NotRun` after the first failing check.
- Update the slice order line (B3 → B3a → B3b → B3c or fold into B3 as
  sub-cases).

**Acceptance:** each item above appears in the plan with a seam and an
acceptance line; the plan and ADR-0005 agree.

---

## Status

Design review of Phase 1's verify path is converged after I15. Remaining
before Phase 1 code:

- **I4 (P1) — owner decision.** Last blocker.
- I15 (plan drift, above).
- D5 (k8s version pin), D6 (snapshots on kind) — P2, not blocking B1–B3.
- Round-1 P3 batch — license headers, `maxIterations` default, `Ref`
  markers, `go mod tidy`, e2e suite, controller tidy-ups.

Suggested next for the builder: I15, then the round-1 P3 batch (it touches
`loop_types.go` and `loop_controller.go`, which B1 is about to change — land
it first to keep B1's diff clean), then D5.
