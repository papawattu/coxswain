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

// D84a (F74): a lost workspace PVC with no usable checkpoint fails the Loop
// terminally. Before D84a the A2 fallback recreated the workspace PVC EMPTY
// and the Loop carried on (status.baseCommit stayed set, so init-workspace
// never re-cloned, and the agent ran in an empty workspace). The decision
// (owner, 2026-11-01, D84(c)): fail the Loop instead — the terminal
// WorkspaceLost reason, modelled on failLoopUnschedulable (the F93a pattern:
// a Failed condition with a reason and a message naming the fix, the phase
// flipped to Failed, and one Warning Event). The Restore condition stays
// False reason NoUsableCheckpoint as the CAUSE record.
//
// The specs:
//
//   - (a) the PVC is lost and the checkpoint's snapshot is ABSENT: the Loop
//     is Failed (reason WorkspaceLost; the message names the lost
//     checkpoint and the fix — delete and re-apply the Loop); the Restore
//     condition keeps NoUsableCheckpoint as the cause; no empty PVC is
//     created; the sandbox is stopped (no new sandbox pod runs); one
//     Warning Event.
//   - (b) the same with UNTRUSTED checkpoint entries (none is a checkpoint
//     of this Loop): also Failed.
//   - (c) a lost PVC WITH a usable checkpoint still restores (not Failed) —
//     the positive control for the branch selection.
//   - (d) an in-progress state (the PVC is present and Bound): no decision,
//     not Failed (I49).
//
// The named mutations (applied in a scratch worktree — see the PR):
//
//   - M1 (the branch carries on as before): restore the pre-D84a b2KindLostPVC
//     arm (ensureWorkspacePVC + carry-on, no terminal fail). Specs (a) and (b)
//     must FAIL (the Loop is not Failed).
//   - M2 (the fail fires on the usable-checkpoint path): move the terminal
//     fail into b2ApplyLostPVCRestore's completion (after the PVC is created
//     from the snapshot). Spec (c) must FAIL (the restored Loop is Failed).

package controller

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
	sandboxv1beta1 "sigs.k8s.io/agent-sandbox/api/v1beta1"

	coxv1alpha1 "github.com/papawattu/coxswain/api/v1alpha1"
)

// d84aDriveToCheckpoint drives a fresh Loop (the given name, in ns) to a
// successful verify so a usable checkpoint (iter-0, ReadyToUse) exists. It
// returns the reconciler (with the driver/CRD seams + a FakeRecorder) and the
// snapshot name.
func d84aDriveToCheckpoint(r *LoopReconciler, ns, name string) string {
	b2EnsureSandbox(ns, name)
	b1DriveToSucceeded(ctx, r, ns, name)
	snapName := checkpointName(&coxv1alpha1.Loop{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}}, 0)
	b2MakeSnapshotReady(ctx, ns, snapName)
	return snapName
}

// d84aLostPVC drives a Loop to a ready iter-0 checkpoint, applies cp (a
// status.checkpoints replacement on the fresh Loop; nil leaves it), loses
// the workspace PVC (finalizers removed first — the A2 idiom), and reconciles
// once. It returns the reconciler, the recorder, the Loop name, and the
// iter-0 snapshot name.
func d84aLostPVC(ns, name string, cp func(*coxv1alpha1.Loop)) (*LoopReconciler, *record.FakeRecorder, string) {
	Expect(k8sClient.Create(ctx, &coxv1alpha1.Loop{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec:       b2LoopSpec(),
	})).To(Succeed())
	r, rec := b2NewReconcilerWithRecorder()
	snapName := d84aDriveToCheckpoint(r, ns, name)
	loop := &coxv1alpha1.Loop{}
	Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, loop)).To(Succeed())
	if cp != nil {
		cp(loop)
		Expect(k8sClient.Status().Update(ctx, loop)).To(Succeed())
	}
	pvc, err := b2GetPVC(ctx, ns, name)
	Expect(err).NotTo(HaveOccurred())
	pvc.Finalizers = []string{}
	Expect(k8sClient.Update(ctx, pvc)).To(Succeed())
	Expect(k8sClient.Delete(ctx, pvc)).To(Succeed())
	b2Reconcile(r, ns, name)
	return r, rec, snapName
}

// d84aFailedCondition returns the Loop's Failed (terminal) condition (nil
// when absent).
func d84aFailedCondition(loop *coxv1alpha1.Loop) *metav1.Condition {
	for i := range loop.Status.Conditions {
		c := loop.Status.Conditions[i]
		if c.Type == string(coxv1alpha1.LoopPhaseFailed) {
			return &c
		}
	}
	return nil
}

