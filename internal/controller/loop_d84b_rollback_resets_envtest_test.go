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

// D84b (F77, decision D84(a) + D106): when a rollback completes
// (spec.restoreFrom: iter-N, the Restore condition becomes True/Restored),
// the operator resets the Loop's position to the restored content:
// status.iteration = N, phase = Verifying (with desiredPhase set to match),
// and the terminal Failed and Succeeded conditions are cleared. The restored
// tree is re-verified and then follows the normal flow.
//
// Today the phase and iteration SURVIVE a rollback (F77,
// docs/REVIEW-PHASE1-R32.md:118-121): a Loop at iteration 3 rolled back to
// iter-0 stays at iteration 3, a Failed or Succeeded Loop keeps its terminal
// phase and conditions (so it is stuck), and a Loop at the maxIterations
// ceiling rolled back to iter-0 fails instead of re-verifying from 0.
//
// The specs (envtest) reuse the B2 ROLLBACK (ready, level-based) setup and
// its b2 helpers (stop the sandbox -> delete the PVC -> recreate the PVC
// from the iter-0 snapshot), then assert the reset the operator must perform
// when the restore completes:
//
//   - (a) a Loop at iteration 3 in Implementing is rolled back to iter-0:
//     afterwards status.iteration == 0, phase == Verifying,
//     desiredPhase == Verifying.
//   - (b) a Failed Loop (terminal phase + a Failed=True condition) is rolled
//     back: afterwards the phase is Verifying and there is no Failed=True
//     condition.
//   - (c) a Succeeded Loop is rolled back: afterwards the phase is Verifying
//     and there is no Succeeded=True condition.
//   - (d) a Loop at status.iteration == spec.loop.maxIterations is rolled
//     back to iter-0: afterwards it is NOT Failed (iteration is 0 and the
//     phase is Verifying).
//   - (e) in-progress: the restore has NOT completed yet (the sandbox is
//     still stopping — the level-based sequence's first step). The phase and
//     iteration are UNCHANGED (no decision while in progress, AGENTS I49).

package controller

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	sandboxv1beta1 "sigs.k8s.io/agent-sandbox/api/v1beta1"

	coxv1alpha1 "github.com/papawattu/coxswain/api/v1alpha1"
)

// d84bTrueCondition returns the first Loop condition with the given type and
// status True (nil when absent) — the assertion target for "no terminal
// condition remains".
func d84bTrueCondition(loop *coxv1alpha1.Loop, condType string) *metav1.Condition {
	for i := range loop.Status.Conditions {
		c := loop.Status.Conditions[i]
		if c.Type == condType && c.Status == metav1.ConditionTrue {
			return &c
		}
	}
	return nil
}

// d84bSetupRollback creates a namespace + Loop, drives it to a successful
// verify at iter-0 (the B1 idiom, checkpoint recorded and the sandbox pod
// present), makes the iter-0 snapshot ReadyToUse, sets
// spec.restoreFrom=iter-0, and returns everything the rollback specs need.
func d84bSetupRollback(ctx context.Context) (ns, name string, r *LoopReconciler) {
	ns = "d84b-" + nowSuffix()
	Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
	name = "d84b-loop"
	Expect(k8sClient.Create(ctx, &coxv1alpha1.Loop{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec:       b2LoopSpec(),
	})).To(Succeed())

	r, _ = b2NewReconcilerWithRecorder()
	// The sandbox is RUNNING with its pod present (the rollback case: a
	// mounted PVC is held by pvc-protection — the restore must stop the
	// sandbox first, like the B2 ready-rollback spec's setup).
	b2EnsureSandbox(ns, name)
	b2EnsureSandboxPod(ns, name)
	b1DriveToSucceeded(ctx, r, ns, name)
	snapName := checkpointName(&coxv1alpha1.Loop{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}}, 0)
	b2MakeSnapshotReady(ctx, ns, snapName)

	l := &coxv1alpha1.Loop{}
	Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, l)).To(Succeed())
	l.Spec.RestoreFrom = "iter-0"
	Expect(k8sClient.Update(ctx, l)).To(Succeed())
	return ns, name, r
}

// d84bSetTerminalCondition appends a terminal True condition of the given type
// (with a lastTransitionTime, so the status subresource's validation accepts
// it) to the Loop's conditions.
func d84bSetTerminalCondition(l *coxv1alpha1.Loop, condType string) {
	now := metav1.Now()
	l.Status.Conditions = append(l.Status.Conditions, metav1.Condition{
		Type:               condType,
		Status:             metav1.ConditionTrue,
		Reason:             "D84bTestTerminal",
		Message:            "terminal record (test fixture)",
		LastTransitionTime: now,
	})
}

