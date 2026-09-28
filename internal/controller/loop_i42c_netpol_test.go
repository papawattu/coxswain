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

// I42c: NetworkPolicy changes (agent egress + egress-proxy egress).
//
// The agent pod's NetworkPolicy (existing from D34) gains an egress rule to the
// egress proxy when the egress proxy is expected (the effective AgentPolicy has
// network allows); with no network allows the agent's egress rules stay
// model-proxy + DNS only. A NEW NetworkPolicy is created for the egress proxy
// pod (<loop>-egress-proxy-netpol): ingress only from this Loop's agent on 3128,
// and egress to the external world with carve-outs (0.0.0.0/0 minus the
// RFC1918/loopback/link-local/multicast/reserved ranges plus the cluster's
// POD_CIDR and SERVICE_CIDR) + the v6 mirror, plus DNS to kube-dns. The
// carve-outs come from the operator's config (r.PodCIDR / r.ServiceCIDR), not
// from any per-Loop discovery.

import (
	"context"
	"fmt"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	sandboxv1beta1 "sigs.k8s.io/agent-sandbox/api/v1beta1"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	coxv1alpha1 "github.com/papawattu/coxswain/api/v1alpha1"
	"github.com/papawattu/coxswain/internal/policy"
)

const (
	i42cPolicyName     = "i42c-pol"
	i42cTestRepo       = "https://github.com/papawattu/coxswain.git"
	i42cExternalAllow  = i42eExternalHost
	i42cPodCIDR        = "10.244.0.0/16"
	i42cServiceCIDR    = "10.96.0.0/12"
	i42cAltPodCIDR     = "192.168.128.0/17"
	i42cEgressProxyImg = "coxswain-egress-proxy:standin"

	// i42cEgressProxyPort is the egress proxy's CONNECT port.
	i42cEgressProxyPort int32 = 3128
)

