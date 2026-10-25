// Copyright 2026.
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
	"errors"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	coxv1alpha1 "github.com/papawattu/coxswain/api/v1alpha1"
	"github.com/papawattu/coxswain/internal/cni"
)

// F84 (P2-2): the D35a hold (the sandbox is Suspended until the model proxy pod
// is Ready) used to be SILENT, and the budget read now treats a starting proxy
// as "no reading yet", so a proxy that never comes up showed nothing at all.
// The hold carries a condition and one Warning Event.
var _ = Describe("F84: the model proxy hold is surfaced", func() {
	ctx := context.Background()

	newF84 := func(ns, name string) (*LoopReconciler, *coxv1alpha1.Loop, *record.FakeRecorder) {
		rec := record.NewFakeRecorder(32)
		r := &LoopReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), apiReader: k8sClient,
			AllowUnenforced: true, Recorder: rec, CNIProber: cni.NewFakeProber()}
		r.CNIProber.(*cni.FakeProber).SetResult(cni.CNIProbeResult{Reason: cni.ReasonCNIEnforced})
		Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
		DeferCleanup(func() {
			_ = k8sClient.Delete(context.Background(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})
		})
		loop := &coxv1alpha1.Loop{
			ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
			Spec: coxv1alpha1.LoopSpec{
				Goal:      "F84 model proxy hold",
				Workspace: testWorkspace(),
				Agent: coxv1alpha1.AgentConfig{
					Image:             runnerImage,
					Model:             testModel,
					EndpointSecretRef: f84ModelSecret,
					ModelEndpoint:     "model.example.com:443",
				},
			},
		}
		Expect(k8sClient.Create(ctx, loop)).To(Succeed())
		return r, loop, rec
	}

	newF84NoEndpoint := func(ns, name string) (*LoopReconciler, *coxv1alpha1.Loop) {
		Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
		DeferCleanup(func() {
			_ = k8sClient.Delete(context.Background(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})
		})
		loop := &coxv1alpha1.Loop{
			ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
			Spec: coxv1alpha1.LoopSpec{
				Goal:      "F84 no endpoint",
				Workspace: testWorkspace(),
				Agent:     coxv1alpha1.AgentConfig{Image: runnerImage, Model: testModel},
			},
		}
		Expect(k8sClient.Create(ctx, loop)).To(Succeed())
		r := &LoopReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), apiReader: k8sClient}
		return r, loop
	}

	It("F84: a proxy pod that never becomes Ready sets ModelProxyNotReady (once, with one Event) and clears when it is Ready", func() {
		ns := "f84-proxy-" + nowSuffix()
		r, loop, rec := newF84(ns, "f84hold")

		By("no proxy pod: the gate holds and says why")
		Expect(r.modelProxyHold(ctx, loop)).To(BeTrue(), "the D35a gate holds the sandbox Suspended")
		cond := apimeta.FindStatusCondition(loop.Status.Conditions, coxv1alpha1.ModelProxyNotReadyCondition)
		Expect(cond).NotTo(BeNil(), "F84: the hold must be visible")
		Expect(cond.Status).To(Equal(metav1.ConditionTrue))
		Expect(cond.Message).To(ContainSubstring(proxyPodName("f84hold")), "the message names the pod")
		Expect(strings.Join(drainEvents(rec), "\n")).To(ContainSubstring("ModelProxyNotReady"), "one Warning Event on the transition")

		By("a second pass with the pod still absent: no second Event (transition-gated) and a stable message")
		msg := cond.Message
		Expect(r.modelProxyHold(ctx, loop)).To(BeTrue())
		Expect(apimeta.FindStatusCondition(loop.Status.Conditions, coxv1alpha1.ModelProxyNotReadyCondition).Message).To(Equal(msg),
			"the message must be stable while the hold lasts (no status write per pass)")
		Expect(strings.Join(drainEvents(rec), "\n")).NotTo(ContainSubstring("ModelProxyNotReady"), "no second Event")

		By("the pod becomes Ready: the condition clears")
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: proxyPodName("f84hold")},
			Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "proxy", Image: "example.com/proxy:v1"}}},
		}
		Expect(k8sClient.Create(ctx, pod)).To(Succeed())
		owned := pod.DeepCopy()
		owned.OwnerReferences = []metav1.OwnerReference{{
			APIVersion: coxv1alpha1.GroupVersion.String(), Kind: "Loop", Name: loop.Name, UID: loop.UID,
			Controller: new(true), BlockOwnerDeletion: new(true),
		}}
		Expect(k8sClient.Update(ctx, owned)).To(Succeed())
		owned.Status.PodIP = "127.0.0.1"
		owned.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
		Expect(k8sClient.SubResource("status").Update(ctx, owned)).To(Succeed())

		Eventually(func() bool { return r.modelProxyHold(ctx, loop) }).WithTimeout(10*time.Second).WithPolling(100*time.Millisecond).Should(BeFalse(), "a Ready proxy pod releases the hold")
		cleared := apimeta.FindStatusCondition(loop.Status.Conditions, coxv1alpha1.ModelProxyNotReadyCondition)
		Expect(cleared).NotTo(BeNil())
		Expect(cleared.Status).To(Equal(metav1.ConditionFalse))
		Expect(cleared.Reason).To(Equal("Ready"))
	})

	It("F84: no model endpoint clears the hold condition", func() {
		ns := "f84-proxy-none-" + nowSuffix()
		r, loop := newF84NoEndpoint(ns, "f84noendpoint")
		setCondition(loop, coxv1alpha1.ModelProxyNotReadyCondition, metav1.ConditionTrue, "ModelProxyNotReady", "held")
		r.applyBudget(ctx, loop)
		cond := apimeta.FindStatusCondition(loop.Status.Conditions, coxv1alpha1.ModelProxyNotReadyCondition)
		Expect(cond).NotTo(BeNil())
		Expect(cond.Status).To(Equal(metav1.ConditionFalse))
		Expect(cond.Reason).To(Equal("NoEndpoint"))
	})

	// F84b/F84c: the condition must not outlive the hold it describes. A Loop that
	// reaches a terminal phase while the proxy pod is not Ready used to keep
	// ModelProxyNotReady=True forever (the R34 I120(b) class). F84c moved the
	// clear out of the D35a gate and into Reconcile, so these cases drive
	// Reconcile with the D30 gate HOLDING (no enforcer, no --allow-unenforced):
	// the clear must not depend on the earlier gates passing.
	DescribeTable("F84b/F84c: a terminal phase clears the model proxy hold through Reconcile, with D30 holding",
		func(phase coxv1alpha1.LoopPhase) {
			ns := "f84c-terminal-" + strings.ToLower(string(phase)) + "-" + nowSuffix()
			_, loop, _ := newF84(ns, "f84cterm")

			rec := record.NewFakeRecorder(32)
			r := &LoopReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), apiReader: k8sClient,
				AllowUnenforced: false, Recorder: rec, CNIProber: cni.NewFakeProber()}
			// The D30 gate HOLDS: no Enforcer is wired and AllowUnenforced is
			// false, so enforcementStatus reports not-enforced and
			// sandboxSuspendedByGate returns before the model proxy gate. (The
			// prober below feeds D38, which is the LAST check in the gate, after
			// the model proxy one — it is not what holds this fixture.)
			r.CNIProber.(*cni.FakeProber).SetResult(cni.CNIProbeResult{Reason: cni.ReasonCNIUnenforced})

			By("the Loop reaches " + string(phase) + " carrying a recorded hold")
			loop.Status.Phase = phase
			loop.Status.DesiredPhase = phase
			setCondition(loop, coxv1alpha1.ModelProxyNotReadyCondition, metav1.ConditionTrue, "ModelProxyNotReady",
				"the model proxy pod f84cterm-proxy is not Ready: it does not exist yet; the sandbox is held Suspended")
			Expect(k8sClient.Status().Update(ctx, loop)).To(Succeed())

			_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: loop.Name}})
			Expect(err).NotTo(HaveOccurred())

			got := &coxv1alpha1.Loop{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: loop.Name}, got)).To(Succeed())
			cond := apimeta.FindStatusCondition(got.Status.Conditions, coxv1alpha1.ModelProxyNotReadyCondition)
			Expect(cond).NotTo(BeNil(), "the condition is cleared, not removed")
			Expect(cond.Status).To(Equal(metav1.ConditionFalse))
			Expect(cond.Reason).To(Equal(modelProxyTerminalReason))
			Expect(cond.Message).To(ContainSubstring(string(phase)), "the message names the terminal phase")
			Expect(r.modelProxyHold(ctx, got)).To(BeFalse(), "a terminal Loop is never held for its proxy")

			By("a second reconcile at the terminal phase: the clear is stable (no rewrite, no Event)")
			_ = drainEvents(rec)
			_, err = r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: loop.Name}})
			Expect(err).NotTo(HaveOccurred())
			again := &coxv1alpha1.Loop{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: loop.Name}, again)).To(Succeed())
			cleared := apimeta.FindStatusCondition(again.Status.Conditions, coxv1alpha1.ModelProxyNotReadyCondition)
			Expect(cleared.Message).To(Equal(cond.Message))
			Expect(cleared.LastTransitionTime).To(Equal(cond.LastTransitionTime), "a cleared condition is not rewritten every pass")
			Expect(strings.Join(drainEvents(rec), "\n")).NotTo(ContainSubstring("ModelProxyNotReady"), "no Event for a clear")
		},
		Entry("Failed", coxv1alpha1.LoopPhaseFailed),
		Entry("Succeeded", coxv1alpha1.LoopPhaseSucceeded),
	)

	// F84c: the read-error branch has no condition-visible error text, and it is
	// distinguishable from NotFound — the `#265` review's `if true ||
	// apierrors.IsNotFound(err)` mutation passed because nothing pinned it.
	DescribeTable("F84c: the model proxy read-error phrase is stable and distinct",
		func(err error, wantWhy string, wantDetail bool) {
			notReady, why, detail := modelProxyNotReady(&corev1.Pod{}, &coxv1alpha1.Loop{}, err)
			Expect(notReady).To(BeTrue())
			Expect(why).To(Equal(wantWhy))
			if wantDetail {
				Expect(detail).NotTo(BeEmpty(), "the error text is kept for the Event/log")
			} else {
				Expect(detail).To(BeEmpty(), "no error text to carry")
			}
		},
		Entry("not found: no error to carry",
			apierrors.NewNotFound(corev1.Resource("pods"), "f84c-proxy"), "it does not exist yet", false),
		Entry("an apiserver error: a distinct, stable phrase plus the detail",
			apierrors.NewInternalError(errors.New("boom")), "it could not be read", true),
	)

	// F84c (P2, the review round): the clear must run before EVERY hold that can
	// return early, including validateStep — which PERSISTS status while it
	// holds, so a stale ModelProxyNotReady=True would be written on every pass,
	// indefinitely, for a terminal Loop whose referenced policy was deleted.
	It("F84c: a missing policy reference cannot keep the model proxy hold on a terminal Loop", func() {
		ns := "f84c-missing-policy-" + nowSuffix()
		Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
		DeferCleanup(func() {
			_ = k8sClient.Delete(context.Background(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})
		})
		loop := &coxv1alpha1.Loop{
			ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "f84cmissing"},
			Spec: coxv1alpha1.LoopSpec{
				Goal:       "F84c missing policy",
				Workspace:  testWorkspace(),
				PolicyRefs: []string{"a-policy-that-does-not-exist"},
				Agent: coxv1alpha1.AgentConfig{
					Image:             runnerImage,
					Model:             testModel,
					EndpointSecretRef: f84ModelSecret,
					ModelEndpoint:     "model.example.com:443",
				},
			},
		}
		Expect(k8sClient.Create(ctx, loop)).To(Succeed())
		loop.Status.Phase = coxv1alpha1.LoopPhaseFailed
		loop.Status.DesiredPhase = coxv1alpha1.LoopPhaseFailed
		setCondition(loop, coxv1alpha1.ModelProxyNotReadyCondition, metav1.ConditionTrue, "ModelProxyNotReady",
			"the model proxy pod f84cmissing-proxy is not Ready: it does not exist yet; the sandbox is held Suspended")
		Expect(k8sClient.Status().Update(ctx, loop)).To(Succeed())

		rec := record.NewFakeRecorder(32)
		r := &LoopReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), apiReader: k8sClient,
			AllowUnenforced: false, Recorder: rec, CNIProber: cni.NewFakeProber()}
		r.CNIProber.(*cni.FakeProber).SetResult(cni.CNIProbeResult{Reason: cni.ReasonCNIEnforced})

		// validateStep is what returns this pass (the policy is missing), and it
		// persists status while it holds — which is exactly why the clear has to
		// run before it.
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: loop.Name}})
		Expect(err).NotTo(HaveOccurred())

		got := &coxv1alpha1.Loop{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: loop.Name}, got)).To(Succeed())
		cond := apimeta.FindStatusCondition(got.Status.Conditions, coxv1alpha1.ModelProxyNotReadyCondition)
		Expect(cond).NotTo(BeNil(), "the condition is cleared, not removed")
		Expect(cond.Status).To(Equal(metav1.ConditionFalse),
			"a hold that no longer applies must not be persisted by the validateStep hold")
		Expect(cond.Reason).To(Equal(modelProxyTerminalReason))
	})

	// F84c (the review round's remaining P2): the DELETING path persists the
	// conditions as the durable record a deleted Loop leaves behind, so the
	// terminal clear has to run there too — the main Reconcile clear is never
	// reached for a Loop with a deletionTimestamp (Reconcile returns to
	// finalizeB2DeleteStep first). Without the clear in loop_checkpoint.go the
	// record of a Failed Loop deleted while the hold was recorded keeps saying
	// "held Suspended until it is Ready" for a hold that no longer exists.
	It("F84c: a deleting terminal Loop persists the cleared hold (the deleting path is the only clear it gets)", func() {
		ns := "f84c-deleting-" + nowSuffix()
		Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
		DeferCleanup(func() {
			_ = k8sClient.Delete(context.Background(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})
		})
		const name = "f84cdel"
		loop := &coxv1alpha1.Loop{
			ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
			Spec: coxv1alpha1.LoopSpec{
				Goal:      "F84c deleting terminal Loop",
				Workspace: testWorkspace(),
				Agent: coxv1alpha1.AgentConfig{
					Image:             runnerImage,
					Model:             testModel,
					EndpointSecretRef: f84ModelSecret,
					ModelEndpoint:     "model.example.com:443",
				},
			},
		}
		Expect(k8sClient.Create(ctx, loop)).To(Succeed())
		r := &LoopReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), apiReader: k8sClient}
		Expect(r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: name}})).Error().NotTo(HaveOccurred())

		// The durable record at the moment of the delete: a terminal Loop with
		// the hold recorded (the F84 condition as surfaceModelProxyNotReady
		// writes it).
		loop = &coxv1alpha1.Loop{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, loop)).To(Succeed())
		Expect(loop.Finalizers).To(ContainElement(loopCheckpointFinalizer))
		loop.Status.Phase = coxv1alpha1.LoopPhaseFailed
		loop.Status.DesiredPhase = coxv1alpha1.LoopPhaseFailed
		setCondition(loop, coxv1alpha1.ModelProxyNotReadyCondition, metav1.ConditionTrue, "ModelProxyNotReady",
			"the model proxy pod "+proxyPodName(name)+" is not Ready: it does not exist yet; the sandbox is held Suspended until it is")
		Expect(k8sClient.Status().Update(ctx, loop)).To(Succeed())

		// A foreign finalizer keeps the object alive past the operator's own
		// finalizer removal, so the status the deleting pass persists can be
		// read back (the apiserver deletes the object the moment the checkpoint
		// finalizer is gone, which is what the B1 specs assert). On a real
		// cluster any other controller's finalizer plays this role.
		loop = &coxv1alpha1.Loop{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, loop)).To(Succeed())
		loop.Finalizers = append(loop.Finalizers, "test.coxswain/hold")
		Expect(k8sClient.Update(ctx, loop)).To(Succeed())

		Expect(k8sClient.Delete(ctx, loop)).To(Succeed())
		deleting := &coxv1alpha1.Loop{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, deleting)).To(Succeed())
		Expect(deleting.DeletionTimestamp.IsZero()).To(BeFalse(), "the delete is registered before the pass")

		Expect(r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: name}})).Error().NotTo(HaveOccurred())

		got := &coxv1alpha1.Loop{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, got)).To(Succeed())
		cond := apimeta.FindStatusCondition(got.Status.Conditions, coxv1alpha1.ModelProxyNotReadyCondition)
		Expect(cond).NotTo(BeNil(), "the condition is cleared, not removed")
		Expect(cond.Status).To(Equal(metav1.ConditionFalse),
			"the deleting path persists the cleared hold (its status write is the Loop's durable record)")
		Expect(cond.Reason).To(Equal(modelProxyTerminalReason))
	})

	// F84c (P3, the pre-review's finding 4): the why/detail routing in
	// modelProxyHold had no end-to-end spec — the phrase table tests
	// modelProxyNotReady alone and the byte-identical spec tests setCondition
	// alone, so a transposition (or wiring the raw error back into `why`) would
	// have put the error text in the condition message again with nothing
	// failing.
	It("F84c: a read error reaches the Event, and never the condition message", func() {
		loop := &coxv1alpha1.Loop{
			ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "f84creaderr"},
			Spec: coxv1alpha1.LoopSpec{
				Agent: coxv1alpha1.AgentConfig{EndpointSecretRef: f84ModelSecret, ModelEndpoint: "model.example.com:443"},
			},
		}
		rec := record.NewFakeRecorder(8)
		c := fake.NewClientBuilder().WithScheme(k8sClient.Scheme()).WithInterceptorFuncs(interceptor.Funcs{
			Get: func(_ context.Context, _ client.WithWatch, _ client.ObjectKey, _ client.Object, _ ...client.GetOption) error {
				return apierrors.NewInternalError(errors.New("boom-read-failure"))
			},
		}).Build()
		r := &LoopReconciler{Client: c, Scheme: k8sClient.Scheme(), Recorder: rec}

		Expect(r.modelProxyHold(ctx, loop)).To(BeTrue(), "an unreadable proxy pod is the same fail-closed hold")
		cond := apimeta.FindStatusCondition(loop.Status.Conditions, coxv1alpha1.ModelProxyNotReadyCondition)
		Expect(cond).NotTo(BeNil())
		Expect(cond.Message).To(ContainSubstring("could not be read"), "the stable phrase")
		Expect(cond.Message).NotTo(ContainSubstring("boom-read-failure"),
			"the raw error belongs in the Event and the log, never in a setCondition message")

		events := strings.Join(drainEvents(rec), "\n")
		Expect(events).To(ContainSubstring("ModelProxyNotReady"))
		Expect(events).To(ContainSubstring("boom-read-failure"), "the one Event carries the detail")
	})

	It("F84c: the read-error condition message is byte-identical across two different read errors", func() {
		ns := "f84c-readerr-" + nowSuffix()
		r, loop, _ := newF84(ns, "f84creaderr")
		loop.Status.Phase = coxv1alpha1.LoopPhaseImplementing
		Expect(k8sClient.Status().Update(ctx, loop)).To(Succeed())

		// Two different errors, one condition message: an error string must never
		// reach setCondition (it compares message text and would re-stamp every
		// pass).
		r.surfaceModelProxyNotReady(loop, "it could not be read", truncate(300, apierrors.NewInternalError(errors.New("boom")).Error()))
		first := apimeta.FindStatusCondition(loop.Status.Conditions, coxv1alpha1.ModelProxyNotReadyCondition)
		Expect(first).NotTo(BeNil())
		Expect(first.Message).NotTo(ContainSubstring("boom"), "the error text belongs in the Event, not the condition")

		r.surfaceModelProxyNotReady(loop, "it could not be read", truncate(300, apierrors.NewInternalError(errors.New("a different failure")).Error()))
		second := apimeta.FindStatusCondition(loop.Status.Conditions, coxv1alpha1.ModelProxyNotReadyCondition)
		Expect(second.Message).To(Equal(first.Message), "the message must not change with the error")
		Expect(second.LastTransitionTime).To(Equal(first.LastTransitionTime), "so the condition is not re-stamped")
	})
})

// f84ModelSecret is the endpoint Secret name the F84 fixture points at.
const f84ModelSecret = "f84-model-creds"

// ptrTo is the generic helper for the boolean owner-reference fields.
