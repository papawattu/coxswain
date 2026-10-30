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

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	coxv1alpha1 "github.com/papawattu/coxswain/api/v1alpha1"
)

// The gate helpers below were extracted from verifyOutcome (F60-lint: the F60
// canary switch and the check scan put it over the gocyclo budget of 30). Each
// returns the decision for its own gate ONLY; the gate is skipped (nil) when
// the pod status shows the gate has not finished yet and verifyOutcome keeps
// walking (the gate is still in progress — the I49 norm: in progress is never
// a decision). verifyOutcome calls them in evidence order (tamper -> artifact
// -> canary) and returns the first non-nil decision.
type verifyDecision struct {
	// outcome is the verifyOutcome result code for this gate.
	outcome int
	// requeue is the verifyOutcome requeue flag. A gate that returns
	// requeue=true is never a decision (it is in progress); verifyOutcome
	// stops walking on it.
	requeue bool
	// container is the failing container's name (for the status progress
	// record) and exit code (the container's own evidence).
	container string
	exitCode  int32
}

// tamperGateDecision reads the tamper init's gate decision from the pod's
// init statuses. The tamper init is the GATE to artifact: a terminated
// non-zero tamper means the protected paths changed (verifyTampered,
// terminal) or the script's own git diff failed (tamperGitErrorExit, an
// infrastructure failure, never a subversion/tamper verdict) or the kubelet
// OOM-killed it (F83c: an infrastructure fault, never tampering).
//
// Returns nil when the gate is not in force yet: the tamper status is absent
// from the pod (a malformed Job / pre-F39 legacy pod — the caller treats an
// absent gate as "not finished", requeue) or the tamper has not terminated
// yet (Running/Waiting — in progress, never a decision: the I49 norm).
func tamperGateDecision(pod *corev1.Pod) *verifyDecision {
	var status *corev1.ContainerStatus
	for i := range pod.Status.InitContainerStatuses {
		if pod.Status.InitContainerStatuses[i].Name == verifyTamperInit {
			status = &pod.Status.InitContainerStatuses[i]
			break
		}
	}
	if status == nil {
		// The tamper init is not in the pod status at all: no evidence, no
		// decision — the caller requeues (exactly as verifyOutcome's
		// tamperIdx < 0 branch did).
		return &verifyDecision{outcome: verifyNoDecision, requeue: true, container: verifyTamperInit, exitCode: 0}
	}
	if status.State.Terminated == nil {
		// Still Running or Waiting: in progress — no decision, requeue.
		return &verifyDecision{outcome: verifyNoDecision, requeue: true, container: verifyTamperInit, exitCode: 0}
	}
	switch {
	case status.State.Terminated.ExitCode == tamperGitErrorExit:
		// F39: the script's own git diff failed (an infrastructure fault),
		// not that the agent changed a protected path. It fails closed as an
		// infra failure, never as TamperedVerify.
		return &verifyDecision{outcome: verifyInfraFailed, requeue: false, container: verifyTamperInit, exitCode: status.State.Terminated.ExitCode}
	case status.State.Terminated.Reason == tamperOOMKilledReason:
		// F83c: an OOMKilled tamper init is the kubelet's kill, not a
		// protected-path diff. Mapping it to verifyTampered would fail the
		// Loop permanently for a resource limit, so it is an infra failure
		// (the same fail-closed-but-not-blamed treatment as the git-error
		// exit above).
		return &verifyDecision{outcome: verifyInfraFailed, requeue: false, container: verifyTamperInit, exitCode: status.State.Terminated.ExitCode}
	case status.State.Terminated.ExitCode != 0:
		return &verifyDecision{outcome: verifyTampered, requeue: false, container: verifyTamperInit, exitCode: status.State.Terminated.ExitCode}
	}
	// Clean (exit 0): the gate passes — no decision from this gate.
	return nil
}

