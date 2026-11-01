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
// the sole actor on the Loop's status (the runner never writes it); since
// D85 (owner decision, R34) the operator is ALSO the sole writer of the two
// approval conditions (PlanApproved, PlanRejected): the user's `cox
// approve` / `cox reject` no longer patch the status subresource (the editor
// role's loops/status grant went back to get-only), they patch the Loop
// object's METADATA with a one-shot annotation (coxswain.io/approve: <plan
// sha256> / coxswain.io/reject: <reason>), and this file consumes the
// annotation — turning it into the condition (the same message format the
// CLI wrote before D85, so approvedPlanHash / rejectedPlanReason keep
// working) and removing it.
//
// The operator does NOT read the plan (no peek pod, no pod-log read): the
// approve annotation's hash is copied VERBATIM into PlanApproved. A stale
// approval (the plan re-planned after the user approved) is caught
// downstream by I93 — the runner compares the current PLAN.md with the
// approved hash at Implementing start and claims PlanMismatch, the operator
// sets PlanMismatch (internal/controller/loop_s4_phase.go) and the approval
// gate re-holds the Loop. This file is the operator's read of the
// conditions + the phase-machine decision they drive:
//
//   - Manual mode (spec.approval.mode == Manual): the operator holds the
//     phase machine at AwaitingApproval after Planning, until the operator
//     sets PlanApproved with the SHA-256 the CLI put in the one-shot
//     approve annotation (parsed back from the condition's message). A plan
//     change after approval (a fresh Planning run re-writes PLAN.md, the
//     runner's I93 plan-hash check claims PlanMismatch) re-holds the Loop.
//   - Rejection: when the operator has set PlanRejected from the user's
//     one-shot reject annotation, the operator's action
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
	"context"
	"regexp"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

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
// consumer of this value (the gate + the phase-init file writer). Since D85
// the operator is also the sole WRITER: it sets the condition from the
// coxswain.io/approve annotation (consumeApprovalAnnotations, below), the
// same message format the CLI wrote before D85 (the CLI now only patches
// the annotation; the editor role's status write is gone).
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
// or its status is not True. The message format the operator writes is
// "Rejected: <reason>" (D85: the operator sets the condition from the
// coxswain.io/reject annotation; before D85 the CLI wrote it); this returns
// the whole message (the operator does not parse the prefix — the message IS
// the reason, and a condition set by hand with a different format is not
// rejected).
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
// The values are the OPERATOR's view of the two approval conditions + the
// B4 verify-failure summary:
//   - planHash is approvedPlanHash (the PlanApproved message's hash, "" if
//     absent/stale/malformed). The runner reads it after writing the fresh
//     PLAN.md and compares it to the new plan's hash: a mismatch means the
//     plan changed since the user approved (the runner re-writes the
//     PlanApproved condition as stale, the operator re-enters the hold).
//   - planFeedback is the operator's most recent feedback to feed into the
//     next planning prompt. B3 sets it from the user's rejection reason
//     (status.approval.lastRejectionReason, durable across the PlanRejected
//     clear). B4 (PLAN-BETA / PLAN.md Phase 6 remainder) ALSO sets it from
//     the verify-failure summary (status.verify.lastFailureSummary, set on a
//     failed verify and cleared when the re-plan is consumed: the plan is
//     approved, the plan is rejected, or the verify passes). When onVerifyFail
//     is Plan, a failed verify sends the Loop back to Planning with the
//     failure summary fed into the next planning prompt, reusing the SAME
//     plan-feedback channel B3 uses (no second channel). The verify-failure
//     summary takes PRECEDENCE over the rejection reason when both are set
//     (a re-plan after a failed verify is the freshest feedback; the
//     rejection reason from a prior plan cycle is stale by definition — the
//     user rejected the prior plan, not the new one). The summary is cleared
//     when the rejection reason is set (the rejection takes over) or when
//     the plan is approved (the re-plan is consumed), so a stale summary
//     never leaks into a later planning prompt. When both are empty, the
//     file is removed (no stale feedback leaks into the next plan).
//
// Both are written ONLY when the phase is Planning (the only phase the
// runner reads them in) — the other phases write "" (the phase-init script
// removes the files, so an Implementing phase does not carry a stale
// feedback into the conversation).
// I93 (R29, kind finding): the approved hash must reach the workspace at the
// START of an Implementing run (the runner's planHashMismatchAtStart reads
// .coxswain/plan-hash and blocks on a mismatch — a post-approval edit to
// PLAN.md must be caught before any agent work). The phase-init re-runs when
// the sandbox recycles for the AwaitingApproval -> Implementing transition
// (DesiredPhase=Implementing), so planApprovalFiles MUST return the approved
// hash for Implementing as well, not only for Planning — otherwise the
// phase-init's `rm -f .coxswain/plan-hash` REMOVES the hash on the Implementing
// transition and the runner's check is a no-op (the edited plan is implemented
// silently, no PlanMismatch condition, no PlanHashMismatch Event). The FEEDBACK
// file stays Planning-only (the planning prompt reads it; an Implementing run
// must not carry a stale rejection/replan feedback into the conversation).
//
// The phase is Planning (the only phase the runner reads the feedback file in)
// for the feedback, and Planning OR Implementing for the approved hash (the
// runner reads the hash in both: after a Planning rewrite to detect a plan
// change, and at the Implementing start to catch a post-approval edit).
func (r *LoopReconciler) planApprovalFiles(loop *coxv1alpha1.Loop) (string, string) {
	switch loop.Status.DesiredPhase {
	case coxv1alpha1.LoopPhasePlanning:
		// B4: the verify-failure summary (status.verify.lastFailureSummary) takes
		// precedence over the rejection reason (a re-plan after a failed verify is
		// the freshest feedback). When the summary is absent (onVerifyFail is not
		// Plan, or the last verify passed), fall back to B3's rejection reason
		// (status.approval.lastRejectionReason, durable across the PlanRejected
		// clear).
		var feedback string
		if loop.Status.Verify != nil && loop.Status.Verify.LastFailureSummary != "" {
			feedback = loop.Status.Verify.LastFailureSummary
		}
		if feedback == "" {
			// B3: the rejection reason (the user's cox reject --reason), durable
			// across the PlanRejected clear.
			reason := rejectedPlanReason(loop)
			if loop.Status.Approval != nil && loop.Status.Approval.LastRejectionReason != "" {
				reason = loop.Status.Approval.LastRejectionReason
			}
			feedback = reason
		}
		return approvedPlanHash(loop), feedback
	case coxv1alpha1.LoopPhaseImplementing:
		// I93 (R29, kind finding): the approved hash is written for the
		// Implementing start (the runner's planHashMismatchAtStart read channel).
		// The feedback file stays empty (the planning prompt's read channel —
		// an Implementing run must not carry a stale rejection/replan feedback
		// into the conversation).
		return approvedPlanHash(loop), ""
	default:
		return "", ""
	}
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
//
// D85 (owner decision, R34): the one-shot approval annotations.
//
// The CLI's `cox approve` / `cox reject` no longer write the approval
// conditions (the editor role's loops/status grant went back to get-only).
// They patch the Loop object's METADATA with a one-shot annotation
// (coxswain.io/approve: <plan sha256> / coxswain.io/reject: <reason>), and
// consumeApprovalAnnotations — called at the top of applyApprovalGate,
// before the decision — turns the annotation into the condition the
// operator writes ITSELF and removes the annotation:
//
//   - approve: set PlanApproved True copying the annotation's hash
//     VERBATIM (message "Approved: <hash>" — the same format
//     approvedPlanHash parses, so the downstream parsing is unchanged) and
//     remove the annotation. The gate's approved branch then releases the
//     hold on this same reconcile. The operator does NOT re-read the plan
//     to check the hash (no peek pod, no pod-log read): a stale hash (a
//     re-plan between the CLI's read and this reconcile) is caught
//     downstream by I93 (the runner's plan-hash check at Implementing start
//     claims PlanMismatch; see loop_i93_planhash_envtest_test.go).
//   - reject: set PlanRejected True (message "Rejected: <reason>") and
//     remove the annotation; the gate's existing reject flow (onReject
//     Replan / Fail) continues on this same reconcile.
//   - BOTH annotations: reject wins; BOTH are removed; an Event says so
//     (the collision is a user error — the two writes raced).
//   - an annotation on a Loop that is NOT held for approval (the phase is
//     not AwaitingApproval): removed with a Normal Event and NO effect
//     (the approval window has closed; a stale annotation must not
//     resurrect a decision).
//
// The operator is now the ONLY writer of PlanApproved and PlanRejected
// (before D85 the user was the sole writer of the two conditions; the
// user's write was the defect: the editor role's loops/status grant
// authorised every status field the operator acts on).
func (r *LoopReconciler) applyApprovalGate(loop *coxv1alpha1.Loop) bool {
	if approvalMode(loop) != coxv1alpha1.ApprovalModeManual {
		return false // Auto: the S4 behaviour (no gate).
	}
	// D85: consume the one-shot approval annotation BEFORE the decision.
	// The consume returns true when it set a condition (the switch below
	// MUST act on it this pass — reject flow / approved release) or when
	// it removed an annotation (the switch then matches no branch — the
	// object is unchanged — and the trailing persist is a no-op). The
	// consume persists its two writes itself (the condition's status patch
	// FIRST, then the annotation's metadata patch) and refreshes loop from
	// a fresh read, so the trailing persist writes cleanly (no 409) and
	// carries only the rest of the reconcile's changes. Do NOT
	// early-return here: the condition the consume just set is read by the
	// switch on this same pass (a consumed reject must re-plan on this same
	// reconcile; a consumed current-hash approve must release the hold on
	// this same reconcile).
	changed := r.consumeApprovalAnnotations(loop)
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
				// Planning sandbox run. MOVE the phase to Planning (the re-plan
				// runs now — the runner's next Planning claim, once the user
				// re-approves, advances to Implementing). The operator is the sole
				// writer of status (ADR-0004).
				// I42b: increment planAttempt so a stale Planning success claim
				// from the prior attempt is rejected by advancePhaseFromClaim (the
				// claim's planAttempt does not match the new status.planAttempt).
				// Also clear the PlanApproved condition: a stale approval from
				// the prior plan must not survive the rejection (the gate's
				// Planning -> Implementing hold checks approvedPlanHash; a stale
				// approval would allow a stale claim to advance to Implementing
				// without the user's re-approval).
				setLastRejectionReason(loop, rejectedPlanReason(loop))
				// B4: clear the failure summary (the rejection reason takes over
				// as the planning feedback — a stale verify-failure summary must
				// not leak into the next planning prompt after a rejection).
				clearLastFailureSummary(loop)
				clearCondition(loop, coxv1alpha1.PlanRejectedCondition)
				// I42b: clear the stale PlanApproved (the rejection invalidates
				// the prior approval; the user must re-approve the new plan).
				clearCondition(loop, coxv1alpha1.PlanApprovedCondition)
				loop.Status.Phase = coxv1alpha1.LoopPhasePlanning
				loop.Status.DesiredPhase = coxv1alpha1.LoopPhasePlanning
				loop.Status.PlanAttempt++
				// Note: lastActivityTime is NOT refreshed here (P1 reviewer fix).
				// Stamping at the reject would make the field move on a user action
				// even if the agent never runs. The refresh happens on the re-plan's
				// claim-driven advance (recordPhaseClaim in loop_s4_phase.go), where
				// a new planAttempt claim signals that the agent actually ran.
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
			// B4: clear the failure summary (the plan is approved — the re-plan
			// is consumed; a stale verify-failure summary must not leak into a
			// later planning prompt).
			clearLastFailureSummary(loop)
			// I93 (R29 P2): the re-approval releases the hold — clear the
			// PlanMismatch surface (the mismatch that caused the re-hold is
			// resolved by the user's approval of the current plan).
			if conditionTruePresent(loop, coxv1alpha1.PlanMismatchCondition) {
				setCondition(loop, coxv1alpha1.PlanMismatchCondition, metav1.ConditionFalse, "Cleared", "the current plan was re-approved")
			}
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
			// B4: clear the failure summary (the rejection reason takes over
			// as the planning feedback).
			clearLastFailureSummary(loop)
			clearCondition(loop, coxv1alpha1.PlanRejectedCondition)
			// F38 (gates P2-1): a rejection invalidates the prior approval and
			// starts a new attempt, as the AwaitingApproval Replan branch does.
			// Without the clear the stale hash opens the gate; without the bump
			// a stale Planning success claim already on the pod matches the
			// current planAttempt and is consumed, absorbing the rejection.
			clearCondition(loop, coxv1alpha1.PlanApprovedCondition)
			loop.Status.PlanAttempt++
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
				// B4: clear the failure summary (the rejection reason takes over
				// as the planning feedback).
				clearLastFailureSummary(loop)
				clearCondition(loop, coxv1alpha1.PlanRejectedCondition)
				changed = true
			}
		}
	}
	return changed
}

