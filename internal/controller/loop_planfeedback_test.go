package controller

import (
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	coxv1alpha1 "github.com/papawattu/coxswain/api/v1alpha1"
)

// B3 (PLAN-BETA / PLAN.md Phase 4): the operator's side of the "onReject:
// Replan feeds the reason into the next plan prompt" handoff. The operator is
// the SOLE WRITER of the .coxswain/plan-feedback file (the phase-init
// container, from the PlanRejected condition's message); the runner is the
// SOLE READER (the planning prompt). These execution tests prove the operator
// writes the reason to the file (planApprovalFiles + the phaseInitScript the
// phase-init container runs). AGENTS.md: "Every gate spec must FAIL when the
// gate is disabled" — mutation (b) (write an empty plan-feedback) must make
// these FAIL.
//
// The phase-init container is the execution seam: the phaseInitScript is the
// exact sh -c body it runs (the operator writes the file, the runner reads it
// from the PVC). The tests run the operator's decision (planApprovalFiles) +
// the script it generates, asserting the file write carries the reason.

const b3ReplanReason = "the plan misses the edge case: it does not handle empty input"

// b3LoopWithDurableRejection builds a Manual-mode Loop at Planning with the
// rejection reason on the DURABLE status field (status.approval.
// lastRejectionReason) — the state after the operator's Replan branch has
// run (the PlanRejected condition is CLEARED, the reason is on the durable
// field). This is what planApprovalFiles reads to write .coxswain/plan-feedback
// (the B3 Replan requirement: the reason feeds the next plan even though the
// condition was cleared on the same reconcile).
func b3LoopWithDurableRejection(reason string) *coxv1alpha1.Loop {
	return &coxv1alpha1.Loop{
		ObjectMeta: metav1.ObjectMeta{Name: "b3", Namespace: "b3"},
		Spec:       coxv1alpha1.LoopSpec{Approval: &coxv1alpha1.Approval{Mode: coxv1alpha1.ApprovalModeManual, OnReject: coxv1alpha1.OnRejectReplan}},
		Status: coxv1alpha1.LoopStatus{
			DesiredPhase: coxv1alpha1.LoopPhasePlanning,
			Approval:     &coxv1alpha1.ApprovalStatus{LastRejectionReason: "Rejected: " + reason},
		},
	}
}

// b3LoopWithRejectedCondition builds a Manual-mode Loop at Planning with a
// PlanRejected condition (message "Rejected: <reason>") and NO durable field —
// the state BEFORE the operator's Replan branch runs (the condition is still
// present, the reason not yet made durable). The fallback path of
// planApprovalFiles (a rejection not yet Replan'd, or the Fail action where the
// condition persists).
func b3LoopWithRejectedCondition(reason string) *coxv1alpha1.Loop {
	return &coxv1alpha1.Loop{
		ObjectMeta: metav1.ObjectMeta{Name: "b3", Namespace: "b3"},
		Spec:       coxv1alpha1.LoopSpec{Approval: &coxv1alpha1.Approval{Mode: coxv1alpha1.ApprovalModeManual, OnReject: coxv1alpha1.OnRejectReplan}},
		Status: coxv1alpha1.LoopStatus{
			DesiredPhase: coxv1alpha1.LoopPhasePlanning,
			Conditions: []metav1.Condition{{
				Type:    coxv1alpha1.PlanRejectedCondition,
				Status:  metav1.ConditionTrue,
				Reason:  "Rejected",
				Message: "Rejected: " + reason,
			}},
		},
	}
}

