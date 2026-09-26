# Phase 0 review, round 7 — open issues

Review of `88d3eb3`..`7f60865` (2026-09-26), since tag `review/phase0-r6`.
Baseline at review time: runner `go test -count=1 ./...` green (I10 timeout
tests re-run 3× in a clean worktree of `88d3eb3`: stable, ~3 s), runner
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
| `88d3eb3` | I10 process-group kill on shell timeout | OK + notes | I13 |
| `7f60865` | I11 model timeout, I12 truncation + nits | OK + notes | I13 |

I10 is fixed properly: own process group, `cmd.Cancel` kills the group,
`WaitDelay` bounds pipe waits, and the group is also killed after a
successful command so background children don't leak across steps. I11 and
I12 match the requested fixes. Tick I10–I12 in `REVIEW-PHASE0-R5.md`.

---

## Phase 0 → Phase 1 gate: status board

All runner and controller P1 **bugs** from rounds 1–5 are closed. What's
left before Phase 1 code starts:

| ID | Where | Pri | Blocks | State |
|----|-------|-----|--------|-------|
| **D14** | R6 | P1 | B3 | termination message is forgeable — amend ADR-0005 |
| **I4** | R1 | P1 | Phase 0 sign-off | **owner decision** — don't pick silently |
| D15 | R6 | P2 | B2/B3 | git access for ref → SHA resolution |
| D16 | R6 | P2 | B2 | explicit glob preset instead of language detection |
| D5 | R1 | P2 | CI | pin one k8s version for kind + envtest + CI |
| D6 | R1 | P2 | Phase 3 | VolumeSnapshots on kind |
| R1 P3 list | R1 | P3 | — | license headers, `maxIterations` default, `Ref` markers, `go mod tidy`, e2e suite, controller tidy-ups |

Bookkeeping in `REVIEW-PHASE0.md` (round 1):
- **D2, D3** — superseded by ADR-0005 (D7/D10). Tick them as "Decided
  (ADR-0005)" rather than leaving them open.
- **"Scratch file committed"** — done by `5762374`; tick it.

Suggested order while waiting on I4: D14 → D16 → D15 (all ADR-0005 edits,
one commit is fine), then the round-1 P3 batch, then D5.

---

## P3 — Cleanup

### I13. Runner nits from `88d3eb3` / `7f60865`

- [ ] Done

- `runner_robustness_test.go` `TestRunnerShellTimeoutKillsBackgroundChildren`
  asserts only wall time and a success/blocked status. The I10 acceptance
  also asked that the timeout be visible to the model: assert the tool-result
  message fed back in request 2 contains the timeout / `signal: killed` text.
- `truncateToolOutput`: `elided` is computed before the rune-boundary
  adjustments, so the reported count can be off by up to 6 bytes. Compute it
  as `len(s) - len(head) - len(tail)`.
- `knownToolNames()` now just returns `knownTools`; drop the wrapper and use
  the var directly.
- `run`: both `http.Client.Timeout` and the per-request `context.WithTimeout`
  are set to `modelTimeout`. Keep only the context (it's the one a run
  deadline can cancel); set no client timeout, or a much larger safety net.
- Accepted, no action: a command using `setsid` escapes the process group and
  can outlive the tool call; with `WaitDelay` it can no longer hang the
  runner. Inside the sandbox that's acceptable — note it in a code comment.

**Acceptance:** runner tests green; the timeout test asserts the fed-back
tool message.

---

## Next round will check

- ADR-0005 amendments for D14 (P1), D15, D16.
- Round-1 P3 batch as it lands.
- I4 — still needs the owner's decision.
