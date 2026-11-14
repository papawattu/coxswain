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

// J5b step 3b (D132): the Judge proxy's NetworkPolicy (<loop>-jproxy-netpol)
// is modelled on the Loop proxy's (loop_i43_proxy_netpol_test.go pins that
// shape): INGRESS only from the Judge Job's pods on 8080 (the Judge's only
// network call is a POST to this proxy) and from the operator on the usage
// port (the same peer the Loop proxy's usage ingress uses), EGRESS only to
// the Judge's endpoint plus cluster DNS. The AGENT's endpoint is NEVER an
// egress peer of the jproxy (the jproxy serves the Judge's endpoint only).

import (
	"context"
	"testing"

	coxv1alpha1 "github.com/papawattu/coxswain/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// j3bFixture builds a Loop with the Judge enabled on its own endpoint (an IP
// literal, so modelPeer resolves to an exact ipBlock /32 peer) plus the
// agent's own, DIFFERENT endpoint (so a spec can assert the agent's endpoint
// is not an egress peer).
func j3bFixture() *coxv1alpha1.Loop {
	return &coxv1alpha1.Loop{
		ObjectMeta: metav1.ObjectMeta{Name: "j3b-loop", Namespace: "j3b-ns"},
		Spec: coxv1alpha1.LoopSpec{
			Agent: coxv1alpha1.AgentConfig{
				ModelEndpoint: "10.0.0.9:9999",
			},
			Verify: coxv1alpha1.VerifyConfig{
				Judge: &coxv1alpha1.JudgeConfig{
					Enabled:       true,
					MaxTokens:     512,
					ModelEndpoint: "10.0.0.7:9100",
				},
			},
		},
	}
}

// TestJudgeProxyNetPolName pins the jproxy NetworkPolicy name.
func TestJudgeProxyNetPolName(t *testing.T) {
	if got := judgeProxyNetPolName("loop-b"); got != "loop-b-jproxy-netpol" {
		t.Fatalf("judgeProxyNetPolName: got %q, want loop-b-jproxy-netpol", got)
	}
}

// TestJudgeProxyNetPolSelector pins the podSelector to judgeProxyLabels
// (DISJOINT from the Loop proxy's proxyLabels) and the policy types.
func TestJudgeProxyNetPolSelector(t *testing.T) {
	r := &LoopReconciler{}
	loop := j3bFixture()
	np := r.buildJudgeProxyNetPol(loop)

	want := judgeProxyLabels(loop.Name)
	if got := np.Spec.PodSelector.MatchLabels; len(got) != len(want) {
		t.Fatalf("podSelector = %v, want exactly judgeProxyLabels %v", got, want)
	}
	for k, v := range want {
		if got := np.Spec.PodSelector.MatchLabels[k]; got != v {
			t.Fatalf("podSelector[%s] = %q, want %q", k, got, v)
		}
	}
	// The DISJOINT guard: the Loop proxy's component value must NOT be the
	// jproxy's (a jproxy pod selected by the Loop proxy's netpol would be
	// fenced by the wrong spec).
	if np.Spec.PodSelector.MatchLabels["app.kubernetes.io/component"] != "judge-proxy" {
		t.Fatalf("podSelector component = %q, want judge-proxy (disjoint from model-proxy)",
			np.Spec.PodSelector.MatchLabels["app.kubernetes.io/component"])
	}
	// The jproxy's -for key is the Judge's, not the Loop proxy's.
	if _, ok := np.Spec.PodSelector.MatchLabels["coxswain.io/proxy-for"]; ok {
		t.Fatal("the jproxy podSelector must NOT carry coxswain.io/proxy-for (the Loop proxy's key)")
	}
	if _, ok := np.Spec.PodSelector.MatchLabels["coxswain.io/judge-proxy-for"]; !ok {
		t.Fatal("the jproxy podSelector must carry coxswain.io/judge-proxy-for")
	}
	foundIngress, foundEgress := false, false
	for _, pt := range np.Spec.PolicyTypes {
		if pt == networkingv1.PolicyTypeIngress {
			foundIngress = true
		}
		if pt == networkingv1.PolicyTypeEgress {
			foundEgress = true
		}
	}
	if !foundIngress || !foundEgress {
		t.Fatalf("policyTypes = %v, want Ingress and Egress", np.Spec.PolicyTypes)
	}
}

// judgeProxyIngressRuleFor returns the ingress rule admitting the given port,
// or nil.
func judgeProxyIngressRuleFor(np *networkingv1.NetworkPolicy, port int) *networkingv1.NetworkPolicyIngressRule {
	for i := range np.Spec.Ingress {
		for _, p := range np.Spec.Ingress[i].Ports {
			if p.Port.IntValue() == port {
				return &np.Spec.Ingress[i]
			}
		}
	}
	return nil
}

// TestJudgeProxyNetPolIngressJudgeJobOnly pins the 8080 ingress: the ONLY
// admitted caller is the Judge Job's pods (by their minimal -for label,
// judgeForLabel — the memory-writer / Judge peer pattern the Loop proxy's
// 8080 ingress uses) on proxyPort. The agent and the memory writer are NOT
// peers (their model calls go through the Loop's own proxy, never the jproxy).
func TestJudgeProxyNetPolIngressJudgeJobOnly(t *testing.T) {
	r := &LoopReconciler{OperatorNamespace: "coxswain-system"}
	np := r.buildJudgeProxyNetPol(j3bFixture())

	rule := judgeProxyIngressRuleFor(np, int(proxyPort))
	if rule == nil {
		t.Fatal("the jproxy netpol must carry an ingress rule on proxyPort (8080)")
	}
	if len(rule.From) != 1 {
		t.Fatalf("the 8080 ingress must admit EXACTLY one peer (the Judge Job's pods), got %d: %+v", len(rule.From), rule.From)
	}
	peer := rule.From[0]
	if peer.PodSelector == nil || peer.PodSelector.MatchLabels == nil {
		t.Fatalf("the 8080 ingress peer must be a pod selector, got %+v", peer)
	}
	// The minimal -for label peer (the memory-writer / Judge peer pattern
	// the Loop proxy's 8080 ingress uses): exactly judgeForLabel -> loopName.
	if got, want := len(peer.PodSelector.MatchLabels), 1; got != want {
		t.Fatalf("the 8080 ingress peer must be EXACTLY the minimal -for label (got %d labels: %v), want 1 label {judgeForLabel: j3b-loop}",
			got, peer.PodSelector.MatchLabels)
	}
	if peer.PodSelector.MatchLabels[judgeForLabel] != "j3b-loop" {
		t.Fatalf("the 8080 ingress peer must be {judgeForLabel: j3b-loop}, got %v",
			peer.PodSelector.MatchLabels)
	}
	// Fail-closed on the port: only TCP 8080.
	for _, p := range rule.Ports {
		if p.Protocol != nil && *p.Protocol != "TCP" {
			t.Fatalf("the 8080 ingress peer must be TCP, got %v", *p.Protocol)
		}
	}
}

// TestJudgeProxyNetPolUsageIngressOperatorOnly pins the usage-port ingress:
// the same peer the Loop proxy's usage ingress uses (the operator namespace +
// the controller-manager pod), and omitted (fail-closed) when the operator
// namespace is unset.
func TestJudgeProxyNetPolUsageIngressOperatorOnly(t *testing.T) {
	// With an operator namespace: the usage port is admitted from the
	// operator peer only.
	r := &LoopReconciler{OperatorNamespace: "coxswain-system"}
	np := r.buildJudgeProxyNetPol(j3bFixture())
	rule := judgeProxyIngressRuleFor(np, int(proxyUsagePort))
	if rule == nil {
		t.Fatal("with an operator namespace the jproxy netpol must carry a usage-port (9090) ingress rule")
	}
	if len(rule.From) != 1 {
		t.Fatalf("the usage-port ingress must admit EXACTLY one peer (the operator), got %d", len(rule.From))
	}
	peer := rule.From[0]
	if peer.NamespaceSelector == nil || peer.NamespaceSelector.MatchLabels["kubernetes.io/metadata.name"] != "coxswain-system" {
		t.Fatalf("the usage-port ingress peer must select the operator namespace, got %+v", peer.NamespaceSelector)
	}
	if peer.PodSelector == nil || peer.PodSelector.MatchLabels["control-plane"] != "controller-manager" {
		t.Fatalf("the usage-port ingress peer must select the controller-manager pod, got %+v", peer.PodSelector)
	}
	for _, p := range rule.Ports {
		if p.Port.IntValue() != int(proxyUsagePort) {
			t.Fatalf("the operator peer must be on the usage port only, got port %d", p.Port.IntValue())
		}
	}

	// Without an operator namespace: the usage ingress is omitted
	// (fail-closed — the usage endpoint is unreachable rather than
	// wide-open).
	r2 := &LoopReconciler{}
	np2 := r2.buildJudgeProxyNetPol(j3bFixture())
	if rule2 := judgeProxyIngressRuleFor(np2, int(proxyUsagePort)); rule2 != nil {
		t.Fatalf("without an operator namespace the usage-port ingress must be omitted (fail-closed), got %+v", rule2)
	}
}

// TestJudgeProxyNetPolEgressJudgeEndpointOnly pins the egress: cluster DNS
// and the Judge's endpoint (10.0.0.7/32, port 9100) ONLY — the agent's
// endpoint (10.0.0.9:9999) is not an egress peer of the jproxy.
func TestJudgeProxyNetPolEgressJudgeEndpointOnly(t *testing.T) {
	r := &LoopReconciler{}
	np := r.buildJudgeProxyNetPol(j3bFixture())

	if len(np.Spec.Egress) != 2 {
		t.Fatalf("the jproxy netpol must carry exactly 2 egress rules (DNS + the Judge endpoint), got %d: %+v",
			len(np.Spec.Egress), np.Spec.Egress)
	}

	// Egress 0: cluster DNS (the dnsPeer shape).
	dns := np.Spec.Egress[0]
	if len(dns.To) != 1 || dns.To[0].PodSelector == nil ||
		dns.To[0].PodSelector.MatchLabels["k8s-app"] != "kube-dns" {
		t.Fatalf("egress[0] must be the cluster DNS peer, got %+v", dns.To)
	}

	// Egress 1: the Judge's endpoint (an IP literal -> exact ipBlock /32).
	judge := np.Spec.Egress[1]
	if len(judge.To) != 1 || judge.To[0].IPBlock == nil {
		t.Fatalf("egress[1] must carry exactly one ipBlock peer, got %+v", judge.To)
	}
	if judge.To[0].IPBlock.CIDR != "10.0.0.7/32" {
		t.Fatalf("the Judge endpoint peer must be the ipBlock 10.0.0.7/32, got %s", judge.To[0].IPBlock.CIDR)
	}
	foundPort := false
	for _, p := range judge.Ports {
		if p.Port.IntValue() == 9100 && (p.Protocol == nil || *p.Protocol == "TCP") {
			foundPort = true
		}
	}
	if !foundPort {
		t.Fatalf("the Judge endpoint egress rule must carry port 9100/TCP, got %+v", judge.Ports)
	}

	// The agent's endpoint is NOT an egress peer anywhere in the jproxy
	// netpol (the jproxy serves the Judge's endpoint only — an egress to the
	// agent's endpoint would let the Judge's metered call hit the wrong
	// server).
	for i := range np.Spec.Egress {
		for j := range np.Spec.Egress[i].To {
			peer := np.Spec.Egress[i].To[j]
			if peer.IPBlock != nil && peer.IPBlock.CIDR == "10.0.0.9/32" {
				t.Fatalf("the agent's endpoint (10.0.0.9/32) must NOT be an egress peer of the jproxy netpol: %+v", peer)
			}
		}
	}
}

// TestJudgeProxyNetPolEgressDNSOnlyWhenEndpointInexpressible pins the
// fail-closed shape: when the Judge's endpoint cannot be expressed as a
// NetworkPolicy peer (modelPeer nil — e.g. a hostname), the egress stays
// DNS-only (the Judge's call fails rather than going wide-open).
func TestJudgeProxyNetPolEgressDNSOnlyWhenEndpointInexpressible(t *testing.T) {
	loop := j3bFixture()
	// A hostname the serviceNamespaceFromHost cannot turn into an ipBlock or
	// a cluster Service peer (see modelPeer's rules).
	loop.Spec.Verify.Judge.ModelEndpoint = "judge.example.com:9100"
	r := &LoopReconciler{}
	np := r.buildJudgeProxyNetPol(loop)
	if len(np.Spec.Egress) != 1 {
		t.Fatalf("an inexpressible Judge endpoint must leave the egress DNS-only (fail-closed), got %d rules: %+v",
			len(np.Spec.Egress), np.Spec.Egress)
	}
	if np.Spec.Egress[0].To[0].PodSelector == nil || np.Spec.Egress[0].To[0].PodSelector.MatchLabels["k8s-app"] != "kube-dns" {
		t.Fatalf("the lone egress rule must be the DNS peer, got %+v", np.Spec.Egress[0].To)
	}
}

// TestJudgeJobNetPolOwnEndpointTargetsJudgeProxy pins rule 2: with an own
// endpoint the Judge Job's NetworkPolicy egress targets the Judge proxy pods
// (judgeProxyLabels) INSTEAD of the Loop's proxy (proxy-for).
func TestJudgeJobNetPolOwnEndpointTargetsJudgeProxy(t *testing.T) {
	np := judgeNetworkPolicy(j3bFixture())
	if len(np.Spec.Egress) != 2 {
		t.Fatalf("an own-endpoint Judge Job netpol must carry 2 egress rules (jproxy + DNS), got %d: %+v",
			len(np.Spec.Egress), np.Spec.Egress)
	}
	proxyRule := np.Spec.Egress[0]
	if len(proxyRule.To) != 1 {
		t.Fatalf("the jproxy egress rule must carry exactly one peer, got %+v", proxyRule.To)
	}
	peer := proxyRule.To[0]
	if peer.PodSelector == nil {
		t.Fatalf("the jproxy egress peer must be a pod selector, got %+v", peer)
	}
	want := judgeProxyLabels("j3b-loop")
	if len(peer.PodSelector.MatchLabels) != len(want) {
		t.Fatalf("the jproxy egress peer must be EXACTLY judgeProxyLabels (got %v), want %v",
			peer.PodSelector.MatchLabels, want)
	}
	for k, v := range want {
		if peer.PodSelector.MatchLabels[k] != v {
			t.Fatalf("jproxy egress peer[%s] = %q, want %q", k, peer.PodSelector.MatchLabels[k], v)
		}
	}
	for _, p := range proxyRule.Ports {
		if p.Port.IntValue() != int(proxyPort) {
			t.Fatalf("the jproxy egress rule must carry proxyPort (8080), got %d", p.Port.IntValue())
		}
	}
	// INSTEAD: the Loop's proxy (proxy-for) must be absent from EVERY egress
	// rule (a rule to the Loop proxy would fence the Judge out of its actual
	// path — the jproxy pod does not carry proxy-for).
	for i := range np.Spec.Egress {
		for j := range np.Spec.Egress[i].To {
			peer := np.Spec.Egress[i].To[j]
			if peer.PodSelector != nil && peer.PodSelector.MatchLabels["coxswain.io/proxy-for"] == "j3b-loop" {
				t.Fatalf("an own-endpoint Judge Job netpol must NOT carry an egress to the Loop's proxy (proxy-for), got %+v", peer)
			}
		}
	}
	// Egress 1 is still the DNS rule (the jproxy Service URL is a DNS name).
	if np.Spec.Egress[1].To[0].PodSelector == nil || np.Spec.Egress[1].To[0].PodSelector.MatchLabels["k8s-app"] != "kube-dns" {
		t.Fatalf("the own-endpoint Judge Job netpol's egress[1] must be the DNS peer, got %+v", np.Spec.Egress[1].To)
	}
}

// TestJudgeJobNetPolNoOwnEndpointUnchanged pins the ELSE branch: without an
// own endpoint the Judge Job's egress is the Loop's model proxy (proxy-for) +
// DNS, and with no model endpoint at all it is fully denied.
func TestJudgeJobNetPolNoOwnEndpointUnchanged(t *testing.T) {
	// Judge enabled but no own endpoint + the Loop HAS a model endpoint: the
	// egress targets the Loop's proxy (the pre-J5b shape).
	loop := j3bFixture()
	loop.Spec.Verify.Judge.ModelEndpoint = ""
	np := judgeNetworkPolicy(loop)
	if len(np.Spec.Egress) != 2 {
		t.Fatalf("a no-own-endpoint Judge Job netpol must carry 2 egress rules (Loop proxy + DNS), got %d: %+v",
			len(np.Spec.Egress), np.Spec.Egress)
	}
	peer := np.Spec.Egress[0].To[0]
	if peer.PodSelector == nil || peer.PodSelector.MatchLabels["coxswain.io/proxy-for"] != "j3b-loop" {
		t.Fatalf("the no-own-endpoint egress must target the Loop's proxy (proxy-for), got %+v", peer)
	}

	// Judge disabled + no model endpoint: fully denied (no egress rules —
	// the deny-all fence, the memory-writer's P5b shape).
	loop2 := &coxv1alpha1.Loop{ObjectMeta: metav1.ObjectMeta{Name: "j3b-loop", Namespace: "j3b-ns"}}
	np2 := judgeNetworkPolicy(loop2)
	if len(np2.Spec.Egress) != 0 {
		t.Fatalf("a Judge with no endpoint (no model endpoint at all) must have NO egress rules (deny-all), got %+v", np2.Spec.Egress)
	}
}

// TestDeleteJudgeProxyNetPolNotFoundAndForeign pins the two non-error paths
// of deleteJudgeProxyNetworkPolicy (3b exports the pair; 3a wires the call
// into the reconcile): NotFound is fine (no error, nothing to delete), and
// a FOREIGN netpol occupying the name is left untouched (I2/I42b
// never-take-over — it is not deleted).
func TestDeleteJudgeProxyNetPolNotFoundAndForeign(t *testing.T) {
	ctx := context.Background()
	loop := j3bFixture()
	scheme := newSchemeForJudgeNetPolTest(t)

	// NotFound: no netpol at all -> nil error.
	r := &LoopReconciler{Client: fake.NewClientBuilder().WithScheme(scheme).Build()}
	if err := r.deleteJudgeProxyNetworkPolicy(ctx, loop); err != nil {
		t.Fatalf("deleteJudgeProxyNetworkPolicy on an absent netpol must be nil (NotFound is fine), got %v", err)
	}

	// Foreign netpol occupying the name: left alone (not deleted).
	foreign := &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: judgeProxyNetPolName(loop.Name), Namespace: loop.Namespace},
		Spec:       networkingv1.NetworkPolicySpec{},
	}
	r2 := &LoopReconciler{Client: fake.NewClientBuilder().WithScheme(scheme).Build()}
	if err := r2.Create(ctx, foreign); err != nil {
		t.Fatalf("create the foreign netpol: %v", err)
	}
	if err := r2.deleteJudgeProxyNetworkPolicy(ctx, loop); err != nil {
		t.Fatalf("deleteJudgeProxyNetworkPolicy on a foreign netpol must be nil (leave it alone), got %v", err)
	}
	if err := r2.Get(ctx, client.ObjectKey{Namespace: loop.Namespace, Name: foreign.Name}, &networkingv1.NetworkPolicy{}); err != nil {
		t.Fatalf("a foreign netpol occupying the name must be LEFT ALONE, got %v", err)
	}
}

// newSchemeForJudgeNetPolTest builds a scheme with the coxswain and core
// types registered (the fake client needs both for the Loop-less netpol
// create and the Get in TestDeleteJudgeProxyNetPolNotFoundAndForeign).
func newSchemeForJudgeNetPolTest(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := coxv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add coxswain to scheme: %v", err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("add core to scheme: %v", err)
	}
	if err := networkingv1.AddToScheme(scheme); err != nil {
		t.Fatalf("add networking to scheme: %v", err)
	}
	return scheme
}
