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

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	coxv1alpha1 "github.com/papawattu/coxswain/api/v1alpha1"
)

// B4 (PLAN-BETA B4; PLAN.md Phase 6 remainder): re-plan on verify failure.
// These specs drive the operator's applyVerifyOutcome (the B3 verify-outcome
// reader) through the reconciler's Reconcile, with a stand-in verify Job pod
// whose check-0 init container terminates non-zero (a failed verify). They
// cover:
//
//   - onVerifyFail=Plan: a failed verify goes to PLANNING (not Implementing),
//     the iteration increments (a re-plan is not a reset), and the failure
//     summary is made DURABLE on status.verify.lastFailureSummary (the
//     phase-init reads it to write .coxswain/plan-feedback, reusing B3's
//     plan-feedback channel).
//   - onVerifyFail default (empty / Implement): a failed verify goes to
//     IMPLEMENTING (the alpha behaviour, unchanged), and the failure summary
//     is NOT set (the default path does not write lastFailureSummary).
//   - onVerifyFail=Plan + approval.mode=Manual: the re-plan goes through
//     AwaitingApproval again (the claim-driven advance: the runner's next
//     Planning claim, once the user approves, advances to Implementing — the
//     gate's hold re-enters on the new plan).
//   - The stall detector's StallEntry is appended (the entry is in
//     status.stallHistory) even on a re-plan (the stall count spans re-plans).
//   - The maxIterations cap applies to the re-plan path (a re-plan does not
//     bypass the cap: the increment is the same either way, so the cap is
//     reached at the same iteration — the Loop is Failed:MaxIterationsExceeded).
//   - An in-progress verify (check-0 still Running, not terminated) makes NO
//     decision (no phase change, no iteration increment, no summary written)
//     — the operator requeues (the verify is not yet terminal).
//
// The specs use the s5a helpers (s5aDriveToVerifying, s5aVerifyPod) from
// loop_verify_job_envtest_test.go. Each It is self-contained with its own
// namespace (envtest gotcha: a shared namespace lets one It's sandbox/status
// leak into the next).

