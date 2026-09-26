# Phase 1 — TDD plan (DRAFT)

Phase 1 is the loop, minimal: the CRD types get fleshed out, the Runner becomes
the in-sandbox **phase driver**, and the controller drives the **reconcile state
machine** over the phase enum. The trust model (protected paths, deterministic
verify) lands here, not in Phase 6.

Read `docs/PLAN.md` "Phase 1" and `docs/adr/0003-one-shot-loop-fork-not-retask.md`
before working this — the Loop is one-shot and the operator is a deterministic
state machine.

## The two surfaces

### A. Runner as phase driver (`runner/`)
The Phase 0 runner ran once and exited. Phase 1 replaces it with a daemon that:
reads `desiredPhase` (the operator writes it — via a file the runner watches, or
an env/polling channel), does the work for that phase, and writes `observedPhase`
plus `result.json`. Model context survives across phases within an iteration.

**Candidate seams (to confirm):**
- **A1** — *Phase contract.* The runner reads `loop.Status.DesiredPhase` (via the
  API), does the work, and patches `loop.Status.ObservedPhase` + writes
  `result.json`. The seam is the **Loop status** (observed via the API client in
  the test) + the workspace + result.json — never the model internals. A fake
  model drives the phase work. The runner gets RBAC to patch Loop `status`.
- **A2** — *Planning phase.* Given a goal, the runner writes `PLAN.md` at
  `.coxswain/PLAN.md` with a ≤4KB summary, and sets `observedPhase=Planning`
  complete. Seam: the PLAN.md file + result.
- **A3** — *Implementing phase.* The runner runs the model with a shell tool
  (Phase 0 R3 already proved tool exec); here it additionally must write
  `result.json.status` per the phase outcome and `filesChanged[]`. Seam: workspace
  files + result.
- **A4** — *Verifying phase.* The runner runs each acceptance check (a shell
  command) and records exit codes. Exit 0 = pass. The verify output is written so
  the operator can feed it into the next prompt. Seam: workspace + result.
- **A5** — *Model context continuity.* Across Planning→Implementing→Verifying
  within one iteration, the runner keeps the same conversation (the model is
  called with the accumulated message history, not reset each phase). Seam: the
  fake model's request history across a multi-phase run.

### B. Controller reconcile state machine (`internal/controller/`)
The Phase 0 controller only ensured the sandbox + set `Pending`. Phase 1 drives
the phase enum based on `status.observedPhase` (from the runner) and the trust
model.

**Candidate seams (to confirm):**
- **B1** — *Phase transitions.* The controller reads `status.observedPhase` (the
  runner's signal) and advances `status.phase` per the transition table:
  `Pending → Planning → Implementing → Verifying → Succeeded` (happy path), and
  `Verifying → Implementing` (re-plan/implement on failure), `Verifying → Failed`
  on `MaxIterations`. Seam: cluster state via envtest (set observedPhase, reconcile,
  assert phase).
- **B2** — *Protected paths / TamperedVerify.* The operator hashes the
  acceptance-check source files at iteration start; if they changed during
  implement, the iteration fails with `TamperedVerify` **before any check runs**
  (terminal). Seam: create a Loop with a protected test file, mutate it in the
  "implement" step (simulate the runner), reconcile, assert `Failed:
  TamperedVerify` and that no verify check ran. This is the core anti-gaming test.
- **B3** — *Deterministic verify.* Run each acceptance check; all exit 0 →
  `Succeeded`; any non-zero → feed output forward to the next implement prompt.
  Seam: envtest with a fake "check ran" signal (or a real shell in a sandbox —
  but for the controller unit test, the verify *outcome* is an input, not
  recomputed).
- **B4** — *Iteration + history.* Each transition increments `status.iteration`
  and appends to `status.history[]` (the audit trail). Seam: assert iteration
  count and history entries after a multi-iteration run.
- **B5** — *maxIterations.* A Loop that keeps failing stops at `maxIterations`
  with `Failed:MaxIterations` (terminal). Seam: a Loop with `maxIterations: 2`
  that always fails → `Failed:MaxIterations` after 2 tries.

## Settled design questions (2026-09-26, all confirmed with user)

1. **Phase channel = Loop status subresource.** `desiredPhase` and
   `observedPhase` are Loop status fields (`status.desiredPhase`,
   `status.observedPhase`). The operator is a pure state machine: it sets
   `desiredPhase` and reads `observedPhase`. The runner is the only thing that
   touches the workspace; it patches `loop.status` via the API. **Consequence:
   the runner needs RBAC to `patch`/`update` the Loop `status` subresource**, and
   `LoopStatus` gains `desiredPhase` + `observedPhase` fields (see "CRD changes").
2. **Tamper hash = runner reports.** The runner reports the sha256 of each
   acceptance-check source file in `result.json` (baseline at iteration start,
   re-reported after implement). The operator compares reported hashes to the
   recorded baseline and *decides* `TamperedVerify` (terminal, before any check
   runs). The operator never opens files — it stays content-free. The runner is
   trusted to report hashes honestly (the operator can't verify the hash is of
   the real file; this is the accepted trade-off for keeping the operator
   deterministic).
3. **Verify = runner executes, operator decides.** The runner runs each
   acceptance check in the sandbox and reports exit codes in `result.json`.
   The operator reads the outcome and decides Succeeded vs re-implement.

## CRD changes (Phase 1)

`LoopSpec` gains:
- `workspace.gitCredentialSecret` (string, optional) — secret with the git token
- `verify.acceptanceCheckPaths[]` (optional) — if empty, the runner protects the
  whole repo root (or a configurable default); these are the protected paths for
  TamperedVerify. (The plan's `acceptanceChecks[]` commands stay as-is.)
- `loop.phaseTimeout` (metav1.Duration, default 30m) — Phase 1 adds the field;
  the timeout *enforcement* (restart from checkpoint) is Phase 3, so Phase 1
  only records it.
- `approval` (already in the enum; `mode: Auto|Manual`, `onReject: Replan|Fail`)
  — Phase 1 uses `mode: Auto` only; the Manual gate is Phase 4.

`LoopStatus` gains:
- `desiredPhase` / `observedPhase` (Phase) — the phase channel (design Q1)
- `plan` `{ summary string (≤4KB), hash string (sha256 of PLAN.md) }`
- `history []HistoryEntry` — the audit trail: `{ iteration, phase, reason, message, timestamp }`
- `iteration` (int32) — already present from Phase 0
- `verify` `{ lastCheckResults []string, baselineHashes []FileHash }` — the
  recorded hash baseline for TamperedVerify (design Q2) and the last verify
  output to feed forward

`FileHash` = `{ path string, sha256 string }`.

RBAC: the runner (a ServiceAccount in the sandbox) gets `update`/`patch` on
`loops/status`. The operator keeps full CRUD on loops + sandboxes.


## Out of scope for Phase 1

Budgets/stall (Phase 2), checkpoints/fork (Phase 3), approval gate +
kubectl-cox (Phase 4), shared memory (Phase 5), PR + Judge (Phase 6), hardening
(Phase 7). The `AwaitingApproval` phase is in the enum but the approval *gate*
is Phase 4 — Phase 1 uses `spec.approval.mode: Auto` only.

## Suggested slice order (once seams are confirmed)

A1 → A2 → A3 → A4 → A5 (runner, each red→green), then
B1 → B2 → B3 → B4 → B5 (controller, each red→green). B2 (TamperedVerify) is the
highest-value test — the anti-gaming guarantee.
