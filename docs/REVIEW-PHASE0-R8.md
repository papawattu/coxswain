# Phase 0 review, round 8 — open issues

Review of `2645f8f`..`42341f5` (2026-09-26), since tag `review/phase0-r7`.
Docs-only round: ADR-0005 and `TDD-PLAN-PHASE1.md` revised for round-6
D14–D16.

Each issue is self-contained so it can be picked up independently. Work
test-first as in `docs/TDD-PLAN.md`: write the failing test at the named seam,
then fix. Tick the box and add the commit hash when done. Issue IDs continue
from earlier rounds so every ID is unique.

Priority: **P1** = fix before Phase 1 starts. **P2** = decide during Phase 1
(design). **P3** = cleanup, batch whenever.

## Verdicts on reviewed commits

| Commit | Issue | Verdict | Follow-ups |
|--------|-------|---------|------------|
| `2645f8f` | round-5 bookkeeping | OK | — |
| `42341f5` | D14, D15, D16 | OK + notes | D17, D18, D19, I14 |

D14 is closed correctly: every gate value is a kubelet-recorded init-container
exit code, the tamper check runs in its own trusted container before any
agent code, the reserved exit code and the termination message are gone, and
B3's seam matches. D15 (go-git + `secrets/get`) and D16 (explicit
`verify.preset`) are closed as asked. Tick D14–D16 in `REVIEW-PHASE0-R6.md`.

**With D14 closed, no P1 design item blocks Phase 1 code.** The only
remaining P1 is **I4 (owner decision)**. D17 below is important but additive:
B2/B3 can be built as designed while it's decided.

---

## P2 — Design

### D17. Code under test can subvert the test runner in-process; the tamper check can't see it

- [x] Decided (ADR-0005 "Residual risk" + mitigations) — commit `b58a548`

  - ADR-0005 has a "Residual risk: in-process test subversion (D17)" section:
    acceptance checks are the only *automated* gate; the human draft-PR review
    (Phase 6) is the final gate; layered cheap mitigations (negative canary
    control, static flag on the diff of non-protected files, optional Judge).
  - B3 gains a canary slice (the probe as a fixture must end non-`Succeeded`);
    CONTEXT.md "Acceptance checks" softened to "only automated gate".

