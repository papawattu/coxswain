package controller

import (
	"context"
	"strings"
	"testing"

	coxv1alpha1 "github.com/papawattu/coxswain/api/v1alpha1"
	"github.com/papawattu/coxswain/internal/policy"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

// D41d netpol test constants.
const (
	d41dNetpolNS        = "ns1"
	d41dNetpolPolicyRef = "pol"
	d41dAgentNetpol     = "lp-agent-netpol"
	d41dToolNetpolGH    = "lp-tool-gh-netpol"
	d41dToolNetpolOther = "lp-tool-other-netpol"
	d41dOtherTool       = "other"
)

// D41d: the agent's COX_TOOL_<NAME>_URL env, the tool proxy netpol rule,
// the owned+Ready gate, the foreign-object gate, and the I43 update/remove
// spec.

// d41NetpolScheme returns a runtime.Scheme for the D41d netpol tests.
func d41NetpolScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := coxv1alpha1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	if err := corev1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	if err := networkingv1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	return s
}

// newD41NetpolFakeClient returns a fake client with the D41d netpol scheme.
func newD41NetpolFakeClient(t *testing.T) client.Client {
	t.Helper()
	s := d41NetpolScheme(t)
	return fake.NewClientBuilder().WithScheme(s).Build()
}

// newD41NetpolLoop returns a Loop with a policy ref for the D41d netpol tests.
func newD41NetpolLoop() *coxv1alpha1.Loop {
	return &coxv1alpha1.Loop{
		ObjectMeta: metav1.ObjectMeta{Name: "lp", Namespace: d41dNetpolNS, UID: types.UID("lp-uid")},
		Spec: coxv1alpha1.LoopSpec{
			Goal: "test goal",
			Workspace: coxv1alpha1.Workspace{
				Repo: "https://github.com/example/repo",
			},
			PolicyRefs: []string{d41dNetpolPolicyRef},
		},
	}
}

// D41d spec 1: the agent's env carries one COX_TOOL_<NAME>_URL per tool in the
// effective policy. The URL is the tool proxy's Service FQDN (the agent
// reaches the tool proxy directly, not through the egress proxy).
func TestD41AgentEnvCOXTOOLURL(t *testing.T) {
	r := &LoopReconciler{ClusterDomain: policy.DefaultClusterDomain}
	t1 := coxv1alpha1.ToolSpec{
		Name:     "gh",
		Upstream: d41cUpstreamA,
	}
	env := r.toolEnv("lp", "ns1", t1)
	if env.Name != "COX_TOOL_GH_URL" {
		t.Fatalf("env name = %q, want COX_TOOL_GH_URL", env.Name)
	}
	wantURL := "http://lp-tool-gh.ns1.svc.cluster.local:8080"
	if env.Value != wantURL {
		t.Fatalf("COX_TOOL_GH_URL = %q, want %q", env.Value, wantURL)
	}
}

// D41d spec 2: the agent's NO_PROXY includes the tool proxy Service FQDN when
// the effective policy has tools.
func TestD41AgentNOProxyIncludesToolProxyFQDNs(t *testing.T) {
	ctx := context.Background()
	cl := newD41NetpolFakeClient(t)
	r := &LoopReconciler{Client: cl, Scheme: cl.Scheme(), ClusterDomain: policy.DefaultClusterDomain}
	loop := newD41NetpolLoop()
	ap := &coxv1alpha1.AgentPolicy{ObjectMeta: metav1.ObjectMeta{Name: d41dNetpolPolicyRef, Namespace: d41dNetpolNS}}
	ap.Spec = coxv1alpha1.AgentPolicySpec{Tools: []coxv1alpha1.ToolSpec{{Name: "gh", Upstream: d41cUpstreamA}}}
	if err := cl.Create(ctx, loop); err != nil {
		t.Fatal(err)
	}
	if err := cl.Create(ctx, ap); err != nil {
		t.Fatal(err)
	}

	noProxy := r.egressNOProxy(ctx, loop)
	if !strings.Contains(noProxy, "lp-tool-gh.ns1.svc") {
		t.Fatalf("NO_PROXY must include the tool proxy FQDN lp-tool-gh.ns1.svc; got %q", noProxy)
	}
}

