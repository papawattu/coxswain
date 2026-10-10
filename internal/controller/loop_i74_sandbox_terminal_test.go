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

// I74: a Loop's one-shot runner executes only the Planning or Implementing
// phase. When the Loop reaches a terminal phase (Succeeded, Failed), the
// runner has nothing to execute: leaving the sandbox running makes the agent
// container exit with 'unknown desired-phase "Succeeded"' and the restart
// policy crashloops it for the rest of the Loop's lifetime (seen live on
// i54-hello: after Succeeded, the sandbox agent kept restarting).
//
// The fix has two cooperating parts:
//
//  1. The S4 phase-advance recycle (ensureSandbox): a genuine desired-phase
//     advance (the Sandbox's coxswain.io/desired-phase annotation differs
//     from status.desiredPhase) deletes the Sandbox and requeues. A terminal
//     phase IS such an advance, so the operator DELETES the sandbox — there
//     is no live sandbox to crashloop.
//  2. The P2f suspension gate (sandboxOperatingMode), extended (I74) to
//     suspend on a terminal phase: even if a fresh Sandbox were recreated for
//     the terminal phase, it would be built Suspended, not Running. A
//     Running-mode sandbox for a terminal phase would run the one-shot runner
//     on 'unknown desired-phase <Succeeded|Failed>' and crashloop.
//
// The specs assert the OBSERVABLE outcome: on a terminal phase the sandbox is
// deleted (S4 recycle) and NOT recreated on a re-reconcile (same-Loop
// re-reconcile per the test norms). The deliver Job still runs — it lives in
// its own pod (clone-base/import-agent/push read the workspace from a
// volume, independent of the sandbox pod), so delivery works after Succeeded.
//
// Mutation (recorded in the PR): drop the terminal-phase stop from
// sandboxOperatingMode (revert to spec.suspend||Paused) and the I74 unit spec
// fails (the recreated Sandbox would be built Running — the crashloop).
// The envtest specs also guard the deletion: the S4 recycle must delete the
// sandbox on the terminal-phase advance and must not recreate it on a
// re-reconcile.

import (
	"context"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	sandboxv1beta1 "sigs.k8s.io/agent-sandbox/api/v1beta1"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	coxv1alpha1 "github.com/papawattu/coxswain/api/v1alpha1"
	"github.com/papawattu/coxswain/internal/cni"
)

// i74HeadCommit is the 40-hex verified commit for the I74 delivery spec (the
// D27 stamp the deliver Job carries).
const i74HeadCommit = "d4e5f6a7b8c9d0e1f2a3b4c5d6e7f8a9b0c1d2e3"

// I74 unit spec (a plain Go test, not envtest): the suspension gate must
// suspend on a terminal phase. This is the direct regression guard for the
// crashloop — without the I74 terminal-phase branch, a Sandbox built for a
// terminal phase is Running (the one-shot runner would exit on
// 'unknown desired-phase <Succeeded|Failed>' and crashloop). The
// sandboxOperatingMode gate is the single decision point; the envtest specs
// below cover the operator's deletion + no-recreate behaviour end-to-end.
func TestI74SandboxGateSuspendedOnTerminalPhase(t *testing.T) {
	r := &LoopReconciler{}
	cases := map[coxv1alpha1.LoopPhase]sandboxv1beta1.SandboxOperatingMode{
		coxv1alpha1.LoopPhaseSucceeded: sandboxv1beta1.SandboxOperatingModeSuspended,
		coxv1alpha1.LoopPhaseFailed:    sandboxv1beta1.SandboxOperatingModeSuspended,
	}
	for phase, want := range cases {
		loop := &coxv1alpha1.Loop{}
		loop.Status.Phase = phase
		got := r.sandboxOperatingMode(loop, false)
		if got != want {
			t.Fatalf("sandboxOperatingMode(%s) = %s, want %s (I74: the sandbox must be stopped on a terminal phase)",
				phase, got, want)
		}
	}
	// Negative control: a non-terminal phase with no suspend stays Running
	// (the gate does not over-suspend).
	running := &coxv1alpha1.Loop{}
	running.Status.Phase = coxv1alpha1.LoopPhaseVerifying
	if got := r.sandboxOperatingMode(running, false); got != sandboxv1beta1.SandboxOperatingModeRunning {
		t.Fatalf("sandboxOperatingMode(Verifying) = %s, want Running (the gate must not over-suspend a non-terminal phase)", got)
	}
}

