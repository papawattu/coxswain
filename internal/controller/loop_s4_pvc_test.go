/*
Copyright 2026 papawattu.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on the "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the distributed under License is distributed on an
"AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either
express or implied. See the code for the distributed under the License
for the specific language governing permissions and limitations under
the License.
*/

package controller

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	sandboxv1beta1 "sigs.k8s.io/agent-sandbox/api/v1beta1"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	coxv1alpha1 "github.com/papawattu/coxswain/api/v1alpha1"
)

// S4 review P1 (R17): the workspace must SURVIVE the per-phase pod recycle.
// The per-Loop workspace PVC (<loop>-workspace, RWO, 1Gi, default
// StorageClass, controller-owned by the Loop) backs the sandbox's
// 'workspace' volume; init-workspace is idempotent on it (clone only when
// /workspace/.git is absent). Each It is self-contained with its own
// namespace (the established envtest idiom).
var _ = Describe("S4 review: per-Loop workspace PVC (workspace survives the phase recycle)", func() {
	ctx := context.Background()

	s4LoopSpec := coxv1alpha1.LoopSpec{Goal: loopGoal, Workspace: testWorkspace()}

	const (
		pvcLoopName    = "pvclp"
		pvcSandboxName = "pvclp-sandbox"
	)

	// pvcFor returns the Loop's workspace PVC (nil when absent).
	pvcFor := func(ns, name string) *corev1.PersistentVolumeClaim {
		pvc := &corev1.PersistentVolumeClaim{}
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: name + "-workspace", Namespace: ns}, pvc); err != nil {
			if apierrors.IsNotFound(err) {
				return nil
			}
			Expect(err).NotTo(HaveOccurred())
			return nil
		}
		return pvc
	}

	It("backs the sandbox's workspace volume with the Loop's PVC (scratch/tmp stay emptyDir)", func() {
		ns := "s4-pvcvol-" + nowSuffix()
		Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
		defer func() {
			_ = k8sClient.Delete(context.Background(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})
		}()

		r := &LoopReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
		nn := types.NamespacedName{Name: pvcLoopName, Namespace: ns}
		Expect(k8sClient.Create(ctx, &coxv1alpha1.Loop{
			ObjectMeta: metav1.ObjectMeta{Name: pvcLoopName, Namespace: ns},
			Spec:       s4LoopSpec,
		})).To(Succeed())
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
		Expect(err).NotTo(HaveOccurred())

		sb := &sandboxv1beta1.Sandbox{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: pvcSandboxName, Namespace: ns}, sb)).To(Succeed())
		var wsVol *corev1.Volume
		for i := range sb.Spec.PodTemplate.Spec.Volumes {
			v := &sb.Spec.PodTemplate.Spec.Volumes[i]
			switch v.Name {
			case workspaceVolumeName:
				wsVol = v
			case "scratch", "tmp":
				Expect(v.EmptyDir).NotTo(BeNil(), "volume %q must stay an emptyDir (ephemeral, per-pod)", v.Name)
			}
		}
		Expect(wsVol).NotTo(BeNil(), "the sandbox pod must carry the workspace volume")
		Expect(wsVol.PersistentVolumeClaim).NotTo(BeNil(),
			"the workspace volume must reference the per-Loop PVC (NOT an emptyDir — an emptyDir is wiped on every phase recycle)")
		Expect(wsVol.PersistentVolumeClaim.ClaimName).To(Equal("pvclp-workspace"),
			"the workspace volume must reference the Loop's workspace PVC <loop>-workspace")
	})

	It("creates the PVC RWO/1Gi/default-StorageClass, controller-owned by the Loop", func() {
		ns := "s4-pvcown-" + nowSuffix()
		Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
		defer func() {
			_ = k8sClient.Delete(context.Background(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})
		}()

		r := &LoopReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
		nn := types.NamespacedName{Name: pvcLoopName, Namespace: ns}
		loop := &coxv1alpha1.Loop{
			ObjectMeta: metav1.ObjectMeta{Name: pvcLoopName, Namespace: ns},
			Spec:       s4LoopSpec,
		}
		Expect(k8sClient.Create(ctx, loop)).To(Succeed())
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
		Expect(err).NotTo(HaveOccurred())

		pvc := pvcFor(ns, pvcLoopName)
		Expect(pvc).NotTo(BeNil(), "the operator must create the Loop's workspace PVC")
		Expect(pvc.Spec.AccessModes).To(ConsistOf(corev1.ReadWriteOnce),
			"the workspace PVC must be RWO (one sandbox at a time per Loop)")
		storage, ok := pvc.Spec.Resources.Requests[corev1.ResourceStorage]
		Expect(ok).To(BeTrue(), "the workspace PVC must request storage")
		Expect(storage.String()).To(Equal("1Gi"),
			"the workspace PVC must request 1Gi (bounded)")
		Expect(pvc.Spec.StorageClassName).To(BeNil(),
			"the workspace PVC must use the default StorageClass (nil = cluster default)")
		owned := false
		for _, ref := range pvc.OwnerReferences {
			if ref.Kind == "Loop" && ref.Name == pvcLoopName && ref.Controller != nil && *ref.Controller {
				owned = true
			}
		}
		Expect(owned).To(BeTrue(),
			"the PVC must be controller-owned by the Loop (so it is garbage-collected with the Loop)")
	})

	It("a phase recycle does not replace the PVC (the same object, same UID, across reconciles)", func() {
		// I43: an INPUT CHANGE (the phase advance) re-reconciles, and the PVC
		// must survive — not be recreated/replaced. The spec drives a real
		// phase advance (a completed Planning claim via the readPhaseClaim
		// seam recycles the sandbox) and asserts the PVC is the SAME object
		// (same UID + resourceVersion).
		ns := "s4-pvcrecy-" + nowSuffix()
		Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
		defer func() {
			_ = k8sClient.Delete(context.Background(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})
		}()

		r := &LoopReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
		r.readPhaseClaim = func(_ context.Context, _ *coxv1alpha1.Loop) (*PhaseClaim, error) {
			return nil, nil // agent running (no claim yet)
		}
		nn := types.NamespacedName{Name: pvcLoopName, Namespace: ns}
		Expect(k8sClient.Create(ctx, &coxv1alpha1.Loop{
			ObjectMeta: metav1.ObjectMeta{Name: pvcLoopName, Namespace: ns},
			Spec:       s4LoopSpec,
		})).To(Succeed())
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
		Expect(err).NotTo(HaveOccurred())

		pvcBefore := pvcFor(ns, pvcLoopName)
		Expect(pvcBefore).NotTo(BeNil(), "the PVC must exist after the first reconcile")

		// Recreate the sandbox (the recycle stand-in; ensureSandbox will
		// recreate it fresh after the advance deletes it) and inject the
		// completed Planning claim: the advance Planning -> Implementing
		// recycles the sandbox pod (the input change the I43 norm covers).
		sb := &sandboxv1beta1.Sandbox{}
		if gerr := k8sClient.Get(ctx, types.NamespacedName{Name: pvcSandboxName, Namespace: ns}, sb); apierrors.IsNotFound(gerr) {
			_ = k8sClient.Create(ctx, &sandboxv1beta1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{Name: pvcSandboxName, Namespace: ns},
			})
		}
		r.readPhaseClaim = func(_ context.Context, _ *coxv1alpha1.Loop) (*PhaseClaim, error) {
			return &PhaseClaim{ObservedPhase: coxv1alpha1.LoopPhasePlanning, Status: claimSuccess}, nil
		}
		_, err = r.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
		Expect(err).NotTo(HaveOccurred())
		r.readPhaseClaim = nil

		loop := &coxv1alpha1.Loop{}
		Expect(k8sClient.Get(ctx, nn, loop)).To(Succeed())
		Expect(loop.Status.Phase).To(Equal(coxv1alpha1.LoopPhaseImplementing),
			"setup: the completed Planning claim advanced the Loop (the recycle happened)")

		pvcAfter := pvcFor(ns, pvcLoopName)
		Expect(pvcAfter).NotTo(BeNil(), "the PVC must still exist after the phase recycle")
		Expect(pvcAfter.UID).To(Equal(pvcBefore.UID),
			"a phase recycle must NOT replace the PVC (same object UID — the workspace persists across phases)")
		Expect(pvcAfter.ResourceVersion).To(Equal(pvcBefore.ResourceVersion),
			"a phase recycle must NOT rewrite the PVC (no status/spec write on a no-change reconcile)")
	})
})
