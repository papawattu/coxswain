/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"context"
	"errors"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	coxv1alpha1 "github.com/papawattu/coxswain/api/v1alpha1"
)

// f36RBACClient behaves like the operator's real role for VolumeSnapshots: it
// grants `patch` but NOT `update` (envtest runs as admin, so without this the
// Update/Patch mismatch of F36 P1-2 is invisible). Hooks let a spec observe
// the state at the moment of the PVC create or fail a verb.
type f36RBACClient struct {
	client.Client
	failPatchSnapshot bool
	failPVCCreate     bool
	labelAtPVCCreate  *bool // set to whether the source snapshot carried the label when the PVC was created
	snapNS, snapName  string
}

func isSnapshot(obj client.Object) bool {
	u, ok := obj.(*unstructured.Unstructured)
	return ok && u.GroupVersionKind().Kind == volumeSnapshotKind
}

func (c *f36RBACClient) Update(ctx context.Context, obj client.Object, opts ...client.UpdateOption) error {
	if isSnapshot(obj) {
		return apierrors.NewForbidden(schema.GroupResource{Group: snapshotGroupRef, Resource: "volumesnapshots"}, obj.GetName(),
			errors.New("the operator role grants patch, not update, on volumesnapshots"))
	}
	return c.Client.Update(ctx, obj, opts...)
}

func (c *f36RBACClient) Patch(ctx context.Context, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
	if isSnapshot(obj) && c.failPatchSnapshot {
		return apierrors.NewForbidden(schema.GroupResource{Group: snapshotGroupRef, Resource: "volumesnapshots"}, obj.GetName(), errors.New("patch denied"))
	}
	return c.Client.Patch(ctx, obj, patch, opts...)
}

func (c *f36RBACClient) Create(ctx context.Context, obj client.Object, opts ...client.CreateOption) error {
	if pvc, ok := obj.(*corev1.PersistentVolumeClaim); ok && pvc.Spec.DataSource != nil {
		if c.labelAtPVCCreate != nil {
			snap := &unstructured.Unstructured{}
			snap.SetGroupVersionKind(volumeSnapshotGVK)
			if err := c.Get(ctx, types.NamespacedName{Namespace: c.snapNS, Name: c.snapName}, snap); err == nil {
				_, has := snap.GetLabels()[forkSnapshotLabelKey]
				*c.labelAtPVCCreate = has
			}
		}
		if c.failPVCCreate {
			return errors.New("injected PVC create failure")
		}
	}
	return c.Client.Create(ctx, obj, opts...)
}

