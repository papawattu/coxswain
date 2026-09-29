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

// I42f (D35 part 2): KubeArmorPolicy emitters for the two operator-owned proxy
// pods (the egress proxy and the model proxy). Each is a pod-level inner fence
// that catches a bug in the proxy's application-level enforcement: the
// process block allows only the proxy's own binary (so a process that
// compromised it cannot exec anything else), and the network block carries the
// exact egress the proxy is supposed to have (the effective AgentPolicy
// network allows for the egress proxy; the model endpoint for the model
// proxy). Everything else is denied by spec.action Block.

import (
	"strings"

	"github.com/papawattu/coxswain/internal/policy"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// egressProxyBinaryPath is the egress proxy binary's absolute path in its
// image (cmd/egress-proxy/Dockerfile: COPY to /usr/local/bin/egress-proxy,
// ENTRYPOINT the same). The process fence allows ONLY this path.
const egressProxyBinaryPath = "/usr/local/bin/egress-proxy"

// modelProxyBinaryPath is the model proxy binary's absolute path in its image
// (cmd/proxy-standin/Dockerfile: COPY to /usr/local/bin/proxy). The process
// fence allows ONLY this path.
const modelProxyBinaryPath = "/usr/local/bin/proxy"

// EmitEgressProxyKubeArmorPolicy emits the KubeArmorPolicy for a Loop's egress
// proxy pod. The selector is the egress proxy's DISJOINT label set (component
// = egress-proxy + coxswain.io/egress-proxy-for, via policy — never
// coxswain.io/loop, the agent policy's selector key, or the agent's exec and
// network rules would bind to the egress proxy pod). networkAllows are the
// effective AgentPolicy network allows (host:port strings): they become
// matchDNSQueries + matchProtocols exactly like the agent policy's
// translation (the port is lost in translation — the egress proxy's
// NetworkPolicy + application layer carry the port precision). An empty
// network allow set emits no network block (the process fence still applies).
//
// The policy is a fully-shaped unstructured object so the API server's
// structural validation accepts it (P1 #1 from C6b), and its matchPaths /
// matchDNSQueries / matchProtocols items are OBJECTS ({path}, {domain},
// {protocol}), path-only (no execname: execname overrides path in v1.7.5's
// BPF-LSM rule keying).
func EmitEgressProxyKubeArmorPolicy(loopName, namespace string, networkAllows []string) *unstructured.Unstructured {
	spec := map[string]any{
		// Default-deny posture: the same Block semantics as the agent policy
		// (C6b) — per-rule action Allow is the carve-out.
		"action": "Block",
		"selector": map[string]any{
			"matchLabels": egressProxyKaptSelector(loopName),
		},
		"process": map[string]any{
			kaptActionKey: kaptAllowAction,
			"matchPaths":  toPathItems([]string{egressProxyBinaryPath}),
		},
	}
	if len(networkAllows) > 0 {
		spec["network"] = proxyNetworkBlock(networkAllows)
	}

	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": kaptGroup + "/" + kaptVersion,
		"kind":       kaptKind,
		"metadata": map[string]any{
			"name":      "coxswain-" + loopName + "-egress-proxy",
			"namespace": namespace,
		},
		"spec": spec,
	}}
}

// EmitModelProxyKubeArmorPolicy emits the KubeArmorPolicy for a Loop's model
// proxy pod (D35 part 2: the model proxy had no KubeArmor policy before C6b's
// agent-pod-only policy). The selector is the model proxy label set
// (component = model-proxy + coxswain.io/proxy-for). modelEndpoint is the
// endpoint the proxy dials (spec.agent.modelEndpoint, or the proxy's
// configured target in Phase 1): host, host:port, or a URL — only the host
// matters to a DNS allowlist. The network block allows that host + tcp (the
// proxy dials the model endpoint over TCP; KubeArmor cannot express the port,
// the proxy's NetworkPolicy carries the precision).
func EmitModelProxyKubeArmorPolicy(loopName, namespace, modelEndpoint string) *unstructured.Unstructured {
	host, _ := modelEndpointHost(modelEndpoint)
	spec := map[string]any{
		kaptActionKey: "Block",
		"selector": map[string]any{
			"matchLabels": modelProxyKaptSelector(loopName),
		},
		"process": map[string]any{
			kaptActionKey: kaptAllowAction,
			"matchPaths":  toPathItems([]string{modelProxyBinaryPath}),
		},
	}
	if host != "" {
		spec["network"] = map[string]any{
			kaptActionKey:     kaptAllowAction,
			"matchDNSQueries": toDomainItems([]string{host}),
			"matchProtocols":  toProtocolItems([]string{"tcp"}),
		}
	}

	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": kaptGroup + "/" + kaptVersion,
		"kind":       kaptKind,
		"metadata": map[string]any{
			"name":      "coxswain-" + loopName + "-proxy",
			"namespace": namespace,
		},
		"spec": spec,
	}}
}

// proxyNetworkBlock builds the KubeArmorPolicy network block for a set of
// network allows: matchDNSQueries ({domain}) + matchProtocols ({protocol}),
// action Allow. It reuses splitNetworkAllows so the proxy translation cannot
// drift from the agent policy's (host:port -> domain + protocol name; the
// port is lost, as in C6b — the proxy NetworkPolicies carry the precision).
func proxyNetworkBlock(networkAllows []string) map[string]any {
	domains, protocols := splitNetworkAllows(networkAllows)
	return map[string]any{
		kaptActionKey:     kaptAllowAction,
		"matchDNSQueries": toDomainItems(domains),
		"matchProtocols":  toProtocolItems(protocols),
	}
}

// modelEndpointHost extracts the host a model endpoint resolves to (for the
// proxy's DNS allowlist). Accepts "host", "host:port", or a full URL
// (http:// or https://); the port is dropped (KubeArmor matches by DNS query
// name, not port — the proxy's NetworkPolicy carries the port precision).
func modelEndpointHost(endpoint string) (string, string) {
	e := endpoint
	if i := strings.Index(e, "://"); i >= 0 {
		e = e[i+3:]
	}
	return splitHostPort(e)
}

// egressProxyKaptSelector returns the label selector for a Loop's egress
// proxy pod: the proxy's disjoint label set (via the policy package's label
// constants — component = egress-proxy + coxswain.io/egress-proxy-for).
// These keys must stay in sync with egressProxyLabels (internal/controller);
// the selector NEVER carries coxswain.io/loop (the agent policy's key).
func egressProxyKaptSelector(loopName string) map[string]any {
	return map[string]any{
		policy.ComponentLabelKey:       policy.ComponentEgressProxyLabel,
		"coxswain.io/egress-proxy-for": loopName,
	}
}

// modelProxyKaptSelector returns the label selector for a Loop's model proxy
// pod (via proxyLabels in internal/controller: component = model-proxy +
// coxswain.io/proxy-for). The selector NEVER carries coxswain.io/loop.
func modelProxyKaptSelector(loopName string) map[string]any {
	return map[string]any{
		policy.ComponentLabelKey: policy.ComponentProxyLabel,
		"coxswain.io/proxy-for":  loopName,
	}
}