// artifactGateDecision reads the artifact init's gate decision from the pod's
// init statuses. The I47 artifact check (the operator-side filter) runs
// BEFORE the checks: it is the gate to the acceptance checks, exactly like
// tamper is the gate to artifact (evidence order: tamper -> artifact ->
// checks). A non-zero artifact exit is an ITERATE with the container's own
// name ('artifact') and exit code.
//
// Returns nil when the artifact status is absent from the pod (a pre-I47
// legacy pod — the gate is not in force, fall through to the checks) or the
// artifact has not terminated yet (in progress — no decision, requeue: the
// S5a pending-regression class).
func artifactGateDecision(pod *corev1.Pod) *verifyDecision {
	var status *corev1.ContainerStatus
	for i := range pod.Status.InitContainerStatuses {
		if pod.Status.InitContainerStatuses[i].Name == verifyArtifactInit {
			status = &pod.Status.InitContainerStatuses[i]
			break
		}
	}
	if status == nil {
		// Absent from the pod's status (a pre-I47 legacy pod, or a test
		// fixture that plants the old init set): the artifact gate is not in
		// force — the caller falls through to the checks (the pre-I47
		// decision path).
		return nil
	}
	if status.State.Terminated == nil {
		// Still Running or Waiting: in progress — PENDING, no decision, requeue
		// (the S5a pending-regression class: a non-terminated init is a wait,
		// not an iterate).
		return &verifyDecision{outcome: verifyNoDecision, requeue: true, container: verifyArtifactInit, exitCode: 0}
	}
	if status.State.Terminated.ExitCode != 0 {
		return &verifyDecision{outcome: verifyIterate, requeue: false, container: verifyArtifactInit, exitCode: status.State.Terminated.ExitCode}
	}
	// Clean (exit 0): the gate passes — no decision from this gate.
	return nil
}

// canaryGateDecision reads the canary init's gate decision from the pod's
// init statuses (F60, D17.1). The canary runs BETWEEN artifact and check-0
// (tamper -> artifact -> canary -> checks). Tamper and artifact are clean at
// this point, so the canary is the next gate. A terminated canary with exit
// verifyCanarySubvertedExit (43) is subversion (verifySubverted, terminal),
// verifyCanaryUnavailableExit (44) is canary-unavailable
// (verifyCanaryUnavailable, no decision + one event),
// verifyCanaryBuildErrorExit (45) is an infra failure (verifyInfraFailed,
// bounded retry). A terminated canary with exit verifyCanaryHealthyExit (0)
// is the HEALTHY negative control — the generated test honestly failed and
// the gate passes, so the caller proceeds to the acceptance checks.
//
// Returns nil when the canary is absent from the pod's status (a pre-F60
// legacy pod, or a test fixture that plants the old init set — the canary
// gate is not in force, so the caller falls through to the checks, the
// pre-F60 decision path) or the canary has not terminated yet (a canary
// still Running/Waiting is PENDING — no decision, requeue, exactly like the
// tamper / artifact gates above).
func canaryGateDecision(pod *corev1.Pod) *verifyDecision {
	canary := readVerifyCanary(pod)
	if !canary.found {
		// Absent: the canary gate is not in force (pre-F60 legacy pod or a
		// fixture that plants the old init set) — fall through to the checks.
		return nil
	}
	if !canary.terminated {
		// Still Running or Waiting: in progress — no decision, requeue (the
		// I49 norm, exactly like the tamper / artifact gates above).
		return &verifyDecision{outcome: verifyNoDecision, requeue: true, container: verifyCanaryInit, exitCode: 0}
	}
	switch canary.code {
	case verifyCanarySubvertedExit:
		// The test binary ignored the forced t.Fatal (the round-8 os.Exit(0)
		// probe) — subversion. Terminal, like tamper: the agent's own code
		// made the test binary dishonest, so no check can be trusted.
		return &verifyDecision{outcome: verifySubverted, requeue: false, container: verifyCanaryInit, exitCode: canary.code}
	case verifyCanaryUnavailableExit:
		// The target package has no *_test.go (the canary has nothing to run):
		// NO decision on the agent's work. Return requeue=false so the
		// Reconcile reaches applyVerifyOutcome, which records the one-time
		// VerifyCanaryUnavailable event and requeues (staying Verifying). It
		// is not a subversion verdict, not an infra retry (M7).
		return &verifyDecision{outcome: verifyCanaryUnavailable, requeue: false, container: verifyCanaryInit, exitCode: canary.code}
	case verifyCanaryBuildErrorExit:
		// A build error is an infra failure (bounded retry), NEVER a
		// subversion verdict (M8's trap: mapping 45 to infra is correct; the
		// subverted path is exit 43 only).
		return &verifyDecision{outcome: verifyInfraFailed, requeue: false, container: verifyCanaryInit, exitCode: canary.code}
	case verifyCanaryHealthyExit:
		// exit 0 (verifyCanaryHealthyExit): the generated test honestly failed,
		// the negative control held. The gate passes — no decision from this
		// gate, the caller proceeds to the acceptance checks.
		return nil
	default:
		// Unrecognized code (not 0/43/44/45): not a defined canary outcome.
		// No decision — the caller falls through to the check scan (the
		// original verifyOutcome's fall-through for 0, now also for any other
		// code, since the canary script never produces one).
		return nil
	}
}

