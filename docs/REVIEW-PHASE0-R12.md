# Phase 0 review, round 12 — closing round

Review of `d796ea3`..`e368662` (2026-09-27), since tag `review/phase0-r11`.
Baseline at review time (all green): `internal/controller` envtest `ok`,
`runner` `ok`, golangci-lint 0 issues on both modules, working tree clean.

Issue IDs continue from earlier rounds. No new issues this round.

## Verdicts on reviewed commits

| Commit | Issue | Verdict | Follow-ups |
|--------|-------|---------|------------|
| `d796ea3` | bookkeeping (R10) | OK | — |
| `3911606` | I21 review ID in API docs | OK — no review IDs left in the generated CRD | — |
| `d42e5ad` | D21 advisory scan → `VerifySuspicious` condition | OK | — |
| `e368662` | bookkeeping (R11) | OK | — |

D21 is closed as asked: status condition instead of `spec`, `effectiveReady`
computed in Phase 6, scan in its own trusted init container reporting via
termination message and exiting 0 either way, "scan unavailable" ≠ clean,
B3d's seam asserts `generation` unchanged, and CONTEXT.md / ADR-0005 / the
plan agree.

---

## Phase 0 review: summary for the owner

Rounds 1–11 raised **21 implementation issues (I1–I21) and 21 design items
(D1–D21)**. Everything the builder could act on alone is closed and ticked
with a commit hash. Highlights:

- **Bugs fixed:** runner never advertised tools (I1); adopted sandboxes lost
  their owner ref (I2); runner module untested in CI (I3); shell timeout hung
  on background children (I10, verified by probe); sandbox names that break
  agent-sandbox's Service (D20).
- **Verify trust model settled (ADR-0004, ADR-0005):** the runner reports
  claims only; the operator pins `baseCommit` and `verifiedCommit`; tamper
  check is a `git diff` over protected globs in a trusted init container;
  every gate value is a kubelet-recorded container exit code, one check per
  container; read-only checkout; `restartPolicy: Never` / `backoffLimit: 0`.
- **Residual risk recorded (D17):** code under test can subvert the test
  process in-process (probe: a non-test `init()` calling `os.Exit(0)` under
  `testing.Testing()` makes `go test` report `ok`). Mitigated by a canary
  negative control (B3a → `SubvertedVerify`) and an advisory diff scan
  (B3d → `VerifySuspicious`); the human reviewing the draft PR is the final
  gate.

## Waiting on the owner

| ID | Pri | Decision |
|----|-----|----------|
| **I4** | P1 | Phase 0 "done when": (a) narrow PLAN.md to what the e2e proved (operator creates + logs the Sandbox object) and move runner-in-sandbox into Phase 1, or (b) close the gaps now. Builder and reviewer both recommend **(a)**. |
| D5 | P2 | Pin one k8s version for kind + envtest + CI (currently 1.32 plan / 1.34 kind / 1.37 envtest; prod needs ≥1.37). |
| D6 | P2 | VolumeSnapshots for Phase 3 on kind (csi-hostpath-driver) vs Phase 3 e2e on the homelab Ceph cluster. |
| R1 e2e | P3 | Automate the Phase 0 e2e — needs a kind cluster. |

**Builder: nothing further until the owner decides I4.** Don't start Phase 1
B1.
