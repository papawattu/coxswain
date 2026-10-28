package controller

// F77 (R32 I113(d), the pin half; pi3 design .samples/design-f74-f82.md §PR A):
// a completed restore replaces the workspace PVC from a snapshot, so the verify
// pin (status.currentVerify.verifiedCommit / status.verify.verifiedCommit) and
// the tamper record written for it describe work the restore threw away.
// Leaving the pin in place had two effects, both fixed here:
//
//   - the old pin still gated delivery (deliveryRequested / deliveryEnabled), so
//     a delivery queued before the rollback could still push a PR for the
//     discarded work;
//   - the next delivery body rendered the previous iteration's tamper record.
//
// The phase/iteration half of F77 (a rollback does not reset the phase machine)
// is BLOCKED on owner decision D84 and is not built here.

import (
	"context"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"

	cov "github.com/papawattu/coxswain/api/v1alpha1"
)

const (
	// f77Pin is the verified commit the Succeeded Loop is pinned to before the
	// restore. The restored workspace cannot contain it.
	f77Pin = "f77f77f77f77f77f77f77f77f77f77f77f77f77"
	// f77Repo is a repo the deliver step needs (deliveryRequested requires a
	// non-empty spec.workspace.repo).
	f77Repo = "https://example.test/owner/repo.git"
)