// checkGateDecision reads the check inits' gate decision from the pod's init
// statuses (check-0 .. check-<checkCount-1). A terminated check with a
// non-zero exit is an ITERATE with the check's own name and exit code. An
// OOM-killed check (the kubelet's OOMKilled reason, F58) is an INFRA failure
// (bounded retry, never an iterate — the exit code 137 is SIGKILL in both,
// so the reason is what separates a resource limit from a check that killed
// itself): it is decided BEFORE the code!=0 branch.
//
// Returns nil when no decision is reached: a check's status is absent from
// the pod (a malformed Job with fewer inits than expected — no evidence) or
// a check is still Running/Waiting (in progress — no decision, requeue:
// treating it as a failure is the S5a regression that iterated a passing
// run to Failed:MaxIterationsExceeded with the misleading text
// 'check-0 failed (exit 0)'). When all checks terminated with exit 0 the
// decision is verifySucceeded (the whole evidence ran clean).
func checkGateDecision(pod *corev1.Pod, checkCount int) *verifyDecision {
	for i := range checkCount {
		name := fmt.Sprintf("check-%d", i)
		check := readVerifyCheck(pod, name)
		if !check.found {
			// The check init is not in the pod status at all (the Job pod has
			// fewer inits than expected — a malformed Job). No evidence: the
			// caller takes no decision and requeues.
			return &verifyDecision{outcome: verifyNoDecision, requeue: true}
		}
		if !check.terminated {
			// Not terminated: the check is still running or waiting (kubelet
			// has not advanced it yet). That is PENDING, not a failure —
			// exactly like a non-terminated tamper: take no decision and
			// requeue.
			return &verifyDecision{outcome: verifyNoDecision, requeue: true}
		}
		code, oom := check.code, check.oom
		// F58: an OOM-killed check is the kubelet killing the container at
		// its memory limit, not the agent's work failing — the same
		// fail-closed-but-not-blamed rule F83c applies to the tamper init.
		// It goes to verifyInfraFailed (a bounded infra retry, then
		// Failed:VerifyInfraFailed) BEFORE the code!=0 branch, because the
		// OOM exit code is 137 and would otherwise be reported as 'check-k
		// failed (exit 137)' — burning an iteration and a stall entry for a
		// resource limit.
		if oom {
			return &verifyDecision{outcome: verifyInfraFailed, requeue: false, container: name, exitCode: code}
		}
		if code != 0 {
			return &verifyDecision{outcome: verifyIterate, requeue: false, container: name, exitCode: code}
		}
	}
	// Every check terminated with exit 0: the whole evidence ran clean.
	return &verifyDecision{outcome: verifySucceeded, requeue: false}
}

