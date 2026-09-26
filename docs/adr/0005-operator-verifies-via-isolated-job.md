# ADR-0005: The operator verifies via a pinned base commit + isolated Job, not from `result.json`

**Date:** 2026-09-26 (Phase 1 design, round-2 review D7; round-4 review D10/D11/D12/D13)
**Supersedes:** D2 and D3 in `docs/REVIEW-PHASE0.md` (how the operator reads
`result.json`, and verify isolation). Amends ADR-0004.
**Revised:** round 4 (D10) replaced the hash-list tamper check with a pinned
`baseCommit` + protected globs; D11 pins the verified commit; D12 isolates the
Job; D13 fixes the Job reporting contract.

## Context

ADR-0004 removed the runner's write access to Loop status (no Loop-status RBAC;
the runner writes `result.json` only). But the Phase 1 plan moved the same
self-reporting into `result.json` itself:

- The runner **computed and reported** sha256 of protected files (baseline at
  iteration start, re-reported after implement).
- The runner **ran** the acceptance checks and **reported** their exit codes.
- The operator made the `TamperedVerify` / `Succeeded` decision
  *deterministically* — but on input the runner supplied.

The runner runs in the same container where the model has a shell. Anything the
runner can write, the model can write. So a model that edits a test file can
also:
1. edit the protected file **and** make the reported hash match the baseline
   (so `TamperedVerify` never fires),
2. report `exitCode: 0` for a check it did not run, or
3. have tampered in iteration N, which becomes iteration N+1's "baseline."

**And (round-4 D10) the hash-list approach itself was gameable even with
operator-computed hashes:** hashing a *list of existing files* misses *added*
files. The agent can pass `go test ./...` without touching any *existing*
protected file by *adding* one — a `zz_test.go` with `func TestMain(m
"testing.M") { os.Exit(0) }`, an `init()` that short-circuits, or a `replace`
in `go.mod` pointing a dependency at a stub. The Job isolation (fresh pod) does
not help because the gaming is in the *committed code the Job runs*. Hashing
also forced the operator to clone the repo and hold file content, against
CONTEXT.md's content-free operator.

The fix is to stop hashing files and instead **compare the protected paths
between two commits** the operator itself pins. The agent has no write path to
that comparison.

## Decision

**`result.json` carries the agent's *claims* — never *evidence* the operator
gates on.**

Claims (operator reads these to build the next prompt / PR description / audit
text, but never as a gate): `summary`, `filesChanged`, `verificationNotes`,
`lessons[]`, `nextIterationPlan`, `needs_input`.

Evidence (the operator gates on these, and obtains them **independently**, not
from `result.json`):
- **The base commit** — resolved by the operator from `spec.workspace.ref` at
  Loop start and pinned in `status.baseCommit`. Immutable for the Loop's life.
- **The verified commit** — resolved by the operator from the Loop branch at
  `Verifying` start and pinned in `status.history[n].verifiedCommit` (D11).
- **The tamper verdict** — `git diff --name-only <baseCommit>
  <verifiedCommit> -- <protectedGlobs>` (run in the Job; non-empty ⇒
  `TamperedVerify`). Catches **added, modified, deleted, and renamed**
  protected files. No stored hashes.
- **The acceptance-check outcome** — per-check exit codes, read from the Job
  pod (D13).

Concretely:

1. **At Loop start** the operator resolves `spec.workspace.ref` to a SHA
   (`git ls-remote`, or a tiny resolve step) and stores it in
   `status.baseCommit`.
2. **Protected paths are globs** (`spec.verify.protectedPaths[]`). When empty,
   a per-language default applies — for Go: `**/*_test.go`, `**/testdata/**`,
   `go.mod`, `go.sum`, plus any files a check command references (`Makefile`
   if a check calls `make`). *Documented:* protecting `go.mod`/`go.sum` means
   the agent can't add dependencies; it is overridable (a Loop can list
   specific paths to narrow or widen).
