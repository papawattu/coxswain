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
	sandboxv1beta1 "sigs.k8s.io/agent-sandbox/api/v1beta1"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

const (
	i42fTestPolicyName = "i42f-pol"
	i42fModelEndpoint  = "model.test:8443" // host:port form (the CRD rejects a URL)

	// i42f loop names (goconst: they recur across the specs below).
	i42fLoopBoth    = "i42f-loop"
	i42fLoopNoAllow = "i42f-nol"
	i42fLoopClean   = "i42f-cl"
	i42fLoopDNS     = "i42f-dns"
	i42fLoopNdots   = "i42f-nd"
	i42fLoopForeign = "i42f-fx"

	// i42fModelCredsSecret is the model-creds Secret name the I42f fixtures
	// use (goconst: it recurs across the specs below).
	i42fModelCredsSecret = "model-creds"
	// i42fModelAPIKey is the API key value in the model-creds Secret (goconst:
	// it recurs across the specs below).
	i42fModelAPIKey = "sk-test"
	// i42fModelAPIKeyName is the API key field name in the model-creds Secret
	// (goconst: it recurs across the specs below).
	i42fModelAPIKeyName = "openai.api_key"

	// unstructuredNs and unstructuredTrue (goconst: they recur across the specs
	// below).
	unstructuredNs   = "namespace"
	unstructuredTrue = "true"
)

// getKapt fetches a KubeArmorPolicy by name, unstructured.
func getKapt(ctx context.Context, ns, name string) *unstructured.Unstructured {
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(engine.KubeArmorGVK)
	Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, obj)).To(Succeed(),
		"KubeArmorPolicy %s/%s must exist", ns, name)
	return obj
}

// kaptSpec returns the policy's spec block (nested under spec in the
// unstructured object; the engine package's shared KubeArmorPolicy keys).
func kaptSpec(obj *unstructured.Unstructured) map[string]any {
	spec, _, _ := unstructured.NestedMap(obj.Object, engine.KaptSpecKey)
	return spec
}

// kaptDNSDomains returns the policy's network.matchDNSQueries domains.
func kaptDNSDomains(obj *unstructured.Unstructured) []string {
	network, _, _ := unstructured.NestedMap(kaptSpec(obj), "network")
	items, _ := network[engine.KaptMatchDNSKey].([]any)
	domains := make([]string, 0, len(items))
	for _, it := range items {
		domains = append(domains, it.(map[string]any)["domain"].(string))
	}
	return domains
}

// kaptSelectorLabels returns the policy's selector.matchLabels.
func kaptSelectorLabels(obj *unstructured.Unstructured) map[string]string {
	sel, _, _ := unstructured.NestedMap(kaptSpec(obj), engine.KaptSelectorKey)
	raw, _ := sel[engine.KaptMatchLabelsKey].(map[string]any)
	out := make(map[string]string, len(raw))
	for k, v := range raw {
		out[k] = v.(string)
	}
	return out
}

