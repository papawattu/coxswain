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

// I42f (plan envtests 3-4, D35 part 2): the operator emits a KubeArmorPolicy
// for EACH operator-owned proxy pod (the egress proxy AND the model proxy),
// owned by the Loop and cleaned up when the proxy is not expected. The agent's
// C6b policy is unchanged.

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
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

const (
	i42fTestPolicyName = "i42f-pol"
	i42fModelEndpoint  = "model.test:8443" // host:port form (the CRD rejects a URL)
)

// getKapt fetches a KubeArmorPolicy by name, unstructured.
func getKapt(ctx context.Context, ns, name string) *unstructured.Unstructured {
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(engine.KubeArmorGVK)
	Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, obj)).To(Succeed(),
		"KubeArmorPolicy %s/%s must exist", ns, name)
	return obj
}

// kaptDNSDomains returns the policy's network.matchDNSQueries domains.
func kaptDNSDomains(obj *unstructured.Unstructured) []string {
	network, _, _ := unstructured.NestedMap(obj.Object, "spec", "network")
	items, _ := network["matchDNSQueries"].([]any)
	domains := make([]string, 0, len(items))
	for _, it := range items {
		domains = append(domains, it.(map[string]any)["domain"].(string))
	}
	return domains
}

// kaptSelectorLabels returns the policy's selector.matchLabels.
func kaptSelectorLabels(obj *unstructured.Unstructured) map[string]string {
	sel, _, _ := unstructured.NestedMap(obj.Object, "spec", "selector")
	raw, _ := sel["matchLabels"].(map[string]any)
	out := make(map[string]string, len(raw))
	for k, v := range raw {
		out[k] = v.(string)
	}
	return out
}

// kaptProcessPaths returns the policy's process.matchPaths paths.
func kaptProcessPaths(obj *unstructured.Unstructured) []string {
	proc, _, _ := unstructured.NestedMap(obj.Object, "spec", "process")
	items, _ := proc["matchPaths"].([]any)
	paths := make([]string, 0, len(items))
	for _, it := range items {
		paths = append(paths, it.(map[string]any)["path"].(string))
	}
	return paths
}

