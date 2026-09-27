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

**Slice status:** B1 ✅ done — `status.desiredPhase`/`status.observedPhase` added
(CRD); `nextPhase(current, reported)` pure transition function (happy path
Pending → Planning → Implementing → Verifying → Succeeded; terminal + iterate/
failed branches left for B3/B4); Reconcile advances the machine when the runner
reports a valid forward step. Tests: pure `nextPhase` table + envtest (advance
on valid report, no-op on skip-ahead).

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
  verify Job (fresh pod, no shared process namespace, isolated per D12). **Every
  gate value is a kubelet-recorded container exit code, one check per container
  (D14 — the termination message was forgeable):** a clone init container checks
  out the pinned SHA; a tamper-check init container (trusted image, only `git` +
  the glob diff, before any agent code runs) exits non-zero on divergence;
  **each acceptance check is its own init container**. The operator reads
  `status.initContainerStatuses[i].state.terminated.exitCode` (tamper non-zero →
  `TamperedVerify` with no check container run; all checks 0 → `Succeeded`; check
  *k* non-zero → iterate with *k* in history; pod errored → `VerifyError`
  re-run once). Pod **logs** (`pods/log`) are the feed-forward channel for the
  next prompt (claims-grade, never a gate). Seam: **envtest has no Job
  controller**, so the B3 test sets `initContainerStatuses[].state.terminated.exitCode`
  per check directly (not a termination message) and asserts the phase. A minimal
  verify Job image (clone + one tamper init container + one check init container
  per check) is built in Phase 1 and runs isolated (D12).
- **B3a** — *Canary / negative control (ADR-0005 D17).* The code under test can
  subvert the test process in-process (an `init(){ if testing.Testing() {
  os.Exit(0) } }` makes `go test` exit 0 without running tests; no protected file
  changes, so the glob diff is empty). Mitigation: the verify Job includes a
  **canary check container** — the trusted tamper container generates a canary
  test per tested package (random name, `t.Fatal` with a random token) into a
  *separate copy* of the checkout, and a canary check container runs it that
  **must fail** and whose failure must be reported by `go test` (non-zero exit).
  A binary that exits 0 regardless of tests makes the canary *pass* →
  **`Failed:SubvertedVerify`** (a new terminal reason; added to CONTEXT.md
  "Failure reason"). Seam: envtest — assert the verify Job spec the operator
  builds contains a canary check container (trusted image, over a separate
  checkout copy); and the controller decision: canary container exit 0 ⇒
  `Failed:SubvertedVerify`. e2e fixture: the round-8 probe (`init(){ if
  testing.Testing() { os.Exit(0) } }`) as a fixture must end non-`Succeeded`.
- **B3b** — *Restart semantics (ADR-0005 D18).* The operator reads the *single*
  pod of the verify Job; `restartPolicy: OnFailure` / `backoffLimit > 0` would
  restart or recreate pods and break "check *k* failed ⇒ iterate". Fix: the Job
  spec has **`restartPolicy: Never`, `backoffLimit: 0`, `activeDeadlineSeconds`**
  (D12); the operator reads the single pod by the Job's `controller-uid` label —
  **zero or >1 pods ⇒ `VerifyError`**; `VerifyError`'s re-run creates a **new
  Job** (name includes an attempt suffix), never reuses one. Seam: envtest —
  assert the Job spec has `restartPolicy: Never` + `backoffLimit: 0`;
  controller — a second pod for the Job's `controller-uid` ⇒ `VerifyError` (not
  a pass); the re-run creates a new Job with an attempt suffix.
- **B3c** — *Read-only checkout (ADR-0005 D19).* The "otherwise writable"
  checkout branch let agent code in check 1 rewrite `*_test.go` after the
  tamper check, so check 2 could compile doctored tests. Fix: the checkout is
  mounted **read-only into every check container** (unconditional); each check
  gets its own writable scratch `emptyDir` for `HOME`, `GOCACHE`, `GOPATH`,
  `TMPDIR`; a check needing a writable tree gets a fresh copy from a trusted
  init step. Seam: envtest — assert every check container mounts the checkout
  `readOnly: true` and has its own scratch `emptyDir`.
- **B3d** — *Advisory static diff scan (ADR-0005 D17, mitigation 3; D21).* The
  in-process test-subversion canary (B3a) is the automated gate; this is the
  *advisory* layer that runs alongside it. It scans the base→verified diff of
  **non-protected** files for `testing.Testing()`, `os.Exit` inside `init`,
  `//go:linkname`, and `flag.Lookup("test.`. **Advisory only — never a gate, and
  never writes `spec`** (D21: `spec` is user-owned desired state — rewriting it
  fights `kubectl apply`/GitOps, bumps `generation`, and blurs the audit
  trail). Instead a hit records a `VerifySuspicious=True` **condition** (reason
  `SubversionPatternInDiff`, message listing the matched patterns + files) and a
  history note. Phase 6's PR step reads that condition:
  `effectiveReady = spec.pr.ready && !VerifySuspicious`. Reporting channel: the
  scan runs as its own trusted init container **after the tamper check**; hits
  are reported by its **termination message** (acceptable here — no agent code
  runs in that container, per D15) and the container **exits 0 either way** so
  the scan can never block the checks. A missing/garbled message is treated as
  "scan unavailable" (a `Warning` event), **not** as clean. Seam: envtest — a
  Loop whose non-protected change contains `testing.Testing()` in an `init`
  still reaches `Succeeded`, and the operator records the
  `VerifySuspicious=True` condition + a `Warning` event + a history note, with
  **`spec` unchanged (generation stays the same)**; a clean change records no
  such condition/event/note.
