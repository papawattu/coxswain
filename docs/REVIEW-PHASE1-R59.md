# Phase 1 review, round 59: the Planning phase can change the code, and Implementing commits it

**Scope:** one finding, from the R2c upgrade run on kind-ref (11-17) at `52a52ed` (v0.3.0), checked against main `579d46c`. The R2c Loops ran the lab's runner image `local/coxswain/runner:34daeee` (`r2-lab.log:2,37,64`), which is not in v0.3.0's history; `34daeee` differs from `52a52ed` in 10 files under `runner/`. The behaviour cited below is the same at `34daeee` (`knownTools`, `commitWorkspace`'s `:(exclude).coxswain` status and its "No changes" branch), but all line numbers are at `579d46c`. The owner asked for it to be raised as a review item. pi3 did the first read-only trace; I re-checked every citation below at `579d46c`. This round does not audit the window since R58. R57's I271 and I272 and R58's I273 are not re-checked here and stay open.

## P2

- [ ] **I274: Planning has the same write-capable shell as Implementing, nothing stops it editing the workspace, and Implementing's commit picks the edit up**

  **Where:**
  - `runner/runner.go:651`: `var knownTools = map[string]bool{toolNameShell: true}`. There is one tool map for every phase.
  - `runner/phase.go:386`: Planning runs `drivePhaseOnce(cfg, planningPrompt(…), …)`, the same model loop as Implementing. Each tool call runs `execShell` (`runner/runner.go:750`), which is `exec.CommandContext(ctx, "sh", "-c", args.Command)` (`runner/runner.go:568`) in the workspace.
  - `runner/phase.go:1151-1154`: the only restraint is the prompt: "The plan must be a summary of the steps you will take, not the implementation itself."
  - `internal/controller/loop_controller.go:2289`: the agent container mounts the workspace `{Name: workspaceVolumeName, MountPath: agentWorkspaceMount}`, writable, in every phase. The rootfs is read-only (`:2284`), but the workspace is not.
  - `internal/controller/loop_controller.go:3029-3033`: the workspace is one per-Loop PVC "that survives the per-phase pod recycle", so a Planning edit is still there in Implementing.
  - `runner/phase.go:911-990` (`commitWorkspace`, called from the Implementing branch at `:468`): it stages every path that `git status --porcelain=v1 -z --untracked-files=all -- :(exclude).coxswain` lists. Nothing tells a Planning edit from an Implementing one. If the agent committed during the phase, the "No changes" branch uses that HEAD as the verification target, so a commit made in Planning carries over too.
  - `internal/controller/loop_s4_phase.go:474` (`recordPhaseClaim`): the operator checks the claim's phase and records the head commit. It does not check that Planning left the workspace unchanged. The B3 plan-hash check (`runner/phase.go:388-404`) hashes PLAN.md only.

  **Problem:** the phase machine says Planning plans and Implementing changes code. The code does not enforce that split. Two consequences:
  - **Manual approval (`spec.approval.mode: Manual`) approves a plan, but the code may already have been changed.** `api/v1alpha1/loop_types.go:37-42` says Manual "adds an AwaitingApproval hold after Planning"; `docs/QUICKSTART.md:415-417` says "the user reads the plan and either approves or rejects it". The user sees PLAN.md (`cox plan`, `cox diff`), not the workspace. A plan the user rejects can leave edits behind: Replan re-enters Planning on the same PVC, so they are still there when Implementing commits.
  - **The Implementing reply can misreport its own commit.** The model sees a tree that is already changed and says it changed nothing, while the runner commits that change.

  Shown in R2c (`.samples/r2c-v030/run1/`, reviewer-local, not committed):
  - r2-smoke, Planning, 13:55:00 (`r2-smoke-runner.log:4`): `observedPhase=Planning summary="The tests pass … My plan was followed and the fix is verified.` Its steps say: "Inspected `round.go` (still had the seed `math.Trunc` bug)", "Edited `round.go` only: Replaced the body with `return math.Round(v * scale) / scale`", "Ran `go test ./...` → `ok example.com/gocli`".
  - r2-smoke, Implementing, 13:56:03 (`r2-smoke-runner.log:12-13`): `headCommit=e82b597… staged=1`, then `observedPhase=Implementing summary="Already fixed and green. No changes needed.` with "**Files changed:** None".
  - r2-across, Planning, 13:52:40 (`r2-across-runner.log:3`): "The fix is already applied and tests pass. Since this is the planning phase, here is the plan", ending "**Result (verified):** `round.go` now uses `math.Round(v*scale)/scale`". This reply is primed by shared memory ("**Context / constraints (from shared memory):** … The accepted fix is `math.Round(v*scale)/scale`") and lists the edit as a future step ("2. In `round.go`, change the return…"). It does not say that Planning made the edit.
  - r2-across, Implementing, 13:53:45 (`r2-across-runner.log:10-11`): `headCommit=59070f7… staged=1`, then "The fix was already applied from a prior loop". That phrase may come from the memory-primed plan ("the fix may already be present from a prior loop"), not from what the model saw. Either way, the workspace is per-Loop (`loop_controller.go:3029-3033`), so another Loop's workspace cannot be where the change came from.

  What R2c shows and does not show:
  - **Shown:** `round.go` differed from HEAD when Implementing committed (`staged=1`), and Implementing said it changed nothing.
  - **Shown only as the model's word, in one Loop:** that Planning made the edit. Only the r2-smoke Planning reply says so ("**Steps (executed):** … Edited `round.go` only"). r2-across's is ambiguous (see above).
  - **Not shown:** the shell commands either phase ran. The claim that reaches the operator (the termination message, `loop_s4_phase.go:107`) has no tool trace. `runner/runner.go:156` defines `ToolTrace` in `result.json`, and that file was not captured.

  **Fix:** D143.

  **Acceptance:**
  - An envtest or runner spec, per D143, that fails with the guard disabled. The mutation diff and the focus command are in the PR.
  - A spec where the Planning model's shell edits a tracked file **and** one where it runs `git commit`. Both must end with no source change reaching Implementing's commit, or, under D143(b), with the Loop failing visibly.
  - A spec where Manual approval rejects a plan after Planning edited a file, and Replan's next Implementing commit does not contain that edit.
  - A kind run, since the change touches a volume that every phase shares (R55 I263(c)): a Loop whose Planning goal tempts an edit (the r2-smoke goal did). It must show that Implementing's commit contains only Implementing's own edits, and that PLAN.md, the plan-hash and `.coxswain/gotmp` still work.

## Owner decision

- **D143: how the operator stops the Planning phase changing the code.**
  - **(a) Recommended: the Planning pod mounts the workspace read-only, except for `.coxswain`.**
    - The operator already recreates the sandbox pod for each phase (`loop_s4_phase.go:40-46`), so it can set `ReadOnly: true` on the workspace mount when the desired phase is Planning, and add a second, writable mount of the same PVC with `subPath: .coxswain` at `/workspace/.coxswain`.
    - The kubelet enforces it, not the process the model drives. A `git commit` in Planning fails, because `.git` is read-only.
    - What Planning still needs to write is all writable. The runner's own writes are all under `.coxswain`: PLAN.md (`runner/phase.go:655-660`), the claim (`:759`), conversation.json (`:1542-1545`) and result.json (`runner/runner.go:766-774`). `GOTMPDIR` is `/workspace/.coxswain/gotmp` (`loop_controller.go:2176`). `HOME` and `GOCACHE` are `/scratch` (`:2160`). `/tmp` is its own volume. The plan-hash and plan feedback are written by the phase-init container (`phaseInitScript`, `loop_controller.go:6783`), which keeps its writable mount; Planning only reads the plan-hash (`runner/phase.go:388-404`). That list is part of (a)'s acceptance.
    - `git status` and `git diff` still work on a read-only tree; git's index refresh just fails without an error.
    - The cost: a Planning command that writes into the repo now fails. That is the intent, but it can surprise a model that "verifies" by building into the tree; the failure shows in the tool output, and the plan still gets written. `.coxswain` must exist before the agent container starts; the phase-init container already recreates `.coxswain/gotmp` at every phase start (`phaseInitScript`, `loop_controller.go:6783`, `rm -rf`/`mkdir -p "${DEST}/.coxswain/gotmp"`).
    - **What (a) does not cover:** `.coxswain` stays writable by the Planning model, as it is today, so the model can still change `.coxswain` files the operator reads (`result.json`, the plan-hash). That is outside I274, but (a) does not make Planning unable to touch anything the operator trusts.
  - **(b) The runner checks after Planning and fails the phase.** It records HEAD and `git status` before the model loop. If either changed afterwards, it writes a failed claim naming the paths, and the operator surfaces it as an Event. It is cheaper than (a), but the check runs in the container the model just drove. The model can hide a change from the check (`.git/info/exclude`, `git update-index --assume-unchanged`), and a check that the model can defeat is advisory.
  - **(c) Accept and document it.** OPERATOR.md and QUICKSTART §6 say that Planning may run any command, that Manual approval approves the plan text and not the tree, and that a rejected plan's edits stay in the workspace.
  - **Not proposed: the phase-init resets the tree to HEAD before Implementing.** It would run git over the agent-controlled `.git` (planted hooks, `core.fsmonitor`), the ADR-0006 hazard that `loop_controller.go:2301-2312` keeps git away from.
  - **Whichever option is chosen:** capture `result.json`'s ToolTrace in the R2 evidence (the runner-log helper or the claim), so that a later run can show which phase ran which command. The R2c evidence cannot.

## What I did not verify

- **Which phase ran the edit.** R2c has no tool trace (see I274). Implementing could also have edited `round.go` and then said it did not; the summaries are the model's word.
- **How often it happens.** n=2 Loops, both on the same seeded `round.go` goal with qwen3.8-27b, and only r2-smoke's Planning reply says the fix was done in Planning. Other goals and models are not checked.
- **Whether (a)'s subPath mount works on every storage class we support.** Read-only plus a subPath mount of the same RWO PVC is standard, but not run here; the kind run in the acceptance covers kind's local-path only.
- **ApprovalModeManual in R2c.** Both R2c Loops ran in the default Auto mode. The Manual consequence above is from the code, not from a run.
