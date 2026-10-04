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

// I43 test norm (R16): same-Loop update specs for the agent KubeArmorPolicy
// (coxswain-<loop>). The C6b specs only ever asserted the policy at creation;
// this spec edits a referenced AgentPolicy (exec + network) on the SAME Loop
// and re-reconciles, asserting the EXISTING policy is updated in place —
// the exec rules gain the new path and the DNS allowlist gains the new host
// (and the egress-proxy FQDN when network allows first appear). No production
// behaviour changed.

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	coxv1alpha1 "github.com/papawattu/coxswain/api/v1alpha1"
	"github.com/papawattu/coxswain/internal/engine"
)

const (
	i43AgentKaptPolicy = "i43-agent-kapt-pol"
	i43ExecOne         = "/usr/bin/curl"
	i43ExecTwo         = "/usr/bin/rsync"
	i43NetAllow        = i42eExternalHost
	i43TestRepo        = "https://github.com/papawattu/coxswain.git"
)

var _ = Describe("I43: agent KubeArmorPolicy same-Loop update", func() {
	var (
		ctx context.Context
		r   *LoopReconciler
	)

	BeforeEach(func() {
		ctx = context.Background()
		// The real KubeArmor enforcer (the fakeEnforcer writes no policies).
		r = &LoopReconciler{
			Client:           k8sClient,
			Scheme:           k8sClient.Scheme(),
			Enforcer:         &engine.KubeArmorEnforcer{Client: k8sClient, ProxyFQDN: ProxyServiceFQDN, EgressProxyFQDN: EgressProxyServiceFQDN},
			PodCIDR:          i42bPodCIDR,
			ServiceCIDR:      i42bServiceCIDR,
			EgressProxyImage: i42bEgressProxyImg,
			// AllowUnenforced: isolate the policy update from the D30 gate
			// (the envtest Enforcer never reports enforcing).
			AllowUnenforced: true,
		}
	})

	reconcileLoop := func(name, ns string) {
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: name}})
		Expect(err).NotTo(HaveOccurred())
	}

	// kaptExecPaths returns the agent policy's process.matchPaths paths.
	kaptExecPaths := func(ns, loopName string) []string {
		obj := &unstructured.Unstructured{}
		obj.SetGroupVersionKind(engine.KubeArmorGVK)
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "coxswain-" + loopName}, obj)).To(Succeed(),
			"the agent KubeArmorPolicy %s must exist", loopName)
		spec, _, _ := unstructured.NestedMap(obj.Object, engine.KaptSpecKey)
		proc, _, _ := unstructured.NestedMap(spec, engine.KaptProcessKey)
		items, _ := proc[engine.KaptMatchPathsKey].([]any)
		paths := make([]string, 0, len(items))
		for _, it := range items {
			paths = append(paths, it.(map[string]any)["path"].(string))
		}
		return paths
	}

	// kaptAgentDNSDomains returns the agent policy's network.matchDNSQueries
	// domains.
	kaptAgentDNSDomains := func(ns, loopName string) []string {
		obj := &unstructured.Unstructured{}
		obj.SetGroupVersionKind(engine.KubeArmorGVK)
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "coxswain-" + loopName}, obj)).To(Succeed(),
			"the agent KubeArmorPolicy %s must exist", loopName)
		spec, _, _ := unstructured.NestedMap(obj.Object, engine.KaptSpecKey)
		network, _, _ := unstructured.NestedMap(spec, "network")
		items, _ := network[engine.KaptMatchDNSKey].([]any)
		domains := make([]string, 0, len(items))
		for _, it := range items {
			domains = append(domains, it.(map[string]any)["domain"].(string))
		}
		return domains
	}

	It("updates the agent KubeArmorPolicy when the referenced AgentPolicy's exec and network are edited (I43 same-Loop)", func() {
		ns := "i43-agent-kapt-" + nowSuffix()
		Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}) })

		// The referenced AgentPolicy starts with one exec allow and NO
		// network allows (no egress proxy expected).
		Expect(k8sClient.Create(ctx, &coxv1alpha1.AgentPolicy{
			ObjectMeta: metav1.ObjectMeta{Name: i43AgentKaptPolicy, Namespace: ns},
			Spec:       coxv1alpha1.AgentPolicySpec{Exec: []string{i43ExecOne, "/bin/sh"}},
		})).To(Succeed())

		const loopName = "i43-kapt"
		loop := &coxv1alpha1.Loop{
			ObjectMeta: metav1.ObjectMeta{Name: loopName, Namespace: ns},
			Spec: coxv1alpha1.LoopSpec{
				Goal:       "i43 agent kapt update",
				Workspace:  coxv1alpha1.Workspace{Repo: i43TestRepo, Ref: loopRef},
				PolicyRefs: []string{i43AgentKaptPolicy},
				Agent:      coxv1alpha1.AgentConfig{Image: runnerImage, Model: testModel},
			},
		}
		Expect(k8sClient.Create(ctx, loop)).To(Succeed())
		reconcileLoop(loopName, ns)

		// The agent KubeArmorPolicy exists with the initial exec allow only.
		Expect(kaptExecPaths(ns, loopName)).To(ContainElement(i43ExecOne))
		Expect(kaptExecPaths(ns, loopName)).ToNot(ContainElement(i43ExecTwo))
		// No network allows -> the agent policy carries no egress-proxy FQDN.
		Expect(kaptAgentDNSDomains(ns, loopName)).ToNot(ContainElement(EgressProxyServiceFQDN(loopName, ns)),
			"setup: no network allows -> no egress proxy FQDN on the agent allowlist")

		// EDIT the referenced AgentPolicy on the SAME Loop: add a second exec
		// allow and a network allow (the egress proxy now becomes expected).
		ap := &coxv1alpha1.AgentPolicy{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: i43AgentKaptPolicy}, ap)).To(Succeed())
		ap.Spec.Exec = []string{i43ExecOne, i43ExecTwo, "/bin/sh"}
		ap.Spec.Network = []string{i43NetAllow}
		Expect(k8sClient.Update(ctx, ap)).To(Succeed())
		reconcileLoop(loopName, ns)

		// The EXISTING agent KubeArmorPolicy is updated in place (I43: the
		// update path, not just creation):
		paths := kaptExecPaths(ns, loopName)
		Expect(paths).To(ContainElement(i43ExecTwo),
			"the existing agent KubeArmorPolicy must GAIN the new exec allow on the same-Loop reconcile")
		Expect(paths).To(ContainElement(i43ExecOne),
			"the existing exec allow must remain (the update is a union, not a replace-and-forget)")
		// The network allow first appearing -> the egress proxy FQDN joins
		// the agent's DNS allowlist (needsEgressProxy flipped on the same
		// Loop; the existing policy must pick it up).
		Expect(kaptAgentDNSDomains(ns, loopName)).To(ContainElement(EgressProxyServiceFQDN(loopName, ns)),
			"the existing agent KubeArmorPolicy must GAIN the egress proxy FQDN when network allows are added")

		// The policy is still owner-ref'd to the Loop (the update did not
		// drop ownership).
		obj := &unstructured.Unstructured{}
		obj.SetGroupVersionKind(engine.KubeArmorGVK)
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "coxswain-" + loopName}, obj)).To(Succeed())
		ownerRefs := obj.GetOwnerReferences()
		Expect(ownerRefs).NotTo(BeEmpty())
		Expect(ownerRefs[0].Name).To(Equal(loopName))
	})
})