// On Replan, the operator's phase-init container writes the rejection reason
// to .coxswain/plan-feedback (the runner reads it into the planning prompt).
// The reason is read from the DURABLE status.approval.lastRejectionReason (the
// B3 Replan requirement — it survives the PlanRejected clear).
func TestPhaseInitScriptWritesRejectionReason(t *testing.T) {
	r := &LoopReconciler{CNIProber: testCNIProber()}
	loop := b3LoopWithDurableRejection(b3ReplanReason)
	planHash, planFeedback := r.planApprovalFiles(loop)
	// The operator's decision: the feedback file carries the full rejection
	// message (the reason the user set via cox reject --reason), read from the
	// DURABLE field (the condition is cleared by the Replan branch — the
	// durable field is what survives to the phase-init).
	if planFeedback != "Rejected: "+b3ReplanReason {
		t.Fatalf("planApprovalFiles feedback = %q, want the rejection reason \"Rejected: %s\" (read from the durable status field)", planFeedback, b3ReplanReason)
	}
	// The durable field must win over the (cleared) condition. A loop with the
	// condition present AND the durable field set reads the durable field.
	loopWithBoth := b3LoopWithRejectedCondition(b3ReplanReason)
	loopWithBoth.Status.Approval = &coxv1alpha1.ApprovalStatus{LastRejectionReason: "Rejected: " + b3ReplanReason}
	if _, fb := r.planApprovalFiles(loopWithBoth); fb != "Rejected: "+b3ReplanReason {
		t.Fatalf("planApprovalFiles with both condition + durable field = %q, want the durable value", fb)
	}
	// The fallback: a rejection NOT yet Replan'd (the condition present, the
	// durable field absent) still reads the condition (the phase-init writes it
	// before the Replan branch clears the condition + makes it durable).
	if _, fb := r.planApprovalFiles(b3LoopWithRejectedCondition(b3ReplanReason)); fb != "Rejected: "+b3ReplanReason {
		t.Fatalf("planApprovalFiles fallback (condition present, no durable field) = %q, want the condition's reason", fb)
	}
	// The phase-init script (the phase-init container's sh -c body) writes the
	// file with that content. The script is the execution seam — the operator
	// is the sole writer.
	script := phaseInitScript(loop.Status.DesiredPhase, loop.Status.Iteration, loop.Status.PlanAttempt, planHash, planFeedback)
	if !strings.Contains(script, coxPlanFeedbackFile) {
		t.Fatalf("the phase-init script does not reference %s;\nscript:\n%s", coxPlanFeedbackFile, script)
	}
	// The reason must appear in the script (shell-quoted, written to the file).
	if !strings.Contains(script, b3ReplanReason) {
		t.Fatalf("the phase-init script does not carry the rejection reason %q;\nscript:\n%s", b3ReplanReason, script)
	}
	// It writes (printf >), not removes (rm -f), the feedback file.
	if strings.Contains(script, "rm -f \"+\"${DEST}/.coxswain/"+coxPlanFeedbackFile) || strings.Contains(script, `rm -f "${DEST}/.coxswain/`+coxPlanFeedbackFile+`"`) {
		t.Fatalf("the phase-init script REMOVES %s instead of writing it (the Replan action must write the reason);\nscript:\n%s", coxPlanFeedbackFile, script)
	}
	if !strings.Contains(script, `printf '%s'`) {
		t.Fatalf("the phase-init script does not printf the file contents;\nscript:\n%s", script)
	}
}

// When there is no rejection (no PlanRejected condition), the phase-init
// container REMOVES .coxswain/plan-feedback (a stale file from a prior run
// must not leak into the next plan).
func TestPhaseInitScriptRemovesFeedbackWhenNoRejection(t *testing.T) {
	r := &LoopReconciler{CNIProber: testCNIProber()}
	loop := &coxv1alpha1.Loop{
		ObjectMeta: metav1.ObjectMeta{Name: "b3", Namespace: "b3"},
		Spec:       coxv1alpha1.LoopSpec{Approval: &coxv1alpha1.Approval{Mode: coxv1alpha1.ApprovalModeManual}},
		Status:     coxv1alpha1.LoopStatus{DesiredPhase: coxv1alpha1.LoopPhasePlanning},
	}
	_, planFeedback := r.planApprovalFiles(loop)
	if planFeedback != "" {
		t.Fatalf("planApprovalFiles feedback = %q, want \"\" (no rejection)", planFeedback)
	}
	script := phaseInitScript(loop.Status.DesiredPhase, loop.Status.Iteration, loop.Status.PlanAttempt, "", planFeedback)
	if !strings.Contains(script, `rm -f "${DEST}/.coxswain/`+coxPlanFeedbackFile+`"`) {
		t.Fatalf("the phase-init script does not remove %s when there is no rejection;\nscript:\n%s", coxPlanFeedbackFile, script)
	}
}

// A rejection while the Loop is NOT at Planning (e.g. Implementing) does not
// write the feedback file (the runner only reads it in Planning; a stale file
// must not leak into a non-Planning phase).
func TestPhaseInitScriptNoFeedbackWhenNotPlanning(t *testing.T) {
	r := &LoopReconciler{CNIProber: testCNIProber()}
	loop := b3LoopWithRejectedCondition(b3ReplanReason)
	loop.Status.DesiredPhase = coxv1alpha1.LoopPhaseImplementing
	_, planFeedback := r.planApprovalFiles(loop)
	if planFeedback != "" {
		t.Fatalf("planApprovalFiles feedback at Implementing = %q, want \"\" (the runner reads the feedback only in Planning)", planFeedback)
	}
	script := phaseInitScript(loop.Status.DesiredPhase, loop.Status.Iteration, loop.Status.PlanAttempt, "", planFeedback)
	if !strings.Contains(script, `rm -f "${DEST}/.coxswain/`+coxPlanFeedbackFile+`"`) {
		t.Fatalf("the phase-init script does not remove %s at a non-Planning phase;\nscript:\n%s", coxPlanFeedbackFile, script)
	}
}
