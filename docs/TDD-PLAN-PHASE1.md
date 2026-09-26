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
reads `desiredPhase` (the operator writes it to `.coxswain/desired-phase`), does
the work for that phase, and writes `result.json` (which carries the reported
`observedPhase`). Model context survives across phases within an iteration.

> **Channel (ADR-0004):** the runner writes **`result.json` only** and gets **no
> Loop-status RBAC**. It does not patch the Loop's status. The operator reads
> `result.json` and *it* writes `loop.Status.ObservedPhase`/`Iteration`/`History`.
> `observedPhase` in `result.json` is the runner's *report*; the operator turns
> that report into the Loop status. This is what changed from the earlier
> status-subresource decision.

**Candidate seams (to confirm):**
- **A1** — *Phase contract.* The runner reads `desiredPhase` from
  `.coxswain/desired-phase`, does the work, and writes `result.json` (with the
  reported `observedPhase` + the Phase 0 result fields). The seam is the
  **result file** + the workspace + the fake model's request history — never the
  model internals. A fake model drives the phase work. The runner gets **no**
  Loop-status RBAC (ADR-0004); the test asserts only on `result.json` + workspace
  files. (The earlier "patch Loop status" mock is gone.)
- **A2** — *Planning phase.* Given a goal, the runner writes `PLAN.md` at
  `.coxswain/PLAN.md` with a ≤4KB summary, and sets `observedPhase=Planning`
  complete. Seam: the PLAN.md file + result.
- **A3** — *Implementing phase.* The runner runs the model with a shell tool
  (Phase 0 R3 already proved tool exec); here it additionally must write
  `result.json.status` per the phase outcome and `filesChanged[]`. Seam: workspace
  files + result.
- **A4** — *Model context continuity.* Across Planning→Implementing within one
  iteration, the runner keeps the same conversation (the model is called with the
  accumulated message history, not reset each phase). Seam: the fake model's
  request history across a multi-phase run.

> **A4 (old: Verifying phase) is DROPPED (ADR-0005).** The runner does **not**
> run acceptance checks or report exit codes. Verify evidence is obtained by the
> operator via an isolated Job (B3) and base-ref hashing (B2); the runner's job
> ends at writing `result.json` (claims) and pushing its commit.

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
- **B2** — *Protected paths / TamperedVerify via base-commit glob diff
  (ADR-0005 round-4 D10).* The operator pins `status.baseCommit` (resolved from
  `spec.workspace.ref` at Loop start). Protected paths are globs
  (`spec.verify.protectedPaths[]`, per-language default when empty). At
  `Verifying`, the verify Job runs `git diff --name-only <baseCommit>
  <verifiedCommit> -- <globs>` **before any check runs**; non-empty ⇒
  `TamperedVerify` (terminal). The agent has no write path to the evidence
  (the two SHAs are operator-pinned). **Anti-gaming test set (D10 acceptance):**
  (a) edit an existing `*_test.go`; (b) **add a new `*_test.go` with `TestMain`
  → `os.Exit(0)`**; (c) **add a `replace` to `go.mod`** — each ends
  `Failed:TamperedVerify` with **no check run**, even when the fake runner's
  `result.json` claims the protected paths are untouched. **This is the core
  anti-gaming test (D7/D10 acceptance).**
- **B3** — *Deterministic verify via the Job's pod status (ADR-0005 D11/D13).*
  At `Verifying` start the operator resolves the Loop branch head to a SHA and
  records `history[n].verifiedCommit` (D11). The operator creates a short-lived
  verify Job (fresh pod, no shared process namespace, isolated per D12) that
  checks out **that SHA** and: first runs the tamper glob diff (reserved
  *tampered* exit code on divergence), then runs the acceptance checks, reporting
  per-check exit codes in a termination message (`/dev/termination-log`). The
  operator reads the Job pod status from the API (reserved code →
  `TamperedVerify`; all checks exit 0 → `Succeeded`; a check non-zero → feed the
  pod **log** into the next prompt via `pods/log`; Job errored → `VerifyError`
  re-run once). Seam: **envtest has no Job controller**, so the B3 test sets the
  Job / pod status directly (reserved code / termination message) and asserts
  the phase — it does not run a real Job. The verify *outcome* is the Job status
  the operator read (not a `result.json` value, not recomputed by the operator).
  A minimal verify Job image (checkout a pinned SHA + run the glob diff + run
  the checks + report reserved codes / termination message) is built in Phase 1
  and runs isolated (D12): no SA token, read-only clone creds, sandbox
  NetworkPolicy, resource limits + `activeDeadlineSeconds`, runtime class.
