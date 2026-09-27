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

// c6aGitBin is the absolute path of the git binary the C6a test AgentPolicies
// allow (a real binary outside the writable mounts, satisfying the exec
// absolute-path CEL rule). Shared so goconst doesn't flag the repeated literal.
const (
	c6aGitBin        = "/usr/bin/git"
	c6aTestRepo      = "https://github.com/papawattu/coxswain.git"
	nonCanonLoopName = "noncanon-loop"
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
				Workspace: coxv1alpha1.Workspace{Repo: repo, Ref: loopRef},
			},
		}
	}

	It("records the union hash of multiple referenced AgentPolicies", func() {
		ns := "c6a-union"
		Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
		defer func() { _ = k8sClient.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}) }()

		Expect(k8sClient.Create(ctx, &coxv1alpha1.AgentPolicy{
			ObjectMeta: metav1.ObjectMeta{Name: "p1", Namespace: ns},
			Spec:       coxv1alpha1.AgentPolicySpec{Exec: []string{c6aGitBin}, Network: []string{"proxy.golang.org:443"}},
		})).To(Succeed())
		Expect(k8sClient.Create(ctx, &coxv1alpha1.AgentPolicy{
			ObjectMeta: metav1.ObjectMeta{Name: "p2", Namespace: ns},
			Spec:       coxv1alpha1.AgentPolicySpec{Exec: []string{"/usr/local/go/bin/go"}, Files: []string{"/data"}},
		})).To(Succeed())

		loop := buildLoop("loop-c6a-union", ns, "make it pass", "https://github.com/papawattu/coxswain.git")
		loop.Spec.PolicyRefs = []string{"p1", "p2"}
		Expect(k8sClient.Create(ctx, loop)).To(Succeed())

		By("reconciling")
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: "loop-c6a-union"}})
		Expect(err).NotTo(HaveOccurred())

		// The expected union hash: exec={c6aGitBin,/usr/local/go/bin/go}, network={proxy.golang.org:443}, files={/data}.
		want := policy.EffectiveHash(policy.EffectivePolicy{
			Exec:    []string{c6aGitBin, "/usr/local/go/bin/go"},
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

	It("rejects exec entries that are not absolute paths or are under the writable mounts", func() {
		ns := "c6a-exec-validation"
		Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
		defer func() { _ = k8sClient.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}) }()

		// A valid absolute path outside the writable mounts is accepted.
		Expect(k8sClient.Create(ctx, &coxv1alpha1.AgentPolicy{
			ObjectMeta: metav1.ObjectMeta{Name: "ok", Namespace: ns},
			Spec:       coxv1alpha1.AgentPolicySpec{Exec: []string{c6aGitBin}},
		})).To(Succeed(), "a real binary path outside the writable mounts must be allowed")

		// A bare command name (no leading /) is rejected — it is spoofable.
		Expect(k8sClient.Create(ctx, &coxv1alpha1.AgentPolicy{
			ObjectMeta: metav1.ObjectMeta{Name: "bare", Namespace: ns},
			Spec:       coxv1alpha1.AgentPolicySpec{Exec: []string{"git"}},
		})).To(MatchError(ContainSubstring("exec")), "a bare command name must be rejected by the CEL rule")

		// A path under a writable mount (/tmp) is rejected — the agent could write
		// its own /tmp/git and spoof the allow.
		for _, spoof := range []string{"/tmp/git", "/workspace", "/scratch/foo", "/tmp"} {
			Expect(k8sClient.Create(ctx, &coxv1alpha1.AgentPolicy{
				ObjectMeta: metav1.ObjectMeta{Name: "spoof", Namespace: ns},
				Spec:       coxv1alpha1.AgentPolicySpec{Exec: []string{spoof}},
			})).To(HaveOccurred(), "a path at or under a writable mount (%s) must be rejected", spoof)
		}

		// Non-canonical paths that resolve to a writable mount after normalization
		// are NOT rejected by the CRD CEL rule (cost budget), but ARE rejected by
		// the controller in effectivePolicyHash (P1, R15). See the
		// "rejects a Loop that references an AgentPolicy with a non-canonical exec
		// path" spec below for the controller-level test.
	})

	It("rejects a Loop that references an AgentPolicy with a non-canonical exec path", func() {
		ns := "c6a-noncanon-loop"
		Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
		defer func() { _ = k8sClient.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}) }()

		// The CRD accepts non-canonical paths (CEL cost budget), so create succeeds.
		ap := &coxv1alpha1.AgentPolicy{
			ObjectMeta: metav1.ObjectMeta{Name: "noncanon", Namespace: ns},
			Spec:       coxv1alpha1.AgentPolicySpec{Exec: []string{"/usr/../tmp/git"}},
		}
		Expect(k8sClient.Create(ctx, ap)).To(Succeed(),
			"the CRD accepts non-canonical paths (CEL cost budget); the controller rejects them")

		// A Loop that references this AgentPolicy must fail reconciliation.
		loop := &coxv1alpha1.Loop{
			ObjectMeta: metav1.ObjectMeta{Name: nonCanonLoopName, Namespace: ns},
			Spec: coxv1alpha1.LoopSpec{
				Goal: "test non-canonical exec path rejection",
				Workspace: coxv1alpha1.Workspace{
					Repo: c6aTestRepo,
					Ref:  loopRef,
				},
				Verify: coxv1alpha1.VerifyConfig{
					AcceptanceChecks: []string{"go test ./..."},
				},
				Agent:      coxv1alpha1.AgentConfig{Model: "test"},
				Loop:       coxv1alpha1.LoopSettings{MaxIterations: 1},
				PolicyRefs: []string{"noncanon"},
			},
		}
		Expect(k8sClient.Create(ctx, loop)).To(Succeed())

		// Reconcile: the controller must set the PolicyValid=False condition
		// (NonCanonicalExecPath) and NOT create a sandbox pod.
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: nonCanonLoopName}})
		Expect(err).ToNot(HaveOccurred(),
			"the controller sets a condition, not an error (R15 round 2)")

		// Assert the condition is set.
		gotLoop := &coxv1alpha1.Loop{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: nonCanonLoopName}, gotLoop)).To(Succeed())
		var policyValid *metav1.Condition
		for i := range gotLoop.Status.Conditions {
			if gotLoop.Status.Conditions[i].Type == "PolicyValid" {
				policyValid = &gotLoop.Status.Conditions[i]
			}
		}
		Expect(policyValid).ToNot(BeNil(),
			"the Loop must have a PolicyValid condition when a non-canonical exec path is found")
		Expect(policyValid.Status).To(Equal(metav1.ConditionFalse),
			"PolicyValid must be False for a non-canonical exec path")
		Expect(policyValid.Reason).To(Equal("NonCanonicalExecPath"),
			"the reason must be NonCanonicalExecPath")

		// Assert no sandbox pod was created.
		sandboxPod := &corev1.Pod{}
		err = k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "noncanon-loop-sandbox"}, sandboxPod)
		Expect(err).To(HaveOccurred(),
			"a Loop with a non-canonical exec path must NOT get a sandbox pod")
	})
})
