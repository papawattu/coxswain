# ADR-0004 Amendment A1: as built

**Status:** Proposed (docs-only; to be reviewed per R31 I107).
**Date:** 2026-10-24
**Amends:** ADR-0004 (runner result file / phase claim channel) — rows 1-3 of
`docs/REVIEW-PHASE1-R31.md` (I107) and the corrections table
(`.samples/r31-adr-corrections.md`).
**Supersedes:** the two ADR-0004 rows quoted below. The original ADR is a
decision record and is not edited; this amendment records what was built.
**Code line numbers:** as of `6eb74c2` (main on 2026-10-24); later merges may
move them; the function names are the stable anchor.

## Context

The ADR-0004 design predates the S4 runner split and the D38 termination-message
pattern. Three of its claims no longer describe the code. Per I107, the
amendment records the built behaviour; the ADR text itself is left unchanged.

## Amendment

### Row 1 — drop `needs_input` (I107 row 1)

**ADR-0004 said** (`docs/adr/0004-runner-result-file-only.md:20-22`): the
runner's `result.json` carries `needs_input` ("status, summary, filesChanged,
verificationNotes, lessons, nextIterationPlan, needs_input"), and a
`needs_input` status is what triggers `AwaitingApproval`.

**What was built:** `result.json` has no `NeedsInput` field. The `Result`
struct is `runner/runner.go:123-129` — `{Status, Summary, FilesChanged,
VerificationNotes, NextIterationPlan, Lessons}` (plus `ToolTrace`,
`ObservedPhase`, `Iteration`, `PlanAttempt`, `LessonsAvailable`,
`HeadCommit`). `grep -rn -i 'needs_input\|NeedsInput' runner/` = 0 hits. The
`AwaitingApproval` phase exists (`api/v1alpha1/loop_types.go:111`) and is
reached only by the plan-approval gate and its I93 re-hold — the phase is
assigned at three sites, `internal/controller/loop_s4_phase.go:601`, `:625`
and `:883` (the last being the I93 plan-mismatch re-hold) — a phase transition
on a Planning claim in `ApprovalModeManual`, not on any result-file status
value. `CONTEXT.md:69` carried the same stale sentence; it is corrected
in this amendment's PR to match what is built.

**Decision now recorded:** the result file has no `needs_input` status. The
status values the runner writes are `success` / `blocked`; the `AwaitingApproval`
hold is a controller transition driven by the Manual plan gate, not by a
runner-reported status.

### Row 2 — the claim channel is the termination message (I107 row 2)

**ADR-0004 said** (`docs/adr/0004-runner-result-file-only.md:23`): the operator
reads `result.json` via `kubectl exec cat` (per ADR-0005/D2). (The same claim
appears at ADR-0005 `:122-123`.)

**What was built:** the live read is the **agent container termination
message**: `internal/controller/loop_s4_phase.go:313`
(`readPhaseClaimFromTerminationMessage` — an APIReader `Get` of the pod,
parsing `status.containerStatuses[agent].state.terminated` into the strict
claim object, `internal/controller/loop_s4_phase.go:26-28` / `:100-101`). The
runner mirrors the strict `{"observedPhase","status","blockedReason"}` object
into `/dev/termination-log` (`runner/phase.go:171-184`; the writer is
`writeClaim`, `runner/phase.go:658-`). `result.json` remains on the workspace
volume as the full record, but the operator's read-back is the termination
message, not an exec. The operator's own comment records the pattern: "no
exec, no kubelet ReadFile, and no new RBAC"
(`internal/controller/loop_s4_phase.go:52-54`). ADR-0005's D2 wording is
covered by the matching ADR-0005 amendment (#219).

**Decision now recorded:** the runner→operator claim channel is the agent
container's termination message (size-limited, strict-parsed by the operator,
never a gate input). `result.json` is the full result file on the workspace
for debugging and the ADR-0005 claims record; the operator does not read it
via `kubectl exec`.

### Row 3 — no `pods/exec` grant (I107 row 3)

**ADR-0004 said** (`docs/adr/0004-runner-result-file-only.md:63-65`): "The
operator gains `pods/exec` (read claims) and `jobs` create/delete/list/get
(obtain evidence) RBAC." (The same claim appears at ADR-0005 `:319-322`.)

**What was built:** `pods/exec` was never granted and is never needed.
`grep -rn 'verbs=.*exec' internal/` = 0 hits; a grep for
`resources=pods` finds the pods marker at
`internal/controller/loop_controller.go:549`
(`+kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch;create;update;delete`),
which has no exec verb, and the generated `config/rbac/role.yaml` pods rule
(`:9-20`) carries no `exec` verb and no subresource.

**Decision now recorded:** the operator's claim read needs no `pods/exec`
(the termination message arrives via the APIReader pod `Get`, covered by the
existing pod verbs). The ADR's `pods/exec` grant is over-stated and is dropped
from the record; the `jobs` verbs remain as built.

## What this amendment does NOT change

- **The result file itself** — `result.json` is still the runner's full
  output on the workspace; only the operator's read path (row 2) and two
  stale details (rows 1 and 3) are corrected.
- **ADR-0005** — the shared rows (D2, `:319-322`) are amended under
  ADR-0005's own amendment (#219); this amendment only records the ADR-0004
  side of them.
- **The `AwaitingApproval` phase** — it exists and is reachable; only the
  trigger record is corrected (row 1).