var _ = Describe("F77: a completed restore clears the verify pin", func() {
	ctx := context.Background()

	f77Ns := func(prefix string) string {
		ns := prefix + "-" + nowSuffix()
		Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
		DeferCleanup(func() {
			_ = k8sClient.Delete(context.Background(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})
		})
		return ns
	}

	f77Get := func(ns, name string) *cov.Loop {
		loop := &cov.Loop{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, loop)).To(Succeed())
		return loop
	}

	// f77EnsureVSC creates the VolumeSnapshotClass the B1 checkpoint path needs
	// (cluster-scoped and shared, so a second spec's create is tolerated).
	f77EnsureVSC := func() {
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

	// f77SeedPin writes the state a Succeeded, delivery-enabled Loop carries
	// before a rollback: the verify pin plus the tamper record measured for it.
	f77SeedPin := func(ns, name string) {
		loop := f77Get(ns, name)
		exit := int32(0)
		loop.Status.CurrentVerify = &cov.CurrentVerifyStatus{VerifiedCommit: f77Pin}
		loop.Status.Verify = &cov.VerifyStatus{
			VerifiedCommit: f77Pin,
			TamperExitCode: &exit,
			TamperJobUID:   "f77-verify-job-uid",
		}
		Expect(k8sClient.Status().Update(ctx, loop)).To(Succeed())
	}

	// f77DrainEvents drops the recorder's buffered events and returns them.
	f77DrainEvents := func(rec *record.FakeRecorder) []string {
		var out []string
		for {
			select {
			case e := <-rec.Events:
				out = append(out, e)
			default:
				return out
			}
		}
	}

	// f77ExpectCleared asserts the F77 invariant on the restored Loop, and — the
	// point of the fix — that the operator's deliver gate can no longer fire for
	// the old pin. The gate is asserted as the operator computes it
	// (deliveryRequested / deliveryEnabled, which ensureDeliverJob returns early
	// on) with a delivery config in place; the live Loop keeps spec.delivery
	// unset so this spec stays about the completed state (the live, Job-level
	// half of the same claim — no deliver Job for the pin while the restore is in
	// progress — is pinned in loop_f75_legacy_restore_envtest_test.go, F3.3).
	f77ExpectCleared := func(restored *cov.Loop) {
		Expect(restored.Status.CurrentVerify).To(BeNil(),
			"F77: a completed restore clears status.currentVerify (the old pin describes a discarded workspace)")
		if restored.Status.Verify != nil {
			Expect(restored.Status.Verify.VerifiedCommit).To(BeEmpty(),
				"F77: the pin copy in status.verify goes with the pin")
			Expect(restored.Status.Verify.TamperExitCode).To(BeNil(),
				"R34 I119(c): the tamper record goes with the pin it was measured for")
			Expect(restored.Status.Verify.TamperJobUID).To(BeEmpty())
		}

		By("the deliver gate can no longer fire for the old pin")
		delivering := restored.DeepCopy()
		delivering.Spec.Delivery = &cov.DeliveryConfig{Mode: cov.DeliveryModePullRequest}
		delivering.Spec.Workspace.Repo = f77Repo
		Expect(restored.Status.Phase).To(Equal(cov.LoopPhaseSucceeded),
			"precondition: the phase stays Succeeded (the phase half of F77 waits on D84)")
		Expect(deliveryEnabled(delivering)).To(BeFalse(),
			"F77: with no pin the deliver step must not run")
		requested, why := deliveryRequested(delivering)
		Expect(requested).To(BeFalse(), "F77: no deliver Job may be created for the discarded commit")
		Expect(why).To(ContainSubstring("no verifiedCommit pinned"),
			"the reason names the missing pin, so a reader can tell this apart from a delivery that already ran")

		By("positive control: the same gate DOES fire with the pin restored")
		pinned := delivering.DeepCopy()
		pinned.Status.CurrentVerify = &cov.CurrentVerifyStatus{VerifiedCommit: f77Pin}
		Expect(deliveryEnabled(pinned)).To(BeTrue(),
			"the gate is real: with the pin present, delivery is expected (so the assertion above is not vacuous)")
		requestedPinned, _ := deliveryRequested(pinned)
		Expect(requestedPinned).To(BeTrue(),
			"the control exercises the SAME function that produced the negative result (deliveryRequested), not only its phase-independent half")
	}

	// f77ExpectPinEvent asserts the completion's Normal Event names the clear
	// (F77: the dropped delivery is announced, not silent).
	f77ExpectPinEvent := func(rec *record.FakeRecorder) {
		found := false
		for _, e := range f77DrainEvents(rec) {
			if strings.Contains(e, "reason="+cov.RestoreReasonRestored) && strings.Contains(e, "verify pin") {
				found = true
			}
		}
		Expect(found).To(BeTrue(), "the Restored Event must say the verify pin was cleared")
	}

	It("clears the pin and the tamper record when a ROLLBACK completes", func() {
		ns := f77Ns("f77-rb")
		const name = "f77-rb"
		Expect(k8sClient.Create(ctx, &cov.Loop{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec:       b2LoopSpec(),
		})).To(Succeed())
		f77EnsureVSC()

		r, rec := b2NewReconcilerWithRecorder()
		b2EnsureSandbox(ns, name)
		b2EnsureSandboxPod(ns, name)
		b1DriveToSucceeded(ctx, r, ns, name)
		snapName := checkpointName(&cov.Loop{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}}, 0)
		b2MakeSnapshotReady(ctx, ns, snapName)

		By("a Succeeded Loop with a verify pin, a tamper record, and a rollback request")
		f77SeedPin(ns, name)
		pinned := f77Get(ns, name)
		Expect(pinned.Status.CurrentVerify).NotTo(BeNil(), "precondition: the pin is in place")
		pinned.Spec.RestoreFrom = "iter-0"
		Expect(k8sClient.Update(ctx, pinned)).To(Succeed())

		By("the level-based rollback (stop -> delete -> create) drives restoreFrom to Restored")
		_ = f77DrainEvents(rec)  // drop the pre-rollback events
		b2Reconcile(r, ns, name) // step 1: stop the sandbox
		Expect(f77Get(ns, name).Status.CurrentVerify).NotTo(BeNil(),
			"the pin survives while the rollback is in progress (it is cleared at the completion)")
		b2DeleteSandboxPod(ctx, ns, name) // the agent-sandbox controller removes the pod
		b2Reconcile(r, ns, name)          // step 2: delete the PVC
		b2RemovePVCProtectionFinalizer(ctx, ns, name)
		restored := b2Reconcile(r, ns, name) // step 3: create the PVC from the snapshot

		cond := b2RestoreCondition(restored)
		Expect(cond).NotTo(BeNil())
		Expect(cond.Reason).To(Equal(cov.RestoreReasonRestored), "the rollback completed")
		f77ExpectCleared(restored)
		f77ExpectPinEvent(rec)
	})

	It("clears the pin and the tamper record when a LOST-PVC restore completes", func() {
		ns := f77Ns("f77-lost")
		const name = "f77-lost"
		Expect(k8sClient.Create(ctx, &cov.Loop{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec:       b2LoopSpec(),
		})).To(Succeed())
		f77EnsureVSC()

		r, rec := b2NewReconcilerWithRecorder()
		b2EnsureSandbox(ns, name)
		b1DriveToSucceeded(ctx, r, ns, name)
		snapName := checkpointName(&cov.Loop{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}}, 0)
		b2MakeSnapshotReady(ctx, ns, snapName)

		By("a Succeeded Loop with a verify pin whose workspace PVC is then lost")
		f77SeedPin(ns, name)
		pvc, err := b2GetPVC(ctx, ns, name)
		Expect(err).NotTo(HaveOccurred())
		pvc.Finalizers = []string{} // envtest runs no pvc-protection controller
		Expect(k8sClient.Update(ctx, pvc)).To(Succeed())
		Expect(k8sClient.Delete(ctx, pvc)).To(Succeed())

		_ = f77DrainEvents(rec)
		restored := b2Reconcile(r, ns, name)

		lostCond := b2RestoreCondition(restored)
		Expect(lostCond).NotTo(BeNil())
		Expect(lostCond.Reason).To(Equal(cov.RestoreReasonRestored), "the lost-PVC restore completed")
		f77ExpectCleared(restored)
		f77ExpectPinEvent(rec)
	})
})
