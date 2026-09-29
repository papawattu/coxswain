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

package engine

// I42f (docs/TDD-PLAN-PHASE1.md section I42f, D35 part 2): the operator emits
// a KubeArmorPolicy for EACH operator-owned proxy pod (the egress proxy and the
// model proxy), in addition to the existing agent-pod policy (C6b). These are
// the inner fence that catches a bug in the proxy's application-level
// enforcement.
//
// Test list (plan envtests 1-2; 3-4 live in
// internal/controller/loop_i42_kubearmor_test.go):
//   1. Egress proxy KubeArmor policy shape.
//   2. Model proxy KubeArmor policy shape.
//   3. Both proxy policies allow platform DNS (udp+tcp) — the R16 D39 fix
//      (PR #28 early review P1): with spec.action Block and matchProtocols
//      tcp-only, both proxies would be unable to resolve ANY name.
//   4. A bare single-label modelEndpoint gets the search-expanded
//      <host>.<ns>.svc.cluster.local form on the allowlist (kept alongside the
//      bare form): the proxy's resolver queries the expanded name first.
//   (edge) a policy with no process/network/file allows emits only the
//          selector + action Block.
//   (edge) the emitted selectors never carry coxswain.io/loop (the agent
//          policy's selector key): a stray key would bind the agent's rules to
//          the proxy pods.

import (
	"slices"
	"testing"
)

const (
	i42fLoop    = "i42f-loop"
	i42fNS      = "i42f-ns"
	i42fAllow   = "proxy.golang.org:443"
	i42fModelEP = "api.openai.com:443"
)

// plan envtest 1: the egress proxy policy's shape. Selector = the egress
// proxy's disjoint labels (component + egress-proxy-for, NEVER
// coxswain.io/loop); process.matchPaths = only the egress proxy binary's
// absolute path in its image (cmd/egress-proxy/Dockerfile:
// /usr/local/bin/egress-proxy); network = the effective AgentPolicy network
// allows (DNS + protocols); spec.action Block.
func TestEmitEgressProxyKubeArmorPolicyShape(t *testing.T) {
	obj := EmitEgressProxyKubeArmorPolicy(i42fLoop, i42fNS, []string{i42fAllow})
	if obj.GetAPIVersion() != "security.kubearmor.com/v1" || obj.GetKind() != "KubeArmorPolicy" {
		t.Fatalf("wrong apiVersion/kind: %s %s", obj.GetAPIVersion(), obj.GetKind())
	}
	if obj.GetName() != "coxswain-"+i42fLoop+"-egress-proxy" || obj.GetNamespace() != i42fNS {
		t.Fatalf("metadata wrong: %s/%s", obj.GetNamespace(), obj.GetName())
	}
	spec := obj.Object["spec"].(map[string]any)
	if spec["action"] != "Block" {
		t.Fatalf("spec.action must be Block (default-deny), got %v", spec["action"])
	}

	// Selector: the egress proxy's disjoint labels only.
	labels := spec["selector"].(map[string]any)["matchLabels"].(map[string]any)
	if labels["app.kubernetes.io/component"] != "egress-proxy" || labels["coxswain.io/egress-proxy-for"] != i42fLoop {
		t.Fatalf("selector must match component=egress-proxy + egress-proxy-for=%s, got %v", i42fLoop, labels)
	}
	if _, has := labels["coxswain.io/loop"]; has {
		t.Fatalf("the egress proxy selector must NOT carry coxswain.io/loop (the agent policy's selector key): %v", labels)
	}

	// process.matchPaths: exactly one path-only item for the egress binary.
	proc := spec["process"].(map[string]any)
	if proc["action"] != "Allow" {
		t.Fatalf("process action must be Allow, got %v", proc["action"])
	}
	paths := matchPathItems(t, proc["matchPaths"])
	if !slices.Equal(paths, []string{"/usr/local/bin/egress-proxy"}) {
		t.Fatalf("process.matchPaths must be exactly [{path: /usr/local/bin/egress-proxy}], got %v", paths)
	}

	// network: the effective allows -> DNS domain + tcp protocol.
	net := spec["network"].(map[string]any)
	if net["action"] != "Allow" {
		t.Fatalf("network action must be Allow, got %v", net["action"])
	}
	if !slices.Contains(matchDomainItems(t, net["matchDNSQueries"]), "proxy.golang.org") {
		t.Fatalf("matchDNSQueries must include proxy.golang.org, got %v", net["matchDNSQueries"])
	}
	if !slices.Contains(matchProtocolItems(t, net["matchProtocols"]), "tcp") {
		t.Fatalf("matchProtocols must include tcp, got %v", net["matchProtocols"])
	}
	if _, has := spec["file"]; has {
		t.Fatalf("the egress proxy policy must not carry a file block, got %v", spec["file"])
	}
}

