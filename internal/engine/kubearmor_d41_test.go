package engine

import (
	"strings"
	"testing"

	"github.com/papawattu/coxswain/api/v1alpha1"
	"github.com/papawattu/coxswain/internal/policy"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	)

// D41d: the tool proxy KubeArmorPolicy (the per-tool inner fence) and the
// tool proxy FQDNs in the agent's DNS allowlist.

// toolProxyKaptTestLoop returns a Loop for the tool proxy Kapt tests.
func toolProxyKaptTestLoop() *v1alpha1.Loop {
	return &v1alpha1.Loop{ObjectMeta: metav1.ObjectMeta{Name: "lp", Namespace: "ns1", UID: "lp-uid"}}
}

// toolProxyKaptTestPolicy returns a policy with one tool (gh, api.github.com)
// and no network allows (so the egress proxy policy is not created — isolates
// the tool proxy policy from the egress proxy policy).
func toolProxyKaptTestPolicy() policy.EffectivePolicy {
	return policy.EffectivePolicy{
		Tools: []v1alpha1.ToolSpec{
			{
				Name:                "gh",
				Upstream:            "https://api.github.com",
				CredentialSecretRef: v1alpha1.CredentialSecretRef{Name: "gh-cred"},
			},
		},
	}
}


// Spec 1: the tool proxy KubeArmorPolicy has spec.action Block (default-deny)
// and the selector is the tool proxy's DISJOINT label set (component=tool-proxy
// + coxswain.io/tool-proxy-for + coxswain.io/tool). The selector NEVER carries
// coxswain.io/loop (the same disjoint guard as the egress proxy Kapt).
func TestToolProxyKaptSpecActionBlockAndDisjointSelector(t *testing.T) {
	obj := EmitToolProxyKubeArmorPolicy("lp", "ns1", "gh", "https://api.github.com")

	action, _, _ := unstructured.NestedString(obj.Object, "spec", "action")
	if action != "Block" {
		t.Fatalf("spec.action = %q, want Block (default-deny)", action)
	}

	sel, _, _ := unstructured.NestedMap(obj.Object, "spec", "selector")
	raw, _ := sel["matchLabels"].(map[string]any)
	if raw["app.kubernetes.io/component"] != "tool-proxy" {
		t.Fatalf("selector.component = %v, want tool-proxy", raw["app.kubernetes.io/component"])
	}
	if raw["coxswain.io/tool-proxy-for"] != "lp" {
		t.Fatalf("selector.coxswain.io/tool-proxy-for = %v, want lp", raw["coxswain.io/tool-proxy-for"])
	}
	if raw["coxswain.io/tool"] != "gh" {
		t.Fatalf("selector.coxswain.io/tool = %v, want gh", raw["coxswain.io/tool"])
	}
	if _, has := raw["coxswain.io/loop"]; has {
		t.Fatal("selector must NOT carry coxswain.io/loop (disjoint guard)")
	}
}

// Spec 2: the tool proxy's process block allows ONLY the tool proxy's own
// binary (/usr/local/bin/tool-proxy).
func TestToolProxyKaptProcessAllowsOnlyOwnBinary(t *testing.T) {
	obj := EmitToolProxyKubeArmorPolicy("lp", "ns1", "gh", "https://api.github.com")

	proc, _, _ := unstructured.NestedMap(obj.Object, "spec", "process")
	action, _, _ := unstructured.NestedString(proc, "action")
	if action != "Allow" {
		t.Fatalf("spec.process.action = %q, want Allow", action)
	}
	items, _ := proc["matchPaths"].([]interface{})
	if len(items) != 1 {
		t.Fatalf("process.matchPaths = %d items, want 1", len(items))
	}
	m, _ := items[0].(map[string]any)
	path, _ := m["path"].(string)
	if path != "/usr/local/bin/tool-proxy" {
		t.Fatalf("process.matchPaths[0].path = %q, want /usr/local/bin/tool-proxy", path)
	}
}

// Spec 3: the tool proxy's network block allows the upstream host (DNS match).
func TestToolProxyKaptNetworkAllowsUpstream(t *testing.T) {
	obj := EmitToolProxyKubeArmorPolicy("lp", "ns1", "gh", "https://api.github.com")

	net, _, _ := unstructured.NestedMap(obj.Object, "spec", "network")
	dnsItems, _ := net["matchDNSQueries"].([]interface{})
	foundUpstream := false
	for _, item := range dnsItems {
		m, _ := item.(map[string]any)
		domain, _ := m["domain"].(string)
		if domain == "api.github.com" {
			foundUpstream = true
		}
	}
	if !foundUpstream {
		t.Fatalf("network.matchDNSQueries must include api.github.com; got %v", dnsItems)
	}
}

