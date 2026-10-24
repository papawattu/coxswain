# ADR-0005 Amendment A1: as built

**Status:** Proposed (docs-only; to be reviewed per R31 I107).
**Date:** 2026-10-24
**Amends:** ADR-0005 (the operator verifies via a pinned base commit +
isolated Job) — rows 2, 4, 8, 9, 10, 11 and 14 of
`docs/REVIEW-PHASE1-R31.md` (I107) and the corrections table
(`.samples/r31-adr-corrections.md`).
**Supersedes:** the ADR-0005 rows quoted below. The original ADR is a decision
record and is not edited; this amendment records what was built. Row 2's
shared ADR-0004 side is recorded in ADR-0004 Amendment A1.

## Context

The ADR-0005 design predates the S3a/S4 termination-message pattern (D38-style)
and the go-git-free operator. Several of its mechanism claims no longer
describe the code. Per I107, this amendment records the built behaviour; the
ADR text itself is left unchanged.

## Amendment

### Row 2 — how the operator reads the runner's result (I107 row 2)

**ADR-0005 said** (`docs/adr/0005-operator-verifies-via-isolated-job.md:122-123`):
the operator `exec`s `cat /workspace/.coxswain/result.json` into the sandbox
pod to read the runner's claims (the "resolves D2" section). (The matching
ADR-0004 wording at `0004:23` is amended under ADR-0004 Amendment A1, row 2.)

**What was built:** the live read is the **agent container termination
message**: `internal/controller/loop_s4_phase.go:313`
(`readPhaseClaimFromTerminationMessage` — an APIReader `Get` of the pod,
parsing the agent container's terminated status into the strict claim object).
The runner mirrors the strict `{"observedPhase","status","blockedReason"}`
object into `/dev/termination-log` (`runner/phase.go:171-184`; the writer is
`writeClaim`, `runner/phase.go:658`). The operator's own comment records the
pattern: "no exec, no kubelet ReadFile, and no new RBAC"
(`internal/controller/loop_s4_phase.go:52-54`).

**Decision now recorded:** the operator reads the runner's claims from the
agent container's termination message, not via `pods/exec cat`. `result.json`
remains the runner's full output on the workspace volume; the operator's
read-back channel is the termination message.

### Row 4 — the pod-errored reason and its bound (I107 row 4)

**ADR-0005 said** (`docs/adr/0005-operator-verifies-via-isolated-job.md:117-118`,
also `:213-214`): if the verify pod itself errored (crash, missing status), the
reason is `VerifyError`, "re-run once, then terminal" (the same name appears in
the D18 pod-selection bullet at `:213-214`).

**What was built:** there is no `VerifyError` reason. A verify infra failure
(pod crash, missing status) is bounded by `verifyInfraRetries = 2`
(`internal/controller/loop_verify_job.go:1620` — 1 original attempt + 2
recreations = 3 total runs) and the Failed condition reason is
`verifyInfraFailedReason = "VerifyInfraFailed"`
(`internal/controller/loop_verify_job.go:1626`). The only `VerifyError` hits in
the file are comments, not a constant or condition reason. `CONTEXT.md:57`
carried the same stale sentence; it is corrected in this amendment's PR.

**Decision now recorded:** the reason is `VerifyInfraFailed`, and the bound is
2 recreations (3 total runs), not "re-run once". The `VerifyError` name is
dropped from the record.

### Row 8 — no `pods/log` grant; failure text is the termination message (I107 row 8)

**ADR-0005 said** (`docs/adr/0005-operator-verifies-via-isolated-job.md:196-197`;
shared RBAC appendix at `:319-322`, cited once here for rows 8 and 9): the
operator gets a `pods/log` grant and pod logs are the feed-forward channel for
the next prompt.

**What was built:** there is no `pods/log` grant — `grep -c 'subresource\|secrets'
config/rbac/role.yaml` = 0 (the generated role has no pod subresource and no
secrets rule). The failure text that feeds the next prompt comes from the
failed check's **termination message**: `verifyFailureSummary`
(`internal/controller/loop_verify_job.go:1561`), recorded at
`status.verify.lastFailureSummary`.

**Decision now recorded:** the verify-failure feed-forward channel is the check
container's termination message, not pod logs; the `pods/log` grant was never
made and is not needed.

### Row 9 — no `secrets/get`; the git credential is a volume (I107 row 9)

**ADR-0005 said** (`docs/adr/0005-operator-verifies-via-isolated-job.md:271-272`,
shared RBAC appendix at `:319-322`, cited in row 8): the operator's RBAC adds
`secrets/get` in the Loop's namespace (D15, for go-git ref resolution).

**What was built:** there is no `secrets` rule — `grep -rn 'secrets'
config/rbac/` = 0 hits. The git credential is a **volume** mounted into the
`clone-base` init container only:
`internal/controller/loop_verify_job.go:606`
(`Name: "git-cred", MountPath: "/git-cred", ReadOnly: true`), which the kubelet
mounts without any operator RBAC.

**Decision now recorded:** the operator never reads the git credential through
the API; the credential reaches the Job as a volume (the D15 `secrets/get`
mechanism was never built — see row 10).

### Row 10 — no go-git; the base SHA comes from the init container's termination message (I107 row 10)

**ADR-0005 said** (`docs/adr/0005-operator-verifies-via-isolated-job.md:266-272`,
D15): the operator resolves refs in-process with go-git's `remote.List` (the
manager image is distroless, so no `ls-remote` binary).

