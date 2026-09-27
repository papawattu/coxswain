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
	policyValidType  = "PolicyValid"
	missingPolName   = "missing-pol-loop"
	cleanLoopName    = "clean-loop"
	watchLoopName    = "watch-loop"
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

	It("rejects an AgentPolicy with a non-canonical exec path at admission (CEL)", func() {
		ns := "c6a-noncanon-loop"
		Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
		defer func() { _ = k8sClient.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}) }()

		// R15 round 3: the CEL rule now rejects non-canonical paths at admission
		// (the contains/endsWith checks fit the budget with items:MaxLength=512).
		// /usr/../tmp/git is rejected by the CRD, not the controller.
		ap := &coxv1alpha1.AgentPolicy{
			ObjectMeta: metav1.ObjectMeta{Name: "noncanon", Namespace: ns},
			Spec:       coxv1alpha1.AgentPolicySpec{Exec: []string{"/usr/../tmp/git"}},
		}
		Expect(k8sClient.Create(ctx, ap)).ToNot(Succeed(),
			"the CRD must reject /usr/../tmp/git (contains '/../')")

		// /./tmp/git is also rejected.
		ap2 := &coxv1alpha1.AgentPolicy{
			ObjectMeta: metav1.ObjectMeta{Name: "noncanon2", Namespace: ns},
			Spec:       coxv1alpha1.AgentPolicySpec{Exec: []string{"/./tmp/git"}},
		}
		Expect(k8sClient.Create(ctx, ap2)).ToNot(Succeed(),
			"the CRD must reject /./tmp/git (contains '/./')")

		// A canonical path is accepted.
		ap3 := &coxv1alpha1.AgentPolicy{
			ObjectMeta: metav1.ObjectMeta{Name: "canon", Namespace: ns},
			Spec:       coxv1alpha1.AgentPolicySpec{Exec: []string{c6aGitBin}},
		}
		Expect(k8sClient.Create(ctx, ap3)).To(Succeed(),
			"the CRD must accept a canonical path")
	})
})

var _ = Describe("C6a (R15 round 3): fail-closed policy validation", func() {
	It("rejects a Loop that references a nonexistent AgentPolicy (PolicyNotFound)", func() {
		ns := "c6a-nonexistent-" + nowSuffix()
		Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())

		loop := &coxv1alpha1.Loop{
			ObjectMeta: metav1.ObjectMeta{Name: missingPolName, Namespace: ns},
			Spec: coxv1alpha1.LoopSpec{
				Goal: "test missing policy rejection",
				Workspace: coxv1alpha1.Workspace{
					Repo: c6aTestRepo,
					Ref:  loopRef,
				},
				PolicyRefs: []string{"does-not-exist"},
				Agent: coxv1alpha1.AgentConfig{
					Image: runnerImage,
					Model: testModel,
				},
				Loop: coxv1alpha1.LoopSettings{MaxIterations: 1},
			},
		}
		Expect(k8sClient.Create(ctx, loop)).To(Succeed())

		// Reconcile: the controller must set PolicyValid=False (PolicyNotFound)
		// and NOT create a sandbox pod.
		r := &LoopReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: missingPolName}})
		Expect(err).ToNot(HaveOccurred())

		// Check the condition.
		gotLoop := &coxv1alpha1.Loop{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: missingPolName}, gotLoop)).To(Succeed())
		var policyValid *metav1.Condition
		for i := range gotLoop.Status.Conditions {
			if gotLoop.Status.Conditions[i].Type == policyValidType {
				policyValid = &gotLoop.Status.Conditions[i]
			}
		}
		Expect(policyValid).ToNot(BeNil(), "the Loop must have a PolicyValid condition")
		Expect(policyValid.Status).To(Equal(metav1.ConditionFalse))
		Expect(policyValid.Reason).To(Equal("PolicyNotFound"),
			"a missing AgentPolicy must produce reason PolicyNotFound")

		// No sandbox pod.
		sandboxPod := &corev1.Pod{}
		err = k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "missing-pol-loop-sandbox"}, sandboxPod)
		Expect(err).To(HaveOccurred(), "a Loop with a missing AgentPolicy must NOT get a sandbox pod")
	})

	It("sets PolicyValid=True on a clean pass", func() {
		ns := "c6a-clean-" + nowSuffix()
		Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())

		// Create a valid AgentPolicy.
		ap := &coxv1alpha1.AgentPolicy{
			ObjectMeta: metav1.ObjectMeta{Name: "clean-pol", Namespace: ns},
			Spec: coxv1alpha1.AgentPolicySpec{
				Exec: []string{"/usr/bin/git"},
			},
		}
		Expect(k8sClient.Create(ctx, ap)).To(Succeed())

		loop := &coxv1alpha1.Loop{
			ObjectMeta: metav1.ObjectMeta{Name: cleanLoopName, Namespace: ns},
			Spec: coxv1alpha1.LoopSpec{
				Goal: "test clean policy pass",
				Workspace: coxv1alpha1.Workspace{
					Repo: c6aTestRepo,
					Ref:  loopRef,
				},
				PolicyRefs: []string{"clean-pol"},
				Agent: coxv1alpha1.AgentConfig{
					Image: runnerImage,
					Model: testModel,
				},
				Loop: coxv1alpha1.LoopSettings{MaxIterations: 1},
			},
		}
		Expect(k8sClient.Create(ctx, loop)).To(Succeed())

		// Reconcile: the controller should set PolicyValid=True.
		r := &LoopReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: cleanLoopName}})
		Expect(err).ToNot(HaveOccurred())

		// Check the condition.
		gotLoop := &coxv1alpha1.Loop{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: cleanLoopName}, gotLoop)).To(Succeed())
		var policyValid *metav1.Condition
		for i := range gotLoop.Status.Conditions {
			if gotLoop.Status.Conditions[i].Type == policyValidType {
				policyValid = &gotLoop.Status.Conditions[i]
			}
		}
		Expect(policyValid).ToNot(BeNil(), "the Loop must have a PolicyValid condition")
		Expect(policyValid.Status).To(Equal(metav1.ConditionTrue),
			"a clean policy pass must set PolicyValid=True")
		Expect(policyValid.Reason).To(Equal("Valid"))
	})
})