// d84aCountEvents counts the recorded Events containing needle.
func d84aCountEvents(rec *record.FakeRecorder, needle string) int {
	count := 0
	for {
		select {
		case e := <-rec.Events:
			if strings.Contains(e, needle) {
				count++
			}
		default:
			return count
		}
	}
}

var _ = Describe("D84a: a lost workspace PVC with no usable checkpoint fails the Loop (envtest)", func() {
	var ns string

	BeforeEach(func() {
		ns = "d84a-" + nowSuffix()
		Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
		DeferCleanup(func() {
			_ = k8sClient.Delete(context.Background(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})
		})
		// The B1 checkpoint path (b1DriveToSucceeded) requires a VSC for the
		// workspace driver (cluster-scoped; shared across specs in the suite).
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

	It("D84a (a): a LOST PVC whose checkpoint snapshot is GONE fails the Loop terminally (Failed=WorkspaceLost, the fix named; Restore=NoUsableCheckpoint as the cause; no empty PVC; the sandbox is stopped; one Warning Event)", func() {
		const name = "d84a-gone"
		r, rec, snapName := d84aLostPVC(ns, name, nil)
		defer func() { _ = k8sClient.Delete(context.Background(), &coxv1alpha1.Loop{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}}) }()
		loop := &coxv1alpha1.Loop{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, loop)).To(Succeed())

		By("failing the Loop terminally (Phase + DesiredPhase = Failed; the F93a terminal pattern)")
		Expect(loop.Status.Phase).To(Equal(coxv1alpha1.LoopPhaseFailed),
			"D84a(c): a lost PVC with no usable checkpoint must fail the Loop, not carry on in an empty workspace")
		Expect(loop.Status.DesiredPhase).To(Equal(coxv1alpha1.LoopPhaseFailed))
		cond := d84aFailedCondition(loop)
		Expect(cond).NotTo(BeNil(), "the terminal failure is recorded as a Failed condition (the F93a pattern)")
		Expect(cond.Status).To(Equal(metav1.ConditionTrue))
		Expect(cond.Reason).To(Equal(coxv1alpha1.LoopFailedReasonWorkspaceLost))
		Expect(cond.Message).To(ContainSubstring(snapName), "the Failed message names the lost checkpoint")
		Expect(cond.Message).To(ContainSubstring("delete and re-apply"),
			"the Failed message names the fix: a fresh Loop clones from scratch")

		By("keeping the Restore condition False reason NoUsableCheckpoint as the cause record")
		cause := b2RestoreCondition(loop)
		Expect(cause).NotTo(BeNil(), "the cause record is the Restore condition (persisted at the point of decision)")
		Expect(cause.Status).To(Equal(metav1.ConditionFalse))
		Expect(cause.Reason).To(Equal(coxv1alpha1.RestoreReasonNoUsableCheckpoint))

		By("NOT creating an empty PVC for the failed Loop, on the decision pass or a later one")
		noPVC := func() {
			pvc := &corev1.PersistentVolumeClaim{}
			Expect(apierrors.IsNotFound(k8sClient.Get(ctx, types.NamespacedName{Name: workspacePVCName(name), Namespace: ns}, pvc))).To(
				BeTrue(), "D84a(c): the fallback does not seed the failed Loop with an empty workspace (the F36 fork-reject precedent)")
		}
		noPVC()
		// A LATER pass re-enters the same arm (b2LostPVCTarget re-triggers:
		// the PVC is still gone and the checkpoint is still missing). The
		// fail must stay terminal — the Failed condition and the phase stay
		// put, and the arm must not seed an empty PVC on the re-entry either.
		r2 := b2Reconcile(r, ns, name)
		Expect(r2.Status.Phase).To(Equal(coxv1alpha1.LoopPhaseFailed), "the later pass is a no-op on the terminal state")
		Expect(d84aFailedCondition(r2)).NotTo(BeNil())
		noPVC()

		By("stopping the sandbox (no sandbox runs for a failed Loop — the terminal phase suspends it)")
		sb := &sandboxv1beta1.Sandbox{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: b2SandboxName(name), Namespace: ns}, sb)).To(Succeed())
		Expect(sb.Spec.OperatingMode).To(Equal(sandboxv1beta1.SandboxOperatingModeSuspended),
			"a Failed Loop's sandbox is suspended (loopSandboxStopped), so no sandbox pod runs for the failed Loop")

		By("emitting one Warning Event for the decision (the terminal re-entry emits none)")
		Expect(d84aCountEvents(rec, "reason=NoUsableCheckpoint")).To(Equal(1))
	})

	It("D84a (b): a LOST PVC whose status.checkpoints are ALL UNTRUSTED fails the Loop terminally (the same terminal, the untrusted cause in the message)", func() {
		const name = "d84a-untrusted"
		_, rec, _ := d84aLostPVC(ns, name, func(loop *coxv1alpha1.Loop) {
			// Only a forged entry (wrong shape for this Loop's checkpoint
			// naming — the F47 untrusted case): no usable checkpoint.
			loop.Status.Checkpoints = []coxv1alpha1.CheckpointStatus{
				{Iteration: 3, Name: "checkpoint-victim-iter-3", At: metav1.Now()},
			}
		})
		loop := &coxv1alpha1.Loop{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, loop)).To(Succeed())
		Expect(loop.Status.Phase).To(Equal(coxv1alpha1.LoopPhaseFailed),
			"D84a(c): all-untrusted checkpoints are no checkpoint — the Loop fails, it does not run empty")
		cond := d84aFailedCondition(loop)
		Expect(cond).NotTo(BeNil())
		Expect(cond.Status).To(Equal(metav1.ConditionTrue))
		Expect(cond.Reason).To(Equal(coxv1alpha1.LoopFailedReasonWorkspaceLost))
		Expect(cond.Message).To(ContainSubstring("none of the status.checkpoints entries"),
			"the Failed message carries the untrusted cause (the three-variant cause record)")
		cause := b2RestoreCondition(loop)
		Expect(cause).NotTo(BeNil())
		Expect(cause.Reason).To(Equal(coxv1alpha1.RestoreReasonNoUsableCheckpoint))
		Expect(d84aCountEvents(rec, "reason=NoUsableCheckpoint")).To(Equal(1))
	})

	It("D84a (c): a LOST PVC WITH a usable checkpoint still RESTORES and is NOT failed (the positive control for the branch selection)", func() {
		const name = "d84a-restored"
		_, _, snapName := d84aLostPVC(ns, name, nil)
		loop := &coxv1alpha1.Loop{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, loop)).To(Succeed())

		By("restoring the PVC from the ready checkpoint (the A2 restore path is untouched by D84a)")
		pvc, err := b2GetPVC(ctx, ns, name)
		Expect(err).NotTo(HaveOccurred(), "the operator restores the LOST PVC from the usable checkpoint")
		Expect(pvc.Spec.DataSource).NotTo(BeNil(), "the restored PVC carries the checkpoint's VolumeSnapshot")
		Expect(pvc.Spec.DataSource.Name).To(Equal(snapName))
		restored := b2RestoreCondition(loop)
		Expect(restored).NotTo(BeNil())
		Expect(restored.Status).To(Equal(metav1.ConditionTrue))
		Expect(restored.Reason).To(Equal(coxv1alpha1.RestoreReasonRestored))

		By("NOT failing the Loop")
		Expect(loop.Status.Phase).NotTo(Equal(coxv1alpha1.LoopPhaseFailed),
			"a usable checkpoint restores the workspace — the Loop carries on, it is not Failed")
		Expect(d84aFailedCondition(loop)).To(BeNil(), "no Failed condition on the restore path")
	})

	It("D84a (d): a PRESENT, Bound workspace PVC is an in-progress state with NO decision (the Loop is not failed — I49)", func() {
		const name = "d84a-present"
		Expect(k8sClient.Create(ctx, &coxv1alpha1.Loop{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec:       b2LoopSpec(),
		})).To(Succeed())
		r, _ := b2NewReconcilerWithRecorder()
		snapName := d84aDriveToCheckpoint(r, ns, name)
		// The PVC is present (the in-progress state — the reactive restore
		// trigger, b2LostPVCTarget, is nil while the PVC exists).
		pvc, err := b2GetPVC(ctx, ns, name)
		Expect(err).NotTo(HaveOccurred())
		Expect(pvc.Status.Phase).To(Equal(corev1.ClaimBound),
			"the fixture's PVC is Bound (the cluster default provisioner binds it)")
		_ = snapName

		By("reconciling: no restore decision, no terminal fail")
		loop := b2Reconcile(r, ns, name)
		Expect(loop.Status.Phase).NotTo(Equal(coxv1alpha1.LoopPhaseFailed),
			"a present, Bound PVC is not a loss — no decision, I49")
		Expect(d84aFailedCondition(loop)).To(BeNil(), "no Failed condition while the PVC is present")
	})
})
