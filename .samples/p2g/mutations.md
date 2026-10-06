# P2g gate mutations (I49 norm — scratch worktree)

Base SHA: `c0cb7bb` (HEAD of `slice/p2g-conditions-events` at mutation time)
Worktree: `/tmp/p2g-mut` (created with `git worktree add /tmp/p2g-mut c0cb7bb`)
Envtest assets: `KUBEBUILDER_ASSETS="$(/home/jamie/projects/coxswain/bin/setup-envtest use 1.34.0 --bin-dir /home/jamie/projects/coxswain/bin -p path)"`

---

## M1: Drop the `Resumed` Event from the valid-resume site

**The plan's mutation:** drop the `Resumed` Event from its transition site →
specs 4 and 10 FAIL.

### Diff

```diff
--- a/internal/controller/loop_p2f_paused.go
+++ b/internal/controller/loop_p2f_paused.go
@@ -269,7 +269,7 @@ func (r *LoopReconciler) handlePausedLoop(_ context.Context, loop *coxv1alpha1.L
 	setCondition(loop, coxv1alpha1.PausedCondition, metav1.ConditionFalse,
 		resumedCondReason,
 		"resumed: phase "+string(resumeTo)+" (was paused: "+string(resumeFrom)+")")
-	r.emitResumeEvent(loop, resumeTo, resumeFrom)
+	// [MUTATION] r.emitResumeEvent(loop, resumeTo, resumeFrom) DROPPED
 	return false, true, true
 }
```

### Build

`go build ./...` — OK (no compile errors).

### Test run

```
Ran 10 of 347 Specs in 19.746 seconds
FAIL! -- 8 Passed | 2 Failed | 0 Pending | 337 Skipped
```

### Failed specs (exact names from the output)

1. **spec 4:** `P2g: conditions + events for every P2 transition (the auditability sweep) [It] spec 4: resume (suspend=false): the Paused condition is False/Resumed; the Resumed Event names the phase + reason`
   - Assertion: `a Resumed Event must be recorded: [Normal NetworkEnforced ... Normal Paused paused: source suspend, from phase Implementing]`
   - Expected `resumed` to be non-empty; got empty (the Event is absent).

2. **spec 10:** `P2g: conditions + events for every P2 transition (the auditability sweep) [It] spec 10: every transition in the inventory has a condition + an Event with a NON-EMPTY message (the structural sweep)`
   - Failing case: `Paused -> phase (resume)`
   - Assertion: `Paused -> phase (resume): an Event with reason Resumed must be recorded: [Normal NetworkEnforced ... Normal Paused paused: source suspend, from phase Implementing]`
   - Expected `ev` to be non-empty; got empty (the Event is absent).

Both failures are exactly the plan's prediction: the `Resumed` Event is
missing from the recorded events, so both the dedicated spec (4) and the
structural sweep (10) fail on the Event assertion. The Paused condition
itself (False/Resumed) is still set — the mutation only drops the Event,
not the condition.

---

## M2: Drop the `Paused` condition from the stall-Pause site

**The mutation:** drop the `Paused` condition (but keep the `Paused` Event)
from the stall-Pause transition site → specs 2 and 10 FAIL.

### Diff

```diff
--- a/internal/controller/loop_p2e_stall.go
+++ b/internal/controller/loop_p2e_stall.go
@@ -343,9 +343,7 @@ func (r *LoopReconciler) applyStallGate(ctx context.Context, loop *coxv1alpha1.L
 		loop.Status.Phase = coxv1alpha1.LoopPhasePaused
 		loop.Status.DesiredPhase = coxv1alpha1.LoopPhasePaused
 		loop.Status.PausedReason = coxv1alpha1.PausedReasonStall
-		setCondition(loop, coxv1alpha1.PausedCondition, metav1.ConditionTrue,
-			pausedCondReasonPaused,
-					"paused (stall): stall detector fired after "+fmt.Sprintf("%d", run)+" consecutive identical verify failures")
+		// [MUTATION] Paused condition DROPPED
 		setCondition(loop, string(coxv1alpha1.StalledCondition), metav1.ConditionTrue,
 			"Stalled", fmt.Sprintf("stall detector fired: %d consecutive identical verify failures (stallAction=Pause)", run))
```

### Build

`go build ./...` — OK.

### Test run

```
Ran 10 of 347 Specs in 19.007 seconds
FAIL! -- 8 Passed | 2 Failed | 0 Pending | 337 Skipped
```

### Failed specs (exact names from the output)

1. **spec 2:** `P2g: conditions + events for every P2 transition (the auditability sweep) [It] spec 2: -> Paused (stall source): the Paused condition names the source; the Paused Event names it; the Stalled condition is True`
   - Assertion: `the Paused condition must be set` — expected non-nil, got nil.

2. **spec 10:** `P2g: conditions + events for every P2 transition (the auditability sweep) [It] spec 10: every transition in the inventory has a condition + an Event with a NON-EMPTY message (the structural sweep)`
   - Failing case: `-> Paused (stall)`
   - Assertion: `-> Paused (stall): the Paused condition must be set` — expected non-nil, got nil.

---

## M3: Drop the `Paused` Event from the stall-Pause site

**The mutation:** drop the `Paused` Event (but keep the `Paused` condition)
from the stall-Pause transition site → specs 2 and 10 FAIL.

### Diff