var _ = Describe("C6a (R15 round 4 P2): AgentPolicy watch", func() {
	It("reconciles a Loop when its AgentPolicy is created after the Loop", func() {
		ns := "c6a-policy-watch-" + nowSuffix()
		Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())

		// Create the Loop FIRST (without the AgentPolicy).
		loop := &coxv1alpha1.Loop{
			ObjectMeta: metav1.ObjectMeta{Name: watchLoopName, Namespace: ns},
			Spec: coxv1alpha1.LoopSpec{
				Goal: "test policy watch",
				Workspace: coxv1alpha1.Workspace{
					Repo: c6aTestRepo,
					Ref:  loopRef,
				},
				PolicyRefs: []string{"late-policy"},
				Agent: coxv1alpha1.AgentConfig{
					Image: runnerImage,
					Model: testModel,
				},
				Loop: coxv1alpha1.LoopSettings{MaxIterations: 1},
			},
		}
		Expect(k8sClient.Create(ctx, loop)).To(Succeed())

		// Reconcile: the policy doesn't exist yet, so the Loop gets
		// PolicyValid=False (PolicyNotFound).
		r := &LoopReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: watchLoopName}})
		Expect(err).ToNot(HaveOccurred())

		gotLoop := &coxv1alpha1.Loop{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: watchLoopName}, gotLoop)).To(Succeed())
		var policyValid *metav1.Condition
		for i := range gotLoop.Status.Conditions {
			if gotLoop.Status.Conditions[i].Type == policyValidType {
				policyValid = &gotLoop.Status.Conditions[i]
			}
		}
		Expect(policyValid).ToNot(BeNil())
		Expect(policyValid.Status).To(Equal(metav1.ConditionFalse),
			"before the policy exists, the Loop must be PolicyValid=False")
		Expect(policyValid.Reason).To(Equal("PolicyNotFound"))

		// Now create the AgentPolicy.
		ap := &coxv1alpha1.AgentPolicy{
			ObjectMeta: metav1.ObjectMeta{Name: "late-policy", Namespace: ns},
			Spec:       coxv1alpha1.AgentPolicySpec{Exec: []string{c6aGitBin}},
		}
		Expect(k8sClient.Create(ctx, ap)).To(Succeed())

		// The envtest suite has no running manager, so the Watches won't
		// fire automatically. Manually call Reconcile to simulate the watch
		// event.
		_, err = r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: watchLoopName}})
		Expect(err).ToNot(HaveOccurred())

		// The Loop should now be PolicyValid=True and have a sandbox.
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: watchLoopName}, gotLoop)).To(Succeed())
		for i := range gotLoop.Status.Conditions {
			if gotLoop.Status.Conditions[i].Type == policyValidType {
				Expect(gotLoop.Status.Conditions[i].Status).To(Equal(metav1.ConditionTrue),
					"after the policy is created, the Loop must be PolicyValid=True")
				break
			}
		}
	})

	It("unit tests the agentPolicyToLoopRequests map function", func() {
		ns := "c6a-map-func-" + nowSuffix()
		Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())

		// Create two Loops, one referencing "test-policy" and one not.
		loop1 := &coxv1alpha1.Loop{
			ObjectMeta: metav1.ObjectMeta{Name: "ref-loop", Namespace: ns},
			Spec: coxv1alpha1.LoopSpec{
				Goal:       "test map func",
				Workspace:  coxv1alpha1.Workspace{Repo: c6aTestRepo, Ref: loopRef},
				PolicyRefs: []string{"test-policy"},
				Agent:      coxv1alpha1.AgentConfig{Image: runnerImage, Model: testModel},
				Loop:       coxv1alpha1.LoopSettings{MaxIterations: 1},
			},
		}
		loop2 := &coxv1alpha1.Loop{
			ObjectMeta: metav1.ObjectMeta{Name: "unrelated-loop", Namespace: ns},
			Spec: coxv1alpha1.LoopSpec{
				Goal:      "test map func",
				Workspace: coxv1alpha1.Workspace{Repo: c6aTestRepo, Ref: loopRef},
				Agent:     coxv1alpha1.AgentConfig{Image: runnerImage, Model: testModel},
				Loop:      coxv1alpha1.LoopSettings{MaxIterations: 1},
			},
		}
		Expect(k8sClient.Create(ctx, loop1)).To(Succeed())
		Expect(k8sClient.Create(ctx, loop2)).To(Succeed())

		// Create the AgentPolicy.
		ap := &coxv1alpha1.AgentPolicy{
			ObjectMeta: metav1.ObjectMeta{Name: "test-policy", Namespace: ns},
			Spec:       coxv1alpha1.AgentPolicySpec{Exec: []string{c6aGitBin}},
		}
		Expect(k8sClient.Create(ctx, ap)).To(Succeed())

		// Unit test the map function: it should return only ref-loop.
		r := &LoopReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
		requests := r.agentPolicyToLoopRequests(ctx, ap)
		Expect(requests).To(HaveLen(1),
			"the map function should return only the Loop that references the policy")
		Expect(requests[0].Name).To(Equal("ref-loop"))
		Expect(requests[0].Namespace).To(Equal(ns))
	})
})
