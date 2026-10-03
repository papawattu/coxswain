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
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	coxv1alpha1 "github.com/papawattu/coxswain/api/v1alpha1"
)

// S4 (ADR-0004/ADR-0005): the claim reader's and the gate's mutation checks
// (the I43 norm — a gate spec must FAIL when the gate is disabled, recorded
// here instead of as a committed mutation).
//
// (1) CACHED CLIENT: the live reader MUST read the sandbox pod through the
// APIReader path (a real, non-cached client) — the sandbox pod is NOT in the
// manager's Pod cache, so a read through the reconciler's cached client would
// return NotFound and the claim would be silently skipped (the Loop never
// advances from a valid claim). This spec is REAL, not an imitation: the
// reconciler's Client wraps k8sClient in podBlindClient, which serves every
// type from the real API server EXCEPT *corev1.Pod (NotFound — exactly the
// scoped-cache behaviour), with apiReader: k8sClient (the production default).
// The LIVE production code path runs; the spec asserts the SAME pod + claim
// the APIReader spec advances on (loop_s4_reader_test.go: Planning ->
// Implementing) is READ and the phase ADVANCES. Recorded mutation (not
// committed, per I43): changing readPhaseClaimFromTerminationMessage to
// `reader := client.Reader(r)` (reading through the cached client) makes this
// spec FAIL — the pod-blind Get returns NotFound and the advance never
// happens.
//
// (2) GATE DENY: if the OS8 PhaseGate denies the advance (the mutation), the
// advance must be held (no phase move, no Event, requeue until the hold
// clears) while the claim is still READ and RECORDED (progress). This spec
// is REAL: the reconciler runs the live reader (apiReader: k8sClient, no
// readPhaseClaim seam) over the RUNNER'S REAL claim shape — a terminated
// agent whose claim names the CURRENT phase it executed (Planning), status
// success — with the denying gate wired. The completed-phase mapping
// (claimPhaseForAdvance) maps it to Implementing, so the advance the spec
// holds is the Planning -> Implementing step the auto-approve specs take.
// Recorded mutation (not committed): changing recordPhaseClaim to
// `if allow || true {` (ignoring the gate's answer) makes this spec FAIL —
// the advance is taken despite the deny.

// podBlindClient is the scoped-cache stand-in for the live manager's client:
// it serves every type from the wrapped real client EXCEPT *corev1.Pod,
// which is NotFound (the sandbox pod is not in the manager's Pod cache). A
// Get of any other kind (including the Loop the reconcile reads) is
// delegated to the real client, so the ONLY effect on the production path is
// pod-blindness — exactly the mutation the spec guards against.
type podBlindClient struct {
	client.Client
}

// Get is pod-blind: *corev1.Pod is NotFound (the scoped cache), everything
// else is served from the wrapped real client.
func (p podBlindClient) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	if _, isPod := obj.(*corev1.Pod); isPod {
		return apierrors.NewNotFound(schema.GroupResource{Resource: "pods"}, key.Name)
	}
	return p.Client.Get(ctx, key, obj, opts...)
}

// List is pod-blind the same way Get is: a *corev1.PodList is NotFound (the
// scoped cache has no pods — the S6 deliver read-back's list path), every
// other list is served from the wrapped real client.
func (p podBlindClient) List(ctx context.Context, obj client.ObjectList, opts ...client.ListOption) error {
	if _, isPodList := obj.(*corev1.PodList); isPodList {
		return apierrors.NewNotFound(schema.GroupResource{Resource: "pods"}, "")
	}
	return p.Client.List(ctx, obj, opts...)
}

// denyingPhaseGate is the gate mutation: it denies every advance (a future
// approval hold plugs in here).
type denyingPhaseGate struct{}

// Allow always denies (the mutation under test).
func (denyingPhaseGate) Allow(_ *coxv1alpha1.Loop, _, _ coxv1alpha1.LoopPhase) (bool, string) {
	return false, "mutation: the gate denies every advance"
}

