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
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	sandboxv1beta1 "sigs.k8s.io/agent-sandbox/api/v1beta1"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	coxv1alpha1 "github.com/papawattu/coxswain/api/v1alpha1"
	"github.com/papawattu/coxswain/internal/cni"
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
		// The gate DENIES the Planning -> Implementing advance (the approval
		// gate's Allow returns false for that transition when the PlanApproved
		// condition is absent). The claim is consumed (recorded into progress)
		// and the phase MOVES to AwaitingApproval (the hold re-enters — the
		// user approves/rejects the plan, and the gate's approved branch then
		// advances to Implementing). The operator is the sole writer of status
		// (ADR-0004).
		reportClaim(r, ns, "b3-hold", &PhaseClaim{ObservedPhase: coxv1alpha1.LoopPhasePlanning, Status: "success"})

		loop := &coxv1alpha1.Loop{}
		Expect(k8sClient.Get(ctx, nn, loop)).To(Succeed())
		Expect(loop.Status.Phase).To(Equal(coxv1alpha1.LoopPhaseAwaitingApproval),
			"Manual mode: the operator moves the phase to AwaitingApproval on the successful Planning claim (the hold re-enters; the user approves/rejects the plan)")
		// The claim was consumed (recorded into progress) — the hold is on the
		// advance, not on the claim read.
		Expect(loop.Status.Progress).NotTo(BeNil(), "the claim is recorded into progress (OS1) even while the gate holds")
		Expect(loop.Status.Progress.LastResultStatus).To(Equal("success"))
	})

	// F49 (T-2): the Manual-mode AwaitingApproval sandbox hold has no other
	// spec, and "assert the sandbox is Suspended" alone would pass for the
	// WRONG reason: a reconciler with no Enforcer and AllowUnenforced unset is
	// held Suspended by the D30 fail-closed gate whatever the phase is. So this
	// spec wires what D30 needs to be satisfied (a CNI prober that reports
	// CNIEnforced) and AllowUnenforced, then asserts the NetworkEnforced
	// condition is True while the sandbox is Suspended — the only remaining
	// cause is approvalHoldPhase. The control is the same sandbox RUNNING at
	// Planning before the claim, and RUNNING again after the approval.
	It("Manual mode: the AwaitingApproval hold — not the D30/CNI hold — keeps the sandbox Suspended, and approving releases it", func() {
		ns := "b3-holdmode-" + nowSuffix()
		nsObj := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}
		Expect(k8sClient.Create(ctx, nsObj)).To(Succeed())
		defer func() { _ = k8sClient.Delete(context.Background(), nsObj) }()

		r := &LoopReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), apiReader: k8sClient,
			AllowUnenforced: true, CNIProber: cni.NewFakeProber()}
		r.CNIProber.(*cni.FakeProber).SetResult(cni.CNIProbeResult{Reason: cni.ReasonCNIEnforced})
		r.wireApprovalGate()
		r.readPhaseClaim = func(_ context.Context, _ *coxv1alpha1.Loop) (*PhaseClaim, error) {
			return nil, nil
		}

		name := "b3-holdmode"
		loop := &coxv1alpha1.Loop{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec: coxv1alpha1.LoopSpec{
				Goal: loopGoal, Workspace: testWorkspace(),
				Approval: &coxv1alpha1.Approval{Mode: coxv1alpha1.ApprovalModeManual},
			},
		}
		Expect(k8sClient.Create(ctx, loop)).To(Succeed())
		sandboxMode := func() sandboxv1beta1.SandboxOperatingMode {
			sb := &sandboxv1beta1.Sandbox{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: sandboxName(name)}, sb)).To(Succeed(),
				"the operator must have created the sandbox")
			return sb.Spec.OperatingMode
		}
		networkEnforced := func() *metav1.Condition {
			got := &coxv1alpha1.Loop{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, got)).To(Succeed())
			for i := range got.Status.Conditions {
				if got.Status.Conditions[i].Type == cni.NetworkEnforcedCondition {
					return &got.Status.Conditions[i]
				}
			}
			return nil
		}

		// settle runs the reconcile to a steady state: a phase advance deletes
		// the live Sandbox on one pass (the desired-phase annotation mismatch)
		// and the next pass creates the fresh one with the new phase's mode.
		settle := func() {
			for range 3 {
				reconcileLoop(r, ns, name)
			}
		}

		By("the control: at Planning, with the CNI enforced, the sandbox RUNS")
		settle()
		Expect(sandboxMode()).To(Equal(sandboxv1beta1.SandboxOperatingModeRunning),
			"the control condition: nothing holds the sandbox at Planning when the network gate is satisfied")

		By("the hold: the Planning claim parks the Loop at AwaitingApproval and the sandbox goes Suspended")
		reportClaim(r, ns, name, &PhaseClaim{ObservedPhase: coxv1alpha1.LoopPhasePlanning, Status: "success"})
		settle()
		got := &coxv1alpha1.Loop{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, got)).To(Succeed())
		Expect(got.Status.Phase).To(Equal(coxv1alpha1.LoopPhaseAwaitingApproval))
		Expect(sandboxMode()).To(Equal(sandboxv1beta1.SandboxOperatingModeSuspended),
			"the Manual-mode AwaitingApproval hold must suspend the sandbox (approvalHoldPhase)")
		c := networkEnforced()
		Expect(c).NotTo(BeNil(), "the CNI gate must have reported a NetworkEnforced condition")
		Expect(c.Status).To(Equal(metav1.ConditionTrue),
			"the network gate is SATISFIED here, so it is not what suspends the sandbox — the approval hold is")

		By("the release: an approval lets the sandbox RUN again (the hold no longer applies)")
		setCondition(ns, name, coxv1alpha1.PlanApprovedCondition, "True",
			"Approved: 1234567890abcdef1234567890abcdef1234567890abcdef1234567890abcdef")
		reportClaim(r, ns, name, &PhaseClaim{ObservedPhase: coxv1alpha1.LoopPhasePlanning, Status: "success"})
		settle()
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, got)).To(Succeed())
		Expect(got.Status.Phase).To(Equal(coxv1alpha1.LoopPhaseImplementing))
		Expect(sandboxMode()).To(Equal(sandboxv1beta1.SandboxOperatingModeRunning),
			"with the plan approved, the approval hold is gone and the sandbox runs")
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

	// B3 (R1 review P1): the FULL Replan sequence — AwaitingApproval ->
	// PlanRejected (Replan) -> reconcile -> the operator makes the reason
	// DURABLE (status.approval.lastRejectionReason) AND clears the PlanRejected
	// condition on the same reconcile -> the phase-init script (run on the next
	// Planning sandbox) writes the reason to .coxswain/plan-feedback from the
	// DURABLE field (the condition is already cleared — the durable field is
	// what survives to the phase-init). This is the handoff the kind finding
	// proved was broken (the condition did not survive the clear); it FAILS
	// against the pre-fix code (dc0ee8c: planApprovalFiles read the transient
	// condition, which the Replan branch had cleared).
	It("Manual mode, onReject=Replan: the rejection reason is DURABLE (status.approval.lastRejectionReason) and the phase-init script writes it to plan-feedback even after the PlanRejected condition is cleared (the full AwaitingApproval -> Replan -> phase-init sequence)", func() {
		ns := "b3-replan-durable-" + nowSuffix()
		nsObj := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}
		Expect(k8sClient.Create(ctx, nsObj)).To(Succeed())
		defer func() { _ = k8sClient.Delete(context.Background(), nsObj) }()

		const reason = "Rejected: the plan misses the edge case: it does not handle empty input"
		// A Manual-mode Loop at AwaitingApproval (the hold kept it there after
		// Planning). The user rejects the plan (the PlanRejected condition is
		// set). onReject=Replan.
		_, r := makeLoop(ns, "b3-replan-durable", coxv1alpha1.ApprovalModeManual, coxv1alpha1.OnRejectReplan)
		loop := &coxv1alpha1.Loop{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "b3-replan-durable", Namespace: ns}, loop)).To(Succeed())
		loop.Status.Phase = coxv1alpha1.LoopPhaseAwaitingApproval
		loop.Status.DesiredPhase = coxv1alpha1.LoopPhaseAwaitingApproval
		Expect(k8sClient.Status().Update(ctx, loop)).To(Succeed())
		setCondition(ns, "b3-replan-durable", coxv1alpha1.PlanRejectedCondition, "True", reason)

		// The gate's applyApprovalGate sees PlanRejected True at AwaitingApproval
		// + onReject=Replan: it sets the DURABLE status.approval.
		// lastRejectionReason AND clears the PlanRejected condition on the SAME
		// reconcile. The phase STAYS at AwaitingApproval (the hold is still in
		// force; the re-plan is triggered by the next Planning claim).
		reconcileLoop(r, ns, "b3-replan-durable")
		loop = &coxv1alpha1.Loop{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "b3-replan-durable", Namespace: ns}, loop)).To(Succeed())
		// The DURABLE field now carries the reason (the operator made it durable
		// before clearing the condition).
		Expect(loop.Status.Approval).ToNot(BeNil(),
			"the operator set status.approval (the durable rejection record)")
		Expect(loop.Status.Approval.LastRejectionReason).To(Equal(reason),
			"the rejection reason is DURABLE on status.approval.lastRejectionReason")
		// The PlanRejected condition is CLEARED (the re-plan starts fresh).
		hasRejected := false
		for _, c := range loop.Status.Conditions {
			if c.Type == coxv1alpha1.PlanRejectedCondition && c.Status == metav1.ConditionTrue {
				hasRejected = true
			}
		}
		Expect(hasRejected).To(BeFalse(), "the PlanRejected condition is cleared (the re-plan starts fresh)")
		// The operator moved the phase to Planning on the Replan reconcile (the
		// re-plan runs now — the runner's next Planning claim, once the user
		// re-approves, advances to Implementing). ASSERT the phase is Planning
		// (the gate's own decision, not a manual force).
		Expect(loop.Status.Phase).To(Equal(coxv1alpha1.LoopPhasePlanning),
			"the operator moved the phase to Planning on the Replan reconcile (the re-plan runs now)")

		// Now the phase-init script (the operator's sole writer of
		// .coxswain/plan-feedback) runs on the next Planning sandbox. It reads
		// the DURABLE field (the condition is already cleared) and writes the
		// reason to the file. The phase is now Planning (the re-plan).
		loop = &coxv1alpha1.Loop{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "b3-replan-durable", Namespace: ns}, loop)).To(Succeed())

		// The phase-init's decision (planApprovalFiles) reads the DURABLE field
		// (the condition is cleared — the durable field is what survives).
		loop = &coxv1alpha1.Loop{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "b3-replan-durable", Namespace: ns}, loop)).To(Succeed())
		planHash, planFeedback := r.planApprovalFiles(loop)
		Expect(planFeedback).To(Equal(reason),
			"the phase-init writes the DURABLE reason to plan-feedback (the condition was cleared on the prior reconcile)")
		// The phase-init script carries the reason (shell-quoted, written to the
		// file) and references the feedback file.
		script := phaseInitScript(loop.Status.DesiredPhase, loop.Status.Iteration, loop.Status.PlanAttempt, planHash, planFeedback)
		Expect(script).To(ContainSubstring(coxPlanFeedbackFile),
			"the phase-init script references the plan-feedback file")
		Expect(script).To(ContainSubstring("the plan misses the edge case"),
			"the phase-init script carries the rejection reason")
		Expect(script).NotTo(ContainSubstring("rm -f \"${DEST}/.coxswain/"+coxPlanFeedbackFile+"\""),
			"the phase-init WRITES (not removes) the plan-feedback file on a Replan re-plan")
	})

	// B3 (R1 review P1, second round): the DURABLE rejection reason must be
	// CLEARED when an approval is accepted, so a LATER Planning run (a plan
	// change that re-enters the hold, or a new rejection cycle) does NOT read a
	// stale reason from a prior rejection. The full sequence: reject (Replan) ->
	// the durable reason is set -> re-plan -> approve (the approval is
	// accepted) -> the durable reason is CLEARED -> another Planning run ->
	// planApprovalFiles writes an EMPTY plan-feedback (no stale reason leaks
	// into the next plan's prompt). This spec FAILS against 18aa3dd (the
	// durable reason was never cleared, so the later Planning run reads the
	// stale reason).
	It("Manual mode, onReject=Replan: the durable rejection reason is CLEARED when an approval is accepted, so a later Planning run writes an EMPTY plan-feedback (no stale reason leaks into the next plan)", func() {
		ns := "b3-replan-clear-" + nowSuffix()
		nsObj := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}
		Expect(k8sClient.Create(ctx, nsObj)).To(Succeed())
		defer func() { _ = k8sClient.Delete(context.Background(), nsObj) }()

		const reason = "Rejected: the plan misses the edge case"
		const approvedHash = "9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08" // sha256("plan-v1\n")
		// A Manual-mode Loop at AwaitingApproval. The user rejects the plan
		// (Replan). The gate sets the durable reason + clears the condition.
		_, r := makeLoop(ns, "b3-replan-clear", coxv1alpha1.ApprovalModeManual, coxv1alpha1.OnRejectReplan)
		loop := &coxv1alpha1.Loop{}
		nn := types.NamespacedName{Name: "b3-replan-clear", Namespace: ns}
		Expect(k8sClient.Get(ctx, nn, loop)).To(Succeed())
		loop.Status.Phase = coxv1alpha1.LoopPhaseAwaitingApproval
		loop.Status.DesiredPhase = coxv1alpha1.LoopPhaseAwaitingApproval
		Expect(k8sClient.Status().Update(ctx, loop)).To(Succeed())
		setCondition(ns, "b3-replan-clear", coxv1alpha1.PlanRejectedCondition, "True", reason)
		reconcileLoop(r, ns, "b3-replan-clear")
		// The durable reason is now set.
		loop = &coxv1alpha1.Loop{}
		Expect(k8sClient.Get(ctx, nn, loop)).To(Succeed())
		Expect(loop.Status.Approval).ToNot(BeNil(), "the operator set the durable rejection record")
		Expect(loop.Status.Approval.LastRejectionReason).To(Equal(reason),
			"the rejection reason is DURABLE after the Replan reconcile")
		// The operator moved the phase to Planning on the Replan reconcile (the
		// re-plan runs now). ASSERT the phase is Planning (the gate's own
		// decision, not a manual force).
		Expect(loop.Status.Phase).To(Equal(coxv1alpha1.LoopPhasePlanning),
			"the operator moved the phase to Planning on the Replan reconcile (the re-plan runs now)")

		// Now the user approves the (re)plan. The gate's approved branch clears
		// the durable reason + advances to Implementing. (The phase is Planning
		// — the re-plan; the user re-approves the new plan. The gate's approved
		// branch fires when the phase is AwaitingApproval, so force the phase
		// back to AwaitingApproval to simulate the hold re-entering on the new
		// plan, then the user approves it.)
		loop = &coxv1alpha1.Loop{}
		Expect(k8sClient.Get(ctx, nn, loop)).To(Succeed())
		loop.Status.Phase = coxv1alpha1.LoopPhaseAwaitingApproval
		loop.Status.DesiredPhase = coxv1alpha1.LoopPhaseAwaitingApproval
		Expect(k8sClient.Status().Update(ctx, loop)).To(Succeed())
		setCondition(ns, "b3-replan-clear", coxv1alpha1.PlanApprovedCondition, "True", approvedHash)
		reconcileLoop(r, ns, "b3-replan-clear")
		// The approval is accepted: the phase advanced to Implementing AND the
		// durable reason is CLEARED.
		loop = &coxv1alpha1.Loop{}
		Expect(k8sClient.Get(ctx, nn, loop)).To(Succeed())
		Expect(loop.Status.Phase).To(Equal(coxv1alpha1.LoopPhaseImplementing),
			"the approval released the hold (the phase advanced to Implementing)")
		Expect(loop.Status.Approval).To(BeNil(),
			"the durable rejection record is CLEARED after the approval is accepted (the prior rejection is no longer relevant)")

		// Now a LATER Planning run (e.g. a plan change that re-enters the hold).
		// The phase-init's planApprovalFiles must write an EMPTY plan-feedback
		// (no stale reason leaks into the next plan's prompt).
		loop = &coxv1alpha1.Loop{}
		Expect(k8sClient.Get(ctx, nn, loop)).To(Succeed())
		loop.Status.Phase = coxv1alpha1.LoopPhasePlanning
		loop.Status.DesiredPhase = coxv1alpha1.LoopPhasePlanning
		Expect(k8sClient.Status().Update(ctx, loop)).To(Succeed())
		loop = &coxv1alpha1.Loop{}
		Expect(k8sClient.Get(ctx, nn, loop)).To(Succeed())
		_, planFeedback := r.planApprovalFiles(loop)
		Expect(planFeedback).To(BeEmpty(),
			"the later Planning run writes an EMPTY plan-feedback (the durable reason was cleared on approval; no stale reason leaks into the next plan's prompt)")
	})

	// B3 (R1 P1, fourth round): on a SUCCESSFUL Planning claim with no
	// approval (a Manual-mode Loop), the operator MUST MOVE the phase to
	// AwaitingApproval (the hold re-enters). The gate DENIES the Planning ->
	// Implementing advance (no approval), but the runner COMPLETED the Planning
	// phase (a success claim with a valid headCommit) — the Loop must NOT rerun
	// Planning forever. The claim-driven advance (recordPhaseClaim) moves the
	// phase to AwaitingApproval when the gate denies the Planning ->
	// Implementing transition. This spec FAILS against 521d7d5 (the phase stayed
	// at Planning — nothing moved it to the hold).
	It("Manual mode: a successful Planning claim with no approval MOVES the phase to AwaitingApproval (the hold re-enters; the Loop does NOT rerun Planning forever)", func() {
		ns := "b3-plan-claim-" + nowSuffix()
		nsObj := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}
		Expect(k8sClient.Create(ctx, nsObj)).To(Succeed())
		defer func() { _ = k8sClient.Delete(context.Background(), nsObj) }()

		_, r := makeLoop(ns, "b3-plan-claim", coxv1alpha1.ApprovalModeManual, coxv1alpha1.OnRejectReplan)
		loop := &coxv1alpha1.Loop{}
		nn := types.NamespacedName{Name: "b3-plan-claim", Namespace: ns}
		Expect(k8sClient.Get(ctx, nn, loop)).To(Succeed())
		// The Loop is at Planning (the runner is running the Planning phase).
		loop.Status.Phase = coxv1alpha1.LoopPhasePlanning
		loop.Status.DesiredPhase = coxv1alpha1.LoopPhasePlanning
		Expect(k8sClient.Status().Update(ctx, loop)).To(Succeed())
		// Simulate the runner's successful Planning claim (the runner wrote
		// PLAN.md + a success claim with a valid headCommit). The claim is on
		// the sandbox pod (the operator reads it from the pod's annotation or
		// the PVC — in envtest, the claim is simulated by calling
		// recordPhaseClaim directly, which is the operator's own decision).
		loop = &coxv1alpha1.Loop{}
		Expect(k8sClient.Get(ctx, nn, loop)).To(Succeed())
		claim := &PhaseClaim{
			ObservedPhase: coxv1alpha1.LoopPhasePlanning,
			Status:        "success",
			Iteration:     1,
			HeadCommit:    "9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08", // sha256("plan-v1\n") — 40-hex
		}
		now := metav1.Now()
		changed, advanced := r.recordPhaseClaim(ctx, loop, claim, now)
		_ = changed
		Expect(advanced).To(BeTrue(), "the phase advanced (the claim-driven move to AwaitingApproval)")
		// The phase is now AwaitingApproval (the hold re-enters — the user
		// approves/rejects the plan). The operator is the sole writer of
		// status (ADR-0004).
		Expect(loop.Status.Phase).To(Equal(coxv1alpha1.LoopPhaseAwaitingApproval),
			"the operator moved the phase to AwaitingApproval on the successful Planning claim (the hold re-enters; the Loop does NOT rerun Planning forever)")
	})

	// P1 (reviewer fix): lastActivityTime is NOT refreshed at the rejection.
	// Stamping at the reject would make the field move on a user action even
	// if the agent never runs. The refresh happens on the re-plan's
	// claim-driven advance (recordPhaseClaim), where a new planAttempt claim
	// signals that the agent actually ran.
	//
	// (a) reject alone, with no new claim, leaves lastActivityTime UNCHANGED.
	It("P1 (a): reject alone, with no new claim, leaves lastActivityTime unchanged", func() {
		ns := "p2-reject-a-" + nowSuffix()
		nsObj := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}
		Expect(k8sClient.Create(ctx, nsObj)).To(Succeed())
		defer func() { _ = k8sClient.Delete(context.Background(), nsObj) }()

		_, r := makeLoop(ns, "p2-reject-a", coxv1alpha1.ApprovalModeManual, coxv1alpha1.OnRejectReplan)
		// Move the Loop to AwaitingApproval with a STALE lastActivityTime.
		staleTime := metav1.NewTime(time.Now().Add(-2 * time.Hour).Truncate(time.Second))
		loop := &coxv1alpha1.Loop{}
		nn := types.NamespacedName{Name: "p2-reject-a", Namespace: ns}
		Expect(k8sClient.Get(ctx, nn, loop)).To(Succeed())
		loop.Status.Phase = coxv1alpha1.LoopPhaseAwaitingApproval
		loop.Status.DesiredPhase = coxv1alpha1.LoopPhaseAwaitingApproval
		loop.Status.Progress = &coxv1alpha1.ProgressStatus{
			LastActivityTime: &staleTime,
		}
		Expect(k8sClient.Status().Update(ctx, loop)).To(Succeed())

		// The user rejects the plan (the PlanRejected condition is set).
		setCondition(ns, "p2-reject-a", coxv1alpha1.PlanRejectedCondition, "True",
			"Rejected: the plan is missing error handling")

		// Reconcile: the gate's applyApprovalGate sees PlanRejected True at
		// AwaitingApproval + onReject=Replan: it re-plans. But the rejection
		// does NOT refresh lastActivityTime (P1 fix).
		reconcileLoop(r, ns, "p2-reject-a")

		loop = &coxv1alpha1.Loop{}
		Expect(k8sClient.Get(ctx, nn, loop)).To(Succeed())
		// The phase is Planning (the re-plan runs now).
		Expect(loop.Status.Phase).To(Equal(coxv1alpha1.LoopPhasePlanning),
			"the re-plan moved the phase to Planning")
		// P1 (a): lastActivityTime is UNCHANGED (the rejection is a user action,
		// not agent activity).
		Expect(loop.Status.Progress).ToNot(BeNil(),
			"status.progress must be set")
		Expect(loop.Status.Progress.LastActivityTime).ToNot(BeNil(),
			"lastActivityTime must be set")
		// The API server truncates timestamps to seconds; compare with Truncate.
		actualTime := loop.Status.Progress.LastActivityTime.Truncate(time.Second)
		expectedTime := staleTime.Truncate(time.Second)
		Expect(actualTime.Equal(expectedTime)).To(BeTrue(),
			"lastActivityTime must be UNCHANGED after a reject with no new claim (the rejection does not refresh it)")
	})

	// P1 (reviewer fix): a new planAttempt claim refreshes lastActivityTime
	// on the claim-driven advance (recordPhaseClaim). The re-plan claim must
	// be identical to the previous one in every progress field except
	// planAttempt (same phase, status, iteration, blockedReason), so that
	// only the new PlanAttempt comparison in progressEqual makes the
	// progress write fire. This is the fix for the F4 gap: the re-plan's
	// advance did not refresh lastActivityTime because progressEqual
	// suppressed the write when the claim-derived fields were the same.
	//
	// (b) reject then a new planAttempt claim (identical in every progress
	// field except planAttempt) refreshes lastActivityTime.
	It("P1 (b): reject then a new planAttempt claim refreshes lastActivityTime", func() {
		ns := "p2-replan-b-" + nowSuffix()
		nsObj := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}
		Expect(k8sClient.Create(ctx, nsObj)).To(Succeed())
		defer func() { _ = k8sClient.Delete(context.Background(), nsObj) }()

		_, r := makeLoop(ns, "p2-replan-b", coxv1alpha1.ApprovalModeManual, coxv1alpha1.OnRejectReplan)
		// Move the Loop to AwaitingApproval with a STALE lastActivityTime and
		// a progress record from the PRIOR claim (planAttempt=0, same
		// phase/status/iteration as the re-plan claim). The reject branch does
		// not write progress (it only moves the phase to Planning and
		// increments planAttempt), so the progress record survives the reject
		// unchanged. The re-plan claim (planAttempt=1) differs from the
		// progress record in exactly one field (planAttempt), so
		// progressEqual can only make the progress write fire on the new
		// PlanAttempt comparison.
		//
		// The clock is overridden so the refresh is deterministic (the operator
		// stamps lastActivityTime with now(); without the override, the
		// same-second refresh can be indistinguishable from the stale value at
		// 1-second precision).
		var fakeNow int64
		r.now = func() metav1.Time { return metav1.NewTime(time.Now().Add(time.Duration(fakeNow) * time.Second)) }
		staleTime := metav1.NewTime(time.Now().Add(-2 * time.Hour).Truncate(time.Second))
		loop := &coxv1alpha1.Loop{}
		nn := types.NamespacedName{Name: "p2-replan-b", Namespace: ns}
		Expect(k8sClient.Get(ctx, nn, loop)).To(Succeed())
		loop.Status.Phase = coxv1alpha1.LoopPhaseAwaitingApproval
		loop.Status.DesiredPhase = coxv1alpha1.LoopPhaseAwaitingApproval
		// The iteration is 1 (matching the progress record and the re-plan
		// claim). The S5a stale-iteration guard (advancePhaseFromClaim) rejects
		// a claim whose Iteration differs from loop.Status.Iteration; without
		// this, the re-plan claim (Iteration=1) is rejected before
		// recordPhaseClaim is called.
		loop.Status.Iteration = 1
		// The progress record is the PRIOR claim's record (planAttempt=0,
		// same phase/status/iteration as the re-plan claim). Only
		// planAttempt differs, so progressEqual can only make the progress
		// write fire on the new PlanAttempt comparison.
		loop.Status.Progress = &coxv1alpha1.ProgressStatus{
			Phase:            coxv1alpha1.LoopPhasePlanning,
			LastActivityTime: &staleTime,
			Iteration:        1,
			LastResultStatus: "success",
			PlanAttempt:      0, // the prior plan (the reject increments to 1)
		}
		Expect(k8sClient.Status().Update(ctx, loop)).To(Succeed())

		// The user rejects the plan (the PlanRejected condition is set).
		setCondition(ns, "p2-replan-b", coxv1alpha1.PlanRejectedCondition, "True",
			"Rejected: the plan is missing error handling")
		reconcileLoop(r, ns, "p2-replan-b")

		// The reject branch does NOT write progress (it only moves the phase
		// to Planning and increments planAttempt). The progress record is
		// still the prior claim's record (planAttempt=0, stale
		// lastActivityTime). The re-plan claim (planAttempt=1) will be
		// compared against that record on the claim-driven advance.
		loop = &coxv1alpha1.Loop{}
		Expect(k8sClient.Get(ctx, nn, loop)).To(Succeed())
		Expect(loop.Status.Progress).ToNot(BeNil(),
			"the progress record must survive the reject (the reject branch does not write progress)")
		Expect(loop.Status.Progress.LastActivityTime).ToNot(BeNil(),
			"the stale lastActivityTime must survive the reject (the reject branch does not refresh it)")
		preClaimTime := loop.Status.Progress.LastActivityTime.Truncate(time.Second)

		// The re-plan moved the phase to Planning.
		loop = &coxv1alpha1.Loop{}
		Expect(k8sClient.Get(ctx, nn, loop)).To(Succeed())
		Expect(loop.Status.Phase).To(Equal(coxv1alpha1.LoopPhasePlanning),
			"the re-plan moved the phase to Planning")

		// Now the runner's re-plan Planning claim arrives. The claim is
		// identical to the previous one in every progress field except
		// planAttempt (same phase=Planning, status=success, iteration=1,
		// no blockedReason; planAttempt=1 vs the prior 0). recordPhaseClaim
		// refreshes lastActivityTime on the claim-driven advance (the new
		// PlanAttempt comparison in progressEqual makes the write fire).
		fakeNow = 1
		reportClaim(r, ns, "p2-replan-b", &PhaseClaim{
			ObservedPhase: coxv1alpha1.LoopPhasePlanning,
			Status:        "success",
			Iteration:     1, // matches the progress record's iteration (the S5a guard passes)
			PlanAttempt:   1, // the re-plan (planAttempt incremented by the reject; the only field that differs)
		})

		loop = &coxv1alpha1.Loop{}
		Expect(k8sClient.Get(ctx, nn, loop)).To(Succeed())
		Expect(loop.Status.Progress).ToNot(BeNil(),
			"status.progress must be set")
		Expect(loop.Status.Progress.LastActivityTime).ToNot(BeNil(),
			"lastActivityTime must be set after the re-plan claim")
		// P1 (b): lastActivityTime is > the pre-claim value (the claim-driven
		// advance refreshed it; a no-refresh would leave the pre-claim value
		// in place). preClaimTime is the stale value the reject left in the
		// progress record (the reject branch does not write progress). The
		// clock was advanced 1s before the claim, so the refreshed value is
		// strictly after the pre-claim value (deterministic, not a
		// same-second flake).
		actualTime := loop.Status.Progress.LastActivityTime.Truncate(time.Second)
		Expect(actualTime.After(preClaimTime)).To(BeTrue(),
			"lastActivityTime must be > the stale pre-claim value (the re-plan claim refreshed it; a no-refresh would leave the stale value in place)")

		// Mutation B: drop `&& a.PlanAttempt == b.PlanAttempt` from
		// progressEqual (loop_s4_phase.go:575-576) — the re-plan claim is
		// identical to the progress record in every progress field, so
		// progressEqual returns true and no progress write fires;
		// lastActivityTime stays at the stale value and this spec FAILS.
		//
		// Mutation C: drop `PlanAttempt: claim.PlanAttempt,` from
		// recordPhaseClaim (loop_s4_phase.go:420) — the progress record's
		// PlanAttempt is 0 (zero-value), the same as the prior claim; the
		// candidate's PlanAttempt is also 0, so progressEqual returns true
		// and no progress write fires; lastActivityTime stays at the stale
		// value and this spec FAILS.
	})
})