```diff
--- a/internal/controller/loop_p2e_stall.go
+++ b/internal/controller/loop_p2e_stall.go
@@ -349,8 +349,7 @@ func (r *LoopReconciler) applyStallGate(ctx context.Context, loop *coxv1alpha1.L
 		setCondition(loop, string(coxv1alpha1.StalledCondition), metav1.ConditionTrue,
 			"Stalled", fmt.Sprintf("stall detector fired: %d consecutive identical verify failures (stallAction=Pause)", run))
 		if r.Recorder != nil {
-			r.Recorder.Eventf(loop, corev1.EventTypeNormal, pauseEventReason,
-				"paused: source stall, from phase %s (stall detector fired)", loop.Status.PausedFrom)
+			// [MUTATION] Paused Event DROPPED
 			r.Recorder.Eventf(loop, corev1.EventTypeWarning, "StallDetected", "stall detector fired: %d consecutive identical verify failures (stallAction=Pause)", run)
 		}
```

### Build

`go build ./...` — OK.

### Test run

```
Ran 10 of 347 Specs in 18.366 seconds
FAIL! -- 8 Passed | 2 Failed | 0 Pending | 337 Skipped
```

### Failed specs (exact names from the output)

1. **spec 2:** `P2g: conditions + events for every P2 transition (the auditability sweep) [It] spec 2: -> Paused (stall source): the Paused condition names the source; the Paused Event names it; the Stalled condition is True`
   - Assertion: `a Paused Event must be recorded: [Normal NetworkEnforced ... Warning StallDetected stall detector fired: 3 consecutive identical verify failures (stallAction=Pause)]`
   - Expected `paused` to be non-empty; got empty (the Event is absent).

2. **spec 10:** `P2g: conditions + events for every P2 transition (the auditability sweep) [It] spec 10: every transition in the inventory has a condition + an Event with a NON-EMPTY message (the structural sweep)`
   - Failing case: `-> Paused (stall)`
   - Assertion: `-> Paused (stall): an Event with reason Paused must be recorded: [Normal NetworkEnforced ... Warning StallDetected stall detector fired: 3 consecutive identical verify failures (stallAction=Pause)]`
   - Expected `ev` to be non-empty; got empty (the Event is absent).

---

## M4: Drop the `BudgetExceeded` Event (Fail path, the one naming the Failed phase)

**The mutation:** drop the second `BudgetExceeded` Event from the budget-Fail
transition site (the one that names the Failed phase: "budget cap %s hit at
%s; the Loop is Failed (onExceeded=Fail, from phase %s)") → specs 6 and 10
FAIL.

### Diff

```diff
--- a/internal/controller/loop_p2d_budget.go
+++ b/internal/controller/loop_p2d_budget.go
@@ -510,10 +510,8 @@ func (r *LoopReconciler) fireBudgetExceeded(ctx context.Context, loop *coxv1alpha
 		// Fail: terminal Failed, reason BudgetExceeded. The Failed-phase
 		// cleanup (sandbox suspension via the spec.suspend/S1 path is not
 		// automatic; the Loop is terminal and the next reconcile's gates hold
 		// the sandbox). A Normal Event.
-		from := loop.Status.Phase
 		loop.Status.Phase = coxv1alpha1.LoopPhaseFailed
 		loop.Status.DesiredPhase = coxv1alpha1.LoopPhaseFailed
 		setCondition(loop, string(coxv1alpha1.LoopPhaseFailed), metav1.ConditionTrue, budgetFailedReason,
 			fmt.Sprintf("budget cap %s hit at %s (onExceeded=Fail)", capName, value))
-		if r.Recorder != nil {
-			r.Recorder.Eventf(loop, corev1.EventTypeNormal, budgetExceededEventReason,
-				"budget cap %s hit at %s; the Loop is Failed (onExceeded=Fail, from phase %s)", capName, value, from)
-		}
+		// [MUTATION] the Fail-path BudgetExceeded Event (the one naming the Failed phase) DROPPED
 		return true
```

### Build

`go build ./...` — OK.

### Test run

```
Ran 10 of 347 Specs in 20.794 seconds
FAIL! -- 8 Passed | 2 Failed | 0 Pending | 337 Skipped
```

### Failed specs (exact names from the output)

1. **spec 6:** `P2g: conditions + events for every P2 transition (the auditability sweep) [It] spec 6: -> Failed:BudgetExceeded (budget Fail): the Failed condition is reason BudgetExceeded; the BudgetExceeded condition is True; the BudgetExceeded Event fires`
   - Assertion: `a BudgetExceeded Event must be recorded: [...]` — expected non-empty, got empty.

2. **spec 10:** `P2g: conditions + events for every P2 transition (the auditability sweep) [It] spec 10: every transition in the inventory has a condition + an Event with a NON-EMPTY message (the structural sweep)`
   - Failing case: `-> Failed:BudgetExceeded`
   - Assertion: `-> Failed:BudgetExceeded: an Event with reason BudgetExceeded must be recorded: [...]`
   - Expected `ev` to be non-empty; got empty (the Event is absent).

---

## Summary table

| # | Transition | Dropped | Specs that FAIL |
|---|-----------|---------|-----------------|
| M1 | Paused → phase (resume) | `Resumed` Event | 4, 10 |
| M2 | → Paused (stall) | `Paused` condition | 2, 10 |
| M3 | → Paused (stall) | `Paused` Event | 2, 10 |
| M4 | → Failed:BudgetExceeded | `BudgetExceeded` Event (Fail) | 6, 10 |
