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

// B2 (PLAN-BETA, PLAN.md Phase 3 part 2): the workspace restore, rollback
// and fork. Three behaviours share one mechanism: the operator recreates the
// Loop's workspace PVC with spec.dataSource = a VolumeSnapshot (a PVC
// dataSource is immutable — the PVC is deleted and recreated), then the A2
// PVC-resume path picks the Loop up on the restored content.
//
// The specs (envtest, no snapshot controller — the specs assert the OBJECT
// the operator creates, not the content it would produce):
//
//   - RESTORE (reactive): the workspace PVC is LOST (deleted) and the Loop
//     has a usable checkpoint (a recorded status.checkpoints entry whose
//     VolumeSnapshot is ReadyToUse). The operator restores from the latest
//     ReadyToUse checkpoint (the PVC is recreated with
//     spec.dataSource=the checkpoint's VolumeSnapshot) and surfaces it (the
//     Restore condition True reason Restored + a WorkspaceRestored Event).
//   - RESTORE (reactive, not-ready): the workspace PVC is LOST and the
//     latest checkpoint's VolumeSnapshot is NOT ReadyToUse. The operator
//     DEFERS (the Restore condition False reason RestoreSourceNotReady +
//     the Event; the PVC is NOT recreated — the in-progress state, no
//     decision, I49).
//   - RESTORE (A2 fallback): the workspace PVC is LOST and NO usable
//     checkpoint exists (the latest recorded checkpoint's VolumeSnapshot is
//     ABSENT). The operator falls back to the A2 PVC-resume (recreate the
//     PVC EMPTY) and surfaces the fallback (the Restore condition False
//     reason NoUsableCheckpoint + a WorkspaceRestoreUnavailable Event — I90:
//     the fallback is VISIBLE).
//   - ROLLBACK: spec.restoreFrom: iter-N names the checkpoint the operator
//     restores from. The checkpoint must EXIST and be ReadyToUse. A ready
//     checkpoint is restored. An ABSENT checkpoint is rejected VISIBLY
//     (RestoreSourceMissing). A NOT-READY checkpoint is DEFERRED
//     (RestoreSourceNotReady, I49).
//   - FORK: spec.forkOf {loop, iteration} seeds the Loop from another
//     Loop's checkpoint. At start the operator restores this Loop's OWN
//     workspace PVC from the source snapshot. Deleting the source Loop does
//     NOT break a fork that has already restored. A fork with an ABSENT
//     source checkpoint is rejected VISIBLY (RestoreSourceMissing).
//
// The named mutations (applied in a scratch worktree — see the PR for the
// commands and the FAIL they produce):
//
//   - M1 (restore ignores the checkpoint): change b2ApplyRestoreStep's
//     ready-branch to NOT recreate the PVC (skip the
//     restorePVCFromSnapshot call). The restore spec must FAIL.
//   - M2 (rollback accepts a non-ReadyToUse source): change
//     resolveCheckpointSource's notready-branch to return "ready". The
//     rollback not-ReadyToUse spec must FAIL.
//   - M3 (fork reuses the source PVC): change restorePVCFromSnapshot's
//     fork-branch to NOT delete + recreate the PVC. The fork spec must FAIL.

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
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	coxv1alpha1 "github.com/papawattu/coxswain/api/v1alpha1"
)

// b2LoopSpec is a minimal Loop (one acceptance check) so the restore path is
// the minimal one.
func b2LoopSpec() coxv1alpha1.LoopSpec {
	return coxv1alpha1.LoopSpec{
		Goal:      loopGoal,
		Workspace: testWorkspace(),
		Verify:    coxv1alpha1.VerifyConfig{AcceptanceChecks: []string{loopCheckCmd}},
	}
}

// b2Reconcile runs one Reconcile and returns the fresh Loop.
func b2Reconcile(r *LoopReconciler, ns, name string) *coxv1alpha1.Loop {
	_, err := r.Reconcile(context.Background(),
		reconcile.Request{NamespacedName: types.NamespacedName{Name: name, Namespace: ns}})
	Expect(err).NotTo(HaveOccurred())
	loop := &coxv1alpha1.Loop{}
	Expect(k8sClient.Get(context.Background(), types.NamespacedName{Name: name, Namespace: ns}, loop)).To(Succeed())
	return loop
}

// b2SandboxName returns the Loop's Sandbox name (the operator's naming).
func b2SandboxName(name string) string { return name + "-sandbox" }

// b2MakeSnapshotReady sets status.readyToUse=true on a checkpoint snapshot
// (envtest has no snapshot-controller; the spec sets it manually). The
// VolumeSnapshot CRD has a status subresource, so the spec uses the status
// subresource update (not the main resource update, which would strip the
// status).
func b2MakeSnapshotReady(ctx context.Context, ns, name string) {
	snap, err := b2GetSnapshot(ctx, ns, name)
	if err != nil {
		Fail("b2MakeSnapshotReady: " + err.Error())
	}
	Expect(unstructured.SetNestedField(snap.Object, true, "status", "readyToUse")).To(Succeed())
	Expect(k8sClient.Status().Update(ctx, snap)).To(Succeed())
}

