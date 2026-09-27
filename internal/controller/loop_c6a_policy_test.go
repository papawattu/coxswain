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
	"github.com/papawattu/coxswain/internal/policy"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

var _ = Describe("C6a effective AgentPolicy union", func() {
	var (
		ctx context.Context
		r   *LoopReconciler
	)
	BeforeEach(func() {
		ctx = context.Background()
		r = &LoopReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
	})

	// buildLoop is a local helper (loop builders are per-It in this package).
	buildLoop := func(name, ns, goal, repo string) *coxv1alpha1.Loop {
		return &coxv1alpha1.Loop{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec: coxv1alpha1.LoopSpec{
				Goal:      goal,
				Workspace: coxv1alpha1.Workspace{Repo: repo, Ref: "main"},
			},
		}
	}

	It("records the union hash of multiple referenced AgentPolicies", func() {
		ns := "c6a-union"
		Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
		defer func() { _ = k8sClient.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}) }()

		Expect(k8sClient.Create(ctx, &coxv1alpha1.AgentPolicy{
			ObjectMeta: metav1.ObjectMeta{Name: "p1", Namespace: ns},
			Spec:       coxv1alpha1.AgentPolicySpec{Exec: []string{agentPolicyExecGit}, Network: []string{"proxy.golang.org:443"}},
		})).To(Succeed())
		Expect(k8sClient.Create(ctx, &coxv1alpha1.AgentPolicy{
			ObjectMeta: metav1.ObjectMeta{Name: "p2", Namespace: ns},
			Spec:       coxv1alpha1.AgentPolicySpec{Exec: []string{"go"}, Files: []string{"/data"}},
		})).To(Succeed())

		loop := buildLoop("loop-c6a-union", ns, "make it pass", "https://github.com/papawattu/coxswain.git")
		loop.Spec.PolicyRefs = []string{"p1", "p2"}
		Expect(k8sClient.Create(ctx, loop)).To(Succeed())

		By("reconciling")
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: "loop-c6a-union"}})
		Expect(err).NotTo(HaveOccurred())

		// The expected union hash: exec={git,go}, network={proxy.golang.org:443}, files={/data}.
		want := policy.EffectiveHash(policy.EffectivePolicy{
			Exec:    []string{agentPolicyExecGit, "go"},
			Network: []string{"proxy.golang.org:443"},
			Files:   []string{"/data"},
		})

		var got coxv1alpha1.Loop
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "loop-c6a-union"}, &got)).To(Succeed())
		Expect(got.Status.Policy).NotTo(BeNil(), "the operator should record the effective policy")
		Expect(got.Status.Policy.EffectiveHash).To(Equal(want),
			"status.policy.effectiveHash must be the union of the referenced policies' allows")
	})

	It("leaves the policy hash empty when no policyRefs are set", func() {
		ns := "c6a-none"
		Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
		defer func() { _ = k8sClient.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}) }()

		loop := buildLoop("loop-c6a-none", ns, "make it pass", "https://github.com/papawattu/coxswain.git")
		Expect(k8sClient.Create(ctx, loop)).To(Succeed())

		By("reconciling")
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: "loop-c6a-none"}})
		Expect(err).NotTo(HaveOccurred())

		var got coxv1alpha1.Loop
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "loop-c6a-none"}, &got)).To(Succeed())
		// With no policyRefs the operator records no effective policy (default-deny
		// minimum); the hash must stay empty.
		Expect(got.Status.Policy == nil || got.Status.Policy.EffectiveHash == "").To(BeTrue(),
			"no policyRefs means no effective policy hash (default-deny minimum)")
	})
})