// The verifyOutcome action helpers below were extracted from
// applyVerifyOutcome (F60-lint: the F60 subverted / canary-unavailable
// cases put it over the gocyclo budget of 30). They are the switch cases
// verbatim — each mutates the Loop's status exactly as the case did and
// returns (changed, requeue).

// applyVerifySucceeded is the applyVerifyOutcome verifySucceeded case: the
// last verify passed, so the failure summary is cleared and the Loop either
// holds at Verifying until delivery settles (deliveryEnabled, P5f) or
// advances to Succeeded.
func (r *LoopReconciler) applyVerifySucceeded(loop *coxv1alpha1.Loop) (bool, bool) {
	// B4: clear the failure summary (the last verify passed — the failure
	// is resolved; the next planning prompt must not carry a stale summary).
	clearLastFailureSummary(loop)
	if deliveryEnabled(loop) {
		// P5f (I98, D69 option i): hold at Verifying (reason Delivering)
		// until the Delivered condition is terminal; Succeeded (phase and
		// desiredPhase) is set by settleDeliveryHold.
		setCondition(loop, string(coxv1alpha1.LoopPhaseVerifying), metav1.ConditionTrue,
			coxv1alpha1.ReasonVerifyingDelivering,
			"verify passed; holding Succeeded until delivery (Delivered condition) is terminal")
		return true, false
	}
	loop.Status.Phase = coxv1alpha1.LoopPhaseSucceeded
	loop.Status.DesiredPhase = coxv1alpha1.LoopPhaseSucceeded
	// B5a: terminal transition — bump the completion counter and clear the
	// active gauge.
	bumpLoopCompletion("succeeded", "verify_passed")
	return true, false
}

// applyVerifyNotDescendant is the applyVerifyOutcome verifyNotDescendant
// case (F43): the pinned commit does not descend from baseCommit. Terminal
// and distinct from TamperedVerify / VerifyInfraFailed (no retry: a re-run
// would import the same pin).
func (r *LoopReconciler) applyVerifyNotDescendant(loop *coxv1alpha1.Loop) (bool, bool) {
	if loop.Status.Phase == coxv1alpha1.LoopPhaseVerifying {
		setCondition(loop, string(coxv1alpha1.LoopPhaseFailed), metav1.ConditionTrue,
			PinNotDescendantReason, "the pinned commit is not a descendant of baseCommit (the runner's headCommit claim was refused)")
		loop.Status.Phase = coxv1alpha1.LoopPhaseFailed
		loop.Status.DesiredPhase = coxv1alpha1.LoopPhaseFailed
		bumpLoopCompletion("failed", "pin_not_descendant")
		if r.Recorder != nil {
			r.Recorder.Eventf(loop, corev1.EventTypeWarning, PinNotDescendantReason,
				"the pinned commit %s is not a descendant of baseCommit %s; failing the Loop",
				loop.Status.CurrentVerify.VerifiedCommit, loop.Status.BaseCommit)
		}
		return true, false
	}
	return false, false
}

// applyVerifyTampered is the applyVerifyOutcome verifyTampered case: the
// tamper verdict (the tamper init terminated with a protected path changed
// between baseCommit and verifiedCommit). Terminal: fail the Loop with the
// distinct TamperedVerify reason.
func (r *LoopReconciler) applyVerifyTampered(loop *coxv1alpha1.Loop) (bool, bool) {
	if loop.Status.Phase == coxv1alpha1.LoopPhaseVerifying {
		setCondition(loop, string(coxv1alpha1.LoopPhaseFailed), metav1.ConditionTrue,
			TamperedVerifyReason, "a protected path changed between baseCommit and verifiedCommit")
		loop.Status.Phase = coxv1alpha1.LoopPhaseFailed
		loop.Status.DesiredPhase = coxv1alpha1.LoopPhaseFailed
		// B5a: terminal transition — bump the completion counter and clear the
		// active gauge.
		bumpLoopCompletion("failed", "tampered")
		return true, false
	}
	return false, false
}