// d84bStatusUpdate gets the Loop, applies mutate, and status-updates it.
func d84bStatusUpdate(ctx context.Context, ns, name string, mutate func(*coxv1alpha1.Loop)) {
	l := &coxv1alpha1.Loop{}
	Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, l)).To(Succeed())
	mutate(l)
	Expect(k8sClient.Status().Update(ctx, l)).To(Succeed())
}

// d84bGetLoop returns the fresh Loop.
func d84bGetLoop(ctx context.Context, ns, name string) *coxv1alpha1.Loop {
	l := &coxv1alpha1.Loop{}
	Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, l)).To(Succeed())
	return l
}

// d84bCompleteRollback runs the level-based sequence to completion (stop the
// sandbox, the pod goes, delete the PVC, the finalizer goes, recreate the
// PVC from the snapshot) and returns the fresh Loop (the Restore condition
// must be True reason Restored — the rollback's one-shot marker, the same
// shape the B2 ready-rollback spec asserts at its final step). The manual
// cluster writes just need a live context (context.Background()).
func d84bCompleteRollback(r *LoopReconciler, ns, name string) *coxv1alpha1.Loop {
	b2Reconcile(r, ns, name)                           // step 1: stop the sandbox
	b2DeleteSandboxPod(context.Background(), ns, name) // the agent-sandbox controller removes the pod
	b2Reconcile(r, ns, name)                           // step 2: delete the PVC
	b2RemovePVCProtectionFinalizer(context.Background(), ns, name)
	return b2Reconcile(r, ns, name) // step 3: recreate the PVC from the snapshot
}