var _ = Describe("I42f: proxy KubeArmorPolicies", func() {
	var (
		ctx context.Context
		r   *LoopReconciler
	)

	BeforeEach(func() {
		ctx = context.Background()
		r = &LoopReconciler{
			Client:           k8sClient,
			Scheme:           k8sClient.Scheme(),
			PodCIDR:          i42bPodCIDR,
			ServiceCIDR:      i42bServiceCIDR,
			EgressProxyImage: i42bEgressProxyImg,
			Enforcer:         &engine.KubeArmorEnforcer{Client: k8sClient},
		}
	})

	buildLoop := func(name, ns string, policyRefs []string) *coxv1alpha1.Loop {
		return &coxv1alpha1.Loop{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec: coxv1alpha1.LoopSpec{
				Goal:       "i42f test",
				Workspace:  coxv1alpha1.Workspace{Repo: i42bTestRepo, Ref: loopRef},
				PolicyRefs: policyRefs,
				Agent: coxv1alpha1.AgentConfig{
					Image:             runnerImage,
					Model:             testModel,
					EndpointSecretRef: "model-creds",
					ModelEndpoint:     i42fModelEndpoint,
				},
			},
		}
	}

	setupNS := func(name string) string {
		ns := "i42f-" + name + "-" + nowSuffix()
		Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}) })
		return ns
	}

	// plan envtest 3: after reconcile with both proxies expected (network
	// allows + model endpoint), two proxy KubeArmorPolicies exist, both
	// owner-ref'd to the Loop. (AllowUnenforced: the envtest sandbox pod is
	// not scheduled; the gate does not gate policy emission.)
	It("emits both proxy KubeArmorPolicies, owner-ref'd to the Loop, when both proxies are expected", func() {
		r.AllowUnenforced = true
		ns := setupNS("both")
		Expect(k8sClient.Create(ctx, &coxv1alpha1.AgentPolicy{
			ObjectMeta: metav1.ObjectMeta{Name: i42fTestPolicyName, Namespace: ns},
			Spec:       coxv1alpha1.AgentPolicySpec{Network: []string{i42bExternalAllow}},
		})).To(Succeed())
		loop := buildLoop("i42f-loop", ns, []string{i42fTestPolicyName})
		Expect(k8sClient.Create(ctx, loop)).To(Succeed())
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: "i42f-loop"}})
		Expect(err).NotTo(HaveOccurred())

		// Model proxy policy.
		modelKap := getKapt(ctx, ns, "coxswain-i42f-loop-proxy")
		Expect(kaptSelectorLabels(modelKap)).To(Equal(map[string]string{
			"app.kubernetes.io/component": "model-proxy",
			"coxswain.io/proxy-for":       "i42f-loop",
		}))
		Expect(kaptProcessPaths(modelKap)).To(Equal([]string{"/usr/local/bin/proxy"}))
		Expect(kaptDNSDomains(modelKap)).To(ContainElement("model.test"))

		// Egress proxy policy.
		egressKap := getKapt(ctx, ns, "coxswain-i42f-loop-egress-proxy")
		Expect(kaptSelectorLabels(egressKap)).To(Equal(map[string]string{
			"app.kubernetes.io/component":  "egress-proxy",
			"coxswain.io/egress-proxy-for": "i42f-loop",
		}))
		Expect(kaptProcessPaths(egressKap)).To(Equal([]string{"/usr/local/bin/egress-proxy"}))
		Expect(kaptDNSDomains(egressKap)).To(ContainElement("proxy.golang.org"))

		// Both owned by the Loop (GC with the Loop).
		for _, kap := range []*unstructured.Unstructured{modelKap, egressKap} {
			ownerRefs := kap.GetOwnerReferences()
			Expect(ownerRefs).NotTo(BeEmpty(), "KubeArmorPolicy %s must be owner-ref'd to the Loop", kap.GetName())
			Expect(ownerRefs[0].Name).To(Equal("i42f-loop"))
			Expect(ownerRefs[0].Kind).To(Equal("Loop"))
		}

		// The agent's C6b policy is unchanged: the coxswain-<loop> policy still
		// exists with its own selector (coxswain.io/loop), NOT a proxy label.
		agentKap := getKapt(ctx, ns, "coxswain-i42f-loop")
		Expect(kaptSelectorLabels(agentKap)).To(HaveKeyWithValue("coxswain.io/loop", "i42f-loop"))
	})

	// plan envtest 4: a Loop with no network allows -> no egress proxy
	// KubeArmorPolicy; the model proxy's KubeArmorPolicy still exists (the
	// model proxy is expected via EndpointSecretRef).
	It("emits no egress proxy KubeArmorPolicy when there are no network allows (model proxy policy stays)", func() {
		r.AllowUnenforced = true
		ns := setupNS("noallow")
		Expect(k8sClient.Create(ctx, &coxv1alpha1.AgentPolicy{
			ObjectMeta: metav1.ObjectMeta{Name: i42fTestPolicyName, Namespace: ns},
			Spec:       coxv1alpha1.AgentPolicySpec{}, // no network allows
		})).To(Succeed())
		loop := buildLoop("i42f-nol", ns, []string{i42fTestPolicyName})
		Expect(k8sClient.Create(ctx, loop)).To(Succeed())
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: "i42f-nol"}})
		Expect(err).NotTo(HaveOccurred())

		obj := &unstructured.Unstructured{}
		obj.SetGroupVersionKind(engine.KubeArmorGVK)
		err = k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "coxswain-i42f-nol-egress-proxy"}, obj)
		Expect(err).To(HaveOccurred(),
			"the egress proxy KubeArmorPolicy must NOT exist with no network allows")

		modelKap := getKapt(ctx, ns, "coxswain-i42f-nol-proxy")
		Expect(kaptSelectorLabels(modelKap)).To(HaveKeyWithValue("coxswain.io/proxy-for", "i42f-nol"))
	})

	// cleanup: a Loop that LOST its model endpoint (EndpointSecretRef removed)
	// has its model proxy KubeArmorPolicy cleaned up (only when the proxy is
	// expected does the policy exist).
	It("cleans up the model proxy KubeArmorPolicy when the model endpoint is no longer configured", func() {
		r.AllowUnenforced = true
		ns := setupNS("clean")
		Expect(k8sClient.Create(ctx, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "model-creds", Namespace: ns},
			StringData: map[string]string{"openai.api_key": "sk-test"},
		})).To(Succeed())
		loop := buildLoop("i42f-cl", ns, nil)
		Expect(k8sClient.Create(ctx, loop)).To(Succeed())
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: "i42f-cl"}})
		Expect(err).NotTo(HaveOccurred())
		getKapt(ctx, ns, "coxswain-i42f-cl-proxy")

		// Remove the model endpoint: the proxy is no longer expected.
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "i42f-cl"}, loop)).To(Succeed())
		loop.Spec.Agent.EndpointSecretRef = ""
		loop.Spec.Agent.ModelEndpoint = ""
		Expect(k8sClient.Update(ctx, loop)).To(Succeed())
		_, err = r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: "i42f-cl"}})
		Expect(err).NotTo(HaveOccurred())

		obj := &unstructured.Unstructured{}
		obj.SetGroupVersionKind(engine.KubeArmorGVK)
		err = k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "coxswain-i42f-cl-proxy"}, obj)
		Expect(err).To(HaveOccurred(),
			"the model proxy KubeArmorPolicy must be cleaned up when the model endpoint is gone")
	})
})