// D41d spec 3: the agent's NetworkPolicy gains an egress rule per tool proxy
// (port 8080 TCP, peer selector = the tool proxy's disjoint label set).
func TestD41AgentNetpolToolProxyEgressRule(t *testing.T) {
	ctx := context.Background()
	cl := newD41NetpolFakeClient(t)
	r := &LoopReconciler{Client: cl, Scheme: cl.Scheme(), ClusterDomain: policy.DefaultClusterDomain}
	loop := newD41NetpolLoop()
	ap := &coxv1alpha1.AgentPolicy{ObjectMeta: metav1.ObjectMeta{Name: d41dNetpolPolicyRef, Namespace: d41dNetpolNS}}
	ap.Spec = coxv1alpha1.AgentPolicySpec{Tools: []coxv1alpha1.ToolSpec{{Name: "gh", Upstream: d41cUpstreamA}}}
	if err := cl.Create(ctx, loop); err != nil {
		t.Fatal(err)
	}
	if err := cl.Create(ctx, ap); err != nil {
		t.Fatal(err)
	}
	if err := r.ensureNetworkPolicy(ctx, loop); err != nil {
		t.Fatal(err)
	}

	np := &networkingv1.NetworkPolicy{}
	if err := cl.Get(ctx, client.ObjectKey{Namespace: d41dNetpolNS, Name: d41dAgentNetpol}, np); err != nil {
		t.Fatalf("expected lp-agent-netpol: %v", err)
	}
	found := false
	for _, rule := range np.Spec.Egress {
		for _, peer := range rule.To {
			if peer.PodSelector == nil {
				continue
			}
			if peer.PodSelector.MatchLabels["coxswain.io/tool-proxy-for"] == "lp" && peer.PodSelector.MatchLabels["coxswain.io/tool"] == "gh" {
				found = true
			}
		}
	}
	if !found {
		t.Fatal("agent netpol must have an egress rule with the tool proxy peer selector")
	}
}

// D41d spec 4: no tools → no extra egress rule (the agent's netpol is
// unchanged).
func TestD41AgentNetpolNoToolsNoEgressRule(t *testing.T) {
	ctx := context.Background()
	cl := newD41NetpolFakeClient(t)
	r := &LoopReconciler{Client: cl, Scheme: cl.Scheme(), ClusterDomain: policy.DefaultClusterDomain}
	loop := newD41NetpolLoop()
	ap := &coxv1alpha1.AgentPolicy{ObjectMeta: metav1.ObjectMeta{Name: d41dNetpolPolicyRef, Namespace: d41dNetpolNS}}
	ap.Spec = coxv1alpha1.AgentPolicySpec{} // no tools
	if err := cl.Create(ctx, loop); err != nil {
		t.Fatal(err)
	}
	if err := cl.Create(ctx, ap); err != nil {
		t.Fatal(err)
	}
	if err := r.ensureNetworkPolicy(ctx, loop); err != nil {
		t.Fatal(err)
	}

	np := &networkingv1.NetworkPolicy{}
	if err := cl.Get(ctx, client.ObjectKey{Namespace: d41dNetpolNS, Name: d41dAgentNetpol}, np); err != nil {
		t.Fatalf("expected lp-agent-netpol: %v", err)
	}
	for _, rule := range np.Spec.Egress {
		for _, peer := range rule.To {
			if peer.PodSelector != nil && peer.PodSelector.MatchLabels["coxswain.io/tool-proxy-for"] != "" {
				t.Fatal("agent netpol must NOT have a tool proxy egress rule when no tools")
			}
		}
	}
}

// D41d spec 5: COX_TOOL_<NAME>_URL is a reserved env name. User-supplied env
// vars with this name are dropped (the operator value wins).
func TestD41AgentEnvCOXTOOLURLReserved(t *testing.T) {
	// isToolEnv must return true for COX_TOOL_GH_URL and false for others.
	if !isToolEnv("COX_TOOL_GH_URL") {
		t.Fatal("isToolEnv(COX_TOOL_GH_URL) must be true")
	}
	if isToolEnv("COX_TOOL_") {
		t.Fatal("isToolEnv(COX_TOOL_) must be false (no name between prefix and suffix)")
	}
	if isToolEnv("MY_CUSTOM_URL") {
		t.Fatal("isToolEnv(MY_CUSTOM_URL) must be false")
	}
	if isToolEnv("COX_TOOL_GH") {
		t.Fatal("isToolEnv(COX_TOOL_GH) must be false (no _URL suffix)")
	}
}