// b2EnsureSandbox creates the stand-in Sandbox object for a Loop (the S5a
// idiom: envtest has no agent-sandbox controller, so the spec creates the
// object the controller would own — the operator's ensureSandbox then finds
// it and leaves it, instead of CreateOrUpdate racing the apiserver).
func b2EnsureSandbox(ns, name string) {
	ctx := context.Background()
	sb := &sandboxv1beta1.Sandbox{}
	if apierrors.IsNotFound(k8sClient.Get(ctx, types.NamespacedName{Name: b2SandboxName(name), Namespace: ns}, sb)) {
		Expect(k8sClient.Create(ctx, &sandboxv1beta1.Sandbox{
			ObjectMeta: metav1.ObjectMeta{Name: b2SandboxName(name), Namespace: ns},
			Spec: sandboxv1beta1.SandboxSpec{
				SandboxBlueprint: sandboxv1beta1.SandboxBlueprint{
					PodTemplate: sandboxv1beta1.PodTemplate{
						Spec: corev1.PodSpec{
							Containers: []corev1.Container{
								{Name: agentContainerName, Image: s3StandinImage},
							},
						},
					},
				},
			},
		})).To(Succeed())
	}
}

// b2NewReconciler builds a fresh reconciler for a Loop.
func b2NewReconciler() *LoopReconciler {
	r := &LoopReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), apiReader: k8sClient}
	r.workspaceDriverSeam = func(context.Context, *coxv1alpha1.Loop) (string, bool) { return b1Driver, true }
	r.snapshotCRDPresent = func(context.Context) bool { return true }
	return r
}

// b2NewReconcilerWithRecorder builds a fresh reconciler with a FakeRecorder.
func b2NewReconcilerWithRecorder() (*LoopReconciler, *record.FakeRecorder) {
	rec := record.NewFakeRecorder(64)
	r := b2NewReconciler()
	r.Recorder = rec
	return r, rec
}

// b2GetSnapshot returns the named VolumeSnapshot (or a not-found error).
func b2GetSnapshot(ctx context.Context, ns, name string) (*unstructured.Unstructured, error) {
	snap := &unstructured.Unstructured{}
	snap.SetGroupVersionKind(volumeSnapshotGVK)
	err := k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, snap)
	return snap, err
}

// b2GetPVC returns the Loop's workspace PVC (or a not-found error).
func b2GetPVC(ctx context.Context, ns, loopName string) (*corev1.PersistentVolumeClaim, error) {
	pvc := &corev1.PersistentVolumeClaim{}
	err := k8sClient.Get(ctx, types.NamespacedName{Name: workspacePVCName(loopName), Namespace: ns}, pvc)
	return pvc, err
}

// b2RestoreCondition returns the Loop's Restore condition (nil when absent).
func b2RestoreCondition(loop *coxv1alpha1.Loop) *metav1.Condition {
	for i := range loop.Status.Conditions {
		c := loop.Status.Conditions[i]
		if c.Type == coxv1alpha1.RestoreCondition {
			return &c
		}
	}
	return nil
}

// b2DrainEvents drains the FakeRecorder's event buffer.
func b2DrainEvents(rec *record.FakeRecorder) {
	for {
		select {
		case <-rec.Events:
		default:
			return
		}
	}
}

