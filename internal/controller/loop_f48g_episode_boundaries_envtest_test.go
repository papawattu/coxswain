package controller

// F48g: the sandbox-failure count (status.agent.infraAttempts, F48b) belongs to
// a RUN, not to a Sandbox and not to "since the last accepted claim". #266 moved
// the episode boundary to "a claim the operator accepts", which left one state
// with no reachable clear and two transitions that start a new run with no claim
// at all (pi3 review-postmerge-9 P1-1, P2-1; design .samples/design-f48g.md):
//
//   - at Verifying the claim clear is UNREACHABLE (the evidence-gated hold
//     returns before resolvePhaseClaim) while a failure CAN be counted (the
//     F48b mount path), so the count only grows there;
//   - the exits from Verifying (the iterate, the re-plan) and the approval
//     advance start a new run with no claim;
//   - a completed restore rebuilds the workspace without moving iteration or
//     desiredPhase.
//
// One later failure then reads as attempt 3 and fails the Loop terminally
// (Failed:SandboxFailed has no retry). The fix is the run key
// (status.agent.infraEpisode = "<iteration>/<desiredPhase>"): the count restarts
// when the run changes, so no call site has to remember to clear it. The specs
// below drive Reconcile where the composition is what leaked; the retry
// invariant (a recreate must NOT reset the count) is F48b's spec.

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"

	cov "github.com/papawattu/coxswain/api/v1alpha1"
)