// applyVerifySubverted is the applyVerifyOutcome verifySubverted case (F60,
// D17.1): the canary detected in-process subversion — the test binary
// ignored the forced t.Fatal (exit 43, the round-8 os.Exit(0) probe).
// Terminal, like TamperedVerify: the agent's own code made the test binary
// dishonest, so no acceptance check is trustworthy. Fail the Loop with the
// distinct SubvertedVerify reason; no new verify Job is created (a re-run
// would import the same subverted commit).
func (r *LoopReconciler) applyVerifySubverted(loop *coxv1alpha1.Loop) (bool, bool) {
	if loop.Status.Phase == coxv1alpha1.LoopPhaseVerifying {
		setCondition(loop, string(coxv1alpha1.LoopPhaseFailed), metav1.ConditionTrue,
			SubvertedVerifyReason,
			"the verify canary detected in-process subversion: the test binary ignored the forced failure (round-8 probe)")
		loop.Status.Phase = coxv1alpha1.LoopPhaseFailed
		loop.Status.DesiredPhase = coxv1alpha1.LoopPhaseFailed
		// B5a: terminal transition — bump the completion counter and clear the
		// active gauge.
		bumpLoopCompletion("failed", "subverted")
		if r.Recorder != nil {
			r.Recorder.Eventf(loop, corev1.EventTypeWarning, SubvertedVerifyReason,
				"the verify canary detected in-process subversion (the test binary ignored the forced failure); failing the Loop")
		}
		return true, false
	}
	return false, false
}

// applyVerifyCanaryUnavailable is the applyVerifyOutcome
// verifyCanaryUnavailable case (F60): the canary exited 44 — the target
// package has no *_test.go (nothing to canary). NO decision on the agent's
// work: the Loop stays Verifying (requeue), and the operator records a
// VerifyCanaryUnavailable Warning event exactly ONCE (idempotent — gated on
// a one-shot annotation so it does not repeat every reconcile).
func (r *LoopReconciler) applyVerifyCanaryUnavailable(loop *coxv1alpha1.Loop) (bool, bool) {
	if loop.Status.Phase == coxv1alpha1.LoopPhaseVerifying &&
		loop.Annotations[verifyCanaryUnavailableAnnotation] != "1" && r.Recorder != nil {
		if loop.Annotations == nil {
			loop.Annotations = map[string]string{}
		}
		loop.Annotations[verifyCanaryUnavailableAnnotation] = "1"
		r.Recorder.Eventf(loop, corev1.EventTypeWarning, VerifyCanaryUnavailableReason,
			"the verify canary is unavailable (no *_test.go in the target package); no decision recorded")
		return true, true // requeue (stays Verifying)
	}
	return true, true // no decision: requeue, stay Verifying
}

