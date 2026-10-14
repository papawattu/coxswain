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

// loopWithRejectedCondition builds a Manual-mode Loop at Planning with a
// PlanRejected condition (message "Rejected: <reason>") — the state the
// operator's planApprovalFiles reads when it decides what the phase-init
// container should write.
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
func TestPhaseInitScriptWritesRejectionReason(t *testing.T) {
	r := &LoopReconciler{}
	loop := b3LoopWithRejectedCondition(b3ReplanReason)
	planHash, planFeedback := r.planApprovalFiles(loop)
	// The operator's decision: the feedback file carries the full rejection
	// message (the reason the user set via cox reject --reason).
	if planFeedback != "Rejected: "+b3ReplanReason {
		t.Fatalf("planApprovalFiles feedback = %q, want the rejection reason \"Rejected: %s\"", planFeedback, b3ReplanReason)
	}
	// The phase-init script (the phase-init container's sh -c body) writes the
	// file with that content. The script is the execution seam — the operator
	// is the sole writer.
	script := phaseInitScript(loop.Status.DesiredPhase, loop.Status.Iteration, planHash, planFeedback)
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
	r := &LoopReconciler{}
	loop := &coxv1alpha1.Loop{
		ObjectMeta: metav1.ObjectMeta{Name: "b3", Namespace: "b3"},
		Spec:       coxv1alpha1.LoopSpec{Approval: &coxv1alpha1.Approval{Mode: coxv1alpha1.ApprovalModeManual}},
		Status:     coxv1alpha1.LoopStatus{DesiredPhase: coxv1alpha1.LoopPhasePlanning},
	}
	_, planFeedback := r.planApprovalFiles(loop)
	if planFeedback != "" {
		t.Fatalf("planApprovalFiles feedback = %q, want \"\" (no rejection)", planFeedback)
	}
	script := phaseInitScript(loop.Status.DesiredPhase, loop.Status.Iteration, "", planFeedback)
	if !strings.Contains(script, `rm -f "${DEST}/.coxswain/`+coxPlanFeedbackFile+`"`) {
		t.Fatalf("the phase-init script does not remove %s when there is no rejection;\nscript:\n%s", coxPlanFeedbackFile, script)
	}
}

// A rejection while the Loop is NOT at Planning (e.g. Implementing) does not
// write the feedback file (the runner only reads it in Planning; a stale file
// must not leak into a non-Planning phase).
func TestPhaseInitScriptNoFeedbackWhenNotPlanning(t *testing.T) {
	r := &LoopReconciler{}
	loop := b3LoopWithRejectedCondition(b3ReplanReason)
	loop.Status.DesiredPhase = coxv1alpha1.LoopPhaseImplementing
	_, planFeedback := r.planApprovalFiles(loop)
	if planFeedback != "" {
		t.Fatalf("planApprovalFiles feedback at Implementing = %q, want \"\" (the runner reads the feedback only in Planning)", planFeedback)
	}
	script := phaseInitScript(loop.Status.DesiredPhase, loop.Status.Iteration, "", planFeedback)
	if !strings.Contains(script, `rm -f "${DEST}/.coxswain/`+coxPlanFeedbackFile+`"`) {
		t.Fatalf("the phase-init script does not remove %s at a non-Planning phase;\nscript:\n%s", coxPlanFeedbackFile, script)
	}
}
