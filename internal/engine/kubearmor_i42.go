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
	"slices"
	"strings"

	"github.com/papawattu/coxswain/internal/policy"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// KubeArmorPolicy spec-level keys (the same keys EmitKubeArmorPolicy builds,
// now the package constants in kubearmor.go: the two emitters and the
// unstructured-map shape share one source of truth).

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
		kaptActionKey: kaptActionValue,
		KaptSelectorKey: map[string]any{
			KaptMatchLabelsKey: egressProxyKaptSelector(loopName),
		},
		KaptProcessKey: map[string]any{
			kaptActionKey:     kaptAllowAction,
			KaptMatchPathsKey: toPathItems([]string{egressProxyBinaryPath}),
		},
	}
	if len(networkAllows) > 0 {
		spec["network"] = proxyNetworkBlock(networkAllows)
	}

	return &unstructured.Unstructured{Object: map[string]any{
		kaptAPIVersionKey: kaptGroup + "/" + kaptVersion,
		kaptKindKey:       kaptKind,
		kaptMetadataKey: map[string]any{
			kaptNameKey:      "coxswain-" + loopName + "-egress-proxy",
			kaptNamespaceKey: namespace,
		},
		KaptSpecKey: spec,
	}}
}

// EmitModelProxyKubeArmorPolicy emits the KubeArmorPolicy for a Loop's model
// proxy pod (D35 part 2: the model proxy had no KubeArmor policy before C6b's
// agent-pod-only policy). The selector is the model proxy label set
// (component = model-proxy + coxswain.io/proxy-for). modelEndpoint is the
// endpoint the proxy dials (spec.agent.modelEndpoint, or the proxy's
// configured target in Phase 1): host, host:port, or a URL — only the host
// matters to a DNS allowlist. The network block allows that host (+ its
// search-expanded form when it is a bare single-label in-cluster Service name,
// review P1: the resolver queries <host>.<ns>.svc.<clusterDomain> first) +
// tcp + the platform DNS allow (udp+tcp, so the proxy can resolve the host
// under spec.action Block). clusterDomain is the cluster's service DNS domain
// (default cluster.local, R16 I44 item 2: the enforcer must not hard-code it).
func EmitModelProxyKubeArmorPolicy(loopName, namespace, modelEndpoint, clusterDomain string) *unstructured.Unstructured {
	host, _ := modelEndpointHost(modelEndpoint)
	if clusterDomain == "" {
		clusterDomain = defaultClusterDomain
	}
	spec := map[string]any{
		kaptActionKey: kaptActionValue,
		KaptSelectorKey: map[string]any{
			KaptMatchLabelsKey: modelProxyKaptSelector(loopName),
		},
		KaptProcessKey: map[string]any{
			kaptActionKey:     kaptAllowAction,
			KaptMatchPathsKey: toPathItems([]string{modelProxyBinaryPath}),
		},
	}
	if host != "" {
		// A bare single-label host is an in-cluster Service name: the resolver
		// queries the search-expanded form first, so allowlist both (the bare
		// form too, for the absolute-name query under ndots:1).
		domains := []string{host}
		if isSingleLabelName(host) {
			domains = append(domains, host+"."+namespace+".svc."+clusterDomain)
		}
		spec["network"] = map[string]any{
			kaptActionKey:   kaptAllowAction,
			KaptMatchDNSKey: toDomainItems(dedupe(domains)),
			// tcp: the proxy dials the model endpoint over TCP. udp+tcp: the
			// platform DNS allow (review P1: a tcp-only matchProtocols with
			// spec.action Block denies the proxy's own DNS lookups).
			KaptMatchProtoKey: toProtocolItems(appendDeduped([]string{"tcp"}, dnsAllowProtocols()...)),
		}
	}

	return &unstructured.Unstructured{Object: map[string]any{
		kaptAPIVersionKey: kaptGroup + "/" + kaptVersion,
		kaptKindKey:       kaptKind,
		kaptMetadataKey: map[string]any{
			kaptNameKey:      "coxswain-" + loopName + "-proxy",
			kaptNamespaceKey: namespace,
		},
		KaptSpecKey: spec,
	}}
}

// proxyNetworkBlock builds the KubeArmorPolicy network block for a set of
// network allows: matchDNSQueries ({domain}) + matchProtocols ({protocol}),
// action Allow. It reuses splitNetworkAllows so the proxy translation cannot
// drift from the agent policy's (host:port -> domain + protocol name; the
// port is lost, as in C6b — the proxy NetworkPolicies carry the precision).
// The platform DNS allow (policy.DNSAllow's udp+tcp protocols) is ALWAYS
// present (review P1: a tcp-only matchProtocols with spec.action Block
// denies the proxy's own DNS lookups — the proxy could not resolve ANY
// allowed host). The constant flows through splitNetworkAllows' special
// case, the same translation the agent policy uses.
func proxyNetworkBlock(networkAllows []string) map[string]any {
	domains, protocols := splitNetworkAllows(append([]string{policy.DNSAllow}, networkAllows...))
	return map[string]any{
		kaptActionKey:     kaptAllowAction,
		KaptMatchDNSKey:   toDomainItems(domains),
		KaptMatchProtoKey: toProtocolItems(protocols),
	}
}

// dnsAllowProtocols returns the protocol names the platform DNS allow
// (policy.DNSAllow) expands to — ["udp", "tcp"] via splitNetworkAllows' special
// case. One source of truth: the emitter's model-proxy network block (which
// does not go through proxyNetworkBlock) reuses it so the two proxy policies
// and the agent policy cannot drift.
func dnsAllowProtocols() []string {
	_, protocols := splitNetworkAllows([]string{policy.DNSAllow})
	return protocols
}

// isSingleLabelName reports whether host is a bare single-label name (no dot,
// no trailing dot) — the form a resolver search-expands to <host>.<ns>.
// svc.cluster.local. Multi-label names are NOT expanded (the first candidate
// is the name itself, or an absolute form the user gave).
func isSingleLabelName(host string) bool {
	return host != "" && !strings.Contains(host, ".")
}

// appendDeduped returns in with extra appended, de-duplicated (order preserved
// for the extra entries; the result is sorted by the caller's toProtocolItems
// consumers only when dedupe is applied — here the order is stable for tests
// via slices.Contains, so a plain dedupe suffices).
func appendDeduped(in []string, extra ...string) []string {
	out := append([]string{}, in...)
	for _, e := range extra {
		if !slices.Contains(out, e) {
			out = append(out, e)
		}
	}
	return out
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
