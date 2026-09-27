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

package controller

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	coxv1alpha1 "github.com/papawattu/coxswain/api/v1alpha1"
)

// B2 (D10) anti-gaming envtest. The operator's TamperedVerify decision at
// Verifying is driven by its OWN tamper evidence (status.verify.tamperExitCode,
// which B3 reads from the verify Job pod's initContainerStatuses), never by the
// runner's result.json claim. In envtest (no Job controller) the tests set
// status.verify.tamperExitCode directly. A non-zero tamper code ends the Loop
// Failed:TamperedVerify, terminal, before any check runs — even when the
// runner reports Verifying done (the claim) with a success-flavoured status.
var _ = Describe("B2 TamperedVerify via base-commit glob diff (D10)", func() {
	ctx := context.Background()

	// driveToVerifying drives a fresh Loop through the B1 claim path to
	// Verifying (Pending -> Planning -> Implementing -> Verifying).
	driveToVerifying := func(nn types.NamespacedName) {
		r := &LoopReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
		for _, reported := range []coxv1alpha1.LoopPhase{
			coxv1alpha1.LoopPhasePlanning,
			coxv1alpha1.LoopPhaseImplementing,
			coxv1alpha1.LoopPhaseVerifying,
		} {
			loop := &coxv1alpha1.Loop{}
			Expect(k8sClient.Get(ctx, nn, loop)).To(Succeed())
			loop.Status.ObservedPhase = reported
			Expect(k8sClient.Status().Update(ctx, loop)).To(Succeed())
			_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
			Expect(err).NotTo(HaveOccurred())
		}
	}

	It("ends Failed:TamperedVerify (terminal) when the tamper code is non-zero, even though the runner claims success (D10 anti-gaming)", func() {
		ns := "b2-tamper-" + nowSuffix()
		nsObj := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}
		Expect(k8sClient.Create(ctx, nsObj)).To(Succeed())
		defer func() { _ = k8sClient.Delete(context.Background(), nsObj) }()

		nn := types.NamespacedName{Name: "b2-tamper", Namespace: ns}
		loop := &coxv1alpha1.Loop{
			ObjectMeta: metav1.ObjectMeta{Name: "b2-tamper", Namespace: ns},
			Spec:       coxv1alpha1.LoopSpec{Goal: loopGoal, Workspace: testWorkspace()},
		}
		Expect(k8sClient.Create(ctx, loop)).To(Succeed())

		// Drive to Verifying, then set the operator's tamper evidence (a
		// protected path changed between the pinned SHAs) + the runner's claim
		// that it's done (the anti-gaming claim).
		driveToVerifying(nn)
		got := &coxv1alpha1.Loop{}
		Expect(k8sClient.Get(ctx, nn, got)).To(Succeed())
		Expect(got.Status.Phase).To(Equal(coxv1alpha1.LoopPhaseVerifying),
			"setup: the Loop must reach Verifying before the tamper verdict")

		got.Status.Verify = &coxv1alpha1.VerifyStatus{TamperExitCode: 1}
		got.Status.ObservedPhase = coxv1alpha1.LoopPhaseVerifying // the runner's claim
		Expect(k8sClient.Status().Update(ctx, got)).To(Succeed())

		r := &LoopReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
		Expect(err).NotTo(HaveOccurred())

		Expect(k8sClient.Get(ctx, nn, got)).To(Succeed())
		Expect(got.Status.Phase).To(Equal(coxv1alpha1.LoopPhaseFailed),
			"D10: a non-zero tamper code must end the Loop Failed, not Succeeded")
		var failed bool
		for _, c := range got.Status.Conditions {
			if c.Type == string(coxv1alpha1.LoopPhaseFailed) && c.Status == metav1.ConditionTrue &&
				c.Reason == TamperedVerifyReason {
				failed = true
			}
		}
		Expect(failed).To(BeTrue(),
			"the terminal condition must record reason TamperedVerify (the operator's evidence, not the claim)")
	})

	It("stays Verifying when the tamper code is clean (0) — no tamper verdict", func() {
		ns := "b2-clean-" + nowSuffix()
		nsObj := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}
		Expect(k8sClient.Create(ctx, nsObj)).To(Succeed())
		defer func() { _ = k8sClient.Delete(context.Background(), nsObj) }()

		nn := types.NamespacedName{Name: "b2-clean", Namespace: ns}
		loop := &coxv1alpha1.Loop{
			ObjectMeta: metav1.ObjectMeta{Name: "b2-clean", Namespace: ns},
			Spec:       coxv1alpha1.LoopSpec{Goal: loopGoal, Workspace: testWorkspace()},
		}
		Expect(k8sClient.Create(ctx, loop)).To(Succeed())
		driveToVerifying(nn)

		got := &coxv1alpha1.Loop{}
		Expect(k8sClient.Get(ctx, nn, got)).To(Succeed())
		got.Status.Verify = &coxv1alpha1.VerifyStatus{TamperExitCode: 0} // clean
		got.Status.ObservedPhase = coxv1alpha1.LoopPhaseVerifying
		Expect(k8sClient.Status().Update(ctx, got)).To(Succeed())

		r := &LoopReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
		Expect(err).NotTo(HaveOccurred())

		Expect(k8sClient.Get(ctx, nn, got)).To(Succeed())
		Expect(got.Status.Phase).To(Equal(coxv1alpha1.LoopPhaseVerifying),
			"a clean tamper code must not flip the Loop to Failed; the checks (B3) decide the outcome")
	})
})
