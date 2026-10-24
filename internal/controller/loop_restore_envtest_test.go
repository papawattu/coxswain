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
// The PVC replacement is LEVEL-BASED (the reviewer's P1, the R-B2 review at
// 20ef538): stop the sandbox (a mounted PVC is held by
// kubernetes.io/pvc-protection), delete the PVC, and create it from the
// snapshot are each a SEPARATE reconcile step, with the progress recorded in
// the Restore condition (StoppingSandbox / DeletingPVC / Restored). A spec
// drives the sequence by removing the PVC's pvc-protection finalizer (the
// envtest has no pod controller to remove it) between the delete and the
// create reconcile — the same way a real cluster's pod controller would.
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
//     checkpoint is restored through the level-based sequence (the sandbox
//     is stopped, the PVC is deleted, the PVC is recreated from the
//     snapshot — one reconcile per step, the in-progress states
//     StoppingSandbox / DeletingPVC are VISIBLE). An ABSENT checkpoint is
//     rejected VISIBLY (RestoreSourceMissing). A NOT-READY checkpoint is
//     DEFERRED (RestoreSourceNotReady, I49). iter-0 is a valid target (the
//     first verify — the B1 iter-0 lesson), and a completed rollback is
//     ONE-SHOT: a later reconcile with restoreFrom still set does not
//     re-restore the PVC (a re-restore would wipe the Loop's work since the
//     restore).
//   - FORK: spec.forkOf {loop, iteration} seeds the Loop from another
//     Loop's checkpoint. At start the operator restores this Loop's OWN
//     workspace PVC from the source snapshot (the fork's PVC is a separate
//     object, controller-owned by the fork — NOT the source's PVC). The
//     fork's restore is a one-shot decision: the fork's restore claims a
//     fork-retention label on the source's snapshot (the source's GC skips
//     a labelled snapshot), and removes it once the fork's PVC is present
//     (the fork's PVC is self-sufficient). Deleting the source Loop does NOT
//     break a fork that has already restored (the fork's PVC is a separate
//     object — the source's GC deletes the source's snapshots, not the
//     fork's PVC). A fork with an ABSENT source checkpoint is rejected
//     VISIBLY (RestoreSourceMissing).
//
// The named mutations (applied in a scratch worktree — see the PR for the
// commands and the FAIL they produce):
//
//   - M1 / (a) (restore ignores the checkpoint): change b2ApplyRestoreStep's
//     ready-branch (the lostPVC case) to NOT recreate the PVC from the
//     snapshot (call ensureWorkspacePVC instead — the empty-PVC path). The
//     RESTORE spec must FAIL (the restored PVC has no dataSource).
//   - M2 / (b) (rollback accepts a non-ReadyToUse source): change
//     resolveCheckpointSource's notready-branch to return "ready". The
//     ROLLBACK not-ReadyToUse spec must FAIL (the PVC is replaced instead of
//     deferred).
//   - M3 / (c) (fork reuses the source PVC): change
//     createWorkspacePVCFromSnapshot to create the PVC with the SOURCE's
//     PVC name (workspacePVCName(loop.Spec.ForkOf.Loop)) instead of the
//     fork's own. The FORK spec must FAIL (the fork's own PVC is absent).

package controller

