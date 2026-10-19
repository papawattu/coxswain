package controller

import (
	"context"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	coxv1alpha1 "github.com/papawattu/coxswain/api/v1alpha1"
)

// findCondition returns the loop's status condition of the given type (nil
// when absent).
func findCondition(loop *coxv1alpha1.Loop, condType string) *metav1.Condition {
	for i := range loop.Status.Conditions {
		c := loop.Status.Conditions[i]
		if c.Type == condType {
			return &c
		}
	}
	return nil
}

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
		//
		// The claim is a BLOCKED claim (the R29 P1 runner shape): the runner
		// checks the hash at the START of the Implementing run, BEFORE any
		// agent work, and on a mismatch it exits with status=blocked +
		// PlanMismatch=true + a blockedReason naming both hashes. There is no
		// headCommit (no work was done). A BLOCKED claim never advances the
		// phase machine, so the re-hold below is the ONLY phase effect.
		r.readPhaseClaim = func(_ context.Context, _ *coxv1alpha1.Loop) (*PhaseClaim, error) {
			return &PhaseClaim{
				ObservedPhase: coxv1alpha1.LoopPhaseImplementing,
				Status:        "blocked",
				BlockedReason: "PlanMismatch: current plan hash deadbeef does not match the approved hash 1234567890abcdef1234567890abcdef1234567890abcdef1234567890abcdef",
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
		// I93 (R29 P2): the mismatch is SURFACED as the PlanMismatch condition
		// (visible via `kubectl cox describe`) with the hashes in the message.
		planMismatchCond := findCondition(loop, coxv1alpha1.PlanMismatchCondition)
		Expect(planMismatchCond).NotTo(BeNil(),
			"the PlanMismatch condition is set (I90: the mismatch is visible, not silent)")
		Expect(planMismatchCond.Status).To(Equal(metav1.ConditionTrue))
		Expect(planMismatchCond.Message).To(ContainSubstring(
			"1234567890abcdef1234567890abcdef1234567890abcdef1234567890abcdef"),
			"the PlanMismatch condition's message names the approved hash")
	})

	It("an Implementing claim with PlanMismatch=true emits the PlanHashMismatch Warning Event", func() {
		ns := "i93-event-" + nowSuffix()
		Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
		defer func() {
			_ = k8sClient.Delete(context.Background(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})
		}()

		name := "i93-event-loop"
		spec := s5aLoopSpec()
		spec.Approval = &coxv1alpha1.Approval{Mode: coxv1alpha1.ApprovalModeManual}
		Expect(k8sClient.Create(ctx, &coxv1alpha1.Loop{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec:       spec,
		})).To(Succeed())

		// A FakeRecorder captures the Events (the suite client has no
		// recorder wired for this scratch reconciler).
		rec := record.NewFakeRecorder(64)
		r := &LoopReconciler{
			Client:    k8sClient,
			Scheme:    k8sClient.Scheme(),
			apiReader: k8sClient,
			Recorder:  rec,
		}
		r.wireApprovalGate()
		r.readPhaseClaim = func(_ context.Context, _ *coxv1alpha1.Loop) (*PhaseClaim, error) {
			return nil, nil
		}

		// Drive to Implementing (the same steps as the mismatch spec: bootstrap
		// -> Planning claim -> AwaitingApproval -> user approves).
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

		// The mismatch claim (status=blocked, the R29 P1 shape: the runner
		// blocks BEFORE any agent work, so there is no headCommit).
		r.readPhaseClaim = func(_ context.Context, _ *coxv1alpha1.Loop) (*PhaseClaim, error) {
			return &PhaseClaim{
				ObservedPhase: coxv1alpha1.LoopPhaseImplementing,
				Status:        "blocked",
				BlockedReason: "PlanMismatch: current plan hash deadbeef does not match the approved hash 1234567890abcdef1234567890abcdef1234567890abcdef1234567890abcdef",
				PlanMismatch:  true,
			}, nil
		}
		_, _ = r.Reconcile(context.Background(),
			reconcile.Request{NamespacedName: types.NamespacedName{Name: name, Namespace: ns}})
		loop = &coxv1alpha1.Loop{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, loop)).To(Succeed())
		Expect(loop.Status.Phase).To(Equal(coxv1alpha1.LoopPhaseAwaitingApproval))

		// The PlanHashMismatch Warning Event was emitted with the hashes in
		// the message. The FakeRecorder buffer holds every Event from the
		// whole spec (the bootstrap / approve transitions emit their own
		// events), so the scan must walk the WHOLE buffer, not just the first
		// event: the PlanHashMismatch event is the last one the spec emitted
		// (the blocked reconcile's transition-into-True).
		found := false
		var first string
		for {
			select {
			case e := <-rec.Events:
				if first == "" {
					first = e
				}
				if strings.Contains(e, "PlanHashMismatch") && strings.Contains(e, "1234567890abcdef1234567890abcdef1234567890abcdef1234567890abcdef") {
					found = true
				}
			default:
				Expect(found).To(BeTrue(),
					"the PlanHashMismatch Warning Event was emitted with the approved hash (I90: every state change has an Event); buffer: %s", first)
				return
			}
		}
	})

	It("a re-approval after the re-hold clears the PlanMismatch condition (the mismatch is resolved by the user accepting the current plan)", func() {
		ns := "i93-clear-" + nowSuffix()
		Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
		defer func() {
			_ = k8sClient.Delete(context.Background(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})
		}()

		name := "i93-clear-loop"
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

		// Drive to Implementing (bootstrap -> Planning claim ->
		// AwaitingApproval -> approve), then trigger the mismatch (the
		// same sequence as the mismatch spec; abbreviated here).
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

		// The mismatch (a blocked claim) re-enters the hold and sets the
		// PlanMismatch condition.
		r.readPhaseClaim = func(_ context.Context, _ *coxv1alpha1.Loop) (*PhaseClaim, error) {
			return &PhaseClaim{
				ObservedPhase: coxv1alpha1.LoopPhaseImplementing,
				Status:        "blocked",
				BlockedReason: "PlanMismatch: current plan hash deadbeef does not match the approved hash 1234567890abcdef1234567890abcdef1234567890abcdef1234567890abcdef",
				PlanMismatch:  true,
			}, nil
		}
		_, _ = r.Reconcile(context.Background(),
			reconcile.Request{NamespacedName: types.NamespacedName{Name: name, Namespace: ns}})
		r.readPhaseClaim = func(_ context.Context, _ *coxv1alpha1.Loop) (*PhaseClaim, error) {
			return nil, nil
		}
		loop = &coxv1alpha1.Loop{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, loop)).To(Succeed())
		Expect(loop.Status.Phase).To(Equal(coxv1alpha1.LoopPhaseAwaitingApproval))
		mismatchCond := findCondition(loop, coxv1alpha1.PlanMismatchCondition)
		Expect(mismatchCond).NotTo(BeNil())
		Expect(mismatchCond.Status).To(Equal(metav1.ConditionTrue))

		// The user re-approves the CURRENT plan: the gate's approved branch
		// advances AwaitingApproval -> Implementing AND clears the
		// PlanMismatch surface (the mismatch is resolved by the approval).
		l3 := &coxv1alpha1.Loop{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, l3)).To(Succeed())
		now2 := metav1.Now()
		l3.Status.Conditions = append(l3.Status.Conditions, metav1.Condition{
			Type:               coxv1alpha1.PlanApprovedCondition,
			Status:             metav1.ConditionTrue,
			Reason:             coxv1alpha1.PlanApprovedCondition,
			Message:            "Approved: deadbeef0000000000000000000000000000000000000000000000000000000000",
			LastTransitionTime: now2,
		})
		Expect(k8sClient.Status().Update(ctx, l3)).To(Succeed())
		_, _ = r.Reconcile(context.Background(),
			reconcile.Request{NamespacedName: types.NamespacedName{Name: name, Namespace: ns}})
		loop = &coxv1alpha1.Loop{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, loop)).To(Succeed())
		Expect(loop.Status.Phase).To(Equal(coxv1alpha1.LoopPhaseImplementing),
			"the re-approval advances back to Implementing")
		mismatchCond = findCondition(loop, coxv1alpha1.PlanMismatchCondition)
		Expect(mismatchCond).NotTo(BeNil(), "the PlanMismatch condition is still present (cleared, not deleted)")
		Expect(mismatchCond.Status).To(Equal(metav1.ConditionFalse),
			"the PlanMismatch condition is cleared to False on re-approval (the mismatch is resolved)")
		Expect(mismatchCond.Reason).To(Equal("Cleared"))
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

	// I93 (R29, kind finding — the writer-side gap): the approved hash must be
	// written to .coxswain/plan-hash at the START of an Implementing run so
	// the runner's planHashMismatchAtStart can detect a post-approval edit to
	// PLAN.md before any agent work. The phase-init re-runs when the sandbox
	// recycles for the AwaitingApproval -> Implementing transition
	// (DesiredPhase=Implementing), so planApprovalFiles MUST return the
	// approved hash for Implementing (not only for Planning) — otherwise the
	// phase-init's `rm -f .coxswain/plan-hash` REMOVES the hash on the
	// Implementing transition and the runner's check is a no-op (the edited
	// plan is implemented silently, no PlanMismatch, no Event). The FEEDBACK
	// file stays Planning-only (the planning prompt's read channel; an
	// Implementing run must not carry a stale rejection/replan feedback into
	// the conversation).
	It("the phase-init for DesiredPhase=Implementing writes .coxswain/plan-hash with the approved hash (the runner's Implementing-start check read channel)", func() {
		r := &LoopReconciler{}
		// A Manual-mode Loop at Implementing (DesiredPhase=Implementing) with
		// a valid PlanApproved condition (the user approved the plan).
		loop := &coxv1alpha1.Loop{
			ObjectMeta: metav1.ObjectMeta{Name: "i93-w", Namespace: "i93-w"},
			Spec:       coxv1alpha1.LoopSpec{Approval: &coxv1alpha1.Approval{Mode: coxv1alpha1.ApprovalModeManual}},
			Status: coxv1alpha1.LoopStatus{
				DesiredPhase: coxv1alpha1.LoopPhaseImplementing,
				Conditions: []metav1.Condition{{
					Type:    coxv1alpha1.PlanApprovedCondition,
					Status:  metav1.ConditionTrue,
					Reason:  "Approved",
					Message: "Approved: 1234567890abcdef1234567890abcdef1234567890abcdef1234567890abcdef",
				}},
			},
		}
		// The operator's decision: the approved hash is returned for
		// Implementing (the phase-init writes it to .coxswain/plan-hash). The
		// feedback is empty (the planning prompt's read channel — an Implementing
		// run must not carry a stale rejection/replan feedback).
		hash, feedback := r.planApprovalFiles(loop)
		wantHash := "1234567890abcdef1234567890abcdef1234567890abcdef1234567890abcdef"
		Expect(hash).To(Equal(wantHash),
			"planApprovalFiles at Implementing must return the approved hash (the phase-init writes .coxswain/plan-hash; the runner's planHashMismatchAtStart reads it at the Implementing start)")
		Expect(feedback).To(Equal(""),
			"planApprovalFiles at Implementing must return an EMPTY feedback (the planning prompt's read channel — no stale rejection/replan leaks into the Implementing run)")

		// The phase-init script (the phase-init container's sh -c body) WRITES
		// the hash file (printf >), not removes it (rm -f).
		script := phaseInitScript(loop.Status.DesiredPhase, loop.Status.Iteration, loop.Status.PlanAttempt, hash, feedback)
		Expect(script).To(ContainSubstring(coxPlanHashFile),
			"the phase-init script must reference the plan-hash file")
		Expect(script).To(ContainSubstring(wantHash),
			"the phase-init script must carry the approved hash")
		// It writes (printf >), not removes (rm -f), the plan-hash file.
		hashRemove := `rm -f "${DEST}/.coxswain/` + coxPlanHashFile + `"`
		Expect(script).ToNot(ContainSubstring(hashRemove),
			"the phase-init script must NOT remove .coxswain/plan-hash at the Implementing start (it must write the approved hash); the runner's check needs the hash to detect a post-approval edit")
		Expect(script).To(ContainSubstring(`printf '%s'`),
			"the phase-init script must printf the file contents")
		// The feedback file is still removed (empty feedback) — the planning
		// prompt's read channel must not carry a stale rejection into the
		// Implementing run.
		feedbackRemove := `rm -f "${DEST}/.coxswain/` + coxPlanFeedbackFile + `"`
		Expect(script).To(ContainSubstring(feedbackRemove),
			"the phase-init script must remove .coxswain/plan-feedback at the Implementing start (no stale rejection/replan feedback)")

		// Planning still carries BOTH the hash and the feedback (the existing
		// behaviour is preserved — the planning prompt reads the feedback, the
		// plan-change check reads the hash).
		planningLoop := loop.DeepCopy()
		planningLoop.Status.DesiredPhase = coxv1alpha1.LoopPhasePlanning
		planningLoop.Status.Approval = &coxv1alpha1.ApprovalStatus{LastRejectionReason: "Rejected: the plan is missing the edge case"}
		phash, pfeedback := r.planApprovalFiles(planningLoop)
		Expect(phash).To(Equal(wantHash), "planApprovalFiles at Planning returns the approved hash (unchanged)")
		Expect(pfeedback).To(ContainSubstring("Rejected:"),
			"planApprovalFiles at Planning returns the rejection feedback (the planning prompt's read channel — unchanged)")

		// A non-Planning, non-Implementing phase (e.g. Verifying) writes NEITHER
		// file (the runner reads the hash only in Planning + Implementing; a
		// stale hash must not leak into a later phase).
		verifyingLoop := loop.DeepCopy()
		verifyingLoop.Status.DesiredPhase = coxv1alpha1.LoopPhaseVerifying
		vhash, vfeedback := r.planApprovalFiles(verifyingLoop)
		Expect(vhash).To(Equal(""),
			"planApprovalFiles at Verifying returns an EMPTY hash (the runner reads the hash only in Planning + Implementing; a stale hash must not leak)")
		Expect(vfeedback).To(Equal(""),
			"planApprovalFiles at Verifying returns an EMPTY feedback (the planning prompt's read channel — no stale rejection)")
	})
})