var _ = Describe("F48g: the sandbox-failure count belongs to a run", func() {
	ctx := context.Background()

	f48gNs := func(prefix string) string {
		ns := prefix + "-" + nowSuffix()
		Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
		DeferCleanup(func() {
			_ = k8sClient.Delete(context.Background(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})
		})
		return ns
	}

	f48gGet := func(ns, name string) *cov.Loop {
		loop := &cov.Loop{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, loop)).To(Succeed())
		return loop
	}

	// f48gSeedCount writes the state the leak leaves behind: the previous run's
	// count and the episode key it belongs to.
	f48gSeedCount := func(ns, name string, attempts int32, episode string) {
		loop := f48gGet(ns, name)
		loop.Status.Agent = &cov.AgentStatus{
			InfraAttempts: attempts, InfraSandboxUID: "episode-one-sandbox", InfraEpisode: episode,
		}
		Expect(k8sClient.Status().Update(ctx, loop)).To(Succeed())
	}

	f48gFailure := func() *sandboxFailure {
		return &sandboxFailure{
			sandboxUID: types.UID("episode-two-sandbox"), container: "<pod>",
			reason: "FailedMount", message: `secret "git-cred" not found`, mountFailure: true,
		}
	}

	It("S2: a count from the Verifying run does not follow the Loop into the next run", func() {
		ns := f48gNs("f48g-s2")
		const name = "f48g-s2"
		Expect(k8sClient.Create(ctx, &cov.Loop{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec:       s5aLoopSpec(),
		})).To(Succeed())
		r := &LoopReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), apiReader: k8sClient}

		// The Loop has iterated out of Verifying (the operator's own transition:
		// iteration 2, desiredPhase Implementing) and still carries the count
		// the Verifying run accumulated.
		loop := f48gGet(ns, name)
		loop.Status.Phase = cov.LoopPhaseImplementing
		loop.Status.DesiredPhase = cov.LoopPhaseImplementing
		loop.Status.Iteration = 2
		Expect(k8sClient.Status().Update(ctx, loop)).To(Succeed())
		f48gSeedCount(ns, name, 2, "1/Verifying")

		By("one failure in the new run is attempt 1, not 3")
		fresh := f48gGet(ns, name)
		Expect(r.applySandboxFailure(ctx, fresh, f48gFailure())).To(BeTrue())
		Expect(fresh.Status.Agent.InfraAttempts).To(Equal(int32(1)),
			"the run changed (1/Verifying -> 2/Implementing): the episode starts again")
		Expect(fresh.Status.Agent.InfraEpisode).To(Equal("2/Implementing"))
		Expect(fresh.Status.Phase).To(Equal(cov.LoopPhaseImplementing), "one attempt never fails the Loop")
	})

	It("S3: the real verify iterate (through Reconcile) ends the episode, so the next failure is attempt 1", func() {
		ns := f48gNs("f48g-s3")
		const name = "f48g-s3"
		Expect(k8sClient.Create(ctx, &cov.Loop{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec:       s5aLoopSpec(),
		})).To(Succeed())

		r := s5aDriveToVerifying(ns, name)
		s5aReconcile(r, ns, name) // the verify Job is created for the pin
		jobName := name + "-verify-1"
		f48gSeedCount(ns, name, 2, "1/Verifying")

		By("check-0 fails: the operator iterates (Verifying -> Implementing, iteration 2)")
		s5aVerifyPod(ctx, ns, name, jobName, 1)
		iterated := s5aReconcile(r, ns, name)
		Expect(iterated.Status.Phase).To(Equal(cov.LoopPhaseImplementing), "the failing check iterates")
		Expect(iterated.Status.Iteration).To(Equal(2))
		Expect(iterated.Status.Agent).NotTo(BeNil())
		Expect(iterated.Status.Agent.InfraAttempts).To(Equal(int32(2)),
			"the iterate itself clears nothing — the COUNT restarts when the next failure names a different run")
		Expect(iterated.Status.Agent.InfraEpisode).To(Equal("1/Verifying"),
			"the recorded episode is still the one the counted failures came from")

		By("one failure in the new run is attempt 1 (the run key changed under it)")
		fresh := f48gGet(ns, name)
		Expect(r.applySandboxFailure(ctx, fresh, f48gFailure())).To(BeTrue())
		Expect(fresh.Status.Agent.InfraAttempts).To(Equal(int32(1)),
			"without the run key this reads 3 and fails the Loop terminally")
		Expect(fresh.Status.Agent.InfraEpisode).To(Equal("2/Implementing"))
	})

	It("S4: a completed restore ends the episode (the workspace those failures described is gone)", func() {
		ns := f48gNs("f48g-s4")
		const name = "f48g-s4"
		Expect(k8sClient.Create(ctx, &cov.Loop{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec:       b2LoopSpec(),
		})).To(Succeed())

		// The B1 checkpoint path needs a VolumeSnapshotClass for the
		// workspace driver (the b1/b2 fixture; cluster-scoped and shared, so
		// a second spec's create is tolerated).
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

		r, _ := b2NewReconcilerWithRecorder()
		b2EnsureSandbox(ns, name)
		b1DriveToSucceeded(ctx, r, ns, name)
		snapName := checkpointName(&cov.Loop{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}}, 0)
		b2MakeSnapshotReady(ctx, ns, snapName)

		By("losing the workspace PVC (the restore's trigger)")
		pvc, err := b2GetPVC(ctx, ns, name)
		Expect(err).NotTo(HaveOccurred())
		pvc.Finalizers = []string{} // envtest runs no pvc-protection controller
		Expect(k8sClient.Update(ctx, pvc)).To(Succeed())
		Expect(k8sClient.Delete(ctx, pvc)).To(Succeed())

		By("the pre-restore run's count is in place (a restore does not move the run key)")
		loop := f48gGet(ns, name)
		key := loop.Status.Iteration
		f48gSeedCount(ns, name, 2, sandboxRunKey(loop))

		b2Reconcile(r, ns, name)

		restoredLoop := f48gGet(ns, name)
		cond := b2RestoreCondition(restoredLoop)
		Expect(cond).NotTo(BeNil())
		Expect(cond.Reason).To(Equal(cov.RestoreReasonRestored), "the restore completed")
		Expect(restoredLoop.Status.Iteration).To(Equal(key), "a restore moves neither iteration nor desiredPhase")
		Expect(restoredLoop.Status.Agent).NotTo(BeNil())
		Expect(restoredLoop.Status.Agent.InfraAttempts).To(BeZero(),
			"F48g: the restore is a boundary no run key can see, so it clears explicitly")
		Expect(restoredLoop.Status.Agent.InfraSandboxUID).To(BeEmpty())
		Expect(restoredLoop.Status.Agent.InfraEpisode).To(BeEmpty())
	})

	It("S4b: a completed ROLLBACK (spec.restoreFrom) ends the episode too", func() {
		ns := f48gNs("f48g-s4b")
		const name = "f48g-s4b"
		Expect(k8sClient.Create(ctx, &cov.Loop{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec:       b2LoopSpec(),
		})).To(Succeed())

		// The B1 checkpoint path needs a VolumeSnapshotClass (the b1/b2 fixture).
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

		r, _ := b2NewReconcilerWithRecorder()
		b2EnsureSandbox(ns, name)
		b2EnsureSandboxPod(ns, name)
		b1DriveToSucceeded(ctx, r, ns, name)
		snapName := checkpointName(&cov.Loop{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}}, 0)
		b2MakeSnapshotReady(ctx, ns, snapName)

		By("requesting a rollback with the pre-rollback run's count in place")
		rbLoop := f48gGet(ns, name)
		rbLoop.Spec.RestoreFrom = "iter-0"
		Expect(k8sClient.Update(ctx, rbLoop)).To(Succeed())
		seeded := f48gGet(ns, name)
		f48gSeedCount(ns, name, 2, sandboxRunKey(seeded))

		By("the level-based rollback (stop -> delete -> create) drives restoreFrom to Restored")
		b2Reconcile(r, ns, name)          // step 1: stop the sandbox
		b2DeleteSandboxPod(ctx, ns, name) // the agent-sandbox controller removes the pod
		b2Reconcile(r, ns, name)          // step 2: delete the PVC
		b2RemovePVCProtectionFinalizer(ctx, ns, name)
		restoredLoop := b2Reconcile(r, ns, name) // step 3: create the PVC from the snapshot
		cond := b2RestoreCondition(restoredLoop)
		Expect(cond).NotTo(BeNil())
		Expect(cond.Reason).To(Equal(cov.RestoreReasonRestored), "the rollback completed")
		Expect(restoredLoop.Status.Iteration).To(Equal(seeded.Status.Iteration),
			"a rollback moves neither iteration nor desiredPhase — no run key can see it")
		Expect(restoredLoop.Status.Agent).NotTo(BeNil())
		Expect(restoredLoop.Status.Agent.InfraAttempts).To(BeZero(),
			"F48g: the rollback is a boundary the run key cannot see, so it clears explicitly")
		Expect(restoredLoop.Status.Agent.InfraSandboxUID).To(BeEmpty())
		Expect(restoredLoop.Status.Agent.InfraEpisode).To(BeEmpty())
	})

	It("S5: a pause does not reset the count (the pod is restarted in place, not recreated)", func() {
		ns := f48gNs("f48g-s5")
		const name = "f48g-s5"
		Expect(k8sClient.Create(ctx, &cov.Loop{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec:       s5aLoopSpec(),
		})).To(Succeed())
		r := s5aDriveToVerifying(ns, name)
		_ = r
		loop := f48gGet(ns, name)
		loop.Status.Agent = &cov.AgentStatus{InfraAttempts: 1, InfraSandboxUID: "episode-one-sandbox",
			InfraEpisode: sandboxRunKey(loop)}
		Expect(k8sClient.Status().Update(ctx, loop)).To(Succeed())

		By("pausing and resuming: the desired phase — and so the run — never changes")
		paused := f48gGet(ns, name)
		paused.Spec.Suspend = true
		Expect(k8sClient.Update(ctx, paused)).To(Succeed())
		s5aReconcile(r, ns, name)
		afterPause := f48gGet(ns, name)
		Expect(afterPause.Status.Agent).NotTo(BeNil())
		Expect(afterPause.Status.Agent.InfraAttempts).To(Equal(int32(1)), "a pause is not a boundary (P3-2)")

		afterPause.Spec.Suspend = false
		Expect(k8sClient.Update(ctx, afterPause)).To(Succeed())
		s5aReconcile(r, ns, name)
		afterResume := f48gGet(ns, name)
		Expect(afterResume.Status.Agent).NotTo(BeNil())
		Expect(afterResume.Status.Agent.InfraAttempts).To(Equal(int32(1)),
			"the resume restarts the same pod: the episode is continuous")
	})

	It("S6: a claim the stale guards reject ends no episode (the clear stays after them)", func() {
		loop := &cov.Loop{
			ObjectMeta: metav1.ObjectMeta{Name: "f48g-s6", Namespace: "default"},
			Status: cov.LoopStatus{
				Phase:     cov.LoopPhaseImplementing,
				Iteration: 2,
				Agent: &cov.AgentStatus{InfraAttempts: 2, InfraSandboxUID: "episode-one-sandbox",
					InfraEpisode: "2/Implementing"},
				Conditions: []metav1.Condition{{
					Type: cov.AgentFailedCondition, Status: metav1.ConditionTrue,
					Reason: "PodFailed", Message: "the pod failed", LastTransitionTime: metav1.Now(),
				}},
			},
		}
		r := &LoopReconciler{
			// A claim from the PREVIOUS iteration: the S5a stale guard must
			// discard it, and a discarded claim is not a recovery signal.
			readPhaseClaim: func(context.Context, *cov.Loop) (*PhaseClaim, error) {
				return &PhaseClaim{ObservedPhase: cov.LoopPhaseImplementing, Status: claimSuccess, Iteration: 1}, nil
			},
		}
		_, changed, _ := r.advancePhaseFromClaim(ctx, loop)
		Expect(loop.Status.Agent.InfraAttempts).To(Equal(int32(2)),
			"a stale claim must not end the episode (the clear sits after every stale/consumed guard)")
		Expect(loop.Status.Agent.InfraEpisode).To(Equal("2/Implementing"))

		// P3-3 (the swept F48d class): the F6 clear above DID run, so the
		// discarded claim must still report the change or the clear is lost.
		cleared := findConditionFn(loop.Status.Conditions, cov.AgentFailedCondition)
		Expect(cleared).NotTo(BeNil())
		Expect(cleared.Status).To(Equal(metav1.ConditionFalse))
		Expect(changed).To(BeTrue(),
			"P3-3: a stale-claim return must keep the changed flag the F6 clear just set")
	})
})