- **B4** — *Iteration + history.* Each transition increments `status.iteration`
  and appends to `status.history[]` (the audit trail, incl. `verifiedCommit`).
  Seam: assert iteration count and history entries after a multi-iteration run.
- **B5** — *maxIterations.* A Loop that keeps failing stops at `maxIterations`
  with `Failed:MaxIterations` (terminal). Seam: a Loop with `maxIterations: 2`
  that always fails → `Failed:MaxIterations` after 2 tries.
- **B6** — *Foreign-owned sandbox → condition + requeue, not a retry storm or
  a wedge (D8 + D9).* `ensureSandbox` returns `AlreadyOwnedError` when the
  Loop's sandbox is owned by a different controller (I2, `900c72f`); as-is that
  loops `Reconcile` into an exponential-backoff requeue forever, visible only in
  logs. Fix (when the phase machine + conditions exist): on `AlreadyOwnedError`,
  emit a `Warning` event, set condition `SandboxReady=False` reason
  `SandboxNameConflict`, **return nil, and `RequeueAfter` a long interval (e.g.
  5m) while the condition is set** (D9: a plain `return nil` wedges the Loop
  because `Owns(&Sandbox{})` only maps events from sandboxes owned by this Loop,
  so a later deletion of the foreign sandbox enqueues nothing). Alternative:
  a `Watches` on Sandboxes mapping by name (`<loop>-sandbox` → Loop). Seam:
  (1) the foreign-owner test in `loop_adoption_test.go` is updated to assert the
  condition + event and that `Reconcile` returns nil with a requeue (it
  currently asserts an error — deliberately left as a red marker until B6
  lands); (2) **delete the foreign sandbox → the next reconcile creates the
  Loop's own sandbox and clears the condition.** Do not implement before the
  condition/event infrastructure from B1 exists.

## Settled design questions (2026-09-26, all confirmed with user)

1. **Phase channel (REVISED, ADR-0004).** ~~Loop status subresource~~ → **result
   file only.** The runner writes `result.json` (which reports the
   `observedPhase`) and gets **no Loop-status RBAC**. The operator reads
   `result.json` and writes `loop.Status.ObservedPhase`/`Iteration`/`History`.
   The operator communicates `desiredPhase` to the runner via a file on the
   workspace (`.coxswain/desired-phase`). `loop.Status.ObservedPhase` stays a
   field, but it is the operator's *record of the runner's report*, written by
   the operator — not a value the runner writes. Rationale: a runner with a
   shell that can patch its own Loop status can lie about its own progress,
   which breaks determinism + auditability and contradicts CONTEXT.md's
   "result file is the only output the operator reads." See ADR-0004.
2. **Tamper check = base-commit glob diff (REVISED, ADR-0005 round-4 D10).**
   ~~Operator computes baseline hashes from the base ref~~ → the operator pins
   **`status.baseCommit`** (resolved from `spec.workspace.ref` at Loop start) and
   protected paths are **globs** (`spec.verify.protectedPaths[]`, per-language
   default when empty — for Go: `**/*_test.go`, `**/testdata/**`, `go.mod`,
   `go.sum`, + files the checks reference). At `Verifying`, the verify Job runs
   `git diff --name-only <baseCommit> <verifiedCommit> -- <globs>`; non-empty ⇒
   `TamperedVerify` (terminal, before any check runs). This catches **added,
   modified, deleted, and renamed** protected files (a hash list missed added
   files — D10), needs no stored hashes, and keeps the operator content-free
   (it never clones or holds file content). See ADR-0005.
3. **Verify = isolated Job, operator reads Job status (REVISED, ADR-0005).**
   ~~Runner runs each acceptance check and reports exit codes in
   `result.json`~~ → at `Verifying` the operator creates a short-lived Job
   (fresh pod, no shared process namespace with the sandbox) that checks out
   the Loop branch at the iteration commit, runs the acceptance checks (from
   the base ref), and reports via container exit code / Job status, which the
   operator reads from the API. `result.json` carries claims only. See
   ADR-0005.