3. **At `Verifying` start** the operator resolves the Loop branch head to a
   SHA and records `status.history[n].verifiedCommit` (D11 — the Job checks out
   *that* SHA, not the branch name, so a force-push after verify can't slip in).
4. **The operator creates a short-lived Job** (a fresh pod, **no shared process
   namespace with the sandbox**, the agent never touches it; isolation per D12)
   that, in order:
   a. **Tamper check (first, before any check runs):** `git diff --name-only
      <baseCommit> <verifiedCommit> -- <protectedGlobs>`. Non-empty ⇒ the pod
      exits with the *tampered* reserved code (init container or first main
      step; D13).
   b. **Checks:** run each `acceptanceChecks` command, reporting per-check
      exit codes (D13's reserved-code / termination-message contract).
5. **The operator reads the Job pod status** from the API and decides:
   - tamper code → `TamperedVerify` (terminal), **no check runs**;
   - all checks exit 0 → `Succeeded` (record the verified SHA);
   - a check exits non-zero → re-plan/implement (feed the pod log into the next
     prompt);
   - the Job itself errored (crash, no report) → `VerifyError` (re-run once,
     then terminal — CONTEXT.md "Failure reason").
6. **`result.json` is read by the operator only for the claims** (via `pods/exec`
   `cat`; see I9). It is never the input to a gate decision, and it has no
   verify-evidence fields.

### How the operator reads `result.json` (resolves D2)

The operator `exec`s `cat /workspace/.coxswain/result.json` into the sandbox
pod. The Job's *evidence* never touches this path — the Job reports via its own
pod status. **I9 (noted):** `pods/exec` on the operator's ClusterRole is
namespace-wide (it can exec into any pod in any Loop namespace). Acceptable for
Phase 1; revisit in Phase 7 hardening (a read-only sidecar serving
`result.json` would remove the need for `exec` entirely).

### ADR-0004 amendment

ADR-0004 said the operator reads `result.json` and updates status "from what the
result file reported," including "the verify outcome + reported file hashes."
**That clause is withdrawn.** The operator never gates on, and never reads back
as evidence, any verify field the runner wrote. Specifically:

- **The operator never reads `.coxswain/desired-phase` back.** It writes that
  file as a hint to the runner; `status.desiredPhase` (the API field) is the
  only truth. If the two disagree, the operator trusts its own status.
- **`result.json` has no verify-evidence fields.** The verify outcome and the
  tamper verdict do not appear in `result.json` — they come from the Job.
  `verificationNotes` is a *claim* (free text the agent wrote), not evidence.

### Verified-commit binding (D11)

The agent controls the branch. If the iteration SHA came from `result.json` (a
claim), or the branch head were read at a different moment from when the Job
clones, the agent could have commit A verified and commit B end up as the PR
head (force-push after verify). So: the operator resolves the branch head
*itself* at `Verifying` start (`ls-remote`), records it as
`status.history[n].verifiedCommit`, the Job checks out **that SHA**, `Succeeded`
records the verified SHA, and the PR step (Phase 6) must open the PR at exactly
that SHA or fail if the branch head moved.

### Job isolation (D12)

The verify Job executes the agent's committed code (that's what tests do). A
fresh pod stops process/PATH gaming, but the code can still reach the network,
the API server, or credentials mounted into the Job. So the Job gets:
`automountServiceAccountToken: false`, **read-only clone credentials only**
(never the push token), the same `NetworkPolicy` as the sandbox, resource limits
+ `activeDeadlineSeconds`, and the same runtime class as agent-sandbox pods
(e.g. gVisor) if one is configured. (Consider running it as an agent-sandbox
`Sandbox` rather than a raw `Job` so isolation policy lives in one place.)

### Job reporting contract (D13)

One pod does the tamper check + N checks and must report three distinct
outcomes the operator treats differently: **tampered** (terminal
`TamperedVerify`), **check failed** (iterate), **infra error** (`VerifyError`,
re-run once). The contract (choose one; Phase 1 picks the first):

- **Reserved exit codes** in the main container: the tamper check runs first;
  if it diverges, the pod exits with a reserved *tampered* code (distinct from
  any check exit code). Per-check exit codes go to a **termination message**
  (`/dev/termination-log`, ≤4 KB JSON: per-check exit codes) read from pod
  status.
- **`pods/log` RBAC** is added so the operator can fetch the raw Job log to feed
  the next implement prompt (evidence + signal, separate from the gate).
- **B3 test note:** envtest has no Job controller, so the B3 test sets the Job
  / pod status directly (it does not run a real Job).

## Minimum acceptable alternative (recorded as accepted risk)

If the Job is too heavy for the Phase 1 schedule, the **minimum** variant is:
the operator `exec`s a **fixed verify command** directly into the sandbox (no
runner involvement) and takes the tamper check as a `git diff --name-only`
between the pinned base and verified commit. This still closes the
"runner reports the exit code" and "added protected file" gaps (the diff is
operator-pinned), but **leaves open** the PATH-shadowing / persistent background
process / `go.mod replace` *runtime* gaming (the checks run in the agent's
sandbox). We choose the Job; this is the fallback only if the Job proves
impractical, and it is recorded here so the residual risk is explicit.

## Why

1. **The operator's determinism is real only if its input is evidence it
   obtained, not a claim the agent made.** The pinned commits + the Job's
   status satisfy that; `result.json` verify fields do not.
2. **The base-commit + glob diff closes the gaps the reviews named** — edited
   test (modified), *added* test with `TestMain` short-circuit (added), `go.mod
   replace` (modified), deleted/renamed protected files (D10) — by
   construction, and needs **no stored hashes** and **no operator cloning**.
3. **It keeps the runner simple** (ADR-0004 reason 3): the runner only writes
   `result.json` (claims) and pushes its commit; it never computes or reports
   verify evidence.
4. **It is auditable**: every gate decision traces to two pinned SHAs
   (`baseCommit`, `verifiedCommit`) and a Job object the operator created (with
   a full pod log + termination message) — all retrievable from the cluster, not
   from the agent's word.

## Consequences

- The operator gains `jobs` create/delete/list/get, `pods/get` (Job pod
  status), `pods/log` (D13), and `pods/exec` (read claims) RBAC in the Loop's
  namespace. `pods/exec` is namespace-wide (I9); revisit in Phase 7.
- A verify Job image is needed (a small image that checks out a pinned SHA,
  runs the glob diff + the checks, and reports via reserved exit codes + a
  termination message). Phase 1 builds a minimal one. It runs isolated (D12):
  no SA token, read-only clone creds, sandbox NetworkPolicy, limits, runtime
  class.
- `loop.Status` changes: `verify.baselineHashes` is **removed** and replaced by
  `baseCommit` (string, set at Loop start). `history[]` entries gain
  `verifiedCommit` (string, set at `Verifying` start). The runner writes none of
  these.
- `spec.verify.acceptanceCheckPaths[]` is **replaced** by
  `spec.verify.protectedPaths[]` (globs; per-language default when empty).
- The Phase 1 B-slices change: **B2** (TamperedVerify) becomes "Job/Operator
  compares the base-commit glob diff to the verified commit" (the anti-gaming
  test set is a: edit an existing `*_test.go`; **b: add a new `*_test.go` with
  `TestMain` → `os.Exit(0)`; c: add a `replace` to `go.mod`; each ends
  `Failed:TamperedVerify` with no check run, even when the fake `result.json`
  claims otherwise); **B3** (deterministic verify) becomes "operator reads Job
  pod status (reserved codes + termination message)". **A4** (runner runs
  checks + reports exit codes) is **dropped** — the runner does not run or
  report checks.
