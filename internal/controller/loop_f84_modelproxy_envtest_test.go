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
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"

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

	// F84b: the condition must not outlive the hold it describes. A Loop that
	// reaches a terminal phase while the proxy pod is not Ready used to keep
	// ModelProxyNotReady=True forever (the R34 I120(b) class): the gate runs at
	// every phase, the proxy pod stays absent (no pod is created in this spec),
	// so only the terminal clear can remove it.
	DescribeTable("F84b: a terminal phase clears the model proxy hold",
		func(phase coxv1alpha1.LoopPhase) {
			ns := "f84b-terminal-" + strings.ToLower(string(phase)) + "-" + nowSuffix()
			r, loop, rec := newF84(ns, "f84bterm")

			By("the hold is recorded first (the proxy pod does not exist)")
			Expect(r.modelProxyHold(ctx, loop)).To(BeTrue())
			Expect(apimeta.FindStatusCondition(loop.Status.Conditions, coxv1alpha1.ModelProxyNotReadyCondition).Status).
				To(Equal(metav1.ConditionTrue))
			_ = drainEvents(rec)

			By("the Loop reaches " + string(phase) + ": the hold no longer applies and the condition says so")
			loop.Status.Phase = phase
			loop.Status.DesiredPhase = phase
			Expect(k8sClient.Status().Update(ctx, loop)).To(Succeed())
			Expect(r.modelProxyHold(ctx, loop)).To(BeFalse(), "a terminal Loop is not held for its proxy")
			cond := apimeta.FindStatusCondition(loop.Status.Conditions, coxv1alpha1.ModelProxyNotReadyCondition)
			Expect(cond).NotTo(BeNil(), "the condition is cleared, not removed")
			Expect(cond.Status).To(Equal(metav1.ConditionFalse))
			Expect(cond.Reason).To(Equal(modelProxyTerminalReason))
			Expect(cond.Message).To(ContainSubstring(string(phase)), "the message names the terminal phase")

			By("a second pass at the terminal phase: the clear is stable (no rewrite, no Event)")
			Expect(r.modelProxyHold(ctx, loop)).To(BeFalse())
			again := apimeta.FindStatusCondition(loop.Status.Conditions, coxv1alpha1.ModelProxyNotReadyCondition)
			Expect(again.Message).To(Equal(cond.Message))
			Expect(again.LastTransitionTime).To(Equal(cond.LastTransitionTime), "a cleared condition is not rewritten every pass")
			Expect(strings.Join(drainEvents(rec), "\n")).NotTo(ContainSubstring("ModelProxyNotReady"), "no Event for a clear")
		},
		Entry("Failed", coxv1alpha1.LoopPhaseFailed),
		Entry("Succeeded", coxv1alpha1.LoopPhaseSucceeded),
	)
})

// f84ModelSecret is the endpoint Secret name the F84 fixture points at.
const f84ModelSecret = "f84-model-creds"

// ptrTo is the generic helper for the boolean owner-reference fields.
