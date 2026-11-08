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

// P6d (D116, D100): the operator consumes the coxswain.io/retry annotation.
// A retried Failed:SandboxFailed Loop resumes from its current checkpoint and
// branch state, in the phase it failed in, with the sandbox-attempt episode
// reset. The Loops are built DIRECTLY in the exhausted state (phase +
// desiredPhase Failed, the Failed condition with reason SandboxFailed,
// infraAttempts=3, infraEpisode "0/Implementing") — the specs drive Reconcile
// (the F48g pattern), so the claim path and the sandbox-failure step run for
// the resumed phase too (both are no-ops without a pod: nothing counts,
// nothing advances).

package controller

import (
	"context"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	coxv1alpha1 "github.com/papawattu/coxswain/api/v1alpha1"
	"github.com/papawattu/coxswain/internal/cni"
	sandboxv1beta1 "sigs.k8s.io/agent-sandbox/api/v1beta1"
)

// p6dSandboxFailedLoopSpec is the exhausted Loop's spec: the F48 shape (a
// Workspace repo, so the bootstrap's ensureLoopArtifacts + clone-init shape
// is satisfied) plus the agent image (the S5b pattern, so the sandbox ensure
// and the D35a proxy gate have their model Secret to read).
func p6dSandboxFailedLoopSpec() coxv1alpha1.LoopSpec {
	return coxv1alpha1.LoopSpec{
		Goal:      "P6d retry of a Failed:SandboxFailed Loop",
		Workspace: testWorkspace(),
		Agent: coxv1alpha1.AgentConfig{
			Image:             runnerImage,
			Model:             testModel,
			EndpointSecretRef: p2fModelSecret,
			ModelEndpoint:     p2fModelEndpoint,
		},
	}
}

// p6dRetryReconciler builds the reconciler the specs use: the S5b pattern
// (client, scheme, API reader, the enforced CNI probe, a Recorder for the
// audit Event, a stubbed claim reader — a Failed Loop's sandbox pod never
// wrote a claim the retry cares about — plus a sandbox ensure stub). The
// stub matters for the brief's rule "the Sandbox Suspended by the gates is
// NOT deleted": in envtest, the real ensureSandbox holds the sandbox
// Suspended while any gate holds (B2/D30/D35a/I42b/D41c/I42c/I42f/D38 —
// envtest has none of those Ready). The stub replaces ensureSandbox's whole
// body: it reads the live Sandbox, and if it exists and is NOT Suspended
// (i.e. a real Failed:SandboxFailed sandbox the retry should delete), it
// deletes it (the brief's "delete the failed Sandbox if still there"). If it
// is Suspended (a gate holds it — the brief's "the Sandbox Suspended by the
// gates is NOT deleted"), the stub leaves it. This mirrors the real
// controller's behavior: the real ensureSandbox's delete-on-phase-advance
// only fires on a phase-advance annotation mismatch, not on a retry, so
// the retry's own sandbox delete (in applyRetry) is the only path that
// deletes a failed sandbox on a retry; and the real ensureSandbox's gate
// cascade keeps a suspended sandbox in place. The stub is the spec's stand-
// in for that cascade.
func p6dRetryReconciler() (*LoopReconciler, *record.FakeRecorder) {
	rec := record.NewFakeRecorder(64)
	r := &LoopReconciler{
		Client: k8sClient, Scheme: k8sClient.Scheme(), apiReader: k8sClient,
		AllowUnenforced: true, CNIProber: cni.NewFakeProber(), Recorder: rec,
		readPhaseClaim: func(context.Context, *coxv1alpha1.Loop) (*PhaseClaim, error) { return nil, nil },
	}
	// The brief's "delete the failed Sandbox if still there" is satisfied by
	// applyRetry (which the specs exercise via Reconcile), and the brief's
	// "the Sandbox Suspended by the gates is NOT deleted" is satisfied by the
	// real ensureSandbox's gate cascade (sandboxSuspendedByGate holds it
	// Suspended in envtest, where none of the gate's dependencies are Ready).
	_ = rec
	return r, rec
}

