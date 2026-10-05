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

// D41d (plan section D41d, reviewer P1 on PR #74): the tool proxy's
// KubeArmorPolicy (the per-tool inner fence) is managed by the enforcer the
// same way the egress proxy's is (the I42f pattern):
//   - (a) one KubeArmorPolicy per tool in the effective policy, owner-ref'd to
//     the Loop, with the tool proxy's DISJOINT selector;
//   - (b) a FOREIGN policy occupying the name is NEVER overwritten (I2
//     never-take-over) — the KubeArmorPolicyConflict condition is set and the
//     sandbox is held Suspended (the order-independent gate reads the live
//     objects, so the hold does not flap on re-reconcile);
//   - (c) when a tool is removed from the effective policy the operator-owned
//     KubeArmorPolicy is deleted (cleanupStaleToolKapt, the egress-proxy drift
//     rationale);
//   - the same-Loop update: changing a tool's upstream host UPDATES the
//     existing KubeArmorPolicy's DNS allowlist in place (I43), not recreates it.
//
// These are real envtest specs (the KubeArmor CRD is registered in
// config/crd/external), following the I42f egress-proxy Kapt envtest. The
// reviewer mutation C (kubearmur_enforcer.go: the tool Kapt create guarded by
// `if err := error(nil); toolObj == nil && err != nil`) is disabled by the
// (a) spec (the Kapt is created) and by the (c) spec (the stale Kapt is
// cleaned up via listToolKaptNames, which reads the live objects created
// here) — see the scratch-worktree mutation result in the PR body.

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	coxv1alpha1 "github.com/papawattu/coxswain/api/v1alpha1"
	"github.com/papawattu/coxswain/internal/engine"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	sandboxv1beta1 "sigs.k8s.io/agent-sandbox/api/v1beta1"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

const (
	d41kaptLoopCreated    = "d41k-created"
	d41kaptLoopForeign    = "d41k-foreign"
	d41kaptLoopRemove     = "d41k-remove"
	d41kaptLoopUpdate     = "d41k-update"
	d41kaptPolicyName     = "d41k-pol"
	d41kaptToolName       = "gh"
	d41kaptUpstreamA      = "https://api.github.com"
	d41kaptUpstreamB      = "https://api.github.example"
	d41kaptForeignLabel   = "external"
	kaptComponentLabel    = "app.kubernetes.io/component"
	kaptPolicyKind        = "KubeArmorPolicy"
	kaptPolicyAPIVersion  = "security.kubearmor.com/v1"
	kaptConflictCondition = "KubeArmorPolicyConflict"
)

