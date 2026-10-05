// Copyright 2026 papawattu.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package controller

// P2f (TDD-PLAN-PHASE2): the Paused phase — the pause entry (one mechanism,
// three entry points), the suspension gate, and the resume semantics.
//
// Entry points: spec.suspend=true (pausedReason Suspend — this slice
// generalises the S1 sandbox suspension into a real phase), P2d's
// onExceeded=Pause (Budget) and P2e's stallAction=Pause (Stall). P2d/P2e set
// phase/pausedFrom/pausedReason themselves; this slice owns the suspension
// gate they rely on (ensureSandbox suspends a Paused sandbox regardless of
// spec.suspend).
//
// Resume: spec.suspend=false resumes ONLY a Suspend pause; a Stall or Budget
// pause resumes via the one-shot coxswain.io/resume annotation (the phase to
// resume to is always status.pausedFrom — the exact phase). A resume while a
// budget cap is still exceeded is refused (P3: the sandbox must not bounce
// Running -> Suspended). lastActiveStamp is reset to now on resume so the
// first post-resume reconcile does not add the pause's duration to
// activeSeconds (item E).
import (
	"context"
	"time"

	coxv1alpha1 "github.com/papawattu/coxswain/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// resumeAnnotation is the one-shot resume trigger for a Stall or Budget pause
// (P2f, item 4's simplification). Its value is "true" — NOT an exact-phase
// name: the phase to resume to is always status.pausedFrom, and naming it in
// the annotation would only add a mismatched-value attack surface. The
// operator clears it on a valid resume; a leftover annotation on a
// non-paused Loop is ignored (left for the next pause/resume cycle).
const resumeAnnotation = "coxswain.io/resume"

// Event reasons (P2f). Stable, documented strings (the OS5 pattern).
const (
	// pauseEventReason is the Normal Event reason when a Loop enters Paused.
	pauseEventReason = "Paused"
	// resumeEventReason is the Normal Event reason on a valid resume.
	resumeEventReason = "Resumed"
	// resumeRefusedEventReason is the Warning Event reason when a resume is
	// refused because a budget cap is still exceeded (P3).
	resumeRefusedEventReason = "ResumeRefused"
)

// Paused condition reasons (the condition type is coxswain's PausedCondition).
const (
	// pausedCondReasonPaused names the True-condition state.
	pausedCondReasonPaused = "Paused"
	// resumedCondReason is the False-condition reason on a valid resume.
	resumedCondReason = "Resumed"
	// suspendedRefusedCondReason is the False-condition reason when
	// suspend=true is refused on a Succeeded Loop whose deliver Job is still
	// in flight (item F).
	suspendedRefusedCondReason = "SuspendRefused"
)

// pausedPhases lists every non-terminal phase a Loop may pause FROM (item 9:
// the pause entry is defined for every non-terminal phase — Pending,
// Planning, AwaitingApproval, Implementing, Verifying). Succeeded is
// normally not pausable (terminal-success); its deliver-in-flight case is
// handled by the refusal below, not by a pause entry.
var pausedPhases = []coxv1alpha1.LoopPhase{
	coxv1alpha1.LoopPhasePending,
	coxv1alpha1.LoopPhasePlanning,
	coxv1alpha1.LoopPhaseAwaitingApproval,
	coxv1alpha1.LoopPhaseImplementing,
	coxv1alpha1.LoopPhaseVerifying,
}

// isPausablePhase reports whether a phase the pause entry may be taken from
// (every non-terminal phase; Succeeded/Failed are terminal and not pausable).
func isPausablePhase(p coxv1alpha1.LoopPhase) bool {
	for _, ph := range pausedPhases {
		if p == ph {
			return true
		}
	}
	return false
}

// pausedLoopInDeliveryFlight reports whether a Succeeded Loop has a deliver
// Job in flight: spec.delivery.mode == PullRequest and no status.delivery
// record yet (the deliver Job has not recorded its outcome). Item F:
// suspend=true on such a Loop is REFUSED — delivery finishes, and the
// alternative (suspending while the deliver Job pushes) would strand a
// half-pushed PR.
func pausedLoopInDeliveryFlight(loop *coxv1alpha1.Loop) bool {
	if loop.Status.Phase != coxv1alpha1.LoopPhaseSucceeded {
		return false
	}
	if loop.Spec.Delivery == nil || loop.Spec.Delivery.Mode != coxv1alpha1.DeliveryModePullRequest {
		return false
	}
	return loop.Status.Delivery == nil
}

// loopPaused reports whether the Loop is in the Paused phase (the suspension
// gate's input: a Paused sandbox is Suspended regardless of spec.suspend).
func loopPaused(loop *coxv1alpha1.Loop) bool {
	return loop.Status.Phase == coxv1alpha1.LoopPhasePaused
}

// operatorNow returns the operator's clock (r.now when set — the tests
// advance it — otherwise metav1.Now).
func (r *LoopReconciler) operatorNow() metav1.Time {
	if r.now != nil {
		return r.now()
	}
	return metav1.Now()
}

// applyPauseMechanics runs the P2f pause/resume step. It is called on every
// reconcile, after the S4 bootstrap and BEFORE ensureSandbox, so the sandbox
// is built with the post-pause/resume phase and OperatingMode in the same
// pass.
//
// It returns (pauseBlocked, changed, resumeCleared):
//
//	pauseBlocked — spec.suspend=true was REFUSED because a Succeeded Loop's
//	  deliver Job is still in flight (item F). The caller passes this to
//	  ensureSandbox, which then keeps the sandbox Running (delivery
//	  completes) instead of honouring the suspend.
//	changed — the Loop's status was mutated.
//	resumeCleared — a valid resume happened this reconcile; the caller then
//	  patches the Loop to drop the coxswain.io/resume annotation (I52
//	  pattern, after the status write). A refused resume never clears it.
func (r *LoopReconciler) applyPauseMechanics(ctx context.Context, loop *coxv1alpha1.Loop) (pauseBlocked, changed, resumeCleared bool) {
	if loopPaused(loop) {
		return r.handlePausedLoop(ctx, loop)
	}
	blocked, c := r.handleSuspendEntry(loop)
	return blocked, c, false
}

// handleSuspendEntry processes a spec.suspend=true entry (the S1 path,
// upgraded) on a NON-paused Loop.
//
//   - A pausable non-terminal phase: enter Paused (pausedFrom = the phase
//     the Loop left, pausedReason = Suspend, the Paused condition True, a
//     Normal Event naming the source). The pausedFrom/pausedReason record is
//     written ONLY the first time (a re-reconcile with suspend still true
//     must not overwrite it — a P2d/P2e entry may have set them to
//     Budget/Stall, which the S1 path must not clobber).
//   - Succeeded with a deliver Job in flight: REFUSE (item F) — the phase
//     stays Succeeded, the Paused condition is False with a message naming
//     the reason, the sandbox keeps running (delivery completes).
//   - Terminal (Failed, Succeeded with delivery done / no delivery): not
//     pausable — no phase change, no pausedFrom/pausedReason.
func (r *LoopReconciler) handleSuspendEntry(loop *coxv1alpha1.Loop) (pauseBlocked, changed bool) {
	if !loop.Spec.Suspend {
		return false, false
	}
	if pausedLoopInDeliveryFlight(loop) {
		// Item F: refuse. The deliver Job is in flight — let it finish. The
		// Paused condition records the refusal (False, the reason names it).
		setCondition(loop, coxv1alpha1.PausedCondition, metav1.ConditionFalse,
			suspendedRefusedCondReason,
			"suspend refused while a deliver Job is in flight (phase Succeeded, delivery not recorded)")
		return true, true
	}
	if !isPausablePhase(loop.Status.Phase) {
		// Terminal phase (Failed, or Succeeded with no delivery in flight):
		// the sandbox is suspended via spec.suspend (the S1 branch), but the
		// phase machine does not enter Paused.
		return false, false
	}
	// Enter Paused. First time only: a re-reconcile with suspend still true
	// keeps the existing pausedFrom/pausedReason (never overwritten — the
	// plan's mutation "drop the first-time-only guard" makes spec 2 fail).
	// The phase the Loop left must be captured BEFORE overwriting
	// status.phase.
	enteredFrom := loop.Status.Phase
	loop.Status.Phase = coxv1alpha1.LoopPhasePaused
	if loop.Status.PausedFrom == "" {
		loop.Status.PausedFrom = enteredFrom
		loop.Status.PausedReason = coxv1alpha1.PausedReasonSuspend
	}
	setCondition(loop, coxv1alpha1.PausedCondition, metav1.ConditionTrue,
		pausedCondReasonPaused, "paused (suspend)")
	r.emitPauseEvent(loop)
	return false, true
}

// handlePausedLoop processes a Loop that is already in the Paused phase: it
// evaluates the resume triggers (spec.suspend=false for a Suspend pause; the
// coxswain.io/resume annotation for a Stall/Budget pause), applies the
// P3 refuse-while-exceeded gate and the item-5 re-evaluation, and returns
// (pauseBlocked, changed, resumeCleared). pauseBlocked is false here (the
// Loop is already paused, so the suspension gate holds it regardless of
// spec.suspend); resumeCleared is true only on a valid resume (the caller
// patches the annotation off then).
func (r *LoopReconciler) handlePausedLoop(ctx context.Context, loop *coxv1alpha1.Loop) (pauseBlocked, changed, resumeCleared bool) {
	// No resume trigger: the Loop stays paused. Nothing mutates (the
	// conditions are already set by the entry point).
	suspendResume := loop.Spec.Suspend == false &&
		loop.Status.PausedReason == coxv1alpha1.PausedReasonSuspend
	annotationResume := loop.Annotations[resumeAnnotation] == "true"
	if !suspendResume && !annotationResume {
		return false, false, false
	}

	// A Budget pause re-evaluates the current caps against the current counts
	// (item 5): raising a cap clears the exceedance.
	if loop.Status.PausedReason == coxv1alpha1.PausedReasonBudget {
		changed = r.reEvaluateBudgetOnResume(loop) || changed
	}

	// P3: a resume while a cap is still exceeded is REFUSED — the sandbox
	// stays Suspended (no Running -> Suspended bounce), the annotation is NOT
	// cleared (the operator did not act on it), the Paused condition stays
	// True with a message naming the still-exceeded cap, and a Warning
	// ResumeRefused Event fires.
	if r.budgetStillExceeded(loop) {
		setCondition(loop, coxv1alpha1.PausedCondition, metav1.ConditionTrue,
			pausedCondReasonPaused,
			"resume refused: budget cap still exceeded ("+string(r.exceededCapName(loop.Spec.Budget, loop.Status.Budget))+")")
		r.emitResumeRefusedEvent(loop)
		return false, true, false
	}

	// Valid resume: back to the EXACT pausedFrom phase, the record cleared,
	// the condition False/Resumed, a Normal Event, and — item E — the
	// lastActiveStamp reset to now (the wall clock never counts the pause).
	resumeFrom := loop.Status.PausedReason
	resumeTo := loop.Status.PausedFrom
	loop.Status.Phase = resumeTo
	loop.Status.DesiredPhase = resumeTo
	loop.Status.PausedFrom = ""
	loop.Status.PausedReason = ""
	if loop.Status.Budget != nil {
		now := r.operatorNow()
		loop.Status.Budget.LastActiveStamp = &now
	}
	setCondition(loop, coxv1alpha1.PausedCondition, metav1.ConditionFalse,
		resumedCondReason,
		"resumed: phase "+string(resumeTo)+" (was paused: "+string(resumeFrom)+")")
	r.emitResumeEvent(loop, resumeTo, resumeFrom)
	return false, true, true
}

// reEvaluateBudgetOnResume (item 5) recomputes status.budget.exceeded against
// the CURRENT spec.budget caps and the CURRENT status.budget counts for a
// Budget-paused Loop on resume. Raising a cap (or the counts falling under
// it) CLEARS the exceedance and flips the BudgetExceeded condition to False
// reason ClearedOnResume with a Normal Event. It returns true when the
// Loop's status was mutated.
func (r *LoopReconciler) reEvaluateBudgetOnResume(loop *coxv1alpha1.Loop) bool {
	b := loop.Status.Budget
	if b == nil || !b.Exceeded {
		return false
	}
	spec := loop.Spec.Budget
	if spec == nil {
		// No caps at all: the exceedance cannot persist.
		b.Exceeded = false
		b.ExceededReason = ""
		r.clearBudgetExceededCondition(loop)
		return true
	}
	stillHit := r.exceededCapName(spec, b)
	if stillHit == "" {
		// No cap is hit anymore (caps were raised, or counts fell under).
		b.Exceeded = false
		b.ExceededReason = ""
		r.clearBudgetExceededCondition(loop)
		return true
	}
	return false
}

// clearBudgetExceededCondition flips the BudgetExceeded condition to False
// reason ClearedOnResume and emits a Normal Event (item 5).
func (r *LoopReconciler) clearBudgetExceededCondition(loop *coxv1alpha1.Loop) {
	setCondition(loop, coxv1alpha1.BudgetExceededCondition, metav1.ConditionFalse,
		"ClearedOnResume", "budget cap no longer exceeded (re-evaluated on resume)")
	if r.Recorder != nil {
		r.Recorder.Eventf(loop, corev1.EventTypeNormal,
			coxv1alpha1.BudgetExceededCondition,
			"budget exceedance cleared on resume (caps raised)")
	}
}

// budgetStillExceeded reports whether any spec.budget cap is still hit
// against the current counts (the P3 refuse-while-exceeded gate's input). It
// is true for a Budget pause whose exceedance was not cleared by the
// re-evaluation, and for a Suspend/Stall pause whose wall clock has since
// elapsed (a cap that was not hit before the pause has since been hit).
func (r *LoopReconciler) budgetStillExceeded(loop *coxv1alpha1.Loop) bool {
	spec := loop.Spec.Budget
	if spec == nil {
		return false
	}
	if loop.Status.Budget != nil && loop.Status.Budget.Exceeded {
		// Already recorded as exceeded (a Budget pause not cleared by the
		// re-evaluation): the caps are still hit.
		return true
	}
	// No recorded exceedance, but a cap may have been hit SINCE the pause
	// (the wall clock elapsed while paused). Re-check the caps.
	return r.exceededCapName(spec, loop.Status.Budget) != ""
}

// exceededCapName returns the name of the first spec.budget cap that the
// current counts exceed (empty when no cap is hit). b may be nil (no counts
// recorded yet — only the wall-clock cap can be evaluated, from
// b.ActiveSeconds, which is 0 when b is nil).
func (r *LoopReconciler) exceededCapName(spec *coxv1alpha1.BudgetConfig, b *coxv1alpha1.BudgetStatus) string {
	if spec == nil {
		return ""
	}
	var (
		tokens int64
		active int64
	)
	if b != nil {
		tokens = b.PromptTokens + b.CompletionTokens
		active = b.ActiveSeconds
	}
	if spec.MaxTokens != nil && tokens > *spec.MaxTokens {
		return "Tokens"
	}
	if wc := r.parseMaxWallClockSeconds(spec.MaxWallClock); wc > 0 && active > wc {
		return "WallClock"
	}
	// The cost cap needs prices; without them the derivation is inert
	// (fail-closed, P2d). Not evaluated here.
	return ""
}

// parseMaxWallClockSeconds parses spec.budget.maxWallClock (a Go duration
// string) into seconds via the shared seam (0 when unset, malformed, or
// sub-second — those read as no wall-clock cap for the P2f re-evaluation;
// the CRD pattern and the P2d decision enforce the "≥1s when set" rule).
func (r *LoopReconciler) parseMaxWallClockSeconds(s string) int64 {
	d, err := coxv1alpha1.ParseMaxWallClock(s)
	if err != nil || d <= 0 {
		return 0
	}
	return int64(d / time.Second)
}

// emitPauseEvent emits the Normal Paused Event naming the pause source
// (P2f: the message names the source — suspend).
func (r *LoopReconciler) emitPauseEvent(loop *coxv1alpha1.Loop) {
	if r.Recorder == nil {
		return
	}
	r.Recorder.Eventf(loop, corev1.EventTypeNormal, pauseEventReason,
		"paused: source suspend, from phase %s", loop.Status.PausedFrom)
}

// emitResumeEvent emits the Normal Resumed Event naming the resumed-to phase
// and the paused-from reason (P2f).
func (r *LoopReconciler) emitResumeEvent(loop *coxv1alpha1.Loop, to coxv1alpha1.LoopPhase, from coxv1alpha1.PausedReason) {
	if r.Recorder == nil {
		return
	}
	r.Recorder.Eventf(loop, corev1.EventTypeNormal, resumeEventReason,
		"resumed: phase %s (paused reason %s)", to, from)
}

// emitResumeRefusedEvent emits the Warning ResumeRefused Event (P3: the caps
// are still hit, so the resume is refused and the sandbox stays Suspended).
func (r *LoopReconciler) emitResumeRefusedEvent(loop *coxv1alpha1.Loop) {
	if r.Recorder == nil {
		return
	}
	reason := ""
	if loop.Status.Budget != nil {
		reason = string(loop.Status.Budget.ExceededReason)
	}
	r.Recorder.Eventf(loop, corev1.EventTypeWarning, resumeRefusedEventReason,
		"resume refused: budget cap still exceeded (%s); raise the cap and retry", reason)
}

// removeResumeAnnotation patches the Loop to drop the coxswain.io/resume
// annotation after a VALID resume (the I52 pattern: a merge patch from a fresh
// read, run AFTER the trailing status update so the two Loop writes never
// race). It is called only on the valid-resume path; a refused resume never
// clears the annotation (the operator did not act on it — the caller leaves
// it for the retry). No-op when the fresh object does not carry it.
func (r *LoopReconciler) removeResumeAnnotation(ctx context.Context, loop *coxv1alpha1.Loop) error {
	fresh := &coxv1alpha1.Loop{}
	if err := r.Get(ctx, types.NamespacedName{Name: loop.Name, Namespace: loop.Namespace}, fresh); err != nil {
		return client.IgnoreNotFound(err)
	}
	if _, present := fresh.Annotations[resumeAnnotation]; !present {
		return nil // absent (already cleared)
	}
	updated := fresh.DeepCopy()
	delete(updated.Annotations, resumeAnnotation)
	if err := r.Patch(ctx, updated, client.MergeFrom(fresh)); err != nil {
		return err
	}
	return nil
}