// applyVerifyIterate is the applyVerifyOutcome verifyIterate case: a failing
// acceptance check sends the Loop BACK (the stall gate is evaluated first,
// BEFORE the budget/maxIterations cap — stall wins: a capped loop that is
// ALSO stalled is Failed:Stalled, not Failed:MaxIterationsExceeded). When
// the cap is hit the Loop is Failed:MaxIterationsExceeded (terminal, the
// hot-loop guard) instead of iterating. The destination is onVerifyFail:
// Implement (the default) iterates back to Implementing; Plan re-plans back
// to Planning with the failure summary fed into the next planning prompt.
//
// The iteration is STILL incremented either way (the stall detector and
// maxIterations count iterations across re-plans — a re-plan is not a reset;
// the cap applies to both destinations). The pin is cleared either way (the
// next run produces a new headCommit and re-pins on the next advance). With
// approval.mode Manual the re-plan goes through AwaitingApproval again (the
// claim-driven advance: the runner's next Planning claim, once the user
// approves, advances to Implementing — the gate's hold re-enters on the new
// plan). The operator is the sole writer of status (ADR-0004).
func (r *LoopReconciler) applyVerifyIterate(ctx context.Context, loop *coxv1alpha1.Loop, pod *corev1.Pod, failedCheck string, checkExitCode int32) (bool, bool) {
	// P2e stall gate (evaluated BEFORE the budget/maxIterations cap —
	// stall wins). Read the failing check's raw output (the readCheckOutput
	// seam / terminationMessage), normalise it, append the StallEntry
	// (dedup'd by jobName), and evaluate the stall decision. A fire applies
	// the stall action (Fail / Pause / Continue); a non-fire falls through
	// to the iterate below.
	if r.applyStallGate(ctx, loop, pod, failedCheck) {
		return true, false
	}
	// B4 (PLAN-BETA B4; PLAN.md Phase 6 remainder): onVerifyFail selects the
	// destination on a failed verify. Implement (the default) iterates back
	// to Implementing (the agent re-works the failing check — the alpha
	// behaviour). Plan sends the Loop back to PLANNING with the failure
	// summary fed into the next planning prompt (reusing B3's plan-feedback
	// channel, .coxswain/plan-feedback, written by the phase-init from the
	// durable status field status.verify.lastFailureSummary).
	onVerifyFail := loop.Spec.Verify.OnVerifyFail
	if onVerifyFail == "" {
		onVerifyFail = coxv1alpha1.OnVerifyFailImplement // the CRD default
	}
	nextIter := max(loop.Status.Iteration+1, 2)
	cap := loop.Spec.Loop.MaxIterations
	if cap <= 0 {
		cap = defaultMaxIterations
	}
	if nextIter > cap {
		// The cap is hit: the last allowed iteration's check failed. Fail
		// the Loop (terminal) instead of iterating again — this is the
		// hot-loop guard (a failing check without a cap would create
		// verify-N jobs in a tight loop). The cap applies to BOTH the
		// iterate (Implementing) and the re-plan (Planning) paths: a
		// re-plan does not bypass it (the increment is the same either way,
		// so the cap is reached at the same iteration).
		setCondition(loop, string(coxv1alpha1.LoopPhaseFailed), metav1.ConditionTrue,
			MaxIterationsExceededReason,
			fmt.Sprintf("acceptance check %s failed (exit %d) on iteration %d; the maxIterations cap (%d) is reached",
				failedCheck, checkExitCode, loop.Status.Iteration, cap))
		loop.Status.Phase = coxv1alpha1.LoopPhaseFailed
		loop.Status.DesiredPhase = coxv1alpha1.LoopPhaseFailed
		// B5a: terminal transition — bump the completion counter and clear the
		// active gauge.
		bumpLoopCompletion("failed", "max_iterations")
		return true, false
	}
	loop.Status.Iteration = nextIter
	// B5a: count the iteration increment (the iterate and re-plan paths both
	// increment status.iteration; this is the "iterations per Loop" metric
	// from PLAN-BETA B5).
	bumpIteration()
	from := loop.Status.Phase // Verifying — the ACTUAL previous phase (OS5)
	// Clear the current pin: the next run will produce a new headCommit and
	// re-pin on the next advance (the fresh Job is created for THAT pin only).
	// Applies to BOTH the iterate (Implementing) and the re-plan (Planning)
	// paths.
	loop.Status.CurrentVerify = nil
	// F83d (P3-5): the tamper record is per-pin, so it goes with the pin.
	// Without this the previous pin's verdict stays on display for the WHOLE
	// next Implementing run (the record is only re-stamped at the next
	// advance, and the record writer runs before this decision in the same
	// pass) — exactly the window P3-5 names, and the delivered-PR-body
	// renderer reads it with no pin guard.
	clearTamperRecord(loop.Status.Verify)
	// F52 B2 follow-up (pi3 early-343 P3-1): the settle record on the
	// Verifying condition (False, reason Delivered / DeliveryFailed /
	// VerifySettled) describes the iteration that settled. This attempt
	// FAILED and the Loop is iterating, so the record is stale — clear it
	// with the rest of the per-pin state (the line above) instead of
	// leaving "verify passed, delivered" on a Loop that is going back to
	// Implementing or Planning.
	clearCondition(loop, string(coxv1alpha1.LoopPhaseVerifying))
	// OS1: record the failing check into progress (the next run can target
	// it). The progress block is the operator's structured record; this is
	// the operator's own evidence (the verify Job pod's exit codes), not a
	// runner claim.
	if loop.Status.Progress == nil {
		loop.Status.Progress = &coxv1alpha1.ProgressStatus{}
	}
	loop.Status.Progress.LastResultStatus = fmt.Sprintf("check-failed: %s (exit %d)", failedCheck, checkExitCode)
	loop.Status.Progress.Iteration = loop.Status.Iteration
	switch onVerifyFail {
	case coxv1alpha1.OnVerifyFailPlan:
		// B4: re-plan — back to Planning with the failure summary fed into
		// the next planning prompt (the B3 plan-feedback channel). The
		// failure summary is made DURABLE on status.verify.lastFailureSummary
		// (the operator's record; the phase-init reads it to write .coxswain/
		// plan-feedback). The summary names the failing check, its exit code,
		// and (when readable) the check's raw output tail (reusing the
		// readCheckOutputSeam the stall gate already read — no second read).
		loop.Status.Phase = coxv1alpha1.LoopPhasePlanning
		loop.Status.DesiredPhase = coxv1alpha1.LoopPhasePlanning
		// I42b: increment planAttempt so a stale Planning success claim
		// from a prior attempt is rejected by advancePhaseFromClaim (the
		// claim's planAttempt does not match the new status.planAttempt).
		loop.Status.PlanAttempt++
		// F38 (gates P1-1): the re-plan invalidates the prior approval, as
		// the rejection path does (loop_approval.go). Left in place, the
		// stale approved hash opens the Planning -> Implementing gate for a
		// plan the user never saw.
		if conditionTruePresent(loop, coxv1alpha1.PlanApprovedCondition) {
			clearCondition(loop, coxv1alpha1.PlanApprovedCondition)
		}
		if loop.Status.Verify == nil {
			loop.Status.Verify = &coxv1alpha1.VerifyStatus{}
		}
		loop.Status.Verify.LastFailureSummary = verifyFailureSummary(pod, failedCheck, checkExitCode)
		// OS5: the re-plan is a distinct Event with the correct from-phase
		// (Verifying -> Planning, never the stale phase a leftover claim
		// would imply). The failing check + exit code ride in the message.
		r.emitVerifyReplanEvent(loop, from, failedCheck, checkExitCode, nextIter)
		return true, false
	default: // OnVerifyFailImplement (the default)
		// Back to Implementing, iteration+1 — NOT a new verify Job (the Job
		// is created only for a fresh pin at the next advance). The failing
		// check rides into status.progress so the next Implementing run can
		// target it.
		loop.Status.Phase = coxv1alpha1.LoopPhaseImplementing
		loop.Status.DesiredPhase = coxv1alpha1.LoopPhaseImplementing
		// OS5: the iterate is a distinct Event with the correct from-phase
		// (Verifying -> Implementing, never the stale phase a leftover claim
		// would imply). The failing check + exit code ride in the message.
		r.emitVerifyIteratedEvent(loop, from, failedCheck, checkExitCode, nextIter)
		return true, false
	}
}

