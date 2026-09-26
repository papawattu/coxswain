# Phase 0 review, round 2 — open issues

Review of the round-1 fix commits `e19c5a2`..`900c72f` (2026-09-26), against
`docs/REVIEW-PHASE0.md` (round 1). Baseline at review time:
`cd runner && go test ./...` green, `internal/controller` envtest green.
Uncommitted work in the tree at review time (`Makefile`, `.golangci.yml`,
`runner/*`) was **not** reviewed; it's picked up next round once committed.

Each issue is self-contained so it can be picked up independently. Work
test-first as in `docs/TDD-PLAN.md`: write the failing test at the named seam,
then fix. Tick the box and add the commit hash when done. Issue IDs continue
from round 1 (I6+, D7+) so every ID is unique across rounds.

Priority: **P1** = fix before Phase 1 starts. **P2** = decide during Phase 1
(design). **P3** = cleanup, batch whenever.

## Verdicts on reviewed commits

| Commit | Round-1 issue | Verdict | Follow-ups |
|--------|---------------|---------|------------|
| `900c72f` | I2 owner ref on adoption | OK + notes | D8 |
| `d13940a` | I1 runner advertises tools | OK | — |
| `e5027e9` | D4 API group rename | OK + notes | I6 |
| `e19c5a2` | D1 → ADR-0004 result-file only | **CHANGES** | D7 (blocks Phase 1 B2/B3) |

Round-1 bookkeeping: D1 is decided by ADR-0004 and D4 is done by `e5027e9`,
but neither box is ticked in `REVIEW-PHASE0.md` — tick both with the hashes.

---

## P1 — Blocking

### D7. Phase 1 plan still lets the runner self-report verify evidence

- [ ] Decided (ADR: 0005)

**Where:** `docs/adr/0004-runner-result-file-only.md`;
`docs/TDD-PLAN-PHASE1.md` settled questions 2 ("Tamper hash = runner
reports") and 3 ("Verify = runner executes, operator decides"), slices A4, B2,
B3; status field `verify.baselineHashes`.

**Problem:** ADR-0004 correctly removes the runner's Loop-status RBAC, but the
Phase 1 plan moves the same self-reporting into `result.json`:
- The runner computes and reports sha256 of protected files. The runner runs
  in the container where the model has a shell; anything the runner can write,
  the model can write. A model that edits a test can also make the reported
  hash match the baseline, so `TamperedVerify` never fires. The plan itself
  notes the runner is "trusted to report hashes honestly".
- The runner runs acceptance checks and reports exit codes; a model can write
  `exitCode: 0`. The operator's decision is deterministic but on unverifiable
  input — exactly what ADR-0004's reason 1 rejects.
- Baseline hashes are taken "at iteration start" from the workspace, so
  tampering that slipped through iteration N becomes iteration N+1's baseline.
- `.coxswain/desired-phase` is agent-writable. OK as a hint to the runner, but
  nothing states the operator must never read it back.

**Fix:** write ADR-0005 covering D2 + D3 together (how the operator obtains
*evidence*) with this rule: **`result.json` carries the agent's claims
(summary, filesChanged, lessons, needs_input) — never evidence the operator
gates on.** Recommended design:
1. At Verifying, the operator creates a short-lived Job (fresh pod, no shared
   process namespace, agent never touches it) that checks out the Loop branch
   at the iteration commit, takes protected paths / check definitions from the
   **base ref**, runs `acceptanceChecks`, and reports via container exit code /
   Job status, read from the API.
2. Baseline hashes computed once, from the base ref, at Loop start.
3. ADR-0004 amended: operator never reads `.coxswain/desired-phase`;
   `status.desiredPhase` is the only truth.

Minimum acceptable alternative (record as accepted risk in the ADR if chosen):
operator `exec`s a fixed verify command into the sandbox itself (no runner
involvement), baseline from base ref. Leaves PATH-shadowing / background
process / `go.mod replace` gaming open.

Then update `TDD-PLAN-PHASE1.md`: settled questions 2–3, drop or rewrite A4,
B2/B3 seams become Job status (or operator exec result), not `result.json`.

**Acceptance:** ADR-0005 committed; TDD-PLAN-PHASE1 has no slice where an
operator gate decision reads a value from `result.json` or the workspace; B2
has a test where the agent edits a protected file **and** rewrites
`result.json` to claim the baseline hash, and the Loop still ends
`Failed:TamperedVerify`.

---

## P2 — Design / Phase 1 plan additions

### D8. Foreign-owned sandbox should surface as a condition, not a retry storm

- [ ] Added to Phase 1 plan

**Where:** `internal/controller/loop_controller.go` (`ensureSandbox`, after
`900c72f`); `docs/TDD-PLAN-PHASE1.md`.

**Problem:** a sandbox owned by another controller now makes `Reconcile`
return `AlreadyOwnedError`: exponential-backoff requeue forever, visible only
in logs. CONTEXT.md: audit trail is "never in logs alone".

**Fix:** add a Phase 1 B-slice: on `AlreadyOwnedError`, emit a `Warning`
event, set condition `SandboxReady=False` reason `SandboxNameConflict`, return
no error. Don't fix before the phase machine/conditions exist.

**Acceptance:** slice listed in `TDD-PLAN-PHASE1.md`; when built, the
foreign-owner test asserts the condition + event and `Reconcile` returns nil.

---

## P3 — Cleanup

### I6. D4 rename leftovers

- [ ] Done

**Where / fix** (`grep -rn 'cox\.dev'`, excluding ADR-0001 and round-1 review):
- `cmd/main.go:169` — `LeaderElectionID: "a54de9d2.cox.dev"` →
  `a54de9d2.coxswain.wattu.com`.
- `docs/PLAN.md` lines 15, 96, 123 — group is `coxswain.wattu.com`; drop the
  "confirm `cox.dev` is unused" to-dos.
- `docs/E2E-PHASE0.md` — `cox.cox.dev_loops.yaml` →
  `coxswain.wattu.com_loops.yaml`, `apiVersion: coxswain.wattu.com/v1alpha1`;
  the documented repro is currently broken.
- Optional: rename `config/samples/cox_v1alpha1_loop.yaml` →
  `coxswain_v1alpha1_loop.yaml` and update its kustomization.

**Acceptance:** `grep -rn 'cox\.cox\.dev\|a54de9d2\.cox\.dev'` returns nothing
outside `docs/REVIEW-PHASE0*.md`; `make test` green.

---

## Next round will check

- I3 — `make test` / `make lint` fail when a runner test / runner lint is
  broken; CI workflows go through those targets.
- I5 — runner robustness (uncommitted `runner/*` changes at review time).
- I4 — still needs the owner's decision (amend Phase 0 done-when vs close the
  gaps). Don't pick silently.