// kaptProcessPaths returns the policy's process.matchPaths paths.
func kaptProcessPaths(obj *unstructured.Unstructured) []string {
	proc, _, _ := unstructured.NestedMap(kaptSpec(obj), engine.KaptProcessKey)
	items, _ := proc[engine.KaptMatchPathsKey].([]any)
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
			Enforcer:         &engine.KubeArmorEnforcer{Client: k8sClient, ProxyFQDN: ProxyServiceFQDN, EgressProxyFQDN: EgressProxyServiceFQDN, ToolProxyFQDN: ToolProxyServiceFQDN},
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
					EndpointSecretRef: i42fModelCredsSecret,
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
		loop := buildLoop(i42fLoopBoth, ns, []string{i42fTestPolicyName})
		Expect(k8sClient.Create(ctx, loop)).To(Succeed())
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: i42fLoopBoth}})
		Expect(err).NotTo(HaveOccurred())

		// Model proxy policy.
		modelKap := getKapt(ctx, ns, "coxswain-i42f-loop-proxy")
		Expect(kaptSelectorLabels(modelKap)).To(Equal(map[string]string{
			"app.kubernetes.io/component": "model-proxy",
			"coxswain.io/proxy-for":       i42fLoopBoth,
		}))
		Expect(kaptProcessPaths(modelKap)).To(Equal([]string{"/usr/local/bin/proxy"}))
		Expect(kaptDNSDomains(modelKap)).To(ContainElement("model.test"))

		// Egress proxy policy.
		egressKap := getKapt(ctx, ns, "coxswain-i42f-loop-egress-proxy")
		Expect(kaptSelectorLabels(egressKap)).To(Equal(map[string]string{
			"app.kubernetes.io/component":  "egress-proxy",
			"coxswain.io/egress-proxy-for": i42fLoopBoth,
		}))
		Expect(kaptProcessPaths(egressKap)).To(Equal([]string{"/usr/local/bin/egress-proxy"}))
		Expect(kaptDNSDomains(egressKap)).To(ContainElement("proxy.golang.org"))

		// Both owned by the Loop (GC with the Loop).
		for _, kap := range []*unstructured.Unstructured{modelKap, egressKap} {
			ownerRefs := kap.GetOwnerReferences()
			Expect(ownerRefs).NotTo(BeEmpty(), "KubeArmorPolicy %s must be owner-ref'd to the Loop", kap.GetName())
			Expect(ownerRefs[0].Name).To(Equal(i42fLoopBoth))
			Expect(ownerRefs[0].Kind).To(Equal("Loop"))
		}

		// The agent's C6b policy is unchanged: the coxswain-<loop> policy still
		// exists with its own selector (coxswain.io/loop), NOT a proxy label.
		agentKap := getKapt(ctx, ns, "coxswain-i42f-loop")
		Expect(kaptSelectorLabels(agentKap)).To(HaveKeyWithValue("coxswain.io/loop", i42fLoopBoth))
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
		loop := buildLoop(i42fLoopNoAllow, ns, []string{i42fTestPolicyName})
		Expect(k8sClient.Create(ctx, loop)).To(Succeed())
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: i42fLoopNoAllow}})
		Expect(err).NotTo(HaveOccurred())

		obj := &unstructured.Unstructured{}
		obj.SetGroupVersionKind(engine.KubeArmorGVK)
		err = k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "coxswain-i42f-nol-egress-proxy"}, obj)
		Expect(err).To(HaveOccurred(),
			"the egress proxy KubeArmorPolicy must NOT exist with no network allows")

		modelKap := getKapt(ctx, ns, "coxswain-i42f-nol-proxy")
		Expect(kaptSelectorLabels(modelKap)).To(HaveKeyWithValue("coxswain.io/proxy-for", i42fLoopNoAllow))
	})

	// cleanup: a Loop that LOST its model endpoint (EndpointSecretRef removed)
	// has its model proxy KubeArmorPolicy cleaned up (only when the proxy is
	// expected does the policy exist).
	It("cleans up the model proxy KubeArmorPolicy when the model endpoint is no longer configured", func() {
		r.AllowUnenforced = true
		ns := setupNS("clean")
		Expect(k8sClient.Create(ctx, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: i42fModelCredsSecret, Namespace: ns},
			StringData: map[string]string{i42fModelAPIKeyName: i42fModelAPIKey},
		})).To(Succeed())
		loop := buildLoop(i42fLoopClean, ns, nil)
		Expect(k8sClient.Create(ctx, loop)).To(Succeed())
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: i42fLoopClean}})
		Expect(err).NotTo(HaveOccurred())
		getKapt(ctx, ns, "coxswain-i42f-cl-proxy")

		// Remove the model endpoint: the proxy is no longer expected.
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: i42fLoopClean}, loop)).To(Succeed())
		loop.Spec.Agent.EndpointSecretRef = ""
		loop.Spec.Agent.ModelEndpoint = ""
		Expect(k8sClient.Update(ctx, loop)).To(Succeed())
		_, err = r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: i42fLoopClean}})
		Expect(err).NotTo(HaveOccurred())

		obj := &unstructured.Unstructured{}
		obj.SetGroupVersionKind(engine.KubeArmorGVK)
		err = k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "coxswain-i42f-cl-proxy"}, obj)
		Expect(err).To(HaveOccurred(),
			"the model proxy KubeArmorPolicy must be cleaned up when the model endpoint is gone")
	})

	// review P1 (PR #28 early review, R16 D39): BOTH proxy policies must carry
	// the platform DNS allow (udp + tcp in matchProtocols). With spec.action
	// Block and a tcp-only matchProtocols, the proxies could not resolve any
	// name under KubeArmor enforcement.
	It("carries the platform DNS allow (udp+tcp) in both proxy policies' matchProtocols", func() {
		r.AllowUnenforced = true
		ns := setupNS("dns")
		Expect(k8sClient.Create(ctx, &coxv1alpha1.AgentPolicy{
			ObjectMeta: metav1.ObjectMeta{Name: i42fTestPolicyName, Namespace: ns},
			Spec:       coxv1alpha1.AgentPolicySpec{Network: []string{i42bExternalAllow}},
		})).To(Succeed())
		loop := buildLoop(i42fLoopDNS, ns, []string{i42fTestPolicyName})
		Expect(k8sClient.Create(ctx, loop)).To(Succeed())
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: i42fLoopDNS}})
		Expect(err).NotTo(HaveOccurred())

		kaptProtocols := func(obj *unstructured.Unstructured) []string {
			network, _, _ := unstructured.NestedMap(obj.Object, "spec", "network")
			items, _ := network["matchProtocols"].([]any)
			out := make([]string, 0, len(items))
			for _, it := range items {
				out = append(out, it.(map[string]any)["protocol"].(string))
			}
			return out
		}

		modelProtocols := kaptProtocols(getKapt(ctx, ns, "coxswain-i42f-dns-proxy"))
		Expect(modelProtocols).To(ContainElement("udp"),
			"the model proxy policy must allow udp (platform DNS); a tcp-only allow blocks the proxy's DNS under spec.action Block: %v", modelProtocols)
		Expect(modelProtocols).To(ContainElement("tcp"))

		egressProtocols := kaptProtocols(getKapt(ctx, ns, "coxswain-i42f-dns-egress-proxy"))
		Expect(egressProtocols).To(ContainElement("udp"),
			"the egress proxy policy must allow udp (platform DNS): %v", egressProtocols)
		Expect(egressProtocols).To(ContainElement("tcp"))
	})

	// review P1 (PR #28 early review, D39): both proxy pods must carry
	// dnsConfig ndots:1 so their resolvers send absolute names for in-cluster
	// URLs without search-suffix expansion (with the default ndots:5, the
	// first query for a bare single-label name is the search-expanded form,
	// which the allowlist must also carry — the emitter handles the expanded
	// form; ndots:1 makes the absolute name the primary one).
	It("sets dnsConfig ndots:1 on the egress-proxy and model-proxy pods", func() {
		r.AllowUnenforced = true
		ns := setupNS("ndots")
		Expect(k8sClient.Create(ctx, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: i42fModelCredsSecret, Namespace: ns},
			StringData: map[string]string{i42fModelAPIKeyName: i42fModelAPIKey},
		})).To(Succeed())
		Expect(k8sClient.Create(ctx, &coxv1alpha1.AgentPolicy{
			ObjectMeta: metav1.ObjectMeta{Name: i42fTestPolicyName, Namespace: ns},
			Spec:       coxv1alpha1.AgentPolicySpec{Network: []string{i42bExternalAllow}},
		})).To(Succeed())
		loop := buildLoop(i42fLoopNdots, ns, []string{i42fTestPolicyName})
		Expect(k8sClient.Create(ctx, loop)).To(Succeed())
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: i42fLoopNdots}})
		Expect(err).NotTo(HaveOccurred())

		assertNdots := func(podName string) {
			pod := &corev1.Pod{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: podName}, pod)).To(Succeed())
			dnsCfg := pod.Spec.DNSConfig
			Expect(dnsCfg).ToNot(BeNil(),
				"the %s pod must carry a dnsConfig (ndots:1) so in-cluster names resolve without search expansion", podName)
			found := false
			for _, opt := range dnsCfg.Options {
				if opt.Name == "ndots" && opt.Value != nil && *opt.Value == "1" {
					found = true
				}
			}
			Expect(found).To(BeTrue(),
				"the %s pod dnsConfig must set ndots=1 (search-suffix expansion breaks KubeArmor DNS allowlist matching)", podName)
		}
		assertNdots(i42fLoopNdots + "-egress-proxy")
		assertNdots(i42fLoopNdots + "-proxy")
	})

	// review P2 (PR #28 early review, R2): a FOREIGN KubeArmorPolicy occupying
	// a coxswain-<loop>[-egress-proxy|-proxy] name is NEVER overwritten
	// (I42c's createOrUpdateNP never-take-over, applied to the KubeArmorPols).
	// The reconciler sets KubeArmorPolicyConflict=True/ForeignKubeArmorPolicy,
	// holds the sandbox Suspended, and does not error-loop.
	//
	// Round 2 made this spec meaningful (it previously reconciled once with no
	// Ready proxy pods, so the D35a/I42b gates already held it Suspended
	// regardless of the KubeArmorPolicy conflict): (1) mark the model-proxy and
	// egress-proxy pods Ready so the D35a/I42b gates pass and the KubeArmor
	// gate is the one holding it Suspended; (2) reconcile twice, asserting
	// Suspended after each; (3) delete the foreign policy, reconcile, and assert
	// our policy is created with our controller ref, the condition is Resolved,
	// and the sandbox goes Running.
	It("does not overwrite a foreign egress-proxy KubeArmorPolicy; holds the sandbox Suspended across reconciles; resolves on deletion", func() {
		r.AllowUnenforced = true
		ns := setupNS("foreign")
		Expect(k8sClient.Create(ctx, &coxv1alpha1.AgentPolicy{
			ObjectMeta: metav1.ObjectMeta{Name: i42fTestPolicyName, Namespace: ns},
			Spec:       coxv1alpha1.AgentPolicySpec{Network: []string{i42bExternalAllow}},
		})).To(Succeed())

		// Pre-create a FOREIGN KubeArmorPolicy occupying the egress proxy's
		// name (a different owner; the Loop's owner ref is absent).
		foreignName := "coxswain-" + i42fLoopForeign + "-egress-proxy"
		foreign := &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "security.kubearmor.com/v1",
			"kind":       "KubeArmorPolicy",
			"metadata": map[string]any{
				"name":         foreignName,
				unstructuredNs: ns,
				"labels":       map[string]any{"external": unstructuredTrue},
			},
			"spec": map[string]any{"action": "Audit"},
		}}
		Expect(k8sClient.Create(ctx, foreign)).To(Succeed())

		loop := buildLoop(i42fLoopForeign, ns, []string{i42fTestPolicyName})
		Expect(k8sClient.Create(ctx, loop)).To(Succeed())

		// First reconcile: creates the sandbox + proxies. The D35a/I42b gates
		// hold it Suspended (the proxy pods are not Ready yet).
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: i42fLoopForeign}})
		Expect(err).NotTo(HaveOccurred(),
			"a foreign KubeArmorPolicy must not error-loop the reconcile (the sandbox is held Suspended instead)")

		// Mark the model-proxy and egress-proxy pods Ready so the D35a/I42b
		// gates pass. With both proxies Ready and no KubeArmor conflict, the
		// sandbox would be Running; the ONLY thing holding it Suspended is the
		// order-independent KubeArmorPolicy gate (foreign egress-proxy policy).
		for _, podName := range []string{i42fLoopForeign + "-proxy", i42fLoopForeign + "-egress-proxy"} {
			pod := &corev1.Pod{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: podName}, pod)).To(Succeed(),
				"proxy pod %s must exist", podName)
			pod.Status.Conditions = []corev1.PodCondition{{
				Type:   corev1.PodReady,
				Status: corev1.ConditionTrue,
			}}
			Expect(k8sClient.Status().Update(ctx, pod)).To(Succeed(),
				"mark %s Ready", podName)
		}

		// Second reconcile: both proxies are Ready, so the D35a/I42b gates
		// pass; the KubeArmorPolicy gate must hold it Suspended (fail-closed:
		// the egress-proxy policy is the foreign one, not the one the operator
		// built).
		_, err = r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: i42fLoopForeign}})
		Expect(err).NotTo(HaveOccurred())

		sb := &sandboxv1beta1.Sandbox{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: sandboxName(i42fLoopForeign)}, sb)).To(Succeed())
		Expect(sb.Spec.OperatingMode).To(Equal(sandboxv1beta1.SandboxOperatingModeSuspended),
			"the sandbox must be Suspended while a foreign KubeArmorPolicy occupies the name, even with both proxy pods Ready")

		// Third reconcile: the gate is order-independent (it reads the live
		// KubeArmorPolicy objects in ensureSandbox, which runs before
		// applyEffectivePolicyAndConditions), so a second pass must NOT flip it
		// back to Running. This is the flap the I42c round-3 fix removed.
		_, err = r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: i42fLoopForeign}})
		Expect(err).NotTo(HaveOccurred())
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: sandboxName(i42fLoopForeign)}, sb)).To(Succeed())
		Expect(sb.Spec.OperatingMode).To(Equal(sandboxv1beta1.SandboxOperatingModeSuspended),
			"the sandbox must STAY Suspended on the second reconcile (no flap back to Running)")

		// The foreign object is left untouched (spec + label intact, no
		// owner ref added).
		got := &unstructured.Unstructured{}
		got.SetGroupVersionKind(engine.KubeArmorGVK)
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: foreignName}, got)).To(Succeed())
		Expect(got.GetOwnerReferences()).To(BeEmpty(),
			"the foreign KubeArmorPolicy must not be owner-ref'd to the Loop")
		lbl, _, _ := unstructured.NestedMap(got.Object, "metadata", "labels")
		Expect(lbl).To(HaveKey("external"),
			"the foreign KubeArmorPolicy must be left untouched (its labels are intact)")

		// The conflict condition is set (same pattern as NetworkPolicyConflict).
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: i42fLoopForeign}, loop)).To(Succeed())
		var conflict *metav1.Condition
		for i := range loop.Status.Conditions {
			if loop.Status.Conditions[i].Type == "KubeArmorPolicyConflict" {
				conflict = &loop.Status.Conditions[i]
			}
		}
		Expect(conflict).ToNot(BeNil(), "KubeArmorPolicyConflict condition must be set")
		Expect(conflict.Status).To(Equal(metav1.ConditionTrue))
		Expect(conflict.Reason).To(Equal("ForeignKubeArmorPolicy"))

		// Delete the foreign policy. The next reconcile creates our egress-proxy
		// policy (with our controller ref) and clears the condition to Resolved;
		// with both proxies Ready and no conflict, the sandbox goes Running.
		Expect(k8sClient.Delete(ctx, foreign)).To(Succeed())
		_, err = r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: i42fLoopForeign}})
		Expect(err).NotTo(HaveOccurred())

		// Our egress-proxy policy now exists with our controller ref.
		ourKap := getKapt(ctx, ns, foreignName)
		resolvedLoop := &coxv1alpha1.Loop{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: i42fLoopForeign}, resolvedLoop)).To(Succeed())
		Expect(metav1.IsControlledBy(ourKap, resolvedLoop)).To(BeTrue(),
			"our egress-proxy KubeArmorPolicy must carry the Loop's controller ref")

		// The condition is Resolved (cleared by the hadKaptConflict path).
		var resolvedConflict *metav1.Condition
		for i := range resolvedLoop.Status.Conditions {
			if resolvedLoop.Status.Conditions[i].Type == "KubeArmorPolicyConflict" {
				resolvedConflict = &resolvedLoop.Status.Conditions[i]
			}
		}
		Expect(resolvedConflict).ToNot(BeNil(), "KubeArmorPolicyConflict condition must still be present")
		Expect(resolvedConflict.Status).To(Equal(metav1.ConditionFalse),
			"KubeArmorPolicyConflict must be cleared to False once the foreign policy is gone")
		Expect(resolvedConflict.Reason).To(Equal("Resolved"))

		// The sandbox goes Running (both proxies Ready, no conflict).
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: sandboxName(i42fLoopForeign)}, sb)).To(Succeed())
		Expect(sb.Spec.OperatingMode).To(Equal(sandboxv1beta1.SandboxOperatingModeRunning),
			"the sandbox must go Running once the foreign policy is deleted and the conflict is resolved")
	})

	// I43 same-Loop: a same-Loop input change must UPDATE the existing
	// model-proxy KubeArmorPolicy on re-reconcile (createOrUpdateKapt
	// re-asserts the desired spec against the live object), not just create
	// I43 same-Loop: the model-proxy KubeArmorPolicy is built from
	// spec.agent.modelEndpoint (the DNS allowlist is the endpoint's host),
	// so a same-Loop endpoint change must UPDATE the EXISTING policy's
	// matchDNSQueries (createOrUpdateKapt re-asserts the desired spec
	// against the live object), not just create it. The endpoint host is the
	// input the KAPT depends on; a create-only enforcer would leave the
	// stale domain (the old host) on the live policy.
	//
	// NOTE: spec.agent.modelEndpoint is CEL-validated immutable (`self ==
	// oldSelf || self == ''`), and the envtest API server enforces the CRD
	// CEL rules for ALL clients (typed and unstructured), so a host change
	// on the same Loop is rejected at admission. This spec exercises the
	// update path by bypassing the API server: it tampers the live KAPT
	// object's matchDNSQueries (simulating a "pre-rule object" that does not
	// match the desired spec) and then re-reconciles the same Loop, asserting
	// the controller's createOrUpdateKapt restores the desired spec. This is
	// the "pre-rule objects / CRD drift" case the controller must handle. If
	// the controller does NOT restore the tampered KAPT, that is a real bug
	// — report it, do not fix it (per the handoff).
	It("restores a tampered model-proxy KubeArmorPolicy on the same Loop (I43 same-Loop, drift correction)", func() {
		r.AllowUnenforced = true
		ns := setupNS("i43same")
		const loopName = "i43-kap"
		const initialHost = "model.test"
		const tamperedHost = "model2.test"
		Expect(k8sClient.Create(ctx, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: i42fModelCredsSecret, Namespace: ns},
			StringData: map[string]string{i42fModelAPIKeyName: i42fModelAPIKey},
		})).To(Succeed())
		Expect(k8sClient.Create(ctx, &coxv1alpha1.AgentPolicy{
			ObjectMeta: metav1.ObjectMeta{Name: i42fTestPolicyName, Namespace: ns},
			Spec:       coxv1alpha1.AgentPolicySpec{Network: []string{i42bExternalAllow}},
		})).To(Succeed())
		loop := buildLoop(loopName, ns, []string{i42fTestPolicyName})
		loop.Spec.Agent.ModelEndpoint = initialHost + ":8000"
		Expect(k8sClient.Create(ctx, loop)).To(Succeed())
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: loopName}})
		Expect(err).NotTo(HaveOccurred())

		// The model proxy KubeArmorPolicy exists with the initial endpoint's
		// domain in matchDNSQueries.
		modelKap := getKapt(ctx, ns, "coxswain-"+loopName+"-proxy")
		Expect(kaptDNSDomains(modelKap)).To(ContainElement(initialHost),
			"the initial model-proxy policy must allow the initial endpoint host")

		// TAMPER the live KAPT out-of-band: change matchDNSQueries to the
		// TAMPERED host's domain (model2.test), simulating a "pre-rule
		// object" that the controller must re-assert on the next reconcile.
		// The controller's createOrUpdateKapt must restore the DESIRED spec
		// (initialHost's domain) — a create-only enforcer would leave the
		// tampered domain.
		Expect(unstructured.SetNestedField(modelKap.Object,
			[]any{map[string]any{"domain": tamperedHost}},
			"spec", "network", "matchDNSQueries")).To(Succeed())
		Expect(k8sClient.Update(ctx, modelKap)).To(Succeed())

		// Reconcile the SAME Loop: the controller must restore the tampered
		// KAPT to its desired state (initialHost's domain in matchDNSQueries).
		_, err = r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: loopName}})
		Expect(err).NotTo(HaveOccurred())

		// The EXISTING model proxy KubeArmorPolicy was RESTORED in place
		// (I43: the update path ran against the live object — a create-only
		// enforcer would leave the tampered domain): matchDNSQueries contains
		// the initial host (the desired state) and NOT the tampered domain,
		// the policy is still owned by the Loop, and udp+tcp are still
		// present (the platform DNS allow).
		modelKap = getKapt(ctx, ns, "coxswain-"+loopName+"-proxy")
		domains := kaptDNSDomains(modelKap)
		Expect(domains).To(ContainElement(initialHost),
			"the restored model-proxy KubeArmorPolicy must contain the DESIRED endpoint host (the controller re-asserted the live object)")
		Expect(domains).ToNot(ContainElement(tamperedHost),
			"the restored model-proxy KubeArmorPolicy must NOT contain the tampered domain (the controller corrected the drift)")
		Expect(kaptSelectorLabels(modelKap)).To(HaveKeyWithValue("coxswain.io/proxy-for", loopName),
			"the restored policy must still select this Loop's model proxy pod")
		ownerRefs := modelKap.GetOwnerReferences()
		Expect(ownerRefs).NotTo(BeEmpty())
		Expect(ownerRefs[0].Name).To(Equal(loopName))
		// udp+tcp are still present (the platform DNS allow — the proxy must
		// be able to resolve the endpoint host under spec.action Block).
		kaptProtocols := func(obj *unstructured.Unstructured) []string {
			network, _, _ := unstructured.NestedMap(obj.Object, "spec", "network")
			items, _ := network["matchProtocols"].([]any)
			out := make([]string, 0, len(items))
			for _, it := range items {
				out = append(out, it.(map[string]any)["protocol"].(string))
			}
			return out
		}
		proto := kaptProtocols(modelKap)
		Expect(proto).To(ContainElement("udp"), "the platform DNS allow (udp) must still be present")
		Expect(proto).To(ContainElement("tcp"), "the platform DNS allow (tcp) must still be present")
	})

	It("updates the egress-proxy KubeArmorPolicy on the same Loop when a network allow is added (I43 same-Loop)", func() {
		r.AllowUnenforced = true
		ns := setupNS("i43eg")
		const loopName = "i43-eg-kap"
		Expect(k8sClient.Create(ctx, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: i42fModelCredsSecret, Namespace: ns},
			StringData: map[string]string{i42fModelAPIKeyName: i42fModelAPIKey},
		})).To(Succeed())
		const secondAllow = "second.example"
		Expect(k8sClient.Create(ctx, &coxv1alpha1.AgentPolicy{
			ObjectMeta: metav1.ObjectMeta{Name: i42fTestPolicyName, Namespace: ns},
			Spec:       coxv1alpha1.AgentPolicySpec{Network: []string{i42bExternalAllow}},
		})).To(Succeed())
		loop := buildLoop(loopName, ns, []string{i42fTestPolicyName})
		Expect(k8sClient.Create(ctx, loop)).To(Succeed())
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: loopName}})
		Expect(err).NotTo(HaveOccurred())

		// The egress proxy KubeArmorPolicy exists with the initial allow's
		// domain (proxy.golang.org:443 -> domain proxy.golang.org).
		egressKap := getKapt(ctx, ns, "coxswain-"+loopName+"-egress-proxy")
		Expect(kaptDNSDomains(egressKap)).To(HaveLen(1),
			"the initial egress-proxy policy must carry exactly the one allowed domain")
		Expect(kaptDNSDomains(egressKap)).To(ContainElement("proxy.golang.org"))

		// Add a second network allow to the SAME Loop's referenced
		// AgentPolicy and re-reconcile.
		ap := &coxv1alpha1.AgentPolicy{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: i42fTestPolicyName}, ap)).To(Succeed())
		ap.Spec.Network = append(ap.Spec.Network, secondAllow)
		Expect(k8sClient.Update(ctx, ap)).To(Succeed())
		_, err = r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: loopName}})
		Expect(err).NotTo(HaveOccurred())

		// The EXISTING egress-proxy KubeArmorPolicy is updated in place: the
		// new domain is present alongside the old one (I43: the update path,
		// not just creation).
		egressKap = getKapt(ctx, ns, "coxswain-"+loopName+"-egress-proxy")
		Expect(kaptDNSDomains(egressKap)).To(HaveLen(2),
			"the existing egress-proxy KubeArmorPolicy must gain the new domain on the same-Loop reconcile")
		Expect(kaptDNSDomains(egressKap)).To(ContainElement(secondAllow),
			"the added network allow must be present in the updated policy")
		ownerRefs := egressKap.GetOwnerReferences()
		Expect(ownerRefs).NotTo(BeEmpty())
		Expect(ownerRefs[0].Name).To(Equal(loopName))
	})
})