**What was built:** there is no go-git dependency or call — `grep -n 'go-git'
go.mod go.sum` = 0 and `grep -rn 'go-git' --include=*.go internal/` = 0. The
base SHA comes from the workspace-init container's **termination message**:
`internal/controller/loop_controller.go:1152`
(`readBaseCommitFromTerminationMessage`, called at `:1140`).

**Decision now recorded:** ref resolution uses the init container's termination
message (the ADR's option (b), "a tiny resolve step that reports the SHA via
its termination message"), not in-process go-git. The property the ADR wanted
(the base SHA is operator-recorded, not the agent's word) holds.

### Row 11 — the pin is the runner's claim, not the operator's `ls-remote` (I107 row 11)

**ADR-0005 said** (`docs/adr/0005-operator-verifies-via-isolated-job.md:154-156`,
D11): the operator resolves the branch head *itself* at `Verifying` start
(`ls-remote`) and records it as the pin.

**What was built:** the pin is the **runner's claim**:
`internal/controller/loop_s4_phase.go:567`
(`VerifiedCommit: claim.HeadCommit`), shape-checked as 40-hex at
`internal/controller/loop_s4_phase.go:217-224`. The verify Job checks out the
pinned SHA out of the workspace PVC, and the deliver Job asserts the resolved
commit equals the pin.

**Decision now recorded:** the property holds — the pinned SHA is what verify
checks and deliver pushes. The mechanism differs from the ADR's letter: the pin
is taken from the runner's claim (the operator writes it, never the runner
directly), and the ancestry check that closes the force-push gap is F43.

### Row 14 — the built `verifiedCommit` fields, not `history[]` (I107 row 14)

**ADR-0005 said** (`docs/adr/0005-operator-verifies-via-isolated-job.md:156`,
also `:331-335`): `status.history[n]` entries gain `verifiedCommit` and record
which checks were not run.

**What was built:** there is no `status.history[]` (the only `History` hits in
`api/v1alpha1/loop_types.go` are `StallHistory` and `StallEntry`). The built
fields are `status.currentVerify.verifiedCommit`
(`api/v1alpha1/loop_types.go:393`) and `status.verify.verifiedCommit`
(`api/v1alpha1/loop_types.go:286`); the not-run set is a **nil entry** in
`status.verify.lastCheckResults` (`api/v1alpha1/loop_types.go:314-327` — "A nil
entry means that check did not run (I14 NotRun)").

**Decision now recorded:** the per-iteration verified SHA lives in
`status.currentVerify.verifiedCommit` (the pin for the current iteration) and
`status.verify.verifiedCommit` (the SHA the evidence was measured at); the
not-run set lives in `status.verify.lastCheckResults`. The `history[]` framing
is dropped from the record.

## What this amendment does NOT change

- **The pinned-SHA property** (row 11) — the Job verifies the SHA that gets
  delivered, immutable per iteration; only the pin's source is corrected.
- **The ADR-0005 rows that are build items** (rows 5, 6, 7, 12, 13 in I107) —
  those are queued as F-items (F53(a), F53(b), and new F-items) and are out of
  scope for this docs amendment.
- **The ADR-0004 side of row 2** — recorded in ADR-0004 Amendment A1; this
  amendment records the ADR-0005 side only.