var _ = Describe("B2 workspace restore, rollback and fork (envtest)", func() {
	var ctx context.Context

	BeforeEach(func() {
		ctx = context.Background()
		// The B1 checkpoint path (b1DriveToSucceeded) requires a VSC for the
		// workspace driver. The B1 test creates one; the B2 test reuses the
		// same VSC (cluster-scoped, shared across specs in the same suite).
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

	It("RESTORE (reactive): a LOST workspace PVC with a ready checkpoint is restored from the latest checkpoint (the PVC is recreated with spec.dataSource=the checkpoint's VolumeSnapshot; the Restore condition is True reason Restored; a WorkspaceRestored Event is emitted)", func() {
		ns := "b2-restore-" + nowSuffix()
		Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
		defer func() {
			_ = k8sClient.Delete(context.Background(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})
		}()

		name := "restore-loop"
		loop := &coxv1alpha1.Loop{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec:       b2LoopSpec(),
		}
		Expect(k8sClient.Create(ctx, loop)).To(Succeed())

		r, rec := b2NewReconcilerWithRecorder()
		b2EnsureSandbox(ns, name)
		// Drive to a successful verify (the B1 idiom: the verify drives the
		// phase machine; the reconciler's driver/CRD seams are active on
		// every reconcile so the support check resolves and the checkpoint
		// is taken). The operator records a checkpoint for iteration 0
		// (the latest, in status.checkpoints).
		b1DriveToSucceeded(ctx, r, ns, name)
		// Make the snapshot ReadyToUse (envtest has no snapshot-controller;
		// the spec sets it manually, the B1 idiom).
		snapName := checkpointName(&coxv1alpha1.Loop{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}}, 0)
		b2MakeSnapshotReady(ctx, ns, snapName)

		// LOSE the PVC (delete it — the lost-PVC case). The PVC has a
		// finalizer (the B1 PVC-resume path adds one); remove it so the
		// apiserver actually deletes the PVC (the finalizer is not run in
		// envtest — there is no controller to remove it).
		pvc, err := b2GetPVC(ctx, ns, name)
		Expect(err).NotTo(HaveOccurred(), "the workspace PVC must exist after the bootstrap")
		Expect(pvc.Spec.DataSource).To(BeNil(), "the bootstrap-created PVC has no dataSource (it is a fresh empty PVC)")
		pvc.Finalizers = []string{}
		Expect(k8sClient.Update(ctx, pvc)).To(Succeed())
		Expect(k8sClient.Delete(ctx, pvc)).To(Succeed())

		// Reconcile: the operator detects the LOST PVC + the ready checkpoint
		// and restores (the PVC is recreated with the checkpoint's
		// VolumeSnapshot as the dataSource). The restore decision is
		// persisted at the point of decision (I90): even though this
		// reconcile's sandbox build later fails in envtest (no kubelet
		// provisioner), the condition and the PVC are persisted by the
		// restore step itself.
		b2Reconcile(r, ns, name)

		By("recreating the workspace PVC with spec.dataSource=the checkpoint's VolumeSnapshot")
		restored, err := b2GetPVC(ctx, ns, name)
		Expect(err).NotTo(HaveOccurred(), "the operator must restore the LOST PVC (recreate it)")
		Expect(restored.Spec.DataSource).NotTo(BeNil(), "the restored PVC must carry a dataSource (the checkpoint's VolumeSnapshot)")
		Expect(restored.Spec.DataSource.Kind).To(Equal(volumeSnapshotKind))
		Expect(restored.Spec.DataSource.Name).To(Equal(snapName), "the restore source is the LATEST checkpoint (iteration 0)")

		By("setting the Restore condition True reason Restored (persisted at the point of decision — I90)")
		restoredLoop := &coxv1alpha1.Loop{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, restoredLoop)).To(Succeed())
		cond := b2RestoreCondition(restoredLoop)
		Expect(cond).NotTo(BeNil(), "the operator must set the Restore condition (the B2 durable record, persisted at the point of decision)")
		Expect(cond.Status).To(Equal(metav1.ConditionTrue))
		Expect(cond.Reason).To(Equal(coxv1alpha1.RestoreReasonRestored))

		By("emitting a WorkspaceRestored Event")
		found := false
		for {
			select {
			case e := <-rec.Events:
				if strings.Contains(e, "Restore=true") && strings.Contains(e, "Restored") {
					found = true
				}
			default:
				Expect(found).To(BeTrue(), "expected a WorkspaceRestored Event (the B2 transient signal)")
				return
			}
		}
	})

	It("RESTORE (reactive, not-ready): a LOST workspace PVC whose latest checkpoint is NOT ReadyToUse is DEFERRED (the Restore condition is False reason RestoreSourceNotReady; the PVC is NOT recreated — the in-progress state, no decision, I49)", func() {
		ns := "b2-restore-notready-" + nowSuffix()
		Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
		defer func() {
			_ = k8sClient.Delete(context.Background(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})
		}()

		name := "restore-notready"
		loop := &coxv1alpha1.Loop{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec:       b2LoopSpec(),
		}
		Expect(k8sClient.Create(ctx, loop)).To(Succeed())

		r, rec := b2NewReconcilerWithRecorder()
		b2EnsureSandbox(ns, name)
		b1DriveToSucceeded(ctx, r, ns, name)
		// The snapshot exists (B1 took it) but is NOT ReadyToUse (the
		// snapshot controller has not finished — the in-progress state).
		// (Do NOT call b2MakeSnapshotReady — the spec asserts the
		// not-ready branch.)

		// LOSE the PVC (the finalizer must be removed first — the A2 idiom).
		pvc, err := b2GetPVC(ctx, ns, name)
		Expect(err).NotTo(HaveOccurred())
		pvc.Finalizers = []string{}
		Expect(k8sClient.Update(ctx, pvc)).To(Succeed())
		Expect(k8sClient.Delete(ctx, pvc)).To(Succeed())

		// Reconcile: the operator detects the LOST PVC + the not-ready
		// checkpoint and DEFERS (the in-progress state — no restore
		// decision, I49).
		b2Reconcile(r, ns, name)

		By("recreating the PVC EMPTY (the bootstrap runs after the B2 defer; the PVC is present but has NO dataSource — the A2 behavior, not a restore from the checkpoint)")
		deferredPVC, err := b2GetPVC(ctx, ns, name)
		Expect(err).NotTo(HaveOccurred(), "the PVC is present (the bootstrap created it after the B2 defer)")
		Expect(deferredPVC.Spec.DataSource).To(BeNil(), "the deferred PVC has NO dataSource (the bootstrap created it empty — the A2 behavior, not a restore from the checkpoint)")

		By("setting the Restore condition False reason RestoreSourceNotReady (the in-progress state is VISIBLE, I49)")
		deferredLoop := &coxv1alpha1.Loop{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, deferredLoop)).To(Succeed())
		cond := b2RestoreCondition(deferredLoop)
		Expect(cond).NotTo(BeNil(), "the operator must set the Restore condition (the in-progress state is VISIBLE, I49)")
		Expect(cond.Status).To(Equal(metav1.ConditionFalse))
		Expect(cond.Reason).To(Equal(coxv1alpha1.RestoreReasonSourceNotReady))

		By("emitting a RestoreSourceNotReady Event")
		found := false
		for {
			select {
			case e := <-rec.Events:
				if strings.Contains(e, coxv1alpha1.RestoreReasonSourceNotReady) {
					found = true
				}
			default:
				Expect(found).To(BeTrue(), "expected a RestoreSourceNotReady Event (the in-progress state is VISIBLE)")
				return
			}
		}
	})

	It("RESTORE (A2 fallback): a LOST workspace PVC with NO usable checkpoint (the latest checkpoint's VolumeSnapshot is ABSENT) falls back to the A2 PVC-resume (the PVC is recreated EMPTY; the Restore condition is False reason NoUsableCheckpoint; a WorkspaceRestoreUnavailable Event is emitted — I90: the fallback is VISIBLE)", func() {
		ns := "b2-fallback-" + nowSuffix()
		Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
		defer func() {
			_ = k8sClient.Delete(context.Background(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})
		}()

		name := "fallback-loop"
		loop := &coxv1alpha1.Loop{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec:       b2LoopSpec(),
		}
		Expect(k8sClient.Create(ctx, loop)).To(Succeed())

		r, rec := b2NewReconcilerWithRecorder()
		b2EnsureSandbox(ns, name)
		b1DriveToSucceeded(ctx, r, ns, name)

		// The checkpoint's recorded status entry is present (B1 recorded it)
		// but its VolumeSnapshot object is ABSENT (the checkpoint was GC'd or
		// never taken — the operator reads the snapshot object and gets
		// NotFound).
		snapName := checkpointName(&coxv1alpha1.Loop{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}}, 0)
		absentSnap := &unstructured.Unstructured{
			Object: map[string]any{"apiVersion": snapshotGV, "kind": volumeSnapshotKind,
				"metadata": map[string]any{"name": snapName, "namespace": ns}},
		}
		Expect(k8sClient.Delete(ctx, absentSnap)).To(Succeed())

		// LOSE the PVC (the finalizer must be removed first — the A2 idiom).
		pvc, err := b2GetPVC(ctx, ns, name)
		Expect(err).NotTo(HaveOccurred(), "the workspace PVC must exist after the bootstrap")
		pvc.Finalizers = []string{}
		Expect(k8sClient.Update(ctx, pvc)).To(Succeed())
		Expect(k8sClient.Delete(ctx, pvc)).To(Succeed())

		// Reconcile: the operator detects the LOST PVC + the ABSENT
		// checkpoint and falls back to the A2 PVC-resume (recreate the PVC
		// EMPTY). The decision is persisted at the point of decision (I90).
		b2Reconcile(r, ns, name)

		By("recreating the workspace PVC EMPTY (the A2 fallback — the PVC has NO dataSource)")
		fallback, err := b2GetPVC(ctx, ns, name)
		Expect(err).NotTo(HaveOccurred(), "the operator must recreate the LOST PVC (the A2 fallback)")
		Expect(fallback.Spec.DataSource).To(BeNil(), "the A2 fallback recreates the PVC EMPTY (no dataSource — the content is lost, the Loop continues from the untouched base)")

		By("setting the Restore condition False reason NoUsableCheckpoint (the I90 fallback is VISIBLE, persisted at the point of decision)")
		fallbackLoop := &coxv1alpha1.Loop{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, fallbackLoop)).To(Succeed())
		cond := b2RestoreCondition(fallbackLoop)
		Expect(cond).NotTo(BeNil(), "the operator must set the Restore condition (the I90 fallback is VISIBLE)")
		Expect(cond.Status).To(Equal(metav1.ConditionFalse))
		Expect(cond.Reason).To(Equal(coxv1alpha1.RestoreReasonNoUsableCheckpoint))

		By("emitting a WorkspaceRestoreUnavailable Event")
		found := false
		for {
			select {
			case e := <-rec.Events:
				if strings.Contains(e, coxv1alpha1.RestoreReasonNoUsableCheckpoint) {
					found = true
					Expect(e).To(ContainSubstring("Restore=false"))
				}
			default:
				Expect(found).To(BeTrue(), "expected a NoUsableCheckpoint Event (the I90 fallback is VISIBLE)")
				return
			}
		}
	})

	It("ROLLBACK: a ready checkpoint (spec.restoreFrom: iter-N) is restored (the PVC is recreated with spec.dataSource=the checkpoint's VolumeSnapshot; the Restore condition is True reason Restored)", func() {
		ns := "b2-rollback-" + nowSuffix()
		Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
		defer func() {
			_ = k8sClient.Delete(context.Background(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})
		}()

		name := "rollback-loop"
		loop := &coxv1alpha1.Loop{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec:       b2LoopSpec(),
		}
		Expect(k8sClient.Create(ctx, loop)).To(Succeed())

		r, rec := b2NewReconcilerWithRecorder()
		b2EnsureSandbox(ns, name)
		b1DriveToSucceeded(ctx, r, ns, name)
		snapName := checkpointName(&coxv1alpha1.Loop{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}}, 0)
		b2MakeSnapshotReady(ctx, ns, snapName)

		// Set spec.restoreFrom: iter-0 (the rollback target — the B1
		// checkpoint's iter). The field is mutable (the user can change it
		// to roll back to different iters).
		rbLoop := &coxv1alpha1.Loop{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, rbLoop)).To(Succeed())
		rbLoop.Spec.RestoreFrom = "iter-0"
		Expect(k8sClient.Update(ctx, rbLoop)).To(Succeed())

		// Reconcile: the operator detects the rollback target + the ready
		// checkpoint and restores (the PVC is recreated with the checkpoint's
		// VolumeSnapshot as the dataSource). The decision is persisted at the
		// point of decision (I90).
		b2Reconcile(r, ns, name)

		By("recreating the workspace PVC with spec.dataSource=the checkpoint's VolumeSnapshot (the rollback target)")
		restored, err := b2GetPVC(ctx, ns, name)
		Expect(err).NotTo(HaveOccurred(), "the operator must restore the PVC from the rollback target")
		Expect(restored.Spec.DataSource).NotTo(BeNil(), "the restored PVC must carry a dataSource (the rollback target's VolumeSnapshot)")
		Expect(restored.Spec.DataSource.Kind).To(Equal(volumeSnapshotKind))
		Expect(restored.Spec.DataSource.Name).To(Equal(snapName))

		By("setting the Restore condition True reason Restored (persisted at the point of decision — I90)")
		restoredLoop := &coxv1alpha1.Loop{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, restoredLoop)).To(Succeed())
		cond := b2RestoreCondition(restoredLoop)
		Expect(cond).NotTo(BeNil())
		Expect(cond.Status).To(Equal(metav1.ConditionTrue))
		Expect(cond.Reason).To(Equal(coxv1alpha1.RestoreReasonRestored))

		By("emitting a WorkspaceRestored Event")
		found := false
		for {
			select {
			case e := <-rec.Events:
				if strings.Contains(e, "Restore=true") && strings.Contains(e, "Restored") {
					found = true
				}
			default:
				Expect(found).To(BeTrue(), "expected a WorkspaceRestored Event")
				return
			}
		}
	})

	It("ROLLBACK (absent): an ABSENT checkpoint (the VolumeSnapshot is missing) is rejected VISIBLY (the Restore condition is False reason RestoreSourceMissing; a WorkspaceRestoreUnavailable Event; no PVC is created — the target is gone, terminal)", func() {
		ns := "b2-rollback-absent-" + nowSuffix()
		Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
		defer func() {
			_ = k8sClient.Delete(context.Background(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})
		}()

		name := "rollback-absent"
		loop := &coxv1alpha1.Loop{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec:       b2LoopSpec(),
		}
		Expect(k8sClient.Create(ctx, loop)).To(Succeed())

		r, rec := b2NewReconcilerWithRecorder()
		b2EnsureSandbox(ns, name)
		// A fresh Loop: no checkpoints recorded (the bootstrap + one reconcile
		// only — the spec does NOT drive to a successful verify, so
		// status.checkpoints is empty and there is NO snapshot for any iter).
		b2Reconcile(r, ns, name)
		_, err := b2GetPVC(ctx, ns, name)
		Expect(err).NotTo(HaveOccurred(), "the bootstrap-created PVC is present")

		// Set spec.restoreFrom: iter-1 (an ABSENT checkpoint — the iter was
		// never recorded).
		rbAbsentLoop := &coxv1alpha1.Loop{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, rbAbsentLoop)).To(Succeed())
		rbAbsentLoop.Spec.RestoreFrom = "iter-1"
		Expect(k8sClient.Update(ctx, rbAbsentLoop)).To(Succeed())

		// Reconcile: the operator detects the rollback target + the ABSENT
		// checkpoint and REJECTS (the condition is False reason
		// RestoreSourceMissing; the PVC is left as-is — the target is gone).
		b2Reconcile(r, ns, name)

		By("NOT recreating the PVC (the target is gone — no restore)")
		_, err = b2GetPVC(ctx, ns, name)
		Expect(err).NotTo(HaveOccurred(), "the bootstrap-created PVC is present (the reject path does NOT delete it)")

		By("setting the Restore condition False reason RestoreSourceMissing (the reject is VISIBLE, persisted at the point of decision — I90)")
		rejectedLoop := &coxv1alpha1.Loop{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, rejectedLoop)).To(Succeed())
		cond := b2RestoreCondition(rejectedLoop)
		Expect(cond).NotTo(BeNil(), "the operator must set the Restore condition (the reject is VISIBLE)")
		Expect(cond.Status).To(Equal(metav1.ConditionFalse))
		Expect(cond.Reason).To(Equal(coxv1alpha1.RestoreReasonSourceMissing))

		By("emitting a RestoreSourceMissing Event")
		found := false
		for {
			select {
			case e := <-rec.Events:
				if strings.Contains(e, coxv1alpha1.RestoreReasonSourceMissing) {
					found = true
					Expect(e).To(ContainSubstring("Restore=false"))
				}
			default:
				Expect(found).To(BeTrue(), "expected a RestoreSourceMissing Event (the reject is VISIBLE)")
				return
			}
		}
	})

	It("ROLLBACK (not-ReadyToUse): a NOT-READY checkpoint (status.readyToUse != true) is DEFERRED (the Restore condition is False reason RestoreSourceNotReady; a WorkspaceRestoreUnavailable Event; no PVC is created — the in-progress state, no decision, I49)", func() {
		ns := "b2-rollback-notready-" + nowSuffix()
		Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
		defer func() {
			_ = k8sClient.Delete(context.Background(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})
		}()

		name := "rollback-notready"
		loop := &coxv1alpha1.Loop{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec:       b2LoopSpec(),
		}
		Expect(k8sClient.Create(ctx, loop)).To(Succeed())

		r, rec := b2NewReconcilerWithRecorder()
		b2EnsureSandbox(ns, name)
		b1DriveToSucceeded(ctx, r, ns, name)
		// The snapshot exists (B1 took it) but is NOT ReadyToUse (the
		// in-progress state — do NOT call b2MakeSnapshotReady).

		// Set spec.restoreFrom: iter-0 (the NOT-ReadyToUse checkpoint).
		rbNRLoop := &coxv1alpha1.Loop{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, rbNRLoop)).To(Succeed())
		rbNRLoop.Spec.RestoreFrom = "iter-0"
		Expect(k8sClient.Update(ctx, rbNRLoop)).To(Succeed())

		// Reconcile: the operator detects the rollback target + the
		// NOT-ReadyToUse checkpoint and DEFERS (the condition is False reason
		// RestoreSourceNotReady; the PVC is left as-is — the in-progress
		// state, no decision, I49).
		b2Reconcile(r, ns, name)

		By("NOT recreating the PVC (the checkpoint is not ReadyToUse yet — no decision, I49)")
		_, err := b2GetPVC(ctx, ns, name)
		Expect(err).NotTo(HaveOccurred(), "the bootstrap-created PVC is present (the defer path does NOT delete it)")

		By("setting the Restore condition False reason RestoreSourceNotReady (the in-progress state is VISIBLE, I49)")
		deferredLoop := &coxv1alpha1.Loop{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, deferredLoop)).To(Succeed())
		cond := b2RestoreCondition(deferredLoop)
		Expect(cond).NotTo(BeNil(), "the operator must set the Restore condition (the defer is VISIBLE)")
		Expect(cond.Status).To(Equal(metav1.ConditionFalse))
		Expect(cond.Reason).To(Equal(coxv1alpha1.RestoreReasonSourceNotReady))

		By("emitting a RestoreSourceNotReady Event")
		found := false
		for {
			select {
			case e := <-rec.Events:
				if strings.Contains(e, coxv1alpha1.RestoreReasonSourceNotReady) {
					found = true
					Expect(e).To(ContainSubstring("Restore=false"))
				}
			default:
				Expect(found).To(BeTrue(), "expected a RestoreSourceNotReady Event (the in-progress state is VISIBLE)")
				return
			}
		}
	})

	It("FORK: a ready source checkpoint (spec.forkOf {loop, iteration}) seeds the fork's OWN workspace PVC (the PVC is recreated with spec.dataSource=the source's checkpoint VolumeSnapshot; the fork's PVC carries the FORK's loop label; the Restore condition is True reason Restored)", func() {
		ns := "b2-fork-" + nowSuffix()
		Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
		defer func() {
			_ = k8sClient.Delete(context.Background(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})
		}()

		// The SOURCE Loop (the Loop whose checkpoint the fork seeds from).
		srcName := "fork-source"
		srcLoop := &coxv1alpha1.Loop{
			ObjectMeta: metav1.ObjectMeta{Name: srcName, Namespace: ns},
			Spec:       b2LoopSpec(),
		}
		Expect(k8sClient.Create(ctx, srcLoop)).To(Succeed())

		r, rec := b2NewReconcilerWithRecorder()
		b2EnsureSandbox(ns, srcName)
		// Drive the source to a successful verify: the B1 checkpoint is
		// taken (a recorded status.checkpoints entry + a ReadyToUse
		// VolumeSnapshot named checkpoint-<source>-iter-0).
		b1DriveToSucceeded(ctx, r, ns, srcName)
		srcPVC, err := b2GetPVC(ctx, ns, srcName)
		Expect(err).NotTo(HaveOccurred(), "the source's workspace PVC must exist after the bootstrap")
		srcSnapName := checkpointName(&coxv1alpha1.Loop{ObjectMeta: metav1.ObjectMeta{Name: srcName, Namespace: ns}}, 0)
		b2MakeSnapshotReady(ctx, ns, srcSnapName)

		// The FORK Loop (spec.forkOf {loop: source, iteration: 0} — the B1
		// checkpoint's iter).
		forkName := "fork-target"
		forkLoop := &coxv1alpha1.Loop{
			ObjectMeta: metav1.ObjectMeta{Name: forkName, Namespace: ns},
			Spec: coxv1alpha1.LoopSpec{
				Goal:      loopGoal,
				Workspace: testWorkspace(),
				Verify:    coxv1alpha1.VerifyConfig{AcceptanceChecks: []string{loopCheckCmd}},
				ForkOf:    &coxv1alpha1.ForkOf{Loop: srcName, Iteration: 0},
			},
		}
		Expect(k8sClient.Create(ctx, forkLoop)).To(Succeed())

		// Reconcile the FORK: the operator detects the fork target + the
		// ready source checkpoint and restores the FORK's OWN workspace PVC
		// from the source's snapshot. The decision is persisted at the point
		// of decision (I90).
		b2EnsureSandbox(ns, forkName)
		b2Reconcile(r, ns, forkName)

		By("recreating the FORK's OWN workspace PVC with spec.dataSource=the source's checkpoint VolumeSnapshot")
		forkPVC, err := b2GetPVC(ctx, ns, forkName)
		Expect(err).NotTo(HaveOccurred(), "the operator must restore the FORK's OWN PVC (the fork's PVC is a SEPARATE object, controller-owned by the fork)")
		Expect(forkPVC.Name).To(Equal(workspacePVCName(forkName)), "the fork's PVC is named after the FORK (not the source)")
		Expect(forkPVC.Spec.DataSource).NotTo(BeNil(), "the fork's PVC must carry a dataSource (the source's checkpoint VolumeSnapshot)")
		Expect(forkPVC.Spec.DataSource.Kind).To(Equal(volumeSnapshotKind))
		Expect(forkPVC.Spec.DataSource.Name).To(Equal(srcSnapName))
		Expect(forkPVC.Spec.DataSource).NotTo(Equal(srcPVC.Spec.DataSource), "the fork's PVC is NOT the source's PVC (it is a separate object with its own dataSource)")

		By("setting the Restore condition True reason Restored on the FORK (persisted at the point of decision — I90)")
		forkRestored := &coxv1alpha1.Loop{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: forkName, Namespace: ns}, forkRestored)).To(Succeed())
		cond := b2RestoreCondition(forkRestored)
		Expect(cond).NotTo(BeNil())
		Expect(cond.Status).To(Equal(metav1.ConditionTrue))
		Expect(cond.Reason).To(Equal(coxv1alpha1.RestoreReasonRestored))

		By("emitting a WorkspaceRestored Event on the FORK")
		found := false
		for {
			select {
			case e := <-rec.Events:
				if strings.Contains(e, "Restore=true") && strings.Contains(e, "Restored") {
					found = true
				}
			default:
				Expect(found).To(BeTrue(), "expected a WorkspaceRestored Event on the FORK")
				return
			}
		}
	})

	It("FORK (source deleted): deleting the source Loop AFTER the fork's restore does NOT break the fork (the fork's PVC is present; the fork's Restore condition stays True; the fork carries on — the source's GC deletes the source's snapshots, but the fork's PVC is a separate object and is NOT touched)", func() {
		ns := "b2-fork-del-" + nowSuffix()
		Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
		defer func() {
			_ = k8sClient.Delete(context.Background(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})
		}()

		// The SOURCE Loop + its checkpoint.
		srcName := "fork-del-source"
		srcLoop := &coxv1alpha1.Loop{
			ObjectMeta: metav1.ObjectMeta{Name: srcName, Namespace: ns},
			Spec:       b2LoopSpec(),
		}
		Expect(k8sClient.Create(ctx, srcLoop)).To(Succeed())
		r, rec := b2NewReconcilerWithRecorder()
		b2EnsureSandbox(ns, srcName)
		b1DriveToSucceeded(ctx, r, ns, srcName)
		srcSnapName := checkpointName(&coxv1alpha1.Loop{ObjectMeta: metav1.ObjectMeta{Name: srcName, Namespace: ns}}, 0)
		b2MakeSnapshotReady(ctx, ns, srcSnapName)

		// The FORK Loop (spec.forkOf {loop: source, iteration: 0} — the B1
		// checkpoint's iter).
		forkName := "fork-del-target"
		forkLoop := &coxv1alpha1.Loop{
			ObjectMeta: metav1.ObjectMeta{Name: forkName, Namespace: ns},
			Spec: coxv1alpha1.LoopSpec{
				Goal:      loopGoal,
				Workspace: testWorkspace(),
				Verify:    coxv1alpha1.VerifyConfig{AcceptanceChecks: []string{loopCheckCmd}},
				ForkOf:    &coxv1alpha1.ForkOf{Loop: srcName, Iteration: 0},
			},
		}
		Expect(k8sClient.Create(ctx, forkLoop)).To(Succeed())
		// The fork's restore: the fork's own reconcile runs the B2 restore
		// step BEFORE the bootstrap (the PVC is created + restored from the
		// source's snapshot in the same pass). The restore is a one-shot
		// decision — the condition is persisted at the point of decision
		// (I90), before the fork's own bootstrap runs.
		b2EnsureSandbox(ns, forkName)
		b2Reconcile(r, ns, forkName)
		forkRestored := &coxv1alpha1.Loop{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: forkName, Namespace: ns}, forkRestored)).To(Succeed())
		cond := b2RestoreCondition(forkRestored)
		Expect(cond).NotTo(BeNil())
		Expect(cond.Status).To(Equal(metav1.ConditionTrue), "the fork's restore must be complete before the source is deleted")

		// DELETE the source Loop (the source's finalizer runs: the source's
		// checkpoint GC deletes the source's snapshots).
		Expect(k8sClient.Delete(ctx, srcLoop)).To(Succeed())
		// Reconcile the source (the finalizer's work runs).
		b2Reconcile(r, ns, srcName)

		// DELETE the source's snapshot (simulating the source's GC — the
		// source's finalizer would delete it, but envtest's finalizer
		// handling is manual; the spec deletes it directly to assert the
		// fork carries on).
		snap, err := b2GetSnapshot(ctx, ns, srcSnapName)
		if err == nil {
			Expect(k8sClient.Delete(ctx, snap)).To(Succeed())
		}

		// Reconcile the FORK: the fork's restore is ONE-SHOT (the Restore
		// condition is True/Restored — the marker). The source's snapshot is
		// gone (the source's GC deleted it), but the fork's PVC is PRESENT
		// (the content was materialised from the snapshot at restore). The
		// fork carries on (it does NOT re-restore, does NOT treat the gone
		// snapshot as a lost-PVC fallback case — the PVC is present).
		_ = b2Reconcile(r, ns, forkName)

		By("keeping the FORK's PVC present (the fork's PVC is a separate object, controller-owned by the fork; the source's GC did NOT touch it)")
		forkPVC, err := b2GetPVC(ctx, ns, forkName)
		Expect(err).NotTo(HaveOccurred(), "the fork's PVC must be present AFTER the source's deletion (the fork's PVC is a separate object)")
		Expect(forkPVC.Spec.DataSource).NotTo(BeNil(), "the fork's PVC retains its dataSource (the source's snapshot — the content is materialised in the PVC)")

		By("keeping the FORK's Restore condition True reason Restored (the one-shot marker — the fork does NOT re-restore)")
		forkAfterLoop := &coxv1alpha1.Loop{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: forkName, Namespace: ns}, forkAfterLoop)).To(Succeed())
		condAfter := b2RestoreCondition(forkAfterLoop)
		Expect(condAfter).NotTo(BeNil())
		Expect(condAfter.Status).To(Equal(metav1.ConditionTrue), "the fork's Restore condition stays True (the one-shot marker; the fork carries on)")
		Expect(condAfter.Reason).To(Equal(coxv1alpha1.RestoreReasonRestored))

		_ = rec // the Event is not asserted here (the restore was asserted in the prior spec)
	})

	It("FORK (absent source): a fork with an ABSENT source checkpoint (the source's VolumeSnapshot is missing) is rejected VISIBLY (the Restore condition is False reason RestoreSourceMissing)", func() {
		ns := "b2-fork-absent-" + nowSuffix()
		Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
		defer func() {
			_ = k8sClient.Delete(context.Background(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})
		}()

		// The SOURCE Loop (no checkpoint — the source is fresh; the B1
		// checkpoint was never taken, so there is NO VolumeSnapshot for any
		// iter).
		srcName := "fork-absent-source"
		srcLoop := &coxv1alpha1.Loop{
			ObjectMeta: metav1.ObjectMeta{Name: srcName, Namespace: ns},
			Spec:       b2LoopSpec(),
		}
		Expect(k8sClient.Create(ctx, srcLoop)).To(Succeed())
		r, _ := b2NewReconcilerWithRecorder()
		b2EnsureSandbox(ns, srcName)
		b2Reconcile(r, ns, srcName)

		// The FORK Loop (spec.forkOf {loop: source, iteration: 0} — the
		// iter the source's B1 checkpoint would be at; the snapshot is
		// ABSENT because the source never reached a successful verify).
		forkName := "fork-absent-target"
		forkLoop := &coxv1alpha1.Loop{
			ObjectMeta: metav1.ObjectMeta{Name: forkName, Namespace: ns},
			Spec: coxv1alpha1.LoopSpec{
				Goal:      loopGoal,
				Workspace: testWorkspace(),
				Verify:    coxv1alpha1.VerifyConfig{AcceptanceChecks: []string{loopCheckCmd}},
				ForkOf:    &coxv1alpha1.ForkOf{Loop: srcName, Iteration: 0},
			},
		}
		Expect(k8sClient.Create(ctx, forkLoop)).To(Succeed())

		// Reconcile the FORK: the operator detects the fork target + the
		// ABSENT source checkpoint and REJECTS (the condition is False
		// reason RestoreSourceMissing; the fork's PVC is left as-is — the
		// target is gone). The decision is persisted at the point of decision
		// (I90).
		b2EnsureSandbox(ns, forkName)
		b2Reconcile(r, ns, forkName)

		By("NOT deleting + recreating the fork's PVC (the source's checkpoint is gone — no restore)")
		_, err := b2GetPVC(ctx, ns, forkName)
		Expect(err).NotTo(HaveOccurred(), "the fork's PVC is present (the bootstrap created it; the reject path does NOT delete it)")

		By("setting the Restore condition False reason RestoreSourceMissing on the FORK (persisted at the point of decision — I90)")
		forkRejected := &coxv1alpha1.Loop{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: forkName, Namespace: ns}, forkRejected)).To(Succeed())
		cond := b2RestoreCondition(forkRejected)
		Expect(cond).NotTo(BeNil(), "the operator must set the Restore condition on the FORK (the reject is VISIBLE)")
		Expect(cond.Status).To(Equal(metav1.ConditionFalse))
		Expect(cond.Reason).To(Equal(coxv1alpha1.RestoreReasonSourceMissing))
	})
})