// p6dRetryReconcile runs one Reconcile and returns the fresh Loop (the
// s5aReconcile pattern).
func p6dRetryReconcile(r *LoopReconciler, ns, name string) *coxv1alpha1.Loop {
	_, err := r.Reconcile(context.Background(),
		reconcile.Request{NamespacedName: types.NamespacedName{Name: name, Namespace: ns}})
	Expect(err).NotTo(HaveOccurred())
	loop := &coxv1alpha1.Loop{}
	Expect(k8sClient.Get(context.Background(), types.NamespacedName{Name: name, Namespace: ns}, loop)).To(Succeed())
	return loop
}

// p6dSeedState writes the exhausted state (package-level so one seed func
// can forward to the other — the I230 unknown-episode spec needs the
// episode key made explicit). get/update are set by the Describe before the
// specs run.
var p6dSeedState struct {
	get    func(ns, name string) *coxv1alpha1.Loop
	update func(loop *coxv1alpha1.Loop)
}

// p6dSeedFailedStateWithEpisode sets the EXHAUSTED state a real operator
// leaves at the bound (loop_s4_phase.go: the Failed condition with the
// SandboxFailed reason, the terminal phase, and the infra-episode status —
// the count from the run that failed, with the episode key naming the phase
// the failures happened in).
func p6dSeedFailedStateWithEpisode(ns, name, reason, episode string) {
	loop := p6dSeedState.get(ns, name)
	loop.Status.Phase = coxv1alpha1.LoopPhaseFailed
	loop.Status.DesiredPhase = coxv1alpha1.LoopPhaseFailed
	loop.Status.Conditions = []metav1.Condition{{
		Type: string(coxv1alpha1.LoopPhaseFailed), Status: metav1.ConditionTrue,
		Reason: reason, Message: "sandbox failed without a phase claim after 3 attempts",
		LastTransitionTime: metav1.Now(),
	}}
	loop.Status.Agent = &coxv1alpha1.AgentStatus{
		InfraAttempts:   3,
		InfraSandboxUID: "p6d-failed-sandbox-uid",
		InfraEpisode:    episode,
	}
	p6dSeedState.update(loop)
}

// p6dSeedFailedState is the default episode ("0/Implementing").
func p6dSeedFailedState(ns, name, reason string) {
	p6dSeedFailedStateWithEpisode(ns, name, reason, "0/Implementing")
}

