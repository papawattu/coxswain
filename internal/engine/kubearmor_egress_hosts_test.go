// Copyright 2026 papawattu.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on the License.
// See the License for the specific language governing permissions and
// limitations under the License.

package engine

import (
	"context"
	"slices"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// The egress proxy serves more hosts than the AgentPolicy's network allows:
// the workspace clone, the verify clone-base and the deliver push of an
// external repo. Its KubeArmorPolicy is built from EgressProxyHosts (the
// proxy's whole allowlist) when the controller sets it. Otherwise KubeArmor
// blocks the DNS lookup of the repo host and the clone fails (homelab,
// 2026-11-26: "dns=nxdomain" for github.com).

func getEgressKapt(t *testing.T, cl client.Client) *unstructured.Unstructured {
	t.Helper()
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(KubeArmorGVK)
	if err := cl.Get(context.Background(), client.ObjectKey{Namespace: jproxyTestNS, Name: "coxswain-" + jproxyTestLoopName + "-egress-proxy"}, obj); err != nil {
		return nil
	}
	return obj
}

func egressKaptDomains(t *testing.T, obj *unstructured.Unstructured) []string {
	t.Helper()
	net, _ := obj.Object["spec"].(map[string]any)["network"].(map[string]any)
	return matchDomainItems(t, net["matchDNSQueries"])
}

func TestApplyEgressProxyKaptCarriesTheRepoHost(t *testing.T) {
	cl := newJproxyTestClient(t)
	e := newJproxyEnforcer(t, cl)
	p := jproxyTestPolicy() // Network: proxy.golang.org:443
	p.EgressProxyHosts = []string{"proxy.golang.org:443", "github.com:443"}
	if err := e.Apply(context.Background(), jproxyLoopNoOwnEndpoint(), p); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	obj := getEgressKapt(t, cl)
	if obj == nil {
		t.Fatal("the egress proxy KubeArmorPolicy must exist")
	}
	got := egressKaptDomains(t, obj)
	for _, want := range []string{"proxy.golang.org", "github.com"} {
		if !slices.Contains(got, want) {
			t.Fatalf("matchDNSQueries must include %s (the proxy serves it), got %v", want, got)
		}
	}
}

func TestApplyEgressProxyKaptForACloneOnlyProxy(t *testing.T) {
	cl := newJproxyTestClient(t)
	e := newJproxyEnforcer(t, cl)
	p := jproxyTestPolicy()
	p.Network = nil // no AgentPolicy allows: the proxy exists only for the clone
	p.EgressProxyHosts = []string{"github.com:443"}
	if err := e.Apply(context.Background(), jproxyLoopNoOwnEndpoint(), p); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	obj := getEgressKapt(t, cl)
	if obj == nil {
		t.Fatal("an egress proxy that exists only for the clone must still get its KubeArmorPolicy")
	}
	if got := egressKaptDomains(t, obj); !slices.Contains(got, "github.com") {
		t.Fatalf("matchDNSQueries must include github.com, got %v", got)
	}
}

func TestApplyEgressProxyKaptFallsBackToNetwork(t *testing.T) {
	cl := newJproxyTestClient(t)
	e := newJproxyEnforcer(t, cl)
	if err := e.Apply(context.Background(), jproxyLoopNoOwnEndpoint(), jproxyTestPolicy()); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	obj := getEgressKapt(t, cl)
	if obj == nil {
		t.Fatal("with network allows and no EgressProxyHosts the egress proxy policy must exist")
	}
	if got := egressKaptDomains(t, obj); !slices.Equal(got, []string{"proxy.golang.org"}) {
		t.Fatalf("matchDNSQueries must be the network allows only, got %v", got)
	}
}

func TestApplyNoEgressProxyKaptWithoutHosts(t *testing.T) {
	cl := newJproxyTestClient(t)
	e := newJproxyEnforcer(t, cl)
	p := jproxyTestPolicy()
	p.Network = nil
	if err := e.Apply(context.Background(), jproxyLoopNoOwnEndpoint(), p); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if obj := getEgressKapt(t, cl); obj != nil {
		t.Fatal("no network allows and no EgressProxyHosts: no egress proxy, so no policy")
	}
}