// conditionTruePresent reports whether the loop's status carries condType as
// True (any reason). It is the Event-dedup guard: a Warning Event is emitted
// only on the transition into True, not on every reconcile while the
// condition is already True (I93, R29 P2).
func conditionTruePresent(loop *coxv1alpha1.Loop, condType string) bool {
	for i := range loop.Status.Conditions {
		if loop.Status.Conditions[i].Type == condType {
			return loop.Status.Conditions[i].Status == metav1.ConditionTrue
		}
	}
	return false
}

// clearCondition removes a condition from the loop's status (a no-op when
// absent).
func clearCondition(loop *coxv1alpha1.Loop, condType string) {
	for i := range loop.Status.Conditions {
		if loop.Status.Conditions[i].Type == condType {
			loop.Status.Conditions = append(loop.Status.Conditions[:i], loop.Status.Conditions[i+1:]...)
			return
		}
	}
}

// D85 (owner decision, R34): the one-shot approval annotations. The CLI
// (cox approve / cox reject) patches the Loop object's METADATA with one of
// these (a JSON merge patch on the main resource — the editor role's
// existing `patch` on `loops` authorises it); the operator consumes each
// exactly once (consumeApprovalAnnotations) and removes it.
const (
	// approveAnnotationKey is the `cox approve` one-shot annotation; its
	// value is the SHA-256 (lowercase hex) of the plan the user read from
	// the workspace (the same hash the CLI put in the PlanApproved
	// condition's message before D85).
	approveAnnotationKey = "coxswain.io/approve"
	// rejectAnnotationKey is the `cox reject` one-shot annotation; its
	// value is the reason the user passed to --reason.
	rejectAnnotationKey = "coxswain.io/reject"
)

