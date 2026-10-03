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

// R16 I44 item 2: the cluster domain is a LoopReconciler field (default
// cluster.local) used by proxyServiceURL / egressProxyServiceURL /
// egressNOProxy, and (via the FQDN functions wired into the Enforcer) by the
// bare-host FQDN in the model-proxy policy. A unit test pins that a
// NON-default domain flows through every one of them, so a hard-coded
// cluster.local cannot re-sneak back in.

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	coxv1alpha1 "github.com/papawattu/coxswain/api/v1alpha1"
	"github.com/papawattu/coxswain/internal/engine"
	"github.com/papawattu/coxswain/internal/policy"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

const (
	i44TestLoop  = "i44-loop"
	i44TestNS    = "i44-ns"
	i44TestModel = "model-svc"
	// i44AltDomain is a non-default cluster domain (the kind a cluster with a
	// DNS-domain override on its service CIDR would have).
	i44AltDomain = "corp.example"
)

// TestClusterDomainFlowsThroughProxyURLs asserts a non-default ClusterDomain is
// used by the agent's proxy Service URLs (COX_MODEL_BASE_URL + *_PROXY) and by
// the FQDN form of NO_PROXY, not the default cluster.local.
func TestClusterDomainFlowsThroughProxyURLs(t *testing.T) {
	const loop, ns = i44TestLoop, i44TestNS

	r := &LoopReconciler{ClusterDomain: i44AltDomain}

	// COX_MODEL_BASE_URL host: <loop>-proxy.<ns>.svc.<domain>.
	if got, want := r.proxyServiceURL(loop, ns),
		"http://"+loop+"-proxy."+ns+".svc."+i44AltDomain+":8080"; got != want {
		t.Fatalf("proxyServiceURL with a non-default domain: got %q, want %q", got, want)
	}

	// *_PROXY host: <loop>-egress-proxy.<ns>.svc.<domain>.
	if got, want := r.egressProxyServiceURL(loop, ns),
		"http://"+loop+"-egress-proxy."+ns+".svc."+i44AltDomain+":3128"; got != want {
		t.Fatalf("egressProxyServiceURL with a non-default domain: got %q, want %q", got, want)
	}

	// egressNOProxy: the model proxy Service's FQDN form carries the domain.
	noProxy := r.egressNOProxy(&coxv1alpha1.Loop{ObjectMeta: metav1.ObjectMeta{Name: loop, Namespace: ns}})
	if !strings.Contains(noProxy, loop+"-proxy."+ns+".svc."+i44AltDomain) {
		t.Fatalf("egressNOProxy must carry the model proxy FQDN form with the non-default domain (%s), got %q", i44AltDomain, noProxy)
	}
}

// TestClusterDomainDefaultsToClusterLocal asserts the default (unset
// ClusterDomain) is cluster.local — the behaviour the rest of the suite pins —
// so a non-default-domain change does not silently widen or narrow the default.
func TestClusterDomainDefaultsToClusterLocal(t *testing.T) {
	const loop, ns = i44TestLoop, i44TestNS

	r := &LoopReconciler{} // ClusterDomain unset.

	if got := r.clusterDomain(); got != "cluster.local" {
		t.Fatalf("unset ClusterDomain must default to cluster.local, got %q", got)
	}
	if got := r.proxyServiceURL(loop, ns); got != "http://"+loop+"-proxy."+ns+".svc.cluster.local:8080" {
		t.Fatalf("proxyServiceURL default: got %q", got)
	}
	if got := r.egressProxyServiceURL(loop, ns); got != "http://"+loop+"-egress-proxy."+ns+".svc.cluster.local:3128" {
		t.Fatalf("egressProxyServiceURL default: got %q", got)
	}
}

// TestModelProxyPolicyUsesClusterDomain asserts the bare-host FQDN in the
// model-proxy KubeArmorPolicy uses the enforcer's cluster domain (a
// non-default domain flows through), not a hard-coded cluster.local.
func TestModelProxyPolicyUsesClusterDomain(t *testing.T) {
	// A bare single-label modelEndpoint: the search-expanded form
	// <host>.<ns>.svc.<domain> must be on the allowlist.
	obj := engine.EmitModelProxyKubeArmorPolicy(i44TestLoop, i44TestNS, i44TestModel, i44AltDomain)

	domains := kaptDNSDomainsOf(obj)
	if !containsStr(domains, i44TestModel) {
		t.Fatalf("model proxy policy must allowlist the bare host, got %v", domains)
	}
	if !containsStr(domains, i44TestModel+"."+i44TestNS+".svc."+i44AltDomain) {
		t.Fatalf("model proxy policy must carry the search-expanded form with the non-default domain (%s), got %v", i44AltDomain, domains)
	}
	// The default-domain form must NOT be present (a hard-coded cluster.local
	// would have produced it).
	if containsStr(domains, i44TestModel+"."+i44TestNS+".svc.cluster.local") {
		t.Fatalf("model proxy policy must NOT carry the hard-coded cluster.local form when a non-default domain is set, got %v", domains)
	}
}

// TestApplyWithoutWiredFQDNsErrors asserts the enforcer's Apply returns an error
// (not a silent fallback) when the proxy FQDN functions are not wired at
// construction (R16 I44 item 1 + review #30 P2: deleting the engine's literal
// fallbacks makes a missing wiring a configuration error).
func TestApplyWithoutWiredFQDNsErrors(t *testing.T) {
	e := &engine.KubeArmorEnforcer{Client: nil} // ProxyFQDN/EgressProxyFQDN nil.
	loop := &coxv1alpha1.Loop{ObjectMeta: metav1.ObjectMeta{Name: i44TestLoop, Namespace: i44TestNS}}
	if err := e.Apply(context.Background(), loop, policy.EffectivePolicy{}); !errors.Is(err, engine.ErrNoProxyFQDNs) {
		t.Fatalf("Apply with unwired FQDNs must return ErrNoProxyFQDNs, got %v", err)
	}
}

// --- helpers (test-local). ---

// kaptDNSDomainsOf returns the network.matchDNSQueries domains of a
// KubeArmorPolicy unstructured object (the engine package's shared keys).
func kaptDNSDomainsOf(obj *unstructured.Unstructured) []string {
	network, _, _ := unstructured.NestedMap(obj.Object, "spec", "network")
	items, _ := network[engine.KaptMatchDNSKey].([]any)
	out := make([]string, 0, len(items))
	for _, it := range items {
		out = append(out, it.(map[string]any)["domain"].(string))
	}
	return out
}

// containsStr reports whether s is in ss.
func containsStr(ss []string, s string) bool {
	return slices.Contains(ss, s)
}
