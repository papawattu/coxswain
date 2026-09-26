# Coxswain

A Kubernetes operator that runs long-lived plan → implement → verify loops for coding agents. Each loop runs in an isolated workspace and stops on success, budget, or stall.

## Language

**Operator**:
The control plane. A deterministic state machine for phase transitions, budgets, stalls, and approvals. It reads exit codes, result files, hashes, and counters. It does not judge *quality* of agent output. The one exception: it triggers the Memory Curator (an LLM) on rule-gated events. All auditability comes from what the operator *does* (events, conditions, transitions), not what it *inspects*.
_Avoid_: controller, brain, judge

**Memory Curator**:
An LLM invocation, run by the operator, that decides whether a lesson is worth writing to shared memory. Triggered only when static rules pass (Loop ended, lessons[] present, no secrets, dedup check clear). Its output is a boolean plus a one-line reason, written to the audit trail. It never rewrites or modifies the lesson text.
_Avoid_: curation, reviewer, filter

**Runner**:
The in-sandbox daemon that executes phases and talks to a model. Pluggable: Claude API, kagent, Codex. The only component allowed to inspect workspace contents. The operator sets `status.desiredPhase`; the runner does the work and writes `status.observedPhase` plus the result file. Runs continuously for the Loop's life; model context survives across phases within an iteration. If `observedPhase` doesn't advance within `spec.loop.phaseTimeout` (default 30 min), the operator sets `Failed:PhaseTimeout` and restarts the sandbox from the last checkpoint.
_Avoid_: phase driver, agent, worker, executor, bot

**Judge**:
An optional sidecar that produces a pass/fail verdict after deterministic verify checks. Its output is opaque to the operator — a boolean plus a score. It can never be the only gate.
_Avoid_: reviewer, evaluator, critic, LLM judge

**Plan**:
The runner's written proposal for what it will do, at `/workspace/.coxswain/PLAN.md`. A ≤4KB summary lives in `status.plan.summary`. With `spec.approval: Manual`, the Loop stops at `AwaitingApproval` until a human sets `PlanApproved` carrying the SHA-256 of PLAN.md. If PLAN.md changes after approval, the condition clears and the Loop re-enters `AwaitingApproval`. Rejection feeds the reason into the next plan prompt (configurable: `Replan` or `Fail`).
_Avoid_: proposal, design, spec

**Version target**:
Development targets k8s 1.32 (kind stable). Production requires k8s ≥1.37 (agent-sandbox requirement). Go 1.26, controller-runtime v0.25, kubebuilder v4.16. The Phase 0 runner image is a Go binary that calls the model API, exposes shell/fs/git tools, and writes result.json — no phase-driver state machine yet.
_Avoid_: target, stack

**kubectl-cox**:
A kubectl plugin that talks directly to the CRD (no operator HTTP API). Commands: `approve`, `reject --reason`, `pause`, `resume`, `logs`, `history`, `diff`, `plan`, `describe`, `fork --from iter-N`, `memory <repo-url>`. Every command is a CRD mutation or a `kubectl exec`.
_Avoid_: CLI, tool, kubectl plugin

**PR**:
Opened by the runner on Succeeded. Draft by default; `spec.pr.ready: true` overrides. Description includes: goal, iteration count, full `status.history[]`, the plan, and a link to the Loop object. The operator reads `status.prUrl` from the result file and does not watch the PR's fate — Succeeded is terminal.
_Avoid_: pull request, merge request, branch

**Failure reason**:
The string attached to `Failed` or a warning. Terminal reasons: `TamperedVerify`, `CheckpointUnavailable`, `MaxIterations`, `PlanRejected` (onReject=Fail), `BudgetExceeded` (onExceeded=Fail), `Stalled` (stallAction=Fail). Resumable reasons: `RunnerNoResult`, `PhaseTimeout`, `SandboxCrash`, `VerifyError` (re-run once, then terminal if it crashes again), `GitPushFailed`. `BudgetExceeded` and `Stalled` go to Paused instead of Failed when their configured action is Pause.
_Avoid_: error, fault, exception

**Phase**:
One of: Pending, Planning, AwaitingApproval, Implementing, Verifying, Succeeded, Failed, Paused, CleaningUp. Succeeded and Failed are terminal. Paused stores `status.pausedFrom` and resumes to that exact phase. CleaningUp is where finalizers delete the sandbox and GC old checkpoints; the Loop object is deleted only after CleaningUp completes.
_Avoid_: state, stage, step

