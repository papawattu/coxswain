package controller

// F75 / F77 follow-ups (pi3's first review of PR #337, `early-337.md`):
//
//   - F1 (P1): the first F75 fix answered "incomplete" for EVERY keyless
//     (pre-F35) Restored marker once the spec named a restore target. Every
//     rollback/fork completed on v0.1.1/v0.1.2 has a keyless marker AND a spec
//     that still names the same target (restoreFrom/forkOf are immutable and
//     nothing clears restoreFrom), so on the first reconcile after upgrade the
//     operator deleted the workspace PVC and restored the old snapshot over
//     everything done since — silently, announced only by a normal `Restored`
//     Event. The marker's own text names the target it restored, so the
//     decision can tell a completed restore from a new one.
//   - F2: the F75 spec asserted `b2RestoreTarget` on a fake client, so the
//     arm that actually wipes the workspace was never exercised. These specs
//     drive Reconcile through the stop -> delete -> create sequence.
//   - F3.3: the F77 "no delivery is made for it" claim was true only after the
//     completion; during the in-progress passes the deliver Job was stopped and
//     re-created in the same pass (the pin is deliberately kept until the
//     completion). deliveryRequested now refuses while a restore is in
//     progress, and this spec asserts that at the Job level rather than at the
//     predicate level.

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"

	cov "github.com/papawattu/coxswain/api/v1alpha1"
)

