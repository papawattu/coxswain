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
//   - RESTORE (A2 fallback): the workspace PVC is LOST and NO usable
//     checkpoint exists (the latest recorded checkpoint's VolumeSnapshot is
//     ABSENT). The operator falls back to the A2 PVC-resume (recreate the
//     PVC EMPTY) and surfaces the fallback (the Restore condition False
//     reason NoUsableCheckpoint + a WorkspaceRestoreUnavailable Event — I90:
//     the fallback is VISIBLE).
//   - ROLLBACK: spec.restoreFrom: iter-N names the checkpoint the operator
//     restores from. The checkpoint must EXIST (a recorded status.checkpoints
//     entry whose VolumeSnapshot is present) and be ReadyToUse. A ready
//     checkpoint is restored (the PVC is recreated with
//     spec.dataSource=the checkpoint's VolumeSnapshot, the Restore condition
//     True reason Restored). An ABSENT checkpoint is rejected VISIBLY (the
//     Restore condition False reason RestoreSourceMissing + a
//     WorkspaceRestoreUnavailable Event; no PVC is created — the target is
//     gone, terminal). A NOT-READY checkpoint (status.readyToUse != true) is
//     DEFERRED (the Restore condition False reason RestoreSourceNotReady + a
//     WorkspaceRestoreUnavailable Event; no PVC is created — the in-progress
//     state, no decision, I49).
//   - FORK: spec.forkOf {loop, iteration} seeds the Loop from another Loop's
//     checkpoint. At start the operator restores this Loop's OWN workspace
//     PVC from the source snapshot (the PVC is recreated with
//     spec.dataSource=the source's checkpoint VolumeSnapshot, the Restore
//     condition True reason Restored, the fork's PVC carries the loop label
//     for the FORK). The fork then runs a normal one-shot Loop (own branch,
//     own deliver, own PR — ADR-0003: forks, not re-tasks). Deleting the
//     source Loop does NOT break a fork that has already restored: the
//     fork's PVC owns its content (the content was materialised from the
//     snapshot at restore), and the source's checkpoint GC (the source's
//     finalizer, CleaningUp) deletes the SOURCE's snapshots (the fork's PVC
//     is a separate object, controller-owned by the fork, and is NOT touched
//     by the source's GC). The fork's restore is a ONE-SHOT decision recorded
//     in status (a status.condition Restore=True/Restored is the marker):
//     once the source snapshot is gone the fork carries on (it does not
//     re-restore, does not treat the gone snapshot as a lost-PVC fallback
//     case — the PVC is present). A fork with an ABSENT source checkpoint is
//     rejected VISIBLY (the Restore condition False reason
//     RestoreSourceMissing).
//
// The named mutations (applied in a scratch worktree — see the PR for the
// commands and the FAIL they produce):
//
//   - M1 (restore ignores the checkpoint): change b2ApplyRestoreStep's
//     ready-branch to NOT recreate the PVC (skip the
//     restorePVCFromSnapshot call). The restore spec must FAIL (the PVC is
//     absent, the Restore condition is absent — the operator ignored the
//     checkpoint and did not restore).
//   - M2 (rollback accepts a non-ReadyToUse source): change
//     resolveCheckpointSource's notready-branch to return "ready" (a
//     non-ReadyToUse snapshot is treated as ready). The rollback
//     not-ReadyToUse spec must FAIL (the operator restores from a
//     not-ReadyToUse snapshot — the PVC is created with the snapshot as the
//     dataSource, and the Restore condition is True — when it must DEFER
//     (no PVC, Restore=False reason RestoreSourceNotReady)).
//   - M3 (fork reuses the source PVC): change restorePVCFromSnapshot's
//     fork-branch to NOT delete + recreate the PVC (reuse the source's
//     existing PVC). The fork spec must FAIL (the fork's PVC is the
//     SOURCE's PVC — the same name, the same UIDs — when it must be a
//     SEPARATE PVC with its own name and the fork's loop label).
//
// The checkpoint seam: the VolumeSnapshot CRD is present in envtest (the
// vendored CRD), so the operator can read the snapshot objects. The specs
// seed the snapshots (a hand-built unstructured VolumeSnapshot with
// status.readyToUse set) and drive the reconcile; the operator's
// resolveCheckpointSource reads the snapshot and classifies it (ready /
// notready / missing). The PVC's dataSource is asserted (the operator
// recreated the PVC with the snapshot as the dataSource — the content
// materialisation is the kubelet's job, not the operator's).

package controller