var _ = Describe("F36: fork-retention label (patch, before the create) and PVC restore progress", func() {
	ctx := context.Background()

	BeforeEach(func() {
		// The B1 checkpoint path needs a VolumeSnapshotClass for the workspace
		// driver (cluster-scoped, shared with the B2 specs).
		vsc := &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": snapshotGV, "kind": volumeSnapshotClassKind,
			"metadata": map[string]any{"name": "b1-test-vsc"}, "driver": b1Driver, "deletionPolicy": "Delete",
		}}
		if err := k8sClient.Create(ctx, vsc); err != nil && !apierrors.IsAlreadyExists(err) {
			Expect(err).ToNot(HaveOccurred())
		}
	})

	// f36Fork creates a source Loop with a ready checkpoint and a fork Loop of
	// it. It returns the namespace, fork name and the source snapshot name.
	f36Fork := func() (ns, forkName, srcSnap string) {
		ns = "f36-" + nowSuffix()
		Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
		DeferCleanup(func() {
			_ = k8sClient.Delete(context.Background(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})
		})
		srcName := "src"
		Expect(k8sClient.Create(ctx, &coxv1alpha1.Loop{
			ObjectMeta: metav1.ObjectMeta{Name: srcName, Namespace: ns}, Spec: b2LoopSpec()})).To(Succeed())
		r, _ := b2NewReconcilerWithRecorder()
		b2EnsureSandbox(ns, srcName)
		b1DriveToSucceeded(ctx, r, ns, srcName)
		srcSnap = checkpointName(&coxv1alpha1.Loop{ObjectMeta: metav1.ObjectMeta{Name: srcName, Namespace: ns}}, 0)
		b2MakeSnapshotReady(ctx, ns, srcSnap)
		forkName = "fork"
		Expect(k8sClient.Create(ctx, &coxv1alpha1.Loop{
			ObjectMeta: metav1.ObjectMeta{Name: forkName, Namespace: ns},
			Spec: coxv1alpha1.LoopSpec{
				Goal: loopGoal, Workspace: testWorkspace(),
				Verify: coxv1alpha1.VerifyConfig{AcceptanceChecks: []string{loopCheckCmd}},
				ForkOf: &coxv1alpha1.ForkOf{Loop: srcName, Iteration: 0},
			},
		})).To(Succeed())
		b2EnsureSandbox(ns, forkName)
		return ns, forkName, srcSnap
	}

	It("P1-2: the fork-retention label is written with PATCH (the role has no `update` on volumesnapshots) and is already on the snapshot when the fork PVC is created (P3-3)", func() {
		ns, forkName, srcSnap := f36Fork()
		var labelAtCreate bool
		r, rec := b2NewReconcilerWithRecorder()
		r.Client = &f36RBACClient{Client: k8sClient, labelAtPVCCreate: &labelAtCreate, snapNS: ns, snapName: srcSnap}

		b2Reconcile(r, ns, forkName)

		snap, err := b2GetSnapshot(ctx, ns, srcSnap)
		Expect(err).NotTo(HaveOccurred())
		Expect(snap.GetLabels()).To(HaveKeyWithValue(forkSnapshotLabelKey, forkName),
			"with a role that grants patch but not update, the label must still be written")
		Expect(labelAtCreate).To(BeTrue(), "P3-3: the label must be on the snapshot BEFORE the fork's PVC is created")
		l := &coxv1alpha1.Loop{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: forkName, Namespace: ns}, l)).To(Succeed())
		Expect(b2RestoreCondition(l).Reason).To(Equal(coxv1alpha1.RestoreReasonRestored))
		for len(rec.Events) > 0 {
			Expect(<-rec.Events).NotTo(ContainSubstring("ForkRetentionLabel"), "no failure event on the success path")
		}
	})

	It("P1-2: a failed label write is a Warning Event, not a V(1) log", func() {
		ns, forkName, _ := f36Fork()
		r, rec := b2NewReconcilerWithRecorder()
		r.Client = &f36RBACClient{Client: k8sClient, failPatchSnapshot: true}

		b2Reconcile(r, ns, forkName)

		var events []string
		for len(rec.Events) > 0 {
			events = append(events, <-rec.Events)
		}
		Expect(strings.Join(events, "\n")).To(ContainSubstring("Warning ForkRetentionLabelFailed"))
	})

	It("P3-3: when the fork's PVC create fails the label written before it is removed again", func() {
		ns, forkName, srcSnap := f36Fork()
		r, _ := b2NewReconcilerWithRecorder()
		r.Client = &f36RBACClient{Client: k8sClient, failPVCCreate: true}

		b2Reconcile(r, ns, forkName)

		snap, err := b2GetSnapshot(ctx, ns, srcSnap)
		Expect(err).NotTo(HaveOccurred())
		Expect(snap.GetLabels()).NotTo(HaveKey(forkSnapshotLabelKey), "a failed restore must not leave the retention label behind")
		l := &coxv1alpha1.Loop{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: forkName, Namespace: ns}, l)).To(Succeed())
		Expect(b2RestoreCondition(l).Status).To(Equal(metav1.ConditionFalse))
	})

	It("P2-2: a fork whose own PVC is already present is DELETED and recreated from the snapshot (the restore completes)", func() {
		ns, forkName, srcSnap := f36Fork()
		// A plain (non-restored) PVC under the fork's workspace name.
		fork := &coxv1alpha1.Loop{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: forkName, Namespace: ns}, fork)).To(Succeed())
		pvc := workspacePVCSpec(fork)
		pvc.Name = workspacePVCName(forkName)
		pvc.Namespace = ns
		Expect(k8sClient.Create(ctx, pvc)).To(Succeed())
		oldUID := pvc.UID

		r, _ := b2NewReconcilerWithRecorder()
		l := b2Reconcile(r, ns, forkName)
		Expect(b2RestoreCondition(l).Reason).To(Equal(coxv1alpha1.RestoreReasonDeletingPVC), "first pass deletes the old PVC")
		// envtest has no pvc-protection controller: drop the finalizer so the delete completes.
		if cur, err := b2GetPVC(ctx, ns, forkName); err == nil && cur.DeletionTimestamp != nil {
			b2RemovePVCProtectionFinalizer(ctx, ns, forkName)
		}
		l = b2Reconcile(r, ns, forkName)
		Expect(b2RestoreCondition(l).Reason).To(Equal(coxv1alpha1.RestoreReasonRestored),
			"before F36 the branch announced DeletingPVC for ever and never deleted anything")
		restored, err := b2GetPVC(ctx, ns, forkName)
		Expect(err).NotTo(HaveOccurred())
		Expect(restored.UID).NotTo(Equal(oldUID))
		Expect(restored.Spec.DataSource).NotTo(BeNil())
		Expect(restored.Spec.DataSource.Name).To(Equal(srcSnap))
	})

	It("P2-1: a rollback whose old PVC is still Terminating is NOT reported Restored; it completes once the PVC is gone", func() {
		ns := "f36-rb-" + nowSuffix()
		Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
		DeferCleanup(func() {
			_ = k8sClient.Delete(context.Background(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})
		})
		name := "rb"
		Expect(k8sClient.Create(ctx, &coxv1alpha1.Loop{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}, Spec: b2LoopSpec()})).To(Succeed())
		r, _ := b2NewReconcilerWithRecorder()
		b2EnsureSandbox(ns, name)
		b2EnsureSandboxPod(ns, name)
		b1DriveToSucceeded(ctx, r, ns, name)
		snapName := checkpointName(&coxv1alpha1.Loop{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}}, 0)
		b2MakeSnapshotReady(ctx, ns, snapName)
		l := &coxv1alpha1.Loop{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, l)).To(Succeed())
		l.Spec.RestoreFrom = "iter-0"
		Expect(k8sClient.Update(ctx, l)).To(Succeed())

		b2Reconcile(r, ns, name) // stop the sandbox
		b2DeleteSandboxPod(ctx, ns, name)
		b2Reconcile(r, ns, name) // delete the PVC (it stays Terminating: the pvc-protection finalizer is still set)
		pvc, err := b2GetPVC(ctx, ns, name)
		Expect(err).NotTo(HaveOccurred())
		Expect(pvc.DeletionTimestamp).NotTo(BeNil(), "the PVC is Terminating")
		terminatingUID := pvc.UID

		l = b2Reconcile(r, ns, name) // the Terminating PVC still owns the name
		c := b2RestoreCondition(l)
		Expect(c.Reason).NotTo(Equal(coxv1alpha1.RestoreReasonRestored),
			"AlreadyExists on a Terminating PVC was reported as a completed restore")
		Expect(c.Status).To(Equal(metav1.ConditionFalse))

		b2RemovePVCProtectionFinalizer(ctx, ns, name)
		l = b2Reconcile(r, ns, name)
		Expect(b2RestoreCondition(l).Reason).To(Equal(coxv1alpha1.RestoreReasonRestored))
		restored, err := b2GetPVC(ctx, ns, name)
		Expect(err).NotTo(HaveOccurred())
		Expect(restored.UID).NotTo(Equal(terminatingUID))
		Expect(restored.Spec.DataSource).NotTo(BeNil())
	})
})
