# Phase 0 review, round 4 — open issues

Review of `21ccf36`..`cd8d960` (2026-09-26), since tag `review/phase0-r3`.
Docs-heavy round: the substantive commit is `cd8d960` (ADR-0005). No code
changed in `api/`, `internal/`, or `runner/` since round 3.

Each issue is self-contained so it can be picked up independently. Work
test-first as in `docs/TDD-PLAN.md`: write the failing test at the named seam,
then fix. Tick the box and add the commit hash when done. Issue IDs continue
from earlier rounds so every ID is unique.

Priority: **P1** = fix before Phase 1 starts. **P2** = decide during Phase 1
(design). **P3** = cleanup, batch whenever.

## Verdicts on reviewed commits

| Commit | Issue | Verdict | Follow-ups |
|--------|-------|---------|------------|
| `21ccf36` | round-1 bookkeeping | OK | tick round-1 P3 "sample CR stub" + "scratch file" (done by `5762374`) |
| `5762374` | I6 rename leftovers | OK | — |
| `9c5592e` | D8 → Phase 1 slice B6 | OK + notes | D9 |
| `cd8d960` | D7 → ADR-0005 | OK + notes | **D10 (P1)**, D11, D12, D13 |

ADR-0005 resolves the core of D7: evidence no longer comes from
`result.json`, `.coxswain/desired-phase` is never read back, and A4 is
dropped. Tick D7. The follow-ups below are gaps *inside* the new design; D10
must be settled before B2 is built.

---

## P1 — Blocking (before Phase 1 slice B2)

### D10. Protected-path hashing misses new files; replace hashes with a pinned base commit + protected globs

- [ ] Decided (amend ADR-0005)

**Where:** `docs/adr/0005-operator-verifies-via-isolated-job.md` (Decision
§2); `docs/TDD-PLAN-PHASE1.md` B2, CRD field `verify.acceptanceCheckPaths[]`
(line ~137), status `verify.baselineHashes`.

**Problem:**
1. **Hashing a list of existing files doesn't catch *added* files.** The agent
   can pass `go test ./...` without touching any protected file: add
   `zz_test.go` with `func TestMain(m *testing.M) { os.Exit(0) }` to the
   package, or an `init()` that short-circuits, or a `replace` in `go.mod`
   pointing a dependency at a stub. The Job isolation (fresh pod) doesn't help —
   the gaming is in the committed code the Job runs.
2. **`acceptanceCheckPaths` empty ⇒ "the runner protects the whole repo root"**
   — stale (the runner no longer protects anything), and protecting the whole
   repo would forbid the agent from changing any code.
3. **"Operator computes baseline hashes from the base ref at Loop start"** means
   the operator must clone the repo — heavy, and it puts file content in the
   operator, against CONTEXT.md's content-free operator.

**Fix:** drop hash lists. Instead:
- At Loop start the operator resolves `spec.workspace.ref` to a SHA
  (`git ls-remote`, or a tiny "resolve" Job) and records
  `status.baseCommit`. It's immutable for the Loop's life.
- Protected paths become **globs** (`spec.verify.protectedPaths[]`), with a
  per-language default when empty — for Go: `**/*_test.go`, `**/testdata/**`,
  `go.mod`, `go.sum`, plus any files the check commands reference
  (`Makefile` if a check calls `make`). Document that `go.mod`/`go.sum`
  protection means the agent can't add deps; make it overridable.
- Tamper check (in the verify Job, first step, before any check runs):
  `git diff --name-only <baseCommit> <iterationCommit> -- <globs>` non-empty
  ⇒ `TamperedVerify`. This catches **added, modified, deleted, and renamed**
  files, needs no stored hashes, and keeps the operator content-free.

**Acceptance:** ADR-0005 amended; `status.verify.baselineHashes` replaced by
`status.baseCommit`; B2's test set includes (a) edit an existing
`*_test.go`, (b) **add** a new `*_test.go` with `TestMain` → `os.Exit(0)`,
(c) add a `replace` to `go.mod`; each ends `Failed:TamperedVerify` with no
check run, even when the fake runner's `result.json` claims otherwise.

