# Coxswain: build plan

**Goal:** a Kubernetes operator that runs long-lived plan → implement → verify loops. It runs each loop in an isolated workspace, stops on success, budget or stall, and hands back a PR.

**Stack:** Go, kubebuilder v4, controller-runtime. Workspaces run on [kubernetes-sigs/agent-sandbox](https://github.com/kubernetes-sigs/agent-sandbox). Agents are pluggable: start with a direct Claude API call from inside the sandbox, and add kagent Agents later.

Timelines assume part-time solo work.

## Phase 0: Foundations (week 1)

- Scaffold the repo with kubebuilder: `cox.dev/v1alpha1`, `kind: Loop`
- Set up a kind cluster with agent-sandbox installed and a basic `SandboxTemplate`, such as a Python or Go dev image
- Build one "agent runner" image that takes a prompt and a workspace path, calls the model with shell, fs and git tools, and writes a result file
- Add CI: lint, unit tests, envtest

**Done when:** `kubectl apply` of an empty Loop creates a Sandbox and the controller logs it.

## Phase 1: The loop, minimal (weeks 2–3)

- Write the Loop CRD types: goal, workspace, phases (plan, implement, verify.checks), `loop.maxIterations`
- Build the reconcile state machine: `Pending → Planning → Implementing → Verifying → (Succeeded | back to Implementing | Failed)`
- Execute each phase as an exec or short job inside the loop's sandbox
- Make verify deterministic only: run each command and treat exit code 0 as pass
- Feed failures forward: write verify output to the workspace and pass it into the next implement prompt
- Record `status.phase`, `status.iteration` and a basic `history[]`

**Done when:** a Loop on a toy repo with a failing test iterates until the test passes, or stops at `maxIterations`.

## Phase 2: Stopping properly (week 4)

- Add stall detection: hash the normalised failing-check output, and after N identical results set `Failed` with reason `Stalled`
- Add budgets, counted by a token-metering proxy sidecar rather than trusting the agent: tokens, wall clock and cost, with `onExceeded: Pause | Fail`
- Add a `Paused` phase with resume via annotation or a `spec.suspend` field
- Add conditions and events for every transition

**Done when:** a deliberately impossible goal stops cleanly with the right reason instead of spinning.

## Phase 3: Durability (weeks 5–6)

- Take a VolumeSnapshot after each verify and store the reference in `status.checkpoint`
- Resume after a controller or pod restart from the last checkpoint
- Add rollback (`spec.restoreFrom: iteration-2`) and fork (a new Loop seeded from another Loop's checkpoint)
- Add finalizers to clean up sandboxes and snapshots

**Done when:** killing the sandbox mid-implement resumes correctly, and a fork produces an independent loop.

## Phase 4: Humans in the loop (week 7)

- Add the plan approval gate: `approval: Manual` sets `AwaitingApproval` until a `PlanApproved` condition is set
- Build a `kubectl-cox` plugin with `approve`, `reject --reason`, `pause`, `resume`, `logs`, `history` and `diff`
- Add optional re-plan on reject, feeding the rejection reason into the planner

**Done when:** you can review a plan, reject it with feedback, and see the re-plan.

## Phase 5: Output and judgement (week 8)

- Open a PR on success: push a branch and create a draft PR with the loop history in the description
- Add an optional LLM judge after deterministic checks. It can never be the only gate.
- Add `onVerifyFail: Plan` to re-plan from scratch instead of re-implementing

**Done when:** a successful loop produces a reviewable PR with its iteration history attached.

## Phase 6: Hardening and release (weeks 9–10)

- RBAC least privilege, a default NetworkPolicy per sandbox, and secrets for model keys and git tokens
- Metrics: iterations per loop, pass rate, tokens per success, stall rate
- A Helm chart, docs, three example Loops, and a demo video
- Tag v0.1.0

## Decisions to make early

1. **One sandbox per loop or one per phase?** Start with one per loop. It's simpler and state carries over naturally.
2. **Agent interface:** define a small contract (prompt in, files and result out) so kagent, Claude Agent SDK, or other runners can plug in later.
3. **Where verify runs:** in the same sandbox (simplest) or a separate clean one (stops the agent tampering with tests). Consider protecting test files from edits during implement.

## Risks

- **The agent games verify**, for example by deleting or weakening tests. Mitigate by diffing protected paths and failing if they change.
- **Cost blowouts.** Mitigate with budgets enforced from day one of Phase 2, not later.
- **Overlap with kagent or agent-sandbox.** Stay the orchestration layer on top of them and contribute upstream where it makes sense.

## Before Phase 0

- [ ] Name check: GitHub, the `cox.dev` domain, and the CNCF landscape for "Coxswain"