// D41d spec 6: the tool proxy's owned+Ready gate. toolProxyGatesSuspended holds
// the sandbox Suspended while any expected tool proxy pod is not owned+Ready by
// the Loop, and releases once every expected pod is owned and Ready.
func TestD41ToolProxyOwnedReadyGate(t *testing.T) {
	ctx := context.Background()
	cl := newD41NetpolFakeClient(t)
	r := &LoopReconciler{Client: cl, Scheme: cl.Scheme(), ClusterDomain: policy.DefaultClusterDomain}
	loop := newD41NetpolLoop()
	ap := &coxv1alpha1.AgentPolicy{ObjectMeta: metav1.ObjectMeta{Name: d41dNetpolPolicyRef, Namespace: d41dNetpolNS}}
	ap.Spec = coxv1alpha1.AgentPolicySpec{Tools: []coxv1alpha1.ToolSpec{{Name: "gh", Upstream: d41cUpstreamA}}}
	if err := cl.Create(ctx, loop); err != nil {
		t.Fatal(err)
	}
	if err := cl.Create(ctx, ap); err != nil {
		t.Fatal(err)
	}

	// No tool proxy pod yet: the gate holds (fail-closed).
	if !r.toolProxyGatesSuspended(ctx, loop) {
		t.Fatal("the gate must hold while the tool proxy pod is absent (fail-closed)")
	}

	// An owned but NOT Ready pod: the gate still holds.
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "lp-tool-gh", Namespace: d41dNetpolNS}}
	if err := controllerutil.SetControllerReference(loop, pod, cl.Scheme()); err != nil {
		t.Fatal(err)
	}
	if err := cl.Create(ctx, pod); err != nil {
		t.Fatal(err)
	}
	if !r.toolProxyGatesSuspended(ctx, loop) {
		t.Fatal("the gate must hold while the tool proxy pod is owned but not Ready")
	}

	// A Ready pod: the gate releases.
	if err := cl.Get(ctx, client.ObjectKey{Namespace: d41dNetpolNS, Name: "lp-tool-gh"}, pod); err != nil {
		t.Fatal(err)
	}
	pod.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
	if err := cl.Status().Update(ctx, pod); err != nil {
		t.Fatal(err)
	}
	if r.toolProxyGatesSuspended(ctx, loop) {
		t.Fatal("the gate must release once the tool proxy pod is owned and Ready")
	}
}

// D41d spec 9: I43 update/remove (netpol half). Removing one tool from a
// multi-tool set deletes that tool's netpol on the SAME-Loop re-reconcile while
// the other tool's netpol remains (cleanupStaleToolProxyNetpols runs every
// reconcile, not only when the tool set is empty).
func TestD41ToolProxyNetpolRemovedWhenToolRemoved(t *testing.T) {
	ctx := context.Background()
	cl := newD41NetpolFakeClient(t)
	r := &LoopReconciler{Client: cl, Scheme: cl.Scheme(), ClusterDomain: policy.DefaultClusterDomain, PodCIDR: d41cPodCIDR, ServiceCIDR: d41cServiceCIDR}
	loop := newD41NetpolLoop()
	ap := &coxv1alpha1.AgentPolicy{ObjectMeta: metav1.ObjectMeta{Name: d41dNetpolPolicyRef, Namespace: d41dNetpolNS}}
	ap.Spec = coxv1alpha1.AgentPolicySpec{Tools: []coxv1alpha1.ToolSpec{
		{Name: "gh", Upstream: d41cUpstreamA},
		{Name: d41dOtherTool, Upstream: d41cUpstreamB},
	}}
	if err := cl.Create(ctx, loop); err != nil {
		t.Fatal(err)
	}
	if err := cl.Create(ctx, ap); err != nil {
		t.Fatal(err)
	}
	if err := r.ensureNetworkPolicy(ctx, loop); err != nil {
		t.Fatal(err)
	}

	// Both tool netpols exist.
	for _, name := range []string{"lp-tool-gh-netpol", "lp-tool-other-netpol"} {
		np := &networkingv1.NetworkPolicy{}
		if err := cl.Get(ctx, client.ObjectKey{Namespace: d41dNetpolNS, Name: name}, np); err != nil {
			t.Fatalf("expected tool netpol %s to exist: %v", name, err)
		}
	}

	// Remove the "other" tool from the effective policy.
	if err := cl.Get(ctx, client.ObjectKey{Namespace: d41dNetpolNS, Name: d41dNetpolPolicyRef}, ap); err != nil {
		t.Fatal(err)
	}
	ap.Spec.Tools = []coxv1alpha1.ToolSpec{{Name: "gh", Upstream: d41cUpstreamA}}
	if err := cl.Update(ctx, ap); err != nil {
		t.Fatal(err)
	}
	if err := r.ensureNetworkPolicy(ctx, loop); err != nil {
		t.Fatal(err)
	}

	// The removed tool's netpol is deleted...
	removed := &networkingv1.NetworkPolicy{}
	err := cl.Get(ctx, client.ObjectKey{Namespace: d41dNetpolNS, Name: "lp-tool-other-netpol"}, removed)
	if !apierrors.IsNotFound(err) {
		t.Fatalf("the removed tool's netpol must be deleted; got err=%v", err)
	}
	// ...and the remaining tool's netpol is untouched.
	kept := &networkingv1.NetworkPolicy{}
	if err := cl.Get(ctx, client.ObjectKey{Namespace: d41dNetpolNS, Name: "lp-tool-gh-netpol"}, kept); err != nil {
		t.Fatalf("the remaining tool's netpol must be untouched: %v", err)
	}
	if !metav1.IsControlledBy(kept, loop) {
		t.Fatal("the remaining tool's netpol must still be controller-owned by the Loop")
	}
}

