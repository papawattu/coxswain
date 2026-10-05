# P2f Gate Mutations (I49 norm, scratch worktree)

Worktree: `/tmp/p2f-gate` (detached HEAD at `af73a67`). Each mutation applied exactly, confirmed applied+building, named spec run. Results below are copied from the test output.

## Mutation 1: Drop the `pausedReason` check from the `suspend=false` resume

**Diff** (1 line changed in `resumeTriggered`):
```diff
-	if loop.Status.PausedReason == coxv1alpha1.PausedReasonSuspend {
-		return !loop.Spec.Suspend
+	if !loop.Spec.Suspend {
+		return true
 	}
```

**Result:** Spec 6 FAILS — `suspend=false must NOT resume a Budget pause`. 302 Passed | 1 Failed.

## Mutation 2: Drop the `phase != Paused` gate from the claim reader

**Diff** (early return commented out in `advancePhaseFromClaim`):
```diff
-	if loop.Status.Phase == coxv1alpha1.LoopPhasePaused {
-		return false, false
-	}
+	// (removed)
```

**Result:** Spec 4 does NOT fail. The `nextPhase` table itself prevents advancing from `Paused` (`nextPhase(Paused, X)` returns `Paused` for all X). The early return is defensive but the table is the real gate. **Equivalent mutant** — the table, not the early return, is the protection.

## Mutation 3: Overwrite `pausedFrom`/`pausedReason` on every reconcile

**Diff** (guard removed in `handleSuspendEntry`):
```diff
-	if loop.Status.PausedFrom == "" {
-		loop.Status.PausedFrom = enteredFrom
-		loop.Status.PausedReason = coxv1alpha1.PausedReasonSuspend
-	}
+	loop.Status.PausedFrom = enteredFrom
+	loop.Status.PausedReason = coxv1alpha1.PausedReasonSuspend
```

**Result:** Equivalent mutant. An already-paused Loop is dispatched to `handlePausedLoop` (the `loopPaused` check in `applyPauseMechanics`), so `handleSuspendEntry` never sees a paused Loop and the guard is unreachable. The real protection is the **dispatch**.

## Mutation 3b: Remove the `loopPaused` dispatch (the real protection)

**Diff** (dispatch removed + overwrite guard dropped):
```diff
-	if loopPaused(loop) {
-		return r.handlePausedLoop(ctx, loop)
-	}
+	// (removed)
```

**Result:** 296 Passed | 7 Failed. Failed specs: **5, 7, 8, 11, 12, 13, 15**. The dispatch is the real protection — without it, `handleSuspendEntry` runs for already-paused Loops and overwrites `pausedFrom` with `Paused`, corrupting the resume target. Spec 2 itself passes (the overwrite sets `pausedFrom=Paused` but the test's re-reconcile path doesn't trigger the overwrite because `handleSuspendEntry` is called before the status is written back).

## Mutation 4: Treat the resume annotation as an exact-phase name

**Diff** (annotation check changed in `resumeTriggered`):
```diff
-	return loop.Annotations[resumeAnnotation] == "true"
+	return loop.Annotations[resumeAnnotation] == string(loop.Status.PausedFrom)
```

**Result:** Spec 7 FAILS — `back to the exact pausedFrom phase`. 300 Passed | 3 Failed (spec 7 + collateral).

## Mutation 5: Make `exceeded` sticky-through-resume (drop re-evaluation)

**Diff** (re-evaluation call commented out in `handlePausedLoop`):
```diff
-	if loop.Status.PausedReason == coxv1alpha1.PausedReasonBudget {
-		r.reEvaluateBudgetOnResume(loop)
-	}
+	// (removed)
```

**Result:** Spec 8 FAILS — the raised cap does not clear the exceedance. 302 Passed | 1 Failed.

## Mutation 6: Resume a still-exceeded pause anyway (drop P3 refuse)

**Diff** (P3 gate commented out in `handlePausedLoop`):
```diff
-	if r.budgetStillExceeded(loop) {
-		setCondition(...)
-		r.emitResumeRefusedEvent(loop)
-		return false, true, false
-	}
+	// (removed)
```

**Result:** Spec 8 FAILS — the sandbox transitions Suspended→Running on the resume (the `ResumeRefused` Event is absent). 302 Passed | 1 Failed.

## Mutation 7: Allow pause from `Succeeded`/`Failed` (drop terminal-phase guard)

**Diff** (terminal guard commented out in `handleSuspendEntry`):
```diff
-	if !isPausablePhase(loop.Status.Phase) {
-		return false, false
-	}
+	// (removed)
```

**Result:** Spec 9 FAILS — `pausedFrom` is set on a terminal phase. 302 Passed | 1 Failed.

## Mutation 8: Suspend a `Succeeded` Loop with a deliver Job in flight (drop item-F refusal)

**Diff** (item-F refusal commented out in `handleSuspendEntry`):
```diff
-	if pausedLoopInDeliveryFlight(loop) {
-		setCondition(...)
-		return true, true
-	}
+	// (removed)
```

**Result:** Spec 9 FAILS — the deliver Job is suspended/deleted, delivery is stranded. 302 Passed | 1 Failed.

## Mutation 9: Resume to a hard-coded phase (drop exact-phase resume)

**Diff** (resume target changed in `handlePausedLoop`):
```diff
-	resumeTo := loop.Status.PausedFrom
+	resumeTo := coxv1alpha1.LoopPhaseImplementing
```

**Result:** 300 Passed | 3 Failed. Failed specs: **7, 12, 13** (collateral). Spec 5 (the named spec) does NOT fail because it pauses from `Implementing` — resuming to `Implementing` is the same as the correct behavior. The hard-coded `Implementing` only breaks specs that pause from a DIFFERENT phase (7: Verifying, 12: Implementing→resume→Implementing but the iteration check fails, 13: Planning/Pending/AwaitingApproval).

## Mutation 10: Reset `lastActiveStamp` to `zero` on resume

**Diff** (reset to zero instead of now in `handlePausedLoop`):
```diff
-		now := r.operatorNow()
-		loop.Status.Budget.LastActiveStamp = &now
+		loop.Status.Budget.LastActiveStamp = &metav1.Time{}
```

**Result:** Spec 15 FAILS — the reset stamp is the zero time, not the resume time. 302 Passed | 1 Failed.

## Mutation 11: Do not reset `lastActiveStamp` at all on resume

**Diff** (reset block commented out in `handlePausedLoop`):
```diff
-	if loop.Status.Budget != nil {
-		now := r.operatorNow()
-		loop.Status.Budget.LastActiveStamp = &now
-	}
+	// (removed)
```

**Result:** Spec 15 FAILS — the stamp is not reset (the round-1 bug: the first post-resume reconcile adds the whole pause). 302 Passed | 1 Failed.