import (
	"context"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
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

// b2NewReconciler builds a fresh reconciler for a Loop. The snapshot CRD seam
// is the default (present — the vendored CRD is in envtest). The driver seam
// resolves b1Driver (the csi-hostpath-driver's provisioner — the B1 constant).
func b2NewReconciler() *LoopReconciler {
	r := &LoopReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), apiReader: k8sClient}
	r.workspaceDriverSeam = func(context.Context, *coxv1alpha1.Loop) (string, bool) { return b1Driver, true }
	return r
}

// b2NewReconcilerWithRecorder builds a fresh reconciler with a FakeRecorder
// (the Event assertion needs the Recorder).
func b2NewReconcilerWithRecorder() (*LoopReconciler, *record.FakeRecorder) {
	rec := record.NewFakeRecorder(64)
	r := b2NewReconciler()
	r.Recorder = rec
	return r, rec
}

// b2SeedCheckpoint seeds a recorded status.checkpoints entry + a hand-built
// VolumeSnapshot (with status.readyToUse set) for a Loop's checkpoint. The
// snapshot is named checkpoint-<loop>-iter-<iteration> (the B1 naming).
// readyToUse=true makes the operator's resolveCheckpointSource classify the
// snapshot as "ready"; readyToUse=false (or absent) makes it "notready".
func b2SeedCheckpoint(ctx context.Context, loop *coxv1alpha1.Loop, readyToUse bool) {
	iteration := 1
	name := checkpointName(loop, iteration)
	// The status entry (the operator's GC reads the status entries; the
	// restore reads the status to find the latest checkpoint).
	loop.Status.Checkpoints = append(loop.Status.Checkpoints, coxv1alpha1.CheckpointStatus{
		Iteration: iteration,
		Name:      name,
		Milestone: true,
		At:        metav1.Now(),
	})
	Expect(k8sClient.Status().Update(ctx, loop)).To(Succeed())
	// The VolumeSnapshot object (hand-built; the operator reads it).
	snap := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": snapshotGV,
		"kind":       volumeSnapshotKind,
		"metadata": map[string]any{"name": name, "namespace": loop.Namespace,
			"labels": map[string]any{"coxswain.wattu.com/loop": loop.Name}},
		"spec": map[string]any{
			"source": map[string]any{
				"persistentVolumeClaimName": workspacePVCName(loop.Name),
			},
		},
	}}
	if readyToUse {
		snap.Object["status"] = map[string]any{"readyToUse": true}
	}
	Expect(k8sClient.Create(ctx, snap)).To(Succeed())
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