var _ = Describe("S4: claim reader mutation checks (I43, real code)", func() {
	ctx := context.Background()

	// s4LoopSpec uses an IN-CLUSTER .svc repo: the S4 spec exercises the
	// claim-read path (the pod-blind Client vs the real apiReader) and must
	// NOT create an egress proxy — an external repo's unpinned workspace-init
	// clone would (S6) create one and hold the sandbox Suspended (the I42b
	// gate), so the phase advance under test would never fire. The .svc repo
	// keeps the direct repo-peer rule (no proxy hop) and leaves the claim path
	// as the only thing under test.
	s4LoopSpec := coxv1alpha1.LoopSpec{Goal: loopGoal, Workspace: coxv1alpha1.Workspace{
		Repo: inClusterRepoURL,
	}}

	// standinClaimPod creates the stand-in sandbox pod with a terminated agent
	// carrying a valid claim naming phase (the runner's real claim shape: the
	// phase it EXECUTED, the current phase) — the SAME claim the live
	// APIReader specs advance on.
	standinClaimPod := func(ns, name, phase string) {
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: name + "-sandbox", Namespace: ns},
			Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: agentContainerName, Image: s3StandinImage}}},
		}
		Expect(k8sClient.Create(ctx, pod)).To(Succeed())
		terminated := int32(0)
		pod.Status.ContainerStatuses = []corev1.ContainerStatus{
			{Name: agentContainerName, State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
				ExitCode: terminated, Message: `{"observedPhase":"` + phase + `","status":"success","blockedReason":""}`}}},
		}
		Expect(k8sClient.Status().Update(ctx, pod)).To(Succeed())
	}

	It("reads the claim through the APIReader path (the pod-blind cached client must leave it unread)", func() {
		ns := "s4-mut-cache-" + nowSuffix()
		Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
		defer func() {
			_ = k8sClient.Delete(context.Background(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})
		}()

		// The REAL production reader with a pod-blind cached client: the
		// reconciler's Client serves every type from the real API server
		// EXCEPT pods (NotFound — the scoped-cache behaviour), while
		// apiReader is the real, non-cached client (the production default).
		// If the production code read the pod through r.Client (the mutation
		// `reader := client.Reader(r)`), this Get would return NotFound and
		// the advance below would never happen.
		r := &LoopReconciler{
			Client:    podBlindClient{Client: k8sClient},
			Scheme:    k8sClient.Scheme(),
			apiReader: k8sClient,
		}
		nn := types.NamespacedName{Name: "cachelp", Namespace: ns}
		Expect(k8sClient.Create(ctx, &coxv1alpha1.Loop{
			ObjectMeta: metav1.ObjectMeta{Name: "cachelp", Namespace: ns},
			Spec:       s4LoopSpec,
		})).To(Succeed())
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
		Expect(err).NotTo(HaveOccurred())

		// The terminated claim: a completed Planning claim (the current phase,
		// the runner's real claim shape).
		standinClaimPod(ns, "cachelp", "Planning")
		_, err = r.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
		Expect(err).NotTo(HaveOccurred())

		loop := &coxv1alpha1.Loop{}
		Expect(k8sClient.Get(ctx, nn, loop)).To(Succeed())
		Expect(loop.Status.Phase).To(Equal(coxv1alpha1.LoopPhaseImplementing),
			"the claim MUST be read through the APIReader path and advance the machine (the live reader reads the real, non-cached client; a read through the pod-blind cached client would find no pod and leave the Loop here)")
		Expect(loop.Status.ObservedPhase).To(Equal(coxv1alpha1.LoopPhasePlanning),
			"the claim is READ: status.observedPhase records the claimed phase (a pod-blind read records nothing)")
		Expect(loop.Status.Progress).NotTo(BeNil(), "the claim is RECORDED into progress (OS1)")
	})

	It("a denying PhaseGate must hold the advance (the runner's real claim shape, recorded, not acted on)", func() {
		ns := "s4-mut-gate-" + nowSuffix()
		Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
		defer func() {
			_ = k8sClient.Delete(context.Background(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})
		}()

		// The REAL production reader (no readPhaseClaim seam) + the denying
		// gate mutation: the claim is READ and RECORDED (progress) but the
		// advance is HELD (no phase move, no Event, requeue until the hold
		// clears). The claim names the CURRENT phase it executed (Planning,
		// the runner's real shape), so the completed-phase mapping (a claim
		// for the current phase completes that phase) maps it to the
		// Planning -> Implementing advance the auto-approve specs take — the
		// deny holds exactly that step.
		r := &LoopReconciler{
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
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
		Expect(err).NotTo(HaveOccurred())

		standinClaimPod(ns, "gatelp", "Planning")
		res, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
		Expect(err).NotTo(HaveOccurred())
		Expect(res.RequeueAfter).To(BeNumerically(">", time.Second),
			"a held advance requeues (the operator re-asks once the hold clears)")

		loop := &coxv1alpha1.Loop{}
		Expect(k8sClient.Get(ctx, nn, loop)).To(Succeed())
		Expect(loop.Status.Phase).To(Equal(coxv1alpha1.LoopPhasePlanning),
			"a denying gate must HOLD the advance (the phase does not move)")
		Expect(loop.Status.DesiredPhase).To(Equal(coxv1alpha1.LoopPhasePlanning),
			"a denying gate must not move the desired phase (no pod recycle for a held advance)")
		Expect(loop.Status.ObservedPhase).To(Equal(coxv1alpha1.LoopPhasePlanning),
			"the claim is READ and its phase recorded (observedPhase), even when the gate holds the advance")
		Expect(loop.Status.Progress).NotTo(BeNil(),
			"the claim is RECORDED into progress (OS1) even when the gate holds the advance")
		Expect(loop.Status.Progress.Phase).To(Equal(coxv1alpha1.LoopPhasePlanning))
		Expect(loop.Status.Progress.LastResultStatus).To(Equal(claimSuccess))
	})
})