var _ = Describe("D84b: a completed rollback re-verifies at the restored iteration", func() {
	var ctx context.Context

	BeforeEach(func() {
		ctx = context.Background()
		// The B1 checkpoint path (b1DriveToSucceeded) requires a VSC for the
		// workspace driver (the same cluster-scoped fixture the B2 suite's
		// BeforeEach creates; the B1 suite's AfterEach removes it, so this
		// recreates it when absent).
		vsc := &unstructured.Unstructured{Object: map[string]any{
			"apiVersion":     snapshotGV,
			"kind":           volumeSnapshotClassKind,
			"metadata":       map[string]any{"name": "b1-test-vsc"},
			"driver":         b1Driver,
			"deletionPolicy": "Delete",
		}}
		if err := k8sClient.Create(ctx, vsc); err != nil && !apierrors.IsAlreadyExists(err) {
			Expect(err).ToNot(HaveOccurred(), "create the VolumeSnapshotClass fixture")
		}
	})

	It("(a) a Loop at iteration 3 in Implementing is rolled back to iter-0: the operator sets status.iteration = 0 and phase = Verifying (desiredPhase too) — the restored tree is re-verified from the restored position", func() {
		ns, name, r := d84bSetupRollback(ctx)
		defer func() {
			_ = k8sClient.Delete(context.Background(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})
		}()

		// The rollback target state: iteration 3 in Implementing (the Loop
		// has been iterating since the iter-0 checkpoint the rollback
		// restores).
		d84bStatusUpdate(ctx, ns, name, func(l *coxv1alpha1.Loop) {
			l.Status.Iteration = 3
			l.Status.Phase = coxv1alpha1.LoopPhaseImplementing
			l.Status.DesiredPhase = coxv1alpha1.LoopPhaseImplementing
		})

		after := d84bCompleteRollback(r, ns, name)

		By("completing the rollback (the Restore condition is True reason Restored)")
		c := b2RestoreCondition(after)
		Expect(c).NotTo(BeNil(), "the rollback must complete (the one-shot marker)")
		Expect(c.Status).To(Equal(metav1.ConditionTrue))
		Expect(c.Reason).To(Equal(coxv1alpha1.RestoreReasonRestored))

		By("resetting the position to the restored iteration (D84b: the restored tree is re-verified from iteration 0)")
		Expect(after.Status.Iteration).To(Equal(0), "a completed rollback must set status.iteration to the restored iteration (today it survives at 3)")
		Expect(after.Status.Phase).To(Equal(coxv1alpha1.LoopPhaseVerifying), "a completed rollback must set the phase to Verifying (the restored tree is re-verified)")
		Expect(after.Status.DesiredPhase).To(Equal(coxv1alpha1.LoopPhaseVerifying), "the desiredPhase must match (the phase machine must not keep aiming at the pre-rollback phase)")
	})

	It("(b) a FAILED Loop is rolled back to iter-0: the phase is Verifying and the terminal Failed condition is cleared", func() {
		ns, name, r := d84bSetupRollback(ctx)
		defer func() {
			_ = k8sClient.Delete(context.Background(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})
		}()

		// The terminal Failed state (phase + desiredPhase + a Failed=True
		// condition, the terminal record).
		d84bStatusUpdate(ctx, ns, name, func(l *coxv1alpha1.Loop) {
			l.Status.Phase = coxv1alpha1.LoopPhaseFailed
			l.Status.DesiredPhase = coxv1alpha1.LoopPhaseFailed
			d84bSetTerminalCondition(l, "Failed")
		})

		after := d84bCompleteRollback(r, ns, name)

		By("completing the rollback (the Restore condition is True reason Restored)")
		c := b2RestoreCondition(after)
		Expect(c).NotTo(BeNil(), "the rollback must complete (the one-shot marker)")
		Expect(c.Status).To(Equal(metav1.ConditionTrue))
		Expect(c.Reason).To(Equal(coxv1alpha1.RestoreReasonRestored))

		By("clearing the terminal state (D84b: the restored tree is re-verified, not stuck Failed)")
		Expect(after.Status.Phase).To(Equal(coxv1alpha1.LoopPhaseVerifying), "a completed rollback must move a Failed Loop to Verifying (today the terminal phase survives)")
		Expect(after.Status.DesiredPhase).To(Equal(coxv1alpha1.LoopPhaseVerifying), "the desiredPhase must match")
		Expect(d84bTrueCondition(after, "Failed")).To(BeNil(), "the terminal Failed condition must be cleared (a leftover Failed=True would record a Loop as failed after the rollback restored it)")
	})

	It("(c) a SUCCEEDED Loop is rolled back to iter-0: the phase is Verifying and the terminal Succeeded condition is cleared", func() {
		ns, name, r := d84bSetupRollback(ctx)
		defer func() {
			_ = k8sClient.Delete(context.Background(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})
		}()

		// The terminal Succeeded state (b1DriveToSucceeded already left the
		// Loop Succeeded at iteration 1; a terminal Succeeded=True condition
		// is the terminal record).
		d84bStatusUpdate(ctx, ns, name, func(l *coxv1alpha1.Loop) {
			l.Status.Phase = coxv1alpha1.LoopPhaseSucceeded
			l.Status.DesiredPhase = coxv1alpha1.LoopPhaseSucceeded
			d84bSetTerminalCondition(l, "Succeeded")
		})

		after := d84bCompleteRollback(r, ns, name)

		By("completing the rollback (the Restore condition is True reason Restored)")
		c := b2RestoreCondition(after)
		Expect(c).NotTo(BeNil(), "the rollback must complete (the one-shot marker)")
		Expect(c.Status).To(Equal(metav1.ConditionTrue))
		Expect(c.Reason).To(Equal(coxv1alpha1.RestoreReasonRestored))

		By("clearing the terminal state (D84b: the restored tree is re-verified, not stuck Succeeded)")
		Expect(after.Status.Phase).To(Equal(coxv1alpha1.LoopPhaseVerifying), "a completed rollback must move a Succeeded Loop to Verifying (today the terminal phase survives)")
		Expect(after.Status.DesiredPhase).To(Equal(coxv1alpha1.LoopPhaseVerifying), "the desiredPhase must match")
		Expect(d84bTrueCondition(after, "Succeeded")).To(BeNil(), "the terminal Succeeded condition must be cleared (a leftover Succeeded=True would record a rolled-back Loop as done)")
	})

	It("(d) a Loop at the maxIterations ceiling is rolled back to iter-0: it is NOT Failed — the position resets (iteration 0, phase Verifying)", func() {
		ns, name, r := d84bSetupRollback(ctx)
		defer func() {
			_ = k8sClient.Delete(context.Background(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})
		}()

		// The ceiling state: spec.loop.maxIterations = 2, status.iteration =
		// 2 (the Loop has exhausted its budget at the last verify — today a
		// rollback from here would fail the Loop instead of re-verifying
		// from the restored 0).
		l := d84bGetLoop(ctx, ns, name)
		l.Spec.Loop = coxv1alpha1.LoopSettings{MaxIterations: 2}
		Expect(k8sClient.Update(ctx, l)).To(Succeed())
		d84bStatusUpdate(ctx, ns, name, func(l *coxv1alpha1.Loop) {
			l.Status.Iteration = 2
			l.Status.Phase = coxv1alpha1.LoopPhaseVerifying
			l.Status.DesiredPhase = coxv1alpha1.LoopPhaseVerifying
		})

		after := d84bCompleteRollback(r, ns, name)

		By("completing the rollback (the Restore condition is True reason Restored)")
		c := b2RestoreCondition(after)
		Expect(c).NotTo(BeNil(), "the rollback must complete (the one-shot marker)")
		Expect(c.Status).To(Equal(metav1.ConditionTrue))
		Expect(c.Reason).To(Equal(coxv1alpha1.RestoreReasonRestored))

		By("resetting the position (D84b: a rollback from the ceiling must not fail the Loop — the restored tree is re-verified from 0)")
		Expect(after.Status.Phase).NotTo(Equal(coxv1alpha1.LoopPhaseFailed), "a rollback from the maxIterations ceiling must NOT fail the Loop (today the ceiling survives the rollback and fails it)")
		Expect(after.Status.Iteration).To(Equal(0), "the iteration must reset to the restored iteration")
		Expect(after.Status.Phase).To(Equal(coxv1alpha1.LoopPhaseVerifying), "the phase must be Verifying (the restored tree is re-verified)")
	})

	It("(e) in-progress: while the rollback has NOT completed yet (the sandbox is still stopping), the phase and iteration are UNCHANGED (no decision while in progress, AGENTS I49)", func() {
		ns, name, r := d84bSetupRollback(ctx)
		defer func() {
			_ = k8sClient.Delete(context.Background(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})
		}()

		// The pre-rollback state: iteration 3 in Implementing (the same
		// position as (a), so the assertion is a direct before/after).
		d84bStatusUpdate(ctx, ns, name, func(l *coxv1alpha1.Loop) {
			l.Status.Iteration = 3
			l.Status.Phase = coxv1alpha1.LoopPhaseImplementing
			l.Status.DesiredPhase = coxv1alpha1.LoopPhaseImplementing
		})

		// ONE reconcile: the level-based sequence's first step (stop the
		// sandbox). The restore has NOT completed (the PVC is still present
		// and the snapshot not applied) — the B2 ready-rollback spec shows
		// this mid-sequence state.
		l1 := b2Reconcile(r, ns, name)

		By("stopping the sandbox (the in-progress state is VISIBLE — Restore False reason StoppingSandbox)")
		sb1 := &sandboxv1beta1.Sandbox{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: b2SandboxName(name), Namespace: ns}, sb1)).To(Succeed())
		Expect(sb1.Spec.OperatingMode).To(Equal(sandboxv1beta1.SandboxOperatingModeSuspended), "the restore's first step stops the sandbox before deleting its PVC")
		c1 := b2RestoreCondition(l1)
		Expect(c1).NotTo(BeNil(), "the in-progress state must be VISIBLE (the Restore condition is the durable record)")
		Expect(c1.Status).To(Equal(metav1.ConditionFalse))
		Expect(c1.Reason).To(Equal(coxv1alpha1.RestoreReasonStoppingSandbox))

		By("leaving the position UNCHANGED while the restore is in progress (AGENTS I49: no decision while in progress)")
		Expect(l1.Status.Iteration).To(Equal(3), "the iteration must NOT change before the restore completes")
		Expect(l1.Status.Phase).To(Equal(coxv1alpha1.LoopPhaseImplementing), "the phase must NOT change before the restore completes")
		Expect(l1.Status.DesiredPhase).To(Equal(coxv1alpha1.LoopPhaseImplementing), "the desiredPhase must NOT change before the restore completes")
	})

	It("(f) a LOST-PVC restore (not a rollback) leaves the phase and iteration alone — the D106 reset is rollback-only", func() {
		fns := "d84bf-" + nowSuffix()
		Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: fns}})).To(Succeed())
		DeferCleanup(func() {
			_ = k8sClient.Delete(context.Background(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: fns}})
		})
		b1CreateVSC()
		const name = "d84b-lostpvc"
		d84aLostPVC(fns, name, false, func(l *coxv1alpha1.Loop) {
			l.Status.Phase = coxv1alpha1.LoopPhaseImplementing
			l.Status.DesiredPhase = coxv1alpha1.LoopPhaseImplementing
			l.Status.Iteration = 3
		})
		after := &coxv1alpha1.Loop{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: fns}, after)).To(Succeed())
		c := b2RestoreCondition(after)
		Expect(c).NotTo(BeNil(), "the lost-PVC restore must complete")
		Expect(c.Reason).To(Equal(coxv1alpha1.RestoreReasonRestored))
		Expect(after.Status.Iteration).To(Equal(3), "a lost-PVC restore must not reset the iteration (D106 is rollback-only)")
		Expect(after.Status.Phase).To(Equal(coxv1alpha1.LoopPhaseImplementing), "a lost-PVC restore must not move the phase")
	})
})