var _ = Describe("I74: the sandbox is stopped once the Loop is terminal (Succeeded, Failed)", func() {
	ctx := context.Background()

	// newI74Reconciler builds a reconciler whose gates are all satisfied so
	// the sandbox reaches Running when it should (D30 AllowUnenforced, D38 CNI
	// enforced, D35a proxy Ready+owned — the P2f fixture's newP2fReconciler).
	newI74Reconciler := func(recorder *record.FakeRecorder) *LoopReconciler {
		r := &LoopReconciler{
			Client:          k8sClient,
			Scheme:          k8sClient.Scheme(),
			AllowUnenforced: true,
			CNIProber:       cni.NewFakeProber(),
			Recorder:        recorder,
			readPhaseClaim:  func(context.Context, *coxv1alpha1.Loop) (*PhaseClaim, error) { return nil, nil },
		}
		r.CNIProber.(*cni.FakeProber).SetResult(cni.CNIProbeResult{Reason: cni.ReasonCNIEnforced})
		return r
	}

	i74NS := func(prefix string) string {
		ns := prefix + "-" + nowSuffix()
		Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
		return ns
	}

	// i74PrimeProxy creates the model secret the D33 proxy needs and lets the
	// operator build the real proxy pod (spec + spec-hash annotation), then
	// marks it Ready — the D35a gate's peer (the P2f fixture's primeProxy).
	i74PrimeProxy := func(r *LoopReconciler, loop *coxv1alpha1.Loop) {
		_ = k8sClient.Create(ctx, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: p2fModelSecret, Namespace: loop.Namespace},
			StringData: map[string]string{modelAPIKey: "i74-dummy", modelBaseURL: p2fModelEndpoint},
		})
		Expect(r.ensureProxy(ctx, loop)).To(Succeed())
		pod := &corev1.Pod{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: proxyPodName(loop.Name), Namespace: loop.Namespace}, pod)).To(Succeed(),
			"I74: the operator's ensureProxy must have created the proxy pod")
		now := metav1.Now()
		pod.Status.Conditions = []corev1.PodCondition{
			{Type: corev1.PodReady, Status: corev1.ConditionTrue, LastTransitionTime: now},
		}
		Expect(k8sClient.Status().Update(ctx, pod)).To(Succeed())
	}

	i74Loop := func(ns, name string, deliveryMode coxv1alpha1.DeliveryMode) {
		loop := &coxv1alpha1.Loop{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec: coxv1alpha1.LoopSpec{
				Goal:      "I74 terminal sandbox spec",
				Workspace: testWorkspace(),
				Agent: coxv1alpha1.AgentConfig{
					Image:             runnerImage,
					Model:             testModel,
					EndpointSecretRef: p2fModelSecret,
					ModelEndpoint:     p2fModelEndpoint,
				},
			},
		}
		if deliveryMode != "" {
			loop.Spec.Delivery = &coxv1alpha1.DeliveryConfig{Mode: deliveryMode}
		}
		Expect(k8sClient.Create(ctx, loop)).To(Succeed())
	}

	i74Reconcile := func(r *LoopReconciler, ns, name string) {
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: name}})
		Expect(err).NotTo(HaveOccurred(), "I74: reconcile %s/%s", ns, name)
	}

	i74GetLoop := func(ns, name string) *coxv1alpha1.Loop {
		got := &coxv1alpha1.Loop{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, got)).To(Succeed())
		return got
	}

	i74SetPhase := func(ns, name string, phase coxv1alpha1.LoopPhase) {
		l := i74GetLoop(ns, name)
		l.Status.Phase = phase
		l.Status.DesiredPhase = phase
		Expect(k8sClient.Status().Update(ctx, l)).To(Succeed())
	}

	// i74SandboxState returns the Sandbox's OperatingMode, or "" if the
	// Sandbox does not exist (a transient gap after the S4 delete, before the
	// next reconcile recreates it). The terminal-phase assertion is on the
	// OperatingMode: the sandbox must NEVER be Running on a terminal phase
	// (a Running-mode sandbox runs the one-shot runner on 'unknown desired-
	// phase <Succeeded|Failed>' and crashloops). Suspended (or absent in the
	// transient gap) is the stopped state.
	i74SandboxState := func(ns, name string) string {
		sb := &sandboxv1beta1.Sandbox{}
		if err := k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: name + "-sandbox"}, sb); err != nil {
			if apierrors.IsNotFound(err) {
				return ""
			}
			Expect(err).NotTo(HaveOccurred())
		}
		return string(sb.Spec.OperatingMode)
	}

	// i74SandboxStopped reports the sandbox is stopped on a terminal phase:
	// either absent (transient gap after the S4 delete) or Suspended. It must
	// NOT be Running (the crashloop).
	i74SandboxStopped := func(ns, name string, detail string) {
		state := i74SandboxState(ns, name)
		Expect(state).ToNot(Equal(string(sandboxv1beta1.SandboxOperatingModeRunning)),
			"I74: the sandbox must NOT be Running on a terminal phase (%s); got %q (a Running-mode sandbox would crashloop on the terminal desired-phase)", detail, state)
	}

	It("stops the sandbox on a Succeeded Loop (deleted on the phase advance, not recreated)", func() {
		ns := i74NS("i74-succeeded")
		defer deleteNS(ctx, ns)
		r := newI74Reconciler(record.NewFakeRecorder(64))

		name := "i74s"
		i74Loop(ns, name, "")
		i74PrimeProxy(r, i74GetLoop(ns, name))
		i74Reconcile(r, ns, name) // bootstrap: the sandbox is built (phase Planning)
		Expect(i74SandboxState(ns, name)).To(Equal(string(sandboxv1beta1.SandboxOperatingModeRunning)),
			"I74: a non-terminal Loop's sandbox is Running (the pre-terminal baseline)")

		By("the Loop reaches Succeeded (terminal) -> the sandbox is stopped (not Running)")
		i74SetPhase(ns, name, coxv1alpha1.LoopPhaseSucceeded)
		i74Reconcile(r, ns, name)
		Expect(i74GetLoop(ns, name).Status.Phase).To(Equal(coxv1alpha1.LoopPhaseSucceeded))
		i74SandboxStopped(ns, name, "Succeeded, after the phase advance")

		By("a re-reconcile (same Loop) keeps the sandbox stopped (not Running)")
		i74Reconcile(r, ns, name)
		i74SandboxStopped(ns, name, "Succeeded, after the re-reconcile")
	})

	It("stops the sandbox on a Failed Loop (not Running after the phase advance or re-reconcile)", func() {
		ns := i74NS("i74-failed")
		defer deleteNS(ctx, ns)
		r := newI74Reconciler(record.NewFakeRecorder(64))

		name := "i74f"
		i74Loop(ns, name, "")
		i74PrimeProxy(r, i74GetLoop(ns, name))
		i74Reconcile(r, ns, name) // bootstrap: the sandbox is built
		Expect(i74SandboxState(ns, name)).To(Equal(string(sandboxv1beta1.SandboxOperatingModeRunning)),
			"I74: a non-terminal Loop's sandbox is Running (the pre-terminal baseline)")

		By("the Loop reaches Failed (terminal) -> the sandbox is stopped (not Running)")
		i74SetPhase(ns, name, coxv1alpha1.LoopPhaseFailed)
		i74Reconcile(r, ns, name)
		Expect(i74GetLoop(ns, name).Status.Phase).To(Equal(coxv1alpha1.LoopPhaseFailed))
		i74SandboxStopped(ns, name, "Failed, after the phase advance")

		By("a re-reconcile (same Loop) keeps the sandbox stopped (not Running)")
		i74Reconcile(r, ns, name)
		i74SandboxStopped(ns, name, "Failed, after the re-reconcile")
	})

	It("delivery still creates its Job after Succeeded (the deliver Job does not need the sandbox pod)", func() {
		ns := i74NS("i74-deliver")
		defer deleteNS(ctx, ns)
		r := newI74Reconciler(record.NewFakeRecorder(64))

		name := "i74d"
		i74Loop(ns, name, coxv1alpha1.DeliveryModePullRequest)
		i74PrimeProxy(r, i74GetLoop(ns, name))
		i74Reconcile(r, ns, name) // bootstrap
		By("the Loop reaches Succeeded with delivery mode PullRequest (terminal)")
		l := i74GetLoop(ns, name)
		l.Status.Phase = coxv1alpha1.LoopPhaseSucceeded
		l.Status.DesiredPhase = coxv1alpha1.LoopPhaseSucceeded
		l.Status.CurrentVerify = &coxv1alpha1.CurrentVerifyStatus{VerifiedCommit: i74HeadCommit}
		Expect(k8sClient.Status().Update(ctx, l)).To(Succeed())
		i74Reconcile(r, ns, name)

		By("the deliver Job is created (delivery works after Succeeded — it reads the workspace from a volume, not the sandbox pod)")
		job := &batchv1.Job{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: deliverJobName(name)}, job)).To(Succeed(),
			"I74: the deliver Job must be created on a Succeeded delivery Loop (the deliver step does not need the sandbox)")
		Expect(job.Annotations[verifyCommitAnnotation]).To(Equal(i74HeadCommit),
			"I74: the deliver Job is stamped with the verifiedCommit it was built for")

		By("the sandbox is stopped on the terminal Succeeded phase (delivery is undisturbed)")
		i74SandboxStopped(ns, name, "Succeeded with delivery in flight (the deliver Job runs in its own pod)")
	})
})