var _ = Describe("B4 re-plan on verify failure", func() {
	ctx := context.Background()

	// b4LoopSpec builds a Loop spec with onVerifyFail=Plan (and the given
	// approval mode). The acceptance check is a single check (check-0) so the
	// verify Job's check inits are non-empty. The onVerifyFail is always Plan
	// (the default/Implement path is tested by the S5a specs, not here).
	b4LoopSpec := func(mode coxv1alpha1.ApprovalMode) coxv1alpha1.LoopSpec {
		spec := s5aLoopSpec()
		spec.Verify.OnVerifyFail = coxv1alpha1.OnVerifyFailPlan
		if mode != "" {
			spec.Approval = &coxv1alpha1.Approval{Mode: mode}
		}
		return spec
	}

	// b4FailingPod creates the stand-in verify Job pod with check-0 FAILED
	// (exit 1) and a terminationMessage (the check's raw output tail — the
	// operator reads it for the failure summary). The trusted inits and
	// tamper are exit 0 (the verify infrastructure is healthy; the check is
	// what failed).
	b4FailingPod := func(ctx context.Context, ns, name, jobName string) {
		job := &batchv1.Job{}
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: jobName, Namespace: ns}, job); err != nil {
			if apierrors.IsNotFound(err) {
				job = &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: jobName, Namespace: ns, Labels: verifyJobLabels(name)}}
				Expect(k8sClient.Create(ctx, job)).To(Succeed())
			} else {
				Expect(err).NotTo(HaveOccurred())
			}
		}
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      name + "-verify-pod",
				Namespace: ns,
				Labels:    map[string]string{s5aJobNameLabel: jobName, verifyForLabel: name},
				OwnerReferences: []metav1.OwnerReference{{
					APIVersion: batchv1.SchemeGroupVersion.String(),
					Kind:       i65JobKind,
					Name:       jobName,
					UID:        job.UID,
				}},
			},
			Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: verifyNoopContainer, Image: verifyBusybox}}},
		}
		Expect(k8sClient.Create(ctx, pod)).To(Succeed())
		pod.Status.InitContainerStatuses = []corev1.ContainerStatus{
			{Name: s5aCloneBase, State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0}}},
			{Name: s5aImportAgent, State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0}}},
			{Name: s5aTamper, State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0}}},
			{Name: s5aCheck0, State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
				ExitCode: 1,
				Message:  "FAIL: expected file /workspace/out.txt to exist\ngit diff --stat: 3 files changed",
			}}},
		}
		Expect(k8sClient.Status().Update(ctx, pod)).To(Succeed())
	}

	// s5aPassingPod creates the stand-in verify Job pod with check-0 PASSED
	// (exit 0). The trusted inits and tamper are exit 0. The verify PASSES.
	s5aPassingPod := func(ctx context.Context, ns, name, jobName string) {
		job := &batchv1.Job{}
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: jobName, Namespace: ns}, job); err != nil {
			if apierrors.IsNotFound(err) {
				job = &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: jobName, Namespace: ns, Labels: verifyJobLabels(name)}}
				Expect(k8sClient.Create(ctx, job)).To(Succeed())
			} else {
				Expect(err).NotTo(HaveOccurred())
			}
		}
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      name + "-verify-pod",
				Namespace: ns,
				Labels:    map[string]string{s5aJobNameLabel: jobName, verifyForLabel: name},
				OwnerReferences: []metav1.OwnerReference{{
					APIVersion: batchv1.SchemeGroupVersion.String(),
					Kind:       i65JobKind,
					Name:       jobName,
					UID:        job.UID,
				}},
			},
			Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: verifyNoopContainer, Image: verifyBusybox}}},
		}
		Expect(k8sClient.Create(ctx, pod)).To(Succeed())
		pod.Status.InitContainerStatuses = []corev1.ContainerStatus{
			{Name: s5aCloneBase, State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0}}},
			{Name: s5aImportAgent, State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0}}},
			{Name: s5aTamper, State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0}}},
			{Name: s5aCheck0, State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0}}},
		}
		Expect(k8sClient.Status().Update(ctx, pod)).To(Succeed())
	}

	// b4Reconcile runs one Reconcile and returns the fresh Loop.
	b4Reconcile := func(r *LoopReconciler, ns, name string) *coxv1alpha1.Loop {
		_, err := r.Reconcile(context.Background(),
			reconcile.Request{NamespacedName: types.NamespacedName{Name: name, Namespace: ns}})
		Expect(err).NotTo(HaveOccurred())
		loop := &coxv1alpha1.Loop{}
		Expect(k8sClient.Get(context.Background(), types.NamespacedName{Name: name, Namespace: ns}, loop)).To(Succeed())
		return loop
	}

	It("onVerifyFail=Plan: a failed verify goes to PLANNING (not Implementing), iteration increments, and the failure summary is DURABLE on status.verify.lastFailureSummary", func() {
		ns := "b4-plan-" + nowSuffix()
		Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
		defer func() {
			_ = k8sClient.Delete(context.Background(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})
		}()

		name := "b4-plan"
		Expect(k8sClient.Create(ctx, &coxv1alpha1.Loop{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec:       b4LoopSpec(""),
		})).To(Succeed())

		r := s5aDriveToVerifying(ns, name)
		loop := b4Reconcile(r, ns, name)
		Expect(loop.Status.Phase).To(Equal(coxv1alpha1.LoopPhaseVerifying))

		jobName := verifyJobName(&coxv1alpha1.Loop{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}})
		b4FailingPod(ctx, ns, name, jobName)

		// The verify is terminal (check-0 terminated non-zero). The operator
		// reads the outcome and applies the onVerifyFail=Plan action: back to
		// PLANNING (not Implementing), iteration incremented (a re-plan is not
		// a reset), and the failure summary made DURABLE on
		// status.verify.lastFailureSummary.
		loop = b4Reconcile(r, ns, name)
		Expect(loop.Status.Phase).To(Equal(coxv1alpha1.LoopPhasePlanning),
			"onVerifyFail=Plan: a failed verify goes to PLANNING (not Implementing)")
		Expect(loop.Status.DesiredPhase).To(Equal(coxv1alpha1.LoopPhasePlanning))
		Expect(loop.Status.Iteration).To(Equal(2),
			"the iteration increments on the re-plan (a re-plan is not a reset)")
		Expect(loop.Status.CurrentVerify).To(BeNil(),
			"the pin clears on the re-plan (the next run re-pins)")
		// The failure summary is DURABLE on status.verify.lastFailureSummary
		// (the operator's record; the phase-init reads it to write
		// .coxswain/plan-feedback). It names the failing check and its exit
		// code, and includes the check's output tail.
		Expect(loop.Status.Verify).ToNot(BeNil(),
			"status.verify is set (the verify evidence record)")
		Expect(loop.Status.Verify.LastFailureSummary).To(ContainSubstring("check-0"),
			"the failure summary names the failing check")
		Expect(loop.Status.Verify.LastFailureSummary).To(ContainSubstring("exit 1"),
			"the failure summary names the check's exit code")
		Expect(loop.Status.Verify.LastFailureSummary).To(ContainSubstring("expected file /workspace/out.txt"),
			"the failure summary includes the check's output tail (the terminationMessage)")
		// The StallEntry is appended (the stall count spans re-plans — the
		// entry is in status.stallHistory, so the stall detector's history
		// includes this re-plan's failure).
		Expect(loop.Status.StallHistory).ToNot(BeEmpty(),
			"the StallEntry is appended on the re-plan (the stall count spans re-plans)")
	})

	It("onVerifyFail default (empty): a failed verify goes to IMPLEMENTING (the alpha behaviour, unchanged), and the failure summary is NOT set", func() {
		ns := "b4-default-" + nowSuffix()
		Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
		defer func() {
			_ = k8sClient.Delete(context.Background(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})
		}()

		name := "b4-default"
		// onVerifyFail is UNSET (the CRD default = Implement). The spec uses
		// the s5aLoopSpec (which does not set OnVerifyFail) so the CRD
		// default applies.
		Expect(k8sClient.Create(ctx, &coxv1alpha1.Loop{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec:       s5aLoopSpec(),
		})).To(Succeed())

		r := s5aDriveToVerifying(ns, name)
		loop := b4Reconcile(r, ns, name)
		Expect(loop.Status.Phase).To(Equal(coxv1alpha1.LoopPhaseVerifying))

		jobName := verifyJobName(&coxv1alpha1.Loop{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}})
		b4FailingPod(ctx, ns, name, jobName)

		// The default (Implement) behaviour is unchanged: back to
		// IMPLEMENTING, iteration incremented. The failure summary is NOT set
		// (the default path does not write lastFailureSummary — the check's
		// name + exit code ride into status.progress instead, the alpha
		// behaviour).
		loop = b4Reconcile(r, ns, name)
		Expect(loop.Status.Phase).To(Equal(coxv1alpha1.LoopPhaseImplementing),
			"onVerifyFail default (empty): a failed verify goes to IMPLEMENTING (the alpha behaviour)")
		Expect(loop.Status.DesiredPhase).To(Equal(coxv1alpha1.LoopPhaseImplementing))
		Expect(loop.Status.Iteration).To(Equal(2),
			"the iteration increments on the iterate (the alpha behaviour)")
		// The failure summary is NOT set (the default path does not write
		// lastFailureSummary).
		if loop.Status.Verify != nil {
			Expect(loop.Status.Verify.LastFailureSummary).To(BeEmpty(),
				"the failure summary is NOT set on the default (Implement) path")
		}
	})

	// F38 (gates P1-1): the re-plan must re-hold for approval WHETHER OR NOT the
	// runner reports planChanged. The B4 re-plan transition itself clears the
	// stale PlanApproved; the planChanged clear (now before recordPhaseClaim)
	// is the second line. planChanged=false is the case that isolates the B4
	// clear (a runner that does not signal a change must still not advance).
	for _, replanPlanChanged := range []bool{true, false} {
		It(fmt.Sprintf("onVerifyFail=Plan + approval.mode=Manual (claim planChanged=%v): the re-plan clears PlanApproved and re-holds at AwaitingApproval; the agent cannot run", replanPlanChanged), func() {
			ns := "b4-manual-" + nowSuffix()
			Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
			defer func() {
				_ = k8sClient.Delete(context.Background(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})
			}()

			name := "b4-manual"
			Expect(k8sClient.Create(ctx, &coxv1alpha1.Loop{
				ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
				Spec:       b4LoopSpec(coxv1alpha1.ApprovalModeManual),
			})).To(Succeed())

			// Wire the approval gate (the OS8 seam: the S4 default is
			// autoApprovePhaseGate; B3 replaces it with the approval gate so the
			// Manual-mode hold is exercised).
			r := &LoopReconciler{CNIProber: testCNIProber(),
				Client:    k8sClient,
				Scheme:    k8sClient.Scheme(),
				apiReader: k8sClient,
			}
			r.wireApprovalGate()
			r.readPhaseClaim = func(_ context.Context, _ *coxv1alpha1.Loop) (*PhaseClaim, error) {
				return nil, nil
			}

			// Drive to Verifying: Pending -> Planning (bootstrap) -> claim ->
			// Implementing -> claim (headCommit) -> Verifying. In Manual mode,
			// the Planning claim moves the phase to AwaitingApproval (the gate's
			// hold), so the spec must approve the plan first to advance to
			// Implementing, then the Implementing claim advances to Verifying.
			//
			// Step 1: bootstrap (Pending -> Planning).
			b4Reconcile(r, ns, name)
			// Pin status.baseCommit (the tamper diff needs it).
			loop := &coxv1alpha1.Loop{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, loop)).To(Succeed())
			loop.Status.BaseCommit = s5aBaseCommit
			Expect(k8sClient.Status().Update(ctx, loop)).To(Succeed())

			// Step 2: the Planning claim (the runner executed Planning). In
			// Manual mode, the gate holds the advance — the phase moves to
			// AwaitingApproval (the hold re-enters).
			r.readPhaseClaim = func(_ context.Context, _ *coxv1alpha1.Loop) (*PhaseClaim, error) {
				return &PhaseClaim{
					ObservedPhase: coxv1alpha1.LoopPhasePlanning,
					Status:        "success",
					HeadCommit:    s5aHeadCommit,
				}, nil
			}
			b4Reconcile(r, ns, name)
			loop = &coxv1alpha1.Loop{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, loop)).To(Succeed())
			Expect(loop.Status.Phase).To(Equal(coxv1alpha1.LoopPhaseAwaitingApproval),
				"Manual mode: the Planning claim moves the phase to AwaitingApproval (the hold re-enters)")
			// Re-arm the claim reader to (nil, nil) (agent running).
			r.readPhaseClaim = func(_ context.Context, _ *coxv1alpha1.Loop) (*PhaseClaim, error) {
				return nil, nil
			}

			// Step 3: the user approves the plan (the PlanApproved condition is
			// set with a valid hash). The gate's approvalPhaseGate sees the
			// condition present and allows the Planning -> Implementing advance.
			// But the phase is AwaitingApproval (not Planning), so the gate's
			// applyApprovalGate (the AwaitingApproval branch) advances it to
			// Implementing.
			setCondition := func(ns, name string, condType string, status string, message string) {
				nn := types.NamespacedName{Name: name, Namespace: ns}
				l := &coxv1alpha1.Loop{}
				Expect(k8sClient.Get(ctx, nn, l)).To(Succeed())
				now := metav1.Now()
				l.Status.Conditions = append(l.Status.Conditions, metav1.Condition{
					Type:               condType,
					Status:             metav1.ConditionStatus(status),
					Reason:             condType,
					Message:            message,
					LastTransitionTime: now,
				})
				Expect(k8sClient.Status().Update(ctx, l)).To(Succeed())
			}
			setCondition(ns, name, coxv1alpha1.PlanApprovedCondition, "True",
				"Approved: 1234567890abcdef1234567890abcdef1234567890abcdef1234567890abcdef")
			b4Reconcile(r, ns, name)
			loop = &coxv1alpha1.Loop{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, loop)).To(Succeed())
			Expect(loop.Status.Phase).To(Equal(coxv1alpha1.LoopPhaseImplementing),
				"Manual mode: the PlanApproved condition releases the hold (the phase advanced to Implementing)")

			// Step 4: the Implementing claim (with the headCommit — the operator
			// pins it on the Implementing -> Verifying advance).
			r.readPhaseClaim = func(_ context.Context, _ *coxv1alpha1.Loop) (*PhaseClaim, error) {
				return &PhaseClaim{
					ObservedPhase: coxv1alpha1.LoopPhaseImplementing,
					Status:        "success",
					HeadCommit:    s5aHeadCommit,
				}, nil
			}
			b4Reconcile(r, ns, name)
			loop = &coxv1alpha1.Loop{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, loop)).To(Succeed())
			Expect(loop.Status.Phase).To(Equal(coxv1alpha1.LoopPhaseVerifying),
				"the Implementing claim advances to Verifying")
			// D61 (I90): the verify-egress hold requires the egress proxy pod to be
			// Ready before the verify Job is created (external repo). Drive the
			// operator's pod Ready so the verify Job is created for the failing-pod
			// evidence below.
			d61ReadyVerifyProxy(r, ns, name)
			// Re-arm the claim reader to (nil, nil) (agent running).
			r.readPhaseClaim = func(_ context.Context, _ *coxv1alpha1.Loop) (*PhaseClaim, error) {
				return nil, nil
			}

			// Step 5: the verify Job pod with check-0 FAILED (exit 1). The
			// onVerifyFail=Plan action fires: back to PLANNING. The re-plan goes
			// to PLANNING (not Implementing) — the re-plan does NOT skip the plan
			// approval gate. In Manual mode, the NEXT Planning claim (once the
			// runner writes a new plan with a DIFFERENT hash) triggers the B3
			// plan-change gate's re-hold (the PlanChanged signal clears the
			// PlanApproved condition, and the gate's hold re-enters on the next
			// reconcile). If the runner writes the SAME plan (same hash), the
			// approval is still valid and the Loop advances directly to Implementing
			// (no re-hold). This spec asserts the B4 concern: the re-plan goes to
			// Planning (not Implementing), and the PlanChanged signal clears
			// PlanApproved (the re-hold mechanism, when the plan hash differs).
			jobName := verifyJobName(&coxv1alpha1.Loop{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}})
			b4FailingPod(ctx, ns, name, jobName)
			loop = b4Reconcile(r, ns, name)
			Expect(loop.Status.Phase).To(Equal(coxv1alpha1.LoopPhasePlanning),
				"onVerifyFail=Plan + Manual: the failed verify goes to PLANNING (the re-plan)")
			Expect(loop.Status.Iteration).To(Equal(2),
				"the iteration increments on the re-plan (a re-plan is not a reset)")
			Expect(findCondition(loop, coxv1alpha1.PlanApprovedCondition)).To(BeNil(),
				"F38: the B4 re-plan transition clears the stale approval")

			// Step 6: the runner's NEXT Planning claim (the re-plan runs, the
			// runner writes a new PLAN.md). In Manual mode, the re-plan goes to
			// PLANNING (the phase is Planning, not Implementing — the re-plan does
			// NOT skip the plan approval gate). The plan-change gate (B3) clears
			// the PlanApproved condition on the PlanChanged signal (the re-hold),
			// and the gate's hold re-enters on the NEXT reconcile (the B3
			// plan-change mechanism, tested by the B3 specs). This spec asserts
			// the B4 concern: the re-plan goes to Planning (not Implementing), the
			// iteration increments, and the failure summary is set. The
			// AwaitingApproval hold is a B3 concern (the plan-change gate).
			replanIter := loop.Status.Iteration
			replanAttempt := loop.Status.PlanAttempt
			r.readPhaseClaim = func(_ context.Context, _ *coxv1alpha1.Loop) (*PhaseClaim, error) {
				return &PhaseClaim{
					ObservedPhase: coxv1alpha1.LoopPhasePlanning,
					Status:        "success",
					Iteration:     replanIter,
					HeadCommit:    s5aHeadCommit,
					PlanAttempt:   replanAttempt,
					PlanChanged:   replanPlanChanged,
				}, nil
			}
			b4Reconcile(r, ns, name)
			loop = &coxv1alpha1.Loop{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, loop)).To(Succeed())
			// F38: the re-plan's Planning claim must NOT advance to Implementing:
			// the approval was cleared, so the gate denies and the Manual hold
			// re-enters. (The previous version of this spec asserted only that the
			// approval was cleared and accepted the advance to Implementing, which
			// codified the bug: an unapproved re-plan was implemented.)
			Expect(loop.Status.Phase).To(Equal(coxv1alpha1.LoopPhaseAwaitingApproval),
				"F38: an unapproved re-plan holds at AwaitingApproval")
			Expect(loop.Status.DesiredPhase).NotTo(Equal(coxv1alpha1.LoopPhaseImplementing),
				"F38: the agent must not be sent to Implementing before the re-plan is approved")
			Expect(findCondition(loop, coxv1alpha1.PlanApprovedCondition)).To(BeNil(),
				"the stale approval stays cleared")
			// A further reconcile with no approval keeps the hold.
			r.readPhaseClaim = func(_ context.Context, _ *coxv1alpha1.Loop) (*PhaseClaim, error) {
				return nil, nil
			}
			loop = b4Reconcile(r, ns, name)
			Expect(loop.Status.Phase).To(Equal(coxv1alpha1.LoopPhaseAwaitingApproval))
			Expect(loop.Status.DesiredPhase).NotTo(Equal(coxv1alpha1.LoopPhaseImplementing))
			// Re-arm the claim reader to (nil, nil) (agent running).
			r.readPhaseClaim = func(_ context.Context, _ *coxv1alpha1.Loop) (*PhaseClaim, error) {
				return nil, nil
			}
		})
	}

	// F38 (review of #214): the PlanChanged clear must run BEFORE
	// recordPhaseClaim. On the B4 path the re-plan clear already ran, so only
	// this case isolates the reorder: a plan-change claim lands while an
	// approval is still set (the user re-approved during the re-plan window).
	// With the clear AFTER recordPhaseClaim the gate reads the stale hash and
	// allows Planning -> Implementing.
	It("a PlanChanged Planning claim while an approval is set re-holds at AwaitingApproval (the clear runs before the gate) and emits PlanChangedReHold", func() {
		ns := "b4-planchanged-" + nowSuffix()
		Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
		defer func() {
			_ = k8sClient.Delete(context.Background(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})
		}()
		name := "b4-planchanged"
		Expect(k8sClient.Create(ctx, &coxv1alpha1.Loop{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec:       b4LoopSpec(coxv1alpha1.ApprovalModeManual),
		})).To(Succeed())
		rec := record.NewFakeRecorder(20)
		r := &LoopReconciler{CNIProber: testCNIProber(), Client: k8sClient, Scheme: k8sClient.Scheme(), apiReader: k8sClient, Recorder: rec}
		r.wireApprovalGate()
		r.readPhaseClaim = func(_ context.Context, _ *coxv1alpha1.Loop) (*PhaseClaim, error) { return nil, nil }

		loop := b4Reconcile(r, ns, name)
		Expect(loop.Status.Phase).To(Equal(coxv1alpha1.LoopPhasePlanning))
		loop.Status.Conditions = append(loop.Status.Conditions, metav1.Condition{
			Type: coxv1alpha1.PlanApprovedCondition, Status: metav1.ConditionTrue, Reason: "Approved",
			Message:            "Approved: 1234567890abcdef1234567890abcdef1234567890abcdef1234567890abcdef",
			LastTransitionTime: metav1.Now()})
		Expect(k8sClient.Status().Update(ctx, loop)).To(Succeed())

		attempt := loop.Status.PlanAttempt
		r.readPhaseClaim = func(_ context.Context, _ *coxv1alpha1.Loop) (*PhaseClaim, error) {
			return &PhaseClaim{
				ObservedPhase: coxv1alpha1.LoopPhasePlanning,
				Status:        "success",
				HeadCommit:    s5aHeadCommit,
				PlanAttempt:   attempt,
				PlanChanged:   true,
			}, nil
		}
		loop = b4Reconcile(r, ns, name)
		Expect(loop.Status.Phase).To(Equal(coxv1alpha1.LoopPhaseAwaitingApproval),
			"the changed plan must be re-approved before Implementing")
		Expect(loop.Status.DesiredPhase).NotTo(Equal(coxv1alpha1.LoopPhaseImplementing))
		Expect(findCondition(loop, coxv1alpha1.PlanApprovedCondition)).To(BeNil())
		var events []string
		for len(rec.Events) > 0 {
			events = append(events, <-rec.Events)
		}
		Expect(events).To(ContainElement(ContainSubstring(planChangedReHoldReason)))
	})

	// F38 (gates P2-1): a rejection while the phase is Planning must clear the
	// stale approval and bump planAttempt, so a stale Planning success claim
	// already on the pod (carrying the old planAttempt) is ignored instead of
	// being consumed (which moved the Loop to AwaitingApproval without a new
	// plan and absorbed the rejection).
	It("a rejection while Planning clears PlanApproved, bumps planAttempt, and a stale Planning claim does not advance the Loop", func() {
		ns := "b4-rejplanning-" + nowSuffix()
		Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
		defer func() {
			_ = k8sClient.Delete(context.Background(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})
		}()
		name := "b4-rejplanning"
		Expect(k8sClient.Create(ctx, &coxv1alpha1.Loop{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec:       b4LoopSpec(coxv1alpha1.ApprovalModeManual),
		})).To(Succeed())
		r := &LoopReconciler{CNIProber: testCNIProber(), Client: k8sClient, Scheme: k8sClient.Scheme(), apiReader: k8sClient}
		r.wireApprovalGate()
		r.readPhaseClaim = func(_ context.Context, _ *coxv1alpha1.Loop) (*PhaseClaim, error) { return nil, nil }

		loop := b4Reconcile(r, ns, name)
		Expect(loop.Status.Phase).To(Equal(coxv1alpha1.LoopPhasePlanning))
		attemptBefore := loop.Status.PlanAttempt

		// A pending approval from a prior cycle, and a rejection while Planning.
		now := metav1.Now()
		loop.Status.Conditions = append(loop.Status.Conditions,
			metav1.Condition{Type: coxv1alpha1.PlanApprovedCondition, Status: metav1.ConditionTrue, Reason: "Approved",
				Message: "Approved: 1234567890abcdef1234567890abcdef1234567890abcdef1234567890abcdef", LastTransitionTime: now},
			metav1.Condition{Type: coxv1alpha1.PlanRejectedCondition, Status: metav1.ConditionTrue, Reason: "Rejected",
				Message: "use a different approach", LastTransitionTime: now})
		Expect(k8sClient.Status().Update(ctx, loop)).To(Succeed())

		// The stale claim: a Planning success claim for the PRIOR attempt.
		r.readPhaseClaim = func(_ context.Context, _ *coxv1alpha1.Loop) (*PhaseClaim, error) {
			return &PhaseClaim{
				ObservedPhase: coxv1alpha1.LoopPhasePlanning,
				Status:        "success",
				HeadCommit:    s5aHeadCommit,
				PlanAttempt:   attemptBefore,
			}, nil
		}
		loop = b4Reconcile(r, ns, name)
		Expect(loop.Status.Phase).To(Equal(coxv1alpha1.LoopPhasePlanning),
			"the stale claim must not advance a Loop whose plan was just rejected")
		Expect(loop.Status.PlanAttempt).To(Equal(attemptBefore+1), "the rejection starts a new plan attempt")
		Expect(findCondition(loop, coxv1alpha1.PlanApprovedCondition)).To(BeNil(),
			"the stale approval is cleared")
		Expect(loop.Status.Approval).NotTo(BeNil())
		Expect(loop.Status.Approval.LastRejectionReason).To(Equal("use a different approach"))
	})

	// The maxIterations cap spec is OMITTED: the cap check is in the shared
	// verifyIterate branch of applyVerifyOutcome (the increment is the same
	// for both the re-plan and the iterate paths), so the existing S5a cap
	// specs (loop_verify_job_envtest_test.go) already cover it. A re-plan does
	// not bypass the cap because the cap check is path-agnostic (it reads
	// loop.Status.Iteration and spec.Loop.MaxIterations, not the onVerifyFail
	// action).

	It("an in-progress verify (check-0 still Running) makes NO decision (no phase change, no iteration increment, no summary written)", func() {
		ns := "b4-inprog-" + nowSuffix()
		Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
		defer func() {
			_ = k8sClient.Delete(context.Background(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})
		}()

		name := "b4-inprog"
		Expect(k8sClient.Create(ctx, &coxv1alpha1.Loop{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec:       b4LoopSpec(""),
		})).To(Succeed())

		r := s5aDriveToVerifying(ns, name)
		loop := b4Reconcile(r, ns, name)
		Expect(loop.Status.Phase).To(Equal(coxv1alpha1.LoopPhaseVerifying))
		Expect(loop.Status.Iteration).To(Equal(0), "the iteration is 0 before the verify (the Loop's first cycle)")

		jobName := verifyJobName(&coxv1alpha1.Loop{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}})
		// The verify pod with check-0 STILL RUNNING (not terminated) — the
		// verify is IN PROGRESS. The operator must not make a decision:
		// no phase change, no iteration increment, no summary written.
		b4InProgPod(ctx, ns, name, jobName)
		loop = b4Reconcile(r, ns, name)
		Expect(loop.Status.Phase).To(Equal(coxv1alpha1.LoopPhaseVerifying),
			"an in-progress verify makes NO phase change (the operator requeues)")
		Expect(loop.Status.Iteration).To(Equal(0),
			"an in-progress verify does NOT increment the iteration")
		// The failure summary is NOT written (the verify is not terminal).
		if loop.Status.Verify != nil {
			Expect(loop.Status.Verify.LastFailureSummary).To(BeEmpty(),
				"an in-progress verify does NOT write the failure summary")
		}
	})

	It("reject after a failed verify: the plan-feedback is the REJECT reason (not the stale failure summary)", func() {
		ns := "b4-rej-" + nowSuffix()
		Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
		defer func() {
			_ = k8sClient.Delete(context.Background(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})
		}()

		name := "b4-rej"
		Expect(k8sClient.Create(ctx, &coxv1alpha1.Loop{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec:       b4LoopSpec(coxv1alpha1.ApprovalModeManual),
		})).To(Succeed())

		// Drive to Verifying in Manual mode (the s5aDriveToVerifying helper
		// assumes Auto mode; Manual mode holds at AwaitingApproval after the
		// Planning claim, so the spec must approve the plan to advance).
		r := &LoopReconciler{CNIProber: testCNIProber(), Client: k8sClient, Scheme: k8sClient.Scheme(), apiReader: k8sClient}
		r.wireApprovalGate()
		r.readPhaseClaim = func(_ context.Context, _ *coxv1alpha1.Loop) (*PhaseClaim, error) {
			return nil, nil
		}
		s5aEnsureSandbox(ns, name)
		// Bootstrap: Pending -> Planning.
		b4Reconcile(r, ns, name)
		// Pin status.baseCommit.
		loop := &coxv1alpha1.Loop{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, loop)).To(Succeed())
		loop.Status.BaseCommit = s5aBaseCommit
		Expect(k8sClient.Status().Update(ctx, loop)).To(Succeed())
		// Planning claim -> AwaitingApproval (Manual hold).
		r.readPhaseClaim = func(_ context.Context, _ *coxv1alpha1.Loop) (*PhaseClaim, error) {
			return &PhaseClaim{
				ObservedPhase: coxv1alpha1.LoopPhasePlanning,
				Status:        "success",
				HeadCommit:    s5aHeadCommit,
			}, nil
		}
		b4Reconcile(r, ns, name)
		r.readPhaseClaim = func(_ context.Context, _ *coxv1alpha1.Loop) (*PhaseClaim, error) {
			return nil, nil
		}
		// Set PlanApproved (the user approves the plan).
		loop = &coxv1alpha1.Loop{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, loop)).To(Succeed())
		now := metav1.Now()
		loop.Status.Conditions = append(loop.Status.Conditions, metav1.Condition{
			Type:               coxv1alpha1.PlanApprovedCondition,
			Status:             metav1.ConditionTrue,
			Reason:             "Approved",
			Message:            "1234567890abcdef1234567890abcdef1234567890abcdef1234567890abcdef",
			LastTransitionTime: now,
		})
		Expect(k8sClient.Status().Update(ctx, loop)).To(Succeed())
		b4Reconcile(r, ns, name)
		// Implementing claim -> Verifying.
		s5aEnsureSandbox(ns, name)
		r.readPhaseClaim = func(_ context.Context, _ *coxv1alpha1.Loop) (*PhaseClaim, error) {
			return &PhaseClaim{
				ObservedPhase: coxv1alpha1.LoopPhaseImplementing,
				Status:        "success",
				Iteration:     0,
				HeadCommit:    s5aHeadCommit,
			}, nil
		}
		b4Reconcile(r, ns, name)
		r.readPhaseClaim = func(_ context.Context, _ *coxv1alpha1.Loop) (*PhaseClaim, error) {
			return nil, nil
		}
		loop = b4Reconcile(r, ns, name)
		Expect(loop.Status.Phase).To(Equal(coxv1alpha1.LoopPhaseVerifying),
			"Manual mode: the Loop is at Verifying (the plan was approved)")
		// D61 (I90): the verify-egress hold requires the egress proxy pod to be
		// Ready before the verify Job is created (external repo). Drive the
		// operator's pod Ready so the verify Job is created for the failing-pod
		// evidence below.
		d61ReadyVerifyProxy(r, ns, name)

		// Failed verify -> re-plan (Planning). The failure summary is set.
		jobName := verifyJobName(&coxv1alpha1.Loop{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}})
		b4FailingPod(ctx, ns, name, jobName)
		loop = b4Reconcile(r, ns, name)
		Expect(loop.Status.Phase).To(Equal(coxv1alpha1.LoopPhasePlanning))
		Expect(loop.Status.Verify).NotTo(BeNil())
		Expect(loop.Status.Verify.LastFailureSummary).To(ContainSubstring("check-0"),
			"the failure summary is set on the re-plan")

		// The user rejects the plan (the PlanRejected condition is set with a
		// reason). The operator's approval gate (the AwaitingApproval or
		// Planning branch) sets lastRejectionReason AND clears
		// lastFailureSummary (the rejection reason takes over as the planning
		// feedback).
		// Simulate the rejection: set the PlanRejected condition on the Loop.
		loop = &coxv1alpha1.Loop{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, loop)).To(Succeed())
		now = metav1.Now()
		loop.Status.Conditions = append(loop.Status.Conditions, metav1.Condition{
			Type:               coxv1alpha1.PlanRejectedCondition,
			Status:             metav1.ConditionTrue,
			Reason:             "Rejected",
			Message:            "rejected: the plan is wrong",
			LastTransitionTime: now,
		})
		Expect(k8sClient.Status().Update(ctx, loop)).To(Succeed())

		// The approval gate runs (the operator's applyApprovalGate). The
		// Planning branch sees the PlanRejected condition, sets
		// lastRejectionReason, and clears lastFailureSummary.
		_ = b4Reconcile(r, ns, name)
		loop = &coxv1alpha1.Loop{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, loop)).To(Succeed())
		// The rejection reason is now durable on status.approval.
		Expect(loop.Status.Approval).NotTo(BeNil())
		Expect(loop.Status.Approval.LastRejectionReason).To(Equal("rejected: the plan is wrong"),
			"the rejection reason is durable")
		// The failure summary is CLEARED (the rejection reason takes over).
		Expect(loop.Status.Verify).NotTo(BeNil())
		Expect(loop.Status.Verify.LastFailureSummary).To(BeEmpty(),
			"the failure summary is cleared after the rejection (the reject reason takes over)")
		// planApprovalFiles returns the rejection reason (not the stale summary).
		planHash, planFeedback := r.planApprovalFiles(loop)
		_ = planHash
		Expect(planFeedback).To(Equal("rejected: the plan is wrong"),
			"the plan-feedback is the REJECT reason (not the stale failure summary)")
	})

	It("failed verify then passing verify: a later Planning has EMPTY feedback (the summary is cleared on pass)", func() {
		ns := "b4-pass-" + nowSuffix()
		Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
		defer func() {
			_ = k8sClient.Delete(context.Background(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})
		}()

		name := "b4-pass"
		Expect(k8sClient.Create(ctx, &coxv1alpha1.Loop{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec:       b4LoopSpec(""),
		})).To(Succeed())

		r := s5aDriveToVerifying(ns, name)
		loop := b4Reconcile(r, ns, name)
		Expect(loop.Status.Phase).To(Equal(coxv1alpha1.LoopPhaseVerifying))

		// First verify FAILS -> re-plan (Planning). The failure summary is set.
		jobName := verifyJobName(&coxv1alpha1.Loop{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}})
		b4FailingPod(ctx, ns, name, jobName)
		loop = b4Reconcile(r, ns, name)
		Expect(loop.Status.Phase).To(Equal(coxv1alpha1.LoopPhasePlanning))
		Expect(loop.Status.Verify).NotTo(BeNil())
		Expect(loop.Status.Verify.LastFailureSummary).To(ContainSubstring("check-0"),
			"the failure summary is set on the first re-plan")

		// Drive the Loop back through the re-plan cycle to a SECOND verify:
		// Planning -> Implementing (claim) -> Verifying (claim with headCommit).
		replanIter := loop.Status.Iteration
		replanAttempt := loop.Status.PlanAttempt
		s5aEnsureSandbox(ns, name)
		r.readPhaseClaim = func(_ context.Context, _ *coxv1alpha1.Loop) (*PhaseClaim, error) {
			return &PhaseClaim{
				ObservedPhase: coxv1alpha1.LoopPhasePlanning,
				Status:        "success",
				Iteration:     replanIter,
				PlanAttempt:   replanAttempt,
			}, nil
		}
		b4Reconcile(r, ns, name)
		r.readPhaseClaim = func(_ context.Context, _ *coxv1alpha1.Loop) (*PhaseClaim, error) {
			return nil, nil
		}
		s5aEnsureSandbox(ns, name)
		r.readPhaseClaim = func(_ context.Context, _ *coxv1alpha1.Loop) (*PhaseClaim, error) {
			return &PhaseClaim{
				ObservedPhase: coxv1alpha1.LoopPhaseImplementing,
				Status:        "success",
				Iteration:     replanIter,
				HeadCommit:    s5aHeadCommit,
			}, nil
		}
		b4Reconcile(r, ns, name)
		loop = &coxv1alpha1.Loop{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, loop)).To(Succeed())
		Expect(loop.Status.Phase).To(Equal(coxv1alpha1.LoopPhaseVerifying),
			"the re-plan advanced back to Verifying (the second verify)")

		// Second verify PASSES (check-0 exit 0). The operator's verifyOutcome
		// returns verifySucceeded. The failure summary is CLEARED (the
		// failure is resolved). The iteration is 2 after the re-plan, so the
		// verify Job name uses iteration 2 (verifyJobName clamps to min 1).
		oldPod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name + "-verify-pod", Namespace: ns}}
		Expect(k8sClient.Delete(ctx, oldPod)).To(Succeed())
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name + "-verify-pod", Namespace: ns}, &corev1.Pod{})).ToNot(Succeed())
		// The verify Job name for the CURRENT iteration (the re-plan
		// incremented it to 2). verifyJobName uses loop.Status.Iteration
		// (min 1), so we pass the actual loop (with status) to get the
		// correct name.
		jobName = verifyJobName(loop)
		// Create a PASSING verify pod (check-0 exit 0).
		s5aPassingPod(ctx, ns, name, jobName)
		loop = b4Reconcile(r, ns, name)
		Expect(loop.Status.Phase).To(Equal(coxv1alpha1.LoopPhaseSucceeded),
			"the passing verify advances to Succeeded")
		// The failure summary is CLEARED (the verify passed).
		Expect(loop.Status.Verify).NotTo(BeNil())
		Expect(loop.Status.Verify.LastFailureSummary).To(BeEmpty(),
			"the failure summary is cleared on a passing verify (the failure is resolved)")
		// A later Planning (if the Loop were re-created) would have EMPTY
		// feedback (no stale summary leaks into the planning prompt).
		loop.Status.Phase = coxv1alpha1.LoopPhasePlanning
		loop.Status.DesiredPhase = coxv1alpha1.LoopPhasePlanning
		planHash, planFeedback := r.planApprovalFiles(loop)
		_ = planHash
		Expect(planFeedback).To(BeEmpty(),
			"a later Planning has EMPTY feedback (the summary was cleared on pass)")
	})
})