- **B4** — *Iteration + history.* Each transition increments `status.iteration`
  and appends to `status.history[]` (the audit trail, incl. `verifiedCommit`);
  **also records which checks were `NotRun` after the first failing check
  (I14)** — sequential init containers stop at the first non-zero, so the model
  fixes one check per iteration and the next prompt targets the not-run set.
  Seam: assert iteration count + history entries after a multi-iteration run,
  and that checks after the first failing one are recorded as `NotRun`.
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
2. **Tamper check = base-commit glob diff (REVISED, ADR-0005 round-4 D10; round-6
   D14/D16).** ~~Operator computes baseline hashes from the base ref~~ → the
   operator pins **`status.baseCommit`** (resolved from `spec.workspace.ref` at
   Loop start via go-git — D15) and protected paths come from **
   `spec.verify.preset`** (enum, default `go`) + `protectedPaths[]` (+
   `protectedPathsOverride`). At `Verifying`, the verify Job's **tamper-check
   init container** (trusted image, only `git` + the glob diff, before any agent
   code runs) does `git diff --name-only <baseCommit> <verifiedCommit> --
   <globs>`; non-zero exit ⇒ `TamperedVerify` (terminal, before any check
   container runs). This catches **added, modified, deleted, and renamed**
   protected files (a hash list missed added files — D10), needs no stored
   hashes, and keeps the operator content-free (it never clones or holds file
   content). The check *results* are the exit codes of per-check init containers
   (D14 — the termination message was forgeable). See ADR-0005.
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
- `verify.protectedPaths[]` (optional, **globs**) + `verify.preset` (enum, default `go`; ADR-0005 round-6 D16) + `verify.protectedPathsOverride` (bool, default false) — the protected paths for the TamperedVerify glob diff. The content-free operator can't detect the repo's language, so the preset is **explicit**: `preset: go` expands to the Go glob set (`**/*_test.go`, `**/testdata/**`, `go.mod`, `go.sum`); `protectedPaths[]` **adds** to it; `protectedPathsOverride: true` (or `preset: none`) **replaces** it. Drop the "files a check references" heuristic; document that a check calling `make` should list `Makefile` in `protectedPaths`. Protecting `go.mod`/`go.sum` means the agent can't add dependencies (documented). (The plan's `acceptanceChecks[]` commands stay as-is.)
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
- `verify` `{ lastCheckResults []string }` — the last verify outcome (from the verify Job's `initContainerStatuses` exit codes, one check per container — ADR-0005 D14) to feed forward. (The old `baselineHashes` is gone — replaced by `baseCommit` + the Job's glob diff.)

**`result.json` (the runner's claims file) carries NO verify-evidence fields**
(ADR-0005). Its schema for Phase 1 is: `status` (success|blocked|needs_input),
`summary`, `filesChanged[]`, `verificationNotes` (a *claim*, free text),
`nextIterationPlan?`, `lessons[]`, and the reported `observedPhase`. The verify
outcome and the tamper verdict are **not** in `result.json` — they come from
the verify Job (pod `initContainerStatuses` exit codes, one check per container — D14).

RBAC: the runner gets **no** Loop RBAC (ADR-0004) — it is credential-free and
only writes `result.json`. The operator keeps full CRUD on loops + sandboxes
and, per ADR-0005, gains:
- `pods/exec` — to `cat` the runner's claims `result.json` out of the sandbox
  (namespace-wide; I9 — revisit in Phase 7).
- `jobs` create/delete/list/get (in the Loop's namespace) — to create the verify
  Job and read its status (the verify evidence).
- `pods/get` (in the Loop's namespace) — to read the verify Job's pod status
  (`initContainerStatuses[].state.terminated.exitCode`, one check per container;
  D14).
- `pods/log` (in the Loop's namespace) — to fetch the raw verify Job log to feed
  the next implement prompt (claims-grade, never a gate; D14).
- `secrets/get` (in the Loop's namespace) — to read
  `spec.workspace.gitCredentialSecret` for go-git ref resolution (D15).


## Out of scope for Phase 1

Budgets/stall (Phase 2), checkpoints/fork (Phase 3), approval gate +
kubectl-cox (Phase 4), shared memory (Phase 5), PR + Judge (Phase 6), hardening
(Phase 7). The `AwaitingApproval` phase is in the enum but the approval *gate*
is Phase 4 — Phase 1 uses `spec.approval.mode: Auto` only.

## Suggested slice order (once seams are confirmed)

A1 → A2 → A3 → A4 (runner, each red→green — note A4 is now *context
continuity*; the old A4 "runner runs checks" is dropped per ADR-0005), then
B1 → B2 → B3 → B3a → B3b → B3c → B3d → B4 → B5 → B6 (controller, each red→green;
B3's verify path is expanded into B3a/B3b/B3c/B3d for the D17 canary + advisory
scan, D18 restart
semantics, and D19 read-only checkout). B2 (TamperedVerify
via base-ref hashing) is the highest-value test — the anti-gaming guarantee
(D7). B6 (foreign-owned sandbox → condition, D8) needs B1's condition/event
infrastructure.