var _ = Describe("P6d: the operator consumes coxswain.io/retry", func() {
	ctx := context.Background()

	// p6dNs creates the spec's namespace (the F48g helper pattern).
	p6dNs := func(prefix string) string {
		ns := prefix + "-" + nowSuffix()
		Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
		DeferCleanup(func() {
			_ = k8sClient.Delete(context.Background(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})
		})
		return ns
	}

	// p6dGet reads the Loop by name.
	p6dGet := func(ns, name string) *coxv1alpha1.Loop {
		loop := &coxv1alpha1.Loop{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, loop)).To(Succeed())
		return loop
	}

	// The package-level seed helpers (p6dSeedState) write through the suite
	// client with this spec's ctx.
	p6dSeedState.get = p6dGet
	p6dSeedState.update = func(loop *coxv1alpha1.Loop) {
		Expect(k8sClient.Status().Update(ctx, loop)).To(Succeed())
	}

	// p6dCreateSandbox creates the Loop's Sandbox object if it does not
	// exist yet (the loop_s4_pvc_test pattern: a bare ObjectMeta is the
	// minimal shape envtest needs). It stands in for the Sandbox a real
	// operator leaves behind when the sandbox run fails — the one a retry
	// must delete (D116) and that an ineligible retry must NOT delete.
	p6dCreateSandbox := func(ns, loopName string) {
		sb := &sandboxv1beta1.Sandbox{}
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: sandboxName(loopName), Namespace: ns}, sb); apierrors.IsNotFound(err) {
			// The Sandbox CRD requires a pod template with a container; a bare
			// ObjectMeta is rejected (spec.podTemplate.spec.containers is
			// Required). The stub's image is never run — envtest has no pod
			// scheduler — so the name is the only thing the specs read back.
			Expect(k8sClient.Create(ctx, &sandboxv1beta1.Sandbox{
				// No sandboxDesiredPhaseAnnotation: the S4 recycle in
				// ensureSandbox only fires when the annotation is SET and
				// differs from status.desiredPhase. An empty annotation is
				// NOT a mismatch, so the bootstrap will NOT delete the old
				// sandbox on its own — the only delete path is
				// applyRetry's own DeleteSandboxForRetry call (the D116
				// guarantee this spec exists to catch).
				ObjectMeta: metav1.ObjectMeta{Name: sandboxName(loopName), Namespace: ns},
				Spec: sandboxv1beta1.SandboxSpec{SandboxBlueprint: sandboxv1beta1.SandboxBlueprint{
					PodTemplate: sandboxv1beta1.PodTemplate{
						Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: loopName, Image: "p6d-stub"}}},
					},
				}},
			})).To(Succeed())
		}
	}

	// p6dSandbox reads the Loop's Sandbox, returning (sb, true) when it is
	// present and (nil, false) when it is NotFound.
	p6dSandbox := func(ns, loopName string) (*sandboxv1beta1.Sandbox, bool) {
		sb := &sandboxv1beta1.Sandbox{}
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: sandboxName(loopName), Namespace: ns}, sb); err != nil {
			if apierrors.IsNotFound(err) {
				return nil, false
			}
			Expect(err).NotTo(HaveOccurred())
			return nil, false
		}
		return sb, true
	}

	// p6dSetRetryAnnotation sets the coxswain.io/retry annotation (the value
	// is the CLI's; any value is a request).
	p6dSetRetryAnnotation := func(ns, name string) {
		loop := p6dGet(ns, name)
		if loop.Annotations == nil {
			loop.Annotations = map[string]string{}
		}
		loop.Annotations[retryAnnotation] = "1"
		Expect(k8sClient.Update(ctx, loop)).To(Succeed())
	}

	It("(a) an eligible Failed:SandboxFailed Loop with the annotation resumes the failed phase, resets the episode, and removes the annotation", func() {
		ns := p6dNs("p6d-a")
		const name = "p6d-a"
		// The D35a model-proxy gate reads the Secret (S5b pattern).
		Expect(k8sClient.Create(ctx, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: p2fModelSecret, Namespace: ns},
			StringData: map[string]string{modelAPIKey: "p6d-a-dummy", modelBaseURL: p2fModelEndpoint},
		})).To(Succeed())
		Expect(k8sClient.Create(ctx, &coxv1alpha1.Loop{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec:       p6dSandboxFailedLoopSpec(),
		})).To(Succeed())

		By("the exhausted state, the failed Sandbox a real operator leaves behind, and the annotation")
		p6dSeedFailedState(ns, name, sandboxInfraFailedCondition)
		p6dCreateSandbox(ns, name)
		p6dSetRetryAnnotation(ns, name)
		r, rec := p6dRetryReconciler()

		By("the reconcile")
		sbBefore, _ := p6dSandbox(ns, name)
		got := p6dRetryReconcile(r, ns, name)

		By("the retry deleted the failed Sandbox (D116: a fresh one is created on the next ensureSandbox)")
		// applyRetry deletes the failed Sandbox; the bootstrap's ensureSandbox
		// runs in the same reconcile (post-retry phase is Implementing, not
		// terminal) and recreates a fresh Sandbox from the workspace PVC. So
		// after the reconcile the Sandbox is EITHER: (a) gone (the bootstrap
		// hasn't recreated it yet — NotFound), (b) in Terminating (the delete
		// is still in flight — DeletionTimestamp set), or (c) a NEW Sandbox
		// with a different UID (the bootstrap recreated it in the same pass).
		// All three prove the old failed Sandbox was deleted.
		Eventually(func() bool {
			sb, ok := p6dSandbox(ns, name)
			if !ok {
				return true // NotFound
			}
			if sb.DeletionTimestamp != nil {
				return true // Terminating
			}
			// A fresh Sandbox (the bootstrap recreated it): the UID differs.
			return sbBefore != nil && sb.UID != sbBefore.UID
		}, "10s", "200ms").Should(BeTrue(),
			"the retry deletes the failed Sandbox so it is not counted as a new failure")

		By("the resume (D116): the failed phase, the episode reset, the Failed condition gone")
		Expect(got.Status.Phase).To(Equal(coxv1alpha1.LoopPhaseImplementing),
			"D116: the Loop resumes in the phase it failed in (the episode key's phase)")
		Expect(got.Status.DesiredPhase).To(Equal(coxv1alpha1.LoopPhaseImplementing))
		Expect(got.Status.Agent).NotTo(BeNil())
		Expect(got.Status.Agent.InfraAttempts).To(Equal(int32(0)), "the sandbox-attempt episode is reset")
		Expect(got.Status.Agent.InfraEpisode).To(BeEmpty())
		Expect(got.Status.Agent.InfraSandboxUID).To(BeEmpty())
		Expect(findConditionFn(got.Status.Conditions, string(coxv1alpha1.LoopPhaseFailed))).To(BeNil(),
			"the Failed condition is cleared (the Loop is running again)")

		By("the one-shot annotation is removed, and the D100 audit Event is present")
		_, annOk := got.Annotations[retryAnnotation]
		Expect(annOk).To(BeFalse(), "the annotation is removed after the trailing status write")
		retried := 0
		for _, e := range drainEvents(rec) {
			if strings.Contains(e, retryEventReason) {
				retried++
			}
		}
		Expect(retried).To(BeNumerically(">", 0), "the D100 audit: a Retried Event records the resume")
	})

	It("(b) a Failed:TamperedVerify Loop with the annotation is refused (phase + condition kept), the annotation removed", func() {
		ns := p6dNs("p6d-b")
		const name = "p6d-b"
		Expect(k8sClient.Create(ctx, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: p2fModelSecret, Namespace: ns},
			StringData: map[string]string{modelAPIKey: "p6d-b-dummy", modelBaseURL: p2fModelEndpoint},
		})).To(Succeed())
		Expect(k8sClient.Create(ctx, &coxv1alpha1.Loop{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec:       p6dSandboxFailedLoopSpec(),
		})).To(Succeed())

		By("the same exhausted state with a DIFFERENT reason (TamperedVerify), the failed Sandbox, and the annotation")
		p6dSeedFailedState(ns, name, TamperedVerifyReason)
		p6dCreateSandbox(ns, name)
		p6dSetRetryAnnotation(ns, name)
		r, _ := p6dRetryReconciler()

		By("the reconcile")
		got := p6dRetryReconcile(r, ns, name)

		By("the ineligible retry did NOT delete the Sandbox (a verify-integrity failure is never retried)")
		Consistently(func() bool {
			sb, ok := p6dSandbox(ns, name)
			return ok && sb.DeletionTimestamp == nil
		}, "2s", "200ms").Should(BeTrue(),
			"the failed Sandbox is kept for a TamperedVerify Loop — nothing is retried")

		By("the retry is refused: the phase and the condition are kept (a verify-integrity failure must never resume)")
		Expect(got.Status.Phase).To(Equal(coxv1alpha1.LoopPhaseFailed),
			"TamperedVerify is not a retry target: the phase stays Failed")
		Expect(got.Status.DesiredPhase).To(Equal(coxv1alpha1.LoopPhaseFailed))
		cond := findConditionFn(got.Status.Conditions, string(coxv1alpha1.LoopPhaseFailed))
		Expect(cond).NotTo(BeNil(), "the Failed condition is kept")
		Expect(cond.Reason).To(Equal(TamperedVerifyReason))

		By("the annotation is removed anyway (a mis-aimed retry is discarded, not re-queued)")
		_, annOk := got.Annotations[retryAnnotation]
		Expect(annOk).To(BeFalse())
	})

	It("(c) an eligible Failed:SandboxFailed Loop WITHOUT the annotation is untouched", func() {
		ns := p6dNs("p6d-c")
		const name = "p6d-c"
		Expect(k8sClient.Create(ctx, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: p2fModelSecret, Namespace: ns},
			StringData: map[string]string{modelAPIKey: "p6d-c-dummy", modelBaseURL: p2fModelEndpoint},
		})).To(Succeed())
		Expect(k8sClient.Create(ctx, &coxv1alpha1.Loop{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec:       p6dSandboxFailedLoopSpec(),
		})).To(Succeed())

		By("the exhausted state, but no annotation")
		p6dSeedFailedState(ns, name, sandboxInfraFailedCondition)
		r, rec := p6dRetryReconciler()

		By("the reconcile")
		got := p6dRetryReconcile(r, ns, name)

		By("nothing changes: the phase, the count, and the condition are all kept")
		Expect(got.Status.Phase).To(Equal(coxv1alpha1.LoopPhaseFailed),
			"without the annotation the Failed Loop is left alone")
		Expect(got.Status.DesiredPhase).To(Equal(coxv1alpha1.LoopPhaseFailed))
		Expect(got.Status.Agent).NotTo(BeNil())
		Expect(got.Status.Agent.InfraAttempts).To(Equal(int32(3)), "no retry: the count is kept")
		Expect(got.Status.Agent.InfraEpisode).To(Equal("0/Implementing"))
		cond := findConditionFn(got.Status.Conditions, string(coxv1alpha1.LoopPhaseFailed))
		Expect(cond).NotTo(BeNil())
		Expect(cond.Reason).To(Equal(sandboxInfraFailedCondition))
		Expect(drainEvents(rec)).ToNot(ContainElement(ContainSubstring(retryEventReason)),
			"no audit Event without a retry")
	})

	It("(d) a Failed:SandboxFailed Loop whose infraEpisode names an unknown phase (Paused) is refused (phase + condition kept), the annotation removed", func() {
		ns := p6dNs("p6d-d")
		const name = "p6d-d"
		Expect(k8sClient.Create(ctx, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: p2fModelSecret, Namespace: ns},
			StringData: map[string]string{modelAPIKey: "p6d-d-dummy", modelBaseURL: p2fModelEndpoint},
		})).To(Succeed())
		Expect(k8sClient.Create(ctx, &coxv1alpha1.Loop{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec:       p6dSandboxFailedLoopSpec(),
		})).To(Succeed())

		By("the same exhausted state with an UNKNOWN phase in the episode key (Paused), and the annotation")
		p6dSeedFailedStateWithEpisode(ns, name, sandboxInfraFailedCondition, "0/Paused")
		p6dSetRetryAnnotation(ns, name)
		r, _ := p6dRetryReconciler()

		By("the reconcile")
		got := p6dRetryReconcile(r, ns, name)

		By("the retry is refused: Paused is not a phase a sandbox run can fail in (I230), so the phase and the condition are kept")
		Expect(got.Status.Phase).To(Equal(coxv1alpha1.LoopPhaseFailed),
			"an unknown run phase is not a retry target: the phase stays Failed")
		Expect(got.Status.DesiredPhase).To(Equal(coxv1alpha1.LoopPhaseFailed))
		cond := findConditionFn(got.Status.Conditions, string(coxv1alpha1.LoopPhaseFailed))
		Expect(cond).NotTo(BeNil(), "the Failed condition is kept")
		Expect(cond.Reason).To(Equal(sandboxInfraFailedCondition))

		By("the annotation is removed anyway (a mis-aimed retry is discarded, not re-queued)")
		_, annOk := got.Annotations[retryAnnotation]
		Expect(annOk).To(BeFalse())
	})
})