// consumeApprovalAnnotations turns the user's one-shot approval annotations
// (D85) into the conditions the operator writes itself, and removes the
// annotations. It runs at the top of applyApprovalGate (Manual mode),
// BEFORE the gate's decision, so a consumed annotation releases the hold on
// this same reconcile. It returns true when the gate must continue the
// reconcile (set a condition — the switch below reads it this pass — or
// removed an annotation — the metadata patch triggers the requeue), and
// false when nothing was present (the gate runs its normal decision).
//
// The decision:
//
//  1. NOT held for approval (the phase is not AwaitingApproval): any
//     approval annotation is removed with a Normal StaleApprovalAnnotation
//     Event and has NO effect (the approval window has closed; a stale
//     annotation must not resurrect a decision).
//  2. Held for approval:
//     - reject wins: if the reject annotation is present (regardless of the
//     approve annotation), the operator sets PlanRejected True (message
//     "Rejected: <value>" — the same format rejectedPlanReason expects)
//     and removes the reject annotation. When BOTH are present, the
//     approve annotation is also removed and a RejectWins Event says so
//     (the collision is a user error). A leading "Rejected: " in the
//     annotation's value is stripped so the message never carries the
//     prefix twice.
//     - approve (no reject): set PlanApproved True copying the
//     annotation's hash VERBATIM (message "Approved: <hash>") and remove
//     the annotation. The operator does NOT read the plan: a stale hash is
//     I93's job (the runner's plan-hash check at Implementing start claims
//     PlanMismatch and the gate re-holds; see
//     loop_i93_planhash_envtest_test.go).
//
// Ordering (review P1 on #377): the CONDITION is persisted FIRST (a status
// merge patch from a fresh read, persistConsumedApprovalCondition below) and
// the annotation is removed only AFTER that write succeeds (a metadata
// merge patch from a fresh read). A failed status write must NOT drop the
// user's decision — the annotation stays on the object, and the next
// reconcile re-consumes it (idempotent: same hash, same condition —
// setCondition is a no-op when the condition is already set with the same
// reason+message+status). After both writes are durable the in-memory loop
// is refreshed from a fresh read (its resourceVersion is two writes stale,
// and the pass's trailing status write must not overwrite a concurrent
// status change with the stale version); a refresh failure aborts the pass
// (the requeue re-consumes from a fresh state — the consumed decision is
// already durable).
func (r *LoopReconciler) consumeApprovalAnnotations(loop *coxv1alpha1.Loop) bool {
	approvePresent := loop.Annotations[approveAnnotationKey] != ""
	rejectPresent := loop.Annotations[rejectAnnotationKey] != ""
	if !approvePresent && !rejectPresent {
		return false
	}
	// The Loop must be held for approval (the approval window is open).
	// Out-of-hold: remove the annotations with a Normal Event and no effect.
	if loop.Status.Phase != coxv1alpha1.LoopPhaseAwaitingApproval {
		// Out-of-hold: nothing to persist (no condition); the removal is
		// safe on its own (a failed removal requeues; the stale annotation
		// is re-removed on the next reconcile).
		if r.Recorder != nil {
			r.Recorder.Eventf(loop, corev1.EventTypeNormal, "StaleApprovalAnnotation",
				"approval annotation outside the approval hold was removed (no effect)")
		}
		if err := r.removeApprovalAnnotations(loop, approvePresent, rejectPresent); err != nil {
			logf.FromContext(context.Background()).Error(err, "remove the stale approval annotation")
			return false
		}
		return true
	}
	// Review P1: persist the condition BEFORE removing the annotation, so a
	// failed status write leaves the user's decision on the object (the
	// next reconcile re-consumes the still-present annotation — idempotent:
	// same hash, same condition).
	if rejectPresent {
		// Reject wins (the user's decision to stop the plan): set
		// PlanRejected from the annotation's value (the operator is the
		// sole writer — before D85 the CLI wrote this condition; the
		// message format is unchanged, so rejectedPlanReason / the
		// gate's reject flow keep working).
		reason := strings.TrimPrefix(loop.Annotations[rejectAnnotationKey], "Rejected: ")
		setCondition(loop, coxv1alpha1.PlanRejectedCondition, metav1.ConditionTrue, "Rejected", "Rejected: "+reason)
		if approvePresent {
			// BOTH present: reject wins; the approve annotation is dropped
			// (no PlanApproved — the approve did not happen), and the Event
			// names the collision.
			if r.Recorder != nil {
				r.Recorder.Eventf(loop, corev1.EventTypeWarning, "RejectWins",
					"both approve and reject annotations were present; reject wins (both were removed)")
			}
		}
	} else if approvePresent {
		// Approve (no reject): copy the annotation's hash VERBATIM into
		// PlanApproved (the same message format the CLI wrote before D85 —
		// "Approved: <hash>" — so approvedPlanHash keeps parsing it). The
		// operator does NOT read the plan (no peek pod, no pod-log read):
		// a stale hash is caught downstream by I93 (the runner's plan-hash
		// check at Implementing start claims PlanMismatch and the gate
		// re-holds; see loop_i93_planhash_envtest_test.go).
		setCondition(loop, coxv1alpha1.PlanApprovedCondition, metav1.ConditionTrue,
			"Approved", "Approved: "+loop.Annotations[approveAnnotationKey])
	}
	if err := r.persistConsumedApprovalCondition(loop); err != nil {
		// The condition is not durable — do NOT remove the annotation, so the
		// user's decision survives on the object for the next reconcile.
		return false
	}
	if err := r.removeApprovalAnnotations(loop, approvePresent, rejectPresent); err != nil {
		logf.FromContext(context.Background()).Error(err, "remove the consumed approval annotation")
		return false
	}
	// removeApprovalAnnotations refreshed loop from a fresh read (its own
	// final step — the metadata patch bumped the resourceVersion, and the
	// trailing status write must not overwrite a concurrent status change
	// with the stale version). Both writes are durable; the consume's job is
	// done.
	return true
}

