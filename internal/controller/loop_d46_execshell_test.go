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

// D46 (docs/REVIEW-PHASE1-R20.md, owner decision (c), 2026-10-03): exec
// fencing does not apply to the agent in the MVP — the runner runs every tool
// call via /bin/sh -c and a shell command spawns an open-ended set of
// binaries, so an agent exec list that omits the runner's shell wedges the
// Loop under an enforcing Block policy (observed: 30 minutes of no output in
// the S5b demo). This spec asserts the fail-fast gate: a Loop whose
// policyRefs carry an exec list WITHOUT the runner's shell gets
// PolicyValid=False reason ExecListMissingShell and the sandbox is never
// created; the same exec list WITH /bin/sh gets no such condition and the
// sandbox is created (I43 test norm: same-Loop re-reads from the API server
// and re-reconciles the update path).
//
// Gate mutation (R20 I49 norm, scratch worktree only): disabling the
// missingShellInExecList call in validateAgentPolicies must make the first
// spec FAIL (the sandbox gets created despite the shell-less exec list).

import (
	"context"
	"fmt"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	sandboxv1beta1 "sigs.k8s.io/agent-sandbox/api/v1beta1"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	coxv1alpha1 "github.com/papawattu/coxswain/api/v1alpha1"
	"github.com/papawattu/coxswain/internal/engine"
)

const (
	d46PolicyName = "d46-pol"
	d46LoopName   = "d46-loop"
	d46Reason     = "ExecListMissingShell"
)

// d46Condition returns the Loop's PolicyValid condition re-read from the API
// server, or nil when the condition is absent.
func d46Condition(ctx context.Context, ns string) *metav1.Condition {
	loop := &coxv1alpha1.Loop{}
	Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: d46LoopName}, loop)).To(Succeed())
	for i := range loop.Status.Conditions {
		if loop.Status.Conditions[i].Type == policyValidType {
			return &loop.Status.Conditions[i]
		}
	}
	return nil
}

// d46SandboxExists re-reads the Loop's sandbox from the API server.
func d46SandboxExists(ctx context.Context, ns, loopName string) bool {
	err := k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: loopName + "-sandbox"}, &sandboxv1beta1.Sandbox{})
	if err == nil {
		return true
	}
	return !strings.Contains(err.Error(), "not found")
}