// plan envtest 2: the model proxy policy's shape. Selector = proxyLabels
// (component + proxy-for); process = only the model proxy binary; network =
// the model endpoint (the single host the proxy dials).
func TestEmitModelProxyKubeArmorPolicyShape(t *testing.T) {
	obj := EmitModelProxyKubeArmorPolicy(i42fLoop, i42fNS, i42fModelEP)
	spec := obj.Object["spec"].(map[string]any)
	if spec["action"] != "Block" {
		t.Fatalf("spec.action must be Block, got %v", spec["action"])
	}

	labels := spec["selector"].(map[string]any)["matchLabels"].(map[string]any)
	if labels["app.kubernetes.io/component"] != "model-proxy" || labels["coxswain.io/proxy-for"] != i42fLoop {
		t.Fatalf("selector must match component=model-proxy + proxy-for=%s, got %v", i42fLoop, labels)
	}
	if _, has := labels["coxswain.io/loop"]; has {
		t.Fatalf("the model proxy selector must NOT carry coxswain.io/loop: %v", labels)
	}

	proc := spec["process"].(map[string]any)
	paths := matchPathItems(t, proc["matchPaths"])
	// cmd/proxy-standin/Dockerfile: /usr/local/bin/proxy.
	if !slices.Equal(paths, []string{"/usr/local/bin/proxy"}) {
		t.Fatalf("process.matchPaths must be exactly [{path: /usr/local/bin/proxy}], got %v", paths)
	}

	net := spec["network"].(map[string]any)
	if !slices.Contains(matchDomainItems(t, net["matchDNSQueries"]), "api.openai.com") {
		t.Fatalf("matchDNSQueries must include the model endpoint host api.openai.com, got %v", net["matchDNSQueries"])
	}
	if !slices.Contains(matchProtocolItems(t, net["matchProtocols"]), "tcp") {
		t.Fatalf("matchProtocols must include tcp, got %v", net["matchProtocols"])
	}
}

// edge: a proxy with no network allows still emits the policy (the process
// fence is the point of the inner fence) — selector + process + action Block,
// no network block.
func TestEmitEgressProxyKubeArmorPolicyNoNetworkAllows(t *testing.T) {
	obj := EmitEgressProxyKubeArmorPolicy(i42fLoop, i42fNS, nil)
	spec := obj.Object["spec"].(map[string]any)
	if spec["action"] != "Block" {
		t.Fatalf("spec.action must be Block, got %v", spec["action"])
	}
	if _, ok := spec["process"]; !ok {
		t.Fatal("process block must exist even with no network allows")
	}
	if _, ok := spec["network"]; ok {
		t.Fatalf("a proxy with no network allows must not emit a network block, got %v", spec["network"])
	}
}

// review P1 (PR #28 early review): the platform DNS allow (the same
// policy.DNSAllow constant the agent policy gets via policy.Translate,
// splitNetworkAllows' special case "dns/udp+tcp") must be in BOTH proxy
// policies' matchProtocols — udp and tcp. Without udp, spec.action Block
// denies the proxies' DNS lookups and neither proxy can resolve anything
// (every CONNECT / model call fails).
func TestEmitProxyPoliciesAllowPlatformDNS(t *testing.T) {
	t.Run("egress proxy policy allows udp+tcp", func(t *testing.T) {
		obj := EmitEgressProxyKubeArmorPolicy(i42fLoop, i42fNS, []string{i42fAllow})
		spec := obj.Object["spec"].(map[string]any)
		protocols := matchProtocolItems(t, spec["network"].(map[string]any)["matchProtocols"])
		if !slices.Contains(protocols, "udp") || !slices.Contains(protocols, "tcp") {
			t.Fatalf("egress proxy matchProtocols must include udp AND tcp (platform DNS), got %v", protocols)
		}
	})
	t.Run("model proxy policy allows udp+tcp", func(t *testing.T) {
		obj := EmitModelProxyKubeArmorPolicy(i42fLoop, i42fNS, i42fModelEP)
		spec := obj.Object["spec"].(map[string]any)
		protocols := matchProtocolItems(t, spec["network"].(map[string]any)["matchProtocols"])
		if !slices.Contains(protocols, "udp") || !slices.Contains(protocols, "tcp") {
			t.Fatalf("model proxy matchProtocols must include udp AND tcp (platform DNS), got %v", protocols)
		}
	})
}

