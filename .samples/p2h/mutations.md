# P2h real-bug mutations (I55 norm — scratch worktree)

P2h is a kind-acceptance slice, not an envtest-spec slice, so its gates are
the acceptance Loops + the I55 execution test (`test/e2e/p2h-execstub-test.sh`).
The mutations below validate the REAL BUGS the acceptance run surfaced —
each mutation re-introduces the pre-fix behaviour and shows the guard that
catches it FAILs. These are bug-mutations (the plan's named mutations are
the acceptance Loops themselves, run in-kind), recorded here per the I55
norm: exact diff, scratch worktree, result recorded.

Base: working tree of `slice/p2h-kind-acceptance` (post the writeClaim fix;
the runner unit tests live in `runner/`).
Worktree: `/tmp/p2h-mut-claim` (created with `git worktree add --detach`,
with the working-tree `runner/phase.go`, `runner/phase_test.go`,
`runner/phase_commit_test.go` copied in so the mutation applies to the
FIXED code).

---

## M1: Drop the `Iteration` field from `writeClaim`'s claim struct

**The mutation (re-introduces the P2h root-cause bug):** drop the
`Iteration` field from the claim struct + its assignment in `writeClaim`
(`runner/phase.go`). The operator's stale-iteration guard reads
`claim.Iteration`; a missing `"iteration"` key parses as 0, so the claim
reads as STALE from iteration 0 and is ignored whenever
`status.iteration > 0` — every Loop would stall at its first iterate.

### Diff

```diff
--- a/runner/phase.go
+++ b/runner/phase.go
@@ writeClaim
 	type claim struct {
 		ObservedPhase string `json:"observedPhase"`
 		Status        string `json:"status"`
 		BlockedReason string `json:"blockedReason,omitempty"`
 		HeadCommit    string `json:"headCommit,omitempty"`
-		// S4 (R19 OS1): the .coxswain/iteration the runner read. ...
-		Iteration int `json:"iteration"`
 	}
 	c := claim{
 		ObservedPhase: res.ObservedPhase,
 		Status:        res.Status,
 		BlockedReason: res.VerificationNotes,
 		HeadCommit:    res.HeadCommit,
-		Iteration:     res.Iteration,
 	}
```

### Build

`go build ./...` — OK (no compile errors).

### Test run

```
$ go test -run 'TestWriteClaimCarriesIteration|TestPhaseRunImplementingClaimCarriesCurrentIteration|TestPhaseRunPlanningWritesPlanAndClaim' -v .
```

### Failed tests (exact output)

1. **`TestPhaseRunImplementingClaimCarriesCurrentIteration`**
   - `phase_commit_test.go:404: claim iteration = 0, want 2 (the current .coxswain/iteration — a missing key parses as 0, which the operator's stale-iteration guard discards when status.iteration is 2; the P2h kind-run stall): {"observedPhase":"Implementing","status":"success","headCommit":"fc55d610c8ee07e421c4b4dd8af244c7bddd5e36"}`

2. **`TestWriteClaimCarriesIteration`**
   - `phase_commit_test.go:439: the claim MUST carry the iteration key (a missing key parses as 0 at the operator, which the stale-iteration guard discards when status.iteration is 2): {"observedPhase":"Implementing","status":"success","headCommit":"abc"}`

3. **`TestPhaseRunPlanningWritesPlanAndClaim`**
   - `phase_test.go:127: the claim must carry iteration=0 (no .coxswain/iteration marker): map[observedPhase:Planning status:success]`

All three FAIL — exactly the guard the fix adds. The first two are the
P2h acceptance's direct unit guards (the iteration the runner read must
ride the claim); the third asserts the field is present even when 0 (the
operator accepts 0 only when `status.iteration` is also 0).

---

## M0: Restore the `msg != ""` guard in `defaultReadCheckOutput`

**The mutation (re-introduces the P2h finding):** an empty
`terminationMessage` is treated as INERT again — `("", false)` instead of
`("", true)`. A silently-failing check (`test -f /nonexistent`) terminated
with an empty message, and the stall detector's
consecutive-identical-failure run never advanced, so the impossible goal
never stopped (the P2h kind-run stall).

### Diff

