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

// I42b (F4 dogfood finding): a re-plan (reject -> Planning, and
// onVerifyFail=Plan -> Planning) must invalidate the previous Planning claim.
//
// Without the planAttempt guard, a reject at AwaitingApproval re-enters
// Planning in the SAME iteration (0). The original Planning success claim
// {iteration 0, observedPhase Planning, success} still matches the S5a
// consumed-guard (observedPhase == current phase), and advancePhaseFromClaim
// re-advances the NEW Planning phase at once — the re-plan completes in 15s
// with no new inference (the F4 dogfood: status.progress.lastActivityTime was
// unchanged from the first plan pass).
//
// The fix: a planAttempt counter in status that increments on every re-entry
// to Planning. The phase-init writes it to .coxswain/plan-attempt; the runner
// echoes it into its claim. advancePhaseFromClaim ignores a claim whose
// planAttempt does not match status.planAttempt.
//
// Spec 1: Manual Loop at AwaitingApproval, reject, the old Planning success
// claim (planAttempt 0) still present. The operator must STAY in Planning
// (no AwaitingApproval) until a NEW claim for the new attempt (planAttempt 1)
// arrives, then it advances (held at AwaitingApproval by the gate, as before).
//
// Spec 2: the same guard for onVerifyFail=Plan (the verify-fail re-plan path).
//
// Mutation: removing the planAttempt guard from advancePhaseFromClaim must
// make spec 1 FAIL (the stale claim re-advances to AwaitingApproval).

