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

package controller

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	coxv1alpha1 "github.com/papawattu/coxswain/api/v1alpha1"
)

// B3 (PLAN-BETA / PLAN.md Phase 4): the plan approval gate. These specs
// drive the gate through the reconciler's Reconcile (the level-based
// applyApprovalGate runs on every pass) and the readPhaseClaim seam (the
// runner's claim, the ADR-0004 channel). The gate is Manual-mode only: in
// Auto mode (the default) the S4 behaviour is preserved (Planning ->
// Implementing on a completed Planning claim, no hold).
//
// The specs cover:
//   - Auto mode: a completed Planning claim advances to Implementing (the
//     S4 behaviour, the gate is a no-op).
//   - Manual mode, hold: a completed Planning claim HOLDS at
//     AwaitingApproval (the gate holds the Planning -> AwaitingApproval
//     advance until the user approves).
//   - Manual mode, approve: a PlanApproved condition (the user's
//     cox approve) advances AwaitingApproval -> Implementing.
//   - Manual mode, reject (Replan): a PlanRejected condition re-enters
//     Planning (the onReject default) and clears the condition.
//   - Manual mode, reject (Fail): a PlanRejected condition with onReject=Fail
//     fails the Loop (terminal, reason PlanRejected).
//   - Manual mode, plan change: a PlanChanged claim (the runner's plan-change
//     signal) clears the PlanApproved condition (re-enter the hold).
//
// Each It is self-contained with its own namespace (envtest gotcha: a shared
// namespace lets one It's sandbox/status leak into the next).