## CRD changes (Phase 1)

`LoopSpec` gains:
- `workspace.gitCredentialSecret` (string, optional) — secret with the git token
- `verify.protectedPaths[]` (optional, **globs**; ADR-0005 round-4 D10) — the
  protected paths for the TamperedVerify glob diff. If empty, a per-language
  default applies — for Go: `**/*_test.go`, `**/testdata/**`, `go.mod`, `go.sum`,
  + any files a check command references (`Makefile` if a check calls `make`).
  Protecting `go.mod`/`go.sum` means the agent can't add dependencies
  (documented; overridable). (The plan's `acceptanceChecks[]` commands stay
  as-is.)
- `loop.phaseTimeout` (metav1.Duration, default 30m) — Phase 1 adds the field;
  the timeout *enforcement* (restart from checkpoint) is Phase 3, so Phase 1
  only records it.
- `approval` (already in the enum; `mode: Auto|Manual`, `onReject: Replan|Fail`)
  — Phase 1 uses `mode: Auto` only; the Manual gate is Phase 4.

`LoopStatus` gains (all **operator-written**; ADR-0004 — the runner never writes these, it only reports them in `result.json`):
- `desiredPhase` / `observedPhase` (Phase) — the operator records the phase it asked for and the phase the runner reported. `desiredPhase` is also copied to `.coxswain/desired-phase` for the runner to read.
- `baseCommit` (string) — the SHA resolved from `spec.workspace.ref` at Loop start, pinned for the Loop's life (ADR-0005 round-4 D10). **Replaces** the old `verify.baselineHashes`.
- `plan` `{ summary string (≤4KB), hash string (sha256 of PLAN.md) }`
- `history []HistoryEntry` — the audit trail: `{ iteration, phase, reason, message, timestamp, verifiedCommit string (the SHA the verify Job checks out, set at `Verifying` start — ADR-0005 D11) }`
- `iteration` (int) — already present from Phase 0
- `verify` `{ lastCheckResults []string }` — the last verify outcome (from the verify Job's termination message / pod status) to feed forward. (The old `baselineHashes` is gone — replaced by `baseCommit` + the Job's glob diff.)

**`result.json` (the runner's claims file) carries NO verify-evidence fields**
(ADR-0005). Its schema for Phase 1 is: `status` (success|blocked|needs_input),
`summary`, `filesChanged[]`, `verificationNotes` (a *claim*, free text),
`nextIterationPlan?`, `lessons[]`, and the reported `observedPhase`. The verify
outcome and the tamper verdict are **not** in `result.json` — they come from
the verify Job (pod status: reserved codes + termination message).

RBAC: the runner gets **no** Loop RBAC (ADR-0004) — it is credential-free and
only writes `result.json`. The operator keeps full CRUD on loops + sandboxes
and, per ADR-0005, gains:
- `pods/exec` — to `cat` the runner's claims `result.json` out of the sandbox
  (namespace-wide; I9 — revisit in Phase 7).
- `jobs` create/delete/list/get (in the Loop's namespace) — to create the verify
  Job and read its status (the verify evidence).
- `pods/get` (in the Loop's namespace) — to read the verify Job's pod status
  (reserved exit code / termination message; D13).
- `pods/log` (in the Loop's namespace) — to fetch the raw verify Job log to feed
  the next implement prompt (D13).


## Out of scope for Phase 1

Budgets/stall (Phase 2), checkpoints/fork (Phase 3), approval gate +
kubectl-cox (Phase 4), shared memory (Phase 5), PR + Judge (Phase 6), hardening
(Phase 7). The `AwaitingApproval` phase is in the enum but the approval *gate*
is Phase 4 — Phase 1 uses `spec.approval.mode: Auto` only.

## Suggested slice order (once seams are confirmed)

A1 → A2 → A3 → A4 (runner, each red→green — note A4 is now *context
continuity*; the old A4 "runner runs checks" is dropped per ADR-0005), then
B1 → B2 → B3 → B4 → B5 → B6 (controller, each red→green). B2 (TamperedVerify
via base-ref hashing) is the highest-value test — the anti-gaming guarantee
(D7). B6 (foreign-owned sandbox → condition, D8) needs B1's condition/event
infrastructure.