// review P1 (PR #28 early review, D39): modelEndpoint can be a bare in-cluster
// Service name (single-label, same namespace). The proxy's resolver queries
// the search-expanded name <host>.<ns>.svc.cluster.local first, so the
// expanded form must be on the allowlist too (kept alongside the bare form).
func TestEmitModelProxyKubeArmorPolicySingleLabelEndpoint(t *testing.T) {
	// A bare single-label endpoint (host, no port).
	obj := EmitModelProxyKubeArmorPolicy(i42fLoop, i42fNS, "model-svc")
	spec := obj.Object["spec"].(map[string]any)
	domains := matchDomainItems(t, spec["network"].(map[string]any)["matchDNSQueries"])
	if !slices.Contains(domains, "model-svc") {
		t.Fatalf("matchDNSQueries must include the bare single-label host model-svc, got %v", domains)
	}
	if !slices.Contains(domains, "model-svc."+i42fNS+".svc.cluster.local") {
		t.Fatalf("matchDNSQueries must include the search-expanded form model-svc.%s.svc.cluster.local, got %v", i42fNS, domains)
	}
	// A single-label endpoint with an explicit port: same expansion.
	obj = EmitModelProxyKubeArmorPolicy(i42fLoop, i42fNS, "model-svc:8080")
	spec = obj.Object["spec"].(map[string]any)
	domains = matchDomainItems(t, spec["network"].(map[string]any)["matchDNSQueries"])
	if !slices.Contains(domains, "model-svc") {
		t.Fatalf("matchDNSQueries must include the bare single-label host model-svc, got %v", domains)
	}
	if !slices.Contains(domains, "model-svc."+i42fNS+".svc.cluster.local") {
		t.Fatalf("matchDNSQueries must include the search-expanded form model-svc.%s.svc.cluster.local, got %v", i42fNS, domains)
	}
	// A multi-label endpoint is NOT expanded (it is already a valid search
	// candidate; expanding would add a name that never resolves).
	obj = EmitModelProxyKubeArmorPolicy(i42fLoop, i42fNS, "model.other")
	spec = obj.Object["spec"].(map[string]any)
	domains = matchDomainItems(t, spec["network"].(map[string]any)["matchDNSQueries"])
	if !slices.Contains(domains, "model.other") {
		t.Fatalf("matchDNSQueries must include model.other, got %v", domains)
	}
	if len(domains) != 1 {
		t.Fatalf("a multi-label endpoint must not be search-expanded, got %v", domains)
	}
}

// item-shape helpers (P1 #2 from C6b: items are OBJECTS {path}/{domain}/
// {protocol}, not strings; path-only: no execname).
func matchPathItems(t *testing.T, items any) []string {
	t.Helper()
	list, _ := items.([]any)
	out := make([]string, 0, len(list))
	for _, it := range list {
		m, ok := it.(map[string]any)
		if !ok {
			t.Fatalf("matchPaths item must be an object, got %T", it)
		}
		if _, has := m["execname"]; has {
			t.Fatalf("matchPaths item must not carry execname (it overrides path in v1.7.5's BPF-LSM rule keying), got %v", m)
		}
		out = append(out, m["path"].(string))
	}
	return out
}

func matchDomainItems(t *testing.T, items any) []string {
	t.Helper()
	list, _ := items.([]any)
	out := make([]string, 0, len(list))
	for _, it := range list {
		m := it.(map[string]any)
		out = append(out, m["domain"].(string))
	}
	return out
}

func matchProtocolItems(t *testing.T, items any) []string {
	t.Helper()
	list, _ := items.([]any)
	out := make([]string, 0, len(list))
	for _, it := range list {
		m := it.(map[string]any)
		out = append(out, m["protocol"].(string))
	}
	return out
}
