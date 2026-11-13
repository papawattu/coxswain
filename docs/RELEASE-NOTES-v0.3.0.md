# Release notes: v0.3.0

Draft: the `TODO(<slice>)` lines are filled as J5a, M1, S1 and J5b merge.
The pre-tag smoke and the v0.2.0 upgrade proof are added by the reviewer (R2's
method, R1's upgrade method).

Entries are added per merged PR, each citing the PR and the squash commit it
landed as in the heading, like `RELEASE-NOTES-v0.2.0.md`.

## Summary

- **The Judge is draft-forcing beyond false** (J3, #482, 85d62c2): a Judge verdict that is `Unknown` (no verdict recorded) or whose diff was truncated now forces delivery into a draft PR, whatever `spec.delivery.draft` says, like a false verdict (D133). A clean true verdict or no Judge leaves `spec.delivery.draft` exactly as it is.
- **The `Judge` column** (J4, #486, f70660a): `kubectl get loop` shows a one-word `Judge` column from the new top-level `status.judgeSummary` field: `False` > `Truncated` > `Unknown` > `True` (the most serious signal wins); empty when the Loop has no Judge (D131, D138).
- **Measured: the Judge catches the #371 probe** (E0, #485, 0deb5a6): the planted probe ran on kind-ref against the released v0.2.0 operator with the Judge on. The canary, the tamper check and all three acceptance checks passed with the repo still broken and the Loop Succeeded; the Judge gave verdict false, score 0, and the delivery was a draft PR. The result and its limits (n=1, one model, an easy-to-read probe) are under a new "Measured" heading in `docs/CANARY-CHANNEL.md`.
- **The Judge can ask for its own model** (J5a, #488, c8f56e3): `spec.verify.judge.model` names the model the Judge asks for, through the Loop's own model proxy and endpoint (D132). Empty means the Loop's `spec.agent.model` (D123), so existing Loops are unchanged.
- **arm64:** built, not run (D137, carried over from v0.2.0).
- **A Judge-flagged Loop writes no lessons** (M1, #487, 8f73d0b): a Loop the Judge flags (verdict `false`, `Unknown`, or a truncated diff: the D133 set) creates no memory-writer Job and records `MemoryWriteDone` with the reason `LessonsWithheld`, plus a Normal Event of the same reason (D140).
- TODO(S1): filled when S1 merges
- TODO(J5b): filled when J5b merges

## Upgrade

### The `Judge` column and `status.judgeSummary` (J4, #486): a CRD change — re-apply the CRD before the new operator

The Loop CRD gains a new top-level status field `status.judgeSummary` and the new `Judge` print column (jsonPath `.status.judgeSummary`). As for v0.2.0: apply this release's `install.yaml` (or chart) so the CRD is current **before** the new operator starts, so the operator's status writes are not pruned and the column renders.

### The new Judge fields (`spec.verify.judge.model`, J5a; TODO(J5b) `judge.modelEndpoint` + `judge.endpointSecretRef`)

Both are new optional fields on `spec.verify.judge`. A Loop without them is unchanged: the Judge uses the Loop's model and the Loop's own model proxy endpoint. (Filled when the slices merge.)

### The diff scan's init container (TODO(S1))

S1 adds a trusted init container to the verify Job. (Filled when S1 merges.)

### The scan can flag Loops WITHOUT a Judge (TODO(S1))

The diff scan's `VerifySuspicious` flag applies to every Loop, with or without `spec.verify.judge`: a hit on a Judge-less Loop forces a draft and shows in the `Judge` column (S1 sets `Suspicious` on `status.judgeSummary` itself). (Filled when S1 merges.)

## New features

### J3: an Unknown or truncated Judge verdict forces a draft PR (D133) — #482 (85d62c2)

**What was wrong:** a Judge that could not give a verdict (unavailable, so `Unknown`), or that saw only a truncated diff, left `spec.delivery.draft` exactly as it was — only a false verdict held the PR as a draft.

**What changed:** `deliverDraft` (`internal/controller/loop_deliver_job.go`) now returns draft when `status.judge` exists and its verdict is false, its verdict is nil (Unknown), or its diff was truncated; the PR description's Judge line notes "(the PR is held as a draft)" in those cases. A clean true verdict, or a Loop with no `status.judge`, is unchanged.

**How to use it:** nothing; it is delivery behaviour, not a new field.

**On upgrade:** a Loop that was delivered non-draft after an `Unknown` or truncated Judge is now delivered as a draft.

### J4: `status.judgeSummary` and the `Judge` column in `kubectl get loop` (D131, D138) — #486 (f70660a)

**What it is:** the new top-level `status.judgeSummary` holds the one-word Judge signal: `False`, `Truncated`, `Unknown` or `True` (the most serious wins; the `JudgeVerdict` condition keeps the detail). It is set by the verdict recorders and the verify outcome gates, and the CRD adds the `Judge` print column on `kubectl get loop`. Empty when the Loop has no Judge.

**Why a top-level field:** `status.judge` is nil for a Loop without a Judge; S1 sets `Suspicious` on Loops that have no Judge, and creating `status.judge` there would read as an Unknown verdict (which J3 turns into a draft).

**On upgrade:** a new optional status field and a new column; nothing changes for Loops without a Judge (the column is empty). See the CRD note in Upgrade.

### E0: the Judge flags the #371 probe on kind — #485 (0deb5a6)

Docs only. The E0 canary run (planted probe on kind-ref, released v0.2.0 operator, Judge on) is recorded under the new "Measured" heading in `docs/CANARY-CHANNEL.md`, with the evidence pointer (`.samples/e0-run19/`) and its stated limits. It also produced the finding behind M1 (D140): the Memory Curator kept a lesson from the flagged Loop calling the probe harmless.

### H0: docs — a release-page notice rule, and the Judge vocabulary as built — #483 (1f5fdae)

Docs only. AGENTS.md gains the rule that a defect found after a release gets a notice on that release's GitHub page (D136). `docs/OPERATOR.md` and `docs/QUICKSTART.md` drop the stale "alphas are prereleases" pin-the-alpha wording (v0.2.0 is now a full release), and the Judge wording matches the build.

### DOC1: the README outlines the core concepts — #479 (bb6cc88)

Docs only. The README's "How it works" intro is replaced by a "Core concepts" outline: one short bullet per concept (Loop, Phase, Iteration, Operator, Runner, Sandbox and AgentPolicy, Acceptance checks, Judge, Delivery, Budget and stall, Model proxy and metering, Shared memory and lessons, Checkpoint).

### D139: a release tag publishes a full release, not a prerelease — #484 (2ec9cbe)

The release workflow no longer passes `--prerelease` to `gh release create`, so the v0.3.0 tag publishes as a full release and GitHub's `releases/latest` points at it with no manual promote.

### J5a: `spec.verify.judge.model` — the Judge's own model (D132) — #488 (c8f56e3)

**What it is:** `spec.verify.judge.model` (optional, MaxLength 253) names the model the Judge asks for, through the Loop's own model proxy and endpoint (D132). When empty, the Judge asks for the Loop's `spec.agent.model` (D123), so existing Loops are unchanged.

**How it works:** the verify Job's `COX_JUDGE_MODEL` is set to `spec.verify.judge.model` when it names a model, else to the Loop's model (`internal/controller/loop_judge_job.go`'s `judgeEnvValueMap`); the call still goes through the Loop's model proxy — no new credential or egress. The proxy's usage reading still names the Loop's model for every call, the Judge's included (ADR-0011's 2026-11-13 amendment).

**On upgrade:** a new optional field; a Loop without it is unchanged. TODO(J5b): the field is proven on kind with J5b (the lab endpoint serves no clean second model on one address).

**Covered by:** unit specs for the `COX_JUDGE_MODEL` choice in `internal/controller/loop_j5a_judge_model_unit_test.go` (the Judge's own model when named, the Loop's model when empty, and no key with no endpoint).

### M1: a Judge-flagged Loop writes no shared-memory lessons (D140) — #487 (8f73d0b)

**What it is:** a Loop the Judge flags — verdict `false`, verdict `Unknown` (no verdict recorded), or a truncated diff: the D133 set that forces a draft — writes NO shared-memory lessons (D140). No memory-writer Job is created; the `MemoryWriteDone` condition records the reason `LessonsWithheld`, with a Normal Event of the same reason (`internal/controller/loop_memory_writer_job.go`, `judgeWithholdsLessons`). TODO(S1): the diff scan's `VerifySuspicious` hit joins the same rule.

**Why:** E0 (the "Measured" section of `docs/CANARY-CHANNEL.md`) found the Memory Curator keeping a lesson from the flagged #371 probe Loop — a lesson that calls the probe harmless (D140, `docs/PLAN-PHASE8.md`). A gamed Loop's lessons would teach the next Loop the trick.

**On upgrade:** a Loop that ends after a Judge flag no longer adds lessons to the shared memory. A Loop without a Judge, or with a clean true verdict, is unchanged.

### TODO(S1): the diff scan — VerifySuspicious (D135)

Filled when S1 merges.

### TODO(J5b): `judge.modelEndpoint` + `judge.endpointSecretRef` (D132, D141)

Filled when J5b merges.

## Known issues

- `make docker-build` fails on Docker without buildx, since C1 (#473) set `export DOCKER_BUILDKIT=1` in the Makefile. Build on a Docker that has buildx, or install buildx.
- `kind load docker-image` of the multi-arch release tag fails when only one platform was pulled: kind imports every platform in the manifest. Load it with `docker save --platform linux/amd64 -o img.tar <image>` then `kind load image-archive img.tar`.
