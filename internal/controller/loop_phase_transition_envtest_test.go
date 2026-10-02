/*
Copyright 2026 papawattu.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on the "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the code for the distributed under the License is distributed on an "AS IS" BASIS,
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

// B1 (phase machine): the controller applies the transition when the runner
// reports a finished phase. Each It is self-contained with its own namespace
// (envtest gotcha: a shared namespace lets one It's sandbox/status leak into
// the next).
//
// R17 (2026-10-03): these specs now drive the machine through the
// readPhaseClaim test seam — the runner's claim is injected exactly as the
// live reader would deliver it (the strict-JSON termination message parsed
// into a *PhaseClaim). The legacy seam that wrote status.observedPhase
// directly (and the production fallback that read it back) was DELETED: it
// clobbered status.progress with the previous phase while a phase was still
// running. A bare reconciler (no APIReader, pod-blind like the scoped cache)
// would find no claim on its own, so the seam is the ONLY source of claims
// here — the operator advances the phase iff the claim's completed phase
// maps to the immediate-next phase (claimPhaseForAdvance + the nextPhase
// table).
//
// The S4 bootstrap moves a fresh Loop Pending -> Planning on its first
// reconcile, so the effective transition under test is the step PAST the
// bootstrap. The LIVE claim reader (the agent termination message via the
// APIReader path) is covered by loop_s4_reader_test.go.
var _ = Describe("B1 phase transitions via Reconcile (claim seam)", func() {
	ctx := context.Background()

	// b1Reconciler builds the reconciler the specs drive: a plain client (the
	// sandbox pod is not created by the specs, so the default claim reader
	// finds nothing) and a readPhaseClaim seam that starts serving "agent
	// still running" ((nil, nil) — the operator requeues). reportPhase re-arms
	// the seam to serve a specific claim for that reconcile.
	b1Reconciler := func() *LoopReconciler {
		r := &LoopReconciler{
			Client: k8sClient,
			Scheme: k8sClient.Scheme(),
		}
		r.readPhaseClaim = func(_ context.Context, _ *coxv1alpha1.Loop) (*PhaseClaim, error) {
			return nil, nil
		}
		return r
	}

	// claim is the runner's claim shape: the phase it EXECUTED (the current
	// phase, never the next) with status success — the completed-phase form
	// the advance path maps through claimPhaseForAdvance.
	claim := func(phase coxv1alpha1.LoopPhase) *PhaseClaim {
		return &PhaseClaim{ObservedPhase: phase, Status: "success"}
	}

	reconcileLoop := func(r *LoopReconciler, ns, name string) {
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: name, Namespace: ns}})
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

	// reportPhase injects the runner's claim (the phase it EXECUTED, status
	// success) via the readPhaseClaim seam, then reconciles once. The operator
	// advances status.phase iff the completed phase maps to the immediate-next
	// phase (claimPhaseForAdvance + nextPhase). The annotation-based recycle
	// may delete the sandbox on an advance; recreate it if so. The seam goes
	// back to (nil, nil) (agent running) after the reconcile, so a later
	// reportPhase starts from a clean poll.
	reportPhase := func(r *LoopReconciler, ns, name string, executed coxv1alpha1.LoopPhase) {
		r.readPhaseClaim = func(_ context.Context, _ *coxv1alpha1.Loop) (*PhaseClaim, error) {
			return claim(executed), nil
		}
		reconcileLoop(r, ns, name)
		r.readPhaseClaim = func(_ context.Context, _ *coxv1alpha1.Loop) (*PhaseClaim, error) {
			return nil, nil
		}
		ensureSandboxObject(ns, name)
	}

	BeforeEach(func() {
		// Each It creates its own namespace inline (self-contained) — the
		// shared BeforeEach namespace breaks after the first It's teardown.
	})

	// makeLoop creates the Loop and primes it: the S4 bootstrap moves it
	// Pending -> Planning (option B). The seam serves (nil, nil) on the prime
	// reconcile (the agent is "still running") so the prime never consumes a
	// claim; reportPhase re-arms the seam per claim.
	makeLoop := func(ns, name string) (types.NamespacedName, *LoopReconciler) {
		nn := types.NamespacedName{Name: name, Namespace: ns}
		r := b1Reconciler()
		loop := &coxv1alpha1.Loop{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec:       coxv1alpha1.LoopSpec{Goal: loopGoal, Workspace: testWorkspace()},
		}
		Expect(k8sClient.Create(ctx, loop)).To(Succeed())
		reconcileLoop(r, ns, name)
		return nn, r
	}

	It("records the bootstrap phase's report (Planning) and advances to Implementing (a completed phase completes the phase)", func() {
		ns := "b1-plan-" + nowSuffix()
		nsObj := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}
		Expect(k8sClient.Create(ctx, nsObj)).To(Succeed())
		defer func() { _ = k8sClient.Delete(context.Background(), nsObj) }()

		nn, r := makeLoop(ns, "b1-plan")
		reportPhase(r, ns, "b1-plan", coxv1alpha1.LoopPhasePlanning)

		loop := &coxv1alpha1.Loop{}
		Expect(k8sClient.Get(ctx, nn, loop)).To(Succeed())
		Expect(loop.Status.Phase).To(Equal(coxv1alpha1.LoopPhaseImplementing),
			"the S4 bootstrap moved Pending -> Planning, so a COMPLETED Planning claim completes that phase: the machine advances to its successor (claimPhaseForAdvance + nextPhase)")
		Expect(loop.Status.DesiredPhase).To(Equal(coxv1alpha1.LoopPhaseImplementing))
		Expect(loop.Status.ObservedPhase).To(Equal(coxv1alpha1.LoopPhasePlanning),
			"status.observedPhase records the claimed phase (the B1 field)")
		Expect(loop.Status.Progress).NotTo(BeNil(), "the claim is recorded into progress (OS1)")
		Expect(loop.Status.Progress.LastResultStatus).To(Equal("success"))
	})

	It("advances Planning -> Implementing -> Verifying from successive claims, and stops there (D23)", func() {
		ns := "b1-happy-" + nowSuffix()
		nsObj := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}
		Expect(k8sClient.Create(ctx, nsObj)).To(Succeed())
		defer func() { _ = k8sClient.Delete(context.Background(), nsObj) }()

		nn, r := makeLoop(ns, "b1-happy")

		// The S4 bootstrap left Phase=Planning. Drive the claim-driven path as
		// far as it goes: Planning -> Implementing -> Verifying. The exit from
		// Verifying to Succeeded is evidence-gated (verify Job, B3) and must NOT
		// be driven by a runner report, so the happy-path-to-Succeeded test lives
		// in B3, not here.
		reportPhase(r, ns, "b1-happy", coxv1alpha1.LoopPhasePlanning)
		reportPhase(r, ns, "b1-happy", coxv1alpha1.LoopPhaseImplementing)

		loop := &coxv1alpha1.Loop{}
		Expect(k8sClient.Get(ctx, nn, loop)).To(Succeed())
		Expect(loop.Status.Phase).To(Equal(coxv1alpha1.LoopPhaseVerifying),
			"the claim-driven path should stop at Verifying")
		Expect(loop.Status.DesiredPhase).To(Equal(coxv1alpha1.LoopPhaseVerifying))
	})

	It("does not let a runner report of Succeeded exit Verifying (D23, cluster seam)", func() {
		ns := "b1-nogate-" + nowSuffix()
		nsObj := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}
		Expect(k8sClient.Create(ctx, nsObj)).To(Succeed())
		defer func() { _ = k8sClient.Delete(context.Background(), nsObj) }()

		_, r := makeLoop(ns, "b1-nogate")
		// Drive to Verifying first (the S4 bootstrap left Phase=Planning).
		reportPhase(r, ns, "b1-nogate", coxv1alpha1.LoopPhasePlanning)
		reportPhase(r, ns, "b1-nogate", coxv1alpha1.LoopPhaseImplementing)

		// The runner now claims Succeeded, but with no verify Job the operator
		// must stay in Verifying — Succeeded is only ever evidence-gated (B3).
		// The Verifying hold returns before any claim handling: even a claim
		// naming Verifying changes nothing.
		reportPhase(r, ns, "b1-nogate", coxv1alpha1.LoopPhaseVerifying)

		loop := &coxv1alpha1.Loop{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "b1-nogate", Namespace: ns}, loop)).To(Succeed())
		Expect(loop.Status.Phase).To(Equal(coxv1alpha1.LoopPhaseVerifying),
			"a runner claim must not complete the Loop without verify evidence")
	})

	It("does not advance when the runner's report skips ahead", func() {
		ns := "b1-stay-" + nowSuffix()
		nsObj := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}
		Expect(k8sClient.Create(ctx, nsObj)).To(Succeed())
		defer func() { _ = k8sClient.Delete(context.Background(), nsObj) }()

		nn, r := makeLoop(ns, "b1-stay")
		// The S4 bootstrap left Phase=Planning. A completed Succeeded claim
		// skips ahead of the only valid step (a completed Planning claim maps
		// to Implementing), so the operator must stay in Planning. (A live
		// runner could never emit this — the R17 parse validation rejects an
		// observedPhase outside {Planning, Implementing, Verifying} — but the
		// seam can deliver it, which is what proves the pure-match hold here.)
		reportPhase(r, ns, "b1-stay", coxv1alpha1.LoopPhaseSucceeded)

		loop := &coxv1alpha1.Loop{}
		Expect(k8sClient.Get(ctx, nn, loop)).To(Succeed())
		Expect(loop.Status.Phase).To(Equal(coxv1alpha1.LoopPhasePlanning),
			"a skip-ahead report must not advance the machine (the bootstrap's Planning is the current phase)")
	})

	It("while a phase is running (no terminated claim yet), progress stays at the last recorded claim", func() {
		// R17 P1 acceptance: at Implementing with the agent still running,
		// progress must STAY {Planning, success} across reconciles — the
		// deleted B1 seam used to clobber it with {Planning, ""} + a fresh
		// timestamp on every poll.
		ns := "b1-run-" + nowSuffix()
		nsObj := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}
		Expect(k8sClient.Create(ctx, nsObj)).To(Succeed())
		defer func() { _ = k8sClient.Delete(context.Background(), nsObj) }()

		nn, r := makeLoop(ns, "b1-run")
		reportPhase(r, ns, "b1-run", coxv1alpha1.LoopPhasePlanning)

		loop := &coxv1alpha1.Loop{}
		Expect(k8sClient.Get(ctx, nn, loop)).To(Succeed())
		Expect(loop.Status.Phase).To(Equal(coxv1alpha1.LoopPhaseImplementing), "the completed Planning claim advances to Implementing")
		Expect(loop.Status.Progress).NotTo(BeNil())
		Expect(loop.Status.Progress.Phase).To(Equal(coxv1alpha1.LoopPhasePlanning),
			"progress records the phase the claim named (Planning), not the phase the machine moved to")
		Expect(loop.Status.Progress.LastResultStatus).To(Equal("success"))
		activity := loop.Status.Progress.LastActivityTime.DeepCopy()
		Expect(activity).NotTo(BeNil())

		// The agent is now running Implementing: the claim reader finds no
		// terminated claim (the seam serves nil). Reconcile again — the
		// progress record must be untouched (the seam that clobbered it is
		// gone).
		reconcileLoop(r, ns, "b1-run")
		Expect(k8sClient.Get(ctx, nn, loop)).To(Succeed())
		Expect(loop.Status.Phase).To(Equal(coxv1alpha1.LoopPhaseImplementing))
		Expect(loop.Status.Progress).NotTo(BeNil(), "progress must not be dropped while the phase is running")
		Expect(loop.Status.Progress.Phase).To(Equal(coxv1alpha1.LoopPhasePlanning),
			"while Implementing runs, progress stays at the Planning claim (no clobber with an empty status)")
		Expect(loop.Status.Progress.LastResultStatus).To(Equal("success"),
			"the running phase must not blank the last result status")
		Expect(loop.Status.Progress.LastActivityTime).NotTo(BeNil())
		Expect(activity).NotTo(BeNil())
		Expect(loop.Status.Progress.LastActivityTime.Unix()).To(Equal(activity.Unix()),
			"the lastActivityTime of a STABLE claim must not be rewritten on a poll (it is 'last activity', not 'last poll')")
		Expect(loop.Status.ObservedPhase).To(Equal(coxv1alpha1.LoopPhasePlanning),
			"status.observedPhase stays at the claimed phase while the next phase runs")
	})
})