// persistConsumedApprovalCondition writes the condition the consume just set
// (PlanApproved or PlanRejected) to the status subresource as a merge patch
// from a FRESH read, before the annotation is removed (review P1: a failed
// write must not drop the user's decision). The fresh read carries the
// annotation (irrelevant to a status patch), and the merge patch touches
// only status, so the write never races the metadata write that follows.
func (r *LoopReconciler) persistConsumedApprovalCondition(loop *coxv1alpha1.Loop) error {
	var cond *metav1.Condition
	for i := range loop.Status.Conditions {
		if loop.Status.Conditions[i].Type == coxv1alpha1.PlanApprovedCondition ||
			loop.Status.Conditions[i].Type == coxv1alpha1.PlanRejectedCondition {
			c := &loop.Status.Conditions[i]
			if c.Status == metav1.ConditionTrue {
				cond = c
			}
			break
		}
	}
	if cond == nil {
		return nil // nothing to persist (a defensive path — the caller sets one)
	}
	fresh := &coxv1alpha1.Loop{}
	if err := r.Get(ctxFor(loop), client.ObjectKeyFromObject(loop), fresh); err != nil {
		return client.IgnoreNotFound(err)
	}
	freshCopy := fresh.DeepCopy()
	setCondition(freshCopy, cond.Type, cond.Status, cond.Reason, cond.Message)
	if err := r.Status().Patch(ctxFor(loop), freshCopy, client.MergeFrom(fresh)); err != nil {
		logf.FromContext(context.Background()).Error(err, "persist the consumed approval condition")
		return err
	}
	return nil
}

