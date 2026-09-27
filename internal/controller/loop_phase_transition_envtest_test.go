/*
Copyright 2026 papawattu.

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

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	coxv1alpha1 "github.com/papawattu/coxswain/api/v1alpha1"
)

// B1 (cluster seam): the controller applies the transition when the runner
// reports a finished phase. Each It is self-contained with its own namespace
// (envtest gotcha: a shared namespace lets one It's sandbox/status leak into
// the next). The operator is the sole writer of Loop status (ADR-0004) — in
// the test we set ObservedPhase directly to stand in for the runner's
// result.json report, then reconcile and assert the operator advanced the phase.
var _ = Describe("B1 phase transitions via Reconcile", func() {
	ctx := context.Background()

	// reportPhase simulates the runner reporting that it finished <reported>
	// (the phase it was executing) by setting status.observedPhase, then
	// reconciles. The operator advances status.phase iff reported is the
	// immediate-next phase after the current one (nextPhase).
	reportPhase := func(ns, name string, reported coxv1alpha1.LoopPhase) {
		nn := types.NamespacedName{Name: name, Namespace: ns}
		loop := &coxv1alpha1.Loop{}
		Expect(k8sClient.Get(ctx, nn, loop)).To(Succeed())
		loop.Status.ObservedPhase = reported
		Expect(k8sClient.Status().Update(ctx, loop)).To(Succeed())
		r := &LoopReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
		Expect(err).NotTo(HaveOccurred())
		Expect(k8sClient.Get(ctx, nn, loop)).To(Succeed())
	}

	BeforeEach(func() {
		// Each It creates its own namespace inline (self-contained) — the
		// shared BeforeEach namespace breaks after the first It's teardown.
	})

	makeLoop := func(ns, name string) types.NamespacedName {
		nn := types.NamespacedName{Name: name, Namespace: ns}
		loop := &coxv1alpha1.Loop{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec:       coxv1alpha1.LoopSpec{Goal: loopGoal, Workspace: testWorkspace()},
		}
		Expect(k8sClient.Create(ctx, loop)).To(Succeed())
		// Prime: a fresh Loop reconciles to Pending with the sandbox ensured.
		r := &LoopReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
		Expect(err).NotTo(HaveOccurred())
		return nn
	}

	It("advances Pending -> Planning when the runner reports Planning done", func() {
		ns := "b1-plan-" + nowSuffix()
		nsObj := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}
		Expect(k8sClient.Create(ctx, nsObj)).To(Succeed())
		defer func() { _ = k8sClient.Delete(context.Background(), nsObj) }()

		nn := makeLoop(ns, "b1-plan")
		reportPhase(ns, "b1-plan", coxv1alpha1.LoopPhasePlanning)

		loop := &coxv1alpha1.Loop{}
		Expect(k8sClient.Get(ctx, nn, loop)).To(Succeed())
		Expect(loop.Status.Phase).To(Equal(coxv1alpha1.LoopPhasePlanning),
			"the operator should advance to Planning once the runner reports it done")
		Expect(loop.Status.DesiredPhase).To(Equal(coxv1alpha1.LoopPhasePlanning))
	})

	It("advances Planning -> Implementing -> Verifying -> Succeeded on the happy path", func() {
		ns := "b1-happy-" + nowSuffix()
		nsObj := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}
		Expect(k8sClient.Create(ctx, nsObj)).To(Succeed())
		defer func() { _ = k8sClient.Delete(context.Background(), nsObj) }()

		nn := makeLoop(ns, "b1-happy")

		// Prime left Phase=Pending. Drive the full happy path:
		// Pending -> Planning -> Implementing -> Verifying -> Succeeded.
		reportPhase(ns, "b1-happy", coxv1alpha1.LoopPhasePlanning)
		reportPhase(ns, "b1-happy", coxv1alpha1.LoopPhaseImplementing)
		reportPhase(ns, "b1-happy", coxv1alpha1.LoopPhaseVerifying)
		reportPhase(ns, "b1-happy", coxv1alpha1.LoopPhaseSucceeded)

		loop := &coxv1alpha1.Loop{}
		Expect(k8sClient.Get(ctx, nn, loop)).To(Succeed())
		Expect(loop.Status.Phase).To(Equal(coxv1alpha1.LoopPhaseSucceeded),
			"the happy path should end in Succeeded")
	})

	It("does not advance when the runner's report skips ahead", func() {
		ns := "b1-stay-" + nowSuffix()
		nsObj := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}
		Expect(k8sClient.Create(ctx, nsObj)).To(Succeed())
		defer func() { _ = k8sClient.Delete(context.Background(), nsObj) }()

		nn := makeLoop(ns, "b1-stay")
		// Prime left Phase=Pending. A report of Succeeded skips ahead of the
		// only valid step (Planning), so the operator must stay in Pending.
		loop := &coxv1alpha1.Loop{}
		Expect(k8sClient.Get(ctx, nn, loop)).To(Succeed())
		loop.Status.ObservedPhase = coxv1alpha1.LoopPhaseSucceeded
		Expect(k8sClient.Status().Update(ctx, loop)).To(Succeed())
		r := &LoopReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
		Expect(err).NotTo(HaveOccurred())

		Expect(k8sClient.Get(ctx, nn, loop)).To(Succeed())
		Expect(loop.Status.Phase).To(Equal(coxv1alpha1.LoopPhasePending),
			"a skip-ahead report must not advance the machine")
	})
})
