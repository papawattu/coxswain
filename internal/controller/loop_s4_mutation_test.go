/*
Copyright 2026 papawattu.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the code for the distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the code for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"context"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	coxv1alpha1 "github.com/papawattu/coxswain/api/v1alpha1"
)

// S4 (ADR-0004/ADR-0005): the claim reader's mutation checks (the I43 norm —
// a gate spec must FAIL when the gate is disabled, recorded here instead of
// as a committed mutation).
//
// (1) CACHED CLIENT: if the reader read the sandbox pod through the manager's
//   CACHED client (like the S3 baseCommit reader must not), the pod would be
//   NotFound (the sandbox pod is not in the manager's Pod cache) and the claim
//   would be silently skipped — the Loop never advances from a valid claim.
//   This spec wires a stand-in reader that mimics that mutation (always nil
//   claim) and proves the spec then FAILS to advance: the same pod + claim
//   that the live APIReader path (loop_s4_reader_test.go) advances on must be
//   inert under the cached-client mutation.
//
// (2) GATE DENY: if the OS8 PhaseGate denies the advance (the mutation), the
//   advance must be held (no phase move, no Event, requeue until the hold
//   clears). This spec wires a denying gate and proves the claim is recorded
//   (progress) but the phase does not move.

// denyingPhaseGate is the gate mutation: it denies every advance (a future
// approval hold plugs in here).
type denyingPhaseGate struct{}

// Allow always denies (the mutation under test).
func (denyingPhaseGate) Allow(_ *coxv1alpha1.Loop, _, _ coxv1alpha1.LoopPhase) (bool, string) {
	return false, "mutation: the gate denies every advance"
}

var _ = Describe("S4: claim reader mutation checks (I43)", func() {
	ctx := context.Background()

	s4LoopSpec := coxv1alpha1.LoopSpec{Goal: loopGoal, Workspace: testWorkspace()}

	// standinClaimPod creates the stand-in sandbox pod with a terminated agent
	// carrying a valid claim naming Implementing (the immediate-next phase
	// after Planning) — the SAME claim the live APIReader spec advances on.
	standinClaimPod := func(ns, name string) {
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: name + "-sandbox", Namespace: ns},
			Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: agentContainerName, Image: s3StandinImage}}},
		}
		Expect(k8sClient.Create(ctx, pod)).To(Succeed())
		terminated := int32(0)
		pod.Status.ContainerStatuses = []corev1.ContainerStatus{
			{Name: agentContainerName, State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
				ExitCode: terminated, Message: `{"observedPhase":"Implementing","status":"success","blockedReason":""}`}}},
		}
		Expect(k8sClient.Status().Update(ctx, pod)).To(Succeed())
	}

	It("mutation-check: a cached-client read (the pod is NOT in the manager's Pod cache) must leave the claim unread", func() {
		ns := "s4-mut-cache-" + nowSuffix()
		Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
		defer func() {
			_ = k8sClient.Delete(context.Background(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})
		}()

		// The mutation under test: the reader resolves the pod through the
		// manager's CACHED client. In a live deployment the sandbox pod is NOT
		// in the manager's Pod cache (the cache is scoped to the controller's
		// watched types), so a cached Get returns NotFound and the reader
		// returns (nil, nil) — the claim is silently skipped. This stand-in
		// reader reproduces that mutation exactly (always (nil, nil)); the
		// spec then asserts the Loop does NOT advance, which is the OPPOSITE
		// of the live APIReader spec's outcome (loop_s4_reader_test.go: the
		// same pod + claim advances Planning -> Implementing).
		cached := &LoopReconciler{
			Client: k8sClient,
			Scheme: k8sClient.Scheme(),
			readPhaseClaim: func(ctx context.Context, loop *coxv1alpha1.Loop) (*PhaseClaim, error) {
				// Mimics the cached-client mutation: the sandbox pod is
				// NotFound in the scoped cache, so the reader sees no claim.
				return nil, nil
			},
		}
		nn := types.NamespacedName{Name: "cachelp", Namespace: ns}
		Expect(k8sClient.Create(ctx, &coxv1alpha1.Loop{
			ObjectMeta: metav1.ObjectMeta{Name: "cachelp", Namespace: ns},
			Spec:       s4LoopSpec,
		})).To(Succeed())
		_, err := cached.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
		Expect(err).NotTo(HaveOccurred())

		standinClaimPod(ns, "cachelp")
		res, err := cached.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
		Expect(err).NotTo(HaveOccurred())
		Expect(res.RequeueAfter).To(BeNumerically(">", time.Second),
			"a cached-client read finds no claim (NotFound in the scoped cache): the reader requeues")
		loop := &coxv1alpha1.Loop{}
		Expect(k8sClient.Get(ctx, nn, loop)).To(Succeed())
		Expect(loop.Status.Phase).To(Equal(coxv1alpha1.LoopPhasePlanning),
			"the mutation leaves the claim unread: the Loop must NOT advance (the live APIReader spec asserts the SAME claim advances — that spec FAILS under this mutation)")
		Expect(loop.Status.ObservedPhase).To(BeEmpty(), "a cached-client read records no claim")
	})

	It("mutation-check: a denying PhaseGate must hold the advance (recorded, not acted on)", func() {
		ns := "s4-mut-gate-" + nowSuffix()
		Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
		defer func() {
			_ = k8sClient.Delete(context.Background(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})
		}()

		// The mutation under test: the OS8 gate DENIES the advance (a future
		// approval hold plugs in here). The live build ships autoApprovePhaseGate
		// (option B); under the denying mutation the claim is READ and
		// RECORDED (progress) but the phase is HELD (no advance, no Event,
		// requeue until the hold clears). The loop_s4_reader_test.go happy-path
		// spec (which asserts the advance) FAILS under this mutation.
		denied := &LoopReconciler{
			Client:    k8sClient,
			Scheme:    k8sClient.Scheme(),
			apiReader: k8sClient,
			phaseGate: denyingPhaseGate{},
		}
		nn := types.NamespacedName{Name: "gatelp", Namespace: ns}
		Expect(k8sClient.Create(ctx, &coxv1alpha1.Loop{
			ObjectMeta: metav1.ObjectMeta{Name: "gatelp", Namespace: ns},
			Spec:       s4LoopSpec,
		})).To(Succeed())
		_, err := denied.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
		Expect(err).NotTo(HaveOccurred())

		standinClaimPod(ns, "gatelp")
		_, err = denied.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
		Expect(err).NotTo(HaveOccurred())

		loop := &coxv1alpha1.Loop{}
		Expect(k8sClient.Get(ctx, nn, loop)).To(Succeed())
		Expect(loop.Status.Phase).To(Equal(coxv1alpha1.LoopPhasePlanning),
			"a denying gate must HOLD the advance (the phase does not move)")
		Expect(loop.Status.DesiredPhase).To(Equal(coxv1alpha1.LoopPhasePlanning),
			"a denying gate must not move the desired phase (no pod recycle for a held advance)")
		Expect(loop.Status.ObservedPhase).To(Equal(coxv1alpha1.LoopPhaseImplementing),
			"the claim is READ and its phase recorded (observedPhase), even when the gate holds the advance")
		Expect(loop.Status.Progress).NotTo(BeNil(),
			"the claim is RECORDED into progress (OS1) even when the gate holds the advance")
		Expect(loop.Status.Progress.Phase).To(Equal(coxv1alpha1.LoopPhaseImplementing))
	})
})