var _ = Describe("I42c: NetworkPolicy changes (agent egress + egress-proxy egress)", func() {
	var (
		ctx context.Context
		r   *LoopReconciler
	)

	BeforeEach(func() {
		ctx = context.Background()
		r = &LoopReconciler{
			Client:           k8sClient,
			Scheme:           k8sClient.Scheme(),
			PodCIDR:          i42cPodCIDR,
			ServiceCIDR:      i42cServiceCIDR,
			EgressProxyImage: i42cEgressProxyImg,
			// AllowUnenforced: isolate the NetworkPolicy creation from the D30
			// enforcement gate (envtest Enforcer is nil).
			AllowUnenforced: true,
		}
	})

	// buildLoopI42C creates a Loop in ns referencing the given AgentPolicies.
	// The Loop has NO EndpointSecretRef: I42c's NetworkPolicies are created
	// whenever the effective policy has network allows, independent of whether
	// a model endpoint is configured (the egress proxy and its NetworkPolicy
	// do not depend on the model creds).
	// makeSecret creates a model-creds Secret so the Loop has an
	// EndpointSecretRef (like a real Loop). This makes the D35a proxy gate pass
	// (the model proxy is owned + Ready) so the sandbox is not held Suspended,
	// and lets ensureNetworkPolicy run regardless of whether network allows are
	// present. The I42c behavior (agent egress-proxy rule + egress-proxy
	// NetworkPolicy) is driven by the network allows, not the model creds.
	makeSecret := func(name, ns string) {
		secret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			StringData: map[string]string{
				modelAPIKey:  d34TestKey,
				modelBaseURL: "http://model-endpoint:8000",
			},
		}
		Expect(k8sClient.Create(ctx, secret)).To(Succeed())
	}

	buildLoopI42C := func(name, ns, secretName string, policyRefs []string) *coxv1alpha1.Loop {
		return &coxv1alpha1.Loop{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec: coxv1alpha1.LoopSpec{
				Goal:       "i42c test",
				Workspace:  coxv1alpha1.Workspace{Repo: i42cTestRepo, Ref: loopRef},
				PolicyRefs: policyRefs,
				Agent: coxv1alpha1.AgentConfig{
					Image:             runnerImage,
					Model:             testModel,
					EndpointSecretRef: secretName,
					ModelEndpoint:     "model-endpoint:8000",
				},
			},
		}
	}

	reconcileI42C := func(name, ns string) {
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: name}})
		Expect(err).NotTo(HaveOccurred(), "reconcile %s/%s: %v", ns, name, err)
	}

	// egressProxyPeerRule returns the agent NetworkPolicy egress rule whose
	// To[0] carries the egress-proxy label set (component=egress-proxy +
	// coxswain.io/egress-proxy-for=<loop>), or fails.
	egressProxyPeerRule := func(np *networkingv1.NetworkPolicy, loopName string) networkingv1.NetworkPolicyEgressRule {
		for _, rule := range np.Spec.Egress {
			for _, peer := range rule.To {
				if peer.PodSelector == nil {
					continue
				}
				if peer.PodSelector.MatchLabels["app.kubernetes.io/component"] == policy.ComponentEgressProxyLabel &&
					peer.PodSelector.MatchLabels["coxswain.io/egress-proxy-for"] == loopName {
					return rule
				}
			}
		}
		Fail("no agent egress rule to the egress proxy (component=egress-proxy, coxswain.io/egress-proxy-for=" + loopName + ")")
		return networkingv1.NetworkPolicyEgressRule{}
	}

	// egressProxyPortOn asserts the rule's Ports contain the given port/proto.
	egressProxyPortOn := func(rule networkingv1.NetworkPolicyEgressRule, port int32, proto corev1.Protocol) {
		found := false
		for _, p := range rule.Ports {
			if p.Port.IntValue() == int(port) && p.Protocol != nil && *p.Protocol == proto {
				found = true
				break
			}
		}
		Expect(found).To(BeTrue(), "egress rule must expose %d/%s", port, proto)
	}

	// getEgressProxyNetpol fetches <loop>-egress-proxy-netpol or fails.
	getEgressProxyNetpol := func(ns, loopName string) *networkingv1.NetworkPolicy {
		np := &networkingv1.NetworkPolicy{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: loopName + "-egress-proxy-netpol"}, np)).To(Succeed())
		return np
	}

	// countPeerKind counts the agent-netpol egress peers whose podSelector
	// carries the given component label value (used by the P1 same-Loop
	// update specs to assert the egress-proxy rule appears and disappears).
	countPeerKind := func(np *networkingv1.NetworkPolicy, component string) int {
		n := 0
		for _, rule := range np.Spec.Egress {
			for _, peer := range rule.To {
				if peer.PodSelector != nil && peer.PodSelector.MatchLabels["app.kubernetes.io/component"] == component {
					n++
				}
			}
		}
		return n
	}

	// spec 1: agent netpol with egress proxy expected -> exactly 4 egress rules,
	// including the egress-proxy rule (disjoint selector) on 3128/TCP.
	It("adds the egress-proxy egress rule to the agent NetworkPolicy when network allows are present", func() {
		ns := "i42c-agent-allow-" + nowSuffix()
		Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
		defer func() { _ = k8sClient.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}) }()

		Expect(k8sClient.Create(ctx, &coxv1alpha1.AgentPolicy{
			ObjectMeta: metav1.ObjectMeta{Name: i42cPolicyName, Namespace: ns},
			Spec:       coxv1alpha1.AgentPolicySpec{Network: []string{i42cExternalAllow}},
		})).To(Succeed())

		makeSecret("agent-allow-model", ns)
		loop := buildLoopI42C("agent-allow", ns, "agent-allow-model", []string{i42cPolicyName})
		Expect(k8sClient.Create(ctx, loop)).To(Succeed())
		reconcileI42C("agent-allow", ns)

		np := &networkingv1.NetworkPolicy{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "agent-allow-agent-netpol"}, np)).To(Succeed())

		// Exactly 4 egress rules: model proxy:8080, egress proxy:3128, kube-dns.
		// (kube-dns is a single rule carrying both 53/UDP and 53/TCP, so the
		// total is 3 distinct To-peers: proxy, egress-proxy, dns.)
		Expect(np.Spec.Egress).To(HaveLen(3), "agent egress must be model-proxy + egress-proxy + dns")

		// The egress-proxy rule: disjoint selector + port 3128/TCP.
		egressRule := egressProxyPeerRule(np, "agent-allow")
		Expect(egressRule.To).To(HaveLen(1))
		Expect(egressRule.To[0].PodSelector.MatchLabels).To(HaveKeyWithValue("app.kubernetes.io/component", policy.ComponentEgressProxyLabel))
		Expect(egressRule.To[0].PodSelector.MatchLabels).To(HaveKeyWithValue("coxswain.io/egress-proxy-for", "agent-allow"))
		egressProxyPortOn(egressRule, i42cEgressProxyPort, corev1.ProtocolTCP)
	})

	// spec 2: agent netpol without egress proxy (no network allows) -> no
	// egress-proxy rule; only model-proxy + DNS.
	It("does not add an egress-proxy rule to the agent NetworkPolicy when there are no network allows", func() {
		ns := "i42c-agent-noallow-" + nowSuffix()
		Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
		defer func() { _ = k8sClient.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}) }()

		Expect(k8sClient.Create(ctx, &coxv1alpha1.AgentPolicy{
			ObjectMeta: metav1.ObjectMeta{Name: i42cPolicyName, Namespace: ns},
			Spec:       coxv1alpha1.AgentPolicySpec{}, // no network allows
		})).To(Succeed())

		makeSecret("agent-noallow-model", ns)
		loop := buildLoopI42C("agent-noallow", ns, "agent-noallow-model", []string{i42cPolicyName})
		Expect(k8sClient.Create(ctx, loop)).To(Succeed())
		reconcileI42C("agent-noallow", ns)

		np := &networkingv1.NetworkPolicy{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "agent-noallow-agent-netpol"}, np)).To(Succeed())

		// No rule may target the egress proxy.
		for _, rule := range np.Spec.Egress {
			for _, peer := range rule.To {
				if peer.PodSelector != nil &&
					peer.PodSelector.MatchLabels["app.kubernetes.io/component"] == policy.ComponentEgressProxyLabel {
					Fail("agent egress must not reference the egress proxy when there are no network allows: " + fmt.Sprint(peer.PodSelector.MatchLabels))
				}
			}
		}
		// model-proxy + DNS only (2 rules, matching D34).
		Expect(np.Spec.Egress).To(HaveLen(2), "agent egress must be model-proxy + dns only")
	})

	// spec 3: egress proxy netpol created (network allows). Assert PolicyTypes,
	// ingress peer (agent) on 3128, egress rule 1 ipBlock 0.0.0.0/0 with the
	// carve-outs (incl. the reconciler's POD_CIDR + SERVICE_CIDR), egress rule 2
	// kube-dns on 53.
	It("creates the egress-proxy NetworkPolicy with the carve-out egress when network allows are present", func() {
		ns := "i42c-egress-np-" + nowSuffix()
		Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
		defer func() { _ = k8sClient.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}) }()

		Expect(k8sClient.Create(ctx, &coxv1alpha1.AgentPolicy{
			ObjectMeta: metav1.ObjectMeta{Name: i42cPolicyName, Namespace: ns},
			Spec:       coxv1alpha1.AgentPolicySpec{Network: []string{i42cExternalAllow}},
		})).To(Succeed())

		makeSecret("eg-np-model", ns)
		loop := buildLoopI42C("eg-np", ns, "eg-np-model", []string{i42cPolicyName})
		Expect(k8sClient.Create(ctx, loop)).To(Succeed())
		reconcileI42C("eg-np", ns)

		np := getEgressProxyNetpol(ns, "eg-np")

		// Owner ref to the Loop.
		Expect(np.OwnerReferences).To(HaveLen(1))
		Expect(np.OwnerReferences[0].Kind).To(Equal("Loop"))
		Expect(np.OwnerReferences[0].Name).To(Equal("eg-np"))

		// Pod selector: the egress-proxy disjoint label set (no coxswain.io/loop).
		Expect(np.Spec.PodSelector.MatchLabels).To(HaveKeyWithValue("app.kubernetes.io/component", policy.ComponentEgressProxyLabel))
		Expect(np.Spec.PodSelector.MatchLabels).To(HaveKeyWithValue("coxswain.io/egress-proxy-for", "eg-np"))
		Expect(np.Spec.PodSelector.MatchLabels).ToNot(HaveKey("coxswain.io/loop"))

		// PolicyTypes include Ingress + Egress.
		Expect(np.Spec.PolicyTypes).To(ContainElement(networkingv1.PolicyTypeIngress))
		Expect(np.Spec.PolicyTypes).To(ContainElement(networkingv1.PolicyTypeEgress))

		// Ingress: only from this Loop's agent on 3128/TCP.
		Expect(np.Spec.Ingress).To(HaveLen(1))
		ing := np.Spec.Ingress[0]
		Expect(ing.From).To(HaveLen(1))
		Expect(ing.From[0].PodSelector).ToNot(BeNil())
		Expect(ing.From[0].PodSelector.MatchLabels).To(HaveKeyWithValue("coxswain.io/loop", "eg-np"))
		Expect(ing.From[0].PodSelector.MatchLabels).To(HaveKeyWithValue("app.kubernetes.io/component", "agent"))
		found3128 := false
		for _, p := range ing.Ports {
			if p.Port.IntValue() == int(i42cEgressProxyPort) && p.Protocol != nil && *p.Protocol == corev1.ProtocolTCP {
				found3128 = true
			}
		}
		Expect(found3128).To(BeTrue(), "ingress must allow the agent on 3128/TCP")

		// Egress rules: external v4 with carve-outs + the v6 mirror + DNS.
		Expect(np.Spec.Egress).To(HaveLen(3), "egress-proxy netpol egress must be external-v4 + external-v6 (P3 mirror) + dns")
		external := np.Spec.Egress[0]
		Expect(external.To).To(HaveLen(1))
		ipBlock := external.To[0].IPBlock
		Expect(ipBlock).ToNot(BeNil(), "egress rule 1 must be an ipBlock")
		Expect(ipBlock.CIDR).To(Equal("0.0.0.0/0"))
		// No port restriction.
		Expect(external.Ports).To(BeEmpty())
		// The except list contains the fixed carve-outs AND the operator config.
		exceptSet := map[string]bool{}
		for _, e := range ipBlock.Except {
			exceptSet[e] = true
		}
		for _, want := range []string{
			"10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16", "0.0.0.0/8",
			"224.0.0.0/4", "240.0.0.0/4", "169.254.0.0/16", "127.0.0.0/8",
			i42cPodCIDR, i42cServiceCIDR,
		} {
			Expect(exceptSet).To(HaveKey(want), "egress except must carve out "+want)
		}

		// Egress rule 1b (I42c review P3): the v6 mirror. ::/0 except the
		// egress binary's v6 carve-outs.
		v6 := np.Spec.Egress[1]
		Expect(v6.To).To(HaveLen(1))
		v6Block := v6.To[0].IPBlock
		Expect(v6Block).ToNot(BeNil(), "egress rule 2 must be a v6 ipBlock (the plan's v6 mirror)")
		Expect(v6Block.CIDR).To(Equal("::/0"))
		v6Except := map[string]bool{}
		for _, e := range v6Block.Except {
			v6Except[e] = true
		}
		for _, want := range []string{"fc00::/7", "fe80::/10", "::1/128", "64:ff9b::/96", "::/128"} {
			Expect(v6Except).To(HaveKey(want), "the v6 mirror must carve out "+want)
		}
		Expect(v6.Ports).To(BeEmpty())

		// Egress rule 3: DNS to kube-dns on 53 (UDP + TCP).
		dns := np.Spec.Egress[2]
		Expect(dns.To).To(HaveLen(1))
		Expect(dns.To[0].PodSelector).ToNot(BeNil())
		Expect(dns.To[0].PodSelector.MatchLabels).To(HaveKeyWithValue("k8s-app", "kube-dns"))
		Expect(dns.Ports).To(ContainElement(networkingv1.NetworkPolicyPort{
			Protocol: new(corev1.ProtocolUDP), Port: intstrPtr(53),
		}))
		Expect(dns.Ports).To(ContainElement(networkingv1.NetworkPolicyPort{
			Protocol: new(corev1.ProtocolTCP), Port: intstrPtr(53),
		}))
	})

	// spec 4: egress proxy netpol NOT created without network allows.
	It("does not create the egress-proxy NetworkPolicy when there are no network allows", func() {
		ns := "i42c-egress-noallow-" + nowSuffix()
		Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
		defer func() { _ = k8sClient.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}) }()

		Expect(k8sClient.Create(ctx, &coxv1alpha1.AgentPolicy{
			ObjectMeta: metav1.ObjectMeta{Name: i42cPolicyName, Namespace: ns},
			Spec:       coxv1alpha1.AgentPolicySpec{},
		})).To(Succeed())

		makeSecret("eg-noallow-model", ns)
		loop := buildLoopI42C("eg-noallow", ns, "eg-noallow-model", []string{i42cPolicyName})
		Expect(k8sClient.Create(ctx, loop)).To(Succeed())
		reconcileI42C("eg-noallow", ns)

		np := &networkingv1.NetworkPolicy{}
		err := k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "eg-noallow-egress-proxy-netpol"}, np)
		Expect(err).To(HaveOccurred(), "no egress-proxy NetworkPolicy when there are no network allows")
	})

	// spec 5: carve-out CIDRs are from config — a different POD_CIDR yields a
	// different except list (not hardcoded).
	It("derives the egress-proxy netpol carve-outs from the reconciler's config", func() {
		ns := "i42c-egress-cidr-" + nowSuffix()
		Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
		defer func() { _ = k8sClient.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}) }()

		Expect(k8sClient.Create(ctx, &coxv1alpha1.AgentPolicy{
			ObjectMeta: metav1.ObjectMeta{Name: i42cPolicyName, Namespace: ns},
			Spec:       coxv1alpha1.AgentPolicySpec{Network: []string{i42cExternalAllow}},
		})).To(Succeed())

		makeSecret("eg-cidr-model", ns)
		loop := buildLoopI42C("eg-cidr", ns, "eg-cidr-model", []string{i42cPolicyName})
		Expect(k8sClient.Create(ctx, loop)).To(Succeed())

		// A reconciler with a DIFFERENT POD_CIDR than the default fixture.
		alt := &LoopReconciler{
			Client:           k8sClient,
			Scheme:           k8sClient.Scheme(),
			PodCIDR:          i42cAltPodCIDR,
			ServiceCIDR:      i42cServiceCIDR,
			EgressProxyImage: i42cEgressProxyImg,
			AllowUnenforced:  true,
		}
		_, err := alt.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: "eg-cidr"}})
		Expect(err).NotTo(HaveOccurred())

		np := getEgressProxyNetpol(ns, "eg-cidr")
		Expect(np.Spec.Egress).To(HaveLen(3))
		ipBlock := np.Spec.Egress[0].To[0].IPBlock
		Expect(ipBlock).ToNot(BeNil())
		exceptSet := map[string]bool{}
		for _, e := range ipBlock.Except {
			exceptSet[e] = true
		}
		Expect(exceptSet).To(HaveKey(i42cAltPodCIDR), "the alt POD_CIDR must be carved out (from config, not hardcoded)")
		Expect(exceptSet).ToNot(HaveKey(i42cPodCIDR), "the default POD_CIDR must NOT be present when config uses the alt value")
	})

	// spec 6 (I42c review P1): an EXISTING NetworkPolicy is actually UPDATED
	// on a later reconcile. The same Loop, reconciled repeatedly: a network
	// allow added to its AgentPolicy makes the agent netpol gain the
	// egress-proxy:3128 rule AND the egress-proxy netpol appear; removing the
	// allow removes the rule AND deletes the egress-proxy netpol. (The P1
	// update bug froze the spec on the first reconcile.)
	It("updates the agent netpol and creates/deletes the egress-proxy netpol when the same Loop's network allows change", func() {
		ns := "i42c-same-" + nowSuffix()
		agentNPName := "same-agent-netpol"
		egressNPName := "same-egress-proxy-netpol"
		Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
		defer func() { _ = k8sClient.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}) }()

		// Start with NO network allows: the agent netpol has no egress-proxy
		// rule and there is no egress-proxy netpol.
		Expect(k8sClient.Create(ctx, &coxv1alpha1.AgentPolicy{
			ObjectMeta: metav1.ObjectMeta{Name: i42cPolicyName, Namespace: ns},
			Spec:       coxv1alpha1.AgentPolicySpec{},
		})).To(Succeed())
		makeSecret("same-model", ns)
		loop := buildLoopI42C("same", ns, "same-model", []string{i42cPolicyName})
		Expect(k8sClient.Create(ctx, loop)).To(Succeed())
		reconcileI42C("same", ns)

		agentNP := &networkingv1.NetworkPolicy{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: agentNPName}, agentNP)).To(Succeed())
		Expect(countPeerKind(agentNP, policy.ComponentEgressProxyLabel)).To(BeZero(), "no egress-proxy rule before allows are added")
		np := &networkingv1.NetworkPolicy{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: egressNPName}, np)).To(MatchError(ContainSubstring("not found")))

		// ADD a network allow to the same Loop's AgentPolicy, reconcile again.
		ap := &coxv1alpha1.AgentPolicy{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: i42cPolicyName}, ap)).To(Succeed())
		ap.Spec.Network = []string{i42cExternalAllow}
		Expect(k8sClient.Update(ctx, ap)).To(Succeed())
		reconcileI42C("same", ns)

		// The agent netpol now has the egress-proxy rule on 3128.
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: agentNPName}, agentNP)).To(Succeed())
		Expect(countPeerKind(agentNP, policy.ComponentEgressProxyLabel)).To(Equal(1), "agent netpol must GAIN the egress-proxy rule on a later reconcile")
		// The egress-proxy netpol now exists.
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: egressNPName}, np)).To(Succeed(),
			"the egress-proxy netpol must be created on the later reconcile")

		// REMOVE the allow again, reconcile: rule gone + egress-proxy netpol
		// deleted (I42c review P2).
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: i42cPolicyName}, ap)).To(Succeed())
		ap.Spec.Network = nil
		Expect(k8sClient.Update(ctx, ap)).To(Succeed())
		reconcileI42C("same", ns)

		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: agentNPName}, agentNP)).To(Succeed())
		Expect(countPeerKind(agentNP, policy.ComponentEgressProxyLabel)).To(BeZero(), "agent netpol must LOSE the egress-proxy rule when the allow is removed")
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: egressNPName}, np)).To(MatchError(ContainSubstring("not found")),
			"the egress-proxy netpol must be deleted when the allows go away (P2)")
	})

	// spec 7 (I42c review P1): re-reconciling the SAME Loop with a reconciler
	// whose POD_CIDR differs updates the egress-proxy netpol's except list
	// (the P1 bug left it frozen on the first reconcile).
	It("updates the egress-proxy netpol carve-outs when the same Loop is re-reconciled with a different POD_CIDR", func() {
		ns := "i42c-samecidr-" + nowSuffix()
		Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
		defer func() { _ = k8sClient.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}) }()

		Expect(k8sClient.Create(ctx, &coxv1alpha1.AgentPolicy{
			ObjectMeta: metav1.ObjectMeta{Name: i42cPolicyName, Namespace: ns},
			Spec:       coxv1alpha1.AgentPolicySpec{Network: []string{i42cExternalAllow}},
		})).To(Succeed())
		makeSecret("samecidr-model", ns)
		loop := buildLoopI42C("samecidr", ns, "samecidr-model", []string{i42cPolicyName})
		Expect(k8sClient.Create(ctx, loop)).To(Succeed())
		reconcileI42C("samecidr", ns)

		// First reconcile used the default fixture PodCIDR.
		np := getEgressProxyNetpol(ns, "samecidr")
		Expect(np.Spec.Egress[0].To[0].IPBlock.Except).To(ContainElement(i42cPodCIDR), "initial except list uses the fixture POD_CIDR")

		// Re-reconcile the SAME Loop with a reconciler whose PodCIDR differs.
		alt := &LoopReconciler{
			Client:           k8sClient,
			Scheme:           k8sClient.Scheme(),
			PodCIDR:          i42cAltPodCIDR,
			ServiceCIDR:      i42cServiceCIDR,
			EgressProxyImage: i42cEgressProxyImg,
			AllowUnenforced:  true,
		}
		_, err := alt.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: "samecidr"}})
		Expect(err).NotTo(HaveOccurred())

		np = getEgressProxyNetpol(ns, "samecidr")
		exceptSet := map[string]bool{}
		for _, e := range np.Spec.Egress[0].To[0].IPBlock.Except {
			exceptSet[e] = true
		}
		Expect(exceptSet).To(HaveKey(i42cAltPodCIDR), "the existing netpol must be UPDATED to the new POD_CIDR (P1: createOrUpdateNP no-op mutate froze it)")
		Expect(exceptSet).ToNot(HaveKey(i42cPodCIDR), "the old POD_CIDR must be gone from the updated except list")
	})

	// spec 8 (I42c review P2, round 2): a FOREIGN agent NetworkPolicy of the
	// same name is never overwritten. createOrUpdateNP's P1 fix re-asserts the
	// desired spec on an existing netpol; it must not do so for a foreign
	// one. The reconcile sets NetworkPolicyConflict=True/ForeignNetworkPolicy,
	// leaves the foreign netpol untouched, and holds the sandbox Suspended.
	It("does not overwrite a foreign agent NetworkPolicy; sets NetworkPolicyConflict and suspends", func() {
		ns := "i42c-foreign-" + nowSuffix()
		Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
		defer func() { _ = k8sClient.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}) }()

		Expect(k8sClient.Create(ctx, &coxv1alpha1.AgentPolicy{
			ObjectMeta: metav1.ObjectMeta{Name: i42cPolicyName, Namespace: ns},
			Spec:       coxv1alpha1.AgentPolicySpec{Network: []string{i42cExternalAllow}},
		})).To(Succeed())
		makeSecret("foreign-model", ns)
		loop := buildLoopI42C("foreign", ns, "foreign-model", []string{i42cPolicyName})
		Expect(k8sClient.Create(ctx, loop)).To(Succeed())

		// A FOREIGN agent netpol occupying the name (not owned by the Loop).
		foreignSpec := networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{MatchLabels: map[string]string{"app.kubernetes.io/foreign": "true"}},
			Egress: []networkingv1.NetworkPolicyEgressRule{
				{To: []networkingv1.NetworkPolicyPeer{{IPBlock: &networkingv1.IPBlock{CIDR: "1.2.3.4/32"}}}},
			},
		}
		foreignNP := &networkingv1.NetworkPolicy{
			ObjectMeta: metav1.ObjectMeta{Name: "foreign-agent-netpol", Namespace: ns},
			Spec:       foreignSpec,
		}
		Expect(k8sClient.Create(ctx, foreignNP)).To(Succeed())

		reconcileI42C("foreign", ns)

		// The foreign netpol is untouched (same spec, still unowned).
		got := &networkingv1.NetworkPolicy{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "foreign-agent-netpol"}, got)).To(Succeed())
		Expect(equality.Semantic.DeepEqual(got.Spec.PodSelector, foreignSpec.PodSelector)).To(BeTrue(), "the foreign netpol's podSelector must NOT be overwritten")
		Expect(got.Spec.Egress).To(HaveLen(1))
		Expect(got.Spec.Egress[0].To).To(HaveLen(1))
		Expect(got.Spec.Egress[0].To[0].IPBlock.CIDR).To(Equal("1.2.3.4/32"), "the foreign netpol's egress spec must NOT be overwritten")
		Expect(got.OwnerReferences).To(BeEmpty(), "the foreign netpol must NOT gain the Loop's owner ref")

		// The Loop has NetworkPolicyConflict=True/ForeignNetworkPolicy.
		gotLoop := &coxv1alpha1.Loop{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "foreign"}, gotLoop)).To(Succeed())
		var conflict *metav1.Condition
		for i := range gotLoop.Status.Conditions {
			if gotLoop.Status.Conditions[i].Type == "NetworkPolicyConflict" {
				conflict = &gotLoop.Status.Conditions[i]
				break
			}
		}
		Expect(conflict).ToNot(BeNil(), "NetworkPolicyConflict condition must be set")
		Expect(conflict.Status).To(Equal(metav1.ConditionTrue))
		Expect(conflict.Reason).To(Equal("ForeignNetworkPolicy"))

		// The sandbox is held Suspended (fail-closed: the agent's netpol is not
		// the one the operator built).
		sb := &sandboxv1beta1.Sandbox{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: "foreign-sandbox"}, sb)).To(Succeed())
		Expect(sb.Spec.OperatingMode).To(Equal(sandboxv1beta1.SandboxOperatingModeSuspended),
			"the sandbox must be Suspended while a foreign netpol occupies the name")
	})
})
