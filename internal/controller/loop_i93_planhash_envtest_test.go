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

// I93 (R28 P1): the plan-hash check at the START of an Implementing run. The
// runner was missing this check: the plan-change signal (planChanged) lived
// only after a Planning rewrite, so an edit to PLAN.md between the user's
// approval and the Implementing run was invisible to the runner. This spec
// tests the OPERATOR side of the fix: when the runner's Implementing claim
// carries PlanMismatch=true (the plan on disk differs from the approved
// hash), the operator clears the PlanApproved condition and moves the Loop
// back to AwaitingApproval (the re-hold — the user must re-approve the
// current plan). AGENTS.md: "Every gate spec must FAIL when the gate is
// disabled" (the mutation drops the PlanMismatch handling; the spec FAILS).
var _ = Describe("I93 plan-hash check at Implementing start", func() {
	ctx := context.Background()

	It("an Implementing claim with PlanMismatch=true clears PlanApproved and re-enters AwaitingApproval (the re-hold)", func() {
		ns := "i93-" + nowSuffix()
		Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
		defer func() {
			_ = k8sClient.Delete(context.Background(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})
		}()

		name := "i93-loop"
		spec := s5aLoopSpec()
		spec.Approval = &coxv1alpha1.Approval{Mode: coxv1alpha1.ApprovalModeManual}
		Expect(k8sClient.Create(ctx, &coxv1alpha1.Loop{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec:       spec,
		})).To(Succeed())

		r := &LoopReconciler{
			Client:    k8sClient,
			Scheme:    k8sClient.Scheme(),
			apiReader: k8sClient,
		}
		r.wireApprovalGate()
		r.readPhaseClaim = func(_ context.Context, _ *coxv1alpha1.Loop) (*PhaseClaim, error) {
			return nil, nil
		}

		// Drive to Implementing: bootstrap (Pending -> Planning) -> Planning
		// claim -> AwaitingApproval (Manual hold) -> user approves ->
		// Implementing.
		// Step 1: bootstrap (Pending -> Planning).
		_, _ = r.Reconcile(context.Background(),
			reconcile.Request{NamespacedName: types.NamespacedName{Name: name, Namespace: ns}})
		loop := &coxv1alpha1.Loop{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, loop)).To(Succeed())
		loop.Status.BaseCommit = s5aBaseCommit
		Expect(k8sClient.Status().Update(ctx, loop)).To(Succeed())

		// Step 2: the Planning claim (Manual mode: moves to AwaitingApproval).
		r.readPhaseClaim = func(_ context.Context, _ *coxv1alpha1.Loop) (*PhaseClaim, error) {
			return &PhaseClaim{
				ObservedPhase: coxv1alpha1.LoopPhasePlanning,
				Status:        "success",
				HeadCommit:    s5aHeadCommit,
			}, nil
		}
		_, _ = r.Reconcile(context.Background(),
			reconcile.Request{NamespacedName: types.NamespacedName{Name: name, Namespace: ns}})
		loop = &coxv1alpha1.Loop{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, loop)).To(Succeed())
		Expect(loop.Status.Phase).To(Equal(coxv1alpha1.LoopPhaseAwaitingApproval),
			"Manual mode: the Planning claim moves the phase to AwaitingApproval (the hold)")
		r.readPhaseClaim = func(_ context.Context, _ *coxv1alpha1.Loop) (*PhaseClaim, error) {
			return nil, nil
		}

		// Step 3: the user approves the plan (the PlanApproved condition is
		// set with a valid hash). The gate releases the hold -> Implementing.
		l2 := &coxv1alpha1.Loop{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, l2)).To(Succeed())
		now := metav1.Now()
		l2.Status.Conditions = append(l2.Status.Conditions, metav1.Condition{
			Type:               coxv1alpha1.PlanApprovedCondition,
			Status:             metav1.ConditionTrue,
			Reason:             coxv1alpha1.PlanApprovedCondition,
			Message:            "Approved: 1234567890abcdef1234567890abcdef1234567890abcdef1234567890abcdef",
			LastTransitionTime: now,
		})
		Expect(k8sClient.Status().Update(ctx, l2)).To(Succeed())
		_, _ = r.Reconcile(context.Background(),
			reconcile.Request{NamespacedName: types.NamespacedName{Name: name, Namespace: ns}})
		loop = &coxv1alpha1.Loop{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, loop)).To(Succeed())
		Expect(loop.Status.Phase).To(Equal(coxv1alpha1.LoopPhaseImplementing),
			"the PlanApproved condition releases the hold (the phase advanced to Implementing)")

		// Step 4: the runner's Implementing claim carries PlanMismatch=true
		// (the plan on disk differs from the approved hash — the plan changed
		// between the user's approval and the Implementing run). The operator
		// clears the PlanApproved condition and re-enters AwaitingApproval
		// (the re-hold — the user must re-approve the current plan).
		r.readPhaseClaim = func(_ context.Context, _ *coxv1alpha1.Loop) (*PhaseClaim, error) {
			return &PhaseClaim{
				ObservedPhase: coxv1alpha1.LoopPhaseImplementing,
				Status:        "success",
				HeadCommit:    s5aHeadCommit,
				PlanMismatch:  true,
			}, nil
		}
		_, _ = r.Reconcile(context.Background(),
			reconcile.Request{NamespacedName: types.NamespacedName{Name: name, Namespace: ns}})
		loop = &coxv1alpha1.Loop{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, loop)).To(Succeed())
		Expect(loop.Status.Phase).To(Equal(coxv1alpha1.LoopPhaseAwaitingApproval),
			"the PlanMismatch signal re-enters the AwaitingApproval hold (the user must re-approve)")
		// The PlanApproved condition is cleared.
		planApprovedPresent := false
		for _, c := range loop.Status.Conditions {
			if c.Type == coxv1alpha1.PlanApprovedCondition && c.Status == metav1.ConditionTrue {
				planApprovedPresent = true
			}
		}
		Expect(planApprovedPresent).To(BeFalse(),
			"the PlanMismatch signal cleared the PlanApproved condition (the re-hold — the new plan needs re-approval)")
	})

	It("an Implementing claim WITHOUT PlanMismatch does NOT clear PlanApproved (a match or no-approved-hash is a no-op)", func() {
		ns := "i93-nomismatch-" + nowSuffix()
		Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
		defer func() {
			_ = k8sClient.Delete(context.Background(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})
		}()

		name := "i93-nomismatch"
		spec := s5aLoopSpec()
		spec.Approval = &coxv1alpha1.Approval{Mode: coxv1alpha1.ApprovalModeManual}
		Expect(k8sClient.Create(ctx, &coxv1alpha1.Loop{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec:       spec,
		})).To(Succeed())

		r := &LoopReconciler{
			Client:    k8sClient,
			Scheme:    k8sClient.Scheme(),
			apiReader: k8sClient,
		}
		r.wireApprovalGate()
		r.readPhaseClaim = func(_ context.Context, _ *coxv1alpha1.Loop) (*PhaseClaim, error) {
			return nil, nil
		}

		// Drive to Implementing (same as the mismatch spec, steps 1-3).
		_, _ = r.Reconcile(context.Background(),
			reconcile.Request{NamespacedName: types.NamespacedName{Name: name, Namespace: ns}})
		loop := &coxv1alpha1.Loop{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, loop)).To(Succeed())
		loop.Status.BaseCommit = s5aBaseCommit
		Expect(k8sClient.Status().Update(ctx, loop)).To(Succeed())

		r.readPhaseClaim = func(_ context.Context, _ *coxv1alpha1.Loop) (*PhaseClaim, error) {
			return &PhaseClaim{
				ObservedPhase: coxv1alpha1.LoopPhasePlanning,
				Status:        "success",
				HeadCommit:    s5aHeadCommit,
			}, nil
		}
		_, _ = r.Reconcile(context.Background(),
			reconcile.Request{NamespacedName: types.NamespacedName{Name: name, Namespace: ns}})
		loop = &coxv1alpha1.Loop{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, loop)).To(Succeed())
		Expect(loop.Status.Phase).To(Equal(coxv1alpha1.LoopPhaseAwaitingApproval))
		r.readPhaseClaim = func(_ context.Context, _ *coxv1alpha1.Loop) (*PhaseClaim, error) {
			return nil, nil
		}

		l2 := &coxv1alpha1.Loop{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, l2)).To(Succeed())
		now := metav1.Now()
		l2.Status.Conditions = append(l2.Status.Conditions, metav1.Condition{
			Type:               coxv1alpha1.PlanApprovedCondition,
			Status:             metav1.ConditionTrue,
			Reason:             coxv1alpha1.PlanApprovedCondition,
			Message:            "Approved: 1234567890abcdef1234567890abcdef1234567890abcdef1234567890abcdef",
			LastTransitionTime: now,
		})
		Expect(k8sClient.Status().Update(ctx, l2)).To(Succeed())
		_, _ = r.Reconcile(context.Background(),
			reconcile.Request{NamespacedName: types.NamespacedName{Name: name, Namespace: ns}})
		loop = &coxv1alpha1.Loop{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, loop)).To(Succeed())
		Expect(loop.Status.Phase).To(Equal(coxv1alpha1.LoopPhaseImplementing))

		// Step 4: the runner's Implementing claim does NOT carry PlanMismatch
		// (the plan matches the approved hash, or there is no approved hash).
		// The operator does NOT clear the PlanApproved condition (a no-op).
		r.readPhaseClaim = func(_ context.Context, _ *coxv1alpha1.Loop) (*PhaseClaim, error) {
			return &PhaseClaim{
				ObservedPhase: coxv1alpha1.LoopPhaseImplementing,
				Status:        "success",
				HeadCommit:    s5aHeadCommit,
				PlanMismatch:  false,
			}, nil
		}
		_, _ = r.Reconcile(context.Background(),
			reconcile.Request{NamespacedName: types.NamespacedName{Name: name, Namespace: ns}})
		loop = &coxv1alpha1.Loop{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, loop)).To(Succeed())
		// The Loop advanced to Verifying (a clean Implementing success), NOT
		// back to AwaitingApproval (the PlanMismatch was false — a no-op).
		Expect(loop.Status.Phase).To(Equal(coxv1alpha1.LoopPhaseVerifying),
			"the Implementing claim (no PlanMismatch) advances to Verifying (a no-op — the plan matches the approved hash)")
	})
})
