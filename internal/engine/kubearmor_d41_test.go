package engine

import (
	"strings"
	"testing"

	"github.com/papawattu/coxswain/internal/policy"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// d41EngineUpstreamA is the upstream host used in the D41 engine Kapt tests.
const d41EngineUpstreamA = "api.github.com"

// D41d: the tool proxy KubeArmorPolicy (the per-tool inner fence) and the
// tool proxy FQDNs in the agent's DNS allowlist.

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
	items, _ := proc["matchPaths"].([]any)
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
	dnsItems, _ := net["matchDNSQueries"].([]any)
	foundUpstream := false
	for _, item := range dnsItems {
		m, _ := item.(map[string]any)
		domain, _ := m["domain"].(string)
		if domain == d41EngineUpstreamA {
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
	dnsItems, _ := net["matchDNSQueries"].([]any)
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
		Allows:    []policy.Allow{{Type: policy.AllowNetwork, Match: "api.github.com"}},
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
		items, _ := net[key].([]any)
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
		Allows:    []policy.Allow{{Type: policy.AllowNetwork, Match: "api.github.com"}},
	})
	obj := EmitKubeArmorPolicyWithToolFQDNs("lp", "ns1", ep, nil)

	net, _, _ := unstructured.NestedMap(obj.Object, "spec", "network")
	if net == nil {
		return // no network block is fine when there are no allows
	}
	for _, key := range []string{"matchDomains", "matchDNSQueries"} {
		items, _ := net[key].([]any)
		for _, item := range items {
			m, _ := item.(map[string]any)
			if str, _ := m["domain"].(string); strings.Contains(str, "-tool-") {
				t.Fatalf("agent Kapt must NOT include tool proxy FQDNs when no tools; got %q", str)
			}
		}
	}
}