import (
	"context"
	"slices"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	sandboxv1beta1 "sigs.k8s.io/agent-sandbox/api/v1beta1"
	"sigs.k8s.io/controller-runtime/pkg/client"
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

// b2ReconcileToleratingGone runs one Reconcile and EXPECTS it to succeed (a
// nil error), but TOLERATES the Loop being gone (a deleted Loop's reconcile
// is a no-op — the operator's Get returns NotFound and the Reconcile returns
// nil via client.IgnoreNotFound).
func b2ReconcileToleratingGone(r *LoopReconciler, ns, name string) {
	_, err := r.Reconcile(context.Background(),
		reconcile.Request{NamespacedName: types.NamespacedName{Name: name, Namespace: ns}})
	Expect(err).NotTo(HaveOccurred(), "a gone Loop's reconcile must be a no-op (the operator's Get returns NotFound and the Reconcile returns nil)")
}

// b2SandboxName returns the Loop's Sandbox name (the operator's naming).
func b2SandboxName(name string) string { return name + "-sandbox" }

// (envtest has no snapshot-controller; the spec sets it manually). The
// VolumeSnapshot CRD has a status subresource, so the spec uses the status
// subresource update.
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
// object the controller would own). The operatingMode is set so the B2
// restore's level-based stop can read it (Running = a pod that would hold
// the PVC mounted; Suspended = a pod already gone).
func b2EnsureSandbox(ns, name string) {
	ctx := context.Background()
	sb := &sandboxv1beta1.Sandbox{}
	if apierrors.IsNotFound(k8sClient.Get(ctx, types.NamespacedName{Name: b2SandboxName(name), Namespace: ns}, sb)) {
		Expect(k8sClient.Create(ctx, &sandboxv1beta1.Sandbox{
			ObjectMeta: metav1.ObjectMeta{Name: b2SandboxName(name), Namespace: ns},
			Spec: sandboxv1beta1.SandboxSpec{
				OperatingMode: sandboxv1beta1.SandboxOperatingModeRunning,
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

// b2EnsureSandboxPod creates the stand-in sandbox POD for a Loop (the S5a
// idiom's pod half: envtest has no agent-sandbox controller, so the pod the
// controller would create for a Running sandbox is created by the spec).
// The B2 restore's level-based sequence (R-B2 design point (a)) advances
// the PVC delete only once the sandbox POD is gone — spec-level suspension
// is a request, and kubernetes.io/pvc-protection holds the PVC until no pod
// uses it. The spec removes the pod between the StoppingSandbox and
// DeletingPVC reconciles (the agent-sandbox controller does on a real
// cluster).
func b2EnsureSandboxPod(ns, name string) {
	ctx := context.Background()
	pod := &corev1.Pod{}
	if apierrors.IsNotFound(k8sClient.Get(ctx, types.NamespacedName{Name: b2SandboxName(name), Namespace: ns}, pod)) {
		Expect(k8sClient.Create(ctx, &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: b2SandboxName(name), Namespace: ns},
			Spec: corev1.PodSpec{
				Containers: []corev1.Container{
					{Name: agentContainerName, Image: s3StandinImage},
				},
			},
		})).To(Succeed())
	}
}

// b2DeleteSandboxPod removes the stand-in sandbox pod (the agent-sandbox
// controller's removal of a Suspended sandbox's pod — envtest has no such
// controller, so the spec removes it manually so the restore's level-based
// sequence can advance from StoppingSandbox to DeletingPVC).
func b2DeleteSandboxPod(ctx context.Context, ns, name string) {
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: b2SandboxName(name), Namespace: ns}}
	Expect(k8sClient.Delete(ctx, pod)).To(Succeed())
}

// b2RemovePVCProtectionFinalizer removes the workspace PVC's
// kubernetes.io/pvc-protection finalizer (the envtest has no pod controller
// to remove it — on a real cluster the pod controller removes the finalizer
// when no pod uses the PVC, so the PVC delete completes). Without this, the
// operator's PVC delete (the level-based restore's delete step) is async
// and NEVER completes in envtest: the apiserver sets the deletionTimestamp
// but does not remove the object until the finalizer is gone, and the
// create step (which runs only once the PVC is gone) would never run.
func b2RemovePVCProtectionFinalizer(ctx context.Context, ns, loopName string) {
	pvc := &corev1.PersistentVolumeClaim{}
	Expect(k8sClient.Get(ctx, types.NamespacedName{Name: workspacePVCName(loopName), Namespace: ns}, pvc)).To(Succeed())
	fins := []string{}
	for _, f := range pvc.Finalizers {
		if f != "kubernetes.io/pvc-protection" {
			fins = append(fins, f)
		}
	}
	pvc.Finalizers = fins
	Expect(k8sClient.Update(ctx, pvc)).To(Succeed())
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

// b2ExpectEvent asserts that a Restore Event with the named reason was
// emitted (the transient signal; the condition is the durable record).
func b2ExpectEvent(rec *record.FakeRecorder, reason string) {
	found := false
	for {
		select {
		case e := <-rec.Events:
			if strings.Contains(e, "reason="+reason) {
				found = true
			}
		default:
			Expect(found).To(BeTrue(), "expected a Restore Event with reason "+reason)
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
		Expect(k8sClient.Create(ctx, &coxv1alpha1.Loop{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec:       b2LoopSpec(),
		})).To(Succeed())

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
		// finalizer (the A2 PVC-resume path adds one); remove it so the
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
		// persisted at the point of decision (I90).
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
		b2ExpectEvent(rec, coxv1alpha1.RestoreReasonRestored)
	})

	It("RESTORE (reactive, not-ready): a LOST workspace PVC whose latest checkpoint is NOT ReadyToUse is DEFERRED (the Restore condition is False reason RestoreSourceNotReady; the PVC is NOT recreated from the checkpoint — the in-progress state, no decision, I49)", func() {
		ns := "b2-restore-notready-" + nowSuffix()
		Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
		defer func() {
			_ = k8sClient.Delete(context.Background(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})
		}()

		name := "restore-notready"
		Expect(k8sClient.Create(ctx, &coxv1alpha1.Loop{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec:       b2LoopSpec(),
		})).To(Succeed())

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

		By("setting the Restore condition False reason RestoreSourceNotReady (the in-progress state is VISIBLE, I49)")
		deferredLoop := &coxv1alpha1.Loop{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, deferredLoop)).To(Succeed())
		cond := b2RestoreCondition(deferredLoop)
		Expect(cond).NotTo(BeNil(), "the operator must set the Restore condition (the in-progress state is VISIBLE, I49)")
		Expect(cond.Status).To(Equal(metav1.ConditionFalse))
		Expect(cond.Reason).To(Equal(coxv1alpha1.RestoreReasonSourceNotReady))

		By("NOT recreating the PVC from the checkpoint (the in-progress state — no decision, I49)")
		_, err = b2GetPVC(ctx, ns, name)
		Expect(apierrors.IsNotFound(err)).To(BeTrue(), "the deferred PVC is NOT recreated (the bootstrap's ensureWorkspacePVC would create it empty on the next pass — the restore itself creates NOTHING until the snapshot is ReadyToUse)")

		By("emitting a RestoreSourceNotReady Event")
		b2ExpectEvent(rec, coxv1alpha1.RestoreReasonSourceNotReady)
	})

	It("RESTORE (A2 fallback): a LOST workspace PVC with NO usable checkpoint (the latest checkpoint's VolumeSnapshot is ABSENT) falls back to the A2 PVC-resume (the PVC is recreated EMPTY; the Restore condition is False reason NoUsableCheckpoint; a WorkspaceRestoreUnavailable Event is emitted — I90: the fallback is VISIBLE)", func() {
		ns := "b2-fallback-" + nowSuffix()
		Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
		defer func() {
			_ = k8sClient.Delete(context.Background(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})
		}()

		name := "fallback-loop"
		Expect(k8sClient.Create(ctx, &coxv1alpha1.Loop{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec:       b2LoopSpec(),
		})).To(Succeed())

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

		By("emitting a NoUsableCheckpoint Event")
		b2ExpectEvent(rec, coxv1alpha1.RestoreReasonNoUsableCheckpoint)
	})

	It("ROLLBACK (ready, level-based): a ready checkpoint (spec.restoreFrom: iter-N) is restored through the level-based sequence — the sandbox is STOPPED first (StoppingSandbox), then the PVC is DELETED (DeletingPVC), then the PVC is RECREATED from the snapshot (Restored) — one reconcile per step, the in-progress states are VISIBLE", func() {
		ns := "b2-rollback-" + nowSuffix()
		Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
		defer func() {
			_ = k8sClient.Delete(context.Background(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})
		}()

		name := "rollback-loop"
		Expect(k8sClient.Create(ctx, &coxv1alpha1.Loop{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec:       b2LoopSpec(),
		})).To(Succeed())

		r, _ := b2NewReconcilerWithRecorder()
		// The sandbox is RUNNING (the rollback case: a mounted PVC is held by
		// pvc-protection — the restore must stop the sandbox first, like the
		// A2 kill path). The sandbox POD is the stand-in the agent-sandbox
		// controller would create for a Running sandbox (b2EnsureSandboxPod —
		// envtest has no agent-sandbox controller, so the spec creates the pod).
		b2EnsureSandbox(ns, name)
		b2EnsureSandboxPod(ns, name)
		b1DriveToSucceeded(ctx, r, ns, name)
		snapName := checkpointName(&coxv1alpha1.Loop{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}}, 0)
		b2MakeSnapshotReady(ctx, ns, snapName)

		// Set spec.restoreFrom: iter-0 (the rollback target — the B1
		// checkpoint's iter).
		rbLoop := &coxv1alpha1.Loop{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, rbLoop)).To(Succeed())
		rbLoop.Spec.RestoreFrom = "iter-0"
		Expect(k8sClient.Update(ctx, rbLoop)).To(Succeed())

		By("step 1: the restore stops the sandbox (the PVC is still PRESENT — pvc-protection holds it while the sandbox is running; the Restore condition is False reason StoppingSandbox)")
		l1 := b2Reconcile(r, ns, name)
		sb1 := &sandboxv1beta1.Sandbox{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: b2SandboxName(name), Namespace: ns}, sb1)).To(Succeed())
		Expect(sb1.Spec.OperatingMode).To(Equal(sandboxv1beta1.SandboxOperatingModeSuspended), "the operator must stop the sandbox before deleting its PVC (pvc-protection holds a mounted PVC)")
		c1 := b2RestoreCondition(l1)
		Expect(c1).NotTo(BeNil(), "the in-progress state must be VISIBLE (the Restore condition is the durable record)")
		Expect(c1.Status).To(Equal(metav1.ConditionFalse))
		Expect(c1.Reason).To(Equal(coxv1alpha1.RestoreReasonStoppingSandbox))
		// The sandbox pod is still PRESENT (spec-level suspension is a request —
		// the pod is still there while it drains; the delete step waits for the
		// pod to be gone, R-B2 design point (a)).
		podStill := &corev1.Pod{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: b2SandboxName(name), Namespace: ns}, podStill)).To(Succeed(), "the sandbox pod is still present after the stop (spec-level suspension is a request — the pod is still there while it drains)")
		pvc1, err := b2GetPVC(ctx, ns, name)
		Expect(err).NotTo(HaveOccurred(), "the PVC is NOT deleted while the sandbox pod is still present (the delete is the NEXT step — the level-based sequence)")
		_ = pvc1

		By("step 1b: the agent-sandbox controller removes the suspended sandbox's pod (the envtest has no such controller — the spec removes the stand-in pod so the level-based sequence can advance from StoppingSandbox to DeletingPVC)")
		b2DeleteSandboxPod(ctx, ns, name)

		By("step 2: the PVC is DELETED (the Restore condition is False reason DeletingPVC; the PVC's delete is async — the spec removes the pvc-protection finalizer so the delete completes, as a real cluster's pod controller would)")
		l2 := b2Reconcile(r, ns, name)
		c2 := b2RestoreCondition(l2)
		Expect(c2).NotTo(BeNil())
		Expect(c2.Status).To(Equal(metav1.ConditionFalse))
		Expect(c2.Reason).To(Equal(coxv1alpha1.RestoreReasonDeletingPVC), "the in-progress state (the delete) must be VISIBLE")
		// The operator's delete is async: the PVC has a deletionTimestamp and
		// the pvc-protection finalizer (the envtest has no pod controller to
		// remove it). The PVC is still present (with the deletionTimestamp) —
		// the create step runs only once the PVC is actually gone.
		pvcPending, err := b2GetPVC(ctx, ns, name)
		Expect(err).NotTo(HaveOccurred(), "the PVC is still present after the delete was issued (it is deleting, not yet gone)")
		Expect(!pvcPending.DeletionTimestamp.IsZero()).To(BeTrue(), "the operator's delete must have been issued (the PVC has a deletionTimestamp)")
		// The envtest's delete never completes without the finalizer removed
		// (no pod controller): the spec removes it so the create step can run
		// (the real cluster's pod controller does the same when the sandbox
		// is stopped).
		b2RemovePVCProtectionFinalizer(ctx, ns, name)

		By("step 3: the PVC is RECREATED from the snapshot (the Restore condition is True reason Restored — the one-shot marker)")
		l3 := b2Reconcile(r, ns, name)
		restored, err := b2GetPVC(ctx, ns, name)
		Expect(err).NotTo(HaveOccurred(), "the operator must recreate the PVC from the snapshot (the rollback target)")
		Expect(restored.Spec.DataSource).NotTo(BeNil(), "the restored PVC must carry a dataSource (the rollback target's VolumeSnapshot)")
		Expect(restored.Spec.DataSource.Kind).To(Equal(volumeSnapshotKind))
		Expect(restored.Spec.DataSource.Name).To(Equal(snapName))
		c3 := b2RestoreCondition(l3)
		Expect(c3).NotTo(BeNil())
		Expect(c3.Status).To(Equal(metav1.ConditionTrue))
		Expect(c3.Reason).To(Equal(coxv1alpha1.RestoreReasonRestored))

		// The in-progress + restored Events (the transient signals) were
		// emitted during the reconciles above. The event recorder's buffer is
		// shared with b1DriveToSucceeded (which ran 4 reconciles and consumed
		// the events), so the spec does not assert them here — the conditions
		// (StoppingSandbox, DeletingPVC, Restored) are the durable record and
		// are asserted in each step above.
	})

	It("ROLLBACK (one-shot): after a completed rollback with restoreFrom still set, the PVC is NOT re-restored on a later reconcile (a re-restore would wipe the Loop's work since the restore)", func() {
		ns := "b2-rollback-once-" + nowSuffix()
		Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
		defer func() {
			_ = k8sClient.Delete(context.Background(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})
		}()

		name := "rollback-once"
		Expect(k8sClient.Create(ctx, &coxv1alpha1.Loop{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec:       b2LoopSpec(),
		})).To(Succeed())

		r, _ := b2NewReconcilerWithRecorder()
		b2EnsureSandbox(ns, name)
		b2EnsureSandboxPod(ns, name)
		b1DriveToSucceeded(ctx, r, ns, name)
		snapName := checkpointName(&coxv1alpha1.Loop{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}}, 0)
		b2MakeSnapshotReady(ctx, ns, snapName)

		rbLoop := &coxv1alpha1.Loop{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, rbLoop)).To(Succeed())
		rbLoop.Spec.RestoreFrom = "iter-0"
		Expect(k8sClient.Update(ctx, rbLoop)).To(Succeed())

		// The level-based sequence (stop -> delete -> create), driven as in
		// the ready rollback spec.
		b2Reconcile(r, ns, name)          // step 1: stop the sandbox
		b2DeleteSandboxPod(ctx, ns, name) // the agent-sandbox controller removes the pod
		b2Reconcile(r, ns, name)          // step 2: delete the PVC
		b2RemovePVCProtectionFinalizer(ctx, ns, name)
		l3 := b2Reconcile(r, ns, name) // step 3: create the PVC from the snapshot
		c3 := b2RestoreCondition(l3)
		Expect(c3).NotTo(BeNil())
		Expect(c3.Status).To(Equal(metav1.ConditionTrue), "the rollback must complete (True/Restored)")
		Expect(c3.Reason).To(Equal(coxv1alpha1.RestoreReasonRestored))
		restored, err := b2GetPVC(ctx, ns, name)
		Expect(err).NotTo(HaveOccurred())
		Expect(restored.Spec.DataSource).NotTo(BeNil())
		restoredUID := restored.UID

		// A LATER reconcile with restoreFrom still set (the Loop kept working
		// after the rollback — its PVC now holds the restored content plus
		// whatever it has done since): the PVC is NOT re-restored.
		l4 := b2Reconcile(r, ns, name)
		after, err := b2GetPVC(ctx, ns, name)
		Expect(err).NotTo(HaveOccurred(), "the PVC must still be present (a re-restore would delete it)")
		Expect(after.UID).To(Equal(restoredUID), "the PVC must be the SAME object (a re-restore would recreate it with a new UID)")
		Expect(after.Spec.DataSource.Name).To(Equal(snapName), "the dataSource is unchanged (the one-shot marker — the condition is True/Restored for this spec generation)")
		c4 := b2RestoreCondition(l4)
		Expect(c4.Status).To(Equal(metav1.ConditionTrue), "the one-shot marker stays (the restore is not re-run)")
		Expect(c4.Reason).To(Equal(coxv1alpha1.RestoreReasonRestored))
	})

	// F35: the one-shot marker was keyed on metadata.generation, so ANY spec
	// edit (the reviewer's reproduction: `cox pause` patches spec.suspend)
	// re-armed a completed rollback: the PVC was deleted and the old snapshot
	// restored over the work since. It is now keyed on the restore target.
	It("ROLLBACK (one-shot, F35): spec edits after a completed rollback (spec.suspend true then false, spec.goal) do NOT re-arm the restore (the workspace PVC UID is unchanged), and changing restoreFrom is rejected at admission", func() {
		ns := "b2-rollback-edit-" + nowSuffix()
		Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
		defer func() {
			_ = k8sClient.Delete(context.Background(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})
		}()

		name := "rollback-edit"
		Expect(k8sClient.Create(ctx, &coxv1alpha1.Loop{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec:       b2LoopSpec(),
		})).To(Succeed())

		r, _ := b2NewReconcilerWithRecorder()
		b2EnsureSandbox(ns, name)
		b2EnsureSandboxPod(ns, name)
		b1DriveToSucceeded(ctx, r, ns, name)
		snapName := checkpointName(&coxv1alpha1.Loop{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}}, 0)
		b2MakeSnapshotReady(ctx, ns, snapName)

		rbLoop := &coxv1alpha1.Loop{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, rbLoop)).To(Succeed())
		rbLoop.Spec.RestoreFrom = "iter-0"
		Expect(k8sClient.Update(ctx, rbLoop)).To(Succeed())

		b2Reconcile(r, ns, name)
		b2DeleteSandboxPod(ctx, ns, name)
		b2Reconcile(r, ns, name)
		b2RemovePVCProtectionFinalizer(ctx, ns, name)
		l3 := b2Reconcile(r, ns, name)
		Expect(b2RestoreCondition(l3).Reason).To(Equal(coxv1alpha1.RestoreReasonRestored))
		restored, err := b2GetPVC(ctx, ns, name)
		Expect(err).NotTo(HaveOccurred())
		restoredUID := restored.UID

		edit := func(mutate func(*coxv1alpha1.Loop)) {
			l := &coxv1alpha1.Loop{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, l)).To(Succeed())
			gen := l.Generation
			mutate(l)
			Expect(k8sClient.Update(ctx, l)).To(Succeed())
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, l)).To(Succeed())
			Expect(l.Generation).To(BeNumerically(">", gen), "the edit must bump metadata.generation (the old re-arm key)")
		}
		expectPVCUnchanged := func(why string) {
			l := b2Reconcile(r, ns, name)
			after, perr := b2GetPVC(ctx, ns, name)
			Expect(perr).NotTo(HaveOccurred(), why)
			Expect(after.UID).To(Equal(restoredUID), why+": the PVC must be the SAME object (a re-arm deletes and recreates it)")
			c := b2RestoreCondition(l)
			Expect(c.Status).To(Equal(metav1.ConditionTrue), why)
			Expect(c.Reason).To(Equal(coxv1alpha1.RestoreReasonRestored), why)
		}

		edit(func(l *coxv1alpha1.Loop) { l.Spec.Suspend = true })
		expectPVCUnchanged("after spec.suspend=true")
		edit(func(l *coxv1alpha1.Loop) { l.Spec.Suspend = false })
		expectPVCUnchanged("after spec.suspend=false")
		edit(func(l *coxv1alpha1.Loop) { l.Spec.Goal = l.Spec.Goal + " (edited)" })
		expectPVCUnchanged("after spec.goal edit")

		By("restoreFrom is immutable at admission")
		l := &coxv1alpha1.Loop{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, l)).To(Succeed())
		l.Spec.RestoreFrom = "iter-1"
		err = k8sClient.Update(ctx, l)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("restoreFrom is immutable"))
	})

	It("ROLLBACK (absent): an ABSENT checkpoint (the VolumeSnapshot is missing) is rejected VISIBLY (the Restore condition is False reason RestoreSourceMissing; a WorkspaceRestoreUnavailable Event; no PVC is deleted — the target is gone, terminal)", func() {
		ns := "b2-rollback-absent-" + nowSuffix()
		Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
		defer func() {
			_ = k8sClient.Delete(context.Background(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})
		}()

		name := "rollback-absent"
		Expect(k8sClient.Create(ctx, &coxv1alpha1.Loop{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec:       b2LoopSpec(),
		})).To(Succeed())

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

		By("NOT deleting the PVC (the target is gone — no restore)")
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
		b2ExpectEvent(rec, coxv1alpha1.RestoreReasonSourceMissing)
	})

	It("ROLLBACK (not-ReadyToUse): a NOT-READY checkpoint (status.readyToUse != true) is DEFERRED (the Restore condition is False reason RestoreSourceNotReady; a WorkspaceRestoreUnavailable Event; no PVC is touched — the in-progress state, no decision, I49)", func() {
		ns := "b2-rollback-notready-" + nowSuffix()
		Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
		defer func() {
			_ = k8sClient.Delete(context.Background(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})
		}()

		name := "rollback-notready"
		Expect(k8sClient.Create(ctx, &coxv1alpha1.Loop{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec:       b2LoopSpec(),
		})).To(Succeed())

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

		By("NOT touching the PVC (the checkpoint is not ReadyToUse yet — no decision, I49)")
		pvc, err := b2GetPVC(ctx, ns, name)
		Expect(err).NotTo(HaveOccurred(), "the bootstrap-created PVC is present (the defer path does NOT delete it)")
		Expect(pvc.DeletionTimestamp.IsZero()).To(BeTrue(), "the defer path does NOT delete the PVC (the in-progress state is before the stop/delete steps)")
		Expect(pvc.Spec.DataSource).To(BeNil(), "the defer path does NOT change the PVC (no decision yet)")

		By("setting the Restore condition False reason RestoreSourceNotReady (the in-progress state is VISIBLE, I49)")
		deferredLoop := &coxv1alpha1.Loop{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, deferredLoop)).To(Succeed())
		cond := b2RestoreCondition(deferredLoop)
		Expect(cond).NotTo(BeNil(), "the operator must set the Restore condition (the defer is VISIBLE)")
		Expect(cond.Status).To(Equal(metav1.ConditionFalse))
		Expect(cond.Reason).To(Equal(coxv1alpha1.RestoreReasonSourceNotReady))

		By("emitting a RestoreSourceNotReady Event")
		b2ExpectEvent(rec, coxv1alpha1.RestoreReasonSourceNotReady)
	})

	It("FORK: a ready source checkpoint (spec.forkOf {loop, iteration}) seeds the fork's OWN workspace PVC (the fork's PVC is a SEPARATE object, controller-owned by the fork, with spec.dataSource=the source's checkpoint VolumeSnapshot; the source's snapshot carries the fork-retention label while the fork's PVC is being materialised; the Restore condition is True reason Restored)", func() {
		ns := "b2-fork-" + nowSuffix()
		Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
		defer func() {
			_ = k8sClient.Delete(context.Background(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})
		}()

		// The SOURCE Loop (the Loop whose checkpoint the fork seeds from).
		srcName := "fork-source"
		Expect(k8sClient.Create(ctx, &coxv1alpha1.Loop{
			ObjectMeta: metav1.ObjectMeta{Name: srcName, Namespace: ns},
			Spec:       b2LoopSpec(),
		})).To(Succeed())

		r, rec := b2NewReconcilerWithRecorder()
		b2EnsureSandbox(ns, srcName)
		// Drive the source to a successful verify: the B1 checkpoint is
		// taken (a recorded status.checkpoints entry + a VolumeSnapshot named
		// checkpoint-<source>-iter-0).
		b1DriveToSucceeded(ctx, r, ns, srcName)
		srcPVC, err := b2GetPVC(ctx, ns, srcName)
		Expect(err).NotTo(HaveOccurred(), "the source's workspace PVC must exist after the bootstrap")
		srcSnapName := checkpointName(&coxv1alpha1.Loop{ObjectMeta: metav1.ObjectMeta{Name: srcName, Namespace: ns}}, 0)
		b2MakeSnapshotReady(ctx, ns, srcSnapName)

		// The FORK Loop (spec.forkOf {loop: source, iteration: 0} — the B1
		// checkpoint's iter).
		forkName := "fork-target"
		Expect(k8sClient.Create(ctx, &coxv1alpha1.Loop{
			ObjectMeta: metav1.ObjectMeta{Name: forkName, Namespace: ns},
			Spec: coxv1alpha1.LoopSpec{
				Goal:      loopGoal,
				Workspace: testWorkspace(),
				Verify:    coxv1alpha1.VerifyConfig{AcceptanceChecks: []string{loopCheckCmd}},
				ForkOf:    &coxv1alpha1.ForkOf{Loop: srcName, Iteration: 0},
			},
		})).To(Succeed())

		// Reconcile the FORK: the operator detects the fork target + the
		// ready source checkpoint and restores the FORK's OWN workspace PVC
		// from the source's snapshot (a fresh Loop — the PVC is absent and
		// the sandbox does not exist, so the level-based sequence goes
		// straight to the create). The decision is persisted at the point of
		// decision (I90).
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
		Expect(forkPVC.UID).NotTo(Equal(srcPVC.UID), "the fork's PVC is a SEPARATE object (a different UID — not the source's PVC)")

		By("setting the Restore condition True reason Restored on the FORK (persisted at the point of decision — I90)")
		forkRestored := &coxv1alpha1.Loop{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: forkName, Namespace: ns}, forkRestored)).To(Succeed())
		cond := b2RestoreCondition(forkRestored)
		Expect(cond).NotTo(BeNil())
		Expect(cond.Status).To(Equal(metav1.ConditionTrue))
		Expect(cond.Reason).To(Equal(coxv1alpha1.RestoreReasonRestored))

		By("claiming the fork-retention label on the source's snapshot (the source's GC skips a labelled snapshot — the safety net against a source delete racing the fork's restore)")
		srcSnap, err := b2GetSnapshot(ctx, ns, srcSnapName)
		Expect(err).NotTo(HaveOccurred(), "the source's snapshot is still present (the fork's PVC is materialising from it)")
		Expect(srcSnap.GetLabels()[forkSnapshotLabelKey]).To(Equal(forkName), "the fork-retention label is set to the fork's name (the source's GC skips it while present)")

		By("emitting a WorkspaceRestored Event on the FORK")
		b2ExpectEvent(rec, coxv1alpha1.RestoreReasonRestored)
	})

	It("FORK (one-shot + label release): the fork's restore is one-shot (a later reconcile does NOT re-restore), and the fork-retention label is REMOVED once the fork's PVC is present (the source's GC is free to delete the snapshot — the fork's PVC is self-sufficient)", func() {
		ns := "b2-fork-once-" + nowSuffix()
		Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
		defer func() {
			_ = k8sClient.Delete(context.Background(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})
		}()

		srcName := "fork-once-source"
		Expect(k8sClient.Create(ctx, &coxv1alpha1.Loop{
			ObjectMeta: metav1.ObjectMeta{Name: srcName, Namespace: ns},
			Spec:       b2LoopSpec(),
		})).To(Succeed())

		r, _ := b2NewReconcilerWithRecorder()
		b2EnsureSandbox(ns, srcName)
		b1DriveToSucceeded(ctx, r, ns, srcName)
		srcSnapName := checkpointName(&coxv1alpha1.Loop{ObjectMeta: metav1.ObjectMeta{Name: srcName, Namespace: ns}}, 0)
		b2MakeSnapshotReady(ctx, ns, srcSnapName)

		forkName := "fork-once-target"
		Expect(k8sClient.Create(ctx, &coxv1alpha1.Loop{
			ObjectMeta: metav1.ObjectMeta{Name: forkName, Namespace: ns},
			Spec: coxv1alpha1.LoopSpec{
				Goal:      loopGoal,
				Workspace: testWorkspace(),
				Verify:    coxv1alpha1.VerifyConfig{AcceptanceChecks: []string{loopCheckCmd}},
				ForkOf:    &coxv1alpha1.ForkOf{Loop: srcName, Iteration: 0},
			},
		})).To(Succeed())
		b2EnsureSandbox(ns, forkName)
		b2Reconcile(r, ns, forkName) // the fork's restore (one-shot)

		// A LATER reconcile: the fork's PVC is present and the Restore marker
		// is True/Restored — the restore is one-shot (no re-restore) and the
		// fork-retention label is removed (the fork's PVC is self-sufficient;
		// the source's GC is free to delete the snapshot).
		forkPVC, err := b2GetPVC(ctx, ns, forkName)
		Expect(err).NotTo(HaveOccurred(), "the fork's PVC must be present after its restore")
		restoredUID := forkPVC.UID
		b2Reconcile(r, ns, forkName)

		By("keeping the fork's PVC the SAME object (the one-shot marker — no re-restore)")
		forkAfter, err := b2GetPVC(ctx, ns, forkName)
		Expect(err).NotTo(HaveOccurred())
		Expect(forkAfter.UID).To(Equal(restoredUID), "the fork's PVC must not be recreated (the restore is one-shot)")

		By("removing the fork-retention label from the source's snapshot (the source's GC is free to delete it)")
		srcSnap, err := b2GetSnapshot(ctx, ns, srcSnapName)
		Expect(err).NotTo(HaveOccurred(), "the source's snapshot is still present (the source is not being deleted — the label is simply released)")
		Expect(srcSnap.GetLabels()[forkSnapshotLabelKey]).To(BeEmpty(), "the fork-retention label must be removed once the fork's PVC is present (the fork's PVC is self-sufficient)")
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
		r, _ := b2NewReconcilerWithRecorder()
		b2EnsureSandbox(ns, srcName)
		b1DriveToSucceeded(ctx, r, ns, srcName)
		srcSnapName := checkpointName(&coxv1alpha1.Loop{ObjectMeta: metav1.ObjectMeta{Name: srcName, Namespace: ns}}, 0)
		b2MakeSnapshotReady(ctx, ns, srcSnapName)

		// The FORK Loop + its restore (one-shot).
		forkName := "fork-del-target"
		Expect(k8sClient.Create(ctx, &coxv1alpha1.Loop{
			ObjectMeta: metav1.ObjectMeta{Name: forkName, Namespace: ns},
			Spec: coxv1alpha1.LoopSpec{
				Goal:      loopGoal,
				Workspace: testWorkspace(),
				Verify:    coxv1alpha1.VerifyConfig{AcceptanceChecks: []string{loopCheckCmd}},
				ForkOf:    &coxv1alpha1.ForkOf{Loop: srcName, Iteration: 0},
			},
		})).To(Succeed())
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
		// Reconcile the source (the finalizer's work runs; the B2 restore step
		// is a no-op for the source — its PVC is present and it has no
		// restoreFrom/forkOf — and the CleaningUp work GCs its snapshots).
		b2ReconcileToleratingGone(r, ns, srcName)

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
	})

	It("FORK (absent source): a fork with an ABSENT source checkpoint (the source's VolumeSnapshot is missing) is rejected VISIBLY (the Restore condition is False reason RestoreSourceMissing; the fork's PVC is NOT created from a snapshot — the target is gone, terminal)", func() {
		ns := "b2-fork-absent-" + nowSuffix()
		Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
		defer func() {
			_ = k8sClient.Delete(context.Background(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})
		}()

		// The SOURCE Loop (no checkpoint — the source is fresh; the B1
		// checkpoint was never taken, so there is NO VolumeSnapshot for any
		// iter).
		srcName := "fork-absent-source"
		Expect(k8sClient.Create(ctx, &coxv1alpha1.Loop{
			ObjectMeta: metav1.ObjectMeta{Name: srcName, Namespace: ns},
			Spec:       b2LoopSpec(),
		})).To(Succeed())
		r, _ := b2NewReconcilerWithRecorder()
		b2EnsureSandbox(ns, srcName)
		b2Reconcile(r, ns, srcName)

		// The FORK Loop (spec.forkOf {loop: source, iteration: 0} — the
		// iter the source's B1 checkpoint would be at; the snapshot is
		// ABSENT because the source never reached a successful verify).
		forkName := "fork-absent-target"
		Expect(k8sClient.Create(ctx, &coxv1alpha1.Loop{
			ObjectMeta: metav1.ObjectMeta{Name: forkName, Namespace: ns},
			Spec: coxv1alpha1.LoopSpec{
				Goal:      loopGoal,
				Workspace: testWorkspace(),
				Verify:    coxv1alpha1.VerifyConfig{AcceptanceChecks: []string{loopCheckCmd}},
				ForkOf:    &coxv1alpha1.ForkOf{Loop: srcName, Iteration: 0},
			},
		})).To(Succeed())

		// Reconcile the FORK: the operator detects the fork target + the
		// ABSENT source checkpoint and REJECTS (the condition is False
		// reason RestoreSourceMissing; the fork's PVC is left as-is — the
		// target is gone). The decision is persisted at the point of decision
		// (I90).
		b2EnsureSandbox(ns, forkName)
		b2Reconcile(r, ns, forkName)

		By("NOT creating the fork's PVC (the source's checkpoint is gone — the reject is VISIBLY held; the hold keeps the fork's bootstrap out of the same pass AND every later pass (the target applies until the marker says True/Restored), so no empty PVC is seeded and the user's choice is the durable record — the Restore condition + the Event; the fork's Loop can be seeded by delete + recreate, or by the source's snapshot reappearing)")
		_, err := b2GetPVC(ctx, ns, forkName)
		Expect(apierrors.IsNotFound(err)).To(BeTrue(), "the reject path does NOT create the fork's PVC (the target is gone; the hold keeps the bootstrap out so no empty PVC masks the reject)")

		By("keeping the reject HOLD on a LATER reconcile (the hold is permanent for the marker: b2RestoreTarget is nil only when the marker says True/Restored; the reject says False/RestoreSourceMissing, so the bootstrap stays skipped and no empty PVC is seeded on the next pass either)")
		b2Reconcile(r, ns, forkName)
		_, err = b2GetPVC(ctx, ns, forkName)
		Expect(apierrors.IsNotFound(err)).To(BeTrue(), "a later reconcile does NOT seed an empty PVC (the hold is permanent for the reject; the bootstrap is still skipped)")

		By("setting the Restore condition False reason RestoreSourceMissing on the FORK (persisted at the point of decision — I90)")
		forkRejected := &coxv1alpha1.Loop{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: forkName, Namespace: ns}, forkRejected)).To(Succeed())
		cond := b2RestoreCondition(forkRejected)
		Expect(cond).NotTo(BeNil(), "the operator must set the Restore condition on the FORK (the reject is VISIBLE)")
		Expect(cond.Status).To(Equal(metav1.ConditionFalse))
		Expect(cond.Reason).To(Equal(coxv1alpha1.RestoreReasonSourceMissing))
	})

	// B2 kind acceptance bug: the restore logic only waits for the SANDBOX pod
	// (not the verify/deliver Job pods). The verify pods mount the PVC and keep
	// it protected (kubernetes.io/pvc-protection). The restore must also delete
	// or await every Loop-owned pod mounting the PVC (verify and deliver Job
	// pods), as a level-based step with a visible reason.
	//
	// This spec tests the CURRENT behavior (the restore doesn't wait for the
	// verify pod). The PVC is stuck in Terminating state because the verify
	// pod is still present. The restore does not complete (the Restore
	// condition is "DeletingPVC" not "Restored").
	It("RESTORE: the workspace PVC is LOST and a VERIFY pod is still present (the restore COMPLETES — the operator deletes the verify Job and the PVC is recreated)", func() {
		ctx := context.Background()
		const ns = "b2-verify-pod"
		name := "verify-pod-completes"

		By("creating the namespace")
		Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())

		By("creating the Loop")
		r := b2NewReconciler()
		Expect(k8sClient.Create(ctx, &coxv1alpha1.Loop{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec:       b2LoopSpec(),
		})).To(Succeed())

		By("creating the sandbox pod + the workspace PVC")
		b2EnsureSandbox(ns, name)
		b2EnsureSandboxPod(ns, name)
		b2Reconcile(r, ns, name)
		_, err := b2GetPVC(ctx, ns, name)
		Expect(err).NotTo(HaveOccurred())

		By("driving the Loop to a successful verify (the B1 idiom — creates the verify Job + the verify Pod)")
		b1DriveToSucceeded(ctx, r, ns, name)
		snapName := checkpointName(&coxv1alpha1.Loop{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}}, 0)
		b2MakeSnapshotReady(ctx, ns, snapName)

		By("creating a SEPARATE verify pod that mounts the workspace PVC (the bug scenario — the verify pod keeps the PVC protected)")
		// The verify pod created by b1DriveToSucceeded does NOT mount the
		// workspace PVC (the envtest's verify pod is a simple pod). The bug
		// scenario: the verify pod MOUNTS the PVC and keeps it protected
		// (kubernetes.io/pvc-protection). The spec creates a SEPARATE verify
		// pod that mounts the PVC (the bug scenario). The pod is Loop-owned
		// (it carries the verifyLoopLabel: loop.Name label), so the operator's
		// stopWorkloadsForRestore detects it and waits.
		//
		// The pod is NOT owned by the verify Job (it's a separate pod). The
		// operator's stopWorkloadsForRestore deletes the verify Job (but NOT
		// the separate verify pod). The separate verify pod is still present
		// (it mounts the PVC), so the restore is in progress (StoppingWorkloads).
		// The spec must manually delete the separate verify pod (the Job
		// controller's behavior — the envtest has no Job controller).
		separateVerifyPodName := name + "-separate-verify"
		Expect(k8sClient.Create(ctx, &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: separateVerifyPodName, Namespace: ns, Labels: map[string]string{verifyLoopLabel: name}},
			Spec: corev1.PodSpec{
				Containers: []corev1.Container{
					{Name: "verify", Image: s3StandinImage},
				},
				Volumes: []corev1.Volume{
					{
						Name: "workspace",
						VolumeSource: corev1.VolumeSource{
							PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
								ClaimName: workspacePVCName(name),
							},
						},
					},
				},
			},
		})).To(Succeed())

		By("setting spec.restoreFrom to trigger the rollback restore")
		loop := &coxv1alpha1.Loop{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, loop)).To(Succeed())
		loop.Spec.RestoreFrom = "iter-0"
		Expect(k8sClient.Update(ctx, loop)).To(Succeed())

		By("reconciling (the restore's StoppingWorkloads step — the operator deletes the verify Job)")
		b2Reconcile(r, ns, name)

		By("the verify Job is deleted (the operator's stopWorkloadsForRestore deletes it)")
		// b1DriveToSucceeded creates the verify Job with the name
		// "<loop>-verify-1" (the B1 naming). The operator's
		// stopWorkloadsForRestore deletes it.
		verifyJobName := name + "-verify-1"
		verifyJob := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: verifyJobName, Namespace: ns}}
		err = k8sClient.Get(ctx, types.NamespacedName{Name: verifyJobName, Namespace: ns}, verifyJob)
		Expect(apierrors.IsNotFound(err)).To(BeTrue(), "the verify Job is deleted (the operator's stopWorkloadsForRestore deletes it)")

		By("the Restore condition is StoppingWorkloads (the in-progress state — the separate verify pod is still present, and it mounts the PVC)")
		loop2 := &coxv1alpha1.Loop{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, loop2)).To(Succeed())
		cond := b2RestoreCondition(loop2)
		Expect(cond).NotTo(BeNil(), "the operator must set the Restore condition (the restore is in progress)")
		Expect(cond.Reason).To(Equal("StoppingWorkloads"), "the restore is in the StoppingWorkloads step (the verify Job is deleted, but the separate verify pod is still present, and it mounts the PVC)")

		By("deleting the separate verify pod (the spec's workaround — the operator's stopWorkloadsForRestore does not delete the separate verify pod)")
		separateVerifyPod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: separateVerifyPodName, Namespace: ns}}
		Expect(k8sClient.Delete(ctx, separateVerifyPod)).To(Succeed())

		By("reconciling (the restore's StoppingSandbox step — the operator deletes the sandbox pod)")
		b2Reconcile(r, ns, name)

		By("deleting the sandbox pod (the A2 kill path's stop — the envtest has no agent-sandbox controller)")
		b2DeleteSandboxPod(ctx, ns, name)

		By("reconciling (the restore's DeletingPVC step — the operator deletes the PVC)")
		b2Reconcile(r, ns, name)

		By("removing the pvc-protection finalizer (the envtest workaround — the envtest has no pod controller to remove it)")
		pvc := &corev1.PersistentVolumeClaim{}
		if err = k8sClient.Get(ctx, types.NamespacedName{Name: workspacePVCName(name), Namespace: ns}, pvc); err == nil {
			fins := []string{}
			for _, f := range pvc.Finalizers {
				if f != "kubernetes.io/pvc-protection" {
					fins = append(fins, f)
				}
			}
			pvc.Finalizers = fins
			Expect(k8sClient.Update(ctx, pvc)).To(Succeed())
		}

		By("reconciling (the restore's create step — the PVC is gone, the create step runs)")
		b2Reconcile(r, ns, name)

		By("the PVC is restored (the separate verify pod is gone, the restore completes)")
		restoredPVC, err := b2GetPVC(ctx, ns, name)
		Expect(err).NotTo(HaveOccurred(), "the PVC is restored after the separate verify pod is gone (the operator's fix deletes the verify Job and waits until no Loop-owned pod mounts the PVC)")
		Expect(restoredPVC.Spec.DataSource).NotTo(BeNil(), "the restored PVC has a dataSource (the checkpoint's VolumeSnapshot)")

		By("the Restore condition is Restored (the restore completed)")
		loop3 := &coxv1alpha1.Loop{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, loop3)).To(Succeed())
		cond2 := b2RestoreCondition(loop3)
		Expect(cond2).NotTo(BeNil(), "the operator must set the Restore condition (the restore completed)")
		Expect(cond2.Status).To(Equal(metav1.ConditionTrue), "the restore completed (the separate verify pod is gone, the PVC is recreated from the snapshot)")
		Expect(cond2.Reason).To(Equal(coxv1alpha1.RestoreReasonRestored), "the restore is 'Restored' (the PVC is recreated from the snapshot after the separate verify pod is gone)")
	})

	It("RESTORE (reactive, Terminating PVC): a workspace PVC with a non-nil DeletionTimestamp (being deleted) is treated as LOST (the reactive restore starts; the Restore condition is StoppingWorkloads or StoppingSandbox; no new verify Job is created while the restore hold is active)", func() {
		ns := "b2-restore-terminating-" + nowSuffix()
		Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
		defer func() {
			_ = k8sClient.Delete(context.Background(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})
		}()

		name := "restore-terminating-loop"
		Expect(k8sClient.Create(ctx, &coxv1alpha1.Loop{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec:       b2LoopSpec(),
		})).To(Succeed())

		r, rec := b2NewReconcilerWithRecorder()
		b2EnsureSandbox(ns, name)
		b1DriveToSucceeded(ctx, r, ns, name)
		snapName := checkpointName(&coxv1alpha1.Loop{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}}, 0)
		b2MakeSnapshotReady(ctx, ns, snapName)

		// Put the PVC in Terminating state: add a finalizer, then delete it.
		// The finalizer keeps the PVC in Terminating (the apiserver sets the
		// DeletionTimestamp, but the PVC is not fully deleted because the
		// finalizer work is not done — in envtest there is no controller to
		// remove the finalizer).
		pvc, err := b2GetPVC(ctx, ns, name)
		Expect(err).NotTo(HaveOccurred(), "the workspace PVC must exist after the bootstrap")
		pvc.Finalizers = append(pvc.Finalizers, "b2-test/fake-finalizer")
		Expect(k8sClient.Update(ctx, pvc)).To(Succeed())
		Expect(k8sClient.Delete(ctx, pvc)).To(Succeed())

		// Verify the PVC is in Terminating state (the DeletionTimestamp is
		// set, the PVC is still present).
		terminatingPVC, err := b2GetPVC(ctx, ns, name)
		Expect(err).NotTo(HaveOccurred(), "the PVC must still be present (it is Terminating, not gone)")
		Expect(terminatingPVC.DeletionTimestamp).NotTo(BeZero(), "the PVC must have a non-nil DeletionTimestamp (it is being deleted)")

		// Reconcile: the operator detects the Terminating PVC (treated as LOST
		// by workspacePVCPresent) + the ready checkpoint and starts the
		// reactive restore. The restore hold is active (the Restore condition
		// is set to StoppingWorkloads or StoppingSandbox), so the verify Job is
		// stopped (deleted) by the restore logic (it would mount the PVC and
		// hold it in Terminating via pvc-protection, stalling the restore).
		b2Reconcile(r, ns, name)

		By("the Restore condition is set (StoppingWorkloads or StoppingSandbox — the restore is in progress)")
		restoredLoop := &coxv1alpha1.Loop{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, restoredLoop)).To(Succeed())
		cond := b2RestoreCondition(restoredLoop)
		Expect(cond).NotTo(BeNil(), "the operator must set the Restore condition (the restore is in progress)")
		Expect(cond.Status).To(Equal(metav1.ConditionFalse), "the restore is in progress (not complete yet)")
		expectedReasons := []string{coxv1alpha1.RestoreReasonStoppingWorkloads, coxv1alpha1.RestoreReasonStoppingSandbox, coxv1alpha1.RestoreReasonDeletingPVC}
		Expect(slices.Contains(expectedReasons, cond.Reason)).To(BeTrue(),
			"the Restore condition's reason must be one of the in-progress reasons (StoppingWorkloads, StoppingSandbox, DeletingPVC), got: "+cond.Reason)

		By("the verify Job is stopped (deleted) by the restore logic (the restore hold is active, and the verify Job would mount the PVC and hold it in Terminating via pvc-protection, stalling the restore)")
		verifyJobsAfter := &batchv1.JobList{}
		Expect(k8sClient.List(ctx, verifyJobsAfter,
			client.InNamespace(ns),
			client.MatchingLabels{verifyLoopLabel: name})).To(Succeed())
		Expect(verifyJobsAfter.Items).To(BeEmpty(),
			"the verify Job must be stopped (deleted) by the restore logic (the restore hold is active, and the verify Job would mount the PVC and hold it in Terminating via pvc-protection, stalling the restore)")

		_ = rec // the Event is emitted by the operator; the condition is the durable record
	})

	It("RESTORE (reactive, Terminating PVC + sandbox pod): the sandbox is suspended while Restore is StoppingWorkloads (the sandbox pod carries the loop label and mounts the workspace PVC; the restore hold suspends the sandbox so the pod is deleted and the PVC is released)", func() {
		ns := "b2-restore-term-sandbox-" + nowSuffix()
		Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
		defer func() {
			_ = k8sClient.Delete(context.Background(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})
		}()

		name := "restore-term-sandbox-loop"
		Expect(k8sClient.Create(ctx, &coxv1alpha1.Loop{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec:       b2LoopSpec(),
		})).To(Succeed())

		r, rec := b2NewReconcilerWithRecorder()
		b2EnsureSandbox(ns, name)
		b1DriveToSucceeded(ctx, r, ns, name)
		snapName := checkpointName(&coxv1alpha1.Loop{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}}, 0)
		b2MakeSnapshotReady(ctx, ns, snapName)

		// Create the sandbox pod with the loop label and the workspace PVC
		// volume (the sandbox pod's shape on a real cluster: it carries
		// coxswain.wattu.com/loop=<loop> and mounts the workspace PVC).
		// Delete any existing pod first (the operator's ensureSandbox may have
		// created one during b1DriveToSucceeded).
		existingPod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: b2SandboxName(name), Namespace: ns}}
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: b2SandboxName(name), Namespace: ns}, existingPod); err == nil {
			_ = k8sClient.Delete(ctx, existingPod)
		}
		Expect(k8sClient.Create(ctx, &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      b2SandboxName(name),
				Namespace: ns,
				Labels:    map[string]string{loopLabelKey: name},
			},
			Spec: corev1.PodSpec{
				Containers: []corev1.Container{
					{Name: agentContainerName, Image: s3StandinImage},
				},
				Volumes: []corev1.Volume{
					{
						Name: "workspace",
						VolumeSource: corev1.VolumeSource{
							PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
								ClaimName: workspacePVCName(name),
							},
						},
					},
				},
			},
		})).To(Succeed())

		// Ensure the sandbox is Running (the restore hold must suspend it;
		// if it's already Suspended, the spec can't prove the hold works).
		sb := &sandboxv1beta1.Sandbox{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: b2SandboxName(name), Namespace: ns}, sb)).To(Succeed())
		if sb.Spec.OperatingMode != sandboxv1beta1.SandboxOperatingModeRunning {
			sb.Spec.OperatingMode = sandboxv1beta1.SandboxOperatingModeRunning
			Expect(k8sClient.Update(ctx, sb)).To(Succeed())
		}

		// Put the PVC in Terminating state: add a finalizer, then delete it.
		pvc, err := b2GetPVC(ctx, ns, name)
		Expect(err).NotTo(HaveOccurred(), "the workspace PVC must exist after the bootstrap")
		pvc.Finalizers = append(pvc.Finalizers, "b2-test/fake-finalizer")
		Expect(k8sClient.Update(ctx, pvc)).To(Succeed())
		Expect(k8sClient.Delete(ctx, pvc)).To(Succeed())

		// Reconcile: the operator detects the Terminating PVC (treated as LOST
		// by workspacePVCPresent) + the ready checkpoint and starts the
		// reactive restore. The restore hold is active (StoppingWorkloads), so
		// the sandbox is suspended (the sandbox pod is deleted, releasing the
		// PVC). Two reconciles: the first sets the Restore condition
		// (StoppingWorkloads), the second applies the hold (suspends the
		// sandbox via sandboxSuspendedByGate).
		b2Reconcile(r, ns, name)
		b2Reconcile(r, ns, name)

		By("the Restore condition is StoppingWorkloads (the restore is in progress)")
		restoredLoop := &coxv1alpha1.Loop{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, restoredLoop)).To(Succeed())
		cond := b2RestoreCondition(restoredLoop)
		Expect(cond).NotTo(BeNil(), "the operator must set the Restore condition (the restore is in progress)")
		Expect(cond.Reason).To(Equal(coxv1alpha1.RestoreReasonStoppingWorkloads),
			"the Restore condition's reason must be StoppingWorkloads (the workloads are being stopped), got: "+cond.Reason)

		By("the Sandbox is suspended (the restore hold covers StoppingWorkloads, so the sandbox is Suspended and its pod is deleted, releasing the PVC)")
		sb2 := &sandboxv1beta1.Sandbox{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: b2SandboxName(name), Namespace: ns}, sb2)).To(Succeed())
		Expect(sb2.Spec.OperatingMode).To(Equal(sandboxv1beta1.SandboxOperatingModeSuspended),
			"the Sandbox must be Suspended while Restore is StoppingWorkloads (the restore hold covers StoppingWorkloads; the sandbox pod is deleted and the PVC is released)")

		_ = rec
	})

	It("RESTORE (deleted Loop): a deleting Loop with a lost PVC skips the B2 restore decision (the restore step is skipped so the checkpoint finalizer is cleared and the Loop is deleted, rather than being held open by a lost-PVC restore that requeues on the still-mounted workspace PVC)", func() {
		ns := "b2-restore-deleted-" + nowSuffix()
		Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
		defer func() {
			_ = k8sClient.Delete(context.Background(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})
		}()

		name := "restore-deleted-loop"
		Expect(k8sClient.Create(ctx, &coxv1alpha1.Loop{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec:       b2LoopSpec(),
		})).To(Succeed())

		r, _ := b2NewReconcilerWithRecorder()
		b2EnsureSandbox(ns, name)
		b1DriveToSucceeded(ctx, r, ns, name)
		snapName := checkpointName(&coxv1alpha1.Loop{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}}, 0)
		b2MakeSnapshotReady(ctx, ns, snapName)

		// The sandbox pod carries the loop label and mounts the workspace PVC
		// (the shape on a real cluster): stopWorkloadsForRestore returns false
		// while it is present, so the lost-PVC restore would stall at
		// StoppingWorkloads and requeue. Delete any existing pod first (the
		// operator may have created one during b1DriveToSucceeded).
		existingPod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: b2SandboxName(name), Namespace: ns}}
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: b2SandboxName(name), Namespace: ns}, existingPod); err == nil {
			_ = k8sClient.Delete(ctx, existingPod)
		}
		Expect(k8sClient.Create(ctx, &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      b2SandboxName(name),
				Namespace: ns,
				Labels:    map[string]string{loopLabelKey: name},
			},
			Spec: corev1.PodSpec{
				Containers: []corev1.Container{
					{Name: agentContainerName, Image: s3StandinImage},
				},
				Volumes: []corev1.Volume{
					{
						Name: "workspace",
						VolumeSource: corev1.VolumeSource{
							PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
								ClaimName: workspacePVCName(name),
							},
						},
					},
				},
			},
		})).To(Succeed())

		// Put the PVC in Terminating state (the reactive lost-PVC restore would
		// trigger for a LIVE Loop: a Terminating PVC is treated as LOST by
		// workspacePVCPresent, and the ready checkpoint is the restore source).
		pvc, err := b2GetPVC(ctx, ns, name)
		Expect(err).NotTo(HaveOccurred(), "the workspace PVC must exist after the bootstrap")
		pvc.Finalizers = append(pvc.Finalizers, "b2-test/fake-finalizer")
		Expect(k8sClient.Update(ctx, pvc)).To(Succeed())
		Expect(k8sClient.Delete(ctx, pvc)).To(Succeed())

		// Delete the Loop (the checkpoint finalizer holds it; the apiserver
		// sets the deletionTimestamp).
		Expect(k8sClient.Delete(ctx, &coxv1alpha1.Loop{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}})).To(Succeed())

		// Reconcile: the B2 restore step is SKIPPED (the deletion guard). The
		// CleaningUp work runs (the checkpoint GC + the finalizer removal), so
		// the Loop is fully deleted. Without the guard, the lost-PVC restore
		// would run for the deleting Loop: the PVC is still present (the
		// sandbox pod mounts it), stopWorkloadsForRestore returns false, the
		// restore requeues at StoppingWorkloads, and the checkpoint finalizer
		// is never cleared (the Loop is held open).
		b2ReconcileToleratingGone(r, ns, name)

		By("the Loop is fully deleted (the checkpoint finalizer was cleared; the lost-PVC restore did not hold the delete open)")
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, &coxv1alpha1.Loop{})).To(
			MatchError(apierrors.IsNotFound, "apiserver returns NotFound for a fully-deleted object"),
			"the Loop must be fully deleted (the checkpoint finalizer was cleared; the B2 restore step is skipped for a deleting Loop)")
	})

})
