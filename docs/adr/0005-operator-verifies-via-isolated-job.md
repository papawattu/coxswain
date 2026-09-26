# ADR-0005: The operator obtains verify *evidence* from an isolated Job, not from `result.json`

**Date:** 2026-09-26 (Phase 1 design, round-2 review D7)
**Supersedes:** D2 and D3 in `docs/REVIEW-PHASE0.md` (how the operator reads
`result.json`, and verify isolation). Amends ADR-0004.

## Context

ADR-0004 removed the runner's write access to Loop status (no Loop-status RBAC;
the runner writes `result.json` only). But the Phase 1 plan moved the same
self-reporting into `result.json` itself:

- The runner **computes and reports** sha256 of protected files (baseline at
  iteration start, re-reported after implement).
- The runner **runs** the acceptance checks and **reports** their exit codes.
- The operator then makes the `TamperedVerify` / `Succeeded` decision
  *deterministically* — but on input the runner supplied.

The runner runs in the same container where the model has a shell. Anything the
runner can write, the model can write. So a model that edits a test file can
also:
1. edit the protected file **and** make the reported hash match the baseline
   (so `TamperedVerify` never fires), or
2. report `exitCode: 0` for a check it did not run, or
3. have tampered in iteration N, which becomes iteration N+1's "baseline."

The plan itself admits the runner is "trusted to report hashes honestly." That
is exactly the self-reporting ADR-0004's reason 1 rejects: the operator is
making a deterministic decision on **unverifiable** input. The operator's
*determinism* is preserved only if the input is *evidence the operator can
trust*, not a claim the agent made.

## Decision

**`result.json` carries the agent's *claims* — never *evidence* the operator
gates on.**

Claims (operator reads these to build the next prompt / PR description / audit
text, but never as a gate): `summary`, `filesChanged`, `verificationNotes`,
`lessons[]`, `nextIterationPlan`, `needs_input`.

Evidence (operator gates on these, and the operator **obtains them
independently**, not from `result.json`):
- **Acceptance-check outcome** — a container exit code / Job status, read from
  the Kubernetes API.
- **Baseline hashes of protected paths** — computed **once, from the base ref,
  at Loop start**, by the operator (not the runner).

Concretely, at `Verifying`:

1. **The operator creates a short-lived Job** (a fresh pod, **no shared process
   namespace with the sandbox**, the agent never touches it) that:
   - checks out the Loop branch at the **iteration commit** (the commit the
     runner pushed after implement),
   - takes the **check definitions and protected paths from the base ref**
     (not from the workspace — the agent can't have edited them),
   - runs each `acceptanceChecks` command,
   - and reports via **container exit code / Job `status`**, which the operator
     reads from the API.
2. **Baseline hashes** are computed **once, from the base ref, at Loop start**
   (the operator hashes the protected paths as they exist at the base ref) and
   stored in `loop.Status.Verify.BaselineHashes`. At `Verifying`, the Job also
   hashes the protected paths **at the iteration commit** and the operator
   compares: any divergence → `TamperedVerify` (terminal) **before any check
   runs**.
3. `result.json` is read by the operator **only for the claims** (to feed the
   next prompt, the PR description, the audit `history[]` text). It is never
   the input to a gate decision.

**How the operator reads `result.json` (resolves D2):** the operator `exec`s
`cat /workspace/.coxswain/result.json` into the sandbox pod (the operator needs
`pods/exec`). The Job's *evidence* never touches this path — the Job reports
via its own status. This keeps a single, small operator-side read path for the
claims.

## ADR-0004 amendment

ADR-0004 said the operator reads `result.json` and updates status "from what the
result file reported," including "the verify outcome + reported file hashes."
**That clause is withdrawn.** The operator never gates on, and never reads back
as evidence, any verify field the runner wrote. Specifically:

- **The operator never reads `.coxswain/desired-phase` back.** It writes that
  file as a hint to the runner; `status.desiredPhase` (the API field) is the
  only truth. If the two disagree, the operator trusts its own status.
- **`result.json` has no verify-evidence fields.** The verify outcome and
  baseline/iteration hashes do not appear in `result.json` at all — they come
  from the Job (outcome) and the operator's own base-ref hashing (baseline).
  `verificationNotes` is a *claim* (free text the agent wrote), not evidence.

## Minimum acceptable alternative (recorded as accepted risk)

If the Job is too heavy for the Phase 1 schedule, the **minimum** variant is:
the operator `exec`s a **fixed verify command** directly into the sandbox
(no runner involvement) and takes the baseline from the base ref. This still
closes the "runner reports the exit code" gap, but **leaves open** the
PATH-shadowing / persistent background process / `go.mod replace` gaming
(recommended in D3) because the checks run in the agent's sandbox. We choose
the Job; this is the fallback only if the Job proves impractical, and it is
recorded here so the residual risk is explicit.

## Why

1. **The operator's determinism is real only if its input is evidence it
   obtained, not a claim the agent made.** The Job's exit code and the
   operator's own base-ref hashes satisfy that; `result.json` verify fields do
   not.
2. **It closes the gaming gaps the review named** (edited test + matching hash,
   fake `exitCode: 0`, baseline poisoning across iterations) by construction —
   the agent has no write path to the evidence.
3. **It keeps the runner simple** (ADR-0004 reason 3): the runner still only
   writes `result.json` (claims) and pushes its commit; it never computes or
   reports verify evidence.
4. **It is auditable**: every gate decision traces to a Job object the operator
   created (with a full pod log) and to hashes the operator computed from the
   base ref — both retrievable from the cluster, not from the agent's word.

## Consequences

- The operator gains `jobs` create/delete/list/get RBAC (in the Loop's
  namespace) and `pods/exec` (to read the claims `result.json`).
- A verify Job image is needed (a small image that checks out a git ref, runs
  commands, and reports via exit code). Phase 1 builds a minimal one.
- `loop.Status.Verify` fields are now **operator-written** (baseline from base
  ref, last check results from the Job) — the runner writes none of them.
- The Phase 1 B-slices change: **B2** (TamperedVerify) becomes
  "Job/Operator compares base-ref baseline to iteration-commit hashes," and
  **B3** (deterministic verify) becomes "operator reads Job status." **A4**
  (runner runs checks + reports exit codes) is **dropped** — the runner does
  not run or report checks.
- The B2 acceptance test (the core anti-gaming test): the agent edits a
  protected file **and** rewrites `result.json` to claim the baseline hash
  matches — the Loop still ends `Failed:TamperedVerify` (because the operator
  compares its own base-ref baseline to the iteration-commit hash, ignoring
  the forged `result.json`).