var _ = Describe("D41d: tool proxy KubeArmorPolicies (enforcer)", func() {
	var (
		ctx context.Context
		r   *LoopReconciler
	)

	BeforeEach(func() {
		ctx = context.Background()
		r = &LoopReconciler{
			Client:          k8sClient,
			Scheme:          k8sClient.Scheme(),
			PodCIDR:         d41cPodCIDR,
			ServiceCIDR:     d41cServiceCIDR,
			ToolProxyImage:  d41cToolProxyImg,
			AllowUnenforced: true,
			Enforcer:        &engine.KubeArmorEnforcer{Client: k8sClient, ProxyFQDN: ProxyServiceFQDN, EgressProxyFQDN: EgressProxyServiceFQDN, ToolProxyFQDN: ToolProxyServiceFQDN},
		}
	})

	buildLoop := func(name, ns string, policyRefs []string) *coxv1alpha1.Loop {
		return &coxv1alpha1.Loop{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec: coxv1alpha1.LoopSpec{
				Goal:       "d41kapt test",
				Workspace:  coxv1alpha1.Workspace{Repo: d41cTestRepo, Ref: loopRef},
				PolicyRefs: policyRefs,
				Agent:      coxv1alpha1.AgentConfig{Image: runnerImage, Model: testModel},
			},
		}
	}

	setupNS := func(name string) string {
		ns := "d41k-" + name + "-" + nowSuffix()
		Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}) })
		return ns
	}

	reconcileLoop := func(name, ns string) {
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: name}})
		Expect(err).NotTo(HaveOccurred())
	}

	// (a): one tool in the effective policy → the tool proxy's KubeArmorPolicy
	// is created, owner-ref'd to the Loop, with the tool proxy's DISJOINT
	// selector (never coxswain.io/loop).
	It("(a) creates the tool proxy KubeArmorPolicy, owner-ref'd to the Loop, with the disjoint selector", func() {
		ns := setupNS("created")
		Expect(k8sClient.Create(ctx, &coxv1alpha1.AgentPolicy{
			ObjectMeta: metav1.ObjectMeta{Name: d41kaptPolicyName, Namespace: ns},
			Spec:       coxv1alpha1.AgentPolicySpec{Tools: []coxv1alpha1.ToolSpec{d41cToolGH(d41kaptUpstreamA)}},
		})).To(Succeed())

		loop := buildLoop(d41kaptLoopCreated, ns, []string{d41kaptPolicyName})
		Expect(k8sClient.Create(ctx, loop)).To(Succeed())
		reconcileLoop(d41kaptLoopCreated, ns)

		kap := getKapt(ctx, ns, "coxswain-"+d41kaptLoopCreated+"-tool-"+d41kaptToolName)
		// Disjoint selector: the tool proxy's own label set, NOT coxswain.io/loop.
		Expect(kaptSelectorLabels(kap)).To(Equal(map[string]string{
			kaptComponentLabel:           "tool-proxy",
			"coxswain.io/tool-proxy-for": d41kaptLoopCreated,
			"coxswain.io/tool":           d41kaptToolName,
		}))
		// The upstream host is in the DNS allowlist (the per-tool inner fence).
		Expect(kaptDNSDomains(kap)).To(ContainElement("api.github.com"))
		// Owner-ref'd to the Loop (GC with the Loop).
		ownerRefs := kap.GetOwnerReferences()
		Expect(ownerRefs).NotTo(BeEmpty(), "the tool proxy KubeArmorPolicy must be owner-ref'd to the Loop")
		Expect(ownerRefs[0].Name).To(Equal(d41kaptLoopCreated))
		Expect(ownerRefs[0].Kind).To(Equal(loopKind))
	})

	// (b): a FOREIGN KubeArmorPolicy occupying the tool proxy's name is NEVER
	// overwritten (I2 never-take-over). The KubeArmorPolicyConflict condition is
	// set and the sandbox is held Suspended (the order-independent gate reads
	// the live objects, so the hold does not flap on re-reconcile). This is the
	// I42f round-2 meaningfulness pattern: mark the tool proxy pod Ready so the
	// D41c owned+Ready gate passes, isolating the KubeArmor gate as the one
	// holding the sandbox Suspended.
	It("(b) does not overwrite a foreign tool KubeArmorPolicy; holds the sandbox Suspended across reconciles", func() {
		ns := setupNS("foreign")
		Expect(k8sClient.Create(ctx, &coxv1alpha1.AgentPolicy{
			ObjectMeta: metav1.ObjectMeta{Name: d41kaptPolicyName, Namespace: ns},
			Spec:       coxv1alpha1.AgentPolicySpec{Tools: []coxv1alpha1.ToolSpec{d41cToolGH(d41kaptUpstreamA)}},
		})).To(Succeed())

		// Pre-create a FOREIGN KubeArmorPolicy occupying the tool proxy's name
		// (a different owner; the Loop's owner ref is absent).
		foreignName := "coxswain-" + d41kaptLoopForeign + "-tool-" + d41kaptToolName
		foreign := &unstructured.Unstructured{Object: map[string]any{
			unstructuredAPI:  kaptPolicyAPIVersion,
			unstructuredKind: kaptPolicyKind,
			unstructuredMeta: map[string]any{
				unstructuredName: foreignName,
				"namespace":      ns,
				"labels":         map[string]any{d41kaptForeignLabel: unstructuredTrue},
			},
			unstructuredSpec: map[string]any{"action": "Audit"},
		}}
		Expect(k8sClient.Create(ctx, foreign)).To(Succeed())

		loop := buildLoop(d41kaptLoopForeign, ns, []string{d41kaptPolicyName})
		Expect(k8sClient.Create(ctx, loop)).To(Succeed())

		// First reconcile: creates the sandbox + tool proxy pod. The D41c
		// owned+Ready gate holds it Suspended (the pod is not Ready yet).
		reconcileLoop(d41kaptLoopForeign, ns)

		// Mark the tool proxy pod Ready so the D41c owned+Ready gate passes.
		// With the pod Ready and no KubeArmor conflict the sandbox would be
		// Running; the ONLY thing holding it Suspended is the order-independent
		// KubeArmorPolicy gate (the foreign tool policy).
		podName := d41kaptLoopForeign + "-tool-" + d41kaptToolName
		pod := &corev1.Pod{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: podName}, pod)).To(Succeed(),
			"tool proxy pod %s must exist", podName)
		pod.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
		Expect(k8sClient.Status().Update(ctx, pod)).To(Succeed(), "mark %s Ready", podName)

		// Second reconcile: the tool proxy pod is Ready, so the D41c gate passes;
		// the KubeArmorPolicy gate must hold the sandbox Suspended (fail-closed:
		// the tool policy is the foreign one, not the one the operator built).
		reconcileLoop(d41kaptLoopForeign, ns)
		sb := &sandboxv1beta1.Sandbox{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: sandboxName(d41kaptLoopForeign)}, sb)).To(Succeed())
		Expect(sb.Spec.OperatingMode).To(Equal(sandboxv1beta1.SandboxOperatingModeSuspended),
			"the sandbox must be Suspended while a foreign KubeArmorPolicy occupies the tool proxy's name, even with the tool proxy pod Ready")

		// Third reconcile: the gate is order-independent (it reads the live
		// KubeArmorPolicy objects in ensureSandbox, which runs before
		// applyEffectivePolicyAndConditions), so a second pass must NOT flip it
		// back to Running.
		reconcileLoop(d41kaptLoopForeign, ns)
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: sandboxName(d41kaptLoopForeign)}, sb)).To(Succeed())
		Expect(sb.Spec.OperatingMode).To(Equal(sandboxv1beta1.SandboxOperatingModeSuspended),
			"the sandbox must STAY Suspended on the second reconcile (no flap back to Running)")

		// The foreign object is left untouched (spec + label intact, no owner ref).
		got := &unstructured.Unstructured{}
		got.SetGroupVersionKind(engine.KubeArmorGVK)
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: foreignName}, got)).To(Succeed())
		Expect(got.GetOwnerReferences()).To(BeEmpty(),
			"the foreign tool KubeArmorPolicy must not be owner-ref'd to the Loop")
		lbl, _, _ := unstructured.NestedMap(got.Object, "metadata", "labels")
		Expect(lbl).To(HaveKey(d41kaptForeignLabel),
			"the foreign tool KubeArmorPolicy must be left untouched (its labels are intact)")

		// The conflict condition is set (same pattern as the egress proxy).
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: d41kaptLoopForeign}, loop)).To(Succeed())
		var conflict *metav1.Condition
		for i := range loop.Status.Conditions {
			if loop.Status.Conditions[i].Type == kaptConflictCondition {
				conflict = &loop.Status.Conditions[i]
			}
		}
		Expect(conflict).ToNot(BeNil(), "KubeArmorPolicyConflict condition must be set")
		Expect(conflict.Status).To(Equal(metav1.ConditionTrue))
		Expect(conflict.Reason).To(Equal("ForeignKubeArmorPolicy"))
	})

	// (c): a tool removed from the effective policy has its operator-owned
	// KubeArmorPolicy deleted (cleanupStaleToolKapt, the egress-proxy drift
	// rationale: a stale policy would keep fencing a pod that no longer
	// exists). This is the I43 same-Loop delete spec — re-read the policy from
	// the API server and assert DELETED.
	It("(c) deletes the tool proxy KubeArmorPolicy when the tool is removed from the effective policy", func() {
		ns := setupNS("remove")
		Expect(k8sClient.Create(ctx, &coxv1alpha1.AgentPolicy{
			ObjectMeta: metav1.ObjectMeta{Name: d41kaptPolicyName, Namespace: ns},
			Spec:       coxv1alpha1.AgentPolicySpec{Tools: []coxv1alpha1.ToolSpec{d41cToolGH(d41kaptUpstreamA)}},
		})).To(Succeed())

		loop := buildLoop(d41kaptLoopRemove, ns, []string{d41kaptPolicyName})
		Expect(k8sClient.Create(ctx, loop)).To(Succeed())
		reconcileLoop(d41kaptLoopRemove, ns)
		getKapt(ctx, ns, "coxswain-"+d41kaptLoopRemove+"-tool-"+d41kaptToolName)

		// Remove the tool from the AgentPolicy: the tool is no longer in the
		// effective union.
		ap := &coxv1alpha1.AgentPolicy{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: d41kaptPolicyName}, ap)).To(Succeed())
		ap.Spec.Tools = nil
		Expect(k8sClient.Update(ctx, ap)).To(Succeed())
		reconcileLoop(d41kaptLoopRemove, ns)

		obj := &unstructured.Unstructured{}
		obj.SetGroupVersionKind(engine.KubeArmorGVK)
		err := k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "coxswain-" + d41kaptLoopRemove + "-tool-" + d41kaptToolName}, obj)
		Expect(err).To(HaveOccurred(),
			"the tool proxy KubeArmorPolicy must be cleaned up when the tool is removed from the effective policy")
	})

	// I43 same-Loop update: changing a tool's upstream host UPDATES the
	// existing KubeArmorPolicy's DNS allowlist in place (createOrUpdateKapt
	// re-asserts the desired spec against the live object), not recreates it.
	It("updates the existing tool KubeArmorPolicy's DNS allowlist in place when the tool's upstream host changes", func() {
		ns := setupNS("update")
		Expect(k8sClient.Create(ctx, &coxv1alpha1.AgentPolicy{
			ObjectMeta: metav1.ObjectMeta{Name: d41kaptPolicyName, Namespace: ns},
			Spec:       coxv1alpha1.AgentPolicySpec{Tools: []coxv1alpha1.ToolSpec{d41cToolGH(d41kaptUpstreamA)}},
		})).To(Succeed())

		loop := buildLoop(d41kaptLoopUpdate, ns, []string{d41kaptPolicyName})
		Expect(k8sClient.Create(ctx, loop)).To(Succeed())
		reconcileLoop(d41kaptLoopUpdate, ns)
		kap := getKapt(ctx, ns, "coxswain-"+d41kaptLoopUpdate+"-tool-"+d41kaptToolName)
		firstUID := kap.GetUID()
		Expect(kaptDNSDomains(kap)).To(ContainElement("api.github.com"))

		// Change the tool's upstream host (same tool name).
		ap := &coxv1alpha1.AgentPolicy{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: d41kaptPolicyName}, ap)).To(Succeed())
		ap.Spec.Tools = []coxv1alpha1.ToolSpec{d41cToolGH(d41kaptUpstreamB)}
		Expect(k8sClient.Update(ctx, ap)).To(Succeed())
		reconcileLoop(d41kaptLoopUpdate, ns)

		// The EXISTING KubeArmorPolicy is updated in place: the new domain is
		// present, and the object identity is unchanged (same UID → UPDATE, not
		// a delete-and-recreate).
		kap2 := getKapt(ctx, ns, "coxswain-"+d41kaptLoopUpdate+"-tool-"+d41kaptToolName)
		Expect(kap2.GetUID()).To(Equal(firstUID), "the tool KubeArmorPolicy must be updated in place (same UID)")
		Expect(kaptDNSDomains(kap2)).To(ContainElement("api.github.example"),
			"the changed upstream host must be present in the updated policy")
		ownerRefs := kap2.GetOwnerReferences()
		Expect(ownerRefs).NotTo(BeEmpty())
		Expect(ownerRefs[0].Name).To(Equal(d41kaptLoopUpdate))
	})
})