// D41d spec 1: the tool proxy netpol is created with the right shape
// (PolicyTypes Ingress+Egress, ingress from agent on 8080, egress external
// with carve-outs + DNS).
func TestD41ToolProxyNetpolCreated(t *testing.T) {
	ctx := context.Background()
	cl := newD41NetpolFakeClient(t)
	r := &LoopReconciler{Client: cl, Scheme: cl.Scheme(), ClusterDomain: policy.DefaultClusterDomain, PodCIDR: d41cPodCIDR, ServiceCIDR: d41cServiceCIDR}
	loop := newD41NetpolLoop()
	ap := &coxv1alpha1.AgentPolicy{ObjectMeta: metav1.ObjectMeta{Name: d41dNetpolPolicyRef, Namespace: d41dNetpolNS}}
	ap.Spec = coxv1alpha1.AgentPolicySpec{Tools: []coxv1alpha1.ToolSpec{{Name: "gh", Upstream: d41cUpstreamA}}}
	if err := cl.Create(ctx, loop); err != nil {
		t.Fatal(err)
	}
	if err := cl.Create(ctx, ap); err != nil {
		t.Fatal(err)
	}
	if err := r.ensureNetworkPolicy(ctx, loop); err != nil {
		t.Fatal(err)
	}

	np := &networkingv1.NetworkPolicy{}
	if err := cl.Get(ctx, client.ObjectKey{Namespace: d41dNetpolNS, Name: d41dToolNetpolGH}, np); err != nil {
		t.Fatalf("expected lp-tool-gh-netpol: %v", err)
	}
	hasIngress := false
	hasEgress := false
	for _, pt := range np.Spec.PolicyTypes {
		if pt == networkingv1.PolicyTypeIngress {
			hasIngress = true
		}
		if pt == networkingv1.PolicyTypeEgress {
			hasEgress = true
		}
	}
	if !hasIngress || !hasEgress {
		t.Fatalf("PolicyTypes must include Ingress+Egress, got %v", np.Spec.PolicyTypes)
	}
}