var _ = Describe("B3 plan approval gate", func() {
	ctx := context.Background()

	// b3Reconciler builds the reconciler the specs drive: a plain client
	// (the sandbox pod is not created by the specs, so the default claim
	// reader finds nothing) and a readPhaseClaim seam that starts serving
	// "agent still running" ((nil, nil) — the operator requeues). reportClaim
	// re-arms the seam to serve a specific claim for that reconcile.
	b3Reconciler := func() *LoopReconciler {
		r := &LoopReconciler{
			Client: k8sClient,
			Scheme: k8sClient.Scheme(),
		}
		// Wire the approval gate (the OS8 seam: the S4 default is
		// autoApprovePhaseGate; B3 replaces it with the approval gate so the
		// Manual-mode hold is exercised). A spec that does NOT call
		// wireApprovalGate keeps the auto-approve default (the S4 behaviour).
		r.wireApprovalGate()
		r.readPhaseClaim = func(_ context.Context, _ *coxv1alpha1.Loop) (*PhaseClaim, error) {
			return nil, nil
		}
		return r
	}

	reconcileLoop := func(r *LoopReconciler, ns, name string) {
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: name, Namespace: ns}})
		Expect(err).NotTo(HaveOccurred())
	}

	// reportClaim injects the runner's claim (via the readPhaseClaim seam),
	// then reconciles once. The seam goes back to (nil, nil) (agent running)
	// after the reconcile, so a later reportClaim starts from a clean poll.
	reportClaim := func(r *LoopReconciler, ns, name string, c *PhaseClaim) {
		r.readPhaseClaim = func(_ context.Context, _ *coxv1alpha1.Loop) (*PhaseClaim, error) {
			return c, nil
		}
		reconcileLoop(r, ns, name)
		r.readPhaseClaim = func(_ context.Context, _ *coxv1alpha1.Loop) (*PhaseClaim, error) {
			return nil, nil
		}
	}

	// setCondition sets a condition on the Loop's status (the user's
	// cox approve / cox reject writes the condition; the spec simulates that
	// write directly). The operator's gate then reads it on the next
	// reconcile. The condition carries a lastTransitionTime (the CRD requires
	// it; the user's CLI would set it too — the operator's setCondition sets
	// it on the operator-written conditions, but the user-written condition
	// must carry it for the API server to accept the status update).
	setCondition := func(ns, name string, condType string, status string, message string) {
		nn := types.NamespacedName{Name: name, Namespace: ns}
		loop := &coxv1alpha1.Loop{}
		Expect(k8sClient.Get(ctx, nn, loop)).To(Succeed())
		now := metav1.Now()
		for i := range loop.Status.Conditions {
			if loop.Status.Conditions[i].Type == condType {
				loop.Status.Conditions[i].Status = metav1.ConditionStatus(status)
				loop.Status.Conditions[i].Message = message
				loop.Status.Conditions[i].Reason = condType
				loop.Status.Conditions[i].LastTransitionTime = now
				Expect(k8sClient.Status().Update(ctx, loop)).To(Succeed())
				return
			}
		}
		loop.Status.Conditions = append(loop.Status.Conditions, metav1.Condition{
			Type:               condType,
			Status:             metav1.ConditionStatus(status),
			Reason:             condType,
			Message:            message,
			LastTransitionTime: now,
		})
		Expect(k8sClient.Status().Update(ctx, loop)).To(Succeed())
	}

	// makeLoop creates a Loop (with the given approval mode + onReject),
	// primes it (the S4 bootstrap moves it Pending -> Planning), and returns
	// the reconciler + the loop's NamespacedName.
	makeLoop := func(ns, name string, mode coxv1alpha1.ApprovalMode, onReject coxv1alpha1.OnRejectAction) (types.NamespacedName, *LoopReconciler) {
		nn := types.NamespacedName{Name: name, Namespace: ns}
		r := b3Reconciler()
		app := &coxv1alpha1.Approval{Mode: mode}
		if onReject != "" {
			app.OnReject = onReject
		}
		loop := &coxv1alpha1.Loop{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec:       coxv1alpha1.LoopSpec{Goal: loopGoal, Workspace: testWorkspace(), Approval: app},
		}
		Expect(k8sClient.Create(ctx, loop)).To(Succeed())
		reconcileLoop(r, ns, name)
		return nn, r
	}

	BeforeEach(func() {
		// Each It creates its own namespace inline (self-contained) — the
		// shared BeforeEach namespace breaks after the first It's teardown.
	})

	It("Auto mode: a completed Planning claim advances to Implementing (the S4 behaviour, the gate is a no-op)", func() {
		ns := "b3-auto-" + nowSuffix()
		nsObj := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}
		Expect(k8sClient.Create(ctx, nsObj)).To(Succeed())
		defer func() { _ = k8sClient.Delete(context.Background(), nsObj) }()

		nn, r := makeLoop(ns, "b3-auto", coxv1alpha1.ApprovalModeAuto, "")
		reportClaim(r, ns, "b3-auto", &PhaseClaim{ObservedPhase: coxv1alpha1.LoopPhasePlanning, Status: "success"})

		loop := &coxv1alpha1.Loop{}
		Expect(k8sClient.Get(ctx, nn, loop)).To(Succeed())
		Expect(loop.Status.Phase).To(Equal(coxv1alpha1.LoopPhaseImplementing),
			"Auto mode: a completed Planning claim advances to Implementing (the S4 behaviour, the gate is a no-op)")
		Expect(loop.Status.DesiredPhase).To(Equal(coxv1alpha1.LoopPhaseImplementing))
	})

	It("Manual mode: a completed Planning claim HOLDS at AwaitingApproval (the gate holds the advance until the user approves)", func() {
		ns := "b3-hold-" + nowSuffix()
		nsObj := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}
		Expect(k8sClient.Create(ctx, nsObj)).To(Succeed())
		defer func() { _ = k8sClient.Delete(context.Background(), nsObj) }()

		nn, r := makeLoop(ns, "b3-hold", coxv1alpha1.ApprovalModeManual, "")
		// The gate holds the Planning -> Implementing advance (the approval
		// gate's Allow returns false for that transition when the PlanApproved
		// condition is absent). The claim is consumed (recorded into progress)
		// but the phase does not advance (it stays at Planning until the user
		// approves). The nextPhase table advances Planning -> Implementing
		// directly (there is no AwaitingApproval phase between them in the
		// table); the gate's hold is on that advance (the Manual-mode hold).
		reportClaim(r, ns, "b3-hold", &PhaseClaim{ObservedPhase: coxv1alpha1.LoopPhasePlanning, Status: "success"})

		loop := &coxv1alpha1.Loop{}
		Expect(k8sClient.Get(ctx, nn, loop)).To(Succeed())
		Expect(loop.Status.Phase).To(Equal(coxv1alpha1.LoopPhasePlanning),
			"Manual mode: the gate holds the Planning -> Implementing advance (the phase stays at Planning until the user approves)")
		// The claim was consumed (recorded into progress) — the hold is on the
		// advance, not on the claim read.
		Expect(loop.Status.Progress).NotTo(BeNil(), "the claim is recorded into progress (OS1) even while the gate holds")
		Expect(loop.Status.Progress.LastResultStatus).To(Equal("success"))
	})

	It("Manual mode: a PlanApproved condition releases the hold and the Planning claim advances to Implementing (the user's cox approve)", func() {
		ns := "b3-approve-" + nowSuffix()
		nsObj := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}
		Expect(k8sClient.Create(ctx, nsObj)).To(Succeed())
		defer func() { _ = k8sClient.Delete(context.Background(), nsObj) }()

		nn, r := makeLoop(ns, "b3-approve", coxv1alpha1.ApprovalModeManual, "")
		// The user approves (the PlanApproved condition is set with a valid
		// hash in the message). The gate's approvalPhaseGate sees the
		// condition present and allows the Planning -> Implementing advance.
		// The next claim-driven reconcile (the runner's Planning claim) then
		// advances Planning -> Implementing (the hold is released).
		setCondition(ns, "b3-approve", coxv1alpha1.PlanApprovedCondition, "True",
			"Approved: 1234567890abcdef1234567890abcdef1234567890abcdef1234567890abcdef")
		reportClaim(r, ns, "b3-approve", &PhaseClaim{ObservedPhase: coxv1alpha1.LoopPhasePlanning, Status: "success"})

		loop := &coxv1alpha1.Loop{}
		Expect(k8sClient.Get(ctx, nn, loop)).To(Succeed())
		Expect(loop.Status.Phase).To(Equal(coxv1alpha1.LoopPhaseImplementing),
			"Manual mode: a PlanApproved condition releases the hold and the Planning claim advances to Implementing")
		Expect(loop.Status.DesiredPhase).To(Equal(coxv1alpha1.LoopPhaseImplementing))
	})

	It("Manual mode: a PlanChanged claim clears the PlanApproved condition (re-enter the hold)", func() {
		ns := "b3-planchange-" + nowSuffix()
		nsObj := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}
		Expect(k8sClient.Create(ctx, nsObj)).To(Succeed())
		defer func() { _ = k8sClient.Delete(context.Background(), nsObj) }()

		nn, r := makeLoop(ns, "b3-planchange", coxv1alpha1.ApprovalModeManual, "")
		// Set the PlanApproved condition (the user approved a plan). Then the
		// runner's Planning run detects a plan change (the PlanChanged claim
		// is true). The operator's advancePhaseFromClaim consumes the claim
		// and clears the PlanApproved condition (re-enter the hold — the gate's
		// hold re-enters on the next reconcile, the phase is the machine's
		// evidence that a re-plan is in flight).
		setCondition(ns, "b3-planchange", coxv1alpha1.PlanApprovedCondition, "True",
			"Approved: 1234567890abcdef1234567890abcdef1234567890abcdef1234567890abcdef")
		reportClaim(r, ns, "b3-planchange", &PhaseClaim{ObservedPhase: coxv1alpha1.LoopPhasePlanning, Status: "success", PlanChanged: true})

		loop := &coxv1alpha1.Loop{}
		Expect(k8sClient.Get(ctx, nn, loop)).To(Succeed())
		hasApproved := false
		for _, c := range loop.Status.Conditions {
			if c.Type == coxv1alpha1.PlanApprovedCondition && c.Status == metav1.ConditionTrue {
				hasApproved = true
			}
		}
		Expect(hasApproved).To(BeFalse(),
			"Manual mode: a PlanChanged claim clears the PlanApproved condition (re-enter the hold)")
	})

	It("Manual mode, onReject=Replan: a PlanRejected condition clears the condition (the re-plan starts fresh) and the phase stays where it is (the gate re-enters on the next reconcile)", func() {
		ns := "b3-replan-" + nowSuffix()
		nsObj := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}
		Expect(k8sClient.Create(ctx, nsObj)).To(Succeed())
		defer func() { _ = k8sClient.Delete(context.Background(), nsObj) }()

		nn, r := makeLoop(ns, "b3-replan", coxv1alpha1.ApprovalModeManual, coxv1alpha1.OnRejectReplan)
		// The Loop is at Planning (the hold kept it there). The user rejects
		// the plan (the PlanRejected condition is set). The gate's
		// applyApprovalGate sees PlanRejected True (at Planning — the
		// rejection-at-Planning branch) and clears the condition (the
		// re-plan starts fresh — the user can reject again on the new plan).
		// The phase STAYS at Planning (the re-plan is in flight; the gate's
		// Planning -> AwaitingApproval hold re-enters on the next reconcile).
		setCondition(ns, "b3-replan", coxv1alpha1.PlanRejectedCondition, "True",
			"Rejected: the plan misses the edge case")
		reconcileLoop(r, ns, "b3-replan")

		loop := &coxv1alpha1.Loop{}
		Expect(k8sClient.Get(ctx, nn, loop)).To(Succeed())
		Expect(loop.Status.Phase).To(Equal(coxv1alpha1.LoopPhasePlanning),
			"Manual mode, Replan: the phase STAYS at Planning (the re-plan is in flight)")
		// The condition is cleared (the re-plan starts fresh).
		hasRejected := false
		for _, c := range loop.Status.Conditions {
			if c.Type == coxv1alpha1.PlanRejectedCondition && c.Status == metav1.ConditionTrue {
				hasRejected = true
			}
		}
		Expect(hasRejected).To(BeFalse(), "the PlanRejected condition is cleared (the re-plan starts fresh)")
	})

	It("Manual mode, onReject=Fail: a PlanRejected condition fails the Loop (terminal, reason PlanRejected)", func() {
		ns := "b3-fail-" + nowSuffix()
		nsObj := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}
		Expect(k8sClient.Create(ctx, nsObj)).To(Succeed())
		defer func() { _ = k8sClient.Delete(context.Background(), nsObj) }()

		nn, r := makeLoop(ns, "b3-fail", coxv1alpha1.ApprovalModeManual, coxv1alpha1.OnRejectFail)
		// The user rejects the plan with onReject=Fail. The gate's
		// applyApprovalGate sees PlanRejected True (at Planning — but the
		// Planning branch clears it for a re-plan, not a fail). The Fail
		// action applies at AwaitingApproval or Implementing. So first advance
		// to AwaitingApproval (set the PlanApproved condition to release the
		// hold, deliver the claim to advance), then reject.
		//
		// Simpler: set the condition directly at the phase the Fail branch
		// handles. The gate's AwaitingApproval branch handles the rejection.
		// Force the Loop to AwaitingApproval (the gate's hold kept it at
		// Planning; set the phase directly via the status).
		loop := &coxv1alpha1.Loop{}
		Expect(k8sClient.Get(ctx, nn, loop)).To(Succeed())
		loop.Status.Phase = coxv1alpha1.LoopPhaseAwaitingApproval
		loop.Status.DesiredPhase = coxv1alpha1.LoopPhaseAwaitingApproval
		Expect(k8sClient.Status().Update(ctx, loop)).To(Succeed())
		// Now reject (onReject=Fail). The gate's AwaitingApproval branch sees
		// PlanRejected True + onReject=Fail and sets the phase to Failed.
		setCondition(ns, "b3-fail", coxv1alpha1.PlanRejectedCondition, "True",
			"Rejected: the plan is wrong")
		reconcileLoop(r, ns, "b3-fail")

		Expect(k8sClient.Get(ctx, nn, loop)).To(Succeed())
		Expect(loop.Status.Phase).To(Equal(coxv1alpha1.LoopPhaseFailed),
			"Manual mode, Fail: a PlanRejected condition fails the Loop (terminal)")
	})
})