var _ = Describe("B2 workspace restore, rollback and fork (envtest)", func() {
	var ctx context.Context

	BeforeEach(func() {
		ctx = context.Background()
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
		// Drive the Loop to a state where the workspace PVC exists (the
		// bootstrap creates it). A single reconcile is enough: the bootstrap
		// runs on the first reconcile (Pending -> Planning), and the PVC is
		// created by ensureWorkspacePVC (the A2 path).
		b2Reconcile(r, ns, name)
		// The PVC must be present (the bootstrap created it).
		pvc, err := b2GetPVC(ctx, ns, name)
		Expect(err).NotTo(HaveOccurred(), "the workspace PVC must exist after the bootstrap")
		Expect(pvc.Spec.DataSource).To(BeNil(), "the bootstrap-created PVC has no dataSource (it is a fresh empty PVC)")

		// Seed the checkpoint (a recorded status.checkpoints entry + a
		// ReadyToUse VolumeSnapshot).
		loop = b2Reconcile(r, ns, name)
		b2SeedCheckpoint(ctx, loop, true)

		// LOSE the PVC (delete it — the lost-PVC case).
		Expect(k8sClient.Delete(ctx, pvc)).To(Succeed())

		// Reconcile: the operator detects the LOST PVC + the ready checkpoint
		// and restores (the PVC is recreated with the checkpoint's
		// VolumeSnapshot as the dataSource).
		b2Reconcile(r, ns, name)

		By("recreating the workspace PVC with spec.dataSource=the checkpoint's VolumeSnapshot")
		restored, err := b2GetPVC(ctx, ns, name)
		Expect(err).NotTo(HaveOccurred(), "the operator must restore the LOST PVC (recreate it)")
		Expect(restored.Spec.DataSource).NotTo(BeNil(), "the restored PVC must carry a dataSource (the checkpoint's VolumeSnapshot)")
		Expect(restored.Spec.DataSource.Kind).To(Equal(volumeSnapshotKind))
		Expect(restored.Spec.DataSource.Name).To(Equal(checkpointName(loop, 1)))

		By("setting the Restore condition True reason Restored")
		restoredLoop := b2Reconcile(r, ns, name)
		cond := b2RestoreCondition(restoredLoop)
		Expect(cond).NotTo(BeNil(), "the operator must set the Restore condition (the B2 durable record)")
		Expect(cond.Status).To(Equal(metav1.ConditionTrue))
		Expect(cond.Reason).To(Equal(coxv1alpha1.RestoreReasonRestored))

		By("emitting a WorkspaceRestored Event")
		select {
		case e := <-rec.Events:
			Expect(e).To(ContainSubstring("Restore=true"))
			Expect(e).To(ContainSubstring("Restored"))
		case <-time.After(2 * time.Second):
			Fail("expected a WorkspaceRestored Event (the B2 transient signal)")
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
		b2Reconcile(r, ns, name)
		pvc, err := b2GetPVC(ctx, ns, name)
		Expect(err).NotTo(HaveOccurred(), "the workspace PVC must exist after the bootstrap")

		// Seed the checkpoint's status entry (the operator reads the status
		// to find the latest checkpoint) but DO NOT seed the VolumeSnapshot
		// object (the checkpoint is "absent" — the snapshot was GC'd or never
		// taken).
		loop = b2Reconcile(r, ns, name)
		loop.Status.Checkpoints = append(loop.Status.Checkpoints, coxv1alpha1.CheckpointStatus{
			Iteration: 1,
			Name:      checkpointName(loop, 1),
			Milestone: true,
			At:        metav1.Now(),
		})
		Expect(k8sClient.Status().Update(ctx, loop)).To(Succeed())

		// LOSE the PVC.
		Expect(k8sClient.Delete(ctx, pvc)).To(Succeed())

		// Reconcile: the operator detects the LOST PVC + the ABSENT
		// checkpoint and falls back to the A2 PVC-resume (recreate the PVC
		// EMPTY).
		b2Reconcile(r, ns, name)

		By("recreating the workspace PVC EMPTY (the A2 fallback — the PVC has NO dataSource)")
		fallback, err := b2GetPVC(ctx, ns, name)
		Expect(err).NotTo(HaveOccurred(), "the operator must recreate the LOST PVC (the A2 fallback)")
		Expect(fallback.Spec.DataSource).To(BeNil(), "the A2 fallback recreates the PVC EMPTY (no dataSource — the content is lost, the Loop continues from the untouched base)")

		By("setting the Restore condition False reason NoUsableCheckpoint")
		fallbackLoop := b2Reconcile(r, ns, name)
		cond := b2RestoreCondition(fallbackLoop)
		Expect(cond).NotTo(BeNil(), "the operator must set the Restore condition (the I90 fallback is VISIBLE)")
		Expect(cond.Status).To(Equal(metav1.ConditionFalse))
		Expect(cond.Reason).To(Equal("NoUsableCheckpoint"))

		By("emitting a WorkspaceRestoreUnavailable Event")
		select {
		case e := <-rec.Events:
			Expect(e).To(ContainSubstring("Restore=false"))
			Expect(e).To(ContainSubstring("NoUsableCheckpoint"))
		case <-time.After(2 * time.Second):
			Fail("expected a WorkspaceRestoreUnavailable Event (the I90 fallback is VISIBLE)")
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
		b2Reconcile(r, ns, name)
		_, err := b2GetPVC(ctx, ns, name)
		Expect(err).NotTo(HaveOccurred(), "the workspace PVC must exist after the bootstrap")

		// Seed the checkpoint (a recorded status.checkpoints entry + a
		// ReadyToUse VolumeSnapshot).
		loop = b2Reconcile(r, ns, name)
		b2SeedCheckpoint(ctx, loop, true)

		// Set spec.restoreFrom: iter-1 (the rollback target). The Loop is
		// updated (a patch on the main resource).
		loop.Spec.RestoreFrom = "iter-1"
		Expect(k8sClient.Update(ctx, loop)).To(Succeed())

		// Reconcile: the operator detects the rollback target + the ready
		// checkpoint and restores (the PVC is recreated with the checkpoint's
		// VolumeSnapshot as the dataSource).
		b2Reconcile(r, ns, name)

		By("recreating the workspace PVC with spec.dataSource=the checkpoint's VolumeSnapshot (the rollback target)")
		restored, err := b2GetPVC(ctx, ns, name)
		Expect(err).NotTo(HaveOccurred(), "the operator must restore the PVC from the rollback target")
		Expect(restored.Spec.DataSource).NotTo(BeNil(), "the restored PVC must carry a dataSource (the rollback target's VolumeSnapshot)")
		Expect(restored.Spec.DataSource.Kind).To(Equal(volumeSnapshotKind))
		Expect(restored.Spec.DataSource.Name).To(Equal(checkpointName(loop, 1)))

		By("setting the Restore condition True reason Restored")
		restoredLoop := b2Reconcile(r, ns, name)
		cond := b2RestoreCondition(restoredLoop)
		Expect(cond).NotTo(BeNil())
		Expect(cond.Status).To(Equal(metav1.ConditionTrue))
		Expect(cond.Reason).To(Equal(coxv1alpha1.RestoreReasonRestored))

		By("emitting a WorkspaceRestored Event")
		select {
		case e := <-rec.Events:
			Expect(e).To(ContainSubstring("Restore=true"))
		case <-time.After(2 * time.Second):
			Fail("expected a WorkspaceRestored Event")
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
		b2Reconcile(r, ns, name)
		_, err := b2GetPVC(ctx, ns, name)
		Expect(err).NotTo(HaveOccurred(), "the workspace PVC must exist after the bootstrap")

		// Seed the checkpoint's status entry but DO NOT seed the
		// VolumeSnapshot object (the checkpoint is "absent").
		loop = b2Reconcile(r, ns, name)
		loop.Status.Checkpoints = append(loop.Status.Checkpoints, coxv1alpha1.CheckpointStatus{
			Iteration: 1,
			Name:      checkpointName(loop, 1),
			Milestone: true,
			At:        metav1.Now(),
		})
		Expect(k8sClient.Status().Update(ctx, loop)).To(Succeed())

		// Set spec.restoreFrom: iter-1.
		loop.Spec.RestoreFrom = "iter-1"
		Expect(k8sClient.Update(ctx, loop)).To(Succeed())

		// Reconcile: the operator detects the rollback target + the ABSENT
		// checkpoint and REJECTS (the condition is False reason
		// RestoreSourceMissing; no PVC is created — the target is gone).
		b2Reconcile(r, ns, name)

		By("NOT recreating the PVC (the target is gone — no restore)")
		// The PVC was created by the bootstrap (it is present). The operator
		// does NOT recreate it (the reject path does not delete + recreate —
		// the PVC is left as-is; the Loop is terminal).
		_, err = b2GetPVC(ctx, ns, name)
		Expect(err).NotTo(HaveOccurred(), "the bootstrap-created PVC is present (the reject path does NOT delete it)")

		By("setting the Restore condition False reason RestoreSourceMissing")
		rejectedLoop := b2Reconcile(r, ns, name)
		cond := b2RestoreCondition(rejectedLoop)
		Expect(cond).NotTo(BeNil(), "the operator must set the Restore condition (the reject is VISIBLE)")
		Expect(cond.Status).To(Equal(metav1.ConditionFalse))
		Expect(cond.Reason).To(Equal(coxv1alpha1.RestoreReasonSourceMissing))

		By("emitting a WorkspaceRestoreUnavailable Event")
		select {
		case e := <-rec.Events:
			Expect(e).To(ContainSubstring("Restore=false"))
			Expect(e).To(ContainSubstring("RestoreSourceMissing"))
		case <-time.After(2 * time.Second):
			Fail("expected a WorkspaceRestoreUnavailable Event (the reject is VISIBLE)")
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
		b2Reconcile(r, ns, name)
		_, err := b2GetPVC(ctx, ns, name)
		Expect(err).NotTo(HaveOccurred(), "the workspace PVC must exist after the bootstrap")

		// Seed the checkpoint (a recorded status.checkpoints entry + a
		// NOT-ReadyToUse VolumeSnapshot — status.readyToUse is false /
		// absent).
		loop = b2Reconcile(r, ns, name)
		b2SeedCheckpoint(ctx, loop, false)

		// Set spec.restoreFrom: iter-1.
		loop.Spec.RestoreFrom = "iter-1"
		Expect(k8sClient.Update(ctx, loop)).To(Succeed())

		// Reconcile: the operator detects the rollback target + the
		// NOT-ReadyToUse checkpoint and DEFERS (the condition is False
		// reason RestoreSourceNotReady; no PVC is created — the in-progress
		// state, no decision).
		b2Reconcile(r, ns, name)

		By("NOT recreating the PVC (the checkpoint is not ReadyToUse yet — no decision, I49)")
		// The PVC was created by the bootstrap (it is present). The operator
		// does NOT recreate it (the defer path does not delete + recreate —
		// the PVC is left as-is; the requeue retries when the snapshot is
		// ready).
		_, err = b2GetPVC(ctx, ns, name)
		Expect(err).NotTo(HaveOccurred(), "the bootstrap-created PVC is present (the defer path does NOT delete it)")

		By("setting the Restore condition False reason RestoreSourceNotReady")
		deferredLoop := b2Reconcile(r, ns, name)
		cond := b2RestoreCondition(deferredLoop)
		Expect(cond).NotTo(BeNil(), "the operator must set the Restore condition (the defer is VISIBLE)")
		Expect(cond.Status).To(Equal(metav1.ConditionFalse))
		Expect(cond.Reason).To(Equal(coxv1alpha1.RestoreReasonSourceNotReady))

		By("emitting a WorkspaceRestoreUnavailable Event")
		select {
		case e := <-rec.Events:
			Expect(e).To(ContainSubstring("Restore=false"))
			Expect(e).To(ContainSubstring("RestoreSourceNotReady"))
		case <-time.After(2 * time.Second):
			Fail("expected a WorkspaceRestoreUnavailable Event (the defer is VISIBLE)")
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
		b2Reconcile(r, ns, srcName)
		srcPVC, err := b2GetPVC(ctx, ns, srcName)
		Expect(err).NotTo(HaveOccurred(), "the source's workspace PVC must exist after the bootstrap")

		// Seed the source's checkpoint (a recorded status.checkpoints entry +
		// a ReadyToUse VolumeSnapshot).
		srcLoop = b2Reconcile(r, ns, srcName)
		b2SeedCheckpoint(ctx, srcLoop, true)
		srcSnapName := checkpointName(srcLoop, 1)

		// The FORK Loop (spec.forkOf {loop: source, iteration: 1}).
		forkName := "fork-target"
		forkLoop := &coxv1alpha1.Loop{
			ObjectMeta: metav1.ObjectMeta{Name: forkName, Namespace: ns},
			Spec: coxv1alpha1.LoopSpec{
				Goal:      loopGoal,
				Workspace: testWorkspace(),
				Verify:    coxv1alpha1.VerifyConfig{AcceptanceChecks: []string{loopCheckCmd}},
				ForkOf: &coxv1alpha1.ForkOf{
					Loop:      srcName,
					Iteration: 1,
				},
			},
		}
		Expect(k8sClient.Create(ctx, forkLoop)).To(Succeed())

		// Reconcile the FORK: the operator detects the fork target + the
		// ready source checkpoint and restores the FORK's OWN workspace PVC
		// from the source's snapshot.
		b2Reconcile(r, ns, forkName)

		By("recreating the FORK's OWN workspace PVC with spec.dataSource=the source's checkpoint VolumeSnapshot")
		forkPVC, err := b2GetPVC(ctx, ns, forkName)
		Expect(err).NotTo(HaveOccurred(), "the operator must restore the FORK's OWN PVC (the fork's PVC is a SEPARATE object, controller-owned by the fork)")
		Expect(forkPVC.Name).To(Equal(workspacePVCName(forkName)), "the fork's PVC is named after the FORK (not the source)")
		Expect(forkPVC.Spec.DataSource).NotTo(BeNil(), "the fork's PVC must carry a dataSource (the source's checkpoint VolumeSnapshot)")
		Expect(forkPVC.Spec.DataSource.Kind).To(Equal(volumeSnapshotKind))
		Expect(forkPVC.Spec.DataSource.Name).To(Equal(srcSnapName))
		Expect(forkPVC.GetLabels()["coxswain.wattu.com/loop"]).To(Equal(forkName), "the fork's PVC carries the FORK's loop label (not the source's)")
		Expect(forkPVC.Spec.DataSource).NotTo(Equal(srcPVC.Spec.DataSource), "the fork's PVC is NOT the source's PVC (it is a separate object with its own dataSource)")

		By("setting the Restore condition True reason Restored on the FORK")
		forkRestored := b2Reconcile(r, ns, forkName)
		cond := b2RestoreCondition(forkRestored)
		Expect(cond).NotTo(BeNil())
		Expect(cond.Status).To(Equal(metav1.ConditionTrue))
		Expect(cond.Reason).To(Equal(coxv1alpha1.RestoreReasonRestored))

		By("emitting a WorkspaceRestored Event on the FORK")
		select {
		case e := <-rec.Events:
			Expect(e).To(ContainSubstring("Restore=true"))
		case <-time.After(2 * time.Second):
			Fail("expected a WorkspaceRestored Event on the FORK")
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
		b2Reconcile(r, ns, srcName)
		srcLoop = b2Reconcile(r, ns, srcName)
		b2SeedCheckpoint(ctx, srcLoop, true)
		srcSnapName := checkpointName(srcLoop, 1)

		// The FORK Loop (spec.forkOf {loop: source, iteration: 1}).
		forkName := "fork-del-target"
		forkLoop := &coxv1alpha1.Loop{
			ObjectMeta: metav1.ObjectMeta{Name: forkName, Namespace: ns},
			Spec: coxv1alpha1.LoopSpec{
				Goal:      loopGoal,
				Workspace: testWorkspace(),
				Verify:    coxv1alpha1.VerifyConfig{AcceptanceChecks: []string{loopCheckCmd}},
				ForkOf:    &coxv1alpha1.ForkOf{Loop: srcName, Iteration: 1},
			},
		}
		Expect(k8sClient.Create(ctx, forkLoop)).To(Succeed())
		b2Reconcile(r, ns, forkName)
		// The fork's restore is complete (the PVC is restored, the Restore
		// condition is True).
		forkRestored := b2Reconcile(r, ns, forkName)
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
		forkAfter := b2Reconcile(r, ns, forkName)

		By("keeping the FORK's PVC present (the fork's PVC is a separate object, controller-owned by the fork; the source's GC did NOT touch it)")
		forkPVC, err := b2GetPVC(ctx, ns, forkName)
		Expect(err).NotTo(HaveOccurred(), "the fork's PVC must be present AFTER the source's deletion (the fork's PVC is a separate object)")
		Expect(forkPVC.Spec.DataSource).NotTo(BeNil(), "the fork's PVC retains its dataSource (the source's snapshot — the content is materialised in the PVC)")

		By("keeping the FORK's Restore condition True reason Restored (the one-shot marker — the fork does NOT re-restore)")
		condAfter := b2RestoreCondition(forkAfter)
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

		// The SOURCE Loop (no checkpoint — the VolumeSnapshot is absent).
		srcName := "fork-absent-source"
		srcLoop := &coxv1alpha1.Loop{
			ObjectMeta: metav1.ObjectMeta{Name: srcName, Namespace: ns},
			Spec:       b2LoopSpec(),
		}
		Expect(k8sClient.Create(ctx, srcLoop)).To(Succeed())
		r, _ := b2NewReconcilerWithRecorder()
		b2Reconcile(r, ns, srcName)

		// The FORK Loop (spec.forkOf {loop: source, iteration: 1}).
		forkName := "fork-absent-target"
		forkLoop := &coxv1alpha1.Loop{
			ObjectMeta: metav1.ObjectMeta{Name: forkName, Namespace: ns},
			Spec: coxv1alpha1.LoopSpec{
				Goal:      loopGoal,
				Workspace: testWorkspace(),
				Verify:    coxv1alpha1.VerifyConfig{AcceptanceChecks: []string{loopCheckCmd}},
				ForkOf:    &coxv1alpha1.ForkOf{Loop: srcName, Iteration: 1},
			},
		}
		Expect(k8sClient.Create(ctx, forkLoop)).To(Succeed())

		// Reconcile the FORK: the operator detects the fork target + the
		// ABSENT source checkpoint and REJECTS (the condition is False
		// reason RestoreSourceMissing; no PVC is created — the target is
		// gone).
		b2Reconcile(r, ns, forkName)

		By("NOT recreating the fork's PVC (the source's checkpoint is gone — no restore)")
		_, err := b2GetPVC(ctx, ns, forkName)
		Expect(apierrors.IsNotFound(err)).To(BeTrue(), "the fork's PVC must be ABSENT (the reject path does NOT create it — the source's checkpoint is gone)")

		By("setting the Restore condition False reason RestoreSourceMissing on the FORK")
		forkRejected := b2Reconcile(r, ns, forkName)
		cond := b2RestoreCondition(forkRejected)
		Expect(cond).NotTo(BeNil(), "the operator must set the Restore condition on the FORK (the reject is VISIBLE)")
		Expect(cond.Status).To(Equal(metav1.ConditionFalse))
		Expect(cond.Reason).To(Equal(coxv1alpha1.RestoreReasonSourceMissing))
	})
})