// D41d spec 2: the except list in the tool proxy netpol egress reflects the
// reconciler's POD_CIDR and SERVICE_CIDR (not hardcoded).
func TestD41ToolProxyNetpolCIDRsFromConfig(t *testing.T) {
	ctx := context.Background()
	cl := newD41NetpolFakeClient(t)
	r := &LoopReconciler{Client: cl, Scheme: cl.Scheme(), ClusterDomain: policy.DefaultClusterDomain, PodCIDR: "192.168.100.0/24", ServiceCIDR: "172.20.0.0/16"}
	loop := newD41NetpolLoop()
	ap := &coxv1alpha1.AgentPolicy{ObjectMeta: metav1.ObjectMeta{Name: d41dNetpolPolicyRef, Namespace: d41dNetpolNS}}
	ap.Spec = coxv1alpha1.AgentPolicySpec{Tools: []coxv1alpha1.ToolSpec{{Name: "gh", Upstream: d41cUpstreamA}}}
	if err := cl.Create(ctx, loop); err != nil {
		t.Fatal(err)
	}
	if err := cl.Create(ctx, ap); err != nil {
		t.Fatal(err)
	}
	if err := r.ensureNetworkPolicy(ctx, loop); err != nil {
		t.Fatal(err)
	}

	np := &networkingv1.NetworkPolicy{}
	if err := cl.Get(ctx, client.ObjectKey{Namespace: d41dNetpolNS, Name: d41dToolNetpolGH}, np); err != nil {
		t.Fatalf("expected lp-tool-gh-netpol: %v", err)
	}
	found := false
	for _, rule := range np.Spec.Egress {
		for _, peer := range rule.To {
			if peer.IPBlock == nil || peer.IPBlock.CIDR != "0.0.0.0/0" {
				continue
			}
			found = true
			foundPod := false
			foundSvc := false
			for _, cidr := range peer.IPBlock.Except {
				if cidr == "192.168.100.0/24" {
					foundPod = true
				}
				if cidr == "172.20.0.0/16" {
					foundSvc = true
				}
			}
			if !foundPod {
				t.Fatalf("except list must contain POD_CIDR 192.168.100.0/24; got %v", peer.IPBlock.Except)
			}
			if !foundSvc {
				t.Fatalf("except list must contain SERVICE_CIDR 172.20.0.0/16; got %v", peer.IPBlock.Except)
			}
		}
	}
	if !found {
		t.Fatal("tool proxy netpol egress must include an ipBlock 0.0.0.0/0 rule")
	}
}

// D41d spec 10: the tool proxy's netpol egress allows the external world (with
// carve-outs) + DNS. It does NOT allow other tool proxies or the model proxy
// as peers.
func TestD41ToolProxyNetpolUpstreamOnly(t *testing.T) {
	ctx := context.Background()
	cl := newD41NetpolFakeClient(t)
	r := &LoopReconciler{Client: cl, Scheme: cl.Scheme(), ClusterDomain: policy.DefaultClusterDomain, PodCIDR: d41cPodCIDR, ServiceCIDR: d41cServiceCIDR}
	loop := newD41NetpolLoop()
	ap := &coxv1alpha1.AgentPolicy{ObjectMeta: metav1.ObjectMeta{Name: d41dNetpolPolicyRef, Namespace: d41dNetpolNS}}
	ap.Spec = coxv1alpha1.AgentPolicySpec{Tools: []coxv1alpha1.ToolSpec{{Name: "gh", Upstream: d41cUpstreamA}}}
	if err := cl.Create(ctx, loop); err != nil {
		t.Fatal(err)
	}
	if err := cl.Create(ctx, ap); err != nil {
		t.Fatal(err)
	}
	if err := r.ensureNetworkPolicy(ctx, loop); err != nil {
		t.Fatal(err)
	}

	np := &networkingv1.NetworkPolicy{}
	if err := cl.Get(ctx, client.ObjectKey{Namespace: d41dNetpolNS, Name: d41dToolNetpolGH}, np); err != nil {
		t.Fatalf("expected lp-tool-gh-netpol: %v", err)
	}

	// Ingress: only from agent on 8080 TCP.
	if len(np.Spec.Ingress) != 1 {
		t.Fatalf("ingress rules = %d, want 1", len(np.Spec.Ingress))
	}
	for _, from := range np.Spec.Ingress[0].From {
		if from.PodSelector == nil {
			t.Fatal("ingress peer must have a podSelector")
		}
		if from.PodSelector.MatchLabels["coxswain.io/loop"] != "lp" {
			t.Fatalf("ingress peer loop = %q, want lp", from.PodSelector.MatchLabels["coxswain.io/loop"])
		}
	}

	// Egress: must NOT include the model proxy or other tool proxies as
	// podSelector peers (only ipBlock + DNS).
	for _, rule := range np.Spec.Egress {
		for _, peer := range rule.To {
			if peer.PodSelector != nil {
				if comp, ok := peer.PodSelector.MatchLabels[policy.ComponentLabelKey]; ok {
					if comp == policy.ComponentProxyLabel || comp == policy.ComponentToolProxyLabel {
						t.Fatalf("tool proxy netpol egress must not allow peer with component %q", comp)
					}
				}
			}
		}
	}
}

