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

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	coxv1alpha1 "github.com/papawattu/coxswain/api/v1alpha1"
	"github.com/papawattu/coxswain/internal/engine"
	"github.com/papawattu/coxswain/internal/policy"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	sandboxv1beta1 "sigs.k8s.io/agent-sandbox/api/v1beta1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

// fakeEnforcer is a test Enforcer whose Enforcing result is configurable, so the
// D30 fail-closed gate can be driven without a real eBPF engine.
type fakeEnforcer struct {
	enforcing bool
	reason    string
}

func (f *fakeEnforcer) Apply(_ context.Context, _ *coxv1alpha1.Loop, _ policy.EffectivePolicy) error {
	return nil
}
func (f *fakeEnforcer) Enforcing(_ context.Context, _ *coxv1alpha1.Loop) (bool, string) {
	return f.enforcing, f.reason
}

var _ = Describe("D30 fail-closed enforcement gate (C6b)", func() {
	var (
		ctx context.Context
		r   *LoopReconciler
	)

	buildLoop := func(ns string, policyRefs ...string) *coxv1alpha1.Loop {
		return &coxv1alpha1.Loop{
			ObjectMeta: metav1.ObjectMeta{Name: "l1", Namespace: ns},
			Spec: coxv1alpha1.LoopSpec{
				Goal:       "g",
				Workspace:  coxv1alpha1.Workspace{Repo: "https://example.com/x.git"},
				Verify:     coxv1alpha1.VerifyConfig{AcceptanceChecks: []string{"go test ./..."}},
				PolicyRefs: policyRefs,
			},
		}
	}
	createPolicy := func(ns string) {
		Expect(k8sClient.Create(ctx, &coxv1alpha1.AgentPolicy{
			ObjectMeta: metav1.ObjectMeta{Name: "p1", Namespace: ns},
			Spec:       coxv1alpha1.AgentPolicySpec{Exec: []string{agentPolicyExecGit}},
		})).To(Succeed())
	}
	getLoop := func(ns, name string) *coxv1alpha1.Loop {
		got := &coxv1alpha1.Loop{}
		Expect(k8sClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, got)).To(Succeed())
		return got
	}
	getSandboxMode := func(ns, name string) sandboxv1beta1.SandboxOperatingMode {
		sbx := &sandboxv1beta1.Sandbox{}
		Expect(k8sClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, sbx)).To(Succeed())
		return sbx.Spec.OperatingMode
	}

	It("holds the sandbox Suspended + PolicyEnforced=False when a declared policy is not enforced", func() {
		ctx = context.Background()
		r = &LoopReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(),
			Enforcer: &fakeEnforcer{enforcing: false, reason: engine.ReasonNodeNotEnforcing}}

		ns := "c6b-notenforced"
		Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
		defer func() { _ = k8sClient.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}) }()
		createPolicy(ns)
		Expect(k8sClient.Create(ctx, buildLoop(ns, "p1"))).To(Succeed())

		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: "l1"}})
		Expect(err).NotTo(HaveOccurred()) // fail-closed waits, never errors / never fails the Loop

		Eventually(func() bool {
			return getSandboxMode(ns, "l1-sandbox") == sandboxv1beta1.SandboxOperatingModeSuspended
		}, "10s").Should(BeTrue(), "the sandbox must be held Suspended while the engine is not enforcing (D30)")

		cond := policyEnforcedCondition(getLoop(ns, "l1").Status.Conditions)
		Expect(cond).NotTo(BeNil())
		Expect(cond.Status).To(Equal(metav1.ConditionFalse))
		Expect(cond.Reason).To(Equal("NodeNotEnforcing"))
		// The Loop must NOT be Failed (D30: the Loop waits, never fails on enforcement).
		Expect(getLoop(ns, "l1").Status.Phase).NotTo(Equal(coxv1alpha1.LoopPhaseFailed))
	})

	It("runs the sandbox + PolicyEnforced=True when the engine is enforcing the declared policy", func() {
		ctx = context.Background()
		r = &LoopReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), Enforcer: &fakeEnforcer{enforcing: true}}

		ns := "c6b-enforced"
		Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
		defer func() { _ = k8sClient.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}) }()
		createPolicy(ns)
		Expect(k8sClient.Create(ctx, buildLoop(ns, "p1"))).To(Succeed())

		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: "l1"}})
		Expect(err).NotTo(HaveOccurred())

		Eventually(func() bool {
			return getSandboxMode(ns, "l1-sandbox") == sandboxv1beta1.SandboxOperatingModeRunning
		}, "10s").Should(BeTrue(), "the sandbox must run when the engine is enforcing (D30)")

		cond := policyEnforcedCondition(getLoop(ns, "l1").Status.Conditions)
		Expect(cond).NotTo(BeNil())
		Expect(cond.Status).To(Equal(metav1.ConditionTrue))
	})

	It("gates a no-policyRefs Loop too (P1 #1: the gate applies to EVERY Loop)", func() {
		ctx = context.Background()
		r = &LoopReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()} // no Enforcer

		ns := "c6b-nogate"
		Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
		defer func() { _ = k8sClient.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}) }()
		Expect(k8sClient.Create(ctx, buildLoop(ns))).To(Succeed())

		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: "l1"}})
		Expect(err).NotTo(HaveOccurred())

		// P1 #1: a no-policyRefs Loop runs the platform minimum, still gated on the
		// engine. With no Enforcer (engine not installed) it is held Suspended +
		// PolicyEnforced=False (fail-closed), never Running unenforced.
		Eventually(func() bool {
			return getSandboxMode(ns, "l1-sandbox") == sandboxv1beta1.SandboxOperatingModeSuspended
		}, "10s").Should(BeTrue(), "a no-policyRefs Loop with no engine must be held Suspended (P1 #1)")
		loop := getLoop(ns, "l1")
		c := policyEnforcedCondition(loop.Status.Conditions)
		Expect(c).NotTo(BeNil(), "the PolicyEnforced condition must exist")
		Expect(string(c.Status)).To(Equal("False"), "no engine -> PolicyEnforced=False (fail-closed)")
		Expect(c.Reason).To(Equal("EngineUnavailable"), "no Enforcer -> reason EngineUnavailable")
		Expect(string(loop.Status.Phase)).NotTo(Equal(string(coxv1alpha1.LoopPhaseFailed)), "the Loop is never failed by the gate")
	})
	It("applies the effective policy via Enforcer.Apply and labels the pod (P1 #2)", func() {
		ctx = context.Background()
		rec := &recordingEnforcer{}
		r = &LoopReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), Enforcer: rec}

		ns := "c6b-apply"
		Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
		defer func() { _ = k8sClient.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}) }()
		createPolicy(ns)
		Expect(k8sClient.Create(ctx, buildLoop(ns, "p1"))).To(Succeed())

		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: "l1"}})
		Expect(err).NotTo(HaveOccurred())

		// P1 #2: the operator must call Enforcer.Apply with the effective policy.
		Expect(rec.applied).NotTo(BeEmpty(), "the operator must call Enforcer.Apply with the effective policy")

		// P1 #2: the sandbox pod template must carry the coxswain.io/loop label the
		// KubeArmorPolicy selector targets.
		sbx := &sandboxv1beta1.Sandbox{}
		Expect(k8sClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: "l1-sandbox"}, sbx)).To(Succeed())
		Expect(sbx.Spec.PodTemplate.ObjectMeta.Labels).To(HaveKeyWithValue("coxswain.io/loop", "l1"),
			"the sandbox pod template must carry the coxswain.io/loop label the KubeArmorPolicy selects on")
	})

	It("owner-refs the KubeArmorPolicy to the Loop (P2)", func() {
		ctx = context.Background()
		r = &LoopReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), Enforcer: &engine.KubeArmorEnforcer{Client: k8sClient}, AllowUnenforced: true}
		ns := "c6b-ownerref"
		Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
		defer func() { _ = k8sClient.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}) }()
		Expect(k8sClient.Create(ctx, buildLoop(ns))).To(Succeed())

		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: "l1"}})
		Expect(err).NotTo(HaveOccurred())

		// The real KubeArmorEnforcer created a KubeArmorPolicy owner-ref'd to the Loop.
		kap := &unstructured.Unstructured{}
		kap.SetGroupVersionKind(engine.KubeArmorGVK)
		Expect(k8sClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: "coxswain-l1"}, kap)).To(Succeed(),
			"a KubeArmorPolicy must exist (the real enforcer created it)")
		ownerRefs := kap.GetOwnerReferences()
		Expect(ownerRefs).NotTo(BeEmpty(), "the KubeArmorPolicy must be owner-ref'd to the Loop")
		Expect(ownerRefs[0].Name).To(Equal("l1"), "the owner ref must point at the Loop")
		Expect(ownerRefs[0].Kind).To(Equal("Loop"), "the owner ref kind must be Loop")
		Expect(ownerRefs[0].UID).NotTo(BeEmpty(), "the owner ref must carry the Loop's UID")
	})

	It("runs with PolicyEnforced=False (EnforcementDisabled) when AllowUnenforced is set (P1 merge)", func() {
		ctx = context.Background()
		r = &LoopReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), AllowUnenforced: true}
		ns := "c6b-unenforced"
		Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
		defer func() { _ = k8sClient.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}) }()
		Expect(k8sClient.Create(ctx, buildLoop(ns))).To(Succeed())

		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: "l1"}})
		Expect(err).NotTo(HaveOccurred())

		// The Loop runs (not held Suspended) because AllowUnenforced is set,
		// but the condition records EnforcementDisabled (NOT enforced).
		Eventually(func() bool {
			return getSandboxMode(ns, "l1-sandbox") == sandboxv1beta1.SandboxOperatingModeRunning
		}, "10s").Should(BeTrue(), "AllowUnenforced lets the Loop run")
		loop := getLoop(ns, "l1")
		c := policyEnforcedCondition(loop.Status.Conditions)
		Expect(c).NotTo(BeNil(), "the PolicyEnforced condition must exist")
		Expect(string(c.Status)).To(Equal("False"), "AllowUnenforced runs the Loop but it is NOT enforced")
		Expect(c.Reason).To(Equal("EnforcementDisabled"), "reason must be EnforcementDisabled (loudly visible)")
	})

})

// policyEnforcedCondition finds the PolicyEnforced condition.
func policyEnforcedCondition(conds []metav1.Condition) *metav1.Condition {
	for i := range conds {
		if conds[i].Type == "PolicyEnforced" {
			return &conds[i]
		}
	}
	return nil
}

// recordingEnforcer records the Apply calls so the test can assert the operator
// applies the effective policy (P1 #2: the gate must have something behind it).
type recordingEnforcer struct {
	applied []policy.EffectivePolicy
}

func (f *recordingEnforcer) Apply(_ context.Context, _ *coxv1alpha1.Loop, p policy.EffectivePolicy) error {
	f.applied = append(f.applied, p)
	return nil
}
func (f *recordingEnforcer) Enforcing(_ context.Context, _ *coxv1alpha1.Loop) (bool, string) {
	return true, ""
}
