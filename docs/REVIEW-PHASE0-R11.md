# Phase 0 review, round 11 — open issues

Review of `3a7d369`..`e413ae7` (2026-09-27), since tag `review/phase0-r10`.
Baseline at review time: `internal/controller` envtest green, root
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
| `3a7d369` | I19 test cleanup context | OK | — |
| `87af144` | I18 `Repo` validation, API doc trim | OK + notes | I21 |
| `6e14e6f` | D20 Loop-name CEL validation | OK | — |
| `e413ae7` | I16 B3d advisory scan slice | OK + notes | **D21** |

D20 is closed well: CEL rule on the root object, the admission message
explains why, `sandboxName` is back to `<name>-sandbox`, and the
hash-truncation code is gone. I18's pattern and accept/reject tests match
the requested cases.

Bookkeeping: the I16 tick line in `REVIEW-PHASE0-R10.md` currently reads
"Done — `6e14e6f`? no, landed as B3d …". Replace it with the real hash,
`e413ae7`.

---

## P2 — Design

### D21. The advisory scan must not write the user's `spec`, and needs a reporting channel

- [x] Plan amended — `d42e5ad`: B3d + ADR-0005 D17 mitigation 3 now record a
  `VerifySuspicious=True` condition (reason `SubversionPatternInDiff`) + history
  note instead of writing `spec`; Phase 6 computes
  `effectiveReady = spec.pr.ready && !VerifySuspicious`; the scan runs as its own
  trusted init container (after the tamper check) reporting via its termination
  message, exiting 0 either way (missing/garbled → "scan unavailable", not
  clean); B3d seam asserts the condition + event + history note with `spec`
  unchanged (generation stays the same). CONTEXT.md gains a `VerifySuspicious`
  condition entry.

**Where:** `docs/TDD-PLAN-PHASE1.md` B3d ("forces `spec.pr.ready=false`");
ADR-0005 D17 mitigation 3 (same wording originated in round 8's review —
reviewer's error, corrected here).

**Problem:**
1. `spec` is user-owned desired state. An operator that rewrites it fights
   `kubectl apply` / GitOps (the next apply flips `ready` back), bumps
   `generation` (a spurious `observedGeneration` change), and blurs the audit
   trail between what the user asked for and what the operator decided.
2. B3d doesn't say how the scan's result reaches the operator. The scan runs
   in the trusted tamper container, so this is fine to specify, but it must be
   specified.

**Fix:**
- Record the decision in **status**, not spec: a condition
  `VerifySuspicious=True` (reason `SubversionPatternInDiff`, message listing
  the matched patterns + files) and a history note. Phase 6's PR step reads
  that condition: `effectiveReady = spec.pr.ready && !VerifySuspicious`.
- Reporting channel: run the scan as its own trusted init container after the
  tamper check; hits are reported by its **termination message** (acceptable
  here — no agent code runs in that container, as noted for D15), and the
  container exits 0 either way so the scan can never block checks. The
  operator treats a missing/garbled message as "scan unavailable" (Warning
  event), not as clean.
- Update ADR-0005 D17 mitigation 3 to the same wording.

**Acceptance:** B3d's seam asserts the condition + event + history note and
that `spec` is unchanged (`generation` stays the same); plan and ADR agree.

---

## P3 — Cleanup

### I21. One review ID left in the API field docs

- [x] Done — `3911606`: dropped "(I18)" from the `Workspace.Repo` comment;
  CRD regenerated.

`api/v1alpha1/loop_types.go` `Workspace.Repo` comment ends "rejected at
admission (I18)". Drop "(I18)" and regenerate the CRD — same reason as I18:
field docs are what users read in `kubectl explain`.

---

## Status

After D21 and I21 (both small), the builder has nothing left that doesn't
need the owner:

- **I4 (P1)** — Phase 0 done-when scope.
- **D5, D6 (P2)** — k8s version pin; snapshots on kind.
- **Round-1 e2e suite (P3)** — needs a kind cluster.

**Builder: do D21 + I21, then stop and wait for the owner.** Don't start
Phase 1 B1.