// Spec 3 (no tools → no tool netpol): when the effective policy has no tools,
// no tool proxy NetworkPolicy is created.
func TestD41NoToolsNoToolNetpol(t *testing.T) {
	ctx := context.Background()
	cl := newD41NetpolFakeClient(t)
	r := &LoopReconciler{Client: cl, Scheme: cl.Scheme(), ClusterDomain: policy.DefaultClusterDomain, PodCIDR: d41cPodCIDR, ServiceCIDR: d41cServiceCIDR}
	loop := newD41NetpolLoop()
	ap := &coxv1alpha1.AgentPolicy{ObjectMeta: metav1.ObjectMeta{Name: d41dNetpolPolicyRef, Namespace: d41dNetpolNS}}
	ap.Spec = coxv1alpha1.AgentPolicySpec{} // no tools
	if err := cl.Create(ctx, loop); err != nil {
		t.Fatal(err)
	}
	if err := cl.Create(ctx, ap); err != nil {
		t.Fatal(err)
	}
	if err := r.ensureNetworkPolicy(ctx, loop); err != nil {
		t.Fatal(err)
	}

	np := &networkingv1.NetworkPolicy{}
	if err := cl.Get(ctx, client.ObjectKey{Namespace: d41dNetpolNS, Name: d41dToolNetpolGH}, np); err == nil {
		t.Fatal("no tool proxy netpol should exist when no tools")
	}
}

// Spec 10: two tools → two of everything (two netpols, two agent egress rules,
// two env entries). No cross-talk between tools.
func TestD41TwoToolsTwoOfEverything(t *testing.T) {
	ctx := context.Background()
	cl := newD41NetpolFakeClient(t)
	r := &LoopReconciler{Client: cl, Scheme: cl.Scheme(), ClusterDomain: policy.DefaultClusterDomain, PodCIDR: d41cPodCIDR, ServiceCIDR: d41cServiceCIDR}
	loop := newD41NetpolLoop()
	ap := &coxv1alpha1.AgentPolicy{ObjectMeta: metav1.ObjectMeta{Name: d41dNetpolPolicyRef, Namespace: d41dNetpolNS}}
	ap.Spec = coxv1alpha1.AgentPolicySpec{Tools: []coxv1alpha1.ToolSpec{
		{Name: "gh", Upstream: d41cUpstreamA},
		{Name: "other", Upstream: "https://api.other.com"},
	}}
	if err := cl.Create(ctx, loop); err != nil {
		t.Fatal(err)
	}
	if err := cl.Create(ctx, ap); err != nil {
		t.Fatal(err)
	}
	if err := r.ensureNetworkPolicy(ctx, loop); err != nil {
		t.Fatal(err)
	}

	// Two tool proxy netpols.
	for _, name := range []string{d41dToolNetpolGH, d41dToolNetpolOther} {
		np := &networkingv1.NetworkPolicy{}
		if err := cl.Get(ctx, client.ObjectKey{Namespace: "ns1", Name: name}, np); err != nil {
			t.Fatalf("expected %s: %v", name, err)
		}
	}

	// Agent netpol has two tool proxy egress rules.
	agentNP := &networkingv1.NetworkPolicy{}
	if err := cl.Get(ctx, client.ObjectKey{Namespace: d41dNetpolNS, Name: d41dAgentNetpol}, agentNP); err != nil {
		t.Fatalf("expected lp-agent-netpol: %v", err)
	}
	toolRules := 0
	for _, rule := range agentNP.Spec.Egress {
		for _, peer := range rule.To {
			if peer.PodSelector != nil && peer.PodSelector.MatchLabels["coxswain.io/tool-proxy-for"] == "lp" {
				toolRules++
			}
		}
	}
	if toolRules != 2 {
		t.Fatalf("agent netpol tool proxy egress rules = %d, want 2", toolRules)
	}

	// Two env entries.
	t1 := coxv1alpha1.ToolSpec{Name: "gh", Upstream: d41cUpstreamA}
	t2 := coxv1alpha1.ToolSpec{Name: "other", Upstream: "https://api.other.com"}
	env1 := r.toolEnv("lp", "ns1", t1)
	env2 := r.toolEnv("lp", "ns1", t2)
	if env1.Name == env2.Name {
		t.Fatal("two tools must produce two DIFFERENT env names")
	}
	if env1.Name != "COX_TOOL_GH_URL" {
		t.Fatalf("env1 name = %q, want COX_TOOL_GH_URL", env1.Name)
	}
	if env2.Name != "COX_TOOL_OTHER_URL" {
		t.Fatalf("env2 name = %q, want COX_TOOL_OTHER_URL", env2.Name)
	}
}