var _ = Describe("I42b stale Planning claim after re-plan", func() {
	ctx := context.Background()

	i42bReconciler := func() *LoopReconciler {
		r := &LoopReconciler{
			Client: k8sClient,
			Scheme: k8sClient.Scheme(),
		}
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

	// reportClaim injects the runner's claim via the readPhaseClaim seam,
	// then reconciles once. The seam goes back to (nil, nil) after.
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
	// cox approve / cox reject writes the condition; the spec simulates it).
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

	// makeManualLoop creates a Manual-mode Loop, primes it (Pending ->
	// Planning), and returns the reconciler + NamespacedName.
	makeManualLoop := func(ns, name string) (types.NamespacedName, *LoopReconciler) {
		nn := types.NamespacedName{Name: name, Namespace: ns}
		r := i42bReconciler()
		loop := &coxv1alpha1.Loop{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec: coxv1alpha1.LoopSpec{
				Goal:      loopGoal,
				Workspace: testWorkspace(),
				Approval:  &coxv1alpha1.Approval{Mode: coxv1alpha1.ApprovalModeManual},
			},
		}
		Expect(k8sClient.Create(ctx, loop)).To(Succeed())
		reconcileLoop(r, ns, name)
		return nn, r
	}

	It("reject -> re-plan: the stale Planning claim (planAttempt 0) does NOT advance the new Planning phase (planAttempt 1); a new claim (planAttempt 1) does", func() {
		ns := "i42b-reject-" + nowSuffix()
		nsObj := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}
		Expect(k8sClient.Create(ctx, nsObj)).To(Succeed())
		defer func() { _ = k8sClient.Delete(context.Background(), nsObj) }()

		nn, r := makeManualLoop(ns, "i42b-rej")

		// Step 1: the runner completes Planning (planAttempt 0). The gate
		// holds (no approval) and moves the Loop to AwaitingApproval.
		staleClaim := &PhaseClaim{
			ObservedPhase: coxv1alpha1.LoopPhasePlanning,
			Status:        "success",
			Iteration:     0,
			PlanAttempt:   0,
		}
		reportClaim(r, ns, "i42b-rej", staleClaim)

		loop := &coxv1alpha1.Loop{}
		Expect(k8sClient.Get(ctx, nn, loop)).To(Succeed())
		Expect(loop.Status.Phase).To(Equal(coxv1alpha1.LoopPhaseAwaitingApproval),
			"the Manual-mode gate holds the Planning -> Implementing advance and moves the Loop to AwaitingApproval")
		Expect(loop.Status.PlanAttempt).To(Equal(0), "the initial planAttempt is 0")

		// Step 2: the user rejects (PlanRejected condition, onReject=Replan).
		setCondition(ns, "i42b-rej", coxv1alpha1.PlanRejectedCondition, "True", "the plan is wrong")

		// Step 3: reconcile. applyApprovalGate moves AwaitingApproval ->
		// Planning and increments planAttempt to 1. The pod still has the
		// STALE claim (planAttempt 0) — re-inject it via the seam to simulate
		// the operator reading it from the pod. The planAttempt guard must
		// REJECT it: the Loop stays in Planning.
		reportClaim(r, ns, "i42b-rej", staleClaim)

		Expect(k8sClient.Get(ctx, nn, loop)).To(Succeed())
		Expect(loop.Status.PlanAttempt).To(Equal(1),
			"the re-plan incremented planAttempt to 1")
		Expect(loop.Status.Phase).To(Equal(coxv1alpha1.LoopPhasePlanning),
			"the stale Planning claim (planAttempt 0) must NOT advance the new Planning phase (planAttempt 1): the Loop stays in Planning, not AwaitingApproval")

		// Step 4: the runner re-runs Planning (the sandbox is recycled) and
		// produces a NEW claim (planAttempt 1). The planAttempt guard passes;
		// the gate holds (no approval) and moves to AwaitingApproval again.
		newClaim := &PhaseClaim{
			ObservedPhase: coxv1alpha1.LoopPhasePlanning,
			Status:        "success",
			Iteration:     0,
			PlanAttempt:   1,
		}
		reportClaim(r, ns, "i42b-rej", newClaim)

		Expect(k8sClient.Get(ctx, nn, loop)).To(Succeed())
		Expect(loop.Status.Phase).To(Equal(coxv1alpha1.LoopPhaseAwaitingApproval),
			"the NEW Planning claim (planAttempt 1) is accepted: the gate holds and moves the Loop to AwaitingApproval (as before)")
	})

	It("onVerifyFail=Plan -> re-plan: the stale Planning claim is also rejected by the planAttempt guard", func() {
		ns := "i42b-verify-" + nowSuffix()
		nsObj := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}
		Expect(k8sClient.Create(ctx, nsObj)).To(Succeed())
		defer func() { _ = k8sClient.Delete(context.Background(), nsObj) }()

		// Build a Loop with onVerifyFail=Plan (Auto mode, so the claim-driven
		// advance is not gated by the approval hold — the planAttempt guard is
		// the ONLY thing that can stop the stale claim from re-advancing).
		r := i42bReconciler()
		nn := types.NamespacedName{Name: "i42b-vfy", Namespace: ns}
		loopSpec := s5aLoopSpec()
		loopSpec.Verify.OnVerifyFail = coxv1alpha1.OnVerifyFailPlan
		loop := &coxv1alpha1.Loop{
			ObjectMeta: metav1.ObjectMeta{Name: "i42b-vfy", Namespace: ns},
			Spec:       loopSpec,
		}
		Expect(k8sClient.Create(ctx, loop)).To(Succeed())
		reconcileLoop(r, ns, "i42b-vfy")

		// Drive to Verifying: Planning claim (planAttempt 0) -> Implementing
		// (with headCommit) -> Verifying.
		reportClaim(r, ns, "i42b-vfy", &PhaseClaim{
			ObservedPhase: coxv1alpha1.LoopPhasePlanning,
			Status:        "success",
			Iteration:     0,
			PlanAttempt:   0,
		})
		reportClaim(r, ns, "i42b-vfy", &PhaseClaim{
			ObservedPhase: coxv1alpha1.LoopPhaseImplementing,
			Status:        "success",
			Iteration:     0,
			PlanAttempt:   0,
			HeadCommit:    "c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4",
		})

		Expect(k8sClient.Get(ctx, nn, loop)).To(Succeed())
		Expect(loop.Status.Phase).To(Equal(coxv1alpha1.LoopPhaseVerifying),
			"the Loop is at Verifying (the verify Job is about to run)")

		// Simulate a verify failure: the operator's applyVerifyOutcome (driven
		// by the verify Job pod's check-0 exit code) sets onVerifyFail=Plan ->
		// Planning. We simulate the phase change directly (the verify Job pod
		// is not created in this spec — the B4 specs cover that path). The
		// key assertion is the planAttempt increment + the stale claim guard.
		Expect(k8sClient.Get(ctx, nn, loop)).To(Succeed())
		loop.Status.Phase = coxv1alpha1.LoopPhasePlanning
		loop.Status.DesiredPhase = coxv1alpha1.LoopPhasePlanning
		loop.Status.Iteration++   // a re-plan is not a reset (B4)
		loop.Status.PlanAttempt++ // I42b: the verify-fail re-plan increments planAttempt
		Expect(k8sClient.Status().Update(ctx, loop)).To(Succeed())

		Expect(loop.Status.PlanAttempt).To(Equal(1),
			"the verify-fail re-plan incremented planAttempt to 1")

		// The stale Planning claim (planAttempt 0) is still on the pod.
		// Reconcile: advancePhaseFromClaim must REJECT it (planAttempt 0 != 1).
		staleClaim := &PhaseClaim{
			ObservedPhase: coxv1alpha1.LoopPhasePlanning,
			Status:        "success",
			Iteration:     loop.Status.Iteration,
			PlanAttempt:   0,
		}
		reportClaim(r, ns, "i42b-vfy", staleClaim)

		Expect(k8sClient.Get(ctx, nn, loop)).To(Succeed())
		Expect(loop.Status.Phase).To(Equal(coxv1alpha1.LoopPhasePlanning),
			"the stale Planning claim (planAttempt 0) must NOT advance the new Planning phase (planAttempt 1): the Loop stays in Planning")

		// A NEW claim (planAttempt 1) is accepted: the Loop advances to
		// Implementing (Auto mode, no approval gate).
		newClaim := &PhaseClaim{
			ObservedPhase: coxv1alpha1.LoopPhasePlanning,
			Status:        "success",
			Iteration:     loop.Status.Iteration,
			PlanAttempt:   1,
		}
		reportClaim(r, ns, "i42b-vfy", newClaim)

		Expect(k8sClient.Get(ctx, nn, loop)).To(Succeed())
		Expect(loop.Status.Phase).To(Equal(coxv1alpha1.LoopPhaseImplementing),
			"the NEW Planning claim (planAttempt 1) is accepted: the Loop advances to Implementing (Auto mode)")
	})
})
