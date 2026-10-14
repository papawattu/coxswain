/*
Copyright 2026 papawattu.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// B3 (PLAN-BETA / PLAN.md Phase 4): the plan approval gate. The operator is
// the sole actor on the Loop's status (the runner never writes it); the user
// (via cox approve / cox reject) is the sole actor on the two approval
// conditions (PlanApproved, PlanRejected). This file is the operator's read
// of those conditions + the phase-machine decision they drive:
//
//   - Manual mode (spec.approval.mode == Manual): the operator holds the
//     phase machine at AwaitingApproval after Planning, until the user sets
//     PlanApproved with the SHA-256 of the plan (parsed from the condition's
//     message). A plan change after approval (a fresh Planning run re-writes
//     PLAN.md, the runner detects the mismatch with the approved hash and
//     re-writes the PlanApproved condition as stale) clears the condition and
//     the Loop re-enters the hold.
//   - Rejection: when the user sets PlanRejected, the operator's action
//     depends on spec.approval.onReject: Replan (default) re-enters Planning
//     (the operator writes the reason to the workspace via the phase-init
//     container for the next plan); Fail is terminal (the operator sets the
//     Loop to Failed:PlanRejected).
//   - Auto mode (the default): the gate is inert — the phase machine
//     advances Planning -> Implementing as soon as the runner completes the
//     Planning phase (the S4 behaviour, unchanged).
//
// The gate is a pure function of (loop, claim) that the operator runs on
// every reconcile (level-based, like the other conditions). It returns the
// operator's decision: a phase change (AwaitingApproval / Implementing /
// Failed) or a condition change (clear PlanApproved / clear PlanRejected).
// The caller (Reconcile) applies the decision to the Loop's status and
// persists it (the shared Status().Update at the end of the reconcile).

package controller

import (
	"regexp"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	coxv1alpha1 "github.com/papawattu/coxswain/api/v1alpha1"
)

// approvalMode returns the Loop's effective approval mode (Manual or Auto).
// An empty spec.approval or an empty mode reads as Auto (the CRD default).
func approvalMode(loop *coxv1alpha1.Loop) coxv1alpha1.ApprovalMode {
	if loop.Spec.Approval == nil {
		return coxv1alpha1.ApprovalModeAuto
	}
	if loop.Spec.Approval.Mode == "" {
		return coxv1alpha1.ApprovalModeAuto
	}
	return loop.Spec.Approval.Mode
}

// onRejectAction returns the Loop's effective onReject action (Replan or
// Fail). An empty spec.approval or an empty onReject reads as Replan (the
// CRD default).
func onRejectAction(loop *coxv1alpha1.Loop) coxv1alpha1.OnRejectAction {
	if loop.Spec.Approval == nil {
		return coxv1alpha1.OnRejectReplan
	}
	if loop.Spec.Approval.OnReject == "" {
		return coxv1alpha1.OnRejectReplan
	}
	return loop.Spec.Approval.OnReject
}

// planHashRe parses a 64-char lowercase-hex SHA-256 from a condition message.
// The operator's decision: a missing or malformed hash is NOT an error (the
// user may have set the condition without the hash, or a future version of
// the CLI may set it differently) — the operator treats it as "no valid hash"
// (the approval does not match any plan). The message format the CLI writes
// is "Approved: <64-hex>"; this regex extracts the hash from anywhere in the
// message (a user who set the message by hand with a different format is not
// rejected — the operator just does not recognise the hash).
var planHashRe = regexp.MustCompile(`[0-9a-f]{64}`)

// approvedPlanHash returns the SHA-256 the user approved (parsed from the
// PlanApproved condition's message), or "" when the condition is absent or
// the message carries no valid 64-hex hash. The operator is the sole
// consumer of this value (the gate + the phase-init file writer); the CLI
// is the sole writer (it computes the hash of the plan and sets the
// condition's message to carry it).
func approvedPlanHash(loop *coxv1alpha1.Loop) string {
	for i := range loop.Status.Conditions {
		c := &loop.Status.Conditions[i]
		if c.Type != coxv1alpha1.PlanApprovedCondition {
			continue
		}
		if c.Status != metav1.ConditionTrue {
			return ""
		}
		m := planHashRe.FindString(c.Message)
		if m == "" {
			return ""
		}
		return m
	}
	return ""
}

// rejectedPlanReason returns the rejection reason the user set (parsed from
// the PlanRejected condition's message), or "" when the condition is absent
// or its status is not True. The message format the CLI writes is
// "Rejected: <reason>"; this returns the whole message (the operator does
// not parse the prefix — the message IS the reason, and a user who set the
// condition by hand with a different format is not rejected).
func rejectedPlanReason(loop *coxv1alpha1.Loop) string {
	for i := range loop.Status.Conditions {
		c := &loop.Status.Conditions[i]
		if c.Type != coxv1alpha1.PlanRejectedCondition {
			continue
		}
		if c.Status != metav1.ConditionTrue {
			return ""
		}
		return c.Message
	}
	return ""
}

// setLastRejectionReason makes the rejection reason DURABLE on the Loop's
// status (status.approval.lastRejectionReason) so it survives the PlanRejected
// condition clear. The Replan branch calls it BEFORE clearCondition (the
// condition is cleared on the same reconcile, before the phase-init reads the
// Loop — the kind finding the R1 review called a P1). The phase-init's
// planApprovalFiles reads this field to write .coxswain/plan-feedback (the
// runner's planning-prompt read channel). An empty reason is a no-op (nothing
// to make durable). The operator is the sole writer (ADR-0004).
func setLastRejectionReason(loop *coxv1alpha1.Loop, reason string) {
	if reason == "" {
		return
	}
	if loop.Status.Approval == nil {
		loop.Status.Approval = &coxv1alpha1.ApprovalStatus{}
	}
	loop.Status.Approval.LastRejectionReason = reason
}

// clearLastRejectionReason clears the DURABLE rejection reason
// (status.approval.lastRejectionReason) so a later Planning run (a plan change
// that re-enters the hold, or a new rejection cycle) does NOT read a stale
// reason from a prior rejection. The gate calls it when an approval is
// ACCEPTED (the user approved the plan — the prior rejection is no longer
// relevant; the phase-init's planApprovalFiles then writes an empty
// plan-feedback, so the next plan has no stale rejection to address). An
// absent/empty field is a no-op. The operator is the sole writer (ADR-0004).
func clearLastRejectionReason(loop *coxv1alpha1.Loop) {
	// Nil the whole record (not just the string): the prior rejection is no
	// longer relevant, and planApprovalFiles reads the field as "" when the
	// record is absent (the fallback to the condition applies; a later
	// rejection re-sets the record via setLastRejectionReason).
	loop.Status.Approval = nil
}

// planApprovalFiles returns the (planHash, planFeedback) the phase-init
// container should write to the workspace for THIS phase run. Both are ""
// when there is nothing to write (the phase-init script then REMOVES the
// file — a stale file from a prior phase run must not leak into the next
// plan).
//
// The values are the OPERATOR's view of the two approval conditions:
//   - planHash is approvedPlanHash (the PlanApproved message's hash, "" if
//     absent/stale/malformed). The runner reads it after writing the fresh
//     PLAN.md and compares it to the new plan's hash: a mismatch means the
//     plan changed since the user approved (the runner re-writes the
//     PlanApproved condition as stale, the operator re-enters the hold).
//   - planFeedback is rejectedPlanReason (the PlanRejected message, "" if
//     absent). The runner's planning prompt includes it (the next plan
//     addresses the rejection).
//
// Both are written ONLY when the phase is Planning (the only phase the
// runner reads them in) — the other phases write "" (the phase-init script
// removes the files, so an Implementing phase does not carry a stale
// feedback into the conversation).
func (r *LoopReconciler) planApprovalFiles(loop *coxv1alpha1.Loop) (string, string) {
	if loop.Status.DesiredPhase != coxv1alpha1.LoopPhasePlanning {
		return "", ""
	}
	// The DURABLE reason (status.approval.lastRejectionReason, set by the
	// Replan branch) wins: it survives the PlanRejected clear, so the
	// phase-init writes it into the next planning prompt (the B3 Replan
	// requirement — the reason feeds the next plan, even though the condition
	// was cleared on the same reconcile). The condition is the fallback (the
	// Fail action keeps the condition; a rejection not yet Replan'd).
	reason := rejectedPlanReason(loop)
	if loop.Status.Approval != nil && loop.Status.Approval.LastRejectionReason != "" {
		reason = loop.Status.Approval.LastRejectionReason
	}
	return approvedPlanHash(loop), reason
}

// applyApprovalGate runs the B3 approval decision on the Loop (level-based,
// every reconcile). It mutates the Loop's status (phase / conditions) and
// returns changed (whether the operator wrote anything this pass — the
// caller ORs it into the shared status-changed flag). It is a NO-OP in Auto
// mode (the S4 behaviour is preserved: the phase machine advances
// Planning -> Implementing as soon as the runner completes Planning).
//
// The decision (Manual mode only):
//
//  1. If the Loop is at AwaitingApproval and the user has approved a plan
//     that matches the CURRENT plan (PlanApproved True, the message's hash
//     is present and non-empty), advance to Implementing.
//  2. If the user has rejected the plan (PlanRejected True), apply the
//     onReject action: Replan -> set the phase to Planning (the next
//     reconcile re-runs Planning; the phase-init writes the reason to the
//     workspace for the re-plan), clear PlanRejected (the re-plan starts
//     fresh — the user can reject again on the new plan), and keep
//     AwaitingApproval (the hold is still in force until the new plan is
//     approved); Fail -> set the phase to Failed (terminal, reason
//     PlanRejected).
//  3. If the Loop is at Planning (the runner is re-planning after a
//     rejection) and the PlanApproved condition is still True (a STALE
//     approval from before the rejection — the user has not yet approved
//     the new plan), clear it (the operator's plan-change detection: the
//     approved plan is no longer the one on the disk). The hold re-enters on
//     the next reconcile (the runner's claim advances Planning ->
//     AwaitingApproval).
//
// The gate does NOT write the AwaitingApproval phase itself: that transition
// is the claim-driven advance (the runner's Planning claim -> the
// nextPhase table), and the gate's job is to HOLD the claim from advancing
// past AwaitingApproval (the phaseGate seam, see approvalPhaseGate below).
func (r *LoopReconciler) applyApprovalGate(loop *coxv1alpha1.Loop) bool {
	if approvalMode(loop) != coxv1alpha1.ApprovalModeManual {
		return false // Auto: the S4 behaviour (no gate).
	}
	changed := false
	planRejected := rejectedPlanReason(loop) != ""
	planApproved := approvedPlanHash(loop) != ""
	switch loop.Status.Phase {
	case coxv1alpha1.LoopPhaseAwaitingApproval:
		if planRejected {
			// Rejection: apply the onReject action.
			switch onRejectAction(loop) {
			case coxv1alpha1.OnRejectFail:
				loop.Status.Phase = coxv1alpha1.LoopPhaseFailed
				loop.Status.DesiredPhase = coxv1alpha1.LoopPhaseFailed
				setCondition(loop, coxv1alpha1.PlanRejectedCondition, metav1.ConditionTrue,
					"Rejected", rejectedPlanReason(loop))
				changed = true
			default: // Replan
				// Re-plan: make the reason DURABLE (status.approval.
				// lastRejectionReason) BEFORE clearing the condition. The
				// condition is cleared on this same reconcile (the re-plan starts
				// fresh — the user can reject again on the new plan), so the
				// condition alone would not survive to the phase-init (which reads
				// the Loop AFTER this reconcile). The durable status field does,
				// and planApprovalFiles reads it to write .coxswain/plan-feedback
				// (the runner's planning-prompt read channel) on the next
				// Planning sandbox run. The phase STAYS at AwaitingApproval (the
				// hold is still in force; the runner's next Planning claim, once
				// the user re-approves, advances to Implementing).
				setLastRejectionReason(loop, rejectedPlanReason(loop))
				clearCondition(loop, coxv1alpha1.PlanRejectedCondition)
				changed = true
			}
		} else if planApproved {
			// Approved: advance to Implementing. The phase-init script
			// (written on the next pod recreation) carries the hash to the
			// runner (the plan-change check on the next Planning run, if any).
			// CLEAR the durable rejection reason (the user accepted the plan —
			// the prior rejection is no longer relevant; a later Planning run
			// (a plan change that re-enters the hold, or a new rejection) must
			// not read a stale reason from the prior rejection). The operator is
			// the sole writer of status (ADR-0004).
			clearLastRejectionReason(loop)
			loop.Status.Phase = coxv1alpha1.LoopPhaseImplementing
			loop.Status.DesiredPhase = coxv1alpha1.LoopPhaseImplementing
			changed = true
		}
		// Not approved and not rejected: hold (no change). The sandbox is
		// Suspended (the phase gate, below).
	case coxv1alpha1.LoopPhasePlanning:
		// A stale approval from a prior plan (the user approved a plan, the
		// Loop re-entered Planning (a re-plan after a rejection, or a plan
		// change that cleared the approval and the runner re-wrote PLAN.md),
		// but the user has not yet re-approved the new plan): the gate does
		// NOT clear it on this reconcile. The claim-driven advance (allowed
		// when the condition is present, see approvalPhaseGate) is the happy
		// path: the user's PlanApproved is the signal that the plan on disk
		// is the one to approve, and the runner's Planning claim (consumed by
		// advancePhaseFromClaim, which runs AFTER the gate) then advances
		// Planning -> Implementing. Clearing the condition here would
		// clobber the user's approval before the claim-driven advance sees
		// it. The stale-approval detection is the operator's own read of the
		// phase (a re-plan is in flight: the phase is Planning, the user has
		// not re-approved) + the runner's plan-change signal (the
		// PlanChanged claim, consumed by advancePhaseFromClaim, clears the
		// condition there), NOT a clear on every Planning + PlanApproved.
		// A rejection at Planning (the user rejected the plan before it was
		// approved — the Loop is still planning the re-plan): clear the
		// condition (the re-plan starts fresh). The phase STAYS at Planning
		// (the re-plan is in flight; the runner's next Planning claim, once
		// the user re-approves, advances to Implementing).
		if planRejected {
			setLastRejectionReason(loop, rejectedPlanReason(loop))
			clearCondition(loop, coxv1alpha1.PlanRejectedCondition)
			changed = true
		}
	case coxv1alpha1.LoopPhaseImplementing:
		// The plan was approved (or the gate is Auto); the runner is
		// implementing. A rejection while implementing is the user's decision
		// to stop the Loop (the plan the user approved is no longer wanted):
		// the onReject action applies (Replan -> re-enter Planning with the
		// reason; Fail -> terminal). The phase machine does NOT advance
		// forward from here (the gate holds the claim).
		if planRejected {
			switch onRejectAction(loop) {
			case coxv1alpha1.OnRejectFail:
				loop.Status.Phase = coxv1alpha1.LoopPhaseFailed
				loop.Status.DesiredPhase = coxv1alpha1.LoopPhaseFailed
				changed = true
			default: // Replan
				// Re-plan: make the reason DURABLE (status.approval.
				// lastRejectionReason) before clearing the condition (the same
				// AwaitingApproval rationale — the condition is cleared on this
				// reconcile, so the durable field carries the reason to the
				// phase-init on the next Planning run). The phase STAYS at
				// Implementing (the runner's next Planning claim, once the user
				// re-approves, advances to AwaitingApproval -> Implementing).
				setLastRejectionReason(loop, rejectedPlanReason(loop))
				clearCondition(loop, coxv1alpha1.PlanRejectedCondition)
				changed = true
			}
		}
	}
	return changed
}

// clearCondition removes a condition from the Loop's status (the operator's
// "clear" for a user-set condition: the user set it, the operator removes it
// when its reason no longer applies). It is a no-op when the condition is
// absent. Unlike setCondition (which upserts), clearCondition never creates
// a condition (the user is the sole creator of PlanApproved / PlanRejected;
// the operator only clears them).
func clearCondition(loop *coxv1alpha1.Loop, condType string) {
	for i := range loop.Status.Conditions {
		if loop.Status.Conditions[i].Type == condType {
			loop.Status.Conditions = append(loop.Status.Conditions[:i], loop.Status.Conditions[i+1:]...)
			return
		}
	}
}

// approvalPhaseGate is the B3 phase-gate implementation (the OS8 seam): it
// decides whether the phase machine may advance from current to next. In
// Manual mode, the gate HOLDS the Planning -> AwaitingApproval advance (the
// claim-driven advance, driven by the runner's Planning claim) until the
// user sets PlanApproved. The gate is the operator's sole hold point for the
// approval: the applyApprovalGate function above is the operator's read of
// the conditions (the phase change + the condition clears); this gate is
// the hold on the claim-driven advance (the runner's Planning claim must not
// advance to AwaitingApproval until the user approves — the hold is the
// operator's decision, not the runner's).
//
// The decision:
//   - current=Planning, next=AwaitingApproval (the claim-driven advance):
//     ALLOW only when the user has approved a plan (PlanApproved True, a
//     valid hash in the message). Otherwise HOLD (the operator re-asks on
//     the next reconcile — the sandbox is Suspended while held, so the
//     runner's claim is not re-read).
//   - current=AwaitingApproval, next=Implementing (the gate's own advance,
//     driven by applyApprovalGate when the user approves): ALLOW (the
//     operator already decided the phase change; the gate's job is done).
//   - Everything else: ALLOW (the gate is the approval hold only; the other
//     advances are the S4 behaviour, unchanged).
type approvalPhaseGate struct{}

// Allow implements PhaseGate.
func (approvalPhaseGate) Allow(loop *coxv1alpha1.Loop, current, next coxv1alpha1.LoopPhase) (bool, string) {
	if approvalMode(loop) != coxv1alpha1.ApprovalModeManual {
		return true, "" // Auto: no gate (the S4 behaviour).
	}
	// The Manual-mode hold: the Planning -> Implementing advance (the
	// claim-driven advance, driven by the runner's Planning claim) is held
	// until the user sets PlanApproved. The nextPhase table advances
	// Planning -> Implementing directly (there is no AwaitingApproval phase
	// between them in the table); the gate's hold is on that advance (the
	// operator's decision, not the runner's — ADR-0005).
	if current == coxv1alpha1.LoopPhasePlanning && next == coxv1alpha1.LoopPhaseImplementing {
		if approvedPlanHash(loop) != "" {
			return true, ""
		}
		return false, "awaiting plan approval (set the PlanApproved condition via cox approve)"
	}
	return true, ""
}

// wireApprovalGate sets the reconciler's phaseGate to the B3 approval gate
// (the OS8 seam: the S4 build ships autoApprovePhaseGate as the default;
// B3 replaces it with the approval gate so the Manual-mode hold is
// exercised). It is called from SetupWithManager (the production wiring) and
// from the envtest specs (the test wiring — a spec that does NOT call it
// keeps the auto-approve default, the S4 behaviour).
func (r *LoopReconciler) wireApprovalGate() {
	r.phaseGate = approvalPhaseGate{}
}

// approvalHoldPhase reports whether the operator must hold the sandbox
// Suspended for the B3 approval gate (the Manual-mode AwaitingApproval
// hold). The sandbox is Suspended while the Loop is at AwaitingApproval in
// Manual mode: the runner's one-shot container would otherwise re-run
// Planning on every pod recycle (the claim would be re-read and the gate
// re-asked, churning the pod). Suspending the sandbox stops the churn (the
// sandbox is the only pod the runner runs in; the verify Job is
// independent and is not relevant at AwaitingApproval).
//
// The hold is ONLY in Manual mode: Auto mode never enters AwaitingApproval
// (the S4 behaviour advances Planning -> Implementing directly), so the hold
// is a no-op there.
func (r *LoopReconciler) approvalHoldPhase(loop *coxv1alpha1.Loop) bool {
	return approvalMode(loop) == coxv1alpha1.ApprovalModeManual &&
		loop.Status.Phase == coxv1alpha1.LoopPhaseAwaitingApproval
}