// removeApprovalAnnotations patches the Loop's METADATA to drop the given
// approval annotations (D85, the I52/removeResumeAnnotation pattern: a merge
// patch from a FRESH read — the consumed condition is persisted FIRST by the
// caller (persistConsumedApprovalCondition), and only then is the annotation
// removed; a failed removal leaves the annotation in place and the next
// reconcile re-consumes it (idempotent: same hash, same condition)).
func (r *LoopReconciler) removeApprovalAnnotations(loop *coxv1alpha1.Loop, approve, reject bool) error {
	if !approve && !reject {
		return nil
	}
	fresh := &coxv1alpha1.Loop{}
	if err := r.Get(ctxFor(loop), client.ObjectKeyFromObject(loop), fresh); err != nil {
		return client.IgnoreNotFound(err)
	}
	updated := fresh.DeepCopy()
	if updated.Annotations == nil {
		return nil // nothing to remove
	}
	dropped := false
	if approve {
		if _, ok := updated.Annotations[approveAnnotationKey]; ok {
			delete(updated.Annotations, approveAnnotationKey)
			dropped = true
		}
	}
	if reject {
		if _, ok := updated.Annotations[rejectAnnotationKey]; ok {
			delete(updated.Annotations, rejectAnnotationKey)
			dropped = true
		}
	}
	if !dropped {
		return nil
	}
	if err := r.Patch(ctxFor(loop), updated, client.MergeFrom(fresh)); err != nil {
		return err
	}
	// The patch bumped the Loop's resourceVersion; the in-memory loop is now
	// stale. Refresh it from a fresh read (the patch is the durable write —
	// a refresh failure aborts the pass and the requeue re-runs from a fresh
	// state, the annotation already gone). The fresh read carries the
	// authoritative post-patch state (the annotation is gone), so the
	// in-memory loop's resourceVersion now matches the fresh object and the
	// pass's trailing status write (the shared Status().Update) does not 409
	// on the stale version.
	if err := r.Get(ctxFor(loop), client.ObjectKeyFromObject(loop), loop); err != nil {
		logf.FromContext(context.Background()).Error(err, "refresh the Loop after the annotation removal")
		return err
	}
	return nil
}