// The verifyOutcome evidence helpers below were extracted from verifyOutcome
// (F60-lint: the trusted-init walk and the recordCheckResults recording loop
// added branches on top of the F60 canary switch and the check scan, so the
// two functions sat over the gocyclo budget of 30). Each returns the decision
// for its own concern ONLY; verifyOutcome / applyVerifyOutcome call them in
// the same order as before and return what they return.

// trustedInitDecision walks the trusted inits (clone-base, import-agent) —
// the inits that run BEFORE the evidence inits — and returns their decision.
// A terminated non-zero trusted init is an infra failure naming the failed
// container (I65: the checks never ran, so the failure says nothing about
// the agent's work). The import-agent's verifyNotDescendantExit (F43) is
// terminal and distinct: the pinned commit is not a descendant of
// baseCommit, so the pin is the agent's claim and cannot be an honest one.
//
// Returns nil when no decision is reached: no trusted init is in the pod's
// init statuses at all (nothing to walk — the caller continues to the
// tamper gate) or a trusted init is still Running/Waiting (in progress — the
// I49 norm: no decision, requeue, even if the Job condition is already
// Failed: a backoff-retry is in flight for a Never-restart Job the pod is
// still settling; the next poll decides).
func trustedInitDecision(pod *corev1.Pod) *verifyDecision {
	for j := range pod.Status.InitContainerStatuses {
		ics := &pod.Status.InitContainerStatuses[j]
		if ics.Name != verifyCloneBaseInit && ics.Name != verifyImportAgentInit {
			continue
		}
		if ics.State.Terminated != nil && ics.Name == verifyImportAgentInit &&
			ics.State.Terminated.ExitCode == verifyNotDescendantExit {
			return &verifyDecision{outcome: verifyNotDescendant, requeue: false, container: ics.Name, exitCode: ics.State.Terminated.ExitCode}
		}
		if ics.State.Terminated != nil && ics.State.Terminated.ExitCode != 0 {
			return &verifyDecision{outcome: verifyInfraFailed, requeue: false, container: ics.Name, exitCode: ics.State.Terminated.ExitCode}
		}
		if ics.State.Terminated == nil {
			// Still Running or Waiting: the evidence inits cannot have started.
			// No decision, requeue — even if the Job condition is already
			// Failed (a backoff-retry is in flight for a Never-restart Job the
			// pod is still settling; the next poll decides).
			return &verifyDecision{outcome: verifyNoDecision, requeue: true}
		}
	}
	// No trusted-init decision (none present, or all terminated 0): the
	// caller continues to the tamper gate.
	return nil
}