---

## P2 — Design (settle while building B2/B3)

### D11. Bind the verified commit to what gets reported and PR'd

- [ ] Decided

**Where:** ADR-0005 Decision §1 ("the commit the runner pushed after
implement"); Phase 6 PR flow.

**Problem:** the agent controls the branch. If the iteration SHA comes from
`result.json` (a claim) or the branch head is read at a different moment from
when the Job clones, the agent can have commit A verified and commit B end up
as the PR head (force-push after verify).

**Fix:** at `Verifying` start the operator resolves the branch head itself
(`ls-remote`), records `status.history[n].verifiedCommit`, and the Job checks
out **that SHA** (not the branch name). `Succeeded` records the verified SHA;
the PR step (Phase 6) must open the PR at exactly that SHA, or fail if the
branch head moved.

### D12. The verify Job runs untrusted code — isolate it like the sandbox

- [ ] Decided

**Where:** ADR-0005 Consequences.

**Problem:** the Job executes the agent's committed code (that's what tests
do). A fresh pod stops process/PATH gaming, but the code can still reach the
network, the API server, or credentials mounted into the Job.

**Fix:** the verify Job gets: `automountServiceAccountToken: false`,
read-only clone credentials only (never the push token), the same
NetworkPolicy as the sandbox, resource limits + `activeDeadlineSeconds`, and
the same runtime class as agent-sandbox pods (e.g. gVisor) if one is
configured. Consider running it as an agent-sandbox `Sandbox` rather than a raw
Job so isolation policy lives in one place.

### D13. Job reporting contract: distinguish tampered / failed / errored

- [ ] Decided

**Where:** ADR-0005 Decision §1–2; `TDD-PLAN-PHASE1.md` B2/B3.

**Problem:** one pod does tamper check + N checks and reports "via exit
code". The operator must distinguish `TamperedVerify` (terminal) from check
failure (iterate) from infra error (`VerifyError`, re-run once — CONTEXT.md
"Failure reason"). Feeding check output forward also needs pod logs
(`pods/log` RBAC), which the ADR doesn't list.

**Fix:** fix the contract, e.g. tamper check as an init container (non-zero
⇒ `TamperedVerify`), checks in the main container with reserved exit codes, or
a termination message (`/dev/termination-log`, ≤4 KB JSON: per-check exit
codes) read from pod status — evidence the operator reads from the API, with
the raw log fetched separately for the next prompt. Add `pods/log` to the RBAC
list. Note for B3's test: envtest has no Job controller, so the test sets Job
/ pod status directly — say so in the plan.

### D9. B6 "return nil, no requeue" can wedge the Loop

- [ ] Plan amended

**Where:** `docs/TDD-PLAN-PHASE1.md` B6.

**Problem:** `Owns(&Sandbox{})` only maps events from sandboxes owned by
this Loop. When the foreign-owned `<loop>-sandbox` is later deleted, nothing
enqueues the Loop, so with no requeue it stays `SandboxNameConflict` forever.

**Fix:** either `RequeueAfter` a long interval (e.g. 5m) while the conflict
condition is set, or add a `Watches` on Sandboxes mapping by name
(`<loop>-sandbox` → Loop). Add to B6's test: delete the foreign sandbox →
the next reconcile creates the Loop's own and clears the condition.

---

## P3 — Cleanup

### I9. Operator `pods/exec` is namespace-wide

- [ ] Noted in ADR-0005

`pods/exec` on the operator's ClusterRole lets it exec into any pod in any
Loop namespace. Acceptable for Phase 1; record it in ADR-0005 Consequences and
revisit in Phase 7 hardening (a read-only sidecar serving `result.json` would
remove the need for exec entirely).

---

## Next round will check

- ADR-0005 amendments for D10 (P1) and D11–D13.
- I5 + I7 (runner robustness on typed structs) and I8.
- I4 — still needs the owner's decision. Don't pick silently.