```diff
--- a/internal/controller/loop_p2e_stall.go
+++ b/internal/controller/loop_p2e_stall.go
@@ defaultReadCheckOutput
 		// The CURRENT terminated state first: ... An EMPTY message still counts: a
 		// silently-failing check (test -f /nonexistent) terminated, and the
 		// empty output is the stall detector's evidence.
-		if ics.State.Terminated != nil {
+		if ics.State.Terminated != nil && ics.State.Terminated.Message != "" {
 			return ics.State.Terminated.Message, true
 		}
-		// Fall back to the LAST terminated state: a restarted container
-		// (restartCount > 0) carries its previous incarnation's message there.
-		if ics.LastTerminationState.Terminated != nil {
+		if ics.LastTerminationState.Terminated != nil && ics.LastTerminationState.Terminated.Message != "" {
 			return ics.LastTerminationState.Terminated.Message, true
 		}
```

### Build

`go build ./...` — OK (no compile errors).

### Test run

```
$ go test -run 'TestDefaultReadCheckOutput' -count=1 -v ./internal/controller/
```

### Failed tests (exact output)

1. **`TestDefaultReadCheckOutput`** — case (d): `loop_p2e_stall_test.go:443: (d) a terminated check with an empty message must be ("", true): got ("", false)` — the pre-p2h guard reads a terminated empty-message check as INERT, the exact regression the fix removed (cases (a), (b), (c) still pass: they do not exercise the empty-message path).

---

## Summary table

| # | Component | Mutation | Guard that FAILs |
|---|-----------|----------|------------------|
| M0 | `internal/controller/loop_p2e_stall.go` `defaultReadCheckOutput` | Restore the `msg != ""` guard (empty message = INERT) | `TestDefaultReadCheckOutput` case (d) + (d2) |
| M1 | `runner/phase.go` `writeClaim` | Drop the `Iteration` claim field | `TestWriteClaimCarriesIteration`, `TestPhaseRunImplementingClaimCarriesCurrentIteration`, `TestPhaseRunPlanningWritesPlanAndClaim` |

---

## Gate mutations (the P2h plan's three named gate mutations, I49 norm)

Each mutation: `git worktree add --detach /tmp/p2h-mut-gN c57281b`, the
exact diff applied to the FIXED code, `go build ./...` OK, a UNIQUE
scratch image built (`coxswain-mut-g<N>-<name>:20261006170128`), and
`P2H_OPERATOR_IMAGE=<scratch> bash test/e2e/p2-e2e.sh` run against
`kind-coxswain-dev` (the script still loads + rolls + digest-verifies the
given image; the running pod's imageID must equal the built digest or the
run FAILs). Each run has its own tee'd log in `.samples/p2h/run-<ts>.log`.

## G1: the stall gate disabled (drop the fire)

**The mutation (re-introduces the pre-P2e behaviour):** `applyStallGate`'s
body is replaced by `return false` (the gate never takes a decision). The
stall Loop spins to its `maxIterations` cap (10) instead of stopping at
iteration 3 with `Failed:Stalled`; no Stalled condition, no StallDetected
Event.

### Diff

```diff
--- a/internal/controller/loop_p2e_stall.go
+++ b/internal/controller/loop_p2e_stall.go
@@ -35,7 +35,7 @@
 	"context"
-	"fmt"
+	_ "fmt"
 	"strconv"
@@ -279,108 +279,13 @@
-func (r *LoopReconciler) applyStallGate(ctx context.Context, loop *coxv1alpha1.Loop, pod *corev1.Pod, failedCheck string) bool {
-	// ... (the whole gate body: the readCheckOutput seam, the normalise,
-	// the StallEntry append, the stallDecision, the Fail/Pause/Continue
-	// actions, the events) ...
-}
+func (r *LoopReconciler) applyStallGate(_ context.Context, _ *coxv1alpha1.Loop, _ *corev1.Pod, _ string) bool {
+	// MUTATION G1 (the P2h gate mutation): the stall gate is DISABLED —
+	// the fire is dropped and the gate never takes a decision (the
+	// stall Loop spins to its maxIterations instead of stopping at 3
+	// with Stalled).
+	return false
+}
```

### Build

`go build ./...` — OK. Scratch image:
`coxswain-mut-g1-stall-disabled:20261006170128`
(`sha256:0e98d00990fda9ea9feabc5487acbf89eff0d0b20e6a9b358fbb706f1df8cbd9`).

### Run (the full acceptance script against the scratch image)

