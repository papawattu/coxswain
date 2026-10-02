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
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	sandboxv1beta1 "sigs.k8s.io/agent-sandbox/api/v1beta1"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	coxv1alpha1 "github.com/papawattu/coxswain/api/v1alpha1"
)

// B1 (cluster seam, legacy status-path): the controller applies the
// transition when the runner reports a finished phase. Each It is
// self-contained with its own namespace (envtest gotcha: a shared namespace
// lets one It's sandbox/status leak into the next). The operator is the sole
// writer of Loop status (ADR-0004) — in the test we set ObservedPhase
// directly to stand in for the runner's report, then reconcile and assert the
// operator advanced the phase. S4 (ADR-0004): this file keeps the pure
// nextPhase contract as a seam-level spec; the live claim reader (the agent
// termination message via the APIReader path) is covered by
// loop_s4_reader_test.go. The S4 bootstrap moves a fresh Loop Pending ->
// Planning on its first reconcile, so the effective transition under test is
// the step PAST the bootstrap (a report of Implementing advances
// Planning -> Implementing, and so on).
var _ = Describe("B1 phase transitions via Reconcile", func() {
	ctx := context.Background()

	// ensureClaimPod creates the stand-in sandbox pod the S4 live claim reader
	// reads. A bare reconciler (no APIReader) falls back to the cached client;
	// a stand-in pod with NO terminated agent status yields (nil, nil) — the
	// reader requeues and records nothing, and no sandbox recycle deletes a
	// claim the spec sets by hand.
	ensureClaimPod := func(ns, name string) {
		_ = k8sClient.Create(ctx, &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: name + "-sandbox", Namespace: ns},
			Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: agentContainerName, Image: s3StandinImage}}},
		})
	}
	reconcileLoop := func(ns, name string) {
		_, err := (&LoopReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}).
			Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: name, Namespace: ns}})
		Expect(err).NotTo(HaveOccurred())
	}
	// ensureSandboxObject creates the sandbox object if it does not exist
	// (the operator's annotation-based recycle deletes it on a phase advance;
	// recreating it is the stand-in for the operator's own ensureSandbox on
	// the next reconcile).
	ensureSandboxObject := func(ns, name string) {
		sb := &sandboxv1beta1.Sandbox{}
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: name + "-sandbox", Namespace: ns}, sb); apierrors.IsNotFound(err) {
			_ = k8sClient.Create(ctx, &sandboxv1beta1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{Name: name + "-sandbox", Namespace: ns},
			})
		}
	}

	// reportPhase simulates the runner reporting that it finished <reported>
	// (the phase it was executing) by setting status.observedPhase, then
	// reconciling. The operator advances status.phase iff reported is the
	// immediate-next phase after the current one (nextPhase). The annotation-
	// based recycle may delete the sandbox on an advance; recreate it if so
	// (the stand-in for the operator's own ensureSandbox on the next reconcile).
	reportPhase := func(ns, name string, reported coxv1alpha1.LoopPhase) {
		nn := types.NamespacedName{Name: name, Namespace: ns}
		loop := &coxv1alpha1.Loop{}
		Expect(k8sClient.Get(ctx, nn, loop)).To(Succeed())
		loop.Status.ObservedPhase = reported
		Expect(k8sClient.Status().Update(ctx, loop)).To(Succeed())
		ensureClaimPod(ns, name)
		reconcileLoop(ns, name)
		ensureSandboxObject(ns, name)
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
		// Prime: the S4 bootstrap moves a fresh Loop to Planning (option B).
		// ensureClaimPod creates the stand-in claim pod the live reader reads
		// (see reportPhase).
		ensureClaimPod(ns, name)
		reconcileLoop(ns, name)
		return nn
	}

	It("records a report of the bootstrap phase (Planning) without advancing (S4 bootstrap already moved Pending -> Planning)", func() {
		ns := "b1-plan-" + nowSuffix()
		nsObj := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}
		Expect(k8sClient.Create(ctx, nsObj)).To(Succeed())
		defer func() { _ = k8sClient.Delete(context.Background(), nsObj) }()

		nn := makeLoop(ns, "b1-plan")
		reportPhase(ns, "b1-plan", coxv1alpha1.LoopPhasePlanning)

		loop := &coxv1alpha1.Loop{}
		Expect(k8sClient.Get(ctx, nn, loop)).To(Succeed())
		Expect(loop.Status.Phase).To(Equal(coxv1alpha1.LoopPhasePlanning),
			"the bootstrap left the Loop at Planning; a report of the CURRENT phase (Planning) must not advance again")
		Expect(loop.Status.DesiredPhase).To(Equal(coxv1alpha1.LoopPhasePlanning))
	})

	It("advances Planning -> Implementing -> Verifying from successive reports, and stops there (D23)", func() {
		ns := "b1-happy-" + nowSuffix()
		nsObj := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}
		Expect(k8sClient.Create(ctx, nsObj)).To(Succeed())
		defer func() { _ = k8sClient.Delete(context.Background(), nsObj) }()

		nn := makeLoop(ns, "b1-happy")

		// The S4 bootstrap left Phase=Planning. Drive the claim-driven path as
		// far as it goes: Planning -> Implementing -> Verifying. The exit from
		// Verifying to Succeeded is evidence-gated (verify Job, B3) and must NOT
		// be driven by a runner report, so the happy-path-to-Succeeded test lives
		// in B3, not here.
		reportPhase(ns, "b1-happy", coxv1alpha1.LoopPhaseImplementing)
		reportPhase(ns, "b1-happy", coxv1alpha1.LoopPhaseVerifying)

		loop := &coxv1alpha1.Loop{}
		Expect(k8sClient.Get(ctx, nn, loop)).To(Succeed())
		Expect(loop.Status.Phase).To(Equal(coxv1alpha1.LoopPhaseVerifying),
			"the claim-driven path should stop at Verifying")
	})

	It("does not let a runner report of Succeeded exit Verifying (D23, cluster seam)", func() {
		ns := "b1-nogate-" + nowSuffix()
		nsObj := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}
		Expect(k8sClient.Create(ctx, nsObj)).To(Succeed())
		defer func() { _ = k8sClient.Delete(context.Background(), nsObj) }()

		nn := makeLoop(ns, "b1-nogate")
		// Drive to Verifying first (the S4 bootstrap left Phase=Planning).
		reportPhase(ns, "b1-nogate", coxv1alpha1.LoopPhaseImplementing)
		reportPhase(ns, "b1-nogate", coxv1alpha1.LoopPhaseVerifying)

		// The runner now claims Succeeded, but with no verify Job the operator
		// must stay in Verifying — Succeeded is only ever evidence-gated (B3).
		reportPhase(ns, "b1-nogate", coxv1alpha1.LoopPhaseSucceeded)

		loop := &coxv1alpha1.Loop{}
		Expect(k8sClient.Get(ctx, nn, loop)).To(Succeed())
		Expect(loop.Status.Phase).To(Equal(coxv1alpha1.LoopPhaseVerifying),
			"a runner report of Succeeded must not complete the Loop without verify evidence")
	})

	It("does not advance when the runner's report skips ahead", func() {
		ns := "b1-stay-" + nowSuffix()
		nsObj := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}
		Expect(k8sClient.Create(ctx, nsObj)).To(Succeed())
		defer func() { _ = k8sClient.Delete(context.Background(), nsObj) }()

		nn := makeLoop(ns, "b1-stay")
		// The S4 bootstrap left Phase=Planning. A report of Succeeded skips
		// ahead of the only valid step (Implementing), so the operator must
		// stay in Planning.
		loop := &coxv1alpha1.Loop{}
		Expect(k8sClient.Get(ctx, nn, loop)).To(Succeed())
		loop.Status.ObservedPhase = coxv1alpha1.LoopPhaseSucceeded
		Expect(k8sClient.Status().Update(ctx, loop)).To(Succeed())
		ensureClaimPod(ns, "b1-stay")
		reconcileLoop(ns, "b1-stay")

		Expect(k8sClient.Get(ctx, nn, loop)).To(Succeed())
		Expect(loop.Status.Phase).To(Equal(coxv1alpha1.LoopPhasePlanning),
			"a skip-ahead report must not advance the machine (the bootstrap's Planning is the current phase)")
	})
})