// ctxFor returns the reconcile context the caller is running in; the
// approval-gate helpers are called from Reconcile with the reconcile ctx in
// scope, but the gate signature predates ctx threading (applyApprovalGate
// takes only the loop) — context.Background() is a safe default for the
// annotation PATCH (the API call does not need the cancel-aware ctx to be
// correct; a cancel during the patch would leave the requeue to retry).
func ctxFor(_ *coxv1alpha1.Loop) context.Context {
	return context.Background()
}

// approvalPhaseGate is the B3 phase-gate implementation (the OS8 seam): it
// decides whether the phase machine may advance from current to next. In
// Manual mode, the gate HOLDS the Planning -> AwaitingApproval advance (the
// claim-driven advance, driven by the runner's Planning claim) until the
// user's approve annotation is consumed (PlanApproved True).
// The gate is the operator's sole hold point for the
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
	// until the user's approve annotation is consumed (PlanApproved True).
	// The nextPhase table advances
	// Planning -> Implementing directly (there is no AwaitingApproval phase
	// between them in the table); the gate's hold is on that advance (the
	// operator's decision, not the runner's — ADR-0005).
	if current == coxv1alpha1.LoopPhasePlanning && next == coxv1alpha1.LoopPhaseImplementing {
		if approvedPlanHash(loop) != "" {
			return true, ""
		}
		return false, "awaiting plan approval (run cox approve)"
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