var _ = Describe("D46: agent exec list without the runner's shell fails fast", func() {
	var (
		ctx context.Context
		r   *LoopReconciler
		ns  string
	)

	BeforeEach(func() {
		ctx = context.Background()
		// The real KubeArmor enforcer + AllowUnenforced so the D30 gate passes
		// once the policy is valid (the envtest Enforcer never reports
		// enforcing) and the clean pass is indistinguishable from an
		// enforcing cluster.
		r = &LoopReconciler{
			Client:           k8sClient,
			Scheme:           k8sClient.Scheme(),
			Enforcer:         &engine.KubeArmorEnforcer{Client: k8sClient, ProxyFQDN: ProxyServiceFQDN, EgressProxyFQDN: EgressProxyServiceFQDN, ToolProxyFQDN: ToolProxyServiceFQDN},
			PodCIDR:          i42bPodCIDR,
			ServiceCIDR:      i42bServiceCIDR,
			EgressProxyImage: i42bEgressProxyImg,
			AllowUnenforced:  true,
		}
		ns = "d46-" + nowSuffix()
		Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}) })
	})

	// buildD46Loop is the per-file Loop builder: a reference runner Loop that
	// points at the file's AgentPolicy (created per case).
	buildD46Loop := func() *coxv1alpha1.Loop {
		return &coxv1alpha1.Loop{
			ObjectMeta: metav1.ObjectMeta{Name: d46LoopName, Namespace: ns},
			Spec: coxv1alpha1.LoopSpec{
				Goal:       "d46 exec shell gate",
				Workspace:  coxv1alpha1.Workspace{Repo: c6aTestRepo, Ref: loopRef},
				PolicyRefs: []string{d46PolicyName},
				Agent:      coxv1alpha1.AgentConfig{Image: runnerImage, Model: testModel},
			},
		}
	}

	It("sets PolicyValid=False reason ExecListMissingShell and never creates the sandbox for an exec list without the runner's shell", func() {
		// The S5b task-1 list verbatim: go + git, no shell. The CRD CEL rules
		// admit it (absolute canonical paths, outside the writable mounts), so
		// the controller is the gate that must catch it.
		Expect(k8sClient.Create(ctx, &coxv1alpha1.AgentPolicy{
			ObjectMeta: metav1.ObjectMeta{Name: d46PolicyName, Namespace: ns},
			Spec:       coxv1alpha1.AgentPolicySpec{Exec: []string{"/usr/local/go/bin/go", c6aGitBin}},
		})).To(Succeed())

		Expect(k8sClient.Create(ctx, buildD46Loop())).To(Succeed())
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: d46LoopName}})
		Expect(err).NotTo(HaveOccurred())

		pv := d46Condition(ctx, ns)
		Expect(pv).ToNot(BeNil(), "the Loop must have a PolicyValid condition")
		Expect(pv.Status).To(Equal(metav1.ConditionFalse))
		Expect(pv.Reason).To(Equal(d46Reason),
			"D46: an agent exec list without the runner's shell must fail fast with reason ExecListMissingShell")
		Expect(pv.Message).To(ContainSubstring("/bin/sh"),
			"the condition message must name the missing shell (/bin/sh)")
		Expect(pv.Message).To(ContainSubstring("D46"),
			"the condition message must name the decision (D46)")
		// Fail-closed BEFORE creation: the sandbox must not exist (not just be
		// Suspended) — validateAgentPolicies runs before ensureSandbox.
		Expect(d46SandboxExists(ctx, ns, d46LoopName)).To(BeFalse(),
			"D46: the sandbox must NOT be started when the exec list lacks the runner's shell")
	})

	It("sets no such condition and creates the sandbox for an exec list WITH the runner's shell (same-Loop flip)", func() {
		// Same Loop, same policy name: start with the shell-less list (the
		// rejection state), then EDIT the referenced AgentPolicy to add the
		// shell and re-reconcile the SAME Loop. The I43 test norm: the update
		// path re-reads from the API server and the condition flips False ->
		// True on the same object.
		Expect(k8sClient.Create(ctx, &coxv1alpha1.AgentPolicy{
			ObjectMeta: metav1.ObjectMeta{Name: d46PolicyName, Namespace: ns},
			Spec:       coxv1alpha1.AgentPolicySpec{Exec: []string{"/usr/bin/git"}},
		})).To(Succeed())
		Expect(k8sClient.Create(ctx, buildD46Loop())).To(Succeed())
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: d46LoopName}})
		Expect(err).NotTo(HaveOccurred())

		pv := d46Condition(ctx, ns)
		Expect(pv).ToNot(BeNil())
		Expect(pv.Status).To(Equal(metav1.ConditionFalse))
		Expect(pv.Reason).To(Equal(d46Reason))
		Expect(d46SandboxExists(ctx, ns, d46LoopName)).To(BeFalse(),
			"step 1: the sandbox must not exist while the exec list lacks the shell")

		// Step 2: the SAME Loop, the referenced policy edited to include the
		// runner's shell (/bin/sh). Re-reconcile the live Loop (re-read from
		// the API server inside Reconcile).
		ap := &coxv1alpha1.AgentPolicy{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: d46PolicyName}, ap)).To(Succeed())
		ap.Spec.Exec = []string{runnerShellPath, c6aGitBin}
		Expect(k8sClient.Update(ctx, ap)).To(Succeed())
		_, err = r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: d46LoopName}})
		Expect(err).NotTo(HaveOccurred())

		pv = d46Condition(ctx, ns)
		Expect(pv).ToNot(BeNil())
		Expect(pv.Status).To(Equal(metav1.ConditionTrue),
			"step 2: the SAME Loop must flip PolicyValid=True once the exec list includes /bin/sh (update path, not just creation)")
		Expect(pv.Reason).To(Equal("Valid"))
		Expect(d46SandboxExists(ctx, ns, d46LoopName)).To(BeTrue(),
			"step 2: the sandbox must be created once the exec list includes the runner's shell")
	})

	It("treats an empty exec list as a clean pass (the default-deny minimum)", func() {
		// No exec allows at all: the KubeArmor policy carries no process rule,
		// so exec is unrestricted — the posture the demo runs. The gate must
		// NOT reject this (a referenced policy without an exec list is the
		// intended minimum; only a non-empty shell-less list wedges).
		Expect(k8sClient.Create(ctx, &coxv1alpha1.AgentPolicy{
			ObjectMeta: metav1.ObjectMeta{Name: d46PolicyName, Namespace: ns},
			Spec:       coxv1alpha1.AgentPolicySpec{Network: []string{i42eExternalHost}},
		})).To(Succeed())
		Expect(k8sClient.Create(ctx, buildD46Loop())).To(Succeed())
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: d46LoopName}})
		Expect(err).NotTo(HaveOccurred())

		pv := d46Condition(ctx, ns)
		Expect(pv).ToNot(BeNil())
		Expect(pv.Status).To(Equal(metav1.ConditionTrue),
			"an empty exec list is the default-deny minimum, not a shell-less wedge")
		Expect(pv.Reason).NotTo(Equal(d46Reason), fmt.Sprintf("no %s condition for the empty exec list", d46Reason))
	})
})