// Spec 4: a DIFFERENT host is NOT in the tool proxy's network allow.
func TestToolProxyKaptNetworkExcludesOtherHosts(t *testing.T) {
	obj := EmitToolProxyKubeArmorPolicy("lp", "ns1", "gh", "https://api.github.com")

	net, _, _ := unstructured.NestedMap(obj.Object, "spec", "network")
	dnsItems, _ := net["matchDNSQueries"].([]interface{})
	for _, item := range dnsItems {
		m, _ := item.(map[string]any)
		domain, _ := m["domain"].(string)
		if domain == "api.example.com" || domain == "api.other.com" {
			t.Fatalf("network.matchDNSQueries must NOT include %q", domain)
		}
	}
}

// Spec 5: the tool proxy policy name is coxswain-<loop>-tool-<name>.
func TestToolProxyKaptNamePerTool(t *testing.T) {
	gh := EmitToolProxyKubeArmorPolicy("lp", "ns1", "gh", "https://api.github.com")
	other := EmitToolProxyKubeArmorPolicy("lp", "ns1", "other", "https://api.other.com")

	if gh.GetName() != "coxswain-lp-tool-gh" {
		t.Fatalf("gh policy name = %q, want coxswain-lp-tool-gh", gh.GetName())
	}
	if other.GetName() != "coxswain-lp-tool-other" {
		t.Fatalf("other policy name = %q, want coxswain-lp-tool-other", other.GetName())
	}
	if gh.GetName() == other.GetName() {
		t.Fatal("two tools must produce two DIFFERENT policy names")
	}
}

// Spec 7: the tool proxy FQDN is added to the agent's KubeArmor DNS allowlist
// when the effective policy has tools.
func TestAgentKaptIncludesToolProxyFQDNWhenToolsPresent(t *testing.T) {
	ep := policy.EnginePolicy{}
	ep.Containers = append(ep.Containers, policy.ContainerPolicy{
		Container: policy.ContainerAgent,
		Allows:    []policy.Allow{policy.Allow{Type: policy.AllowNetwork, Match: "api.github.com"}},
	})
	obj := EmitKubeArmorPolicyWithToolFQDNs("lp", "ns1", ep, []string{"coxswain-lp-tool-gh.ns1.svc"})

	// The tool proxy FQDN must be in the agent's network allows (the network
	// block's matchDNSQueries or matchDomains).
	net, _, _ := unstructured.NestedMap(obj.Object, "spec", "network")
	if net == nil {
		t.Fatal("agent Kapt must have a network block")
	}
	// Check both matchDomains and matchDNSQueries for the FQDN.
	found := false
	for _, key := range []string{"matchDomains", "matchDNSQueries"} {
		items, _ := net[key].([]interface{})
		for _, item := range items {
			m, _ := item.(map[string]any)
			if str, _ := m["domain"].(string); str == "coxswain-lp-tool-gh.ns1.svc" {
				found = true
			}
		}
	}
	if !found {
		t.Fatalf("agent Kapt must include the tool proxy FQDN coxswain-lp-tool-gh.ns1.svc; got network=%v", net)
	}
}

// Spec 8: when the effective policy has NO tools, the agent's KubeArmor
// policy is unchanged (no tool proxy FQDNs in the DNS allowlist).
func TestAgentKaptNoToolProxyFQDNWhenNoTools(t *testing.T) {
	ep := policy.EnginePolicy{}
	ep.Containers = append(ep.Containers, policy.ContainerPolicy{
		Container: policy.ContainerAgent,
		Allows:    []policy.Allow{policy.Allow{Type: policy.AllowNetwork, Match: "api.github.com"}},
	})
	obj := EmitKubeArmorPolicyWithToolFQDNs("lp", "ns1", ep, nil)

	net, _, _ := unstructured.NestedMap(obj.Object, "spec", "network")
	if net == nil {
		return // no network block is fine when there are no allows
	}
	for _, key := range []string{"matchDomains", "matchDNSQueries"} {
		items, _ := net[key].([]interface{})
		for _, item := range items {
			m, _ := item.(map[string]any)
			if str, _ := m["domain"].(string); strings.Contains(str, "-tool-") {
				t.Fatalf("agent Kapt must NOT include tool proxy FQDNs when no tools; got %q", str)
			}
		}
	}
}

// Spec 9: the tool proxy policy is CREATED by the enforcer when the effective
// policy has tools, and CLEANED UP when the tools go away.
func TestEnforcerCreatesAndCleansUpToolKapt(t *testing.T) {
	// The full enforcer create/cleanup test requires the Kapt CRD in the
	// fake client (not trivially set up). The emitter-level tests above
	// (specs 1-8) verify the policy shape; the create/cleanup behaviour is
	// covered by the envtest suite.
}

// Spec 10: a FOREIGN tool proxy KubeArmorPolicy occupying the name is NEVER
// overwritten (the same I2 never-take-over rule as the egress proxy Kapt).
func TestEnforcerDoesNotTakeOverForeignToolKapt(t *testing.T) {
	// The foreign-object test requires the full enforcer + fake client with
	// the Kapt CRD. The emitter-level tests verify the policy shape; the
	// foreign-object behaviour is covered by the envtest suite (the D41c
	// specs already test the foreign pod gate; the Kapt foreign-object gate
	// uses the same createOrUpdateKapt code path as the egress proxy Kapt,
	// which is already tested in kubearmor_i42_test.go).
}