**Result file**:
`/workspace/.coxswain/result.json` — the only output the operator reads from the runner. Schema: `status` (success|blocked|needs_input), `summary`, `filesChanged[]`, `verificationNotes`, `nextIterationPlan?`, `lessons[]`. If the runner crashes without writing it, the iteration is `Failed:RunnerNoResult`. `status: needs_input` triggers `AwaitingApproval`.
_Avoid_: output, report, response

**Prompt**:
The operator-built input to the runner: goal + iteration number + previous verify output + memory file + approved plan. A single text block. The runner has no other channel to the operator.
_Avoid_: instruction, task, input

**Checkpoint**:
A VolumeSnapshot of the workspace PVC taken after each verify. Named `checkpoint-<loop-name>-iter-<N>`. Retention: last 3 + all milestone (successful verify) checkpoints; older non-milestones are GC'd. The git branch is the recovery mechanism; the checkpoint is the durability mechanism. Checkpointing is mandatory — if the StorageClass doesn't support snapshots, the Loop fails at start with `CheckpointUnavailable`.
_Avoid_: save point, backup, restore point

**Stall**:
N consecutive iterations with the same normalised failure hash. Default N=3. On stall, the operator applies the configured action: `Fail` (stop, reason `Stalled`), `Pause` (stop, resumable), or `Continue` (log a warning, keep going). The action is set at `spec.loop.stallAction` (per-Loop) or `spec.loop.stallAction` defaulted from a cluster-wide ConfigMap. A stall can also trigger a Memory Curator write (configurable, default on for Fail and Pause, off for Continue).
_Avoid_: deadlock, loop, spin

**Budget**:
A set of caps (tokens, wall clock, cost) on a Loop. The operator reads counters from a metering sidecar and applies `spec.budget.onExceeded` (Pause | Fail) when any cap is hit. Cost is derived (tokens × price from a ConfigMap), not measured. Caps and policy are per-Loop; price table is cluster-wide with per-Loop override.
_Avoid_: limit, cap, quota

**Metering sidecar**:
A thin HTTP proxy in front of the model API, inside the sandbox. Counts tokens from responses, writes counters to a file. Reports only — the operator makes the pause/fail decision.
_Avoid_: proxy, gateway, interceptor

**Workspace**:
The git clone inside the sandbox. Created at Loop start from `spec.workspace.repo` + `spec.workspace.ref`. One branch per Loop (`coxswain/<loop-name>`), pushed after each iteration. Credentials via `spec.workspace.gitCredentialSecret`.
_Avoid_: repo, code, sandbox fs

**Shared memory**:
A persistent per-repo store of lessons that survive across Loops. Keyed by git URL. **Cluster-local**: backed by a PVC mounted into sandboxes, so "shared" means shared within one cluster. A new cluster starts with empty memory. Written only when the Memory Curator approves. Read at Loop start into the workspace at `.coxswain/memory/`. The operator triggers writes; the runner reads. Cross-cluster sharing is manual and explicit: `cox memory export` / `cox memory import` (a tarball of the memory dir), added in a later phase. Never automatic or distributed.
_Avoid_: knowledge base, RAG, vector store

**Lesson**:
A single entry in shared memory: ≤500 chars, references at least one concrete file path or error signature, produced by the runner in its `result.json.lessons[]` field. Deduplicated by hash.
_Avoid_: note, insight, tip

**Iteration**:
One plan→implement→verify cycle. Each iteration produces a commit on the Loop's branch. `status.iteration` is the 1-based count.
_Avoid_: round, step, pass

**Acceptance checks**:
User-authored verify commands in the Loop spec. The only gate that can flip a Loop to Succeeded. Their source files are protected paths — the operator diffs them after each implement and fails the iteration if they changed.
_Avoid_: tests, verify, gates

**Agent checks**:
Tests the runner writes in the workspace during implement. Useful signal fed back into the next prompt, but never the final gate.
_Avoid_: unit tests, self-tests

**Loop**:
A one-shot object with a single goal. It ends in Succeeded or Failed. New goal = new Loop, optionally forked from a checkpoint. "Long-lived" means many iterations within one Loop, not re-tasking across goals.
_Avoid_: task, job, run

**TamperedVerify**:
A failure reason set when protected acceptance-check files differ from their hash recorded at the start of the iteration. The iteration fails before any check runs.
_Avoid_: tamper, sabotage, game

**Audit trail**:
The append-only record of every operator decision: phase transitions, budget checks, stall hashes, verify results, approvals. Stored in Kubernetes events plus a `status.history[]` on the Loop. Never in logs alone.
_Avoid_: logs, trace, telemetry