var _ = Describe("F75/F77: a legacy restore marker and the restore's delivery window", func() {
	ctx := context.Background()

	// f75Ns makes a self-contained namespace.
	f75Ns := func(prefix string) string {
		ns := prefix + "-" + nowSuffix()
		Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
		DeferCleanup(func() {
			_ = k8sClient.Delete(context.Background(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})
		})
		return ns
	}

	f75Get := func(ns, name string) *cov.Loop {
		loop := &cov.Loop{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, loop)).To(Succeed())
		return loop
	}

	// f75VSC creates the VolumeSnapshotClass the checkpoint path needs
	// (cluster-scoped and shared, so a second spec's create is tolerated).
	f75VSC := func() {
		vsc := &unstructured.Unstructured{Object: map[string]any{
			"apiVersion":     snapshotGV,
			"kind":           volumeSnapshotClassKind,
			"metadata":       map[string]any{"name": "b1-test-vsc"},
			"driver":         b1Driver,
			"deletionPolicy": "Delete",
		}}
		if err := k8sClient.Create(ctx, vsc); err != nil && !apierrors.IsAlreadyExists(err) {
			Expect(err).NotTo(HaveOccurred(), "create the VolumeSnapshotClass fixture")
		}
	}

	// f75SeedLegacyMarker writes a Restored marker exactly as a pre-F35
	// completion wrote it: the human text with the kind and iteration, and NO
	// " [spec-target=…]" block.
	f75SeedLegacyMarker := func(ns, name string, kind b2Kind, iteration int) {
		loop := f75Get(ns, name)
		meta.SetStatusCondition(&loop.Status.Conditions, metav1.Condition{
			Type:               cov.RestoreCondition,
			Status:             metav1.ConditionTrue,
			Reason:             cov.RestoreReasonRestored,
			Message:            "workspace PVC restored from snap (" + string(kind) + ", iter-" + itoa(iteration) + ")",
			ObservedGeneration: loop.Generation,
		})
		Expect(k8sClient.Status().Update(ctx, loop)).To(Succeed())
	}

	// f75RequestRollback sets spec.restoreFrom on the live Loop (the one write
	// an upgraded Loop already carries, or a user's request).
	f75RequestRollback := func(ns, name, iter string) {
		loop := f75Get(ns, name)
		loop.Spec.RestoreFrom = iter
		Expect(k8sClient.Update(ctx, loop)).To(Succeed())
	}

	// f75DriveRollback runs the three restore passes the rollback's level-based
	// sequence needs in envtest (there is no agent-sandbox controller to remove
	// the pod and no pvc-protection controller to release the PVC): stop the
	// sandbox, remove the pod, delete the PVC, release the finalizer, create.
	// It returns the Loop after the final pass.
	f75DriveRollback := func(r *LoopReconciler, ns, name string) *cov.Loop {
		b2Reconcile(r, ns, name) // step 1: stop the sandbox
		b2DeleteSandboxPod(ctx, ns, name)
		b2Reconcile(r, ns, name) // step 2: stop the workloads and delete the PVC
		b2RemovePVCProtectionFinalizer(ctx, ns, name)
		return b2Reconcile(r, ns, name) // step 3: create the PVC from the snapshot
	}

	// f75Succeeded drives a fresh Loop to Succeeded with a ready checkpoint at
	// iteration 0 (the source the rollback restores from).
	f75Succeeded := func(r *LoopReconciler, ns, name string) {
		Expect(k8sClient.Create(ctx, &cov.Loop{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec:       b2LoopSpec(),
		})).To(Succeed())
		f75VSC()
		b2EnsureSandbox(ns, name)
		b2EnsureSandboxPod(ns, name)
		b1DriveToSucceeded(ctx, r, ns, name)
		Expect(f75Get(ns, name).Status.Phase).To(Equal(cov.LoopPhaseSucceeded),
			"precondition: the Loop is Succeeded (the rollback starts from a terminal phase)")
		snapName := checkpointName(&cov.Loop{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}}, 0)
		b2MakeSnapshotReady(ctx, ns, snapName)
	}

	It("does not re-run a rollback a legacy (keyless) marker already completed (PR #337 P1)", func() {
		ns := f75Ns("f75-legacy-rb")
		const name = "f75-legacy-rb"
		r, _ := b2NewReconcilerWithRecorder()
		f75Succeeded(r, ns, name)

		before, err := b2GetPVC(ctx, ns, name)
		Expect(err).NotTo(HaveOccurred())
		pvcUID := before.UID

		By("a rollback that completed on a pre-F35 build: a keyless marker naming iter-0, and the spec still naming iter-0")
		f75SeedLegacyMarker(ns, name, b2KindRollback, 0)
		f75RequestRollback(ns, name, "iter-0")
		seeded := f75Get(ns, name)
		Expect(restoreCompletedForSpec(seeded)).To(BeTrue(),
			"precondition: the keyless marker names the target the immutable spec still names, so the restore is complete")

		By("driving the same passes a re-armed rollback would need: the workspace must not be touched")
		final := f75DriveRollback(r, ns, name)

		after, err := b2GetPVC(ctx, ns, name)
		Expect(err).NotTo(HaveOccurred(), "the workspace PVC must still exist")
		Expect(after.UID).To(Equal(pvcUID),
			"P1: a completed restore must not be re-armed — the rollback deletes the PVC and re-creates it from the snapshot, which is silent data loss for every Loop that restored on v0.1.1/v0.1.2")

		cond := b2RestoreCondition(final)
		Expect(cond).NotTo(BeNil())
		Expect(cond.Reason).To(Equal(cov.RestoreReasonRestored))
		Expect(cond.Message).NotTo(ContainSubstring(restoreSpecKeyMarker),
			"the keyless marker is the completed record: no restore ran, so nothing rewrote it with a key")
	})

	It("runs the rollback when a legacy LOST-PVC marker meets a restoreFrom set afterwards (F75)", func() {
		ns := f75Ns("f75-legacy-lost")
		const name = "f75-legacy-lost"
		r, _ := b2NewReconcilerWithRecorder()
		f75Succeeded(r, ns, name)

		before, err := b2GetPVC(ctx, ns, name)
		Expect(err).NotTo(HaveOccurred())

		By("a pre-F35 LOST-PVC restore, then a rollback target set afterwards (the spec names one the marker never restored)")
		f75SeedLegacyMarker(ns, name, b2KindLostPVC, 0)
		f75RequestRollback(ns, name, "iter-0")

		final := f75DriveRollback(r, ns, name)

		cond := b2RestoreCondition(final)
		Expect(cond).NotTo(BeNil())
		Expect(cond.Reason).To(Equal(cov.RestoreReasonRestored), "F75: the rollback the keyless marker does not cover must run")
		Expect(cond.Message).To(ContainSubstring(restoreSpecKeyMarker+"rollback:iter-0]"),
			"the completion records the target it restored (F35's key), so the next pass does not re-arm it")

		after, err := b2GetPVC(ctx, ns, name)
		Expect(err).NotTo(HaveOccurred())
		Expect(after.UID).NotTo(Equal(before.UID),
			"the rollback replaced the workspace PVC from the snapshot (the arm this spec exists to exercise)")
	})

	It("creates no deliver Job for the pin while a restore is in progress (F3.3)", func() {
		ns := f75Ns("f75-deliver")
		const name = "f75-deliver"
		spec := b2LoopSpec()
		spec.Workspace.Repo = inClusterRepoURL // the in-cluster peer keeps the direct path (no I72 proxy gate)
		Expect(k8sClient.Create(ctx, &cov.Loop{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec:       spec,
		})).To(Succeed())
		r, _ := b2NewReconcilerWithRecorder()
		f75VSC()
		b2EnsureSandbox(ns, name)
		b2EnsureSandboxPod(ns, name)
		b1DriveToSucceeded(ctx, r, ns, name)
		snapName := checkpointName(&cov.Loop{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}}, 0)
		b2MakeSnapshotReady(ctx, ns, snapName)

		By("positive control: at Succeeded, with a pin and delivery mode, the deliver Job IS created")
		loop := f75Get(ns, name)
		loop.Status.CurrentVerify = &cov.CurrentVerifyStatus{VerifiedCommit: f77Pin}
		Expect(k8sClient.Status().Update(ctx, loop)).To(Succeed())
		loop = f75Get(ns, name)
		loop.Spec.Delivery = &cov.DeliveryConfig{Mode: cov.DeliveryModePullRequest}
		Expect(k8sClient.Update(ctx, loop)).To(Succeed())
		_ = b2Reconcile(r, ns, name)
		queued := &batchv1.Job{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: deliverJobName(name)}, queued)).To(Succeed(),
			"the gate is real: a Succeeded Loop with a pin and delivery mode gets its deliver Job")
		queuedUID := queued.UID

		By("a rollback request: the in-progress passes create no new deliver Job for the pin")
		f75RequestRollback(ns, name, "iter-0")
		inProgress := b2Reconcile(r, ns, name) // step 1: stop the sandbox
		cond := b2RestoreCondition(inProgress)
		Expect(cond).NotTo(BeNil())
		Expect(cond.Reason).To(Equal(cov.RestoreReasonStoppingSandbox), "precondition: the restore is in progress")
		requested, why := deliveryRequested(inProgress)
		Expect(requested).To(BeFalse(), "F3.3: no deliver Job may be created for a pin whose workspace is being replaced")
		Expect(why).To(ContainSubstring("restore is in progress"))
		still := &batchv1.Job{}
		if err := k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: deliverJobName(name)}, still); err == nil {
			Expect(still.UID).To(Equal(queuedUID),
				"the in-progress pass must not create a NEW deliver Job for the pin (the queued one is stopped by the restore)")
		} else {
			Expect(apierrors.IsNotFound(err)).To(BeTrue())
		}

		b2DeleteSandboxPod(ctx, ns, name)
		b2Reconcile(r, ns, name) // step 2: stop the workloads (this deletes the queued Job) and delete the PVC
		s6RequireNoJob(ns, name)
		b2RemovePVCProtectionFinalizer(ctx, ns, name)
		restored := b2Reconcile(r, ns, name) // step 3: create the PVC from the snapshot
		s6RequireNoJob(ns, name)

		cond = b2RestoreCondition(restored)
		Expect(cond).NotTo(BeNil())
		Expect(cond.Reason).To(Equal(cov.RestoreReasonRestored), "precondition: the rollback completed")
		Expect(restored.Status.CurrentVerify).To(BeNil(), "the completion cleared the pin")
	})
})
