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

func int32PtrEnv(v int32) *int32 {
	p := new(int32)
	*p = v
	return p
}

// A fixed commit SHA the B2 envtest cases use for both the evidence's and the
// operator-pinned verified commit (named to keep goconst quiet).
const b2Commit = "deadbeef"

// B2 (D10/D24) anti-gaming + fail-closed envtest. The operator's TamperedVerify
// decision at Verifying is driven by its OWN evidence (status.verify.*), never
// by the runner's result.json claim. In envtest (no Job controller) the tests
// set status.verify.* directly.
//
// The tri-state (D24): nil tamper evidence is NEVER clean (fail-closed) — the
// Loop stays in Verifying and B3 cannot reach Succeeded on it. Explicit 0 is
// clean (B3's check containers then decide). Non-zero ends Failed:TamperedVerify
// (terminal) even when the runner claims Verifying done.
var _ = Describe("B2 TamperedVerify via base-commit glob diff (D10/D24)", func() {
	ctx := context.Background()

	// driveToVerifying drives a fresh Loop through the B1 claim path to Verifying.
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

	makeLoop := func(ns, name string) types.NamespacedName {
		nn := types.NamespacedName{Name: name, Namespace: ns}
		loop := &coxv1alpha1.Loop{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec:       coxv1alpha1.LoopSpec{Goal: loopGoal, Workspace: testWorkspace()},
		}
		Expect(k8sClient.Create(ctx, loop)).To(Succeed())
		return nn
	}

	It("ends Failed:TamperedVerify (terminal) when the tamper code is non-zero, even though the runner claims success (D10 anti-gaming)", func() {
		ns := "b2-tamper-" + nowSuffix()
		nsObj := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}
		Expect(k8sClient.Create(ctx, nsObj)).To(Succeed())
		defer func() { _ = k8sClient.Delete(context.Background(), nsObj) }()

		nn := makeLoop(ns, "b2-tamper")
		driveToVerifying(nn)
		got := &coxv1alpha1.Loop{}
		Expect(k8sClient.Get(ctx, nn, got)).To(Succeed())
		Expect(got.Status.Phase).To(Equal(coxv1alpha1.LoopPhaseVerifying),
			"setup: the Loop must reach Verifying before the tamper verdict")

		// The operator's tamper evidence: a protected path changed (non-zero),
		// for the current verifiedCommit. Plus the runner's claim that it's done.
		// The operator pinned the current commit (D11) to the SAME SHA the
		// evidence names, so the evidence is bound (D27).
		got.Status.Verify = &coxv1alpha1.VerifyStatus{
			TamperExitCode: int32PtrEnv(1),
			VerifiedCommit: b2Commit,
			JobName:        "b2-tamper-verify-1",
		}
		got.Status.CurrentVerify = &coxv1alpha1.CurrentVerifyStatus{VerifiedCommit: b2Commit}
		got.Status.ObservedPhase = coxv1alpha1.LoopPhaseVerifying
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

	It("stays Verifying when the tamper code is clean (0) — B3's checks decide the outcome", func() {
		ns := "b2-clean-" + nowSuffix()
		nsObj := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}
		Expect(k8sClient.Create(ctx, nsObj)).To(Succeed())
		defer func() { _ = k8sClient.Delete(context.Background(), nsObj) }()

		nn := makeLoop(ns, "b2-clean")
		driveToVerifying(nn)
		got := &coxv1alpha1.Loop{}
		Expect(k8sClient.Get(ctx, nn, got)).To(Succeed())
		// Clean tamper code (0) bound to the current commit (D27): the evidence
		// names the same SHA the operator pinned, so it counts.
		got.Status.Verify = &coxv1alpha1.VerifyStatus{
			TamperExitCode: int32PtrEnv(0),
			VerifiedCommit: b2Commit,
		}
		got.Status.CurrentVerify = &coxv1alpha1.CurrentVerifyStatus{VerifiedCommit: b2Commit}
		got.Status.ObservedPhase = coxv1alpha1.LoopPhaseVerifying
		Expect(k8sClient.Status().Update(ctx, got)).To(Succeed())

		r := &LoopReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
		Expect(err).NotTo(HaveOccurred())

		Expect(k8sClient.Get(ctx, nn, got)).To(Succeed())
		Expect(got.Status.Phase).To(Equal(coxv1alpha1.LoopPhaseVerifying),
			"a clean tamper code must not flip the Loop to Failed; B3's checks decide")
	})

	It("stays Verifying when there is NO tamper evidence (nil) — D24 fail-closed, never clean", func() {
		ns := "b2-nonev-" + nowSuffix()
		nsObj := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}
		Expect(k8sClient.Create(ctx, nsObj)).To(Succeed())
		defer func() { _ = k8sClient.Delete(context.Background(), nsObj) }()

		nn := makeLoop(ns, "b2-nonev")
		driveToVerifying(nn)
		got := &coxv1alpha1.Loop{}
		Expect(k8sClient.Get(ctx, nn, got)).To(Succeed())
		// No verify block at all (the Job never ran / crashed before the tamper
		// container finished / status lost). Absence of evidence is NOT a pass.
		got.Status.Verify = nil
		got.Status.ObservedPhase = coxv1alpha1.LoopPhaseVerifying
		Expect(k8sClient.Status().Update(ctx, got)).To(Succeed())

		r := &LoopReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
		Expect(err).NotTo(HaveOccurred())

		Expect(k8sClient.Get(ctx, nn, got)).To(Succeed())
		Expect(got.Status.Phase).To(Equal(coxv1alpha1.LoopPhaseVerifying),
			"D24: nil tamper evidence must not advance the Loop out of Verifying (fail-closed)")
		Expect(got.Status.Phase).ToNot(Equal(coxv1alpha1.LoopPhaseSucceeded),
			"the Loop must NOT reach Succeeded on no evidence")
		Expect(got.Status.Phase).ToNot(Equal(coxv1alpha1.LoopPhaseFailed),
			"no evidence is Unknown, not Tampered — it must not end Failed either")
	})

	It("stays Verifying when the evidence names a DIFFERENT commit than the pinned current one (D27 stale evidence)", func() {
		ns := "b2-stale-" + nowSuffix()
		nsObj := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}
		Expect(k8sClient.Create(ctx, nsObj)).To(Succeed())
		defer func() { _ = k8sClient.Delete(context.Background(), nsObj) }()

		nn := makeLoop(ns, "b2-stale")
		driveToVerifying(nn)
		got := &coxv1alpha1.Loop{}
		Expect(k8sClient.Get(ctx, nn, got)).To(Succeed())
		// The evidence is a CLEAN 0, but it names commit A ("aaaa") — leftover from
		// a previous iteration's Job — while the operator pinned the CURRENT
		// commit to B ("bbbb") at Verifying start (D11). A clean-but-stale
		// evidence must NOT be treated as clean (D27 fail-closed): the Loop stays
		// in Verifying and B3 cannot reach Succeeded on it.
		got.Status.Verify = &coxv1alpha1.VerifyStatus{
			TamperExitCode: int32PtrEnv(0),
			VerifiedCommit: "aaaa",
			JobName:        "b2-stale-verify-1",
		}
		got.Status.CurrentVerify = &coxv1alpha1.CurrentVerifyStatus{VerifiedCommit: "bbbb"}
		got.Status.ObservedPhase = coxv1alpha1.LoopPhaseVerifying
		Expect(k8sClient.Status().Update(ctx, got)).To(Succeed())

		r := &LoopReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
		Expect(err).NotTo(HaveOccurred())

		Expect(k8sClient.Get(ctx, nn, got)).To(Succeed())
		Expect(got.Status.Phase).To(Equal(coxv1alpha1.LoopPhaseVerifying),
			"D27: a clean-0 evidence bound to a different commit than the pinned current must not advance the Loop")
		Expect(got.Status.Phase).ToNot(Equal(coxv1alpha1.LoopPhaseSucceeded),
			"stale evidence must not be treated as clean (the loop must not reach Succeeded on it)")
	})
})