// tamper TERMINATED (exit 0) but check-0 STILL RUNNING (not terminated) —
// the verify is IN PROGRESS (the outcome is not yet recorded). The operator
// must not make a decision (no phase change, no iteration increment, no
// summary written) while the verify is still running.
func b4InProgPod(ctx context.Context, ns, name, jobName string) {
	job := &batchv1.Job{}
	if err := k8sClient.Get(ctx, types.NamespacedName{Name: jobName, Namespace: ns}, job); err != nil {
		if apierrors.IsNotFound(err) {
			job = &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: jobName, Namespace: ns, Labels: verifyJobLabels(name)}}
			Expect(k8sClient.Create(ctx, job)).To(Succeed())
		} else {
			Expect(err).NotTo(HaveOccurred())
		}
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name + "-verify-pod",
			Namespace: ns,
			Labels:    map[string]string{s5aJobNameLabel: jobName, verifyForLabel: name},
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: batchv1.SchemeGroupVersion.String(),
				Kind:       i65JobKind,
				Name:       jobName,
				UID:        job.UID,
			}},
		},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: verifyNoopContainer, Image: verifyBusybox}}},
	}
	Expect(k8sClient.Create(ctx, pod)).To(Succeed())
	// check-0 is STILL RUNNING (State is not Terminated) — the verify is in
	// progress. The operator's verifyOutcome returns (verifyNoDecision,
	// requeue=true) for this pod, so applyVerifyOutcome requeues BEFORE it
	// reaches the verifyIterate branch.
	pod.Status.InitContainerStatuses = []corev1.ContainerStatus{
		{Name: s5aCloneBase, State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0}}},
		{Name: s5aImportAgent, State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0}}},
		{Name: s5aTamper, State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0}}},
		// check-0 is Running (not terminated): the verify is in progress.
		{Name: s5aCheck0, State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}},
	}
	Expect(k8sClient.Status().Update(ctx, pod)).To(Succeed())
}
