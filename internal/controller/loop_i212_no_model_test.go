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

// I212 (R47): a runner Loop with NO model endpoint at all (no
// spec.agent.endpointSecretRef, no spec.agent.modelEndpoint) was admitted and
// the sandbox was created anyway; the agent then crash-looped in Planning on
// `COX_MODEL_BASE_URL is empty; the runner requires the operator-set model
// proxy URL` (runner/cmd/main.go:108) with no condition on the Loop naming
// the cause. The v0.1.3 smoke run hit this applying the shipped sample as-is
// (the sample ships the model fields commented out). The fix holds such a
// Loop pre-sandbox with ModelConfigValid=False reason=NoModelEndpoint and no
// sandbox, no model-proxy, no egress-proxy objects. A non-runner agent image
// (spec.agent.image != --runner-image) is NOT held: an external agent may
// not need the operator's model proxy at all.
package controller

import (
	"context"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	sandboxv1beta1 "sigs.k8s.io/agent-sandbox/api/v1beta1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	coxv1alpha1 "github.com/papawattu/coxswain/api/v1alpha1"
)

var _ = Describe("I212: a runner Loop with no model endpoint is held (no sandbox)", func() {
	ctx := context.Background()

	const (
		i212Runner        = "example.test/coxswain-runner:test"
		i212Other         = "example.test/other:1"
		i212Secret        = "i212-model-cred"
		i212Endpoint      = "fake-model:8000"
		i212NoModelReason = "NoModelEndpoint"
	)

	i212Ns := func(prefix string) string {
		ns := prefix + "-" + nowSuffix()
		Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
		return ns
	}

	i212Loop := func(ns, name, agentImage string, withModel bool) *coxv1alpha1.Loop {
		loop := &coxv1alpha1.Loop{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec: coxv1alpha1.LoopSpec{
				Goal:      loopGoal,
				Workspace: testWorkspace(),
				Verify:    coxv1alpha1.VerifyConfig{AcceptanceChecks: []string{loopCheckCmd}},
			},
		}
		loop.Spec.Agent.Image = agentImage
		if withModel {
			loop.Spec.Agent.Model = testModel
			loop.Spec.Agent.EndpointSecretRef = i212Secret
			loop.Spec.Agent.ModelEndpoint = i212Endpoint
		}
		return loop
	}

	i212Reconciler := func() *LoopReconciler {
		return &LoopReconciler{
			CNIProber:       testCNIProber(),
			Client:          k8sClient,
			Scheme:          k8sClient.Scheme(),
			apiReader:       k8sClient,
			Recorder:        record.NewFakeRecorder(64),
			RunnerImage:     i212Runner,
			AllowUnenforced: true, // the D30 gate is not what this spec is about
		}
	}

	i212Reconcile := func(r *LoopReconciler, ns, name string) {
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: name}})
		Expect(err).NotTo(HaveOccurred())
	}

	i212Get := func(ns, name string) *coxv1alpha1.Loop {
		got := &coxv1alpha1.Loop{}
		Expect(k8sClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, got)).To(Succeed())
		return got
	}

	i212Condition := func(loop *coxv1alpha1.Loop) *metav1.Condition {
		for i := range loop.Status.Conditions {
			if loop.Status.Conditions[i].Type == "ModelConfigValid" {
				return &loop.Status.Conditions[i]
			}
		}
		return nil
	}

	// i212AssertNoModelObjects asserts the held Loop got no sandbox and no
	// model-proxy or egress-proxy objects (the gate returns before the
	// bootstrap that would create them).
	i212AssertNoModelObjects := func(ns, name string) {
		sbx := &sandboxv1beta1.Sandbox{}
		err := k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: sandboxName(name)}, sbx)
		Expect(err).To(HaveOccurred(), "a held runner Loop must not get a sandbox")
		proxyPod := &corev1.Pod{}
		err = k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: proxyPodName(name)}, proxyPod)
		Expect(err).To(HaveOccurred(), "a held runner Loop must not get a model-proxy pod")
		proxySvc := &corev1.Service{}
		err = k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: name + "-proxy"}, proxySvc)
		Expect(err).To(HaveOccurred(), "a held runner Loop must not get a model-proxy Service")
		egressPod := &corev1.Pod{}
		err = k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: egressProxyPodName(name)}, egressPod)
		Expect(err).To(HaveOccurred(), "a held runner Loop must not get an egress-proxy pod")
	}

	It("(a) a runner Loop with no model endpoint is held with ModelConfigValid=False NoModelEndpoint, and gets no sandbox or proxy objects", func() {
		ns := i212Ns("i212-nomodel")
		defer func() { _ = k8sClient.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}) }()

		r := i212Reconciler()
		name := "l1"
		Expect(k8sClient.Create(ctx, i212Loop(ns, name, "", false))).To(Succeed())
		i212Reconcile(r, ns, name)

		loop := i212Get(ns, name)
		cond := i212Condition(loop)
		Expect(cond).NotTo(BeNil(), "the Loop must say why it is held (I212: before, nothing named the model)")
		Expect(cond.Status).To(Equal(metav1.ConditionFalse))
		Expect(cond.Reason).To(Equal(i212NoModelReason))
		Expect(cond.Message).To(ContainSubstring("spec.agent.modelEndpoint"), "the message must name the field")
		Expect(cond.Message).To(ContainSubstring("runner"), "the message must say the runner needs a model")

		By("the reason is distinct from the D34 pair check (MissingModelEndpoint)")
		Expect(cond.Reason).ToNot(Equal("MissingModelEndpoint"))

		By("the Loop is held before the sandbox exists")
		i212AssertNoModelObjects(ns, name)
	})

	It("(b) the same Loop on a non-runner image is NOT held by this check", func() {
		ns := i212Ns("i212-pinned")
		defer func() { _ = k8sClient.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}) }()

		r := i212Reconciler()
		name := "l2"
		Expect(k8sClient.Create(ctx, i212Loop(ns, name, i212Other, false))).To(Succeed())
		i212Reconcile(r, ns, name)

		loop := i212Get(ns, name)
		cond := i212Condition(loop)
		if cond != nil {
			// A non-runner agent may not need the model proxy: this check must
			// not hold it. (Any other reason would be a different check; the
			// gate is spec-pinned on the reason.)
			Expect(cond.Reason).ToNot(Equal(i212NoModelReason),
				"a non-runner image must not be held by the runner model check")
		}
	})

	It("(c) a runner Loop WITH the model pair is not held by this check", func() {
		ns := i212Ns("i212-pair")
		defer func() { _ = k8sClient.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}) }()

		_ = k8sClient.Create(ctx, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: i212Secret, Namespace: ns},
		})
		r := i212Reconciler()
		name := "l3"
		Expect(k8sClient.Create(ctx, i212Loop(ns, name, "", true))).To(Succeed())
		i212Reconcile(r, ns, name)

		loop := i212Get(ns, name)
		cond := i212Condition(loop)
		if cond != nil {
			Expect(cond.Reason).ToNot(Equal(i212NoModelReason),
				"a runner Loop with the full model pair must not be held by the runner model check")
			Expect(cond.Status).ToNot(Equal(metav1.ConditionFalse),
				"a runner Loop with the full model pair must not carry ModelConfigValid=False")
		}
	})

	It("(d) deleting a held runner Loop with no model completes (the finalizer is not wedged)", func() {
		ns := i212Ns("i212-delete")
		defer func() { _ = k8sClient.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}) }()

		r := i212Reconciler()
		name := "l4"
		Expect(k8sClient.Create(ctx, i212Loop(ns, name, "", false))).To(Succeed())
		i212Reconcile(r, ns, name)

		loop := i212Get(ns, name)
		Expect(i212Condition(loop)).NotTo(BeNil(), "the Loop must be held before the delete")
		Expect(loop.Finalizers).ToNot(BeEmpty(), "the checkpoint finalizer must be present for the wedge to be testable")
		By("the held Loop got no sandbox, so the delete has no sandbox to wait on")
		i212AssertNoModelObjects(ns, name)

		Expect(k8sClient.Delete(ctx, loop)).To(Succeed())
		gone := false
		for range 20 {
			loopNow := &coxv1alpha1.Loop{}
			gerr := k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: ns}, loopNow)
			if gerr == nil {
				Expect(gerr).NotTo(HaveOccurred())
				if !loopNow.DeletionTimestamp.IsZero() {
					i212Reconcile(r, ns, name)
				}
				time.Sleep(50 * time.Millisecond)
				continue
			}
			gone = client.IgnoreNotFound(gerr) == nil
			break
		}
		Expect(gone).To(BeTrue(),
			"the deleting path must not wedge on the model hold (I212: the deleting path records, does not hold)")
	})
})
