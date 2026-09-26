# Phase 0 — TDD test plan

Red → green → refactor, one vertical slice per cycle. Seams confirmed with the
user before the first test (per the TDD skill: no test is written at an
unconfirmed seam).

The **seam** for the runner is the `result.json` file + the workspace — observed
through the file, never through the model-call internals. The **model is the one
system boundary** and is faked with a local `httptest` server (the runner takes a
base URL, the test points it at the fake). No mocking of the runner's own code.

## Runner (module: `runner/`)

| # | Slice (behavior, not implementation) | Seam | Status |
|---|---------------------------------------|------|--------|
| R1 | Given a prompt, the runner writes a `result.json` with `status:"success"` and a non-empty `summary` | result file | ✅ green |
| R2 | The runner sends the prompt as the user message to the model endpoint | fake model (request body) | ✅ green |
| R3 | A `shell` tool call from the model is executed in the workspace; its output is fed back | workspace + fake model | ✅ green |
| R4 | When the model endpoint is unreachable, the runner still writes a `result.json` (non-panic, `status:"blocked"`) | result file | ✅ green |
| R5 | `result.json` is written to `$COX_WORKSPACE/.coxswain/result.json`, creating parent dirs | result file path | ✅ covered by R1 (same path assertion — a separate test would be a tautological duplicate) |

## Operator (module: `internal/controller`)

The controller's seam is the cluster state after `Reconcile`, observed through the
client (envtest). The controller+CRD already have a passing test (S0), written
after-the-fact — kept. Adding the missing seams:

| # | Slice | Seam | Status |
|---|-------|------|--------|
| S0 | Loop → a Sandbox owned by it exists; phase set to Pending; idempotent on re-reconcile | envtest | ✅ green (written after code — noted) |
| S1 | A Loop with `spec.suspend:true` does not run a Running sandbox | envtest | ⬜ red |
| S2 | A pre-existing Sandbox (from a prior controller instance) is not re-created | envtest | ⬜ |
| S3 | `status.observedGeneration` tracks spec changes | envtest | ⬜ |

## Out of scope for Phase 0

The full phase state machine, budgets, stall detection, checkpointing, and memory
curation are Phase 1+. Testing them now would be horizontal slicing (testing the
shape of imagined behavior before it exists).