Log: `.samples/p2h/run-20261006172434.log` (the operator digest line
confirms the RUNNING pod's imageID equals the built scratch digest).

### Failed assertions (exact output)

```
   operator digest (verified against the running pod): sha256:0e98d00990fda9ea9feabc5487acbf89eff0d0b20e6a9b358fbb706f1df8cbd9

--- assertion 1: the Stalled path (stall Loop + the control contrast) ---
   [FAIL] stall Loop: phase=Failed failed-reason=MaxIterationsExceeded stalled-cond= (expected Failed/Stalled/True)
   [FAIL] stall Loop: status.iteration=10 (expected 3)
   [FAIL] stall Loop: no Stall event (events in .samples/p2h/events-stall.json)
   [PASS] control Loop: phase=Failed at status.iteration == 5 (the maxIterations cap) — the contrast
   [assert 1] state=fail
...
   [assert 2] state=pass   (the budget gate is independent — it still fires)
   [assert 3] state=pass   (the cross-check still reaches Prometheus)
   [assert 4] state=pass   (the suspend pause/resume is independent)
   [assert 5] state=pass   (the budget-Pause is independent)
   [FAIL] assertion accounting: assertion 1 did not reach pass (state: 1:fail 2:pass 3:pass 4:pass 5:pass)
RESULT: FAIL
```

Assertion 1 FAILS exactly as the plan predicts (the stall Loop spins to
its `maxIterations` cap — 10 — instead of stopping at 3 with
`Failed:Stalled`; no Stalled condition, no StallDetected Event). The other
four assertions still pass: the budget gate, the cross-check, the suspend
pause/resume and the budget-Pause are independent of the stall gate. The
control Loop (stallAfter:10, maxIterations:5) still hits its cap at
iteration 5 (the contrast is preserved — the control Loop's stallAfter is
never reached, so the disabled gate does not change its outcome).

## G2: the budget gate disabled (skip the phase transition)

**The mutation (re-introduces the pre-P2d behaviour):** `applyBudgetDecision`'s
body is replaced by `return false` (the cap hit is evaluated nowhere,
`onExceeded` never fires, the phase transition `Failed:BudgetExceeded` is
skipped). The budget Loop runs on to its `maxIterations` cap (10) instead of
stopping at the cap with `Failed:BudgetExceeded`; the budget-Pause Loops run
on to `Failed:MaxIterationsExceeded` instead of pausing.

### Diff

```diff
--- a/internal/controller/loop_p2d_budget.go
+++ b/internal/controller/loop_p2d_budget.go
@@ -441,33 +441,10 @@
-func (r *LoopReconciler) applyBudgetDecision(ctx context.Context, loop *coxv1alpha1.Loop) bool {
-	spec := loop.Spec.Budget
-	if spec == nil {
-		return false
-	}
-	var (
-		tokens  int64
-		active  int64
-		costUsd string
-	)
-	if b := loop.Status.Budget; b != nil {
-		tokens = b.PromptTokens + b.CompletionTokens
-		active = b.ActiveSeconds
-		costUsd = b.CostUsd
-	}
-	hit, reason, value := r.capName(spec, tokens, active, costUsd)
-	if hit == "" {
-		return false
-	}
-	if loop.Status.Budget != nil && loop.Status.Budget.Exceeded {
-		return false
-	}
-	return r.fireBudgetExceeded(ctx, loop, spec, hit, reason, value)
-}
+func (r *LoopReconciler) applyBudgetDecision(ctx context.Context, loop *coxv1alpha1.Loop) bool {
+	// MUTATION G2 (the P2h gate mutation): the budget gate is DISABLED —
+	// the cap hit is evaluated nowhere, onExceeded never fires and the
+	// phase transition (Failed:BudgetExceeded) is skipped.
+	return false
+}
```

### Build

`go build ./...` — OK. Scratch image:
`coxswain-mut-g2-budget-disabled:20261006170128`
(`sha256:45c9a00f61314ccedf1c75c9fa8467505ee97c8f950e010fec454f77b98178e5`).

### Run (the full acceptance script against the scratch image)

Log: `.samples/p2h/run-20261006184855.log`.

### Failed assertions (exact output)

```
   operator digest (verified against the running pod): sha256:45c9a00f61314ccedf1c75c9fa8467505ee97c8f950e010fec454f77b98178e5

--- assertion 2: the BudgetExceeded path (budget Loop) ---
   [FAIL] budget Loop: phase=Failed reason=MaxIterationsExceeded exceeded= exceededReason=
   [PASS] budget Loop: status.budget token total 4400 >= 300 (the >= cap assertion, item 14)
   [FAIL] budget Loop: no BudgetExceeded Event (events in .samples/p2h/events-budget.json)
   [assert 2] state=fail

--- assertion 4: the paused-Loop resume (the resume Loop) ---
   [FAIL] assertion 4: the resume Loop was never at Implementing in the flip window (last phase=Failed); the pause/resume cycle was not exercised
   [assert 4] state=fail

--- assertion 5: the budget-Pause + raised-cap resume (both sub-cases) ---
   [FAIL] budget-Pause Loop: pausedFrom= pausedReason= exceeded= (expected Planning/Implementing-or-Verifying/Budget/true)
   [FAIL] budget-Pause Loop (raised cap): phase=Failed exceeded= pausedReason= resumed-event=no
   [FAIL] budget-Pause control Loop (un-raised): phase=Failed exceeded= (expected Paused + exceeded=true)
   [assert 5] state=fail

   assertion states:  1:pass 2:fail 3:pass 4:fail 5:fail
RESULT: FAIL
```

Assertion 2 FAILS exactly as the plan predicts (the budget Loop runs on to
its `maxIterations` cap — 10 — instead of stopping at the cap with
`Failed:BudgetExceeded`; no BudgetExceeded Event). The token total still
accumulates (4400 >= 300) because the meter is independent of the gate —
the gate is the DECISION, not the COUNT. Assertion 5 FAILS (the budget-Pause
Loops run on to `Failed:MaxIterationsExceeded` instead of pausing).

**Why assertion 4 ALSO fails under G2 (from the log):** the resume Loop
has NO `spec.budget` (its spec sets only `maxIterations: 100`,
`stallAfter: 50`), so the disabled budget gate does not stop it — but its
pause/resume cycle still cannot be exercised. With the budget gate ON, the
Loop's 3rd Implementing iteration hits the wall-clock budget cap (the
resume Loop runs the full 600s flip window; on the green run it is at
iteration 18 with 9200 tokens when the flip lands) and pauses (Budget) —
but a pause does not stop iteration: `handlePausedLoop` re-evaluates the
caps on every reconcile, and when the cap clears (the meter accumulates
only while the sandbox runs) the Loop resumes and keeps iterating, so the
phase keeps cycling through Implementing and the flip lands. With the
budget gate OFF, the wall clock never fires, the Loop never pauses, and it
runs the full 600s flip window at ~200 tokens/iteration with no budget
pause to slow it down: by the time the flip window expires the Loop is at
its `maxIterations` cap (evidence dump: `p2h-resume-20261006184855
Failed 100` — iteration 100, `cond: Failed True
MaxIterationsExceeded`), a terminal phase, so the flip loop's `if ph =
Succeeded/Failed: break` exits and assertion 4 FAILs with "was never at
Implementing in the flip window (last phase=Failed); the pause/resume
cycle was not exercised". The fail is a CASCADE of the G2 mutation
(indirectly: no budget pause -> no cycling through Implementing while the
wall clock runs -> the Loop out-runs the flip window), not a direct
target of the mutation — the plan names assertion 2 as G2's target, and
this is recorded here as the log explains it.

Assertion 1 (stall) still passes: the stall gate is independent of the
budget gate.

## G3: the pausedReason check dropped (suspend=false resumes every pause)

**The mutation (re-introduces the pre-P2f behaviour):** `resumeTriggered`
DROPS the `PausedReason == Suspend` check — `spec.suspend=false` becomes a
resume trigger for EVERY pause reason, so a BUDGET (or STALL) pause is
un-paused by `suspend=false`, which it must not be (only the
`coxswain.io/resume` annotation may resume a Budget pause).

### Diff

```diff
--- a/internal/controller/loop_p2f_paused.go
+++ b/internal/controller/loop_p2f_paused.go
@@ -98,10 +98,11 @@
 func (r *LoopReconciler) resumeTriggered(loop *coxv1alpha1.Loop) bool {
-	if loop.Status.PausedReason == coxv1alpha1.PausedReasonSuspend {
-		return !loop.Spec.Suspend
-	}
-	return loop.Annotations[resumeAnnotation] == "true"
+	// MUTATION G3 (the P2h gate mutation): the PausedReason == Suspend
+	// check is DROPPED. spec.suspend=false is now a resume trigger for
+	// EVERY pause reason, so a BUDGET pause is un-paused by suspend=false
+	// (which it must not — only the annotation may resume a Budget pause).
+	return !loop.Spec.Suspend || loop.Annotations[resumeAnnotation] == "true"
 }
