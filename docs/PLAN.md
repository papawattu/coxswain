# Coxswain: build plan

**Goal:** a Kubernetes operator that runs one-shot **plan → implement → verify** Loops for coding agents. Each Loop runs in an isolated [agent-sandbox](https://github.com/kubernetes-sigs/agent-sandbox) workspace, stops on success, budget, or stall, and hands back a draft PR. A Loop is single-goal and terminal; new direction is a **fork** from a checkpoint, never a re-task (ADR-0003).

**Canonical terms** live in [CONTEXT.md](../CONTEXT.md). The operator is a **deterministic state machine**: it reads exit codes, the runner's `result.json`, hashes, and counters, and makes every phase/budget/stall/approval decision as a pure function. It never interprets content itself — the single controlled exception is the rule-gated **Memory Curator** LLM (ADR-0002). All auditability comes from `status.history[]` + Kubernetes events, never logs alone.

**Stack:** Go, kubebuilder v4.16, controller-runtime v0.25. Workspaces run on agent-sandbox (v1beta1: `Sandbox`, `SandboxTemplate`, `SandboxClaim`, `SandboxWarmPool`). Dev targets k8s 1.32 (kind stable); production requires k8s ≥1.37 (agent-sandbox requirement). Agents are pluggable behind the runner contract — start with a direct Claude API call, add kagent/Codex later.

**Identity:** keep the name "Coxswain" (ADR-0001). Go module path is `github.com/papawattu/coxswain`. The CRD API group is `coxswain.wattu.com` (owned domain — `cox.dev` is taken, and `cox.cox.dev` was renamed in commit e5027e9).

Timelines assume part-time solo work.

## Phase 0: Foundations (week 1)

- Scaffold the repo with kubebuilder v4: group `coxswain.wattu.com/v1alpha1`, `kind: Loop`. (Module path is the GitHub path; the CRD API group `coxswain.wattu.com` is under a domain we own — the CNCF-landscape check remains a pre-Phase-6 item.)
- Set up a kind cluster (**k8s 1.34**, pinned — D5) with the agent-sandbox CRDs applied.
- Create the Loop CRD (`api/v1alpha1`), the operator that creates a `Sandbox` per Loop, and the runner module (a Go binary that calls an OpenAI-compatible model and writes `result.json`).
- Add CI: golangci-lint, unit tests, envtest (apply the agent-sandbox CRDs into the envtest setup before starting).

**Done when (narrowed, I4):** `kubectl apply` of a Loop causes the operator to **create and log the Sandbox object** for it (the Sandbox is the operator's own output; its Pod does not have to appear — running the agent-sandbox controller that materialises the Pod is Phase 1). The e2e proves exactly this on a kind cluster (see `docs/E2E-PHASE0.md`). The runner-in-sandbox run and the agent-sandbox controller were **moved to Phase 1** rather than closing them here, so Phase 0's "done when" matches what was actually proven.

> **Phase 0 sign-off (I4, decided 2026-09-27):** closed by the narrow done-when above. The runner-in-sandbox smoke run and running the agent-sandbox controller are Phase 1 work. **D22 (2026-09-27) confirmed the controller does have a pullable image** — `registry.k8s.io/agent-sandbox/agent-sandbox-controller:v1.0.4` from the v1.0.4 *release* manifest (the source `k8s/controller.yaml` is a `ko://` placeholder, which is what made Phase 0 think no image existed) — and that it runs + reconciles on kind 1.34.

## Phase 1: The loop, minimal (weeks 2–3)

**The trust model lands here, not in Phase 6.**

- **(Moved from Phase 0, I4)** Bring the sandbox to life end-to-end: run the agent-sandbox controller in the kind cluster (D22: the v1.0.4 release manifest installs it, image `registry.k8s.io/agent-sandbox/agent-sandbox-controller:v1.0.4`; it reconciles a `Sandbox` to a Running pod on kind 1.34) so the operator's `Sandbox` materialises a Pod, and smoke-run the runner *inside* the sandbox (it calls the model and writes `result.json` the controller logs). Phase 0 proved the operator creates the Sandbox object; this is the step that proves the agent actually runs in it. Opt into the per-Sandbox Service with `spec.service: true` where a Service is needed (D22).
- Write the Loop CRD types:
  - `spec.goal` (string)
  - `spec.workspace`: `{ repo, ref, gitCredentialSecret }` — clone at `ref`, one branch per Loop (`coxswain/<loop-name>`), pushed after each iteration
  - `spec.verify.acceptanceChecks[]` — user-authored commands; the **only** gate to Succeeded
  - `spec.loop`: `{ maxIterations, phaseTimeout }`
  - `spec.approval`: `{ mode: Auto|Manual, onReject: Replan|Fail }`
  - `spec.suspend` (bool)
- Build the **Runner** as the in-sandbox phase driver (replaces the smoke-test runner): the operator sets `status.desiredPhase`, the runner does the work and writes `status.observedPhase` + `result.json`. Model context survives across phases within an iteration.
- Build the reconcile state machine over the settled phase enum:
  `Pending → Planning → [AwaitingApproval] → Implementing → Verifying → { Succeeded | →Implementing | Failed | Paused }`, plus `CleaningUp`.
- **Protected paths from day one:** hash the acceptance-check source files at iteration start; if they changed during implement, fail the iteration with `TamperedVerify` **before any check runs** (terminal).
- Make verify deterministic only: run each acceptance check, exit code 0 = pass.
- Feed failures forward: write verify output into the next implement prompt.
- Record `status.phase`, `status.iteration`, and `status.history[]`.

**Done when:** a Loop on a toy repo with a failing acceptance test iterates until the test passes, or stops at `maxIterations`. Tampering with a test file produces `TamperedVerify`, not a pass.

## Phase 2: Stopping properly (week 4)

- **Stall detection:** hash the normalised failing-check output (strip timestamps, hex addresses, temp paths, line numbers — version the rules in `status.stallHistory[].normalisationVersion`). After **N consecutive** identical hashes (default 3, `spec.loop.stallAfter`), apply `spec.loop.stallAction`: `Fail` → `Failed:Stalled`, `Pause` → `Paused`, `Continue` → warn and keep going. Consecutive only; non-adjacent repetition is oscillation (deferred). Global default via a cluster-wide ConfigMap, overridable per Loop.
- **Budgets:** a **metering sidecar** (thin HTTP proxy in front of the model API, in the sandbox) counts tokens and writes counters to a file. It *reports only*. The operator reads counters into `status.budget` and applies `spec.budget.onExceeded: Pause|Fail` when any cap (`maxTokens`, `maxWallClock`, `maxCostUsd`) is hit. Cost is **derived** (tokens × price from a cluster-wide `coxswain-model-prices` ConfigMap, overridable per Loop), not measured.
- Add the `Paused` phase with `status.pausedFrom`; resume via `spec.suspend=false` or annotation returns to the exact phase.
- Add conditions and events for every transition.

**Done when:** a deliberately impossible goal stops cleanly with the right reason (`Stalled` or `BudgetExceeded`) instead of spinning, and a paused Loop resumes where it left off.

## Phase 3: Durability (weeks 5–6)

> **D6 (decided 2026-09-27):** Phase 3 snapshot e2e runs on **kind + csi-hostpath-driver** — kind gets the `VolumeSnapshot` CRDs + the hostpath CSI driver so the whole flow (incl. checkpoints) stays on one local cluster. (The homelab Ceph RBD alternative was rejected to avoid a second cluster.)

- Take a **VolumeSnapshot** after each verify; store the reference in `status.checkpoint`. Name `checkpoint-<loop-name>-iter-<N>`.
- Checkpointing is **mandatory**: if the StorageClass doesn't support snapshots, fail at start with `CheckpointUnavailable` (terminal) rather than run without durability.
- Retention: last 3 + all milestone (successful-verify) checkpoints; GC the rest.
- Resume after a controller or pod restart from the last checkpoint.
- Add **rollback** (`spec.restoreFrom: iter-2`) and **fork** (a new Loop seeded from another Loop's checkpoint).
- Add finalizers to clean up sandboxes and snapshots, in `CleaningUp`.

**Done when:** killing the sandbox mid-implement resumes correctly, and a `cox fork --from iter-2` produces an independent Loop.

## Phase 4: Humans in the loop (week 7)

- Add the plan approval gate: `spec.approval.mode: Manual` sets `AwaitingApproval` until a `PlanApproved` condition is set. The condition carries the **SHA-256 of `PLAN.md`**; if the plan changes after approval, the condition clears and the Loop re-enters `AwaitingApproval`.
- Rejection: `PlanRejected` with a reason; `onReject: Replan` (default) feeds the reason into the next plan prompt, `onReject: Fail` stops the Loop.
- Build the **kubectl-cox** plugin (talks directly to the CRD — no operator HTTP API): `approve`, `reject --reason`, `pause`, `resume`, `logs`, `history`, `diff`, `plan`, `describe`, `fork --from iter-N`, `memory <repo-url>`.

**Done when:** you can review a plan, reject it with feedback, and see the re-plan.

## Phase 5: Shared memory (week 8)

Self-learning across Loops, per repo.

- **Shared memory** is cluster-local: a PVC mounted into sandboxes, keyed by git URL. Read at Loop start into the workspace at `.coxswain/memory/`; the runner includes it in its prompt.
- **Write policy (static rules, enforced by the operator in code):** a lesson is eligible only if the Loop reached `Succeeded` or `Failed:Stalled`, the runner's `result.json.lessons[]` is non-empty, and each lesson is ≤500 chars, references at least one file path or error signature, passes a secrets regex-scan, and is new by hash (dedup).
- **Memory Curator (ADR-0002):** only when all static rules pass does the operator invoke the curator LLM to decide "worth keeping?" The verdict (boolean + one-line reason) is written to the audit trail; the curator never rewrites lesson text. A stall can seed a negative lesson (configurable; default on for `Fail`/`Pause`, off for `Continue`).
- Add `cox memory export` / `import` (a tarball of the memory dir) for explicit, manual cross-cluster sharing. Never automatic or distributed.

**Done when:** a second Loop on the same repo starts with the first Loop's approved lessons in its prompt, and the audit trail shows exactly which lessons were written and why.

## Phase 6: Output and judgement (week 9)

- Open a **draft PR** on success: the **runner** (already git-authenticated) pushes the branch and creates the PR. Description: goal, iteration count, full `status.history[]`, the plan, and a link to the Loop object. `spec.pr.ready: true` overrides draft. The operator reads `status.prUrl` and does not watch the PR's fate.
- Add the optional **Judge** (sidecar) after deterministic checks. It produces a boolean + score, opaque to the operator, and can **never** be the only gate — acceptance checks always must pass.
- Add `spec.verify.onVerifyFail: Plan` to re-plan from scratch instead of re-implementing.

**Done when:** a successful Loop produces a reviewable draft PR with its iteration history attached.

## Phase 7: Hardening and release (weeks 10–11)

- RBAC least privilege, a default NetworkPolicy per sandbox, secrets for model keys and git tokens.
- Metrics: iterations per Loop, pass rate, tokens per success, stall rate, memory-write rate.
- A Helm chart, docs, three example Loops, and a demo video.
- Confirm the `coxswain.wattu.com` group has no collision in the CNCF landscape entry (the group itself is under a domain we own).
- Tag v0.1.0.

## Decisions already made (from the design session)

These are settled — see ADRs and CONTEXT.md. Listed here so the plan and the model agree.

1. **One sandbox per Loop** (state carries over naturally).
2. **Runner contract** = prompt in, `result.json` + files out. The only runner→operator channel.
3. **Where verify runs** = in the same sandbox, with protected acceptance-check paths (tamper ⇒ `TamperedVerify`, terminal).
4. **Operator is deterministic**; memory curation is the one rule-gated LLM exception (ADR-0002).
5. **Loop is one-shot**; forks, not re-tasks (ADR-0003).
6. **kubectl-cox talks to the CRD directly**, no operator HTTP API.
7. **PR opened by the runner**, draft by default, operator doesn't watch it.
8. **Shared memory is cluster-local** with manual export/import; never distributed.
9. **Phase 0 "done when" is narrow (I4, 2026-09-27):** the operator creating + logging the Sandbox object. The runner-in-sandbox smoke run and the agent-sandbox controller move to Phase 1.
10. **Dev/CI k8s version is 1.34 (D5, 2026-09-27):** kind + envtest + CI all pin 1.34; the production floor stays >=1.37.
11. **Phase 3 snapshot e2e runs on kind + csi-hostpath-driver (D6, 2026-09-27):** kind gets the snapshot CRDs + hostpath CSI so the whole flow stays on one local cluster.
12. **agent-sandbox v1.0.4 runs on k8s 1.34 (D22, 2026-09-27, smoke-verified):** the controller installs (image `registry.k8s.io/agent-sandbox/agent-sandbox-controller:v1.0.4`, from the *release* manifest — the source `k8s/controller.yaml` is a `ko://` placeholder) and reconciles a bare `Sandbox` to a Running pod on kind 1.34, so dev/CI pinning 1.34 is safe. The per-Sandbox **Service is opt-in** (`spec.service: true`), not created by default — so the D20 DNS-1035-name constraint only matters when we opt into a Service. The vendored CRD at `config/crd/external/` already matches v1.0.4 (it has `spec.service`, `shutdownPolicy`, `shutdownTime`), so no re-vendoring is needed. The earlier "needs >=1.37" claim was folklore — v1.0.4's docs state no minimum server version and it reconciles on 1.34; the production floor stays >=1.37 only because the operator is expected to track the agent-sandbox release line, not because 1.34 was broken.

## Risks

- **The agent games verify.** Closed from Phase 1: acceptance checks are user-authored and their paths are protected (`TamperedVerify`, terminal). Agent-authored checks are never the gate.
- **Cost blowouts.** Budgets enforced from Phase 2, metered by a sidecar the operator doesn't trust to decide — only to report.
- **Oscillation (not stall).** An agent that alternates between two failure modes never trips the consecutive-hash stall detector. Deferred; needs its own detector. Flagged, not scheduled.
- **Memory pollution.** A bad lesson, once written, biases every future Loop on that repo. Mitigations: the curator gate, dedup, and `cox memory` inspection + deletion. (No automated forgetting yet — a future concern.)
- **Overlap with kagent or agent-sandbox.** Stay the orchestration layer on top of them; the runner contract is the seam. Contribute upstream where it makes sense.
- **agent-sandbox is v1beta1 / fast-moving.** The API can move under us. **Dev/CI pin k8s 1.34 (D5); agent-sandbox v1.0.4 is smoke-verified on 1.34 (D22), and the production floor stays >=1.37 only to track the agent-sandbox release line, not because 1.34 is unsupported.** The per-Sandbox Service is opt-in (`spec.service: true`) and the vendored CRD already matches v1.0.4. Isolate the dependency behind a thin internal interface so an upgrade is contained.

## Before Phase 0

- [x] Confirm the API group string is unused — done by renaming the group to `coxswain.wattu.com` (owned domain; commit e5027e9). CNCF landscape check for a "Coxswain" entry remains below.
- [ ] Check the CNCF landscape for an existing "Coxswain" entry
- [ ] Read the agent-sandbox README end to end; note install prerequisites and the `sandboxd` model
