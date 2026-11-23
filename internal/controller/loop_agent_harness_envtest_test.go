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

// D147 (docs/PLAN-AGENT-ADAPTERS.md): a Loop picks an agent adapter with
// spec.agent.harness, and the operator runs it only in the image it pins for
// that name (--agent-harness name=image). An unlisted name holds the Loop with
// no sandbox; listing it releases the Loop on the next reconcile.
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
	sandboxv1beta1 "sigs.k8s.io/agent-sandbox/api/v1beta1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	coxv1alpha1 "github.com/papawattu/coxswain/api/v1alpha1"
)

var _ = Describe("D147: spec.agent.harness runs an operator-pinned agent adapter", func() {
	ctx := context.Background()
	const piImage = "registry.example/agent-pi@sha256:0000000000000000000000000000000000000000000000000000000000000001"

	hNs := func(prefix string) string {
		ns := prefix + "-" + nowSuffix()
		Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
		Expect(k8sClient.Create(ctx, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "h-model-cred", Namespace: ns},
		})).To(Succeed())
		return ns
	}
	hLoop := func(ns, name, harness string) *coxv1alpha1.Loop {
		l := &coxv1alpha1.Loop{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec: coxv1alpha1.LoopSpec{
				Goal:      loopGoal,
				Workspace: testWorkspace(),
				Verify:    coxv1alpha1.VerifyConfig{AcceptanceChecks: []string{loopCheckCmd}},
			},
		}
		l.Spec.Agent.Harness = harness
		l.Spec.Agent.Model = testModel
		l.Spec.Agent.EndpointSecretRef = "h-model-cred"
		l.Spec.Agent.ModelEndpoint = "fake-model:8000"
		return l
	}
	hReconciler := func(recorder *record.FakeRecorder, harnesses map[string]string) *LoopReconciler {
		return &LoopReconciler{
			CNIProber:       testCNIProber(),
			Client:          k8sClient,
			Scheme:          k8sClient.Scheme(),
			apiReader:       k8sClient,
			Recorder:        recorder,
			RunnerImage:     "example.test/coxswain-runner:test",
			AgentHarnesses:  harnesses,
			AllowUnenforced: true, // the D30 gate is not what these specs are about
		}
	}
	hReconcile := func(r *LoopReconciler, ns, name string) {
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: name}})
		Expect(err).NotTo(HaveOccurred())
	}
	hGet := func(ns, name string) *coxv1alpha1.Loop {
		got := &coxv1alpha1.Loop{}
		Expect(k8sClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, got)).To(Succeed())
		return got
	}
	hCondition := func(loop *coxv1alpha1.Loop) *metav1.Condition {
		for i := range loop.Status.Conditions {
			if loop.Status.Conditions[i].Type == coxv1alpha1.RunnerAvailableCondition {
				return &loop.Status.Conditions[i]
			}
		}
		return nil
	}
	hSandbox := func(ns, name string) (*sandboxv1beta1.Sandbox, error) {
		sbx := &sandboxv1beta1.Sandbox{}
		err := k8sClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: name + "-sandbox"}, sbx)
		return sbx, err
	}
	hAgent := func(sbx *sandboxv1beta1.Sandbox) corev1.Container {
		for _, c := range sbx.Spec.PodTemplate.Spec.Containers {
			if c.Name == "agent" {
				return c
			}
		}
		Fail("the sandbox has no agent container")
		return corev1.Container{}
	}
	hEvents := func(recorder *record.FakeRecorder, reason string) []string {
		out := []string{}
		for {
			select {
			case e := <-recorder.Events:
				if strings.Contains(e, reason) {
					out = append(out, e)
				}
			default:
				return out
			}
		}
	}

	It("H1: a listed harness runs the adapter entrypoint in the operator's pinned image", func() {
		ns := hNs("h1")
		defer func() { _ = k8sClient.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}) }()
		recorder := record.NewFakeRecorder(64)
		r := hReconciler(recorder, map[string]string{"pi": piImage})

		Expect(k8sClient.Create(ctx, hLoop(ns, "l1", "pi"))).To(Succeed())
		hReconcile(r, ns, "l1")

		cond := hCondition(hGet(ns, "l1"))
		Expect(cond).NotTo(BeNil())
		Expect(cond.Status).To(Equal(metav1.ConditionTrue))
		Expect(cond.Reason).To(Equal("HarnessImageSet"))
		Expect(cond.Message).To(ContainSubstring(piImage))

		sbx, err := hSandbox(ns, "l1")
		Expect(err).NotTo(HaveOccurred())
		agent := hAgent(sbx)
		Expect(agent.Image).To(Equal(piImage), "the harness's pinned image, not --runner-image")
		Expect(agent.Command).NotTo(BeEmpty())
		Expect(agent.Command[0]).To(Equal("/usr/local/bin/runner"), "the adapter entrypoint, not the sleep stand-in")
		Expect(hEvents(recorder, HarnessUnknownEventReason)).To(BeEmpty())
	})

	It("H2: an unlisted harness holds the Loop with no sandbox, once; listing it releases the Loop", func() {
		ns := hNs("h2")
		defer func() { _ = k8sClient.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}) }()
		recorder := record.NewFakeRecorder(64)
		r := hReconciler(recorder, map[string]string{"pi": piImage})

		Expect(k8sClient.Create(ctx, hLoop(ns, "l2", "codex"))).To(Succeed())
		hReconcile(r, ns, "l2")

		cond := hCondition(hGet(ns, "l2"))
		Expect(cond).NotTo(BeNil(), "the Loop must say why it is held")
		Expect(cond.Status).To(Equal(metav1.ConditionFalse))
		Expect(cond.Reason).To(Equal(HarnessUnknownEventReason))
		Expect(cond.Message).To(ContainSubstring(`"codex" is not one of the operator's harnesses (pi)`))
		_, err := hSandbox(ns, "l2")
		Expect(apierrors.IsNotFound(err)).To(BeTrue(), "a held Loop gets no sandbox (no sleep-infinity stand-in): %v", err)

		By("one Warning Event per transition, not per reconcile")
		Expect(hEvents(recorder, HarnessUnknownEventReason)).To(HaveLen(1))
		hReconcile(r, ns, "l2")
		Expect(hEvents(recorder, HarnessUnknownEventReason)).To(BeEmpty())

		By("the same Loop is released when the operator lists the harness")
		r.AgentHarnesses["codex"] = "registry.example/agent-codex:pin"
		hReconcile(r, ns, "l2")
		cond = hCondition(hGet(ns, "l2"))
		Expect(cond.Status).To(Equal(metav1.ConditionTrue))
		Expect(cond.Reason).To(Equal("HarnessImageSet"))
		sbx, err := hSandbox(ns, "l2")
		Expect(err).NotTo(HaveOccurred())
		Expect(hAgent(sbx).Image).To(Equal("registry.example/agent-codex:pin"))
	})

	It("H3: harness and image together are rejected at admission", func() {
		ns := hNs("h3")
		defer func() { _ = k8sClient.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}) }()
		l := hLoop(ns, "l3", "pi")
		l.Spec.Agent.Image = "registry.example/anything:latest"
		err := k8sClient.Create(ctx, l)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("set at most one of harness and image"))
	})

	It("H4: a harness Loop with no model endpoint is held like a runner Loop (NoModelEndpoint)", func() {
		ns := hNs("h4")
		defer func() { _ = k8sClient.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}) }()
		r := hReconciler(record.NewFakeRecorder(64), map[string]string{"pi": piImage})
		l := hLoop(ns, "l4", "pi")
		l.Spec.Agent.EndpointSecretRef, l.Spec.Agent.ModelEndpoint = "", ""
		Expect(k8sClient.Create(ctx, l)).To(Succeed())
		hReconcile(r, ns, "l4")

		got := hGet(ns, "l4")
		var mc *metav1.Condition
		for i := range got.Status.Conditions {
			if got.Status.Conditions[i].Type == "ModelConfigValid" {
				mc = &got.Status.Conditions[i]
			}
		}
		Expect(mc).NotTo(BeNil())
		Expect(mc.Reason).To(Equal("NoModelEndpoint"))
		_, err := hSandbox(ns, "l4")
		Expect(apierrors.IsNotFound(err)).To(BeTrue())
	})
})