```

### Build

`go build ./...` — OK. Scratch image:
`coxswain-mut-g3-pausedreason:20261006170128`
(`sha256:3dc6007b711697a0d3298b4efd28bb8edff7fa7d54e51dbc3a22fa20283ae957`).

### Run (the full acceptance script against the scratch image)

Log: `.samples/p2h/run-20261006192100.log`. The running pod's imageID
verifies as the scratch digest:

```
   operator digest (verified against the running pod): sha256:3dc6007b711697a0d3298b4efd28bb8edff7fa7d54e51dbc3a22fa20283ae957
   assertion states:  1:pass 2:pass 3:pass 4:pass 5:pass
RESULT: PASS
```

**The expected failure (assertion 5's budget-Pause Loop wrongly resuming on
`suspend=false`) did NOT manifest — `5:pass`.** The wrong trigger existed:
the budget-Pause Loop (`p2h-budgetpause-<ts>`) sits at `Paused`
(`pausedReason=Budget`) with `spec.suspend=false`, so on every reconcile
`handlePausedLoop` (called from `applyPauseMechanics` on every reconcile
of a paused Loop) sees `resumeTriggered` == true under the mutation and
re-evaluates + resumes. The P3 refuse-while-exceeded gate still fires
first for the un-raised control Loop (the re-evaluation leaves
`exceeded=true`), and the raised-cap Loop resumes via the annotation
anyway — so the observable end states are identical to the unmutated
run, and every assertion in the section still passes.

Why: the `pausedReason` check's ONLY observable effect is the resume path
of a budget-paused Loop whose cap has NOT been raised and which was NOT
annotated with `coxswain.io/resume` (i.e. a `spec.suspend` flip, which
nothing in the acceptance script drives on a budget-Pause Loop — the
script drives `suspend` only on the resume Loop, whose pause is a Suspend
pause and for which `suspend=false` is the LEGITIMATE trigger, so the
mutation is a no-op there too). The script's assertion 5 drives exactly
two budget-pause resumes — the raised-cap one (via the annotation, valid
under either behaviour) and the un-raised one (refused by the P3 gate,
valid under either behaviour) — so the mutation's effect (a Suspend
trigger accepted for a Budget pause with no annotation) is never
exercised, and the run cannot catch it.

**Verdict: the G3 mutation as written is INADEQUATE as a gate test — the
script's assertions do not distinguish the mutated behaviour.** To catch
it, assertion 5 needs a THIRD sub-case: a third budget-Pause Loop whose
cap is NOT raised and which is resumed by `spec.suspend=true -> false`
(never annotated), asserting it STAYS Paused (the correct behaviour — a
Suspend flip must not resume a Budget pause) and FAILs under the mutation
(the flipped `suspend=false` would wrongly resume it). That sub-case
requires a script change (a third budget-Pause Loop + a suspend-flip
sub-assertion) — out of scope for this record; the G3 record stands as: run executed, digest verified, all five
assertions pass, expected 5:fail NOT achieved because the script's
assertion 5 does not exercise the mutated path.


### Sub-case (c) first run: run-20261006195618 (inadequate — the refuse-while-exceeded guard masks G3)

The first rewritten sub-case (c) drove the suspend flip on the UN-RAISED
budget-Pause control Loop (`p2h-budgetpause2-<ts>`): flip `spec.suspend`
true -> false (no annotation) and assert it STAYS Paused >= 30s. Normal
run (the real operator, branch commit `df88848`, digest
`sha256:770d9f4cbb730052387280f8a8c62ef1d2834fe6a2d2a0b1909f2fa56fbe0c45`):
`1:pass 2:pass 3:pass 4:pass 5:pass` — 5:pass as required. G3 re-run (the
scratch image, digest `sha256:3dc6007b711697a0d3298b4efd28bb8edff7fa7d54e51dbc3a22fa20283ae957`):
`1:pass 2:pass 3:pass 4:fail 5:pass` (log: `run-20261006200606.log`) —
**5 still PASS, the expected 5:fail did not manifest.**

**The reviewer's analysis (confirmed): the mutated `resumeTriggered` DOES
fire for the un-raised budget pause — but the refuse-while-exceeded guard
(`budgetStillExceeded`) re-pauses it, so "stays Paused" holds under the
mutation too.** On the un-raised control Loop the cap is still exceeded at
the moment of the suspend flip (`status.budget.exceeded=true`, the meter
stopped accumulating at the pause), so the G3 path (the suspend flip now
triggers a resume re-evaluation) runs the re-evaluation, the guard sees
the cap is still hit, and the P3 refuse-while-exceeded re-pause fires
before the phase can leave Paused. The `pausedReason=Budget` +
`exceeded=true` end state is identical under the mutation and the real
operator, and the sub-case (which asserts that state) passes either way.
The `4:fail` in that run was a SEPARATE timing race (the resume Loop
leaving Implementing between the phase check and the pause landing —
`pausedFrom=Verifying` instead of Implementing, iteration 12 vs pre-pause
11): recorded below and fixed.

The ONLY G3-distinguishable state is a budget pause whose cap HAS been
raised but which is NOT annotated: correctly it stays Paused until
`coxswain.io/resume` (a suspend flip may not resume a Budget pause — the
re-evaluation clears `exceeded` and nothing re-pauses it, so the only
thing holding it in Paused is the operator's rule that a Budget pause
resumes only via the annotation); under G3, the `spec.suspend=false`
resume trigger fires, the cap is no longer exceeded, and the guard does
NOT refuse — the Loop resumes by itself.

### Sub-case (c) rewritten (the G3 gate the reviewer demanded): the raised-cap / no-annotation budgetpause3 Loop

The acceptance script (branch commit `6ad8843`) now has a THIRD
budget-Pause Loop, `p2h-budgetpause3-<ts>` (maxTokens 400, onExceeded
Pause, maxIterations 10, stallAfter 3/Continue — the budget pause fires at
400 tokens, request 2). Assertion 5's sub-case (c) on it:

1. Raise the cap (`spec.budget.maxTokens: 8000`) — NO annotation. The cap
   is no longer exceeded; the only thing that must keep the Loop Paused is
   the operator's annotation-only resume rule.
2. Flip `spec.suspend` true -> false (NO annotation) and assert the Loop
   STAYS Paused (`pausedReason=Budget`) for **>= 45s**. Under the G3
   mutation the suspend flip resumes it (the cap is raised, the guard
   does not refuse) -> the sub-case FAILs.
3. THEN annotate (`coxswain.io/resume=<ts>`, `--overwrite`) and assert the
   Loop resumes to `Implementing` within 240s (the annotation is the
   Budget pause's legal resume trigger; the cap was raised in step 1, so
   the refuse-while-exceeded guard does not refuse the annotation resume).

The assertion-5 accounting no longer pins `budgetpause2`'s
`exceeded=true` (sub-case (c) moved off that Loop); it pins B1_OK (the
entry point), B2_OK (the un-raised re-pause, the control), and B3_OK
(sub-case (c), now on budgetpause3).

The A4 race (the `pausedFrom=Verifying` / iteration-advanced failure in
run-20261006200606) is fixed in the same commit: the reference stub now
serves a `p2h-stub-slow` model (a 30s `time.sleep` before answering), and
ONLY the resume Loop uses it — its Implementing phase lasts >= 30s, so the
script's suspend-flip window (polled every 2s, `wait_phase Paused 180`)
lands inside an Implementing deterministically instead of racing the
phase transition to Verifying. The stub's audit log and the per-Loop
budget math are unchanged (usage is still 100/100 per request; the sleep
adds wall clock, not tokens).

### Build

The scratch image is UNCHANGED from the first G3 run (the operator-only
mutation; the script changes are in the acceptance script, not the
operator): `coxswain-mut-g3-pausedreason:20261006170128`
(`sha256:3dc6007b711697a0d3298b4efd28bb8edff7fa7d54e51dbc3a22fa20283ae957`).

### Run (the rewritten sub-case; PENDING)

Two runs are required before this G3 record is complete:

1. **The normal run** (the real operator, branch commit `6ad8843`, digest
   `sha256:770d9f4cbb730052387280f8a8c62ef1d2834fe6a2d2a0b1909f2fa56fbe0c45`
   — the operator image is unchanged, only the script changed): all five
   assertions must pass (1:pass 2:pass 3:pass 4:pass 5:pass).
2. **The G3 re-run** (the scratch image, the same digest as above):
   assertion 5 must now FAIL — sub-case (c) step 2 (the suspend flip on
   the raised-cap, no-annotation budgetpause3 Loop) must resume the Loop
   under the mutation, ending the hold early and failing the "stays
   Paused >= 45s" assertion; the subsequent annotation-resume check then
   sees the Loop already running (a no-op PASS, but B3_OK is already 0 so
   the section FAILs).

Both runs are in progress; the log filenames + the exact assertion states
will be recorded here when they complete.