// recordCheckResults records the check exit codes into
// status.verify.lastCheckResults (one per acceptance check, in order). It
// was extracted from applyVerifyOutcome (F60-lint: the nested found/notRun
// branches added complexity). A check that did not run (the pod status lacks
// it) or is still running (not terminated) is recorded as -1 as a sentinel
// (the API server rejects nil).
func recordCheckResults(pod *corev1.Pod, loop *coxv1alpha1.Loop, checkCount int) {
	if pod == nil {
		return
	}
	results := make([]*int32, checkCount)
	for i := range checkCount {
		name := fmt.Sprintf("check-%d", i)
		found := false
		for j := range pod.Status.InitContainerStatuses {
			if pod.Status.InitContainerStatuses[j].Name == name {
				if pod.Status.InitContainerStatuses[j].State.Terminated != nil {
					code := pod.Status.InitContainerStatuses[j].State.Terminated.ExitCode
					results[i] = &code
				} else {
					// The check is still running (not terminated): record -1
					// as a sentinel (the API server rejects nil).
					notRun := int32(-1)
					results[i] = &notRun
				}
				found = true
				break
			}
		}
		if !found {
			// The check did not run: record -1 as a sentinel.
			notRun := int32(-1)
			results[i] = &notRun
		}
	}
	loop.Status.Verify.LastCheckResults = results
}