**Where:** ADR-0005 (Decision, Why); CONTEXT.md "Acceptance checks" ("the
only gate that can flip a Loop to Succeeded").

**Problem:** the tamper check guarantees the test *definitions* are
unchanged. It can't guarantee the *code under test* doesn't subvert the test
process, because that code runs inside it. Verified with a probe — a
non-test, unprotected file:

```go
// add.go (Add is deliberately broken)
func init() {
    if testing.Testing() { os.Exit(0) }
}
```

`go test ./...` prints `ok` and exits 0; `TestAdd` never runs. No protected
file changed, so the glob diff is empty and the check container exits 0 →
`Succeeded`. Variants: `//go:linkname` into the `testing` package, printing a
fake `--- PASS` stream, monkeypatching the runner in interpreted languages
(pytest `conftest`-free import hooks). Any output-based detection is forgeable
by the same process.

**Fix:** be explicit that this is residual risk, and add cheap layered
mitigations — none is complete alone:
1. **Say it in ADR-0005** (new "Residual risk" section) and soften CONTEXT.md:
   acceptance checks are the only *automated* gate; a human reviewing the
   draft PR (Phase 6, draft by default) is the final gate.
2. **Negative control (catches the naive class, incl. the probe above).** In
   the trusted tamper container, generate a canary test file per tested
   package (random name, `t.Fatal` with a random token) into a *separate copy*
   of the checkout, and run one extra check container on it that **must fail**
   and whose failure must be reported by `go test` (non-zero exit). A binary
   that exits 0 regardless of tests makes the canary pass → `TamperedVerify`
   (or a new `SubvertedVerify` reason). Cheap, and the always-exit-0 trick is
   the obvious one a model will stumble into.
3. **Static flag on the diff (advisory, not a gate).** The trusted tamper
   container greps the base→verified diff of *non-protected* files for
   `testing.Testing()`, `os.Exit` inside `init`, `//go:linkname`,
   `flag.Lookup("test.` and records hits as a `Warning` event + history note,
   and forces `spec.pr.ready=false`. With `approval: Manual` (Phase 4), a hit
   could pause for approval.
4. Optional (Phase 6): the Judge sidecar gets the diff and is asked
   specifically whether the change games the tests.

**Acceptance:** ADR-0005 has a Residual-risk section naming in-process
subversion; B3 gains a slice for the canary (the probe above as a test
fixture must end non-`Succeeded`); CONTEXT.md wording updated.

### D18. Job / pod restart semantics must be pinned or the operator reads the wrong pod

- [x] Decided — ADR-0005 (D18) + B3 — commit `b58a548`

  - Pod `restartPolicy: Never`, Job `backoffLimit: 0`, `activeDeadlineSeconds`
    set; operator reads the single pod by the Job's `controller-uid` label
    (zero or >1 pods ⇒ `VerifyError`); "re-run once" creates a new Job. B3 test:
    a second pod is a `VerifyError`, not a pass.

**Where:** ADR-0005 Decision §4–5; B3.

**Problem:** with `restartPolicy: OnFailure`, a failing init container is
**restarted in place** (the check re-runs, possibly passing on a retry of a
flaky or time-dependent test); with Job `backoffLimit > 0`, the Job creates
**new pods** after a failure, and "read `initContainerStatuses`" becomes
"which pod?". Either breaks "check *k* failed ⇒ iterate".

**Fix:** state it: pod `restartPolicy: Never`, Job `backoffLimit: 0`,
`activeDeadlineSeconds` set (D12), and the operator reads the single pod
selected by the Job's `controller-uid` label; zero or >1 pods ⇒
`VerifyError`. `VerifyError`'s "re-run once" creates a **new Job**, never
relies on Job retries. Add both to B3's test (a second pod appearing is a
`VerifyError`, not a pass).

### D19. Check containers must not share a writable checkout

- [x] Decided — ADR-0005 (D19) — commit `b58a548`

  - Unconditional: the checkout is mounted **read-only** into every check
    container; each check gets its own writable scratch (`emptyDir` for
    `HOME`/`GOCACHE`/`GOPATH`/`TMPDIR`); a check needing a writable tree gets a
    fresh copy from a trusted init step, never the shared one.

**Where:** ADR-0005 Decision §4 ("read-only … if the checks don't need to
write; fresh copy per check otherwise").

**Problem:** the "otherwise" branch allows a shared writable checkout. Agent
code executing in check 1 can then rewrite `*_test.go` in the shared
`emptyDir` *after* the tamper check ran, so check 2 compiles doctored tests.

**Fix:** make it unconditional: the checkout is mounted **read-only** into
every check container; each gets its own writable scratch (`emptyDir` for
`HOME`, `GOCACHE`, `GOPATH`, `TMPDIR`). If a check genuinely needs a writable
tree, it gets a **fresh copy** made by a trusted init step, never the shared
one.

---

## P3 — Cleanup

### I14. Init containers stop at the first failing check

- [x] Noted in ADR-0005 — commit `b58a548`

  - ADR-0005 Consequences: sequential init containers stop at the first failing
    check (one fix per iteration); `history[]` records which checks were not
    run; revisit with one pod per check if iteration counts suffer.

Sequential init containers stop at the first non-zero exit, so a later
failing check is never run and never fed forward; the model fixes one check
per iteration. Fine for Phase 1 — note it in ADR-0005 Consequences, and
record in `history[]` which checks were **not run** (vs passed). Revisit with
one pod per check if iteration counts suffer.

---

## Next round will check

- D17–D19 decisions (ADR-0005 edits), I14 note, I13 (runner nits).
- Round-1 P3 batch; D5 (k8s version pin).
- I4 — owner decision, the last P1.
